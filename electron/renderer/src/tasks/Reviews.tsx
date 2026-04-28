import { useTasksStore } from '../store/tasksStore'

// REVIEWS panel: renders the reviewer's verdict history from
// task.reviews. Each entry is a string the orchestrator formatted as
// "<phase>: <body>" where phase is plan / code / etc. We parse the
// prefix into a chip and render the body verbatim. Variant classes
// (approved / revise / fix / reject) are derived from the body text
// so visual severity tracks the actual verdict keyword.
export default function Reviews() {
  const detail = useTasksStore((s) => s.detail)
  const reviews = detail?.task?.reviews ?? []

  return (
    <section className="tk-panel">
      <div className="tk-panel-hd">
        <span className="title">REVIEWS</span>
        <span className="sub">
          SLOT C · {reviews.length} verdict{reviews.length === 1 ? '' : 's'}
        </span>
      </div>
      <div className="tk-panel-body">
        {reviews.length === 0 ? (
          <div className="plan-empty">
            <div className="plan-empty-title">NO VERDICTS YET</div>
            <div className="plan-empty-sub">
              The reviewer emits a verdict when it completes a plan or code
              review. Streaming progress is visible in the TRACE panel.
            </div>
          </div>
        ) : (
          reviews.map((raw, i) => {
            const { phase, verdict, body } = parseReview(raw)
            const variant = variantFromVerdict(verdict ?? body)
            return (
              <div key={i} className={`review-card ${variant}`}>
                <div className="review-hd">
                  <span className="review-iter">ITER {i + 1}</span>
                  <span className={`review-verdict ${variant}`}>
                    {(verdict ?? phase ?? 'REVIEW').toUpperCase()}
                  </span>
                </div>
                <div className="review-body">{body}</div>
              </div>
            )
          })
        )}
      </div>
    </section>
  )
}

// Reviews land formatted as "plan: <json or prose>" or "code: ..." --
// the orchestrator prepends a phase tag so the reviewer's raw output
// can be traced back to when it fired. This splits the tag off for
// display and attempts to pull a JSON verdict field out of the body.
function parseReview(raw: string): {
  phase: string | null
  verdict: string | null
  body: string
} {
  let phase: string | null = null
  let body = raw
  const m = /^(plan|code|exec|review)\s*:\s*/i.exec(raw)
  if (m) {
    phase = m[1].toLowerCase()
    body = raw.slice(m[0].length)
  }

  let verdict: string | null = null
  // Many reviewers emit a JSON-ish blob like {"verdict":"approve",...}.
  // Parse it properly first -- the naive regex false-matches when the
  // body contains a NESTED "verdict" key (e.g. inside a reason string
  // like "the prior verdict was wrong"). Fall back to the regex for
  // non-JSON bodies (free-form prose, partial JSON, etc).
  try {
    const parsed = JSON.parse(body) as unknown
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      const v = (parsed as { verdict?: unknown }).verdict
      if (typeof v === 'string') verdict = v
    }
  } catch {
    /* not JSON -- fall through to regex */
  }
  if (!verdict) {
    const vm = /"verdict"\s*:\s*"([^"]+)"/i.exec(body)
    if (vm) verdict = vm[1]
  }

  return { phase, verdict, body: body.trim() }
}

function variantFromVerdict(s: string | null | undefined): string {
  if (!s) return 'fix'
  const lower = s.toLowerCase()
  if (/\b(approv|accept|pass|ok)/.test(lower)) return 'approved'
  if (/\brevise\b/.test(lower)) return 'revise'
  if (/\b(fix|reject|fail|deny)/.test(lower)) return 'fix'
  return 'fix'
}
