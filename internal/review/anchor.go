package review

import (
	"strings"
	"unicode"
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

// markupBytes are the characters the renderer consumes, which therefore appear
// in the source but never in the text the browser handed us. Skipping them lets
// a quote of "a quoted caution" match a source of "a quoted **caution**".
const markupBytes = "*_`~[]()\\"

// Locate finds an anchor's quote in src.
//
// It tries an exact match first, then a match that tolerates Markdown syntax
// inside the passage. When a quote occurs more than once, the occurrence whose
// neighbouring text best matches the recorded prefix and suffix wins, which is
// what keeps a comment on "the retry policy" attached to the paragraph the
// reviewer meant rather than the first one mentioning it.
func Locate(src string, a Anchor) (Location, bool) {
	quote := strings.TrimSpace(a.Quote)
	if quote == "" {
		return Location{}, false
	}

	candidates := findAll(src, quote)
	if len(candidates) == 0 {
		return Location{}, false
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}

	best, bestScore := candidates[0], -1
	for _, c := range candidates {
		if score := contextScore(src, c, a); score > bestScore {
			best, bestScore = c, score
		}
	}
	return best, true
}

// findAll returns every position where the quote matches, exactly or modulo
// Markdown syntax and whitespace folding.
func findAll(src, quote string) []Location {
	var out []Location
	for i := 0; i < len(src); i++ {
		if end, ok := matchAt(src, quote, i); ok {
			out = append(out, Location{Start: i, End: end})
			// Overlapping matches of the same passage are never distinct
			// anchors, so resume past this one.
			i = end - 1
		}
	}
	return out
}

// matchAt reports whether quote matches src starting at i, allowing the source
// to contain Markdown syntax the renderer would have removed and to differ in
// how whitespace is broken across lines.
func matchAt(src, quote string, i int) (int, bool) {
	// The match must begin on the passage's own first character. Without this,
	// a quote of "bold words" would match "**bold words**" starting at the
	// asterisk, and the highlight would sit two characters to the left of the
	// text it describes.
	if len(quote) == 0 || src[i] != quote[0] {
		return 0, false
	}

	si, qi := i, 0

	for qi < len(quote) {
		if si >= len(src) {
			return 0, false
		}

		sc, qc := src[si], quote[qi]
		if sc == qc {
			si++
			qi++
			continue
		}

		// A line break in the source is a single space once rendered.
		if isSpaceByte(sc) && isSpaceByte(qc) {
			si = skipSpace(src, si)
			qi = skipSpace(quote, qi)
			continue
		}

		// Syntax the renderer ate. Only skippable in the source: the quote came
		// from rendered text and never contains it in this position.
		if strings.IndexByte(markupBytes, sc) >= 0 {
			si++
			continue
		}

		return 0, false
	}

	return si, true
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
// it still has around it. Comparison is on letters and digits only, so the
// Markdown syntax between words does not count against a match.
func contextScore(src string, at Location, a Anchor) int {
	before := normalise(tail(src[:at.Start], 4*len(a.Prefix)+16))
	after := normalise(head(src[at.End:], 4*len(a.Suffix)+16))

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
