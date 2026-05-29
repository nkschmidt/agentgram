package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os/exec"
	"strings"

	"github.com/schmidt/agentgram/internal/backend"
	"github.com/schmidt/agentgram/internal/backend/toolfmt"
)

// consume parses the process stdout into JSON events and emits chunks.
// On completion clears state and, if necessary, closes out (if Stop was called).
func (b *Backend) consume(ctx context.Context, cmd *exec.Cmd, cancel context.CancelFunc, stdout, stderr io.Reader) {
	defer cancel()

	// stderr is read in parallel — logged; to avoid blocking the process pipe.
	doneStderr := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" {
				log.Printf("claude stderr: %s", line)
			}
		}
		close(doneStderr)
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			log.Printf("claude: non-JSON line: %s", line)
			continue
		}
		b.handleEvent(ev)
	}

	<-doneStderr
	waitErr := cmd.Wait()

	b.mu.Lock()
	b.cmd = nil
	b.cancel = nil
	shouldClose := b.stopped
	b.mu.Unlock()

	// If we cancelled the ctx ourselves (Interrupt/Stop) — silent. Otherwise the
	// process exit error is a real problem, propagate it into the chat.
	if waitErr != nil && !errors.Is(ctx.Err(), context.Canceled) {
		b.emit(backend.Chunk{Err: waitErr})
	}

	if shouldClose {
		close(b.out)
	}
}

func (b *Backend) handleEvent(ev event) {
	// session_id may arrive in init and result events — save it,
	// so the next Send continues the same conversation via --resume.
	if ev.SessionID != "" {
		b.mu.Lock()
		b.sessionID = ev.SessionID
		b.mu.Unlock()
	}

	switch ev.Type {
	case "assistant":
		b.emitAssistant(ev.Message)
	case "result":
		if ev.Result != "" {
			b.emit(backend.Chunk{Text: ev.Result, Final: true})
		}
	case "system":
		// init — internal (model, config), not needed in chat
	case "user":
		// tool_result from the system back to the model — technical step
	}
}

func (b *Backend) emitAssistant(m *eventMessage) {
	if m == nil {
		return
	}
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				b.emit(backend.Chunk{Text: c.Text})
			}
		case "tool_use":
			if toolfmt.Internal(c.Name) {
				continue // bot's own MCP tools have their own chat UI; don't show the step
			}
			b.emit(backend.Chunk{Text: toolfmt.ToolUse(c.Name, c.Input)})
		case "thinking":
			if c.Thinking != "" {
				b.emit(backend.Chunk{Text: "💭 " + c.Thinking})
			}
		}
	}
}

func (b *Backend) emit(chunk backend.Chunk) {
	select {
	case b.out <- chunk:
	default:
		log.Printf("claude: out channel overflow")
	}
}
