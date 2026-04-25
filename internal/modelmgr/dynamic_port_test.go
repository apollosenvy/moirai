package modelmgr

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPickFreePortReturnsUsablePort verifies pickFreePort yields a port
// that the caller can actually bind to immediately after.
func TestPickFreePortReturnsUsablePort(t *testing.T) {
	port, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	if port < 1024 || port > 65535 {
		t.Fatalf("pickFreePort returned out-of-range port %d", port)
	}
	// Bind it ourselves to confirm the kernel hasn't already handed it
	// out to someone else. There's a tiny TOCTOU window; if this test
	// flakes under load, the real fix is openat-style atomic handoff,
	// not a wider window. Single-user daemon does not warrant that.
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("rebind picked port %d: %v", port, err)
	}
	l.Close()
}

// TestPickFreePortDistinct: two consecutive calls should usually return
// different ports. Relax the assertion to "do not panic and both work";
// kernel may legitimately reuse a just-closed port if nothing else is
// pressuring the ephemeral range.
func TestPickFreePortDistinct(t *testing.T) {
	a, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort #1: %v", err)
	}
	b, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort #2: %v", err)
	}
	if a < 1024 || b < 1024 {
		t.Fatalf("pickFreePort returned reserved port a=%d b=%d", a, b)
	}
	// No equality assertion -- the kernel may reuse the just-closed port.
}

// TestStartAssignsDynamicPortWhenZero exercises the dynamic-port path:
// when ModelConfig.Port is zero, start() picks a free port, mutates the
// passed-in cfg, and the chosen port appears in the args llama-server
// would receive. We use /bin/true as the llama-server binary; start
// will spawn it, fail waitReady (true exits immediately so no /v1/models
// listener), and return a "not ready" error. That's expected -- we are
// only testing the port-picking path.
func TestStartAssignsDynamicPortWhenZero(t *testing.T) {
	tmp := t.TempDir()
	m, err := New(Config{
		LlamaServerBin: "/bin/true",
		LogDir:         tmp,
		BootTimeout:    250 * time.Millisecond,
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/bin/true", Port: 0},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cfg := m.cfg.Models[SlotPlanner]
	if cfg.Port != 0 {
		t.Fatalf("setup: expected cfg.Port == 0, got %d", cfg.Port)
	}
	// start() will spawn /bin/true, true exits, waitReady fails. We do
	// not care about the failure; we care that cfg.Port has been set
	// to a non-zero value by the time start returns.
	_ = m.start(t.Context(), SlotPlanner, &cfg)

	if cfg.Port == 0 {
		t.Fatalf("dynamic port not assigned: cfg.Port still 0 after start()")
	}
	if cfg.Port < 1024 || cfg.Port > 65535 {
		t.Fatalf("dynamic port out of range: %d", cfg.Port)
	}
}

// TestStartHonorsExplicitPort: a caller that pinned cfg.Port to a real
// number must NOT have it overridden. (Back-compat with prior config
// JSONs that hardcoded 8001/8002/8003.)
func TestStartHonorsExplicitPort(t *testing.T) {
	tmp := t.TempDir()
	m, err := New(Config{
		LlamaServerBin: "/bin/true",
		LogDir:         tmp,
		BootTimeout:    250 * time.Millisecond,
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/bin/true", Port: 19999},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cfg := m.cfg.Models[SlotPlanner]
	_ = m.start(t.Context(), SlotPlanner, &cfg)

	if cfg.Port != 19999 {
		t.Fatalf("explicit port overridden: want 19999, got %d", cfg.Port)
	}
}

// TestBuildLlamaArgsContainsPort: round-trip the chosen port through
// buildLlamaArgs to make sure the --port flag actually carries it.
func TestBuildLlamaArgsContainsPort(t *testing.T) {
	port, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	args := buildLlamaArgs(ModelConfig{
		Slot:      SlotPlanner,
		ModelPath: "/tmp/x.gguf",
		Port:      port,
	})
	want := "--port"
	for i, a := range args {
		if a == want && i+1 < len(args) {
			if args[i+1] != strconv.Itoa(port) {
				t.Fatalf("args[--port+1] = %q, want %q", args[i+1], strconv.Itoa(port))
			}
			return
		}
	}
	t.Fatalf("--port not present in args: %s", strings.Join(args, " "))
}
