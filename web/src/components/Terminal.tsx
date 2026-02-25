import { useEffect, useRef } from 'react'
import { Terminal as XTerminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebglAddon } from '@xterm/addon-webgl'
import '@xterm/xterm/css/xterm.css'

const MSG_INPUT = 0
const MSG_RESIZE = 1
const MSG_PING = 2
const MSG_OUTPUT = 0

interface TerminalProps {
  sessionId: string
}

export function Terminal({ sessionId }: TerminalProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<XTerminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const fitRef = useRef<FitAddon | null>(null)

  useEffect(() => {
    if (!containerRef.current || !sessionId) return

    const term = new XTerminal({
      cursorBlink: true,
      fontSize: 14,
      fontFamily: 'Menlo, Monaco, "Courier New", monospace',
      theme: {
        background: '#0a0a0a',
        foreground: '#fafafa',
        cursor: '#fafafa',
        selectionBackground: '#3f3f46',
      },
    })

    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)

    term.open(containerRef.current)

    try {
      term.loadAddon(new WebglAddon())
    } catch {
      // WebGL not available, fall back to canvas
    }

    fitAddon.fit()
    termRef.current = term
    fitRef.current = fitAddon

    // WebSocket connection
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const ws = new WebSocket(`${protocol}//${window.location.host}/ws/terminal/${sessionId}`)
    ws.binaryType = 'arraybuffer'
    wsRef.current = ws

    ws.onopen = () => {
      // Send initial resize
      sendResize(ws, term.cols, term.rows)
    }

    ws.onmessage = (event) => {
      const data = new Uint8Array(event.data as ArrayBuffer)
      if (data.length === 0) return
      const msgType = data[0]
      const payload = data.slice(1)
      if (msgType === MSG_OUTPUT) {
        term.write(payload)
      }
    }

    ws.onclose = () => {
      term.write('\r\n\x1b[31m[Connection closed]\x1b[0m\r\n')
    }

    // Terminal input → WebSocket
    const inputDisposable = term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) {
        const encoded = new TextEncoder().encode(data)
        const msg = new Uint8Array(1 + encoded.length)
        msg[0] = MSG_INPUT
        msg.set(encoded, 1)
        ws.send(msg)
      }
    })

    // Resize handler
    const resizeDisposable = term.onResize(({ cols, rows }) => {
      if (ws.readyState === WebSocket.OPEN) {
        sendResize(ws, cols, rows)
      }
    })

    const handleWindowResize = () => fitAddon.fit()
    window.addEventListener('resize', handleWindowResize)

    // Ping interval
    const pingInterval = setInterval(() => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(new Uint8Array([MSG_PING]))
      }
    }, 30000)

    return () => {
      clearInterval(pingInterval)
      window.removeEventListener('resize', handleWindowResize)
      inputDisposable.dispose()
      resizeDisposable.dispose()
      ws.close()
      term.dispose()
      termRef.current = null
      wsRef.current = null
      fitRef.current = null
    }
  }, [sessionId])

  // Re-fit on container resize
  useEffect(() => {
    if (!containerRef.current || !fitRef.current) return
    const observer = new ResizeObserver(() => fitRef.current?.fit())
    observer.observe(containerRef.current)
    return () => observer.disconnect()
  }, [sessionId])

  return (
    <div
      ref={containerRef}
      className="h-full w-full"
      style={{ padding: '4px' }}
    />
  )
}

function sendResize(ws: WebSocket, cols: number, rows: number) {
  const msg = new Uint8Array(5)
  msg[0] = MSG_RESIZE
  msg[1] = (cols >> 8) & 0xff
  msg[2] = cols & 0xff
  msg[3] = (rows >> 8) & 0xff
  msg[4] = rows & 0xff
  ws.send(msg)
}
