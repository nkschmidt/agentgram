package bot

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Transcriber — narrow ASR interface needed by bot.Service for voice messages.
// Implementation — in the asr package (Noop / whispercpp).
type Transcriber interface {
	Transcribe(ctx context.Context, audioPath string) (string, error)
	Available() bool
}

// composer assembles the final user message text: to caption/text it
// appends mentions of downloaded attachments (with local path — the agent
// can read them via the Read tool) and transcription of voice messages.
//
// Files are saved in `<workDir>/.tmp/` if the user has workDir set,
// otherwise in `TempDir/tg-bot/<userID>/`. Inside workdir — so that opencode
// doesn't block reading via permission.asked (external_directory outside
// the working folder requires a grant).
type composer struct {
	api         *tgbotapi.BotAPI
	transcriber Transcriber
	workDirOf   func(userID int64) string
}

func newComposer(api *tgbotapi.BotAPI, t Transcriber, workDirOf func(userID int64) string) *composer {
	return &composer{api: api, transcriber: t, workDirOf: workDirOf}
}

// compose turns an incoming message into a string for backend.Send.
// Returns an empty string if there's nothing to forward (no text or
// attachments). In that case bot.Service forwards nothing to the Forwarder.
func (c *composer) compose(ctx context.Context, m *tgbotapi.Message) string {
	userText := strings.TrimSpace(firstNonEmpty(m.Text, m.Caption))

	var attachments []string

	if m.Document != nil {
		path, err := c.download(ctx, m.From.ID, m.Document.FileID, m.Document.FileName)
		if err != nil {
			log.Printf("composer: download document: %v", err)
			attachments = append(attachments, "[failed to download document: "+err.Error()+"]")
		} else {
			attachments = append(attachments, "[Attached document: "+path+"]")
		}
	}

	if len(m.Photo) > 0 {
		// Telegram sends multiple resolutions — take the largest one.
		largest := m.Photo[len(m.Photo)-1]
		path, err := c.download(ctx, m.From.ID, largest.FileID, "photo.jpg")
		if err != nil {
			log.Printf("composer: download photo: %v", err)
			attachments = append(attachments, "[failed to download image: "+err.Error()+"]")
		} else {
			attachments = append(attachments, "[Attached image: "+path+"]")
		}
	}

	if m.Voice != nil {
		attachments = append(attachments, c.handleAudio(ctx, m.From.ID, m.Voice.FileID, "voice.ogg"))
	}
	if m.Audio != nil {
		name := m.Audio.FileName
		if name == "" {
			name = "audio.mp3"
		}
		attachments = append(attachments, c.handleAudio(ctx, m.From.ID, m.Audio.FileID, name))
	}

	var b strings.Builder
	for _, a := range attachments {
		if a == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(a)
	}
	if userText != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(userText)
	}
	return b.String()
}

// handleAudio downloads the audio and tries to transcribe it. Returns
// a single string to substitute into the final prompt — either the
// transcription text, or a "not configured / failed" message.
func (c *composer) handleAudio(ctx context.Context, userID int64, fileID, name string) string {
	path, err := c.download(ctx, userID, fileID, name)
	if err != nil {
		log.Printf("composer: download audio: %v", err)
		return "[failed to download voice: " + err.Error() + "]"
	}
	if !c.transcriber.Available() {
		return "[Voice message received (" + path + "), but transcription is not configured]"
	}
	text, err := c.transcriber.Transcribe(ctx, path)
	if err != nil {
		log.Printf("composer: transcribe %s: %v", path, err)
		return "[failed to transcribe voice: " + err.Error() + "]"
	}
	if text == "" {
		return "[Voice message received but transcription is empty]"
	}
	return "[Voice message (transcription): " + text + "]"
}

// download gets the direct file URL from Telegram and saves it to disk.
// Target directory — inside the user's workdir (if set) or in TempDir.
// The filename is prefixed with a nanosecond timestamp to rule out collisions.
func (c *composer) download(ctx context.Context, userID int64, fileID, suggestedName string) (string, error) {
	url, err := c.api.GetFileDirectURL(fileID)
	if err != nil {
		return "", fmt.Errorf("get file url: %w", err)
	}

	dir := c.uploadDir(userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}

	name := suggestedName
	if name == "" {
		name = "file"
	}
	path := filepath.Join(dir, fmt.Sprintf("%d-%s", time.Now().UnixNano(), filepath.Base(name)))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// uploadDir — where to place downloaded files for the user. Inside workdir
// (so opencode/claude see them as "their own" and don't ask for permission),
// if workdir is set; otherwise fallback to TempDir.
func (c *composer) uploadDir(userID int64) string {
	if c.workDirOf != nil {
		if wd := c.workDirOf(userID); wd != "" {
			return filepath.Join(wd, ".tmp")
		}
	}
	return filepath.Join(os.TempDir(), "tg-bot", fmt.Sprintf("%d", userID))
}
