# A machine reviewer

**Status: a proposal.** Nothing here is built. It is written down so it can be
argued with before any code moves.

Today the human reviews and the model works: the reviewer selects a passage,
comments on it, and the daemon reports what the model did about it. This
proposes the other direction — the model reads a document and raises its own
comments, and the human answers them.

The two directions share almost everything that is hard. A thread is an anchor
plus a transcript, and it does not care who wrote the first message. The
highlight, the re-anchoring after an edit, the outdating of a passage that was
rewritten, the click from a highlighted passage to the conversation about it:
all of it already works on any anchored thread. What is new is where the anchor
comes from, and that turns out to be the whole design.

## The inversion

A reviewer's thread is a question aimed at the model, and `StatusOpen` means the
reviewer has the answer and can act on it. A machine thread reverses who owes
whom: the model has said its piece and is waiting on a person. That is the only
genuinely new state in the system, and it does not need a new status — `open`
still means *someone owes something*, and which someone is derivable from who
wrote the last message. Adding statuses is not free: each one has to be taught
to the Gren decoder in `Protocol.gren` and to `reanchor`, and the four that
exist are already the vocabulary the whole client reasons in.

## The brief

A pass is asked for with a brief: *check that the argument holds*, *cut what
repeats*, *is this still true of the code*. The trigger is still one button —
the brief is a setting rather than something typed each time — but a pass
without one is worse than no pass.

**The brief is the stopping rule.** With no brief every sentence is in scope,
and a model with unbounded scope produces uniform shallow coverage: a *consider
tightening this* on each paragraph, none of it falsifiable, and no reason ever
to say nothing at all. A brief makes *not what you asked about* a reason to stay
quiet, so a section with nothing to say about brevity files nothing rather than
filing two polite remarks. This is the density problem from the other end:
sectioning makes the count scale with the document, and the brief makes it scale
with what is actually wrong.

An empty box every time is no better. A reviewer does not know what to ask for
until they have seen what is there, and a field that has to be filled in gets
`review this` typed into it, which is the bare trigger with friction in front of
it.

So the default lives in a file, `.ai-reviewer/review.md`, and the trigger takes
a one-off override.

A file rather than a field on `Settings`, for two reasons. `Settings` is
assembled on demand precisely because every field in it already has an owner
elsewhere, and a brief invented there would be the second copy that design
exists to avoid. And a brief is a thing to edit, diff and commit beside the
documents it governs, which is already how this repository carries its
conventions — in a CLAUDE.md the critic can read, since it runs at the workspace
root.

**The shipped default still has to say something.** *"Review this document"*
reads to a model as *find fault*, and fault is what comes back. A default worth
having names a reader rather than a task: read this as someone who has not seen
it before, and raise what makes them stop.

**The brief sets priority, not a filter.** A pass briefed on brevity that
notices a factual error and swallows it is worse than one that raises it out of
turn. That belongs in the critic's system prompt, where it costs a sentence and
no schema at all.

**The brief is per pass, and it is recorded on the thread.** Two passes over one
document under different briefs is an ordinary thing to want — *now check
whether it is true* — and it is cheap, because the document is already in that
conversation's context. Keeping the brief on each thread answers "why is this
comment here?" three passes later, and gives the sidebar something to group by.
It is one string recording what was asked, not a taxonomy inferred from what the
model meant, so it does not reopen the question of classifying comments.

It also relaxes deduplication. Two comments on one sentence for two different
reasons are two threads, legitimately. The anchors handed to a pass should
suppress only what the same brief has already raised.

## One section at a time

The daemon walks the document's top-level heading sections and runs one turn per
section. The reviewer asks for a review once; the iteration is the daemon's.

**Because the quote has to come from somewhere.** A proposed comment is only a
comment if `review.Locate` can find its passage, and `Locate` searches the
document *as rendered*. A model given the Read tool quotes the Markdown source —
`**[a guide](x.md)**` — and that string does not occur in the rendered text,
which reads `a guide`. This is the same wall the original anchoring design hit,
and it has the same answer: there is one string the passage can be quoted out
of, and it is the one `mdast.Flatten` produces. So the section's rendered text
goes in the prompt and the instruction is to quote out of *that*, verbatim. A
section is the unit that makes this affordable; a whole document's rendered text
in every prompt is both large and, worse, full of phrases that repeat.

**Because density should scale with the document.** Asked to review a whole
file, a model returns about five comments whether the file is three hundred
words or five thousand. Per section, the count follows the document.

**Because the reviewer can start before the pass finishes.** Threads land as
each section completes, the sidebar fills from the top, and interrupting means
keeping what has arrived rather than losing the turn.

**Because re-review is then incremental.** After three suggestions are accepted
the text has moved, and only the sections that moved need looking at again. The
anchors of the threads already open on a section can go in the prompt too, so a
pass does not raise again what the same brief has already raised.

A document with no headings is one section. A section longer than some cap is
split on paragraph boundaries — the cap is about prompt size and about how much
text a single quote can go missing in, not about the heading structure.

## The proposal needs an answer, not just a reply

Every proposed comment must pass `Locate` before a thread exists. A suggestion
whose passage cannot be found is not a suggestion; it is a fragment of prose
about a document, and the reviewer has nowhere to put it.

That check wants to happen while the model can still do something about it. If
the pass ends with a JSON array of findings, an unanchorable quote can only be
dropped — after it was paid for, and without the model ever learning that the
text it quoted was not the text it was given. The check belongs inside the turn:
a tool the daemon answers, called once per finding, which either files the
thread or comes back with *that passage is not in the section I gave you;
requote it*. The same round-trip gives streaming for free — threads appear one
at a time, as they are proposed — and it fits the existing shape of the system,
where the daemon learns what happened by watching which tools were called.

The cost is an MCP server over stdio, which is new machinery in `claudeproc`.
Asking for a fenced JSON block and parsing the final text is a reasonable first
version, as long as it is understood as one: the anchoring failure mode is the
thing the design is *for*, and the cheap version has no answer to it.

## The critic is a second process, and the code already insists

`prompt.go`'s system prompt says *make it with the Edit tool*. A critic must not
edit — a machine reviewer that quietly fixes what it found has destroyed the
review. So the two need different system prompts, and the system prompt lives in
`claudeproc.Config` on the **Manager**, one for every session it hands out.
Sessions are keyed by document path, so a second session id for the same
document is not enough; this needs a second Manager.

That is the right answer anyway. Two managers, `editors` and `critics`, each
with its own `MaxLive` cap, and the critic launched with `AllowedTools` of
`Read`, `Grep`, `Glob` and nothing else. Taking Edit and Write away at the
process boundary removes a whole class of "it fixed it instead of asking" rather
than asking the prompt to hold that line.

The two also want different context. The editing conversation holds the document
and every comment on it, and is served from the prompt cache; the reviewing
conversation holds a document and a list of its own criticisms. Keeping them in
one conversation would flood the first and bias the second.

## Replying to a machine thread

A reply goes to the **editing** session, not to the critic. *"Yes, fix that"* has
to land where the Edit tool is, and the critic cannot act on it.

The editing conversation has never seen the proposal, so the passage and the
model's own comment have to be stated to it. That mechanism exists:
`reopenPrompt` already restates a passage for a reply landing in a conversation
that no longer remembers it, because the context was cleared. Generalising it to
also carry the original machine comment is a small change, and then replying on
a proposed thread needs no new turn machinery at all.

## Arranging them

Sort threads by where their passage falls, not by when they were filed. A
machine pass files a section's worth at once and their creation order is an
artefact of the model's output; document order is what a reviewer reads in.

The client already locates every thread's range to draw the highlight, so
sorting by `range.start` there is nearly free, with creation order as the
tiebreak and unanchored threads at the bottom. The server's `order` stays
chronological: that is a record of what happened, and it is what persistence
and `reanchor` walk.

## Two client gaps this will expose

Both pre-date the proposal. Both are survivable today because a human selects
passages, and neither survives a model quoting them.

**`Marks.find` ignores the context it is given.** It returns the first
occurrence of the quote and never looks at `prefix` or `suffix`, while the
server's `Locate` scores every candidate by how much of the surrounding text it
still has. A human selects a passage they can see, which is usually long enough
to be unique. A machine quotes *"the reviewer"* or *"one per document"*, and
then the server anchors the thread to the right sentence while the browser
highlights the wrong one. The client needs the same context scoring, or machine
quotes need a floor on how short they may be — probably both.

**`threadAt` keeps only the first range covering a character.** Two threads
whose passages overlap, and one highlight is lost. Rare when a human is
selecting; routine when a section's review produces a comment on a sentence and
another on a clause inside it.

## What not to build

**Do not classify the comment on the way in.** That principle stands and is
unaffected: *"why this?"* and *"reword this"* are still not worth guessing
between. Outbound is a different question — the model knows whether it is asking
or suggesting, so a `kind` on a machine thread is reported fact rather than
inference. But the brief on the thread already supplies the grouping a `kind`
would have been wanted for, and it does it without anybody choosing from a
vocabulary. Leave it out until something needs it that the brief cannot answer.

**Do not add a `declined` status yet.** *"I looked and I disagree"* is different
information from *"done"*, and a review whose record cannot distinguish them has
lost the interesting half. But it is a status, with the decoder and `reanchor`
cost every status has, and reusing `resolved` with the reason in the reviewer's
last message loses little. Worth revisiting once there is something that reads
the record back.

## The shape of the change

```
Thread.Origin               reviewer | model, on the wire and in state.json
Thread.Brief                what the pass was asked for, kept per thread
.ai-reviewer/review.md      the default brief; a built-in one when absent
internal/review/critic.go   sectioning, the pass, the proposal gate
internal/review/prompt.go   a second system prompt; no Edit tool in it
internal/claudeproc/        a second Manager, or a per-session prompt
web/src/Marks.gren          context scoring in find; overlap in threadAt
web/src/Main.gren           document-order sort; "awaiting you" in the sidebar
```

Staged so that each step is worth having on its own: sectioning and the pass
with JSON parsing first, since that is what proves whether the comments are any
good — with the brief from the beginning, because an unbriefed pass is not the
idea being judged; the client anchoring fixes next, because that is where it
will visibly misbehave; the proposal tool last, when the JSON version's failure
rate says how much it is needed.
