import { useState, useEffect, useRef } from 'react'
import { useNavigate, useLocation } from 'react-router-dom'
import { Plus, Trash2, Pause, Play, Loader2, Laptop, Box, ChevronDown, ChevronRight } from 'lucide-react'
import {
  type Sandbox,
  type AgentInfo,
  createSandbox,
  deleteSandbox,
  pauseSandbox,
  resumeSandbox,
  createAgentCode,
} from '../lib/api'
import { CreateSandboxModal } from './CreateSandboxModal'
import { ConfirmModal } from './Modals'

function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(k))
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i]
}

function AgentInfoPanel({ info }: { info: AgentInfo }) {
  const items = [
    { label: 'OS', value: `${info.platform} ${info.platform_version}` },
    { label: 'Arch', value: info.kernel_arch },
    { label: 'Host', value: info.hostname },
    { label: 'CPU', value: info.cpu_model_name ? `${info.cpu_model_name} (${info.cpu_count_logical} cores)` : `${info.cpu_count_logical} cores` },
    { label: 'Memory', value: formatBytes(info.memory_total) },
    { label: 'Disk', value: `${formatBytes(info.disk_free)} free / ${formatBytes(info.disk_total)}` },
    { label: 'Agent', value: info.agent_version || 'Unknown' },
    { label: 'opencode', value: info.opencode_version || 'Unknown' },
  ]
  return (
    <div className="border-t border-[var(--border)] bg-[var(--card)] px-3 py-2">
      <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-[11px]">
        {items.map(({ label, value }) => (
          <div key={label} className="contents">
            <span className="text-[var(--muted-foreground)]">{label}</span>
            <span className="truncate text-[var(--foreground)]" title={value}>{value}</span>
          </div>
        ))}
      </div>
      <div className="mt-1.5 text-[10px] text-[var(--muted-foreground)]">
        Updated {new Date(info.updated_at).toLocaleString()}
      </div>
    </div>
  )
}

interface SandboxListProps {
  selectedWorkspaceId: string | null
  sandboxes: Sandbox[]
  setSandboxes: React.Dispatch<React.SetStateAction<Sandbox[]>>
  onRefreshSandboxes: () => void
  creating: boolean
  setCreating: (v: boolean) => void
}

function StatusDot({ status }: { status: string }) {
  switch (status) {
    case 'running':
      return <span className="inline-block h-2 w-2 rounded-full bg-green-500" title="Running" />
    case 'paused':
      return <span className="inline-block h-2 w-2 rounded-full bg-yellow-500" title="Paused" />
    case 'offline':
      return <span className="inline-block h-2 w-2 rounded-full bg-red-500" title="Offline" />
    case 'pausing':
    case 'resuming':
    case 'creating':
      return <Loader2 size={10} className="animate-spin text-[var(--muted-foreground)]" />
    default:
      return <span className="inline-block h-2 w-2 rounded-full bg-gray-500" />
  }
}

export function SandboxList({
  selectedWorkspaceId,
  sandboxes,
  setSandboxes,
  onRefreshSandboxes,
  creating,
  setCreating,
}: SandboxListProps) {
  const navigate = useNavigate()
  const location = useLocation()
  const sandboxMatch = location.pathname.match(/^\/sandboxes\/(.+)$/)
  const activeSandboxId = sandboxMatch?.[1] ?? null
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const [showCreateModal, setShowCreateModal] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState<{ id: string; name: string } | null>(null)
  const [confirmPause, setConfirmPause] = useState<{ id: string; name: string } | null>(null)
  const [agentCodeData, setAgentCodeData] = useState<{ code: string; expires_at: string; command: string } | null>(null)
  const [quotaError, setQuotaError] = useState<string | null>(null)
  const [expandedSandboxId, setExpandedSandboxId] = useState<string | null>(null)

  // Poll when any sandbox is in a transitional state.
  useEffect(() => {
    const hasTransitional = sandboxes.some(
      (s) => s.status === 'pausing' || s.status === 'resuming' || s.status === 'creating'
    )
    if (hasTransitional) {
      if (!pollRef.current) {
        pollRef.current = setInterval(onRefreshSandboxes, 2000)
      }
    } else {
      if (pollRef.current) {
        clearInterval(pollRef.current)
        pollRef.current = null
      }
    }
    return () => {
      if (pollRef.current) {
        clearInterval(pollRef.current)
        pollRef.current = null
      }
    }
  }, [sandboxes, onRefreshSandboxes])

  const handleCreateSandbox = async (
    name: string,
    type: 'opencode' | 'openclaw',
    cpu?: number,
    memory?: number,
    idleTimeout?: number,
  ) => {
    if (creating || !selectedWorkspaceId) return
    setCreating(true)
    setShowCreateModal(false)
    setQuotaError(null)
    navigate('/')
    try {
      const sbx = await createSandbox(selectedWorkspaceId, name, type, cpu, memory, idleTimeout)
      setSandboxes((prev) => [...prev, sbx])
      navigate(`/sandboxes/${sbx.id}`)
    } catch (err: unknown) {
      const qe = err as { error?: string; message?: string } | undefined
      if ((qe?.error === 'quota_exceeded' || qe?.error === 'resource_budget_exceeded') && qe.message) {
        setQuotaError(qe.message)
      }
    } finally {
      setCreating(false)
    }
  }

  const handleDelete = (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    const sbx = sandboxes.find((s) => s.id === id)
    setConfirmDelete({ id, name: sbx?.name || 'this sandbox' })
  }

  const doDelete = async (id: string) => {
    setConfirmDelete(null)
    try {
      await deleteSandbox(id)
      setSandboxes((prev) => prev.filter((s) => s.id !== id))
      if (activeSandboxId === id) {
        navigate('/')
      }
    } catch {
      // ignore
    }
  }

  const handlePause = (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    const sbx = sandboxes.find((s) => s.id === id)
    setConfirmPause({ id, name: sbx?.name || 'this sandbox' })
  }

  const doPause = async (id: string) => {
    setConfirmPause(null)
    try {
      await pauseSandbox(id)
      setSandboxes((prev) =>
        prev.map((s) => (s.id === id ? { ...s, status: 'pausing' as const } : s))
      )
    } catch {
      // ignore
    }
  }

  const handleResume = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    try {
      await resumeSandbox(id)
      setSandboxes((prev) =>
        prev.map((s) => (s.id === id ? { ...s, status: 'resuming' as const } : s))
      )
    } catch {
      // ignore
    }
  }

  const toggleExpand = (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    setExpandedSandboxId((prev) => (prev === id ? null : id))
  }

  return (
    <div className="flex h-full w-60 flex-col border-r border-[var(--border)] bg-[var(--muted)]">
      {/* Sandbox header */}
      <div className="flex items-center justify-between border-b border-[var(--border)] p-3">
        <div className="flex items-center gap-1.5">
          <Box size={14} className="text-[var(--muted-foreground)]" />
          <span className="text-sm font-medium">Sandboxes</span>
        </div>
        <div className="flex gap-1">
          <button
            onClick={async () => {
              if (!selectedWorkspaceId) return
              try {
                const data = await createAgentCode(selectedWorkspaceId)
                const serverUrl = window.location.origin
                const command = `agentserver connect --server ${serverUrl} --code ${data.code} --name "My PC"`
                setAgentCodeData({ ...data, command })
              } catch {
                // ignore
              }
            }}
            disabled={!selectedWorkspaceId}
            className="rounded p-1 hover:bg-[var(--secondary)] disabled:opacity-50"
            title="Connect local agent"
          >
            <Laptop size={16} />
          </button>
          <button
            onClick={() => setShowCreateModal(true)}
            disabled={creating || !selectedWorkspaceId}
            className="rounded p-1 hover:bg-[var(--secondary)] disabled:opacity-50"
            title="New sandbox"
          >
            {creating ? <Loader2 size={16} className="animate-spin" /> : <Plus size={16} />}
          </button>
        </div>
      </div>

      {/* Sandbox list */}
      <div className="flex-1 overflow-y-auto">
        {quotaError && (
          <div className="mx-3 mt-2 flex items-start gap-2 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-400">
            <span className="flex-1">{quotaError}</span>
            <button
              onClick={() => setQuotaError(null)}
              className="shrink-0 font-medium hover:text-red-300"
            >
              Dismiss
            </button>
          </div>
        )}
        {sandboxes.map((sbx) => (
          <div key={sbx.id}>
            <div
              onClick={() => navigate(`/sandboxes/${sbx.id}`)}
              className={`group flex cursor-pointer items-center gap-2 px-3 py-2 text-sm hover:bg-[var(--secondary)] ${
                activeSandboxId === sbx.id ? 'bg-[var(--secondary)]' : ''
              }`}
            >
              <StatusDot status={sbx.status} />
              {sbx.is_local && sbx.agent_info && (
                <button
                  onClick={(e) => toggleExpand(sbx.id, e)}
                  className="-ml-1 shrink-0 text-[var(--muted-foreground)] hover:text-[var(--foreground)]"
                >
                  {expandedSandboxId === sbx.id ? <ChevronDown size={10} /> : <ChevronRight size={10} />}
                </button>
              )}
              <span className="flex-1 truncate">{sbx.name}</span>
              {sbx.is_local ? (
                <span className="shrink-0 rounded bg-emerald-500/15 px-1.5 py-0.5 text-[10px] font-medium text-emerald-400">
                  local
                </span>
              ) : sbx.type === 'openclaw' ? (
                <span className="shrink-0 rounded bg-purple-500/15 px-1.5 py-0.5 text-[10px] font-medium text-purple-400">
                  claw
                </span>
              ) : (
                <span className="shrink-0 rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-medium text-blue-400">
                  code
                </span>
              )}
              <div className="hidden gap-0.5 group-hover:flex">
                {!sbx.is_local && sbx.status === 'running' && (
                  <button
                    onClick={(e) => handlePause(sbx.id, e)}
                    className="rounded p-1 hover:bg-[var(--muted-foreground)]/20"
                    title="Pause sandbox"
                  >
                    <Pause size={12} />
                  </button>
                )}
                {!sbx.is_local && sbx.status === 'paused' && (
                  <button
                    onClick={(e) => handleResume(sbx.id, e)}
                    className="rounded p-1 hover:bg-[var(--muted-foreground)]/20"
                    title="Resume sandbox"
                  >
                    <Play size={12} />
                  </button>
                )}
                <button
                  onClick={(e) => handleDelete(sbx.id, e)}
                  className="rounded p-1 hover:bg-[var(--destructive)] hover:text-white"
                  title="Delete sandbox"
                >
                  <Trash2 size={12} />
                </button>
              </div>
            </div>
            {sbx.is_local && sbx.agent_info && expandedSandboxId === sbx.id && (
              <AgentInfoPanel info={sbx.agent_info} />
            )}
          </div>
        ))}
        {sandboxes.length === 0 && !creating && selectedWorkspaceId && (
          <div className="p-3 text-center text-sm text-[var(--muted-foreground)]">
            No sandboxes yet
          </div>
        )}
        {!selectedWorkspaceId && (
          <div className="p-3 text-center text-sm text-[var(--muted-foreground)]">
            Select a workspace
          </div>
        )}
      </div>

      {showCreateModal && selectedWorkspaceId && (
        <CreateSandboxModal
          workspaceId={selectedWorkspaceId}
          onClose={() => setShowCreateModal(false)}
          onCreate={handleCreateSandbox}
          creating={creating}
        />
      )}

      {confirmDelete && (
        <ConfirmModal
          title="Delete Sandbox"
          message={`Are you sure you want to delete "${confirmDelete.name}"? This action cannot be undone.`}
          confirmLabel="Delete"
          destructive
          onConfirm={() => doDelete(confirmDelete.id)}
          onCancel={() => setConfirmDelete(null)}
        />
      )}

      {confirmPause && (
        <ConfirmModal
          title="Pause Sandbox"
          message={`Are you sure you want to pause "${confirmPause.name}"?`}
          confirmLabel="Pause"
          onConfirm={() => doPause(confirmPause.id)}
          onCancel={() => setConfirmPause(null)}
        />
      )}

      {agentCodeData && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50" onClick={() => setAgentCodeData(null)}>
          <div
            className="w-full max-w-lg rounded-lg border border-[var(--border)] bg-[var(--card)] p-6 shadow-xl"
            onClick={(e) => e.stopPropagation()}
          >
            <h2 className="text-lg font-semibold text-[var(--foreground)] mb-4">Connect Local Agent</h2>
            <div className="flex flex-col gap-3 text-sm">
              <p className="text-[var(--muted-foreground)]">
                Run the following command on your local machine to connect your opencode instance:
              </p>
              <div className="relative rounded-md bg-[var(--secondary)] p-3">
                <code className="block whitespace-pre-wrap break-all text-xs text-[var(--foreground)]">
                  {agentCodeData.command}
                </code>
                <button
                  onClick={() => navigator.clipboard.writeText(agentCodeData.command)}
                  className="absolute right-2 top-2 rounded px-2 py-1 text-xs text-[var(--muted-foreground)] hover:bg-[var(--muted)] hover:text-[var(--foreground)]"
                >
                  Copy
                </button>
              </div>
              <p className="text-xs text-[var(--muted-foreground)]">
                Code expires at {new Date(agentCodeData.expires_at).toLocaleString()}. It can only be used once.
              </p>
            </div>
            <div className="flex justify-end mt-5">
              <button
                onClick={() => setAgentCodeData(null)}
                className="rounded-md border border-[var(--border)] px-4 py-2 text-sm font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]"
              >
                Close
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
