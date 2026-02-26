export type SessionStatus = 'running' | 'pausing' | 'paused' | 'resuming'

export interface Session {
  id: string
  name: string
  status: SessionStatus
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

// Message types
export interface StreamEvent {
  type: string
  text?: string
  thinking?: string
  tool?: ToolPayload
  suggestions?: string[]
}

export interface ToolPayload {
  id: string
  name: string
  title: string
  status: 'started' | 'completed' | 'failed'
  parent_id?: string | null
  input?: Record<string, unknown>
  result?: unknown
  error?: string
  children?: ToolPayload[]
}

export interface Message {
  id: string
  session_id: string
  role: 'user' | 'assistant'
  content_text: string
  content_render: { events: StreamEvent[] }
  stream_status: 'in_progress' | 'completed' | 'failed' | 'interrupted'
  created_at: string
  usage?: { input_tokens?: number; output_tokens?: number }
  total_cost_usd?: number
}

export interface StreamEnvelope {
  sessionId: string
  messageId: string
  streamId: string
  seq: number
  kind: string
  payload: Record<string, unknown>
  ts: string
}

// Message API
export async function getMessages(sessionId: string): Promise<Message[]> {
  const res = await fetch(`/api/sessions/${sessionId}/messages`)
  if (!res.ok) throw new Error('Failed to get messages')
  return res.json()
}

// Chat completion (proxied to sidecar)
export async function sendMessage(sessionId: string, prompt: string): Promise<{ message_id: string; session_id: string }> {
  const res = await fetch(`/api/sessions/${sessionId}/chat`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ prompt }),
  })
  if (!res.ok) throw new Error('Failed to send message')
  return res.json()
}

// SSE stream
export function createEventSource(sessionId: string, afterSeq: number = 0): EventSource {
  return new EventSource(`/api/sessions/${sessionId}/stream?after_seq=${afterSeq}`)
}

// Stop stream
export async function stopStream(sessionId: string): Promise<void> {
  await fetch(`/api/sessions/${sessionId}/stream`, { method: 'DELETE' })
}
