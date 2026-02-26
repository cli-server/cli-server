import { useState, useEffect, useRef, useCallback } from 'react'
import { Loader2, Play } from 'lucide-react'
import { getMessages, sendMessage, createEventSource, stopStream, type Message, type StreamEvent, type StreamEnvelope, type SessionStatus, resumeSession } from '../../lib/api'
import { MessageBubble } from './MessageBubble'
import { ChatInput } from './ChatInput'

interface ChatProps {
  sessionId: string
  sandboxName?: string
  status: SessionStatus
  onStatusChange?: () => void
}

export function Chat({ sessionId, status, onStatusChange }: ChatProps) {
  const [messages, setMessages] = useState<Message[]>([])
  const [loading, setLoading] = useState(true)
  const [streaming, setStreaming] = useState(false)
  const [resuming, setResuming] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)
  const eventSourceRef = useRef<EventSource | null>(null)
  const lastSeqRef = useRef(0)
  const activeMessageIdRef = useRef<string | null>(null)

  const scrollToBottom = useCallback(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [])

  const connectSSE = useCallback((sid: string, afterSeq: number) => {
    eventSourceRef.current?.close()
    const es = createEventSource(sid, afterSeq)
    eventSourceRef.current = es

    es.addEventListener('stream', (event: Event) => {
      const msgEvent = event as MessageEvent
      if (!msgEvent.data) return
      try {
        const envelope: StreamEnvelope = JSON.parse(msgEvent.data)
        const seq = envelope.seq || 0
        if (seq > lastSeqRef.current) {
          lastSeqRef.current = seq
        }

        // Only treat terminal events for the actively streaming message
        const isActiveMessage = activeMessageIdRef.current === envelope.messageId

        if (envelope.kind === 'complete' || envelope.kind === 'cancelled') {
          const completionData: Partial<Message> = {
            stream_status: envelope.kind === 'cancelled' ? 'interrupted' as const : 'completed' as const,
          }
          if (envelope.kind === 'complete' && envelope.payload) {
            if (envelope.payload.usage) {
              completionData.usage = envelope.payload.usage as Message['usage']
            }
            if (envelope.payload.total_cost_usd !== undefined) {
              completionData.total_cost_usd = envelope.payload.total_cost_usd as number
            }
          }
          setMessages(prev => prev.map(m =>
            m.id === envelope.messageId
              ? { ...m, ...completionData }
              : m
          ))
          if (isActiveMessage) {
            setStreaming(false)
            activeMessageIdRef.current = null
            es.close()
            eventSourceRef.current = null
          }
          return
        }

        if (envelope.kind === 'error') {
          setMessages(prev => prev.map(m =>
            m.id === envelope.messageId
              ? { ...m, stream_status: 'failed' as const }
              : m
          ))
          if (isActiveMessage) {
            setStreaming(false)
            activeMessageIdRef.current = null
            es.close()
            eventSourceRef.current = null
          }
          return
        }

        // Accumulate events into the assistant message
        const streamEvent: StreamEvent = {
          type: envelope.kind,
          ...envelope.payload,
        }

        setMessages(prev => {
          const updated = [...prev]
          const idx = updated.findIndex(m => m.id === envelope.messageId)
          if (idx >= 0) {
            const msg = { ...updated[idx] }
            const events = [...(msg.content_render?.events || []), streamEvent]
            msg.content_render = { events }
            if (streamEvent.type === 'assistant_text' && streamEvent.text) {
              msg.content_text += streamEvent.text
            }
            updated[idx] = msg
          }
          return updated
        })
      } catch {
        // ignore parse errors
      }
    })

    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED) {
        setStreaming(false)
        activeMessageIdRef.current = null
        eventSourceRef.current = null
      }
    }
  }, [])

  // Load messages on mount
  useEffect(() => {
    if (status !== 'running') {
      setLoading(false)
      return
    }
    let cancelled = false
    setLoading(true)
    // Reset seq tracking when switching sessions
    lastSeqRef.current = 0
    activeMessageIdRef.current = null
    getMessages(sessionId).then((msgs) => {
      if (cancelled) return
      setMessages(msgs)
      setLoading(false)

      // Check if last assistant message is still streaming
      const lastMsg = msgs[msgs.length - 1]
      if (lastMsg?.role === 'assistant' && lastMsg.stream_status === 'in_progress') {
        setStreaming(true)
        activeMessageIdRef.current = lastMsg.id
        connectSSE(sessionId, 0)
      }
    }).catch(() => {
      if (!cancelled) setLoading(false)
    })
    return () => { cancelled = true }
  }, [sessionId, status, connectSSE])

  // Auto-scroll on message changes
  useEffect(() => {
    scrollToBottom()
  }, [messages, scrollToBottom])

  // Cleanup SSE on unmount
  useEffect(() => {
    return () => {
      eventSourceRef.current?.close()
      eventSourceRef.current = null
    }
  }, [sessionId])

  const handleSend = useCallback(async (text: string) => {
    if (streaming) return

    // Add user message optimistically
    const tempUserMsg: Message = {
      id: 'temp-' + Date.now(),
      session_id: sessionId,
      role: 'user',
      content_text: text,
      content_render: { events: [] },
      stream_status: 'completed',
      created_at: new Date().toISOString(),
    }
    setMessages(prev => [...prev, tempUserMsg])
    setStreaming(true)

    try {
      const result = await sendMessage(sessionId, text)

      // Replace temp user message and add assistant placeholder
      const assistantMsg: Message = {
        id: result.message_id,
        session_id: sessionId,
        role: 'assistant',
        content_text: '',
        content_render: { events: [] },
        stream_status: 'in_progress',
        created_at: new Date().toISOString(),
      }

      activeMessageIdRef.current = result.message_id

      setMessages(prev => {
        const updated = prev.filter(m => m.id !== tempUserMsg.id)
        // Re-add user message with real data, then assistant
        return [...updated, { ...tempUserMsg, id: 'user-' + result.message_id }, assistantMsg]
      })

      connectSSE(sessionId, lastSeqRef.current)
    } catch {
      setStreaming(false)
      activeMessageIdRef.current = null
      // Remove temp message on error
      setMessages(prev => prev.filter(m => m.id !== tempUserMsg.id))
    }
  }, [sessionId, streaming, connectSSE])

  const handleStop = useCallback(async () => {
    try {
      await stopStream(sessionId)
    } catch {
      // ignore
    }
    eventSourceRef.current?.close()
    eventSourceRef.current = null
    activeMessageIdRef.current = null
    setStreaming(false)
  }, [sessionId])

  const handleResume = async () => {
    setResuming(true)
    try {
      await resumeSession(sessionId)
      onStatusChange?.()
    } catch {
      // ignore
    } finally {
      setResuming(false)
    }
  }

  // Paused overlay
  if (status === 'paused') {
    return (
      <div className="flex h-full w-full flex-col items-center justify-center gap-4">
        <p className="text-[var(--muted-foreground)]">Session is paused</p>
        <button
          onClick={handleResume}
          disabled={resuming}
          className="flex items-center gap-2 rounded-md bg-[var(--primary)] px-4 py-2 text-sm font-medium text-[var(--primary-foreground)] hover:opacity-90 disabled:opacity-50"
        >
          <Play size={14} />
          {resuming ? 'Resuming...' : 'Resume'}
        </button>
      </div>
    )
  }

  // Transitional states
  if (status === 'pausing' || status === 'resuming') {
    return (
      <div className="flex h-full w-full flex-col items-center justify-center gap-3">
        <Loader2 size={24} className="animate-spin text-[var(--muted-foreground)]" />
        <p className="text-[var(--muted-foreground)]">
          {status === 'pausing' ? 'Pausing session...' : 'Resuming session...'}
        </p>
      </div>
    )
  }

  const isEmpty = !loading && messages.length === 0

  return (
    <div className="flex h-full w-full flex-col">
      {isEmpty ? (
        <div className="flex flex-1 flex-col items-center justify-center gap-4 px-4">
          <p className="text-[var(--muted-foreground)]">Send a message to start chatting</p>
          <div className="w-full max-w-2xl">
            <ChatInput
              onSend={handleSend}
              disabled={status !== 'running'}
              streaming={streaming}
              onStop={handleStop}
              borderless
            />
          </div>
        </div>
      ) : (
        <>
          <div ref={scrollRef} className="flex-1 overflow-y-auto">
            {loading ? (
              <div className="flex h-full items-center justify-center">
                <Loader2 size={24} className="animate-spin text-[var(--muted-foreground)]" />
              </div>
            ) : (
              <div className="divide-y divide-[var(--border)]/50">
                {messages.map((msg) => (
                  <MessageBubble
                    key={msg.id}
                    message={msg}
                    isStreaming={streaming && msg.role === 'assistant' && msg.stream_status === 'in_progress'}
                  />
                ))}
              </div>
            )}
          </div>
          <ChatInput
            onSend={handleSend}
            disabled={status !== 'running'}
            streaming={streaming}
            onStop={handleStop}
          />
        </>
      )}
    </div>
  )
}
