# Sandbox Proxy Extraction Design

## Goal

Extract sandbox traffic proxying (WebSocket tunnel + HTTP subdomain reverse proxy) from agentserver into an independently deployable `sandbox-proxy` service within the same mono-repo.

## Architecture

```
Browser ─── subdomain traffic ──→ sandbox-proxy ──→ Pod / Local Agent
Browser ─── API traffic ─────────→ agentserver
                                       ↑
llmproxy ── validate-proxy-token ──────┘

Both services share PostgreSQL.
```

## Approach: Shared DB (Option A)

sandbox-proxy connects directly to the same PostgreSQL database. This matches the existing llmproxy pattern, avoids extra network hops for latency-sensitive proxy traffic, and keeps implementation simple.

## Scope

### sandbox-proxy owns

1. Subdomain routing middleware — parse Host header, dispatch to opencode/openclaw proxy
2. Opencode subdomain proxy — cookie auth + HTTP reverse proxy (pod) / tunnel proxy (local)
3. Openclaw subdomain proxy — cookie auth + HTTP reverse proxy (pod)
4. Tunnel WebSocket endpoint — `/api/tunnel/{sandboxId}` accept agent connections
5. Static asset serving — opencode frontend SPA + shared asset domain
6. Activity/heartbeat DB writes for proxied sandboxes

### agentserver keeps

- Sandbox lifecycle management (create/pause/delete)
- Agent registration (`/api/agent/register`, one-time code)
- LLM proxy token validation (`/internal/validate-proxy-token`)
- Workspace/user management, all other API endpoints

## New Code Structure

```
cmd/sandboxproxy/main.go          — service entry point
internal/sandboxproxy/
    server.go                      — Server struct, Router(), New()
    config.go                      — Config from env vars
    opencode_proxy.go              — moved from server/opencode_proxy.go
    openclaw_proxy.go              — moved from server/openclaw_proxy.go
    tunnel.go                      — moved from server/tunnel.go
    error_page.go                  — moved from server/error_page.go
    activity.go                    — throttledActivity helper
```

## Shared Packages (read-only reuse)

| Package | Usage |
|---------|-------|
| `internal/db` | Query sandbox info, validate tunnel token, update heartbeat/activity |
| `internal/auth` | Validate user cookie tokens (shared JWT secret) |
| `internal/sbxstore` | In-memory sandbox store (Resolve by ID/shortID) |
| `internal/tunnel` | Registry + Protocol (unchanged) |
| `internal/shortid` | Short ID parsing |
| `opencodeweb` | Embedded opencode frontend static assets |

## Server Struct

```go
type Server struct {
    Auth             *auth.Auth
    DB               *db.DB
    Sandboxes        *sbxstore.Store
    TunnelRegistry   *tunnel.Registry
    OpencodeStaticFS fs.FS
    BaseDomain               string
    OpencodeAssetDomain      string
    OpencodeSubdomainPrefix  string
    OpenclawSubdomainPrefix  string
    activityMu   sync.Mutex
    activityLast map[string]time.Time
}
```

## Configuration (Environment Variables)

| Variable | Description |
|----------|-------------|
| `DATABASE_URL` | PostgreSQL connection string |
| `AUTH_JWT_SECRET` | Shared JWT secret with agentserver |
| `BASE_DOMAIN` | Base domain for subdomain routing |
| `OPENCODE_ASSET_DOMAIN` | Shared static asset domain |
| `OPENCODE_SUBDOMAIN_PREFIX` | Subdomain prefix for opencode (default: "code") |
| `OPENCLAW_SUBDOMAIN_PREFIX` | Subdomain prefix for openclaw (default: "claw") |
| `LISTEN_ADDR` | Listen address (default: ":8082") |

## Routing

```
Subdomain middleware:
  {prefix}-{id}.{baseDomain}   → opencode/openclaw proxy handler
  opencodeapp.{baseDomain}     → static asset handler

Path routes:
  GET  /healthz                → health check
  ANY  /api/tunnel/{sandboxId} → WebSocket tunnel endpoint
```

## Code Removal from agentserver

| File | Action |
|------|--------|
| `server/opencode_proxy.go` | Delete (moved to sandboxproxy) |
| `server/openclaw_proxy.go` | Delete (moved to sandboxproxy) |
| `server/tunnel.go` | Delete (moved to sandboxproxy) |
| `server/error_page.go` | Delete or extract to shared pkg |
| `server/server.go` | Remove: subdomain middleware, TunnelRegistry field, OpencodeStaticFS, BaseDomain, asset domain, subdomain prefixes, activityLast, throttledActivity(), initOpencodeAssetIndex() |
| `cmd/serve.go` | Remove: tunnel registry init, opencode static FS init, related env vars |

## Deployment Topology

```
Before:  Ingress → agentserver (all traffic)

After:   Ingress → agentserver    (API: /api/*, /internal/*)
         Ingress → sandbox-proxy  (subdomains: *.{baseDomain})
```

Subdomain traffic routed by Ingress Host-header rules to sandbox-proxy. API traffic continues to agentserver.

## Decisions

- Agent registration stays in agentserver (creates sandbox, generates tokens)
- Proxy token validation stays in agentserver (llmproxy calls agentserver)
- Both services share the same DB and same `internal/db` package
- Mono-repo: new binary at `cmd/sandboxproxy/`, shared internal packages
