// Package wsbridge bidirectionally forwards WebSocket frames between two
// connections. Frame-level forwarding: doesn't parse JSON-RPC. JSON-RPC
// envelope boundaries are preserved because each Read/Write pair transfers
// exactly one frame — never byte concatenation.
package wsbridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"nhooyr.io/websocket"
)

// keepAliveInterval is how often we send a ws ping on the public-facing
// connection to defeat middlebox idle timeouts. Istio's envoy default
// upstream idle_timeout is ~240s; LLM responses regularly cross that
// when the model takes minutes to answer, leading to silent TCP RST.
const keepAliveInterval = 30 * time.Second

// RunProxy starts two pumps in parallel and returns when either side
// closes or errors. Both ws conns are left open for the caller to close.
// onFrame is called on every successfully read frame (from either direction);
// pass nil if no callback is needed.
//
// A background ping is sent on `a` every keepAliveInterval to keep
// idle-killing middleboxes from severing the connection during long
// quiet periods (e.g. the LLM taking minutes to respond before any
// frame flows).
func RunProxy(ctx context.Context, a, b *websocket.Conn, onFrame func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- pump(ctx, a, b, onFrame) }()
	go func() { errCh <- pump(ctx, b, a, onFrame) }()
	go keepAlive(ctx, a)
	err := <-errCh
	cancel()
	<-errCh
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// keepAlive sends a ws ping on conn every keepAliveInterval until ctx
// is cancelled. Ping failures are silent — if the connection is dead,
// the pump goroutine will surface the real error.
func keepAlive(ctx context.Context, conn *websocket.Conn) {
	KeepAlive(ctx, conn, keepAliveInterval)
}

// KeepAlive sends a ws ping on conn every interval until ctx is
// cancelled. Exported so the codex-exec-gateway /bridge and inbound
// handlers (which run their own custom pumps and don't go through
// RunProxy) can install the same anti-idle behavior.
func KeepAlive(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	if interval <= 0 {
		interval = keepAliveInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = conn.Ping(pingCtx)
			cancel()
		}
	}
}

// PumpFrames reads one frame at a time from src and writes the exact same
// (MessageType, payload) to dst. This preserves JSON-RPC envelope boundaries.
//
// Returns nil when src closes cleanly; otherwise the underlying error.
func PumpFrames(ctx context.Context, src, dst *websocket.Conn) error {
	return pump(ctx, src, dst, nil)
}

// DropFrame is returned by an Interceptor callback to indicate the frame
// should NOT be forwarded downstream. Distinct from returning nil
// (forward unchanged) and from returning a rewritten slice (forward
// replacement).
var DropFrame = []byte("__wsbridge_drop_frame__")

// Interceptor lets a caller observe and optionally rewrite frames as
// they cross the bridge. Both callbacks may be nil. Returning a non-nil
// slice replaces the frame written downstream; returning nil forwards
// the original frame untouched. Return DropFrame to swallow the frame
// entirely. Callbacks MUST NOT block.
type Interceptor struct {
	OnClientFrame func(frame []byte) []byte // a → b direction
	OnServerFrame func(frame []byte) []byte // b → a direction
}

// RunProxyWithInterceptor is like RunProxy but lets the caller observe
// and rewrite frames per direction. `onFrame` is invoked on every
// successfully forwarded frame (pass nil to skip).
func RunProxyWithInterceptor(ctx context.Context, a, b *websocket.Conn, intc Interceptor, onFrame func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- pumpWithIntercept(ctx, a, b, intc.OnClientFrame, onFrame) }()
	go func() { errCh <- pumpWithIntercept(ctx, b, a, intc.OnServerFrame, onFrame) }()
	go keepAlive(ctx, a)
	err := <-errCh
	cancel()
	<-errCh
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func pumpWithIntercept(
	ctx context.Context,
	src, dst *websocket.Conn,
	onFrameBytes func([]byte) []byte,
	onTick func(),
) error {
	for {
		mt, data, err := src.Read(ctx)
		if err != nil {
			closeErr := websocket.CloseStatus(err)
			if closeErr == websocket.StatusNormalClosure || closeErr == websocket.StatusGoingAway {
				return nil
			}
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		out := data
		if onFrameBytes != nil {
			if rewritten := onFrameBytes(data); rewritten != nil {
				if bytes.Equal(rewritten, DropFrame) {
					if onTick != nil {
						onTick()
					}
					continue
				}
				out = rewritten
			}
		}
		if onTick != nil {
			onTick()
		}
		if err := dst.Write(ctx, mt, out); err != nil {
			return err
		}
	}
}

func pump(ctx context.Context, src, dst *websocket.Conn, onFrame func()) error {
	for {
		mt, data, err := src.Read(ctx)
		if err != nil {
			// Normal-closure on src is not an error to propagate up.
			closeErr := websocket.CloseStatus(err)
			if closeErr == websocket.StatusNormalClosure || closeErr == websocket.StatusGoingAway {
				return nil
			}
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if onFrame != nil {
			onFrame()
		}
		if err := dst.Write(ctx, mt, data); err != nil {
			return err
		}
	}
}
