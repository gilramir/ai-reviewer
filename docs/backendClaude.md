# Driving Claude Code from your own program

How ai-reviewer uses the `claude` CLI as a backend, and what you need to know to
do the same from any language that can spawn a process and read a pipe.

Everything here is verified against `claude` 2.1.266 by running it — the traces
below are real output, not illustrations.

## The thing that is not obvious

`claude -p` looks like a one-shot command: prompt in, answer out, process exits.
That is what it does with the default `--input-format text`.

With `--input-format stream-json` it is something else entirely: a **persistent
bidirectional session over pipes**. The process reads newline-delimited JSON
messages from stdin for as long as stdin stays open, and writes newline-delimited
JSON events to stdout. You write one message, read events until the turn ends,
and then — the process still running, the conversation still in its context —
write the next one.

That single fact is what makes a program like this one possible. A document under
review stays in one conversation: the second comment on a file does not re-read
the file, does not resend the transcript, and is served largely from the prompt
cache.

```
your program                          claude -p --input-format stream-json
     │                                        --output-format stream-json
     ├── {"type":"user",...}\n  ──stdin──>
     │                          <──stdout──   {"type":"system","subtype":"init"}
     │                          <──stdout──   {"type":"assistant",...}   tool_use
     │                          <──stdout──   {"type":"user",...}        tool_result
     │                          <──stdout──   {"type":"assistant",...}   text
     │                          <──stdout──   {"type":"result",...}      turn over
     ├── {"type":"user",...}\n  ──stdin──>    (same process, same context)
     │                          <──stdout──   ...
```

## Why the CLI and not an SDK

Three surfaces can put Claude behind your application, and they are not
interchangeable:

| Surface | What you get | What you write |
|---|---|---|
| **Messages API** (`anthropic` SDK) | The model | The whole agent loop: tool definitions, the tool-execution loop, file reading, editing, context management |
| **Claude Agent SDK** (`claude-agent-sdk`, `@anthropic-ai/claude-agent-sdk`) | Claude Code as a library — built-in Read/Edit/Write/Grep/Glob/Bash, the loop, sessions, permissions, hooks | A prompt and some options |
| **The `claude` CLI over pipes** | The same harness as the Agent SDK, as a subprocess | A subprocess and a JSON-lines codec |

The Agent SDK is the right answer if you are writing Python or TypeScript. It is
the same harness; you just call `query(prompt, options)` instead of managing a
process. Its docs are at `code.claude.com/docs/en/agent-sdk`.

This daemon is Go, and **there is no Go binding for the Agent SDK**. That is the
whole reason it drives the CLI over a pipe. The trade is honest: about 700 lines
of process and framing code (`internal/claudeproc/`) buys the entire Claude Code
harness — the tool loop, the edit machinery, session persistence, the prompt
cache — without an API key, without reimplementing an agent, and without a
dependency that has to track the model API.

Three practical consequences worth knowing before you choose it:

- **Authentication is the user's, not yours.** The CLI uses whatever credentials
  `claude` already has. There is no API key in your config, and no key to leak.
  It also means you cannot run this on a server where nobody has logged in.
- **You inherit a Node process per conversation.** Not free. Cap them.
- **The protocol is a surface you do not control.** It is stable in practice, but
  it is the CLI's, and it grows frames. Ignore what you do not recognise (see
  below) and it will keep working across upgrades.

## The command line

This is what the daemon runs, one process per document (`session.go:args`):

```
claude -p
  --input-format stream-json          # read user messages from stdin, forever
  --output-format stream-json         # emit events as JSON lines
  --verbose                           # required for per-message output
  --permission-mode acceptEdits       # never block on a prompt nobody will answer
  --strict-mcp-config                 # ignore the user's own MCP servers
  --tools Read,Edit,Write,Grep,Glob   # no Bash
  --session-id <uuid>                 # first launch only
  --resume <uuid>                     # every launch after that
  --model <alias-or-name>             # optional: "opus", "sonnet", "claude-opus-5"
  --append-system-prompt <text>       # shape the role without replacing the default
  --max-budget-usd 5.00               # optional spend cap for this process
```

Notes on the ones that carry weight:

**`--permission-mode acceptEdits`** — the CLI's default is to ask before writing.
There is no terminal here and nobody is watching, so a prompt is a hang. Accepting
edits is safe only because of what is around it: the working directory bounds
which files exist at all (see below), and the tool list has no way to run a
command. Choose the narrowest mode that does not block, not `bypassPermissions`.

**`--tools`** restricts to the built-in set by name. `--tools ""` disables all
tools; `--tools default` allows everything. Bash is deliberately absent here: the
daemon is the only writer of git history, and a model that can run commands could
rewrite it. `--restricted` is a stronger blanket version — it removes the
command-running tools and WebFetch, and ignores user/project settings files.

**`--strict-mcp-config`** — without it the process inherits whatever MCP servers
the user has configured, which widens the tool surface unpredictably and differs
per machine. A backend wants a fixed tool surface.

**`--append-system-prompt`** adds to the default system prompt; `--system-prompt`
replaces it. Appending is almost always what you want — the default is what makes
the tools work properly.

**`--max-budget-usd`** caps spend for the process. Like every flag here it only
works with `-p`.

**`--model`** takes an alias for the current model in a family (`opus`, `sonnet`,
`fable`) or an exact name (`claude-opus-5`). Aliases move when new models ship;
exact names do not.

Two flags this daemon does not use, but you might:

- **`--include-partial-messages`** streams token-level deltas instead of whole
  message blocks. Use it if you are rendering into a terminal-style live view.
  This UI streams per block, which is enough for a sidebar and much less traffic.
- **`--replay-user-messages`** echoes your stdin frames back on stdout, so you can
  positively acknowledge that a message was consumed.

## Where the process runs

Not a detail. **The working directory is the CLI's file-permission boundary.**
Everything under it can be read and edited; everything above it is refused, and
the refusal comes back as a tool result the model then has to explain to your
user.

Two experiments, same repository — `CLAUDE.md` and `main.go` at the top, the
document under review in `doc/`:

```
cwd = repo/doc                          cwd = repo
─────────────────────────────           ─────────────────────────────
"secret codeword?"   ZUCCHINI-42        "secret codeword?"   ZUCCHINI-42
   (CLAUDE.md at the repo root is          (same — it is found either way)
    found from a subdirectory)

"read ../main.go"    CANNOT READ       "read main.go"       BROCCOLI-99
   Claude requested permissions to        1  package main
   read from .../repo/main.go             3  // TOKEN: BROCCOLI-99
   [refused: outside the working
    directory]
```

So the two halves of "project context" behave differently, and it is worth
knowing which is which:

- **`CLAUDE.md` is found by walking up parent directories.** Starting the process
  in a subdirectory does not lose it.
- **File access does not walk up.** A document that references `../src/Tui.gren`
  — a normal thing for documentation to do — cannot be checked against it from a
  process started in `doc/`.

This daemon therefore runs Claude at the **repository top level** (whatever
`git rev-parse --show-toplevel` says), not at the directory being reviewed, and
falls back to the reviewed directory when it is not in a repository at all
(`review.Review.work`, from `gitstore.History.Root`).

That is a deliberate widening, and it comes with a rule: **once you move the
working directory, every path you hand the model has to move with it.** The
prompt says `File: doc/spec.md`, not `File: spec.md`, and the paths the model
reports back are made relative to the same root before they reach `git add` —
which also runs there. Get that wrong and the failure is quiet in the worst way:
the edit lands on disk, `git add spec.md` finds nothing at the repository root,
and the commit that was supposed to record the change never happens.

Keep the two coordinate systems straight and name them in the code:

| Root | What it addresses |
|---|---|
| the review root (`--root`) | what the *browser* may open; the paths in the UI |
| the workspace (repo top level) | where Claude runs; the paths in prompts, tool results and commits |

## The protocol

### What you write

One JSON object per line, exactly the Messages API user-message shape:

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"..."}]}}
```

Write it, append `\n`, flush. Nothing else is required — no id, no session field.

### What you read

One JSON object per line. Here is a real two-turn trace, timings included, from
one process (`Read,Edit` allowed, working directory holding `note.md` containing
`Line one.`):

```
turn 1 — "In note.md, change the word 'one' to 'two'. Then reply with one short sentence."
   0.5s  system/init
   1.2s  rate_limit_event
   2.3s  assistant        content: [tool_use  Read   {"file_path": ".../note.md"}]
   2.3s  user             content: [tool_result  "1\tLine one.\n2\t"]
   4.3s  assistant        content: [tool_use  Edit   {"file_path": ".../note.md",
                                                      "old_string": "Line one.",
                                                      "new_string": "Line two."}]
   4.3s  user             content: [tool_result  "The file ... has been updated successfully."]
   5.2s  assistant        content: [text  "note.md now reads \"Line two.\""]
   5.2s  result/success   result: "note.md now reads \"Line two.\""

turn 2 — "What word did you just replace? Answer in three words."   (same process)
   0.0s  system/init
   1.4s  assistant        content: [text  "\"one\" became \"two\""]
   1.4s  result/success   result: "\"one\" became \"two\""
```

The second turn is the point of the whole exercise: nothing was resent, the file
was not re-read, and the answer took 1.4 seconds.

The frames that matter:

| `type` | When | What to do with it |
|---|---|---|
| `system` / `subtype: "init"` | **Start of every turn**, not just the first | Carries `session_id`, `cwd`, `model`, `tools`, `permissionMode`. Seeing it is how you know the CLI has accepted this session id |
| `assistant` | Model output | `message.content` is a block array: `text` blocks are prose, `tool_use` blocks name a tool and carry its `input` |
| `user` | The harness's own tool results | Echoed back so you can display the loop. Ignore it unless you are showing tool activity |
| `result` | **Once, at the end of a turn** | The only end-of-turn signal. Carries `result` (final prose), `is_error`, `total_cost_usd`, `usage`, `duration_api_ms` |
| anything else | e.g. `rate_limit_event` | **Ignore it.** New frame types appear in CLI upgrades |

That last row is a rule, not an aside. `rate_limit_event` is not in this daemon's
switch statement and it does not need to be — an unknown frame falls through and
the turn proceeds. A parser that errors on unrecognised types is a parser that
breaks on the next release.

### Turn boundaries

`result` ends the turn. Nothing else does — not an `assistant` frame with text in
it, not a pause. A turn may contain any number of `assistant`/`user` pairs as the
model works, and the model may emit prose, then call another tool, then emit more
prose.

The `result` frame's `result` field is the authoritative final text. It is not
always the concatenation of the `text` blocks you saw: the harness can retry
inside a turn. This daemon accumulates text for live streaming and then replaces
it with `result` when the turn closes (`session.go:applyFrame`).

**A failed turn is still a `result` frame.** `subtype` is
`error_during_execution` rather than `success`, `is_error` is true, and an
`errors` array says what happened. Read `is_error` — a turn can fail without the
process failing, and if you only watch for a dead process you will render the
empty `result` string as the model's answer.

## Sessions: the durable part

Every conversation has a UUID. You choose it, you persist it, and it is what
turns a crashed daemon into a resumed conversation instead of a lost one.

- **First launch:** `--session-id <uuid>`. Must be a valid UUID.
- **Every launch after that:** `--resume <uuid>`.

Getting this backwards fails loudly, which is the good outcome — but the two
directions fail in **different shapes**, and that difference decides where your
handling goes:

```
$ claude -p --session-id <an id already used> ...
Error: Session ID 22222222-... is already in use.        # stderr, exit 1, no frames

$ claude -p --resume <an id the CLI never had> ...
No conversation found with session ID: 99999999-...      # stderr, exit 0
{"type":"result","subtype":"error_during_execution","is_error":true,
 "errors":["No conversation found with session ID: 99999999-..."],...}
```

The first is a dead process: your frame channel closes and you never see a
`result`. The second is a **completed turn that says it failed** — a well-formed
`result` frame with `is_error` set. A caller that only checks for errors from the
process misses it entirely and reports an empty answer.

So you need one bit of state per conversation: *has the CLI seen this id yet?*

That bit is more slippery than it looks, and it is worth getting right rather
than clever:

- It is **not derivable from the id** — a UUID looks the same before and after
  its first launch.
- Tracking it in memory is not enough. Your process restarts; the id is
  persisted, the bit is not; your first launch says `--session-id` for an id the
  CLI has already met, and is refused.
- Persisting the bit only moves the problem. An id minted and written to disk
  moments before a crash was never handed to the CLI, and would then be resumed
  just as wrongly — `Error: No conversation found with session ID ...`.

The reliable signal is the refusal itself. Guess, and treat "already in use" as
the bit you were missing: flip to resume and retry the turn once. This daemon
learns the bit from the `system/init` frame and recovers from the refusal when
it turns out to have guessed wrong (`session.go:recoverSessionMode`); the id
lives in `sessions` in `.ai-reviewer/state.json`.

Guessing wrong the other way is rarer — it needs the CLI to lose a transcript
this program has already used — and, per the shapes above, it arrives as a failed
turn rather than a dead process. Check `is_error` and you will see it.

Resuming really does restore the conversation, in a **new process**, minutes or
days later:

```
$ claude -p --resume 22222222-... <<< '{"type":"user",...,"text":"What word did you replace earlier?"}'
result: "\"one\" became \"two\""
```

The transcript itself lives in the CLI's own store, one JSONL file per session:

```
~/.claude/projects/<slugified-working-directory>/<session-uuid>.jsonl
```

Your program does not read that file — resume is the interface — but knowing it
exists explains the division of state. Your side stores *which* conversation
belongs to *what*. The CLI stores the conversation. Lose your side and the
transcripts are orphaned but intact; `--fork-session` will branch one into a new
id if you ever need to.

## Process lifecycle

A Node process per conversation is real memory, so the daemon keeps two limits
(`manager.go`):

- **Idle timeout** (default 30 min): a reaper kills processes that have not run a
  turn. The session id survives, so the next comment resumes.
- **A live cap** (default 6): opening a seventh document evicts the
  least-recently-used process.

Both are the same trick — killing a process is not ending a conversation. That is
only true because of `--resume`, and it is what makes the resource question easy.

Three lifecycle details that are less obvious:

**One reader goroutine for the life of the process.** Not one per turn. A reader
started per turn is still blocked in `Decode` when the next turn begins, and two
goroutines sharing a `json.Decoder` corrupts it outright — "JSON decoder out of
sync". The reader owns the decoder and posts frames to a channel; turns read the
channel.

**Use a streaming JSON decoder, not a line scanner.** Frames routinely run to
several kilobytes and a `tool_result` can be far larger. Go's `bufio.Scanner`
gives up past its buffer limit; `json.Decoder` over a buffered reader does not.

**A cancelled turn takes the process with it.** If you abandon a turn mid-stream —
timeout, user interrupt — the unread remainder of that turn is still in the pipe
and will desynchronise every turn after it. Kill the process and resume. That is
also how the reviewer's *interrupt* button works: the CLI does speak a
control-protocol interrupt over the same pipe, but killing is unambiguous and
needs no undocumented framing, and the partial turn was what the reviewer wanted
to throw away anyway.

Also: `Wait()` on the child somewhere, or a long-running daemon accumulates
zombies.

## Knowing what the model actually did

This is the part an API-level integration would have to invent.

**Which files changed** — watch `tool_use` blocks for the writing tools and read
`input.file_path`:

```go
func isWriteTool(name string) bool {
    switch name {
    case "Edit", "Write", "MultiEdit", "NotebookEdit":
        return true
    }
    return false
}
```

The daemon collects those paths, de-duplicated, and hands them to `git add` when
the turn ends. A turn that edited nothing stages nothing and records no commit —
which is the normal outcome for *"why this?"* and is how the UI can tell an
answer from a change without ever classifying the comment.

**What it cost** — `total_cost_usd` on the `result` frame. The same frame's
`usage` carries `cache_read_input_tokens` and `cache_creation_input_tokens`,
which is where you can see the prompt cache doing its work across turns.

**Whether it failed** — `is_error` on the `result` frame. Distinct from the
process dying, which you see as the frame channel closing.

**Why it failed** — keep the tail of stderr. The daemon holds the last 8 KB in a
ring buffer (`ring.go`) and quotes it when a process exits mid-turn. Unbounded
buffering of output nobody reads is just a leak.

## Testing without a model in the loop

The protocol is plain JSON lines, so a stub speaks it in under a hundred lines.
`internal/server/testdata/fake-claude` is a Python script that reads user frames
from stdin, and — if the prompt contains "reword" — rewrites the quoted passage in
the named file and emits an `Edit` `tool_use` frame followed by a `result`.

That makes the entire pipeline testable, free and deterministic: comment in,
`tool_use` out, edit on disk, commit made, document re-rendered, frames pushed to
the browser. The integration tests in `internal/server` run against it.

A stub can only prove you speak the protocol you wrote it against, so there is one
test that runs against the real CLI, gated behind an environment variable:

```sh
AI_REVIEWER_LIVE=1 go test ./internal/server -run TestLiveClaude -v
```

Worth running when the CLI updates.

## Traps, collected

Every one of these cost someone time:

1. **stdin must stay open.** Closing it ends the session — that is the clean
   shutdown, and it is also an easy accidental one.
2. **`result` is the only end-of-turn signal.** Do not treat a text block as the
   end.
3. **`system/init` arrives on every turn**, not once per process.
4. **Ignore unknown frame types.** They will appear.
5. **`--session-id` twice on the same id is an error**, and so is `--resume` for
   an id the CLI has never seen. Recover from the refusal rather than trying to
   know which applies — see *Sessions* above.
6. **One decoder, one reader goroutine**, for the life of the process.
7. **Don't use a line scanner** — frames outgrow its buffer.
8. **A cancelled turn desynchronises the stream.** Kill and resume.
9. **`--verbose` is required** for streamed output, and says so:
   `Error: When using --print, --output-format=stream-json requires --verbose`.
10. **Most flags need `-p`** — `--max-budget-usd`, `--input-format`,
    `--output-format` and `--include-partial-messages` all say "only works with
    --print".
11. **Reap the child** or collect zombies.
12. **Cap live processes.** Each one is a Node runtime.
13. **The working directory is the permission boundary**, and every path you send
    is relative to it. Choose it deliberately; `CLAUDE.md` will be found from
    below, but files above will not.

## Where the code is

```
internal/claudeproc/session.go   the process, the args, the framing, one turn
internal/claudeproc/manager.go   one session per document, idle reaping, LRU cap
internal/claudeproc/ring.go      bounded stderr buffer
internal/review/prompt.go        the system prompt and how a comment is phrased
internal/review/review.go        turn lifecycle: prompt, watch tools, commit
internal/server/testdata/fake-claude   the stub that speaks the same protocol
```

`session.go` is the file to read first; it is about 500 lines, half of them
comments explaining the decisions this document summarises.

## See also

- `claude --help` — the authority on flags, and it changes.
- `code.claude.com/docs/en/agent-sdk` — the Agent SDK, if you are in Python or
  TypeScript and would rather not own a subprocess.
- [`../README.md`](../README.md) — what this daemon is for.
