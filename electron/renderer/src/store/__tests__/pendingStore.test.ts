import { beforeEach, describe, expect, it } from 'vitest'
import { usePendingStore } from '../pendingStore'

describe('pendingStore', () => {
  beforeEach(() => {
    usePendingStore.setState({ pending: {} })
  })

  it('setPending writes a fresh entry', () => {
    usePendingStore
      .getState()
      .setPending('coder', { ctx_size: 131072, kv_cache: 'turbo3' })
    expect(usePendingStore.getState().pending.coder).toEqual({
      ctx_size: 131072,
      kv_cache: 'turbo3',
    })
  })

  it('setPending merges with existing entry', () => {
    const s = usePendingStore.getState()
    s.setPending('coder', { model_path: '/m/a.gguf' })
    s.setPending('coder', { ctx_size: 262144 })
    expect(usePendingStore.getState().pending.coder).toEqual({
      model_path: '/m/a.gguf',
      ctx_size: 262144,
    })
  })

  it('setPending skips empty-string and zero fields', () => {
    usePendingStore.getState().setPending('coder', {
      model_path: '',
      ctx_size: 0,
      kv_cache: '',
    })
    expect(usePendingStore.getState().pending.coder).toBeUndefined()
  })

  it('setPending with non-zero then empty does not overwrite', () => {
    const s = usePendingStore.getState()
    s.setPending('coder', { ctx_size: 131072 })
    s.setPending('coder', { ctx_size: 0 })
    expect(usePendingStore.getState().pending.coder?.ctx_size).toBe(131072)
  })

  it('clearPending removes the entry', () => {
    const s = usePendingStore.getState()
    s.setPending('coder', { ctx_size: 131072 })
    s.clearPending('coder')
    expect(usePendingStore.getState().pending.coder).toBeUndefined()
  })

  it('clearPending on missing slot is a no-op', () => {
    const s = usePendingStore.getState()
    const before = usePendingStore.getState().pending
    s.clearPending('reviewer')
    expect(usePendingStore.getState().pending).toBe(before)
  })

  it('hasPending reflects whether an entry has at least one field', () => {
    const s = usePendingStore.getState()
    expect(s.hasPending('coder')).toBe(false)
    s.setPending('coder', { kv_cache: 'turbo3' })
    expect(usePendingStore.getState().hasPending('coder')).toBe(true)
    usePendingStore.getState().clearPending('coder')
    expect(usePendingStore.getState().hasPending('coder')).toBe(false)
  })

  it('setPending does not bleed between slots', () => {
    const s = usePendingStore.getState()
    s.setPending('coder', { ctx_size: 131072 })
    s.setPending('planner', { model_path: '/p.gguf' })
    expect(usePendingStore.getState().pending).toEqual({
      coder: { ctx_size: 131072 },
      planner: { model_path: '/p.gguf' },
    })
  })
})
