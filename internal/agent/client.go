package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/imryao/cli-server/internal/tunnel"
	"nhooyr.io/websocket"
)

// Client is the cli-agent tunnel client that connects to the server
// and forwards HTTP requests to a local opencode instance.
type Client struct {
	ServerURL       string
	SandboxID       string
	TunnelToken     string
	OpencodeURL     string
	OpencodePassword string
	httpClient      *http.Client
}

// NewClient creates a new agent tunnel client.
func NewClient(serverURL, sandboxID, tunnelToken, opencodeURL, opencodePassword string) *Client {
	return &Client{
		ServerURL:       serverURL,
		SandboxID:       sandboxID,
		TunnelToken:     tunnelToken,
		OpencodeURL:     opencodeURL,
		OpencodePassword: opencodePassword,
		httpClient: &http.Client{
			Timeout: 0, // No timeout for SSE streams.
		},
	}
}

// Register registers a new local agent with the server using a one-time code.
func Register(serverURL, code, name string) (*Config, error) {
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
		SandboxID   string `json:"sandboxId"`
		TunnelToken string `json:"tunnelToken"`
		WorkspaceID string `json:"workspaceId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &Config{
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
		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.Printf("tunnel disconnected: %v", err)
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

	// Increase read limit for large responses.
	conn.SetReadLimit(64 * 1024 * 1024) // 64MB

	log.Printf("tunnel connected (sandbox: %s)", c.SandboxID)

	// Read and process frames.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var frame tunnel.IncomingFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			log.Printf("failed to unmarshal frame: %v", err)
			continue
		}

		if frame.Type == tunnel.FrameTypeRequest {
			var reqFrame tunnel.RequestFrame
			if err := json.Unmarshal(data, &reqFrame); err != nil {
				log.Printf("failed to unmarshal request frame: %v", err)
				continue
			}
			go c.handleRequest(ctx, conn, &reqFrame)
		}
	}
}

func (c *Client) handleRequest(ctx context.Context, conn *websocket.Conn, reqFrame *tunnel.RequestFrame) {
	// Build the local HTTP request.
	targetURL := c.OpencodeURL + reqFrame.Path

	var bodyReader io.Reader
	if reqFrame.Body != "" {
		bodyBytes, err := base64.StdEncoding.DecodeString(reqFrame.Body)
		if err != nil {
			c.sendErrorResponse(ctx, conn, reqFrame.ID, http.StatusBadGateway, "failed to decode request body")
			return
		}
		bodyReader = strings.NewReader(string(bodyBytes))
	}

	httpReq, err := http.NewRequestWithContext(ctx, reqFrame.Method, targetURL, bodyReader)
	if err != nil {
		c.sendErrorResponse(ctx, conn, reqFrame.ID, http.StatusBadGateway, "failed to create request")
		return
	}

	// Copy headers from the request frame.
	for k, v := range reqFrame.Headers {
		httpReq.Header.Set(k, v)
	}

	// Add Basic Auth for opencode if password is provided.
	if c.OpencodePassword != "" {
		httpReq.SetBasicAuth("opencode", c.OpencodePassword)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		c.sendErrorResponse(ctx, conn, reqFrame.ID, http.StatusBadGateway, "failed to reach local opencode: "+err.Error())
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

	// Check if this is an SSE stream.
	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "text/event-stream") {
		c.handleStreamResponse(ctx, conn, reqFrame.ID, resp, headers)
		return
	}

	// Normal response: read body entirely and send as ResponseFrame.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.sendErrorResponse(ctx, conn, reqFrame.ID, http.StatusBadGateway, "failed to read response")
		return
	}

	respFrame := tunnel.ResponseFrame{
		Type:    tunnel.FrameTypeResponse,
		ID:      reqFrame.ID,
		Status:  resp.StatusCode,
		Headers: headers,
		Body:    base64.StdEncoding.EncodeToString(body),
	}

	data, _ := json.Marshal(respFrame)
	conn.Write(ctx, websocket.MessageText, data)
}

func (c *Client) handleStreamResponse(ctx context.Context, conn *websocket.Conn, requestID string, resp *http.Response, headers map[string]string) {
	// Send first frame with headers and status.
	buf := make([]byte, 4096)
	firstFrame := true
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			frame := tunnel.StreamFrame{
				Type:  tunnel.FrameTypeStream,
				ID:    requestID,
				Chunk: base64.StdEncoding.EncodeToString(buf[:n]),
				Done:  false,
			}
			if firstFrame {
				frame.Status = resp.StatusCode
				frame.Headers = headers
				firstFrame = false
			}
			data, _ := json.Marshal(frame)
			if writeErr := conn.Write(ctx, websocket.MessageText, data); writeErr != nil {
				return
			}
		}
		if err != nil {
			// Send done frame.
			doneFrame := tunnel.StreamFrame{
				Type:  tunnel.FrameTypeStream,
				ID:    requestID,
				Chunk: "",
				Done:  true,
			}
			if firstFrame {
				doneFrame.Status = resp.StatusCode
				doneFrame.Headers = headers
			}
			data, _ := json.Marshal(doneFrame)
			conn.Write(ctx, websocket.MessageText, data)
			return
		}
	}
}

func (c *Client) sendErrorResponse(ctx context.Context, conn *websocket.Conn, requestID string, status int, message string) {
	resp := tunnel.ResponseFrame{
		Type:    tunnel.FrameTypeResponse,
		ID:      requestID,
		Status:  status,
		Headers: map[string]string{"Content-Type": "text/plain"},
		Body:    base64.StdEncoding.EncodeToString([]byte(message)),
	}
	data, _ := json.Marshal(resp)
	conn.Write(ctx, websocket.MessageText, data)
}
