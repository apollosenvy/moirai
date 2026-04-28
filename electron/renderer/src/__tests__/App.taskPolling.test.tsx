import { render, act } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from '../App'
import { useDaemonStore } from '../store/daemonStore'
import { useTasksStore } from '../store/tasksStore'
import { useModelsStore } from '../store/modelsStore'
import { usePendingStore } from '../store/pendingStore'
import type { DaemonStatus, Task } from '../lib/daemonClient'

function resetStores() {
  useDaemonStore.setState({
    connected: false,
    ready: false,
    status: null,
    slots: [],
    swapId: 0,
  })
  useTasksStore.setState({ list: [], selectedId: null, detail: null })
  useModelsStore.setState({ list: [] })
  usePendingStore.setState({ pending: {} })
}

function makeStatus(): DaemonStatus {
  return {
    service: 'agent-router',
    port: 5984,
    active_slot: 'coder',
    active_port: 6001,
    task_count: 1,
    running: 1,
    last_verdict: null,
    turboquant_supported: true,
    daemon_version: 'v0.4.2',
    started_at: '2026-04-23T00:00:00Z',
    uptime: '00:00:05',
  }
}

function makeTask(): Task {
  return {
    id: 'T-POLL',
    status: 'running',
    phase: 'coding',
    iterations: 1,
    replans: 0,
    active_model: 'coder',
    repo_root: '/r',
    branch: 'main',
    description: 'hi',
    created_at: '2026-04-23T00:00:00Z',
    updated_at: '2026-04-23T00:00:00Z',
  }
}

describe('App TaskDetail polling (C1)', () => {
  let invoke: ReturnType<typeof vi.fn>

  beforeEach(() => {
    resetStores()
    vi.useFakeTimers()
    invoke = vi.fn().mockImplementation((_ch: string, payload: unknown) => {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const p = payload as any
      if (p?.path === '/ready') {
        return Promise.resolve({ status: 200, body: '' })
      }
      if (p?.path === '/slots') {
        return Promise.resolve({ status: 200, body: '[]' })
      }
      if (p?.path === '/tasks') {
        return Promise.resolve({
          status: 200,
          body: JSON.stringify([makeTask()]),
        })
      }
      if (p?.path === '/tasks/T-POLL') {
        return Promise.resolve({
          status: 200,
          body: JSON.stringify({ task: makeTask(), recent: [] }),
        })
      }
      return Promise.resolve({ status: 200, body: '{}' })
    })
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    ;(window as any).moirai = { invoke, on: () => () => {} }
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('re-fetches task detail on the poll cadence while a task is selected', async () => {
    useDaemonStore.setState({
      connected: true,
      ready: true,
      status: makeStatus(),
      slots: [],
    })
    useTasksStore.setState({
      list: [makeTask()],
      selectedId: 'T-POLL',
      detail: { task: makeTask(), recent: [] },
    })

    await act(async () => {
      render(<App />)
      // flush the immediate fetchDetail promise
      await Promise.resolve()
      await Promise.resolve()
    })

    const detailCallsAfterMount = invoke.mock.calls.filter(
      (c) => (c[1] as { path?: string })?.path === '/tasks/T-POLL',
    ).length
    expect(detailCallsAfterMount).toBeGreaterThanOrEqual(1)

    // Advance past two 3s tick cycles -- the self-scheduling setTimeout
    // fires fetchDetail each time.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3100)
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3100)
    })

    const detailCalls = invoke.mock.calls.filter(
      (c) => (c[1] as { path?: string })?.path === '/tasks/T-POLL',
    )
    expect(detailCalls.length).toBeGreaterThanOrEqual(2)
    // Every invocation is the same task id.
    for (const c of detailCalls) {
      expect((c[1] as { path: string }).path).toBe('/tasks/T-POLL')
    }
  })
})
