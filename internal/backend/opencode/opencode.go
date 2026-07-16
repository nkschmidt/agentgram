package opencode

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/schmidt/agentgram/internal/backend"
)

// Backend — backend.Backend implementation on top of the opencode HTTP API.
// Each Backend holds its own session on the server and its own SSE subscription.
// The server itself is started lazily via LazyServer on the first Start.
//
// Events are parsed in events.go, JSON models — in models.go.
type Backend struct {
	lazy      *LazyServer
	workDir   func() string
	mcpConfig func() []byte // bot's MCP server as an opencode.json doc (may be nil)
	system    string        // per-message system instruction (MCP tool guidance)
	client    *Client

	mu        sync.Mutex
	sessionID string
	out       chan backend.Chunk
	cancel    context.CancelFunc
	sessCtx   context.Context // cancelled on Stop; aborts the blocking POST /message
	stopped   bool

	// Current "assembly" of the model's response to the current user request.
	// Cleared on session.idle / session.error.
	// lastSent stores the prompt of the last Send — opencode echoes it back
	// via message.part.updated (for user-message); we filter such parts out,
	// otherwise they end up in the model's final answer.
	stateMu    sync.Mutex
	parts      map[string]opPart // id → part (text / tool / question)
	order      []string          // order of part appearance
	lastSent   string            // text of the last Send (for echo filter)
	sealedUpTo int               // parts before this index belong to an already-sealed segment
	asked      map[string]bool   // part ids for which a KindQuestion was already emitted
}

// opPart — unit of the accumulated snapshot. kind distinguishes the model's
// prose ("text") from tool steps ("tool") and ask_user calls ("question");
// prose and steps are rendered per-segment (parts since the last seal).
type opPart struct {
	kind    string // "text" | "tool" | "question"
	content string // text ready for display
}

// New returns a Backend factory. provider is read on every SendMessage
// for the given userID — each user has their own working directory.
//
// mcpConfig, if non-nil, returns the opencode.json document (for the given
// user) that registers the bot's MCP server. Since one `opencode serve` is
// shared by all users, the only per-user hook is the project config in each
// user's working directory — we write it on Start so opencode picks it up when
// it resolves the session's directory.
//
// systemPrompt, if non-empty, is sent as the per-message `system` instruction
// so the agent knows to use the bot's MCP tools (ask_user / send_*).
func New(lazy *LazyServer, provider func(userID int64) string, mcpConfig func(userID int64) []byte, systemPrompt string) backend.Factory {
	return func(userID int64) backend.Backend {
		b := &Backend{
			lazy:    lazy,
			workDir: func() string { return provider(userID) },
			system:  systemPrompt,
			parts:   map[string]opPart{},
			asked:   map[string]bool{},
		}
		if mcpConfig != nil {
			b.mcpConfig = func() []byte { return mcpConfig(userID) }
		}
		return b
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
	b.sessCtx = subCtx
	b.mu.Unlock()

	// Write the per-user MCP config into the working directory before creating
	// the session, so opencode picks it up while resolving the directory.
	if b.mcpConfig != nil {
		if wd := b.workDir(); wd != "" {
			if err := writeMCPConfig(wd, b.mcpConfig()); err != nil {
				log.Printf("opencode: mcp config: %v", err)
			}
		}
	}

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
	sessCtx := b.sessCtx
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
	// POST /message blocks for the whole turn; sessCtx (cancelled on Stop) lets a
	// session switch abort the in-flight request instead of hanging or erroring late.
	if sessCtx == nil {
		sessCtx = context.Background()
	}
	return b.client.SendMessage(sessCtx, sessionID, input, b.system)
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
