// Package command — bot slash-commands, callback query routing
// from inline buttons and pending-state for multi-step input.
//
// Commands don't know about telegram-bot-api: they answer the user via
// the narrow Replier interface (defined here). The concrete Replier
// implementation lives in internal/bot.
package command

import "context"

// Replier — narrow interface for sending a reply to Telegram, without leaking tgbotapi.
// Uses inline keyboards (buttons in the message), whose presses arrive as
// callback queries — this gives a clean split between "command UI" and
// "agent dialogue": the user's input field is always free for communication.
//
// Send/Edit — plain without parse mode (for places where text may contain
// arbitrary `_`, `*`, `<` — file paths, command names, intermediate
// tool steps).
// SendHTML/EditHTML — with parse_mode=HTML, for the model's final answers,
// which may contain markdown formatting. Text preparation — via
// MarkdownToHTML. If Telegram rejects broken HTML, caller falls back to plain.
type Replier interface {
	Send(ctx context.Context, chatID int64, text string, kb InlineKeyboard) (messageID int, err error)
	Edit(ctx context.Context, chatID int64, messageID int, text string, kb InlineKeyboard) error
	SendHTML(ctx context.Context, chatID int64, text string, kb InlineKeyboard) (messageID int, err error)
	EditHTML(ctx context.Context, chatID int64, messageID int, text string, kb InlineKeyboard) error
	// Delete deletes a message from the chat.
	Delete(ctx context.Context, chatID int64, messageID int) error
	// EditMarkup updates only a message's inline keyboard (an empty kb removes
	// it), leaving the text intact. Used to move the ⏹ Stop button to the
	// message that's currently streaming.
	EditMarkup(ctx context.Context, chatID int64, messageID int, kb InlineKeyboard) error
	// Answer — ack the callback query. Telegram spins the button until
	// it receives this call. Toast — optional popup message.
	Answer(ctx context.Context, callbackID string, toast string) error
	// Typing sends the "typing" chat action. Client-side effect is ~5 sec,
	// for long generation it must be repeated.
	Typing(ctx context.Context, chatID int64) error
}

// InlineKeyboard — 2D array of buttons (rows × buttons in a row).
// Empty/nil = "no keyboard".
type InlineKeyboard [][]InlineButton

// InlineButton — button with a label and callback_data.
// CallbackData is limited to 64 bytes on Telegram's side.
type InlineButton struct {
	Text string
	Data string
}
