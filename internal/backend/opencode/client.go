package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// serverUser — username for HTTP Basic Auth. When OPENCODE_SERVER_PASSWORD
// is set, opencode protects the server with Basic Auth; the username defaults
// to "opencode" (overridable on the server via OPENCODE_SERVER_USERNAME, which
// we don't set). The password is the token we pass in OPENCODE_SERVER_PASSWORD.
const serverUser = "opencode"

// Client — thin HTTP client for the opencode server.
// workDir — provider of the current working directory. On every request
// it's added to the URL as ?directory=... — that's the only way to tell
// opencode which folder to work in (the field in the body is ignored).
//
// http — regular REST calls with a timeout (see Server.Client).
// stream — SSE subscription to /event, without timeout (long-lived).
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	stream  *http.Client
	workDir func() string
}

// CreateSession creates a new session and returns its ID.
func (c *Client) CreateSession(ctx context.Context) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/session", map[string]any{}, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("opencode: create session: empty id in response")
	}
	return out.ID, nil
}

// SendMessage sends a prompt to the session.
// Uses the stream client without timeout: opencode returns a response only
// after the model has finished generating (this can take minutes,
// especially with tool calls).
func (c *Client) SendMessage(ctx context.Context, sessionID, text, system string) error {
	payload := map[string]any{
		"sessionID": sessionID,
		"parts": []map[string]any{
			{"type": "text", "text": text},
		},
	}
	if system != "" {
		// Per-message system instruction — tells the agent to use the bot's MCP
		// tools. opencode merges it with its own system prompt.
		payload["system"] = system
	}
	return c.doWith(ctx, c.stream, http.MethodPost, "/session/"+sessionID+"/message", payload, nil)
}

// Abort interrupts the current request in the session (analog of SIGINT for claude).
func (c *Client) Abort(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodPost, "/session/"+sessionID+"/abort", nil, nil)
}

// DeleteSession deletes the session on the server (called on Stop).
func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodDelete, "/session/"+sessionID, nil, nil)
}

// Events opens an SSE subscription and returns an event channel.
func (c *Client) Events(ctx context.Context) (<-chan SSEEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.buildURL("/event"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.SetBasicAuth(serverUser, c.token)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode events: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("opencode events: HTTP %d: %s", resp.StatusCode, string(body))
	}
	out := make(chan SSEEvent, 64)
	go func() {
		defer resp.Body.Close()
		readSSE(ctx, resp.Body, out)
	}()
	return out, nil
}

// do — universal JSON call via the short client (30s timeout).
func (c *Client) do(ctx context.Context, method, path string, body, dst any) error {
	return c.doWith(ctx, c.http, method, path, body, dst)
}

// doWith — same as do, but lets you choose a specific http.Client
// (short or stream without timeout).
func (c *Client) doWith(ctx context.Context, httpClient *http.Client, method, path string, body, dst any) error {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opencode marshal: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.buildURL(path), bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.SetBasicAuth(serverUser, c.token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("opencode %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opencode %s %s: HTTP %d: %s", method, path, resp.StatusCode, string(raw))
	}
	if dst == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// buildURL glues baseURL + path and mixes in ?directory=...,
// if the provider returned a non-empty value.
func (c *Client) buildURL(path string) string {
	full := c.baseURL + path
	if c.workDir == nil {
		return full
	}
	wd := c.workDir()
	if wd == "" {
		return full
	}
	u, err := url.Parse(full)
	if err != nil {
		return full
	}
	q := u.Query()
	q.Set("directory", wd)
	u.RawQuery = q.Encode()
	return u.String()
}
