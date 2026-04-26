# agent-router

A three-model local coding daemon. Planner, Coder, and Reviewer run as
`llama-server` instances on a single 7900 XTX, swapped through VRAM one at a
time. Daily driver for real repo work, not a benchmark toy.

See `SPEC.md` for the long-form design and `SPEC_DEVIATIONS.md` for choices
that diverged from the whiteboard.

## Build

Go 1.26 or newer.

```bash
go build -o bin/agent-router ./cmd/agent-router
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

1. Create a config (optional; defaults are sane for Gary's box):

   ```bash
   mkdir -p ~/.config/agent-router
   cat > ~/.config/agent-router/config.json <<'JSON'
   {
     "port": 5984,
     "llama_server_bin": "/home/aegis/Projects/llama-cpp-turboquant/build/bin/llama-server",
     "default_repo": "/home/aegis/Projects/some-repo",
     "max_coder_retries": 3,
     "max_replans": 1,
     "max_plan_revisions": 3,
     "max_ro_turns": 40,
     "models": {
       "planner":  {"slot": "planner",  "model_path": "/home/aegis/Models/Qwen3.5-27B-Claude-Distill/Qwen3.5-27B-Claude-4.6-Opus-Reasoning-Distilled-Q4_K_M.gguf", "ctx_size": 8192, "n_gpu_layers": 99, "port": 8001},
       "coder":    {"slot": "coder",    "model_path": "/home/aegis/Models/gpt-oss-20b-bf16.gguf",                                                                      "ctx_size": 8192, "n_gpu_layers": 99, "port": 8002},
       "reviewer": {"slot": "reviewer", "model_path": "/home/aegis/Models/Ministral-3-14B-Reasoning/Ministral-3-14B-Instruct-2512-Q4_K_M.gguf",                        "ctx_size": 8192, "n_gpu_layers": 99, "port": 8003}
     }
   }
   JSON
   ```

   The numeric override fields (`port`, `max_coder_retries`, `max_replans`,
   `max_plan_revisions`, `max_ro_turns`, `boot_timeout_seconds`) are decoded
   as JSON pointers so that an explicit `0` is honored as "use this exact
   value" rather than silently falling back to the built-in default. Omit
   the key entirely to use the default.

2. Start the daemon:

   ```bash
   bin/agent-router daemon
   ```

   It listens on `127.0.0.1:5984` (HTTP) and shows up as `agent-router` in
   `btop`/`htop`/`ps`.

3. Submit a task:

   ```bash
   bin/agent-router task "refactor the cache eviction logic to use LRU" --repo ~/Projects/some-repo
   ```

4. Watch it work:

   ```bash
   bin/agent-router inspect <task_id>
   tail -f ~/.local/share/agent-router/traces/<task_id>.jsonl | jq .
   ```

5. When it finishes, the diff lives on `agent-router/task-<id>`. You merge,
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
| `/slots/<id>` | PATCH | Reconfigure a model slot (`model_path`, `ctx_size`, `kv_cache`). Body capped at 64 KiB; unknown fields rejected. |

## Workflow (what the daemon does to a task)

```
         Planner A (VRAM)            |  Coder B and Reviewer C on disk
              |
              | plan
              v
         Reviewer C (VRAM)           |  Planner and Coder on disk
              |
              | approve/reject
              v
         Coder B (VRAM, tool loop)   |  Planner and Reviewer on disk
              |
              | diff
              v
         Reviewer C (VRAM)           |  approve -> commit, reject -> loop to B
              |
              v
         Done: local branch + diff. No push. Ever.
```

Model swaps happen by killing the active `llama-server` and spawning a new
one for the slot we need. See `SPEC_DEVIATIONS.md` for why we don't keep three
servers alive in parallel.

Iteration caps (tunable via `max_coder_retries`, `max_replans` in config):
the coder gets 3 attempts per plan, then the planner revises once, then
the whole task fails and the branch is left for human review.

## Tool loop

When the coder is active, it's asked to emit tool calls wrapped as
`<TOOL>{"name": "...", "args": {...}}</TOOL>`. The daemon executes each call
and feeds the result back as `<RESULT>...</RESULT>`. Available tools:

- `fs.read {"path"}` -> reads a file relative to repo root
- `fs.write {"path", "content"}` -> writes a file
- `fs.search {"pattern", "path"}` -> ripgrep
- `shell.exec {"cmd"}` -> sandboxed shell (bwrap, network off)
- `test.run {}` -> runs the configured test command
- `compile.run {}` -> runs the configured compile command
- `done {"summary"}` -> ends the tool loop

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
- Phase 5/6 (reviewer and coder training): another agent's job. This
  framework loads any GGUF you point the config at, so when the trained
  checkpoints land you just swap paths.

## See also

- `SPEC.md`: full design.
- `SPEC_DEVIATIONS.md`: choices that diverged from the spec and why.
- `smoke-test.sh`: end-to-end sanity script.
