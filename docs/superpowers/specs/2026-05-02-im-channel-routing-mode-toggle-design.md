# IM Channel Routing Mode Toggle

**Date:** 2026-05-02
**Status:** Draft
**Author:** mryao + Claude

## 1. Overview

### 1.1 Problem Statement

The stateless CC stack is deployed on k8s but has never served real traffic because every IM channel still has `routing_mode='nanoclaw'` (the default after migration 018). Switching `routing_mode` today requires a raw SQL UPDATE — there is no HTTP API and no UI control. This blocks smoke-testing the stateless CC flow end-to-end.

### 1.2 Goal

Expose a per-channel `routing_mode` toggle in the workspace IM channel management UI, so a workspace member can switch a channel between `nanoclaw` and `stateless_cc` from the web UI and have the change take effect on the next inbound IM message — no service restart, no SQL.

### 1.3 Non-Goals

- Any pre-flight readiness check (OpenViking tree validation, executor availability). If the user switches to `stateless_cc` without those conditions met, the turn will fail downstream — that is acceptable for smoke-test sophistication. Readiness checks can be added later as a separate concern.
- Routing modes other than `nanoclaw` and `stateless_cc`.
- Changing defaults for existing channels (they stay `nanoclaw`).

## 2. Architecture

Extends the existing `PATCH /api/workspaces/{wid}/im/channels/{id}` path. No new routes, no new services. Follows the exact pattern already used for `require_mention`: write both the database and an in-memory map on the Bridge so the change is visible to the next inbound message without restarting the long-poller.

```
Web UI (WorkspaceDetail.tsx)
   │  select value change
   ▼
PATCH /api/workspaces/{wid}/im/channels/{cid}   Body: {"routing_mode":"stateless_cc"}
   │
   ▼
agentserver  (server.go:333 → imbridgeProxy, existing)
   │
   ▼
imbridgesvc.handleUpdateWorkspaceIMChannel  (handlers.go:898)
   ├─ requireWorkspaceMember                              (existing auth)
   ├─ GetIMChannel + workspace ownership check           (existing)
   ├─ validate routing_mode ∈ {nanoclaw, stateless_cc}   (new)
   ├─ db.UpdateIMChannelRoutingMode(channelID, mode)     (new DB fn)
   └─ bridge.SetChannelRoutingMode(channelID, mode)      (new in-memory setter)
   │
   ▼
200 {"status":"updated"}

Next inbound message from this channel:
  forwardMessage → getChannelRoutingMode(channelID) hit
                → "stateless_cc" → forwardToAgentserver
```

## 3. Components

### 3.1 Bridge Runtime State (`internal/imbridge/bridge.go`)

Add an in-memory map mirroring `channelMention`:

```go
type Bridge struct {
    // ... existing fields ...
    channelMention map[string]bool
    channelRouting map[string]string  // NEW — channelID → "nanoclaw" | "stateless_cc"
    mu             sync.Mutex
}

// SetChannelRoutingMode updates the in-memory routing_mode for a channel.
func (b *Bridge) SetChannelRoutingMode(channelID, mode string) {
    b.mu.Lock()
    b.channelRouting[channelID] = mode
    b.mu.Unlock()
}

// getChannelRoutingMode returns the in-memory routing_mode, or "" if not set.
func (b *Bridge) getChannelRoutingMode(channelID string) string {
    b.mu.Lock()
    v := b.channelRouting[channelID]
    b.mu.Unlock()
    return v
}
```

`StartPoller` seeds the map with `binding.RoutingMode` so the map is authoritative once a poller has started.

`forwardMessage` is changed to consult the map first, falling back to `binding.RoutingMode` only if the map has no entry (defensive; should not happen once StartPoller seeds it):

```go
func (b *Bridge) forwardMessage(ctx context.Context, binding BridgeBinding, msg InboundMessage) (bool, error) {
    mode := b.getChannelRoutingMode(binding.ChannelID)
    if mode == "" {
        mode = binding.RoutingMode
    }
    switch mode {
    case "stateless_cc":
        return b.forwardToAgentserver(ctx, binding, msg)
    default:
        return b.forwardToNanoClaw(ctx, binding, msg)
    }
}
```

### 3.2 DB Layer (`internal/db/im_channels.go`)

New standalone function (kept separate from `UpdateIMChannelSettings` so each settable column has a clear-purpose setter):

```go
// UpdateIMChannelRoutingMode updates the routing_mode column for a channel.
// Caller must validate `mode` before calling.
func (db *DB) UpdateIMChannelRoutingMode(channelID, mode string) error {
    _, err := db.Exec(
        `UPDATE workspace_im_channels SET routing_mode = $1 WHERE id = $2`,
        mode, channelID,
    )
    return err
}
```

### 3.3 imbridgesvc Handler (`internal/imbridgesvc/handlers.go:898`)

Extend the existing request struct to accept the new optional field:

```go
var req struct {
    RequireMention *bool   `json:"require_mention"`
    RoutingMode    *string `json:"routing_mode"`  // NEW
}
```

After the existing `require_mention` branch, add:

```go
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
```

Existing checks (`requireWorkspaceMember`, workspace ownership) cover this new branch without changes.

### 3.3b imbridgesvc List Handler

`handleListWorkspaceIMChannels` (in the same file) renders each channel through an inline `channelResp` struct that is also used as the JSON shape for `GET /api/workspaces/{wid}/im/channels`. Extend it with `RoutingMode string `json:"routing_mode"`` and populate it from `ch.RoutingMode` during the append. Without this, the frontend `<select>` value cannot be hydrated after a refetch and the toggle appears to revert on every page load.

### 3.4 Frontend (`web/src/`)

**`lib/api.ts`**

- Extend `IMChannel`:
  ```ts
  export interface IMChannel {
      id: string
      provider: string
      bot_id: string
      require_mention: boolean
      routing_mode: string          // NEW
  }
  ```
- Extend `updateWorkspaceIMChannel` settings:
  ```ts
  settings: {
      require_mention?: boolean
      routing_mode?: 'nanoclaw' | 'stateless_cc'   // NEW
  }
  ```

**`components/WorkspaceDetail.tsx` (near line 744, inside the IM channel row)**

Add a Select control below the existing `require_mention` checkbox, bound to `ch.routing_mode`. Options:
- `nanoclaw`
- `stateless_cc (Beta)` — label contains the suffix; value is `stateless_cc`

onChange handler:
```tsx
onChange={async (e) => {
    await updateWorkspaceIMChannel(workspaceId, ch.id, { routing_mode: e.target.value })
    // refetch
    listWorkspaceIMChannels(workspaceId).then(r => setImChannels(r.channels || []))
}}
```

Use the same Select primitive the codebase already uses (match the existing `WorkspaceDetail` patterns; no new component library).

## 4. Error Handling

| Failure | Response | State consistency |
|---------|----------|-------------------|
| Body malformed | 400 `invalid request body` (existing) | No mutation |
| `routing_mode` not in `{nanoclaw, stateless_cc}` | 400 `invalid routing_mode` | No mutation |
| Caller not a workspace member | 403 (existing) | No mutation |
| Channel not found or not in workspace | 404 (existing) | No mutation |
| `db.UpdateIMChannelRoutingMode` fails | 500 `failed to update channel`, `SetChannelRoutingMode` NOT called | DB and in-memory both unchanged — consistent |
| `SetChannelRoutingMode` — no IO, cannot fail | n/a | n/a |

If DB succeeds but the process crashes before `SetChannelRoutingMode` returns, the next imbridge startup reads `routing_mode` from DB via `restoreAllPollers` and seeds the map from `binding.RoutingMode`. Convergence is preserved across restarts.

## 5. Testing

### 5.1 Unit

- `db.UpdateIMChannelRoutingMode`: write then read back via `GetIMChannel`, both values observed.
- `Bridge.SetChannelRoutingMode` / `getChannelRoutingMode`: basic set/get; concurrent writers/readers do not race (leveraging existing `b.mu`).
- `Bridge.forwardMessage`: with map populated with `stateless_cc`, assert it calls `forwardToAgentserver` even when `binding.RoutingMode` is `nanoclaw` (proves in-memory map wins over the frozen binding).

### 5.2 Handler

- PATCH with `{routing_mode:"stateless_cc"}` → 200, DB updated, `SetChannelRoutingMode` called with `"stateless_cc"`.
- PATCH with `{routing_mode:"bogus"}` → 400, DB unchanged, Bridge unchanged.
- PATCH with both fields → both applied.
- PATCH with neither field → 200, noop (matches current behavior).

### 5.3 Manual Smoke Test (post-deploy)

1. Pick an `agent-ws-*` test workspace that has a bound WeChat channel.
2. Open the UI, flip routing_mode to `stateless_cc (Beta)`.
3. Verify `SELECT routing_mode FROM workspace_im_channels WHERE id = …;` shows `stateless_cc`.
4. Send a WeChat message; observe imbridge logs show `forwardToAgentserver`; observe `agent_session_events` in the `ccbroker` DB gets rows.
5. Flip back to `nanoclaw`; next message goes to the NanoClaw pod.

## 6. Rollout

Single PR. Requires deploying `agentserver`, `imbridge` containers (new handler + Bridge state) and the web frontend (bundled with agentserver). No schema migration (migration 018 already added the column). No data backfill. No feature flag — the new field is optional in both API and request body, so old clients keep working.

## 7. Risks

1. **Race on rapid toggle.** If a user flips the Select twice within a few hundred ms, two PATCHes race. DB `UPDATE` is single-row and last-write-wins; the in-memory Set is under `b.mu`, also last-write-wins. Final state matches whichever PATCH arrived last at the DB. Acceptable.
2. **Bridge instance that never started a poller for this channel.** `StartPoller` is what seeds `channelRouting`. If a channel has no active poller (e.g., credentials missing, sandbox paused) and someone PATCHes routing_mode, `SetChannelRoutingMode` still writes the map entry — it just sits there until a poller appears. Next `StartPoller` call seeds the map again with `binding.RoutingMode` from DB, which is the value we just wrote. Convergent.
3. **Downstream readiness failures.** Switching to `stateless_cc` for a workspace without OpenViking tree or online executors means the next turn will log an error and the user will get no reply. This is intentional scope — out of this spec, tracked as a separate smoke-test precondition.
