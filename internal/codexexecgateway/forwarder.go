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
// Returns nil when src closes cleanly; otherwise the underlying error.
// Either side closing causes pumpFrames to return; the bridge handler
// closes the peer when this returns so both halves shut down together.
func pumpFrames(ctx context.Context, src, dst *websocket.Conn) error {
	return wsbridge.PumpFrames(ctx, src, dst)
}
