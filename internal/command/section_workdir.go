package command

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/schmidt/agentgram/internal/router"
)

// WorkDirRepo — per-user working directory in which backend agents are launched.
type WorkDirRepo interface {
	WorkDirOf(userID int64) string
	SetWorkDirOf(userID int64, path string) error
}

// WorkDirSection — /settings section: per-user working directory.
// UI — file-browser with subdirectory navigation. callback_data:
//
//	settings:work_dir:view             — section screen
//	settings:work_dir:edit             — start the browser
//	settings:work_dir:up               — parent folder
//	settings:work_dir:enter:<idx>      — enter subfolder by index
//	settings:work_dir:apply            — save current path
//	settings:work_dir:reset            — clear value
type WorkDirSection struct {
	repo WorkDirRepo

	mu         sync.Mutex
	browsePath map[int64]string // userID → current folder in the browser
}

// maxBrowseEntries — Telegram holds at most 100 buttons per keyboard.
// If a folder has more — extras are silently truncated.
const maxBrowseEntries = 100

func NewWorkDirSection(repo WorkDirRepo) *WorkDirSection {
	return &WorkDirSection{repo: repo, browsePath: map[int64]string{}}
}

func (*WorkDirSection) Slug() string { return "work_dir" }

func (*WorkDirSection) MenuButton() InlineButton {
	return InlineButton{Text: "📂 Working directory", Data: "settings:work_dir:view"}
}

func (s *WorkDirSection) ResetState(userID int64) {
	s.mu.Lock()
	delete(s.browsePath, userID)
	s.mu.Unlock()
}

// Accept — the section doesn't use text input.
func (*WorkDirSection) Accept(_ context.Context, _ router.Message, _ Replier) (bool, error) {
	return false, nil
}

func (s *WorkDirSection) Handle(ctx context.Context, cb router.Callback, r Replier, sub []string) error {
	action := ""
	if len(sub) > 0 {
		action = sub[0]
	}
	switch action {
	case "", "view":
		s.ResetState(cb.UserID)
		if err := r.Edit(ctx, cb.ChatID, cb.MessageID, s.viewText(cb.UserID), s.viewKeyboard(cb.UserID)); err != nil {
			return err
		}
		return r.Answer(ctx, cb.ID, "")
	case "edit":
		start := s.repo.WorkDirOf(cb.UserID)
		if start == "" {
			if home, err := os.UserHomeDir(); err == nil {
				start = home
			} else {
				start = "/"
			}
		}
		s.setBrowse(cb.UserID, start)
		return s.showBrowse(ctx, cb, r)
	case "up":
		current := s.getBrowse(cb.UserID)
		if current == "" {
			return r.Answer(ctx, cb.ID, "")
		}
		s.setBrowse(cb.UserID, filepath.Dir(current))
		return s.showBrowse(ctx, cb, r)
	case "enter":
		if len(sub) < 2 {
			return r.Answer(ctx, cb.ID, "")
		}
		idx, err := strconv.Atoi(sub[1])
		if err != nil {
			return r.Answer(ctx, cb.ID, "")
		}
		current := s.getBrowse(cb.UserID)
		if current == "" {
			return r.Answer(ctx, cb.ID, "")
		}
		entries, _ := listSubdirs(current, maxBrowseEntries)
		if idx < 0 || idx >= len(entries) {
			return r.Answer(ctx, cb.ID, "")
		}
		s.setBrowse(cb.UserID, filepath.Join(current, entries[idx]))
		return s.showBrowse(ctx, cb, r)
	case "apply":
		current := s.getBrowse(cb.UserID)
		if current == "" {
			return r.Answer(ctx, cb.ID, "")
		}
		if err := s.repo.SetWorkDirOf(cb.UserID, current); err != nil {
			return r.Answer(ctx, cb.ID, "Error: "+err.Error())
		}
		s.ResetState(cb.UserID)
		if err := r.Edit(ctx, cb.ChatID, cb.MessageID, "📂 Working directory set:\n"+current, nil); err != nil {
			return err
		}
		return r.Answer(ctx, cb.ID, "Applied")
	case "reset":
		if err := s.repo.SetWorkDirOf(cb.UserID, ""); err != nil {
			return r.Answer(ctx, cb.ID, "Error: "+err.Error())
		}
		if err := r.Edit(ctx, cb.ChatID, cb.MessageID, "🚫 Working directory reset.\nAgents will run in the bot's current directory.", nil); err != nil {
			return err
		}
		return r.Answer(ctx, cb.ID, "Reset")
	default:
		return r.Answer(ctx, cb.ID, "")
	}
}

// ---------- internals ----------

func (s *WorkDirSection) viewText(userID int64) string {
	wd := s.repo.WorkDirOf(userID)
	if wd == "" {
		return "📂 Working directory\n\nNot set — agents run in the bot's current directory."
	}
	return "📂 Working directory\n\n" + wd
}

func (s *WorkDirSection) viewKeyboard(userID int64) InlineKeyboard {
	rows := [][]InlineButton{
		{{Text: "✏️ Edit", Data: "settings:work_dir:edit"}},
	}
	if s.repo.WorkDirOf(userID) != "" {
		rows[0] = append(rows[0], InlineButton{Text: "🚫 Reset", Data: "settings:work_dir:reset"})
	}
	rows = append(rows, []InlineButton{{Text: "◀ Back", Data: "settings:open"}})
	return rows
}

// showBrowse draws the browser screen: current path + subdirectory buttons.
func (s *WorkDirSection) showBrowse(ctx context.Context, cb router.Callback, r Replier) error {
	current := s.getBrowse(cb.UserID)
	entries, err := listSubdirs(current, maxBrowseEntries)
	if err != nil {
		text := "❌ Failed to read directory:\n" + current + "\n" + err.Error()
		if eErr := r.Edit(ctx, cb.ChatID, cb.MessageID, text, s.browseErrorKeyboard(current)); eErr != nil {
			return eErr
		}
		return r.Answer(ctx, cb.ID, "")
	}
	text := "📂 " + current
	if len(entries) == 0 {
		text += "\n\nNo subdirectories."
	}
	if eErr := r.Edit(ctx, cb.ChatID, cb.MessageID, text, s.browseKeyboard(current, entries)); eErr != nil {
		return eErr
	}
	return r.Answer(ctx, cb.ID, "")
}

// browseKeyboard: ".. (up)" on top, then list of subdirectories,
// then "✅ Apply" / "◀ Cancel".
func (s *WorkDirSection) browseKeyboard(current string, entries []string) InlineKeyboard {
	rows := make([][]InlineButton, 0, len(entries)+2)
	if current != "" && current != "/" && current != "." {
		rows = append(rows, []InlineButton{{Text: "📁 .. (up)", Data: "settings:work_dir:up"}})
	}
	for i, name := range entries {
		rows = append(rows, []InlineButton{{
			Text: "📁 " + truncatePathSegment(name, 32),
			Data: fmt.Sprintf("settings:work_dir:enter:%d", i),
		}})
	}
	rows = append(rows, []InlineButton{
		{Text: "✅ Apply", Data: "settings:work_dir:apply"},
		{Text: "◀ Cancel", Data: "settings:work_dir:view"},
	})
	return rows
}

func (s *WorkDirSection) browseErrorKeyboard(current string) InlineKeyboard {
	var rows [][]InlineButton
	if current != "" && current != "/" && current != "." {
		rows = append(rows, []InlineButton{{Text: "📁 .. (up)", Data: "settings:work_dir:up"}})
	}
	rows = append(rows, []InlineButton{{Text: "◀ Cancel", Data: "settings:work_dir:view"}})
	return rows
}

func (s *WorkDirSection) setBrowse(userID int64, path string) {
	s.mu.Lock()
	s.browsePath[userID] = path
	s.mu.Unlock()
}

func (s *WorkDirSection) getBrowse(userID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.browsePath[userID]
}

// listSubdirs returns subdirectory names, sorted, up to limit entries.
// Hidden ones (starting with ".") are skipped.
func listSubdirs(path string, limit int) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if limit > 0 && len(names) > limit {
		names = names[:limit]
	}
	return names, nil
}

func truncatePathSegment(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
