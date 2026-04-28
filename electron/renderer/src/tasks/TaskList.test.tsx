import { describe, expect, it } from 'vitest'
import { statusBucket } from './TaskList'

describe('statusBucket', () => {
  it('classifies all backend taskstore.Status values', () => {
    // Backend canonical values from internal/taskstore.
    expect(statusBucket('pending')).toBe('wait')
    expect(statusBucket('running')).toBe('run')
    expect(statusBucket('awaiting_user')).toBe('wait')
    expect(statusBucket('succeeded')).toBe('done')
    expect(statusBucket('failed')).toBe('err')
    expect(statusBucket('aborted')).toBe('err')
    expect(statusBucket('interrupted')).toBe('err')
  })

  it('keeps historical synonyms working', () => {
    expect(statusBucket('executing')).toBe('run')
    expect(statusBucket('waiting')).toBe('wait')
    expect(statusBucket('queued')).toBe('wait')
    expect(statusBucket('done')).toBe('done')
    expect(statusBucket('completed')).toBe('done')
    expect(statusBucket('error')).toBe('err')
  })

  it('is case-insensitive', () => {
    expect(statusBucket('RUNNING')).toBe('run')
    expect(statusBucket('Succeeded')).toBe('done')
  })

  it('phase=done implies done when status is empty', () => {
    expect(statusBucket('', 'done')).toBe('done')
  })

  it('unknown values fall back to unknown', () => {
    expect(statusBucket('mystery')).toBe('unknown')
    expect(statusBucket('')).toBe('unknown')
  })
})
