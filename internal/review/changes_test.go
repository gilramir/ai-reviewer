package review

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gilramir/ai-reviewer/internal/mdast"
)

// rendered is what the reviewer sees: the document's text with the markup gone,
// which is the string the change offsets index into.
func rendered(t *testing.T, src string) []rune {
	t.Helper()
	var out []rune
	for _, token := range mdast.Words(mdast.Render("", 0, []byte(src)).Root) {
		out = append(out, []rune(token.Text)...)
	}
	return out
}

// highlighted renders a set of change spans the way the page does, so a failing
// case reads as the passage that lit up.
func highlighted(t *testing.T, src string, changes []Range) string {
	t.Helper()
	text := rendered(t, src)

	var out []rune
	at := 0
	for _, change := range changes {
		out = append(out, text[at:change.Start]...)
		out = append(out, '[')
		out = append(out, text[change.Start:change.End]...)
		out = append(out, ']')
		at = change.End
	}
	return string(append(out, text[at:]...))
}

// changesAfter opens a document, edits it, and returns what the second render
// says has changed since the first.
func changesAfter(t *testing.T, before, after string) []Range {
	t.Helper()

	root := t.TempDir()
	path := filepath.Join(root, "spec.md")
	require.NoError(t, os.WriteFile(path, []byte(before), 0o644))

	rev := newReview(t, root)
	_, _, err := rev.render("spec.md")
	require.NoError(t, err, "first render")

	require.NoError(t, os.WriteFile(path, []byte(after), 0o644))
	_, changes, err := rev.render("spec.md")
	require.NoError(t, err, "second render")
	return changes
}

func TestChangesSinceTheSessionStarted(t *testing.T) {
	cases := []struct {
		name   string
		before string
		after  string
		want   string
	}{
		{
			name:   "a word the model replaced",
			before: "The cat sat on the mat.\n",
			after:  "The dog sat on the mat.\n",
			want:   "The [dog] sat on the mat.",
		},
		{
			name:   "a rewritten clause is one highlight, not four",
			before: "The cat sat on the mat.\n",
			after:  "The cat lay down beside the mat.\n",
			want:   "The cat [lay down beside] the mat.",
		},
		{
			name:   "a whole new paragraph",
			before: "First.\n",
			after:  "First.\n\nSecond one.\n",
			want:   "First.[Second one.]",
		},
		{
			name: "re-wrapping a paragraph changes nothing",
			before: "The quick brown fox jumps over the lazy dog and keeps\n" +
				"on running.\n",
			after: "The quick brown fox jumps\nover the lazy dog and keeps on running.\n",
			want:  "The quick brown fox jumps over the lazy dog and keeps on running.",
		},
		{
			name:   "markup the reader cannot see is not a change",
			before: "A *word* here.\n",
			after:  "A **word** here.\n",
			want:   "A word here.",
		},
		{
			name:   "a deleted sentence leaves nothing marked",
			before: "One. Two. Three.\n",
			after:  "One. Three.\n",
			want:   "One. Three.",
		},
		{
			name:   "a line rewritten inside a code block",
			before: "```go\nx := 1\ny := 2\n```\n",
			after:  "```go\nx := 1\ny := 3\n```\n",
			want:   "x := 1\ny := [3]\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			changes := changesAfter(t, c.before, c.after)
			assert.Equal(t, c.want, highlighted(t, c.after, changes))
		})
	}
}

// The first sight of a document is the baseline, so nothing on it is new -- and
// that has to hold for a file created in the middle of a session too.
func TestTheFirstRenderHasNoChanges(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "spec.md"), []byte("Some words.\n"), 0o644))

	rev := newReview(t, root)
	_, changes, err := rev.render("spec.md")
	require.NoError(t, err, "render")
	assert.Empty(t, changes, "changes on a first render")
}

// Every edit is measured against the session, not against the render before it:
// two turns in a row leave both of their changes lit.
func TestChangesAccumulateAcrossTurns(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "spec.md")
	write := func(content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	write("One two three four.\n")
	rev := newReview(t, root)
	_, _, err := rev.render("spec.md")
	require.NoError(t, err)

	write("One TWO three four.\n")
	_, _, err = rev.render("spec.md")
	require.NoError(t, err)

	const final = "One TWO three FOUR.\n"
	write(final)
	_, changes, err := rev.render("spec.md")
	require.NoError(t, err)

	assert.Equal(t, "One [TWO] three [FOUR.]", highlighted(t, final, changes))
}
