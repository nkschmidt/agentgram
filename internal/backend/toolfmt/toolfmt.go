// Package toolfmt — common formatting of tool calls into strings for chat.
// Used by the claude and opencode adapters: both parse different JSON
// formats, but the visual representation in Telegram must be unified.
//
// Principle: tool name → emoji (case-insensitive), input → short
// "label" (file_path / command / pattern, etc). We don't show raw
// JSON objects — they're almost always unreadable in chat.
package toolfmt

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Tool names from different CLIs differ in case (claude — PascalCase,
// opencode — lowercase). We store lowercase, normalize input on lookup.
var toolEmoji = map[string]string{
	"read":         "📖",
	"write":        "✍️",
	"edit":         "✏️",
	"notebookedit": "📓",
	"bash":         "💻",
	"bashoutput":   "💻",
	"glob":         "📁",
	"grep":         "🔍",
	"webfetch":     "🌐",
	"websearch":    "🔎",
	"task":         "🧩",
	"todowrite":    "📝",
	"list":         "📋",
	"ls":           "📋",
}

// Internal reports whether a tool name belongs to the bot's own MCP server
// (agentgram — claude names it mcp__agentgram__x, opencode agentgram_x). These
// tools already have a visible effect in the chat (a sent file, or an ask_user
// question message), so their raw tool-call steps are pure noise in the
// progress view — especially when a weak model fires several at once — and the
// backends skip rendering them.
func Internal(name string) bool {
	return strings.Contains(strings.ToLower(name), "agentgram")
}

// IsAskUser reports whether the tool is the bot's ask_user tool (claude names
// it mcp__agentgram__ask_user, opencode agentgram_ask_user). Backends turn such
// a call into a KindQuestion event instead of an activity step.
func IsAskUser(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "agentgram") && strings.Contains(n, "ask_user")
}

// AskInput parses an ask_user tool input (the {question, options} object both
// backends receive in the tool call) so the question can be surfaced as a
// KindQuestion event. Returns empty question if the input isn't ready yet.
func AskInput(raw json.RawMessage) (question string, options []string) {
	var in struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}
	_ = json.Unmarshal(raw, &in)
	return in.Question, in.Options
}

// ToolUse renders a tool call into a string for chat.
// For example: "📖 Read · /path/to/file" or "💻 Bash · ls -la".
// If name is empty — "tool" is used. If there's no known emoji for name —
// fallback "🔧". If input is empty/unintelligible — only the name is shown.
func ToolUse(name string, input json.RawMessage) string {
	if name == "" {
		name = "tool"
	}
	emoji, ok := toolEmoji[strings.ToLower(name)]
	if !ok {
		emoji = "🔧"
	}
	if hint := summarize(input); hint != "" {
		return fmt.Sprintf("%s %s · %s", emoji, name, Truncate(hint, 200))
	}
	return fmt.Sprintf("%s %s", emoji, name)
}

// summarize tries to extract the most useful field from input. First a
// list of known keys (names differ between claude / opencode), then
// fallback to any first non-empty string field.
func summarize(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// input sometimes arrives as just a string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return s
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	keys := []string{
		"file_path", "filePath", "file", "filename",
		"command", "cmd",
		"pattern", "regex",
		"path", "target", "directory", "dir",
		"url", "uri",
		"query",
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if str, ok := v.(string); ok && str != "" {
				return str
			}
		}
	}
	for _, v := range m {
		if str, ok := v.(string); ok && str != "" {
			return str
		}
	}
	return ""
}

// Truncate truncates s to n characters, adding an ellipsis if needed.
// TrimSpace is applied before the length check.
func Truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
