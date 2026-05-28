# agentgram — Telegram bot wrapper for CLI agents

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go)](https://go.dev/) [![License: MIT](https://img.shields.io/badge/License-MIT-yellow?style=flat-square)](LICENSE)

A Telegram bot that hooks [Claude Code](https://claude.ai/code) and [OpenCode](https://github.com/opencode-ai/opencode) into Telegram — chat with AI agents straight from your messenger and drive your projects from any device.

## What is it?

The bot turns Telegram into a console for CLI agents. Send messages, get streamed responses, just like working in a terminal:

```
You: Refactor the auth middleware, extract it into a separate package
Bot: ⏳ Working...
      📖 Read: middleware.go
      📖 Read: auth.go
      ✏️ Edit: middleware.go
Bot: Done. Moved authentication into package auth, updated imports, all 12 tests pass.
```

## Features

### AI backends

- **Claude Code** — per-message CLI invocation with `--resume` for multi-turn sessions. Supports thinking, tool_use, result.
- **OpenCode** — HTTP API to a local `opencode serve` with SSE streaming and lazy server startup.
- **Modular architecture** — adding a new backend = one package implementing the `Backend` interface.

### In-chat streaming

- A single message updates as chunks arrive (typing + edit).
- An `⏹ Stop` button — soft interrupt (SIGINT / `/abort`).
- The final answer is formatted as HTML with a plain-text fallback.

### Attachments

- **Documents and photos** — downloaded to the server, the path is passed to the agent.
- **Voice messages** — transcribed via whisper.cpp right inside the bot.

### Files from the agent (MCP)

The bot runs a local [MCP](https://modelcontextprotocol.io/) server that gives agents two extra tools, so they can push files **back** to your chat — not just reply with text:

- **`send_photo`** — send an image inline as a photo (with optional caption).
- **`send_document`** — send any file as a downloadable attachment.

```
You: Plot the sales data from report.csv and send me the chart
Bot: 📊 Built the chart from 1,240 rows.
     [photo: sales_q3.png]  ← delivered via send_photo
```

Routing is per-user: the bot mints a Bearer token per user and wires it into each backend's MCP config, so a tool call always lands in the right chat. Works for both backends:

- **Claude Code** — token passed per invocation via `--mcp-config` + `--allowedTools`.
- **OpenCode** — registered as a remote MCP server in a per-workdir `opencode.json` (the bot writes/merges it on session start).

### Settings

- **Working directories** — per-user, folder browser via inline keyboard, up to 100 subdirectories per screen.
- **Whitelist** — access by `user_id`; the first writer is added automatically.
- **Voice** — paths to whisper-cli and the model are configured via `/settings`, with autodetect on startup.

### Commands

| Command | Description |
|---------|-------------|
| `/settings` | Allowed users, working directory, voice |
| `/new_session` | New session with backend selection |
| `/restart` | Restart the bot (in-place exec) |

## Quick start

### Requirements

- **Go 1.25+**
- **Telegram Bot Token** — get one from [@BotFather](https://t.me/botfather)
- **Claude Code CLI** — [install](https://claude.ai/code) (for the claude backend)
- **OpenCode** — [install](https://github.com/opencode-ai/opencode) (for the opencode backend)
- **whisper.cpp** — optional, for voice transcription

### Install

```bash
git clone https://github.com/nkschmidt/agentgram.git
cd agentgram
```

### Configure

```bash
cp .env.example .env
# Edit .env:
```

```env
TELEGRAM_BOT_TOKEN=<TOKEN>
```

On first run the bot will automatically:
1. Detect paths to `whisper-cli` and the model (if present on the system).
2. Save them to `settings.json`.
3. Add the first user to the whitelist.

### Run

```bash
go run ./cmd/bot
```

Write to the bot on Telegram — the first user is added to the whitelist automatically.

## Project structure

```
cmd/bot/main.go              — entry point
internal/settings            — runtime settings (JSON, RWMutex)
internal/router              — routing: Message → command / state / backend
internal/backend             — Backend interface + factory registry
  claude                     — per-message CLI with --resume
  opencode                   — HTTP API + SSE, lazy server
  toolfmt                    — tool-call rendering
internal/session             — per-user sessions, auto-stop, chunk handler
internal/command             — commands, streaming, callbacks, state machine
internal/bot                 — tgbotapi glue: service, replier, composer, sender
internal/mcp                 — local MCP server: send_photo / send_document
internal/asr                 — transcription (whisper.cpp + noop)
```

## Architecture

### Backend abstraction

Every AI backend implements a single interface:

```go
type Backend interface {
    Start(ctx context.Context) error
    Send(input string) error
    Recv() <-chan Chunk
    Interrupt() error
    Stop() error
}
```

Factories are registered in a global registry by name (`claude`, `opencode`). A new backend = a new package without touching the core.

### Sessions

`session.Manager` keeps one active session per user. When a new message is sent the previous session is stopped automatically. Chunks are delivered to the chat via `StreamCoordinator`.

### Settings

Stored in `settings.json` (bot cwd). Keys:

```json
{
  "allowed_users": [150041509],
  "work_dirs": {
    "150041509": "/path/to/project"
  },
  "whisper_bin": "/opt/homebrew/bin/whisper-cli",
  "whisper_model": "/path/to/ggml-base.bin"
}
```

- `allowed_users` — empty list = autoseed (the first user is added).
- `work_dirs` — when the directory changes, the user's session is restarted.
- `whisper_*` — autodetected on startup, changed via `/settings`.

## Deployment

### Local

```bash
go build -o bot ./cmd/bot
./bot
```

### VPS (systemd)

```bash
sudo tee /etc/systemd/system/agentgram.service > /dev/null <<EOF
[Unit]
Description=agentgram — Telegram AI Agent Bot
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/agentgram
ExecStart=/opt/agentgram/bot
Restart=on-failure
RestartSec=5
EnvironmentFile=/opt/agentgram/.env

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl enable --now agentgram
```

## Extending

### New backend

1. Create a package `internal/backend/X`.
2. Implement `Backend` + `Factory`.
3. Register it in `backendReg` in `main.go`.

### New command

1. Create a file in `internal/command/` with a `Command` implementation.
2. Register it in `registry` in `main.go`.

### New /settings section

1. Implement `SettingsSection` (Slug + MenuButton + Handle + Accept + ResetState).
2. Add it as `section_*.go` and register in `command.NewSettings(...)`.

## License

MIT
