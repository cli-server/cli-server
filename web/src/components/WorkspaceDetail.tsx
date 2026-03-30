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
  Settings,
  Plus,
  Minus,
  Key,
  Bot,
  Hash,
} from 'lucide-react'
import {
  listMembers,
  addMember,
  removeMember,
  getWorkspaceDefaults,
  getWorkspaceLLMQuota,
  getWorkspaceTraces,
  getWorkspaceTraceDetail,
  getWorkspaceLLMConfig,
  setWorkspaceLLMConfig,
  deleteWorkspaceLLMConfig,
  getModelserverStatus,
  disconnectModelserver,
  renameWorkspace,
  listWorkspaceIMChannels,
  deleteWorkspaceIMChannel,
  updateWorkspaceIMChannel,
  type Workspace,
  type WorkspaceMember,
  type WorkspaceSandboxDefaults,
  type WorkspaceLLMQuota,
  type WorkspaceLLMConfig,
  type LLMModel,
  type TraceItem,
  type ModelserverStatus,
  type IMChannel,
} from '../lib/api'
import { ConfirmModal } from './Modals'
import { TracesTab, TRACES_PER_PAGE } from './SandboxDetail'
import { WeixinLoginModal } from './WeixinLoginModal'
import { TelegramConfigModal } from './TelegramConfigModal'
import { MatrixConfigModal } from './MatrixConfigModal'

type Tab = 'overview' | 'members' | 'traces' | 'settings'

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
    { key: 'settings', label: 'Settings', icon: <Settings size={15} /> },
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
        {tab === 'settings' && (
          <SettingsTab workspaceId={workspace.id} />
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
  const [addEmail, setAddEmail] = useState('')
  const [addRole, setAddRole] = useState('developer')
  const [addError, setAddError] = useState<string | null>(null)
  const [confirmRemove, setConfirmRemove] = useState<WorkspaceMember | null>(null)

  const handleAdd = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!addEmail.trim()) return
    setAddError(null)
    try {
      const m = await addMember(workspaceId, addEmail.trim(), addRole)
      setMembers((prev) => [...prev, m])
      setAddEmail('')
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
              value={addEmail}
              onChange={(e) => setAddEmail(e.target.value)}
              placeholder="Email"
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
              disabled={!addEmail.trim()}
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
                    <img src={m.picture} alt={m.email} className="h-8 w-8 shrink-0 rounded-full object-cover" />
                  ) : (
                    <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-[var(--secondary)] text-xs font-medium text-[var(--foreground)]">
                      {(m.email || '?')[0].toUpperCase()}
                    </div>
                  )}
                  <span className="text-sm text-[var(--foreground)] truncate">{m.email}</span>
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
          message={`Remove "${confirmRemove.email}" from this workspace?`}
          confirmLabel="Remove"
          destructive
          onConfirm={() => doRemove(confirmRemove.user_id)}
          onCancel={() => setConfirmRemove(null)}
        />
      )}
    </div>
  )
}

function SettingsTab({ workspaceId }: { workspaceId: string }) {
  const [config, setConfig] = useState<WorkspaceLLMConfig | null>(null)
  const [editMode, setEditMode] = useState(false)
  const [baseUrl, setBaseUrl] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [models, setModels] = useState<LLMModel[]>([{ id: '', name: '' }])
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [msStatus, setMsStatus] = useState<ModelserverStatus | null>(null)

  // IM Channels state
  const [imChannels, setImChannels] = useState<IMChannel[]>([])
  const [showWeixinLogin, setShowWeixinLogin] = useState(false)
  const [showTelegramConfig, setShowTelegramConfig] = useState(false)
  const [showMatrixConfig, setShowMatrixConfig] = useState(false)
  const [confirmDeleteChannel, setConfirmDeleteChannel] = useState<IMChannel | null>(null)

  const loadChannels = useCallback(() => {
    listWorkspaceIMChannels(workspaceId).then(r => setImChannels(r.channels || [])).catch(() => {})
  }, [workspaceId])

  const load = useCallback(() => {
    getWorkspaceLLMConfig(workspaceId).then(setConfig).catch(() => {})
  }, [workspaceId])

  useEffect(() => { load() }, [load])
  useEffect(() => { loadChannels() }, [loadChannels])

  useEffect(() => {
    getModelserverStatus(workspaceId).then(setMsStatus).catch(() => setMsStatus({ connected: false }))
  }, [workspaceId])

  useEffect(() => {
    const params = new URLSearchParams(window.location.search)
    const msParam = params.get('modelserver')
    if (msParam === 'connected' || msParam === 'error') {
      if (msParam === 'connected') {
        getModelserverStatus(workspaceId).then(setMsStatus)
      }
      // Clean URL params
      const url = new URL(window.location.href)
      url.searchParams.delete('modelserver')
      url.searchParams.delete('message')
      window.history.replaceState({}, '', url.pathname + (url.searchParams.toString() ? '?' + url.searchParams.toString() : ''))
    }
  }, [])

  const startEdit = () => {
    if (config?.configured) {
      setBaseUrl(config.base_url || '')
      setApiKey('')
      setModels(config.models && config.models.length > 0 ? [...config.models] : [{ id: '', name: '' }])
    } else {
      setBaseUrl('')
      setApiKey('')
      setModels([{ id: '', name: '' }])
    }
    setError(null)
    setEditMode(true)
  }

  const handleSave = async () => {
    const validModels = models.filter(m => m.id.trim() && m.name.trim())
    if (!baseUrl.trim()) {
      setError('Base URL is required')
      return
    }
    if (!apiKey.trim() && !config?.configured) {
      setError('API Key is required')
      return
    }
    if (validModels.length === 0) {
      setError('At least one model with ID and name is required')
      return
    }
    setSaving(true)
    setError(null)
    try {
      await setWorkspaceLLMConfig(workspaceId, {
        base_url: baseUrl.trim(),
        api_key: apiKey.trim(), // empty string = keep existing key on server
        models: validModels.map(m => ({ id: m.id.trim(), name: m.name.trim() })),
      })
      setEditMode(false)
      load()
    } catch {
      setError('Failed to save configuration')
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async () => {
    setConfirmDelete(false)
    try {
      await deleteWorkspaceLLMConfig(workspaceId)
      setEditMode(false)
      load()
    } catch { /* ignore */ }
  }

  const addModel = () => setModels([...models, { id: '', name: '' }])
  const removeModel = (i: number) => setModels(models.filter((_, idx) => idx !== i))
  const updateModel = (i: number, field: 'id' | 'name', value: string) => {
    const next = [...models]
    next[i] = { ...next[i], [field]: value }
    setModels(next)
  }

  const inputCls = 'w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-1.5 text-sm text-[var(--foreground)] outline-none focus:border-[var(--primary)]'

  if (!config) return null

  if (editMode) {
    return (
      <div className="max-w-2xl">
        <div className="rounded-lg border border-[var(--border)] bg-[var(--card)] p-5">
          <h3 className="text-sm font-medium text-[var(--foreground)] mb-4">LLM Provider Configuration</h3>

          <div className="flex flex-col gap-4">
            <div>
              <label className="text-xs text-[var(--muted-foreground)] mb-1 block">Base URL</label>
              <input type="text" value={baseUrl} onChange={e => setBaseUrl(e.target.value)} placeholder="https://api.anthropic.com" className={inputCls} />
            </div>
            <div>
              <label className="text-xs text-[var(--muted-foreground)] mb-1 block">API Key</label>
              <input type="password" value={apiKey} onChange={e => setApiKey(e.target.value)} placeholder={config.configured ? 'Leave empty to keep current key' : 'sk-...'} className={inputCls} />
            </div>
            <div>
              <div className="flex items-center justify-between mb-2">
                <label className="text-xs text-[var(--muted-foreground)]">Models</label>
                <button onClick={addModel} className="inline-flex items-center gap-1 text-xs text-[var(--muted-foreground)] hover:text-[var(--foreground)]">
                  <Plus size={12} /> Add
                </button>
              </div>
              <div className="flex flex-col gap-2">
                {models.map((m, i) => (
                  <div key={i} className="flex gap-2 items-center">
                    <input type="text" value={m.id} onChange={e => updateModel(i, 'id', e.target.value)} placeholder="Model ID" className={inputCls} />
                    <input type="text" value={m.name} onChange={e => updateModel(i, 'name', e.target.value)} placeholder="Display Name" className={inputCls} />
                    {models.length > 1 && (
                      <button onClick={() => removeModel(i)} className="shrink-0 rounded p-1 text-[var(--muted-foreground)] hover:text-red-400">
                        <Minus size={14} />
                      </button>
                    )}
                  </div>
                ))}
              </div>
            </div>

            {error && <p className="text-xs text-red-400">{error}</p>}

            <div className="flex gap-2 justify-end pt-2">
              <button onClick={() => setEditMode(false)} className="rounded-md border border-[var(--border)] bg-[var(--card)] px-4 py-1.5 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]">
                Cancel
              </button>
              <button onClick={handleSave} disabled={saving} className="rounded-md bg-[var(--primary)] px-4 py-1.5 text-xs font-medium text-[var(--primary-foreground)] hover:opacity-90 disabled:opacity-50">
                {saving ? 'Saving...' : 'Save'}
              </button>
            </div>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="max-w-2xl">
      <h3 className="text-base font-semibold text-[var(--foreground)] mb-4">LLM Provider</h3>

      {/* ModelServer */}
      <div className="rounded-lg border border-[var(--border)] bg-[var(--card)] mb-4">
        <div className="flex items-center justify-between border-b border-[var(--border)] px-5 py-3">
          <span className="text-sm font-medium text-[var(--foreground)]">ModelServer</span>
          {msStatus?.connected && (
            <div className="flex gap-2">
              <button
                onClick={() => { window.location.href = `/api/workspaces/${workspaceId}/modelserver/connect` }}
                className="rounded-md border border-[var(--border)] bg-[var(--card)] px-3 py-1 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]"
              >
                Reconnect
              </button>
              <button
                onClick={async () => {
                  await disconnectModelserver(workspaceId)
                  setMsStatus({ connected: false })
                }}
                className="rounded-md border border-[var(--border)] bg-[var(--card)] px-3 py-1 text-xs font-medium text-red-400 hover:bg-red-500/10"
              >
                Disconnect
              </button>
            </div>
          )}
        </div>
        <div className="px-5 py-4">
          {msStatus?.connected ? (
            <div>
              <p className="mb-2 text-sm text-[var(--foreground)]">
                Connected to project: <strong>{msStatus.project_name}</strong>
              </p>
              {msStatus.models && msStatus.models.length > 0 && (
                <div className="flex flex-wrap gap-1">
                  {msStatus.models.map(m => (
                    <span key={m.id} className="px-2 py-0.5 text-xs rounded bg-[var(--muted)] text-[var(--muted-foreground)]">{m.id}</span>
                  ))}
                </div>
              )}
            </div>
          ) : (
            <div className="flex items-center justify-between">
              <span className="text-sm text-[var(--muted-foreground)]">Not connected</span>
              <button
                onClick={() => { window.location.href = `/api/workspaces/${workspaceId}/modelserver/connect` }}
                className="rounded-md border border-[var(--border)] bg-[var(--card)] px-3 py-1 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]"
              >
                Connect
              </button>
            </div>
          )}
        </div>
      </div>

      {msStatus?.connected && (
        <p className="text-xs text-[var(--muted-foreground)] mb-4">
          Custom provider configuration below is overridden by the ModelServer connection.
        </p>
      )}

      {/* Custom Provider */}
      <div className="rounded-lg border border-[var(--border)] bg-[var(--card)]">
        <div className="flex items-center justify-between border-b border-[var(--border)] px-5 py-3">
          <div className="flex items-center gap-2">
            <Key size={14} className="text-[var(--muted-foreground)]" />
            <span className="text-sm font-medium text-[var(--foreground)]">Custom Provider</span>
          </div>
          <div className="flex gap-2">
            <button onClick={startEdit} className="rounded-md border border-[var(--border)] bg-[var(--card)] px-3 py-1 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]">
              {config.configured ? 'Edit' : 'Configure'}
            </button>
            {config.configured && (
              <button onClick={() => setConfirmDelete(true)} className="rounded-md border border-red-500/20 bg-red-500/10 px-3 py-1 text-xs font-medium text-red-400 hover:bg-red-500/20">
                Remove
              </button>
            )}
          </div>
        </div>
        <div className="px-5 py-4">
          {config.configured ? (
            <div className="flex flex-col gap-2 text-sm">
              <div className="flex justify-between">
                <span className="text-[var(--muted-foreground)]">Base URL</span>
                <span className="text-[var(--foreground)] font-mono text-xs">{config.base_url}</span>
              </div>
              <div className="flex justify-between">
                <span className="text-[var(--muted-foreground)]">API Key</span>
                <span className="text-[var(--foreground)] font-mono text-xs">{config.api_key}</span>
              </div>
              <div className="flex justify-between">
                <span className="text-[var(--muted-foreground)]">Models</span>
                <span className="text-[var(--foreground)]">{config.models?.length || 0} configured</span>
              </div>
              {config.models && config.models.length > 0 && (
                <div className="mt-1 flex flex-wrap gap-1.5">
                  {config.models.map(m => (
                    <span key={m.id} className="rounded-full border border-[var(--border)] bg-[var(--secondary)] px-2.5 py-0.5 text-[10px] font-medium text-[var(--muted-foreground)]">
                      {m.name}
                    </span>
                  ))}
                </div>
              )}
            </div>
          ) : (
            <div className="text-sm text-[var(--muted-foreground)]">
              Using platform default (LLM Proxy). Configure your own API key to connect sandboxes directly to your LLM provider.
            </div>
          )}
        </div>
      </div>

      {confirmDelete && (
        <ConfirmModal
          title="Remove LLM Configuration"
          message="Remove custom LLM configuration? New sandboxes will use the platform default."
          confirmLabel="Remove"
          destructive
          onConfirm={handleDelete}
          onCancel={() => setConfirmDelete(false)}
        />
      )}

      {/* IM Channels */}
      <div className="mt-6 rounded-lg border border-[var(--border)] bg-[var(--card)]">
        <div className="flex items-center justify-between border-b border-[var(--border)] px-5 py-3">
          <div className="flex items-center gap-2">
            <MessageSquare size={14} className="text-green-400" />
            <span className="text-sm font-medium text-[var(--foreground)]">IM Channels</span>
          </div>
        </div>
        <div className="px-5 py-4">
          {imChannels.length > 0 ? (
            <div className="flex flex-col gap-2 mb-4">
              {imChannels.map((ch) => (
                <div key={ch.id} className="flex items-center justify-between rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2">
                  <div className="flex items-center gap-3">
                    <span className={`inline-flex items-center rounded px-1.5 py-0.5 text-[10px] font-medium ${
                      ch.provider === 'telegram'
                        ? 'bg-blue-500/10 text-blue-400'
                        : ch.provider === 'matrix'
                          ? 'bg-purple-500/10 text-purple-400'
                          : 'bg-green-500/10 text-green-400'
                    }`}>
                      {ch.provider === 'telegram' ? 'Telegram' : ch.provider === 'matrix' ? 'Matrix' : 'WeChat'}
                    </span>
                    <span className="text-xs font-mono text-[var(--foreground)]">{ch.bot_id}</span>
                    <span className="text-[11px] text-[var(--muted-foreground)]">
                      {new Date(ch.bound_at).toLocaleString()}
                    </span>
                  </div>
                  <div className="flex items-center gap-2">
                    <label className="flex items-center gap-1.5 text-[11px] text-[var(--muted-foreground)] cursor-pointer" title="Only reply when @mentioned in group chats">
                      <input
                        type="checkbox"
                        checked={ch.require_mention}
                        onChange={async (e) => {
                          try {
                            await updateWorkspaceIMChannel(workspaceId, ch.id, { require_mention: e.target.checked })
                            loadChannels()
                          } catch {}
                        }}
                        className="rounded"
                      />
                      @mention
                    </label>
                    <button
                      onClick={() => setConfirmDeleteChannel(ch)}
                      className="rounded p-1 text-[var(--muted-foreground)] hover:bg-red-500/10 hover:text-red-400 transition-colors"
                      title="Delete channel"
                    >
                      <Trash2 size={13} />
                    </button>
                  </div>
                </div>
              ))}
            </div>
          ) : (
            <p className="text-sm text-[var(--muted-foreground)] mb-4">No IM channels configured.</p>
          )}
          <div className="flex gap-2">
            <button
              onClick={() => setShowWeixinLogin(true)}
              className="inline-flex items-center gap-1.5 rounded-md border border-green-500/30 bg-green-500/10 px-3 py-1.5 text-xs font-medium text-green-400 hover:bg-green-500/20 transition-colors"
            >
              <MessageSquare size={13} />
              WeChat
            </button>
            <button
              onClick={() => setShowTelegramConfig(true)}
              className="inline-flex items-center gap-1.5 rounded-md border border-blue-500/30 bg-blue-500/10 px-3 py-1.5 text-xs font-medium text-blue-400 hover:bg-blue-500/20 transition-colors"
            >
              <Bot size={13} />
              Telegram
            </button>
            <button
              onClick={() => setShowMatrixConfig(true)}
              className="inline-flex items-center gap-1.5 rounded-md border border-purple-500/30 bg-purple-500/10 px-3 py-1.5 text-xs font-medium text-purple-400 hover:bg-purple-500/20 transition-colors"
            >
              <Hash size={13} />
              Matrix
            </button>
          </div>
        </div>
      </div>

      {/* IM Channel modals */}
      {confirmDeleteChannel && (
        <ConfirmModal
          title="Delete IM Channel"
          message={`Delete the ${confirmDeleteChannel.provider} channel "${confirmDeleteChannel.bot_id}"? Any sandbox bound to this channel will be unbound.`}
          confirmLabel="Delete"
          destructive
          onConfirm={async () => {
            const ch = confirmDeleteChannel
            setConfirmDeleteChannel(null)
            try {
              await deleteWorkspaceIMChannel(workspaceId, ch.id)
              loadChannels()
            } catch { /* ignore */ }
          }}
          onCancel={() => setConfirmDeleteChannel(null)}
        />
      )}
      {showWeixinLogin && (
        <WeixinLoginModal
          workspaceId={workspaceId}
          onClose={() => setShowWeixinLogin(false)}
          onConnected={() => { loadChannels() }}
        />
      )}
      {showTelegramConfig && (
        <TelegramConfigModal
          workspaceId={workspaceId}
          onClose={() => setShowTelegramConfig(false)}
          onConnected={() => { loadChannels() }}
        />
      )}
      {showMatrixConfig && (
        <MatrixConfigModal
          workspaceId={workspaceId}
          onClose={() => setShowMatrixConfig(false)}
          onConnected={() => { loadChannels() }}
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
