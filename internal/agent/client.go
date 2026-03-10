package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentserver/agentserver/internal/tunnel"
	"nhooyr.io/websocket"
)

// Client is the cli-agent tunnel client that connects to the server
// and forwards HTTP requests to a local opencode instance.
type Client struct {
	ServerURL     string
	SandboxID     string
	TunnelToken   string
	OpencodeURL   string
	OpencodeToken string
	httpClient    *http.Client
}

// NewClient creates a new agent tunnel client.
func NewClient(serverURL, sandboxID, tunnelToken, opencodeURL, opencodeToken string) *Client {
	return &Client{
		ServerURL:     serverURL,
		SandboxID:     sandboxID,
		TunnelToken:   tunnelToken,
		OpencodeURL:   opencodeURL,
		OpencodeToken: opencodeToken,
		httpClient: &http.Client{
			Timeout: 0, // No timeout for SSE streams.
		},
	}
}

// Register registers a new local agent with the server using a one-time code.
func Register(serverURL, code, name string) (*RegistryEntry, error) {
	body := fmt.Sprintf(`{"code":%q,"name":%q}`, code, name)
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

		// Reset backoff if we were connected for a reasonable duration,
		// indicating the disconnect was not an immediate failure.
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

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	log.Printf("tunnel connected (sandbox: %s)", c.SandboxID)

	// Send initial agent info.
	if err := c.sendAgentInfo(ctx, conn); err != nil {
		return fmt.Errorf("send initial agent info: %w", err)
	}

	// Periodically re-send agent info as keepalive. This serves as
	// application-level upstream traffic (visible to proxies), while
	// also keeping the server's agent metadata fresh (disk, memory, etc.).
	// The server sends application-level ping frames for downstream traffic.
	infoCtx, infoCancel := context.WithCancel(ctx)
	defer infoCancel()
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-infoCtx.Done():
				return
			case <-ticker.C:
				if err := c.sendAgentInfo(infoCtx, conn); err != nil {
					log.Printf("failed to send agent info: %v", err)
					infoCancel()
					return
				}
			}
		}
	}()

	// Read and process binary frames.
	for {
		_, data, err := conn.Read(infoCtx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		headerJSON, payload, err := tunnel.DecodeFrameHeader(data)
		if err != nil {
			log.Printf("failed to decode frame: %v", err)
			continue
		}

		var hdr tunnel.IncomingHeader
		if err := json.Unmarshal(headerJSON, &hdr); err != nil {
			log.Printf("failed to unmarshal header: %v", err)
			continue
		}

		if hdr.Type == tunnel.FrameTypePong {
			continue
		}

		if hdr.Type == tunnel.FrameTypePing {
			pongHeader := struct {
				Type string `json:"type"`
			}{Type: tunnel.FrameTypePong}
			if msg, err := tunnel.EncodeFrame(pongHeader, nil); err == nil {
				conn.Write(infoCtx, websocket.MessageBinary, msg)
			}
			continue
		}

		if hdr.Type == tunnel.FrameTypeRequest {
			var reqHeader tunnel.RequestHeader
			if err := json.Unmarshal(headerJSON, &reqHeader); err != nil {
				log.Printf("failed to unmarshal request header: %v", err)
				continue
			}
			go c.handleRequest(ctx, conn, &reqHeader, payload)
		}
	}
}

func (c *Client) sendAgentInfo(ctx context.Context, conn *websocket.Conn) error {
	agentInfo := collectAgentInfo(c.OpencodeURL)
	infoMsg := struct {
		Type string         `json:"type"`
		Data *AgentInfoData `json:"data"`
	}{
		Type: tunnel.FrameTypeAgentInfo,
		Data: agentInfo,
	}
	infoBytes, err := json.Marshal(infoMsg)
	if err != nil {
		return fmt.Errorf("marshal agent info: %w", err)
	}
	return conn.Write(ctx, websocket.MessageText, infoBytes)
}

// maxChunkSize is the maximum raw bytes per stream chunk.
// Keeps each WebSocket binary message well under the default 32KB read limit
// (header JSON ~200 bytes + 16KB payload = ~16.5KB).
const maxChunkSize = 16 * 1024

func (c *Client) handleRequest(ctx context.Context, conn *websocket.Conn, reqHeader *tunnel.RequestHeader, reqBody []byte) {
	// Build the local HTTP request using safe URL construction.
	base, err := url.Parse(c.OpencodeURL)
	if err != nil {
		c.sendErrorResponse(ctx, conn, reqHeader.ID, http.StatusBadGateway, "invalid opencode URL")
		return
	}

	// Sanitize the path: must start with "/" and not contain scheme/host components.
	path := reqHeader.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	parsed, err := url.Parse(path)
	if err != nil || parsed.Host != "" || parsed.Scheme != "" {
		c.sendErrorResponse(ctx, conn, reqHeader.ID, http.StatusBadRequest, "invalid request path")
		return
	}
	target := base.ResolveReference(parsed)

	var bodyReader io.Reader
	if len(reqBody) > 0 {
		bodyReader = bytes.NewReader(reqBody)
	}

	httpReq, err := http.NewRequestWithContext(ctx, reqHeader.Method, target.String(), bodyReader)
	if err != nil {
		c.sendErrorResponse(ctx, conn, reqHeader.ID, http.StatusBadGateway, "failed to create request")
		return
	}

	// Copy headers from the request frame.
	for k, v := range reqHeader.Headers {
		httpReq.Header.Set(k, v)
	}

	// Add Basic Auth for opencode if password is provided.
	if c.OpencodeToken != "" {
		httpReq.SetBasicAuth("opencode", c.OpencodeToken)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		c.sendErrorResponse(ctx, conn, reqHeader.ID, http.StatusBadGateway, "failed to reach local opencode: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Collect response headers.
	headers := make(map[string]string)
	for k, vals := range resp.Header {
		if len(vals) > 0 {
			headers[k] = vals[0]
		}
	}

	// Stream response body as chunked binary frames.
	buf := make([]byte, maxChunkSize)
	firstFrame := true
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			sh := tunnel.StreamHeader{
				Type: tunnel.FrameTypeStream,
				ID:   reqHeader.ID,
				Done: false,
			}
			if firstFrame {
				sh.Status = resp.StatusCode
				sh.Headers = headers
				firstFrame = false
			}
			msg, _ := tunnel.EncodeFrame(sh, buf[:n])
			if writeErr := conn.Write(ctx, websocket.MessageBinary, msg); writeErr != nil {
				return
			}
		}
		if readErr != nil {
			// Send done frame (no payload).
			sh := tunnel.StreamHeader{
				Type: tunnel.FrameTypeStream,
				ID:   reqHeader.ID,
				Done: true,
			}
			if firstFrame {
				sh.Status = resp.StatusCode
				sh.Headers = headers
			}
			msg, _ := tunnel.EncodeFrame(sh, nil)
			conn.Write(ctx, websocket.MessageBinary, msg)
			return
		}
	}
}

func (c *Client) sendErrorResponse(ctx context.Context, conn *websocket.Conn, requestID string, status int, message string) {
	// Send error as a single stream frame with done=true.
	sh := tunnel.StreamHeader{
		Type:    tunnel.FrameTypeStream,
		ID:      requestID,
		Status:  status,
		Headers: map[string]string{"Content-Type": "text/plain"},
		Done:    false,
	}
	msg, _ := tunnel.EncodeFrame(sh, []byte(message))
	conn.Write(ctx, websocket.MessageBinary, msg)

	// Send done frame.
	doneSh := tunnel.StreamHeader{
		Type: tunnel.FrameTypeStream,
		ID:   requestID,
		Done: true,
	}
	doneMsg, _ := tunnel.EncodeFrame(doneSh, nil)
	conn.Write(ctx, websocket.MessageBinary, doneMsg)
}
