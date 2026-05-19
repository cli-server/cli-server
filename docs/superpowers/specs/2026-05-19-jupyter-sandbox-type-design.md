# Jupyter as a Sandbox Type — Design

**Date**: 2026-05-19
**Status**: approved
**Author**: mryao (with Claude)
**Replaces**: the supervisor-managed workspace-level notebook (Plan 3a–3c, shipped in chart 0.57–0.59) is removed.

## Goal

Replace the bespoke `internal/notebooksupervisor` workspace-singleton notebook with a `jupyter` value of the existing sandbox `type` enum. JupyterLab pods become first-class sandboxes, indistinguishable in lifecycle from `opencode`/`claudecode`/`openclaw`/`nanoclaw`.

## Why

The supervisor + per-workspace vhost path accumulated four bugs in one afternoon (path-vs-base_url 404, stale cache after external delete, readiness race, double-`?token=`). Each was specific to the supervisor not reusing existing infrastructure. The sandbox path already solves all four:

| Bug | Fix in sandbox path |
|---|---|
| `base_url` vs proxy prefix conflict | proxy forwards full path to pod IP — no prefix to strip |
| Stale supervisor cache | `sbxstore` reads from DB + watches pods, no in-memory ServiceURL cache |
| Readiness race | `process.Manager.Create` waits for pod ready before returning |
| Double `?token=` | sandboxproxy `/auth` always strips token from URL after cookie exchange |

Plus: pause/resume, quotas, idle timeout, namespace network policy, observability — all already work for sandboxes.

## Non-goals

- Migrating existing `.ipynb` files off the supervisor-mounted `workspace-drive` PVC. Users export/copy manually.
- Mounting workspace-drive in jupyter sandboxes. Each sandbox is isolated on its own RWO `session-data`.
- Jupyter ↔ agentserver MCP bridge (no env injection of `AGENTSERVER_URL`/`TOKEN`). The `agentserver_jupyter_ext` package stays installed but unused.
- Pause-time kernel checkpoint. Pausing loses kernel state; UI shows a tooltip.
- Multi-user shared jupyter sandbox. Each user creates their own.

## Architecture

```
agentserver (POST /api/workspaces/{wid}/sandboxes  type=jupyter)
   └─ sandbox.Manager.Create
        └─ k8s Pod in agent-ws-<short>
             image: registry.../agentserver-jupyter:main
             port: 8888
             mounts: session-data PVC at /home/agent
             env: JUPYTER_TOKEN=<ProxyToken>, NOTEBOOK_BASE_URL=/

sandbox-proxy (existing Deployment)
   hostname: jupyter-<sandboxID>.agent.cs.ac.cn  (subset of *.agent.cs.ac.cn)
   GET /auth?token=<session>:
     Auth.ValidateToken → IsWorkspaceMember →
     Set-Cookie jupyter-token (HttpOnly, Secure, no Domain) → 302 /lab
   else:
     read cookie → ValidateToken → membership →
     httputil.ReverseProxy → PodIP:8888 (full path intact)
```

No new control-plane components. No new HTTPRoute (existing `*.agent.cs.ac.cn → sandboxproxy` already covers `jupyter-*`).

## Components

### Backend code

**Image** — new `Dockerfile.jupyter`. Body identical to current `Dockerfile.notebook` except:
- `WORKDIR /home/agent` (was `/workspace`)
- Reads `NOTEBOOK_BASE_URL=/` and `JUPYTER_TOKEN=<token>` from env (existing `notebook/jupyter_server_config.py` already supports both)
- `agentserver_jupyter_ext` stays — costs nothing, may be used later

**`internal/sandbox/config.go`** — three new fields:
```go
JupyterImage            string  // env: JUPYTER_IMAGE
JupyterPort             int     // 8888
JupyterRuntimeClassName string  // env: JUPYTER_RUNTIME_CLASS
```

**`internal/sandbox/manager.go`** — one new `case "jupyter":` block in the container-spec switch (paralleling `case "claudecode"`), and one in the runtimeClass switch. ~25 lines total.

**`internal/sandboxproxy/jupyter_proxy.go`** (new, ~80 LOC) — `handleJupyterSubdomainProxy(w, r, sandboxID)` plus `exchangeJupyterToken`. Models `claudecode_proxy.go` minus tunnel/SPA/asset paths. Cookie name `jupyter-token`, scope per-subdomain (no Domain attr), `MaxAge` 7d.

**`internal/sandboxproxy/server.go`** — new `JupyterSubdomainPrefix` field; one new `if strings.HasPrefix(sub, jupyterPrefix)` branch in the subdomain middleware.

**`internal/sandboxproxy/config.go`** — `JupyterSubdomainPrefix` read from `JUPYTER_SUBDOMAIN_PREFIX` env (default `"jupyter"`).

**`cmd/sandboxproxy/main.go`** — pass the new config through.

**Type validation** — `"jupyter"` added to the type allow-list in three places:
- `internal/server/server.go:1477` (POST /api/workspaces/{wid}/sandboxes)
- `internal/server/agent_register.go:80` (agent self-registration; jupyter never self-registers but consistency is cheap)
- any other sandbox-type switch found by grep

### Removals

**Packages** — delete entirely:
- `internal/notebooksupervisor/` (supervisor.go, spawn.go, reaper.go, types.go, doc.go + tests)
- `internal/notebookjwt/` (HMAC token no longer needed)

**Files** — delete:
- `internal/server/notebook_session.go` + `_test.go`
- `internal/server/notebook_proxy.go` + `_test.go`
- `internal/server/notebook_vhost.go` + `_test.go`
- `Dockerfile.notebook`

**Edits** — remove notebook plumbing from:
- `internal/server/server.go`: drop `NotebookSupervisor`, `NotebookJWTSecret`, `NotebookHostBaseDomain`, `NotebookSubdomainPrefix` fields; the Router vhost middleware; the `/api/notebooks/{ws}/session` and `/api/notebooks/{ws}/*` routes
- `cmd/serve.go`: drop the supervisor init block (lines 303–347) and its 11 `NOTEBOOK_*` env reads
- `.github/workflows/build.yml`: rename `build-notebook` → `build-jupyter`, point at `Dockerfile.jupyter`, image name `agentserver-jupyter`

### Frontend

- `web/src/components/NotebooksPanel.tsx`: replace "Open Notebook" launcher with a button that filters the sandbox list to `type='jupyter'` (open existing) or routes to the sandbox-create dialog with type preselected.
- `web/src/lib/api.ts`: delete `createNotebookSession` + `NotebookSession`.
- Sandbox-create dialog: add `jupyter` to the type radio with a Jupyter icon and a small "Pausing this sandbox loses kernel state" note.

### Helm chart (`deploy/helm/agentserver/`)

- `values.yaml`: delete the entire `notebook:` block. Add `sandbox.jupyter: {image, runtimeClassName}` and `sandboxProxy.jupyterSubdomainPrefix: "jupyter"`.
- `templates/deployment.yaml` (agentserver container): remove the 13 `NOTEBOOK_*` env lines. Add `JUPYTER_IMAGE` and `JUPYTER_RUNTIME_CLASS` to the sandbox env block (port stays hardcoded 8888 — see Open items).
- `templates/deployment.yaml` (sandboxproxy container, separate template): add `JUPYTER_SUBDOMAIN_PREFIX`.
- `templates/httproute.yaml`: delete the `agentserver-notebook-vhost` HTTPRoute block.
- `Chart.yaml`: `0.59.x` → **`0.60.0`** (breaking).
- `README.md` / CHANGELOG: breaking-change note explaining the removed `/api/notebooks/*` endpoints.

### Pulumi (`/root/k8s/stacks/agentserver.ts`)

- Remove `notebookJwtSecret` RandomPassword.
- Remove the `notebook:` values block.
- Add `sandbox.jupyter.image: "registry.nj.cs.ac.cn/ghcr/agentserver/agentserver-jupyter:main"`.
- Remove HTTPRoute `agentserver-notebook-vhost-cn` (the 6b block). `*.agent.cs.ac.cn → sandboxproxy` route already covers `jupyter-*` as a subset.
- Chart version → `0.60.0`.

## Lifecycle (no new code)

| Operation | Endpoint | Behavior |
|---|---|---|
| Create | `POST /api/workspaces/{wid}/sandboxes` `{"type":"jupyter","name":"…"}` | `manager.Create` builds pod; returns sandbox row |
| Open | `https://jupyter-{id}.agent.cs.ac.cn/auth?token=<sess>` | sandboxproxy exchanges cookie → 302 `/lab` |
| Pause | `POST /api/sandboxes/{id}/pause` | scale 0; kernel lost; `.ipynb` preserved on session PVC |
| Resume | `POST /api/sandboxes/{id}/resume` | scale 1; waitReady; PodIP refreshed |
| Delete | `DELETE /api/sandboxes/{id}` | delete pod + PVC + row |
| Idle | `sbxstore.IdleWatcher` | auto-pause on `idle_timeout` from workspace config |
| Activity | sandboxproxy `throttledActivity` per request | already wired |

Jupyter sandboxes count against `workspaceMaxSandboxes` (default 2). If a workspace wants both an opencode and a jupyter sandbox at once, raise that quota.

## Cluster-level cleanup (run once after deploy)

```sh
# 1. Wipe any supervisor-created notebook pods/services across all workspace ns.
kubectl get deploy -A -l managed-by=agentserver -o json \
  | jq -r '.items[] | select(.metadata.name | startswith("notebook-")) | "\(.metadata.namespace)/\(.metadata.name)"' \
  | xargs -I{} sh -c 'kubectl -n $(echo {} | cut -d/ -f1) delete deploy,svc $(echo {} | cut -d/ -f2)'

# 2. Drop the dead HTTPRoute if Pulumi doesn't garbage-collect it.
kubectl -n agentserver delete httproute agentserver-notebook-vhost-cn --ignore-not-found
```

`workspace-drive` PVCs (`agent-ws-<short>-disk`) stay — they're workspace file storage, unrelated to jupyter sandboxes. Existing `.ipynb` files inside them are untouched; users open the workspace-drive from a regular shell sandbox to retrieve them.

## Migration sequence (single PR + single pulumi up)

1. agentserver PR:
   1. Add jupyter sandbox type code, `Dockerfile.jupyter`, sandboxproxy handler, chart fields.
   2. Delete supervisor / vhost / jwt packages + files + chart blocks.
   3. Frontend swap.
   4. Tests: unit for `handleJupyterSubdomainProxy` (auth exchange, missing cookie, membership check, status filters); manager case covered by existing sandbox tests + one new `TestManager_Create_Jupyter`.
   5. Chart bump 0.60.0; CHANGELOG breaking-change note.
2. CI builds `agentserver-jupyter:main` + `agentserver-jupyter:v0.60.0` + chart 0.60.0.
3. `/root/k8s` PR: chart version, values diff, delete `nb-*` HTTPRoute, remove notebookJwtSecret.
4. `pulumi up` — picks up new chart, restarts agentserver + sandboxproxy.
5. Run the cluster-level cleanup snippet above.

## Risks & rollback

- **Risk**: users with active `.ipynb` files in `workspace-drive` lose direct UI access. **Mitigation**: a regular shell sandbox can mount workspace-drive and copy/export. Call this out in the release note.
- **Risk**: 0.60.0 deploy fails. **Rollback**: re-pin Pulumi to chart 0.59.4 (which still has the supervisor + vhost path) and `pulumi up`. No data loss — sandboxes created against 0.60.0 stay, their pods just orphan briefly until cleaned up.
- **Risk**: `jupyter-*` HTTPRoute collides with sandbox-proxy wildcard differently than expected. **Mitigation**: existing `*.agent.cs.ac.cn → sandboxproxy` already handles this in production for `code-*`, `claw-*`, `claude-*`; same code path.

## Open items (decide during implementation, not blocking spec)

- Cookie `MaxAge`: 7d to match opencode. Reconsider if Jupyter token cookies prove too long-lived for security review.
- `JupyterPort` configurable vs hardcoded 8888. Default hardcoded; expose env only if a deployer asks.
- `agentserver_jupyter_ext` keep-or-cut: keep for now (zero runtime cost without env wiring).
