package tunnel

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"sync"

	"github.com/hashicorp/yamux"
	"nhooyr.io/websocket"
)

// Registry tracks active WebSocket tunnels keyed by sandbox ID.
type Registry struct {
	mu      sync.RWMutex
	tunnels map[string]*Tunnel
}

// NewRegistry creates a new tunnel registry.
func NewRegistry() *Registry {
	return &Registry{
		tunnels: make(map[string]*Tunnel),
	}
}

// Register accepts a WebSocket connection, wraps it in WSConn + yamux,
// and registers the resulting Tunnel for the given sandbox.
func (r *Registry) Register(ctx context.Context, sandboxID string, ws *websocket.Conn) *Tunnel {
	t := newTunnel(ctx, sandboxID, ws)
	r.mu.Lock()
	if old, ok := r.tunnels[sandboxID]; ok {
		old.Close()
	}
	r.tunnels[sandboxID] = t
	r.mu.Unlock()
	return t
}

// Unregister removes the tunnel only if it matches the provided instance.
// Returns true if the tunnel was actually removed.
func (r *Registry) Unregister(sandboxID string, t *Tunnel) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.tunnels[sandboxID]; ok && existing == t {
		delete(r.tunnels, sandboxID)
		return true
	}
	return false
}

// Get returns the active tunnel for a sandbox.
func (r *Registry) Get(sandboxID string) (*Tunnel, bool) {
	r.mu.RLock()
	t, ok := r.tunnels[sandboxID]
	r.mu.RUnlock()
	return t, ok
}

// Tunnel represents an active multiplexed tunnel to a local agent.
// It wraps a WebSocket connection with yamux for stream multiplexing.
type Tunnel struct {
	SandboxID string
	mux       *yamux.Session
	wsConn    *WSConn
	done      chan struct{}
	closeOnce sync.Once

	// OnAgentInfo is called when the agent sends a control message with agent info.
	OnAgentInfo func(data json.RawMessage)
}

func newTunnel(ctx context.Context, sandboxID string, ws *websocket.Conn) *Tunnel {
	conn := NewWSConn(ctx, ws)
	session, err := ServerMux(conn)
	if err != nil {
		log.Printf("tunnel %s: failed to create yamux session: %v", sandboxID, err)
		conn.Close()
		done := make(chan struct{})
		close(done) // unblock waiters immediately
		return &Tunnel{
			SandboxID: sandboxID,
			done:      done,
		}
	}
	t := &Tunnel{
		SandboxID: sandboxID,
		mux:       session,
		wsConn:    conn,
		done:      make(chan struct{}),
	}
	go t.acceptLoop()
	return t
}

// acceptLoop accepts streams opened by the agent (control messages).
func (t *Tunnel) acceptLoop() {
	defer t.Close()
	for {
		stream, err := t.mux.Accept()
		if err != nil {
			return
		}
		go t.handleAgentStream(stream)
	}
}

// handleAgentStream processes a stream opened by the agent.
func (t *Tunnel) handleAgentStream(stream net.Conn) {
	defer stream.Close()
	streamType, _, err := ReadStreamHeader(stream)
	if err != nil {
		log.Printf("tunnel %s: read agent stream header: %v", t.SandboxID, err)
		return
	}
	switch streamType {
	case StreamTypeControl:
		data, err := io.ReadAll(stream)
		if err != nil {
			log.Printf("tunnel %s: read control data: %v", t.SandboxID, err)
			return
		}
		if t.OnAgentInfo != nil {
			t.OnAgentInfo(json.RawMessage(data))
		}
	default:
		log.Printf("tunnel %s: unexpected agent stream type: %d", t.SandboxID, streamType)
	}
}

// OpenHTTPStream opens a new yamux stream for proxying an HTTP request.
// The caller must close the returned body reader when done.
//
// Protocol:
//  1. Server writes: stream header (StreamTypeHTTP + HTTPStreamMeta with BodyLen)
//  2. Server writes: request body bytes (exactly BodyLen bytes)
//  3. Agent reads BodyLen bytes, processes request, then writes response.
//  4. Agent writes: stream header (StreamTypeHTTP + HTTPResponseMeta)
//  5. Agent writes: response body until stream close.
func (t *Tunnel) OpenHTTPStream(ctx context.Context, meta HTTPStreamMeta, reqBody []byte) (HTTPResponseMeta, io.ReadCloser, error) {
	if t.mux == nil {
		return HTTPResponseMeta{}, nil, yamux.ErrSessionShutdown
	}

	stream, err := t.mux.Open()
	if err != nil {
		return HTTPResponseMeta{}, nil, err
	}

	// Set body length in metadata so agent knows when request body ends.
	meta.BodyLen = len(reqBody)

	// Write stream header with HTTP metadata.
	metaJSON, err := MarshalStreamMeta(meta)
	if err != nil {
		stream.Close()
		return HTTPResponseMeta{}, nil, err
	}
	if err := WriteStreamHeader(stream, StreamTypeHTTP, metaJSON); err != nil {
		stream.Close()
		return HTTPResponseMeta{}, nil, err
	}

	// Write request body (agent reads exactly BodyLen bytes).
	if len(reqBody) > 0 {
		if _, err := stream.Write(reqBody); err != nil {
			stream.Close()
			return HTTPResponseMeta{}, nil, err
		}
	}

	// Read response header from agent.
	_, respMetaJSON, err := ReadStreamHeader(stream)
	if err != nil {
		stream.Close()
		return HTTPResponseMeta{}, nil, err
	}
	var respMeta HTTPResponseMeta
	if err := UnmarshalStreamMeta(respMetaJSON, &respMeta); err != nil {
		stream.Close()
		return HTTPResponseMeta{}, nil, err
	}

	// Return the stream as the response body reader. Caller must close it.
	return respMeta, stream, nil
}

// OpenTerminalStream opens a new yamux stream for bidirectional terminal I/O.
// The returned net.Conn carries raw terminal data in both directions.
func (t *Tunnel) OpenTerminalStream() (net.Conn, error) {
	if t.mux == nil {
		return nil, yamux.ErrSessionShutdown
	}
	stream, err := t.mux.Open()
	if err != nil {
		return nil, err
	}
	if err := WriteStreamHeader(stream, StreamTypeTerminal, nil); err != nil {
		stream.Close()
		return nil, err
	}
	return stream, nil
}

// Close shuts down the tunnel and underlying connections.
func (t *Tunnel) Close() {
	t.closeOnce.Do(func() {
		close(t.done)
		if t.mux != nil {
			t.mux.Close()
		}
		if t.wsConn != nil {
			t.wsConn.Close()
		}
	})
}

// Done returns a channel that is closed when the tunnel shuts down.
func (t *Tunnel) Done() <-chan struct{} {
	return t.done
}
