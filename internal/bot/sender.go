package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// askCallbackPrefix tags inline-button callbacks that answer an ask_user
// question. The payload after it is the option index (callback_data is capped
// at 64 bytes, so we never put the label itself there).
const askCallbackPrefix = "aq:"

// askTimeout bounds how long ask_user waits for the user before giving up, so a
// never-answered question doesn't hang the agent forever.
const askTimeout = 10 * time.Minute

// pendingAsk is one in-flight ask_user question for a user.
type pendingAsk struct {
	chatID    int64
	messageID int
	question  string
	options   []string
	answer    chan string // buffered(1); first tap/reply wins
}

// AskUser implements the question half of the mcp.Bot contract: it posts the
// question (with option buttons if any), then blocks until the user taps a
// button or types a reply — both routed in here via handleCallback/handleMessage
// — or ctx is cancelled (interrupt) or the timeout fires.
func (s *Service) AskUser(ctx context.Context, userID int64, question string, options []string) (string, error) {
	pa := &pendingAsk{
		chatID:   s.chatFor(userID),
		question: question,
		options:  options,
		answer:   make(chan string, 1),
	}
	if _, loaded := s.asks.LoadOrStore(userID, pa); loaded {
		return "", fmt.Errorf("a question is already pending for this user")
	}
	defer s.asks.Delete(userID)

	msg := tgbotapi.NewMessage(pa.chatID, "❓ "+question)
	if len(options) > 0 {
		msg.ReplyMarkup = askKeyboard(options)
	}
	sent, err := s.api.Send(msg)
	if err != nil {
		return "", fmt.Errorf("ask: send question: %w", err)
	}
	pa.messageID = sent.MessageID

	select {
	case ans := <-pa.answer:
		s.finishAsk(pa, ans)
		return ans, nil
	case <-ctx.Done():
		s.finishAsk(pa, "")
		return "", ctx.Err()
	case <-time.After(askTimeout):
		s.finishAsk(pa, "")
		return "", fmt.Errorf("ask: no response from user within %s", askTimeout)
	}
}

// tryAnswerText delivers a typed reply to a pending question. Returns true if a
// question was pending for the user (so the caller must NOT forward the message
// to the agent as a new prompt).
func (s *Service) tryAnswerText(userID int64, text string) bool {
	v, ok := s.asks.Load(userID)
	if !ok {
		return false
	}
	if text != "" {
		deliver(v.(*pendingAsk), text)
	}
	return true
}

// tryAnswerCallback handles an "aq:<idx>" button tap. Returns true if the data
// is an ask callback (ours), so the caller acks it and stops dispatching.
func (s *Service) tryAnswerCallback(userID int64, data string) bool {
	if !strings.HasPrefix(data, askCallbackPrefix) {
		return false
	}
	if v, ok := s.asks.Load(userID); ok {
		pa := v.(*pendingAsk)
		if idx, err := strconv.Atoi(strings.TrimPrefix(data, askCallbackPrefix)); err == nil && idx >= 0 && idx < len(pa.options) {
			deliver(pa, pa.options[idx])
		}
	}
	return true
}

// isCommand reports whether text is a slash command. While a question is
// pending we still let commands through (e.g. /new_session, /restart) so the
// user always has an escape hatch instead of being forced to answer.
func isCommand(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "/")
}

// deliver hands the answer to the waiting AskUser without blocking; a racing
// second answer (tap + reply) is dropped because the channel is buffered to 1.
func deliver(pa *pendingAsk, answer string) {
	select {
	case pa.answer <- answer:
	default:
	}
}

func askKeyboard(options []string) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(options))
	for i, opt := range options {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(opt, askCallbackPrefix+strconv.Itoa(i)),
		})
	}
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// finishAsk rewrites the question message to show the outcome and drops the
// keyboard (a non-nil empty markup clears it in one Edit).
func (s *Service) finishAsk(pa *pendingAsk, answer string) {
	text := "❓ " + pa.question
	if answer != "" {
		text += "\n\n✅ " + answer
	} else {
		text += "\n\n⨯ no answer"
	}
	edit := tgbotapi.NewEditMessageTextAndMarkup(pa.chatID, pa.messageID, text,
		tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
	_, _ = s.api.Send(edit)
}

// SendPhoto delivers a local image file to the user's chat as an inline photo.
// It implements the Sender contract consumed by internal/mcp (satisfied
// structurally — bot doesn't import mcp, so tgbotapi stays locked here).
func (s *Service) SendPhoto(_ context.Context, userID int64, path, caption string) error {
	msg := tgbotapi.NewPhoto(s.chatFor(userID), tgbotapi.FilePath(path))
	if caption != "" {
		msg.Caption = caption
	}
	if _, err := s.api.Send(msg); err != nil {
		return fmt.Errorf("send photo: %w", err)
	}
	return nil
}

// SendDocument delivers a local file to the user's chat as a document.
func (s *Service) SendDocument(_ context.Context, userID int64, path, caption string) error {
	msg := tgbotapi.NewDocument(s.chatFor(userID), tgbotapi.FilePath(path))
	if caption != "" {
		msg.Caption = caption
	}
	if _, err := s.api.Send(msg); err != nil {
		return fmt.Errorf("send document: %w", err)
	}
	return nil
}

// chatFor returns the chat to deliver to for a user — the last chat they wrote
// from, falling back to the user id itself (true for private chats).
func (s *Service) chatFor(userID int64) int64 {
	if v, ok := s.chatIDs.Load(userID); ok {
		return v.(int64)
	}
	return userID
}
