import type { ReactNode } from 'react'

interface Tick {
  /** Left offset as a CSS percentage string, e.g. "25%". */
  left: string
  /** Major ticks render taller and brighter. */
  major?: boolean
}

interface ContextSliderProps {
  /** Fill + thumb position as a CSS percentage string, e.g. "50%". */
  percent: string
  ticks: Tick[]
  /** Value label rendered to the right (e.g. "256K" or "128K"). */
  valueLabel: ReactNode
  /** Called with the clicked percentage (0-100) when the track is clicked. */
  onChange?: (percent: number) => void
}

// Slider control for per-slot ctx length. Pure CSS-styled rail with
// rotated-square thumb. Clicking the track computes a relative position
// and emits onChange(percent); SlotControls maps that to the nearest
// power-of-two ctx_size.
export default function ContextSlider({
  percent,
  ticks,
  valueLabel,
  onChange,
}: ContextSliderProps) {
  return (
    <div className="slider">
      <div
        className="track"
        onClick={(e) => {
          if (!onChange) return
          const rect = (e.currentTarget as HTMLDivElement).getBoundingClientRect()
          const x = e.clientX - rect.left
          const pct = Math.max(0, Math.min(100, (x / rect.width) * 100))
          onChange(pct)
        }}
        style={onChange ? { cursor: 'pointer' } : undefined}
      >
        <div className="fill" style={{ width: percent }} />
        <div className="thumb" style={{ left: percent }} />
        <div className="ticks">
          {ticks.map((t, i) => (
            <span
              key={i}
              className={`tick${t.major ? ' major' : ''}`}
              style={{ left: t.left }}
            />
          ))}
        </div>
      </div>
      <div className="slider-val">{valueLabel}</div>
    </div>
  )
}
