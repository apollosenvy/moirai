import { useEffect, useRef, useState } from 'react'
import ContextSlider from './ContextSlider'
import KvQuantDropdown, { type KvOption } from './KvQuantDropdown'
import ModelDropdown from './ModelDropdown'
import StatusDot, { type StatusDotVariant } from '../chrome/StatusDot'
import { useDaemonStore } from '../store/daemonStore'
import { usePendingStore, type PendingEntry } from '../store/pendingStore'
import { useModelsStore } from '../store/modelsStore'
import { createDaemonClient } from '../lib/daemonClient'
import type { SlotView } from '../lib/daemonClient'
import { displayGlyphForSlot, formatCtx } from '../lib/slotView'

// Three-column row of per-slot controls. The daemon orders the /slots
// array so we render directly in response order; the panel itself is a
// notched-rect with its own SVG border. Class names + inline styles are
// preserved verbatim from the source HTML.
export default function SlotControls() {
  const slots = useDaemonStore((s) => s.slots)
  const turboSupported = useDaemonStore(
    (s) => s.status?.turboquant_supported ?? false,
  )
  const setModels = useModelsStore((s) => s.setList)

  // Fetch the /models catalog once on mount. We keep it in a store so
  // multiple ModelDropdowns share the same data.
  useEffect(() => {
    const client = createDaemonClient()
    let cancelled = false
    client
      .getModels()
      .then((models) => {
        if (!cancelled) setModels(Array.isArray(models) ? models : [])
      })
      .catch(() => {
        /* tolerated -- the offline panel gate keeps this UI unmounted
           while the daemon is down; a mid-run failure just leaves the
           catalog empty */
      })
    return () => {
      cancelled = true
    }
  }, [setModels])

  return (
    <section className="controls">
      {slots.map((slot) => (
        <SlotPanel
          key={slot.slot}
          slot={slot}
          turboSupported={turboSupported}
        />
      ))}
    </section>
  )
}

interface SlotPanelProps {
  slot: SlotView
  turboSupported: boolean
}

function SlotPanel({ slot, turboSupported }: SlotPanelProps) {
  const pending = usePendingStore((s) => s.pending[slot.slot])
  const setPending = usePendingStore((s) => s.setPending)
  const clearPending = usePendingStore((s) => s.clearPending)

  const hasPending = Boolean(pending)
  const letter = displayGlyphForSlot(slot)
  const loaded = slot.loaded
  const generating = slot.generating

  const subLabel = loaded ? 'VRAM · ACTIVE' : 'DRAM · WARM'

  // Resolve effective (pending-or-current) values for display.
  const effectiveModel = pending?.model_path ?? slot.model_path
  const effectiveCtx = pending?.ctx_size ?? slot.ctx_size
  const effectiveKv = pending?.kv_cache ?? slot.kv_cache

  const dotVariant: StatusDotVariant = loaded
    ? 'ice'
    : generating
      ? 'lime'
      : hasPending
        ? 'amber'
        : 'ice-dim'
  const statusText = loaded
    ? 'LOADED'
    : generating
      ? 'GEN'
      : hasPending
        ? 'PENDING'
        : 'IDLE'
  const statusColor = loaded
    ? 'var(--ice)'
    : hasPending
      ? 'var(--amber)'
      : undefined

  const activeClass = loaded ? ' active' : ''
  const titleColor = loaded ? 'var(--ice)' : undefined
  const subColor = loaded ? 'var(--ice-dim)' : undefined

  return (
    <div className={`ctrl notch${activeClass}`}>
      <svg className="notch-border" viewBox="0 0 500 310" preserveAspectRatio="none">
        <path
          d="M0,0 L490,0 L500,10 L500,310 L0,310 Z"
          {...(loaded ? { stroke: '#00c8ff', strokeOpacity: '1' } : {})}
        />
      </svg>
      <div className="ctrl-head">
        <div className="ctrl-letter">{letter}</div>
        <div style={{ flex: 1 }}>
          <div className="ctrl-title" style={titleColor ? { color: titleColor } : undefined}>
            {slot.slot.toUpperCase()}
          </div>
          <div className="ctrl-sub" style={subColor ? { color: subColor } : undefined}>
            {subLabel}
          </div>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
          <StatusDot variant={dotVariant} />
          <span
            className="ctrl-sub"
            style={{
              letterSpacing: '0.32em',
              ...(statusColor ? { color: statusColor } : {}),
            }}
          >
            {statusText}
          </span>
        </div>
      </div>

      <ModelRow
        slot={slot.slot}
        value={effectiveModel}
        onChange={(model_path) => setPending(slot.slot, { model_path })}
        loaded={loaded}
      />

      <CtxRow
        slot={slot.slot}
        value={effectiveCtx}
        pending={pending?.ctx_size}
        onChange={(ctx_size) => setPending(slot.slot, { ctx_size })}
      />

      <KvRow
        slot={slot.slot}
        modelPath={effectiveModel}
        value={effectiveKv}
        pending={pending?.kv_cache}
        turboSupported={turboSupported}
        onChange={(kv_cache) => setPending(slot.slot, { kv_cache })}
      />

      <ApplyFoot
        slot={slot}
        hasPending={hasPending}
        pending={pending}
        onApplied={() => clearPending(slot.slot)}
      />
    </div>
  )
}

function ModelRow({
  value,
  onChange,
  loaded,
}: {
  slot: string
  value: string
  onChange: (next: string) => void
  loaded: boolean
}) {
  const models = useModelsStore((s) => s.list)
  return (
    <div className="ctrl-row">
      <div className="k">MODEL</div>
      <ModelDropdown
        value={displayModelName(value, models)}
        style={
          loaded
            ? {
                borderColor: 'var(--ice-dim)',
                background: 'rgba(0,200,255,0.04)',
              }
            : undefined
        }
        options={models.map((m) => ({ path: m.path, label: m.name }))}
        onSelect={onChange}
      />
      <div />
    </div>
  )
}

function displayModelName(
  path: string,
  models: { path: string; name: string }[],
): string {
  if (!path) return '--'
  const hit = models.find((m) => m.path === path)
  if (hit) return hit.name
  // Fall back to the tail of the path so we still show something
  // meaningful when the catalog hasn't loaded yet.
  const parts = path.split('/')
  return parts[parts.length - 1] || path
}

function CtxRow({
  value,
  pending,
  onChange,
}: {
  slot: string
  value: number
  pending: number | undefined
  onChange: (next: number) => void
}) {
  // Derive the slider position from the ctx_size. We anchor 100% at
  // 512K to match the mock, which gives us a familiar visual range for
  // the common 32K -> 512K band.
  const maxCtx = 512 * 1024
  const pct = Math.max(6, Math.min(100, (value / maxCtx) * 100))
  const percent = `${pct.toFixed(0)}%`
  const valueLabel =
    pending !== undefined ? (
      <>
        {formatCtx(value)}
        <span className="pending-chip">PENDING</span>
      </>
    ) : (
      formatCtx(value)
    )

  return (
    <div className="ctrl-row">
      <div className="k">CTX</div>
      <ContextSlider
        percent={percent}
        ticks={[
          { left: '6%' },
          { left: '12%' },
          { left: '25%', major: true },
          { left: '50%', major: true },
          { left: '100%', major: true },
        ]}
        valueLabel={valueLabel}
        onChange={(next) => {
          // Map percentage back to ctx_size in powers-of-two steps.
          const sizes = [
            8 * 1024,
            16 * 1024,
            32 * 1024,
            64 * 1024,
            128 * 1024,
            256 * 1024,
            512 * 1024,
          ]
          const chosen = sizes.reduce((best, s) => {
            return Math.abs(s / maxCtx - next / 100) <
              Math.abs(best / maxCtx - next / 100)
              ? s
              : best
          }, sizes[0])
          onChange(chosen)
        }}
      />
      <div />
    </div>
  )
}

const KV_OPTIONS: KvOption[] = [
  { label: 'F16 · baseline', size: '--', kv: 'f16' },
  { label: 'Q8_0', size: '~2×', kv: 'q8_0' },
  { label: 'Q5_1', size: '~2.5×', kv: 'q5_1' },
  { label: 'Q4_0', size: '~3.2×', kv: 'q4_0' },
  { label: 'Turbo3', size: 'HIP parity', ratio: '4.6×', kv: 'turbo3', turbo: true },
  { label: 'Turbo4', size: '4.25-bit', ratio: '3.8×', kv: 'turbo4', turbo: true },
]

function KvRow({
  modelPath,
  value,
  pending,
  turboSupported,
  onChange,
}: {
  slot: string
  modelPath: string
  value: string
  pending: string | undefined
  turboSupported: boolean
  onChange: (next: string) => void
}) {
  const models = useModelsStore((s) => s.list)
  const effectiveKv = pending ?? value
  const isTurbo = effectiveKv.startsWith('turbo')

  // Two distinct warnings (brief §6):
  //   1. binary doesn't support TurboQuant -- keyed off /status.
  //   2. selected model has head_dim=128 with a turbo KV -- keyed off
  //      the /models catalog. Without the catalog we can't classify, so
  //      we skip the warning rather than fire false positives.
  const modelInfo = models.find((m) => m.path === modelPath)
  const headDimWarn = isTurbo && modelInfo?.head_dim === 128
  const unsupportedWarn = !turboSupported && isTurbo

  const warn = headDimWarn || unsupportedWarn

  const current = KV_OPTIONS.find((o) => o.kv === value)
  const displayLabel = current
    ? pending !== undefined
      ? `${current.label} · PENDING`
      : current.label
    : value || '--'

  return (
    <>
      <div className="ctrl-row">
        <div className="k">KV CACHE</div>
        <KvQuantDropdown
          value={displayLabel}
          warn={warn}
          options={KV_OPTIONS}
          selectedKv={value}
          turboSupported={turboSupported}
          onSelect={(kv) => onChange(kv)}
        />
        <div />
      </div>
      {headDimWarn && (
        <div className="ctrl-row" style={{ marginTop: '-4px' }}>
          <div />
          <div className="warn-line">
            ⚠ head_dim=128 + turbo* may produce garbage text
          </div>
          <div />
        </div>
      )}
      {unsupportedWarn && !headDimWarn && (
        <div className="ctrl-row" style={{ marginTop: '-4px' }}>
          <div />
          <div className="warn-line">
            ⚠ binary lacks TurboQuant support -- install llama-cpp-turboquant
          </div>
          <div />
        </div>
      )}
    </>
  )
}

function ApplyFoot({
  slot,
  hasPending,
  pending,
  onApplied,
}: {
  slot: SlotView
  hasPending: boolean
  pending: PendingEntry | undefined
  onApplied: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Brief §6 momentary "Applied" flash. Set true for ~400ms after a
  // successful patchSlot so CSS can run a one-shot lime glow + show
  // the OK label even before the slot list re-polls. We use a counter
  // (not a boolean) because back-to-back applies need to retrigger the
  // animation; bumping the key remounts the className.
  const [flashKey, setFlashKey] = useState(0)
  const flashTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const justApplied = flashKey > 0
  useEffect(() => {
    if (!justApplied) return
    if (flashTimer.current) clearTimeout(flashTimer.current)
    flashTimer.current = setTimeout(() => setFlashKey(0), 400)
    return () => {
      if (flashTimer.current) clearTimeout(flashTimer.current)
    }
  }, [flashKey, justApplied])

  const disabled = !hasPending || slot.generating || busy
  const label = busy
    ? 'APPLY…'
    : justApplied
      ? 'OK'
      : hasPending
        ? 'APPLY'
        : slot.loaded
          ? 'OK'
          : 'APPLIED'

  const apply = async () => {
    if (!pending) return
    setBusy(true)
    setError(null)
    try {
      const client = createDaemonClient()
      await client.patchSlot(slot.slot, pending)
      onApplied()
      setFlashKey((k) => k + 1)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  // Prefer the daemon's reported VRAM telemetry when present; fall back to
  // a non-numeric "ACTIVE" pill so we don't lie to the user with a stale
  // 14.2/24.0 mock value. The daemon /status payload exposes
  // vram_used_mb / vram_total_mb when available.
  const vramUsedMb = useDaemonStore(
    (s) =>
      (s.status as unknown as { vram_used_mb?: number })?.vram_used_mb,
  )
  const vramTotalMb = useDaemonStore(
    (s) =>
      (s.status as unknown as { vram_total_mb?: number })?.vram_total_mb,
  )
  const haveVramTelemetry =
    typeof vramUsedMb === 'number' &&
    typeof vramTotalMb === 'number' &&
    vramTotalMb > 0
  const stat = slot.loaded ? (
    <>
      VRAM ·{' '}
      <b style={{ color: 'var(--ice)' }}>
        {haveVramTelemetry
          ? `${(vramUsedMb / 1024).toFixed(1)} / ${(vramTotalMb / 1024).toFixed(1)} GB`
          : 'ACTIVE'}
      </b>
    </>
  ) : hasPending ? (
    <>
      VRAM EST ·{' '}
      <b style={{ color: 'var(--amber)' }}>
        {formatCtx(pending?.ctx_size ?? slot.ctx_size)} ctx
      </b>
    </>
  ) : (
    <>
      VRAM EST · <b>--</b>
    </>
  )

  const border = slot.loaded ? (
    <path d="M8,1 L72,1 L79,12 L72,23 L8,23 L1,12 Z" stroke="#39ff88" />
  ) : (
    <path d="M8,1 L72,1 L79,12 L72,23 L8,23 L1,12 Z" />
  )

  const buttonClass = [
    'apply',
    disabled ? 'disabled' : '',
    justApplied ? 'just-applied' : '',
  ]
    .filter(Boolean)
    .join(' ')

  return (
    <div className="ctrl-foot">
      <div className="stat">{stat}</div>
      <button
        key={`apply-${flashKey}`}
        className={buttonClass}
        onClick={apply}
        disabled={disabled}
        title={error ?? undefined}
      >
        <svg className="apply-border" viewBox="0 0 80 24" preserveAspectRatio="none">
          {border}
        </svg>
        <span
          style={
            justApplied || slot.loaded ? { color: 'var(--lime)' } : undefined
          }
        >
          {label}
        </span>
      </button>
    </div>
  )
}
