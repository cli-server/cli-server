import { useEffect, useState } from 'react'
import { Plus, Trash2 } from 'lucide-react'
import { type Session, listSessions, createSession, deleteSession } from '../lib/api'

interface SessionListProps {
  activeId: string | null
  onSelect: (id: string) => void
}

export function SessionList({ activeId, onSelect }: SessionListProps) {
  const [sessions, setSessions] = useState<Session[]>([])

  const refresh = async () => {
    try {
      const list = await listSessions()
      setSessions(list)
    } catch {
      // ignore
    }
  }

  useEffect(() => {
    refresh()
  }, [])

  const handleCreate = async () => {
    try {
      const sess = await createSession()
      setSessions((prev) => [...prev, sess])
      onSelect(sess.id)
    } catch {
      // ignore
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

  return (
    <div className="flex h-full w-60 flex-col border-r border-[var(--border)] bg-[var(--muted)]">
      <div className="flex items-center justify-between border-b border-[var(--border)] p-3">
        <span className="text-sm font-medium">Sessions</span>
        <button
          onClick={handleCreate}
          className="rounded p-1 hover:bg-[var(--secondary)]"
          title="New session"
        >
          <Plus size={16} />
        </button>
      </div>
      <div className="flex-1 overflow-y-auto">
        {sessions.map((sess) => (
          <div
            key={sess.id}
            onClick={() => onSelect(sess.id)}
            className={`group flex cursor-pointer items-center justify-between px-3 py-2 text-sm hover:bg-[var(--secondary)] ${
              activeId === sess.id ? 'bg-[var(--secondary)]' : ''
            }`}
          >
            <span className="truncate">{sess.name}</span>
            <button
              onClick={(e) => handleDelete(sess.id, e)}
              className="hidden rounded p-1 hover:bg-[var(--destructive)] hover:text-white group-hover:block"
              title="Delete session"
            >
              <Trash2 size={14} />
            </button>
          </div>
        ))}
        {sessions.length === 0 && (
          <div className="p-3 text-center text-sm text-[var(--muted-foreground)]">
            No sessions yet
          </div>
        )}
      </div>
    </div>
  )
}
