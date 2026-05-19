import { useState, useEffect, useCallback } from 'react'
import { Routes, Route, useNavigate, useParams, useLocation, useSearchParams, Navigate } from 'react-router-dom'
import {
  checkAuth,
  listWorkspaces,
  listSandboxes,
  getMe,
  pauseSandbox,
  resumeSandbox,
  submitOAuthLogin,
  deleteSandbox,
  type Workspace,
  type Sandbox,
} from './lib/api'
import { Login } from './components/Login'
import { OAuthConsent } from './components/OAuthConsent'
import { OAuthDevice } from './components/OAuthDevice'
import { OAuthLogin, PENDING_LOGIN_CHALLENGE_KEY } from './components/OAuthLogin'

const PENDING_CONSENT_CHALLENGE_KEY = 'agentserver_pending_consent_challenge'
const PENDING_DEVICE_PARAMS_KEY = 'agentserver_pending_device_params'
import { TopBar } from './components/TopBar'
import { SandboxDetail } from './components/SandboxDetail'
import { ManageWorkspaces } from './components/ManageWorkspaces'
import { AdminPanel } from './components/AdminPanel'
import { WorkspaceDetail, type Tab as WorkspaceTab } from './components/WorkspaceDetail'

export interface UserInfo {
  id: string
  email: string
  name?: string | null
  picture?: string | null
  role: string
}

function WorkspaceDetailRoute({
  workspaces,
  workspacesLoaded,
  onRename,
  initialTab,
  sandboxOverride,
}: {
  workspaces: Workspace[]
  workspacesLoaded: boolean
  onRename?: (id: string, name: string) => void
  initialTab?: WorkspaceTab
  sandboxOverride?: React.ReactNode
}) {
  const { workspaceId } = useParams<{ workspaceId: string }>()
  if (!workspacesLoaded) {
    // List still loading — don't redirect to "/" on a direct/reload
    // visit to /w/<id>; otherwise the user gets bounced to the first
    // workspace before find() ever sees a populated list.
    return null
  }
  const workspace = workspaces.find((w) => w.id === workspaceId)
  if (!workspace) {
    return <Navigate to="/" replace />
  }
  return (
    <WorkspaceDetail
      workspace={workspace}
      onRename={onRename}
      initialTab={initialTab}
      sandboxOverride={sandboxOverride}
    />
  )
}

function SandboxDetailRoute({
  sandboxes,
  onPause,
  onResume,
  onDelete,
  onRename,
}: {
  sandboxes: Sandbox[]
  onPause: (id: string) => void
  onResume: (id: string) => void
  onDelete: (id: string) => void
  onRename?: (id: string, name: string) => void
}) {
  const { sandboxId } = useParams<{ sandboxId: string }>()
  const sandbox = sandboxes.find((s) => s.id === sandboxId)
  if (!sandbox) {
    return (
      <div className="flex items-center justify-center h-full">
        <span className="text-[var(--muted-foreground)]">Sandbox not found</span>
      </div>
    )
  }
  return (
    <SandboxDetail
      sandbox={sandbox}
      onPause={onPause}
      onResume={onResume}
      onDelete={onDelete}
      onRename={onRename}
    />
  )
}

function OAuthLoginRoute() {
  const [searchParams] = useSearchParams()
  const challenge = searchParams.get('login_challenge') ?? ''
  if (!challenge) return <div>Missing login_challenge</div>
  return <OAuthLogin challenge={challenge} />
}

function OAuthDeviceRoute() {
  const [searchParams] = useSearchParams()
  const challenge = searchParams.get('device_challenge') ?? ''
  const userCode = searchParams.get('user_code') ?? ''
  if (!challenge) return <div>Missing device_challenge</div>
  return <OAuthDevice challenge={challenge} userCode={userCode} />
}

function OAuthConsentRoute() {
  const [searchParams] = useSearchParams()
  const challenge = searchParams.get('consent_challenge') ?? ''
  if (!challenge) return <div>Missing consent_challenge</div>
  return <OAuthConsent challenge={challenge} />
}

export default function App() {
  const navigate = useNavigate()
  const location = useLocation()

  const [authed, setAuthed] = useState<boolean | null>(null)
  const [user, setUser] = useState<UserInfo | null>(null)
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [workspacesLoaded, setWorkspacesLoaded] = useState(false)
  const [selectedWorkspaceId, setSelectedWorkspaceId] = useState<string | null>(null)
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([])

  const refreshSandboxes = useCallback(async () => {
    if (!selectedWorkspaceId) return
    try {
      const list = await listSandboxes(selectedWorkspaceId)
      setSandboxes(list)
    } catch {
      // ignore
    }
  }, [selectedWorkspaceId])

  useEffect(() => {
    checkAuth().then(async (ok) => {
      setAuthed(ok)
      if (ok) {
        // After OIDC login redirect, complete any pending OAuth challenge.
        const pendingLogin = sessionStorage.getItem(PENDING_LOGIN_CHALLENGE_KEY)
        if (pendingLogin) {
          sessionStorage.removeItem(PENDING_LOGIN_CHALLENGE_KEY)
          try {
            const { redirect_to } = await submitOAuthLogin(pendingLogin)
            window.location.href = redirect_to
            return
          } catch {
            // Challenge expired or invalid, continue to normal app.
          }
        }
        const pendingConsent = sessionStorage.getItem(PENDING_CONSENT_CHALLENGE_KEY)
        if (pendingConsent) {
          sessionStorage.removeItem(PENDING_CONSENT_CHALLENGE_KEY)
          window.location.href = `/oauth2/consent?consent_challenge=${encodeURIComponent(pendingConsent)}`
          return
        }
        const pendingDevice = sessionStorage.getItem(PENDING_DEVICE_PARAMS_KEY)
        if (pendingDevice) {
          sessionStorage.removeItem(PENDING_DEVICE_PARAMS_KEY)
          window.location.href = `/oauth2/device${pendingDevice}`
          return
        }
        listWorkspaces().then((ws) => {
          setWorkspaces(ws)
          // Use workspace ID from URL if valid, otherwise default to first
          const match = window.location.pathname.match(/^\/w\/([^/]+)/)
          const urlWsId = match?.[1]
          if (urlWsId && ws.some(w => w.id === urlWsId)) {
            setSelectedWorkspaceId(urlWsId)
          } else if (ws.length > 0) {
            setSelectedWorkspaceId(ws[0].id)
          }
        }).catch(() => {}).finally(() => setWorkspacesLoaded(true))
        getMe().then(setUser).catch(() => {})
      }
    })
  }, [])

  // Sync selectedWorkspaceId from URL on back/forward navigation
  useEffect(() => {
    const match = location.pathname.match(/^\/w\/([^/]+)/)
    const urlWsId = match?.[1]
    if (urlWsId && urlWsId !== selectedWorkspaceId && workspaces.some(w => w.id === urlWsId)) {
      setSelectedWorkspaceId(urlWsId)
    }
  }, [location.pathname, workspaces])

  useEffect(() => {
    if (selectedWorkspaceId) {
      refreshSandboxes()
    } else {
      setSandboxes([])
    }
  }, [selectedWorkspaceId, refreshSandboxes])

  const handleSelectWorkspace = useCallback((id: string) => {
    setSelectedWorkspaceId(id || null)
    navigate(id ? `/w/${id}` : '/')
  }, [navigate])

  const handleLogout = useCallback(() => {
    setAuthed(false)
    setUser(null)
    setWorkspaces([])
    setSelectedWorkspaceId(null)
    setSandboxes([])
    navigate('/')
  }, [navigate])

  const handlePause = useCallback(async (id: string) => {
    try {
      await pauseSandbox(id)
      setSandboxes((prev) => prev.map((s) => (s.id === id ? { ...s, status: 'pausing' as const } : s)))
    } catch { /* ignore */ }
  }, [])

  const handleResume = useCallback(async (id: string) => {
    try {
      await resumeSandbox(id)
      setSandboxes((prev) => prev.map((s) => (s.id === id ? { ...s, status: 'resuming' as const } : s)))
    } catch { /* ignore */ }
  }, [])

  const handleDelete = useCallback(async (id: string) => {
    try {
      await deleteSandbox(id)
      setSandboxes((prev) => prev.filter((s) => s.id !== id))
      navigate(selectedWorkspaceId ? `/w/${selectedWorkspaceId}` : '/')
    } catch { /* ignore */ }
  }, [navigate, selectedWorkspaceId])

  const handleRenameWorkspace = useCallback((id: string, name: string) => {
    setWorkspaces((prev) => prev.map((w) => (w.id === id ? { ...w, name } : w)))
  }, [])

  const handleRenameSandbox = useCallback((id: string, name: string) => {
    setSandboxes((prev) => prev.map((s) => (s.id === id ? { ...s, name } : s)))
  }, [])

  // OAuth login page bypasses the auth guard — it embeds its own Login component.
  // Device and consent pages stay behind the guard (user must be authenticated to approve).
  if (location.pathname === '/oauth2/login') {
    return <OAuthLoginRoute />
  }

  if (authed === null) {
    return (
      <div className="flex min-h-screen items-center justify-center">
        <span className="text-[var(--muted-foreground)]">Loading...</span>
      </div>
    )
  }

  if (!authed) {
    // If landing on protected OAuth pages without auth, save params
    // so they survive OIDC redirects.
    if (location.pathname === '/oauth2/consent') {
      const params = new URLSearchParams(location.search)
      const challenge = params.get('consent_challenge')
      if (challenge) {
        sessionStorage.setItem(PENDING_CONSENT_CHALLENGE_KEY, challenge)
      }
    }
    if (location.pathname === '/oauth2/device') {
      sessionStorage.setItem(PENDING_DEVICE_PARAMS_KEY, location.search)
    }
    return (
      <Login
        onSuccess={() => {
          setAuthed(true)
          listWorkspaces().then((ws) => {
            setWorkspaces(ws)
            if (ws.length > 0) setSelectedWorkspaceId(ws[0].id)
          }).catch(() => {})
          getMe().then(setUser).catch(() => {})
        }}
      />
    )
  }

  return (
    <div className="flex flex-col h-screen">
      <TopBar
        workspaces={workspaces}
        setWorkspaces={setWorkspaces}
        selectedWorkspaceId={selectedWorkspaceId}
        onSelectWorkspace={handleSelectWorkspace}
        user={user}
        onLogout={handleLogout}
        onShowAdmin={user?.role === 'admin' ? () => navigate('/admin') : undefined}
        onShowManageWorkspaces={() => navigate('/workspaces')}
      />
      <Routes>
        <Route path="/w/:workspaceId" element={
          <WorkspaceDetailRoute
            workspaces={workspaces}
            workspacesLoaded={workspacesLoaded}
            onRename={handleRenameWorkspace}
            initialTab="sandbox"
          />
        } />
        <Route
          path="/w/:workspaceId/sandboxes/:sandboxId"
          element={
            <WorkspaceDetailRoute
              workspaces={workspaces}
              workspacesLoaded={workspacesLoaded}
              onRename={handleRenameWorkspace}
              initialTab="sandbox"
              sandboxOverride={
                <SandboxDetailRoute
                  sandboxes={sandboxes}
                  onPause={handlePause}
                  onResume={handleResume}
                  onDelete={handleDelete}
                  onRename={handleRenameSandbox}
                />
              }
            />
          }
        />
        <Route
          path="/workspaces"
          element={
            <ManageWorkspaces
              workspaces={workspaces}
              selectedWorkspaceId={selectedWorkspaceId}
              onSelectWorkspace={handleSelectWorkspace}
              onRenameWorkspace={handleRenameWorkspace}
            />
          }
        />
        <Route
          path="/admin/*"
          element={<AdminPanel />}
        />
        <Route path="/oauth2/consent" element={<OAuthConsentRoute />} />
        <Route path="/oauth2/device" element={<OAuthDeviceRoute />} />
        <Route path="*" element={selectedWorkspaceId ? <Navigate to={`/w/${selectedWorkspaceId}`} replace /> : null} />
      </Routes>
    </div>
  )
}
