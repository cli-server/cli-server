import { useState } from 'react'
import { ChevronRight, Brain } from 'lucide-react'

interface ThinkingBlockProps {
  thinking: string
  isStreaming?: boolean
}

export function ThinkingBlock({ thinking, isStreaming }: ThinkingBlockProps) {
  const [open, setOpen] = useState(isStreaming ?? false)

  return (
    <div className="my-1 rounded border border-[var(--border)] bg-[var(--muted)]">
      <button
        onClick={() => setOpen(!open)}
        className="flex w-full items-center gap-2 px-3 py-2 text-xs text-[var(--muted-foreground)] hover:bg-[var(--secondary)]"
      >
        <ChevronRight
          size={12}
          className={`transition-transform ${open ? 'rotate-90' : ''}`}
        />
        <Brain size={12} />
        <span>{isStreaming ? 'Thinking...' : 'Thought process'}</span>
      </button>
      {open && (
        <div className="border-t border-[var(--border)] px-3 py-2 text-xs text-[var(--muted-foreground)] whitespace-pre-wrap">
          {thinking}
        </div>
      )}
    </div>
  )
}
