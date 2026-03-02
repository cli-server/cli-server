import { useState, useEffect } from 'react'
import { ArrowLeft, Loader2 } from 'lucide-react'
import {
  type AdminUser,
  type AdminWorkspace,
  type AdminSandbox,
  type QuotaDefaults,
  type UserQuotaResponse,
  type WorkspaceQuotaResponse,
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
} from '../lib/api'

type Tab = 'users' | 'workspaces' | 'sandboxes' | 'settings'

interface AdminPanelProps {
  onBack: () => void
}

export function AdminPanel({ onBack }: AdminPanelProps) {
  const [tab, setTab] = useState<Tab>('users')
  const [users, setUsers] = useState<AdminUser[]>([])
  const [workspaces, setWorkspaces] = useState<AdminWorkspace[]>([])
  const [sandboxes, setSandboxes] = useState<AdminSandbox[]>([])
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    setLoading(true)
    const fetch = async () => {
      try {
        switch (tab) {
          case 'users':
            setUsers(await adminListUsers())
            break
          case 'workspaces':
            setWorkspaces(await adminListWorkspaces())
            break
          case 'sandboxes':
            setSandboxes(await adminListSandboxes())
            break
        }
      } catch {
        // ignore
      } finally {
        setLoading(false)
      }
    }
    fetch()
  }, [tab])

  const handleRoleChange = async (userId: string, newRole: string) => {
    try {
      await adminUpdateUserRole(userId, newRole)
      setUsers((prev) =>
        prev.map((u) => (u.id === userId ? { ...u, role: newRole } : u))
      )
    } catch {
      // ignore
    }
  }

  const tabs: { key: Tab; label: string }[] = [
    { key: 'users', label: 'Users' },
    { key: 'workspaces', label: 'Workspaces' },
    { key: 'sandboxes', label: 'Sandboxes' },
    { key: 'settings', label: 'Settings' },
  ]

  return (
    <div className="flex h-full w-full flex-col bg-[var(--background)]">
      {/* Header */}
      <div className="flex items-center gap-4 border-b border-[var(--border)] px-6 py-4">
        <button
          onClick={onBack}
          className="rounded-md p-1 hover:bg-[var(--secondary)]"
          title="Back to dashboard"
        >
          <ArrowLeft size={20} />
        </button>
        <h1 className="text-lg font-semibold text-[var(--foreground)]">Admin Panel</h1>
      </div>

      {/* Tabs */}
      <div className="flex border-b border-[var(--border)] px-6">
        {tabs.map((t) => (
          <button
            key={t.key}
            onClick={() => setTab(t.key)}
            className={`px-4 py-3 text-sm font-medium transition-colors ${
              tab === t.key
                ? 'border-b-2 border-[var(--foreground)] text-[var(--foreground)]'
                : 'text-[var(--muted-foreground)] hover:text-[var(--foreground)]'
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      {/* Content */}
      <div className="flex-1 overflow-auto p-6">
        {loading ? (
          <div className="flex items-center justify-center py-12">
            <Loader2 size={24} className="animate-spin text-[var(--muted-foreground)]" />
          </div>
        ) : (
          <>
            {tab === 'users' && <UsersTable users={users} onRoleChange={handleRoleChange} />}
            {tab === 'workspaces' && <WorkspacesTable workspaces={workspaces} />}
            {tab === 'sandboxes' && <SandboxesTable sandboxes={sandboxes} />}
            {tab === 'settings' && <SettingsTab />}
          </>
        )}
      </div>
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
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Username</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Name</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Email</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Role</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Quota</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Created At</th>
            </tr>
          </thead>
          <tbody>
            {users.map((u) => (
              <tr key={u.id} className="border-b border-[var(--border)] last:border-b-0">
                <td className="px-4 py-3 text-[var(--foreground)]">{u.username}</td>
                <td className="px-4 py-3 text-[var(--muted-foreground)]">{u.name || '—'}</td>
                <td className="px-4 py-3 text-[var(--muted-foreground)]">{u.email || '—'}</td>
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
                  {new Date(u.createdAt).toLocaleString()}
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
  const [quotaWorkspace, setQuotaWorkspace] = useState<AdminWorkspace | null>(null)

  if (workspaces.length === 0) {
    return <p className="text-sm text-[var(--muted-foreground)]">No workspaces found.</p>
  }
  return (
    <>
      <div className="overflow-x-auto rounded-lg border border-[var(--border)]">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-[var(--border)] bg-[var(--muted)]">
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Name</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">ID</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Quota</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Created At</th>
              <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Updated At</th>
            </tr>
          </thead>
          <tbody>
            {workspaces.map((ws) => (
              <tr key={ws.id} className="border-b border-[var(--border)] last:border-b-0">
                <td className="px-4 py-3 text-[var(--foreground)]">{ws.name}</td>
                <td className="px-4 py-3 font-mono text-xs text-[var(--muted-foreground)]">{ws.id}</td>
                <td className="px-4 py-3">
                  <button
                    onClick={() => setQuotaWorkspace(ws)}
                    className="rounded-md border border-[var(--border)] px-2 py-1 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]"
                  >
                    Edit
                  </button>
                </td>
                <td className="px-4 py-3 text-[var(--muted-foreground)]">
                  {new Date(ws.createdAt).toLocaleString()}
                </td>
                <td className="px-4 py-3 text-[var(--muted-foreground)]">
                  {new Date(ws.updatedAt).toLocaleString()}
                </td>
              </tr>
            ))}
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
                {sbx.isLocal && (
                  <span className="ml-2 rounded bg-emerald-500/15 px-1.5 py-0.5 text-[10px] font-medium text-emerald-400">
                    local
                  </span>
                )}
              </td>
              <td className="px-4 py-3 font-mono text-xs text-[var(--muted-foreground)]">{sbx.workspaceId}</td>
              <td className="px-4 py-3 text-[var(--muted-foreground)]">{sbx.type}</td>
              <td className="px-4 py-3">
                <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${statusColor(sbx.status)}`}>
                  {sbx.status}
                </span>
              </td>
              <td className="px-4 py-3 text-[var(--muted-foreground)]">
                {new Date(sbx.createdAt).toLocaleString()}
              </td>
              <td className="px-4 py-3 text-[var(--muted-foreground)]">
                {sbx.lastActivityAt ? new Date(sbx.lastActivityAt).toLocaleString() : '—'}
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
      setMaxWs(String(d.maxWorkspacesPerUser))
      setMaxSbx(String(d.maxSandboxesPerWorkspace))
      setMaxSandboxCpu(String(d.maxSandboxCpu))
      setMaxSandboxMemory(String(d.maxSandboxMemory))
      setMaxIdleTimeout(String(d.maxIdleTimeout))
      setWsMaxTotalCpu(String(d.wsMaxTotalCpu))
      setWsMaxTotalMemory(String(d.wsMaxTotalMemory))
      setWsMaxIdleTimeout(String(d.wsMaxIdleTimeout))
      setMaxWorkspaceDriveSize(String(d.maxWorkspaceDriveSize))
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
        maxWorkspacesPerUser: ws,
        maxSandboxesPerWorkspace: sbx,
        ...(!isNaN(cpu) ? { maxSandboxCpu: cpu } : {}),
        ...(!isNaN(mem) ? { maxSandboxMemory: mem } : {}),
        ...(!isNaN(idle) ? { maxIdleTimeout: idle } : {}),
        ...(!isNaN(totalCpu) ? { wsMaxTotalCpu: totalCpu } : {}),
        ...(!isNaN(totalMem) ? { wsMaxTotalMemory: totalMem } : {}),
        ...(!isNaN(totalIdle) ? { wsMaxIdleTimeout: totalIdle } : {}),
        ...(!isNaN(driveSize) ? { maxWorkspaceDriveSize: driveSize } : {}),
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

  if (!defaults) {
    return (
      <div className="flex items-center justify-center py-12">
        <Loader2 size={24} className="animate-spin text-[var(--muted-foreground)]" />
      </div>
    )
  }

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
      setMaxWs(d.overrides?.maxWorkspaces != null ? String(d.overrides.maxWorkspaces) : '')
    }).catch(() => {}).finally(() => setLoading(false))
  }, [user.id])

  const handleSave = async () => {
    const ws = maxWs !== '' ? parseInt(maxWs, 10) : undefined
    if (ws !== undefined && (isNaN(ws) || ws < 0)) return
    setSaving(true)
    try {
      await adminSetUserQuota(user.id, {
        ...(ws !== undefined ? { maxWorkspaces: ws } : {}),
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
          Quota: {user.username}
        </h2>
        {loading ? (
          <div className="flex items-center justify-center py-8">
            <Loader2 size={24} className="animate-spin text-[var(--muted-foreground)]" />
          </div>
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
                placeholder={String(data.defaults.maxWorkspacesPerUser)}
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
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    adminGetWorkspaceQuota(workspace.id).then((d) => {
      setData(d)
      setMaxSbx(d.overrides?.maxSandboxes != null ? String(d.overrides.maxSandboxes) : '')
      setMaxSandboxCpu(d.overrides?.maxSandboxCpu != null ? String(d.overrides.maxSandboxCpu) : '')
      setMaxSandboxMemory(d.overrides?.maxSandboxMemory != null ? String(d.overrides.maxSandboxMemory) : '')
      setMaxIdleTimeout(d.overrides?.maxIdleTimeout != null ? String(d.overrides.maxIdleTimeout) : '')
      setMaxTotalCpu(d.overrides?.maxTotalCpu != null ? String(d.overrides.maxTotalCpu) : '')
      setMaxTotalMemory(d.overrides?.maxTotalMemory != null ? String(d.overrides.maxTotalMemory) : '')
      setMaxDriveSize(d.overrides?.maxDriveSize != null ? String(d.overrides.maxDriveSize) : '')
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
        ...(sbx !== undefined ? { maxSandboxes: sbx } : {}),
        ...(cpu !== undefined && !isNaN(cpu) ? { maxSandboxCpu: cpu } : {}),
        ...(mem !== undefined && !isNaN(mem) ? { maxSandboxMemory: mem } : {}),
        ...(idle !== undefined && !isNaN(idle) ? { maxIdleTimeout: idle } : {}),
        ...(totalCpu !== undefined && !isNaN(totalCpu) ? { maxTotalCpu: totalCpu } : {}),
        ...(totalMem !== undefined && !isNaN(totalMem) ? { maxTotalMemory: totalMem } : {}),
        ...(drive !== undefined && !isNaN(drive) ? { maxDriveSize: drive } : {}),
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
      await adminDeleteWorkspaceQuota(workspace.id)
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
          <div className="flex items-center justify-center py-8">
            <Loader2 size={24} className="animate-spin text-[var(--muted-foreground)]" />
          </div>
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
                placeholder={String(data.defaults.maxSandboxes)}
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
                placeholder={String(data.defaults.maxSandboxCpu)}
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
                placeholder={String(data.defaults.maxSandboxMemory)}
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
                placeholder={String(data.defaults.maxIdleTimeout)}
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
                placeholder={String(data.defaults.maxTotalCpu)}
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
                placeholder={String(data.defaults.maxTotalMemory)}
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
                placeholder={String(data.defaults.maxDriveSize)}
                className={inputClass}
              />
              <p className="text-xs text-[var(--muted-foreground)] mt-1">Bytes, e.g. 10737418240 = 10 GiB. Applied on creation.</p>
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
