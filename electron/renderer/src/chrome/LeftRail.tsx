import type { AppView } from './Header'
import { useClickableDiv } from '../lib/a11y'

interface LeftRailProps {
  activeView: AppView
  onSwitchView: (view: AppView) => void
}

// 60px vertical nav rail. All four primary views (Routing, Tasks,
// Metrics, Settings) are wired to the view switcher. The active view
// gets the `.active` class for the ice left-indicator bar; CSS in
// fabric.css extends per-view colour so a glance at the rail tells you
// where you are.
export default function LeftRail({ activeView, onSwitchView }: LeftRailProps) {
  const routingClick = useClickableDiv(() => onSwitchView('routing'))
  const tasksClick = useClickableDiv(() => onSwitchView('task'))
  const metricsClick = useClickableDiv(() => onSwitchView('metrics'))
  const settingsClick = useClickableDiv(() => onSwitchView('settings'))
  return (
    <nav className="rail">
      <div
        {...routingClick}
        className={`rail-btn${activeView === 'routing' ? ' active' : ''}`}
        data-nav="routing"
        title="Routing (1)"
        aria-label="Routing"
        aria-pressed={activeView === 'routing'}
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5">
          <circle cx="12" cy="5" r="2" />
          <circle cx="5" cy="19" r="2" />
          <circle cx="19" cy="19" r="2" />
          <path d="M12 7v4l-6 7M12 11l6 7" />
        </svg>
      </div>
      <div
        {...tasksClick}
        className={`rail-btn${activeView === 'task' ? ' active' : ''}`}
        data-nav="tasks"
        title="Tasks (2)"
        aria-label="Tasks"
        aria-pressed={activeView === 'task'}
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5">
          <path d="M4 6h16M4 12h16M4 18h10" />
        </svg>
      </div>
      <div
        {...metricsClick}
        className={`rail-btn${activeView === 'metrics' ? ' active' : ''}`}
        data-nav="metrics"
        title="Metrics (3)"
        aria-label="Metrics"
        aria-pressed={activeView === 'metrics'}
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5">
          <path d="M4 20V10M10 20V4M16 20v-8M22 20h-20" />
        </svg>
      </div>
      <div
        {...settingsClick}
        className={`rail-btn${activeView === 'settings' ? ' active' : ''}`}
        data-nav="settings"
        title="Settings (4)"
        aria-label="Settings"
        aria-pressed={activeView === 'settings'}
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5">
          <circle cx="12" cy="12" r="3" />
          <path d="M12 2v3M12 19v3M2 12h3M19 12h3M4.9 4.9l2.1 2.1M17 17l2.1 2.1M4.9 19.1L7 17M17 7l2.1-2.1" />
        </svg>
      </div>
      <div className="rail-num">PHOS · 7900XTX</div>
    </nav>
  )
}
