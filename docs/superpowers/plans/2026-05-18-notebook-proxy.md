# Notebook Proxy + JWT Implementation Plan (Plan 3b)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Web UI iframe can reach the per-workspace Jupyter Server via `/api/notebooks/{ws}/*` on agentserver web, authenticated by a short-lived JWT minted by `/api/notebooks/{ws}/session`. Jupyter inside the pod sees an `X-Forwarded-User` header set by the proxy and binds it onto each kernel via a custom `KernelProvisioner` so SDK calls carry per-user attribution.

**Architecture:** Three layers. (1) **`internal/notebookjwt/`** — mint/verify HMAC-SHA256 JWT-shape tokens (same pattern as `captoken.go`), 10-min TTL, claims = `{user_id, workspace_id, exp}`. (2) **agentserver web** — `POST /api/notebooks/{ws}/session` mints token + calls `Plan 3a`'s `Supervisor.EnsureRunning`, returns `{url, token}`. `* /api/notebooks/{ws}/*` reverse proxies (HTTP + WS) to the supervisor's `ServiceURL`, validating the JWT on every request, injecting `X-Forwarded-User` + Touch'ing the supervisor on each request. (3) **Jupyter image** — custom `KernelProvisioner` that reads `X-Forwarded-User` from the kernel-start request context and sets `AGENTSERVER_USER_ID` in the kernel env so the SDK can attribute calls.

**Tech Stack:** Go 1.26 (HMAC tokens, std `net/http/httputil.ReverseProxy`, `nhooyr.io/websocket` for WS upgrade) · Python 3.12 (`jupyter_server.auth.IdentityProvider`, `jupyter_client.provisioning.LocalProvisioner`) · helm chart · existing notebook image from Plan 1.

**Out of scope:**
- React `<NotebooksPanel />` (Plan 3c)
- `<OperationsPanel />` (Plan 3c)
- Multiple users editing the same `.ipynb` (Jupyter RTC) — Plan 4
- LLM-initiated tool-call logging in env-mcp (v1.5)

---

## File Structure

```
internal/notebookjwt/                          # NEW
├── doc.go
├── jwt.go                                     # Mint + Verify (HMAC-SHA256, JWT-shape)
└── jwt_test.go

internal/server/
├── notebook_session.go                        # POST /api/notebooks/{ws}/session
├── notebook_session_test.go
├── notebook_proxy.go                          # HTTP+WS proxy /api/notebooks/{ws}/*
└── notebook_proxy_test.go

internal/server/server.go                      # MODIFIED: NotebookJWTSecret + route wiring

notebook/identity_provider.py                  # NEW: trust X-Forwarded-User from proxy
notebook/kernel_provisioner.py                 # NEW: inject AGENTSERVER_USER_ID per kernel
notebook/jupyter_server_config.py              # MODIFIED: plug provider + provisioner + base_url
Dockerfile.notebook                            # MODIFIED: COPY new Python files

internal/notebooksupervisor/                   # MODIFIED (small Plan 3a amendment)
├── types.go                                   # add Config.ExtraEnvVars map[string]string
└── spawn.go                                   # apply env vars to container

deploy/helm/agentserver/
├── values.yaml                                # MODIFIED: notebook.jwtSecret
└── templates/deployment.yaml                  # MODIFIED: NOTEBOOK_JWT_SECRET env
```

---

## Design Decisions

**1. JWT = HMAC-SHA256 over JSON claims, JWT-shape encoding.** Same on-the-wire format as `internal/codexappgateway/captoken.go` but with claims `{user_id, workspace_id, exp}` and `typ: "AS-NOTEBOOK"`. No `iat` (TTL is short enough). No external JWT lib — repo already does this pattern; consistency wins.

**2. Token lives in the URL query string `?token=...`** for the iframe load. Reverse proxy reads it from query OR `Authorization: Bearer …` header (for fetch from inside the iframe). 10-minute TTL; web UI refreshes before expiry.

**3. Agentserver web proxy is the trust boundary.** Inside Jupyter we trust `X-Forwarded-User` set by the proxy. The proxy validates the JWT once per request and writes the header. Jupyter's custom `IdentityProvider` reads `X-Forwarded-User` and constructs an `User` object — no JWT re-validation inside the pod. Keeps the pod stateless w.r.t. our key.

**4. Per-kernel user attribution via custom `KernelProvisioner`.** When jupyter receives a `POST /api/kernels` call, it spawns a kernel. Our subclass of `LocalProvisioner` reads `X-Forwarded-User` from the request context (via a `ContextVar` set by `IdentityProvider`) and stamps `AGENTSERVER_USER_ID=<user>` into the kernel's `os.environ` before launch. The SDK in the kernel then includes that in `_meta.agentserver_user_id` on every gateway call.

**5. WebSocket proxy reuses standard library `httputil.ReverseProxy`.** Jupyter kernel comm is WS. Go's `httputil.ReverseProxy` since 1.20+ supports WS via `r.Header.Get("Upgrade") == "websocket"`. No custom hijacking required — std behavior works for client-initiated upgrades. We add `X-Forwarded-User` to the request before proxy fires.

**6. Plan 3a amendment.** Add `Config.ExtraEnvVars map[string]string` to the supervisor. Plan 3b sets `NOTEBOOK_BASE_URL=/api/notebooks/{workspace_id}/` so Jupyter's `c.ServerApp.base_url` reads the right value. Substitution rules same as `WorkspacePVCName`.

**7. JWT secret in helm.** `notebook.jwtSecret` value, can be auto-generated by `helm install` with `randAlphaNum 64` on first install (helm trick: `lookup` against the deployed Secret and reuse if present). Bound to deployment.yaml env `NOTEBOOK_JWT_SECRET`.

**8. Session endpoint trust.** `POST /api/notebooks/{ws}/session` is behind existing user-auth (cookie/session) so the workspace_id + user_id come from verified server-side identity. The body has no user-supplied identity.

**9. WS upgrade through chi router.** Chi's `r.Handle("/api/notebooks/{ws}/*", proxy)` works for both HTTP and WS upgrades — chi's middleware doesn't break upgrades. Confirmed via the existing `internal/codexappgateway/server.go:219` (chi + ws via nhooyr).

**10. Idempotent EnsureRunning is critical.** Session endpoint is hit on every iframe open; supervisor must be tolerant of repeat calls within the same workspace. (Plan 3a already covers this.)

---

## Task 1: `notebookjwt` package

**Files:**
- Create: `internal/notebookjwt/doc.go`
- Create: `internal/notebookjwt/jwt.go`
- Create: `internal/notebookjwt/jwt_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/notebookjwt/jwt_test.go`:

```go
package notebookjwt

import (
	"strings"
	"testing"
	"time"
)

func TestMintVerifyRoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	tok, err := Mint(secret, "u-1", "ws-1", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	c, err := Verify(secret, tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.UserID != "u-1" || c.WorkspaceID != "ws-1" {
		t.Errorf("claims=%+v", c)
	}
	if c.Exp < time.Now().Unix() {
		t.Errorf("exp in past: %d", c.Exp)
	}
}

func TestVerify_ExpiredRejected(t *testing.T) {
	secret := []byte("s")
	tok, err := Mint(secret, "u", "w", -time.Second) // already expired
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(secret, tok); err == nil {
		t.Fatal("expected expired error")
	}
}

func TestVerify_TamperedRejected(t *testing.T) {
	secret := []byte("s")
	tok, _ := Mint(secret, "u", "w", time.Minute)
	// Mutate one char of the signature segment
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("parts=%d", len(parts))
	}
	bad := parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))
	if _, err := Verify(secret, bad); err == nil {
		t.Fatal("expected tampered error")
	}
}

func TestVerify_WrongSecretRejected(t *testing.T) {
	tok, _ := Mint([]byte("right"), "u", "w", time.Minute)
	if _, err := Verify([]byte("wrong"), tok); err == nil {
		t.Fatal("expected wrong-secret error")
	}
}

func TestMint_EmptyArgs(t *testing.T) {
	if _, err := Mint(nil, "u", "w", time.Minute); err == nil {
		t.Error("nil secret should error")
	}
	if _, err := Mint([]byte("s"), "", "w", time.Minute); err == nil {
		t.Error("empty user_id should error")
	}
	if _, err := Mint([]byte("s"), "u", "", time.Minute); err == nil {
		t.Error("empty workspace_id should error")
	}
}
```

- [ ] **Step 2: Run, confirm failure**

```bash
cd /root/agentserver
go test ./internal/notebookjwt -v
```
Expected: FAIL — package missing.

- [ ] **Step 3: Implement**

Create `internal/notebookjwt/doc.go`:

```go
// Package notebookjwt mints + verifies short-lived HMAC-SHA256 tokens
// for the agentserver web notebook proxy. Claims: user_id, workspace_id,
// exp. Format mirrors internal/codexappgateway/captoken.go (JWT-shape,
// base64url-no-pad).
package notebookjwt
```

Create `internal/notebookjwt/jwt.go`:

```go
package notebookjwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Claims are what callers get back from Verify.
type Claims struct {
	UserID      string `json:"user_id"`
	WorkspaceID string `json:"workspace_id"`
	Exp         int64  `json:"exp"`
}

const header = `{"alg":"HS256","typ":"AS-NOTEBOOK"}`

// Mint produces a token valid for ttl from now.
func Mint(secret []byte, userID, workspaceID string, ttl time.Duration) (string, error) {
	if len(secret) == 0 {
		return "", fmt.Errorf("notebookjwt: empty secret")
	}
	if userID == "" || workspaceID == "" {
		return "", fmt.Errorf("notebookjwt: user_id/workspace_id required")
	}
	c := Claims{
		UserID:      userID,
		WorkspaceID: workspaceID,
		Exp:         time.Now().Add(ttl).Unix(),
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("notebookjwt: marshal: %w", err)
	}
	enc := base64.RawURLEncoding
	headerB64 := enc.EncodeToString([]byte(header))
	bodyB64 := enc.EncodeToString(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(headerB64 + "." + bodyB64))
	return headerB64 + "." + bodyB64 + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// Verify parses, checks signature + expiry, returns claims.
func Verify(secret []byte, tok string) (*Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("notebookjwt: malformed token")
	}
	enc := base64.RawURLEncoding
	wantSig, err := enc.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("notebookjwt: sig decode: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(mac.Sum(nil), wantSig) {
		return nil, fmt.Errorf("notebookjwt: signature mismatch")
	}
	bodyBytes, err := enc.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("notebookjwt: body decode: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(bodyBytes, &c); err != nil {
		return nil, fmt.Errorf("notebookjwt: body parse: %w", err)
	}
	if c.Exp < time.Now().Unix() {
		return nil, fmt.Errorf("notebookjwt: token expired (exp=%d, now=%d)", c.Exp, time.Now().Unix())
	}
	if c.UserID == "" || c.WorkspaceID == "" {
		return nil, fmt.Errorf("notebookjwt: missing required claims")
	}
	return &c, nil
}
```

- [ ] **Step 4: Run, confirm pass**

```bash
cd /root/agentserver
go vet ./internal/notebookjwt
go test ./internal/notebookjwt -v
```
Expected: 5 pass.

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/notebookjwt/
git commit -m "feat(notebookjwt): mint + verify HMAC-SHA256 tokens

mirrors internal/codexappgateway/captoken.go format with notebook claims
(user_id, workspace_id, exp). Used by the notebook proxy in P3b.4."
```

---

## Task 2: Plan 3a amendment — `Config.ExtraEnvVars`

**Files:**
- Modify: `internal/notebooksupervisor/types.go` — add field
- Modify: `internal/notebooksupervisor/spawn.go` — inject env into container
- Modify: `internal/notebooksupervisor/spawn_test.go` — add test

- [ ] **Step 1: Write failing test**

Append to `internal/notebooksupervisor/spawn_test.go`:

```go
func TestBuildDeployment_ExtraEnvVarsWithSubstitution(t *testing.T) {
	c := Config{
		Image:            "img:tag",
		WorkspacePVCName: "pvc",
		ExtraEnvVars: map[string]string{
			"NOTEBOOK_BASE_URL": "/api/notebooks/{workspace_id}/",
			"STATIC_VAR":        "literal",
		},
	}.WithDefaults()
	k := Key{WorkspaceID: "alpha", Namespace: "ns"}

	d, err := BuildDeployment(k, c)
	if err != nil {
		t.Fatal(err)
	}
	env := d.Spec.Template.Spec.Containers[0].Env
	got := map[string]string{}
	for _, e := range env {
		got[e.Name] = e.Value
	}
	if got["NOTEBOOK_BASE_URL"] != "/api/notebooks/alpha/" {
		t.Errorf("base_url=%q", got["NOTEBOOK_BASE_URL"])
	}
	if got["STATIC_VAR"] != "literal" {
		t.Errorf("static=%q", got["STATIC_VAR"])
	}
}
```

- [ ] **Step 2: Run, confirm failure**

```bash
cd /root/agentserver
go test ./internal/notebooksupervisor -run ExtraEnvVars -v
```
Expected: FAIL — `Config.ExtraEnvVars` undefined.

- [ ] **Step 3: Add the field**

Edit `internal/notebooksupervisor/types.go`. Add to `Config` struct (anywhere; before `ReadyTimeout` is a logical place):

```go
// ExtraEnvVars are injected into the notebook container env. Values
// may contain `{workspace_id}` which BuildDeployment substitutes with
// the Key.WorkspaceID. Keys are passed through as-is.
ExtraEnvVars map[string]string
```

No change to `WithDefaults` (nil map is acceptable; range over nil is empty).

- [ ] **Step 4: Inject in BuildDeployment**

Edit `internal/notebooksupervisor/spawn.go`. In `BuildDeployment`, after the existing `pvcName := strings.ReplaceAll(...)` line and before constructing the container, add:

```go
envVars := []corev1.EnvVar{}
for k2, v := range c.ExtraEnvVars {
	envVars = append(envVars, corev1.EnvVar{
		Name:  k2,
		Value: strings.ReplaceAll(v, "{workspace_id}", k.WorkspaceID),
	})
}
```

And update the container to include `Env: envVars`:

```go
container := corev1.Container{
	Name:            "notebook",
	Image:           c.Image,
	ImagePullPolicy: corev1.PullPolicy(c.ImagePullPolicy),
	Env:             envVars,
	Ports:           []corev1.ContainerPort{ /* unchanged */ },
	// ...
}
```

- [ ] **Step 5: Run, confirm pass**

```bash
cd /root/agentserver
go vet ./internal/notebooksupervisor
go test ./internal/notebooksupervisor -v
```
Expected: 25 pass (was 24 + 1 new).

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add internal/notebooksupervisor/types.go internal/notebooksupervisor/spawn.go internal/notebooksupervisor/spawn_test.go
git commit -m "feat(notebooksupervisor): Config.ExtraEnvVars with {workspace_id}

env vars piped into the notebook container with the same template
substitution as WorkspacePVCName. Used by Plan 3b to set
NOTEBOOK_BASE_URL per workspace."
```

---

## Task 3: `POST /api/notebooks/{ws}/session`

**Files:**
- Create: `internal/server/notebook_session.go`
- Create: `internal/server/notebook_session_test.go`
- Modify: `internal/server/server.go` — add `NotebookJWTSecret []byte` field

- [ ] **Step 1: Inspect existing auth helpers**

```bash
cd /root/agentserver
grep -rn "func.*requireUser\|userFromContext\|AuthMiddleware\|withUser\|getUserID" internal/server/ 2>/dev/null | head -10
grep -rn "WorkspaceMember\|workspace.*role\|user.*workspace" internal/server/ 2>/dev/null | head -10
```
Identify:
- The existing pattern to get the current user from the request (cookie/session middleware)
- The existing helper to verify a user is a member of a workspace

Use these. Don't invent.

- [ ] **Step 2: Write failing test**

Create `internal/server/notebook_session_test.go`:

```go
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostNotebookSession_ReturnsURLAndToken(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	// stub: pretend user "u-1" is a member of workspace "ws-1"
	s.SetTestUserWorkspaceMembership("u-1", "ws-1")
	s.NotebookJWTSecret = []byte("test-secret")

	// (Test scaffold: this test assumes there's a way to mock the
	// notebook supervisor's EnsureRunning to avoid real k8s. If
	// `*Server` has a supervisor interface field that can be swapped
	// for a stub, use that. Otherwise, gate this test on a "supervisor
	// nil" branch — the handler should return 503 in that case, and
	// the next test below covers it.)

	req := httptest.NewRequest(http.MethodPost, "/api/notebooks/ws-1/session", nil)
	authedReq := s.AuthenticatedRequestForTests(req, "u-1")
	rr := httptest.NewRecorder()
	s.RouterForTests().ServeHTTP(rr, authedReq)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		URL   string `json:"url"`
		Token string `json:"token"`
		ExpiresAt int64 `json:"expires_at"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.URL == "" || resp.Token == "" || resp.ExpiresAt == 0 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestPostNotebookSession_NonMemberRejected(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	// Do NOT register membership
	s.NotebookJWTSecret = []byte("s")

	req := httptest.NewRequest(http.MethodPost, "/api/notebooks/ws-1/session", nil)
	authedReq := s.AuthenticatedRequestForTests(req, "u-1")
	rr := httptest.NewRecorder()
	s.RouterForTests().ServeHTTP(rr, authedReq)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status=%d, want 403", rr.Code)
	}
}

func TestPostNotebookSession_NoJWTSecret_Returns503(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	s.SetTestUserWorkspaceMembership("u-1", "ws-1")
	s.NotebookJWTSecret = nil

	req := httptest.NewRequest(http.MethodPost, "/api/notebooks/ws-1/session", nil)
	authedReq := s.AuthenticatedRequestForTests(req, "u-1")
	rr := httptest.NewRecorder()
	s.RouterForTests().ServeHTTP(rr, authedReq)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", rr.Code)
	}
}
```

If the helpers `SetTestUserWorkspaceMembership` / `AuthenticatedRequestForTests` / `RouterForTests` don't exist, write the minimal equivalents per the existing test conventions you discovered in Step 1. The names are illustrative; adopt repo names.

- [ ] **Step 3: Implement handler**

Create `internal/server/notebook_session.go`:

```go
package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/notebookjwt"
	"github.com/agentserver/agentserver/internal/notebooksupervisor"
)

const notebookSessionTTL = 10 * time.Minute

// postNotebookSession is POST /api/notebooks/{ws}/session.
// Auth: existing user session (cookie/session middleware).
// Body: none (workspace from URL, user from session).
// Response: {url, token, expires_at}.
func (s *Server) postNotebookSession(w http.ResponseWriter, r *http.Request) {
	if len(s.NotebookJWTSecret) == 0 {
		http.Error(w, "notebook feature disabled (no JWT secret configured)", http.StatusServiceUnavailable)
		return
	}
	if s.NotebookSupervisor == nil {
		http.Error(w, "notebook supervisor unavailable", http.StatusServiceUnavailable)
		return
	}

	userID := s.userIDFromRequest(r) // use whatever helper exists for current-user
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wsID := chi.URLParam(r, "ws")
	if wsID == "" {
		http.Error(w, "workspace id required", http.StatusBadRequest)
		return
	}

	// Authorize: user must be a member of the workspace.
	ok, err := s.isUserWorkspaceMember(r.Context(), userID, wsID)
	if err != nil {
		http.Error(w, "membership check failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Look up the workspace's namespace.
	ns, err := s.workspaceNamespace(r.Context(), wsID)
	if err != nil {
		http.Error(w, "workspace namespace lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Spawn (or get-existing) the notebook pod.
	k := notebooksupervisor.Key{WorkspaceID: wsID, Namespace: ns}
	if _, err := s.NotebookSupervisor.EnsureRunning(r.Context(), k); err != nil {
		http.Error(w, "ensure notebook: "+err.Error(), http.StatusInternalServerError)
		return
	}

	tok, err := notebookjwt.Mint(s.NotebookJWTSecret, userID, wsID, notebookSessionTTL)
	if err != nil {
		http.Error(w, "mint jwt: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// URL the iframe loads. Relative to agentserver web origin; the
	// proxy route mounted at /api/notebooks/{ws}/ handles forwarding.
	url := "/api/notebooks/" + wsID + "/lab"
	resp := map[string]any{
		"url":        url,
		"token":      tok,
		"expires_at": time.Now().Add(notebookSessionTTL).Unix(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
```

Note: `s.userIDFromRequest` / `s.isUserWorkspaceMember` / `s.workspaceNamespace` are placeholder names. Use whatever already exists; if a method doesn't exist, the implementer should look at where similar checks are done elsewhere in `internal/server/`.

- [ ] **Step 4: Add field + route**

Edit `internal/server/server.go`. Add to `*Server` struct:

```go
NotebookJWTSecret []byte // empty disables /api/notebooks/* routes
```

Register the route in the chi router (find where `/api/...` routes are mounted):

```go
r.Post("/api/notebooks/{ws}/session", s.postNotebookSession)
```

- [ ] **Step 5: Run, confirm pass**

```bash
cd /root/agentserver
go vet ./internal/server
go test ./internal/server -run TestPostNotebookSession -v
```
Expected: 3 pass.

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add internal/server/notebook_session.go \
        internal/server/notebook_session_test.go \
        internal/server/server.go
git commit -m "feat(server): POST /api/notebooks/{ws}/session

mints 10-min JWT scoped to (user, workspace); calls Supervisor.EnsureRunning;
returns {url, token, expires_at}. Membership-checked; 503 if feature off."
```

---

## Task 4: HTTP + WS reverse proxy `/api/notebooks/{ws}/*`

**Files:**
- Create: `internal/server/notebook_proxy.go`
- Create: `internal/server/notebook_proxy_test.go`
- Modify: `internal/server/server.go` — register route

- [ ] **Step 1: Write failing tests**

Create `internal/server/notebook_proxy_test.go`:

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/notebookjwt"
)

func TestNotebookProxy_HTTPForwardsWithUserHeader(t *testing.T) {
	// Stub upstream "jupyter"
	var gotUser, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-Forwarded-User")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	s := newTestServer(t)
	defer s.Close()
	s.NotebookJWTSecret = []byte("s")
	s.SetTestNotebookHandle("ws-1", upstream.URL)  // returns ServiceURL=upstream from supervisor stub

	tok, _ := notebookjwt.Mint(s.NotebookJWTSecret, "u-1", "ws-1", time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/api/notebooks/ws-1/lab?token="+url.QueryEscape(tok), nil)
	rr := httptest.NewRecorder()
	s.RouterForTests().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Errorf("body=%q", rr.Body.String())
	}
	if gotUser != "u-1" {
		t.Errorf("X-Forwarded-User=%q", gotUser)
	}
	if gotPath != "/lab" {
		t.Errorf("path=%q (should strip /api/notebooks/{ws})", gotPath)
	}
}

func TestNotebookProxy_MissingTokenRejected(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	s.NotebookJWTSecret = []byte("s")
	s.SetTestNotebookHandle("ws-1", "http://stub")

	req := httptest.NewRequest(http.MethodGet, "/api/notebooks/ws-1/lab", nil)
	rr := httptest.NewRecorder()
	s.RouterForTests().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}

func TestNotebookProxy_BadTokenRejected(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	s.NotebookJWTSecret = []byte("s")
	s.SetTestNotebookHandle("ws-1", "http://stub")

	req := httptest.NewRequest(http.MethodGet, "/api/notebooks/ws-1/lab?token=garbage", nil)
	rr := httptest.NewRecorder()
	s.RouterForTests().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}

func TestNotebookProxy_WrongWorkspaceRejected(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	s.NotebookJWTSecret = []byte("s")
	s.SetTestNotebookHandle("ws-1", "http://stub")

	tok, _ := notebookjwt.Mint(s.NotebookJWTSecret, "u-1", "OTHER-ws", time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/api/notebooks/ws-1/lab?token="+url.QueryEscape(tok), nil)
	rr := httptest.NewRecorder()
	s.RouterForTests().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status=%d, want 403 (JWT workspace mismatches URL)", rr.Code)
	}
}

func TestNotebookProxy_AuthorizationHeaderAccepted(t *testing.T) {
	var gotUser string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-Forwarded-User")
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	s := newTestServer(t)
	defer s.Close()
	s.NotebookJWTSecret = []byte("s")
	s.SetTestNotebookHandle("ws-1", upstream.URL)

	tok, _ := notebookjwt.Mint(s.NotebookJWTSecret, "u-bearer", "ws-1", time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/api/notebooks/ws-1/api/status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.RouterForTests().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if gotUser != "u-bearer" {
		t.Errorf("X-Forwarded-User=%q", gotUser)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

```bash
cd /root/agentserver
go test ./internal/server -run TestNotebookProxy -v
```
Expected: FAIL — handler not registered.

- [ ] **Step 3: Implement proxy**

Create `internal/server/notebook_proxy.go`:

```go
package server

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/notebookjwt"
	"github.com/agentserver/agentserver/internal/notebooksupervisor"
)

// notebookProxy handles "/api/notebooks/{ws}/*". Validates the JWT
// (from ?token=… or Authorization: Bearer …), strips the prefix,
// adds X-Forwarded-User, and reverse-proxies to the workspace's
// Jupyter Server. HTTP + WS in one path — httputil.ReverseProxy
// since Go 1.20 handles WS upgrades automatically.
func (s *Server) notebookProxy(w http.ResponseWriter, r *http.Request) {
	if len(s.NotebookJWTSecret) == 0 || s.NotebookSupervisor == nil {
		http.Error(w, "notebook feature disabled", http.StatusServiceUnavailable)
		return
	}

	wsID := chi.URLParam(r, "ws")
	if wsID == "" {
		http.Error(w, "workspace id required", http.StatusBadRequest)
		return
	}

	tok := extractToken(r)
	if tok == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	claims, err := notebookjwt.Verify(s.NotebookJWTSecret, tok)
	if err != nil {
		http.Error(w, "invalid token: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if claims.WorkspaceID != wsID {
		http.Error(w, "token workspace mismatch", http.StatusForbidden)
		return
	}

	// Get the upstream URL from the supervisor cache.
	ns, err := s.workspaceNamespace(r.Context(), wsID)
	if err != nil {
		http.Error(w, "namespace lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	k := notebooksupervisor.Key{WorkspaceID: wsID, Namespace: ns}
	handle, err := s.NotebookSupervisor.EnsureRunning(r.Context(), k)
	if err != nil {
		http.Error(w, "notebook not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.NotebookSupervisor.Touch(k) // refresh idle clock on every request

	upstreamURL, err := url.Parse(handle.ServiceURL)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusInternalServerError)
		return
	}

	prefix := "/api/notebooks/" + wsID
	rp := httputil.NewSingleHostReverseProxy(upstreamURL)
	originalDirector := rp.Director
	rp.Director = func(req *http.Request) {
		originalDirector(req)
		// Strip the prefix so jupyter sees /lab, /api/kernels, etc.
		if strings.HasPrefix(req.URL.Path, prefix) {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
			if req.URL.Path == "" {
				req.URL.Path = "/"
			}
		}
		// Drop our token from the forwarded query string.
		q := req.URL.Query()
		q.Del("token")
		req.URL.RawQuery = q.Encode()
		// Identity for jupyter (proxy is the trust boundary).
		req.Header.Set("X-Forwarded-User", claims.UserID)
	}
	rp.ServeHTTP(w, r)
}

func extractToken(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}
```

- [ ] **Step 4: Register route**

In `internal/server/server.go`'s chi router setup, add:

```go
r.HandleFunc("/api/notebooks/{ws}/*", s.notebookProxy)
```

`HandleFunc` (vs `Get`) so it accepts all methods including the implicit GET-then-Upgrade for WebSocket.

- [ ] **Step 5: Run, confirm pass**

```bash
cd /root/agentserver
go vet ./internal/server
go test ./internal/server -run TestNotebookProxy -v
```
Expected: 5 pass.

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add internal/server/notebook_proxy.go \
        internal/server/notebook_proxy_test.go \
        internal/server/server.go
git commit -m "feat(server): /api/notebooks/{ws}/* reverse proxy

HTTP + WS via httputil.ReverseProxy; JWT validated per request (query
or Bearer); X-Forwarded-User injected; prefix stripped before forwarding;
supervisor.Touch on every request to refresh idle clock."
```

---

## Task 5: Custom IdentityProvider (Python)

**Files:**
- Create: `notebook/identity_provider.py`
- Create: `notebook/identity_provider_test.py`

Pure Python; tested with the standard `unittest` shipped with Python 3.

- [ ] **Step 1: Write failing test**

Create `notebook/identity_provider_test.py`:

```python
"""Pure-Python tests for AgentserverIdentityProvider.

Run from the notebook image (or any Python 3.12 with jupyter-server
installed): `python -m unittest notebook/identity_provider_test.py`.
"""
import unittest
from unittest.mock import MagicMock

from identity_provider import AgentserverIdentityProvider


class TestAgentserverIdentityProvider(unittest.TestCase):
    def test_returns_user_from_x_forwarded_user(self):
        ip = AgentserverIdentityProvider()
        handler = MagicMock()
        handler.request.headers = {"X-Forwarded-User": "u-1"}
        # IdentityProvider's interface is async (get_user) in 2.x;
        # our impl reads the header sync — wrap in a coroutine run.
        import asyncio
        user = asyncio.run(ip.get_user(handler))
        self.assertIsNotNone(user)
        self.assertEqual(user.username, "u-1")

    def test_returns_anonymous_when_header_missing(self):
        ip = AgentserverIdentityProvider()
        handler = MagicMock()
        handler.request.headers = {}
        import asyncio
        user = asyncio.run(ip.get_user(handler))
        # Anonymous → either None (block by default) or a stub user
        # with a sentinel name; agentserver convention: return None so
        # jupyter rejects the request.
        self.assertIsNone(user)


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run, confirm failure**

```bash
cd /root/agentserver/notebook
PYTHONPATH=. python -m unittest identity_provider_test.py
```
Expected: ImportError (module missing).

- [ ] **Step 3: Implement**

Create `notebook/identity_provider.py`:

```python
"""Trust X-Forwarded-User from the agentserver web proxy.

The proxy validates a short-lived HMAC JWT before forwarding, so by the
time the request reaches Jupyter the header is trusted. Any request
arriving WITHOUT X-Forwarded-User is rejected (None return) — this
makes mis-deployment fail closed.
"""
from __future__ import annotations

from jupyter_server.auth import IdentityProvider, User


class AgentserverIdentityProvider(IdentityProvider):
    """Reads X-Forwarded-User; returns None for unauthenticated requests."""

    async def get_user(self, handler):
        username = handler.request.headers.get("X-Forwarded-User", "")
        if not username:
            return None
        return User(
            username=username,
            name=username,
            display_name=username,
            initials=username[:2].upper(),
            color=None,
            avatar_url=None,
        )
```

- [ ] **Step 4: Run, confirm pass**

```bash
cd /root/agentserver/notebook
PYTHONPATH=. python -m unittest identity_provider_test.py
```
Expected: 2 ok.

(If the test fails because `jupyter_server` isn't installed in your dev env, run inside the docker image: `docker run --rm -v $PWD:/work agentserver-notebook:dev python -m unittest /work/identity_provider_test.py` from the notebook directory.)

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add notebook/identity_provider.py notebook/identity_provider_test.py
git commit -m "feat(notebook): IdentityProvider trusting X-Forwarded-User

reads the header set by the agentserver web proxy; rejects unauthed
requests (None) so mis-deploy fails closed."
```

---

## Task 6: Custom KernelProvisioner (Python)

**Files:**
- Create: `notebook/kernel_provisioner.py`
- Create: `notebook/kernel_provisioner_test.py`

- [ ] **Step 1: Write failing test**

Create `notebook/kernel_provisioner_test.py`:

```python
"""Tests for AgentserverKernelProvisioner."""
import asyncio
import os
import unittest
from unittest.mock import patch

from kernel_provisioner import AgentserverKernelProvisioner, _current_user_ctx


class TestAgentserverKernelProvisioner(unittest.TestCase):
    def test_pre_launch_injects_user_id(self):
        prov = AgentserverKernelProvisioner()
        # Simulate IdentityProvider having set the contextvar before
        # the kernel manager calls pre_launch.
        token = _current_user_ctx.set("u-42")
        try:
            env = {"FOO": "bar"}
            asyncio.run(prov._apply_user_env(env))
            self.assertEqual(env["AGENTSERVER_USER_ID"], "u-42")
            self.assertEqual(env["FOO"], "bar")
        finally:
            _current_user_ctx.reset(token)

    def test_pre_launch_passthrough_when_no_user(self):
        prov = AgentserverKernelProvisioner()
        env = {"X": "y"}
        asyncio.run(prov._apply_user_env(env))
        # No user set → AGENTSERVER_USER_ID stays unset
        self.assertNotIn("AGENTSERVER_USER_ID", env)


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run, confirm failure**

```bash
cd /root/agentserver/notebook
PYTHONPATH=. python -m unittest kernel_provisioner_test.py
```
Expected: ImportError.

- [ ] **Step 3: Implement**

Create `notebook/kernel_provisioner.py`:

```python
"""KernelProvisioner that stamps AGENTSERVER_USER_ID into kernel env.

Reads the user from a ContextVar set upstream by the IdentityProvider
(per-request). Jupyter's kernel-start path runs inside that request
context, so the ContextVar resolves the per-call user even with shared
KernelProvisioner instances.

Wires the ContextVar in identity_provider.py's get_user — we set it
there so this provisioner stays stateless.
"""
from __future__ import annotations

import contextvars
from typing import Any

from jupyter_client.provisioning import LocalProvisioner


# Set by the request-scope IdentityProvider in identity_provider.py.
# Default empty string means "no user attribution" → no env injected.
_current_user_ctx: contextvars.ContextVar[str] = contextvars.ContextVar(
    "agentserver_current_user", default=""
)


class AgentserverKernelProvisioner(LocalProvisioner):
    """Wraps LocalProvisioner; injects AGENTSERVER_USER_ID into kernel env."""

    async def pre_launch(self, **kwargs: Any) -> dict[str, Any]:
        result = await super().pre_launch(**kwargs)
        env = result.get("env", {})
        await self._apply_user_env(env)
        result["env"] = env
        return result

    async def _apply_user_env(self, env: dict[str, str]) -> None:
        user = _current_user_ctx.get()
        if user:
            env["AGENTSERVER_USER_ID"] = user
```

Also update `notebook/identity_provider.py` to set the ContextVar in `get_user` (so the provisioner has something to read):

```python
# Add near the top:
from kernel_provisioner import _current_user_ctx

# Modify get_user to set the ctxvar:
async def get_user(self, handler):
    username = handler.request.headers.get("X-Forwarded-User", "")
    if not username:
        return None
    _current_user_ctx.set(username)
    return User(...)  # unchanged
```

- [ ] **Step 4: Run, confirm pass**

```bash
cd /root/agentserver/notebook
PYTHONPATH=. python -m unittest kernel_provisioner_test.py
PYTHONPATH=. python -m unittest identity_provider_test.py
```
Expected: 4 ok total.

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add notebook/kernel_provisioner.py \
        notebook/kernel_provisioner_test.py \
        notebook/identity_provider.py
git commit -m "feat(notebook): KernelProvisioner injects AGENTSERVER_USER_ID

LocalProvisioner subclass; reads per-request user from contextvars set
by AgentserverIdentityProvider; stamps env so the SDK in the kernel
attributes calls to the right user."
```

---

## Task 7: jupyter_server_config + Dockerfile.notebook update

**Files:**
- Modify: `notebook/jupyter_server_config.py`
- Modify: `Dockerfile.notebook`

- [ ] **Step 1: Update jupyter config**

Replace `notebook/jupyter_server_config.py` with:

```python
"""Plan 3b: plug AgentserverIdentityProvider + AgentserverKernelProvisioner.
base_url reads from NOTEBOOK_BASE_URL env (set per-workspace by the
supervisor) so generated jupyter URLs include /api/notebooks/{ws}/.
"""
import os
import sys

# Make our Python modules importable.
sys.path.insert(0, "/etc/jupyter/agentserver")

c = get_config()  # type: ignore[name-defined]  # noqa: F821 (provided by jupyter at runtime)

c.ServerApp.ip = "0.0.0.0"
c.ServerApp.port = 8888
c.ServerApp.open_browser = False
c.ServerApp.disable_check_xsrf = True
c.ServerApp.allow_origin = "*"
c.ServerApp.root_dir = "/workspace"
c.ServerApp.allow_root = True
c.ServerApp.base_url = os.environ.get("NOTEBOOK_BASE_URL", "/")

# Auth — trust X-Forwarded-User from agentserver web.
c.ServerApp.identity_provider_class = "identity_provider.AgentserverIdentityProvider"

# Kernel provisioner — inject per-request user_id env.
c.KernelManager.default_provisioner_name = "agentserver-local-provisioner"
```

For the kernel provisioner registration, add an entry-point in the Dockerfile (via pip install of a tiny shim package, OR via a `jupyter_client` plugin entry. Simplest: set `c.MultiKernelManager.kernel_provisioner_class` directly).

Actually, cleanest for v1: set `c.MappingKernelManager.kernel_provisioner_class` to the dotted class name:

```python
c.MultiKernelManager.kernel_provisioner_class = "kernel_provisioner.AgentserverKernelProvisioner"
```

If `MultiKernelManager.kernel_provisioner_class` isn't accepted directly (provisioner plugins are usually entry-point-driven), fall back to a small `provisioners.toml`:

```python
# Inline pyproject-like entry inside a small site-packages shim.
# Simpler: register via env var KERNEL_PROVISIONER_FACTORIES.
```

The implementer should pick whatever the installed `jupyter_client` version supports. If the entry-point approach is the only way, add a tiny `setup.py` that registers `agentserver-local-provisioner = kernel_provisioner:AgentserverKernelProvisioner` under `jupyter_client.kernel_provisioners`, and `pip install -e .` it in the Dockerfile.

- [ ] **Step 2: Update Dockerfile.notebook**

Edit `Dockerfile.notebook`. After the existing SDK install block, add:

```dockerfile
# Agentserver jupyter extensions (Plan 3b).
RUN mkdir -p /etc/jupyter/agentserver
COPY notebook/identity_provider.py /etc/jupyter/agentserver/
COPY notebook/kernel_provisioner.py /etc/jupyter/agentserver/
```

(If you took the entry-point route in Step 1, also COPY a `setup.py` and `pip install -e /etc/jupyter/agentserver`.)

- [ ] **Step 3: Build + bare-smoke**

```bash
cd /root/agentserver
docker build -f Dockerfile.notebook -t agentserver-notebook:dev .
# Smoke: jupyter can load the config without erroring
docker run --rm --entrypoint python agentserver-notebook:dev \
  -c "from identity_provider import AgentserverIdentityProvider; \
      from kernel_provisioner import AgentserverKernelProvisioner; \
      print('ok')"
```
Expected: builds; smoke prints "ok".

- [ ] **Step 4: Commit**

```bash
cd /root/agentserver
git add notebook/jupyter_server_config.py Dockerfile.notebook
git commit -m "feat(notebook): wire IdentityProvider + KernelProvisioner

jupyter_server_config plugs AgentserverIdentityProvider and registers
AgentserverKernelProvisioner. base_url from NOTEBOOK_BASE_URL env.
Dockerfile COPYs both Python modules to /etc/jupyter/agentserver."
```

---

## Task 8: Helm values + Deployment env + smoke

**Files:**
- Modify: `deploy/helm/agentserver/values.yaml`
- Modify: `deploy/helm/agentserver/templates/deployment.yaml`
- Modify: `cmd/serve.go` — read JWT secret from env, set on Server, plus pass `NOTEBOOK_BASE_URL` pattern through ExtraEnvVars

- [ ] **Step 1: values.yaml**

In `notebook:` block (added in Plan 3a), append:

```yaml
notebook:
  # ...existing fields from Plan 3a...

  # Plan 3b additions.
  jwtSecret: ""  # Empty = feature disabled. Generate with `openssl rand -hex 32`
  baseURLPattern: "/api/notebooks/{workspace_id}/"
```

- [ ] **Step 2: deployment.yaml**

Edit `deploy/helm/agentserver/templates/deployment.yaml`. Add to the agentserver web env block:

```yaml
{{- if .Values.notebook.jwtSecret }}
- name: NOTEBOOK_JWT_SECRET
  value: {{ .Values.notebook.jwtSecret | quote }}
{{- end }}
- name: NOTEBOOK_BASE_URL_PATTERN
  value: {{ .Values.notebook.baseURLPattern | quote }}
```

(Production hardening: move jwtSecret to a Secret resource and reference via secretKeyRef. v1 ships plaintext for ergonomics; document the upgrade path in the README.)

- [ ] **Step 3: cmd/serve.go wiring**

In `cmd/serve.go`, near where the notebook supervisor is constructed (added in Plan 3a's task 6), insert before `notebooksupervisor.New(...)`:

```go
// Plan 3b: set per-workspace base URL via ExtraEnvVars.
baseURLPattern := envOrDefault("NOTEBOOK_BASE_URL_PATTERN", "/api/notebooks/{workspace_id}/")
nbCfg.ExtraEnvVars = map[string]string{
    "NOTEBOOK_BASE_URL": baseURLPattern,
}
```

And separately set the JWT secret on the server:

```go
if v := os.Getenv("NOTEBOOK_JWT_SECRET"); v != "" {
    srv.NotebookJWTSecret = []byte(v)
}
```

- [ ] **Step 4: Build + helm lint**

```bash
cd /root/agentserver
go vet ./...
go build ./...
helm lint deploy/helm/agentserver
helm template deploy/helm/agentserver --set notebook.jwtSecret=dummy-test | grep -B1 -A2 "NOTEBOOK_"
```
Expected: clean lint; both env vars render.

- [ ] **Step 5: Update docker-compose smoke (from Plan 1)**

Edit `notebook/docker-compose.smoke.yml` (assumes Plan 1 is merged — if not, skip this step and note it). Add to the notebook service env:

```yaml
NOTEBOOK_BASE_URL: "/"  # smoke uses no proxy prefix
```

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add deploy/helm/agentserver/values.yaml \
        deploy/helm/agentserver/templates/deployment.yaml \
        cmd/serve.go \
        notebook/docker-compose.smoke.yml
git commit -m "feat(helm,server): wire NOTEBOOK_JWT_SECRET + base URL pattern

values.notebook.jwtSecret + baseURLPattern; agentserver web reads both
from env. baseURLPattern flows to supervisor ExtraEnvVars and lands as
NOTEBOOK_BASE_URL in each notebook pod."
```

---

## Self-review checklist (for the implementer)

After all tasks:
- [ ] `go test ./internal/notebookjwt ./internal/notebooksupervisor ./internal/server -v` — all pass
- [ ] `go vet ./... && go build ./...` clean
- [ ] `python -m unittest` for both notebook test files — pass
- [ ] `docker build -f Dockerfile.notebook -t agentserver-notebook:dev .` succeeds
- [ ] `helm lint deploy/helm/agentserver` clean
- [ ] /api/notebooks/{ws}/session returns {url, token, expires_at} for a member; 403 for non-member; 503 with no secret
- [ ] /api/notebooks/{ws}/* rejects missing/bad token; injects X-Forwarded-User; strips prefix

## After this plan

When Plan 3b is merged + Plan 3a is merged + Plan 1 + Plan 2:
- Cluster-side smoke: user opens NotebooksPanel (Plan 3c needed) → POST /session → iframe loads → jupyter renders → cell runs `await alpha.shell(...)` → SDK calls gateway → operation logged with correct user_id
- Plan 3c is just the React panels + minimal frontend wiring
