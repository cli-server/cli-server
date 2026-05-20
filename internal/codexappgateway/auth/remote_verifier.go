// Package auth implements inbound bearer-token verification.
//
// Phase 2 default is RemoteVerifier: each ws connect POSTs the supplied
// bearer to agentserver's /api/internal/codex/tokens/verify, which owns
// the codex_remote_tokens table and applies bcrypt + expiry + revocation
// policy. This couples the gateway to agentserver's lifecycle but keeps
// the gateway stateless.
//
// HMACAuthenticator stays in the package as a break-glass / local-test
// implementation but is no longer used in chart-deployed pods.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUnauthorized is returned by Verify when agentserver responds 401.
// Distinguishable so handlers can map directly to HTTP 401 without leaking
// other error reasons (network failure → 500, etc.).
var ErrUnauthorized = errors.New("auth: unauthorized")

// RemoteVerifier delegates token verification to agentserver's internal API.
type RemoteVerifier struct {
	baseURL    string
	bearer     string
	httpClient *http.Client
}

// NewRemoteVerifier constructs a verifier targeting agentserver's internal
// HTTP API. baseURL is the http base (e.g.
// "http://release-agentserver.namespace.svc:8080"); bearer is the value of
// INTERNAL_API_SECRET used as the X-Internal-Secret header.
func NewRemoteVerifier(baseURL, bearer string) *RemoteVerifier {
	return &RemoteVerifier{
		baseURL:    strings.TrimRight(baseURL, "/"),
		bearer:     bearer,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// Verify implements Authenticator.
func (v *RemoteVerifier) Verify(ctx context.Context, token string) (Identity, error) {
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return Identity{}, fmt.Errorf("marshal verify body: %w", err)
	}
	url := v.baseURL + "/api/internal/codex/tokens/verify"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Identity{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", v.bearer)
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("verify call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return Identity{}, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return Identity{}, fmt.Errorf("verify call: status=%d body=%q", resp.StatusCode, b)
	}

	var out struct {
		UserID      string `json:"user_id"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Identity{}, fmt.Errorf("decode verify response: %w", err)
	}
	return Identity{UserID: out.UserID, WorkspaceID: out.WorkspaceID}, nil
}

// OpenSession verifies the token AND inserts a browser-session row in
// codex_browser_sessions, returning the session id so the caller can close
// it on ws disconnect. Implements auth.SessionTracker.
func (v *RemoteVerifier) OpenSession(ctx context.Context, token, clientIP, clientUA, codexVersion, osStr string) (Identity, string, error) {
	body, err := json.Marshal(map[string]string{
		"token":         token,
		"client_ip":     clientIP,
		"client_ua":     clientUA,
		"codex_version": codexVersion,
		"os":            osStr,
	})
	if err != nil {
		return Identity{}, "", fmt.Errorf("marshal session-open body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.baseURL+"/api/internal/codex/tokens/session-open", bytes.NewReader(body))
	if err != nil {
		return Identity{}, "", fmt.Errorf("build session-open request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", v.bearer)
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return Identity{}, "", fmt.Errorf("session-open call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return Identity{}, "", ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return Identity{}, "", fmt.Errorf("session-open call: status=%d body=%q", resp.StatusCode, b)
	}
	var out struct {
		UserID      string `json:"user_id"`
		WorkspaceID string `json:"workspace_id"`
		SessionID   string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Identity{}, "", fmt.Errorf("decode session-open response: %w", err)
	}
	return Identity{UserID: out.UserID, WorkspaceID: out.WorkspaceID}, out.SessionID, nil
}

// CloseSession stamps disconnected_at on the row. Best-effort — callers
// invoke from a deferred goroutine with a short bg ctx so the ws close
// path is never blocked on it. Implements auth.SessionTracker.
func (v *RemoteVerifier) CloseSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	body, err := json.Marshal(map[string]string{"session_id": sessionID})
	if err != nil {
		return fmt.Errorf("marshal session-close body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.baseURL+"/api/internal/codex/tokens/session-close", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build session-close request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", v.bearer)
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("session-close call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("session-close call: status=%d body=%q", resp.StatusCode, b)
	}
	return nil
}
