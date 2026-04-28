import { create } from 'zustand'
import type { DaemonStatus, SlotView } from '../lib/daemonClient'

// Central daemon store. Tracks connection + readiness flags plus the
// latest /status and /slots payloads. The visualizer watches `swapId`
// to fire the plasma pulse whenever the active_slot changes; we
// increment it inside setStatus so every downstream subscriber sees
// the same monotonic counter.
//
// All setters bail out when the incoming payload is shallow-equal to the
// current state. At the 3s poll cadence most ticks produce identical
// slots/status payloads, and without this guard every tick triggered a
// re-render storm through every subscriber (RouteVisualizer, SlotControls,
// Trace, TaskList, ...). JSON.stringify is cheap enough at current sizes
// and is stable for the primitive+array+object shape these payloads use.
export interface DaemonState {
  connected: boolean
  ready: boolean
  status: DaemonStatus | null
  slots: SlotView[]
  swapId: number

  setConnected: (connected: boolean) => void
  setReady: (ready: boolean) => void
  setStatus: (status: DaemonStatus | null) => void
  setSlots: (slots: SlotView[]) => void
}

function jsonEq(a: unknown, b: unknown): boolean {
  if (a === b) return true
  try {
    return JSON.stringify(a) === JSON.stringify(b)
  } catch {
    return false
  }
}

export const useDaemonStore = create<DaemonState>((set) => ({
  connected: false,
  ready: false,
  status: null,
  slots: [],
  swapId: 0,

  setConnected: (connected) =>
    set((prev) => (prev.connected === connected ? prev : { connected })),
  setReady: (ready) =>
    set((prev) => (prev.ready === ready ? prev : { ready })),
  setStatus: (status) =>
    set((prev) => {
      const prevSlot = prev.status?.active_slot ?? null
      const nextSlot = status?.active_slot ?? null
      const slotChanged = prevSlot !== nextSlot
      // No slot change + identical payload = nothing downstream cares.
      if (!slotChanged && jsonEq(prev.status, status)) return prev
      const swapId = slotChanged ? prev.swapId + 1 : prev.swapId
      return { status, swapId }
    }),
  setSlots: (slots) =>
    set((prev) => {
      // Defensive: if a caller hands us undefined/null (e.g. an IPC layer
      // that bypassed daemonClient's asArray guard), coerce to [] so
      // SlotControls.tsx's slots.map never crashes.
      const next = Array.isArray(slots) ? slots : []
      return jsonEq(prev.slots, next) ? prev : { slots: next }
    }),
}))
