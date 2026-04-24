// agent-router: three-model coding daemon.
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
//
// Defaults live in defaultConfig() below; they target Gary's Aegis box.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aegis/agent-router/internal/aegis"
	"github.com/aegis/agent-router/internal/api"
	"github.com/aegis/agent-router/internal/modelmgr"
	"github.com/aegis/agent-router/internal/orchestrator"
	"github.com/aegis/agent-router/internal/proctitle"
	"github.com/aegis/agent-router/internal/taskstore"
)

type Config struct {
	Port           int                                 `json:"port"`
	LlamaServerBin string                              `json:"llama_server_bin"`
	Models         map[modelmgr.Slot]modelmgr.ModelConfig `json:"models"`
	DefaultRepo    string                              `json:"default_repo"`
	ScratchDir     string                              `json:"scratch_dir"`
	LogDir         string                              `json:"log_dir"`
	MaxCoderRetries int                                `json:"max_coder_retries"`
	MaxReplans     int                                 `json:"max_replans"`
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
	var override Config
	if err := json.Unmarshal(data, &override); err != nil {
		return cfg, err
	}
	mergeConfig(&cfg, override)
	return cfg, nil
}

func mergeConfig(base *Config, o Config) {
	if o.Port != 0 {
		base.Port = o.Port
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
	if o.MaxCoderRetries != 0 {
		base.MaxCoderRetries = o.MaxCoderRetries
	}
	if o.MaxReplans != 0 {
		base.MaxReplans = o.MaxReplans
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
	fmt.Fprintln(os.Stderr, `agent-router: three-model coding daemon

usage:
  agent-router daemon [--config PATH]
  agent-router task "description" [--repo PATH]
  agent-router inspect <task_id>
  agent-router abort <task_id>
  agent-router status

The daemon serves HTTP on the configured port (default 5984).
See README.md and SPEC_DEVIATIONS.md for details.`)
}

// ---- daemon ---------------------------------------------------------------

func cmdDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config JSON")
	fs.Parse(args)

	_ = proctitle.Set("agent-router")

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fatal("load config: %v", err)
	}

	if _, err := os.Stat(cfg.LlamaServerBin); err != nil {
		fmt.Fprintf(os.Stderr, "warning: llama-server binary not found at %s\n", cfg.LlamaServerBin)
	}

	if err := os.MkdirAll(cfg.ScratchDir, 0o755); err != nil {
		fatal("mkdir scratch: %v", err)
	}
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		fatal("mkdir log: %v", err)
	}

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

	orch := orchestrator.New(orchestrator.Config{
		ModelMgr:         mm,
		Store:            store,
		L2:               l2,
		DefaultRepo:      cfg.DefaultRepo,
		MaxCoderRetries:  cfg.MaxCoderRetries,
		MaxPlanRevisions: cfg.MaxReplans, // config's "max_replans" maps to plan-revision budget
		ScratchDir:       cfg.ScratchDir,
	})

	srv := &api.Server{
		Orch:      orch,
		ModelMgr:  mm,
		StartedAt: time.Now(),
		Port:      cfg.Port,
		ModelsDir: cfg.ModelsDir,
	}

	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", cfg.Port),
		Handler: srv.Handler(),
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)

	// Daemon is fully initialized: modelmgr built, taskstore replayed, orchestrator
	// wired. Flip the ready flag so /ready flips to 200.
	srv.ReadyFlag.Store(true)

	go func() {
		fmt.Fprintf(os.Stderr, "agent-router listening on %s\n", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "http: %v\n", err)
		}
	}()

	<-sigc
	fmt.Fprintln(os.Stderr, "\nagent-router: shutting down")

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
		fatal("usage: agent-router task \"description\" [--repo PATH]")
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
		fatal("usage: agent-router inspect <task_id>")
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
		fatal("usage: agent-router abort <task_id>")
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
	fmt.Fprintf(os.Stderr, "agent-router: "+f+"\n", args...)
	os.Exit(1)
}
