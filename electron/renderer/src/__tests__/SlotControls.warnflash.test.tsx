import { act, fireEvent, render, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import SlotControls from '../slots/SlotControls'
import { useDaemonStore } from '../store/daemonStore'
import { useModelsStore } from '../store/modelsStore'
import { usePendingStore } from '../store/pendingStore'
import type {
  DaemonStatus,
  ModelInfo,
  SlotView,
} from '../lib/daemonClient'

function resetStores() {
  useDaemonStore.setState({
    connected: true,
    ready: true,
    status: null,
    slots: [],
    swapId: 0,
  })
  useModelsStore.setState({ list: [] })
  usePendingStore.setState({ pending: {} })
}

function makeStatus(overrides: Partial<DaemonStatus> = {}): DaemonStatus {
  return {
    service: 'agent-router',
    port: 5984,
    active_slot: 'reviewer',
    active_port: 6001,
    task_count: 0,
    running: 0,
    last_verdict: null,
    turboquant_supported: true,
    daemon_version: 'v0.4.2',
    started_at: '2026-04-23T00:00:00Z',
    uptime: '00:01:00',
    ...overrides,
  }
}

function makeSlot(overrides: Partial<SlotView> = {}): SlotView {
  return {
    slot: 'planner',
    role_label: 'Planner',
    model_path: '/models/planner.gguf',
    model_name: 'planner-model',
    ctx_size: 32768,
    kv_cache: 'turbo3',
    loaded: false,
    listen_port: 6000,
    generating: false,
    ...overrides,
  }
}

function makeModel(overrides: Partial<ModelInfo> = {}): ModelInfo {
  return {
    path: '/models/planner.gguf',
    name: 'planner-model',
    size_bytes: 1_000_000_000,
    head_dim: 128,
    turboquant_safe: false,
    ...overrides,
  }
}

describe('SlotControls warnings + apply flash', () => {
  beforeEach(() => {
    resetStores()
    // Stub Phos bridge so getModels() in SlotControls doesn't throw.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    ;(window as any).moirai = {
      invoke: vi
        .fn()
        .mockResolvedValue({ status: 200, body: '[]' }),
      on: () => () => {},
    }
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('shows head_dim=128 warning when turbo + head_dim=128 model selected', async () => {
    useDaemonStore.setState({
      status: makeStatus({ turboquant_supported: true }),
      slots: [makeSlot({ kv_cache: 'turbo3' })],
    })
    useModelsStore.setState({ list: [makeModel({ head_dim: 128 })] })

    const { container } = render(<SlotControls />)
    await waitFor(() => {
      const warn = container.querySelector('.warn-line')
      expect(warn).toBeInTheDocument()
      expect(warn?.textContent).toMatch(/head_dim=128/)
    })
  })

  it('does NOT show head_dim warning when model is head_dim=64', () => {
    useDaemonStore.setState({
      status: makeStatus({ turboquant_supported: true }),
      slots: [makeSlot({ kv_cache: 'turbo3' })],
    })
    useModelsStore.setState({ list: [makeModel({ head_dim: 64 })] })

    const { container } = render(<SlotControls />)
    const warn = container.querySelector('.warn-line')
    expect(warn).toBeNull()
  })

  it('shows the unsupported-binary warning when turboquant_supported=false', () => {
    useDaemonStore.setState({
      status: makeStatus({ turboquant_supported: false }),
      slots: [makeSlot({ kv_cache: 'turbo3' })],
    })
    useModelsStore.setState({ list: [makeModel({ head_dim: 64 })] })

    const { container } = render(<SlotControls />)
    const warn = container.querySelector('.warn-line')
    expect(warn).toBeInTheDocument()
    expect(warn?.textContent).toMatch(/TurboQuant/i)
  })

  it('does NOT warn when KV is not turbo*', () => {
    useDaemonStore.setState({
      status: makeStatus({ turboquant_supported: false }),
      slots: [makeSlot({ kv_cache: 'f16' })],
    })
    useModelsStore.setState({ list: [makeModel({ head_dim: 128 })] })

    const { container } = render(<SlotControls />)
    expect(container.querySelector('.warn-line')).toBeNull()
  })

  it('apply button gets just-applied flash class for ~400ms after a successful patch', async () => {
    vi.useFakeTimers()
    // Make patchSlot succeed.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    ;(window as any).moirai.invoke = vi.fn().mockResolvedValue({
      status: 200,
      body: '{"applied":true,"pending":false,"reason":""}',
    })
    useDaemonStore.setState({
      status: makeStatus(),
      slots: [makeSlot({ kv_cache: 'f16' })],
    })
    // Seed a pending change so the Apply button is enabled.
    usePendingStore.setState({
      pending: { planner: { ctx_size: 65536 } },
    })

    const { container } = render(<SlotControls />)
    const btn = container.querySelector('button.apply') as HTMLButtonElement
    expect(btn).toBeInTheDocument()
    expect(btn.className).not.toMatch(/just-applied/)

    await act(async () => {
      fireEvent.click(btn)
      // Drain microtasks so patchSlot resolves before we inspect the button.
      await Promise.resolve()
      await Promise.resolve()
    })

    // Re-query: setFlashKey remounts the button via key change.
    const flashed = container.querySelector('button.apply') as HTMLButtonElement
    expect(flashed.className).toMatch(/just-applied/)

    // Advance past the 400ms flash window.
    await act(async () => {
      vi.advanceTimersByTime(450)
    })
    const settled = container.querySelector('button.apply') as HTMLButtonElement
    expect(settled.className).not.toMatch(/just-applied/)
  })
})
