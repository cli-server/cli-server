package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/agentserver/agentserver/internal/tunnel"
	"github.com/hashicorp/yamux"
	"nhooyr.io/websocket"
)

// Client is the agent tunnel client that connects to the server
// and maintains a persistent connection for heartbeat and control.
type Client struct {
	ServerURL   string
	SandboxID   string
	TunnelToken string
	Workdir     string
	BackendType        string // "claudecode"
	cachedCapabilities *AgentCapabilities
	capabilitiesMu     sync.Mutex
	lastProbeTime      time.Time
}

// NewClient creates a new agent tunnel client.
func NewClient(serverURL, sandboxID, tunnelToken, workdir string) *Client {
	return &Client{
		ServerURL:   serverURL,
		SandboxID:   sandboxID,
		TunnelToken: tunnelToken,
		Workdir:     workdir,
		BackendType: "claudecode",
	}
}

// Run connects to the server and enters the tunnel event loop.
// It automatically reconnects with exponential backoff on disconnection.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	maxBackoff := 60 * time.Second

	for {
		connectedAt := time.Now()
		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.Printf("tunnel disconnected: %v", err)
		}

		if time.Since(connectedAt) > 30*time.Second {
			backoff = time.Second
		}

		log.Printf("reconnecting in %s...", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *Client) connectAndServe(ctx context.Context) error {
	// Build WebSocket URL.
	wsURL := c.ServerURL + "/api/tunnel/" + c.SandboxID + "?token=" + c.TunnelToken
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)

	log.Printf("connecting to %s", wsURL)

	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	log.Printf("tunnel connected (sandbox: %s)", c.SandboxID)

	// Wrap WebSocket as net.Conn and create yamux client session.
	conn := tunnel.NewWSConn(ctx, ws)
	session, err := tunnel.ClientMux(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("create yamux session: %w", err)
	}
	defer session.Close()

	// Periodically send agent info via control streams (agent-initiated).
	infoCtx, infoCancel := context.WithCancel(ctx)
	defer infoCancel()
	go c.sendAgentInfoLoop(infoCtx, session)

	// Accept and handle streams opened by the server.
	for {
		stream, err := session.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accept stream: %w", err)
		}
		go c.handleServerStream(stream)
	}
}

// handleServerStream dispatches a server-opened stream by its type.
func (c *Client) handleServerStream(stream net.Conn) {
	defer stream.Close()

	streamType, _, err := tunnel.ReadStreamHeader(stream)
	if err != nil {
		log.Printf("read stream header: %v", err)
		return
	}

	switch streamType {
	case tunnel.StreamTypeHTTP:
		log.Printf("received HTTP proxy stream but agent is headless; closing")
	case tunnel.StreamTypeTerminal:
		log.Printf("received terminal stream but agent is headless; closing")
	default:
		log.Printf("unknown server stream type: %d", streamType)
	}
}

// sendAgentInfoLoop periodically sends agent info via control streams.
func (c *Client) sendAgentInfoLoop(ctx context.Context, session *yamux.Session) {
	// Send initial info immediately.
	c.sendAgentInfo(session)

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sendAgentInfo(session)
		}
	}
}

func (c *Client) sendAgentInfo(session *yamux.Session) {
	stream, err := session.Open()
	if err != nil {
		return
	}
	defer stream.Close()

	info := collectAgentInfo("", c.Workdir)

	// Attach capabilities (probe outside lock to avoid blocking heartbeats).
	c.capabilitiesMu.Lock()
	needsProbe := c.cachedCapabilities == nil || time.Since(c.lastProbeTime) > 1*time.Hour
	c.capabilitiesMu.Unlock()

	if needsProbe {
		caps := ProbeCapabilities(context.Background())
		c.capabilitiesMu.Lock()
		c.cachedCapabilities = caps
		c.lastProbeTime = time.Now()
		c.capabilitiesMu.Unlock()
	}

	c.capabilitiesMu.Lock()
	info.Capabilities = c.cachedCapabilities
	c.capabilitiesMu.Unlock()

	data, err := json.Marshal(info)
	if err != nil {
		return
	}

	if err := tunnel.WriteStreamHeader(stream, tunnel.StreamTypeControl, nil); err != nil {
		return
	}
	stream.Write(data)
}
