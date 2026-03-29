package tunnel

import (
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

// MuxConfig returns the default yamux configuration for the tunnel.
func MuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 10 * time.Second
	// Accept a reasonable backlog of streams so HTTP requests don't stall
	// while the accept loop is busy.
	cfg.AcceptBacklog = 256
	// Silence yamux's internal logger (we handle errors at the caller).
	cfg.LogOutput = nil
	return cfg
}

// ServerMux creates a yamux server session over conn.
// The agentserver side acts as the yamux server: it accepts streams
// opened by the agent (control messages) and opens streams towards
// the agent (HTTP proxy, terminal).
func ServerMux(conn net.Conn) (*yamux.Session, error) {
	return yamux.Server(conn, MuxConfig())
}

// ClientMux creates a yamux client session over conn.
// The local agent acts as the yamux client: it opens streams towards
// the server (control messages) and accepts streams from the server
// (HTTP proxy, terminal).
func ClientMux(conn net.Conn) (*yamux.Session, error) {
	return yamux.Client(conn, MuxConfig())
}
