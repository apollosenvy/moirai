# Moirai

A three-model local coding daemon. **Planner**, **Coder**, and
**Reviewer-Orchestrator** run as `llama-server` instances on a single GPU,
swapped through VRAM one at a time. The Reviewer drives an LLM-orchestrated
tool loop, calls the Planner and Coder when it needs them, runs `compile.run`
and `test.run` against the working tree, and emits `done` or `fail` when the
artifact is deliverable (or definitely isn't).

Daily driver for real repo work on Gary's box. Not a benchmark toy.

```
   user task
       │
       ▼
   ┌──────────────────────────────────────────────────────────┐
   │              Reviewer-Orchestrator (RO loop)             │
   │   ask_planner · ask_coder · fs.* · test.run · compile.run│
   │           pensive.search · done · fail                   │
   └────────┬───────────────────────┬─────────────────────────┘
            │ ask_planner           │ ask_coder
            ▼                       ▼
        Planner                  Coder
        (one-shot plan)          (writes diffs against
                                  working tree, gated by
                                  acceptance criteria)
            │                       │
            └─────── single GPU ────┘
                  one model VRAM-resident at a time
```

See `SPEC.md` for the long-form design and `SPEC_DEVIATIONS.md` for choices
that diverged from the whiteboard. The 2026-04-27/28 reviewer optimization
study (which model goes in which slot, what the failure modes are, and the
path to a 90/100 reviewer) lives in `MOIRAI_OPTIMIZATION_TESTING.md`.

Target hardware: AMD 7900 XTX (24 GB) with ROCm + a custom `llama-server`
(`llama-cpp-turboquant`) for `turbo3` KV-cache compression; runs anywhere
`llama-server` runs but the swap budget assumes a single fast GPU.

## Build

Go 1.26 or newer.

```bash
go build -o bin/moirai ./cmd/moirai
```

Produces a single ~14 MB static binary. No external runtime deps beyond
`llama-server`, `git`, `rg`, and (optionally) `bwrap`.

### Diagnostic tools (rematch postmortem trio)

Three small CLIs sit beside the daemon for inspecting and comparing
rematch trace files (`~/.local/share/agent-router/traces/<task-id>.jsonl`):

```bash
go build -o bin/trace-tail    ./cmd/trace-tail      # live single-line stream
go build -o bin/trace-summary ./cmd/trace-summary   # post-mortem rollup (one page)
go build -o bin/trace-diff    ./cmd/trace-diff      # side-by-side comparison
```

Typical rematch debug session:

```bash
# In one terminal: watch a running task
trace-tail ~/.local/share/agent-router/traces/<task-id>.jsonl

# After the task ends: see the rollup
trace-summary ~/.local/share/agent-router/traces/<task-id>.jsonl

# After a fix landed: confirm the new run made it further
trace-diff <baseline-task>.jsonl <candidate-task>.jsonl
```

## Quickstart

1. Create a config (optional; defaults are sane). The setup below mirrors
   the post-`MOIRAI_OPTIMIZATION_TESTING.md` recommendation — Qwen3.5-27B
   for planning, `gpt-oss-20b` Q-quant in BOTH coder and reviewer slots
   with the safe-mode spawn flags that work around the `openai_moe_iswa`
   graph-reserve crash on default args:

   ```bash
   mkdir -p ~/.config/agent-router
   cat > ~/.config/agent-router/config.json <<'JSON'
   {
     "port": 5984,
     "llama_server_bin": "/home/aegis/Projects/llama-cpp-turboquant/build/bin/llama-server",
     "default_repo": "/home/aegis/Projects/some-repo",
     "models_dir": "/home/aegis/Models",
     "max_coder_retries": 5,
     "max_plan_revisions": 3,
     "max_ro_turns": 40,
     "models": {
       "planner": {
         "slot": "planner",
         "model_path": "/home/aegis/Models/Qwen3.5-27B-Claude-Distill/Qwen3.5-27B-Claude-4.6-Opus-Reasoning-Distilled-Q4_K_M.gguf",
         "ctx_size": 32768,
         "kv_cache": "turbo3",
         "n_gpu_layers": 99,
         "port": 8001,
         "extra_args": ["-fa","on","--reasoning","on","--reasoning-format","deepseek","--reasoning-budget","-1","-np","1"]
       },
       "coder": {
         "slot": "coder",
         "model_path": "/home/aegis/Models/gpt-oss/gpt-oss-20b.gguf",
         "ctx_size": 16384,
         "kv_cache": "f16",
         "n_gpu_layers": 99,
         "port": 8002,
         "extra_args": ["-fa","off","--no-warmup","-np","1"]
       },
       "reviewer": {
         "slot": "reviewer",
         "model_path": "/home/aegis/Models/gpt-oss/gpt-oss-20b.gguf",
         "ctx_size": 32768,
         "kv_cache": "f16",
         "n_gpu_layers": 99,
         "port": 8003,
         "extra_args": ["-fa","off","--no-warmup","-np","1"]
       }
     }
   }
   JSON
   ```

   The numeric override fields (`port`, `max_coder_retries`, `max_replans`,
   `max_plan_revisions`, `max_ro_turns`, `boot_timeout_seconds`) are decoded
   as JSON pointers so an explicit `0` is honored as "use this exact value"
   rather than silently falling back to the built-in default. Omit the key
   entirely to use the default. `models_dir` (added 2026-04-28) is scanned
   recursively for the `/models` picker.

2. Start the daemon:

   ```bash
   bin/moirai daemon
   ```

   It listens on `127.0.0.1:5984` (HTTP) and shows up as `moirai` in
   `btop`/`htop`/`ps`.

3. Submit a task:

   ```bash
   bin/moirai task "refactor the cache eviction logic to use LRU" --repo ~/Projects/some-repo
   ```

4. Watch it work:

   ```bash
   bin/moirai inspect <task_id>
   tail -f ~/.local/share/agent-router/traces/<task_id>.jsonl | jq .
   ```

5. When it finishes, the diff lives on `moirai/task-<id>`. You merge,
   you rebase, you throw it away. The daemon never pushes.

## Subcommands

| Command | What it does |
|---------|--------------|
| `daemon [--config PATH]` | Run the HTTP daemon |
| `task "desc" [--repo PATH]` | Submit a task, get back a task id |
| `inspect <task_id>` | Dump task state + last 20 trace events |
| `abort <task_id>` | Stop a running task, preserve state |
| `status` | List all tasks and daemon health |

All client commands talk HTTP to the daemon by default. `status` falls back
to reading the task store on disk if the daemon is down.

## HTTP API

| Route | Method | Purpose |
|-------|--------|---------|
| `/health` | GET | Liveness check |
| `/status` | GET | Daemon + active slot + task counts |
| `/tasks` | GET | List all tasks |
| `/tasks/<id>` | GET | Full task record + last 20 events |
| `/tasks/<id>/abort` | POST | Stop a task. 409 if task is already in a terminal state. |
| `/tasks/<id>/inject` | POST | `{"message": "..."}` - queue a user-authored guidance message for the running RO loop. 404 if unknown, 409 if terminal, 400 on empty message. Body capped at 256 KiB. |
| `/tasks/<id>/interrupt` | POST | Soft interrupt: queues a fixed "stop your current line of reasoning" nudge. 404 if unknown, 409 if terminal. |
| `/submit` | POST | `{"description": "...", "repo_root": "..."}`. Body capped at 256 KiB; unknown JSON fields rejected with 400. |
| `/slots` | GET | View all slot configs and runtime status |
| `/slots/<id>` | PATCH | Reconfigure a model slot (`model_path`, `ctx_size`, `kv_cache`). Body capped at 64 KiB; unknown fields rejected. |
| `/models` | GET | List GGUFs under `models_dir` (recursive, depth-capped) plus any active slot paths |

## Workflow (what the daemon does to a task)

The Reviewer-Orchestrator drives the loop end-to-end. Planner and Coder
are LLM-callable tools, not separate phases — the Reviewer decides when to
ask each, when to verify, and when to declare `done` or `fail`.

```
   Reviewer (VRAM, tool loop)
        │
        ├── ask_planner         → Planner produces plan text
        ├── ask_coder           → Coder writes diffs against working tree
        ├── fs.read / fs.write  → direct edits without the coder
        ├── fs.search           → ripgrep within the repo
        ├── compile.run         → run [commands].compile from .agent-router.toml
        ├── test.run            → run [commands].test
        ├── pensive.search      → recall reasoning atoms from past tasks
        ├── pensive.emit_atom   → write a discovery/failure/insight atom
        ├── done(summary)       → gated by acceptance criteria
        └── fail(reason)        → ends the loop with a reason
```

Acceptance gating: `done` is rejected when the planner-supplied acceptance
criteria are not satisfied. The reviewer then has to either run the actual
mechanical checks (compile/test) and try again, or call `fail` honestly.
This is the "stop the reviewer from lying about completion" guard rail.

Model swaps happen by killing the active `llama-server` and spawning a new
one for the slot we need. See `SPEC_DEVIATIONS.md` for why we don't keep three
servers alive in parallel. Slot reuse is liveness-checked: before reusing a
slot reported as `loaded=true`, the manager pings `/v1/models` with a 1-second
timeout, and respawns on failure.

Iteration caps (tunable via `max_coder_retries`, `max_plan_revisions`, and
`max_ro_turns` in config). When the RO loop nears its budget — turn `N-8` or
8 minutes of wall-clock left — the orchestrator injects a soft directive
demanding `done` or `fail` rather than starting new work. At `N-2` or 2
minutes left the directive becomes a hard demand. This is the "force-conclude"
mechanism that keeps the reviewer from running out the clock without ever
declaring an outcome.

File writes honor `.agent-router.toml`'s `[forbidden].paths` list. Paths
outside the repo root are always rejected.

## Per-repo config

Drop `.agent-router.toml` at the root of any repo the daemon should touch:

```toml
[commands]
test = "pytest -x"
compile = "make"
lint = "ruff check ."

[style]
language = "python"
line_length = 100

[forbidden]
paths = ["secrets/", ".env"]

[budget]
max_runtime = "30m"
max_iterations = 6
```

Missing sections use defaults (30 min wall-clock, 6 iterations). Missing file
is fine; the daemon warns and uses defaults.

## Sandbox

`shell.exec`, `test.run`, and `compile.run` run under `bwrap` with:

- `/usr`, `/etc`, `/lib`, `/lib64`, `/bin`, `/sbin`, `/opt` read-only
- Repo root read-write, `--chdir` into it
- Scratch dir at `~/.local/share/agent-router/scratch/` read-write
- `--unshare-net` (no network) by default
- `--unshare-pid`, `--unshare-ipc`, `--unshare-uts`

If `bwrap` is missing the daemon falls back to an unsandboxed exec and
warns on stderr. Don't run untrusted tasks with `bwrap` missing.

## AEGIS memory

- **L1:** current task state, lives in the orchestrator struct.
- **L2:** per-repo SQLite at `~/.local/share/agent-router/repo-memory.db`.
  Facts and verdicts keyed by repo root.
- **L3:** `engram-emit` for writes, `pensive-recall` for reads. Cross-repo
  insights get pushed through the existing Pensive plumbing. No special
  MCP client; the CLI is the contract.

## Traces

Every task writes a JSONL trace to `~/.local/share/agent-router/traces/<id>.jsonl`.
One event per line. Tailable and greppable:

```bash
tail -f ~/.local/share/agent-router/traces/<id>.jsonl | jq 'select(.kind=="tool_call")'
```

Event kinds: `phase`, `swap`, `llm_call`, `tool_call`, `verdict`, `error`,
`info`, `done`.

## Smoke test

Two smoke tests live in the repo root:

- `./smoke-test.sh` builds the binary, spins up a throwaway git repo, and
  runs the daemon against small placeholder GGUFs (tinyllama, qwen3-8b,
  phi3-mini) to verify the A-C-B-C loop end to end. Expect ~60 seconds wall
  clock on a warm kernel cache. Requires real model files. Output lands in
  `/tmp/agent-router-smoke.log`.
- `./smoke-test-ro.sh` is the Reviewer-Orchestrator (RO) loop smoke test.
  It needs no real LLMs; three tiny Python HTTP stubs mimic
  llama-server's chat-completions endpoint with canned responses, then
  the full daemon runs against them. Verifies submit + RO loop reaches
  ask_planner / ask_coder / fs.write / test.run / done and that the trace
  records `ro_tool_call`, `p_reply`, `c_reply` events. Stub ports are
  picked dynamically by default; set `AR_SMOKE_PORT_BASE=18801` to pin a
  fixed three-port block.

## State layout

| Path | Purpose |
|------|---------|
| `~/.local/share/agent-router/tasks/` | Per-task JSON state (resume after restart) |
| `~/.local/share/agent-router/traces/` | Per-task JSONL traces |
| `~/.local/share/agent-router/logs/` | Per-slot llama-server stdout/stderr |
| `~/.local/share/agent-router/scratch/` | Writable dir exposed to the sandbox |
| `~/.local/share/agent-router/repo-memory.db` | L2 per-repo facts |
| `~/.config/agent-router/config.json` | Optional config override |

## What this will NOT do

- `git push` or `git push --force`. Ever.
- `git reset --hard`, `git commit --amend`, `git rebase -i`, or any op that
  rewrites Gary's existing history.
- Delete files.
- Reach the network from inside the sandbox by default.
- Touch paths outside the repo root.

## Status

- Phase 1 (daemon + model manager): done.
- Phase 4 (tools + AEGIS + sandbox): done.
- Phase 7 (diff-gate + trace + inspect/abort + per-repo config): done.
- Reviewer optimization (which model goes where, why the current
  defaults are what they are): documented in
  `MOIRAI_OPTIMIZATION_TESTING.md`. Best reviewer score 82/100 with the
  default harness, +15 from the F8/F9 changes that landed in this branch;
  path to 90 sketched in `docs/superpowers/plans/2026-04-28-moirai-multi-gpu.md`.
- Phase 5/6 (reviewer and coder training): another agent's job. This
  framework loads any GGUF you point the config at, so when trained
  checkpoints land you just swap paths.

## Naming

Moirai was previously called `agent-router`. The on-disk filesystem paths
are intentionally still `agent-router`-named:

| Old name | Why it still exists |
|----------|---------------------|
| `~/.config/agent-router/config.json` | Config file. Renaming would break every operator's setup. |
| `~/.local/share/agent-router/{tasks,traces,logs,scratch}/` | Years of trace data live here. |
| `.agent-router.toml` | Per-repo config in already-published repos. |

The daemon binary, the Phos UI shell, and the public-facing project name
are all "Moirai." Path migration to `~/.config/moirai/` etc. is a future
chore tracked separately. Until then, the old paths are the single source
of truth.

## See also

- `SPEC.md` — full design.
- `SPEC_DEVIATIONS.md` — choices that diverged from the spec and why.
- `MOIRAI_OPTIMIZATION_TESTING.md` — 11-reviewer bake-off, rubric, harness
  experiment, path to 90/100.
- `docs/superpowers/plans/2026-04-28-moirai-multi-gpu.md` — multi-GPU
  implementation plan (R9700 dual + per-slot pinning).
- `smoke-test.sh` — end-to-end sanity script (real GGUFs).
- `smoke-test-ro.sh` — RO-loop smoke test (no real LLMs; Python stubs).
