package review

import (
	"encoding/json"

	"github.com/gilramir/ai-reviewer/internal/mdast"
)

// The frames below are the server half of the protocol Protocol.gren decodes.
// Each is a separate type rather than one struct with optional fields, because
// `doc` is a string in some frames and a document tree in others.

type docFrame struct {
	Type string         `json:"type"`
	Doc  mdast.Document `json:"doc"`
	// Changes are the passages this render has that the session did not start
	// with. They travel with the tree because they are only true of this
	// render of it.
	Changes []Range `json:"changes,omitempty"`
}

type threadsFrame struct {
	Type    string    `json:"type"`
	Doc     string    `json:"doc"`
	Threads []*Thread `json:"threads"`
}

type assistantFrame struct {
	Type     string `json:"type"`
	ThreadID string `json:"threadId"`
	Text     string `json:"text"`
}

type turnEndFrame struct {
	Type     string `json:"type"`
	ThreadID string `json:"threadId"`
	Edited   bool   `json:"edited"`
	Commit   string `json:"commit"`
}

type busyFrame struct {
	Type string `json:"type"`
	Doc  string `json:"doc"`
	Busy bool   `json:"busy"`
}

// passProgress is how far a reviewing pass has got. It is a frame of its own
// rather than a flag on busy, because "thinking" and "on section 3 of 9" are
// different things to be told: one is a spinner and the other is a reason to
// wait.
type passProgress struct {
	Active  bool   `json:"active"`
	Section int    `json:"section,omitempty"`
	Total   int    `json:"total,omitempty"`
	Title   string `json:"title,omitempty"`
	Brief   string `json:"brief,omitempty"`
}

type passFrame struct {
	Type string `json:"type"`
	Doc  string `json:"doc"`
	passProgress
}

type docListFrame struct {
	Type string   `json:"type"`
	Docs []string `json:"docs"`
}

// editSourceFrame hands the browser the Markdown behind a passage it asked to
// edit. It carries the quote it answers, since every frame goes to every
// connection and a second tab may have a composer of its own open.
type editSourceFrame struct {
	Type  string `json:"type"`
	Doc   string `json:"doc"`
	Quote string `json:"quote"`
	Text  string `json:"text"`
}

// editAppliedFrame confirms a hand edit. The editor stays on screen until this
// arrives, for the same reason the composer does: typed words are the one thing
// the server cannot give back.
type editAppliedFrame struct {
	Type   string `json:"type"`
	Doc    string `json:"doc"`
	Quote  string `json:"quote"`
	Commit string `json:"commit,omitempty"`
}

type errorFrame struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type settingsFrame struct {
	Type     string   `json:"type"`
	Settings Settings `json:"settings"`
}

// Subscribe registers a browser connection. The returned channel carries
// pre-marshalled frames; cancel must be called when the connection closes.
//
// Sends are dropped rather than blocked: one wedged browser tab must not stall
// a turn that other tabs are watching.
func (r *Review) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 64)

	r.mu.Lock()
	id := r.nextSub
	r.nextSub++
	r.subs[id] = ch
	r.mu.Unlock()

	return ch, func() {
		r.mu.Lock()
		if existing, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(existing)
		}
		r.mu.Unlock()
	}
}

// publish marshals a frame and fans it out to every connection.
func (r *Review) publish(frame any) {
	data, err := json.Marshal(frame)
	if err != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.subs {
		select {
		case ch <- data:
		default:
			// Subscriber is not keeping up. Dropping a frame is recoverable:
			// the browser reconnects and refetches the document and threads.
		}
	}
}

// PublishDoc renders a document and pushes it to every connection.
func (r *Review) PublishDoc(docPath string) error {
	doc, changes, err := r.render(docPath)
	if err != nil {
		return err
	}
	r.publish(docFrame{Type: "doc", Doc: doc, Changes: changes})
	return nil
}

// PublishDocList pushes the current set of reviewable documents.
func (r *Review) PublishDocList() {
	docs, err := r.Docs()
	if err != nil {
		r.publish(errorFrame{Type: "error", Message: err.Error()})
		return
	}
	r.publish(docListFrame{Type: "docList", Docs: docs})
}

func (r *Review) broadcastThreads(docPath string) {
	r.mu.Lock()
	threads := r.threadsForLocked(docPath)
	r.mu.Unlock()
	r.publish(threadsFrame{Type: "threads", Doc: docPath, Threads: threads})
}

// PublishThreads pushes the threads on a document to every connection.
func (r *Review) PublishThreads(docPath string) {
	r.broadcastThreads(docPath)
}

// PublishNotices surfaces whatever this review could not load — a state file
// that had to be moved aside, say. They are sent on every connection rather
// than once, since the reviewer who needs to see one may open the page long
// after the daemon started.
func (r *Review) PublishNotices() {
	for _, message := range r.Notices() {
		r.publish(errorFrame{Type: "error", Message: message})
	}
}

// PublishSettings pushes the current configuration to every connection. Sent on
// connect, when the model changes, and at the end of each turn -- which is when
// the spend and the running model change.
func (r *Review) PublishSettings() {
	r.publish(settingsFrame{Type: "settings", Settings: r.Settings()})
}

// PublishEditSource sends the Markdown behind a passage to the browser, so the
// reviewer can change it themselves.
func (r *Review) PublishEditSource(docPath, quote, text string) {
	r.publish(editSourceFrame{Type: "editSource", Doc: docPath, Quote: quote, Text: text})
}

// publishPass reports where a reviewing pass has got to. A zero progress is the
// pass being over, which is what takes the notice off the screen.
func (r *Review) publishPass(docPath string, at passProgress) {
	r.publish(passFrame{Type: "pass", Doc: docPath, passProgress: at})
}

// PublishError surfaces a message in the browser.
func (r *Review) PublishError(message string) {
	r.publish(errorFrame{Type: "error", Message: message})
}
