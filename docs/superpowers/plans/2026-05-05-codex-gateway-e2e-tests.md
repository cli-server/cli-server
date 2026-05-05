# codex-app-gateway + codex-exec-gateway — End-to-End Acceptance Harness Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up a docker-compose stack — Postgres + codex-app-gateway +
codex-exec-gateway + a fake `codex-app` ws client + a fake `codex-exec` ws
server — and run scripted scenarios that prove every Acceptance bullet
(1–6) of the design spec. Wire it into `make test-e2e-codex` and a CI
workflow gating on the codex stack changes. This is the gate that
declares phase 1 shippable.

**Architecture:**

```
fake_codex_app  ──wss──>  codex-app-gateway  ──spawn──>  codex (real CLI)
   (Go test bin)              ^                                │
                              │                                │ ws bridge
                              │                                ▼
                              │                         codex-exec-gateway
                              │                                │
                              └─── /api/exec-gateway ──────────┤
                                   /connected                  │ ws inbound
                              ▲                                ▼
                              │                          fake_codex_exec
                              │                          (Go test bin)
                              │
                       Postgres (one db, two table sets)
```

The fake-app drives the user side (codex-app v2 JSON-RPC); the fake-exec
drives the executor side (codex exec-server JSON-RPC). The codex CLI
itself is REAL — that's what is being validated end-to-end (P1–P4 fork
patches).

**Tech Stack:** Go 1.26, docker compose v2, Postgres 16, the real `codex`
CLI (built from the agentserver fork), `nhooyr.io/websocket` for the fake
ws clients.

**Spec:**
`docs/superpowers/specs/2026-05-05-codex-app-gateway-and-exec-gateway-design.md`
in this repo (read § Testing strategy + § Acceptance first).

**Depends on:** Plans 1 (codex fork P1–P4), 2 (codex-app-gateway), and 3
(codex-exec-gateway) being executable to a `docker build`-able state.
This plan does not implement gateway code — it consumes it.

**Working directory:** All tasks operate in `/root/agentserver`.

---

## File Structure

| File | Responsibility |
|---|---|
| `docker-compose-codex.yml` | The acceptance stack: postgres + 2 gateways + minio (workspace store) + fixtures-loader |
| `Dockerfile.codex-fake-app` | Build container for `cmd/codex-fake-app` |
| `Dockerfile.codex-fake-exec` | Build container for `cmd/codex-fake-exec` |
| `cmd/codex-fake-app/main.go` | Fake codex-app client: opens ws, drives a scripted session, asserts on notifications, exits 0/non-zero |
| `cmd/codex-fake-exec/main.go` | Fake codex-exec server: connects to `/codex-exec/{exe_id}`, replies to scripted exec-server RPCs, records calls received |
| `internal/codexe2e/scenarios/scripted_turn.go` | Scenario 1 driver (single executor, single shell call) |
| `internal/codexe2e/scenarios/multi_executor.go` | Scenario 2 driver (two executors, env_id steering) |
| `internal/codexe2e/scenarios/reconnect_replay.go` | Scenario 3 driver (disconnect mid-turn → reconnect → thread/read replays) |
| `internal/codexe2e/fixtures/loader.go` | Provisioner: creates user JWT, registers executors, binds them to a workspace |
| `internal/codexe2e/harness/harness.go` | Test harness: `docker compose up`, wait for healthy, expose URLs, `down` on cleanup |
| `internal/codexe2e/e2e_test.go` | The `//go:build e2e_codex` test that drives the three scenarios |
| `Makefile` (modify) | Add `test-e2e-codex` target that runs the e2e tests |
| `.github/workflows/codex-e2e.yml` | CI workflow that runs the e2e suite on PRs touching codex-app-gateway / codex-exec-gateway / codex fork |

---

## Task 1: Compose skeleton — postgres + both gateways + minio + fixtures-loader

**Goal:** Bring the stack up healthy, verify both gateways respond on
`/healthz`, before adding any fakes.

**Files:**
- Create: `docker-compose-codex.yml`
- Create: `internal/codexe2e/harness/harness.go`
- Create: `internal/codexe2e/harness/harness_test.go`

- [ ] **Step 1: Write `docker-compose-codex.yml`**

```yaml
# Acceptance harness for codex-app-gateway + codex-exec-gateway.
# Brought up by `make test-e2e-codex` (or directly via
# `docker compose -f docker-compose-codex.yml up -d --wait`).
#
# Stack contents:
#   postgres        — shared db, distinct schemas per gateway
#   minio           — S3-compatible workspace store for codex sessions/jsonl
#   minio-init      — creates the codex-sessions bucket
#   codex-app-gateway     — speaks app-server v2 JSON-RPC to fake-app
#   codex-exec-gateway    — speaks exec-server JSON-RPC to fake-exec + bridge
#   fake-codex-app  — built but not auto-run; the test harness exec's it
#   fake-codex-exec — built but not auto-run; the test harness exec's it

services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: codex
      POSTGRES_PASSWORD: codex
      POSTGRES_DB: codex
    healthcheck:
      test: ["CMD", "pg_isready", "-U", "codex"]
      interval: 1s
      timeout: 3s
      retries: 30
    ports:
      - "55432:5432"

  minio:
    image: minio/minio:latest
    command: server /data --console-address ":9001"
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/ready"]
      interval: 2s
      timeout: 3s
      retries: 30
    ports:
      - "59000:9000"

  minio-init:
    image: minio/mc:latest
    depends_on:
      minio:
        condition: service_healthy
    entrypoint:
      - sh
      - -c
      - |
        mc alias set local http://minio:9000 minioadmin minioadmin
        mc mb -p local/codex-sessions || true
        echo done

  codex-app-gateway:
    build:
      context: .
      dockerfile: Dockerfile.codex-app-gateway
    environment:
      # All env-var names must match codexappgateway.LoadConfigFromEnv (Plan 2a).
      # Naming convention: CXG_* prefix shared by both gateways.
      CXG_PORT: "6050"
      CXG_DATABASE_URL: "postgres://codex:codex@postgres:5432/codex?sslmode=disable&search_path=codex_app"
      CXG_CAPTOKEN_HMAC_SECRET: "e2e-test-hmac-secret-32-bytes-min!!"
      # User-facing JWT public key (PEM). For e2e the fixtures-loader mints
      # tokens with the matching RSA-2048 private key; the pubkey ships in
      # the env. Provided by .env or set by `make test-e2e-codex` setup-keypair
      # task (see Task 1.5). Must be the RS256 public key (PEM) corresponding
      # to the private key used by the fixtures Mint helper.
      CXG_AUTH_JWT_PUBLIC_KEY: ${CXG_AUTH_JWT_PUBLIC_KEY:-}
      # Shared secret used to call codex-exec-gateway's internal admin API
      # (/api/exec-gateway/connected, /api/exec-gateway/revoke-turn).
      CXG_INTERNAL_SHARED_SECRET: "e2e-internal-bearer-token"
      CXG_EXEC_GATEWAY_URL: "ws://codex-exec-gateway:6060"
      CXG_EXEC_GATEWAY_HTTP_URL: "http://codex-exec-gateway:6060"
      CXG_S3_ENDPOINT: "http://minio:9000"
      CXG_S3_BUCKET: "codex-sessions"
      CXG_S3_ACCESS_KEY_ID: "minioadmin"
      CXG_S3_SECRET_ACCESS_KEY: "minioadmin"
      CXG_S3_PATH_STYLE: "true"
      CXG_LLMPROXY_URL: "http://mock-llm:9999"
      CXG_LOG_LEVEL: "debug"
      # Real codex CLI path inside the image (installed by Dockerfile).
      CXG_BIN: "/usr/local/bin/codex"
    depends_on:
      postgres:
        condition: service_healthy
      minio-init:
        condition: service_completed_successfully
      codex-exec-gateway:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:6050/healthz"]
      interval: 1s
      timeout: 3s
      retries: 60
    ports:
      - "56050:6050"

  codex-exec-gateway:
    build:
      context: .
      dockerfile: Dockerfile.codex-exec-gateway
    environment:
      # All env-var names must match codexexecgateway.LoadConfigFromEnv (Plan 3).
      CXG_PORT: "6060"
      CXG_DATABASE_URL: "postgres://codex:codex@postgres:5432/codex?sslmode=disable&search_path=codex_exec"
      CXG_CAPTOKEN_HMAC_SECRET: "e2e-test-hmac-secret-32-bytes-min!!"
      CXG_INTERNAL_SHARED_SECRET: "e2e-internal-bearer-token"
      CXG_LOG_LEVEL: "debug"
    depends_on:
      postgres:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:6060/healthz"]
      interval: 1s
      timeout: 3s
      retries: 60
    ports:
      - "56060:6060"

  mock-llm:
    # A tiny scripted OpenAI-compatible /v1/responses endpoint that returns
    # a fixed sequence of tool calls. Built once in Task 5; until then,
    # leave the image as a stub that just returns 200 on /healthz.
    image: ghcr.io/agentserver/codex-mock-llm:e2e
    pull_policy: never
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:9999/healthz"]
      interval: 1s
      timeout: 3s
      retries: 60

volumes: {}
```

- [ ] **Step 2: Write `internal/codexe2e/harness/harness.go`**

```go
// Package harness brings up and tears down the docker-compose-codex.yml
// stack for the e2e tests. It is import-only from tests under the
// `e2e_codex` build tag.
package harness

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"
)

// Stack is a brought-up docker-compose-codex stack. URLs are the
// host-side (mapped) addresses for tests to dial.
type Stack struct {
	AppGatewayWS       string // ws://localhost:56050
	AppGatewayHTTP     string // http://localhost:56050
	ExecGatewayWS      string // ws://localhost:56060
	ExecGatewayHTTP    string // http://localhost:56060
	InternalBearer        string
	UserJWTPrivateKeyPath string // /tmp/test-jwt.key — written by Task 1.5
	HMACSecret            string
	composeFile           string
}

const composeFileDefault = "docker-compose-codex.yml"

// Up brings up the compose stack, waits for healthy, and registers
// t.Cleanup to bring it down. The composeFile path is relative to the
// agentserver repo root; tests should `cd` there first or pass an
// absolute path.
func Up(t *testing.T) *Stack {
	t.Helper()
	s := &Stack{
		AppGatewayWS:    "ws://localhost:56050",
		AppGatewayHTTP:  "http://localhost:56050",
		ExecGatewayWS:   "ws://localhost:56060",
		ExecGatewayHTTP: "http://localhost:56060",
		InternalBearer:        "e2e-internal-bearer-token",
		UserJWTPrivateKeyPath: "/tmp/test-jwt.key", // provisioned by Task 1.5 / make test-e2e-codex
		HMACSecret:            "e2e-test-hmac-secret-32-bytes-min!!",
		composeFile:           composeFileDefault,
	}

	cmd := exec.Command("docker", "compose", "-f", s.composeFile,
		"up", "-d", "--wait", "--wait-timeout", "180")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compose up failed: %v\n%s", err, out)
	}

	t.Cleanup(func() {
		dump := exec.Command("docker", "compose", "-f", s.composeFile, "logs", "--no-color")
		logs, _ := dump.CombinedOutput()
		t.Logf("---- compose logs ----\n%s", logs)
		down := exec.Command("docker", "compose", "-f", s.composeFile, "down", "-v", "--remove-orphans")
		_ = down.Run()
	})

	if err := waitHealthy(s.AppGatewayHTTP+"/healthz", 60*time.Second); err != nil {
		t.Fatalf("app-gateway not healthy: %v", err)
	}
	if err := waitHealthy(s.ExecGatewayHTTP+"/healthz", 60*time.Second); err != nil {
		t.Fatalf("exec-gateway not healthy: %v", err)
	}
	return s
}

func waitHealthy(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("never became healthy: %w", lastErr)
}
```

- [ ] **Step 3: Write a smoke test that just brings the stack up**

`internal/codexe2e/harness/harness_test.go`:
```go
//go:build e2e_codex

package harness

import (
	"net/http"
	"testing"
)

func TestStackComesUp(t *testing.T) {
	s := Up(t)
	for _, u := range []string{s.AppGatewayHTTP + "/healthz", s.ExecGatewayHTTP + "/healthz"} {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		if resp.StatusCode != 200 {
			t.Errorf("GET %s: status=%d", u, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
```

- [ ] **Step 4: Build the gateway images locally (one-shot, dependency check)**

```bash
cd /root/agentserver
docker compose -f docker-compose-codex.yml build codex-app-gateway codex-exec-gateway
```
Expected: both images build successfully. If they don't, Plans 2 and 3
have not produced their Dockerfiles yet — block this plan on those.

- [ ] **Step 5: Run the smoke test**

```bash
cd /root/agentserver
go test -tags=e2e_codex ./internal/codexe2e/harness/... -v -count=1 -timeout=5m
```
Expected:
```
=== RUN   TestStackComesUp
--- PASS: TestStackComesUp (XX.XXs)
PASS
```
And both `/healthz` endpoints return 200.

- [ ] **Step 6: Commit**

```bash
git add docker-compose-codex.yml internal/codexe2e/harness/
git commit -m "test(codex-e2e): compose skeleton + harness Up/Down"
```

---

## Task 1.5: Local JWT keypair provisioning

**Goal:** Generate the RS256 keypair used for user-JWT auth in the e2e
stack. The fixtures Mint helper signs with the private key
(`/tmp/test-jwt.key`); the gateway verifies with the public key exported
into docker-compose as `CXG_AUTH_JWT_PUBLIC_KEY`. This task documents the
shell snippet that the `make test-e2e-codex` target runs before
`docker compose up`.

This is a stop-gap for phase 1 — the production JWT story (RS256 vs HS256,
who mints, how compose mounts the keypair) is deferred. For e2e the
keypair is generated on the fly and lives only in `/tmp`.

- [ ] **Step 1: Add the keypair-provisioning snippet to the e2e Make target**

In `Makefile` (or whichever `make test-e2e-codex` rule exists), prepend
the following commands to the existing recipe so they run before
`docker compose up`:

```makefile
test-e2e-codex: setup-jwt-keypair
	cd /root/agentserver && go test -tags=e2e_codex ./internal/codexe2e/... -count=1 -timeout=15m

.PHONY: setup-jwt-keypair
setup-jwt-keypair:
	@echo ">>> generating ephemeral RS256 keypair for e2e"
	@openssl genrsa -out /tmp/test-jwt.key 2048 2>/dev/null
	@openssl rsa -in /tmp/test-jwt.key -pubout -out /tmp/test-jwt.pub 2>/dev/null
	@chmod 600 /tmp/test-jwt.key
	@echo "CXG_AUTH_JWT_PUBLIC_KEY=$$(cat /tmp/test-jwt.pub)" > .env.codex-e2e
	@echo ">>> wrote /tmp/test-jwt.key (private) and .env.codex-e2e (pubkey)"
```

The `.env.codex-e2e` file is picked up by `docker compose
--env-file .env.codex-e2e -f docker-compose-codex.yml up`. The PEM string
is multi-line; bash's `$(cat …)` substitution preserves the newlines and
docker-compose's `${VAR}` substitution expands it correctly into the YAML
literal block (the `${CXG_AUTH_JWT_PUBLIC_KEY:-}` substitution we use
keeps the value as a single env value, with newlines).

- [ ] **Step 2: Update the harness Up() to pass the env file to compose**

In `internal/codexe2e/harness/harness.go`, when shelling out to
`docker compose`, include `--env-file .env.codex-e2e`:

```go
cmd := exec.CommandContext(ctx, "docker", "compose",
    "--env-file", ".env.codex-e2e",
    "-f", s.composeFile, "up", "-d", "--wait")
```

- [ ] **Step 3: Verify the keypair is present and the gateway accepts a fixtures-minted JWT**

```bash
cd /root/agentserver
make setup-jwt-keypair
test -s /tmp/test-jwt.key && test -s /tmp/test-jwt.pub
grep -q "BEGIN PUBLIC KEY" .env.codex-e2e
```
Expected: all three commands exit 0.

A subsequent run of any e2e scenario (Task 5 onwards) implicitly verifies
that the gateway's RS256 verifier accepts the fixtures Mint output — a
401 from `/healthz`-protected endpoints would surface there.

- [ ] **Step 4: Commit**

```bash
git add Makefile .gitignore
git commit -m "test(codex-e2e): RS256 keypair provisioning for fixtures Mint"
```

(Add `.env.codex-e2e` and `/tmp/test-jwt.*` patterns to `.gitignore` if
not already excluded by repo-wide rules.)

---

## Task 2: Fake codex-exec executor

**Goal:** A small Go program that connects to
`/codex-exec/{exe_id}` on codex-exec-gateway, authenticates with a
registration token, and replies to the bare minimum exec-server JSON-RPC
methods needed for codex's `shell` tool to complete: `process/start`,
`process/wait`, `process/kill` (and `initialize` if codex sends one).
For phase 1 the fake responds with a scripted stdout/exit_code per
inbound `process/start.command`.

**Files:**
- Create: `cmd/codex-fake-exec/main.go`
- Create: `cmd/codex-fake-exec/scripts.go`
- Create: `cmd/codex-fake-exec/main_test.go`
- Create: `Dockerfile.codex-fake-exec`

- [ ] **Step 1: Write the failing test**

`cmd/codex-fake-exec/main_test.go`:
```go
package main

import (
	"encoding/json"
	"testing"
)

func TestScriptLookup_EchoHello(t *testing.T) {
	s := DefaultScripts()
	out, code, ok := s.Lookup([]string{"bash", "-lc", "echo hello"})
	if !ok {
		t.Fatal("expected default echo script to match")
	}
	if out != "hello\n" || code != 0 {
		t.Errorf("got out=%q code=%d", out, code)
	}
}

func TestScriptLookup_Pwd(t *testing.T) {
	s := DefaultScripts()
	out, code, _ := s.Lookup([]string{"pwd"})
	if out != "/workspace\n" || code != 0 {
		t.Errorf("got out=%q code=%d", out, code)
	}
}

func TestScriptLookup_Unknown(t *testing.T) {
	s := DefaultScripts()
	_, _, ok := s.Lookup([]string{"weird-binary"})
	if ok {
		t.Error("unknown command should not match")
	}
}

func TestRPCEnvelope_RoundTrip(t *testing.T) {
	in := jsonRPCMessage{JSONRPC: "2.0", ID: json.RawMessage(`1`),
		Method: "process/start", Params: json.RawMessage(`{"command":["echo","hi"]}`)}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out jsonRPCMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Method != "process/start" {
		t.Errorf("method=%q", out.Method)
	}
}
```

- [ ] **Step 2: Implement `cmd/codex-fake-exec/scripts.go`**

```go
package main

import "strings"

// Scripts is a tiny in-memory pattern→reply table for shell commands.
type Scripts struct {
	entries []scriptEntry
}

type scriptEntry struct {
	match func([]string) bool
	out   string
	code  int
}

func DefaultScripts() *Scripts {
	return &Scripts{entries: []scriptEntry{
		{matchEcho, "hello\n", 0},
		{matchPwd, "/workspace\n", 0},
		{matchEnvBeta, "running on exe_beta\n", 0},
		{matchEnvAlpha, "running on exe_alpha\n", 0},
		{matchAny, "fake-exec default reply\n", 0},
	}}
}

func (s *Scripts) Lookup(cmd []string) (string, int, bool) {
	for _, e := range s.entries[:len(s.entries)-1] { // skip catch-all in lookup
		if e.match(cmd) {
			return e.out, e.code, true
		}
	}
	return "", 0, false
}

// LookupOrDefault always returns a reply (using catch-all).
func (s *Scripts) LookupOrDefault(cmd []string) (string, int) {
	if out, code, ok := s.Lookup(cmd); ok {
		return out, code
	}
	last := s.entries[len(s.entries)-1]
	return last.out, last.code
}

func joined(cmd []string) string { return strings.Join(cmd, " ") }

func matchEcho(cmd []string) bool { return strings.Contains(joined(cmd), "echo hello") }
func matchPwd(cmd []string) bool  { return len(cmd) >= 1 && cmd[len(cmd)-1] == "pwd" }
func matchEnvAlpha(cmd []string) bool {
	return strings.Contains(joined(cmd), "ALPHA_SCENARIO")
}
func matchEnvBeta(cmd []string) bool {
	return strings.Contains(joined(cmd), "BETA_SCENARIO")
}
func matchAny(_ []string) bool { return true }
```

- [ ] **Step 3: Implement `cmd/codex-fake-exec/main.go`**

```go
// Command codex-fake-exec is a stand-in for `codex exec-server --connect`
// used in the e2e harness. It authenticates with a registration token,
// listens for exec-server JSON-RPC messages on the inbound ws conn, and
// replies with scripted output for `process/start`/`process/wait`. It
// records every received call to /tmp/codex-fake-exec/calls.jsonl so the
// test driver can assert on what codex actually dispatched.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type jsonRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type processStartParams struct {
	Command []string `json:"command"`
	Workdir string   `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type processStartResult struct {
	PID int `json:"pid"`
}

type processWaitResult struct {
	ExitCode         int    `json:"exit_code"`
	AggregatedOutput string `json:"aggregated_output"`
}

func main() {
	var (
		gatewayURL = flag.String("gateway", "ws://codex-exec-gateway:6060", "codex-exec-gateway base URL")
		exeID      = flag.String("exe-id", "", "exe_id to register as")
		token      = flag.String("token", "", "registration token")
		callsPath  = flag.String("calls", "/tmp/codex-fake-exec/calls.jsonl", "path to append received call lines")
	)
	flag.Parse()
	if *exeID == "" || *token == "" {
		log.Fatal("--exe-id and --token are required")
	}
	if err := os.MkdirAll(filepath.Dir(*callsPath), 0o755); err != nil {
		log.Fatal(err)
	}
	calls, err := os.OpenFile(*callsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer calls.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	url := fmt.Sprintf("%s/codex-exec/%s", *gatewayURL, *exeID)
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + *token}},
	})
	if err != nil {
		log.Fatalf("dial %s: %v", url, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "shutdown")

	scripts := DefaultScripts()
	var pidSeq atomic.Int64

	for {
		var msg jsonRPCMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			log.Printf("read: %v", err)
			return
		}
		// Record the inbound message verbatim.
		raw, _ := json.Marshal(msg)
		fmt.Fprintln(calls, string(raw))

		switch msg.Method {
		case "initialize":
			reply(ctx, conn, msg.ID, json.RawMessage(`{"protocolVersion":"1","serverInfo":{"name":"fake-exec"}}`))
		case "process/start":
			var p processStartParams
			_ = json.Unmarshal(msg.Params, &p)
			pid := int(pidSeq.Add(1))
			reply(ctx, conn, msg.ID, mustJSON(processStartResult{PID: pid}))
		case "process/wait":
			// Look up the latest start by reading our own log tail is overkill;
			// for phase-1 fake we use the message that triggered the wait,
			// which carries the PID we returned. The TUI scripts look up by
			// command in scripts; we use the most-recent process/start params
			// in the calls log to pick output.
			out, code := lastStartOutput(*callsPath, scripts)
			reply(ctx, conn, msg.ID, mustJSON(processWaitResult{ExitCode: code, AggregatedOutput: out}))
		case "process/kill":
			reply(ctx, conn, msg.ID, json.RawMessage(`{}`))
		default:
			if len(msg.ID) > 0 {
				reply(ctx, conn, msg.ID, json.RawMessage(`{}`))
			}
		}
	}
}

func reply(ctx context.Context, conn *websocket.Conn, id json.RawMessage, result json.RawMessage) {
	out := jsonRPCMessage{JSONRPC: "2.0", ID: id, Result: result}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := wsjson.Write(cctx, conn, out); err != nil {
		log.Printf("write: %v", err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// lastStartOutput tails the calls log, finds the most-recent process/start
// params, and returns the scripted (output, exit_code) for that command.
func lastStartOutput(path string, scripts *Scripts) (string, int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return scripts.LookupOrDefault([]string{""})
	}
	dec := json.NewDecoder(bytesReader(data))
	var lastCmd []string
	for dec.More() {
		var m jsonRPCMessage
		if err := dec.Decode(&m); err != nil {
			break
		}
		if m.Method == "process/start" {
			var p processStartParams
			if err := json.Unmarshal(m.Params, &p); err == nil {
				lastCmd = p.Command
			}
		}
	}
	return scripts.LookupOrDefault(lastCmd)
}
```

Add a tiny helper file `cmd/codex-fake-exec/bytes_reader.go`:
```go
package main

import "bytes"

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
```

- [ ] **Step 4: Run unit tests**

```bash
cd /root/agentserver
go test ./cmd/codex-fake-exec/... -v -count=1
```
Expected: all 4 tests PASS.

- [ ] **Step 5: Write `Dockerfile.codex-fake-exec`**

```dockerfile
FROM golang:1.26-trixie AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o codex-fake-exec ./cmd/codex-fake-exec

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/codex-fake-exec /usr/local/bin/codex-fake-exec
ENTRYPOINT ["codex-fake-exec"]
```

Verify it builds:
```bash
docker build -f Dockerfile.codex-fake-exec -t codex-fake-exec:e2e .
```
Expected: image built, no errors.

- [ ] **Step 6: Commit**

```bash
git add cmd/codex-fake-exec/ Dockerfile.codex-fake-exec
git commit -m "test(codex-e2e): fake codex-exec executor (process/start scripted replies)"
```

---

## Task 3: Fake codex-app TUI client

**Goal:** A Go program that opens a wss to `/codex-app/`, performs
`initialize` → optionally `thread/start` → `turn/start` with a scripted
prompt, accumulates inbound `ServerNotification`s into an event log, and
asserts on the final state. Exits 0 on success, non-zero with diagnostics
on assertion failure.

**Files:**
- Create: `cmd/codex-fake-app/main.go`
- Create: `cmd/codex-fake-app/client.go`
- Create: `cmd/codex-fake-app/client_test.go`
- Create: `Dockerfile.codex-fake-app`

- [ ] **Step 1: Write the failing test for envelope round-trip**

`cmd/codex-fake-app/client_test.go`:
```go
package main

import (
	"encoding/json"
	"testing"
)

func TestRPCEnvelope_RoundTrip(t *testing.T) {
	in := jsonRPCMessage{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize"}
	b, _ := json.Marshal(in)
	var out jsonRPCMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Method != "initialize" {
		t.Errorf("method=%q", out.Method)
	}
}

func TestEventLog_Append_Filter(t *testing.T) {
	log := &EventLog{}
	log.Append("turn/started", json.RawMessage(`{"threadId":"th1","turn":{"id":"t1","status":"in_progress","items":[]}}`))
	log.Append("item/completed", json.RawMessage(`{"threadId":"th1","turnId":"t1","item":{"id":"i","type":"agentMessage","text":"hi"}}`))
	log.Append("turn/completed", json.RawMessage(`{"threadId":"th1","turn":{"id":"t1","status":"completed","items":[]}}`))
	got := log.Filter("item/completed")
	if len(got) != 1 {
		t.Fatalf("got %d entries", len(got))
	}
}
```

- [ ] **Step 2: Implement `cmd/codex-fake-app/client.go`**

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type jsonRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// EventLog is the ordered stream of ServerNotifications received from the
// gateway. Safe for one-writer / many-reader.
type EventLog struct {
	mu      sync.Mutex
	entries []EventEntry
}

type EventEntry struct {
	Method string
	Params json.RawMessage
	At     time.Time
}

func (l *EventLog) Append(method string, params json.RawMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, EventEntry{Method: method, Params: params, At: time.Now()})
}

func (l *EventLog) Snapshot() []EventEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]EventEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

func (l *EventLog) Filter(method string) []EventEntry {
	out := []EventEntry{}
	for _, e := range l.Snapshot() {
		if e.Method == method {
			out = append(out, e)
		}
	}
	return out
}

// Client is a thin codex-app v2 JSON-RPC client.
type Client struct {
	conn   *websocket.Conn
	nextID int
	mu     sync.Mutex
	Log    *EventLog
}

// Dial opens a ws to the gateway and authenticates with the bearer token.
func Dial(ctx context.Context, url, bearer string) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + bearer}},
	})
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	return &Client{conn: conn, Log: &EventLog{}}, nil
}

// Call makes a request and waits for the matching response. Notifications
// arriving in the meantime are appended to Log.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	idJSON, _ := json.Marshal(id)
	pJSON, _ := json.Marshal(params)
	req := jsonRPCMessage{JSONRPC: "2.0", ID: idJSON, Method: method, Params: pJSON}
	if err := wsjson.Write(ctx, c.conn, req); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}
	for {
		var msg jsonRPCMessage
		if err := wsjson.Read(ctx, c.conn, &msg); err != nil {
			return nil, fmt.Errorf("read after %s: %w", method, err)
		}
		if len(msg.ID) > 0 && string(msg.ID) == string(idJSON) {
			if msg.Error != nil {
				return nil, fmt.Errorf("rpc %s: %s", method, msg.Error.Message)
			}
			return msg.Result, nil
		}
		// Notification or unrelated response.
		if msg.Method != "" {
			c.Log.Append(msg.Method, msg.Params)
		}
	}
}

// Notify sends a fire-and-forget client notification (no id).
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	pJSON, _ := json.Marshal(params)
	return wsjson.Write(ctx, c.conn, jsonRPCMessage{JSONRPC: "2.0", Method: method, Params: pJSON})
}

// PumpUntil reads server messages and appends them to Log until either
// `predicate` returns true on the freshly-appended entry, or ctx fires.
func (c *Client) PumpUntil(ctx context.Context, predicate func(EventEntry) bool) error {
	for {
		var msg jsonRPCMessage
		if err := wsjson.Read(ctx, c.conn, &msg); err != nil {
			return fmt.Errorf("pump read: %w", err)
		}
		if msg.Method == "" {
			continue // unsolicited response — ignore
		}
		entry := EventEntry{Method: msg.Method, Params: msg.Params, At: time.Now()}
		c.Log.Append(msg.Method, msg.Params)
		if predicate(entry) {
			return nil
		}
	}
}

func (c *Client) Close() { _ = c.conn.Close(websocket.StatusNormalClosure, "bye") }
```

- [ ] **Step 3: Implement `cmd/codex-fake-app/main.go`**

```go
// Command codex-fake-app is a scripted codex-app v2 client used in the
// e2e harness. It is invoked by the test driver with --scenario, runs
// the scenario against the gateway URL, and exits 0 on success.
//
// Scenarios are minimal here; the bulk of scenario logic lives under
// internal/codexe2e/scenarios/ and uses Dial / Call / PumpUntil from
// client.go.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"
)

func main() {
	var (
		gateway  = flag.String("gateway", "ws://codex-app-gateway:6050/codex-app/", "ws URL")
		bearer   = flag.String("bearer", "", "user JWT bearer")
		scenario = flag.String("scenario", "ping", "scenario to run: ping")
	)
	flag.Parse()
	if *bearer == "" {
		log.Fatal("--bearer is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := Dial(ctx, *gateway, *bearer)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer c.Close()

	switch *scenario {
	case "ping":
		if _, err := c.Call(ctx, "initialize", map[string]any{
			"clientInfo": map[string]string{"name": "fake-app"},
		}); err != nil {
			log.Fatalf("initialize: %v", err)
		}
		fmt.Println("ok")
		os.Exit(0)
	default:
		log.Fatalf("unknown scenario: %s", *scenario)
	}
}
```

- [ ] **Step 4: Run unit tests**

```bash
cd /root/agentserver
go test ./cmd/codex-fake-app/... -v -count=1
```
Expected: 2 tests PASS.

- [ ] **Step 5: Write `Dockerfile.codex-fake-app`** (mirrors fake-exec one)

```dockerfile
FROM golang:1.26-trixie AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o codex-fake-app ./cmd/codex-fake-app

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/codex-fake-app /usr/local/bin/codex-fake-app
ENTRYPOINT ["codex-fake-app"]
```

Verify it builds:
```bash
docker build -f Dockerfile.codex-fake-app -t codex-fake-app:e2e .
```
Expected: image built.

- [ ] **Step 6: Commit**

```bash
git add cmd/codex-fake-app/ Dockerfile.codex-fake-app
git commit -m "test(codex-e2e): fake codex-app client (Dial/Call/PumpUntil + EventLog)"
```

---

## Task 4: Fixture provisioning — user, workspace, executor binding

**Goal:** A reusable Go provisioner that, given a stack, creates a test
user JWT, registers fake-exec executor(s), binds them to a fresh
workspace, and returns all the credentials/ids needed by scenarios.

**Files:**
- Create: `internal/codexe2e/fixtures/loader.go`
- Create: `internal/codexe2e/fixtures/loader_test.go`

- [ ] **Step 1: Write the failing test**

`internal/codexe2e/fixtures/loader_test.go`:
```go
//go:build e2e_codex

package fixtures

import (
	"strings"
	"testing"

	"github.com/agentserver/agentserver/internal/codexe2e/harness"
)

func TestProvision_SingleExecutor(t *testing.T) {
	stack := harness.Up(t)
	f := New(stack)
	ws, err := f.NewWorkspace(t.Context(), "ws-e2e-single")
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	exe, err := f.NewExecutor(t.Context(), "exe-alpha", "Alpha laptop", "/workspace")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	if err := f.BindExecutor(t.Context(), ws.ID, exe.ID, true); err != nil {
		t.Fatalf("BindExecutor: %v", err)
	}
	if !strings.HasPrefix(ws.UserJWT, "ey") {
		t.Errorf("UserJWT looks invalid: %q", ws.UserJWT)
	}
	if exe.RegistrationToken == "" {
		t.Error("RegistrationToken empty")
	}
}
```

- [ ] **Step 2: Implement `internal/codexe2e/fixtures/loader.go`**

```go
// Package fixtures provisions users, workspaces, executors, and
// workspace-executor bindings against a running codex-e2e stack via the
// gateways' admin endpoints (or, where admin endpoints don't exist in
// phase 1, by direct SQL through the postgres port mapped on 55432).
package fixtures

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/agentserver/agentserver/internal/codexe2e/harness"
)

type Fixtures struct {
	stack *harness.Stack
	hc    *http.Client
}

func New(s *harness.Stack) *Fixtures {
	return &Fixtures{stack: s, hc: &http.Client{Timeout: 10 * time.Second}}
}

type Workspace struct {
	ID      string
	UserID  string
	UserJWT string
}

type Executor struct {
	ID                string
	RegistrationToken string
}

// NewWorkspace creates a user_id + workspace_id pair and mints a user JWT
// signed with the RS256 private key provisioned by Task 1.5
// (`/tmp/test-jwt.key`). The matching public key is exported into the
// docker-compose stack as CXG_AUTH_JWT_PUBLIC_KEY.
func (f *Fixtures) NewWorkspace(ctx context.Context, id string) (*Workspace, error) {
	userID := "u-" + id
	jwt, err := mintRS256JWT(f.stack.UserJWTPrivateKeyPath, map[string]any{
		"sub": userID,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		return nil, err
	}
	return &Workspace{ID: id, UserID: userID, UserJWT: jwt}, nil
}

// NewExecutor calls codex-exec-gateway's POST /api/codex-exec/register
// with the user JWT to mint a fresh exe_id + registration token. The
// gateway always generates the exe_id server-side (spec § Executor
// registration); callers do not propose one. The `id` argument is kept
// only as a human-readable label that participates in description
// formatting.
func (f *Fixtures) NewExecutor(ctx context.Context, id, displayName, defaultCwd string) (*Executor, error) {
	_ = id // retained for description formatting; not sent over the wire
	body, _ := json.Marshal(map[string]any{
		"display_name": displayName,
		"description":  displayName + " — " + defaultCwd,
		"default_cwd":  defaultCwd,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		f.stack.ExecGatewayHTTP+"/api/codex-exec/register",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.stack.InternalBearer)
	resp, err := f.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		return nil, fmt.Errorf("register: status %d", resp.StatusCode)
	}
	var out struct {
		ExeID             string `json:"exe_id"`
		RegistrationToken string `json:"registration_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &Executor{ID: out.ExeID, RegistrationToken: out.RegistrationToken}, nil
}

// BindExecutor calls POST /api/codex-exec/workspaces/{wid}/executors.
func (f *Fixtures) BindExecutor(ctx context.Context, workspaceID, exeID string, isDefault bool) error {
	body, _ := json.Marshal(map[string]any{
		"exe_id":     exeID,
		"is_default": isDefault,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		f.stack.ExecGatewayHTTP+"/api/codex-exec/workspaces/"+workspaceID+"/executors",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.stack.InternalBearer)
	resp, err := f.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("bind: status %d", resp.StatusCode)
	}
	return nil
}

// mintRS256JWT writes a minimal JWT-compatible token signed with the
// RSA-2048 private key at privateKeyPath (PEM). The matching public key
// must be exported as CXG_AUTH_JWT_PUBLIC_KEY in the docker-compose env
// so the gateway can verify the signature. See Task 1.5 for the keypair
// provisioning shell snippet.
func mintRS256JWT(privateKeyPath string, payload map[string]any) (string, error) {
	keyPEM, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return "", fmt.Errorf("read jwt private key %s: %w", privateKeyPath, err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return "", fmt.Errorf("decode pem at %s", privateKeyPath)
	}
	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Fallback: PKCS#8 (openssl genpkey output).
		anyPriv, perr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if perr != nil {
			return "", fmt.Errorf("parse rsa key %s: %w / %w", privateKeyPath, err, perr)
		}
		var ok bool
		priv, ok = anyPriv.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("key at %s is not RSA", privateKeyPath)
		}
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(payload)
	headB64 := base64.RawURLEncoding.EncodeToString(hb)
	payB64 := base64.RawURLEncoding.EncodeToString(pb)
	signing := headB64 + "." + payB64
	hashed := sha256.Sum256([]byte(signing))
	sigBytes, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("rsa sign: %w", err)
	}
	sig := base64.RawURLEncoding.EncodeToString(sigBytes)
	return signing + "." + sig, nil
}
```

- [ ] **Step 3: Run the test**

```bash
cd /root/agentserver
go test -tags=e2e_codex ./internal/codexe2e/fixtures/... -v -count=1 -timeout=5m
```
Expected:
```
=== RUN   TestProvision_SingleExecutor
--- PASS: TestProvision_SingleExecutor (XX.XXs)
PASS
```

- [ ] **Step 4: Commit**

```bash
git add internal/codexe2e/fixtures/
git commit -m "test(codex-e2e): fixtures loader (user JWT, executor register, workspace bind)"
```

---

## Task 5: Acceptance scenario "scripted_turn" (covers Acceptance bullets 1, 2, 3, 4)

**Goal:** Single executor + single shell call. Drives the fake-app to:
1. Connect to codex-app-gateway with a user JWT (Acceptance 3 — auth +
   thread list).
2. Call `thread/start`, `turn/start` with a prompt that should produce
   one shell call.
3. Watch fake-exec to confirm it received exactly one `process/start`
   (Acceptance 4 — fan-out via bridge).
4. Confirm fake-app received `turn/completed` with an
   `agent_message` item containing the scripted output.

This implicitly demonstrates Acceptance 1 (executor connected — the
scenario won't progress otherwise) and 2 (binding — without it the
manifest is empty).

**Files:**
- Create: `internal/codexe2e/scenarios/scripted_turn.go`
- Create: `internal/codexe2e/e2e_test.go`
- Create: `internal/codexe2e/runfake.go` (helper to launch fake-exec containers)
- Create: `mock-llm/main.go` + `Dockerfile.mock-llm`

- [ ] **Step 1: Build the mock LLM (returns a fixed shell tool call)**

The real codex CLI talks to an OpenAI-compatible endpoint. For
deterministic e2e we need the LLM to emit exactly one `shell`-tool call
with command `bash -lc "echo hello"`, then on second turn return a final
text message.

`mock-llm/main.go`:
```go
// mock-llm serves a minimal OpenAI Responses-compatible endpoint that
// responds with deterministic, scenario-driven tool calls. Selection is
// keyed off the X-Mock-Scenario header passed by the codex spawn (codex
// forwards arbitrary headers through CODEX_HEADERS env in our gateway
// config; see the codex-app-gateway runner for the wiring).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
)

var counter atomic.Int64

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/v1/responses", handleResponses)
	addr := ":9999"
	if v := os.Getenv("PORT"); v != "" {
		addr = ":" + v
	}
	fmt.Println("mock-llm listening on", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		panic(err)
	}
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	scenario := r.Header.Get("X-Mock-Scenario")
	turn := counter.Add(1)
	w.Header().Set("Content-Type", "application/json")
	switch scenario {
	case "multi_executor":
		writeMultiExecutor(w, turn)
	case "reconnect_replay":
		writeReconnectReplay(w, turn)
	default:
		writeScriptedTurn(w, turn)
	}
}

// writeScriptedTurn:
//  turn 1 → single shell call: bash -lc "echo hello"
//  turn 2 → final agent_message: "shell said: hello"
func writeScriptedTurn(w http.ResponseWriter, turn int64) {
	if turn%2 == 1 {
		json.NewEncoder(w).Encode(map[string]any{
			"id":     fmt.Sprintf("resp_%d", turn),
			"object": "response",
			"output": []any{
				map[string]any{
					"type": "function_call",
					"name": "shell",
					"arguments": `{"command":["bash","-lc","echo hello"]}`,
					"call_id": fmt.Sprintf("call_%d", turn),
				},
			},
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"id":     fmt.Sprintf("resp_%d", turn),
		"object": "response",
		"output": []any{
			map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": "shell said: hello"},
				},
			},
		},
	})
}

func writeMultiExecutor(w http.ResponseWriter, turn int64) {
	// turn 1 → shell on exe_alpha; turn 2 → shell on exe_beta;
	// turn 3 → final agent_message.
	switch turn % 3 {
	case 1:
		json.NewEncoder(w).Encode(map[string]any{
			"id": fmt.Sprintf("resp_%d", turn), "object": "response",
			"output": []any{map[string]any{
				"type": "function_call", "name": "shell",
				"arguments": `{"command":["bash","-lc","echo ALPHA_SCENARIO"],"environment_id":"exe-alpha"}`,
				"call_id":   fmt.Sprintf("call_%d", turn),
			}},
		})
	case 2:
		json.NewEncoder(w).Encode(map[string]any{
			"id": fmt.Sprintf("resp_%d", turn), "object": "response",
			"output": []any{map[string]any{
				"type": "function_call", "name": "shell",
				"arguments": `{"command":["bash","-lc","echo BETA_SCENARIO"],"environment_id":"exe-beta"}`,
				"call_id":   fmt.Sprintf("call_%d", turn),
			}},
		})
	default:
		json.NewEncoder(w).Encode(map[string]any{
			"id": fmt.Sprintf("resp_%d", turn), "object": "response",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "alpha=alpha; beta=beta"}},
			}},
		})
	}
}

func writeReconnectReplay(w http.ResponseWriter, turn int64) {
	// Same script as scripted_turn but with a 3-second sleep between the
	// shell call and final message, so the test can disconnect mid-turn.
	if turn%2 == 1 {
		json.NewEncoder(w).Encode(map[string]any{
			"id": fmt.Sprintf("resp_%d", turn), "object": "response",
			"output": []any{map[string]any{
				"type": "function_call", "name": "shell",
				"arguments": `{"command":["bash","-lc","sleep 3 && echo hello"]}`,
				"call_id":   fmt.Sprintf("call_%d", turn),
			}},
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"id": fmt.Sprintf("resp_%d", turn), "object": "response",
		"output": []any{map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "shell said: hello"}},
		}},
	})
}
```

`Dockerfile.mock-llm`:
```dockerfile
FROM golang:1.26-trixie AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o mock-llm ./mock-llm

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/mock-llm /usr/local/bin/mock-llm
EXPOSE 9999
ENTRYPOINT ["mock-llm"]
```

Build it and load into the compose stack's image namespace:
```bash
cd /root/agentserver
docker build -f Dockerfile.mock-llm -t ghcr.io/agentserver/codex-mock-llm:e2e .
```

- [ ] **Step 2: Implement `internal/codexe2e/runfake.go`**

```go
package codexe2e

import (
	"os/exec"
	"testing"
)

// StartFakeExec launches a codex-fake-exec container connected to the
// stack's exec-gateway. Returns immediately; t.Cleanup stops/removes the
// container.
func StartFakeExec(t *testing.T, name, exeID, token string) {
	t.Helper()
	stop := exec.Command("docker", "rm", "-f", name)
	_ = stop.Run()
	cmd := exec.Command("docker", "run", "-d",
		"--name", name,
		"--network", "agentserver_default", // adjust if compose project rename
		"codex-fake-exec:e2e",
		"--gateway", "ws://codex-exec-gateway:6060",
		"--exe-id", exeID,
		"--token", token,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run fake-exec: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		dump := exec.Command("docker", "logs", name)
		logs, _ := dump.CombinedOutput()
		t.Logf("---- fake-exec %s logs ----\n%s", name, logs)
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
}
```

**Note:** The compose project name (here `agentserver`) determines the
default network. Verify with `docker network ls` after `compose up` and
update if different (e.g. compose may use the directory name).

- [ ] **Step 3: Implement `internal/codexe2e/scenarios/scripted_turn.go`**

```go
// Package scenarios drives end-to-end acceptance scenarios against a
// running codex-e2e stack. Each scenario function returns nil on success
// and a descriptive error on failure; tests invoke them under
// build-tag=e2e_codex.
package scenarios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	fakeapp "github.com/agentserver/agentserver/cmd/codex-fake-app"
	"github.com/agentserver/agentserver/internal/codexe2e/fixtures"
	"github.com/agentserver/agentserver/internal/codexe2e/harness"
)

// ScriptedTurn covers Acceptance bullets 1-4: a connected executor, bound
// to a workspace, gets one shell call routed through the bridge during a
// real codex turn, and the user sees the result.
func ScriptedTurn(ctx context.Context, stack *harness.Stack, ws *fixtures.Workspace, exe *fixtures.Executor) error {
	url := stack.AppGatewayWS + "/codex-app/?workspace_id=" + ws.ID
	c, err := fakeapp.Dial(ctx, url, ws.UserJWT)
	if err != nil {
		return fmt.Errorf("dial app: %w", err)
	}
	defer c.Close()

	if _, err := c.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "fake-app", "version": "0.1"},
	}); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := c.Notify(ctx, "initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized: %w", err)
	}
	threadResp, err := c.Call(ctx, "thread/start", map[string]any{
		"workspaceId": ws.ID,
	})
	if err != nil {
		return fmt.Errorf("thread/start: %w", err)
	}
	var threadOut struct {
		Thread struct{ ID string `json:"id"` } `json:"thread"`
	}
	_ = json.Unmarshal(threadResp, &threadOut)
	if threadOut.Thread.ID == "" {
		return errors.New("thread/start returned empty id")
	}

	if _, err := c.Call(ctx, "turn/start", map[string]any{
		"threadId": threadOut.Thread.ID,
		"input":    []map[string]string{{"type": "text", "text": "echo hello via shell"}},
	}); err != nil {
		return fmt.Errorf("turn/start: %w", err)
	}

	pumpCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := c.PumpUntil(pumpCtx, func(e fakeapp.EventEntry) bool {
		return e.Method == "turn/completed"
	}); err != nil {
		return fmt.Errorf("pump until turn/completed: %w", err)
	}

	// Assert: at least one item/completed had an agent_message containing
	// "shell said: hello".
	var sawMsg bool
	for _, e := range c.Log.Filter("item/completed") {
		var p struct {
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal(e.Params, &p); err != nil {
			continue
		}
		if p.Item.Type == "agentMessage" && strings.Contains(p.Item.Text, "shell said: hello") {
			sawMsg = true
		}
	}
	if !sawMsg {
		return fmt.Errorf("expected agentMessage containing 'shell said: hello', got log: %+v", c.Log.Snapshot())
	}
	return nil
}
```

- [ ] **Step 4: Wire the scenario into `internal/codexe2e/e2e_test.go`**

```go
//go:build e2e_codex

package codexe2e

import (
	"context"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexe2e/fixtures"
	"github.com/agentserver/agentserver/internal/codexe2e/harness"
	"github.com/agentserver/agentserver/internal/codexe2e/scenarios"
)

func TestScriptedTurn(t *testing.T) {
	stack := harness.Up(t)
	f := fixtures.New(stack)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ws, err := f.NewWorkspace(ctx, "ws-scripted")
	if err != nil { t.Fatal(err) }
	exe, err := f.NewExecutor(ctx, "exe-alpha", "Alpha", "/workspace")
	if err != nil { t.Fatal(err) }
	if err := f.BindExecutor(ctx, ws.ID, exe.ID, true); err != nil { t.Fatal(err) }

	StartFakeExec(t, "fake-exec-alpha", exe.ID, exe.RegistrationToken)
	time.Sleep(2 * time.Second) // give it time to connect

	if err := scenarios.ScriptedTurn(ctx, stack, ws, exe); err != nil {
		t.Fatalf("ScriptedTurn: %v", err)
	}
}
```

- [ ] **Step 5: Run the scenario**

```bash
cd /root/agentserver
docker build -f Dockerfile.codex-fake-exec -t codex-fake-exec:e2e .
docker build -f Dockerfile.mock-llm -t ghcr.io/agentserver/codex-mock-llm:e2e .
go test -tags=e2e_codex ./internal/codexe2e/ -run TestScriptedTurn -v -count=1 -timeout=10m
```
Expected:
```
=== RUN   TestScriptedTurn
--- PASS: TestScriptedTurn (XX.XXs)
PASS
```

- [ ] **Step 6: Commit**

```bash
git add mock-llm/ Dockerfile.mock-llm internal/codexe2e/scenarios/scripted_turn.go internal/codexe2e/e2e_test.go internal/codexe2e/runfake.go
git commit -m "test(codex-e2e): scripted_turn scenario (Acceptance bullets 1-4)"
```

---

## Task 6: Acceptance scenario "multi_executor" (covers Acceptance bullet 5)

**Goal:** Two executors bound to the workspace; mock-llm script issues
two `shell` tool calls, one with `environment_id=exe-alpha` and one with
`environment_id=exe-beta`; assert both fake-execs received exactly one
matching call.

**Files:**
- Create: `internal/codexe2e/scenarios/multi_executor.go`
- Modify: `internal/codexe2e/e2e_test.go` (add `TestMultiExecutor`)
- Create: `internal/codexe2e/inspect.go` (helper to read fake-exec calls.jsonl out of a container)

- [ ] **Step 1: Add `inspect.go`**

```go
package codexe2e

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// InspectFakeExecCalls cats /tmp/codex-fake-exec/calls.jsonl out of a
// running fake-exec container and returns one decoded entry per line.
func InspectFakeExecCalls(t *testing.T, containerName string) []map[string]any {
	t.Helper()
	out, err := exec.Command("docker", "exec", containerName,
		"cat", "/tmp/codex-fake-exec/calls.jsonl").CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec cat: %v\n%s", err, out)
	}
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			entries = append(entries, m)
		}
	}
	return entries
}

// CountStartCallsContaining returns how many process/start entries
// contained `needle` anywhere in their JSON params.
func CountStartCallsContaining(entries []map[string]any, needle string) int {
	n := 0
	for _, e := range entries {
		if e["method"] != "process/start" {
			continue
		}
		raw, _ := json.Marshal(e["params"])
		if strings.Contains(string(raw), needle) {
			n++
		}
	}
	return n
}
```

- [ ] **Step 2: Implement `internal/codexe2e/scenarios/multi_executor.go`**

```go
package scenarios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	fakeapp "github.com/agentserver/agentserver/cmd/codex-fake-app"
	"github.com/agentserver/agentserver/internal/codexe2e/fixtures"
	"github.com/agentserver/agentserver/internal/codexe2e/harness"
)

// MultiExecutor covers Acceptance bullet 5: <environments> block lists
// both bound executors, and the LLM picks one per `shell` call via
// environment_id. Mock-llm is configured (via X-Mock-Scenario header)
// to emit one call per environment.
func MultiExecutor(ctx context.Context, stack *harness.Stack, ws *fixtures.Workspace) error {
	url := stack.AppGatewayWS + "/codex-app/?workspace_id=" + ws.ID
	c, err := fakeapp.Dial(ctx, url, ws.UserJWT)
	if err != nil {
		return err
	}
	defer c.Close()

	if _, err := c.Call(ctx, "initialize", map[string]any{}); err != nil {
		return err
	}
	if err := c.Notify(ctx, "initialized", map[string]any{}); err != nil {
		return err
	}
	thr, err := c.Call(ctx, "thread/start", map[string]any{"workspaceId": ws.ID})
	if err != nil {
		return err
	}
	var t struct{ Thread struct{ ID string `json:"id"` } `json:"thread"` }
	_ = json.Unmarshal(thr, &t)
	if _, err := c.Call(ctx, "turn/start", map[string]any{
		"threadId": t.Thread.ID,
		"input":    []map[string]string{{"type": "text", "text": "echo on alpha and beta"}},
		"metadata": map[string]string{"X-Mock-Scenario": "multi_executor"},
	}); err != nil {
		return err
	}
	pumpCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := c.PumpUntil(pumpCtx, func(e fakeapp.EventEntry) bool {
		return e.Method == "turn/completed"
	}); err != nil {
		return err
	}
	if !errors.Is(pumpCtx.Err(), nil) && pumpCtx.Err() != nil {
		return fmt.Errorf("turn timed out: %v", pumpCtx.Err())
	}
	return nil
}
```

- [ ] **Step 3: Add `TestMultiExecutor` to `e2e_test.go`**

```go
func TestMultiExecutor(t *testing.T) {
	stack := harness.Up(t)
	f := fixtures.New(stack)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	ws, err := f.NewWorkspace(ctx, "ws-multi")
	if err != nil { t.Fatal(err) }
	alpha, err := f.NewExecutor(ctx, "exe-alpha", "Alpha", "/workspace")
	if err != nil { t.Fatal(err) }
	beta, err := f.NewExecutor(ctx, "exe-beta", "Beta", "/workspace")
	if err != nil { t.Fatal(err) }
	if err := f.BindExecutor(ctx, ws.ID, alpha.ID, true); err != nil { t.Fatal(err) }
	if err := f.BindExecutor(ctx, ws.ID, beta.ID, false); err != nil { t.Fatal(err) }

	StartFakeExec(t, "fake-exec-alpha", alpha.ID, alpha.RegistrationToken)
	StartFakeExec(t, "fake-exec-beta", beta.ID, beta.RegistrationToken)
	time.Sleep(2 * time.Second)

	if err := scenarios.MultiExecutor(ctx, stack, ws); err != nil {
		t.Fatalf("MultiExecutor: %v", err)
	}
	alphaCalls := InspectFakeExecCalls(t, "fake-exec-alpha")
	betaCalls := InspectFakeExecCalls(t, "fake-exec-beta")
	if got := CountStartCallsContaining(alphaCalls, "ALPHA_SCENARIO"); got != 1 {
		t.Errorf("alpha process/start with ALPHA_SCENARIO: got %d, want 1", got)
	}
	if got := CountStartCallsContaining(betaCalls, "BETA_SCENARIO"); got != 1 {
		t.Errorf("beta process/start with BETA_SCENARIO: got %d, want 1", got)
	}
	// Anti-cross-talk: alpha must NOT have received the beta command.
	if got := CountStartCallsContaining(alphaCalls, "BETA_SCENARIO"); got != 0 {
		t.Errorf("alpha received BETA_SCENARIO: got %d, want 0", got)
	}
	if got := CountStartCallsContaining(betaCalls, "ALPHA_SCENARIO"); got != 0 {
		t.Errorf("beta received ALPHA_SCENARIO: got %d, want 0", got)
	}
}
```

- [ ] **Step 4: Run the scenario**

```bash
cd /root/agentserver
go test -tags=e2e_codex ./internal/codexe2e/ -run TestMultiExecutor -v -count=1 -timeout=10m
```
Expected:
```
=== RUN   TestMultiExecutor
--- PASS: TestMultiExecutor (XX.XXs)
PASS
```

- [ ] **Step 5: Commit**

```bash
git add internal/codexe2e/inspect.go internal/codexe2e/scenarios/multi_executor.go internal/codexe2e/e2e_test.go
git commit -m "test(codex-e2e): multi_executor scenario (Acceptance bullet 5)"
```

---

## Task 7: Acceptance scenario "reconnect_replay" (covers Acceptance bullet 6)

**Goal:** Open a fake-app, start a turn that takes ~3s (mock-llm sleeps),
disconnect mid-turn, reconnect, call `thread/read`, assert the persisted
event log contains the events emitted while disconnected, AND that new
events resume streaming on the new conn until `turn/completed`.

**Files:**
- Create: `internal/codexe2e/scenarios/reconnect_replay.go`
- Modify: `internal/codexe2e/e2e_test.go` (add `TestReconnectReplay`)

- [ ] **Step 1: Implement `internal/codexe2e/scenarios/reconnect_replay.go`**

```go
package scenarios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	fakeapp "github.com/agentserver/agentserver/cmd/codex-fake-app"
	"github.com/agentserver/agentserver/internal/codexe2e/fixtures"
	"github.com/agentserver/agentserver/internal/codexe2e/harness"
)

// ReconnectReplay covers Acceptance bullet 6: TUI disconnects mid-turn,
// reconnects, calls thread/read, and sees both the events that fired
// while disconnected and any newly-emitted events.
func ReconnectReplay(ctx context.Context, stack *harness.Stack, ws *fixtures.Workspace) error {
	url := stack.AppGatewayWS + "/codex-app/?workspace_id=" + ws.ID

	// Connection 1: start a turn, then close after seeing turn/started.
	c1, err := fakeapp.Dial(ctx, url, ws.UserJWT)
	if err != nil {
		return err
	}
	if _, err := c1.Call(ctx, "initialize", map[string]any{}); err != nil {
		return err
	}
	if err := c1.Notify(ctx, "initialized", map[string]any{}); err != nil {
		return err
	}
	thrResp, err := c1.Call(ctx, "thread/start", map[string]any{"workspaceId": ws.ID})
	if err != nil {
		return err
	}
	var thr struct{ Thread struct{ ID string `json:"id"` } `json:"thread"` }
	_ = json.Unmarshal(thrResp, &thr)
	if _, err := c1.Call(ctx, "turn/start", map[string]any{
		"threadId": thr.Thread.ID,
		"input":    []map[string]string{{"type": "text", "text": "echo with sleep"}},
		"metadata": map[string]string{"X-Mock-Scenario": "reconnect_replay"},
	}); err != nil {
		return err
	}

	pumpCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := c1.PumpUntil(pumpCtx, func(e fakeapp.EventEntry) bool {
		return e.Method == "turn/started"
	}); err != nil {
		cancel()
		return fmt.Errorf("c1 wait turn/started: %w", err)
	}
	cancel()
	c1.Close() // disconnect mid-turn

	// Connection 2: reconnect, call thread/read, assert replay.
	time.Sleep(4 * time.Second) // let the turn complete server-side
	c2, err := fakeapp.Dial(ctx, url, ws.UserJWT)
	if err != nil {
		return fmt.Errorf("dial c2: %w", err)
	}
	defer c2.Close()
	if _, err := c2.Call(ctx, "initialize", map[string]any{}); err != nil {
		return err
	}
	if err := c2.Notify(ctx, "initialized", map[string]any{}); err != nil {
		return err
	}
	readResp, err := c2.Call(ctx, "thread/read", map[string]any{"threadId": thr.Thread.ID})
	if err != nil {
		return fmt.Errorf("thread/read: %w", err)
	}
	var read struct {
		Events []struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		} `json:"events"`
	}
	if err := json.Unmarshal(readResp, &read); err != nil {
		return fmt.Errorf("decode read: %w", err)
	}
	var sawTurnStarted, sawTurnCompleted bool
	for _, e := range read.Events {
		if e.Method == "turn/started" {
			sawTurnStarted = true
		}
		if e.Method == "turn/completed" {
			sawTurnCompleted = true
		}
	}
	if !sawTurnStarted {
		return errors.New("thread/read missing turn/started")
	}
	if !sawTurnCompleted {
		return errors.New("thread/read missing turn/completed")
	}
	return nil
}
```

- [ ] **Step 2: Add `TestReconnectReplay` to `e2e_test.go`**

```go
func TestReconnectReplay(t *testing.T) {
	stack := harness.Up(t)
	f := fixtures.New(stack)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	ws, err := f.NewWorkspace(ctx, "ws-reconnect")
	if err != nil { t.Fatal(err) }
	exe, err := f.NewExecutor(ctx, "exe-alpha-rc", "Alpha", "/workspace")
	if err != nil { t.Fatal(err) }
	if err := f.BindExecutor(ctx, ws.ID, exe.ID, true); err != nil { t.Fatal(err) }
	StartFakeExec(t, "fake-exec-alpha-rc", exe.ID, exe.RegistrationToken)
	time.Sleep(2 * time.Second)

	if err := scenarios.ReconnectReplay(ctx, stack, ws); err != nil {
		t.Fatalf("ReconnectReplay: %v", err)
	}
}
```

- [ ] **Step 3: Run the scenario**

```bash
cd /root/agentserver
go test -tags=e2e_codex ./internal/codexe2e/ -run TestReconnectReplay -v -count=1 -timeout=10m
```
Expected:
```
=== RUN   TestReconnectReplay
--- PASS: TestReconnectReplay (XX.XXs)
PASS
```

- [ ] **Step 4: Commit**

```bash
git add internal/codexe2e/scenarios/reconnect_replay.go internal/codexe2e/e2e_test.go
git commit -m "test(codex-e2e): reconnect_replay scenario (Acceptance bullet 6)"
```

---

## Task 8: `make test-e2e-codex` target

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Append the target**

Append to `/root/agentserver/Makefile`:
```makefile
.PHONY: test-e2e-codex test-e2e-codex-build test-e2e-codex-down

# Build all images required by docker-compose-codex.yml. Idempotent;
# Docker layer cache makes repeat invocations cheap.
test-e2e-codex-build:
	docker build -f Dockerfile.codex-app-gateway -t codex-app-gateway:e2e .
	docker build -f Dockerfile.codex-exec-gateway -t codex-exec-gateway:e2e .
	docker build -f Dockerfile.codex-fake-exec    -t codex-fake-exec:e2e .
	docker build -f Dockerfile.codex-fake-app     -t codex-fake-app:e2e .
	docker build -f Dockerfile.mock-llm           -t ghcr.io/agentserver/codex-mock-llm:e2e .

# Run the full codex e2e acceptance suite. Requires `docker compose` v2,
# Go 1.26+, and the codex CLI binary baked into Dockerfile.codex-app-gateway
# (Plan 2 owns that). Brings the stack up per-test (harness.Up) and tears
# it down on completion.
test-e2e-codex: test-e2e-codex-build
	go test -tags=e2e_codex ./internal/codexe2e/... -v -count=1 -timeout=20m

# Manual cleanup if a test crashed without running its t.Cleanup.
test-e2e-codex-down:
	docker compose -f docker-compose-codex.yml down -v --remove-orphans
```

Also add the new targets to the top-level `.PHONY` line for
discoverability (the existing one has `test docker docker-agent ...` —
extend it).

- [ ] **Step 2: Verify the target builds**

```bash
cd /root/agentserver
make test-e2e-codex-build
```
Expected: 5 images built, no errors.

- [ ] **Step 3: Verify the full target runs end-to-end**

```bash
cd /root/agentserver
make test-e2e-codex
```
Expected:
```
=== RUN   TestStackComesUp
--- PASS: TestStackComesUp ...
=== RUN   TestProvision_SingleExecutor
--- PASS: TestProvision_SingleExecutor ...
=== RUN   TestScriptedTurn
--- PASS: TestScriptedTurn ...
=== RUN   TestMultiExecutor
--- PASS: TestMultiExecutor ...
=== RUN   TestReconnectReplay
--- PASS: TestReconnectReplay ...
PASS
ok  	github.com/agentserver/agentserver/internal/codexe2e ...
```

- [ ] **Step 4: Commit**

```bash
git add Makefile
git commit -m "build(codex-e2e): make test-e2e-codex target"
```

---

## Task 9: CI workflow `.github/workflows/codex-e2e.yml`

**Goal:** Run the e2e suite on PRs that touch any of:
- `cmd/codex-app-gateway/**`, `cmd/codex-exec-gateway/**`
- `cmd/codex-fake-app/**`, `cmd/codex-fake-exec/**`
- `internal/codexappgateway/**`, `internal/codexexecgateway/**`
- `internal/codexe2e/**`
- `Dockerfile.codex-*`, `docker-compose-codex.yml`
- the codex submodule (if vendored under `codex/` per Plan 1)
- this workflow file itself

**Files:**
- Create: `.github/workflows/codex-e2e.yml`

- [ ] **Step 1: Verify CI lives where I think**

```bash
ls /root/agentserver/.github/workflows/
```
Expected: `build.yml` (current). New file goes alongside.

- [ ] **Step 2: Write `.github/workflows/codex-e2e.yml`**

```yaml
name: Codex E2E

on:
  pull_request:
    paths:
      - "cmd/codex-app-gateway/**"
      - "cmd/codex-exec-gateway/**"
      - "cmd/codex-fake-app/**"
      - "cmd/codex-fake-exec/**"
      - "internal/codexappgateway/**"
      - "internal/codexexecgateway/**"
      - "internal/codexe2e/**"
      - "internal/storage/agentworkspace/**"
      - "mock-llm/**"
      - "Dockerfile.codex-app-gateway"
      - "Dockerfile.codex-exec-gateway"
      - "Dockerfile.codex-fake-app"
      - "Dockerfile.codex-fake-exec"
      - "Dockerfile.mock-llm"
      - "docker-compose-codex.yml"
      - ".github/workflows/codex-e2e.yml"
  push:
    branches: [main]
    paths:
      - "cmd/codex-app-gateway/**"
      - "cmd/codex-exec-gateway/**"
      - "internal/codexappgateway/**"
      - "internal/codexexecgateway/**"
      - "internal/codexe2e/**"
  workflow_dispatch: {}

jobs:
  e2e:
    runs-on: ubuntu-latest
    timeout-minutes: 40
    steps:
      - uses: actions/checkout@v6
        with:
          submodules: true

      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod

      - name: Verify docker compose v2
        run: docker compose version

      - name: Build all e2e images
        run: make test-e2e-codex-build

      - name: Run e2e suite
        run: make test-e2e-codex
        env:
          # The mock-llm replaces real OpenAI traffic; no real key needed.
          DOCKER_BUILDKIT: "1"
          COMPOSE_DOCKER_CLI_BUILD: "1"

      - name: Dump compose logs on failure
        if: failure()
        run: |
          docker compose -f docker-compose-codex.yml logs --no-color > compose-logs.txt || true

      - name: Upload compose logs
        if: failure()
        uses: actions/upload-artifact@v4
        with:
          name: codex-e2e-compose-logs
          path: compose-logs.txt
```

- [ ] **Step 3: Validate YAML syntax locally**

```bash
cd /root/agentserver
python3 -c 'import yaml,sys; yaml.safe_load(open(".github/workflows/codex-e2e.yml"))'
```
Expected: no output, exit 0.

- [ ] **Step 4: Commit and push, observe the workflow runs on the PR**

```bash
git add .github/workflows/codex-e2e.yml
git commit -m "ci(codex-e2e): run docker-compose acceptance harness on PRs touching codex stack"
```

When the PR is opened, verify in GitHub Actions that the workflow
triggers and goes green. (Initial run may need iteration on resource
limits; ubuntu-latest has 4 vCPU / 16 GB RAM which is sufficient for the
stack.)

---

## Self-Review Checklist (run after all 9 tasks)

- [ ] **Spec coverage — every Acceptance bullet has an explicit assertion:**
  - Bullet 1 (executor connected) — Task 5 (`StartFakeExec` + scenario will fail before bridge if connection isn't live; also covered implicitly by `/api/exec-gateway/connected` returning the executor in manifest construction)
  - Bullet 2 (binding) — Task 4 (`BindExecutor`), required by Tasks 5/6/7
  - Bullet 3 (TUI auth + thread list) — Task 5 (initialize + thread/start round-trip)
  - Bullet 4 (single-env shell call routed via bridge) — Task 5 (assert agent_message containing scripted output AND fake-exec received process/start)
  - Bullet 5 (multi-env, env_id steering) — Task 6 (per-executor call counts + cross-talk anti-assertion)
  - Bullet 6 (reconnect mid-turn replay) — Task 7 (thread/read returns persisted events)

- [ ] **No placeholders:** every step contains complete, copy-pasteable
  Go / YAML / shell; no "implement X" without showing X.

- [ ] **Real codex, fake everything else:** `Dockerfile.codex-app-gateway`
  (owned by Plan 2) installs the real codex CLI; the fakes are the
  fake-app TUI client, fake-exec executor, and the mock-llm. P1–P4 patches
  in the codex fork are exercised by every scenario.

- [ ] **No approvals:** scenarios never expect a `ServerRequest`. Mock-llm
  scripts only emit `function_call` (auto-approved by codex's
  `ApprovalPolicy=never`) and `message` outputs.

- [ ] **Cleanup:** every test brings the stack up via `harness.Up` which
  registers `t.Cleanup(compose down -v)`; fake-exec containers also
  `t.Cleanup(docker rm -f)`. Re-running the suite is idempotent.

- [ ] **CI gate:** `.github/workflows/codex-e2e.yml` runs the suite on
  any PR touching the codex stack. Logs are uploaded on failure.

---

## Open items / handoff notes

- The mock-llm payload format may need tightening if the Plan 2 runner
  emits different OpenAI-compatible body shapes (Responses API vs
  Chat Completions). Plan 2 is source of truth — adjust mock-llm
  payloads in Task 5 / Task 6 to match.
- The compose project name (`agentserver_default` network) in
  `internal/codexe2e/runfake.go` may differ depending on the directory
  name docker-compose uses. Confirm with `docker network ls` once the
  first stack is up and adjust the `--network` flag (or pass an explicit
  `-p` project name when invoking `docker compose`).
- Approval-flow scenarios (phase 2) are out of scope and not
  enumerated here; this plan demonstrates only the phase-1 17-RPC
  surface.
- If Plan 2's app-gateway uses a different turn-metadata mechanism than
  passing arbitrary headers via `metadata`, the `X-Mock-Scenario`
  trick in Tasks 6/7 needs to switch (e.g., per-workspace scenario
  config in mock-llm). Verify when integrating.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-05-codex-gateway-e2e-tests.md`.**
