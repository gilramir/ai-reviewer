package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestChangesReachTheBrowser follows a change all the way out: a document is
// opened, the file underneath it is rewritten, and the next render has to say
// which words are new -- in coordinates that index the text the browser builds
// out of the very frame it was sent.
//
// That last part is the point of going through the socket rather than calling
// Render. The offsets are agreed between two languages, and the only way to
// know the agreement holds is to rebuild the string the way the client does,
// from the JSON the client receives, and see what the offsets land on.
func TestChangesReachTheBrowser(t *testing.T) {
	rev, root := newReview(t)

	const secret = "test-secret"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	waitFor(t, conn, "docList")

	// The first sight of the document is the baseline this session measures
	// against, so it must arrive with nothing marked.
	send(t, conn, map[string]any{"type": "openDoc", "path": "spec.md"})
	first := waitFor(t, conn, "doc")
	if changes, ok := first["changes"]; ok {
		t.Fatalf("the first render reported changes: %v", changes)
	}

	rewritten := strings.Replace(testDoc,
		"The system SHALL retry indefinitely until the operation succeeds.",
		"The system retries three times, with backoff, until the operation succeeds.", 1)
	if err := os.WriteFile(filepath.Join(root, "spec.md"), []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}

	send(t, conn, map[string]any{"type": "openDoc", "path": "spec.md"})
	second := waitFor(t, conn, "doc")

	text := clientText(t, second["doc"])
	marked := highlight(t, text, second["changes"])

	const want = "Retry policyThe system [retries three times, with backoff,] until the operation succeeds.Unrelated paragraph."
	if marked != want {
		t.Errorf("\n got %q\nwant %q", marked, want)
	}
}

// clientText rebuilds a document's text the way Doc.text in the browser does:
// each node's own contribution, then its children's, with nothing between one
// block and the next. Written out here rather than shared with the server's
// own walk, so that a change to the server's idea of the text shows up as a
// failure instead of being agreed with.
func clientText(t *testing.T, doc any) []rune {
	t.Helper()

	frame, ok := doc.(map[string]any)
	if !ok {
		t.Fatalf("doc frame has no document: %T", doc)
	}
	var out []rune
	var walk func(node any)
	walk = func(node any) {
		n, ok := node.(map[string]any)
		if !ok {
			return
		}
		text, _ := n["text"].(string)
		switch n["kind"] {
		case "text", "codeSpan", "codeBlock", "htmlBlock", "rawHtml":
			out = append(out, []rune(text)...)
		case "lineBreak":
			out = append(out, ' ')
		}
		kids, _ := n["children"].([]any)
		for _, kid := range kids {
			walk(kid)
		}
	}
	walk(frame["root"])
	return out
}

// highlight brackets what the change spans cover, so a failure reads as the
// passage that lit up rather than as a pair of numbers.
func highlight(t *testing.T, text []rune, changes any) string {
	t.Helper()

	spans, ok := changes.([]any)
	if !ok {
		t.Fatalf("no changes in the frame: %T", changes)
	}

	var out []rune
	at := 0
	for _, span := range spans {
		s, ok := span.(map[string]any)
		if !ok {
			t.Fatalf("a change is not an object: %T", span)
		}
		start, end := int(s["start"].(float64)), int(s["end"].(float64))
		if start < at || end > len(text) || end < start {
			t.Fatalf("change %d..%d is not inside 0..%d, or is out of order", start, end, len(text))
		}
		out = append(out, text[at:start]...)
		out = append(out, '[')
		out = append(out, text[start:end]...)
		out = append(out, ']')
		at = end
	}
	return string(append(out, text[at:]...))
}
