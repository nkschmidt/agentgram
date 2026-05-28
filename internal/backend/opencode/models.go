package opencode

import "encoding/json"

// ---------- JSON event models ----------
//
// Exact schemas — in the opencode sources (packages/opencode/src/session/).
// Here — the narrow minimum we actually read.

type busEvent struct {
	Type       string        `json:"type"`
	Properties busEventProps `json:"properties"`
}

type busEventProps struct {
	SessionID string       `json:"sessionID,omitempty"`
	Part      *partPayload `json:"part,omitempty"`
	Error     busError     `json:"error,omitempty"`
}

// busError — struct from the session.error event. The server sends error
// as an object with a nested data.message, not as a plain string.
type busError struct {
	Name string `json:"name,omitempty"`
	Data struct {
		Message string `json:"message,omitempty"`
	} `json:"data,omitempty"`
}

func (e busError) message() string {
	if e.Data.Message != "" {
		return e.Data.Message
	}
	return e.Name
}

func (e busEvent) belongsToSession(sessionID string) bool {
	if e.Properties.SessionID == "" {
		return false
	}
	return e.Properties.SessionID == sessionID
}

// partPayload — common message-part structure. opencode sends several
// types ("text", "tool", "reasoning", ...) in the same shape, we handle
// only text and tool.
type partPayload struct {
	ID     string     `json:"id,omitempty"`
	CallID string     `json:"callID,omitempty"`
	Type   string     `json:"type"`
	Text   string     `json:"text,omitempty"`
	Tool   string     `json:"tool,omitempty"`
	State  *toolState `json:"state,omitempty"`
}

// partID — stable key for the part. For text-parts opencode sends id;
// for tool-parts there's also callID (equals id, duplicated).
func (p partPayload) partID() string {
	if p.ID != "" {
		return p.ID
	}
	return p.CallID
}

type toolState struct {
	Status string          `json:"status"` // pending / running / completed / error
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}
