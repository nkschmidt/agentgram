package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

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

// AskUser blocks until the user answers the agent's question, returning their
// reply. The StreamCoordinator displays the question (with option buttons) and
// collects the answer; this runs when the agent executes the tool, so the
// question appears after the prose the agent just wrote.
func (s *Service) AskUser(ctx context.Context, userID int64, question string, options []string) (string, error) {
	if s.prompter == nil {
		return "", errors.New("ask_user is not available")
	}
	return s.prompter.WaitAnswer(ctx, userID, s.chatFor(userID), question, options)
}

// chatFor returns the chat to deliver to for a user — the last chat they wrote
// from, falling back to the user id itself (true for private chats).
func (s *Service) chatFor(userID int64) int64 {
	if v, ok := s.chatIDs.Load(userID); ok {
		return v.(int64)
	}
	return userID
}

// isCommand reports whether text is a slash command. While a question is
// pending we still let commands through (e.g. /new_session, /restart) so the
// user always has an escape hatch instead of being forced to answer.
func isCommand(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "/")
}
