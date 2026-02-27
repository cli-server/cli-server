export type SessionStatus = 'creating' | 'running' | 'pausing' | 'paused' | 'resuming'

export interface Session {
  id: string
  name: string
  status: SessionStatus
  opencodeUrl?: string
  createdAt: string
  lastActivityAt: string | null
  pausedAt: string | null
}

export async function login(username: string, password: string): Promise<boolean> {
  const res = await fetch('/api/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
  return res.ok
}

export async function register(username: string, password: string): Promise<boolean> {
  const res = await fetch('/api/auth/register', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
  return res.ok
}

export async function checkAuth(): Promise<boolean> {
  const res = await fetch('/api/auth/check')
  return res.ok
}

export async function getOIDCProviders(): Promise<string[]> {
  const res = await fetch('/api/auth/oidc/providers')
  if (!res.ok) return []
  const data = await res.json()
  return data.providers || []
}

export async function getMe(): Promise<{ id: string; username: string; email?: string | null }> {
  const res = await fetch('/api/auth/me')
  if (!res.ok) throw new Error('Failed to get user info')
  return res.json()
}

export async function logout(): Promise<void> {
  await fetch('/api/auth/logout', { method: 'POST' })
}

export async function listSessions(): Promise<Session[]> {
  const res = await fetch('/api/sessions')
  if (!res.ok) throw new Error('Failed to list sessions')
  return res.json()
}

export async function getSession(id: string): Promise<Session> {
  const res = await fetch(`/api/sessions/${id}`)
  if (!res.ok) throw new Error('Failed to get session')
  return res.json()
}

export async function createSession(name?: string): Promise<Session> {
  const res = await fetch('/api/sessions', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: name || 'New Session' }),
  })
  if (!res.ok) throw new Error('Failed to create session')
  return res.json()
}

export async function deleteSession(id: string): Promise<void> {
  const res = await fetch(`/api/sessions/${id}`, { method: 'DELETE' })
  if (!res.ok) throw new Error('Failed to delete session')
}

export async function pauseSession(id: string): Promise<void> {
  const res = await fetch(`/api/sessions/${id}/pause`, { method: 'POST' })
  if (!res.ok) throw new Error('Failed to pause session')
}

export async function resumeSession(id: string): Promise<void> {
  const res = await fetch(`/api/sessions/${id}/resume`, { method: 'POST' })
  if (!res.ok) throw new Error('Failed to resume session')
}
