import { useEffect, useRef, useState } from 'react'
import { usePrefsStore } from '../store/prefsStore'

// Brief §10. Ambient effect. Every 30-90s a short plasma-coloured spark
// travels a random segment of the background hex grid. Pure decoration
// -- the spark MUST never grab pointer events or steal focus, so we
// render fixed-position with `pointer-events: none` and aria-hidden.
//
// Rendered as a single SVG with one <circle> using <animateMotion> +
// <mpath>. Each scheduled tick replaces the current spark by bumping a
// React key, which remounts the SVG and restarts the animation. This is
// simpler than juggling SMIL begin attributes and the GC cost of one
// re-render every 30-90s is irrelevant.
//
// Pauses when the document is hidden: a tab switch shouldn't queue up
// dozens of sparks that all fire at once when the user comes back.

interface Spark {
  key: number
  // Pre-rendered SVG path string. We keep the path inline rather than
  // referenced via <mpath> to keep this component self-contained --
  // <ConnectorLines /> only mounts inside the visualizer.
  path: string
  duration: number
}

// Hex-edge unit length on the rendered backdrop pattern (84x48 SVG
// pattern; a hex edge there is 28 wide horizontal, 24 long along the
// 60-deg diagonal). We approximate with a single edge length so the
// path math is uniform; the visual difference is invisible at 0.3
// opacity over 400ms.
const EDGE = 26

// Six hex-edge directions in radians (horizontal hexes -> 0, 60, 120,
// 180, 240, 300 degrees).
const ANGLES = [0, 60, 120, 180, 240, 300].map((d) => (d * Math.PI) / 180)

function buildPath(width: number, height: number): string {
  // Random origin a comfortable margin inside the viewport so the spark
  // doesn't clip on entry/exit. Pick 1-3 connected hex-edge segments
  // from there.
  const margin = 80
  const x0 = margin + Math.random() * Math.max(1, width - margin * 2)
  const y0 = margin + Math.random() * Math.max(1, height - margin * 2)

  const segments = 1 + Math.floor(Math.random() * 3) // 1..3 edges
  let x = x0
  let y = y0
  let parts = `M ${x.toFixed(1)} ${y.toFixed(1)}`
  // Start with a random direction, then bias subsequent edges towards
  // a 60deg turn so the path follows the hex contour rather than
  // backtracking.
  let dirIdx = Math.floor(Math.random() * ANGLES.length)
  for (let i = 0; i < segments; i += 1) {
    const angle = ANGLES[dirIdx]
    x += Math.cos(angle) * EDGE
    y += Math.sin(angle) * EDGE
    parts += ` L ${x.toFixed(1)} ${y.toFixed(1)}`
    // Turn 60deg either direction for the next edge.
    dirIdx = (dirIdx + (Math.random() < 0.5 ? 1 : ANGLES.length - 1)) % ANGLES.length
  }
  return parts
}

function nextDelay(): number {
  // 30-90s, uniform. Brief §10.
  return 30000 + Math.random() * 60000
}

export default function TraceSparks() {
  const enabled = usePrefsStore((s) => s.traceSparks)
  const [spark, setSpark] = useState<Spark | null>(null)
  const sizeRef = useRef({ w: 0, h: 0 })

  // Track viewport size so the path generator stays inside it. We use a
  // ref + a one-time resize listener rather than state because the spark
  // generator only needs the current size at the moment it fires.
  useEffect(() => {
    const sync = () => {
      sizeRef.current = {
        w: window.innerWidth || 1600,
        h: window.innerHeight || 1000,
      }
    }
    sync()
    window.addEventListener('resize', sync)
    return () => window.removeEventListener('resize', sync)
  }, [])

  useEffect(() => {
    if (!enabled) {
      setSpark(null)
      return
    }
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | null = null

    const fire = () => {
      if (cancelled) return
      // Skip-and-reschedule when the tab is hidden so we don't waste
      // GPU cycles on an invisible animation -- and don't backlog.
      if (document.visibilityState === 'hidden') {
        schedule()
        return
      }
      const { w, h } = sizeRef.current
      const path = buildPath(w, h)
      // 350-500ms duration centred on the brief's "~400ms" target.
      const duration = 350 + Math.random() * 150
      setSpark({ key: Date.now(), path, duration })
      schedule()
    }

    const schedule = () => {
      if (cancelled) return
      timer = setTimeout(fire, nextDelay())
    }

    schedule()
    return () => {
      cancelled = true
      if (timer) clearTimeout(timer)
    }
  }, [enabled])

  if (!enabled || !spark) return null

  return (
    <svg
      key={spark.key}
      className="trace-sparks"
      aria-hidden="true"
      // Span the whole viewport in user-space coordinates so the
      // path values (raw pixels) line up with the screen.
      width="100%"
      height="100%"
      preserveAspectRatio="none"
    >
      <path id={`spark-path-${spark.key}`} d={spark.path} fill="none" stroke="none" />
      <circle r="3" fill="var(--plasma)" opacity="0">
        <animateMotion
          dur={`${spark.duration}ms`}
          fill="freeze"
          rotate="auto"
          begin="0s"
        >
          <mpath href={`#spark-path-${spark.key}`} />
        </animateMotion>
        <animate
          attributeName="opacity"
          dur={`${spark.duration}ms`}
          values="0;0.3;0.3;0"
          keyTimes="0;0.15;0.85;1"
          fill="freeze"
        />
      </circle>
    </svg>
  )
}
