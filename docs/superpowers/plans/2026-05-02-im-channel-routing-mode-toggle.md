# IM Channel Routing Mode Toggle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 workspace 管理员能在 web UI 上把某个 IM channel 的 `routing_mode` 在 `nanoclaw` 与 `stateless_cc` 之间切换，立即生效（下一条 IM 消息就按新路由走），无需重启服务或执行 SQL。

**Architecture:** 复用现有 `PATCH /api/workspaces/{wid}/im/channels/{id}` 路径；镜像 `require_mention` 的 in-memory 模式 —— DB 写 `workspace_im_channels.routing_mode` 列，同时在 Bridge 里更新 `channelRouting` 内存 map，让 `forwardMessage` 先查 map 再回落到初始 `binding.RoutingMode`。

**Tech Stack:** Go 1.22+ (chi router), React 19 + TypeScript, PostgreSQL (migration 018 已建列，无 schema 变更)。

**Testing strategy note:** `internal/imbridgesvc/` 与 `internal/db/` 目前没有任何测试基础设施（grep `_test.go` 为空）。本 plan 只在 `internal/imbridge/` 补一份单测（与现有 `matrix_api_test.go` 风格对齐），DB 函数与 handler 的行为通过手动 smoke test 覆盖。这和 repo 现有风格一致，避免本次 PR 引入整套 mock infra。

**Spec:** `docs/superpowers/specs/2026-05-02-im-channel-routing-mode-toggle-design.md`

---

## File Structure

| 操作 | 路径 | 职责 |
|------|------|------|
| Create | `internal/imbridge/bridge_routing_test.go` | Bridge in-memory routing map 的单测（并发 Set/Get + forwardMessage 走向） |
| Modify | `internal/db/im_channels.go` | 新增 `UpdateIMChannelRoutingMode` 函数 |
| Modify | `internal/imbridge/bridge.go` | Bridge struct 加 `channelRouting` map；`NewBridge` 初始化；`SetChannelRoutingMode`/`getChannelRoutingMode` 方法；`StartPoller` seed map；`forwardMessage` 先查 map |
| Modify | `internal/imbridgesvc/handlers.go` | `handleUpdateWorkspaceIMChannel` 的 req struct 增加 `RoutingMode *string` 字段 + 校验 + 双写 |
| Modify | `web/src/lib/api.ts` | `IMChannel` interface 加 `routing_mode`；`updateWorkspaceIMChannel` 的 settings 参数加 `routing_mode` |
| Modify | `web/src/components/WorkspaceDetail.tsx` | channel 行内 `require_mention` 旁加 `<select>` 控件 |

---

## Phase 1 — DB Layer

### Task 1.1: 新增 `UpdateIMChannelRoutingMode` 函数

**Files:**
- Modify: `internal/db/im_channels.go` (追加到文件末尾；紧邻现有 `UpdateIMChannelSettings` 后更易维护)

- [ ] **Step 1: 打开 `internal/db/im_channels.go`，在 `UpdateIMChannelSettings` 函数 (~line 175) 正下方追加**

```go
// UpdateIMChannelRoutingMode updates the routing_mode column for a channel.
// Caller is expected to validate `mode` before calling (valid values:
// "nanoclaw", "stateless_cc"). Unknown values are accepted by the DB but
// will cause forwardMessage to fall through to the default nanoclaw branch.
func (db *DB) UpdateIMChannelRoutingMode(channelID, mode string) error {
	_, err := db.Exec(
		`UPDATE workspace_im_channels SET routing_mode = $1 WHERE id = $2`,
		mode, channelID,
	)
	return err
}
```

- [ ] **Step 2: 编译验证**

Run: `cd /root/agentserver && go build ./internal/db/...`
Expected: 0 errors.

- [ ] **Step 3: Commit**

```bash
git add internal/db/im_channels.go
git commit -m "feat(db): add UpdateIMChannelRoutingMode

For the upcoming routing_mode toggle UI. Kept as a standalone setter
(separate from UpdateIMChannelSettings) so each mutable column has a
clear-purpose function."
```

---

## Phase 2 — Bridge In-Memory State + forwardMessage

This phase is TDD: we write the `forwardMessage` behaviour test first (red), then change the Bridge code to make it pass (green).

### Task 2.1: 写 failing 单测

**Files:**
- Create: `internal/imbridge/bridge_routing_test.go`

- [ ] **Step 1: 新建文件并写入下面内容**

Path: `internal/imbridge/bridge_routing_test.go`

```go
package imbridge

import (
	"context"
	"sync"
	"testing"
)

// TestForwardMessageChannelRoutingOverridesBinding verifies that
// SetChannelRoutingMode's in-memory value wins over the initial
// binding.RoutingMode captured at StartPoller time. This is the
// core property that makes the toggle take effect without restarting
// the poller.
func TestForwardMessageChannelRoutingOverridesBinding(t *testing.T) {
	b := &Bridge{
		providers:        map[string]Provider{},
		pollers:          map[string]context.CancelFunc{},
		registeredGroups: map[string]string{},
		channelMention:   map[string]bool{},
		channelRouting:   map[string]string{},
		typingSessions:   map[string]func(){},
	}

	// Override with stateless_cc in the in-memory map.
	b.SetChannelRoutingMode("ch-abc", "stateless_cc")

	// Simulate forwardMessage's routing decision directly. We cannot
	// invoke forwardMessage end-to-end here without a real provider /
	// HTTP target, so we assert on the effective mode computation.
	got := b.getChannelRoutingMode("ch-abc")
	if got != "stateless_cc" {
		t.Fatalf("expected in-memory routing=stateless_cc, got %q", got)
	}

	// Missing channel → empty string so forwardMessage falls back to
	// binding.RoutingMode.
	if b.getChannelRoutingMode("unknown") != "" {
		t.Fatalf("expected empty routing for unknown channel")
	}
}

// TestSetChannelRoutingModeConcurrent ensures the setter/getter
// are safe under concurrent access (mirrors SetChannelRequireMention
// concurrency assumptions).
func TestSetChannelRoutingModeConcurrent(t *testing.T) {
	b := &Bridge{
		providers:        map[string]Provider{},
		pollers:          map[string]context.CancelFunc{},
		registeredGroups: map[string]string{},
		channelMention:   map[string]bool{},
		channelRouting:   map[string]string{},
		typingSessions:   map[string]func(){},
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.SetChannelRoutingMode("ch1", "stateless_cc")
		}()
		go func() {
			defer wg.Done()
			_ = b.getChannelRoutingMode("ch1")
		}()
	}
	wg.Wait()

	if b.getChannelRoutingMode("ch1") != "stateless_cc" {
		t.Fatalf("expected stateless_cc after concurrent writes")
	}
}
```

- [ ] **Step 2: 运行测试验证 FAIL**

Run: `cd /root/agentserver && go test -run TestForwardMessageChannelRoutingOverridesBinding -v ./internal/imbridge/`
Expected: 编译失败，错误大意是 `undefined: channelRouting` / `undefined: SetChannelRoutingMode` / `undefined: getChannelRoutingMode`（也可能是 `Bridge struct has no field channelRouting`）。这是预期的 — 说明测试和目标代码对齐。

---

### Task 2.2: 给 `Bridge` 加 `channelRouting` 字段 + `NewBridge` 初始化

**Files:**
- Modify: `internal/imbridge/bridge.go` (Bridge struct at ~line 56, NewBridge at ~line 71)

- [ ] **Step 1: 在 `Bridge` struct 里 `channelMention` 行下方新增字段**

把 `internal/imbridge/bridge.go` 里（约 line 64）：

```go
	channelMention   map[string]bool               // key: channelID → require_mention setting
	typingSessions   map[string]func()             // key: "channelID:userID" → cancel func
```

改为：

```go
	channelMention   map[string]bool               // key: channelID → require_mention setting
	channelRouting   map[string]string             // key: channelID → routing_mode (runtime override of binding)
	typingSessions   map[string]func()             // key: "channelID:userID" → cancel func
```

- [ ] **Step 2: 在 `NewBridge` 的 return literal 里加 map 初始化**

约 line 85。把：

```go
		channelMention:   make(map[string]bool),
		typingSessions:   make(map[string]func()),
```

改为：

```go
		channelMention:   make(map[string]bool),
		channelRouting:   make(map[string]string),
		typingSessions:   make(map[string]func()),
```

- [ ] **Step 3: 编译验证**

Run: `cd /root/agentserver && go build ./internal/imbridge/...`
Expected: 0 errors (tests 仍然会失败，因为 Set/Get 方法还没加)。

---

### Task 2.3: 加 `SetChannelRoutingMode` / `getChannelRoutingMode` 方法

**Files:**
- Modify: `internal/imbridge/bridge.go` (紧随 `getChannelRequireMention` 之后插入，约 line 210)

- [ ] **Step 1: 在 `getChannelRequireMention` 结束位置下方追加**

约 line 209（`return v` 后的大括号后）。插入：

```go
// SetChannelRoutingMode updates the in-memory routing_mode for a channel.
// The value takes precedence over the routing_mode captured in
// BridgeBinding at StartPoller time, so a configuration change applied
// via this setter is visible on the next inbound message.
func (b *Bridge) SetChannelRoutingMode(channelID, mode string) {
	b.mu.Lock()
	b.channelRouting[channelID] = mode
	b.mu.Unlock()
}

// getChannelRoutingMode reads the in-memory routing_mode. Returns ""
// if the channel has no override — callers fall back to
// BridgeBinding.RoutingMode in that case.
func (b *Bridge) getChannelRoutingMode(channelID string) string {
	b.mu.Lock()
	v := b.channelRouting[channelID]
	b.mu.Unlock()
	return v
}
```

- [ ] **Step 2: 跑测试验证先前失败的单测现在通过**

Run: `cd /root/agentserver && go test -run 'TestForwardMessageChannelRoutingOverridesBinding|TestSetChannelRoutingModeConcurrent' -race -v ./internal/imbridge/`
Expected: 两个测试都 PASS；`-race` 无 data race 报告。

---

### Task 2.4: 让 `forwardMessage` 先查内存 map 再回落 binding

**Files:**
- Modify: `internal/imbridge/bridge.go` (~line 319)

- [ ] **Step 1: 打开 bridge.go，把 `forwardMessage` 函数整体改成**

原函数（约 line 316-325）：

```go
func (b *Bridge) forwardMessage(ctx context.Context, binding BridgeBinding, msg InboundMessage) (bool, error) {
	switch binding.RoutingMode {
	case "stateless_cc":
		return b.forwardToAgentserver(ctx, binding, msg)
	default: // "nanoclaw" or empty (backward compatible)
		return b.forwardToNanoClaw(ctx, binding, msg)
	}
}
```

改为：

```go
func (b *Bridge) forwardMessage(ctx context.Context, binding BridgeBinding, msg InboundMessage) (bool, error) {
	// In-memory routing mode (set via SetChannelRoutingMode) wins over
	// the routing_mode captured at StartPoller time. Empty map value
	// means no override — fall through to binding.RoutingMode.
	mode := b.getChannelRoutingMode(binding.ChannelID)
	if mode == "" {
		mode = binding.RoutingMode
	}
	switch mode {
	case "stateless_cc":
		return b.forwardToAgentserver(ctx, binding, msg)
	default: // "nanoclaw" or empty (backward compatible)
		return b.forwardToNanoClaw(ctx, binding, msg)
	}
}
```

- [ ] **Step 2: 编译验证 + 跑全量 imbridge 包测试**

Run: `cd /root/agentserver && go test -race ./internal/imbridge/...`
Expected: all PASS。

---

### Task 2.5: `StartPoller` 内 seed map

**Files:**
- Modify: `internal/imbridge/bridge.go` (~line 108)

目的：一个刚启动的 poller 会把 `binding.RoutingMode` 写进 map，这样后续 PATCH 调 `SetChannelRoutingMode` 覆盖值也是从一个已知的基线出发（便于排查：`channelRouting[channelID]` 始终等于"当前生效的值"）。

- [ ] **Step 1: 打开 bridge.go，在 `StartPoller` 里 `go b.pollLoop(ctx, binding)` 之前加 seed 语句**

原函数（约 line 108-122）：

```go
func (b *Bridge) StartPoller(binding BridgeBinding) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := binding.ChannelID
	if cancel, ok := b.pollers[key]; ok {
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.pollers[key] = cancel

	go b.pollLoop(ctx, binding)
}
```

改为：

```go
func (b *Bridge) StartPoller(binding BridgeBinding) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := binding.ChannelID
	if cancel, ok := b.pollers[key]; ok {
		cancel()
	}

	// Seed the in-memory routing map so getChannelRoutingMode returns
	// the value that forwardMessage would use by default. Later calls
	// to SetChannelRoutingMode override this seeded value.
	b.channelRouting[key] = binding.RoutingMode

	ctx, cancel := context.WithCancel(context.Background())
	b.pollers[key] = cancel

	go b.pollLoop(ctx, binding)
}
```

- [ ] **Step 2: 跑测试**

Run: `cd /root/agentserver && go test -race ./internal/imbridge/...`
Expected: all PASS。

---

### Task 2.6: Commit Phase 2

- [ ] **Step 1: commit**

```bash
git add internal/imbridge/bridge.go internal/imbridge/bridge_routing_test.go
git commit -m "feat(imbridge): runtime routing_mode override via channelRouting map

Mirror the require_mention pattern: Bridge now keeps an in-memory
channelRouting map, updated via SetChannelRoutingMode. forwardMessage
consults the map first and falls back to BridgeBinding.RoutingMode
when the map has no entry. StartPoller seeds the map so the override
always reflects the currently effective value.

Enables the upcoming routing_mode toggle API to take effect without
restarting the long-poller."
```

---

## Phase 3 — imbridgesvc Handler

### Task 3.1: 扩展 `handleUpdateWorkspaceIMChannel`

**Files:**
- Modify: `internal/imbridgesvc/handlers.go` (~line 898 `handleUpdateWorkspaceIMChannel`)

- [ ] **Step 1: 打开 handlers.go，把 `handleUpdateWorkspaceIMChannel` 函数整体改成**

原函数（约 line 898-932）：

```go
func (s *Server) handleUpdateWorkspaceIMChannel(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	channelID := chi.URLParam(r, "channelId")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	ch, err := s.db.GetIMChannel(channelID)
	if err != nil || ch.WorkspaceID != wsID {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	var req struct {
		RequireMention *bool `json:"require_mention"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.RequireMention != nil {
		if err := s.db.UpdateIMChannelSettings(channelID, *req.RequireMention); err != nil {
			http.Error(w, "failed to update channel", http.StatusInternalServerError)
			return
		}
		s.bridge.SetChannelRequireMention(channelID, *req.RequireMention)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}
```

改为：

```go
func (s *Server) handleUpdateWorkspaceIMChannel(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	channelID := chi.URLParam(r, "channelId")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	ch, err := s.db.GetIMChannel(channelID)
	if err != nil || ch.WorkspaceID != wsID {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	var req struct {
		RequireMention *bool   `json:"require_mention"`
		RoutingMode    *string `json:"routing_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.RequireMention != nil {
		if err := s.db.UpdateIMChannelSettings(channelID, *req.RequireMention); err != nil {
			http.Error(w, "failed to update channel", http.StatusInternalServerError)
			return
		}
		s.bridge.SetChannelRequireMention(channelID, *req.RequireMention)
	}

	if req.RoutingMode != nil {
		mode := *req.RoutingMode
		if mode != "nanoclaw" && mode != "stateless_cc" {
			http.Error(w, "invalid routing_mode", http.StatusBadRequest)
			return
		}
		if err := s.db.UpdateIMChannelRoutingMode(channelID, mode); err != nil {
			http.Error(w, "failed to update channel", http.StatusInternalServerError)
			return
		}
		s.bridge.SetChannelRoutingMode(channelID, mode)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}
```

- [ ] **Step 2: 编译验证**

Run: `cd /root/agentserver && go build ./internal/imbridgesvc/...`
Expected: 0 errors。

- [ ] **Step 3: 跑全量 Go 测试确保无回归**

Run: `cd /root/agentserver && go test -race ./...`
Expected: all PASS (关注 `imbridge` 和 `imbridgesvc` 包；其他包不受影响)。

- [ ] **Step 4: Commit**

```bash
git add internal/imbridgesvc/handlers.go
git commit -m "feat(imbridgesvc): accept routing_mode in PATCH IM channel

handleUpdateWorkspaceIMChannel now accepts {require_mention?,
routing_mode?}. routing_mode is validated against {nanoclaw,
stateless_cc}; invalid values return 400. On success we write both
the DB column and the Bridge in-memory map so the change takes effect
on the next inbound message."
```

---

### Task 3.2: 扩展 list handler 的 `channelResp` JSON 结构

**Files:**
- Modify: `internal/imbridgesvc/handlers.go` (~line 848 `handleListWorkspaceIMChannels`)

Without this change, `GET /api/workspaces/{wid}/im/channels` omits `routing_mode` from the response and the frontend `<select>` falls back to `nanoclaw` on every refetch, making the Phase 4 toggle appear broken.

- [ ] **Step 1: Extend `channelResp` struct to include `RoutingMode`**

把：

```go
	type channelResp struct {
		ID             string `json:"id"`
		Provider       string `json:"provider"`
		BotID          string `json:"bot_id"`
		UserID         string `json:"user_id,omitempty"`
		RequireMention bool   `json:"require_mention"`
		BoundAt        string `json:"bound_at"`
	}
```

改为：

```go
	type channelResp struct {
		ID             string `json:"id"`
		Provider       string `json:"provider"`
		BotID          string `json:"bot_id"`
		UserID         string `json:"user_id,omitempty"`
		RequireMention bool   `json:"require_mention"`
		RoutingMode    string `json:"routing_mode"`
		BoundAt        string `json:"bound_at"`
	}
```

并在其下方 for-loop 的 `resp = append(resp, channelResp{...})` 里添加 `RoutingMode: ch.RoutingMode,`。

- [ ] **Step 2: 编译验证**

`cd /root/agentserver && go build ./internal/imbridgesvc/...` — 0 errors.

- [ ] **Step 3: Commit**（可和 Task 3.1 合并为同一个 commit，或独立 commit 均可）

---

## Phase 4 — Frontend

### Task 4.1: 扩展 `IMChannel` 接口 + `updateWorkspaceIMChannel` 参数

**Files:**
- Modify: `web/src/lib/api.ts` (~line 444, 460)

- [ ] **Step 1: 扩展 `IMChannel` interface**

约 line 444。把：

```ts
export interface IMChannel {
  id: string
  workspace_id: string
  provider: string
  bot_id: string
  user_id: string
  require_mention: boolean
  bound_at: string
}
```

改为：

```ts
export interface IMChannel {
  id: string
  workspace_id: string
  provider: string
  bot_id: string
  user_id: string
  require_mention: boolean
  routing_mode: string
  bound_at: string
}
```

- [ ] **Step 2: 扩展 `updateWorkspaceIMChannel` 的 settings 参数**

约 line 460。把：

```ts
export async function updateWorkspaceIMChannel(workspaceId: string, channelId: string, settings: { require_mention?: boolean }): Promise<void> {
```

改为：

```ts
export async function updateWorkspaceIMChannel(
  workspaceId: string,
  channelId: string,
  settings: { require_mention?: boolean; routing_mode?: 'nanoclaw' | 'stateless_cc' },
): Promise<void> {
```

（函数体不变。）

- [ ] **Step 3: TypeScript 编译验证**

Run: `cd /root/agentserver/web && pnpm tsc --noEmit`
Expected: 0 errors.

---

### Task 4.2: 在 `WorkspaceDetail.tsx` 的 channel 行加 `<select>`

**Files:**
- Modify: `web/src/components/WorkspaceDetail.tsx` (~line 744，在 require_mention `<label>` 之前)

注意：此处没有已成型的 `<Select>` 组件库（repo 未引入 shadcn Select；现有 UI 控件基于原生 `<input>`/`<button>` + Tailwind），沿用原生 `<select>` 保持风格一致。

- [ ] **Step 1: 打开 WorkspaceDetail.tsx，定位到现有 require_mention `<label>` (~line 741)**

该段当前形如：

```tsx
                  <div className="flex items-center gap-2">
                    <label className="flex items-center gap-1.5 text-[11px] text-[var(--muted-foreground)] cursor-pointer" title="Only reply when @mentioned in group chats">
                      <input
                        type="checkbox"
                        checked={ch.require_mention}
                        onChange={async (e) => {
                          try {
                            await updateWorkspaceIMChannel(workspaceId, ch.id, { require_mention: e.target.checked })
                            loadChannels()
                          } catch {}
                        }}
                        className="rounded"
                      />
                      @mention
                    </label>
                    <button
                      onClick={() => setConfirmDeleteChannel(ch)}
```

在 `<label>` **之前**插入一个 `<select>`：

```tsx
                  <div className="flex items-center gap-2">
                    <select
                      value={ch.routing_mode || 'nanoclaw'}
                      onChange={async (e) => {
                        try {
                          await updateWorkspaceIMChannel(workspaceId, ch.id, {
                            routing_mode: e.target.value as 'nanoclaw' | 'stateless_cc',
                          })
                          loadChannels()
                        } catch {}
                      }}
                      className="rounded border border-[var(--border)] bg-[var(--background)] px-1.5 py-0.5 text-[11px] text-[var(--foreground)]"
                      title="Routing mode: nanoclaw = legacy NanoClaw sandbox; stateless_cc = new stateless Claude Code broker (Beta)"
                    >
                      <option value="nanoclaw">nanoclaw</option>
                      <option value="stateless_cc">stateless_cc (Beta)</option>
                    </select>
                    <label className="flex items-center gap-1.5 text-[11px] text-[var(--muted-foreground)] cursor-pointer" title="Only reply when @mentioned in group chats">
```

（`||  'nanoclaw'` 兼容旧后端返回 `undefined` 的情况；其余代码不变。）

- [ ] **Step 2: 前端类型检查 + 打包**

Run: `cd /root/agentserver/web && pnpm tsc --noEmit && pnpm build`
Expected: TypeScript 0 errors；`pnpm build` 成功产生 dist。

- [ ] **Step 3: Commit Phase 4**

```bash
git add web/src/lib/api.ts web/src/components/WorkspaceDetail.tsx
git commit -m "feat(web): IM channel routing_mode toggle

Add routing_mode to IMChannel and updateWorkspaceIMChannel settings.
Render a native <select> in the workspace IM channel row with
nanoclaw / stateless_cc (Beta) options. onChange PATCHes the backend
and refetches the channel list."
```

---

## Phase 5 — Deploy & Smoke Test

### Task 5.1: 推分支 + CI 构建

- [ ] **Step 1: 推分支，触发 CI**

```bash
git push -u origin HEAD
```

- [ ] **Step 2: 等 `agentserver` / `imbridge` 镜像 CI 跑完**

检查 `.gitlab-ci.yml` 相关 job 成功；新的 `:main` 或 PR tag 镜像已推到 `registry.nj.cs.ac.cn/ghcr/agentserver/`.

---

### Task 5.2: 部署到 k8s

- [ ] **Step 1: 触发两个 Deployment 的 rollout**

```bash
kubectl -n agentserver rollout restart deploy/agentserver
kubectl -n agentserver rollout restart deploy/agentserver-imbridge
```

- [ ] **Step 2: 等 Pod 就绪**

```bash
kubectl -n agentserver rollout status deploy/agentserver --timeout=2m
kubectl -n agentserver rollout status deploy/agentserver-imbridge --timeout=2m
```

Expected: 两个 rollout 都 `successfully rolled out`。

---

### Task 5.3: API smoke test — 合法切换

- [ ] **Step 1: 选一个测试 channel 并记下 ID**

```bash
PG=$(kubectl -n agentserver get secret agentserver-secret -o jsonpath='{.data.database-url}' | base64 -d)
kubectl -n agentserver exec agentserver-postgresql-0 -- \
  psql "$PG" -c "SELECT id, workspace_id, provider, bot_id, routing_mode FROM workspace_im_channels LIMIT 5;"
```

Expected: 能看到多行，挑一个 provider/bot_id 便于后面对照。记下 `WID` 和 `CID`。

- [ ] **Step 2: PATCH routing_mode 到 stateless_cc**

需要携带用户 session cookie（通过浏览器登录后 F12 → Application → Cookies 拷贝 `session=...`），或在 kubectl 端内部 pod 里直接调。最简单是从浏览器的 devtools Network 面板 copy curl。

内部 pod 内部调（无需 auth，直接通过 imbridge svc）示例：

```bash
kubectl -n agentserver run -i --rm curl-patch --image=curlimages/curl:latest --restart=Never --quiet -- \
  curl -s -X PATCH \
    "http://agentserver-imbridge.agentserver.svc:8083/api/workspaces/<WID>/im/channels/<CID>" \
    -H 'Content-Type: application/json' \
    -H 'X-Internal-Secret: <INTERNAL_API_SECRET from env>' \
    -d '{"routing_mode":"stateless_cc"}'
```

> Note: 直接打 imbridge svc 绕过了 agentserver 的 session auth，仅供内网 smoke test。真实 UI 场景通过 agentserver 代理已带好 cookie。

Expected: `{"status":"updated"}`。

- [ ] **Step 3: 验证 DB 生效**

```bash
kubectl -n agentserver exec agentserver-postgresql-0 -- \
  psql "$PG" -c "SELECT id, routing_mode FROM workspace_im_channels WHERE id = '<CID>';"
```

Expected: `routing_mode = stateless_cc`。

- [ ] **Step 4: 验证 imbridge 日志里下一条消息走 stateless**

```bash
kubectl -n agentserver logs deploy/agentserver-imbridge --tail=200 -f
```

然后从对应 IM 向 bot 发一条消息。
Expected: 日志里出现 `forward to agentserver` 相关条目（或 `forwardToAgentserver`），且 agentserver 也会 log 对应的 `/im/inbound` 请求。

同时可以查 cc-broker 的 session 表确认事件入库：

```bash
kubectl -n agentserver exec agentserver-postgresql-0 -- \
  psql "${PG%/*}/ccbroker" -c "SELECT id, workspace_id, source FROM agent_sessions ORDER BY created_at DESC LIMIT 3;"
```

Expected: 看到一条新 session（`source='weixin'` 或类似）。

---

### Task 5.4: API smoke test — 非法值返回 400

- [ ] **Step 1: 发非法 routing_mode**

```bash
kubectl -n agentserver run -i --rm curl-bad --image=curlimages/curl:latest --restart=Never --quiet -- \
  curl -s -o /dev/null -w '%{http_code}\n' -X PATCH \
    "http://agentserver-imbridge.agentserver.svc:8083/api/workspaces/<WID>/im/channels/<CID>" \
    -H 'Content-Type: application/json' \
    -d '{"routing_mode":"bogus"}'
```

Expected: `400`.

- [ ] **Step 2: 确认 DB 值未变**

```bash
kubectl -n agentserver exec agentserver-postgresql-0 -- \
  psql "$PG" -c "SELECT routing_mode FROM workspace_im_channels WHERE id = '<CID>';"
```

Expected: 仍是上一步设的 `stateless_cc`，未被非法请求改动。

---

### Task 5.5: API smoke test — 回切到 nanoclaw

- [ ] **Step 1: PATCH 切回**

```bash
kubectl -n agentserver run -i --rm curl-back --image=curlimages/curl:latest --restart=Never --quiet -- \
  curl -s -X PATCH \
    "http://agentserver-imbridge.agentserver.svc:8083/api/workspaces/<WID>/im/channels/<CID>" \
    -H 'Content-Type: application/json' \
    -d '{"routing_mode":"nanoclaw"}'
```

Expected: `{"status":"updated"}`.

- [ ] **Step 2: 再发一条 IM 消息，确认落到 NanoClaw 路径**

观察 imbridge 日志没有 `forwardToAgentserver`，改为原本的 NanoClaw pod 转发路径。

---

### Task 5.6: UI smoke test（可选但推荐）

- [ ] **Step 1: 打开前端 Workspace 详情页**

浏览器访问 `https://agent.cs.ac.cn/workspaces/<WID>`，定位到 IM channels 区块。

- [ ] **Step 2: 看到 routing_mode 下拉**

Expected: 每个 channel 旁有一个 `<select>`，显示当前值。

- [ ] **Step 3: 切换并验证**

选 `stateless_cc (Beta)` → 刷新后值保持为 stateless_cc。切回 nanoclaw → 值保持 nanoclaw。

---

## Rollback

如果上线后发现问题：

1. **快速回滚 routing_mode 数据**：
   ```sql
   UPDATE workspace_im_channels SET routing_mode = 'nanoclaw' WHERE routing_mode = 'stateless_cc';
   ```
   并 `kubectl -n agentserver rollout restart deploy/agentserver-imbridge` 让重启时的 `restoreAllPollers` 走 DB 最新值（虽然 in-memory seed 会覆盖）。

2. **回滚代码**：`git revert` 相关 commit，`git push`，等 CI 构建，`kubectl rollout restart` 两个 deployment。由于无 schema 变更，无需回滚 migration。

---

## Open Risks (from spec §7)

1. **Race on rapid toggle** — 可接受：DB + map 都是 last-write-wins，收敛一致。
2. **Poller 重建时的 seed** — `StartPoller` 现在会 seed `channelRouting`，重启 imbridge 后从 DB 读的 `binding.RoutingMode` 会被 seed 回内存，和 DB 一致。
3. **stateless_cc 下游未就绪** — 本 plan 不加 readiness check；用户切错会导致下一条 IM 无响应。由运维/smoke-test 阶段检测。
