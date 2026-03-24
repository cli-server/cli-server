import { useState, useEffect } from 'react'
import { Routes, Route, Navigate, useNavigate, useLocation, useParams } from 'react-router-dom'
import { ArrowLeft, Loader2, Users, Box, Container, Settings, ChevronRight } from 'lucide-react'
import {
  type AdminUser,
  type AdminWorkspace,
  type AdminSandbox,
  type QuotaDefaults,
  type UserQuotaResponse,
  type WorkspaceQuotaResponse,
  type LLMQuotaResponse,
  adminListUsers,
  adminListWorkspaces,
  adminListSandboxes,
  adminUpdateUserRole,
  adminGetQuotaDefaults,
  adminSetQuotaDefaults,
  adminGetUserQuota,
  adminSetUserQuota,
  adminDeleteUserQuota,
  adminGetWorkspaceQuota,
  adminSetWorkspaceQuota,
  adminDeleteWorkspaceQuota,
  adminGetWorkspaceLLMQuota,
  adminSetWorkspaceLLMQuota,
  adminDeleteWorkspaceLLMQuota,
} from '../lib/api'

const tabs = [
  { path: 'users', label: 'Users', icon: Users },
  { path: 'workspaces', label: 'Workspaces', icon: Box },
  { path: 'sandboxes', label: 'Sandboxes', icon: Container },
  { path: 'settings', label: 'Settings', icon: Settings },
] as const

export function AdminPanel() {
  const navigate = useNavigate()
  const location = useLocation()

  const activeTab = tabs.find((t) => location.pathname.startsWith(`/admin/${t.path}`))?.path ?? 'users'

  return (
    <div className="flex h-full w-full bg-[var(--background)]">
      {/* Sidebar */}
      <div className="flex w-52 flex-col border-r border-[var(--border)]">
        <button
          onClick={() => navigate('/')}
          className="flex items-center gap-2 px-4 py-4 text-sm text-[var(--muted-foreground)] hover:text-[var(--foreground)]"
        >
          <ArrowLeft size={16} />
          Back to workspace
        </button>
        <nav className="flex flex-col gap-0.5 px-2">
          {tabs.map((t) => (
            <button
              key={t.path}
              onClick={() => navigate(`/admin/${t.path}`)}
              className={`flex items-center gap-2 rounded-md px-3 py-2 text-sm font-medium transition-colors ${
                activeTab === t.path
                  ? 'bg-[var(--secondary)] text-[var(--foreground)]'
                  : 'text-[var(--muted-foreground)] hover:bg-[var(--secondary)] hover:text-[var(--foreground)]'
              }`}
            >
              <t.icon size={16} />
              {t.label}
            </button>
          ))}
        </nav>
      </div>

      {/* Content */}
      <div className="flex-1 overflow-auto p-6">
        <Routes>
          <Route index element={<Navigate to="users" replace />} />
          <Route path="users" element={<UsersTab />} />
          <Route path="workspaces" element={<WorkspacesTab />} />
          <Route path="workspaces/:workspaceId/sandboxes" element={<WorkspaceSandboxesTab />} />
          <Route path="sandboxes" element={<SandboxesTab />} />
          <Route path="settings" element={<SettingsTab />} />
          <Route path="*" element={<Navigate to="users" replace />} />
        </Routes>
      </div>
    </div>
  )
}

function UsersTab() {
  const [users, setUsers] = useState<AdminUser[]>([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    adminListUsers().then(setUsers).catch(() => {}).finally(() => setLoading(false))
  }, [])

  const handleRoleChange = async (userId: string, newRole: string) => {
    try {
      await adminUpdateUserRole(userId, newRole)
      setUsers((prev) => prev.map((u) => (u.id === userId ? { ...u, role: newRole } : u)))
    } catch {
      // ignore
    }
  }

  if (loading) return <LoadingSpinner />
  return <UsersTable users={users} onRoleChange={handleRoleChange} />
}

function WorkspacesTab() {
  const [workspaces, setWorkspaces] = useState<AdminWorkspace[]>([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    adminListWorkspaces().then(setWorkspaces).catch(() => {}).finally(() => setLoading(false))
  }, [])

  if (loading) return <LoadingSpinner />
  return <WorkspacesTable workspaces={workspaces} />
}

function WorkspaceSandboxesTab() {
  const { workspaceId } = useParams<{ workspaceId: string }>()
  const navigate = useNavigate()
  const [sandboxes, setSandboxes] = useState<AdminSandbox[]>([])
  const [loading, setLoading] = useState(true)
  const [workspaceName, setWorkspaceName] = useState('')

  useEffect(() => {
    if (!workspaceId) return
    Promise.all([
      adminListSandboxes(),
      adminListWorkspaces(),
    ]).then(([allSandboxes, allWorkspaces]) => {
      setSandboxes(allSandboxes.filter((s) => s.workspace_id === workspaceId))
      const ws = allWorkspaces.find((w) => w.id === workspaceId)
      if (ws) setWorkspaceName(ws.name)
    }).catch(() => {}).finally(() => setLoading(false))
  }, [workspaceId])

  if (loading) return <LoadingSpinner />

  return (
    <div>
      <div className="mb-4 flex items-center gap-2 text-sm text-[var(--muted-foreground)]">
        <button onClick={() => navigate('/admin/workspaces')} className="hover:text-[var(--foreground)]">
          Workspaces
        </button>
        <ChevronRight size={14} />
        <span className="text-[var(--foreground)]">{workspaceName || workspaceId}</span>
      </div>
      <SandboxesTable sandboxes={sandboxes} />
    </div>
  )
}

function SandboxesTab() {
  const [sandboxes, setSandboxes] = useState<AdminSandbox[]>([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    adminListSandboxes().then(setSandboxes).catch(() => {}).finally(() => setLoading(false))
  }, [])

  if (loading) return <LoadingSpinner />
  return <SandboxesTable sandboxes={sandboxes} />
}

function LoadingSpinner() {
  return (
    <div className="flex items-center justify-center py-12">
      <Loader2 size={24} className="animate-spin text-[var(--muted-foreground)]" />
    </div>
  )
}

function OwnerAvatar({ owner }: { owner: AdminWorkspace['owner'] }) {
  if (!owner) return <span className="text-[var(--muted-foreground)]">—</span>

  const displayName = owner.name || owner.email
  const initials = (displayName || '?').charAt(0).toUpperCase()

  return (
    <div className="flex items-center gap-2">
      {owner.picture ? (
        <img src={owner.picture} alt="" className="h-6 w-6 rounded-full object-cover" />
      ) : (
        <div className="flex h-6 w-6 items-center justify-center rounded-full bg-[var(--secondary)] text-xs font-medium text-[var(--foreground)]">
          {initials}
        </div>
      )}
      <span className="text-[var(--foreground)]">{displayName}</span>
    </div>
  )
}

function UsersTable({
  users,
  onRoleChange,
}: {
  users: AdminUser[]
  onRoleChange: (userId: string, role: string) => void
}) {
  const [quotaUser, setQuotaUser] = useState<AdminUser | null>(null)

  if (users.length === 0) {
    return <p className="text-sm text-[var(--muted-foreground)]">No users found.</p>
  }
  return (
    <>
      <div className="overflow-x-auto rounded-lg border border-[var(--border)]">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-[var(--border)] bg-[var(--muted)]">
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Email</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Name</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Role</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Quota</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Created At</th>
            </tr>
          </thead>
          <tbody>
            {users.map((u) => (
              <tr key={u.id} className="border-b border-[var(--border)] last:border-b-0">
                <td className="px-4 py-3 text-[var(--foreground)]">{u.email}</td>
                <td className="px-4 py-3 text-[var(--muted-foreground)]">{u.name || '—'}</td>
                <td className="px-4 py-3">
                  <select
                    value={u.role}
                    onChange={(e) => onRoleChange(u.id, e.target.value)}
                    className="rounded-md border border-[var(--border)] bg-[var(--background)] px-2 py-1 text-sm text-[var(--foreground)]"
                  >
                    <option value="user">user</option>
                    <option value="admin">admin</option>
                  </select>
                </td>
                <td className="px-4 py-3">
                  <button
                    onClick={() => setQuotaUser(u)}
                    className="rounded-md border border-[var(--border)] px-2 py-1 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]"
                  >
                    Edit
                  </button>
                </td>
                <td className="px-4 py-3 text-[var(--muted-foreground)]">
                  {new Date(u.created_at).toLocaleString()}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {quotaUser && (
        <UserQuotaModal user={quotaUser} onClose={() => setQuotaUser(null)} />
      )}
    </>
  )
}

function WorkspacesTable({ workspaces }: { workspaces: AdminWorkspace[] }) {
  const navigate = useNavigate()
  const [quotaWorkspace, setQuotaWorkspace] = useState<AdminWorkspace | null>(null)
  const [quotaMap, setQuotaMap] = useState<Map<string, LLMQuotaResponse>>(new Map())

  useEffect(() => {
    if (workspaces.length === 0) return
    const fetchQuotas = async () => {
      const entries = await Promise.all(
        workspaces.map(async (ws) => {
          try {
            const q = await adminGetWorkspaceLLMQuota(ws.id)
            return [ws.id, q] as const
          } catch {
            return null
          }
        })
      )
      setQuotaMap(new Map(entries.filter((e): e is NonNullable<typeof e> => e !== null)))
    }
    fetchQuotas()
  }, [workspaces])

  if (workspaces.length === 0) {
    return <p className="text-sm text-[var(--muted-foreground)]">No workspaces found.</p>
  }
  return (
    <>
      <div className="overflow-x-auto rounded-lg border border-[var(--border)]">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-[var(--border)] bg-[var(--muted)]">
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Owner</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Name</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Sandboxes</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">RPD</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Quota</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Created At</th>
            </tr>
          </thead>
          <tbody>
            {workspaces.map((ws) => {
              const q = quotaMap.get(ws.id)
              const effectiveMax = q?.workspace_quota?.max_rpd ?? q?.default_max_rpd ?? null
              const used = q?.today_request_count ?? 0
              const limitStr = effectiveMax === null ? '—' : effectiveMax === 0 ? '\u221E' : String(effectiveMax)
              const maxSbxStr = ws.max_sandboxes === 0 ? '\u221E' : String(ws.max_sandboxes)
              return (
                <tr key={ws.id} className="border-b border-[var(--border)] last:border-b-0">
                  <td className="px-4 py-3">
                    <OwnerAvatar owner={ws.owner} />
                  </td>
                  <td className="px-4 py-3 text-[var(--foreground)]">{ws.name}</td>
                  <td className="px-4 py-3">
                    <button
                      onClick={() => navigate(`/admin/workspaces/${ws.id}/sandboxes`)}
                      className="text-[var(--muted-foreground)] hover:text-[var(--foreground)]"
                    >
                      {ws.sandbox_count} / {maxSbxStr}
                      <ChevronRight size={14} className="ml-1 inline-block" />
                    </button>
                  </td>
                  <td className="px-4 py-3 text-[var(--muted-foreground)]">{q ? `${used} / ${limitStr}` : '—'}</td>
                  <td className="px-4 py-3">
                    <button
                      onClick={() => setQuotaWorkspace(ws)}
                      className="rounded-md border border-[var(--border)] px-2 py-1 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]"
                    >
                      Edit
                    </button>
                  </td>
                  <td className="px-4 py-3 text-[var(--muted-foreground)]">
                    {new Date(ws.created_at).toLocaleString()}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
      {quotaWorkspace && (
        <WorkspaceQuotaModal workspace={quotaWorkspace} onClose={() => setQuotaWorkspace(null)} />
      )}
    </>
  )
}

function SandboxesTable({ sandboxes }: { sandboxes: AdminSandbox[] }) {
  if (sandboxes.length === 0) {
    return <p className="text-sm text-[var(--muted-foreground)]">No sandboxes found.</p>
  }

  const statusColor = (status: string) => {
    switch (status) {
      case 'running':
        return 'bg-green-500/10 text-green-500'
      case 'paused':
        return 'bg-yellow-500/10 text-yellow-500'
      case 'offline':
        return 'bg-red-500/10 text-red-500'
      default:
        return 'bg-gray-500/10 text-[var(--muted-foreground)]'
    }
  }

  return (
    <div className="overflow-x-auto rounded-lg border border-[var(--border)]">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b border-[var(--border)] bg-[var(--muted)]">
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Name</th>
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Workspace ID</th>
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Type</th>
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Status</th>
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Created At</th>
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Last Activity</th>
          </tr>
        </thead>
        <tbody>
          {sandboxes.map((sbx) => (
            <tr key={sbx.id} className="border-b border-[var(--border)] last:border-b-0">
              <td className="px-4 py-3 text-[var(--foreground)]">
                {sbx.name}
                {sbx.is_local && (
                  <span className="ml-2 rounded bg-emerald-500/15 px-1.5 py-0.5 text-[10px] font-medium text-emerald-400">
                    local
                  </span>
                )}
              </td>
              <td className="px-4 py-3 font-mono text-xs text-[var(--muted-foreground)]">{sbx.workspace_id}</td>
              <td className="px-4 py-3 text-[var(--muted-foreground)]">{sbx.type}</td>
              <td className="px-4 py-3">
                <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${statusColor(sbx.status)}`}>
                  {sbx.status}
                </span>
              </td>
              <td className="px-4 py-3 text-[var(--muted-foreground)]">
                {new Date(sbx.created_at).toLocaleString()}
              </td>
              <td className="px-4 py-3 text-[var(--muted-foreground)]">
                {sbx.last_activity_at ? new Date(sbx.last_activity_at).toLocaleString() : '—'}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

const inputClass = "w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)]"

function SettingsTab() {
  const [defaults, setDefaults] = useState<QuotaDefaults | null>(null)
  const [maxWs, setMaxWs] = useState('')
  const [maxSbx, setMaxSbx] = useState('')
  const [maxSandboxCpu, setMaxSandboxCpu] = useState('')
  const [maxSandboxMemory, setMaxSandboxMemory] = useState('')
  const [maxIdleTimeout, setMaxIdleTimeout] = useState('')
  const [wsMaxTotalCpu, setWsMaxTotalCpu] = useState('')
  const [wsMaxTotalMemory, setWsMaxTotalMemory] = useState('')
  const [wsMaxIdleTimeout, setWsMaxIdleTimeout] = useState('')
  const [maxWorkspaceDriveSize, setMaxWorkspaceDriveSize] = useState('')
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    adminGetQuotaDefaults().then((d) => {
      setDefaults(d)
      setMaxWs(String(d.max_workspaces_per_user))
      setMaxSbx(String(d.max_sandboxes_per_workspace))
      setMaxSandboxCpu(String(d.max_sandbox_cpu))
      setMaxSandboxMemory(String(d.max_sandbox_memory))
      setMaxIdleTimeout(String(d.max_idle_timeout))
      setWsMaxTotalCpu(String(d.ws_max_total_cpu))
      setWsMaxTotalMemory(String(d.ws_max_total_memory))
      setWsMaxIdleTimeout(String(d.ws_max_idle_timeout))
      setMaxWorkspaceDriveSize(String(d.max_workspace_drive_size))
    }).catch(() => {})
  }, [])

  const handleSave = async () => {
    const ws = parseInt(maxWs, 10)
    const sbx = parseInt(maxSbx, 10)
    if (isNaN(ws) || ws < 0 || isNaN(sbx) || sbx < 0) return
    setSaving(true)
    try {
      const cpu = parseInt(maxSandboxCpu, 10)
      const mem = parseInt(maxSandboxMemory, 10)
      const idle = parseInt(maxIdleTimeout, 10)
      const totalCpu = parseInt(wsMaxTotalCpu, 10)
      const totalMem = parseInt(wsMaxTotalMemory, 10)
      const totalIdle = parseInt(wsMaxIdleTimeout, 10)
      const driveSize = parseInt(maxWorkspaceDriveSize, 10)
      const updated = await adminSetQuotaDefaults({
        max_workspaces_per_user: ws,
        max_sandboxes_per_workspace: sbx,
        ...(!isNaN(cpu) ? { max_sandbox_cpu: cpu } : {}),
        ...(!isNaN(mem) ? { max_sandbox_memory: mem } : {}),
        ...(!isNaN(idle) ? { max_idle_timeout: idle } : {}),
        ...(!isNaN(totalCpu) ? { ws_max_total_cpu: totalCpu } : {}),
        ...(!isNaN(totalMem) ? { ws_max_total_memory: totalMem } : {}),
        ...(!isNaN(totalIdle) ? { ws_max_idle_timeout: totalIdle } : {}),
        ...(!isNaN(driveSize) ? { max_workspace_drive_size: driveSize } : {}),
      })
      setDefaults(updated)
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    } catch {
      // ignore
    } finally {
      setSaving(false)
    }
  }

  if (!defaults) return <LoadingSpinner />

  return (
    <div className="max-w-md">
      <p className="text-sm text-[var(--muted-foreground)] mb-6">
        Set system-wide default limits for all users. Use 0 for unlimited where applicable.
      </p>

      {/* Quotas */}
      <h2 className="text-base font-semibold text-[var(--foreground)] mb-3">Quotas</h2>
      <div className="flex flex-col gap-4 mb-6">
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
            Max workspaces per user
          </label>
          <input
            type="number"
            min="0"
            value={maxWs}
            onChange={(e) => setMaxWs(e.target.value)}
            className={inputClass}
          />
        </div>
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
            Max sandboxes per workspace
          </label>
          <input
            type="number"
            min="0"
            value={maxSbx}
            onChange={(e) => setMaxSbx(e.target.value)}
            className={inputClass}
          />
        </div>
      </div>

      <hr className="border-[var(--border)] mb-6" />

      {/* Sandbox Defaults */}
      <h2 className="text-base font-semibold text-[var(--foreground)] mb-3">Sandbox Defaults</h2>
      <div className="flex flex-col gap-4 mb-6">
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
            CPU limit (millicores)
          </label>
          <input
            type="number"
            min="0"
            value={maxSandboxCpu}
            onChange={(e) => setMaxSandboxCpu(e.target.value)}
            placeholder="2000"
            className={inputClass}
          />
          <p className="text-xs text-[var(--muted-foreground)] mt-1">Millicores, e.g. 2000 = 2 cores</p>
        </div>
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
            Memory limit (bytes)
          </label>
          <input
            type="number"
            min="0"
            value={maxSandboxMemory}
            onChange={(e) => setMaxSandboxMemory(e.target.value)}
            placeholder="2147483648"
            className={inputClass}
          />
          <p className="text-xs text-[var(--muted-foreground)] mt-1">Bytes, e.g. 2147483648 = 2 GiB</p>
        </div>
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
            Idle timeout (seconds)
          </label>
          <input
            type="number"
            min="0"
            value={maxIdleTimeout}
            onChange={(e) => setMaxIdleTimeout(e.target.value)}
            placeholder="1800"
            className={inputClass}
          />
          <p className="text-xs text-[var(--muted-foreground)] mt-1">Seconds, e.g. 1800 = 30 min. Use 0 to disable.</p>
        </div>
      </div>

      <hr className="border-[var(--border)] mb-6" />

      {/* Workspace Limits */}
      <h2 className="text-base font-semibold text-[var(--foreground)] mb-3">Workspace Limits</h2>
      <div className="flex flex-col gap-4 mb-6">
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
            Max total CPU budget (millicores)
          </label>
          <input
            type="number"
            min="0"
            value={wsMaxTotalCpu}
            onChange={(e) => setWsMaxTotalCpu(e.target.value)}
            placeholder="0"
            className={inputClass}
          />
          <p className="text-xs text-[var(--muted-foreground)] mt-1">Total millicores across all sandboxes. 0 = unlimited.</p>
        </div>
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
            Max total memory budget (bytes)
          </label>
          <input
            type="number"
            min="0"
            value={wsMaxTotalMemory}
            onChange={(e) => setWsMaxTotalMemory(e.target.value)}
            placeholder="0"
            className={inputClass}
          />
          <p className="text-xs text-[var(--muted-foreground)] mt-1">Total bytes across all sandboxes. 0 = unlimited.</p>
        </div>
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
            Max idle timeout (seconds)
          </label>
          <input
            type="number"
            min="0"
            value={wsMaxIdleTimeout}
            onChange={(e) => setWsMaxIdleTimeout(e.target.value)}
            placeholder="0"
            className={inputClass}
          />
          <p className="text-xs text-[var(--muted-foreground)] mt-1">Max idle timeout per workspace in seconds. 0 = unlimited.</p>
        </div>
      </div>

      <hr className="border-[var(--border)] mb-6" />

      {/* Storage */}
      <h2 className="text-base font-semibold text-[var(--foreground)] mb-3">Storage</h2>
      <div className="flex flex-col gap-4 mb-6">
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
            Workspace drive size (bytes)
          </label>
          <input
            type="number"
            min="0"
            value={maxWorkspaceDriveSize}
            onChange={(e) => setMaxWorkspaceDriveSize(e.target.value)}
            placeholder="10737418240"
            className={inputClass}
          />
          <p className="text-xs text-[var(--muted-foreground)] mt-1">Bytes, e.g. 10737418240 = 10 GiB. Applied on server restart.</p>
        </div>
      </div>

      <button
        onClick={handleSave}
        disabled={saving}
        className="self-start rounded-md bg-[var(--foreground)] px-4 py-2 text-sm font-medium text-[var(--background)] hover:opacity-90 disabled:opacity-50"
      >
        {saving ? 'Saving...' : saved ? 'Saved' : 'Save'}
      </button>
    </div>
  )
}

function UserQuotaModal({ user, onClose }: { user: AdminUser; onClose: () => void }) {
  const [loading, setLoading] = useState(true)
  const [data, setData] = useState<UserQuotaResponse | null>(null)
  const [maxWs, setMaxWs] = useState('')
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    adminGetUserQuota(user.id).then((d) => {
      setData(d)
      setMaxWs(d.overrides?.max_workspaces != null ? String(d.overrides.max_workspaces) : '')
    }).catch(() => {}).finally(() => setLoading(false))
  }, [user.id])

  const handleSave = async () => {
    const ws = maxWs !== '' ? parseInt(maxWs, 10) : undefined
    if (ws !== undefined && (isNaN(ws) || ws < 0)) return
    setSaving(true)
    try {
      await adminSetUserQuota(user.id, {
        ...(ws !== undefined ? { max_workspaces: ws } : {}),
      })
      onClose()
    } catch {
      // ignore
    } finally {
      setSaving(false)
    }
  }

  const handleRevert = async () => {
    setSaving(true)
    try {
      await adminDeleteUserQuota(user.id)
      onClose()
    } catch {
      // ignore
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50" onClick={onClose}>
      <div
        className="w-full max-w-sm rounded-lg border border-[var(--border)] bg-[var(--card)] p-6 shadow-xl max-h-[80vh] overflow-y-auto"
        onClick={(e) => e.stopPropagation()}
      >
        <h2 className="text-lg font-semibold text-[var(--foreground)] mb-4">
          Quota: {user.email}
        </h2>
        {loading ? (
          <LoadingSpinner />
        ) : data ? (
          <div className="flex flex-col gap-4">
            <p className="text-xs text-[var(--muted-foreground)]">
              Leave blank to use system defaults. Use 0 for unlimited.
            </p>
            <div>
              <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                Max workspaces
              </label>
              <input
                type="number"
                min="0"
                value={maxWs}
                onChange={(e) => setMaxWs(e.target.value)}
                placeholder={String(data.defaults.max_workspaces_per_user)}
                className={inputClass}
              />
            </div>
            <div className="flex justify-between mt-2">
              <button
                onClick={handleRevert}
                disabled={saving || !data.overrides}
                className="rounded-md border border-[var(--border)] px-3 py-2 text-sm font-medium text-[var(--foreground)] hover:bg-[var(--secondary)] disabled:opacity-50"
              >
                Revert to defaults
              </button>
              <div className="flex gap-2">
                <button
                  onClick={onClose}
                  className="rounded-md border border-[var(--border)] px-3 py-2 text-sm font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]"
                >
                  Cancel
                </button>
                <button
                  onClick={handleSave}
                  disabled={saving}
                  className="rounded-md bg-[var(--foreground)] px-4 py-2 text-sm font-medium text-[var(--background)] hover:opacity-90 disabled:opacity-50"
                >
                  {saving ? 'Saving...' : 'Save'}
                </button>
              </div>
            </div>
          </div>
        ) : (
          <p className="text-sm text-[var(--muted-foreground)]">Failed to load quota data.</p>
        )}
      </div>
    </div>
  )
}

function WorkspaceQuotaModal({ workspace, onClose }: { workspace: AdminWorkspace; onClose: () => void }) {
  const [loading, setLoading] = useState(true)
  const [data, setData] = useState<WorkspaceQuotaResponse | null>(null)
  const [maxSbx, setMaxSbx] = useState('')
  const [maxSandboxCpu, setMaxSandboxCpu] = useState('')
  const [maxSandboxMemory, setMaxSandboxMemory] = useState('')
  const [maxIdleTimeout, setMaxIdleTimeout] = useState('')
  const [maxTotalCpu, setMaxTotalCpu] = useState('')
  const [maxTotalMemory, setMaxTotalMemory] = useState('')
  const [maxDriveSize, setMaxDriveSize] = useState('')
  const [maxRpd, setMaxRpd] = useState('')
  const [defaultMaxRpd, setDefaultMaxRpd] = useState(0)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    Promise.all([
      adminGetWorkspaceQuota(workspace.id),
      adminGetWorkspaceLLMQuota(workspace.id).catch(() => null),
    ]).then(([d, rpd]) => {
      setData(d)
      if (rpd) {
        setDefaultMaxRpd(rpd.default_max_rpd)
        setMaxRpd(rpd.workspace_quota?.max_rpd != null ? String(rpd.workspace_quota.max_rpd) : '')
      }
      setMaxSbx(d.overrides?.max_sandboxes != null ? String(d.overrides.max_sandboxes) : '')
      setMaxSandboxCpu(d.overrides?.max_sandbox_cpu != null ? String(d.overrides.max_sandbox_cpu) : '')
      setMaxSandboxMemory(d.overrides?.max_sandbox_memory != null ? String(d.overrides.max_sandbox_memory) : '')
      setMaxIdleTimeout(d.overrides?.max_idle_timeout != null ? String(d.overrides.max_idle_timeout) : '')
      setMaxTotalCpu(d.overrides?.max_total_cpu != null ? String(d.overrides.max_total_cpu) : '')
      setMaxTotalMemory(d.overrides?.max_total_memory != null ? String(d.overrides.max_total_memory) : '')
      setMaxDriveSize(d.overrides?.max_drive_size != null ? String(d.overrides.max_drive_size) : '')
    }).catch(() => {}).finally(() => setLoading(false))
  }, [workspace.id])

  const handleSave = async () => {
    const sbx = maxSbx !== '' ? parseInt(maxSbx, 10) : undefined
    if (sbx !== undefined && (isNaN(sbx) || sbx < 0)) return
    setSaving(true)
    try {
      const cpu = maxSandboxCpu !== '' ? parseInt(maxSandboxCpu, 10) : undefined
      const mem = maxSandboxMemory !== '' ? parseInt(maxSandboxMemory, 10) : undefined
      const idle = maxIdleTimeout !== '' ? parseInt(maxIdleTimeout, 10) : undefined
      const totalCpu = maxTotalCpu !== '' ? parseInt(maxTotalCpu, 10) : undefined
      const totalMem = maxTotalMemory !== '' ? parseInt(maxTotalMemory, 10) : undefined
      const drive = maxDriveSize !== '' ? parseInt(maxDriveSize, 10) : undefined
      await adminSetWorkspaceQuota(workspace.id, {
        ...(sbx !== undefined ? { max_sandboxes: sbx } : {}),
        ...(cpu !== undefined && !isNaN(cpu) ? { max_sandbox_cpu: cpu } : {}),
        ...(mem !== undefined && !isNaN(mem) ? { max_sandbox_memory: mem } : {}),
        ...(idle !== undefined && !isNaN(idle) ? { max_idle_timeout: idle } : {}),
        ...(totalCpu !== undefined && !isNaN(totalCpu) ? { max_total_cpu: totalCpu } : {}),
        ...(totalMem !== undefined && !isNaN(totalMem) ? { max_total_memory: totalMem } : {}),
        ...(drive !== undefined && !isNaN(drive) ? { max_drive_size: drive } : {}),
      })
      const rpd = maxRpd !== '' ? parseInt(maxRpd, 10) : undefined
      if (rpd !== undefined && !isNaN(rpd) && rpd >= 0) {
        await adminSetWorkspaceLLMQuota(workspace.id, rpd)
      }
      onClose()
    } catch {
      // ignore
    } finally {
      setSaving(false)
    }
  }

  const handleRevert = async () => {
    setSaving(true)
    try {
      await adminDeleteWorkspaceQuota(workspace.id)
      await adminDeleteWorkspaceLLMQuota(workspace.id).catch(() => {})
      onClose()
    } catch {
      // ignore
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50" onClick={onClose}>
      <div
        className="w-full max-w-sm rounded-lg border border-[var(--border)] bg-[var(--card)] p-6 shadow-xl max-h-[80vh] overflow-y-auto"
        onClick={(e) => e.stopPropagation()}
      >
        <h2 className="text-lg font-semibold text-[var(--foreground)] mb-4">
          Workspace Quota: {workspace.name}
        </h2>
        {loading ? (
          <LoadingSpinner />
        ) : data ? (
          <div className="flex flex-col gap-4">
            <p className="text-xs text-[var(--muted-foreground)]">
              Leave blank to use system defaults. Use 0 for unlimited where applicable.
            </p>
            <div>
              <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                Max sandboxes
              </label>
              <input
                type="number"
                min="0"
                value={maxSbx}
                onChange={(e) => setMaxSbx(e.target.value)}
                placeholder={String(data.defaults.max_sandboxes)}
                className={inputClass}
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                Sandbox CPU (millicores)
              </label>
              <input
                type="number"
                min="0"
                value={maxSandboxCpu}
                onChange={(e) => setMaxSandboxCpu(e.target.value)}
                placeholder={String(data.defaults.max_sandbox_cpu)}
                className={inputClass}
              />
              <p className="text-xs text-[var(--muted-foreground)] mt-1">Millicores, e.g. 2000 = 2 cores</p>
            </div>
            <div>
              <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                Sandbox memory (bytes)
              </label>
              <input
                type="number"
                min="0"
                value={maxSandboxMemory}
                onChange={(e) => setMaxSandboxMemory(e.target.value)}
                placeholder={String(data.defaults.max_sandbox_memory)}
                className={inputClass}
              />
              <p className="text-xs text-[var(--muted-foreground)] mt-1">Bytes, e.g. 2147483648 = 2 GiB</p>
            </div>
            <div>
              <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                Idle timeout (seconds)
              </label>
              <input
                type="number"
                min="0"
                value={maxIdleTimeout}
                onChange={(e) => setMaxIdleTimeout(e.target.value)}
                placeholder={String(data.defaults.max_idle_timeout)}
                className={inputClass}
              />
              <p className="text-xs text-[var(--muted-foreground)] mt-1">Seconds, e.g. 1800 = 30 min. Use 0 to disable.</p>
            </div>
            <div>
              <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                Max total CPU budget (millicores)
              </label>
              <input
                type="number"
                min="0"
                value={maxTotalCpu}
                onChange={(e) => setMaxTotalCpu(e.target.value)}
                placeholder={String(data.defaults.max_total_cpu)}
                className={inputClass}
              />
              <p className="text-xs text-[var(--muted-foreground)] mt-1">Total millicores across all sandboxes. 0 = unlimited.</p>
            </div>
            <div>
              <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                Max total memory budget (bytes)
              </label>
              <input
                type="number"
                min="0"
                value={maxTotalMemory}
                onChange={(e) => setMaxTotalMemory(e.target.value)}
                placeholder={String(data.defaults.max_total_memory)}
                className={inputClass}
              />
              <p className="text-xs text-[var(--muted-foreground)] mt-1">Total bytes across all sandboxes. 0 = unlimited.</p>
            </div>
            <div>
              <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                Workspace drive size (bytes)
              </label>
              <input
                type="number"
                min="0"
                value={maxDriveSize}
                onChange={(e) => setMaxDriveSize(e.target.value)}
                placeholder={String(data.defaults.max_drive_size)}
                className={inputClass}
              />
              <p className="text-xs text-[var(--muted-foreground)] mt-1">Bytes, e.g. 10737418240 = 10 GiB. Applied on creation.</p>
            </div>
            <div>
              <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                Max requests per day (RPD)
              </label>
              <input
                type="number"
                min="0"
                value={maxRpd}
                onChange={(e) => setMaxRpd(e.target.value)}
                placeholder={String(defaultMaxRpd)}
                className={inputClass}
              />
              <p className="text-xs text-[var(--muted-foreground)] mt-1">LLM API requests per day. 0 = unlimited.</p>
            </div>
            <div className="flex justify-between mt-2">
              <button
                onClick={handleRevert}
                disabled={saving || (!data.overrides && maxRpd === "")}
                className="rounded-md border border-[var(--border)] px-3 py-2 text-sm font-medium text-[var(--foreground)] hover:bg-[var(--secondary)] disabled:opacity-50"
              >
                Revert to defaults
              </button>
              <div className="flex gap-2">
                <button
                  onClick={onClose}
                  className="rounded-md border border-[var(--border)] px-3 py-2 text-sm font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]"
                >
                  Cancel
                </button>
                <button
                  onClick={handleSave}
                  disabled={saving}
                  className="rounded-md bg-[var(--foreground)] px-4 py-2 text-sm font-medium text-[var(--background)] hover:opacity-90 disabled:opacity-50"
                >
                  {saving ? 'Saving...' : 'Save'}
                </button>
              </div>
            </div>
          </div>
        ) : (
          <p className="text-sm text-[var(--muted-foreground)]">Failed to load quota data.</p>
        )}
      </div>
    </div>
  )
}
