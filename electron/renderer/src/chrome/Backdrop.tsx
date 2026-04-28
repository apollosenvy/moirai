// Hex-grid fabric background + scanlines + vignette. Matches the
// three fixed-position layers from the design HTML.
export default function Backdrop() {
  return (
    <>
      <div className="backdrop" aria-hidden="true">
        <svg xmlns="http://www.w3.org/2000/svg">
          <defs>
            <pattern
              id="hexgrid"
              x="0"
              y="0"
              width="84"
              height="48"
              patternUnits="userSpaceOnUse"
            >
              {/* Horizontal hexagons tiled, 2x scale */}
              <path
                d="M 14 0 L 42 0 L 56 24 L 42 48 L 14 48 L 0 24 Z M 56 24 L 70 0 L 98 0 L 84 24 L 98 48 L 70 48 Z"
                fill="none"
                stroke="#33496b"
                strokeWidth="1.25"
                strokeOpacity="0.38"
              />
            </pattern>
            <radialGradient id="bgfade" cx="50%" cy="40%" r="70%">
              <stop offset="0%" stopColor="#070c16" stopOpacity="1" />
              <stop offset="100%" stopColor="#04060b" stopOpacity="1" />
            </radialGradient>
            <radialGradient id="gridmask" cx="50%" cy="45%" r="85%">
              <stop offset="0%" stopColor="#fff" stopOpacity="1" />
              <stop offset="75%" stopColor="#fff" stopOpacity="0.85" />
              <stop offset="100%" stopColor="#fff" stopOpacity="0.35" />
            </radialGradient>
            <mask id="grid-vignette">
              <rect width="100%" height="100%" fill="url(#gridmask)" />
            </mask>
          </defs>
          <rect width="100%" height="100%" fill="url(#bgfade)" />
          <rect width="100%" height="100%" fill="url(#hexgrid)" mask="url(#grid-vignette)" />
        </svg>
      </div>
      <div className="scanlines" />
      <div className="vignette" />
    </>
  )
}
