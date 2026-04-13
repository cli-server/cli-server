import { useSearchParams } from 'react-router-dom'
import { type Workspace } from '../lib/api'
import { WorkspaceDetail } from './WorkspaceDetail'
import { FolderOpen } from 'lucide-react'

interface ManageWorkspacesProps {
  workspaces: Workspace[]
  selectedWorkspaceId: string | null
  onSelectWorkspace: (id: string) => void
  onRenameWorkspace?: (id: string, name: string) => void
}

export function ManageWorkspaces({ workspaces, selectedWorkspaceId, onSelectWorkspace, onRenameWorkspace }: ManageWorkspacesProps) {
  const [searchParams] = useSearchParams()
  const tabParam = searchParams.get('tab')
  const validTabs = ['overview', 'members', 'traces', 'settings']
  const initialTab = (tabParam && validTabs.includes(tabParam)) ? tabParam as 'overview' | 'members' | 'traces' | 'settings' : undefined

  const selectedWorkspace = workspaces.find((w) => w.id === selectedWorkspaceId)

  return (
    <div className="flex h-full">
      {/* Workspace list panel */}
      <div className="w-60 shrink-0 border-r border-[var(--border)] bg-[var(--muted)] overflow-y-auto">
        <div className="px-3 py-3 border-b border-[var(--border)]">
          <span className="text-sm font-medium text-[var(--foreground)]">Workspaces</span>
        </div>
        {workspaces.map((ws) => (
          <div
            key={ws.id}
            onClick={() => onSelectWorkspace(ws.id)}
            className={`flex cursor-pointer items-center gap-2 px-3 py-2 text-sm hover:bg-[var(--secondary)] ${
              selectedWorkspaceId === ws.id ? 'bg-[var(--secondary)]' : ''
            }`}
          >
            <FolderOpen size={14} className="shrink-0 text-[var(--muted-foreground)]" />
            <span className="truncate">{ws.name}</span>
          </div>
        ))}
        {workspaces.length === 0 && (
          <div className="p-3 text-center text-sm text-[var(--muted-foreground)]">
            No workspaces
          </div>
        )}
      </div>

      {/* Detail panel */}
      <div className="flex-1 overflow-hidden">
        {selectedWorkspace ? (
          <WorkspaceDetail workspace={selectedWorkspace} onRename={onRenameWorkspace} initialTab={initialTab} />
        ) : (
          <div className="flex items-center justify-center h-full">
            <span className="text-[var(--muted-foreground)]">Select a workspace</span>
          </div>
        )}
      </div>
    </div>
  )
}
