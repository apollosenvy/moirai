package modelmgr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestEnsureSlotSerialises fires two goroutines into EnsureSlot against
// different slots with missing model files. The model-file stat check
// inside EnsureSlot forces the body to run (there's no fast "already
// loaded" exit), so if swapMu is absent the two bodies overlap and the
// concurrent-entry counter spikes to 2. With swapMu in place the counter
// never exceeds 1.
func TestEnsureSlotSerialises(t *testing.T) {
	m, err := New(Config{
		LlamaServerBin: "/bin/true",
		Models: map[Slot]ModelConfig{
			SlotPlanner:  {Slot: SlotPlanner, ModelPath: "/tmp/definitely-not-a-real-gguf-a.gguf", Port: 9101},
			SlotCoder:    {Slot: SlotCoder, ModelPath: "/tmp/definitely-not-a-real-gguf-b.gguf", Port: 9102},
			SlotReviewer: {Slot: SlotReviewer, ModelPath: "/tmp/definitely-not-a-real-gguf-c.gguf", Port: 9103},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var inside atomic.Int32
	var peak atomic.Int32

	// Hook fires inside the swapMu critical section. Any concurrent entry
	// indicates serialization is broken.
	m.ensureSlotEnter = func() {
		n := inside.Add(1)
		for {
			prev := peak.Load()
			if n <= prev {
				break
			}
			if peak.CompareAndSwap(prev, n) {
				break
			}
		}
		// Hold briefly so a parallel caller would have time to overlap.
		time.Sleep(20 * time.Millisecond)
		inside.Add(-1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, slot := range []Slot{SlotPlanner, SlotCoder, SlotReviewer} {
		wg.Add(1)
		go func(s Slot) {
			defer wg.Done()
			// We expect these to return a stat error; that's fine -- what we
			// actually care about is observing the critical-section counter.
			_, _ = m.EnsureSlot(ctx, s)
		}(slot)
	}
	wg.Wait()

	if got := peak.Load(); got > 1 {
		t.Fatalf("EnsureSlot swap body not serialised: peak concurrent entries = %d", got)
	}
}

// TestEnsureSlotUnknownSlotDoesNotKillActive verifies that when EnsureSlot
// is called with a slot name that has no config entry, the currently-loaded
// model is NOT torn down. Prior to the fix, the code stopped the active
// process BEFORE validating the target slot, leaving the daemon with
// nothing loaded after any bogus swap request.
func TestEnsureSlotUnknownSlotDoesNotKillActive(t *testing.T) {
	// Use /usr/bin/sleep so the simulated active process survives -- we
	// want to be able to signal it ourselves after the failed EnsureSlot.
	if _, err := os.Stat("/usr/bin/sleep"); err != nil {
		t.Skip("/usr/bin/sleep not available on this system")
	}
	m, err := New(Config{
		LlamaServerBin: "/usr/bin/true",
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/tmp/fake.gguf", Port: 19101},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Simulate a planner process already running. We spawn /usr/bin/sleep
	// in its own process group so stop() would be able to signal it.
	cmd := exec.Command("/usr/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	m.mu.Lock()
	m.activeSlot = SlotPlanner
	m.cmd = cmd
	m.port = 19101
	m.started = time.Now()
	m.mu.Unlock()

	// Call EnsureSlot with a slot that has no config entry.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = m.EnsureSlot(ctx, Slot("bogus"))
	if err == nil {
		t.Fatalf("expected error for unknown slot, got nil")
	}

	// Planner must still be loaded.
	m.mu.Lock()
	active := m.activeSlot
	curCmd := m.cmd
	port := m.port
	m.mu.Unlock()
	if active != SlotPlanner {
		t.Errorf("activeSlot changed after failed EnsureSlot: got %q, want %q", active, SlotPlanner)
	}
	if curCmd == nil {
		t.Fatalf("m.cmd was cleared after failed EnsureSlot")
	}
	if curCmd.Process == nil {
		t.Fatalf("m.cmd.Process was cleared after failed EnsureSlot")
	}
	if port != 19101 {
		t.Errorf("port changed after failed EnsureSlot: got %d, want 19101", port)
	}

	// Confirm the sleep process is still alive by signalling it with 0.
	if err := syscall.Kill(curCmd.Process.Pid, 0); err != nil {
		t.Errorf("active process appears dead after failed EnsureSlot: %v", err)
	}
}

// TestEnsureSlotMissingModelDoesNotKillActive is the twin of the unknown-slot
// test: target slot exists in config but its ModelPath doesn't stat. Active
// slot must survive.
func TestEnsureSlotMissingModelDoesNotKillActive(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sleep"); err != nil {
		t.Skip("/usr/bin/sleep not available on this system")
	}
	m, err := New(Config{
		LlamaServerBin: "/usr/bin/true",
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/tmp/fake-planner.gguf", Port: 19111},
			SlotCoder:   {Slot: SlotCoder, ModelPath: "/tmp/absolutely-does-not-exist-9983.gguf", Port: 19112},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cmd := exec.Command("/usr/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	m.mu.Lock()
	m.activeSlot = SlotPlanner
	m.cmd = cmd
	m.port = 19111
	m.started = time.Now()
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = m.EnsureSlot(ctx, SlotCoder)
	if err == nil {
		t.Fatalf("expected error for missing model file, got nil")
	}

	m.mu.Lock()
	active := m.activeSlot
	curCmd := m.cmd
	m.mu.Unlock()
	if active != SlotPlanner {
		t.Errorf("activeSlot = %q, want %q", active, SlotPlanner)
	}
	if curCmd == nil || curCmd.Process == nil {
		t.Fatalf("active process cleared after failed EnsureSlot")
	}
	if err := syscall.Kill(curCmd.Process.Pid, 0); err != nil {
		t.Errorf("active process appears dead after failed EnsureSlot: %v", err)
	}
}

// countOpenFDs returns the number of entries in /proc/self/fd, skipping
// the directory handle created by os.ReadDir itself. Returns -1 if /proc
// is not available on this platform.
func countOpenFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

// fakeReadyServer binds a local tcp listener and serves /v1/models so
// waitReady() returns nil. Returns the chosen port and a cleanup fn.
func fakeReadyServer(t *testing.T) (int, func()) {
	t.Helper()
	srv := newFakeLlamaServer(t)
	return srv.port, srv.close
}

// TestStartClosesParentLogFile verifies that after a successful start(),
// the parent *os.File for the llama-server log is closed so we don't leak
// one fd per swap. The fd count just before calling should be higher
// than the count after; at minimum, it should not grow.
func TestStartClosesParentLogFile(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("/proc/self/fd not available on this system")
	}
	// For this test we want start() to succeed. Use a tiny script that
	// binds to the requested --port so waitReady() gets a 200 on
	// /v1/models. The fakeLlamaServer helper does that.
	port, cleanup := fakeReadyServer(t)
	defer cleanup()

	// We're going to invoke start() directly so we fully control the
	// llama_server_bin spawn. Point the bin at /usr/bin/true (just so the
	// exec.Cmd fields are consistent); waitReady is what gates "success",
	// and that points at the fake server we already spun up.
	logDir := t.TempDir()
	m, err := New(Config{
		LlamaServerBin: "/usr/bin/true",
		LogDir:         logDir,
		BootTimeout:    5 * time.Second,
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/tmp/fake.gguf", Port: port},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cfg := m.cfg.Models[SlotPlanner]

	before := countOpenFDs(t)
	if err := m.start(context.Background(), SlotPlanner, cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.stop() })

	// Give fd table a moment to settle.
	time.Sleep(50 * time.Millisecond)
	after := countOpenFDs(t)

	// The parent's logFile fd must be closed. Allow 1-2 fds of headroom
	// for goroutine-scheduler / timer fds that Go may have created
	// incidentally, but the delta should be bounded.
	if after > before+2 {
		t.Errorf("fd leak detected: before=%d after=%d; expected parent logFile to be closed", before, after)
	}

	// Walk /proc/self/fd looking for a link pointing at the llama-planner.log
	// file. Parent fd must be closed.
	logPath := filepath.Join(logDir, "llama-planner.log")
	entries, _ := os.ReadDir("/proc/self/fd")
	for _, e := range entries {
		target, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err != nil {
			continue
		}
		if target == logPath {
			t.Errorf("parent process still holds an open fd to %s (fd %s)", logPath, e.Name())
		}
	}
}

// TestWaitReadyFailureReapsChild points start() at /usr/bin/true (exits
// immediately, never listens) so waitReady() fails. Before the fix, cmd
// never had Wait() called, leaving a zombie. After the fix, start()
// reaps the child via a goroutine+timeout. We assert cmd.ProcessState
// is non-nil within a short grace period, indicating Wait completed.
func TestWaitReadyFailureReapsChild(t *testing.T) {
	// /usr/bin/true returns immediately and never binds a port.
	m, err := New(Config{
		LlamaServerBin: "/usr/bin/true",
		LogDir:         t.TempDir(),
		BootTimeout:    500 * time.Millisecond,
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/tmp/fake.gguf", Port: pickUnusedPort(t)},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cfg := m.cfg.Models[SlotPlanner]
	err = m.start(context.Background(), SlotPlanner, cfg)
	if err == nil {
		t.Fatalf("expected start() to fail because /usr/bin/true never listens")
	}

	// The child should be reaped by the deferred wait inside start()'s
	// failure path. Poll briefly for cmd.ProcessState != nil via /proc;
	// start() doesn't expose its internal *exec.Cmd, so we fall back to
	// the weaker "nothing owned by the manager is left behind" check:
	// m.cmd is nil and m.activeSlot == "". This combined with the
	// bounded Wait() in start() ensures no zombie lingers.
	m.mu.Lock()
	if m.cmd != nil {
		t.Errorf("manager still holds cmd after failed start()")
	}
	if m.activeSlot != "" {
		t.Errorf("activeSlot = %q after failed start(), want empty", m.activeSlot)
	}
	m.mu.Unlock()

	// Direct zombie check via /proc. Any zombie under our pgid would show
	// up as state='Z'. Scanning all children of the current process is
	// portable enough for this environment.
	if _, err := os.Stat("/proc/self/task"); err == nil {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !hasZombieChild(t) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		if hasZombieChild(t) {
			t.Errorf("zombie child still present after failed start()")
		}
	}
}

// TestEnsureSlotRespectsContextCancellation verifies the BE-P5-1 fix: a
// caller whose ctx gets cancelled while it's queued behind swapMu returns
// ctx.Err() immediately once the lock is handed to it, without stopping or
// starting any process. Before the fix, a cancelled caller would still run
// the full stop/start/waitReady cycle (up to 5+ seconds) before noticing.
func TestEnsureSlotRespectsContextCancellation(t *testing.T) {
	m, err := New(Config{
		LlamaServerBin: "/usr/bin/true",
		LogDir:         t.TempDir(),
		Models: map[Slot]ModelConfig{
			// Use a model path that WOULD pass the stat check if we ever got
			// past the ctx check, so a regression that skips the ctx check
			// actually fires stop/start and fails the test loudly rather than
			// quietly bailing on a missing-file error.
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/usr/bin/true", Port: pickUnusedPort(t)},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Observability: count calls through the critical-section hook and track
	// whether start() was invoked. We can't easily stub start() without
	// changing the Manager shape, so we assert indirectly: after a cancelled
	// EnsureSlot returns, m.cmd must still be nil and m.activeSlot must still
	// be "" -- no process was started.
	var hookCalls atomic.Int32
	m.ensureSlotEnter = func() { hookCalls.Add(1) }

	// Hold swapMu externally so the EnsureSlot goroutine has to queue.
	m.swapMu.Lock()

	ctx, cancel := context.WithCancel(context.Background())
	retCh := make(chan error, 1)
	go func() {
		_, err := m.EnsureSlot(ctx, SlotPlanner)
		retCh <- err
	}()

	// Give the goroutine a moment to actually block on swapMu.
	time.Sleep(50 * time.Millisecond)

	// Cancel while the goroutine is still waiting on the lock. When we
	// release swapMu below, the goroutine should see ctx.Err() immediately
	// and return without spawning anything.
	cancel()
	m.swapMu.Unlock()

	select {
	case gotErr := <-retCh:
		if gotErr == nil {
			t.Fatalf("expected non-nil error from cancelled EnsureSlot, got nil")
		}
		if gotErr != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", gotErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("EnsureSlot did not return within 2s of ctx cancellation")
	}

	// The ensureSlotEnter hook fires AFTER swapMu is acquired but BEFORE
	// the ctx.Err() check was in place. With the fix, the ctx.Err() check
	// is the very first thing after swapMu.Lock(), so the hook should NOT
	// fire for a cancelled caller. (Production code sets the hook to nil,
	// so this is purely a test-only observability gate.)
	if n := hookCalls.Load(); n != 0 {
		t.Errorf("ensureSlotEnter hook fired %d times; expected 0 (ctx check should short-circuit before hook)", n)
	}

	// Nothing should have been spawned.
	m.mu.Lock()
	active := m.activeSlot
	curCmd := m.cmd
	m.mu.Unlock()
	if active != "" {
		t.Errorf("activeSlot = %q after cancelled EnsureSlot, want empty", active)
	}
	if curCmd != nil {
		t.Errorf("m.cmd is non-nil after cancelled EnsureSlot; a process was spawned")
	}
}

// TestEnsureSlotRespectsCancellationBetweenStopAndStart verifies the second
// ctx.Err() check: after stop() but before start(). We set up an active slot,
// then call EnsureSlot for a different slot whose ctx gets cancelled while
// stop() is running. The function should return ctx.Err() without spawning
// a new llama-server for the target slot.
func TestEnsureSlotRespectsCancellationBetweenStopAndStart(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sleep"); err != nil {
		t.Skip("/usr/bin/sleep not available on this system")
	}
	m, err := New(Config{
		LlamaServerBin: "/usr/bin/true",
		LogDir:         t.TempDir(),
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/usr/bin/true", Port: pickUnusedPort(t)},
			SlotCoder:   {Slot: SlotCoder, ModelPath: "/usr/bin/true", Port: pickUnusedPort(t)},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Simulate an active Planner process via /usr/bin/sleep. stop() will
	// SIGTERM it; the Wait() inside stop() returns quickly once sleep exits.
	cmd := exec.Command("/usr/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	m.mu.Lock()
	m.activeSlot = SlotPlanner
	m.cmd = cmd
	m.port = 19201
	m.started = time.Now()
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel the ctx during EnsureSlot's critical section, AFTER the entry
	// ctx.Err() check has already passed. The ensureSlotEnter hook runs
	// synchronously right after that first check and before stop(), so
	// cancelling here lands between the entry check and the post-stop check
	// -- the window we want to exercise. stop() on /usr/bin/sleep returns in
	// milliseconds (SIGTERM + Wait), so the post-stop ctx.Err() check will
	// see the cancellation and return before start() runs.
	m.ensureSlotEnter = func() {
		cancel()
	}

	retCh := make(chan error, 1)
	go func() {
		_, err := m.EnsureSlot(ctx, SlotCoder)
		retCh <- err
	}()

	select {
	case gotErr := <-retCh:
		if gotErr != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", gotErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("EnsureSlot did not return within 10s")
	}

	// Active slot should be torn down (stop ran), but no new process started
	// for SlotCoder. Verify by confirming m.cmd is nil.
	m.mu.Lock()
	active := m.activeSlot
	curCmd := m.cmd
	m.mu.Unlock()
	if active != "" {
		t.Errorf("activeSlot = %q after cancelled EnsureSlot; expected empty (stop ran, start skipped)", active)
	}
	if curCmd != nil {
		t.Errorf("m.cmd is non-nil after cancelled EnsureSlot; start() should have been skipped")
	}
}

// hasZombieChild walks /proc and returns true if any process has our pid
// as its parent AND state 'Z' (zombie). Used by the waitReady-failure test.
func hasZombieChild(t *testing.T) bool {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	mypid := os.Getpid()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		statBytes, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// /proc/<pid>/stat fields: pid (comm) state ppid ...
		// comm is in parens and may contain spaces, so find the closing ')'.
		s := string(statBytes)
		rparen := -1
		for i := len(s) - 1; i >= 0; i-- {
			if s[i] == ')' {
				rparen = i
				break
			}
		}
		if rparen < 0 || rparen+4 >= len(s) {
			continue
		}
		// After ')': " X P ..." where X is state and P is ppid.
		rest := s[rparen+2:]
		var state string
		var ppid int
		if _, err := fmt.Sscanf(rest, "%s %d", &state, &ppid); err != nil {
			continue
		}
		if ppid == mypid && state == "Z" {
			return true
		}
	}
	return false
}
