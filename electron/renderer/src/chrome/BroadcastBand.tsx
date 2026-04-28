import { useEffect, useRef, useState } from 'react'
import { usePrefsStore } from '../store/prefsStore'
import { useDaemonStore } from '../store/daemonStore'
import { useTasksStore } from '../store/tasksStore'

// Brief §9 broadcast band. A ~40px ice-tinted gradient stripe sweeps
// from top to bottom of the viewport once over 600ms when a major
// state change lands (swap completes, verdict arrives, task submitted).
//
// The band is implemented as a single absolutely-positioned div whose
// translateY animates 0% -> 100% via a CSS keyframe. Each trigger
// remounts the div by bumping a key, so re-firing is free of any
// stuck-state hazard.
//
// Triggers are detected by subscribing to store deltas:
//   - swapId increment   -> active VRAM slot changed
//   - last_verdict change -> reviewer reached a verdict
//   - tasks list grew    -> a new task was submitted
//
// We deliberately swallow the very first observation so we don't fire
// the band on initial app hydration when stores get filled.

interface Pulse {
  key: number
  reason: 'swap' | 'verdict' | 'submit'
}

export default function BroadcastBand() {
  const enabled = usePrefsStore((s) => s.broadcastBand)
  const swapId = useDaemonStore((s) => s.swapId)
  const verdict = useDaemonStore((s) => s.status?.last_verdict ?? null)
  const taskCount = useTasksStore((s) => s.list.length)

  // Refs to track previous values without re-running effects every render.
  const lastSwap = useRef<number | null>(null)
  const lastVerdict = useRef<string | null | undefined>(undefined)
  const lastTaskCount = useRef<number | null>(null)
  const [pulse, setPulse] = useState<Pulse | null>(null)
  const clearTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  // Common emitter that gates on enabled and resets the auto-clear timer.
  const emit = (reason: Pulse['reason']) => {
    if (!enabled) return
    setPulse({ key: Date.now(), reason })
    if (clearTimer.current) clearTimeout(clearTimer.current)
    // 700ms = animation duration (600ms) + a tiny buffer so the
    // remove-from-DOM happens after the keyframe finishes painting.
    clearTimer.current = setTimeout(() => setPulse(null), 700)
  }

  // Swap detection. swapId starts at 0; the first time we see any value
  // we just record it without firing.
  useEffect(() => {
    if (lastSwap.current === null) {
      lastSwap.current = swapId
      return
    }
    if (swapId !== lastSwap.current) {
      lastSwap.current = swapId
      emit('swap')
    }
    // emit closes over the latest enabled flag through the store hook,
    // so excluding it from deps avoids a re-run loop.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [swapId])

  // Verdict change detection. Verdicts come in as free-form strings;
  // any non-null change is a real verdict landing.
  useEffect(() => {
    if (lastVerdict.current === undefined) {
      lastVerdict.current = verdict
      return
    }
    if (verdict !== lastVerdict.current && verdict !== null) {
      lastVerdict.current = verdict
      emit('verdict')
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [verdict])

  // Task-count growth = new submission.
  useEffect(() => {
    if (lastTaskCount.current === null) {
      lastTaskCount.current = taskCount
      return
    }
    if (taskCount > lastTaskCount.current) {
      lastTaskCount.current = taskCount
      emit('submit')
    } else {
      // Trim case: keep ref synced so a re-grow later doesn't double-fire.
      lastTaskCount.current = taskCount
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [taskCount])

  useEffect(() => {
    return () => {
      if (clearTimer.current) clearTimeout(clearTimer.current)
    }
  }, [])

  if (!enabled || !pulse) return null

  return (
    <div
      key={pulse.key}
      className={`broadcast-band broadcast-${pulse.reason}`}
      aria-hidden="true"
    />
  )
}
