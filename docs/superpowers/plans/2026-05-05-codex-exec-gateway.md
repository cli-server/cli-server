# codex-exec-gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the new `codex-exec-gateway` Go service: a transparent
WebSocket frame-level forwarder that pairs inbound `codex exec-server
--connect` executor connections (`/codex-exec/{exe_id}`) with bridge
connections from spawned codex subprocesses (`/bridge/{exe_id}`),
authenticates each side once at connect (bcrypt tunnel token / HMAC
capability token), exposes admin + internal HTTP endpoints, and owns the
`executors` and `workspace_executors` Postgres tables.

**Architecture:** Single Go binary at `cmd/codex-exec-gateway`. Package
`internal/codexexecgateway` follows the same shape as
`internal/executorregistry`: `chi` router + lifecycle in `server.go`,
`store.go` for Postgres CRUD with embedded migrations, per-handler files
under `handlers/`, plus three new pieces unique to this service —
`auth.go` (HMAC capability-token verify), `registry.go` (in-memory
`exe_id → ws conn` map with single-conn-per-id eviction), `revocation.go`
(in-memory revoked turn_id set, capped + exp-pruned). Forwarding is
strictly **frame-level** (`ws.Read` → `ws.Write` per frame) — never byte
concatenation — so JSON-RPC envelope boundaries are preserved without
parsing them. Auth runs once per connection at accept time.

**Tech Stack:** Go 1.26, `nhooyr.io/websocket v1.8.17` (confirmed in
`/root/agentserver/go.mod`), `github.com/go-chi/chi/v5`, `github.com/lib/pq`,
`golang.org/x/crypto/bcrypt`, stdlib `crypto/hmac`, `crypto/sha256`,
`encoding/base64`, `encoding/json`. No new top-level deps required.

**Spec:** `/root/agentserver/docs/superpowers/specs/2026-05-05-codex-app-gateway-and-exec-gateway-design.md`
(Subsystem 3 + cross-cutting "Auth model" + "Capability token" sections
are the binding contract).

**Module path:** `github.com/agentserver/agentserver/internal/codexexecgateway`
(matches `module github.com/agentserver/agentserver` in `/root/agentserver/go.mod`).

**Working directory:** All tasks operate in `/root/agentserver` unless
otherwise noted.

---

## File Structure

| File | Responsibility |
|---|---|
| `cmd/codex-exec-gateway/main.go` | Process entrypoint: load config, open store, build server, http.ListenAndServe with graceful shutdown |
| `Dockerfile.codex-exec-gateway` | Multi-stage build (golang:1.26-trixie → debian:trixie-slim); EXPOSE 6060 |
| `internal/codexexecgateway/config.go` | `Config` + `LoadConfigFromEnv` (port 6060, db url, HMAC secret, internal-API shared-secret, ping interval, idle timeout) |
| `internal/codexexecgateway/server.go` | `Server` struct + `NewServer` + `Routes()` chi router wiring |
| `internal/codexexecgateway/models.go` | `Executor`, `WorkspaceExecutor`, `ConnectedExecutor` JSON types |
| `internal/codexexecgateway/store.go` | Postgres CRUD: CreateExecutor, GetExecutor, UpdateLastSeen, BindWorkspaceExecutor, UnbindWorkspaceExecutor, ListWorkspaceExecutors, ConnectedExecutorsForWorkspace; embedded migrations FS |
| `internal/codexexecgateway/migrations/001_executors.sql` | DDL for `executors` and `workspace_executors` tables |
| `internal/codexexecgateway/auth.go` | `VerifyCapabilityToken(token, secret) (CapPayload, error)` — splits on `.`, base64url-decodes, HMAC-SHA256 verifies, parses `{turn_id, workspace_id, exe_ids[], iat, exp}`, checks `exp` |
| `internal/codexexecgateway/registry.go` | `ConnRegistry`: `Register(exe_id, conn) (evicted *websocket.Conn)`, `Lookup(exe_id) (*websocket.Conn, bool)`, `Unregister(exe_id, conn)` (only unregisters if value matches); concurrent-safe via `sync.Mutex` |
| `internal/codexexecgateway/revocation.go` | `RevokedSet`: `Add(turn_id, exp)`, `Contains(turn_id) bool`; cap 10000 entries, periodic exp-pruning via background ticker |
| `internal/codexexecgateway/inbound.go` | `handleInbound`: accept ws at `/codex-exec/{exe_id}`, bcrypt-verify tunnel token, register conn, update `last_seen_at`, block until close |
| `internal/codexexecgateway/bridge.go` | `handleBridge`: accept ws at `/bridge/{exe_id}`, verify cap token + revocation + allow-list, look up registry, run paired frame pumps |
| `internal/codexexecgateway/forwarder.go` | `pumpFrames(ctx, src, dst) error` — `src.Read` → `dst.Write` loop preserving frame type (text/binary), exits on error/close |
| `internal/codexexecgateway/handlers/register.go` | `POST /api/codex-exec/register` — issue exe_id + raw token, store bcrypt hash |
| `internal/codexexecgateway/handlers/workspace_binding.go` | `POST/DELETE/GET /api/codex-exec/workspaces/{wid}/executors` |
| `internal/codexexecgateway/handlers/internal_api.go` | `GET /api/exec-gateway/connected?workspace_id=…` and `POST /api/exec-gateway/revoke-turn`; protected by shared-secret bearer middleware |
| `internal/codexexecgateway/handlers/middleware.go` | `requireSharedSecret(secret)` middleware for internal API |
| `internal/codexexecgateway/*_test.go` | One test file per source file; integration tests under `integration_test.go` |

---

## Task 1: Repo bootstrap, cmd skeleton, Dockerfile, config, chi server

**Files:**
- Create: `/root/agentserver/cmd/codex-exec-gateway/main.go`
- Create: `/root/agentserver/Dockerfile.codex-exec-gateway`
- Create: `/root/agentserver/internal/codexexecgateway/config.go`
- Create: `/root/agentserver/internal/codexexecgateway/config_test.go`
- Create: `/root/agentserver/internal/codexexecgateway/server.go`
- Create: `/root/agentserver/internal/codexexecgateway/server_test.go`

- [ ] **Step 1: Write failing config test**

`/root/agentserver/internal/codexexecgateway/config_test.go`:
```go
package codexexecgateway

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv("CXG_DATABASE_URL", "postgres://x")
	t.Setenv("CXG_CAPTOKEN_HMAC_SECRET", "s3cret")
	t.Setenv("CXG_INTERNAL_SHARED_SECRET", "intern")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.Port != "6060" {
		t.Errorf("Port: want 6060, got %q", cfg.Port)
	}
	if cfg.PingInterval != 30*time.Second {
		t.Errorf("PingInterval: want 30s, got %v", cfg.PingInterval)
	}
	if cfg.IdleTimeout != 5*time.Minute {
		t.Errorf("IdleTimeout: want 5m, got %v", cfg.IdleTimeout)
	}
}

func TestLoadConfigFromEnv_RequiresDB(t *testing.T) {
	os.Unsetenv("CXG_DATABASE_URL")
	t.Setenv("CXG_CAPTOKEN_HMAC_SECRET", "s3cret")
	t.Setenv("CXG_INTERNAL_SHARED_SECRET", "intern")
	if _, err := LoadConfigFromEnv(); err == nil {
		t.Fatal("expected error when CXG_DATABASE_URL unset")
	}
}

func TestLoadConfigFromEnv_OverridesDuration(t *testing.T) {
	t.Setenv("CXG_DATABASE_URL", "postgres://x")
	t.Setenv("CXG_CAPTOKEN_HMAC_SECRET", "s3cret")
	t.Setenv("CXG_INTERNAL_SHARED_SECRET", "intern")
	t.Setenv("CXG_PING_INTERVAL", "10s")
	t.Setenv("CXG_IDLE_TIMEOUT", "2m")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.PingInterval != 10*time.Second {
		t.Errorf("PingInterval: want 10s, got %v", cfg.PingInterval)
	}
	if cfg.IdleTimeout != 2*time.Minute {
		t.Errorf("IdleTimeout: want 2m, got %v", cfg.IdleTimeout)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/...`
Expected: build error (`undefined: LoadConfigFromEnv`).

- [ ] **Step 3: Implement config.go**

`/root/agentserver/internal/codexexecgateway/config.go`:
```go
package codexexecgateway

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	Port                 string
	DatabaseURL          string
	CapTokenHMACSecret           []byte
	InternalSharedSecret string
	PingInterval         time.Duration
	IdleTimeout          time.Duration
	LogLevel             slog.Level
}

func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		Port:                 envOr("CXG_PORT", "6060"),
		DatabaseURL:          os.Getenv("CXG_DATABASE_URL"),
		CapTokenHMACSecret:           []byte(os.Getenv("CXG_CAPTOKEN_HMAC_SECRET")),
		InternalSharedSecret: os.Getenv("CXG_INTERNAL_SHARED_SECRET"),
		PingInterval:         30 * time.Second,
		IdleTimeout:          5 * time.Minute,
		LogLevel:             slog.LevelInfo,
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("CXG_DATABASE_URL is required")
	}
	if len(cfg.CapTokenHMACSecret) == 0 {
		return cfg, fmt.Errorf("CXG_CAPTOKEN_HMAC_SECRET is required")
	}
	if cfg.InternalSharedSecret == "" {
		return cfg, fmt.Errorf("CXG_INTERNAL_SHARED_SECRET is required")
	}
	if v := os.Getenv("CXG_PING_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("parse CXG_PING_INTERVAL: %w", err)
		}
		cfg.PingInterval = d
	}
	if v := os.Getenv("CXG_IDLE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("parse CXG_IDLE_TIMEOUT: %w", err)
		}
		cfg.IdleTimeout = d
	}
	if v := os.Getenv("CXG_LOG_LEVEL"); v != "" {
		switch strings.ToLower(v) {
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

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/... -run Config`
Expected: PASS.

- [ ] **Step 5: Write failing server smoke test**

`/root/agentserver/internal/codexexecgateway/server_test.go`:
```go
package codexexecgateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServer_HealthZ(t *testing.T) {
	srv := NewServer(Config{}, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz: want 200, got %d", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Fatalf("healthz body: want ok, got %q", rr.Body.String())
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/... -run Server`
Expected: build error (`undefined: NewServer`).

- [ ] **Step 7: Implement server.go skeleton**

`/root/agentserver/internal/codexexecgateway/server.go`:
```go
package codexexecgateway

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server bundles the chi router with its dependencies.
// store, registry, revoked may be nil during smoke tests.
type Server struct {
	config   Config
	store    *Store
	registry *ConnRegistry
	revoked  *RevokedSet
	logger   *slog.Logger
}

func NewServer(cfg Config, store *Store) *Server {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	return &Server{
		config:   cfg,
		store:    store,
		registry: NewConnRegistry(),
		revoked:  NewRevokedSet(10000),
		logger:   logger,
	}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// More routes added in later tasks.
	return r
}
```

`ConnRegistry` and `RevokedSet` are defined in tasks 5 and 6; for the
skeleton to compile, add stubs at the bottom of the same file (deleted in
those tasks):

```go
type ConnRegistry struct{}

func NewConnRegistry() *ConnRegistry { return &ConnRegistry{} }

type RevokedSet struct{}

func NewRevokedSet(int) *RevokedSet { return &RevokedSet{} }
```

- [ ] **Step 8: Run test to verify it passes**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/...`
Expected: PASS for both Config and Server tests.

- [ ] **Step 9: Implement cmd/main.go**

`/root/agentserver/cmd/codex-exec-gateway/main.go`:
```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentserver/agentserver/internal/codexexecgateway"
)

func main() {
	cfg, err := codexexecgateway.LoadConfigFromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := codexexecgateway.NewStore(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()

	srv := codexexecgateway.NewServer(cfg, store)
	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		httpServer.Shutdown(ctx)
	}()

	log.Printf("codex-exec-gateway listening on :%s", cfg.Port)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
```

This will not build until task 2 introduces `NewStore`. That is fine —
the next task fixes it.

- [ ] **Step 10: Write Dockerfile**

`/root/agentserver/Dockerfile.codex-exec-gateway`:
```
# Build Go binary
FROM golang:1.26-trixie AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o codex-exec-gateway ./cmd/codex-exec-gateway

# Runtime image (minimal — no Docker CLI, no codex binary needed)
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/codex-exec-gateway /usr/local/bin/codex-exec-gateway
EXPOSE 6060
ENTRYPOINT ["codex-exec-gateway"]
```

- [ ] **Step 11: Commit**

```bash
cd /root/agentserver
git add cmd/codex-exec-gateway Dockerfile.codex-exec-gateway internal/codexexecgateway/config.go internal/codexexecgateway/config_test.go internal/codexexecgateway/server.go internal/codexexecgateway/server_test.go
git commit -m "feat(codex-exec-gateway): bootstrap cmd, config, chi server skeleton"
```

---

## Task 2: Postgres migrations + Store skeleton

**Files:**
- Create: `/root/agentserver/internal/codexexecgateway/migrations/001_executors.sql`
- Create: `/root/agentserver/internal/codexexecgateway/store.go`
- Create: `/root/agentserver/internal/codexexecgateway/store_test.go`
- Create: `/root/agentserver/internal/codexexecgateway/models.go`

- [ ] **Step 1: Write the migration SQL**

`/root/agentserver/internal/codexexecgateway/migrations/001_executors.sql`:
```sql
CREATE TABLE IF NOT EXISTS executors (
    exe_id                  TEXT PRIMARY KEY,
    user_id                 TEXT NOT NULL,
    display_name            TEXT,
    description             TEXT,
    default_cwd             TEXT,
    registration_token_hash TEXT NOT NULL,
    registered_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at            TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_executors_user ON executors(user_id);

CREATE TABLE IF NOT EXISTS workspace_executors (
    workspace_id TEXT NOT NULL,
    exe_id       TEXT NOT NULL REFERENCES executors(exe_id) ON DELETE CASCADE,
    is_default   BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, exe_id)
);

CREATE INDEX IF NOT EXISTS idx_workspace_executors_workspace ON workspace_executors(workspace_id);
```

- [ ] **Step 2: Write the models file**

`/root/agentserver/internal/codexexecgateway/models.go`:
```go
package codexexecgateway

import "time"

// Executor is the persistent identity of a codex-exec node.
type Executor struct {
	ExeID        string     `json:"exe_id"`
	UserID       string     `json:"user_id"`
	DisplayName  string     `json:"display_name,omitempty"`
	Description  string     `json:"description,omitempty"`
	DefaultCwd   string     `json:"default_cwd,omitempty"`
	RegisteredAt time.Time  `json:"registered_at"`
	LastSeenAt   *time.Time `json:"last_seen_at,omitempty"`
}

// WorkspaceExecutor is a row in workspace_executors.
type WorkspaceExecutor struct {
	WorkspaceID string    `json:"workspace_id"`
	ExeID       string    `json:"exe_id"`
	IsDefault   bool      `json:"is_default"`
	CreatedAt   time.Time `json:"created_at"`
}

// ConnectedExecutor is the join shape returned by /api/exec-gateway/connected.
type ConnectedExecutor struct {
	ExeID       string     `json:"exe_id"`
	Description string     `json:"description"`
	DefaultCwd  string     `json:"default_cwd"`
	IsDefault   bool       `json:"is_default"`
	LastSeenAt  *time.Time `json:"last_seen_at,omitempty"`
}
```

- [ ] **Step 3: Write the failing store test**

`/root/agentserver/internal/codexexecgateway/store_test.go`:
```go
package codexexecgateway

import (
	"context"
	"os"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	store, err := NewStore(dbURL)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() {
		store.Exec(`DELETE FROM workspace_executors`)
		store.Exec(`DELETE FROM executors`)
		store.Close()
	})
	return store
}

func TestStore_CreateAndGetExecutor(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	exe := Executor{
		ExeID:        "exe_test1",
		UserID:       "user_a",
		DisplayName:  "laptop",
		Description:  "Daisy MBP",
		DefaultCwd:   "/home/daisy",
		RegisteredAt: time.Now().UTC(),
	}
	if err := store.CreateExecutor(ctx, exe, "hashed_token"); err != nil {
		t.Fatalf("CreateExecutor: %v", err)
	}
	got, err := store.GetExecutor(ctx, "exe_test1")
	if err != nil {
		t.Fatalf("GetExecutor: %v", err)
	}
	if got == nil || got.ExeID != "exe_test1" || got.Description != "Daisy MBP" {
		t.Fatalf("GetExecutor: got %+v", got)
	}
}
```

- [ ] **Step 4: Run test to verify it fails**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/... -run Store`
Expected: build error (`undefined: NewStore`).

- [ ] **Step 5: Implement store.go (skeleton with migrations + Create/Get)**

Replace the `ConnRegistry`/`RevokedSet` stubs added in Task 1 by removing them
from `server.go` later when tasks 5/6 supply the real types. For now keep them
in `server.go` so server tests compile.

`/root/agentserver/internal/codexexecgateway/store.go`:
```go
package codexexecgateway

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log"
	"sort"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store provides Postgres access for executors + workspace bindings.
type Store struct {
	*sql.DB
}

// NewStore opens a database connection and runs migrations.
func NewStore(databaseURL string) (*Store, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	if _, err := s.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		name := e.Name()
		var exists bool
		if err := s.QueryRow("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		content, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(content)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec("INSERT INTO schema_migrations(version) VALUES ($1)", name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		log.Printf("applied migration: %s", name)
	}
	return nil
}

// CreateExecutor inserts a new executor row. Caller supplies the bcrypt hash.
func (s *Store) CreateExecutor(ctx context.Context, e Executor, registrationTokenHash string) error {
	_, err := s.ExecContext(ctx, `
		INSERT INTO executors (exe_id, user_id, display_name, description, default_cwd,
		                       registration_token_hash, registered_at)
		VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), NULLIF($5,''), $6, $7)`,
		e.ExeID, e.UserID, e.DisplayName, e.Description, e.DefaultCwd,
		registrationTokenHash, e.RegisteredAt)
	if err != nil {
		return fmt.Errorf("insert executor: %w", err)
	}
	return nil
}

// GetExecutor returns the executor by id, or (nil, nil) if absent.
func (s *Store) GetExecutor(ctx context.Context, exeID string) (*Executor, error) {
	row := s.QueryRowContext(ctx, `
		SELECT exe_id, user_id,
		       COALESCE(display_name, ''),
		       COALESCE(description, ''),
		       COALESCE(default_cwd, ''),
		       registered_at, last_seen_at
		FROM executors WHERE exe_id=$1`, exeID)
	var e Executor
	var lastSeen sql.NullTime
	err := row.Scan(&e.ExeID, &e.UserID, &e.DisplayName, &e.Description, &e.DefaultCwd,
		&e.RegisteredAt, &lastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get executor: %w", err)
	}
	if lastSeen.Valid {
		t := lastSeen.Time
		e.LastSeenAt = &t
	}
	return &e, nil
}

// GetRegistrationTokenHash returns the bcrypt hash used to authenticate
// /codex-exec/{exe_id} ws connections.
func (s *Store) GetRegistrationTokenHash(ctx context.Context, exeID string) (string, error) {
	var hash string
	err := s.QueryRowContext(ctx, `SELECT registration_token_hash FROM executors WHERE exe_id=$1`, exeID).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get registration token hash: %w", err)
	}
	return hash, nil
}

// UpdateLastSeen sets the last_seen_at timestamp to NOW().
func (s *Store) UpdateLastSeen(ctx context.Context, exeID string) error {
	_, err := s.ExecContext(ctx, `UPDATE executors SET last_seen_at=NOW() WHERE exe_id=$1`, exeID)
	if err != nil {
		return fmt.Errorf("update last_seen: %w", err)
	}
	return nil
}

// Close closes the underlying DB.
func (s *Store) Close() error { return s.DB.Close() }
```

- [ ] **Step 6: Run test to verify it passes**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run Store`
Expected: PASS (or SKIP if `TEST_DATABASE_URL` is unset).

Verify default skip path:
Run: `cd /root/agentserver && go test ./internal/codexexecgateway/...`
Expected: all tests SKIP/PASS, no failures.

- [ ] **Step 7: Verify cmd builds**

Run: `cd /root/agentserver && go build ./cmd/codex-exec-gateway`
Expected: no output, exit 0.

- [ ] **Step 8: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/migrations internal/codexexecgateway/models.go internal/codexexecgateway/store.go internal/codexexecgateway/store_test.go
git commit -m "feat(codex-exec-gateway): postgres migrations + executor store skeleton"
```

---

## Task 3: Workspace-binding store methods

**Files:**
- Modify: `/root/agentserver/internal/codexexecgateway/store.go`
- Modify: `/root/agentserver/internal/codexexecgateway/store_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `/root/agentserver/internal/codexexecgateway/store_test.go`:
```go
func TestStore_BindAndListWorkspaceExecutors(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	exe := Executor{ExeID: "exe_a", UserID: "u1", Description: "alpha", RegisteredAt: time.Now().UTC()}
	if err := store.CreateExecutor(ctx, exe, "h"); err != nil {
		t.Fatalf("CreateExecutor: %v", err)
	}
	if err := store.BindWorkspaceExecutor(ctx, "ws_1", "exe_a", true); err != nil {
		t.Fatalf("BindWorkspaceExecutor: %v", err)
	}
	rows, err := store.ListWorkspaceExecutors(ctx, "ws_1")
	if err != nil {
		t.Fatalf("ListWorkspaceExecutors: %v", err)
	}
	if len(rows) != 1 || rows[0].ExeID != "exe_a" || !rows[0].IsDefault {
		t.Fatalf("got %+v", rows)
	}
}

func TestStore_UnbindWorkspaceExecutor(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	exe := Executor{ExeID: "exe_b", UserID: "u1", RegisteredAt: time.Now().UTC()}
	store.CreateExecutor(ctx, exe, "h")
	store.BindWorkspaceExecutor(ctx, "ws_1", "exe_b", false)
	if err := store.UnbindWorkspaceExecutor(ctx, "ws_1", "exe_b"); err != nil {
		t.Fatalf("UnbindWorkspaceExecutor: %v", err)
	}
	rows, _ := store.ListWorkspaceExecutors(ctx, "ws_1")
	if len(rows) != 0 {
		t.Fatalf("after unbind got %d rows", len(rows))
	}
}

func TestStore_ConnectedExecutorsForWorkspace(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, e := range []Executor{
		{ExeID: "exe_x", UserID: "u1", Description: "x desc", DefaultCwd: "/x", RegisteredAt: now},
		{ExeID: "exe_y", UserID: "u1", Description: "y desc", DefaultCwd: "/y", RegisteredAt: now},
	} {
		store.CreateExecutor(ctx, e, "h")
	}
	store.BindWorkspaceExecutor(ctx, "ws_1", "exe_x", true)
	store.BindWorkspaceExecutor(ctx, "ws_1", "exe_y", false)
	got, err := store.ConnectedExecutorsForWorkspace(ctx, "ws_1", []string{"exe_x"})
	if err != nil {
		t.Fatalf("ConnectedExecutorsForWorkspace: %v", err)
	}
	if len(got) != 1 || got[0].ExeID != "exe_x" || !got[0].IsDefault {
		t.Fatalf("got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/... -run Workspace`
Expected: build error (`undefined: BindWorkspaceExecutor`).

- [ ] **Step 3: Implement the missing methods**

Append to `/root/agentserver/internal/codexexecgateway/store.go`:
```go
// BindWorkspaceExecutor inserts a workspace ↔ executor binding (or upserts is_default).
func (s *Store) BindWorkspaceExecutor(ctx context.Context, workspaceID, exeID string, isDefault bool) error {
	_, err := s.ExecContext(ctx, `
		INSERT INTO workspace_executors (workspace_id, exe_id, is_default)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, exe_id)
		DO UPDATE SET is_default = EXCLUDED.is_default`,
		workspaceID, exeID, isDefault)
	if err != nil {
		return fmt.Errorf("bind workspace executor: %w", err)
	}
	return nil
}

// UnbindWorkspaceExecutor removes a binding row.
func (s *Store) UnbindWorkspaceExecutor(ctx context.Context, workspaceID, exeID string) error {
	_, err := s.ExecContext(ctx, `
		DELETE FROM workspace_executors
		WHERE workspace_id=$1 AND exe_id=$2`, workspaceID, exeID)
	if err != nil {
		return fmt.Errorf("unbind workspace executor: %w", err)
	}
	return nil
}

// ListWorkspaceExecutors returns all bindings for a workspace, joined with executor metadata.
func (s *Store) ListWorkspaceExecutors(ctx context.Context, workspaceID string) ([]ConnectedExecutor, error) {
	rows, err := s.QueryContext(ctx, `
		SELECT we.exe_id,
		       COALESCE(e.description, ''),
		       COALESCE(e.default_cwd, ''),
		       we.is_default,
		       e.last_seen_at
		FROM workspace_executors we
		JOIN executors e ON e.exe_id = we.exe_id
		WHERE we.workspace_id = $1
		ORDER BY we.created_at`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace executors: %w", err)
	}
	defer rows.Close()
	var out []ConnectedExecutor
	for rows.Next() {
		var c ConnectedExecutor
		var lastSeen sql.NullTime
		if err := rows.Scan(&c.ExeID, &c.Description, &c.DefaultCwd, &c.IsDefault, &lastSeen); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			t := lastSeen.Time
			c.LastSeenAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConnectedExecutorsForWorkspace returns the intersection of (workspace's bound
// executors) ∩ (the connected exe_id list passed in). Used by the internal
// `/api/exec-gateway/connected` endpoint.
func (s *Store) ConnectedExecutorsForWorkspace(ctx context.Context, workspaceID string, connectedIDs []string) ([]ConnectedExecutor, error) {
	all, err := s.ListWorkspaceExecutors(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	connSet := make(map[string]struct{}, len(connectedIDs))
	for _, id := range connectedIDs {
		connSet[id] = struct{}{}
	}
	var out []ConnectedExecutor
	for _, c := range all {
		if _, ok := connSet[c.ExeID]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run Workspace`
Expected: 3 PASS.

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/store.go internal/codexexecgateway/store_test.go
git commit -m "feat(codex-exec-gateway): workspace binding store methods"
```

---

## Task 4: Capability-token verification

The token format from spec § Capability token:

```
token   = base64url(header) "." base64url(payload) "." base64url(sig)
header  = '{"alg":"HS256","typ":"CXG"}'
payload = '{"turn_id":"trn_xxx","workspace_id":"ws_xxx",
           "exe_ids":["exe_alpha","exe_beta"],
           "iat":...,"exp":...}'
sig     = HMAC-SHA256(secret, base64url(header) "." base64url(payload))
```

Use base64url **without padding** (RFC 7515 / JWT convention).

**Files:**
- Create: `/root/agentserver/internal/codexexecgateway/auth.go`
- Create: `/root/agentserver/internal/codexexecgateway/auth_test.go`

- [ ] **Step 1: Write failing tests**

`/root/agentserver/internal/codexexecgateway/auth_test.go`:
```go
package codexexecgateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func mintToken(t *testing.T, secret []byte, payload CapPayload) string {
	t.Helper()
	header := []byte(`{"alg":"HS256","typ":"CXG"}`)
	pj, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(header) + "." + enc.EncodeToString(pj)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	sig := mac.Sum(nil)
	return signingInput + "." + enc.EncodeToString(sig)
}

func TestVerifyCapabilityToken_HappyPath(t *testing.T) {
	secret := []byte("k")
	now := time.Now().Unix()
	tok := mintToken(t, secret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1",
		ExeIDs: []string{"exe_a", "exe_b"}, IAT: now, EXP: now + 60,
	})
	got, err := VerifyCapabilityToken(tok, secret)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.TurnID != "trn_1" || len(got.ExeIDs) != 2 || got.ExeIDs[1] != "exe_b" {
		t.Fatalf("payload: %+v", got)
	}
}

func TestVerifyCapabilityToken_BadSig(t *testing.T) {
	tok := mintToken(t, []byte("k1"), CapPayload{TurnID: "t", EXP: time.Now().Unix() + 60})
	if _, err := VerifyCapabilityToken(tok, []byte("k2")); err != ErrBadSignature {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestVerifyCapabilityToken_Expired(t *testing.T) {
	tok := mintToken(t, []byte("k"), CapPayload{TurnID: "t", EXP: time.Now().Unix() - 1})
	if _, err := VerifyCapabilityToken(tok, []byte("k")); err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestVerifyCapabilityToken_Malformed(t *testing.T) {
	cases := []string{"", "a.b", "a.b.c.d", "!.!.!"}
	for _, c := range cases {
		if _, err := VerifyCapabilityToken(c, []byte("k")); err != ErrMalformed {
			t.Fatalf("%q: want ErrMalformed, got %v", c, err)
		}
	}
}

func TestCapPayload_AllowsExeID(t *testing.T) {
	p := CapPayload{ExeIDs: []string{"exe_a", "exe_b"}}
	if !p.AllowsExeID("exe_b") {
		t.Fatal("want true")
	}
	if p.AllowsExeID("exe_z") {
		t.Fatal("want false")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/... -run Capability`
Expected: build error (`undefined: VerifyCapabilityToken`).

- [ ] **Step 3: Implement auth.go**

`/root/agentserver/internal/codexexecgateway/auth.go`:
```go
package codexexecgateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// CapPayload is the parsed JSON payload from a CODEX_EXEC_GATEWAY_TOKEN.
type CapPayload struct {
	TurnID      string   `json:"turn_id"`
	WorkspaceID string   `json:"workspace_id"`
	ExeIDs      []string `json:"exe_ids"`
	IAT         int64    `json:"iat"`
	EXP         int64    `json:"exp"`
}

// AllowsExeID reports whether the named exe_id is in the token's allow set.
func (p CapPayload) AllowsExeID(exeID string) bool {
	for _, id := range p.ExeIDs {
		if id == exeID {
			return true
		}
	}
	return false
}

var (
	ErrMalformed    = errors.New("malformed capability token")
	ErrBadSignature = errors.New("bad capability token signature")
	ErrExpired      = errors.New("capability token expired")
)

// VerifyCapabilityToken parses and verifies a 3-part HMAC token.
// Returns the parsed payload on success.
func VerifyCapabilityToken(token string, secret []byte) (CapPayload, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return CapPayload{}, ErrMalformed
	}
	headerB64, payloadB64, sigB64 := parts[0], parts[1], parts[2]
	if headerB64 == "" || payloadB64 == "" || sigB64 == "" {
		return CapPayload{}, ErrMalformed
	}

	enc := base64.RawURLEncoding
	headerBytes, err := enc.DecodeString(headerB64)
	if err != nil {
		return CapPayload{}, ErrMalformed
	}
	var hdr struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return CapPayload{}, ErrMalformed
	}
	if hdr.Alg != "HS256" || hdr.Typ != "CXG" {
		return CapPayload{}, ErrMalformed
	}

	gotSig, err := enc.DecodeString(sigB64)
	if err != nil {
		return CapPayload{}, ErrMalformed
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(headerB64 + "." + payloadB64))
	wantSig := mac.Sum(nil)
	if !hmac.Equal(gotSig, wantSig) {
		return CapPayload{}, ErrBadSignature
	}

	payloadBytes, err := enc.DecodeString(payloadB64)
	if err != nil {
		return CapPayload{}, ErrMalformed
	}
	var payload CapPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return CapPayload{}, ErrMalformed
	}
	if time.Now().Unix() > payload.EXP {
		return CapPayload{}, ErrExpired
	}
	return payload, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/... -run Capability`
Expected: 5 PASS.

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/auth.go internal/codexexecgateway/auth_test.go
git commit -m "feat(codex-exec-gateway): capability-token HMAC verify"
```

---

## Task 5: ConnRegistry (in-memory exe_id → ws conn)

**Files:**
- Create: `/root/agentserver/internal/codexexecgateway/registry.go`
- Create: `/root/agentserver/internal/codexexecgateway/registry_test.go`
- Modify: `/root/agentserver/internal/codexexecgateway/server.go` (remove stub)

- [ ] **Step 1: Write failing tests**

`/root/agentserver/internal/codexexecgateway/registry_test.go`:
```go
package codexexecgateway

import (
	"sync"
	"testing"

	"nhooyr.io/websocket"
)

// fakeConn is a no-op stand-in for *websocket.Conn so tests don't need a live ws.
// We rely on pointer identity, never invoke ws methods.
func fakeConn() *websocket.Conn { return (*websocket.Conn)(nil) }

func TestConnRegistry_RegisterAndLookup(t *testing.T) {
	r := NewConnRegistry()
	c1 := new(websocket.Conn) // pointer identity only
	if evicted := r.Register("exe_a", c1); evicted != nil {
		t.Fatalf("first register should not evict: got %p", evicted)
	}
	got, ok := r.Lookup("exe_a")
	if !ok || got != c1 {
		t.Fatalf("lookup: ok=%v got=%p want %p", ok, got, c1)
	}
}

func TestConnRegistry_RegisterEvictsExisting(t *testing.T) {
	r := NewConnRegistry()
	c1, c2 := new(websocket.Conn), new(websocket.Conn)
	r.Register("exe_a", c1)
	evicted := r.Register("exe_a", c2)
	if evicted != c1 {
		t.Fatalf("evicted: got %p want %p", evicted, c1)
	}
	got, _ := r.Lookup("exe_a")
	if got != c2 {
		t.Fatalf("after eviction lookup: got %p want %p", got, c2)
	}
}

func TestConnRegistry_UnregisterOnlyIfMatches(t *testing.T) {
	r := NewConnRegistry()
	c1, c2 := new(websocket.Conn), new(websocket.Conn)
	r.Register("exe_a", c1)
	// Try to unregister with a stale conn — must NOT remove c1.
	r.Unregister("exe_a", c2)
	if got, _ := r.Lookup("exe_a"); got != c1 {
		t.Fatalf("stale unregister should be no-op; got %p", got)
	}
	r.Unregister("exe_a", c1)
	if _, ok := r.Lookup("exe_a"); ok {
		t.Fatal("should be removed")
	}
}

func TestConnRegistry_ConnectedIDs(t *testing.T) {
	r := NewConnRegistry()
	r.Register("exe_a", new(websocket.Conn))
	r.Register("exe_b", new(websocket.Conn))
	got := r.ConnectedIDs()
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestConnRegistry_Concurrent(t *testing.T) {
	r := NewConnRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := new(websocket.Conn)
			r.Register("exe_x", c)
			r.Lookup("exe_x")
			r.Unregister("exe_x", c)
		}()
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/... -run ConnRegistry`
Expected: build error (no `Register`/`Lookup`/`Unregister`/`ConnectedIDs` methods).

- [ ] **Step 3: Implement registry.go**

`/root/agentserver/internal/codexexecgateway/registry.go`:
```go
package codexexecgateway

import (
	"sync"

	"nhooyr.io/websocket"
)

// ConnRegistry tracks the single live inbound /codex-exec/{exe_id} ws conn
// per exe_id. Re-registering an exe_id evicts the prior connection.
type ConnRegistry struct {
	mu    sync.Mutex
	conns map[string]*websocket.Conn
}

func NewConnRegistry() *ConnRegistry {
	return &ConnRegistry{conns: make(map[string]*websocket.Conn)}
}

// Register installs `c` as the conn for `exeID`. If an existing conn was
// present, it is returned so the caller can close it; otherwise nil.
func (r *ConnRegistry) Register(exeID string, c *websocket.Conn) (evicted *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.conns[exeID]
	r.conns[exeID] = c
	if prev != nil && prev != c {
		return prev
	}
	return nil
}

// Lookup returns the registered conn for `exeID`, if any.
func (r *ConnRegistry) Lookup(exeID string) (*websocket.Conn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[exeID]
	return c, ok
}

// Unregister removes `exeID` only if its current value is `c`. This guards
// against a goroutine for an old conn deleting a new conn after eviction.
func (r *ConnRegistry) Unregister(exeID string, c *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[exeID] == c {
		delete(r.conns, exeID)
	}
}

// ConnectedIDs returns a snapshot of currently registered exe_ids.
func (r *ConnRegistry) ConnectedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.conns))
	for id := range r.conns {
		out = append(out, id)
	}
	return out
}
```

- [ ] **Step 4: Remove the stub from server.go**

Edit `/root/agentserver/internal/codexexecgateway/server.go`: delete the
two stub blocks at the bottom:
```go
type ConnRegistry struct{}
func NewConnRegistry() *ConnRegistry { return &ConnRegistry{} }
type RevokedSet struct{}
func NewRevokedSet(int) *RevokedSet { return &RevokedSet{} }
```
and replace with just:
```go
// (real ConnRegistry lives in registry.go; real RevokedSet in revocation.go)
```

The `RevokedSet` real type is added in Task 6. Until then keep a single
stub for `RevokedSet` only:

```go
// Stub kept until task 6 supplies the real revocation.go.
type RevokedSet struct{}

func NewRevokedSet(int) *RevokedSet { return &RevokedSet{} }
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/...`
Expected: all PASS (registry tests + earlier tests still pass).

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/registry.go internal/codexexecgateway/registry_test.go internal/codexexecgateway/server.go
git commit -m "feat(codex-exec-gateway): in-memory connection registry"
```

---

## Task 6: RevokedSet (capped, exp-pruned, concurrent-safe)

**Files:**
- Create: `/root/agentserver/internal/codexexecgateway/revocation.go`
- Create: `/root/agentserver/internal/codexexecgateway/revocation_test.go`
- Modify: `/root/agentserver/internal/codexexecgateway/server.go` (remove final stub)

- [ ] **Step 1: Write failing tests**

`/root/agentserver/internal/codexexecgateway/revocation_test.go`:
```go
package codexexecgateway

import (
	"sync"
	"testing"
	"time"
)

func TestRevokedSet_AddAndContains(t *testing.T) {
	r := NewRevokedSet(100)
	if r.Contains("trn_1") {
		t.Fatal("empty set should not contain anything")
	}
	r.Add("trn_1", time.Now().Add(time.Hour).Unix())
	if !r.Contains("trn_1") {
		t.Fatal("after Add should contain")
	}
}

func TestRevokedSet_PruneExpired(t *testing.T) {
	r := NewRevokedSet(100)
	r.Add("trn_old", time.Now().Add(-time.Second).Unix())
	r.Add("trn_new", time.Now().Add(time.Hour).Unix())
	r.Prune()
	if r.Contains("trn_old") {
		t.Fatal("expired entry should be pruned")
	}
	if !r.Contains("trn_new") {
		t.Fatal("non-expired entry should remain")
	}
}

func TestRevokedSet_CapEvictsOldest(t *testing.T) {
	r := NewRevokedSet(3)
	exp := time.Now().Add(time.Hour).Unix()
	r.Add("a", exp)
	r.Add("b", exp)
	r.Add("c", exp)
	r.Add("d", exp) // forces an eviction
	if r.Size() > 3 {
		t.Fatalf("size %d > cap 3", r.Size())
	}
	if !r.Contains("d") {
		t.Fatal("newest must remain")
	}
}

func TestRevokedSet_Concurrent(t *testing.T) {
	r := NewRevokedSet(1000)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Add("trn", time.Now().Add(time.Hour).Unix())
			r.Contains("trn")
		}(i)
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/... -run Revoked`
Expected: build error (`Add`, `Contains`, `Prune`, `Size` undefined on stub).

- [ ] **Step 3: Implement revocation.go**

`/root/agentserver/internal/codexexecgateway/revocation.go`:
```go
package codexexecgateway

import (
	"container/list"
	"sync"
	"time"
)

// RevokedSet is a bounded, concurrent-safe set of revoked turn_ids with
// per-entry expiry. Designed for the spec's "in-memory revoked set, cap
// ~10k, periodically pruned of entries past their original exp".
type RevokedSet struct {
	mu    sync.Mutex
	cap   int
	order *list.List               // FIFO of turn_ids; front = oldest
	items map[string]*list.Element // turn_id → element holding {turnID, exp}
}

type revokedEntry struct {
	turnID string
	exp    int64 // unix seconds
}

func NewRevokedSet(cap int) *RevokedSet {
	if cap <= 0 {
		cap = 10000
	}
	return &RevokedSet{
		cap:   cap,
		order: list.New(),
		items: make(map[string]*list.Element, cap),
	}
}

// Add inserts (turnID, exp). Re-adding refreshes the entry's position.
// When at capacity, the oldest entry is evicted.
func (r *RevokedSet) Add(turnID string, exp int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.items[turnID]; ok {
		el.Value = revokedEntry{turnID: turnID, exp: exp}
		r.order.MoveToBack(el)
		return
	}
	for r.order.Len() >= r.cap {
		oldest := r.order.Front()
		if oldest == nil {
			break
		}
		r.order.Remove(oldest)
		delete(r.items, oldest.Value.(revokedEntry).turnID)
	}
	el := r.order.PushBack(revokedEntry{turnID: turnID, exp: exp})
	r.items[turnID] = el
}

// Contains reports whether turnID is in the set (regardless of expiry —
// callers may rely on Prune to clean stale entries).
func (r *RevokedSet) Contains(turnID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.items[turnID]
	return ok
}

// Prune removes entries whose exp has passed.
func (r *RevokedSet) Prune() {
	now := time.Now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	for el := r.order.Front(); el != nil; {
		next := el.Next()
		if el.Value.(revokedEntry).exp < now {
			r.order.Remove(el)
			delete(r.items, el.Value.(revokedEntry).turnID)
		}
		el = next
	}
}

// Size returns the current number of entries.
func (r *RevokedSet) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.order.Len()
}

// StartPruner runs Prune at the given interval until ctx is done.
// Caller is responsible for cancelling ctx on shutdown.
func (r *RevokedSet) StartPruner(stop <-chan struct{}, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.Prune()
			}
		}
	}()
}
```

- [ ] **Step 4: Remove the final stub from server.go**

Delete the `RevokedSet` stub block from `server.go` so only the comment
remains. The package now has the real types in `registry.go` and
`revocation.go`.

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/...`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/revocation.go internal/codexexecgateway/revocation_test.go internal/codexexecgateway/server.go
git commit -m "feat(codex-exec-gateway): bounded in-memory revoked turn set"
```

---

## Task 7: Inbound `/codex-exec/{exe_id}` acceptor + bcrypt verify

**Files:**
- Create: `/root/agentserver/internal/codexexecgateway/inbound.go`
- Create: `/root/agentserver/internal/codexexecgateway/inbound_test.go`
- Modify: `/root/agentserver/internal/codexexecgateway/server.go` (wire route)

- [ ] **Step 1: Write failing test**

`/root/agentserver/internal/codexexecgateway/inbound_test.go`:
```go
package codexexecgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"nhooyr.io/websocket"
)

func newInboundTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	store := newTestStore(t)
	cfg := Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s",
		PingInterval: time.Second, IdleTimeout: 10 * time.Second}
	srv := NewServer(cfg, store)
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)
	return hs, srv
}

func TestInbound_RejectsBadToken(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	ctx := context.Background()
	hash, _ := bcrypt.GenerateFromPassword([]byte("right_token"), bcrypt.DefaultCost)
	srv.store.CreateExecutor(ctx, Executor{
		ExeID: "exe_inb1", UserID: "u", RegisteredAt: time.Now().UTC(),
	}, string(hash))

	wsURL := "ws" + hs.URL[len("http"):] + "/codex-exec/exe_inb1?token=wrong"
	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		t.Fatal("expected dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

func TestInbound_AcceptsAndRegisters(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	ctx := context.Background()
	hash, _ := bcrypt.GenerateFromPassword([]byte("good"), bcrypt.DefaultCost)
	srv.store.CreateExecutor(ctx, Executor{
		ExeID: "exe_inb2", UserID: "u", RegisteredAt: time.Now().UTC(),
	}, string(hash))

	wsURL := "ws" + hs.URL[len("http"):] + "/codex-exec/exe_inb2?token=good"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Wait for the handler to register; poll briefly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.registry.Lookup("exe_inb2"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := srv.registry.Lookup("exe_inb2"); !ok {
		t.Fatal("registry should hold exe_inb2 after accept")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run Inbound`
Expected: route 404 / handler missing.

- [ ] **Step 3: Implement inbound.go**

`/root/agentserver/internal/codexexecgateway/inbound.go`:
```go
package codexexecgateway

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
	"nhooyr.io/websocket"
)

// handleInbound accepts the long-lived ws connection from a local
// `codex exec-server --connect` process. The token is supplied as a query
// string parameter so the codex-exec --auth-token-env flow works without
// custom headers.
func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request) {
	exeID := chi.URLParam(r, "exe_id")
	token := r.URL.Query().Get("token")
	if exeID == "" || token == "" {
		http.Error(w, "missing parameters", http.StatusBadRequest)
		return
	}

	hash, err := s.store.GetRegistrationTokenHash(r.Context(), exeID)
	if err != nil {
		s.logger.Error("inbound: get token hash", "exe_id", exeID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hash == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(token)); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Error("inbound: ws accept", "exe_id", exeID, "error", err)
		return
	}

	if evicted := s.registry.Register(exeID, ws); evicted != nil {
		s.logger.Info("inbound: evicted prior conn", "exe_id", exeID)
		evicted.Close(websocket.StatusPolicyViolation, "replaced by new connection")
	}
	if err := s.store.UpdateLastSeen(r.Context(), exeID); err != nil {
		s.logger.Warn("inbound: update last_seen", "exe_id", exeID, "error", err)
	}
	s.logger.Info("inbound: connected", "exe_id", exeID)

	// Block until the client disconnects or the bridge pump closes the conn.
	// We do not parse frames here — the bridge pump in /bridge/{exe_id}
	// will read from this conn while it is paired. While unpaired, we just
	// hold the conn open and respond to keepalive pings (handled by nhooyr).
	<-r.Context().Done()
	_ = ws.Close(websocket.StatusNormalClosure, "")
	s.registry.Unregister(exeID, ws)
	bg := context.Background()
	if err := s.store.UpdateLastSeen(bg, exeID); err != nil {
		s.logger.Warn("inbound: final last_seen", "exe_id", exeID, "error", err)
	}
	s.logger.Info("inbound: disconnected", "exe_id", exeID)
}
```

- [ ] **Step 4: Wire the route in server.go**

In `Routes()`, add inside the chi.NewRouter() block:
```go
r.Get("/codex-exec/{exe_id}", s.handleInbound)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run Inbound`
Expected: 2 PASS.

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/inbound.go internal/codexexecgateway/inbound_test.go internal/codexexecgateway/server.go
git commit -m "feat(codex-exec-gateway): inbound /codex-exec acceptor with bcrypt auth"
```

---

## Task 8: Bridge `/bridge/{exe_id}` acceptor + paired frame pumps

The frame-pump preserves frame boundaries: each `Read` call yields one
frame's `MessageType` (`websocket.MessageText` or `websocket.MessageBinary`)
plus its payload, and we hand that exact (type, payload) pair to the
peer's `Write`. We never use `io.Copy`-on-`NetConn` here because that
flattens frames into a byte stream, violating the spec.

**Files:**
- Create: `/root/agentserver/internal/codexexecgateway/forwarder.go`
- Create: `/root/agentserver/internal/codexexecgateway/forwarder_test.go`
- Create: `/root/agentserver/internal/codexexecgateway/bridge.go`
- Create: `/root/agentserver/internal/codexexecgateway/bridge_test.go`
- Modify: `/root/agentserver/internal/codexexecgateway/server.go` (wire route)

- [ ] **Step 1: Write failing forwarder unit test**

`/root/agentserver/internal/codexexecgateway/forwarder_test.go`:
```go
package codexexecgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// dialServerEcho creates a ws server that, on accept, runs pumpFrames(ws, ws)
// — which trivially echoes — and returns its httptest base URL.
func dialPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	// Start two ws servers; client A dials server S1, client B dials server S2;
	// pumpFrames pipes between the two server-side connections.
	srvSideA := make(chan *websocket.Conn, 1)
	srvSideB := make(chan *websocket.Conn, 1)
	hsA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, _ := websocket.Accept(w, r, nil)
		srvSideA <- ws
	}))
	t.Cleanup(hsA.Close)
	hsB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, _ := websocket.Accept(w, r, nil)
		srvSideB <- ws
	}))
	t.Cleanup(hsB.Close)

	ctx := context.Background()
	cliA, _, err := websocket.Dial(ctx, "ws"+hsA.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	cliB, _, err := websocket.Dial(ctx, "ws"+hsB.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}

	sa := <-srvSideA
	sb := <-srvSideB
	go pumpFrames(ctx, sa, sb)
	go pumpFrames(ctx, sb, sa)
	return cliA, cliB
}

func TestPumpFrames_PreservesTextAndBinary(t *testing.T) {
	a, b := dialPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Text frame A → B
	if err := a.Write(ctx, websocket.MessageText, []byte(`{"id":1}`)); err != nil {
		t.Fatalf("a.Write: %v", err)
	}
	mt, data, err := b.Read(ctx)
	if err != nil {
		t.Fatalf("b.Read: %v", err)
	}
	if mt != websocket.MessageText || string(data) != `{"id":1}` {
		t.Fatalf("got mt=%v data=%q", mt, data)
	}

	// Binary frame B → A
	if err := b.Write(ctx, websocket.MessageBinary, []byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("b.Write: %v", err)
	}
	mt, data, err = a.Read(ctx)
	if err != nil {
		t.Fatalf("a.Read: %v", err)
	}
	if mt != websocket.MessageBinary || len(data) != 3 || data[2] != 0x03 {
		t.Fatalf("got mt=%v data=%v", mt, data)
	}

	a.Close(websocket.StatusNormalClosure, "")
	b.Close(websocket.StatusNormalClosure, "")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/... -run PumpFrames`
Expected: build error (`undefined: pumpFrames`).

- [ ] **Step 3: Implement forwarder.go**

`/root/agentserver/internal/codexexecgateway/forwarder.go`:
```go
package codexexecgateway

import (
	"context"
	"errors"
	"io"

	"nhooyr.io/websocket"
)

// pumpFrames reads one frame at a time from src and writes the exact same
// (MessageType, payload) to dst. This preserves JSON-RPC envelope boundaries
// — the spec requires frame-level forwarding, never byte concatenation.
//
// Returns nil when src closes cleanly; otherwise the underlying error.
// Either side closing causes pumpFrames to return; the bridge handler
// closes the peer when this returns so both halves shut down together.
func pumpFrames(ctx context.Context, src, dst *websocket.Conn) error {
	for {
		mt, data, err := src.Read(ctx)
		if err != nil {
			// Normal-closure on src is not an error to propagate up.
			closeErr := websocket.CloseStatus(err)
			if closeErr == websocket.StatusNormalClosure || closeErr == websocket.StatusGoingAway {
				return nil
			}
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if err := dst.Write(ctx, mt, data); err != nil {
			return err
		}
	}
}
```

- [ ] **Step 4: Run forwarder test to verify it passes**

Run: `cd /root/agentserver && go test ./internal/codexexecgateway/... -run PumpFrames`
Expected: PASS.

- [ ] **Step 5: Write failing bridge integration test**

`/root/agentserver/internal/codexexecgateway/bridge_test.go`:
```go
package codexexecgateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"nhooyr.io/websocket"
)

func mintBridgeToken(secret []byte, p CapPayload) string {
	header := []byte(`{"alg":"HS256","typ":"CXG"}`)
	pj, _ := json.Marshal(p)
	enc := base64.RawURLEncoding
	si := enc.EncodeToString(header) + "." + enc.EncodeToString(pj)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(si))
	return si + "." + enc.EncodeToString(mac.Sum(nil))
}

// connectInbound is a helper: it registers an executor (db row + bcrypt hash),
// then dials the inbound endpoint so the gateway has a live conn to pair.
func connectInbound(t *testing.T, srv *Server, baseURL, exeID string) *websocket.Conn {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte("rt"), bcrypt.DefaultCost)
	srv.store.CreateExecutor(context.Background(), Executor{
		ExeID: exeID, UserID: "u", RegisteredAt: time.Now().UTC(),
	}, string(hash))
	url := "ws" + baseURL[len("http"):] + "/codex-exec/" + exeID + "?token=rt"
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("inbound dial: %v", err)
	}
	// Wait for registration.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.registry.Lookup(exeID); ok {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("inbound not registered for %s", exeID)
	return nil
}

func TestBridge_Rejects401OnBadToken(t *testing.T) {
	hs, _ := newInboundTestServer(t)
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_x?token=garbage"
	_, resp, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatal("dial should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

func TestBridge_Rejects403WhenExeIDNotInAllowList(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1", ExeIDs: []string{"exe_other"},
		IAT: now, EXP: now + 60,
	})
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_target?token=" + tok
	_, resp, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatal("dial should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %v", resp)
	}
}

func TestBridge_Rejects503WhenExecutorOffline(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1", ExeIDs: []string{"exe_offline"},
		IAT: now, EXP: now + 60,
	})
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_offline?token=" + tok
	_, resp, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatal("dial should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %v", resp)
	}
}

func TestBridge_PairsAndForwardsBidirectional(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	inbound := connectInbound(t, srv, hs.URL, "exe_pair")
	defer inbound.Close(websocket.StatusNormalClosure, "")

	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1", ExeIDs: []string{"exe_pair"},
		IAT: now, EXP: now + 60,
	})
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_pair?token=" + tok
	bridge, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("bridge dial: %v", err)
	}
	defer bridge.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// bridge → inbound
	if err := bridge.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"initialize"}`)); err != nil {
		t.Fatalf("bridge.Write: %v", err)
	}
	mt, data, err := inbound.Read(ctx)
	if err != nil {
		t.Fatalf("inbound.Read: %v", err)
	}
	if mt != websocket.MessageText || string(data) != `{"id":1,"method":"initialize"}` {
		t.Fatalf("got mt=%v data=%q", mt, data)
	}

	// inbound → bridge
	if err := inbound.Write(ctx, websocket.MessageText, []byte(`{"id":1,"result":{}}`)); err != nil {
		t.Fatalf("inbound.Write: %v", err)
	}
	mt, data, err = bridge.Read(ctx)
	if err != nil {
		t.Fatalf("bridge.Read: %v", err)
	}
	if mt != websocket.MessageText || string(data) != `{"id":1,"result":{}}` {
		t.Fatalf("got mt=%v data=%q", mt, data)
	}
}

func TestBridge_RejectsRevokedTurn(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	connectInbound(t, srv, hs.URL, "exe_rev")
	now := time.Now().Unix()
	srv.revoked.Add("trn_revoked", now+60)
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_revoked", WorkspaceID: "ws_1", ExeIDs: []string{"exe_rev"},
		IAT: now, EXP: now + 60,
	})
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_rev?token=" + tok
	_, resp, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatal("dial should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}
```

- [ ] **Step 6: Implement bridge.go**

`/root/agentserver/internal/codexexecgateway/bridge.go`:
```go
package codexexecgateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"nhooyr.io/websocket"
)

// handleBridge accepts a ws connection from a spawned codex subprocess and
// pairs it with the registered inbound /codex-exec/{exe_id} conn. Auth is
// verified once at connect time; thereafter forwarding is unconditional
// until either side closes.
func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	exeID := chi.URLParam(r, "exe_id")
	token := r.URL.Query().Get("token")
	if exeID == "" || token == "" {
		http.Error(w, "missing parameters", http.StatusBadRequest)
		return
	}

	payload, err := VerifyCapabilityToken(token, s.config.CapTokenHMACSecret)
	if err != nil {
		switch {
		case errors.Is(err, ErrExpired):
			http.Error(w, "token expired", http.StatusUnauthorized)
		case errors.Is(err, ErrBadSignature), errors.Is(err, ErrMalformed):
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		return
	}
	if !payload.AllowsExeID(exeID) {
		http.Error(w, "exe_id not in token allow set", http.StatusForbidden)
		return
	}
	if s.revoked.Contains(payload.TurnID) {
		http.Error(w, "turn revoked", http.StatusUnauthorized)
		return
	}
	inbound, ok := s.registry.Lookup(exeID)
	if !ok {
		http.Error(w, "executor not connected", http.StatusServiceUnavailable)
		return
	}

	bridge, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Error("bridge: ws accept", "exe_id", exeID, "error", err)
		return
	}
	s.logger.Info("bridge: paired", "exe_id", exeID, "turn_id", payload.TurnID)

	pumpCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 2)
	go func() { errCh <- pumpFrames(pumpCtx, bridge, inbound) }()
	go func() { errCh <- pumpFrames(pumpCtx, inbound, bridge) }()

	// Wait for either pump to return; close both sides so the other pump
	// unblocks.
	first := <-errCh
	cancel()
	bridge.Close(websocket.StatusNormalClosure, "peer closed")
	// We deliberately do NOT close `inbound` here: another bridge connection
	// for the same exe_id may arrive next. The inbound conn lives until its
	// own /codex-exec/{exe_id} handler exits.
	<-errCh // wait for the other pump too

	if first != nil {
		s.logger.Info("bridge: pump ended", "exe_id", exeID, "err", first)
	} else {
		s.logger.Info("bridge: pump ended", "exe_id", exeID)
	}
}
```

- [ ] **Step 7: Wire the route in server.go**

In `Routes()`:
```go
r.Get("/bridge/{exe_id}", s.handleBridge)
```

- [ ] **Step 8: Run tests to verify they pass**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run Bridge`
Expected: 5 PASS.

- [ ] **Step 9: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/forwarder.go internal/codexexecgateway/forwarder_test.go internal/codexexecgateway/bridge.go internal/codexexecgateway/bridge_test.go internal/codexexecgateway/server.go
git commit -m "feat(codex-exec-gateway): bridge endpoint with paired frame pumps"
```

---

## Task 9: HTTP endpoint — POST /api/codex-exec/register

**Files:**
- Create: `/root/agentserver/internal/codexexecgateway/handlers/register.go`
- Create: `/root/agentserver/internal/codexexecgateway/handlers_register_test.go`
- Modify: `/root/agentserver/internal/codexexecgateway/server.go` (wire route)

(`handlers/` package keeps HTTP-specific code separated from the ws core,
mirroring the structure declared in the spec's repo layout.)

- [ ] **Step 1: Write failing test**

`/root/agentserver/internal/codexexecgateway/handlers_register_test.go`:
```go
package codexexecgateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHandleRegister_HappyPath(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s"}, store)

	body := bytes.NewReader([]byte(`{"display_name":"laptop","description":"d","default_cwd":"/x"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/codex-exec/register", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", "user_a") // see step 3 — placeholder auth header
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ExeID             string `json:"exe_id"`
		RegistrationToken string `json:"registration_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.ExeID, "exe_") || resp.RegistrationToken == "" {
		t.Fatalf("bad response: %+v", resp)
	}
	// Token round-trip: bcrypt hash from DB must verify against returned token.
	hash, err := store.GetRegistrationTokenHash(req.Context(), resp.ExeID)
	if err != nil || hash == "" {
		t.Fatalf("hash: %v %q", err, hash)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(resp.RegistrationToken)); err != nil {
		t.Fatalf("bcrypt verify: %v", err)
	}
}

func TestHandleRegister_RequiresUser(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s"}, store)
	req := httptest.NewRequest(http.MethodPost, "/api/codex-exec/register", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}
```

> Auth note: per the spec the user is identified by their JWT bearer.
> The bearer-validation middleware is built once at the agentserver level
> and is out of scope for this plan. To keep this plan self-contained,
> the handler reads the user_id from a server-trusted header
> `X-User-Id` injected by an upstream auth middleware (the same pattern
> used by other agentserver internal HTTP services). The middleware is
> wired in deployment, not in this plan.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run HandleRegister`
Expected: 404 (route absent).

- [ ] **Step 3: Implement handler**

`/root/agentserver/internal/codexexecgateway/handlers/register.go`:
```go
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/agentserver/agentserver/internal/codexexecgateway"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Store is the subset of *codexexecgateway.Store required by the register handler.
type Store interface {
	CreateExecutor(ctx context.Context, e codexexecgateway.Executor, registrationTokenHash string) error
}

type registerRequest struct {
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	DefaultCwd  string `json:"default_cwd"`
}

type registerResponse struct {
	ExeID             string `json:"exe_id"`
	RegistrationToken string `json:"registration_token"`
}

// Register returns an http.HandlerFunc that creates a new executor row and
// returns the freshly-minted (raw) registration token. The DB only stores
// the bcrypt hash — the raw token is never persisted or logged.
func Register(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Header.Get("X-User-Id")
		if userID == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing user"})
			return
		}
		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		raw, err := generateToken()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(raw), bcrypt.DefaultCost)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
			return
		}
		exe := codexexecgateway.Executor{
			ExeID:        "exe_" + uuid.NewString(),
			UserID:       userID,
			DisplayName:  req.DisplayName,
			Description:  req.Description,
			DefaultCwd:   req.DefaultCwd,
			RegisteredAt: time.Now().UTC(),
		}
		if err := store.CreateExecutor(r.Context(), exe, string(hash)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create executor"})
			return
		}
		writeJSON(w, http.StatusCreated, registerResponse{
			ExeID:             exe.ExeID,
			RegistrationToken: raw,
		})
	}
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 4: Wire route in server.go**

Add to imports:
```go
"github.com/agentserver/agentserver/internal/codexexecgateway/handlers"
```

In `Routes()`:
```go
r.Post("/api/codex-exec/register", handlers.Register(s.store))
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run HandleRegister`
Expected: 2 PASS.

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/handlers/register.go internal/codexexecgateway/handlers_register_test.go internal/codexexecgateway/server.go
git commit -m "feat(codex-exec-gateway): POST /api/codex-exec/register"
```

---

## Task 10: Workspace ↔ executor binding admin endpoints

Endpoints:
- `POST   /api/codex-exec/workspaces/{wid}/executors`   body `{exe_id, is_default}`
- `DELETE /api/codex-exec/workspaces/{wid}/executors/{exe_id}`
- `GET    /api/codex-exec/workspaces/{wid}/executors`

**Files:**
- Create: `/root/agentserver/internal/codexexecgateway/handlers/workspace_binding.go`
- Create: `/root/agentserver/internal/codexexecgateway/handlers_workspace_binding_test.go`
- Modify: `/root/agentserver/internal/codexexecgateway/server.go` (wire routes)

- [ ] **Step 1: Write failing tests**

`/root/agentserver/internal/codexexecgateway/handlers_workspace_binding_test.go`:
```go
package codexexecgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWorkspaceBinding_PostListDelete(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s"}, store)

	// Pre-seed an executor.
	store.CreateExecutor(context.Background(), Executor{
		ExeID: "exe_w1", UserID: "u", Description: "alpha", RegisteredAt: time.Now().UTC(),
	}, "h")

	// POST
	body := bytes.NewReader([]byte(`{"exe_id":"exe_w1","is_default":true}`))
	req := httptest.NewRequest(http.MethodPost, "/api/codex-exec/workspaces/ws_a/executors", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST: want 201, got %d body=%s", rr.Code, rr.Body.String())
	}

	// GET
	req = httptest.NewRequest(http.MethodGet, "/api/codex-exec/workspaces/ws_a/executors", nil)
	rr = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: want 200, got %d", rr.Code)
	}
	var got []ConnectedExecutor
	json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got) != 1 || got[0].ExeID != "exe_w1" || !got[0].IsDefault {
		t.Fatalf("GET body: %+v", got)
	}

	// DELETE
	req = httptest.NewRequest(http.MethodDelete, "/api/codex-exec/workspaces/ws_a/executors/exe_w1", nil)
	rr = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE: want 204, got %d", rr.Code)
	}

	// GET again — should be empty
	req = httptest.NewRequest(http.MethodGet, "/api/codex-exec/workspaces/ws_a/executors", nil)
	rr = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got) != 0 {
		t.Fatalf("after delete: %+v", got)
	}
}

func TestWorkspaceBinding_PostBadJSON(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s"}, store)
	req := httptest.NewRequest(http.MethodPost, "/api/codex-exec/workspaces/ws_a/executors", bytes.NewReader([]byte(`!`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run WorkspaceBinding`
Expected: 404 / route absent.

- [ ] **Step 3: Implement handler**

`/root/agentserver/internal/codexexecgateway/handlers/workspace_binding.go`:
```go
package handlers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/agentserver/agentserver/internal/codexexecgateway"
	"github.com/go-chi/chi/v5"
)

// BindingStore is the subset of *codexexecgateway.Store required by the workspace
// binding handlers.
type BindingStore interface {
	BindWorkspaceExecutor(ctx context.Context, workspaceID, exeID string, isDefault bool) error
	UnbindWorkspaceExecutor(ctx context.Context, workspaceID, exeID string) error
	ListWorkspaceExecutors(ctx context.Context, workspaceID string) ([]codexexecgateway.ConnectedExecutor, error)
}

type bindRequest struct {
	ExeID     string `json:"exe_id"`
	IsDefault bool   `json:"is_default"`
}

func PostBinding(store BindingStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := chi.URLParam(r, "wid")
		var req bindRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if req.ExeID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "exe_id required"})
			return
		}
		if err := store.BindWorkspaceExecutor(r.Context(), wid, req.ExeID, req.IsDefault); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "bind"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
	}
}

func DeleteBinding(store BindingStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := chi.URLParam(r, "wid")
		exeID := chi.URLParam(r, "exe_id")
		if err := store.UnbindWorkspaceExecutor(r.Context(), wid, exeID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unbind"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func ListBinding(store BindingStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := chi.URLParam(r, "wid")
		rows, err := store.ListWorkspaceExecutors(r.Context(), wid)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list"})
			return
		}
		if rows == nil {
			rows = []codexexecgateway.ConnectedExecutor{}
		}
		writeJSON(w, http.StatusOK, rows)
	}
}
```

- [ ] **Step 4: Wire routes in server.go**

In `Routes()`:
```go
r.Route("/api/codex-exec/workspaces/{wid}/executors", func(r chi.Router) {
	r.Post("/", handlers.PostBinding(s.store))
	r.Get("/", handlers.ListBinding(s.store))
	r.Delete("/{exe_id}", handlers.DeleteBinding(s.store))
})
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run WorkspaceBinding`
Expected: 2 PASS.

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/handlers/workspace_binding.go internal/codexexecgateway/handlers_workspace_binding_test.go internal/codexexecgateway/server.go
git commit -m "feat(codex-exec-gateway): workspace binding admin endpoints"
```

---

## Task 11: Internal endpoint — GET /api/exec-gateway/connected

Spec: returns the intersection of currently-connected executors and the
workspace's binding. Protected by `Authorization: Bearer <shared>` against
`Config.InternalSharedSecret`.

**Files:**
- Create: `/root/agentserver/internal/codexexecgateway/handlers/middleware.go`
- Create: `/root/agentserver/internal/codexexecgateway/handlers/internal_api.go`
- Create: `/root/agentserver/internal/codexexecgateway/handlers_internal_api_test.go`
- Modify: `/root/agentserver/internal/codexexecgateway/server.go`

- [ ] **Step 1: Write failing test**

`/root/agentserver/internal/codexexecgateway/handlers_internal_api_test.go`:
```go
package codexexecgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestInternalConnected_RequiresSharedSecret(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s3cret"}, store)
	req := httptest.NewRequest(http.MethodGet, "/api/exec-gateway/connected?workspace_id=ws_a", nil)
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestInternalConnected_ReturnsIntersection(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s3cret"}, store)
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)

	// Seed: two executors bound to workspace, one connected.
	for _, e := range []Executor{
		{ExeID: "exe_on", UserID: "u", Description: "online", DefaultCwd: "/x", RegisteredAt: time.Now().UTC()},
		{ExeID: "exe_off", UserID: "u", Description: "offline", DefaultCwd: "/y", RegisteredAt: time.Now().UTC()},
	} {
		store.CreateExecutor(context.Background(), e, "h")
		store.BindWorkspaceExecutor(context.Background(), "ws_a", e.ExeID, e.ExeID == "exe_on")
	}
	srv.registry.Register("exe_on", new(websocket.Conn))

	req, _ := http.NewRequest(http.MethodGet,
		hs.URL+"/api/exec-gateway/connected?workspace_id=ws_a", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got []ConnectedExecutor
	json.NewDecoder(resp.Body).Decode(&got)
	if len(got) != 1 || got[0].ExeID != "exe_on" {
		t.Fatalf("intersection: %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run InternalConnected`
Expected: 404 / route absent.

- [ ] **Step 3: Implement middleware**

`/root/agentserver/internal/codexexecgateway/handlers/middleware.go`:
```go
package handlers

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireSharedSecret rejects requests whose Authorization: Bearer header
// does not constant-time-match `secret`.
func RequireSharedSecret(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(h, prefix) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			got := h[len(prefix):]
			if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

- [ ] **Step 4: Implement internal_api.go (Connected only — Revoke in Task 12)**

`/root/agentserver/internal/codexexecgateway/handlers/internal_api.go`:
```go
package handlers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/agentserver/agentserver/internal/codexexecgateway"
)

// ConnectedStore is the subset of *codexexecgateway.Store required by Connected.
type ConnectedStore interface {
	ConnectedExecutorsForWorkspace(ctx context.Context, workspaceID string, connectedIDs []string) ([]codexexecgateway.ConnectedExecutor, error)
}

// Registry is satisfied by *codexexecgateway.ConnRegistry.
type Registry interface {
	ConnectedIDs() []string
}

// Connected returns the intersection of (workspace's bound executors) ∩
// (currently-connected exe_ids). Used by codex-app-gateway when composing
// the per-turn manifest.
func Connected(store ConnectedStore, reg Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := r.URL.Query().Get("workspace_id")
		if wid == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workspace_id required"})
			return
		}
		ids := reg.ConnectedIDs()
		rows, err := store.ConnectedExecutorsForWorkspace(r.Context(), wid, ids)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list"})
			return
		}
		if rows == nil {
			rows = []codexexecgateway.ConnectedExecutor{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rows)
	}
}
```

- [ ] **Step 5: Wire routes in server.go**

```go
r.Route("/api/exec-gateway", func(r chi.Router) {
	r.Use(handlers.RequireSharedSecret(s.config.InternalSharedSecret))
	r.Get("/connected", handlers.Connected(s.store, s.registry))
})
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run InternalConnected`
Expected: 2 PASS.

- [ ] **Step 7: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/handlers/middleware.go internal/codexexecgateway/handlers/internal_api.go internal/codexexecgateway/handlers_internal_api_test.go internal/codexexecgateway/server.go
git commit -m "feat(codex-exec-gateway): internal /api/exec-gateway/connected"
```

---

## Task 12: Internal endpoint — POST /api/exec-gateway/revoke-turn

**Files:**
- Modify: `/root/agentserver/internal/codexexecgateway/handlers/internal_api.go`
- Create: `/root/agentserver/internal/codexexecgateway/handlers_revoke_test.go`
- Modify: `/root/agentserver/internal/codexexecgateway/server.go`

- [ ] **Step 1: Write failing test**

`/root/agentserver/internal/codexexecgateway/handlers_revoke_test.go`:
```go
package codexexecgateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRevokeTurn_AddsToSet(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "secret"}, store)
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)

	body := bytes.NewReader([]byte(`{"turn_id":"trn_42","exp":9999999999}`))
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/exec-gateway/revoke-turn", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
	if !srv.revoked.Contains("trn_42") {
		t.Fatal("revoked set should contain trn_42")
	}
}

func TestRevokeTurn_RejectsBadAuth(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "secret"}, store)
	req := httptest.NewRequest(http.MethodPost, "/api/exec-gateway/revoke-turn",
		bytes.NewReader([]byte(`{"turn_id":"x","exp":1}`)))
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestRevokeTurn_BadJSON(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "secret"}, store)
	req := httptest.NewRequest(http.MethodPost, "/api/exec-gateway/revoke-turn",
		bytes.NewReader([]byte(`!`)))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run RevokeTurn`
Expected: 404 (route absent).

- [ ] **Step 3: Extend handlers/internal_api.go**

Append:
```go
// RevokedAdder is satisfied by *codexexecgateway.RevokedSet.
type RevokedAdder interface {
	Add(turnID string, exp int64)
}

type revokeRequest struct {
	TurnID string `json:"turn_id"`
	Exp    int64  `json:"exp"`
}

// RevokeTurn adds a turn_id to the in-memory revoked set so future bridge
// connect attempts presenting that turn's CODEX_EXEC_GATEWAY_TOKEN are
// rejected even within the token's exp window.
func RevokeTurn(rev RevokedAdder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req revokeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if req.TurnID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "turn_id required"})
			return
		}
		// If caller omits exp, default to "1 hour from now" (spec turn slack).
		if req.Exp == 0 {
			req.Exp = timeNowUnix() + 3600
		}
		rev.Add(req.TurnID, req.Exp)
		w.WriteHeader(http.StatusNoContent)
	}
}

// timeNowUnix exists as a small indirection so tests could later swap time.
func timeNowUnix() int64 { return time.Now().Unix() }
```

Add the import `"time"` to the file.

- [ ] **Step 4: Wire route in server.go**

Inside the existing `Route("/api/exec-gateway", ...)` block, add:
```go
r.Post("/revoke-turn", handlers.RevokeTurn(s.revoked))
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run RevokeTurn`
Expected: 3 PASS.

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/handlers/internal_api.go internal/codexexecgateway/handlers_revoke_test.go internal/codexexecgateway/server.go
git commit -m "feat(codex-exec-gateway): internal /api/exec-gateway/revoke-turn"
```

---

## Task 13: Frame-pump close & error propagation tests

The bridge handler closes the bridge side and lets the inbound handler
own the inbound conn lifecycle. We need to verify:

1. Closing the bridge side causes both pumps to return (no goroutine leak).
2. Closing the inbound side causes both pumps to return.
3. After bridge close, the inbound conn is *still* in the registry (the
   inbound handler owns it).

**Files:**
- Modify: `/root/agentserver/internal/codexexecgateway/bridge_test.go`

- [ ] **Step 1: Write failing test**

Append to `bridge_test.go`:
```go
import "runtime"

func TestBridge_CloseFromBridgeSidePropagates(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	inbound := connectInbound(t, srv, hs.URL, "exe_close1")
	defer inbound.Close(websocket.StatusInternalError, "test cleanup")

	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1", ExeIDs: []string{"exe_close1"},
		IAT: now, EXP: now + 60,
	})
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_close1?token=" + tok

	beforeG := runtime.NumGoroutine()
	bridge, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("bridge dial: %v", err)
	}
	// Active pair: 2 pump goroutines + 1 handler goroutine on the server side.
	bridge.Close(websocket.StatusNormalClosure, "client done")

	// Give the pumps time to wind down.
	time.Sleep(200 * time.Millisecond)
	afterG := runtime.NumGoroutine()
	// Allow some scheduler slack: assert no NET growth of more than 1.
	if afterG > beforeG+1 {
		t.Fatalf("possible goroutine leak: before=%d after=%d", beforeG, afterG)
	}

	// Inbound conn must still be registered.
	if _, ok := srv.registry.Lookup("exe_close1"); !ok {
		t.Fatal("inbound should still be registered after bridge close")
	}
}

func TestBridge_CloseFromInboundSidePropagates(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	inbound := connectInbound(t, srv, hs.URL, "exe_close2")

	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1", ExeIDs: []string{"exe_close2"},
		IAT: now, EXP: now + 60,
	})
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_close2?token=" + tok
	bridge, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("bridge dial: %v", err)
	}
	defer bridge.Close(websocket.StatusInternalError, "test cleanup")

	// Close inbound; the bridge pump should observe and return.
	inbound.Close(websocket.StatusNormalClosure, "executor offline")

	// The bridge client should observe close within a short window.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err = bridge.Read(ctx)
	if err == nil {
		t.Fatal("bridge.Read should have errored after inbound close")
	}
}
```

- [ ] **Step 2: Run tests to verify they pass**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run BridgeClose -v`
Expected: 2 PASS. If they fail, the bridge handler is leaking goroutines
— inspect that `pumpFrames` returns on close-status errors and that
`<-errCh` is read twice.

- [ ] **Step 3: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/bridge_test.go
git commit -m "test(codex-exec-gateway): bridge close + leak propagation tests"
```

---

## Task 14: End-to-end byte-fidelity test (paired fake codex-exec + fake bridge client)

Already covered in Task 8 (`TestBridge_PairsAndForwardsBidirectional`), but
we need a stronger test: many text frames + a binary frame, varying
sizes, ordered correctly. Plus a curl-based smoke check that the bridge
URL is reachable.

**Files:**
- Modify: `/root/agentserver/internal/codexexecgateway/bridge_test.go`

- [ ] **Step 1: Write failing test**

Append to `bridge_test.go`:
```go
func TestBridge_E2EByteFidelity(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	inbound := connectInbound(t, srv, hs.URL, "exe_e2e")
	defer inbound.Close(websocket.StatusNormalClosure, "")

	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1", ExeIDs: []string{"exe_e2e"},
		IAT: now, EXP: now + 60,
	})
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_e2e?token=" + tok
	bridge, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("bridge dial: %v", err)
	}
	defer bridge.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Bridge -> inbound: 5 distinct JSON-RPC text frames, each must arrive
	// as ONE frame (boundary preserved) in order.
	sent := []string{
		`{"id":1,"method":"initialize","params":{"clientName":"x"}}`,
		`{"id":1,"result":{}}`,
		`{"method":"initialized","params":{}}`,
		`{"id":2,"method":"process/start","params":{"processId":"p1","argv":["bash","-lc","echo hi"]}}`,
		`{"id":2,"result":{"processId":"p1"}}`,
	}
	for _, s := range sent {
		if err := bridge.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
			t.Fatalf("bridge.Write %q: %v", s, err)
		}
	}
	for _, want := range sent {
		mt, data, err := inbound.Read(ctx)
		if err != nil {
			t.Fatalf("inbound.Read: %v", err)
		}
		if mt != websocket.MessageText {
			t.Fatalf("frame type drift: got %v want text", mt)
		}
		if string(data) != want {
			t.Fatalf("frame contents drift: got %q want %q", data, want)
		}
	}

	// Inbound -> bridge: large binary frame (32 KiB) round-trips intact.
	big := make([]byte, 32*1024)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if err := inbound.Write(ctx, websocket.MessageBinary, big); err != nil {
		t.Fatalf("inbound.Write big: %v", err)
	}
	mt, data, err := bridge.Read(ctx)
	if err != nil {
		t.Fatalf("bridge.Read big: %v", err)
	}
	if mt != websocket.MessageBinary || len(data) != len(big) {
		t.Fatalf("big frame: mt=%v len=%d", mt, len(data))
	}
	for i := range big {
		if data[i] != big[i] {
			t.Fatalf("byte %d: got %x want %x", i, data[i], big[i])
		}
	}
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -run E2EByteFidelity -v`
Expected: PASS.

- [ ] **Step 3: Manual smoke (optional, run if Postgres + service are deployed)**

Start service:
```bash
CXG_DATABASE_URL=postgres://… \
CXG_CAPTOKEN_HMAC_SECRET=secret \
CXG_INTERNAL_SHARED_SECRET=is \
go run ./cmd/codex-exec-gateway
```
Then in another shell:
```bash
curl -sS http://localhost:6060/healthz
```
Expected output: `ok`

```bash
curl -sS -H "X-User-Id: user_a" \
     -H "Content-Type: application/json" \
     -d '{"display_name":"laptop","description":"d","default_cwd":"/x"}' \
     http://localhost:6060/api/codex-exec/register
```
Expected: HTTP 201 with JSON `{"exe_id":"exe_…","registration_token":"…"}`.

- [ ] **Step 4: Run the full test suite once**

Run: `cd /root/agentserver && TEST_DATABASE_URL=postgres://… go test ./internal/codexexecgateway/... -v`
Expected: every test PASS or SKIP. If any FAIL, fix before continuing.

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/codexexecgateway/bridge_test.go
git commit -m "test(codex-exec-gateway): end-to-end byte fidelity across paired ws"
```

---

## Self-Review

Run this checklist before declaring the plan complete.

**1. Spec coverage** — every binding requirement in Subsystem 3 of the
design spec maps to a task here:

| Spec requirement | Task |
|---|---|
| Accept `/codex-exec/{exe_id}` with persistent tunnel-token bcrypt | 7 |
| Single conn per exe_id; new evicts old | 5 (registry), 7 (handler uses it) |
| Update `executors.last_seen_at` | 7 |
| Accept `/bridge/{exe_id}` with capability-token JWT-style HMAC | 4 (verify), 8 (handler) |
| Reject 401 bad sig / 401 expired / 403 not-in-allow-list / 401 revoked / 503 not-connected | 8 |
| Frame-level transparent forwarding (preserve boundaries) | 8, 14 |
| Auth checked once at connect, not per frame | 8 (auth in handler before pumps start) |
| `executors` + `workspace_executors` Postgres tables owned here | 2 |
| Tunnel tokens stored as bcrypt hashes; never logged | 9 (bcrypt hash; raw token only in 201 response, never `s.logger.…`) |
| `POST /api/codex-exec/register` | 9 |
| Workspace binding `POST/DELETE/GET` | 10 |
| `GET /api/exec-gateway/connected?workspace_id=…` shared-secret protected | 11 |
| `POST /api/exec-gateway/revoke-turn` | 12 |
| In-memory revoked turn_id set, cap 10k, exp-pruned | 6 |
| Phase 1 ws defaults: ping 30s, idle 5m, env-overridable | 1 (`Config.PingInterval`/`IdleTimeout`) |
| Module path `github.com/agentserver/agentserver/internal/codexexecgateway` | header |

**2. Placeholder scan** — search for "TBD", "TODO", "fill in", "similar to":
none present in the plan body. Each task contains exact file paths,
complete code, and runnable commands.

**3. Type consistency** — names used across tasks match:
- `CapPayload` (auth.go) — referenced unchanged in tasks 4, 8.
- `ConnRegistry` with methods `Register / Lookup / Unregister / ConnectedIDs` — defined in task 5; used by tasks 7, 8, 11.
- `RevokedSet` with methods `Add / Contains / Prune / Size` — defined in task 6; used by tasks 8, 12.
- `Store` methods consistent: `CreateExecutor / GetExecutor / GetRegistrationTokenHash / UpdateLastSeen / BindWorkspaceExecutor / UnbindWorkspaceExecutor / ListWorkspaceExecutors / ConnectedExecutorsForWorkspace` (tasks 2, 3, 7, 9, 10, 11).
- `Executor` and `ConnectedExecutor` JSON shapes consistent across handler test bodies and store tests.

If any subsequent edit drifts these names, the corresponding test will
fail at compile time — that is the safety net.

---

## Execution Handoff

Plan complete and saved to `/root/agentserver/docs/superpowers/plans/2026-05-05-codex-exec-gateway.md`. Two execution options:

**1. Subagent-Driven (recommended)** — dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
