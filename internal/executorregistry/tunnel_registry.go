package executorregistry

import (
	"bufio"
	"fmt"
	"net/http"
	"sync"

	"github.com/hashicorp/yamux"
)

// TunnelRegistry is an in-memory registry mapping executor_id to yamux.Session.
type TunnelRegistry struct {
	mu      sync.RWMutex
	tunnels map[string]*yamux.Session
}

// NewTunnelRegistry creates a new TunnelRegistry.
func NewTunnelRegistry() *TunnelRegistry {
	return &TunnelRegistry{
		tunnels: make(map[string]*yamux.Session),
	}
}

// Register adds a yamux session for the given executor. If a session already
// exists for the executor, the old session is closed before registering the new one.
func (tr *TunnelRegistry) Register(executorID string, session *yamux.Session) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if old, ok := tr.tunnels[executorID]; ok {
		old.Close()
	}
	tr.tunnels[executorID] = session
}

// Unregister closes and removes the yamux session for the given executor.
func (tr *TunnelRegistry) Unregister(executorID string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if session, ok := tr.tunnels[executorID]; ok {
		session.Close()
		delete(tr.tunnels, executorID)
	}
}

// Get returns the yamux session for the given executor, if one exists.
func (tr *TunnelRegistry) Get(executorID string) (*yamux.Session, bool) {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	session, ok := tr.tunnels[executorID]
	return session, ok
}

// ExecViaTunnel opens a yamux stream to the specified executor, writes the
// given HTTP request over it, and reads back the HTTP response.
func (tr *TunnelRegistry) ExecViaTunnel(executorID string, httpReq *http.Request) (*http.Response, error) {
	session, ok := tr.Get(executorID)
	if !ok {
		return nil, fmt.Errorf("no tunnel for executor %s", executorID)
	}

	stream, err := session.Open()
	if err != nil {
		return nil, fmt.Errorf("open yamux stream: %w", err)
	}

	if err := httpReq.Write(stream); err != nil {
		stream.Close()
		return nil, fmt.Errorf("write http request: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(stream), httpReq)
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("read http response: %w", err)
	}

	return resp, nil
}
