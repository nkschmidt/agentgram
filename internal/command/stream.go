package command

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/schmidt/agentgram/internal/backend"
	"github.com/schmidt/agentgram/internal/router"
)

// StreamCoordinator renders a backend turn into chat. The backend emits a
// single ordered stream of typed chunks (see backend.ChunkKind); this is the
// only place that turns them into Telegram messages. Three roles:
//
//   - Activity (KindActivity): an ephemeral "⏳" message with the agent's tool
//     steps; capped, removed at each boundary (question / end of turn).
//   - Prose (KindProse): the agent's words — persistent messages, shown live,
//     split across messages when longer than Telegram allows, and "sealed"
//     (rendered as HTML, no longer edited) at each boundary so the next prose
//     starts a new message below.
//   - Question (KindQuestion): an ask_user call — a message with inline option
//     buttons. The answer is returned to the blocked agent via WaitAnswer.
//
// The ⏹ Stop button rides on whichever message is currently being written
// (the "live" message), moved with EditMarkup.
type StreamCoordinator struct {
	replier     Replier
	interruptFn InterruptSessionFunc

	mu     sync.Mutex
	states map[int64]*streamState // userID -> render state

	qmu       sync.Mutex
	questions map[int64]*pendingQuestion // userID -> in-flight ask_user
}

// InterruptSessionFunc asks the active session to interrupt current work
// (Ctrl+C equivalent). A closure to avoid a cyclic init dependency on Manager.
type InterruptSessionFunc func(userID int64) error

const (
	maxStepRunes   = 3500     // activity message cap, under Telegram's 4096
	maxProseRunes  = 4000     // per prose message; longer prose spans several
	askTimeout     = 10 * time.Minute
	answerCallback = "ans:" // inline-button callback prefix for ask_user answers
)

type streamState struct {
	chatID       int64
	typingCancel context.CancelFunc

	activityID int    // ephemeral steps message; 0 = none
	steps      string // accumulated steps (capped)

	proseText  string // current (unsealed) prose segment, full
	shownRunes int    // runes already placed in finalized prose messages
	proseID    int    // current growing prose message; 0 = none

	liveID int // message currently holding the ⏹ Stop button; 0 = none
}

type pendingQuestion struct {
	chatID   int64
	msgID    int    // 0 until the question message is displayed
	question string
	options  []string
	prose    string      // agent's lead-in that streams in after the tool call
	answer   chan string // buffered(1); first tap/reply wins
	answered bool
}

// render builds the question card text: the lead-in prose (if any) above the
// question. Folding the prose in keeps it correctly ordered regardless of
// when it streams in relative to the tool call (opencode executes ask_user
// before flushing the surrounding text).
func (pq *pendingQuestion) render() string {
	if pq.prose == "" {
		return "❓ " + pq.question
	}
	return pq.prose + "\n\n❓ " + pq.question
}

func NewStreamCoordinator(r Replier, interrupt InterruptSessionFunc) *StreamCoordinator {
	return &StreamCoordinator{
		replier:     r,
		interruptFn: interrupt,
		states:      map[int64]*streamState{},
		questions:   map[int64]*pendingQuestion{},
	}
}

// IsActive — does the user have an open stream. Forwarder uses this to reject a
// second request until the first response arrives.
func (sc *StreamCoordinator) IsActive(userID int64) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	_, ok := sc.states[userID]
	return ok
}

// Begin starts the typing indicator and posts the initial "⏳ Working…"
// activity message (with the Stop button).
func (sc *StreamCoordinator) Begin(userID, chatID int64) {
	sc.mu.Lock()
	if old, ok := sc.states[userID]; ok && old.typingCancel != nil {
		old.typingCancel()
	}
	typingCtx, cancel := context.WithCancel(context.Background())
	st := &streamState{chatID: chatID, typingCancel: cancel}
	sc.states[userID] = st
	sc.updateActivity(st, "", false)
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

// OnChunk renders one ordered chunk of the current turn.
func (sc *StreamCoordinator) OnChunk(ctx context.Context, userID, chatID int64, c backend.Chunk) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	st := sc.states[userID]
	if st == nil {
		st = &streamState{chatID: chatID}
		sc.states[userID] = st
	}

	// While a question is awaiting an answer, the agent's text belongs to that
	// question's lead-in (opencode emits the ask_user call before the text), so
	// fold prose into the question card above the question rather than letting
	// it land in a separate message below. Activity during the wait is noise.
	if sc.foldIntoQuestion(userID, c) {
		return
	}

	switch c.Kind {
	case backend.KindActivity:
		if c.Text == "" {
			return
		}
		sc.updateActivity(st, c.Text, c.Replace)

	case backend.KindProse:
		if c.Replace {
			st.proseText = c.Text
		} else if c.Text != "" {
			if st.proseText == "" {
				st.proseText = c.Text
			} else {
				st.proseText += "\n" + c.Text
			}
		}
		sc.renderProse(st)

	case backend.KindEnd:
		sc.sealProse(st)
		sc.deleteActivity(st)
		if st.typingCancel != nil {
			st.typingCancel()
		}
		delete(sc.states, userID)
	}
}

// updateActivity writes the steps into the ephemeral activity message and makes
// it the live (Stop-bearing) message.
func (sc *StreamCoordinator) updateActivity(st *streamState, text string, replace bool) {
	if text != "" {
		if replace || st.steps == "" {
			st.steps = capRunes(text, maxStepRunes)
		} else {
			st.steps = capRunes(st.steps+"\n"+text, maxStepRunes)
		}
	}
	display := st.steps
	if display == "" {
		display = "⏳ Working…"
	}
	sc.writeLive(st, &st.activityID, display)
}

// renderProse shows the current prose segment, splitting it across messages of
// at most maxProseRunes. Earlier (full) messages are finalized without the Stop
// button; the last (growing) message is the live one.
func (sc *StreamCoordinator) renderProse(st *streamState) {
	r := []rune(st.proseText)
	for len(r)-st.shownRunes > maxProseRunes {
		head := string(r[st.shownRunes : st.shownRunes+maxProseRunes])
		if st.proseID == 0 {
			id, err := sc.replier.Send(context.Background(), st.chatID, head, nil)
			if err != nil {
				log.Printf("stream: prose send: %v", err)
				return
			}
			st.proseID = id
		} else {
			_ = sc.replier.Edit(context.Background(), st.chatID, st.proseID, head, nil)
		}
		if st.liveID == st.proseID {
			st.liveID = 0
		}
		st.shownRunes += maxProseRunes
		st.proseID = 0 // next overflow / tail starts a new message
	}
	tail := string(r[st.shownRunes:])
	if tail != "" {
		sc.writeLive(st, &st.proseID, tail)
	}
}

// sealProse finalizes the current prose segment: the live (tail) message is
// re-rendered as HTML (plain fallback) and loses the Stop button; state resets
// so the next prose begins a new message.
func (sc *StreamCoordinator) sealProse(st *streamState) {
	if st.proseID != 0 {
		tail := string([]rune(st.proseText)[st.shownRunes:])
		if err := sc.replier.EditHTML(context.Background(), st.chatID, st.proseID, MarkdownToHTML(tail), nil); err != nil {
			_ = sc.replier.Edit(context.Background(), st.chatID, st.proseID, tail, nil)
		}
		if st.liveID == st.proseID {
			st.liveID = 0
		}
	}
	st.proseText = ""
	st.shownRunes = 0
	st.proseID = 0
}

func (sc *StreamCoordinator) deleteActivity(st *streamState) {
	if st.activityID == 0 {
		return
	}
	_ = sc.replier.Delete(context.Background(), st.chatID, st.activityID)
	if st.liveID == st.activityID {
		st.liveID = 0
	}
	st.activityID = 0
	st.steps = ""
}

// writeLive sends (if *id==0) or edits the message to text with the Stop button
// and makes it the live message, clearing the button from the previous one.
func (sc *StreamCoordinator) writeLive(st *streamState, id *int, text string) {
	if *id == 0 {
		newID, err := sc.replier.Send(context.Background(), st.chatID, text, stopKeyboard())
		if err != nil {
			log.Printf("stream: send: %v", err)
			return
		}
		*id = newID
	} else {
		_ = sc.replier.Edit(context.Background(), st.chatID, *id, text, stopKeyboard())
	}
	sc.setLive(st, *id)
}

// setLive moves the Stop button onto message id (already written with it),
// clearing it from the previously live message.
func (sc *StreamCoordinator) setLive(st *streamState, id int) {
	if st.liveID == id {
		return
	}
	if st.liveID != 0 {
		_ = sc.replier.EditMarkup(context.Background(), st.chatID, st.liveID, nil)
	}
	st.liveID = id
}

// ---------- ask_user ----------

// WaitAnswer is the ask_user round-trip, called by the MCP tool handler when
// the agent executes ask_user — which happens after the agent produced the
// preceding prose, so displaying the question here keeps it below that prose.
// It seals the current prose, removes the activity message, posts the question
// with option buttons, and blocks until the user taps/types an answer (or ctx
// is cancelled / it times out).
func (sc *StreamCoordinator) WaitAnswer(ctx context.Context, userID, chatID int64, question string, options []string) (string, error) {
	sc.mu.Lock()
	if st := sc.states[userID]; st != nil {
		sc.sealProse(st)
		sc.deleteActivity(st)
		if chatID == 0 {
			chatID = st.chatID
		}
	}
	sc.mu.Unlock()

	// Register before sending so a fast tap isn't lost between the two.
	pq := &pendingQuestion{chatID: chatID, question: question, options: options, answer: make(chan string, 1)}
	sc.qmu.Lock()
	sc.questions[userID] = pq
	sc.qmu.Unlock()
	defer func() {
		sc.qmu.Lock()
		delete(sc.questions, userID)
		sc.qmu.Unlock()
	}()

	msgID, err := sc.replier.Send(ctx, chatID, pq.render(), askKeyboard(options))
	if err != nil {
		return "", err
	}
	sc.qmu.Lock()
	pq.msgID = msgID
	sc.qmu.Unlock()

	select {
	case ans := <-pq.answer:
		return ans, nil
	case <-ctx.Done():
		sc.closeQuestion(pq, "")
		return "", ctx.Err()
	case <-time.After(askTimeout):
		sc.closeQuestion(pq, "")
		return "", errors.New("no response from user in time")
	}
}

// closeQuestion removes the buttons from an unanswered question (cancel/timeout).
func (sc *StreamCoordinator) closeQuestion(pq *pendingQuestion, _ string) {
	sc.qmu.Lock()
	if pq.answered || pq.msgID == 0 {
		sc.qmu.Unlock()
		return
	}
	pq.answered = true
	chatID, msgID, text := pq.chatID, pq.msgID, pq.render()
	sc.qmu.Unlock()
	_ = sc.replier.Edit(context.Background(), chatID, msgID, text+"\n\n⨯ no answer", nil)
}

// foldIntoQuestion routes chunks that arrive while a question is awaiting an
// answer: prose is folded into the question card (above the question, so it
// reads in order even though opencode emits the ask_user call first); activity
// is suppressed. Returns true if the chunk was handled here.
func (sc *StreamCoordinator) foldIntoQuestion(userID int64, c backend.Chunk) bool {
	if c.Kind != backend.KindProse && c.Kind != backend.KindActivity {
		return false
	}
	sc.qmu.Lock()
	pq := sc.questions[userID]
	if pq == nil || pq.msgID == 0 {
		sc.qmu.Unlock()
		return false
	}
	if c.Kind == backend.KindActivity {
		sc.qmu.Unlock()
		return true // suppress activity while a question is shown
	}
	if c.Replace || pq.prose == "" {
		pq.prose = capRunes(c.Text, maxStepRunes)
	} else if c.Text != "" {
		pq.prose = capRunes(pq.prose+"\n"+c.Text, maxStepRunes)
	}
	chatID, msgID, text, opts := pq.chatID, pq.msgID, pq.render(), pq.options
	sc.qmu.Unlock()
	_ = sc.replier.Edit(context.Background(), chatID, msgID, text, askKeyboard(opts))
	return true
}

// TryAnswerText delivers a typed reply to the displayed question. Returns true
// if a question was awaiting (so the caller must not forward the text).
func (sc *StreamCoordinator) TryAnswerText(userID int64, text string) bool {
	sc.qmu.Lock()
	pq := sc.questions[userID]
	displayed := pq != nil && pq.msgID != 0
	sc.qmu.Unlock()
	if !displayed {
		return false
	}
	if text != "" {
		sc.answerQuestion(pq, text)
	}
	return true
}

// TryAnswerCallback handles an "ans:<idx>" button tap. Returns true if the data
// is an answer callback (ours).
func (sc *StreamCoordinator) TryAnswerCallback(userID int64, data string) bool {
	if !strings.HasPrefix(data, answerCallback) {
		return false
	}
	sc.qmu.Lock()
	pq := sc.questions[userID]
	sc.qmu.Unlock()
	if pq != nil {
		if idx, err := strconv.Atoi(strings.TrimPrefix(data, answerCallback)); err == nil && idx >= 0 && idx < len(pq.options) {
			sc.answerQuestion(pq, pq.options[idx])
		}
	}
	return true
}

// answerQuestion delivers the answer to the waiting agent and marks the
// question message as answered.
func (sc *StreamCoordinator) answerQuestion(pq *pendingQuestion, answer string) {
	sc.qmu.Lock()
	if pq.answered {
		sc.qmu.Unlock()
		return
	}
	pq.answered = true
	chatID, msgID, text := pq.chatID, pq.msgID, pq.render()
	select {
	case pq.answer <- answer:
	default:
	}
	sc.qmu.Unlock()

	if msgID != 0 {
		_ = sc.replier.Edit(context.Background(), chatID, msgID, text+"\n\n✅ "+answer, nil)
	}
}

func askKeyboard(options []string) InlineKeyboard {
	if len(options) == 0 {
		return nil
	}
	kb := make(InlineKeyboard, 0, len(options))
	for i, opt := range options {
		kb = append(kb, []InlineButton{{Text: opt, Data: answerCallback + strconv.Itoa(i)}})
	}
	return kb
}

// ---------- unplanned close / interrupt ----------

// Finish closes the stream without an answer (Send error / process exit). Keeps
// any prose, removes the activity message.
func (sc *StreamCoordinator) Finish(userID int64) {
	sc.mu.Lock()
	st, ok := sc.states[userID]
	if !ok {
		sc.mu.Unlock()
		return
	}
	if st.typingCancel != nil {
		st.typingCancel()
	}
	sc.deleteActivity(st)
	sc.sealProse(st)
	delete(sc.states, userID)
	sc.mu.Unlock()
}

// ---------- CallbackPrefixHandler: "⏹ Stop" button ----------

func (*StreamCoordinator) Prefix() string { return "stream" }

func (sc *StreamCoordinator) HandleCallback(ctx context.Context, cb router.Callback, r Replier) error {
	parts := strings.Split(cb.Data, ":")
	if len(parts) < 2 || parts[1] != "stop" {
		return r.Answer(ctx, cb.ID, "")
	}

	sc.mu.Lock()
	st, ok := sc.states[cb.UserID]
	if !ok {
		sc.mu.Unlock()
		return r.Answer(ctx, cb.ID, "Already finished")
	}
	if st.typingCancel != nil {
		st.typingCancel()
	}
	chatID := st.chatID
	sc.deleteActivity(st)
	sc.sealProse(st)
	delete(sc.states, cb.UserID)
	sc.mu.Unlock()

	_, _ = r.Send(ctx, chatID, "⏹ Request interrupted", nil)

	// Interrupt in a goroutine — it may hang on an HTTP abort; the UI must
	// respond instantly. A pending ask_user unblocks via ctx cancellation.
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

// capRunes keeps s within max runes, retaining the most recent tail.
func capRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…\n" + string(r[len(r)-max:])
}
