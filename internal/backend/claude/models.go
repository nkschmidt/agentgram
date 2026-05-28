package claude

import "encoding/json"

// ---------- JSON models for stream-JSON events from the claude CLI ----------
//
// Here — only the narrow minimum we actually read; the full schema
// (system/init, assistant/message, user/tool_result, result/success)
// is defined by the claude CLI itself.

type event struct {
	Type      string        `json:"type"`
	Subtype   string        `json:"subtype,omitempty"`
	SessionID string        `json:"session_id,omitempty"`
	Message   *eventMessage `json:"message,omitempty"`
	Result    string        `json:"result,omitempty"`
}

type eventMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// contentBlock covers all block types that appear in content
// (text, tool_use, thinking). Raw input for tool — RawMessage,
// so it can be passed to toolfmt without premature parsing.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
}
