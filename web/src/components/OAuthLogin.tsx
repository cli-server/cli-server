import { useState, useEffect } from 'react'
import { Login } from './Login'
import { checkAuth, submitOAuthLogin } from '../lib/api'

const PENDING_LOGIN_CHALLENGE_KEY = 'agentserver_pending_login_challenge'

interface OAuthLoginProps {
  challenge: string
}

export function OAuthLogin({ challenge }: OAuthLoginProps) {
  const [error, setError] = useState('')
  const [autoSubmitting, setAutoSubmitting] = useState(false)

  // Persist challenge in sessionStorage so it survives OIDC redirects.
  sessionStorage.setItem(PENDING_LOGIN_CHALLENGE_KEY, challenge)

  // If the user is already authenticated, auto-submit the challenge.
  useEffect(() => {
    checkAuth().then(async (ok) => {
      if (ok) {
        setAutoSubmitting(true)
        try {
          sessionStorage.removeItem(PENDING_LOGIN_CHALLENGE_KEY)
          const { redirect_to } = await submitOAuthLogin(challenge)
          window.location.href = redirect_to
        } catch {
          setAutoSubmitting(false)
          setError('Failed to complete authorization. Please try again.')
        }
      }
    })
  }, [challenge])

  const handleLoginSuccess = async () => {
    try {
      sessionStorage.removeItem(PENDING_LOGIN_CHALLENGE_KEY)
      const { redirect_to } = await submitOAuthLogin(challenge)
      window.location.href = redirect_to
    } catch {
      setError('Failed to complete OAuth login. Please try again.')
    }
  }

  if (autoSubmitting) {
    return (
      <div className="min-h-screen flex items-center justify-center">
        <span className="text-[var(--muted-foreground)]">Authorizing...</span>
      </div>
    )
  }

  return (
    <div className="min-h-screen flex items-center justify-center">
      <div className="w-full max-w-md space-y-4">
        <div className="text-center mb-6">
          <h2 className="text-lg font-semibold">Sign in to authorize agent</h2>
          <p className="text-sm text-[var(--muted-foreground)]">
            An agent is requesting access to your account
          </p>
        </div>
        {error && (
          <div className="text-sm text-red-500 text-center">{error}</div>
        )}
        <Login onSuccess={handleLoginSuccess} />
      </div>
    </div>
  )
}

// Exported for App.tsx to check after OIDC redirect.
export { PENDING_LOGIN_CHALLENGE_KEY }
