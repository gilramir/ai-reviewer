package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gilramir/ai-reviewer/internal/review"
)

// waitForPass blocks until the reviewing pass on a document is over.
func waitForPass(t *testing.T, rev *review.Review, docPath string) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !rev.PassRunning(docPath) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the review of %s never finished", docPath)
}

// waitForReply blocks until the editing turn on a thread has answered.
func waitForReply(t *testing.T, rev *review.Review, docPath, threadID string) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, thread := range rev.ThreadsFor(docPath) {
			if thread.ID == threadID && thread.Status == review.StatusOpen && len(thread.Messages) >= 3 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the reply on %s was never answered", threadID)
}

// A pass files threads the reviewer did not open, anchored to passages that are
// really in the document. The anchoring is the whole feature: a comment whose
// quote cannot be found is a remark with nowhere to put it, so the stub sends
// one of each and only one of them may survive.
func TestAReviewingPassFilesAnchoredThreads(t *testing.T) {
	rev, root := newReview(t)

	if err := rev.StartPass("spec.md", "check the claims"); err != nil {
		t.Fatalf("StartPass: %v", err)
	}
	waitForPass(t, rev, "spec.md")

	threads := rev.ThreadsFor("spec.md")
	if len(threads) != 1 {
		t.Fatalf("got %d threads, want 1: %+v", len(threads), threads)
	}

	thread := threads[0]
	if thread.Origin != review.OriginModel {
		t.Errorf("origin = %q, want %q", thread.Origin, review.OriginModel)
	}
	if thread.Brief != "check the claims" {
		t.Errorf("brief = %q, want the brief the pass was given", thread.Brief)
	}
	if thread.Status != review.StatusOpen || !thread.AwaitsReviewer() {
		t.Errorf("a freshly raised comment should be open and waiting on a person: %+v", thread)
	}
	if len(thread.Messages) != 1 || thread.Messages[0].Role != review.RoleAssistant {
		t.Fatalf("messages = %+v, want one from the model", thread.Messages)
	}

	// The gate, restated as the test that matters: the anchor must find its
	// passage in the file exactly as every later re-anchoring will.
	src, err := os.ReadFile(filepath.Join(root, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := review.Locate(string(src), thread.Anchor); !ok {
		t.Fatalf("the filed anchor cannot be found in the document: %+v", thread.Anchor)
	}

	// A bare quote is not an anchor. The daemon reads the context off wherever
	// it found the passage, so the client can choose between repeats the same
	// way the server does.
	if thread.Anchor.Prefix == "" && thread.Anchor.Suffix == "" {
		t.Errorf("anchor carries no context either side: %+v", thread.Anchor)
	}
}

// The reviewer's answer has to reach the process that can act on it, which is
// not the one that raised the comment.
func TestReplyingToAMachineThreadReachesTheEditor(t *testing.T) {
	rev, root := newReview(t)

	if err := rev.StartPass("spec.md", "check the claims"); err != nil {
		t.Fatalf("StartPass: %v", err)
	}
	waitForPass(t, rev, "spec.md")

	threads := rev.ThreadsFor("spec.md")
	if len(threads) != 1 {
		t.Fatalf("got %d threads, want 1", len(threads))
	}
	quoted := threads[0].Anchor.Quote

	if err := rev.Reply(threads[0].ID, "reword this"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	waitForReply(t, rev, "spec.md", threads[0].ID)

	// The editing conversation never saw the pass, so the reply had to state
	// the passage from scratch. That it edited proves the passage arrived.
	src, err := os.ReadFile(filepath.Join(root, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), quoted) {
		t.Errorf("the passage was not edited; document still reads %q", src)
	}

	after := rev.ThreadsFor("spec.md")
	if len(after) != 1 || after[0].Commit == "" {
		t.Errorf("the edit was not committed against the thread: %+v", after)
	}
}

// A second pass on a document already being reviewed would read the same
// sections and race the first to file comments on them.
func TestOnlyOnePassRunsPerDocument(t *testing.T) {
	rev, _ := newReview(t)

	if err := rev.StartPass("spec.md", "check the claims"); err != nil {
		t.Fatalf("StartPass: %v", err)
	}
	err := rev.StartPass("spec.md", "something else")
	waitForPass(t, rev, "spec.md")

	if err == nil {
		t.Fatal("a second pass was allowed to start")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error = %v, want it to say a review is already running", err)
	}
}

// The reviewer is told where the pass has got to, and told when it is over.
func TestAPassReportsItsProgress(t *testing.T) {
	rev, _ := newReview(t)

	frames, cancel := rev.Subscribe()
	defer cancel()

	if err := rev.StartPass("spec.md", "check the claims"); err != nil {
		t.Fatalf("StartPass: %v", err)
	}
	waitForPass(t, rev, "spec.md")

	var active, finished bool
	deadline := time.After(5 * time.Second)
	for !(active && finished) {
		select {
		case data := <-frames:
			var frame struct {
				Type    string `json:"type"`
				Doc     string `json:"doc"`
				Active  bool   `json:"active"`
				Section int    `json:"section"`
				Total   int    `json:"total"`
			}
			if err := json.Unmarshal(data, &frame); err != nil || frame.Type != "pass" {
				continue
			}
			if frame.Doc != "spec.md" {
				t.Errorf("pass frame names %q", frame.Doc)
			}
			if frame.Active {
				active = true
				if frame.Section < 1 || frame.Section > frame.Total {
					t.Errorf("section %d of %d makes no sense", frame.Section, frame.Total)
				}
			} else {
				finished = true
			}
		case <-deadline:
			t.Fatalf("pass frames: active=%v finished=%v", active, finished)
		}
	}
}

// The brief comes from a file beside the documents it governs, so it can be
// edited, diffed and committed with them.
func TestTheStandingBriefIsReadFromDisk(t *testing.T) {
	rev, root := newReview(t)

	if brief := rev.Brief(); !strings.Contains(brief, "has not seen it before") {
		t.Errorf("with no file on disk, got the wrong default: %q", brief)
	}

	dir := filepath.Join(root, ".ai-reviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte("Cut what repeats.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if brief := rev.Brief(); brief != "Cut what repeats." {
		t.Errorf("brief = %q, want what the file says", brief)
	}
	if got := rev.Settings().Brief; got != "Cut what repeats." {
		t.Errorf("settings brief = %q", got)
	}
}

// The claim the whole feature rests on: a pass cannot change the document.
func TestTheCriticHasNoEditingTools(t *testing.T) {
	rev, _ := newReview(t)

	tools := rev.Settings().CriticTools
	if len(tools) == 0 {
		t.Fatal("the critic reports no tools at all")
	}
	for _, tool := range tools {
		if tool == "Edit" || tool == "Write" {
			t.Errorf("the critic was given %s", tool)
		}
	}
}
