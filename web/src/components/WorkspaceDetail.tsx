import { useState, useEffect, useCallback } from 'react'
import {
  Clock,
  Users,
  LayoutDashboard,
  Box,
  UserPlus,
  Trash2,
  X,
  MessageSquare,
  Pencil,
} from 'lucide-react'
import {
  listMembers,
  addMember,
  removeMember,
  getWorkspaceDefaults,
  getWorkspaceLLMQuota,
  getWorkspaceTraces,
  getWorkspaceTraceDetail,
  renameWorkspace,
  type Workspace,
  type WorkspaceMember,
  type WorkspaceSandboxDefaults,
  type WorkspaceLLMQuota,
  type TraceItem,
} from '../lib/api'
import { ConfirmModal } from './Modals'
import { TracesTab, TRACES_PER_PAGE } from './SandboxDetail'

type Tab = 'overview' | 'members' | 'traces'

interface WorkspaceDetailProps {
  workspace: Workspace
  onRename?: (id: string, name: string) => void
}

export function WorkspaceDetail({ workspace, onRename }: WorkspaceDetailProps) {
  const [tab, setTab] = useState<Tab>('overview')
  const [members, setMembers] = useState<WorkspaceMember[]>([])
  const [sbxQuota, setSbxQuota] = useState<{ current: number; max: number } | null>(null)
  const [defaults, setDefaults] = useState<WorkspaceSandboxDefaults | null>(null)
  const [llmQuota, setLlmQuota] = useState<WorkspaceLLMQuota | null>(null)
  const [traces, setTraces] = useState<TraceItem[]>([])
  const [tracesTotal, setTracesTotal] = useState(0)
  const [tracesPage, setTracesPage] = useState(0)
  const [editing, setEditing] = useState(false)
  const [editName, setEditName] = useState(workspace.name)

  useEffect(() => {
    setTab('overview')
    setMembers([])
    setSbxQuota(null)
    setDefaults(null)
    setLlmQuota(null)
    setTraces([])
    setTracesTotal(0)
    setTracesPage(0)

    listMembers(workspace.id).then(setMembers).catch(() => {})
    getWorkspaceDefaults(workspace.id).then((d) => {
      setDefaults(d)
      setSbxQuota({ current: d.current_sandboxes, max: d.max_sandboxes })
    }).catch(() => {})
    getWorkspaceLLMQuota(workspace.id).then(setLlmQuota).catch(() => {})
    getWorkspaceTraces(workspace.id, TRACES_PER_PAGE, 0).then((r) => {
      setTraces(r.traces || [])
      setTracesTotal(r.total || 0)
    }).catch(() => {})
  }, [workspace.id])

  useEffect(() => {
    if (tracesPage === 0) return
    getWorkspaceTraces(workspace.id, TRACES_PER_PAGE, tracesPage * TRACES_PER_PAGE).then((r) => {
      setTraces(r.traces || [])
      setTracesTotal(r.total || 0)
    }).catch(() => {})
  }, [workspace.id, tracesPage])

  const totalPages = Math.ceil(tracesTotal / TRACES_PER_PAGE)
  const fetchDetail = useCallback((traceId: string) => getWorkspaceTraceDetail(workspace.id, traceId), [workspace.id])

  const tabs: { key: Tab; label: string; icon: React.ReactNode }[] = [
    { key: 'overview', label: 'Overview', icon: <LayoutDashboard size={15} /> },
    { key: 'members', label: 'Members', icon: <Users size={15} /> },
    { key: 'traces', label: 'Traces', icon: <MessageSquare size={15} /> },
  ]

  return (
    <div className="flex h-full w-full flex-col">
      {/* Header */}
      <div className="shrink-0 border-b border-[var(--border)] bg-[var(--card)] px-6 py-4">
        <div className="flex items-start justify-between gap-4">
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2">
              {editing ? (
                <input
                  autoFocus
                  className="text-lg font-semibold text-[var(--foreground)] bg-transparent border-b border-[var(--border)] outline-none"
                  value={editName}
                  onChange={(e) => setEditName(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      const trimmed = editName.trim()
                      if (trimmed && trimmed !== workspace.name) {
                        renameWorkspace(workspace.id, trimmed).then(() => onRename?.(workspace.id, trimmed)).catch(() => {})
                      }
                      setEditing(false)
                    } else if (e.key === 'Escape') {
                      setEditName(workspace.name)
                      setEditing(false)
                    }
                  }}
                  onBlur={() => {
                    const trimmed = editName.trim()
                    if (trimmed && trimmed !== workspace.name) {
                      renameWorkspace(workspace.id, trimmed).then(() => onRename?.(workspace.id, trimmed)).catch(() => {})
                    }
                    setEditing(false)
                  }}
                />
              ) : (
                <>
                  <h1 className="text-lg font-semibold text-[var(--foreground)] truncate">{workspace.name}</h1>
                  <button onClick={() => { setEditName(workspace.name); setEditing(true) }} className="text-[var(--muted-foreground)] hover:text-[var(--foreground)]">
                    <Pencil size={14} />
                  </button>
                </>
              )}
            </div>
            <div className="mt-1 text-xs text-[var(--muted-foreground)]">Workspace</div>
          </div>
        </div>
        <div className="mt-4 flex gap-1">
          {tabs.map((t) => (
            <button
              key={t.key}
              onClick={() => setTab(t.key)}
              className={`inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-xs font-medium transition-colors ${
                tab === t.key
                  ? 'bg-[var(--secondary)] text-[var(--foreground)]'
                  : 'text-[var(--muted-foreground)] hover:text-[var(--foreground)] hover:bg-[var(--secondary)]/50'
              }`}
            >
              {t.icon}
              {t.label}
              {t.key === 'members' && members.length > 0 && (
                <span className="ml-0.5 rounded-full bg-[var(--muted)] px-1.5 py-0 text-[10px] text-[var(--muted-foreground)]">
                  {members.length}
                </span>
              )}
              {t.key === 'traces' && tracesTotal > 0 && (
                <span className="ml-0.5 rounded-full bg-[var(--muted)] px-1.5 py-0 text-[10px] text-[var(--muted-foreground)]">
                  {tracesTotal}
                </span>
              )}
            </button>
          ))}
        </div>
      </div>

      {/* Content */}
      <div className="flex-1 overflow-y-auto p-6">
        {tab === 'overview' && (
          <OverviewTab
            workspace={workspace}
            sbxQuota={sbxQuota}
            defaults={defaults}
            llmQuota={llmQuota}
          />
        )}
        {tab === 'members' && (
          <MembersTab
            workspaceId={workspace.id}
            members={members}
            setMembers={setMembers}
          />
        )}
        {tab === 'traces' && (
          <TracesTab
            traces={traces}
            tracesTotal={tracesTotal}
            tracesPage={tracesPage}
            totalPages={totalPages}
            onPageChange={setTracesPage}
            fetchDetail={fetchDetail}
            showSandboxId
          />
        )}
      </div>
    </div>
  )
}

function OverviewTab({ workspace, sbxQuota, defaults, llmQuota }: {
  workspace: Workspace
  sbxQuota: { current: number; max: number } | null
  defaults: WorkspaceSandboxDefaults | null
  llmQuota: WorkspaceLLMQuota | null
}) {
  const effectiveMaxRpd = llmQuota?.workspace_quota?.max_rpd ?? llmQuota?.default_max_rpd ?? null
  return (
    <div className="flex flex-col gap-6 max-w-3xl">
      {/* Info cards */}
      <div className="grid grid-cols-2 gap-4 sm:grid-cols-3">
        <InfoCard icon={<Clock size={14} />} label="Created" value={new Date(workspace.created_at).toLocaleString()} />
        {sbxQuota && (
          <InfoCard
            icon={<Box size={14} />}
            label="Sandboxes"
            value={`${sbxQuota.current} / ${sbxQuota.max === 0 ? '\u221E' : sbxQuota.max}`}
          />
        )}
        {effectiveMaxRpd !== null && (
          <InfoCard
            icon={<Box size={14} />}
            label="RPD"
            value={`${llmQuota?.today_request_count ?? 0} / ${effectiveMaxRpd === 0 ? '\u221E' : String(effectiveMaxRpd)}`}
          />
        )}
      </div>

      {/* Resource limits */}
      {defaults && (
        <div className="rounded-lg border border-[var(--border)] bg-[var(--card)]">
          <div className="flex items-center gap-2 border-b border-[var(--border)] px-5 py-3">
            <span className="text-sm font-medium text-[var(--foreground)]">Resource Limits</span>
          </div>
          <div className="grid grid-cols-2 gap-px bg-[var(--border)] sm:grid-cols-4">
            <StatCell label="Max CPU" value={`${(defaults.max_sandbox_cpu / 1000).toFixed(1)} cores`} />
            <StatCell label="Max Memory" value={`${Math.round(defaults.max_sandbox_memory / (1024 * 1024))} MB`} />
            <StatCell label="Max Idle" value={defaults.max_idle_timeout > 0 ? `${Math.round(defaults.max_idle_timeout / 60)} min` : 'Unlimited'} />
            <StatCell label="Max Sandboxes" value={defaults.max_sandboxes === 0 ? '\u221E' : String(defaults.max_sandboxes)} />
          </div>
        </div>
      )}
    </div>
  )
}

function MembersTab({ workspaceId, members, setMembers }: {
  workspaceId: string
  members: WorkspaceMember[]
  setMembers: React.Dispatch<React.SetStateAction<WorkspaceMember[]>>
}) {
  const [showAdd, setShowAdd] = useState(false)
  const [addUsername, setAddUsername] = useState('')
  const [addRole, setAddRole] = useState('developer')
  const [addError, setAddError] = useState<string | null>(null)
  const [confirmRemove, setConfirmRemove] = useState<WorkspaceMember | null>(null)

  const handleAdd = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!addUsername.trim()) return
    setAddError(null)
    try {
      const m = await addMember(workspaceId, addUsername.trim(), addRole)
      setMembers((prev) => [...prev, m])
      setAddUsername('')
      setShowAdd(false)
    } catch {
      setAddError('Failed to add member. User may not exist.')
    }
  }

  const doRemove = async (userId: string) => {
    setConfirmRemove(null)
    try {
      await removeMember(workspaceId, userId)
      setMembers((prev) => prev.filter((m) => m.user_id !== userId))
    } catch { /* ignore */ }
  }

  const roleColors: Record<string, string> = {
    owner: 'bg-amber-500/10 text-amber-400 border-amber-500/20',
    maintainer: 'bg-blue-500/10 text-blue-400 border-blue-500/20',
    developer: 'bg-green-500/10 text-green-400 border-green-500/20',
    guest: 'bg-gray-500/10 text-[var(--muted-foreground)] border-gray-500/20',
  }

  return (
    <div className="max-w-2xl">
      <div className="flex items-center justify-between mb-4">
        <span className="text-sm font-medium text-[var(--foreground)]">
          {members.length} member{members.length !== 1 ? 's' : ''}
        </span>
        <button
          onClick={() => setShowAdd((v) => !v)}
          className="inline-flex items-center gap-1.5 rounded-md border border-[var(--border)] bg-[var(--card)] px-3 py-1.5 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)] transition-colors"
        >
          {showAdd ? <X size={13} /> : <UserPlus size={13} />}
          {showAdd ? 'Cancel' : 'Add member'}
        </button>
      </div>

      {showAdd && (
        <form onSubmit={handleAdd} className="mb-4 rounded-lg border border-[var(--border)] bg-[var(--card)] p-4">
          <div className="flex gap-3">
            <input
              autoFocus
              type="text"
              value={addUsername}
              onChange={(e) => setAddUsername(e.target.value)}
              placeholder="Username"
              className="flex-1 rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-1.5 text-sm text-[var(--foreground)] outline-none focus:border-[var(--primary)]"
            />
            <select
              value={addRole}
              onChange={(e) => setAddRole(e.target.value)}
              className="rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-1.5 text-sm text-[var(--foreground)] outline-none focus:border-[var(--primary)]"
            >
              <option value="developer">Developer</option>
              <option value="maintainer">Maintainer</option>
              <option value="guest">Guest</option>
            </select>
            <button
              type="submit"
              disabled={!addUsername.trim()}
              className="rounded-md bg-[var(--primary)] px-4 py-1.5 text-sm font-medium text-[var(--primary-foreground)] hover:opacity-90 disabled:opacity-50"
            >
              Add
            </button>
          </div>
          {addError && (
            <p className="mt-2 text-xs text-red-400">{addError}</p>
          )}
        </form>
      )}

      <div className="rounded-lg border border-[var(--border)] bg-[var(--card)] overflow-hidden">
        {members.length === 0 ? (
          <div className="flex flex-col items-center justify-center py-12 text-[var(--muted-foreground)]">
            <Users size={32} className="mb-3 opacity-30" />
            <span className="text-sm">No members</span>
          </div>
        ) : (
          <div className="divide-y divide-[var(--border)]">
            {members.map((m) => (
              <div key={m.user_id} className="group flex items-center justify-between px-4 py-3 hover:bg-[var(--secondary)]/30 transition-colors">
                <div className="flex items-center gap-3 min-w-0">
                  {m.picture ? (
                    <img src={m.picture} alt={m.username} className="h-8 w-8 shrink-0 rounded-full object-cover" />
                  ) : (
                    <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-[var(--secondary)] text-xs font-medium text-[var(--foreground)]">
                      {(m.username || '?')[0].toUpperCase()}
                    </div>
                  )}
                  <span className="text-sm text-[var(--foreground)] truncate">{m.username}</span>
                </div>
                <div className="flex items-center gap-2">
                  <span className={`rounded-full border px-2.5 py-0.5 text-[10px] font-medium ${roleColors[m.role] || roleColors.guest}`}>
                    {m.role}
                  </span>
                  {m.role !== 'owner' && (
                    <button
                      onClick={() => setConfirmRemove(m)}
                      className="hidden rounded p-1 text-[var(--muted-foreground)] hover:bg-red-500/10 hover:text-red-400 group-hover:block transition-colors"
                      title="Remove member"
                    >
                      <Trash2 size={13} />
                    </button>
                  )}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      {confirmRemove && (
        <ConfirmModal
          title="Remove Member"
          message={`Remove "${confirmRemove.username}" from this workspace?`}
          confirmLabel="Remove"
          destructive
          onConfirm={() => doRemove(confirmRemove.user_id)}
          onCancel={() => setConfirmRemove(null)}
        />
      )}
    </div>
  )
}

function InfoCard({ icon, label, value }: { icon: React.ReactNode; label: string; value: string }) {
  return (
    <div className="rounded-lg border border-[var(--border)] bg-[var(--card)] px-4 py-3">
      <div className="flex items-center gap-1.5 text-[var(--muted-foreground)] mb-1">
        {icon}
        <span className="text-xs">{label}</span>
      </div>
      <div className="text-sm font-medium text-[var(--foreground)] truncate">{value}</div>
    </div>
  )
}

function StatCell({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-[var(--card)] px-4 py-3">
      <div className="text-xs text-[var(--muted-foreground)]">{label}</div>
      <div className="text-sm font-semibold text-[var(--foreground)] mt-0.5">{value}</div>
    </div>
  )
}
