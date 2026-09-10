package server

import (
	"encoding/json"
	"io"
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

// TestFiledCommentComesBackInThreads pins the acknowledgement the composer
// waits for. The browser keeps a comment on screen until it sees it in a
// `threads` frame, so an accepted comment must come back carrying both the
// quote it was anchored to and the reviewer's own words.
func TestFiledCommentComesBackInThreads(t *testing.T) {
	rev, _ := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	send(t, conn, map[string]any{"type": "openDoc", "path": "spec.md"})
	waitFor(t, conn, "doc")

	const quote = "Unrelated paragraph."
	const body = "why is this here?"

	send(t, conn, map[string]any{
		"type":   "comment",
		"doc":    "spec.md",
		"anchor": map[string]any{"nodeId": "n-2", "quote": quote, "prefix": "", "suffix": ""},
		"body":   body,
	})

	// Opening a document broadcasts the threads it already has, so the frame
	// that matters is the first one carrying this comment — exactly the test
	// the browser applies before it closes the composer.
	var frame map[string]any
	for range 5 {
		frame = waitFor(t, conn, "threads")
		if threadCarries(frame, quote, body) {
			return
		}
	}
	t.Errorf("no thread frame matched the comment that was sent: %v", frame["threads"])
}

// threadCarries mirrors the match the client makes: same anchor quote, and a
// user message holding the text that was typed.
func threadCarries(frame map[string]any, quote, body string) bool {
	threads, _ := frame["threads"].([]any)
	for _, entry := range threads {
		thread, _ := entry.(map[string]any)
		anchor, _ := thread["anchor"].(map[string]any)
		if anchor["quote"] != quote {
			continue
		}
		messages, _ := thread["messages"].([]any)
		for _, m := range messages {
			message, _ := m.(map[string]any)
			if message["role"] == "user" && message["text"] == body {
				return true
			}
		}
	}
	return false
}

// A comment on a passage that is no longer in the file must be refused with an
// error rather than accepted, since the browser reads that error as "your words
// are still yours to edit and send again".
func TestCommentOnAVanishedPassageIsRefused(t *testing.T) {
	rev, _ := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	send(t, conn, map[string]any{"type": "openDoc", "path": "spec.md"})
	waitFor(t, conn, "doc")

	send(t, conn, map[string]any{
		"type": "comment",
		"doc":  "spec.md",
		"anchor": map[string]any{
			"nodeId": "n-1",
			"quote":  "a sentence that was edited away while the reviewer typed",
			"prefix": "",
			"suffix": "",
		},
		"body": "reword this",
	})

	frame := waitFor(t, conn, "error")
	if message, _ := frame["message"].(string); !strings.Contains(message, "selected passage") {
		t.Errorf("error frame = %v, want it to name the missing passage", frame)
	}
}

// A state file the daemon could not read is the one thing a reviewer must not
// miss: their threads are not on screen, and the only clue is this notice.
// Terminal output is easy to have scrolled past, so it goes to the browser too.
func TestStateNoticeReachesTheBrowser(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".ai-reviewer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".ai-reviewer", "state.json"), []byte(`{"threads":[ truncated`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "spec.md"), []byte(testDoc), 0o644); err != nil {
		t.Fatal(err)
	}

	rev, err := review.New(review.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rev.Close() })

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)

	frame := waitFor(t, conn, "error")
	message, _ := frame["message"].(string)
	if !strings.Contains(message, "state.json") || !strings.Contains(message, "kept as") {
		t.Errorf("notice does not say what happened to the file: %q", message)
	}
}

// newReviewInSubdir sets up the ordinary shape: a repository whose documents
// live in a subdirectory, with the repository's own CLAUDE.md above them.
func newReviewInSubdir(t *testing.T) (*review.Review, string) {
	t.Helper()

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if err := os.MkdirAll(filepath.Join(repo, "doc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "doc", "spec.md"), []byte(testDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("# repository rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-qm", "initial")

	stub, err := filepath.Abs("testdata/fake-claude")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := review.New(review.Options{
		Root:         filepath.Join(repo, "doc"),
		Branch:       "review/test",
		ClaudeBinary: stub,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rev.Close() })

	return rev, repo
}

// TestReviewOfASubdirectoryStillCommits is the shape most repositories have:
// documents in doc/ or docs/, the repository root above them. Claude runs at the
// repository root — that directory is its file-permission boundary, so a doc
// that references ../src is answerable — and every path it is given or reports
// is relative to the same place, which is what git needs to stage them.
//
// Getting that wrong is quiet in the worst way: the edit lands on disk and the
// commit meant to record it never happens.
func TestReviewOfASubdirectoryStillCommits(t *testing.T) {
	rev, repo := newReviewInSubdir(t)

	if rev.WorkRoot() != repo {
		t.Errorf("Claude would run in %q, want the repository root %q", rev.WorkRoot(), repo)
	}
	if rev.Root() != filepath.Join(repo, "doc") {
		t.Errorf("review root = %q", rev.Root())
	}

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)

	// The browser addresses documents relative to the review root, as always.
	send(t, conn, map[string]any{"type": "openDoc", "path": "spec.md"})
	waitFor(t, conn, "doc")

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
	if commit, _ := end["commit"].(string); commit == "" {
		t.Fatalf("the edit was not committed: %v", end)
	}

	updated, err := os.ReadFile(filepath.Join(repo, "doc", "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "REWORDED") {
		t.Errorf("document was not edited:\n%s", updated)
	}

	// The commit must name the file the way the repository does.
	staged := gitRun(t, repo, "show", "--name-only", "--format=", "HEAD")
	if staged != "doc/spec.md" {
		t.Errorf("commit touched %q, want doc/spec.md", staged)
	}
	message := gitRun(t, repo, "log", "-1", "--format=%B")
	if !strings.Contains(message, "Document: doc/spec.md") {
		t.Errorf("commit body does not name the document as the repository sees it:\n%s", message)
	}
}

// The settings the reviewer can see arrive unprompted, so opening the page is
// enough to answer "which model am I talking to?".
func TestSettingsArriveOnConnect(t *testing.T) {
	rev, _ := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)

	frame := waitFor(t, conn, "settings")
	settings, _ := frame["settings"].(map[string]any)
	if settings == nil {
		t.Fatalf("settings frame carries nothing: %v", frame)
	}

	if mode, _ := settings["permissionMode"].(string); mode != "acceptEdits" {
		t.Errorf("permissionMode = %v", settings["permissionMode"])
	}
	tools, _ := settings["tools"].([]any)
	if len(tools) == 0 || tools[0] != "Read" {
		t.Errorf("tools = %v", settings["tools"])
	}
	if ws, _ := settings["workspace"].(string); ws != rev.WorkRoot() {
		t.Errorf("workspace = %v, want %q", settings["workspace"], rev.WorkRoot())
	}
	choices, _ := settings["modelChoices"].([]any)
	if len(choices) < 2 {
		t.Errorf("modelChoices = %v", settings["modelChoices"])
	}
}

// Choosing a model has to come back as a new settings frame: the panel shows
// what the daemon confirms, not what the select element was set to.
func TestChangingTheModelIsConfirmed(t *testing.T) {
	rev, _ := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	waitFor(t, conn, "settings")

	send(t, conn, map[string]any{"type": "setModel", "model": "sonnet"})

	for range 5 {
		frame := waitFor(t, conn, "settings")
		settings, _ := frame["settings"].(map[string]any)
		if model, _ := settings["model"].(string); model == "sonnet" {
			return
		}
	}
	t.Error("no settings frame reported the new model")
}

// A model nobody offers is refused rather than passed through to a launch that
// would fail later, inside a turn the reviewer is waiting on.
func TestAnUnknownModelIsRefused(t *testing.T) {
	rev, _ := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	waitFor(t, conn, "settings")

	send(t, conn, map[string]any{"type": "setModel", "model": "gpt-4"})

	frame := waitFor(t, conn, "error")
	if message, _ := frame["message"].(string); !strings.Contains(message, "gpt-4") {
		t.Errorf("error = %v, want it to name the model", frame)
	}
	if got := rev.Settings().Model; got != "" {
		t.Errorf("model changed to %q despite being refused", got)
	}
}

// A reviewer who already has the words they want should not have to spend a
// turn on them. The whole path — ask for the source, send back a replacement —
// runs over the same socket and lands as a commit of its own.
func TestHandEditWritesAndCommits(t *testing.T) {
	rev, root := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	send(t, conn, map[string]any{"type": "openDoc", "path": "spec.md"})
	waitFor(t, conn, "doc")

	const quote = "retry indefinitely until"
	anchor := map[string]any{"nodeId": "n-2", "quote": quote, "prefix": "", "suffix": ""}

	send(t, conn, map[string]any{"type": "editSource", "doc": "spec.md", "anchor": anchor})
	frame := waitFor(t, conn, "editSource")
	source, _ := frame["text"].(string)
	if source != quote {
		t.Fatalf("source = %q, want the passage as the file holds it", source)
	}
	if frame["quote"] != quote {
		t.Errorf("the source frame does not name the passage it answers: %v", frame)
	}

	const replacement = "retry up to five times before"
	send(t, conn, map[string]any{
		"type":        "applyEdit",
		"doc":         "spec.md",
		"anchor":      anchor,
		"original":    source,
		"replacement": replacement,
	})

	applied := waitFor(t, conn, "editApplied")
	if commit, _ := applied["commit"].(string); commit == "" {
		t.Errorf("editApplied carried no commit: %v", applied)
	}

	after, err := os.ReadFile(filepath.Join(root, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), replacement) {
		t.Errorf("the document does not carry the edit:\n%s", after)
	}

	// A hand edit is separable from a turn in the history: nobody's reasoning
	// is attached to it, and the trailer says so.
	message := gitRun(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(message, "Review-Edit: hand") {
		t.Errorf("commit message does not mark the edit as the reviewer's:\n%s", message)
	}
	if !strings.Contains(message, "hand edit of") {
		t.Errorf("commit subject does not name the passage:\n%s", message)
	}
}

// The compare-and-swap over the socket: an editor opened on text that has since
// moved on is refused, and the reviewer keeps their words to try again.
func TestHandEditOnStaleSourceIsRefused(t *testing.T) {
	rev, root := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	send(t, conn, map[string]any{"type": "openDoc", "path": "spec.md"})
	waitFor(t, conn, "doc")

	send(t, conn, map[string]any{
		"type":   "applyEdit",
		"doc":    "spec.md",
		"anchor": map[string]any{"nodeId": "n-2", "quote": "retry indefinitely until"},
		// What the file actually says is "retry indefinitely until".
		"original":    "retry for a while until",
		"replacement": "retry twice until",
	})

	frame := waitFor(t, conn, "error")
	if message, _ := frame["message"].(string); !strings.Contains(message, "changed while you were editing") {
		t.Errorf("error frame = %v, want it to say the passage moved on", frame)
	}

	after, _ := os.ReadFile(filepath.Join(root, "spec.md"))
	if string(after) != testDoc {
		t.Errorf("the refused edit reached the file:\n%s", after)
	}
}

// The whole reason the file route exists: a document embeds an image, and until
// this the browser had nowhere to fetch it from.
func TestEmbeddedImagesAreServed(t *testing.T) {
	rev, root := newReview(t)

	const png = "\x89PNG\r\n\x1a\nnot really"
	if err := os.WriteFile(filepath.Join(root, "flow.png"), []byte(png), 0o644); err != nil {
		t.Fatal(err)
	}

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	client := login(t, ts, secret)
	resp, err := client.Get(ts.URL + "/file/flow.png")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /file/flow.png = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != png {
		t.Errorf("body = %q, want the file's bytes", body)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != "sandbox" {
		t.Errorf("Content-Security-Policy = %q, want sandbox", got)
	}
}

func TestTheFileRouteRefusesWhatIsNotUnderTheRoot(t *testing.T) {
	rev, root := newReview(t)

	outside := filepath.Join(filepath.Dir(root), "outside.png")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	// Redirects are followed here: a climb out of the root is cleaned away by
	// the mux into a redirect, and what matters is where that lands.
	client := &http.Client{Jar: login(t, ts, secret).Jar}

	for _, path := range []string{"/file/../outside.png", "/file/.git/config", "/file/"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
		if strings.Contains(string(body), "secret") {
			t.Errorf("GET %s returned the file outside the root", path)
		}
	}
}

func TestTheFileRouteNeedsASession(t *testing.T) {
	rev, root := newReview(t)
	if err := os.WriteFile(filepath.Join(root, "flow.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth("test-secret-phrase")}))
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + "/file/flow.png")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("an unauthenticated fetch returned %d, want a redirect to the login page", resp.StatusCode)
	}
}

// Clearing the context drops what the model is carrying without touching what
// the review has recorded, and a reply afterwards still knows which passage it
// is about — the conversation that used to carry that is gone.
//
// The stub only edits when the prompt names the file and quotes the passage, so
// a reply that lands on the document is proof that the passage was re-stated.
func TestClearContextKeepsThreadsAndStillAnswers(t *testing.T) {
	rev, root := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)
	waitFor(t, conn, "docList")

	const quote = "The system SHALL retry indefinitely until the operation succeeds."
	send(t, conn, map[string]any{
		"type":   "comment",
		"doc":    "spec.md",
		"anchor": map[string]any{"nodeId": "n-1", "quote": quote},
		"body":   "why this?",
	})
	waitFor(t, conn, "turnEnd")

	threadID := ""
	for range 5 {
		frame := waitFor(t, conn, "threads")
		if threads, _ := frame["threads"].([]any); len(threads) > 0 {
			thread, _ := threads[0].(map[string]any)
			threadID, _ = thread["id"].(string)
			break
		}
	}
	if threadID == "" {
		t.Fatal("no thread came back for the comment")
	}

	send(t, conn, map[string]any{"type": "clearContext"})
	waitFor(t, conn, "settings")

	// The record of the review is the daemon's, not the process's. Nothing is
	// republished by the clear, because nothing about the review changed —
	// so ask for the threads the way a browser does.
	send(t, conn, map[string]any{"type": "openDoc", "path": "spec.md"})
	frame := waitFor(t, conn, "threads")
	if threads, _ := frame["threads"].([]any); len(threads) != 1 {
		t.Errorf("threads after clearing context = %v, want the one that was filed", frame["threads"])
	}

	send(t, conn, map[string]any{"type": "reply", "threadId": threadID, "body": "reword this"})
	end := waitFor(t, conn, "turnEnd")
	if edited, _ := end["edited"].(bool); !edited {
		t.Fatalf("a reply after clearing the context did not reach the document: %v", end)
	}

	after, err := os.ReadFile(filepath.Join(root, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "REWORDED") {
		t.Errorf("the reply did not carry the passage it was about:\n%s", after)
	}
}

// The panel has to be able to say where the review's commits are and how to
// land them, and the count has to fall to zero once they have been landed —
// including by a merge run from a terminal, which the daemon never hears about.
func TestSettingsReportTheBranchAndHowToMergeIt(t *testing.T) {
	rev, root := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)

	settings := settingsFrom(t, waitFor(t, conn, "settings"))
	if settings["branch"] != "review/test" {
		t.Errorf("branch = %v, want review/test", settings["branch"])
	}
	if settings["baseBranch"] != "main" {
		t.Errorf("baseBranch = %v, want the branch the review was cut from", settings["baseBranch"])
	}
	if commits, _ := settings["commits"].(float64); commits != 0 {
		t.Errorf("commits = %v, want none before anything is recorded", settings["commits"])
	}
	if command, _ := settings["mergeCommand"].(string); command != "" {
		t.Errorf("mergeCommand = %q, want nothing to merge", command)
	}

	// One turn, one commit.
	send(t, conn, map[string]any{
		"type":   "comment",
		"doc":    "spec.md",
		"anchor": map[string]any{"nodeId": "n-1", "quote": "The system SHALL retry indefinitely until the operation succeeds."},
		"body":   "reword this",
	})
	waitFor(t, conn, "turnEnd")

	settings = settingsFrom(t, waitFor(t, conn, "settings"))
	if commits, _ := settings["commits"].(float64); commits != 1 {
		t.Errorf("commits = %v, want the one the turn recorded", settings["commits"])
	}
	if command, _ := settings["mergeCommand"].(string); command != "git switch main && git merge review/test" {
		t.Errorf("mergeCommand = %q", command)
	}

	// What the reviewer does with that command, in their own terminal.
	gitRun(t, root, "switch", "main")
	gitRun(t, root, "merge", "review/test")
	gitRun(t, root, "switch", "review/test")

	send(t, conn, map[string]any{"type": "setModel", "model": "sonnet"})
	settings = settingsFrom(t, waitFor(t, conn, "settings"))
	if commits, _ := settings["commits"].(float64); commits != 0 {
		t.Errorf("commits = %v after the branch was merged, want none", settings["commits"])
	}
	if command, _ := settings["mergeCommand"].(string); command != "" {
		t.Errorf("mergeCommand = %q after the branch was merged, want nothing to merge", command)
	}
}

// The tree is on the review branch by the time a restarted daemon can look, so
// the branch it was cut from has to survive in the state file.
func TestTheBaseBranchSurvivesARestart(t *testing.T) {
	rev, root := newReview(t)
	if base := rev.Settings().BaseBranch; base != "main" {
		t.Fatalf("baseBranch = %q, want main", base)
	}
	if err := rev.Close(); err != nil {
		t.Fatal(err)
	}

	stub, err := filepath.Abs("testdata/fake-claude")
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := review.New(review.Options{Root: root, Branch: "review/test", ClaudeBinary: stub})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })

	if base := restarted.Settings().BaseBranch; base != "main" {
		t.Errorf("baseBranch after a restart = %q, want main", base)
	}
}

func settingsFrom(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	settings, ok := frame["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings frame carries no settings: %v", frame)
	}
	return settings
}
