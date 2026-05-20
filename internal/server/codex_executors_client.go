package server

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

// ExecutorsClient talks to codex-exec-gateway's user-management API on
// behalf of a session-authenticated agentserver user. Every request
// carries:
//   - X-Internal-Secret: agentserver's INTERNAL_API_SECRET (matches the
//     RequireAgentserverSecret middleware on the gateway side)
//   - X-User-Id: the calling user's ID (gateway uses this for its own
//     ownership ACL on executors)
type ExecutorsClient struct {
	baseURL    string
	secret     string
	httpClient *http.Client
}

func NewExecutorsClient(baseURL, secret string) *ExecutorsClient {
	return &ExecutorsClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		secret:     secret,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// RegisterExecutorRequest matches codex-exec-gateway's
// POST /api/codex-exec/register body. Per v0.54.0, executors no
// longer carry a default_cwd; that field was useless to the LLM and
// confusing in the UI.
type RegisterExecutorRequest struct {
	DisplayName string `json:"display_name,omitempty"`
}

// RegisterExecutorResponse matches codex-exec-gateway's response (raw
// token is returned ONCE — agentserver forwards it to the web UI for
// one-time display, never stores it).
type RegisterExecutorResponse struct {
	ExeID             string `json:"exe_id"`
	RegistrationToken string `json:"registration_token"`
}

// ListedExecutor matches codex-exec-gateway's
// GET /api/codex-exec/workspaces/{wid}/executors element shape.
// Per v0.54.0, surfaced fields are binding-level (name + description)
// from workspace_executors, not the executor row.
type ListedExecutor struct {
	ExeID          string     `json:"exe_id"`
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	IsDefault      bool       `json:"is_default"`
	LastSeenAt     *time.Time `json:"last_seen_at,omitempty"`
	ClientIP       string     `json:"client_ip,omitempty"`
	ClientUA       string     `json:"client_ua,omitempty"`
	CodexVersion   string     `json:"codex_version,omitempty"`
	OS             string     `json:"os,omitempty"`
	ConnectedAt    *time.Time `json:"connected_at,omitempty"`
	DisconnectedAt *time.Time `json:"disconnected_at,omitempty"`
	IsOnline       bool       `json:"is_online"`
}

// Register creates a new executor owned by userID and returns the raw
// registration token (one-time).
func (c *ExecutorsClient) Register(ctx context.Context, userID string, req RegisterExecutorRequest) (RegisterExecutorResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return RegisterExecutorResponse{}, fmt.Errorf("marshal register body: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/api/codex-exec/register", userID, bytes.NewReader(body))
	if err != nil {
		return RegisterExecutorResponse{}, err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return RegisterExecutorResponse{}, fmt.Errorf("register call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return RegisterExecutorResponse{}, fmt.Errorf("register: status=%d body=%q", resp.StatusCode, b)
	}
	var out RegisterExecutorResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return RegisterExecutorResponse{}, fmt.Errorf("decode register response: %w", err)
	}
	return out, nil
}

// Bind attaches an existing executor to a workspace under the
// caller-supplied name (workspace-unique). description is optional.
func (c *ExecutorsClient) Bind(ctx context.Context, userID, workspaceID, exeID, name, description string, isDefault bool) error {
	body, _ := json.Marshal(map[string]any{
		"exe_id":      exeID,
		"name":        name,
		"description": description,
		"is_default":  isDefault,
	})
	url := fmt.Sprintf("/api/codex-exec/workspaces/%s/executors", workspaceID)
	httpReq, err := c.newRequest(ctx, http.MethodPost, url, userID, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("bind call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("bind: status=%d body=%q", resp.StatusCode, b)
	}
	return nil
}

// Unregister deletes an executor row (and any of its bindings via
// CASCADE) from the gateway store. Used by the agentserver Register
// handler to clean up after a failed Bind so we don't leak orphan
// executors. Idempotent: 404 is treated as success.
func (c *ExecutorsClient) Unregister(ctx context.Context, userID, exeID string) error {
	url := fmt.Sprintf("/api/codex-exec/executors/%s", exeID)
	httpReq, err := c.newRequest(ctx, http.MethodDelete, url, userID, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("unregister call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("unregister: status=%d body=%q", resp.StatusCode, b)
	}
	return nil
}

// Unbind removes an executor from a workspace.
func (c *ExecutorsClient) Unbind(ctx context.Context, userID, workspaceID, exeID string) error {
	url := fmt.Sprintf("/api/codex-exec/workspaces/%s/executors/%s", workspaceID, exeID)
	httpReq, err := c.newRequest(ctx, http.MethodDelete, url, userID, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("unbind call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("unbind: status=%d body=%q", resp.StatusCode, b)
	}
	return nil
}

// List returns all executors bound to the workspace.
func (c *ExecutorsClient) List(ctx context.Context, userID, workspaceID string) ([]ListedExecutor, error) {
	url := fmt.Sprintf("/api/codex-exec/workspaces/%s/executors", workspaceID)
	httpReq, err := c.newRequest(ctx, http.MethodGet, url, userID, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("list call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("list: status=%d body=%q", resp.StatusCode, b)
	}
	var out []ListedExecutor
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list response: %w", err)
	}
	return out, nil
}

func (c *ExecutorsClient) newRequest(ctx context.Context, method, path, userID string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.secret != "" {
		req.Header.Set("X-Internal-Secret", c.secret)
	}
	if userID != "" {
		req.Header.Set("X-User-Id", userID)
	}
	return req, nil
}
