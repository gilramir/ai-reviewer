package wirelog

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gilramir/ai-reviewer/internal/claudeproc"
)

func newLog(t *testing.T) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude.db")
	log, err := Create(path, Run{Root: "/review", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	return log, path
}

func newReader(t *testing.T, path string) *Reader {
	t.Helper()
	r, err := OpenReader(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// The two-turn trace from docs/backendClaude.md, as the CLI writes it: a Read,
// an Edit, prose, and the result frame that closes the turn.
var trace = []string{
	`{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-5","apiKeySource":"none"}`,
	`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
	`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/w/note.md"}}]},"parent_tool_use_id":null}`,
	`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"1\tLine one.\n2\t"}]}}`,
	`{"type":"assistant","message":{"id":"msg_2","content":[{"type":"tool_use","id":"toolu_2","name":"Edit","input":{"file_path":"/w/note.md","old_string":"Line one.","new_string":"Line two."}}]}}`,
	`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","is_error":true,"content":[{"type":"text","text":"File has not been read"}]}]}}`,
	`{"type":"assistant","message":{"id":"msg_3","content":[{"type":"text","text":"note.md now reads \"Line two.\""}]}}`,
	`{"type":"result","subtype":"success","is_error":false,"result":"note.md now reads \"Line two.\"","total_cost_usd":0.0412,"duration_ms":5200,"duration_api_ms":4800,"num_turns":3,"usage":{"input_tokens":12,"output_tokens":340,"cache_read_input_tokens":18200,"cache_creation_input_tokens":900}}`,
}

func TestATurnIsATreeOfFrames(t *testing.T) {
	log, path := newLog(t)

	p := log.Spawn(claudeproc.Spawn{Role: "reviewer", SessionID: "s1", Doc: "note.md", Argv: []string{"claude", "-p"}, Dir: "/w", PID: 42})
	p.In(claudeproc.TurnLabel{Doc: "note.md", Purpose: "thread t1"},
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"change one to two"}]}}`))
	for _, line := range trace {
		p.Out([]byte(line))
	}
	p.Stderr([]byte("warning: something after the turn\n"))
	p.Note("stop", "idle timeout")
	p.Note("stop", "shutdown")
	p.Exited(errors.New("signal: killed"))

	r := newReader(t, path)
	rows, err := r.Rows(0, 100)
	require.NoError(t, err)

	var dirs, types []string
	for _, row := range rows {
		dirs = append(dirs, row.Dir)
		types = append(types, row.Type)
	}
	assert.Equal(t, []string{"daemon", "in", "out", "out", "out", "out", "out", "out", "out", "out", "stderr", "daemon", "daemon", "daemon"}, dirs)
	assert.Equal(t, []string{"spawn", "user", "system", "rate_limit_event", "assistant", "user", "assistant", "user", "assistant", "result", "", "stop", "stop", "exit"}, types)

	turn := rows[1].Turn
	require.NotNil(t, turn)
	for i := 1; i <= 9; i++ {
		require.NotNil(t, rows[i].Turn, "frame %d belongs to the turn", i)
		assert.Equal(t, *turn, *rows[i].Turn)
	}
	assert.Nil(t, rows[0].Turn, "the spawn precedes every turn")
	assert.Nil(t, rows[10].Turn, "stderr after the result is the process's, not the turn's")

	assert.Equal(t, "prompt: change one to two", rows[1].Summary)
	assert.Equal(t, "init claude-sonnet-5", rows[2].Summary)
	assert.Equal(t, "Read /w/note.md", rows[4].Summary)
	assert.Equal(t, "Read result: 1\tLine one. …", rows[5].Summary)
	assert.Equal(t, "Edit error: File has not been read", rows[7].Summary)
	assert.Equal(t, `“note.md now reads "Line two."”`, rows[8].Summary)
	assert.Equal(t, "note.md", rows[5].Doc)

	// A tool_result's parent is the frame holding its tool_use.
	require.NotNil(t, rows[5].ReplyTo)
	assert.Equal(t, rows[4].ID, *rows[5].ReplyTo)
	require.NotNil(t, rows[7].ReplyTo)
	assert.Equal(t, rows[6].ID, *rows[7].ReplyTo)

	require.NotNil(t, rows[9].Cost)
	assert.InDelta(t, 0.0412, *rows[9].Cost, 1e-9)

	d, err := r.Detail(rows[7].ID)
	require.NoError(t, err)
	require.Len(t, d.Tools, 1)
	assert.Equal(t, "Edit", d.Tools[0].Name)
	assert.Equal(t, rows[6].ID, d.Tools[0].UseFrame)
	require.NotNil(t, d.Tools[0].IsError)
	assert.True(t, *d.Tools[0].IsError)

	require.NotNil(t, d.Turn)
	assert.Equal(t, "thread t1", d.Turn.Purpose)
	assert.Equal(t, "claude-sonnet-5", d.Turn.Model)
	assert.Equal(t, "success", d.Turn.Subtype)
	assert.InDelta(t, 0.0412, *d.Turn.CostUSD, 1e-9)
	assert.Equal(t, int64(18200), *d.Turn.CacheReadTokens)
	assert.Equal(t, int64(900), *d.Turn.CacheCreationTokens)
	assert.Equal(t, int64(340), *d.Turn.OutputTokens)
	assert.Equal(t, int64(4800), *d.Turn.DurationAPIMS)
	assert.Equal(t, int64(3), *d.Turn.NumTurns)

	assert.Equal(t, "reviewer", d.Proc.Role)
	assert.Equal(t, int64(42), d.Proc.PID)
	assert.JSONEq(t, `["claude","-p"]`, string(d.Proc.Argv))
	assert.Equal(t, "idle timeout", d.Proc.EndReason, "the first reason to stop is the one that counts")
	assert.Equal(t, "signal: killed", d.Proc.ExitStatus)

	// The frame is kept as the CLI wrote it, key order and all.
	d, err = r.Detail(rows[2].ID)
	require.NoError(t, err)
	assert.Equal(t, trace[0], string(d.Raw))
}

// Two processes that both call their first tool toolu_1 -- as a stub CLI will
// -- each link their own result to their own use.
func TestToolCallsAreMatchedWithinTheirProcess(t *testing.T) {
	log, path := newLog(t)
	a := log.Spawn(claudeproc.Spawn{Role: "reviewer"})
	b := log.Spawn(claudeproc.Spawn{Role: "reviewer"})
	a.Out([]byte(trace[2]))
	b.Out([]byte(trace[2]))
	a.Out([]byte(trace[3]))
	b.Out([]byte(trace[3]))

	r := newReader(t, path)
	rows, err := r.Rows(0, 100)
	require.NoError(t, err)
	var outs []Row
	for _, row := range rows {
		if row.Dir == DirOut {
			outs = append(outs, row)
		}
	}
	require.Len(t, outs, 4)
	require.NotNil(t, outs[2].ReplyTo)
	require.NotNil(t, outs[3].ReplyTo)
	assert.Equal(t, outs[0].ID, *outs[2].ReplyTo)
	assert.Equal(t, outs[1].ID, *outs[3].ReplyTo)

	d, err := r.Detail(outs[2].ID)
	require.NoError(t, err)
	require.Len(t, d.Tools, 1)
	assert.Equal(t, outs[0].ID, d.Tools[0].UseFrame)
}

func TestRowsPageFromTheLastIDSeen(t *testing.T) {
	log, path := newLog(t)
	p := log.Spawn(claudeproc.Spawn{Role: "reviewer"})
	for _, line := range trace {
		p.Out([]byte(line))
	}

	r := newReader(t, path)
	first, err := r.Rows(0, 3)
	require.NoError(t, err)
	require.Len(t, first, 3)

	rest, err := r.Rows(first[2].ID, 100)
	require.NoError(t, err)
	assert.Len(t, rest, 1+len(trace)-3)
	assert.Greater(t, rest[0].ID, first[2].ID)
}

func TestDetailOfAMissingFrame(t *testing.T) {
	_, path := newLog(t)
	_, err := newReader(t, path).Detail(999)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestWritesAfterCloseAreDropped(t *testing.T) {
	log, _ := newLog(t)
	p := log.Spawn(claudeproc.Spawn{Role: "reviewer"})
	require.NoError(t, log.Close())

	// A process reaped during shutdown reports its exit after this.
	p.Exited(nil)
	assert.False(t, log.failed)
}

// The whole path: a real Session, the stub CLI the server tests use, and a
// reader open on the file while the writer still has it.
func TestASessionIsLoggedThroughItsTap(t *testing.T) {
	log, path := newLog(t)

	stub, err := filepath.Abs("../server/testdata/fake-claude")
	require.NoError(t, err)

	s := claudeproc.New("11111111-1111-1111-1111-111111111111", claudeproc.Config{
		Binary:  stub,
		WorkDir: t.TempDir(),
		Role:    "critic",
		Tap:     log,
	})
	ctx := claudeproc.WithTurnLabel(context.Background(), claudeproc.TurnLabel{Doc: "spec.md", Purpose: "thread t9"})
	result, err := s.Ask(ctx, "why this?", nil)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	r := newReader(t, path)

	// The exit is reported by the goroutine reaping the child, in its own time.
	var rows []Row
	require.Eventually(t, func() bool {
		rows, err = r.Rows(0, 100)
		return err == nil && len(rows) > 0 && rows[len(rows)-1].Type == "exit"
	}, 5*time.Second, 20*time.Millisecond)

	var prompt Row
	for _, row := range rows {
		if row.Dir == DirIn {
			prompt = row
		}
	}
	assert.Equal(t, "prompt: why this?", prompt.Summary)
	assert.Equal(t, "spec.md", prompt.Doc)
	assert.Equal(t, "critic", prompt.Role)

	d, err := r.Detail(rows[len(rows)-1].ID)
	require.NoError(t, err)
	assert.Equal(t, "closed", d.Proc.EndReason)
	assert.Equal(t, "spec.md", d.Proc.Doc)

	var argv []string
	require.NoError(t, json.Unmarshal(d.Proc.Argv, &argv))
	assert.Equal(t, stub, argv[0])
	assert.Contains(t, argv, "--session-id")

	d, err = r.Detail(prompt.ID)
	require.NoError(t, err)
	require.NotNil(t, d.Turn)
	assert.InDelta(t, result.CostUSD, *d.Turn.CostUSD, 1e-9)
}
