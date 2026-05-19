package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// Pool maintains one BridgeClient per exe_id, dialed lazily on
// first Get and reused across calls. Closed connections (detected via
// the BridgeClient's `closed` channel) are dropped and redialed
// transparently. Used by env-mcp tools to multiplex multiple executor
// targets behind one stdio MCP server.
type Pool struct {
	gatewayBaseURL string // e.g. wss://exec-gw.../bridge (exe_id appended per dial)
	token          string // workspace-scoped cap-token
	logger         *slog.Logger

	mu    sync.Mutex
	conns map[string]*BridgeClient
}

// NewPool returns a pool. gatewayBaseURL should be the prefix to
// which `/<exe_id>` is appended (i.e. include `/bridge` but no trailing
// slash); token is the workspace-scoped capability token issued for
// this turn.
func NewPool(gatewayBaseURL, token string, logger *slog.Logger) *Pool {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pool{
		gatewayBaseURL: strings.TrimRight(gatewayBaseURL, "/"),
		token:          token,
		logger:         logger,
		conns:          map[string]*BridgeClient{},
	}
}

// Get returns a live BridgeClient for exeID. Dials on first use (with
// codex exec-server `initialize` handshake performed in-band so the
// caller can start issuing process/* and fs/* calls immediately). On
// subsequent calls, returns the cached client if it's still open.
//
// Race-safety: dial happens outside the pool lock so a slow dial for
// exe_a doesn't block Get(exe_b). If two goroutines race to dial the
// same exe_id, both will dial but only one connection ends up in the
// map; the loser closes its connection and returns the winner's.
func (p *Pool) Get(ctx context.Context, exeID string) (*BridgeClient, error) {
	if exeID == "" {
		return nil, fmt.Errorf("Pool.Get: empty exe_id")
	}
	p.mu.Lock()
	if c, ok := p.conns[exeID]; ok {
		select {
		case <-c.closed:
			delete(p.conns, exeID)
		default:
			p.mu.Unlock()
			return c, nil
		}
	}
	p.mu.Unlock()

	url := p.gatewayBaseURL + "/" + exeID
	c, err := DialBridge(ctx, url, p.token, p.logger)
	if err != nil {
		return nil, fmt.Errorf("dial bridge %s: %w", exeID, err)
	}
	// Perform exec-server handshake before exposing the connection — the
	// first process/* or fs/* RPC would otherwise race the initialize.
	initParams, _ := json.Marshal(ExecInitializeParams{ClientName: "codex-env-mcp"})
	if _, err := c.Call(ctx, ExecMethodInitialize, initParams); err != nil {
		c.Close()
		return nil, fmt.Errorf("exec-server initialize for %s: %w", exeID, err)
	}
	if err := c.Notify(ctx, ExecMethodInitialized, nil); err != nil {
		c.Close()
		return nil, fmt.Errorf("exec-server initialized notify for %s: %w", exeID, err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.conns[exeID]; ok {
		select {
		case <-existing.closed:
			p.conns[exeID] = c
		default:
			c.Close() // lose the race
			return existing, nil
		}
	} else {
		p.conns[exeID] = c
	}
	return c, nil
}

// Close shuts down every pooled connection. Idempotent.
func (p *Pool) Close() {
	p.mu.Lock()
	conns := p.conns
	p.conns = map[string]*BridgeClient{}
	p.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}
