export type SandboxStatus = 'creating' | 'running' | 'pausing' | 'paused' | 'resuming' | 'offline'
export type WorkspaceRole = 'owner' | 'maintainer' | 'developer' | 'guest'

export interface Workspace {
  id: string
  name: string
  created_at: string
  updated_at: string
}

export interface WorkspaceMember {
  user_id: string
  username: string
  role: WorkspaceRole
  picture?: string
}

export interface Sandbox {
  id: string
  workspace_id: string
  name: string
  type: string
  status: SandboxStatus
  opencode_url?: string
  openclaw_url?: string
  created_at: string
  last_activity_at: string | null
  paused_at: string | null
  is_local: boolean
  last_heartbeat_at?: string | null
  cpu?: number
  memory?: number
  idle_timeout?: number
}

export async function login(username: string, password: string): Promise<boolean> {
  const res = await fetch('/api/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
  return res.ok
}

export async function register(username: string, email: string, password: string): Promise<boolean> {
  const res = await fetch('/api/auth/register', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, email, password }),
  })
  return res.ok
}

export async function checkAuth(): Promise<boolean> {
  const res = await fetch('/api/auth/check')
  return res.ok
}

export async function getOIDCProviders(): Promise<{ providers: string[]; password_auth: boolean }> {
  const res = await fetch('/api/auth/oidc/providers')
  if (!res.ok) return { providers: [], password_auth: true }
  const data = await res.json()
  return {
    providers: data.providers || [],
    password_auth: data.password_auth !== false,
  }
}

export async function getMe(): Promise<{ id: string; username: string; email: string; name?: string | null; picture?: string | null; role: string }> {
  const res = await fetch('/api/auth/me')
  if (!res.ok) throw new Error('Failed to get user info')
  return res.json()
}

export async function logout(): Promise<void> {
  await fetch('/api/auth/logout', { method: 'POST' })
}

// Workspace API

async function checkQuotaError(res: Response): Promise<QuotaExceededError | ResourceBudgetExceededError | null> {
  if (res.status !== 403) return null
  try {
    const body = await res.json()
    if (body.error === 'quota_exceeded') return body as QuotaExceededError
    if (body.error === 'resource_budget_exceeded') return body as ResourceBudgetExceededError
  } catch {
    // not a quota error
  }
  return null
}

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
  if (!res.ok) {
    const err = await checkQuotaError(res)
    if (err) throw err
    throw new Error('Failed to create workspace')
  }
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

export async function getWorkspacesQuota(): Promise<WorkspacesQuota> {
  const res = await fetch('/api/workspaces/quota')
  if (!res.ok) throw new Error('Failed to get workspaces quota')
  return res.json()
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

export interface WorkspaceSandboxDefaults {
  max_sandbox_cpu: number    // millicores
  max_sandbox_memory: number // bytes
  max_idle_timeout: number   // seconds
  max_sandboxes: number      // 0 = unlimited
  current_sandboxes: number
}

export async function getWorkspaceDefaults(workspaceId: string): Promise<WorkspaceSandboxDefaults> {
  const res = await fetch(`/api/workspaces/${workspaceId}/defaults`)
  if (!res.ok) throw new Error('Failed to get workspace defaults')
  return res.json()
}

export async function listSandboxes(workspaceId: string): Promise<Sandbox[]> {
  const res = await fetch(`/api/workspaces/${workspaceId}/sandboxes`)
  if (!res.ok) throw new Error('Failed to list sandboxes')
  return res.json()
}

export async function createSandbox(
  workspaceId: string,
  name?: string,
  type?: 'opencode' | 'openclaw',
  cpu?: number,
  memory?: number,
  idleTimeout?: number,
): Promise<Sandbox> {
  const body: Record<string, unknown> = {
    name: name || 'New Sandbox',
    type: type || 'opencode',
  }
  if (cpu !== undefined) body.cpu = cpu
  if (memory !== undefined) body.memory = memory
  if (idleTimeout !== undefined) body.idle_timeout = idleTimeout
  const res = await fetch(`/api/workspaces/${workspaceId}/sandboxes`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!res.ok) {
    const err = await checkQuotaError(res)
    if (err) throw err
    throw new Error('Failed to create sandbox')
  }
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

// Usage & Traces API

export interface UsageSummary {
  provider: string
  model: string
  input_tokens: number
  output_tokens: number
  cache_creation_input_tokens: number
  cache_read_input_tokens: number
  request_count: number
}

export interface TraceItem {
  id: string
  sandbox_id: string
  workspace_id: string
  source: string
  created_at: string
  updated_at: string
  request_count: number
  total_input_tokens: number
  total_output_tokens: number
  total_cache_read_tokens: number
  total_cache_creation_tokens: number
  models: string
}

export interface UsageResponse {
  usage: UsageSummary[]
}

export interface TracesResponse {
  traces: TraceItem[]
  total: number
}

export async function getSandboxUsage(id: string): Promise<UsageResponse> {
  const res = await fetch(`/api/sandboxes/${id}/usage`)
  if (!res.ok) throw new Error('Failed to get sandbox usage')
  return res.json()
}

export async function getSandboxTraces(id: string, limit: number, offset: number): Promise<TracesResponse> {
  const res = await fetch(`/api/sandboxes/${id}/traces?limit=${limit}&offset=${offset}`)
  if (!res.ok) throw new Error('Failed to get sandbox traces')
  return res.json()
}

export interface TokenUsageItem {
  id: string
  trace_id: string
  provider: string
  model: string
  message_id?: string
  input_tokens: number
  output_tokens: number
  cache_creation_input_tokens: number
  cache_read_input_tokens: number
  streaming: boolean
  duration: number
  ttft: number
  created_at: string
}

export interface TraceDetailResponse {
  trace: TraceItem
  requests: TokenUsageItem[]
}

export async function getTraceDetail(sandboxId: string, traceId: string): Promise<TraceDetailResponse> {
  const res = await fetch(`/api/sandboxes/${sandboxId}/traces/${traceId}`)
  if (!res.ok) throw new Error('Failed to get trace detail')
  return res.json()
}

export async function getWorkspaceTraces(workspaceId: string, limit: number, offset: number): Promise<TracesResponse> {
  const res = await fetch(`/api/workspaces/${workspaceId}/traces?limit=${limit}&offset=${offset}`)
  if (!res.ok) throw new Error('Failed to get workspace traces')
  return res.json()
}

export async function getWorkspaceTraceDetail(workspaceId: string, traceId: string): Promise<TraceDetailResponse> {
  const res = await fetch(`/api/workspaces/${workspaceId}/traces/${traceId}`)
  if (!res.ok) throw new Error('Failed to get trace detail')
  return res.json()
}

// Agent registration code API

export async function createAgentCode(workspaceId: string): Promise<{ code: string; expires_at: string }> {
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
  email: string
  name: string | null
  role: string
  created_at: string
}

export interface AdminWorkspace {
  id: string
  name: string
  created_at: string
  updated_at: string
}

export interface AdminSandbox {
  id: string
  name: string
  workspace_id: string
  type: string
  status: string
  created_at: string
  last_activity_at: string | null
  is_local: boolean
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

// Quota types

export interface QuotaDefaults {
  max_workspaces_per_user: number
  max_sandboxes_per_workspace: number
  max_workspace_drive_size: number   // bytes
  max_sandbox_cpu: number           // millicores
  max_sandbox_memory: number        // bytes
  max_idle_timeout: number          // seconds
  ws_max_total_cpu: number           // millicores
  ws_max_total_memory: number        // bytes
  ws_max_idle_timeout: number        // seconds
}

export interface UserQuotaOverrides {
  max_workspaces: number | null
  updated_at: string
}

export interface UserQuotaResponse {
  defaults: { max_workspaces_per_user: number }
  overrides: UserQuotaOverrides | null
}

export interface WorkspaceQuotaOverrides {
  max_sandboxes: number | null
  max_sandbox_cpu: number | null    // millicores
  max_sandbox_memory: number | null // bytes
  max_idle_timeout: number | null   // seconds
  max_total_cpu: number | null      // millicores
  max_total_memory: number | null   // bytes
  max_drive_size: number | null     // bytes
  updated_at: string
}

export interface WorkspaceQuotaDefaults {
  max_sandboxes: number
  max_sandbox_cpu: number           // millicores
  max_sandbox_memory: number        // bytes
  max_idle_timeout: number          // seconds
  max_total_cpu: number             // millicores
  max_total_memory: number          // bytes
  max_drive_size: number            // bytes
}

export interface WorkspaceQuotaResponse {
  defaults: WorkspaceQuotaDefaults
  overrides: WorkspaceQuotaOverrides | null
}

export interface QuotaExceededError {
  error: 'quota_exceeded'
  message: string
  quota: { current: number; max: number }
}

export interface ResourceBudgetExceededError {
  error: 'resource_budget_exceeded'
  message: string
}

export interface WorkspacesQuota {
  current: number
  max: number
}

// Admin quota API

export async function adminGetQuotaDefaults(): Promise<QuotaDefaults> {
  const res = await fetch('/api/admin/quotas/defaults')
  if (!res.ok) throw new Error('Failed to get quota defaults')
  return res.json()
}

export async function adminSetQuotaDefaults(defaults: Partial<QuotaDefaults>): Promise<QuotaDefaults> {
  const res = await fetch('/api/admin/quotas/defaults', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(defaults),
  })
  if (!res.ok) throw new Error('Failed to set quota defaults')
  return res.json()
}

export async function adminGetUserQuota(userId: string): Promise<UserQuotaResponse> {
  const res = await fetch(`/api/admin/users/${userId}/quota`)
  if (!res.ok) throw new Error('Failed to get user quota')
  return res.json()
}

export async function adminSetUserQuota(
  userId: string,
  overrides: {
    max_workspaces?: number
  }
): Promise<void> {
  const res = await fetch(`/api/admin/users/${userId}/quota`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(overrides),
  })
  if (!res.ok) throw new Error('Failed to set user quota')
}

export async function adminDeleteUserQuota(userId: string): Promise<void> {
  const res = await fetch(`/api/admin/users/${userId}/quota`, { method: 'DELETE' })
  if (!res.ok) throw new Error('Failed to delete user quota')
}

// Workspace quota API

export async function adminGetWorkspaceQuota(workspaceId: string): Promise<WorkspaceQuotaResponse> {
  const res = await fetch(`/api/admin/workspaces/${workspaceId}/quota`)
  if (!res.ok) throw new Error('Failed to get workspace quota')
  return res.json()
}

export async function adminSetWorkspaceQuota(
  workspaceId: string,
  overrides: {
    max_sandboxes?: number
    max_sandbox_cpu?: number
    max_sandbox_memory?: number
    max_idle_timeout?: number
    max_total_cpu?: number
    max_total_memory?: number
    max_drive_size?: number
  }
): Promise<void> {
  const res = await fetch(`/api/admin/workspaces/${workspaceId}/quota`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(overrides),
  })
  if (!res.ok) throw new Error('Failed to set workspace quota')
}

export async function adminDeleteWorkspaceQuota(workspaceId: string): Promise<void> {
  const res = await fetch(`/api/admin/workspaces/${workspaceId}/quota`, { method: 'DELETE' })
  if (!res.ok) throw new Error('Failed to delete workspace quota')
}
