package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBashOnlyPlannerPromptContract pins the bash-only planner prompt's
// load-bearing substrings. These are the contract between the planner
// role-prompt and:
//   - the orchestrator-side parser (plan.Parse expects the JSON shape)
//   - the verify-shape validator (validAcceptanceVerify accepts only
//     file:<path> and bash:<cmd>:pass)
//   - the bash-only doc layout (docs/plans/PLAN.md)
//   - Karpathy discipline knobs (Think Before, Goal-Driven, Paranoid)
//
// A regression that drops any of these silently breaks the run BEFORE
// the planner emits a parseable plan -- a hard failure mode where the
// orchestrator never knows what acceptance items even exist.
func TestBashOnlyPlannerPromptContract(t *testing.T) {
	prompt := bashOnlyPlannerSystemPrompt()
	for _, want := range []string{
		"json",                       // fenced JSON output
		"phases",                     // top-level field
		"acceptance",                 // top-level field
		"verify",                     // AcceptanceItem field
		"file:",                      // legal verify shape
		"bash:",                      // legal verify shape (bash tool)
		":pass",                      // closer of bash verify
		"docs/plans/PLAN.md",         // canonical PLAN.md path
		"PATH DISCIPLINE",            // path canonicalization
		"PHASE GRANULARITY",          // phase-sizing guidance
		"THINK BEFORE PLANNING",      // Karpathy
		"GOAL-DRIVEN ACCEPTANCE",     // Karpathy
		"PARANOID ABOUT AMBIGUITY",   // stance
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("bashOnlyPlannerSystemPrompt missing required substring %q", want)
		}
	}
}

// TestBashOnlyPlannerPromptForbidsLegacyVerify pins the negative side: the
// new prompt MUST NOT advertise test.run:pass / compile.run:pass as
// acceptance verify shapes -- those are the legacy shapes that produced
// the v2 verify-vocabulary inflation finding (one passing test ticked 8
// items at once). The new shape (bash:<exact cmd>:pass) forces per-item
// granularity. If a copy-paste from the legacy prompt re-introduces the
// old shapes, the planner emits ambiguous verify and the bug returns.
//
// This is the documented exception: legacy prompts CAN mention these,
// but the bash-only one must not. Run this test against ONLY the
// bash-only variant, not the legacy one.
func TestBashOnlyPlannerPromptForbidsLegacyVerify(t *testing.T) {
	prompt := bashOnlyPlannerSystemPrompt()
	for _, banned := range []string{
		"test.run:pass",
		"compile.run:pass",
	} {
		if strings.Contains(prompt, banned) {
			t.Errorf("bashOnlyPlannerSystemPrompt should NOT mention legacy verify %q (causes verify-vocabulary inflation)", banned)
		}
	}
}

// TestBashOnlyCoderPromptContract pins the coder role-prompt for
// bash-only mode. Substrings pinned:
//   - "bash"            -- the only tool the coder uses
//   - "heredoc"         -- the file-write idiom
//   - "<<'EOF'"         -- the quoted heredoc that suppresses expansion
//   - "SIMPLICITY FIRST"
//   - "speculative"     -- negative pressure
//   - "OUTPUT FORMAT"
//   - "fenced"          -- emit fenced bash blocks
//   - "docs/plans/PLAN.md"
func TestBashOnlyCoderPromptContract(t *testing.T) {
	prompt := bashOnlyCoderSystemPrompt(false)
	for _, want := range []string{
		"bash",
		"heredoc",
		"<<'EOF'",
		"SIMPLICITY FIRST",
		"speculative",
		"OUTPUT FORMAT",
		"fenced",
		"docs/plans/PLAN.md",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("bashOnlyCoderSystemPrompt missing required substring %q", want)
		}
	}
}

// TestBashOnlyCoderRetryModeAddsSurgicalChanges pins the retry-mode
// extension to the coder prompt: when the previous attempt failed, the
// coder must be told to make SURGICAL CHANGES and emit a minimal patch.
// Without this nudge, the coder's default "rewrite the whole file"
// response burns context and risks regressing working code.
func TestBashOnlyCoderRetryModeAddsSurgicalChanges(t *testing.T) {
	prompt := bashOnlyCoderSystemPrompt(true)
	for _, want := range []string{
		"RETRY MODE",
		"SURGICAL CHANGES",
		"minimal patch",
		"cat",
		"grep",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("bashOnlyCoderSystemPrompt(retry=true) missing %q", want)
		}
	}
	// Negative: retry-mode prompt must still carry the base format
	// contract -- a regression that nukes the heredoc instruction
	// would silently strand the coder in retry mode without a way to
	// write files.
	if !strings.Contains(prompt, "heredoc") {
		t.Error("retry-mode prompt should still carry the base heredoc instruction")
	}
}

// TestBashOnlyROPromptContract pins the reviewer role-prompt for bash-only
// mode. The STATUS-line scheme replaces done(); the REVIEW.md path is the
// audit trail; the discipline keywords (Goal-Driven, Gatekeeper, scope
// creep, REVIEW DISCIPLINE) all appear.
func TestBashOnlyROPromptContract(t *testing.T) {
	for _, auditMode := range []bool{false, true} {
		prompt := bashOnlyROSystemPrompt(auditMode)
		for _, want := range []string{
			"GOAL-DRIVEN EXECUTION",
			"GATEKEEPER",
			"REVIEW DISCIPLINE",
			"scope creep",
			"docs/review/REVIEW.md",
			"## STATUS: DONE",
			"## STATUS: BLOCKED:",
			"BEFORE done()",
			"failing test",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("bashOnlyROSystemPrompt(auditMode=%v) missing %q", auditMode, want)
			}
		}
	}
}

// TestBashOnlyROPromptAuditModeConditional pins the audit-mode gate the
// same way the legacy prompt's audit-mode test does: AUDIT-ONLY content
// MUST NOT appear in the normal prompt and MUST appear in the audit
// prompt. Burns ~40 lines of context for non-audit tasks otherwise.
func TestBashOnlyROPromptAuditModeConditional(t *testing.T) {
	normal := bashOnlyROSystemPrompt(false)
	audit := bashOnlyROSystemPrompt(true)

	for _, marker := range []string{
		"AUDIT-ONLY MODE",
		"AUDIT PERSONA ROTATION",
		"security-OWASP",
		"CHECKLIST DISCIPLINE",
	} {
		if strings.Contains(normal, marker) {
			t.Errorf("auditMode=false prompt should NOT contain %q (context burn for normal tasks)", marker)
		}
		if !strings.Contains(audit, marker) {
			t.Errorf("auditMode=true prompt MUST contain %q", marker)
		}
	}
	// audit-mode strictly extends normal.
	if !strings.HasPrefix(audit, normal) {
		t.Error("auditMode=true prompt must extend (not replace) the normal prompt")
	}
}

// TestBashOnlyTerminalStatusDetectsDone pins the STATUS: DONE detector
// for the canonical reviewer-written REVIEW.md. A `## STATUS: DONE` line
// as the last non-blank line MUST be reported as kind="done".
func TestBashOnlyTerminalStatusDetectsDone(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs", "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Review\n\n## Turn 1\nSomething happened.\n\n## STATUS: DONE\n"
	if err := os.WriteFile(filepath.Join(repo, "docs", "review", "REVIEW.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	kind, reason, ok := bashOnlyTerminalStatus(repo)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if kind != "done" {
		t.Errorf("kind = %q, want %q", kind, "done")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

// TestBashOnlyTerminalStatusDetectsBlocked pins the BLOCKED detector
// including the reason capture.
func TestBashOnlyTerminalStatusDetectsBlocked(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs", "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Review\n\n## STATUS: BLOCKED: cannot satisfy acceptance A2 within budget\n"
	if err := os.WriteFile(filepath.Join(repo, "docs", "review", "REVIEW.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	kind, reason, ok := bashOnlyTerminalStatus(repo)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if kind != "blocked" {
		t.Errorf("kind = %q, want %q", kind, "blocked")
	}
	if !strings.Contains(reason, "cannot satisfy acceptance A2") {
		t.Errorf("reason = %q, expected the trailing payload", reason)
	}
}

// TestBashOnlyTerminalStatusIgnoresEmbeddedSTATUS pins the "last non-blank
// line" rule: a STATUS line that appears mid-document (e.g. inside a
// section deliberating about what status to write) MUST NOT terminate
// the run. Only the very last non-blank line counts.
func TestBashOnlyTerminalStatusIgnoresEmbeddedSTATUS(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs", "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `# Review

## Turn 3
Considering whether to write ## STATUS: DONE here -- not yet, build still
fails. Continuing.

## Turn 4
Build now passes; tests still need to run.
`
	if err := os.WriteFile(filepath.Join(repo, "docs", "review", "REVIEW.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	kind, _, ok := bashOnlyTerminalStatus(repo)
	if ok {
		t.Errorf("expected ok=false (STATUS embedded mid-doc), got kind=%q", kind)
	}
}

// TestBashOnlyTerminalStatusEmptyReason returns "(no reason provided)"
// when the reviewer wrote BLOCKED with no payload after the colon.
// Mirrors the existing fail() handler's empty-reason sanitation so
// LastError doesn't end up as "fail: ".
func TestBashOnlyTerminalStatusEmptyReason(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs", "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Review\n## STATUS: BLOCKED: \n"
	if err := os.WriteFile(filepath.Join(repo, "docs", "review", "REVIEW.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, reason, ok := bashOnlyTerminalStatus(repo)
	if !ok {
		t.Fatal("expected ok=true even for empty reason")
	}
	if reason != "(no reason provided)" {
		t.Errorf("reason = %q, want sentinel", reason)
	}
}

// TestBashOnlyTerminalStatusMissingFile returns ok=false cleanly when
// REVIEW.md doesn't exist (the reviewer hasn't written one yet, normal
// state during early turns).
func TestBashOnlyTerminalStatusMissingFile(t *testing.T) {
	repo := t.TempDir()
	_, _, ok := bashOnlyTerminalStatus(repo)
	if ok {
		t.Error("expected ok=false for missing REVIEW.md")
	}
}

// TestPromptSelectorReturnsBashOnlyWhenConfigured pins the wiring
// between Config.ToolSurface and the selector helpers. A misconfigured
// selector silently routes a bash-only daemon to the legacy prompts,
// which would tell the model to emit fs.write JSON tool calls -- those
// fail in bash-only mode (the legacy fs.write dispatcher is still
// registered and would write files, but the prompt says bash and the
// real flow would diverge from what the v3 trace expects).
//
// Test is a property check: the selector for ToolSurface=="bash-only"
// returns the bash-only string, and for "" returns the legacy string.
// Comparing the WHOLE prompt would be fragile; we just check a
// signature substring unique to each variant.
func TestPromptSelectorReturnsBashOnlyWhenConfigured(t *testing.T) {
	cases := []struct {
		surface       string
		wantBashOnly  bool
	}{
		{"", false},
		{"legacy", false},
		{"bash-only", true},
	}
	for _, tc := range cases {
		o := &Orchestrator{cfg: Config{ToolSurface: tc.surface}}

		// Planner: legacy mentions fs.write tool, bash-only doesn't.
		plan := o.plannerSystemPromptForCfg()
		gotBashOnly := strings.Contains(plan, "docs/plans/PLAN.md")
		if gotBashOnly != tc.wantBashOnly {
			t.Errorf("planner surface=%q: gotBashOnly=%v want=%v", tc.surface, gotBashOnly, tc.wantBashOnly)
		}

		// Coder: bash-only uses the quoted heredoc form `<<'EOF'`.
		// Legacy talks about `# file:` markers and never mentions
		// heredoc fence-quoting. The `<<'EOF'` signature is unique
		// to the bash-only prompt body (legacy mentions "heredoc" in
		// passing inside the nested-fence guidance, but never the
		// quoted form).
		coder := o.coderSystemPromptForCfg(false)
		gotBashOnlyC := strings.Contains(coder, "<<'EOF'")
		if gotBashOnlyC != tc.wantBashOnly {
			t.Errorf("coder surface=%q: gotBashOnly=%v want=%v", tc.surface, gotBashOnlyC, tc.wantBashOnly)
		}

		// Reviewer: bash-only mentions docs/review/REVIEW.md.
		ro := o.roSystemPromptForCfg(false)
		gotBashOnlyR := strings.Contains(ro, "docs/review/REVIEW.md")
		if gotBashOnlyR != tc.wantBashOnly {
			t.Errorf("ro surface=%q: gotBashOnly=%v want=%v", tc.surface, gotBashOnlyR, tc.wantBashOnly)
		}
	}
}
