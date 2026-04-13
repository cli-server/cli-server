# Workspace Guidance Empty State Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the static "Select or create a sandbox" empty state with status-aware guidance cards showing IM channel and ModelServer configuration status, with quick-jump links to the workspace settings page.

**Architecture:** New `WorkspaceEmptyState` component fetches IM channel list and ModelServer status for the current workspace, renders status cards with action buttons. Action buttons navigate to `/workspaces` with a `?tab=settings` query param. `WorkspaceDetail` is updated to read an optional `tab` query param to support deep-linking to the settings tab.

**Tech Stack:** React, TypeScript, react-router-dom, lucide-react, existing API functions from `lib/api.ts`.

**Spec:** `docs/superpowers/specs/2026-04-13-workspace-guidance-empty-state-design.md`

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `web/src/components/WorkspaceEmptyState.tsx` | Create | New component: fetches status, renders guidance cards |
| `web/src/components/WorkspaceDetail.tsx` | Modify | Accept `initialTab` prop for deep-linking to settings tab |
| `web/src/components/ManageWorkspaces.tsx` | Modify | Read `tab` query param, pass as `initialTab` to WorkspaceDetail |
| `web/src/App.tsx` | Modify | Import and use `WorkspaceEmptyState` in place of static text |

---

### Task 1: Add `tab` query param support to WorkspaceDetail

`WorkspaceDetail` currently always opens on the 'overview' tab. We need it to accept a `tab` query parameter so that navigation from the empty state can deep-link to the settings tab.

**Files:**
- Modify: `web/src/components/WorkspaceDetail.tsx:52-72`

- [ ] **Step 1: Add `initialTab` prop to WorkspaceDetail**

In `web/src/components/WorkspaceDetail.tsx`, add an optional `initialTab` prop:

```tsx
interface WorkspaceDetailProps {
  workspace: Workspace
  onRename?: (id: string, name: string) => void
  initialTab?: Tab
}

export function WorkspaceDetail({ workspace, onRename, initialTab }: WorkspaceDetailProps) {
  const [tab, setTab] = useState<Tab>(initialTab ?? 'overview')
```

Also update the `useEffect` that resets tab on workspace change (line 72) to respect the prop:

```tsx
  useEffect(() => {
    setTab(initialTab ?? 'overview')
    setMembers([])
    // ... rest of existing reset logic unchanged
  }, [workspace.id, initialTab])
```

- [ ] **Step 2: Pass `initialTab` from ManageWorkspaces based on URL query param**

In `web/src/components/ManageWorkspaces.tsx`, read the `tab` search param and pass it through:

```tsx
import { useSearchParams } from 'react-router-dom'
import { type Workspace } from '../lib/api'
import { WorkspaceDetail } from './WorkspaceDetail'
import { FolderOpen } from 'lucide-react'

// ... inside the component:
export function ManageWorkspaces({ workspaces, selectedWorkspaceId, onSelectWorkspace, onRenameWorkspace }: ManageWorkspacesProps) {
  const [searchParams, setSearchParams] = useSearchParams()
  const tabParam = searchParams.get('tab')
  const validTabs = ['overview', 'members', 'traces', 'settings']
  const initialTab = (tabParam && validTabs.includes(tabParam)) ? tabParam as 'overview' | 'members' | 'traces' | 'settings' : undefined

  const selectedWorkspace = workspaces.find((w) => w.id === selectedWorkspaceId)

  return (
    // ... existing JSX, update the WorkspaceDetail usage:
        {selectedWorkspace ? (
          <WorkspaceDetail workspace={selectedWorkspace} onRename={onRenameWorkspace} initialTab={initialTab} />
        ) : (
    // ... rest unchanged
```

- [ ] **Step 3: Verify deep-linking works**

Start the dev server, navigate to `/workspaces?tab=settings`. Confirm that the WorkspaceDetail opens with the Settings tab selected (not Overview).

Run: `cd /root/agentserver/web && npm run dev`

Open browser to the app, navigate to `/workspaces?tab=settings`, verify the Settings tab is active.

- [ ] **Step 4: Commit**

```bash
git add web/src/components/WorkspaceDetail.tsx web/src/components/ManageWorkspaces.tsx
git commit -m "feat(web): add initialTab prop to WorkspaceDetail for deep-linking"
```

---

### Task 2: Create WorkspaceEmptyState component

**Files:**
- Create: `web/src/components/WorkspaceEmptyState.tsx`

- [ ] **Step 1: Create the component with data fetching**

Create `web/src/components/WorkspaceEmptyState.tsx`:

```tsx
import { useState, useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { MessageSquare, Box, ArrowRight } from 'lucide-react'
import {
  listWorkspaceIMChannels,
  getModelserverStatus,
  type IMChannel,
  type ModelserverStatus,
} from '../lib/api'

interface WorkspaceEmptyStateProps {
  workspaceId: string
}

export function WorkspaceEmptyState({ workspaceId }: WorkspaceEmptyStateProps) {
  const navigate = useNavigate()
  const [imChannels, setImChannels] = useState<IMChannel[]>([])
  const [msStatus, setMsStatus] = useState<ModelserverStatus | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    setLoading(true)
    Promise.all([
      listWorkspaceIMChannels(workspaceId).then((r) => setImChannels(r.channels || [])).catch(() => setImChannels([])),
      getModelserverStatus(workspaceId).then(setMsStatus).catch(() => setMsStatus({ connected: false })),
    ]).finally(() => setLoading(false))
  }, [workspaceId])

  const goToSettings = () => navigate('/workspaces?tab=settings')

  const providerLabel = (p: string) =>
    p === 'telegram' ? 'Telegram' : p === 'matrix' ? 'Matrix' : 'WeChat'

  const imStatusText = () => {
    if (imChannels.length === 0) return 'No channels configured'
    const providers = [...new Set(imChannels.map((ch) => providerLabel(ch.provider)))]
    return `${imChannels.length} channel${imChannels.length > 1 ? 's' : ''}: ${providers.join(', ')}`
  }

  const msStatusText = () => {
    if (!msStatus?.connected) return 'Not connected'
    return `Connected to ${msStatus.project_name}`
  }

  return (
    <div className="flex flex-col items-center justify-center gap-6 h-full">
      <span className="text-[var(--muted-foreground)]">Select or create a sandbox</span>

      <div className="w-full max-w-sm">
        <div className="rounded-lg border border-[var(--border)] bg-[var(--card)]">
          <div className="px-4 py-3 border-b border-[var(--border)]">
            <span className="text-xs font-medium text-[var(--muted-foreground)] uppercase tracking-wide">Quick Setup</span>
          </div>
          <div className="flex flex-col divide-y divide-[var(--border)]">
            {/* IM Channels */}
            <div className="px-4 py-3">
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-2.5">
                  <div className={`flex items-center justify-center w-7 h-7 rounded-md ${imChannels.length > 0 ? 'bg-green-500/10' : 'bg-[var(--muted)]'}`}>
                    <MessageSquare size={14} className={imChannels.length > 0 ? 'text-green-400' : 'text-[var(--muted-foreground)]'} />
                  </div>
                  <div>
                    <div className="text-sm font-medium text-[var(--foreground)]">IM Channels</div>
                    <div className={`text-xs ${imChannels.length > 0 ? 'text-green-400' : 'text-[var(--muted-foreground)]'}`}>
                      {loading ? '\u00A0' : imStatusText()}
                    </div>
                  </div>
                </div>
                <button
                  onClick={goToSettings}
                  className="inline-flex items-center gap-1 rounded-md px-2.5 py-1 text-xs font-medium text-[var(--muted-foreground)] hover:text-[var(--foreground)] hover:bg-[var(--secondary)] transition-colors"
                >
                  {imChannels.length > 0 ? 'Manage' : 'Configure'}
                  <ArrowRight size={12} />
                </button>
              </div>
            </div>

            {/* ModelServer */}
            <div className="px-4 py-3">
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-2.5">
                  <div className={`flex items-center justify-center w-7 h-7 rounded-md ${msStatus?.connected ? 'bg-blue-500/10' : 'bg-[var(--muted)]'}`}>
                    <Box size={14} className={msStatus?.connected ? 'text-blue-400' : 'text-[var(--muted-foreground)]'} />
                  </div>
                  <div>
                    <div className="text-sm font-medium text-[var(--foreground)]">ModelServer</div>
                    <div className={`text-xs ${msStatus?.connected ? 'text-blue-400' : 'text-[var(--muted-foreground)]'}`}>
                      {loading ? '\u00A0' : msStatusText()}
                    </div>
                  </div>
                </div>
                <button
                  onClick={goToSettings}
                  className="inline-flex items-center gap-1 rounded-md px-2.5 py-1 text-xs font-medium text-[var(--muted-foreground)] hover:text-[var(--foreground)] hover:bg-[var(--secondary)] transition-colors"
                >
                  {msStatus?.connected ? 'Manage' : 'Connect'}
                  <ArrowRight size={12} />
                </button>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}
```

- [ ] **Step 2: Commit**

```bash
git add web/src/components/WorkspaceEmptyState.tsx
git commit -m "feat(web): add WorkspaceEmptyState component with status cards"
```

---

### Task 3: Wire WorkspaceEmptyState into App.tsx

**Files:**
- Modify: `web/src/App.tsx:4-17` (imports), `web/src/App.tsx:270-279` (defaultContent)

- [ ] **Step 1: Add import**

In `web/src/App.tsx`, add the import alongside other component imports (after line 27):

```tsx
import { WorkspaceEmptyState } from './components/WorkspaceEmptyState'
```

- [ ] **Step 2: Replace static defaultContent with WorkspaceEmptyState**

In `web/src/App.tsx`, replace lines 275-279:

```tsx
// Before (lines 275-279):
  ) : (
    <div className="flex items-center justify-center h-full">
      <span className="text-[var(--muted-foreground)]">Select or create a sandbox</span>
    </div>
  )

// After:
  ) : (
    <WorkspaceEmptyState workspaceId={selectedWorkspaceId!} />
  )
```

- [ ] **Step 3: Commit**

```bash
git add web/src/App.tsx
git commit -m "feat(web): use WorkspaceEmptyState in workspace default view"
```

---

### Task 4: Manual verification

**Files:** None (testing only)

- [ ] **Step 1: Start the dev server**

Run: `cd /root/agentserver/web && npm run dev`

- [ ] **Step 2: Test empty state with unconfigured workspace**

Open the app in a browser. Navigate to a workspace that has no IM channels and no ModelServer connected. Verify:
- "Select or create a sandbox" text is displayed
- Below it, a "Quick Setup" card with two rows
- IM Channels row shows "No channels configured" in muted text, "Configure" button
- ModelServer row shows "Not connected" in muted text, "Connect" button

- [ ] **Step 3: Test empty state with configured workspace**

Navigate to a workspace that has IM channels configured and/or ModelServer connected. Verify:
- IM Channels row shows channel count and provider names in green, "Manage" button
- ModelServer row shows "Connected to {project}" in blue, "Manage" button

- [ ] **Step 4: Test navigation**

Click the "Configure"/"Manage"/"Connect" buttons. Verify:
- Navigates to `/workspaces?tab=settings`
- WorkspaceDetail opens with the Settings tab active
- The correct workspace is selected

- [ ] **Step 5: Test workspace switching**

In the top bar, switch to a different workspace. Verify the status cards update to reflect the new workspace's configuration.

- [ ] **Step 6: Test loading state**

Refresh the page on a workspace view. Verify status text shows non-breaking space (no layout shift) while loading, then populates.
