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
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [isRegister, setIsRegister] = useState(false)
  const [oidcProviders, setOidcProviders] = useState<string[]>([])
  const [passwordAuth, setPasswordAuth] = useState(true)
  const [providersLoaded, setProvidersLoaded] = useState(false)

  useEffect(() => {
    getOIDCProviders().then((data) => {
      setOidcProviders(data.providers)
      setPasswordAuth(data.passwordAuth)
      setProvidersLoaded(true)
    })
  }, [])

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault()
    setError('')
    setLoading(true)
    try {
      if (isRegister) {
        const ok = await register(username, email, password)
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
          agentserver
        </h1>
        {providersLoaded && !passwordAuth && oidcProviders.length === 0 && (
          <p className="text-center text-sm text-[var(--destructive)]">
            No authentication methods are configured. Contact your administrator.
          </p>
        )}
        {passwordAuth && (
          <>
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
              {isRegister && (
                <div>
                  <input
                    type="email"
                    placeholder="Email"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    className="w-full rounded-md border border-[var(--input)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] placeholder-[var(--muted-foreground)] outline-none focus:ring-2 focus:ring-[var(--ring)]"
                  />
                </div>
              )}
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
                disabled={loading || !username || !password || (isRegister && !email)}
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
          </>
        )}
        {oidcProviders.length > 0 && (
          <div className={passwordAuth ? "mt-4" : ""}>
            {passwordAuth && (
              <div className="relative flex items-center justify-center">
                <div className="absolute inset-0 flex items-center">
                  <div className="w-full border-t border-[var(--border)]" />
                </div>
                <span className="relative bg-[var(--card)] px-2 text-xs text-[var(--muted-foreground)]">or</span>
              </div>
            )}
            <div className={passwordAuth ? "mt-4 space-y-2" : "space-y-2"}>
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
        {passwordAuth && (
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
        )}
      </div>
      <div className="mt-6 flex items-center justify-center gap-3 text-xs text-[var(--muted-foreground)]">
        <a href="https://agentserver.dev" target="_blank" rel="noopener noreferrer" className="hover:text-[var(--foreground)]">
          agentserver.dev
        </a>
        <span>·</span>
        <a href="https://github.com/agentserver/agentserver" target="_blank" rel="noopener noreferrer" className="flex items-center gap-1 hover:text-[var(--foreground)]">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.37 0 0 5.37 0 12c0 5.31 3.435 9.795 8.205 11.385.6.105.825-.255.825-.57 0-.285-.015-1.23-.015-2.235-3.015.555-3.795-.735-4.035-1.41-.135-.345-.72-1.41-1.23-1.695-.42-.225-1.02-.78-.015-.795.945-.015 1.62.87 1.845 1.23 1.08 1.815 2.805 1.305 3.495.99.105-.78.42-1.305.765-1.605-2.67-.3-5.46-1.335-5.46-5.925 0-1.305.465-2.385 1.23-3.225-.12-.3-.54-1.53.12-3.18 0 0 1.005-.315 3.3 1.23.96-.27 1.98-.405 3-.405s2.04.135 3 .405c2.295-1.56 3.3-1.23 3.3-1.23.66 1.65.24 2.88.12 3.18.765.84 1.23 1.905 1.23 3.225 0 4.605-2.805 5.625-5.475 5.925.435.375.81 1.095.81 2.22 0 1.605-.015 2.895-.015 3.3 0 .315.225.69.825.57A12.02 12.02 0 0 0 24 12c0-6.63-5.37-12-12-12z"/></svg>
          GitHub
        </a>
      </div>
    </div>
  )
}
