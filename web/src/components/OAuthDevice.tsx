import { useState } from 'react'

interface OAuthDeviceProps {
  challenge: string
  userCode: string
}

export function OAuthDevice({ challenge, userCode }: OAuthDeviceProps) {
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')

  const handleConfirm = async () => {
    setSubmitting(true)
    setError('')
    try {
      const res = await fetch('/api/oauth2/device/accept', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({ device_challenge: challenge, user_code: userCode }),
      })
      if (!res.ok) {
        const text = await res.text()
        throw new Error(text || 'Failed to confirm')
      }
      const { redirect_to } = await res.json()
      window.location.href = redirect_to
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to confirm device. Please try again.')
      setSubmitting(false)
    }
  }

  const handleDeny = () => {
    window.close()
  }

  return (
    <div className="min-h-screen flex items-center justify-center">
      <div className="w-full max-w-md border border-[var(--border)] rounded-lg p-6 space-y-6">
        <div className="text-center">
          <h2 className="text-lg font-semibold">Confirm Device Login</h2>
          <p className="text-sm text-[var(--muted-foreground)] mt-1">
            A device is requesting access to your account
          </p>
        </div>

        <div className="text-center">
          <p className="text-sm text-[var(--muted-foreground)]">Confirm this code matches what you see on your device:</p>
          <div className="mt-3 inline-block rounded-md bg-[var(--secondary)] px-6 py-3">
            <code className="text-2xl font-mono font-bold tracking-widest text-[var(--foreground)]">
              {userCode}
            </code>
          </div>
        </div>

        {error && (
          <div className="text-sm text-red-500 text-center">{error}</div>
        )}

        <div className="flex gap-3 justify-end">
          <button
            onClick={handleDeny}
            disabled={submitting}
            className="px-4 py-2 text-sm border border-[var(--border)] rounded-md hover:bg-[var(--muted)]"
          >
            Deny
          </button>
          <button
            onClick={handleConfirm}
            disabled={submitting}
            className="px-4 py-2 text-sm bg-[var(--primary)] text-[var(--primary-foreground)] rounded-md hover:opacity-90 disabled:opacity-50"
          >
            {submitting ? 'Confirming...' : 'Confirm'}
          </button>
        </div>
      </div>
    </div>
  )
}
