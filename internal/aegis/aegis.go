// Package aegis is a thin client to the existing L1/L2/L3 memory stack.
//
// L1: in-process task state (the orchestrator holds this directly; we just
//     expose token-budget helpers).
// L2: per-repo SQLite DB at ~/.local/share/agent-router/repo-memory.db.
//     TODO(rename): consider migrating to ~/.local/share/moirai/ in a future commit;
//     orphaning existing repo-memory data in production today.
// L3: engram-emit CLI for writes, pensive-recall CLI for reads.
//
// Subagent trust rules apply: nothing in this package ever deletes.
package aegis

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// --- L2: per-repo SQLite ---------------------------------------------------

type L2 struct {
	path string
	db   *sql.DB
}

func l2Path() string {
	home, _ := os.UserHomeDir()
	// TODO(rename): consider migrating to ~/.local/share/moirai/ in a future commit;
	// orphaning existing repo-memory.db in production today.
	return filepath.Join(home, ".local", "share", "agent-router", "repo-memory.db")
}

func OpenL2() (*L2, error) {
	p := l2Path()
	// SEC-PASS5-005: 0700 dir; SQLite file is chmod'd to 0600 after open
	// below. The L2 stores verdict reasoning (model-authored prose about
	// task code), which can echo file content; not world-readable.
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(filepath.Dir(p), 0o700)
	// Pre-open integrity check: if the file exists and SQLite reports
	// corruption, rename it aside and start fresh so we don't leave the
	// daemon stuck unable to record verdicts. This is an L2 cache; losing
	// it costs us cross-task memory recall but does not lose primary task
	// state (that's in taskstore JSON files). Renaming preserves the
	// corrupt copy for postmortem.
	if _, statErr := os.Stat(p); statErr == nil {
		probe, probeErr := sql.Open("sqlite3", p+"?_journal=WAL&_busy_timeout=5000&mode=rw")
		if probeErr == nil {
			row := probe.QueryRow(`PRAGMA integrity_check`)
			var result string
			if scanErr := row.Scan(&result); scanErr != nil || result != "ok" {
				_ = probe.Close()
				corruptName := fmt.Sprintf("%s.corrupt-%d", p, time.Now().Unix())
				_ = os.Rename(p, corruptName)
				// Best-effort rename of the WAL/SHM siblings so a
				// fresh DB doesn't reattach to corrupt journals.
				_ = os.Rename(p+"-wal", corruptName+"-wal")
				_ = os.Rename(p+"-shm", corruptName+"-shm")
				fmt.Fprintf(os.Stderr, "aegis L2: integrity_check failed (%q); renamed to %s and starting fresh\n", result, corruptName)
			} else {
				_ = probe.Close()
			}
		}
	}
	db, err := sql.Open("sqlite3", p+"?_journal=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	// Tighten file mode after open. go-sqlite3 doesn't expose a creation
	// mode on the DSN, so we Chmod the file (and any WAL/SHM siblings the
	// driver may have created) explicitly. Best-effort: if Chmod fails the
	// daemon still works; we just log a warning to stderr.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		fp := p + suffix
		if _, statErr := os.Stat(fp); statErr == nil {
			if chmErr := os.Chmod(fp, 0o600); chmErr != nil {
				fmt.Fprintf(os.Stderr, "aegis L2: chmod %s: %v\n", fp, chmErr)
			}
		}
	}
	// SQLite + go-sqlite3 wants a single writer connection. With the default
	// unbounded pool, two run goroutines emitting RememberFact/RecordVerdict
	// concurrently can each open their own write transaction and hit
	// SQLITE_BUSY despite the busy_timeout. Cap the pool at 1 so writes
	// serialize cleanly and the busy_timeout never has to fire.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS repo_facts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repo_root TEXT NOT NULL,
  kind TEXT NOT NULL,
  key TEXT,
  content TEXT NOT NULL,
  ts TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_repo_facts_repo ON repo_facts(repo_root, kind);
CREATE TABLE IF NOT EXISTS verdicts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repo_root TEXT NOT NULL,
  task_id TEXT NOT NULL,
  phase TEXT NOT NULL,
  verdict TEXT NOT NULL,
  reasoning TEXT,
  ts TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_verdicts_repo ON verdicts(repo_root, task_id);
`); err != nil {
		db.Close()
		return nil, err
	}
	return &L2{path: p, db: db}, nil
}

func (l *L2) Close() error { return l.db.Close() }

func (l *L2) Path() string { return l.path }

// RememberFact stores something the reviewer or planner learned about the repo.
func (l *L2) RememberFact(repo, kind, key, content string) error {
	_, err := l.db.Exec(`INSERT INTO repo_facts(repo_root, kind, key, content) VALUES (?, ?, ?, ?)`,
		repo, kind, key, content)
	return err
}

type Fact struct {
	Kind    string
	Key     string
	Content string
	TS      string
}

func (l *L2) RecallFacts(repo, kind string, limit int) ([]Fact, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := l.db.Query(`SELECT kind, COALESCE(key,''), content, ts FROM repo_facts WHERE repo_root=? AND (?='' OR kind=?) ORDER BY ts DESC LIMIT ?`,
		repo, kind, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		var f Fact
		if err := rows.Scan(&f.Kind, &f.Key, &f.Content, &f.TS); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RecordVerdict writes a reviewer verdict for later introspection.
func (l *L2) RecordVerdict(repo, taskID, phase, verdict, reasoning string) error {
	_, err := l.db.Exec(`INSERT INTO verdicts(repo_root, task_id, phase, verdict, reasoning) VALUES (?, ?, ?, ?, ?)`,
		repo, taskID, phase, verdict, reasoning)
	return err
}

// --- L3: engram / pensive ---------------------------------------------------

// L3Emit shells out to engram-emit to record a cross-repo insight. Uses the
// richer flag-based CLI. Errors are surfaced but never fail the parent
// operation; call sites should log and continue.
func L3Emit(ctx context.Context, kind, project, content string, tags []string) error {
	tagsCsv := strings.Join(tags, ",")
	_, err := PensiveEmit(ctx, kind, project, content, "", tagsCsv)
	return err
}

// PensiveEmit writes an atom to Engram via engram-emit using the full CLI
// flag surface (principle, context, tags, domain, topic). Returns a short
// text result (e.g. emission id) on success; error if the CLI is missing
// or exits non-zero.
//
// kind:      "discovery" | "failure" (RO's emit_atom maps "insight" to
//            "discovery" with a tag).
// project:   arbitrary project tag, e.g. "training:llamacpp-commits" or
//            the repo under work. Used for project-scoped recall.
// principle: the transferable rule / lesson / verdict reason.
// context:   the situational "shape" of the problem; what the lesson is
//            about. May be empty.
// tagsCsv:   comma-separated tags. Caller passes through whatever RO
//            supplied plus any orchestrator-injected provenance tags.
func PensiveEmit(ctx context.Context, kind, project, principle, contextStr, tagsCsv string) (string, error) {
	bin, err := exec.LookPath("engram-emit")
	if err != nil {
		return "", fmt.Errorf("engram-emit not on PATH")
	}
	if kind == "" {
		kind = "discovery"
	}
	if project == "" {
		project = "agent-router"
	}
	if principle == "" {
		return "", fmt.Errorf("principle required")
	}

	// Derive a domain tag from the csv for engram's --domain field. We pick
	// "reviewer" if the caller mentioned it; else "coder" if mentioned; else
	// "general". RO can override by including an explicit domain tag.
	domain := "general"
	if strings.Contains(tagsCsv, "reviewer") {
		domain = "reviewer"
	} else if strings.Contains(tagsCsv, "coder") {
		domain = "coder"
	}
	// Pick a topic: first tag token, fallback to domain.
	topic := domain
	if tagsCsv != "" {
		topic = strings.SplitN(tagsCsv, ",", 2)[0]
	}

	args := []string{
		kind,
		"--project", project,
		"--principle", principle,
		"--domain", domain,
		"--topic", topic,
	}
	if contextStr != "" {
		args = append(args, "--context", contextStr)
	}
	if tagsCsv != "" {
		args = append(args, "--tags", tagsCsv)
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, args...)
	var out strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("engram-emit rc!=0: %s", strings.TrimSpace(stderr.String()))
	}
	result := strings.TrimSpace(out.String())
	if result == "" {
		result = fmt.Sprintf("emitted atom (kind=%s project=%s)", kind, project)
	}
	return result, nil
}

// L3Recall queries pensive-recall for cross-repo matches. Returns the CLI's
// raw JSON (parsed into a generic map) plus a short text summary.
type L3Hit struct {
	Score   float64 `json:"score"`
	Text    string  `json:"text"`
	Source  string  `json:"source"`
	Project string  `json:"project"`
}

// L3Recall shells out to pensive-recall. The CLI expects a --project argument,
// which we default to "agent-router" when the caller hasn't specified one;
// this keeps backward compatibility with older callers that passed only a
// query string. The query parameter is intentionally ignored -- the current
// pensive-recall CLI surfaces the most-recent atoms scoped to a project; it
// does not accept a positional query. Kept on the signature for back-compat
// with older callers; rename to L3RecentByProject is preferred for new code.
func L3Recall(ctx context.Context, _ string, limit int) ([]L3Hit, error) {
	return L3RecentByProject(ctx, "agent-router", limit)
}

// L3RecentByProject returns the most recent atoms in pensive scoped to the
// given project. The function name reflects what the underlying CLI actually
// does -- the previous spelling (L3RecallProject) implied a query-aware recall
// that the CLI doesn't support, which led to a silent `_ = query` discard.
// New callers should prefer this name.
func L3RecentByProject(ctx context.Context, project string, limit int) ([]L3Hit, error) {
	bin, err := exec.LookPath("pensive-recall")
	if err != nil {
		return nil, fmt.Errorf("pensive-recall not on PATH")
	}
	if limit <= 0 {
		limit = 5
	}
	if project == "" {
		project = "agent-router"
	}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// pensive-recall usage: pensive-recall --project NAME [--compact] [--limit N] [--json]
	// We pass --json so we can parse the output reliably across CLI shape
	// variants. The CLI does not accept a positional query string, so callers
	// that need text matching must filter the returned hits client-side.
	cmd := exec.CommandContext(runCtx, bin,
		"--project", project,
		"--limit", fmt.Sprintf("%d", limit),
		"--json",
	)
	out, err := cmd.Output()
	if err != nil {
		// Non-zero exit is not always fatal; stderr may contain useful text.
		// Surface the error upward; caller decides whether to log-and-continue.
		return nil, fmt.Errorf("pensive-recall: %w", err)
	}
	// The CLI's JSON shape varies by version. Try the two shapes we've seen:
	// 1) a bare list of hits
	// 2) an object with {"atoms": [...]} or {"hits": [...]}
	var raw []L3Hit
	if err := json.Unmarshal(out, &raw); err == nil && len(raw) > 0 {
		return raw, nil
	}
	var wrapped struct {
		Hits    []L3Hit `json:"hits"`
		Atoms   []L3Hit `json:"atoms"`
		Results []L3Hit `json:"results"`
	}
	if err := json.Unmarshal(out, &wrapped); err == nil {
		switch {
		case len(wrapped.Hits) > 0:
			return wrapped.Hits, nil
		case len(wrapped.Atoms) > 0:
			return wrapped.Atoms, nil
		case len(wrapped.Results) > 0:
			return wrapped.Results, nil
		}
	}
	// Fallback: return a synthetic single-hit so the RO sees *something* it
	// can reason about rather than an empty list when the shape is entirely
	// unexpected.
	return []L3Hit{{Text: strings.TrimSpace(string(out))}}, nil
}

// PensiveSearchRaw returns the combined text of the top-k hits, formatted as
// a short block suitable for injection into a tool-call <RESULT>. Empty string
// means no matches. Errors are surfaced to the caller.
func PensiveSearchRaw(ctx context.Context, project, query string, k int) (string, error) {
	// The underlying CLI does not accept a query string; it returns the most
	// recent project-scoped atoms. We keep `query` on the signature for
	// caller-facing API stability and (for now) discard it before the shell.
	_ = query
	hits, err := L3RecentByProject(ctx, project, k)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "(no pensive hits)", nil
	}
	var b strings.Builder
	for i, h := range hits {
		if i >= k {
			break
		}
		fmt.Fprintf(&b, "[%d] score=%.3f src=%s project=%s\n", i+1, h.Score, h.Source, h.Project)
		t := h.Text
		if len(t) > 1200 {
			t = t[:1200] + "...[truncated]"
		}
		b.WriteString(t)
		b.WriteString("\n---\n")
	}
	return b.String(), nil
}
