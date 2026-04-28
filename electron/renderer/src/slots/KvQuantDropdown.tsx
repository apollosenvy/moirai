import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'

export interface KvOption {
  label: string
  size: string
  ratio?: string
  kv: string
  /** True when this option is only valid on turbo-supported hardware. */
  turbo?: boolean
}

interface KvQuantDropdownProps {
  /** The currently-chosen display value shown in the combo field. */
  value: string
  /** Adds .warn border to the combo (e.g. when turbo3 is selected on a
   *  head_dim=128 model). */
  warn?: boolean
  /** The currently-selected kv key (matches one of options[].kv). */
  selectedKv?: string
  /** Full list of options rendered in the open panel. */
  options?: KvOption[]
  /** True if the hardware supports turbo* kv quants; disables turbo
   *  options when false. */
  turboSupported?: boolean
  /** Fires when a non-disabled option is clicked. */
  onSelect?: (kv: string) => void
  /** Extra inline style. */
  style?: React.CSSProperties
  /** Optional children rendered inside the combo after the chevron,
   *  used by the showcase dropdown overlay. */
  children?: ReactNode
}

// KV-cache quant dropdown. Manages its own open state so the combo
// and dropdown panel live outside each other (the combo uses clip-path
// which would otherwise hide the panel). Click-outside and Escape
// close it.
export default function KvQuantDropdown({
  value,
  warn = false,
  selectedKv,
  options,
  turboSupported = false,
  onSelect,
  style,
  children,
}: KvQuantDropdownProps) {
  const [open, setOpen] = useState(false)
  const wrapRef = useRef<HTMLDivElement | null>(null)
  const comboRef = useRef<HTMLDivElement | null>(null)
  const panelRef = useRef<HTMLDivElement | null>(null)
  // coords is set each time the panel opens and on scroll/resize while
  // open. We portal the panel to document.body to escape the notch
  // card's clip-path, so we need absolute viewport coords instead of
  // relative top:100% positioning.
  const [coords, setCoords] = useState<{
    left: number
    top: number
    width: number
    placeAbove: boolean
  } | null>(null)
  const canOpen = Boolean(options && options.length > 0 && onSelect)
  const className = `combo${warn ? ' warn' : ''}`

  useEffect(() => {
    if (!open) return
    const onDocClick = (e: MouseEvent) => {
      const target = e.target as Node
      if (wrapRef.current?.contains(target)) return
      if (panelRef.current?.contains(target)) return
      setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onDocClick)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDocClick)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  // Position the portal panel against the combo whenever it opens or the
  // viewport changes while it's open. If there's not enough room below
  // the combo for the full option list, flip to open above instead.
  //
  // Scroll/resize are throttled via requestAnimationFrame so a high-rate
  // scroll (trace panel firing events at 60+ fps) can't land reposition +
  // setState on every frame. Without throttling, holding the dropdown
  // open while the trace panel streamed caused visible jank on the notch.
  useLayoutEffect(() => {
    if (!open) {
      setCoords(null)
      return
    }
    const reposition = () => {
      const combo = comboRef.current
      if (!combo) return
      const r = combo.getBoundingClientRect()
      // Estimate panel height: 30px per item + 8px padding. Conservative
      // so the flip decision is stable even if the panel hasn't rendered
      // yet. This is a floor -- actual scroll still works when cramped.
      const rowH = 30
      const pad = 8
      const itemCount = options?.length ?? 0
      const wantedH = itemCount * rowH + pad
      const spaceBelow = window.innerHeight - r.bottom - 8
      const spaceAbove = r.top - 8
      const placeAbove = wantedH > spaceBelow && spaceAbove > spaceBelow
      const next = {
        left: r.left,
        top: placeAbove ? r.top : r.bottom,
        width: r.width,
        placeAbove,
      }
      setCoords((prev) =>
        prev &&
        prev.left === next.left &&
        prev.top === next.top &&
        prev.width === next.width &&
        prev.placeAbove === next.placeAbove
          ? prev
          : next,
      )
    }
    let pending = false
    let rafId: number | null = null
    const schedule = () => {
      if (pending) return
      pending = true
      rafId =
        typeof requestAnimationFrame !== 'undefined'
          ? requestAnimationFrame(() => {
              pending = false
              rafId = null
              reposition()
            })
          : (setTimeout(() => {
              pending = false
              rafId = null
              reposition()
            }, 16) as unknown as number)
    }
    reposition()
    window.addEventListener('resize', schedule)
    window.addEventListener('scroll', schedule, true)
    return () => {
      window.removeEventListener('resize', schedule)
      window.removeEventListener('scroll', schedule, true)
      if (rafId != null) {
        if (typeof cancelAnimationFrame !== 'undefined') {
          cancelAnimationFrame(rafId)
        } else {
          clearTimeout(rafId as unknown as ReturnType<typeof setTimeout>)
        }
      }
    }
  }, [open, options])

  const panel =
    open && coords && options && onSelect
      ? createPortal(
          <div
            ref={panelRef}
            className="dropdown"
            style={{
              position: 'fixed',
              left: coords.left,
              width: coords.width,
              // When placing above, anchor bottom to the combo's top so
              // the panel grows upward; translate -100% lifts it above
              // the combo without overlapping.
              top: coords.placeAbove
                ? undefined
                : Math.round(coords.top + 4),
              bottom: coords.placeAbove
                ? Math.round(window.innerHeight - coords.top + 4)
                : undefined,
              maxHeight: `min(${
                coords.placeAbove
                  ? Math.max(120, coords.top - 16)
                  : Math.max(120, window.innerHeight - coords.top - 16)
              }px, 320px)`,
              overflowY: 'auto',
              zIndex: 1000,
            }}
          >
            {options.map((opt) => {
              const disabled = Boolean(opt.turbo) && !turboSupported
              const selected = opt.kv === selectedKv
              const itemClass = [
                'dd-item',
                selected ? 'selected' : '',
                disabled ? 'disabled' : '',
              ]
                .filter(Boolean)
                .join(' ')
              return (
                <div
                  key={opt.kv}
                  className={itemClass}
                  onClick={(e) => {
                    e.stopPropagation()
                    if (disabled) return
                    onSelect(opt.kv)
                    setOpen(false)
                  }}
                >
                  <span>{opt.label}</span>
                  <span className="size">{opt.size}</span>
                  {opt.ratio ? (
                    <span className="ratio">{opt.ratio}</span>
                  ) : (
                    <span />
                  )}
                </div>
              )
            })}
          </div>,
          document.body,
        )
      : null

  return (
    <div
      ref={wrapRef}
      style={{ position: 'relative', width: '100%', minWidth: 0 }}
    >
      <div
        ref={comboRef}
        className={className}
        style={{ ...(style ?? {}), cursor: canOpen ? 'pointer' : 'default' }}
        onClick={() => {
          if (canOpen) setOpen((v) => !v)
        }}
      >
        <span className="val">{value}</span>
        <span className="chev">▾</span>
        {children}
      </div>
      {panel}
    </div>
  )
}
