package orchestrator

import "testing"

func TestExtractToolCallShorthand(t *testing.T) {
	// Exact shape Gemma emitted during Observatory task T193358
	input := `<TOOL>ask_coder args: {"instruction": "Initialize the project structure for Phase 1. \n\n1. Create src/", "plan": "Init"}</TOOL>`
	tc, ok := extractToolCall(input)
	if !ok {
		t.Fatal("failed to extract shorthand tool call")
	}
	if tc.Name != "ask_coder" {
		t.Errorf("name = %q, want ask_coder", tc.Name)
	}
	if tc.Args["instruction"] == nil {
		t.Errorf("instruction arg missing; got args=%v", tc.Args)
	}
}

func TestExtractToolCallStillHandlesStrictForm(t *testing.T) {
	input := `<TOOL>{"name":"fs.write","args":{"path":"x","content":"y"}}</TOOL>`
	tc, ok := extractToolCall(input)
	if !ok {
		t.Fatal("failed strict form")
	}
	if tc.Name != "fs.write" {
		t.Errorf("name = %q, want fs.write", tc.Name)
	}
}

func TestExtractToolCallShorthandMultiline(t *testing.T) {
	input := "<TOOL>pensive.search args: {\n  \"query\": \"router-observatory\",\n  \"k\": 5\n}</TOOL>"
	tc, ok := extractToolCall(input)
	if !ok {
		t.Fatal("failed multiline shorthand")
	}
	if tc.Name != "pensive.search" {
		t.Errorf("name = %q", tc.Name)
	}
	if tc.Args["query"] != "router-observatory" {
		t.Errorf("args wrong: %v", tc.Args)
	}
}
