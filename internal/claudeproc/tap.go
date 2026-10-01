package claudeproc

import "context"

// Tap observes the pipes between the daemon and each claude process, for the
// wire log. It sees bytes exactly as they crossed, not the decoded frames:
// streamFrame keeps only the fields the daemon acts on, and what a log is for
// is the fields nobody has acted on yet -- a frame type a CLI upgrade added, a
// usage figure the daemon never read.
//
// Every call blocks the pipe it describes. A tap that falls behind slows the
// turn rather than losing frames, because a debugging log with holes in it
// misleads.
type Tap interface {
	Spawn(Spawn) ProcessTap
}

// Spawn describes a process as it was launched.
type Spawn struct {
	// Role is Config.Role: which kind of conversation this process holds.
	Role      string
	SessionID string
	// Doc is the document the turn that caused the launch was about.
	Doc  string
	Argv []string
	Dir  string
	PID  int
}

// ProcessTap observes one process, from launch to exit. Calls arrive from
// several goroutines -- the turn, the stdout reader, the stderr reader and the
// reaper -- and the implementation must serialise them itself.
type ProcessTap interface {
	// In is a user frame about to be written to stdin. It starts a turn.
	In(turn TurnLabel, line []byte)
	// Out is one frame read from stdout.
	Out(line []byte)
	// Stderr is a chunk of stderr, split wherever the read happened to land.
	Stderr(chunk []byte)
	// Note records something the daemon did or saw that is not a frame:
	// "stop" with the reason it killed the process, "decode" with why stdout
	// stopped parsing.
	Note(event, detail string)
	// Exited reports the process's exit status, nil for a clean exit.
	Exited(err error)
}

// TurnLabel says what a turn was for. claudeproc knows neither the document
// nor the thread, so the caller attaches them to the context it passes to Ask.
type TurnLabel struct {
	Doc     string
	Purpose string
}

type labelKey struct{}

// WithTurnLabel attaches a label for the wire log to a turn's context.
func WithTurnLabel(ctx context.Context, label TurnLabel) context.Context {
	return context.WithValue(ctx, labelKey{}, label)
}

func turnLabel(ctx context.Context) TurnLabel {
	label, _ := ctx.Value(labelKey{}).(TurnLabel)
	return label
}

// noTap stands in when no log was asked for, so the session code has no nil
// checks to forget.
type noTap struct{}

func (noTap) In(TurnLabel, []byte) {}
func (noTap) Out([]byte)           {}
func (noTap) Stderr([]byte)        {}
func (noTap) Note(string, string)  {}
func (noTap) Exited(error)         {}
