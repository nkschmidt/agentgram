// Package backend defines the Backend abstraction — a long-lived process
// with stdin/stdout, wrapped for our bot's needs. Concrete adapters
// (claude, opencode, generic) live in subpackages and implement the
// common interface.
//
// Extension (OCP): a new adapter = new package in internal/backend/X +
// registration in Registry. The core (session.Manager) is not edited.
package backend

import "context"

// Backend — unified interface for all adapters. Life cycle:
//
//	b.Start(ctx)        // start the process
//	b.Send(line)        // write to stdin (line by line)
//	<-b.Recv()          // read from stdout (as a stream)
//	b.Interrupt()       // SIGINT — ask the process to interrupt current work
//	                    //   (Ctrl+C equivalent; the process may stay alive)
//	b.Stop()            // SIGKILL — guaranteed termination
//
// Recv() returns the same channel for the session's lifetime.
// The channel closes when the process has exited (the last Chunk may
// carry Err — the exit error).
type Backend interface {
	Start(ctx context.Context) error
	Send(input string) error
	Recv() <-chan Chunk
	Interrupt() error
	Stop() error
}

// ChunkKind classifies a Chunk so the UI can render each role distinctly. The
// backend emits a single ordered stream of these per turn; the coordinator is
// the only renderer.
type ChunkKind uint8

const (
	// KindProse — the agent's words (answer text). Shown live and kept as a
	// persistent message; sealed into a new message at each KindQuestion /
	// KindEnd boundary, split across messages if longer than Telegram allows.
	KindProse ChunkKind = iota
	// KindActivity — a tool step / thinking ("what the agent is doing"). Shown
	// in an ephemeral progress message, removed at the next boundary.
	KindActivity
	// KindEnd — the turn finished. Seals prose and removes the activity message.
	KindEnd
)

// Note: ask_user is intentionally NOT a chunk kind. The question is displayed
// by the coordinator when the MCP tool actually executes (WaitAnswer) — that
// happens after the agent has produced the preceding prose, which keeps the
// question below it in the chat. Emitting it from the event stream rendered it
// too early (opencode surfaces the tool part before the text finishes).

// Chunk — one ordered piece of the backend process's output for the current
// turn.
//
//   - Kind classifies the chunk (see ChunkKind).
//   - Text — content for Prose/Activity.
//   - Replace=true — for Prose/Activity, Text REPLACES the accumulated buffer
//     instead of appending (opencode sends incremental full-state updates).
//   - Err != nil only in the very last Chunk before the channel closes: signals
//     the process has exited (and with what status); Kind is irrelevant then.
type Chunk struct {
	Kind    ChunkKind
	Text    string
	Replace bool
	Err     error
}
