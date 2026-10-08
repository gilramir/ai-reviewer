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
	// AddDirs are directories beyond WorkDir the tools may reach. The working
	// directory is the CLI's permission boundary, and in -p mode there is
	// nobody to answer the prompt for a path outside it, so the read is simply
	// refused. Absolute paths: the CLI resolves a relative one against WorkDir,
	// which is not where whoever typed it was standing.
	AddDirs []string
	// MaxBudgetUSD caps spend for a single process, 0 for no cap.
	MaxBudgetUSD float64
	// Env, when non-nil, replaces the child's environment.
	Env []string
	// Role names the kind of conversation, for the wire log: "reviewer" or
	// "critic".
	Role string
	// Tap, when non-nil, sees every byte crossing each process's pipes.
	Tap Tap
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
	// Denials are the tool calls the CLI refused during the turn. The model
	// sees a refusal as a tool error and usually says only that it could not
	// do something; this is the record of what it was refused, and where.
	Denials []Denial
}

// Denial is one tool call the CLI refused to run.
type Denial struct {
	Tool string
	// Target is what the call was aimed at: a file path for Read and Edit, the
	// search directory or pattern for Grep and Glob. Empty when the input names
	// nothing recognisable.
	Target string
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
	tap     ProcessTap
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

	if err := s.send(ctx, prompt); err != nil {
		// The most likely cause is a process that exited between turns. Restart
		// once, resuming the same conversation, before giving up.
		s.stop("send failed: " + err.Error())
		if err := s.ensureStarted(ctx); err != nil {
			return TurnResult{}, err
		}
		if err := s.send(ctx, prompt); err != nil {
			return TurnResult{}, fmt.Errorf("send prompt: %w", err)
		}
	}

	result, err := s.readTurn(ctx, emit)
	if err != nil && s.recoverSessionMode(err) {
		if err := s.ensureStarted(ctx); err != nil {
			return TurnResult{}, err
		}
		if err := s.send(ctx, prompt); err != nil {
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
	s.stop("interrupted")
}

// Close shuts the process down. The conversation survives on disk and can be
// resumed by constructing a Session with the same id.
func (s *Session) Close() error {
	return s.closeFor("closed")
}

// closeFor is Close with the reason the wire log should give. The manager has
// four reasons to close a session, and from the log they would otherwise be
// indistinguishable from each other and from a crash.
func (s *Session) closeFor(reason string) error {
	s.stop(reason)
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
		s.stop("model changed")
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

	var tap ProcessTap = noTap{}
	if s.cfg.Tap != nil {
		tap = s.cfg.Tap.Spawn(Spawn{
			Role:      s.cfg.Role,
			SessionID: s.ID,
			Doc:       turnLabel(ctx).Doc,
			Argv:      append([]string{s.cfg.Binary}, args...),
			Dir:       cmd.Dir,
			PID:       cmd.Process.Pid,
		})
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stderr = newRing(8 << 10)
	s.tap = tap
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
	go readFrames(dec, frames, tap)

	go s.drainStderr(stderrPipe, tap)

	// Reap the child so a long-running daemon does not accumulate zombies.
	// Nothing waits on this: the frames channel closing is what tells a turn
	// the process has stopped talking.
	go func() { tap.Exited(cmd.Wait()) }()

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
	// One switch per directory. --add-dir is variadic, so a list behind a
	// single switch would swallow whatever argument came after it.
	for _, dir := range s.cfg.AddDirs {
		args = append(args, "--add-dir", dir)
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

func (s *Session) send(ctx context.Context, prompt string) error {
	s.mu.Lock()
	stdin := s.stdin
	started := s.started
	tap := s.tap
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
	// Logged before the write, so the turn exists by the time the first frame
	// of the reply could arrive to be filed under it.
	tap.In(turnLabel(ctx), line)
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
//
// Each frame is taken whole first and only then picked apart, so the tap sees
// the bytes the CLI wrote rather than the handful of fields streamFrame keeps.
func readFrames(dec *json.Decoder, out chan<- streamFrame, tap ProcessTap) {
	defer close(out)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if !errors.Is(err, io.EOF) {
				tap.Note("decode", err.Error())
			}
			return
		}
		tap.Out(raw)

		var msg streamFrame
		if err := json.Unmarshal(raw, &msg); err != nil {
			tap.Note("decode", err.Error())
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
			s.stop("turn abandoned: " + ctx.Err().Error())
			return result, ctx.Err()

		case msg, ok := <-frames:
			if !ok {
				s.stop("exited mid-turn")
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
		result.Denials = denials(msg.PermissionDenials)
		s.mu.Lock()
		s.lastUse = time.Now()
		s.mu.Unlock()
		return true
	}
	return false
}

// denials folds the CLI's refusals into one entry per tool and target. A model
// refused once tends to try the same path again, sometimes spelled
// differently, and the second identical line says nothing the first did not.
func denials(raw []permissionDenial) []Denial {
	var out []Denial
	seen := map[Denial]bool{}
	for _, d := range raw {
		in := d.ToolInput
		target := in.FilePath
		for _, alt := range []string{in.NotebookPath, in.Path, in.Pattern} {
			if target == "" {
				target = alt
			}
		}
		denial := Denial{Tool: d.ToolName, Target: target}
		if !seen[denial] {
			seen[denial] = true
			out = append(out, denial)
		}
	}
	return out
}

func isWriteTool(name string) bool {
	switch name {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

func (s *Session) drainStderr(r io.Reader, tap ProcessTap) {
	buf := make([]byte, 4<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			tap.Stderr(buf[:n])
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

// stop kills the process, if there is one, giving the wire log the reason.
func (s *Session) stop(reason string) {
	s.mu.Lock()
	cmd, stdin, tap := s.cmd, s.stdin, s.tap
	s.cmd, s.stdin, s.frames, s.tap = nil, nil, nil, nil
	s.started = false
	s.mu.Unlock()

	if cmd != nil && tap != nil {
		tap.Note("stop", reason)
	}

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
	// PermissionDenials is only on the result frame.
	PermissionDenials []permissionDenial `json:"permission_denials"`
}

type permissionDenial struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Path         string `json:"path"`
		Pattern      string `json:"pattern"`
	} `json:"tool_input"`
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
