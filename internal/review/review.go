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
	root string
	// work is the directory Claude Code runs in and the base every path it is
	// given or reports is relative to: the repository top level when the
	// documents live inside one, the review root when they do not.
	//
	// It is wider than root on purpose. The CLI's working directory is its file
	// permission boundary — a document in doc/ cannot read ../src from a
	// process started in doc/ — and a repository's CLAUDE.md, settings and
	// sibling sources are exactly the context a question about a document tends
	// to need. Which files the *browser* can open is a separate question, and
	// that answer is still root.
	work   string
	branch string
	// base is the branch the review branch was cut from, and so the branch its
	// commits are waiting to be merged into. It is remembered across restarts
	// because by then the tree is already on the review branch and the answer
	// is no longer on disk anywhere.
	base  string
	hist  gitstore.History
	procs *claudeproc.Manager

	mu       sync.Mutex
	revs     map[string]int      // document path -> render revision
	assets   map[string][]string // document path -> files its last render embeds
	sessions map[string]string   // document path -> Claude session id
	inflight map[string]int      // document path -> turns running
	threads  map[string]*Thread  // thread id -> thread
	order    []string            // thread ids, oldest first
	subs     map[int]chan []byte // subscriber id -> outbound frames
	nextSub  int
	notices  []string // things the reviewer must be told about this review

	// runningModel is what the CLI reported for the most recent turn, and
	// spentUSD what every turn since startup has cost. Both are display only.
	runningModel string
	spentUSD     float64

	cliVersion string
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

	// Asked before the branch is switched: afterwards every answer is the
	// review branch itself.
	base := hist.Branch()

	if opts.Branch != "" {
		if err := hist.EnsureBranch(opts.Branch); err != nil {
			return nil, err
		}
	}

	r := &Review{
		root:   root,
		work:   hist.Root(),
		branch: opts.Branch,
		hist:   hist,
		procs: claudeproc.NewManager(
			claudeproc.Config{
				Binary:       opts.ClaudeBinary,
				WorkDir:      hist.Root(),
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
		assets:   map[string][]string{},
		sessions: map[string]string{},
		inflight: map[string]int{},
		threads:  map[string]*Thread{},
		subs:     map[int]chan []byte{},
	}

	r.cliVersion = claudeVersion(opts.ClaudeBinary)

	state, err := r.load()
	if err != nil {
		return nil, err
	}

	// A model chosen in the browser is remembered across restarts, but --model
	// on this run's command line is the more recent decision and wins.
	if opts.Model == "" && state.Model != "" {
		r.procs.SetModel(state.Model)
	}

	// A restart finds the tree already on the review branch, which says nothing
	// about where it came from. That is only knowable from what the run that
	// created it wrote down.
	if base == "" || base == opts.Branch {
		base = state.Base
	}
	r.base = base

	return r, nil
}

// Close stops every Claude process and flushes state to disk.
func (r *Review) Close() error {
	r.procs.Close()
	return r.save()
}

// Root is the directory under review.
func (r *Review) Root() string { return r.root }

// WorkRoot is where Claude Code runs: the repository holding the documents, or
// the review root outside a repository.
func (r *Review) WorkRoot() string { return r.work }

// workspacePath rewrites a review-root-relative document path into one relative
// to the workspace, which is the form both the model and git understand. The
// browser keeps using the review-root-relative form; the two differ whenever a
// review is rooted at a subdirectory of a repository.
func (r *Review) workspacePath(docPath string) string {
	full := filepath.Join(r.root, filepath.FromSlash(docPath))
	rel, err := filepath.Rel(r.work, full)
	if err != nil {
		return docPath
	}
	return filepath.ToSlash(rel)
}

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

	doc := mdast.Render(docPath, rev, src)

	// The images are pointed at the file route and stamped with what is on disk
	// right now, which is also how a document learns which files it depends on.
	r.noteAssets(docPath, r.linkAssets(docPath, &doc.Root))

	return doc, nil
}

func (r *Review) read(docPath string) ([]byte, error) {
	full, err := r.resolve(docPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

// resolve turns a client-supplied document path into an absolute one.
func (r *Review) resolve(docPath string) (string, error) {
	full, err := r.resolvePath(docPath)
	if err != nil {
		return "", err
	}
	if !isMarkdown(full) {
		return "", fmt.Errorf("%q is not a Markdown document", docPath)
	}
	return full, nil
}

// resolvePath turns a client-supplied path into an absolute one, refusing
// anything that escapes the review root. Every path in a request is
// attacker-controlled once the daemon listens on a network interface.
func (r *Review) resolvePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	clean := filepath.Clean(filepath.FromSlash(p))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", errOutsideRoot(p)
	}
	full := filepath.Join(r.root, clean)
	rel, err := filepath.Rel(r.root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", errOutsideRoot(p)
	}
	return full, nil
}

func errOutsideRoot(p string) error {
	return fmt.Errorf("path %q is outside the review root", p)
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

	prompt := commentPrompt(r.workspacePath(docPath), anchor.Quote, blockAround(string(src), at), body)
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
	anchor := thread.Anchor
	thread.Messages = append(thread.Messages, Message{Role: RoleUser, Text: body})
	thread.Status = StatusThinking
	// A reply normally leans on the conversation to know which passage it is
	// about. With no session there is no conversation to lean on -- the context
	// was cleared -- so the passage has to be said again.
	remembers := r.sessions[docPath] != ""
	r.mu.Unlock()

	prompt := replyPrompt(body)
	if !remembers {
		prompt = r.reopenPrompt(docPath, anchor, body)
	}

	go r.runTurn(threadID, docPath, prompt, body)

	r.broadcastThreads(docPath)
	return nil
}

// reopenPrompt states a thread's passage from scratch, for a reply that lands in
// a conversation which no longer remembers it. It falls back to the bare reply
// when the passage cannot be found, which is no worse than saying nothing.
func (r *Review) reopenPrompt(docPath string, anchor Anchor, body string) string {
	src, err := r.read(docPath)
	if err != nil {
		return replyPrompt(body)
	}
	at, ok := Locate(string(src), anchor)
	if !ok {
		return replyPrompt(body)
	}
	return commentPrompt(r.workspacePath(docPath), anchor.Quote, blockAround(string(src), at), body)
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
		r.noteTurnCost(result.Model, result.CostUSD)
		r.finishTurn(threadID, docPath, Message{
			Role: RoleAssistant,
			Text: "The review process failed: " + err.Error(),
		}, "")
		r.publish(errorFrame{Type: "error", Message: err.Error()})
		return
	}

	r.noteTurnCost(result.Model, result.CostUSD)

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
		Body:    fmt.Sprintf("Document: %s\nComment: %s", r.workspacePath(docPath), subject),
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

// relativise converts the paths the model reported into workspace-relative
// ones, dropping anything outside the workspace.
//
// Workspace-relative rather than review-root-relative because these paths go
// straight to `git add`, which runs at the workspace root. A review rooted at
// docs/ inside a repository would otherwise stage "spec.md" from the repository
// top level, where no such file exists: the edit lands on disk and the commit
// that was supposed to record it never happens.
func (r *Review) relativise(paths []string) []string {
	var out []string
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(r.work, p)
		}
		rel, err := filepath.Rel(r.work, abs)
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
	r.PublishSettings()
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
	Branch string `json:"branch"`
	// Base is the branch Branch was created from. Without it a restarted
	// daemon cannot say where the review's commits are meant to land.
	Base string `json:"base,omitempty"`
	// Model is the choice made in the browser, which outlives the process that
	// ran it. Absent means no choice was made and the CLI's own default stands.
	Model    string            `json:"model,omitempty"`
	Sessions map[string]string `json:"sessions"`
	Threads  []*Thread         `json:"threads"`
}

// writeSynced writes a file and waits for the bytes to reach the disk.
func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (r *Review) statePath() string {
	return filepath.Join(r.root, ".ai-reviewer", "state.json")
}

// load restores the saved state and returns it, so New can apply the parts that
// belong to something other than the Review itself.
func (r *Review) load() (persisted, error) {
	path := r.statePath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return persisted{}, nil
	}
	if err != nil {
		return persisted{}, err
	}

	var state persisted
	if err := json.Unmarshal(data, &state); err != nil {
		// The threads in this file are the record of a review, and the first
		// thing this run would otherwise do is save an empty state over them.
		// So the file is moved aside, kept whole and hand-recoverable, and the
		// review continues without it — a bad state file should not make the
		// documents unreviewable. What it must never be is quiet: the notice
		// goes to the terminal and to every browser that connects.
		kept, moveErr := quarantine(path)
		if moveErr != nil {
			// Nothing has been lost yet, and continuing would overwrite the
			// file at the next save. Refuse to start instead.
			return persisted{}, fmt.Errorf("%s is unreadable (%v) and could not be moved aside: %w", path, err, moveErr)
		}
		r.notice(fmt.Sprintf(
			"%s could not be read (%v), so no comment threads were loaded. "+
				"The file was kept as %s.",
			path, err, filepath.Base(kept)))
		return persisted{}, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if state.Sessions != nil {
		r.sessions = state.Sessions
	}
	skipped := 0
	for _, t := range state.Threads {
		if t == nil || t.ID == "" {
			skipped++
			continue
		}
		// A turn cannot survive a restart; anything mid-flight is reopened.
		if t.Status == StatusThinking {
			t.Status = StatusOpen
		}
		r.threads[t.ID] = t
		r.order = append(r.order, t.ID)
	}
	if skipped > 0 {
		r.notices = append(r.notices, fmt.Sprintf(
			"%d thread(s) in %s had no id and were not loaded.", skipped, filepath.Base(path)))
	}
	return state, nil
}

// quarantine moves a state file aside and reports where it went. The timestamp
// keeps a second bad start from overwriting what the first one saved.
func quarantine(path string) (string, error) {
	base := path + ".corrupt-" + time.Now().UTC().Format("20060102T150405")
	dest := base
	for i := 1; ; i++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = fmt.Sprintf("%s-%d", base, i)
	}
	return dest, os.Rename(path, dest)
}

// notice records something the reviewer has to be told. Notices are rare and
// never routine: each one means state this daemon could not use.
func (r *Review) notice(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, message)
}

// Notices returns what this review could not load. main prints them; the
// browser shows them on connect.
func (r *Review) Notices() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notices...)
}

func (r *Review) save() error {
	// Read outside the lock: the manager has its own, and nothing here needs
	// the two held together.
	model := r.procs.Config().Model

	r.mu.Lock()
	state := persisted{
		Branch:   r.branch,
		Base:     r.base,
		Model:    model,
		Sessions: map[string]string{},
	}
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

	// Write through a temporary file, flushed to the disk before it is put in
	// place, so a crash mid-save cannot leave the review with a truncated
	// state file. Without the flush the rename can land while the contents are
	// still in the page cache, which is exactly how a state file ends up
	// half-written and unreadable on the next start.
	tmp := r.statePath() + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		return err
	}
	return os.Rename(tmp, r.statePath())
}
