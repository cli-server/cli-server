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

cli-server is to [Claude Code](https://docs.anthropic.com/en/docs/claude-code) what [code-server](https://github.com/coder/code-server) is to VS Code — a self-hosted web interface that lets your team use Claude Code from a browser, no local installation required.

## Highlights

- **Browser-based Claude Code** — Full Claude Code CLI running in isolated containers, accessible from any device
- **Multi-user with sessions** — Each user gets their own sessions with persistent storage; pause and resume at any time
- **Two sandbox backends** — Run agent containers via Docker (single node) or Kubernetes with [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) + gVisor isolation
- **SSO / OIDC** — Built-in GitHub OAuth and generic OIDC support; accounts are linked by email
- **Helm one-liner** — Deploy to any Kubernetes cluster in minutes

## Architecture

```
Browser ──▶ cli-server (Go) ──▶ sidecar (Python) ──▶ agent container
               │                     │                   └─ Claude Code CLI
               │                     └─ Claude Agent SDK
               ├─ PostgreSQL (users, sessions, messages)
               └─ Redis (live streaming)
```

| Component | Description |
|-----------|-------------|
| **cli-server** | Go HTTP server — auth, session management, WebSocket terminal, static frontend |
| **sidecar** | Python FastAPI service — drives the Claude Agent SDK, streams responses via Redis SSE |
| **agent** | Minimal Debian container with Claude Code CLI installed — one per session |

## Quick Start

### Prerequisites

- Kubernetes cluster (or Docker for local dev)
- PostgreSQL database
- Redis instance
- An [Anthropic API key](https://console.anthropic.com/)

### Helm Install

```bash
helm install cli-server oci://ghcr.io/cli-server/charts/cli-server \
  --namespace cli-server --create-namespace \
  --set database.url="postgres://user:pass@postgres:5432/cliserver?sslmode=disable" \
  --set redis.url="redis://redis:6379" \
  --set anthropicApiKey="sk-ant-..." \
  --set ingress.enabled=true \
  --set ingress.host="cli.example.com"
```

That's it. Open `https://cli.example.com`, register an account, and start coding with Claude.

### Docker Compose (Local Development)

```bash
git clone https://github.com/cli-server/cli-server.git
cd cli-server

# Build the agent image first
docker build -f Dockerfile.agent -t cli-server-agent:latest .

# Set your API key
export ANTHROPIC_API_KEY="sk-ant-..."

# Start everything
docker compose up -d
```

Open `http://localhost:8080` in your browser.

## Configuration

### Helm Values

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Server image | `ghcr.io/cli-server/cli-server` |
| `image.tag` | Server image tag | `latest` |
| `sidecar.image` | Sidecar image | `ghcr.io/cli-server/sidecar:latest` |
| `agent.image` | Agent container image | `ghcr.io/cli-server/agent:latest` |
| `database.url` | PostgreSQL connection string | (required) |
| `redis.url` | Redis connection string | (required) |
| `anthropicApiKey` | Anthropic API key | (required) |
| `anthropicBaseUrl` | Custom Anthropic API base URL | `""` |
| `backend` | Session backend: `docker` or `k8s` | `docker` |
| `idleTimeout` | Auto-pause idle sessions after | `30m` |
| `persistence.userDriveSize` | Persistent storage per user | `10Gi` |
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
  --set agent.runtimeClassName=gvisor \
  --set sandbox.namespace=cli-server
```

This uses the [Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) controller to manage isolated pods per session.

### Environment Variables

| Variable | Description |
|----------|-------------|
| `DATABASE_URL` | PostgreSQL connection string |
| `REDIS_URL` | Redis connection string |
| `ANTHROPIC_API_KEY` | Anthropic API key |
| `ANTHROPIC_BASE_URL` | Custom API base URL |
| `OIDC_REDIRECT_BASE_URL` | External URL for OIDC callbacks |
| `GITHUB_CLIENT_ID` | GitHub OAuth client ID |
| `GITHUB_CLIENT_SECRET` | GitHub OAuth client secret |
| `OIDC_ISSUER_URL` | Generic OIDC issuer URL |
| `OIDC_CLIENT_ID` | Generic OIDC client ID |
| `OIDC_CLIENT_SECRET` | Generic OIDC client secret |

## Contributing

```bash
# Backend
go run . serve --db-url "postgres://..." --backend docker

# Frontend (separate terminal)
cd web && pnpm install && pnpm dev

# Sidecar (separate terminal)
cd sidecar && pip install -r requirements.txt
uvicorn app.main:app --port 8081 --reload
```

## License

[MIT](LICENSE)
