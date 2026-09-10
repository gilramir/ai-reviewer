package mdast

import "testing"

func TestFlattenIsWhatTheReaderSees(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "a link is its text",
			src:  "See **[the widgets guide](widgets.md)** -- start here.\n",
			want: "See the widgets guide -- start here.\n",
		},
		{
			name: "an image is nothing at all",
			src:  "Before ![a screenshot](shots/editor.png) after.\n",
			want: "Before  after.\n",
		},
		{
			name: "a code span is its contents",
			src:  "Set `Copied.toSystem = False` first.\n",
			want: "Set Copied.toSystem = False first.\n",
		},
		{
			name: "a heading loses its hashes",
			src:  "## The clipboard\n\nAnd then some prose.\n",
			want: "The clipboard\nAnd then some prose.\n",
		},
		{
			name: "a soft wrap is the space the line break stands for",
			src:  "One sentence that runs\nacross two lines.\n",
			want: "One sentence that runs across two lines.\n",
		},
		{
			name: "list items are blocks, and separated as such",
			src:  "  - first item\n  - second item\n",
			want: "first item\nsecond item\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Flatten([]byte(tt.src)).Text; got != tt.want {
				t.Errorf("Flatten(%q).Text = %q, want %q", tt.src, got, tt.want)
			}
		})
	}
}

// The mapping is what an anchor is worth: a passage found among the words has
// to come back as the bytes under it, or an edit lands in the wrong place.
func TestSourceMapsBackToTheBytes(t *testing.T) {
	const src = "See **[the widgets guide](widgets.md)** -- start here.\n"
	rendered := Flatten([]byte(src))

	at := indexOf(t, rendered.Text, "widgets guide -- start")
	start, end, ok := rendered.Source(at, at+len("widgets guide -- start"))
	if !ok {
		t.Fatal("Source did not map a range it had just produced")
	}
	if got := src[start:end]; got != "widgets guide](widgets.md)** -- start" {
		t.Errorf("source = %q", got)
	}
}

// A run that carries no source of its own -- the space a line break stands for
// -- must not swallow the passage next to it.
func TestSourceAcrossALineBreak(t *testing.T) {
	const src = "One sentence that runs\nacross two lines.\n"
	rendered := Flatten([]byte(src))

	at := indexOf(t, rendered.Text, "runs across two")
	start, end, ok := rendered.Source(at, at+len("runs across two"))
	if !ok {
		t.Fatal("Source did not map across the line break")
	}
	if got := src[start:end]; got != "runs\nacross two" {
		t.Errorf("source = %q", got)
	}
}

func TestSourceRefusesAnEmptyRange(t *testing.T) {
	rendered := Flatten([]byte("Some prose.\n"))
	if _, _, ok := rendered.Source(3, 3); ok {
		t.Error("an empty range was mapped")
	}
	if _, _, ok := rendered.Source(0, 9999); ok {
		t.Error("a range past the end was mapped")
	}
}

func indexOf(t *testing.T, text, want string) int {
	t.Helper()
	for i := 0; i+len(want) <= len(text); i++ {
		if text[i:i+len(want)] == want {
			return i
		}
	}
	t.Fatalf("%q is not in the rendered text %q", want, text)
	return 0
}
