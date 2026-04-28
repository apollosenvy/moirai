import { fireEvent, render } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import KvQuantDropdown, { type KvOption } from './KvQuantDropdown'

const options: KvOption[] = [
  { label: 'F16', size: '--', kv: 'f16' },
  { label: 'Q8', size: '-33%', kv: 'q8' },
  { label: 'Q5', size: '-50%', kv: 'q5' },
  { label: 'Turbo3', size: 'HIP parity', ratio: '4.6x', kv: 'turbo3', turbo: true },
]

describe('KvQuantDropdown placement', () => {
  const origInnerHeight = window.innerHeight

  beforeEach(() => {
    // Force a short viewport so the combo's bottom is near the floor.
    Object.defineProperty(window, 'innerHeight', {
      configurable: true,
      writable: true,
      value: 200,
    })
  })

  afterEach(() => {
    Object.defineProperty(window, 'innerHeight', {
      configurable: true,
      writable: true,
      value: origInnerHeight,
    })
    vi.restoreAllMocks()
  })

  it('places the panel above the combo when there is no room below', () => {
    // Mock getBoundingClientRect so the combo sits at y=180 in a 200px
    // viewport -- spaceBelow is only ~12px, spaceAbove is ~172px, so
    // placeAbove must be true.
    const fakeRect = {
      left: 10,
      top: 180,
      right: 210,
      bottom: 200,
      width: 200,
      height: 20,
      x: 10,
      y: 180,
      toJSON: () => {},
    } as DOMRect
    const origGBR = Element.prototype.getBoundingClientRect
    Element.prototype.getBoundingClientRect = function () {
      if (this instanceof HTMLElement && this.classList.contains('combo')) {
        return fakeRect
      }
      return origGBR.call(this)
    }

    try {
      const { container } = render(
        <KvQuantDropdown
          value="F16"
          options={options}
          selectedKv="f16"
          turboSupported
          onSelect={() => {}}
        />,
      )
      const combo = container.querySelector('.combo') as HTMLDivElement
      fireEvent.click(combo)
      const panel = document.querySelector('.dropdown') as HTMLElement
      expect(panel).not.toBeNull()
      const style = panel.style
      // placeAbove uses `bottom` not `top`.
      expect(style.bottom).not.toBe('')
      expect(style.top).toBe('')
    } finally {
      Element.prototype.getBoundingClientRect = origGBR
    }
  })
})
