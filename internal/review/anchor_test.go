package review

import (
	"strings"
	"testing"
)

func loc(t *testing.T, src string, a Anchor) string {
	t.Helper()
	at, ok := Locate(src, a)
	if !ok {
		t.Fatalf("anchor %q not located in %q", a.Quote, src)
	}
	return src[at.Start:at.End]
}

func TestExactMatch(t *testing.T) {
	src := "The system SHALL retry indefinitely.\n"
	got := loc(t, src, Anchor{Quote: "SHALL retry indefinitely"})
	if got != "SHALL retry indefinitely" {
		t.Errorf("got %q", got)
	}
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
			got := loc(t, tc.src, Anchor{Quote: tc.quote})
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A paragraph wrapped across source lines renders as one run of text.
func TestMatchesAcrossSoftWrap(t *testing.T) {
	src := "The system SHALL retry\nindefinitely until it succeeds.\n"
	got := loc(t, src, Anchor{Quote: "SHALL retry indefinitely until"})
	if got != "SHALL retry\nindefinitely until" {
		t.Errorf("got %q", got)
	}
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
	if got := src[first.Start:]; got[:30] != "Retry policy applies to reads." {
		t.Errorf("chose the wrong occurrence: %q", got[:30])
	}

	second := Locate2(t, src, Anchor{
		Quote:  "Retry policy",
		Suffix: " applies to writes.",
	})
	if got := src[second.Start:]; got[:31] != "Retry policy applies to writes." {
		t.Errorf("chose the wrong occurrence: %q", got[:31])
	}
	if first.Start == second.Start {
		t.Error("both anchors resolved to the same occurrence")
	}
}

// Locate2 is a test helper that fails rather than returning a flag.
func Locate2(t *testing.T, src string, a Anchor) Location {
	t.Helper()
	at, ok := Locate(src, a)
	if !ok {
		t.Fatalf("anchor %q not located", a.Quote)
	}
	return at
}

func TestMissingQuoteReportsNotFound(t *testing.T) {
	if _, ok := Locate("nothing to see", Anchor{Quote: "a passage that was deleted"}); ok {
		t.Error("expected a deleted passage to fail to locate")
	}
}

func TestEmptyQuoteIsNotAnAnchor(t *testing.T) {
	if _, ok := Locate("some text", Anchor{Quote: "   "}); ok {
		t.Error("whitespace-only quote should not locate")
	}
}

// A match must start on the words themselves, never on the syntax before them,
// or the highlight would sit one character to the left of the passage.
func TestMatchStartsOnContent(t *testing.T) {
	src := "Text with **bold words** in it."
	at, ok := Locate(src, Anchor{Quote: "bold words"})
	if !ok {
		t.Fatal("not located")
	}
	if src[at.Start] != 'b' {
		t.Errorf("match starts at %q, want the letter b", src[at.Start])
	}
}

func TestLocatesAfterAnEarlierEdit(t *testing.T) {
	before := "# Title\n\nFirst para.\n\nThe retry policy is strict.\n"
	after := "# Title\n\nFirst para, now considerably longer than it was.\n\nThe retry policy is strict.\n"

	anchor := Anchor{Quote: "retry policy is strict", Prefix: "The ", Suffix: "."}

	from, _ := Locate(before, anchor)
	to, ok := Locate(after, anchor)
	if !ok {
		t.Fatal("anchor lost after an unrelated edit earlier in the file")
	}
	if from.Start == to.Start {
		t.Fatal("test is not exercising a shift; offsets should differ")
	}
	if got := after[to.Start:to.End]; got != "retry policy is strict" {
		t.Errorf("relocated to %q", got)
	}
}

func TestFindAllDoesNotOverlap(t *testing.T) {
	// "aa" inside "aaaa" must yield two matches, not three overlapping ones.
	got := findAll("aaaa", "aa")
	if len(got) != 2 {
		t.Fatalf("want 2 non-overlapping matches, got %d: %v", len(got), got)
	}
	if got[0].End > got[1].Start {
		t.Errorf("matches overlap: %v", got)
	}
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
	if !ok {
		t.Fatal("passage not found; a comment on it would be refused")
	}
	got := src[at.Start:at.End]
	if !strings.HasPrefix(got, "The three doors") || !strings.HasSuffix(got, "is yours,") {
		t.Errorf("located %q", got)
	}
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
		if _, ok := Locate(src, Anchor{Quote: quote}); !ok {
			t.Errorf("quote %q not found", quote)
		}
	}
}

// Folding whitespace must not fold words together: a quote whose words are
// joined is a different passage, not a wrapped one.
func TestWhitespaceFoldingDoesNotJoinWords(t *testing.T) {
	const src = "one two three\n"
	for _, quote := range []string{"onetwo", "two three four"} {
		if _, ok := Locate(src, Anchor{Quote: quote}); ok {
			t.Errorf("quote %q should not have matched", quote)
		}
	}
}
