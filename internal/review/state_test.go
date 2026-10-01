package review

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newReview builds a Review over an empty directory. No git repository is
// needed: gitstore falls back to snapshots, and none of these tests record a
// turn.
func newReview(t *testing.T, root string) *Review {
	t.Helper()
	rev, err := New(Options{Root: root})
	require.NoError(t, err, "New")
	t.Cleanup(func() { _ = rev.Close() })
	return rev
}

func writeState(t *testing.T, root string, content string) string {
	t.Helper()
	dir := filepath.Join(root, ".ai-reviewer")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "state.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// TestCorruptStateIsKeptAndReported is the point of the whole exercise: a state
// file that cannot be parsed holds a review's threads, and losing it quietly is
// worse than any parse error. It must survive on disk and be reported.
func TestCorruptStateIsKeptAndReported(t *testing.T) {
	root := t.TempDir()
	const truncated = `{"threads":[{"id":"abc","doc":"spec.md","messages":[{"role":"user","tex`
	path := writeState(t, root, truncated)

	rev := newReview(t, root)

	notices := rev.Notices()
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "state.json", "notice does not name the file")

	matches, err := filepath.Glob(path + ".corrupt-*")
	require.NoError(t, err)
	require.Len(t, matches, 1, "want one quarantined file")
	kept, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.Equal(t, truncated, string(kept), "quarantined file was altered")

	// The review is usable, and saving over it must not touch what was kept.
	require.NoError(t, rev.save(), "save")
	again, err := os.ReadFile(matches[0])
	if assert.NoError(t, err, "quarantined file did not survive a save") {
		assert.Equal(t, truncated, string(again), "quarantined file did not survive a save")
	}
}

// Two bad starts in a row must not have the second overwrite what the first
// rescued.
func TestASecondCorruptStateDoesNotOverwriteTheFirst(t *testing.T) {
	root := t.TempDir()
	path := writeState(t, root, `{"threads":[ oops`)
	_ = newReview(t, root)

	writeState(t, root, `{"threads":[ also oops`)
	_ = newReview(t, root)

	matches, _ := filepath.Glob(path + ".corrupt-*")
	require.Len(t, matches, 2, "want two quarantined files")
}

// When the file cannot even be moved aside, starting would overwrite it at the
// first save. Refusing to start is the only thing that keeps the threads.
func TestUnmovableCorruptStateRefusesToStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can rename inside a read-only directory")
	}

	root := t.TempDir()
	// A git root, so gitstore does not try to create its snapshot directory
	// inside the one this test is about to seal.
	out, err := exec.Command("git", "-C", root, "init", "-q", "-b", "main").CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	writeState(t, root, `{"threads":[ oops`)
	dir := filepath.Join(root, ".ai-reviewer")
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	rev, err := New(Options{Root: root})
	if err == nil {
		_ = rev.Close()
	}
	require.Error(t, err, "New succeeded despite an unreadable, unmovable state file")
	assert.Contains(t, err.Error(), "state.json", "error does not name the file")
}

func TestGoodStateLoadsWithoutNotices(t *testing.T) {
	root := t.TempDir()
	state := persisted{
		Sessions: map[string]string{"spec.md": "session-1"},
		Threads: []*Thread{
			{ID: "t1", Doc: "spec.md", Status: StatusResolved, Anchor: Anchor{Quote: "a passage"}},
			// A turn cannot survive a restart, so this one must come back open.
			{ID: "t2", Doc: "spec.md", Status: StatusThinking, Anchor: Anchor{Quote: "another"}},
		},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	writeState(t, root, string(data))

	rev := newReview(t, root)
	assert.Empty(t, rev.Notices(), "unexpected notices")

	threads := rev.ThreadsFor("spec.md")
	require.Len(t, threads, 2)
	assert.Equal(t, StatusResolved, threads[0].Status, "resolved thread")
	assert.Equal(t, StatusOpen, threads[1].Status, "in-flight thread")
}

// A round trip is what proves the format the quarantine protects is the one
// actually written.
func TestSaveAndLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	rev := newReview(t, root)

	rev.mu.Lock()
	rev.threads["t1"] = &Thread{
		ID:       "t1",
		Doc:      "spec.md",
		Status:   StatusResolved,
		Anchor:   Anchor{Quote: "why is there a link here", NodeID: "n-30"},
		Messages: []Message{{Role: RoleUser, Text: "why is there a link here"}},
	}
	rev.order = append(rev.order, "t1")
	rev.sessions["spec.md"] = "session-1"
	rev.mu.Unlock()

	require.NoError(t, rev.save())

	reloaded := newReview(t, root)
	threads := reloaded.ThreadsFor("spec.md")
	require.Len(t, threads, 1, "threads did not survive a restart")
	require.Equal(t, "t1", threads[0].ID, "threads did not survive a restart")
	assert.Equal(t, StatusResolved, threads[0].Status)
	assert.Equal(t, "why is there a link here", threads[0].Anchor.Quote)
}

// A model chosen in the browser is a decision about this review, not about this
// process, so it has to outlive the daemon that took it.
func TestTheChosenModelSurvivesARestart(t *testing.T) {
	root := t.TempDir()

	first := newReview(t, root)
	require.NoError(t, first.SetModel("sonnet"))
	require.Equal(t, "sonnet", first.Settings().Model, "model after choosing sonnet")
	// Written when the choice is made, not at the end of some later turn.
	assert.Contains(t, readState(t, root), `"model": "sonnet"`, "the choice reached state.json only after a further save")

	restarted := newReview(t, root)
	assert.Equal(t, "sonnet", restarted.Settings().Model, "model after a restart")
}

// --model on the command line is the more recent decision and outranks the
// stored one; the stored value then follows it, rather than lying in wait.
func TestAnExplicitModelFlagOutranksTheStoredOne(t *testing.T) {
	root := t.TempDir()

	first := newReview(t, root)
	require.NoError(t, first.SetModel("sonnet"))

	flagged, err := New(Options{Root: root, Model: "opus"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = flagged.Close() })

	assert.Equal(t, "opus", flagged.Settings().Model, "want the flag's model")
	require.NoError(t, flagged.save())
	assert.Contains(t, readState(t, root), `"model": "opus"`, "the stored model did not follow the flag")
}

// Choosing the default means asking for no model at all, which is a real
// choice and must not read back as "sonnet, still".
func TestChoosingTheDefaultClearsTheStoredModel(t *testing.T) {
	root := t.TempDir()

	first := newReview(t, root)
	require.NoError(t, first.SetModel("haiku"))
	require.NoError(t, first.SetModel(""))

	assert.NotContains(t, readState(t, root), `"model"`, "state.json still names a model")
	assert.Empty(t, newReview(t, root).Settings().Model, "model after choosing the default")
}

func readState(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".ai-reviewer", "state.json"))
	require.NoError(t, err)
	return string(data)
}
