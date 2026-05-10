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
func RunProxy(ctx context.Context, userWS, childWS *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- pump(ctx, userWS, childWS) }()
	go func() { errCh <- pump(ctx, childWS, userWS) }()
	err := <-errCh
	cancel()
	<-errCh
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func pump(ctx context.Context, src, dst *websocket.Conn) error {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return err
		}
	}
}
