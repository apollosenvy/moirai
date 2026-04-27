package main

// Tests for FIXER-4 audit fixes (pass 4):
//   REC-1: bind failure must surface to main and cause non-zero exit
//          (we test the goroutine signalling channel here; a full e2e
//          test would require spawning the binary, which is brittle).
//   REC-2: daemon lockfile must reject a second daemon when the first
//          is alive, but recover from a stale lockfile written by a
//          dead PID.

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// ----- REC-2: daemon lockfile -------------------------------------------

func TestAcquireDaemonLockBasic(t *testing.T) {
	tmp := t.TempDir()
	lock := filepath.Join(tmp, "daemon.pid")
	t.Setenv("AGENT_ROUTER_LOCK", lock)

	path, release, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if path != lock {
		t.Errorf("path mismatch: want %q got %q", lock, path)
	}
	defer release()

	// PID file should contain our PID.
	data, err := os.ReadFile(lock)
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	pid, _ := strconv.Atoi(string(data[:len(data)-1])) // strip trailing \n
	if pid != os.Getpid() {
		t.Errorf("lockfile pid mismatch: want %d got %d", os.Getpid(), pid)
	}
}

func TestAcquireDaemonLockRefusesWhenAlive(t *testing.T) {
	tmp := t.TempDir()
	lock := filepath.Join(tmp, "daemon.pid")
	t.Setenv("AGENT_ROUTER_LOCK", lock)

	// First acquire succeeds.
	_, release1, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release1()

	// Second acquire (same process == alive PID) must refuse.
	_, _, err = acquireDaemonLock()
	if err == nil {
		t.Fatal("expected second acquire to fail")
	}
	if !contains(err.Error(), "already running") {
		t.Errorf("expected 'already running' error, got %v", err)
	}
}

func TestAcquireDaemonLockClearsStale(t *testing.T) {
	tmp := t.TempDir()
	lock := filepath.Join(tmp, "daemon.pid")
	t.Setenv("AGENT_ROUTER_LOCK", lock)

	// Write a lockfile with a PID that is almost certainly dead.
	// Use 999999999 -- not negative (which the parser rejects), and well
	// above any plausible live PID on a normal Linux system.
	if err := os.WriteFile(lock, []byte("999999999\n"), 0o644); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}

	path, release, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("expected stale recovery, got %v", err)
	}
	defer release()
	if path != lock {
		t.Errorf("path mismatch")
	}
	// Lockfile now contains OUR PID.
	data, _ := os.ReadFile(lock)
	pid, _ := strconv.Atoi(string(data[:len(data)-1]))
	if pid != os.Getpid() {
		t.Errorf("expected our pid after stale recovery, got %d", pid)
	}
}

func TestReleaseLockFunctionIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	lock := filepath.Join(tmp, "daemon.pid")
	t.Setenv("AGENT_ROUTER_LOCK", lock)

	_, release, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	release() // must not panic, must not delete some other process's lock
	if _, err := os.Stat(lock); err == nil {
		t.Errorf("lockfile should be gone after release")
	}
}

// ----- REC-1: bind failure surfaces via httpErrCh ------------------------

// TestBindFailureSurfaces verifies the goroutine pattern used in cmdDaemon:
// ListenAndServe on an already-bound port should produce an error that lands
// on the error channel (not silently logged-and-forgotten). We don't run
// cmdDaemon directly (it does too much init), but we replicate its core
// goroutine + select logic to prove the contract holds for our fix.
func TestBindFailureSurfaces(t *testing.T) {
	// Bind something on a chosen port so the next attempt EADDRINUSE's.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("seed listener: %v", err)
	}
	addr := l.Addr().String()
	defer l.Close()

	httpSrv := &http.Server{Addr: addr}
	httpErrCh := make(chan error, 1)
	go func() {
		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErrCh <- err
		}
	}()

	select {
	case got := <-httpErrCh:
		if got == nil {
			t.Fatal("expected non-nil error on httpErrCh")
		}
		// "address already in use" or similar -- exact text varies by OS.
		if !contains(got.Error(), "in use") && !contains(got.Error(), "bind") {
			t.Logf("got error (acceptable): %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for httpErrCh; bind error was lost")
	}

	// Cleanup: best-effort shutdown of the never-bound server (no-op).
	_ = httpSrv.Close()
}

// contains is a tiny helper to avoid importing strings just for one Contains.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
