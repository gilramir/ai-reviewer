package review

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
			require.Len(t, got, len(test.want), "%+v", got)
			for i, quote := range test.want {
				assert.Equal(t, quote, got[i].Quote, "proposal %d quote", i)
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
	require.Len(t, sections, 2)

	first, at, ok := AnchorIn(rendered, sections[0], "The daemon waits.")
	require.True(t, ok, "the passage was not found in the first section")
	second, also, ok := AnchorIn(rendered, sections[1], "The daemon waits.")
	require.True(t, ok, "the passage was not found in the second section")

	require.NotEqual(t, at.Start, also.Start, "both sections anchored to the same occurrence")
	assert.NotEqual(t, first.Prefix, second.Prefix, "the two occurrences got the same context")
	assert.Contains(t, first.Prefix, "One", "context does not say which heading it sits under")
	assert.Contains(t, second.Prefix, "Two", "context does not say which heading it sits under")

	// And the context is what makes the two tell apart afterwards.
	found, ok := LocateIn(rendered, second)
	require.True(t, ok, "the second anchor could not be re-located")
	assert.Greater(t, found.Start, at.Start, "the second anchor re-located onto the first occurrence")
}

// A quote taken from Markdown source rather than the rendered text cannot be
// found, and the gate has to be the thing that notices.
func TestAQuoteCarryingMarkdownIsNotAnchored(t *testing.T) {
	src := []byte("See **[a guide](x.md)** and on.\n")
	rendered := mdast.Flatten(src)
	sections := rendered.Sections(0)

	_, _, ok := AnchorIn(rendered, sections[0], "**[a guide](x.md)** and on")
	assert.False(t, ok, "a quote carrying syntax the reader never sees was accepted")
	_, _, ok = AnchorIn(rendered, sections[0], "a guide and on")
	assert.True(t, ok, "the same passage as the reader sees it was not found")
}

// The hint a misquote gets back is the whole value of the second look. "Not
// found" is a coin flip to retry against: the model cannot tell whether it
// invented the passage or changed a word in it.
func TestAMisquoteIsToldWhereItDiverged(t *testing.T) {
	src := []byte("# Anchors\n\nThe reviewer selected the passage, and the daemon found it again.\n")
	rendered := mdast.Flatten(src)
	section := rendered.Sections(0)[0]

	gate := &sectionGate{rendered: rendered, section: section, current: rendered}

	// One word wrong, which is the failure that actually happens.
	reason := gate.missing("The reviewer has selected the passage, and the daemon")

	assert.Contains(t, reason, "The reviewer", "the reason does not say how far the quote matched")
	assert.Contains(t, reason, "selected the passage", "the reason does not show what the text actually says")
	assert.NotContains(t, reason, "no part of that", "a quote that mostly matched was reported as wholly absent")
}

// An invented passage gets told so plainly, rather than being handed a hint
// built out of one accidental letter.
func TestAnInventedPassageIsToldItIsAbsent(t *testing.T) {
	rendered := mdast.Flatten([]byte("# Anchors\n\nThe reviewer selected the passage.\n"))
	section := rendered.Sections(0)[0]

	gate := &sectionGate{rendered: rendered, section: section, current: rendered}

	assert.Contains(t, gate.missing("§§ nowhere at all §§"), "no part of that", "want it to say the passage is simply absent")
}

// The prefix search stops at the first miss, which is only sound because a
// quote whose opening is absent cannot have a longer opening that is present.
func TestTheLongestMatchingPrefixIsFound(t *testing.T) {
	rendered := mdast.Flatten([]byte("alpha beta gamma delta\n"))
	section := rendered.Sections(0)[0]

	prefix, at, ok := longestPrefixIn(rendered, section, "alpha beta GAMMA")
	require.True(t, ok, "nothing matched at all")
	assert.Equal(t, "alpha beta ", prefix)
	assert.Equal(t, 0, at.Start, "prefix location")

	_, _, ok = longestPrefixIn(rendered, section, "zeta")
	assert.False(t, ok, "a quote with nothing in common reported a matching prefix")
}

// Quotes arrive from a model and can hold anything. Cutting one to find its
// longest matching prefix must not cut a rune in half.
func TestPrefixSearchDoesNotSplitARune(t *testing.T) {
	rendered := mdast.Flatten([]byte("the naïve approach — it fails\n"))
	section := rendered.Sections(0)[0]

	prefix, _, ok := longestPrefixIn(rendered, section, "the naïve approach — and then")
	require.True(t, ok, "nothing matched")
	assert.True(t, utf8.ValidString(prefix), "prefix %q is not valid UTF-8", prefix)
	assert.Contains(t, prefix, "naïve", "want it past the accented word")
}

// A rejection the model can do nothing about is not sent back to it: asking for
// an overlapping passage again invites it to widen the quote until the check
// stops noticing.
func TestAnOverlappingProposalIsSkippedRatherThanRefused(t *testing.T) {
	rendered := mdast.Flatten([]byte("# Doc\n\nThe system retries indefinitely until it succeeds.\n"))
	section := rendered.Sections(0)[0]

	rev := newReview(t, t.TempDir())
	gate := &sectionGate{rev: rev, rendered: rendered, section: section, current: rendered}

	first := proposal{Quote: "The system retries indefinitely until it succeeds.", Comment: "One."}
	_, ok := gate.admit(first)
	require.True(t, ok, "the first proposal was refused")

	inside := proposal{Quote: "retries indefinitely until it succeeds", Comment: "Two."}
	why, ok := gate.admit(inside)
	assert.True(t, ok, "an overlapping proposal was sent back for repair: %q", why.reason)
	assert.Len(t, gate.kept, 1, "want the overlapping one dropped quietly")
}
