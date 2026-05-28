package command

import (
	"context"
	"log"

	"github.com/schmidt/agentgram/internal/router"
	"github.com/schmidt/agentgram/internal/session"
)

// Forwarder forwards regular user text messages into the active
// backend process via session.Manager. Implements router.SessionForwarder.
//
// UI coordination (typing + edit of a single message as chunks arrive)
// is delegated to StreamCoordinator — Forwarder is only responsible for
// the "input" (passing text to backend), main.onChunk is responsible for
// the "output" (chunks → UI).
type Forwarder struct {
	sessions *session.Manager
	replier  Replier
	stream   *StreamCoordinator
}

func NewForwarder(s *session.Manager, r Replier, stream *StreamCoordinator) *Forwarder {
	return &Forwarder{sessions: s, replier: r, stream: stream}
}

func (f *Forwarder) Forward(ctx context.Context, msg router.Message) error {
	s, ok := f.sessions.Active(msg.UserID)
	if !ok {
		_, err := f.replier.Send(ctx, msg.ChatID, "No active session. Start one via /new_session.", nil)
		return err
	}
	// Parallel requests from the same user would break text accumulation
	// in StreamCoordinator (chunks of the previous response would land in
	// the next one's message). Suggest waiting.
	if f.stream.IsActive(msg.UserID) {
		_, err := f.replier.Send(ctx, msg.ChatID, "⏳ Wait for the response to the previous request.", nil)
		return err
	}

	f.stream.Begin(msg.UserID, msg.ChatID)
	if err := s.Backend.Send(msg.Text); err != nil {
		f.stream.Finish(msg.UserID)
		log.Printf("forwarder: send failed for user %d: %v", msg.UserID, err)
		_, sErr := f.replier.Send(ctx, msg.ChatID, "Failed to send to session: "+err.Error(), nil)
		return sErr
	}
	return nil
}
