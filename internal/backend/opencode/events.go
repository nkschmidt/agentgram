package opencode

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/schmidt/agentgram/internal/backend"
	"github.com/schmidt/agentgram/internal/backend/toolfmt"
)

// consume — handler for the SSE stream. Parses each event into busEvent and
// routes it through handleEvent. On channel close, closes out.
func (b *Backend) consume(events <-chan SSEEvent, sessionID string) {
	defer func() {
		b.mu.Lock()
		close(b.out)
		b.mu.Unlock()
	}()

	for ev := range events {
		if len(ev.Data) == 0 {
			continue
		}
		var msg busEvent
		if err := json.Unmarshal(ev.Data, &msg); err != nil {
			log.Printf("opencode: non-JSON event: %s", string(ev.Data))
			continue
		}
		if !msg.belongsToSession(sessionID) {
			continue
		}
		b.handleEvent(msg)
	}
}

func (b *Backend) handleEvent(msg busEvent) {
	switch msg.Type {
	case "message.part.updated":
		b.updatePart(msg.Properties.Part)
	case "session.idle":
		// Model is done. We assemble a "clean final" — only text parts,
		// without tool-call steps. On a Final-chunk with text, StreamCoordinator
		// replaces accumulated progress with this summary (unification with claude).
		finalText := b.collectFinalText()
		b.emit(backend.Chunk{Text: finalText, Final: true})
		b.resetParts()
	case "session.error":
		text := msg.Properties.Error.message()
		if text == "" {
			text = "session error"
		}
		b.emit(backend.Chunk{Text: "⚠ " + text, Final: true})
		b.resetParts()
	}
}

// updatePart updates the "snapshot" of the response and emits it whole with Replace=true.
// part.id (or callID for tool) — stable key; the same part
// may arrive many times with growing text.
func (b *Backend) updatePart(p *partPayload) {
	if p == nil {
		return
	}
	id := p.partID()
	if id == "" {
		return
	}

	content := b.renderPart(p)
	if content == "" {
		return
	}

	b.stateMu.Lock()
	if _, exists := b.parts[id]; !exists {
		b.order = append(b.order, id)
	}
	b.parts[id] = opPart{kind: p.Type, content: content}
	full := b.rebuildLocked()
	b.stateMu.Unlock()

	if full != "" {
		b.emit(backend.Chunk{Text: full, Replace: true})
	}
}

// renderPart turns a part into a string for the UI. Returns "" if the part
// is uninteresting (completed tool — we've already shown running, no need
// to duplicate; a type we don't know how to format — skipped).
//
// Separately filters out a text-part that literally duplicates the user's prompt:
// opencode sends message.part.updated for the user-message (echo) too, and we
// must not add it to the snapshot — otherwise the model's final answer
// gets mixed with the user's own question.
func (b *Backend) renderPart(p *partPayload) string {
	switch p.Type {
	case "text":
		if p.Text == "" {
			return ""
		}
		b.stateMu.Lock()
		isEcho := b.lastSent != "" &&
			strings.TrimSpace(p.Text) == strings.TrimSpace(b.lastSent)
		b.stateMu.Unlock()
		if isEcho {
			return ""
		}
		return p.Text
	case "tool":
		if p.State == nil {
			return ""
		}
		switch p.State.Status {
		case "pending", "running":
			return toolfmt.ToolUse(p.Tool, p.State.Input)
		case "error":
			err := p.State.Error
			if err == "" {
				err = "error"
			}
			return "❌ " + p.Tool + " · " + toolfmt.Truncate(err, 200)
		default:
			return "" // completed — running already shown, don't re-show
		}
	}
	return ""
}

// collectFinalText — model summary without tool steps, for the final chunk.
// If the model didn't produce any text — returns "".
func (b *Backend) collectFinalText() string {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	var sb strings.Builder
	for _, id := range b.order {
		p, ok := b.parts[id]
		if !ok || p.kind != "text" || p.content == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(p.content)
	}
	return sb.String()
}

// rebuildLocked returns all accumulated parts in order of appearance.
// Must be called under stateMu.
func (b *Backend) rebuildLocked() string {
	if len(b.order) == 0 {
		return ""
	}
	out := make([]string, 0, len(b.order))
	for _, id := range b.order {
		if v, ok := b.parts[id]; ok && v.content != "" {
			out = append(out, v.content)
		}
	}
	return strings.Join(out, "\n")
}

func (b *Backend) resetParts() {
	b.stateMu.Lock()
	b.parts = map[string]opPart{}
	b.order = b.order[:0]
	b.stateMu.Unlock()
}

func (b *Backend) emit(chunk backend.Chunk) {
	select {
	case b.out <- chunk:
	default:
		log.Printf("opencode: out channel overflow")
	}
}
