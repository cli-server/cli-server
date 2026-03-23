# ModelServer OAuth Integration Design

## Overview

Enable agentserver workspace users to connect their workspace to a modelserver project via OAuth 2.0 Authorization Code Flow (Ory Hydra). After authorization, the workspace's LLM traffic is automatically routed through modelserver using the OAuth access_token — no API keys involved. Token refresh is handled transparently by agentserver's llmproxy.

## Context

- **agentserver** — Multi-tenant coding agent platform. Sandboxes call LLM providers either directly (BYOK mode) or through agentserver's built-in llmproxy (:8081).
- **modelserver** — LLM API gateway. Admin API on `:8081`, LLM proxy on `:8080`. Projects have members (owner/maintainer/developer), credit-based rate limiting, and subscriptions.
- **Ory Hydra** — OAuth 2.0 / OIDC server, deployed as standalone Docker container. Public on `:4444`, Admin on `:4445`.

The two systems have independent user accounts. Domain routing between ports is operator-managed and not covered here.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  modelserver infrastructure                                      │
│                                                                  │
│  ┌────────────┐   ┌─────────────────┐   ┌────────────────────┐  │
│  │ Ory Hydra   │   │ modelserver     │   │ modelserver        │  │
│  │ Public:4444 │◄─►│ Admin API :8081 │   │ Proxy :8080        │  │
│  │ Admin :4445 │   │ (Login/Consent  │   │ (accepts API keys  │  │
│  │             │   │  Provider)      │   │  AND Hydra tokens) │  │
│  └────────────┘   └─────────────────┘   └────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────┐
│  agentserver infrastructure                                      │
│                                                                  │
│  ┌───────────────┐  ┌──────────┐  ┌────────────────┐            │
│  │ agentserver    │  │ llmproxy │  │ Web Frontend   │            │
│  │ API :8080      │  │ :8081    │  │ (React)        │            │
│  │ (OAuth Client) │──│ forwards │  │                │            │
│  │                │  │ to MS    │  │                │            │
│  └───────────────┘  └──────────┘  └────────────────┘            │
└──────────────────────────────────────────────────────────────────┘
```

**Key insight:** When a workspace is connected to modelserver, sandboxes use the same llmproxy path as non-BYOK mode. The llmproxy detects the modelserver connection and forwards to modelserver with the current access_token, refreshing it transparently. No credentials are injected into the sandbox.

## OAuth 2.0 Flow

```
User Browser           agentserver          Hydra(:4444)        modelserver(:8081)
    │                      │                    │                     │
    │ 1. Click "Connect    │                    │                     │
    │    to ModelServer"   │                    │                     │
    │─────────────────────>│                    │                     │
    │                      │                    │                     │
    │ 2. 302 → Hydra /oauth2/auth              │                     │
    │    ?client_id=agentserver                 │                     │
    │    &redirect_uri=...                      │                     │
    │    &state=<random_hex>                    │                     │
    │    &scope=project:inference offline_access      │                     │
    │    &response_type=code                    │                     │
    │    &code_challenge=<S256>                 │                     │
    │    &code_challenge_method=S256            │                     │
    │<─────────────────────│                    │                     │
    │                      │                    │                     │
    │ 3. GET /oauth2/auth ─────────────────────>│                     │
    │                                           │                     │
    │ 4. 302 → modelserver /oauth/login         │                     │
    │    ?login_challenge=...                   │                     │
    │<──────────────────────────────────────────│                     │
    │                                                                 │
    │ 5. Login (if not already logged in)                             │
    │<───────────────────────────────────────────────────────────────>│
    │ 6. Accept login challenge → 302 back to Hydra                   │
    │<───────────────────────────────────────────────────────────────│
    │                                           │                     │
    │ 7. 302 → modelserver /oauth/consent       │                     │
    │    ?consent_challenge=...                 │                     │
    │<──────────────────────────────────────────│                     │
    │                                                                 │
    │ 8. Project selection page                                       │
    │<───────────────────────────────────────────────────────────────>│
    │ 9. User selects project                                         │
    │───────────────────────────────────────────────────────────────>│
    │                                                                 │
    │ 10. Accept consent with:                                        │
    │     grant_scope: ["project:inference", "offline_access"]              │
    │     session.access_token: {project_id, project_name, user_id}   │
    │     remember: false                                             │
    │     302 → back to Hydra                                         │
    │<───────────────────────────────────────────────────────────────│
    │                                           │                     │
    │ 11. 302 → agentserver callback ?code&state│                     │
    │<──────────────────────────────────────────│                     │
    │                      │                    │                     │
    │ 12. callback?code&state                   │                     │
    │─────────────────────>│                    │                     │
    │                      │                    │                     │
    │                      │ 13. POST /oauth2/token                   │
    │                      │     {code, client_secret, code_verifier} │
    │                      │───────────────────>│                     │
    │                      │ 14. {access_token, refresh_token,        │
    │                      │      expires_in}   │                     │
    │                      │<──────────────────│                     │
    │                      │                    │                     │
    │                      │ 15. GET /v1/models (:8080)               │
    │                      │     Authorization: Bearer <access_token> │
    │                      │─────────────────────────────────────────>│
    │                      │ 16. {data: ["claude-sonnet-4-...", ...]} │
    │                      │<────────────────────────────────────────│
    │                      │                    │                     │
    │                      │ 17. Save tokens + models to              │
    │                      │     workspace_modelserver_tokens         │
    │                      │                    │                     │
    │ 18. 302 → workspace settings              │                     │
    │<─────────────────────│                    │                     │
```

**After connection is established — sandbox LLM call flow:**

```
Sandbox              agentserver llmproxy(:8081)        modelserver proxy(:8080)
  │                          │                                │
  │ POST /v1/messages        │                                │
  │ proxy_token: <token>     │                                │
  │─────────────────────────>│                                │
  │                          │                                │
  │                          │ validate proxy_token            │
  │                          │ → workspace has MS connection   │
  │                          │                                │
  │                          │ get access_token (refresh if    │
  │                          │ expired)                        │
  │                          │                                │
  │                          │ POST /v1/messages               │
  │                          │ Authorization: Bearer <AT>      │
  │                          │───────────────────────────────>│
  │                          │                                │
  │                          │ <streaming response>           │
  │                          │<───────────────────────────────│
  │                          │                                │
  │ <streaming response>     │                                │
  │<─────────────────────────│                                │
```

## modelserver Changes

### 1. Ory Hydra Deployment

Deploy Hydra as standalone Docker container:

- **Public :4444**: `/oauth2/auth`, `/oauth2/token`, `/.well-known/openid-configuration`
- **Admin :4445**: Internal, used by modelserver for login/consent flows
- **Database**: Shared PostgreSQL (separate schema) or dedicated DB
- **Configuration**:
  - `URLS_LOGIN` → modelserver `/oauth/login` endpoint
  - `URLS_CONSENT` → modelserver `/oauth/consent` endpoint
  - `URLS_SELF_ISSUER` → Hydra external URL

Register agentserver as OAuth client:
```json
{
  "client_id": "agentserver",
  "client_secret": "<generated>",
  "redirect_uris": ["https://<agentserver>/api/auth/modelserver/callback"],
  "grant_types": ["authorization_code", "refresh_token"],
  "response_types": ["code"],
  "scope": "project:inference offline_access",
  "token_endpoint_auth_method": "client_secret_post"
}
```

`offline_access` enables refresh_token issuance. `grant_types` includes `refresh_token`.

### 2. Login Provider

**Session mechanism:** modelserver currently uses short-lived JWT (15-min) without cookie sessions. Add a new cookie session for the OAuth login flow:
- `modelserver-oauth-session` cookie: AES-GCM encrypted `{user_id, expires_at}` (24-hour TTL)
- Flags: HttpOnly, Secure, SameSite=Lax

**Endpoints (new, on admin API :8081):**

`GET /oauth/login?login_challenge=<challenge>`
- Calls Hydra Admin `GET /admin/oauth2/auth/requests/login?login_challenge=<challenge>`
- If valid session cookie exists → accept login immediately (Hydra `skip`)
- Otherwise → render login page (Go HTML template)

`POST /oauth/login`
- Body: `{login_challenge, email, password}`
- Validates credentials (reuse `internal/auth/`)
- On success: set session cookie, accept login via Hydra Admin API, redirect

**Notes:**
- Login page rendered via Go `html/template` (new directory: `internal/admin/templates/`)
- v1: email/password only. OAuth provider buttons (GitHub/Google) add nested-OAuth complexity, defer to later.

### 3. Consent Provider

**Endpoints (new, on admin API :8081):**

`GET /oauth/consent?consent_challenge=<challenge>`
- Calls Hydra Admin to get challenge details + user identity
- Fetches user's projects via `store.ListProjectsByUser(userID)`
- Renders project selection page (Go HTML template)

`POST /oauth/consent`
- Body: `{consent_challenge, project_id}`
- Validates user has access to the project
- Accepts consent via Hydra Admin API:
  - `grant_scope: ["project:inference", "offline_access"]`
  - `session.access_token: {project_id, project_name, user_id}`
  - `remember: false`
- Redirects to URL from Hydra

**No API key creation.** The consent just binds the access_token to a project.

### 4. Proxy Hydra Token Auth

modelserver's LLM proxy (:8080) currently authenticates via API keys only. Add Hydra access_token support:

**Updated `extractAndValidateAuth` flow:**
1. Extract token from `x-api-key` header or `Authorization: Bearer` header
2. Try API key validation (existing: HMAC check → SHA256 hash lookup)
3. If not a valid API key, try Hydra token introspection:
   - `POST /admin/oauth2/introspect` to Hydra Admin (:4445) with `{token}`
   - If `active: true`, extract `ext.project_id` and `ext.user_id`
   - Load project from DB, verify status is active
   - Build an auth context equivalent to API key auth (project, user, rate limit policy)
4. If neither validates → 401

**Rate limiting:** When authenticated via Hydra token, rate limiting uses the project's subscription policy (same as API key auth for that project). The `ext.user_id` enables per-user credit quota enforcement if configured on the project member.

**Caching:** Token introspection results can be cached for a short period (e.g., 30 seconds) keyed by token hash, to avoid per-request Hydra calls.

### 5. Login/Consent Templates

New directory: `internal/admin/templates/`

**`login.html`:** Email + password form, hidden `login_challenge` field, error display. Styled to match modelserver branding.

**`consent.html`:** Project selection cards showing name, description, role, subscription plan. Hidden `consent_challenge` field. Each project is a form POST button.

## agentserver Changes

### 1. New Configuration

```
MODELSERVER_OAUTH_CLIENT_ID       - OAuth client ID (e.g., "agentserver")
MODELSERVER_OAUTH_CLIENT_SECRET   - OAuth client secret
MODELSERVER_OAUTH_AUTH_URL        - Hydra public authorization endpoint
MODELSERVER_OAUTH_TOKEN_URL       - Hydra public token endpoint
MODELSERVER_OAUTH_INTROSPECT_URL  - Hydra admin introspection endpoint (for extracting token claims in callback)
MODELSERVER_OAUTH_REDIRECT_URI    - Callback URL
MODELSERVER_PROXY_URL             - modelserver LLM proxy URL (for forwarding and /v1/models)
```

### 2. Database Migration

**`006_modelserver_oauth.sql`:**

```sql
CREATE TABLE workspace_modelserver_tokens (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL,
    project_name TEXT NOT NULL,
    user_id TEXT NOT NULL,              -- modelserver user ID (from token claims)
    access_token TEXT NOT NULL,
    refresh_token TEXT NOT NULL,
    token_expires_at TIMESTAMPTZ NOT NULL,
    models JSONB NOT NULL DEFAULT '[]', -- cached [{id, name}] from /v1/models
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);
```

**Note:** No changes to `workspace_llm_config`. The modelserver connection is an independent path from manual BYOK.

### 3. New Go Types

```go
// internal/db/modelserver_tokens.go (new file)

type ModelserverConnection struct {
    WorkspaceID    string          `json:"workspace_id"`
    ProjectID      string          `json:"project_id"`
    ProjectName    string          `json:"project_name"`
    UserID         string          `json:"user_id"`
    AccessToken    string          `json:"-"`
    RefreshToken   string          `json:"-"`
    TokenExpiresAt time.Time       `json:"token_expires_at"`
    Models         []LLMModel      `json:"models"`
    CreatedAt      time.Time       `json:"created_at"`
    UpdatedAt      time.Time       `json:"updated_at"`
}

func (db *DB) GetModelserverConnection(workspaceID string) (*ModelserverConnection, error)
func (db *DB) SetModelserverConnection(c *ModelserverConnection) error  // upsert
func (db *DB) DeleteModelserverConnection(workspaceID string) error
func (db *DB) UpdateModelserverTokens(workspaceID, accessToken, refreshToken string, expiresAt time.Time) error
```

### 4. New Backend Endpoints

**`GET /api/workspaces/{id}/modelserver/connect`**
- Requires: owner/maintainer role
- State: random 32-byte hex → cookie `modelserver-oauth-state` (10-min, HttpOnly, Secure, SameSite=Lax)
- Workspace ID: cookie `modelserver-oauth-wsid` (same flags)
- PKCE: generate `code_verifier` → cookie `modelserver-oauth-pkce`, compute `code_challenge` (S256)
- 302 → `MODELSERVER_OAUTH_AUTH_URL?client_id=...&redirect_uri=...&state=...&scope=project:inference offline_access&response_type=code&code_challenge=...&code_challenge_method=S256`

**`GET /api/auth/modelserver/callback`**
- Query: `code`, `state`
- Processing:
  1. Validate `state` vs cookie, extract `workspace_id` and `code_verifier`, clear cookies
  2. Verify user has owner/maintainer role on workspace
  3. Exchange code: `POST MODELSERVER_OAUTH_TOKEN_URL` with `{grant_type: authorization_code, code, client_id, client_secret, redirect_uri, code_verifier}` → `{access_token, refresh_token, expires_in}` (timeout 10s)
  4. Extract `project_id`, `project_name`, `user_id` from access_token via Hydra token introspection: `POST MODELSERVER_OAUTH_INTROSPECT_URL` with `{token: access_token}` → response `ext` field contains `{project_id, project_name, user_id}`. (Use introspection rather than local JWT decode to stay consistent with modelserver proxy's validation approach and ensure revoked tokens are rejected.)
  5. Fetch models: `GET MODELSERVER_PROXY_URL/v1/models` with `Authorization: Bearer <access_token>` → `{data: ["model-a", ...]}` (timeout 10s, empty list on failure)
  6. Transform models: `string[] → []LLMModel` — `LLMModel{ID: s, Name: s}` for each
  7. If workspace has existing BYOK config (`workspace_llm_config`), delete it (modelserver connection supersedes manual BYOK)
  8. Upsert `workspace_modelserver_tokens`: `{workspace_id, project_id, project_name, user_id, access_token, refresh_token, token_expires_at, models}`
  9. 302 → `/workspaces/{id}?tab=settings&modelserver=connected`
- Error: 302 → `/workspaces/{id}?tab=settings&modelserver=error&message=<urlencoded>`

**`DELETE /api/workspaces/{id}/modelserver/disconnect`**
- Requires: owner/maintainer role
- Delete `workspace_modelserver_tokens` row
- Return 204

**`GET /api/workspaces/{id}/modelserver/status`**
- Requires: workspace member
- Returns connection status:
```json
{
  "connected": true,
  "project_id": "...",
  "project_name": "My Project",
  "models": [{"id": "claude-sonnet-4-20250514", "name": "claude-sonnet-4-20250514"}, ...],
  "connected_at": "..."
}
```
Or `{"connected": false}` if no connection.

### 5. llmproxy Changes

The llmproxy (:8081) needs to detect modelserver-connected workspaces and forward accordingly.

**Extended internal validation:** The existing `POST /internal/validate-proxy-token` response adds a field:

```json
{
  "sandbox_id": "...",
  "workspace_id": "...",
  "status": "running",
  "modelserver_upstream_url": "https://code.ai.cs.ac.cn"
}
```

`modelserver_upstream_url` is present (non-empty) only if the workspace has a modelserver connection. The workspace_id for token fetching is already available from the top-level field.

**New internal endpoint:** `GET /internal/workspaces/{id}/modelserver-token`
- Returns the current access_token for the workspace, refreshing it if expired
- Response: `{"access_token": "...", "expires_at": "..."}`
- Called by llmproxy when it needs to forward a request
- llmproxy caches the token with short TTL (e.g., 5 minutes or until `expires_at`)

**llmproxy forwarding logic — refactoring `handleAnthropicProxy`:**

The current handler creates a static `httputil.ReverseProxy` targeting Anthropic. This needs to become a **per-request dynamic target selection**:

```go
func (s *Server) handleAnthropicProxy(w http.ResponseWriter, r *http.Request) {
    // ... read body, extract trace, validate proxy_token (existing) ...
    sbx, err := s.ValidateProxyToken(r.Context(), proxyToken)

    // Determine upstream target
    var targetURL string
    var authHeader string
    if sbx.ModelserverUpstreamURL != "" {
        // Modelserver path: get fresh access_token, forward to modelserver
        token, err := s.getModelserverToken(sbx.WorkspaceID)
        targetURL = sbx.ModelserverUpstreamURL
        authHeader = "Bearer " + token
    } else {
        // Anthropic path (existing)
        targetURL = s.config.AnthropicBaseURL
        authHeader = ""  // use x-api-key with AnthropicAPIKey
    }

    proxy := &httputil.ReverseProxy{
        Director: func(req *http.Request) {
            target, _ := url.Parse(targetURL)
            req.URL.Scheme = target.Scheme
            req.URL.Host = target.Host
            req.Host = target.Host
            if authHeader != "" {
                req.Header.Del("x-api-key")
                req.Header.Set("Authorization", authHeader)
            } else {
                // existing Anthropic auth injection
                req.Header.Set("x-api-key", s.config.AnthropicAPIKey)
            }
        },
        // ... ModifyResponse, FlushInterval (existing) ...
    }
    proxy.ServeHTTP(w, r)
}
```

**Usage tracking:** llmproxy continues to record usage for modelserver-forwarded requests (for agentserver's own analytics), but **skips RPD quota enforcement** for modelserver-connected workspaces — quota is managed by modelserver's credit system.

```go
// In RPD check:
if sbx.ModelserverUpstreamURL != "" {
    // Skip RPD quota — modelserver manages its own rate limiting
} else {
    // Existing RPD check for Anthropic path
}
```

**Token caching in llmproxy:** `getModelserverToken` caches access_tokens keyed by workspace_id with TTL = min(5 minutes, token_expires_at - now). Uses `sync.Map` or similar. On cache miss, calls agentserver `GET /internal/workspaces/{id}/modelserver-token`.

### 6. Token Refresh

agentserver implements a helper used by both the callback and the internal token endpoint. Uses `singleflight.Group` to prevent concurrent refresh storms when multiple llmproxy requests hit an expired token simultaneously:

```go
var tokenRefreshGroup singleflight.Group

func (s *Server) getValidModelserverToken(workspaceID string) (string, error) {
    conn, err := s.DB.GetModelserverConnection(workspaceID)
    if err != nil || conn == nil {
        return "", fmt.Errorf("no modelserver connection")
    }

    // Return if still valid (with 60s buffer)
    if time.Now().Before(conn.TokenExpiresAt.Add(-60 * time.Second)) {
        return conn.AccessToken, nil
    }

    // Refresh via singleflight to deduplicate concurrent refresh attempts
    result, err, _ := tokenRefreshGroup.Do(workspaceID, func() (interface{}, error) {
        // Re-check after acquiring the flight (another goroutine may have refreshed)
        conn, err := s.DB.GetModelserverConnection(workspaceID)
        if err != nil || conn == nil {
            return nil, fmt.Errorf("no modelserver connection")
        }
        if time.Now().Before(conn.TokenExpiresAt.Add(-60 * time.Second)) {
            return conn.AccessToken, nil
        }

        resp, err := refreshHydraToken(s.modelserverOAuthConfig, conn.RefreshToken)
        if err != nil {
            return nil, fmt.Errorf("refresh failed: %w", err)
        }

        expiresAt := time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
        s.DB.UpdateModelserverTokens(workspaceID, resp.AccessToken, resp.RefreshToken, expiresAt)
        return resp.AccessToken, nil
    })
    if err != nil {
        return "", err
    }
    return result.(string), nil
}
```

**Refresh failures:** If the refresh_token itself is expired (Hydra default: 30 days) or revoked, `getValidModelserverToken` returns an error. The llmproxy returns 502 to the sandbox. The user needs to re-authorize (click "Reconnect" in settings).

### 7. Sandbox Creation Changes

In `handlePostSandbox` (server.go), after the existing BYOK check, add modelserver connection check:

```go
llmConfig, _ := s.DB.GetWorkspaceLLMConfig(wsID)
msConn, _ := s.DB.GetModelserverConnection(wsID)

if msConn != nil {
    // Modelserver connection — sandbox uses llmproxy (no BYOK injection)
    // The llmproxy will forward to modelserver using the access_token
    // No BYOKBaseURL/BYOKAPIKey set → sandbox routes through llmproxy
} else if llmConfig != nil {
    // Manual BYOK — inject directly (existing behavior)
    opts.BYOKBaseURL = llmConfig.BaseURL
    opts.BYOKAPIKey = llmConfig.APIKey
    opts.BYOKModels = convertModels(llmConfig.Models)
}
// else: platform default via llmproxy
```

**Priority:** modelserver connection > manual BYOK > platform default.

**OpenClaw model list:** Currently `container/manager.go` only populates `cfgModels` inside the `if opts.BYOKBaseURL != ""` block. For modelserver connections (no BYOK), the models aren't injected into the OpenClaw config, so it falls back to default models. This needs a small refactor in `container/manager.go`:

```go
// Before:
var cfgModels []process.LLMModel
if opts.BYOKBaseURL != "" {
    cfgModels = opts.BYOKModels
}

// After: check CustomModels independently of BYOKBaseURL
var cfgModels []process.LLMModel
if opts.BYOKBaseURL != "" {
    cfgModels = opts.BYOKModels
} else if len(opts.CustomModels) > 0 {
    cfgModels = opts.CustomModels
}
```

Add `CustomModels []LLMModel` to `process.StartOptions` (used when modelserver connection provides models but sandbox still routes through llmproxy). In `handlePostSandbox`, set `opts.CustomModels = msConn.Models` when modelserver is connected.

For OpenCode sandboxes this is not needed — OpenCode discovers models dynamically via `GET /v1/models` through the llmproxy, which will forward to modelserver.

### 8. Frontend Changes (WorkspaceDetail.tsx SettingsTab)

**Add modelserver connection section above the BYOK section:**

When modelserver is connected (`status.connected === true`):
- Show: "Connected to ModelServer project: **{project_name}**"
- Show: model list as badges
- "Reconnect" button → `window.location.href = /api/workspaces/{id}/modelserver/connect`
- "Disconnect" button → `DELETE /api/workspaces/{id}/modelserver/disconnect`, reload

When modelserver is not connected:
- Show "Connect to ModelServer" button → `window.location.href = /api/workspaces/{id}/modelserver/connect`

Below that, show manual BYOK section (existing UI). If modelserver is connected, show a note: "Manual BYOK configuration is overridden by the ModelServer connection."

**URL parameter handling:**
- On mount, check `?modelserver=connected` → success toast, clean URL
- Check `?modelserver=error&message=...` → error toast, clean URL

**New API calls:**
```typescript
export async function getModelserverStatus(workspaceId: string): Promise<ModelserverStatus> {
  const res = await fetch(`/api/workspaces/${workspaceId}/modelserver/status`)
  return res.json()
}

export async function disconnectModelserver(workspaceId: string): Promise<void> {
  await fetch(`/api/workspaces/${workspaceId}/modelserver/disconnect`, { method: 'DELETE' })
}

interface ModelserverStatus {
  connected: boolean
  project_id?: string
  project_name?: string
  models?: LLMModel[]
  connected_at?: string
}
```

## Error Handling

| Scenario | Behavior |
|----------|----------|
| User cancels OAuth | Hydra redirects with `error=access_denied` → agentserver shows "Authorization cancelled" |
| Code exchange fails | Redirect to settings with error message |
| Models fetch fails | Store connection with empty models (non-fatal) |
| Access token expired during LLM call | llmproxy fetches a refreshed token before forwarding (60s buffer makes mid-flight expiry near-impossible; if modelserver returns 401, error propagates to sandbox) |
| Refresh token expired | llmproxy returns 502; user must click "Reconnect" |
| Project suspended on modelserver | modelserver proxy rejects request; sandbox sees error |
| Re-authorization (reconnect) | New tokens replace old; no key cleanup needed |
| Disconnect | Delete stored tokens; sandbox LLM calls fall back to platform default |
| Concurrent OAuth flows | Last one wins (upsert) |

## Security Considerations

1. **No credentials in sandbox**: sandbox has no modelserver tokens. LLM traffic routes through llmproxy which holds and refreshes the access_token server-side.
2. **State parameter**: Random hex in cookie, matches existing OIDC pattern (`internal/auth/oidc.go`).
3. **PKCE (RFC 7636)**: S256 code challenge, defense-in-depth.
4. **Scoped tokens**: Access token scoped to one project (`ext.project_id`); modelserver proxy rejects access to other projects.
5. **Token introspection**: modelserver validates Hydra tokens via admin introspection (not local JWT decode), ensuring revoked tokens are rejected.
6. **Short-lived access tokens**: Hydra access tokens are short-lived (configurable). Refresh handled transparently.
7. **Consent not remembered**: `remember: false` ensures project selection every time.
8. **Token storage**: Access/refresh tokens stored in agentserver DB (plaintext, consistent with existing patterns). Future: encrypt at rest.
9. **Internal token endpoint**: `/internal/workspaces/{id}/modelserver-token` is on the internal-only route group, not externally accessible.

## Testing Plan

1. **Unit tests**: State/PKCE generation, token refresh logic, model format transform, DB CRUD
2. **Integration tests (agentserver)**: Callback flow with mock Hydra; llmproxy forwarding with mock modelserver; token refresh cycle
3. **Integration tests (modelserver)**: Login/consent providers with mock Hydra admin; proxy dual-auth (API key + Hydra token); rate limiting with Hydra-authed requests
4. **E2E**: Browser flow → connection established → sandbox LLM call routes through modelserver → token refresh mid-session → disconnect
5. **Error cases**: Expired refresh token, revoked token, modelserver down, concurrent reconnects
