// Package mcp exposes a subset of the bot's capabilities to CLI agents
// (claude, opencode) over a local MCP server (Streamable HTTP transport).
//
// Right now the only capability is "send a file to the Telegram user": the
// agent calls the send_photo / send_document tool with a path, and the bot
// delivers that file to the chat. This lets an agent produce an image or a
// document mid-task and push it to the user directly, instead of only being
// able to return text.
//
// Per-user routing. A single MCP server serves every user. The tool call
// itself carries no Telegram identity, so the bot mints a stable per-user
// token (TokenFor) and the backends put it into the MCP client config as a
// Bearer header. On every HTTP request getServer resolves token -> userID and
// hands back a server instance whose tool handlers are bound to that userID.
//
// tgbotapi never reaches here: the actual delivery goes through the Sender
// interface, implemented in internal/bot.
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/schmidt/agentgram/internal/diffimg"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerName is the MCP server name. Agents namespace tools as
// mcp__<ServerName>__<tool>, so it also appears in claude's --allowedTools.
const ServerName = "agentgram"

// Tool names — single source of truth, used both when registering the tools
// (build) and when whitelisting them for claude (ClaudeMCPConfig), so the two
// can't drift (a missing entry makes the tool unavailable in headless mode).
const (
	toolSendPhoto    = "send_photo"
	toolSendDocument = "send_document"
	toolAskUser      = "ask_user"
	toolRenderDiff   = "render_diff"
)

// allTools lists every tool the server exposes.
var allTools = []string{toolSendPhoto, toolSendDocument, toolAskUser, toolRenderDiff}

// Bot is the bridge to the Telegram side for a resolved user. Implemented in
// internal/bot so tgbotapi stays locked there; satisfied structurally, mcp
// doesn't import bot.
type Bot interface {
	SendPhoto(ctx context.Context, userID int64, path, caption string) error
	SendDocument(ctx context.Context, userID int64, path, caption string) error
	// AskUser sends a question to the user and blocks until they answer — by
	// tapping one of options (if any) or typing free-form text — returning the
	// chosen/typed answer. ctx cancellation (interrupt) or a timeout aborts it.
	AskUser(ctx context.Context, userID int64, question string, options []string) (answer string, err error)
}

// AgentGuidance is injected into the agent's system prompt so it actually uses
// these tools (rather than its own broken-in-headless question UI or plain
// text). Backends wire it in their own way (claude: --append-system-prompt).
const AgentGuidance = "CRITICAL RULE — you are in a Telegram chat and CANNOT talk to the user mid-turn: they only see " +
	"your message at the very end, and cannot reply in between. Therefore, to ask ANY question, get ANY choice, or " +
	"confirm ANYTHING, you MUST call the `ask_user` tool and wait for its result. ALWAYS pass 2–5 short `options` so " +
	"the user can tap an answer (they may also type). This includes: clarifying questions, picking between approaches, " +
	"yes/no confirmation before destructive actions, and offering ways to proceed when you are blocked. " +
	"It is FORBIDDEN to write a question, a numbered list of choices, or phrases like \"which do you prefer?\", " +
	"\"let me know\", \"could you tell me\" as plain text — the user literally cannot answer that. If you catch " +
	"yourself about to ask in text, stop and call `ask_user` instead. " +
	"Also: `send_photo` to show an image inline, `send_document` to send a file, and `render_diff` to send the user " +
	"a picture of the repository's uncommitted changes (git diff)."

// Server is the local MCP endpoint shared by all users.
type Server struct {
	bot       Bot
	workDirOf func(userID int64) string // resolves a user's working directory; may be nil
	baseURL   string                    // http://host:port, filled by Listen

	mu      sync.Mutex
	tokens  map[int64]string      // userID -> token (stable for the process lifetime)
	byToken map[string]int64      // token -> userID (reverse index)
	cache   map[int64]*sdk.Server // per-user MCP server, tools bound to that userID

	dedupMu sync.Mutex
	recent  map[dedupKey]time.Time // recently delivered sends, for duplicate suppression
}

// dedupKey identifies a single logical "send this file" so identical calls can
// be collapsed.
type dedupKey struct {
	userID  int64
	tool    string
	path    string
	caption string
}

// dedupWindow is how long an identical send is treated as an accidental
// duplicate. Weak models sometimes emit the same tool call twice in one turn
// (observed with opencode + small models like qwen); claude doesn't. The window
// only needs to cover one assistant turn — a genuine re-send later still goes
// through.
const dedupWindow = 30 * time.Second

// NewServer creates the MCP server. Call Listen to start serving. workDirOf
// resolves a user's working directory (used by render_diff to locate the repo);
// pass nil if unavailable — render_diff then requires an explicit 'dir'.
func NewServer(bot Bot, workDirOf func(userID int64) string) *Server {
	return &Server{
		bot:       bot,
		workDirOf: workDirOf,
		tokens:    make(map[int64]string),
		byToken:   make(map[string]int64),
		cache:     make(map[int64]*sdk.Server),
		recent:    make(map[dedupKey]time.Time),
	}
}

// Listen binds addr (host:port; ":0" picks a free port) and serves the MCP
// endpoint at /mcp in a background goroutine. URL is valid after it returns.
func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mcp listen: %w", err)
	}
	s.baseURL = "http://" + ln.Addr().String()

	handler := sdk.NewStreamableHTTPHandler(s.getServer, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.auth(handler))

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// URL is the MCP endpoint agents connect to. Empty before Listen.
func (s *Server) URL() string {
	if s.baseURL == "" {
		return ""
	}
	return s.baseURL + "/mcp"
}

// TokenFor returns a stable token for the user, minting one on first use.
func (s *Server) TokenFor(userID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tokens[userID]; ok {
		return t
	}
	t := generateToken()
	s.tokens[userID] = t
	s.byToken[t] = userID
	return t
}

// auth rejects requests without a known token before they reach the MCP
// handler, so getServer always resolves to a real user.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.userFor(r) == 0 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) getServer(r *http.Request) *sdk.Server {
	userID := s.userFor(r)
	if userID == 0 {
		return nil
	}
	return s.serverFor(userID)
}

// userFor resolves the Bearer token from the request to a userID; 0 if unknown.
func (s *Server) userFor(r *http.Request) int64 {
	tok := bearer(r.Header.Get("Authorization"))
	if tok == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byToken[tok]
}

// serverFor returns the cached per-user MCP server, building it on first use.
func (s *Server) serverFor(userID int64) *sdk.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	if srv, ok := s.cache[userID]; ok {
		return srv
	}
	srv := s.build(userID)
	s.cache[userID] = srv
	return srv
}

// reserve atomically claims a send. It returns true if this is the first
// occurrence of the key within dedupWindow (caller should deliver), or false if
// an identical send was just made (caller should skip — it's a duplicate).
func (s *Server) reserve(k dedupKey) bool {
	now := time.Now()
	s.dedupMu.Lock()
	defer s.dedupMu.Unlock()
	if last, ok := s.recent[k]; ok && now.Sub(last) < dedupWindow {
		return false
	}
	s.recent[k] = now
	for kk, t := range s.recent { // opportunistic cleanup of stale entries
		if now.Sub(t) > dedupWindow {
			delete(s.recent, kk)
		}
	}
	return true
}

// release drops a reservation so a genuine retry isn't suppressed when the
// delivery itself failed.
func (s *Server) release(k dedupKey) {
	s.dedupMu.Lock()
	delete(s.recent, k)
	s.dedupMu.Unlock()
}

// sendArgs is the shared input for both file-sending tools.
type sendArgs struct {
	Path    string `json:"path"`
	Caption string `json:"caption,omitempty"`
}

// sendResult is the structured output: just an acknowledgement.
type sendResult struct {
	OK bool `json:"ok"`
}

// askArgs / askResult are the I/O for the ask_user tool.
type askArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

type askResult struct {
	Answer string `json:"answer"`
}

// renderDiffArgs is the input for the render_diff tool.
type renderDiffArgs struct {
	Dir string `json:"dir,omitempty"`
}

// build constructs an MCP server whose tools deliver to the given userID.
func (s *Server) build(userID int64) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: ServerName, Version: "1"}, nil)

	sdk.AddTool(srv, &sdk.Tool{
		Name: toolSendPhoto,
		Description: "Send an image file to the user in this Telegram chat, shown inline as a photo. " +
			"Use for images you want the user to view directly (png, jpg, webp, gif). " +
			"'path' is an absolute path or a path relative to the working directory. " +
			"'caption' is optional text shown under the photo. " +
			"This does not replace your normal text answer — it is an extra delivery channel.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in sendArgs) (*sdk.CallToolResult, sendResult, error) {
		k := dedupKey{userID, "send_photo", in.Path, in.Caption}
		if !s.reserve(k) {
			return okResult("already sent (duplicate call ignored)"), sendResult{OK: true}, nil
		}
		if err := s.bot.SendPhoto(ctx, userID, in.Path, in.Caption); err != nil {
			s.release(k)
			return errorResult(err), sendResult{}, nil
		}
		return okResult("photo sent"), sendResult{OK: true}, nil
	})

	sdk.AddTool(srv, &sdk.Tool{
		Name: toolSendDocument,
		Description: "Send any file to the user in this Telegram chat as a document (downloadable attachment). " +
			"Use for non-image files or images you want delivered as files rather than inline photos. " +
			"'path' is an absolute path or a path relative to the working directory. " +
			"'caption' is optional text shown with the document.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in sendArgs) (*sdk.CallToolResult, sendResult, error) {
		k := dedupKey{userID, "send_document", in.Path, in.Caption}
		if !s.reserve(k) {
			return okResult("already sent (duplicate call ignored)"), sendResult{OK: true}, nil
		}
		if err := s.bot.SendDocument(ctx, userID, in.Path, in.Caption); err != nil {
			s.release(k)
			return errorResult(err), sendResult{}, nil
		}
		return okResult("document sent"), sendResult{OK: true}, nil
	})

	sdk.AddTool(srv, &sdk.Tool{
		Name: toolAskUser,
		Description: "Ask the user a question and wait for their reply. " +
			"If you pass 'options', each is shown as a tappable button in the chat; the user may still answer with " +
			"free-form text instead. With no 'options' it's an open question answered by text. " +
			"Use this whenever you need the user to choose or clarify mid-task, instead of asking in your normal reply — " +
			"your normal reply is only delivered at the end of the turn. " +
			"Returns the user's answer (the chosen option's label, or whatever text they typed).",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in askArgs) (*sdk.CallToolResult, askResult, error) {
		ans, err := s.bot.AskUser(ctx, userID, in.Question, in.Options)
		if err != nil {
			return errorResult(err), askResult{}, nil
		}
		return okResult(ans), askResult{Answer: ans}, nil
	})

	sdk.AddTool(srv, &sdk.Tool{
		Name: toolRenderDiff,
		Description: "Render the repository's uncommitted changes (git diff HEAD plus untracked files) as image(s) " +
			"and send them to the user in this Telegram chat as documents (crisp, not recompressed). " +
			"Use when the user asks to see the diff as a picture. " +
			"'dir' is the repo directory to diff; omit it to use the user's current working directory. " +
			"Requires git and silicon on the host. Read-only — never modifies git state.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in renderDiffArgs) (*sdk.CallToolResult, sendResult, error) {
		dir := strings.TrimSpace(in.Dir)
		if dir == "" && s.workDirOf != nil {
			dir = s.workDirOf(userID)
		}
		if dir == "" {
			return errorResult(fmt.Errorf("no working directory set; pass 'dir'")), sendResult{}, nil
		}
		k := dedupKey{userID, toolRenderDiff, dir, ""}
		if !s.reserve(k) {
			return okResult("already rendered (duplicate call ignored)"), sendResult{OK: true}, nil
		}
		imgs, cleanup, err := diffimg.Render(ctx, dir)
		defer cleanup()
		if err != nil {
			s.release(k)
			return errorResult(err), sendResult{}, nil
		}
		if len(imgs) == 0 {
			return okResult("no uncommitted changes"), sendResult{OK: true}, nil
		}
		for _, img := range imgs {
			if err := s.bot.SendDocument(ctx, userID, img.Path, img.Caption); err != nil {
				s.release(k)
				return errorResult(err), sendResult{}, nil
			}
		}
		return okResult(fmt.Sprintf("sent %d diff image(s) as documents", len(imgs))), sendResult{OK: true}, nil
	})

	return srv
}

func okResult(text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}
}

// errorResult reports a tool failure back to the model (IsError) instead of a
// protocol error, so the agent sees the reason and can react.
func errorResult(err error) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}},
	}
}

func bearer(header string) string {
	const prefix = "Bearer "
	if strings.HasPrefix(header, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(header, prefix))
	}
	return ""
}

func generateToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "mcp_" + hex.EncodeToString(b)
}

// ClaudeMCPConfig returns the inline JSON for claude's --mcp-config flag and
// the tool names to whitelist via --allowedTools, for the given user.
// Returns ("", nil) if the server isn't listening yet.
func (s *Server) ClaudeMCPConfig(userID int64) (configJSON string, allowedTools []string) {
	if s.URL() == "" {
		return "", nil
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{
			ServerName: map[string]any{
				"type": "http",
				"url":  s.URL(),
				"headers": map[string]any{
					"Authorization": "Bearer " + s.TokenFor(userID),
				},
			},
		},
	}
	b, _ := json.Marshal(cfg)
	tools := make([]string, len(allTools))
	for i, name := range allTools {
		tools[i] = "mcp__" + ServerName + "__" + name
	}
	return string(b), tools
}

// OpencodeConfig returns the bytes of a project-level opencode.json that
// registers the bot's MCP server as a remote MCP for the given user.
// Returns nil if the server isn't listening yet.
func (s *Server) OpencodeConfig(userID int64) []byte {
	if s.URL() == "" {
		return nil
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		// ask_user blocks until the user answers; opencode otherwise cancels an
		// MCP tool after ~60s. Push the timeout to opencode's hard ceiling
		// (~120s, capped by the AI SDK's streamText step limit) to give the user
		// more time to answer. Beyond that opencode cancels regardless.
		"experimental": map[string]any{
			"mcp_timeout": 120000,
		},
		"mcp": map[string]any{
			ServerName: map[string]any{
				"type":    "remote",
				"url":     s.URL(),
				"enabled": true,
				"headers": map[string]any{
					"Authorization": "Bearer " + s.TokenFor(userID),
				},
			},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return b
}
