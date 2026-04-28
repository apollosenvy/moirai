import { useTasksStore } from '../store/tasksStore'

// PLAN panel: shows the current planner output for the selected task.
// The orchestrator stores a free-form multi-line plan string in
// task.plan. We preserve it verbatim (monospaced, wrapped) because
// planners vary in format -- numbered-step markdown, prose sections,
// JSON blobs. The UI's job is to display what the model emitted, not
// to parse it.
//
// Empty-state: "NO PLAN YET" until the planner responds for the first
// time. Fresh tasks land on this state immediately after submit.
export default function Plan() {
  const detail = useTasksStore((s) => s.detail)
  const task = detail?.task
  const plan = task?.plan?.trim() ?? ''
  const description = task?.description ?? ''

  const revisions = task?.replans ?? 0
  const revLabel =
    revisions > 0 ? `revision ${revisions + 1}` : 'revision 1'

  return (
    <section className="tk-panel">
      <div className="tk-panel-hd">
        <span className="title">PLAN</span>
        <span className="sub">SLOT A · {revLabel}</span>
      </div>
      <div className="tk-panel-body">
        {plan ? (
          <pre className="plan-raw">{plan}</pre>
        ) : (
          <div className="plan-empty">
            <div className="plan-empty-title">NO PLAN YET</div>
            {description && (
              <div className="plan-empty-sub">
                Task description:
                <br />
                <span className="plan-empty-desc">{description}</span>
              </div>
            )}
          </div>
        )}
      </div>
    </section>
  )
}
