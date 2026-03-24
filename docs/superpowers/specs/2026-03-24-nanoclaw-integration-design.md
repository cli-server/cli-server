# NanoClaw Integration Design Spec

## Overview

Add NanoClaw as a third sandbox type in agentserver, alongside opencode and openclaw. NanoClaw is a lightweight AI assistant platform built on Claude Agent SDK with multi-channel messaging support (WhatsApp, Telegram, Discord, Slack, etc.).

The integration reuses agentserver's existing sandbox lifecycle management, llmproxy for API key injection, and iLink backend for WeChat support. WeChat messages are bridged through agentserver rather than having NanoClaw connect directly to iLink.

**Source:** https://github.com/qwibitai/nanoclaw

## Architecture

```
┌──────────────────────────────────────────────────┐
│                  agentserver                      │
│                                                   │
│  ┌─────────┐  ┌──────────┐  ┌─────────────────┐ │
│  │ Web UI  │  │ llmproxy │  │ iLink Backend   │ │
│  │ (React) │  │ (rate    │  │ (WeChat QR +    │ │
│  │         │  │  limit)  │  │  msg bridge)    │ │
│  └────┬────┘  └────┬─────┘  └───────┬─────────┘ │
│       │            │                │             │
│       ▼            ▼                ▼             │
│  ┌─────────────────────────────────────────────┐ │
│  │          Sandbox Manager (K8s)               │ │
│  │  type: "opencode" | "openclaw" | "nanoclaw" │ │
│  └──────────────────┬──────────────────────────┘ │
└─────────────────────┼────────────────────────────┘
                      │
          ┌───────────┼───────────┐
          ▼           ▼           ▼
    ┌──────────┐ ┌──────────┐ ┌──────────────┐
    │ opencode │ │ openclaw │ │  nanoclaw    │
    │ Pod      │ │ Pod      │ │  Pod         │
    │          │ │          │ │              │
    │ opencode │ │ openclaw │ │ NanoClaw     │
    │ binary   │ │ gateway  │ │ orchestrator │
    │          │ │ + plugins│ │ + channels   │
    └──────────┘ └──────────┘ │ + Agent SDK  │
                               └──────────────┘
```

## Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Integration level | Full (same as openclaw) | User requirement |
| Web UI proxy | Not needed | NanoClaw interacts via messaging channels, no Web UI |
| Agent execution | Direct Claude Agent SDK | K8s Pod provides isolation; no Docker daemon available inside Pod |
| API key injection | Via llmproxy | Consistent with openclaw; rate limiting and key management |
| Container image | Project-provided Dockerfile.nanoclaw | Pre-installs weixin channel and dependencies |
| WeChat messages | Bridged via agentserver | Centralizes iLink credential management; NanoClaw doesn't hold iLink secrets |

## Components

### 1. Sandbox Type System Extension

**File: `internal/sandbox/config.go`**

Add new config fields:

```go
type Config struct {
    // ... existing fields ...
    NanoclawImage            string
    NanoclawRuntimeClassName string
    NanoclawWeixinEnabled    bool
}
```

New environment variables:
- `NANOCLAW_IMAGE` — container image for NanoClaw pods
- `NANOCLAW_RUNTIME_CLASS` — optional K8s runtime class
- `NANOCLAW_WEIXIN_ENABLED` — enable WeChat channel in NanoClaw pods

Add `BuildNanoclawConfig()` function that generates NanoClaw's `.env` content:

```go
func BuildNanoclawConfig(proxyBaseURL, proxyToken, assistantName string, weixinBridgeURL string) string {
    // ANTHROPIC_BASE_URL=<llmproxy URL>
    // ANTHROPIC_API_KEY=<per-sandbox proxy token>
    // ASSISTANT_NAME=<configurable>
    // NANOCLAW_NO_CONTAINER=true
    // NANOCLAW_WEIXIN_BRIDGE_URL=<agentserver webhook URL>  (if weixin enabled)
}
```

**File: `internal/server/server.go`**

- Extend sandbox type validation: `"opencode" | "openclaw" | "nanoclaw"`
- Add nanoclaw branch in sandbox creation logic
- Route weixin handlers to work with nanoclaw sandboxes (currently openclaw-only)

### 2. Dockerfile.nanoclaw

```dockerfile
FROM node:20-slim AS builder

WORKDIR /app

# Install Claude Code CLI (for Agent SDK)
RUN npm install -g @anthropic-ai/claude-code

# Clone NanoClaw source
RUN apt-get update && apt-get install -y git && \
    git clone https://github.com/qwibitai/nanoclaw.git . && \
    npm ci && npm run build

# Copy weixin channel implementation
COPY nanoclaw-weixin-channel/ src/channels/weixin/

# Rebuild with weixin channel
RUN npm run build

FROM node:20-slim

WORKDIR /app
COPY --from=builder /app /app
COPY --from=builder /usr/local/lib/node_modules/@anthropic-ai /usr/local/lib/node_modules/@anthropic-ai
COPY --from=builder /usr/local/bin/claude /usr/local/bin/claude

# NanoClaw data directories
RUN mkdir -p /app/store /app/groups /app/data

CMD ["node", "dist/index.js"]
```

### 3. Agent Execution Adaptation

NanoClaw's `container-runner.ts` spawns Docker containers to run agents. In K8s Pod mode (no Docker daemon), this must be adapted.

**Approach:** When `NANOCLAW_NO_CONTAINER=true`, modify `container-runner.ts` to run the agent-runner as a direct child process instead of inside a Docker container.

Key changes:
- Skip Docker-specific logic (volume mounts, container networking)
- Spawn `agent-runner` directly via `child_process.spawn()`
- Credential proxy becomes transparent (ANTHROPIC_BASE_URL already points to llmproxy)
- Group folder isolation maintained via filesystem permissions (Pod-level)

This adaptation should be minimal — NanoClaw's agent-runner is already a standalone Node.js script that reads stdin JSON and writes stdout JSON.

### 4. WeChat Channel for NanoClaw

A new channel implementation within the NanoClaw container that receives/sends WeChat messages via agentserver as a bridge.

**File: `nanoclaw-weixin-channel/index.ts`** (copied into container at build time)

```typescript
import { registerChannel } from '../registry.js';
import { Channel, ChannelOpts, NewMessage } from '../../types.js';
import http from 'http';

class WeixinChannel implements Channel {
    name = 'weixin';
    private server: http.Server;
    private opts: ChannelOpts;
    private bridgeURL: string;  // agentserver webhook URL for outbound messages
    private connected = false;

    constructor(opts: ChannelOpts, bridgeURL: string) {
        this.opts = opts;
        this.bridgeURL = bridgeURL;
        // HTTP server to receive inbound messages from agentserver
        this.server = http.createServer((req, res) => {
            // POST /message — receive message from agentserver bridge
            // POST /metadata — receive chat metadata updates
        });
    }

    async connect(): Promise<void> {
        await new Promise<void>(resolve => this.server.listen(3002, resolve));
        this.connected = true;
    }

    async sendMessage(jid: string, text: string): Promise<void> {
        // HTTP POST to agentserver bridge URL with {jid, text}
        // agentserver forwards to iLink API → WeChat user
    }

    isConnected(): boolean { return this.connected; }
    ownsJid(jid: string): boolean { return jid.startsWith('weixin:'); }
    async disconnect(): Promise<void> { this.server.close(); }
}

registerChannel('weixin', (opts) => {
    const bridgeURL = process.env.NANOCLAW_WEIXIN_BRIDGE_URL;
    if (!bridgeURL) return null;
    return new WeixinChannel(opts, bridgeURL);
});
```

### 5. WeChat Message Bridge (agentserver side)

Extend agentserver's existing iLink integration to bridge messages between iLink and NanoClaw pods.

**New endpoints:**

```
POST /api/sandboxes/{id}/weixin/message-callback
```
Called by iLink when a WeChat user sends a message. Agentserver forwards to the NanoClaw pod's weixin channel HTTP endpoint.

```
POST /api/sandboxes/{id}/weixin/send
```
Called by the NanoClaw pod's weixin channel to send a reply. Agentserver forwards to iLink API.

**Message flow (inbound):**
1. WeChat user sends message
2. iLink calls agentserver webhook
3. agentserver looks up sandbox by weixin binding
4. agentserver HTTP POSTs to NanoClaw pod IP:3002/message
5. NanoClaw weixin channel delivers message to orchestrator

**Message flow (outbound):**
1. NanoClaw agent generates reply
2. Weixin channel POSTs to agentserver bridge URL
3. agentserver calls iLink API to send message to WeChat user

**Reused components:**
- `internal/weixin/ilink.go` — iLink API client (QR code generation, message sending)
- `internal/db/weixin_bindings.go` — binding persistence
- `WeixinLoginModal.tsx` — QR code scanning UI (works unchanged)
- Sandbox weixin handlers (QR start/wait) — extended to support nanoclaw type

### 6. K8s Pod Management

Extend existing sandbox K8s management to handle nanoclaw pods.

**Pod spec differences from openclaw:**
- Image: `NanoclawImage` (from config)
- No gateway port exposure (no Web UI)
- Environment variables: `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`, `NANOCLAW_NO_CONTAINER=true`, `ASSISTANT_NAME`, `NANOCLAW_WEIXIN_BRIDGE_URL`
- PVC mount for SQLite data persistence (`/app/store`, `/app/data`, `/app/groups`)
- Exposed port 3002 (weixin channel HTTP endpoint, cluster-internal only)

**No reverse proxy needed:** Unlike openclaw which needs subdomain routing for its Control UI, nanoclaw has no Web UI. All interaction happens through messaging channels.

**Health check:** HTTP GET on a lightweight endpoint (to be added to NanoClaw) or TCP check on port 3002.

### 7. Frontend Changes

**Sandbox creation form (`web/src/components/`):**
- Add "NanoClaw" option to sandbox type selector
- NanoClaw-specific config: assistant name (default "Andy")

**Sandbox detail view (`SandboxDetail.tsx`):**
- Show NanoClaw status, uptime
- WeChat binding section (reuse existing `WeixinLoginModal` and binding display)
- No "Open UI" button (NanoClaw has no Web UI)
- Show connected messaging channels

**API client (`api.ts`):**
- No new API endpoints needed beyond the weixin bridge endpoints (covered above)

## File Changes Summary

| File | Change |
|------|--------|
| `internal/sandbox/config.go` | Add NanoClaw config fields, `BuildNanoclawConfig()` |
| `internal/server/server.go` | Add nanoclaw type validation, creation logic, weixin bridge endpoints |
| `internal/weixin/ilink.go` | Add message send/receive functions (extend existing QR-only client) |
| `internal/sandboxproxy/` | No changes (no Web proxy for nanoclaw) |
| `Dockerfile.nanoclaw` | New file — NanoClaw container image with weixin channel |
| `nanoclaw-weixin-channel/` | New directory — weixin channel TypeScript implementation |
| `nanoclaw-patches/` | New directory — container-runner.ts patch for no-container mode |
| `web/src/components/SandboxDetail.tsx` | Add nanoclaw type handling |
| `web/src/components/CreateSandboxModal.tsx` (or equivalent) | Add nanoclaw option |
| `internal/db/migrations/` | No new migrations needed (existing schema sufficient) |

## Testing Strategy

1. **Unit tests:** `BuildNanoclawConfig()` generates correct .env content
2. **Integration tests:** Weixin bridge endpoints forward messages correctly
3. **Container tests:** Dockerfile.nanoclaw builds successfully, NanoClaw starts in no-container mode
4. **E2E:** Create nanoclaw sandbox → WeChat QR bind → send/receive message

## Open Questions

1. **iLink message webhook format:** Need to verify the exact webhook payload format from iLink for message push (current integration only uses QR scan flow, not message forwarding)
2. **NanoClaw health endpoint:** NanoClaw doesn't currently have a health check endpoint — may need to add one or use TCP probe
3. **Session persistence across Pod restarts:** NanoClaw uses SQLite — PVC ensures data survives Pod restarts, but in-memory state (channel connections, polling loop) resets. NanoClaw's startup recovery handles this.
4. **Multi-channel support:** Should the first version support other channels (WhatsApp, Telegram) beyond WeChat? These would require their own credential management in agentserver.
