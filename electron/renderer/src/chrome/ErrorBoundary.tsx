import React from 'react'

// Root ErrorBoundary -- catches render-time throws anywhere in the
// component tree and renders a minimal diagnostic panel instead of
// leaving <div id="root" /> empty. React 19 still requires class
// components for error boundaries (there is no hook equivalent).
//
// Style uses the same palette (ice / amber / plasma / void) as the
// rest of the fabric so the panel looks intentional rather than a raw
// browser error screen.

interface ErrorBoundaryProps {
  children: React.ReactNode
}

interface ErrorBoundaryState {
  error: Error | null
  info: React.ErrorInfo | null
}

export default class ErrorBoundary extends React.Component<
  ErrorBoundaryProps,
  ErrorBoundaryState
> {
  constructor(props: ErrorBoundaryProps) {
    super(props)
    this.state = { error: null, info: null }
  }

  static getDerivedStateFromError(error: Error): Partial<ErrorBoundaryState> {
    return { error }
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    // Log so devtools / remote loggers / Phos host capture it. We
    // intentionally keep this minimal -- the diagnostic panel surfaces
    // the user-facing detail below.
    // eslint-disable-next-line no-console
    console.error('[ErrorBoundary]', error, info)
    this.setState({ info })
  }

  private handleReload = () => {
    if (typeof window !== 'undefined' && window.location) {
      window.location.reload()
    }
  }

  render() {
    const { error, info } = this.state
    if (!error) return this.props.children

    return (
      <div
        role="alert"
        data-testid="error-boundary-panel"
        style={{
          position: 'fixed',
          inset: 0,
          background: 'var(--void, #04060b)',
          color: 'var(--text, #e6ecf5)',
          padding: '32px',
          overflow: 'auto',
          zIndex: 9999,
          fontFamily: 'var(--ff-mono-display, ui-monospace, monospace)',
          fontSize: '12px',
          lineHeight: 1.5,
        }}
      >
        <div
          style={{
            maxWidth: '960px',
            margin: '0 auto',
            border: '1px solid var(--magenta, #ff2bd6)',
            background: 'var(--panel, #0a1424)',
            padding: '24px',
          }}
        >
          <div
            style={{
              color: 'var(--magenta, #ff2bd6)',
              letterSpacing: '0.32em',
              fontSize: '11px',
              marginBottom: '16px',
              textTransform: 'uppercase',
            }}
          >
            FABRIC · RENDER FAULT
          </div>
          <div
            style={{
              color: 'var(--amber, #ffb347)',
              fontSize: '14px',
              marginBottom: '8px',
              fontFamily: 'var(--ff-mono-num, ui-monospace, monospace)',
            }}
          >
            {error.name || 'Error'}
          </div>
          <div
            data-testid="error-boundary-message"
            style={{
              color: 'var(--text, #e6ecf5)',
              marginBottom: '20px',
              whiteSpace: 'pre-wrap',
              wordBreak: 'break-word',
            }}
          >
            {error.message || '(no message)'}
          </div>

          {info?.componentStack && (
            <>
              <div
                style={{
                  color: 'var(--ice-dim, #007aa0)',
                  letterSpacing: '0.24em',
                  fontSize: '10px',
                  marginBottom: '6px',
                  textTransform: 'uppercase',
                }}
              >
                Component Stack
              </div>
              <pre
                data-testid="error-boundary-stack"
                style={{
                  color: 'var(--text-dim, #96a8c2)',
                  background: 'var(--void, #04060b)',
                  border: '1px solid var(--border, #264060)',
                  padding: '12px',
                  fontSize: '11px',
                  overflow: 'auto',
                  maxHeight: '320px',
                  margin: 0,
                  whiteSpace: 'pre-wrap',
                  wordBreak: 'break-word',
                }}
              >
                {info.componentStack}
              </pre>
            </>
          )}

          <div style={{ marginTop: '24px', display: 'flex', gap: '12px' }}>
            <button
              type="button"
              onClick={this.handleReload}
              style={{
                background: 'transparent',
                color: 'var(--ice, #00c8ff)',
                border: '1px solid var(--ice, #00c8ff)',
                padding: '10px 18px',
                letterSpacing: '0.32em',
                fontSize: '11px',
                fontFamily: 'inherit',
                cursor: 'pointer',
                textTransform: 'uppercase',
              }}
            >
              Reload
            </button>
          </div>
        </div>
      </div>
    )
  }
}
