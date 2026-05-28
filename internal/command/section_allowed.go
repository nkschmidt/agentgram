package command

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/schmidt/agentgram/internal/router"
)

// AllowedUsersRepo — whitelist of allowed users.
type AllowedUsersRepo interface {
	ListAllowed() []int64
	AllowUser(userID int64) (added bool, err error)
	RevokeUser(userID int64) (removed bool, err error)
}

// AllowedUsersSection — /settings section managing the whitelist.
// Inside: list as buttons, tap → user screen with deletion,
// adding via pending state (user enters user_id as text).
type AllowedUsersSection struct {
	repo AllowedUsersRepo

	mu         sync.Mutex
	pendingAdd map[int64]bool // userID → whether we're waiting for a new user_id from them
}

func NewAllowedUsersSection(repo AllowedUsersRepo) *AllowedUsersSection {
	return &AllowedUsersSection{repo: repo, pendingAdd: map[int64]bool{}}
}

func (*AllowedUsersSection) Slug() string { return "allowed_users" }

func (*AllowedUsersSection) MenuButton() InlineButton {
	return InlineButton{Text: "🛡 Allowed users", Data: "settings:allowed_users:list"}
}

func (s *AllowedUsersSection) ResetState(userID int64) {
	s.mu.Lock()
	delete(s.pendingAdd, userID)
	s.mu.Unlock()
}

func (s *AllowedUsersSection) Handle(ctx context.Context, cb router.Callback, r Replier, sub []string) error {
	action := ""
	if len(sub) > 0 {
		action = sub[0]
	}
	switch action {
	case "", "list":
		s.ResetState(cb.UserID)
		if err := r.Edit(ctx, cb.ChatID, cb.MessageID, s.listText(), s.listKeyboard()); err != nil {
			return err
		}
		return r.Answer(ctx, cb.ID, "")
	case "add":
		s.setPendingAdd(cb.UserID, true)
		text := "Send the user_id to add (integer).\n\nTo cancel — press the button."
		if err := r.Edit(ctx, cb.ChatID, cb.MessageID, text, cancelKb("settings:allowed_users:list")); err != nil {
			return err
		}
		return r.Answer(ctx, cb.ID, "")
	case "view":
		if len(sub) < 2 {
			return r.Answer(ctx, cb.ID, "")
		}
		id, err := parseUserID(sub[1])
		if err != nil {
			return r.Answer(ctx, cb.ID, "")
		}
		return s.showUser(ctx, cb, r, id)
	case "delete":
		if len(sub) < 2 {
			return r.Answer(ctx, cb.ID, "")
		}
		id, err := parseUserID(sub[1])
		if err != nil {
			return r.Answer(ctx, cb.ID, "")
		}
		return s.delete(ctx, cb, r, id)
	default:
		return r.Answer(ctx, cb.ID, "")
	}
}

// Accept — text input. Activated only if the user has pendingAdd set.
func (s *AllowedUsersSection) Accept(ctx context.Context, msg router.Message, r Replier) (bool, error) {
	s.mu.Lock()
	waiting := s.pendingAdd[msg.UserID]
	if waiting {
		delete(s.pendingAdd, msg.UserID)
	}
	s.mu.Unlock()
	if !waiting {
		return false, nil
	}
	return true, s.applyAdd(ctx, msg, r)
}

// ---------- internals ----------

func (s *AllowedUsersSection) setPendingAdd(userID int64, on bool) {
	s.mu.Lock()
	if on {
		s.pendingAdd[userID] = true
	} else {
		delete(s.pendingAdd, userID)
	}
	s.mu.Unlock()
}

func (s *AllowedUsersSection) applyAdd(ctx context.Context, msg router.Message, r Replier) error {
	id, err := parseUserID(msg.Text)
	if err != nil {
		_, sErr := r.Send(ctx, msg.ChatID, "Doesn't look like a user_id: "+err.Error()+"\nOpen /settings and try again.", nil)
		return sErr
	}
	added, err := s.repo.AllowUser(id)
	if err != nil {
		_, sErr := r.Send(ctx, msg.ChatID, "Failed to save: "+err.Error(), nil)
		return sErr
	}
	prefix := fmt.Sprintf("User %d was already in the list.", id)
	if added {
		prefix = fmt.Sprintf("User %d added.", id)
	}
	_, err = r.Send(ctx, msg.ChatID, prefix+"\n\n"+s.listText(), s.listKeyboard())
	return err
}

func (s *AllowedUsersSection) showUser(ctx context.Context, cb router.Callback, r Replier, id int64) error {
	text := fmt.Sprintf("👤 User %d", id)
	if err := r.Edit(ctx, cb.ChatID, cb.MessageID, text, s.userKeyboard(id, id == cb.UserID)); err != nil {
		return err
	}
	return r.Answer(ctx, cb.ID, "")
}

func (s *AllowedUsersSection) delete(ctx context.Context, cb router.Callback, r Replier, id int64) error {
	if id == cb.UserID {
		return r.Answer(ctx, cb.ID, "Can't remove yourself")
	}
	removed, err := s.repo.RevokeUser(id)
	if err != nil {
		return r.Answer(ctx, cb.ID, "Error: "+err.Error())
	}
	toast := fmt.Sprintf("User %d was not in the list", id)
	if removed {
		toast = fmt.Sprintf("User %d removed", id)
	}
	if err := r.Edit(ctx, cb.ChatID, cb.MessageID, s.listText(), s.listKeyboard()); err != nil {
		return err
	}
	return r.Answer(ctx, cb.ID, toast)
}

func (s *AllowedUsersSection) listText() string {
	if len(s.repo.ListAllowed()) == 0 {
		return "🛡 Allowed users\n\nThe list is empty."
	}
	return "🛡 Allowed users"
}

func (s *AllowedUsersSection) listKeyboard() InlineKeyboard {
	ids := s.repo.ListAllowed()
	rows := make([][]InlineButton, 0, len(ids)+2)
	for _, id := range ids {
		rows = append(rows, []InlineButton{{
			Text: fmt.Sprintf("👤 %d", id),
			Data: fmt.Sprintf("settings:allowed_users:view:%d", id),
		}})
	}
	rows = append(rows, []InlineButton{{Text: "➕ Add", Data: "settings:allowed_users:add"}})
	rows = append(rows, []InlineButton{{Text: "◀ Back", Data: "settings:open"}})
	return rows
}

func (*AllowedUsersSection) userKeyboard(id int64, isSelf bool) InlineKeyboard {
	var rows [][]InlineButton
	if !isSelf {
		rows = append(rows, []InlineButton{{
			Text: "➖ Remove",
			Data: fmt.Sprintf("settings:allowed_users:delete:%d", id),
		}})
	}
	rows = append(rows, []InlineButton{{Text: "◀ Back to list", Data: "settings:allowed_users:list"}})
	return rows
}

func parseUserID(text string) (int64, error) {
	s := strings.TrimSpace(text)
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("expected an integer, got %q", s)
	}
	if id <= 0 {
		return 0, fmt.Errorf("user_id must be positive")
	}
	return id, nil
}
