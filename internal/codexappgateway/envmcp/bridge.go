package envmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"nhooyr.io/websocket"
)

// BridgeClient wraps one WebSocket connection to /bridge/{exe_id} on
// codex-exec-gateway and exposes a JSON-RPC client interface that env-mcp
// uses to talk codex's exec-server protocol.
//
// Concurrency model: a single background goroutine reads frames and
// dispatches them to a per-id reply channel; Call() blocks on its
// channel until the goroutine delivers, the context is cancelled, or
// the connection closes.
type BridgeClient struct {
	ws       *websocket.Conn
	nextID   atomic.Int64
	mu       sync.Mutex
	pending  map[int64]chan *JSONRPCMessage
	closed   chan struct{}
	closeErr error
	cancel   context.CancelFunc
	logger   *slog.Logger
}

// DialBridge dials wsURL and, when authToken is non-empty, sets
// `Authorization: Bearer <authToken>` on the upgrade request. Returns
// once the WebSocket handshake completes; subsequent reads are pumped
// by a background goroutine.
//
// nhooyr.io/websocket does NOT request `permessage-deflate` by default —
// we rely on that, because codex's exec-server closes connections that
// do (see spec § PoC #2 gotchas).
func DialBridge(ctx context.Context, wsURL, authToken string, logger *slog.Logger) (*BridgeClient, error) {
	opts := &websocket.DialOptions{}
	if authToken != "" {
		opts.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + authToken}}
	}
	ws, _, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		return nil, fmt.Errorf("ws dial %s: %w", wsURL, err)
	}
	ws.SetReadLimit(-1) // exec-server can stream large process/read responses

	if logger == nil {
		logger = slog.Default()
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	bc := &BridgeClient{
		ws:      ws,
		pending: make(map[int64]chan *JSONRPCMessage),
		closed:  make(chan struct{}),
		cancel:  cancel,
		logger:  logger,
	}
	go bc.readLoop(loopCtx)
	return bc, nil
}

// Call sends a JSON-RPC request and blocks until the response arrives,
// the context is cancelled, or the connection closes.
func (bc *BridgeClient) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := bc.nextID.Add(1)
	ch := make(chan *JSONRPCMessage, 1)

	bc.mu.Lock()
	if bc.isClosedLocked() {
		bc.mu.Unlock()
		return nil, errors.New("bridge: connection closed")
	}
	bc.pending[id] = ch
	bc.mu.Unlock()
	defer func() {
		bc.mu.Lock()
		delete(bc.pending, id)
		bc.mu.Unlock()
	}()

	msg := JSONRPCMessage{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	out, err := json.Marshal(&msg)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	if err := bc.ws.Write(ctx, websocket.MessageText, out); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-bc.closed:
		bc.mu.Lock()
		err := bc.closeErr
		bc.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("bridge: connection closed")
	case reply := <-ch:
		if reply.Error != nil {
			return nil, fmt.Errorf("%s: %s (code=%d)", method, reply.Error.Message, reply.Error.Code)
		}
		return reply.Result, nil
	}
}

// Notify sends a JSON-RPC notification (no id, no reply expected).
func (bc *BridgeClient) Notify(ctx context.Context, method string, params json.RawMessage) error {
	bc.mu.Lock()
	closed := bc.isClosedLocked()
	bc.mu.Unlock()
	if closed {
		return errors.New("bridge: connection closed")
	}
	msg := JSONRPCMessage{JSONRPC: "2.0", Method: method, Params: params}
	out, err := json.Marshal(&msg)
	if err != nil {
		return fmt.Errorf("marshal notify: %w", err)
	}
	return bc.ws.Write(ctx, websocket.MessageText, out)
}

// Close shuts the connection. Safe to call repeatedly; first call wins.
func (bc *BridgeClient) Close() {
	bc.mu.Lock()
	if bc.isClosedLocked() {
		bc.mu.Unlock()
		return
	}
	close(bc.closed)
	bc.mu.Unlock()
	bc.cancel()
	_ = bc.ws.Close(websocket.StatusNormalClosure, "client closing")
}

func (bc *BridgeClient) isClosedLocked() bool {
	select {
	case <-bc.closed:
		return true
	default:
		return false
	}
}

func (bc *BridgeClient) readLoop(ctx context.Context) {
	defer func() {
		bc.mu.Lock()
		if !bc.isClosedLocked() {
			close(bc.closed)
		}
		bc.mu.Unlock()
	}()
	for {
		_, data, err := bc.ws.Read(ctx)
		if err != nil {
			bc.mu.Lock()
			bc.closeErr = err
			bc.mu.Unlock()
			return
		}
		var msg JSONRPCMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			bc.logger.Warn("bridge: dropping malformed frame", "err", err, "len", len(data))
			continue
		}
		if msg.ID == nil {
			// Server-pushed notification (process/exited, process/output, ...).
			// env-mcp polls process/read instead, so we drop these for now.
			// Future runtime/cancel work may want to consume them.
			continue
		}
		bc.mu.Lock()
		ch, ok := bc.pending[*msg.ID]
		bc.mu.Unlock()
		if !ok {
			continue
		}
		select {
		case ch <- &msg:
		default:
		}
	}
}
