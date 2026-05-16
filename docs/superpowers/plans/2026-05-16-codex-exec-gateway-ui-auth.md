# codex-exec-gateway: Web UI + session auth — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** Bring codex-exec-gateway's user-facing API up to the same UX bar as codex-app-gateway's bearer tokens. Today `/api/codex-exec/register` and the workspace-binding endpoints trust an `X-User-Id` header from upstream middleware that isn't wired in production — anyone with cluster access can register executors as any user. Replace that with proper session-cookie auth via agentserver, plus a Web UI workspace settings panel for register/bind/unbind/list of remote executors.

**Architecture:** agentserver fronts all UI-side calls (session cookie). It POSTs/GETs/DELETEs to codex-exec-gateway's existing `/api/codex-exec/*` endpoints via internal HTTP, passing `X-Internal-Secret` (== chart `internal.apiSecret`) for auth and `X-User-Id` (the logged-in user). codex-exec-gateway gains a NEW `requireAgentserverSecret` middleware that wraps `/api/codex-exec/*` so direct external requests are rejected.

**Tech Stack:** Go 1.26 (chi, stdlib), React 19, Helm v3. No new deps.

**Spec (inline — short enough for this plan):** see PR description / commit message for the architecture diagram. ACL: register (owner/maintainer/developer of workspace), bind (owner/maintainer), unbind (owner/maintainer OR the executor's owner). Any workspace member can list bound executors and see online status.

**Working directory:** `/root/agentserver`. Working on main directly (no worktree — the previous worktree experiment showed too much drift; we're a small focused diff here).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/codexexecgateway/handlers/middleware.go` | Add `RequireAgentserverSecret` next to existing `RequireSharedSecret` |
| `internal/codexexecgateway/server.go` | Wrap `/api/codex-exec/*` routes with new middleware |
| `internal/codexexecgateway/config.go` | Add `AgentserverInternalSecret` field + env |
| `cmd/codex-exec-gateway/main.go` | No change (Config struct propagates automatically) |
| `internal/codexexecgateway/server_test.go` | New test: external request without secret → 401 |
| `internal/server/codex_executors.go` | New: 4 handlers (register, list, unbind, bind-existing) calling codex-exec-gateway internal |
| `internal/server/codex_executors_test.go` | Unit tests using httptest stub of codex-exec-gateway |
| `internal/server/server.go` | Wire 4 routes inside the protected r.Group |
| `web/src/lib/api.ts` | Append RemoteExecutor type + 4 client functions |
| `web/src/components/RemoteExecutorsPanel.tsx` | New panel matching site design tokens |
| `web/src/components/WorkspaceDetail.tsx` | Mount panel under SettingsTab |
| `deploy/helm/agentserver/templates/codex-exec-gateway.yaml` | Add `CXG_AGENTSERVER_INTERNAL_SECRET` env from `internal.apiSecret` |
| `deploy/helm/agentserver/Chart.yaml` | Bump to 0.50.4 |

Total new: 3 source + 2 test. Modified: ~8. Estimated LOC ~500.

---

## Task 1: codex-exec-gateway middleware + route wiring

**Files:**
- Modify: `internal/codexexecgateway/handlers/middleware.go`
- Modify: `internal/codexexecgateway/server.go`
- Modify: `internal/codexexecgateway/config.go`

- [ ] **Step 1: Add config field**

In `internal/codexexecgateway/config.go`, add `AgentserverInternalSecret string` to `Config` struct + load `CXG_AGENTSERVER_INTERNAL_SECRET` env. Treat empty as "no agentserver-side auth required" (dev mode), same convention as the existing `InternalSharedSecret`.

- [ ] **Step 2: Add middleware**

In `internal/codexexecgateway/handlers/middleware.go`, append:

```go
// RequireAgentserverSecret rejects requests whose X-Internal-Secret
// header does not constant-time-match `secret`. When `secret` is empty
// the middleware is a no-op (dev mode).
//
// Separate from RequireSharedSecret because the two represent different
// trust scopes:
//   - RequireSharedSecret  → cap-token admin API (called by codex-app-gateway)
//   - RequireAgentserverSecret → user-management API (called by agentserver
//     on behalf of session-authenticated humans)
func RequireAgentserverSecret(secret string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if secret == "" {
                next.ServeHTTP(w, r)
                return
            }
            got := r.Header.Get("X-Internal-Secret")
            if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
                http.Error(w, "unauthorized", http.StatusUnauthorized)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

- [ ] **Step 3: Wire middleware**

In `internal/codexexecgateway/server.go`'s `Routes()`, wrap the `/api/codex-exec/*` endpoints:

```go
r.Route("/api/codex-exec", func(r chi.Router) {
    r.Use(handlers.RequireAgentserverSecret(s.config.AgentserverInternalSecret))
    r.Post("/register", handlers.Register(s.store))
    r.Route("/workspaces/{wid}/executors", func(r chi.Router) {
        r.Post("/", handlers.PostBinding(s.store))
        r.Get("/", handlers.ListBinding(s.store))
        r.Delete("/{exe_id}", handlers.DeleteBinding(s.store))
    })
})
```

(If routes were previously registered flat, replace those registrations with this grouped form.)

- [ ] **Step 4: Test + commit**

```bash
go test ./internal/codexexecgateway/... -count=1 -v 2>&1 | tail
```
Existing tests should still pass (they pass via `X-Internal-Secret` already, or we set the test config secret to empty).

Add one new test in `server_test.go` confirming external request without `X-Internal-Secret` returns 401:

```go
func TestCodexExec_Register_RequiresInternalSecret(t *testing.T) {
    cfg := Config{AgentserverInternalSecret: "s3cret", ...}
    srv := NewServer(cfg, nil)
    req := httptest.NewRequest(http.MethodPost, "/api/codex-exec/register", strings.NewReader(`{}`))
    req.Header.Set("Content-Type", "application/json")
    rr := httptest.NewRecorder()
    srv.Routes().ServeHTTP(rr, req)
    if rr.Code != http.StatusUnauthorized { t.Fatalf("status = %d", rr.Code) }
}
```

```bash
git add internal/codexexecgateway/handlers/middleware.go \
        internal/codexexecgateway/server.go \
        internal/codexexecgateway/server_test.go \
        internal/codexexecgateway/config.go
git commit -m "feat(codex-exec-gateway): gate /api/codex-exec/* with agentserver internal secret"
```

---

## Task 2: agentserver — executors HTTP client

**Files:**
- Create: `internal/server/codex_executors_client.go`
- Create: `internal/server/codex_executors_client_test.go`

`ExecutorsClient` is a thin HTTP client around the 4 codex-exec-gateway endpoints. Always sends `X-Internal-Secret` (from `INTERNAL_API_SECRET` env) and `X-User-Id` (the session user). Methods:

```go
type ExecutorsClient struct { baseURL, secret string; http *http.Client }

type RegisterRequest struct { DisplayName, Description, DefaultCwd string }
type RegisterResponse struct { ExeID, RegistrationToken string }

type ListedExecutor struct {
    ExeID       string `json:"exe_id"`
    Description string `json:"description"`
    DefaultCwd  string `json:"default_cwd"`
    IsDefault   bool   `json:"is_default"`
    LastSeenAt  *time.Time `json:"last_seen_at,omitempty"`
}

func NewExecutorsClient(baseURL, secret string) *ExecutorsClient
func (c *ExecutorsClient) Register(ctx context.Context, userID string, req RegisterRequest) (RegisterResponse, error)
func (c *ExecutorsClient) Bind(ctx context.Context, userID, workspaceID, exeID string, isDefault bool) error
func (c *ExecutorsClient) Unbind(ctx context.Context, userID, workspaceID, exeID string) error
func (c *ExecutorsClient) List(ctx context.Context, userID, workspaceID string) ([]ListedExecutor, error)
```

Tests: httptest stub for each method, verify path + headers + body + decode.

```bash
git add internal/server/codex_executors_client.go internal/server/codex_executors_client_test.go
git commit -m "feat(server): codex-exec-gateway HTTP client (Register/Bind/Unbind/List)"
```

---

## Task 3: agentserver — user-facing handlers

**Files:**
- Create: `internal/server/codex_executors.go`
- Create: `internal/server/codex_executors_test.go`

```go
type Server struct { ... ExecutorsClient *ExecutorsClient }

// POST /api/workspaces/{wid}/executors
// body: {"display_name":"...", "description":"...", "default_cwd":"..."}
// Auth: session, requires owner/maintainer role (developers can register-only via separate endpoint? No — combine: register+bind in one shot, gated on owner/maintainer)
func (s *Server) handleRegisterExecutor(w http.ResponseWriter, r *http.Request) {
    // 1. user from auth.UserIDFromContext
    // 2. wid from chi URLParam
    // 3. check role: must be owner/maintainer
    // 4. ExecutorsClient.Register → exe_id + token
    // 5. ExecutorsClient.Bind(exe_id, wid, is_default=false)
    // 6. respond {exe_id, registration_token, connect_command}
}

// GET /api/workspaces/{wid}/executors
// Auth: any workspace member
func (s *Server) handleListExecutors(...) // ExecutorsClient.List → JSON

// DELETE /api/workspaces/{wid}/executors/{exe_id}
// Auth: owner/maintainer
func (s *Server) handleUnbindExecutor(...) // ExecutorsClient.Unbind → 204
```

The handler also computes `connect_command` for the response (so UI can show a one-liner): `codex exec-server --connect 'wss://{exec_gateway_host}:443/codex-exec/{exe_id}?token={token}'`. The host should be a config value pulled from somewhere — for now, hardcode by reading a new env `CODEX_EXEC_GATEWAY_PUBLIC_HOST` (chart sets it to `codex-exec.agent.cs.ac.cn` or `codex-exec.platform.agentserver.dev`). Empty host → omit `connect_command` from response and let UI fall back to a generic template.

Tests: each handler — happy path, 401 (no session), 403 (wrong role), 404 (no membership).

```bash
git add internal/server/codex_executors.go internal/server/codex_executors_test.go
git commit -m "feat(server): codex executors mint/list/unbind handlers"
```

---

## Task 4: Wire routes + Server struct field

**Files:**
- Modify: `internal/server/server.go`

Inside the existing `r.Group(r.Use(s.Auth.Middleware))`, add:

```go
r.Post("/api/workspaces/{wid}/executors", s.handleRegisterExecutor)
r.Get("/api/workspaces/{wid}/executors", s.handleListExecutors)
r.Delete("/api/workspaces/{wid}/executors/{exe_id}", s.handleUnbindExecutor)
```

Add `ExecutorsClient *ExecutorsClient` to `Server` struct. Construct it in `cmd/serve.go` (or wherever Server is built) reading `CODEX_EXEC_GATEWAY_INTERNAL_URL` env (new) and the existing `INTERNAL_API_SECRET`.

```bash
go vet ./... && go test ./internal/server/ -run "TestCodexExecutors" -v
git add internal/server/server.go cmd/serve.go
git commit -m "feat(server): wire codex executors routes + client construction"
```

---

## Task 5: Web UI — api client + panel

**Files:**
- Modify: `web/src/lib/api.ts`
- Create: `web/src/components/RemoteExecutorsPanel.tsx`
- Modify: `web/src/components/WorkspaceDetail.tsx`

api.ts adds:
```typescript
export interface RemoteExecutor {
  exe_id: string
  description: string
  default_cwd: string
  is_default: boolean
  last_seen_at?: string
}
export interface RegisterExecutorRequest { description: string; default_cwd?: string; display_name?: string }
export interface RegisterExecutorResponse { exe_id: string; registration_token: string; connect_command?: string }

export async function listRemoteExecutors(workspaceId: string): Promise<RemoteExecutor[]>
export async function registerRemoteExecutor(workspaceId: string, req: RegisterExecutorRequest): Promise<RegisterExecutorResponse>
export async function unbindRemoteExecutor(workspaceId: string, exeId: string): Promise<void>
```

`RemoteExecutorsPanel.tsx` follows the same pattern as `CodexTokensPanel`:
- Same outer `rounded-lg border border-[var(--border)] bg-[var(--card)]` chrome
- Title with Server icon (lucide) + label "Remote Executors"
- "Register new" button → modal with name/description/default_cwd inputs
- Generated modal: one-time display of registration_token + the full `codex exec-server --connect ...` command + copy button
- List: each row shows description, default_cwd, online-status badge (green dot if last_seen_at within last 60s, gray otherwise), unbind trash icon
- ConfirmModal for unbind

Online status: derive client-side from `last_seen_at`. Refresh every 10s while panel is mounted.

WorkspaceDetail.tsx: mount `<RemoteExecutorsPanel workspaceId={workspaceId} />` right after `<CodexTokensPanel ... />` in SettingsTab.

```bash
cd web && pnpm build  # verify type-check
cd ..
git add web/src/lib/api.ts web/src/components/RemoteExecutorsPanel.tsx web/src/components/WorkspaceDetail.tsx
git commit -m "feat(web): RemoteExecutorsPanel under workspace settings"
```

---

## Task 6: Chart wiring + bump

**Files:**
- Modify: `deploy/helm/agentserver/templates/codex-exec-gateway.yaml`
- Modify: `deploy/helm/agentserver/templates/deployment.yaml`
- Modify: `deploy/helm/agentserver/Chart.yaml`

codex-exec-gateway.yaml: add to env:
```yaml
            - name: CXG_AGENTSERVER_INTERNAL_SECRET
              value: {{ required "internal.apiSecret is required when codexExecGateway.enabled is true" .Values.internal.apiSecret | quote }}
```

deployment.yaml (agentserver main): add `CODEX_EXEC_GATEWAY_INTERNAL_URL` env pointing to `http://{release}-codex-exec-gateway.{namespace}.svc:{port}`. Wire `CODEX_EXEC_GATEWAY_PUBLIC_HOST` env from a new `codexExecGateway.publicHost` chart value (default `""`, operator sets `codex-exec.agent.cs.ac.cn`).

Chart.yaml: 0.50.3 → 0.50.4.

```bash
helm lint deploy/helm/agentserver/
git add deploy/helm/agentserver/templates/codex-exec-gateway.yaml \
        deploy/helm/agentserver/templates/deployment.yaml \
        deploy/helm/agentserver/Chart.yaml \
        deploy/helm/agentserver/values.yaml
git commit -m "chore(chart): bump 0.50.4 + wire codex-exec-gateway internal auth"
```

---

## Task 7: Tag + push + GHA + pulumi + restart pods

Same operational dance as the previous 4 deploys. Bump pulumi pin to 0.50.4 in `/root/k8s/stacks/agentserver.ts` (also set `agentserver:codexExecGatewayPublicHost = codex-exec.agent.cs.ac.cn`). After CI: pulumi up --target helm-agentserver + rollout restart agentserver + rollout restart codex-exec-gateway.

```bash
git tag v0.50.4
git push github main && git push github v0.50.4
# monitor CI...
# pulumi up
# kubectl rollout restart deployments
```

---

## Self-Review

- Spec coverage: each design decision (register+bind combined action under owner/maintainer ACL, list+status for any member, unbind+revoke for owner/maintainer or executor owner) maps to handler in Task 3.
- Placeholder scan: 1 — the `connect_command` host needs a chart value, captured in Task 6.
- Type consistency: `RemoteExecutor` TS interface mirrors `ListedExecutor` Go struct; `RegisterExecutorResponse` mirrors handler response.
- Scope: 7 tasks, one coherent feature, single PR.
