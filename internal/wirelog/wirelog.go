// Package wirelog records the traffic between the daemon and its claude
// processes in a SQLite file, and reads it back for the viewer.
//
// The pipe carries no request/response pairs. One user frame written to stdin
// is answered by any number of frames on stdout, up to a result frame, so the
// log is a tree rather than a list of exchanges:
//
//	run       one `ai-reviewer serve`
//	process   one claude launch: its argv, why the daemon ended it, how it exited
//	turn      the user frame that opened it, and the cost its result frame reported
//	frame     everything that crossed, in either direction, plus stderr
//
// and across that tree, a tool_result frame points back at the frame holding
// the tool_use it answers (tool_call), since that is the link a reader follows.
//
// Every frame is stored as it crossed the pipe, less any credentials (see
// redact.go). The other columns are taken out of it on the way in so the list
// can be drawn without parsing anything.
package wirelog

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/gilramir/ai-reviewer/internal/claudeproc"
)

const schema = `
CREATE TABLE IF NOT EXISTS run (
	id         INTEGER PRIMARY KEY,
	started_at INTEGER NOT NULL,
	root       TEXT,
	version    TEXT,
	pid        INTEGER
);
CREATE TABLE IF NOT EXISTS process (
	id          INTEGER PRIMARY KEY,
	run_id      INTEGER NOT NULL REFERENCES run(id),
	started_at  INTEGER NOT NULL,
	ended_at    INTEGER,
	role        TEXT,
	doc         TEXT,
	session_id  TEXT,
	argv        TEXT,
	cwd         TEXT,
	pid         INTEGER,
	end_reason  TEXT,
	exit_status TEXT
);
CREATE TABLE IF NOT EXISTS turn (
	id                    INTEGER PRIMARY KEY,
	process_id            INTEGER NOT NULL REFERENCES process(id),
	started_at            INTEGER NOT NULL,
	ended_at              INTEGER,
	doc                   TEXT,
	purpose               TEXT,
	model                 TEXT,
	subtype               TEXT,
	is_error              INTEGER,
	cost_usd              REAL,
	input_tokens          INTEGER,
	output_tokens         INTEGER,
	cache_read_tokens     INTEGER,
	cache_creation_tokens INTEGER,
	duration_ms           INTEGER,
	duration_api_ms       INTEGER,
	num_turns             INTEGER
);
CREATE TABLE IF NOT EXISTS frame (
	id                 INTEGER PRIMARY KEY,
	process_id         INTEGER NOT NULL REFERENCES process(id),
	turn_id            INTEGER REFERENCES turn(id),
	ts                 INTEGER NOT NULL,
	dir                TEXT NOT NULL,
	type               TEXT,
	subtype            TEXT,
	message_id         TEXT,
	parent_tool_use_id TEXT,
	reply_to           INTEGER REFERENCES frame(id),
	cost_usd           REAL,
	summary            TEXT,
	raw                TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS frame_turn ON frame(turn_id);
CREATE INDEX IF NOT EXISTS frame_process ON frame(process_id);
CREATE TABLE IF NOT EXISTS tool_call (
	process_id      INTEGER NOT NULL REFERENCES process(id),
	tool_use_id     TEXT NOT NULL,
	name            TEXT,
	use_frame_id    INTEGER NOT NULL REFERENCES frame(id),
	result_frame_id INTEGER REFERENCES frame(id),
	is_error        INTEGER,
	PRIMARY KEY (process_id, tool_use_id)
);
`

// Directions a frame can have crossed in. "daemon" rows are the daemon's own
// notes -- a launch, a kill, an exit -- so the list tells the whole story
// without a second table to read alongside it.
const (
	DirIn     = "in"
	DirOut    = "out"
	DirStderr = "stderr"
	DirDaemon = "daemon"
)

// Run describes the serve invocation writing to the log.
type Run struct {
	Root    string
	Version string
}

// Log is an open wire log being written. It implements claudeproc.Tap.
//
// Writes are synchronous under one mutex. SQLite has one writer anyway, an
// insert into a WAL database costs microseconds, and doing it inline is what
// makes a slow disk slow the turn down instead of dropping frames.
type Log struct {
	db    *sql.DB
	runID int64

	mu sync.Mutex
	// failed is set by the first write that fails, which is reported once on
	// stderr rather than once per frame.
	failed bool
	// closed is set by Close. A process reaped during shutdown reports its
	// exit after the log is gone, and that is not a failure worth a warning.
	closed bool
}

// Create opens path for writing, creating it and its schema as needed, and
// records the start of a run. The file is readable by its owner only: it holds
// every file the model read.
func Create(path string, run Run) (*Log, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()

	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	// One connection: one writer, and no second connection to see a
	// half-applied pragma.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	res, err := db.Exec(`INSERT INTO run (started_at, root, version, pid) VALUES (?, ?, ?, ?)`,
		now(), run.Root, run.Version, os.Getpid())
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	runID, _ := res.LastInsertId()

	return &Log{db: db, runID: runID}, nil
}

// Close flushes and closes the database.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return l.db.Close()
}

// Spawn records a process launch and returns the tap for its pipes.
func (l *Log) Spawn(s claudeproc.Spawn) claudeproc.ProcessTap {
	argv := redact(mustJSON(s.Argv))

	l.mu.Lock()
	defer l.mu.Unlock()

	p := &processTap{log: l, tools: map[string]string{}}
	res, ok := l.execLocked(`INSERT INTO process (run_id, started_at, role, doc, session_id, argv, cwd, pid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		l.runID, now(), s.Role, s.Doc, s.SessionID, string(argv), s.Dir, s.PID)
	if ok {
		p.id, _ = res.LastInsertId()
	}

	p.daemonLocked("spawn", map[string]any{
		"role":       s.Role,
		"doc":        s.Doc,
		"session_id": s.SessionID,
		"argv":       json.RawMessage(argv),
		"cwd":        s.Dir,
		"pid":        s.PID,
	}, fmt.Sprintf("spawn pid %d  %s", s.PID, s.Doc))
	return p
}

// execLocked runs one write, reporting whether it happened.
func (l *Log) execLocked(query string, args ...any) (sql.Result, bool) {
	if l.closed {
		return nil, false
	}
	res, err := l.db.Exec(query, args...)
	return res, l.check(err)
}

// check reports whether err is nil, and complains about the first failure.
// A log that cannot be written is not a reason to stop reviewing.
func (l *Log) check(err error) bool {
	if err == nil {
		return true
	}
	if !l.failed {
		l.failed = true
		fmt.Fprintf(os.Stderr, "  warning: the claude wire log stopped recording: %v\n", err)
	}
	return false
}

// processTap is one process's view of the log. Every method takes the log's
// mutex, which is what serialises the four goroutines calling in.
//
// Tool calls are matched within a process. The API's ids are unique anyway,
// but a stub CLI need not mint them so carefully, and a link to the wrong
// process's frame is worse than none.
type processTap struct {
	log  *Log
	id   int64
	turn int64 // the open turn, 0 between turns
	// tools maps a tool_use id to the call's name, until its result arrives.
	tools map[string]string
}

func (p *processTap) In(label claudeproc.TurnLabel, line []byte) {
	line = redact(line)

	l := p.log
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := now()
	res, ok := l.execLocked(`INSERT INTO turn (process_id, started_at, doc, purpose) VALUES (?, ?, ?, ?)`,
		p.id, ts, label.Doc, label.Purpose)
	if !ok {
		return
	}
	p.turn, _ = res.LastInsertId()

	f := parseFrame(line)
	f.summary = summariseIn(f)
	p.insertLocked(ts, DirIn, f, nil, string(line))
}

func (p *processTap) Out(line []byte) {
	line = redact(line)

	l := p.log
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := now()
	f := parseFrame(line)
	f.summary = summarise(f, p.tools)

	// A tool_result answers a tool_use in an earlier frame. A frame can carry
	// several results; the first one is the frame's parent, and every one of
	// them is in tool_call for the viewer to follow.
	var replyTo any
	for _, r := range f.results {
		var useFrame int64
		if !l.closed && l.db.QueryRow(`SELECT use_frame_id FROM tool_call WHERE process_id = ? AND tool_use_id = ?`, p.id, r.id).Scan(&useFrame) == nil && replyTo == nil {
			replyTo = useFrame
		}
	}

	id := p.insertLocked(ts, DirOut, f, replyTo, string(line))
	if id == 0 {
		return
	}

	for _, u := range f.uses {
		p.tools[u.id] = u.name
		l.execLocked(`INSERT OR IGNORE INTO tool_call (process_id, tool_use_id, name, use_frame_id) VALUES (?, ?, ?, ?)`,
			p.id, u.id, u.name, id)
	}
	for _, r := range f.results {
		delete(p.tools, r.id)
		l.execLocked(`UPDATE tool_call SET result_frame_id = ?, is_error = ? WHERE process_id = ? AND tool_use_id = ?`,
			id, r.isError, p.id, r.id)
	}

	if f.typ == "system" && f.subtype == "init" && p.turn != 0 {
		l.execLocked(`UPDATE turn SET model = ? WHERE id = ?`, f.model, p.turn)
	}

	if f.result != nil && p.turn != 0 {
		r := f.result
		l.execLocked(`UPDATE turn SET ended_at = ?, subtype = ?, is_error = ?, cost_usd = ?,
			input_tokens = ?, output_tokens = ?, cache_read_tokens = ?, cache_creation_tokens = ?,
			duration_ms = ?, duration_api_ms = ?, num_turns = ? WHERE id = ?`,
			ts, f.subtype, r.IsError, r.TotalCostUSD,
			r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CacheReadInputTokens, r.Usage.CacheCreationInputTokens,
			r.DurationMS, r.DurationAPIMS, r.NumTurns, p.turn)
		// The turn is over. Anything before the next prompt -- stderr, a
		// stop, an exit -- belongs to the process, not to this turn.
		p.turn = 0
	}
}

func (p *processTap) Stderr(chunk []byte) {
	text := string(redact(chunk))

	l := p.log
	l.mu.Lock()
	defer l.mu.Unlock()

	f := frameInfo{summary: firstLine(text, 120)}
	p.insertLocked(now(), DirStderr, f, nil, string(mustJSON(map[string]string{"text": text})))
}

func (p *processTap) Note(event, detail string) {
	l := p.log
	l.mu.Lock()
	defer l.mu.Unlock()

	if event == "stop" {
		// The first reason wins: a turn abandoned by its context, then the
		// manager closing what is already dead, is the abandonment.
		l.execLocked(`UPDATE process SET end_reason = ? WHERE id = ? AND end_reason IS NULL`, detail, p.id)
	}
	p.daemonLocked(event, map[string]any{"detail": detail}, event+": "+detail)
}

func (p *processTap) Exited(err error) {
	status := "exit 0"
	if err != nil {
		status = err.Error()
	}

	l := p.log
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := now()
	l.execLocked(`UPDATE process SET ended_at = ?, exit_status = ? WHERE id = ?`, ts, status, p.id)
	p.daemonLocked("exit", map[string]any{"status": status}, "exit: "+status)
}

func (p *processTap) daemonLocked(event string, fields map[string]any, summary string) {
	fields["event"] = event
	f := frameInfo{typ: event, summary: summary}
	p.insertLocked(now(), DirDaemon, f, nil, string(mustJSON(fields)))
}

// insertLocked writes one frame row and returns its id, 0 if the write failed.
func (p *processTap) insertLocked(ts int64, dir string, f frameInfo, replyTo any, raw string) int64 {
	l := p.log
	if p.id == 0 {
		// The process row itself was never written.
		return 0
	}
	var turn any
	if p.turn != 0 {
		turn = p.turn
	}
	var cost any
	if f.result != nil {
		cost = f.result.TotalCostUSD
	}
	res, ok := l.execLocked(`INSERT INTO frame (process_id, turn_id, ts, dir, type, subtype, message_id,
		parent_tool_use_id, reply_to, cost_usd, summary, raw) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.id, turn, ts, dir, nullable(f.typ), nullable(f.subtype), nullable(f.messageID),
		nullable(f.parentToolUseID), replyTo, cost, f.summary, raw)
	if !ok {
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

// now is the timestamp every row carries: microseconds since the epoch, which
// orders the frames of a fast turn that milliseconds would tie.
func now() int64 {
	return time.Now().UnixMicro()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Only ever called on strings, string slices and maps of them.
		panic(err)
	}
	return b
}

// ErrNotFound is returned by the readers for an id the log does not hold.
var ErrNotFound = errors.New("wirelog: not found")
