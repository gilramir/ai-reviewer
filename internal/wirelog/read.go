package wirelog

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Reader reads a wire log, possibly while a serve process is still writing it.
// WAL mode is what lets the two run side by side.
type Reader struct {
	db *sql.DB
}

// OpenReader opens path read-only. It refuses a file that is not there rather
// than creating an empty one, which would look like a log with nothing in it.
func OpenReader(path string) (*Reader, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Reader{db: db}, nil
}

// Close closes the database.
func (r *Reader) Close() error {
	return r.db.Close()
}

// Row is one line of the viewer's list.
type Row struct {
	ID        int64    `json:"id"`
	TS        int64    `json:"ts"`
	Dir       string   `json:"dir"`
	Type      string   `json:"type"`
	Subtype   string   `json:"subtype"`
	Process   int64    `json:"process"`
	Run       int64    `json:"run"`
	Role      string   `json:"role"`
	Doc       string   `json:"doc"`
	Turn      *int64   `json:"turn"`
	TurnStart *int64   `json:"turnStart"`
	ReplyTo   *int64   `json:"replyTo"`
	Cost      *float64 `json:"cost"`
	Summary   string   `json:"summary"`
}

// Rows returns up to limit frames with ids above after, oldest first. Polling
// with the last id seen is how the viewer follows a log still being written.
func (r *Reader) Rows(after int64, limit int) ([]Row, error) {
	rows, err := r.db.Query(`
		SELECT f.id, f.ts, f.dir, COALESCE(f.type, ''), COALESCE(f.subtype, ''),
		       f.process_id, p.run_id, COALESCE(p.role, ''),
		       COALESCE(t.doc, p.doc, ''), f.turn_id, t.started_at, f.reply_to,
		       f.cost_usd, COALESCE(f.summary, '')
		FROM frame f
		JOIN process p ON p.id = f.process_id
		LEFT JOIN turn t ON t.id = f.turn_id
		WHERE f.id > ?
		ORDER BY f.id
		LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Row{}
	for rows.Next() {
		var row Row
		if err := rows.Scan(&row.ID, &row.TS, &row.Dir, &row.Type, &row.Subtype,
			&row.Process, &row.Run, &row.Role, &row.Doc, &row.Turn, &row.TurnStart, &row.ReplyTo,
			&row.Cost, &row.Summary); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Detail is everything the lower pane shows for one frame.
type Detail struct {
	ID    int64           `json:"id"`
	TS    int64           `json:"ts"`
	Dir   string          `json:"dir"`
	Raw   json.RawMessage `json:"raw"`
	Tools []ToolCall      `json:"tools"`
	Turn  *Turn           `json:"turn"`
	Proc  Process         `json:"process"`
}

// ToolCall links a tool_use to the frame holding its result.
type ToolCall struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	UseFrame    int64  `json:"useFrame"`
	ResultFrame *int64 `json:"resultFrame"`
	IsError     *bool  `json:"isError"`
}

// Turn is a turn's row: what it was for and what its result frame reported.
type Turn struct {
	ID                  int64    `json:"id"`
	StartedAt           int64    `json:"startedAt"`
	EndedAt             *int64   `json:"endedAt"`
	Doc                 string   `json:"doc"`
	Purpose             string   `json:"purpose"`
	Model               string   `json:"model"`
	Subtype             string   `json:"subtype"`
	IsError             *bool    `json:"isError"`
	CostUSD             *float64 `json:"cost"`
	InputTokens         *int64   `json:"inputTokens"`
	OutputTokens        *int64   `json:"outputTokens"`
	CacheReadTokens     *int64   `json:"cacheReadTokens"`
	CacheCreationTokens *int64   `json:"cacheCreationTokens"`
	DurationMS          *int64   `json:"durationMs"`
	DurationAPIMS       *int64   `json:"durationApiMs"`
	NumTurns            *int64   `json:"numTurns"`
}

// Process is a process's row.
type Process struct {
	ID         int64           `json:"id"`
	Run        int64           `json:"run"`
	StartedAt  int64           `json:"startedAt"`
	EndedAt    *int64          `json:"endedAt"`
	Role       string          `json:"role"`
	Doc        string          `json:"doc"`
	SessionID  string          `json:"sessionId"`
	Argv       json.RawMessage `json:"argv"`
	Cwd        string          `json:"cwd"`
	PID        int64           `json:"pid"`
	EndReason  string          `json:"endReason"`
	ExitStatus string          `json:"exitStatus"`
}

// Detail returns one frame with its turn, its process and its tool links.
func (r *Reader) Detail(id int64) (Detail, error) {
	var d Detail
	var raw string
	var turnID *int64
	var procID int64
	err := r.db.QueryRow(`SELECT id, ts, dir, raw, turn_id, process_id FROM frame WHERE id = ?`, id).
		Scan(&d.ID, &d.TS, &d.Dir, &raw, &turnID, &procID)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.Raw = json.RawMessage(raw)
	if !json.Valid(d.Raw) {
		// Every row is written as JSON, but a log is not the place to fail
		// over one that somehow is not.
		d.Raw = mustJSON(map[string]string{"text": raw})
	}

	var argv string
	err = r.db.QueryRow(`SELECT id, run_id, started_at, ended_at, COALESCE(role, ''), COALESCE(doc, ''),
		COALESCE(session_id, ''), COALESCE(argv, '[]'), COALESCE(cwd, ''), COALESCE(pid, 0),
		COALESCE(end_reason, ''), COALESCE(exit_status, '') FROM process WHERE id = ?`, procID).
		Scan(&d.Proc.ID, &d.Proc.Run, &d.Proc.StartedAt, &d.Proc.EndedAt, &d.Proc.Role, &d.Proc.Doc,
			&d.Proc.SessionID, &argv, &d.Proc.Cwd, &d.Proc.PID, &d.Proc.EndReason, &d.Proc.ExitStatus)
	if err != nil {
		return d, err
	}
	d.Proc.Argv = json.RawMessage(argv)

	if turnID != nil {
		var t Turn
		err = r.db.QueryRow(`SELECT id, started_at, ended_at, COALESCE(doc, ''), COALESCE(purpose, ''),
			COALESCE(model, ''), COALESCE(subtype, ''), is_error, cost_usd, input_tokens, output_tokens,
			cache_read_tokens, cache_creation_tokens, duration_ms, duration_api_ms, num_turns
			FROM turn WHERE id = ?`, *turnID).
			Scan(&t.ID, &t.StartedAt, &t.EndedAt, &t.Doc, &t.Purpose, &t.Model, &t.Subtype, &t.IsError,
				&t.CostUSD, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheCreationTokens,
				&t.DurationMS, &t.DurationAPIMS, &t.NumTurns)
		if err != nil {
			return d, err
		}
		d.Turn = &t
	}

	rows, err := r.db.Query(`SELECT tool_use_id, COALESCE(name, ''), use_frame_id, result_frame_id, is_error
		FROM tool_call WHERE use_frame_id = ? OR result_frame_id = ? ORDER BY use_frame_id, rowid`, id, id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	d.Tools = []ToolCall{}
	for rows.Next() {
		var c ToolCall
		if err := rows.Scan(&c.ID, &c.Name, &c.UseFrame, &c.ResultFrame, &c.IsError); err != nil {
			return d, err
		}
		d.Tools = append(d.Tools, c)
	}
	return d, rows.Err()
}
