# Custom Agent Protocol Specification

This document describes the full protocol for connecting a custom agent to agentserver. Custom agents can be written in any language that supports WebSocket and yamux.

## Table of Contents

1. [Overview](#1-overview)
2. [Authentication](#2-authentication)
3. [Registration](#3-registration)
4. [Tunnel Protocol](#4-tunnel-protocol)
5. [Stream Types](#5-stream-types)
6. [HTTP Proxy](#6-http-proxy)
7. [Task Execution](#7-task-execution)
8. [Agent Discovery](#8-agent-discovery)
9. [Error Handling](#9-error-handling)

---

## 1. Overview

A **custom agent** is a process you run anywhere — your laptop, a server, a container — that connects back to agentserver over a persistent WebSocket tunnel. Once connected, agentserver can:

- **Proxy HTTP requests** to your agent so users can access a Web UI through the platform subdomain `code-{shortID}.{baseDomain}`.
- **Dispatch tasks** to your agent from other agents or the Web UI, using the task executor API.

Custom agents use the same tunnel infrastructure as built-in opencode/claudecode agents, with no differences in the transport layer. The distinction is that custom agents skip platform-managed authentication injection — your HTTP handler receives requests exactly as the platform received them from the user's browser.

### Modes of Operation

| Mode | What it does | Required |
|------|-------------|----------|
| HTTP service | Serves a Web UI through the subdomain proxy | Optional |
| Task executor | Polls and executes assigned tasks | Optional |

Both modes can be active simultaneously. An agent that handles neither is still valid (heartbeats keep it online), but not useful.

---

## 2. Authentication

Custom agents authenticate via the **OAuth 2.0 Device Authorization Grant** (RFC 8628). This lets a headless process ask a human to authenticate in a browser without needing to handle a redirect.

### Step 1 — Request a device code

```
POST /api/oauth2/device/auth
Content-Type: application/x-www-form-urlencoded

client_id=agentserver-agent-cli&scope=openid%20profile%20agent%3Aregister
```

Response `200 OK`:

```json
{
  "device_code": "GmRhmhcxhwAzkoEqiMEg_DnyEysNkuNhszIySk9eS",
  "user_code": "WDJB-MJHT",
  "verification_uri": "https://agent.example.com/device",
  "verification_uri_complete": "https://agent.example.com/device?user_code=WDJB-MJHT",
  "expires_in": 1800,
  "interval": 5
}
```

Display `verification_uri_complete` (or a QR code of it) to the user. The user visits the URL in a browser and approves access.

### Step 2 — Poll for the access token

Poll at the interval returned by step 1 (minimum 5 seconds):

```
POST /api/oauth2/token
Content-Type: application/x-www-form-urlencoded

grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Adevice_code
  &client_id=agentserver-agent-cli
  &device_code=GmRhmhcxhwAzkoEqiMEg_DnyEysNkuNhszIySk9eS
```

**While waiting**, the server returns one of these error codes:

| `error` value | Meaning | Action |
|---------------|---------|--------|
| `authorization_pending` | User has not yet acted | Continue polling |
| `slow_down` | Polling too fast | Add 5 seconds to the interval, then continue |
| `access_denied` | User denied access | Stop, show error |
| `expired_token` | Device code expired | Stop, restart from step 1 |

**On success** (`200 OK`):

```json
{
  "access_token": "eyJhbGci...",
  "token_type": "Bearer",
  "expires_in": 3600,
  "scope": "openid profile agent:register"
}
```

Store the `access_token`; it is used in all subsequent API calls.

---

## 3. Registration

After authenticating, register your agent to obtain the tunnel and proxy tokens:

```
POST /api/agent/register
Authorization: Bearer {access_token}
Content-Type: application/json

{
  "name": "My Custom Agent",
  "type": "custom"
}
```

Response `201 Created`:

```json
{
  "sandbox_id": "550e8400-e29b-41d4-a716-446655440000",
  "tunnel_token": "a3f1b2c4d5e6f7a8b9c0d1e2f3a4b5c6",
  "proxy_token":  "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d",
  "short_id":     "xk9mTqLpRwZvNcYu",
  "workspace_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
}
```

| Field | Description |
|-------|-------------|
| `sandbox_id` | UUID identifying your agent; used to establish the tunnel |
| `tunnel_token` | Secret for the WebSocket tunnel connection |
| `proxy_token` | Bearer token for task polling and status updates |
| `short_id` | 16-character ID used in the Web UI subdomain |
| `workspace_id` | Workspace this agent was registered under |

Persist these credentials — you do not need to re-register on reconnect. Re-registration creates a new sandbox entry. The proxy subdomain is `code-{shortID}.{baseDomain}`.

---

## 4. Tunnel Protocol

### WebSocket Connection

Connect to the tunnel endpoint using the `sandbox_id` and `tunnel_token` from registration:

```
wss://{server}/api/tunnel/{sandbox_id}?token={tunnel_token}
```

The server validates the token, then upgrades the HTTP connection to WebSocket. The WebSocket frames carry raw bytes of a yamux session.

### yamux Session

Create a **yamux client session** over the WebSocket connection with these settings:

| Setting | Value |
|---------|-------|
| `EnableKeepAlive` | `false` |
| `ConnectionWriteTimeout` | `10s` |
| `AcceptBacklog` | `256` |

Agentserver acts as the yamux server. This means:

- The **server opens** streams toward the agent for HTTP proxy requests and terminal sessions.
- The **agent opens** streams toward the server for heartbeat control messages.

Your agent must call `session.Accept()` in a loop to handle server-initiated streams.

### Stream Header Format

Every yamux stream (in both directions) begins with a 5-byte header followed by variable-length JSON metadata:

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|  stream_type  |         metadata_length (32-bit big-endian)   |
+-+-+-+-+-+-+-+-+                               +-+-+-+-+-+-+-+-+
|                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|           metadata (JSON, metadata_length bytes)              |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

- **`stream_type`** (1 byte): identifies the stream purpose (see table below).
- **`metadata_length`** (4 bytes, big-endian uint32): byte length of the JSON metadata that follows.
- **`metadata`** (variable): JSON object. May be empty (`metadata_length = 0`) for streams with no metadata.

Payload bytes (request body, response body, terminal data) follow the header directly with no additional framing.

---

## 5. Stream Types

| Type | Byte | Direction | Required | Purpose |
|------|------|-----------|----------|---------|
| HTTP | `0x01` | Server → Agent | Yes (if serving HTTP) | Proxied HTTP request |
| Terminal | `0x02` | Server → Agent | No | Interactive terminal session |
| Control | `0x03` | Agent → Server | Yes | Heartbeat / agent info |

### HTTP Stream (0x01)

The server opens this stream type to forward a proxied HTTP request to the agent. See [Section 6](#6-http-proxy) for the full request/response cycle.

**Metadata sent by server (`HTTPStreamMeta`)**:

```json
{
  "method": "GET",
  "path": "/dashboard?tab=summary",
  "headers": {
    "Accept": "text/html",
    "Cookie": "session=abc123",
    "X-Forwarded-For": "203.0.113.1"
  },
  "body_len": 0
}
```

`body_len` is the number of request body bytes that follow the header. For `GET` requests this is `0`.

**Response metadata sent by agent (`HTTPResponseMeta`)**:

```json
{
  "status": 200,
  "headers": {
    "Content-Type": "text/html; charset=utf-8"
  }
}
```

Response body bytes follow the response header immediately. Close the stream when done.

### Terminal Stream (0x02)

The server opens this stream type to establish an interactive terminal session. For custom agents, terminal support is optional.

- **Metadata length is always 0** — no metadata follows the 5-byte header.
- After the header, the stream is **bidirectional raw bytes** (stdin/stdout of a shell or REPL).
- If your agent does not support terminals, **close the stream immediately** after reading the 5-byte header.

### Control Stream (0x03) — Heartbeat

The **agent opens** this stream type toward the server every 20 seconds. Control streams serve as both a keepalive and a metadata refresh.

Send the header with **empty metadata** (metadata_length=0), followed by the agent info JSON as the stream body, then close the stream:

```
[header: type=0x03, metadata_length=0][agent info JSON body]
```

Note: Unlike HTTP streams which put structured data in the metadata field, control streams put the agent info in the stream body. The server reads the body after discarding the empty metadata.

**Minimal required fields**:

```json
{
  "hostname": "dev-laptop",
  "os": "linux"
}
```

**Full optional fields**:

```json
{
  "hostname": "dev-laptop",
  "os": "linux",
  "platform": "ubuntu",
  "platform_version": "22.04",
  "kernel_arch": "x86_64",
  "cpu_model_name": "AMD Ryzen 9 5900X",
  "cpu_count_logical": 16,
  "memory_total": 34359738368,
  "disk_total": 1000204886016,
  "disk_free": 500102443008,
  "agent_version": "1.0.0",
  "workdir": "/home/dev/project",
  "capabilities": {
    "languages": [
      {"name": "python3", "version": "3.11.0", "path": "/usr/bin/python3"}
    ],
    "tools": [
      {"name": "docker", "version": "24.0.0", "path": "/usr/bin/docker"}
    ]
  }
}
```

The server uses this to:
- Update `last_heartbeat_at` for liveness tracking (agent appears "offline" after 60 seconds without a heartbeat).
- Store agent metadata in the `agent_info` table.
- Populate the agent discovery card from `capabilities`.

Send the first heartbeat **immediately** on connect, then repeat every 20 seconds.

---

## 6. HTTP Proxy

### How Routing Works

When a user visits `code-{shortID}.{baseDomain}`, the sandboxproxy service:

1. Extracts `shortID` from the subdomain.
2. Looks up the sandbox in the database.
3. Finds the active yamux session in the tunnel registry.
4. Opens a new yamux stream, sends the HTTP request over it, and streams the response back.

There is no network connection between the sandboxproxy and your agent — everything flows through the tunnel.

### Request Lifecycle

```
User Browser          SandboxProxy          Agent (via tunnel)
     |                     |                      |
     |-- GET /dashboard --> |                      |
     |                     |-- open yamux stream ->|
     |                     |   [HTTP stream header]|
     |                     |   [HTTPStreamMeta JSON]
     |                     |   [request body bytes]|
     |                     |                      |
     |                     |<-- [HTTP stream header]
     |                     |    [HTTPResponseMeta JSON]
     |                     |    [response body bytes]
     |<-- 200 OK + body ---|                      |
```

### Important Behaviors

- **No auth injection**: For custom agents, the sandboxproxy does not inject any `Authorization` header. Your handler receives requests exactly as the user sent them.
- **Request timeout**: 120 seconds total. Long-running responses must send at least one byte within this window.
- **Streaming**: The sandboxproxy flushes response body chunks to the browser as they arrive. SSE and chunked transfer encoding work correctly.
- **WebSocket upgrades**: Not supported. The yamux stream is not a raw TCP pipe — HTTP/1.1 upgrade requests will not work.

### Implementing the HTTP Handler

For each accepted stream of type `0x01`:

1. Read 5-byte header, confirm `stream_type == 0x01`.
2. Read `metadata_length` bytes, parse as `HTTPStreamMeta` JSON.
3. Read exactly `body_len` bytes of request body (0 for GET).
4. Process the request.
5. Write a 5-byte header with `stream_type = 0x01` and the length of the `HTTPResponseMeta` JSON.
6. Write the `HTTPResponseMeta` JSON.
7. Write the response body bytes.
8. Close the stream.

---

## 7. Task Execution

### Overview

Tasks are units of work dispatched to your agent by other agents or the Web UI. The task system uses HTTP polling — your agent periodically asks the server for work.

Task polling uses the `proxy_token` from registration (not the OAuth access token).

### Polling for Tasks

```
GET /api/agent/tasks/poll
Authorization: Bearer {proxy_token}
```

**Response `200 OK`** — a task is available:

```json
{
  "task_id": "task_abc123",
  "skill": "code_review",
  "prompt": "Review this PR for security issues",
  "system_context": "You are a security-focused code reviewer",
  "timeout_seconds": 300,
  "created_at": "2026-04-15T10:00:00Z"
}
```

**Response `204 No Content`** — no tasks currently available.

The server atomically marks the task as `assigned` on return, preventing double-pickup.

### Recommended Poll Interval

- Poll every **5 seconds** when idle.
- Poll **immediately** after completing a task (to pick up the next one).
- Back off to **30 seconds** after 5 minutes of consecutive idle responses.

### Reporting Task Status

Use `PUT /api/agent/tasks/{task_id}/status` with `Authorization: Bearer {proxy_token}`.

**Mark as running** (do this before starting work):

```json
{ "status": "running" }
```

**Mark as completed**:

```json
{
  "status": "completed",
  "output": "Found 2 potential SQL injection vulnerabilities in auth.go lines 47 and 83.",
  "cost_usd": 0.0042,
  "num_turns": 3
}
```

**Mark as failed**:

```json
{
  "status": "failed",
  "failure_reason": "LLM API timeout after 300 seconds"
}
```

Valid status transitions: `assigned` → `running` → `completed` | `failed`.

### Task Creation (by other agents or Web UI)

Other agents or the Web UI can send tasks to your agent:

```
POST /api/workspaces/{workspace_id}/tasks
Authorization: Bearer {access_token}   (or session cookie)
Content-Type: application/json

{
  "target_id": "{your_sandbox_id}",
  "skill": "code_review",
  "prompt": "Review this PR for security issues",
  "timeout_seconds": 300
}
```

---

## 8. Agent Discovery

Declare your agent's capabilities so that the Web UI and other agents can discover what your agent does and how to interact with it.

### Register a Discovery Card

```
POST /api/agent/discovery/cards
Authorization: Bearer {proxy_token}
Content-Type: application/json

{
  "display_name": "Code Review Bot",
  "description": "Automated code review powered by a custom LLM pipeline",
  "agent_type": "custom",
  "card": {
    "skills": ["code_review", "lint", "security_scan"],
    "accepts_tasks": true,
    "has_web_ui": true,
    "version": "1.0.0"
  }
}
```

| Field | Effect |
|-------|--------|
| `skills` | Skills this agent can handle (matched against task `skill` field) |
| `accepts_tasks` | If `true`, the Web UI shows this agent as a valid task target |
| `has_web_ui` | If `true`, the Web UI shows a link to `code-{shortID}.{baseDomain}` |

### Online/Offline Status

The platform determines agent availability based on heartbeat recency:

| Condition | Status |
|-----------|--------|
| Last heartbeat within 60 seconds | `available` |
| Last heartbeat older than 60 seconds | `offline` |

### Query Agents in a Workspace

```
GET /api/workspaces/{workspace_id}/agents
Authorization: Bearer {access_token}

Response 200:
[
  {
    "agent_id": "550e8400-e29b-41d4-a716-446655440000",
    "display_name": "Code Review Bot",
    "description": "Automated code review...",
    "agent_type": "custom",
    "status": "available",
    "card": { "skills": ["code_review"], "accepts_tasks": true, "has_web_ui": true, "version": "1.0.0" }
  }
]
```

---

## 9. Error Handling

### Reconnection Strategy

The tunnel is a persistent connection — network disruptions, server restarts, and deployments will cause disconnects. Implement automatic reconnection with exponential backoff:

| Attempt | Wait before retry |
|---------|------------------|
| 1st | 1 second |
| 2nd | 2 seconds |
| 3rd | 4 seconds |
| 4th | 8 seconds |
| ... | doubles each time |
| Max | 60 seconds |

**Reset the backoff to 1 second** if the previous connection stayed up for more than 30 seconds. This prevents transient network blips from causing long waits.

### Registration Persistence

Do not re-register on every connection. Persist the `sandbox_id`, `tunnel_token`, and `proxy_token` to disk and reuse them across reconnects. This keeps your agent's subdomain (`code-{shortID}.{baseDomain}`) stable.

### Token Expiry

OAuth access tokens (`access_token`) expire. The `proxy_token` and `tunnel_token` from registration do not expire — they are valid for the lifetime of the sandbox. If a registration API call returns `401`, re-authenticate via Device Flow and register again.

### Timeouts

| Operation | Timeout |
|-----------|---------|
| WebSocket dial | Use context cancellation |
| yamux stream write | 10 seconds (yamux `ConnectionWriteTimeout`) |
| HTTP request (per request via tunnel) | 120 seconds |
| Task poll HTTP request | 10 seconds |
| Task status update | No explicit timeout (use context) |

### Common Error Scenarios

**`401 Unauthorized` on `/api/tunnel/...`**: The `tunnel_token` is wrong or the sandbox does not exist. Re-register.

**yamux session closed with `io: read/write on closed pipe`**: The server closed the session (e.g., restart or idle timeout). Reconnect.

**Task poll returns `401`**: The `proxy_token` is invalid. Re-register.

**`metadata too large`**: Stream metadata exceeded 1 MB. This should not happen in normal operation; file a bug.
