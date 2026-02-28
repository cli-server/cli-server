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

// responseWaiter is a channel that receives either a ResponseFrame or StreamFrames.
type responseWaiter struct {
	responseCh chan *ResponseFrame
	streamCh   chan *StreamFrame
	isStream   bool
}

// Tunnel represents an active WebSocket tunnel to a local agent.
type Tunnel struct {
	SandboxID string
	Conn      *websocket.Conn
	pending   map[string]*responseWaiter
	mu        sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
}

func newTunnel(sandboxID string, conn *websocket.Conn) *Tunnel {
	t := &Tunnel{
		SandboxID: sandboxID,
		Conn:      conn,
		pending:   make(map[string]*responseWaiter),
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
			close(w.responseCh)
			if w.streamCh != nil {
				close(w.streamCh)
			}
			delete(t.pending, id)
		}
		t.mu.Unlock()
	})
}

// Done returns a channel that is closed when the tunnel shuts down.
func (t *Tunnel) Done() <-chan struct{} {
	return t.done
}

// SendRequest sends an HTTP request through the tunnel and waits for a complete response.
func (t *Tunnel) SendRequest(ctx context.Context, req *RequestFrame) (*ResponseFrame, error) {
	w := &responseWaiter{
		responseCh: make(chan *ResponseFrame, 1),
	}
	t.mu.Lock()
	t.pending[req.ID] = w
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
	}()

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request frame: %w", err)
	}
	if err := t.Conn.Write(ctx, websocket.MessageText, data); err != nil {
		return nil, fmt.Errorf("write request frame: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, errors.New("tunnel closed")
	case resp, ok := <-w.responseCh:
		if !ok {
			return nil, errors.New("tunnel closed")
		}
		return resp, nil
	}
}

// SendStreamRequest sends an HTTP request and returns a channel for streaming response frames.
func (t *Tunnel) SendStreamRequest(ctx context.Context, req *RequestFrame) (<-chan *StreamFrame, error) {
	ch := make(chan *StreamFrame, 64)
	w := &responseWaiter{
		responseCh: make(chan *ResponseFrame, 1),
		streamCh:   ch,
		isStream:   true,
	}
	t.mu.Lock()
	t.pending[req.ID] = w
	t.mu.Unlock()

	data, err := json.Marshal(req)
	if err != nil {
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, fmt.Errorf("marshal request frame: %w", err)
	}
	if err := t.Conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, fmt.Errorf("write request frame: %w", err)
	}

	return ch, nil
}

// CleanupStream removes a pending stream waiter after the stream is done.
func (t *Tunnel) CleanupStream(requestID string) {
	t.mu.Lock()
	delete(t.pending, requestID)
	t.mu.Unlock()
}

// readLoop reads frames from the WebSocket and dispatches them to pending waiters.
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
			log.Printf("tunnel %s: read error: %v", t.SandboxID, err)
			return
		}

		var incoming IncomingFrame
		if err := json.Unmarshal(data, &incoming); err != nil {
			log.Printf("tunnel %s: failed to unmarshal frame: %v", t.SandboxID, err)
			continue
		}

		t.mu.Lock()
		w, ok := t.pending[incoming.ID]
		t.mu.Unlock()
		if !ok {
			log.Printf("tunnel %s: no pending waiter for request %s", t.SandboxID, incoming.ID)
			continue
		}

		switch incoming.Type {
		case FrameTypeResponse:
			var resp ResponseFrame
			if err := json.Unmarshal(data, &resp); err != nil {
				log.Printf("tunnel %s: failed to unmarshal response: %v", t.SandboxID, err)
				continue
			}
			select {
			case w.responseCh <- &resp:
			default:
			}

		case FrameTypeStream:
			var sf StreamFrame
			if err := json.Unmarshal(data, &sf); err != nil {
				log.Printf("tunnel %s: failed to unmarshal stream frame: %v", t.SandboxID, err)
				continue
			}
			if w.streamCh != nil {
				select {
				case w.streamCh <- &sf:
				default:
				}
				if sf.Done {
					close(w.streamCh)
				}
			}
		}
	}
}
