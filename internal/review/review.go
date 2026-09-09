// Package review is the daemon's core: it owns the documents under review, the
// comment threads on them, the Claude process behind each document, and the
// commit that records each turn.
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gilramir/ai-reviewer/internal/claudeproc"
	"github.com/gilramir/ai-reviewer/internal/gitstore"
	"github.com/gilramir/ai-reviewer/internal/mdast"
)

// Options configures a Review.
type Options struct {
	// Root is the directory holding the documents under review.
	Root string
	// Branch is the task branch commits land on. Empty leaves the current
	// branch alone, which is the right behaviour outside a repository.
	Branch string
	// Model, ClaudeBinary and MaxBudgetUSD are passed through to each process.
	Model        string
	ClaudeBinary string
	MaxBudgetUSD float64
	// IdleTimeout and MaxLive bound how many Claude processes stay resident.
	IdleTimeout time.Duration
	MaxLive     int
}

// Review is safe for concurrent use by every connected browser.
type Review struct {
	root   string
	branch string
	hist   gitstore.History
	procs  *claudeproc.Manager

	mu       sync.Mutex
	revs     map[string]int      // document path -> render revision
	sessions map[string]string   // document path -> Claude session id
	inflight map[string]int      // document path -> turns running
	threads  map[string]*Thread  // thread id -> thread
	order    []string            // thread ids, oldest first
	subs     map[int]chan []byte // subscriber id -> outbound frames
	nextSub  int
}

// New prepares a review of root. It does not start watching for file changes;
// call Watch for that.
func New(opts Options) (*Review, error) {
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("review root %q is not a directory", opts.Root)
	}

	hist, err := gitstore.Open(root)
	if err != nil {
		return nil, err
	}
	if opts.Branch != "" {
		if err := hist.EnsureBranch(opts.Branch); err != nil {
			return nil, err
		}
	}

	r := &Review{
		root:   root,
		branch: opts.Branch,
		hist:   hist,
		procs: claudeproc.NewManager(
			claudeproc.Config{
				Binary:       opts.ClaudeBinary,
				WorkDir:      root,
				Model:        opts.Model,
				SystemPrompt: systemPrompt,
				MaxBudgetUSD: opts.MaxBudgetUSD,
			},
			claudeproc.ManagerOptions{
				MaxLive:     opts.MaxLive,
				IdleTimeout: opts.IdleTimeout,
			},
		),
		revs:     map[string]int{},
		sessions: map[string]string{},
		inflight: map[string]int{},
		threads:  map[string]*Thread{},
		subs:     map[int]chan []byte{},
	}

	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// Close stops every Claude process and flushes state to disk.
func (r *Review) Close() error {
	r.procs.Close()
	return r.save()
}

// Root is the directory under review.
func (r *Review) Root() string { return r.root }

// --- documents --------------------------------------------------------------

// Docs lists the Markdown files under the review root, relative and sorted.
func (r *Review) Docs() ([]string, error) {
	var out []string
	err := filepath.Walk(r.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // an unreadable subtree should not hide the rest
		}
		if info.IsDir() {
			if skipDir(info.Name()) && path != r.root {
				return filepath.SkipDir
			}
			return nil
		}
		if !isMarkdown(info.Name()) {
			return nil
		}
		rel, err := filepath.Rel(r.root, path)
		if err != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}

func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".ai-reviewer", "gren_packages", ".gren":
		return true
	}
	return strings.HasPrefix(name, ".") && name != "."
}

func isMarkdown(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".md" || ext == ".markdown"
}

// Render parses a document and returns the tree the browser draws.
func (r *Review) Render(docPath string) (mdast.Document, error) {
	src, err := r.read(docPath)
	if err != nil {
		return mdast.Document{}, err
	}

	r.mu.Lock()
	r.revs[docPath]++
	rev := r.revs[docPath]
	r.mu.Unlock()

	return mdast.Render(docPath, rev, src), nil
}

func (r *Review) read(docPath string) ([]byte, error) {
	full, err := r.resolve(docPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

// resolve turns a client-supplied path into an absolute one, refusing anything
// that escapes the review root. Every path in a request is attacker-controlled
// once the daemon listens on a network interface.
func (r *Review) resolve(docPath string) (string, error) {
	if docPath == "" {
		return "", fmt.Errorf("empty document path")
	}
	clean := filepath.Clean(filepath.FromSlash(docPath))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("path %q is outside the review root", docPath)
	}
	full := filepath.Join(r.root, clean)
	rel, err := filepath.Rel(r.root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q is outside the review root", docPath)
	}
	if !isMarkdown(full) {
		return "", fmt.Errorf("%q is not a Markdown document", docPath)
	}
	return full, nil
}

// --- threads ----------------------------------------------------------------

// ThreadsFor returns the threads on a document, oldest first.
func (r *Review) ThreadsFor(docPath string) []*Thread {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.threadsForLocked(docPath)
}

func (r *Review) threadsForLocked(docPath string) []*Thread {
	out := make([]*Thread, 0, len(r.order))
	for _, id := range r.order {
		if t := r.threads[id]; t != nil && t.Doc == docPath {
			out = append(out, t.clone())
		}
	}
	return out
}

// Comment files a new thread and starts the turn that answers it.
func (r *Review) Comment(docPath string, anchor Anchor, body string) (*Thread, error) {
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("empty comment")
	}
	src, err := r.read(docPath)
	if err != nil {
		return nil, err
	}

	// Confirm the passage is really in the file before spending a turn on it.
	at, ok := Locate(string(src), anchor)
	if !ok {
		return nil, fmt.Errorf("could not find the selected passage in %s", docPath)
	}

	thread := &Thread{
		ID:       uuid.NewString(),
		Doc:      docPath,
		Anchor:   anchor,
		Status:   StatusThinking,
		Messages: []Message{{Role: RoleUser, Text: body}},
		Created:  time.Now().UTC(),
	}

	r.mu.Lock()
	r.threads[thread.ID] = thread
	r.order = append(r.order, thread.ID)
	r.mu.Unlock()

	prompt := commentPrompt(docPath, anchor.Quote, blockAround(string(src), at), body)
	go r.runTurn(thread.ID, docPath, prompt, body)

	r.broadcastThreads(docPath)
	return thread.clone(), nil
}

// Reply continues an existing thread.
func (r *Review) Reply(threadID, body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("empty reply")
	}

	r.mu.Lock()
	thread := r.threads[threadID]
	if thread == nil {
		r.mu.Unlock()
		return fmt.Errorf("no such thread")
	}
	docPath := thread.Doc
	thread.Messages = append(thread.Messages, Message{Role: RoleUser, Text: body})
	thread.Status = StatusThinking
	r.mu.Unlock()

	go r.runTurn(threadID, docPath, replyPrompt(body), body)

	r.broadcastThreads(docPath)
	return nil
}

// Resolve closes a thread without further comment.
func (r *Review) Resolve(threadID string) error {
	r.mu.Lock()
	thread := r.threads[threadID]
	if thread == nil {
		r.mu.Unlock()
		return fmt.Errorf("no such thread")
	}
	thread.Status = StatusResolved
	docPath := thread.Doc
	r.mu.Unlock()

	r.broadcastThreads(docPath)
	return r.save()
}

// Interrupt stops the turn running against a document.
func (r *Review) Interrupt(docPath string) {
	r.mu.Lock()
	sessionID := r.sessions[docPath]
	r.mu.Unlock()
	if sessionID == "" {
		return
	}
	r.procs.Session(docPath, sessionID).Interrupt()
}

// blockAround extracts the paragraph-sized neighbourhood of a location, giving
// the model enough surrounding text to edit the passage in place.
func blockAround(src string, at Location) string {
	start := strings.LastIndex(src[:at.Start], "\n\n")
	if start < 0 {
		start = 0
	} else {
		start += 2
	}
	end := strings.Index(src[at.End:], "\n\n")
	if end < 0 {
		end = len(src)
	} else {
		end += at.End
	}
	return strings.TrimSpace(src[start:end])
}

// --- turns ------------------------------------------------------------------

// runTurn drives one exchange and records whatever came of it.
func (r *Review) runTurn(threadID, docPath, prompt, subject string) {
	r.setBusy(docPath, 1)
	defer r.setBusy(docPath, -1)

	r.mu.Lock()
	sessionID := r.sessions[docPath]
	if sessionID == "" {
		sessionID = uuid.NewString()
		r.sessions[docPath] = sessionID
	}
	r.mu.Unlock()

	session := r.procs.Session(docPath, sessionID)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	result, err := session.Ask(ctx, prompt, func(ev claudeproc.Event) {
		if ev.Kind == claudeproc.EventText && ev.Text != "" {
			r.publish(assistantFrame{Type: "assistant", ThreadID: threadID, Text: ev.Text})
		}
	})
	if err != nil {
		r.finishTurn(threadID, docPath, Message{
			Role: RoleAssistant,
			Text: "The review process failed: " + err.Error(),
		}, "")
		r.publish(errorFrame{Type: "error", Message: err.Error()})
		return
	}

	commit := r.record(docPath, result.Edited, subject, threadID)

	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = "(no reply)"
	}
	r.finishTurn(threadID, docPath, Message{Role: RoleAssistant, Text: text}, commit)
}

// record commits whatever the turn changed. A turn that only answered a
// question edits nothing and records nothing, which is the common case for
// "why this?".
func (r *Review) record(docPath string, edited []string, subject, threadID string) string {
	paths := r.relativise(edited)
	if len(paths) == 0 {
		return ""
	}

	ref, err := r.hist.Record(gitstore.Commit{
		Paths:   paths,
		Subject: commitSubject(subject),
		Body:    fmt.Sprintf("Document: %s\nComment: %s", docPath, subject),
		Trailers: []gitstore.Trailer{
			{Key: "Review-Thread", Value: threadID},
		},
	})
	if err != nil {
		r.publish(errorFrame{Type: "error", Message: "could not record the change: " + err.Error()})
		return ""
	}
	return ref
}

// relativise converts the paths the model reported into repository-relative
// ones, dropping anything outside the review root.
func (r *Review) relativise(paths []string) []string {
	var out []string
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(r.root, p)
		}
		rel, err := filepath.Rel(r.root, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

// commitSubject turns a reviewer's comment into a one-line subject.
func commitSubject(comment string) string {
	line := strings.TrimSpace(strings.SplitN(comment, "\n", 2)[0])
	line = strings.Join(strings.Fields(line), " ")
	const max = 62
	if len(line) > max {
		line = strings.TrimSpace(line[:max]) + "…"
	}
	if line == "" {
		line = "review change"
	}
	return "review: " + line
}

func (r *Review) finishTurn(threadID, docPath string, reply Message, commit string) {
	r.mu.Lock()
	if thread := r.threads[threadID]; thread != nil {
		thread.Messages = append(thread.Messages, reply)
		thread.Status = StatusOpen
		if commit != "" {
			thread.Commit = commit
		}
	}
	r.mu.Unlock()

	r.publish(turnEndFrame{
		Type:     "turnEnd",
		ThreadID: threadID,
		Edited:   commit != "",
		Commit:   commit,
	})
	r.broadcastThreads(docPath)
	_ = r.save()
}

func (r *Review) setBusy(docPath string, delta int) {
	r.mu.Lock()
	r.inflight[docPath] += delta
	busy := r.inflight[docPath] > 0
	r.mu.Unlock()
	r.publish(busyFrame{Type: "busy", Doc: docPath, Busy: busy})
}

// --- re-anchoring -----------------------------------------------------------

// reanchor re-locates every thread on a document after its content changed.
// A thread whose passage has vanished is marked outdated rather than deleted:
// the conversation usually explains why the text is gone.
func (r *Review) reanchor(docPath string, src string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, id := range r.order {
		thread := r.threads[id]
		if thread == nil || thread.Doc != docPath {
			continue
		}
		if thread.Status == StatusResolved || thread.Status == StatusThinking {
			continue
		}
		if _, ok := Locate(src, thread.Anchor); ok {
			thread.Status = StatusOpen
		} else {
			thread.Status = StatusOutdated
		}
	}
}

// --- persistence ------------------------------------------------------------

type persisted struct {
	Branch   string            `json:"branch"`
	Sessions map[string]string `json:"sessions"`
	Threads  []*Thread         `json:"threads"`
}

func (r *Review) statePath() string {
	return filepath.Join(r.root, ".ai-reviewer", "state.json")
}

func (r *Review) load() error {
	data, err := os.ReadFile(r.statePath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	var state persisted
	if err := json.Unmarshal(data, &state); err != nil {
		// A corrupt state file should not make the documents unreviewable.
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if state.Sessions != nil {
		r.sessions = state.Sessions
	}
	for _, t := range state.Threads {
		if t == nil || t.ID == "" {
			continue
		}
		// A turn cannot survive a restart; anything mid-flight is reopened.
		if t.Status == StatusThinking {
			t.Status = StatusOpen
		}
		r.threads[t.ID] = t
		r.order = append(r.order, t.ID)
	}
	return nil
}

func (r *Review) save() error {
	r.mu.Lock()
	state := persisted{Branch: r.branch, Sessions: map[string]string{}}
	for k, v := range r.sessions {
		state.Sessions[k] = v
	}
	for _, id := range r.order {
		if t := r.threads[id]; t != nil {
			state.Threads = append(state.Threads, t.clone())
		}
	}
	r.mu.Unlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.statePath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Write through a temporary file so a crash mid-save cannot leave the
	// review with a truncated state file.
	tmp := r.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.statePath())
}
