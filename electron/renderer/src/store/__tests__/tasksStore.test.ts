import { beforeEach, describe, expect, it } from 'vitest'
import type { Task, TaskDetail } from '../../lib/daemonClient'
import { useTasksStore } from '../tasksStore'

function makeTask(id: string): Task {
  return {
    id,
    status: 'running',
    phase: 'coding',
    iterations: 0,
    replans: 0,
    active_model: 'model',
    repo_root: '/r',
    branch: 'main',
    description: 'desc',
    created_at: '2026-04-23T00:00:00Z',
    updated_at: '2026-04-23T00:00:00Z',
  }
}

describe('tasksStore', () => {
  beforeEach(() => {
    useTasksStore.setState({ list: [], selectedId: null, detail: null })
  })

  it('setList stores the task array', () => {
    const tasks = [makeTask('T-1'), makeTask('T-2')]
    useTasksStore.getState().setList(tasks)
    expect(useTasksStore.getState().list).toEqual(tasks)
  })

  it('selectTask sets id and clears detail when id changes', () => {
    const detail: TaskDetail = {
      task: makeTask('T-1'),
      recent: [],
    }
    useTasksStore.setState({
      selectedId: 'T-1',
      detail,
    })

    useTasksStore.getState().selectTask('T-2')

    expect(useTasksStore.getState().selectedId).toBe('T-2')
    expect(useTasksStore.getState().detail).toBeNull()
  })

  it('selectTask is a no-op when id does not change', () => {
    const detail: TaskDetail = {
      task: makeTask('T-1'),
      recent: [],
    }
    useTasksStore.setState({
      selectedId: 'T-1',
      detail,
    })

    useTasksStore.getState().selectTask('T-1')

    expect(useTasksStore.getState().selectedId).toBe('T-1')
    // Detail preserved because no selection change.
    expect(useTasksStore.getState().detail).toBe(detail)
  })

  it('selectTask(null) clears detail', () => {
    useTasksStore.setState({
      selectedId: 'T-1',
      detail: { task: makeTask('T-1'), recent: [] },
    })
    useTasksStore.getState().selectTask(null)
    expect(useTasksStore.getState().selectedId).toBeNull()
    expect(useTasksStore.getState().detail).toBeNull()
  })

  it('setDetail sets the detail payload', () => {
    const detail: TaskDetail = {
      task: makeTask('T-5'),
      recent: [{ ts: 'now', kind: 'phase' }],
    }
    useTasksStore.getState().setDetail(detail)
    expect(useTasksStore.getState().detail).toEqual(detail)
  })

  it('setDetail(null) clears the detail', () => {
    useTasksStore.setState({
      detail: { task: makeTask('T-1'), recent: [] },
    })
    useTasksStore.getState().setDetail(null)
    expect(useTasksStore.getState().detail).toBeNull()
  })
})
