import { useState, useEffect, useCallback } from 'react'
import { Plus, Trash2, Copy, Check, X } from 'lucide-react'
import {
  type CodexToken, type MintCodexTokenResponse,
  listCodexTokens, mintCodexToken, revokeCodexToken,
} from '../lib/api'

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
    try {
      const resp = await mintCodexToken({
        workspace_id: workspaceId,
        name: newName,
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
    if (!confirm('Revoke this token? Any active codex --remote sessions using it will be cut at next reconnect.')) return
    try {
      await revokeCodexToken(id)
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
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-lg font-semibold">Codex Remote Access</h3>
          <p className="text-sm text-gray-500">
            Use these tokens with{' '}
            <code className="px-1 bg-gray-100 rounded">
              codex --remote wss://&lt;host&gt;/codex-app/ws --remote-auth-token-env &lt;ENV_VAR&gt;
            </code>
          </p>
        </div>
        <button
          onClick={() => setShowMint(true)}
          className="flex items-center gap-1 px-3 py-1.5 bg-blue-600 text-white rounded hover:bg-blue-700"
        >
          <Plus size={16} />
          Generate token
        </button>
      </div>

      {error && <div className="p-2 bg-red-50 text-red-700 text-sm rounded">{error}</div>}

      {loading ? (
        <div className="text-gray-500">Loading…</div>
      ) : tokens.length === 0 ? (
        <div className="text-gray-500 italic">No tokens yet.</div>
      ) : (
        <table className="w-full text-sm">
          <thead className="text-left text-gray-500 border-b">
            <tr>
              <th className="py-2">Name</th>
              <th className="py-2">Created</th>
              <th className="py-2">Expires</th>
              <th className="py-2">Last used</th>
              <th className="py-2"></th>
            </tr>
          </thead>
          <tbody>
            {tokens.map(t => (
              <tr key={t.id} className="border-b last:border-0">
                <td className="py-2">{t.name}</td>
                <td className="py-2">{new Date(t.created_at).toLocaleDateString()}</td>
                <td className="py-2">{new Date(t.expires_at).toLocaleDateString()}</td>
                <td className="py-2">
                  {t.last_used_at ? new Date(t.last_used_at).toLocaleString() : <span className="text-gray-400">never</span>}
                </td>
                <td className="py-2 text-right">
                  <button
                    onClick={() => onRevoke(t.id)}
                    className="text-red-600 hover:text-red-800"
                    aria-label="Revoke token"
                  >
                    <Trash2 size={16} />
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {showMint && (
        <div className="fixed inset-0 z-50 bg-black/30 flex items-center justify-center">
          <div className="bg-white rounded shadow-lg p-6 w-96 space-y-3">
            <div className="flex items-center justify-between">
              <h4 className="font-semibold">Generate codex token</h4>
              <button onClick={() => setShowMint(false)}><X size={16} /></button>
            </div>
            <label className="block text-sm">
              Name
              <input
                type="text"
                value={newName}
                onChange={e => setNewName(e.target.value)}
                className="mt-1 w-full border rounded px-2 py-1"
                placeholder="my mac"
              />
            </label>
            <label className="block text-sm">
              TTL (days)
              <select
                value={newTTL}
                onChange={e => setNewTTL(parseInt(e.target.value, 10))}
                className="mt-1 w-full border rounded px-2 py-1"
              >
                {TTL_OPTIONS.map(d => <option key={d} value={d}>{d}</option>)}
              </select>
            </label>
            <div className="flex justify-end gap-2 pt-2">
              <button onClick={() => setShowMint(false)} className="px-3 py-1 border rounded">Cancel</button>
              <button
                onClick={onMint}
                disabled={!newName.trim()}
                className="px-3 py-1 bg-blue-600 text-white rounded disabled:opacity-50"
              >
                Generate
              </button>
            </div>
          </div>
        </div>
      )}

      {generated && (
        <div className="fixed inset-0 z-50 bg-black/30 flex items-center justify-center">
          <div className="bg-white rounded shadow-lg p-6 w-[36rem] space-y-3">
            <h4 className="font-semibold text-green-700">&#10003; Token generated</h4>
            <p className="text-sm text-gray-700">
              Copy it now — you won't see it again.
            </p>
            <div className="flex items-center gap-2">
              <code className="flex-1 px-2 py-2 bg-gray-100 rounded font-mono text-xs break-all">
                {generated.token}
              </code>
              <button
                onClick={copyToken}
                className="p-2 border rounded hover:bg-gray-50"
                aria-label="Copy token"
              >
                {copied ? <Check size={16} /> : <Copy size={16} />}
              </button>
            </div>
            <pre className="text-xs bg-gray-50 p-2 rounded overflow-x-auto">{`export AGENTSERVER_TOKEN='${generated.token}'
codex --remote wss://<host>/codex-app/ws \\
      --remote-auth-token-env AGENTSERVER_TOKEN`}</pre>
            <div className="flex justify-end pt-2">
              <button
                onClick={() => setGenerated(null)}
                className="px-3 py-1 bg-blue-600 text-white rounded"
              >
                I've saved it
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
