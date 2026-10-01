package review

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A review rooted at a subdirectory is the only case where the review root and
// the workspace differ, and it is the case the state file's location is chosen
// for. Everything here sets one up.
func newSubdirReview(t *testing.T) (repo string, docs string) {
	t.Helper()

	repo = t.TempDir()
	out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "main").CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	docs = filepath.Join(repo, "docs")
	require.NoError(t, os.MkdirAll(docs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(docs, "spec.md"), []byte("# Spec\n\nA passage.\n"), 0o644))
	return repo, docs
}

// writeWorkspaceState puts a state file where the daemon now looks for one.
func writeWorkspaceState(t *testing.T, repo string, state persisted) {
	t.Helper()

	data, err := json.Marshal(state)
	require.NoError(t, err)
	dir := filepath.Join(repo, ".ai-reviewer")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644))
}

func readWorkspaceState(t *testing.T, repo string) persisted {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(repo, ".ai-reviewer", "state.json"))
	require.NoError(t, err, "reading the state file")
	var state persisted
	require.NoError(t, json.Unmarshal(data, &state), "parsing the state file")
	return state
}

// The file belongs to the repository, so it is written beside .git and not
// inside whichever subdirectory this run happened to be pointed at.
func TestStateIsWrittenBesideGit(t *testing.T) {
	repo, docs := newSubdirReview(t)

	rev, err := New(Options{Root: docs})
	require.NoError(t, err)
	require.NoError(t, rev.Close(), "Close")

	assert.FileExists(t, filepath.Join(repo, ".ai-reviewer", "state.json"), "no state file beside .git")
	_, err = os.Stat(filepath.Join(docs, ".ai-reviewer"))
	assert.ErrorIs(t, err, fs.ErrNotExist, "a state directory was left in the review root")
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
	require.NoError(t, err)
	t.Cleanup(func() { _ = rev.Close() })

	threads := rev.ThreadsFor("spec.md")
	require.Len(t, threads, 1, "threads on spec.md")
	assert.Equal(t, "t1", threads[0].ID)

	rev.mu.Lock()
	session := rev.sessions["spec.md"]
	rev.mu.Unlock()
	assert.Equal(t, "session-1", session, "session for spec.md")
}

// Saving puts the paths back in the workspace's terms, so the next run from any
// root still finds them.
func TestSaveWritesWorkspacePaths(t *testing.T) {
	_, docs := newSubdirReview(t)
	repo := filepath.Dir(docs)

	rev, err := New(Options{Root: docs})
	require.NoError(t, err)
	rev.mu.Lock()
	rev.sessions["spec.md"] = "session-1"
	rev.turns["spec.md"] = 3
	rev.threads["t1"] = &Thread{ID: "t1", Doc: "spec.md", Status: StatusOpen}
	rev.order = append(rev.order, "t1")
	rev.mu.Unlock()

	require.NoError(t, rev.Close())

	state := readWorkspaceState(t, repo)
	assert.Contains(t, state.Sessions, "docs/spec.md", "sessions are not keyed by workspace path")
	assert.Contains(t, state.Turns, "docs/spec.md", "turns are not keyed by workspace path")
	if assert.Len(t, state.Threads, 1, "thread was not written under its workspace path") {
		assert.Equal(t, "docs/spec.md", state.Threads[0].Doc, "thread was not written under its workspace path")
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
	require.NoError(t, err)

	// It is invisible from here, which is correct: the browser cannot open it.
	assert.Empty(t, rev.ThreadsFor("README.md"), "a document outside the root is being served")
	require.NoError(t, rev.Close())

	state := readWorkspaceState(t, repo)
	require.Len(t, state.Threads, 1, "the outside thread did not survive the save")
	require.Equal(t, "outside", state.Threads[0].ID, "the outside thread did not survive the save")
	assert.Equal(t, "README.md", state.Threads[0].Doc, "the outside thread's path was rewritten")
	assert.Equal(t, "session-elsewhere", state.Sessions["README.md"], "the outside session did not survive")
	assert.Equal(t, 4, state.Turns["README.md"], "the outside turn count did not survive")
	assert.Equal(t, 0.5, state.Spend["README.md"], "the outside spend did not survive")
}
