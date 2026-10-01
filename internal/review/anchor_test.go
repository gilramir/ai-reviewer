package review

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loc(t *testing.T, src string, a Anchor) string {
	t.Helper()
	at, ok := Locate(src, a)
	require.True(t, ok, "anchor %q not located in %q", a.Quote, src)
	return src[at.Start:at.End]
}

func TestExactMatch(t *testing.T) {
	src := "The system SHALL retry indefinitely.\n"
	assert.Equal(t, "SHALL retry indefinitely", loc(t, src, Anchor{Quote: "SHALL retry indefinitely"}))
}

// The browser hands us rendered text, so a selection spanning bold or code
// carries none of the syntax that is still in the file.
func TestMatchesAcrossInlineMarkup(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		quote string
		want  string
	}{
		{"bold", "A quoted **caution** here.", "quoted caution here", "quoted **caution** here"},
		{"italic", "An *emphatic* claim.", "An emphatic claim", "An *emphatic* claim"},
		{"code", "Call `retry()` twice.", "Call retry() twice", "Call `retry()` twice"},
		{"link", "See [the spec](http://x) now.", "See the spec", "See [the spec"},
		{"nested", "Use **`hard`** limits.", "Use hard limits", "Use **`hard`** limits"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, loc(t, tc.src, Anchor{Quote: tc.quote}))
		})
	}
}

// A paragraph wrapped across source lines renders as one run of text.
func TestMatchesAcrossSoftWrap(t *testing.T) {
	src := "The system SHALL retry\nindefinitely until it succeeds.\n"
	assert.Equal(t, "SHALL retry\nindefinitely until", loc(t, src, Anchor{Quote: "SHALL retry indefinitely until"}))
}

func TestPrefixAndSuffixDisambiguate(t *testing.T) {
	src := "" +
		"Retry policy applies to reads.\n" +
		"\n" +
		"Retry policy applies to writes.\n"

	first := Locate2(t, src, Anchor{
		Quote:  "Retry policy",
		Suffix: " applies to reads.",
	})
	assert.Equal(t, "Retry policy applies to reads.", src[first.Start:][:30], "chose the wrong occurrence")

	second := Locate2(t, src, Anchor{
		Quote:  "Retry policy",
		Suffix: " applies to writes.",
	})
	assert.Equal(t, "Retry policy applies to writes.", src[second.Start:][:31], "chose the wrong occurrence")
	assert.NotEqual(t, first.Start, second.Start, "both anchors resolved to the same occurrence")
}

// Locate2 is a test helper that fails rather than returning a flag.
func Locate2(t *testing.T, src string, a Anchor) Location {
	t.Helper()
	at, ok := Locate(src, a)
	require.True(t, ok, "anchor %q not located", a.Quote)
	return at
}

func TestMissingQuoteReportsNotFound(t *testing.T) {
	_, ok := Locate("nothing to see", Anchor{Quote: "a passage that was deleted"})
	assert.False(t, ok, "expected a deleted passage to fail to locate")
}

func TestEmptyQuoteIsNotAnAnchor(t *testing.T) {
	_, ok := Locate("some text", Anchor{Quote: "   "})
	assert.False(t, ok, "whitespace-only quote should not locate")
}

// A match must start on the words themselves, never on the syntax before them,
// or the highlight would sit one character to the left of the passage.
func TestMatchStartsOnContent(t *testing.T) {
	src := "Text with **bold words** in it."
	at, ok := Locate(src, Anchor{Quote: "bold words"})
	require.True(t, ok, "not located")
	assert.Equal(t, byte('b'), src[at.Start], "match should start at the letter b")
}

func TestLocatesAfterAnEarlierEdit(t *testing.T) {
	before := "# Title\n\nFirst para.\n\nThe retry policy is strict.\n"
	after := "# Title\n\nFirst para, now considerably longer than it was.\n\nThe retry policy is strict.\n"

	anchor := Anchor{Quote: "retry policy is strict", Prefix: "The ", Suffix: "."}

	from, _ := Locate(before, anchor)
	to, ok := Locate(after, anchor)
	require.True(t, ok, "anchor lost after an unrelated edit earlier in the file")
	require.NotEqual(t, from.Start, to.Start, "test is not exercising a shift; offsets should differ")
	assert.Equal(t, "retry policy is strict", after[to.Start:to.End])
}

func TestFindAllDoesNotOverlap(t *testing.T) {
	// "aa" inside "aaaa" must yield two matches, not three overlapping ones.
	got := findAll("aaaa", "aa")
	require.Len(t, got, 2, "want 2 non-overlapping matches")
	assert.LessOrEqual(t, got[0].End, got[1].Start, "matches overlap: %v", got)
}

// TestMatchesAcrossSoftWrapInAnIndentedBlock is the case a reviewer hits first,
// because prose in a bullet list wraps like prose anywhere else.
//
// The browser renders a soft wrap as a line break, so a selection crossing one
// carries a bare "\n". The source has that newline plus the list item's indent.
// Folding whitespace as runs is what lets the two meet; comparing the newlines
// byte for byte leaves the indent behind and the passage looks deleted.
func TestMatchesAcrossSoftWrapInAnIndentedBlock(t *testing.T) {
	const src = "  - **[Driving a C or C++ library from Gren](native.md)** -- the general\n" +
		"    problem, of which this binding is one instance. The three doors out of a\n" +
		"    Gren program and which one is yours, the four ways C can reach Node and\n" +
		"    how to choose.\n"

	// Exactly what a browser hands over for a selection across the wrap.
	const quote = "The three doors out of a\nGren program and which one is yours,"

	at, ok := Locate(src, Anchor{Quote: quote})
	require.True(t, ok, "passage not found; a comment on it would be refused")
	got := src[at.Start:at.End]
	assert.True(t, strings.HasPrefix(got, "The three doors"), "located %q", got)
	assert.True(t, strings.HasSuffix(got, "is yours,"), "located %q", got)
}

// The same selection with the wrap as a space, which is what some browsers give
// and what the old code happened to be tested with.
func TestMatchesAcrossSoftWrapAsASpace(t *testing.T) {
	const src = "  - a list item whose prose wraps\n    onto the next line.\n"

	for _, quote := range []string{
		"prose wraps onto the next",
		"prose wraps\nonto the next",
		"prose wraps\n    onto the next",
		"prose wraps  \n  onto the next",
	} {
		_, ok := Locate(src, Anchor{Quote: quote})
		assert.True(t, ok, "quote %q not found", quote)
	}
}

// Folding whitespace must not fold words together: a quote whose words are
// joined is a different passage, not a wrapped one.
func TestWhitespaceFoldingDoesNotJoinWords(t *testing.T) {
	const src = "one two three\n"
	for _, quote := range []string{"onetwo", "two three four"} {
		_, ok := Locate(src, Anchor{Quote: quote})
		assert.False(t, ok, "quote %q should not have matched", quote)
	}
}

// The bug this was rewritten for. A bullet that opens with a link is the most
// ordinary shape in a document of links, and a selection running out of the
// link and into the words after it used to be reported as missing: the matcher
// walked the source, skipped the `]` and the `(`, and then met the destination,
// which is ordinary text the renderer ate.
func TestMatchesOutOfALinkAndIntoTheTextAfterIt(t *testing.T) {
	const src = `  - **[Turbo Vision, from Gren](widgets.md)** -- what you are programming
    against. The programming model, the anatomy of the screen.
`
	at := locate(t, src, Anchor{Quote: "Turbo Vision, from Gren -- what you are programming"})

	assert.Equal(t, "Turbo Vision, from Gren](widgets.md)** -- what you are programming", src[at.Start:at.End], "source span")
}

// The whole bullet, wrap and all, which is what a reviewer selects when they
// mean "this item".
func TestMatchesAWholeBulletAcrossItsWrap(t *testing.T) {
	const src = `  - **[Turbo Vision, from Gren](widgets.md)** -- what you are programming
    against. The programming model.
`
	quote := "Turbo Vision, from Gren -- what you are programming\nagainst. The programming model."
	at := locate(t, src, Anchor{Quote: quote})

	got := src[at.Start:at.End]
	assert.True(t, strings.HasSuffix(got, "The programming model."), "source span = %q, want it to reach the end of the bullet", got)
}

// An image contributes its alt text to the page but nothing to a selection, and
// its URL is not on screen at all.
func TestMatchesAcrossAnImage(t *testing.T) {
	const src = "Before ![a screenshot of the editor](shots/editor.png) after the picture.\n"

	at := locate(t, src, Anchor{Quote: "Before  after the picture."})
	assert.Equal(t, src[:len(src)-1], src[at.Start:at.End], "source span")
}

// Inline code renders as its contents; the backticks are not on screen.
func TestMatchesOutOfACodeSpan(t *testing.T) {
	const src = "Set `Copied.toSystem = False` and nothing reaches the clipboard.\n"

	at := locate(t, src, Anchor{Quote: "Copied.toSystem = False and nothing reaches"})
	assert.Equal(t, "Copied.toSystem = False` and nothing reaches", src[at.Start:at.End], "source span")
}

// A selection that runs from one paragraph into the next carries the blank line
// between them as whitespace, and the rendered text has one newline there.
func TestMatchesAcrossTwoBlocks(t *testing.T) {
	const src = "The first paragraph ends here.\n\nThe second one starts here.\n"

	at := locate(t, src, Anchor{Quote: "ends here.\n\nThe second one"})
	assert.Equal(t, "ends here.\n\nThe second one", src[at.Start:at.End], "source span")
}

// A heading is a block like any other, and its hashes are not on screen.
func TestMatchesAHeading(t *testing.T) {
	const src = "# gren-tvision documentation\n\nTurbo Vision terminal UIs.\n"

	at := locate(t, src, Anchor{Quote: "gren-tvision documentation"})
	assert.Equal(t, "gren-tvision documentation", src[at.Start:at.End], "source span")
}

// The passage the hand editor hands back is the source under the selection, so
// a location that is off by a byte is an edit that eats one.
func TestSpanIsTheSourceUnderTheSelection(t *testing.T) {
	const src = "A sentence with **bold words** in the middle of it.\n"

	at := locate(t, src, Anchor{Quote: "bold words"})
	assert.Equal(t, "bold words", src[at.Start:at.End], "want the words without their syntax")
}

func locate(t *testing.T, src string, a Anchor) Location {
	t.Helper()
	at, ok := Locate(src, a)
	require.True(t, ok, "Locate did not find %q", a.Quote)
	return at
}
