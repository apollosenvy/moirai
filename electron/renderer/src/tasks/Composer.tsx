import { useEffect, useRef, useState } from 'react'
import { useTasksStore } from '../store/tasksStore'
import { createDaemonClient } from '../lib/daemonClient'

// INJECT composer: textarea + SEND button that hands free-form guidance
// to the orchestrator for the currently-selected task. The message
// lands as a user-role turn at the top of the next RO iteration via
// the /tasks/<id>/inject endpoint. Safe to fire mid-task; it does NOT
// abort or replan -- see Orchestrator.Inject for the wire contract.
//
// Keyboard: Cmd/Ctrl+Enter submits. Disabled when no task is selected
// or the task isn't running (backend rejects non-running tasks with a
// 400, but we guard here too to avoid the round-trip).
export default function Composer() {
  const task = useTasksStore((s) => s.detail?.task)
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [status, setStatus] = useState<string | null>(null)

  // Track the 'queued' auto-clear timeout so unmount + re-fire don't leak
  // timers or call setState after the component is gone.
  const statusTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      if (statusTimerRef.current) {
        clearTimeout(statusTimerRef.current)
        statusTimerRef.current = null
      }
    }
  }, [])

  const canSend =
    !!task &&
    task.status === 'running' &&
    text.trim().length > 0 &&
    !busy

  const send = async () => {
    if (!task || !canSend) return
    setBusy(true)
    setStatus(null)
    try {
      const client = createDaemonClient()
      await client.injectGuidance(task.id, text.trim())
      if (!mountedRef.current) return
      setText('')
      setStatus('queued')
      if (statusTimerRef.current) clearTimeout(statusTimerRef.current)
      statusTimerRef.current = setTimeout(() => {
        if (mountedRef.current) setStatus(null)
        statusTimerRef.current = null
      }, 2000)
    } catch (err) {
      if (mountedRef.current) setStatus((err as Error).message)
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  const onKey = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      send()
    }
  }

  const disabledReason = !task
    ? 'no task selected'
    : task.status !== 'running'
      ? `task is ${task.status}`
      : null

  return (
    <div className="composer">
      <div className="composer-hd" id="composer-label">
        <span className="title">INJECT</span>
        <span className="sub">
          send guidance to active slot · steers without aborting
        </span>
      </div>
      <div className="composer-body">
        <textarea
          id="composer-input"
          aria-labelledby="composer-label"
          className="composer-ta"
          placeholder={
            disabledReason
              ? `(${disabledReason})`
              : 'Nudge the agent. Message is inserted at the next tool boundary...'
          }
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={onKey}
          disabled={!task || task.status !== 'running' || busy}
        />
        <button
          className="composer-send"
          onClick={send}
          disabled={!canSend}
          style={{
            opacity: canSend ? 1 : 0.4,
            cursor: canSend ? 'pointer' : 'not-allowed',
          }}
        >
          {busy ? 'SEND...' : 'SEND'}
          <br />
          <span style={{ fontSize: '8px', opacity: 0.7 }}>⌘↵</span>
        </button>
      </div>
      <div className="composer-hint">
        <span>
          <span className="kbd">⌘ ↵</span> SEND
        </span>
        <span>
          <span className="kbd">⌘ .</span> INTERRUPT
        </span>
        <span style={{ marginLeft: 'auto', color: 'var(--text-mute)' }}>
          {status
            ? status === 'queued'
              ? 'QUEUED · fires next RO turn'
              : `ERROR: ${status}`
            : 'INJECTS AT NEXT TOOL BOUNDARY · NOT MID-TOKEN'}
        </span>
      </div>
    </div>
  )
}
