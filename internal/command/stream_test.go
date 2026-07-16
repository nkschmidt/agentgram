package command

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/schmidt/agentgram/internal/backend"
)

// fakeReplier records message operations and hands out incrementing IDs.
type fakeReplier struct {
	mu      sync.Mutex
	nextID  int
	text    map[int]string // last text per message id
	html    map[int]bool   // last write to id was HTML
	deleted map[int]bool
}

func newFakeReplier() *fakeReplier {
	return &fakeReplier{text: map[int]string{}, html: map[int]bool{}, deleted: map[int]bool{}}
}

func (f *fakeReplier) send(text string, html bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.text[f.nextID] = text
	f.html[f.nextID] = html
	return f.nextID
}
func (f *fakeReplier) Send(_ context.Context, _ int64, t string, _ InlineKeyboard) (int, error) {
	return f.send(t, false), nil
}
func (f *fakeReplier) SendHTML(_ context.Context, _ int64, t string, _ InlineKeyboard) (int, error) {
	return f.send(t, true), nil
}
func (f *fakeReplier) Edit(_ context.Context, _ int64, id int, t string, _ InlineKeyboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text[id], f.html[id] = t, false
	return nil
}
func (f *fakeReplier) EditHTML(_ context.Context, _ int64, id int, t string, _ InlineKeyboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text[id], f.html[id] = t, true
	return nil
}
func (f *fakeReplier) EditMarkup(context.Context, int64, int, InlineKeyboard) error { return nil }
func (f *fakeReplier) Delete(_ context.Context, _ int64, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted[id] = true
	return nil
}
func (f *fakeReplier) Answer(context.Context, string, string) error { return nil }
func (f *fakeReplier) Typing(context.Context, int64) error          { return nil }

func (f *fakeReplier) get(id int) (string, bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text[id], f.html[id], f.deleted[id]
}

func newCoord(r Replier) *StreamCoordinator {
	return NewStreamCoordinator(r, func(int64) error { return nil })
}

// Activity goes to an ephemeral message that is removed on End; prose is sealed
// into its own message as HTML.
func TestProseSealedActivityRemoved(t *testing.T) {
	fr := newFakeReplier()
	sc := newCoord(fr)
	ctx := context.Background()

	sc.Begin(1, 100) // activity message id=1 ("⏳ Working…")
	sc.OnChunk(ctx, 1, 100, backend.Chunk{Kind: backend.KindActivity, Text: "📖 Read · x"})
	sc.OnChunk(ctx, 1, 100, backend.Chunk{Kind: backend.KindProse, Text: "The answer."})
	sc.OnChunk(ctx, 1, 100, backend.Chunk{Kind: backend.KindEnd})

	if _, _, del := fr.get(1); !del {
		t.Fatal("activity message should be deleted at end")
	}
	txt, html, _ := fr.get(2)
	if !strings.Contains(txt, "The answer.") {
		t.Fatalf("prose message should hold the answer, got %q", txt)
	}
	if !html {
		t.Fatal("prose should be sealed as HTML")
	}
	if sc.IsActive(1) {
		t.Fatal("stream should be closed after End")
	}
}

// WaitAnswer (run when the agent executes ask_user) seals prior prose, removes
// activity, posts the question below, and returns the tapped answer — reflected
// in the question message.
func TestQuestionAnswerRoundTrip(t *testing.T) {
	fr := newFakeReplier()
	sc := newCoord(fr)
	ctx := context.Background()

	sc.Begin(1, 100)                                                                     // activity id=1
	sc.OnChunk(ctx, 1, 100, backend.Chunk{Kind: backend.KindProse, Text: "Let me ask."}) // prose id=2

	got := make(chan string, 1)
	go func() {
		ans, _ := sc.WaitAnswer(ctx, 1, 100, "Pick one", []string{"A", "B"})
		got <- ans
	}()
	// Question message (id=3) appears, then tap option B.
	waitFor(t, func() bool { txt, _, _ := fr.get(3); return strings.Contains(txt, "Pick one") })
	if _, _, del := fr.get(1); !del {
		t.Fatal("activity should be removed when the question is shown")
	}
	if _, html, _ := fr.get(2); !html {
		t.Fatal("prose before the question should be sealed (HTML)")
	}
	sc.TryAnswerCallback(1, "ans:1")

	if ans := <-got; ans != "B" {
		t.Fatalf("want answer B, got %q", ans)
	}
	if txt, _, _ := fr.get(3); !strings.Contains(txt, "✅ B") {
		t.Fatalf("question message should show the chosen answer, got %q", txt)
	}
}

// A typed reply also answers the question.
func TestQuestionTextAnswer(t *testing.T) {
	fr := newFakeReplier()
	sc := newCoord(fr)
	ctx := context.Background()

	sc.Begin(1, 100)
	got := make(chan string, 1)
	go func() {
		ans, _ := sc.WaitAnswer(ctx, 1, 100, "Name?", nil)
		got <- ans
	}()
	waitFor(t, func() bool { txt, _, _ := fr.get(2); return strings.Contains(txt, "Name?") })
	if !sc.TryAnswerText(1, "Biome") {
		t.Fatal("typed reply should be consumed as the answer")
	}
	if ans := <-got; ans != "Biome" {
		t.Fatalf("want Biome, got %q", ans)
	}
}

// Prose that streams in while a question is pending is folded into the question
// card (above the question), not posted as a separate message below it — so it
// reads in order even though opencode emits the ask_user call before the text.
func TestQuestionFoldsProse(t *testing.T) {
	fr := newFakeReplier()
	sc := newCoord(fr)
	ctx := context.Background()

	sc.Begin(1, 100) // activity id=1 (deleted when the question shows)
	go func() { _, _ = sc.WaitAnswer(ctx, 1, 100, "Pick?", []string{"A"}) }()
	waitFor(t, func() bool { txt, _, _ := fr.get(2); return strings.Contains(txt, "Pick?") }) // question id=2

	sc.OnChunk(ctx, 1, 100, backend.Chunk{Kind: backend.KindProse, Text: "Here is context."})

	if txt, _, _ := fr.get(3); txt != "" {
		t.Fatalf("prose during a question must fold in, not create msg3; got %q", txt)
	}
	txt, _, _ := fr.get(2)
	if !strings.Contains(txt, "Here is context.") || !strings.Contains(txt, "Pick?") {
		t.Fatalf("question card should hold prose + question, got %q", txt)
	}
	sc.TryAnswerCallback(1, "ans:0")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
