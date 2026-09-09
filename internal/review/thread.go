package review

import "time"

// Status is where a thread sits in its lifecycle.
type Status string

const (
	// StatusOpen is a thread awaiting the reviewer.
	StatusOpen Status = "open"
	// StatusThinking is a thread with a turn in flight.
	StatusThinking Status = "thinking"
	// StatusResolved is a thread the reviewer closed.
	StatusResolved Status = "resolved"
	// StatusOutdated is a thread whose passage no longer exists in the
	// document. The conversation is kept — it is often the record of why the
	// text changed — but it can no longer be pointed at anything.
	StatusOutdated Status = "outdated"
)

// Role distinguishes who wrote a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn of a thread's transcript.
type Message struct {
	Role Role   `json:"role"`
	Text string `json:"text"`
}

// Thread is a comment and everything that followed from it.
type Thread struct {
	ID       string    `json:"id"`
	Doc      string    `json:"doc"`
	Anchor   Anchor    `json:"anchor"`
	Status   Status    `json:"status"`
	Messages []Message `json:"messages"`
	// Commit references the change this thread caused, if any.
	Commit  string    `json:"commit"`
	Created time.Time `json:"created"`
}

// clone returns a copy safe to hand to another goroutine for marshalling.
func (t *Thread) clone() *Thread {
	out := *t
	out.Messages = append([]Message(nil), t.Messages...)
	return &out
}
