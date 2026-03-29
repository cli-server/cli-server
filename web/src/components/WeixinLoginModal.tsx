import { useState, useEffect, useRef, useCallback } from 'react'
import { X, Loader2, CheckCircle2, AlertCircle, SmartphoneNfc } from 'lucide-react'
import { QRCodeSVG } from 'qrcode.react'
import { weixinQRStart, weixinQRWait, workspaceWeixinQRStart, workspaceWeixinQRWait } from '../lib/api'

interface WeixinLoginModalProps {
  sandboxId?: string
  workspaceId?: string
  onClose: () => void
  onConnected?: () => void
}

type Phase = 'loading' | 'qr' | 'scanned' | 'connected' | 'error'

export function WeixinLoginModal({ sandboxId, workspaceId, onClose, onConnected }: WeixinLoginModalProps) {
  const [phase, setPhase] = useState<Phase>('loading')
  const [qrUrl, setQrUrl] = useState('')
  const [message, setMessage] = useState('')
  const [error, setError] = useState('')
  const cancelledRef = useRef(false)

  const startLogin = useCallback(async () => {
    setPhase('loading')
    setError('')
    try {
      const res = workspaceId
        ? await workspaceWeixinQRStart(workspaceId)
        : await weixinQRStart(sandboxId!)
      if (cancelledRef.current) return
      setQrUrl(res.qrcode_url)
      setMessage(res.message)
      setPhase('qr')
    } catch (err) {
      if (cancelledRef.current) return
      setError(String(err))
      setPhase('error')
    }
  }, [sandboxId, workspaceId])

  // Start QR generation on mount
  useEffect(() => {
    cancelledRef.current = false
    startLogin()
    return () => { cancelledRef.current = true }
  }, [startLogin])

  // Poll for scan status once QR is shown
  const isPolling = phase === 'qr' || phase === 'scanned'
  useEffect(() => {
    if (!isPolling) return
    let active = true

    const poll = async () => {
      while (active) {
        try {
          const res = workspaceId
            ? await workspaceWeixinQRWait(workspaceId)
            : await weixinQRWait(sandboxId!)
          if (!active) return

          if (res.connected) {
            setPhase('connected')
            setMessage(res.message)
            onConnected?.()
            return
          }

          if (res.status === 'scaned') {
            setPhase('scanned')
            setMessage(res.message)
          } else if (res.status === 'expired' && res.qrcode_url) {
            setQrUrl(res.qrcode_url)
            setPhase('qr')
            setMessage(res.message)
          }
          // "wait" → continue polling
        } catch {
          if (!active) return
          // Transient error, keep polling
          await new Promise(r => setTimeout(r, 2000))
        }
      }
    }

    poll()
    return () => { active = false }
  }, [isPolling, sandboxId, workspaceId])

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm" onClick={onClose}>
      <div
        className="relative w-full max-w-sm rounded-lg border border-[var(--border)] bg-[var(--card)] p-6 shadow-xl"
        onClick={e => e.stopPropagation()}
      >
        <button
          onClick={onClose}
          className="absolute right-3 top-3 rounded p-1 text-[var(--muted-foreground)] hover:bg-[var(--secondary)] transition-colors"
        >
          <X size={16} />
        </button>

        <h3 className="mb-4 text-base font-semibold text-[var(--foreground)]">Connect WeChat</h3>

        {phase === 'loading' && (
          <div className="flex flex-col items-center gap-3 py-8">
            <Loader2 size={32} className="animate-spin text-[var(--muted-foreground)]" />
            <p className="text-sm text-[var(--muted-foreground)]">Generating QR code...</p>
          </div>
        )}

        {(phase === 'qr' || phase === 'scanned') && (
          <div className="flex flex-col items-center gap-4">
            <div className="rounded-lg border border-[var(--border)] bg-white p-3">
              <QRCodeSVG value={qrUrl} size={192} />
            </div>
            <div className="flex items-center gap-2 text-sm text-[var(--muted-foreground)]">
              {phase === 'scanned' ? (
                <>
                  <SmartphoneNfc size={16} className="text-green-400" />
                  <span className="text-green-400">{message}</span>
                </>
              ) : (
                <>
                  <Loader2 size={14} className="animate-spin" />
                  <span>{message}</span>
                </>
              )}
            </div>
          </div>
        )}

        {phase === 'connected' && (
          <div className="flex flex-col items-center gap-3 py-8">
            <CheckCircle2 size={40} className="text-green-400" />
            <p className="text-sm font-medium text-green-400">{message}</p>
            <button
              onClick={onClose}
              className="mt-2 rounded-md bg-[var(--primary)] px-4 py-1.5 text-xs font-medium text-[var(--primary-foreground)] hover:opacity-90 transition-opacity"
            >
              Done
            </button>
          </div>
        )}

        {phase === 'error' && (
          <div className="flex flex-col items-center gap-3 py-8">
            <AlertCircle size={40} className="text-red-400" />
            <p className="text-sm text-red-400">{error}</p>
            <button
              onClick={startLogin}
              className="mt-2 rounded-md border border-[var(--border)] bg-[var(--card)] px-4 py-1.5 text-xs font-medium text-[var(--foreground)] hover:bg-[var(--secondary)] transition-colors"
            >
              Retry
            </button>
          </div>
        )}
      </div>
    </div>
  )
}
