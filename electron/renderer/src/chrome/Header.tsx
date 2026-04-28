import StatusDot from './StatusDot'
import { useDaemonStore } from '../store/daemonStore'
import { useTasksStore } from '../store/tasksStore'
import { displayGlyphForSlot } from '../lib/slotView'

export type AppView = 'routing' | 'task' | 'metrics' | 'settings'

interface HeaderProps {
  activeView: AppView
  onSwitchView: (view: AppView) => void
}

// Top bar: wordmark, connection status, active-slot chip, view tabs,
// counters, uptime. The view tabs come from the source HTML's inline
// switcher; clicking the close glyph on the TASK tab falls back to the
// ROUTING view, matching the original JS behavior. Numbers + status
// text are driven by daemonStore + tasksStore.
export default function Header({ activeView, onSwitchView }: HeaderProps) {
  const connected = useDaemonStore((s) => s.connected)
  const status = useDaemonStore((s) => s.status)
  const slots = useDaemonStore((s) => s.slots)
  const selectedId = useTasksStore((s) => s.selectedId)

  const handleTabClick = (
    view: AppView,
    e: React.MouseEvent<HTMLDivElement>,
  ) => {
    const target = e.target as HTMLElement
    if (target.classList.contains('close')) {
      onSwitchView('routing')
      return
    }
    onSwitchView(view)
  }

  const activeSlot = slots.find((s) => s.loaded) ?? null
  const activeGlyph = displayGlyphForSlot(activeSlot)
  const activeLabel = activeSlot
    ? `${activeGlyph} · ${activeSlot.slot.toUpperCase()}`
    : '-- · IDLE'

  const running = status?.running ?? 0
  const uptime = status?.uptime ?? '--'
  const version = status?.daemon_version ?? '--'
  const port = status?.port ?? 5984

  const taskLabel = selectedId ?? 'T-----'

  return (
    <header className="header">
      <div className="wordmark" aria-label="MOIRAI">
        M<span className="join">⋈</span>IRAI
      </div>

      <div className="h-status">
        <StatusDot variant={connected ? 'lime' : 'magenta'} />
        <span>{connected ? 'CONNECTED' : 'OFFLINE'}</span>
        <span className="h-port">:{port}</span>
      </div>

      <div className="h-active">
        <span className="sq" />
        <span>{activeLabel}</span>
      </div>

      <div className="view-tabs">
        <div
          className={`v-tab${activeView === 'routing' ? ' active' : ''}`}
          data-view-target="routing"
          role="button"
          tabIndex={0}
          aria-pressed={activeView === 'routing'}
          onClick={(e) => handleTabClick('routing', e)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ' || e.key === 'Spacebar') {
              e.preventDefault()
              onSwitchView('routing')
            }
          }}
        >
          ROUTING
        </div>
        <div
          className={`v-tab${activeView === 'task' ? ' active' : ''}`}
          data-view-target="task"
          role="button"
          tabIndex={0}
          aria-pressed={activeView === 'task'}
          onClick={(e) => handleTabClick('task', e)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ' || e.key === 'Spacebar') {
              e.preventDefault()
              onSwitchView('task')
            }
          }}
        >
          TASK<span className="tid">{taskLabel}</span>
          <span className="close">×</span>
        </div>
        <div
          className={`v-tab${activeView === 'metrics' ? ' active' : ''}`}
          data-view-target="metrics"
          role="button"
          tabIndex={0}
          aria-pressed={activeView === 'metrics'}
          onClick={() => onSwitchView('metrics')}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ' || e.key === 'Spacebar') {
              e.preventDefault()
              onSwitchView('metrics')
            }
          }}
        >
          METRICS
        </div>
        <div
          className={`v-tab${activeView === 'settings' ? ' active' : ''}`}
          data-view-target="settings"
          role="button"
          tabIndex={0}
          aria-pressed={activeView === 'settings'}
          onClick={() => onSwitchView('settings')}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ' || e.key === 'Spacebar') {
              e.preventDefault()
              onSwitchView('settings')
            }
          }}
        >
          SETTINGS
        </div>
      </div>

      <div className="h-spacer" />

      <div className="h-counter">
        {running}
        <span className="label">RUNNING</span>
      </div>

      <div className="h-status">
        <StatusDot variant="ice-dim" />
        <span>FABRIC</span>
        <span className="h-port">{version}</span>
      </div>

      <div className="h-uptime">{uptime}</div>
    </header>
  )
}
