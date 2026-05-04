package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// AuthSource is implemented by AuthController. Bus uses it to fetch a fresh
// access token for every request (it's cheap when the token is already valid),
// and to mark credentials as invalid when the server rejects them (401).
type AuthSource interface {
	EnsureValid(ctx context.Context) (string, error)
	Invalidate()
}

type BusConfig struct {
	ServerURL   string
	WorkspaceID string
	ExecutorID  string
	Auth        AuthSource
	HTTP        *http.Client // optional; defaults to 30s timeout
}

type Bus struct {
	cfg  BusConfig
	http *http.Client
}

func NewBus(cfg BusConfig) *Bus {
	h := cfg.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	return &Bus{cfg: cfg, http: h}
}

// APIError is returned for any 4xx/5xx response with the standard
// {"error":{"code":"...","message":"..."}} body. Code may be empty for
// non-standard responses.
type APIError struct {
	HTTPStatus int
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api: %s (HTTP %d): %s", e.Code, e.HTTPStatus, e.Message)
}

// do performs the request with the bearer token, decodes a JSON response if
// `out` is non-nil, and unwraps {"error":{...}} responses into *APIError.
func (b *Bus) do(ctx context.Context, method, path string, body any, out any) error {
	tk, err := b.cfg.Auth.EnsureValid(ctx)
	if err != nil {
		return err
	}
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.cfg.ServerURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tk)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var wrap struct {
			Error APIError `json:"error"`
		}
		bb, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(bb, &wrap)
		wrap.Error.HTTPStatus = resp.StatusCode
		if wrap.Error.Code == "" {
			wrap.Error.Code = fmt.Sprintf("http_%d", resp.StatusCode)
			wrap.Error.Message = string(bb)
		}
		// 401 means the saved access token is no longer trusted by the server
		// (revoked, account deleted, signing key rotated, etc). Tell auth to
		// drop creds so the user can /login again — otherwise AuthController
		// stays in LoggedIn and refuses StartLogin with "already logged in".
		if resp.StatusCode == http.StatusUnauthorized {
			b.cfg.Auth.Invalidate()
		}
		return &wrap.Error
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ---- POST /api/agents/workspaces/{wid}/inbound ----

type InboundRequest struct {
	SessionID           string             `json:"session_id,omitempty"`
	Text                string             `json:"text"`
	Attachments         []InboundAttachment `json:"attachments,omitempty"`
	Metadata            map[string]any     `json:"metadata,omitempty"`
	PermissionResponder bool               `json:"permission_responder,omitempty"`
}

type InboundAttachment struct {
	Kind       string `json:"kind"`
	Filename   string `json:"filename"`
	Size       int    `json:"size"`
	ContentB64 string `json:"content_b64"`
}

type InboundResponse struct {
	SessionID    string `json:"session_id"`
	TurnID       string `json:"turn_id"`
	NextEventSeq int64  `json:"next_event_seq"`
}

// PostInbound submits a user prompt. The server creates a session implicitly
// if SessionID is empty, then claims an active turn (returning 409 if one
// is already in progress) and asynchronously kicks off cc-broker.
func (b *Bus) PostInbound(ctx context.Context, in InboundRequest) (*InboundResponse, error) {
	body := struct {
		InboundRequest
		ExecutorID string `json:"executor_id"`
	}{InboundRequest: in, ExecutorID: b.cfg.ExecutorID}
	var out InboundResponse
	err := b.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/agents/workspaces/%s/inbound", b.cfg.WorkspaceID),
		body, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- POST /api/agents/sessions ----

func (b *Bus) NewSession(ctx context.Context, permissionMode string, preferredExecutorID string) (string, error) {
	var out struct {
		SessionID string `json:"session_id"`
	}
	err := b.do(ctx, http.MethodPost, "/api/agents/sessions", map[string]any{
		"workspace_id":          b.cfg.WorkspaceID,
		"executor_id":           b.cfg.ExecutorID,
		"permission_mode":       permissionMode,
		"preferred_executor_id": preferredExecutorID,
	}, &out)
	return out.SessionID, err
}

// ---- POST /api/agents/sessions/{sid}/attach ----

type AttachResponse struct {
	SessionID         string  `json:"session_id"`
	PermResponder     *string `json:"permission_responder"`
	PreviousResponder string  `json:"previous_responder"`
	PreviousPreferred string  `json:"previous_preferred"`
}

func (b *Bus) AttachSession(ctx context.Context, sid, mode string) (*AttachResponse, error) {
	var out AttachResponse
	err := b.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/agents/sessions/%s/attach", sid),
		map[string]any{
			"executor_id":             b.cfg.ExecutorID,
			"mode":                    mode,
			"as_permission_responder": mode == "operator",
			"also_become_preferred":   mode == "operator",
		}, &out)
	return &out, err
}

// ---- GET /api/agents/sessions ----

type SessionListItem struct {
	SessionID           string  `json:"session_id"`
	ExternalID          string  `json:"external_id"`
	Title               string  `json:"title"`
	LastActivityAt      string  `json:"last_activity_at"`
	PermissionResponder *string `json:"permission_responder"`
}

func (b *Bus) ListSessions(ctx context.Context) ([]SessionListItem, error) {
	q := url.Values{}
	q.Set("workspace_id", b.cfg.WorkspaceID)
	q.Set("channel_type", "tui")
	q.Set("executor_id", b.cfg.ExecutorID)
	q.Set("latest", "20")
	var out struct {
		Sessions []SessionListItem `json:"sessions"`
	}
	err := b.do(ctx, http.MethodGet, "/api/agents/sessions?"+q.Encode(), nil, &out)
	return out.Sessions, err
}

// ---- POST /api/agents/sessions/{sid}/control ----

func (b *Bus) PostControl(ctx context.Context, sid, command string, args map[string]any) (json.RawMessage, error) {
	var out json.RawMessage
	err := b.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/agents/sessions/%s/control", sid),
		map[string]any{"command": command, "args": args}, &out)
	return out, err
}

// ---- POST /api/agents/sessions/{sid}/turns/{tid}/cancel ----

func (b *Bus) PostCancel(ctx context.Context, sid, tid string) error {
	return b.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/agents/sessions/%s/turns/%s/cancel", sid, tid),
		struct{}{}, nil)
}

// ---- POST /api/agents/sessions/{sid}/permissions/{pid} ----

func (b *Bus) PostDecision(ctx context.Context, sid, pid, decision, scope string) error {
	return b.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/agents/sessions/%s/permissions/%s", sid, pid),
		map[string]any{
			"decision":              decision,
			"scope":                 scope,
			"responder_executor_id": b.cfg.ExecutorID,
		}, nil)
}

// ---- GET /api/agents/executors/{id}/status ----

type ExecutorStatusResp struct {
	ExecutorID    string `json:"executor_id"`
	Status        string `json:"status"`
	LastHeartbeat string `json:"last_heartbeat_at"`
}

func (b *Bus) FetchExecutorStatus(ctx context.Context) (*ExecutorStatusResp, error) {
	var out ExecutorStatusResp
	err := b.do(ctx, http.MethodGet, "/api/agents/executors/"+b.cfg.ExecutorID+"/status", nil, &out)
	return &out, err
}

// ---- Accessors for SSE consumer (Task 4) ----

func (b *Bus) ServerURL() string  { return b.cfg.ServerURL }
func (b *Bus) ExecutorID() string { return b.cfg.ExecutorID }

// SetExecutorID updates the executor ID on the Bus. This should only be called
// during initialization (before any session activity), not concurrently with
// active requests. Used by tui_run.go when the executor is registered lazily
// (post-login) and the ID wasn't known at Bus construction time.
func (b *Bus) SetExecutorID(id string) {
	b.cfg.ExecutorID = id
}

// SetWorkspaceID updates the workspace ID on the Bus. Same caveats as
// SetExecutorID: only call during init, before any concurrent requests. Used
// when --workspace-id wasn't provided and the ID is resolved post-login by
// listing the user's workspaces.
func (b *Bus) SetWorkspaceID(id string) {
	b.cfg.WorkspaceID = id
}

func (b *Bus) WorkspaceID() string { return b.cfg.WorkspaceID }

// AccessToken exposes Auth.EnsureValid for the SSE consumer, which builds
// long-lived requests outside of `do`'s code path.
func (b *Bus) AccessToken(ctx context.Context) (string, error) {
	return b.cfg.Auth.EnsureValid(ctx)
}

// ---- GET /api/agents/workspaces ----

type WorkspaceListItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ListWorkspaces returns all workspaces the authenticated user is a member of.
// Used at startup when --workspace-id is not provided and no saved executor
// session matches the current server.
func (b *Bus) ListWorkspaces(ctx context.Context) ([]WorkspaceListItem, error) {
	var out []WorkspaceListItem
	if err := b.do(ctx, http.MethodGet, "/api/agents/workspaces", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
