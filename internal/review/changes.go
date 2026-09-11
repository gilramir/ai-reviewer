package review

import (
	"github.com/gilramir/ai-reviewer/internal/mdast"
	"github.com/gilramir/ai-reviewer/internal/textdiff"
)

// A review is a conversation about a document that keeps moving underneath it.
// After a few turns it is genuinely hard to say which sentences are the ones
// that moved: the model reports what it did in the margin, but the page it did
// it to looks much as it did before. So the words a document had when this
// daemon first rendered it are kept, and anything on the page that is not among
// them is marked.
//
// The baseline is this run's, not the branch's. "Since the session started" is
// the question a reviewer is actually asking when they look up from a reply and
// wonder what just happened, and a restart is a fair place for that question to
// start over. `git log` on the review branch is still the record of everything.

// Range is a half-open span of a document's text, counted in the code point
// offsets the browser counts in -- the same coordinates a comment's highlight
// uses, so the two are drawn by the same code.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// changesIn returns the passages of a freshly parsed document that were not in
// it when the session started, and remembers a document's words the first time
// it sees them.
//
// A document seen for the first time has no changes by definition, which is
// also the right answer for one created mid-session: what it says is what it
// has always said, as far as this reviewer is concerned.
func (r *Review) changesIn(docPath string, root mdast.Node) []Range {
	tokens := mdast.Words(root)
	now := make([]string, len(tokens))
	for i, token := range tokens {
		now[i] = token.Text
	}

	r.mu.Lock()
	before, seen := r.baseline[docPath]
	if !seen {
		r.baseline[docPath] = now
	}
	r.mu.Unlock()

	if !seen {
		return nil
	}
	return spans(tokens, textdiff.Changed(before, now))
}

// spans turns marked words into the passages the browser highlights.
//
// Whitespace is never highlighted for its own sake: a mark hanging off the end
// of a line is a smudge rather than information, and a paragraph the model only
// re-wrapped would otherwise light up along every seam. Between two marked
// words it is swallowed, so a rewritten sentence is one highlight and not a row
// of them with gaps.
func spans(tokens []mdast.Token, changed []bool) []Range {
	var out []Range
	last := -1 // the word the span being built ends on

	for i, token := range tokens {
		if token.Space || !changed[i] {
			continue
		}
		if last >= 0 && onlySpace(tokens[last+1:i]) {
			out[len(out)-1].End = token.End
		} else {
			out = append(out, Range{Start: token.Start, End: token.End})
		}
		last = i
	}
	return out
}

func onlySpace(tokens []mdast.Token) bool {
	for _, token := range tokens {
		if !token.Space {
			return false
		}
	}
	return true
}
