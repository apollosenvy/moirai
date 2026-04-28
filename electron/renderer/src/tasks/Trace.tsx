import { useTasksStore } from '../store/tasksStore'
import type { TraceEvent } from '../lib/daemonClient'
import { parseDaemonTs } from '../lib/parseDaemonTs'

// TRACE panel: live event stream with filter chips. Rows are typed
// (phase / swap / llm / think / tool / err) and color-coded per the
// source stylesheet. Events come from tasksStore.detail.recent.
export default function Trace() {
  const recent = useTasksStore((s) => s.detail?.recent ?? [])

  return (
    <section className="tk-panel">
      <div className="tk-panel-hd">
        <span className="title">TRACE</span>
        <span className="sub">
          live stream · {recent.length.toLocaleString()} events
        </span>
        <div className="trace-filters">
          <span className="trace-filt on">PHASE</span>
          <span className="trace-filt on">SWAP</span>
          <span className="trace-filt on">LLM</span>
          <span className="trace-filt on">TOOL</span>
          <span className="trace-filt">THINK</span>
          <span className="trace-filt on">ERR</span>
        </div>
      </div>
      <div className="trace-body">
        {recent.length === 0 ? (
          <div
            style={{
              padding: '16px',
              color: 'var(--text-mute)',
              letterSpacing: '0.2em',
              fontSize: '11px',
            }}
          >
            NO TRACE EVENTS YET
          </div>
        ) : (
          recent.map((ev, i) => (
            <TraceRow
              key={`${ev.ts ?? ''}-${ev.kind ?? ''}-${i}`}
              event={ev}
            />
          ))
        )}

        <div className="trace-cursor">
          <span className="elapsed">+0 ms</span>
          <span className="dot-wrap">
            <span className="dot" />
          </span>
          <span className="bar">STREAM</span>
          <span style={{ color: 'var(--text-mute)', letterSpacing: '0.02em' }}>
            live
          </span>
        </div>
      </div>
    </section>
  )
}

function TraceRow({ event }: { event: TraceEvent }) {
  const kind = (event.kind || '').toLowerCase()
  const rowClass = rowClassFor(kind)
  const sc = deriveSc(event)
  const msg = deriveMsg(event)

  // LLM_CALL events carry a `head` field with the first ~400 chars of the
  // model's response (added in the orchestrator). Render it as a second
  // wrapped line under the summary row so it's visible without expanding.
  // This is the thing that tells you "why did gemma emit no tool call 23
  // turns in a row" -- the answer is literally the reasoning text.
  //
  // Wire invariant: head lives under event.data.head (orchestrator keeps it
  // nested). Some older trace events carried it top-level as event.head --
  // we fall back to that so historical traces still render.
  const head =
    typeof event.data?.head === 'string'
      ? event.data.head
      : typeof event.head === 'string'
        ? event.head
        : null

  return (
    <div className={`trace-row ${rowClass}`}>
      <span className="t">{formatTs(event.ts)}</span>
      <span className="sc">{sc}</span>
      <span className="kind">{(event.kind || 'EVT').toUpperCase()}</span>
      <span className="msg">{msg}</span>
      {head && (
        <div
          className="trace-head"
          style={{
            gridColumn: '3 / -1',
            marginTop: 2,
            paddingLeft: 8,
            color: 'var(--text-dim)',
            fontSize: 10,
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-word',
            opacity: 0.85,
          }}
        >
          {head}
        </div>
      )}
    </div>
  )
}

function rowClassFor(kind: string): string {
  if (kind.startsWith('phase')) return 'phase'
  if (kind.startsWith('swap')) return 'swap'
  if (kind.startsWith('llm')) return 'llm'
  if (kind.startsWith('think')) return 'think'
  if (kind.startsWith('tool')) return 'tool'
  if (kind.startsWith('err') || kind === 'error' || kind === 'fatal')
    return 'err'
  return ''
}

function formatTs(ts: unknown): string {
  if (typeof ts !== 'string' || !ts) return '--:--:--.---'
  const d = parseDaemonTs(ts)
  if (Number.isNaN(d.getTime())) return ts
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  const ss = String(d.getSeconds()).padStart(2, '0')
  const ms = String(d.getMilliseconds()).padStart(3, '0')
  return `${hh}:${mm}:${ss}.${ms}`
}

// Derive the source / slot tag shown in the `sc` column. The Go trace.Event
// only has {ts, task_id, kind, data, notes, raw}; routing info lives inside
// `data`. Look in the order the orchestrator populates these fields:
//   swap events -> data.to (e.g. {"to":"reviewer","reason":"ro_loop"})
//   some events -> data.slot (legacy / alt key)
//   llm_call    -> data.role (e.g. {"role":"reviewer","turn":3,...})
function deriveSc(event: TraceEvent): string {
  const data = event.data
  if (data) {
    if (typeof data.to === 'string' && data.to) return data.to
    if (typeof data.slot === 'string' && data.slot) return data.slot
    if (typeof data.role === 'string' && data.role) return data.role
  }
  return '·'
}

// Derive the `msg` column. Shape handling walks the orchestrator emit sites
// in internal/orchestrator/orchestrator.go. We special-case the common shapes
// and fall back to a compact JSON-ish summary of data.
function deriveMsg(event: TraceEvent): string {
  const kind = (event.kind || '').toLowerCase()
  const data = event.data
  if (!data) return describeEvent(event)

  // error / fatal -> surface the error text
  if (kind === 'error' || kind === 'fatal') {
    if (typeof data.fatal === 'string' && data.fatal) return data.fatal
    if (typeof data.error === 'string' && data.error) return data.error
  }

  // info events often use {"message": "..."} (abort path)
  if (typeof data.message === 'string' && data.message) return data.message

  // swap events -> "to <dest> (reason)"
  if (kind === 'swap' || kind.startsWith('swap')) {
    const to = typeof data.to === 'string' ? data.to : null
    const reason = typeof data.reason === 'string' ? data.reason : null
    if (to && reason) return `→ ${to} (${reason})`
    if (to) return `→ ${to}`
    if (reason) return reason
  }

  // phase events -> show the phase
  if (kind === 'phase' || kind.startsWith('phase')) {
    if (typeof data.phase === 'string' && data.phase) return data.phase
  }

  // llm_call -> "turn N · B bytes"
  if (kind === 'llm_call' || kind.startsWith('llm')) {
    const parts: string[] = []
    if (typeof data.turn === 'number') parts.push(`turn ${data.turn}`)
    if (typeof data.bytes === 'number') parts.push(`${data.bytes}B`)
    if (parts.length) return parts.join(' · ')
  }

  // tool_call -> "name · Nbytes" or just name
  if (kind === 'tool_call' || kind.startsWith('tool')) {
    const name = typeof data.name === 'string' ? data.name : null
    const bytes = typeof data.bytes === 'number' ? data.bytes : null
    const toolErr = typeof data.error === 'string' ? data.error : null
    const parts: string[] = []
    if (name) parts.push(name)
    if (typeof bytes === 'number') parts.push(`${bytes}B`)
    if (toolErr) parts.push(`err: ${toolErr}`)
    if (parts.length) return parts.join(' · ')
  }

  // done events
  if (kind === 'done') {
    if (typeof data.summary === 'string' && data.summary) return data.summary
  }

  return describeEvent(event)
}

// Compact description of an event for the fallback case. Strips the
// envelope fields (ts / task_id / kind / data) and formats the data blob
// as compact key=val pairs instead of a full JSON stringify.
function describeEvent(event: TraceEvent): string {
  const data = event.data
  if (data && typeof data === 'object') {
    const parts: string[] = []
    for (const [k, v] of Object.entries(data)) {
      if (k === 'head') continue // rendered separately
      if (v === null || v === undefined) continue
      const s = typeof v === 'string' ? v : JSON.stringify(v)
      const truncated = s.length > 80 ? s.slice(0, 77) + '…' : s
      parts.push(`${k}=${truncated}`)
      if (parts.length >= 4) break
    }
    if (parts.length) return parts.join(' ')
  }
  // Last-ditch fallback: strip known envelope fields and stringify.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  const { ts, kind, task_id, data: _data, ...rest } = event as Record<
    string,
    unknown
  > & { ts?: unknown; kind?: unknown; task_id?: unknown; data?: unknown }
  void ts
  void kind
  void task_id
  void _data
  return Object.keys(rest).length ? JSON.stringify(rest) : ''
}
