// Package logview serves a page for reading a claude wire log.
//
// It has no authentication, and main refuses to bind it anywhere but loopback.
// The log holds every file the model read and every prompt it was sent, which
// is more than the review itself exposes, so a password page would be the one
// thing standing between the LAN and all of it. An SSH tunnel is the way to
// read it from elsewhere.
package logview

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gilramir/ai-reviewer/internal/wirelog"
	"github.com/gilramir/ai-reviewer/web"
)

// maxRows bounds one /api/frames reply. The viewer asks again from the last id
// until a reply comes back short, so a large log arrives in pieces rather than
// as one response the browser has to hold twice.
const maxRows = 2000

// Handler serves the viewer page and the JSON it reads.
func Handler(log *wirelog.Reader) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/frames", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		rows, err := log.Rows(after, maxRows)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"frames": rows, "more": len(rows) == maxRows})
	})
	mux.HandleFunc("/api/frame", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil {
			http.Error(w, "id must be a number", http.StatusBadRequest)
			return
		}
		d, err := log.Detail(id)
		if errors.Is(err, wirelog.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, d)
	})
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(web.Static()))))
	mux.Handle("/dist/", http.StripPrefix("/dist/", http.FileServer(http.FS(web.Dist()))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page, err := web.Static().Open("log.html")
		if err != nil {
			http.Error(w, "missing log.html", http.StatusInternalServerError)
			return
		}
		defer page.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "log.html", time.Time{}, page.(readSeeker))
	})
	return mux
}

type readSeeker interface {
	Read([]byte) (int, error)
	Seek(int64, int) (int64, error)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
