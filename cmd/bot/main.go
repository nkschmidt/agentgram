// Command bot — entry point of the Telegram bot wrapper over CLI agents.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/schmidt/agentgram/internal/asr/whispercpp"
	"github.com/schmidt/agentgram/internal/backend"
	"github.com/schmidt/agentgram/internal/backend/claude"
	"github.com/schmidt/agentgram/internal/backend/opencode"
	"github.com/schmidt/agentgram/internal/bot"
	"github.com/schmidt/agentgram/internal/command"
	"github.com/schmidt/agentgram/internal/mcp"
	"github.com/schmidt/agentgram/internal/router"
	"github.com/schmidt/agentgram/internal/session"
	"github.com/schmidt/agentgram/internal/settings"
)

const envTokenKey = "TELEGRAM_BOT_TOKEN"

func main() {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("warn: cannot load .env: %v", err)
	}

	token := os.Getenv(envTokenKey)
	if token == "" {
		log.Fatalf("env %s is required (see .env.example)", envTokenKey)
	}

	store, err := settings.Open()
	if err != nil {
		log.Fatalf("%v", err)
	}

	// ASR for voice messages via whisper.cpp. Binary and model — in settings;
	// on first start we do autodetect and save what was found, the user can
	// change them later via /settings.
	if store.WhisperBin() == "" {
		if p := whispercpp.LookupBin(); p != "" {
			_ = store.SetWhisperBin(p)
			log.Printf("whisper: auto-detected bin %s", p)
		}
	}
	if store.WhisperModel() == "" {
		if p := whispercpp.LookupModel(); p != "" {
			_ = store.SetWhisperModel(p)
			log.Printf("whisper: auto-detected model %s", p)
		}
	}
	// Providers read the current values on every Transcribe —
	// settings changes apply immediately without restarting the bot.
	transcriber := whispercpp.New(store.WhisperBin, store.WhisperModel)

	botSvc, err := bot.New(token, store, transcriber, store.WorkDirOf)
	if err != nil {
		log.Fatalf("%v", err)
	}
	replier := botSvc.Replier()

	// Local MCP server: lets agents push files to the user's chat via the
	// send_photo / send_document tools. botSvc satisfies mcp.Sender structurally
	// (tgbotapi stays inside internal/bot). Per-user routing is by a Bearer token
	// the backends inject into each agent's MCP client config.
	mcpSrv := mcp.NewServer(botSvc)
	if err := mcpSrv.Listen("127.0.0.1:0"); err != nil {
		log.Printf("warn: mcp server not started, agents can't send files: %v", err)
	} else {
		log.Printf("mcp server: ready at %s", mcpSrv.URL())
	}

	// Backend registry. claude runs via CLI per-message.
	// opencode talks to a local `opencode serve`; we start the server itself
	// lazily — only when the user first selects opencode in /new_session.
	// Working directory — from settings, read on every process request.
	// Each backend also gets the bot's MCP server wired in per user.
	backendReg := backend.NewRegistry()
	backendReg.Register(claude.Name, claude.New(store.WorkDirOf, mcpSrv.ClaudeMCPConfig, mcp.AgentGuidance))

	opencodeLazy := opencode.NewLazyServer(4096, "127.0.0.1")
	defer opencodeLazy.Shutdown()
	backendReg.Register(opencode.Name, opencode.New(opencodeLazy, store.WorkDirOf, mcpSrv.OpencodeConfig, mcp.AgentGuidance))

	// Forward-declare sessionMgr — it's needed by the closure inside StreamCoordinator
	// (so the "⏹ Stop" button can send SIGINT to the process), but Manager itself
	// requires onChunk, which in turn calls streamCoord. The closure references
	// the variable we'll populate below — the runtime call happens after its
	// initialization.
	var sessionMgr *session.Manager

	streamCoord := command.NewStreamCoordinator(replier, func(userID int64) error {
		return sessionMgr.Interrupt(userID)
	})

	// onChunk: bridge between session.Manager (doesn't know about Telegram) and UI.
	// Intermediate chunks accumulate in a single message, the final one (Final=true)
	// replaces the accumulated text; a process error is shown separately.
	onChunk := func(s *session.Session, chunk backend.Chunk) {
		if chunk.Err != nil {
			streamCoord.Finish(s.UserID)
			text := fmt.Sprintf("⚠ Session %s ended: %v", s.Name, chunk.Err)
			if _, err := replier.Send(context.Background(), s.ChatID, text, nil); err != nil {
				log.Printf("notify exit: %v", err)
			}
			return
		}
		streamCoord.OnChunk(context.Background(), s.UserID, s.ChatID, chunk.Text, chunk.Replace, chunk.Final)
	}

	// Session manager: per-user, one active session. Shutdown in defer —
	// so we don't leave zombie processes when the bot stops.
	sessionMgr = session.NewManager(backendReg, onChunk)
	defer sessionMgr.Shutdown()

	// Commands and handlers. UI everywhere — inline keyboard in messages,
	// the user's input field is always free for dialogue with the CLI agent.
	settingsCmd := command.NewSettings(
		command.NewAllowedUsersSection(store),
		command.NewWorkDirSection(store),
		command.NewWhisperSection(store),
	)
	sessionsCmd := command.NewSessions(sessionMgr)
	restartCmd := command.NewRestart(func() {
		log.Println("tg-bot: restarting (syscall.Exec)")
		sessionMgr.Shutdown()
		_ = opencodeLazy.Shutdown()
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("restart: cannot resolve executable: %v", err)
		}
		// Small pause for graceful close of network connections
		// before replacing the process.
		time.Sleep(200 * time.Millisecond)
		if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
			log.Fatalf("restart: exec %s: %v", exe, err)
		}
	})

	registry := command.NewRegistry(replier)
	// /start is registered first so it appears at the top of the Menu button.
	// Its handler reads registry.Menu() at call time, so other commands
	// registered below are still picked up in the welcome list.
	startCmd := command.NewStart(registry.Menu)
	registry.Register(startCmd)
	registry.Register(settingsCmd)
	registry.Register(sessionsCmd)
	registry.Register(restartCmd)

	cbRouter := command.NewCallbackRouter(replier)
	cbRouter.Register(settingsCmd)
	cbRouter.Register(sessionsCmd)
	// restartCmd is intentionally without callback buttons — no confirmations,
	// so the command always fires, even if the Telegram API lags.
	cbRouter.Register(streamCoord)

	stateChain := command.NewStateChain(replier)
	stateChain.Add(settingsCmd)

	forwarder := command.NewForwarder(sessionMgr, replier, streamCoord)
	rt := router.New(stateChain, registry, cbRouter, forwarder)

	// When a specific user's working directory changes — restart only
	// THEIR active session (if any). Other users are not touched.
	store.SetOnWorkDirChange(func(userID int64) {
		s, ok := sessionMgr.Active(userID)
		if !ok {
			return
		}
		if err := sessionMgr.Start(context.Background(), s.UserID, s.ChatID, s.Name); err != nil {
			log.Printf("restart session for user %d: %v", s.UserID, err)
			return
		}
		if _, sErr := replier.Send(context.Background(), s.ChatID,
			"🔄 Working directory changed, session restarted.", nil); sErr != nil {
			log.Printf("notify restart: %v", sErr)
		}
	})

	if err := botSvc.PublishMenu(registry.Menu()); err != nil {
		log.Printf("warn: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Println("tg-bot: starting")
	if err := botSvc.Run(ctx, rt); err != nil {
		log.Fatalf("tg-bot: exited with error: %v", err)
	}
	log.Println("tg-bot: stopped")
}
