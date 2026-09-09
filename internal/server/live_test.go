package server

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	const doc = `# Retry policy

The system SHALL retry indefinitely until the operation succeeds.
`
	if err := os.WriteFile(filepath.Join(root, "spec.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-qm", "initial")

	rev, err := review.New(review.Options{
		Root:         root,
		Branch:       "review/live",
		ClaudeBinary: "claude",
		Model:        "haiku",
		MaxBudgetUSD: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
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

	if edited, _ := edit["edited"].(bool); !edited {
		t.Fatalf("the model did not edit the document: %v", edit)
	}

	updated, err := os.ReadFile(filepath.Join(root, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("document is now:\n%s", updated)

	if strings.Contains(string(updated), "SHALL retry indefinitely") {
		t.Errorf("the original wording survived:\n%s", updated)
	}
	message := gitRun(t, root, "log", "-1", "--format=%B")
	t.Logf("commit:\n%s", message)
	if !strings.Contains(message, "Review-Thread:") {
		t.Errorf("commit lacks the thread trailer:\n%s", message)
	}
}
