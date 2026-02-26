import { useState, useRef, useCallback } from 'react'
import { Send, Square } from 'lucide-react'

interface ChatInputProps {
  onSend: (text: string) => void
  disabled?: boolean
  streaming?: boolean
  onStop?: () => void
  borderless?: boolean
}

export function ChatInput({ onSend, disabled, streaming, onStop, borderless }: ChatInputProps) {
  const [text, setText] = useState('')
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  const handleSend = useCallback(() => {
    const trimmed = text.trim()
    if (!trimmed || disabled || streaming) return
    onSend(trimmed)
    setText('')
    if (textareaRef.current) {
      textareaRef.current.style.height = 'auto'
    }
  }, [text, disabled, streaming, onSend])

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      handleSend()
    }
  }

  const handleInput = () => {
    const el = textareaRef.current
    if (el) {
      el.style.height = 'auto'
      el.style.height = Math.min(el.scrollHeight, 200) + 'px'
    }
  }

  return (
    <div className={`flex items-end gap-2 p-3${borderless ? '' : ' border-t border-[var(--border)] bg-[var(--muted)]'}`}>
      <textarea
        ref={textareaRef}
        value={text}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={handleKeyDown}
        onInput={handleInput}
        placeholder="Send a message..."
        disabled={disabled}
        rows={1}
        className="flex-1 resize-none rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm text-[var(--foreground)] placeholder:text-[var(--muted-foreground)] focus:outline-none focus:ring-1 focus:ring-[var(--ring)] disabled:opacity-50"
        style={{ minHeight: '40px', maxHeight: '200px' }}
      />
      {streaming ? (
        <button
          onClick={onStop}
          className="flex h-10 w-10 shrink-0 items-center justify-center rounded-md bg-[var(--destructive)] text-white hover:opacity-90"
          title="Stop generation"
        >
          <Square size={16} />
        </button>
      ) : (
        <button
          onClick={handleSend}
          disabled={!text.trim() || disabled}
          className="flex h-10 w-10 shrink-0 items-center justify-center rounded-md bg-[var(--primary)] text-[var(--primary-foreground)] hover:opacity-90 disabled:opacity-30"
          title="Send message"
        >
          <Send size={16} />
        </button>
      )}
    </div>
  )
}
