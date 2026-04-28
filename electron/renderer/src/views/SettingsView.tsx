import RectPanel from '../chrome/RectPanel'
import StatusDot from '../chrome/StatusDot'
import { useDaemonStore } from '../store/daemonStore'
import { usePrefsStore } from '../store/prefsStore'

// SETTINGS view. Read-only display of the effective daemon configuration
// plus the state paths Moirai reads/writes on disk. The daemon does not
// expose a /config endpoint, so this is intentionally informational --
// to change anything live, the user edits ~/.config/agent-router/config.json
// and restarts. We avoid faking control knobs that have no daemon route.

const NOTCH_VIEWBOX = '0 0 800 240'
const NOTCH_PATH = 'M0,0 L790,0 L800,10 L800,240 L0,240 Z'

const STATE_PATHS = [
  {
    label: 'Per-task state',
    path: '~/.local/share/agent-router/tasks/',
    note: 'Resume after restart',
  },
  {
    label: 'JSONL traces',
    path: '~/.local/share/agent-router/traces/<task-id>.jsonl',
    note: 'tail -f friendly',
  },
  {
    label: 'llama-server logs',
    path: '~/.local/share/agent-router/logs/',
    note: 'Per-slot stdout/stderr',
  },
  {
    label: 'Sandbox scratch',
    path: '~/.local/share/agent-router/scratch/',
    note: 'Writable from bwrap',
  },
  {
    label: 'L2 repo memory',
    path: '~/.local/share/agent-router/repo-memory.db',
    note: 'Per-repo facts (SQLite)',
  },
  {
    label: 'Config override',
    path: '~/.config/agent-router/config.json',
    note: 'Optional, defaults baked in',
  },
]

const SAFETY_RULES = [
  'No git push (force or otherwise)',
  'No git reset --hard, amend, or rebase -i',
  'No file deletion outside the sandbox',
  'No network from bwrap by default',
  'No paths outside the active repo root',
]

const TOOLS = [
  { name: 'ask_planner', detail: 'Send instruction to the planner slot; returns plan text' },
  { name: 'ask_coder', detail: 'Send instruction + plan to the coder slot; returns code' },
  { name: 'fs.read', detail: 'Read file relative to repo root' },
  { name: 'fs.write', detail: 'Write file (forbidden paths blocked)' },
  { name: 'fs.search', detail: 'ripgrep within repo' },
  { name: 'shell.exec', detail: 'bwrap-sandboxed shell, network off' },
  { name: 'test.run', detail: 'Run [commands].test from .agent-router.toml' },
  { name: 'compile.run', detail: 'Run [commands].compile' },
  { name: 'pensive.search', detail: 'Recall reasoning atoms for a project (k recent)' },
  { name: 'pensive.emit_atom', detail: 'Emit a discovery/failure/insight atom' },
  { name: 'done', detail: 'End the tool loop with a summary (gated by acceptance)' },
  { name: 'fail', detail: 'End the tool loop with a fail reason' },
]

function envName(): string {
  // Best-effort host hint. Hardcoded to "localhost" elsewhere in the
  // shell; here we just surface it next to the daemon block.
  return 'localhost'
}

export default function SettingsView() {
  const status = useDaemonStore((s) => s.status)
  const slots = useDaemonStore((s) => s.slots)
  const traceSparks = usePrefsStore((s) => s.traceSparks)
  const broadcastBand = usePrefsStore((s) => s.broadcastBand)
  const setTraceSparks = usePrefsStore((s) => s.setTraceSparks)
  const setBroadcastBand = usePrefsStore((s) => s.setBroadcastBand)

  return (
    <div className="metrics-main">
      <div className="settings-banner">
        <span className="settings-banner-tag">READ ONLY</span>
        <span>
          These values come from the running daemon. To change them, edit
          <code> ~/.config/agent-router/config.json </code>
          and restart <code>moirai daemon</code>.
        </span>
      </div>

      <div className="metrics-grid">
        {/* DAEMON CARD */}
        <RectPanel className="metrics-card" viewBox={NOTCH_VIEWBOX} borderPath={NOTCH_PATH}>
          <header className="metrics-card-hd">
            <span>DAEMON</span>
            <StatusDot variant={status ? 'lime' : 'magenta'} />
          </header>
          <dl className="metrics-list">
            <div><dt>HOST</dt><dd>{envName()}</dd></div>
            <div><dt>SERVICE</dt><dd>{status?.service ?? 'agent-router'}</dd></div>
            <div><dt>VERSION</dt><dd>{status?.daemon_version ?? '--'}</dd></div>
            <div><dt>BIND</dt><dd>127.0.0.1:{status?.port ?? 5984}</dd></div>
            <div><dt>STARTED</dt><dd className="metrics-dim">{status?.started_at ?? '--'}</dd></div>
            <div><dt>UPTIME</dt><dd>{status?.uptime ?? '--'}</dd></div>
            <div>
              <dt>TURBOQUANT</dt>
              <dd>
                {status?.turboquant_supported === true && <span className="metrics-ok">SUPPORTED</span>}
                {status?.turboquant_supported === false && <span className="metrics-warn">NOT SUPPORTED</span>}
                {status?.turboquant_supported === undefined && '--'}
              </dd>
            </div>
          </dl>
        </RectPanel>

        {/* ITERATION CAPS CARD */}
        <RectPanel className="metrics-card" viewBox={NOTCH_VIEWBOX} borderPath={NOTCH_PATH}>
          <header className="metrics-card-hd">
            <span>ITERATION CAPS</span>
          </header>
          <dl className="metrics-list">
            <div>
              <dt>RO TURNS</dt>
              <dd>{status?.max_ro_turns ?? '--'}</dd>
            </div>
            <div>
              <dt>CODER RETRIES</dt>
              <dd className="metrics-dim">5 (default, not on /status)</dd>
            </div>
            <div>
              <dt>REPLANS (legacy)</dt>
              <dd className="metrics-dim">3 (default, not on /status)</dd>
            </div>
            <div>
              <dt>PLAN REVISIONS</dt>
              <dd className="metrics-dim">3 (default, not on /status)</dd>
            </div>
            <div>
              <dt>BODY CAP (SUBMIT)</dt>
              <dd>256 KiB</dd>
            </div>
            <div>
              <dt>BODY CAP (PATCH SLOT)</dt>
              <dd>64 KiB</dd>
            </div>
          </dl>
          <div className="settings-foot-note">
            Daemon does not expose iteration caps on <code>/status</code>; defaults
            are listed for reference. Edit config to override.
          </div>
        </RectPanel>

        {/* SAFETY CARD */}
        <RectPanel className="metrics-card" viewBox={NOTCH_VIEWBOX} borderPath={NOTCH_PATH}>
          <header className="metrics-card-hd">
            <span>SAFETY</span>
            <StatusDot variant="lime" />
          </header>
          <ul className="settings-rules">
            {SAFETY_RULES.map((rule) => (
              <li key={rule}>
                <span className="settings-rules-glyph">✗</span>
                {rule}
              </li>
            ))}
          </ul>
        </RectPanel>

        {/* UI PREFERENCES CARD */}
        <RectPanel className="metrics-card" viewBox={NOTCH_VIEWBOX} borderPath={NOTCH_PATH}>
          <header className="metrics-card-hd">
            <span>UI PREFERENCES</span>
          </header>
          <ul className="settings-prefs">
            <li>
              <span className="settings-prefs-lbl">TRACE SPARKS</span>
              <span className="settings-prefs-detail">
                Random plasma dots travelling the hex grid every 30-90 seconds
                (brief §10 ambient signature).
              </span>
              <button
                type="button"
                role="switch"
                aria-checked={traceSparks}
                aria-label="Toggle trace sparks"
                className="settings-toggle"
                onClick={() => setTraceSparks(!traceSparks)}
              />
            </li>
            <li>
              <span className="settings-prefs-lbl">BROADCAST BAND</span>
              <span className="settings-prefs-detail">
                Ice/lime/amber stripe sweep on swap, verdict, or task submit
                (brief §9 motion vocabulary).
              </span>
              <button
                type="button"
                role="switch"
                aria-checked={broadcastBand}
                aria-label="Toggle broadcast band"
                className="settings-toggle"
                onClick={() => setBroadcastBand(!broadcastBand)}
              />
            </li>
          </ul>
          <div className="settings-foot-note">
            Stored in <code>localStorage</code> under <code>moirai.prefs.*</code>.
            Toggling does not touch the daemon.
          </div>
        </RectPanel>

        {/* SLOTS PATHS CARD */}
        <RectPanel
          className="metrics-card metrics-card-wide"
          viewBox={NOTCH_VIEWBOX}
          borderPath={NOTCH_PATH}
        >
          <header className="metrics-card-hd">
            <span>SLOT BINDINGS</span>
            <span className="metrics-total">{slots.length} / 3</span>
          </header>
          <table className="metrics-slot-table settings-bind-table">
            <thead>
              <tr>
                <th>SLOT</th>
                <th>MODEL PATH</th>
                <th>CTX</th>
                <th>KV</th>
                <th>PORT</th>
              </tr>
            </thead>
            <tbody>
              {slots.length === 0 && (
                <tr>
                  <td colSpan={5} className="metrics-dim">
                    no slots reported (daemon idle or not yet warmed)
                  </td>
                </tr>
              )}
              {slots.map((s) => (
                <tr key={s.slot}>
                  <td>{s.slot.toUpperCase()}</td>
                  <td className="settings-path" title={s.model_path}>
                    {s.model_path || '--'}
                  </td>
                  <td className="num">{s.ctx_size || '--'}</td>
                  <td>{s.kv_cache || '--'}</td>
                  <td className="num">{s.listen_port || '--'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </RectPanel>

        {/* STATE PATHS CARD */}
        <RectPanel
          className="metrics-card metrics-card-wide"
          viewBox={NOTCH_VIEWBOX}
          borderPath={NOTCH_PATH}
        >
          <header className="metrics-card-hd">
            <span>STATE PATHS</span>
          </header>
          <ul className="settings-paths">
            {STATE_PATHS.map((p) => (
              <li key={p.path}>
                <span className="settings-paths-lbl">{p.label}</span>
                <code className="settings-path">{p.path}</code>
                <span className="settings-paths-note">{p.note}</span>
              </li>
            ))}
          </ul>
        </RectPanel>

        {/* TOOLS CARD */}
        <RectPanel
          className="metrics-card metrics-card-wide"
          viewBox={NOTCH_VIEWBOX}
          borderPath={NOTCH_PATH}
        >
          <header className="metrics-card-hd">
            <span>CODER TOOLBELT</span>
          </header>
          <ul className="settings-tools">
            {TOOLS.map((t) => (
              <li key={t.name}>
                <code className="settings-tool-name">{t.name}</code>
                <span className="settings-tool-detail">{t.detail}</span>
              </li>
            ))}
          </ul>
        </RectPanel>
      </div>
    </div>
  )
}
