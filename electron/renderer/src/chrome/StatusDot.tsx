import type { CSSProperties } from 'react'

export type StatusDotVariant =
  | 'lime'
  | 'amber'
  | 'amber-steady'
  | 'ice'
  | 'ice-dim'
  | 'magenta'
  | 'mute'

interface StatusDotProps {
  variant: StatusDotVariant
  style?: CSSProperties
}

// The pulsing/steady status dots used across header, slot cards and
// task rows. Class names preserved from the source HTML (.dot.lime etc).
export default function StatusDot({ variant, style }: StatusDotProps) {
  return <span className={`dot ${variant}`} style={style} />
}
