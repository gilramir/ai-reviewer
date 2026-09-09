package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/gilramir/ai-reviewer/internal/review"
)

const testDoc = `# Retry policy

The system SHALL retry indefinitely until the operation succeeds.

Unrelated paragraph.
`

// newReview sets up a git repository holding one document, reviewed through the
// stub CLI in testdata.
func newReview(t *testing.T) (*review.Review, string) {
	t.Helper()

	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	docPath := filepath.Join(root, "spec.md")
	if err := os.WriteFile(docPath, []byte(testDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-qm", "initial")

	stub, err := filepath.Abs("testdata/fake-claude")
	if err != nil {
		t.Fatal(err)
	}

	rev, err := review.New(review.Options{
		Root:         root,
		Branch:       "review/test",
		ClaudeBinary: stub,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rev.Close() })

	return rev, root
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// login performs the password exchange and returns a client holding the session
// cookie.
func login(t *testing.T, ts *httptest.Server, secret string) *http.Client {
	t.Helper()

	jar, err := newJar(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.PostForm(ts.URL+"/login", url.Values{"password": {secret}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login returned %d, want a redirect", resp.StatusCode)
	}
	return client
}

func newJar(base string) (http.CookieJar, error) {
	return &simpleJar{base: base}, nil
}

// simpleJar keeps every cookie for the test server without the public-suffix
// rules net/http/cookiejar applies to bare hosts.
type simpleJar struct {
	base    string
	cookies []*http.Cookie
}

func (j *simpleJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	j.cookies = append(j.cookies, cookies...)
}

func (j *simpleJar) Cookies(*url.URL) []*http.Cookie { return j.cookies }

// dialWS opens the review socket with the cookies the client collected.
func dialWS(t *testing.T, ts *httptest.Server, client *http.Client, origin string) *websocket.Conn {
	t.Helper()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	for _, c := range client.Jar.Cookies(nil) {
		header.Add("Cookie", c.Name+"="+c.Value)
	}

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial: %v (status %d)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// waitFor reads frames until one of the given type arrives.
func waitFor(t *testing.T, conn *websocket.Conn, frameType string) map[string]any {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %q: %v", frameType, err)
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		if frame["type"] == frameType {
			return frame
		}
	}
	t.Fatalf("timed out waiting for a %q frame", frameType)
	return nil
}

func send(t *testing.T, conn *websocket.Conn, frame map[string]any) {
	t.Helper()
	if err := conn.WriteJSON(frame); err != nil {
		t.Fatal(err)
	}
}

// TestReviewRoundTrip exercises the whole path: log in, open a document, file a
// comment, and confirm the edit reached disk and the branch.
func TestReviewRoundTrip(t *testing.T) {
	rev, root := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	client := login(t, ts, secret)
	conn := dialWS(t, ts, client, ts.URL)

	// The document list arrives unprompted so a reconnecting browser recovers.
	list := waitFor(t, conn, "docList")
	docs, _ := list["docs"].([]any)
	if len(docs) != 1 || docs[0] != "spec.md" {
		t.Fatalf("docList = %v, want [spec.md]", list["docs"])
	}

	send(t, conn, map[string]any{"type": "openDoc", "path": "spec.md"})
	docFrame := waitFor(t, conn, "doc")
	doc, _ := docFrame["doc"].(map[string]any)
	if doc["path"] != "spec.md" {
		t.Fatalf("doc frame is for %v", doc["path"])
	}
	if _, ok := doc["root"].(map[string]any); !ok {
		t.Fatal("doc frame carries no tree")
	}

	send(t, conn, map[string]any{
		"type": "comment",
		"doc":  "spec.md",
		"anchor": map[string]any{
			"nodeId": "n-1",
			"quote":  "The system SHALL retry indefinitely until the operation succeeds.",
			"prefix": "",
			"suffix": "",
		},
		"body": "reword this",
	})

	end := waitFor(t, conn, "turnEnd")
	if edited, _ := end["edited"].(bool); !edited {
		t.Fatalf("turn reported no edit: %v", end)
	}
	if commit, _ := end["commit"].(string); commit == "" {
		t.Fatal("turn reported an edit but no commit")
	}

	// The edit must be on disk...
	updated, err := os.ReadFile(filepath.Join(root, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), "SHALL retry indefinitely") {
		t.Errorf("document was not edited:\n%s", updated)
	}
	if !strings.Contains(string(updated), "REWORDED") {
		t.Errorf("expected the stub's replacement in the file:\n%s", updated)
	}

	// ...and recorded on the task branch, with the thread id in the trailer.
	if branch := gitRun(t, root, "rev-parse", "--abbrev-ref", "HEAD"); branch != "review/test" {
		t.Errorf("on branch %q, want review/test", branch)
	}
	message := gitRun(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(message, "review: reword this") {
		t.Errorf("commit subject not derived from the comment:\n%s", message)
	}
	if !strings.Contains(message, "Review-Thread:") {
		t.Errorf("commit is missing the thread trailer:\n%s", message)
	}
}

// A question should be answered without touching the document, which is what
// keeps "why this?" from silently rewriting a spec.
func TestQuestionDoesNotEdit(t *testing.T) {
	rev, root := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	before, err := os.ReadFile(filepath.Join(root, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	waitFor(t, conn, "docList")

	send(t, conn, map[string]any{
		"type": "comment",
		"doc":  "spec.md",
		"anchor": map[string]any{
			"quote": "The system SHALL retry indefinitely until the operation succeeds.",
		},
		"body": "why this?",
	})

	end := waitFor(t, conn, "turnEnd")
	if edited, _ := end["edited"].(bool); edited {
		t.Error("a question should not have produced an edit")
	}

	after, err := os.ReadFile(filepath.Join(root, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("document changed in response to a question")
	}
}

func TestUnauthenticatedSocketIsRefused(t *testing.T) {
	rev, _ := newReview(t)

	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth("secret")}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	header := http.Header{"Origin": {ts.URL}}
	if _, resp, err := websocket.DefaultDialer.Dial(wsURL, header); err == nil {
		t.Fatal("socket accepted a request with no session")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v (%v)", resp, err)
	}
}

// Cross-site WebSocket hijacking is the one attack this daemon is genuinely
// shaped for: the browser attaches the session cookie to a cross-origin
// upgrade, so the Origin check is what stands between a visited page and the
// reviewer's documents.
func TestForeignOriginIsRefused(t *testing.T) {
	rev, _ := newReview(t)

	const secret = "secret"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	client := login(t, ts, secret)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	for _, origin := range []string{"http://evil.example", ""} {
		header := http.Header{}
		if origin != "" {
			header.Set("Origin", origin)
		}
		for _, c := range client.Jar.Cookies(nil) {
			header.Add("Cookie", c.Name+"="+c.Value)
		}

		if _, resp, err := websocket.DefaultDialer.Dial(wsURL, header); err == nil {
			t.Errorf("socket accepted origin %q", origin)
		} else if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("origin %q: want 403, got %v (%v)", origin, resp, err)
		}
	}
}

func TestBadPasswordIsRejected(t *testing.T) {
	rev, _ := newReview(t)

	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth("correct")}))
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.PostForm(ts.URL+"/login", url.Values{"password": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want the login page back, got %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("a failed login issued a session cookie")
		}
	}
}

func TestPathsOutsideRootAreRefused(t *testing.T) {
	rev, _ := newReview(t)

	const secret = "secret"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	waitFor(t, conn, "docList")

	send(t, conn, map[string]any{"type": "openDoc", "path": "../../etc/passwd"})
	frame := waitFor(t, conn, "error")
	if msg, _ := frame["message"].(string); !strings.Contains(msg, "outside the review root") {
		t.Errorf("error was %q", msg)
	}
}

// The browser cannot read a rejected WebSocket handshake's status, so it asks
// /session instead. Getting this wrong leaves a page that retries forever while
// every click silently does nothing.
func TestSessionProbeReportsAuthState(t *testing.T) {
	rev, _ := newReview(t)

	const secret = "secret"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	// Without a session: 401, and not a redirect to the login page, which the
	// client's fetch would follow and mistake for success.
	bare := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := bare.Get(ts.URL + "/session")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /session = %d, want 401", resp.StatusCode)
	}

	// With one: 204.
	client := login(t, ts, secret)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/session", nil)
	for _, c := range client.Jar.Cookies(nil) {
		req.AddCookie(c)
	}
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Errorf("authenticated /session = %d, want 204", resp2.StatusCode)
	}
}

// A restart forgets every session, which is what made a still-open page retry
// against a cookie that could never work again.
func TestSessionsDoNotSurviveANewAuth(t *testing.T) {
	rev, _ := newReview(t)

	const secret = "secret"
	first := NewTokenAuth(secret)
	ts := httptest.NewServer(New(Options{Review: rev, Auth: first}))
	defer ts.Close()

	client := login(t, ts, secret)
	cookies := client.Jar.Cookies(nil)
	if len(cookies) == 0 {
		t.Fatal("login issued no cookie")
	}

	// A fresh Auth stands in for the daemon coming back up.
	replacement := NewTokenAuth(secret)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/session", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	if replacement.Authenticated(req) {
		t.Error("a cookie from the previous process was accepted after restart")
	}
}
