import { render, fireEvent } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import App from '../App'
import { useDaemonStore } from '../store/daemonStore'
import { useTasksStore } from '../store/tasksStore'
import { useModelsStore } from '../store/modelsStore'
import { usePendingStore } from '../store/pendingStore'
import type {
  DaemonStatus,
  SlotView,
  Task,
  TaskDetail,
} from '../lib/daemonClient'

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

function makeStatus(overrides: Partial<DaemonStatus> = {}): DaemonStatus {
  return {
    service: 'agent-router',
    port: 5984,
    active_slot: 'reviewer',
    active_port: 6001,
    task_count: 2,
    running: 1,
    last_verdict: 'approved',
    turboquant_supported: true,
    daemon_version: 'v0.4.2',
    started_at: '2026-04-23T00:00:00Z',
    uptime: '00:14:22',
    ...overrides,
  }
}

function makeSlots(): SlotView[] {
  return [
    {
      slot: 'planner',
      role_label: 'Planner',
      model_path: '/models/planner.gguf',
      model_name: 'planner-model',
      ctx_size: 262144,
      kv_cache: 'f16',
      loaded: false,
      listen_port: 6000,
      generating: false,
    },
    {
      slot: 'coder',
      role_label: 'Coder',
      model_path: '/models/coder.gguf',
      model_name: 'coder-model',
      ctx_size: 131072,
      kv_cache: 'turbo3',
      loaded: false,
      listen_port: 6001,
      generating: false,
    },
    {
      slot: 'reviewer',
      role_label: 'Reviewer',
      model_path: '/models/reviewer.gguf',
      model_name: 'reviewer-model',
      ctx_size: 524288,
      kv_cache: 'turbo3',
      loaded: true,
      listen_port: 6002,
      generating: false,
    },
  ]
}

function makeTask(id: string, overrides: Partial<Task> = {}): Task {
  return {
    id,
    status: 'running',
    phase: 'coding',
    iterations: 2,
    replans: 0,
    active_model: 'coder-model',
    repo_root: '/r',
    branch: 'main',
    description: `desc for ${id}`,
    created_at: '2026-04-23T00:00:00Z',
    updated_at: '2026-04-23T00:00:00Z',
    ...overrides,
  }
}

describe('App', () => {
  beforeEach(() => {
    resetStores()
    // Stub the moirai bridge to return empty-but-non-zero responses so
    // that internal polling resolves without contaminating the render.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    ;(window as any).moirai = {
      invoke: vi.fn().mockImplementation((_ch: string, payload: unknown) => {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        const p = payload as any
        if (p?.path === '/ready') return Promise.resolve({ status: 200, body: '' })
        if (p?.path === '/slots')
          return Promise.resolve({ status: 200, body: '[]' })
        if (p?.path === '/tasks')
          return Promise.resolve({ status: 200, body: '[]' })
        if (p?.path === '/models')
          return Promise.resolve({ status: 200, body: '[]' })
        return Promise.resolve({ status: 200, body: '{}' })
      }),
      on: () => () => {},
    }
  })

  it('shows offline panel when disconnected', () => {
    const { container } = render(<App />)
    expect(container.querySelector('[data-offline]')).toBeInTheDocument()
    expect(container.textContent).toMatch(/DAEMON OFFLINE/i)
    expect(container.textContent).toMatch(/SPAWN DAEMON/i)
  })

  it('SPAWN DAEMON button invokes daemon-spawn', async () => {
    const { container } = render(<App />)
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const invoke = (window as any).moirai.invoke as ReturnType<typeof vi.fn>
    const btn = container.querySelector('button.submit-btn') as HTMLButtonElement
    expect(btn).toBeInTheDocument()
    fireEvent.click(btn)
    expect(invoke).toHaveBeenCalledWith('daemon-spawn', {})
  })

  it('shows warming panel when connected but not ready', () => {
    useDaemonStore.setState({ connected: true, ready: false })
    const { container } = render(<App />)
    expect(container.textContent).toMatch(/DAEMON WARMING UP/i)
  })

  it('renders routing view when connected + ready', () => {
    useDaemonStore.setState({
      connected: true,
      ready: true,
      status: makeStatus(),
      slots: makeSlots(),
      swapId: 1,
    })
    const { container } = render(<App />)
    expect(container.querySelector('.app')).toHaveAttribute(
      'data-view',
      'routing',
    )
    // M⋈IRAI -- the unicode bowtie ⋈ stands in for the O so a regex on
    // M.IRAI matches without depending on the literal join glyph.
    expect(container.querySelector('.wordmark')?.textContent).toMatch(/M.IRAI/i)
    // Header uses daemon data.
    expect(container.textContent).toMatch(/CONNECTED/)
    expect(container.textContent).toMatch(/:5984/)
    expect(container.textContent).toMatch(/v0\.4\.2/)
  })

  it('renders the fabric backdrop when connected', () => {
    useDaemonStore.setState({
      connected: true,
      ready: true,
      status: makeStatus(),
      slots: makeSlots(),
    })
    const { container } = render(<App />)
    expect(container.querySelector('.backdrop')).toBeInTheDocument()
  })

  it('populates task detail when a task is selected', () => {
    useDaemonStore.setState({
      connected: true,
      ready: true,
      status: makeStatus(),
      slots: makeSlots(),
    })
    const detail: TaskDetail = {
      task: makeTask('T-9999', { description: 'selected task' }),
      recent: [],
    }
    useTasksStore.setState({
      list: [makeTask('T-9999', { description: 'selected task' })],
      selectedId: 'T-9999',
      detail,
    })

    const { container } = render(<App />)
    // TaskList row rendered
    expect(container.textContent).toMatch(/T-9999/)
    // TaskDetail populated from detail
    const taskMain = container.querySelector('.task-main')
    expect(taskMain).toBeInTheDocument()
    expect(taskMain?.textContent).toMatch(/T-9999/)
  })

  it('rail-btn metrics + settings switch view', () => {
    useDaemonStore.setState({
      connected: true,
      ready: true,
      status: makeStatus(),
      slots: makeSlots(),
    })
    const { container } = render(<App />)
    const metricsBtn = container.querySelector(
      '.rail-btn[data-nav="metrics"]',
    ) as HTMLDivElement
    const settingsBtn = container.querySelector(
      '.rail-btn[data-nav="settings"]',
    ) as HTMLDivElement
    expect(metricsBtn).toBeInTheDocument()
    expect(settingsBtn).toBeInTheDocument()

    fireEvent.click(metricsBtn)
    expect(container.querySelector('.app')).toHaveAttribute(
      'data-view',
      'metrics',
    )

    fireEvent.click(settingsBtn)
    expect(container.querySelector('.app')).toHaveAttribute(
      'data-view',
      'settings',
    )
  })

  it('Ctrl+K opens command palette, Esc closes it', () => {
    useDaemonStore.setState({
      connected: true,
      ready: true,
      status: makeStatus(),
      slots: makeSlots(),
    })
    const { container, baseElement } = render(<App />)
    // Open
    fireEvent.keyDown(window, { key: 'k', ctrlKey: true })
    expect(baseElement.querySelector('.cmd-palette')).toBeInTheDocument()
    // Close via Esc
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(baseElement.querySelector('.cmd-palette')).not.toBeInTheDocument()
    // App view unchanged (still routing)
    expect(container.querySelector('.app')).toHaveAttribute(
      'data-view',
      'routing',
    )
  })

  it('? toggles help overlay', () => {
    useDaemonStore.setState({
      connected: true,
      ready: true,
      status: makeStatus(),
      slots: makeSlots(),
    })
    const { baseElement } = render(<App />)
    fireEvent.keyDown(window, { key: '?' })
    expect(baseElement.querySelector('.help-panel')).toBeInTheDocument()
    fireEvent.keyDown(window, { key: '?' })
    expect(baseElement.querySelector('.help-panel')).not.toBeInTheDocument()
  })

  it('clicking a task row calls selectTask', () => {
    useDaemonStore.setState({
      connected: true,
      ready: true,
      status: makeStatus(),
      slots: makeSlots(),
    })
    useTasksStore.setState({
      list: [makeTask('T-123')],
      selectedId: null,
      detail: null,
    })
    const { container } = render(<App />)
    const row = container.querySelector('.task-row') as HTMLDivElement
    expect(row).toBeInTheDocument()
    fireEvent.click(row)
    expect(useTasksStore.getState().selectedId).toBe('T-123')
  })
})
