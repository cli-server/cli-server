import { useState, useEffect, useRef } from 'react'
import { Plus, Trash2, Pause, Play, Loader2, LogOut, ExternalLink } from 'lucide-react'
import {
  type Session,
  createSession,
  deleteSession,
  pauseSession,
  resumeSession,
  logout,
} from '../lib/api'
import type { UserInfo } from '../App'

interface SessionListProps {
  sessions: Session[]
  setSessions: React.Dispatch<React.SetStateAction<Session[]>>
  activeId: string | null
  onSelect: (id: string) => void
  onRefresh: () => void
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

export function SessionList({ sessions, setSessions, activeId, onSelect, onRefresh, creating, setCreating, user, onLogout }: SessionListProps) {
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const [menuOpen, setMenuOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)

  // Close menu on outside click
  useEffect(() => {
    if (!menuOpen) return
    const handler = (e: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
        setMenuOpen(false)
      }
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [menuOpen])

  // Poll when any session is in a transitional state.
  useEffect(() => {
    const hasTransitional = sessions.some(
      (s) => s.status === 'pausing' || s.status === 'resuming' || s.status === 'creating'
    )
    if (hasTransitional) {
      if (!pollRef.current) {
        pollRef.current = setInterval(onRefresh, 2000)
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
  }, [sessions, onRefresh])

  const handleCreate = async () => {
    if (creating) return
    setCreating(true)
    onSelect('') // clear current selection so main area shows "creating" state
    try {
      const sess = await createSession()
      setSessions((prev) => [...prev, sess])
      onSelect(sess.id)
    } catch {
      // ignore
    } finally {
      setCreating(false)
    }
  }

  const handleDelete = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    try {
      await deleteSession(id)
      setSessions((prev) => prev.filter((s) => s.id !== id))
      if (activeId === id) {
        onSelect('')
      }
    } catch {
      // ignore
    }
  }

  const handlePause = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    try {
      await pauseSession(id)
      setSessions((prev) =>
        prev.map((s) => (s.id === id ? { ...s, status: 'pausing' } : s))
      )
    } catch {
      // ignore
    }
  }

  const handleResume = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    try {
      await resumeSession(id)
      setSessions((prev) =>
        prev.map((s) => (s.id === id ? { ...s, status: 'resuming' } : s))
      )
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

  const displayName = user?.username || 'User'

  return (
    <div className="flex h-full w-60 flex-col border-r border-[var(--border)] bg-[var(--muted)]">
      <div className="flex items-center justify-between border-b border-[var(--border)] p-3">
        <span className="text-sm font-medium">Sessions</span>
        <button
          onClick={handleCreate}
          disabled={creating}
          className="rounded p-1 hover:bg-[var(--secondary)] disabled:opacity-50"
          title="New session"
        >
          {creating ? <Loader2 size={16} className="animate-spin" /> : <Plus size={16} />}
        </button>
      </div>
      <div className="flex-1 overflow-y-auto">
        {sessions.map((sess) => (
          <div
            key={sess.id}
            onClick={() => onSelect(sess.id)}
            className={`group flex cursor-pointer items-center gap-2 px-3 py-2 text-sm hover:bg-[var(--secondary)] ${
              activeId === sess.id ? 'bg-[var(--secondary)]' : ''
            }`}
          >
            <StatusDot status={sess.status} />
            <span className="flex-1 truncate">{sess.name}</span>
            <div className="hidden gap-0.5 group-hover:flex">
              {sess.status === 'running' && (
                <>
                  <a
                    href={sess.opencodeUrl || `/oc/${sess.id}/`}
                    onClick={(e) => e.stopPropagation()}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="rounded p-1 hover:bg-[var(--muted-foreground)]/20"
                    title="Open in OpenCode"
                  >
                    <ExternalLink size={12} />
                  </a>
                  <button
                    onClick={(e) => handlePause(sess.id, e)}
                    className="rounded p-1 hover:bg-[var(--muted-foreground)]/20"
                    title="Pause session"
                  >
                    <Pause size={12} />
                  </button>
                </>
              )}
              {sess.status === 'paused' && (
                <button
                  onClick={(e) => handleResume(sess.id, e)}
                  className="rounded p-1 hover:bg-[var(--muted-foreground)]/20"
                  title="Resume session"
                >
                  <Play size={12} />
                </button>
              )}
              <button
                onClick={(e) => handleDelete(sess.id, e)}
                className="rounded p-1 hover:bg-[var(--destructive)] hover:text-white"
                title="Delete session"
              >
                <Trash2 size={12} />
              </button>
            </div>
          </div>
        ))}
        {sessions.length === 0 && !creating && (
          <div className="p-3 text-center text-sm text-[var(--muted-foreground)]">
            No sessions yet
          </div>
        )}
      </div>
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
    </div>
  )
}
