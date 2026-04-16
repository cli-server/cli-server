package executorregistry

import (
	"bufio"
	"encoding/json"
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
// given HTTP request over it, reads back the HTTP response, decodes it into
// an ExecuteResponse, and closes the stream before returning.
func (tr *TunnelRegistry) ExecViaTunnel(executorID string, httpReq *http.Request) (ExecuteResponse, error) {
	session, ok := tr.Get(executorID)
	if !ok {
		return ExecuteResponse{}, fmt.Errorf("no tunnel for executor %s", executorID)
	}

	stream, err := session.Open()
	if err != nil {
		return ExecuteResponse{}, fmt.Errorf("open yamux stream: %w", err)
	}
	defer stream.Close()

	if err := httpReq.Write(stream); err != nil {
		return ExecuteResponse{}, fmt.Errorf("write http request: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(stream), httpReq)
	if err != nil {
		return ExecuteResponse{}, fmt.Errorf("read http response: %w", err)
	}
	defer resp.Body.Close()

	var result ExecuteResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ExecuteResponse{}, fmt.Errorf("decode response: %w", err)
	}

	return result, nil
}
