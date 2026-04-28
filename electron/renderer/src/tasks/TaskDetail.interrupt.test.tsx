import { fireEvent, render, act } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import TaskDetail from './TaskDetail'
import { useTasksStore } from '../store/tasksStore'
import type { Task } from '../lib/daemonClient'

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: 'T-INT',
    status: 'running',
    phase: 'coding',
    iterations: 1,
    replans: 0,
    active_model: 'coder-model',
    repo_root: '/r',
    branch: 'main',
    description: 'desc',
    created_at: '2026-04-23T00:00:00Z',
    updated_at: '2026-04-23T00:00:00Z',
    ...overrides,
  }
}

function findInterruptBtn(container: HTMLElement): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll('button'))
  const btn = buttons.find(
    (b) => (b.textContent ?? '').trim() === 'INTERRUPT',
  ) as HTMLButtonElement | undefined
  if (!btn) throw new Error('INTERRUPT button not found')
  return btn
}

describe('TaskDetail INTERRUPT', () => {
  let invoke: ReturnType<typeof vi.fn>

  beforeEach(() => {
    useTasksStore.setState({ list: [], selectedId: null, detail: null })
    invoke = vi.fn().mockResolvedValue({
      status: 200,
      body: JSON.stringify({ interrupted: 'T-INT' }),
    })
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    ;(window as any).moirai = { invoke, on: () => () => {} }
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('clicking INTERRUPT when running fires client.interruptTask', async () => {
    useTasksStore.setState({
      detail: { task: makeTask({ status: 'running' }), recent: [] },
    })
    const { container } = render(<TaskDetail />)
    const btn = findInterruptBtn(container)
    expect(btn.disabled).toBe(false)
    await act(async () => {
      fireEvent.click(btn)
      await Promise.resolve()
    })
    expect(invoke).toHaveBeenCalledWith('daemon-call', {
      method: 'POST',
      path: '/tasks/T-INT/interrupt',
      body: undefined,
    })
  })

  it('INTERRUPT is disabled when task is not running', () => {
    useTasksStore.setState({
      detail: { task: makeTask({ status: 'done' }), recent: [] },
    })
    const { container } = render(<TaskDetail />)
    const btn = findInterruptBtn(container)
    expect(btn.disabled).toBe(true)
    expect(btn.title).toMatch(/task is done/i)
  })

  it('INTERRUPT tooltip reflects current status', () => {
    useTasksStore.setState({
      detail: { task: makeTask({ status: 'queued' }), recent: [] },
    })
    const { container } = render(<TaskDetail />)
    const btn = findInterruptBtn(container)
    expect(btn.title).toMatch(/task is queued/i)
  })

  it('busy guard prevents double-click firing interruptTask twice', async () => {
    // Make the invoke pending indefinitely so busy stays true.
    let resolvePending: (v: unknown) => void = () => {}
    const pending = new Promise((r) => {
      resolvePending = r
    })
    invoke.mockReturnValueOnce(pending)

    useTasksStore.setState({
      detail: { task: makeTask({ status: 'running' }), recent: [] },
    })
    const { container } = render(<TaskDetail />)
    const btn = findInterruptBtn(container)

    await act(async () => {
      fireEvent.click(btn)
      fireEvent.click(btn)
      await Promise.resolve()
    })
    expect(invoke).toHaveBeenCalledTimes(1)

    await act(async () => {
      resolvePending({ status: 200, body: JSON.stringify({ interrupted: 'T-INT' }) })
      await Promise.resolve()
      await Promise.resolve()
    })
  })
})
