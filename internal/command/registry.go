package command

import (
	"context"
	"log"
	"strings"

	"github.com/schmidt/agentgram/internal/router"
)

// Command — bot slash-command. Name — without leading slash ("settings", not "/settings").
// Description is used in the Telegram menu (the Menu button bottom-left).
type Command interface {
	Name() string
	Description() string
	Handle(ctx context.Context, msg router.Message, r Replier) error
}

// MenuItem — bot menu item for setMyCommands. A neutral struct,
// so we don't pull tgbotapi into command/main.
type MenuItem struct {
	Name        string
	Description string
}

// Registry — slash-command registry. Implements router.CommandHandler.
// OCP: a new command = Register(NewMyCommand(...)), no edits to Registry itself needed.
type Registry struct {
	commands map[string]Command
	order    []string // preserve registration order for stable menu
	replier  Replier
}

// NewRegistry creates an empty registry. replier is passed to each command
// in Handle — that's more convenient than duplicating the dependency in every command.
func NewRegistry(r Replier) *Registry {
	return &Registry{commands: map[string]Command{}, replier: r}
}

// Register adds a command. Re-registration under the same name
// overwrites the previous one (this is explicit behavior, not silent merge).
func (reg *Registry) Register(c Command) {
	if _, exists := reg.commands[c.Name()]; !exists {
		reg.order = append(reg.order, c.Name())
	}
	reg.commands[c.Name()] = c
}

// Menu returns the list of menu items in registration order.
// Used by bot.Service.PublishMenu for setMyCommands.
func (reg *Registry) Menu() []MenuItem {
	items := make([]MenuItem, 0, len(reg.order))
	for _, name := range reg.order {
		c := reg.commands[name]
		items = append(items, MenuItem{Name: name, Description: c.Description()})
	}
	return items
}

// Handle parses the command name from msg.Text and hands control to the found one.
// Unknown command — soft response to the user, not an error for the caller.
// On success the original message with the command is deleted from the chat
// so as not to clutter history — the command's UI lives in reply inline messages.
func (reg *Registry) Handle(ctx context.Context, msg router.Message) error {
	name := commandName(msg.Text)
	c, ok := reg.commands[name]
	if !ok {
		_, err := reg.replier.Send(ctx, msg.ChatID, "Unknown command: /"+name, nil)
		return err
	}
	if err := c.Handle(ctx, msg, reg.replier); err != nil {
		return err
	}
	if msg.MessageID > 0 {
		if err := reg.replier.Delete(ctx, msg.ChatID, msg.MessageID); err != nil {
			// not critical — just log
			log.Printf("registry: delete command message: %v", err)
		}
	}
	return nil
}

// commandName extracts the name from "/name", "/name@botusername", "/name arg ...".
func commandName(text string) string {
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	rest := text[1:]
	if i := strings.IndexAny(rest, " @"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
