# Agent Info Collection & Display

## Overview

Collect local agent system information via gopsutil v4 and display it in the sandbox list UI.

## Decisions

- **采集时机**: 每次隧道连接时上报（WebSocket 连接成功后发送）
- **展示位置**: 在现有 SandboxList 卡片中展开显示
- **opencode 版本获取**: 通过 opencode API 获取，失败留空，前端显示 "Unknown"
- **系统信息库**: github.com/shirou/gopsutil/v4，字段名与其一致
- **存储策略**: 单独 agent_info 表，主要指标为独立列，详细数据存 JSONB

## Database Schema

```sql
CREATE TABLE agent_info (
    sandbox_id         TEXT PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,

    -- 主要指标
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

    -- 详细指标 (gopsutil 原始结构)
    host_info          JSONB,
    cpu_info           JSONB,
    memory_info        JSONB,
    disk_info          JSONB,

    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

## Data Flow

```
Agent 启动
  → gopsutil 采集系统信息 (host/cpu/mem/disk)
  → 尝试调 opencode API 获取版本（失败留空）
  → WebSocket 连接 /api/tunnel/{id}?token=xxx
  → 连接成功后发送文本消息: {"type":"agent_info","data":{...}}
  → Server 隧道读循环识别该类型，调用 UpsertAgentInfo() 写入 DB
  → GET /api/sandboxes/{id} 或列表接口返回 agent_info 字段
  → 前端卡片展开显示系统信息
```

## Tunnel Protocol Extension

Agent 连接成功后发送一条 JSON 文本消息:
```json
{"type": "agent_info", "data": {...}}
```
Server 在隧道读循环中区分文本消息和二进制帧，文本消息走 agent_info 处理，不影响现有 HTTP 代理。

## API Response

sandboxResponse 新增:
```go
AgentInfo *AgentInfoResponse `json:"agent_info,omitempty"` // 仅 is_local=true
```

## Frontend

SandboxList.tsx 对 is_local 沙箱卡片添加 chevron 展开按钮，展开后显示系统信息网格。内存/磁盘格式化为人类可读单位。opencode_version 为空时显示 "Unknown"。

## Files to Change

| File | Change |
|------|--------|
| `go.mod` | 新增 gopsutil/v4 依赖 |
| `internal/db/migrations/002_agent_info.sql` | 建表 |
| `internal/db/agent_info.go` | UpsertAgentInfo() + GetAgentInfo() |
| `internal/agent/client.go` | collectAgentInfo(), 隧道连接后发送 |
| `internal/tunnel/protocol.go` | agent_info 消息类型常量 |
| `internal/server/tunnel.go` | 读循环处理 agent_info 文本消息 |
| `internal/server/server.go` | sandboxResponse + toSandboxResponse 扩展 |
| `web/src/lib/api.ts` | AgentInfo 类型定义 |
| `web/src/components/SandboxList.tsx` | 可展开系统信息面板 |
