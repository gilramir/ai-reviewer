package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gilramir/ai-reviewer/internal/review"
)

// askToRead files a comment asking the stub to read path, and returns the
// reply as the browser receives it in the threads frame.
func askToRead(t *testing.T, rev *review.Review, path string) map[string]any {
	t.Helper()

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	t.Cleanup(ts.Close)

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	waitFor(t, conn, "docList")

	send(t, conn, map[string]any{
		"type": "comment",
		"doc":  "spec.md",
		"anchor": map[string]any{
			"quote": "The system SHALL retry indefinitely until the operation succeeds.",
		},
		"body": "please read " + path,
	})

	// The threads frame that follows turnEnd is the one with the reply in it.
	waitFor(t, conn, "turnEnd")
	frame := waitFor(t, conn, "threads")
	threads, _ := frame["threads"].([]any)
	require.Len(t, threads, 1)
	messages, _ := threads[0].(map[string]any)["messages"].([]any)
	require.Len(t, messages, 2, "messages = %v", messages)
	reply, _ := messages[1].(map[string]any)
	return reply
}

// A file outside the repository is refused, and the refusal reaches the
// browser beside the reply that mentions it, marked as the kind --add-dir
// fixes. Without this the reviewer sees only "permission was denied" and has
// no way to tell which file, or what to do about it.
func TestARefusalReachesTheBrowser(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "notes.md")
	require.NoError(t, os.WriteFile(outside, []byte("from elsewhere"), 0o644))

	rev, _ := newReview(t)
	reply := askToRead(t, rev, outside)

	denied, _ := reply["denied"].([]any)
	require.Len(t, denied, 1, "reply = %v", reply)
	d, _ := denied[0].(map[string]any)
	assert.Equal(t, "Read", d["tool"])
	assert.Equal(t, outside, d["target"])
	assert.Equal(t, true, d["outside"], "a path beyond the workspace was not marked outside")
}

// --add-dir reaches the CLI, and with it the same read succeeds and nothing is
// reported refused.
func TestAnAddedDirectoryCanBeRead(t *testing.T) {
	extra := t.TempDir()
	outside := filepath.Join(extra, "notes.md")
	require.NoError(t, os.WriteFile(outside, []byte("from elsewhere"), 0o644))

	rev, _ := newReviewWith(t, func(o *review.Options) { o.AddDirs = []string{extra} })
	reply := askToRead(t, rev, outside)

	assert.Equal(t, "It says: from elsewhere", reply["text"])
	assert.Nil(t, reply["denied"], "a successful read reported a refusal")
	assert.Equal(t, []string{extra}, rev.Settings().AddDirs)
}
