package mdast

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	require.Len(t, sections, 4, "%+v", sections)

	// The title opens a section of its own: a heading shallower than the level
	// the document divides at still divides it.
	titles := []string{"A title", "First", "Second", "Third"}
	for i, want := range titles {
		assert.Equal(t, want, sections[i].Title, "section %d title", i)
	}

	opening := rendered.Slice(sections[0])
	assert.Contains(t, opening, "A title", "the opening section holds the title")
	assert.Contains(t, opening, "Opening words", "the opening section holds the words under the title")
	assert.Contains(t, rendered.Slice(sections[2]), "Body of the second")
	assert.NotContains(t, rendered.Slice(sections[2]), "Body of the third", "second section ran into the third")
}

// A section's text has to be a literal substring of the text Locate searches,
// or a quote taken out of one cannot be found in the other. This is the whole
// reason sections are ranges rather than subtrees.
func TestSectionTextIsASubstringOfTheDocument(t *testing.T) {
	src := []byte("# One\n\nSee **[a guide](x.md)** and on.\n\n# Two\n\nMore.\n")

	rendered := Flatten(src)
	for _, section := range rendered.Sections(0) {
		require.Contains(t, rendered.Text, rendered.Slice(section), "a section is not a substring of the document")
	}
}

// Content before the first heading has no title, which is how it is told apart
// from a section whose heading is blank.
func TestContentBeforeTheFirstHeadingIsItsOwnSection(t *testing.T) {
	rendered := Flatten([]byte("Loose opening words.\n\n## First\n\nBody.\n\n## Second\n\nMore.\n"))

	sections := rendered.Sections(0)
	require.Len(t, sections, 3, "%+v", sections)
	assert.Empty(t, sections[0].Title, "untitled opening section was named")
	assert.Contains(t, rendered.Slice(sections[0]), "Loose opening words")
}

func TestSectionsWithoutHeadingsAreOneSection(t *testing.T) {
	rendered := Flatten([]byte("Just a paragraph.\n\nAnd another.\n"))

	sections := rendered.Sections(0)
	require.Len(t, sections, 1)
	assert.Contains(t, rendered.Slice(sections[0]), "And another", "the one section is missing the second paragraph")
}

func TestSectionsSplitAtTheTopWhenNoLevelRepeats(t *testing.T) {
	rendered := Flatten([]byte("# One\n\nBody.\n\n## Under one\n\nMore.\n"))

	sections := rendered.Sections(0)
	require.Len(t, sections, 1, "%+v", sections)
	assert.Equal(t, "One", sections[0].Title)
}

func TestLongSectionsAreCappedOnBlockBoundaries(t *testing.T) {
	src := []byte("## Long\n\n" +
		"Paragraph one is here.\n\nParagraph two is here.\n\nParagraph three is here.\n\n" +
		"## Short\n\nBrief.\n")

	rendered := Flatten(src)
	sections := rendered.Sections(40)

	require.GreaterOrEqual(t, len(sections), 3, "want the long one split: %+v", sections)
	for _, section := range sections {
		// Every piece must still begin and end on a block boundary, so no
		// quote is ever offered half a sentence.
		require.Contains(t, rendered.Text, rendered.Slice(section), "a piece is not a substring of the document")
	}
	// The continuations are still that section as far as the reviewer is told.
	assert.Equal(t, "Long", sections[0].Title, "continuation lost its title: %+v", sections[:2])
	assert.Equal(t, "Long", sections[1].Title, "continuation lost its title: %+v", sections[:2])
}
