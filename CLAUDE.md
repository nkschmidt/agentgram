# Project `agentgram` — Telegram bot wrapper for CLI agents

Main project context. Read automatically at the start of every Claude Code session.

## What it is

A Go-based Telegram bot that proxies user messages into CLI agents (Claude Code, opencode). The user types messages in chat → the bot forwards them to the active backend → the model's response is streamed back as a single message with progress (typing, tool steps, final).

Modular architecture: adding a new backend / command / settings section doesn't require rewriting the core.

## Conventions

- **User-facing language is English.** All UI strings (`replier.Send`, buttons, toasts, error texts shown in chat) are English.
- **Code and comments are English too.** Names, package docs, doc-comments on functions/structs/fields, inline comments.
- **No parse mode.** `Send`/`Edit` — plain (paths and commands contain `_`, `*`, `` ` `` which break Markdown). `SendHTML`/`EditHTML` — only for the model's final answer via `MarkdownToHTML`, with a plain-text fallback.
- **Inline keyboard, not reply keyboard.** The user's input field stays free for talking to the agent.
- **tgbotapi is locked inside `internal/bot`.** It doesn't leak outwards.

## Architectural decisions

- **Stack:** Go 1.25+ (bumped by the MCP go-sdk), `go-telegram-bot-api/v5`, `joho/godotenv`, `modelcontextprotocol/go-sdk` (local MCP server). HTTP — stdlib, SSE parser is custom.
- **Access:** whitelist by `agentgram user_id`. If the whitelist is empty, the first writer is added automatically (autoseed). Messages from non-allowed users are dropped at the entry point.
- **Settings storage:** JSON file `settings.json` in the bot's cwd. Fields: `allowed_users`, `work_dirs` (per-user), `whisper_bin`, `whisper_model`. Thread-safe Store.
- **Working directory:** per-user. On change, the callback `OnWorkDirChange(userID)` restarts only that user's session.
- **Backend abstraction:** `Backend{Start, Send, Recv, Interrupt, Stop}` + `Factory(userID)` + `Registry`. Each adapter implements its own protocol.
  - `claude` — per-message `claude -p --output-format stream-json --verbose [--resume <id>]`. Multi-turn via `--resume`. Interrupt = SIGINT through `cmd.Cancel`.
  - `opencode` — HTTP API to a local `opencode serve`. **LazyServer** — one process for all users, started on the first opencode selection. Each user gets their own session on the server. `directory` is passed as a query parameter on every request. SSE for stream, POST `/message` is blocking (no timeout). Interrupt = POST `/abort`.
- **Response streaming:** one message in the chat, updated via Edit. Typing indicator (re-sent every 4s). An ⏹ Stop button hangs below the message (SIGINT for claude, POST `/abort` for opencode; launched in a goroutine — doesn't block the UI). The final is unified: `Chunk{Text, Final: true}` replaces the accumulated progress.
- **Agent ↔ user (MCP):** the bot runs a local MCP server (`internal/mcp`, official go-sdk over Streamable HTTP) giving agents three tools — `send_photo` / `send_document` (push a file straight into the chat, separate from the text answer) and `ask_user` (ask a question mid-task, rendered as inline buttons, also answerable by free text). A tool call carries no Telegram identity, so the bot mints a stable per-user Bearer token and the backends inject it into each agent's MCP client config; `getServer` resolves token→userID per request. All three go through `mcp.Bot`, implemented in `internal/bot` (tgbotapi stays locked there).
  - **ask_user is blocking:** the tool handler calls `Bot.AskUser`, which posts the question and blocks until the user taps a button or types a reply (intercepted in `handleCallback`/`handleMessage` before normal dispatch; commands like `/new_session` are exempt as an escape hatch) or ctx is cancelled (interrupt) / 10-min timeout. This works in headless because the answer returns over the MCP connection, not stdin — claude's native `AskUserQuestion` can't (no way to feed the result back), so it's disabled (`--disallowedTools`) and the agent is steered to `ask_user` via `mcp.AgentGuidance` (claude: `--append-system-prompt`; opencode: per-message `system` field).
  - Per-backend MCP wiring:
  - `claude` — per-message subprocess, so per-user `--mcp-config <inline JSON>` + `--allowedTools mcp__agentgram__*`. Fully multi-user-safe.
  - `opencode` — one shared `opencode serve`, so the only per-user hook is the project config: on `Start` the backend writes/merges `<workdir>/opencode.json` with the user's remote-MCP entry. Verified: opencode reads project MCP config per `directory` and connects per session, so this is multi-user-safe too.
  - **Duplicate suppression:** weak models (seen with opencode + qwen) sometimes emit the *same* `tools/call` twice in one turn (two distinct JSON-RPC ids, not a transport retry), so the file was delivered twice. `mcp.Server` collapses identical sends (same userID/tool/path/caption within `dedupWindow`) — delivers once, returns OK for the duplicate. claude doesn't double, so it's unaffected.
- **User attachments:** Document / Photo / Voice / Audio are downloaded to `<workdir>/.tmp/` (if workdir is set) or `os.TempDir()/agentgram/<userID>/`. The path goes into the prompt, the agent reads them via the Read tool. Voice is transcribed via ASR (whisper.cpp). The binary and model live in Settings; main does autodetect on startup, the user changes them via `/settings`.
- **Polling:** every update is handled in **a separate goroutine** — a long-running `Backend.Send` (e.g. a blocking POST to opencode) doesn't block update reception, the ⏹ button always works.
- **`/restart`:** hard `syscall.Exec` with no confirmation. The notification Send and exec run in goroutines so the command fires even if something is stuck.

## Package layout

Each layer = a separate Go package, depending only on packages above it in the list.

```
cmd/bot/main.go             — entry point, wiring, whisper autodetect,
                              OnWorkDirChange callback
internal/settings/          — JSON Store (AllowedUsers + WorkDirs per-user
                              + WhisperBin/Model). RWMutex, change callback.
internal/router/            — orchestrator: Message → (command → state → session),
                              Callback → CallbackHandler. Neutral types.
internal/backend/           — Backend interface + Factory(userID) + Registry.
                              Chunk{Text, Replace, Final, Err}.
  toolfmt/                  — shared ToolUse(name, input) for rendering tool steps.
  claude/                   — per-message `claude -p`, --resume for multi-turn.
    claude.go               — Backend struct + lifecycle.
    events.go               — consume / handleEvent / emitAssistant.
    models.go               — JSON schemas.
  opencode/                 — HTTP client for local `opencode serve`.
    lazy.go                 — LazyServer: one serve process for all, own ctx.
    server.go               — launch + wait for TCP readiness.
    client.go               — REST + SSE. directory as query on all requests.
                              Two http.Client: 30s timeout / no timeout.
    sse.go                  — text/event-stream parser.
    opencode.go             — Backend struct + lifecycle.
    events.go               — consume / updatePart / renderPart / collectFinalText.
                              Snapshot rebuild with Replace=true; user-echo filtered.
    models.go               — JSON schemas.
internal/session/manager.go — Manager: per-user, one active session,
                              auto-stop of the previous with a cancelled flag.
                              ChunkHandler callback, ActiveSessions/Active/
                              ActiveName/Interrupt/Stop/Shutdown.
internal/asr/               — ASR abstraction:
    transcriber.go          — interface Transcriber + Noop.
    whispercpp/             — implementation via whisper-cli. Provider functions
                              from Store, autodetect bin/model.
internal/bot/               — tgbotapi glue (the only package with tgbotapi):
    service.go              — Service: polling, allowlist, conversion
                              Update → router.Message/Callback. PublishMenu.
    replier.go              — command.Replier on top of tgbotapi.
    composer.go             — Text/Caption + Document/Photo/Voice/Audio → prompt.
                              Downloads into `<workdir>/.tmp/` or TempDir,
                              voice goes through Transcriber.
    sender.go               — Service implements mcp.Bot (SendPhoto/
                              SendDocument/AskUser) + userID→chatID tracking +
                              ask_user state (pendingAsk). handleMessage/
                              handleCallback intercept the user's reply/tap to
                              answer a pending question (commands exempt).
internal/mcp/               — local MCP server (official go-sdk, Streamable
                              HTTP) exposing send_photo / send_document /
                              ask_user to the agents. Per-user routing:
                              TokenFor(userID) mints a Bearer token,
                              getServer(*http.Request) resolves token→userID and
                              returns a per-user server whose tools call Bot.
                              ClaudeMCPConfig / OpencodeConfig build each
                              backend's MCP client config; AgentGuidance is the
                              system-prompt nudge to actually use the tools.
internal/command/           — command layer:
    replier.go              — Replier interface (Send/Edit/SendHTML/EditHTML/
                              Delete/Answer/Typing).
    registry.go             — Command + Registry + MenuItem. Handle deletes
                              the original command message.
    callbacks.go            — CallbackPrefixHandler + CallbackRouter.
    state.go                — StateAcceptor + StateChain.
    markup.go               — MarkdownToHTML for final answers.
    sessions.go             — /new_session: backend pick, current active.
    forwarder.go            — user text → Backend.Send (guard: stream.IsActive).
    stream.go               — StreamCoordinator: typing + Edit + ⏹ button
                              (interrupt in a goroutine).
    restart.go              — /restart: syscall.Exec without confirm.
    settings.go             — /settings entry-point + menu + slug dispatcher.
    section_allowed.go      — AllowedUsersSection (depends on AllowedUsersRepo).
    section_workdir.go      — WorkDirSection (browser, depends on WorkDirRepo).
    section_whisper.go      — WhisperSection (depends on WhisperRepo).
```

## Key interfaces

```go
// internal/backend
type Backend interface {
    Start(ctx context.Context) error
    Send(input string) error
    Recv() <-chan Chunk
    Interrupt() error  // soft SIGINT-equivalent
    Stop() error       // hard close
}
type Chunk struct {
    Text    string
    Replace bool  // text replaces accumulated buffer (stream continues)
    Final   bool  // text replaces + stream closes; Final without Text — close only
    Err     error
}
type Factory func(userID int64) Backend

// internal/router
type Message struct { UserID, ChatID int64; MessageID int; UserName, Text string }
type Callback struct { ID, Data, UserName string; UserID, ChatID int64; MessageID int }

// internal/command
type Command interface {
    Name() string
    Description() string
    Handle(ctx context.Context, msg router.Message, r Replier) error
}
type CallbackPrefixHandler interface {
    Prefix() string
    HandleCallback(ctx context.Context, cb router.Callback, r Replier) error
}
type StateAcceptor interface {
    AcceptText(ctx context.Context, msg router.Message, r Replier) (handled bool, err error)
}
type SettingsSection interface {
    Slug() string
    MenuButton() InlineButton
    Handle(ctx context.Context, cb router.Callback, r Replier, sub []string) error
    Accept(ctx context.Context, msg router.Message, r Replier) (bool, error)
    ResetState(userID int64)
}
type Replier interface {
    Send(ctx, chatID, text, kb) (msgID int, err error)
    Edit(ctx, chatID, msgID, text, kb) error
    SendHTML(ctx, chatID, text, kb) (msgID int, err error)
    EditHTML(ctx, chatID, msgID, text, kb) error
    Delete(ctx, chatID, msgID) error
    Answer(ctx, callbackID, toast) error
    Typing(ctx, chatID) error
}
```

## Message flow

```
Telegram update
  → bot.Service.handleUpdate (separate goroutine per update)
    → handleMessage: allowlist filter → composer.compose (file downloads,
      voice transcription) → router.Message
      → router.Dispatch: command → state → SessionForwarder
                          ↓                        ↓
                         Registry          Backend.Send (per-user backend)
                                                   ↓
                                           Backend executes (process / HTTP)
                                                   ↓
                                           Chunk via Recv → Manager.drain
                                                   ↓
                                           onChunk → StreamCoordinator.OnChunk
                                                   ↓
                                           Edit of a single chat message
                                                   ↓
                                           Final → MarkdownToHTML → EditHTML
                                                   (with plain fallback)

    → handleCallback: allowlist filter → router.Callback
      → router.DispatchCallback → CallbackRouter → matching prefix-handler
        (settings / new_session / stream / restart)
```

## Extension rules

- **New backend** = package `internal/backend/X` implementing `Backend` + registration in `backendReg`.
- **New command** = file in `internal/command/` implementing `Command` (+ optionally `CallbackPrefixHandler` / `StateAcceptor`) + registration in `registry` / `cbRouter` / `stateChain`.
- **New /settings section** = struct implementing `SettingsSection` (Slug + MenuButton + Handle + Accept + ResetState) in a `section_*.go` file + registration in `command.NewSettings(...)`. `SettingsCommand` itself isn't touched — OCP.
- **New ASR provider** = package in `internal/asr/X` implementing `Transcriber{Transcribe, Available}`.
- **Streaming UI changes** = only `StreamCoordinator`. Backends are not touched — `Chunk` semantics is stable.

## Run

```sh
# .env: TELEGRAM_BOT_TOKEN=<token>
go run ./cmd/bot
```

For voice: install whisper.cpp (`brew install whisper-cpp` on macOS) and ffmpeg (`brew install ffmpeg`); place the `ggml-base.bin` model into `~/.cache/whisper-cpp/` or set the path via `/settings → 🎙 Voice`.

For opencode: `opencode auth login` and configure the model in `~/.config/opencode/opencode.jsonc`.
