package claudeproc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// launches sets up a Session factory over the stub CLI in testdata, and returns
// a way to read back how each launch was invoked.
func launches(t *testing.T) (func(id string) *Session, func() []string) {
	t.Helper()

	dir := t.TempDir()
	bin, err := filepath.Abs("testdata/stub-claude")
	if err != nil {
		t.Fatal(err)
	}

	argvLog := filepath.Join(dir, "argv.log")
	usedIDs := filepath.Join(dir, "used-ids")
	env := append(os.Environ(), "ARGV_LOG="+argvLog, "USED_IDS="+usedIDs)

	newSession := func(id string) *Session {
		s := New(id, Config{Binary: bin, WorkDir: dir, Env: env})
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	modes := func() []string {
		data, err := os.ReadFile(argvLog)
		if err != nil {
			return nil
		}
		var out []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			switch {
			case strings.Contains(line, "--resume"):
				out = append(out, "--resume")
			case strings.Contains(line, "--session-id"):
				out = append(out, "--session-id")
			default:
				out = append(out, "?")
			}
		}
		return out
	}

	return newSession, modes
}

func ask(t *testing.T, s *Session, prompt string) TurnResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := s.Ask(ctx, prompt, nil)
	if err != nil {
		t.Fatalf("Ask(%q): %v", prompt, err)
	}
	return result
}

const testID = "11111111-2222-4333-8444-555555555555"

// The first launch pins the session id and every later launch resumes it. Get
// this wrong in either direction and the CLI refuses to start.
func TestFirstLaunchPinsThenResumes(t *testing.T) {
	newSession, modes := launches(t)

	s := newSession(testID)
	ask(t, s, "one")
	_ = s.Close() // as the idle reaper does
	ask(t, s, "two")

	if got := modes(); len(got) != 2 || got[0] != "--session-id" || got[1] != "--resume" {
		t.Errorf("launch modes = %v, want [--session-id --resume]", got)
	}
}

// A restarted daemon has the session id -- it is persisted -- but not the
// knowledge that the CLI has already seen it, so its first launch is refused.
// The refusal is the missing bit: the turn must recover, not fail.
func TestARestartedDaemonResumesRatherThanFailing(t *testing.T) {
	newSession, modes := launches(t)

	first := newSession(testID)
	ask(t, first, "before the restart")
	_ = first.Close()

	// A brand-new Session over the same persisted id, as review.New builds after
	// reading state.json.
	restarted := newSession(testID)
	result := ask(t, restarted, "after the restart")
	if result.Text != "ok" {
		t.Errorf("turn text = %q", result.Text)
	}

	got := modes()
	want := []string{"--session-id", "--session-id", "--resume"}
	if len(got) != len(want) {
		t.Fatalf("launch modes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("launch modes = %v, want %v", got, want)
		}
	}

	// And the recovery is once per Session, not once per turn.
	ask(t, restarted, "another turn")
	if n := len(modes()); n != 3 {
		t.Errorf("%d launches after a second turn, want 3", n)
	}
}

// An id the CLI has never seen must not be resumed. Nothing sets everStarted
// without an init frame, so this is the shape the recovery must not break.
func TestAnUnknownIDIsPinnedNotResumed(t *testing.T) {
	newSession, modes := launches(t)

	ask(t, newSession(testID), "first ever turn")

	if got := modes(); len(got) != 1 || got[0] != "--session-id" {
		t.Errorf("launch modes = %v, want [--session-id]", got)
	}
}

// A failure that is not the session-id refusal must surface, not spin.
func TestAnUnrelatedFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	s := New(testID, Config{Binary: filepath.Join(dir, "does-not-exist"), WorkDir: dir})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Ask(ctx, "hello", nil); err == nil {
		t.Fatal("Ask succeeded with no CLI on disk")
	}
}

// The two refusals do not have the same shape, and only one of them is a dead
// process. Resuming an id the CLI has lost comes back as a completed turn that
// says it failed -- so a caller that only checks the error misses it.
func TestResumingAnUnknownIDIsAFailedTurnNotADeadProcess(t *testing.T) {
	dir := t.TempDir()
	bin, err := filepath.Abs("testdata/stub-claude")
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"ARGV_LOG="+filepath.Join(dir, "argv.log"),
		"USED_IDS="+filepath.Join(dir, "used-ids"))

	s := New(testID, Config{Binary: bin, WorkDir: dir, Env: env})
	t.Cleanup(func() { _ = s.Close() })

	// Force the resume path for an id no CLI has ever seen.
	s.everStarted = true

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := s.Ask(ctx, "hello", nil)
	if err != nil {
		t.Fatalf("Ask returned an error rather than a failed turn: %v", err)
	}
	if !result.IsError {
		t.Error("turn did not report is_error")
	}
}

// The process must start in the directory the caller asked for, because that
// directory is the CLI's file-permission boundary: it can read what is under
// its working directory and is refused everything above it.
func TestTheProcessRunsInTheConfiguredDirectory(t *testing.T) {
	dir := t.TempDir()
	workDir := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin, err := filepath.Abs("testdata/stub-claude")
	if err != nil {
		t.Fatal(err)
	}

	cwdLog := filepath.Join(dir, "cwd.log")
	env := append(os.Environ(),
		"ARGV_LOG="+filepath.Join(dir, "argv.log"),
		"USED_IDS="+filepath.Join(dir, "used-ids"),
		"CWD_LOG="+cwdLog)

	s := New(testID, Config{Binary: bin, WorkDir: workDir, Env: env})
	t.Cleanup(func() { _ = s.Close() })
	ask(t, s, "hello")

	got, err := os.ReadFile(cwdLog)
	if err != nil {
		t.Fatal(err)
	}
	// macOS hands out /var symlinks to /private/var, so compare resolved paths.
	want, _ := filepath.EvalSymlinks(workDir)
	have, _ := filepath.EvalSymlinks(strings.TrimSpace(string(got)))
	if have != want {
		t.Errorf("process ran in %q, want %q", have, want)
	}
}
