import { render } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import Plan from './Plan'
import { useTasksStore } from '../store/tasksStore'
import type { Task } from '../lib/daemonClient'

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: 'T-PLAN',
    status: 'running',
    phase: 'planning',
    iterations: 0,
    replans: 0,
    active_model: 'planner',
    repo_root: '/r',
    branch: 'main',
    description: 'describe the task',
    created_at: '2026-04-23T00:00:00Z',
    updated_at: '2026-04-23T00:00:00Z',
    ...overrides,
  }
}

describe('Plan', () => {
  beforeEach(() => {
    useTasksStore.setState({ list: [], selectedId: null, detail: null })
  })

  it('renders task.plan inside a <pre>', () => {
    useTasksStore.setState({
      detail: {
        task: makeTask({ plan: 'Step 1\nStep 2' }),
        recent: [],
      },
    })
    const { container } = render(<Plan />)
    const pre = container.querySelector('pre.plan-raw') as HTMLElement
    expect(pre).toBeInTheDocument()
    expect(pre.textContent).toBe('Step 1\nStep 2')
  })

  it('empty state shows NO PLAN YET and the task description', () => {
    useTasksStore.setState({
      detail: {
        task: makeTask({ plan: undefined, description: 'build a widget' }),
        recent: [],
      },
    })
    const { container } = render(<Plan />)
    expect(container.textContent).toMatch(/NO PLAN YET/)
    expect(container.textContent).toMatch(/build a widget/)
  })

  it('revLabel maps replans 0 -> "revision 1"', () => {
    useTasksStore.setState({
      detail: { task: makeTask({ replans: 0 }), recent: [] },
    })
    const { container } = render(<Plan />)
    expect(container.textContent).toMatch(/revision 1/i)
  })

  it('revLabel maps replans 2 -> "revision 3"', () => {
    useTasksStore.setState({
      detail: { task: makeTask({ replans: 2 }), recent: [] },
    })
    const { container } = render(<Plan />)
    expect(container.textContent).toMatch(/revision 3/i)
  })
})
