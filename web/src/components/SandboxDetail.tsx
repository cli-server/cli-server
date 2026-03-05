import { useState, useEffect, useCallback } from 'react'
import {
  ExternalLink,
  Clock,
  Activity,
  Cpu,
  MemoryStick,
  Timer,
  Hash,
  MessageSquare,
  ChevronLeft,
  ChevronRight,
  Play,
  Pause,
  Trash2,
  LayoutDashboard,
  Copy,
  Check,
} from 'lucide-react'
import {
  getSandboxUsage,
  getSandboxTraces,
  getTraceDetail,
  type Sandbox,
  type UsageSummary,
  type TraceItem,
  type TokenUsageItem,
  type TraceDetailResponse,
} from '../lib/api'
import { ConfirmModal } from './Modals'

type Tab = 'overview' | 'traces'

interface SandboxDetailProps {
  sandbox: Sandbox
  onPause: (id: string) => void
  onResume: (id: string) => void
  onDelete: (id: string) => void
}

export const TRACES_PER_PAGE = 20

function formatTokens(n: number): string {
  return n.toLocaleString()
}

function StatusBadge({ status, isLocal }: { status: string; isLocal: boolean }) {
  const colors: Record<string, string> = {
    running: 'bg-green-500/10 text-green-400 border-green-500/20',
    paused: 'bg-yellow-500/10 text-yellow-400 border-yellow-500/20',
    offline: 'bg-red-500/10 text-red-400 border-red-500/20',
    pausing: 'bg-orange-500/10 text-orange-400 border-orange-500/20',
    resuming: 'bg-blue-500/10 text-blue-400 border-blue-500/20',
    creating: 'bg-gray-500/10 text-[var(--muted-foreground)] border-gray-500/20',
  }
  const dotColors: Record<string, string> = {
    running: 'bg-green-400',
    paused: 'bg-yellow-400',
    offline: 'bg-red-400',
    pausing: 'bg-orange-400',
    resuming: 'bg-blue-400',
    creating: 'bg-gray-400',
  }
  return (
    <div className="flex items-center gap-2">
      <span className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium ${colors[status] || colors.creating}`}>
        <span className={`inline-block h-1.5 w-1.5 rounded-full ${dotColors[status] || dotColors.creating}`} />
        {status}
      </span>
      {isLocal && (
        <span className="rounded-full border border-emerald-500/20 bg-emerald-500/10 px-2 py-0.5 text-[10px] font-medium text-emerald-400">
          local
        </span>
      )}
    </div>
  )
}

function CopyableId({ id }: { id: string }) {
  const [copied, setCopied] = useState(false)
  const handleCopy = useCallback(() => {
    navigator.clipboard.writeText(id).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }, [id])
  return (
    <span className="inline-flex items-center gap-1 text-xs text-[var(--muted-foreground)] font-mono">
      {id.slice(0, 8)}
      <button
        onClick={handleCopy}
        className="rounded p-0.5 hover:bg-[var(--secondary)] hover:text-[var(--foreground)] transition-colors"
        title="Copy sandbox ID"
      >
        {copied ? <Check size={12} className="text-green-400" /> : <Copy size={12} />}
      </button>
    </span>
  )
}

export function SandboxDetail({ sandbox, onPause, onResume, onDelete }: SandboxDetailProps) {
  const [tab, setTab] = useState<Tab>('overview')
  const [usageData, setUsageData] = useState<UsageSummary[] | null>(null)
  const [traces, setTraces] = useState<TraceItem[]>([])
  const [tracesTotal, setTracesTotal] = useState(0)
  const [tracesPage, setTracesPage] = useState(0)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [confirmPause, setConfirmPause] = useState(false)

  // Fetch data on sandbox change.
  useEffect(() => {
    setUsageData(null)
    setTraces([])
    setTracesTotal(0)
    setTracesPage(0)
    setTab('overview')
    getSandboxUsage(sandbox.id).then((r) => setUsageData(r.usage || [])).catch(() => {})
    getSandboxTraces(sandbox.id, TRACES_PER_PAGE, 0).then((r) => {
      setTraces(r.traces || [])
      setTracesTotal(r.total || 0)
    }).catch(() => {})
  }, [sandbox.id])

  // Fetch traces on page change.
  useEffect(() => {
    if (tracesPage === 0) return
    getSandboxTraces(sandbox.id, TRACES_PER_PAGE, tracesPage * TRACES_PER_PAGE).then((r) => {
      setTraces(r.traces || [])
      setTracesTotal(r.total || 0)
    }).catch(() => {})
  }, [sandbox.id, tracesPage])

  const isRunning = sandbox.status === 'running'
  const isPaused = sandbox.status === 'paused'
  const isTransitional = sandbox.status === 'pausing' || sandbox.status === 'resuming' || sandbox.status === 'creating'
  const isOpenClaw = sandbox.type === 'openclaw'
  const sandboxUrl = isOpenClaw ? sandbox.openclaw_url : sandbox.opencode_url

  const totalRequests = usageData ? usageData.reduce((s, u) => s + u.request_count, 0) : 0
  const totalInput = usageData ? usageData.reduce((s, u) => s + u.input_tokens, 0) : 0
  const totalOutput = usageData ? usageData.reduce((s, u) => s + u.output_tokens, 0) : 0
  const totalCacheRead = usageData ? usageData.reduce((s, u) => s + u.cache_read_input_tokens, 0) : 0
  const totalCacheWrite = usageData ? usageData.reduce((s, u) => s + u.cache_creation_input_tokens, 0) : 0
  const totalPages = Math.ceil(tracesTotal / TRACES_PER_PAGE)

  const tabs: { key: Tab; label: string; icon: React.ReactNode }[] = [
    { key: 'overview', label: 'Overview', icon: <LayoutDashboard size={15} /> },
    { key: 'traces', label: 'Traces', icon: <MessageSquare size={15} /> },
  ]

  return (
    <div className="flex h-full w-full flex-col">
      {/* Header */}
      <div className="shrink-0 border-b border-[var(--border)] bg-[var(--card)] px-6 py-4">
        <div className="flex items-start justify-between gap-4">
          <div className="min-w-0 flex-1">
            <h1 className="text-lg font-semibold text-[var(--foreground)] truncate">{sandbox.name}</h1>
            <div className="mt-1.5 flex items-center gap-3">
              <StatusBadge status={sandbox.status} isLocal={sandbox.is_local} />
              <CopyableId id={sandbox.id} />
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            {isRunning && sandboxUrl && (
              <a
                href={sandboxUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1.5 rounded-md bg-[var(--primary)] px-3 py-1.5 text-xs font-medium text-[var(--primary-foreground)] hover:opacity-90 transition-opacity"
              >
                <ExternalLink size={13} />
                {isOpenClaw ? 'Open' : 'Open'}
              </a>
            )}
            {!sandbox.is_local && isRunning && (
              <button
                onClick={() => setConfirmPause(true)}
                className="inline-flex items-center gap-1.5 rounded-md border border-[var(--border)] bg-[var(--card)] px-3 py-1.5 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)] transition-colors"
              >
                <Pause size={13} />
                Pause
              </button>
            )}
            {!sandbox.is_local && isPaused && (
              <button
                onClick={() => onResume(sandbox.id)}
                className="inline-flex items-center gap-1.5 rounded-md border border-green-500/30 bg-green-500/10 px-3 py-1.5 text-xs font-medium text-green-400 hover:bg-green-500/20 transition-colors"
              >
                <Play size={13} />
                Resume
              </button>
            )}
            <button
              onClick={() => setConfirmDelete(true)}
              disabled={isTransitional}
              className="inline-flex items-center gap-1.5 rounded-md border border-red-500/30 bg-transparent px-3 py-1.5 text-xs font-medium text-red-400 hover:bg-red-500/10 transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
            >
              <Trash2 size={13} />
              Delete
            </button>
          </div>
        </div>

        {/* Tabs */}
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
        {tab === 'overview' && <OverviewTab sandbox={sandbox} usageData={usageData} totals={{ totalRequests, totalInput, totalOutput, totalCacheRead, totalCacheWrite }} />}
        {tab === 'traces' && <TracesTab traces={traces} tracesTotal={tracesTotal} tracesPage={tracesPage} totalPages={totalPages} onPageChange={setTracesPage} fetchDetail={(traceId) => getTraceDetail(sandbox.id, traceId)} />}
      </div>

      {/* Modals */}
      {confirmDelete && (
        <ConfirmModal
          title="Delete Sandbox"
          message={`Are you sure you want to delete "${sandbox.name}"? This action cannot be undone.`}
          confirmLabel="Delete"
          destructive
          onConfirm={() => { setConfirmDelete(false); onDelete(sandbox.id) }}
          onCancel={() => setConfirmDelete(false)}
        />
      )}
      {confirmPause && (
        <ConfirmModal
          title="Pause Sandbox"
          message={`Are you sure you want to pause "${sandbox.name}"?`}
          confirmLabel="Pause"
          onConfirm={() => { setConfirmPause(false); onPause(sandbox.id) }}
          onCancel={() => setConfirmPause(false)}
        />
      )}
    </div>
  )
}

function OverviewTab({ sandbox, usageData, totals }: {
  sandbox: Sandbox
  usageData: UsageSummary[] | null
  totals: { totalRequests: number; totalInput: number; totalOutput: number; totalCacheRead: number; totalCacheWrite: number }
}) {
  const isOffline = sandbox.status === 'offline'
  const isRunning = sandbox.status === 'running'
  const isOpenClaw = sandbox.type === 'openclaw'
  const sandboxUrl = isOpenClaw ? sandbox.openclaw_url : sandbox.opencode_url
  const fallbackLabel = isOpenClaw ? 'OpenClaw' : 'OpenCode'

  return (
    <div className="flex flex-col gap-6 max-w-3xl">
      {/* Status message for non-running */}
      {!isRunning && (
        <div className={`rounded-lg border px-4 py-3 text-sm ${
          isOffline
            ? 'border-red-500/20 bg-red-500/5 text-red-400'
            : sandbox.status === 'paused'
              ? 'border-yellow-500/20 bg-yellow-500/5 text-yellow-400'
              : 'border-[var(--border)] bg-[var(--secondary)] text-[var(--muted-foreground)]'
        }`}>
          {isOffline
            ? 'Agent is offline. Reconnect the local agent to access.'
            : sandbox.status === 'paused'
              ? 'Sandbox is paused. Resume to continue working.'
              : `Sandbox is ${sandbox.status}...`}
        </div>
      )}
      {isRunning && !sandboxUrl && (
        <div className="rounded-lg border border-[var(--border)] bg-[var(--secondary)] px-4 py-3 text-sm text-[var(--muted-foreground)]">
          {fallbackLabel} URL not configured
        </div>
      )}

      {/* Info Grid */}
      <div className="grid grid-cols-2 gap-4 sm:grid-cols-3">
        <InfoCard icon={<Clock size={14} />} label="Created" value={new Date(sandbox.created_at).toLocaleString()} />
        {sandbox.last_activity_at && (
          <InfoCard icon={<Activity size={14} />} label="Last active" value={new Date(sandbox.last_activity_at).toLocaleString()} />
        )}
        {sandbox.idle_timeout != null && (
          <InfoCard
            icon={<Timer size={14} />}
            label="Idle timeout"
            value={sandbox.idle_timeout >= 60 ? `${Math.round(sandbox.idle_timeout / 60)} min` : `${sandbox.idle_timeout}s`}
          />
        )}
        {!sandbox.is_local && sandbox.cpu ? (
          <InfoCard icon={<Cpu size={14} />} label="CPU" value={`${(sandbox.cpu / 1000).toFixed(1)} cores`} />
        ) : null}
        {!sandbox.is_local && sandbox.memory ? (
          <InfoCard icon={<MemoryStick size={14} />} label="Memory" value={`${Math.round(sandbox.memory / (1024 * 1024))} MB`} />
        ) : null}
      </div>

      {/* Usage */}
      {usageData && usageData.length > 0 && (
        <div className="rounded-lg border border-[var(--border)] bg-[var(--card)]">
          <div className="flex items-center gap-2 border-b border-[var(--border)] px-5 py-3">
            <Hash size={14} className="text-[var(--muted-foreground)]" />
            <span className="text-sm font-medium text-[var(--foreground)]">Token Usage</span>
          </div>
          <div className="grid grid-cols-2 gap-px bg-[var(--border)] sm:grid-cols-5">
            <StatCell label="Requests" value={formatTokens(totals.totalRequests)} />
            <StatCell label="Input" value={formatTokens(totals.totalInput)} />
            <StatCell label="Output" value={formatTokens(totals.totalOutput)} />
            <StatCell label="Cache read" value={formatTokens(totals.totalCacheRead)} />
            <StatCell label="Cache write" value={formatTokens(totals.totalCacheWrite)} />
          </div>
          {usageData.length > 1 && (
            <div className="border-t border-[var(--border)] px-5 py-3">
              <div className="text-xs text-[var(--muted-foreground)] mb-2">Per model</div>
              <div className="flex flex-col gap-2">
                {usageData.map((u) => (
                  <div key={`${u.provider}-${u.model}`} className="flex items-center justify-between text-xs">
                    <span className="text-[var(--foreground)] font-mono truncate mr-3">{u.model}</span>
                    <div className="flex items-center gap-3 text-[var(--muted-foreground)] whitespace-nowrap">
                      <span>{formatTokens(u.request_count)} req</span>
                      <span>{formatTokens(u.input_tokens + u.output_tokens)} tok</span>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          )}
        </div>
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

export function TracesTab({ traces, tracesTotal, tracesPage, totalPages, onPageChange, fetchDetail, showSandboxId }: {
  traces: TraceItem[]
  tracesTotal: number
  tracesPage: number
  totalPages: number
  onPageChange: (page: number) => void
  fetchDetail: (traceId: string) => Promise<TraceDetailResponse>
  showSandboxId?: boolean
}) {
  const [expandedTraceId, setExpandedTraceId] = useState<string | null>(null)
  const [traceRequests, setTraceRequests] = useState<Record<string, TokenUsageItem[]>>({})
  const [loadingTraceId, setLoadingTraceId] = useState<string | null>(null)

  const toggleExpand = async (traceId: string) => {
    if (expandedTraceId === traceId) {
      setExpandedTraceId(null)
      return
    }
    setExpandedTraceId(traceId)
    if (traceRequests[traceId]) return
    setLoadingTraceId(traceId)
    try {
      const detail = await fetchDetail(traceId)
      setTraceRequests((prev) => ({ ...prev, [traceId]: detail.requests || [] }))
    } catch {
      // leave empty on error
    } finally {
      setLoadingTraceId(null)
    }
  }

  if (traces.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-16 text-[var(--muted-foreground)]">
        <MessageSquare size={32} className="mb-3 opacity-30" />
        <span className="text-sm">No traces yet</span>
      </div>
    )
  }

  return (
    <div>
      <div className="rounded-lg border border-[var(--border)] bg-[var(--card)] overflow-hidden">
        <table className="w-full text-xs">
          <thead>
            <tr className="border-b border-[var(--border)] bg-[var(--secondary)]/50">
              <th className="w-8 py-2.5 px-2" />
              <th className="text-left py-2.5 px-4 font-medium text-[var(--muted-foreground)]">Session</th>
              {showSandboxId && <th className="text-left py-2.5 px-4 font-medium text-[var(--muted-foreground)]">Sandbox</th>}
              <th className="text-left py-2.5 px-4 font-medium text-[var(--muted-foreground)]">Source</th>
              <th className="text-right py-2.5 px-4 font-medium text-[var(--muted-foreground)]">Req</th>
              <th className="text-right py-2.5 px-4 font-medium text-[var(--muted-foreground)]">Input</th>
              <th className="text-right py-2.5 px-4 font-medium text-[var(--muted-foreground)]">Output</th>
              <th className="text-right py-2.5 px-4 font-medium text-[var(--muted-foreground)]">Cache R</th>
              <th className="text-right py-2.5 px-4 font-medium text-[var(--muted-foreground)]">Cache W</th>
              <th className="text-right py-2.5 px-4 font-medium text-[var(--muted-foreground)]">Last active</th>
            </tr>
          </thead>
          <tbody>
            {traces.map((t) => {
              const isExpanded = expandedTraceId === t.id
              const isLoading = loadingTraceId === t.id
              const requests = traceRequests[t.id]
              return (
                <>
                  <tr key={t.id} className="border-b border-[var(--border)] last:border-0 hover:bg-[var(--secondary)]/30 transition-colors cursor-pointer" onClick={() => toggleExpand(t.id)}>
                    <td className="py-2.5 px-2 text-center text-[var(--muted-foreground)]">
                      <ChevronRight size={14} className={`inline-block transition-transform ${isExpanded ? 'rotate-90' : ''}`} />
                    </td>
                    <td className="py-2.5 px-4 text-[var(--muted-foreground)] font-mono truncate max-w-[200px]" title={t.id}>{t.id}</td>
                    {showSandboxId && <td className="py-2.5 px-4 text-[var(--muted-foreground)] font-mono truncate max-w-[140px]" title={t.sandbox_id}>{t.sandbox_id}</td>}
                    <td className="py-2.5 px-4 text-[var(--foreground)] font-mono truncate max-w-[140px]">{t.source || '-'}</td>
                    <td className="py-2.5 px-4 text-right text-[var(--muted-foreground)]">{t.request_count}</td>
                    <td className="py-2.5 px-4 text-right text-[var(--muted-foreground)]">{formatTokens(t.total_input_tokens)}</td>
                    <td className="py-2.5 px-4 text-right text-[var(--muted-foreground)]">{formatTokens(t.total_output_tokens)}</td>
                    <td className="py-2.5 px-4 text-right text-[var(--muted-foreground)]">{formatTokens(t.total_cache_read_tokens)}</td>
                    <td className="py-2.5 px-4 text-right text-[var(--muted-foreground)]">{formatTokens(t.total_cache_creation_tokens)}</td>
                    <td className="py-2.5 px-4 text-right text-[var(--muted-foreground)] whitespace-nowrap">{new Date(t.updated_at).toLocaleString()}</td>
                  </tr>
                  {isExpanded && (
                    <tr key={`${t.id}-detail`} className="border-b border-[var(--border)] last:border-0">
                      <td colSpan={9} className="p-0">
                        {isLoading ? (
                          <div className="py-4 text-center text-xs text-[var(--muted-foreground)]">Loading requests...</div>
                        ) : requests && requests.length > 0 ? (
                          <table className="w-full text-xs bg-[var(--secondary)]/30">
                            <thead>
                              <tr className="border-b border-[var(--border)]">
                                <th className="text-left py-2 px-4 pl-10 font-medium text-[var(--muted-foreground)]">Msg ID</th>
                                <th className="text-left py-2 px-4 font-medium text-[var(--muted-foreground)]">Model</th>
                                <th className="text-right py-2 px-4 font-medium text-[var(--muted-foreground)]">Input</th>
                                <th className="text-right py-2 px-4 font-medium text-[var(--muted-foreground)]">Output</th>
                                <th className="text-right py-2 px-4 font-medium text-[var(--muted-foreground)]">Cache R</th>
                                <th className="text-right py-2 px-4 font-medium text-[var(--muted-foreground)]">Cache W</th>
                                <th className="text-right py-2 px-4 font-medium text-[var(--muted-foreground)]">Duration</th>
                                <th className="text-right py-2 px-4 font-medium text-[var(--muted-foreground)]">TTFT</th>
                                <th className="text-center py-2 px-4 font-medium text-[var(--muted-foreground)]">Stream</th>
                                <th className="text-right py-2 px-4 font-medium text-[var(--muted-foreground)]">Time</th>
                              </tr>
                            </thead>
                            <tbody>
                              {requests.map((req) => (
                                <tr key={req.id} className="border-b border-[var(--border)] last:border-0">
                                  <td className="py-2 px-4 pl-10 text-[var(--muted-foreground)] font-mono truncate max-w-[200px]" title={req.message_id || ''}>{req.message_id || '-'}</td>
                                  <td className="py-2 px-4 text-[var(--foreground)] font-mono truncate max-w-[160px]">{req.model}</td>
                                  <td className="py-2 px-4 text-right text-[var(--muted-foreground)]">{formatTokens(req.input_tokens)}</td>
                                  <td className="py-2 px-4 text-right text-[var(--muted-foreground)]">{formatTokens(req.output_tokens)}</td>
                                  <td className="py-2 px-4 text-right text-[var(--muted-foreground)]">{formatTokens(req.cache_read_input_tokens)}</td>
                                  <td className="py-2 px-4 text-right text-[var(--muted-foreground)]">{formatTokens(req.cache_creation_input_tokens)}</td>
                                  <td className="py-2 px-4 text-right text-[var(--muted-foreground)]">{req.duration ? `${(req.duration / 1000).toFixed(1)}s` : '-'}</td>
                                  <td className="py-2 px-4 text-right text-[var(--muted-foreground)]">{req.ttft ? `${req.ttft}ms` : '-'}</td>
                                  <td className="py-2 px-4 text-center text-[var(--muted-foreground)]">{req.streaming ? 'Yes' : 'No'}</td>
                                  <td className="py-2 px-4 text-right text-[var(--muted-foreground)] whitespace-nowrap">{new Date(req.created_at).toLocaleString()}</td>
                                </tr>
                              ))}
                            </tbody>
                          </table>
                        ) : (
                          <div className="py-4 text-center text-xs text-[var(--muted-foreground)]">No requests in this trace</div>
                        )}
                      </td>
                    </tr>
                  )}
                </>
              )
            })}
          </tbody>
        </table>
      </div>
      {totalPages > 1 && (
        <div className="flex items-center justify-between mt-4">
          <span className="text-xs text-[var(--muted-foreground)]">
            {tracesPage * TRACES_PER_PAGE + 1}&ndash;{Math.min((tracesPage + 1) * TRACES_PER_PAGE, tracesTotal)} of {tracesTotal}
          </span>
          <div className="flex items-center gap-1">
            <button
              onClick={() => onPageChange(Math.max(0, tracesPage - 1))}
              disabled={tracesPage === 0}
              className="inline-flex items-center justify-center rounded-md border border-[var(--border)] w-7 h-7 text-xs text-[var(--foreground)] hover:bg-[var(--secondary)] disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
            >
              <ChevronLeft size={14} />
            </button>
            {(() => {
              const pages: (number | '...')[] = []
              if (totalPages <= 7) {
                for (let i = 0; i < totalPages; i++) pages.push(i)
              } else {
                pages.push(0)
                if (tracesPage > 2) pages.push('...')
                const start = Math.max(1, tracesPage - 1)
                const end = Math.min(totalPages - 2, tracesPage + 1)
                for (let i = start; i <= end; i++) pages.push(i)
                if (tracesPage < totalPages - 3) pages.push('...')
                pages.push(totalPages - 1)
              }
              return pages.map((p, i) =>
                p === '...' ? (
                  <span key={`ellipsis-${i}`} className="w-7 h-7 flex items-center justify-center text-xs text-[var(--muted-foreground)]">…</span>
                ) : (
                  <button
                    key={p}
                    onClick={() => onPageChange(p)}
                    className={`w-7 h-7 rounded-md text-xs transition-colors ${
                      p === tracesPage
                        ? 'bg-[var(--primary)] text-[var(--primary-foreground)]'
                        : 'border border-[var(--border)] text-[var(--foreground)] hover:bg-[var(--secondary)]'
                    }`}
                  >
                    {p + 1}
                  </button>
                )
              )
            })()}
            <button
              onClick={() => onPageChange(Math.min(totalPages - 1, tracesPage + 1))}
              disabled={tracesPage >= totalPages - 1}
              className="inline-flex items-center justify-center rounded-md border border-[var(--border)] w-7 h-7 text-xs text-[var(--foreground)] hover:bg-[var(--secondary)] disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
            >
              <ChevronRight size={14} />
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
