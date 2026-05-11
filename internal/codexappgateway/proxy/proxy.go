// Package proxy bidirectionally forwards ws frames between a user-side
// ws and a child-side ws. Frame-level: doesn't parse JSON-RPC.
package proxy

import (
	"context"
	"errors"

	"nhooyr.io/websocket"
)

// RunProxy starts two pumps in parallel and returns when either side
// closes or errors. Both ws conns are left open for the caller to close.
// onFrame is called on every successfully read frame (from either direction);
// pass nil if no callback is needed.
func RunProxy(ctx context.Context, userWS, childWS *websocket.Conn, onFrame func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- pump(ctx, userWS, childWS, onFrame) }()
	go func() { errCh <- pump(ctx, childWS, userWS, onFrame) }()
	err := <-errCh
	cancel()
	<-errCh
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func pump(ctx context.Context, src, dst *websocket.Conn, onFrame func()) error {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		if onFrame != nil {
			onFrame()
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return err
		}
	}
}
