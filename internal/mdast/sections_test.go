package mdast

import (
	"strings"
	"testing"
)

func TestSectionsSplitAtTheRepeatedHeadingLevel(t *testing.T) {
	// One title and three `##` under it: the title is not a division, the
	// level that repeats is.
	src := []byte(`# A title

Opening words.

## First

Body of the first.

## Second

Body of the second.

## Third

Body of the third.
`)

	rendered := Flatten(src)
	sections := rendered.Sections(0)

	if len(sections) != 4 {
		t.Fatalf("got %d sections, want 4: %+v", len(sections), sections)
	}

	// The title opens a section of its own: a heading shallower than the level
	// the document divides at still divides it.
	titles := []string{"A title", "First", "Second", "Third"}
	for i, want := range titles {
		if sections[i].Title != want {
			t.Errorf("section %d title = %q, want %q", i, sections[i].Title, want)
		}
	}

	if got := rendered.Slice(sections[0]); !contains(got, "A title") || !contains(got, "Opening words") {
		t.Errorf("opening section = %q, want the title and the words under it", got)
	}
	if got := rendered.Slice(sections[2]); !contains(got, "Body of the second") {
		t.Errorf("second section = %q", got)
	}
	if contains(rendered.Slice(sections[2]), "Body of the third") {
		t.Errorf("second section ran into the third")
	}
}

// A section's text has to be a literal substring of the text Locate searches,
// or a quote taken out of one cannot be found in the other. This is the whole
// reason sections are ranges rather than subtrees.
func TestSectionTextIsASubstringOfTheDocument(t *testing.T) {
	src := []byte("# One\n\nSee **[a guide](x.md)** and on.\n\n# Two\n\nMore.\n")

	rendered := Flatten(src)
	for _, section := range rendered.Sections(0) {
		slice := rendered.Slice(section)
		if !contains(rendered.Text, slice) {
			t.Fatalf("section %q is not a substring of %q", slice, rendered.Text)
		}
	}
}

// Content before the first heading has no title, which is how it is told apart
// from a section whose heading is blank.
func TestContentBeforeTheFirstHeadingIsItsOwnSection(t *testing.T) {
	rendered := Flatten([]byte("Loose opening words.\n\n## First\n\nBody.\n\n## Second\n\nMore.\n"))

	sections := rendered.Sections(0)
	if len(sections) != 3 {
		t.Fatalf("got %d sections, want 3: %+v", len(sections), sections)
	}
	if sections[0].Title != "" {
		t.Errorf("untitled opening section was named %q", sections[0].Title)
	}
	if !contains(rendered.Slice(sections[0]), "Loose opening words") {
		t.Errorf("opening section = %q", rendered.Slice(sections[0]))
	}
}

func TestSectionsWithoutHeadingsAreOneSection(t *testing.T) {
	rendered := Flatten([]byte("Just a paragraph.\n\nAnd another.\n"))

	sections := rendered.Sections(0)
	if len(sections) != 1 {
		t.Fatalf("got %d sections, want 1", len(sections))
	}
	if !contains(rendered.Slice(sections[0]), "And another") {
		t.Errorf("the one section is missing the second paragraph")
	}
}

func TestSectionsSplitAtTheTopWhenNoLevelRepeats(t *testing.T) {
	rendered := Flatten([]byte("# One\n\nBody.\n\n## Under one\n\nMore.\n"))

	sections := rendered.Sections(0)
	if len(sections) != 1 {
		t.Fatalf("got %d sections, want 1: %+v", len(sections), sections)
	}
	if sections[0].Title != "One" {
		t.Errorf("title = %q, want One", sections[0].Title)
	}
}

func TestLongSectionsAreCappedOnBlockBoundaries(t *testing.T) {
	src := []byte("## Long\n\n" +
		"Paragraph one is here.\n\nParagraph two is here.\n\nParagraph three is here.\n\n" +
		"## Short\n\nBrief.\n")

	rendered := Flatten(src)
	sections := rendered.Sections(40)

	if len(sections) < 3 {
		t.Fatalf("got %d sections, want the long one split: %+v", len(sections), sections)
	}
	for _, section := range sections {
		// Every piece must still begin and end on a block boundary, so no
		// quote is ever offered half a sentence.
		slice := rendered.Slice(section)
		if !contains(rendered.Text, slice) {
			t.Fatalf("piece %q is not a substring of the document", slice)
		}
	}
	// The continuations are still that section as far as the reviewer is told.
	if sections[0].Title != "Long" || sections[1].Title != "Long" {
		t.Errorf("continuation lost its title: %+v", sections[:2])
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
