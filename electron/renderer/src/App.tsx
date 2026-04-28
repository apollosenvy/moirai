import { useCallback, useEffect, useState } from 'react'
import Backdrop from './chrome/Backdrop'
import BroadcastBand from './chrome/BroadcastBand'
import CommandPalette from './chrome/CommandPalette'
import Footer from './chrome/Footer'
import Header, { type AppView } from './chrome/Header'
import HelpOverlay from './chrome/HelpOverlay'
import LeftRail from './chrome/LeftRail'
import TraceSparks from './chrome/TraceSparks'
import RouteVisualizer from './visualizer/RouteVisualizer'
import SlotControls from './slots/SlotControls'
import TaskDetail from './tasks/TaskDetail'
import TaskList from './tasks/TaskList'
import TaskSubmit from './tasks/TaskSubmit'
import MetricsView from './views/MetricsView'
import SettingsView from './views/SettingsView'
import { useDaemonStore } from './store/daemonStore'
import { useTasksStore } from './store/tasksStore'
import { createDaemonClient, type DaemonStatus } from './lib/daemonClient'

// Top-level app shell. Mirrors the source design's flexible two-view
// layout (ROUTING | TASK). Wires bridge.on('daemon-status') for live
// connection state and polls /ready /slots /tasks every 3s. Shows an
// offline panel until daemon-status reports connected=true, a warming
// panel until /ready reports 200, then the normal routing + task views.
export default function App() {
  const [view, setView] = useState<AppView>('routing')
  const [paletteOpen, setPaletteOpen] = useState(false)
  const [helpOpen, setHelpOpen] = useState(false)

  const connected = useDaemonStore((s) => s.connected)
  const ready = useDaemonStore((s) => s.ready)
  const setConnected = useDaemonStore((s) => s.setConnected)
  const setReady = useDaemonStore((s) => s.setReady)
  const setStatus = useDaemonStore((s) => s.setStatus)
  const setSlots = useDaemonStore((s) => s.setSlots)

  const selectedId = useTasksStore((s) => s.selectedId)
  const setList = useTasksStore((s) => s.setList)
  const setDetail = useTasksStore((s) => s.setDetail)

  // daemon-status stream from the Phos bridge -- fires every ~1.5s with
  // a parsed /status payload OR {connected: false, error} on failure.
  useEffect(() => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const bridge = (window as any).moirai
    if (!bridge || typeof bridge.on !== 'function') return
    const handler = (payload: unknown) => {
      if (!payload || typeof payload !== 'object') return
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const p = payload as any
      if (p.connected === false) {
        setConnected(false)
        setReady(false)
        return
      }
      setConnected(true)
      setStatus(p as DaemonStatus)
    }
    const off = bridge.on('daemon-status', handler)
    // StrictMode runs effects twice in dev; if bridge.on returns something
    // other than a Function (e.g. an undefined return, a listener id), the
    // first cleanup is a no-op and the second mount installs a duplicate
    // handler. Guard against both shapes: prefer the Function return,
    // otherwise call bridge.off?.(channel, handler) explicitly.
    return () => {
      if (typeof off === 'function') {
        off()
      } else if (typeof bridge.off === 'function') {
        try {
          bridge.off('daemon-status', handler)
        } catch {
          /* defensive */
        }
      }
    }
  }, [setConnected, setReady, setStatus])

  // Poll /ready, /slots, /tasks every 3s while connected. Self-scheduling
  // setTimeout (instead of setInterval) so a slow tick never stacks: the
  // next fetch only schedules AFTER the current one resolves. This fixes
  // the "polling race" where setInterval kept firing even when the last
  // fetch was still in flight, pinning the UI during daemon hiccups.
  useEffect(() => {
    if (!connected) {
      setReady(false)
      return
    }
    const client = createDaemonClient()
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | null = null

    const tick = async () => {
      try {
        const [readyRes, slots, tasks] = await Promise.all([
          client.getReady().catch(() => false),
          client.getSlots().catch(() => []),
          client.listTasks().catch(() => []),
        ])
        if (cancelled) return
        setReady(Boolean(readyRes))
        setSlots(slots)
        setList(tasks)
      } catch {
        /* handled individually above */
      }
    }

    const schedule = () => {
      if (cancelled) return
      timer = setTimeout(async () => {
        await tick()
        schedule()
      }, 3000)
    }

    // Fire once immediately, then self-schedule.
    tick().then(() => schedule())

    return () => {
      cancelled = true
      if (timer) clearTimeout(timer)
    }
  }, [connected, setReady, setSlots, setList])

  // Fetch task detail when selection changes AND re-poll every 3s while
  // the selection is held. Without the re-poll, a user watching a running
  // task would see the detail freeze on the first-fetch snapshot -- no
  // new plan, no new reviews, no new trace events. That was the #1 cause
  // of the "UI feels dead" complaint.
  //
  // Self-scheduling setTimeout (same pattern as the top poll) so a slow
  // getTask() call can't stack with the next tick.
  useEffect(() => {
    if (!selectedId) {
      setDetail(null)
      return
    }
    if (!connected) return
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | null = null
    const client = createDaemonClient()

    const fetchDetail = async () => {
      try {
        const detail = await client.getTask(selectedId)
        if (!cancelled) setDetail(detail)
      } catch {
        if (!cancelled) setDetail(null)
      }
    }

    const schedule = () => {
      if (cancelled) return
      timer = setTimeout(async () => {
        await fetchDetail()
        schedule()
      }, 3000)
    }

    fetchDetail().then(() => schedule())

    return () => {
      cancelled = true
      if (timer) clearTimeout(timer)
    }
  }, [selectedId, connected, setDetail])

  // Global keyboard shortcuts. Palette (Ctrl/Cmd+K), help (?), and the
  // 1/2/3/4 view switches are all wired here so they fire from anywhere
  // in the shell. Skip when focus is inside an input/textarea/contenteditable
  // so the user can still type a literal "?" or "1" inside the composer
  // without bouncing the view.
  const handleKey = useCallback((e: KeyboardEvent) => {
    const target = e.target as HTMLElement | null
    const tag = target?.tagName ?? ''
    const inEditable =
      tag === 'INPUT' ||
      tag === 'TEXTAREA' ||
      tag === 'SELECT' ||
      target?.isContentEditable === true

    // Ctrl+K / Cmd+K opens palette regardless of focus context.
    if ((e.ctrlKey || e.metaKey) && (e.key === 'k' || e.key === 'K')) {
      e.preventDefault()
      setPaletteOpen((v) => !v)
      setHelpOpen(false)
      return
    }

    if (inEditable) return

    if (e.key === '?') {
      e.preventDefault()
      setHelpOpen((v) => !v)
      setPaletteOpen(false)
      return
    }

    if (e.key === '1') {
      setView('routing')
    } else if (e.key === '2') {
      setView('task')
    } else if (e.key === '3') {
      setView('metrics')
    } else if (e.key === '4') {
      setView('settings')
    }
  }, [])

  useEffect(() => {
    window.addEventListener('keydown', handleKey)
    return () => window.removeEventListener('keydown', handleKey)
  }, [handleKey])

  if (!connected) {
    return <OfflinePanel />
  }

  if (!ready) {
    return <WarmingPanel />
  }

  return (
    <>
      <Backdrop />
      <TraceSparks />
      <BroadcastBand />
      <div className="app" data-view={view}>
        <Header activeView={view} onSwitchView={setView} />
        <LeftRail activeView={view} onSwitchView={setView} />

        {/* ROUTING VIEW */}
        <div className="view view-routing">
          <main className="main">
            <RouteVisualizer />
            <SlotControls />
            <section className="bottom">
              <TaskSubmit />
              <TaskList />
            </section>
          </main>
        </div>

        {/* TASK VIEW */}
        <div className="view view-task">
          <TaskDetail />
        </div>

        {/* METRICS VIEW */}
        <div className="view view-metrics">
          <MetricsView />
        </div>

        {/* SETTINGS VIEW */}
        <div className="view view-settings">
          <SettingsView />
        </div>

        <Footer />
      </div>
      <CommandPalette
        open={paletteOpen}
        onClose={() => setPaletteOpen(false)}
        onSwitchView={setView}
      />
      <HelpOverlay open={helpOpen} onClose={() => setHelpOpen(false)} />
    </>
  )
}

function OfflinePanel() {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const spawn = async () => {
    setBusy(true)
    setError(null)
    // Guard against a hung daemon-spawn IPC. The C++ side blocks up to
    // ~40s polling /health and /ready; if posix_spawn exits 0 but the
    // opener wedges, the React tree freezes on this panel forever
    // without the timeout. We use AbortController + clearTimeout so
    // the 45s timer is freed when invoke wins (the previous Promise.race
    // approach left the timer alive and produced an unobserved
    // rejection).
    const SPAWN_TIMEOUT_MS = 45000
    let timer: ReturnType<typeof setTimeout> | null = null
    const ac = new AbortController()
    try {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const bridge = (window as any).moirai
      if (!bridge || typeof bridge.invoke !== 'function') {
        throw new Error('moirai bridge unavailable')
      }
      const timeoutPromise = new Promise<never>((_, reject) => {
        timer = setTimeout(() => {
          ac.abort()
          reject(new Error('spawn timed out after 45s'))
        }, SPAWN_TIMEOUT_MS)
      })
      const res = (await Promise.race([
        bridge.invoke('daemon-spawn', {}),
        timeoutPromise,
      ])) as { error?: string } | undefined
      if (res?.error) setError(res.error)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      if (timer) clearTimeout(timer)
      setBusy(false)
    }
  }

  return (
    <>
      <Backdrop />
      <div className="app" data-view="offline" data-offline>
        <div
          style={{
            position: 'absolute',
            inset: 0,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
          }}
        >
          <div
            style={{
              padding: '32px 40px',
              border: '1px solid var(--rule)',
              background: 'rgba(10,10,10,0.6)',
              textAlign: 'center',
              letterSpacing: '0.2em',
              fontFamily: 'var(--ff-mono-display)',
              minWidth: '320px',
            }}
          >
            <div
              style={{
                color: 'var(--magenta)',
                fontSize: '14px',
                marginBottom: '8px',
              }}
            >
              DAEMON OFFLINE
            </div>
            <div
              style={{
                color: 'var(--text-mute)',
                fontSize: '11px',
                marginBottom: '20px',
              }}
            >
              no response on :5984
            </div>
            <button
              className="submit-btn"
              onClick={spawn}
              disabled={busy}
              style={{ minWidth: '200px' }}
            >
              <span>{busy ? 'SPAWNING…' : '▸ SPAWN DAEMON'}</span>
            </button>
            {error && (
              <div
                style={{
                  color: 'var(--magenta)',
                  fontSize: '10px',
                  marginTop: '12px',
                }}
              >
                {error}
              </div>
            )}
          </div>
        </div>
      </div>
    </>
  )
}

function WarmingPanel() {
  return (
    <>
      <Backdrop />
      <div className="app" data-view="warming">
        <div
          style={{
            position: 'absolute',
            inset: 0,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
          }}
        >
          <div
            style={{
              padding: '32px 40px',
              border: '1px solid var(--rule)',
              background: 'rgba(10,10,10,0.6)',
              textAlign: 'center',
              letterSpacing: '0.2em',
              fontFamily: 'var(--ff-mono-display)',
            }}
          >
            <div
              style={{
                color: 'var(--amber)',
                fontSize: '14px',
                marginBottom: '10px',
                animation: 'status-pulse 1.2s ease-in-out infinite',
              }}
            >
              DAEMON WARMING UP
            </div>
            <div style={{ color: 'var(--text-mute)', fontSize: '11px' }}>
              waiting for /ready to return 200
            </div>
          </div>
        </div>
      </div>
    </>
  )
}
