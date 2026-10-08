package claudeproc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// launches sets up a Session factory over the stub CLI in testdata, and returns
// a way to read back how each launch was invoked.
func launches(t *testing.T) (func(id string) *Session, func() []string) {
	t.Helper()

	dir := t.TempDir()
	bin, err := filepath.Abs("testdata/stub-claude")
	require.NoError(t, err)

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
	require.NoError(t, err, "Ask(%q)", prompt)
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

	assert.Equal(t, []string{"--session-id", "--resume"}, modes(), "launch modes")
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
	assert.Equal(t, "ok", result.Text, "turn text")

	require.Equal(t, []string{"--session-id", "--session-id", "--resume"}, modes(), "launch modes")

	// And the recovery is once per Session, not once per turn.
	ask(t, restarted, "another turn")
	assert.Len(t, modes(), 3, "launches after a second turn")
}

// An id the CLI has never seen must not be resumed. Nothing sets everStarted
// without an init frame, so this is the shape the recovery must not break.
func TestAnUnknownIDIsPinnedNotResumed(t *testing.T) {
	newSession, modes := launches(t)

	ask(t, newSession(testID), "first ever turn")

	assert.Equal(t, []string{"--session-id"}, modes(), "launch modes")
}

// A failure that is not the session-id refusal must surface, not spin.
func TestAnUnrelatedFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	s := New(testID, Config{Binary: filepath.Join(dir, "does-not-exist"), WorkDir: dir})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.Ask(ctx, "hello", nil)
	require.Error(t, err, "Ask succeeded with no CLI on disk")
}

// The two refusals do not have the same shape, and only one of them is a dead
// process. Resuming an id the CLI has lost comes back as a completed turn that
// says it failed -- so a caller that only checks the error misses it.
func TestResumingAnUnknownIDIsAFailedTurnNotADeadProcess(t *testing.T) {
	dir := t.TempDir()
	bin, err := filepath.Abs("testdata/stub-claude")
	require.NoError(t, err)
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
	require.NoError(t, err, "Ask returned an error rather than a failed turn")
	assert.True(t, result.IsError, "turn did not report is_error")
}

// The process must start in the directory the caller asked for, because that
// directory is the CLI's file-permission boundary: it can read what is under
// its working directory and is refused everything above it.
func TestTheProcessRunsInTheConfiguredDirectory(t *testing.T) {
	dir := t.TempDir()
	workDir := filepath.Join(dir, "workspace")
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	bin, err := filepath.Abs("testdata/stub-claude")
	require.NoError(t, err)

	cwdLog := filepath.Join(dir, "cwd.log")
	env := append(os.Environ(),
		"ARGV_LOG="+filepath.Join(dir, "argv.log"),
		"USED_IDS="+filepath.Join(dir, "used-ids"),
		"CWD_LOG="+cwdLog)

	s := New(testID, Config{Binary: bin, WorkDir: workDir, Env: env})
	t.Cleanup(func() { _ = s.Close() })
	ask(t, s, "hello")

	got, err := os.ReadFile(cwdLog)
	require.NoError(t, err)
	// macOS hands out /var symlinks to /private/var, so compare resolved paths.
	want, _ := filepath.EvalSymlinks(workDir)
	have, _ := filepath.EvalSymlinks(strings.TrimSpace(string(got)))
	assert.Equal(t, want, have, "the directory the process ran in")
}

// A model chosen mid-review reaches the next launch, and the conversation is
// resumed rather than restarted: the CLI is happy to continue a session on a
// different model, so switching costs nothing said so far.
func TestChangingTheModelRelaunchesAndResumes(t *testing.T) {
	dir := t.TempDir()
	bin, err := filepath.Abs("testdata/stub-claude")
	require.NoError(t, err)
	argvLog := filepath.Join(dir, "argv.log")
	env := append(os.Environ(),
		"ARGV_LOG="+argvLog,
		"USED_IDS="+filepath.Join(dir, "used-ids"))

	s := New(testID, Config{Binary: bin, WorkDir: dir, Env: env})
	t.Cleanup(func() { _ = s.Close() })

	ask(t, s, "before")
	require.True(t, s.Running(), "no process to relaunch")

	s.SetModel("sonnet")

	// The running process is left alone until the next turn: a setting change
	// must not interrupt a question already in flight.
	assert.True(t, s.Running(), "the process was killed by a setting change")

	ask(t, s, "after")

	data, err := os.ReadFile(argvLog)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 2, "launches:\n%s", data)
	assert.NotContains(t, lines[0], "--model", "first launch already had a model")
	assert.Contains(t, lines[1], "--model sonnet", "second launch did not carry the new model")
	assert.Contains(t, lines[1], "--resume", "second launch restarted the conversation instead of resuming")
}

// Each extra directory gets a switch of its own. --add-dir is variadic, so two
// directories behind one switch would also work -- until a later flag's value
// was taken for a third directory.
func TestEachExtraDirectoryGetsItsOwnSwitch(t *testing.T) {
	s := New(testID, Config{AddDirs: []string{"/srv/shared", "/opt/specs"}})
	args := strings.Join(s.args(), " ")
	assert.Contains(t, args, "--add-dir /srv/shared --add-dir /opt/specs")
}

// The refusals come off the result frame, one per tool and target however
// many times the model retried, and named by whatever field that tool aims
// with.
func TestDenialsAreReadFromTheResult(t *testing.T) {
	s := New(testID, Config{})
	frame := streamFrame{Type: "result"}
	require.NoError(t, json.Unmarshal([]byte(`{"type":"result","permission_denials":[
		{"tool_name":"Read","tool_use_id":"a","tool_input":{"file_path":"/elsewhere/a.md"}},
		{"tool_name":"Read","tool_use_id":"b","tool_input":{"file_path":"/elsewhere/a.md"}},
		{"tool_name":"Grep","tool_use_id":"c","tool_input":{"pattern":"retry","path":"/elsewhere"}},
		{"tool_name":"Glob","tool_use_id":"d","tool_input":{"pattern":"**/*.md"}}
	]}`), &frame))

	var result TurnResult
	done := s.applyFrame(frame, &result, map[string]bool{}, nil)
	require.True(t, done, "a result frame did not end the turn")
	assert.Equal(t, []Denial{
		{Tool: "Read", Target: "/elsewhere/a.md"},
		{Tool: "Grep", Target: "/elsewhere"},
		{Tool: "Glob", Target: "**/*.md"},
	}, result.Denials)
}
