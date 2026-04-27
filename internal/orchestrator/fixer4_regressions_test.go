package orchestrator

// Regression tests for FIXER-4 audit fixes (pass 4):
//   STATE-FUZZ-001: fs.write must reject calls that omit the content key.
//   STATE-FUZZ-002: done() must reject calls before any work has happened.
//   STATE-FUZZ-004: bare-JSON fallback parser must not execute prose.
//   C10:            multiple <TOOL> blocks in one response are rejected.
//   misc:           fs.search empty pattern, fs.read on directory, fs.write
//                   size cap, fail() empty reason sanitize.
//
// These tests run with -race -count=N and exercise the orchestrator code
// directly (no llama-server, no real toolbox where avoidable).

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aegis/moirai/internal/repoconfig"
	"github.com/aegis/moirai/internal/taskstore"
	"github.com/aegis/moirai/internal/toolbox"
	"github.com/aegis/moirai/internal/trace"
)

// ----- STATE-FUZZ-002: done() before any work -----------------------------

func TestDoneBeforeWorkRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))

	// Stub that emits done() on the first turn, then a sane fail() on later
	// turns so the loop terminates predictably for the assertion.
	stub := &stubModelMgr{
		responses: []string{
			`<TOOL>{"name":"done","args":{"summary":"victory"}}</TOOL>`,
			`<TOOL>{"name":"fail","args":{"reason":"giving up"}}</TOOL>`,
		},
	}
	o, err := New(Config{Store: store, ModelMgr: stub})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id := "fxr4-done-" + newTaskID()
	task := &taskstore.Task{ID: id, Status: taskstore.StatusRunning, RepoRoot: t.TempDir()}
	_ = store.Save(task)
	tr, _ := trace.Open(id)
	defer tr.Close()

	st := &runState{cancel: func() {}, task: task, trace: tr}
	// workOps starts at 0; the done() call on turn 1 must be rejected and
	// the loop continues to turn 2 where fail() lands.
	summary, ok, runErr := o.roLoop(context.Background(), st, nil)
	if runErr != nil {
		t.Fatalf("roLoop: %v", runErr)
	}
	if ok {
		t.Fatalf("expected ok=false (rejected done -> fail), got summary=%q", summary)
	}
	if !strings.Contains(summary, "giving up") {
		t.Errorf("expected fail reason in summary, got %q", summary)
	}
	// Confirm we got at least 2 turns (i.e. done() was rejected, fail() ran).
	if st.roTurns < 2 {
		t.Errorf("expected >=2 turns, got %d", st.roTurns)
	}
}

// ----- STATE-FUZZ-001: fs.write missing content key ------------------------

func TestFSWriteRejectsMissingContent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	o, _ := New(Config{Store: store, ModelMgr: &stubModelMgr{}})

	tmpRepo := t.TempDir()
	rcfg, _, _ := repoconfig.Load(tmpRepo)
	tb, err := toolbox.New(tmpRepo, "test-branch", t.TempDir(), rcfg, false)
	if err != nil {
		t.Fatalf("toolbox.New: %v", err)
	}

	id := "fxr4-fsw-" + newTaskID()
	task := &taskstore.Task{ID: id, Status: taskstore.StatusRunning, RepoRoot: tmpRepo}
	tr, _ := trace.Open(id)
	defer tr.Close()
	st := &runState{cancel: func() {}, task: task, trace: tr}

	// Args has "path" but no "content" key at all -- must be rejected.
	tc := toolCall{Name: "fs.write", Args: map[string]any{"path": "x.txt"}}
	_, err = o.executeROTool(context.Background(), st, tb, tc)
	if err == nil {
		t.Fatal("expected error when content key absent, got nil")
	}
	if !strings.Contains(err.Error(), "missing required arg: content") {
		t.Errorf("expected 'missing required arg: content', got %v", err)
	}

	// Explicit empty-string content must succeed (deliberate truncation
	// of an existing file is a legitimate operation).
	tc2 := toolCall{Name: "fs.write", Args: map[string]any{"path": "x.txt", "content": ""}}
	if _, err := o.executeROTool(context.Background(), st, tb, tc2); err != nil {
		t.Errorf("expected empty-content write to succeed, got %v", err)
	}

	// SEC-PASS5-003: explicit JSON null for content. The key IS present in
	// the args map (so argHas returns true) but the type assertion in
	// argStr fails and we get the empty string. Today's contract: this
	// succeeds and writes an empty file -- same as the explicit "" case.
	// Pin the contract via a test so a future tightening of argHas (e.g.
	// rejecting explicit null) does not silently regress in either
	// direction.
	tc3 := toolCall{Name: "fs.write", Args: map[string]any{"path": "x.txt", "content": nil}}
	if _, err := o.executeROTool(context.Background(), st, tb, tc3); err != nil {
		t.Errorf("expected JSON-null content to be treated as empty (current contract), got %v", err)
	}
}

// ----- STATE-FUZZ-004: bare-JSON fallback should not execute prose --------

func TestExtractToolCallProseNotExecuted(t *testing.T) {
	// Prose containing a JSON-shaped object MUST NOT be parsed as a tool
	// call. The fallback now requires the response to start with "{".
	prose := `I am thinking about whether to call { "name": "fail", "args": {"reason": "give up"} }
but actually I should keep working.`
	tc, ok := extractToolCall(prose)
	if ok {
		t.Errorf("prose with embedded JSON should NOT match: got %+v", tc)
	}
}

func TestExtractToolCallBareJSONOK(t *testing.T) {
	// A response that IS a bare JSON object at the start should still match
	// (legacy behaviour for models that omit the <TOOL> wrapper).
	bare := `   {"name":"done","args":{"summary":"all good"}}`
	tc, ok := extractToolCall(bare)
	if !ok || tc.Name != "done" {
		t.Errorf("expected bare JSON to match name=done, got ok=%v tc=%+v", ok, tc)
	}
}

func TestExtractToolCallTaggedStillWorks(t *testing.T) {
	// Strict <TOOL> form must continue to work.
	wrapped := `Thinking... <TOOL>{"name":"fs.read","args":{"path":"go.mod"}}</TOOL>`
	tc, ok := extractToolCall(wrapped)
	if !ok || tc.Name != "fs.read" {
		t.Errorf("expected tagged form to match, got ok=%v tc=%+v", ok, tc)
	}
}

// ----- C10: multiple <TOOL> blocks in one response -----------------------

func TestExtractToolCallMultipleBlocksRejected(t *testing.T) {
	dual := `<TOOL>{"name":"fs.read","args":{"path":"a"}}</TOOL>
<TOOL>{"name":"fs.read","args":{"path":"b"}}</TOOL>`
	_, ok, err := extractToolCallChecked(dual)
	if ok {
		t.Errorf("expected ok=false for multiple TOOL blocks")
	}
	if !errors.Is(err, ErrMultipleToolCalls) {
		t.Errorf("expected ErrMultipleToolCalls, got %v", err)
	}
}

// ----- fs.search empty pattern -----------------------------------------

func TestFSSearchEmptyPatternRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	o, _ := New(Config{Store: store, ModelMgr: &stubModelMgr{}})

	tmpRepo := t.TempDir()
	rcfg, _, _ := repoconfig.Load(tmpRepo)
	tb, _ := toolbox.New(tmpRepo, "test-branch", t.TempDir(), rcfg, false)

	id := "fxr4-search-" + newTaskID()
	task := &taskstore.Task{ID: id, Status: taskstore.StatusRunning, RepoRoot: tmpRepo}
	tr, _ := trace.Open(id)
	defer tr.Close()
	st := &runState{cancel: func() {}, task: task, trace: tr}

	for _, pat := range []string{"", "   ", "\t\n"} {
		tc := toolCall{Name: "fs.search", Args: map[string]any{"pattern": pat}}
		_, err := o.executeROTool(context.Background(), st, tb, tc)
		if err == nil {
			t.Errorf("expected error for empty/whitespace pattern %q", pat)
		}
	}
}

// ----- fs.read on directory ---------------------------------------------

func TestFSReadOnDirectoryRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	o, _ := New(Config{Store: store, ModelMgr: &stubModelMgr{}})

	tmpRepo := t.TempDir()
	rcfg, _, _ := repoconfig.Load(tmpRepo)
	tb, _ := toolbox.New(tmpRepo, "test-branch", t.TempDir(), rcfg, false)

	id := "fxr4-readdir-" + newTaskID()
	task := &taskstore.Task{ID: id, Status: taskstore.StatusRunning, RepoRoot: tmpRepo}
	tr, _ := trace.Open(id)
	defer tr.Close()
	st := &runState{cancel: func() {}, task: task, trace: tr}

	// repo root itself is a directory; fs.read("") should bottom out there.
	tc := toolCall{Name: "fs.read", Args: map[string]any{"path": "."}}
	_, err := o.executeROTool(context.Background(), st, tb, tc)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("expected 'is a directory' error, got %v", err)
	}
}

// ----- fs.write 4MB cap --------------------------------------------------

func TestFSWriteSizeCap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	o, _ := New(Config{Store: store, ModelMgr: &stubModelMgr{}})

	tmpRepo := t.TempDir()
	rcfg, _, _ := repoconfig.Load(tmpRepo)
	tb, _ := toolbox.New(tmpRepo, "test-branch", t.TempDir(), rcfg, false)

	id := "fxr4-cap-" + newTaskID()
	task := &taskstore.Task{ID: id, Status: taskstore.StatusRunning, RepoRoot: tmpRepo}
	tr, _ := trace.Open(id)
	defer tr.Close()
	st := &runState{cancel: func() {}, task: task, trace: tr}

	huge := strings.Repeat("A", fsWriteMaxBytes+1)
	tc := toolCall{Name: "fs.write", Args: map[string]any{"path": "huge.txt", "content": huge}}
	_, err := o.executeROTool(context.Background(), st, tb, tc)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("expected size-cap error, got %v", err)
	}
}

// ----- fail() empty reason sanitize -------------------------------------

func TestFailEmptyReasonSanitized(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	stub := &stubModelMgr{
		responses: []string{
			`<TOOL>{"name":"fail","args":{"reason":""}}</TOOL>`,
		},
	}
	o, _ := New(Config{Store: store, ModelMgr: stub})

	id := "fxr4-fail-" + newTaskID()
	task := &taskstore.Task{ID: id, Status: taskstore.StatusRunning, RepoRoot: t.TempDir()}
	_ = store.Save(task)
	tr, _ := trace.Open(id)
	defer tr.Close()
	st := &runState{cancel: func() {}, task: task, trace: tr, workOps: 1}

	summary, ok, _ := o.roLoop(context.Background(), st, nil)
	if ok {
		t.Fatal("expected ok=false")
	}
	if summary == "" || strings.HasSuffix(summary, ":") {
		t.Errorf("expected sanitized non-empty reason, got %q", summary)
	}
	if !strings.Contains(summary, "no reason provided") {
		t.Errorf("expected 'no reason provided' marker, got %q", summary)
	}
}
