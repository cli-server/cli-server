import { useState, useEffect } from 'react'
import { X, Loader2 } from 'lucide-react'
import { getWorkspaceDefaults, type WorkspaceSandboxDefaults } from '../lib/api'

interface CreateSandboxModalProps {
  workspaceId: string
  onClose: () => void
  onCreate: (name: string, type: 'opencode' | 'openclaw', cpu?: number, memory?: number, idleTimeout?: number) => void
  creating: boolean
}

export function CreateSandboxModal({ workspaceId, onClose, onCreate, creating }: CreateSandboxModalProps) {
  const [name, setName] = useState('New Sandbox')
  const [sandboxType, setSandboxType] = useState<'opencode' | 'openclaw'>('opencode')
  const [defaults, setDefaults] = useState<WorkspaceSandboxDefaults | null>(null)
  const [loadingDefaults, setLoadingDefaults] = useState(true)

  // CPU in cores (display), stored as millicores internally
  const [cpuCores, setCpuCores] = useState<string>('')
  // Memory in MB (display), stored as bytes internally
  const [memoryMB, setMemoryMB] = useState<string>('')
  // Idle timeout in minutes (display), stored as seconds internally
  const [timeoutMinutes, setTimeoutMinutes] = useState<string>('')

  const [validationError, setValidationError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setLoadingDefaults(true)
    getWorkspaceDefaults(workspaceId)
      .then((d) => {
        if (cancelled) return
        setDefaults(d)
        setCpuCores(String(d.maxSandboxCpu / 1000))
        setMemoryMB(String(Math.round(d.maxSandboxMemory / (1024 * 1024))))
        setTimeoutMinutes(String(Math.round(d.maxIdleTimeout / 60)))
      })
      .catch(() => {
        if (!cancelled) setDefaults(null)
      })
      .finally(() => {
        if (!cancelled) setLoadingDefaults(false)
      })
    return () => { cancelled = true }
  }, [workspaceId])

  const validate = (): { cpu: number; memory: number; idleTimeout: number } | null => {
    if (!defaults) return null

    const cpu = parseFloat(cpuCores)
    const mem = parseFloat(memoryMB)
    const timeout = parseFloat(timeoutMinutes)

    if (isNaN(cpu) || cpu <= 0) {
      setValidationError('CPU must be greater than 0')
      return null
    }
    const cpuMillis = Math.round(cpu * 1000)
    if (cpuMillis > defaults.maxSandboxCpu) {
      setValidationError(`CPU cannot exceed ${defaults.maxSandboxCpu / 1000} cores`)
      return null
    }

    if (isNaN(mem) || mem <= 0) {
      setValidationError('Memory must be greater than 0')
      return null
    }
    const memBytes = Math.round(mem * 1024 * 1024)
    if (memBytes > defaults.maxSandboxMemory) {
      setValidationError(`Memory cannot exceed ${Math.round(defaults.maxSandboxMemory / (1024 * 1024))} MB`)
      return null
    }

    if (isNaN(timeout) || timeout < 0) {
      setValidationError('Idle timeout must be 0 or greater')
      return null
    }
    const timeoutSecs = Math.round(timeout * 60)
    if (defaults.maxIdleTimeout > 0 && timeoutSecs > defaults.maxIdleTimeout) {
      setValidationError(`Idle timeout cannot exceed ${Math.round(defaults.maxIdleTimeout / 60)} minutes`)
      return null
    }

    setValidationError(null)
    return { cpu: cpuMillis, memory: memBytes, idleTimeout: timeoutSecs }
  }

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    const resources = validate()
    if (!resources) return
    onCreate(name, sandboxType, resources.cpu, resources.memory, resources.idleTimeout)
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50" onClick={onClose}>
      <div
        className="w-full max-w-sm rounded-lg border border-[var(--border)] bg-[var(--card)] p-6 shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between mb-4">
          <h2 className="text-lg font-semibold text-[var(--foreground)]">Create Sandbox</h2>
          <button
            onClick={onClose}
            className="rounded p-1 hover:bg-[var(--secondary)]"
          >
            <X size={16} />
          </button>
        </div>

        <form onSubmit={handleSubmit} className="flex flex-col gap-4">
          <div>
            <label className="block text-sm font-medium text-[var(--foreground)] mb-1">Name</label>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              className="w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] outline-none focus:border-[var(--primary)]"
            />
          </div>

          <div>
            <label className="block text-sm font-medium text-[var(--foreground)] mb-2">Type</label>
            <div className="flex gap-2">
              <button
                type="button"
                onClick={() => setSandboxType('opencode')}
                className={`flex-1 rounded-md border px-3 py-2 text-sm font-medium transition-colors ${
                  sandboxType === 'opencode'
                    ? 'border-[var(--primary)] bg-[var(--primary)] text-[var(--primary-foreground)]'
                    : 'border-[var(--border)] bg-[var(--background)] text-[var(--foreground)] hover:bg-[var(--secondary)]'
                }`}
              >
                OpenCode
              </button>
              <button
                type="button"
                onClick={() => setSandboxType('openclaw')}
                className={`flex-1 rounded-md border px-3 py-2 text-sm font-medium transition-colors ${
                  sandboxType === 'openclaw'
                    ? 'border-[var(--primary)] bg-[var(--primary)] text-[var(--primary-foreground)]'
                    : 'border-[var(--border)] bg-[var(--background)] text-[var(--foreground)] hover:bg-[var(--secondary)]'
                }`}
              >
                OpenClaw
              </button>
            </div>
          </div>

          {loadingDefaults ? (
            <div className="flex items-center justify-center gap-2 py-3 text-sm text-[var(--muted-foreground)]">
              <Loader2 size={14} className="animate-spin" />
              Loading defaults...
            </div>
          ) : defaults ? (
            <>
              <div>
                <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                  CPU (cores)
                  <span className="ml-1 text-xs font-normal text-[var(--muted-foreground)]">
                    max {defaults.maxSandboxCpu / 1000}
                  </span>
                </label>
                <input
                  type="number"
                  value={cpuCores}
                  onChange={(e) => setCpuCores(e.target.value)}
                  step="0.5"
                  min="0.5"
                  max={defaults.maxSandboxCpu / 1000}
                  className="w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] outline-none focus:border-[var(--primary)]"
                />
              </div>

              <div>
                <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                  Memory (MB)
                  <span className="ml-1 text-xs font-normal text-[var(--muted-foreground)]">
                    max {Math.round(defaults.maxSandboxMemory / (1024 * 1024))}
                  </span>
                </label>
                <input
                  type="number"
                  value={memoryMB}
                  onChange={(e) => setMemoryMB(e.target.value)}
                  step="256"
                  min="256"
                  max={Math.round(defaults.maxSandboxMemory / (1024 * 1024))}
                  className="w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] outline-none focus:border-[var(--primary)]"
                />
              </div>

              <div>
                <label className="block text-sm font-medium text-[var(--foreground)] mb-1">
                  Idle Timeout (minutes)
                  <span className="ml-1 text-xs font-normal text-[var(--muted-foreground)]">
                    {defaults.maxIdleTimeout > 0 ? `max ${Math.round(defaults.maxIdleTimeout / 60)}` : 'unlimited'}
                    {', '}0 = never
                  </span>
                </label>
                <input
                  type="number"
                  value={timeoutMinutes}
                  onChange={(e) => setTimeoutMinutes(e.target.value)}
                  step="5"
                  min="0"
                  max={defaults.maxIdleTimeout > 0 ? Math.round(defaults.maxIdleTimeout / 60) : undefined}
                  className="w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] outline-none focus:border-[var(--primary)]"
                />
              </div>
            </>
          ) : null}

          {validationError && (
            <p className="text-sm text-red-500">{validationError}</p>
          )}

          <div className="flex gap-2 justify-end mt-2">
            <button
              type="button"
              onClick={onClose}
              className="rounded-md border border-[var(--border)] px-4 py-2 text-sm font-medium text-[var(--foreground)] hover:bg-[var(--secondary)]"
            >
              Cancel
            </button>
            <button
              type="submit"
              disabled={creating || !name.trim() || loadingDefaults}
              className="rounded-md bg-[var(--primary)] px-4 py-2 text-sm font-medium text-[var(--primary-foreground)] hover:opacity-90 disabled:opacity-50"
            >
              {creating ? 'Creating...' : 'Create'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}
