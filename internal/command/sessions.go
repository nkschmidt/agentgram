package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/schmidt/agentgram/internal/router"
)

// SessionService — narrow interface to session.Manager that the command needs.
// session.Manager satisfies it automatically.
type SessionService interface {
	Start(ctx context.Context, userID, chatID int64, backendName string) error
	ActiveName(userID int64) (name string, ok bool)
}

// SessionsCommand — /new_session command: shows backend selection
// for a new CLI session and launches the chosen one via SessionService.
// If the user already has an active session — it's shown in the menu
// header and will be stopped when a new backend is selected (handled by
// session.Manager).
type SessionsCommand struct {
	sessions SessionService
}

func NewSessions(s SessionService) *SessionsCommand {
	return &SessionsCommand{sessions: s}
}

func (*SessionsCommand) Name() string        { return "new_session" }
func (*SessionsCommand) Description() string { return "Start a new session" }
func (*SessionsCommand) Prefix() string      { return "new_session" }

func (c *SessionsCommand) Handle(ctx context.Context, msg router.Message, r Replier) error {
	_, err := r.Send(ctx, msg.ChatID, c.menuText(msg.UserID), c.menuKeyboard())
	return err
}

func (c *SessionsCommand) HandleCallback(ctx context.Context, cb router.Callback, r Replier) error {
	parts := strings.Split(cb.Data, ":")
	if len(parts) < 2 {
		return r.Answer(ctx, cb.ID, "")
	}
	switch parts[1] {
	case "claude":
		return c.start(ctx, cb, r, "claude", "Claude")
	case "opencode":
		return c.start(ctx, cb, r, "opencode", "Opencode")
	case "close":
		if err := r.Delete(ctx, cb.ChatID, cb.MessageID); err != nil {
			return err
		}
		return r.Answer(ctx, cb.ID, "")
	default:
		return r.Answer(ctx, cb.ID, "")
	}
}

// start launches a backend session via SessionService. The previous session
// (if any) is stopped inside the manager — no need to know about it here.
//
// The callback is answered up front: starting a backend can be slow (a cold
// opencode server spinning up), and Telegram invalidates the callback query
// after ~15s ("query is too old"). We then show progress and report the result
// by editing the message, so a slow start never leaves the UI stuck.
func (c *SessionsCommand) start(ctx context.Context, cb router.Callback, r Replier, backendName, label string) error {
	_ = r.Answer(ctx, cb.ID, "Starting "+label+"…")
	_ = r.Edit(ctx, cb.ChatID, cb.MessageID, "⏳ Starting "+label+"…", nil)

	if err := c.sessions.Start(ctx, cb.UserID, cb.ChatID, backendName); err != nil {
		// Restore the menu so the user can retry or pick another backend.
		return r.Edit(ctx, cb.ChatID, cb.MessageID,
			"⚠ Failed to start "+label+": "+err.Error(), c.menuKeyboard())
	}
	text := fmt.Sprintf("✨ Session %s started.\n\nWrite messages — they'll be forwarded to the process.", label)
	return r.Edit(ctx, cb.ChatID, cb.MessageID, text, nil)
}

// menuText shows the header plus — if any — the active session.
func (c *SessionsCommand) menuText(userID int64) string {
	name, ok := c.sessions.ActiveName(userID)
	if !ok {
		return "🚀 New session"
	}
	return fmt.Sprintf("🚀 New session\n\nCurrently active: %s", backendLabel(name))
}

// backendLabel — human-readable backend name for the UI.
func backendLabel(name string) string {
	switch name {
	case "claude":
		return "Claude"
	case "opencode":
		return "Opencode"
	default:
		return name
	}
}

func (*SessionsCommand) menuKeyboard() InlineKeyboard {
	return InlineKeyboard{
		{{Text: "🤖 Claude Session", Data: "new_session:claude"}},
		{{Text: "📦 Opencode Session", Data: "new_session:opencode"}},
		{{Text: "✖ Close", Data: "new_session:close"}},
	}
}
