// Typed client for the agent-router daemon at localhost:5984. All
// network calls go through the Phos bridge (`window.moirai.invoke`) so the
// desktop shell can proxy them over the native IPC channel. This module
// owns the JSON wire types and normalises error handling: status 0
// (bridge cannot reach the daemon) and 4xx/5xx both throw, so callers
// only need try/catch rather than shape-checking the response.

// Phase mirrors the taskstore.Phase string set. Post-RO-rewrite the
// orchestrator only emits 'init', 'coding', and 'done' -- the older
// fine-grained values ('planning', 'plan_review', 'code_review',
// 'revise') are still defined in the Go side for backwards-compat with
// any persisted task records that predate the rewrite, so the wire
// type accepts them. New code should not branch on them.
export type Phase =
  | 'init'
  | 'planning'
  | 'plan_review'
  | 'coding'
  | 'code_review'
  | 'revise'
  | 'done'

// Verdict is intentionally a free-form string. The reviewer emits verdict
// tokens parsed from LLM output and cannot be enumerated -- tokens like
// 'approve', 'approved', 'succeeded', 'accept', 'pass', 'ok', 'revise',
// 'fix', 'replan', etc. all show up in the wild. Downstream code (see
// lib/slotView.ts verdictChipVariant) interprets the token with regex.
export type Verdict = string | null
export type Slot = 'planner' | 'coder' | 'reviewer'

export interface DaemonStatus {
  service: string
  port: number
  active_slot: string
  active_port: number
  task_count: number
  running: number
  last_verdict: Verdict
  turboquant_supported: boolean
  daemon_version: string
  started_at: string
  uptime: string
  // Added by the RO-rewrite cleanup. Optional because older daemons
  // omit them; the UI degrades to fallback values.
  max_ro_turns?: number
  vram_used_mb?: number
  vram_total_mb?: number
  corrupt_task_count?: number
}

export interface SlotView {
  slot: string
  role_label: string
  model_path: string
  model_name: string
  ctx_size: number
  kv_cache: string
  loaded: boolean
  listen_port: number
  generating: boolean
  pending_changes?: {
    model_path?: string
    ctx_size?: number
    kv_cache?: string
  }
  // Sampling parameters the daemon uses when generating with this slot.
  // Optional because older / simpler backends may omit them entirely.
  // No UI consumer yet; kept here so the type doesn't silently drop the
  // field when it comes back from the wire.
  sampling?: {
    temperature?: number
    top_k?: number
    top_p?: number
    min_p?: number
  }
}

export interface ModelInfo {
  path: string
  name: string
  size_bytes: number
  head_dim?: number
  detected_ctx_max?: number
  turboquant_safe: boolean
}

export interface PatchSlotResponse {
  applied: boolean
  pending: boolean
  reason: string
}

export interface Task {
  id: string
  status: string
  phase: Phase
  iterations: number
  replans: number
  active_model: string
  repo_root: string
  branch: string
  description: string
  created_at: string
  updated_at: string
  // Populated by the orchestrator as the task progresses. Present in the
  // /tasks/<id> response but optional because fresh tasks haven't produced
  // either yet. `plan` is a free-form string (multi-line); `reviews` is a
  // chronological list of verdict blobs formatted as `<phase>: <text>`.
  plan?: string
  reviews?: string[]
  last_error?: string
  trace_path?: string
  meta?: Record<string, string>
}

export interface TraceEvent {
  ts: string
  kind: string
  // The Go trace.Event shape is {ts, task_id, kind, data, notes, raw}.
  // Routing/message info lives inside `data` (not at the top level):
  //   swap events     -> {to, reason}
  //   llm_call events -> {role, turn, bytes, head}
  //   info events     -> {message} (or arbitrary k/v pairs)
  //   phase events    -> {phase}
  //   error / fatal   -> {error} or {fatal}
  // LLM_CALL events carry the first ~400 chars of the model reply under
  // `data.head` (the orchestrator nests it intentionally so the wire type
  // stays backwards compatible with older clients that only needed ts/kind).
  // Some older trace events still have it at top-level -- Trace.tsx falls
  // back to event.head when data.head is missing.
  data?: { head?: string } & Record<string, unknown>
  [extra: string]: unknown
}

export interface TaskDetail {
  task: Task
  recent: TraceEvent[]
}

export interface PatchSlotBody {
  model_path?: string
  ctx_size?: number
  kv_cache?: string
}

// Minimal shape of the bridge response produced by the Phos runtime.
interface BridgeResponse {
  status: number
  body: string
  error?: string
}

type InvokeFn = (channel: string, payload: unknown) => Promise<BridgeResponse>

function getInvoke(): InvokeFn {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const p = (globalThis as any).window?.moirai ?? (globalThis as any).moirai
  if (!p || typeof p.invoke !== 'function') {
    throw new Error('moirai bridge unavailable')
  }
  return p.invoke.bind(p) as InvokeFn
}

async function call<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const invoke = getInvoke()
  const res = await invoke('daemon-call', { method, path, body })

  if (!res || res.status === 0) {
    throw new Error(
      `daemon unreachable: ${res?.error ?? 'no response from bridge'}`,
    )
  }

  if (res.status < 200 || res.status >= 300) {
    throw new Error(
      `daemon ${method} ${path} failed: ${res.status} ${res.body ?? ''}`.trim(),
    )
  }

  if (!res.body) {
    return undefined as T
  }

  try {
    return JSON.parse(res.body) as T
  } catch (err) {
    throw new Error(
      `daemon ${method} ${path} returned invalid JSON: ${(err as Error).message}`,
    )
  }
}

export interface DaemonClient {
  getStatus(): Promise<DaemonStatus>
  getReady(): Promise<boolean>
  getSlots(): Promise<SlotView[]>
  getModels(): Promise<ModelInfo[]>
  patchSlot(slot: string, body: PatchSlotBody): Promise<PatchSlotResponse>
  listTasks(): Promise<Task[]>
  getTask(id: string): Promise<TaskDetail>
  submitTask(description: string, repoRoot: string): Promise<Task>
  abortTask(id: string): Promise<{ aborted: string }>
  interruptTask(id: string): Promise<{ interrupted: string }>
  injectGuidance(id: string, message: string): Promise<{ injected: string }>
}

// asArray coerces an IPC response to a real array. The daemon sometimes
// returns an empty 200 body when there's nothing to send, which call<T>()
// resolves to `undefined`. Downstream consumers (.map / .find / .length)
// then explode. Wrap every list-shaped getter so the consumer never sees
// undefined.
function asArray<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : []
}

export function createDaemonClient(): DaemonClient {
  return {
    getStatus: () => call<DaemonStatus>('GET', '/status'),
    getReady: async () => {
      // /ready returns 200 when ready, 503 otherwise; we surface a boolean.
      const invoke = getInvoke()
      const res = await invoke('daemon-call', {
        method: 'GET',
        path: '/ready',
      })
      if (!res || res.status === 0) {
        throw new Error(
          `daemon unreachable: ${res?.error ?? 'no response from bridge'}`,
        )
      }
      return res.status === 200
    },
    getSlots: () =>
      call<SlotView[] | null | undefined>('GET', '/slots').then(asArray),
    getModels: () =>
      call<ModelInfo[] | null | undefined>('GET', '/models').then(asArray),
    patchSlot: (slot, body) =>
      call<PatchSlotResponse>('PATCH', `/slots/${slot}`, body),
    listTasks: () =>
      call<Task[] | null | undefined>('GET', '/tasks').then(asArray),
    getTask: (id) => call<TaskDetail>('GET', `/tasks/${id}`),
    submitTask: (description, repoRoot) =>
      call<Task>('POST', '/submit', {
        description,
        repo_root: repoRoot,
      }),
    abortTask: (id) =>
      call<{ aborted: string }>('POST', `/tasks/${id}/abort`),
    interruptTask: (id) =>
      call<{ interrupted: string }>('POST', `/tasks/${id}/interrupt`),
    injectGuidance: (id, message) =>
      call<{ injected: string }>('POST', `/tasks/${id}/inject`, { message }),
  }
}
