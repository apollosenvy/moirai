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
	"sync"
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

// ModelConfig describes a single model the manager can load.
type ModelConfig struct {
	Slot       Slot   `json:"slot"`
	ModelPath  string `json:"model_path"`
	CtxSize    int    `json:"ctx_size"`
	NGpuLayers int    `json:"n_gpu_layers"`
	Port       int    `json:"port"`
	ExtraArgs  []string `json:"extra_args,omitempty"`
}

// Config drives the manager.
type Config struct {
	LlamaServerBin string
	Models         map[Slot]ModelConfig
	LogDir         string
	BootTimeout    time.Duration
}

// Manager owns at most one live llama-server process at a time.
type Manager struct {
	cfg Config

	mu         sync.Mutex
	activeSlot Slot
	cmd        *exec.Cmd
	port       int
	logPath    string
	started    time.Time

	onSwap func(from, to Slot)
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
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return nil, fmt.Errorf("modelmgr: mkdir log dir: %w", err)
	}
	return &Manager{cfg: cfg}, nil
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
// callers; serialises swaps.
func (m *Manager) EnsureSlot(ctx context.Context, slot Slot) (string, error) {
	m.mu.Lock()
	if m.activeSlot == slot && m.cmd != nil && m.cmd.Process != nil {
		port := m.port
		m.mu.Unlock()
		return fmt.Sprintf("http://127.0.0.1:%d", port), nil
	}
	from := m.activeSlot
	m.mu.Unlock()

	// Stop current (outside lock to avoid holding it across SIGTERM waits).
	if err := m.stop(); err != nil {
		// Log but keep going; a stopped-but-not-reaped process is still stopped
		// enough for our purposes.
		fmt.Fprintf(os.Stderr, "modelmgr: stop during swap: %v\n", err)
	}

	cfg, ok := m.cfg.Models[slot]
	if !ok {
		return "", fmt.Errorf("modelmgr: no config for slot %q", slot)
	}
	if _, err := os.Stat(cfg.ModelPath); err != nil {
		return "", fmt.Errorf("modelmgr: model file missing for slot %s: %w", slot, err)
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

func (m *Manager) start(ctx context.Context, slot Slot, cfg ModelConfig) error {
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
	args = append(args, cfg.ExtraArgs...)

	logPath := filepath.Join(m.cfg.LogDir, fmt.Sprintf("llama-%s.log", slot))
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("modelmgr: create log: %w", err)
	}

	cmd := exec.Command(m.cfg.LlamaServerBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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
		logFile.Close()
		return fmt.Errorf("modelmgr: slot %s not ready: %w (see %s)", slot, err, logPath)
	}

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
	prev := m.activeSlot
	m.activeSlot = ""
	m.port = 0
	m.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = prev
	pgid := cmd.Process.Pid
	// SIGTERM the whole process group so any llama-server children die too.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
		return nil
	}
}

// Shutdown tears down whatever is currently running. Call on daemon exit.
func (m *Manager) Shutdown() error {
	return m.stop()
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
	m.mu.Lock()
	port := m.port
	m.mu.Unlock()
	if port == 0 {
		return "", fmt.Errorf("modelmgr: no model loaded")
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
	// Generous timeout. Reasoning-budget unlimited + 32k+ ctx can push
	// single-call wall time past 15 min on slow paths; we'd rather let the
	// run complete than ragequit on token-rate headwinds.
	client := &http.Client{Timeout: 45 * time.Minute}
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
