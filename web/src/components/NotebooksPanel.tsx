import { useState } from 'react'
import { Loader2, AlertCircle, ExternalLink, BookOpen } from 'lucide-react'
import { createNotebookSession } from '../lib/api'

interface Props {
  workspaceId: string
}

export default function NotebooksPanel({ workspaceId }: Props) {
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const launch = async () => {
    setLoading(true)
    setError(null)
    try {
      const s = await createNotebookSession(workspaceId)
      const url = `${s.url}?token=${encodeURIComponent(s.token)}`
      window.open(url, '_blank', 'noopener,noreferrer')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="rounded-lg border border-[var(--border)] bg-[var(--card)] p-5">
      <div className="flex items-start gap-4">
        <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-md bg-[var(--secondary)] text-[var(--foreground)]">
          <BookOpen size={18} />
        </div>
        <div className="min-w-0 flex-1">
          <div className="text-sm font-medium text-[var(--foreground)]">Jupyter Notebook</div>
          <p className="mt-1 text-xs text-[var(--muted-foreground)]">
            Launch a per-workspace JupyterLab session in a new tab. Sessions reuse the workspace volume and idle out after inactivity.
          </p>
          <div className="mt-3">
            <button
              onClick={launch}
              disabled={loading}
              className="inline-flex items-center gap-1.5 rounded-md border border-[var(--border)] bg-[var(--card)] px-3 py-1.5 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)] disabled:opacity-50 transition-colors"
            >
              {loading ? <Loader2 size={12} className="animate-spin" /> : <ExternalLink size={12} />}
              {loading ? 'Starting…' : 'Open Notebook'}
            </button>
          </div>
          {error && (
            <div className="mt-3 flex items-start gap-2 rounded-md border border-red-500/30 bg-red-500/10 p-2.5">
              <AlertCircle size={14} className="mt-0.5 shrink-0 text-red-400" />
              <div className="whitespace-pre-wrap text-xs text-red-400">{error}</div>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
