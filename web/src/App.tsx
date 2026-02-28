import { useState, useEffect, useCallback } from 'react'
import { Loader2, ExternalLink, Clock, Activity } from 'lucide-react'
import { checkAuth, listWorkspaces, listSandboxes, getMe, type Workspace, type Sandbox } from './lib/api'
import { Login } from './components/Login'
import { SandboxList } from './components/SandboxList'

export interface UserInfo {
  id: string
  username: string
  email?: string | null
}

export default function App() {
  const [authed, setAuthed] = useState<boolean | null>(null)
  const [user, setUser] = useState<UserInfo | null>(null)
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [selectedWorkspaceId, setSelectedWorkspaceId] = useState<string | null>(null)
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([])
  const [activeSandboxId, setActiveSandboxId] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)

  const refreshSandboxes = useCallback(async () => {
    if (!selectedWorkspaceId) return
    try {
      const list = await listSandboxes(selectedWorkspaceId)
      setSandboxes(list)
    } catch {
      // ignore
    }
  }, [selectedWorkspaceId])

  // On auth, fetch workspaces and auto-select the first one.
  useEffect(() => {
    checkAuth().then((ok) => {
      setAuthed(ok)
      if (ok) {
        listWorkspaces().then((ws) => {
          setWorkspaces(ws)
          if (ws.length > 0) {
            setSelectedWorkspaceId(ws[0].id)
          }
        }).catch(() => {})
        getMe().then(setUser).catch(() => {})
      }
    })
  }, [])

  // On workspace change, fetch sandboxes for that workspace.
  useEffect(() => {
    if (selectedWorkspaceId) {
      refreshSandboxes()
      setActiveSandboxId(null)
    } else {
      setSandboxes([])
      setActiveSandboxId(null)
    }
  }, [selectedWorkspaceId, refreshSandboxes])

  const handleSelectWorkspace = useCallback((id: string) => {
    setSelectedWorkspaceId(id || null)
  }, [])

  const handleLogout = useCallback(() => {
    setAuthed(false)
    setUser(null)
    setWorkspaces([])
    setSelectedWorkspaceId(null)
    setSandboxes([])
    setActiveSandboxId(null)
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
          listWorkspaces().then((ws) => {
            setWorkspaces(ws)
            if (ws.length > 0) {
              setSelectedWorkspaceId(ws[0].id)
            }
          }).catch(() => {})
          getMe().then(setUser).catch(() => {})
        }}
      />
    )
  }

  const activeSandboxData = sandboxes.find((s) => s.id === activeSandboxId)

  let mainContent
  if (creating) {
    mainContent = (
      <div className="flex flex-col items-center justify-center gap-3">
        <Loader2 size={24} className="animate-spin text-[var(--muted-foreground)]" />
        <span className="text-[var(--muted-foreground)]">Creating sandbox...</span>
      </div>
    )
  } else if (activeSandboxId && activeSandboxData) {
    const isRunning = activeSandboxData.status === 'running'
    const opencodeUrl = activeSandboxData.opencodeUrl
    mainContent = (
      <div className="flex flex-col items-center gap-6 w-full max-w-md px-6">
        <div className="w-full rounded-lg border border-[var(--border)] bg-[var(--card)] p-6">
          <h2 className="text-lg font-semibold text-[var(--foreground)] mb-4">{activeSandboxData.name}</h2>
          <div className="flex flex-col gap-3 text-sm">
            <div className="flex items-center gap-2">
              <span className="text-[var(--muted-foreground)]">Status:</span>
              <span className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium ${
                isRunning
                  ? 'bg-green-500/10 text-green-500'
                  : activeSandboxData.status === 'paused'
                    ? 'bg-yellow-500/10 text-yellow-500'
                    : 'bg-gray-500/10 text-[var(--muted-foreground)]'
              }`}>
                <span className={`inline-block h-1.5 w-1.5 rounded-full ${
                  isRunning
                    ? 'bg-green-500'
                    : activeSandboxData.status === 'paused'
                      ? 'bg-yellow-500'
                      : 'bg-gray-500'
                }`} />
                {activeSandboxData.status}
              </span>
            </div>
            <div className="flex items-center gap-2 text-[var(--muted-foreground)]">
              <Clock size={14} />
              <span>Created: {new Date(activeSandboxData.createdAt).toLocaleString()}</span>
            </div>
            {activeSandboxData.lastActivityAt && (
              <div className="flex items-center gap-2 text-[var(--muted-foreground)]">
                <Activity size={14} />
                <span>Last active: {new Date(activeSandboxData.lastActivityAt).toLocaleString()}</span>
              </div>
            )}
          </div>
        </div>
        {isRunning && opencodeUrl ? (
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
            {isRunning ? 'OpenCode URL not configured' : 'Sandbox must be running to open OpenCode'}
          </span>
        )}
      </div>
    )
  } else {
    mainContent = (
      <span className="text-[var(--muted-foreground)]">
        Select or create a sandbox
      </span>
    )
  }

  return (
    <div className="flex h-screen">
      <SandboxList
        workspaces={workspaces}
        setWorkspaces={setWorkspaces}
        selectedWorkspaceId={selectedWorkspaceId}
        onSelectWorkspace={handleSelectWorkspace}
        sandboxes={sandboxes}
        setSandboxes={setSandboxes}
        activeSandboxId={activeSandboxId}
        onSelectSandbox={setActiveSandboxId}
        onRefreshSandboxes={refreshSandboxes}
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
