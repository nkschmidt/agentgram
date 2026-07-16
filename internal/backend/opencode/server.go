// Package opencode — backend adapter for the `opencode` CLI.
//
// Architecturally: opencode is a TUI agent, and unlike claude's headless
// mode via CLI, it has none. But it can run as an HTTP server: `opencode serve`
// starts a local API that we hit from Go. Multi-turn, interruption and the
// event stream are implemented on top of REST + SSE.
package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

const Name = "opencode"

// Server manages the lifecycle of the child `opencode serve` process.
// One server serves all sessions of all users — opencode allows multiple
// parallel sessions in a single process.
type Server struct {
	cmd     *exec.Cmd
	baseURL string
	token   string
	done    chan struct{} // closed when the process exits

	mu      sync.Mutex
	stopped bool
}

// StartServer launches `opencode serve` and waits until it starts accepting
// TCP connections. If the binary is not installed or the port is busy —
// returns an error, and main simply won't register opencode in the backend registry.
func StartServer(ctx context.Context, port int, hostname string) (*Server, error) {
	if hostname == "" {
		hostname = "127.0.0.1"
	}
	if port == 0 {
		port = 4096
	}

	token := generateToken()

	cmd := exec.CommandContext(ctx, "opencode", "serve",
		"--port", fmt.Sprintf("%d", port),
		"--hostname", hostname,
	)
	cmd.Env = append(os.Environ(), "OPENCODE_SERVER_PASSWORD="+token)
	// SIGINT gives opencode a chance to shut down cleanly.
	cmd.Cancel = func() error {
		return cmd.Process.Signal(os.Interrupt)
	}
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opencode serve stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("opencode serve stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("opencode serve start: %w", err)
	}

	go logPipe("opencode stdout", stdout)
	go logPipe("opencode stderr", stderr)

	addr := net.JoinHostPort(hostname, fmt.Sprintf("%d", port))
	if err := waitTCPReady(addr, 10*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("opencode serve not ready at %s: %w", addr, err)
	}

	baseURL := fmt.Sprintf("http://%s", addr)
	log.Printf("opencode server: ready at %s", baseURL)
	s := &Server{cmd: cmd, baseURL: baseURL, token: token, done: make(chan struct{})}
	// One goroutine owns cmd.Wait(); closing done lets LazyServer notice the
	// process died (so it can restart it) and lets Shutdown wait without a
	// second, racing Wait.
	go func() {
		_ = cmd.Wait()
		log.Printf("opencode server: process exited")
		close(s.done)
	}()
	return s, nil
}

// Alive reports whether the server process is still running.
func (s *Server) Alive() bool {
	if s == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// BaseURL — root URL of the server (http://host:port), without trailing slash.
func (s *Server) BaseURL() string { return s.baseURL }

// Client — HTTP client to work with this server. workDir — provider of the
// working directory; read on every request and passed as the query parameter
// `?directory=...`.
//
// Two HTTP clients:
//   - http (60s timeout) — short operations: session create, abort, delete.
//     Usually <2s; the generous ceiling tolerates a cold first session create
//     (opencode validating a fresh provider/model can briefly take ~30s) rather
//     than failing it with "Failed to start".
//   - stream (no timeout) — long-running operations: SSE /event and
//     POST /message (it's blocking in opencode — returns only when the
//     model has finished generating; may take minutes).
func (s *Server) Client(workDir func() string) *Client {
	return &Client{
		baseURL: s.baseURL,
		token:   s.token,
		http:    &http.Client{Timeout: 60 * time.Second},
		stream:  &http.Client{Timeout: 0},
		workDir: workDir,
	}
}

// Shutdown sends SIGINT to the server and waits. Worst case — kill via WaitDelay.
func (s *Server) Shutdown() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	s.mu.Unlock()

	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	<-s.done // the Wait goroutine closes this; cmd.WaitDelay bounds the wait
	return nil
}

func waitTCPReady(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timeout")
}

func logPipe(label string, r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			log.Printf("%s: %s", label, string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

func generateToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "tg_" + hex.EncodeToString(b)
}
