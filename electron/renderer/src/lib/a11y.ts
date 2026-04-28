import type { KeyboardEvent } from 'react'

// Accessibility helpers for the Hex Fabric UI.
//
// useClickableDiv turns a non-button <div onClick> into a proper
// keyboard-operable role="button": Enter and Space both fire the
// handler (matching native button behavior), and the element advertises
// itself to assistive tech.
//
// Use: <div {...useClickableDiv(handler)}>...</div>
//
// Callers can still pass their own onKeyDown by composing manually --
// this helper is intentionally small.

export interface ClickableDivProps {
  onClick: () => void
  onKeyDown: (e: KeyboardEvent<HTMLElement>) => void
  role: 'button'
  tabIndex: number
}

export function useClickableDiv(onClick: () => void): ClickableDivProps {
  return {
    onClick,
    onKeyDown: (e) => {
      if (e.key === 'Enter' || e.key === ' ' || e.key === 'Spacebar') {
        e.preventDefault()
        onClick()
      }
    },
    role: 'button',
    tabIndex: 0,
  }
}
