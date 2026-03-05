import { useState, useEffect, useRef } from 'react'
import { ChevronDown, Plus, Trash2, FolderOpen, Sun, Moon, Monitor, Shield, LogOut, Settings } from 'lucide-react'
import {
  type Workspace,
  createWorkspace,
  deleteWorkspace,
  logout,
} from '../lib/api'
import type { UserInfo } from '../App'
import { ConfirmModal, PromptModal } from './Modals'

interface TopBarProps {
  workspaces: Workspace[]
  setWorkspaces: React.Dispatch<React.SetStateAction<Workspace[]>>
  selectedWorkspaceId: string | null
  onSelectWorkspace: (id: string) => void
  user: UserInfo | null
  onLogout: () => void
  onShowAdmin?: () => void
  onShowManageProjects: () => void
}

function UserAvatar({ name, picture }: { name: string; picture?: string | null }) {
  if (picture) {
    return (
      <img
        src={picture}
        alt={name}
        className="h-7 w-7 shrink-0 rounded-full"
      />
    )
  }
  const initial = (name || '?')[0].toUpperCase()
  return (
    <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-[var(--secondary)] text-xs font-medium text-[var(--foreground)]">
      {initial}
    </div>
  )
}

export function TopBar({
  workspaces,
  setWorkspaces,
  selectedWorkspaceId,
  onSelectWorkspace,
  user,
  onLogout,
  onShowAdmin,
  onShowManageProjects,
}: TopBarProps) {
  const [wsDropdownOpen, setWsDropdownOpen] = useState(false)
  const [menuOpen, setMenuOpen] = useState(false)
  const [showCreateWs, setShowCreateWs] = useState(false)
  const [confirmDeleteWs, setConfirmDeleteWs] = useState<{ id: string; name: string } | null>(null)
  const [quotaError, setQuotaError] = useState<string | null>(null)
  const [theme, setThemeState] = useState<'system' | 'light' | 'dark'>(() => {
    return (localStorage.getItem('theme') as 'light' | 'dark') || 'system'
  })
  const wsDropdownRef = useRef<HTMLDivElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)

  const setTheme = (t: 'system' | 'light' | 'dark') => {
    setThemeState(t)
    if (t === 'system') {
      localStorage.removeItem('theme')
      const dark = window.matchMedia('(prefers-color-scheme: dark)').matches
      document.documentElement.classList.toggle('dark', dark)
    } else {
      localStorage.setItem('theme', t)
      document.documentElement.classList.toggle('dark', t === 'dark')
    }
  }

  useEffect(() => {
    if (!wsDropdownOpen && !menuOpen) return
    const handler = (e: MouseEvent) => {
      if (wsDropdownOpen && wsDropdownRef.current && !wsDropdownRef.current.contains(e.target as Node)) {
        setWsDropdownOpen(false)
      }
      if (menuOpen && menuRef.current && !menuRef.current.contains(e.target as Node)) {
        setMenuOpen(false)
      }
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [wsDropdownOpen, menuOpen])

  const selectedWorkspace = workspaces.find((w) => w.id === selectedWorkspaceId)
  const displayName = user?.name || user?.username || 'User'

  const handleCreateWorkspace = () => {
    setWsDropdownOpen(false)
    setShowCreateWs(true)
  }

  const doCreateWorkspace = async (name: string) => {
    setShowCreateWs(false)
    setQuotaError(null)
    try {
      const ws = await createWorkspace(name)
      setWorkspaces((prev) => [...prev, ws])
      onSelectWorkspace(ws.id)
    } catch (err: unknown) {
      const qe = err as { error?: string; message?: string } | undefined
      if ((qe?.error === 'quota_exceeded' || qe?.error === 'resource_budget_exceeded') && qe.message) {
        setQuotaError(qe.message)
      }
    }
  }

  const handleDeleteWorkspace = (id: string, e: React.MouseEvent) => {
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

  return (
    <>
      <div className="flex h-14 shrink-0 items-center justify-between border-b border-[var(--border)] bg-[var(--card)] px-4">
        {/* Left: brand + workspace selector */}
        <div className="flex items-center gap-4">
          <span className="text-sm font-semibold text-[var(--foreground)]">agentserver</span>

          <div className="relative" ref={wsDropdownRef}>
            <button
              onClick={() => setWsDropdownOpen((v) => !v)}
              className="flex items-center gap-2 rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-1.5 text-sm hover:bg-[var(--secondary)]"
            >
              <FolderOpen size={14} className="shrink-0 text-[var(--muted-foreground)]" />
              <span className="max-w-[160px] truncate">{selectedWorkspace?.name || 'Select workspace'}</span>
              <ChevronDown size={14} className={`shrink-0 transition-transform ${wsDropdownOpen ? 'rotate-180' : ''}`} />
            </button>
            {wsDropdownOpen && (
              <div className="absolute left-0 top-full z-50 mt-1 min-w-[220px] overflow-hidden rounded-md border border-[var(--border)] bg-[var(--card)] shadow-lg">
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
                    <div className="flex items-center gap-2 min-w-0 flex-1">
                      <FolderOpen size={13} className="shrink-0 text-[var(--muted-foreground)]" />
                      <span className="truncate">{ws.name}</span>
                    </div>
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
                <div className="border-t border-[var(--border)]">
                  <button
                    onClick={() => {
                      setWsDropdownOpen(false)
                      onShowManageProjects()
                    }}
                    className="flex w-full items-center gap-2 px-3 py-2 text-sm text-[var(--muted-foreground)] hover:bg-[var(--secondary)]"
                  >
                    <Settings size={14} />
                    Manage Projects
                  </button>
                </div>
              </div>
            )}
          </div>
        </div>

        {/* Right: user menu */}
        <div className="relative" ref={menuRef}>
          <button
            onClick={() => setMenuOpen((v) => !v)}
            className="flex items-center gap-2 rounded-md px-2 py-1.5 hover:bg-[var(--secondary)]"
          >
            <UserAvatar name={displayName} picture={user?.picture} />
            <span className="max-w-[120px] truncate text-sm text-[var(--foreground)]">{displayName}</span>
            <ChevronDown size={14} className={`shrink-0 transition-transform ${menuOpen ? 'rotate-180' : ''}`} />
          </button>
          {menuOpen && (
            <div className="absolute right-0 top-full z-50 mt-1 min-w-[200px] overflow-hidden rounded-md border border-[var(--border)] bg-[var(--card)] shadow-lg">
              <div className="flex items-center justify-between px-3 py-2 border-b border-[var(--border)]">
                <span className="text-xs text-[var(--muted-foreground)]">Theme</span>
                <div className="flex gap-1">
                  <button
                    onClick={() => setTheme('system')}
                    className={`rounded p-1 ${theme === 'system' ? 'bg-[var(--secondary)] text-[var(--foreground)]' : 'text-[var(--muted-foreground)] hover:text-[var(--foreground)]'}`}
                    title="System"
                  >
                    <Monitor size={14} />
                  </button>
                  <button
                    onClick={() => setTheme('light')}
                    className={`rounded p-1 ${theme === 'light' ? 'bg-[var(--secondary)] text-[var(--foreground)]' : 'text-[var(--muted-foreground)] hover:text-[var(--foreground)]'}`}
                    title="Light"
                  >
                    <Sun size={14} />
                  </button>
                  <button
                    onClick={() => setTheme('dark')}
                    className={`rounded p-1 ${theme === 'dark' ? 'bg-[var(--secondary)] text-[var(--foreground)]' : 'text-[var(--muted-foreground)] hover:text-[var(--foreground)]'}`}
                    title="Dark"
                  >
                    <Moon size={14} />
                  </button>
                </div>
              </div>
              {onShowAdmin && (
                <button
                  onClick={() => { onShowAdmin(); setMenuOpen(false) }}
                  className="flex w-full items-center gap-2 px-3 py-2 text-sm text-[var(--foreground)] hover:bg-[var(--secondary)]"
                >
                  <Shield size={14} />
                  Admin
                </button>
              )}
              <button
                onClick={handleLogout}
                className="flex w-full items-center gap-2 px-3 py-2 text-sm text-[var(--foreground)] hover:bg-[var(--secondary)]"
              >
                <LogOut size={14} />
                Log out
              </button>
            </div>
          )}
        </div>
      </div>

      {/* Quota error toast */}
      {quotaError && (
        <div className="mx-4 mt-2 flex items-start gap-2 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-400">
          <span className="flex-1">{quotaError}</span>
          <button onClick={() => setQuotaError(null)} className="shrink-0 font-medium hover:text-red-300">
            Dismiss
          </button>
        </div>
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
    </>
  )
}
