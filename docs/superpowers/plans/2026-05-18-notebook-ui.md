# Notebook UI Panels Implementation Plan (Plan 3c)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Two new tabs on the WorkspaceDetail page — **Notebooks** (iframe that loads JupyterLab via `/api/notebooks/{ws}/session` + JWT) and **Operations** (filterable table of `operations/list`).

**Architecture:** Pure React 19 (no new deps). Two component files + small `lib/api.ts` additions + wire two tabs into the existing `WorkspaceDetail.tsx` Tab union. Notebook session JWT is refreshed before the 10-min expiry (single setTimeout). Operations panel uses standard fetch + manual filter state — no react-query needed.

**Tech Stack:** React 19 · TypeScript · Tailwind v4 · lucide-react icons · react-router-dom 7 · vanilla `fetch` (no react-query) — all existing in the project.

**Out of scope:**
- Operations replay button (read-only in v1)
- Notebook collaboration (Jupyter RTC) — Plan 4
- Notebook templates / starter examples — separate spec
- Per-workspace token rotation flow (the JWT is per-iframe-session; workspace token rotation is admin-side)

---

## File Structure

```
web/src/lib/api.ts                                  MODIFY — add types + 2 functions
web/src/components/NotebooksPanel.tsx               NEW
web/src/components/OperationsPanel.tsx              NEW
web/src/components/WorkspaceDetail.tsx              MODIFY — 2 new tabs + imports
```

Total ~400-500 LOC new + ~20 LOC modified.

---

## Design Decisions

**1. No data-fetching lib.** The codebase already uses raw `fetch` everywhere (see `CodexTokensPanel.tsx` for the standard pattern: `useState` + `useEffect` + `useCallback` refresh). Stay consistent.

**2. NotebooksPanel = single-iframe.** Component:
- On mount: POST `/api/notebooks/{ws}/session` → `{url, token, expires_at}`
- Renders `<iframe src={url + '?token=' + token} />` 
- Sets up a single `setTimeout` to refresh ~1 min before `expires_at`; on refresh, repost session and update iframe `src` (forces reload — acceptable for v1)
- If the page already has an active iframe and the user clicks back to the tab, reuse the existing JWT (don't re-POST unless expired)

**3. Per-workspace state scoped to WorkspaceDetail unmount.** When the user switches to another workspace or closes the tab, the panel unmounts, the iframe is dropped, and the next mount fetches a fresh session. No global state needed.

**4. OperationsPanel = scrollable table + filter controls + auto-refresh.** Filters: env, tool, source, is_error, time-since. Page size 100 (matches backend default). "Refresh" button + 30s auto-refresh option (default off — opt-in via checkbox).

**5. Tab key conventions match existing.** `WorkspaceDetail.tsx`'s `Tab` union currently `'overview' | 'members' | 'traces' | 'credentials' | 'settings'` — add `'notebooks' | 'operations'`. Order in the tabs nav: after traces, before credentials.

**6. Loading + error states match existing pattern.** From `CodexTokensPanel.tsx`: `loading`/`error` state, render spinner/error message before content.

**7. Iframe sandbox attributes.** `sandbox="allow-scripts allow-same-origin allow-forms allow-popups"` — same-origin needed for JupyterLab's WebSocket connections.

**8. URL hash on operations row.** A click on an operation row opens a detail modal that shows full `arguments` + `result_summary` (pretty-printed JSON). No row click → no modal; nothing fancy.

**9. No URL-search-param state for tabs in v1.** Existing `WorkspaceDetail` accepts `initialTab` prop but doesn't bidirectionally sync to URL. We don't change this contract. Selecting a tab is in-memory only.

**10. Empty / disabled / loading-spinner states for Notebooks.** If `/session` returns 503 (feature disabled), show a friendly "Notebook feature is not enabled for this deployment" message. If 403 (not a member), the workspace tab wouldn't even render — defensive only.

---

## Task 1: API types + functions

**Files:**
- Modify: `web/src/lib/api.ts`

- [ ] **Step 1: Add types + fetch helpers**

Append to `web/src/lib/api.ts`:

```ts
// === Notebook (Plan 3c) ===

export interface NotebookSession {
  url: string         // path to load in iframe (relative to current origin)
  token: string       // JWT to include as ?token=
  expires_at: number  // unix seconds
}

/**
 * Mint a fresh notebook session. The returned token is good for 10 min.
 * 503 if the notebook feature is not enabled for this deployment.
 * 403 if the current user is not a workspace member.
 */
export async function createNotebookSession(workspaceId: string): Promise<NotebookSession> {
  const res = await fetch(`/api/notebooks/${encodeURIComponent(workspaceId)}/session`, {
    method: 'POST',
    credentials: 'include',
  })
  if (res.status === 503) {
    throw new Error('Notebook feature is not enabled for this deployment.')
  }
  if (!res.ok) {
    const body = await res.text()
    throw new Error(`createNotebookSession: ${res.status} ${body || res.statusText}`)
  }
  return res.json()
}

// === Operations (Plan 3c) ===

export interface Operation {
  id: string
  workspace_id: string
  user_id?: string | null
  source: 'sdk' | 'tui' | 'llm'
  thread_id?: string | null
  env_id: string
  tool: string
  arguments?: unknown
  arguments_meta?: { truncated: true; size_bytes: number; sha256: string } | null
  is_error: boolean
  result_summary?: string | null
  result_meta?: { truncated: true; total_bytes: number } | null
  started_at: string  // RFC3339
  completed_at: string
  duration_ms: number
}

export interface ListOperationsFilters {
  env_id?: string
  tool?: string
  source?: 'sdk' | 'tui' | 'llm'
  is_error?: boolean
  since?: string  // RFC3339Nano
  limit?: number  // default 100, max 1000
}

/**
 * List operations for a workspace, server-side filtered.
 * Backed by GET /internal/operations on agentserver. The web UI hits
 * the public-facing /api/workspaces/{id}/operations alias.
 */
export async function listOperations(
  workspaceId: string,
  filters: ListOperationsFilters = {},
): Promise<Operation[]> {
  const params = new URLSearchParams({ workspace_id: workspaceId })
  if (filters.env_id) params.set('env_id', filters.env_id)
  if (filters.tool) params.set('tool', filters.tool)
  if (filters.source) params.set('source', filters.source)
  if (filters.is_error !== undefined) params.set('is_error', String(filters.is_error))
  if (filters.since) params.set('since', filters.since)
  if (filters.limit) params.set('limit', String(filters.limit))

  const res = await fetch(`/api/workspaces/${encodeURIComponent(workspaceId)}/operations?${params}`, {
    credentials: 'include',
  })
  if (!res.ok) {
    const body = await res.text()
    throw new Error(`listOperations: ${res.status} ${body || res.statusText}`)
  }
  const data = await res.json()
  return data.operations ?? []
}
```

NOTE: This task depends on a new agentserver-web endpoint `GET /api/workspaces/{id}/operations` that wraps the existing `/internal/operations` (Plan 2's). The internal endpoint is bearer-secret-authed and not user-facing. Two options:

- **Option A** (recommended): add a thin user-auth handler `s.getWorkspaceOperations(w, r)` that calls `s.DB.ListOperations` directly, behind the same `s.Auth.Middleware` + membership check used by other workspace endpoints. ~30 LOC in `internal/server/operations.go` or a sibling file.
- **Option B**: have the proxy forward the user-auth endpoint to the existing internal one (loopback HTTP). More moving parts; not worth it.

Add the Go handler as part of this task (it's tiny). Append to `internal/server/operations.go`:

```go
// getWorkspaceOperations is GET /api/workspaces/{id}/operations.
// User-session authed; membership-checked; otherwise mirrors
// getInternalOperations's filter behavior.
func (s *Server) getWorkspaceOperations(w http.ResponseWriter, r *http.Request) {
    userID := auth.UserIDFromContext(r.Context())  // ADAPT to actual helper
    if userID == "" {
        http.Error(w, "unauthorized", http.StatusUnauthorized)
        return
    }
    wsID := chi.URLParam(r, "id")
    if wsID == "" {
        http.Error(w, "workspace id required", http.StatusBadRequest)
        return
    }
    ok, err := s.DB.IsWorkspaceMember(wsID, userID)  // ADAPT
    if err != nil {
        http.Error(w, "membership check failed", http.StatusInternalServerError)
        return
    }
    if !ok {
        http.Error(w, "forbidden", http.StatusForbidden)
        return
    }
    // Force workspace_id from URL, ignore query value (security).
    q := r.URL.Query()
    q.Set("workspace_id", wsID)
    r.URL.RawQuery = q.Encode()
    s.getInternalOperations(w, r)
}
```

Register the route in `server.go` next to other `/api/workspaces/{id}/*` routes:

```go
r.Get("/api/workspaces/{id}/operations", s.getWorkspaceOperations)
```

- [ ] **Step 2: TypeScript build check**

```bash
cd /root/agentserver/web
pnpm install   # if needed
pnpm tsc -b --noEmit
```
Expected: clean (no type errors).

- [ ] **Step 3: Commit**

```bash
cd /root/agentserver
git add web/src/lib/api.ts internal/server/operations.go internal/server/server.go
git commit -m "feat(web,server): notebook session + operations API surface

new web API types + fns (createNotebookSession, listOperations).
new user-auth endpoint GET /api/workspaces/{id}/operations wrapping
getInternalOperations with membership check."
```

---

## Task 2: `NotebooksPanel` component

**Files:**
- Create: `web/src/components/NotebooksPanel.tsx`

- [ ] **Step 1: Implement**

Create `web/src/components/NotebooksPanel.tsx`:

```tsx
import { useEffect, useRef, useState } from 'react'
import { Loader2, AlertCircle } from 'lucide-react'
import { createNotebookSession, type NotebookSession } from '../lib/api'

interface Props {
  workspaceId: string
}

// Refresh the session this many seconds before the JWT actually expires.
// 10-min TTL minus 1-min safety = refresh every 9 minutes.
const REFRESH_BUFFER_SECONDS = 60

export default function NotebooksPanel({ workspaceId }: Props) {
  const [session, setSession] = useState<NotebookSession | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const timerRef = useRef<number | null>(null)

  useEffect(() => {
    let cancelled = false

    const fetchSession = async () => {
      try {
        setLoading(true)
        const s = await createNotebookSession(workspaceId)
        if (cancelled) return
        setSession(s)
        setError(null)
        scheduleRefresh(s.expires_at)
      } catch (e) {
        if (cancelled) return
        setError(e instanceof Error ? e.message : String(e))
        setSession(null)
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    const scheduleRefresh = (expiresAt: number) => {
      if (timerRef.current !== null) {
        window.clearTimeout(timerRef.current)
      }
      const now = Math.floor(Date.now() / 1000)
      const delayMs = Math.max(1000, (expiresAt - now - REFRESH_BUFFER_SECONDS) * 1000)
      timerRef.current = window.setTimeout(() => void fetchSession(), delayMs)
    }

    void fetchSession()

    return () => {
      cancelled = true
      if (timerRef.current !== null) {
        window.clearTimeout(timerRef.current)
        timerRef.current = null
      }
    }
  }, [workspaceId])

  if (loading && !session) {
    return (
      <div className="flex items-center justify-center h-96 text-gray-500">
        <Loader2 className="w-5 h-5 mr-2 animate-spin" />
        Starting notebook environment…
      </div>
    )
  }

  if (error) {
    return (
      <div className="flex items-start gap-3 p-4 border border-red-300 bg-red-50 rounded">
        <AlertCircle className="w-5 h-5 text-red-600 mt-0.5 shrink-0" />
        <div>
          <div className="font-medium text-red-900">Could not start notebook</div>
          <div className="text-sm text-red-800 mt-1 whitespace-pre-wrap">{error}</div>
        </div>
      </div>
    )
  }

  if (!session) return null

  const iframeSrc = `${session.url}?token=${encodeURIComponent(session.token)}`

  return (
    <div className="h-[80vh] w-full">
      <iframe
        key={session.token}  // re-mount iframe when token refreshes
        src={iframeSrc}
        sandbox="allow-scripts allow-same-origin allow-forms allow-popups"
        className="w-full h-full border rounded"
        title="Notebook"
      />
    </div>
  )
}
```

- [ ] **Step 2: Type-check**

```bash
cd /root/agentserver/web
pnpm tsc -b --noEmit
```
Expected: clean.

- [ ] **Step 3: Commit**

```bash
cd /root/agentserver
git add web/src/components/NotebooksPanel.tsx
git commit -m "feat(web): NotebooksPanel component

POSTs /api/notebooks/{ws}/session on mount, iframes the returned URL,
re-fetches ~1 min before token expiry. Loading + error states match
existing panel pattern (lucide icons + tailwind)."
```

---

## Task 3: `OperationsPanel` component

**Files:**
- Create: `web/src/components/OperationsPanel.tsx`

- [ ] **Step 1: Implement**

Create `web/src/components/OperationsPanel.tsx`:

```tsx
import { useCallback, useEffect, useState } from 'react'
import { RefreshCw, AlertCircle } from 'lucide-react'
import {
  listOperations,
  type Operation,
  type ListOperationsFilters,
} from '../lib/api'

interface Props {
  workspaceId: string
}

const AUTO_REFRESH_INTERVAL_MS = 30_000

const SOURCES: Array<'' | 'sdk' | 'tui' | 'llm'> = ['', 'sdk', 'tui', 'llm']

export default function OperationsPanel({ workspaceId }: Props) {
  const [filters, setFilters] = useState<ListOperationsFilters>({ limit: 100 })
  const [ops, setOps] = useState<Operation[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [autoRefresh, setAutoRefresh] = useState(false)
  const [selected, setSelected] = useState<Operation | null>(null)

  const refresh = useCallback(async () => {
    setLoading(true)
    try {
      const rows = await listOperations(workspaceId, filters)
      setOps(rows)
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [workspaceId, filters])

  useEffect(() => { void refresh() }, [refresh])

  useEffect(() => {
    if (!autoRefresh) return
    const id = window.setInterval(() => void refresh(), AUTO_REFRESH_INTERVAL_MS)
    return () => window.clearInterval(id)
  }, [autoRefresh, refresh])

  return (
    <div className="flex flex-col gap-3">
      {/* Filter bar */}
      <div className="flex flex-wrap items-end gap-3 p-3 border rounded bg-gray-50">
        <FilterInput
          label="env"
          value={filters.env_id ?? ''}
          onChange={(v) => setFilters((f) => ({ ...f, env_id: v || undefined }))}
        />
        <FilterInput
          label="tool"
          value={filters.tool ?? ''}
          onChange={(v) => setFilters((f) => ({ ...f, tool: v || undefined }))}
        />
        <div className="flex flex-col">
          <label className="text-xs text-gray-600">source</label>
          <select
            className="border rounded px-2 py-1 text-sm"
            value={filters.source ?? ''}
            onChange={(e) =>
              setFilters((f) => ({
                ...f,
                source: (e.target.value || undefined) as ListOperationsFilters['source'],
              }))
            }
          >
            {SOURCES.map((s) => (
              <option key={s} value={s}>
                {s || '(all)'}
              </option>
            ))}
          </select>
        </div>
        <label className="flex items-center gap-1 text-sm">
          <input
            type="checkbox"
            checked={filters.is_error === true}
            onChange={(e) =>
              setFilters((f) => ({
                ...f,
                is_error: e.target.checked ? true : undefined,
              }))
            }
          />
          errors only
        </label>
        <label className="flex items-center gap-1 text-sm">
          <input
            type="checkbox"
            checked={autoRefresh}
            onChange={(e) => setAutoRefresh(e.target.checked)}
          />
          auto-refresh (30s)
        </label>
        <button
          onClick={() => void refresh()}
          className="flex items-center gap-1 px-3 py-1 text-sm bg-blue-600 text-white rounded hover:bg-blue-700"
        >
          <RefreshCw className={`w-3 h-3 ${loading ? 'animate-spin' : ''}`} />
          Refresh
        </button>
      </div>

      {error && (
        <div className="flex items-start gap-2 p-3 border border-red-300 bg-red-50 rounded">
          <AlertCircle className="w-4 h-4 text-red-600 mt-0.5 shrink-0" />
          <div className="text-sm text-red-800 whitespace-pre-wrap">{error}</div>
        </div>
      )}

      {/* Table */}
      <div className="overflow-x-auto border rounded">
        <table className="w-full text-sm">
          <thead className="bg-gray-100 text-gray-700">
            <tr>
              <th className="px-3 py-2 text-left">started</th>
              <th className="px-3 py-2 text-left">env</th>
              <th className="px-3 py-2 text-left">tool</th>
              <th className="px-3 py-2 text-left">source</th>
              <th className="px-3 py-2 text-left">user</th>
              <th className="px-3 py-2 text-right">dur (ms)</th>
              <th className="px-3 py-2 text-left">status</th>
            </tr>
          </thead>
          <tbody>
            {ops.length === 0 && !loading ? (
              <tr>
                <td colSpan={7} className="px-3 py-6 text-center text-gray-500">
                  No operations match these filters.
                </td>
              </tr>
            ) : (
              ops.map((op) => (
                <tr
                  key={op.id}
                  onClick={() => setSelected(op)}
                  className="border-t cursor-pointer hover:bg-gray-50"
                >
                  <td className="px-3 py-1 font-mono text-xs">
                    {new Date(op.started_at).toLocaleString()}
                  </td>
                  <td className="px-3 py-1 font-mono">{op.env_id}</td>
                  <td className="px-3 py-1 font-mono">{op.tool}</td>
                  <td className="px-3 py-1">{op.source}</td>
                  <td className="px-3 py-1 font-mono text-xs text-gray-600">
                    {op.user_id ?? '—'}
                  </td>
                  <td className="px-3 py-1 text-right tabular-nums">{op.duration_ms}</td>
                  <td className="px-3 py-1">
                    {op.is_error ? (
                      <span className="text-red-600 font-medium">error</span>
                    ) : (
                      <span className="text-green-700">ok</span>
                    )}
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      {/* Detail modal */}
      {selected && (
        <OperationDetailModal op={selected} onClose={() => setSelected(null)} />
      )}
    </div>
  )
}

function FilterInput({
  label,
  value,
  onChange,
}: {
  label: string
  value: string
  onChange: (v: string) => void
}) {
  return (
    <div className="flex flex-col">
      <label className="text-xs text-gray-600">{label}</label>
      <input
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="border rounded px-2 py-1 text-sm w-32"
      />
    </div>
  )
}

function OperationDetailModal({
  op,
  onClose,
}: {
  op: Operation
  onClose: () => void
}) {
  return (
    <div
      className="fixed inset-0 bg-black/40 flex items-center justify-center z-50 p-4"
      onClick={onClose}
    >
      <div
        className="bg-white rounded-lg shadow-xl max-w-3xl w-full max-h-[80vh] overflow-auto"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-4 py-3 border-b flex items-center justify-between">
          <div className="font-mono text-sm">{op.id}</div>
          <button
            onClick={onClose}
            className="text-gray-500 hover:text-gray-900"
          >
            ✕
          </button>
        </div>
        <div className="p-4 space-y-3 text-sm">
          <Row label="started_at" value={op.started_at} />
          <Row label="duration_ms" value={String(op.duration_ms)} />
          <Row label="env" value={op.env_id} />
          <Row label="tool" value={op.tool} />
          <Row label="source" value={op.source} />
          <Row label="user" value={op.user_id ?? '—'} />
          <Row
            label="is_error"
            value={op.is_error ? 'true' : 'false'}
            valueClass={op.is_error ? 'text-red-600 font-medium' : ''}
          />
          {op.arguments !== undefined && op.arguments !== null && (
            <div>
              <div className="text-xs text-gray-600 mb-1">arguments</div>
              <pre className="bg-gray-50 border rounded p-2 overflow-auto max-h-64 text-xs">
                {JSON.stringify(op.arguments, null, 2)}
              </pre>
            </div>
          )}
          {op.arguments_meta && (
            <div className="text-xs text-gray-600">
              arguments truncated: {op.arguments_meta.size_bytes} bytes, sha256{' '}
              <code>{op.arguments_meta.sha256.slice(0, 12)}…</code>
            </div>
          )}
          {op.result_summary && (
            <div>
              <div className="text-xs text-gray-600 mb-1">result_summary</div>
              <pre className="bg-gray-50 border rounded p-2 overflow-auto max-h-64 text-xs whitespace-pre-wrap">
                {op.result_summary}
              </pre>
            </div>
          )}
          {op.result_meta && (
            <div className="text-xs text-gray-600">
              result truncated: total {op.result_meta.total_bytes} bytes
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function Row({
  label,
  value,
  valueClass = '',
}: {
  label: string
  value: string
  valueClass?: string
}) {
  return (
    <div className="flex gap-3">
      <div className="text-xs text-gray-600 w-24 shrink-0">{label}</div>
      <div className={`flex-1 font-mono ${valueClass}`}>{value}</div>
    </div>
  )
}
```

- [ ] **Step 2: Type-check**

```bash
cd /root/agentserver/web
pnpm tsc -b --noEmit
```
Expected: clean.

- [ ] **Step 3: Commit**

```bash
cd /root/agentserver
git add web/src/components/OperationsPanel.tsx
git commit -m "feat(web): OperationsPanel component

filter bar (env/tool/source/errors-only) + auto-refresh toggle (30s);
table rows clickable for detail modal showing full args/result_summary."
```

---

## Task 4: Wire two new tabs into `WorkspaceDetail`

**Files:**
- Modify: `web/src/components/WorkspaceDetail.tsx`

- [ ] **Step 1: Inspect current tab definition**

```bash
cd /root/agentserver
grep -n "type Tab\|tabs:\|setTab\|tab ===" web/src/components/WorkspaceDetail.tsx | head -20
```
You should see the `Tab` type definition, the `tabs` array (used for the nav bar), and the conditional `{tab === 'X' && <Component .../>}` blocks.

- [ ] **Step 2: Add imports**

Near the existing imports (e.g. `import CodexTokensPanel from './CodexTokensPanel'`), add:

```tsx
import NotebooksPanel from './NotebooksPanel'
import OperationsPanel from './OperationsPanel'
```

Also import icons from lucide-react. Add to the existing lucide import line:

```tsx
import { ..., BookOpen, Activity } from 'lucide-react'
```

(Adjust `...` to whatever icons are already imported.)

- [ ] **Step 3: Extend the Tab type**

Find:
```tsx
export type Tab = 'overview' | 'members' | 'traces' | 'credentials' | 'settings'
```

Change to:
```tsx
export type Tab = 'overview' | 'members' | 'traces' | 'notebooks' | 'operations' | 'credentials' | 'settings'
```

- [ ] **Step 4: Add nav entries**

Find the `tabs:` array (constant inside the component) and insert two entries after `'traces'`:

```tsx
{ key: 'notebooks',  label: 'Notebooks',  icon: <BookOpen className="w-4 h-4" /> },
{ key: 'operations', label: 'Operations', icon: <Activity className="w-4 h-4" /> },
```

- [ ] **Step 5: Add the conditional renders**

After the existing `{tab === 'traces' && ...}` block, add:

```tsx
{tab === 'notebooks' && (
  <NotebooksPanel workspaceId={workspace.id} />
)}
{tab === 'operations' && (
  <OperationsPanel workspaceId={workspace.id} />
)}
```

- [ ] **Step 6: Type-check + lint**

```bash
cd /root/agentserver/web
pnpm tsc -b --noEmit
pnpm lint
```
Expected: clean.

- [ ] **Step 7: Commit**

```bash
cd /root/agentserver
git add web/src/components/WorkspaceDetail.tsx
git commit -m "feat(web): wire Notebooks + Operations tabs into WorkspaceDetail

extends Tab union; adds nav entries (BookOpen / Activity icons);
mounts the new panels per workspace id."
```

---

## Task 5: Build + visual smoke

**Files:** none new.

- [ ] **Step 1: Production build**

```bash
cd /root/agentserver/web
pnpm build
```
Expected: clean build; `dist/` regenerated.

- [ ] **Step 2: Dev server visual smoke (manual, optional)**

```bash
cd /root/agentserver/web
pnpm dev
```
Open the URL printed by Vite. Navigate to a workspace; switch to Notebooks tab → should attempt POST to `/api/notebooks/{ws}/session`. Without a backend running it'll show a network error — that's expected; we're just verifying the component renders + tab nav works.

Skip if no browser available. Tab + render check is provable from `pnpm tsc`.

- [ ] **Step 3: Commit (if Step 1 regenerated dist with diff)**

If `dist/` has new content tracked in git (check `git status web/dist`):

```bash
cd /root/agentserver
git add web/dist
git commit -m "build(web): regenerate dist for Plan 3c panels" || true
```

If `web/dist` is gitignored, skip.

---

## Self-review checklist

After all 5 tasks:
- [ ] `pnpm tsc -b --noEmit` clean
- [ ] `pnpm lint` clean (or only pre-existing warnings)
- [ ] `pnpm build` succeeds
- [ ] `go vet ./internal/server && go build ./...` clean (Task 1's Go addition)
- [ ] WorkspaceDetail.tsx exports the extended `Tab` type
- [ ] No new web dep added (verify `web/package.json` diff: should only be unchanged)
- [ ] Iframe sandbox attributes appropriate
- [ ] Auto-refresh timer clears on unmount

## After this plan

Plan 3c lands as the final piece. End-to-end story is complete:
- User opens NotebooksPanel → POST session → iframe loads JupyterLab → cell runs `await alpha.shell(...)` → SDK calls gateway → operation logged with correct user_id → OperationsPanel shows the row

Real-cluster smoke verifies all 4 PRs (Plan 1 + 2 + 3a + 3b + 3c) work together. The only remaining manual config a deployer needs: set `notebook.jwtSecret` in helm values (or it generates one on install via `randAlphaNum 64` if the chart adds a `lookup` trick).
