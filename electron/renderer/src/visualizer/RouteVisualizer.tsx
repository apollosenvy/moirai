import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import ConnectorLines, { type ConnectorPaths } from './ConnectorLines'
import PlasmaPulse from './PlasmaPulse'
import Chip from '../chrome/Chip'
import { useDaemonStore } from '../store/daemonStore'
import {
  displayGlyphForSlot,
  formatCtx,
  layoutSlots,
  slotLetter,
  verdictChipVariant,
  verdictLabel,
} from '../lib/slotView'
import type { SlotView } from '../lib/daemonClient'

// SVG viewBox for the connector overlay. preserveAspectRatio="none"
// means viewBox coords map to container pixels by a pure X/Y scale, so
// we can project DOM rects into viewBox space with a simple ratio.
const VIEWBOX_W = 1480
const VIEWBOX_H = 480

// Fallback paths (match the original design HTML coordinates) used on
// first render before refs resolve, and as a sane default if the
// ResizeObserver never fires (e.g. in SSR-ish test harnesses).
const FALLBACK_PATHS: ConnectorPaths = {
  rest: 'M740 230 C 580 280, 440 370, 320 370',
  swap: 'M740 230 C 900 280, 1040 370, 1160 370',
  pred: 'M740 240 C 580 290, 440 380, 320 380',
  cross: 'M320 400 C 580 430, 900 430, 1160 400',
  pathB: 'M1160 370 C 1040 370, 900 280, 740 230',
}

// Hero panel: VRAM slot (centered top), two DRAM slots (bottom corners),
// connector lines and traveling plasma pulse overlay.
// Class names, SVG path geometry and vertex positions are preserved
// verbatim from the design HTML. Live data from daemonStore.
export default function RouteVisualizer() {
  const slots = useDaemonStore((s) => s.slots)
  const status = useDaemonStore((s) => s.status)
  const swapId = useDaemonStore((s) => s.swapId)

  const { vram, dramLeft, dramRight } = layoutSlots(slots)

  // Re-mount PlasmaPulse whenever swapId changes so the SVG animation
  // restarts. We keep the previous key in ref so the component still
  // renders even before any swap happens.
  const [pulseKey, setPulseKey] = useState(0)
  const lastSwap = useRef(swapId)
  useEffect(() => {
    if (swapId !== lastSwap.current) {
      lastSwap.current = swapId
      setPulseKey((k) => k + 1)
    }
  }, [swapId])

  // Refs for the three slot frames + the visualizer itself. We measure
  // panel bounding boxes and project them into the SVG's 1480x480
  // viewBox so the connector lines stay attached when panels move.
  const sectionRef = useRef<HTMLElement | null>(null)
  const vramRef = useRef<HTMLDivElement | null>(null)
  const dramLeftRef = useRef<HTMLDivElement | null>(null)
  const dramRightRef = useRef<HTMLDivElement | null>(null)
  const [paths, setPaths] = useState<ConnectorPaths>(FALLBACK_PATHS)

  useLayoutEffect(() => {
    const section = sectionRef.current
    const v = vramRef.current
    const l = dramLeftRef.current
    const r = dramRightRef.current
    if (!section || !v || !l || !r) return

    const recompute = () => {
      const sect = section.getBoundingClientRect()
      if (sect.width === 0 || sect.height === 0) return
      const sx = VIEWBOX_W / sect.width
      const sy = VIEWBOX_H / sect.height

      const project = (el: HTMLElement) => {
        const b = el.getBoundingClientRect()
        return {
          bottomCx: ((b.left + b.width / 2) - sect.left) * sx,
          bottomY: (b.bottom - sect.top) * sy,
          topCx: ((b.left + b.width / 2) - sect.left) * sx,
          topY: (b.top - sect.top) * sy,
        }
      }

      const vm = project(v)
      const lm = project(l)
      const rm = project(r)

      // VRAM anchor = bottom-center of VRAM frame.
      // DRAM anchors = top-center of each DRAM frame.
      const vx = vm.bottomCx
      const vy = vm.bottomY
      const lx = lm.topCx
      const ly = lm.topY
      const rx = rm.topCx
      const ry = rm.topY

      // Cubic control points that roughly reproduce the design curve
      // (arc outward then down to the DRAM panel). Placed one-third of
      // the way from each endpoint with a vertical offset proportional
      // to the vertical span between VRAM and DRAM so the curve scales
      // with panel spacing.
      const dyL = Math.max(40, (ly - vy) * 0.6)
      const dyR = Math.max(40, (ry - vy) * 0.6)

      const rest = `M${vx.toFixed(1)} ${vy.toFixed(1)} C ${(vx + (lx - vx) * 0.35).toFixed(1)} ${(vy + dyL * 0.5).toFixed(1)}, ${(vx + (lx - vx) * 0.75).toFixed(1)} ${(ly).toFixed(1)}, ${lx.toFixed(1)} ${ly.toFixed(1)}`
      const swap = `M${vx.toFixed(1)} ${vy.toFixed(1)} C ${(vx + (rx - vx) * 0.35).toFixed(1)} ${(vy + dyR * 0.5).toFixed(1)}, ${(vx + (rx - vx) * 0.75).toFixed(1)} ${(ry).toFixed(1)}, ${rx.toFixed(1)} ${ry.toFixed(1)}`
      const pred = `M${vx.toFixed(1)} ${(vy + 10).toFixed(1)} C ${(vx + (lx - vx) * 0.35).toFixed(1)} ${(vy + dyL * 0.5 + 10).toFixed(1)}, ${(vx + (lx - vx) * 0.75).toFixed(1)} ${(ly + 10).toFixed(1)}, ${lx.toFixed(1)} ${(ly + 10).toFixed(1)}`
      // DRAM <-> DRAM bottom trace. We anchor it below the DRAM tops;
      // use mid-height of the DRAM frames as a rough visual baseline.
      const lBottom = (l.getBoundingClientRect().bottom - sect.top) * sy
      const rBottom = (r.getBoundingClientRect().bottom - sect.top) * sy
      const crossY = (lBottom + rBottom) / 2
      const cross = `M${lx.toFixed(1)} ${crossY.toFixed(1)} C ${(lx + (rx - lx) * 0.25).toFixed(1)} ${(crossY + 20).toFixed(1)}, ${(lx + (rx - lx) * 0.75).toFixed(1)} ${(crossY + 20).toFixed(1)}, ${rx.toFixed(1)} ${crossY.toFixed(1)}`
      // PlasmaPulse follows #path-b from DRAM-right to VRAM (reverse of swap).
      const pathB = `M${rx.toFixed(1)} ${ry.toFixed(1)} C ${(vx + (rx - vx) * 0.75).toFixed(1)} ${(ry).toFixed(1)}, ${(vx + (rx - vx) * 0.35).toFixed(1)} ${(vy + dyR * 0.5).toFixed(1)}, ${vx.toFixed(1)} ${vy.toFixed(1)}`

      const next: ConnectorPaths = { rest, swap, pred, cross, pathB }
      // Guard against identical recomputation -- infinite ResizeObserver
      // feedback loops on re-entry, plus saves a render when slots change
      // but panel geometry doesn't (e.g. only kv_cache changed).
      setPaths((prev) =>
        prev.rest === next.rest &&
        prev.swap === next.swap &&
        prev.pred === next.pred &&
        prev.cross === next.cross &&
        prev.pathB === next.pathB
          ? prev
          : next,
      )
    }

    recompute()

    // ResizeObserver is absent in some test environments (jsdom). We
    // still want a single measurement pass on mount and on window
    // resize; that covers the live app. Guard so tests do not crash.
    const RO = typeof window !== 'undefined'
      ? (window as unknown as { ResizeObserver?: typeof ResizeObserver }).ResizeObserver
      : undefined
    let ro: ResizeObserver | null = null
    if (RO) {
      ro = new RO(recompute)
      ro.observe(section)
      ro.observe(v)
      ro.observe(l)
      ro.observe(r)
    }
    window.addEventListener('resize', recompute)
    return () => {
      if (ro) ro.disconnect()
      window.removeEventListener('resize', recompute)
    }
    // Re-run whenever slots change at the root. The old deps only watched
    // count + presence flags, so mutations to ctx_size / kv_cache / loaded
    // on an existing slot did not retrigger layout. The setPaths guard
    // above keeps this from looping.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [slots])

  // Empty state: no slots yet from daemon.
  if (slots.length === 0) {
    return (
      <section className="visualizer" ref={sectionRef}>
        <div className="section-tag">ROUTE FABRIC</div>
        <div className="section-tag r">STATE · AWAITING DAEMON</div>
        <div
          style={{
            position: 'absolute',
            inset: 0,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            color: 'var(--text-mute)',
            letterSpacing: '0.2em',
            fontSize: '11px',
          }}
        >
          AWAITING SLOT DATA
        </div>
      </section>
    )
  }

  const vramLetter = slotLetter(vram)
  // next_slots was retired with the dead phase taxonomy in the RO
  // rewrite; predicted-slot chrome is no longer driven from /status.
  const nextSlots: string[] = []

  const stateTag = vram
    ? `STATE · ${vramLetter}-LOADED`
    : 'STATE · NO VRAM SLOT'

  const verdictActive = vram?.slot === 'reviewer'

  return (
    <section className="visualizer" ref={sectionRef}>
      <div className="section-tag">ROUTE FABRIC</div>
      <div className="section-tag r">{stateTag}</div>

      {/* Connector + plasma path SVG */}
      <svg
        className="viz-svg"
        viewBox={`0 0 ${VIEWBOX_W} ${VIEWBOX_H}`}
        preserveAspectRatio="none"
        role="img"
        aria-label="Route visualizer showing P/C/RO slot lifecycle and current flow"
      >
        <title>Route visualizer showing P/C/RO slot lifecycle and current flow</title>
        <ConnectorLines paths={paths} />
        <PlasmaPulse key={pulseKey} />
      </svg>

      {/* VRAM container label */}
      <div className="viz-label tl">VRAM · SLOT ACTIVE</div>
      <div className="viz-label tr legend" style={{ bottom: 'auto', right: 24, padding: 0 }}>
        <div className="li">
          <span className="sw" />
          <span>TRACE</span>
        </div>
        <div className="li">
          <span className="sw amber" />
          <span>PREDICTED</span>
        </div>
        <div className="li">
          <span className="sw ice" />
          <span>ACTIVE SWAP</span>
        </div>
        <div className="li">
          <span className="sw plasma" />
          <span>PLASMA PULSE</span>
        </div>
      </div>

      {/* VRAM slot */}
      {renderVramSlot(vram, vramRef)}

      {/* Verdict chip -- only meaningful on reviewer */}
      {verdictActive && (
        <div className="verdict-wrap">
          <span className="verdict-label">LAST VERDICT</span>
          <Chip variant={verdictChipVariant(status?.last_verdict ?? null)}>
            {verdictLabel(status?.last_verdict ?? null)}
          </Chip>
          <span className="verdict-label" style={{ paddingLeft: '10px' }}>
            TASKS {status?.task_count ?? 0}
          </span>
        </div>
      )}

      {renderDramSlot('dram-left', dramLeft, isPredicted('dram-left', dramLeft, nextSlots), dramLeftRef)}
      {renderDramSlot('dram-right', dramRight, isPredicted('dram-right', dramRight, nextSlots), dramRightRef)}
    </section>
  )
}

function isPredicted(
  _position: 'dram-left' | 'dram-right',
  slot: SlotView | null,
  nextSlots: string[],
): boolean {
  if (!slot) return false
  return nextSlots.includes(slot.slot)
}

function renderVramSlot(
  vram: SlotView | null,
  frameRef: React.RefObject<HTMLDivElement | null>,
) {
  if (!vram) {
    return (
      <div className="slot-frame vram" ref={frameRef}>
        <div className="slot-tag">VRAM EMPTY</div>
        <div className="slot vram hex">
          <svg className="hex-border" viewBox="0 0 560 184" preserveAspectRatio="none">
            <path d="M20,1 L540,1 L559,92 L540,183 L20,183 L1,92 Z" />
          </svg>
          <span className="vertex" style={{ left: '20px', top: 0 }} />
          <span className="vertex" style={{ left: 'calc(100% - 20px)', top: 0 }} />
          <span className="vertex" style={{ left: '100%', top: '50%' }} />
          <span className="vertex" style={{ left: 'calc(100% - 20px)', top: '100%' }} />
          <span className="vertex" style={{ left: '20px', top: '100%' }} />
          <span className="vertex" style={{ left: 0, top: '50%' }} />
          <div className="slot-inner">
            <div className="slot-glyph">·</div>
            <div className="slot-meta">
              <div className="role">NO SLOT LOADED</div>
              <div className="model">--</div>
              <div className="specs">
                <b>--</b> ctx · <b>--</b> kv
              </div>
            </div>
            <div className="slot-rate">
              <div className="num dash">--</div>
              <div className="unit">IDLE</div>
            </div>
          </div>
        </div>
      </div>
    )
  }

  const letter = slotLetter(vram)
  return (
    <div className="slot-frame vram" ref={frameRef}>
      <div className="slot-tag">SLOT {letter}</div>
      <div className="slot vram hex active">
        <svg className="hex-border" viewBox="0 0 560 184" preserveAspectRatio="none">
          <path d="M20,1 L540,1 L559,92 L540,183 L20,183 L1,92 Z" />
        </svg>
        <span className="vertex" style={{ left: '20px', top: 0 }} />
        <span className="vertex" style={{ left: 'calc(100% - 20px)', top: 0 }} />
        <span className="vertex" style={{ left: '100%', top: '50%' }} />
        <span className="vertex" style={{ left: 'calc(100% - 20px)', top: '100%' }} />
        <span className="vertex" style={{ left: '20px', top: '100%' }} />
        <span className="vertex" style={{ left: 0, top: '50%' }} />

        <div className="slot-inner">
          <div className="slot-glyph">▣</div>
          <div className="slot-meta">
            <div className="role">{displayGlyphForSlot(vram)} · VRAM-RESIDENT</div>
            <div className="model">{vram.model_name || '--'}</div>
            <div className="specs">
              <b>{formatCtx(vram.ctx_size)}</b> ctx · <b>{vram.kv_cache || '--'}</b> kv
            </div>
          </div>
          <div className="slot-rate">
            <div className="num">{vram.generating ? '●' : '--'}</div>
            <div className="unit">{vram.generating ? 'GEN' : 'IDLE'}</div>
          </div>
        </div>
      </div>
    </div>
  )
}

function renderDramSlot(
  position: 'dram-left' | 'dram-right',
  slot: SlotView | null,
  predicted: boolean,
  frameRef: React.RefObject<HTMLDivElement | null>,
) {
  const pending = Boolean(slot?.pending_changes)
  const classes = ['slot', position, 'hex']
  if (predicted) classes.push('pending')
  if (pending) classes.push('pending')
  const className = classes.join(' ')

  if (!slot) {
    return (
      <div className={`slot-frame ${position}`} ref={frameRef}>
        <div className="slot-tag" style={{ color: 'var(--text-mute)' }}>
          EMPTY
        </div>
        <div className={className}>
          <svg className="hex-border" viewBox="0 0 430 140" preserveAspectRatio="none">
            <path d="M20,1 L410,1 L429,70 L410,139 L20,139 L1,70 Z" />
          </svg>
          <span className="vertex" style={{ left: '20px', top: 0 }} />
          <span className="vertex" style={{ left: 'calc(100% - 20px)', top: 0 }} />
          <span className="vertex" style={{ left: '100%', top: '50%' }} />
          <span className="vertex" style={{ left: 'calc(100% - 20px)', top: '100%' }} />
          <span className="vertex" style={{ left: '20px', top: '100%' }} />
          <span className="vertex" style={{ left: 0, top: '50%' }} />
          <div className="slot-inner">
            <div className="slot-glyph">·</div>
            <div className="slot-meta">
              <div className="role">NO SLOT</div>
              <div className="model">--</div>
              <div className="specs">
                <b>--</b> ctx · <b>--</b> kv
              </div>
            </div>
            <div className="slot-rate">
              <div className="num dash">--</div>
              <div className="unit">IDLE</div>
            </div>
          </div>
        </div>
      </div>
    )
  }

  const letter = slotLetter(slot)
  const tagColor = predicted ? 'var(--amber)' : 'var(--text-mute)'
  const roleColor = predicted ? 'var(--amber)' : undefined
  const tagText = predicted ? `SLOT ${letter} · PREDICTED` : `SLOT ${letter} · DRAM`
  const roleText = `${displayGlyphForSlot(slot)} · ${predicted ? 'INBOUND' : 'WARM'}`

  return (
    <div className={`slot-frame ${position}`} ref={frameRef}>
      <div className="slot-tag" style={{ color: tagColor }}>
        {tagText}
      </div>
      <div className={className}>
        <svg className="hex-border" viewBox="0 0 430 140" preserveAspectRatio="none">
          <path d="M20,1 L410,1 L429,70 L410,139 L20,139 L1,70 Z" />
        </svg>
        <span className="vertex" style={{ left: '20px', top: 0 }} />
        <span className="vertex" style={{ left: 'calc(100% - 20px)', top: 0 }} />
        <span className="vertex" style={{ left: '100%', top: '50%' }} />
        <span className="vertex" style={{ left: 'calc(100% - 20px)', top: '100%' }} />
        <span className="vertex" style={{ left: '20px', top: '100%' }} />
        <span className="vertex" style={{ left: 0, top: '50%' }} />
        <div className="slot-inner">
          <div className="slot-glyph">{position === 'dram-left' ? '▢' : '▲'}</div>
          <div className="slot-meta">
            <div className="role" style={roleColor ? { color: roleColor } : undefined}>
              {roleText}
            </div>
            <div className="model">{slot.model_name || '--'}</div>
            <div className="specs">
              <b>{formatCtx(slot.ctx_size)}</b> ctx · <b>{slot.kv_cache || '--'}</b> kv
            </div>
          </div>
          <div className="slot-rate">
            <div className="num dash">--</div>
            <div className="unit">{predicted ? 'WARM' : 'IDLE'}</div>
          </div>
        </div>
      </div>
    </div>
  )
}
