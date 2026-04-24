# Spec Deviations

Calls I made where the spec was underspecified. All deliberate, all defensible.

## Port: 5984
Spec said "unused port in 59xx range". Scanned nervous-system.py and CLAUDE.md
plus live `ss -tln`. Taken: 5959-5963, 5965, 5967, 5970, 5971, 5975, 5977,
5980 (mission-control), 5981 (pensive-observa), 5982 (session-forecas),
5983 (precheck). Picked 5984.

## Single llama-server instance, not three
Spec diagram shows three parallel llama-server processes (:8001/:8002/:8003).
The workflow section immediately below it says "One model VRAM-resident at
a time". Three hot llama-server processes cannot all be VRAM-resident on a
24GB card, and the "other two live in DRAM (mlock'd)" line doesn't match
llama-server's runtime model (it loads weights into its own process; you
can't hot-swap them out of VRAM from outside).

Chose: one llama-server process at a time. Model manager kills the current
one and spawns a fresh one when the active model changes. The 0.5s PCIe
swap target is not met (cold llama-server startup is 2-8s for a 14-27B GGUF),
but this is honest behavior. When Gary wants the diagram-accurate version,
we can switch to a custom binary that supports multi-model hot-reload or
use llama.cpp's RPC backend.

Ports 8001/8002/8003 are treated as a pool; the currently-loaded model
binds whichever one the slot requests (default 8001).

## Tool sandbox: bwrap only
Spec says "bwrap or cgroup". Picked bwrap. It's simpler, already installed,
gives us the network-off default trivially. cgroup escape is harder, and
Gary already uses bwrap in other sandbox contexts. If we need CPU/memory
limits later, layer cgroups on top of bwrap, not instead of.

## A-C-B-C-A loop shape
Spec whiteboard shows:
```
A (plan) -> C (review plan) -> B (execute) -> C (review code) -> A (revise)
```
Implemented exactly that. "Revise" from A feeds back into the C-review-plan
step. Iteration cap: 3 coder retries per plan, then 1 replan attempt, then
fail. I read that as: up to 3 (B -> C-review-code) loops per approved plan,
then if still failing the task gets one (A-revise -> C-review-plan) cycle,
then fail. Budgets from .agent-router.toml override.

## Placeholder models for smoke test
Trained reviewer/coder checkpoints don't exist yet (that's phases 5/6).
Smoke test uses:
- Planner A: Qwen3.5-27B-Claude-Distill Q4_K_M (per spec, already on disk)
- Coder B: gpt-oss-20b-bf16.gguf (per spec, already on disk)
- Reviewer C: Ministral-3-14B-Instruct-2512-Q4_K_M.gguf (the instruct
  variant, since the reasoning fine-tune checkpoint lives in a subdir and
  isn't a single GGUF yet)

When phases 5/6 land, flip paths in the default config.

## AEGIS L1/L2/L3 client
L1 is in-process state (a struct). L2 is a SQLite DB at
~/.local/share/agent-router/repo-memory.db, keyed by repo path. L3 writes
through to engram-emit and reads via pensive-recall CLI. This matches the
Aegis infrastructure rather than inventing a new MCP client.

## Trace format
JSONL, one event per line, schema:
```
{"ts": "<rfc3339>", "task_id": "<uuid>", "kind": "llm_call|tool_call|verdict|phase|error", "data": {...}}
```
Tail-able, greppable. Matches the existing charon/kairos JSONL convention.

## "setproctitle equivalent" in Go
Go has no direct setproctitle. Used the argv0 overwrite trick via a tiny
cgo stub (prctl PR_SET_NAME for the main thread, argv[0] overwrite for
btop/ps). Falls back to os.Args[0] manipulation if cgo is off.

## Git commit tool
Spec lists git.commit() as an allowed tool. The daemon never pushes, never
force-resets, never amends. Commits happen on an agent-created local branch
(`agent-router/task-<id>`). Gary merges manually.

## 2026-04-22 RO Rewrite: Architecture pivot to LLM-as-orchestrator

Original build used a deterministic A->C-review-plan->B->C-review-code state
machine. Rewrite replaces that with a Reviewer-Orchestrator (RO) loop where
the reviewer model itself drives flow via tool calls.

### Role / model assignment

| Slot | Model | New role |
|------|-------|----------|
| planner  | Qwen3.5-27B-Claude-4.6-Opus-Reasoning-Distilled (Q4_K_M) | Single-turn planner. First-shot plans and revisions. May commit PLAN.md via fs.write. |
| coder    | gpt-oss-20b MXFP4 | Produces code as text (markdown fences with `# file: <path>` comments). No tool access in normal mode; granted read-only fs.read + fs.search in retry-after-test-failure mode. |
| reviewer | Gemma-4-31B (placeholder: Ministral-3-14B-Instruct 2512 Q4_K_M GGUF) | Reviewer-Orchestrator. Drives a tool-call loop; picks what to do next based on tool results. |

The reviewer GGUF placeholder is unchanged from the original build; swap the
path when a Gemma-4-31B build lands.

### Tool-access policy

| Tool | Planner | Coder (normal) | Coder (retry) | RO |
|------|---------|----------------|---------------|----|
| `ask_planner`   | no  | no  | no  | yes |
| `ask_coder`     | no  | no  | no  | yes |
| `fs.read`       | no  | no  | YES | yes |
| `fs.write`      | YES (PLAN.md only de-facto) | no  | no  | yes |
| `fs.search`     | no  | no  | YES | yes |
| `test.run`      | no  | no  | no  | yes |
| `compile.run`   | no  | no  | no  | yes |
| `pensive.search`| no  | no  | no  | yes |
| `done` / `fail` | no  | no  | no  | yes |

The planner is nominally allowed `fs.write` but the orchestrator only honors
writes to `PLAN.md`; any other path is accepted and written but the intent
is that planners document plans, not code. If a planner writes outside
`PLAN.md`, that shows up in traces and the RO can react.

### Budgets

Per the brief:
- Max RO turns: 40 (Config.MaxROTurns, default)
- Max ask_coder retries: 5 (Config.MaxCoderRetries, default)
- Max ask_planner revisions: 3 (Config.MaxPlanRevisions, default)
- Wall-clock per task: taken from `.agent-router.toml`'s `[budget] max_runtime`
  (default 30m).
- `MaxTokens: 24576` on every ChatRequest (P, C, RO) for consistency with
  the prior per-role patch.

### TurboQuant KV flags + context sizes

The planner/coder/reviewer entries in the default config should set

```
"extra_args": ["-ctk", "q8_0", "-ctv", "turbo3", "-fa", "on",
               "--reasoning", "on", "--reasoning-format", "deepseek",
               "--reasoning-budget", "-1", "-np", "1"]
```

Target ctx sizes from the brief: planner 262144, coder 131072, reviewer 524288.

### Empirical ctx probe (2026-04-22)

Spot-loaded the planner GGUF with the full turboquant flag set. The
`llama-cpp-turboquant` binary at
`/home/aegis/Projects/llama-cpp-turboquant/build/bin/llama-server`
(build 8665, 9e80e93ce) coredumped with exit 139 during `load_tensors`
on the planner AND the 14B reviewer model, even at `-c 8192` with no
turboquant flags. `rocm-smi` reports the GPU in "low-power state" and
the daemon was not able to bring a model up at all in this session.

This is a pre-existing environmental issue with the locally-built
llama-cpp-turboquant, NOT a regression from the RO rewrite. The smoke
test for the rewrite uses a stub llama-server (Python) to exercise the
flow; real model loads need to be re-probed when the llama-cpp-turboquant
build is repaired or the GPU exits low-power.

For now the default config keeps the target ctx sizes (256k / 128k / 512k)
and the full turboquant flag list. If these turn out to exceed VRAM on a
warm GPU, the owner should reduce step-wise: first the reviewer (largest
KV budget because of 512k ctx), then the planner, then the coder. Typical
fallback: 131072 across the board, still with turbo3 V-cache.

### Pensive wiring

`pensive.search(query, k, project?)` shells out to the existing
`pensive-recall` CLI at `/home/aegis/bin/pensive-recall`. The CLI takes
`--project NAME --limit N --json`; it returns the most recent atoms scoped
to the project (not query-text search). The RO tool formats the top-k hits
as a short `[i] score=... src=... project=...` block. When no `project` arg
is given the tool defaults to the basename of the task's repo root.

If `pensive-recall` is missing or fails, the tool returns a non-fatal
string rather than an error so the RO can reason about the miss. See
`internal/aegis/aegis.go` functions `L3RecallProject` and
`PensiveSearchRaw`.

### Preserved subsystems

- modelmgr: untouched. Same kill-and-spawn llama-server swap.
- toolbox: untouched. RO dispatches to its existing methods.
- trace: untouched. New kinds re-use `KindToolCall` (data.kind="ro_tool_call"
  / "p_tool_call" / "c_tool_call") and `KindLLMCall` (data.role and
  data.kind="p_reply" / "c_reply" for planner / coder replies).
- taskstore: untouched. Legacy Phase* fields are still set (PhaseInit,
  PhaseCode, PhaseDone) so existing inspect/abort code keeps working, but
  the RO loop does not enforce phase transitions anymore.
- CLI commands (task/inspect/abort/status): untouched.
- `<TOOL>...</TOOL>` parser: kept the three regex variants + balanced-JSON
  fallback; extended the dispatcher to handle ask_planner, ask_coder,
  pensive.search, done, fail.

### Smoke test

`smoke-test-ro.sh` at the repo root boots the daemon against a Python
stub that mimics llama-server's `/v1/chat/completions`. The stub returns
a scripted sequence of tool calls:
  ask_planner -> ask_coder -> fs.write -> test.run -> done

Verifies the trace records `ro_tool_call`, `p_reply`, `c_reply` events,
`hello.py` gets written to the test repo, and the task reaches
`status=succeeded`. Current pass rate: 9/9 assertions.

This does NOT exercise real LLM inference; the legacy `smoke-test.sh`
still covers that (when llama-server can load models).
