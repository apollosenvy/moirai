import { render } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import Trace from './Trace'
import { useTasksStore } from '../store/tasksStore'
import type { Task, TraceEvent } from '../lib/daemonClient'

function makeTask(): Task {
  return {
    id: 'T-TRC',
    status: 'running',
    phase: 'coding',
    iterations: 0,
    replans: 0,
    active_model: 'coder',
    repo_root: '/r',
    branch: 'main',
    description: 'desc',
    created_at: '2026-04-23T00:00:00Z',
    updated_at: '2026-04-23T00:00:00Z',
  }
}

function row(container: HTMLElement, idx = 0): HTMLElement {
  const rows = container.querySelectorAll('.trace-row')
  return rows[idx] as HTMLElement
}

function col(row: HTMLElement, cls: string): string {
  const el = row.querySelector(`.${cls}`)
  return el?.textContent ?? ''
}

describe('Trace', () => {
  beforeEach(() => {
    useTasksStore.setState({ list: [], selectedId: null, detail: null })
  })

  it('renders head from event.data.head', () => {
    const ev: TraceEvent = {
      ts: '2026-04-23T00:00:00.000Z',
      kind: 'llm_call',
      data: { role: 'coder', head: 'MODEL REPLY PREVIEW' },
    }
    useTasksStore.setState({
      detail: { task: makeTask(), recent: [ev] },
    })
    const { container } = render(<Trace />)
    const head = container.querySelector('.trace-head') as HTMLElement
    expect(head).toBeInTheDocument()
    expect(head.textContent).toBe('MODEL REPLY PREVIEW')
  })

  it('falls back to event.head when data.head is missing', () => {
    const ev: TraceEvent = {
      ts: '2026-04-23T00:00:00.000Z',
      kind: 'llm_call',
      data: { role: 'coder' },
      head: 'LEGACY TOP-LEVEL HEAD',
    }
    useTasksStore.setState({
      detail: { task: makeTask(), recent: [ev] },
    })
    const { container } = render(<Trace />)
    const head = container.querySelector('.trace-head') as HTMLElement
    expect(head).toBeInTheDocument()
    expect(head.textContent).toBe('LEGACY TOP-LEVEL HEAD')
  })

  it('does not render head line when absent from both places', () => {
    const ev: TraceEvent = {
      ts: '2026-04-23T00:00:00.000Z',
      kind: 'phase',
      data: { phase: 'ro_loop' },
    }
    useTasksStore.setState({
      detail: { task: makeTask(), recent: [ev] },
    })
    const { container } = render(<Trace />)
    expect(container.querySelector('.trace-head')).toBeNull()
  })

  it('uses composite keys so ring-eviction does not remount other rows', () => {
    const ev = (ts: string, msg: string): TraceEvent => ({
      ts,
      kind: 'llm',
      data: { role: 'coder', message: msg },
    })
    const initial = [
      ev('2026-04-23T00:00:00.000Z', 'a'),
      ev('2026-04-23T00:00:01.000Z', 'b'),
      ev('2026-04-23T00:00:02.000Z', 'c'),
    ]
    useTasksStore.setState({ detail: { task: makeTask(), recent: initial } })
    const { container, rerender } = render(<Trace />)
    const before = Array.from(
      container.querySelectorAll('.trace-row'),
    ) as HTMLElement[]
    expect(before.length).toBe(3)

    const next = [
      ev('2026-04-23T00:00:01.000Z', 'b'),
      ev('2026-04-23T00:00:02.000Z', 'c'),
      ev('2026-04-23T00:00:03.000Z', 'd'),
    ]
    useTasksStore.setState({ detail: { task: makeTask(), recent: next } })
    rerender(<Trace />)
    const after = Array.from(
      container.querySelectorAll('.trace-row'),
    ) as HTMLElement[]
    expect(after.length).toBe(3)
    expect(after[2].textContent).toMatch(/d/)
  })

  // F1: shape-specific tests walking the orchestrator emit sites.

  it('swap event: sc from data.to, msg mentions dest + reason', () => {
    const ev: TraceEvent = {
      ts: '2026-04-23T00:00:00.000Z',
      kind: 'swap',
      data: { to: 'reviewer', reason: 'ro_loop' },
    }
    useTasksStore.setState({
      detail: { task: makeTask(), recent: [ev] },
    })
    const { container } = render(<Trace />)
    const r = row(container)
    expect(col(r, 'sc')).toBe('reviewer')
    expect(col(r, 'msg')).toMatch(/reviewer/)
    expect(col(r, 'msg')).toMatch(/ro_loop/)
  })

  it('llm_call event: sc from data.role, msg has turn/bytes, head renders', () => {
    const ev: TraceEvent = {
      ts: '2026-04-23T00:00:00.000Z',
      kind: 'llm_call',
      data: {
        role: 'reviewer',
        turn: 3,
        bytes: 695,
        head: 'I think we should...',
      },
    }
    useTasksStore.setState({
      detail: { task: makeTask(), recent: [ev] },
    })
    const { container } = render(<Trace />)
    const r = row(container)
    expect(col(r, 'sc')).toBe('reviewer')
    expect(col(r, 'msg')).toMatch(/turn 3/)
    expect(col(r, 'msg')).toMatch(/695/)
    const head = container.querySelector('.trace-head') as HTMLElement
    expect(head).toBeInTheDocument()
    expect(head.textContent).toBe('I think we should...')
  })

  it('info event with data.message surfaces the message', () => {
    const ev: TraceEvent = {
      ts: '2026-04-23T00:00:00.000Z',
      kind: 'info',
      data: { message: 'aborted by user' },
    }
    useTasksStore.setState({
      detail: { task: makeTask(), recent: [ev] },
    })
    const { container } = render(<Trace />)
    const r = row(container)
    expect(col(r, 'msg')).toBe('aborted by user')
  })

  it('phase event: msg shows data.phase', () => {
    const ev: TraceEvent = {
      ts: '2026-04-23T00:00:00.000Z',
      kind: 'phase',
      data: { phase: 'ro_loop' },
    }
    useTasksStore.setState({
      detail: { task: makeTask(), recent: [ev] },
    })
    const { container } = render(<Trace />)
    const r = row(container)
    expect(col(r, 'msg')).toBe('ro_loop')
  })

  it('error event: msg has data.error', () => {
    const ev: TraceEvent = {
      ts: '2026-04-23T00:00:00.000Z',
      kind: 'error',
      data: { error: 'ensure reviewer failed' },
    }
    useTasksStore.setState({
      detail: { task: makeTask(), recent: [ev] },
    })
    const { container } = render(<Trace />)
    const r = row(container)
    expect(col(r, 'msg')).toBe('ensure reviewer failed')
  })

  it('fatal event: msg has data.fatal (via error kind row class)', () => {
    // KindError carries data.fatal per orchestrator.go:993.
    const ev: TraceEvent = {
      ts: '2026-04-23T00:00:00.000Z',
      kind: 'error',
      data: { fatal: 'context deadline exceeded' },
    }
    useTasksStore.setState({
      detail: { task: makeTask(), recent: [ev] },
    })
    const { container } = render(<Trace />)
    const r = row(container)
    expect(col(r, 'msg')).toBe('context deadline exceeded')
    // Row class should tag the err row.
    expect(r.className).toMatch(/err/)
  })

  it('swap event falls through to data.slot if data.to missing', () => {
    const ev: TraceEvent = {
      ts: '2026-04-23T00:00:00.000Z',
      kind: 'swap',
      data: { slot: 'planner', reason: 'warmup' },
    }
    useTasksStore.setState({
      detail: { task: makeTask(), recent: [ev] },
    })
    const { container } = render(<Trace />)
    const r = row(container)
    expect(col(r, 'sc')).toBe('planner')
  })

  it('unknown event shape: sc falls back to dot, msg is compact', () => {
    const ev: TraceEvent = {
      ts: '2026-04-23T00:00:00.000Z',
      kind: 'info',
      data: { branch: 'main' },
    }
    useTasksStore.setState({
      detail: { task: makeTask(), recent: [ev] },
    })
    const { container } = render(<Trace />)
    const r = row(container)
    expect(col(r, 'sc')).toBe('·')
    expect(col(r, 'msg')).toMatch(/branch=main/)
  })
})
