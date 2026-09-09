package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/gilramir/ai-reviewer/internal/review"
)

// clientFrame is every message the browser can send. One struct suffices here
// because no field means two different shapes in different frames.
type clientFrame struct {
	Type     string        `json:"type"`
	Path     string        `json:"path"`
	Doc      string        `json:"doc"`
	Anchor   review.Anchor `json:"anchor"`
	Body     string        `json:"body"`
	ThreadID string        `json:"threadId"`
}

const (
	writeTimeout = 10 * time.Second
	pongTimeout  = 60 * time.Second
	pingEvery    = (pongTimeout * 9) / 10
	maxFrame     = 1 << 20
)

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     s.originAllowed,
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written a response.
		return
	}
	defer conn.Close()

	conn.SetReadLimit(maxFrame)

	frames, cancel := s.opts.Review.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go s.writeLoop(conn, frames, done)

	// Everything the browser needs to draw itself, so a reconnect after a
	// daemon restart recovers without the reviewer touching anything.
	s.opts.Review.PublishDocList()

	s.readLoop(conn)
	close(done)
}

// readLoop handles messages from one browser until it disconnects.
func (s *Server) readLoop(conn *websocket.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var frame clientFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			s.opts.Review.PublishError("malformed request")
			continue
		}
		s.dispatch(frame)
	}
}

func (s *Server) dispatch(frame clientFrame) {
	rev := s.opts.Review

	switch frame.Type {
	case "openDoc":
		if err := rev.PublishDoc(frame.Path); err != nil {
			rev.PublishError(err.Error())
			return
		}
		rev.PublishThreads(frame.Path)

	case "comment":
		if _, err := rev.Comment(frame.Doc, frame.Anchor, frame.Body); err != nil {
			rev.PublishError(err.Error())
		}

	case "reply":
		if err := rev.Reply(frame.ThreadID, frame.Body); err != nil {
			rev.PublishError(err.Error())
		}

	case "resolve":
		if err := rev.Resolve(frame.ThreadID); err != nil {
			rev.PublishError(err.Error())
		}

	case "interrupt":
		rev.Interrupt(frame.Doc)

	default:
		rev.PublishError("unknown request: " + frame.Type)
	}
}

// writeLoop forwards published frames and keeps the connection alive.
func (s *Server) writeLoop(conn *websocket.Conn, frames <-chan []byte, done <-chan struct{}) {
	ticker := time.NewTicker(pingEvery)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return

		case frame, ok := <-frames:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				return
			}

		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Log is where the server reports problems that are not the browser's business.
var Log = log.Default()
