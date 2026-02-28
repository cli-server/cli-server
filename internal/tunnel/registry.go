package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

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

// Register adds a tunnel for the given sandbox.
func (r *Registry) Register(sandboxID string, conn *websocket.Conn) *Tunnel {
	t := newTunnel(sandboxID, conn)
	r.mu.Lock()
	// Close any existing tunnel for this sandbox.
	if old, ok := r.tunnels[sandboxID]; ok {
		old.Close()
	}
	r.tunnels[sandboxID] = t
	r.mu.Unlock()
	return t
}

// Unregister removes the tunnel for the given sandbox (only if it matches the provided tunnel).
func (r *Registry) Unregister(sandboxID string, t *Tunnel) {
	r.mu.Lock()
	if existing, ok := r.tunnels[sandboxID]; ok && existing == t {
		delete(r.tunnels, sandboxID)
	}
	r.mu.Unlock()
}

// Get returns the active tunnel for a sandbox, if any.
func (r *Registry) Get(sandboxID string) (*Tunnel, bool) {
	r.mu.RLock()
	t, ok := r.tunnels[sandboxID]
	r.mu.RUnlock()
	return t, ok
}

// StreamMessage holds a decoded stream frame header and its binary payload.
type StreamMessage struct {
	Header  StreamHeader
	Payload []byte
}

// streamWaiter receives StreamMessages for a pending request.
type streamWaiter struct {
	ch chan *StreamMessage
}

// Tunnel represents an active WebSocket tunnel to a local agent.
type Tunnel struct {
	SandboxID string
	Conn      *websocket.Conn
	pending   map[string]*streamWaiter
	mu        sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
}

func newTunnel(sandboxID string, conn *websocket.Conn) *Tunnel {
	t := &Tunnel{
		SandboxID: sandboxID,
		Conn:      conn,
		pending:   make(map[string]*streamWaiter),
		done:      make(chan struct{}),
	}
	go t.readLoop()
	return t
}

// Close shuts down the tunnel.
func (t *Tunnel) Close() {
	t.closeOnce.Do(func() {
		close(t.done)
		t.Conn.Close(websocket.StatusNormalClosure, "closing")
		// Drain all pending waiters.
		t.mu.Lock()
		for id, w := range t.pending {
			close(w.ch)
			delete(t.pending, id)
		}
		t.mu.Unlock()
	})
}

// Done returns a channel that is closed when the tunnel shuts down.
func (t *Tunnel) Done() <-chan struct{} {
	return t.done
}

// SendRequest sends a request frame through the tunnel and returns a channel
// for receiving streamed response messages.
func (t *Tunnel) SendRequest(ctx context.Context, header *RequestHeader, body []byte) (<-chan *StreamMessage, error) {
	ch := make(chan *StreamMessage, 64)
	w := &streamWaiter{ch: ch}
	t.mu.Lock()
	t.pending[header.ID] = w
	t.mu.Unlock()

	msg, err := EncodeFrame(header, body)
	if err != nil {
		t.mu.Lock()
		delete(t.pending, header.ID)
		t.mu.Unlock()
		return nil, fmt.Errorf("encode request frame: %w", err)
	}
	if err := t.Conn.Write(ctx, websocket.MessageBinary, msg); err != nil {
		t.mu.Lock()
		delete(t.pending, header.ID)
		t.mu.Unlock()
		return nil, fmt.Errorf("write request frame: %w", err)
	}

	return ch, nil
}

// CleanupRequest removes a pending waiter after the request is done.
func (t *Tunnel) CleanupRequest(requestID string) {
	t.mu.Lock()
	delete(t.pending, requestID)
	t.mu.Unlock()
}

// readLoop reads binary frames from the WebSocket and dispatches them to pending waiters.
func (t *Tunnel) readLoop() {
	defer t.Close()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, data, err := t.Conn.Read(ctx)
		cancel()
		if err != nil {
			select {
			case <-t.done:
				return
			default:
			}
			if !errors.Is(err, context.Canceled) {
				log.Printf("tunnel %s: read error: %v", t.SandboxID, err)
			}
			return
		}

		headerJSON, payload, err := DecodeFrameHeader(data)
		if err != nil {
			log.Printf("tunnel %s: failed to decode frame: %v", t.SandboxID, err)
			continue
		}

		var incoming IncomingHeader
		if err := json.Unmarshal(headerJSON, &incoming); err != nil {
			log.Printf("tunnel %s: failed to unmarshal header: %v", t.SandboxID, err)
			continue
		}

		t.mu.Lock()
		w, ok := t.pending[incoming.ID]
		t.mu.Unlock()
		if !ok {
			log.Printf("tunnel %s: no pending waiter for request %s", t.SandboxID, incoming.ID)
			continue
		}

		if incoming.Type == FrameTypeStream {
			var sh StreamHeader
			if err := json.Unmarshal(headerJSON, &sh); err != nil {
				log.Printf("tunnel %s: failed to unmarshal stream header: %v", t.SandboxID, err)
				continue
			}
			msg := &StreamMessage{Header: sh, Payload: payload}
			select {
			case w.ch <- msg:
			default:
			}
			if sh.Done {
				close(w.ch)
			}
		}
	}
}
