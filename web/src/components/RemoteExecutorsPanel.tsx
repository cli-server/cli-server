import { useState, useEffect, useCallback } from 'react'
import { Plus, Trash2, Copy, Check, X, Server } from 'lucide-react'
import {
  type RemoteExecutor, type RegisterExecutorResponse, type ConnectCommands,
  listRemoteExecutors, registerRemoteExecutor, unbindRemoteExecutor,
} from '../lib/api'
import { ConfirmModal } from './Modals'
import { DeviceListPanel, type DeviceRow } from './DeviceListPanel'

interface Props {
  workspaceId: string
}

export default function RemoteExecutorsPanel({ workspaceId }: Props) {
  const [rows, setRows] = useState<RemoteExecutor[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [showRegister, setShowRegister] = useState(false)
  const [newName, setNewName] = useState('')
  const [newDesc, setNewDesc] = useState('')
  const [generated, setGenerated] = useState<RegisterExecutorResponse | null>(null)
  const [unbindTarget, setUnbindTarget] = useState<RemoteExecutor | null>(null)

  const refresh = useCallback(async () => {
    try {
      const r = await listRemoteExecutors(workspaceId)
      setRows(r)
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [workspaceId])

  // Initial load + 10s poll. Online state is authoritative from the API
  // (is_online from the gateway's live registry); the poll just refreshes
  // it on a reasonable cadence.
  useEffect(() => {
    void refresh()
    const id = window.setInterval(() => { void refresh() }, 10_000)
    return () => window.clearInterval(id)
  }, [refresh])

  const onRegister = async () => {
    if (!newName.trim()) return
    try {
      const resp = await registerRemoteExecutor(workspaceId, {
        name: newName.trim(),
        description: newDesc.trim() || undefined,
      })
      setGenerated(resp)
      setShowRegister(false)
      setNewName('')
      setNewDesc('')
      void refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const onUnbind = async (exeId: string) => {
    try {
      await unbindRemoteExecutor(workspaceId, exeId)
      setUnbindTarget(null)
      void refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const deviceRows: DeviceRow[] = rows.map((r) => ({
    id: r.exe_id,
    name: r.name,
    description: r.description,
    is_online: r.is_online,
    client_ip: r.client_ip,
    os: r.os,
    codex_version: r.codex_version,
    connected_at: r.connected_at,
    disconnected_at: r.disconnected_at,
    lastSeenFallback: r.last_seen_at,
  }))

  const findRow = (id: string) => rows.find((r) => r.exe_id === id)

  return (
    <>
      <DeviceListPanel
        title="Connectors"
        icon={Server}
        iconClassName="text-emerald-400"
        rows={deviceRows}
        loading={loading}
        error={error}
        emptyMessage="No connectors yet — register one to expose a machine to this workspace."
        description={
          <>
            Register a machine to expose its shell to codex sessions in this workspace.
            Run the printed <code className="rounded bg-[var(--background)] px-1 py-0.5 font-mono text-[11px] text-[var(--foreground)]">codex exec-server --remote ...</code> command on that machine to bring it online.
          </>
        }
        headerAction={
          <button
            onClick={() => setShowRegister(true)}
            className="inline-flex items-center gap-1.5 rounded-md border border-[var(--border)] bg-[var(--card)] px-3 py-1 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)] transition-colors"
          >
            <Plus size={12} />
            Register connector
          </button>
        }
        actions={(row) => (
          <button
            onClick={() => {
              const r = findRow(row.id)
              if (r) setUnbindTarget(r)
            }}
            className="rounded p-1 text-[var(--muted-foreground)] hover:bg-[var(--secondary)] hover:text-[var(--destructive)]"
            aria-label="Unbind connector"
            title="Unbind from workspace"
          >
            <Trash2 size={14} />
          </button>
        )}
      />

      {showRegister && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50" onClick={() => setShowRegister(false)}>
          <div
            className="w-full max-w-sm rounded-lg border border-[var(--border)] bg-[var(--card)] p-6 shadow-xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="mb-4 flex items-center justify-between">
              <h2 className="text-lg font-semibold text-[var(--foreground)]">Register connector</h2>
              <button onClick={() => setShowRegister(false)} className="rounded p-1 hover:bg-[var(--secondary)]">
                <X size={16} />
              </button>
            </div>
            <form onSubmit={(e) => { e.preventDefault(); void onRegister() }} className="flex flex-col gap-4">
              <div>
                <label className="mb-1 block text-sm font-medium text-[var(--foreground)]">Name</label>
                <input
                  autoFocus
                  type="text"
                  value={newName}
                  onChange={(e) => setNewName(e.target.value)}
                  placeholder="hpc-kunshan"
                  className="w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] outline-none focus:border-[var(--primary)]"
                />
                <p className="mt-1 text-[11px] text-[var(--muted-foreground)]">
                  Unique per workspace. This is what codex sees when it picks an environment.
                </p>
              </div>
              <div>
                <label className="mb-1 block text-sm font-medium text-[var(--foreground)]">Description <span className="text-[var(--muted-foreground)]">(optional)</span></label>
                <input
                  type="text"
                  value={newDesc}
                  onChange={(e) => setNewDesc(e.target.value)}
                  placeholder="Kunshan HPC cluster, SLURM partition xahdtest"
                  className="w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] outline-none focus:border-[var(--primary)]"
                />
              </div>
              <div className="flex justify-end gap-2">
                <button type="button" onClick={() => setShowRegister(false)} className="rounded-md border border-[var(--border)] px-4 py-2 text-sm font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]">
                  Cancel
                </button>
                <button type="submit" disabled={!newName.trim()} className="rounded-md bg-[var(--primary)] px-4 py-2 text-sm font-medium text-[var(--primary-foreground)] hover:opacity-90 disabled:opacity-50">
                  Register
                </button>
              </div>
            </form>
          </div>
        </div>
      )}

      {generated && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50">
          <div className="w-full max-w-2xl rounded-lg border border-[var(--border)] bg-[var(--card)] p-6 shadow-xl">
            <div className="mb-3 flex items-center justify-between">
              <h2 className="text-lg font-semibold text-[var(--foreground)]">Connector registered</h2>
              <button onClick={() => setGenerated(null)} className="rounded p-1 hover:bg-[var(--secondary)]">
                <X size={16} />
              </button>
            </div>
            <p className="mb-3 text-sm text-[var(--muted-foreground)]">
              Run this on the machine you want to expose. The token won't be shown again — copy it now.
            </p>
            {generated.connect_commands ? (
              <div className="mb-4">
                <ConnectCommandsPanel cmds={generated.connect_commands} />
              </div>
            ) : generated.connect_command ? (
              <div className="mb-4">
                <SingleCommandBlock cmd={generated.connect_command} />
              </div>
            ) : (
              <div className="mb-4 rounded-md border border-[var(--border)] bg-[var(--background)] p-3 font-mono text-xs text-[var(--foreground)]">
                <div>exe_id: {generated.exe_id}</div>
                <div>registration_token: {generated.registration_token}</div>
                <div className="mt-2 text-[var(--muted-foreground)]">
                  Gateway public host not configured — compose the connect URL manually with your operator.
                </div>
              </div>
            )}
            <div className="flex justify-end">
              <button
                onClick={() => setGenerated(null)}
                className="rounded-md bg-[var(--primary)] px-4 py-2 text-sm font-medium text-[var(--primary-foreground)] hover:opacity-90"
              >
                I've saved it
              </button>
            </div>
          </div>
        </div>
      )}

      {unbindTarget && (
        <ConfirmModal
          title="Unbind connector"
          message={`Remove "${unbindTarget.name}" from this workspace? The connector will stay registered but codex sessions here won't be able to invoke it.`}
          confirmLabel="Unbind"
          destructive
          onConfirm={() => onUnbind(unbindTarget.exe_id)}
          onCancel={() => setUnbindTarget(null)}
        />
      )}
    </>
  )
}

// --- Connect-command rendering helpers ---

function SingleCommandBlock({ cmd }: { cmd: string }) {
  const [copied, setCopied] = useState(false)
  const onCopy = async () => {
    await navigator.clipboard.writeText(cmd)
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }
  return (
    <div className="flex items-center gap-2">
      <pre className="flex-1 overflow-x-auto rounded-md border border-[var(--border)] bg-[var(--background)] p-3 font-mono text-xs text-[var(--foreground)] whitespace-pre-wrap break-all">{cmd}</pre>
      <button
        onClick={onCopy}
        className="rounded-md border border-[var(--border)] p-2 text-[var(--foreground)] hover:bg-[var(--secondary)]"
        aria-label="Copy command"
        title="Copy"
      >
        {copied ? <Check size={14} /> : <Copy size={14} />}
      </button>
    </div>
  )
}

type ConnectTabKey = keyof ConnectCommands

const CONNECT_TABS: { key: ConnectTabKey; label: string; hint: string }[] = [
  {
    key: 'agent_identity',
    label: 'Agent Identity (headless)',
    hint: 'Best for unattended machines. One paste, done.',
  },
  {
    key: 'chatgpt_browser',
    label: 'codex login (browser)',
    hint: 'For desktops. SSO with your agentserver session.',
  },
  {
    key: 'chatgpt_device_auth',
    label: 'codex login --device-auth',
    hint: 'Headless + ChatGPT-account semantics. Enter a code in a browser.',
  },
]

function ConnectCommandsPanel({ cmds }: { cmds: ConnectCommands }) {
  // Default to the first tab whose command is actually populated, falling
  // back to agent_identity if none are (so the panel still renders something).
  const firstAvailable = CONNECT_TABS.find((t) => cmds[t.key])?.key ?? 'agent_identity'
  const [tab, setTab] = useState<ConnectTabKey>(firstAvailable)
  const [copied, setCopied] = useState(false)
  const current = cmds[tab] || ''

  const onCopy = async () => {
    if (!current) return
    await navigator.clipboard.writeText(current)
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }

  return (
    <div>
      <div className="mb-3 flex flex-wrap gap-2">
        {CONNECT_TABS.map(({ key, label }) => {
          const active = tab === key
          const disabled = !cmds[key]
          return (
            <button
              key={key}
              type="button"
              disabled={disabled}
              onClick={() => setTab(key)}
              className={[
                'rounded-md border px-3 py-1 text-xs font-medium transition-colors',
                active
                  ? 'border-[var(--primary)] bg-[var(--primary)] text-[var(--primary-foreground)]'
                  : 'border-[var(--border)] bg-[var(--card)] text-[var(--foreground)] hover:bg-[var(--secondary)]',
                disabled ? 'cursor-not-allowed opacity-40' : '',
              ].join(' ')}
              title={disabled ? 'Not available for this connector' : undefined}
            >
              {label}
            </button>
          )
        })}
      </div>
      <p className="mb-2 text-[11px] text-[var(--muted-foreground)]">
        {CONNECT_TABS.find((t) => t.key === tab)!.hint}
      </p>
      <div className="flex items-center gap-2">
        <pre className="flex-1 overflow-x-auto rounded-md border border-[var(--border)] bg-[var(--background)] p-3 font-mono text-xs text-[var(--foreground)] whitespace-pre-wrap break-all">
          {current || <span className="italic opacity-60">(not available)</span>}
        </pre>
        <button
          onClick={onCopy}
          disabled={!current}
          className="rounded-md border border-[var(--border)] p-2 text-[var(--foreground)] hover:bg-[var(--secondary)] disabled:cursor-not-allowed disabled:opacity-40"
          aria-label="Copy command"
          title="Copy"
        >
          {copied ? <Check size={14} /> : <Copy size={14} />}
        </button>
      </div>
    </div>
  )
}
