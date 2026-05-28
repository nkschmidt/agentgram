package command

import (
	"context"
	"strings"

	"github.com/schmidt/agentgram/internal/router"
)

// StartCommand — /start: welcome message + auto-generated list of commands
// pulled from the registry via a provider closure. New commands registered
// later show up in /start without any extra wiring (OCP).
type StartCommand struct {
	menu func() []MenuItem
}

func NewStart(menu func() []MenuItem) *StartCommand {
	return &StartCommand{menu: menu}
}

func (*StartCommand) Name() string        { return "start" }
func (*StartCommand) Description() string { return "Show welcome and command list" }

func (c *StartCommand) Handle(ctx context.Context, msg router.Message, r Replier) error {
	var b strings.Builder
	b.WriteString("👋 Hi! I'm a Telegram bot wrapping CLI agents (Claude Code, opencode).\n\n")
	b.WriteString("Available commands:\n")
	for _, m := range c.menu() {
		b.WriteString("/")
		b.WriteString(m.Name)
		b.WriteString(" — ")
		b.WriteString(m.Description)
		b.WriteByte('\n')
	}
	b.WriteString("\nSend any message and it'll be forwarded to the active session. ")
	b.WriteString("If there's no active session yet, start one via /new_session.\n\n")
	b.WriteString("You can also send documents, photos and voice messages — they'll be passed to the agent together with your text.")
	_, err := r.Send(ctx, msg.ChatID, b.String(), nil)
	return err
}
