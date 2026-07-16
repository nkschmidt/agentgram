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
		// Model is done. Prose already streamed via KindProse — just seal.
		b.emit(backend.Chunk{Kind: backend.KindEnd})
		b.resetParts()
	case "session.error":
		text := msg.Properties.Error.message()
		if text == "" {
			text = "session error"
		}
		b.emit(backend.Chunk{Kind: backend.KindProse, Text: "⚠ " + text, Replace: true})
		b.emit(backend.Chunk{Kind: backend.KindEnd})
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

	switch p.Type {
	case "text":
		if p.Text == "" {
			return
		}
		b.stateMu.Lock()
		isEcho := b.lastSent != "" && strings.TrimSpace(p.Text) == strings.TrimSpace(b.lastSent)
		if isEcho {
			b.stateMu.Unlock()
			return
		}
		b.storeLocked(id, opPart{kind: "text", content: p.Text})
		prose := b.segmentLocked("text")
		b.stateMu.Unlock()
		if prose != "" {
			b.emit(backend.Chunk{Kind: backend.KindProse, Text: prose, Replace: true})
		}

	case "tool":
		if p.State == nil {
			return
		}
		if toolfmt.IsAskUser(p.Tool) {
			b.markQuestionBoundary(id, p)
			return
		}
		if toolfmt.Internal(p.Tool) {
			return // send_* — its effect is the sent file, no step
		}
		content := renderToolStep(p)
		if content == "" {
			return
		}
		b.stateMu.Lock()
		b.storeLocked(id, opPart{kind: "tool", content: content})
		steps := b.segmentLocked("tool")
		b.stateMu.Unlock()
		if steps != "" {
			b.emit(backend.Chunk{Kind: backend.KindActivity, Text: steps, Replace: true})
		}
	}
}

// markQuestionBoundary seals the current segment when an ask_user part appears
// with a complete input, so prose/steps produced after the user answers start a
// fresh segment (not concatenated with the pre-question prose). The question
// itself is displayed by the coordinator when the tool executes (WaitAnswer),
// which is correctly ordered after the prose — so nothing is emitted here.
func (b *Backend) markQuestionBoundary(id string, p *partPayload) {
	q, _ := toolfmt.AskInput(p.State.Input)
	if q == "" {
		return // input not complete yet
	}
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	if b.asked[id] {
		return
	}
	b.asked[id] = true
	b.storeLocked(id, opPart{kind: "question"})
	b.sealedUpTo = len(b.order)
}

// renderToolStep formats a tool part as an activity step ("" to skip a
// completed tool — its running state was already shown — or an empty one).
//
// The text-part echo filter (opencode echoes the user's own prompt back as a
// message.part) lives in updatePart.
func renderToolStep(p *partPayload) string {
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
		return ""
	}
}

// storeLocked records a part, appending its id to order on first sight.
func (b *Backend) storeLocked(id string, part opPart) {
	if _, exists := b.parts[id]; !exists {
		b.order = append(b.order, id)
	}
	b.parts[id] = part
}

// segmentLocked joins the content of parts of the given kind since the last
// seal (the current segment). Must be called under stateMu.
func (b *Backend) segmentLocked(kind string) string {
	out := make([]string, 0, len(b.order))
	for _, id := range b.order[b.sealedUpTo:] {
		if v, ok := b.parts[id]; ok && v.kind == kind && v.content != "" {
			out = append(out, v.content)
		}
	}
	return strings.Join(out, "\n")
}

func (b *Backend) resetParts() {
	b.stateMu.Lock()
	b.parts = map[string]opPart{}
	b.order = b.order[:0]
	b.sealedUpTo = 0
	b.asked = map[string]bool{}
	b.stateMu.Unlock()
}

func (b *Backend) emit(chunk backend.Chunk) {
	select {
	case b.out <- chunk:
	default:
		log.Printf("opencode: out channel overflow")
	}
}
