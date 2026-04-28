import { fireEvent, render } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import ModelDropdown from '../slots/ModelDropdown'
import KvQuantDropdown, { type KvOption } from '../slots/KvQuantDropdown'

describe('ModelDropdown', () => {
  // ModelDropdown now portals its panel to document.body (same pattern as
  // KvQuantDropdown) so parent clip-paths can't hide it. Queries for
  // '.dropdown' must go against `document`, not the testing-library
  // container.
  it('does not render the panel until the combo is clicked', () => {
    render(
      <ModelDropdown
        value="initial"
        options={[{ path: '/a', label: 'Alpha' }]}
        onSelect={() => {}}
      />,
    )
    expect(document.querySelector('.dropdown')).toBeNull()
  })

  it('opens on click and fires onSelect when an option is clicked', () => {
    const onSelect = vi.fn()
    const { container } = render(
      <ModelDropdown
        value="initial"
        options={[
          { path: '/a', label: 'Alpha' },
          { path: '/b', label: 'Beta' },
        ]}
        onSelect={onSelect}
      />,
    )
    const combo = container.querySelector('.combo') as HTMLDivElement
    fireEvent.click(combo)
    expect(document.querySelector('.dropdown')).not.toBeNull()
    const items = document.querySelectorAll('.dropdown .dd-item')
    expect(items.length).toBe(2)
    fireEvent.click(items[1] as HTMLElement)
    expect(onSelect).toHaveBeenCalledWith('/b')
    // Panel closes on selection.
    expect(document.querySelector('.dropdown')).toBeNull()
  })

  it('closes when Escape is pressed', () => {
    const { container } = render(
      <ModelDropdown
        value="initial"
        options={[{ path: '/a', label: 'Alpha' }]}
        onSelect={() => {}}
      />,
    )
    const combo = container.querySelector('.combo') as HTMLDivElement
    fireEvent.click(combo)
    expect(document.querySelector('.dropdown')).not.toBeNull()
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(document.querySelector('.dropdown')).toBeNull()
  })
})

describe('KvQuantDropdown', () => {
  const options: KvOption[] = [
    { label: 'F16', size: '--', kv: 'f16' },
    { label: 'Turbo3', size: 'HIP parity', ratio: '4.6x', kv: 'turbo3', turbo: true },
  ]

  // KvQuantDropdown portals its panel to document.body to escape parent
  // clip-paths. Tests must query document (not the testing-library
  // container) to find portal content.
  it('opens on click and fires onSelect', () => {
    const onSelect = vi.fn()
    const { container } = render(
      <KvQuantDropdown
        value="F16"
        options={options}
        selectedKv="f16"
        turboSupported
        onSelect={onSelect}
      />,
    )
    const combo = container.querySelector('.combo') as HTMLDivElement
    fireEvent.click(combo)
    const items = document.querySelectorAll('.dropdown .dd-item')
    expect(items.length).toBe(2)
    fireEvent.click(items[1] as HTMLElement)
    expect(onSelect).toHaveBeenCalledWith('turbo3')
  })

  it('disables turbo options when turboSupported is false', () => {
    const onSelect = vi.fn()
    const { container } = render(
      <KvQuantDropdown
        value="F16"
        options={options}
        selectedKv="f16"
        turboSupported={false}
        onSelect={onSelect}
      />,
    )
    const combo = container.querySelector('.combo') as HTMLDivElement
    fireEvent.click(combo)
    const items = document.querySelectorAll('.dropdown .dd-item')
    const turboItem = items[1] as HTMLElement
    expect(turboItem.className).toContain('disabled')
    fireEvent.click(turboItem)
    expect(onSelect).not.toHaveBeenCalled()
  })
})
