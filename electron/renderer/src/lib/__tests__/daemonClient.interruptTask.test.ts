import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createDaemonClient } from '../daemonClient'

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const w = window as any

describe('daemonClient.interruptTask', () => {
  let invoke: ReturnType<typeof vi.fn>

  beforeEach(() => {
    invoke = vi.fn()
    w.moirai = { invoke, on: () => {} }
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('invokes POST /tasks/<id>/interrupt and parses the response', async () => {
    invoke.mockResolvedValueOnce({
      status: 200,
      body: JSON.stringify({ interrupted: 'T-XYZ' }),
    })
    const client = createDaemonClient()
    const res = await client.interruptTask('T-XYZ')
    expect(res.interrupted).toBe('T-XYZ')
    expect(invoke).toHaveBeenCalledWith('daemon-call', {
      method: 'POST',
      path: '/tasks/T-XYZ/interrupt',
      body: undefined,
    })
  })

  it('throws when the daemon rejects the request', async () => {
    // The daemon returns 404 when the task id is unknown (the
    // post-pass-1 contract). Older mocks asserted 400; updated to
    // mirror the real backend behaviour.
    invoke.mockResolvedValueOnce({
      status: 404,
      body: JSON.stringify({ error: 'task not found' }),
    })
    const client = createDaemonClient()
    await expect(client.interruptTask('T-NOPE')).rejects.toThrow(/404/)
  })
})
