package command

import (
	"context"
	"strings"

	"github.com/schmidt/agentgram/internal/router"
)

// SettingsSection — a standalone /settings section.
// Each implementation encapsulates its own UI, state and Store dependencies.
// SettingsCommand just routes callbacks by slug and assembles the menu.
//
// Adding a new section = a new `section_*.go` file + registration
// in main. Trunk code of SettingsCommand isn't edited (OCP).
type SettingsSection interface {
	Slug() string                                                                  // callback_data routing key ("allowed_users")
	MenuButton() InlineButton                                                      // row in the main /settings menu
	Handle(ctx context.Context, cb router.Callback, r Replier, sub []string) error // callback subaction
	Accept(ctx context.Context, msg router.Message, r Replier) (bool, error)       // text input from the user; handled=false if the section doesn't need it
	ResetState(userID int64)                                                       // reset the user's pending/browse state (e.g. on /settings reopen)
}

// SettingsCommand — entry-point /settings. Assembles the menu from sections
// and routes callbacks and text input between them.
type SettingsCommand struct {
	sections []SettingsSection
	bySlug   map[string]SettingsSection
}

// NewSettings creates a command with the given sections. The order of sections
// is the order of buttons in the main menu.
func NewSettings(sections ...SettingsSection) *SettingsCommand {
	bySlug := make(map[string]SettingsSection, len(sections))
	for _, s := range sections {
		bySlug[s.Slug()] = s
	}
	return &SettingsCommand{sections: sections, bySlug: bySlug}
}

func (*SettingsCommand) Name() string        { return "settings" }
func (*SettingsCommand) Description() string { return "Bot settings" }
func (*SettingsCommand) Prefix() string      { return "settings" }

// Handle — reaction to /settings: show the main menu.
func (c *SettingsCommand) Handle(ctx context.Context, msg router.Message, r Replier) error {
	c.ResetState(msg.UserID)
	_, err := r.Send(ctx, msg.ChatID, menuText, c.menuKeyboard())
	return err
}

func (c *SettingsCommand) HandleCallback(ctx context.Context, cb router.Callback, r Replier) error {
	parts := strings.Split(cb.Data, ":")
	if len(parts) < 2 {
		return r.Answer(ctx, cb.ID, "")
	}
	switch parts[1] {
	case "open":
		c.ResetState(cb.UserID)
		if err := r.Edit(ctx, cb.ChatID, cb.MessageID, menuText, c.menuKeyboard()); err != nil {
			return err
		}
		return r.Answer(ctx, cb.ID, "")
	case "close":
		c.ResetState(cb.UserID)
		if err := r.Delete(ctx, cb.ChatID, cb.MessageID); err != nil {
			return err
		}
		return r.Answer(ctx, cb.ID, "")
	}
	// Any other prefix — a section slug.
	section, ok := c.bySlug[parts[1]]
	if !ok {
		return r.Answer(ctx, cb.ID, "")
	}
	return section.Handle(ctx, cb, r, parts[2:])
}

func (c *SettingsCommand) AcceptText(ctx context.Context, msg router.Message, r Replier) (bool, error) {
	for _, s := range c.sections {
		if handled, err := s.Accept(ctx, msg, r); handled {
			return true, err
		}
	}
	return false, nil
}

func (c *SettingsCommand) ResetState(userID int64) {
	for _, s := range c.sections {
		s.ResetState(userID)
	}
}

// menuText — main menu header. Telegram requires non-empty text
// for a message with an inline keyboard, keep it short.
const menuText = "⚙ Settings"

func (c *SettingsCommand) menuKeyboard() InlineKeyboard {
	rows := make([][]InlineButton, 0, len(c.sections)+1)
	for _, s := range c.sections {
		rows = append(rows, []InlineButton{s.MenuButton()})
	}
	rows = append(rows, []InlineButton{{Text: "✖ Close", Data: "settings:close"}})
	return rows
}

// cancelKb — reusable "◀ Cancel" button with arbitrary navigation target.
// Used by sections in pending modes.
func cancelKb(returnTo string) InlineKeyboard {
	return InlineKeyboard{{{Text: "◀ Cancel", Data: returnTo}}}
}
