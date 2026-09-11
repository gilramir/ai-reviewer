package review

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A review rooted at a subdirectory is the only case where the review root and
// the workspace differ, and it is the case the state file's location is chosen
// for. Everything here sets one up.
func newSubdirReview(t *testing.T) (repo string, docs string) {
	t.Helper()

	repo = t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	docs = filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "spec.md"), []byte("# Spec\n\nA passage.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, docs
}

// writeWorkspaceState puts a state file where the daemon now looks for one.
func writeWorkspaceState(t *testing.T, repo string, state persisted) {
	t.Helper()

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, ".ai-reviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readWorkspaceState(t *testing.T, repo string) persisted {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(repo, ".ai-reviewer", "state.json"))
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	var state persisted
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parsing the state file: %v", err)
	}
	return state
}

// The file belongs to the repository, so it is written beside .git and not
// inside whichever subdirectory this run happened to be pointed at.
func TestStateIsWrittenBesideGit(t *testing.T) {
	repo, docs := newSubdirReview(t)

	rev, err := New(Options{Root: docs})
	if err != nil {
		t.Fatal(err)
	}
	if err := rev.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(filepath.Join(repo, ".ai-reviewer", "state.json")); err != nil {
		t.Errorf("no state file beside .git: %v", err)
	}
	if _, err := os.Stat(filepath.Join(docs, ".ai-reviewer")); !os.IsNotExist(err) {
		t.Errorf("a state directory was left in the review root (%v)", err)
	}
}

// The point of the move: --root selects a view, not an identity. A thread filed
// on a document is the same thread whichever root the document was reached
// through, and the paths in the file are the workspace's so that both agree.
func TestAThreadIsTheSameFromEitherRoot(t *testing.T) {
	repo, docs := newSubdirReview(t)

	// Filed by a review of the whole repository: the document is docs/spec.md.
	writeWorkspaceState(t, repo, persisted{
		Sessions: map[string]string{"docs/spec.md": "session-1"},
		Threads: []*Thread{
			{ID: "t1", Doc: "docs/spec.md", Status: StatusOpen, Anchor: Anchor{Quote: "A passage."}},
		},
	})

	// Read by a review rooted at docs/, where the same document is spec.md.
	rev, err := New(Options{Root: docs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rev.Close() })

	threads := rev.ThreadsFor("spec.md")
	if len(threads) != 1 {
		t.Fatalf("threads on spec.md = %d, want 1", len(threads))
	}
	if threads[0].ID != "t1" {
		t.Errorf("thread id = %q, want t1", threads[0].ID)
	}

	rev.mu.Lock()
	session := rev.sessions["spec.md"]
	rev.mu.Unlock()
	if session != "session-1" {
		t.Errorf("session for spec.md = %q, want session-1", session)
	}
}

// Saving puts the paths back in the workspace's terms, so the next run from any
// root still finds them.
func TestSaveWritesWorkspacePaths(t *testing.T) {
	_, docs := newSubdirReview(t)
	repo := filepath.Dir(docs)

	rev, err := New(Options{Root: docs})
	if err != nil {
		t.Fatal(err)
	}
	rev.mu.Lock()
	rev.sessions["spec.md"] = "session-1"
	rev.turns["spec.md"] = 3
	rev.threads["t1"] = &Thread{ID: "t1", Doc: "spec.md", Status: StatusOpen}
	rev.order = append(rev.order, "t1")
	rev.mu.Unlock()

	if err := rev.Close(); err != nil {
		t.Fatal(err)
	}

	state := readWorkspaceState(t, repo)
	if _, ok := state.Sessions["docs/spec.md"]; !ok {
		t.Errorf("sessions are not keyed by workspace path: %v", state.Sessions)
	}
	if _, ok := state.Turns["docs/spec.md"]; !ok {
		t.Errorf("turns are not keyed by workspace path: %v", state.Turns)
	}
	if len(state.Threads) != 1 || state.Threads[0].Doc != "docs/spec.md" {
		t.Errorf("thread was not written under its workspace path: %+v", state.Threads)
	}
}

// One file for the whole workspace means a review of one subdirectory opens a
// file that may hold another review's threads. They are not this run's to
// address, and they are not this run's to throw away either.
func TestThreadsOutsideTheRootSurviveASave(t *testing.T) {
	repo, docs := newSubdirReview(t)

	writeWorkspaceState(t, repo, persisted{
		Sessions: map[string]string{"README.md": "session-elsewhere"},
		Turns:    map[string]int{"README.md": 4},
		Spend:    map[string]float64{"README.md": 0.5},
		Threads: []*Thread{
			{ID: "outside", Doc: "README.md", Status: StatusOpen, Anchor: Anchor{Quote: "elsewhere"}},
		},
	})

	rev, err := New(Options{Root: docs})
	if err != nil {
		t.Fatal(err)
	}

	// It is invisible from here, which is correct: the browser cannot open it.
	if threads := rev.ThreadsFor("README.md"); len(threads) != 0 {
		t.Errorf("a document outside the root is being served: %v", threads)
	}
	if err := rev.Close(); err != nil {
		t.Fatal(err)
	}

	state := readWorkspaceState(t, repo)
	if len(state.Threads) != 1 || state.Threads[0].ID != "outside" {
		t.Fatalf("the outside thread did not survive the save: %+v", state.Threads)
	}
	if state.Threads[0].Doc != "README.md" {
		t.Errorf("the outside thread's path was rewritten: %q", state.Threads[0].Doc)
	}
	if state.Sessions["README.md"] != "session-elsewhere" {
		t.Errorf("the outside session did not survive: %v", state.Sessions)
	}
	if state.Turns["README.md"] != 4 || state.Spend["README.md"] != 0.5 {
		t.Errorf("the outside turn count or spend did not survive: %v %v", state.Turns, state.Spend)
	}
}
