package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const editableDoc = `# Retry policy

The system SHALL retry **indefinitely** until the operation succeeds.

Unrelated paragraph.
`

func newDoc(t *testing.T) (*Review, string) {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "spec.md"), []byte(editableDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	return newReview(t, root), root
}

// The passage the browser rendered has no asterisks in it; the passage in the
// file does. What the reviewer is handed to edit must be the second one, or
// saving it back would silently unbold the sentence.
func TestSourceOfKeepsTheMarkdownTheRendererAte(t *testing.T) {
	rev, _ := newDoc(t)

	source, err := rev.SourceOf("spec.md", Anchor{Quote: "retry indefinitely until"})
	if err != nil {
		t.Fatalf("SourceOf: %v", err)
	}
	if source != "retry **indefinitely** until" {
		t.Errorf("source = %q, want the passage with its markup", source)
	}
}

func TestApplyEditWritesTheReviewersOwnWords(t *testing.T) {
	rev, root := newDoc(t)

	const original = "retry **indefinitely** until"
	const replacement = "retry up to **five times** before"

	commit, err := rev.ApplyEdit("spec.md", Anchor{Quote: "retry indefinitely until"}, original, replacement)
	if err != nil {
		t.Fatalf("ApplyEdit: %v", err)
	}
	if commit == "" {
		t.Error("the edit was not recorded")
	}

	after, err := os.ReadFile(filepath.Join(root, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "The system SHALL "+replacement+" the operation succeeds.") {
		t.Errorf("document does not carry the edit:\n%s", after)
	}
	// Only the passage moves: the rest of the file is untouched.
	if !strings.Contains(string(after), "Unrelated paragraph.") {
		t.Errorf("the edit disturbed the rest of the document:\n%s", after)
	}
}

// The anchor still finds the passage after the model has rewritten it, so
// locating it is not enough to know it still says what the editor was opened
// on. Without the comparison, a reviewer who left an editor open would
// overwrite a change they never saw.
func TestApplyEditRefusesAPassageThatChangedUnderIt(t *testing.T) {
	rev, root := newDoc(t)

	edited := strings.Replace(editableDoc, "**indefinitely**", "**three times**", 1)
	if err := os.WriteFile(filepath.Join(root, "spec.md"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := rev.ApplyEdit(
		"spec.md",
		Anchor{Quote: "retry three times until"},
		"retry **indefinitely** until",
		"retry once until",
	)
	if err == nil {
		t.Fatal("ApplyEdit accepted an edit based on stale source")
	}
	if !strings.Contains(err.Error(), "changed while you were editing") {
		t.Errorf("error = %v, want it to say the passage moved on", err)
	}

	after, _ := os.ReadFile(filepath.Join(root, "spec.md"))
	if string(after) != edited {
		t.Errorf("the refused edit was written anyway:\n%s", after)
	}
}

func TestApplyEditRefusesAVanishedPassage(t *testing.T) {
	rev, _ := newDoc(t)

	_, err := rev.ApplyEdit("spec.md", Anchor{Quote: "no such sentence"}, "no such sentence", "something")
	if err == nil || !strings.Contains(err.Error(), "selected passage") {
		t.Errorf("err = %v, want the missing passage named", err)
	}
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
	if err == nil || !strings.Contains(err.Error(), "turn is running") {
		t.Errorf("err = %v, want the running turn named", err)
	}
}

func TestHandEditSubjectNamesThePassage(t *testing.T) {
	if got := handEditSubject("  the retry\npolicy "); got != "review: hand edit of “the retry policy”" {
		t.Errorf("subject = %q", got)
	}
	long := handEditSubject(strings.Repeat("a", 80))
	if len([]rune(long)) > 70 {
		t.Errorf("subject was not shortened: %q", long)
	}
}
