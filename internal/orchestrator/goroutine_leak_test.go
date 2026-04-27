package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aegis/moirai/internal/taskstore"
)

// TestNoGoroutineLeakOnSubmitAbortCycle verifies the orchestrator does not
// leak goroutines across many submit+abort cycles. failingModelMgr makes
// EnsureSlot fail immediately so each run goroutine reaches the fail() path
// without needing a real llama-server. We assert the post-cycle live count
// stays within a small headroom of the baseline.
//
// Counts ALL goroutines, not just orchestrator-owned ones; the test harness
// itself adds a couple. The +10 tolerance absorbs that without permitting
// a real per-task leak (rounds=100 would manifest as 80-100+ leaked).
func TestNoGoroutineLeakOnSubmitAbortCycle(t *testing.T) {
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

	// Settle baseline: any lazy initialisation in the std lib (DNS, etc.)
	// has already happened in the test harness setup; sample now.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const rounds = 100
	for i := 0; i < rounds; i++ {
		task, err := o.Submit(context.Background(), fmt.Sprintf("desc %d", i), home)
		if err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
		// Tiny pause so the run goroutine actually starts; otherwise Abort
		// finds the entry in o.running but the goroutine hasn't had a
		// scheduler turn yet.
		time.Sleep(time.Millisecond)
		_ = o.Abort(task.ID)
	}

	// Wait for o.running to drain.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		live := len(o.running)
		o.mu.Unlock()
		if live == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	o.mu.Lock()
	stillRunning := len(o.running)
	o.mu.Unlock()
	if stillRunning != 0 {
		t.Errorf("o.running not drained after %d rounds: still %d", rounds, stillRunning)
	}

	// Settle and re-sample.
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()

	// Allow scheduler / runtime fluctuation; reject anything that looks
	// like a per-task leak.
	if after > baseline+10 {
		t.Errorf("goroutine leak: baseline=%d after=%d (rounds=%d)", baseline, after, rounds)
	}
}

// TestShutdownDrainsRunGoroutines verifies o.Shutdown() actually awaits
// in-flight run goroutines via the WaitGroup. We Submit several tasks,
// call Shutdown with a generous timeout, and assert no run goroutines
// remain live afterward.
func TestShutdownDrainsRunGoroutines(t *testing.T) {
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

	for i := 0; i < 20; i++ {
		if _, err := o.Submit(context.Background(), fmt.Sprintf("desc %d", i), home); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	if err := o.Shutdown(10 * time.Second); err != nil {
		t.Errorf("Shutdown returned error: %v", err)
	}

	o.mu.Lock()
	stillRunning := len(o.running)
	o.mu.Unlock()
	if stillRunning != 0 {
		t.Errorf("after Shutdown, o.running still has %d entries", stillRunning)
	}
}
