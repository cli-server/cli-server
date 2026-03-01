<h1 align="center">agentserver</h1>

<p align="center">
  <strong>Run your <a href="https://github.com/opencode-ai/opencode">coding agent</a> on any machine anywhere and access it in the browser.</strong>
</p>

<p align="center">
  <a href="https://github.com/agentserver/agentserver/actions"><img src="https://github.com/agentserver/agentserver/actions/workflows/build.yml/badge.svg" alt="Build"></a>
  <a href="https://github.com/agentserver/agentserver/blob/main/LICENSE"><img src="https://img.shields.io/github/license/agentserver/agentserver" alt="License"></a>
  <a href="https://github.com/agentserver/agentserver/releases"><img src="https://img.shields.io/github/v/release/agentserver/agentserver" alt="Release"></a>
</p>

---

<p align="center">
  <img src="assets/screenshot.png" alt="agentserver Web UI" width="800">
</p>

agentserver is to [opencode](https://github.com/opencode-ai/opencode) what [code-server](https://github.com/coder/code-server) is to VS Code — a self-hosted web interface that lets your team use a coding agent from a browser, no local installation required.

## Highlights

- **Browser-based coding agent** — Each sandbox runs [opencode](https://github.com/opencode-ai/opencode) serve, accessible via a per-sandbox subdomain
- **Local agent tunneling** — Connect a locally-running opencode instance to agentserver via a WebSocket reverse tunnel, no public IP needed
- **Workspaces & multi-tenancy** — Organize work into workspaces with role-based membership (owner / maintainer / developer / guest); each workspace has a shared persistent disk
- **Sandboxes** — Create multiple sandboxes per workspace; pause, resume, and auto-pause on idle
- **Two backends** — Run sandbox containers via Docker (single node) or Kubernetes with [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) + gVisor isolation
- **SSO / OIDC** — Built-in GitHub OAuth and generic OIDC support; accounts are linked by email
- **Anthropic API proxy** — Sandboxes never see the real API key; agentserver injects it server-side via a per-sandbox proxy token
- **Rich dev environment** — Sandbox image ships with Go, Rust, C/C++, Node.js, Python 3, and common tools out of the box
- **Cross-platform binary** — Pre-built binaries for Linux, macOS, and Windows (amd64 / arm64)
- **Helm one-liner** — Deploy to any Kubernetes cluster in minutes

## Architecture

```
Browser ──▶ agentserver (Go) ──▶ sandbox pod / container
               │                   └─ opencode serve (:4096)
               │
               ├─ PostgreSQL (users, workspaces, sandboxes)
               ├─ Anthropic API proxy (injects real API key)
               │
               │               WebSocket tunnel
Local machine ─┼──▶ agentserver agent connect ──────────▶ agentserver
               └─ opencode serve (:4096)                    │
                                                    Browser access via
                                                    subdomain proxy
```

| Component | Description |
|-----------|-------------|
| **agentserver** | Go HTTP server — auth, workspace & sandbox management, opencode subdomain proxy, WebSocket tunnel, Anthropic API proxy, static frontend |
| **sandbox** | Container running opencode serve — one per sandbox, isolated via Docker or K8s Agent Sandbox |
| **local agent** | `agentserver agent connect` — connects a local opencode instance to the server via a WebSocket reverse tunnel |

## Quick Start

### Prerequisites

- Kubernetes cluster (or Docker for local dev)
- PostgreSQL database
- An [Anthropic API key](https://console.anthropic.com/)

### Helm Install

```bash
helm install agentserver oci://ghcr.io/agentserver/charts/agentserver \
  --namespace agentserver --create-namespace \
  --set database.url="postgres://user:pass@postgres:5432/agentserver?sslmode=disable" \
  --set anthropicApiKey="sk-ant-..." \
  --set ingress.enabled=true \
  --set ingress.host="cli.example.com" \
  --set baseDomain="cli.example.com"
```

Open `https://cli.example.com`, register an account, create a workspace, and launch a sandbox.

### Docker Compose (Local Development)

```bash
git clone https://github.com/agentserver/agentserver.git
cd agentserver

# Build the opencode agent image
docker build -f Dockerfile.opencode -t agentserver-agent:latest .

# Set your API key
export ANTHROPIC_API_KEY="sk-ant-..."

# Start everything
docker compose up -d
```

Open `http://localhost:8080` in your browser.

## Local Agent Tunneling

You can connect a locally-running opencode instance to agentserver without a public IP or any third-party tunnel tool. The server manages it like any other sandbox — accessible via subdomain proxy in the Web UI.

### How it works

1. In the Web UI, click the laptop icon next to "Sandboxes" to generate a one-time registration code
2. On your local machine, download `agentserver` from the [latest release](https://github.com/agentserver/agentserver/releases) and run:

```bash
# First time: register with the code
agentserver agent connect \
  --server https://cli.example.com \
  --code <registration-code> \
  --name "My MacBook" \
  --opencode-url http://localhost:4096

# Subsequent runs: auto-reconnects using saved credentials (~/.agentserver/agent.json)
agentserver agent connect --opencode-url http://localhost:4096
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

### Sandbox types

agentserver supports multiple sandbox types:

| Type | Image | Port | Subdomain | Description |
|------|-------|------|-----------|-------------|
| **opencode** | `opencode-agent` | 4096 | `oc-{id}.{baseDomain}` | [opencode](https://github.com/opencode-ai/opencode) coding agent with password auth |
| **openclaw** | custom | 18789 | `claw-{id}.{baseDomain}` | [OpenClaw](https://github.com/nicepkg/openclaw) gateway with token-based WebSocket auth; optional Telegram bot integration |
| **local** | — | — | `oc-{id}.{baseDomain}` | Local opencode instance connected via WebSocket tunnel |

A sandbox is an isolated container running a coding agent, or a local agent connected via WebSocket tunnel. Each sandbox:

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
| `image.repository` | Server image | `ghcr.io/agentserver/agentserver` |
| `image.tag` | Server image tag | `latest` |
| `opencode.image` | Opencode agent image for sandbox pods | `ghcr.io/agentserver/opencode-agent:latest` |
| `opencode.runtimeClassName` | RuntimeClass for sandbox pods (e.g. `gvisor`) | `""` |
| `openclaw.image` | OpenClaw gateway image | `""` |
| `openclaw.port` | OpenClaw gateway port | `18789` |
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
| `ingress.host` | Ingress hostname | `agentserver.example.com` |
| `ingress.tls` | Enable TLS (cert-manager) | `false` |
| `gateway.enabled` | Enable Gateway API HTTPRoute | `false` |

### OIDC Authentication

agentserver supports GitHub OAuth and generic OIDC providers alongside username/password auth. Accounts with the same email are automatically linked.

**GitHub OAuth:**

```bash
helm upgrade agentserver oci://ghcr.io/agentserver/charts/agentserver \
  --reuse-values \
  --set oidc.redirectBaseUrl="https://cli.example.com" \
  --set oidc.github.enabled=true \
  --set oidc.github.clientId="your-client-id" \
  --set oidc.github.clientSecret="your-client-secret"
```

Set the callback URL in your GitHub OAuth App to: `https://cli.example.com/api/auth/oidc/github/callback`

**Generic OIDC (Keycloak, Authentik, etc.):**

```bash
helm upgrade agentserver oci://ghcr.io/agentserver/charts/agentserver \
  --reuse-values \
  --set oidc.redirectBaseUrl="https://cli.example.com" \
  --set oidc.generic.enabled=true \
  --set oidc.generic.issuerUrl="https://idp.example.com/realms/main" \
  --set oidc.generic.clientId="agentserver" \
  --set oidc.generic.clientSecret="your-secret"
```

### Kubernetes Backend

For production multi-tenant deployments, use the Kubernetes backend with gVisor sandbox isolation:

```bash
helm upgrade agentserver oci://ghcr.io/agentserver/charts/agentserver \
  --reuse-values \
  --set backend=k8s \
  --set opencode.runtimeClassName=gvisor \
  --set sandbox.namespace=agentserver
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

See [docs/api-reference.md](docs/api-reference.md) for the full API reference.

## Contributing

```bash
# Backend
go run . serve --db-url "postgres://..." --backend docker

# Frontend (separate terminal)
cd web && pnpm install && pnpm dev
```

## License

[MIT](LICENSE)
