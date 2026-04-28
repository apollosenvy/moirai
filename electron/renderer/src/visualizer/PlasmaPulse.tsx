// The traveling plasma pulse along the active swap line.
// Implemented with SVG <animateMotion> + <mpath> referencing #path-b,
// which is rendered once by <ConnectorLines />. The two <animate>
// blocks below are verbatim from the design HTML.
export default function PlasmaPulse() {
  return (
    <circle className="plasma-dot" r="6">
      <animateMotion
        dur="0.8s"
        begin="0.8s;swap.end+1.2s"
        id="swap"
        fill="freeze"
        keyTimes="0;0.08;0.92;1"
        keySplines="0.4 0 0.2 1;0.4 0 0.2 1;0.4 0 0.2 1"
        calcMode="spline"
      >
        <mpath href="#path-b" />
      </animateMotion>
      <animate
        attributeName="opacity"
        dur="0.8s"
        begin="0.8s;swap.end+1.2s"
        fill="freeze"
        values="0;1;1;0"
        keyTimes="0;0.08;0.92;1"
      />
    </circle>
  )
}
