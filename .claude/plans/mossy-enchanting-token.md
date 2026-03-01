# Plan: 清理非 OpenCode 相关组件

## Context

项目从最初的多功能平台（终端 + Chat AI + OpenCode IDE）聚焦为纯 OpenCode sandbox 服务。需要清理掉 sidecar（Python/TypeScript chat 服务）、agent-server（Node.js Claude SDK）、chat 相关 API/数据库/前端组件、Redis 依赖等。保留终端 WebSocket 功能。

## 删除的目录（整目录 rm -rf）

| 目录 | 用途 |
|------|------|
| `sidecar/` | Python FastAPI chat 服务 |
| `sidecar-ts/` | TypeScript chat 服务 |
| `agent-server/` | Node.js Claude SDK wrapper |

## 删除的文件

| 文件 | 原因 |
|------|------|
| `Dockerfile.agent` | 旧的多用途 agent Dockerfile，已被 `Dockerfile.opencode` 替代 |
| `internal/db/messages.go` | chat 消息查询代码 |
| `web/src/components/chat/` (整目录) | chat UI 组件（Chat.tsx, ChatInput.tsx, MessageBubble.tsx, MessageRenderer.tsx, ThinkingBlock.tsx, ToolCard.tsx），App.tsx 未引用 |

## 修改的文件

### 1. `internal/server/server.go`

**删除 SidecarProxy 相关代码：**
- struct 字段 `SidecarProxy *httputil.ReverseProxy` (line 37)
- `New()` 中 sidecar URL 解析和 proxy 创建 (lines 46-53)
- struct 初始化中 `SidecarProxy: proxy` (line 69)
- 路由中 chat/messages 端点 (lines 164-170):
  - `r.Get("/api/sessions/{id}/messages", ...)`
  - `r.Post("/api/sessions/{id}/chat", ...)`
  - `r.Get("/api/sessions/{id}/stream", ...)`
  - `r.Delete("/api/sessions/{id}/stream", ...)`
- handler 方法 (lines 576-632):
  - `handleListMessages()`
  - `proxySidecar()`
  - `handleChatProxy()`
  - `handleStreamProxy()`
  - `handleStreamDeleteProxy()`

**清理 imports：**
- 删除不再需要的 `"net/url"` 和 `"net/http/httputil"` imports

### 2. `web/src/lib/api.ts`

删除 chat 相关类型和函数 (lines 90-159):
- `StreamEvent`, `ToolPayload`, `Message`, `StreamEnvelope` 类型
- `getMessages()`, `sendMessage()`, `createEventSource()`, `stopStream()` 函数

### 3. `docker-compose.yml`

- 删除整个 `redis` service (lines 13-16)
- 删除整个 `sidecar` service (lines 18-33)
- 删除 `server` service 中的 `SIDECAR_URL` 环境变量 (line 42)
- 删除 `server` service 的 `depends_on` 中的 `redis` 和 `sidecar`

### 4. `deploy/helm/cli-server/values.yaml`

- 删除 `sidecar` 配置块 (lines 8-10)
- 删除 `redis` 配置块 (lines 14-15)

### 5. `deploy/helm/cli-server/templates/deployment.yaml`

- 删除 cli-server container 中的 `SIDECAR_URL` env (lines 71-72)
- 删除 cli-server container 中的 `REDIS_URL` env (lines 73-74)
- 删除整个 sidecar container 定义 (lines 150-187)

## 新建的文件

### `internal/db/migrations/007_drop_chat_tables.sql`

```sql
DROP TABLE IF EXISTS message_events;
DROP TABLE IF EXISTS messages;
```

## 验证

1. `go build ./...` 编译通过
2. 确认删除后无悬空 import 或未引用代码
3. `cd web && npm run build` 前端编译通过
