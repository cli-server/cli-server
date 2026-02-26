import { useState, useEffect, type FormEvent } from 'react'
import { login, register, getOIDCProviders } from '../lib/api'

interface LoginProps {
  onSuccess: () => void
}

const providerLabels: Record<string, string> = {
  github: 'Sign in with GitHub',
  oidc: 'Sign in with SSO',
}

export function Login({ onSuccess }: LoginProps) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [isRegister, setIsRegister] = useState(false)
  const [oidcProviders, setOidcProviders] = useState<string[]>([])

  useEffect(() => {
    getOIDCProviders().then(setOidcProviders)
  }, [])

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault()
    setError('')
    setLoading(true)
    try {
      if (isRegister) {
        const ok = await register(username, password)
        if (ok) {
          // Auto-login after registration.
          const loginOk = await login(username, password)
          if (loginOk) {
            onSuccess()
          } else {
            setError('Registration succeeded but login failed')
          }
        } else {
          setError('Registration failed (username may be taken)')
        }
      } else {
        const ok = await login(username, password)
        if (ok) {
          onSuccess()
        } else {
          setError('Invalid credentials')
        }
      }
    } catch {
      setError('Connection failed')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center">
      <div className="w-full max-w-sm rounded-lg border border-[var(--border)] bg-[var(--card)] p-6 shadow-lg">
        <h1 className="mb-6 text-center text-xl font-semibold text-[var(--card-foreground)]">
          cli-server
        </h1>
        <form onSubmit={handleSubmit} className="space-y-4">
          <div>
            <input
              type="text"
              placeholder="Username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoFocus
              className="w-full rounded-md border border-[var(--input)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] placeholder-[var(--muted-foreground)] outline-none focus:ring-2 focus:ring-[var(--ring)]"
            />
          </div>
          <div>
            <input
              type="password"
              placeholder="Password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="w-full rounded-md border border-[var(--input)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] placeholder-[var(--muted-foreground)] outline-none focus:ring-2 focus:ring-[var(--ring)]"
            />
          </div>
          {error && (
            <p className="text-sm text-[var(--destructive)]">{error}</p>
          )}
          <button
            type="submit"
            disabled={loading || !username || !password}
            className="w-full rounded-md bg-[var(--primary)] px-4 py-2 text-sm font-medium text-[var(--primary-foreground)] hover:opacity-90 disabled:opacity-50"
          >
            {loading
              ? isRegister
                ? 'Creating account...'
                : 'Signing in...'
              : isRegister
                ? 'Create account'
                : 'Sign in'}
          </button>
        </form>
        {oidcProviders.length > 0 && (
          <div className="mt-4">
            <div className="relative flex items-center justify-center">
              <div className="absolute inset-0 flex items-center">
                <div className="w-full border-t border-[var(--border)]" />
              </div>
              <span className="relative bg-[var(--card)] px-2 text-xs text-[var(--muted-foreground)]">or</span>
            </div>
            <div className="mt-4 space-y-2">
              {oidcProviders.map((provider) => (
                <a
                  key={provider}
                  href={`/api/auth/oidc/${provider}/login`}
                  className="flex w-full items-center justify-center rounded-md border border-[var(--input)] bg-[var(--background)] px-4 py-2 text-sm font-medium text-[var(--foreground)] hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)]"
                >
                  {providerLabels[provider] || `Sign in with ${provider}`}
                </a>
              ))}
            </div>
          </div>
        )}
        <p className="mt-4 text-center text-sm text-[var(--muted-foreground)]">
          {isRegister ? (
            <>
              Already have an account?{' '}
              <button
                onClick={() => { setIsRegister(false); setError('') }}
                className="text-[var(--primary)] hover:underline"
              >
                Sign in
              </button>
            </>
          ) : (
            <>
              No account?{' '}
              <button
                onClick={() => { setIsRegister(true); setError('') }}
                className="text-[var(--primary)] hover:underline"
              >
                Create one
              </button>
            </>
          )}
        </p>
      </div>
    </div>
  )
}
