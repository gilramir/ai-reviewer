package review

import (
	"strings"
	"unicode"

	"github.com/gilramir/ai-reviewer/internal/mdast"
)

// Anchor ties a comment to a passage of text.
//
// It is deliberately text, not byte offsets. An edit anywhere earlier in the
// file shifts every offset after it, but leaves the words around a passage
// alone, so a quote plus its surroundings survives edits that coordinates do
// not.
type Anchor struct {
	// NodeID is the block the reviewer selected inside. It is a hint for the
	// first lookup and for rendering; it is not trusted after an edit.
	NodeID string `json:"nodeId"`
	// Quote is the selected text as the browser rendered it, so it carries no
	// Markdown syntax even when the source does.
	Quote string `json:"quote"`
	// Prefix and Suffix are the characters immediately around the quote, used
	// to choose between repeated occurrences.
	Prefix string `json:"prefix"`
	Suffix string `json:"suffix"`
}

// Location is where an anchor currently sits in the source.
type Location struct {
	Start int
	End   int
}

// Locate finds an anchor's quote in src.
//
// The search runs over the document as the browser renders it, not over the
// Markdown it renders from. Those are different strings, and only one of them
// is the string the reviewer selected out of: `**[a link](x.md)** and on` is
// `a link and on` on screen. Matching against the source instead means seeing
// past the syntax it carries, and syntax is not only punctuation -- a link's
// destination, an image's alt, an entity, the inside of an HTML tag are all
// ordinary characters that the renderer consumed. Every one of them used to end
// a match early and report a passage that is plainly still there as missing.
//
// So the parser's own tree answers the question. Each run of rendered text
// remembers where it came from, the quote is found among the words, and the
// answer is mapped back to bytes.
func Locate(src string, a Anchor) (Location, bool) {
	return LocateIn(mdast.Flatten([]byte(src)), a)
}

// LocateIn is Locate against a document that has already been flattened, for
// callers with many anchors to place in one document.
func LocateIn(rendered mdast.Rendered, a Anchor) (Location, bool) {
	quote := strings.TrimSpace(a.Quote)
	if quote == "" {
		return Location{}, false
	}

	candidates := findAll(rendered.Text, quote)
	if len(candidates) == 0 {
		return Location{}, false
	}

	best := candidates[0]
	if len(candidates) > 1 {
		bestScore := -1
		for _, candidate := range candidates {
			if score := contextScore(rendered.Text, candidate, a); score > bestScore {
				best, bestScore = candidate, score
			}
		}
	}

	start, end, ok := rendered.Source(best.Start, best.End)
	if !ok {
		return Location{}, false
	}
	return Location{Start: start, End: end}, true
}

// findAll returns every place the quote occurs in the rendered text, as ranges
// over that text.
func findAll(text, quote string) []Location {
	var out []Location
	for i := 0; i < len(text); i++ {
		if end, ok := matchAt(text, quote, i); ok {
			out = append(out, Location{Start: i, End: end})
			// Overlapping matches of the same passage are never distinct
			// anchors, so resume past this one.
			i = end - 1
		}
	}
	return out
}

// matchAt reports whether quote matches text starting at i, allowing the two to
// differ in how whitespace fell.
//
// Whitespace is compared as runs, not as bytes: a soft wrap reaches a browser
// selection as a newline and reaches this text as the space a line break stands
// for, and two blocks are separated here by one newline where a selection
// across them carries two. One run matches another whatever either is made of,
// which is the same rule the client applies.
func matchAt(text, quote string, i int) (int, bool) {
	if len(quote) == 0 || text[i] != quote[0] {
		return 0, false
	}

	ti, qi := i, 0

	for qi < len(quote) {
		if ti >= len(text) {
			return 0, false
		}

		tc, qc := text[ti], quote[qi]

		if isSpaceByte(tc) && isSpaceByte(qc) {
			ti = skipSpace(text, ti)
			qi = skipSpace(quote, qi)
			continue
		}

		if tc != qc {
			return 0, false
		}
		ti++
		qi++
	}

	return ti, true
}

func skipSpace(s string, i int) int {
	for i < len(s) && isSpaceByte(s[i]) {
		i++
	}
	return i
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// contextScore rates a candidate by how much of the recorded prefix and suffix
// it still has around it. Comparison is on letters and digits only, so a
// difference in how whitespace fell does not count against a match.
func contextScore(text string, at Location, a Anchor) int {
	before := normalise(tail(text[:at.Start], 4*len(a.Prefix)+16))
	after := normalise(head(text[at.End:], 4*len(a.Suffix)+16))

	return commonSuffix(before, normalise(a.Prefix)) + commonPrefix(after, normalise(a.Suffix))
}

func normalise(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func commonSuffix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return n
}
