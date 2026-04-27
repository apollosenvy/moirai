// moirai: three-model coding daemon. (Project was previously known as agent-router.)
//
// Subcommands:
//   daemon              run the HTTP daemon
//   task "desc"         submit a task (requires daemon running)
//   inspect <id>        dump task state and recent trace events
//   abort <id>          stop a running task
//   status              list tasks and daemon status
//
// Config precedence:
//   --config=PATH > $AGENT_ROUTER_CONFIG > ~/.config/agent-router/config.json
// TODO(rename): consider migrating to ~/.config/moirai/ in a future commit;
// orphaning existing trace data in production today.
//
// Defaults live in defaultConfig() below; they target Gary's Aegis box.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aegis/moirai/internal/aegis"
	"github.com/aegis/moirai/internal/api"
	"github.com/aegis/moirai/internal/modelmgr"
	"github.com/aegis/moirai/internal/orchestrator"
	"github.com/aegis/moirai/internal/proctitle"
	"github.com/aegis/moirai/internal/taskstore"
)

type Config struct {
	Port           int                                 `json:"port"`
	LlamaServerBin string                              `json:"llama_server_bin"`
	Models         map[modelmgr.Slot]modelmgr.ModelConfig `json:"models"`
	DefaultRepo    string                              `json:"default_repo"`
	ScratchDir     string                              `json:"scratch_dir"`
	LogDir         string                              `json:"log_dir"`
	MaxCoderRetries int                                `json:"max_coder_retries"`
	// MaxReplans is the legacy name for the planner-revision budget; the new
	// spelling is MaxPlanRevisions / json:"max_plan_revisions". Both keys are
	// accepted and either populates the orchestrator's MaxPlanRevisions field.
	// MaxPlanRevisions wins when both are present.
	MaxReplans       int `json:"max_replans"`
	MaxPlanRevisions int `json:"max_plan_revisions"`
	// MaxROTurns lets the user tune the reviewer-orchestrator turn cap from
	// config.json. Default = orchestrator.DefaultMaxROTurns (40).
	MaxROTurns int `json:"max_ro_turns"`
	// BootTimeout is how long to wait for a llama-server instance to
	// respond to /health after spawn. Cold loads of 16-27GB Q4 GGUFs on
	// a 7900 XTX routinely take 60-120s; allow generous headroom by
	// default. Override via config.
	BootTimeoutSeconds int                             `json:"boot_timeout_seconds,omitempty"`
	// ModelsDir is the filesystem directory scanned by GET /models for
	// available GGUF files. Defaults to ~/models.
	ModelsDir string `json:"models_dir,omitempty"`
}

func defaultConfig() Config {
	home, _ := os.UserHomeDir()
	// TODO(rename): consider migrating to ~/.local/share/moirai/ in a future commit;
	// orphaning existing trace data in production today.
	base := filepath.Join(home, ".local", "share", "agent-router")
	// Shared TurboQuant KV + reasoning flags. The llama-cpp-turboquant binary
	// supports -ctk/-ctv turbo3 for KV compression. Reasoning flags apply to
	// the planner and reviewer in particular (they are reasoning models); the
	// coder benefits from -fa and KV compression as well.
	turboArgs := []string{
		"-ctk", "q8_0",
		"-ctv", "turbo3",
		"-fa", "on",
		"--reasoning", "on",
		"--reasoning-format", "deepseek",
		"--reasoning-budget", "-1",
		"-np", "1",
	}
	// Copy helper so each ModelConfig gets its own slice.
	dup := func(in []string) []string {
		out := make([]string, len(in))
		copy(out, in)
		return out
	}
	return Config{
		Port:           5984,
		LlamaServerBin: "/home/aegis/Projects/llama-cpp-turboquant/build/bin/llama-server",
		Models: map[modelmgr.Slot]modelmgr.ModelConfig{
			modelmgr.SlotPlanner: {
				Slot:       modelmgr.SlotPlanner,
				ModelPath:  "/home/aegis/Models/Qwen3.5-27B-Claude-Distill/Qwen3.5-27B-Claude-4.6-Opus-Reasoning-Distilled-Q4_K_M.gguf",
				CtxSize:    262144,
				NGpuLayers: 99,
				Port:       8001,
				ExtraArgs:  dup(turboArgs),
			},
			modelmgr.SlotCoder: {
				Slot:       modelmgr.SlotCoder,
				ModelPath:  "/home/aegis/Models/gpt-oss-20b-bf16.gguf",
				CtxSize:    131072,
				NGpuLayers: 99,
				Port:       8002,
				ExtraArgs:  dup(turboArgs),
			},
			modelmgr.SlotReviewer: {
				Slot:       modelmgr.SlotReviewer,
				ModelPath:  "/home/aegis/Models/Ministral-3-14B-Reasoning/Ministral-3-14B-Instruct-2512-Q4_K_M.gguf",
				CtxSize:    524288,
				NGpuLayers: 99,
				Port:       8003,
				ExtraArgs:  dup(turboArgs),
			},
		},
		ScratchDir:      filepath.Join(base, "scratch"),
		LogDir:          filepath.Join(base, "logs"),
		MaxCoderRetries: 5,
		MaxReplans:      3,
		ModelsDir:       filepath.Join(home, "models"),
	}
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	if path == "" {
		path = os.Getenv("AGENT_ROUTER_CONFIG")
	}
	if path == "" {
		home, _ := os.UserHomeDir()
		// TODO(rename): consider migrating to ~/.config/moirai/ in a future commit;
		// orphaning existing config in production today.
		candidate := filepath.Join(home, ".config", "agent-router", "config.json")
		if _, err := os.Stat(candidate); err == nil {
			path = candidate
		}
	}
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	var override overrideConfig
	if err := json.Unmarshal(data, &override); err != nil {
		return cfg, err
	}
	mergeConfig(&cfg, override)
	return cfg, nil
}

// overrideConfig mirrors Config but with pointer fields for the user-tunable
// scalars so explicit zero values (e.g. `port: 0` to mean "OS-pick port",
// `max_coder_retries: 0` to disable retries) survive JSON decoding instead of
// silently falling back to defaults. Non-pointer fields remain non-pointer
// because their zero value (empty string / nil map) is already a valid
// "use default" sentinel.
type overrideConfig struct {
	Port               *int                                   `json:"port,omitempty"`
	LlamaServerBin     string                                 `json:"llama_server_bin,omitempty"`
	Models             map[modelmgr.Slot]modelmgr.ModelConfig `json:"models,omitempty"`
	DefaultRepo        string                                 `json:"default_repo,omitempty"`
	ScratchDir         string                                 `json:"scratch_dir,omitempty"`
	LogDir             string                                 `json:"log_dir,omitempty"`
	MaxCoderRetries    *int                                   `json:"max_coder_retries,omitempty"`
	MaxReplans         *int                                   `json:"max_replans,omitempty"`
	MaxPlanRevisions   *int                                   `json:"max_plan_revisions,omitempty"`
	MaxROTurns         *int                                   `json:"max_ro_turns,omitempty"`
	BootTimeoutSeconds *int                                   `json:"boot_timeout_seconds,omitempty"`
	ModelsDir          string                                 `json:"models_dir,omitempty"`
}

func mergeConfig(base *Config, o overrideConfig) {
	if o.Port != nil {
		base.Port = *o.Port
	}
	if o.LlamaServerBin != "" {
		base.LlamaServerBin = o.LlamaServerBin
	}
	if o.DefaultRepo != "" {
		base.DefaultRepo = o.DefaultRepo
	}
	if o.ScratchDir != "" {
		base.ScratchDir = o.ScratchDir
	}
	if o.LogDir != "" {
		base.LogDir = o.LogDir
	}
	if o.MaxCoderRetries != nil {
		base.MaxCoderRetries = *o.MaxCoderRetries
	}
	if o.MaxReplans != nil {
		base.MaxReplans = *o.MaxReplans
	}
	if o.MaxPlanRevisions != nil {
		base.MaxPlanRevisions = *o.MaxPlanRevisions
	}
	if o.MaxROTurns != nil {
		base.MaxROTurns = *o.MaxROTurns
	}
	if o.BootTimeoutSeconds != nil {
		base.BootTimeoutSeconds = *o.BootTimeoutSeconds
	}
	if o.ModelsDir != "" {
		base.ModelsDir = o.ModelsDir
	}
	for k, v := range o.Models {
		base.Models[k] = v
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "daemon":
		cmdDaemon(os.Args[2:])
	case "task":
		cmdTask(os.Args[2:])
	case "inspect":
		cmdInspect(os.Args[2:])
	case "abort":
		cmdAbort(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `moirai: three-model coding daemon

usage:
  moirai daemon [--config PATH]
  moirai task "description" [--repo PATH]
  moirai inspect <task_id>
  moirai abort <task_id>
  moirai status

The daemon serves HTTP on the configured port (default 5984).
See README.md and SPEC_DEVIATIONS.md for details.`)
}

// ---- daemon ---------------------------------------------------------------

func cmdDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config JSON")
	fs.Parse(args)

	_ = proctitle.Set("moirai")

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fatal("load config: %v", err)
	}

	if _, err := os.Stat(cfg.LlamaServerBin); err != nil {
		fmt.Fprintf(os.Stderr, "warning: llama-server binary not found at %s\n", cfg.LlamaServerBin)
	}

	// SEC-PASS5-005: state dirs at 0700. Log dir contains llama-server logs
	// which can echo prompts and model output; scratch dir holds task
	// working trees. Neither belongs to other local users.
	if err := os.MkdirAll(cfg.ScratchDir, 0o700); err != nil {
		fatal("mkdir scratch: %v", err)
	}
	_ = os.Chmod(cfg.ScratchDir, 0o700)
	if err := os.MkdirAll(cfg.LogDir, 0o700); err != nil {
		fatal("mkdir log: %v", err)
	}
	_ = os.Chmod(cfg.LogDir, 0o700)

	bootSec := cfg.BootTimeoutSeconds
	if bootSec <= 0 {
		bootSec = 300 // 5 min default; cold 16GB+ Q4 loads can exceed 90s
	}
	mm, err := modelmgr.New(modelmgr.Config{
		LlamaServerBin: cfg.LlamaServerBin,
		Models:         cfg.Models,
		LogDir:         cfg.LogDir,
		BootTimeout:    time.Duration(bootSec) * time.Second,
	})
	if err != nil {
		fatal("modelmgr: %v", err)
	}

	store, err := taskstore.Open(taskstore.DefaultDir())
	if err != nil {
		fatal("taskstore: %v", err)
	}
	if err := store.MarkInterrupted(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: mark interrupted: %v\n", err)
	}

	l2, err := aegis.OpenL2()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: L2 unavailable: %v\n", err)
	}
	defer func() {
		if l2 != nil {
			_ = l2.Close()
		}
	}()

	// Resolve plan-revision budget: prefer the new max_plan_revisions key,
	// fall back to legacy max_replans. Either way the orchestrator sees
	// the value via MaxPlanRevisions.
	planRevisions := cfg.MaxPlanRevisions
	if planRevisions == 0 {
		planRevisions = cfg.MaxReplans
	}
	orch, err := orchestrator.New(orchestrator.Config{
		ModelMgr:         mm,
		Store:            store,
		L2:               l2,
		DefaultRepo:      cfg.DefaultRepo,
		MaxCoderRetries:  cfg.MaxCoderRetries,
		MaxPlanRevisions: planRevisions,
		MaxROTurns:       cfg.MaxROTurns,
		ScratchDir:       cfg.ScratchDir,
	})
	if err != nil {
		fatal("orchestrator: %v", err)
	}

	srv := &api.Server{
		Orch:                orch,
		ModelMgr:            mm,
		StartedAt:           time.Now(),
		Port:                cfg.Port,
		ModelsDir:           cfg.ModelsDir,
		TurboquantSupported: modelmgr.DetectTurboquant(cfg.LlamaServerBin),
		DaemonVersion:       "dev",
	}

	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", cfg.Port),
		Handler: srv.Handler(),
	}

	// Daemon-lifetime lockfile: prevent two daemons from racing on the same
	// state dir (taskstore writes, log rotation, llama-server ports). The
	// home-relative state dir is the one stable invariant across configs;
	// the HTTP port can vary, so port-based conflict detection is too narrow.
	lockPath, lockReleaseFn, lockErr := acquireDaemonLock()
	if lockErr != nil {
		fatal("%v", lockErr)
	}
	defer lockReleaseFn()
	_ = lockPath // referenced for logging if needed

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)

	// Daemon is fully initialized: modelmgr built, taskstore replayed, orchestrator
	// wired. Flip the ready flag so /ready flips to 200.
	srv.ReadyFlag.Store(true)

	// httpErrCh signals the main goroutine when ListenAndServe returns a
	// real error (EADDRINUSE, permission denied, fd exhaustion). Without
	// this, a bind failure was silently logged and the daemon continued
	// blocking on sigc forever -- a zombie process holding state-dir lock
	// without serving HTTP. http.ErrServerClosed is the clean-shutdown
	// signal and never lands on this channel.
	httpErrCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "moirai listening on %s\n", httpSrv.Addr)
		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErrCh <- err
		}
	}()

	select {
	case <-sigc:
		fmt.Fprintln(os.Stderr, "\nmoirai: shutting down")
	case err := <-httpErrCh:
		fmt.Fprintf(os.Stderr, "moirai: http listener failed: %v\n", err)
		// Best-effort cleanup before exiting non-zero.
		if shErr := orch.Shutdown(2 * time.Second); shErr != nil {
			fmt.Fprintf(os.Stderr, "orchestrator shutdown: %v\n", shErr)
		}
		_ = mm.Shutdown()
		lockReleaseFn()
		os.Exit(1)
	}

	// Drain in-flight run goroutines first so trace files, taskstore writes,
	// and llama-server tear-down all happen before httpSrv stops accepting
	// new submissions. 8s budget leaves headroom under the overall 10s
	// graceful window for httpSrv.Shutdown.
	if err := orch.Shutdown(8 * time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "orchestrator shutdown: %v\n", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	_ = mm.Shutdown()
}

// ---- CLI client ----------------------------------------------------------

func cmdTask(args []string) {
	fs := flag.NewFlagSet("task", flag.ExitOnError)
	repo := fs.String("repo", "", "repo root")
	port := fs.Int("port", 5984, "daemon port")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fatal("usage: moirai task \"description\" [--repo PATH]")
	}
	desc := strings.Join(fs.Args(), " ")

	body := map[string]string{"description": desc}
	if *repo != "" {
		abs, err := filepath.Abs(*repo)
		if err != nil {
			fatal("repo path: %v", err)
		}
		body["repo_root"] = abs
	}
	jb, _ := json.Marshal(body)
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/submit", *port), "application/json", bytes.NewReader(jb))
	if err != nil {
		fatal("daemon unreachable on :%d (%v)", *port, err)
	}
	defer resp.Body.Close()
	io.Copy(os.Stdout, resp.Body)
}

func cmdInspect(args []string) {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	port := fs.Int("port", 5984, "daemon port")
	fs.Parse(args)
	if fs.NArg() == 0 {
		fatal("usage: moirai inspect <task_id>")
	}
	id := fs.Arg(0)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/tasks/%s", *port, id))
	if err != nil {
		fatal("daemon unreachable: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(os.Stdout, resp.Body)
}

func cmdAbort(args []string) {
	fs := flag.NewFlagSet("abort", flag.ExitOnError)
	port := fs.Int("port", 5984, "daemon port")
	fs.Parse(args)
	if fs.NArg() == 0 {
		fatal("usage: moirai abort <task_id>")
	}
	id := fs.Arg(0)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/tasks/%s/abort", *port, id), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fatal("daemon unreachable: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(os.Stdout, resp.Body)
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	port := fs.Int("port", 5984, "daemon port")
	fs.Parse(args)

	// If daemon is up, fetch live status.
	if daemonUp(*port) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/status", *port))
		if err == nil {
			defer resp.Body.Close()
			io.Copy(os.Stdout, resp.Body)
			return
		}
	}
	// Daemon down: read the task store directly.
	fmt.Fprintln(os.Stderr, "(daemon not responding on :"+fmt.Sprint(*port)+"; reading task store)")
	store, err := taskstore.Open(taskstore.DefaultDir())
	if err != nil {
		fatal("taskstore: %v", err)
	}
	tasks, err := store.List()
	if err != nil {
		fatal("list tasks: %v", err)
	}
	out, _ := json.MarshalIndent(map[string]any{
		"service":     "agent-router",
		"daemon_up":   false,
		"task_count":  len(tasks),
		"tasks":       tasks,
	}, "", "  ")
	fmt.Println(string(out))
}

func daemonUp(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func fatal(f string, args ...any) {
	fmt.Fprintf(os.Stderr, "moirai: "+f+"\n", args...)
	os.Exit(1)
}

// daemonLockPath returns the path to the daemon's PID file. Lives next to
// the rest of moirai state (~/.local/share/agent-router/daemon.pid)
// so a fresh checkout on a clean machine still works without operator
// setup.
// TODO(rename): consider migrating to ~/.local/share/moirai/ in a future commit;
// orphaning existing daemon state in production today.
func daemonLockPath() string {
	if env := os.Getenv("AGENT_ROUTER_LOCK"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	// TODO(rename): consider migrating to ~/.local/share/moirai/ in a future commit;
	// orphaning existing daemon.pid in production today.
	return filepath.Join(home, ".local", "share", "agent-router", "daemon.pid")
}

// acquireDaemonLock takes a process-exclusive lock by writing PID to a
// file with O_EXCL. If the file already exists and the recorded PID is
// alive, refuse to start; if the recorded PID is dead (stale lock from
// a previous daemon crash), remove the file and try again. Returns the
// lockfile path, a release function that removes the file, and an
// error.
//
// The release function is idempotent so the deferred call in main() and
// the explicit call on the http-error path can both fire without
// double-deleting some other daemon's lockfile.
func acquireDaemonLock() (string, func(), error) {
	path := daemonLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", func() {}, fmt.Errorf("daemon lock: mkdir: %w", err)
	}
	_ = os.Chmod(filepath.Dir(path), 0o700)
	for attempt := 0; attempt < 2; attempt++ {
		// SEC-PASS5-005: lockfile at 0600.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			// REC-2: tighten the O_EXCL window with a flock(LOCK_EX|LOCK_NB)
			// before writing the PID. flock is held by the kernel against the
			// open fd; even if a second daemon's O_EXCL somehow races (it
			// can't on a sane FS but this is defense-in-depth), only one of
			// us holds the advisory lock and the other will EWOULDBLOCK.
			// The lock is released automatically when the fd closes; we keep
			// the fd open for the lifetime of the process by leaking it into
			// the release closure rather than Closing immediately.
			if flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); flockErr != nil {
				f.Close()
				os.Remove(path)
				return "", func() {}, fmt.Errorf("daemon lock: flock: %w", flockErr)
			}
			pid := os.Getpid()
			if _, werr := fmt.Fprintf(f, "%d\n", pid); werr != nil {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
				os.Remove(path)
				return "", func() {}, fmt.Errorf("daemon lock: write pid: %w", werr)
			}
			// fsync the PID so a daemon reading the lockfile during a
			// concurrent acquire never sees a 0-byte file.
			_ = f.Sync()
			myPid := pid
			released := false
			lockedFd := f
			release := func() {
				if released {
					return
				}
				released = true
				// Only remove the file if it still contains OUR pid -- defends
				// against a fast-restart race where another daemon has already
				// taken over the lock.
				data, rerr := os.ReadFile(path)
				if rerr == nil {
					if got, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && got == myPid {
						_ = os.Remove(path)
					}
				}
				// Release the advisory flock and close the fd last.
				_ = syscall.Flock(int(lockedFd.Fd()), syscall.LOCK_UN)
				_ = lockedFd.Close()
			}
			return path, release, nil
		}
		if !os.IsExist(err) {
			return "", func() {}, fmt.Errorf("daemon lock: %w", err)
		}
		// File exists. Read the PID and check liveness.
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return "", func() {}, fmt.Errorf("daemon lock: read existing: %w", rerr)
		}
		pidStr := strings.TrimSpace(string(data))
		pid, perr := strconv.Atoi(pidStr)
		if perr != nil || pid <= 0 {
			// Garbage in lockfile; treat as stale.
			if rmErr := os.Remove(path); rmErr != nil {
				return "", func() {}, fmt.Errorf("daemon lock: remove unparseable lock: %w", rmErr)
			}
			continue
		}
		if pidAlive(pid) {
			return "", func() {}, fmt.Errorf("another daemon already running (pid %d, lockfile %s)", pid, path)
		}
		// Stale lockfile -- previous daemon died without releasing. Remove
		// and retry. Logged so operators can see the recovery.
		fmt.Fprintf(os.Stderr, "moirai: stale lockfile %s (pid %d not alive); removing\n", path, pid)
		if rmErr := os.Remove(path); rmErr != nil {
			return "", func() {}, fmt.Errorf("daemon lock: remove stale: %w", rmErr)
		}
	}
	return "", func() {}, fmt.Errorf("daemon lock: could not acquire %s after retry", path)
}

// pidAlive reports whether the given PID maps to a live process. Uses
// signal 0 (no-op) which on POSIX returns ESRCH for dead processes and
// nil for live ones we can signal. Returns false on EPERM too -- if we
// cannot signal it, we cannot manage it, so treating it as "not ours" and
// refusing to delete the lockfile would just deadlock the operator. The
// alternative (we are root and can signal anything) is fine.
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}
