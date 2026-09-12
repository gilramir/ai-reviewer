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

// contextWindow is how much text either side of a quote is kept to tell
// repeated occurrences apart. It matches what the browser records for a
// reviewer's own selection, so both kinds of anchor are scored on the same
// amount of evidence.
const contextWindow = 48

// AnchorIn builds an anchor for a quote inside one section of a document.
//
// A reviewer's anchor arrives with its surroundings already attached, because
// the browser took them from the selection. A model's quote arrives bare, and a
// bare quote is not an anchor: "the reviewer" occurs a dozen times in a document
// about reviewing, and the thread has to say which one. So the context is read
// off wherever the quote was found. That also means the two halves of the system
// choose between repeated occurrences on the same evidence afterwards, which a
// quote carrying no context leaves them free to do differently.
//
// The search is confined to the section the model was shown. A quote it took
// from the text in front of it must be found there and not in some other part
// of the document that happens to say the same thing.
//
// The Location returned is in rendered-text coordinates, not the source
// coordinates Locate reports. It is for comparing one proposal against another
// within a pass, and nothing durable is keyed to it. Call Locate for an answer
// about the file.
func AnchorIn(rendered mdast.Rendered, section mdast.Section, quote string) (Anchor, Location, bool) {
	trimmed := strings.TrimSpace(quote)
	if trimmed == "" {
		return Anchor{}, Location{}, false
	}

	at, ok := firstIn(rendered.Text, trimmed, section.Start, section.End)
	if !ok {
		return Anchor{}, Location{}, false
	}

	return Anchor{
		Quote:  rendered.Text[at.Start:at.End],
		Prefix: tailRunes(rendered.Text[:at.Start], contextWindow),
		Suffix: headRunes(rendered.Text[at.End:], contextWindow),
	}, at, true
}

// firstIn is findAll bounded to one stretch of the text, reporting the first
// match that begins inside it. A match may run past the end: a quote taken from
// the last line of a section is still that section's.
func firstIn(text, quote string, from, to int) (Location, bool) {
	if from < 0 {
		from = 0
	}
	if to > len(text) {
		to = len(text)
	}
	for i := from; i < to; i++ {
		if end, ok := matchAt(text, quote, i); ok {
			return Location{Start: i, End: end}, true
		}
	}
	return Location{}, false
}

// headRunes and tailRunes cut on rune boundaries. The context is compared
// letter by letter after normalising, so a half-written rune at the edge would
// not change an answer -- but it would travel the wire and land in a state file,
// and neither of those is a place to keep broken UTF-8.
func headRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

func tailRunes(s string, n int) string {
	starts := make([]int, 0, n+1)
	for i := range s {
		starts = append(starts, i)
		if len(starts) > n {
			starts = starts[1:]
		}
	}
	if len(starts) == 0 {
		return ""
	}
	return s[starts[0]:]
}
