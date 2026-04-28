import { useMemo } from 'react'
import Chip from '../chrome/Chip'
import RectPanel from '../chrome/RectPanel'
import StatusDot from '../chrome/StatusDot'
import { useDaemonStore } from '../store/daemonStore'
import { useTasksStore } from '../store/tasksStore'
import {
  displayGlyphForSlot,
  formatCtx,
  slotLetter,
  verdictChipVariant,
  verdictLabel,
} from '../lib/slotView'
import type { SlotView, Task } from '../lib/daemonClient'

// METRICS view. Aggregates everything we already poll from the daemon
// (/status + /slots + /tasks) into a single dense readout. No charts
// yet -- the brief calls those "future" -- but every number a user
// would normally have to grep `tail -f` for now lives in one panel.
// Recomputed each render, no extra polling cost.

const SLOT_ORDER: ReadonlyArray<'planner' | 'coder' | 'reviewer'> = [
  'planner',
  'coder',
  'reviewer',
]

// Reusable notch-panel SVG path for a 800x240 viewBox. The notch corner
// itself is fixed-pixel (10x10) so the bevel scale stays constant under
// preserveAspectRatio="none".
const NOTCH_VIEWBOX = '0 0 800 240'
const NOTCH_PATH = 'M0,0 L790,0 L800,10 L800,240 L0,240 Z'

interface PhaseTally {
  init: number
  planning: number
  plan_review: number
  coding: number
  code_review: number
  revise: number
  done: number
  other: number
}

interface StatusTally {
  running: number
  queued: number
  done: number
  failed: number
  aborted: number
  other: number
}

function tallyTasks(tasks: Task[]): {
  status: StatusTally
  phase: PhaseTally
  total: number
} {
  const status: StatusTally = {
    running: 0,
    queued: 0,
    done: 0,
    failed: 0,
    aborted: 0,
    other: 0,
  }
  const phase: PhaseTally = {
    init: 0,
    planning: 0,
    plan_review: 0,
    coding: 0,
    code_review: 0,
    revise: 0,
    done: 0,
    other: 0,
  }
  for (const t of tasks) {
    const s = (t.status || '').toLowerCase()
    if (s === 'running' || s === 'active') status.running += 1
    else if (s === 'queued' || s === 'pending' || s === 'waiting') status.queued += 1
    else if (s === 'done' || s === 'complete' || s === 'completed') status.done += 1
    else if (s === 'failed' || s === 'error') status.failed += 1
    else if (s === 'aborted' || s === 'cancelled' || s === 'canceled') status.aborted += 1
    else status.other += 1

    const p = (t.phase || '') as keyof PhaseTally
    if (p in phase) phase[p] += 1
    else phase.other += 1
  }
  return { status, phase, total: tasks.length }
}

function pendingFieldsCount(slot: SlotView): number {
  const p = slot.pending_changes
  if (!p) return 0
  let n = 0
  if (p.model_path !== undefined) n += 1
  if (p.ctx_size !== undefined) n += 1
  if (p.kv_cache !== undefined) n += 1
  return n
}

function pctOrDash(num: number | undefined, den: number | undefined): string {
  if (num === undefined || den === undefined || den === 0) return '--'
  const v = (num / den) * 100
  if (!Number.isFinite(v)) return '--'
  return `${v.toFixed(1)}%`
}

function mb(n: number | undefined): string {
  if (n === undefined) return '--'
  if (n >= 1024) return `${(n / 1024).toFixed(1)} GB`
  return `${n} MB`
}

export default function MetricsView() {
  const status = useDaemonStore((s) => s.status)
  const slots = useDaemonStore((s) => s.slots)
  const list = useTasksStore((s) => s.list)

  const tally = useMemo(() => tallyTasks(list), [list])

  const orderedSlots = useMemo(() => {
    const byKey = new Map<string, SlotView>()
    for (const s of slots) byKey.set(s.slot, s)
    return SLOT_ORDER.map((k) => byKey.get(k) ?? null)
  }, [slots])

  const activeSlot = slots.find((s) => s.loaded) ?? null
  const generating = slots.find((s) => s.generating) ?? null
  const vramUsed = status?.vram_used_mb
  const vramTotal = status?.vram_total_mb
  const vramPct =
    vramUsed !== undefined && vramTotal !== undefined && vramTotal > 0
      ? Math.min(100, (vramUsed / vramTotal) * 100)
      : 0

  return (
    <div className="metrics-main">
      <div className="metrics-grid">
        {/* DAEMON CARD */}
        <RectPanel className="metrics-card" viewBox={NOTCH_VIEWBOX} borderPath={NOTCH_PATH}>
          <header className="metrics-card-hd">
            <span>DAEMON</span>
            <StatusDot variant={status ? 'lime' : 'magenta'} />
          </header>
          <dl className="metrics-list">
            <div><dt>VERSION</dt><dd>{status?.daemon_version ?? '--'}</dd></div>
            <div><dt>PORT</dt><dd>{status?.port ?? '--'}</dd></div>
            <div><dt>UPTIME</dt><dd>{status?.uptime ?? '--'}</dd></div>
            <div><dt>STARTED</dt><dd className="metrics-dim">{status?.started_at ?? '--'}</dd></div>
            <div>
              <dt>TURBOQUANT</dt>
              <dd>
                {status?.turboquant_supported === true && <span className="metrics-ok">YES</span>}
                {status?.turboquant_supported === false && <span className="metrics-warn">NO</span>}
                {status?.turboquant_supported === undefined && '--'}
              </dd>
            </div>
            <div>
              <dt>RO TURNS CAP</dt>
              <dd>{status?.max_ro_turns ?? '--'}</dd>
            </div>
            <div>
              <dt>CORRUPT TASKS</dt>
              <dd className={status?.corrupt_task_count ? 'metrics-warn' : ''}>
                {status?.corrupt_task_count ?? 0}
              </dd>
            </div>
          </dl>
        </RectPanel>

        {/* VRAM CARD */}
        <RectPanel className="metrics-card" viewBox={NOTCH_VIEWBOX} borderPath={NOTCH_PATH}>
          <header className="metrics-card-hd">
            <span>VRAM</span>
            <StatusDot variant={vramUsed !== undefined ? 'ice' : 'ice-dim'} />
          </header>
          <div className="metrics-vram">
            <div className="metrics-vram-num">
              {mb(vramUsed)}
              <span className="metrics-vram-sep"> / </span>
              <span className="metrics-dim">{mb(vramTotal)}</span>
            </div>
            <div className="metrics-vram-bar" role="progressbar" aria-valuenow={vramPct} aria-valuemin={0} aria-valuemax={100}>
              <div className="metrics-vram-fill" style={{ width: `${vramPct}%` }} />
            </div>
            <div className="metrics-vram-pct">{pctOrDash(vramUsed, vramTotal)}</div>
          </div>
          <dl className="metrics-list">
            <div>
              <dt>ACTIVE SLOT</dt>
              <dd>
                {activeSlot
                  ? `${slotLetter(activeSlot)} · ${displayGlyphForSlot(activeSlot)}`
                  : '--'}
              </dd>
            </div>
            <div>
              <dt>GENERATING</dt>
              <dd>{generating ? `${slotLetter(generating)} (live)` : 'idle'}</dd>
            </div>
            <div>
              <dt>LAST VERDICT</dt>
              <dd>
                <Chip variant={verdictChipVariant(status?.last_verdict ?? null)}>
                  {verdictLabel(status?.last_verdict ?? null)}
                </Chip>
              </dd>
            </div>
          </dl>
        </RectPanel>

        {/* TASKS CARD */}
        <RectPanel className="metrics-card" viewBox={NOTCH_VIEWBOX} borderPath={NOTCH_PATH}>
          <header className="metrics-card-hd">
            <span>TASKS</span>
            <span className="metrics-total">{tally.total}</span>
          </header>
          <div className="metrics-tally">
            <div className="metrics-tally-row">
              <StatusDot variant="lime" />
              <span className="metrics-tally-lbl">RUNNING</span>
              <span className="metrics-tally-num">{tally.status.running}</span>
            </div>
            <div className="metrics-tally-row">
              <StatusDot variant="ice-dim" />
              <span className="metrics-tally-lbl">QUEUED</span>
              <span className="metrics-tally-num">{tally.status.queued}</span>
            </div>
            <div className="metrics-tally-row">
              <StatusDot variant="mute" />
              <span className="metrics-tally-lbl">DONE</span>
              <span className="metrics-tally-num">{tally.status.done}</span>
            </div>
            <div className="metrics-tally-row">
              <StatusDot variant="magenta" />
              <span className="metrics-tally-lbl">FAILED</span>
              <span className="metrics-tally-num">{tally.status.failed}</span>
            </div>
            <div className="metrics-tally-row">
              <StatusDot variant="amber" />
              <span className="metrics-tally-lbl">ABORTED</span>
              <span className="metrics-tally-num">{tally.status.aborted}</span>
            </div>
            {tally.status.other > 0 && (
              <div className="metrics-tally-row">
                <StatusDot variant="ice-dim" />
                <span className="metrics-tally-lbl">OTHER</span>
                <span className="metrics-tally-num">{tally.status.other}</span>
              </div>
            )}
          </div>

          <div className="metrics-phase-hd">PHASE BREAKDOWN</div>
          <div className="metrics-phase">
            {(['init', 'planning', 'plan_review', 'coding', 'code_review', 'revise', 'done'] as const).map(
              (p) => (
                <div key={p} className="metrics-phase-cell">
                  <div className="metrics-phase-num">{tally.phase[p]}</div>
                  <div className="metrics-phase-lbl">{p.replace('_', ' ').toUpperCase()}</div>
                </div>
              ),
            )}
          </div>
        </RectPanel>

        {/* SLOTS CARD */}
        <RectPanel
          className="metrics-card metrics-card-wide"
          viewBox={NOTCH_VIEWBOX}
          borderPath={NOTCH_PATH}
        >
          <header className="metrics-card-hd">
            <span>SLOTS</span>
            <span className="metrics-total">{slots.length} / 3</span>
          </header>
          <table className="metrics-slot-table">
            <thead>
              <tr>
                <th>ROLE</th>
                <th>MODEL</th>
                <th>CTX</th>
                <th>KV</th>
                <th>PORT</th>
                <th>STATE</th>
                <th>PENDING</th>
              </tr>
            </thead>
            <tbody>
              {orderedSlots.map((slot, i) => {
                const key = SLOT_ORDER[i]
                if (!slot) {
                  return (
                    <tr key={key} className="metrics-slot-empty">
                      <td>{key.toUpperCase()}</td>
                      <td colSpan={6} className="metrics-dim">slot not reported by daemon</td>
                    </tr>
                  )
                }
                const pending = pendingFieldsCount(slot)
                const stateLabel = slot.generating
                  ? 'GEN'
                  : slot.loaded
                    ? 'VRAM'
                    : 'DRAM'
                const stateClass = slot.generating
                  ? 'metrics-ok'
                  : slot.loaded
                    ? 'metrics-ice'
                    : 'metrics-dim'
                return (
                  <tr key={slot.slot}>
                    <td>
                      {slotLetter(slot)} · {displayGlyphForSlot(slot)}
                    </td>
                    <td className="metrics-slot-model" title={slot.model_path}>
                      {slot.model_name || '--'}
                    </td>
                    <td className="num">{formatCtx(slot.ctx_size)}</td>
                    <td>{slot.kv_cache || '--'}</td>
                    <td className="num">{slot.listen_port || '--'}</td>
                    <td className={stateClass}>{stateLabel}</td>
                    <td className={pending > 0 ? 'metrics-warn' : 'metrics-dim'}>
                      {pending > 0 ? `${pending} field${pending === 1 ? '' : 's'}` : '--'}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </RectPanel>
      </div>
    </div>
  )
}
