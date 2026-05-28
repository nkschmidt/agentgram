// Package asr — Automatic Speech Recognition. A narrow interface for
// transcribing an audio file into text. Concrete implementations live in
// subpackages (whispercpp). If the bot starts without ASR configured, Noop
// is used — it gracefully returns "no transcription", the bot doesn't crash.
package asr

import "context"

// Transcriber — unified interface for audio → text. Takes a path to a file
// on disk (any format the implementation understands).
type Transcriber interface {
	Transcribe(ctx context.Context, audioPath string) (string, error)
	Available() bool
}

// Noop — stub. Available() = false; Transcribe returns an empty string
// without error. Caller decides what to show the user (e.g. "voice messages
// not configured" in chat).
type Noop struct{}

func (Noop) Transcribe(_ context.Context, _ string) (string, error) { return "", nil }
func (Noop) Available() bool                                        { return false }
