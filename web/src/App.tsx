import { useState, useEffect, useCallback } from 'react'
import { Loader2, ExternalLink, Clock, Activity } from 'lucide-react'
import { checkAuth, listSessions, getMe, type Session } from './lib/api'
import { Login } from './components/Login'
import { SessionList } from './components/SessionList'

export interface UserInfo {
  id: string
  username: string
  email?: string | null
}

export default function App() {
  const [authed, setAuthed] = useState<boolean | null>(null)
  const [user, setUser] = useState<UserInfo | null>(null)
  const [activeSession, setActiveSession] = useState<string | null>(null)
  const [sessions, setSessions] = useState<Session[]>([])
  const [creating, setCreating] = useState(false)

  const refreshSessions = useCallback(async () => {
    try {
      const list = await listSessions()
      setSessions(list)
    } catch {
      // ignore
    }
  }, [])

  useEffect(() => {
    checkAuth().then((ok) => {
      setAuthed(ok)
      if (ok) {
        refreshSessions()
        getMe().then(setUser).catch(() => {})
      }
    })
  }, [refreshSessions])

  const handleLogout = useCallback(() => {
    setAuthed(false)
    setUser(null)
    setSessions([])
    setActiveSession(null)
  }, [])

  if (authed === null) {
    return (
      <div className="flex min-h-screen items-center justify-center">
        <span className="text-[var(--muted-foreground)]">Loading...</span>
      </div>
    )
  }

  if (!authed) {
    return (
      <Login
        onSuccess={() => {
          setAuthed(true)
          refreshSessions()
          getMe().then(setUser).catch(() => {})
        }}
      />
    )
  }

  const activeSessionData = sessions.find((s) => s.id === activeSession)

  let mainContent
  if (creating) {
    mainContent = (
      <div className="flex flex-col items-center justify-center gap-3">
        <Loader2 size={24} className="animate-spin text-[var(--muted-foreground)]" />
        <span className="text-[var(--muted-foreground)]">Creating session...</span>
      </div>
    )
  } else if (activeSession && activeSessionData) {
    const isRunning = activeSessionData.status === 'running'
    const opencodeUrl = activeSessionData.opencodeUrl || `/oc/${activeSession}/`
    mainContent = (
      <div className="flex flex-col items-center gap-6 w-full max-w-md px-6">
        <div className="w-full rounded-lg border border-[var(--border)] bg-[var(--card)] p-6">
          <h2 className="text-lg font-semibold text-[var(--foreground)] mb-4">{activeSessionData.name}</h2>
          <div className="flex flex-col gap-3 text-sm">
            <div className="flex items-center gap-2">
              <span className="text-[var(--muted-foreground)]">Status:</span>
              <span className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium ${
                isRunning
                  ? 'bg-green-500/10 text-green-500'
                  : activeSessionData.status === 'paused'
                    ? 'bg-yellow-500/10 text-yellow-500'
                    : 'bg-gray-500/10 text-[var(--muted-foreground)]'
              }`}>
                <span className={`inline-block h-1.5 w-1.5 rounded-full ${
                  isRunning
                    ? 'bg-green-500'
                    : activeSessionData.status === 'paused'
                      ? 'bg-yellow-500'
                      : 'bg-gray-500'
                }`} />
                {activeSessionData.status}
              </span>
            </div>
            <div className="flex items-center gap-2 text-[var(--muted-foreground)]">
              <Clock size={14} />
              <span>Created: {new Date(activeSessionData.createdAt).toLocaleString()}</span>
            </div>
            {activeSessionData.lastActivityAt && (
              <div className="flex items-center gap-2 text-[var(--muted-foreground)]">
                <Activity size={14} />
                <span>Last active: {new Date(activeSessionData.lastActivityAt).toLocaleString()}</span>
              </div>
            )}
          </div>
        </div>
        {isRunning ? (
          <a
            href={opencodeUrl}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-2 rounded-md bg-[var(--primary)] px-4 py-2 text-sm font-medium text-[var(--primary-foreground)] hover:opacity-90 transition-opacity"
          >
            <ExternalLink size={16} />
            Open OpenCode
          </a>
        ) : (
          <span className="text-sm text-[var(--muted-foreground)]">
            Session must be running to open OpenCode
          </span>
        )}
      </div>
    )
  } else {
    mainContent = (
      <span className="text-[var(--muted-foreground)]">
        Select or create a session
      </span>
    )
  }

  return (
    <div className="flex h-screen">
      <SessionList
        sessions={sessions}
        setSessions={setSessions}
        activeId={activeSession}
        onSelect={setActiveSession}
        onRefresh={refreshSessions}
        creating={creating}
        setCreating={setCreating}
        user={user}
        onLogout={handleLogout}
      />
      <div className="flex flex-1 items-center justify-center bg-[var(--background)]">
        {mainContent}
      </div>
    </div>
  )
}
