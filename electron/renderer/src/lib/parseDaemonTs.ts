// parseDaemonTs centralises timestamp parsing for any string the daemon
// hands us. Normally the Go side emits RFC3339 with a Z suffix
// (`2026-04-24T10:15:32Z`), but historic SQLite datetime('now') output and
// some legacy paths emit `YYYY-MM-DD HH:MM:SS` without an offset -- and JS
// `new Date()` parses that as LOCAL time, shifting by the user's UTC
// offset (7h on PDT). Treating that as UTC matches the daemon's actual
// semantics and keeps elapsed/ago computations sane.
//
// Returns NaN-safe Date. Use Number.isNaN(d.getTime()) at the call site
// to detect parse failure.

const SQL_LIKE = /^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d+)?$/

export function parseDaemonTs(input: string | null | undefined): Date {
  if (!input) return new Date(NaN)
  const trimmed = input.trim()
  if (trimmed === '') return new Date(NaN)

  // Already has a timezone designator (Z or +/-HH:MM)? Trust the parser.
  if (/(Z|[+\-]\d{2}:?\d{2})$/.test(trimmed)) {
    return new Date(trimmed)
  }

  // SQL-shaped string with no offset: assume UTC, normalise to ISO.
  if (SQL_LIKE.test(trimmed)) {
    const iso = trimmed.replace(' ', 'T') + 'Z'
    return new Date(iso)
  }

  // Fallback: hand to native Date and let it do its best. For most real
  // payloads this branch is unreachable.
  return new Date(trimmed)
}

// parseDaemonTsMs returns the epoch-ms or NaN. Convenience for elapsed
// math where we don't need a Date object.
export function parseDaemonTsMs(input: string | null | undefined): number {
  return parseDaemonTs(input).getTime()
}
