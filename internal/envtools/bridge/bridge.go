package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/agentserver/agentserver/internal/relaypb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

// relayProtocolVersion matches `RELAY_MESSAGE_FRAME_VERSION` in codex's
// codex-rs/exec-server/src/relay.rs.
const relayProtocolVersion uint32 = 1

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

	// Relay multiplex state. We use one stream_id (random UUID) per
	// BridgeClient — codex's relay layer demultiplexes by stream_id and
	// spawns a fresh JSON-RPC session per new stream_id, so each env-mcp
	// child gets an isolated exec-server session on its own stream.
	streamID string
	// writeSeq is the monotonically increasing seq number our Data
	// frames carry. Codex doesn't currently enforce ordering on it but
	// the protobuf field is non-optional. Wraps via uint32 overflow.
	writeSeq atomic.Uint32
	// writeMu serialises ws.Write calls — nhooyr's websocket lib allows
	// only one concurrent writer.
	writeMu sync.Mutex
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
		ws:       ws,
		pending:  make(map[int64]chan *JSONRPCMessage),
		closed:   make(chan struct{}),
		cancel:   cancel,
		logger:   logger,
		streamID: uuid.NewString(),
	}

	// Send the Resume frame to announce our stream_id. codex's relay
	// reader looks for this before forwarding any Data frames into a
	// virtual stream.
	if err := bc.writeRelayFrame(ctx, &relaypb.RelayMessageFrame{
		Version:  relayProtocolVersion,
		StreamId: bc.streamID,
		Body: &relaypb.RelayMessageFrame_Resume{
			Resume: &relaypb.RelayResume{NextSeq: 0},
		},
	}); err != nil {
		_ = ws.Close(websocket.StatusInternalError, "resume write failed")
		cancel()
		return nil, fmt.Errorf("send relay resume: %w", err)
	}

	go bc.readLoop(loopCtx)
	return bc, nil
}

// writeRelayFrame marshals + sends a single RelayMessageFrame as one
// binary ws message. Concurrent writes are serialised via writeMu.
func (bc *BridgeClient) writeRelayFrame(ctx context.Context, f *relaypb.RelayMessageFrame) error {
	body, err := proto.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal relay frame: %w", err)
	}
	bc.writeMu.Lock()
	defer bc.writeMu.Unlock()
	return bc.ws.Write(ctx, websocket.MessageBinary, body)
}

// writeJSONRPC wraps a JSON-RPC payload in a relay Data frame and sends.
func (bc *BridgeClient) writeJSONRPC(ctx context.Context, jsonBytes []byte) error {
	seq := bc.writeSeq.Add(1) - 1 // start at 0
	return bc.writeRelayFrame(ctx, &relaypb.RelayMessageFrame{
		Version:  relayProtocolVersion,
		StreamId: bc.streamID,
		Body: &relaypb.RelayMessageFrame_Data{
			Data: &relaypb.RelayData{
				Seq:          seq,
				SegmentIndex: 0,
				SegmentCount: 1,
				Payload:      jsonBytes,
			},
		},
	})
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
	if err := bc.writeJSONRPC(ctx, out); err != nil {
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
	return bc.writeJSONRPC(ctx, out)
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
		mt, data, err := bc.ws.Read(ctx)
		if err != nil {
			bc.mu.Lock()
			bc.closeErr = err
			bc.mu.Unlock()
			return
		}
		// codex v0.131-alpha.22+ sends every frame as binary protobuf-
		// wrapped RelayMessageFrame. Reject text frames outright.
		if mt != websocket.MessageBinary {
			bc.logger.Warn("bridge: ignoring non-binary frame", "type", mt.String(), "len", len(data))
			continue
		}
		var frame relaypb.RelayMessageFrame
		if err := proto.Unmarshal(data, &frame); err != nil {
			bc.logger.Warn("bridge: invalid relay frame", "err", err, "len", len(data))
			continue
		}
		if frame.Version != relayProtocolVersion {
			bc.logger.Warn("bridge: unsupported relay protocol version", "version", frame.Version)
			continue
		}
		// Ignore frames for other stream_ids (shouldn't happen on our
		// connection but the protocol allows multiplexing).
		if frame.StreamId != bc.streamID {
			continue
		}
		switch body := frame.Body.(type) {
		case *relaypb.RelayMessageFrame_Data:
			if body.Data == nil || len(body.Data.Payload) == 0 {
				continue
			}
			var msg JSONRPCMessage
			if err := json.Unmarshal(body.Data.Payload, &msg); err != nil {
				bc.logger.Warn("bridge: malformed json-rpc inside data frame", "err", err, "len", len(body.Data.Payload))
				continue
			}
			if msg.ID == nil {
				// Server-pushed notification (process/exited, etc) —
				// env-mcp polls instead, drop.
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
		case *relaypb.RelayMessageFrame_Reset_:
			reason := ""
			if body.Reset_ != nil {
				reason = body.Reset_.Reason
			}
			bc.logger.Info("bridge: relay reset", "reason", reason)
			bc.mu.Lock()
			bc.closeErr = fmt.Errorf("relay reset: %s", reason)
			bc.mu.Unlock()
			return
		case *relaypb.RelayMessageFrame_AckFrame,
			*relaypb.RelayMessageFrame_Resume,
			*relaypb.RelayMessageFrame_Heartbeat:
			// Acknowledged but no state to update yet.
		default:
			bc.logger.Warn("bridge: unknown relay body type", "frame", &frame)
		}
	}
}
