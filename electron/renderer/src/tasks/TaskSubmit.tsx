import { useState } from 'react'
import { createDaemonClient } from '../lib/daemonClient'
import { useClickableDiv } from '../lib/a11y'

// Client-side description length caps. Match the backend's MaxBytesReader
// limit so the UI fails fast instead of posting and eating a 413. The soft
// cap is purely advisory ("consider summarizing").
export const DESCRIPTION_HARD_LIMIT = 256 * 1024
export const DESCRIPTION_SOFT_LIMIT = 64 * 1024

function formatBytes(n: number): string {
  if (n >= 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`
  if (n >= 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${n} B`
}

// Left side of the bottom row: hex-shaped submit panel with textarea,
// repo picker, dispatch button, quick prompts grid. Wired to
// /submit; on success clears the textarea.
export default function TaskSubmit() {
  const [description, setDescription] = useState('')
  const [repoRoot, setRepoRoot] = useState('~/src/agent-router')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Byte length approximation -- TextEncoder gives us the wire size.
  // Fall back to character count if TextEncoder isn't available (e.g. in
  // unusual jsdom environments).
  const descriptionBytes = (() => {
    try {
      return new TextEncoder().encode(description).length
    } catch {
      return description.length
    }
  })()
  const tooLong = descriptionBytes > DESCRIPTION_HARD_LIMIT
  const largeWarn =
    !tooLong && descriptionBytes > DESCRIPTION_SOFT_LIMIT
  const disabledReason = tooLong
    ? `description too long (${formatBytes(descriptionBytes)} / ${formatBytes(
        DESCRIPTION_HARD_LIMIT,
      )} max)`
    : null

  const dispatch = async () => {
    if (!description.trim() || busy || tooLong) return
    setBusy(true)
    setError(null)
    try {
      const client = createDaemonClient()
      await client.submitTask(description.trim(), repoRoot)
      setDescription('')
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  // Quick-prompt handlers. The source design marks them as static glyphs
  // -- they don't do anything yet. We still wire them as proper buttons
  // so keyboard users can focus+press them and screen readers announce
  // intent. The onClick is a no-op placeholder until the backend wires
  // quick-prompt behavior.
  const replanClick = useClickableDiv(() => {})
  const testsClick = useClickableDiv(() => {})
  const reviewClick = useClickableDiv(() => {})
  const abortClick = useClickableDiv(() => {})

  return (
    <div className="submit-wrap hex">
      <svg className="hex-border" viewBox="0 0 420 298" preserveAspectRatio="none">
        <path d="M20,1 L400,1 L419,149 L400,297 L20,297 L1,149 Z" />
      </svg>
      <span className="vertex" style={{ left: '20px', top: 0 }} />
      <span className="vertex" style={{ left: '400px', top: 0 }} />
      <span className="vertex" style={{ left: '100%', top: '50%' }} />
      <span className="vertex" style={{ left: '400px', top: '100%' }} />
      <span className="vertex" style={{ left: '20px', top: '100%' }} />
      <span className="vertex" style={{ left: 0, top: '50%' }} />

      <div className="submit-title">
        SUBMIT <b>⋈</b> TASK
      </div>

      <label htmlFor="task-description" className="field-label">
        DESCRIPTION
      </label>
      <textarea
        id="task-description"
        className="textarea"
        placeholder="describe the change…"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
      />

      {largeWarn && (
        <div
          className="submit-warn soft"
          data-testid="submit-warn-soft"
          style={{
            color: 'var(--amber)',
            fontSize: '10px',
            letterSpacing: '0.1em',
            padding: '4px 12px',
          }}
        >
          LARGE -- CONSIDER SUMMARIZING (
          {formatBytes(descriptionBytes)} / {formatBytes(DESCRIPTION_HARD_LIMIT)}{' '}
          MAX)
        </div>
      )}
      {tooLong && (
        <div
          className="submit-warn hard"
          data-testid="submit-warn-hard"
          style={{
            color: 'var(--magenta)',
            fontSize: '10px',
            letterSpacing: '0.1em',
            padding: '4px 12px',
          }}
        >
          {disabledReason?.toUpperCase()}
        </div>
      )}

      <label htmlFor="task-repo-root" className="field-label">
        REPO ROOT
      </label>
      <div className="repo-row">
        <input
          id="task-repo-root"
          className="input"
          value={repoRoot}
          onChange={(e) => setRepoRoot(e.target.value)}
          style={{
            background: 'transparent',
            border: 'none',
            color: 'inherit',
            font: 'inherit',
            outline: 'none',
            padding: 0,
            width: '100%',
          }}
        />
        <div className="pick-btn" aria-hidden="true">···</div>
      </div>

      <button
        className="submit-btn"
        onClick={dispatch}
        disabled={busy || tooLong}
        title={disabledReason ?? undefined}
      >
        <span>{busy ? '▸ DISPATCHING…' : '▸ DISPATCH TO FABRIC'}</span>
      </button>

      {error && (
        <div
          style={{
            color: 'var(--magenta)',
            fontSize: '10px',
            letterSpacing: '0.1em',
            padding: '4px 12px',
          }}
        >
          {error}
        </div>
      )}

      <div className="field-label" style={{ marginBottom: '8px' }}>
        QUICK PROMPTS
      </div>
      <div className="quick-grid">
        <div {...replanClick} className="quick" aria-label="Replan">REPLAN</div>
        <div {...testsClick} className="quick" aria-label="Tests">TESTS</div>
        <div {...reviewClick} className="quick" aria-label="Review">REVIEW</div>
        <div {...abortClick} className="quick" aria-label="Abort">ABORT</div>
      </div>
    </div>
  )
}
