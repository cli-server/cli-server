import { useState, useEffect, useRef } from 'react'
import { Plus, Trash2, Pause, Play, Loader2, LogOut, ChevronDown } from 'lucide-react'
import {
  type Workspace,
  type Sandbox,
  createSandbox,
  deleteSandbox,
  pauseSandbox,
  resumeSandbox,
  createWorkspace,
  deleteWorkspace,
  logout,
} from '../lib/api'
import type { UserInfo } from '../App'
import { CreateSandboxModal } from './CreateSandboxModal'
import { ConfirmModal, PromptModal } from './Modals'

interface SandboxListProps {
  workspaces: Workspace[]
  setWorkspaces: React.Dispatch<React.SetStateAction<Workspace[]>>
  selectedWorkspaceId: string | null
  onSelectWorkspace: (id: string) => void
  sandboxes: Sandbox[]
  setSandboxes: React.Dispatch<React.SetStateAction<Sandbox[]>>
  activeSandboxId: string | null
  onSelectSandbox: (id: string) => void
  onRefreshSandboxes: () => void
  creating: boolean
  setCreating: (v: boolean) => void
  user: UserInfo | null
  onLogout: () => void
}

function StatusDot({ status }: { status: string }) {
  switch (status) {
    case 'running':
      return <span className="inline-block h-2 w-2 rounded-full bg-green-500" title="Running" />
    case 'paused':
      return <span className="inline-block h-2 w-2 rounded-full bg-yellow-500" title="Paused" />
    case 'pausing':
    case 'resuming':
    case 'creating':
      return <Loader2 size={10} className="animate-spin text-[var(--muted-foreground)]" />
    default:
      return <span className="inline-block h-2 w-2 rounded-full bg-gray-500" />
  }
}

function UserAvatar({ name }: { name: string }) {
  const initial = (name || '?')[0].toUpperCase()
  return (
    <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-[var(--secondary)] text-xs font-medium text-[var(--foreground)]">
      {initial}
    </div>
  )
}

export function SandboxList({
  workspaces,
  setWorkspaces,
  selectedWorkspaceId,
  onSelectWorkspace,
  sandboxes,
  setSandboxes,
  activeSandboxId,
  onSelectSandbox,
  onRefreshSandboxes,
  creating,
  setCreating,
  user,
  onLogout,
}: SandboxListProps) {
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const [menuOpen, setMenuOpen] = useState(false)
  const [wsDropdownOpen, setWsDropdownOpen] = useState(false)
  const [showCreateModal, setShowCreateModal] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState<{ id: string; name: string } | null>(null)
  const [confirmPause, setConfirmPause] = useState<{ id: string; name: string } | null>(null)
  const [confirmDeleteWs, setConfirmDeleteWs] = useState<{ id: string; name: string } | null>(null)
  const [showCreateWs, setShowCreateWs] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)
  const wsDropdownRef = useRef<HTMLDivElement>(null)

  // Close menu on outside click
  useEffect(() => {
    if (!menuOpen && !wsDropdownOpen) return
    const handler = (e: MouseEvent) => {
      if (menuOpen && menuRef.current && !menuRef.current.contains(e.target as Node)) {
        setMenuOpen(false)
      }
      if (wsDropdownOpen && wsDropdownRef.current && !wsDropdownRef.current.contains(e.target as Node)) {
        setWsDropdownOpen(false)
      }
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [menuOpen, wsDropdownOpen])

  // Poll when any sandbox is in a transitional state.
  useEffect(() => {
    const hasTransitional = sandboxes.some(
      (s) => s.status === 'pausing' || s.status === 'resuming' || s.status === 'creating'
    )
    if (hasTransitional) {
      if (!pollRef.current) {
        pollRef.current = setInterval(onRefreshSandboxes, 2000)
      }
    } else {
      if (pollRef.current) {
        clearInterval(pollRef.current)
        pollRef.current = null
      }
    }
    return () => {
      if (pollRef.current) {
        clearInterval(pollRef.current)
        pollRef.current = null
      }
    }
  }, [sandboxes, onRefreshSandboxes])

  const handleCreateSandbox = async (
    name: string,
    type: 'opencode' | 'openclaw',
    telegramBotToken?: string
  ) => {
    if (creating || !selectedWorkspaceId) return
    setCreating(true)
    setShowCreateModal(false)
    onSelectSandbox('')
    try {
      const sbx = await createSandbox(selectedWorkspaceId, name, type, telegramBotToken)
      setSandboxes((prev) => [...prev, sbx])
      onSelectSandbox(sbx.id)
    } catch {
      // ignore
    } finally {
      setCreating(false)
    }
  }

  const handleDelete = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    const sbx = sandboxes.find((s) => s.id === id)
    setConfirmDelete({ id, name: sbx?.name || 'this sandbox' })
  }

  const doDelete = async (id: string) => {
    setConfirmDelete(null)
    try {
      await deleteSandbox(id)
      setSandboxes((prev) => prev.filter((s) => s.id !== id))
      if (activeSandboxId === id) {
        onSelectSandbox('')
      }
    } catch {
      // ignore
    }
  }

  const handlePause = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    const sbx = sandboxes.find((s) => s.id === id)
    setConfirmPause({ id, name: sbx?.name || 'this sandbox' })
  }

  const doPause = async (id: string) => {
    setConfirmPause(null)
    try {
      await pauseSandbox(id)
      setSandboxes((prev) =>
        prev.map((s) => (s.id === id ? { ...s, status: 'pausing' as const } : s))
      )
    } catch {
      // ignore
    }
  }

  const handleResume = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    try {
      await resumeSandbox(id)
      setSandboxes((prev) =>
        prev.map((s) => (s.id === id ? { ...s, status: 'resuming' as const } : s))
      )
    } catch {
      // ignore
    }
  }

  const handleCreateWorkspace = async () => {
    setWsDropdownOpen(false)
    setShowCreateWs(true)
  }

  const doCreateWorkspace = async (name: string) => {
    setShowCreateWs(false)
    try {
      const ws = await createWorkspace(name)
      setWorkspaces((prev) => [...prev, ws])
      onSelectWorkspace(ws.id)
    } catch {
      // ignore
    }
  }

  const handleDeleteWorkspace = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    setWsDropdownOpen(false)
    const ws = workspaces.find((w) => w.id === id)
    setConfirmDeleteWs({ id, name: ws?.name || 'this workspace' })
  }

  const doDeleteWorkspace = async (id: string) => {
    setConfirmDeleteWs(null)
    try {
      await deleteWorkspace(id)
      setWorkspaces((prev) => prev.filter((w) => w.id !== id))
      if (selectedWorkspaceId === id) {
        onSelectWorkspace('')
      }
    } catch {
      // ignore
    }
  }

  const handleLogout = async () => {
    setMenuOpen(false)
    try {
      await logout()
    } catch {
      // ignore
    }
    onLogout()
  }

  const selectedWorkspace = workspaces.find((w) => w.id === selectedWorkspaceId)
  const displayName = user?.username || 'User'

  return (
    <div className="flex h-full w-60 flex-col border-r border-[var(--border)] bg-[var(--muted)]">
      {/* Workspace selector */}
      <div className="relative border-b border-[var(--border)]" ref={wsDropdownRef}>
        <button
          onClick={() => setWsDropdownOpen((v) => !v)}
          className="flex w-full items-center justify-between px-3 py-3 text-sm font-medium hover:bg-[var(--secondary)]"
        >
          <span className="truncate">{selectedWorkspace?.name || 'Select workspace'}</span>
          <ChevronDown size={14} className={`transition-transform ${wsDropdownOpen ? 'rotate-180' : ''}`} />
        </button>
        {wsDropdownOpen && (
          <div className="absolute left-0 right-0 top-full z-10 border border-[var(--border)] bg-[var(--card)] shadow-lg">
            {workspaces.map((ws) => (
              <div
                key={ws.id}
                onClick={() => {
                  onSelectWorkspace(ws.id)
                  setWsDropdownOpen(false)
                }}
                className={`group flex cursor-pointer items-center justify-between px-3 py-2 text-sm hover:bg-[var(--secondary)] ${
                  selectedWorkspaceId === ws.id ? 'bg-[var(--secondary)]' : ''
                }`}
              >
                <span className="truncate">{ws.name}</span>
                <button
                  onClick={(e) => handleDeleteWorkspace(ws.id, e)}
                  className="hidden rounded p-1 hover:bg-[var(--destructive)] hover:text-white group-hover:block"
                  title="Delete workspace"
                >
                  <Trash2 size={12} />
                </button>
              </div>
            ))}
            <button
              onClick={handleCreateWorkspace}
              className="flex w-full items-center gap-2 px-3 py-2 text-sm text-[var(--muted-foreground)] hover:bg-[var(--secondary)]"
            >
              <Plus size={14} />
              New workspace
            </button>
          </div>
        )}
      </div>

      {/* Sandbox header */}
      <div className="flex items-center justify-between border-b border-[var(--border)] p-3">
        <span className="text-sm font-medium">Sandboxes</span>
        <button
          onClick={() => setShowCreateModal(true)}
          disabled={creating || !selectedWorkspaceId}
          className="rounded p-1 hover:bg-[var(--secondary)] disabled:opacity-50"
          title="New sandbox"
        >
          {creating ? <Loader2 size={16} className="animate-spin" /> : <Plus size={16} />}
        </button>
      </div>

      {/* Sandbox list */}
      <div className="flex-1 overflow-y-auto">
        {sandboxes.map((sbx) => (
          <div
            key={sbx.id}
            onClick={() => onSelectSandbox(sbx.id)}
            className={`group flex cursor-pointer items-center gap-2 px-3 py-2 text-sm hover:bg-[var(--secondary)] ${
              activeSandboxId === sbx.id ? 'bg-[var(--secondary)]' : ''
            }`}
          >
            <StatusDot status={sbx.status} />
            <span className="flex-1 truncate">{sbx.name}</span>
            {sbx.type === 'openclaw' && (
              <span className="shrink-0 rounded bg-purple-500/15 px-1.5 py-0.5 text-[10px] font-medium text-purple-400">
                claw
              </span>
            )}
            <div className="hidden gap-0.5 group-hover:flex">
              {sbx.status === 'running' && (
                <button
                  onClick={(e) => handlePause(sbx.id, e)}
                  className="rounded p-1 hover:bg-[var(--muted-foreground)]/20"
                  title="Pause sandbox"
                >
                  <Pause size={12} />
                </button>
              )}
              {sbx.status === 'paused' && (
                <button
                  onClick={(e) => handleResume(sbx.id, e)}
                  className="rounded p-1 hover:bg-[var(--muted-foreground)]/20"
                  title="Resume sandbox"
                >
                  <Play size={12} />
                </button>
              )}
              <button
                onClick={(e) => handleDelete(sbx.id, e)}
                className="rounded p-1 hover:bg-[var(--destructive)] hover:text-white"
                title="Delete sandbox"
              >
                <Trash2 size={12} />
              </button>
            </div>
          </div>
        ))}
        {sandboxes.length === 0 && !creating && selectedWorkspaceId && (
          <div className="p-3 text-center text-sm text-[var(--muted-foreground)]">
            No sandboxes yet
          </div>
        )}
        {!selectedWorkspaceId && (
          <div className="p-3 text-center text-sm text-[var(--muted-foreground)]">
            Select a workspace
          </div>
        )}
      </div>

      {/* User profile */}
      <div className="relative border-t border-[var(--border)]" ref={menuRef}>
        {menuOpen && (
          <div className="absolute bottom-full left-0 right-0 mb-1 mx-2 overflow-hidden rounded-md border border-[var(--border)] bg-[var(--card)] shadow-lg">
            <button
              onClick={handleLogout}
              className="flex w-full items-center gap-2 px-3 py-2 text-sm text-[var(--foreground)] hover:bg-[var(--secondary)]"
            >
              <LogOut size={14} />
              Log out
            </button>
          </div>
        )}
        <button
          onClick={() => setMenuOpen((v) => !v)}
          className="flex w-full items-center gap-2 px-3 py-3 text-left hover:bg-[var(--secondary)]"
        >
          <UserAvatar name={displayName} />
          <div className="min-w-0 flex-1">
            <div className="truncate text-sm font-medium text-[var(--foreground)]">{displayName}</div>
            {user?.email && (
              <div className="truncate text-xs text-[var(--muted-foreground)]">{user.email}</div>
            )}
          </div>
        </button>
      </div>

      {showCreateModal && (
        <CreateSandboxModal
          onClose={() => setShowCreateModal(false)}
          onCreate={handleCreateSandbox}
          creating={creating}
        />
      )}

      {confirmDelete && (
        <ConfirmModal
          title="Delete Sandbox"
          message={`Are you sure you want to delete "${confirmDelete.name}"? This action cannot be undone.`}
          confirmLabel="Delete"
          destructive
          onConfirm={() => doDelete(confirmDelete.id)}
          onCancel={() => setConfirmDelete(null)}
        />
      )}

      {confirmPause && (
        <ConfirmModal
          title="Pause Sandbox"
          message={`Are you sure you want to pause "${confirmPause.name}"?`}
          confirmLabel="Pause"
          onConfirm={() => doPause(confirmPause.id)}
          onCancel={() => setConfirmPause(null)}
        />
      )}

      {confirmDeleteWs && (
        <ConfirmModal
          title="Delete Workspace"
          message={`Are you sure you want to delete workspace "${confirmDeleteWs.name}"? All sandboxes in this workspace will be stopped and removed.`}
          confirmLabel="Delete"
          destructive
          onConfirm={() => doDeleteWorkspace(confirmDeleteWs.id)}
          onCancel={() => setConfirmDeleteWs(null)}
        />
      )}

      {showCreateWs && (
        <PromptModal
          title="New Workspace"
          label="Workspace name"
          defaultValue="New Workspace"
          confirmLabel="Create"
          onConfirm={doCreateWorkspace}
          onCancel={() => setShowCreateWs(false)}
        />
      )}
    </div>
  )
}
