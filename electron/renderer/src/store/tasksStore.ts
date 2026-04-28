import { create } from 'zustand'
import type { Task, TaskDetail, TraceEvent } from '../lib/daemonClient'

// Task store: the paginated list from /tasks, the currently-selected id,
// and the full detail payload for the selection (task + recent trace).
// selectTask(null) always clears detail so stale data never flashes
// onto a fresh selection.
//
// setList and setDetail short-circuit on equal payloads to stop the 3s
// poll from re-rendering every subscriber (TaskList, Plan, Reviews,
// Trace) when nothing has actually changed.
export interface TasksState {
  list: Task[]
  selectedId: string | null
  detail: TaskDetail | null

  setList: (list: Task[]) => void
  selectTask: (id: string | null) => void
  setDetail: (detail: TaskDetail | null) => void
}

function taskListEqual(a: Task[], b: Task[]): boolean {
  if (a === b) return true
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i += 1) {
    const x = a[i]
    const y = b[i]
    if (x === y) continue
    if (
      x.id !== y.id ||
      x.status !== y.status ||
      x.phase !== y.phase ||
      x.iterations !== y.iterations ||
      x.replans !== y.replans ||
      x.active_model !== y.active_model ||
      x.repo_root !== y.repo_root ||
      x.branch !== y.branch ||
      x.description !== y.description ||
      x.updated_at !== y.updated_at ||
      x.created_at !== y.created_at ||
      (x.last_error ?? null) !== (y.last_error ?? null)
    ) {
      return false
    }
  }
  return true
}

function traceEventEqual(a: TraceEvent, b: TraceEvent): boolean {
  if (a === b) return true
  if (a.ts !== b.ts) return false
  if (a.kind !== b.kind) return false
  // Routing/message info now lives inside data. Compare the primitive
  // fields Trace.tsx actually reads (to, role, slot, message, phase,
  // turn, bytes, error, fatal, head). Anything more nuanced would require
  // a full deep-compare of data, which is overkill for the 3s poll loop.
  const da = a.data ?? {}
  const db = b.data ?? {}
  if ((da.to ?? null) !== (db.to ?? null)) return false
  if ((da.role ?? null) !== (db.role ?? null)) return false
  if ((da.slot ?? null) !== (db.slot ?? null)) return false
  if ((da.message ?? null) !== (db.message ?? null)) return false
  if ((da.phase ?? null) !== (db.phase ?? null)) return false
  if ((da.turn ?? null) !== (db.turn ?? null)) return false
  if ((da.bytes ?? null) !== (db.bytes ?? null)) return false
  if ((da.error ?? null) !== (db.error ?? null)) return false
  if ((da.fatal ?? null) !== (db.fatal ?? null)) return false
  if ((da.head ?? null) !== (db.head ?? null)) return false
  return true
}

function detailEqual(a: TaskDetail | null, b: TaskDetail | null): boolean {
  if (a === b) return true
  if (!a || !b) return false
  const ta = a.task
  const tb = b.task
  if (
    ta.id !== tb.id ||
    ta.status !== tb.status ||
    ta.phase !== tb.phase ||
    ta.iterations !== tb.iterations ||
    ta.replans !== tb.replans ||
    ta.active_model !== tb.active_model ||
    ta.repo_root !== tb.repo_root ||
    ta.branch !== tb.branch ||
    ta.description !== tb.description ||
    ta.updated_at !== tb.updated_at ||
    ta.created_at !== tb.created_at ||
    (ta.plan ?? null) !== (tb.plan ?? null) ||
    (ta.last_error ?? null) !== (tb.last_error ?? null) ||
    (ta.reviews?.length ?? 0) !== (tb.reviews?.length ?? 0)
  ) {
    return false
  }
  // Compare reviews entry-by-entry (they're formatted strings).
  const ra = ta.reviews ?? []
  const rb = tb.reviews ?? []
  for (let i = 0; i < ra.length; i += 1) {
    if (ra[i] !== rb[i]) return false
  }
  // Recent trace: length + per-event primitive fields. Skips deep-object
  // comparison of data beyond head, which is the only field Trace.tsx reads.
  const ea = a.recent
  const eb = b.recent
  if (ea.length !== eb.length) return false
  for (let i = 0; i < ea.length; i += 1) {
    if (!traceEventEqual(ea[i], eb[i])) return false
  }
  return true
}

export const useTasksStore = create<TasksState>((set) => ({
  list: [],
  selectedId: null,
  detail: null,

  setList: (list) => {
    const next = Array.isArray(list) ? list : []
    return set((prev) => (taskListEqual(prev.list, next) ? prev : { list: next }))
  },
  selectTask: (id) =>
    set((prev) => {
      if (id === prev.selectedId) return prev
      return { selectedId: id, detail: null }
    }),
  setDetail: (detail) =>
    set((prev) => (detailEqual(prev.detail, detail) ? prev : { detail })),
}))
