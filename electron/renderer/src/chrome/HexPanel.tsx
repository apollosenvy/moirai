import type { CSSProperties, ReactNode } from 'react'

interface HexPanelProps {
  /** Additional modifier classes (active, pending, alert etc.) */
  className?: string
  /** ViewBox dimensions for the border SVG, matching the panel size. */
  viewBox: string
  /** The d="" attribute for the hex border path. */
  borderPath: string
  /** Inline style passthrough for positioning. */
  style?: CSSProperties
  children: ReactNode
}

// Reusable hex-shaped panel. Mirrors the inline SVG + .vertex dot pattern
// from the design HTML. Callers pass the viewBox + path verbatim so the
// geometry per-panel stays identical to the source.
export default function HexPanel({
  className = '',
  viewBox,
  borderPath,
  style,
  children,
}: HexPanelProps) {
  return (
    <div className={`hex ${className}`.trim()} style={style}>
      <svg className="hex-border" viewBox={viewBox} preserveAspectRatio="none">
        <path d={borderPath} />
      </svg>
      <span className="vertex" style={{ left: '20px', top: 0 }} />
      <span className="vertex" style={{ left: 'calc(100% - 20px)', top: 0 }} />
      <span className="vertex" style={{ left: '100%', top: '50%' }} />
      <span className="vertex" style={{ left: 'calc(100% - 20px)', top: '100%' }} />
      <span className="vertex" style={{ left: '20px', top: '100%' }} />
      <span className="vertex" style={{ left: 0, top: '50%' }} />
      {children}
    </div>
  )
}
