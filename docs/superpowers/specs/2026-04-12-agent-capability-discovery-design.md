# Agent Capability Self-Discovery Design

**Date:** 2026-04-12
**Status:** Draft

## Problem

All local agents currently display identical capability cards:

```
能力：代码编辑、代码审查、终端、代码搜索
```

This happens because agents only report system hardware info via the heartbeat control stream (`AgentInfoData`). The `card_json` field in `agent_cards` is always empty `{}` — the server has no way to know what tools, languages, or runtimes are installed on each agent's machine.

## Goal

When a local agent connects, it should probe its own machine to discover:

1. **Installed languages** — Go, Python, Node, Rust, Java, etc., with versions
2. **Installed tools** — Docker, kubectl, git, make, Helm, etc., with versions
3. **Hardware summary** — CPU, memory, disk, GPU presence

The result should appear in the agent card as categorized information:

```
MacBook Air
• 语言: Go 1.22, Python 3.12, Node 20
• 工具: Docker, Git, Make
• 硬件: 8核 / 16GB / 256GB SSD

MacBook Pro 16
• 语言: Go 1.22, Rust 1.77, Python 3.12
• 工具: Docker, kubectl, Helm, Git
• 硬件: 12核 / 36GB / 1TB SSD / M3 GPU
```

## Approach

Extend the existing heartbeat flow (Approach A): add capability probing to the agent side, send results as part of `AgentInfoData` via the existing yamux control stream, and have the server extract capabilities into `agent_cards.card_json`.

## Design

### 1. Agent-Side: Capability Probing

**New file:** `internal/agent/capabilities.go`

#### Data Structures

```go
// AgentCapabilities is the result of probing the local machine.
// Hardware info is NOT included here — it's already in AgentInfoData
// and stored in agent_info by the existing heartbeat flow.
type AgentCapabilities struct {
    Languages []RuntimeInfo `json:"languages"`
    Tools     []RuntimeInfo `json:"tools"`
    GPU       *GPUInfo      `json:"gpu,omitempty"`
    ProbedAt  time.Time     `json:"probed_at"`
}

// RuntimeInfo describes an installed language or tool.
type RuntimeInfo struct {
    Name    string `json:"name"`              // "go", "python3", "docker"
    Version string `json:"version"`           // "1.22.0", "3.12.1"
    Path    string `json:"path,omitempty"`    // "/usr/local/go/bin/go"
}

// GPUInfo describes GPU hardware (not covered by gopsutil).
type GPUInfo struct {
    Name   string `json:"name"`              // "Apple M3 Pro GPU" or "NVIDIA RTX 4090"
    Memory string `json:"memory,omitempty"`  // "24GB" (discrete GPUs only)
}
```

#### Probe Registry

Each probe is a struct with a command and a parser. Probes are organized by type (language vs. tool):

```go
type probe struct {
    Name    string
    Cmd     string   // Shell command, e.g. "go version"
    Args    []string // Alternative: exec args
    Parser  func(stdout string) (version string)
    Type    string   // "language" or "tool"
}
```

**Initial probe set:**

| Type | Name | Command | Notes |
|------|------|---------|-------|
| language | go | `go version` | |
| language | python3 | `python3 --version` | Falls back to `python --version` |
| language | node | `node --version` | |
| language | rust | `rustc --version` | |
| language | java | `java -version` | Outputs to stderr |
| language | ruby | `ruby --version` | |
| language | php | `php --version` | |
| tool | docker | `docker --version` | |
| tool | kubectl | `kubectl version --client --short` | |
| tool | git | `git --version` | |
| tool | make | `make --version` | |
| tool | helm | `helm version --short` | |
| tool | cmake | `cmake --version` | |
| tool | terraform | `terraform version` | |
| tool | aws | `aws --version` | |
| tool | gcloud | `gcloud --version` | First line only |
| tool | ffmpeg | `ffmpeg -version` | |

Each probe runs via `exec.CommandContext` with a **2-second timeout**. If the command fails or is not found, the probe is silently skipped.

#### Execution Strategy

```go
func ProbeCapabilities(ctx context.Context) *AgentCapabilities {
    // Run all probes concurrently with a WaitGroup.
    // Each probe gets a 2s timeout child context.
    // Collect results into languages[] and tools[].
    // Build HardwareSum from existing gopsutil data.
    // Total wall time: ~2-3 seconds.
}
```

All probes run concurrently via goroutines. A `sync.Mutex` guards the result slices. The function blocks until all probes complete (or time out).

#### GPU Detection

- **macOS:** Parse `system_profiler SPDisplaysDataType -json` for GPU chipset and core count
- **Linux:** Check for `nvidia-smi` binary; if present, run `nvidia-smi --query-gpu=name,memory.total --format=csv,noheader`
- **Fallback:** On Apple Silicon, infer GPU from CPU model name (M1/M2/M3 all have integrated GPU)

#### Hardware Summary

CPU, memory, and disk data is already collected by `collectAgentInfo()` via `gopsutil` and stored in `agent_info`. We do NOT duplicate this in `AgentCapabilities`.

The only hardware info that `AgentCapabilities` adds is GPU detection, since `gopsutil` does not cover GPUs. The server builds the full hardware summary by combining `agent_info` (CPU/memory/disk) with `capabilities.gpu` (GPU).

### 2. Agent-Side: Integration with Heartbeat

**Modified file:** `internal/agent/sysinfo.go`

Add the `Capabilities` field to `AgentInfoData`:

```go
type AgentInfoData struct {
    // ... all existing fields unchanged ...
    Capabilities *AgentCapabilities `json:"capabilities,omitempty"`
}
```

**Modified file:** `internal/agent/client.go`

Add a cached capabilities field to `Client`:

```go
type Client struct {
    // ... existing fields ...
    cachedCapabilities *AgentCapabilities
    capabilitiesMu     sync.Mutex
    lastProbeTime      time.Time
}
```

Update `sendAgentInfo()` to include capabilities:

```go
func (c *Client) sendAgentInfo(session *yamux.Session) {
    // ... existing code ...
    info := collectAgentInfo(c.OpencodeURL, c.Workdir)

    // Attach capabilities (probe if needed).
    // Probe OUTSIDE the lock to avoid blocking heartbeats for 2-3 seconds.
    c.capabilitiesMu.Lock()
    needsProbe := c.cachedCapabilities == nil || time.Since(c.lastProbeTime) > 1*time.Hour
    c.capabilitiesMu.Unlock()

    if needsProbe {
        caps := ProbeCapabilities(context.Background())
        c.capabilitiesMu.Lock()
        c.cachedCapabilities = caps
        c.lastProbeTime = time.Now()
        c.capabilitiesMu.Unlock()
    }

    c.capabilitiesMu.Lock()
    info.Capabilities = c.cachedCapabilities
    c.capabilitiesMu.Unlock()

    // ... rest of existing code (marshal, send via control stream) ...
}
```

**Timing:**
- First heartbeat (immediate on connect): runs full probe (~2-3s one-time cost)
- Subsequent heartbeats (every 20s): uses cached result, no probe overhead
- After 1 hour: re-probes on next heartbeat to catch newly installed tools

### 2b. Agent-Side: Replace Hardcoded Card Registration

**Modified file:** `internal/agent/task_worker.go`

The existing `RegisterDefaultCard()` function hardcodes 4 skills and POSTs to `/api/agent/discovery/cards`. This is the root cause of all agents looking identical.

**Change:** Remove `RegisterDefaultCard()`. Card registration is now driven entirely by the heartbeat capability flow (Section 4 below). The server creates/updates the card when it receives the first heartbeat with capabilities.

**Modified file:** `internal/agent/claudecode.go`

Remove the `RegisterDefaultCard()` call at line 182. The agent card will be created by the server when the first heartbeat arrives with capabilities data (within ~3 seconds of connect).

Note: There is a brief window (~3s) between tunnel connect and the first heartbeat where no card exists. This is acceptable because:
- The agent is already marked as "running" by the tunnel handler
- The card appears within seconds
- The agent discovery API already handles missing cards gracefully

### 3. Server-Side: Store Capabilities

**Modified file:** `internal/db/agent_info.go`

Add a `capabilities` column to `AgentInfo`:

```go
type AgentInfo struct {
    // ... existing fields ...
    Capabilities json.RawMessage `json:"capabilities"`
}
```

Update `UpsertAgentInfo` to include the capabilities column.

**New migration:** `internal/db/migrations/017_agent_capabilities.sql`

```sql
ALTER TABLE agent_info ADD COLUMN capabilities JSONB;
```

### 4. Server-Side: Sync Capabilities to Agent Card

**New file:** `internal/sandboxproxy/capabilities.go`

This file lives in `sandboxproxy` (server-side) and handles the transformation from raw capability JSON to agent card format. It does NOT import the `agent` package — it unmarshals directly from `json.RawMessage` using local struct definitions that mirror the agent-side types.

```go
package sandboxproxy

// capabilitiesPayload mirrors agent.AgentCapabilities for JSON unmarshaling.
// Defined locally to avoid importing the agent (CLI client) package.
type capabilitiesPayload struct {
    Languages []runtimeEntry `json:"languages"`
    Tools     []runtimeEntry `json:"tools"`
    GPU       *gpuEntry      `json:"gpu,omitempty"`
    ProbedAt  string         `json:"probed_at"`
}

type runtimeEntry struct {
    Name    string `json:"name"`
    Version string `json:"version"`
}

type gpuEntry struct {
    Name   string `json:"name"`
    Memory string `json:"memory,omitempty"`
}
```

**`buildCardJSON`** transforms capabilities + agent_info into the `card_json` format:

```go
func buildCardJSON(caps *capabilitiesPayload, info *db.AgentInfo) json.RawMessage {
    card := map[string]any{
        "languages": caps.Languages,
        "tools":     caps.Tools,
        "gpu":       caps.GPU,
        "hardware":  buildHardwareSummary(info, caps.GPU),
        "skills":    buildSkillsFromCapabilities(caps),
        "tags":      buildTagsFromCapabilities(caps, info),
    }
    data, _ := json.Marshal(card)
    return data
}
```

`buildHardwareSummary` constructs the hardware section from `agent_info` data (CPU, memory, disk — already stored) plus `capabilities.gpu`:

```go
func buildHardwareSummary(info *db.AgentInfo, gpu *gpuEntry) map[string]any {
    hw := map[string]any{
        "cpu_summary":  fmt.Sprintf("%s, %d cores", info.CPUModelName, info.CPUCountLogical),
        "memory_gb":    info.MemoryTotal / (1024*1024*1024),
        "disk_gb":      info.DiskTotal / (1024*1024*1024),
        "disk_free_gb": info.DiskFree / (1024*1024*1024),
    }
    if gpu != nil {
        hw["has_gpu"] = true
        hw["gpu_info"] = gpu.Name
    }
    return hw
}
```

**Skill generation rules:**
- Always include base skills: `code-editing`, `code-review`, `terminal`, `code-search`
- Each language → skill with `name: "{lang}-development"`, description from template
- Tools like Docker/kubectl → skill entries (e.g., `name: "container-management"`)

**Tag generation rules:**
- Each language name as a tag: `["go", "python", "node", ...]`
- Each tool name as a tag: `["docker", "kubectl", "git", ...]`
- Hardware tags: `"gpu"` if GPU present, `"high-memory"` if memory >= 32GB

**Modified file:** `internal/sandboxproxy/tunnel.go`

Extend the `OnAgentInfo` callback to also upsert the agent card when capabilities are present:

```go
t.OnAgentInfo = func(data json.RawMessage) {
    // Existing: upsert agent_info
    var info db.AgentInfo
    if err := json.Unmarshal(data, &info); err != nil { ... }
    info.SandboxID = sandboxID
    s.DB.UpsertAgentInfo(&info)

    // New: if capabilities present, build and upsert agent card.
    // We also need agent_info for hardware summary, which we just stored above.
    var parsed struct {
        Capabilities *capabilitiesPayload `json:"capabilities"`
    }
    if err := json.Unmarshal(data, &parsed); err == nil && parsed.Capabilities != nil {
        cardJSON := buildCardJSON(parsed.Capabilities, &info)
        s.DB.UpsertAgentCardFromCapabilities(sandboxID, sbx.WorkspaceID, sbx.Name, cardJSON)
    }
}
```

Note: `sbx` (the sandbox record) is already available in the `handleTunnel` scope — it was loaded for token validation. We pass `sbx.WorkspaceID` and `sbx.Name` so the upsert can create the row if it doesn't exist.

**New DB method:** `internal/db/agent_cards.go`

Uses `INSERT ... ON CONFLICT` (upsert) instead of plain `UPDATE`, so it works for both:
- claudecode agents (where `RegisterDefaultCard` previously created the row)
- opencode agents (where no card row existed)

```go
func (db *DB) UpsertAgentCardFromCapabilities(sandboxID, workspaceID, displayName string, cardJSON json.RawMessage) error {
    _, err := db.Exec(
        `INSERT INTO agent_cards (sandbox_id, workspace_id, agent_type, display_name, card_json, agent_status, version)
         VALUES ($1, $2, 'claudecode', $3, $4, 'available', 1)
         ON CONFLICT (sandbox_id) DO UPDATE SET
           card_json = EXCLUDED.card_json,
           agent_status = 'available',
           version = agent_cards.version + 1,
           updated_at = NOW()`,
        sandboxID, workspaceID, displayName, cardJSON,
    )
    return err
}
```

### 5. API Response

The existing endpoints already return `card_json`:

- `GET /api/workspaces/{wid}/agents` — returns `card` field per agent
- `GET /api/agents/{sandboxId}` — returns full card

No API changes needed. The card now contains real data instead of `{}`.

### 6. MCP Bridge Listing Update

**Modified file:** `internal/mcpbridge/listing.go`

Update `DiscoveredAgent.Card` to parse the new structure:

```go
type DiscoveredAgent struct {
    // ... existing fields ...
    Card struct {
        Tags      []string `json:"tags"`
        Skills    []struct {
            Name        string `json:"name"`
            Description string `json:"description"`
        } `json:"skills"`
        Languages []struct {
            Name    string `json:"name"`
            Version string `json:"version"`
        } `json:"languages"`
        Tools []struct {
            Name    string `json:"name"`
            Version string `json:"version"`
        } `json:"tools"`
        Hardware map[string]any `json:"hardware"`
    } `json:"card"`
}
```

Update `FormatForToolDescription` to include richer info:

```go
// Before:
// - MacBook Air (sandbox-abc): [code, terminal] — available

// After:
// - MacBook Air (sandbox-abc): Go 1.22, Python 3.12, Docker [go, python, docker] — available
//   Hardware: 8 cores / 16GB / 256GB SSD
```

### 7. Display in Chat Interface

The chat interface that formats agent listings (likely in the LLM system prompt or a formatting function) should render the categorized format:

```
MacBook Air 🆕
• ID: 1d6b3b37...
• 状态: ✅ 在线可用
• 语言: Go 1.22, Python 3.12, Node 20
• 工具: Docker, Git, Make
• 硬件: 8核 / 16GB / 256GB SSD
```

This is a presentation-layer change in whatever code formats the agent listing for the user. The data is all available from the `GET /api/workspaces/{wid}/agents` response.

## Data Flow

```
Agent Machine                           Server
─────────────                           ──────

1. Connect via WebSocket
   └─ yamux session established

2. First heartbeat (immediate):
   ├─ collectAgentInfo()        →  gopsutil hardware data
   ├─ ProbeCapabilities()       →  exec "go version", "docker --version", ...
   │   ├─ goroutine: go (2s timeout)
   │   ├─ goroutine: python3 (2s timeout)
   │   ├─ goroutine: docker (2s timeout)
   │   └─ ... (all concurrent, ~2-3s total)
   ├─ Cache capabilities
   └─ Send AgentInfoData{..., Capabilities: {...}}
       via control stream                ──→  OnAgentInfo callback
                                              ├─ UpsertAgentInfo (agent_info table)
                                              └─ if capabilities != nil:
                                                  ├─ buildCardJSON(caps, agentInfo)
                                                  └─ UpsertAgentCardFromCapabilities
                                                     (agent_cards — INSERT or UPDATE)

3. Subsequent heartbeats (every 20s):
   ├─ collectAgentInfo()        →  fresh hardware data
   ├─ Use cached capabilities   →  no re-probe
   └─ Send AgentInfoData{..., Capabilities: {...}}
       via control stream                ──→  Same flow (capabilities unchanged)

4. After 1 hour:
   ├─ collectAgentInfo()
   ├─ ProbeCapabilities()       →  re-probe (detect new installs)
   ├─ Update cache
   └─ Send AgentInfoData{..., Capabilities: {...}}
       via control stream                ──→  Card updated with new capabilities
```

## Files to Modify

| File | Change |
|------|--------|
| `internal/agent/capabilities.go` | **New.** Probe registry, `ProbeCapabilities()`, GPU detection |
| `internal/agent/sysinfo.go` | Add `Capabilities *AgentCapabilities` field to `AgentInfoData` |
| `internal/agent/client.go` | Add capability caching to `Client`, integrate into `sendAgentInfo()` |
| `internal/agent/task_worker.go` | Remove `RegisterDefaultCard()` function |
| `internal/agent/claudecode.go` | Remove `RegisterDefaultCard()` call at line 182 |
| `internal/db/migrations/017_agent_capabilities.sql` | **New.** `ALTER TABLE agent_info ADD COLUMN capabilities JSONB` |
| `internal/db/agent_info.go` | Add `Capabilities` field, update `UpsertAgentInfo`/`GetAgentInfo` |
| `internal/db/agent_cards.go` | Add `UpsertAgentCardFromCapabilities()` method (upsert, not update) |
| `internal/sandboxproxy/capabilities.go` | **New.** `buildCardJSON`, `buildHardwareSummary`, `buildSkills/Tags`, local mirror types |
| `internal/sandboxproxy/tunnel.go` | Extend `OnAgentInfo` to sync capabilities → agent card |
| `internal/mcpbridge/listing.go` | Parse new card fields, update `FormatForToolDescription` |

## Testing

- **Unit tests for parsers:** Each version parser tested with real command output samples
- **Unit test for `ProbeCapabilities`:** Mock exec to return known outputs, verify struct
- **Integration test:** Agent connects → server receives capabilities → card is populated
- **Edge cases:** Missing commands (probe skipped), slow commands (timeout), empty results

## Out of Scope

- Project context analysis (what repos/tech stacks are in the working directory) — future phase
- Custom user-defined capabilities — future phase
- Capability-based task routing (using capabilities to auto-select which agent gets a task) — the data is available for this, but the routing logic is not part of this design
