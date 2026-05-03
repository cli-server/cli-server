# agentserver-agent `tui` 子命令设计

**Date:** 2026-05-03
**Status:** Draft
**Author:** mryao + Claude

**Related specs:**
- [`2026-04-15-stateless-cc-design.md`](2026-04-15-stateless-cc-design.md) — stateless cc 整体架构
- [`2026-05-02-ccbroker-sdk-worker-design.md`](2026-05-02-ccbroker-sdk-worker-design.md) — cc-broker SDK worker 模型
- [`2026-04-12-agent-capability-discovery-design.md`](2026-04-12-agent-capability-discovery-design.md) — agent 能力发现

---

## 1. 指导原则与目标

### 1.1 指导原则（贯穿整个方案）

> **脑子在远程。远程脑子能处理的事情，全部由远程处理。本地 TUI 是一个用户 I/O 界面。**

具象到边界划分：

| TUI 必须做 | TUI 必须不做 |
|---|---|
| 捕获键盘 / 终端尺寸变化 / 附件路径 / 中断信号 | 任何决策类逻辑（选模型、选 executor、何时 compact、给不给权限） |
| 路由用户输入到正确的远程端点（turn / session control / query） | 任何 harness 状态保存（模型偏好、permission mode、turn 历史） |
| 订阅 SSE 并把事件渲染成终端可视形态 | 自己解释 / 改写 / 过滤事件流的语义 |
| 本地手脚的进程内执行（cc-broker 调本机 `remote_*` 时） | 本地手脚的"脑子"（被调用顺序、是否调用全靠远程脑） |
| 本地 I/O 策略（要不要弹权限确认、要不要切 cwd、要不要 yolo） | 用户 prompt 的语义解读 |

### 1.2 Slash 命令的三类分流

按原则推导，所有 slash 命令落入下面三类之一：

| 类别 | 含义 | 例子 | 实现位置 |
|---|---|---|---|
| **L（Local I/O）** | 只关乎本地进程 / 本地手脚 / 本地输入策略 | `/quit`、`/cd <path>`、`/yolo`、`/attach <path>` | TUI 进程内 |
| **S（Session attach）** | 改变 TUI 当前订阅哪条 session 流 | `/clear`、`/resume <id>` | TUI 调 agentserver session API → 重订 SSE |
| **R（Remote brain）** | 凡是影响"脑子怎么想"的 | `/model <id>`、`/permission default\|bypass`、`/compact`、`/cost`、`/agents` | TUI 转 control 请求 POST 到 agentserver；agentserver 持久化为 session preference 或转发给 cc-broker |

**关键设计推论：** L 之外，TUI 不解释任何 slash 命令的语义。它只识别"这是个 R 类命令 → POST 到 `/api/agent-sessions/{sid}/control` 端点"。新增 R 类命令只需在 agentserver/cc-broker 增加 handler，TUI 零改动。

### 1.3 目标

给 `agentserver-agent` 二进制新增 `tui` 子命令。该命令在用户机器上启动一个**双角色进程**：

1. **手脚（Executor 角色）**：与 `executor` 子命令完全等价（向 executor-registry 注册 executor 身份、维护 yamux tunnel、暴露 `Bash/Read/Write/Edit/Glob/Grep/LS`）；唯一外观差异是 `display_name` 默认带 ` (interactive)` 后缀，方便 `/agents` 列表辨识。session 选哪个 executor 由 `preferred_executor_id` 严格控制（§6.6），不依赖任何 executor 侧 flag。
2. **远程 harness 的瘦客户端（TUI 角色）**：Bubble Tea 终端界面，按 §1.1 原则——纯 I/O。

### 1.4 非目标

- 不动 `connect` / `executor` / `mcp-server` 子命令（兼容性优先）。
- 不在本地实现任何 harness 决策（§1.1 直接推论）。
- 不引入 Node 运行时（Go 原生 Bubble Tea + lipgloss）。
- 不复用 ccr v2 bridge SSE worker 模型（cc-broker 已迁移到 SDK + stdio）。
- 不复刻 claude code TUI 的本地 harness UI 元素：onboarding / auto-updater / 本地 `~/.claude/projects/*.jsonl` 管理。

---

## 2. CLI 形态与服务拓扑

### 2.1 子命令矩阵

| 子命令 | 角色 | 是否新增 | 注册到 | tunnel | 本地有 UI |
|---|---|---|---|---|---|
| `connect` (`claudecode`) | 老 headless | 不动 | agent-registry (sandbox) | yamux + ccr v2 bridge | 否 |
| `executor` | 纯手脚 | 不动 | executor-registry | yamux | 否 |
| **`tui`** | **手脚 + 远程 harness 客户端** | **新增** | executor-registry + agentserver session | yamux（手脚）+ HTTPS+SSE（TUI） | **是** |
| `mcp-server` | 旧 MCP stdio bridge | 不动 | — | — | 否 |
| `login` / `list` / `remove` | 工具 | 不动 | — | — | — |

`tui` 与 `executor` 共享几乎全部代码（注册、tunnel、`executortools`、心跳、重连、re-register）；差异仅在：
- 默认 `--name` 带 `(interactive)` 后缀（仅 UI 辨识用，不影响路由）；
- 启动多一个 TUI 协程（Bubble Tea program）；
- 本地权限确认逻辑搬到 cc-broker（§6.4），本进程内零变化。

**为什么不在 executor 上打 `interactive=true` 标记？** 早期设计有过此考虑——让 LLM 知道哪个 executor 有 UI。但 `preferred_executor_id` 是更强的 session 级机制（绑 session、跟随 responder、直接进 system prompt），完全覆盖该需求；权限闸又搬到了 cc-broker，LLM 也不需要知道哪个有 UI。executor 侧 flag 因此是冗余的，删之。

### 2.2 `tui` 子命令 flag

```
agentserver tui [flags]
  --server URL              agentserver 地址（默认从 saved creds 取）
  --workspace-id ID         必需（与 executor 一致）
  --name NAME               显示名（默认 hostname + " (interactive)"）
  --work-dir PATH           手脚工作目录（默认 cwd）
  --resume SID              attach 已有 session（不传则新建）
  --continue, -c            attach 本机最近的 tui session
  --yolo                    启动即关闭权限确认（等同进入后 /yolo）
  --skip-open-browser       OAuth 走 QR/URL
  --model MODEL             首次 turn 的 sticky 模型（之后用 /model 改）
```

注册行为复用 `LoadOrRegisterExecutor`：第一次跑走 OAuth Device Flow（同 `executor`），credentials 落到 `~/.agentserver/`，之后无感重连。

### 2.3 服务拓扑

```
┌──────────────────────── User's Machine ────────────────────────┐
│                                                                 │
│  ┌──── agentserver tui (single OS process, three goroutines) ─┐ │
│  │                                                              │ │
│  │  ┌───────────┐   ┌─────────────┐   ┌──────────────────┐    │ │
│  │  │ Bubble    │   │ SSE consumer│   │ ExecutorClient   │    │ │
│  │  │ Tea TUI   │◄──┤ (HTTPS long │   │ (yamux tunnel,   │    │ │
│  │  │           │   │  poll)      │   │  心跳, 工具执行) │    │ │
│  │  └─────┬─────┘   └──────▲──────┘   └─────────▲────────┘    │ │
│  │        │ keypresses     │                    │              │ │
│  │        ▼                │                    │              │ │
│  │  ┌───────────┐          │                    │              │ │
│  │  │ HTTP cli  │          │                    │              │ │
│  │  │ (POST     │          │                    │              │ │
│  │  │  inbound, │          │                    │              │ │
│  │  │  control, │          │                    │              │ │
│  │  │  decide)  │          │                    │              │ │
│  │  └─────┬─────┘          │                    │              │ │
│  │        │                │                    │              │ │
│  │   零进程内通道           │                    │              │ │
│  └────────┼────────────────┼────────────────────┼──────────────┘ │
└───────────┼────────────────┼────────────────────┼────────────────┘
            │                │                    │
        HTTPS POST       HTTPS SSE             yamux/wss
            │                │                    │
            ▼                │                    │
   ┌────────────────────────────────┐    ┌──────────────────────┐
   │         agentserver            │    │  executor-registry   │
   │  ┌──────────────────────────┐  │    │                      │
   │  │ /tui/inbound             │  │    │ /api/execute ────────┼──┐
   │  │ /events (SSE)            │  │    │ (HTTP → yamux stream)│  │
   │  │ /control                 │  │    └──────────────────────┘  │
   │  │ /permissions/{pid}       │  │              ▲               │
   │  └─────────┬────────────────┘  │              │               │
   │            │                   │              │               │
   │   POST /turns (SSE)            │              │               │
   │   POST /permissions/decide     │              │               │
   └────────────┼───────────────────┘              │               │
                ▼                                  │               │
   ┌──────────────────────┐                       │               │
   │      cc-broker       │                       │               │
   │  ┌────────────────┐  │  permission gate      │               │
   │  │ tools/         │  │  in tool handler:     │               │
   │  │  permission.go │──┼──→ blocks until       │               │
   │  │ tools/         │  │     decision arrives  │               │
   │  │  executor.go   │──┼──→ then dispatches ──►│               │
   │  └────────────────┘  │                                       │
   └──────────────────────┘                                       │
                                                                  │
                          (tunnel stream from registry to local)──┘
```

**三条独立连接：**

| 连接 | 方向 | 用途 | 协议 |
|---|---|---|---|
| **C1** TUI → agentserver | 上行 | prompt、session control、permission decide | HTTPS |
| **C2** agentserver → TUI | 下行 | session events 流 | SSE |
| **C3** executor-registry ↔ TUI 进程的 Executor goroutine | 双向 | 心跳 + 工具下发；与 `executor` 子命令完全同型 | yamux over wss |

**关键解耦：** C2 和 C3 的"端"在同一个 OS 进程内，但**逻辑上互相独立**——任一断开不影响另一条。两边在进程内**没有任何共享通道**：TUI 的 Bubble Tea 程序与 ExecutorClient 完全不通信。即使 ExecutorClient 被攻击者劫持，也无法绕过 cc-broker 的权限闸。

### 2.4 为什么权限闸不在本地

如果权限闸在本地 executor 进程内（如设计早期版本），则：
1. 任何 executor 代码路径上的 bug 都可能被利用绕过权限。
2. 跨机器的 executor 无法用本地 TUI 授权（必须每台机器都跑一个 TUI）。
3. TUI 与 ExecutorClient 必须有进程内通道，违反 §1.1 原则。

把权限闸放到 cc-broker 解决这三个问题：
1. 闸不在被攻击的代码路径上——攻击者控住 executor 也只能等"被调"，决策在远端。
2. TUI 是 session 的 permission_responder，可以给 workspace 内**任何 executor**（包括另一台机器上的）授权。
3. TUI 与 ExecutorClient 进程内零耦合。

### 2.5 新增/修改组件清单

**Agent 侧（新增）：**
- `cmd/agentserver-agent/main.go`：`tuiCmd`（薄壳）。
- `internal/agent/tui.go`：`RunTUI(opts)` 入口。
- `internal/agent/tui/`（新包）：Bubble Tea models / views / messages / keymap / Bus。
- `internal/agent/executor_client.go`：无改动（`tui` 直接复用现有 `ExecutorClient`，仅 `--name` 默认值在 main.go 处理）。

**agentserver 侧（新增）：**
- `POST /api/workspaces/{wid}/tui/inbound` — TUI 上传 prompt（含 active_turn_id CAS）。
- `POST /api/agent-sessions` — 显式新建 session（`/clear`）。
- `POST /api/agent-sessions/{sid}/attach` — 重新认领 permission_responder。
- `GET  /api/agent-sessions/{sid}/events` — **新建** SSE 端点（用户视角）。底层 SSEBroker 复用 `internal/bridge/`，但 HTTP route 是新的，与已有的 worker SSE (`/v1/agent/sessions/{sid}/worker/events/stream`) 完全独立。
- `POST /api/agent-sessions/{sid}/control` — R 类 slash 命令路由。
- `POST /api/agent-sessions/{sid}/turns/{tid}/cancel` — 中断 turn（转发 cc-broker）。
- `POST /api/agent-sessions/{sid}/permissions/{pid}` — 权限决定（转发 cc-broker）。
- `GET  /api/agent-sessions?workspace_id=...&channel_type=tui&executor_id=...&latest=1` — `--continue` 用。
- `GET  /api/executors/{id}/status` — TUI 状态栏拉取（反代 executor-registry）。
- `POST /internal/sessions/{sid}/turn-finished` — cc-broker 内部回调，清 `active_turn_id`。
- 后台 leak worker：每 5min 扫描 `active_turn_id` 非空 + cc-broker 报无此 turn → 强制清；扫 `responder_attached_at < now() - ttl` → 清 responder。
- ⚠️ **不**新增 IMRouter / `tui` provider —— agentserver 当前没有 IMRouter 抽象（`send_message` 由 cc-broker 直接 POST imbridge）。TUI session 的 `send_*` 短路在 cc-broker 端处理（见下）。

**cc-broker 侧（新增）：**
- `internal/ccbroker/tools/permission.go` — 闸逻辑。
- `internal/ccbroker/tools/prompt.go` — system prompt builder。
- `handler_turns.go`：metadata 解析 + 透传 + `turn_kind=compaction` 分支 + turn-finished defer 回调。
- `runner/options.go`：metadata → SDK options 映射。
- `tools/executor.go`：每个 `remote_*` handler 入口套 `gate.Check`。
- `tools/im.go`：`send_message` / `send_image` / `send_file` 在 `tctx.IMChannelID == ""` 时短路。
- 新转发端点：`/api/sessions/{sid}/turns/{tid}/cancel`、`/api/sessions/{sid}/permissions/{pid}/decide`、`/api/sessions/{sid}/compact`、`GET /api/sessions/{sid}/turns/active`。

**改动量预估：**
- agent 侧 ~1200 行（TUI 占大头）；
- agentserver 侧 ~500 行；
- cc-broker 侧 ~300 行。

---

## 3. 端到端数据流时序

按 happy path + 关键变体展开。所有时序假设 TUI 已启动、executor 已注册、session 已 attach、SSE 流已建立。

### 3.1 Happy path: 用户输入一句话，CC 调一个本机工具

```
User    TUI(BubbleTea)  TUI(HTTPCli)   TUI(SSE)   agentserver    cc-broker    exec-registry   TUI(Executor)
 │           │              │             │            │              │              │              │
 │  type prompt + Enter     │             │            │              │              │              │
 ├──────────►│              │             │            │              │              │              │
 │           ├─────────────►│             │            │              │              │              │
 │           │              │  POST /tui/inbound                      │              │              │
 │           │              ├────────────────────────►│              │              │              │
 │           │              │             │            │  CAS active_turn_id                       │
 │           │              │◄─202 Accepted───────────┤              │              │              │
 │           │              │             │            │  POST /api/turns (SSE)                    │
 │           │              │             │            ├─────────────►│              │              │
 │           │              │             │            │              │ TurnLock                   │
 │           │              │             │            │              │ insert agent_session_events│
 │           │              │             │            │              │   (user msg, by cc-broker) │
 │           │              │             │            │              │ workspace.Setup            │
 │           │              │             │            │              │ agentsdk.NewClient         │
 │           │              │             │            │              │ Connect (spawn claude)     │
 │           │              │             │            │              │              │              │
 │           │              │             │            │  ◄─SSE─ tool_use{id=tu1, executor_id=self}│
 │           │              │             │  ◄─SSE─ broadcast tool_use{tu1}                       │
 │           │              │◄────────────┤            │              │              │              │
 │           │◄─────────────┤             │            │              │              │              │
 │  render tool_use block ("executed locally" tag, args 折叠)         │              │              │
 │◄──────────┤              │             │            │              │              │              │
 │           │              │             │            │              │ permission gate:           │
 │           │              │             │            │              │  sticky? bypass? — neither │
 │           │              │             │            │              │  insert event              │
 │           │              │             │            │              │  permission_request{p1}    │
 │           │              │             │            │              │  block this goroutine      │
 │           │              │             │            │              │              │              │
 │           │              │             │  ◄─SSE─ permission_request{p1}                         │
 │           │              │◄────────────┤            │              │              │              │
 │           │◄─────────────┤             │            │              │              │              │
 │  弹权限面板 (y/N/always)  │             │            │              │              │              │
 │◄──────────┤              │             │            │              │              │              │
 │           │              │             │            │              │              │              │
 │  按 'y'    │              │             │            │              │              │              │
 ├──────────►│              │             │            │              │              │              │
 │           ├─────────────►│             │            │              │              │              │
 │           │              │  POST /permissions/p1 {decision=allow, scope=once}                   │
 │           │              ├────────────────────────►│              │              │              │
 │           │              │             │            │  POST /permissions/decide │              │
 │           │              │             │            ├─────────────►│              │              │
 │           │              │             │            │              │ unblock; sticky? no       │
 │           │              │             │            │              │  → call exec-registry      │
 │           │              │             │            │              │  POST /api/execute         │
 │           │              │             │            │              ├─────────────►│              │
 │           │              │             │            │              │              │ yamux: open │
 │           │              │             │            │              │              ├─────────────►│
 │           │              │             │            │              │              │              │ HTTP /tool/execute
 │           │              │             │            │              │              │              │ executortools.bash
 │           │              │             │            │              │              │              │ runs locally
 │           │              │             │            │              │              │ ◄────────────┤
 │           │              │             │            │              │ ◄────────────┤  result      │
 │           │              │             │            │  ◄─SSE─ tool_result{tu1, output, exit_code}│
 │           │              │             │  ◄─SSE─    │              │              │              │
 │  render tool_result (折叠输出)         │            │              │              │              │
 │◄──────────┤              │             │            │              │              │              │
 │           │              │             │            │              │ CC: 继续推理 → assistant   │
 │           │              │             │            │              │    text "已执行，结果是..."│
 │           │              │             │            │  ◄─SSE─ assistant_message               │
 │           │              │             │  ◄─SSE─    │              │              │              │
 │  render 助手消息          │             │            │              │              │              │
 │◄──────────┤              │             │            │              │              │              │
 │           │              │             │            │              │  CC: result message → done │
 │           │              │             │            │  ◄─SSE─ turn_done                       │
 │           │              │             │  ◄─SSE─    │              │              │              │
 │  状态栏：done             │             │            │              │              │              │
 │◄──────────┤              │             │            │              │              │              │
 │           │              │             │            │              │ POST agentserver           │
 │           │              │             │            │              │ /internal/sessions/{sid}/  │
 │           │              │             │            │              │  turn-finished             │
 │           │              │             │            │              ├──────────────►             │
 │           │              │             │            │  clear active_turn_id                     │
```

**关键观察：**
- TUI 三个内部组件**之间没有箭头**——它们各自只和远端服务交互。本机 ExecutorClient 在 `/tool/execute` 发生时才被卷入，且之前不知道 TUI 的存在。
- `tool_use` 事件**先于** `permission_request` 出现，让用户能看到"准备执行什么 + 一会儿要批准什么"的因果关系。
- `tool_use` 上的 `executor_id` 字段决定 TUI 是否打 "executed locally" 标——通过对比 `executor_id == 本进程的 executor_id`。
- `tool_result` 由 cc-broker 收到 executor-registry 回包后**统一 emit**，TUI 不通过 ExecutorClient 拿结果——双路径会引入排序问题。

### 3.2 变体 A: CC 选了**另一台机器**上的 executor

差异（vs 3.1）：
- TUI 收到 `tool_use{executor_id=exe_remote_b}` 时不打 "executed locally" 标，改成 "→ exe_remote_b" 标。
- `permission_request` 仍 emit 到本 session 的 SSE，TUI 仍弹面板，"Allow remote_bash on **exe_remote_b**?"。
- exec-registry 把 `/tool/execute` 走 yamux stream 派到 `exe_remote_b` 的 tunnel，本机 Executor goroutine 完全不参与。

**这正是把权限闸搬到 cc-broker 的福利**——本机 TUI 给跨机器的 executor 授权。

### 3.3 变体 B: 权限被拒（用户按 N）

差异（vs 3.1）：
- TUI POST `{decision:"deny"}`。
- cc-broker 收到决定后，**不调** exec-registry，而是直接生成一个 `tool_result{tu1, output:"permission denied by user", exit_code:1, is_error:true}` emit。
- CC 拿到这个错误结果，自行判断是道歉还是换路径。
- 一条 `permission_resolved{pid:p1, decision:"deny"}` 事件也 emit，TUI 把权限面板替换成 "denied" 行历史记录。

### 3.4 变体 C: `always` 命中

第二次 CC 又调 `remote_bash` 且 args 模式与上次匹配：
- cc-broker permission gate 查到 sticky 规则 → **不 emit `permission_request`**，直接派发。
- 但仍 emit 一条 `permission_resolved{decision:"allow", scope:"sticky"}` 让 TUI 在 tool_use 块旁边小字标 "auto-approved (always)"——保留可观察性。

`always` 的"模式匹配"v1 规则：

```go
func makeRuleKey(tool, executorID string, args json.RawMessage) string {
    switch tool {
    case "remote_bash":
        cmd := extractField(args, "command")
        // 取前两 token 作为粒度。git status / git push 区分；ls / ls -la 同。
        toks := strings.Fields(cmd)
        head := strings.Join(toks[:min(2, len(toks))], " ")
        return fmt.Sprintf("%s|%s|cmd:%s", tool, executorID, head)
    case "remote_read", "remote_write", "remote_edit", "remote_glob", "remote_ls", "remote_grep":
        path := extractField(args, "file_path"); if path == "" { path = extractField(args, "path") }
        return fmt.Sprintf("%s|%s|dir:%s", tool, executorID, filepath.Dir(path))
    default:
        return fmt.Sprintf("%s|%s", tool, executorID)
    }
}
```

**粒度选择推理：**
- 单词太粗：批准 `git status` 一并放过 `git push`、`git reset --hard`，用户预期外。
- 整命令太细：批准 `git status` 不放过 `git status -s`，频繁打扰。
- **取前两 token** 是平衡点：覆盖 `git <subcmd>`、`docker <subcmd>`、`kubectl <subcmd>` 这类常见模式；`bash -c "..."` 这种特殊命令仍按 `bash -c` 一档对待（用户慎选）。

`(tool_name, executor_id, head_pattern)` 三元组完全匹配则命中。文件类工具按 dirname：批准 `~/proj/src/` 下的写入不会自动放行 `/etc/` 下的写入。

v1.x 加规则编辑面板让用户调粒度（升到 word-3 或退到 word-1）。

### 3.5 变体 D: 用户按 Esc 中断 turn

```
User       TUI            TUI(HTTPCli)   agentserver       cc-broker        SDK Client
 │  Esc     │                 │              │                 │                 │
 ├─────────►│                 │              │                 │                 │
 │          ├────────────────►│              │                 │                 │
 │          │  POST /turns/{tid}/cancel                        │                 │
 │          │                 ├─────────────►│                 │                 │
 │          │                 │              │ POST cc-broker /cancel            │
 │          │                 │              ├────────────────►│                 │
 │          │                 │              │                 │ client.Close() ►│ kill claude
 │          │                 │              │                 │ emit turn_cancelled
 │          │ ◄─SSE─ turn_cancelled                            │                 │
 │ "cancelled" status        │              │                 │                 │
```

cc-broker 在 close SDK 后还要：
- 释放 TurnLock；
- 触发 `workspace.Teardown`（仍要 upload 已变更的文件，否则下一 turn 上下文不一致）；
- 把任何尚未完成的 `permission_request` 标 `permission_resolved{decision:"cancelled"}` emit，TUI 关掉所有挂起的权限面板。

### 3.6 变体 E: TUI 进程崩溃 → 重启 → auto-resume

```
[TUI 进程死]
   ├─ Executor goroutine 的 yamux 心跳超时 → exec-registry 标 executor offline
   ├─ SSE 连接断 → agentserver 检测到读端关闭，session 仍存活
   └─ pending permission_request 在 cc-broker 等到超时 → 转 deny → turn 异常结束

[用户重启 tui --continue]
   ├─ Executor goroutine 重新注册（同 OAuth 凭据，复用 executor_id）
   │  → tunnel 上线
   ├─ TUI 调 GET /api/agent-sessions?workspace_id=<wid>&channel_type=tui&executor_id=<eid>&latest=1
   ├─ TUI 调 POST /api/agent-sessions/{sid}/attach (re-claim responder)
   ├─ TUI 调 GET /api/agent-sessions/{sid}/events?since=last_seq SSE 追播
   │  → 渲染所有错过的 user/assistant/tool 事件
   │  → 渲染最末的 turn_cancelled 让用户知道上次 turn 异常结束
   └─ 准备好接收新 prompt
```

`since=<seq>` query 参数 + SSE 标准 `Last-Event-ID` header 都支持。

### 3.7 变体 F: SSE 短暂断开（TUI 进程仍在）

- SSE consumer goroutine 检测到 EOF → 状态栏标记 `events: reconnecting`。
- 指数退避重连，带 `Last-Event-ID`。
- 期间 Executor goroutine 不受影响（C2/C3 独立）。
- 期间 TUI 输入不阻塞——用户可以继续敲 prompt POST 出去，远端事件先在 cc-broker 累积，SSE 重连后追播。但 UI 标 "events delayed" 让用户知道画面落后。

---

## 4. agentserver 端点 Contract

所有 TUI 端点的 auth：`Authorization: Bearer <oauth_access_token>`，token 由 `EnsureValidToken(serverURL)` 维护（与 `executor` 子命令同款）——**用户身份**。

⚠️ **token 区分：** TUI 进程同时持有两种 bearer token：
- **OAuth user access_token**：用户身份，TUI 端点用（本节所有端点）。
- **executor RegistryToken**（`ExecutorSession.RegistryToken`）：executor 身份，仅 ExecutorClient 在 `/api/executors/{id}/heartbeat` 用。

两者来源不同，作用域不同，不可混用。

所有 5xx/4xx 响应 body 为 `{"error":{"code":"...","message":"..."}}`。

### 4.1 端点清单

| 端点 | 方法 | 调用方 | 用途 | 是否新建 |
|---|---|---|---|---|
| `/api/workspaces/{wid}/tui/inbound` | POST | TUI | 提交 prompt（自动 attach 或新建 session） | 新建 |
| `/api/agent-sessions` | POST | TUI | 显式新建 session（`/clear`） | 新建 |
| `/api/agent-sessions/{sid}/attach` | POST | TUI | 重新认领 permission_responder | 新建 |
| `/api/agent-sessions/{sid}/events` | GET (SSE) | TUI | 订阅 session 事件流（用户视角，**不是**已有的 worker SSE） | 新建 |
| `/api/agent-sessions/{sid}/control` | POST | TUI | R 类 slash 命令 | 新建 |
| `/api/agent-sessions/{sid}/turns/{tid}/cancel` | POST | TUI | 中断当前 turn | 新建 |
| `/api/agent-sessions/{sid}/permissions/{pid}` | POST | TUI | 权限决定 | 新建 |
| `/api/agent-sessions?workspace_id=...&channel_type=tui&executor_id=...&latest=1` | GET | TUI | 查找最近 session（`--continue`） | 新建 |
| `/api/executors/{id}/status` | GET | TUI | 拉本机 tunnel 状态（反代到 executor-registry `GET /api/executors/{id}`） | 新建 |
| `/internal/sessions/{sid}/turn-finished` | POST | cc-broker（内部回调） | 清 active_turn_id，避免 leak | 新建 |

**底层基础设施复用：** `/api/agent-sessions/{sid}/events` 的 SSE 实现可以复用现有 `internal/bridge/SSEBroker`（`Subscribe`/`Publish` 已有），但 HTTP route 是新的，认证用 `s.Auth.Middleware`（用户 OAuth），不同于已有的 worker JWT。事件来源也不同：worker SSE 是 cc-broker 写 → bridge 流回 worker；用户 SSE 是 cc-broker 写 `agent_session_events` → bridge SSEBroker fan-out 给所有订阅的 TUI。

### 4.2 `POST /api/workspaces/{wid}/tui/inbound`

```jsonc
// Request
{
  "session_id": "cse_xxx",         // 可选；缺省时按 (executor_id, "tui") 解析或新建
  "executor_id": "exe_tui_a",       // 必填，TUI 所在 executor 身份
  "text": "帮我看看 main.go",
  "attachments": [                  // 可选
    {
      "kind": "file",               // file | image
      "filename": "screenshot.png",
      "size": 102400,
      "content_b64": "..."
    }
  ],
  "metadata": {                     // 可选，turn 级 override（不写入 sticky）
    "model": "claude-opus-4-7",     // /model 设置后此字段缺省（已 sticky）
    "turn_kind": "user"             // user | compaction（系统注入，TUI render 时隐藏 user_message）
  },
  "permission_responder": true      // 第一次来时声明本 SSE 订阅者是权限响应者
}

// Response 202 Accepted
{
  "session_id": "cse_xxx",          // 回传，便于 TUI 拿到新建的 sid
  "turn_id": "trn_yyy",
  "next_event_seq": 142
}
```

**Session 解析规则：**
- `session_id` 给定 → 直接用（404 若不存在或无权限）。
- 未给定且 `--continue` 时 TUI 先调 §4.9 拿 sid。
- 未给定且无 `--continue` → 隐式新建 session：`channel_type="tui"`, `external_id=fmt.Sprintf("tui:%s:%d", executor_id, time.Now().Unix())`, `permission_mode="ask"`。

**幂等与并发（CAS + 防 leak）：**
- 进 inbound 时 server 跑 SQL：
  ```sql
  UPDATE agent_sessions SET active_turn_id=$new_tid
   WHERE id=$sid AND active_turn_id IS NULL
  RETURNING active_turn_id;
  ```
- 0 行 → 已有 turn → 409 `{"code":"turn_in_progress","turn_id":"<existing>"}`。
- 1 行 → 拿到锁，去 cc-broker 启 turn。
- **leak 防御：** cc-broker 在 turn 真正结束（done / cancelled / errored / crashed-recovered）时回调 `POST /internal/sessions/{sid}/turn-finished` 清 `active_turn_id`。如果 cc-broker 进程崩溃没回调，agentserver 后台 worker 每 5min 扫描 `active_turn_id` 非空但 cc-broker 报无此 turn 的 session（通过 `cc-broker GET /api/sessions/{sid}/turns/active`），强制清。

attachments 总大小硬上限 v1：8 MiB。

**异步语义：** 端点立即 202，turn 在后台跑。SSE 是唯一回流途径——本响应不带 turn 内容。

### 4.3 `POST /api/agent-sessions`（`/clear`）

```jsonc
// Request
{
  "workspace_id": "ws_xxx",
  "executor_id": "exe_tui_a",
  "title": null,
  "permission_mode": "ask",
  "preferred_executor_id": "exe_tui_a"
}

// Response 201
{
  "session_id": "cse_zzz",
  "external_id": "tui:exe_tui_a:1730000000",
  "channel_type": "tui",
  "created_at": "..."
}
```

不会自动 attach 或 send prompt——纯创建。

### 4.4 `POST /api/agent-sessions/{sid}/attach`

```jsonc
// Request
{
  "executor_id": "exe_tui_a",
  "mode": "operator",                 // operator | observer
  "as_permission_responder": true,
  "also_become_preferred": true,
  "ttl_seconds": 300
}

// Response 200
{
  "session_id": "cse_xxx",
  "permission_responder": "exe_tui_a",
  "previous_responder": "exe_laptop", // 若覆盖了已有 responder
  "previous_preferred": "exe_laptop"
}
```

**`mode` 语义：**

| mode | 行为 |
|---|---|
| `operator`（默认） | 抢 permission_responder；`also_become_preferred=true` 时 preferred_executor 也跟随 |
| `observer` | 只订阅 SSE，不动 responder/preferred；输入框灰；浮层不显示 |

**多 operator 抢占语义：** 第二个 TUI attach 同一 session 时，若 mode=operator 则强制接管。前任降级为 observer，收 `permission_responder_lost` SSE 事件，cc-broker 把 pending permission_request 全部 emit 给新 responder。

### 4.5 `GET /api/agent-sessions/{sid}/events`（SSE）

标准 SSE。每条事件：

```
id: 142
event: tool_use
data: {"event_id":"evt_aaa","session_id":"cse_xxx","seq":142,"turn_id":"trn_yyy","tool_use_id":"tu_1","tool":"remote_bash","executor_id":"exe_tui_a","args":{"command":"ls"},"created_at":"..."}
```

**Query params：**
- `since=<seq>`：从指定 seq 之后开始追播。
- `tail=<N>`：仅播最近 N 条，再开始接 live（v1 缺省 200）。
- 缺省（无 query）：仅 live。

**Event 类型清单（v1）：**

| event | 触发者 | 关键字段 |
|---|---|---|
| `user_message` | inbound | text, attachments |
| `assistant_message` | cc-broker | text |
| `tool_use` | cc-broker | tool_use_id, tool, executor_id, args |
| `tool_result` | cc-broker | tool_use_id, output, exit_code, is_error |
| `permission_request` | cc-broker permission gate | permission_id, tool, executor_id, args, timeout_ms |
| `permission_resolved` | cc-broker | permission_id, decision, scope (`once`/`always`/`cancelled`/`timeout`/`sticky`) |
| `executor_status` | agentserver | executor_id, status, reason |
| `send_message` | cc-broker `send_message` | text, sender |
| `send_image` | cc-broker `send_image` | source_kind (b64/url/path), data, mime, caption |
| `send_file` | cc-broker `send_file` | source_kind, data, filename, caption |
| `ask_user` | cc-broker `AskUserQuestion` | question_id, question, options[], multi_select |
| `turn_started` | cc-broker | turn_id |
| `turn_done` | cc-broker | turn_id, reason (`complete`/`max_turns`/`error`) |
| `turn_cancelled` | cc-broker | turn_id |
| `compaction` | cc-broker | summary_seq |
| `permission_responder_lost` | agentserver | new_responder |
| `permission_responder_changed` | agentserver | from, to, preferred_changed |

⚠️ **不包含 `executor_status` 事件**——agentserver 不主动监听 executor-registry 心跳。TUI 状态栏走拉取模式（§4.13）。

**Last-Event-ID** header：标准 SSE 重连机制，等同 `since=<id>`。

**Heartbeat**：服务端每 15s 发 `: keepalive\n\n`，防中间代理断线。

### 4.6 `POST /api/agent-sessions/{sid}/control`（R 类命令）

```jsonc
// Request
{
  "command": "model",               // model | permission | compact | cost | agents
  "args": {"model": "claude-haiku-4-5"}
}

// Response 200 — 形态视命令而定
```

**命令分发：**

| command | args | server 行为 | 响应 |
|---|---|---|---|
| `model` | `{"model": "..."}` | 写 `agent_sessions.preferred_model`；下个 turn cc-broker 读取 → `WithModel(...)` | `{"applied": true, "model": "..."}` |
| `permission` | `{"mode": "ask"\|"bypass"}` | 写 `agent_sessions.permission_mode` | `{"applied": true, "mode": "..."}` |
| `compact` | `{}` | POST cc-broker `/api/sessions/{sid}/compact` | `{"queued": true}` |
| `cost` | `{}` | 查 `agent_session_events` 累加 token usage | `{"input_tokens":..., "output_tokens":..., "cost_usd":...}` |
| `agents` | `{}` | 调 executor-registry `GET /api/executors?workspace_id={wid}` | `{"executors":[{...}]}` |

未来加 R 类命令——例如 `/system-prompt-append`——只在这里加分支，TUI 零改动。

### 4.7 `POST /api/agent-sessions/{sid}/turns/{tid}/cancel`

```jsonc
// Request: {} (空)
// Response 202
{"cancelled": true}
```

agentserver 转发到 cc-broker `POST /api/sessions/{sid}/turns/{tid}/cancel`。

**幂等：** 同 turn_id 重复 cancel 返回 200 `{"cancelled":true,"already":true}`。

### 4.8 `POST /api/agent-sessions/{sid}/permissions/{pid}`

```jsonc
// Request
{
  "decision": "allow",              // allow | deny
  "scope": "once",                  // once | always
  "responder_executor_id": "exe_tui_a"
}

// Response 200
{"accepted": true, "applied_at": "..."}
```

**并发：**
- 多个 TUI 同时 POST 同 pid → cc-broker 用 CAS 接受第一个，其余返回 409 `{"code":"already_resolved","decision":"allow|deny"}`。
- pid 已超时 → 返回 410 `{"code":"expired"}`，TUI 收到后清掉本地面板。

### 4.9 `GET /api/agent-sessions?workspace_id=...&channel_type=tui&executor_id=...&latest=1`

```
GET /api/agent-sessions?workspace_id=ws_xxx&channel_type=tui&executor_id=exe_tui_a&latest=1
```

```jsonc
// Response 200
{
  "sessions": [
    {
      "session_id": "cse_xxx",
      "external_id": "tui:exe_tui_a:1730000000",
      "title": "...",
      "last_event_seq": 187,
      "last_activity_at": "...",
      "permission_responder": null
    }
  ]
}
```

Server filter：`channel_type='tui' AND external_id LIKE 'tui:<executor_id>:%'` AND `archived_at IS NULL`。`latest=1` 只取最近活跃的一条（按 `updated_at DESC`）。

`workspace_id` 必填——执行 RBAC 检查（用户必须是该 workspace 成员）。

### 4.10 cc-broker `send_*` 工具的 TUI 短路

⚠️ **agentserver 没有 IMRouter 抽象**。cc-broker 的 `tools/im.go` 直接 POST 到 imbridge `/api/internal/imbridge/send`。让 TUI session 跑通 `send_message` / `send_image` / `send_file` 的方式是 **cc-broker 端短路**：

```go
// internal/ccbroker/tools/im.go - send_message handler 修改示意
func sendMessageHandler(ctx context.Context, in sendMessageInput) (*agentsdk.McpToolResult, error) {
    if in.Text == "" {
        return errResult(fmt.Errorf("text is required")), nil
    }
    // 新增：TUI session 短路（无 IM 通道，直接 ack）
    if tctx.IMChannelID == "" {
        // SDK 自动把这个 tool_use 写入 agent_session_events，
        // SSE 流推给 TUI，TUI 渲染 args.text。无需外发。
        return &agentsdk.McpToolResult{
            Content: []agentsdk.McpToolContent{{Type: "text", Text: "ok"}},
        }, nil
    }
    // 既有 IM 路径不变
    return imbridgePost(ctx, tctx, "/api/internal/imbridge/send", map[string]string{...})
}
```

`send_image` / `send_file` 同理——TUI session 直接 ack，args 里的 source（base64）随 tool_use 进 SSE，TUI 端渲染。

**TUI 渲染端配合：** TUI 的 timeline 把 `tool_use{tool="send_message"}` 不当作普通工具调用渲染（避免噪音），而是当作 assistant 的"主动发言"——视觉上和 `assistant_message` 区分但权重相当。同理 `send_image` → 调用图像协议显示，`send_file` → 下载到本地并打印路径。

### 4.11 数据库 schema 变化

⚠️ 项目里有**两个** `agent_sessions` 表（agentserver 和 cc-broker 各持一份），加字段只动 **agentserver** 那份；cc-broker 自己的表不需要新字段（它从 turn metadata 读这些值）。

**迁移文件 1：`internal/db/migrations/021_tui_session_fields.sql`**（agentserver DB）

```sql
ALTER TABLE agent_sessions
  ADD COLUMN IF NOT EXISTS channel_type           TEXT NOT NULL DEFAULT 'im',
  ADD COLUMN IF NOT EXISTS creator_user_id        TEXT,
  ADD COLUMN IF NOT EXISTS preferred_model        TEXT,
  ADD COLUMN IF NOT EXISTS permission_mode        TEXT NOT NULL DEFAULT 'bypass',
  ADD COLUMN IF NOT EXISTS preferred_executor_id  TEXT,
  ADD COLUMN IF NOT EXISTS permission_responder   TEXT,
  ADD COLUMN IF NOT EXISTS responder_attached_at  TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS active_turn_id         TEXT;

UPDATE agent_sessions SET creator_user_id = 'unknown' WHERE creator_user_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_agent_sessions_channel_external
  ON agent_sessions (workspace_id, channel_type, external_id);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_responder
  ON agent_sessions (permission_responder) WHERE permission_responder IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agent_sessions_active_turn
  ON agent_sessions (active_turn_id) WHERE active_turn_id IS NOT NULL;
```

**迁移文件 2：`internal/executorregistry/migrations/002_executor_owner_sharing.sql`**

```sql
ALTER TABLE executors
  ADD COLUMN IF NOT EXISTS owner_user_id          TEXT,
  ADD COLUMN IF NOT EXISTS shared_to_workspace    BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE executors SET owner_user_id = 'unknown' WHERE owner_user_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_executors_owner ON executors(owner_user_id);
```

**cc-broker DB（`internal/ccbroker/migrations/`）：无变化。**`agent_session_events` 的 `payload JSONB` 已能装下新事件类型（`permission_request` 等），不需要 schema 改。

`permission_mode` 默认 `bypass` 是为了 IM 兼容；TUI 创建 session 时显式设 `ask`。

**v1 `agent_session_permission_pending` 表暂不引入** —— pending permissions 用 cc-broker 内存表。v1.x 引入 cc-broker 多副本时落库。

### 4.12 错误码统一

| code | HTTP | 含义 |
|---|---|---|
| `not_found` | 404 | session/permission/turn 不存在 |
| `forbidden` | 403 | executor_id 不属于该 workspace / 非 responder 试图 decide |
| `turn_in_progress` | 409 | inbound 时已有 turn 在跑 |
| `already_resolved` | 409 | 重复 decide 同 pid |
| `expired` | 410 | pid 已超时 |
| `attachment_too_large` | 413 | attachments 总大小 > 8 MiB |
| `unknown_command` | 400 | control 命令不识别 |
| `unsupported_model` | 400 | model id 不在 cc-broker 已知集合 |
| `not_operator` | 403 | observer 模式下试图 POST inbound |
| `cross_user_denied` | 403 | 跨用户 executor 调用（v1 默认） |

### 4.13 `GET /api/executors/{id}/status`（TUI 状态栏拉取用）

agentserver 反代到 executor-registry `GET /api/executors/{id}`。Auth 校验：调用方必须是该 executor 同 workspace 的成员（与现有 RBAC 一致）。

```jsonc
// Response 200
{
  "executor_id": "exe_tui_a",
  "status": "online",            // online | offline | unknown
  "last_heartbeat_at": "...",
  "tunnel_state": "connected",   // connected | reconnecting | disconnected
  "name": "...",
  "type": "local_agent"
}
```

TUI 每 10s 拉一次更新状态栏（`Bus.FetchTunnelStatus`）。**这取代了 SSE `executor_status` 事件**——更简单，agentserver 不必订阅 executor-registry 心跳。代价是状态有 ≤10s 延迟，可接受。

---

## 5. TUI 内部 Bubble Tea 模型

包名 `internal/agent/tui`。结构遵循 Bubble Tea 标准 Elm-style。

### 5.1 视图布局

```
┌────────────────────────────────────────────────────────────────────┐
│ session: cse_xxx · cwd: /home/me/proj · server: agent.cs.ac.cn     │
│ tunnel: online · events: live · turn: idle · model: opus-4-7       │
├────────────────────────────────────────────────────────────────────┤
│  ▸ user                                                            │
│    帮我看看 main.go 里有没有 race                                   │
│  ▸ assistant                                                       │
│    我先列一下文件…                                                   │
│  ▸ tool_use  remote_glob  → executor_local  ▾                      │
│      pattern: "**/*.go"                                            │
│  ▸ tool_result  ✓ 12 files (180 ms)  ▾                            │
│      main.go                                                       │
│      worker.go                                                     │
│      ... (8 more, press Enter to expand)                          │
│  ▸ assistant                                                       │
│    在 main.go:42 有个潜在 race…                                     │
├────────────────────────────────────────────────────────────────────┤
│ ╭─ permission_request perm_p1 ─────────────────────────────────╮   │
│ │ remote_bash on exe_tui_a (this machine)                       │   │
│ │   command: git diff main.go                                   │   │
│ │ [ y ] allow once   [ a ] always   [ N ] deny   [ esc ] later  │   │
│ ╰────────────────────────────────────────────────────────────────╯   │
├────────────────────────────────────────────────────────────────────┤
│ > _                                                                │
│   (Enter 发送 · Shift+Enter 换行 · Esc 中断 turn · / 命令 · ? 帮助)  │
└────────────────────────────────────────────────────────────────────┘
```

布局用 `lipgloss.JoinVertical`：
- **`statusBar`**（高度 2）：lipgloss styled，sticky。
- **`viewport`**（自适应高度）：`bubbles/viewport.Model` 包 lipgloss 渲染好的 string，自动滚到底，PgUp/PgDn 可回看。
- **浮层**：`permissionPanel` / `askUserPanel`——同时只显示一个最高优先级，其他排队。
- **`inputArea`**（高度 1–4，textarea 自适应）：`bubbles/textarea.Model`。

### 5.2 顶层 Model 状态机

```go
type Mode int
const (
    ModeNormal     Mode = iota   // 接收用户输入，render 消息流
    ModeAwaitPerm                // 浮层显示中，输入框被禁用
    ModeAwaitAskUser             // ask_user 面板显示中
    ModeCommand                  // 用户按 / 进入命令补全面板
    ModeAttachPicker             // 用户按 /attach 进入文件选择
    ModeQuitting                 // 拆卸中
)

type Model struct {
    cfg          Config
    bus          *Bus

    mode         Mode
    sessionID    string
    turnID       string
    cwd          string
    model        string
    permMode     string
    statusTunnel ExecutorStatus  // online | reconnecting | offline | unknown
    statusEvents StreamStatus    // live | reconnecting | delayed
    statusTurn   string          // idle | running | cancelling

    viewport     viewport.Model
    timeline     *Timeline

    input        textarea.Model

    permQueue    []PermissionPanel
    askQueue     []AskUserPanel
    cmdPalette   *CommandPalette
    attachPicker *AttachPicker

    pendingAttach []AttachmentRef
}
```

`Init()` 启动若干长跑 Cmd：
- `bus.subscribeSSE(ctx)`：SSE 连接，每帧 emit `EventArrivedMsg`。
- `bus.tickStatus(1s)`：每秒 emit `StatusTickMsg`，刷新状态栏。
- `bus.fetchInitialState()`：拉 session 元数据，emit `SessionLoadedMsg`。

### 5.3 Msg 类型

```go
// 来自 SSE
type EventArrivedMsg struct{ Event SessionEvent }
type SSEStatusMsg     struct{ Status StreamStatus; Reason string }

// 来自 HTTP 客户端
type InboundAcceptedMsg struct{ TurnID string }
type InboundRejectedMsg struct{ Code, Message string }
type ControlReplyMsg    struct{ Command string; Body json.RawMessage }
type CancelReplyMsg     struct{}
type DecisionAckMsg     struct{ PermissionID string }

// 状态轮询
type StatusTickMsg     struct{ TunnelStatus ExecutorStatus }

// 内部
type SessionLoadedMsg  struct{ Session SessionMeta }
type AttachmentPickedMsg struct{ Attachment AttachmentRef }
type AttachmentRemovedMsg struct{ Index int }
type CommandSelectedMsg  struct{ Command string; Args string }
type ResumeRequestedMsg  struct{ SessionID string }
type ClearRequestedMsg   struct{}
type FatalErrorMsg       struct{ Err error }
```

### 5.4 Timeline 与事件渲染

```go
type Timeline struct {
    items     []TimelineItem
    seqMax    int64
    indexBy   map[string]int      // event_id → items 下标
    expanded  map[string]bool
}

type TimelineItem interface {
    EventID() string
    Render(width int, expanded bool) string
    Folded() string
}
```

每个 SSE event_type 对应一个具体类型实现 `TimelineItem`：
- `userMsgItem` / `assistantMsgItem`：纯文本 + lipgloss 着色。
- `toolUseItem`：tool 名 + executor 标签 + 折叠 args。
- `toolResultItem`：✓/✗ + 持续时间 + 折叠 output（截 100 行预览）。关联到 `tool_use` 的 event_id。
- `permissionRequestItem` / `permissionResolvedItem`：在 timeline 里画一行小字 + 浮层另出。
- `sendImageItem`：终端支持图像协议（iTerm2/Kitty/Sixel）则 inline 显示；否则下载到 `~/.agentserver/downloads/<sid>/<filename>` 并打印路径。
- `sendFileItem`：永远下载到本地后打印路径。
- `compactionItem`：横幅 `─── context compacted (saved N tokens) ───`。
- `executorStatusItem`：本机执行器状态变化时 banner（仅当影响本 session 的 preferred_executor 时打）。

**Timeline 上限：** v1 5000 items，超过自动 drop 最旧。v1.x 加 `/history` 命令拉更早。

### 5.5 浮层（permission / ask_user）

```go
type Panel interface {
    View(width int) string
    HandleKey(key tea.KeyMsg) (Panel, tea.Cmd, bool /* dismissed */)
    ID() string
    Priority() int
}
```

`Update` 在 `ModeAwaitPerm` / `ModeAwaitAskUser` 里把 KeyMsg 优先派发给当前 panel。

**permission 浮层键位：**
- `y` → POST `{decision:"allow", scope:"once"}`
- `a` → POST `{decision:"allow", scope:"always"}`
- `n` 或 `Enter` → POST `{decision:"deny"}`
- `Esc` → 把当前 perm 暂时压栈到 queue 末尾，回 ModeNormal

**ask_user 浮层键位：**
- 上下选；Enter 提交单选；Space 切多选；Tab/Shift-Tab 切换 Other 输入框

**多浮层排队：** permission_request 优先级高于 ask_user，但 v1 简化 FIFO 即可（同时来概率低）。

### 5.6 同 workspace 多 `tui` 并存场景

#### 5.6.1 场景矩阵

| 场景 | 描述 | 频率 |
|---|---|---|
| **A. 多 session 并行** | 同用户同时间起多个 `tui`，各自 session | 高 |
| **B. 跨机器接力** | 笔记本起 session，台机继续，"过户"控制权 | 中 |
| **C. 同 session 共看** | 两个 `tui` 订阅同 sid，一个操作一个旁观 | 低 |
| **D. 跨用户 executor 调用** | Alice 的 session 调 Bob 注册的 executor | 中（多用户必出现） |
| **E. ghost TUI** | 进程异常退出未清理，ResponderClaim 还在 server 上 | 中 |
| **F. 同 session 并发 inbound** | 两个 TUI 抢同一 session 发 prompt | 低 |

#### 5.6.2 策略：A — 多 session 并行（base case）

完全独立。每个 `tui` 有自己的 `executor_id` / session / `preferred_executor_id` / SSE 订阅。互不干扰。

`/agents` 列表里能看到全部，但**默认渲染时把"非 preferred"的 executor 标灰**——避免 CC 误调（视觉提示，cc-broker system prompt 同步给 LLM）。

#### 5.6.3 策略：B — 跨机器接力（核心场景）

机制 = `tui --resume <sid>` + 抢占式 attach + **preferred_executor 跟随 responder**。

```
[Laptop TUI 在跑 session cse_S，preferred_executor=exe_LAPTOP]
   │ 用户走到台机
   ▼
[Desktop 启动 `tui --resume cse_S`]
   ├─ Executor goroutine 注册 → 拿到 exe_DESKTOP
   ├─ POST /api/agent-sessions/cse_S/attach
   │     body={executor_id: exe_DESKTOP, mode: operator,
   │           as_permission_responder: true, also_become_preferred: true}
   │   ↓
   │   Server tx:
   │     - permission_responder = exe_DESKTOP
   │     - preferred_executor_id = exe_DESKTOP   ← 跟随
   │     - responder_attached_at = now()
   │   返回 {previous_responder: exe_LAPTOP, previous_preferred: exe_LAPTOP}
   ├─ Server SSE emit permission_responder_changed{from: exe_LAPTOP, to: exe_DESKTOP}
   │   Laptop TUI 收到 → "Control transferred to <Desktop hostname>" + 输入框禁用
   └─ Desktop TUI 抓 SSE since=last_seq → 渲染历史 + 等输入

[Desktop 输入新 prompt]
   POST /tui/inbound, executor_id=exe_DESKTOP
   cc-broker 下个 turn 的 list_executors 标 exe_DESKTOP 为 preferred
   tool 自然路由到 desktop 机器
```

**为什么 preferred 跟随 responder：** 用户从 A 走到 B 几乎一定希望命令也跑在 B 上。要保留 A 上跑的话用 `/control preferred-executor exe_LAPTOP` 显式覆盖。

#### 5.6.4 策略：C — 同 session 共看

attach 选项 `mode`：

| mode | 行为 |
|---|---|
| `operator`（默认） | 抢 permission_responder，preferred 跟随 |
| `observer` | 只订阅 SSE，不动 responder/preferred；输入框灰 |

操作者 → 旁观切换：`/take-control`；旁观者主动放手：`/observe`。

**多 operator 并发：** server 永远只允许一个 operator——后到者赢，前任降级 observer 收 `permission_responder_lost`。

**输入冲突：** observer 模式下输入框禁用。即使绕过 UI 直接 POST inbound，server 校验 `调用方 executor_id == session preferred_executor_id`，否则 403 `not_operator`。

#### 5.6.5 策略：D — 跨用户 executor 调用

**v1 默认：禁止。** cc-broker permission gate 第一步硬校验：

```go
if executor.OwnerUserID != session.CreatorUserID {
    if !executor.SharedToWorkspace {
        return permissionDeniedResult("cross-user executor invocation not allowed")
    }
    // 跨用户但已共享 → v1.x 走双签路径
}
```

**v1.x 增强（钩子已留好）：** 用户可在 web UI 给自己的 executor 打 "Share to workspace" 开关。开后跨用户调用要求**双签**：一条 permission_request 给 session 的 responder（Alice 的 TUI），一条给 executor 所有者的 "personal SSE"（Bob 的任一 TUI）；两个都同意才放行。

#### 5.6.6 策略：E — ghost TUI 清理

`agent_sessions.responder_attached_at` 由 SSE write-failure 检测维护：

- agentserver 的 SSE handler 每 30s 写一条 `: keepalive\n\n` 帧。
- **写成功** → 在内存里更新该 session 的"上次成功 write 时间"。
- **写失败**（client 已断、TCP RST、proxy 超时）→ HTTP handler 退出，server 知道连接死了，立即把 `permission_responder = NULL` 写回 DB。
- 兜底：后台 worker 每分钟扫描 `responder_attached_at < now() - {responder_ttl}` 的 session，强制清 responder——防止 SSE write 由于 buffering 假成功（rare，但可能）。
- 清空 responder 时把该 session 上所有 pending permission_request emit `permission_orphaned` 事件 → cc-broker 收到 → 超时 deny。

**TTL 配置：** 默认 90s，可通过启动 flag `--responder-ttl` 覆盖。`--responder-ttl=300s` 适合"用户经常 SSH 中断"场景；`--responder-ttl=30s` 适合 demo / 短会话。

依赖关键事实："SSE write 失败 → handler 退出"在 Go `net/http` 是默认行为（`ResponseWriter.Write` 返回 error）。需要在 handler 里检查 err 并 break loop，不要忽略。

#### 5.6.7 策略：F — 同 session 并发 inbound

inbound 端点 server 端用 DB CAS：

```sql
UPDATE agent_sessions
   SET active_turn_id = $new_tid
 WHERE session_id = $sid
   AND active_turn_id IS NULL
RETURNING active_turn_id;
```

返回 0 行 → 已有 turn 在跑 → 409 `turn_in_progress`，UI 提示用户等或 cancel。**v1 不做服务器侧排队**。

#### 5.6.8 sanity check

| 风险 | 防御 |
|---|---|
| 用户在 A 走开后，B 上工具调用却落在 A 上 | preferred_executor 跟随 responder |
| 旁观者意外发起 turn 抢了控制 | observer mode 输入框禁用 + server 端 not_operator 检查 |
| Alice 通过 workspace executor 跑 Bob 机器上的 rm -rf | v1 硬禁；v1.x 走双签 |
| 死掉的 TUI 永久占着 responder | 90s SSE 心跳 TTL，自动释放 |
| LLM 把命令发到非 preferred executor 上 | system prompt 强提示 + `/agents` 灰显非 preferred + tool_use 块上明显标注 executor |
| 两个 operator 同时按 Enter | DB CAS + 409，前端 UI 提示，不做隐式排队 |

### 5.7 网络层 `Bus`

`tui` 包内子结构 `Bus`，封装 §4 全部端点的 HTTP/SSE 调用：

```go
type Bus struct {
    httpCli    *http.Client
    sseCli     *SSEClient
    creds      *Credentials
    serverURL  string
    sessionID  string
    executorID string

    tunnelStatus atomic.Value  // ExecutorStatus
}

func (b *Bus) PostInbound(text string, attach []AttachmentRef, meta Metadata) tea.Cmd
func (b *Bus) PostControl(command string, args any) tea.Cmd
func (b *Bus) PostCancel(turnID string) tea.Cmd
func (b *Bus) PostDecision(pid string, decision, scope string) tea.Cmd
func (b *Bus) PostAnswer(qid string, answer any) tea.Cmd
func (b *Bus) AttachSession(sid string, mode string) tea.Cmd
func (b *Bus) NewSession(prefMode string) tea.Cmd
func (b *Bus) FetchSessionMeta(sid string) tea.Cmd
func (b *Bus) FetchTunnelStatus() tea.Cmd

func (b *Bus) SubscribeEvents(ctx context.Context) chan tea.Msg
```

**SSE 客户端：**
- 持有 `Last-Event-ID`，断线重连恢复。
- 30s 没收到任何帧 → 主动重连。
- 重连指数退避 1s/2s/4s/8s/15s/30s（cap）。
- 状态变化 emit `SSEStatusMsg`。

### 5.8 键位表（v1）

| 键 | Mode | 行为 |
|---|---|---|
| `Enter` | Normal（input 焦点） | 发送 prompt |
| `Shift+Enter` | Normal | 输入框换行 |
| `Esc` | Normal | turn 在跑 → 调 cancel；否则清空当前输入 |
| `Esc` | AwaitPerm/AwaitAskUser | 暂存当前面板，返回 Normal |
| `Ctrl+C` | 任何 | 二次按下退出 |
| `Ctrl+T` | Normal | 打开 session switcher |
| `/` | Normal（input 为空） | 打开命令面板 |
| `?` | Normal（input 为空） | 显示键位帮助浮层 |
| `PgUp/PgDn` | Normal | viewport 翻页 |
| `Home/End` | Normal | viewport 跳顶/底 |
| `Enter` | Tool block 选中态 | 展开/折叠 |

### 5.9 Slash 命令解析

输入框内容以 `/` 起头时，**送出前**在本地解析：

```go
type ParsedCommand struct {
    Class CommandClass   // L (local) | S (session) | R (remote)
    Name  string
    Args  string
}
```

- L 类直接在 Update 里处理（如 `/quit` → `tea.Quit`）。
- S 类调对应的 Bus 方法。
- R 类直接打包成 `Bus.PostControl(name, args)`，**不本地解释**——agentserver 知道。

**关键设计：** L/S 类是写死在 TUI 里的，R 类全靠 server 端注册——TUI 解析 `/` 命令时，只校验"是不是已知 L/S 名称"，否则全部当作 R 类透传。新增 `/system-prompt-foo` 之类的 R 命令时 TUI 零改动。

`/help` 列命令清单时硬编码 `[model, permission, compact, cost, agents]`；v1.x 加 `GET /api/control/commands` 让 server 报清单。

### 5.10 `/cd <path>` 的本地实现

`/cd` 是 L 类，但它影响 **Executor goroutine** 的 cwd。前面 §2 决定二者进程内零耦合。

**v1 选 A 方案：** 复用现有 `~/.agentserver/executors/<executor_id>.json`（`ExecutorSession` 序列化文件，已存在），**扩展加一个 `runtime_cwd` 字段**——非 ExecutorSession 自身字段，TUI 启动时附加，进程退出时清除。Executor goroutine 在 `executortools.New(workDir)` 之上包一层 dynamic-cwd lookup：每次 `Execute()` 之前 stat 该文件 mtime，变了则 reload `runtime_cwd`。

```go
// 文件结构（扩展现有 schema，向后兼容）
{
  "executor_id": "exe_xxx",
  "name": "...",
  "workspace_id": "...",
  "tunnel_token": "...",
  "registry_token": "...",
  "server_url": "...",
  "created_at": "...",
  "runtime_cwd": "/home/me/projects/foo"   // 新增，optional
}
```

旧 `executor` 子命令进程不写此字段，所以 TUI 包装层在字段缺失时回落到 startup `--work-dir`。

**注意：这里破坏了一点解耦原则**——但 cwd 改动是用户从这台机器对这台机器发起的操作，没必要绕一圈到云端。

如担心严格性可改 B 方案：补 tunnel 控制流走云端绕一圈，约 200 行。

### 5.11 退出与清理

- `Ctrl+C` × 2 / `/quit` → `tea.Quit` + cancel 主 ctx → SSE goroutine 退出 → Executor goroutine 退出。
- 当前有 turn → 退出前先 POST `/cancel`，等 200 后再退。
- `~/.agentserver/sessions/...` 不写（TUI 无状态）。

---

## 6. cc-broker 改动

cc-broker 是 stateless cc 的"脑壳"，本设计要让它新增三件事：(1) 接收 metadata 并透传到 SDK 选项，(2) 在每个 `remote_*` 工具入口插权限闸，(3) 把 `preferred_executor_id` 渲染进 system prompt 给 LLM 看。所有改动**不破坏现有 IM 流**。

### 6.1 文件级影响

```
internal/ccbroker/
├── handler_turns.go        ← 改：解析 metadata，传给 runner
├── runner/
│   └── options.go          ← 改：metadata → []agentsdk.QueryOption
├── tools/
│   ├── context.go          ← 改：tools.Context 加 PermissionMode 等字段
│   ├── executor.go         ← 改：每个 remote_* handler 入口套 permGate.Check()
│   ├── permission.go       ← 新：闸逻辑
│   └── prompt.go           ← 新：system prompt builder
└── store.go                ← 改：sticky rules 内存表
```

**新增端点（cc-broker 自身 HTTP server）：**
```
POST /api/sessions/{sid}/turns/{tid}/cancel
POST /api/sessions/{sid}/permissions/{pid}/decide
POST /api/sessions/{sid}/compact
GET  /api/sessions/{sid}/turns/active        ← agentserver leak 兜底用
```

实施时与 cc-broker `internal/ccbroker/server.go` 现有 route 风格对齐（当前看到 `handler_session.go` / `handler_turns.go` 风格无统一 `/api/v1/` 前缀，沿用即可）。

**新增 outbound（cc-broker 调 agentserver）：**
```
POST agentserver /internal/sessions/{sid}/turn-finished  ← turn 结束清 active_turn_id
```

### 6.2 ProcessTurn 请求扩展

`POST /api/turns` body schema 增加（向后兼容，新字段全可选）：

```jsonc
{
  "session_id": "cse_xxx",
  "workspace_id": "ws_xxx",
  "user_message": "...",
  "executors": [...],                // 现有
  "im_channel_id": "...",             // 现有
  "im_user_id": "...",                // 现有
  "metadata": {                       // 新增
    "channel_type": "tui",            // 'im' | 'tui'
    "creator_user_id": "u_alice",     // 跨用户校验
    "permission_mode": "ask",         // 'ask' | 'bypass'
    "model": "claude-opus-4-7",
    "preferred_executor_id": "exe_a",
    "turn_kind": "user"               // 'user' | 'compaction'
                                       // compaction: cc-broker 把 user_message 替换为系统注入的
                                       // "summarize the conversation history" 指令；TUI render 时
                                       // 不渲染 user_message，只渲染最终的 assistant compaction summary
  }
}
```

agentserver 在转发到 cc-broker 时，从 session record 读出 sticky 字段填到 metadata。

### 6.3 metadata → SDK options

```go
func optsFromMetadata(meta TurnMetadata, base []agentsdk.QueryOption) []agentsdk.QueryOption {
    opts := base
    if meta.Model != "" {
        opts = append(opts, agentsdk.WithModel(meta.Model))
    }
    // ask 和 bypass 都设 SDK BypassAll：闸在 tool handler 入口，SDK 不用弹自己的 prompt
    opts = append(opts,
        agentsdk.WithPermissionMode(agentsdk.PermissionBypassAll),
        agentsdk.WithAllowDangerouslySkipPermissions(),
    )
    return opts
}
```

**关键设计：** "ask" mode 下 SDK 仍 BypassAll——SDK 内置 permission prompt 是给 stdin TUI 用户看的，cc-broker 没有 stdin TUI。我们把闸放在 tool handler 入口，对 SDK 透明。

**`turn_kind=compaction` 的处理在 handler_turns.go 而非 options：**

```go
// handler_turns.go 内
userText := req.UserMessage
if req.Metadata.TurnKind == "compaction" {
    userText = "Summarize the conversation history into a concise context " +
               "preserving key facts, decisions, and ongoing tasks. " +
               "Output only the summary, no preamble."
    // 不写 user_message 事件到 agent_session_events，避免 TUI 看到这段奇怪的"用户消息"。
    skipUserMessageEvent = true
}
```

最终 SDK 的 SDKResultMessage 含 summary 文本，cc-broker 写一条 `compaction` 事件（含 summary_seq、summary_text）到 `agent_session_events`，TUI 渲染为 §5.4 的 `compactionItem` 横幅。

### 6.4 Permission Gate 逻辑

```go
// internal/ccbroker/tools/permission.go

type Gate struct {
    mu        sync.Mutex
    pending   map[string]*pendingReq
    sticky    map[string]map[string]Decision  // sessionID → ruleKey → decision
    notifySSE func(sessionID string, evt Event)
}

type pendingReq struct {
    PID         string
    SessionID   string
    TurnID      string
    Tool        string
    ExecutorID  string
    Args        json.RawMessage
    EmittedAt   time.Time
    Deadline    time.Time
    Decided     chan Decision
}

type Decision struct {
    Verdict string  // "allow" | "deny" | "cancelled" | "timeout"
    Scope   string  // "once" | "always"
    By      string  // executor_id of decider
}

func (g *Gate) Check(ctx context.Context, req CheckRequest) error {
    // 1. cross-user 硬校验
    if req.ExecutorOwnerUserID != req.SessionCreatorUserID && !req.ExecutorSharedToWorkspace {
        return ErrCrossUserDenied
    }

    // 2. permission_mode = bypass → 放行
    if req.PermissionMode == "bypass" {
        return nil
    }

    // 3. sticky rule 命中 → 按 sticky 决定
    ruleKey := makeRuleKey(req.Tool, req.ExecutorID, req.Args)
    if d, ok := g.lookupSticky(req.SessionID, ruleKey); ok {
        g.emitResolved(req, d, "sticky")
        if d.Verdict == "allow" {
            return nil
        }
        return ErrPermissionDenied
    }

    // 4. ask mode → emit request, block until decision
    pid := newPID()
    pr := &pendingReq{
        PID: pid, SessionID: req.SessionID, TurnID: req.TurnID,
        Tool: req.Tool, ExecutorID: req.ExecutorID, Args: req.Args,
        EmittedAt: time.Now(),
        Deadline:  time.Now().Add(req.Timeout),
        Decided:   make(chan Decision, 1),
    }
    g.mu.Lock()
    g.pending[pid] = pr
    g.mu.Unlock()

    g.notifySSE(req.SessionID, Event{
        Type: "permission_request",
        Payload: encodePending(pr),
    })

    var d Decision
    select {
    case d = <-pr.Decided:
    case <-time.After(time.Until(pr.Deadline)):
        d = Decision{Verdict: "timeout"}
    case <-ctx.Done():
        d = Decision{Verdict: "cancelled"}
    }

    g.mu.Lock()
    delete(g.pending, pid)
    g.mu.Unlock()

    if d.Scope == "always" && d.Verdict == "allow" {
        g.recordSticky(req.SessionID, ruleKey, d)
    }
    g.emitResolved(req, d, "live")

    if d.Verdict == "allow" {
        return nil
    }
    return ErrPermissionDenied
}

func (g *Gate) Resolve(pid string, d Decision) error { /* ... */ }
func (g *Gate) CancelTurn(turnID string)              { /* ... */ }
```

`makeRuleKey` 见 §3.4。

**v1 sticky 表是进程内 map**——cc-broker 多副本时同 session 永远落到同副本（已有 turn lock 路由保证）。v1.x 升级到 DB 表后多副本互不干扰。

### 6.5 在每个 `remote_*` handler 入口插闸

```go
agentsdk.Tool("remote_bash", desc, func(ctx context.Context, in BashInput) (Result, error) {
    if err := tctx.Gate.Check(ctx, CheckRequest{
        SessionID: tctx.SessionID,
        TurnID:    tctx.CurrentTurnID(),
        Tool:      "remote_bash",
        ExecutorID: in.ExecutorID,
        Args:      mustJSON(in),
        PermissionMode:           tctx.PermissionMode,
        SessionCreatorUserID:     tctx.CreatorUserID,
        ExecutorOwnerUserID:      tctx.LookupExecutorOwner(in.ExecutorID),
        ExecutorSharedToWorkspace: tctx.LookupExecutorShared(in.ExecutorID),
        Timeout:                  30 * time.Second,
    }); err != nil {
        return errResult(err), nil
    }
    return execRegistry.Execute(ctx, ExecuteRequest{...})
})
```

**LLM 可见的失败语义统一：**

```jsonc
{
  "is_error": true,
  "content": [
    {"type": "text", "text": "permission_denied: user declined remote_bash on exe_tui_a (command: rm -rf /)"}
  ]
}
```

`text` 始终以 `<reason_code>:` 开头：`permission_denied` / `permission_timeout` / `permission_cancelled` / `cross_user_denied` / `executor_offline` / `executor_unknown` / `tool_error`。

### 6.6 system prompt 拼接：preferred_executor 提示

```go
// internal/ccbroker/tools/prompt.go

func BuildSystemPrompt(meta TurnMetadata, executors []ExecutorInfo) string {
    var b strings.Builder
    b.WriteString("You are operating in a workspace with the following execution environments:\n\n")
    for _, e := range executors {
        marker := ""
        if e.ExecutorID == meta.PreferredExecutorID {
            marker = " ★ PREFERRED FOR THIS SESSION"
        }
        fmt.Fprintf(&b, "- %s (id=%s, type=%s)%s\n", e.DisplayName, e.ExecutorID, e.Type, marker)
    }

    if meta.PreferredExecutorID != "" {
        fmt.Fprintf(&b, `
The user is operating from executor "%s". Strongly prefer this executor for any
remote_* tool calls unless the task explicitly requires a different environment.
When unsure, ask before routing to a non-preferred executor.
`, meta.PreferredExecutorID)
    }

    if meta.ChannelType == "tui" {
        b.WriteString(`
The user is interacting through an interactive terminal client. You may use
AskUserQuestion freely for clarifications. The user can see tool calls in real
time and will be prompted to authorize potentially destructive operations.
`)
    } else {
        b.WriteString(`
The user is interacting through an instant messaging channel. Keep responses
concise and avoid asking too many clarifying questions in a row.
`)
    }
    return b.String()
}
```

`runner/options.go`：

```go
opts = append(opts, agentsdk.WithSystemPrompt(BuildSystemPrompt(meta, executors)))
```

### 6.7 三个新转发端点

```go
// POST /api/sessions/{sid}/turns/{tid}/cancel
func (s *Server) handleCancelTurn(w http.ResponseWriter, r *http.Request) {
    sid := chi.URLParam(r, "sid")
    tid := chi.URLParam(r, "tid")

    s.activeTurns.Cancel(sid, tid)   // SDK client.Close()
    s.gate.CancelTurn(tid)
    s.broadcast(sid, Event{Type: "turn_cancelled", TurnID: tid})

    w.WriteHeader(http.StatusAccepted)
}

// POST /api/sessions/{sid}/permissions/{pid}/decide
func (s *Server) handleDecidePermission(w http.ResponseWriter, r *http.Request) {
    var body struct {
        Verdict string `json:"verdict"`
        Scope   string `json:"scope"`
        By      string `json:"by"`
    }
    json.NewDecoder(r.Body).Decode(&body)
    pid := chi.URLParam(r, "pid")
    err := s.gate.Resolve(pid, Decision{Verdict: body.Verdict, Scope: body.Scope, By: body.By})
    switch err {
    case nil:
        w.WriteHeader(http.StatusOK)
    case ErrAlreadyResolved:
        w.WriteHeader(http.StatusConflict)
    default:
        http.Error(w, err.Error(), 500)
    }
}

// POST /api/sessions/{sid}/compact
func (s *Server) handleCompactNow(w http.ResponseWriter, r *http.Request) {
    sid := chi.URLParam(r, "sid")
    s.compactQueue.Set(sid)   // 下个 turn 的 metadata.turn_kind = "compaction"
    w.WriteHeader(http.StatusAccepted)
}

// GET /api/sessions/{sid}/turns/active  (agentserver leak 兜底用)
func (s *Server) handleGetActiveTurn(w http.ResponseWriter, r *http.Request) {
    sid := chi.URLParam(r, "sid")
    if tid, ok := s.activeTurns.Get(sid); ok {
        json.NewEncoder(w).Encode(map[string]string{"turn_id": tid})
        return
    }
    json.NewEncoder(w).Encode(map[string]any{"turn_id": nil})
}
```

agentserver 是 cc-broker 的代理：TUI 调 agentserver `/permissions/{pid}` → agentserver 调 cc-broker `/permissions/{pid}/decide`。这一层 indirection 让 agentserver 集中处理 auth + audit log。

**turn-finished 回调（防 leak）：** cc-broker 在 `handler_turns.go` 的 turn 结束（无论 done / cancelled / errored / panic）时，**defer 兜底调用** agentserver `POST /internal/sessions/{sid}/turn-finished {turn_id: ...}`。agentserver 收到后清 `active_turn_id`（带 turn_id 校验，防过期回调误清）。如果调用失败（agentserver 暂时不可达），日志 warn——下次 agentserver leak worker 5min 内会扫到并清理。

### 6.8 现有 IM 路径不受影响验证

- IM session `metadata.channel_type = "im"`、`permission_mode` 默认 `bypass`。
- bypass mode 下闸直接 `return nil`，零额外延迟。
- IM session 不传 `preferred_executor_id`，system prompt 不加偏好提示。
- 现有 `tools/im.go`（send_message 等）零改动。

**回归保证：** cc-broker 接到 IM 来的 ProcessTurn 请求时（`metadata` 字段全空），所有新增逻辑走 default 分支，行为与现状一致。集成测试录一个真实 IM turn 的请求/SSE，新代码下重放验证 byte-for-byte 等价。

---

## 7. 错误处理与边界

### 7.1 失败模式表

| 失败 | 当前 turn 表现 | 自愈 | 用户可见 |
|---|---|---|---|
| TUI OAuth token 过期 | inbound 401 | TUI 自动 refresh，refresh 失败提示登录 | "session expired, /login to re-auth" |
| TUI 无法连 agentserver | inbound POST 失败 | 指数退避重试；SSE 自动重连 | 状态栏 `events: reconnecting` |
| executor-registry 注册失败 | TUI 启动 abort | reset session 文件后重试 | 启动期错误日志 |
| tunnel 短暂断开 | 进行中工具调用挂起 | 现有 `ExecutorClient` 指数退避 | 状态栏 `tunnel: reconnecting` |
| tunnel 长断（> permission timeout） | cc-broker 派发到本机的 `/api/execute` 阻塞至超时 | 不自愈；下次工具调用要等 tunnel 重连 | tool_result "executor offline" |
| TUI 进程崩溃 | 当前 turn 在 cc-broker 继续跑，工具失败 → CC 拿 error → 转 deny | 用户重启 `tui --continue` resume | 重启后看到 turn_cancelled |
| cc-broker 重启 | 进行中 turn 全丢；pending permissions 全失（v1 内存版） | TurnLock 自然释放；下次 inbound 创建新 turn | TUI 收 SSE 断 → 重连 → 状态栏卡 `turn: running` |
| agentserver 重启 | inbound POST 中断；SSE 断；TUI 进程仍活 | TUI 重连；session 状态在 DB 完整保留 | 短暂 `events: reconnecting` |
| Permission 超时（30s 用户没回） | 当作 deny | n/a | 浮层自动消失 + history "permission expired" |
| 多 TUI 抢 attach race | DB 事务确定胜负 | 输方 SSE 收 `permission_responder_lost` | "control taken by <hostname>" |
| 跨用户 executor 调用（v1 禁） | 立刻 deny | n/a | tool_result "cross_user_denied" |
| LLM 在 session 里指定不存在的 executor_id | tool handler 返 error | n/a | tool_result "executor_unknown" |
| metadata.model 是 cc-broker 不识别的 model | 退到 session 默认 model 并 emit warning event | n/a | 状态栏 model 显示 fallback |
| 用户在 ask_user 浮层时进程退出 | qid 在 cc-broker 端 timeout | 重启 TUI 时如有 pending qid 在 SSE replay 里看到，重新弹（仍 pending 才重弹） | 透明 |

### 7.2 不优雅但接受的失败

- **cc-broker 重启时丢挂起 permission**：内存版 sticky/pending 的代价。v1 接受，因为 cc-broker 重启意味着 turn 已死，权限挂起也无意义。v1.x 落 DB 后这类丢失会消失。
- **TUI 重启后无法回放 90s 前的 ask_user/permission_request**：这些事件在 SSE 流里有，但如已被 cc-broker 超时 deny 则浮层弹出来是 stale 的——v1 简化做法：replay 时如遇已 resolved 的 pid/qid，跳过浮层只在 timeline 里展示。

---

## 8. 测试策略

### 8.1 分层

| 层 | 测试形态 | 位置 |
|---|---|---|
| `internal/agent/tui/`（Bubble Tea Model） | 构造 Msg 序列 → 调 Update → 断言 Model 字段 / golden file 对 View 字符串 | `internal/agent/tui/model_test.go` |
| `internal/agent/tui/Bus` | httptest fake agentserver；断言 endpoint 调用形态、SSE 解析、重连退避 | `internal/agent/tui/bus_test.go` |
| `internal/agent/tui/permission` panel | KeyMsg 输入 → POST body 断言 | `internal/agent/tui/panel_test.go` |
| `internal/ccbroker/tools/permission.go` Gate | 并发：emit + Resolve 顺序、超时、CancelTurn、sticky 命中、cross-user deny | `internal/ccbroker/tools/permission_test.go`（含 race detector） |
| `internal/ccbroker/runner/options` metadata 透传 | 表驱动 | `internal/ccbroker/runner/options_test.go` |
| `internal/ccbroker/tools/prompt` | system prompt 拼接 | `internal/ccbroker/tools/prompt_test.go` |
| agentserver TUI inbound / control / decision endpoints | http handler 测试，mock cc-broker client | `internal/server/handler_tui_*_test.go` |
| cc-broker `tools/im.go` TUI 短路 | `IMChannelID=""` 时 send_message 返回 ok 不发 HTTP | `internal/ccbroker/tools/im_test.go` |
| agentserver leak worker | mock cc-broker active turn 接口；断言 stale active_turn_id 被清；TTL 到的 responder 被清 | `internal/server/leak_worker_test.go` |
| cc-broker → agentserver turn-finished 回调 | mock agentserver；断言 done/cancelled/errored 三路径都触发 | `internal/ccbroker/handler_turns_test.go` |
| 多 TUI attach race | 集成测试：起两个 fake TUI，并发 attach | `internal/server/integration/multi_tui_test.go` |
| 端到端 happy path | 真启动 cc-broker + agentserver + 假 SSE 客户端 + 假 executor，跑完整 turn | `e2e/tui_happy_test.go` |
| 端到端 permission flow | 完整 turn 含 1 ask、1 deny、1 always 命中 | `e2e/tui_permission_test.go` |
| 端到端 cross-user deny | 两 user 双 executor 双 session | `e2e/tui_cross_user_test.go` |
| 端到端 接力 | 两 fake TUI，第一个发 prompt，第二个 attach，断言 preferred 跟随 | `e2e/tui_handoff_test.go` |
| 真实 `claude` CLI smoke | 手工 + CI nightly | `e2e/manual/` |
| 现有 IM turn 回归 | 录制真实 IM turn，新代码重放比对 | `internal/ccbroker/im_regression_test.go` |

### 8.2 不测试的

- 终端图像协议渲染（iTerm2/Kitty/Sixel）—— 平台相关，靠人工。
- Bubble Tea View 在不同终端尺寸下的精确像素布局 —— golden 测一两个尺寸即可。
- OAuth Device Flow（已被现有 `executor` 测试覆盖）。

---

## 9. 迁移与兼容性

### 9.1 不破坏的清单

| 现有功能 | 状态 |
|---|---|
| `agentserver connect` / `claudecode` | 完全不动 |
| `agentserver executor` | 完全不动 |
| `agentserver mcp-server` | 完全不动 |
| 现有 `agent_sessions` schema | 增字段 with sensible defaults |
| 现有 IM turn 路径 | metadata 全 nil 时走原 default，行为零差异 |
| cc-broker `/api/turns` 请求 schema | 加可选字段，旧调用不受影响 |
| executor-registry tunnel 协议 | 完全不动；`tui` 复用现有 `ExecutorClient` |

### 9.2 数据库迁移

见 §4.11，幂等。

### 9.3 部署顺序

1. **DB 迁移**
   - `internal/db/migrations/021_tui_session_fields.sql`（agentserver DB）
   - `internal/executorregistry/migrations/002_executor_owner_sharing.sql`（executor-registry DB）
   - cc-broker DB 无变化
2. **executor-registry 部署** —— 用新字段（owner_user_id 等），但旧 registration 请求仍可处理（fallback 'unknown'）。
3. **agentserver 部署** —— 含 TUI 端点（新增）、leak worker、`active_turn_id` CAS、cc-broker `/internal/sessions/.../turn-finished` 接收。旧 IM 流不受影响。
4. **cc-broker 部署** —— 含 metadata 透传、permission gate、`tools/im.go` 短路、turn-finished 回调。metadata 全 nil 时走 default 行为。
5. **agent 二进制发布** —— 加 `tui` 子命令。

每步可独立 rollback：1 是 ADD COLUMN；2/3/4 重启后旧请求格式仍可解析；5 用户不升级则没事。

**关键依赖：** cc-broker 必须先于 agent 二进制（步骤 4 在 5 之前），否则用户启动 `tui` 后发出的 turn 会因 cc-broker 不识别 metadata 走老路径——结果功能上仍能工作（permission_mode 默认 bypass），但 TUI 的权限确认体验失效。

### 9.4 用户文档变更

`README.md` 在 "Local Agent Tunneling" 节后加 "Interactive TUI" 子节，3 段：(1) `agentserver tui` 启动；(2) 与 `connect` / `executor` 区别；(3) 必要 flag。

---

## 10. 已知风险

| 风险 | 严重 | 缓解 |
|---|---|---|
| **Bubble Tea 在 Windows 终端兼容性** | 中 | Windows Terminal / wezterm / mintty 已测良好；老 cmd.exe 不官方支持，文档注明 |
| **LLM 不遵循 preferred_executor 提示** | 中 | system prompt 强提示 + tool_use 块明显标注 + 用户可拒绝；v1.x 可加硬规则"非 preferred 强制问"，但破坏 LLM 灵活性 |
| **cc-broker permission gate 是 in-memory，多副本不共享 sticky** | 低 | turn lock 已保证同 session 落同副本；唯一影响是 cc-broker 重启后 sticky 丢失 |
| **`always` 规则匹配粒度** | 中 | v1 改用 bash 命令前两 token + 路径 dirname（§3.4）：批准 `git status` 不放行 `git push`，批准 `kubectl get` 不放行 `kubectl delete`。残留风险：`bash -c "rm -rf /"` 与 `bash -c "ls"` 同 head——bash -c / sh -c / zsh -c 这类带"嵌套 shell"的命令永远不应 always；TUI 在权限面板上检测到这类前缀时**禁用 `[a] always` 选项**作为兜底。v1.x 加规则编辑面板 |
| **跨机器接力时上下文连续性** | 中 | 依赖 cc-broker 已有的 OpenViking session .jsonl 上传；TurnLock 序列化 + Teardown 阻塞下一 Setup |
| **SSE 长连接经过反代时被超时关闭** | 中 | server 端 15s keepalive；客户端 30s 无帧重连 |
| **多 operator 抢占可能意外失控** | 低 | `permission_responder_lost` 显著提示；用户可立即 `/take-control` 抢回 |
| **本地 `/cd` 走文件协调违反解耦原则** | 低 | §5.10 已注明；如担心可改 B 方案补 tunnel 控制流 |
| **`claude-agent-sdk-go` API 不稳定** | 中 | vendored，pin commit；CI 跑 `go test ./...` |
| **跨用户 v1 硬禁可能体验差** | 中 | 用户反馈强则提前实现 v1.x 双签 |

---

## 附录 A：文件清单总览

### A.1 新增文件

```
cmd/agentserver-agent/main.go                     # 加 tuiCmd
internal/agent/tui.go                              # RunTUI(opts) 入口
internal/agent/tui/                                # 新包
  model.go                                         # Bubble Tea Model
  update.go                                        # Update 主分发
  view.go                                          # View 渲染
  msg.go                                           # Msg 类型
  keymap.go                                        # 键位
  bus.go                                           # HTTP/SSE 客户端
  sse.go                                           # SSE consumer
  timeline.go                                      # Timeline 数据结构
  panels.go                                        # permission/ask_user 浮层
  cmds.go                                          # slash 命令解析
  attach_picker.go                                 # /attach 文件选择
  styles.go                                        # lipgloss styles
  *_test.go

internal/ccbroker/tools/permission.go              # Gate 实现
internal/ccbroker/tools/prompt.go                  # system prompt builder
```

### A.2 修改文件

```
internal/agent/executor_session.go                 # ExecutorSession 加 runtime_cwd 字段（optional）

internal/ccbroker/handler_turns.go                 # 解析 metadata，turn_kind=compaction 处理，
                                                   # turn-finished 回调 agentserver
internal/ccbroker/runner/options.go                # metadata → SDK options
internal/ccbroker/tools/context.go                 # Context 加 PermissionMode/CreatorUserID/
                                                   # PreferredExecutorID/Gate 等字段
internal/ccbroker/tools/executor.go                # 每个 remote_* handler 入口套 Gate.Check
internal/ccbroker/tools/im.go                      # send_message/send_image/send_file 在
                                                   # IMChannelID="" (TUI session) 时短路
internal/ccbroker/server.go                        # 注册新转发端点 + 内部回调路径

internal/server/server.go                          # 挂载新 TUI 路由 group
internal/server/handler_tui_inbound.go             # 新文件: POST /api/workspaces/{wid}/tui/inbound
internal/server/handler_tui_session.go             # 新文件: POST /api/agent-sessions, attach
internal/server/handler_tui_events.go              # 新文件: GET /api/agent-sessions/{sid}/events
internal/server/handler_tui_control.go             # 新文件: POST /control
internal/server/handler_tui_cancel.go              # 新文件: POST /turns/{tid}/cancel
internal/server/handler_tui_decision.go            # 新文件: POST /permissions/{pid}
internal/server/handler_tui_executor_status.go     # 新文件: GET /api/executors/{id}/status (反代)
internal/server/handler_tui_internal.go            # 新文件: POST /internal/sessions/{sid}/turn-finished
internal/server/leak_worker.go                     # 新文件: 后台 active_turn_id 兜底清理 + responder TTL 兜底

internal/db/agent_sessions.go                      # 加 channel_type/preferred_model/permission_mode
                                                   # /preferred_executor_id/permission_responder/
                                                   # responder_attached_at/active_turn_id 字段及 query
```

### A.3 数据库迁移

```
internal/db/migrations/021_tui_session_fields.sql               # agentserver DB
internal/executorregistry/migrations/002_executor_owner_sharing.sql  # executor-registry DB
```

cc-broker DB 无 schema 变化。

---

## 附录 B：开放问题（实施时再决）

1. **`/cd` A 方案 vs B 方案** —— v1 默认 A（文件协调），如团队认为应严格解耦改 B。
2. **R 类命令清单的 server-side 注册** —— v1 硬编码 `[model, permission, compact, cost, agents]`，v1.x 加 `GET /api/control/commands` 让 server 报清单，TUI 动态拉取。
3. **`agent_session_permission_pending` 表落库时机** —— v1 内存，v1.x 引入 cc-broker 多副本时一并落库。
4. **跨用户双签 UX 细节** —— v1 钩子已留好，v1.x 单独设计 personal SSE + dual-consent UI。
