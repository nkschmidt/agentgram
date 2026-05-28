// Package claude — backend adapter for the `claude` CLI (Claude Code).
//
// Architecturally: `claude -p` is designed NOT as a long-lived process with
// a stdin stream, but as a per-message call. Multi-turn is implemented via
// `--resume <session_id>`. session_id is issued by claude itself in
// `system.init` and `result` events — we store it inside Backend and
// substitute it on the next Send.
//
// Life cycle:
//
//	Start(ctx)    — creates the out channel, doesn't launch anything.
//	Send(input)   — launches a new `claude -p` process (stdin = input);
//	                a goroutine parses stdout (stream-JSON) and sends chunks to out.
//	                Finishes when the process exited cleanly.
//	Interrupt()   — sends SIGINT to the current process (if running).
//	                session_id is preserved → next Send continues the conversation.
//	Stop()        — mark as closed, cancel the process, close out.
//
// Event parsing — in events.go, JSON models — in models.go.
package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/schmidt/agentgram/internal/backend"
)

const Name = "claude"

type Backend struct {
	workDir func() string // current working directory, read on every Send

	mu        sync.Mutex
	sessionID string             // issued by claude itself, used for --resume
	cmd       *exec.Cmd          // current process (nil between requests)
	cancel    context.CancelFunc // cancel for the current process's ctx
	rootCtx   context.Context    // from Start, parent for all per-message ctxs
	out       chan backend.Chunk // long-lived channel
	stopped   bool
}

// New returns a factory. provider is read on every process launch for
// the given userID — each user can have their own working directory.
func New(provider func(userID int64) string) backend.Factory {
	return func(userID int64) backend.Backend {
		return &Backend{
			workDir: func() string { return provider(userID) },
		}
	}
}

func (b *Backend) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.out != nil {
		return errors.New("claude: already started")
	}
	b.rootCtx = ctx
	b.out = make(chan backend.Chunk, 64)
	return nil
}

func (b *Backend) Send(input string) error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return errors.New("claude: stopped")
	}
	if b.cmd != nil {
		b.mu.Unlock()
		return errors.New("claude: previous request still running")
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
	}
	if b.sessionID != "" {
		args = append(args, "--resume", b.sessionID)
	}

	ctx, cancel := context.WithCancel(b.rootCtx)
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader(input)
	if b.workDir != nil {
		if wd := b.workDir(); wd != "" {
			cmd.Dir = wd
		}
	}
	// cmd.Cancel is invoked when ctx is cancelled. Default — Process.Kill (SIGKILL).
	// We need SIGINT (like Ctrl+C): claude gets a chance to gracefully finish
	// the current generation and persist session state.
	cmd.Cancel = func() error {
		return cmd.Process.Signal(os.Interrupt)
	}
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		b.mu.Unlock()
		return fmt.Errorf("claude stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		b.mu.Unlock()
		return fmt.Errorf("claude stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		b.mu.Unlock()
		return fmt.Errorf("claude start: %w", err)
	}

	b.cmd = cmd
	b.cancel = cancel
	b.mu.Unlock()

	go b.consume(ctx, cmd, cancel, stdout, stderr)
	return nil
}

func (b *Backend) Recv() <-chan backend.Chunk { return b.out }

// Interrupt cancels the current process's ctx — this invokes cmd.Cancel,
// which sends SIGINT. session_id is preserved, next Send continues the conversation.
func (b *Backend) Interrupt() error {
	b.mu.Lock()
	cancel := b.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Stop fully closes the backend. The current process (if any) is cancelled,
// out channel is closed (in consume or right here, if no process is running).
func (b *Backend) Stop() error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return nil
	}
	b.stopped = true
	cancel := b.cancel
	hasActive := b.cmd != nil
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	// If there's no active process — consume won't run, close here ourselves.
	if !hasActive && b.out != nil {
		close(b.out)
	}
	return nil
}
