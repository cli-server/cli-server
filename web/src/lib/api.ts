export interface Session {
  id: string
  name: string
  createdAt: string
}

export async function login(password: string): Promise<boolean> {
  const res = await fetch('/api/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password }),
  })
  return res.ok
}

export async function checkAuth(): Promise<boolean> {
  const res = await fetch('/api/auth/check')
  return res.ok
}

export async function listSessions(): Promise<Session[]> {
  const res = await fetch('/api/sessions')
  if (!res.ok) throw new Error('Failed to list sessions')
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
