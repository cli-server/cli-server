import { useState } from 'react'
import { X, Loader2, CheckCircle2, Hash } from 'lucide-react'
import { matrixConfigure } from '../lib/api'

interface MatrixConfigModalProps {
  sandboxId: string
  onClose: () => void
  onConnected: () => void
}

export function MatrixConfigModal({ sandboxId, onClose, onConnected }: MatrixConfigModalProps) {
  const [homeserverUrl, setHomeserverUrl] = useState('')
  const [accessToken, setAccessToken] = useState('')
  const [status, setStatus] = useState<'idle' | 'loading' | 'connected' | 'error'>('idle')
  const [error, setError] = useState('')
  const [userId, setUserId] = useState('')

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!homeserverUrl.trim() || !accessToken.trim()) return

    setStatus('loading')
    setError('')
    try {
      const result = await matrixConfigure(sandboxId, homeserverUrl.trim(), accessToken.trim())
      setUserId(result.user_id || result.bot_id)
      setStatus('connected')
      onConnected()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to configure Matrix')
      setStatus('error')
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm">
      <div className="relative w-full max-w-md rounded-xl border border-[var(--border)] bg-[var(--card)] p-6 shadow-2xl">
        <button
          onClick={onClose}
          className="absolute right-4 top-4 text-[var(--muted-foreground)] hover:text-[var(--foreground)]"
        >
          <X size={16} />
        </button>

        <div className="flex items-center gap-2 mb-4">
          <Hash size={20} className="text-purple-400" />
          <h2 className="text-lg font-semibold text-[var(--foreground)]">Configure Matrix Bot</h2>
        </div>

        {status === 'connected' ? (
          <div className="flex flex-col items-center gap-3 py-6">
            <CheckCircle2 size={48} className="text-green-400" />
            <p className="text-sm text-[var(--foreground)]">
              Connected as <span className="font-mono font-medium">{userId}</span>
            </p>
          </div>
        ) : (
          <form onSubmit={handleSubmit}>
            <p className="text-xs text-[var(--muted-foreground)] mb-3">
              Enter your Matrix homeserver URL and bot access token.
            </p>
            <input
              type="url"
              value={homeserverUrl}
              onChange={(e) => setHomeserverUrl(e.target.value)}
              placeholder="https://matrix.example.com"
              autoFocus
              disabled={status === 'loading'}
              className="w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm font-mono text-[var(--foreground)] placeholder:text-[var(--muted-foreground)] focus:outline-none focus:ring-1 focus:ring-[var(--primary)] disabled:opacity-50"
            />
            <input
              type="password"
              value={accessToken}
              onChange={(e) => setAccessToken(e.target.value)}
              placeholder="syt_..."
              disabled={status === 'loading'}
              className="mt-2 w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm font-mono text-[var(--foreground)] placeholder:text-[var(--muted-foreground)] focus:outline-none focus:ring-1 focus:ring-[var(--primary)] disabled:opacity-50"
            />
            {error && (
              <p className="mt-2 text-xs text-red-400">{error}</p>
            )}
            <button
              type="submit"
              disabled={!homeserverUrl.trim() || !accessToken.trim() || status === 'loading'}
              className="mt-4 w-full inline-flex items-center justify-center gap-2 rounded-md bg-purple-600 px-4 py-2 text-sm font-medium text-white hover:bg-purple-500 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
            >
              {status === 'loading' ? (
                <>
                  <Loader2 size={14} className="animate-spin" />
                  Validating...
                </>
              ) : (
                'Connect Bot'
              )}
            </button>
          </form>
        )}
      </div>
    </div>
  )
}
