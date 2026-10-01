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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	return newReviewWith(t, nil)
}

// newReviewWith is newReview with a chance to adjust the options first.
func newReviewWith(t *testing.T, adjust func(*review.Options)) (*review.Review, string) {
	t.Helper()

	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	docPath := filepath.Join(root, "spec.md")
	require.NoError(t, os.WriteFile(docPath, []byte(testDoc), 0o644))
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-qm", "initial")

	stub, err := filepath.Abs("testdata/fake-claude")
	require.NoError(t, err)

	opts := review.Options{
		Root:         root,
		Branch:       "review/test",
		ClaudeBinary: stub,
	}
	if adjust != nil {
		adjust(&opts)
	}
	rev, err := review.New(opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rev.Close() })

	return rev, root
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// login performs the password exchange and returns a client holding the session
// cookie.
func login(t *testing.T, ts *httptest.Server, secret string) *http.Client {
	t.Helper()

	jar, err := newJar(ts.URL)
	require.NoError(t, err)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.PostForm(ts.URL+"/login", url.Values{"password": {secret}})
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusSeeOther, resp.StatusCode, "login should redirect")
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
		require.NoError(t, err, "websocket dial (status %d)", status)
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
		require.NoError(t, err, "waiting for %q", frameType)
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		if frame["type"] == frameType {
			return frame
		}
	}
	require.FailNow(t, "timed out waiting for a frame", "type %q", frameType)
	return nil
}

func send(t *testing.T, conn *websocket.Conn, frame map[string]any) {
	t.Helper()
	require.NoError(t, conn.WriteJSON(frame))
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
	require.Equal(t, []any{"spec.md"}, list["docs"])

	send(t, conn, map[string]any{"type": "openDoc", "path": "spec.md"})
	docFrame := waitFor(t, conn, "doc")
	doc, _ := docFrame["doc"].(map[string]any)
	require.Equal(t, "spec.md", doc["path"])
	require.IsType(t, map[string]any{}, doc["root"], "doc frame carries no tree")

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
	require.Equal(t, true, end["edited"], "turn reported no edit: %v", end)
	require.NotEmpty(t, end["commit"], "turn reported an edit but no commit")

	// The edit must be on disk...
	updated, err := os.ReadFile(filepath.Join(root, "spec.md"))
	require.NoError(t, err)
	assert.NotContains(t, string(updated), "SHALL retry indefinitely", "document was not edited")
	assert.Contains(t, string(updated), "REWORDED", "expected the stub's replacement in the file")

	// ...and recorded on the task branch, with the thread id in the trailer.
	assert.Equal(t, "review/test", gitRun(t, root, "rev-parse", "--abbrev-ref", "HEAD"))
	message := gitRun(t, root, "log", "-1", "--format=%B")
	assert.Contains(t, message, "review: reword this", "commit subject not derived from the comment")
	assert.Contains(t, message, "Review-Thread:", "commit is missing the thread trailer")
}

// A question should be answered without touching the document, which is what
// keeps "why this?" from silently rewriting a spec.
func TestQuestionDoesNotEdit(t *testing.T) {
	rev, root := newReview(t)

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	before, err := os.ReadFile(filepath.Join(root, "spec.md"))
	require.NoError(t, err)

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
	edited, _ := end["edited"].(bool)
	assert.False(t, edited, "a question should not have produced an edit")

	after, err := os.ReadFile(filepath.Join(root, "spec.md"))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "document changed in response to a question")
}

func TestUnauthenticatedSocketIsRefused(t *testing.T) {
	rev, _ := newReview(t)

	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth("secret")}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	header := http.Header{"Origin": {ts.URL}}
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	require.Error(t, err, "socket accepted a request with no session")
	require.NotNil(t, resp, "want 401, got no response (%v)", err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
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

		_, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
		if !assert.Error(t, err, "socket accepted origin %q", origin) {
			continue
		}
		if assert.NotNil(t, resp, "origin %q: want 403, got no response (%v)", origin, err) {
			assert.Equal(t, http.StatusForbidden, resp.StatusCode, "origin %q", origin)
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
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode, "want the login page back")
	for _, c := range resp.Cookies() {
		require.False(t, c.Name == sessionCookie && c.Value != "", "a failed login issued a session cookie")
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
	assert.Contains(t, frame["message"], "outside the review root")
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
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "unauthenticated /session")

	// With one: 204.
	client := login(t, ts, secret)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/session", nil)
	for _, c := range client.Jar.Cookies(nil) {
		req.AddCookie(c)
	}
	resp2, err := client.Do(req)
	require.NoError(t, err)
	resp2.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp2.StatusCode, "authenticated /session")
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
	require.NotEmpty(t, cookies, "login issued no cookie")

	// A fresh Auth stands in for the daemon coming back up.
	replacement := NewTokenAuth(secret)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/session", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	assert.False(t, replacement.Authenticated(req), "a cookie from the previous process was accepted after restart")
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
	assert.Fail(t, "no thread frame matched the comment that was sent", "%v", frame["threads"])
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
	assert.Contains(t, frame["message"], "selected passage", "want the error to name the missing passage")
}

// A state file the daemon could not read is the one thing a reviewer must not
// miss: their threads are not on screen, and the only clue is this notice.
// Terminal output is easy to have scrolled past, so it goes to the browser too.
func TestStateNoticeReachesTheBrowser(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".ai-reviewer"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".ai-reviewer", "state.json"), []byte(`{"threads":[ truncated`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "spec.md"), []byte(testDoc), 0o644))

	rev, err := review.New(review.Options{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rev.Close() })

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	conn := dialWS(t, ts, login(t, ts, secret), ts.URL)

	frame := waitFor(t, conn, "error")
	message, _ := frame["message"].(string)
	assert.Contains(t, message, "state.json", "notice does not say what happened to the file")
	assert.Contains(t, message, "kept as", "notice does not say what happened to the file")
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
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	require.NoError(t, os.MkdirAll(filepath.Join(repo, "doc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "doc", "spec.md"), []byte(testDoc), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("# repository rules\n"), 0o644))
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-qm", "initial")

	stub, err := filepath.Abs("testdata/fake-claude")
	require.NoError(t, err)
	rev, err := review.New(review.Options{
		Root:         filepath.Join(repo, "doc"),
		Branch:       "review/test",
		ClaudeBinary: stub,
	})
	require.NoError(t, err)
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

	assert.Equal(t, repo, rev.WorkRoot(), "Claude should run at the repository root")
	assert.Equal(t, filepath.Join(repo, "doc"), rev.Root(), "review root")

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
	require.NotEmpty(t, end["commit"], "the edit was not committed: %v", end)

	updated, err := os.ReadFile(filepath.Join(repo, "doc", "spec.md"))
	require.NoError(t, err)
	assert.Contains(t, string(updated), "REWORDED", "document was not edited")

	// The commit must name the file the way the repository does.
	assert.Equal(t, "doc/spec.md", gitRun(t, repo, "show", "--name-only", "--format=", "HEAD"))
	message := gitRun(t, repo, "log", "-1", "--format=%B")
	assert.Contains(t, message, "Document: doc/spec.md", "commit body does not name the document as the repository sees it")
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
	require.NotNil(t, settings, "settings frame carries nothing: %v", frame)

	assert.Equal(t, "acceptEdits", settings["permissionMode"])
	tools, _ := settings["tools"].([]any)
	if assert.NotEmpty(t, tools) {
		assert.Equal(t, "Read", tools[0], "tools = %v", tools)
	}
	assert.Equal(t, rev.WorkRoot(), settings["workspace"])
	choices, _ := settings["modelChoices"].([]any)
	assert.GreaterOrEqual(t, len(choices), 2, "modelChoices = %v", settings["modelChoices"])
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
	assert.Fail(t, "no settings frame reported the new model")
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
	assert.Contains(t, frame["message"], "gpt-4", "want the error to name the model")
	assert.Empty(t, rev.Settings().Model, "model changed despite being refused")
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
	require.Equal(t, quote, source, "want the passage as the file holds it")
	assert.Equal(t, quote, frame["quote"], "the source frame does not name the passage it answers")

	const replacement = "retry up to five times before"
	send(t, conn, map[string]any{
		"type":        "applyEdit",
		"doc":         "spec.md",
		"anchor":      anchor,
		"original":    source,
		"replacement": replacement,
	})

	applied := waitFor(t, conn, "editApplied")
	assert.NotEmpty(t, applied["commit"], "editApplied carried no commit: %v", applied)

	after, err := os.ReadFile(filepath.Join(root, "spec.md"))
	require.NoError(t, err)
	assert.Contains(t, string(after), replacement, "the document does not carry the edit")

	// A hand edit is separable from a turn in the history: nobody's reasoning
	// is attached to it, and the trailer says so.
	message := gitRun(t, root, "log", "-1", "--format=%B")
	assert.Contains(t, message, "Review-Edit: hand", "commit message does not mark the edit as the reviewer's")
	assert.Contains(t, message, "hand edit of", "commit subject does not name the passage")
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
	assert.Contains(t, frame["message"], "changed while you were editing", "want the error to say the passage moved on")

	after, _ := os.ReadFile(filepath.Join(root, "spec.md"))
	assert.Equal(t, testDoc, string(after), "the refused edit reached the file")
}

// The whole reason the file route exists: a document embeds an image, and until
// this the browser had nowhere to fetch it from.
func TestEmbeddedImagesAreServed(t *testing.T) {
	rev, root := newReview(t)

	const png = "\x89PNG\r\n\x1a\nnot really"
	require.NoError(t, os.WriteFile(filepath.Join(root, "flow.png"), []byte(png), 0o644))

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	client := login(t, ts, secret)
	resp, err := client.Get(ts.URL + "/file/flow.png")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /file/flow.png")
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, png, string(body), "want the file's bytes")
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "sandbox", resp.Header.Get("Content-Security-Policy"))
}

func TestTheFileRouteRefusesWhatIsNotUnderTheRoot(t *testing.T) {
	rev, root := newReview(t)

	outside := filepath.Join(filepath.Dir(root), "outside.png")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))
	t.Cleanup(func() { _ = os.Remove(outside) })

	const secret = "test-secret-phrase"
	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth(secret)}))
	defer ts.Close()

	// Redirects are followed here: a climb out of the root is cleaned away by
	// the mux into a redirect, and what matters is where that lands.
	client := &http.Client{Jar: login(t, ts, secret).Jar}

	for _, path := range []string{"/file/../outside.png", "/file/.git/config", "/file/"} {
		resp, err := client.Get(ts.URL + path)
		require.NoError(t, err, "GET %s", path)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "GET %s", path)
		assert.NotContains(t, string(body), "secret", "GET %s returned the file outside the root", path)
	}
}

func TestTheFileRouteNeedsASession(t *testing.T) {
	rev, root := newReview(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "flow.png"), []byte("png"), 0o644))

	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth("test-secret-phrase")}))
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + "/file/flow.png")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "an unauthenticated fetch should redirect to the login page")
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
	require.NotEmpty(t, threadID, "no thread came back for the comment")

	send(t, conn, map[string]any{"type": "clearContext"})
	waitFor(t, conn, "settings")

	// The record of the review is the daemon's, not the process's. Nothing is
	// republished by the clear, because nothing about the review changed —
	// so ask for the threads the way a browser does.
	send(t, conn, map[string]any{"type": "openDoc", "path": "spec.md"})
	frame := waitFor(t, conn, "threads")
	threads, _ := frame["threads"].([]any)
	assert.Len(t, threads, 1, "threads after clearing context: want the one that was filed")

	send(t, conn, map[string]any{"type": "reply", "threadId": threadID, "body": "reword this"})
	end := waitFor(t, conn, "turnEnd")
	require.Equal(t, true, end["edited"], "a reply after clearing the context did not reach the document: %v", end)

	after, err := os.ReadFile(filepath.Join(root, "spec.md"))
	require.NoError(t, err)
	assert.Contains(t, string(after), "REWORDED", "the reply did not carry the passage it was about")
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
	assert.Equal(t, "review/test", settings["branch"])
	assert.Equal(t, "main", settings["baseBranch"], "want the branch the review was cut from")
	assert.EqualValues(t, 0, settings["commits"], "want none before anything is recorded")
	assert.Empty(t, landingField(settings, "merge"), "want nothing to merge")

	// One turn, one commit.
	send(t, conn, map[string]any{
		"type":   "comment",
		"doc":    "spec.md",
		"anchor": map[string]any{"nodeId": "n-1", "quote": "The system SHALL retry indefinitely until the operation succeeds."},
		"body":   "reword this",
	})
	waitFor(t, conn, "turnEnd")

	settings = settingsFrom(t, waitFor(t, conn, "settings"))
	assert.EqualValues(t, 1, settings["commits"], "want the one the turn recorded")
	assert.Equal(t, "git switch main && git merge review/test", landingField(settings, "merge"))
	assert.Equal(t, "git switch main && git merge --squash review/test && git commit", landingField(settings, "squash"))
	assert.Equal(t, "git branch -d review/test", landingField(settings, "delete"))
	// The squash needs the capital -D: git cannot see one commit as the
	// commits it was squashed from, so it refuses the lowercase one.
	assert.Equal(t, "git branch -D review/test", landingField(settings, "squashDelete"))

	// What the reviewer does with that command, in their own terminal.
	gitRun(t, root, "switch", "main")
	gitRun(t, root, "merge", "review/test")
	gitRun(t, root, "switch", "review/test")

	send(t, conn, map[string]any{"type": "setModel", "model": "sonnet"})
	settings = settingsFrom(t, waitFor(t, conn, "settings"))
	assert.EqualValues(t, 0, settings["commits"], "after the branch was merged, want none")
	assert.Empty(t, landingField(settings, "merge"), "after the branch was merged, want nothing to merge")
}

// landingField reads one of the commands the panel offers out of a settings
// frame, where an absent landing block reads the same as an empty command.
func landingField(settings map[string]any, name string) string {
	landing, _ := settings["landing"].(map[string]any)
	value, _ := landing[name].(string)
	return value
}

// The tree is on the review branch by the time a restarted daemon can look, so
// the branch it was cut from has to survive in the state file.
func TestTheBaseBranchSurvivesARestart(t *testing.T) {
	rev, root := newReview(t)
	require.Equal(t, "main", rev.Settings().BaseBranch)
	require.NoError(t, rev.Close())

	stub, err := filepath.Abs("testdata/fake-claude")
	require.NoError(t, err)
	restarted, err := review.New(review.Options{Root: root, Branch: "review/test", ClaudeBinary: stub})
	require.NoError(t, err)
	t.Cleanup(func() { _ = restarted.Close() })

	assert.Equal(t, "main", restarted.Settings().BaseBranch, "after a restart")
}

func settingsFrom(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	settings, ok := frame["settings"].(map[string]any)
	require.True(t, ok, "settings frame carries no settings: %v", frame)
	return settings
}

// The icon is the one asset served without a session. A browser asks for it on
// the login page as well, where a redirect to the login page is not an icon.
func TestFaviconIsServedWithoutASession(t *testing.T) {
	rev, _ := newReview(t)

	ts := httptest.NewServer(New(Options{Review: rev, Auth: NewTokenAuth("test-secret-phrase")}))
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + "/favicon.ico")
	require.NoError(t, err)
	defer resp.Body.Close()

	// 404 when the build carries no icon, 200 when it does — never a redirect
	// to the login page, which is what the browser would draw as the icon.
	switch resp.StatusCode {
	case http.StatusOK:
		assert.Equal(t, "image/x-icon", resp.Header.Get("Content-Type"))
	case http.StatusNotFound:
	default:
		assert.Fail(t, "GET /favicon.ico wants 200 or 404", "got %d", resp.StatusCode)
	}
}
