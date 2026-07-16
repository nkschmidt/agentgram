// Package session manages the lifecycle of backend processes:
// each user has at most one active session at any moment.
// Starting a new one automatically stops the previous one (regardless
// of which backend it was).
package session

import (
	"context"
	"log"
	"sync"
	"sync/atomic"

	"github.com/schmidt/agentgram/internal/backend"
)

// ChunkHandler is called on every Chunk from an active session (including
// the final one with Err). The ideal place for the implementation is main,
// which has access to Replier and can send the content back to Telegram.
// Manager itself knows nothing about Telegram (SoC).
type ChunkHandler func(s *Session, chunk backend.Chunk)

// Manager — thread-safe per-user session manager.
type Manager struct {
	registry *backend.Registry
	onChunk  ChunkHandler

	// ctx lives for the entire Manager's life; child session ctxs depend on it,
	// so Shutdown is guaranteed to kill all processes.
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	active map[int64]*Session
}

// Session — a user's active session.
//
// cancelled is raised when the session is closed "by command" (a new Start
// for the same user, an explicit Stop, or Shutdown). drain sees the flag
// and quietly reads the rest of the channel, without duplicating chunks or
// the "session ended" message into the chat — this is an expected closure,
// not a crash.
type Session struct {
	UserID  int64
	ChatID  int64 // for sending the reply back to the right chat
	Name    string
	Backend backend.Backend

	cancel    context.CancelFunc
	cancelled atomic.Bool
}

// Cancelled reports whether the session was closed by command (a backend
// switch, an explicit Stop, or Shutdown) rather than failing on its own. A
// blocking Backend.Send that returns after such a close did so because of the
// switch — the caller should treat that as expected, not an error.
func (s *Session) Cancelled() bool { return s.cancelled.Load() }

func NewManager(reg *backend.Registry, onChunk ChunkHandler) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		registry: reg,
		onChunk:  onChunk,
		ctx:      ctx,
		cancel:   cancel,
		active:   map[int64]*Session{},
	}
}

// Start launches a backend session named backendName for userID.
// If the user already had an active session — it'll be stopped.
// ChatID is needed by the Manager to send chunks from the backend to
// the right chat via onChunk.
func (m *Manager) Start(_ context.Context, userID, chatID int64, backendName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if prev, ok := m.active[userID]; ok {
		log.Printf("session: stopping previous %q for user %d", prev.Name, userID)
		prev.cancelled.Store(true)
		prev.cancel()
		_ = prev.Backend.Stop()
		delete(m.active, userID)
	}

	b, err := m.registry.New(backendName, userID)
	if err != nil {
		return err
	}

	sessCtx, sessCancel := context.WithCancel(m.ctx)
	if err := b.Start(sessCtx); err != nil {
		sessCancel()
		return err
	}

	s := &Session{
		UserID:  userID,
		ChatID:  chatID,
		Name:    backendName,
		Backend: b,
		cancel:  sessCancel,
	}
	m.active[userID] = s

	go m.drain(s)

	log.Printf("session: started %q for user %d", backendName, userID)
	return nil
}

// drain listens on Recv until the channel closes and forwards every chunk
// to onChunk (if set). When the channel closes, removes the session from active.
//
// Important: with cancelled = true we keep reading the channel until it closes —
// otherwise Backend.wait blocks on the final write and the goroutine leaks.
// The chunks themselves in that case are not forwarded to onChunk: the user
// shouldn't see noise from a session they closed themselves.
func (m *Manager) drain(s *Session) {
	for chunk := range s.Backend.Recv() {
		if chunk.Err != nil {
			log.Printf("session %d (%s) exited: %v", s.UserID, s.Name, chunk.Err)
		}
		if s.cancelled.Load() {
			continue
		}
		if m.onChunk != nil {
			m.onChunk(s, chunk)
		}
	}
	m.mu.Lock()
	if cur, ok := m.active[s.UserID]; ok && cur == s {
		delete(m.active, s.UserID)
	}
	m.mu.Unlock()
}

// Active returns the user's active session, if any.
func (m *Manager) Active(userID int64) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.active[userID]
	return s, ok
}

// ActiveSessions returns a snapshot of all active sessions. Used, for example,
// to restart them when a global setting changes (working directory).
// Returns a copy of the list — iteration doesn't hold the mutex.
func (m *Manager) ActiveSessions() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.active))
	for _, s := range m.active {
		out = append(out, s)
	}
	return out
}

// ActiveName — narrow helper for UI (backend name without leaking *Session outside).
func (m *Manager) ActiveName(userID int64) (string, bool) {
	s, ok := m.Active(userID)
	if !ok {
		return "", false
	}
	return s.Name, true
}

// Interrupt asks the user's active process to interrupt the current work (SIGINT).
// The session is NOT removed and not marked cancelled — if the process survived,
// we want to keep seeing its further output.
func (m *Manager) Interrupt(userID int64) error {
	m.mu.Lock()
	s, ok := m.active[userID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return s.Backend.Interrupt()
}

// Stop stops the user's session, if any.
func (m *Manager) Stop(userID int64) error {
	m.mu.Lock()
	s, ok := m.active[userID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.active, userID)
	m.mu.Unlock()

	s.cancelled.Store(true)
	s.cancel()
	return s.Backend.Stop()
}

// Shutdown stops all active sessions. Called when the bot process exits,
// to avoid leaving zombie processes.
func (m *Manager) Shutdown() {
	m.cancel() // child session ctxs cancel cascading

	m.mu.Lock()
	for _, s := range m.active {
		s.cancelled.Store(true)
		_ = s.Backend.Stop()
	}
	m.active = map[int64]*Session{}
	m.mu.Unlock()
}
