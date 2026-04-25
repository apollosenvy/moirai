// Package modelmgr manages the lifecycle of llama-server instances.
//
// Only one model is VRAM-resident at a time. When the active slot changes,
// the manager terminates the current llama-server process and spawns a fresh
// one for the new slot. GGUF weights that aren't VRAM-resident still live on
// disk (the kernel's page cache tends to keep them warm between swaps, so
// in practice the second load of a given model is noticeably faster than
// the first).
//
// See SPEC_DEVIATIONS.md for why we don't run three llama-server processes
// in parallel the way the original whiteboard diagram suggested.
package modelmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Slot is a logical role in the A/C/B/C/A workflow.
type Slot string

const (
	SlotPlanner  Slot = "planner"  // A
	SlotCoder    Slot = "coder"    // B
	SlotReviewer Slot = "reviewer" // C
)

// Sampling holds per-slot default sampling parameters. Any field left at
// the Go zero value is omitted from the llama-server request and the server
// applies its own default (which for llama.cpp is temp=0.8, top_k=40,
// top_p=0.95, min_p=0.05 at time of writing). Orchestrator callers may
// override any of these per-request on ChatRequest.
type Sampling struct {
	Temperature float64 `json:"temperature,omitempty"`
	TopK        int     `json:"top_k,omitempty"`
	TopP        float64 `json:"top_p,omitempty"`
	MinP        float64 `json:"min_p,omitempty"`
}

// ModelConfig describes a single model the manager can load.
type ModelConfig struct {
	Slot       Slot   `json:"slot"`
	ModelPath  string `json:"model_path"`
	CtxSize    int    `json:"ctx_size"`
	NGpuLayers int    `json:"n_gpu_layers"`
	Port       int    `json:"port"`
	ExtraArgs  []string `json:"extra_args,omitempty"`
	// KvCache selects the KV cache quantization passed to llama-server as
	// -ctk/-ctv. Valid values: "" (default/f16), "q8_0", "q5_1", "q4_0",
	// "turbo3", "turbo4". Empty string means vanilla llama.cpp default.
	KvCache string `json:"kv_cache,omitempty"`
	// Sampling holds defaults for temperature / top_k / top_p / min_p. These
	// are applied by Complete() when the incoming ChatRequest leaves the
	// corresponding field zero-valued.
	Sampling Sampling `json:"sampling,omitempty"`
}

// Config drives the manager.
type Config struct {
	LlamaServerBin string
	Models         map[Slot]ModelConfig
	LogDir         string
	BootTimeout    time.Duration
}

// PendingChanges is a partial ModelConfig update queued because the slot
// was busy at PATCH time. All fields optional; zero values mean "no change".
type PendingChanges struct {
	ModelPath string `json:"model_path,omitempty"`
	CtxSize   int    `json:"ctx_size,omitempty"`
	KvCache   string `json:"kv_cache,omitempty"`
}

// Manager owns at most one live llama-server process at a time.
type Manager struct {
	cfg Config

	// swapMu serialises the full EnsureSlot() body across concurrent
	// callers. The older implementation released the snapshot mutex (mu)
	// before stop()/start(), which allowed two callers targeting different
	// slots to overlap a teardown with a startup and briefly run two
	// llama-server processes at once (or fight over state mid-swap). swapMu
	// covers the entire EnsureSlot critical section; mu is still used for
	// the short reads/writes of the fields below.
	swapMu sync.Mutex

	mu         sync.Mutex
	activeSlot Slot
	cmd        *exec.Cmd
	port       int
	logPath    string
	started    time.Time
	pending    map[Slot]PendingChanges // guarded by mu

	// generating is true while a Complete() call is in flight against the
	// active slot. Callers use markGenerating/IsGenerating to coordinate
	// pending config changes. Atomic so IsGenerating is lock-free for
	// hot-path status reads.
	generating atomic.Bool

	onSwap func(from, to Slot)

	// ensureSlotEnter (test-only hook) fires inside the swapMu critical
	// section. Production code leaves it nil. Tests use it to observe how
	// many goroutines are concurrently inside the serialised body.
	ensureSlotEnter func()
}

// IsGenerating reports whether a Complete() call is currently in flight.
func (m *Manager) IsGenerating() bool {
	return m.generating.Load()
}

// markGenerating flips generating=true and returns a deferred release fn.
// Use: defer m.markGenerating()()
func (m *Manager) markGenerating() func() {
	m.generating.Store(true)
	return func() { m.generating.Store(false) }
}

// New builds a Manager. No process is spawned yet.
func New(cfg Config) (*Manager, error) {
	if cfg.LlamaServerBin == "" {
		return nil, fmt.Errorf("modelmgr: llama-server binary required")
	}
	if len(cfg.Models) == 0 {
		return nil, fmt.Errorf("modelmgr: at least one model required")
	}
	if cfg.BootTimeout == 0 {
		cfg.BootTimeout = 60 * time.Second
	}
	if cfg.LogDir == "" {
		cfg.LogDir = filepath.Join(os.TempDir(), "agent-router-llama-logs")
	}
	if err := os.MkdirAll(cfg.LogDir, 0o700); err != nil {
		return nil, fmt.Errorf("modelmgr: mkdir log dir: %w", err)
	}
	_ = os.Chmod(cfg.LogDir, 0o700)
	return &Manager{cfg: cfg, pending: make(map[Slot]PendingChanges)}, nil
}

// SetPending queues a partial config change for the given slot, to be folded
// in at the next natural transition point (slot swap). Merges non-zero fields
// of p over any existing pending entry so sequential PATCHes accumulate rather
// than overwriting each other.
func (m *Manager) SetPending(slot Slot, p PendingChanges) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := m.pending[slot]
	if p.ModelPath != "" {
		existing.ModelPath = p.ModelPath
	}
	if p.CtxSize != 0 {
		existing.CtxSize = p.CtxSize
	}
	if p.KvCache != "" {
		existing.KvCache = p.KvCache
	}
	m.pending[slot] = existing
}

// GetPending returns the queued pending changes for slot, if any.
func (m *Manager) GetPending(slot Slot) (PendingChanges, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[slot]
	return p, ok
}

// ClearPending drops any queued pending changes for slot.
func (m *Manager) ClearPending(slot Slot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, slot)
}

// ApplyPending folds queued changes into the slot's ModelConfig and
// clears them. Returns true if there were pending changes.
//
// TODO: persist the updated ModelConfig back to the daemon config file so
// the change survives a restart. For now, applied changes live only in
// memory for the lifetime of the process.
//
// TODO(config-persist): write m.cfg.Models back to ~/.config/agent-router/config.json
// so user-applied slot changes survive daemon restart. Not needed for initial ship.
func (m *Manager) ApplyPending(slot Slot) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[slot]
	if !ok {
		return false
	}
	cfg := m.cfg.Models[slot]
	if p.ModelPath != "" {
		cfg.ModelPath = p.ModelPath
	}
	if p.CtxSize != 0 {
		cfg.CtxSize = p.CtxSize
	}
	if p.KvCache != "" {
		cfg.KvCache = p.KvCache
	}
	m.cfg.Models[slot] = cfg
	delete(m.pending, slot)
	return true
}

// SlotView is the public snapshot of a slot's state.
type SlotView struct {
	Slot           Slot            `json:"slot"`
	RoleLabel      string          `json:"role_label"`
	ModelPath      string          `json:"model_path"`
	ModelName      string          `json:"model_name"`
	CtxSize        int             `json:"ctx_size"`
	KvCache        string          `json:"kv_cache"`
	Loaded         bool            `json:"loaded"`
	ListenPort     int             `json:"listen_port"`
	Generating     bool            `json:"generating"`
	Sampling       Sampling        `json:"sampling"`
	PendingChanges *PendingChanges `json:"pending_changes,omitempty"`
}

// SlotsView returns snapshots of all configured slots.
func (m *Manager) SlotsView() []SlotView {
	m.mu.Lock()
	active := m.activeSlot
	pending := make(map[Slot]PendingChanges, len(m.pending))
	for k, v := range m.pending {
		pending[k] = v
	}
	cfgs := make(map[Slot]ModelConfig, len(m.cfg.Models))
	for k, v := range m.cfg.Models {
		cfgs[k] = v
	}
	m.mu.Unlock()

	roleFor := func(s Slot) string {
		switch s {
		case SlotPlanner:
			return "A"
		case SlotCoder:
			return "B"
		case SlotReviewer:
			return "C"
		}
		return string(s)
	}
	gen := m.IsGenerating()

	// Preserve deterministic order A, B, C.
	ordered := []Slot{SlotPlanner, SlotCoder, SlotReviewer}
	out := make([]SlotView, 0, len(ordered))
	for _, s := range ordered {
		cfg, ok := cfgs[s]
		if !ok {
			continue
		}
		v := SlotView{
			Slot:       s,
			RoleLabel:  roleFor(s),
			ModelPath:  cfg.ModelPath,
			ModelName:  filepath.Base(cfg.ModelPath),
			CtxSize:    cfg.CtxSize,
			KvCache:    cfg.KvCache,
			Loaded:     s == active,
			ListenPort: cfg.Port,
			Generating: s == active && gen,
			Sampling:   cfg.Sampling,
		}
		if p, has := pending[s]; has {
			pCopy := p
			v.PendingChanges = &pCopy
		}
		out = append(out, v)
	}
	return out
}

// SetSwapHook is called (from inside the manager mutex) whenever the active
// slot changes. Keep the hook short and non-blocking.
func (m *Manager) SetSwapHook(fn func(from, to Slot)) {
	m.mu.Lock()
	m.onSwap = fn
	m.mu.Unlock()
}

// Active returns the current slot or "" if nothing is loaded.
func (m *Manager) Active() Slot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeSlot
}

// ActivePort returns the HTTP port of the current llama-server, 0 if none.
func (m *Manager) ActivePort() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port
}

// EnsureSlot guarantees the named slot is VRAM-resident. Returns the HTTP
// base URL of the llama-server instance for that slot. Safe for concurrent
// callers; the entire swap body is serialised on swapMu so two goroutines
// targeting different slots can never overlap a teardown and a startup.
//
// Context cancellation: swapMu queues callers strictly in arrival order, and
// a single swap can take 30-60 seconds in production (stop + start + waitReady).
// Under backpressure (many queued Abort'd tasks, budget timeouts, shutdown),
// the goroutine ahead of you may be doing real work you cannot interrupt; the
// right move for a cancelled caller is to skip the whole stop/start and let
// the next caller through. We check ctx.Err() at every lock-reacquire point:
// after acquiring swapMu, after the initial state snapshot, and between stop()
// and start(). A cancelled task returns ctx.Err() without spawning anything.
//
// Stress-test note (BE-P5-3): an auditor observed "500 fds held" during a
// 500-task submit+abort stress run. Each task holds a trace fd until its run
// goroutine exits, and with swapMu serialising the backlog all 500 fds stayed
// open while the queue drained. The early ctx.Err() checks below are the real
// fix: cancelled tasks skip stop/start and unblock as fast as swapMu passes
// the lock. In production with a working llama-server the queue never builds
// past 2-3 entries, so effective fd usage stays bounded regardless.
func (m *Manager) EnsureSlot(ctx context.Context, slot Slot) (string, error) {
	m.swapMu.Lock()
	defer m.swapMu.Unlock()

	// Early cancellation bailout: by the time this goroutine got the lock,
	// its caller's ctx may already be cancelled (Abort, budget timeout,
	// shutdown). Returning now avoids a 5-second waitReady cycle for work
	// nobody's waiting on.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if hook := m.ensureSlotEnter; hook != nil {
		hook()
	}

	m.mu.Lock()
	if m.activeSlot == slot && m.cmd != nil && m.cmd.Process != nil {
		port := m.port
		m.mu.Unlock()
		return fmt.Sprintf("http://127.0.0.1:%d", port), nil
	}
	from := m.activeSlot
	cfg, ok := m.cfg.Models[slot]
	m.mu.Unlock()

	// Validate target slot BEFORE tearing down the active model. A bogus
	// slot name or a missing model file used to stop the active process
	// first and only then return an error, leaving the daemon with nothing
	// loaded. Fail fast here so the caller's current slot stays up.
	if !ok {
		return "", fmt.Errorf("modelmgr: no config for slot %q", slot)
	}
	if _, err := os.Stat(cfg.ModelPath); err != nil {
		return "", fmt.Errorf("modelmgr: model file missing for slot %s: %w", slot, err)
	}

	// Apply any pending changes staged while the outgoing slot was busy --
	// must happen before stop() so the next start() sees the updated cfg.
	if from != "" && from != slot {
		m.ApplyPending(from)
	}

	// Stop current.
	if err := m.stop(); err != nil {
		// Log but keep going; a stopped-but-not-reaped process is still stopped
		// enough for our purposes.
		fmt.Fprintf(os.Stderr, "modelmgr: stop during swap: %v\n", err)
	}

	// stop() itself can take up to 5 seconds (SIGTERM + Wait + SIGKILL). If
	// the caller was cancelled during that window, bail before spawning a
	// fresh process nobody will use. The outgoing slot is already torn down;
	// that's fine -- the next EnsureSlot caller will start whatever it needs.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if err := m.start(ctx, slot, cfg); err != nil {
		return "", err
	}

	m.mu.Lock()
	hook := m.onSwap
	m.mu.Unlock()
	if hook != nil {
		hook(from, slot)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", cfg.Port), nil
}

func buildLlamaArgs(cfg ModelConfig) []string {
	args := []string{
		"--model", cfg.ModelPath,
		"--port", strconv.Itoa(cfg.Port),
		"--host", "127.0.0.1",
	}
	if cfg.CtxSize > 0 {
		args = append(args, "-c", strconv.Itoa(cfg.CtxSize))
	}
	if cfg.NGpuLayers != 0 {
		args = append(args, "-ngl", strconv.Itoa(cfg.NGpuLayers))
	}
	if cfg.KvCache != "" {
		// Only add -ctk / -ctv from the KvCache field if ExtraArgs hasn't
		// already supplied them. llama-server takes the LAST occurrence of
		// a flag, so without this dedup an ExtraArgs entry like
		// `-ctk q8_0 -ctv turbo3` would silently override the explicit
		// KvCache value -- the user-facing field would look ignored.
		hasCtk, hasCtv := false, false
		for _, a := range cfg.ExtraArgs {
			switch a {
			case "-ctk":
				hasCtk = true
			case "-ctv":
				hasCtv = true
			}
		}
		if !hasCtk {
			args = append(args, "-ctk", cfg.KvCache)
		}
		if !hasCtv {
			args = append(args, "-ctv", cfg.KvCache)
		}
	}
	args = append(args, cfg.ExtraArgs...)
	return args
}

func (m *Manager) start(ctx context.Context, slot Slot, cfg ModelConfig) error {
	args := buildLlamaArgs(cfg)

	logPath := filepath.Join(m.cfg.LogDir, fmt.Sprintf("llama-%s.log", slot))
	// SEC-PASS5-005: 0o600 daemon log; llama-server stdout/stderr can echo
	// prompts and model output, not for other local users on shared hosts.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("modelmgr: create log: %w", err)
	}

	// Kernel-anvil / smithy profile. If the profile exists (or we can
	// generate it with the kernel-anvil CLI), hand its path to llama-server
	// via SMITHY_CONFIG. The custom MMVQ kernels in llama-cpp-turboquant use
	// that to dispatch shape-specific configs and win ~1.5-2x decode on
	// 7900 XTX. Profile-gen can take up to a few minutes the first time;
	// failures are logged and non-fatal -- llama.cpp still runs with the
	// stock kernel dispatch.
	env := os.Environ()
	if smithyPath, err := ensureSmithyProfile(ctx, cfg.ModelPath); err == nil && smithyPath != "" {
		env = append(env, "SMITHY_CONFIG="+smithyPath)
		fmt.Fprintf(logFile, "[agent-router] SMITHY_CONFIG=%s\n", smithyPath)
	} else if err != nil {
		fmt.Fprintf(logFile, "[agent-router] smithy profile unavailable: %v\n", err)
		fmt.Fprintf(os.Stderr, "modelmgr: smithy profile unavailable for slot %s: %v\n", slot, err)
	}

	cmd := exec.Command(m.cfg.LlamaServerBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = env
	// Setpgid: own process group so Kill(-pgid) takes the whole tree.
	// Pdeathsig: SIGKILL the child if the parent (this daemon) dies. Belt
	// and suspenders against a daemon crash where Shutdown never gets to
	// run; without this, a llama-server child can outlive the daemon and
	// keep its port + GPU buffers pinned.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("modelmgr: start llama-server: %w", err)
	}

	// Wait for /health or /v1/models to respond.
	if err := waitReady(ctx, cfg.Port, m.cfg.BootTimeout); err != nil {
		// Kill the stuck process before bailing.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		time.Sleep(200 * time.Millisecond)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		// Reap the child so it doesn't linger as a zombie. Bounded wait
		// mirrors the pattern used in stop(): the select/timeout guards
		// against the kill failing to reach a stuck process.
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t := time.NewTimer(2 * time.Second)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
		}
		// Close the parent's logFile fd. The earlier version deliberately
		// left this open so postmortem readers could inspect the file, but
		// closing the parent's *os.File does NOT affect readability -- the
		// bytes are already flushed and the file stays on disk at logPath.
		// Leaving it open leaks one fd per failed EnsureSlot, which under
		// a misbehaving llama-server bin (e.g. /usr/bin/true during tests
		// or a crash loop in production) accumulates fast and eventually
		// exhausts the daemon's fd table.
		_ = logFile.Close()
		return fmt.Errorf("modelmgr: slot %s not ready: %w (see %s)", slot, err, logPath)
	}

	// Child has dup'd the logFile fd via cmd.Stdout/Stderr; the parent's
	// *os.File is no longer needed and would otherwise leak one fd per
	// swap. Close on the success path only -- failure paths above leave
	// the log inspectable to whoever is diagnosing the failure.
	_ = logFile.Close()

	m.mu.Lock()
	m.activeSlot = slot
	m.cmd = cmd
	m.port = cfg.Port
	m.logPath = logPath
	m.started = time.Now()
	m.mu.Unlock()
	return nil
}

func (m *Manager) stop() error {
	m.mu.Lock()
	cmd := m.cmd
	m.cmd = nil
	m.activeSlot = ""
	m.port = 0
	m.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid := cmd.Process.Pid
	// SIGTERM the whole process group so any llama-server children die too.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case <-done:
		return nil
	case <-t.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		// Bound the post-SIGKILL wait. If the child is in uninterruptible
		// sleep (D state, e.g. stuck on a kernel/driver call), cmd.Wait()
		// can block forever and stall daemon shutdown indefinitely. Drop the
		// reference after the grace period and surface a stderr warning so
		// the operator notices the orphan.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			fmt.Fprintf(os.Stderr,
				"modelmgr: child pid %d did not reap after SIGKILL; "+
					"orphaning (likely D-state)\n", pgid)
		}
		return nil
	}
}

// Shutdown tears down whatever is currently running. Call on daemon exit.
//
// Acquires swapMu so an in-flight EnsureSlot finishes (or its caller's ctx
// has already been cancelled) before we try to stop the process. Without
// this, a Shutdown that runs while EnsureSlot is mid-start() could observe
// m.cmd == nil at the start() boundary, return without killing anything,
// and then EnsureSlot would write m.cmd = cmd just before main exits --
// leaving the llama-server child orphaned with its port and GPU pinned.
//
// The Pdeathsig set in start() is the second line of defence: even if a
// future caller races past Shutdown, the kernel will SIGKILL the child
// when the daemon process exits.
func (m *Manager) Shutdown() error {
	m.swapMu.Lock()
	defer m.swapMu.Unlock()
	return m.stop()
}

// DetectTurboquant runs `<llamaServerBin> --help` and returns true if the
// output mentions both "turbo3" and "turbo4" as KV cache options. Can be
// forced via AGENT_ROUTER_FORCE_TURBOQUANT=1 env var.
func DetectTurboquant(llamaServerBin string) bool {
	if os.Getenv("AGENT_ROUTER_FORCE_TURBOQUANT") == "1" {
		return true
	}
	if llamaServerBin == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, llamaServerBin, "--help")
	out, _ := cmd.CombinedOutput() // non-zero exit is fine; we just want the text
	text := strings.ToLower(string(out))
	return strings.Contains(text, "turbo3") && strings.Contains(text, "turbo4")
}

// waitReady polls the OpenAI-compatible /v1/models endpoint until success.
func waitReady(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/models", port)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s", timeout)
}

// Complete posts an OpenAI-style chat completion to the currently-loaded
// llama-server. Caller must have called EnsureSlot first.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Messages    []ChatMessage `json:"messages"`
	Temperature float64       `json:"temperature,omitempty"`
	TopK        int           `json:"top_k,omitempty"`
	TopP        float64       `json:"top_p,omitempty"`
	MinP        float64       `json:"min_p,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream"`
}

// serverMessage mirrors the OpenAI response shape but also captures
// llama.cpp's `reasoning_content` field. Qwen3 and similar reasoning models
// route their `<think>...</think>` output into reasoning_content; if we
// ignored it, the assistant's actual answer could look like an empty string.
type serverMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
}

type chatChoice struct {
	Message      serverMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}
type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

// Complete returns the assistant message text.
func (m *Manager) Complete(ctx context.Context, req ChatRequest) (string, error) {
	release := m.markGenerating()
	defer release()
	m.mu.Lock()
	port := m.port
	activeSlot := m.activeSlot
	slotCfg, hasSlotCfg := m.cfg.Models[activeSlot]
	m.mu.Unlock()
	if port == 0 {
		return "", fmt.Errorf("modelmgr: no model loaded")
	}
	// Layer slot defaults underneath anything the caller set. A caller who
	// passed temperature=0 explicitly still reads as zero here; that collapse
	// is intentional -- llama.cpp treats 0 as "greedy" and we want the slot
	// default to cover the common case where the orchestrator just forgot to
	// specify sampling at all.
	if hasSlotCfg {
		if req.Temperature == 0 {
			req.Temperature = slotCfg.Sampling.Temperature
		}
		if req.TopK == 0 {
			req.TopK = slotCfg.Sampling.TopK
		}
		if req.TopP == 0 {
			req.TopP = slotCfg.Sampling.TopP
		}
		if req.MinP == 0 {
			req.MinP = slotCfg.Sampling.MinP
		}
	}
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// No hard wall-clock ceiling; the caller's ctx carries the real deadline
	// (the orchestrator binds it to the task budget). A dedicated 45-minute
	// client-side Timeout used to rug-pull slow reasoning runs even when the
	// orchestrator had budget left. Cancellation still works via ctx.
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		rb, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("llama-server %d: %s", resp.StatusCode, string(rb))
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llama-server: empty response")
	}
	msg := out.Choices[0].Message
	if msg.Content != "" {
		return msg.Content, nil
	}
	// Reasoning model emitted everything inside <think>. llama.cpp pulls
	// that into reasoning_content; use it as a fallback so the orchestrator
	// has something to parse.
	if msg.ReasoningContent != "" {
		return msg.ReasoningContent, nil
	}
	return "", fmt.Errorf("llama-server: empty content (finish=%s)", out.Choices[0].FinishReason)
}
