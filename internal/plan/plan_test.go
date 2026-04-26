package plan

import (
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
