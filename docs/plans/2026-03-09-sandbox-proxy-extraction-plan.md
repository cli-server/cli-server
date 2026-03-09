# Sandbox Proxy Extraction Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Extract sandbox traffic proxying (WebSocket tunnel + HTTP subdomain reverse proxy) from agentserver into an independently deployable `sandbox-proxy` service within the mono-repo.

**Architecture:** New `cmd/sandboxproxy/` binary shares PostgreSQL and reuses `internal/db`, `internal/auth`, `internal/sbxstore`, `internal/tunnel` packages. The proxy handles all subdomain traffic while agentserver keeps API/management endpoints. Key coupling point: agentserver still needs `BaseDomain` and subdomain prefixes to generate sandbox URLs in API responses, and uses `TunnelRegistry` for cleanup on workspace/sandbox deletion.

**Tech Stack:** Go, chi router, nhooyr.io/websocket, httputil.ReverseProxy, PostgreSQL (shared)

---

### Task 1: Create `internal/sandboxproxy/config.go`

**Files:**
- Create: `internal/sandboxproxy/config.go`

**Step 1: Create the config file**

```go
package sandboxproxy

import "os"

// Config holds sandbox-proxy configuration loaded from environment variables.
type Config struct {
	DatabaseURL             string
	ListenAddr              string
	BaseDomain              string
	OpencodeAssetDomain     string
	OpencodeSubdomainPrefix string
	OpenclawSubdomainPrefix string
}

// LoadConfigFromEnv reads configuration from environment variables.
func LoadConfigFromEnv() Config {
	cfg := Config{
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		ListenAddr:              os.Getenv("LISTEN_ADDR"),
		BaseDomain:              os.Getenv("BASE_DOMAIN"),
		OpencodeAssetDomain:     os.Getenv("OPENCODE_ASSET_DOMAIN"),
		OpencodeSubdomainPrefix: os.Getenv("OPENCODE_SUBDOMAIN_PREFIX"),
		OpenclawSubdomainPrefix: os.Getenv("OPENCLAW_SUBDOMAIN_PREFIX"),
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8082"
	}
	if cfg.OpencodeSubdomainPrefix == "" {
		cfg.OpencodeSubdomainPrefix = "code"
	}
	if cfg.OpenclawSubdomainPrefix == "" {
		cfg.OpenclawSubdomainPrefix = "claw"
	}
	if cfg.OpencodeAssetDomain == "" && cfg.BaseDomain != "" {
		cfg.OpencodeAssetDomain = "opencodeapp." + cfg.BaseDomain
	}
	return cfg
}
```

**Step 2: Verify it compiles**

Run: `cd /root/agentserver && go build ./internal/sandboxproxy/`
Expected: Success (no output)

**Step 3: Commit**

```bash
git add internal/sandboxproxy/config.go
git commit -m "feat(sandboxproxy): add config package for sandbox-proxy service"
```

---

### Task 2: Create `internal/sandboxproxy/error_page.go`

Move the error page rendering from `internal/server/error_page.go` into the new package. This is a straight copy with package name change.

**Files:**
- Create: `internal/sandboxproxy/error_page.go`
- Reference: `internal/server/error_page.go` (copy content, change `package server` → `package sandboxproxy`)

**Step 1: Copy error_page.go with package change**

Copy the entire content of `internal/server/error_page.go`, changing only the package declaration from `package server` to `package sandboxproxy`. Keep all types, variables, functions, SVG icons, and the HTML template exactly as-is.

**Step 2: Verify it compiles**

Run: `cd /root/agentserver && go build ./internal/sandboxproxy/`
Expected: Success

**Step 3: Commit**

```bash
git add internal/sandboxproxy/error_page.go
git commit -m "feat(sandboxproxy): add error page rendering (copied from server)"
```

---

### Task 3: Create `internal/sandboxproxy/server.go` with Server struct and Router

**Files:**
- Create: `internal/sandboxproxy/server.go`

**Step 1: Create the server with struct, constructor, and router**

```go
package sandboxproxy

import (
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/sbxstore"
	"github.com/agentserver/agentserver/internal/tunnel"
)

// Server is the sandbox-proxy HTTP server that handles subdomain traffic
// proxying and WebSocket tunnel connections.
type Server struct {
	Auth                    *auth.Auth
	DB                      *db.DB
	Sandboxes               *sbxstore.Store
	TunnelRegistry          *tunnel.Registry
	OpencodeStaticFS        fs.FS
	BaseDomain              string
	OpencodeAssetDomain     string
	OpencodeSubdomainPrefix string
	OpenclawSubdomainPrefix string

	activityMu   sync.Mutex
	activityLast map[string]time.Time
}

// New creates a new sandbox-proxy server.
func New(cfg Config, authSvc *auth.Auth, database *db.DB, sandboxStore *sbxstore.Store, tunnelReg *tunnel.Registry, opcodeStaticFS fs.FS) *Server {
	s := &Server{
		Auth:                    authSvc,
		DB:                      database,
		Sandboxes:               sandboxStore,
		TunnelRegistry:          tunnelReg,
		OpencodeStaticFS:        opcodeStaticFS,
		BaseDomain:              cfg.BaseDomain,
		OpencodeAssetDomain:     cfg.OpencodeAssetDomain,
		OpencodeSubdomainPrefix: cfg.OpencodeSubdomainPrefix,
		OpenclawSubdomainPrefix: cfg.OpenclawSubdomainPrefix,
		activityLast:            make(map[string]time.Time),
	}
	s.initOpencodeAssetIndex()
	return s
}

// throttledActivity updates activity at most once per 30 seconds per sandbox.
func (s *Server) throttledActivity(sandboxID string) {
	s.activityMu.Lock()
	last, ok := s.activityLast[sandboxID]
	now := time.Now()
	if ok && now.Sub(last) < 30*time.Second {
		s.activityMu.Unlock()
		return
	}
	s.activityLast[sandboxID] = now
	s.activityMu.Unlock()
	s.Sandboxes.UpdateActivity(sandboxID)
}

// Router returns the HTTP handler for the sandbox-proxy service.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Subdomain middleware: if the Host matches {prefix}-{sandboxID}.{baseDomain},
	// proxy the entire request to the sandbox and skip all other routes.
	if s.BaseDomain != "" {
		r.Use(func(next http.Handler) http.Handler {
			suffix := "." + s.BaseDomain
			opcodePrefix := s.OpencodeSubdomainPrefix + "-"
			clawPrefix := s.OpenclawSubdomainPrefix + "-"
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host := r.Host
				if idx := strings.LastIndex(host, ":"); idx != -1 {
					host = host[:idx]
				}
				if strings.HasSuffix(host, suffix) {
					sub := strings.TrimSuffix(host, suffix)
					if s.OpencodeAssetDomain != "" && host == s.OpencodeAssetDomain {
						s.handleAssetDomainRequest(w, r)
						return
					}
					if strings.HasPrefix(sub, opcodePrefix) {
						sandboxID := sub[len(opcodePrefix):]
						s.handleSubdomainProxy(w, r, sandboxID)
						return
					}
					if strings.HasPrefix(sub, clawPrefix) {
						sandboxID := sub[len(clawPrefix):]
						s.handleOpenclawSubdomainProxy(w, r, sandboxID)
						return
					}
				}
				next.ServeHTTP(w, r)
			})
		})
	}

	// Health check.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Tunnel endpoint (auth via tunnel token, no cookie auth needed).
	r.HandleFunc("/api/tunnel/{sandboxId}", s.handleTunnel)

	return r
}
```

**Step 2: Verify it compiles**

Run: `cd /root/agentserver && go build ./internal/sandboxproxy/`
Expected: Compile errors for undefined methods (handleSubdomainProxy, handleOpenclawSubdomainProxy, handleAssetDomainRequest, handleTunnel, initOpencodeAssetIndex). This is expected — they will be added in subsequent tasks.

**Step 3: Commit** (skip — commit after methods are added)

---

### Task 4: Create `internal/sandboxproxy/opencode_proxy.go`

Move the opencode subdomain proxy handler from `internal/server/opencode_proxy.go` into the new package.

**Files:**
- Create: `internal/sandboxproxy/opencode_proxy.go`
- Reference: `internal/server/opencode_proxy.go`

**Step 1: Copy opencode_proxy.go with package change**

Copy the entire content of `internal/server/opencode_proxy.go`, changing:
1. `package server` → `package sandboxproxy`
2. All `(s *Server)` method receivers stay the same (both use `Server`)
3. All logic, imports, and constants stay exactly the same

The file contains:
- `handleSubdomainProxy()` — main proxy handler
- `tryServeOpencodeSPAFallback()` — SPA fallback logic
- `serveOpencodeFile()` — static file serving
- `handleAssetDomainRequest()` — shared asset domain handler
- `setAssetCORSHeaders()` — CORS for asset domain
- `initOpencodeAssetIndex()` — crossorigin attribute patching
- Helper types: `patchedFS`, `memFile`, `memFileInfo`, `readSeeker`
- Constants: `opencodePort`, `subdomainCookieKey`
- Variable: `opencodeAPIPrefixes`, `crossoriginTagRe`

**Step 2: Verify it compiles**

Run: `cd /root/agentserver && go build ./internal/sandboxproxy/`
Expected: Still compile errors for missing `handleOpenclawSubdomainProxy` and `handleTunnel`.

---

### Task 5: Create `internal/sandboxproxy/openclaw_proxy.go`

Move the openclaw subdomain proxy handler.

**Files:**
- Create: `internal/sandboxproxy/openclaw_proxy.go`
- Reference: `internal/server/openclaw_proxy.go`

**Step 1: Copy openclaw_proxy.go with package change**

Copy the entire content of `internal/server/openclaw_proxy.go`, changing only `package server` → `package sandboxproxy`. Everything else stays the same.

**Step 2: Verify it compiles**

Run: `cd /root/agentserver && go build ./internal/sandboxproxy/`
Expected: Still compile errors for missing `handleTunnel` and `proxyViaTunnel`.

---

### Task 6: Create `internal/sandboxproxy/tunnel.go`

Move the tunnel WebSocket handler and proxy-via-tunnel logic.

**Files:**
- Create: `internal/sandboxproxy/tunnel.go`
- Reference: `internal/server/tunnel.go`

**Step 1: Copy tunnel.go with package change**

Copy the entire content of `internal/server/tunnel.go`, changing only `package server` → `package sandboxproxy`. Everything else stays the same — all imports (`db`, `sbxstore`, `tunnel`, `websocket`, `chi`, `uuid`) and logic remain identical.

**Step 2: Verify it compiles**

Run: `cd /root/agentserver && go build ./internal/sandboxproxy/`
Expected: SUCCESS — all methods are now defined.

**Step 3: Commit all sandboxproxy files**

```bash
git add internal/sandboxproxy/
git commit -m "feat(sandboxproxy): add sandbox-proxy server package

Includes subdomain routing, opencode/openclaw proxy handlers,
WebSocket tunnel handler, error pages, and config."
```

---

### Task 7: Create `cmd/sandboxproxy/main.go`

**Files:**
- Create: `cmd/sandboxproxy/main.go`
- Reference: `cmd/llmproxy/main.go` (follow same pattern)

**Step 1: Create the service entry point**

```go
package main

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/sandboxproxy"
	"github.com/agentserver/agentserver/internal/sbxstore"
	"github.com/agentserver/agentserver/internal/tunnel"
	"github.com/agentserver/agentserver/opencodeweb"
)

func main() {
	cfg := sandboxproxy.LoadConfigFromEnv()

	if cfg.DatabaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if cfg.BaseDomain == "" {
		log.Fatal("BASE_DOMAIN is required")
	}

	// Connect to PostgreSQL.
	database, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Database connection failed: %v", err)
	}
	defer database.Close()
	log.Println("Connected to PostgreSQL")

	// Load embedded opencode frontend.
	var opcodeStaticFS fs.FS
	ocDistFS, err := fs.Sub(opencodeweb.StaticFS, "dist")
	if err != nil {
		log.Printf("Warning: embedded opencode static files not available: %v", err)
	} else {
		opcodeStaticFS = ocDistFS
	}

	authSvc := auth.New(database)
	sandboxStore := sbxstore.NewStore(database)
	tunnelReg := tunnel.NewRegistry()

	srv := sandboxproxy.New(cfg, authSvc, database, sandboxStore, tunnelReg, opcodeStaticFS)

	httpServer := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: srv.Router(),
	}

	// Graceful shutdown on SIGTERM/SIGINT.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("Received %v, shutting down...", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	log.Printf("Starting sandbox-proxy on %s (domain: %s)", cfg.ListenAddr, cfg.BaseDomain)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
```

**Step 2: Verify it compiles**

Run: `cd /root/agentserver && go build ./cmd/sandboxproxy/`
Expected: Success — produces `sandboxproxy` binary.

**Step 3: Commit**

```bash
git add cmd/sandboxproxy/main.go
git commit -m "feat(sandboxproxy): add service entry point (cmd/sandboxproxy)"
```

---

### Task 8: Create `Dockerfile.sandboxproxy`

**Files:**
- Create: `Dockerfile.sandboxproxy`
- Reference: `Dockerfile.llmproxy` (follow same pattern)

**Step 1: Create the Dockerfile**

```dockerfile
# Stage 1: Build opencode frontend from submodule (needed for embedded static files)
FROM oven/bun:1 AS opencode-frontend
WORKDIR /app
COPY opencode/ ./
RUN bun install --frozen-lockfile
RUN bun run --filter=@opencode-ai/app build

# Stage 2: Build Go binary
FROM golang:1.26-trixie AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=opencode-frontend /app/packages/app/dist ./opencodeweb/dist
RUN CGO_ENABLED=0 go build -o sandboxproxy ./cmd/sandboxproxy

# Stage 3: Runtime image (minimal — no Docker CLI needed)
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/sandboxproxy /usr/local/bin/sandboxproxy
EXPOSE 8082
ENTRYPOINT ["sandboxproxy"]
```

**Step 2: Commit**

```bash
git add Dockerfile.sandboxproxy
git commit -m "feat(sandboxproxy): add Dockerfile for sandbox-proxy service"
```

---

### Task 9: Clean up agentserver — remove proxy code from `internal/server/`

This is the critical step. Remove the proxy-related code from agentserver, but **keep** the pieces agentserver still needs.

**Files:**
- Delete: `internal/server/opencode_proxy.go`
- Delete: `internal/server/openclaw_proxy.go`
- Delete: `internal/server/tunnel.go`
- Delete: `internal/server/error_page.go`
- Modify: `internal/server/server.go`
- Modify: `cmd/serve.go`

**What agentserver still needs (do NOT remove):**
- `BaseDomain`, `OpencodeSubdomainPrefix`, `OpenclawSubdomainPrefix` fields — used by `toSandboxResponse()` to build URLs
- `TunnelRegistry` field — used by `handleDeleteSandbox()` and `handleDeleteWorkspace()` for cleanup

**Step 1: Delete proxy files**

```bash
rm internal/server/opencode_proxy.go
rm internal/server/openclaw_proxy.go
rm internal/server/tunnel.go
rm internal/server/error_page.go
```

**Step 2: Modify `internal/server/server.go`**

Remove these from the `Server` struct:
- `OpencodeStaticFS fs.FS` field
- `OpencodeAssetDomain string` field
- `activityMu sync.Mutex` field
- `activityLast map[string]time.Time` field

Remove these methods/functions:
- `throttledActivity()` method
- `initOpencodeAssetIndex()` call from `New()`

Remove from `New()` function:
- `opcodeStaticFS` parameter
- `OpencodeStaticFS` assignment
- `OpencodeAssetDomain` assignment and env var logic
- `activityLast: make(...)` assignment
- `s.initOpencodeAssetIndex()` call

Remove from `Router()`:
- The entire subdomain middleware block (lines 140-172, the `r.Use(func(next http.Handler) ...)` with Host matching)
- The tunnel endpoint: `r.HandleFunc("/api/tunnel/{sandboxId}", s.handleTunnel)` (line 183)

**Keep in Router():**
- `/internal/validate-proxy-token` endpoint (stays in agentserver)
- `/api/agent/register` endpoint (stays in agentserver)
- Everything else

**Step 3: Modify `cmd/serve.go`**

Remove:
- `opencodeweb` import
- `opencodeStaticFS` variable and its `fs.Sub(opencodeweb.StaticFS, "dist")` initialization
- `opencodeStaticFS` argument from `server.New(...)` call

The `server.New()` call should change from:
```go
srv := server.New(authSvc, oidcMgr, database, sandboxStore, procMgr, driveMgr, nsMgr, tunnel.NewRegistry(), staticFS, opencodeStaticFS, ...)
```
to:
```go
srv := server.New(authSvc, oidcMgr, database, sandboxStore, procMgr, driveMgr, nsMgr, tunnel.NewRegistry(), staticFS, ...)
```

Update `server.New()` signature accordingly (remove `opcodeStaticFS fs.FS` parameter).

**Step 4: Verify agentserver compiles**

Run: `cd /root/agentserver && go build .`
Expected: Success

**Step 5: Verify sandbox-proxy still compiles**

Run: `cd /root/agentserver && go build ./cmd/sandboxproxy/`
Expected: Success

**Step 6: Commit**

```bash
git add -A
git commit -m "refactor: remove proxy code from agentserver server package

Subdomain routing middleware, opencode/openclaw proxy handlers,
tunnel WebSocket handler, and error pages are now handled by the
sandbox-proxy service (internal/sandboxproxy)."
```

---

### Task 10: Verify both binaries build and run

**Step 1: Build both binaries**

Run:
```bash
cd /root/agentserver
go build -o /tmp/agentserver .
go build -o /tmp/sandboxproxy ./cmd/sandboxproxy
go build -o /tmp/llmproxy ./cmd/llmproxy
```
Expected: All three binaries build successfully.

**Step 2: Run vet and lint**

Run: `cd /root/agentserver && go vet ./...`
Expected: No issues.

**Step 3: Check for unused imports or dead code**

Run: `cd /root/agentserver && go build ./... 2>&1`
Expected: Clean build, no warnings.

**Step 4: Commit (if any fixups needed)**

Only commit if fixes were required. Otherwise, no commit.

---

### Task 11: Update design doc with coupling notes

**Files:**
- Modify: `docs/plans/2026-03-09-sandbox-proxy-extraction-design.md`

**Step 1: Add coupling notes section**

Add a "Remaining Coupling Points" section documenting:

1. **`BaseDomain` / subdomain prefixes** — agentserver still reads these env vars to generate sandbox URLs in API responses (`toSandboxResponse`). Both services must be configured with the same values.

2. **`TunnelRegistry`** — agentserver still holds a `TunnelRegistry` instance. When a workspace or sandbox is deleted via the API, agentserver closes the tunnel connection. Since the tunnel is actually managed by sandbox-proxy, this registry in agentserver will always be empty. The close-on-delete behavior now depends on sandbox-proxy detecting the DB status change (sandbox deleted → tunnel auth fails on next heartbeat). Alternatively, agentserver could call an internal API on sandbox-proxy to close tunnels — but this is acceptable as-is since tunnel heartbeats expire naturally.

3. **`sbxstore.Store`** — both services create independent in-memory stores loaded from the same DB. Updates made by one service (e.g., sandbox-proxy updating activity) are not immediately visible to the other's in-memory cache. This is acceptable since `sbxstore` refreshes from DB on each `Resolve()` call for cache misses.

**Step 2: Commit**

```bash
git add docs/plans/2026-03-09-sandbox-proxy-extraction-design.md
git commit -m "docs: add coupling notes to sandbox proxy extraction design"
```
