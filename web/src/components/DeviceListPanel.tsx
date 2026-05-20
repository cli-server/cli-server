import { Circle, type LucideIcon } from 'lucide-react'
import { type ReactNode } from 'react'

// DeviceRow is the unified shape consumed by both the Connectors
// (RemoteExecutorsPanel) and Browsers (CodexTokensPanel) tabs. Caller
// adapts its native type to this shape and supplies an `actions` cell
// renderer for row-level controls (unbind / revoke / etc).
export interface DeviceRow {
  id: string
  name: string
  description?: string
  is_online: boolean
  client_ip?: string
  os?: string
  codex_version?: string
  // ISO timestamps. Last-seen prefers disconnected_at then connected_at
  // then the fallback the caller can provide via lastSeenFallback (e.g.
  // last_used_at on Browsers, registered_at on Connectors).
  connected_at?: string
  disconnected_at?: string
  lastSeenFallback?: string
}

interface Props {
  title: string
  icon: LucideIcon
  iconClassName?: string
  rows: DeviceRow[]
  loading: boolean
  error: string | null
  emptyMessage: string
  headerAction?: ReactNode
  description?: ReactNode
  actions: (row: DeviceRow) => ReactNode
}

// DeviceListPanel renders a uniform Status / Name / OS / Codex / IP /
// Connected / Last seen / Actions table. Both the Connectors and Browsers
// tabs use this so they stay visually identical.
export function DeviceListPanel({
  title, icon: Icon, iconClassName,
  rows, loading, error, emptyMessage,
  headerAction, description, actions,
}: Props) {
  return (
    <div className="mt-6 rounded-lg border border-[var(--border)] bg-[var(--card)]">
      <div className="flex items-center justify-between border-b border-[var(--border)] px-5 py-3">
        <div className="flex items-center gap-2">
          <Icon size={14} className={iconClassName ?? 'text-emerald-400'} />
          <span className="text-sm font-medium text-[var(--foreground)]">{title}</span>
          {!loading && rows.length > 0 && (
            <span className="rounded-full bg-[var(--secondary)] px-2 py-0.5 text-[10px] text-[var(--muted-foreground)]">
              {rows.length}
            </span>
          )}
        </div>
        {headerAction}
      </div>

      <div className="px-5 py-4">
        {description && (
          <div className="mb-3 text-xs text-[var(--muted-foreground)]">{description}</div>
        )}
        {error && (
          <div className="mb-3 rounded-md border border-[var(--destructive)]/30 bg-[var(--destructive)]/10 px-3 py-2 text-xs text-[var(--destructive)]">
            {error}
          </div>
        )}
        {loading ? (
          <div className="text-xs text-[var(--muted-foreground)]">Loading…</div>
        ) : rows.length === 0 ? (
          <div className="rounded-md border border-dashed border-[var(--border)] py-8 text-center text-xs italic text-[var(--muted-foreground)]">
            {emptyMessage}
          </div>
        ) : (
          <div className="overflow-hidden rounded-md border border-[var(--border)]">
            <table className="w-full table-fixed border-collapse text-xs">
              <thead className="bg-[var(--secondary)] text-[var(--muted-foreground)]">
                <tr>
                  <th className="w-20 px-3 py-2 text-left font-medium">Status</th>
                  <th className="px-3 py-2 text-left font-medium">Name</th>
                  <th className="w-24 px-3 py-2 text-left font-medium">OS</th>
                  <th className="w-24 px-3 py-2 text-left font-medium">Codex</th>
                  <th className="w-36 px-3 py-2 text-left font-medium">IP</th>
                  <th className="w-40 px-3 py-2 text-left font-medium">Last seen</th>
                  <th className="w-16 px-3 py-2 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r, i) => (
                  <tr
                    key={r.id}
                    className={`border-t border-[var(--border)] ${i % 2 === 1 ? 'bg-[var(--background)]/40' : ''}`}
                  >
                    <td className="px-3 py-2">
                      <span className="inline-flex items-center gap-1.5">
                        <Circle
                          size={8}
                          className={r.is_online ? 'fill-emerald-500 text-emerald-500' : 'fill-gray-400 text-gray-400'}
                        />
                        <span className="text-[11px] text-[var(--muted-foreground)]">
                          {r.is_online ? 'Online' : 'Offline'}
                        </span>
                      </span>
                    </td>
                    <td className="px-3 py-2">
                      <div className="truncate font-medium text-[var(--foreground)]">{r.name}</div>
                      {r.description && (
                        <div className="truncate text-[11px] text-[var(--muted-foreground)]">{r.description}</div>
                      )}
                    </td>
                    <td className="px-3 py-2 text-[var(--muted-foreground)]">
                      {r.os || <span className="italic opacity-60">—</span>}
                    </td>
                    <td className="px-3 py-2 text-[var(--muted-foreground)]">
                      {r.codex_version || <span className="italic opacity-60">—</span>}
                    </td>
                    <td className="px-3 py-2 font-mono text-[11px] text-[var(--muted-foreground)]">
                      {r.client_ip || <span className="font-sans italic opacity-60">—</span>}
                    </td>
                    <td className="px-3 py-2 text-[11px] text-[var(--muted-foreground)]">
                      {formatLastSeen(r)}
                    </td>
                    <td className="px-3 py-2 text-right">{actions(r)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  )
}

// formatLastSeen picks the best timestamp to show: while online,
// `connected_at`; once offline, `disconnected_at`; otherwise the caller's
// fallback (e.g. token's last_used_at). Empty → "never".
function formatLastSeen(r: DeviceRow): ReactNode {
  const ts = r.is_online ? r.connected_at : (r.disconnected_at ?? r.connected_at ?? r.lastSeenFallback)
  if (!ts) return <span className="italic opacity-60">never</span>
  return new Date(ts).toLocaleString()
}
