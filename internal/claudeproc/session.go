// Package claudeproc drives a long-lived Claude Code process over pipes.
//
// Claude Code's --print mode is not one-shot: with --input-format stream-json
// and --output-format stream-json it reads newline-delimited user messages from
// stdin for as long as stdin stays open, and emits newline-delimited events on
// stdout. One process therefore holds a whole review conversation about one
// document, which is what makes follow-up comments cheap — the document stays
// in context and is served from the prompt cache.
//
// The process is deliberately given no Bash tool. The daemon is the only writer
// of git history, and a model that can run commands could rewrite it.
package claudeproc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Config describes how to launch the CLI. Zero values are filled in by
// applyDefaults.
type Config struct {
	// Binary is the claude executable; defaults to "claude" on PATH.
	Binary string
	// WorkDir is the process's working directory, normally the review root.
	WorkDir string
	// Model is an alias ("opus", "sonnet") or a full model name. Empty uses
	// whatever the user's configuration selects.
	Model string
	// SystemPrompt is appended to the default system prompt.
	SystemPrompt string
	// AllowedTools restricts the built-in tool set. Bash is deliberately absent
	// from the default.
	AllowedTools []string
	// MaxBudgetUSD caps spend for a single process, 0 for no cap.
	MaxBudgetUSD float64
	// Env, when non-nil, replaces the child's environment.
	Env []string
}

func (c *Config) applyDefaults() {
	if c.Binary == "" {
		c.Binary = "claude"
	}
	if len(c.AllowedTools) == 0 {
		c.AllowedTools = []string{"Read", "Edit", "Write", "Grep", "Glob"}
	}
}

// PermissionMode is how the CLI is told to answer its own permission prompts.
// Nothing the reviewer does should stop to ask a question nobody is watching
// for, and this is the narrowest mode that never blocks.
const PermissionMode = "acceptEdits"

// EventKind distinguishes what a Session emits mid-turn.
type EventKind int

const (
	// EventText is a fragment of assistant prose.
	EventText EventKind = iota
	// EventToolUse reports that the model called a tool. For Edit and Write,
	// Path names the file it touched.
	EventToolUse
)

// Event is a mid-turn notification, delivered in order.
type Event struct {
	Kind EventKind
	Text string
	Tool string
	Path string
}

// TurnResult summarises one completed exchange.
type TurnResult struct {
	// Text is the assistant's final prose for the turn.
	Text string
	// Model is the model the CLI resolved for this turn, as it reported it in
	// the init frame -- "claude-sonnet-5" for a --model of "sonnet", and the
	// user's own configured default when the daemon asked for nothing.
	Model string
	// Edited lists absolute or working-dir-relative paths the model wrote to.
	Edited []string
	// CostUSD is what the turn cost, as reported by the CLI.
	CostUSD float64
	// IsError reports a turn the CLI itself considered failed.
	IsError bool
}

// Session is one conversation with one Claude Code process. Turns are
// serialised: a Session handles one Ask at a time.
type Session struct {
	cfg Config

	// ID is the CLI session id. It is generated before the first launch so the
	// same conversation can be resumed after the process (or the daemon) exits.
	ID string

	turnMu sync.Mutex // serialises Ask

	mu      sync.Mutex // guards the fields below
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	frames  chan streamFrame
	stderr  *ring
	started bool
	// everStarted records that the CLI has seen this session id at least once,
	// which is what decides between --session-id and --resume.
	everStarted bool
	// restartPending is set when a setting that only takes effect at launch --
	// the model -- has changed under a live process.
	restartPending bool
	runningModel   string
	lastUse        time.Time
}

// New creates a Session bound to a CLI session id. The id should be a UUID the
// caller persists alongside the document, so a restart resumes rather than
// starting the review over.
func New(sessionID string, cfg Config) *Session {
	cfg.applyDefaults()
	return &Session{cfg: cfg, ID: sessionID}
}

// ErrBusy is returned when a turn is already in flight and the caller asked not
// to wait.
var ErrBusy = errors.New("claudeproc: a turn is already in progress")

// Ask sends one user message and blocks until the turn completes, calling emit
// for each intermediate event. emit may be nil.
//
// A dead process is restarted transparently and the conversation resumed, which
// covers both an idle-timeout kill and a crash.
func (s *Session) Ask(ctx context.Context, prompt string, emit func(Event)) (TurnResult, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	if err := s.ensureStarted(ctx); err != nil {
		return TurnResult{}, err
	}

	if err := s.send(prompt); err != nil {
		// The most likely cause is a process that exited between turns. Restart
		// once, resuming the same conversation, before giving up.
		s.stop()
		if err := s.ensureStarted(ctx); err != nil {
			return TurnResult{}, err
		}
		if err := s.send(prompt); err != nil {
			return TurnResult{}, fmt.Errorf("send prompt: %w", err)
		}
	}

	result, err := s.readTurn(ctx, emit)
	if err != nil && s.recoverSessionMode(err) {
		if err := s.ensureStarted(ctx); err != nil {
			return TurnResult{}, err
		}
		if err := s.send(prompt); err != nil {
			return TurnResult{}, fmt.Errorf("send prompt: %w", err)
		}
		return s.readTurn(ctx, emit)
	}
	return result, err
}

// recoverSessionMode turns the CLI's refusal to reuse a session id into the one
// bit this Session was missing, and reports that the turn is worth retrying.
//
// `--session-id` is only accepted for an id the CLI has never seen; every launch
// after the first must say `--resume`. Which of the two applies is not derivable
// from the id, and this process learns it by watching for an `init` frame — so a
// daemon that restarts knows the id (it is persisted) but not that the CLI has
// already met it, and its first launch is refused:
//
//	Error: Session ID 5f3c... is already in use.
//
// Persisting the bit alongside the id would only move the problem: an id minted
// and persisted just before a crash was never handed to the CLI, and would then
// be resumed just as wrongly. The refusal itself is the reliable signal, so it
// is what the flag is set from.
func (s *Session) recoverSessionMode(err error) bool {
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.everStarted {
		// Already resuming; the refusal means something else.
		return false
	}
	s.everStarted = true
	return true
}

// Interrupt stops an in-flight turn by killing the process. The next Ask
// resumes the conversation from the last completed turn.
//
// The CLI also speaks a control-protocol interrupt over the same pipe, but
// killing is unambiguous and needs no undocumented framing; the cost is losing
// the partial turn, which is what the reviewer asked for anyway.
func (s *Session) Interrupt() {
	s.stop()
}

// Close shuts the process down. The conversation survives on disk and can be
// resumed by constructing a Session with the same id.
func (s *Session) Close() error {
	s.stop()
	return nil
}

// Idle reports how long since this session last ran a turn.
func (s *Session) Idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastUse.IsZero() {
		return 0
	}
	return time.Since(s.lastUse)
}

// Running reports whether a process is currently alive.
func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// SetModel changes the model this session will use.
//
// The CLI takes the model at launch, so a live process has to go. It is not
// killed here: a turn may be in flight, and interrupting the reviewer's own
// question to apply a setting they just changed would be a poor trade. The
// restart happens at the start of the next turn instead, resuming the same
// conversation on the new model -- which the CLI is happy to do.
func (s *Session) SetModel(model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Model == model {
		return
	}
	s.cfg.Model = model
	s.restartPending = s.started
}

// RunningModel is the model the CLI reported for the most recent turn, or empty
// before the first one.
func (s *Session) RunningModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runningModel
}

func (s *Session) ensureStarted(ctx context.Context) error {
	if s.takeRestart() {
		s.stop()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	return s.startLocked(ctx)
}

// takeRestart reports whether a pending relaunch is due, clearing the flag. It
// is called with no lock held, because stopping the process takes one.
func (s *Session) takeRestart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.restartPending {
		return false
	}
	s.restartPending = false
	return true
}

func (s *Session) startLocked(ctx context.Context) error {
	args := s.args()

	// The context deliberately does not bound the process: it outlives any one
	// turn. Cancellation is handled by stop().
	cmd := exec.Command(s.cfg.Binary, args...)
	cmd.Dir = s.cfg.WorkDir
	if s.cfg.Env != nil {
		cmd.Env = s.cfg.Env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", s.cfg.Binary, err)
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stderr = newRing(8 << 10)
	s.started = true

	// A json.Decoder rather than a bufio.Scanner: single frames routinely run to
	// several kilobytes and tool results can be far larger, which a Scanner
	// would reject once past its buffer limit.
	dec := json.NewDecoder(bufio.NewReaderSize(stdout, 64<<10))

	// Exactly one goroutine ever reads this decoder, for the whole life of the
	// process. A reader started per turn would still be blocked in Decode when
	// the next turn began, and two goroutines sharing a json.Decoder corrupts
	// it outright ("JSON decoder out of sync").
	frames := make(chan streamFrame, 64)
	s.frames = frames
	go readFrames(dec, frames)

	go s.drainStderr(stderrPipe)

	// Reap the child so a long-running daemon does not accumulate zombies.
	// Nothing waits on this: the frames channel closing is what tells a turn
	// the process has stopped talking.
	go func() { _ = cmd.Wait() }()

	return nil
}

// args builds the command line. The first launch pins a session id; later
// launches resume it, which is what preserves context across an idle kill.
func (s *Session) args() []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		// Nothing the reviewer does should stop to ask a question nobody is
		// watching for; the working directory is what bounds the damage.
		"--permission-mode", PermissionMode,
		// The reviewer's own MCP servers have no business in a document review,
		// and loading them would widen the tool surface unpredictably.
		"--strict-mcp-config",
		"--tools", strings.Join(s.cfg.AllowedTools, ","),
	}

	if s.resumable() {
		args = append(args, "--resume", s.ID)
	} else {
		args = append(args, "--session-id", s.ID)
	}
	if s.cfg.Model != "" {
		args = append(args, "--model", s.cfg.Model)
	}
	if s.cfg.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", s.cfg.SystemPrompt)
	}
	if s.cfg.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", s.cfg.MaxBudgetUSD))
	}
	return args
}

// resumable reports whether the CLI already knows this session id, which is
// true from the second launch onwards.
func (s *Session) resumable() bool {
	return s.everStarted
}

func (s *Session) send(prompt string) error {
	s.mu.Lock()
	stdin := s.stdin
	started := s.started
	s.mu.Unlock()

	if !started || stdin == nil {
		return errors.New("claudeproc: process is not running")
	}

	frame := userFrame{Type: "user"}
	frame.Message.Role = "user"
	frame.Message.Content = []contentBlock{{Type: "text", Text: prompt}}

	line, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if _, err := stdin.Write(append(line, '\n')); err != nil {
		return err
	}

	s.mu.Lock()
	s.lastUse = time.Now()
	s.mu.Unlock()
	return nil
}

// readFrames decodes stdout until the stream ends, then closes the channel to
// signal that the process is done talking.
func readFrames(dec *json.Decoder, out chan<- streamFrame) {
	defer close(out)
	for {
		var msg streamFrame
		if err := dec.Decode(&msg); err != nil {
			return
		}
		out <- msg
	}
}

// readTurn consumes stream frames until the CLI reports the turn is over.
func (s *Session) readTurn(ctx context.Context, emit func(Event)) (TurnResult, error) {
	s.mu.Lock()
	frames := s.frames
	s.mu.Unlock()

	if frames == nil {
		return TurnResult{}, errors.New("claudeproc: no output stream")
	}

	var result TurnResult
	seen := map[string]bool{}

	for {
		select {
		case <-ctx.Done():
			// Leaving a half-read turn in the stream would desynchronise every
			// turn after it, so an abandoned turn takes the process with it.
			// The conversation is resumed from disk on the next Ask.
			s.stop()
			return result, ctx.Err()

		case msg, ok := <-frames:
			if !ok {
				s.stop()
				return result, fmt.Errorf("claude exited mid-turn: %s", s.stderrTail())
			}
			if done := s.applyFrame(msg, &result, seen, emit); done {
				return result, nil
			}
		}
	}
}

// applyFrame folds one stream frame into the running result. It reports true
// when the frame ends the turn.
func (s *Session) applyFrame(msg streamFrame, result *TurnResult, seen map[string]bool, emit func(Event)) bool {
	switch msg.Type {
	case "system":
		if msg.Subtype == "init" {
			s.mu.Lock()
			s.everStarted = true
			s.runningModel = msg.Model
			s.mu.Unlock()
			result.Model = msg.Model
		}

	case "assistant":
		var m assistantMessage
		if len(msg.Message) > 0 && json.Unmarshal(msg.Message, &m) == nil {
			for _, block := range m.Content {
				switch block.Type {
				case "text":
					result.Text += block.Text
					if emit != nil && block.Text != "" {
						emit(Event{Kind: EventText, Text: block.Text})
					}
				case "tool_use":
					path := block.Input.FilePath
					if isWriteTool(block.Name) && path != "" && !seen[path] {
						seen[path] = true
						result.Edited = append(result.Edited, path)
					}
					if emit != nil {
						emit(Event{Kind: EventToolUse, Tool: block.Name, Path: path})
					}
				}
			}
		}

	case "result":
		// The CLI's own summary wins over accumulated text: it is the final
		// answer after any retries within the turn.
		if msg.Result != "" {
			result.Text = msg.Result
		}
		result.CostUSD = msg.TotalCostUSD
		result.IsError = msg.IsError
		s.mu.Lock()
		s.lastUse = time.Now()
		s.mu.Unlock()
		return true
	}
	return false
}

func isWriteTool(name string) bool {
	switch name {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

func (s *Session) drainStderr(r io.Reader) {
	buf := make([]byte, 4<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.mu.Lock()
			if s.stderr != nil {
				s.stderr.Write(buf[:n])
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) stderrTail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stderr == nil {
		return "no stderr"
	}
	tail := strings.TrimSpace(s.stderr.String())
	if tail == "" {
		return "no stderr output"
	}
	return tail
}

func (s *Session) stop() {
	s.mu.Lock()
	cmd, stdin := s.cmd, s.stdin
	s.cmd, s.stdin, s.frames = nil, nil, nil
	s.started = false
	s.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// --- wire types -------------------------------------------------------------

type userFrame struct {
	Type    string `json:"type"`
	Message struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// streamFrame is the envelope every stdout line shares. Only the fields the
// daemon acts on are declared; the CLI emits a great deal more.
type streamFrame struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	SessionID    string          `json:"session_id"`
	Message      json.RawMessage `json:"message"`
	Model        string          `json:"model"`
	Result       string          `json:"result"`
	IsError      bool            `json:"is_error"`
	TotalCostUSD float64         `json:"total_cost_usd"`
}

type assistantMessage struct {
	Content []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Name  string `json:"name"`
		Input struct {
			FilePath string `json:"file_path"`
		} `json:"input"`
	} `json:"content"`
}
