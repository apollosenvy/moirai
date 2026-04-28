import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import TaskSubmit, {
  DESCRIPTION_HARD_LIMIT,
  DESCRIPTION_SOFT_LIMIT,
} from './TaskSubmit'

function getSubmit(): HTMLButtonElement {
  return screen.getByRole('button', { name: /DISPATCH/i }) as HTMLButtonElement
}

function getTextarea(): HTMLTextAreaElement {
  return screen.getByPlaceholderText(
    /describe the change/i,
  ) as HTMLTextAreaElement
}

describe('TaskSubmit label/input association', () => {
  it('associates DESCRIPTION label with the textarea', () => {
    render(<TaskSubmit />)
    // getByLabelText confirms <label htmlFor> -> <textarea id> wiring.
    const ta = screen.getByLabelText(/description/i)
    expect(ta.tagName).toBe('TEXTAREA')
  })

  it('associates REPO ROOT label with the repo input', () => {
    render(<TaskSubmit />)
    const input = screen.getByLabelText(/repo root/i)
    expect(input.tagName).toBe('INPUT')
  })

  it('quick prompts are keyboard-operable role=button elements', () => {
    render(<TaskSubmit />)
    expect(screen.getByRole('button', { name: /replan/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /tests/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /review/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /abort/i })).toBeInTheDocument()
  })
})

describe('TaskSubmit description length cap', () => {
  it('enables the button for short input', () => {
    render(<TaskSubmit />)
    fireEvent.change(getTextarea(), { target: { value: 'fix the typo' } })
    expect(getSubmit().disabled).toBe(false)
    expect(screen.queryByTestId('submit-warn-soft')).toBeNull()
    expect(screen.queryByTestId('submit-warn-hard')).toBeNull()
  })

  it('shows a soft warning above 64 KiB without disabling', () => {
    render(<TaskSubmit />)
    const payload = 'a'.repeat(DESCRIPTION_SOFT_LIMIT + 128)
    fireEvent.change(getTextarea(), { target: { value: payload } })
    expect(getSubmit().disabled).toBe(false)
    expect(screen.getByTestId('submit-warn-soft')).toBeInTheDocument()
    expect(screen.queryByTestId('submit-warn-hard')).toBeNull()
  })

  it('disables the button above 256 KiB with a hard warning', () => {
    render(<TaskSubmit />)
    const payload = 'a'.repeat(DESCRIPTION_HARD_LIMIT + 1)
    fireEvent.change(getTextarea(), { target: { value: payload } })
    const btn = getSubmit()
    expect(btn.disabled).toBe(true)
    expect(btn.getAttribute('title')).toMatch(/description too long/i)
    expect(screen.getByTestId('submit-warn-hard')).toBeInTheDocument()
  })
})
