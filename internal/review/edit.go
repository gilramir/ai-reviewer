package review

import (
	"fmt"
	"os"
	"strings"

	"github.com/gilramir/ai-reviewer/internal/gitstore"
)

// A hand edit is the reviewer changing a passage themselves, with no model turn
// behind it. Plenty of comments are not questions at all — the reviewer already
// has the words they want, and spending a turn to have them typed back is both
// slower and paid for by the token.
//
// It goes through the same anchor, the same re-render and the same commit as a
// turn does, so a review session's history reads the same whether a change came
// from the model or from the person reading.

// SourceOf returns the Markdown behind an anchored passage.
//
// The browser cannot supply this itself: it renders a tree the server parsed and
// never sees the source, so the words on screen are the passage with its syntax
// already eaten. Handing back the rendered text as something to edit would erase
// every asterisk and link the renderer hid.
func (r *Review) SourceOf(docPath string, anchor Anchor) (string, error) {
	src, err := r.read(docPath)
	if err != nil {
		return "", err
	}

	at, ok := Locate(string(src), anchor)
	if !ok {
		return "", fmt.Errorf("could not find the selected passage in %s", docPath)
	}
	return string(src[at.Start:at.End]), nil
}

// ApplyEdit replaces an anchored passage with the reviewer's own text and
// records the change, returning the commit it landed in.
//
// original is the source the reviewer started from, and it has to still be
// there: the anchor says where the passage is now, but not that it says the
// same thing it did when the editor opened. Comparing it makes this a
// compare-and-swap, so a change the model made in the meantime is refused
// rather than silently overwritten.
func (r *Review) ApplyEdit(docPath string, anchor Anchor, original, replacement string) (string, error) {
	full, err := r.resolve(docPath)
	if err != nil {
		return "", err
	}

	// The document has one writer at a time. A turn in flight is about to edit
	// this file from a copy it read before this edit existed, and neither side
	// would notice the other.
	if r.turnsRunning(docPath) > 0 {
		return "", fmt.Errorf("a turn is running on %s; wait for it to finish", docPath)
	}

	src, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}

	at, ok := Locate(string(src), anchor)
	if !ok {
		return "", fmt.Errorf("could not find the selected passage in %s", docPath)
	}
	if string(src[at.Start:at.End]) != original {
		return "", fmt.Errorf("the passage in %s changed while you were editing it", docPath)
	}
	if replacement == original {
		// Nothing to write, but the editor still closes: the reviewer looked at
		// the passage and left it alone, which is a finished edit.
		r.publish(editAppliedFrame{Type: "editApplied", Doc: docPath, Quote: anchor.Quote})
		return "", nil
	}

	updated := string(src[:at.Start]) + replacement + string(src[at.End:])
	if err := writeSynced(full, []byte(updated)); err != nil {
		return "", err
	}

	commit := r.recordHandEdit(docPath, full, anchor.Quote)

	// The watcher would push this render a moment later anyway, but the
	// reviewer just pressed Save: the document redraws now, not after the
	// settle delay.
	r.reanchor(docPath, updated)
	if err := r.PublishDoc(docPath); err != nil {
		r.PublishError(err.Error())
	}
	r.broadcastThreads(docPath)
	r.publish(editAppliedFrame{Type: "editApplied", Doc: docPath, Quote: anchor.Quote, Commit: commit})
	_ = r.save()

	return commit, nil
}

// turnsRunning reports how many turns are in flight against a document.
func (r *Review) turnsRunning(docPath string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inflight[docPath]
}

// recordHandEdit commits a change the reviewer made themselves. The trailer is
// what separates the two kinds of change in `git log`: a turn carries the thread
// it came from, a hand edit carries nobody's reasoning but the reviewer's.
func (r *Review) recordHandEdit(docPath, full, quote string) string {
	paths := r.relativise([]string{full})
	if len(paths) == 0 {
		return ""
	}

	ref, err := r.hist.Record(gitstore.Commit{
		Paths:   paths,
		Subject: handEditSubject(quote),
		Body:    fmt.Sprintf("Document: %s\nEdited by the reviewer; no model turn ran.", r.workspacePath(docPath)),
		Trailers: []gitstore.Trailer{
			{Key: "Review-Edit", Value: "hand"},
		},
	})
	if err != nil {
		r.publish(errorFrame{Type: "error", Message: "could not record the change: " + err.Error()})
		return ""
	}
	return ref
}

// handEditSubject names the passage that was changed. There is no comment to
// use as a subject here — nobody said what they wanted, they just wrote it.
func handEditSubject(quote string) string {
	line := strings.Join(strings.Fields(quote), " ")

	const max = 44
	if runes := []rune(line); len(runes) > max {
		line = strings.TrimSpace(string(runes[:max])) + "…"
	}
	if line == "" {
		return "review: hand edit"
	}
	return "review: hand edit of “" + line + "”"
}
