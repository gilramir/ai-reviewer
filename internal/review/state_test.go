package review

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newReview builds a Review over an empty directory. No git repository is
// needed: gitstore falls back to snapshots, and none of these tests record a
// turn.
func newReview(t *testing.T, root string) *Review {
	t.Helper()
	rev, err := New(Options{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rev.Close() })
	return rev
}

func writeState(t *testing.T, root string, content string) string {
	t.Helper()
	dir := filepath.Join(root, ".ai-reviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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

	if notices := rev.Notices(); len(notices) != 1 {
		t.Fatalf("notices = %v, want one", notices)
	} else if !strings.Contains(notices[0], "state.json") {
		t.Errorf("notice does not name the file: %q", notices[0])
	}

	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one quarantined file, got %v (%v)", matches, err)
	}
	kept, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != truncated {
		t.Errorf("quarantined file was altered:\n%s", kept)
	}

	// The review is usable, and saving over it must not touch what was kept.
	if err := rev.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	again, err := os.ReadFile(matches[0])
	if err != nil || string(again) != truncated {
		t.Errorf("quarantined file did not survive a save: %v", err)
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
	if len(matches) != 2 {
		t.Fatalf("want two quarantined files, got %v", matches)
	}
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
	if out, err := exec.Command("git", "-C", root, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	writeState(t, root, `{"threads":[ oops`)
	dir := filepath.Join(root, ".ai-reviewer")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	rev, err := New(Options{Root: root})
	if err == nil {
		_ = rev.Close()
		t.Fatal("New succeeded despite an unreadable, unmovable state file")
	}
	if !strings.Contains(err.Error(), "state.json") {
		t.Errorf("error does not name the file: %v", err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	writeState(t, root, string(data))

	rev := newReview(t, root)
	if notices := rev.Notices(); len(notices) != 0 {
		t.Errorf("unexpected notices: %v", notices)
	}

	threads := rev.ThreadsFor("spec.md")
	if len(threads) != 2 {
		t.Fatalf("want 2 threads, got %d", len(threads))
	}
	if threads[0].Status != StatusResolved {
		t.Errorf("resolved thread came back %q", threads[0].Status)
	}
	if threads[1].Status != StatusOpen {
		t.Errorf("in-flight thread came back %q, want open", threads[1].Status)
	}
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

	if err := rev.save(); err != nil {
		t.Fatal(err)
	}

	reloaded := newReview(t, root)
	threads := reloaded.ThreadsFor("spec.md")
	if len(threads) != 1 || threads[0].ID != "t1" {
		t.Fatalf("threads did not survive a restart: %v", threads)
	}
	if threads[0].Status != StatusResolved {
		t.Errorf("status = %q, want resolved", threads[0].Status)
	}
	if threads[0].Anchor.Quote != "why is there a link here" {
		t.Errorf("anchor = %+v", threads[0].Anchor)
	}
}

// A model chosen in the browser is a decision about this review, not about this
// process, so it has to outlive the daemon that took it.
func TestTheChosenModelSurvivesARestart(t *testing.T) {
	root := t.TempDir()

	first := newReview(t, root)
	if err := first.SetModel("sonnet"); err != nil {
		t.Fatal(err)
	}
	if got := first.Settings().Model; got != "sonnet" {
		t.Fatalf("model = %q after choosing sonnet", got)
	}
	// Written when the choice is made, not at the end of some later turn.
	if !strings.Contains(readState(t, root), `"model": "sonnet"`) {
		t.Error("the choice reached state.json only after a further save")
	}

	restarted := newReview(t, root)
	if got := restarted.Settings().Model; got != "sonnet" {
		t.Errorf("model = %q after a restart, want sonnet", got)
	}
}

// --model on the command line is the more recent decision and outranks the
// stored one; the stored value then follows it, rather than lying in wait.
func TestAnExplicitModelFlagOutranksTheStoredOne(t *testing.T) {
	root := t.TempDir()

	first := newReview(t, root)
	if err := first.SetModel("sonnet"); err != nil {
		t.Fatal(err)
	}

	flagged, err := New(Options{Root: root, Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = flagged.Close() })

	if got := flagged.Settings().Model; got != "opus" {
		t.Errorf("model = %q, want the flag's opus", got)
	}
	if err := flagged.save(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readState(t, root), `"model": "opus"`) {
		t.Error("the stored model did not follow the flag")
	}
}

// Choosing the default means asking for no model at all, which is a real
// choice and must not read back as "sonnet, still".
func TestChoosingTheDefaultClearsTheStoredModel(t *testing.T) {
	root := t.TempDir()

	first := newReview(t, root)
	if err := first.SetModel("haiku"); err != nil {
		t.Fatal(err)
	}
	if err := first.SetModel(""); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(readState(t, root), `"model"`) {
		t.Errorf("state.json still names a model:\n%s", readState(t, root))
	}
	if got := newReview(t, root).Settings().Model; got != "" {
		t.Errorf("model = %q after choosing the default, want empty", got)
	}
}

func readState(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".ai-reviewer", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
