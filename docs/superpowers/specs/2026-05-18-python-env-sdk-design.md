# Python env SDK + hosted Jupyter — Design

**Status:** draft
**Date:** 2026-05-18
**Owner:** agentserver / developer experience
**Related:**
- [`2026-05-10-codex-gateway-mcp-rewrite.md`](2026-05-10-codex-gateway-mcp-rewrite.md) — env-mcp per-env tool routing this SDK rides on
- [`2026-05-16-env-mcp-fixed-tools-redesign.md`](2026-05-16-env-mcp-fixed-tools-redesign.md) — tool inventory the SDK wraps
- `cmd/astool/` — single-shot CLI counterpart; this spec is the same idea at scale

## Goal

Give developers a way to **write Python code instead of natural-language prompts** to operate on the same env-mcp tools the LLM uses. Surface envs as Python objects in a hosted Jupyter environment integrated into the existing agentserver web UI, with an async/await SDK and a workspace-wide operation log so every call (SDK / TUI / future LLM) is auditable.

Mental model: each connected executor (env) is a Python object; methods on that object are env-mcp tool calls. The notebook is the editor; the gateway is the dispatcher; the operation log is the history.

## Non-goals (v1)

- **Pulumi-style state engine / IaC.** No declarative resource model, no `up` / `preview` / `destroy` lifecycle, no diff-driven apply. Operations are imperative.
- **Decorators** (`@ctx.task`, `@ctx.parallel_map`, retry, cached, recipe). Pure `async/await` only for v1. Decorators can come back as v1.5 sugar if usage justifies.
- **Real-time collaboration (Jupyter RTC).** Same workspace ⇒ shared notebook files, but co-editing is by convention; no operational-transform layer.
- **Per-user kernel isolation via separate containers.** Per-workspace shared Jupyter Server, multiple kernels inside one container. Per-user attribution via env vars on kernel spawn, not container boundary.
- **Logging LLM-initiated tool calls in v1.** Already recorded in codex thread history; unified logging deferred to v1.5.
- **In-notebook cell/notebook attribution in operation log.** v1 logs (user, workspace, source). Cell-level metadata is v1.5.
- **External pip / PyPI publication of the SDK.** Pre-installed into the notebook image, version pinned per image tag.
- **Static `.pyi` stub generation for custom tools.** v1 relies on `__getattr__` + `dir()` + IPython runtime help. Static-analysis-friendly stubs deferred.

## Architecture

```
                           ┌────────────────────────────────────────────────┐
[Browser]                  │  agentserver web (existing React app)          │
                           │   ┌────────────────────────────────────────┐   │
  ┌─────────────────┐     │   │ Existing LLM chat / etc panels         │   │
  │ User session    │────►│   │                                          │   │
  └─────────────────┘     │   │ NEW: Notebooks panel                     │   │
                           │   │  → <iframe src="/api/notebooks/{ws}/…"> │   │
                           │   │                                          │   │
                           │   │ NEW: Operations panel                    │   │
                           │   └────────────────────────────────────────┘   │
                           │              │                                   │
                           │   ┌──────────▼────────────┐                      │
                           │   │ Jupyter proxy handler │  auth + per-ws       │
                           │   │ (new in agentserver)  │  routing             │
                           │   └──────────┬────────────┘                      │
                           └──────────────┼────────────────────────────────────┘
                                          │
                              ┌───────────▼──────────────┐
                              │ notebook supervisor      │  k8s Deployment per
                              │ (new agentserver pkg)    │  workspace, on-demand
                              └───────────┬──────────────┘
                                          │ EnsureRunning(workspace_id)
                              ┌───────────▼──────────────┐
                              │ jupyter-server container │  one per workspace
                              │  ┌──────────────────────┐│  multi-kernel
                              │  │ ipykernel (per-user) ││
                              │  │   ↓ ctx auto-loaded   ││
                              │  │ agentserver-sdk       ││
                              │  └──────────────────────┘│
                              └───────────┬──────────────┘
                                          │ ws (workspace token)
                              ┌───────────▼──────────────┐
                              │ codex-app-gateway        │  EXISTING
                              │  + oplog Interceptor     │  NEW thin wrapper
                              │  (decode mcpServer/tool/ │
                              │   call frames, log)      │
                              └───────────┬──────────────┘
                                          │ stdio
                              ┌───────────▼──────────────┐
                              │ codex app-server         │  EXISTING
                              │  + env-mcp child         │
                              └──────────────────────────┘
                                          │
                              ┌───────────▼──────────────┐
                              │ agentserver db           │  NEW operations table
                              │  operations              │
                              └──────────────────────────┘
```

## Components

### 1. `agentserver-sdk` (new Python package)

Source: `sdk/python/` at repo root (new top-level dir; pip-installable layout: `pyproject.toml` + `src/agentserver_sdk/`).

Surfaces:
- `Ctx` — global singleton, loaded from env vars at kernel startup, exposes envs + ops history
- `Env` — one instance per connected executor; core typed methods + dynamic dispatch for custom tools
- `Process` — async context manager over `exec_command` / `write_stdin` / `read_output` / `terminate`
- `ShellResult`, `ToolError`, etc. — result and exception types
- All methods async; designed for top-level `await` in IPython

Distribution: pre-installed into notebook image, no separate publication for v1.

### 2. `Dockerfile.notebook` (new image)

One image, parameterized by SDK version pin:

```dockerfile
FROM python:3.12-slim
RUN pip install --no-cache-dir \
      jupyter-server~=2.14 \
      jupyterlab~=4.2 \
      ipykernel~=6.29
COPY sdk/python /tmp/sdk
RUN pip install --no-cache-dir /tmp/sdk && rm -rf /tmp/sdk
RUN mkdir -p /opt/ipython-startup
COPY notebook/ipython_startup/00-ctx.py /opt/ipython-startup/
COPY notebook/jupyter_server_config.py /etc/jupyter/
ENV IPYTHONDIR=/etc/ipython JUPYTER_CONFIG_DIR=/etc/jupyter \
    PYTHONSTARTUP=/opt/ipython-startup/00-ctx.py
EXPOSE 8888
ENTRYPOINT ["jupyter", "server", "--config=/etc/jupyter/jupyter_server_config.py"]
```

SDK source lives at `sdk/python/` in the repo (new top-level dir; `cmd/` is reserved for Go binaries). Image build copies it in and installs from source — no PyPI dependency.

The startup file is one line of real work:
```python
# 00-ctx.py
from agentserver_sdk import Ctx
ctx = Ctx.from_env()
```

`jupyter_server_config.py` does:
- Disable jupyter's built-in token auth (we proxy)
- Plug `AgentserverIdentityProvider` (validates the JWT from query string / header)
- Plug `AgentserverKernelProvisioner` (per-kernel env injection)
- `base_url = /api/notebooks/<workspace>/`
- Bind to `0.0.0.0:8888`

### 3. `internal/notebooksupervisor/` (new agentserver package)

Pattern-for-pattern copy of `internal/codexappgateway/supervisor`:

| | codex supervisor | notebook supervisor |
|---|---|---|
| `Key` | `WorkspaceID` | `WorkspaceID` |
| Managed unit | `codex app-server` subprocess | k8s Deployment running jupyter image |
| Spawn trigger | First ws to `/codex-app/ws` | First `POST /api/notebooks/{ws}/session` |
| Idle reap | already in supervisor | same model, default 4h no kernel activity |
| Lifecycle command | `os.exec` fork | k8s API (apps/v1 Deployment + Service) |

`EnsureRunning(workspaceID)`:
1. If Deployment exists & healthy → return service URL
2. Else: create Deployment + Service (idempotent), wait for Ready (timeout 60s)
3. Update `last_active`

`Reap()`:
- Background loop, every 5 min
- For each workspace's deployment, query `last_active` (touched on every proxy request)
- If idle > 4h, delete Deployment + Service (PV stays)

Resource defaults per workspace deployment: CPU 2c / mem 4GB / ephemeral 5GB.

### 4. Persistence — `workspace volume`

- One PV per workspace, created by agentserver workspace provisioner (existing pattern, not new)
- Mounted at `/workspace` in notebook container
- Jupyter `c.ServerApp.root_dir = '/workspace'` → user sees workspace as root
- `.ipynb` files persist there
- Backup is storage layer concern (PV snapshot or S3 sync); no app-level versioning

### 5. Jupyter proxy (new agentserver web handler)

`/api/notebooks/{workspace}/*`:
- HTTP path: reverse-proxy to `notebook-{workspace}.<ns>.svc.cluster.local:8888`
- WebSocket path: tunnel upgrade through the proxy (jupyter kernel comm uses ws)
- Auth: verify JWT (from query string `?token=…` or `Authorization` header), populated by step 7 below
- 404 unless `EnsureRunning(workspace)` returned ready

### 6. Web UI panels (new React components)

`<NotebooksPanel />`:
```tsx
function NotebooksPanel() {
  const { data: session } = useNotebookSession();   // POST /api/notebooks/{ws}/session
  if (!session) return <Spinner />;
  return (
    <iframe
      src={`${session.url}?token=${session.token}`}
      sandbox="allow-scripts allow-same-origin allow-forms"
      className="w-full h-full"
    />
  );
}
```

Token has 10-min TTL; component refreshes before expiry. Spawn-on-first-load shows a spinner with progress text.

`<OperationsPanel />`:
- Table view, default sort `started_at desc`
- Filters: env / tool / source / time range / user / is_error
- Row → details modal with full args/result (jsonb pretty-rendered)
- Backend: `GET /api/operations?…`

### 7. Operation log

#### Schema

```sql
CREATE TABLE operations (
  id              uuid PRIMARY KEY,
  workspace_id    text NOT NULL,
  user_id         text,                       -- NULL for unattributed (LLM in v1.5)
  source          text NOT NULL,              -- 'sdk' | 'tui'  (v1.5: 'llm')
  thread_id       text,                       -- codex thread, if applicable
  request_id      text,                       -- JSON-RPC id for correlation

  env_id          text NOT NULL,
  tool            text NOT NULL,              -- 'shell', 'submit_task', …
  arguments       jsonb NOT NULL,             -- truncated if > 64KB
  arguments_meta  jsonb,                      -- {"truncated":true,"size_bytes":N,"sha256":"..."}

  is_error        boolean NOT NULL,
  result_summary  text,                       -- first 4KB of text content
  result_meta     jsonb,                      -- {"truncated":true,"total_bytes":N}

  started_at      timestamptz NOT NULL,
  completed_at    timestamptz NOT NULL,
  duration_ms     integer NOT NULL,

  -- v1.5 fields, nullable for v1
  notebook_path   text,
  cell_id         text
);

CREATE INDEX ops_ws_time   ON operations (workspace_id, started_at DESC);
CREATE INDEX ops_ws_env    ON operations (workspace_id, env_id, started_at DESC);
CREATE INDEX ops_ws_source ON operations (workspace_id, source, started_at DESC);
```

Retention: 90 days default TTL via background job; workspace-overridable.

#### Write path — gateway-side Interceptor

`codex-app-gateway`'s current `wsbridge.RunProxy` is dumb byte copy. Add a thin layer in front:

```go
// internal/codexappgateway/oplog/interceptor.go (new)
type Interceptor struct {
    logger OperationLogger   // async Submit(op)
    source string            // "sdk" or "tui", from ws path
    userID string            // from _meta on incoming frame
    workspace string
}
func (i *Interceptor) OnClientFrame(frame []byte) []byte {
    var req map[string]any
    if json.Unmarshal(frame, &req) == nil && req["method"] == "mcpServer/tool/call" {
        i.recordRequest(req["id"], req["params"])
    }
    return frame
}
func (i *Interceptor) OnServerFrame(frame []byte) []byte {
    var resp map[string]any
    if json.Unmarshal(frame, &resp) == nil {
        if id, ok := resp["id"]; ok {
            i.recordResponse(id, resp["result"], resp["error"])
        }
    }
    return frame
}
```

Pending map (`id → started_at + params`) joins request and response into one record. Parse failures or non-tool-call frames pass through with zero impact.

Logger interface allows async fire-and-forget submission. Bounded channel (1024) — if it fills, drop the log and bump a metric. **Tool calls never block on logging.**

#### Source tagging by ws path

SDK uses a separate ws entry on the gateway: **`/notebook/ws`** (vs TUI's existing `/codex-app/ws`). The Interceptor knows `source = "sdk"` because of the path. No reliance on client-supplied tags.

For v1.5 LLM logging, env-mcp itself will emit log records via the existing `/internal/connected` callback path.

#### Payload truncation

| Direction | Threshold | Behavior |
|---|---|---|
| `arguments` | > 64 KB | replace with `null`, populate `arguments_meta = {truncated, size, sha256}` |
| `result.content[].text` | > 4 KB cumulative | truncate to 4 KB into `result_summary`, populate `result_meta` |
| binary content | always | never logged, only sha256 + size |

#### SDK query API

```python
ops = await ctx.history(limit=100)
ops = await ctx.history(env="alpha", is_error=True, since="1h")
op = await ctx.history(id="op_xxx")  # one item
```

Backed by new gateway RPC `operations/list { workspace, filters }`, Bearer-auth.

## SDK shape — concrete examples

```python
# === kernel start: ctx and asyncio pre-loaded ===

# 1. List
envs = await ctx.envs()                       # [Env(alpha), Env(beta), Env(hpc-a)]
alpha = await ctx.env("alpha")
hpc   = await ctx.env("hpc-a")

# 2. Core typed methods
r = await alpha.shell("ls /tmp")              # ShellResult(stdout, stderr, exit_code)
content = await alpha.read_file("/path")      # bytes
await alpha.write_file("/path", b"...")
await alpha.apply_patch(PATCH)

# Long-running process — async context manager
async with alpha.spawn("./train.sh") as proc:
    await proc.write_stdin(b"hyperparams\n")
    chunk = await proc.read_output(timeout=10)
    # auto-terminate on exit

# Cross-env
await ctx.copy(src=(alpha, "/out.tar"), dst=(beta, "/in.tar"))

# 3. Heterogeneous capabilities — discovered at runtime
print(await hpc.tools())
# → [
#     {"name": "shell",       "kind": "core",   "description": "...", "schema": {...}},
#     {"name": "submit_task", "kind": "custom", "description": "submit HPC job", "schema": {...}},
#     ...
#   ]

job = await hpc.submit_task(script="...", resources={"gpus": 4})
# resolved via __getattr__ → env.call("submit_task", {...})

# Equivalent escape hatch:
job = await hpc.call("submit_task", {"script": "...", "resources": {"gpus": 4}})

# 4. Parallel — pure asyncio, no decorator
results = await asyncio.gather(
    alpha.shell("./test.sh"),
    beta.shell("./test.sh"),
    hpc.submit_task(script="./bench.sh"),
)

# 5. History
recent = await ctx.history(limit=20)
errs   = await ctx.history(is_error=True, since="1h")
```

### `Env` implementation outline

```python
CORE_TOOLS = {"shell", "read_file", "write_file", "apply_patch",
              "copy_path", "exec_command", "write_stdin", "read_output", "terminate"}

class Env:
    def __init__(self, name, type, tools_metadata, _client):
        self.name = name
        self.type = type
        self._tools = {t["name"]: t for t in tools_metadata}
        self._client = _client
        for tool_name, meta in self._tools.items():
            if tool_name in CORE_TOOLS:
                continue
            setattr(self, tool_name, self._make_dynamic_method(tool_name, meta))

    def _make_dynamic_method(self, name, meta):
        async def method(**kwargs):
            return await self.call(name, kwargs)
        method.__name__ = name
        method.__doc__ = meta.get("description", "")
        return method

    async def call(self, tool: str, arguments: dict | None = None):
        return await self._client.mcp_tool_call(
            server="env_mcp",
            tool=tool,
            arguments={"environment_id": self.name, **(arguments or {})},
        )

    async def shell(self, cmd: str, timeout: int | None = None) -> ShellResult:
        raw = await self.call("shell", {"command": cmd, "timeout_s": timeout})
        return ShellResult.from_mcp(raw)
    # ... read_file / write_file / apply_patch / spawn / etc
```

### Process abstraction

```python
class Process:
    def __init__(self, env, session_id): ...
    async def write_stdin(self, data: bytes): ...
    async def read_output(self, timeout: float | None = None) -> bytes: ...
    async def terminate(self): ...
    async def __aenter__(self): return self
    async def __aexit__(self, *_): await self.terminate()
```

`env.spawn(cmd)` returns a `Process`. `async with` ensures `terminate` even on exception.

### Jupyter-friendly rendering

Every return type implements `_repr_html_`:
- `Env` → table row (name / type / status / supported tool count)
- list of `Env` → full table
- `ShellResult` → exit code coloured, stdout/stderr collapsible
- `ToolError` → message + env + tool + truncated raw payload
- bytes from `read_file` → preview header + size + lazy "show all" button

## Backend dependency — env-mcp per-env capability reporting

For "heterogeneous resources" (HPC having `submit_task`, normal env not having it) to work end-to-end, **env-mcp must report tool availability per env**, not just globally.

Current state (per `internal/codexappgateway/envmcp/envmcp.go:81`): all envs see all 9 tools. The tools internally route by `environment_id` parameter. Adding a tool that only applies to HPC envs is not expressible.

Required change (separate small spec to follow this one):
- Executor self-registration includes a list of supported tool kinds (`shell`, `exec`, `hpc-submit`, …)
- env-mcp tracks `(env_id → supported_tool_kinds)` map
- New RPC `env/capabilities { env_id } → tools_metadata[]` for SDK to query
- env-mcp rejects `tools/call` if env doesn't support that tool (clearer error than current implicit failure)

**v0 staging (MVP):**
- Skip per-env capability reporting initially
- SDK uses a hard-coded core tool list, plus `env.call(name, ...)` for everything else
- HPC `submit_task` works via `await hpc.call("submit_task", {...})`
- `__getattr__` magic and `env.tools()` return a static workspace-wide list
- Promote to per-env once env-mcp lands the capability work

## Authentication flow

```
Browser                    agentserver web        notebook supervisor   jupyter-server         SDK in kernel           gateway
  │                              │                       │                     │                      │                      │
  ├─1. logged in ───────────────►│                       │                     │                      │                      │
  ├─2. open Notebooks panel ────►│                       │                     │                      │                      │
  │                              ├─3. POST /api/.../session►                   │                      │                      │
  │                              │  (verify user ∈ ws)   │                     │                      │                      │
  │                              │                       │                     │                      │                      │
  │                              │                       ├─4. EnsureRunning(ws)─────►                  │                      │
  │                              │                       │                     │                      │                      │
  │                              │◄──5. ready ───────────┤                     │                      │                      │
  │                              │                                              │                      │                      │
  │                              │ 6. mint JWT           │                     │                      │                      │
  │                              │   (user_id, ws_id,     │                     │                      │                      │
  │                              │    exp=10min,          │                     │                      │                      │
  │                              │    scope=notebook)     │                     │                      │                      │
  │                              │                                              │                      │                      │
  │◄─7. {url, token} ────────────┤                                              │                      │                      │
  │                                                                              │                      │                      │
  ├─8. iframe → jupyter ──────────────────────────────────────────────────────►│                      │                      │
  │                                                                              │ 9. IdentityProvider │                      │
  │                                                                              │    verify JWT       │                      │
  │                                                                              │    user_id ∈ session│                      │
  │                                                                              │                      │                      │
  ├─10. start kernel ──────────────────────────────────────────────────────────►│                      │                      │
  │                                                                              │ 11. KernelProvisioner│                      │
  │                                                                              │   ENV={              │                      │
  │                                                                              │     AGENTSERVER_USER_ID,
  │                                                                              │     AGENTSERVER_WORKSPACE_TOKEN,
  │                                                                              │     AGENTSERVER_GATEWAY_URL,
  │                                                                              │   }                  │                      │
  │                                                                              │ ──spawn ipykernel──►│                      │
  │                                                                              │                     │ 12. ctx=Ctx.from_env │
  │                                                                              │                     │  read ENV, build WS  │
  │                                                                              │                     │                      │
  ├─13. cell `await alpha.shell(...)`──────────────────────────────────────────────────────────────►│                      │
  │                                                                              │                     ├─14. ws.send({       │
  │                                                                              │                     │  method:"mcpServer/  │
  │                                                                              │                     │    tool/call",       │
  │                                                                              │                     │  params:{            │
  │                                                                              │                     │    _meta:{           │
  │                                                                              │                     │      user_id:…       │
  │                                                                              │                     │    }} })             │
  │                                                                              │                     │ ──────────────────►│
  │                                                                              │                     │                     │ 15. Interceptor logs
  │                                                                              │                     │                     │   (source=sdk,user)
```

### Token model

| Token | Lifetime | Scope | Where stored | Used for |
|---|---|---|---|---|
| Browser → jupyter iframe JWT | 10 min, refreshed | (user, workspace, notebook scope) | Web UI memory + query string | jupyter `IdentityProvider` validation |
| `AGENTSERVER_WORKSPACE_TOKEN` | workspace lifetime (rotatable) | workspace | Env var on jupyter container | Kernel → gateway WS Bearer auth |
| `AGENTSERVER_USER_ID` | per-kernel | not auth, attribution only | Env var injected per-kernel | SDK includes in `_meta.user_id` for log attribution |

**Key simplification:** `AGENTSERVER_USER_ID` is **not used for auth**. Gateway already trusts the workspace token (which proves the kernel was spawned by agentserver for that workspace). User ID is metadata only. This avoids per-call signing.

## Network topology

| Hop | URL |
|---|---|
| Browser → agentserver web | existing `https://agentserver/...` |
| Browser ↔ jupyter (iframe) | `https://agentserver/api/notebooks/{ws}/...` (reverse proxy) |
| agentserver web → notebook supervisor | in-process (Go interface call) |
| notebook supervisor → k8s | k8s API (apps/v1, core/v1) |
| jupyter container service | `notebook-{ws}.<ns>.svc.cluster.local:8888` |
| jupyter kernel SDK → gateway | `ws://codex-app-gateway.<ns>.svc.cluster.local:8086/notebook/ws` |
| gateway → codex app-server | existing (codex stdio) |
| gateway → agentserver db (oplog) | existing PG connection pool |

## Failure modes & mitigations

| Failure | Mitigation |
|---|---|
| Notebook container slow to start | 60s timeout in `EnsureRunning`; web UI shows progress; user can retry |
| Gateway oplog DB write slow | Bounded channel, fire-and-forget; never blocks tool call; metric on drop |
| Gateway oplog DB down | Same; system continues serving tool calls; alerting on metric |
| Huge `write_file` payload (e.g. 1 GB binary) | Interceptor inspects frame header only; over threshold logs size+sha256, not bytes |
| Shell loop spams oplog | App-layer sampling toggle (off in v1); DB partitioned by month |
| User session expires during long cell | JWT refresh while iframe is open; in-cell ws connection uses `AGENTSERVER_WORKSPACE_TOKEN` (long-lived), so cell completes |
| Workspace volume PV full | Standard k8s quota; agentserver shows warning on workspace status |
| Custom tool with unknown schema | `env.call(tool, args)` works regardless; SDK doesn't validate schema (env-mcp does) |
| Two users edit same `.ipynb` | Conflict on save (last writer wins for v1); RTC deferred to v1.5 |
| Idle reaper kills container while user inactive | PV persists; next `EnsureRunning` brings it back; kernel state is lost (expected) |

## Out-of-scope items intentionally deferred

1. **State engine / IaC layer** — see "Why no Pulumi" appendix below.
2. **`@ctx.task` / `@ctx.parallel_map` / `@ctx.retry` decorators** — pure async only in v1.
3. **LLM call logging** — codex thread history already has it; unify v1.5.
4. **Real-time collaborative editing** — Jupyter RTC + Y.js.
5. **External SDK distribution** (PyPI / mirror) — version-pinned in image.
6. **Static-analysis `.pyi` stubs for dynamic tools** — runtime help only in v1.
7. **Per-user container isolation** — per-workspace shared by design.
8. **Notebook templates / starter examples** — separate spec.
9. **Replay button on operations panel** — list/read only in v1.

## Why no Pulumi-style state engine (decision record)

State engine surface (state storage + diff + apply + per-resource providers + lock + drift + replace semantics + multi-stack + secrets) is **5-8 person-weeks for an MVP, 3-6 months for production-ready**. Marginal value over imperative SDK in this domain is limited because:

1. env-mcp surface is small (file, dir, process, shell). State machinery cost is the same regardless of resource count.
2. Executors are often ephemeral; drift detection's value scales with infra longevity.
3. `env.shell("./install.sh")` is opaque to diff; the main IaC value-add (diff preview) doesn't apply to most operations.
4. Most env-mcp tools are already idempotent at the tool level (`write_file`, `apply_patch`); the safety case for re-runs is weaker.
5. Audit/reproducibility — the other big IaC story — is already covered by the operation log at ~10% of state-engine cost.

Door left open: SDK methods are atomic with clear inputs/outputs. A future state-engine layer can wrap them as resources without restructuring.

## Implementation phases

**Phase 0 — backend prep**
- env-mcp per-env capability reporting (separate spec)
- New `operations` table migration
- Gateway `/notebook/ws` route + Interceptor skeleton (no DB write yet, just logs to stderr to validate parsing)

**Phase 1 — SDK + jupyter image**
- `sdk/python/` with `Ctx`, `Env`, `Process`, types, `_repr_html_` for jupyter
- `Dockerfile.notebook` + ipython startup
- Local docker-compose smoke test: spin jupyter against a stub gateway, validate `await ctx.envs()` round-trip

**Phase 2 — notebook supervisor + proxy**
- `internal/notebooksupervisor/` (k8s spawner, idle reap)
- Jupyter proxy handler in agentserver web
- JWT minting + jupyter IdentityProvider + KernelProvisioner

**Phase 3 — Web UI**
- `<NotebooksPanel />` iframe + session lifecycle
- `<OperationsPanel />` with filters

**Phase 4 — Operation log write path**
- Wire Interceptor → agentserver DB
- `operations/list` RPC + SDK `ctx.history(...)`
- Payload truncation + bounded channel + metrics

**Phase 5 — Hardening & release**
- Token refresh edge cases
- Idle reap thresholds tuning
- Resource quota documentation
- e2e: real user opens panel, runs notebook against real env-mcp + HPC executor

## Open questions

1. **Storage class for workspace PV** — RWO or RWX? Multiple users in same workspace open jupyter concurrently → RWX needed unless we accept "one notebook editor at a time per workspace".
2. **Per-workspace cost accounting** — notebook container is non-trivial cost; should this surface in workspace billing/quota UI?
3. **What happens to running cells when idle reaper fires** — cells executing right when reap criteria flips. Need a graceful shutdown signal that lets running ops finish (or aborts them cleanly) before container kill.
4. **Token rotation for `AGENTSERVER_WORKSPACE_TOKEN`** — currently "rotatable" but no concrete rotation flow specified. Likely needs a kernel restart to pick up new token; deferred.
5. **Schema validation in SDK** — should `env.submit_task(...)` validate kwargs against `tools/list` schema client-side, or always round-trip to env-mcp for validation? v1 is round-trip (simpler); v1.5 could add client validation.

## Success criteria

A developer can:
1. Open the Notebooks panel in the web UI within 10 seconds of clicking
2. Run `await ctx.envs()` and see their workspace's connected envs as Python objects
3. Run `await alpha.shell("uname -a")` and get a `ShellResult` rendered as a Jupyter cell output
4. Run `await hpc.submit_task(script="...")` without the SDK having been compiled with knowledge of HPC
5. Open the Operations panel and see every call from step 3 and 4 logged, attributable to them, with arguments and timing
6. Save the notebook; close the panel; reopen tomorrow; reload the notebook; pick up where they left off (kernel state likely lost, file state preserved)
