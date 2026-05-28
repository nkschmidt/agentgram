package command

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/schmidt/agentgram/internal/router"
)

// StreamCoordinator ties "Forwarder sent text to backend" and
// "main.onChunk received a response" into one UX: typing indicator + a
// single message in the chat that accumulates intermediate chunks and is
// replaced with the final answer. While the stream is active, an "⏹ Stop"
// button hangs below the message — implements CallbackPrefixHandler for
// the "stream" prefix.
//
// Per-user stream life cycle:
//
//	Begin     — Forwarder calls right after Backend.Send;
//	             typing turns on, state awaits the first chunk.
//	OnChunk   — main.onChunk calls on every chunk; the first chunk
//	             creates the message, the rest edit it. The final one
//	             replaces the accumulated text and closes the stream.
//	Finish    — explicit close (needed on Send error in backend or exit).
//
// InterruptSessionFunc — function "ask the active session to interrupt
// the current work" (Ctrl+C equivalent). Passed as a closure to avoid a
// cyclic dependency on initialization (Manager is created after
// Coordinator).
type InterruptSessionFunc func(userID int64) error

type StreamCoordinator struct {
	replier     Replier
	interruptFn InterruptSessionFunc

	mu     sync.Mutex
	states map[int64]*streamState // userID -> state
}

type streamState struct {
	chatID       int64
	messageID    int    // 0 = not sent yet
	text         string // accumulated text
	typingCancel context.CancelFunc
}

func NewStreamCoordinator(r Replier, interrupt InterruptSessionFunc) *StreamCoordinator {
	return &StreamCoordinator{
		replier:     r,
		interruptFn: interrupt,
		states:      map[int64]*streamState{},
	}
}

// IsActive — does the user have an open stream. Forwarder uses this
// to not send a second request to the backend until the first response arrives.
func (sc *StreamCoordinator) IsActive(userID int64) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	_, ok := sc.states[userID]
	return ok
}

// Begin starts the typing indicator. The message itself appears later,
// when the first chunk arrives.
func (sc *StreamCoordinator) Begin(userID, chatID int64) {
	sc.mu.Lock()
	if old, ok := sc.states[userID]; ok && old.typingCancel != nil {
		old.typingCancel()
	}
	typingCtx, cancel := context.WithCancel(context.Background())
	sc.states[userID] = &streamState{chatID: chatID, typingCancel: cancel}
	sc.mu.Unlock()

	go sc.typingLoop(typingCtx, chatID)
}

func (sc *StreamCoordinator) typingLoop(ctx context.Context, chatID int64) {
	if err := sc.replier.Typing(ctx, chatID); err != nil {
		log.Printf("stream: typing: %v", err)
	}
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := sc.replier.Typing(ctx, chatID); err != nil {
				log.Printf("stream: typing: %v", err)
			}
		}
	}
}

// OnChunk processes a single piece of the backend's response.
//
//	replace=true   → Text REPLACES the accumulated buffer (incremental UI:
//	                 opencode sends updates of the full state).
//	final=true     → stream close. If Text is non-empty — also replaces.
//	otherwise      → Text is appended via \n\n (claude-style append).
//	text=="" && !final → no-op.
func (sc *StreamCoordinator) OnChunk(ctx context.Context, userID, chatID int64, text string, replace, final bool) {
	if text == "" && !final {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()

	state, ok := sc.states[userID]
	if !ok {
		if text != "" {
			if _, err := sc.replier.Send(ctx, chatID, text, nil); err != nil {
				log.Printf("stream: send orphan chunk: %v", err)
			}
		}
		return
	}

	if text != "" {
		switch {
		case final, replace:
			state.text = text
		case state.text == "":
			state.text = text
		default:
			state.text = state.text + "\n" + text
		}
	}

	if state.text != "" {
		var kb InlineKeyboard
		if !final {
			kb = stopKeyboard()
		}
		// On final — try to render the response with HTML formatting
		// (the model often sends **bold**, `code`, etc). If Telegram rejects
		// broken HTML, silently fall back to plain.
		useHTML := final
		var htmlText string
		if useHTML {
			htmlText = MarkdownToHTML(state.text)
		}

		if state.messageID == 0 {
			if useHTML {
				if mid, err := sc.replier.SendHTML(ctx, chatID, htmlText, kb); err == nil {
					state.messageID = mid
				} else {
					log.Printf("stream: html send failed, fallback plain: %v", err)
					if mid, pErr := sc.replier.Send(ctx, chatID, state.text, kb); pErr == nil {
						state.messageID = mid
					} else {
						log.Printf("stream: send: %v", pErr)
						return
					}
				}
			} else {
				mid, err := sc.replier.Send(ctx, chatID, state.text, kb)
				if err != nil {
					log.Printf("stream: send: %v", err)
					return
				}
				state.messageID = mid
			}
		} else {
			if useHTML {
				if err := sc.replier.EditHTML(ctx, chatID, state.messageID, htmlText, kb); err != nil {
					log.Printf("stream: html edit failed, fallback plain: %v", err)
					if pErr := sc.replier.Edit(ctx, chatID, state.messageID, state.text, kb); pErr != nil {
						log.Printf("stream: edit: %v", pErr)
					}
				}
			} else {
				if err := sc.replier.Edit(ctx, chatID, state.messageID, state.text, kb); err != nil {
					log.Printf("stream: edit: %v", err)
				}
			}
		}
	}

	if final {
		if state.typingCancel != nil {
			state.typingCancel()
		}
		delete(sc.states, userID)
	}
}

// Finish — unplanned close of the stream. Stops typing and removes
// the Stop button from the accumulated message (if any).
func (sc *StreamCoordinator) Finish(userID int64) {
	sc.mu.Lock()
	state, ok := sc.states[userID]
	if !ok {
		sc.mu.Unlock()
		return
	}
	if state.typingCancel != nil {
		state.typingCancel()
	}
	chatID := state.chatID
	msgID := state.messageID
	text := state.text
	delete(sc.states, userID)
	sc.mu.Unlock()

	if msgID > 0 {
		_ = sc.replier.Edit(context.Background(), chatID, msgID, text, nil)
	}
}

// ---------- CallbackPrefixHandler: "⏹ Stop" button ----------

func (*StreamCoordinator) Prefix() string { return "stream" }

func (sc *StreamCoordinator) HandleCallback(ctx context.Context, cb router.Callback, r Replier) error {
	parts := strings.Split(cb.Data, ":")
	if len(parts) < 2 || parts[1] != "stop" {
		return r.Answer(ctx, cb.ID, "")
	}

	sc.mu.Lock()
	state, ok := sc.states[cb.UserID]
	if !ok {
		sc.mu.Unlock()
		return r.Answer(ctx, cb.ID, "Already finished")
	}
	if state.typingCancel != nil {
		state.typingCancel()
	}
	chatID := state.chatID
	msgID := state.messageID
	text := state.text
	delete(sc.states, cb.UserID)
	sc.mu.Unlock()

	if msgID > 0 {
		final := text
		if final != "" {
			final += "\n\n"
		}
		final += "⏹ Request interrupted"
		_ = r.Edit(ctx, chatID, msgID, final, nil)
	}

	// Interrupt is launched in a goroutine — it may hang on an HTTP request
	// (if the server doesn't respond to abort), and the UI must reply to the
	// user instantly. The state itself was already cleared above.
	userID := cb.UserID
	go func() {
		if err := sc.interruptFn(userID); err != nil {
			log.Printf("stream: interrupt: %v", err)
		}
	}()

	return r.Answer(ctx, cb.ID, "Interrupted")
}

func stopKeyboard() InlineKeyboard {
	return InlineKeyboard{
		{{Text: "⏹ Stop", Data: "stream:stop"}},
	}
}
