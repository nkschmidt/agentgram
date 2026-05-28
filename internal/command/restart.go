package command

import (
	"context"
	"log"
	"time"

	"github.com/schmidt/agentgram/internal/router"
)

// RestartCommand — /restart: hard restart of the bot. An emergency
// recovery command — must fire even when backend sessions are stuck,
// the Telegram API lags or other commands don't respond.
//
// Design: NO confirmation and no callback buttons. Any confirmation —
// a potential blocker (if Edit/Send hang on the network). The "restarting"
// notification is sent fire-and-forget from a goroutine, exec — from another.
// handleUpdate in bot.Service already parallelizes each update, so this
// command works independently of other calls' state.
//
// Actual shutdown + syscall.Exec is encapsulated in the restart closure
// from main (RestartCommand doesn't know about sessionMgr/opencodeLazy).
type RestartCommand struct {
	restart func()
}

func NewRestart(restart func()) *RestartCommand {
	return &RestartCommand{restart: restart}
}

func (*RestartCommand) Name() string        { return "restart" }
func (*RestartCommand) Description() string { return "Restart the bot" }

// Handle launches exec in a goroutine right away; in parallel best-effort
// sends a notification to the chat. Doesn't block on anything — even if
// the Telegram API doesn't respond, exec still happens.
func (c *RestartCommand) Handle(ctx context.Context, msg router.Message, r Replier) error {
	chatID := msg.ChatID

	go func() {
		// Notification — best-effort. If Send hangs, we don't wait.
		if _, err := r.Send(context.Background(), chatID, "♻ Restarting...", nil); err != nil {
			log.Printf("restart: send notice: %v", err)
		}
	}()

	go func() {
		// Short pause so Send has a chance to go out. If it didn't — exec anyway.
		time.Sleep(300 * time.Millisecond)
		c.restart()
	}()

	return nil
}
