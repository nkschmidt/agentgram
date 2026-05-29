package bot

import (
	"context"
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/schmidt/agentgram/internal/command"
)

// tgReplier — command.Replier implementation on top of tgbotapi.
// The concrete Telegram API is hidden here, doesn't leak outside.
//
// Parse mode is intentionally not set: texts may contain paths,
// command names (/new_session), user-facing dynamic content with `_`/`*`/`` ` ``
// — Telegram's Markdown parser breaks on unpaired entities. Without parse mode
// everything is shown "as is".
type tgReplier struct{ api *tgbotapi.BotAPI }

func (r *tgReplier) Send(_ context.Context, chatID int64, text string, kb command.InlineKeyboard) (int, error) {
	text = clean(text)
	msg := tgbotapi.NewMessage(chatID, text)
	if len(kb) > 0 {
		msg.ReplyMarkup = toMarkup(kb)
	}
	sent, err := r.api.Send(msg)
	if err != nil {
		return 0, fmt.Errorf("send: %w", err)
	}
	return sent.MessageID, nil
}

func (r *tgReplier) Edit(_ context.Context, chatID int64, messageID int, text string, kb command.InlineKeyboard) error {
	text = clean(text)
	// NewEditMessageTextAndMarkup always updates both text and keyboard —
	// an empty kb clears the keyboard (behavior NewEditMessageText doesn't give).
	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, text, toMarkup(kb))
	if _, err := r.api.Send(edit); err != nil {
		if isNotModified(err) {
			return nil
		}
		return fmt.Errorf("edit: %w", err)
	}
	return nil
}

func (r *tgReplier) SendHTML(_ context.Context, chatID int64, text string, kb command.InlineKeyboard) (int, error) {
	text = clean(text)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	if len(kb) > 0 {
		msg.ReplyMarkup = toMarkup(kb)
	}
	sent, err := r.api.Send(msg)
	if err != nil {
		return 0, fmt.Errorf("send html: %w", err)
	}
	return sent.MessageID, nil
}

func (r *tgReplier) EditHTML(_ context.Context, chatID int64, messageID int, text string, kb command.InlineKeyboard) error {
	text = clean(text)
	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, text, toMarkup(kb))
	edit.ParseMode = tgbotapi.ModeHTML
	if _, err := r.api.Send(edit); err != nil {
		if isNotModified(err) {
			return nil
		}
		return fmt.Errorf("edit html: %w", err)
	}
	return nil
}

func (r *tgReplier) Delete(_ context.Context, chatID int64, messageID int) error {
	cfg := tgbotapi.NewDeleteMessage(chatID, messageID)
	if _, err := r.api.Request(cfg); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

func (r *tgReplier) Answer(_ context.Context, callbackID string, toast string) error {
	cb := tgbotapi.NewCallback(callbackID, toast)
	if _, err := r.api.Request(cb); err != nil {
		return fmt.Errorf("answer callback: %w", err)
	}
	return nil
}

func (r *tgReplier) Typing(_ context.Context, chatID int64) error {
	action := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	if _, err := r.api.Request(action); err != nil {
		return fmt.Errorf("typing: %w", err)
	}
	return nil
}

func toMarkup(kb command.InlineKeyboard) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(kb))
	for _, row := range kb {
		cells := make([]tgbotapi.InlineKeyboardButton, 0, len(row))
		for _, b := range row {
			cells = append(cells, tgbotapi.NewInlineKeyboardButtonData(b.Text, b.Data))
		}
		rows = append(rows, cells)
	}
	// Direct construction instead of tgbotapi.NewInlineKeyboardMarkup —
	// that one with empty rows leaves InlineKeyboard=nil, which Telegram
	// serializes as "null" and rejects with "must be of type Array".
	// make guarantees non-nil even for an empty keyboard.
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// clean strips invalid UTF-8 byte sequences. The Telegram API rejects any
// request whose text isn't valid UTF-8 ("Bad Request: strings must be encoded
// in UTF-8"); streamed chunks can end mid-rune (a multi-byte character cut in
// half — common with Cyrillic), so we drop the dangling bytes here, at the
// single boundary to Telegram. On already-valid text this is a no-op.
func clean(s string) string {
	return strings.ToValidUTF8(s, "")
}

// isNotModified — Telegram returns 400 on Edit with the same content.
// This isn't a real error but a silent no-op: skip it.
func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}
