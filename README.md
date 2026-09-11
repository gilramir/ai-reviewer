# ai-reviewer

<img src="docs/ai-reviewer.png" alt="" width="140">

Review Markdown documents in a browser, with Claude Code editing them under you.

![A document under review. In the margin, two resolved threads and a live one where Claude answered a question and then made the edit it was asked for. On the page, the passages that changed since the session started are tinted, the passage the open thread is anchored to is highlighted, and a new comment is being written against the current selection.](docs/screenshot.png)

Select a passage, type a comment — *"why this?"*, *"reword this"* — and the
answer appears in the margin, or the document changes and re-renders. Every
change that lands is a commit on a review branch the daemon cuts from the
branch you were on, so a review session leaves a diff you can read and undo.
Nothing reaches your own branch until you merge the review branch yourself.

It drives the **Claude Code CLI**, not the API. There is no API key to manage:
if `claude` works in your terminal, it works here.

## Build

You need Go and Gren; `devbox shell` provides both, or install them yourself.

```sh
make           # builds bin/ai-reviewer
```

The result is one static binary with the frontend embedded. The machine that
serves a review needs nothing else — build once and `scp` it. Details, tests
and the rest of the toolchain are in [docs/developer.md](docs/developer.md).

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

The daemon prints where it is and how to get in:

```
  ai-reviewer

    reviewing  /home/you/docs
    branch     review/docs-2026-09-08
    open       http://192.168.1.14:8080/
    password   quiet-harbor-lamp-09
```

The password is new on every start. For one that survives restarts, run
`ai-reviewer password` once; it stores a digest in
`~/.config/ai-reviewer/config.json`. Authentication is on by default, and
`--no-auth` is accepted only on a loopback address.

`ai-reviewer serve --help` lists the flags with their defaults.

## Reviewing

**Choosing a document.** The top bar names the document you are reading.
Click it to see the others: type to narrow the list, arrows to walk it, Enter
to open, Escape to put it away. Words match in any order, so `review readme`
finds `internal/review/README.md`.

**Commenting.** Select a passage and write. A question gets an answer in the
margin; a request gets the document edited and re-rendered, with the thread
recording what was done. Shift-Enter sends, in the comment box, the editor,
and a thread's reply box alike.

**Editing it yourself.** When you already have the words you want, **Edit it
myself** opens the passage as Markdown source. Save writes it straight to the
file as its own commit; no turn runs and nothing is spent. **Clear** empties
the box for the common case of taking a passage out. An edit is refused if the
passage changed underneath you or a turn is running on that document, and the
words come back intact.

**What changed while you were reading.** Words the document did not have when
the session started are tinted, so a few turns in you can still see what moved.
It compares words, not lines, so re-wrapping a paragraph marks nothing.
Deletions leave nothing to mark; `git log` on the review branch has the whole
truth. The **changes** row in the settings panel turns the tint off.

**Images.** `![](flow.png)` is served from the review root, and the daemon
watches every file there, not only the Markdown. Regenerate a diagram yourself
and the documents that embed it re-render on their own.

## Settings

The pill in the top bar names the model — `opus`, `sonnet`, or *default model*
when the choice is left to your own `claude` configuration. Clicking it opens a
panel with the rest: the tools, the permission mode, where Claude runs, the
review branch, what the session has cost, and which `claude` binary is
answering.

- **Model** can be changed mid-review. Each document's conversation resumes on
  the new model with its next comment, so nothing already said is lost. The
  choice is remembered across restarts; `--model` on the command line
  overrides it.
- **Clear context** starts every document's conversation over. Your threads
  stay. Worth doing after a model change, since a resumed conversation carries
  every word the old model said and pays for it again on each turn. The panel
  shows how many turns there are to clear and what they cost.
- **Theme** — light, dark, or *auto*. This one is yours rather than the
  review's: it lives in the browser, so two people on the same review can
  disagree. It needs a browser from 2024 or later.

## Landing the changes

When the session is over, every change the review produced — the model's edits
and your hand edits alike — is sitting as commits on the review branch. Your
target branch has not been touched. **Merging is your job**: the daemon never
does it, because a merge can conflict and a page is no place to resolve one.

The settings panel names the review branch, counts what is waiting on it, and
spells out each way to land it:

```
  branch    review/docs-2026-09-09
            4 commits not yet in main

            keep the commits, one per turn
            git switch main && git merge review/docs-2026-09-09

            or fold the review into one commit
            git switch main && git merge --squash review/docs-2026-09-09 && git commit

            then, if you want the branch gone
            git branch -d review/docs-2026-09-09
```

Stopping the daemon prints the same three at more length, at the moment you are
back in a terminal to act on them. Either way you run them yourself.

**Bring the commits across.** One per turn, each with the comment that caused
it as its message:

```sh
git switch main                     # the branch you were on before the review
git merge review/docs-2026-09-09
```

**Or fold them into one.** A squash merge lands the same final text as a single
commit, with a message you write:

```sh
git switch main
git merge --squash review/docs-2026-09-09
git commit                          # your editor opens for the message
```

Take the first when the turn-by-turn history is worth keeping — it is a record
of what was asked and what changed in answer. Take the second when the review
was one piece of work and the branch would just be noise in `git log`.

**Then delete the review branch**, if you want it gone:

```sh
git branch -d review/docs-2026-09-09
```

After a squash merge that is refused: the single commit on `main` is not the
commits on the branch, so git cannot tell they landed. Check with `git log` or
`git diff main review/docs-2026-09-09` that nothing is left behind, then use a
capital `-D` to delete it anyway. `-D` is also how you throw a review away
having merged none of it.

Nothing here is one-way. Until you merge, the review branch is an ordinary
branch: read it with `git log`, `git switch` to it and back, rebase or
cherry-pick it as you would any other.

Outside a git repository there is no branch. The daemon keeps a snapshot of
each file before a turn changes it instead.

## Where the state lives

`.ai-reviewer/` is created beside `.git` (or in the review root, outside a
repository). It holds `state.json` — threads, each document's Claude session
and spend, the review branch, the chosen model — and, outside a repository, the
snapshots. **Add it to your `.gitignore`**: it will sit in `git status` until
you do.

The password is not in there. It is a digest in
`~/.config/ai-reviewer/config.json` and belongs to you, not to any one review.

## Security

- Claude runs with `Read, Edit, Write, Grep, Glob` and no Bash, and with none
  of your personal MCP servers. The daemon is the only writer of git history.
- Claude's working directory is the repository root, not `--root`, so it can
  read the sources a document links to. That is also its reach: it can edit
  anything in the repository, the same as when you run `claude` there yourself.
  The browser can still only open what is under `--root`.
- WebSocket upgrades require a matching `Origin`, so a page you visit while
  logged in cannot drive Claude against your documents.

The reasoning behind each of these is in [docs/developer.md](docs/developer.md).

## Known issues

**Sessions do not survive a restart.** A restart prints a new password and
sends the browser back to the login page. Anything you clicked while
disconnected is queued and sent on reconnect.

## For developers

- [docs/developer.md](docs/developer.md) — architecture, build, tests, and
  the reasoning behind the design.
- [docs/backendClaude.md](docs/backendClaude.md) — how to drive the Claude
  Code CLI from your own program, which is what this daemon does.

## License

ISC. See [LICENSE](LICENSE).
