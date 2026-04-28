import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createDaemonClient } from '../daemonClient'

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const w = window as any

describe('daemonClient.injectGuidance', () => {
  let invoke: ReturnType<typeof vi.fn>

  beforeEach(() => {
    invoke = vi.fn()
    w.moirai = { invoke, on: () => {} }
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('invokes POST /tasks/<id>/inject with the message body', async () => {
    invoke.mockResolvedValueOnce({
      status: 200,
      body: JSON.stringify({ injected: 'T-9' }),
    })
    const client = createDaemonClient()
    const res = await client.injectGuidance('T-9', 'steer left')
    expect(res.injected).toBe('T-9')
    expect(invoke).toHaveBeenCalledWith('daemon-call', {
      method: 'POST',
      path: '/tasks/T-9/inject',
      body: { message: 'steer left' },
    })
  })

  it('surfaces a 400 error from the daemon as a thrown Error with server message', async () => {
    invoke.mockResolvedValueOnce({
      status: 400,
      body: JSON.stringify({ error: 'task is not running' }),
    })
    const client = createDaemonClient()
    // The current client doesn't extract the .error field -- it surfaces
    // the raw status + body, which still contains "task is not running".
    await expect(client.injectGuidance('T-9', 'hi')).rejects.toThrow(
      /task is not running/,
    )
  })
})
