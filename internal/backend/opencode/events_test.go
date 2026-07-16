package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/schmidt/agentgram/internal/backend"
)

func newTestBackend() *Backend {
	return &Backend{
		out:   make(chan backend.Chunk, 64),
		parts: map[string]opPart{},
		asked: map[string]bool{},
	}
}

func textPart(id, text string) *partPayload {
	return &partPayload{ID: id, Type: "text", Text: text}
}
func toolPart(id, tool, input string) *partPayload {
	return &partPayload{ID: id, Type: "tool", Tool: tool, State: &toolState{Status: "running", Input: json.RawMessage(input)}}
}

func drain(b *Backend) []backend.Chunk {
	var out []backend.Chunk
	for {
		select {
		case c := <-b.out:
			out = append(out, c)
		default:
			return out
		}
	}
}

// Prose and steps are emitted per-segment: an ask_user part is a boundary (it
// emits no chunk — the coordinator shows the question at tool-execution time),
// so prose after it starts a fresh segment, not concatenated with the prior.
func TestOpencodeSegmentation(t *testing.T) {
	b := newTestBackend()

	b.updatePart(textPart("t1", "first explanation"))
	b.updatePart(toolPart("x1", "read", `{"file_path":"a.go"}`))
	b.updatePart(toolPart("q1", "agentgram_ask_user", `{"question":"Pick","options":["A","B"]}`))
	b.updatePart(textPart("t2", "second explanation"))

	got := drain(b)
	if len(got) != 3 {
		t.Fatalf("want 3 chunks (prose, activity, prose), got %d: %+v", len(got), got)
	}
	if got[0].Kind != backend.KindProse || got[0].Text != "first explanation" {
		t.Fatalf("chunk0: want prose 'first explanation', got %+v", got[0])
	}
	if got[1].Kind != backend.KindActivity || !strings.Contains(got[1].Text, "read") {
		t.Fatalf("chunk1: want activity read, got %+v", got[1])
	}
	// Post-question prose must be only the new segment, not "first…\nsecond…".
	if got[2].Kind != backend.KindProse || got[2].Text != "second explanation" {
		t.Fatalf("chunk2: want prose 'second explanation' (new segment), got %+v", got[2])
	}
}

// send_* and ask_user tool parts emit no activity step.
func TestOpencodeSkipsInternalTools(t *testing.T) {
	b := newTestBackend()

	b.updatePart(toolPart("s1", "agentgram_send_photo", `{"path":"x.png"}`))
	q := toolPart("q1", "agentgram_ask_user", `{"question":"Q","options":["Y"]}`)
	b.updatePart(q)
	b.updatePart(q) // same id again — still nothing

	if got := drain(b); len(got) != 0 {
		t.Fatalf("want no chunks from internal tools, got %+v", got)
	}
}
