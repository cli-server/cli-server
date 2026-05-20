package codexexecgateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentserver/agentserver/internal/relaypb"
	"google.golang.org/protobuf/proto"
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

	// lastActivity tracks the most recent frame routed through this
	// session (either direction) as unix nanos. Read by the idle reaper
	// goroutine, written by inbound's reader and the bridge pump.
	// atomic.Int64 makes both safe under `-race` without a mutex.
	lastActivity atomic.Int64

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

func newBridgeSession(streamID string, inbound *inboundConn, bridgeWS *websocket.Conn) *bridgeSession {
	b := &bridgeSession{
		streamID: streamID,
		inbound:  inbound,
		bridgeWS: bridgeWS,
		closed:   make(chan struct{}),
	}
	b.touch()
	return b
}

// touch updates lastActivity to now. Safe for concurrent use.
func (b *bridgeSession) touch() {
	b.lastActivity.Store(time.Now().UnixNano())
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

// startIdleReaper runs until ctx is done or i.closed fires. Every
// idleTimeout/4 (clamped to >= 50ms), it scans i.routes and closes any
// session whose lastActivity is older than idleTimeout. On close, it
// sends a RelayMessageFrame{Reset{reason:"idle-timeout"}} on the inbound
// ws so the executor's relay layer can tear down its per-stream JSON-RPC
// session, removes the route, and closes the bridge ws.
//
// idleTimeout <= 0 disables the reaper (returns immediately).
func (i *inboundConn) startIdleReaper(ctx context.Context, idleTimeout time.Duration) {
	if idleTimeout <= 0 {
		return
	}
	tick := idleTimeout / 4
	if tick < 50*time.Millisecond {
		tick = 50 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-i.closed:
			return
		case <-t.C:
			i.reapIdle(ctx, idleTimeout)
		}
	}
}

// reapIdle scans the routes table for sessions whose lastActivity is
// older than idleTimeout, then for each: sends a Reset frame to the
// inbound (executor) ws, removes the route, and closes the bridge ws.
//
// The candidate list is collected under routesMu.RLock; the close work
// happens after the lock is released to avoid holding routesMu across
// blocking ws.Write calls.
func (i *inboundConn) reapIdle(ctx context.Context, idleTimeout time.Duration) {
	cutoff := time.Now().Add(-idleTimeout).UnixNano()
	i.routesMu.RLock()
	var candidates []*bridgeSession
	for _, b := range i.routes {
		if b.lastActivity.Load() < cutoff {
			candidates = append(candidates, b)
		}
	}
	i.routesMu.RUnlock()

	for _, b := range candidates {
		resetFrame := &relaypb.RelayMessageFrame{
			Version:  1,
			StreamId: b.streamID,
			Body: &relaypb.RelayMessageFrame_Reset_{
				Reset_: &relaypb.RelayReset{Reason: "idle-timeout"},
			},
		}
		data, err := proto.Marshal(resetFrame)
		if err != nil {
			i.logger.Warn("reaper: marshal reset", "stream_id", b.streamID, "err", err)
		} else {
			if werr := i.write(ctx, websocket.MessageBinary, data); werr != nil {
				i.logger.Warn("reaper: write reset to inbound", "stream_id", b.streamID, "err", werr)
			}
		}
		i.removeRoute(b.streamID, b)
		b.close(nil)
		i.logger.Info("reaper: closed idle bridge", "stream_id", b.streamID, "timeout", idleTimeout)
	}
}
