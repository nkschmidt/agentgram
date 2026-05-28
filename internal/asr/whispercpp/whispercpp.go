// Package whispercpp — asr.Transcriber implementation via the local
// whisper.cpp CLI. Runs the binary with the given model and audio file,
// reads the result from `<audio>.txt`.
//
// We don't hardcode the binary and model — they come via provider functions
// (typically from settings.Store). This lets the user change the path to
// whisper-cli / model via /settings on the fly, without restarting the bot.
package whispercpp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Transcriber — asr.Transcriber implementation.
//
// binProvider / modelProvider are read on every Available()/Transcribe —
// the setting can change at any moment via /settings.
type Transcriber struct {
	binProvider   func() string
	modelProvider func() string
}

// New creates a transcriber. Both providers are required.
func New(binProvider, modelProvider func() string) *Transcriber {
	return &Transcriber{binProvider: binProvider, modelProvider: modelProvider}
}

// Available returns true if both values are configured.
// The transcriber checks the values right now — if the user just changed
// them via /settings, the next call already picks up the new ones.
func (t *Transcriber) Available() bool {
	return t != nil && t.binProvider() != "" && t.modelProvider() != ""
}

// Transcribe runs whisper-cli and reads the created .txt file next to
// the source audio. ctx allows cancelling a long transcription.
//
// whisper.cpp expects WAV (16kHz, mono, PCM) — Telegram voice arrives as OGG/Opus,
// so before launching we run the audio through ffmpeg. The converted file
// is deleted after transcription.
func (t *Transcriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	if t == nil {
		return "", errors.New("whispercpp: nil transcriber")
	}
	bin := t.binProvider()
	if bin == "" {
		return "", errors.New("whispercpp: binary not configured")
	}
	model := t.modelProvider()
	if model == "" {
		return "", errors.New("whispercpp: model not configured")
	}
	if audioPath == "" {
		return "", errors.New("whispercpp: empty audio path")
	}

	wavPath := audioPath + ".wav"
	if err := convertToWAV(ctx, audioPath, wavPath); err != nil {
		return "", err
	}
	defer os.Remove(wavPath)

	// --output-txt creates <wavPath>.txt; --no-prints silences noise in stdout.
	cmd := exec.CommandContext(ctx, bin,
		"-m", model,
		"-f", wavPath,
		"--output-txt",
		"--no-prints",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("whispercpp: %s failed: %w (output: %s)", bin, err, strings.TrimSpace(string(out)))
	}

	txtPath := wavPath + ".txt"
	defer os.Remove(txtPath)
	data, err := os.ReadFile(txtPath)
	if err != nil {
		return "", fmt.Errorf("whispercpp: read transcript %s: %w", txtPath, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// convertToWAV runs input through ffmpeg into a format whisper.cpp
// is guaranteed to understand: PCM signed 16-bit LE, 16kHz, mono.
// If ffmpeg is not in PATH — returns a clear error with a hint.
func convertToWAV(ctx context.Context, input, output string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("whispercpp: ffmpeg is required for transcription (install: brew install ffmpeg)")
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",                // overwrite output
		"-loglevel", "error", // quieter in logs
		"-i", input,
		"-ar", "16000", // sample rate 16kHz
		"-ac", "1", // mono
		"-c:a", "pcm_s16le", // 16-bit PCM
		output,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("whispercpp: ffmpeg failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
