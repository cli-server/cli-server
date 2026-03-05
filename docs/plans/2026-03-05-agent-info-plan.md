# Agent Info Collection & Display — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Collect local agent system information via gopsutil v4 and display it in the sandbox list UI with expandable detail panels.

**Architecture:** Agent collects system info using gopsutil v4 on each tunnel connection, sends it as a JSON text WebSocket message. Server stores it in a dedicated `agent_info` table. Frontend displays it in expandable cards for local sandboxes.

**Tech Stack:** Go (gopsutil/v4), PostgreSQL (JSONB), React/TypeScript, WebSocket text messages

---

### Task 1: Database Migration — Create `agent_info` Table

**Files:**
- Create: `internal/db/migrations/002_agent_info.sql`

**Step 1: Write the migration file**

```sql
CREATE TABLE agent_info (
    sandbox_id         TEXT PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,

    -- Primary fields (for direct display)
    hostname           TEXT NOT NULL DEFAULT '',
    os                 TEXT NOT NULL DEFAULT '',
    platform           TEXT NOT NULL DEFAULT '',
    platform_version   TEXT NOT NULL DEFAULT '',
    kernel_arch        TEXT NOT NULL DEFAULT '',
    cpu_model_name     TEXT NOT NULL DEFAULT '',
    cpu_count_logical  INTEGER NOT NULL DEFAULT 0,
    memory_total       BIGINT NOT NULL DEFAULT 0,
    disk_total         BIGINT NOT NULL DEFAULT 0,
    disk_free          BIGINT NOT NULL DEFAULT 0,
    agent_version      TEXT NOT NULL DEFAULT '',
    opencode_version   TEXT NOT NULL DEFAULT '',

    -- Detailed info (gopsutil raw structs)
    host_info          JSONB,
    cpu_info           JSONB,
    memory_info        JSONB,
    disk_info          JSONB,

    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

**Step 2: Verify migration compiles**

Run: `go build ./...`
Expected: PASS (migrations are embedded via `//go:embed`)

**Step 3: Commit**

```bash
git add internal/db/migrations/002_agent_info.sql
git commit -m "feat(db): add agent_info table migration"
```

---

### Task 2: Database Layer — `UpsertAgentInfo` and `GetAgentInfo`

**Files:**
- Create: `internal/db/agent_info.go`

**Step 1: Write the database access layer**

```go
package db

import (
	"encoding/json"
	"time"
)

// AgentInfo holds system information reported by a local agent.
type AgentInfo struct {
	SandboxID       string
	Hostname        string
	OS              string
	Platform        string
	PlatformVersion string
	KernelArch      string
	CPUModelName    string
	CPUCountLogical int
	MemoryTotal     int64
	DiskTotal       int64
	DiskFree        int64
	AgentVersion    string
	OpencodeVersion string
	HostInfo        json.RawMessage
	CPUInfo         json.RawMessage
	MemoryInfo      json.RawMessage
	DiskInfo        json.RawMessage
	UpdatedAt       time.Time
}

// UpsertAgentInfo inserts or updates agent info for a sandbox.
func (db *DB) UpsertAgentInfo(info *AgentInfo) error {
	_, err := db.Exec(`
		INSERT INTO agent_info (
			sandbox_id, hostname, os, platform, platform_version, kernel_arch,
			cpu_model_name, cpu_count_logical, memory_total, disk_total, disk_free,
			agent_version, opencode_version,
			host_info, cpu_info, memory_info, disk_info, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,NOW())
		ON CONFLICT (sandbox_id) DO UPDATE SET
			hostname=EXCLUDED.hostname, os=EXCLUDED.os, platform=EXCLUDED.platform,
			platform_version=EXCLUDED.platform_version, kernel_arch=EXCLUDED.kernel_arch,
			cpu_model_name=EXCLUDED.cpu_model_name, cpu_count_logical=EXCLUDED.cpu_count_logical,
			memory_total=EXCLUDED.memory_total, disk_total=EXCLUDED.disk_total, disk_free=EXCLUDED.disk_free,
			agent_version=EXCLUDED.agent_version, opencode_version=EXCLUDED.opencode_version,
			host_info=EXCLUDED.host_info, cpu_info=EXCLUDED.cpu_info,
			memory_info=EXCLUDED.memory_info, disk_info=EXCLUDED.disk_info,
			updated_at=NOW()`,
		info.SandboxID, info.Hostname, info.OS, info.Platform, info.PlatformVersion, info.KernelArch,
		info.CPUModelName, info.CPUCountLogical, info.MemoryTotal, info.DiskTotal, info.DiskFree,
		info.AgentVersion, info.OpencodeVersion,
		info.HostInfo, info.CPUInfo, info.MemoryInfo, info.DiskInfo,
	)
	return err
}

// GetAgentInfo returns the agent info for a sandbox, or nil if not found.
func (db *DB) GetAgentInfo(sandboxID string) (*AgentInfo, error) {
	var info AgentInfo
	err := db.QueryRow(`
		SELECT sandbox_id, hostname, os, platform, platform_version, kernel_arch,
			cpu_model_name, cpu_count_logical, memory_total, disk_total, disk_free,
			agent_version, opencode_version,
			host_info, cpu_info, memory_info, disk_info, updated_at
		FROM agent_info WHERE sandbox_id = $1`, sandboxID,
	).Scan(
		&info.SandboxID, &info.Hostname, &info.OS, &info.Platform, &info.PlatformVersion, &info.KernelArch,
		&info.CPUModelName, &info.CPUCountLogical, &info.MemoryTotal, &info.DiskTotal, &info.DiskFree,
		&info.AgentVersion, &info.OpencodeVersion,
		&info.HostInfo, &info.CPUInfo, &info.MemoryInfo, &info.DiskInfo, &info.UpdatedAt,
	)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	return &info, nil
}
```

**Step 2: Verify build**

Run: `go build ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add internal/db/agent_info.go
git commit -m "feat(db): add UpsertAgentInfo and GetAgentInfo"
```

---

### Task 3: Tunnel Protocol — Add `agent_info` Message Type

**Files:**
- Modify: `internal/tunnel/protocol.go` (add constant)
- Modify: `internal/tunnel/registry.go:146-201` (handle text messages in readLoop)

**Step 1: Add message type constant to `protocol.go`**

After the existing `FrameTypeStream` constant (line 13), add:

```go
const FrameTypeAgentInfo = "agent_info"
```

Also add the AgentInfo message struct:

```go
// AgentInfoMessage is a JSON text message sent by the agent after connecting.
type AgentInfoMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}
```

**Step 2: Add callback support and text message handling to `Tunnel` in `registry.go`**

Add an `OnAgentInfo` callback field to the `Tunnel` struct (line 73):

```go
type Tunnel struct {
	SandboxID    string
	Conn         *websocket.Conn
	OnAgentInfo  func(data json.RawMessage) // called when agent_info message received
	pending      map[string]*streamWaiter
	mu           sync.Mutex
	done         chan struct{}
	closeOnce    sync.Once
}
```

Modify `readLoop()` (line 147) to handle text messages before decoding binary frames:

```go
func (t *Tunnel) readLoop() {
	defer t.Close()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		msgType, data, err := t.Conn.Read(ctx)
		cancel()
		if err != nil {
			select {
			case <-t.done:
				return
			default:
			}
			if !errors.Is(err, context.Canceled) {
				log.Printf("tunnel %s: read error: %v", t.SandboxID, err)
			}
			return
		}

		// Handle text messages (agent_info, etc.)
		if msgType == websocket.MessageText {
			t.handleTextMessage(data)
			continue
		}

		// Existing binary frame handling...
		headerJSON, payload, err := DecodeFrameHeader(data)
		// ... rest unchanged
	}
}

func (t *Tunnel) handleTextMessage(data []byte) {
	var msg struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("tunnel %s: failed to unmarshal text message: %v", t.SandboxID, err)
		return
	}
	switch msg.Type {
	case FrameTypeAgentInfo:
		if t.OnAgentInfo != nil {
			t.OnAgentInfo(msg.Data)
		}
	default:
		log.Printf("tunnel %s: unknown text message type: %s", t.SandboxID, msg.Type)
	}
}
```

**Important note:** The current `readLoop` uses `_, data, err := t.Conn.Read(ctx)` which discards message type. Change to `msgType, data, err := t.Conn.Read(ctx)` to distinguish text vs binary.

**Step 3: Verify build**

Run: `go build ./...`
Expected: PASS

**Step 4: Commit**

```bash
git add internal/tunnel/protocol.go internal/tunnel/registry.go
git commit -m "feat(tunnel): add agent_info text message support"
```

---

### Task 4: Agent — Collect System Info and Send via Tunnel

**Files:**
- Modify: `go.mod` (add gopsutil/v4 dependency)
- Create: `internal/agent/sysinfo.go` (system info collection)
- Modify: `internal/agent/client.go:104-112` (send agent_info after connect)

**Step 1: Add gopsutil dependency**

Run: `go get github.com/shirou/gopsutil/v4@latest`

**Step 2: Create `internal/agent/sysinfo.go`**

```go
package agent

import (
	"context"
	"encoding/json"
	"log"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

// AgentInfoData is the payload sent to the server as agent_info.
type AgentInfoData struct {
	// Primary fields
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Platform        string `json:"platform"`
	PlatformVersion string `json:"platform_version"`
	KernelArch      string `json:"kernel_arch"`
	CPUModelName    string `json:"cpu_model_name"`
	CPUCountLogical int    `json:"cpu_count_logical"`
	MemoryTotal     uint64 `json:"memory_total"`
	DiskTotal       uint64 `json:"disk_total"`
	DiskFree        uint64 `json:"disk_free"`
	AgentVersion    string `json:"agent_version"`
	OpencodeVersion string `json:"opencode_version"`

	// Detailed info (gopsutil raw structs)
	HostInfo   *host.InfoStat            `json:"host_info,omitempty"`
	CPUInfo    *cpuInfoDetail            `json:"cpu_info,omitempty"`
	MemoryInfo *mem.VirtualMemoryStat    `json:"memory_info,omitempty"`
	DiskInfo   *disk.UsageStat           `json:"disk_info,omitempty"`
}

type cpuInfoDetail struct {
	CPUs          []cpu.InfoStat `json:"cpus"`
	CountPhysical int            `json:"count_physical"`
	CountLogical  int            `json:"count_logical"`
}

// Version is set at build time via ldflags.
var Version = "dev"

func collectAgentInfo(opencodeURL string) *AgentInfoData {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info := &AgentInfoData{
		AgentVersion: Version,
	}

	// Host info
	if h, err := host.InfoWithContext(ctx); err == nil {
		info.Hostname = h.Hostname
		info.OS = h.OS
		info.Platform = h.Platform
		info.PlatformVersion = h.PlatformVersion
		info.KernelArch = h.KernelArch
		info.HostInfo = h
	} else {
		log.Printf("agent info: failed to get host info: %v", err)
	}

	// CPU info
	if cpus, err := cpu.InfoWithContext(ctx); err == nil && len(cpus) > 0 {
		info.CPUModelName = cpus[0].ModelName
		detail := &cpuInfoDetail{CPUs: cpus}
		if count, err := cpu.CountsWithContext(ctx, false); err == nil {
			detail.CountPhysical = count
		}
		if count, err := cpu.CountsWithContext(ctx, true); err == nil {
			detail.CountLogical = count
			info.CPUCountLogical = count
		}
		info.CPUInfo = detail
	} else if err != nil {
		log.Printf("agent info: failed to get cpu info: %v", err)
	}

	// Memory info
	if m, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		info.MemoryTotal = m.Total
		info.MemoryInfo = m
	} else {
		log.Printf("agent info: failed to get memory info: %v", err)
	}

	// Disk info (root partition)
	diskPath := "/"
	if runtime.GOOS == "windows" {
		diskPath = "C:"
	}
	if d, err := disk.UsageWithContext(ctx, diskPath); err == nil {
		info.DiskTotal = d.Total
		info.DiskFree = d.Free
		info.DiskInfo = d
	} else {
		log.Printf("agent info: failed to get disk info: %v", err)
	}

	// Opencode version (best-effort)
	if opencodeURL != "" {
		info.OpencodeVersion = fetchOpencodeVersion(opencodeURL)
	}

	return info
}

// fetchOpencodeVersion tries to get the opencode version via its API.
func fetchOpencodeVersion(opencodeURL string) string {
	// TODO: implement once opencode exposes a version endpoint
	// For now, return empty string (displayed as "Unknown" in UI)
	return ""
}
```

**Step 3: Modify `internal/agent/client.go` — send agent_info after WebSocket connect**

In `connectAndServe()` (around line 112, after `log.Printf("tunnel connected...")`), add:

```go
	log.Printf("tunnel connected (sandbox: %s)", c.SandboxID)

	// Collect and send agent info.
	agentInfo := collectAgentInfo(c.OpencodeURL)
	infoMsg := struct {
		Type string          `json:"type"`
		Data *AgentInfoData  `json:"data"`
	}{
		Type: "agent_info",
		Data: agentInfo,
	}
	if infoJSON, err := json.Marshal(infoMsg); err == nil {
		if err := conn.Write(ctx, websocket.MessageText, infoJSON); err != nil {
			log.Printf("failed to send agent info: %v", err)
		}
	}

	// Read and process binary frames.
```

Also add `"nhooyr.io/websocket"` import for `websocket.MessageText` (already imported).

**Step 4: Verify build**

Run: `go build ./...`
Expected: PASS

**Step 5: Commit**

```bash
git add go.mod go.sum internal/agent/sysinfo.go internal/agent/client.go
git commit -m "feat(agent): collect system info via gopsutil and send on tunnel connect"
```

---

### Task 5: Server — Handle `agent_info` Message and Store

**Files:**
- Modify: `internal/server/tunnel.go:55-57` (set OnAgentInfo callback after Register)

**Step 1: Set the OnAgentInfo callback in `handleTunnel`**

In `handleTunnel()`, after `t := s.TunnelRegistry.Register(sandboxID, conn)` (around line 55), add:

```go
	// Register tunnel.
	t := s.TunnelRegistry.Register(sandboxID, conn)
	t.OnAgentInfo = func(data json.RawMessage) {
		var info struct {
			Hostname        string          `json:"hostname"`
			OS              string          `json:"os"`
			Platform        string          `json:"platform"`
			PlatformVersion string          `json:"platform_version"`
			KernelArch      string          `json:"kernel_arch"`
			CPUModelName    string          `json:"cpu_model_name"`
			CPUCountLogical int             `json:"cpu_count_logical"`
			MemoryTotal     int64           `json:"memory_total"`
			DiskTotal       int64           `json:"disk_total"`
			DiskFree        int64           `json:"disk_free"`
			AgentVersion    string          `json:"agent_version"`
			OpencodeVersion string          `json:"opencode_version"`
			HostInfo        json.RawMessage `json:"host_info"`
			CPUInfo         json.RawMessage `json:"cpu_info"`
			MemoryInfo      json.RawMessage `json:"memory_info"`
			DiskInfo        json.RawMessage `json:"disk_info"`
		}
		if err := json.Unmarshal(data, &info); err != nil {
			log.Printf("tunnel %s: failed to parse agent info: %v", sandboxID, err)
			return
		}
		if err := s.DB.UpsertAgentInfo(&db.AgentInfo{
			SandboxID:       sandboxID,
			Hostname:        info.Hostname,
			OS:              info.OS,
			Platform:        info.Platform,
			PlatformVersion: info.PlatformVersion,
			KernelArch:      info.KernelArch,
			CPUModelName:    info.CPUModelName,
			CPUCountLogical: info.CPUCountLogical,
			MemoryTotal:     info.MemoryTotal,
			DiskTotal:       info.DiskTotal,
			DiskFree:        info.DiskFree,
			AgentVersion:    info.AgentVersion,
			OpencodeVersion: info.OpencodeVersion,
			HostInfo:        info.HostInfo,
			CPUInfo:         info.CPUInfo,
			MemoryInfo:      info.MemoryInfo,
			DiskInfo:        info.DiskInfo,
		}); err != nil {
			log.Printf("tunnel %s: failed to store agent info: %v", sandboxID, err)
		}
	}
	log.Printf("tunnel connected: sandbox %s", sandboxID)
```

Ensure `"encoding/json"` and the db package are imported in tunnel.go.

**Step 2: Verify build**

Run: `go build ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add internal/server/tunnel.go
git commit -m "feat(server): store agent_info from tunnel text messages"
```

---

### Task 6: API — Return `agent_info` in Sandbox Responses

**Files:**
- Modify: `internal/server/server.go` (sandboxResponse struct ~line 387, toSandboxResponse ~line 415)

**Step 1: Add AgentInfo to sandboxResponse**

Add a new response struct and field to `sandboxResponse` (after line 404):

```go
type agentInfoResponse struct {
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Platform        string `json:"platform"`
	PlatformVersion string `json:"platform_version"`
	KernelArch      string `json:"kernel_arch"`
	CPUModelName    string `json:"cpu_model_name"`
	CPUCountLogical int    `json:"cpu_count_logical"`
	MemoryTotal     int64  `json:"memory_total"`
	DiskTotal       int64  `json:"disk_total"`
	DiskFree        int64  `json:"disk_free"`
	AgentVersion    string `json:"agent_version"`
	OpencodeVersion string `json:"opencode_version"`
	UpdatedAt       string `json:"updated_at"`
}
```

Add field to `sandboxResponse`:

```go
AgentInfo *agentInfoResponse `json:"agent_info,omitempty"`
```

**Step 2: Populate agent_info in `toSandboxResponse`**

At the end of `toSandboxResponse`, before `return resp`, add:

```go
	if sbx.IsLocal {
		if info, err := s.DB.GetAgentInfo(sbx.ID); err == nil && info != nil {
			resp.AgentInfo = &agentInfoResponse{
				Hostname:        info.Hostname,
				OS:              info.OS,
				Platform:        info.Platform,
				PlatformVersion: info.PlatformVersion,
				KernelArch:      info.KernelArch,
				CPUModelName:    info.CPUModelName,
				CPUCountLogical: info.CPUCountLogical,
				MemoryTotal:     info.MemoryTotal,
				DiskTotal:       info.DiskTotal,
				DiskFree:        info.DiskFree,
				AgentVersion:    info.AgentVersion,
				OpencodeVersion: info.OpencodeVersion,
				UpdatedAt:       info.UpdatedAt.Format(time.RFC3339),
			}
		}
	}
```

**Step 3: Verify build**

Run: `go build ./...`
Expected: PASS

**Step 4: Commit**

```bash
git add internal/server/server.go
git commit -m "feat(api): return agent_info in sandbox responses"
```

---

### Task 7: Frontend — TypeScript Types and API

**Files:**
- Modify: `web/src/lib/api.ts`

**Step 1: Add `AgentInfo` interface**

After the `Sandbox` interface, add:

```typescript
export interface AgentInfo {
  hostname: string
  os: string
  platform: string
  platform_version: string
  kernel_arch: string
  cpu_model_name: string
  cpu_count_logical: number
  memory_total: number
  disk_total: number
  disk_free: number
  agent_version: string
  opencode_version: string
  updated_at: string
}
```

**Step 2: Add `agent_info` to Sandbox interface**

Add to the `Sandbox` interface:

```typescript
agent_info?: AgentInfo
```

**Step 3: Commit**

```bash
git add web/src/lib/api.ts
git commit -m "feat(web): add AgentInfo type definition"
```

---

### Task 8: Frontend — Expandable Agent Info Panel in SandboxList

**Files:**
- Modify: `web/src/components/SandboxList.tsx`

**Step 1: Add helper function for formatting bytes**

At the top of the file (outside the component), add:

```typescript
function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(k))
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i]
}
```

**Step 2: Add expand state tracking**

Inside the component, add state:

```typescript
const [expandedSandboxId, setExpandedSandboxId] = useState<string | null>(null)
```

**Step 3: Add expand toggle and info panel to sandbox cards**

For each sandbox card in the list (the one that maps over `sandboxes`), for `is_local` sandboxes, add:

1. A chevron button that toggles `expandedSandboxId`
2. When expanded, render an info grid showing:
   - OS: `{info.platform} {info.platform_version}` (e.g. "ubuntu 22.04")
   - Arch: `{info.kernel_arch}`
   - Hostname: `{info.hostname}`
   - CPU: `{info.cpu_model_name} ({info.cpu_count_logical} cores)`
   - Memory: `{formatBytes(info.memory_total)}`
   - Disk: `{formatBytes(info.disk_free)} free / {formatBytes(info.disk_total)}`
   - Agent: `{info.agent_version}`
   - opencode: `{info.opencode_version || 'Unknown'}`

The exact JSX/CSS depends on the existing component's styling patterns. Use the same styling conventions as the rest of SandboxList (inline styles or whatever CSS approach is used).

**Key UI details:**
- Chevron icon rotates when expanded (▸ → ▾)
- Info grid uses 2-column layout on desktop, 1-column on mobile
- Labels are muted/gray, values are normal weight
- Only show for `is_local` sandboxes that have `agent_info`
- "Updated at" shown in small muted text at bottom of panel

**Step 4: Build frontend**

Run: `cd web && npm run build`
Expected: PASS

**Step 5: Commit**

```bash
git add web/src/components/SandboxList.tsx
git commit -m "feat(web): add expandable agent info panel for local sandboxes"
```

---

### Task 9: Build Verification and Embed

**Files:**
- Verify: `web/embed.go` (embedded frontend assets)

**Step 1: Rebuild frontend and embed**

Run:
```bash
cd web && npm run build && cd ..
go build ./...
```
Expected: Both PASS

**Step 2: Final commit**

```bash
git add -A
git commit -m "chore: rebuild frontend assets"
```

---

## Task Summary

| Task | Description | Files |
|------|-------------|-------|
| 1 | DB migration | `migrations/002_agent_info.sql` |
| 2 | DB access layer | `db/agent_info.go` |
| 3 | Tunnel protocol extension | `tunnel/protocol.go`, `tunnel/registry.go` |
| 4 | Agent info collection | `agent/sysinfo.go`, `agent/client.go`, `go.mod` |
| 5 | Server tunnel handler | `server/tunnel.go` |
| 6 | API response extension | `server/server.go` |
| 7 | Frontend types | `web/src/lib/api.ts` |
| 8 | Frontend UI panel | `web/src/components/SandboxList.tsx` |
| 9 | Build verification | All |
