// Package wsbridge bidirectionally forwards WebSocket frames between two
// connections. Frame-level forwarding: doesn't parse JSON-RPC. JSON-RPC
// envelope boundaries are preserved because each Read/Write pair transfers
// exactly one frame — never byte concatenation.
package wsbridge

import (
	"context"
	"errors"
	"io"

	"nhooyr.io/websocket"
)

// RunProxy starts two pumps in parallel and returns when either side
// closes or errors. Both ws conns are left open for the caller to close.
// onFrame is called on every successfully read frame (from either direction);
// pass nil if no callback is needed.
func RunProxy(ctx context.Context, a, b *websocket.Conn, onFrame func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- pump(ctx, a, b, onFrame) }()
	go func() { errCh <- pump(ctx, b, a, onFrame) }()
	err := <-errCh
	cancel()
	<-errCh
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// PumpFrames reads one frame at a time from src and writes the exact same
// (MessageType, payload) to dst. This preserves JSON-RPC envelope boundaries.
//
// Returns nil when src closes cleanly; otherwise the underlying error.
func PumpFrames(ctx context.Context, src, dst *websocket.Conn) error {
	return pump(ctx, src, dst, nil)
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
