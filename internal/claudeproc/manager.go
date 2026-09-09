package claudeproc

import (
	"sort"
	"sync"
	"time"
)

// Manager keeps one Session per document.
//
// Per-document is the unit that matters: the document stays in that
// conversation's context, so a second comment on the same file is answered from
// the prompt cache rather than re-read from scratch. The cost is one Node
// process per open document, which is why idle sessions are reaped and the
// total is capped.
//
// Reaping only kills the process. The session id survives, so the next comment
// resumes the same conversation.
type Manager struct {
	cfg         Config
	maxLive     int
	idleTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*Session // keyed by document path
	stop     chan struct{}
	stopOnce sync.Once
}

// ManagerOptions tunes resource limits. Zero values pick sensible defaults.
type ManagerOptions struct {
	// MaxLive caps concurrently running processes. Default 6.
	MaxLive int
	// IdleTimeout kills a process that has not run a turn for this long.
	// Default 30 minutes.
	IdleTimeout time.Duration
}

// NewManager starts a manager and its reaper. Call Close to shut everything
// down.
func NewManager(cfg Config, opts ManagerOptions) *Manager {
	if opts.MaxLive <= 0 {
		opts.MaxLive = 6
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 30 * time.Minute
	}
	cfg.applyDefaults()

	m := &Manager{
		cfg:         cfg,
		maxLive:     opts.MaxLive,
		idleTimeout: opts.IdleTimeout,
		sessions:    map[string]*Session{},
		stop:        make(chan struct{}),
	}
	go m.reap()
	return m
}

// Session returns the session for a document, creating it against sessionID if
// this is the first time the document has been opened. sessionID must be stable
// for a given document across daemon restarts.
func (m *Manager) Session(docPath, sessionID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[docPath]; ok {
		return s
	}

	s := New(sessionID, m.cfg)
	m.sessions[docPath] = s
	m.evictLocked()
	return s
}

// Forget drops a document's session entirely, stopping its process. Used when a
// file is deleted or renamed, where the conversation's context refers to a path
// that no longer means anything.
func (m *Manager) Forget(docPath string) {
	m.mu.Lock()
	s := m.sessions[docPath]
	delete(m.sessions, docPath)
	m.mu.Unlock()

	if s != nil {
		_ = s.Close()
	}
}

// Close stops every process. Conversations remain resumable on disk.
func (m *Manager) Close() {
	m.stopOnce.Do(func() { close(m.stop) })

	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = map[string]*Session{}
	m.mu.Unlock()

	for _, s := range sessions {
		_ = s.Close()
	}
}

// evictLocked stops the least recently used processes until the live count is
// within budget. Callers must hold m.mu.
func (m *Manager) evictLocked() {
	live := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s.Running() {
			live = append(live, s)
		}
	}
	if len(live) <= m.maxLive {
		return
	}

	sort.Slice(live, func(i, j int) bool {
		return live[i].Idle() > live[j].Idle()
	})
	for _, s := range live[:len(live)-m.maxLive] {
		_ = s.Close()
	}
}

func (m *Manager) reap() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.mu.Lock()
			for _, s := range m.sessions {
				if s.Running() && s.Idle() > m.idleTimeout {
					_ = s.Close()
				}
			}
			m.mu.Unlock()
		}
	}
}
