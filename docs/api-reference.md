# API Reference

All endpoints under `/api/` require authentication via cookie unless noted otherwise.

## Auth

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/api/auth/register` | None | Register a new account |
| `POST` | `/api/auth/login` | None | Login with username/password |
| `POST` | `/api/auth/logout` | Cookie | Logout and clear session |
| `GET` | `/api/auth/me` | Cookie | Get current user info |
| `GET` | `/api/auth/oidc/github` | None | Initiate GitHub OAuth flow |
| `GET` | `/api/auth/oidc/github/callback` | None | GitHub OAuth callback |
| `GET` | `/api/auth/oidc/generic` | None | Initiate generic OIDC flow |
| `GET` | `/api/auth/oidc/generic/callback` | None | Generic OIDC callback |

## Workspaces

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/workspaces` | List workspaces for current user |
| `POST` | `/api/workspaces` | Create workspace (caller becomes owner) |
| `GET` | `/api/workspaces/{id}` | Get workspace details |
| `DELETE` | `/api/workspaces/{id}` | Delete workspace (owner only) |

## Members

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/workspaces/{id}/members` | List members |
| `POST` | `/api/workspaces/{id}/members` | Add member (owner/maintainer) |
| `PUT` | `/api/workspaces/{id}/members/{userId}` | Update member role (owner) |
| `DELETE` | `/api/workspaces/{id}/members/{userId}` | Remove member (owner) |

## Sandboxes

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/workspaces/{wid}/sandboxes` | List sandboxes in workspace |
| `POST` | `/api/workspaces/{wid}/sandboxes` | Create sandbox (developer+) |
| `GET` | `/api/sandboxes/{id}` | Get sandbox details |
| `DELETE` | `/api/sandboxes/{id}` | Delete sandbox |
| `POST` | `/api/sandboxes/{id}/pause` | Pause sandbox (cloud only) |
| `POST` | `/api/sandboxes/{id}/resume` | Resume sandbox (cloud only) |

### Create Sandbox Request Body

```json
{
  "name": "my-sandbox",
  "type": "opencode",
  "telegramBotToken": ""
}
```

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Display name for the sandbox |
| `type` | string | Sandbox type: `opencode` or `openclaw` |
| `telegramBotToken` | string | (Optional) Telegram bot token, only for `openclaw` type |

## Local Agent

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/api/workspaces/{wid}/agent-code` | Cookie | Generate one-time registration code (developer+) |
| `POST` | `/api/agent/register` | Registration code | Register local agent, returns sandbox ID and tunnel token |
| `GET` | `/api/tunnel/{sandboxId}?token={tunnelToken}` | Tunnel token | WebSocket tunnel endpoint |

## Subdomain Proxy

Sandbox services are accessed via subdomain-based routing, not through the API directly.

| Sandbox Type | Subdomain Pattern | Description |
|--------------|-------------------|-------------|
| opencode | `oc-{sandboxID}.{baseDomain}` | Proxied to opencode serve (port 4096) |
| openclaw | `claw-{sandboxID}.{baseDomain}` | Proxied to openclaw gateway (port 18789) |

### Anthropic API Proxy

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `*` | `/proxy/anthropic/*` | Proxy token | Proxies requests to Anthropic API, injecting the real API key server-side |

Sandbox containers use their per-sandbox proxy token to access the Anthropic API through this endpoint. The real API key is never exposed to sandboxes.
