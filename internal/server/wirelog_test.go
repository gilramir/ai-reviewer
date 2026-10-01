package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gilramir/ai-reviewer/internal/review"
	"github.com/gilramir/ai-reviewer/internal/wirelog"
)

// A comment filed in the browser reaches the wire log as a turn about the
// right document and the right thread, with the frames the stub sent back and
// the cost its result frame reported.
func TestACommentIsInTheWireLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.db")
	log, err := wirelog.Create(path, wirelog.Run{Root: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })

	rev, _ := newReviewWith(t, func(o *review.Options) { o.Tap = log })

	ts := httptest.NewServer(New(Options{Review: rev}))
	defer ts.Close()
	conn := dialWS(t, ts, &http.Client{Jar: &simpleJar{}}, ts.URL)
	waitFor(t, conn, "docList")

	send(t, conn, map[string]any{
		"type": "comment",
		"doc":  "spec.md",
		"anchor": map[string]any{
			"nodeId": "n-1",
			"quote":  "The system SHALL retry indefinitely until the operation succeeds.",
		},
		"body": "why this?",
	})
	end := waitFor(t, conn, "turnEnd")
	threadID, _ := end["threadId"].(string)
	require.NotEmpty(t, threadID)

	r, err := wirelog.OpenReader(path)
	require.NoError(t, err)
	defer r.Close()
	rows, err := r.Rows(0, 100)
	require.NoError(t, err)

	var types []string
	var prompt wirelog.Row
	for _, row := range rows {
		types = append(types, row.Type)
		if row.Dir == wirelog.DirIn {
			prompt = row
		}
	}
	assert.Equal(t, []string{"spawn", "user", "system", "assistant", "result"}, types)

	d, err := r.Detail(prompt.ID)
	require.NoError(t, err)
	require.NotNil(t, d.Turn)
	assert.Equal(t, "spec.md", d.Turn.Doc)
	assert.Equal(t, "thread "+threadID, d.Turn.Purpose)
	require.NotNil(t, d.Turn.CostUSD)
	assert.InDelta(t, 0.01, *d.Turn.CostUSD, 1e-9)
	assert.Equal(t, "reviewer", d.Proc.Role)
}
