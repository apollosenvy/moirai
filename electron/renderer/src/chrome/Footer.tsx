// 28px-tall bottom bar. Left cluster shows app identity; right cluster
// shows keyboard shortcut hints.
export default function Footer() {
  return (
    <footer className="footer">
      <div>
        <span>localhost</span>
        <span className="dim">·</span>
        <span>electron 32.0.0</span>
        <span className="dim">·</span>
        <span>moirai 0.4.2</span>
        <span className="dim">·</span>
        <span>webkitgtk 6.4.1</span>
      </div>
      <div>
        <span className="kbd">?</span> help
        <span className="dim">·</span>
        <span className="kbd">⌃K</span> palette
        <span className="dim">·</span>
        <span className="kbd">⌃␣</span> focus fabric
        <span className="dim">·</span>
        <span className="kbd">F12</span> inspect
      </div>
    </footer>
  )
}
