package codexappgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WorkspaceTokenClient fetches the workspace's persistent proxy
// token from agentserver. Mirrors the cc-broker pattern: a long-lived
// token scoped to a workspace, injected into spawned subprocesses as
// their LLM credential. llmproxy validates the token per request and
// swaps it for a fresh modelserver JWT bound to the workspace's
// OAuth grant — so OAuth refreshes server-side surface to the pod
// transparently without needing a respawn.
type WorkspaceTokenClient struct {
	baseURL    string
	secret     string
	httpClient *http.Client
}

// NewWorkspaceTokenClient constructs a client against agentserver's
// `POST /internal/workspace-token` endpoint. secret must match the
// agentserver pod's INTERNAL_API_SECRET (sent as X-Internal-Secret).
func NewWorkspaceTokenClient(baseURL, secret string) *WorkspaceTokenClient {
	return &WorkspaceTokenClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		secret:     secret,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// GetOrCreate returns the workspace's persistent proxy token. The
// endpoint auto-creates one on first call.
func (c *WorkspaceTokenClient) GetOrCreate(ctx context.Context, workspaceID string) (string, error) {
	if workspaceID == "" {
		return "", fmt.Errorf("workspace token client: workspaceID required")
	}
	body, _ := json.Marshal(map[string]string{"workspace_id": workspaceID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/workspace-token", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		req.Header.Set("X-Internal-Secret", c.secret)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("workspace token fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("workspace token fetch: status=%d body=%q", resp.StatusCode, b)
	}

	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode workspace token: %w", err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("workspace token: empty token in response")
	}
	return out.Token, nil
}
