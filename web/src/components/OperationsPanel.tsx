import { useCallback, useEffect, useState } from 'react'
import { RefreshCw, AlertCircle } from 'lucide-react'
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
      <div className="flex flex-wrap items-end gap-3 p-3 border rounded bg-gray-50">
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
        <div className="flex flex-col">
          <label className="text-xs text-gray-600">source</label>
          <select
            className="border rounded px-2 py-1 text-sm"
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
        <label className="flex items-center gap-1 text-sm">
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
        <label className="flex items-center gap-1 text-sm">
          <input
            type="checkbox"
            checked={autoRefresh}
            onChange={(e) => setAutoRefresh(e.target.checked)}
          />
          auto-refresh (30s)
        </label>
        <button
          onClick={() => void refresh()}
          className="flex items-center gap-1 px-3 py-1 text-sm bg-blue-600 text-white rounded hover:bg-blue-700"
        >
          <RefreshCw className={`w-3 h-3 ${loading ? 'animate-spin' : ''}`} />
          Refresh
        </button>
      </div>

      {error && (
        <div className="flex items-start gap-2 p-3 border border-red-300 bg-red-50 rounded">
          <AlertCircle className="w-4 h-4 text-red-600 mt-0.5 shrink-0" />
          <div className="text-sm text-red-800 whitespace-pre-wrap">{error}</div>
        </div>
      )}

      {/* Table */}
      <div className="overflow-x-auto border rounded">
        <table className="w-full text-sm">
          <thead className="bg-gray-100 text-gray-700">
            <tr>
              <th className="px-3 py-2 text-left">started</th>
              <th className="px-3 py-2 text-left">env</th>
              <th className="px-3 py-2 text-left">tool</th>
              <th className="px-3 py-2 text-left">source</th>
              <th className="px-3 py-2 text-left">user</th>
              <th className="px-3 py-2 text-right">dur (ms)</th>
              <th className="px-3 py-2 text-left">status</th>
            </tr>
          </thead>
          <tbody>
            {ops.length === 0 && !loading ? (
              <tr>
                <td colSpan={7} className="px-3 py-6 text-center text-gray-500">
                  No operations match these filters.
                </td>
              </tr>
            ) : (
              ops.map((op) => (
                <tr
                  key={op.id}
                  onClick={() => setSelected(op)}
                  className="border-t cursor-pointer hover:bg-gray-50"
                >
                  <td className="px-3 py-1 font-mono text-xs">
                    {new Date(op.started_at).toLocaleString()}
                  </td>
                  <td className="px-3 py-1 font-mono">{op.env_id}</td>
                  <td className="px-3 py-1 font-mono">{op.tool}</td>
                  <td className="px-3 py-1">{op.source}</td>
                  <td className="px-3 py-1 font-mono text-xs text-gray-600">
                    {op.user_id ?? '—'}
                  </td>
                  <td className="px-3 py-1 text-right tabular-nums">{op.duration_ms}</td>
                  <td className="px-3 py-1">
                    {op.is_error ? (
                      <span className="text-red-600 font-medium">error</span>
                    ) : (
                      <span className="text-green-700">ok</span>
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
    <div className="flex flex-col">
      <label className="text-xs text-gray-600">{label}</label>
      <input
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="border rounded px-2 py-1 text-sm w-32"
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
      className="fixed inset-0 bg-black/40 flex items-center justify-center z-50 p-4"
      onClick={onClose}
    >
      <div
        className="bg-white rounded-lg shadow-xl max-w-3xl w-full max-h-[80vh] overflow-auto"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-4 py-3 border-b flex items-center justify-between">
          <div className="font-mono text-sm">{op.id}</div>
          <button
            onClick={onClose}
            className="text-gray-500 hover:text-gray-900"
          >
            ✕
          </button>
        </div>
        <div className="p-4 space-y-3 text-sm">
          <Row label="started_at" value={op.started_at} />
          <Row label="duration_ms" value={String(op.duration_ms)} />
          <Row label="env" value={op.env_id} />
          <Row label="tool" value={op.tool} />
          <Row label="source" value={op.source} />
          <Row label="user" value={op.user_id ?? '—'} />
          <Row
            label="is_error"
            value={op.is_error ? 'true' : 'false'}
            valueClass={op.is_error ? 'text-red-600 font-medium' : ''}
          />
          {op.arguments !== undefined && op.arguments !== null && (
            <div>
              <div className="text-xs text-gray-600 mb-1">arguments</div>
              <pre className="bg-gray-50 border rounded p-2 overflow-auto max-h-64 text-xs">
                {JSON.stringify(op.arguments, null, 2)}
              </pre>
            </div>
          )}
          {op.arguments_meta && (
            <div className="text-xs text-gray-600">
              arguments truncated: {op.arguments_meta.size_bytes} bytes, sha256{' '}
              <code>{op.arguments_meta.sha256.slice(0, 12)}…</code>
            </div>
          )}
          {op.result_summary && (
            <div>
              <div className="text-xs text-gray-600 mb-1">result_summary</div>
              <pre className="bg-gray-50 border rounded p-2 overflow-auto max-h-64 text-xs whitespace-pre-wrap">
                {op.result_summary}
              </pre>
            </div>
          )}
          {op.result_meta && (
            <div className="text-xs text-gray-600">
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
      <div className="text-xs text-gray-600 w-24 shrink-0">{label}</div>
      <div className={`flex-1 font-mono ${valueClass}`}>{value}</div>
    </div>
  )
}
