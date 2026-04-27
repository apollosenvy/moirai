package orchestrator

import (
	"strings"
	"testing"
)

// TestPlannerSystemPromptContainsParserContract closes audit-pass-1
// COV-IMP-8: the planner system prompt is the contract that produces
// the JSON plan downstream parsers depend on. If a refactor accidentally
// removes the JSON-block instruction or changes the verify vocabulary,
// the planner stops emitting parseable plans and st.plan stays nil
// forever -- a SILENT failure mode (plan injection just doesn't fire).
//
// Pin the substrings the parsers and matchers depend on, plus the
// Karpathy-derived behavioral guidance (Think Before, Goal-Driven).
func TestPlannerSystemPromptContainsParserContract(t *testing.T) {
	prompt := plannerSystemPrompt()
	for _, want := range []string{
		"json",                       // requires fenced JSON output
		"phases",                     // top-level field plan.Plan.Phases
		"acceptance",                 // top-level field plan.Plan.Acceptance
		"verify",                     // AcceptanceItem.Verify field
		"file:",                      // verify shape that auto-ticks on fs.write
		"test.run:pass",              // verify shape that auto-ticks on test.run
		"compile.run:pass",           // verify shape that auto-ticks on compile.run
		"PATH DISCIPLINE",            // path-canonicalization guidance
		"PHASE GRANULARITY",          // phase-sizing guidance
		"THINK BEFORE PLANNING",      // Karpathy: surface assumptions
		"GOAL-DRIVEN ACCEPTANCE",     // Karpathy: every criterion is a test
		"PARANOID ABOUT AMBIGUITY",   // stance line
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("plannerSystemPrompt missing required substring %q", want)
		}
	}
}

// TestCoderSystemPromptContainsExtractorContract closes audit-pass-1
// COV-IMP-8 (coder side): if the coder prompt loses the `# file:` marker
// instruction, autoExtractAndCommit returns 0 files and the checklist
// never ticks. Pin the substrings that prompt-engineering depends on.
func TestCoderSystemPromptContainsExtractorContract(t *testing.T) {
	prompt := coderSystemPrompt(false)
	for _, want := range []string{
		"# file:",             // marker the extractor recognizes
		"// file:",            // alternate marker (TypeScript-style)
		"FIRST LINE",          // emphasis on marker placement
		"OUTPUT FORMAT",       // section header
		"fenced",              // fence convention
		"fs.write",            // negative instruction (don't emit JSON tool calls)
		"BLANK LINES",         // multi-file separator convention
		"SIMPLICITY FIRST",    // Karpathy: minimum code that solves the request
		"speculative",         // negative pressure: no speculative abstractions
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("coderSystemPrompt missing required substring %q", want)
		}
	}
}

// TestCoderSystemPromptRetryModeAddsFsAccess closes the same finding for
// the retry-mode branch: when retryMode is true, the prompt MUST grant
// fs.read / fs.search access and tell the coder to inspect the existing
// code before re-emitting.
func TestCoderSystemPromptRetryModeAddsFsAccess(t *testing.T) {
	prompt := coderSystemPrompt(true)
	for _, want := range []string{
		"RETRY MODE",
		"fs.read",
		"fs.search",
		"SURGICAL CHANGES",   // Karpathy: minimal patch in retry mode
		"minimal patch",      // explicit anti-rewrite framing
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("coderSystemPrompt(retry=true) missing %q", want)
		}
	}
	// Negative: retry-mode prompt should also include the base
	// non-retry contract so the coder doesn't lose its format rules.
	if !strings.Contains(prompt, "# file:") {
		t.Error("retry-mode prompt should still carry the base format contract")
	}
}

// TestReviewerSystemPromptContainsGoalDriven pins the Goal-Driven
// Execution section in the reviewer prompt -- the Karpathy principle
// that every dispatch carries its own success test. A regression that
// drops this section silently lets the reviewer fall back to vibes
// (dispatch "fix it" without a test, call done() without test.run).
//
// Tests both auditMode=false (normal builds) and auditMode=true: the
// Goal-Driven section MUST be present in both renderings, since it
// applies to every reviewer dispatch.
func TestReviewerSystemPromptContainsGoalDriven(t *testing.T) {
	for _, auditMode := range []bool{false, true} {
		prompt := roSystemPrompt(auditMode)
		for _, want := range []string{
			"GOAL-DRIVEN EXECUTION", // section header
			"failing test",          // Karpathy: write failing test, then make it pass
			"BEFORE done()",         // pressure to call test.run before terminating
			"GATEKEEPER",            // stance line
			"REVIEW DISCIPLINE",     // RO actually reviews, not just routes
			"scope creep",           // pressure to push back when coder over-builds
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("roSystemPrompt(auditMode=%v) missing required substring %q", auditMode, want)
			}
		}
	}
}

// TestReviewerSystemPromptAuditModeConditional pins the AAR finding
// that AUDIT-ONLY MODE was being injected into every reviewer prompt
// even when the task wasn't an audit -- ~70 lines of context burn for
// 99% of tasks, plus lost-in-the-middle risk. Now gated on the auditMode
// flag the orchestrator computes from the task description prefix.
func TestReviewerSystemPromptAuditModeConditional(t *testing.T) {
	normal := roSystemPrompt(false)
	audit := roSystemPrompt(true)

	// AUDIT-ONLY content must NOT be in the normal-mode prompt.
	for _, marker := range []string{
		"AUDIT-ONLY MODE",
		"AUDIT PERSONA ROTATION",
		"security-OWASP",
		"CHECKLIST DISCIPLINE",
	} {
		if strings.Contains(normal, marker) {
			t.Errorf("auditMode=false prompt should NOT contain %q (~70-line context burn for normal tasks)", marker)
		}
		if !strings.Contains(audit, marker) {
			t.Errorf("auditMode=true prompt MUST contain %q", marker)
		}
	}
	// audit-mode prompt strictly extends normal prompt.
	if !strings.HasPrefix(audit, normal) {
		t.Error("auditMode=true prompt must extend (not replace) the normal prompt")
	}
	// Sanity: audit-mode addition is meaningfully large.
	if len(audit) <= len(normal)+500 {
		t.Errorf("audit-mode addition is suspiciously small: normal=%d audit=%d", len(normal), len(audit))
	}
}
