import StatusDot from '../chrome/StatusDot'
import Composer from './Composer'
import Plan from './Plan'
import Reviews from './Reviews'
import Trace from './Trace'
import { useDaemonStore } from '../store/daemonStore'
import { useTasksStore } from '../store/tasksStore'
import { createDaemonClient } from '../lib/daemonClient'
import { parseDaemonTs, parseDaemonTsMs } from '../lib/parseDaemonTs'
import { useEffect, useRef, useState } from 'react'

// TASK detail view: metadata bar across the top, then a split body
// with PLAN + COMPOSER on the left and REVIEWS + TRACE on the right.
// All metadata reads from tasksStore.detail; null detail renders null.
export default function TaskDetail() {
  const detail = useTasksStore((s) => s.detail)
  // Pull the iteration cap from /status so the fraction reflects the
  // daemon-config'd ceiling rather than a stale hardcoded /5. Falls back
  // to the orchestrator's compile-time default when the field is absent.
  const maxIter = useDaemonStore(
    (s) => (s.status as unknown as { max_ro_turns?: number })?.max_ro_turns,
  )
  const [busy, setBusy] = useState(false)
  // Synchronous guard: React's setBusy() won't re-render until after the
  // current event loop tick, so a fast double-click can land two handlers
  // inside the same tick with busy=false in both closures. A ref catches
  // the second click synchronously.
  const inFlightRef = useRef(false)

  // Cmd/Ctrl+. interrupt: global keydown listener wired to the running task.
  // The Composer hint advertises this shortcut; without the listener the
  // hint was a lie. Listener is installed once and short-circuits when the
  // task isn't running so it doesn't throw against null.
  const taskId = detail?.task?.id ?? null
  const isRunningForKey = detail?.task?.status === 'running'
  useEffect(() => {
    if (!taskId || !isRunningForKey) return
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === '.') {
        e.preventDefault()
        if (inFlightRef.current) return
        inFlightRef.current = true
        const client = createDaemonClient()
        client
          .interruptTask(taskId)
          .catch(() => {
            /* swallow -- interrupt result lands on the trace */
          })
          .finally(() => {
            inFlightRef.current = false
          })
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [taskId, isRunningForKey])

  if (!detail || !detail.task) return null
  const { task } = detail

  const abort = async () => {
    if (inFlightRef.current) return
    inFlightRef.current = true
    setBusy(true)
    try {
      const client = createDaemonClient()
      await client.abortTask(task.id)
    } catch {
      /* swallow -- aborted status will show up on next poll */
    } finally {
      inFlightRef.current = false
      setBusy(false)
    }
  }

  const interrupt = async () => {
    if (inFlightRef.current) return
    inFlightRef.current = true
    setBusy(true)
    try {
      const client = createDaemonClient()
      await client.interruptTask(task.id)
    } catch {
      /* swallow -- interrupt effect will show up in the trace */
    } finally {
      inFlightRef.current = false
      setBusy(false)
    }
  }

  const isRunning = task.status === 'running'

  const elapsed = formatElapsed(task.created_at, task.updated_at)

  return (
    <main className="task-main">
      {/* METADATA BAR */}
      <div className="tk-meta">
        <div className="cell id-cell">
          <div className="lbl">TASK</div>
          <div className="val">{task.id}</div>
        </div>

        <div className="cell status-cell">
          <div className="lbl">STATUS</div>
          <div className="val">
            <StatusDot
              variant={dotForStatus(task.status)}
              style={{
                boxShadow: '0 0 8px rgba(57,255,136,0.7)',
                animation: 'status-pulse 2s ease-in-out infinite',
              }}
            />
            {task.status.toUpperCase()}
          </div>
        </div>

        <div className="cell">
          <div className="lbl">PHASE</div>
          <div className="val">
            <span className="phase-chip">{task.phase.toUpperCase().replace('_', ' ')}</span>
          </div>
        </div>

        <div className="cell iter-cell">
          <div className="lbl">ITER · REPLANS</div>
          <div className="val">
            {task.iterations}
            {/* Show the daemon-reported cap when present; otherwise display
                the raw count without a fraction so we don't lie to the user
                about a /5 ceiling that doesn't exist. */}
            {typeof maxIter === 'number' && maxIter > 0 ? (
              <span className="frac">/{maxIter}</span>
            ) : null}{' '}
            · {task.replans}
          </div>
        </div>

        <div className="cell">
          <div className="lbl">ACTIVE SLOT</div>
          <div
            className="val"
            style={{
              color: 'var(--ice)',
              textShadow: '0 0 8px rgba(0,200,255,0.4)',
              letterSpacing: '0.22em',
              fontFamily: 'var(--ff-mono-display)',
              fontSize: '12px',
            }}
          >
            ■ {task.active_model || '--'}
          </div>
        </div>

        <div className="cell path">
          <div className="lbl">REPO · BRANCH</div>
          <div className="val">
            <span>{task.repo_root || '--'}</span>{' '}
            <span className="branch">@ {task.branch || '--'}</span>
          </div>
        </div>

        <div className="cell">
          <div className="lbl">STARTED</div>
          <div className="val">{formatTimestamp(task.created_at)}</div>
        </div>

        <div className="cell">
          <div className="lbl">ELAPSED</div>
          <div className="val" style={{ color: 'var(--lime)' }}>
            {elapsed}
          </div>
        </div>

        <div className="tk-actions">
          <button
            className="tk-btn warn"
            onClick={interrupt}
            disabled={busy || !isRunning}
            title={
              !isRunning
                ? `task is ${task.status}`
                : 'inject an interrupt message at the next RO turn'
            }
          >
            INTERRUPT
          </button>
          <button
            className="tk-btn"
            disabled
            title="pause/resume not implemented yet -- use INTERRUPT + ABORT for now"
            style={{ opacity: 0.4, cursor: 'not-allowed' }}
          >
            PAUSE
          </button>
          <button
            className="tk-btn danger"
            onClick={abort}
            disabled={busy || !isRunning}
            title={
              !isRunning
                ? `task is already ${task.status}`
                : 'stop this task cleanly; state persists for postmortem'
            }
          >
            ABORT
          </button>
        </div>
      </div>

      {/* BODY SPLIT */}
      <div className="tk-body">
        <div className="tk-left">
          <Plan />
          <Composer />
        </div>
        <div className="tk-right">
          <Reviews />
          <Trace />
        </div>
      </div>
    </main>
  )
}

function dotForStatus(status: string) {
  const s = status.toLowerCase()
  if (s === 'running' || s === 'executing') return 'lime' as const
  if (s === 'error' || s === 'failed') return 'magenta' as const
  if (s === 'waiting' || s === 'pending' || s === 'queued')
    return 'amber' as const
  return 'mute' as const
}

function formatTimestamp(ts: string): string {
  if (!ts) return '--'
  const d = parseDaemonTs(ts)
  if (Number.isNaN(d.getTime())) return '--'
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  const ss = String(d.getSeconds()).padStart(2, '0')
  return `${hh}:${mm}:${ss}`
}

function formatElapsed(start: string, end: string): string {
  const s = parseDaemonTsMs(start)
  const e = parseDaemonTsMs(end)
  if (Number.isNaN(s) || Number.isNaN(e)) return '--'
  const diff = Math.max(0, (e - s) / 1000)
  const h = Math.floor(diff / 3600)
  const m = Math.floor((diff % 3600) / 60)
  const sec = Math.floor(diff % 60)
  return `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}:${String(sec).padStart(2, '0')}`
}
