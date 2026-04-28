// Connector paths + arrow markers that overlay the visualizer fabric.
// The resting (trace), active swap (ice), and predicted (amber dashed)
// lines are computed from real panel positions at render time by
// RouteVisualizer and passed in as path strings so the lines stay
// attached to their panels when layout shifts.

export interface ConnectorPaths {
  /** VRAM-bottom -> DRAM-left -- resting trace (arrow) */
  rest: string
  /** VRAM-bottom -> DRAM-right -- active swap (ice, arrow) */
  swap: string
  /** Same curve as rest, offset down a few px -- predicted (amber dashed) */
  pred: string
  /** DRAM-left <-> DRAM-right -- static trace across the bottom */
  cross: string
  /** Reversed swap path used by PlasmaPulse's <mpath href="#path-b" />.
      This is the swap line run DRAM-right -> VRAM, i.e. the direction
      the plasma dot travels. */
  pathB: string
}

interface Props {
  paths: ConnectorPaths
}

export default function ConnectorLines({ paths }: Props) {
  return (
    <>
      <defs>
        <marker
          id="arrow-ice"
          viewBox="0 0 10 10"
          refX="5"
          refY="5"
          markerWidth="7"
          markerHeight="7"
          orient="auto"
        >
          <path d="M0,0 L10,5 L0,10 Z" fill="#00c8ff" />
        </marker>
        <marker
          id="arrow-trace"
          viewBox="0 0 10 10"
          refX="5"
          refY="5"
          markerWidth="6"
          markerHeight="6"
          orient="auto"
        >
          <path d="M0,0 L10,5 L0,10 Z" fill="#1f3355" />
        </marker>
        <marker
          id="arrow-amber"
          viewBox="0 0 10 10"
          refX="5"
          refY="5"
          markerWidth="6"
          markerHeight="6"
          orient="auto"
        >
          <path d="M0,0 L10,5 L0,10 Z" fill="#ffb347" />
        </marker>

        {/* Reusable travel path for the plasma pulse motion. Force-remount
            via key on the path geometry so WebKit's animateMotion picks up
            new geometry after a layout change instead of caching the old
            curve at animation-start time. */}
        <path key={paths.pathB} id="path-b" d={paths.pathB} />
      </defs>

      {/* Resting connector to DRAM-left (A) */}
      <path className="rest-line" d={paths.rest} markerEnd="url(#arrow-trace)" />

      {/* Active swap in flight: VRAM <-> DRAM-right (B) */}
      <path className="swap-line" d={paths.swap} markerEnd="url(#arrow-ice)" />

      {/* Predicted next (amber dashed) -- shown as a secondary candidate
          to DRAM-left */}
      <path className="pred-line" d={paths.pred} />

      {/* DRAM <-> DRAM static trace */}
      <path className="rest-line" d={paths.cross} opacity="0.35" />
    </>
  )
}
