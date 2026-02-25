import { useState, useEffect } from 'react'
import { checkAuth } from './lib/api'
import { Login } from './components/Login'
import { SessionList } from './components/SessionList'
import { Terminal } from './components/Terminal'

export default function App() {
  const [authed, setAuthed] = useState<boolean | null>(null)
  const [activeSession, setActiveSession] = useState<string | null>(null)

  useEffect(() => {
    checkAuth().then(setAuthed)
  }, [])

  if (authed === null) {
    return (
      <div className="flex min-h-screen items-center justify-center">
        <span className="text-[var(--muted-foreground)]">Loading...</span>
      </div>
    )
  }

  if (!authed) {
    return <Login onSuccess={() => setAuthed(true)} />
  }

  return (
    <div className="flex h-screen">
      <SessionList activeId={activeSession} onSelect={setActiveSession} />
      <div className="flex flex-1 items-center justify-center bg-[var(--background)]">
        {activeSession ? (
          <Terminal key={activeSession} sessionId={activeSession} />
        ) : (
          <span className="text-[var(--muted-foreground)]">
            Select or create a session
          </span>
        )}
      </div>
    </div>
  )
}
