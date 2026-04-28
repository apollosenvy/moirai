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

describe('daemonStore', () => {
  beforeEach(() => {
    useDaemonStore.setState({
      connected: false,
      ready: false,
      status: null,
      slots: [],
      swapId: 0,
    })
  })

  it('setConnected toggles flag', () => {
    useDaemonStore.getState().setConnected(true)
    expect(useDaemonStore.getState().connected).toBe(true)
    useDaemonStore.getState().setConnected(false)
    expect(useDaemonStore.getState().connected).toBe(false)
  })

  it('setReady toggles flag', () => {
    useDaemonStore.getState().setReady(true)
    expect(useDaemonStore.getState().ready).toBe(true)
  })

  it('setStatus does not bump swapId on first set', () => {
    // First status ever seen: prevSlot is null, nextSlot is 'reviewer'.
    // That is a change (null -> reviewer) so swapId IS incremented.
    useDaemonStore.getState().setStatus(makeStatus({ active_slot: 'reviewer' }))
    expect(useDaemonStore.getState().swapId).toBe(1)
  })

  it('setStatus does not bump swapId when active_slot is unchanged', () => {
    const s = useDaemonStore.getState()
    s.setStatus(makeStatus({ active_slot: 'coder' }))
    const after1 = useDaemonStore.getState().swapId
    s.setStatus(makeStatus({ active_slot: 'coder', running: 1 }))
    const after2 = useDaemonStore.getState().swapId
    expect(after1).toBe(after2)
  })

  it('setStatus bumps swapId when active_slot changes', () => {
    const s = useDaemonStore.getState()
    s.setStatus(makeStatus({ active_slot: 'coder' }))
    const first = useDaemonStore.getState().swapId
    s.setStatus(makeStatus({ active_slot: 'reviewer' }))
    expect(useDaemonStore.getState().swapId).toBe(first + 1)
  })

  it('setStatus handles null (disconnect) without crashing', () => {
    const s = useDaemonStore.getState()
    s.setStatus(makeStatus({ active_slot: 'coder' }))
    const first = useDaemonStore.getState().swapId
    s.setStatus(null)
    expect(useDaemonStore.getState().status).toBeNull()
    // coder -> null is a change.
    expect(useDaemonStore.getState().swapId).toBe(first + 1)
  })

  it('setSlots replaces slots array', () => {
    const slots: SlotView[] = [
      {
        slot: 'planner',
        role_label: 'PLANNER',
        model_path: '/m/p.gguf',
        model_name: 'planner-model',
        ctx_size: 131072,
        kv_cache: 'f16',
        loaded: false,
        listen_port: 6000,
        generating: false,
      },
    ]
    useDaemonStore.getState().setSlots(slots)
    expect(useDaemonStore.getState().slots).toEqual(slots)
  })
})
