import { useEffect } from 'react'

// `?` keyboard help overlay. Lists the shortcuts that the rest of the
// shell wires up. Click outside, Esc, or `?` again all close the panel.

interface HelpOverlayProps {
  open: boolean
  onClose: () => void
}

interface Shortcut {
  keys: string[]
  label: string
  group: 'NAV' | 'PALETTE' | 'TASK' | 'SHELL'
}

const SHORTCUTS: Shortcut[] = [
  { keys: ['1'], label: 'Switch to ROUTING', group: 'NAV' },
  { keys: ['2'], label: 'Switch to TASK detail', group: 'NAV' },
  { keys: ['3'], label: 'Switch to METRICS', group: 'NAV' },
  { keys: ['4'], label: 'Switch to SETTINGS', group: 'NAV' },
  { keys: ['Ctrl', 'K'], label: 'Open command palette', group: 'PALETTE' },
  { keys: ['?'], label: 'Toggle this help overlay', group: 'PALETTE' },
  { keys: ['Esc'], label: 'Close palette / help / clear selection', group: 'PALETTE' },
  { keys: ['Ctrl', 'Space'], label: 'Focus the route fabric', group: 'SHELL' },
  { keys: ['F12'], label: 'Open WebKit inspector (dev builds)', group: 'SHELL' },
  { keys: ['↑', '↓'], label: 'Navigate inside palette / lists', group: 'PALETTE' },
  { keys: ['↵'], label: 'Run selected command / open selected task', group: 'PALETTE' },
]

export default function HelpOverlay({ open, onClose }: HelpOverlayProps) {
  useEffect(() => {
    if (!open) return
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        onClose()
      }
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [open, onClose])

  if (!open) return null

  return (
    <div
      className="help-overlay"
      role="dialog"
      aria-modal="true"
      aria-label="Keyboard shortcuts"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="help-panel">
        <header className="help-hd">
          <span className="help-tag">KEYBOARD SHORTCUTS</span>
          <button
            type="button"
            className="help-close"
            aria-label="Close help"
            onClick={onClose}
          >
            ×
          </button>
        </header>
        <div className="help-body">
          {(['NAV', 'PALETTE', 'TASK', 'SHELL'] as const).map((group) => {
            const items = SHORTCUTS.filter((s) => s.group === group)
            if (items.length === 0) return null
            return (
              <section key={group} className="help-section">
                <div className="help-section-hd">{group}</div>
                <ul className="help-list">
                  {items.map((s) => (
                    <li key={s.label} className="help-row">
                      <span className="help-keys">
                        {s.keys.map((k, i) => (
                          <span key={`${s.label}-${k}-${i}`} className="kbd">
                            {k}
                          </span>
                        ))}
                      </span>
                      <span className="help-label">{s.label}</span>
                    </li>
                  ))}
                </ul>
              </section>
            )
          })}
        </div>
      </div>
    </div>
  )
}
