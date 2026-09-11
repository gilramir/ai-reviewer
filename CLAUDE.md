# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

The toolchain (Go, Gren, Node) comes from devbox. Either enter `devbox shell`
once or prefix commands with `devbox run --`.

```sh
make              # web + build: compiles Gren, then the binary to bin/ai-reviewer
make check        # test + vet + gofmt check + gren-format check. Run this before committing.
make test         # Gren tests, then go test ./...
make web          # Gren -> web/dist/app.js (optimised); web-debug for the unoptimised build
make fmt          # gofmt -w and gren-format -r
make tools        # report what is installed against versions.mk
make run          # build, then serve ./docs on 127.0.0.1:8080
```

Single tests:

```sh
go test ./internal/review/ -run TestChangesSinceTheSessionStarted -v
make web-test ARGS=-v                    # all Gren tests, verbose
AI_REVIEWER_LIVE=1 go test ./internal/server -run TestLiveClaude -v   # costs money
```

`web/dist/app.js` is gitignored but `//go:embed`ed by `web/embed.go`, so a
`go build` without a prior `make web` embeds whatever app.js was last compiled.
Use `make`, not `go build`, after touching anything under `web/src`.

`internal/server/testdata/fake-claude` speaks the same stream-json protocol as
the real CLI, so the whole pipeline is testable without a model in the loop.

## Architecture

A Go daemon serves a Gren single-page app over one WebSocket per tab, and drives
one Claude Code CLI process per document under review.

```
browser ──ws──> ai-reviewer (Go) ──pipe──> claude -p --input-format stream-json
   Gren SPA          │                            (one process per document)
                     ├── goldmark ──> JSON AST with source spans
                     ├── fsnotify ──> re-render on change
                     └── git ──────> one commit per turn
```

### The server owns Markdown parsing

`internal/mdast` is the only parser in the system. The browser renders the JSON
tree it is handed and never parses Markdown. A second parser in the client would
be a second opinion about where a paragraph starts, and comment anchors would
drift wherever the two disagreed.

Adding a node kind means teaching **both** `internal/mdast/ast.go` and the
`kindFromTag` decoder in `web/src/Doc.gren`. An unknown kind fails the decode
rather than rendering blank — that is the intended trade.

### Two coordinate systems over the same text

Both are built from the same `ownText` helper, and confusing them silently
misplaces every highlight after the first block.

- **`mdast.Flatten`** joins blocks with `\n` and records where each run came
  from in the source. Used by `internal/review/anchor.go` to locate a quote and
  map it back to byte offsets for editing.
- **`mdast.Words`** and Gren's `Doc.text` join blocks with *nothing*. This is
  what the browser concatenates and what `Marks` counts in — code points, not
  bytes. Change highlights travel the wire in these offsets.

`ownText` in `internal/mdast/rendered.go`, `Doc.text` in `web/src/Doc.gren`, and
`ownText` in `web/src/Main.gren` must agree exactly, down to the single space a
line break stands for. `internal/server/changes_test.go` guards the agreement by
rebuilding the text from the JSON the client receives.

### Anchors are text, not offsets

A thread stores the quoted passage plus the characters either side. An edit
earlier in the file shifts every byte offset after it but leaves the surrounding
words alone. The quote is searched for in the document *as rendered*, because
that is the string the reviewer selected out of: `**[a guide](x.md)** and on`
reads as `a guide and on`, and a selection running out of the link cannot be
matched by skipping syntax characters — `x.md` is ordinary text the renderer
consumed.

Whitespace is compared as runs on both sides: one run matches another whatever
either is made of, because a soft wrap reaches the selection as a newline and
the tree as a space.

### Packages

```
cmd/ai-reviewer/      CLI: serve, password
internal/mdast/       goldmark -> JSON AST with source spans; Flatten, Words
internal/review/      documents, threads, anchoring, assets, turn lifecycle
internal/claudeproc/  the long-lived claude process, one per document
internal/textdiff/    Myers diff over words, for the change highlight
internal/gitstore/    one commit per turn; directory snapshots outside a repo
internal/server/      HTTP, auth, WebSocket
web/src/              Doc (decoder), Protocol (wire), Marks (anchoring +
                      highlights), Picker (document list), Main (app)
```

`docs/developer.md` is the developer-facing write-up: architecture, build, tests,
and the reasoning behind each feature. The README is for users and stays short.

`docs/backendClaude.md` documents the CLI protocol `claudeproc` speaks. Read it
before touching that package.

### Things that are deliberate

- **No Bash tool** is given to the model. The daemon is the only writer of git
  history, and a model that can run commands could rewrite it.
- **`gitstore` has a single writer.** Two documents under review are two
  processes editing two files, and without serialisation their commits race on
  `.git/index.lock`.
- **Claude runs at the repository root, not at `--root`.** Its working directory
  is its permission boundary, and a repo's CLAUDE.md and sibling sources are the
  context a question about a document needs. What the *browser* may open is a
  separate question, and that answer is still `--root`.
- **Two roots, and they are easy to confuse.** `Review.root` is `--root` made
  absolute; `Review.work` is the repository top level, falling back to `root`
  outside a repository. Anything durable is keyed to `work`: the paths handed to
  git, and `.ai-reviewer/state.json` with every document path inside it. Memory
  and the wire are keyed to `root`, which is what the browser speaks.
  `workspacePath` and `reviewPath` convert, and `load`/`save` are the only place
  the boundary is crossed — a state file also holds documents outside this
  review's root, which `load` parks in `Review.foreign` and `save` writes back.
- **Nothing classifies the comment.** "why this?" wants an answer, "reword this"
  wants a change, and many want both. The prompt passes it through and the
  daemon reports what happened by watching which tools were called.
- **Frames are separate Go structs**, not one struct with optional fields,
  because `doc` is a string in some frames and a tree in others. Server side in
  `internal/review/frames.go`, client side in `web/src/Protocol.gren`.

## Gren notes

Gren is Elm-like but not Elm. `when ... is` replaces `case ... of`; there are
**no tuples** (use records); `Array` replaces `List` as the workhorse.

The `Array` API differs from Elm's `List`: `keepIf` not `filter`,
`mapAndKeepJust` not `filterMap`, `mapAndFlatten` not `concatMap`, `findFirst`
returns `Maybe { index, value }`. Check the compiler's suggestions rather than
guessing — it is good at naming the nearby function.

`web/tests` is a node application built over the same `web/src`, so browser code
can be tested without a browser. That only works for modules importing nothing
but `gren-lang/core`, which is why anchoring logic lives in `Marks` and the
picker's filtering in `Picker` rather than inside the view: a module importing
`Html` cannot be compiled for node at all.

`gren-format` is a separate tool from the compiler and needs Node. `make check`
skips it when absent, so a formatting failure can appear later in CI than you
expect.

## Style

Comments explain *why*, not what. The existing ones carry the reasoning behind a
decision, the alternative that was tried and failed, and the bug that forced the
current shape — match that density rather than annotating the obvious.

Commit subjects are a declarative sentence, capitalised, no period and no
`type:` prefix — "Cope with having many files to choose from", "Follow a
highlight to its thread". The body is prose explaining why, including how a
change was verified when it is something the compiler cannot see.
