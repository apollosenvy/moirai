import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type CSSProperties,
} from 'react'
import { createPortal } from 'react-dom'

interface ModelOption {
  path: string
  label: string
}

interface ModelDropdownProps {
  value: string
  /** Extra inline style; used for the VRAM-active variant that tints
   *  the .combo with an ice border and translucent ice background. */
  style?: CSSProperties
  options?: ModelOption[]
  onSelect?: (path: string) => void
}

// Combo-style model selector. Renders closed by default; clicking
// opens a dropdown populated from the live /models catalog. The
// dropdown is portalled to document.body so parent clip-paths (e.g. the
// notch card around slot controls) can't hide it -- same pattern as
// KvQuantDropdown.
export default function ModelDropdown({
  value,
  style,
  options,
  onSelect,
}: ModelDropdownProps) {
  const [open, setOpen] = useState(false)
  const wrapRef = useRef<HTMLDivElement | null>(null)
  const comboRef = useRef<HTMLDivElement | null>(null)
  const panelRef = useRef<HTMLDivElement | null>(null)
  const [coords, setCoords] = useState<{
    left: number
    top: number
    width: number
    placeAbove: boolean
  } | null>(null)
  const canOpen = Boolean(options && options.length > 0 && onSelect)
  const comboStyle: CSSProperties = {
    ...(style ?? {}),
    ...(canOpen ? { cursor: 'pointer' } : { cursor: 'default' }),
  }

  // Close on outside click + Escape.
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

  // Position the portal panel against the combo. Flips to open upward
  // when there's more room above than below (e.g. VRAM slot near the
  // bottom of the fabric). rAF-throttled to match KvQuantDropdown.
  useLayoutEffect(() => {
    if (!open) {
      setCoords(null)
      return
    }
    const reposition = () => {
      const combo = comboRef.current
      if (!combo) return
      const r = combo.getBoundingClientRect()
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
            {options.map((opt) => (
              <div
                key={opt.path}
                className="dd-item"
                onClick={(e) => {
                  e.stopPropagation()
                  onSelect(opt.path)
                  setOpen(false)
                }}
              >
                <span>{opt.label}</span>
                <span className="size">{shorten(opt.path)}</span>
                <span />
              </div>
            ))}
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
        className="combo"
        style={comboStyle}
        onClick={() => {
          if (canOpen) setOpen((v) => !v)
        }}
      >
        <span className="val">{value}</span>
        <span className="chev">▾</span>
      </div>
      {panel}
    </div>
  )
}

function shorten(path: string): string {
  if (path.length <= 28) return path
  return `…${path.slice(path.length - 26)}`
}
