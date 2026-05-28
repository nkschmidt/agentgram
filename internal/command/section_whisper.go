package command

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/schmidt/agentgram/internal/router"
)

// WhisperRepo — global whisper.cpp configuration (binary + model).
type WhisperRepo interface {
	WhisperBin() string
	SetWhisperBin(path string) error
	WhisperModel() string
	SetWhisperModel(path string) error
}

// WhisperSection — /settings section: path to whisper-cli and to the model.
// callback_data:
//
//	settings:whisper:view         — section screen
//	settings:whisper:edit_bin     — pending state: waiting for binary path
//	settings:whisper:edit_model   — pending state: waiting for model path
type WhisperSection struct {
	repo WhisperRepo

	mu      sync.Mutex
	pending map[int64]whisperPending // userID → what we're waiting from them
}

type whisperPending uint8

const (
	whisperPendingNone whisperPending = iota
	whisperPendingBin
	whisperPendingModel
)

func NewWhisperSection(repo WhisperRepo) *WhisperSection {
	return &WhisperSection{repo: repo, pending: map[int64]whisperPending{}}
}

func (*WhisperSection) Slug() string { return "whisper" }

func (*WhisperSection) MenuButton() InlineButton {
	return InlineButton{Text: "🎙 Voice (whisper)", Data: "settings:whisper:view"}
}

func (s *WhisperSection) ResetState(userID int64) {
	s.mu.Lock()
	delete(s.pending, userID)
	s.mu.Unlock()
}

func (s *WhisperSection) Handle(ctx context.Context, cb router.Callback, r Replier, sub []string) error {
	action := ""
	if len(sub) > 0 {
		action = sub[0]
	}
	switch action {
	case "", "view":
		s.ResetState(cb.UserID)
		if err := r.Edit(ctx, cb.ChatID, cb.MessageID, s.viewText(), s.viewKeyboard()); err != nil {
			return err
		}
		return r.Answer(ctx, cb.ID, "")
	case "edit_bin":
		s.setPending(cb.UserID, whisperPendingBin)
		text := "Send the path to whisper-cli (or binary name in PATH)."
		if err := r.Edit(ctx, cb.ChatID, cb.MessageID, text, cancelKb("settings:whisper:view")); err != nil {
			return err
		}
		return r.Answer(ctx, cb.ID, "")
	case "edit_model":
		s.setPending(cb.UserID, whisperPendingModel)
		text := "Send the path to a whisper.cpp .bin model (e.g. ggml-base.bin)."
		if err := r.Edit(ctx, cb.ChatID, cb.MessageID, text, cancelKb("settings:whisper:view")); err != nil {
			return err
		}
		return r.Answer(ctx, cb.ID, "")
	default:
		return r.Answer(ctx, cb.ID, "")
	}
}

func (s *WhisperSection) Accept(ctx context.Context, msg router.Message, r Replier) (bool, error) {
	action := s.takePending(msg.UserID)
	switch action {
	case whisperPendingBin:
		return true, s.applyBin(ctx, msg, r)
	case whisperPendingModel:
		return true, s.applyModel(ctx, msg, r)
	default:
		return false, nil
	}
}

// ---------- internals ----------

func (s *WhisperSection) setPending(userID int64, p whisperPending) {
	s.mu.Lock()
	s.pending[userID] = p
	s.mu.Unlock()
}

func (s *WhisperSection) takePending(userID int64) whisperPending {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[userID]
	delete(s.pending, userID)
	return p
}

func (s *WhisperSection) viewText() string {
	bin := s.repo.WhisperBin()
	model := s.repo.WhisperModel()
	binLine := "Binary: not set"
	if bin != "" {
		binLine = "Binary: " + bin
	}
	modelLine := "Model: not set"
	if model != "" {
		modelLine = "Model: " + model
	}
	status := "❌ transcription not configured"
	if bin != "" && model != "" {
		status = "✅ ready for transcription"
	}
	return "🎙 Voice messages\n\n" + binLine + "\n" + modelLine + "\n\n" + status
}

func (*WhisperSection) viewKeyboard() InlineKeyboard {
	return InlineKeyboard{
		{{Text: "✏️ Edit binary", Data: "settings:whisper:edit_bin"}},
		{{Text: "✏️ Edit model", Data: "settings:whisper:edit_model"}},
		{{Text: "◀ Back", Data: "settings:open"}},
	}
}

func (s *WhisperSection) applyBin(ctx context.Context, msg router.Message, r Replier) error {
	raw := strings.TrimSpace(msg.Text)
	if raw == "" {
		s.setPending(msg.UserID, whisperPendingBin)
		_, sErr := r.Send(ctx, msg.ChatID, "Empty path. Try again.", cancelKb("settings:whisper:view"))
		return sErr
	}
	resolved, err := resolveExecutable(raw)
	if err != nil {
		s.setPending(msg.UserID, whisperPendingBin)
		_, sErr := r.Send(ctx, msg.ChatID, "Executable not found: "+err.Error(), cancelKb("settings:whisper:view"))
		return sErr
	}
	if err := s.repo.SetWhisperBin(resolved); err != nil {
		_, sErr := r.Send(ctx, msg.ChatID, "Failed to save: "+err.Error(), nil)
		return sErr
	}
	_, err = r.Send(ctx, msg.ChatID, "Whisper binary updated:\n"+resolved+"\n\n"+s.viewText(), s.viewKeyboard())
	return err
}

func (s *WhisperSection) applyModel(ctx context.Context, msg router.Message, r Replier) error {
	raw := strings.TrimSpace(msg.Text)
	if raw == "" {
		s.setPending(msg.UserID, whisperPendingModel)
		_, sErr := r.Send(ctx, msg.ChatID, "Empty path. Try again.", cancelKb("settings:whisper:view"))
		return sErr
	}
	abs, err := expandAndAbs(raw)
	if err != nil {
		s.setPending(msg.UserID, whisperPendingModel)
		_, sErr := r.Send(ctx, msg.ChatID, "Failed to resolve path: "+err.Error(), cancelKb("settings:whisper:view"))
		return sErr
	}
	info, err := os.Stat(abs)
	if err != nil {
		s.setPending(msg.UserID, whisperPendingModel)
		_, sErr := r.Send(ctx, msg.ChatID, "Failed to open file: "+err.Error(), cancelKb("settings:whisper:view"))
		return sErr
	}
	if info.IsDir() {
		s.setPending(msg.UserID, whisperPendingModel)
		_, sErr := r.Send(ctx, msg.ChatID, "It's a directory, expected a file.", cancelKb("settings:whisper:view"))
		return sErr
	}
	if err := s.repo.SetWhisperModel(abs); err != nil {
		_, sErr := r.Send(ctx, msg.ChatID, "Failed to save: "+err.Error(), nil)
		return sErr
	}
	_, err = r.Send(ctx, msg.ChatID, "Whisper model updated:\n"+abs+"\n\n"+s.viewText(), s.viewKeyboard())
	return err
}

// resolveExecutable accepts either a name in PATH or a path — verifies
// that the file exists and is executable. Returns an absolute path.
func resolveExecutable(raw string) (string, error) {
	if !strings.ContainsRune(raw, '/') {
		return exec.LookPath(raw)
	}
	abs, err := expandAndAbs(raw)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("it's a directory, expected a file")
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("file is not executable")
	}
	return abs, nil
}

// expandAndAbs — expands `~/` and normalizes to an absolute path.
func expandAndAbs(p string) (string, error) {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return filepath.Abs(p)
}
