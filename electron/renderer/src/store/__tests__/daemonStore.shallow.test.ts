import { beforeEach, describe, expect, it } from 'vitest'
import type { DaemonStatus, SlotView } from '../../lib/daemonClient'
import { useDaemonStore } from '../daemonStore'

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
    uptime: '00:00:01',
    ...overrides,
  }
}

function makeSlot(overrides: Partial<SlotView> = {}): SlotView {
  return {
    slot: 'planner',
    role_label: 'PLANNER',
    model_path: '/m/p.gguf',
    model_name: 'planner-model',
    ctx_size: 131072,
    kv_cache: 'f16',
    loaded: false,
    listen_port: 6000,
    generating: false,
    ...overrides,
  }
}

describe('daemonStore shallow-equality short-circuit', () => {
  beforeEach(() => {
    useDaemonStore.setState({
      connected: false,
      ready: false,
      status: null,
      slots: [],
      swapId: 0,
    })
  })

  it('setSlots with an equal payload leaves the slots reference unchanged', () => {
    const initial = [makeSlot()]
    useDaemonStore.getState().setSlots(initial)
    const first = useDaemonStore.getState().slots
    // Push a freshly-constructed but value-equal payload.
    useDaemonStore.getState().setSlots([makeSlot()])
    const second = useDaemonStore.getState().slots
    expect(second).toBe(first)
  })

  it('setSlots with a different payload replaces the slots reference', () => {
    useDaemonStore.getState().setSlots([makeSlot({ kv_cache: 'f16' })])
    const first = useDaemonStore.getState().slots
    useDaemonStore.getState().setSlots([makeSlot({ kv_cache: 'turbo3' })])
    const second = useDaemonStore.getState().slots
    expect(second).not.toBe(first)
    expect(second[0].kv_cache).toBe('turbo3')
  })

  it('setStatus with identical payload does not bump swapId', () => {
    const s = useDaemonStore.getState()
    s.setStatus(makeStatus({ active_slot: 'coder', running: 1 }))
    const swap1 = useDaemonStore.getState().swapId
    // Re-send the same shape.
    s.setStatus(makeStatus({ active_slot: 'coder', running: 1 }))
    const swap2 = useDaemonStore.getState().swapId
    expect(swap2).toBe(swap1)
  })

  it('setStatus with only non-slot differences updates status but does not bump swapId', () => {
    const s = useDaemonStore.getState()
    s.setStatus(makeStatus({ active_slot: 'coder', running: 1 }))
    const swap1 = useDaemonStore.getState().swapId
    s.setStatus(makeStatus({ active_slot: 'coder', running: 2 }))
    const swap2 = useDaemonStore.getState().swapId
    expect(swap2).toBe(swap1)
    expect(useDaemonStore.getState().status?.running).toBe(2)
  })

  it('setConnected with the same value leaves state unchanged', () => {
    useDaemonStore.getState().setConnected(true)
    const state1 = useDaemonStore.getState()
    useDaemonStore.getState().setConnected(true)
    const state2 = useDaemonStore.getState()
    // Reference equality on full state confirms no setter-induced update.
    expect(state2).toBe(state1)
  })

  it('setReady with the same value leaves state unchanged', () => {
    useDaemonStore.getState().setReady(true)
    const state1 = useDaemonStore.getState()
    useDaemonStore.getState().setReady(true)
    const state2 = useDaemonStore.getState()
    expect(state2).toBe(state1)
  })
})
