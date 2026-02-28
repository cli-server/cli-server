<h1 align="center">cli-server</h1>

<p align="center">
  <strong>Run <a href="https://docs.anthropic.com/en/docs/claude-code">Claude Code</a> on any machine anywhere and access it in the browser.</strong>
</p>

<p align="center">
  <a href="https://github.com/cli-server/cli-server/actions"><img src="https://github.com/cli-server/cli-server/actions/workflows/build.yml/badge.svg" alt="Build"></a>
  <a href="https://github.com/cli-server/cli-server/blob/main/LICENSE"><img src="https://img.shields.io/github/license/cli-server/cli-server" alt="License"></a>
  <a href="https://github.com/cli-server/cli-server/releases"><img src="https://img.shields.io/github/v/release/cli-server/cli-server" alt="Release"></a>
</p>

---

<p align="center">
  <img src="assets/screenshot.png" alt="cli-server Web UI" width="800">
</p>

cli-server is to [Claude Code](https://docs.anthropic.com/en/docs/claude-code) what [code-server](https://github.com/coder/code-server) is to VS Code — a self-hosted web interface that lets your team use Claude Code from a browser, no local installation required.

## Highlights

- **Browser-based Claude Code** — Each sandbox runs [opencode](https://github.com/opencode-ai/opencode) serve, accessible via a per-sandbox subdomain
- **Local agent tunneling** — Connect a locally-running opencode instance to cli-server via a WebSocket reverse tunnel, no public IP needed
- **Workspaces & multi-tenancy** — Organize work into workspaces with role-based membership (owner / maintainer / developer / guest); each workspace has a shared persistent disk
- **Sandboxes** — Create multiple sandboxes per workspace; pause, resume, and auto-pause on idle
- **Two backends** — Run sandbox containers via Docker (single node) or Kubernetes with [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) + gVisor isolation
- **SSO / OIDC** — Built-in GitHub OAuth and generic OIDC support; accounts are linked by email
- **Anthropic API proxy** — Sandboxes never see the real API key; cli-server injects it server-side via a per-sandbox proxy token
- **Rich dev environment** — Sandbox image ships with Go, Rust, C/C++, Node.js, Python 3, and common tools out of the box
- **Cross-platform binary** — Pre-built binaries for Linux, macOS, and Windows (amd64 / arm64)
- **Helm one-liner** — Deploy to any Kubernetes cluster in minutes

## Architecture

```
Browser ──▶ cli-server (Go) ──▶ sandbox pod / container
               │                   └─ opencode serve (:4096)
               │
               ├─ PostgreSQL (users, workspaces, sandboxes)
               ├─ Anthropic API proxy (injects real API key)
               │
               │               WebSocket tunnel
Local machine ─┼──▶ cli-server agent connect ──────────▶ cli-server
               └─ opencode serve (:4096)                    │
                                                    Browser access via
                                                    subdomain proxy
```

| Component | Description |
|-----------|-------------|
| **cli-server** | Go HTTP server — auth, workspace & sandbox management, opencode subdomain proxy, WebSocket tunnel, Anthropic API proxy, static frontend |
| **sandbox** | Container running opencode serve — one per sandbox, isolated via Docker or K8s Agent Sandbox |
| **local agent** | `cli-server agent connect` — connects a local opencode instance to the server via a WebSocket reverse tunnel |

## Quick Start

### Prerequisites

- Kubernetes cluster (or Docker for local dev)
- PostgreSQL database
- An [Anthropic API key](https://console.anthropic.com/)

### Helm Install

```bash
helm install cli-server oci://ghcr.io/cli-server/charts/cli-server \
  --namespace cli-server --create-namespace \
  --set database.url="postgres://user:pass@postgres:5432/cliserver?sslmode=disable" \
  --set anthropicApiKey="sk-ant-..." \
  --set ingress.enabled=true \
  --set ingress.host="cli.example.com" \
  --set baseDomain="cli.example.com"
```

Open `https://cli.example.com`, register an account, create a workspace, and launch a sandbox.

### Docker Compose (Local Development)

```bash
git clone https://github.com/cli-server/cli-server.git
cd cli-server

# Build the opencode agent image
docker build -f Dockerfile.opencode -t cli-server-agent:latest .

# Set your API key
export ANTHROPIC_API_KEY="sk-ant-..."

# Start everything
docker compose up -d
```

Open `http://localhost:8080` in your browser.

## Local Agent Tunneling

You can connect a locally-running opencode instance to cli-server without a public IP or any third-party tunnel tool. The server manages it like any other sandbox — accessible via subdomain proxy in the Web UI.

### How it works

1. In the Web UI, click the laptop icon next to "Sandboxes" to generate a one-time registration code
2. On your local machine, download `cli-server` from the [latest release](https://github.com/cli-server/cli-server/releases) and run:

```bash
# First time: register with the code
cli-server agent connect \
  --server https://cli.example.com \
  --code <registration-code> \
  --name "My MacBook" \
  --opencode-url http://localhost:4096

# Subsequent runs: auto-reconnects using saved credentials (~/.cli-server/agent.json)
cli-server agent connect --opencode-url http://localhost:4096
```

3. A new sandbox labeled **local** appears in the Web UI. Click "Open" to access your local opencode through the browser.

### Features

- **Zero configuration networking** — WebSocket tunnel punches through NATs and firewalls
- **Auto-reconnect** — Exponential backoff reconnection on disconnect (1s → 2s → 4s → ... → 60s)
- **Binary protocol** — Raw binary WebSocket frames with chunked streaming, no base64 overhead
- **SSE streaming** — Agent execution updates stream in real-time through the tunnel
- **Offline detection** — Web UI shows `offline` status when the agent disconnects; automatically recovers to `running` on reconnect

### Tunnel protocol

The tunnel uses a binary WebSocket protocol. Each message is a binary frame:

```
[4 bytes: JSON header length] [JSON header] [raw binary payload]
```

- **Server → Agent**: request header (method, path, HTTP headers) + request body
- **Agent → Server**: stream header (status, HTTP headers, done flag) + response body chunk (16KB max)

All responses are chunked, keeping each WebSocket message well under the default 32KB limit.

## Concepts

### Workspaces

A workspace is a collaborative unit. It has members with roles and owns a shared persistent disk (PVC in K8s, named volume in Docker). All sandboxes in a workspace share this disk at `/data/disk0`.

| Role | Permissions |
|------|-------------|
| **owner** | Full control — manage members, delete workspace, create/manage sandboxes |
| **maintainer** | Add members, create/manage sandboxes |
| **developer** | Create and manage sandboxes |
| **guest** | View sandboxes (read-only access) |

### Sandboxes

A sandbox is an isolated container running opencode serve, or a local agent connected via WebSocket tunnel. Each sandbox:

- Has its own opencode instance accessible via `oc-{sandboxID}.{baseDomain}`
- Cloud sandboxes can be paused (scales to 0 replicas / stops container) and resumed
- Cloud sandboxes are automatically paused after a configurable idle timeout
- Local sandboxes show `offline` when the agent disconnects and recover on reconnect
- Gets a unique proxy token for Anthropic API access

### Sandbox statuses

| Status | Description |
|--------|-------------|
| `creating` | Container is being provisioned |
| `running` | Sandbox is active and accessible |
| `pausing` | Container is being paused |
| `paused` | Container is stopped, can be resumed |
| `resuming` | Container is being restarted |
| `offline` | Local agent disconnected (will recover on reconnect) |

## Configuration

### Helm Values

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Server image | `ghcr.io/cli-server/cli-server` |
| `image.tag` | Server image tag | `latest` |
| `opencode.image` | Opencode agent image for sandbox pods | `ghcr.io/cli-server/opencode-agent:latest` |
| `opencode.runtimeClassName` | RuntimeClass for sandbox pods (e.g. `gvisor`) | `""` |
| `database.url` | PostgreSQL connection string | (required) |
| `anthropicApiKey` | Anthropic API key | (required) |
| `anthropicBaseUrl` | Custom Anthropic API base URL | `""` |
| `anthropicAuthToken` | Anthropic auth token (alternative to API key) | `""` |
| `backend` | Sandbox backend: `docker` or `k8s` | `docker` |
| `baseDomain` | Base domain for subdomain routing (e.g. `cli.example.com`) | `""` |
| `baseScheme` | URL scheme for generated URLs | `https` |
| `idleTimeout` | Auto-pause idle sandboxes after | `30m` |
| `persistence.sessionStorageSize` | Per-sandbox ephemeral storage | `5Gi` |
| `persistence.userDriveSize` | Per-workspace shared disk size | `10Gi` |
| `persistence.storageClassName` | Storage class for PVCs | `""` (cluster default) |
| `workspace.resources` | Resource limits/requests for sandbox pods | `1Gi/1cpu` limits |
| `agentSandbox.install` | Install Agent Sandbox controller | `true` |
| `ingress.enabled` | Enable Nginx Ingress | `false` |
| `ingress.host` | Ingress hostname | `cli-server.example.com` |
| `ingress.tls` | Enable TLS (cert-manager) | `false` |
| `gateway.enabled` | Enable Gateway API HTTPRoute | `false` |

### OIDC Authentication

cli-server supports GitHub OAuth and generic OIDC providers alongside username/password auth. Accounts with the same email are automatically linked.

**GitHub OAuth:**

```bash
helm upgrade cli-server oci://ghcr.io/cli-server/charts/cli-server \
  --reuse-values \
  --set oidc.redirectBaseUrl="https://cli.example.com" \
  --set oidc.github.enabled=true \
  --set oidc.github.clientId="your-client-id" \
  --set oidc.github.clientSecret="your-client-secret"
```

Set the callback URL in your GitHub OAuth App to: `https://cli.example.com/api/auth/oidc/github/callback`

**Generic OIDC (Keycloak, Authentik, etc.):**

```bash
helm upgrade cli-server oci://ghcr.io/cli-server/charts/cli-server \
  --reuse-values \
  --set oidc.redirectBaseUrl="https://cli.example.com" \
  --set oidc.generic.enabled=true \
  --set oidc.generic.issuerUrl="https://idp.example.com/realms/main" \
  --set oidc.generic.clientId="cli-server" \
  --set oidc.generic.clientSecret="your-secret"
```

### Kubernetes Backend

For production multi-tenant deployments, use the Kubernetes backend with gVisor sandbox isolation:

```bash
helm upgrade cli-server oci://ghcr.io/cli-server/charts/cli-server \
  --reuse-values \
  --set backend=k8s \
  --set opencode.runtimeClassName=gvisor \
  --set sandbox.namespace=cli-server
```

This uses the [Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) controller to manage isolated pods per sandbox.

### Environment Variables

| Variable | Description |
|----------|-------------|
| `DATABASE_URL` | PostgreSQL connection string |
| `ANTHROPIC_API_KEY` | Anthropic API key |
| `ANTHROPIC_BASE_URL` | Custom API base URL |
| `ANTHROPIC_AUTH_TOKEN` | Anthropic auth token (alternative to API key) |
| `ANTHROPIC_PROXY_URL` | URL sandbox pods use to reach the Anthropic proxy |
| `BASE_DOMAIN` | Base domain for subdomain routing |
| `BASE_SCHEME` | URL scheme (`http` or `https`) |
| `IDLE_TIMEOUT` | Auto-pause timeout (e.g. `30m`) |
| `AGENT_IMAGE` | Container image for sandbox agents |
| `OIDC_REDIRECT_BASE_URL` | External URL for OIDC callbacks |
| `GITHUB_CLIENT_ID` | GitHub OAuth client ID |
| `GITHUB_CLIENT_SECRET` | GitHub OAuth client secret |
| `OIDC_ISSUER_URL` | Generic OIDC issuer URL |
| `OIDC_CLIENT_ID` | Generic OIDC client ID |
| `OIDC_CLIENT_SECRET` | Generic OIDC client secret |

## API

All endpoints under `/api/` require authentication via cookie unless noted otherwise.

### Workspaces

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/workspaces` | List workspaces for current user |
| `POST` | `/api/workspaces` | Create workspace (caller becomes owner) |
| `GET` | `/api/workspaces/{id}` | Get workspace details |
| `DELETE` | `/api/workspaces/{id}` | Delete workspace (owner only) |

### Members

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/workspaces/{id}/members` | List members |
| `POST` | `/api/workspaces/{id}/members` | Add member (owner/maintainer) |
| `PUT` | `/api/workspaces/{id}/members/{userId}` | Update member role (owner) |
| `DELETE` | `/api/workspaces/{id}/members/{userId}` | Remove member (owner) |

### Sandboxes

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/workspaces/{wid}/sandboxes` | List sandboxes in workspace |
| `POST` | `/api/workspaces/{wid}/sandboxes` | Create sandbox (developer+) |
| `GET` | `/api/sandboxes/{id}` | Get sandbox details |
| `DELETE` | `/api/sandboxes/{id}` | Delete sandbox |
| `POST` | `/api/sandboxes/{id}/pause` | Pause sandbox (cloud only) |
| `POST` | `/api/sandboxes/{id}/resume` | Resume sandbox (cloud only) |

### Local Agent

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/api/workspaces/{wid}/agent-code` | Cookie | Generate one-time registration code (developer+) |
| `POST` | `/api/agent/register` | Registration code | Register local agent, returns sandbox ID and tunnel token |
| `GET` | `/api/tunnel/{sandboxId}?token={tunnelToken}` | Tunnel token | WebSocket tunnel endpoint |

## Contributing

```bash
# Backend
go run . serve --db-url "postgres://..." --backend docker

# Frontend (separate terminal)
cd web && pnpm install && pnpm dev
```

## License

[MIT](LICENSE)
