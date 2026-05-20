import { useState, useEffect, useCallback } from 'react'
import { Plus, Trash2, Copy, Check, X, Key } from 'lucide-react'
import {
  type CodexToken, type MintCodexTokenResponse,
  listCodexTokens, mintCodexToken, revokeCodexToken,
} from '../lib/api'
import { ConfirmModal } from './Modals'

interface Props {
  workspaceId: string
}

const TTL_OPTIONS = [1, 7, 30, 90, 180, 365] as const

export default function CodexTokensPanel({ workspaceId }: Props) {
  const [tokens, setTokens] = useState<CodexToken[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [showMint, setShowMint] = useState(false)
  const [newName, setNewName] = useState('')
  const [newTTL, setNewTTL] = useState<number>(90)
  const [generated, setGenerated] = useState<MintCodexTokenResponse | null>(null)
  const [copied, setCopied] = useState(false)
  const [revokeTarget, setRevokeTarget] = useState<CodexToken | null>(null)

  const refresh = useCallback(async () => {
    setLoading(true)
    try {
      const rows = await listCodexTokens(workspaceId)
      setTokens(rows)
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [workspaceId])

  useEffect(() => { void refresh() }, [refresh])

  const onMint = async () => {
    if (!newName.trim()) return
    try {
      const resp = await mintCodexToken({
        workspace_id: workspaceId,
        name: newName.trim(),
        ttl_days: newTTL,
      })
      setGenerated(resp)
      setShowMint(false)
      setNewName('')
      setNewTTL(90)
      void refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const onRevoke = async (id: string) => {
    try {
      await revokeCodexToken(id)
      setRevokeTarget(null)
      void refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const copyToken = async () => {
    if (!generated) return
    await navigator.clipboard.writeText(generated.token)
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }

  return (
    <div className="mt-6 rounded-lg border border-[var(--border)] bg-[var(--card)]">
      <div className="flex items-center justify-between border-b border-[var(--border)] px-5 py-3">
        <div className="flex items-center gap-2">
          <Key size={14} className="text-blue-400" />
          <span className="text-sm font-medium text-[var(--foreground)]">Codex Remote Access</span>
          {!loading && tokens.length > 0 && (
            <span className="rounded-full bg-[var(--secondary)] px-2 py-0.5 text-[10px] text-[var(--muted-foreground)]">
              {tokens.length}
            </span>
          )}
        </div>
        <button
          onClick={() => setShowMint(true)}
          className="inline-flex items-center gap-1.5 rounded-md border border-[var(--border)] bg-[var(--card)] px-3 py-1 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)] transition-colors"
        >
          <Plus size={12} />
          Generate token
        </button>
      </div>

      <div className="px-5 py-4">
        <p className="mb-3 text-xs text-[var(--muted-foreground)]">
          Use these tokens with{' '}
          <code className="rounded bg-[var(--background)] px-1 py-0.5 font-mono text-[11px] text-[var(--foreground)]">
            codex --remote wss://codex-app.&lt;host&gt;:443 --remote-auth-token-env &lt;ENV_VAR&gt;
          </code>
        </p>

        {error && (
          <div className="mb-3 rounded-md border border-[var(--destructive)]/30 bg-[var(--destructive)]/10 px-3 py-2 text-xs text-[var(--destructive)]">
            {error}
          </div>
        )}

        {loading ? (
          <div className="text-xs text-[var(--muted-foreground)]">Loading…</div>
        ) : tokens.length === 0 ? (
          <div className="rounded-md border border-dashed border-[var(--border)] py-8 text-center text-xs italic text-[var(--muted-foreground)]">
            No tokens yet — generate one to enable a remote codex CLI.
          </div>
        ) : (
          <div className="overflow-hidden rounded-md border border-[var(--border)]">
            <table className="w-full table-fixed border-collapse text-xs">
              <thead className="bg-[var(--secondary)] text-[var(--muted-foreground)]">
                <tr>
                  <th className="px-3 py-2 text-left font-medium">Name</th>
                  <th className="w-32 px-3 py-2 text-left font-medium">Created</th>
                  <th className="w-32 px-3 py-2 text-left font-medium">Expires</th>
                  <th className="w-44 px-3 py-2 text-left font-medium">Last used</th>
                  <th className="w-16 px-3 py-2 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody>
                {tokens.map((t, i) => (
                  <tr
                    key={t.id}
                    className={`border-t border-[var(--border)] ${i % 2 === 1 ? 'bg-[var(--background)]/40' : ''}`}
                  >
                    <td className="truncate px-3 py-2 font-medium text-[var(--foreground)]">{t.name}</td>
                    <td className="px-3 py-2 text-[11px] text-[var(--muted-foreground)]">
                      {new Date(t.created_at).toLocaleDateString()}
                    </td>
                    <td className="px-3 py-2 text-[11px] text-[var(--muted-foreground)]">
                      {new Date(t.expires_at).toLocaleDateString()}
                    </td>
                    <td className="px-3 py-2 text-[11px] text-[var(--muted-foreground)]">
                      {t.last_used_at
                        ? new Date(t.last_used_at).toLocaleString()
                        : <span className="italic opacity-60">never</span>}
                    </td>
                    <td className="px-3 py-2 text-right">
                      <button
                        onClick={() => setRevokeTarget(t)}
                        className="rounded p-1 text-[var(--muted-foreground)] hover:bg-[var(--secondary)] hover:text-[var(--destructive)]"
                        aria-label="Revoke token"
                        title="Revoke token"
                      >
                        <Trash2 size={14} />
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {showMint && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50" onClick={() => setShowMint(false)}>
          <div
            className="w-full max-w-sm rounded-lg border border-[var(--border)] bg-[var(--card)] p-6 shadow-xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="mb-4 flex items-center justify-between">
              <h2 className="text-lg font-semibold text-[var(--foreground)]">Generate codex token</h2>
              <button
                onClick={() => setShowMint(false)}
                className="rounded p-1 hover:bg-[var(--secondary)]"
              >
                <X size={16} />
              </button>
            </div>
            <form onSubmit={(e) => { e.preventDefault(); void onMint() }} className="flex flex-col gap-4">
              <div>
                <label className="mb-1 block text-sm font-medium text-[var(--foreground)]">Name</label>
                <input
                  autoFocus
                  type="text"
                  value={newName}
                  onChange={(e) => setNewName(e.target.value)}
                  placeholder="my mac"
                  className="w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] outline-none focus:border-[var(--primary)]"
                />
              </div>
              <div>
                <label className="mb-1 block text-sm font-medium text-[var(--foreground)]">Expires in</label>
                <select
                  value={newTTL}
                  onChange={(e) => setNewTTL(parseInt(e.target.value, 10))}
                  className="w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] outline-none focus:border-[var(--primary)]"
                >
                  {TTL_OPTIONS.map(d => <option key={d} value={d}>{d} day{d === 1 ? '' : 's'}</option>)}
                </select>
              </div>
              <div className="flex justify-end gap-2">
                <button
                  type="button"
                  onClick={() => setShowMint(false)}
                  className="rounded-md border border-[var(--border)] px-4 py-2 text-sm font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]"
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  disabled={!newName.trim()}
                  className="rounded-md bg-[var(--primary)] px-4 py-2 text-sm font-medium text-[var(--primary-foreground)] hover:opacity-90 disabled:opacity-50"
                >
                  Generate
                </button>
              </div>
            </form>
          </div>
        </div>
      )}

      {generated && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50">
          <div className="w-full max-w-xl rounded-lg border border-[var(--border)] bg-[var(--card)] p-6 shadow-xl">
            <div className="mb-3 flex items-center justify-between">
              <h2 className="text-lg font-semibold text-[var(--foreground)]">Token generated</h2>
              <button
                onClick={() => setGenerated(null)}
                className="rounded p-1 hover:bg-[var(--secondary)]"
              >
                <X size={16} />
              </button>
            </div>
            <p className="mb-3 text-sm text-[var(--muted-foreground)]">
              Copy it now — you won't see it again.
            </p>
            <div className="mb-4 flex items-center gap-2">
              <code className="flex-1 break-all rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 font-mono text-xs text-[var(--foreground)]">
                {generated.token}
              </code>
              <button
                onClick={copyToken}
                className="rounded-md border border-[var(--border)] p-2 text-[var(--foreground)] hover:bg-[var(--secondary)]"
                aria-label="Copy token"
                title="Copy"
              >
                {copied ? <Check size={14} /> : <Copy size={14} />}
              </button>
            </div>
            <pre className="mb-4 overflow-x-auto rounded-md border border-[var(--border)] bg-[var(--background)] p-3 text-[11px] text-[var(--foreground)]">{`export AGENTSERVER_TOKEN='${generated.token}'
codex --remote wss://codex-app.${typeof window !== 'undefined' ? window.location.host : '<host>'}:443 \\
      --remote-auth-token-env AGENTSERVER_TOKEN`}</pre>
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

      {revokeTarget && (
        <ConfirmModal
          title="Revoke codex token"
          message={`Revoke "${revokeTarget.name}"? Active codex --remote sessions using it will be cut at next reconnect.`}
          confirmLabel="Revoke"
          destructive
          onConfirm={() => onRevoke(revokeTarget.id)}
          onCancel={() => setRevokeTarget(null)}
        />
      )}
    </div>
  )
}
