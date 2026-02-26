import { useState } from 'react'
import { ChevronRight, Loader2, Check, X } from 'lucide-react'
import type { ToolPayload } from '../../lib/api'

interface ToolCardProps {
  tool: ToolPayload
}

export function ToolCard({ tool }: ToolCardProps) {
  const [open, setOpen] = useState(false)

  const borderColor =
    tool.status === 'started' ? 'border-blue-500/50' :
    tool.status === 'completed' ? 'border-green-500/50' :
    'border-red-500/50'

  const StatusIcon = () => {
    if (tool.status === 'started') return <Loader2 size={14} className="animate-spin text-blue-400" />
    if (tool.status === 'completed') return <Check size={14} className="text-green-400" />
    return <X size={14} className="text-red-400" />
  }

  return (
    <div className={`my-1 rounded border ${borderColor} bg-[var(--muted)]`}>
      <button
        onClick={() => setOpen(!open)}
        className="flex w-full items-center gap-2 px-3 py-2 text-xs hover:bg-[var(--secondary)]"
      >
        <ChevronRight
          size={12}
          className={`text-[var(--muted-foreground)] transition-transform ${open ? 'rotate-90' : ''}`}
        />
        <StatusIcon />
        <span className="font-mono text-[var(--foreground)]">{tool.title || tool.name}</span>
        {tool.children && tool.children.length > 0 && (
          <span className="ml-auto text-[var(--muted-foreground)]">
            {tool.children.length} sub-tool{tool.children.length > 1 ? 's' : ''}
          </span>
        )}
      </button>
      {open && (
        <div className="border-t border-[var(--border)] px-3 py-2 text-xs">
          {tool.input && (
            <div className="mb-2">
              <span className="text-[var(--muted-foreground)]">Input:</span>
              <pre className="mt-1 overflow-x-auto rounded bg-[var(--background)] p-2 text-[var(--foreground)]">
                {JSON.stringify(tool.input, null, 2)}
              </pre>
            </div>
          )}
          {tool.result !== undefined && (
            <div className="mb-2">
              <span className="text-[var(--muted-foreground)]">Result:</span>
              <pre className="mt-1 max-h-60 overflow-auto rounded bg-[var(--background)] p-2 text-[var(--foreground)]">
                {typeof tool.result === 'string' ? tool.result : JSON.stringify(tool.result, null, 2)}
              </pre>
            </div>
          )}
          {tool.error && (
            <div className="mb-2">
              <span className="text-red-400">Error:</span>
              <pre className="mt-1 overflow-x-auto rounded bg-[var(--background)] p-2 text-red-400">
                {tool.error}
              </pre>
            </div>
          )}
          {tool.children && tool.children.length > 0 && (
            <div className="ml-3 mt-2 border-l border-[var(--border)] pl-2">
              {tool.children.map(child => (
                <ToolCard key={`tool-${child.id}`} tool={child} />
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
