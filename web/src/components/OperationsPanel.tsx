import { useCallback, useEffect, useState } from 'react'
import { RefreshCw, AlertCircle, X } from 'lucide-react'
import {
  listOperations,
  type Operation,
  type ListOperationsFilters,
} from '../lib/api'

interface Props {
  workspaceId: string
}

const AUTO_REFRESH_INTERVAL_MS = 30_000

const SOURCES: Array<'' | 'sdk' | 'tui' | 'llm'> = ['', 'sdk', 'tui', 'llm']

export default function OperationsPanel({ workspaceId }: Props) {
  const [filters, setFilters] = useState<ListOperationsFilters>({ limit: 100 })
  const [ops, setOps] = useState<Operation[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [autoRefresh, setAutoRefresh] = useState(false)
  const [selected, setSelected] = useState<Operation | null>(null)

  const refresh = useCallback(async () => {
    setLoading(true)
    try {
      const rows = await listOperations(workspaceId, filters)
      setOps(rows)
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [workspaceId, filters])

  useEffect(() => { void refresh() }, [refresh])

  useEffect(() => {
    if (!autoRefresh) return
    const id = window.setInterval(() => void refresh(), AUTO_REFRESH_INTERVAL_MS)
    return () => window.clearInterval(id)
  }, [autoRefresh, refresh])

  return (
    <div className="flex flex-col gap-3">
      {/* Filter bar */}
      <div className="flex flex-wrap items-end gap-3 rounded-lg border border-[var(--border)] bg-[var(--card)] p-3">
        <FilterInput
          label="env"
          value={filters.env_id ?? ''}
          onChange={(v) => setFilters((f) => ({ ...f, env_id: v || undefined }))}
        />
        <FilterInput
          label="tool"
          value={filters.tool ?? ''}
          onChange={(v) => setFilters((f) => ({ ...f, tool: v || undefined }))}
        />
        <div className="flex flex-col gap-1">
          <label className="text-xs text-[var(--muted-foreground)]">source</label>
          <select
            className="rounded-md border border-[var(--border)] bg-[var(--background)] px-2 py-1 text-sm text-[var(--foreground)] focus:outline-none focus:ring-1 focus:ring-[var(--primary)]"
            value={filters.source ?? ''}
            onChange={(e) =>
              setFilters((f) => ({
                ...f,
                source: (e.target.value || undefined) as ListOperationsFilters['source'],
              }))
            }
          >
            {SOURCES.map((s) => (
              <option key={s} value={s}>
                {s || '(all)'}
              </option>
            ))}
          </select>
        </div>
        <label className="flex items-center gap-1.5 text-sm text-[var(--foreground)]">
          <input
            type="checkbox"
            checked={filters.is_error === true}
            onChange={(e) =>
              setFilters((f) => ({
                ...f,
                is_error: e.target.checked ? true : undefined,
              }))
            }
          />
          errors only
        </label>
        <label className="flex items-center gap-1.5 text-sm text-[var(--foreground)]">
          <input
            type="checkbox"
            checked={autoRefresh}
            onChange={(e) => setAutoRefresh(e.target.checked)}
          />
          auto-refresh (30s)
        </label>
        <button
          onClick={() => void refresh()}
          className="inline-flex items-center gap-1.5 rounded-md border border-[var(--border)] bg-[var(--card)] px-3 py-1 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)] transition-colors"
        >
          <RefreshCw size={12} className={loading ? 'animate-spin' : ''} />
          Refresh
        </button>
      </div>

      {error && (
        <div className="flex items-start gap-2 rounded-lg border border-red-500/30 bg-red-500/10 p-3">
          <AlertCircle size={16} className="mt-0.5 shrink-0 text-red-400" />
          <div className="whitespace-pre-wrap text-sm text-red-400">{error}</div>
        </div>
      )}

      {/* Table */}
      <div className="overflow-x-auto rounded-lg border border-[var(--border)] bg-[var(--card)]">
        <table className="w-full text-sm">
          <thead className="bg-[var(--muted)] text-[var(--muted-foreground)]">
            <tr>
              <th className="px-3 py-2 text-left font-medium">started</th>
              <th className="px-3 py-2 text-left font-medium">env</th>
              <th className="px-3 py-2 text-left font-medium">tool</th>
              <th className="px-3 py-2 text-left font-medium">source</th>
              <th className="px-3 py-2 text-left font-medium">user</th>
              <th className="px-3 py-2 text-right font-medium">dur (ms)</th>
              <th className="px-3 py-2 text-left font-medium">status</th>
            </tr>
          </thead>
          <tbody>
            {ops.length === 0 && !loading ? (
              <tr>
                <td colSpan={7} className="px-3 py-6 text-center text-sm text-[var(--muted-foreground)]">
                  No operations match these filters.
                </td>
              </tr>
            ) : (
              ops.map((op) => (
                <tr
                  key={op.id}
                  onClick={() => setSelected(op)}
                  className="cursor-pointer border-t border-[var(--border)] hover:bg-[var(--secondary)]/50"
                >
                  <td className="px-3 py-1.5 font-mono text-xs text-[var(--foreground)]">
                    {new Date(op.started_at).toLocaleString()}
                  </td>
                  <td className="px-3 py-1.5 font-mono text-xs text-[var(--foreground)]">{op.env_id}</td>
                  <td className="px-3 py-1.5 font-mono text-xs text-[var(--foreground)]">{op.tool}</td>
                  <td className="px-3 py-1.5 text-xs text-[var(--foreground)]">{op.source}</td>
                  <td className="px-3 py-1.5 font-mono text-xs text-[var(--muted-foreground)]">
                    {op.user_id ?? '—'}
                  </td>
                  <td className="px-3 py-1.5 text-right tabular-nums text-xs text-[var(--foreground)]">{op.duration_ms}</td>
                  <td className="px-3 py-1.5 text-xs">
                    {op.is_error ? (
                      <span className="font-medium text-red-400">error</span>
                    ) : (
                      <span className="text-green-400">ok</span>
                    )}
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      {/* Detail modal */}
      {selected && (
        <OperationDetailModal op={selected} onClose={() => setSelected(null)} />
      )}
    </div>
  )
}

function FilterInput({
  label,
  value,
  onChange,
}: {
  label: string
  value: string
  onChange: (v: string) => void
}) {
  return (
    <div className="flex flex-col gap-1">
      <label className="text-xs text-[var(--muted-foreground)]">{label}</label>
      <input
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="w-32 rounded-md border border-[var(--border)] bg-[var(--background)] px-2 py-1 text-sm text-[var(--foreground)] placeholder:text-[var(--muted-foreground)] focus:outline-none focus:ring-1 focus:ring-[var(--primary)]"
      />
    </div>
  )
}

function OperationDetailModal({
  op,
  onClose,
}: {
  op: Operation
  onClose: () => void
}) {
  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      onClick={onClose}
    >
      <div
        className="max-h-[80vh] w-full max-w-3xl overflow-auto rounded-lg border border-[var(--border)] bg-[var(--card)] shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between border-b border-[var(--border)] px-4 py-3">
          <div className="font-mono text-sm text-[var(--foreground)]">{op.id}</div>
          <button
            onClick={onClose}
            className="text-[var(--muted-foreground)] hover:text-[var(--foreground)]"
          >
            <X size={16} />
          </button>
        </div>
        <div className="space-y-3 p-4 text-sm">
          <Row label="started_at" value={op.started_at} />
          <Row label="duration_ms" value={String(op.duration_ms)} />
          <Row label="env" value={op.env_id} />
          <Row label="tool" value={op.tool} />
          <Row label="source" value={op.source} />
          <Row label="user" value={op.user_id ?? '—'} />
          <Row
            label="is_error"
            value={op.is_error ? 'true' : 'false'}
            valueClass={op.is_error ? 'text-red-400 font-medium' : ''}
          />
          {op.arguments !== undefined && op.arguments !== null && (
            <div>
              <div className="mb-1 text-xs text-[var(--muted-foreground)]">arguments</div>
              <pre className="max-h-64 overflow-auto rounded-md border border-[var(--border)] bg-[var(--background)] p-2 text-xs text-[var(--foreground)]">
                {JSON.stringify(op.arguments, null, 2)}
              </pre>
            </div>
          )}
          {op.arguments_meta && (
            <div className="text-xs text-[var(--muted-foreground)]">
              arguments truncated: {op.arguments_meta.size_bytes} bytes, sha256{' '}
              <code className="text-[var(--foreground)]">{op.arguments_meta.sha256.slice(0, 12)}…</code>
            </div>
          )}
          {op.result_summary && (
            <div>
              <div className="mb-1 text-xs text-[var(--muted-foreground)]">result_summary</div>
              <pre className="max-h-64 overflow-auto whitespace-pre-wrap rounded-md border border-[var(--border)] bg-[var(--background)] p-2 text-xs text-[var(--foreground)]">
                {op.result_summary}
              </pre>
            </div>
          )}
          {op.result_meta && (
            <div className="text-xs text-[var(--muted-foreground)]">
              result truncated: total {op.result_meta.total_bytes} bytes
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function Row({
  label,
  value,
  valueClass = '',
}: {
  label: string
  value: string
  valueClass?: string
}) {
  return (
    <div className="flex gap-3">
      <div className="w-24 shrink-0 text-xs text-[var(--muted-foreground)]">{label}</div>
      <div className={`flex-1 font-mono text-[var(--foreground)] ${valueClass}`}>{value}</div>
    </div>
  )
}
