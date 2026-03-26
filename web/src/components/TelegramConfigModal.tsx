import { useState } from 'react'
import { X, Bot, Loader2, CheckCircle2 } from 'lucide-react'
import { telegramConfigure } from '../lib/api'

interface TelegramConfigModalProps {
  sandboxId: string
  onClose: () => void
  onConnected: () => void
}

export function TelegramConfigModal({ sandboxId, onClose, onConnected }: TelegramConfigModalProps) {
  const [botToken, setBotToken] = useState('')
  const [status, setStatus] = useState<'idle' | 'loading' | 'connected' | 'error'>('idle')
  const [error, setError] = useState('')
  const [botName, setBotName] = useState('')

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!botToken.trim()) return

    setStatus('loading')
    setError('')
    try {
      const result = await telegramConfigure(sandboxId, botToken.trim())
      setBotName(result.bot_name || result.bot_id)
      setStatus('connected')
      onConnected()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to configure bot')
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
          <Bot size={20} className="text-blue-400" />
          <h2 className="text-lg font-semibold text-[var(--foreground)]">Configure Telegram Bot</h2>
        </div>

        {status === 'connected' ? (
          <div className="flex flex-col items-center gap-3 py-6">
            <CheckCircle2 size={48} className="text-green-400" />
            <p className="text-sm text-[var(--foreground)]">
              Bot <span className="font-mono font-medium">@{botName}</span> connected successfully
            </p>
          </div>
        ) : (
          <form onSubmit={handleSubmit}>
            <p className="text-xs text-[var(--muted-foreground)] mb-3">
              Enter your Telegram Bot Token from <span className="font-mono">@BotFather</span>.
            </p>
            <input
              type="password"
              value={botToken}
              onChange={(e) => setBotToken(e.target.value)}
              placeholder="123456:ABC-DEF..."
              autoFocus
              disabled={status === 'loading'}
              className="w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm font-mono text-[var(--foreground)] placeholder:text-[var(--muted-foreground)] focus:outline-none focus:ring-1 focus:ring-[var(--primary)] disabled:opacity-50"
            />
            {error && (
              <p className="mt-2 text-xs text-red-400">{error}</p>
            )}
            <button
              type="submit"
              disabled={!botToken.trim() || status === 'loading'}
              className="mt-4 w-full inline-flex items-center justify-center gap-2 rounded-md bg-blue-600 px-4 py-2 text-sm font-medium text-white hover:bg-blue-500 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
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
