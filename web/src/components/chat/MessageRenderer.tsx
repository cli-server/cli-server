import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { ThinkingBlock } from './ThinkingBlock'
import { ToolCard } from './ToolCard'
import type { StreamEvent, ToolPayload } from '../../lib/api'

interface MessageRendererProps {
  events: StreamEvent[]
  isStreaming?: boolean
}

export function MessageRenderer({ events, isStreaming }: MessageRendererProps) {
  const elements: React.ReactNode[] = []
  let textBuffer = ''

  const flushText = () => {
    if (textBuffer) {
      elements.push(
        <div key={`text-${elements.length}`} className="prose prose-invert prose-sm max-w-none">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{textBuffer}</ReactMarkdown>
        </div>
      )
      textBuffer = ''
    }
  }

  // Pass 1: collect latest state per tool id and build parent→children map
  const toolMap = new Map<string, ToolPayload>()
  const toolChildren = new Map<string, ToolPayload[]>()

  for (const event of events) {
    if ((event.type === 'tool_started' || event.type === 'tool_completed' || event.type === 'tool_failed') && event.tool) {
      const tool = event.tool
      const existing = toolMap.get(tool.id)
      // Merge: keep input from started, add result/error from completed/failed
      const merged: ToolPayload = existing
        ? { ...existing, ...tool, input: tool.input || existing.input }
        : { ...tool }
      toolMap.set(tool.id, merged)

      // Track children by parent_id
      if (tool.parent_id) {
        const siblings = toolChildren.get(tool.parent_id) || []
        if (!siblings.find(t => t.id === tool.id)) {
          siblings.push(merged)
          toolChildren.set(tool.parent_id, siblings)
        } else {
          // Update existing child in-place
          const idx = siblings.findIndex(t => t.id === tool.id)
          siblings[idx] = merged
          toolChildren.set(tool.parent_id, siblings)
        }
      }
    }
  }

  // Attach children to their parents
  for (const [parentId, children] of toolChildren) {
    const parent = toolMap.get(parentId)
    if (parent) {
      parent.children = children
    }
  }

  // Pass 2: build elements, only rendering top-level tools
  const renderedTools = new Set<string>()

  for (const event of events) {
    switch (event.type) {
      case 'assistant_text':
        textBuffer += event.text || ''
        break
      case 'assistant_thinking':
        flushText()
        elements.push(
          <ThinkingBlock
            key={`think-${elements.length}`}
            thinking={event.thinking || ''}
            isStreaming={isStreaming}
          />
        )
        break
      case 'tool_started':
      case 'tool_completed':
      case 'tool_failed':
        if (event.tool) {
          const toolId = event.tool.id
          const tool = toolMap.get(toolId)!

          // Skip child tools — they render inside their parent
          if (tool.parent_id) break

          if (!renderedTools.has(toolId)) {
            flushText()
            renderedTools.add(toolId)
            elements.push(
              <ToolCard key={`tool-${toolId}`} tool={tool} />
            )
          } else {
            // Update: replace the existing element in-place
            const idx = elements.findIndex(
              (el) => el !== null && typeof el === 'object' && 'key' in el && (el as React.ReactElement).key === `tool-${toolId}`
            )
            if (idx >= 0) {
              elements[idx] = <ToolCard key={`tool-${toolId}`} tool={tool} />
            }
          }
        }
        break
    }
  }
  flushText()

  if (elements.length === 0 && isStreaming) {
    elements.push(
      <div key="thinking" className="flex items-center gap-2 text-sm text-[var(--muted-foreground)]">
        <div className="h-2 w-2 animate-pulse rounded-full bg-[var(--muted-foreground)]" />
        Thinking...
      </div>
    )
  }

  return <>{elements}</>
}
