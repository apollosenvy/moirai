import { create } from 'zustand'

export interface PendingEntry {
  model_path?: string
  ctx_size?: number
  kv_cache?: string
}

// Per-slot pending slot configuration, keyed by slot name
// ('planner', 'coder', 'reviewer'). setPending merges into the existing
// entry (non-zero / non-empty fields only) so a ctx_size change does
// not clobber a prior model_path change. Apply button in SlotControls
// reads this; on successful PATCH it calls clearPending.
export interface PendingState {
  pending: Record<string, PendingEntry>

  setPending: (slot: string, value: PendingEntry) => void
  clearPending: (slot: string) => void
  hasPending: (slot: string) => boolean
}

function mergeEntry(
  current: PendingEntry | undefined,
  next: PendingEntry,
): PendingEntry {
  const out: PendingEntry = { ...(current ?? {}) }
  if (next.model_path !== undefined && next.model_path !== '') {
    out.model_path = next.model_path
  }
  if (next.ctx_size !== undefined && next.ctx_size !== 0) {
    out.ctx_size = next.ctx_size
  }
  if (next.kv_cache !== undefined && next.kv_cache !== '') {
    out.kv_cache = next.kv_cache
  }
  return out
}

export const usePendingStore = create<PendingState>((set, get) => ({
  pending: {},

  setPending: (slot, value) =>
    set((prev) => {
      const merged = mergeEntry(prev.pending[slot], value)
      if (
        merged.model_path === undefined &&
        merged.ctx_size === undefined &&
        merged.kv_cache === undefined
      ) {
        // Nothing to track, drop the entry.
        const { [slot]: _, ...rest } = prev.pending
        void _
        return { pending: rest }
      }
      return { pending: { ...prev.pending, [slot]: merged } }
    }),

  clearPending: (slot) =>
    set((prev) => {
      if (!(slot in prev.pending)) return prev
      const { [slot]: _, ...rest } = prev.pending
      void _
      return { pending: rest }
    }),

  hasPending: (slot) => {
    const entry = get().pending[slot]
    if (!entry) return false
    return (
      entry.model_path !== undefined ||
      entry.ctx_size !== undefined ||
      entry.kv_cache !== undefined
    )
  },
}))
