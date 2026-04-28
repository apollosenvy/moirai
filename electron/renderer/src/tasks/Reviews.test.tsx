import { render } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import Reviews from './Reviews'
import { useTasksStore } from '../store/tasksStore'
import type { Task } from '../lib/daemonClient'

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: 'T-REV',
    status: 'running',
    phase: 'plan_review',
    iterations: 0,
    replans: 0,
    active_model: 'reviewer',
    repo_root: '/r',
    branch: 'main',
    description: 'desc',
    created_at: '2026-04-23T00:00:00Z',
    updated_at: '2026-04-23T00:00:00Z',
    ...overrides,
  }
}

function firstCard(container: HTMLElement): HTMLElement {
  const card = container.querySelector('.review-card')
  if (!card) throw new Error('no .review-card found')
  return card as HTMLElement
}

describe('Reviews', () => {
  beforeEach(() => {
    useTasksStore.setState({ list: [], selectedId: null, detail: null })
  })

  it('plan review with verdict "approve" renders approved variant', () => {
    useTasksStore.setState({
      detail: {
        task: makeTask({
          reviews: ['plan: {"verdict":"approve","reason":"looks fine"}'],
        }),
        recent: [],
      },
    })
    const { container } = render(<Reviews />)
    const card = firstCard(container)
    expect(card.className).toContain('approved')
  })

  it('code review with "something failed" in body renders fix variant', () => {
    useTasksStore.setState({
      detail: {
        task: makeTask({
          reviews: ['code: exec failed on step 2'],
        }),
        recent: [],
      },
    })
    const { container } = render(<Reviews />)
    const card = firstCard(container)
    expect(card.className).toContain('fix')
  })

  it('review with no prefix falls back to fix variant', () => {
    useTasksStore.setState({
      detail: {
        task: makeTask({ reviews: ['mystery body no prefix'] }),
        recent: [],
      },
    })
    const { container } = render(<Reviews />)
    const card = firstCard(container)
    // variantFromVerdict(null) -> 'fix' (the defined fallback).
    expect(card.className).toContain('fix')
  })

  it('JSON verdict parsing beats the regex false-match on nested keys', () => {
    // If the regex ran unconditionally it'd match the inner "verdict" too.
    // The JSON parse takes precedence and picks the OUTER verdict, which
    // here is "approve".
    useTasksStore.setState({
      detail: {
        task: makeTask({
          reviews: [
            'plan: {"verdict":"approve","reason":"previous verdict was revise but I disagree"}',
          ],
        }),
        recent: [],
      },
    })
    const { container } = render(<Reviews />)
    const card = firstCard(container)
    expect(card.className).toContain('approved')
  })

  it('non-JSON body still works via regex fallback', () => {
    useTasksStore.setState({
      detail: {
        task: makeTask({
          reviews: ['plan: prose verdict is "revise" because ...'],
        }),
        recent: [],
      },
    })
    const { container } = render(<Reviews />)
    const card = firstCard(container)
    // "revise" in the prose body flags the revise variant via regex.
    expect(card.className).toContain('revise')
  })
})
