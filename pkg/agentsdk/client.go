package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agentserver/agentserver/internal/tunnel"
	"github.com/hashicorp/yamux"
	"nhooyr.io/websocket"
)

// Client is the agentsdk client that registers with agentserver,
// establishes a tunnel, and handles incoming requests.
type Client struct {
	config Config
	reg    *Registration
}

// NewClient creates a new agent SDK client with the given configuration.
// If Config.Type is empty, it defaults to "custom".
// If Config.Name is empty, it defaults to the machine hostname.
func NewClient(cfg Config) *Client {
	if cfg.Type == "" {
		cfg.Type = "custom"
	}
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
		if cfg.Name == "" {
			cfg.Name = "agent"
		}
	}
	return &Client{config: cfg}
}

// Register registers the agent with agentserver using the provided access token
// (obtained via the device flow). It returns a Registration containing the
// sandbox ID and tunnel/proxy tokens.
func (c *Client) Register(ctx context.Context, accessToken string) (*Registration, error) {
	bodyData, err := json.Marshal(map[string]string{
		"name": c.config.Name,
		"type": c.config.Type,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal register body: %w", err)
	}

	reqURL := strings.TrimRight(c.config.ServerURL, "/") + "/api/agent/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(bodyData))
	if err != nil {
		return nil, fmt.Errorf("create register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registration failed (%d): %s", resp.StatusCode, body)
	}

	var reg Registration
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		return nil, fmt.Errorf("decode register response: %w", err)
	}
	c.reg = &reg
	return &reg, nil
}

// SetRegistration sets a pre-existing registration (e.g. loaded from saved
// credentials) instead of calling Register.
func (c *Client) SetRegistration(reg *Registration) {
	c.reg = reg
}

// Connect establishes the WebSocket tunnel to agentserver and enters the
// main event loop. It automatically reconnects with exponential backoff
// on disconnection. Connect blocks until the context is cancelled.
func (c *Client) Connect(ctx context.Context, handlers Handlers, opts ...ConnectOption) error {
	if c.reg == nil {
		return fmt.Errorf("no registration; call Register or SetRegistration first")
	}

	options := connectOptions{
		heartbeatInterval: 20 * time.Second,
		taskPollInterval:  5 * time.Second,
	}
	for _, opt := range opts {
		opt(&options)
	}

	backoff := time.Second
	maxBackoff := 60 * time.Second

	for {
		connectedAt := time.Now()
		err := c.connectAndServe(ctx, handlers, options)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.Printf("agentsdk: tunnel disconnected: %v", err)
		}

		if handlers.OnDisconnect != nil {
			handlers.OnDisconnect(err)
		}

		// Reset backoff if we were connected for a while.
		if time.Since(connectedAt) > 30*time.Second {
			backoff = time.Second
		}

		log.Printf("agentsdk: reconnecting in %s...", backoff)
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

func (c *Client) connectAndServe(ctx context.Context, handlers Handlers, opts connectOptions) error {
	// Build WebSocket URL.
	wsURL := strings.TrimRight(c.config.ServerURL, "/") + "/api/tunnel/" + c.reg.SandboxID + "?token=" + c.reg.TunnelToken
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)

	log.Printf("agentsdk: connecting to %s", c.config.ServerURL)

	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	conn := tunnel.NewWSConn(ctx, ws)
	session, err := tunnel.ClientMux(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("create yamux session: %w", err)
	}
	defer session.Close()

	log.Printf("agentsdk: tunnel connected (sandbox: %s)", c.reg.SandboxID)
	if handlers.OnConnect != nil {
		handlers.OnConnect()
	}

	// Start heartbeat goroutine.
	heartCtx, heartCancel := context.WithCancel(ctx)
	defer heartCancel()
	go c.heartbeatLoop(heartCtx, session, opts.heartbeatInterval)

	// Start task poll goroutine if handler provided.
	if handlers.Task != nil {
		taskCtx, taskCancel := context.WithCancel(ctx)
		defer taskCancel()
		go c.taskPollLoop(taskCtx, handlers.Task, opts.taskPollInterval)
	}

	// Accept and handle streams opened by the server.
	for {
		stream, err := session.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accept stream: %w", err)
		}
		go c.handleStream(stream, handlers)
	}
}

// handleStream dispatches an incoming server stream by its type.
func (c *Client) handleStream(stream net.Conn, handlers Handlers) {
	defer stream.Close()

	streamType, metaBytes, err := tunnel.ReadStreamHeader(stream)
	if err != nil {
		return
	}

	switch streamType {
	case tunnel.StreamTypeHTTP:
		if handlers.HTTP != nil {
			handleHTTPStreamWithMeta(stream, metaBytes, handlers.HTTP)
		}
	case tunnel.StreamTypeTerminal:
		// Custom agents don't support terminal; close the stream.
	}
}

// heartbeatLoop periodically sends agent info via control streams.
func (c *Client) heartbeatLoop(ctx context.Context, session *yamux.Session, interval time.Duration) {
	// Send initial heartbeat immediately.
	c.sendHeartbeat(session)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sendHeartbeat(session)
		}
	}
}

// sendHeartbeat sends a single control stream with agent info.
func (c *Client) sendHeartbeat(session *yamux.Session) {
	stream, err := session.Open()
	if err != nil {
		return
	}
	defer stream.Close()

	hostname, _ := os.Hostname()
	info := map[string]interface{}{
		"hostname":      hostname,
		"os":            "linux",
		"agent_version": "agentsdk/1.0",
	}
	data, err := json.Marshal(info)
	if err != nil {
		return
	}

	if err := tunnel.WriteStreamHeader(stream, tunnel.StreamTypeControl, nil); err != nil {
		return
	}
	stream.Write(data)
}
