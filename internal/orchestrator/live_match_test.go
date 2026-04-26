package orchestrator

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aegis/agent-router/internal/plan"
	"github.com/aegis/agent-router/internal/repoconfig"
	"github.com/aegis/agent-router/internal/taskstore"
	"github.com/aegis/agent-router/internal/toolbox"
	"github.com/aegis/agent-router/internal/trace"
)

// TestAutoExtractAndCommitTicksPlan exercises the FULL auto-extract path
// end-to-end: a real toolbox rooted at a tempdir, a real runState with a
// parsed plan, a real trace writer, and the actual autoExtractAndCommit
// function. Asserts that:
//  1. The file lands on disk under the tempdir.
//  2. Plan.Phases[0].Files[0].Satisfied flips to true.
//  3. The commit summary string contains AUTO-COMMITTED.
//  4. The trace file contains a checklist_ticked info event with n>=1
//     and source="auto_extract".
//
// Originally this file was named TestLiveCheckListMatchPackageJson and
// claimed to verify the orchestrator path, but its body bypassed
// autoExtractAndCommit by inlining extractFileBlocks + MarkFileWritten.
// The coverage audit caught that drift -- this test replaces the inline
// reproduction with a real integration test.
func TestAutoExtractAndCommitTicksPlan(t *testing.T) {
	repoRoot := t.TempDir()

	// Make the tempdir look like a git-managed working tree just enough
	// for the toolbox not to choke. FSWrite does not require git itself,
	// just a real directory.
	tb, err := toolbox.New(repoRoot, "test-branch", t.TempDir(), repoconfig.Config{}, false)
	if err != nil {
		t.Fatalf("toolbox.New: %v", err)
	}

	// Force the trace writer's home directory at a tempdir so the test
	// produces an isolated trace file we can read back.
	t.Setenv("HOME", t.TempDir())

	tw, err := trace.Open("test-extract-task")
	if err != nil {
		t.Fatalf("trace.Open: %v", err)
	}
	defer tw.Close()

	planJSON := `{
		"phases": [{
			"id": "P1",
			"name": "Scaffold",
			"files": [
				{"path": "package.json"},
				{"path": "src/server.ts"}
			]
		}],
		"acceptance": []
	}`
	p, err := plan.Parse("```json\n" + planJSON + "\n```")
	if err != nil || p == nil {
		t.Fatalf("plan.Parse: %v", err)
	}

	st := &runState{
		task:  &taskstore.Task{ID: "test-extract-task", RepoRoot: repoRoot},
		trace: tw,
		plan:  p,
	}

	coderReply := "Here are the files:\n\n" +
		"```json\n" +
		"# file: package.json\n" +
		"{\n  \"name\": \"traceforge\"\n}\n" +
		"```\n\n" +
		"```typescript\n" +
		"# file: src/server.ts\n" +
		"console.log('hi');\n" +
		"```\n"

	summary := autoExtractAndCommit(tb, coderReply, st)

	// --- Assertion 1: AUTO-COMMITTED summary ---
	if !strings.Contains(summary, "AUTO-COMMITTED 2 file(s)") {
		t.Errorf("summary: expected AUTO-COMMITTED 2 file(s), got: %q", summary)
	}

	// --- Assertion 2: files on disk ---
	for _, rel := range []string{"package.json", "src/server.ts"} {
		full := filepath.Join(repoRoot, rel)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected %s on disk: %v", rel, err)
		}
	}

	// --- Assertion 3: Plan.Satisfied flipped ---
	if !p.Phases[0].Files[0].Satisfied {
		t.Error("package.json should be ticked")
	}
	if !p.Phases[0].Files[1].Satisfied {
		t.Error("src/server.ts should be ticked")
	}

	// --- Assertion 4: trace events ---
	// Force any buffered trace IO to disk before we read the file.
	tw.Close()
	tracePath := tw.Path()
	f, err := os.Open(tracePath)
	if err != nil {
		t.Fatalf("open trace file: %v", err)
	}
	defer f.Close()

	tickEvents := 0
	autoExtractEvents := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var e map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		data, _ := e["data"].(map[string]any)
		if data == nil {
			continue
		}
		if _, ok := data["checklist_ticked"]; ok {
			tickEvents++
			if data["source"] == "auto_extract" {
				autoExtractEvents++
			}
		}
	}
	if tickEvents < 2 {
		t.Errorf("expected at least 2 checklist_ticked trace events, got %d", tickEvents)
	}
	if autoExtractEvents < 2 {
		t.Errorf("expected at least 2 events with source=auto_extract, got %d", autoExtractEvents)
	}
}

// TestAutoExtractAndCommitWithoutPlanIsSafe verifies that autoExtractAndCommit
// works (writes files, returns summary) even when st.plan is nil. Pre-fix,
// the trace emit guard was `st != nil` but blindly indirected through
// st.plan; with a nil plan the function still must not panic.
func TestAutoExtractAndCommitWithoutPlanIsSafe(t *testing.T) {
	repoRoot := t.TempDir()
	tb, err := toolbox.New(repoRoot, "test-branch", t.TempDir(), repoconfig.Config{}, false)
	if err != nil {
		t.Fatalf("toolbox.New: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	tw, err := trace.Open("test-no-plan")
	if err != nil {
		t.Fatalf("trace.Open: %v", err)
	}
	defer tw.Close()

	st := &runState{
		task:  &taskstore.Task{ID: "test-no-plan", RepoRoot: repoRoot},
		trace: tw,
		plan:  nil,
	}

	coderReply := "```\n# file: foo.txt\nhello\n```\n"
	summary := autoExtractAndCommit(tb, coderReply, st)
	if !strings.Contains(summary, "AUTO-COMMITTED 1 file") {
		t.Errorf("summary: %q", summary)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "foo.txt")); err != nil {
		t.Errorf("expected foo.txt on disk: %v", err)
	}
}
