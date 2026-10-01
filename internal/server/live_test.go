package server

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gilramir/ai-reviewer/internal/review"
)

// TestLiveClaude runs the same round trip against the real CLI rather than the
// stub in testdata. It costs money and needs an authenticated claude, so it is
// opt-in:
//
//	AI_REVIEWER_LIVE=1 go test ./internal/server -run TestLiveClaude -v
//
// It is worth having: the stub can only prove the daemon speaks the protocol it
// was written against, not that the protocol is still what the CLI emits.
func TestLiveClaude(t *testing.T) {
	if os.Getenv("AI_REVIEWER_LIVE") == "" {
		t.Skip("set AI_REVIEWER_LIVE=1 to run against the real claude CLI")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not on PATH")
	}

	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	const doc = `# Retry policy

The system SHALL retry indefinitely until the operation succeeds.
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "spec.md"), []byte(doc), 0o644))
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-qm", "initial")

	rev, err := review.New(review.Options{
		Root:         root,
		Branch:       "review/live",
		ClaudeBinary: "claude",
		Model:        "haiku",
		MaxBudgetUSD: 1,
	})
	require.NoError(t, err)
	defer rev.Close()

	const secret = "live-test-secret"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	waitFor(t, conn, "docList")

	quote := "The system SHALL retry indefinitely until the operation succeeds."

	// First: a question. The model should answer without touching the file.
	send(t, conn, map[string]any{
		"type": "comment", "doc": "spec.md",
		"anchor": map[string]any{"quote": quote},
		"body":   "why this?",
	})
	answer := waitFor(t, conn, "turnEnd")
	t.Logf("question turn: edited=%v commit=%v", answer["edited"], answer["commit"])

	// Then: an edit request on the same passage, in the same conversation.
	send(t, conn, map[string]any{
		"type": "comment", "doc": "spec.md",
		"anchor": map[string]any{"quote": quote},
		"body":   "reword this: SHALL is too strong, and indefinite retries are wrong. Use bounded retries with backoff.",
	})
	edit := waitFor(t, conn, "turnEnd")

	edited, _ := edit["edited"].(bool)
	require.True(t, edited, "the model did not edit the document: %v", edit)

	updated, err := os.ReadFile(filepath.Join(root, "spec.md"))
	require.NoError(t, err)
	t.Logf("document is now:\n%s", updated)

	assert.NotContains(t, string(updated), "SHALL retry indefinitely", "the original wording survived")
	message := gitRun(t, root, "log", "-1", "--format=%B")
	t.Logf("commit:\n%s", message)
	assert.Contains(t, message, "Review-Thread:", "commit lacks the thread trailer")
}
