import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { useClickableDiv } from '../a11y'

function Clickable({ onClick }: { onClick: () => void }) {
  const props = useClickableDiv(onClick)
  return (
    <div {...props} data-testid="clickable">
      click me
    </div>
  )
}

describe('useClickableDiv', () => {
  it('advertises role="button" and tabIndex=0', () => {
    render(<Clickable onClick={() => {}} />)
    const el = screen.getByTestId('clickable')
    expect(el.getAttribute('role')).toBe('button')
    expect(el.getAttribute('tabindex')).toBe('0')
  })

  it('fires onClick on mouse click', () => {
    const handler = vi.fn()
    render(<Clickable onClick={handler} />)
    fireEvent.click(screen.getByTestId('clickable'))
    expect(handler).toHaveBeenCalledTimes(1)
  })

  it('fires onClick on Enter key', () => {
    const handler = vi.fn()
    render(<Clickable onClick={handler} />)
    fireEvent.keyDown(screen.getByTestId('clickable'), { key: 'Enter' })
    expect(handler).toHaveBeenCalledTimes(1)
  })

  it('fires onClick on Space key', () => {
    const handler = vi.fn()
    render(<Clickable onClick={handler} />)
    fireEvent.keyDown(screen.getByTestId('clickable'), { key: ' ' })
    expect(handler).toHaveBeenCalledTimes(1)
  })

  it('does NOT fire onClick on other keys', () => {
    const handler = vi.fn()
    render(<Clickable onClick={handler} />)
    fireEvent.keyDown(screen.getByTestId('clickable'), { key: 'a' })
    fireEvent.keyDown(screen.getByTestId('clickable'), { key: 'Tab' })
    fireEvent.keyDown(screen.getByTestId('clickable'), { key: 'Escape' })
    expect(handler).not.toHaveBeenCalled()
  })

  it('is findable as role=button via Testing Library', () => {
    render(<Clickable onClick={() => {}} />)
    expect(screen.getByRole('button', { name: /click me/i })).toBeInTheDocument()
  })
})
