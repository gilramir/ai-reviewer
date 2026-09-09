# ai-reviewer

Review Markdown documents in a browser, with Claude Code editing them under you.

Select a passage, type a comment — *"why this?"*, *"reword this"* — and the
answer appears in the margin, or the document changes and re-renders. Every
change that lands is a commit on a task branch, so a review session leaves a
diff you can read and undo.

It drives the **Claude Code CLI**, not the API. There is no API key to manage:
if `claude` works in your terminal, it works here.

## How it fits together

```
browser ──ws──> ai-reviewer (Go) ──pipe──> claude -p --input-format stream-json
   Gren SPA          │                            (one process per document)
                     ├── goldmark ──> JSON AST with source spans
                     ├── fsnotify ──> re-render on change
                     └── git ──────> one commit per turn
```

Three decisions shape everything else:

**The server owns Markdown parsing.** goldmark produces a JSON tree whose nodes
carry byte spans, and Gren renders that tree. The browser never parses Markdown,
so there is only one opinion about where a paragraph starts. A second parser in
the client would be a second opinion, and comment anchors would drift wherever
the two disagreed.

**Comments anchor to text, not offsets.** A thread stores the quoted passage
plus the ~48 characters either side. An edit earlier in the file shifts every
byte offset after it but leaves the surrounding words alone, so anchors survive
edits that coordinates would not. Re-location tolerates the Markdown syntax the
browser stripped: a selection of `a quoted caution` still finds
`a quoted **caution**` in the source.

**Nothing classifies the comment.** *"why this?"* wants an answer, *"reword
this"* wants a change, and plenty of comments want both. The comment is passed
through and the daemon reports what actually happened by watching which tools
the model called.

## Build

The Makefile is the build. devbox is one way to put the tools on PATH; a manual
install is another. Neither is called from the Makefile, so `make` behaves
identically under both.

```sh
make tools     # what's installed vs. what versions.mk expects
make           # compile the Gren frontend, embed it, build the binary
make check     # tests + vet + gofmt
```

With devbox: `devbox shell && make`. Without: install Go and Gren yourself and
run `make`.

`make check` also verifies Gren formatting, if `gren-format` is on PATH. That is
a separate tool from the compiler — `gren format` is not a compiler subcommand —
and it needs Node >= 20, which `devbox.json` supplies as `nodejs@22`. Both
`make check` and `make fmt` skip it with a note when it is absent, so a machine
without Node can still build, test and run everything.

Note that `--diff` exits 0 whether or not it finds differences, so `make check`
gates on the output being empty rather than on the exit status.

`GOTOOLCHAIN=local` is set by the Makefile, and `go.mod` pins a Go floor of
1.25. Go will not silently download a different compiler behind your back — the
drift `versions.mk` exists to catch.

The compiled frontend is embedded with `go:embed`, so `make` produces one static
binary. **The machine serving a review needs no Gren, no Node, and no devbox** —
build once and `scp` it.

## Running

Two deployments, one binary.

**Tunnelled** (Claude Code on a remote host, browser on your desktop):

```sh
ai-reviewer serve --root ./docs
ssh -L 8080:localhost:8080 remote-host
```

**On a trusted LAN**, Jupyter-style:

```sh
ai-reviewer serve --root ./docs --listen 0.0.0.0:8080
```

```
  ai-reviewer

    reviewing  /home/you/docs
    branch     review/docs-2026-09-08
    open       http://192.168.1.14:8080/
    password   quiet-harbor-lamp-09
```

The password is four words rather than 48 hex characters because you read it off
one screen and type it into another.

The CLI is built on [`gilramir/argparse`](https://github.com/gilramir/argparse):
`ai-reviewer --help` lists the subcommands, `ai-reviewer serve --help` lists the
flags with their defaults, and a bare `ai-reviewer` prints the help and exits
non-zero.

For a password that survives restarts, `ai-reviewer password` stores a PBKDF2
digest in `~/.config/ai-reviewer/config.json`. The plaintext is never written.

Authentication is on by default. `--no-auth` is refused unless the bind address
is loopback — the one misconfiguration that would silently publish your
documents to the network.

## Security notes

The threat model is a trusted LAN, but two things are handled properly because
getting them wrong is quiet:

- **Cross-site WebSocket hijacking.** WS handshakes are exempt from CORS and the
  browser attaches the session cookie to a cross-origin upgrade. Without an
  `Origin` check, any page you visit while logged in could open a socket and
  drive Claude against your documents. Origin is required and must match; a
  missing header is refused.
- **`Secure` cookies over plain HTTP.** Set on a LAN address, the browser drops
  the cookie silently and you get a login loop that works perfectly on
  localhost. `Secure` is set only under TLS.

Claude runs with `--tools Read,Edit,Write,Grep,Glob` and `--strict-mcp-config`:
no Bash, and none of your personal MCP servers. The daemon is the only writer of
git history.

## Layout

```
cmd/ai-reviewer/      CLI: serve, password
internal/mdast/       goldmark -> JSON AST with source spans
internal/review/      documents, threads, anchoring, turn lifecycle
internal/claudeproc/  the long-lived claude process, one per document
internal/gitstore/    one commit per turn; snapshots outside a repo
internal/server/      HTTP, auth, WebSocket
web/src/              Gren: Doc (decoder), Protocol (wire), Main (app)
web/static/           index.html, ports.js, style.css
```

## Tests

```sh
make test                                          # stubbed, free, fast
AI_REVIEWER_LIVE=1 go test ./internal/server \
  -run TestLiveClaude -v                           # against the real CLI
```

`testdata/fake-claude` speaks the same stream-json protocol as the real CLI, so
the whole pipeline is testable without a model in the loop. The live test is
worth running when the CLI updates: the stub can only prove the daemon speaks
the protocol it was written against, not that it is still the protocol the CLI
emits.
