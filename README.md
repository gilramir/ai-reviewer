# ai-reviewer

<img src="docs/ai-reviewer.png" alt="" width="140">

Review Markdown documents in a browser, with Claude Code editing them under you.

Select a passage, type a comment — *"why this?"*, *"reword this"* — and the
answer appears in the margin, or the document changes and re-renders. Every
change that lands is a commit on a task branch, so a review session leaves a
diff you can read and undo.

It drives the **Claude Code CLI**, not the API. There is no API key to manage:
if `claude` works in your terminal, it works here. How that works — `claude -p`
with `--input-format stream-json` is a persistent session over pipes, not a
one-shot command — is written up in
**[docs/backendClaude.md](docs/backendClaude.md)**, for anyone who wants to drive
Claude Code from their own program.

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
edits that coordinates would not.

The passage is looked for in the document *as rendered*, which is the string the
reviewer selected out of, and the answer is mapped back to bytes through the
source positions the parser recorded for every run of text. Searching the
Markdown instead was the first design and it does not work: `**[a guide](x.md)**
and on` reads as `a guide and on`, and a selection that runs out of the link
into the words after it cannot be matched by skipping syntax characters, because
`x.md` is not syntax — it is ordinary text the renderer consumed. Link
destinations, image alts, entities and HTML tags are all that same problem.

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
without Node can still build and run everything.

Node also runs the Gren tests in `web/tests`, which `make test` skips the same
way when it is absent.

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
ai-reviewer serve --root ./docs --listen-all 8080
```

`--listen-all PORT` is shorthand for `--listen 0.0.0.0:PORT`, which is the
address anyone serving to a LAN wants and the one it is easiest to fumble. The
two are refused together, and a port is all it takes.

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

## Choosing a document

The top bar names the document you are reading, and clicking that name opens the
list of the others: type to narrow it, arrows to walk it, Enter to open, Escape
to put it away. The rows are grouped under their folder, because a repository's
Markdown is mostly `README.md` files that differ only in which directory they
are in, and the words are matched in any order — `review readme` finds
`internal/review/README.md`, and so does `readme review`.

This used to be a strip of tabs, one per document, and it did not survive being
pointed at a repository rather than a `docs/` directory: the daemon lists every
`.md` under the review root, fifty of them scroll sideways, and the name of the
document actually open can be scrolled out of sight. A top bar owes you the name
of the thing you are reading; the rest is a list, and a list belongs behind it.

## What Claude is running with

The pill in the top bar names the model — `opus`, `sonnet`, or *default model*
when the daemon asks for nothing and leaves the choice to your own `claude`
configuration. Clicking it opens a panel with the rest: the tools, the
permission mode, where Claude runs, the review branch, what the session has
cost, how many processes are live, and which `claude` binary is answering.

The model can be changed from that panel mid-review. Nothing in flight is
interrupted: each document's process relaunches when its next comment arrives
and **resumes the same conversation on the new model**, so switching to Sonnet
for a batch of small edits and back to Opus for a hard question costs nothing
that was already said.

The choice is remembered — it goes into `.ai-reviewer/state.json` when you make
it, so it survives a restart. `--model` on the command line is the more recent
decision and outranks it, and then becomes the remembered one.

**Clear context** in the same panel throws away what the model is carrying, on
every document. Your comment threads stay — they are the daemon's record, and
nothing about them depends on a process being alive — but the next comment on a
document starts a conversation with nothing said in it yet. That is worth doing
after a model change: a resumed conversation reaches the new model with every
word the old one said still in it, and every one of those words is paid for
again on each turn. A reply on an existing thread still works afterwards; the
daemon re-states the passage, since the conversation that used to remember it is
gone.

The panel says how much there is to clear, because otherwise the button is a
guess: **conversation** gives the open document's turns and what they have cost,
with the other documents' totals under it, since clearing takes them all. The
counts survive a restart for the same reason the session ids do — the
conversation survives with them.

The same panel carries the one setting that is yours rather than the review's:
**theme** — light, dark, or *auto*, which follows the machine. It lives in the
browser rather than the daemon, so two people reading the same review can
disagree about it, and it is applied before the first paint, so choosing dark
does not mean a white flash on every load. The login page honours it too.

The palette is written once with CSS `light-dark()`, which makes the whole
switch a single `color-scheme` property — and asks for a browser from 2024 or
later (Chrome 123, Firefox 120, Safari 17.5).

## What changed while you were reading

A few turns in, the page is not the page you started with, and the margin only
says what each turn was asked for. So the words the document did not have when
the session started are tinted — a wash, no border, quiet enough to read
straight through, and quite different from the yellow of a commented passage. A
passage that is both keeps the yellow, because that is the one you can click,
and says the rest in the colour of the line underneath it.

It compares words rather than lines or bytes, which is what keeps it honest
about what actually moved. Re-wrapping a paragraph changes nothing, because a
soft wrap reaches the tree as one space either way. Neither does turning `*this*`
into `**this**`, which the reader cannot see. What is left is the sentences that
say something different, marked from the first changed word to the last with the
spaces between them swallowed, so a rewritten clause is one highlight rather
than a row of them.

**changes** in the settings panel turns the tint off, and the row goes on
counting the passages either way — a document with nothing marked and one whose
highlight is switched off look identical otherwise, and the reviewer who wonders
which they are looking at should not have to toggle it to find out. Like the
theme, the choice is remembered in the browser rather than in the daemon: two
people reading the same review can want different amounts of colour on it.

Two things it cannot do. A deletion leaves nothing on the page to point at, so
it is invisible here — `git log` on the review branch is still where the whole
truth is. And the baseline is this run's: a restart starts the session over, in
the same way and for the same reason that the conversations do.

## Editing it yourself

Not every comment is a question, and some are not even a request: the reviewer
already has the words they want. **Edit it myself** in the composer hands the
passage back as Markdown — the source, asterisks and links included, not the
rendered words the selection was cut from — and Save writes it straight to the
file. No turn runs, and nothing is spent.

The change lands exactly the way a turn's does: one commit on the same branch,
the passage in the subject, and a `Review-Edit: hand` trailer that tells it
apart from what the model wrote.

Two things are refused rather than guessed at. An edit whose passage changed
underneath it — the model rewrote the sentence while the editor sat open — comes
back with the words intact, because the server compares the source the editor
was opened on against what the file says now. And an edit is refused while a
turn is running on that document: the turn is about to write the file from a
copy it read before the edit existed, and neither side would notice the other.

**Shift-Enter sends.** In the comment box, in the editor, and in a thread's
reply box — the same thing as the button beside it.

## Images and generated diagrams

A document that says `![](flow.png)` gets its image from the review root, over a
`/file/` route, with the file's modification time in the URL.

That timestamp is the point. The daemon watches every file under the root, not
only the Markdown, so when you run `dot -Tpng flow.dot -o flow.png` yourself —
which the model cannot do, having no shell — the documents that embed that PNG
re-render on their own. Their image URLs change with the file, and an `<img>`
whose `src` has changed is one the browser actually fetches again. There is
nothing to click; if you ever do want to force a re-read, choosing the open
document again in the picker is one.

Only images are rewritten, and only relative ones that stay inside the review
root: an absolute URL, a `data:` URI, or a path climbing out of the root is left
exactly as the author wrote it. Files go out with `nosniff` and a sandbox
policy, so a directory of arbitrary files cannot become a way to run script in
the review's origin.

## Landing the changes

The panel names the review branch, how many commits are on it that the branch it
was cut from does not have, and the command that lands them:

```
  branch    review/docs-2026-09-09
            4 commits not yet in main
            git switch main && git merge review/docs-2026-09-09
```

The daemon will not run it. A merge can conflict, and a conflict raised inside a
page with no diff view and no way out is worse than no button — so the command
is shown where it can be read and copied, and run somewhere that can answer for
it. Nothing else about the review depends on when you do that.

The count is `git rev-list --count base..HEAD` rather than a tally the daemon
keeps, so it falls to zero on its own once the commits are in — including when
you merge from a terminal, which the daemon never hears about. Outside a
repository there is no branch and no count: gitstore keeps snapshots, and
snapshots are not merged anywhere.

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
git history. The reasoning behind each flag is in
[docs/backendClaude.md](docs/backendClaude.md).

**Claude runs at the repository root, not at `--root`.** Its working directory is
its file-permission boundary, so a review rooted at `doc/` would leave it unable
to read the `../src` its own documents link to. The cost is that the model can
read and edit anything in the repository, not only the documents under review —
the same reach it has when you run `claude` there yourself. What the *browser*
can open is unchanged: still `--root` and below, still refusing paths that escape
it.

## Known issues

**Sessions do not survive a restart.** They live in the daemon's memory, and a
restart prints a new password anyway. The browser notices via `/session` and
sends you to the login page rather than retrying a cookie that can never work
again; anything you clicked while disconnected is queued and sent on reconnect.

## Layout

```
cmd/ai-reviewer/      CLI: serve, password
internal/mdast/       goldmark -> JSON AST with source spans
internal/review/      documents, threads, anchoring, assets, turn lifecycle
internal/claudeproc/  the long-lived claude process, one per document
                      (protocol write-up: docs/backendClaude.md)
internal/gitstore/    one commit per turn; snapshots outside a repo
internal/textdiff/    which words a document gained, for the change highlight
internal/server/      HTTP, auth, WebSocket
web/src/              Gren: Doc (decoder), Protocol (wire), Marks (anchoring),
                      Picker (the document list), Main (app)
web/static/           index.html, ports.js, style.css, favicon.ico if you add one
web/tests/            Gren tests for Marks and Picker, run under gren-unit-node
```

## Tests

```sh
make test                                          # stubbed, free, fast
make web-test ARGS=-v                              # just the Gren tests, verbose
AI_REVIEWER_LIVE=1 go test ./internal/server \
  -run TestLiveClaude -v                           # against the real CLI
```

`testdata/fake-claude` speaks the same stream-json protocol as the real CLI, so
the whole pipeline is testable without a model in the loop. The live test is
worth running when the CLI updates: the stub can only prove the daemon speaks
the protocol it was written against, not that it is still the protocol the CLI
emits. The protocol itself is documented in
[docs/backendClaude.md](docs/backendClaude.md).

### The Gren side

`web/tests` is a node application built over the same `web/src`, so the browser
code can be tested without a browser. That works for anything importing only
`gren-lang/core`, which is why the anchoring logic lives in `Marks`, and the
document picker's filtering and cursor arithmetic in `Picker`, rather than
inside the view: a module that imports `Html` cannot be compiled for node at
all. The main module there is `Check` rather than `Main` only because `../src` is
on the source path and already has one.

The `Marks` suite covers both of the highlights drawn over the words — the
passage a comment is anchored to, and the passage that changed since the session
started — because they overlap freely and every piece of a line has to answer
for both. It is also the record of every anchoring bug found so far — a quote whose line
wrap is a newline where the document's is a space, a passage inside `**[link]()**`,
a highlight that has to cross into a code span — plus the one limit that is not
fixed: a selection spanning two blocks files its comment but is not highlighted.

```sh
cd web/tests && gren make Check --output=app && node app --help
```

## License

ISC. See [LICENSE](LICENSE).
