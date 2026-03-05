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
- **SSO ready** — GitHub OAuth and generic OIDC out of the box
- **API key proxy** — Sandboxes never see the real Anthropic key; injected server-side
- **Batteries included** — Sandbox image ships with Go, Rust, C/C++, Node.js, Python 3, and common tools
- **Deploy anywhere** — Pre-built binaries (Linux/macOS/Windows) and a Helm one-liner for Kubernetes

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

## Quick Start

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

### Docker Compose (Local)

```bash
git clone https://github.com/agentserver/agentserver.git && cd agentserver
docker build -f Dockerfile.opencode -t agentserver-agent:latest .
export ANTHROPIC_API_KEY="sk-ant-..."
docker compose up -d
```

Open `http://localhost:8080` in your browser.

## Local Agent Tunneling

Connect a locally-running opencode instance to agentserver — no public IP or third-party tunnel needed.

1. In the Web UI, click the laptop icon next to "Sandboxes" to generate a registration code
2. On your local machine:

```bash
# Register with the code
agentserver agent connect \
  --server https://cli.example.com \
  --code <registration-code> \
  --name "My MacBook" \
  --opencode-url http://localhost:4096

# Subsequent runs auto-reconnect using saved credentials
agentserver agent connect --opencode-url http://localhost:4096
```

3. A **local** sandbox appears in the Web UI — click "Open" to access your local opencode through the browser.

**Tunnel features:** zero-config networking, auto-reconnect with backoff, binary WebSocket protocol (no base64 overhead), real-time SSE streaming, offline detection with auto-recovery.

## Configuration

See the full [Helm values](#helm-values), [environment variables](#environment-variables), and [API reference](docs/api-reference.md) below.

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

<details>
<summary><strong>Environment Variables</strong></summary>

| Variable | Description |
|----------|-------------|
| `DATABASE_URL` | PostgreSQL connection string |
| `ANTHROPIC_API_KEY` | Anthropic API key |
| `ANTHROPIC_BASE_URL` | Custom API base URL |
| `ANTHROPIC_AUTH_TOKEN` | Anthropic auth token (alternative to API key) |
| `OPENCODE_CONFIG_CONTENT` | JSON opencode config for sandbox pods |
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

</details>

## Contributing

```bash
# Backend
go run . serve --db-url "postgres://..." --backend docker

# Frontend (separate terminal)
cd web && pnpm install && pnpm dev
```

## License

[MIT](LICENSE)
