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

// Chunk — a piece of the backend process's output.
//
//   - Text — textual content.
//   - Replace=true — Text REPLACES the accumulated buffer (instead of appending).
//     The stream continues — used for backends that send an incremental update
//     of the full state (opencode message.part.updated).
//   - Final=true — final chunk: Text replaces the accumulated value (if non-empty),
//     the stream closes. Final=true with empty Text — just "close the stream".
//     The channel is NOT closed: the session lives on and awaits the next input.
//   - Err != nil only in the very last Chunk before the channel closes:
//     signals that the process has exited (and with what status).
//
// Note: Replace and Final are independent flags. If both are true → text
// replaces the accumulated value and the stream closes (behavior matches
// what only Final used to do).
type Chunk struct {
	Text    string
	Replace bool
	Final   bool
	Err     error
}
