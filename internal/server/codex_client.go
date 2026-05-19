package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// resolveCodexGatewayRESTURL returns the base URL of codex-app-gateway's
// REST surface (the host that serves POST /api/turns). Resolution order:
//
//  1. CODEX_APP_GATEWAY_REST_URL — explicit, e.g. "http://cxg.svc:8086"
//  2. CODEX_APP_GATEWAY_URL with the well-known "/notebook/ws" suffix
//     stripped and the scheme rewritten ws→http / wss→https. This is the
//     existing chart-emitted env var used by jupyter SDK pods; we accept
//     it as a fallback so a deployment upgraded mid-stream keeps working.
//  3. "" — caller treats as "feature disabled".
//
// Returns "" when neither var is set or the URL is unusable.
func resolveCodexGatewayRESTURL() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_APP_GATEWAY_REST_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	v := strings.TrimSpace(os.Getenv("CODEX_APP_GATEWAY_URL"))
	if v == "" {
		return ""
	}
	v = strings.TrimSuffix(v, "/notebook/ws")
	v = strings.TrimRight(v, "/")
	switch {
	case strings.HasPrefix(v, "ws://"):
		return "http://" + strings.TrimPrefix(v, "ws://")
	case strings.HasPrefix(v, "wss://"):
		return "https://" + strings.TrimPrefix(v, "wss://")
	case strings.HasPrefix(v, "http://"), strings.HasPrefix(v, "https://"):
		return v
	}
	return ""
}

// CodexClient calls codex-app-gateway's POST /api/turns.
type CodexClient struct {
	baseURL string
	secret  string
	http    *http.Client
}

func NewCodexClient(baseURL, internalSecret string) *CodexClient {
	return &CodexClient{
		baseURL: baseURL,
		secret:  internalSecret,
		// Generous default — caller is the codex_im handler which has its
		// own per-turn timeout coming from the request body.
		http: &http.Client{Timeout: 6 * time.Minute},
	}
}

// CodexTurnRequest mirrors the spec'd /api/turns request body 1:1.
type CodexTurnRequest struct {
	WorkspaceID string          `json:"workspaceId"`
	ThreadID    *string         `json:"threadId,omitempty"`
	Params      json.RawMessage `json:"params"`
	TimeoutMs   int             `json:"timeoutMs,omitempty"`
}

// CodexTurnResponse mirrors the spec'd response.
type CodexTurnResponse struct {
	ThreadID  string               `json:"threadId"`
	Turn      json.RawMessage      `json:"turn,omitempty"`
	Transport *CodexTransportError `json:"transport,omitempty"`
}

type CodexTransportError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *CodexClient) RunTurn(ctx context.Context, req CodexTurnRequest) (*CodexTurnResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/turns", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		hreq.Header.Set("X-Internal-Secret", c.secret)
	}
	hresp, err := c.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("cxg: %w", err)
	}
	defer hresp.Body.Close()
	respBody, _ := io.ReadAll(hresp.Body)
	if hresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cxg /api/turns status=%d body=%s", hresp.StatusCode, string(respBody))
	}
	var out CodexTurnResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode: %w body=%s", err, string(respBody))
	}
	return &out, nil
}
