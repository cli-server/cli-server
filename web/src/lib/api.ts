export type SandboxStatus = 'creating' | 'running' | 'pausing' | 'paused' | 'resuming' | 'offline'
export type WorkspaceRole = 'owner' | 'maintainer' | 'developer' | 'guest'

export interface Workspace {
  id: string
  name: string
  diskPvcName?: string
  createdAt: string
  updatedAt: string
}

export interface WorkspaceMember {
  userId: string
  username: string
  role: WorkspaceRole
}

export interface Sandbox {
  id: string
  workspaceId: string
  name: string
  type: string
  status: SandboxStatus
  opencodeUrl?: string
  openclawUrl?: string
  createdAt: string
  lastActivityAt: string | null
  pausedAt: string | null
  isLocal: boolean
  lastHeartbeatAt?: string | null
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

export async function getMe(): Promise<{ id: string; username: string; email?: string | null; role: string }> {
  const res = await fetch('/api/auth/me')
  if (!res.ok) throw new Error('Failed to get user info')
  return res.json()
}

export async function logout(): Promise<void> {
  await fetch('/api/auth/logout', { method: 'POST' })
}

// Workspace API

export async function listWorkspaces(): Promise<Workspace[]> {
  const res = await fetch('/api/workspaces')
  if (!res.ok) throw new Error('Failed to list workspaces')
  return res.json()
}

export async function createWorkspace(name?: string): Promise<Workspace> {
  const res = await fetch('/api/workspaces', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: name || 'New Workspace' }),
  })
  if (!res.ok) throw new Error('Failed to create workspace')
  return res.json()
}

export async function getWorkspace(id: string): Promise<Workspace> {
  const res = await fetch(`/api/workspaces/${id}`)
  if (!res.ok) throw new Error('Failed to get workspace')
  return res.json()
}

export async function deleteWorkspace(id: string): Promise<void> {
  const res = await fetch(`/api/workspaces/${id}`, { method: 'DELETE' })
  if (!res.ok) throw new Error('Failed to delete workspace')
}

// Workspace member API

export async function listMembers(workspaceId: string): Promise<WorkspaceMember[]> {
  const res = await fetch(`/api/workspaces/${workspaceId}/members`)
  if (!res.ok) throw new Error('Failed to list members')
  return res.json()
}

export async function addMember(workspaceId: string, username: string, role?: string): Promise<WorkspaceMember> {
  const res = await fetch(`/api/workspaces/${workspaceId}/members`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, role: role || 'developer' }),
  })
  if (!res.ok) throw new Error('Failed to add member')
  return res.json()
}

export async function updateMemberRole(workspaceId: string, userId: string, role: string): Promise<void> {
  const res = await fetch(`/api/workspaces/${workspaceId}/members/${userId}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ role }),
  })
  if (!res.ok) throw new Error('Failed to update member role')
}

export async function removeMember(workspaceId: string, userId: string): Promise<void> {
  const res = await fetch(`/api/workspaces/${workspaceId}/members/${userId}`, { method: 'DELETE' })
  if (!res.ok) throw new Error('Failed to remove member')
}

// Sandbox API

export async function listSandboxes(workspaceId: string): Promise<Sandbox[]> {
  const res = await fetch(`/api/workspaces/${workspaceId}/sandboxes`)
  if (!res.ok) throw new Error('Failed to list sandboxes')
  return res.json()
}

export async function createSandbox(
  workspaceId: string,
  name?: string,
  type?: 'opencode' | 'openclaw',
  telegramBotToken?: string
): Promise<Sandbox> {
  const res = await fetch(`/api/workspaces/${workspaceId}/sandboxes`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      name: name || 'New Sandbox',
      type: type || 'opencode',
      ...(telegramBotToken ? { telegramBotToken } : {}),
    }),
  })
  if (!res.ok) throw new Error('Failed to create sandbox')
  return res.json()
}

export async function getSandbox(id: string): Promise<Sandbox> {
  const res = await fetch(`/api/sandboxes/${id}`)
  if (!res.ok) throw new Error('Failed to get sandbox')
  return res.json()
}

export async function deleteSandbox(id: string): Promise<void> {
  const res = await fetch(`/api/sandboxes/${id}`, { method: 'DELETE' })
  if (!res.ok) throw new Error('Failed to delete sandbox')
}

export async function pauseSandbox(id: string): Promise<void> {
  const res = await fetch(`/api/sandboxes/${id}/pause`, { method: 'POST' })
  if (!res.ok) throw new Error('Failed to pause sandbox')
}

export async function resumeSandbox(id: string): Promise<void> {
  const res = await fetch(`/api/sandboxes/${id}/resume`, { method: 'POST' })
  if (!res.ok) throw new Error('Failed to resume sandbox')
}

// Agent registration code API

export async function createAgentCode(workspaceId: string): Promise<{ code: string; expiresAt: string }> {
  const res = await fetch(`/api/workspaces/${workspaceId}/agent-code`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
  })
  if (!res.ok) throw new Error('Failed to create agent code')
  return res.json()
}

// Admin API

export interface AdminUser {
  id: string
  username: string
  email: string | null
  role: string
  createdAt: string
}

export interface AdminWorkspace {
  id: string
  name: string
  createdAt: string
  updatedAt: string
}

export interface AdminSandbox {
  id: string
  name: string
  workspaceId: string
  type: string
  status: string
  createdAt: string
  lastActivityAt: string | null
  isLocal: boolean
}

export async function adminListUsers(): Promise<AdminUser[]> {
  const res = await fetch('/api/admin/users')
  if (!res.ok) throw new Error('Failed to list users')
  return res.json()
}

export async function adminListWorkspaces(): Promise<AdminWorkspace[]> {
  const res = await fetch('/api/admin/workspaces')
  if (!res.ok) throw new Error('Failed to list workspaces')
  return res.json()
}

export async function adminListSandboxes(): Promise<AdminSandbox[]> {
  const res = await fetch('/api/admin/sandboxes')
  if (!res.ok) throw new Error('Failed to list sandboxes')
  return res.json()
}

export async function adminUpdateUserRole(userId: string, role: string): Promise<void> {
  const res = await fetch(`/api/admin/users/${userId}/role`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ role }),
  })
  if (!res.ok) throw new Error('Failed to update user role')
}
