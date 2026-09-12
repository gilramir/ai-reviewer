package review

import (
	"fmt"
	"strings"
)

// systemPrompt shapes the model into a document editor rather than a coding
// agent.
//
// The important instruction is the first one. A reviewer's comment is not
// pre-classified as a question or an edit request — "why this?" wants an
// answer, "reword this" wants a change, and plenty of comments want both. The
// daemon does not guess; it passes the comment through and reports what
// actually happened by watching which tools the model called.
const systemPrompt = `You are editing a document that a human reviewer is reading in a browser,
one comment at a time.

Each message gives you a passage the reviewer selected and their comment on it.

- If the comment is a question, answer it. Do not edit the document.
- If the comment asks for a change, make it with the Edit tool, then reply with
  one sentence saying what you changed and why.
- If it is both, do both.

Rules:
- Change only what the comment is about. Never restructure, retitle, or reformat
  surrounding content that nobody asked about.
- Preserve the document's existing voice, terminology, and Markdown conventions.
- Keep replies short. The reviewer is reading them in a narrow sidebar, not a
  terminal.
- If a comment is ambiguous, make the smallest reasonable change and say what
  you assumed. Do not ask a clarifying question and stop; the reviewer may not
  be looking at the thread.`

// commentPrompt renders one reviewer comment for the model.
//
// The passage is quoted verbatim rather than described by offsets: the Edit
// tool matches on exact strings, so quoted text is the form the model can act
// on directly, and it stays valid even though every byte offset in the file
// shifts the moment anything changes.
func commentPrompt(docPath, quote, context, body string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "File: %s\n\n", docPath)
	b.WriteString("The reviewer selected this passage:\n\n")
	b.WriteString(indentQuote(quote))

	if context != "" && context != quote {
		b.WriteString("\nIt appears in this block:\n\n")
		b.WriteString(indentQuote(context))
	}

	b.WriteString("\nTheir comment:\n\n")
	b.WriteString(indentQuote(body))

	return b.String()
}

// replyPrompt continues an existing thread. The model still has the original
// passage in context from earlier in the conversation.
func replyPrompt(body string) string {
	return "The reviewer replied on that same passage:\n\n" + indentQuote(body)
}

func indentQuote(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n") + "\n"
}

// criticSystemPrompt shapes a reviewing pass. It is a second system prompt
// rather than a second paragraph of the first, because the two contradict each
// other outright: the editing prompt says to make the change with the Edit
// tool, and a reviewer that quietly fixes what it found has destroyed the
// review. The process is launched without Edit or Write for the same reason,
// so this only has to explain the job rather than hold a line.
//
// Everything about quoting is here because it is the one thing a pass can get
// wrong in a way the reviewer cannot see. A comment whose passage cannot be
// found is dropped, and a dropped comment looks exactly like a comment that was
// never raised.
const criticSystemPrompt = `You are reviewing a document that a human is reading in a browser, one
section at a time, and raising comments on it for them to answer.

You do not edit. You have no Edit or Write tool and are not meant to have one.
Your job is to say what you would raise if you were reading this over the
author's shoulder.

Each message gives you what to look for and one section of the document, as the
reader sees it: no Markdown syntax, because that is not what they are reading.
Answer with a fenced JSON block and nothing else that matters:

` + "```" + `json
[
  {"quote": "the exact words you are commenting on", "comment": "what you want to say"}
]
` + "```" + `

Rules for the quote:
- Copy it character for character out of the section text in the message.
  Do not quote the file on disk, do not add Markdown, do not tidy the wording.
  A quote that is not in the text is thrown away and your comment with it.
- Quote a whole phrase, not a word or two. Short quotes land on the wrong
  sentence. Eight words is a good length; a full sentence is better.
- One comment per quote. If two things are wrong with one sentence, pick the
  one worth the reviewer's time.

Rules for the comment:
- Say the specific thing. "This contradicts the paragraph above, which says
  the opposite about X" is worth reading; "consider clarifying" is not.
- A question is a fine comment. So is a disagreement. You are not required to
  propose the fix.
- Keep it to a sentence or two. The reviewer reads these in a narrow sidebar.
- Write to the author, not about them.

Say nothing rather than filling a quota. An empty array is the right answer for
a section that is fine, and it is a common answer. Do not comment on a passage
that is already listed as having a comment on it.`

// defaultBrief is what a pass looks for when nobody has said.
//
// It has to say something. "Review this document" reads to a model as find
// fault, and fault is what comes back -- a remark on every paragraph, none of
// it falsifiable. Naming a reader instead gives the pass a test it can apply
// and, more to the point, a reason to stay quiet.
const defaultBrief = `Read this as someone who has not seen it before and needs it to be right.
Raise what makes such a reader stop: a claim that looks wrong, a step that is
missing, a sentence that can be read two ways, a passage that contradicts
another. Say nothing about anything that merely could be phrased differently.`

// criticPrompt asks for one section.
//
// The section's text is quoted in full even though the model has a Read tool
// and could fetch the file, because the file is Markdown and the anchors are
// not: a quote pulled from the source carries syntax that the rendered text the
// reviewer selected from does not have, and it can never be found again.
func criticPrompt(docPath, brief, title, text string, taken []string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "File: %s\n\n", docPath)

	b.WriteString("Look for:\n\n")
	b.WriteString(indentQuote(brief))

	if title != "" {
		fmt.Fprintf(&b, "\nSection: %s\n", title)
	}
	b.WriteString("\nThe section, as the reader sees it:\n\n")
	b.WriteString(indentQuote(text))

	if len(taken) > 0 {
		b.WriteString("\nPassages here that already have a comment on them. Leave these alone:\n\n")
		for _, quote := range taken {
			fmt.Fprintf(&b, "- %s\n", oneLine(quote))
		}
	}

	return b.String()
}

// oneLine flattens a quote for a list. A passage can be a paragraph long, and a
// list of them has to stay a list.
func oneLine(s string) string {
	line := strings.Join(strings.Fields(s), " ")
	const max = 90
	if len(line) > max {
		line = strings.TrimSpace(line[:max]) + "…"
	}
	return line
}

// handoffPrompt introduces a machine thread to the editing conversation.
//
// The pass that raised the comment ran in a different process with a different
// system prompt, so the conversation that has the Edit tool has never seen any
// of this. It is reopenPrompt's problem -- a reply landing where the passage is
// not remembered -- with one more thing to restate.
func handoffPrompt(docPath, quote, context, raised, body string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "File: %s\n\n", docPath)
	b.WriteString("A review of this document raised a comment on this passage:\n\n")
	b.WriteString(indentQuote(quote))

	if context != "" && context != quote {
		b.WriteString("\nIt appears in this block:\n\n")
		b.WriteString(indentQuote(context))
	}

	b.WriteString("\nThe comment was:\n\n")
	b.WriteString(indentQuote(raised))

	b.WriteString("\nThe reviewer's answer to it:\n\n")
	b.WriteString(indentQuote(body))

	return b.String()
}
