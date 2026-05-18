import { useEffect, useRef, useState } from 'react'
import { Loader2, AlertCircle } from 'lucide-react'
import { createNotebookSession, type NotebookSession } from '../lib/api'

interface Props {
  workspaceId: string
}

// Refresh the session this many seconds before the JWT actually expires.
// 10-min TTL minus 1-min safety = refresh every 9 minutes.
const REFRESH_BUFFER_SECONDS = 60

export default function NotebooksPanel({ workspaceId }: Props) {
  const [session, setSession] = useState<NotebookSession | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const timerRef = useRef<number | null>(null)

  useEffect(() => {
    let cancelled = false

    const scheduleRefresh = (expiresAt: number) => {
      if (timerRef.current !== null) {
        window.clearTimeout(timerRef.current)
      }
      const now = Math.floor(Date.now() / 1000)
      const delayMs = Math.max(1000, (expiresAt - now - REFRESH_BUFFER_SECONDS) * 1000)
      timerRef.current = window.setTimeout(() => void fetchSession(), delayMs)
    }

    const fetchSession = async () => {
      try {
        setLoading(true)
        const s = await createNotebookSession(workspaceId)
        if (cancelled) return
        setSession(s)
        setError(null)
        scheduleRefresh(s.expires_at)
      } catch (e) {
        if (cancelled) return
        setError(e instanceof Error ? e.message : String(e))
        setSession(null)
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    void fetchSession()

    return () => {
      cancelled = true
      if (timerRef.current !== null) {
        window.clearTimeout(timerRef.current)
        timerRef.current = null
      }
    }
  }, [workspaceId])

  if (loading && !session) {
    return (
      <div className="flex h-96 items-center justify-center text-sm text-[var(--muted-foreground)]">
        <Loader2 size={16} className="mr-2 animate-spin" />
        Starting notebook environment…
      </div>
    )
  }

  if (error) {
    return (
      <div className="flex items-start gap-3 rounded-lg border border-red-500/30 bg-red-500/10 p-4">
        <AlertCircle size={18} className="mt-0.5 shrink-0 text-red-400" />
        <div>
          <div className="text-sm font-medium text-[var(--foreground)]">Could not start notebook</div>
          <div className="mt-1 whitespace-pre-wrap text-xs text-red-400">{error}</div>
        </div>
      </div>
    )
  }

  if (!session) return null

  const iframeSrc = `${session.url}?token=${encodeURIComponent(session.token)}`

  return (
    <div className="h-[80vh] w-full overflow-hidden rounded-lg border border-[var(--border)] bg-[var(--card)]">
      <iframe
        key={session.token}
        src={iframeSrc}
        sandbox="allow-scripts allow-same-origin allow-forms allow-popups"
        className="h-full w-full"
        title="Notebook"
      />
    </div>
  )
}
