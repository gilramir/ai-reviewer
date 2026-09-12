package review

import (
	"strings"
	"testing"

	"github.com/gilramir/ai-reviewer/internal/mdast"
)

// A model sends the array it was asked for wrapped in however much prose it
// felt like, and sometimes shows the shape before filling it in.
func TestProposalsAreReadOutOfWhateverTheModelWrapsThemIn(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  []string
	}{
		{
			name:  "a bare array",
			reply: `[{"quote":"the words","comment":"a remark"}]`,
			want:  []string{"the words"},
		},
		{
			name:  "a fenced array under some prose",
			reply: "Here is what I found.\n\n```json\n[{\"quote\":\"the words\",\"comment\":\"a remark\"}]\n```\n",
			want:  []string{"the words"},
		},
		{
			name: "the answer after an example",
			reply: "The shape is:\n\n```json\n[{\"quote\":\"...\",\"comment\":\"...\"}]\n```\n\n" +
				"And my answer:\n\n```json\n[{\"quote\":\"the real one\",\"comment\":\"a remark\"}]\n```\n",
			want: []string{"the real one"},
		},
		{
			name:  "an empty array, which is a common and correct answer",
			reply: "Nothing to raise here.\n\n```json\n[]\n```\n",
			want:  nil,
		},
		{
			name:  "prose and no array at all",
			reply: "I think the second paragraph is wrong.",
			want:  nil,
		},
		{
			name:  "a fence that was never closed",
			reply: "```json\n[{\"quote\":\"the words\",\"comment\":\"a remark\"}]\n",
			want:  []string{"the words"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := parseProposals(test.reply)
			if len(got) != len(test.want) {
				t.Fatalf("got %d proposals, want %d: %+v", len(got), len(test.want), got)
			}
			for i, quote := range test.want {
				if got[i].Quote != quote {
					t.Errorf("proposal %d quote = %q, want %q", i, got[i].Quote, quote)
				}
			}
		})
	}
}

// The model is handed rendered text, so its quote carries no Markdown. Finding
// it has to give back the surroundings too, or a phrase that repeats anchors
// wherever it first occurs rather than where it was meant.
func TestAnchorInReadsTheContextOffThePlaceItFound(t *testing.T) {
	src := []byte("# One\n\nThe daemon waits.\n\n# Two\n\nThe daemon waits.\n")
	rendered := mdast.Flatten(src)

	sections := rendered.Sections(0)
	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2", len(sections))
	}

	first, at, ok := AnchorIn(rendered, sections[0], "The daemon waits.")
	if !ok {
		t.Fatal("the passage was not found in the first section")
	}
	second, also, ok := AnchorIn(rendered, sections[1], "The daemon waits.")
	if !ok {
		t.Fatal("the passage was not found in the second section")
	}

	if at.Start == also.Start {
		t.Fatal("both sections anchored to the same occurrence")
	}
	if first.Prefix == second.Prefix {
		t.Errorf("the two occurrences got the same context: %q", first.Prefix)
	}
	if !strings.Contains(first.Prefix, "One") || !strings.Contains(second.Prefix, "Two") {
		t.Errorf("context does not say which heading it sits under: %q / %q", first.Prefix, second.Prefix)
	}

	// And the context is what makes the two tell apart afterwards.
	if found, ok := LocateIn(rendered, second); !ok {
		t.Fatal("the second anchor could not be re-located")
	} else if found.Start <= at.Start {
		t.Errorf("the second anchor re-located onto the first occurrence")
	}
}

// A quote taken from Markdown source rather than the rendered text cannot be
// found, and the gate has to be the thing that notices.
func TestAQuoteCarryingMarkdownIsNotAnchored(t *testing.T) {
	src := []byte("See **[a guide](x.md)** and on.\n")
	rendered := mdast.Flatten(src)
	sections := rendered.Sections(0)

	if _, _, ok := AnchorIn(rendered, sections[0], "**[a guide](x.md)** and on"); ok {
		t.Error("a quote carrying syntax the reader never sees was accepted")
	}
	if _, _, ok := AnchorIn(rendered, sections[0], "a guide and on"); !ok {
		t.Error("the same passage as the reader sees it was not found")
	}
}
