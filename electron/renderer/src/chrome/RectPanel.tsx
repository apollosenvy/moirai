import type { CSSProperties, ReactNode } from 'react'

interface RectPanelProps {
  className?: string
  viewBox: string
  borderPath: string
  /** Optional stroke override for the border path (e.g. "#00c8ff"). */
  stroke?: string
  strokeOpacity?: number
  style?: CSSProperties
  children: ReactNode
}

// Notched rectangular panel: one clipped corner in the top-right.
// Used for slot controls, tasks table, task metadata bar, task panels.
export default function RectPanel({
  className = '',
  viewBox,
  borderPath,
  stroke,
  strokeOpacity,
  style,
  children,
}: RectPanelProps) {
  return (
    <div className={`notch ${className}`.trim()} style={style}>
      <svg className="notch-border" viewBox={viewBox} preserveAspectRatio="none">
        <path d={borderPath} stroke={stroke} strokeOpacity={strokeOpacity} />
      </svg>
      {children}
    </div>
  )
}
