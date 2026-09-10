package server

import (
	"net/http"
	"os"
	"strings"

	"github.com/gilramir/ai-reviewer/internal/review"
)

// handleFile serves a file the document under review points at.
//
// Rendering rewrites `![](flow.png)` to this route, so an image is fetched from
// the review root rather than resolved against the page's own URL, where there
// has never been anything to find.
//
// Two headers keep a directory of arbitrary files from becoming a way to run
// code in this origin. `nosniff` stops a text file from being guessed into
// HTML, and the sandbox policy neuters an SVG or an HTML file opened directly
// in a tab -- an image is still an image inside <img>, where scripts never run
// either way.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, review.AssetRoute)

	full, err := s.opts.Review.AssetPath(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	file, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	// The URL carries the file's modification time, so a cached copy is only
	// ever the right one -- but a document rendered before its image existed
	// has an unstamped URL, and that one has to be revalidated.
	w.Header().Set("Cache-Control", "no-cache")

	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}
