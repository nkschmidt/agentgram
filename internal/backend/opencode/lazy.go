package opencode

import (
	"context"
	"sync"
)

// LazyServer — lazy holder of Server. The `opencode serve` process itself
// is started only on the first Get call (i.e. when opencode is first
// selected in /new_session). If startup failed — subsequent Get calls
// will try again (e.g. the user may have just installed the binary
// or freed up the port).
//
// The opencode process lifetime equals the LazyServer lifetime (from
// NewLazyServer to Shutdown), not the lifetime of a particular user's
// session. Otherwise stopping one session (e.g. when changing working
// directory via restart) would kill the server, and the next session
// would fail with connection refused. So the exec is bound to Lazy's
// own root-ctx, not the caller's.
type LazyServer struct {
	rootCtx    context.Context
	rootCancel context.CancelFunc
	port       int
	host       string

	mu     sync.Mutex
	server *Server
}

func NewLazyServer(port int, host string) *LazyServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &LazyServer{
		rootCtx:    ctx,
		rootCancel: cancel,
		port:       port,
		host:       host,
	}
}

// Get returns the already-started server or starts it. Thread-safe.
// Caller ctx is only used for possible logging/diagnostics;
// the process itself is bound to the long-lived rootCtx (see LazyServer comment).
func (l *LazyServer) Get(_ context.Context) (*Server, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.server != nil {
		return l.server, nil
	}
	s, err := StartServer(l.rootCtx, l.port, l.host)
	if err != nil {
		return nil, err
	}
	l.server = s
	return s, nil
}

// Shutdown gracefully stops the server: cancelling root-ctx sends SIGINT
// (via cmd.Cancel), Server.Shutdown additionally waits for the process to exit.
func (l *LazyServer) Shutdown() error {
	l.mu.Lock()
	server := l.server
	l.server = nil
	l.mu.Unlock()

	l.rootCancel()
	if server == nil {
		return nil
	}
	return server.Shutdown()
}
