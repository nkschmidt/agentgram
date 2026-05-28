package bot

import (
	"context"
	"fmt"

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

// chatFor returns the chat to deliver to for a user — the last chat they wrote
// from, falling back to the user id itself (true for private chats).
func (s *Service) chatFor(userID int64) int64 {
	if v, ok := s.chatIDs.Load(userID); ok {
		return v.(int64)
	}
	return userID
}
