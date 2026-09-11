package review

import (
	"os"
	"path/filepath"
	"testing"

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
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	rev := newReview(t, root)
	if _, _, err := rev.render("spec.md"); err != nil {
		t.Fatalf("first render: %v", err)
	}

	if err := os.WriteFile(path, []byte(after), 0o644); err != nil {
		t.Fatal(err)
	}
	_, changes, err := rev.render("spec.md")
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
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
			if got := highlighted(t, c.after, changes); got != c.want {
				t.Errorf("\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

// The first sight of a document is the baseline, so nothing on it is new -- and
// that has to hold for a file created in the middle of a session too.
func TestTheFirstRenderHasNoChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "spec.md"), []byte("Some words.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rev := newReview(t, root)
	_, changes, err := rev.render("spec.md")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes on a first render = %v, want none", changes)
	}
}

// Every edit is measured against the session, not against the render before it:
// two turns in a row leave both of their changes lit.
func TestChangesAccumulateAcrossTurns(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "spec.md")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("One two three four.\n")
	rev := newReview(t, root)
	if _, _, err := rev.render("spec.md"); err != nil {
		t.Fatal(err)
	}

	write("One TWO three four.\n")
	if _, _, err := rev.render("spec.md"); err != nil {
		t.Fatal(err)
	}

	const final = "One TWO three FOUR.\n"
	write(final)
	_, changes, err := rev.render("spec.md")
	if err != nil {
		t.Fatal(err)
	}

	if got, want := highlighted(t, final, changes), "One [TWO] three [FOUR.]"; got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}
