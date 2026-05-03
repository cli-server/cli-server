<h1 align="center">agentserver</h1>

<p align="center">
  <strong>Run your coding agent on any machine — access it from the browser.</strong>
</p>

<p align="center">
  <a href="https://platform.agentserver.dev"><img src="https://img.shields.io/badge/Try%20Now-platform.agentserver.dev-blue?style=for-the-badge" alt="Try Now"></a>
</p>

<p align="center">
  <a href="https://github.com/agentserver/agentserver/actions"><img src="https://github.com/agentserver/agentserver/actions/workflows/build.yml/badge.svg" alt="Build"></a>
  <a href="https://github.com/agentserver/agentserver/blob/main/LICENSE"><img src="https://img.shields.io/github/license/agentserver/agentserver" alt="License"></a>
  <a href="https://github.com/agentserver/agentserver/releases"><img src="https://img.shields.io/github/v/release/agentserver/agentserver" alt="Release"></a>
</p>

---

<p align="center">
  <img src="assets/screenshot-1.png" alt="agentserver Web UI" width="800">
</p>
<p align="center">
  <img src="assets/screenshot-2.png" alt="agentserver Coding Agent" width="800">
</p>

agentserver is to [opencode](https://github.com/opencode-ai/opencode) what [code-server](https://github.com/coder/code-server) is to VS Code — a self-hosted platform that lets your team use a coding agent from the browser.

## Why agentserver?

- **Zero install** — Open a browser, start coding with AI
- **Sandboxes** — Isolated containers per task; pause, resume, auto-pause on idle
- **Local tunneling** — Connect a local opencode instance via WebSocket, no public IP needed
- **Multi-tenancy** — Workspaces with role-based access (owner / maintainer / developer / guest)
- **Two backends** — Docker (single node) or Kubernetes with [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) + gVisor isolation
- **SSO ready** — GitHub OAuth and generic OIDC (Keycloak, Authentik, etc.) out of the box
- **API key proxy** — Sandboxes never see the real Anthropic key; injected server-side
- **LLM proxy** — Dedicated proxy service with per-workspace rate limiting (RPD quotas) and usage tracking
- **Admin panel** — Manage users, quotas, and system settings from the web UI
- **Batteries included** — Sandbox image ships with Go, Rust, C/C++, Node.js, Python 3, and common tools
- **Deploy anywhere** — Pre-built binaries, Homebrew, Docker Compose, or Helm for Kubernetes

## Architecture

agentserver consists of three services that can run as a single binary or be deployed independently:

```
                                                ┌──────────────────┐
                                                │  sandbox pod /   │
                                           ┌───▶│  container       │
                                           │    │  └─ opencode     │
Browser ──▶ sandbox-proxy (:8082) ─────────┤    └──────────────────┘
            (subdomain routing)            │        WebSocket tunnel
                                           └───▶ local agent machine
                                                  └─ opencode serve

Browser ──▶ agentserver (:8080) ──────────────▶ PostgreSQL
            ├─ REST API                         (users, workspaces,
            ├─ admin panel                       sandboxes, quotas)
            ├─ agent registration
            └─ tunnel endpoints

Sandbox ──▶ llmproxy (:8081) ──────────────▶ Anthropic API
            ├─ token validation                (real key injected
            ├─ RPD quota enforcement            server-side)
            └─ usage tracking
```

| Service | Default Port | Description |
|---------|-------------|-------------|
| **agentserver** | `:8080` | Main API server, web UI, tunnel endpoints |
| **llmproxy** | `:8081` | LLM API proxy with rate limiting and usage tracking |
| **sandbox-proxy** | `:8082` | Subdomain-based routing to sandbox services |

## Quick Start

### Docker Compose (recommended for local use)

```bash
git clone https://github.com/agentserver/agentserver.git && cd agentserver
docker build -f Dockerfile.opencode -t agentserver-agent:latest .
export ANTHROPIC_API_KEY="sk-ant-..."
docker compose up -d
```

Open `http://localhost:8080` in your browser.

### Helm (Kubernetes)

```bash
helm install agentserver oci://ghcr.io/agentserver/charts/agentserver \
  --namespace agentserver --create-namespace \
  --set database.url="postgres://user:pass@postgres:5432/agentserver?sslmode=disable" \
  --set anthropicApiKey="sk-ant-..." \
  --set ingress.enabled=true \
  --set ingress.host="cli.example.com" \
  --set baseDomain="cli.example.com"
```

### Pre-built Binaries

Download from [GitHub Releases](https://github.com/agentserver/agentserver/releases), or install via Homebrew:

```bash
brew install agentserver/tap/agentserver
```

## Local Agent Tunneling

Connect a locally-running opencode instance to agentserver — no public IP or third-party tunnel needed.

1. In the Web UI, click the laptop icon next to "Sandboxes" to generate a registration code.

2. On your local machine:

```bash
# First time — register with the server
agentserver connect \
  --server https://cli.example.com \
  --code <registration-code> \
  --name "My MacBook"

# Subsequent runs — auto-reconnect using saved credentials
agentserver connect
```

3. A **local** sandbox appears in the Web UI — click "Open" to access your local opencode through the browser.

### Multi-agent support

Register multiple agents on the same machine, each targeting a different directory and workspace:

```bash
# List all registered agents
agentserver list

# Remove a registration
agentserver remove --workspace <workspace-id>
```

Agent credentials are stored in `~/.agentserver/registry.json`.

**Tunnel features:** zero-config networking, auto-reconnect with backoff, binary WebSocket protocol (no base64 overhead), real-time SSE streaming, offline detection with auto-recovery.

## Configuration

See the [API reference](docs/api-reference.md) for full endpoint documentation.

<details>
<summary><strong>Helm Values</strong></summary>

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
| `baseDomain` | Base domain for subdomain routing | `""` |
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

</details>

<details>
<summary><strong>Environment Variables (Main Server)</strong></summary>

| Variable | Description | Default |
|----------|-------------|---------|
| `DATABASE_URL` | PostgreSQL connection string | (required) |
| `ANTHROPIC_API_KEY` | Anthropic API key | (required) |
| `ANTHROPIC_BASE_URL` | Custom API base URL | `https://api.anthropic.com` |
| `ANTHROPIC_AUTH_TOKEN` | Anthropic auth token (alternative to API key) | - |
| `OPENCODE_CONFIG_CONTENT` | JSON opencode config for sandbox pods | - |
| `BASE_DOMAIN` | Base domain for subdomain routing | - |
| `BASE_SCHEME` | URL scheme (`http` or `https`) | `https` |
| `IDLE_TIMEOUT` | Auto-pause timeout (e.g. `30m`) | `30m` |
| `AGENT_IMAGE` | Container image for sandbox agents | `ghcr.io/agentserver/opencode-agent:latest` |
| `LLMPROXY_URL` | Base URL of the LLM proxy service | - |
| `PASSWORD_AUTH_ENABLED` | Enable password-based auth | `true` |
| `OIDC_REDIRECT_BASE_URL` | External URL for OIDC callbacks | - |
| `GITHUB_CLIENT_ID` | GitHub OAuth client ID | - |
| `GITHUB_CLIENT_SECRET` | GitHub OAuth client secret | - |
| `OIDC_ISSUER_URL` | Generic OIDC issuer URL | - |
| `OIDC_CLIENT_ID` | Generic OIDC client ID | - |
| `OIDC_CLIENT_SECRET` | Generic OIDC client secret | - |
| `SANDBOX_NAMESPACE_PREFIX` | K8s namespace prefix | `agent-ws` |
| `NETWORKPOLICY_ENABLED` | Enable K8s NetworkPolicy isolation | `false` |
| `NETWORKPOLICY_DENY_CIDRS` | CIDRs to deny in network policies | - |
| `AGENTSERVER_NAMESPACE` | agentserver's own K8s namespace | - |
| `STORAGE_CLASS` | K8s storage class for PVCs | (cluster default) |
| `USER_DRIVE_SIZE` | Per-workspace storage size | `10Gi` |
| `USER_DRIVE_STORAGE_CLASS` | Storage class for workspace drives | inherits `STORAGE_CLASS` |
| `CC_BROKER_URL` | URL of the cc-broker service (required for TUI flow) | - |
| `EXECUTOR_REGISTRY_URL` | URL of the executor-registry service (required for TUI flow) | - |
| `INTERNAL_API_SECRET` | Shared secret for internal endpoints (recommended) | - |

</details>

<details>
<summary><strong>Environment Variables (LLM Proxy)</strong></summary>

| Variable | Description | Default |
|----------|-------------|---------|
| `LLMPROXY_LISTEN_ADDR` | HTTP listen address | `:8081` |
| `LLMPROXY_DATABASE_URL` | Proxy's own PostgreSQL connection URL | - |
| `LLMPROXY_AGENTSERVER_URL` | agentserver internal API URL for token validation | (required) |
| `ANTHROPIC_API_KEY` | Anthropic API key | (required*) |
| `ANTHROPIC_AUTH_TOKEN` | Anthropic auth token (alternative to API key) | (required*) |
| `ANTHROPIC_BASE_URL` | Upstream Anthropic API URL | `https://api.anthropic.com` |
| `LLMPROXY_DEFAULT_MAX_RPD` | Default max requests per day per workspace (0 = unlimited) | `0` |

</details>

<details>
<summary><strong>Environment Variables (Sandbox Proxy)</strong></summary>

| Variable | Description | Default |
|----------|-------------|---------|
| `DATABASE_URL` | PostgreSQL connection string | (required) |
| `LISTEN_ADDR` | HTTP listen address | `:8082` |
| `BASE_DOMAIN` | Base domain for subdomain routing | (required) |
| `OPENCODE_SUBDOMAIN_PREFIX` | Subdomain prefix for opencode sandboxes | `code` |
| `OPENCLAW_SUBDOMAIN_PREFIX` | Subdomain prefix for openclaw sandboxes | `claw` |
| `OPENCODE_ASSET_DOMAIN` | Domain for opencode static assets | `opencodeapp.{BASE_DOMAIN}` |

</details>

<details>
<summary><strong>OIDC Authentication</strong></summary>

**GitHub OAuth:**

```bash
helm upgrade agentserver oci://ghcr.io/agentserver/charts/agentserver \
  --reuse-values \
  --set oidc.redirectBaseUrl="https://cli.example.com" \
  --set oidc.github.enabled=true \
  --set oidc.github.clientId="your-client-id" \
  --set oidc.github.clientSecret="your-client-secret"
```

Callback URL: `https://cli.example.com/api/auth/oidc/github/callback`

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

</details>

<details>
<summary><strong>Kubernetes Backend</strong></summary>

For production multi-tenant deployments with gVisor isolation:

```bash
helm upgrade agentserver oci://ghcr.io/agentserver/charts/agentserver \
  --reuse-values \
  --set backend=k8s \
  --set opencode.runtimeClassName=gvisor \
  --set sandbox.namespace=agentserver
```

</details>

## Building from Source

```bash
# Prerequisites: Go 1.26, Node.js, pnpm, bun

# Build everything (frontend + backend)
make build

# Build individual components
make backend          # Go binary → bin/agentserver
make frontend         # React frontend → web/dist/
make agent            # Local agent binary → bin/agentserver-agent
make agent-all        # Agent for all platforms (linux/darwin/windows, amd64/arm64)
make llmproxy         # LLM proxy binary → bin/llmproxy

# Docker images
make docker           # Main server image
make docker-agent     # Agent container image
make docker-llmproxy  # LLM proxy image
make docker-all       # All images
```

## Contributing

```bash
# Terminal 1: Start backend
go run . serve --db-url "postgres://..." --backend docker

# Terminal 2: Start frontend dev server
cd web && pnpm install && pnpm dev
```

## License

[MIT](LICENSE)
