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

// Origin says who opened a thread.
//
// It is a field rather than a reading of who wrote the first message, because
// the two questions diverge as soon as anything is appended: the wire and the
// sidebar both want to know whose thread this is, and asking that of a
// transcript is a guess that gets worse over time.
type Origin string

const (
	// OriginReviewer is a thread a person opened by selecting a passage.
	OriginReviewer Origin = "reviewer"
	// OriginModel is a thread a reviewing pass raised.
	OriginModel Origin = "model"
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
	Commit string `json:"commit"`
	// Origin is who opened the thread. Empty in a state file written before
	// machine review existed, which load reads as a reviewer's.
	Origin Origin `json:"origin,omitempty"`
	// Brief is what the pass that raised this thread was asked to look for.
	// Empty on a reviewer's own thread, which was not asked for anything.
	//
	// It is kept so that "why is this comment here?" still has an answer three
	// passes later, and so the sidebar has something to group by. One string
	// recording what was asked is not a classification of what the model meant.
	Brief string `json:"brief,omitempty"`
	// HandedOff records that the editing conversation has been told about this
	// thread. A machine thread was raised in a different conversation, so the
	// first reply on it has to state the passage and the comment from scratch;
	// after that the conversation remembers.
	HandedOff bool      `json:"handedOff,omitempty"`
	Created   time.Time `json:"created"`
}

// AwaitsReviewer reports that the last word was the model's, so the thread is
// waiting on a person.
//
// This is the whole of what machine review adds to the lifecycle, and it is
// derived rather than stored: `open` already means somebody owes something, and
// which somebody is a reading of the transcript, not a fifth status.
func (t *Thread) AwaitsReviewer() bool {
	if t.Status != StatusOpen || len(t.Messages) == 0 {
		return false
	}
	return t.Messages[len(t.Messages)-1].Role == RoleAssistant
}

// clone returns a copy safe to hand to another goroutine for marshalling.
func (t *Thread) clone() *Thread {
	out := *t
	out.Messages = append([]Message(nil), t.Messages...)
	return &out
}
