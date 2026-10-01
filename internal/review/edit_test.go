package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const editableDoc = `# Retry policy

The system SHALL retry **indefinitely** until the operation succeeds.

Unrelated paragraph.
`

func newDoc(t *testing.T) (*Review, string) {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "spec.md"), []byte(editableDoc), 0o644))
	return newReview(t, root), root
}

// The passage the browser rendered has no asterisks in it; the passage in the
// file does. What the reviewer is handed to edit must be the second one, or
// saving it back would silently unbold the sentence.
func TestSourceOfKeepsTheMarkdownTheRendererAte(t *testing.T) {
	rev, _ := newDoc(t)

	source, err := rev.SourceOf("spec.md", Anchor{Quote: "retry indefinitely until"})
	require.NoError(t, err, "SourceOf")
	assert.Equal(t, "retry **indefinitely** until", source, "want the passage with its markup")
}

func TestApplyEditWritesTheReviewersOwnWords(t *testing.T) {
	rev, root := newDoc(t)

	const original = "retry **indefinitely** until"
	const replacement = "retry up to **five times** before"

	commit, err := rev.ApplyEdit("spec.md", Anchor{Quote: "retry indefinitely until"}, original, replacement)
	require.NoError(t, err, "ApplyEdit")
	assert.NotEmpty(t, commit, "the edit was not recorded")

	after, err := os.ReadFile(filepath.Join(root, "spec.md"))
	require.NoError(t, err)
	assert.Contains(t, string(after), "The system SHALL "+replacement+" the operation succeeds.", "document does not carry the edit")
	// Only the passage moves: the rest of the file is untouched.
	assert.Contains(t, string(after), "Unrelated paragraph.", "the edit disturbed the rest of the document")
}

// The anchor still finds the passage after the model has rewritten it, so
// locating it is not enough to know it still says what the editor was opened
// on. Without the comparison, a reviewer who left an editor open would
// overwrite a change they never saw.
func TestApplyEditRefusesAPassageThatChangedUnderIt(t *testing.T) {
	rev, root := newDoc(t)

	edited := strings.Replace(editableDoc, "**indefinitely**", "**three times**", 1)
	require.NoError(t, os.WriteFile(filepath.Join(root, "spec.md"), []byte(edited), 0o644))

	_, err := rev.ApplyEdit(
		"spec.md",
		Anchor{Quote: "retry three times until"},
		"retry **indefinitely** until",
		"retry once until",
	)
	require.Error(t, err, "ApplyEdit accepted an edit based on stale source")
	assert.Contains(t, err.Error(), "changed while you were editing", "want it to say the passage moved on")

	after, _ := os.ReadFile(filepath.Join(root, "spec.md"))
	assert.Equal(t, edited, string(after), "the refused edit was written anyway")
}

func TestApplyEditRefusesAVanishedPassage(t *testing.T) {
	rev, _ := newDoc(t)

	_, err := rev.ApplyEdit("spec.md", Anchor{Quote: "no such sentence"}, "no such sentence", "something")
	assert.ErrorContains(t, err, "selected passage", "want the missing passage named")
}

// A turn is about to write this file from a copy it read before the edit
// existed. Whichever landed second would erase the other without either side
// noticing, so the edit waits.
func TestApplyEditWaitsForATurn(t *testing.T) {
	rev, _ := newDoc(t)

	rev.mu.Lock()
	rev.inflight["spec.md"] = 1
	rev.mu.Unlock()

	_, err := rev.ApplyEdit("spec.md", Anchor{Quote: "Unrelated paragraph."}, "Unrelated paragraph.", "Gone.")
	assert.ErrorContains(t, err, "turn is running", "want the running turn named")
}

func TestHandEditSubjectNamesThePassage(t *testing.T) {
	assert.Equal(t, "review: hand edit of “the retry policy”", handEditSubject("  the retry\npolicy "))
	long := handEditSubject(strings.Repeat("a", 80))
	assert.LessOrEqual(t, len([]rune(long)), 70, "subject was not shortened: %q", long)
}
