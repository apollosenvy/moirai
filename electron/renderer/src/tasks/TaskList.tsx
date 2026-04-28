import StatusDot, { type StatusDotVariant } from '../chrome/StatusDot'
import type { Task } from '../lib/daemonClient'
import { useTasksStore } from '../store/tasksStore'
import { useClickableDiv } from '../lib/a11y'
import { parseDaemonTsMs } from '../lib/parseDaemonTs'

// Right side of the bottom row: notched-rect panel with sticky header,
// column header row, and the scrollable task rows. Data comes from
// tasksStore.list; row clicks drive tasksStore.selectTask.
export default function TaskList() {
  const tasks = useTasksStore((s) => s.list)
  const selectedId = useTasksStore((s) => s.selectedId)
  const selectTask = useTasksStore((s) => s.selectTask)

  const counts = countStatuses(tasks)

  return (
    <div className="tasks-wrap notch">
      <svg className="notch-border" viewBox="0 0 1000 298" preserveAspectRatio="none">
        <path d="M0,0 L990,0 L1000,10 L1000,298 L0,298 Z" />
      </svg>

      <div className="tasks-head">
        <div className="t">TASKS</div>
        <div className="meta">
          <b>{counts.run}</b> RUN <span style={{ color: 'var(--rule)' }}>·</span>{' '}
          <span style={{ color: 'var(--amber)' }}>{counts.wait}</span> WAIT{' '}
          <span style={{ color: 'var(--rule)' }}>·</span>{' '}
          <span style={{ color: 'var(--text-mute)' }}>{counts.done}</span> DONE{' '}
          <span style={{ color: 'var(--rule)' }}>·</span>{' '}
          <span style={{ color: 'var(--magenta)' }}>{counts.err}</span> ERR
        </div>
      </div>

      <div className="tasks-cols">
        <div>WHEN</div>
        <div>ID</div>
        <div>STATUS</div>
        <div>PHASE</div>
        <div style={{ textAlign: 'center' }}>ITR</div>
        <div>DESCRIPTION</div>
      </div>

      <div className="tasks-body">
        {tasks.length === 0 ? (
          <div
            style={{
              padding: '24px 16px',
              color: 'var(--text-mute)',
              letterSpacing: '0.2em',
              fontSize: '11px',
            }}
          >
            NO TASKS · SUBMIT ONE ON THE LEFT
          </div>
        ) : (
          tasks.map((t) => (
            <TaskRow
              key={t.id}
              task={t}
              selected={t.id === selectedId}
              onSelect={() => selectTask(t.id)}
            />
          ))
        )}
      </div>
    </div>
  )
}

interface TaskRowProps {
  task: Task
  selected: boolean
  onSelect: () => void
}

function TaskRow({ task, selected, onSelect }: TaskRowProps) {
  const clickable = useClickableDiv(onSelect)
  const done = isDone(task)
  const row = deriveRowCosmetics(task)
  const cls = [
    'task-row',
    selected ? 'sel' : '',
    done ? 'done' : '',
  ]
    .filter(Boolean)
    .join(' ')
  return (
    <div
      {...clickable}
      aria-pressed={selected}
      className={cls}
      style={{ cursor: 'pointer' }}
    >
      <div className="when">{row.when}</div>
      <div
        className="id"
        title={task.id}
        style={{
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
          minWidth: 0,
        }}
      >
        {task.id}
      </div>
      <div
        className="status"
        style={{ minWidth: 0, whiteSpace: 'nowrap' }}
      >
        <StatusDot variant={row.statusDot} />
        <span style={{ color: row.statusColor }}>{row.statusText}</span>
      </div>
      <div className="phase">{phaseLabel(task.phase)}</div>
      <div className="iter">{task.iterations}</div>
      <div className="desc">{task.description}</div>
    </div>
  )
}

interface StatusCounts {
  run: number
  wait: number
  done: number
  err: number
}

// Canonical status buckets. Backend taskstore.Status values are:
//   pending, running, awaiting_user, succeeded, failed, aborted, interrupted
// Plus the synonyms the UI used historically (waiting / queued / done /
// completed / executing / error) which we keep for defence-in-depth.
export type StatusBucket = 'run' | 'wait' | 'done' | 'err' | 'unknown'

export function statusBucket(status: string, phase?: string): StatusBucket {
  const s = (status || '').toLowerCase()
  if (s === 'running' || s === 'executing') return 'run'
  if (
    s === 'pending' ||
    s === 'waiting' ||
    s === 'queued' ||
    s === 'awaiting_user'
  )
    return 'wait'
  if (s === 'done' || s === 'completed' || s === 'succeeded') return 'done'
  if (
    s === 'error' ||
    s === 'failed' ||
    s === 'aborted' ||
    s === 'interrupted'
  )
    return 'err'
  // Phase-based fallback: older records may not have populated status
  // consistently; phase === 'done' still implies done.
  if (phase === 'done') return 'done'
  return 'unknown'
}

function countStatuses(tasks: Task[]): StatusCounts {
  const counts: StatusCounts = { run: 0, wait: 0, done: 0, err: 0 }
  for (const t of tasks) {
    const b = statusBucket(t.status, t.phase)
    if (b === 'run') counts.run += 1
    else if (b === 'wait') counts.wait += 1
    else if (b === 'err') counts.err += 1
    else if (b === 'done') counts.done += 1
  }
  return counts
}

function isDone(t: Task): boolean {
  return statusBucket(t.status, t.phase) === 'done'
}

function phaseLabel(phase: string): string {
  return phase.toUpperCase().replace('_', ' ')
}

interface RowCosmetics {
  when: string
  statusDot: StatusDotVariant
  statusText: string
  statusColor: string
}

function deriveRowCosmetics(t: Task): RowCosmetics {
  const when = formatAgo(t.updated_at ?? t.created_at)
  const s = (t.status || '').toLowerCase()
  const bucket = statusBucket(t.status, t.phase)
  if (bucket === 'run') {
    return {
      when,
      statusDot: 'lime',
      statusText: s === 'executing' ? 'EXECUTING' : 'RUNNING',
      statusColor: 'var(--lime)',
    }
  }
  if (bucket === 'wait') {
    return {
      when,
      statusDot: 'amber',
      statusText:
        s === 'awaiting_user'
          ? 'AWAITING USER'
          : s === 'queued'
            ? 'QUEUED'
            : s === 'pending'
              ? 'PENDING'
              : 'WAITING',
      statusColor: 'var(--amber)',
    }
  }
  if (bucket === 'err') {
    return {
      when,
      statusDot: 'magenta',
      statusText:
        s === 'aborted'
          ? 'ABORTED'
          : s === 'interrupted'
            ? 'INTERRUPTED'
            : s === 'failed'
              ? 'FAILED'
              : 'ERROR',
      statusColor: 'var(--magenta)',
    }
  }
  if (bucket === 'done') {
    return {
      when,
      statusDot: 'mute',
      statusText: s === 'succeeded' ? 'SUCCEEDED' : 'DONE',
      statusColor: 'var(--text-mute)',
    }
  }
  return {
    when,
    statusDot: 'mute',
    statusText: s.toUpperCase() || 'UNKNOWN',
    statusColor: 'var(--text-mute)',
  }
}

function formatAgo(ts: string): string {
  if (!ts) return '--'
  const then = parseDaemonTsMs(ts)
  if (Number.isNaN(then)) return '--'
  const diffSec = Math.max(0, (Date.now() - then) / 1000)
  const mm = Math.floor(diffSec / 60)
  const ss = Math.floor(diffSec % 60)
  return `${String(mm).padStart(2, '0')}:${String(ss).padStart(2, '0')} AGO`
}
