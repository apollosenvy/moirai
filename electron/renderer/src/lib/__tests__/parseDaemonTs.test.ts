import { describe, expect, it } from 'vitest'
import { parseDaemonTs, parseDaemonTsMs } from '../parseDaemonTs'

describe('parseDaemonTs', () => {
  it('parses RFC3339 with Z suffix as UTC', () => {
    const d = parseDaemonTs('2026-04-24T10:15:32Z')
    expect(d.toISOString()).toBe('2026-04-24T10:15:32.000Z')
  })

  it('parses RFC3339 with numeric offset', () => {
    const d = parseDaemonTs('2026-04-24T03:15:32-07:00')
    expect(d.toISOString()).toBe('2026-04-24T10:15:32.000Z')
  })

  it('parses RFC3339 with positive offset', () => {
    const d = parseDaemonTs('2026-04-24T13:15:32+03:00')
    expect(d.toISOString()).toBe('2026-04-24T10:15:32.000Z')
  })

  it('treats SQLite-style "YYYY-MM-DD HH:MM:SS" as UTC', () => {
    // SQLite datetime('now') emits a space-separated timestamp without
    // a TZ designator. JS new Date() would parse that as LOCAL time
    // (off by the user's UTC offset). parseDaemonTs anchors it as UTC
    // by inserting T and appending Z.
    const d = parseDaemonTs('2026-04-24 10:15:32')
    expect(d.toISOString()).toBe('2026-04-24T10:15:32.000Z')
  })

  it('handles SQLite-style with fractional seconds', () => {
    const d = parseDaemonTs('2026-04-24 10:15:32.500')
    expect(d.toISOString()).toBe('2026-04-24T10:15:32.500Z')
  })

  it('returns NaN-shaped Date for empty string', () => {
    const d = parseDaemonTs('')
    expect(Number.isNaN(d.getTime())).toBe(true)
  })

  it('returns NaN-shaped Date for null input', () => {
    const d = parseDaemonTs(null)
    expect(Number.isNaN(d.getTime())).toBe(true)
  })

  it('returns NaN-shaped Date for undefined input', () => {
    const d = parseDaemonTs(undefined)
    expect(Number.isNaN(d.getTime())).toBe(true)
  })

  it('returns NaN-shaped Date for malformed string', () => {
    const d = parseDaemonTs('not-a-timestamp')
    expect(Number.isNaN(d.getTime())).toBe(true)
  })

  it('returns NaN for whitespace-only input', () => {
    const d = parseDaemonTs('   ')
    expect(Number.isNaN(d.getTime())).toBe(true)
  })
})

describe('parseDaemonTsMs', () => {
  it('returns the epoch-ms for a Z timestamp', () => {
    expect(parseDaemonTsMs('2026-04-24T10:15:32Z')).toBe(
      Date.UTC(2026, 3, 24, 10, 15, 32),
    )
  })

  it('returns NaN for empty input', () => {
    expect(Number.isNaN(parseDaemonTsMs(''))).toBe(true)
  })

  it('returns NaN for null input', () => {
    expect(Number.isNaN(parseDaemonTsMs(null))).toBe(true)
  })
})
