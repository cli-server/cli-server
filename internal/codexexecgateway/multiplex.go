package codexexecgateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"nhooyr.io/websocket"
)

// inboundConn wraps the ws connection from a single
// `codex exec-server --remote` invocation, plus a stream_id → bridge
// route table so multiple concurrent /bridge/{exe_id} sessions can
// share it (per the 2026-05-17 multiplexing spec).
//
// Lifecycle: created when /codex-exec/{exe_id} accepts a ws; destroyed
// when the conn closes OR a fresh inbound evicts it. Survives across
// /bridge sessions — a bridge session ending does not close inbound.
//
// Concurrency:
//   - Exactly ONE goroutine reads ws.Read (the reader loop the inbound
//     handler spawns). All other readers will steal frames.
//   - writeMu serialises ws.Write — any bridge session can write.
//   - routesMu protects routes.
type inboundConn struct {
	exeID  string
	ws     *websocket.Conn
	logger *slog.Logger

	writeMu sync.Mutex // serialise ws.Write

	routesMu sync.RWMutex
	routes   map[string]*bridgeSession // stream_id → bridge

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

func newInboundConn(exeID string, ws *websocket.Conn, logger *slog.Logger, maxFrameBytes int64) *inboundConn {
	if logger == nil {
		logger = slog.Default()
	}
	if ws != nil {
		ws.SetReadLimit(maxFrameBytes)
	}
	return &inboundConn{
		exeID:  exeID,
		ws:     ws,
		logger: logger,
		routes: map[string]*bridgeSession{},
		closed: make(chan struct{}),
	}
}

// addRoute registers b under streamID. If a route for streamID already
// exists, it's evicted and returned (caller MUST close it). Returns nil
// when there was no prior route.
func (i *inboundConn) addRoute(streamID string, b *bridgeSession) *bridgeSession {
	i.routesMu.Lock()
	defer i.routesMu.Unlock()
	old := i.routes[streamID]
	i.routes[streamID] = b
	return old
}

// removeRoute removes streamID from the routes if and only if the
// stored value is still b. Safe under the race where the inbound
// reader is about to re-add a route for the same stream_id.
func (i *inboundConn) removeRoute(streamID string, b *bridgeSession) {
	i.routesMu.Lock()
	defer i.routesMu.Unlock()
	if i.routes[streamID] == b {
		delete(i.routes, streamID)
	}
}

func (i *inboundConn) lookup(streamID string) (*bridgeSession, bool) {
	i.routesMu.RLock()
	defer i.routesMu.RUnlock()
	b, ok := i.routes[streamID]
	return b, ok
}

// streamCount returns the number of currently registered routes (i.e.
// concurrent /bridge sessions for this executor). Used by the bridge
// handler to enforce MaxStreamsPerExecutor.
func (i *inboundConn) streamCount() int {
	i.routesMu.RLock()
	defer i.routesMu.RUnlock()
	return len(i.routes)
}

// write sends a frame to the inbound under writeMu. Concurrent writers
// (multiple bridge sessions) are serialised here.
func (i *inboundConn) write(ctx context.Context, mt websocket.MessageType, data []byte) error {
	i.writeMu.Lock()
	defer i.writeMu.Unlock()
	return i.ws.Write(ctx, mt, data)
}

// close marks the inbound as closed, fans out close to all registered
// bridge sessions, and closes the underlying ws. Idempotent.
func (i *inboundConn) close(err error) {
	i.closeOnce.Do(func() {
		i.closeErr = err
		close(i.closed)
		// Snapshot under lock, then close outside to avoid deadlocking
		// against any bridge holding routesMu via lookup().
		i.routesMu.Lock()
		routes := i.routes
		i.routes = map[string]*bridgeSession{}
		i.routesMu.Unlock()
		for _, b := range routes {
			b.close(errors.New("inbound conn closed"))
		}
		if i.ws != nil {
			_ = i.ws.Close(websocket.StatusNormalClosure, "inbound closed")
		}
	})
}

// bridgeSession is one /bridge/{exe_id} dial, associated with a single
// stream_id. The inbound's reader writes incoming frames for that
// stream_id into bridgeWS via write(). The bridge handler's per-session
// pump reads from bridgeWS and forwards to inbound.write().
type bridgeSession struct {
	streamID string
	inbound  *inboundConn
	bridgeWS *websocket.Conn

	writeMu sync.Mutex // serialise bridgeWS.Write (inbound reader is the only writer today, but defends against future use)

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

func newBridgeSession(streamID string, inbound *inboundConn, bridgeWS *websocket.Conn) *bridgeSession {
	return &bridgeSession{
		streamID: streamID,
		inbound:  inbound,
		bridgeWS: bridgeWS,
		closed:   make(chan struct{}),
	}
}

func (b *bridgeSession) write(ctx context.Context, mt websocket.MessageType, data []byte) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return b.bridgeWS.Write(ctx, mt, data)
}

func (b *bridgeSession) close(err error) {
	b.closeOnce.Do(func() {
		b.closeErr = err
		close(b.closed)
		if b.bridgeWS != nil {
			_ = b.bridgeWS.Close(websocket.StatusNormalClosure, "bridge session closed")
		}
	})
}
