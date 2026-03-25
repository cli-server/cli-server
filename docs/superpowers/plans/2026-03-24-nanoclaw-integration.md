# NanoClaw Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add NanoClaw as a third sandbox type in agentserver with K8s Pod management, llmproxy integration, and WeChat message bridging.

**Architecture:** NanoClaw runs as a K8s Pod (like openclaw), with agents running directly via Claude Agent SDK (no Docker inside Pod). WeChat messages are bridged through agentserver's iLink backend. Config is injected via `NANOCLAW_CONFIG_CONTENT` environment variable.

**Tech Stack:** Go (backend), TypeScript/React (frontend), PostgreSQL (DB), K8s (orchestration), Node.js (NanoClaw container)

**Spec:** `docs/superpowers/specs/2026-03-24-nanoclaw-integration-design.md`

---

## File Structure

| File | Responsibility |
|------|---------------|
| `internal/sandbox/config.go` | Add NanoClaw config fields + `BuildNanoclawConfig()` |
| `internal/sandbox/config_test.go` | Tests for `BuildNanoclawConfig()` |
| `internal/process/process.go` | Add `NanoclawBridgeSecret` to `StartOptions` |
| `internal/sbxstore/store.go` | Add `NanoclawBridgeSecret` to `Sandbox` struct |
| `internal/db/sandboxes.go` | Add `NanoclawBridgeSecret` to Sandbox struct + `UpdateSandboxNanoclawBridgeSecret()` |
| `internal/db/migrations/008_nanoclaw_bridge_secret.sql` | Add column to sandboxes table |
| `internal/sandbox/manager.go` | Add nanoclaw Pod spec, runtime class, health probe |
| `internal/server/server.go` | Type validation, creation logic, guard updates, bridge endpoints |
| `Dockerfile.nanoclaw` | Container image with weixin channel + process-runner |
| `nanoclaw-entrypoint.sh` | Config injection entrypoint script |
| `nanoclaw-weixin-channel/index.ts` | WeChat channel for NanoClaw (bridge mode) |
| `nanoclaw-patches/process-runner.ts` | No-container mode agent execution adapter |
| `web/src/components/CreateSandboxModal.tsx` | Add NanoClaw type option |
| `web/src/components/SandboxDetail.tsx` | Update openclaw-specific guards |
| `internal/db/migrations/009_nanoclaw_weixin_bridge.sql` | Bridge credentials + reverse lookup (Phase 3) |
| `internal/db/weixin_bindings.go` | `GetSandboxByBotID()`, `SaveBotCredentials()` (Phase 3) |
| `internal/weixin/ilink.go` | `SendMessage()`, `RegisterWebhook()` (Phase 3, blocked) |

---

## Phase 1: Sandbox Type + Pod Management

### Task 1: Add NanoClaw Config Fields and BuildNanoclawConfig

**Files:**
- Modify: `internal/sandbox/config.go`
- Create: `internal/sandbox/config_test.go`

- [ ] **Step 1: Write test for BuildNanoclawConfig**

Create `internal/sandbox/config_test.go`:

```go
package sandbox

import (
	"strings"
	"testing"
)

func TestBuildNanoclawConfig_Basic(t *testing.T) {
	result := BuildNanoclawConfig("https://proxy.example.com", "tok-123", "Andy", "", "", "", "")

	if !strings.Contains(result, "ANTHROPIC_BASE_URL=https://proxy.example.com") {
		t.Errorf("missing ANTHROPIC_BASE_URL, got: %s", result)
	}
	if !strings.Contains(result, "ANTHROPIC_API_KEY=tok-123") {
		t.Errorf("missing ANTHROPIC_API_KEY, got: %s", result)
	}
	if !strings.Contains(result, "ASSISTANT_NAME=Andy") {
		t.Errorf("missing ASSISTANT_NAME, got: %s", result)
	}
	if !strings.Contains(result, "NANOCLAW_NO_CONTAINER=true") {
		t.Errorf("missing NANOCLAW_NO_CONTAINER, got: %s", result)
	}
	// Should NOT contain weixin vars when not enabled
	if strings.Contains(result, "NANOCLAW_WEIXIN_BRIDGE_URL") {
		t.Errorf("should not contain NANOCLAW_WEIXIN_BRIDGE_URL when weixin disabled")
	}
}

func TestBuildNanoclawConfig_WithWeixin(t *testing.T) {
	result := BuildNanoclawConfig("https://proxy.example.com", "tok-123", "Andy",
		"https://bridge.example.com/weixin", "secret-abc", "", "")

	if !strings.Contains(result, "NANOCLAW_WEIXIN_BRIDGE_URL=https://bridge.example.com/weixin") {
		t.Errorf("missing NANOCLAW_WEIXIN_BRIDGE_URL, got: %s", result)
	}
	if !strings.Contains(result, "NANOCLAW_BRIDGE_SECRET=secret-abc") {
		t.Errorf("missing NANOCLAW_BRIDGE_SECRET, got: %s", result)
	}
}

func TestBuildNanoclawConfig_BYOK(t *testing.T) {
	result := BuildNanoclawConfig("https://proxy.example.com", "tok-123", "Andy",
		"", "", "https://custom.llm.com", "custom-key-456")

	if !strings.Contains(result, "ANTHROPIC_BASE_URL=https://custom.llm.com") {
		t.Errorf("BYOK should override base URL, got: %s", result)
	}
	if !strings.Contains(result, "ANTHROPIC_API_KEY=custom-key-456") {
		t.Errorf("BYOK should override API key, got: %s", result)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /root/agentserver && go test ./internal/sandbox/ -run TestBuildNanoclawConfig -v`
Expected: FAIL — `BuildNanoclawConfig` undefined

- [ ] **Step 3: Add NanoClaw config fields and BuildNanoclawConfig**

In `internal/sandbox/config.go`, add three fields to `Config` struct after `OpenclawWeixinEnabled`:

```go
NanoclawImage            string
NanoclawRuntimeClassName string
NanoclawWeixinEnabled    bool
```

Add to `DefaultConfig()`:

```go
NanoclawImage:            os.Getenv("NANOCLAW_IMAGE"),
NanoclawRuntimeClassName: os.Getenv("NANOCLAW_RUNTIME_CLASS"),
NanoclawWeixinEnabled:    os.Getenv("NANOCLAW_WEIXIN_ENABLED") == "true",
```

Add `BuildNanoclawConfig` function after `BuildOpenclawConfig`:

```go
// BuildNanoclawConfig returns the .env file content for a NanoClaw sandbox.
// The result is injected via NANOCLAW_CONFIG_CONTENT env var and written
// to /app/.env by the container entrypoint.
func BuildNanoclawConfig(proxyBaseURL, proxyToken, assistantName string, weixinBridgeURL, bridgeSecret string, byokBaseURL, byokAPIKey string) string {
	baseURL := proxyBaseURL
	apiKey := proxyToken
	if byokBaseURL != "" {
		baseURL = byokBaseURL
		apiKey = byokAPIKey
	}

	var lines []string
	lines = append(lines, "ANTHROPIC_BASE_URL="+baseURL)
	lines = append(lines, "ANTHROPIC_API_KEY="+apiKey)
	if assistantName == "" {
		assistantName = "Andy"
	}
	lines = append(lines, "ASSISTANT_NAME="+assistantName)
	lines = append(lines, "NANOCLAW_NO_CONTAINER=true")

	if weixinBridgeURL != "" {
		lines = append(lines, "NANOCLAW_WEIXIN_BRIDGE_URL="+weixinBridgeURL)
	}
	if bridgeSecret != "" {
		lines = append(lines, "NANOCLAW_BRIDGE_SECRET="+bridgeSecret)
	}

	return strings.Join(lines, "\n") + "\n"
}
```

Add `"strings"` to imports.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /root/agentserver && go test ./internal/sandbox/ -run TestBuildNanoclawConfig -v`
Expected: PASS (3 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/config.go internal/sandbox/config_test.go
git commit -m "feat(sandbox): add NanoClaw config fields and BuildNanoclawConfig"
```

---

### Task 2: Add NanoclawBridgeSecret to Process StartOptions

**Files:**
- Modify: `internal/process/process.go`

- [ ] **Step 1: Add NanoclawBridgeSecret field to StartOptions**

In `internal/process/process.go`, add after `CustomModels` field (line 36):

```go
NanoclawBridgeSecret string        // nanoclaw only: shared secret for bridge HTTP auth
```

Update the comment on `SandboxType` field (line 29):

```go
SandboxType      string        // "opencode", "openclaw", or "nanoclaw"
```

- [ ] **Step 2: Commit**

```bash
git add internal/process/process.go
git commit -m "feat(process): add NanoclawBridgeSecret to StartOptions"
```

---

### Task 3: DB Migration for nanoclaw_bridge_secret Column

**Files:**
- Create: `internal/db/migrations/008_nanoclaw_bridge_secret.sql`

Note: Existing migrations go up to `007_drop_username.sql`. This must be `008`.

- [ ] **Step 1: Create migration file**

```sql
-- Add nanoclaw bridge secret column to sandboxes table.
-- Stores the shared secret for HTTP auth between agentserver and NanoClaw pod.
ALTER TABLE sandboxes ADD COLUMN nanoclaw_bridge_secret TEXT;
```

- [ ] **Step 2: Verify numbering**

Run: `ls internal/db/migrations/` — confirm `008_nanoclaw_bridge_secret.sql` is the next sequential file.

- [ ] **Step 3: Commit**

```bash
git add internal/db/migrations/008_nanoclaw_bridge_secret.sql
git commit -m "feat(db): add nanoclaw_bridge_secret column to sandboxes"
```

---

### Task 4: Update DB Layer and Sandbox Store for NanoclawBridgeSecret

**Files:**
- Modify: `internal/db/sandboxes.go` — add field to `Sandbox` struct, `sandboxColumns`, `scanSandbox`, and new `UpdateSandboxNanoclawBridgeSecret` function
- Modify: `internal/sbxstore/store.go` — add field to sbxstore `Sandbox` struct and mapping

The DB layer uses `database/sql` (standard library), NOT pgx. All queries use `db.Exec(query, args...)`, `db.QueryRow(query, args...)`, `db.Query(query, args...)`.

- [ ] **Step 1: Add NanoclawBridgeSecret to db.Sandbox struct**

In `internal/db/sandboxes.go`, add after `TunnelToken` (line 22):

```go
NanoclawBridgeSecret sql.NullString
```

- [ ] **Step 2: Update sandboxColumns**

At line 45, append `, nanoclaw_bridge_secret` to the `sandboxColumns` const:

```go
const sandboxColumns = `id, workspace_id, name, type, status, is_local, short_id, sandbox_name, pod_ip, proxy_token, opencode_token, openclaw_token, tunnel_token, last_activity_at, created_at, paused_at, last_heartbeat_at, cpu, memory, idle_timeout, nanoclaw_bridge_secret`
```

- [ ] **Step 3: Update scanSandbox**

At line 49, add `&s.NanoclawBridgeSecret` to the Scan call (at the end, after `&s.IdleTimeout`):

```go
err := scanner.Scan(&s.ID, &s.WorkspaceID, &s.Name, &s.Type, &s.Status, &s.IsLocal, &s.ShortID, &s.SandboxName, &s.PodIP, &s.ProxyToken, &s.OpencodeToken, &s.OpenclawToken, &s.TunnelToken, &s.LastActivityAt, &s.CreatedAt, &s.PausedAt, &s.LastHeartbeatAt, &s.CPU, &s.Memory, &s.IdleTimeout, &s.NanoclawBridgeSecret)
```

- [ ] **Step 4: Add UpdateSandboxNanoclawBridgeSecret function**

```go
// UpdateSandboxNanoclawBridgeSecret stores the bridge secret for a nanoclaw sandbox.
func (db *DB) UpdateSandboxNanoclawBridgeSecret(id, secret string) error {
	_, err := db.Exec(
		`UPDATE sandboxes SET nanoclaw_bridge_secret = $1 WHERE id = $2`,
		secret, id,
	)
	if err != nil {
		return fmt.Errorf("update nanoclaw bridge secret: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Add NanoclawBridgeSecret to sbxstore.Sandbox struct**

In `internal/sbxstore/store.go`, add to `Sandbox` struct after `OpenclawToken` (line 22):

```go
NanoclawBridgeSecret string     `json:"-"`
```

- [ ] **Step 6: Update dbSandboxToSandbox mapping**

Find the `dbSandboxToSandbox` function in `internal/sbxstore/store.go` and add:

```go
NanoclawBridgeSecret: dbSbx.NanoclawBridgeSecret.String,
```

(Uses `.String` because the DB field is `sql.NullString`.)

- [ ] **Step 7: Commit**

```bash
git add internal/db/sandboxes.go internal/sbxstore/store.go
git commit -m "feat(db): add NanoclawBridgeSecret to sandbox model and DB layer"
```

---

### Task 5: Update Server — Type Validation and Creation Logic

**Files:**
- Modify: `internal/server/server.go`

- [ ] **Step 1: Update type validation in handleCreateSandbox**

At line 1130, change:

```go
if sandboxType != "opencode" && sandboxType != "openclaw" {
    http.Error(w, "invalid sandbox type: must be opencode or openclaw", http.StatusBadRequest)
```

To:

```go
if sandboxType != "opencode" && sandboxType != "openclaw" && sandboxType != "nanoclaw" {
    http.Error(w, "invalid sandbox type: must be opencode, openclaw, or nanoclaw", http.StatusBadRequest)
```

- [ ] **Step 2: Add nanoclaw credential generation**

At lines 1209-1214, add a nanoclaw case to the switch:

```go
switch sandboxType {
case "openclaw":
    openclawToken = generatePassword()
case "nanoclaw":
    // NanoClaw uses a bridge secret instead of openclaw/opencode tokens.
    // The bridge secret is stored separately after sandbox creation.
default: // "opencode"
    opencodeToken = generatePassword()
}
```

After the `Store.Create()` call (line 1221-1231), add:

```go
// Generate and store bridge secret for nanoclaw sandboxes.
if sandboxType == "nanoclaw" {
    bridgeSecret := generatePassword()
    if err := s.DB.UpdateSandboxNanoclawBridgeSecret(id, bridgeSecret); err != nil {
        log.Printf("failed to store nanoclaw bridge secret: %v", err)
    }
    sbx.NanoclawBridgeSecret = bridgeSecret
}
```

(This approach uses a separate update to avoid changing the long `Create()` parameter list.)

- [ ] **Step 3: Add nanoclaw to startOpts**

After the startOpts construction (lines 1234-1243), add:

```go
if sandboxType == "nanoclaw" {
    startOpts.NanoclawBridgeSecret = sbx.NanoclawBridgeSecret
}
```

- [ ] **Step 4: Commit**

```bash
git add internal/server/server.go
git commit -m "feat(server): add nanoclaw type validation and creation logic"
```

---

### Task 6: Update K8s Manager for NanoClaw Pod Spec

**Files:**
- Modify: `internal/sandbox/manager.go`

- [ ] **Step 1: Add nanoclaw case to runtimeClassNameFor**

At `runtimeClassNameFor()` (line 730), add after the openclaw case:

```go
case "nanoclaw":
    if m.cfg.NanoclawRuntimeClassName != "" {
        return strPtr(m.cfg.NanoclawRuntimeClassName)
    }
```

- [ ] **Step 2: Add nanoclaw case to StartContainerWithIP**

In `StartContainerWithIP()`, at the `switch opts.SandboxType` block (line 340), add a new case before `default`:

```go
case "nanoclaw":
    if m.cfg.NanoclawImage != "" {
        sandboxImage = m.cfg.NanoclawImage
    }
    containerPort = 3002 // Health/bridge endpoint
    // Build NanoClaw config as .env content.
    // BYOK overrides are handled inside BuildNanoclawConfig:
    // - proxyBaseURL/proxyToken are the default (llmproxy)
    // - byokBaseURL/byokAPIKey override when non-empty
    weixinBridgeURL := ""
    bridgeSecret := ""
    if m.cfg.NanoclawWeixinEnabled && opts.NanoclawBridgeSecret != "" {
        // TODO: construct actual bridge URL from agentserver base domain
        bridgeSecret = opts.NanoclawBridgeSecret
    }
    nanoclawCfg := BuildNanoclawConfig(
        proxyBaseURL, opts.ProxyToken, "Andy",
        weixinBridgeURL, bridgeSecret,
        opts.BYOKBaseURL, opts.BYOKAPIKey,
    )
    containerEnv = append(containerEnv, corev1.EnvVar{Name: "NANOCLAW_CONFIG_CONTENT", Value: nanoclawCfg})
```

Note: `BuildNanoclawConfig` handles BYOK internally — when `byokBaseURL` is non-empty, it overrides the proxy values. This ensures BYOK works correctly for nanoclaw sandboxes.

- [ ] **Step 3: Add nanoclaw workingDir**

At the `switch opts.SandboxType` block for `workingDir` (line 451), add:

```go
case "nanoclaw":
    workingDir = "/app"
```

- [ ] **Step 4: Update readiness probe for nanoclaw**

After the mainContainer creation (around line 467), add an override for nanoclaw to use HTTP GET instead of TCP:

```go
if opts.SandboxType == "nanoclaw" {
    mainContainer.ReadinessProbe = &corev1.Probe{
        ProbeHandler: corev1.ProbeHandler{
            HTTPGet: &corev1.HTTPGetAction{
                Path: "/health",
                Port: intstr.FromInt32(int32(containerPort)),
            },
        },
        InitialDelaySeconds: 5,
        PeriodSeconds:       5,
        FailureThreshold:    30,
    }
}
```

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/manager.go
git commit -m "feat(manager): add nanoclaw Pod spec with health probe"
```

---

### Task 7: Update Response Builders and Guard Conditions

**Files:**
- Modify: `internal/server/server.go`

- [ ] **Step 1: Update toSandboxResponse to handle nanoclaw type**

At `toSandboxResponse()` (line 467, the switch is at line 487), change the switch to explicitly handle nanoclaw:

```go
switch sbx.Type {
case "openclaw":
    resp.OpenclawURL = "https://" + s.OpenclawSubdomainPrefix + "-" + subID + "." + domain + "/auth?token=" + authToken
case "nanoclaw":
    // NanoClaw has no Web UI — no URL to generate
default: // "opencode"
    resp.OpencodeURL = "https://" + s.OpencodeSubdomainPrefix + "-" + subID + "." + domain + "/auth?token=" + authToken
}
```

- [ ] **Step 2: Update attachWeixinBindings guard**

At `attachWeixinBindings()` (line 531), change:

```go
if resp.Type != "openclaw" {
```

To:

```go
if resp.Type != "openclaw" && resp.Type != "nanoclaw" {
```

- [ ] **Step 3: Update handleWeixinQRStart guard**

At line 1636, change:

```go
if sbx.Type != "openclaw" {
    http.Error(w, "weixin login is only available for openclaw sandboxes", http.StatusBadRequest)
```

To:

```go
if sbx.Type != "openclaw" && sbx.Type != "nanoclaw" {
    http.Error(w, "weixin login is only available for openclaw and nanoclaw sandboxes", http.StatusBadRequest)
```

- [ ] **Step 4: Update handleWeixinQRWait guard**

At line 1675, same change:

```go
if sbx.Type != "openclaw" && sbx.Type != "nanoclaw" {
    http.Error(w, "weixin login is only available for openclaw and nanoclaw sandboxes", http.StatusBadRequest)
```

- [ ] **Step 5: Update saveWeixinCredentials for nanoclaw**

At `saveWeixinCredentials()` (line 1750), the function currently writes credentials into the pod filesystem (openclaw-specific). For nanoclaw, credentials should be saved to DB instead.

**Important restructuring:** The existing function starts with `commander, ok := s.ProcessManager.(execCommander)` which is only needed for openclaw. We need to:
1. Move `accountID` extraction before the commander check (it's needed for both paths)
2. Add a sandbox lookup and type branch before the commander check
3. Keep the existing openclaw logic unchanged after the branch

Rewrite the function as:

```go
func (s *Server) saveWeixinCredentials(ctx context.Context, sandboxID string, result *weixin.StatusResult) error {
	accountID := normalizeAccountID(result.BotID)
	if accountID == "" {
		return fmt.Errorf("empty bot ID from ilink response")
	}

	// For nanoclaw: store credentials in DB (bridge mode).
	sbx, ok := s.Sandboxes.Get(sandboxID)
	if ok && sbx.Type == "nanoclaw" {
		// Phase 3 will add SaveBotCredentials call here.
		// For now, just save the binding record.
		if dbErr := s.DB.CreateWeixinBinding(sandboxID, accountID, result.UserID); dbErr != nil {
			log.Printf("weixin: failed to save binding record: %v", dbErr)
		}
		return nil
	}

	// Existing openclaw logic: write credentials into pod filesystem.
	commander, cmdOk := s.ProcessManager.(execCommander)
	if !cmdOk {
		return fmt.Errorf("process manager does not support exec")
	}

	// ... rest of existing openclaw code unchanged, but note:
	// - Remove the duplicate accountID/normalizeAccountID lines (already done above)
	// - Keep baseURL, credJSON, b64Cred, script, ExecSimple, CreateWeixinBinding as-is
```

The key changes are:
- `accountID` extraction moves before the type branch (was at line 1756, now at the top)
- New `Sandboxes.Get()` call is added to check sandbox type
- The `commander` type assertion is renamed to `cmdOk` to avoid shadowing the outer `ok`
- The rest of the existing openclaw logic remains unchanged

- [ ] **Step 6: Commit**

```bash
git add internal/server/server.go
git commit -m "feat(server): update guards and responses for nanoclaw type"
```

---

### Task 8: Create Dockerfile.nanoclaw and Entrypoint

**Files:**
- Create: `Dockerfile.nanoclaw`
- Create: `nanoclaw-entrypoint.sh`

- [ ] **Step 1: Create entrypoint script**

Create `nanoclaw-entrypoint.sh`:

```bash
#!/bin/sh
# Write .env from NANOCLAW_CONFIG_CONTENT environment variable.
# Same pattern as openclaw config injection via shell heredoc.
if [ -n "$NANOCLAW_CONFIG_CONTENT" ]; then
    echo "$NANOCLAW_CONFIG_CONTENT" > /app/.env
fi
exec "$@"
```

- [ ] **Step 2: Create Dockerfile**

Create `Dockerfile.nanoclaw`:

```dockerfile
ARG NANOCLAW_VERSION=main
FROM node:20-slim AS builder

WORKDIR /app

# Install build dependencies
RUN apt-get update && apt-get install -y git python3 make g++ && rm -rf /var/lib/apt/lists/*

# Clone NanoClaw source at pinned version
ARG NANOCLAW_VERSION
RUN git clone --branch ${NANOCLAW_VERSION} --depth 1 \
    https://github.com/qwibitai/nanoclaw.git . && \
    npm ci && npm run build

FROM node:20-slim

WORKDIR /app

# Install Claude Code CLI
RUN npm install -g @anthropic-ai/claude-code

# Copy built NanoClaw
COPY --from=builder /app /app

# NanoClaw data directories
RUN mkdir -p /app/store /app/groups /app/data

# Config injection entrypoint
COPY nanoclaw-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 3002

ENTRYPOINT ["/entrypoint.sh"]
CMD ["node", "dist/index.js"]
```

Note: Weixin channel and process-runner patches are added in Phase 3. This Dockerfile creates a working baseline NanoClaw image.

- [ ] **Step 3: Commit**

```bash
git add Dockerfile.nanoclaw nanoclaw-entrypoint.sh
git commit -m "feat: add Dockerfile.nanoclaw and entrypoint script"
```

---

### Task 9: Frontend — Add NanoClaw to CreateSandboxModal

**Files:**
- Modify: `web/src/components/CreateSandboxModal.tsx`

- [ ] **Step 1: Update onCreate type**

At the `CreateSandboxModalProps` interface (line 8), update:

```typescript
onCreate: (name: string, type: 'opencode' | 'openclaw' | 'nanoclaw', cpu?: number, memory?: number, idleTimeout?: number) => void
```

- [ ] **Step 2: Add NanoClaw type button**

Find the type selection buttons (lines ~122-148). After the OpenClaw button, add a NanoClaw button following the same pattern:

```tsx
<button
  type="button"
  className={`...${sandboxType === 'nanoclaw' ? ' selected-class' : ''}`}
  onClick={() => setSandboxType('nanoclaw')}
>
  NanoClaw
</button>
```

Match the exact className pattern used by the existing buttons.

- [ ] **Step 3: Update state initial type**

Ensure `sandboxType` state can hold `'nanoclaw'`:

```typescript
const [sandboxType, setSandboxType] = useState<'opencode' | 'openclaw' | 'nanoclaw'>('opencode')
```

- [ ] **Step 4: Commit**

```bash
git add web/src/components/CreateSandboxModal.tsx
git commit -m "feat(web): add NanoClaw type to sandbox creation form"
```

---

### Task 10: Frontend — Update SandboxDetail for NanoClaw

**Files:**
- Modify: `web/src/components/SandboxDetail.tsx`

- [ ] **Step 1: Add isNanoClaw constant**

After `const isOpenClaw = sandbox.type === 'openclaw'` (line 166), add:

```typescript
const isNanoClaw = sandbox.type === 'nanoclaw'
```

- [ ] **Step 2: Update WeChat bindings useEffect**

At lines 149-151, change:

```typescript
if (sandbox.type === 'openclaw') {
```

To:

```typescript
if (sandbox.type === 'openclaw' || sandbox.type === 'nanoclaw') {
```

- [ ] **Step 3: Update WeChat button visibility**

At line 240, change:

```typescript
{isOpenClaw && isRunning && (
```

To:

```typescript
{(isOpenClaw || isNanoClaw) && isRunning && (
```

- [ ] **Step 4: Update sandbox URL**

At line 167, change:

```typescript
const sandboxUrl = isOpenClaw ? sandbox.openclaw_url : sandbox.opencode_url
```

To:

```typescript
const sandboxUrl = isOpenClaw ? sandbox.openclaw_url : isNanoClaw ? null : sandbox.opencode_url
```

And hide the "Open" button when `sandboxUrl` is null (nanoclaw has no Web UI).

- [ ] **Step 5: Update WeChat bindings display**

At lines ~448-470, change the guard from `isOpenClaw` to:

```typescript
{(isOpenClaw || isNanoClaw) && weixinBindings.length > 0 && (
```

- [ ] **Step 6: Commit**

```bash
git add web/src/components/SandboxDetail.tsx
git commit -m "feat(web): update SandboxDetail for nanoclaw type support"
```

---

### Task 11: Verify Phase 1 Build

- [ ] **Step 1: Build backend**

Run: `cd /root/agentserver && go build ./...`
Expected: No errors

- [ ] **Step 2: Run config tests**

Run: `cd /root/agentserver && go test ./internal/sandbox/ -v`
Expected: All tests pass

- [ ] **Step 3: Build frontend**

Run: `cd /root/agentserver/web && npm run build`
Expected: No errors (or check if there's a different build command)

- [ ] **Step 4: Commit any fixes**

If any build issues found, fix and commit.

---

## Phase 3: WeChat Message Bridge

> **BLOCKED:** Phase 3 requires investigating the iLink message API first (spec "Phase 0"). The current `internal/weixin/ilink.go` only implements QR scan flow — message send/receive is entirely new functionality. Before implementing these tasks, confirm:
> 1. Does iLink support webhook/push delivery of inbound messages?
> 2. What is the message send API format?
> 3. How are bot credentials used for messaging?
>
> If iLink only supports polling, the bridge architecture in Task 13 changes significantly.

### Task 12: DB Migration for WeChat Bridge Credentials

**Files:**
- Create: `internal/db/migrations/009_nanoclaw_weixin_bridge.sql`

Note: Must be `009` — follows `008_nanoclaw_bridge_secret.sql` from Phase 1.

- [ ] **Step 1: Create migration**

```sql
-- Add bridge-mode credential columns to sandbox_weixin_bindings.
-- Used by nanoclaw sandboxes where agentserver bridges messages.
ALTER TABLE sandbox_weixin_bindings ADD COLUMN bot_token TEXT;
ALTER TABLE sandbox_weixin_bindings ADD COLUMN ilink_base_url TEXT;
ALTER TABLE sandbox_weixin_bindings ADD COLUMN webhook_registered BOOLEAN DEFAULT FALSE;

-- Index for reverse lookup: given a bot_id from iLink webhook, find the sandbox.
CREATE INDEX IF NOT EXISTS idx_weixin_bindings_bot_id ON sandbox_weixin_bindings(bot_id);
```

- [ ] **Step 2: Commit**

```bash
git add internal/db/migrations/009_nanoclaw_weixin_bridge.sql
git commit -m "feat(db): add weixin bridge credential columns and bot_id index"
```

---

### Task 13: DB Functions for Bridge Credential Lookup

**Files:**
- Modify: `internal/db/weixin_bindings.go`

Note: This project uses `database/sql` (standard library), NOT pgx. Follow the existing pattern in `weixin_bindings.go` — use `db.QueryRow(query, args...)` and `db.Exec(query, args...)`, no context parameter.

- [ ] **Step 1: Add GetSandboxByBotID function**

```go
// GetSandboxByBotID returns the sandbox_id for a given WeChat bot_id.
// Used for routing inbound iLink messages to the correct NanoClaw sandbox.
func (db *DB) GetSandboxByBotID(botID string) (string, error) {
	var sandboxID string
	err := db.QueryRow(
		`SELECT sandbox_id FROM sandbox_weixin_bindings WHERE bot_id = $1 ORDER BY bound_at DESC LIMIT 1`,
		botID,
	).Scan(&sandboxID)
	if err != nil {
		return "", fmt.Errorf("get sandbox by bot ID: %w", err)
	}
	return sandboxID, nil
}
```

- [ ] **Step 2: Add SaveBotCredentials function**

```go
// SaveBotCredentials stores iLink bot credentials for bridge-mode messaging.
// Used by nanoclaw sandboxes where agentserver holds the credentials.
func (db *DB) SaveBotCredentials(sandboxID, botID, botToken, baseURL string) error {
	_, err := db.Exec(
		`UPDATE sandbox_weixin_bindings SET bot_token = $1, ilink_base_url = $2
		 WHERE sandbox_id = $3 AND bot_id = $4`,
		botToken, baseURL, sandboxID, botID,
	)
	if err != nil {
		return fmt.Errorf("save bot credentials: %w", err)
	}
	return nil
}
```

- [ ] **Step 3: Commit**

```bash
git add internal/db/weixin_bindings.go
git commit -m "feat(db): add GetSandboxByBotID and SaveBotCredentials"
```

---

### Task 14: Update saveWeixinCredentials for NanoClaw Bridge Mode

**Files:**
- Modify: `internal/server/server.go`

- [ ] **Step 1: Implement nanoclaw branch in saveWeixinCredentials**

Replace the placeholder from Task 7 Step 5 with full implementation:

```go
if sbx.Type == "nanoclaw" {
    baseURL := result.BaseURL
    if baseURL == "" {
        baseURL = weixin.DefaultAPIBaseURL
    }
    // Save binding record first.
    if dbErr := s.DB.CreateWeixinBinding(sandboxID, accountID, result.UserID); dbErr != nil {
        return fmt.Errorf("save binding: %w", dbErr)
    }
    // Store bot credentials for bridge messaging.
    if dbErr := s.DB.SaveBotCredentials(sandboxID, accountID, result.Token, baseURL); dbErr != nil {
        return fmt.Errorf("save bot credentials: %w", dbErr)
    }
    // TODO: Register webhook with iLink for inbound message delivery
    // (blocked on iLink API investigation)
    return nil
}
```

- [ ] **Step 2: Commit**

```bash
git add internal/server/server.go
git commit -m "feat(server): save weixin bridge credentials for nanoclaw sandboxes"
```

---

### Task 15: iLink Message Send/Receive API (BLOCKED)

> **This task is blocked** until the iLink API is investigated. See spec Phase 0.
> Once the API format is confirmed, implement:
> - `ilink.go`: `SendMessage(botToken, userID, content)` and `RegisterWebhook(botToken, webhookURL)`
> - Server endpoint: `POST /api/weixin/message-callback` (receives iLink push)
> - Server endpoint: `POST /api/internal/nanoclaw/{id}/weixin/send` (NanoClaw → agentserver → iLink)

---

### Task 16: NanoClaw Weixin Channel Implementation (BLOCKED)

> **Blocked on Task 15.** Once the bridge endpoints exist, implement:
> - `nanoclaw-weixin-channel/index.ts` — the TypeScript channel implementation
> - Update Dockerfile.nanoclaw to COPY the channel and add it to the barrel import

---

### Task 17: NanoClaw Process Runner Patch (BLOCKED)

> **Blocked on having a working NanoClaw container to test against.**
> - `nanoclaw-patches/process-runner.ts` — agent execution without Docker
> - Test by building Dockerfile.nanoclaw and running with `NANOCLAW_NO_CONTAINER=true`
