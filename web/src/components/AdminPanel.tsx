import { useState, useEffect } from 'react'
import { ArrowLeft, Loader2 } from 'lucide-react'
import {
  type AdminUser,
  type AdminWorkspace,
  type AdminSandbox,
  adminListUsers,
  adminListWorkspaces,
  adminListSandboxes,
  adminUpdateUserRole,
} from '../lib/api'

type Tab = 'users' | 'workspaces' | 'sandboxes'

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
  if (users.length === 0) {
    return <p className="text-sm text-[var(--muted-foreground)]">No users found.</p>
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-[var(--border)]">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b border-[var(--border)] bg-[var(--muted)]">
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Username</th>
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Email</th>
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Role</th>
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Created At</th>
          </tr>
        </thead>
        <tbody>
          {users.map((u) => (
            <tr key={u.id} className="border-b border-[var(--border)] last:border-b-0">
              <td className="px-4 py-3 text-[var(--foreground)]">{u.username}</td>
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
              <td className="px-4 py-3 text-[var(--muted-foreground)]">
                {new Date(u.createdAt).toLocaleString()}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function WorkspacesTable({ workspaces }: { workspaces: AdminWorkspace[] }) {
  if (workspaces.length === 0) {
    return <p className="text-sm text-[var(--muted-foreground)]">No workspaces found.</p>
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-[var(--border)]">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b border-[var(--border)] bg-[var(--muted)]">
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Name</th>
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">ID</th>
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Created At</th>
            <th className="px-4 py-3 text-left font-medium text-[var(--muted-foreground)]">Updated At</th>
          </tr>
        </thead>
        <tbody>
          {workspaces.map((ws) => (
            <tr key={ws.id} className="border-b border-[var(--border)] last:border-b-0">
              <td className="px-4 py-3 text-[var(--foreground)]">{ws.name}</td>
              <td className="px-4 py-3 font-mono text-xs text-[var(--muted-foreground)]">{ws.id}</td>
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
