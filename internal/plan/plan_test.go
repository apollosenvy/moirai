package plan

import (
	"fmt"
	"strings"
	"testing"
)

const samplePlannerReply = `Here is the build plan for TraceForge.

` + "```" + `json
{
  "phases": [
    {
      "id": "P1",
      "name": "Scaffold",
      "files": [
        {"path": "package.json", "purpose": "workspace root"},
        {"path": "tsconfig.json"},
        {"path": "shared/src/types.ts", "purpose": "cross-package types"}
      ]
    },
    {
      "id": "P2",
      "name": "Backend core",
      "files": [
        {"path": "backend/src/server.ts"},
        {"path": "backend/src/db/schema.ts"}
      ]
    }
  ],
  "acceptance": [
    {"id": "A1", "description": "npm install works", "verify": ""},
    {"id": "A2", "description": "tsc --noEmit passes", "verify": "compile.run:pass"},
    {"id": "A3", "description": "fixtures present", "verify": "file:fixtures/sample.jsonl"}
  ]
}
` + "```"

func TestParseValidPlan(t *testing.T) {
	p, err := Parse(samplePlannerReply)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p == nil {
		t.Fatal("Parse returned nil plan")
	}
	if len(p.Phases) != 2 {
		t.Errorf("phases: got %d, want 2", len(p.Phases))
	}
	if p.Phases[0].ID != "P1" {
		t.Errorf("phase 0 ID: %q, want P1", p.Phases[0].ID)
	}
	if len(p.Phases[0].Files) != 3 {
		t.Errorf("phase 0 files: got %d, want 3", len(p.Phases[0].Files))
	}
	if len(p.Acceptance) != 3 {
		t.Errorf("acceptance: got %d, want 3", len(p.Acceptance))
	}
	if p.Acceptance[1].Verify != "compile.run:pass" {
		t.Errorf("A2 verify: %q", p.Acceptance[1].Verify)
	}
}

func TestParseEmptyReply(t *testing.T) {
	p, err := Parse("")
	if err != nil {
		t.Errorf("Parse(\"\") err: %v", err)
	}
	if p != nil {
		t.Errorf("Parse(\"\") returned non-nil plan")
	}
}

func TestParseProseOnlyNoJSON(t *testing.T) {
	p, err := Parse("Here is my plan in prose. No JSON.")
	if err != nil {
		t.Errorf("err: %v", err)
	}
	if p != nil {
		t.Error("expected nil plan for prose-only reply")
	}
}

func TestParseMalformedJSON(t *testing.T) {
	reply := "```json\n{phases: not-json}\n```"
	p, err := Parse(reply)
	if err == nil {
		t.Error("expected error on malformed JSON")
	}
	if p != nil {
		t.Error("expected nil plan on malformed JSON")
	}
}

func TestParseUnfencedJSON(t *testing.T) {
	// Some reasoning models drop the fence. We accept the trailing
	// balanced JSON.
	reply := `Here's the plan. {"phases":[{"id":"P1","name":"x","files":[{"path":"a.ts"}]}],"acceptance":[]}`
	p, err := Parse(reply)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil plan")
	}
	if len(p.Phases) != 1 {
		t.Fatalf("phases: %d", len(p.Phases))
	}
}

func TestParsePicksLastJSONBlock(t *testing.T) {
	// First block is an example; second is the real plan.
	reply := "Example: ```json\n{\"phases\":[],\"acceptance\":[]}\n```\n\n" +
		"Real plan: ```json\n{\"phases\":[{\"id\":\"P1\",\"name\":\"real\",\"files\":[{\"path\":\"x.ts\"}]}],\"acceptance\":[]}\n```"
	p, err := Parse(reply)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Phases) != 1 || p.Phases[0].Name != "real" {
		t.Errorf("expected real plan, got %+v", p)
	}
}

func TestParseRejectsEmptyJSON(t *testing.T) {
	// Both phases AND acceptance empty -> reject as not a usable plan.
	reply := "```json\n{\"phases\":[],\"acceptance\":[]}\n```"
	p, err := Parse(reply)
	if err == nil {
		t.Error("expected error on empty plan")
	}
	if p != nil {
		t.Error("expected nil plan on empty content")
	}
}

func TestMarkFileWritten(t *testing.T) {
	p, _ := Parse(samplePlannerReply)
	if p == nil {
		t.Fatal("plan parse failed")
	}
	if n := p.MarkFileWritten("package.json"); n != 1 {
		t.Errorf("MarkFileWritten(package.json): n=%d", n)
	}
	if !p.Phases[0].Files[0].Satisfied {
		t.Error("package.json should be satisfied")
	}
	// idempotent
	if n := p.MarkFileWritten("package.json"); n != 0 {
		t.Errorf("re-marking should yield 0 ticks, got %d", n)
	}
	// normalization: ./ prefix
	p.MarkFileWritten("./tsconfig.json")
	if !p.Phases[0].Files[1].Satisfied {
		t.Error("tsconfig.json should be satisfied with ./ prefix")
	}
}

func TestMarkFileWrittenTicksFileVerifyAcceptance(t *testing.T) {
	reply := "```json\n" +
		`{"phases":[{"id":"P1","name":"x","files":[]}],` +
		`"acceptance":[{"id":"A1","description":"fixture","verify":"file:fixtures/sample.jsonl"}]}` +
		"\n```"
	p, _ := Parse(reply)
	if p == nil {
		t.Fatal("parse")
	}
	p.MarkFileWritten("fixtures/sample.jsonl")
	if !p.Acceptance[0].Satisfied {
		t.Error("file: acceptance criterion should be satisfied by matching fs.write")
	}
}

func TestMarkAcceptanceByVerifyKey(t *testing.T) {
	p, _ := Parse(samplePlannerReply)
	if p == nil {
		t.Fatal("parse")
	}
	if n := p.MarkAcceptance("compile.run:pass"); n != 1 {
		t.Errorf("MarkAcceptance compile.run:pass: n=%d", n)
	}
	if !p.Acceptance[1].Satisfied {
		t.Error("A2 should be satisfied")
	}
}

func TestUnsatisfiedAcceptance(t *testing.T) {
	p, _ := Parse(samplePlannerReply)
	if p == nil {
		t.Fatal("parse")
	}
	p.MarkAcceptance("compile.run:pass")
	un := p.UnsatisfiedAcceptance()
	if len(un) != 2 {
		t.Errorf("unsatisfied: got %d, want 2", len(un))
	}
}

func TestMarkFileSuffixUniqueMatch(t *testing.T) {
	// Planner says "web/package.json" but coder writes "apps/web/package.json".
	// With only ONE unticked FileSpec ending in web/package.json, suffix
	// fallback should tick it.
	reply := "```json\n" + `{"phases":[{"id":"P1","name":"x","files":[
		{"path":"web/package.json"},
		{"path":"apps/server/src/main.ts"}
	]}],"acceptance":[]}` + "\n```"
	p, _ := Parse(reply)
	if p == nil {
		t.Fatal("parse")
	}
	if n := p.MarkFileWritten("apps/web/package.json"); n != 1 {
		t.Errorf("suffix match: n=%d, want 1", n)
	}
	if !p.Phases[0].Files[0].Satisfied {
		t.Error("web/package.json should be ticked via suffix")
	}
}

func TestMarkFileSuffixAmbiguousNoMatch(t *testing.T) {
	// Two FileSpecs both ending in package.json -- suffix match is ambiguous,
	// must NOT tick either.
	reply := "```json\n" + `{"phases":[{"id":"P1","name":"x","files":[
		{"path":"apps/web/package.json"},
		{"path":"apps/server/package.json"}
	]}],"acceptance":[]}` + "\n```"
	p, _ := Parse(reply)
	if p == nil {
		t.Fatal("parse")
	}
	// Coder wrote bare "package.json" -- ambiguous against the two specs.
	if n := p.MarkFileWritten("package.json"); n != 0 {
		t.Errorf("ambiguous suffix: n=%d, want 0 (refuse rather than tick wrong)", n)
	}
	if p.Phases[0].Files[0].Satisfied || p.Phases[0].Files[1].Satisfied {
		t.Error("neither should be ticked on ambiguous suffix")
	}
}

func TestMarkFileSuffixDoesNotShadowExact(t *testing.T) {
	// Exact match takes priority. Suffix fallback should not run when the
	// exact match already ticked something.
	reply := "```json\n" + `{"phases":[{"id":"P1","name":"x","files":[
		{"path":"package.json"},
		{"path":"apps/web/package.json"}
	]}],"acceptance":[]}` + "\n```"
	p, _ := Parse(reply)
	if p == nil {
		t.Fatal("parse")
	}
	// Coder wrote "package.json" exactly. Should hit FIRST FileSpec only.
	if n := p.MarkFileWritten("package.json"); n != 1 {
		t.Errorf("exact match: n=%d, want 1", n)
	}
	if !p.Phases[0].Files[0].Satisfied {
		t.Error("root package.json should be ticked")
	}
	if p.Phases[0].Files[1].Satisfied {
		t.Error("apps/web/package.json should NOT be ticked when exact matches root")
	}
}

func TestPathSegmentSuffixRejectsSubstring(t *testing.T) {
	// "ackage.json" must NOT be considered a suffix of "package.json" -- we
	// require segment alignment.
	if pathSegmentSuffix("package.json", "ackage.json") {
		t.Error("substring suffix should be rejected (need segment alignment)")
	}
	// Empty cases.
	if pathSegmentSuffix("", "x") || pathSegmentSuffix("x", "") {
		t.Error("empty strings should never match")
	}
	// Identical -- suffix relation is strict (caller handles equality).
	if pathSegmentSuffix("a/b", "a/b") {
		t.Error("equal paths should not be a strict suffix")
	}
	// Real positive case.
	if !pathSegmentSuffix("apps/web/package.json", "web/package.json") {
		t.Error("web/package.json is a segment-suffix of apps/web/package.json")
	}
	// b longer than a -- rejected.
	if pathSegmentSuffix("a/b", "x/a/b") {
		t.Error("b longer than a should be rejected")
	}
}

func TestParseRejectsAbsolutePathInFileSpec(t *testing.T) {
	// /etc/passwd as a FileSpec.path -- must be silently filtered out.
	reply := "```json\n" + `{"phases":[{"id":"P1","name":"x","files":[
		{"path":"/etc/passwd"},
		{"path":"src/safe.go"}
	]}],"acceptance":[]}` + "\n```"
	p, err := Parse(reply)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Phases[0].Files) != 1 {
		t.Errorf("absolute path should be filtered: got %d files, want 1", len(p.Phases[0].Files))
	}
	if p.Phases[0].Files[0].Path != "src/safe.go" {
		t.Errorf("survivor: %q, want src/safe.go", p.Phases[0].Files[0].Path)
	}
}

func TestParseRejectsTraversalPathInFileSpec(t *testing.T) {
	reply := "```json\n" + `{"phases":[{"id":"P1","name":"x","files":[
		{"path":"../escape.go"},
		{"path":"a/../b.go"},
		{"path":"src/keep.go"}
	]}],"acceptance":[]}` + "\n```"
	p, _ := Parse(reply)
	if p == nil {
		t.Fatal("parse returned nil")
	}
	if len(p.Phases[0].Files) != 1 || p.Phases[0].Files[0].Path != "src/keep.go" {
		t.Errorf("traversal should be filtered, only src/keep.go survives: %+v", p.Phases[0].Files)
	}
}

func TestParseRejectsControlCharsInFileSpec(t *testing.T) {
	reply := "```json\n" + `{"phases":[{"id":"P1","name":"x","files":[
		{"path":"foo\nbar.go"},
		{"path":"a\u0000b.go"},
		{"path":"src/clean.go"}
	]}],"acceptance":[]}` + "\n```"
	p, _ := Parse(reply)
	if p == nil {
		t.Fatal("parse returned nil")
	}
	if len(p.Phases[0].Files) != 1 || p.Phases[0].Files[0].Path != "src/clean.go" {
		t.Errorf("control-char paths should be filtered: %+v", p.Phases[0].Files)
	}
}

func TestParseRejectsEmptyFileVerifyTarget(t *testing.T) {
	// verify:"file:" with empty path after the prefix must NOT install an
	// acceptance item -- it would tick on any empty MarkFileWritten.
	reply := "```json\n" + `{"phases":[],"acceptance":[
		{"id":"A1","description":"typo","verify":"file:"},
		{"id":"A2","description":"good","verify":"file:fixtures/sample.jsonl"},
		{"id":"A3","description":"manual","verify":""}
	]}` + "\n```"
	p, _ := Parse(reply)
	if p == nil {
		t.Fatal("parse returned nil")
	}
	if len(p.Acceptance) != 2 {
		t.Errorf("empty-target verify should be filtered: got %d, want 2", len(p.Acceptance))
	}
	for _, a := range p.Acceptance {
		if a.Verify == "file:" {
			t.Error("empty file: verify should be removed")
		}
	}
}

func TestParseRejectsAbsolutePathInVerify(t *testing.T) {
	// verify:"file:/etc/passwd" must be filtered. Otherwise suffix-uniqueness
	// could tick it on any matching tail of the absolute path.
	reply := "```json\n" + `{"phases":[],"acceptance":[
		{"id":"A1","description":"bad","verify":"file:/etc/passwd"},
		{"id":"A2","description":"good","verify":"file:fixtures/x.jsonl"}
	]}` + "\n```"
	p, _ := Parse(reply)
	if p == nil {
		t.Fatal("parse returned nil")
	}
	if len(p.Acceptance) != 1 || p.Acceptance[0].ID != "A2" {
		t.Errorf("absolute-path verify should be filtered: %+v", p.Acceptance)
	}
}

func TestParseRejectsUnknownVerifyShape(t *testing.T) {
	// "test.run:fail" / "compile.run:warn" / "foo:bar" are typos and must
	// be filtered. Otherwise done() is silently blocked forever.
	reply := "```json\n" + `{"phases":[],"acceptance":[
		{"id":"A1","description":"typo1","verify":"test.run:fail"},
		{"id":"A2","description":"typo2","verify":"compile.run:warn"},
		{"id":"A3","description":"typo3","verify":"FILE:foo.go"},
		{"id":"A4","description":"good","verify":"test.run:pass"}
	]}` + "\n```"
	p, _ := Parse(reply)
	if p == nil {
		t.Fatal("parse returned nil")
	}
	if len(p.Acceptance) != 1 || p.Acceptance[0].ID != "A4" {
		t.Errorf("only test.run:pass should survive; got %+v", p.Acceptance)
	}
}

func TestParseRejectsAllEntriesYieldsError(t *testing.T) {
	// Every FileSpec / Acceptance is malformed -- post-filter both lists
	// are empty, so Parse returns an error rather than installing a
	// no-op plan that bypasses the done() gate.
	reply := "```json\n" + `{"phases":[{"id":"P1","name":"x","files":[
		{"path":"/abs/foo"}
	]}],"acceptance":[
		{"id":"A1","description":"junk","verify":"file:"}
	]}` + "\n```"
	p, err := Parse(reply)
	if err == nil {
		t.Error("expected parse error when all entries are filtered")
	}
	if p != nil {
		t.Error("expected nil plan when every entry is rejected")
	}
}

func TestProgressCounts(t *testing.T) {
	p, _ := Parse(samplePlannerReply)
	if p == nil {
		t.Fatal("parse")
	}
	fd, ft, ad, at := p.ProgressCounts()
	if fd != 0 || ad != 0 {
		t.Errorf("fresh plan: fd=%d ad=%d, want 0,0", fd, ad)
	}
	if ft != 5 || at != 3 {
		t.Errorf("fresh plan totals: ft=%d at=%d, want 5,3", ft, at)
	}
	p.MarkFileWritten("package.json")
	p.MarkFileWritten("backend/src/server.ts")
	p.MarkAcceptance("compile.run:pass")
	fd, ft, ad, at = p.ProgressCounts()
	if fd != 2 || ad != 1 {
		t.Errorf("after ticks: fd=%d ad=%d, want 2,1", fd, ad)
	}
	if ft != 5 || at != 3 {
		t.Errorf("totals shouldn't change: ft=%d at=%d", ft, at)
	}
	// Nil receiver returns zeros, doesn't panic.
	var nilP *Plan
	fd, ft, ad, at = nilP.ProgressCounts()
	if fd != 0 || ft != 0 || ad != 0 || at != 0 {
		t.Errorf("nil plan should return all zeros")
	}
}

func TestRenderChecklistCompactModeForLargePlans(t *testing.T) {
	// Build a plan with > renderCompactThreshold files; verify compact
	// rendering kicks in (no "-- purpose" lines, completed phases collapse).
	var phases []Phase
	// One phase with 60 files, all unsatisfied.
	var files []FileSpec
	for i := 0; i < 60; i++ {
		files = append(files, FileSpec{
			Path:    fmt.Sprintf("src/file%d.ts", i),
			Purpose: "should be hidden in compact mode",
		})
	}
	phases = append(phases, Phase{ID: "P1", Name: "Big phase", Files: files})
	p := &Plan{Phases: phases}

	out := p.RenderChecklist()
	if strings.Contains(out, "should be hidden in compact mode") {
		t.Errorf("compact mode should drop Purpose comments")
	}
	if !strings.Contains(out, "Phase P1 -- Big phase (0/60)") {
		t.Errorf("compact mode should include phase progress fraction: %q", out[:200])
	}
	// All files unticked, so phase shouldn't be collapsed.
	if strings.Contains(out, "(0/60) [x] done") {
		t.Error("phase with no ticks should not be collapsed to 'done'")
	}
}

func TestRenderChecklistCompactCollapsesDonePhases(t *testing.T) {
	// Plan with > threshold files; one phase fully done. Compact mode should
	// collapse the done phase to a single line and show the unfinished phase
	// with file detail.
	var p1files, p2files []FileSpec
	for i := 0; i < 30; i++ {
		p1files = append(p1files, FileSpec{Path: fmt.Sprintf("p1/f%d.ts", i)})
	}
	for i := 0; i < 30; i++ {
		p2files = append(p2files, FileSpec{Path: fmt.Sprintf("p2/f%d.ts", i)})
	}
	p := &Plan{
		Phases: []Phase{
			{ID: "P1", Name: "Done phase", Files: p1files},
			{ID: "P2", Name: "Active phase", Files: p2files},
		},
	}
	// Mark every P1 file satisfied.
	for i := range p.Phases[0].Files {
		p.Phases[0].Files[i].Satisfied = true
	}

	out := p.RenderChecklist()
	if !strings.Contains(out, "Phase P1 -- Done phase (30/30) [x] done") {
		t.Errorf("done phase should collapse: %q", out)
	}
	// Done phase files should NOT appear individually in compact mode.
	if strings.Contains(out, "[x] p1/f0.ts") {
		t.Error("done phase files should NOT be listed individually in compact mode")
	}
	// Active phase files SHOULD appear.
	if !strings.Contains(out, "[ ] p2/f0.ts") {
		t.Error("active phase files should be listed")
	}
}

func TestRenderChecklistFullModeForSmallPlans(t *testing.T) {
	// 3-file plan stays in full mode -- Purpose comment must be visible.
	p := &Plan{Phases: []Phase{{
		ID:   "P1",
		Name: "Tiny",
		Files: []FileSpec{
			{Path: "a.ts", Purpose: "should be visible"},
			{Path: "b.ts"},
		},
	}}}
	out := p.RenderChecklist()
	if !strings.Contains(out, "a.ts -- should be visible") {
		t.Errorf("full mode should include Purpose: %q", out)
	}
}

func TestRenderChecklistCompactSavesBytes(t *testing.T) {
	// Generate the same 100-file plan rendered both ways. Compact mode
	// must be substantially smaller.
	var files []FileSpec
	for i := 0; i < 100; i++ {
		files = append(files, FileSpec{
			Path:    fmt.Sprintf("apps/web/src/components/Component%d.tsx", i),
			Purpose: "a component for the timeline view",
		})
	}
	p := &Plan{Phases: []Phase{{ID: "P1", Name: "Components", Files: files}}}
	full := p.renderChecklistFull()
	compact := p.renderChecklistCompact()
	if len(compact) >= len(full) {
		t.Errorf("compact (%d bytes) should be smaller than full (%d bytes)", len(compact), len(full))
	}
	// Sanity: at least 25% reduction.
	if len(compact) > 3*len(full)/4 {
		t.Errorf("compact saved less than 25%%: full=%d compact=%d", len(full), len(compact))
	}
}

func TestRenderChecklistEmptyPlan(t *testing.T) {
	var p *Plan
	if got := p.RenderChecklist(); got != "" {
		t.Errorf("nil plan should render empty, got %q", got)
	}
}

func TestRenderChecklistNonEmpty(t *testing.T) {
	p, _ := Parse(samplePlannerReply)
	if p == nil {
		t.Fatal("parse")
	}
	p.MarkFileWritten("package.json")
	p.MarkAcceptance("compile.run:pass")
	out := p.RenderChecklist()
	if !strings.Contains(out, "<CHECKLIST>") || !strings.Contains(out, "</CHECKLIST>") {
		t.Errorf("checklist missing tags: %q", out)
	}
	if !strings.Contains(out, "[x] package.json") {
		t.Errorf("ticked file not rendered: %q", out)
	}
	if !strings.Contains(out, "[ ] tsconfig.json") {
		t.Errorf("unticked file not rendered: %q", out)
	}
	if !strings.Contains(out, "[x] A2:") {
		t.Errorf("ticked acceptance not rendered: %q", out)
	}
	if !strings.Contains(out, "Progress: 1/5 files, 1/3 acceptance.") {
		t.Errorf("progress summary wrong: %q", out)
	}
}
