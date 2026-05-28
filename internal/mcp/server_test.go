package mcp

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// stubSender records the last delivery so the test can assert per-user routing.
type stubSender struct {
	mu                  sync.Mutex
	photoUser, docUser  int64
	photoPath, photoCap string
	docPath, docCap     string
	photoN, docN        int
}

func (s *stubSender) SendPhoto(_ context.Context, userID int64, path, caption string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.photoUser, s.photoPath, s.photoCap = userID, path, caption
	s.photoN++
	return nil
}

func (s *stubSender) SendDocument(_ context.Context, userID int64, path, caption string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docUser, s.docPath, s.docCap = userID, path, caption
	s.docN++
	return nil
}

type authRT struct {
	base  http.RoundTripper
	token string
}

func (a authRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if a.token != "" {
		r.Header.Set("Authorization", "Bearer "+a.token)
	}
	return a.base.RoundTrip(r)
}

func connect(t *testing.T, endpoint, token string) *sdk.ClientSession {
	t.Helper()
	tr := &sdk.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: authRT{http.DefaultTransport, token}},
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, tr, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return cs
}

func TestSendPhotoRoutesToUser(t *testing.T) {
	sender := &stubSender{}
	srv := NewServer(sender)
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	const userID = int64(42)
	token := srv.TokenFor(userID)

	cs := connect(t, srv.URL(), token)
	defer cs.Close()

	ctx := context.Background()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(tools.Tools))
	}

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name:      "send_photo",
		Arguments: map[string]any{"path": "/tmp/x.png", "caption": "hi"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %+v", res.Content)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.photoUser != userID || sender.photoPath != "/tmp/x.png" || sender.photoCap != "hi" {
		t.Fatalf("bad delivery: user=%d path=%q cap=%q", sender.photoUser, sender.photoPath, sender.photoCap)
	}
}

// TestDuplicateSendSuppressed reproduces the opencode case where a weak model
// emits the same tool call twice in one turn: the file must be delivered once.
func TestDuplicateSendSuppressed(t *testing.T) {
	sender := &stubSender{}
	srv := NewServer(sender)
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	token := srv.TokenFor(7)
	cs := connect(t, srv.URL(), token)
	defer cs.Close()
	ctx := context.Background()

	args := map[string]any{"path": "/tmp/README.md", "caption": ""}
	for i := 0; i < 3; i++ {
		if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "send_document", Arguments: args}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// A different caption is a distinct send and must go through.
	if _, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name:      "send_document",
		Arguments: map[string]any{"path": "/tmp/README.md", "caption": "v2"},
	}); err != nil {
		t.Fatalf("distinct call: %v", err)
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.docN != 2 {
		t.Fatalf("want 2 deliveries (1 deduped + 1 distinct), got %d", sender.docN)
	}
}

func TestUnknownTokenRejected(t *testing.T) {
	srv := NewServer(&stubSender{})
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	tr := &sdk.StreamableClientTransport{
		Endpoint:   srv.URL(),
		HTTPClient: &http.Client{Transport: authRT{http.DefaultTransport, "mcp_bogus"}},
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cs, err := client.Connect(ctx, tr, nil); err == nil {
		cs.Close()
		t.Fatal("expected connect to fail for unknown token")
	}
}
