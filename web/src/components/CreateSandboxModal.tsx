import { useState } from 'react'
import { X } from 'lucide-react'

interface CreateSandboxModalProps {
  onClose: () => void
  onCreate: (name: string, type: 'opencode' | 'openclaw') => void
  creating: boolean
}

export function CreateSandboxModal({ onClose, onCreate, creating }: CreateSandboxModalProps) {
  const [name, setName] = useState('New Sandbox')
  const [sandboxType, setSandboxType] = useState<'opencode' | 'openclaw'>('opencode')

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    onCreate(name, sandboxType)
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
              disabled={creating || !name.trim()}
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
