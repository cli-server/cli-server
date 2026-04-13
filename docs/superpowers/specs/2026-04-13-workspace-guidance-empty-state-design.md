# Workspace Guidance Empty State

## Problem

When users enter a workspace (`/w/{workspaceId}`) with no sandbox selected, they see only a grey "Select or create a sandbox" message. There is no discoverability for two key workspace-level setup actions:

1. Binding IM channels (WeChat, Telegram, Matrix)
2. Connecting ModelServer for LLM inference

These configurations are buried in the **Manage Workspaces** page (`/workspaces`) under the Settings tab. New users have no way to discover them from the main workspace view.

## Solution

Replace the static empty-state text with a **status-aware guidance panel** that shows real-time configuration status and provides quick-jump links to the settings page.

## Architecture

### New Component: `WorkspaceEmptyState`

**File:** `web/src/components/WorkspaceEmptyState.tsx`

**Props:**
```typescript
interface WorkspaceEmptyStateProps {
  workspaceId: string
}
```

**Data fetching (on mount, and when `workspaceId` changes):**
- `listWorkspaceIMChannels(workspaceId)` — returns `IMChannel[]`
- `getModelserverStatus(workspaceId)` — returns `ModelserverStatus`

Both API functions already exist in `web/src/lib/api.ts`.

### Layout

```
┌─────────────────────────────────────────────────────┐
│                                                     │
│        Select or create a sandbox                   │
│                                                     │
│  ┌─────────────────────────────────────────────┐    │
│  │  Quick Setup                                │    │
│  │                                             │    │
│  │  ┌─────────────────────────────────────┐    │    │
│  │  │ [MessageSquare] IM Channels         │    │    │
│  │  │ No channels configured              │    │    │
│  │  │                     [Configure ->]  │    │    │
│  │  └─────────────────────────────────────┘    │    │
│  │                                             │    │
│  │  ┌─────────────────────────────────────┐    │    │
│  │  │ [Box] ModelServer                   │    │    │
│  │  │ Not connected                       │    │    │
│  │  │                       [Connect ->]  │    │    │
│  │  └─────────────────────────────────────┘    │    │
│  │                                             │    │
│  └─────────────────────────────────────────────┘    │
│                                                     │
└─────────────────────────────────────────────────────┘
```

### Status Display States

**IM Channels:**

| State | Status text | Button label |
|-------|-----------|-------------|
| No channels | "No channels configured" (muted) | "Configure" |
| Has channels | "{n} channel(s): WeChat, Telegram" with provider badges | "Manage" |

**ModelServer:**

| State | Status text | Button label |
|-------|-----------|-------------|
| Not connected | "Not connected" (muted) | "Connect" |
| Connected | "Connected to {project_name}" (green) | "Manage" |

### Navigation

All action buttons navigate to: `/workspaces` — this opens the Manage Workspaces page. The currently selected workspace is preserved via the app-level `selectedWorkspaceId` state, and the WorkspaceDetail component defaults to showing the overview tab. The Settings tab containing IM and ModelServer configuration is one click away.

We use `react-router-dom`'s `useNavigate()` for SPA navigation.

### Styling

Follows existing card patterns from WorkspaceDetail.tsx:
- Outer container: `rounded-lg border border-[var(--border)] bg-[var(--card)]`
- Section header: `text-sm font-medium text-[var(--foreground)]`
- Status rows: `border border-[var(--border)] bg-[var(--background)] rounded-md px-4 py-3`
- Provider badges: same color scheme as WorkspaceDetail (green=WeChat, blue=Telegram, purple=Matrix)
- Connected state: green accent text
- Action buttons: `text-xs font-medium` with hover, matching existing button styles

### Loading State

While fetching status data, show a subtle loading state (skeleton or muted placeholder text). Do not block the "Select or create a sandbox" message — it should appear immediately.

## Changes Required

### `web/src/components/WorkspaceEmptyState.tsx` (new file)

New component implementing the design above. Imports:
- `listWorkspaceIMChannels`, `getModelserverStatus` from `../lib/api`
- `MessageSquare`, `Box`, `ArrowRight` from `lucide-react`
- `useNavigate` from `react-router-dom`

### `web/src/App.tsx` (modify)

Replace the static `defaultContent` block (lines 276-278):

```tsx
// Before
<div className="flex items-center justify-center h-full">
  <span className="text-[var(--muted-foreground)]">Select or create a sandbox</span>
</div>

// After
<WorkspaceEmptyState workspaceId={selectedWorkspaceId!} />
```

Import `WorkspaceEmptyState` from `./components/WorkspaceEmptyState`.

No other files need to be modified. No API changes. No backend changes.

## Testing

- Verify empty state renders with correct status for a workspace with:
  - No IM channels and no ModelServer → both show "not configured/connected"
  - IM channels configured → shows count and provider names
  - ModelServer connected → shows project name
- Verify navigation: clicking action buttons goes to `/workspaces`
- Verify loading state: no flash of error while data loads
- Verify workspace switching: status updates when selecting a different workspace
