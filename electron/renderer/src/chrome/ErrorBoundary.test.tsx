import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ErrorBoundary from './ErrorBoundary'

function Exploder({ message }: { message: string }): never {
  throw new Error(message)
}

function Peaceful() {
  return <div>hello fabric</div>
}

describe('ErrorBoundary', () => {
  // React logs the render-time throw to console.error even when an error
  // boundary catches it. Silence that noise for the duration of this
  // file so the test output stays readable.
  let consoleErrSpy: ReturnType<typeof vi.spyOn>
  beforeEach(() => {
    consoleErrSpy = vi
      .spyOn(console, 'error')
      .mockImplementation(() => undefined)
  })
  afterEach(() => {
    consoleErrSpy.mockRestore()
  })

  it('renders children normally when no error is thrown', () => {
    render(
      <ErrorBoundary>
        <Peaceful />
      </ErrorBoundary>,
    )
    expect(screen.getByText('hello fabric')).toBeInTheDocument()
    expect(screen.queryByTestId('error-boundary-panel')).toBeNull()
  })

  it('renders diagnostic panel when a child throws', () => {
    render(
      <ErrorBoundary>
        <Exploder message="kernel panic in the fabric" />
      </ErrorBoundary>,
    )
    expect(screen.getByTestId('error-boundary-panel')).toBeInTheDocument()
    expect(
      screen.getByTestId('error-boundary-message'),
    ).toHaveTextContent('kernel panic in the fabric')
    // Component stack is surfaced too.
    expect(screen.getByTestId('error-boundary-stack')).toBeInTheDocument()
  })

  it('surfaces a Reload button that calls window.location.reload', () => {
    const reload = vi.fn()
    const originalLocation = window.location
    // @ts-expect-error -- deliberately swap location to stub reload
    delete window.location
    // @ts-expect-error -- minimal shim
    window.location = { ...originalLocation, reload }

    try {
      render(
        <ErrorBoundary>
          <Exploder message="boom" />
        </ErrorBoundary>,
      )
      const btn = screen.getByRole('button', { name: /reload/i })
      expect(btn).toBeInTheDocument()
      fireEvent.click(btn)
      expect(reload).toHaveBeenCalledTimes(1)
    } finally {
      // @ts-expect-error -- restore
      window.location = originalLocation
    }
  })
})
