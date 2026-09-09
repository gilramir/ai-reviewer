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

type docListFrame struct {
	Type string   `json:"type"`
	Docs []string `json:"docs"`
}

type errorFrame struct {
	Type    string `json:"type"`
	Message string `json:"message"`
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
	doc, err := r.Render(docPath)
	if err != nil {
		return err
	}
	r.publish(docFrame{Type: "doc", Doc: doc})
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

// PublishError surfaces a message in the browser.
func (r *Review) PublishError(message string) {
	r.publish(errorFrame{Type: "error", Message: message})
}
