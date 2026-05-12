package codexexecgateway

import (
	"context"

	"github.com/agentserver/agentserver/internal/wsbridge"
	"nhooyr.io/websocket"
)

// pumpFrames reads one frame at a time from src and writes the exact same
// (MessageType, payload) to dst. This preserves JSON-RPC envelope boundaries
// — the spec requires frame-level forwarding, never byte concatenation.
//
// pumpFrames returns when src.Read or dst.Write fails. Caller is responsible for closing connections.
func pumpFrames(ctx context.Context, src, dst *websocket.Conn) error {
	return wsbridge.PumpFrames(ctx, src, dst)
}
