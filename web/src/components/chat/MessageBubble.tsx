import { useState, useEffect, useRef } from 'react'
import { MessageRenderer } from './MessageRenderer'
import { User, Bot } from 'lucide-react'
import type { Message } from '../../lib/api'

interface MessageBubbleProps {
  message: Message
  isStreaming?: boolean
}

function formatElapsed(seconds: number): string {
  const m = Math.floor(seconds / 60)
  const s = seconds % 60
  return m > 0 ? `${m}m ${s}s` : `${s}s`
}

function formatTokens(n: number): string {
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`
  return String(n)
}

function StreamingStatus({ message, isStreaming }: { message: Message; isStreaming: boolean }) {
  const [elapsed, setElapsed] = useState(0)
  const startRef = useRef(Date.now())

  useEffect(() => {
    if (!isStreaming) return
    startRef.current = Date.now()
    setElapsed(0)
    const interval = setInterval(() => {
      setElapsed(Math.floor((Date.now() - startRef.current) / 1000))
    }, 1000)
    return () => clearInterval(interval)
  }, [isStreaming])

  const isComplete = message.stream_status === 'completed' || message.stream_status === 'failed' || message.stream_status === 'interrupted'
  const { usage, total_cost_usd } = message

  if (isStreaming) {
    return (
      <div className="mt-2 flex items-center gap-2 text-xs text-[var(--muted-foreground)]">
        <div className="h-1.5 w-1.5 animate-pulse rounded-full bg-blue-400" />
        <span>{formatElapsed(elapsed)}</span>
      </div>
    )
  }

  if (isComplete && (usage || total_cost_usd !== undefined)) {
    const parts: string[] = []
    if (usage?.input_tokens) parts.push(`${formatTokens(usage.input_tokens)} in`)
    if (usage?.output_tokens) parts.push(`${formatTokens(usage.output_tokens)} out`)
    if (total_cost_usd !== undefined && total_cost_usd !== null) {
      parts.push(`$${total_cost_usd.toFixed(4)}`)
    }
    if (parts.length === 0) return null
    return (
      <div className="mt-2 text-xs text-[var(--muted-foreground)]">
        {parts.join(' · ')}
      </div>
    )
  }

  return null
}

export function MessageBubble({ message, isStreaming }: MessageBubbleProps) {
  if (message.role === 'user') {
    return (
      <div className="flex gap-3 px-4 py-3">
        <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-[var(--secondary)]">
          <User size={14} className="text-[var(--muted-foreground)]" />
        </div>
        <div className="min-w-0 flex-1 pt-0.5 text-sm text-[var(--foreground)] whitespace-pre-wrap">
          {message.content_text}
        </div>
      </div>
    )
  }

  return (
    <div className="flex gap-3 bg-[var(--muted)]/30 px-4 py-3">
      <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-[var(--primary)]/10">
        <Bot size={14} className="text-[var(--primary)]" />
      </div>
      <div className="min-w-0 flex-1 pt-0.5 text-sm">
        <MessageRenderer
          events={message.content_render?.events || []}
          isStreaming={isStreaming}
        />
        <StreamingStatus message={message} isStreaming={!!isStreaming} />
      </div>
    </div>
  )
}
