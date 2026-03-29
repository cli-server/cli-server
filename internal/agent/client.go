package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentserver/agentserver/internal/tunnel"
	"github.com/hashicorp/yamux"
	"nhooyr.io/websocket"
)

// Client is the cli-agent tunnel client that connects to the server
// and forwards HTTP/terminal requests to a local service.
type Client struct {
	ServerURL     string
	SandboxID     string
	TunnelToken   string
	OpencodeURL   string // local HTTP service URL (e.g. http://localhost:4096)
	OpencodeToken string // optional Basic Auth password for opencode
	Workdir       string
	BackendType   string // "opencode" or "claudecode"
	httpClient    *http.Client

	// OnTerminalStream is called when the server opens a terminal stream.
	// Implementations should bridge the stream to a PTY.
	// If nil, terminal streams are rejected.
	OnTerminalStream func(stream net.Conn)
}

// NewClient creates a new agent tunnel client.
func NewClient(serverURL, sandboxID, tunnelToken, opencodeURL, opencodeToken, workdir string) *Client {
	return &Client{
		ServerURL:     serverURL,
		SandboxID:     sandboxID,
		TunnelToken:   tunnelToken,
		OpencodeURL:   opencodeURL,
		OpencodeToken: opencodeToken,
		Workdir:       workdir,
		BackendType:   "opencode",
		httpClient: &http.Client{
			Timeout: 0, // No timeout for SSE streams.
		},
	}
}

// Register registers a new local agent with the server using a one-time code.
func Register(serverURL, code, name, agentType string) (*RegistryEntry, error) {
	if agentType == "" {
		agentType = "opencode"
	}
	body := fmt.Sprintf(`{"code":%q,"name":%q,"type":%q}`, code, name, agentType)
	resp, err := http.Post(
		serverURL+"/api/agent/register",
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("register request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registration failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		SandboxID   string `json:"sandbox_id"`
		TunnelToken string `json:"tunnel_token"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &RegistryEntry{
		Server:      serverURL,
		SandboxID:   result.SandboxID,
		TunnelToken: result.TunnelToken,
		WorkspaceID: result.WorkspaceID,
		Name:        name,
		Type:        agentType,
	}, nil
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
		go c.handleServerStream(ctx, stream)
	}
}

// handleServerStream dispatches a server-opened stream by its type.
func (c *Client) handleServerStream(ctx context.Context, stream net.Conn) {
	defer stream.Close()

	streamType, metadata, err := tunnel.ReadStreamHeader(stream)
	if err != nil {
		log.Printf("read stream header: %v", err)
		return
	}

	switch streamType {
	case tunnel.StreamTypeHTTP:
		c.handleHTTPStream(ctx, stream, metadata)
	case tunnel.StreamTypeTerminal:
		c.handleTerminalStream(stream)
	default:
		log.Printf("unknown server stream type: %d", streamType)
	}
}

// handleHTTPStream proxies an HTTP request to the local service.
func (c *Client) handleHTTPStream(ctx context.Context, stream net.Conn, metadata []byte) {
	var meta tunnel.HTTPStreamMeta
	if err := tunnel.UnmarshalStreamMeta(metadata, &meta); err != nil {
		log.Printf("unmarshal HTTP metadata: %v", err)
		c.writeHTTPError(stream, http.StatusBadRequest, "invalid metadata")
		return
	}

	// Read exactly BodyLen bytes of request body.
	var reqBody []byte
	if meta.BodyLen > 0 {
		reqBody = make([]byte, meta.BodyLen)
		if _, err := io.ReadFull(stream, reqBody); err != nil {
			log.Printf("read request body: %v", err)
			c.writeHTTPError(stream, http.StatusBadGateway, "failed to read request body")
			return
		}
	}

	// Build the local HTTP request.
	base, err := url.Parse(c.OpencodeURL)
	if err != nil {
		c.writeHTTPError(stream, http.StatusBadGateway, "invalid local URL")
		return
	}

	path := meta.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	parsed, err := url.Parse(path)
	if err != nil || parsed.Host != "" || parsed.Scheme != "" {
		c.writeHTTPError(stream, http.StatusBadRequest, "invalid request path")
		return
	}
	target := base.ResolveReference(parsed)

	var bodyReader io.Reader
	if len(reqBody) > 0 {
		bodyReader = bytes.NewReader(reqBody)
	}

	httpReq, err := http.NewRequestWithContext(ctx, meta.Method, target.String(), bodyReader)
	if err != nil {
		c.writeHTTPError(stream, http.StatusBadGateway, "failed to create request")
		return
	}

	for k, v := range meta.Headers {
		httpReq.Header.Set(k, v)
	}
	if c.OpencodeToken != "" {
		httpReq.SetBasicAuth("opencode", c.OpencodeToken)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		c.writeHTTPError(stream, http.StatusBadGateway, "local service error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Write response header.
	headers := make(map[string]string)
	for k, vals := range resp.Header {
		if len(vals) > 0 {
			headers[k] = vals[0]
		}
	}
	respMeta := tunnel.HTTPResponseMeta{
		Status:  resp.StatusCode,
		Headers: headers,
	}
	respMetaJSON, _ := tunnel.MarshalStreamMeta(respMeta)
	if err := tunnel.WriteStreamHeader(stream, tunnel.StreamTypeHTTP, respMetaJSON); err != nil {
		return
	}

	// Stream response body.
	io.Copy(stream, resp.Body)
}

// writeHTTPError writes an error response on an HTTP stream.
func (c *Client) writeHTTPError(stream net.Conn, status int, message string) {
	respMeta := tunnel.HTTPResponseMeta{
		Status:  status,
		Headers: map[string]string{"Content-Type": "text/plain"},
	}
	respMetaJSON, _ := tunnel.MarshalStreamMeta(respMeta)
	tunnel.WriteStreamHeader(stream, tunnel.StreamTypeHTTP, respMetaJSON)
	stream.Write([]byte(message))
}

// handleTerminalStream delegates to the OnTerminalStream callback.
func (c *Client) handleTerminalStream(stream net.Conn) {
	if c.OnTerminalStream == nil {
		log.Printf("terminal stream received but no handler configured")
		return
	}
	// Don't defer stream.Close() here — the callback owns the stream lifecycle.
	c.OnTerminalStream(stream)
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

	info := collectAgentInfo(c.OpencodeURL, c.Workdir)
	data, err := json.Marshal(info)
	if err != nil {
		return
	}

	if err := tunnel.WriteStreamHeader(stream, tunnel.StreamTypeControl, nil); err != nil {
		return
	}
	stream.Write(data)
}
