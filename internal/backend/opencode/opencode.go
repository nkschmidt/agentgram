package opencode

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/schmidt/agentgram/internal/backend"
)

// Backend — backend.Backend implementation on top of the opencode HTTP API.
// Each Backend holds its own session on the server and its own SSE subscription.
// The server itself is started lazily via LazyServer on the first Start.
//
// Events are parsed in events.go, JSON models — in models.go.
type Backend struct {
	lazy    *LazyServer
	workDir func() string
	client  *Client

	mu        sync.Mutex
	sessionID string
	out       chan backend.Chunk
	cancel    context.CancelFunc
	stopped   bool

	// Current "assembly" of the model's response to the current user request.
	// Cleared on session.idle / session.error.
	// lastSent stores the prompt of the last Send — opencode echoes it back
	// via message.part.updated (for user-message); we filter such parts out,
	// otherwise they end up in the model's final answer.
	stateMu  sync.Mutex
	parts    map[string]opPart // id → part (text / tool)
	order    []string          // order of part appearance
	lastSent string            // text of the last Send (for echo filter)
}

// opPart — unit of the accumulated snapshot. kind distinguishes the model's
// text from tool calls: on final we keep only the text (unified with claude,
// where Final also carries only the model's final answer without steps).
type opPart struct {
	kind    string // "text" | "tool"
	content string // text ready for display
}

// New returns a Backend factory. provider is read on every SendMessage
// for the given userID — each user has their own working directory.
func New(lazy *LazyServer, provider func(userID int64) string) backend.Factory {
	return func(userID int64) backend.Backend {
		return &Backend{
			lazy:    lazy,
			workDir: func() string { return provider(userID) },
			parts:   map[string]opPart{},
		}
	}
}

func (b *Backend) Start(ctx context.Context) error {
	server, err := b.lazy.Get(ctx)
	if err != nil {
		return fmt.Errorf("opencode server: %w", err)
	}

	b.mu.Lock()
	if b.out != nil {
		b.mu.Unlock()
		return errors.New("opencode: already started")
	}
	b.client = server.Client(b.workDir)
	b.out = make(chan backend.Chunk, 64)
	subCtx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.mu.Unlock()

	sessionID, err := b.client.CreateSession(ctx)
	if err != nil {
		cancel()
		b.mu.Lock()
		close(b.out)
		b.out = nil
		b.mu.Unlock()
		return fmt.Errorf("opencode create session: %w", err)
	}
	b.mu.Lock()
	b.sessionID = sessionID
	b.mu.Unlock()

	events, err := b.client.Events(subCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("opencode events: %w", err)
	}
	go b.consume(events, sessionID)
	return nil
}

func (b *Backend) Send(input string) error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return errors.New("opencode: stopped")
	}
	sessionID := b.sessionID
	b.mu.Unlock()
	if sessionID == "" {
		return errors.New("opencode: no session")
	}
	// New request — clear the accumulated snapshot of the previous response,
	// so parts don't "stick" to the new round. Remember the text —
	// it'll be needed in renderPart to filter out the user-echo.
	b.resetParts()
	b.stateMu.Lock()
	b.lastSent = input
	b.stateMu.Unlock()
	return b.client.SendMessage(context.Background(), sessionID, input)
}

func (b *Backend) Recv() <-chan backend.Chunk { return b.out }

func (b *Backend) Interrupt() error {
	b.mu.Lock()
	sessionID := b.sessionID
	b.mu.Unlock()
	if sessionID == "" {
		return nil
	}
	return b.client.Abort(context.Background(), sessionID)
}

func (b *Backend) Stop() error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return nil
	}
	b.stopped = true
	sessionID := b.sessionID
	cancel := b.cancel
	b.mu.Unlock()

	if sessionID != "" {
		_ = b.client.DeleteSession(context.Background(), sessionID)
	}
	if cancel != nil {
		cancel()
	}
	return nil
}
