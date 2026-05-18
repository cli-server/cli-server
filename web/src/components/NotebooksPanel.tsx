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
      <div className="flex items-center justify-center h-96 text-gray-500">
        <Loader2 className="w-5 h-5 mr-2 animate-spin" />
        Starting notebook environment…
      </div>
    )
  }

  if (error) {
    return (
      <div className="flex items-start gap-3 p-4 border border-red-300 bg-red-50 rounded">
        <AlertCircle className="w-5 h-5 text-red-600 mt-0.5 shrink-0" />
        <div>
          <div className="font-medium text-red-900">Could not start notebook</div>
          <div className="text-sm text-red-800 mt-1 whitespace-pre-wrap">{error}</div>
        </div>
      </div>
    )
  }

  if (!session) return null

  const iframeSrc = `${session.url}?token=${encodeURIComponent(session.token)}`

  return (
    <div className="h-[80vh] w-full">
      <iframe
        key={session.token}
        src={iframeSrc}
        sandbox="allow-scripts allow-same-origin allow-forms allow-popups"
        className="w-full h-full border rounded"
        title="Notebook"
      />
    </div>
  )
}
