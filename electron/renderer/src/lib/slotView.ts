import type { SlotView, Verdict } from './daemonClient'

// Lay the three slots out for the visualizer + control strip. The VRAM
// slot is whichever entry reports loaded=true. The two DRAM positions
// are filled left-to-right with the remaining slots in their incoming
// order. When we do not have a full three-slot payload, missing
// positions are returned as null so the caller can render a placeholder.
export interface SlotLayout {
  vram: SlotView | null
  dramLeft: SlotView | null
  dramRight: SlotView | null
}

export function layoutSlots(slots: SlotView[]): SlotLayout {
  const vram = slots.find((s) => s.loaded) ?? null
  const rest = slots.filter((s) => s !== vram)
  return {
    vram,
    dramLeft: rest[0] ?? null,
    dramRight: rest[1] ?? null,
  }
}

// Short single-letter tag. Use the first letter of the slot name in
// upper case so `planner` -> `A`? No: the design convention is letters
// by role, but the role_label from the daemon carries the display
// text. For the single-letter decoration we just use the first
// character of slot (so 'planner' -> 'P', 'coder' -> 'C'), falling back
// to '-' when missing.
export function slotLetter(slot: SlotView | null): string {
  if (!slot?.slot) return '-'
  return slot.slot.charAt(0).toUpperCase()
}

// Role-purpose glyph shown in the big slot decoration. The daemon's
// `role_label` carries the A/B/C slot-position letter; the UI prefers
// the role purpose:
//   planner  -> P  (Planning)
//   coder    -> C  (Coding)
//   reviewer -> R/O (Review & Orchestration)
// Falls back to '-' when the slot name is missing or unknown.
export function displayGlyphForSlot(slot: SlotView | null): string {
  const name = slot?.slot
  if (!name) return '-'
  if (name === 'planner') return 'P'
  if (name === 'coder') return 'C'
  if (name === 'reviewer') return 'R/O'
  return name.charAt(0).toUpperCase()
}

// Canonical verdict bucket. The reviewer emits free-form tokens from LLM
// output -- 'approve', 'approved', 'succeeded', 'accept', 'pass', 'ok',
// 'done', 'revise', 'replan', 'fix', 'reject', 'fail', 'deny' -- so we
// collapse them into the four UI buckets with regex prefix matching.
// Exported so phases.ts and other consumers can share the same logic.
export type VerdictBucket = 'approved' | 'revise' | 'fix' | 'pending'

export function classifyVerdict(verdict: Verdict): VerdictBucket {
  if (!verdict) return 'pending'
  const lower = verdict.toLowerCase().trim()
  if (/^(succeed|success|approv|accept|pass|ok\b|done\b)/.test(lower))
    return 'approved'
  if (/^revise/.test(lower)) return 'revise'
  if (/^replan/.test(lower)) return 'revise'
  if (/^(fix|reject|fail|deny)/.test(lower)) return 'fix'
  return 'pending'
}

// Colour bucket for the verdict chip in the visualizer. Maps directly
// to Chip variants provided by chrome/Chip.tsx.
export function verdictChipVariant(
  verdict: Verdict,
): 'approved' | 'revise' | 'fix' | 'pending' {
  return classifyVerdict(verdict)
}

// Display label for a verdict, bracketed to match the mock.
export function verdictLabel(verdict: Verdict): string {
  if (!verdict) return '[ PENDING ]'
  return `[ ${verdict.toUpperCase()} ]`
}

// Compact "256K" or "1.5M" style label for a context size.
export function formatCtx(ctxSize: number | undefined): string {
  if (!ctxSize || ctxSize <= 0) return '--'
  if (ctxSize >= 1024 * 1024) {
    const m = ctxSize / (1024 * 1024)
    return `${m.toFixed(m >= 10 ? 0 : 1)}M`
  }
  const k = ctxSize / 1024
  return `${k.toFixed(k >= 10 ? 0 : 1)}K`
}
