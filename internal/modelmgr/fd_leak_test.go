package modelmgr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStartClosesLogFileOnWaitReadyFailure covers pass-3 B3: when the
// spawned llama-server exits too fast to answer /v1/models (here simulated
// with /usr/bin/true), the earlier implementation left the parent process
// holding the logFile fd. A burst of failed EnsureSlot calls (which is
// exactly what happens under the integration auditor's 50 submit+abort
// reproducer when llama_server_bin is /usr/bin/true) would leak one fd per
// attempt.
//
// We invoke start() directly 30 times. Each call MUST close logFile even
// though waitReady fails. We verify the fd count stays bounded and that no
// open entry under /proc/self/fd points at any llama-planner.log file.
func TestStartClosesLogFileOnWaitReadyFailure(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("/proc/self/fd not available on this system")
	}
	if _, err := os.Stat("/usr/bin/true"); err != nil {
		t.Skip("/usr/bin/true not available on this system")
	}
	// Skip the smithy profile probe; kernel-anvil might be installed on
	// the test host but running it 10+ times per test run would add real
	// wall time for nothing (the test doesn't care about profile gen).
	t.Setenv("AGENT_ROUTER_NO_SMITHY", "1")

	logDir := t.TempDir()
	// /usr/bin/true exits immediately so waitReady hits its first poll on a
	// closed port and retries until the boot timeout expires. The fixed
	// 2-second reap wait inside start() also runs each round. Keep round
	// count modest so the whole test lands in a couple of seconds.
	m, err := New(Config{
		LlamaServerBin: "/usr/bin/true",
		LogDir:         logDir,
		BootTimeout:    150 * time.Millisecond,
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/tmp/fake.gguf", Port: 0},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cfg := m.cfg.Models[SlotPlanner]
	// Pick a high port unlikely to be bound.
	cfg.Port = 59871

	before := countOpenFDs(t)
	const rounds = 10
	for i := 0; i < rounds; i++ {
		// Short per-call timeout so start()'s reap path fires quickly.
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := m.start(ctx, SlotPlanner, &cfg)
		cancel()
		if err == nil {
			t.Fatalf("expected waitReady failure, got nil")
		}
	}
	// Settle.
	time.Sleep(100 * time.Millisecond)
	after := countOpenFDs(t)

	// Conservative bound: one or two fds of scheduler noise are ok, but
	// `rounds` leaked logFile handles would blow past.
	if after > before+5 {
		t.Errorf("fd leak: before=%d after=%d (rounds=%d)", before, after, rounds)
	}

	// Scan /proc/self/fd; no entry should point at llama-planner.log.
	logPath := filepath.Join(logDir, "llama-planner.log")
	entries, _ := os.ReadDir("/proc/self/fd")
	for _, e := range entries {
		target, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err != nil {
			continue
		}
		if target == logPath || strings.HasSuffix(target, "llama-planner.log") {
			t.Errorf("parent process still holds an open fd to %s (fd %s)", logPath, e.Name())
		}
	}
}

// TestTraceFDReleasedOnTerminalStates is the orchestrator-side counterpart
// that the brief asks for: many submit+abort cycles must not accumulate
// open trace file descriptors beyond a small headroom. Lives in the
// orchestrator package's pass3_test.go; a stub here would duplicate work.
//
// Kept as a package-level note so future readers know where to look.
