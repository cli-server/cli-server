import { useState, useEffect, useCallback } from 'react'
import { Loader2 } from 'lucide-react'
import { checkAuth, listSessions, getMe, type Session } from './lib/api'
import { Login } from './components/Login'
import { SessionList } from './components/SessionList'
import { Chat } from './components/chat/Chat'

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
    mainContent = (
      <Chat
        key={activeSession}
        sessionId={activeSession}
        status={activeSessionData.status}
        onStatusChange={refreshSessions}
      />
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
