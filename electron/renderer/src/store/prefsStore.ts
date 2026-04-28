import { create } from 'zustand'

// Client-side UI preferences. Kept in localStorage so the user's choices
// survive a daemon restart or a hot reload. None of these talk to the
// daemon -- this store is for UI affordances (ambient effects, etc.)
// not for daemon configuration.
//
// Keys are namespaced under "moirai." so a future Phos shell that hosts
// multiple apps does not collide. Reads + writes are guarded against
// localStorage being unavailable (private mode, locked-down WebKit).

export interface PrefsState {
  /** Brief §10 ambient effect: random plasma sparks across the hex grid. */
  traceSparks: boolean
  /** Brief §9 broadcast band: ice stripe sweep on major state changes. */
  broadcastBand: boolean

  setTraceSparks: (next: boolean) => void
  setBroadcastBand: (next: boolean) => void
}

const PREF_KEYS = {
  traceSparks: 'moirai.prefs.traceSparks',
  broadcastBand: 'moirai.prefs.broadcastBand',
} as const

function readBool(key: string, fallback: boolean): boolean {
  try {
    const raw = localStorage.getItem(key)
    if (raw === null) return fallback
    return raw === '1' || raw === 'true'
  } catch {
    return fallback
  }
}

function writeBool(key: string, value: boolean): void {
  try {
    localStorage.setItem(key, value ? '1' : '0')
  } catch {
    /* private mode / locked storage -- pref is in-memory only */
  }
}

export const usePrefsStore = create<PrefsState>((set) => ({
  // Defaults: sparks on (it's the brand signature), band on (cheap +
  // visible). Either can be killed if it ends up annoying.
  traceSparks: readBool(PREF_KEYS.traceSparks, true),
  broadcastBand: readBool(PREF_KEYS.broadcastBand, true),

  setTraceSparks: (next) => {
    writeBool(PREF_KEYS.traceSparks, next)
    set({ traceSparks: next })
  },
  setBroadcastBand: (next) => {
    writeBool(PREF_KEYS.broadcastBand, next)
    set({ broadcastBand: next })
  },
}))
