# OAuth Device Flow Agent Registration Design

**Date**: 2026-04-07  
**Status**: Draft  
**Replaces**: One-time code agent registration flow

## Overview

Redesign the local agent registration flow in agentserver to use OAuth 2.0 Device Authorization Grant (RFC 8628) via Ory Hydra, replacing the current one-time code mechanism. Users run `agentserver-agent login` on their local machine, authenticate in a browser, select a workspace, and the agent is registered automatically.

### Goals

- Eliminate the manual "generate code in web UI → paste into CLI" workflow
- Provide a seamless browser-based authentication experience (like Claude Code's OAuth flow)
- Support headless environments with URL display and terminal QR codes
- Integrate workspace selection into the OAuth consent flow
- Maintain a clean token hierarchy: OAuth tokens (workspace-scoped) vs sandbox credentials (agent-scoped)

### Non-Goals

- Replacing the existing user authentication system (password/OIDC) for the web UI
- Supporting Authorization Code Flow with PKCE in the agent CLI (may be added later)
- Hydra HA deployment architecture (out of scope for this design)

## Architecture

### Component Diagram

```
┌─────────────────┐     ┌──────────────┐     ┌─────────────────────┐
│ agentserver-agent│     │   Ory Hydra  │     │    agentserver      │
│   (CLI binary)  │     │ (OAuth2 AS)  │     │ (login/consent      │
│                 │     │              │     │  provider + API)    │
└────────┬────────┘     └──────┬───────┘     └──────────┬──────────┘
         │                     │                        │
    1. POST /oauth2/device/auth│                        │
         │────────────────────>│                        │
         │  device_code,       │                        │
         │  user_code,         │                        │
         │  verification_uri   │                        │
         │<────────────────────│                        │
         │                     │                        │
    2. Try open browser        │                        │
       (verification_uri)      │                        │
       Fallback: URL + QR code │                        │
         │                     │                        │
         │              ┌──────┴──────┐                 │
         │              │ User Browser│                 │
         │              └──────┬──────┘                 │
         │                     │                        │
         │               3. GET verification_uri        │
         │                     │───────────────────────>│
         │                     │  4. Hydra login flow   │
         │                     │<──────────────────────>│
         │                     │  5. Hydra consent flow │
         │                     │    (workspace selection)│
         │                     │<──────────────────────>│
         │                     │                        │
    6. Poll POST /oauth2/token │                        │
       (grant_type=device_code)│                        │
         │────────────────────>│                        │
         │  access_token       │                        │
         │  (w/ workspace claim│                        │
         │  + refresh_token)   │                        │
         │<────────────────────│                        │
         │                     │                        │
    7. POST /api/agent/register│                        │
       (Bearer access_token)   │                        │
         │─────────────────────────────────────────────>│
         │  sandbox_id, tunnel_token, proxy_token       │
         │<─────────────────────────────────────────────│
         │                     │                        │
    8. Store credentials       │                        │
       locally                 │                        │
```

### Token Hierarchy

Two-level scope separation:

| Token | Scope | Purpose | Lifetime |
|-------|-------|---------|----------|
| OAuth access_token | User + Workspace | Identity verification, agent registration | 1 hour |
| OAuth refresh_token | User + Workspace | Renew access_token without re-auth | 30 days |
| sandbox credentials (tunnel_token, proxy_token) | Agent instance | WebSocket tunnel, task polling, ongoing comms | Permanent (until deleted) |

### Credential Refresh Strategy

Three-tier degradation:

1. **sandbox credentials valid** → use directly
2. **sandbox credentials fail (401)** → refresh access_token via refresh_token → re-register → get new sandbox credentials
3. **refresh_token expired** → trigger new Device Flow (interactive)

## Detailed Design

### 1. Ory Hydra Deployment

Hydra deployed as an independent service with its own database.

**Configuration** (`hydra.yaml`):

```yaml
serve:
  public:
    port: 4444
  admin:
    port: 4445

urls:
  self:
    issuer: https://auth.agentserver.example.com
  login: https://agentserver.example.com/oauth/login
  consent: https://agentserver.example.com/oauth/consent

oauth2:
  device_authorization:
    token_polling_interval: 5s

secrets:
  system:
    - ${HYDRA_SYSTEM_SECRET}

dsn: postgres://...
```

**OAuth2 Client Registration** (public client, no client_secret):

```json
{
  "client_id": "agentserver-agent-cli",
  "client_name": "Agentserver Agent CLI",
  "grant_types": [
    "urn:ietf:params:oauth:grant-type:device_code",
    "refresh_token"
  ],
  "response_types": [],
  "scope": "openid profile agent:register",
  "token_endpoint_auth_method": "none",
  "audience": ["https://agentserver.example.com"]
}
```

**Scope Design**:

- `openid` — standard OIDC, get user identity
- `profile` — basic user info
- `agent:register` — custom scope, permits agent registration

### 2. Agentserver as Login Provider

New backend endpoints in `internal/server/oauth_provider.go`:

**`GET /oauth/login`** — Hydra redirects here with `login_challenge`

Flow:
1. Call Hydra Admin API: `GET /admin/oauth2/auth/requests/login?login_challenge=xxx`
2. If user has existing agentserver session cookie → Accept login immediately
   - `PUT /admin/oauth2/auth/requests/login/accept` with `subject=user_id, remember=true`
   - Redirect to Hydra's returned redirect URL
3. If no session → Render login page (reuse existing auth: password / GitHub OIDC / Generic OIDC)
4. On successful login → Accept login via Hydra Admin API → Redirect back to Hydra

**`POST /oauth/login`** — Login form submission (for password auth path)

### 3. Agentserver as Consent Provider (with Workspace Selection)

**`GET /oauth/consent`** — Hydra redirects here with `consent_challenge`

Flow:
1. Call Hydra Admin API: `GET /admin/oauth2/auth/requests/consent?consent_challenge=xxx`
2. Extract `subject` (user_id) from consent request
3. Query user's workspaces: `db.GetUserWorkspaces(userID)` (reuse existing query)
4. Render consent page with workspace selection UI

**Consent Page UI**:

```
┌─────────────────────────────────────┐
│  Agentserver Agent requests access  │
│                                     │
│  Select workspace to join:          │
│  ○ My Team Workspace                │
│  ○ Personal Workspace               │
│  ○ Production Workspace             │
│                                     │
│  Permissions requested:              │
│  ✓ Register as local agent          │
│  ✓ Receive and execute tasks        │
│                                     │
│  [Deny]              [Allow & Join] │
└─────────────────────────────────────┘
```

**`POST /oauth/consent`** — User submits workspace choice

1. Extract selected `workspace_id` from form
2. Verify user has appropriate role in workspace (developer+)
3. Call Hydra Admin API: `PUT /admin/oauth2/auth/requests/consent/accept`

```json
{
  "grant_scope": ["openid", "profile", "agent:register"],
  "session": {
    "access_token": {
      "workspace_id": "selected-workspace-id",
      "workspace_role": "developer"
    },
    "id_token": {
      "workspace_id": "selected-workspace-id"
    }
  }
}
```

4. Redirect to Hydra's returned URL → Hydra completes device flow → CLI poll receives token

### 4. Agent Registration API (Modified)

**`POST /api/agent/register`** — Modified to accept OAuth Bearer token instead of one-time code

```
Authorization: Bearer <access_token>
Content-Type: application/json

{
  "name": "my-agent",
  "type": "claudecode"
}
```

Server-side logic:

1. Extract Bearer token from Authorization header
2. Call Hydra Admin API token introspection: `POST /admin/oauth2/introspect`
3. Verify token is active and has `agent:register` scope
4. Extract `user_id` (subject) and `workspace_id` (from token extra claims)
5. Verify user has workspace membership with developer+ role
6. Create sandbox (reuse existing logic): generate sandbox_id, tunnel_token, proxy_token, short_id
7. Return sandbox credentials

Response:
```json
{
  "sandbox_id": "uuid",
  "tunnel_token": "xxx",
  "proxy_token": "yyy",
  "workspace_id": "ws-id",
  "short_id": "abc123"
}
```

### 5. Hydra Admin Client

New file: `internal/auth/hydra.go`

```go
type HydraClient struct {
    AdminURL  string
    PublicURL string
    client    *http.Client
}

// Login Provider API
func (h *HydraClient) GetLoginRequest(challenge string) (*LoginRequest, error)
func (h *HydraClient) AcceptLogin(challenge string, body AcceptLoginBody) (string, error)
func (h *HydraClient) RejectLogin(challenge string, body RejectBody) (string, error)

// Consent Provider API
func (h *HydraClient) GetConsentRequest(challenge string) (*ConsentRequest, error)
func (h *HydraClient) AcceptConsent(challenge string, body AcceptConsentBody) (string, error)
func (h *HydraClient) RejectConsent(challenge string, body RejectBody) (string, error)

// Token Introspection
func (h *HydraClient) IntrospectToken(token string) (*IntrospectionResult, error)
```

Environment variables:
```
HYDRA_ADMIN_URL=http://hydra:4445
HYDRA_PUBLIC_URL=https://auth.agentserver.example.com
```

### 6. CLI: `login` Subcommand

New file: `internal/agent/login.go`

**Command signature**:
```
agentserver-agent login [flags]

Flags:
  --server string         Agentserver URL (default: from config)
  --name string           Agent display name (default: hostname)
  --type string           Agent type: opencode|claudecode (default: claudecode)
  --skip-open-browser     Don't auto-open browser, show URL + QR only
```

**Implementation**:

```go
func RunLogin(opts LoginOptions) error {
    // 1. Check existing registration
    existing := LoadRegistry(opts.Server)
    if existing != nil && existing.Valid() {
        fmt.Println("Already registered. Use 'remove' first to re-register.")
        return nil
    }

    // 2. Device Authorization Request
    deviceResp := requestDeviceCode(opts.HydraPublicURL, DeviceCodeRequest{
        ClientID: "agentserver-agent-cli",
        Scope:    "openid profile agent:register",
    })
    // Returns: device_code, user_code, verification_uri,
    //          verification_uri_complete, expires_in, interval

    // 3. Display auth info
    fmt.Printf("To authenticate, visit:\n  %s\n\n", deviceResp.VerificationURIComplete)
    fmt.Printf("Or enter code: %s at %s\n\n", deviceResp.UserCode, deviceResp.VerificationURI)

    // 4. Try opening browser
    if !opts.SkipOpenBrowser {
        if err := browser.OpenURL(deviceResp.VerificationURIComplete); err != nil {
            // Browser failed, show QR code
            qrterminal.GenerateWithConfig(deviceResp.VerificationURIComplete,
                qrterminal.Config{Level: qrterminal.L, Writer: os.Stderr})
        }
    } else {
        qrterminal.GenerateWithConfig(deviceResp.VerificationURIComplete,
            qrterminal.Config{Level: qrterminal.L, Writer: os.Stderr})
    }

    // 5. Poll for token
    fmt.Println("Waiting for authentication...")
    tokenResp := pollForToken(opts.HydraPublicURL, PollRequest{
        DeviceCode: deviceResp.DeviceCode,
        ClientID:   "agentserver-agent-cli",
        Interval:   deviceResp.Interval,
        ExpiresIn:  deviceResp.ExpiresIn,
    })

    // 6. Register agent with access_token
    registerResp := registerAgent(opts.AgentServerURL, RegisterRequest{
        AccessToken: tokenResp.AccessToken,
        Name:        opts.Name,
        Type:        opts.Type,
    })

    // 7. Save credentials locally
    SaveCredentials(Credentials{
        AccessToken:  tokenResp.AccessToken,
        RefreshToken: tokenResp.RefreshToken,
        ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
        HydraURL:     opts.HydraPublicURL,
    })

    SaveRegistry(RegistryEntry{
        Server:      opts.Server,
        SandboxID:   registerResp.SandboxID,
        TunnelToken: registerResp.TunnelToken,
        WorkspaceID: registerResp.WorkspaceID,
    })

    fmt.Printf("Registered as '%s' in workspace '%s'\n", opts.Name, registerResp.WorkspaceID)
    return nil
}
```

### 7. CLI: Token Refresh Logic

New file: `internal/agent/token_refresh.go`

```go
func EnsureValidCredentials(entry *RegistryEntry) error {
    // 1. Try existing sandbox credentials
    if err := pingServer(entry); err == nil {
        return nil
    }

    // 2. Load OAuth credentials
    creds := LoadCredentials()
    if creds == nil {
        return ErrNeedReLogin
    }

    // 3. Try refresh_token
    if creds.RefreshToken != "" {
        newToken, err := refreshAccessToken(creds.HydraURL, RefreshRequest{
            ClientID:     "agentserver-agent-cli",
            RefreshToken: creds.RefreshToken,
        })
        if err == nil {
            // 4. Re-register with new access_token
            newCreds, err := reRegisterAgent(entry.Server, newToken.AccessToken, entry)
            if err == nil {
                entry.SandboxID = newCreds.SandboxID
                entry.TunnelToken = newCreds.TunnelToken
                SaveRegistry(*entry)

                creds.AccessToken = newToken.AccessToken
                creds.RefreshToken = newToken.RefreshToken
                creds.ExpiresAt = time.Now().Add(time.Duration(newToken.ExpiresIn) * time.Second)
                SaveCredentials(*creds)
                return nil
            }
        }
    }

    // 5. All tokens expired, need interactive re-auth
    return ErrNeedReLogin
}
```

### 8. Local Credential Storage

**File**: `~/.agentserver/.credentials.json`

Stores OAuth tokens (workspace-scoped):

```json
{
  "accessToken": "eyJhbGciOi...",
  "refreshToken": "ory_rt_...",
  "expiresAt": 1712505600000,
  "hydraUrl": "https://auth.agentserver.example.com",
  "scopes": ["openid", "profile", "agent:register"]
}
```

**File**: `~/.agentserver/registry.json`

Stores sandbox credentials (agent-scoped, existing format extended):

```json
{
  "entries": {
    "/home/user/project": {
      "server": "https://agentserver.example.com",
      "sandboxId": "uuid",
      "tunnelToken": "xxx",
      "workspaceId": "ws-id",
      "registeredAt": "2026-04-07T10:00:00Z"
    }
  }
}
```

Split rationale: OAuth tokens are per-user, sandbox credentials are per-agent-instance. Different lifecycles, different refresh strategies.

## Migration Strategy

### Database

- New migration `015_hydra_oauth.sql`: No new tables needed (Hydra manages its own DB)
- `agent_registration_codes` table: Stop writing, keep for backward compatibility, remove in future migration

### CLI Compatibility

- `connect` command internally calls `RunLogin()` + establishes tunnel (replaces `Register()`)
- `claudecode` command: same
- Remove `--code` flag, add `--skip-open-browser` flag
- `list` / `remove` commands: unchanged
- Old `registry.json` entries without OAuth fields remain valid (backward-compatible read)

### Web UI

- Remove "Generate Registration Code" button
- Workspace settings page: add "Registered Agents" list (already partially exists)

## Security Considerations

- Device Flow `device_code` has Hydra-managed expiration (default 10 min)
- Access token: short-lived (1 hour), refresh token: 30 days
- CLI polling respects Hydra's `interval` and `slow_down` responses
- Token introspection via Hydra Admin API (internal network only)
- Consent page only shows workspaces where user has membership
- Registration API double-checks workspace membership (defense in depth)
- Agent registration requires `developer+` role
- Device code is single-use (Hydra built-in guarantee)
- Access token has audience restriction (`https://agentserver.example.com`)

## Error Handling

### CLI Error Scenarios

| Scenario | Handling |
|----------|---------|
| Network unreachable / Hydra unavailable | Prompt to check network and `--server` config |
| Device code expired (user didn't complete browser flow) | "Authorization expired. Please try again." |
| User denied consent in browser | Hydra returns `access_denied`, CLI shows "Authorization denied." |
| User has no workspaces | Consent page shows "No workspace available. Contact admin." |
| Registration API returns 403 | "No permission to register agent in this workspace." |
| refresh_token expired | Auto-trigger new Device Flow, prompt user to re-authenticate |

### Hydra Polling Responses

| Response | Action |
|----------|--------|
| `authorization_pending` | Continue polling |
| `slow_down` | Increase polling interval |
| `expired_token` | Device code expired, prompt retry |
| `access_denied` | User denied, exit |

## Dependencies

### Go Modules (new)

- `github.com/ory/hydra-client-go/v2` — Hydra Admin API client
- `github.com/pkg/browser` — Cross-platform browser opening (same as kubelogin)
- `github.com/mdp/qrterminal/v3` — Terminal QR code generation
- `golang.org/x/oauth2` — OAuth2 token exchange (already present)

### Frontend

- New consent page component (React, reuses existing UI component library)

## Files to Create/Modify

### New Files

| File | Purpose |
|------|---------|
| `internal/auth/hydra.go` | Hydra Admin API client |
| `internal/server/oauth_provider.go` | Login/Consent provider endpoints |
| `internal/agent/login.go` | CLI `login` subcommand |
| `internal/agent/token_refresh.go` | Token refresh and credential renewal logic |
| `internal/agent/credentials.go` | `.credentials.json` read/write |
| `web/src/pages/OAuthConsent.tsx` | Consent page with workspace selection |
| `web/src/pages/OAuthLogin.tsx` | Login page for OAuth flow (may reuse existing) |

### Modified Files

| File | Changes |
|------|---------|
| `cmd/agentserver-agent/main.go` | Add `login` command, modify `connect`/`claudecode` |
| `cmd/serve.go` | Add Hydra client initialization, register OAuth provider routes |
| `internal/server/server.go` | Register new `/oauth/*` routes |
| `internal/server/agent_register.go` | Change auth from one-time code to Bearer token + introspection |
| `internal/agent/connect.go` | Replace `Register()` call with `RunLogin()` |
| `internal/agent/claudecode.go` | Same |
| `internal/agent/config.go` | Add credentials file management |
