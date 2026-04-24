# agent-router — Build Spec

Working name. Rename at will.

## What this is

A local daily-driver multi-agent coding system. Three specialist open-weight
models orchestrated through a Go daemon on Gary's workstation (Ryzen 9900X,
7900 XTX 24GB, 192GB DDR5). Takes real development tasks, produces real
git branches and diffs, leaves the merge decision to a human.

Success criterion: Gary uses this for actual work on his own repos, commits
it ships are visible in `git log`. NOT a benchmark score.

## Hardware envelope

| | |
|---|---|
| GPU | AMD Radeon RX 7900 XTX, gfx1100, 24 GB VRAM |
| System RAM | 192 GB DDR5-5200 (4x48 GB) |
| CPU | Ryzen 9 9900X (12c/24t) |
| Storage | NVMe (home on NVMe), 40+ GB free required for models resident in DRAM+scratch |
| ROCm | 7.2.1 |
| PyTorch | 2.10 with ROCm |

## Three models (per Gary's whiteboard, 2026-04-22)

| Slot | Model | Role | Ctx | Local path |
|------|-------|------|-----|------------|
| A | Qwen3.5-27B-Claude-4.6-Opus-Reasoning-Distilled-Q4_K_M | Planner | 256k | `/home/aegis/Models/Qwen3.5-27B-Claude-Distill/Qwen3.5-27B-Claude-4.6-Opus-Reasoning-Distilled-Q4_K_M.gguf` |
| B | gpt-oss-20b (MXFP4 native, bf16 activations) | Coder | 128k | `/home/aegis/Models/gpt-oss-20b-bf16.gguf` |
| C | Ministral-3-14B-Reasoning | Reviewer | 512k | `/home/aegis/Models/Ministral-3-14B-Reasoning` (verify at build time) |

Planner A: tool `fs/write`, 3:1 think-to-write ratio. No retraining.
Coder B: must follow A's plan. Needs fine-tune for C/C++/Python (skip ASM).
Reviewer C: reads A's plan AND B's execution. Needs dedicated training (the real gap).

## Workflow (per Gary's whiteboard)

One model VRAM-resident at a time. Other two live in DRAM (mlock'd).
Swap via DRAM→VRAM is ~0.5s over PCIe 5.0 x16 for 12-16GB weights.

```
step | VRAM | DRAM
  1. | A (write plan)   | B, C
  2. | C (review plan)  | A, B
  3. | B (execute)      | A, C
  4. | C (review code)  | A, B
  5. | A (revise)       | B, C
  6. | ...iterate       |
```

Iteration cap: 3 coder retries per plan, then one replan, then fail-logged.

## Architecture

```
                    ┌─────────────────────────────┐
                    │  agent-router daemon (Go)   │
                    │  HTTP :59NN, CLI entry      │
                    └──┬───────────┬───────────┬──┘
                       │           │           │
              ┌────────┴────┐ ┌────┴─────┐ ┌───┴──────┐
              │  Model mgr  │ │ Tool box │ │  AEGIS   │
              │  (VRAM/DRAM │ │  fs/git/ │ │  L1/L2/L3│
              │   swap)     │ │  test/   │ │  bridge  │
              │             │ │  shell,  │ │          │
              │             │ │  sandbox)│ │          │
              └──┬──────────┘ └──────────┘ └──────────┘
                 │
    ┌────────────┼────────────┐
    │            │            │
  llama-server  llama-server  llama-server
  :8001 (A)     :8002 (B)     :8003 (C)
```

Each model lives as its own `llama-server` process (llama-cpp-turboquant
binary, TurboQuant KV cache enabled where applicable). Daemon tracks which
is currently VRAM-resident and issues load/unload via swap operations.

### Extraction target

Forge's routing/orchestration framework already exists:
- `/home/aegis/Projects/forge/src/lib/model-router.ts`
- `/home/aegis/Projects/forge/src/lib/session-orchestrator.ts`

Port the TS logic to Go. Don't lift verbatim; restructure for long-running
daemon semantics. Keep the session-based abstractions (each task = a session),
the routing decisions (which model handles which phase), and the structured
output parsers.

## Tool loop (coder only)

The coder B is the only agent with tool access. Planner A has `fs/write` but
only writes plans, not code. Reviewer C is read-only.

Tools:
- `fs.read(path)` — bounded to repo root
- `fs.write(path, content)` — diff-tracked
- `fs.search(pattern, path)` — ripgrep under the hood
- `git.status()`, `git.diff()`, `git.branch()`, `git.commit()` — no `git push`, no `git reset --hard`
- `test.run(cmd)` — per-repo test command from config
- `compile.run(cmd)` — per-repo compile command
- `shell.exec(cmd)` — sandboxed via bwrap or cgroup, rooted at repo dir, network-off by default

All tool calls logged as JSONL for tracing.

## AEGIS integration

Persistent memory across tasks and repos. Three tiers:

- **L1 (hot, in-process):** current task state, current plan, most recent tool output.
  Token-budgeted to ~10% of active model's context.
- **L2 (per-repo, on-disk):** codebase structure, prior patches on this repo,
  style/lint rules learned, common error patterns.
- **L3 (cross-repo, cold):** patterns Gary prefers, common tool invocations,
  agent decisions that worked vs didn't.

Reviewer writes verdicts and reasoning to L2/L3. Planner reads L2/L3 for
repo context on task start. Coder reads L1 live during iteration.

Use existing Pensive/Engram plumbing via `engram-emit` CLI or the MCP tools.

## Per-repo config

`.agent-router.toml` at repo root:

```toml
[commands]
test = "pytest -x"
compile = "make"
lint = "ruff check ."

[style]
language = "python"
line_length = 100
# free-form style notes for the planner/reviewer

[forbidden]
# never touch these paths
paths = ["secrets/", ".env"]

[budget]
max_runtime = "30m"
max_iterations = 6
```

## Operational requirements (non-negotiable)

These are what make it a daily driver vs. a demo:

1. **Diff-gate.** Never auto-push. Final artifact is a local branch + diff. Human reviews and merges.
2. **Interrupt/inspect.** `agent-router inspect <task_id>` shows current phase, active model, last 20 tool calls. `agent-router abort <task_id>` kills cleanly, state persists for postmortem.
3. **Resume.** If daemon crashes mid-task, on restart task resumes at last phase boundary. State serialized per phase.
4. **Sandbox.** Shell + file operations jailed to repo root via `bwrap` or cgroup. Network off by default. Whitelistable per-repo if needed.
5. **Trace.** JSONL log per task at `~/.local/share/agent-router/traces/<task_id>.jsonl`. Every LLM call, every tool invocation, every verdict. Tail-able.
6. **Budget enforcement.** Wall-clock budget kills runaway tasks. Token/iteration budgets per model.
7. **Observability hooks.** Emit to nerve-center (:5960), kairos, and charon. Session lifecycle integrated with existing Aegis infrastructure.

## Reviewer (C) training

Base: Ministral-3-14B-Reasoning.

Training signal: verifier-grounded rewards. Given `(original_code, proposed_patch, test_suite)`, train the reviewer to:
1. Predict test pass/fail (binary classification head)
2. Generate the specific failure reason when predicting fail
3. Identify reasoning gaps in the planner's plan given the code produced

Data mix (target ~30-40K training samples post-filtering):
- 60% SWE-Bench-Train (non-held-out split)
- 25% multi-language bug-injection synthetic pairs (C/C++/Python)
- 10% high-quality PR review comments scraped from top-tier OSS
  (llama.cpp, linux, redis, sqlite, etc.)
- 5% style/lint review pairs

Optional SFT warm-start on Microsoft CodeReviewer dataset (~1.1M samples) to
teach review format before GRPO.

Training framework: Mud-Puppy. Quant mode: `qat_rocm.py` or `bnb_rocm.py`
depending on base model's format. Evaluate both; pick whichever gives higher
isolated reviewer accuracy on held-out SWE-Bench.

Hardware: 7900 XTX local, overnight. 2-3 nights total including SFT warm-start.

## Coder (B) training

Base: gpt-oss-20b MXFP4 native.
Training framework: Mud-Puppy `mxfp4_rocm.py` in-place (no dequant-retrain-requant).
Training signal: GRPO with compile-and-test rewards.

Data sources per language:
- **C/C++:** llama.cpp commits (ggml-org/llama.cpp), Redis, SQLite, LevelDB.
  Filter for commits that touch small number of files and include tests.
- **Python:** existing Python strength is fine; minimal fine-tune if any.
  Could skip entirely.
- **ASM:** DROPPED per Gary's call.
- **CSS:** skip, base is fine.

Size: 5-15K trajectories (C/C++), total ~15-20K after mixing.
Hardware: 7900 XTX local, overnight. 1-2 nights.

## Training data sets directory

All data lives under `/home/aegis/Projects/mud-puppy/training_data_sets/`.

Layout:
```
training_data_sets/
├── reviewer/
│   ├── swe-bench-train.jsonl
│   ├── bug-injection-multilang.jsonl
│   ├── pr-reviews-osint.jsonl
│   ├── codereviewer-sft-warmup.jsonl
│   └── README.md
├── coder/
│   ├── llamacpp-commits.jsonl
│   ├── redis-commits.jsonl
│   ├── sqlite-commits.jsonl
│   ├── leveldb-commits.jsonl
│   └── README.md
└── README.md     # top-level manifest: sizes, sources, licensing
```

Existing files already in this dir that predate this spec:
- `aegis-reflib-qlora.jsonl` — AEGIS reference library QLoRA data (do not touch, unrelated)
- `opus46_final.jsonl` — Opus 4.6 distill data (do not touch, unrelated)

## Build phases

| Phase | Deliverable | Parallelizable |
|-------|-------------|----------------|
| 1 | Go daemon skeleton + model manager + llama-server wiring | yes (Agent R) |
| 2 | Training data curated into `training_data_sets/` | yes (Agent D) |
| 3 | Mud-Puppy audit + smoke-test training pipeline works | yes (Agent T) |
| 4 | Tool box + AEGIS integration + sandbox | after phase 1 |
| 5 | Reviewer training run | after phase 2 + 3 |
| 6 | Coder training run | after phase 2 + 3 |
| 7 | Diff-gate, trace, inspect/abort, per-repo config | after phase 4 |
| 8 | Dogfooding on real repos. This is the portfolio artifact. | ongoing |

## Hard constraints

- No em dashes in any written output, anywhere. Gary has sworn datacenter retribution.
- No `rm -rf`, no `git push --force`, no destructive ops without USER provenance.
- Subagents cannot assess safety of destructive operations; they report findings only.
- Mud-Puppy has uncommitted work (scripts/, training_data_sets/ added post-power-outage).
  Verify features work before using them. Don't commit on Gary's behalf.
- Preserve existing `aegis-reflib-qlora.jsonl` and `opus46_final.jsonl` in training_data_sets/.

## Success looks like

A git repo at `~/Projects/agent-router/` containing:
- Working Go daemon (`cmd/agent-router/main.go` builds and runs)
- Three llama-server configs for A/B/C with hot-swap
- Tool box with sandbox
- AEGIS integration
- Trained reviewer checkpoint
- Trained coder checkpoint
- README with one-command install and first-task walkthrough
- Trace logs from at least one real task Gary ran end-to-end

And a line Gary can say in an interview:
> "I built a three-model agentic coding system. Runs on my gaming PC. Open
> weights. I custom-trained the reviewer in native MXFP4 on ROCm. Here's
> the repo. Last three commits on kernel-anvil were authored by the agent."
