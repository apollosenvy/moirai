package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aegis/moirai/internal/modelmgr"
	"github.com/aegis/moirai/internal/taskstore"
)

// failingModelMgr fails EnsureSlot immediately so the run goroutine takes
// the fail() path as fast as possible. This mirrors the production scenario
// where llama_server_bin is /usr/bin/true -- EnsureSlot races the HTTP
// response encoder that tries to Marshal the returned Task.
type failingModelMgr struct {
	calls atomic.Int32
}

func (f *failingModelMgr) EnsureSlot(ctx context.Context, slot modelmgr.Slot) (string, error) {
	f.calls.Add(1)
	return "", errors.New("fake llama-server crashed immediately")
}
func (f *failingModelMgr) Active() modelmgr.Slot { return "" }
func (f *failingModelMgr) Complete(ctx context.Context, req modelmgr.ChatRequest) (string, error) {
	return "", errors.New("no complete path on failingModelMgr")
}

// TestSubmitNoRaceAgainstRunGoroutine drives the exact collision that pass-3
// B1 reports: the HTTP response encoder (here, json.Marshal of the Task
// pointer) reads Task fields at the same instant the run goroutine is
// flipping them through fail(). Before the Clone() fix, go test -race
// -count=5 reliably tripped this. After the fix, the encoder sees the
// snapshot and the run goroutine writes to the live struct; no overlap.
func TestSubmitNoRaceAgainstRunGoroutine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	store, err := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatalf("taskstore: %v", err)
	}
	o, err := New(Config{
		Store:    store,
		ModelMgr: &failingModelMgr{},
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	const rounds = 40
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		task, err := o.Submit(context.Background(), fmt.Sprintf("desc %d", i), home)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		// Immediately serialize the returned task. Before the fix, the run
		// goroutine spawned by Submit would race this encoder on Status and
		// LastError. After the fix, `task` is a clone, so json.Marshal walks
		// a snapshot that no other goroutine has a reference to.
		wg.Add(1)
		go func(tk *taskstore.Task) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, err := json.Marshal(tk); err != nil {
					t.Errorf("marshal: %v", err)
					return
				}
				// Read a few more fields to maximise the window.
				_ = tk.Status
				_ = tk.LastError
				_ = tk.Phase
				_ = tk.UpdatedAt
			}
		}(task)
	}
	wg.Wait()

	// Give any stragglers a beat to flush through fail()'s Save, then bound
	// the test with a soft deadline.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tasks, _ := store.List()
		allSettled := true
		for _, tk := range tasks {
			if tk.Status == taskstore.StatusRunning {
				allSettled = false
				break
			}
		}
		if allSettled {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSubmitReturnsSnapshot asserts that mutating the live task after Submit
// does NOT mutate the returned value. This is the contract that lets the HTTP
// layer write the response without coordination.
func TestSubmitReturnsSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	store, err := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatalf("taskstore: %v", err)
	}
	o, err := New(Config{
		Store:    store,
		ModelMgr: &failingModelMgr{},
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	snap, err := o.Submit(context.Background(), "x", home)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	gotID := snap.ID

	// Wait until the run goroutine has settled the live task to failed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		loaded, err := store.Load(gotID)
		if err == nil && loaded.Status != taskstore.StatusRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The live task (now on disk) should be failed, but the snapshot we
	// returned from Submit should still show running. That's the proof we
	// deep-copied.
	if snap.Status != taskstore.StatusRunning {
		t.Errorf("snapshot Status changed after Submit; got %q, want running", snap.Status)
	}
	loaded, err := store.Load(gotID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Status == taskstore.StatusRunning {
		t.Errorf("live task never transitioned away from running; something is wrong with the test harness")
	}
}

// TestNewValidates covers the B2 fix: missing ModelMgr or Store -> error,
// not a nil-deref in a spawned goroutine. A correct config returns a
// non-nil Orchestrator and a nil error.
func TestNewValidates(t *testing.T) {
	store, err := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatalf("taskstore: %v", err)
	}

	// nil ModelMgr -> error.
	if _, err := New(Config{Store: store}); err == nil {
		t.Errorf("expected error for nil ModelMgr")
	}

	// nil Store -> error.
	if _, err := New(Config{ModelMgr: &failingModelMgr{}}); err == nil {
		t.Errorf("expected error for nil Store")
	}

	// Both nil -> error.
	if _, err := New(Config{}); err == nil {
		t.Errorf("expected error for nil ModelMgr and nil Store")
	}

	// Valid config -> no error.
	o, err := New(Config{Store: store, ModelMgr: &failingModelMgr{}})
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if o == nil {
		t.Fatalf("New returned nil Orchestrator for valid config")
	}
	if o.cfg.MaxROTurns == 0 {
		t.Errorf("budget defaults not applied")
	}
}

// TestSubmitInvalidInputSentinel covers the B4 fix: user-input failures
// from Submit wrap ErrInvalidInput so the HTTP layer can render them as
// 400. All the client-fault paths should match.
func TestSubmitInvalidInputSentinel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	o, err := New(Config{Store: store, DefaultRepo: home, ModelMgr: &failingModelMgr{}})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	cases := []struct {
		name, desc, repo string
	}{
		{"empty-desc", "", home},
		{"whitespace-desc", "   \t\n ", home},
		{"missing-repo", "real", "/does/not/exist/anywhere-at-all"},
	}
	// No repo at all: DefaultRepo is set so this passes through; test it
	// separately with a fresh orchestrator that has no DefaultRepo.
	oNoDefault, err := New(Config{Store: store, ModelMgr: &failingModelMgr{}})
	if err != nil {
		t.Fatalf("New no-default: %v", err)
	}
	if _, err := oNoDefault.Submit(context.Background(), "real", ""); err == nil {
		t.Errorf("expected ErrInvalidInput for missing repo")
	} else if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}

	for _, c := range cases {
		_, err := o.Submit(context.Background(), c.desc, c.repo)
		if err == nil {
			t.Errorf("%s: expected error", c.name)
			continue
		}
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: expected ErrInvalidInput, got %v", c.name, err)
		}
	}
}

// TestAbortUnknownTaskSentinel covers the B5 fix: aborting an id the
// orchestrator has no record of wraps ErrTaskNotFound so the HTTP layer can
// render 404.
func TestAbortUnknownTaskSentinel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	o, err := New(Config{Store: store, ModelMgr: &failingModelMgr{}})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	err = o.Abort("no-such-id-anywhere")
	if err == nil {
		t.Fatalf("expected error aborting unknown task")
	}
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("expected ErrTaskNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "no-such-id-anywhere") {
		t.Errorf("expected task id in error message, got %v", err)
	}
}

// TestTraceFDReleasedOnTerminalStates exercises the B3 reproducer from a
// higher level than the modelmgr start() test: we spin 50 submit+abort
// cycles through the orchestrator with a failing ModelMgr and confirm the
// open fd count stays bounded. Integration auditor measured 10 -> 74 -> 94
// growth before the fix.
func TestTraceFDReleasedOnTerminalStates(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("/proc/self/fd not available on this system")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("AGENT_ROUTER_NO_SMITHY", "1")
	store, err := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatalf("taskstore: %v", err)
	}
	o, err := New(Config{
		Store:    store,
		ModelMgr: &failingModelMgr{},
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	before := countOpenFDsT(t)

	const rounds = 50
	for i := 0; i < rounds; i++ {
		task, err := o.Submit(context.Background(), fmt.Sprintf("desc %d", i), home)
		if err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
		// Give the run goroutine a chance to move past initial state.
		time.Sleep(2 * time.Millisecond)
		// Abort may return nil (running goroutine took over) or a benign
		// error; both are fine for the leak test.
		_ = o.Abort(task.ID)
	}

	// Wait for all run goroutines to unwind so their deferred trace.Close
	// has landed.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		live := len(o.running)
		o.mu.Unlock()
		if live == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Poll after a short settle so async close calls land.
	time.Sleep(100 * time.Millisecond)
	after := countOpenFDsT(t)

	// Bound generously: Go runtime, timers, and the test harness can shift
	// the count by a handful. A leak at rounds=50 would manifest as 40-50+
	// extra fds.
	if after > before+10 {
		t.Errorf("fd leak: before=%d after=%d (rounds=%d)", before, after, rounds)
	}

	// Specifically: no open /proc/self/fd entry should point at any
	// task-owned jsonl trace file.
	entries, _ := os.ReadDir("/proc/self/fd")
	for _, e := range entries {
		target, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err != nil {
			continue
		}
		// TODO(rename): substring matches the trace-dir filesystem path
		// ~/.local/share/agent-router/. When that dir migrates to .../moirai/,
		// update this check too.
		if strings.HasSuffix(target, ".jsonl") && strings.Contains(target, "agent-router") {
			t.Errorf("trace fd still open: %s (fd %s)", target, e.Name())
		}
	}
}

// countOpenFDsT mirrors the modelmgr helper of the same name but lives in
// this test package.
func countOpenFDsT(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

