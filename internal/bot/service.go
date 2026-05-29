// Package bot — Telegram interaction service. Receives updates,
// runs them through the access filter, converts them to neutral types
// router.Message / router.Callback and passes them to the Dispatcher. This is
// the only package where dependencies on telegram-bot-api are allowed.
package bot

import (
	"context"
	"fmt"
	"log"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/schmidt/agentgram/internal/command"
	"github.com/schmidt/agentgram/internal/router"
)

// AccessGate decides whether the user is allowed to interact with the bot.
type AccessGate interface {
	IsAllowed(userID int64) bool
	AllowFirstIfEmpty(userID int64) (added bool, err error)
}

// Dispatcher routes already-authorized events further into the system.
type Dispatcher interface {
	Dispatch(ctx context.Context, msg router.Message) error
	DispatchCallback(ctx context.Context, cb router.Callback) error
}

// Service — top-level service orchestrating the Telegram bot's work.
type Service struct {
	api      *tgbotapi.BotAPI
	access   AccessGate
	composer *composer

	// chatIDs remembers the chat each user last wrote from, so the MCP Sender
	// (SendPhoto/SendDocument) can deliver files even though the agent only
	// knows the userID. For a private chat the chat id equals the user id —
	// that's also the fallback before anything is recorded.
	chatIDs sync.Map // userID -> chatID

	// asks holds in-flight ask_user questions per user. While one is pending,
	// the user's next button tap or text message is consumed as the answer
	// instead of being dispatched/forwarded to the agent.
	asks sync.Map // userID -> *pendingAsk
}

// New creates the service and immediately authorizes with Telegram — so we
// fail early and loudly if the token is invalid. transcriber may be Noop —
// then voice messages aren't transcribed (we tell the user directly).
// workDirOf — provider of the user's workdir for compose: attachments are
// downloaded inside workdir so the agents don't consider them external and
// don't ask for permission.
func New(token string, access AccessGate, transcriber Transcriber, workDirOf func(userID int64) string) (*Service, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("telegram auth: %w", err)
	}
	return &Service{
		api:      api,
		access:   access,
		composer: newComposer(api, transcriber, workDirOf),
	}, nil
}

// Replier returns a command.Replier implementation on top of the ready tgbotapi client.
// Returned via a separate method so main can assemble Router/commands
// before launching Run, passing the replier to dependencies.
func (s *Service) Replier() command.Replier {
	return &tgReplier{api: s.api}
}

// PublishMenu publishes the command list to Telegram (setMyCommands).
// After that the bot gets a Menu button to the left of the input field,
// which shows these commands; tap = send command.
// An empty list clears the menu.
func (s *Service) PublishMenu(items []command.MenuItem) error {
	cmds := make([]tgbotapi.BotCommand, 0, len(items))
	for _, it := range items {
		cmds = append(cmds, tgbotapi.BotCommand{Command: it.Name, Description: it.Description})
	}
	if _, err := s.api.Request(tgbotapi.NewSetMyCommands(cmds...)); err != nil {
		return fmt.Errorf("publish menu: %w", err)
	}
	return nil
}

// Run starts long polling and blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context, dispatcher Dispatcher) error {
	log.Printf("authorized as @%s", s.api.Self.UserName)

	updateCfg := tgbotapi.NewUpdate(0)
	updateCfg.Timeout = 60
	updates := s.api.GetUpdatesChan(updateCfg)

	for {
		select {
		case <-ctx.Done():
			s.api.StopReceivingUpdates()
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			// Update is processed in a separate goroutine so a long-running
			// Backend.Send (e.g. a blocking POST /message in opencode)
			// doesn't block polling — otherwise the ⏹ Stop button and any
			// other updates wait for the previous request to finish. All layers
			// below (session.Manager, StreamCoordinator, settings.Store)
			// are already thread-safe.
			go s.handleUpdate(ctx, update, dispatcher)
		}
	}
}

func (s *Service) handleUpdate(ctx context.Context, u tgbotapi.Update, d Dispatcher) {
	switch {
	case u.Message != nil && u.Message.From != nil:
		s.handleMessage(ctx, u.Message, d)
	case u.CallbackQuery != nil && u.CallbackQuery.From != nil:
		s.handleCallback(ctx, u.CallbackQuery, d)
	}
}

func (s *Service) handleMessage(ctx context.Context, m *tgbotapi.Message, d Dispatcher) {
	if !s.authorize(m.From.ID, m.From.UserName) {
		return
	}
	s.chatIDs.Store(m.From.ID, m.Chat.ID)
	// If the agent is waiting on an ask_user question, this message is the
	// answer — deliver it and stop, don't forward it as a new prompt. Commands
	// are exempt so the user can always escape (/new_session, /restart).
	if !isCommand(m.Text) && s.tryAnswerText(m.From.ID, m.Text) {
		return
	}
	// composer assembles the text from Text/Caption + downloads Document/Photo/
	// Voice/Audio and adds the corresponding tags (with local path for
	// documents/images and a transcription for audio). If there's
	// nothing at all — an empty string, we don't forward further.
	text := s.composer.compose(ctx, m)
	if text == "" {
		return
	}
	err := d.Dispatch(ctx, router.Message{
		UserID:    m.From.ID,
		UserName:  m.From.UserName,
		ChatID:    m.Chat.ID,
		MessageID: m.MessageID,
		Text:      text,
	})
	if err != nil {
		log.Printf("dispatch message from %d: %v", m.From.ID, err)
	}
}

func (s *Service) handleCallback(ctx context.Context, q *tgbotapi.CallbackQuery, d Dispatcher) {
	if !s.authorize(q.From.ID, q.From.UserName) {
		return
	}
	if q.Message == nil {
		return
	}
	s.chatIDs.Store(q.From.ID, q.Message.Chat.ID)
	// Answer-button taps for a pending ask_user question are handled here, not
	// dispatched to the regular callback router.
	if s.tryAnswerCallback(q.From.ID, q.Data) {
		_, _ = s.api.Request(tgbotapi.NewCallback(q.ID, "✓"))
		return
	}
	err := d.DispatchCallback(ctx, router.Callback{
		ID:        q.ID,
		Data:      q.Data,
		UserID:    q.From.ID,
		UserName:  q.From.UserName,
		ChatID:    q.Message.Chat.ID,
		MessageID: q.Message.MessageID,
	})
	if err != nil {
		log.Printf("dispatch callback from %d: %v", q.From.ID, err)
	}
}

// authorize lets the user through if they're whitelisted; if the whitelist
// is empty — adds this user as the first one (bootstrap bot owner).
func (s *Service) authorize(userID int64, userName string) bool {
	if s.access.IsAllowed(userID) {
		return true
	}
	added, err := s.access.AllowFirstIfEmpty(userID)
	if err != nil {
		log.Printf("autoseed failed for %d (@%s): %v", userID, userName, err)
		return false
	}
	if added {
		log.Printf("autoseed: added first allowed user %d (@%s)", userID, userName)
		return true
	}
	log.Printf("ignored: user %d (@%s) is not in allowlist", userID, userName)
	return false
}
