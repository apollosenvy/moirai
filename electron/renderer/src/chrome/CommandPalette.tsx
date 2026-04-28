import { useEffect, useMemo, useRef, useState } from 'react'
import type { AppView } from './Header'
import { useDaemonStore } from '../store/daemonStore'
import { useTasksStore } from '../store/tasksStore'
import { createDaemonClient } from '../lib/daemonClient'

// Ctrl+K command palette. Lists view switches, daemon actions, and the
// most-recent tasks so the user can jump to a task detail without
// scrolling the table. Filtering is a simple substring + per-token
// match -- no fuzzy library dependency. Arrow keys move selection,
// Enter executes, Esc closes. Click outside the panel closes too.

export interface CommandPaletteProps {
  open: boolean
  onClose: () => void
  onSwitchView: (view: AppView) => void
}

interface PaletteCommand {
  id: string
  label: string
  hint?: string
  group: 'NAV' | 'TASK' | 'DAEMON'
  run: () => void | Promise<void>
}

function tokenMatch(haystack: string, needle: string): boolean {
  if (!needle) return true
  const hay = haystack.toLowerCase()
  const tokens = needle.toLowerCase().split(/\s+/).filter(Boolean)
  return tokens.every((t) => hay.includes(t))
}

export default function CommandPalette({
  open,
  onClose,
  onSwitchView,
}: CommandPaletteProps) {
  const [query, setQuery] = useState('')
  const [cursor, setCursor] = useState(0)
  const inputRef = useRef<HTMLInputElement | null>(null)
  const listRef = useRef<HTMLDivElement | null>(null)

  const list = useTasksStore((s) => s.list)
  const selectedId = useTasksStore((s) => s.selectedId)
  const selectTask = useTasksStore((s) => s.selectTask)
  const connected = useDaemonStore((s) => s.connected)

  // Build the command list each render. Cheap; the list is small.
  const commands = useMemo<PaletteCommand[]>(() => {
    const cmds: PaletteCommand[] = [
      {
        id: 'nav.routing',
        label: 'Go to ROUTING',
        hint: '1',
        group: 'NAV',
        run: () => onSwitchView('routing'),
      },
      {
        id: 'nav.task',
        label: 'Go to TASK detail',
        hint: '2',
        group: 'NAV',
        run: () => onSwitchView('task'),
      },
      {
        id: 'nav.metrics',
        label: 'Go to METRICS',
        hint: '3',
        group: 'NAV',
        run: () => onSwitchView('metrics'),
      },
      {
        id: 'nav.settings',
        label: 'Go to SETTINGS',
        hint: '4',
        group: 'NAV',
        run: () => onSwitchView('settings'),
      },
    ]

    if (selectedId) {
      cmds.push({
        id: 'task.abort',
        label: `Abort current task ${selectedId}`,
        group: 'TASK',
        run: async () => {
          try {
            await createDaemonClient().abortTask(selectedId)
          } catch {
            /* swallow -- the user will see the task detail update on next poll */
          }
        },
      })
      cmds.push({
        id: 'task.interrupt',
        label: `Soft-interrupt current task ${selectedId}`,
        group: 'TASK',
        run: async () => {
          try {
            await createDaemonClient().interruptTask(selectedId)
          } catch {
            /* swallow */
          }
        },
      })
    }

    // Recent tasks. Cap at 8 so the palette stays scannable.
    for (const t of list.slice(0, 8)) {
      cmds.push({
        id: `task.open.${t.id}`,
        label: `Open ${t.id}`,
        hint: t.description.slice(0, 60),
        group: 'TASK',
        run: () => {
          selectTask(t.id)
          onSwitchView('task')
        },
      })
    }

    if (!connected) {
      cmds.push({
        id: 'daemon.spawn',
        label: 'Spawn daemon',
        group: 'DAEMON',
        run: () => {
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          const bridge = (window as any).moirai
          if (bridge && typeof bridge.invoke === 'function') {
            bridge.invoke('daemon-spawn', {}).catch(() => {
              /* surfaced by App's offline panel error path */
            })
          }
        },
      })
    }

    return cmds
  }, [connected, list, onSwitchView, selectTask, selectedId])

  const filtered = useMemo(
    () =>
      commands.filter((c) =>
        tokenMatch(`${c.label} ${c.hint ?? ''} ${c.group}`, query),
      ),
    [commands, query],
  )

  // Reset query + cursor whenever the palette opens. Without this,
  // re-opening the palette would show last session's filter and a stale
  // selection that may not exist in the new filtered set.
  useEffect(() => {
    if (open) {
      setQuery('')
      setCursor(0)
      // Defer focus so the input exists in the DOM.
      const t = setTimeout(() => inputRef.current?.focus(), 0)
      return () => clearTimeout(t)
    }
  }, [open])

  // Clamp cursor when filtered list shrinks below the current index.
  useEffect(() => {
    if (cursor >= filtered.length) {
      setCursor(Math.max(0, filtered.length - 1))
    }
  }, [cursor, filtered.length])

  // Esc to close, regardless of which child has focus.
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

  // Scroll the active row into view as the cursor moves. jsdom (used by
  // vitest) does not implement scrollIntoView, so feature-detect rather
  // than always invoking it.
  useEffect(() => {
    if (!open) return
    const node = listRef.current?.querySelector<HTMLElement>(
      `[data-cmd-idx="${cursor}"]`,
    )
    if (node && typeof node.scrollIntoView === 'function') {
      node.scrollIntoView({ block: 'nearest' })
    }
  }, [cursor, open])

  if (!open) return null

  const handleKey = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setCursor((i) => Math.min(filtered.length - 1, i + 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setCursor((i) => Math.max(0, i - 1))
    } else if (e.key === 'Enter') {
      e.preventDefault()
      const cmd = filtered[cursor]
      if (cmd) {
        cmd.run()
        onClose()
      }
    } else if (e.key === 'Tab') {
      // Trap tab inside the palette so focus does not escape.
      e.preventDefault()
    }
  }

  return (
    <div
      className="cmd-palette-overlay"
      role="dialog"
      aria-modal="true"
      aria-label="Command palette"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="cmd-palette">
        <div className="cmd-palette-tag">COMMAND PALETTE</div>
        <input
          ref={inputRef}
          className="cmd-palette-input"
          type="text"
          placeholder="Type a command, view, or task id"
          value={query}
          onChange={(e) => {
            setQuery(e.target.value)
            setCursor(0)
          }}
          onKeyDown={handleKey}
          autoComplete="off"
          spellCheck={false}
        />
        <div className="cmd-palette-list" ref={listRef}>
          {filtered.length === 0 && (
            <div className="cmd-palette-empty">no matches</div>
          )}
          {filtered.map((cmd, i) => (
            <div
              key={cmd.id}
              data-cmd-idx={i}
              className={`cmd-palette-row${i === cursor ? ' active' : ''}`}
              onMouseEnter={() => setCursor(i)}
              onMouseDown={(e) => {
                // mousedown (not click) so blur on the input does not
                // steal the event before the palette closes.
                e.preventDefault()
                cmd.run()
                onClose()
              }}
              role="option"
              aria-selected={i === cursor}
            >
              <span className={`cmd-palette-group group-${cmd.group.toLowerCase()}`}>
                {cmd.group}
              </span>
              <span className="cmd-palette-label">{cmd.label}</span>
              {cmd.hint && <span className="cmd-palette-hint">{cmd.hint}</span>}
            </div>
          ))}
        </div>
        <div className="cmd-palette-foot">
          <span><span className="kbd">↑</span><span className="kbd">↓</span> nav</span>
          <span><span className="kbd">↵</span> run</span>
          <span><span className="kbd">esc</span> close</span>
        </div>
      </div>
    </div>
  )
}
