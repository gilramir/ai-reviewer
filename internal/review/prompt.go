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
