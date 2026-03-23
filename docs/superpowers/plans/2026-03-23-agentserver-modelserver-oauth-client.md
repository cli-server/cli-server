# Agentserver ModelServer OAuth Client — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable agentserver workspaces to connect to modelserver projects via OAuth 2.0, then route sandbox LLM traffic through modelserver using the access_token (transparently refreshed by llmproxy).

**Architecture:** agentserver acts as OAuth client to Hydra. On callback, stores access_token + refresh_token. llmproxy detects modelserver-connected workspaces and forwards LLM requests to modelserver with the access_token instead of Anthropic. Token refresh is lazy + singleflight-protected.

**Tech Stack:** Go, chi router, `golang.org/x/oauth2`, PostgreSQL, React/TypeScript

**Spec:** `docs/superpowers/specs/2026-03-23-modelserver-oauth-integration-design.md`

**Project root:** `/root/agentserver`

**Depends on:** modelserver Hydra OAuth Server (plan: `2026-03-23-modelserver-hydra-oauth-server.md`)

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `internal/db/migrations/006_modelserver_oauth.sql` | Create | workspace_modelserver_tokens table |
| `internal/db/modelserver_tokens.go` | Create | DB CRUD for modelserver connection |
| `internal/server/modelserver_oauth.go` | Create | OAuth connect/callback/disconnect/status handlers |
| `internal/server/server.go` | Modify | Add routes (~line 177), update handleCreateSandbox (~line 1153), add Server fields |
| `internal/server/validate_proxy_token.go` | Modify | Add modelserver_upstream_url to response |
| `internal/server/modelserver_token.go` | Create | Internal token endpoint + refresh logic |
| `internal/llmproxy/types.go` | Modify | Add ModelserverUpstreamURL to SandboxInfo |
| `internal/llmproxy/anthropic.go` | Modify | Dynamic target selection, skip RPD for modelserver |
| `internal/llmproxy/modelserver.go` | Create | Token cache + fetch from agentserver |
| `internal/llmproxy/config.go` | Modify | No changes needed (uses agentserver URL) |
| `internal/process/process.go` | Modify | Add CustomModels to StartOptions |
| `internal/container/manager.go` | Modify | Check CustomModels for OpenClaw |
| `web/src/lib/api.ts` | Modify | Add ModelserverStatus type + API functions |
| `web/src/components/WorkspaceDetail.tsx` | Modify | Add modelserver connection UI in SettingsTab |

---

### Task 1: Database Migration + DB Layer

**Files:**
- Create: `internal/db/migrations/006_modelserver_oauth.sql`
- Create: `internal/db/modelserver_tokens.go`

- [ ] **Step 1: Create migration file**

```sql
-- 006_modelserver_oauth.sql
CREATE TABLE workspace_modelserver_tokens (
    workspace_id     TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id       TEXT NOT NULL,
    project_name     TEXT NOT NULL,
    user_id          TEXT NOT NULL,
    access_token     TEXT NOT NULL,
    refresh_token    TEXT NOT NULL,
    token_expires_at TIMESTAMPTZ NOT NULL,
    models           JSONB NOT NULL DEFAULT '[]',
    created_at       TIMESTAMPTZ DEFAULT NOW(),
    updated_at       TIMESTAMPTZ DEFAULT NOW()
);
```

- [ ] **Step 2: Create DB layer**

```go
package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type ModelserverConnection struct {
	WorkspaceID    string     `json:"workspace_id"`
	ProjectID      string     `json:"project_id"`
	ProjectName    string     `json:"project_name"`
	UserID         string     `json:"user_id"`
	AccessToken    string     `json:"-"`
	RefreshToken   string     `json:"-"`
	TokenExpiresAt time.Time  `json:"token_expires_at"`
	Models         []LLMModel `json:"models"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (db *DB) GetModelserverConnection(workspaceID string) (*ModelserverConnection, error) {
	c := &ModelserverConnection{}
	var modelsJSON []byte
	err := db.QueryRow(
		`SELECT workspace_id, project_id, project_name, user_id,
		        access_token, refresh_token, token_expires_at,
		        models, created_at, updated_at
		 FROM workspace_modelserver_tokens WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&c.WorkspaceID, &c.ProjectID, &c.ProjectName, &c.UserID,
		&c.AccessToken, &c.RefreshToken, &c.TokenExpiresAt,
		&modelsJSON, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get modelserver connection: %w", err)
	}
	if err := json.Unmarshal(modelsJSON, &c.Models); err != nil {
		return nil, fmt.Errorf("unmarshal models: %w", err)
	}
	return c, nil
}

func (db *DB) SetModelserverConnection(c *ModelserverConnection) error {
	modelsJSON, err := json.Marshal(c.Models)
	if err != nil {
		return fmt.Errorf("marshal models: %w", err)
	}
	_, err = db.Exec(
		`INSERT INTO workspace_modelserver_tokens
		   (workspace_id, project_id, project_name, user_id,
		    access_token, refresh_token, token_expires_at, models, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		 ON CONFLICT (workspace_id) DO UPDATE SET
		   project_id = EXCLUDED.project_id,
		   project_name = EXCLUDED.project_name,
		   user_id = EXCLUDED.user_id,
		   access_token = EXCLUDED.access_token,
		   refresh_token = EXCLUDED.refresh_token,
		   token_expires_at = EXCLUDED.token_expires_at,
		   models = EXCLUDED.models,
		   updated_at = NOW()`,
		c.WorkspaceID, c.ProjectID, c.ProjectName, c.UserID,
		c.AccessToken, c.RefreshToken, c.TokenExpiresAt, modelsJSON,
	)
	return err
}

func (db *DB) DeleteModelserverConnection(workspaceID string) error {
	_, err := db.Exec("DELETE FROM workspace_modelserver_tokens WHERE workspace_id = $1", workspaceID)
	return err
}

func (db *DB) UpdateModelserverTokens(workspaceID, accessToken, refreshToken string, expiresAt time.Time) error {
	_, err := db.Exec(
		`UPDATE workspace_modelserver_tokens
		 SET access_token = $2, refresh_token = $3, token_expires_at = $4, updated_at = NOW()
		 WHERE workspace_id = $1`,
		workspaceID, accessToken, refreshToken, expiresAt,
	)
	return err
}

func (db *DB) HasModelserverConnection(workspaceID string) (bool, error) {
	var exists bool
	err := db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM workspace_modelserver_tokens WHERE workspace_id = $1)",
		workspaceID,
	).Scan(&exists)
	return exists, err
}
```

- [ ] **Step 3: Verify build**

Run: `cd /root/agentserver && go build ./...`

- [ ] **Step 4: Commit**

```bash
git add internal/db/migrations/006_modelserver_oauth.sql internal/db/modelserver_tokens.go
git commit -m "feat(db): add workspace_modelserver_tokens table and CRUD"
```

---

### Task 2: OAuth Connect/Callback/Disconnect/Status Handlers

**Files:**
- Create: `internal/server/modelserver_oauth.go`
- Modify: `internal/server/server.go` (add Server fields + routes)

- [ ] **Step 1: Add config fields to Server struct**

In `server.go`, add to `Server` struct (~line 47):

```go
// ModelServer OAuth
ModelserverOAuthClientID     string
ModelserverOAuthClientSecret string
ModelserverOAuthAuthURL      string
ModelserverOAuthTokenURL     string
ModelserverOAuthIntrospectURL string
ModelserverOAuthRedirectURI  string
ModelserverProxyURL          string
```

- [ ] **Step 2: Create OAuth handlers**

Create `internal/server/modelserver_oauth.go`:

```go
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/anthropics/agentserver/internal/db"
)

// --- Connect: initiate OAuth flow ---

func (s *Server) handleModelserverConnect(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if err := s.requireWorkspaceRole(r, wsID, "owner", "maintainer"); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.ModelserverOAuthAuthURL == "" {
		http.Error(w, "modelserver OAuth not configured", http.StatusNotImplemented)
		return
	}

	// State cookie (same pattern as oidc.go)
	stateBytes := make([]byte, 16)
	rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)
	http.SetCookie(w, &http.Cookie{
		Name: "modelserver-oauth-state", Value: state,
		Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})

	// Workspace ID cookie
	http.SetCookie(w, &http.Cookie{
		Name: "modelserver-oauth-wsid", Value: wsID,
		Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})

	// PKCE
	verifierBytes := make([]byte, 32)
	rand.Read(verifierBytes)
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	http.SetCookie(w, &http.Cookie{
		Name: "modelserver-oauth-pkce", Value: codeVerifier,
		Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	challengeHash := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])

	// Build authorization URL
	authURL, _ := url.Parse(s.ModelserverOAuthAuthURL)
	q := authURL.Query()
	q.Set("client_id", s.ModelserverOAuthClientID)
	q.Set("redirect_uri", s.ModelserverOAuthRedirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "project:inference offline_access")
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	authURL.RawQuery = q.Encode()

	http.Redirect(w, r, authURL.String(), http.StatusFound)
}

// --- Callback: exchange code, store tokens ---

func (s *Server) handleModelserverCallback(w http.ResponseWriter, r *http.Request) {
	// Validate state
	stateCookie, _ := r.Cookie("modelserver-oauth-state")
	if stateCookie == nil || stateCookie.Value == "" || r.URL.Query().Get("state") != stateCookie.Value {
		s.redirectModelserverError(w, r, "", "invalid OAuth state")
		return
	}
	wsidCookie, _ := r.Cookie("modelserver-oauth-wsid")
	pkceCookie, _ := r.Cookie("modelserver-oauth-pkce")
	if wsidCookie == nil || pkceCookie == nil {
		s.redirectModelserverError(w, r, "", "missing OAuth cookies")
		return
	}
	wsID := wsidCookie.Value
	codeVerifier := pkceCookie.Value

	// Clear cookies
	for _, name := range []string{"modelserver-oauth-state", "modelserver-oauth-wsid", "modelserver-oauth-pkce"} {
		http.SetCookie(w, &http.Cookie{Name: name, Path: "/", MaxAge: -1})
	}

	// Check for OAuth error
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		s.redirectModelserverError(w, r, wsID, "authorization denied: "+errParam)
		return
	}

	// Verify workspace role
	if err := s.requireWorkspaceRole(r, wsID, "owner", "maintainer"); err != nil {
		s.redirectModelserverError(w, r, wsID, "forbidden")
		return
	}

	code := r.URL.Query().Get("code")

	// Exchange code for tokens
	tokenResp, err := s.exchangeModelserverCode(code, codeVerifier)
	if err != nil {
		log.Printf("modelserver token exchange failed: %v", err)
		s.redirectModelserverError(w, r, wsID, "token exchange failed")
		return
	}

	// Introspect to get project_id, project_name, user_id
	projectID, projectName, userID, err := s.introspectModelserverToken(tokenResp.AccessToken)
	if err != nil {
		log.Printf("modelserver token introspection failed: %v", err)
		s.redirectModelserverError(w, r, wsID, "token introspection failed")
		return
	}

	// Fetch models from modelserver proxy
	models := s.fetchModelserverModels(tokenResp.AccessToken)

	// Delete existing BYOK config if any (modelserver supersedes)
	s.DB.DeleteWorkspaceLLMConfig(wsID)

	// Save connection
	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	conn := &db.ModelserverConnection{
		WorkspaceID:    wsID,
		ProjectID:      projectID,
		ProjectName:    projectName,
		UserID:         userID,
		AccessToken:    tokenResp.AccessToken,
		RefreshToken:   tokenResp.RefreshToken,
		TokenExpiresAt: expiresAt,
		Models:         models,
	}
	if err := s.DB.SetModelserverConnection(conn); err != nil {
		log.Printf("failed to save modelserver connection: %v", err)
		s.redirectModelserverError(w, r, wsID, "failed to save connection")
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/workspaces/%s?tab=settings&modelserver=connected", wsID), http.StatusFound)
}

// --- Disconnect ---

func (s *Server) handleModelserverDisconnect(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if err := s.requireWorkspaceRole(r, wsID, "owner", "maintainer"); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := s.DB.DeleteModelserverConnection(wsID); err != nil {
		http.Error(w, "failed to disconnect", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Status ---

func (s *Server) handleModelserverStatus(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if err := s.requireWorkspaceMember(r, wsID); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	conn, err := s.DB.GetModelserverConnection(wsID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if conn == nil {
		json.NewEncoder(w).Encode(map[string]bool{"connected": false})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"connected":    true,
		"project_id":   conn.ProjectID,
		"project_name": conn.ProjectName,
		"models":       conn.Models,
		"connected_at": conn.CreatedAt,
	})
}

// --- Helpers ---

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func (s *Server) exchangeModelserverCode(code, codeVerifier string) (*tokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {s.ModelserverOAuthClientID},
		"client_secret": {s.ModelserverOAuthClientSecret},
		"redirect_uri":  {s.ModelserverOAuthRedirectURI},
		"code_verifier": {codeVerifier},
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(s.ModelserverOAuthTokenURL, data)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, body)
	}
	var result tokenResponse
	return &result, json.NewDecoder(resp.Body).Decode(&result)
}

func (s *Server) introspectModelserverToken(accessToken string) (projectID, projectName, userID string, err error) {
	data := url.Values{"token": {accessToken}}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(s.ModelserverOAuthIntrospectURL, data)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	var result struct {
		Active bool                   `json:"active"`
		Ext    map[string]interface{} `json:"ext"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", "", err
	}
	if !result.Active {
		return "", "", "", fmt.Errorf("token not active")
	}
	projectID, _ = result.Ext["project_id"].(string)
	projectName, _ = result.Ext["project_name"].(string)
	userID, _ = result.Ext["user_id"].(string)
	return projectID, projectName, userID, nil
}

func (s *Server) fetchModelserverModels(accessToken string) []db.LLMModel {
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", s.ModelserverProxyURL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("failed to fetch modelserver models: %v", err)
		return nil
	}
	defer resp.Body.Close()
	var result struct {
		Data []string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	models := make([]db.LLMModel, len(result.Data))
	for i, s := range result.Data {
		models[i] = db.LLMModel{ID: s, Name: s}
	}
	return models
}

func (s *Server) redirectModelserverError(w http.ResponseWriter, r *http.Request, wsID, msg string) {
	if wsID == "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r,
		fmt.Sprintf("/workspaces/%s?tab=settings&modelserver=error&message=%s", wsID, url.QueryEscape(msg)),
		http.StatusFound)
}
```

- [ ] **Step 3: Add routes in server.go**

After line 177 (after LLM config routes):

```go
// ModelServer OAuth
r.Get("/api/workspaces/{id}/modelserver/connect", s.handleModelserverConnect)
r.Delete("/api/workspaces/{id}/modelserver/disconnect", s.handleModelserverDisconnect)
r.Get("/api/workspaces/{id}/modelserver/status", s.handleModelserverStatus)
```

Add callback route **outside** auth middleware (it's a redirect from external service, uses cookie-based auth):

```go
// ModelServer OAuth callback (uses cookie auth from the redirect)
r.Get("/api/auth/modelserver/callback", s.handleModelserverCallback)
```

Note: The callback handler calls `s.requireWorkspaceRole` internally using the auth cookie (the user's browser sends it with the redirect).

- [ ] **Step 4: Verify build**

Run: `cd /root/agentserver && go build ./...`

- [ ] **Step 5: Commit**

```bash
git add internal/server/modelserver_oauth.go internal/server/server.go
git commit -m "feat: add ModelServer OAuth connect/callback/disconnect/status endpoints"
```

---

### Task 3: Token Refresh + Internal Token Endpoint

**Files:**
- Create: `internal/server/modelserver_token.go`

- [ ] **Step 1: Create token refresh + internal endpoint**

```go
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"
)

var modelserverTokenRefresh singleflight.Group

func (s *Server) getValidModelserverToken(workspaceID string) (string, time.Time, error) {
	conn, err := s.DB.GetModelserverConnection(workspaceID)
	if err != nil || conn == nil {
		return "", time.Time{}, fmt.Errorf("no modelserver connection")
	}

	// Return if still valid (60s buffer)
	if time.Now().Before(conn.TokenExpiresAt.Add(-60 * time.Second)) {
		return conn.AccessToken, conn.TokenExpiresAt, nil
	}

	// Refresh with singleflight dedup
	type tokenResult struct {
		token     string
		expiresAt time.Time
	}
	result, err, _ := modelserverTokenRefresh.Do(workspaceID, func() (interface{}, error) {
		// Re-check after acquiring flight
		conn, err := s.DB.GetModelserverConnection(workspaceID)
		if err != nil || conn == nil {
			return nil, fmt.Errorf("no modelserver connection")
		}
		if time.Now().Before(conn.TokenExpiresAt.Add(-60 * time.Second)) {
			return &tokenResult{conn.AccessToken, conn.TokenExpiresAt}, nil
		}

		// Refresh
		data := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {conn.RefreshToken},
			"client_id":     {s.ModelserverOAuthClientID},
			"client_secret": {s.ModelserverOAuthClientSecret},
		}
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.PostForm(s.ModelserverOAuthTokenURL, data)
		if err != nil {
			return nil, fmt.Errorf("refresh request failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("refresh returned status %d", resp.StatusCode)
		}
		var tokenResp tokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
			return nil, fmt.Errorf("decode refresh response: %w", err)
		}

		expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
		newRefresh := tokenResp.RefreshToken
		if newRefresh == "" {
			newRefresh = conn.RefreshToken // Hydra may not rotate refresh tokens
		}
		if err := s.DB.UpdateModelserverTokens(workspaceID, tokenResp.AccessToken, newRefresh, expiresAt); err != nil {
			log.Printf("failed to persist refreshed tokens: %v", err)
		}
		return &tokenResult{tokenResp.AccessToken, expiresAt}, nil
	})
	if err != nil {
		return "", time.Time{}, err
	}
	tr := result.(*tokenResult)
	return tr.token, tr.expiresAt, nil
}

// handleInternalModelserverToken serves the access_token to llmproxy.
// Route: GET /internal/workspaces/{id}/modelserver-token
func (s *Server) handleInternalModelserverToken(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	token, expiresAt, err := s.getValidModelserverToken(wsID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token": token,
		"expires_at":   expiresAt,
	})
}
```

- [ ] **Step 2: Add internal route in server.go**

Find the internal routes section (around line 244 where `/internal/validate-proxy-token` is registered) and add:

```go
r.Get("/internal/workspaces/{id}/modelserver-token", s.handleInternalModelserverToken)
```

- [ ] **Step 3: Add singleflight dependency**

Run: `cd /root/agentserver && go get golang.org/x/sync/singleflight`

- [ ] **Step 4: Verify build**
- [ ] **Step 5: Commit**

```bash
git add internal/server/modelserver_token.go internal/server/server.go go.mod go.sum
git commit -m "feat: add token refresh with singleflight + internal token endpoint"
```

---

### Task 4: Update validate-proxy-token Response

**Files:**
- Modify: `internal/server/validate_proxy_token.go`
- Modify: `internal/llmproxy/types.go`

- [ ] **Step 1: Update validate_proxy_token.go to include modelserver info**

Replace the response encoding (~line 31-36) with:

```go
resp := map[string]interface{}{
	"sandbox_id":   sbx.ID,
	"workspace_id": sbx.WorkspaceID,
	"status":       sbx.Status,
}

// Check if workspace has modelserver connection
if s.ModelserverProxyURL != "" {
	hasMSConn, _ := s.DB.HasModelserverConnection(sbx.WorkspaceID)
	if hasMSConn {
		resp["modelserver_upstream_url"] = s.ModelserverProxyURL
	}
}

w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(resp)
```

- [ ] **Step 2: Update SandboxInfo in llmproxy/types.go**

Add field to SandboxInfo struct (~line 6-10):

```go
type SandboxInfo struct {
	ID                      string `json:"sandbox_id"`
	WorkspaceID             string `json:"workspace_id"`
	Status                  string `json:"status"`
	ModelserverUpstreamURL  string `json:"modelserver_upstream_url,omitempty"`
}
```

- [ ] **Step 3: Verify build**
- [ ] **Step 4: Commit**

```bash
git add internal/server/validate_proxy_token.go internal/llmproxy/types.go
git commit -m "feat: include modelserver_upstream_url in proxy token validation"
```

---

### Task 5: llmproxy Dynamic Forwarding

**Files:**
- Create: `internal/llmproxy/modelserver.go`
- Modify: `internal/llmproxy/anthropic.go`

- [ ] **Step 1: Create modelserver token cache**

```go
package llmproxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type cachedToken struct {
	accessToken string
	expiresAt   time.Time
	fetchedAt   time.Time
}

type modelserverTokenCache struct {
	mu     sync.RWMutex
	tokens map[string]*cachedToken // workspace_id -> token
}

func newModelserverTokenCache() *modelserverTokenCache {
	return &modelserverTokenCache{tokens: make(map[string]*cachedToken)}
}

func (c *modelserverTokenCache) Get(workspaceID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tokens[workspaceID]
	if !ok {
		return "", false
	}
	// Cache for max 5 minutes or until token expires (with 60s buffer)
	if time.Since(t.fetchedAt) > 5*time.Minute || time.Now().After(t.expiresAt.Add(-60*time.Second)) {
		return "", false
	}
	return t.accessToken, true
}

func (c *modelserverTokenCache) Set(workspaceID, token string, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[workspaceID] = &cachedToken{
		accessToken: token,
		expiresAt:   expiresAt,
		fetchedAt:   time.Now(),
	}
}

// fetchModelserverToken gets a fresh access_token from agentserver's internal API.
func (s *Server) fetchModelserverToken(workspaceID string) (string, time.Time, error) {
	// Check cache first
	if token, ok := s.msTokenCache.Get(workspaceID); ok {
		return token, time.Time{}, nil // expiresAt not critical for cached
	}

	url := fmt.Sprintf("%s/internal/workspaces/%s/modelserver-token", s.config.AgentserverURL, workspaceID)
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("fetch modelserver token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", time.Time{}, fmt.Errorf("modelserver token endpoint returned %d", resp.StatusCode)
	}
	var result struct {
		AccessToken string    `json:"access_token"`
		ExpiresAt   time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, err
	}
	s.msTokenCache.Set(workspaceID, result.AccessToken, result.ExpiresAt)
	return result.AccessToken, result.ExpiresAt, nil
}
```

- [ ] **Step 2: Add cache to Server struct**

In `internal/llmproxy/server.go`, add to Server struct:

```go
msTokenCache *modelserverTokenCache
```

In `NewServer()`:

```go
msTokenCache: newModelserverTokenCache(),
```

- [ ] **Step 3: Modify handleAnthropicProxy for dynamic forwarding**

In `internal/llmproxy/anthropic.go`, after proxy token validation (~line 40), before RPD check (~line 42), add modelserver routing:

```go
// Determine upstream target
targetURL := s.config.AnthropicBaseURL
useModelserver := sbx.ModelserverUpstreamURL != ""

if useModelserver {
	targetURL = sbx.ModelserverUpstreamURL
}
```

Replace the RPD check block (~lines 42-57) with:

```go
// RPD quota check — skip for modelserver (modelserver manages its own quota)
if isMessagesEndpoint && !useModelserver {
	if exceeded, current, max := s.checkRPD(sbx.WorkspaceID); exceeded {
		// ... existing RPD error response ...
	}
}
```

Replace the static reverse proxy setup (~lines 92-135) with dynamic target:

```go
target, err := url.Parse(targetURL)
if err != nil {
	logger.Error("invalid upstream URL", "error", err)
	http.Error(w, "invalid upstream URL", http.StatusInternalServerError)
	return
}

startTime := time.Now()

proxy := &httputil.ReverseProxy{
	Director: func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = r.URL.Path
		req.URL.RawQuery = r.URL.RawQuery
		req.Host = target.Host

		if useModelserver {
			// Modelserver: use access_token as Bearer
			msToken, _, err := s.fetchModelserverToken(sbx.WorkspaceID)
			if err != nil {
				logger.Error("failed to get modelserver token", "error", err)
				return
			}
			req.Header.Del("x-api-key")
			req.Header.Set("Authorization", "Bearer "+msToken)
		} else {
			// Anthropic: inject real API credentials (existing)
			if s.config.AnthropicAPIKey != "" {
				req.Header.Set("x-api-key", s.config.AnthropicAPIKey)
			}
			if s.config.AnthropicAuthToken != "" {
				req.Header.Set("Authorization", "Bearer "+s.config.AnthropicAuthToken)
			}
		}
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	},
	// ... rest unchanged (ModifyResponse, FlushInterval, ErrorHandler) ...
}
```

- [ ] **Step 4: Verify build**
- [ ] **Step 5: Commit**

```bash
git add internal/llmproxy/modelserver.go internal/llmproxy/server.go internal/llmproxy/anthropic.go
git commit -m "feat(llmproxy): dynamic forwarding to modelserver with token cache"
```

---

### Task 6: Sandbox Creation + CustomModels

**Files:**
- Modify: `internal/server/server.go` (~line 1153-1205)
- Modify: `internal/process/process.go` (StartOptions)
- Modify: `internal/container/manager.go` (~line 203-216)

- [ ] **Step 1: Add CustomModels to StartOptions**

In `process.go`, add after `BYOKModels` (~line 35):

```go
CustomModels []LLMModel // modelserver connection models (for OpenClaw, independent of BYOK)
```

- [ ] **Step 2: Update sandbox creation in server.go**

Replace BYOK block (~lines 1153-1205) with:

```go
// Look up modelserver connection first, then BYOK config
msConn, _ := s.DB.GetModelserverConnection(wsID)
byokCfg, err := s.DB.GetWorkspaceLLMConfig(wsID)
if err != nil {
	log.Printf("failed to get BYOK config for workspace %s: %v", wsID, err)
	byokCfg = nil
}

// ... (existing code for startOpts setup) ...

if msConn != nil {
	// Modelserver connection: sandbox routes through llmproxy, no BYOK injection
	startOpts.CustomModels = make([]process.LLMModel, len(msConn.Models))
	for i, m := range msConn.Models {
		startOpts.CustomModels[i] = process.LLMModel{ID: m.ID, Name: m.Name}
	}
} else if byokCfg != nil {
	startOpts.BYOKBaseURL = byokCfg.BaseURL
	startOpts.BYOKAPIKey = byokCfg.APIKey
	startOpts.BYOKModels = make([]process.LLMModel, len(byokCfg.Models))
	for i, m := range byokCfg.Models {
		startOpts.BYOKModels[i] = process.LLMModel{ID: m.ID, Name: m.Name}
	}
}
```

- [ ] **Step 3: Update container manager for CustomModels**

In `manager.go` (~line 206), after the BYOK check for OpenClaw:

```go
var cfgModels []process.LLMModel
if opts.BYOKBaseURL != "" {
	cfgBaseURL = opts.BYOKBaseURL
	cfgAPIKey = opts.BYOKAPIKey
	cfgModels = opts.BYOKModels
} else if len(opts.CustomModels) > 0 {
	cfgModels = opts.CustomModels
}
```

- [ ] **Step 4: Verify build**
- [ ] **Step 5: Commit**

```bash
git add internal/server/server.go internal/process/process.go internal/container/manager.go
git commit -m "feat: modelserver connection in sandbox creation + CustomModels for OpenClaw"
```

---

### Task 7: Frontend — ModelServer Connection UI

**Files:**
- Modify: `web/src/lib/api.ts`
- Modify: `web/src/components/WorkspaceDetail.tsx`

- [ ] **Step 1: Add API types and functions**

In `api.ts`, after the LLM config functions (~line 261):

```typescript
// ModelServer connection
export interface ModelserverStatus {
  connected: boolean
  project_id?: string
  project_name?: string
  models?: LLMModel[]
  connected_at?: string
}

export async function getModelserverStatus(workspaceId: string): Promise<ModelserverStatus> {
  const res = await fetch(`/api/workspaces/${workspaceId}/modelserver/status`)
  if (!res.ok) throw new Error('Failed to fetch modelserver status')
  return res.json()
}

export async function disconnectModelserver(workspaceId: string): Promise<void> {
  const res = await fetch(`/api/workspaces/${workspaceId}/modelserver/disconnect`, { method: 'DELETE' })
  if (!res.ok) throw new Error('Failed to disconnect')
}
```

- [ ] **Step 2: Update SettingsTab in WorkspaceDetail.tsx**

Add modelserver state alongside existing BYOK state (~line 398):

```typescript
const [msStatus, setMsStatus] = useState<ModelserverStatus | null>(null)
```

Add useEffect to load modelserver status:

```typescript
useEffect(() => {
  getModelserverStatus(workspaceId).then(setMsStatus).catch(() => {})
}, [workspaceId])
```

Add URL parameter handling for OAuth callback:

```typescript
useEffect(() => {
  const params = new URLSearchParams(window.location.search)
  if (params.get('modelserver') === 'connected') {
    // show success toast
    window.history.replaceState({}, '', window.location.pathname)
    getModelserverStatus(workspaceId).then(setMsStatus)
  } else if (params.get('modelserver') === 'error') {
    // show error toast with params.get('message')
    window.history.replaceState({}, '', window.location.pathname)
  }
}, [])
```

Add ModelServer section above the BYOK section in the JSX:

```tsx
{/* ModelServer Connection */}
<div style={{ marginBottom: 24, padding: 16, border: '1px solid #ddd', borderRadius: 8 }}>
  <h3>ModelServer Connection</h3>
  {msStatus?.connected ? (
    <>
      <p>Connected to project: <strong>{msStatus.project_name}</strong></p>
      {msStatus.models && msStatus.models.length > 0 && (
        <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap', marginBottom: 12 }}>
          {msStatus.models.map(m => (
            <span key={m.id} style={{ padding: '2px 8px', background: '#f0f0f0', borderRadius: 4, fontSize: '0.85rem' }}>{m.id}</span>
          ))}
        </div>
      )}
      <div style={{ display: 'flex', gap: 8 }}>
        <button onClick={() => window.location.href = `/api/workspaces/${workspaceId}/modelserver/connect`}>
          Reconnect
        </button>
        <button onClick={async () => {
          await disconnectModelserver(workspaceId)
          setMsStatus({ connected: false })
        }}>
          Disconnect
        </button>
      </div>
    </>
  ) : (
    <button onClick={() => window.location.href = `/api/workspaces/${workspaceId}/modelserver/connect`}>
      Connect to ModelServer
    </button>
  )}
</div>

{msStatus?.connected && (
  <p style={{ color: '#666', fontSize: '0.9rem', marginBottom: 12 }}>
    Manual BYOK configuration is overridden by the ModelServer connection.
  </p>
)}
```

- [ ] **Step 3: Verify frontend build**

Run: `cd /root/agentserver/web && pnpm build`

- [ ] **Step 4: Commit**

```bash
git add web/src/lib/api.ts web/src/components/WorkspaceDetail.tsx
git commit -m "feat(web): add ModelServer connection UI in workspace settings"
```

---

### Task 8: Configuration Wiring

**Files:**
- Modify: `internal/server/server.go` or `main.go` (wherever env vars are loaded)

- [ ] **Step 1: Add env var loading**

In the server initialization code, read environment variables:

```go
s.ModelserverOAuthClientID = os.Getenv("MODELSERVER_OAUTH_CLIENT_ID")
s.ModelserverOAuthClientSecret = os.Getenv("MODELSERVER_OAUTH_CLIENT_SECRET")
s.ModelserverOAuthAuthURL = os.Getenv("MODELSERVER_OAUTH_AUTH_URL")
s.ModelserverOAuthTokenURL = os.Getenv("MODELSERVER_OAUTH_TOKEN_URL")
s.ModelserverOAuthIntrospectURL = os.Getenv("MODELSERVER_OAUTH_INTROSPECT_URL")
s.ModelserverOAuthRedirectURI = os.Getenv("MODELSERVER_OAUTH_REDIRECT_URI")
s.ModelserverProxyURL = os.Getenv("MODELSERVER_PROXY_URL")
```

- [ ] **Step 2: Verify full build**

Run: `cd /root/agentserver && go build ./...`

- [ ] **Step 3: Commit**

```bash
git add internal/server/server.go
git commit -m "feat: wire ModelServer OAuth environment configuration"
```

---

### Task 9: End-to-End Verification

- [ ] **Step 1: Manual testing checklist**

1. Set env vars, start agentserver + llmproxy + modelserver + Hydra
2. Navigate to workspace settings → click "Connect to ModelServer"
3. Verify redirect to Hydra → modelserver login → project selection → callback
4. Verify `workspace_modelserver_tokens` row created
5. Verify status API returns connected
6. Create a sandbox → verify it routes through llmproxy → modelserver
7. Verify RPD quota is NOT applied for modelserver requests
8. Verify usage is still tracked
9. Click "Disconnect" → verify tokens deleted → sandbox falls back to default
10. Click "Reconnect" → verify new tokens replace old

- [ ] **Step 2: Final commit**

```bash
git commit -m "docs: add manual E2E test checklist for ModelServer OAuth"
```
