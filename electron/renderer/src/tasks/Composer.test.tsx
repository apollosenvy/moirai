import { fireEvent, render, act } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import Composer from './Composer'
import { useTasksStore } from '../store/tasksStore'
import type { Task } from '../lib/daemonClient'

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: 'T-1',
    status: 'running',
    phase: 'coding',
    iterations: 0,
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

describe('Composer', () => {
  let invoke: ReturnType<typeof vi.fn>

  beforeEach(() => {
    useTasksStore.setState({ list: [], selectedId: null, detail: null })
    invoke = vi.fn().mockResolvedValue({
      status: 200,
      body: JSON.stringify({ injected: 'T-1' }),
    })
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    ;(window as any).moirai = { invoke, on: () => () => {} }
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('SEND button is disabled when no task is selected', () => {
    const { container } = render(<Composer />)
    const btn = container.querySelector('.composer-send') as HTMLButtonElement
    expect(btn).toBeInTheDocument()
    expect(btn.disabled).toBe(true)
  })

  it('SEND button is disabled when task is not running', () => {
    useTasksStore.setState({
      detail: { task: makeTask({ status: 'done' }), recent: [] },
    })
    const { container } = render(<Composer />)
    const btn = container.querySelector('.composer-send') as HTMLButtonElement
    const ta = container.querySelector('.composer-ta') as HTMLTextAreaElement
    expect(ta.disabled).toBe(true)
    fireEvent.change(ta, { target: { value: 'hi' } })
    expect(btn.disabled).toBe(true)
  })

  it('SEND button is disabled when textarea is empty', () => {
    useTasksStore.setState({
      detail: { task: makeTask({ status: 'running' }), recent: [] },
    })
    const { container } = render(<Composer />)
    const btn = container.querySelector('.composer-send') as HTMLButtonElement
    // empty/whitespace text should keep it disabled
    const ta = container.querySelector('.composer-ta') as HTMLTextAreaElement
    fireEvent.change(ta, { target: { value: '   ' } })
    expect(btn.disabled).toBe(true)
  })

  it('placeholder shows the disabled reason', () => {
    // no task
    const { container, rerender } = render(<Composer />)
    let ta = container.querySelector('.composer-ta') as HTMLTextAreaElement
    expect(ta.placeholder).toMatch(/no task selected/i)

    // task present but not running
    useTasksStore.setState({
      detail: { task: makeTask({ status: 'queued' }), recent: [] },
    })
    rerender(<Composer />)
    ta = container.querySelector('.composer-ta') as HTMLTextAreaElement
    expect(ta.placeholder).toMatch(/task is queued/i)
  })

  it('Cmd+Enter submits the message via injectGuidance', async () => {
    useTasksStore.setState({
      detail: { task: makeTask({ id: 'T-9', status: 'running' }), recent: [] },
    })
    const { container } = render(<Composer />)
    const ta = container.querySelector('.composer-ta') as HTMLTextAreaElement
    fireEvent.change(ta, { target: { value: 'steer left' } })
    await act(async () => {
      fireEvent.keyDown(ta, { key: 'Enter', metaKey: true })
    })
    expect(invoke).toHaveBeenCalledWith('daemon-call', {
      method: 'POST',
      path: '/tasks/T-9/inject',
      body: { message: 'steer left' },
    })
  })

  it('status rotates queued -> null after 2s', async () => {
    vi.useFakeTimers()
    useTasksStore.setState({
      detail: { task: makeTask({ id: 'T-9', status: 'running' }), recent: [] },
    })
    const { container } = render(<Composer />)
    const ta = container.querySelector('.composer-ta') as HTMLTextAreaElement
    fireEvent.change(ta, { target: { value: 'go' } })
    await act(async () => {
      fireEvent.keyDown(ta, { key: 'Enter', metaKey: true })
      // flush microtasks from the awaited injectGuidance
      await Promise.resolve()
      await Promise.resolve()
    })
    const hint = container.querySelector('.composer-hint') as HTMLElement
    expect(hint.textContent).toMatch(/QUEUED/)
    await act(async () => {
      vi.advanceTimersByTime(2001)
    })
    expect(hint.textContent).not.toMatch(/QUEUED/)
  })

  it('surfaces injectGuidance error in the hint line', async () => {
    invoke.mockReset()
    invoke.mockResolvedValueOnce({
      status: 400,
      body: JSON.stringify({ error: 'not running' }),
    })
    useTasksStore.setState({
      detail: { task: makeTask({ id: 'T-9', status: 'running' }), recent: [] },
    })
    const { container } = render(<Composer />)
    const ta = container.querySelector('.composer-ta') as HTMLTextAreaElement
    fireEvent.change(ta, { target: { value: 'go' } })
    await act(async () => {
      fireEvent.keyDown(ta, { key: 'Enter', metaKey: true })
      await Promise.resolve()
      await Promise.resolve()
    })
    const hint = container.querySelector('.composer-hint') as HTMLElement
    expect(hint.textContent).toMatch(/ERROR/)
  })

  it('associates the INJECT label with the textarea via aria-labelledby', () => {
    useTasksStore.setState({
      detail: { task: makeTask({ id: 'T-10', status: 'running' }), recent: [] },
    })
    const { container } = render(<Composer />)
    const ta = container.querySelector('.composer-ta') as HTMLTextAreaElement
    const labelledBy = ta.getAttribute('aria-labelledby')
    expect(labelledBy).toBe('composer-label')
    const label = document.getElementById(labelledBy!)
    expect(label).not.toBeNull()
    expect(label!.textContent).toMatch(/INJECT/)
  })
})
