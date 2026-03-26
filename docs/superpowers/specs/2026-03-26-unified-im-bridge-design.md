# Unified IM Bridge Layer Design

**Date:** 2026-03-26
**Status:** Draft
**Scope:** agentserver (primary), nanoclaw (minor)

## Problem

WeChat and Telegram use different integration patterns in nanoclaw:

- **WeChat**: agentserver long-polls iLink API, bridges messages to nanoclaw pod via HTTP (`:3002`). Credentials and state managed by agentserver.
- **Telegram**: nanoclaw directly polls Telegram Bot API via `grammy` library. Credentials stored in `.env` on the pod.

Since agentserver already fully proxies WeChat interactions, there's no reason Telegram shouldn't follow the same pattern. A unified bridge layer eliminates inconsistency and makes adding future IM channels trivial.

## Solution

Extract a generic `imbridge` package in agentserver with a `Provider` interface. WeChat and Telegram each implement this interface. The generic bridge handles polling lifecycle, message forwarding, group registration, and cursor management. NanoClaw receives messages from all IMs via the same HTTP bridge on `:3002`.

## Architecture

```
                    agentserver
                    ┌──────────────────────────────┐
                    │  imbridge.Bridge              │
                    │  ┌────────────┬─────────────┐ │
                    │  │ WeixinProv │ TelegramProv│ │
                    │  └─────┬──────┴──────┬──────┘ │
                    │        │             │        │
                    │   iLink API    TG Bot API     │
                    └────────┼─────────────┼────────┘
                             │             │
                        ┌────▼─────────────▼────┐
                        │  Generic poll/forward  │
                        │  forwardToNanoClaw()   │
                        │  ensureGroupRegistered()│
                        └───────────┬───────────┘
                                    │ HTTP POST
                                    ▼
                    ┌───────────────────────────────┐
                    │  NanoClaw Pod (:3002)          │
                    │  bridge-server (shared)        │
                    │  ┌──────────┬───────────────┐  │
                    │  │ weixin   │  telegram      │  │
                    │  │ channel  │  channel       │  │
                    │  └──────────┴───────────────┘  │
                    │       ownsJid:       ownsJid:   │
                    │    @im.wechat          @tg      │
                    └───────────────────────────────┘
```

## Provider Interface

```go
package imbridge

type Provider interface {
    // Name returns the provider identifier: "weixin", "telegram".
    Name() string

    // JIDSuffix returns the suffix used to construct chat JIDs: "@im.wechat", "@tg".
    JIDSuffix() string

    // Poll long-polls the IM API for new messages.
    // cursor is opaque state from the previous poll (empty string on first call).
    Poll(ctx context.Context, creds *Credentials, cursor string) (*PollResult, error)

    // Send sends a text message to a user via the IM API.
    // meta carries provider-specific state (e.g., WeChat context_token). May be nil.
    Send(ctx context.Context, creds *Credentials, toUserID, text string, meta map[string]string) error
}

type Credentials struct {
    SandboxID string
    BotID     string
    BotToken  string
    BaseURL   string
}

type PollResult struct {
    Messages      []InboundMessage
    NewCursor     string
    ShouldBackoff time.Duration // >0 means pause before next poll
}

type InboundMessage struct {
    FromUserID string
    SenderName string
    Text       string
    IsGroup    bool              // true for group/supergroup chats
    Metadata   map[string]string // provider-specific state (e.g., weixin context_token)
}
```

## Generic Bridge

Extracted from `weixin/bridge.go`. Manages per-binding poll goroutines for all providers.

```go
type Bridge struct {
    db               BridgeDB
    resolver         SandboxResolver
    exec             ExecCommander
    providers        map[string]Provider            // keyed by provider name
    pollers          map[string]context.CancelFunc  // key: "sandboxID:provider:botID"
    registeredGroups map[string]bool                // key: "sandboxID:chatJID"
    mu               sync.Mutex
}

type BridgeBinding struct {
    Provider     Provider
    Credentials
    Cursor       string
    PodIP        string
    BridgeSecret string
}
```

### Generic Methods (from weixin/bridge.go)

| Method | Behavior |
|--------|----------|
| `StartPoller(binding)` | Starts long-poll goroutine. Stops existing poller for same key first. |
| `StopPoller(sandboxID, provider, botID)` | Cancels a specific poller. |
| `StopPollersForSandbox(sandboxID)` | Cancels all pollers for a sandbox. |
| `pollLoop(ctx, binding)` | Calls `binding.Provider.Poll()`, forwards messages to nanoclaw, manages cursor. Only advances cursor after all messages successfully forwarded. |
| `forwardToNanoClaw(ctx, binding, msg InboundMessage)` | POST to `http://{podIP}:3002/message`. Calls `ensureGroupRegistered` and `ensureChatRegistered` (with `msg.IsGroup`) first. |
| `ensureGroupRegistered(ctx, sandboxID, chatJID)` | Writes IPC JSON to `/app/data/ipc/main/tasks/register-{folder}.json` via ExecSimple. Idempotent (tracks in-memory). |
| `ensureChatRegistered(ctx, podIP, secret, chatJID)` | POST to `http://{podIP}:3002/metadata`. |
| `FindProviderByJID(jid)` | Matches JID suffix to provider. |

### pollLoop Changes

The generic `pollLoop` replaces the WeChat-specific version:

```go
func (b *Bridge) pollLoop(ctx context.Context, binding BridgeBinding) {
    cursor := binding.Cursor
    consecutiveFailures := 0

    for {
        if ctx.Err() != nil { return }

        result, err := binding.Provider.Poll(ctx, &binding.Credentials, cursor)
        if err != nil {
            // Retry with exponential backoff (same logic as current weixin bridge)
            consecutiveFailures++
            if consecutiveFailures >= maxConsecutiveFailures {
                consecutiveFailures = 0
                sleepCtx(ctx, bridgeBackoffDelay)
            } else {
                sleepCtx(ctx, bridgeRetryDelay)
            }
            continue
        }

        if result.ShouldBackoff > 0 {
            sleepCtx(ctx, result.ShouldBackoff)
            continue
        }

        consecutiveFailures = 0

        // Forward messages BEFORE advancing cursor
        allForwarded := true
        for _, msg := range result.Messages {
            // Persist provider-specific metadata
            for k, v := range msg.Metadata {
                b.db.UpsertProviderMeta(
                    binding.SandboxID, binding.Provider.Name(),
                    binding.BotID, msg.FromUserID, k, v,
                )
            }

            chatJID := msg.FromUserID + binding.Provider.JIDSuffix()

            fwdMsg := msg
            fwdMsg.FromUserID = chatJID // replace with suffixed JID
            if err := b.forwardToNanoClaw(ctx, binding, fwdMsg); err != nil {
                allForwarded = false
                break
            }
        }

        if allForwarded && result.NewCursor != "" {
            cursor = result.NewCursor
            b.db.UpdateCursor(binding.SandboxID, binding.Provider.Name(), binding.BotID, cursor)
        }

        if !allForwarded {
            sleepCtx(ctx, bridgeRetryDelay)
        }
    }
}
```

## WeChat Provider

Wraps the existing `weixin/ilink.go` API client (unchanged).

```go
type WeixinProvider struct{}

func (p *WeixinProvider) Name() string      { return "weixin" }
func (p *WeixinProvider) JIDSuffix() string { return "@im.wechat" }

func (p *WeixinProvider) Poll(ctx context.Context, creds *Credentials, cursor string) (*PollResult, error) {
    resp, err := weixin.GetUpdates(ctx, creds.BaseURL, creds.BotToken, cursor)
    if err != nil { return nil, err }

    // Handle API-level errors
    if resp.Ret != 0 || resp.ErrCode != 0 {
        if resp.ErrCode == weixin.SessionExpiredErrCode || resp.Ret == weixin.SessionExpiredErrCode {
            return &PollResult{ShouldBackoff: 5 * time.Minute}, nil
        }
        return &PollResult{ShouldBackoff: bridgeRetryDelay}, nil
    }

    var msgs []InboundMessage
    for _, m := range resp.Msgs {
        if m.FromUserID == "" { continue }
        text := weixin.ExtractText(m)
        if text == "" { continue }

        meta := map[string]string{}
        if m.ContextToken != "" {
            meta["context_token"] = m.ContextToken
        }

        msgs = append(msgs, InboundMessage{
            FromUserID: m.FromUserID,
            SenderName: m.FromUserID,
            Text:       text,
            Metadata:   meta,
        })
    }

    return &PollResult{Messages: msgs, NewCursor: resp.GetUpdatesBuf}, nil
}

func (p *WeixinProvider) Send(ctx context.Context, creds *Credentials, toUserID, text string, meta map[string]string) error {
    contextToken := meta["context_token"] // may be empty
    return weixin.SendTextMessage(ctx, creds.BaseURL, creds.BotToken, toUserID, text, contextToken)
}
```

The `handleNanoclawIMSend` handler queries `im_provider_meta` for the target user and passes all entries as `meta` to `provider.Send()`. WeChat reads `meta["context_token"]`; Telegram ignores `meta`.

## Telegram Provider

```go
type TelegramProvider struct{}

func (p *TelegramProvider) Name() string      { return "telegram" }
func (p *TelegramProvider) JIDSuffix() string { return "@tg" }

func (p *TelegramProvider) Poll(ctx context.Context, creds *Credentials, cursor string) (*PollResult, error) {
    offset, _ := strconv.ParseInt(cursor, 10, 64)

    updates, err := TelegramGetUpdates(ctx, creds.BaseURL, creds.BotToken, offset, 35)
    if err != nil { return nil, err }

    var msgs []InboundMessage
    var maxID int64
    for _, u := range updates {
        if u.Message == nil || u.Message.Text == "" { continue }
        if u.UpdateID > maxID { maxID = u.UpdateID }

        senderName := u.Message.From.FirstName
        if u.Message.From.Username != "" {
            senderName = u.Message.From.Username
        }

        msgs = append(msgs, InboundMessage{
            FromUserID: fmt.Sprintf("%d", u.Message.Chat.ID),
            SenderName: senderName,
            Text:       u.Message.Text,
        })
    }

    newCursor := cursor
    if maxID > 0 {
        newCursor = strconv.FormatInt(maxID+1, 10)
    }

    return &PollResult{Messages: msgs, NewCursor: newCursor}, nil
}

func (p *TelegramProvider) Send(ctx context.Context, creds *Credentials, toUserID, text string, meta map[string]string) error {
    chatID, _ := strconv.ParseInt(toUserID, 10, 64)
    return TelegramSendMessage(ctx, creds.BaseURL, creds.BotToken, chatID, text)
}
```

### Telegram Bot API Client

Pure HTTP client, no third-party dependencies:

```go
// telegram_api.go

const TelegramDefaultBaseURL = "https://api.telegram.org"

func TelegramGetMe(ctx context.Context, baseURL, botToken string) (*TelegramBotInfo, error)
func TelegramGetUpdates(ctx context.Context, baseURL, botToken string, offset int64, timeout int) ([]TelegramUpdate, error)
func TelegramSendMessage(ctx context.Context, baseURL, botToken string, chatID int64, text string) error
func TelegramSendChatAction(ctx context.Context, baseURL, botToken string, chatID int64, action string) error
```

All functions call `https://api.telegram.org/bot{token}/{method}` with JSON body and parse JSON response.

## Database Schema

### New Migration: `010_unified_im_bindings.sql`

```sql
-- Unified IM binding table (replaces sandbox_weixin_bindings)
CREATE TABLE IF NOT EXISTS sandbox_im_bindings (
    id              SERIAL PRIMARY KEY,
    sandbox_id      TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    provider        TEXT NOT NULL,
    bot_id          TEXT NOT NULL,
    user_id         TEXT NOT NULL DEFAULT '',
    bot_token       TEXT,
    base_url        TEXT,
    cursor          TEXT,
    last_poll_at    TIMESTAMPTZ,
    bound_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_im_bindings_sandbox ON sandbox_im_bindings(sandbox_id);
CREATE INDEX idx_im_bindings_provider_bot ON sandbox_im_bindings(provider, bot_id);

-- Provider-specific per-user metadata (replaces weixin_context_tokens)
CREATE TABLE IF NOT EXISTS im_provider_meta (
    sandbox_id  TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    provider    TEXT NOT NULL,
    bot_id      TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    meta_key    TEXT NOT NULL,
    meta_value  TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (sandbox_id, provider, bot_id, user_id, meta_key)
);

-- Migrate existing WeChat data
INSERT INTO sandbox_im_bindings
    (sandbox_id, provider, bot_id, user_id, bot_token, base_url, cursor, last_poll_at, bound_at)
SELECT
    sandbox_id, 'weixin', bot_id, user_id, bot_token, ilink_base_url, get_updates_buf, last_poll_at, bound_at
FROM sandbox_weixin_bindings;

INSERT INTO im_provider_meta
    (sandbox_id, provider, bot_id, user_id, meta_key, meta_value, updated_at)
SELECT
    sandbox_id, 'weixin', bot_id, user_id, 'context_token', context_token, updated_at
FROM weixin_context_tokens;
```

### BridgeDB Interface

```go
type BridgeDB interface {
    CreateIMBinding(sandboxID, provider, botID, userID string) error
    SaveIMCredentials(sandboxID, provider, botID, botToken, baseURL string) error
    GetIMCredentials(sandboxID, provider, botID string) (botToken, baseURL string, err error)
    ListIMBindings(sandboxID, provider string) ([]IMBinding, error)
    GetActiveBindings(provider string) ([]IMBinding, error)
    UpdateCursor(sandboxID, provider, botID, cursor string) error
    UpsertProviderMeta(sandboxID, provider, botID, userID, key, value string) error
    GetProviderMeta(sandboxID, provider, botID, userID, key string) (string, error)
}
```

## API Routes

### New Routes

```
# WeChat (moved under /im/ namespace)
POST /api/sandboxes/{id}/im/weixin/qr-start     → handleIMWeixinQRStart
POST /api/sandboxes/{id}/im/weixin/qr-wait       → handleIMWeixinQRWait

# Telegram
POST /api/sandboxes/{id}/im/telegram/configure   → handleIMTelegramConfigure
DELETE /api/sandboxes/{id}/im/telegram            → handleIMTelegramDisconnect

# Common
GET  /api/sandboxes/{id}/im/bindings             → handleListIMBindings

# NanoClaw outbound (unified, replaces /weixin/send)
POST /api/internal/nanoclaw/{id}/im/send         → handleNanoclawIMSend
```

### `handleIMTelegramConfigure`

```go
func (s *Server) handleIMTelegramConfigure(w http.ResponseWriter, r *http.Request) {
    var req struct {
        BotToken string `json:"bot_token"`
    }
    // 1. Call TelegramGetMe() to validate token
    // 2. CreateIMBinding(sandboxID, "telegram", botInfo.Username, userID)
    // 3. SaveIMCredentials(sandboxID, "telegram", botInfo.Username, req.BotToken, TelegramDefaultBaseURL)
    // 4. StartPoller with TelegramProvider
}
```

### `handleNanoclawIMSend` (unified outbound)

```go
func (s *Server) handleNanoclawIMSend(w http.ResponseWriter, r *http.Request) {
    // 1. Validate bridge secret
    // 2. Parse request: {to_user_id, text} — to_user_id includes JID suffix
    // 3. Find provider by JID suffix
    // 4. Strip JID suffix to get raw user ID
    // 5. Look up credentials from DB
    // 6. Query provider metadata (context_token for WeChat)
    // 7. Call provider.Send(ctx, creds, rawUserID, text, meta)
}
```

### Legacy Route Compatibility

Old routes remain as aliases during transition:

```go
// Redirect old WeChat routes to new ones
r.Post("/api/sandboxes/{id}/weixin/qr-start", s.handleIMWeixinQRStart)
r.Post("/api/sandboxes/{id}/weixin/qr-wait", s.handleIMWeixinQRWait)
r.Post("/api/internal/nanoclaw/{id}/weixin/send", s.handleNanoclawIMSend)
```

## NanoClaw Changes (Minor)

### New: `src/channels/bridge-server.ts`

Shared HTTP server on `:3002` extracted from `weixin/index.ts`:

```typescript
// Singleton HTTP server shared by all bridge channels.
// Receives POST /message and POST /metadata from agentserver,
// routes to the correct channel based on JID suffix.

interface BridgeChannelHandler {
    channelName: string;
    ownsJid(jid: string): boolean;
    opts: ChannelOpts;
}

const handlers: BridgeChannelHandler[] = [];
let server: http.Server | null = null;

export function registerBridgeHandler(handler: BridgeChannelHandler): void;
export async function startBridgeServer(bridgeSecret: string): Promise<void>;
export function stopBridgeServer(): void;
```

The server's `POST /message` handler finds the right channel by calling `handler.ownsJid(msg.chat_jid)`, then invokes `handler.opts.onMessage()`. Same for `POST /metadata`.

### New: `src/channels/telegram/index.ts`

Thin bridge channel (~40 lines):

```typescript
class TelegramBridgeChannel implements Channel {
    name = 'telegram';

    ownsJid(jid: string): boolean {
        return jid.endsWith('@tg');
    }

    async connect(): Promise<void> {
        registerBridgeHandler({ channelName: 'telegram', ownsJid: this.ownsJid, opts: this.opts });
        await ensureBridgeServerStarted(this.bridgeSecret);
    }

    async sendMessage(jid: string, text: string): Promise<void> {
        // POST to NANOCLAW_BRIDGE_URL with {to_user_id: jid, text}
    }

    // isConnected, disconnect delegate to bridge-server
}

registerChannel('telegram', (opts) => {
    const env = readEnvFile(['NANOCLAW_BRIDGE_URL', 'NANOCLAW_BRIDGE_SECRET']);
    if (!env.NANOCLAW_BRIDGE_URL || !env.NANOCLAW_BRIDGE_SECRET) return null;
    return new TelegramBridgeChannel(opts, env.NANOCLAW_BRIDGE_URL, env.NANOCLAW_BRIDGE_SECRET);
});
```

### Modified: `src/channels/weixin/index.ts`

Refactored to use `bridge-server.ts` instead of its own HTTP server. `ownsJid` and `sendMessage` unchanged. The inline `http.createServer` block is removed — replaced by `registerBridgeHandler()` + `ensureBridgeServerStarted()`.

Reads `NANOCLAW_BRIDGE_URL` with fallback to `NANOCLAW_WEIXIN_BRIDGE_URL` for backwards compatibility.

### Modified: `src/channels/index.ts`

```typescript
import './weixin/index.js';
import './telegram/index.js';  // new
```

## Sandbox Config Changes

### `BuildNanoclawConfig`

```go
func BuildNanoclawConfig(proxyBaseURL, proxyToken, assistantName string,
    bridgeURL, bridgeSecret string, byokBaseURL, byokAPIKey string) string {
    // ...
    lines = append(lines, "NANOCLAW_BRIDGE_URL="+bridgeURL)
    lines = append(lines, "NANOCLAW_BRIDGE_SECRET="+bridgeSecret)
    // Backwards compat (remove after all pods updated)
    lines = append(lines, "NANOCLAW_WEIXIN_BRIDGE_URL="+bridgeURL)
}
```

### Config Struct

```go
type Config struct {
    // Replace NanoclawWeixinEnabled with:
    NanoclawIMBridgeEnabled bool   // env: NANOCLAW_IM_BRIDGE_ENABLED
    NanoclawBridgeBaseURL   string // env: NANOCLAW_BRIDGE_BASE_URL (unchanged)
    // Remove: NanoclawWeixinEnabled
}
```

### Server Initialization

```go
// Replace:
//   s.WeixinBridge = weixin.NewBridge(db, resolver, exec)
// With:
s.IMBridge = imbridge.NewBridge(db, resolver, exec, []Provider{
    &WeixinProvider{},
    &TelegramProvider{},
})
```

### Startup Poller Restoration

```go
func (s *Server) restoreIMBridgePollers() {
    for _, provider := range s.IMBridge.Providers() {
        bindings, _ := s.DB.GetActiveBindings(provider.Name())
        for _, b := range bindings {
            sbx, ok := s.Sandboxes.Get(b.SandboxID)
            if !ok || sbx.PodIP == "" { continue }
            s.IMBridge.StartPoller(BridgeBinding{
                Provider:     provider,
                Credentials:  Credentials{SandboxID: b.SandboxID, BotID: b.BotID, BotToken: b.BotToken, BaseURL: b.BaseURL},
                Cursor:       b.Cursor,
                PodIP:        sbx.PodIP,
                BridgeSecret: sbx.NanoclawBridgeSecret,
            })
        }
    }
}
```

## Frontend Changes

| Component | Change |
|-----------|--------|
| `WeixinLoginModal.tsx` | Update API paths to `/api/sandboxes/{id}/im/weixin/qr-*`. Functional logic unchanged. |
| New: `TelegramConfigModal.tsx` | Simple form: input Bot Token, submit to `/api/sandboxes/{id}/im/telegram/configure`. Show success/error. |
| Sandbox detail page | Add "IM Connections" section listing all bindings from `GET /api/sandboxes/{id}/im/bindings`. Each entry shows provider, bot ID, status, with disconnect button. |

## File Change Summary

### agentserver (primary)

| File | Op | Description |
|------|----|-------------|
| `internal/imbridge/bridge.go` | new | Generic Bridge (from weixin/bridge.go) |
| `internal/imbridge/provider.go` | new | Provider interface + types |
| `internal/imbridge/weixin_provider.go` | new | WeChat provider wrapping weixin/ilink.go |
| `internal/imbridge/telegram_provider.go` | new | Telegram provider |
| `internal/imbridge/telegram_api.go` | new | Telegram Bot API client |
| `internal/weixin/bridge.go` | delete | Logic moved to imbridge |
| `internal/weixin/ilink.go` | keep | iLink API client unchanged |
| `internal/db/im_bindings.go` | new | Replaces weixin_bindings.go |
| `internal/db/weixin_bindings.go` | delete | Replaced by im_bindings.go |
| `internal/db/migrations/010_unified_im_bindings.sql` | new | New tables + data migration |
| `internal/server/server.go` | modify | New routes + handlers, old routes as aliases |
| `internal/sandbox/config.go` | modify | Unified bridge URL config |
| `web/src/components/TelegramConfigModal.tsx` | new | Telegram config UI |
| `web/src/components/WeixinLoginModal.tsx` | modify | API path update |

### nanoclaw (minor)

| File | Op | Description |
|------|----|-------------|
| `src/channels/bridge-server.ts` | new | Shared HTTP bridge server (~60 lines) |
| `src/channels/telegram/index.ts` | new | Telegram bridge channel (~40 lines) |
| `src/channels/weixin/index.ts` | modify | Use bridge-server, remove inline HTTP server |
| `src/channels/index.ts` | modify | Add telegram import |

## JID Format & Compatibility

### New JID Format

All new messages use suffixed JIDs:
- WeChat: `{fromUserID}@im.wechat`
- Telegram: `{chatID}@tg`

### Backwards Compatibility

Existing nanoclaw data (chats, messages, registered_groups in SQLite) uses unsuffixed WeChat JIDs (e.g., `wxid_xxx`). This data is **not migrated**. Instead, nanoclaw's weixin channel `ownsJid` accepts both formats:

```typescript
ownsJid(jid: string): boolean {
    // New format (with suffix)
    if (jid.endsWith('@im.wechat')) return true;
    // Legacy format (no suffix) — any JID not claimed by another channel
    // is assumed to be weixin for backwards compatibility.
    return false;
}
```

The bridge-server's `/message` handler adds a fallback: if no channel's `ownsJid` matches, route to the weixin channel (legacy unsuffixed JIDs). This ensures existing registered groups and message history continue to work without migration.

New messages from agentserver will use the suffixed format. Over time, as groups are re-registered via IPC, the suffixed JIDs will replace the old ones.

## Telegram Group vs Private Chat

`TelegramProvider.Poll` uses `u.Message.Chat.ID` as the user/group identifier:
- Private chats: positive ID (e.g., `123456789@tg`)
- Groups/supergroups: negative ID (e.g., `-1001234567890@tg`)

The `ensureChatRegistered` call passes `is_group` based on the chat type:

```go
isGroup := u.Message.Chat.Type == "group" || u.Message.Chat.Type == "supergroup"
```

This is propagated via an optional field in `InboundMessage`:

```go
type InboundMessage struct {
    FromUserID string
    SenderName string
    Text       string
    IsGroup    bool
    Metadata   map[string]string
}
```

## Telegram Rate Limiting

Telegram Bot API returns HTTP 429 with `Retry-After` header. `TelegramGetUpdates` and `TelegramSendMessage` detect this and return a typed error:

```go
type RateLimitError struct {
    RetryAfter time.Duration
}
```

`TelegramProvider.Poll` converts this to `PollResult.ShouldBackoff`:

```go
if rle, ok := err.(*RateLimitError); ok {
    return &PollResult{ShouldBackoff: rle.RetryAfter}, nil
}
```

## handleNanoclawIMSend: botID Resolution

When nanoclaw sends a reply, the request body contains `to_user_id` (with JID suffix) and `text`. The handler resolves which bot to use:

1. Find provider by JID suffix
2. Optionally read `bot_id` from request body (may be empty)
3. If `bot_id` is empty: query `ListIMBindings(sandboxID, provider.Name())`, use the first binding
4. Look up credentials with resolved `(sandboxID, provider, botID)`

This matches the existing WeChat behavior where `bot_id` is usually omitted.

## Per-Sandbox Poller Restoration

In addition to the global `restoreIMBridgePollers()` at startup, a per-sandbox variant is needed for sandbox resume (when PodIP changes):

```go
func (s *Server) restoreIMBridgePollersForSandbox(sandboxID string) {
    sbx, ok := s.Sandboxes.Get(sandboxID)
    if !ok || sbx.PodIP == "" { return }

    for _, provider := range s.IMBridge.Providers() {
        bindings, _ := s.DB.GetActiveBindings(provider.Name())
        for _, b := range bindings {
            if b.SandboxID != sandboxID { continue }
            s.IMBridge.StartPoller(BridgeBinding{
                Provider:    provider,
                Credentials: Credentials{...},
                Cursor:      b.Cursor,
                PodIP:       sbx.PodIP,
                BridgeSecret: sbx.NanoclawBridgeSecret,
            })
        }
    }
}
```

Called from `handleResumeSandbox` after the pod is ready and PodIP is known.

## Old Table Retention

The migration does NOT drop `sandbox_weixin_bindings` or `weixin_context_tokens`. These tables are kept for rollback safety. A future migration (after the unified bridge is stable) will drop them.

## Migration Strategy

1. **Deploy agentserver first** — new unified tables + routes. Old routes kept as aliases. Existing WeChat bindings migrated to `sandbox_im_bindings` automatically. Old tables retained.
2. **Deploy nanoclaw second** — new image with telegram channel + bridge-server. Reads `NANOCLAW_BRIDGE_URL` (new) with fallback to `NANOCLAW_WEIXIN_BRIDGE_URL` (old). Weixin channel accepts both suffixed and unsuffixed JIDs.
3. **After all pods updated** — remove legacy route aliases and `NANOCLAW_WEIXIN_BRIDGE_URL` env var from `BuildNanoclawConfig`.
4. **After stable** — drop old `sandbox_weixin_bindings` and `weixin_context_tokens` tables.
