import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createDaemonClient } from '../daemonClient'

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const w = window as any

describe('createDaemonClient', () => {
  let invoke: ReturnType<typeof vi.fn>

  beforeEach(() => {
    invoke = vi.fn()
    w.moirai = { invoke, on: () => {} }
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('getStatus parses a 200 JSON body', async () => {
    invoke.mockResolvedValueOnce({
      status: 200,
      body: JSON.stringify({
        service: 'agent-router',
        port: 5984,
        active_slot: 'reviewer',
        active_port: 6001,
        task_count: 4,
        running: 1,
        last_verdict: 'approved',
        max_ro_turns: 40,
        vram_used_mb: 14000,
        vram_total_mb: 24576,
        turboquant_supported: true,
        daemon_version: 'v0.4.2',
        started_at: '2026-04-23T00:00:00Z',
        uptime: '00:14:22',
      }),
    })

    const client = createDaemonClient()
    const status = await client.getStatus()

    expect(status.service).toBe('agent-router')
    expect(status.running).toBe(1)
    expect(status.last_verdict).toBe('approved')
    expect(status.max_ro_turns).toBe(40)
    expect(invoke).toHaveBeenCalledWith('daemon-call', {
      method: 'GET',
      path: '/status',
      body: undefined,
    })
  })

  it('getReady returns true on 200, false on 503', async () => {
    const client = createDaemonClient()

    invoke.mockResolvedValueOnce({ status: 200, body: '' })
    expect(await client.getReady()).toBe(true)

    invoke.mockResolvedValueOnce({ status: 503, body: '' })
    expect(await client.getReady()).toBe(false)
  })

  it('throws when status is 0 (daemon unreachable)', async () => {
    invoke.mockResolvedValueOnce({
      status: 0,
      body: '',
      error: 'ECONNREFUSED',
    })
    const client = createDaemonClient()
    await expect(client.getStatus()).rejects.toThrow(/unreachable/i)
  })

  it('throws on 4xx responses', async () => {
    invoke.mockResolvedValueOnce({
      status: 404,
      body: '{"error":"not found"}',
    })
    const client = createDaemonClient()
    await expect(client.getTask('T-0000')).rejects.toThrow(/404/)
  })

  it('throws on 5xx responses', async () => {
    invoke.mockResolvedValueOnce({
      status: 500,
      body: 'internal server error',
    })
    const client = createDaemonClient()
    await expect(client.listTasks()).rejects.toThrow(/500/)
  })

  it('throws when JSON is malformed', async () => {
    invoke.mockResolvedValueOnce({
      status: 200,
      body: '{not-json',
    })
    const client = createDaemonClient()
    await expect(client.getStatus()).rejects.toThrow(/invalid JSON/i)
  })

  it('patchSlot serializes the body and parses the response', async () => {
    invoke.mockResolvedValueOnce({
      status: 200,
      body: JSON.stringify({
        applied: false,
        pending: true,
        reason: 'slot is generating, queued',
      }),
    })

    const client = createDaemonClient()
    const res = await client.patchSlot('coder', {
      model_path: '/models/qwen3.gguf',
      ctx_size: 131072,
      kv_cache: 'turbo3',
    })

    expect(res.applied).toBe(false)
    expect(res.pending).toBe(true)
    expect(invoke).toHaveBeenCalledWith('daemon-call', {
      method: 'PATCH',
      path: '/slots/coder',
      body: {
        model_path: '/models/qwen3.gguf',
        ctx_size: 131072,
        kv_cache: 'turbo3',
      },
    })
  })

  it('submitTask posts description + repo_root', async () => {
    invoke.mockResolvedValueOnce({
      status: 200,
      body: JSON.stringify({
        id: 'T-2050',
        status: 'queued',
        phase: 'init',
        iterations: 0,
        replans: 0,
        active_model: '',
        repo_root: '/home/aegis/Projects/foo',
        branch: 'main',
        description: 'hello',
        created_at: '2026-04-23T00:00:00Z',
        updated_at: '2026-04-23T00:00:00Z',
      }),
    })

    const client = createDaemonClient()
    const task = await client.submitTask(
      'hello',
      '/home/aegis/Projects/foo',
    )

    expect(task.id).toBe('T-2050')
    expect(invoke).toHaveBeenCalledWith('daemon-call', {
      method: 'POST',
      path: '/submit',
      body: {
        description: 'hello',
        repo_root: '/home/aegis/Projects/foo',
      },
    })
  })

  it('getModels and getSlots parse arrays', async () => {
    invoke.mockResolvedValueOnce({
      status: 200,
      body: JSON.stringify([
        {
          path: '/a.gguf',
          name: 'a',
          size_bytes: 1,
          turboquant_safe: true,
        },
      ]),
    })
    const client = createDaemonClient()
    const models = await client.getModels()
    expect(models).toHaveLength(1)
    expect(models[0].name).toBe('a')
  })

  it('abortTask posts and returns aborted id', async () => {
    invoke.mockResolvedValueOnce({
      status: 200,
      body: JSON.stringify({ aborted: 'T-2044' }),
    })
    const client = createDaemonClient()
    const res = await client.abortTask('T-2044')
    expect(res.aborted).toBe('T-2044')
    expect(invoke).toHaveBeenCalledWith('daemon-call', {
      method: 'POST',
      path: '/tasks/T-2044/abort',
      body: undefined,
    })
  })

  it('throws a clear error when the bridge is absent', async () => {
    delete w.moirai
    const client = createDaemonClient()
    await expect(client.getStatus()).rejects.toThrow(/bridge unavailable/i)
  })
})
