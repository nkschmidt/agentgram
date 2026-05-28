// Package router — orchestrator of incoming Telegram events (messages and
// callback queries). Receives neutral Message/Callback from the bot layer
// and picks a handler: state machine (multi-step input), command (text
// starting with "/"), callback handler, or session forwarder for plain text.
//
// Router doesn't know about telegram-bot-api. Concrete handlers shouldn't
// either: they send replies via the Replier interface (defined in internal/command).
//
// Extension: new update types or new kinds of handlers are added via new
// interfaces and branches in Dispatch* without editing existing ones.
package router

import (
	"context"
	"strings"
)

// Message — neutral representation of an incoming text message.
// MessageID is needed for subsequent operations on the message (e.g.
// deleting the original message with the command after processing).
type Message struct {
	UserID    int64
	UserName  string
	ChatID    int64
	MessageID int
	Text      string
}

// IsCommand returns true if the message is a slash-command ("/start" etc).
func (m Message) IsCommand() bool {
	return strings.HasPrefix(m.Text, "/")
}

// Callback — neutral representation of an inline button press.
// MessageID — ID of the message holding the keyboard (for editing).
type Callback struct {
	ID        string
	Data      string
	UserID    int64
	UserName  string
	ChatID    int64
	MessageID int
}

// CommandHandler handles slash-commands.
type CommandHandler interface {
	Handle(ctx context.Context, msg Message) error
}

// CallbackHandler handles inline button presses.
type CallbackHandler interface {
	Handle(ctx context.Context, cb Callback) error
}

// SessionForwarder passes a regular (not a command, not a state) message
// into the user's backend session.
type SessionForwarder interface {
	Forward(ctx context.Context, msg Message) error
}

// StateAcceptor tries to handle the message within the user's current
// pending-state (e.g. "waiting for user_id to add"). If there's no active
// state for the user — returns handled=false, and the Router moves on.
type StateAcceptor interface {
	TryHandle(ctx context.Context, msg Message) (handled bool, err error)
}

// Router ties four independent handlers. Each is a narrow interface
// (ISP), letting implementations change independently and mock easily in tests (DIP).
type Router struct {
	state     StateAcceptor
	commands  CommandHandler
	callbacks CallbackHandler
	sessions  SessionForwarder
}

// New assembles the Router. All handlers are required; for the bootstrap
// stage, use NewWithLogging from defaults.go.
func New(state StateAcceptor, commands CommandHandler, callbacks CallbackHandler, sessions SessionForwarder) *Router {
	return &Router{state: state, commands: commands, callbacks: callbacks, sessions: sessions}
}

// Dispatch routes a text message.
// Order: command (explicit intent — overrides state) → state → session.
func (r *Router) Dispatch(ctx context.Context, msg Message) error {
	if msg.IsCommand() {
		return r.commands.Handle(ctx, msg)
	}
	if handled, err := r.state.TryHandle(ctx, msg); handled {
		return err
	}
	return r.sessions.Forward(ctx, msg)
}

// DispatchCallback routes an inline button press.
func (r *Router) DispatchCallback(ctx context.Context, cb Callback) error {
	return r.callbacks.Handle(ctx, cb)
}
