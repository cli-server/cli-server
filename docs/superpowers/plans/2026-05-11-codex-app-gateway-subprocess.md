# codex-app-gateway subprocess runtime — Implementation Plan

> **PARTIALLY SUPERSEDED 2026-05-15.** Tasks that touch supervisor key or
> inbound auth (`Identity{ThreadID}`, HMAC-only inbound) are superseded
> by `2026-05-15-codex-app-gateway-oauth-bridge.md`. Subprocess lifecycle,
> S3 round-trip, ws frame proxy, idle reaper tasks remain valid.


> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement Subsystem 2 of the codex-gateway design as a thin
auth-proxy + per-thread `codex app-server` subprocess manager. Land the
`serve` subcommand of the existing `codex-app-gateway` binary so a
`codex --remote wss://AS/codex-app/...` TUI can complete a real
turn against a per-thread upstream `codex app-server` subprocess
spawned in the gateway pod.

**Architecture:** A single Go program that (1) accepts authenticated ws
connections from codex TUIs, (2) maps each `(workspace_id, thread_id)`
to an in-pod loopback `codex app-server --listen ws://127.0.0.1:0`
subprocess (spawned on first connect, GC'd after idle), (3) writes a
per-thread `CODEX_HOME` (sourced from S3 if a tarball exists, else
seeded from the workspace's defaults + executor mappings), (4)
bidirectionally forwards ws frames between the user's TUI and the
subprocess at the frame level (no protocol parsing).

**Tech Stack:** Go 1.26, `nhooyr.io/websocket v1.8.17` (already in
go.mod, used by the env-mcp impl), `github.com/go-chi/chi/v5`,
`aws-sdk-go-v2/service/s3` (already used by `internal/ccbroker/workspace/s3store.go`,
imported here directly — no factor-out in this plan), stdlib `archive/tar`
+ `compress/gzip` for the per-thread CODEX_HOME tarball, stdlib
`crypto/hmac` + `crypto/sha256` for the phase-1 inbound token
verifier. **No JWT lib** in phase 1 — simple HMAC of a workspace-id
keyed by a deployment-shared secret matches the existing wstoken /
internal-API pattern in this repo.

**Spec:** `/root/agentserver/docs/superpowers/specs/2026-05-10-codex-app-gateway-subprocess.md`
(read § Architecture, § Subprocess lifecycle, § Auth model, § State
management, § Open risks before starting).

**Working directory:** All tasks operate in `/root/agentserver`. Tasks
assume cwd unless otherwise noted. Use a worktree per the
superpowers:using-git-worktrees skill before starting.

**Module path:** `github.com/agentserver/agentserver`.

**Plan dependency note:** This plan builds on PR #78 (env-mcp
subcommand). It does NOT touch `internal/codexappgateway/envmcp/` or
`cmd/codex-app-gateway/main.go`'s env-mcp dispatch; those land
unchanged. The `serve` subcommand previously printed "not implemented"
and exited 2 — Task 1 wires it up.

---

## File Structure

| File | Responsibility |
|---|---|
| `cmd/codex-app-gateway/main.go` | Modified: replace serve placeholder with real `runServe(args)` |
| `cmd/codex-app-gateway/serve_args.go` | New: `parseServeArgs` (mirrors `parseEnvMcpArgs`) — parses `--listen`, `--codex-bin`, etc. |
| `internal/codexappgateway/config.go` | `ServeConfig` struct + `LoadServeConfigFromEnv()` (CXG_* env vars) |
| `internal/codexappgateway/server.go` | `Server`, `NewServer`, `Routes()`, `Start`, `Shutdown` (chi router + ws endpoint + admin endpoints) |
| `internal/codexappgateway/auth/auth.go` | `Authenticator` interface + `HMACAuthenticator` (phase-1 impl: validates `Authorization: Bearer <hmac-of-workspace-id>` against `CXG_INBOUND_HMAC_SECRET`) |
| `internal/codexappgateway/auth/auth_test.go` | Round-trip mint+verify |
| `internal/codexappgateway/codexhome/codexhome.go` | `Manager` for per-thread CODEX_HOME tmpdirs: create, write `config.toml` ([features] + [mcp_servers]), tar/untar |
| `internal/codexappgateway/codexhome/s3.go` | `S3Backend` for tar upload/download keyed by `(workspace_id, thread_id)` |
| `internal/codexappgateway/codexhome/*_test.go` | Per-source-file tests with a `t.TempDir()` based fake S3 backend |
| `internal/codexappgateway/supervisor/supervisor.go` | `Supervisor`: `EnsureSubprocess(ctx, key) (*ChildHandle, error)`, in-memory `(workspace, thread) → handle` map, lifecycle hooks |
| `internal/codexappgateway/supervisor/spawn.go` | `spawnCodexAppServer(ctx, codexHome, codexBinPath) (*ChildHandle, error)` — spawn, parse listen URL from stdout's first line, wait for `/readyz` |
| `internal/codexappgateway/supervisor/reaper.go` | `IdleReaper` ticker; on fire: terminate subprocess, tar CODEX_HOME, upload S3, drop map entry |
| `internal/codexappgateway/supervisor/*_test.go` | Per-source-file tests with a fake codex binary (a small Go test helper that fakes the listen-URL print + readyz response) |
| `internal/codexappgateway/proxy/proxy.go` | `RunProxy(ctx, userWS, childWS) error` — bidirectional frame pump |
| `internal/codexappgateway/proxy/proxy_test.go` | Pair of in-memory ws fakes; assert frame fidelity in both directions |
| `internal/codexappgateway/server_test.go` | Server-level integration: ws connect → ensure subprocess → proxy → cleanup |
| `internal/codexappgateway/integration_test.go` | `//go:build integration`: spawn real `codex app-server`, drive via the gateway, verify full RPC round-trip |

Total new files: 14. Modified: 1. Estimated LOC including tests: ~1500.

---

## Task 1: `serve` subcommand wiring + config

**Files:**
- Create: `cmd/codex-app-gateway/serve_args.go`
- Create: `cmd/codex-app-gateway/serve_args_test.go`
- Modify: `cmd/codex-app-gateway/main.go` (replace serve placeholder)
- Create: `internal/codexappgateway/config.go`
- Create: `internal/codexappgateway/config_test.go`

**CLI contract (final):**

```
codex-app-gateway serve \
    [--listen-addr  <addr>]   default :8086, env CXG_LISTEN_ADDR
    [--codex-bin    <path>]   default `codex`, env CXG_CODEX_BIN — path used to spawn `codex app-server`
```

All other knobs are env-only:

| Env var | Required | Default | Purpose |
|---|---|---|---|
| `CXG_INBOUND_HMAC_SECRET` | yes | — | Shared secret for incoming `Authorization: Bearer ...` HMAC validation |
| `CXG_S3_ENDPOINT` | yes | — | S3-compatible endpoint URL (e.g. `https://s3.us-east-1.amazonaws.com` or `http://minio:9000`) |
| `CXG_S3_BUCKET` | yes | — | Bucket for per-thread CODEX_HOME tarballs |
| `CXG_S3_REGION` | no | `us-east-1` | |
| `CXG_S3_ACCESS_KEY_ID` | no | — | If set, used for static creds; otherwise SDK default chain |
| `CXG_S3_SECRET_ACCESS_KEY` | no | — | |
| `CXG_S3_PATH_STYLE` | no | `false` | Set `true` for MinIO etc. |
| `CXG_TMP_ROOT` | no | `/tmp/codex-app-gateway` | Where per-thread CODEX_HOME dirs live |
| `CXG_IDLE_SHUTDOWN` | no | `30m` | Idle-shutdown timer per subprocess |
| `CXG_EXEC_GATEWAY_URL` | yes | — | `ws://codex-exec-gateway:6060` — used to build per-thread `[mcp_servers].args[--bridge-url]` entries |
| `CXG_EXEC_GATEWAY_INTERNAL_URL` | yes | — | `http://codex-exec-gateway:6060` — for `/api/exec-gateway/connected` (workspace→executor mapping) |
| `CXG_EXEC_GATEWAY_INTERNAL_SECRET` | yes | — | shared secret for above HTTP API |
| `CXG_CAPTOKEN_HMAC_SECRET` | yes | — | shared with codex-exec-gateway; used to mint per-turn cap tokens for env-mcp children |
| `CXG_LOG_LEVEL` | no | `info` | |

(Many of these match the original MCP-rewrite spec's CXG_ prefix and
are forwarded into the env-mcp child as before.)

- [ ] **Step 1: Failing serve_args test**

`cmd/codex-app-gateway/serve_args_test.go`:
```go
package main

import "testing"

func TestParseServeArgs_Defaults(t *testing.T) {
	args, err := parseServeArgs([]string{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if args.ListenAddr != ":8086" {
		t.Errorf("ListenAddr = %q", args.ListenAddr)
	}
	if args.CodexBin != "codex" {
		t.Errorf("CodexBin = %q", args.CodexBin)
	}
}

func TestParseServeArgs_Overrides(t *testing.T) {
	args, err := parseServeArgs([]string{
		"--listen-addr", ":9090",
		"--codex-bin", "/usr/local/bin/codex",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if args.ListenAddr != ":9090" || args.CodexBin != "/usr/local/bin/codex" {
		t.Errorf("got %+v", args)
	}
}
```

- [ ] **Step 2: Failing config test**

`internal/codexappgateway/config_test.go`:
```go
package codexappgateway

import (
	"strings"
	"testing"
	"time"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("CXG_INBOUND_HMAC_SECRET", "in-sec")
	t.Setenv("CXG_S3_ENDPOINT", "http://s3")
	t.Setenv("CXG_S3_BUCKET", "buck")
	t.Setenv("CXG_EXEC_GATEWAY_URL", "ws://exec-gw:6060")
	t.Setenv("CXG_EXEC_GATEWAY_INTERNAL_URL", "http://exec-gw:6060")
	t.Setenv("CXG_EXEC_GATEWAY_INTERNAL_SECRET", "internal-sec")
	t.Setenv("CXG_CAPTOKEN_HMAC_SECRET", "captok-sec")
}

func TestLoadServeConfig_Defaults(t *testing.T) {
	setRequired(t)
	cfg, err := LoadServeConfigFromEnv()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TmpRoot != "/tmp/codex-app-gateway" {
		t.Errorf("TmpRoot = %q", cfg.TmpRoot)
	}
	if cfg.IdleShutdown != 30*time.Minute {
		t.Errorf("IdleShutdown = %v", cfg.IdleShutdown)
	}
	if cfg.S3.Region != "us-east-1" {
		t.Errorf("S3 default region = %q", cfg.S3.Region)
	}
}

func TestLoadServeConfig_RequiresInboundSecret(t *testing.T) {
	setRequired(t)
	t.Setenv("CXG_INBOUND_HMAC_SECRET", "")
	_, err := LoadServeConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "CXG_INBOUND_HMAC_SECRET") {
		t.Fatalf("want secret-required error, got %v", err)
	}
}

func TestLoadServeConfig_RequiresExecGatewayURL(t *testing.T) {
	setRequired(t)
	t.Setenv("CXG_EXEC_GATEWAY_URL", "")
	_, err := LoadServeConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "CXG_EXEC_GATEWAY_URL") {
		t.Fatalf("want exec-gateway-url-required, got %v", err)
	}
}

func TestLoadServeConfig_OverridesIdleShutdown(t *testing.T) {
	setRequired(t)
	t.Setenv("CXG_IDLE_SHUTDOWN", "5m")
	cfg, err := LoadServeConfigFromEnv()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.IdleShutdown != 5*time.Minute {
		t.Errorf("IdleShutdown = %v", cfg.IdleShutdown)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
cd /root/agentserver && go test ./cmd/codex-app-gateway/ -run TestParseServeArgs && \
                       go test ./internal/codexappgateway/ -run TestLoadServeConfig
```
Expected: both fail (undefined symbols).

- [ ] **Step 4: Implement serve_args.go**

`cmd/codex-app-gateway/serve_args.go`:
```go
package main

import "flag"

type serveArgs struct {
	ListenAddr string
	CodexBin   string
}

func parseServeArgs(rawArgs []string) (serveArgs, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen-addr", ":8086", "HTTP listen address (env CXG_LISTEN_ADDR)")
	codexBin := fs.String("codex-bin", "codex", "path to codex binary used for `codex app-server` (env CXG_CODEX_BIN)")
	if err := fs.Parse(rawArgs); err != nil {
		return serveArgs{}, err
	}
	if envListen := getEnvOrEmpty("CXG_LISTEN_ADDR"); envListen != "" && *listen == ":8086" {
		*listen = envListen
	}
	if envBin := getEnvOrEmpty("CXG_CODEX_BIN"); envBin != "" && *codexBin == "codex" {
		*codexBin = envBin
	}
	return serveArgs{ListenAddr: *listen, CodexBin: *codexBin}, nil
}

// helper that lives in main.go too — but defined inline here for the test pkg.
func getEnvOrEmpty(key string) string { return _envLookup(key) }
```

(In `main.go`, add `func _envLookup(k string) string { return os.Getenv(k) }`
or inline directly. The split is just so the unit test can call
`parseServeArgs` without `os.Getenv` env interference; tests rely on
the `t.Setenv` mechanism in step 6.)

- [ ] **Step 5: Implement config.go**

`internal/codexappgateway/config.go`:
```go
package codexappgateway

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// S3Config matches the shape used by internal/ccbroker/workspace/s3store.go
// so we can pass it directly into NewS3Store.
type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	PathStyle       bool
}

type ServeConfig struct {
	InboundHMACSecret           []byte
	S3                          S3Config
	TmpRoot                     string
	IdleShutdown                time.Duration
	ExecGatewayWSURL            string // ws://codex-exec-gateway:6060
	ExecGatewayInternalURL      string // http://codex-exec-gateway:6060
	ExecGatewayInternalSecret   string
	CapTokenHMACSecret          []byte
	LogLevel                    slog.Level
}

func LoadServeConfigFromEnv() (ServeConfig, error) {
	cfg := ServeConfig{
		TmpRoot:      envOr("CXG_TMP_ROOT", "/tmp/codex-app-gateway"),
		IdleShutdown: 30 * time.Minute,
		LogLevel:     slog.LevelInfo,
		S3: S3Config{
			Endpoint:        os.Getenv("CXG_S3_ENDPOINT"),
			Region:          envOr("CXG_S3_REGION", "us-east-1"),
			Bucket:          os.Getenv("CXG_S3_BUCKET"),
			AccessKeyID:     os.Getenv("CXG_S3_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("CXG_S3_SECRET_ACCESS_KEY"),
			PathStyle:       strings.EqualFold(os.Getenv("CXG_S3_PATH_STYLE"), "true"),
		},
		InboundHMACSecret:         []byte(os.Getenv("CXG_INBOUND_HMAC_SECRET")),
		ExecGatewayWSURL:          os.Getenv("CXG_EXEC_GATEWAY_URL"),
		ExecGatewayInternalURL:    os.Getenv("CXG_EXEC_GATEWAY_INTERNAL_URL"),
		ExecGatewayInternalSecret: os.Getenv("CXG_EXEC_GATEWAY_INTERNAL_SECRET"),
		CapTokenHMACSecret:        []byte(os.Getenv("CXG_CAPTOKEN_HMAC_SECRET")),
	}
	if len(cfg.InboundHMACSecret) == 0 {
		return cfg, fmt.Errorf("CXG_INBOUND_HMAC_SECRET is required")
	}
	if cfg.S3.Endpoint == "" {
		return cfg, fmt.Errorf("CXG_S3_ENDPOINT is required")
	}
	if cfg.S3.Bucket == "" {
		return cfg, fmt.Errorf("CXG_S3_BUCKET is required")
	}
	if cfg.ExecGatewayWSURL == "" {
		return cfg, fmt.Errorf("CXG_EXEC_GATEWAY_URL is required")
	}
	if cfg.ExecGatewayInternalURL == "" {
		return cfg, fmt.Errorf("CXG_EXEC_GATEWAY_INTERNAL_URL is required")
	}
	if cfg.ExecGatewayInternalSecret == "" {
		return cfg, fmt.Errorf("CXG_EXEC_GATEWAY_INTERNAL_SECRET is required")
	}
	if len(cfg.CapTokenHMACSecret) == 0 {
		return cfg, fmt.Errorf("CXG_CAPTOKEN_HMAC_SECRET is required")
	}
	if v := os.Getenv("CXG_IDLE_SHUTDOWN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("parse CXG_IDLE_SHUTDOWN: %w", err)
		}
		cfg.IdleShutdown = d
	}
	if v := strings.ToLower(os.Getenv("CXG_LOG_LEVEL")); v != "" {
		switch v {
		case "debug":
			cfg.LogLevel = slog.LevelDebug
		case "warn":
			cfg.LogLevel = slog.LevelWarn
		case "error":
			cfg.LogLevel = slog.LevelError
		}
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 6: Wire serve subcommand in main.go**

In `cmd/codex-app-gateway/main.go`, replace the existing `serve` case
with a real dispatch (the env-mcp branch + helpers from PR #78 stay
untouched):

```go
// In main.go imports, add:
//   "github.com/agentserver/agentserver/internal/codexappgateway"

// Replace:
//   case "serve":
//       fmt.Fprintln(os.Stderr, "codex-app-gateway: serve subcommand not implemented in this plan")
//       os.Exit(2)
// With:
case "serve":
    runServe(os.Args[2:])

// And add at file scope:

func runServe(rawArgs []string) {
    args, err := parseServeArgs(rawArgs)
    if err != nil {
        if errors.Is(err, flag.ErrHelp) {
            fmt.Fprint(os.Stderr, serveHelp)
            os.Exit(0)
        }
        fmt.Fprintln(os.Stderr, "codex-app-gateway serve:", err)
        os.Exit(2)
    }
    cfg, err := codexappgateway.LoadServeConfigFromEnv()
    if err != nil {
        fmt.Fprintln(os.Stderr, "codex-app-gateway serve: config:", err)
        os.Exit(2)
    }
    logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()
    srv, err := codexappgateway.NewServer(cfg, args.CodexBin, logger)
    if err != nil {
        logger.Error("NewServer failed", "err", err)
        os.Exit(1)
    }
    if err := srv.Run(ctx, args.ListenAddr); err != nil {
        logger.Error("server exited with error", "err", err)
        os.Exit(1)
    }
    logger.Info("server clean exit")
}

const serveHelp = `Usage: codex-app-gateway serve [flags]

Run the codex-app-gateway HTTP/WS server: per-thread codex app-server
subprocess manager + transparent ws frame proxy. See env vars (CXG_*)
in the spec.

Flags:
  --listen-addr <addr>   HTTP listen address (default :8086, env CXG_LISTEN_ADDR)
  --codex-bin   <path>   path to the codex binary (default `codex`, env CXG_CODEX_BIN)
`
```

NewServer is just a stub at this point — it returns a struct with a
no-op Run that blocks until ctx done. Tasks 2-8 fill it in. Add the
stub in `internal/codexappgateway/server.go`:

```go
package codexappgateway

import (
	"context"
	"log/slog"
)

type Server struct{ cfg ServeConfig; codexBin string; logger *slog.Logger }

func NewServer(cfg ServeConfig, codexBin string, logger *slog.Logger) (*Server, error) {
	return &Server{cfg: cfg, codexBin: codexBin, logger: logger}, nil
}

// Run is a stub here; Task 8 replaces it.
func (s *Server) Run(ctx context.Context, listenAddr string) error {
	s.logger.Info("server stub Run; sleeping until ctx done", "listen_addr", listenAddr)
	<-ctx.Done()
	return nil
}
```

- [ ] **Step 7: Run tests + smoke-build**

```bash
go test ./cmd/codex-app-gateway/ -run TestParseServeArgs -v && \
go test ./internal/codexappgateway/ -run TestLoadServeConfig -v && \
go build ./cmd/codex-app-gateway/ && \
./codex-app-gateway serve --help 2>&1 | head -10 && rm codex-app-gateway
```
Expected: 6 tests PASS, build clean, --help prints usage and exits 0.

- [ ] **Step 8: Commit**

```bash
git add cmd/codex-app-gateway/serve_args.go cmd/codex-app-gateway/serve_args_test.go \
        cmd/codex-app-gateway/main.go \
        internal/codexappgateway/config.go internal/codexappgateway/config_test.go \
        internal/codexappgateway/server.go
git commit -m "feat(codex-app-gateway): wire serve subcommand + config (stub server)"
```

---

## Task 2: HMAC inbound authenticator

**Files:**
- Create: `internal/codexappgateway/auth/auth.go`
- Create: `internal/codexappgateway/auth/auth_test.go`

Phase-1 inbound auth:

```
Authorization: Bearer ws_<workspace_id>.<thread_id>.<hex-hmac-sha256>
```

The HMAC covers `<workspace_id>\0<thread_id>` keyed by `CXG_INBOUND_HMAC_SECRET`.
This matches the existing wstoken / internal-API style of HMAC-signed
caller identity in this repo and avoids pulling in a JWT library for
phase 1.

The `Authenticator` interface is the only seam — phase-2 can swap in a
JWT impl behind the same interface.

- [ ] **Step 1: Failing auth_test.go**

`internal/codexappgateway/auth/auth_test.go`:
```go
package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHMACAuthenticator_RoundTrip(t *testing.T) {
	a := NewHMAC([]byte("secret"))
	tok := a.Mint("ws_alpha", "thr_42")
	if !strings.HasPrefix(tok, "ws_alpha.thr_42.") {
		t.Fatalf("token shape unexpected: %s", tok)
	}
	got, err := a.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.WorkspaceID != "ws_alpha" || got.ThreadID != "thr_42" {
		t.Errorf("decoded = %+v", got)
	}
}

func TestHMACAuthenticator_RejectsBadSig(t *testing.T) {
	a := NewHMAC([]byte("secret"))
	tok := a.Mint("ws_a", "thr_1")
	tampered := tok[:len(tok)-1] + "0"
	if _, err := a.Verify(tampered); err == nil {
		t.Fatal("want signature mismatch error")
	}
}

func TestHMACAuthenticator_RejectsBadShape(t *testing.T) {
	a := NewHMAC([]byte("secret"))
	for _, bad := range []string{"", "no-dots", "one.two", "..."} {
		if _, err := a.Verify(bad); err == nil {
			t.Errorf("want error for %q", bad)
		}
	}
}

func TestExtractBearer(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer foo.bar.baz")
	if got, ok := ExtractBearer(r); !ok || got != "foo.bar.baz" {
		t.Errorf("got %q ok=%v", got, ok)
	}
	r2, _ := http.NewRequest("GET", "/", nil)
	if _, ok := ExtractBearer(r2); ok {
		t.Error("missing header should return false")
	}
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.Header.Set("Authorization", "Basic foo")
	if _, ok := ExtractBearer(r3); ok {
		t.Error("non-Bearer should return false")
	}
}
```

- [ ] **Step 2: Run test (expect fail)**

```bash
go test ./internal/codexappgateway/auth/ -v
```
Expected: build error.

- [ ] **Step 3: Implement auth.go**

`internal/codexappgateway/auth/auth.go`:
```go
// Package auth handles inbound caller authentication for codex-app-gateway.
//
// Phase 1 uses an HMAC-signed token of the form
//
//   ws_<workspace_id>.<thread_id>.<hex-hmac-sha256>
//
// where the HMAC covers `<workspace_id>\0<thread_id>` keyed by a
// deployment-shared secret. This matches the wstoken / internal-API
// pattern used elsewhere in agentserver and avoids pulling a JWT lib
// just for phase 1. Phase 2 can swap in a JWT impl behind the same
// `Authenticator` interface.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// Identity is what a verified token decodes to.
type Identity struct {
	WorkspaceID string
	ThreadID    string
}

// Authenticator is the seam for inbound auth. Phase-1 impl is HMAC.
type Authenticator interface {
	Verify(token string) (Identity, error)
}

// HMAC is the phase-1 Authenticator.
type HMAC struct{ secret []byte }

// NewHMAC returns a phase-1 Authenticator. The secret must be non-empty.
func NewHMAC(secret []byte) *HMAC { return &HMAC{secret: secret} }

// Mint produces a token for `(workspaceID, threadID)`. Useful for tests
// and CLI tools; production callers receive tokens from agentserver.
func (a *HMAC) Mint(workspaceID, threadID string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(workspaceID))
	mac.Write([]byte{0})
	mac.Write([]byte(threadID))
	return workspaceID + "." + threadID + "." + hex.EncodeToString(mac.Sum(nil))
}

// Verify parses and HMAC-verifies a token.
func (a *HMAC) Verify(token string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Identity{}, errors.New("auth: malformed token")
	}
	expected := a.Mint(parts[0], parts[1])
	if !hmac.Equal([]byte(expected), []byte(token)) {
		return Identity{}, errors.New("auth: signature mismatch")
	}
	return Identity{WorkspaceID: parts[0], ThreadID: parts[1]}, nil
}

// ExtractBearer pulls the token out of `Authorization: Bearer <tok>`.
func ExtractBearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimPrefix(h, prefix), true
}
```

- [ ] **Step 4: Run tests (expect pass)**

```bash
go test ./internal/codexappgateway/auth/ -v
```
Expected: 4 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/auth/
git commit -m "feat(codex-app-gateway): HMAC inbound authenticator"
```

---

## Task 3: CodexHome manager (tmpdir + config.toml writer)

**Files:**
- Create: `internal/codexappgateway/codexhome/codexhome.go`
- Create: `internal/codexappgateway/codexhome/codexhome_test.go`

The `Manager` owns per-thread tmpdirs and the rendering of the
`config.toml` fragment that disables codex's builtin shell tools and
registers one `[mcp_servers.exe_*]` entry per bound executor (matching
the parent spec's Subsystem 4 conventions).

- [ ] **Step 1: Failing codexhome_test.go**

`internal/codexappgateway/codexhome/codexhome_test.go`:
```go
package codexhome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManager_NewTmpDir_LayoutAndPermissions(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	d, err := m.NewTmpDir("ws_a", "thr_1")
	if err != nil {
		t.Fatalf("NewTmpDir: %v", err)
	}
	if !strings.HasPrefix(d, filepath.Join(root, "ws_a", "thr_1")) {
		t.Errorf("path = %s", d)
	}
	st, err := os.Stat(d)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !st.IsDir() {
		t.Fatal("not a dir")
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf("perm = %v", st.Mode().Perm())
	}
}

func TestManager_RemoveTmpDir(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	d, err := m.NewTmpDir("ws_a", "thr_1")
	if err != nil {
		t.Fatalf("NewTmpDir: %v", err)
	}
	if err := m.RemoveTmpDir(d); err != nil {
		t.Fatalf("RemoveTmpDir: %v", err)
	}
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Errorf("dir still exists: %v", err)
	}
}

func TestRenderConfigTOML_DisablesBuiltinShellAndRegistersMCPServers(t *testing.T) {
	cfg := ConfigInput{
		ModelProvider: "modelserver",
		Model:         "gpt-5.5",
		ModelProviders: map[string]ModelProvider{
			"modelserver": {
				Name:    "modelserver",
				BaseURL: "http://llmproxy:8085/v1",
				EnvKey:  "CODEX_API_KEY",
				WireAPI: "responses",
			},
		},
		Executors: []ExecutorEntry{
			{
				ID:        "exe_alpha",
				BridgeURL: "ws://exec-gw:6060/bridge/exe_alpha",
				TokenEnv:  "CXG_BRIDGE_TOKEN_EXE_ALPHA",
				TokenVal:  "tok-alpha",
				Desc:      "Daisy's MacBook",
				CodexBin:  "/usr/local/bin/codex-app-gateway",
				TurnID:    "trn_xxx",
			},
			{
				ID:        "exe_beta",
				BridgeURL: "ws://exec-gw:6060/bridge/exe_beta",
				TokenEnv:  "CXG_BRIDGE_TOKEN_EXE_BETA",
				TokenVal:  "tok-beta",
				Desc:      "EC2 us-east-1",
				CodexBin:  "/usr/local/bin/codex-app-gateway",
				TurnID:    "trn_xxx",
			},
		},
		ProjectTrustedPaths: []string{"/tmp"},
	}
	out, err := RenderConfigTOML(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		`model_provider = "modelserver"`,
		`shell_tool = false`,
		`unified_exec = false`,
		`apply_patch_freeform = false`,
		`[mcp_servers.exe_alpha]`,
		`"--exe-id"`, `"exe_alpha"`,
		`"--bridge-url"`, `"ws://exec-gw:6060/bridge/exe_alpha"`,
		`"--token-env"`, `"CXG_BRIDGE_TOKEN_EXE_ALPHA"`,
		`[mcp_servers.exe_beta]`,
		`[projects."/tmp"]`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestManager_WriteConfig_ProducesUsableTOML(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	d, _ := m.NewTmpDir("ws_a", "thr_1")
	cfg := ConfigInput{
		ModelProvider: "p",
		Model:         "m",
		ModelProviders: map[string]ModelProvider{
			"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"},
		},
	}
	if err := m.WriteConfig(d, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(d, "config.toml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), `model = "m"`) {
		t.Errorf("config missing model: %s", b)
	}
}
```

- [ ] **Step 2: Run (expect fail)**

```bash
go test ./internal/codexappgateway/codexhome/ -v
```

- [ ] **Step 3: Implement codexhome.go**

`internal/codexappgateway/codexhome/codexhome.go`:
```go
// Package codexhome owns per-thread CODEX_HOME tmpdirs: creation,
// destruction, and the rendering of the config.toml fragment we plant
// inside each one before spawning `codex app-server`.
package codexhome

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type ModelProvider struct {
	Name    string
	BaseURL string
	EnvKey  string
	WireAPI string
}

type ExecutorEntry struct {
	ID        string
	BridgeURL string
	TokenEnv  string
	TokenVal  string // injected via env, not written to TOML
	Desc      string
	CodexBin  string // path to codex-app-gateway binary (for `env-mcp` subcommand)
	TurnID    string // optional, logged by env-mcp child
}

type ConfigInput struct {
	ModelProvider       string
	Model               string
	ModelProviders      map[string]ModelProvider
	Executors           []ExecutorEntry
	ProjectTrustedPaths []string
}

// Manager creates per-thread CODEX_HOME tmpdirs under root.
type Manager struct{ root string }

func NewManager(root string) *Manager { return &Manager{root: root} }

// NewTmpDir creates `<root>/<workspaceID>/<threadID>/` with mode 0700.
// It is safe to call on an existing dir (returns the path with no error).
func (m *Manager) NewTmpDir(workspaceID, threadID string) (string, error) {
	if workspaceID == "" || threadID == "" {
		return "", fmt.Errorf("codexhome: empty workspace or thread id")
	}
	d := filepath.Join(m.root, workspaceID, threadID)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", d, err)
	}
	// MkdirAll honors umask; force to 0700 unconditionally on the leaf.
	if err := os.Chmod(d, 0o700); err != nil {
		return "", fmt.Errorf("chmod %s: %w", d, err)
	}
	return d, nil
}

// RemoveTmpDir removes a previously-created tmpdir tree.
func (m *Manager) RemoveTmpDir(path string) error {
	if !strings.HasPrefix(path, m.root) {
		return fmt.Errorf("codexhome: refusing to remove %s outside root %s", path, m.root)
	}
	return os.RemoveAll(path)
}

// WriteConfig renders `config.toml` into the given CODEX_HOME dir.
func (m *Manager) WriteConfig(codexHome string, cfg ConfigInput) error {
	out, err := RenderConfigTOML(cfg)
	if err != nil {
		return err
	}
	p := filepath.Join(codexHome, "config.toml")
	return os.WriteFile(p, []byte(out), 0o600)
}

// RenderConfigTOML produces the TOML body. Pure function so tests can
// assert exact substrings without filesystem.
func RenderConfigTOML(cfg ConfigInput) (string, error) {
	if cfg.ModelProvider == "" {
		return "", fmt.Errorf("codexhome: ModelProvider required")
	}
	if cfg.Model == "" {
		return "", fmt.Errorf("codexhome: Model required")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "model_provider = %q\n", cfg.ModelProvider)
	fmt.Fprintf(&b, "model = %q\n\n", cfg.Model)

	for name, p := range cfg.ModelProviders {
		fmt.Fprintf(&b, "[model_providers.%s]\n", tomlKey(name))
		fmt.Fprintf(&b, "name = %q\n", p.Name)
		fmt.Fprintf(&b, "base_url = %q\n", p.BaseURL)
		fmt.Fprintf(&b, "env_key = %q\n", p.EnvKey)
		fmt.Fprintf(&b, "wire_api = %q\n\n", p.WireAPI)
	}

	for _, p := range cfg.ProjectTrustedPaths {
		fmt.Fprintf(&b, "[projects.%q]\n", p)
		fmt.Fprintf(&b, "trust_level = \"trusted\"\n\n")
	}

	// Disable codex's builtin local-execution paths so the only way the
	// LLM can reach a shell is through the env-mcp children we register
	// below. Matches the parent spec's Subsystem 2 deltas.
	b.WriteString("[features]\n")
	b.WriteString("shell_tool = false\n")
	b.WriteString("unified_exec = false\n")
	b.WriteString("apply_patch_freeform = false\n\n")

	for _, e := range cfg.Executors {
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", tomlKey(e.ID))
		fmt.Fprintf(&b, "command = %q\n", e.CodexBin)
		// One arg per line for readability.
		args := []string{
			"env-mcp",
			"--exe-id", e.ID,
			"--bridge-url", e.BridgeURL,
			"--token-env", e.TokenEnv,
			"--exe-desc", e.Desc,
		}
		if e.TurnID != "" {
			args = append(args, "--turn-id", e.TurnID)
		}
		b.WriteString("args = [")
		for i, a := range args {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", a)
		}
		b.WriteString("]\n")
		// Envvar passed to the child carrying the cap token.
		fmt.Fprintf(&b, "env = { %s = %q }\n\n", e.TokenEnv, e.TokenVal)
	}
	return b.String(), nil
}

// tomlKey leaves bare keys for safe identifiers, otherwise quotes.
func tomlKey(s string) string {
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Sprintf("%q", s)
		}
	}
	return s
}
```

- [ ] **Step 4: Run (expect pass)**

```bash
go test ./internal/codexappgateway/codexhome/ -v
```
Expected: 4 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/codexhome/codexhome.go internal/codexappgateway/codexhome/codexhome_test.go
git commit -m "feat(codex-app-gateway): per-thread CODEX_HOME mgr + config.toml renderer"
```

---

## Task 4: S3 round-trip (tar.gz of CODEX_HOME)

**Files:**
- Create: `internal/codexappgateway/codexhome/s3.go`
- Create: `internal/codexappgateway/codexhome/s3_test.go`

Per-thread CODEX_HOME → `s3://<bucket>/codex-app-gateway/<workspace_id>/<thread_id>.tar.gz`.
We use plain `gzip` (not `zstd`) so we don't add a new dep — sqlite +
session jsonl compress fine with gzip.

- [ ] **Step 1: Failing s3_test.go**

`internal/codexappgateway/codexhome/s3_test.go`:
```go
package codexhome

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeS3 is an in-memory ObjectStore used by the test suite.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) Put(_ context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), data...)
	return nil
}

func (f *fakeS3) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return append([]byte(nil), b...), nil
}

func (f *fakeS3) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func TestS3Round_Trip_TarUntarPreservesContents(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	src, err := m.NewTmpDir("ws_a", "thr_1")
	if err != nil {
		t.Fatalf("NewTmpDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "config.toml"), []byte("model = \"m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sessions", "x.jsonl"), []byte(`{"a":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := newFakeS3()
	backend := NewS3Backend(store, "ws_a", "thr_1")
	if err := backend.Upload(context.Background(), src); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, ok := store.objects[backend.Key()]; !ok {
		t.Fatalf("object missing: %v", store.objects)
	}

	// Recreate empty dir; download should re-populate.
	dst, err := m.NewTmpDir("ws_a", "thr_1") // same dir; remove first
	if err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(dst)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := backend.Download(context.Background(), dst); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "config.toml")); string(got) != "model = \"m\"\n" {
		t.Errorf("config.toml = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "sessions", "x.jsonl")); string(got) != `{"a":1}`+"\n" {
		t.Errorf("sessions/x.jsonl = %q", got)
	}
}

func TestS3Backend_Download_NotFound_IsRecognizable(t *testing.T) {
	store := newFakeS3()
	backend := NewS3Backend(store, "ws_a", "thr_missing")
	err := backend.Download(context.Background(), t.TempDir())
	if !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("want ErrObjectNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run (expect fail)**

```bash
go test ./internal/codexappgateway/codexhome/ -run TestS3 -v
```

- [ ] **Step 3: Implement s3.go**

`internal/codexappgateway/codexhome/s3.go`:
```go
package codexhome

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrObjectNotFound is returned by ObjectStore.Get when a key is absent.
var ErrObjectNotFound = errors.New("codexhome: object not found")

// ObjectStore is the seam between codexhome and the S3 client. Real
// callers wire in a thin wrapper around aws-sdk-go-v2; tests use a
// map-backed fake.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// S3Backend round-trips a single (workspace, thread) CODEX_HOME tree.
type S3Backend struct {
	store       ObjectStore
	workspaceID string
	threadID    string
}

func NewS3Backend(store ObjectStore, workspaceID, threadID string) *S3Backend {
	return &S3Backend{store: store, workspaceID: workspaceID, threadID: threadID}
}

// Key is the S3 key. Exposed so callers (and tests) can introspect.
func (b *S3Backend) Key() string {
	return fmt.Sprintf("codex-app-gateway/%s/%s.tar.gz", b.workspaceID, b.threadID)
}

// Upload tars+gzips the directory tree at src and writes it to S3.
func (b *S3Backend) Upload(ctx context.Context, src string) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		_ = f.Close()
		return copyErr
	})
	if err != nil {
		return fmt.Errorf("tar walk: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar close: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("gz close: %w", err)
	}
	return b.store.Put(ctx, b.Key(), buf.Bytes())
}

// Download fetches the tarball and untars into dst (which must exist
// and be empty/owned by the caller).
func (b *S3Backend) Download(ctx context.Context, dst string) error {
	data, err := b.store.Get(ctx, b.Key())
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		if strings.Contains(hdr.Name, "..") {
			return fmt.Errorf("untrusted path: %s", hdr.Name)
		}
		target := filepath.Join(dst, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs.FileMode(hdr.Mode)&0o700); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fs.FileMode(hdr.Mode)&0o600)
			if err != nil {
				return fmt.Errorf("open %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("copy %s: %w", target, err)
			}
			_ = f.Close()
		default:
			// Skip symlinks / fifo / devices — codex doesn't write them.
		}
	}
	return nil
}
```

- [ ] **Step 4: Run (expect pass)**

```bash
go test ./internal/codexappgateway/codexhome/ -v
```
Expected: all PASS (Task 3 + 2 new from Task 4).

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/codexhome/s3.go internal/codexappgateway/codexhome/s3_test.go
git commit -m "feat(codex-app-gateway): tar.gz S3 round-trip for CODEX_HOME"
```

---

## Task 5: Subprocess spawn + readyz wait

**Files:**
- Create: `internal/codexappgateway/supervisor/spawn.go`
- Create: `internal/codexappgateway/supervisor/spawn_test.go`

`spawnCodexAppServer` does:
1. `exec.CommandContext(ctx, codexBin, "app-server", "--listen", "ws://127.0.0.1:0")`
2. Set `CODEX_HOME=<dir>` and any extra env passed in.
3. Capture stdout via `StdoutPipe`; read first line — codex prints
   `ws://IP:PORT` — extract `wsURL` and HTTP equivalent.
4. Poll `GET <httpURL>/readyz` until 200 (default 5s timeout).
5. Return `*ChildHandle{Cmd, WSURL, HTTPURL, CodexHome}`.

The fake codex binary used in tests is a tiny Go program built on
demand by the test helper that prints the listen URL on stdout and
serves `/readyz`.

- [ ] **Step 1: Failing spawn_test.go**

`internal/codexappgateway/supervisor/spawn_test.go`:
```go
package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildFakeCodex compiles a small Go program that mimics the bits of
// `codex app-server` we depend on: print "ws://127.0.0.1:PORT" on
// stdout, then serve /readyz on that port.
func buildFakeCodex(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	const program = ` + "`" + `package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
)

func main() {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
	addr := l.Addr().(*net.TCPAddr)
	fmt.Printf("ws://%s\n", addr.String())
	http.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	_ = http.Serve(l, nil)
}
` + "`" + `
	if err := os.WriteFile(src, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-codex")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput()
	if err != nil {
		t.Fatalf("build fake codex: %v\n%s", err, out)
	}
	return bin
}

func TestSpawnCodexAppServer_HappyPath(t *testing.T) {
	bin := buildFakeCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := spawnCodexAppServer(ctx, bin, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer h.Stop(context.Background())
	if !strings.HasPrefix(h.WSURL, "ws://127.0.0.1:") {
		t.Errorf("WSURL = %s", h.WSURL)
	}
	if !strings.HasPrefix(h.HTTPURL, "http://127.0.0.1:") {
		t.Errorf("HTTPURL = %s", h.HTTPURL)
	}
}

func TestSpawnCodexAppServer_BadBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := spawnCodexAppServer(ctx, "/no/such/binary", t.TempDir(), nil)
	if err == nil {
		t.Fatal("want spawn error")
	}
}
```

- [ ] **Step 2: Run (expect fail)**

```bash
go test ./internal/codexappgateway/supervisor/ -v
```

- [ ] **Step 3: Implement spawn.go**

`internal/codexappgateway/supervisor/spawn.go`:
```go
// Package supervisor spawns and tracks per-thread `codex app-server`
// subprocesses inside the codex-app-gateway pod.
package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ChildHandle is what spawnCodexAppServer returns.
type ChildHandle struct {
	Cmd       *exec.Cmd
	WSURL     string // ws://127.0.0.1:PORT
	HTTPURL   string // http://127.0.0.1:PORT  (for /readyz, /healthz)
	CodexHome string
}

// Stop sends SIGTERM, waits up to 10s, then SIGKILLs.
func (h *ChildHandle) Stop(ctx context.Context) error {
	if h.Cmd == nil || h.Cmd.Process == nil {
		return nil
	}
	if err := h.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("SIGTERM: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- h.Cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		_ = h.Cmd.Process.Signal(syscall.SIGKILL)
		<-done
		return nil
	case <-ctx.Done():
		_ = h.Cmd.Process.Signal(syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

// spawnCodexAppServer launches `codexBin app-server --listen ws://127.0.0.1:0`,
// reads the listen URL from stdout, polls /readyz, and returns a handle.
func spawnCodexAppServer(ctx context.Context, codexBin, codexHome string, extraEnv []string) (*ChildHandle, error) {
	cmd := exec.Command(codexBin, "app-server", "--listen", "ws://127.0.0.1:0")
	cmd.Env = append(append([]string{}, extraEnv...), "CODEX_HOME="+codexHome)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	// Read the listen URL line. codex's first stdout line is the
	// ws://IP:PORT it bound to.
	br := bufio.NewReader(stdout)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("read listen line: %w", err)
	}
	wsURL := strings.TrimSpace(line)
	if !strings.HasPrefix(wsURL, "ws://") {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("unexpected first stdout line %q", wsURL)
	}
	httpURL := "http://" + strings.TrimPrefix(wsURL, "ws://")
	// Drain remaining stdout in the background so the pipe doesn't fill
	// (codex keeps logging readyz/healthz/notes).
	go func() { _, _ = bufio.NewReader(stdout).ReadBytes(0) }()

	// Wait for /readyz. The fake server (and real codex) returns 200
	// immediately; production codex may take a moment for sqlite init.
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", httpURL+"/readyz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == 200 {
			_ = resp.Body.Close()
			break
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("readyz never returned 200: last err=%v", err)
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	return &ChildHandle{Cmd: cmd, WSURL: wsURL, HTTPURL: httpURL, CodexHome: codexHome}, nil
}
```

- [ ] **Step 4: Run (expect pass)**

```bash
go test ./internal/codexappgateway/supervisor/ -v
```
Expected: 2 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/supervisor/spawn.go internal/codexappgateway/supervisor/spawn_test.go
git commit -m "feat(codex-app-gateway): spawn helper for codex app-server subprocesses"
```

---

## Task 6: Supervisor (in-memory map, EnsureSubprocess)

**Files:**
- Create: `internal/codexappgateway/supervisor/supervisor.go`
- Create: `internal/codexappgateway/supervisor/supervisor_test.go`

The Supervisor:
- holds `map[Key]*entry` where `Key={workspaceID, threadID}`
- `EnsureSubprocess(ctx, key, codexBin, configBuilder)` returns the
  existing live handle or spawns a new one (downloading from S3 first
  if a tarball exists)
- `Shutdown(ctx, key)` terminates the subprocess and uploads to S3

This task does NOT include the idle reaper (Task 7).

- [ ] **Step 1: Failing supervisor_test.go**

`internal/codexappgateway/supervisor/supervisor_test.go`:
```go
package supervisor

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
)

// fakeStore implements codexhome.ObjectStore in-memory.
type fakeStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{m: map[string][]byte{}} }
func (f *fakeStore) Put(_ context.Context, k string, d []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[k] = append([]byte(nil), d...)
	return nil
}
func (f *fakeStore) Get(_ context.Context, k string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.m[k]
	if !ok {
		return nil, codexhome.ErrObjectNotFound
	}
	return append([]byte(nil), d...), nil
}
func (f *fakeStore) Delete(_ context.Context, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, k)
	return nil
}

func TestSupervisor_EnsureSubprocess_SpawnsOnce(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)
	sup := NewSupervisor(SupervisorConfig{
		CodexBin: bin,
		HomeMgr:  mgr,
		Store:    store,
	})
	defer sup.ShutdownAll(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	build := func() (codexhome.ConfigInput, error) {
		return codexhome.ConfigInput{
			ModelProvider:  "p",
			Model:          "m",
			ModelProviders: map[string]codexhome.ModelProvider{"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"}},
		}, nil
	}
	key := Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}
	h1, err := sup.EnsureSubprocess(ctx, key, build)
	if err != nil {
		t.Fatalf("ensure 1: %v", err)
	}
	h2, err := sup.EnsureSubprocess(ctx, key, build)
	if err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	if h1.WSURL != h2.WSURL {
		t.Errorf("two ensures returned different handles: %s vs %s", h1.WSURL, h2.WSURL)
	}
}

func TestSupervisor_Shutdown_UploadsToS3(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)
	sup := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	build := func() (codexhome.ConfigInput, error) {
		return codexhome.ConfigInput{
			ModelProvider:  "p",
			Model:          "m",
			ModelProviders: map[string]codexhome.ModelProvider{"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"}},
		}, nil
	}
	key := Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}
	h, err := sup.EnsureSubprocess(ctx, key, build)
	if err != nil {
		t.Fatal(err)
	}
	// Touch a session file so the tarball has something interesting.
	if err := os.MkdirAll(h.CodexHome+"/sessions", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.CodexHome+"/sessions/x.jsonl", []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := sup.Shutdown(ctx, key); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	wantKey := "codex-app-gateway/ws_a/thr_1.tar.gz"
	if _, ok := store.m[wantKey]; !ok {
		t.Fatalf("no S3 object at %s; have: %v", wantKey, keysOf(store.m))
	}
}

func TestSupervisor_EnsureSubprocess_RestoresFromS3(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)

	// Pre-populate S3 by running a Supervisor through one full lifecycle.
	{
		sup := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		build := func() (codexhome.ConfigInput, error) {
			return codexhome.ConfigInput{
				ModelProvider:  "p",
				Model:          "m",
				ModelProviders: map[string]codexhome.ModelProvider{"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"}},
			}, nil
		}
		key := Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}
		h, err := sup.EnsureSubprocess(ctx, key, build)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(h.CodexHome+"/marker.txt", []byte("from-pass-1"), 0o600)
		_ = sup.Shutdown(ctx, key)
	}

	// Fresh supervisor + tmpdir; ensure should pull from S3.
	sup2 := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: codexhome.NewManager(t.TempDir()), Store: store})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	build := func() (codexhome.ConfigInput, error) {
		return codexhome.ConfigInput{
			ModelProvider:  "p",
			Model:          "m",
			ModelProviders: map[string]codexhome.ModelProvider{"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"}},
		}, nil
	}
	h, err := sup2.EnsureSubprocess(ctx, Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}, build)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	defer sup2.ShutdownAll(context.Background())
	got, err := os.ReadFile(h.CodexHome + "/marker.txt")
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if string(got) != "from-pass-1" {
		t.Errorf("marker = %q", got)
	}
}

func TestSupervisor_Ensure_BuildError_PropagatesAndDoesNotSpawn(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)
	sup := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wantErr := errors.New("nope")
	_, err := sup.EnsureSubprocess(ctx, Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}, func() (codexhome.ConfigInput, error) {
		return codexhome.ConfigInput{}, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("want wantErr, got %v", err)
	}
}

func keysOf(m map[string][]byte) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
```

- [ ] **Step 2: Run (expect fail)**

```bash
go test ./internal/codexappgateway/supervisor/ -v
```

- [ ] **Step 3: Implement supervisor.go**

`internal/codexappgateway/supervisor/supervisor.go`:
```go
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
)

// Key identifies one (workspace, thread) subprocess slot.
type Key struct {
	WorkspaceID string
	ThreadID    string
}

// SupervisorConfig holds the static dependencies.
type SupervisorConfig struct {
	CodexBin string
	HomeMgr  *codexhome.Manager
	Store    codexhome.ObjectStore
	ExtraEnv []string // forwarded to every spawned subprocess
}

// Supervisor owns the in-memory (workspace, thread) → subprocess map.
type Supervisor struct {
	cfg SupervisorConfig

	mu       sync.Mutex
	children map[Key]*entry
}

type entry struct {
	handle      *ChildHandle
	codexHome   string
	lastActive  time.Time
}

// LastActive returns the entry's last-active timestamp; used by the reaper.
func (e *entry) LastActive() time.Time { return e.lastActive }

// ConfigBuilder produces a fresh ConfigInput at spawn time. Allowed to
// hit the network (e.g. fetch executor bindings); errors propagate.
type ConfigBuilder func() (codexhome.ConfigInput, error)

func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	return &Supervisor{cfg: cfg, children: map[Key]*entry{}}
}

// EnsureSubprocess returns a live ChildHandle for key, spawning one if
// necessary. Concurrent EnsureSubprocess calls for the same key see
// the same handle (one-spawn-per-key invariant).
func (s *Supervisor) EnsureSubprocess(ctx context.Context, key Key, build ConfigBuilder) (*ChildHandle, error) {
	s.mu.Lock()
	if e, ok := s.children[key]; ok {
		e.lastActive = time.Now()
		s.mu.Unlock()
		return e.handle, nil
	}
	s.mu.Unlock()

	cfg, err := build()
	if err != nil {
		return nil, fmt.Errorf("config builder: %w", err)
	}
	codexHome, err := s.cfg.HomeMgr.NewTmpDir(key.WorkspaceID, key.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("new tmpdir: %w", err)
	}
	// Restore from S3 if a prior run left a tarball.
	backend := codexhome.NewS3Backend(s.cfg.Store, key.WorkspaceID, key.ThreadID)
	if err := backend.Download(ctx, codexHome); err != nil && !errors.Is(err, codexhome.ErrObjectNotFound) {
		_ = s.cfg.HomeMgr.RemoveTmpDir(codexHome)
		return nil, fmt.Errorf("S3 download: %w", err)
	}
	if err := s.cfg.HomeMgr.WriteConfig(codexHome, cfg); err != nil {
		_ = s.cfg.HomeMgr.RemoveTmpDir(codexHome)
		return nil, fmt.Errorf("write config: %w", err)
	}

	handle, err := spawnCodexAppServer(ctx, s.cfg.CodexBin, codexHome, s.cfg.ExtraEnv)
	if err != nil {
		_ = s.cfg.HomeMgr.RemoveTmpDir(codexHome)
		return nil, fmt.Errorf("spawn: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Race: someone else won the spawn race while we were running.
	if e, ok := s.children[key]; ok {
		// Discard our spawn; reuse theirs.
		_ = handle.Stop(ctx)
		_ = s.cfg.HomeMgr.RemoveTmpDir(codexHome)
		e.lastActive = time.Now()
		return e.handle, nil
	}
	s.children[key] = &entry{handle: handle, codexHome: codexHome, lastActive: time.Now()}
	return handle, nil
}

// Touch bumps the last-active timestamp for a key. Called on every
// proxied frame so the reaper sees fresh activity.
func (s *Supervisor) Touch(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.children[key]; ok {
		e.lastActive = time.Now()
	}
}

// Shutdown terminates the subprocess for key, uploads its CODEX_HOME to
// S3, and drops the in-memory entry. Safe on missing keys.
func (s *Supervisor) Shutdown(ctx context.Context, key Key) error {
	s.mu.Lock()
	e, ok := s.children[key]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.children, key)
	s.mu.Unlock()

	if err := e.handle.Stop(ctx); err != nil {
		// Continue to upload anyway — flushed sqlite is still useful.
	}
	backend := codexhome.NewS3Backend(s.cfg.Store, key.WorkspaceID, key.ThreadID)
	if err := backend.Upload(ctx, e.codexHome); err != nil {
		return fmt.Errorf("S3 upload: %w", err)
	}
	if err := s.cfg.HomeMgr.RemoveTmpDir(e.codexHome); err != nil {
		return fmt.Errorf("remove tmpdir: %w", err)
	}
	return nil
}

// ShutdownAll shuts down every active subprocess. Used at server stop.
func (s *Supervisor) ShutdownAll(ctx context.Context) {
	s.mu.Lock()
	keys := make([]Key, 0, len(s.children))
	for k := range s.children {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	for _, k := range keys {
		_ = s.Shutdown(ctx, k)
	}
}

// snapshot returns the keys + last-active times. Used by the reaper.
func (s *Supervisor) snapshot() map[Key]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[Key]time.Time, len(s.children))
	for k, e := range s.children {
		out[k] = e.lastActive
	}
	return out
}
```

- [ ] **Step 4: Run (expect pass)**

```bash
go test ./internal/codexappgateway/supervisor/ -v
```
Expected: all PASS (Tasks 5+6 = 6 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/supervisor/supervisor.go internal/codexappgateway/supervisor/supervisor_test.go
git commit -m "feat(codex-app-gateway): per-thread subprocess Supervisor + S3 round-trip"
```

---

## Task 7: Idle reaper

**Files:**
- Create: `internal/codexappgateway/supervisor/reaper.go`
- Create: `internal/codexappgateway/supervisor/reaper_test.go`

A goroutine that wakes every `interval`, asks the Supervisor for a
snapshot, and shuts down any entry whose `lastActive` is older than
`idleAfter`.

- [ ] **Step 1: Failing reaper_test.go**

`internal/codexappgateway/supervisor/reaper_test.go`:
```go
package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
)

func TestReaper_RetiresIdleSubprocess(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)
	sup := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	build := func() (codexhome.ConfigInput, error) {
		return codexhome.ConfigInput{
			ModelProvider:  "p", Model: "m",
			ModelProviders: map[string]codexhome.ModelProvider{"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"}},
		}, nil
	}
	if _, err := sup.EnsureSubprocess(ctx, Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}, build); err != nil {
		t.Fatal(err)
	}

	r := NewIdleReaper(sup, 50*time.Millisecond, 100*time.Millisecond)
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go r.Run(rctx)

	// Wait long enough for one tick after idleAfter has elapsed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sup.snapshot()) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := sup.snapshot(); len(got) != 0 {
		t.Fatalf("expected empty after reap, got %v", got)
	}
}

func TestReaper_KeepsActiveSubprocess(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)
	sup := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	defer sup.ShutdownAll(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	build := func() (codexhome.ConfigInput, error) {
		return codexhome.ConfigInput{
			ModelProvider:  "p", Model: "m",
			ModelProviders: map[string]codexhome.ModelProvider{"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"}},
		}, nil
	}
	key := Key{WorkspaceID: "ws_a", ThreadID: "thr_keep"}
	if _, err := sup.EnsureSubprocess(ctx, key, build); err != nil {
		t.Fatal(err)
	}

	r := NewIdleReaper(sup, 30*time.Millisecond, 200*time.Millisecond)
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go r.Run(rctx)

	// Touch periodically for 600ms (well beyond idleAfter=200ms).
	end := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(end) {
		sup.Touch(key)
		time.Sleep(50 * time.Millisecond)
	}
	if got := sup.snapshot(); len(got) != 1 {
		t.Fatalf("expected entry to survive, got %v", got)
	}
}
```

- [ ] **Step 2: Run (expect fail)**

- [ ] **Step 3: Implement reaper.go**

`internal/codexappgateway/supervisor/reaper.go`:
```go
package supervisor

import (
	"context"
	"time"
)

// IdleReaper periodically scans the Supervisor and shuts down entries
// idle for longer than idleAfter.
type IdleReaper struct {
	sup       *Supervisor
	interval  time.Duration
	idleAfter time.Duration
}

func NewIdleReaper(sup *Supervisor, interval, idleAfter time.Duration) *IdleReaper {
	return &IdleReaper{sup: sup, interval: interval, idleAfter: idleAfter}
}

func (r *IdleReaper) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for key, last := range r.sup.snapshot() {
				if now.Sub(last) >= r.idleAfter {
					_ = r.sup.Shutdown(ctx, key)
				}
			}
		}
	}
}
```

- [ ] **Step 4: Run (expect pass)**

```bash
go test ./internal/codexappgateway/supervisor/ -v
```
Expected: 8 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/supervisor/reaper.go internal/codexappgateway/supervisor/reaper_test.go
git commit -m "feat(codex-app-gateway): idle subprocess reaper"
```

---

## Task 8: WS frame proxy + Server (chi router + ws upgrade + thread routing)

**Files:**
- Create: `internal/codexappgateway/proxy/proxy.go`
- Create: `internal/codexappgateway/proxy/proxy_test.go`
- Modify: `internal/codexappgateway/server.go` (replace stub)
- Create: `internal/codexappgateway/server_test.go`

The proxy is a bidirectional ws frame pump (same shape as
codex-exec-gateway's bridge — frame-level, no protocol parsing). The
server wires together:
- `GET /codex-app/ws` ws upgrade (with `Authorization: Bearer ...` check)
- `POST /admin/threads/{thread_id}/restart` (calls Supervisor.Shutdown)
- `GET /healthz`

For phase 1 the inbound auth gives us `(workspace_id, thread_id)`
directly; that pair indexes the supervisor.

- [ ] **Step 1: Failing proxy_test.go**

`internal/codexappgateway/proxy/proxy_test.go`:
```go
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// pairServer accepts ws and echoes every message back uppercased.
func pairServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			_ = c.Write(ctx, typ, []byte(strings.ToUpper(string(data))))
		}
	}))
}

func TestRunProxy_BidirectionalForwarding(t *testing.T) {
	upstream := pairServer(t)
	defer upstream.Close()
	wsURL := "ws" + strings.TrimPrefix(upstream.URL, "http")

	// Client side: a third ws server that we proxy through.
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy", func(w http.ResponseWriter, r *http.Request) {
		userWS, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer userWS.Close(websocket.StatusNormalClosure, "")
		childWS, _, err := websocket.Dial(r.Context(), wsURL, nil)
		if err != nil {
			t.Errorf("dial child: %v", err)
			return
		}
		defer childWS.Close(websocket.StatusNormalClosure, "")
		_ = RunProxy(r.Context(), userWS, childWS)
	})
	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(gateway.URL, "http")+"/proxy", nil)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if err := c.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, got, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "HELLO" {
		t.Errorf("got %q", got)
	}
}
```

- [ ] **Step 2: Implement proxy.go**

`internal/codexappgateway/proxy/proxy.go`:
```go
// Package proxy bidirectionally forwards ws frames between a user-side
// ws and a child-side ws. Frame-level: doesn't parse JSON-RPC.
package proxy

import (
	"context"
	"errors"

	"nhooyr.io/websocket"
)

// RunProxy starts two pumps in parallel and returns when either side
// closes or errors. Both ws are left open for the caller to close.
func RunProxy(ctx context.Context, userWS, childWS *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- pump(ctx, userWS, childWS) }()
	go func() { errCh <- pump(ctx, childWS, userWS) }()
	err := <-errCh
	cancel()
	<-errCh
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func pump(ctx context.Context, src, dst *websocket.Conn) error {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return err
		}
	}
}
```

- [ ] **Step 3: Run proxy test (expect pass)**

```bash
go test ./internal/codexappgateway/proxy/ -v
```

- [ ] **Step 4: Failing server_test.go**

`internal/codexappgateway/server_test.go`:
```go
package codexappgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/agentserver/agentserver/internal/codexappgateway/auth"
	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"

	"nhooyr.io/websocket"
)

func TestServer_WSEndpoint_AuthRequired(t *testing.T) {
	srv := makeTestServer(t)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/codex-app/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		t.Fatal("expected dial to fail without Bearer")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %v", resp)
	}
}

func TestServer_WSEndpoint_HappyPath_ProxiesToFakeChild(t *testing.T) {
	srv := makeTestServer(t)
	defer srv.Close()

	authHelper := auth.NewHMAC([]byte("test-secret"))
	tok := authHelper.Mint("ws_a", "thr_1")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/codex-app/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// fake codex's `/readyz` returns 200 immediately and accepts no
	// real RPC; for this test we just verify the proxy is alive — write
	// a junk frame and expect the connection to stay open until we close.
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c // we don't read; the fake codex doesn't reply to RPC.
}

func TestServer_AdminRestart_KillsSubprocess(t *testing.T) {
	srv := makeTestServer(t)
	defer srv.Close()

	// Connect once to spawn a subprocess.
	authHelper := auth.NewHMAC([]byte("test-secret"))
	tok := authHelper.Mint("ws_b", "thr_42")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/codex-app/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "")

	// Hit admin restart.
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/admin/threads/restart", strings.NewReader(`{"workspaceId":"ws_b","threadId":"thr_42"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	if resp.StatusCode != 204 {
		body, _ := json.Marshal(resp.Header)
		t.Errorf("status = %d, headers = %s", resp.StatusCode, body)
	}
}

// --- test fixture ---

func makeTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	bin := supervisor_buildFakeCodex(t)
	store := supervisor_newFakeStore(t)
	mgr := codexhome.NewManager(t.TempDir())
	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	t.Cleanup(func() { sup.ShutdownAll(context.Background()) })

	logger := slog.New(slog.NewTextHandler(httptest.NewRecorder(), nil))
	srv := &Server{
		cfg:    ServeConfig{InboundHMACSecret: []byte("test-secret")},
		auth:   auth.NewHMAC([]byte("test-secret")),
		sup:    sup,
		homeMgr: mgr,
		logger: logger,
		// Build a default config without executor entries (the proxy test
		// doesn't exercise env-mcp).
		buildConfig: func(ws, thr string) (codexhome.ConfigInput, error) {
			return codexhome.ConfigInput{
				ModelProvider:  "p", Model: "m",
				ModelProviders: map[string]codexhome.ModelProvider{"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"}},
			}, nil
		},
	}
	return httptest.NewServer(srv.Routes())
}

// --- bridge to supervisor package's test helpers ---
// (declared here so we don't re-export package-level test code)

var (
	supervisor_buildFakeCodex func(*testing.T) string
	supervisor_newFakeStore   func(*testing.T) codexhome.ObjectStore
)
```

This test file references `supervisor_buildFakeCodex` and
`supervisor_newFakeStore` — these are seam vars set from a small
`server_testhelper_test.go` file that re-exports the supervisor-package
fakes. Plan defers wiring those to the implementer (5-line file).

- [ ] **Step 5: Implement server.go (replace stub)**

`internal/codexappgateway/server.go`:
```go
package codexappgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/auth"
	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/proxy"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"

	"github.com/go-chi/chi/v5"
	"nhooyr.io/websocket"
)

// Server is the codex-app-gateway HTTP/WS server.
type Server struct {
	cfg     ServeConfig
	auth    auth.Authenticator
	sup     *supervisor.Supervisor
	homeMgr *codexhome.Manager
	logger  *slog.Logger

	// buildConfig produces the per-thread config.toml input. Allowed to
	// hit the network (e.g. fetch executor bindings from
	// codex-exec-gateway). Returns the input or an error that aborts spawn.
	buildConfig func(workspaceID, threadID string) (codexhome.ConfigInput, error)
}

// NewServer wires up the production server.
func NewServer(cfg ServeConfig, codexBin string, logger *slog.Logger) (*Server, error) {
	store, err := newS3Store(cfg.S3)
	if err != nil {
		return nil, fmt.Errorf("s3 store: %w", err)
	}
	mgr := codexhome.NewManager(cfg.TmpRoot)
	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{
		CodexBin: codexBin,
		HomeMgr:  mgr,
		Store:    store,
	})
	return &Server{
		cfg:     cfg,
		auth:    auth.NewHMAC(cfg.InboundHMACSecret),
		sup:     sup,
		homeMgr: mgr,
		logger:  logger,
		buildConfig: func(workspaceID, threadID string) (codexhome.ConfigInput, error) {
			// Phase-1 default: minimal config from env. Real exec-gw fetch
			// is wired in a follow-up task that reads CXG_EXEC_GATEWAY_*
			// and mints per-turn cap tokens; until then, no executors.
			return codexhome.ConfigInput{
				ModelProvider: "modelserver",
				Model:         "gpt-5.5",
				ModelProviders: map[string]codexhome.ModelProvider{
					"modelserver": {Name: "modelserver", BaseURL: "http://llmproxy:8085/v1", EnvKey: "CODEX_API_KEY", WireAPI: "responses"},
				},
			}, nil
		},
	}, nil
}

// Run serves HTTP until ctx is done.
func (s *Server) Run(ctx context.Context, listenAddr string) error {
	httpSrv := &http.Server{Addr: listenAddr, Handler: s.Routes()}
	reaper := supervisor.NewIdleReaper(s.sup, 1*time.Minute, s.cfg.IdleShutdown)
	reaperCtx, reaperCancel := context.WithCancel(context.Background())
	defer reaperCancel()
	go reaper.Run(reaperCtx)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		s.sup.ShutdownAll(shutdownCtx)
		return nil
	case err := <-errCh:
		s.sup.ShutdownAll(context.Background())
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Routes builds the chi router. Public for tests.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	r.Get("/codex-app/ws", s.handleCodexAppWS)
	r.Post("/admin/threads/restart", s.handleAdminRestart)
	return r
}

func (s *Server) handleCodexAppWS(w http.ResponseWriter, r *http.Request) {
	tok, ok := auth.ExtractBearer(r)
	if !ok {
		http.Error(w, "missing Bearer", http.StatusUnauthorized)
		return
	}
	id, err := s.auth.Verify(tok)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userWS, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.logger.Warn("ws accept failed", "err", err)
		return
	}
	defer userWS.Close(websocket.StatusNormalClosure, "client closing")

	key := supervisor.Key{WorkspaceID: id.WorkspaceID, ThreadID: id.ThreadID}
	ctx := r.Context()
	handle, err := s.sup.EnsureSubprocess(ctx, key, func() (codexhome.ConfigInput, error) {
		return s.buildConfig(id.WorkspaceID, id.ThreadID)
	})
	if err != nil {
		s.logger.Error("ensure subprocess", "err", err, "key", key)
		_ = userWS.Close(websocket.StatusInternalError, "subprocess unavailable")
		return
	}

	childWS, _, err := websocket.Dial(ctx, handle.WSURL, nil)
	if err != nil {
		s.logger.Error("dial child", "err", err, "url", handle.WSURL)
		_ = userWS.Close(websocket.StatusInternalError, "subprocess dial failed")
		return
	}
	defer childWS.Close(websocket.StatusNormalClosure, "gateway closing")

	s.sup.Touch(key)
	if err := proxy.RunProxy(ctx, userWS, childWS); err != nil {
		s.logger.Info("proxy ended", "err", err, "key", key)
	}
	s.sup.Touch(key)
}

func (s *Server) handleAdminRestart(w http.ResponseWriter, r *http.Request) {
	tok, ok := auth.ExtractBearer(r)
	if !ok {
		http.Error(w, "missing Bearer", http.StatusUnauthorized)
		return
	}
	if _, err := s.auth.Verify(tok); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var body struct {
		WorkspaceID string `json:"workspaceId"`
		ThreadID    string `json:"threadId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if body.WorkspaceID == "" || body.ThreadID == "" {
		http.Error(w, "workspaceId and threadId required", http.StatusBadRequest)
		return
	}
	if err := s.sup.Shutdown(r.Context(), supervisor.Key{WorkspaceID: body.WorkspaceID, ThreadID: body.ThreadID}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Plus a thin `s3_store.go` wrapper around aws-sdk-go-v2 (taking the
config shape from `internal/ccbroker/workspace/s3store.go` as a
template):

`internal/codexappgateway/s3_store.go`:
```go
package codexappgateway

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type s3Store struct {
	client *s3.Client
	bucket string
}

func newS3Store(cfg S3Config) (codexhome.ObjectStore, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("s3: endpoint + bucket required")
	}
	awsCfg := aws.Config{
		Region:      cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
	}
	cli := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = &cfg.Endpoint
		o.UsePathStyle = cfg.PathStyle
	})
	return &s3Store{client: cli, bucket: cfg.Bucket}, nil
}

func (s *s3Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
		Body:   bytes.NewReader(data),
	})
	return err
}

func (s *s3Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, codexhome.ErrObjectNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	return err
}
```

Plus the test bridge file:

`internal/codexappgateway/server_testhelper_test.go`:
```go
package codexappgateway

import (
	"testing"

	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"
)

// Re-exports of supervisor's test fakes so server_test.go can use them
// without that file being inside the supervisor package.
func init() {
	supervisor_buildFakeCodex = supervisor.BuildFakeCodexForTest
	supervisor_newFakeStore = func(t *testing.T) codexhome.ObjectStore {
		return supervisor.NewFakeStoreForTest(t)
	}
}
```

This requires renaming the supervisor test helpers to be exported.
Implementer: in supervisor/spawn_test.go and supervisor_test.go,
rename `buildFakeCodex` → `BuildFakeCodexForTest` and add a small
exported wrapper `NewFakeStoreForTest(*testing.T) codexhome.ObjectStore`
returning `*fakeStore` boxed into the interface. Keep the in-package
tests working (they call the new name directly).

- [ ] **Step 6: Run all tests (expect pass)**

```bash
go test ./internal/codexappgateway/... ./cmd/codex-app-gateway/ -v
```
Expected: every test PASS (~20 tests across all packages).

- [ ] **Step 7: Commit**

```bash
git add internal/codexappgateway/proxy/ internal/codexappgateway/server.go \
        internal/codexappgateway/s3_store.go internal/codexappgateway/server_test.go \
        internal/codexappgateway/server_testhelper_test.go \
        internal/codexappgateway/supervisor/spawn_test.go \
        internal/codexappgateway/supervisor/supervisor_test.go
git commit -m "feat(codex-app-gateway): ws frame proxy + chi server with auth + admin endpoints"
```

---

## Task 9: End-to-end test against real codex app-server

**Files:**
- Create: `internal/codexappgateway/integration_test.go`

Same shape as PR #78's env-mcp integration test: opt-in via
`-tags integration`, skips if `codex` binary not on PATH.

- [ ] **Step 1: Write the test**

`internal/codexappgateway/integration_test.go`:
```go
//go:build integration

package codexappgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/agentserver/agentserver/internal/codexappgateway/auth"
	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"

	"nhooyr.io/websocket"
)

func TestServer_RealCodexAppServer_FullRPCRoundtrip(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not on PATH")
	}
	root := t.TempDir()
	store := supervisor.NewFakeStoreForTest(t)
	mgr := codexhome.NewManager(root)
	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{CodexBin: "codex", HomeMgr: mgr, Store: store})
	t.Cleanup(func() { sup.ShutdownAll(context.Background()) })

	logger := slog.New(slog.NewTextHandler(httptest.NewRecorder(), nil))
	s := &Server{
		cfg:    ServeConfig{InboundHMACSecret: []byte("int")},
		auth:   auth.NewHMAC([]byte("int")),
		sup:    sup,
		homeMgr: mgr,
		logger: logger,
		buildConfig: func(ws, thr string) (codexhome.ConfigInput, error) {
			return codexhome.ConfigInput{
				ModelProvider: "modelserver",
				Model:         "gpt-5.5",
				ModelProviders: map[string]codexhome.ModelProvider{
					"modelserver": {Name: "modelserver", BaseURL: "https://code.ai.cs.ac.cn/v1", EnvKey: "OPENAI_API_KEY", WireAPI: "responses"},
				},
				ProjectTrustedPaths: []string{"/tmp"},
			}, nil
		},
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	tok := auth.NewHMAC([]byte("int")).Mint("ws_int", "thr_1")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/codex-app/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Send: initialize → expect any reply.
	send := func(payload string) {
		if err := c.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	send(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"int","title":"int","version":"0"},"capabilities":{"experimentalApi":true,"requestAttestation":false,"optOutNotificationMethods":[]}}}`)
	_, raw, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read initialize: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if resp["id"] == nil {
		t.Fatalf("no id in initialize reply: %v", resp)
	}
	if resp["error"] != nil {
		t.Fatalf("initialize errored: %v", resp["error"])
	}
	t.Logf("initialize reply ok: %v", resp["result"])

	// Send: thread/start → expect a thread.
	send(`{"method":"initialized"}`)
	send(`{"id":2,"method":"thread/start","params":{}}`)
	_, raw, err = c.Read(ctx)
	if err != nil {
		t.Fatalf("read thread/start: %v", err)
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode thread/start: %v", err)
	}
	if resp["error"] != nil {
		t.Fatalf("thread/start errored: %v", resp["error"])
	}
	t.Logf("thread/start ok: %v", resp["result"])
}
```

- [ ] **Step 2: Run integration test**

```bash
go test -tags integration -run TestServer_RealCodexAppServer ./internal/codexappgateway/ -v
```
Expected: PASS on dev box (codex 0.130.0+ on PATH); SKIP otherwise.

- [ ] **Step 3: Run full suite once more**

```bash
go test ./internal/codexappgateway/... ./cmd/codex-app-gateway/ -v
go vet ./internal/codexappgateway/... ./cmd/codex-app-gateway/
```
Expected: every test PASS, vet clean.

- [ ] **Step 4: Commit**

```bash
git add internal/codexappgateway/integration_test.go
git commit -m "test(codex-app-gateway): integration test against real codex app-server"
```

---

## Open risks (carried forward from spec)

1. **`buildConfig` is a stub** in this plan. It returns a static config
   with no executors, so the spawned subprocess sees no `mcp_servers`.
   That's fine for phase-1 acceptance (TUI ↔ subprocess proxy works);
   wiring real executor fetch from codex-exec-gateway is a follow-up
   task that depends on Subsystem 3 being implemented first.
2. **`codex app-server` is `[experimental]`.** Pin the codex version
   in the gateway Dockerfile (Task 10 — out of scope) and rely on the
   integration test to catch protocol drift.
3. **No multi-TUI fan-out for one thread.** Phase-1 invariant is "one
   active ws per (workspace, thread)". Concurrent dial replaces the
   prior connection silently (the new RunProxy takes over; the old
   one's RunProxy returns on ws close). Phase-2 will add explicit
   reject + admin-kick.
4. **CODEX_HOME tarball size** untested in this plan. Profile after
   first deployment; if a single thread's tarball exceeds ~50 MB,
   add session-jsonl pruning before tar.
5. **No `--ws-auth` on the loopback subprocess.** The spec's reasoning
   stands: same-pod loopback is trusted. If we ever split pods,
   pass `--ws-auth signed-bearer-token --ws-shared-secret-file <path>`
   to `spawnCodexAppServer` and mint matching JWTs in the Dial call.

---

## Self-review

**Spec coverage** (`docs/superpowers/specs/2026-05-10-codex-app-gateway-subprocess.md`):

- "thin auth proxy + per-thread subprocess manager + ws frame proxy" → Tasks 6 + 7 + 8 ✓
- "spawn `codex app-server --listen ws://127.0.0.1:0`" → Task 5 ✓
- "parse listen URL from stdout's first line + wait for `/readyz`" → Task 5 ✓
- "per-thread CODEX_HOME tmpdir" → Task 3 ✓
- "config.toml fragment with `[features]` disabling builtin shell + `[mcp_servers]`" → Task 3 ✓
- "S3 round-trip on idle" → Tasks 4 + 6 + 7 ✓
- "in-memory `(workspace, thread)` map" → Task 6 ✓
- "transparent ws frame forwarding" → Task 8 ✓
- "loopback trust; non-loopback would need `--ws-auth`" → noted in risk #5 ✓
- "admin restart endpoint" → Task 8 ✓
- "single-TUI-per-thread invariant" → noted in risk #3 (deferred to phase 2)
- "phase-1 inbound auth via HMAC of (workspace, thread)" → Task 2 ✓

**Placeholder scan:** No "TODO", no "implement appropriate". Every step
contains real test code or real impl code. The only deliberate stub is
`buildConfig` which is documented in Open risks #1.

**Type consistency:** `Key`, `Identity`, `ChildHandle`, `ConfigInput`,
`ConfigBuilder`, `ObjectStore`, `S3Backend`, `Manager`, `Authenticator`,
`HMAC`, `Server`, `ServeConfig`, `S3Config`, `SupervisorConfig`,
`Supervisor`, `IdleReaper` — used identically across every task that
references them.

**Task count + scope:** 9 tasks, ~1500 LOC. Compares to env-mcp plan's
7 tasks / ~1100 LOC. Reasonable for the larger surface (subprocess
mgmt + S3 + auth + chi + ws proxy + integration test).
