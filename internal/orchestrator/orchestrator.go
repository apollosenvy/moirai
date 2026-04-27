// Package orchestrator runs the Reviewer-Orchestrator (RO) loop.
//
// Architecture: LLM-as-orchestrator.
//
//   Planner (P)   = Qwen-27B-Claude-Opus-Distilled. First-shot planning and
//                   plan revisions. Single-turn calls; no inner loop.
//   Coder (C)     = gpt-oss-20b MXFP4. Produces code as natural text
//                   (markdown fences). Does not call tools. On a retry
//                   after test failure it is given fs.read + fs.search.
//   Reviewer-
//   Orchestrator
//   (RO)          = Gemma-4-31B. The brain. Runs a tool-call loop, picks
//                   what to do next based on results.
//
// RO tools:
//   ask_planner(instruction)           : returns plan text
//   ask_coder(instruction, plan)       : returns code text
//   fs.read(path)                       : read a file in the repo
//   fs.write(path, content)             : write a file in the repo
//   fs.search(pattern, path_glob)       : ripgrep
//   test.run()                          : run configured test command
//   compile.run()                       : run configured compile command
//   pensive.search(query, k)            : engram/pensive cross-repo memory
//   done(summary)                       : terminate task as success
//   fail(reason)                        : terminate task as failure
//
// Flow is emergent; RO decides at runtime whether to plan, revise, code,
// re-code, consult pensive, or finalize. No hardcoded A-C-B-C sequence.
//
// No git push. No git reset --hard. Final artifact is a local branch.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aegis/moirai/internal/aegis"
	"github.com/aegis/moirai/internal/modelmgr"
	"github.com/aegis/moirai/internal/plan"
	"github.com/aegis/moirai/internal/repoconfig"
	"github.com/aegis/moirai/internal/taskstore"
	"github.com/aegis/moirai/internal/toolbox"
	"github.com/aegis/moirai/internal/trace"
)

// Sentinel errors surfaced by Submit and Abort. The HTTP layer maps these to
// client-fault status codes (400 for invalid input, 404 for not found). Keep
// the set small and well-defined; the callers pattern-match via errors.Is.
var (
	// ErrInvalidInput is returned by Submit for caller-supplied data that is
	// malformed, missing, or references a non-existent path. It maps to
	// HTTP 400.
	ErrInvalidInput = errors.New("invalid input")
	// ErrTaskNotFound is returned by Abort / Inspect / Interrupt / Inject
	// when the caller references a task id the orchestrator does not know
	// about. It maps to HTTP 404.
	ErrTaskNotFound = errors.New("task not found")
	// ErrTerminalTask is returned by Abort and Inject (and friends) when the
	// caller targets a task that has already reached a terminal status
	// (succeeded / failed / aborted). The HTTP layer maps this to 409 so the
	// operator gets a clear "no-op, task already <status>" instead of a
	// silent 200. Status text is wrapped in the error message for context.
	ErrTerminalTask = errors.New("task is in terminal state")
)

// IsTerminal reports whether a task status is one the orchestrator considers
// final -- i.e. no further state transitions are possible. Centralised here
// so api/orchestrator agree on the set.
func IsTerminal(s taskstore.Status) bool {
	switch s {
	case taskstore.StatusSucceeded, taskstore.StatusFailed, taskstore.StatusAborted:
		return true
	}
	return false
}

// --- shared budgets --------------------------------------------------------

const (
	// MaxTokensAll is a legacy global cap. Current code uses the
	// per-role caps below; this constant is kept for any external
	// caller that still references it.
	MaxTokensAll = 24576

	// Per-role max_tokens caps.
	//
	// CALIBRATED FOR: gemma-4-26b-a4b at IQ4_XS / 32K ctx (reviewer),
	// qwen3.5-27b-claude-distill at Q4_K_M / 128K ctx (planner),
	// qwen3-coder-30b-a3b at IQ4_NL / 32K ctx (coder).
	// Re-tune when swapping any role to a different model class.
	// Specifically: gemma-4-31B-Claude-Opus-Reasoning-Distilled (the
	// post-2026-04-26 reviewer candidate) is denser and slower per
	// token; reviewer turns may run longer or starve at 4K.
	//
	// Reviewer: 4K balances "force concise turns" with "leave enough
	// headroom to finish emitting the tool call." Rematch #6 with the
	// global 24K cap had reviewer turns running ~10 minutes; rematch
	// #7 at 2K cut off mid-tool-call (5 no_tool_call nudges in 19
	// LLM calls). 4K threads the needle: long-context decode at
	// gemma-4-26B's ~30 tok/s gives roughly 2 minutes per turn, with
	// the <TOOL> envelope plus args fitting comfortably.
	//
	// Planner: 8K is comfortable; structured plans for medium tasks
	// land around 4-6K.
	//
	// Coder: 24K legitimately fills with multi-file code blocks for
	// scaffold phases.
	MaxTokensReviewer = 4096
	MaxTokensPlanner  = 8192
	MaxTokensCoder    = 24576

	// maxPensiveSearchCalls caps how many pensive.search dispatches
	// a single task may make. Observed in rematch #6 + #7: the
	// reviewer over-uses pensive.search as a "verify the previous
	// result" reflex, dispatching 5-8 calls in a row instead of
	// reading <RESULT> blocks already in context. After this many
	// calls, the dispatcher rejects further pensive.search with a
	// structured error nudging the model toward fs.read / test.run
	// or done. Three is generous: a real session needs at most
	// "search before plan, search before commit, search before done".
	maxPensiveSearchCalls = 3

	// Default RO loop caps. All overridable via Config.
	DefaultMaxROTurns       = 40
	DefaultMaxCoderRetries  = 5
	DefaultMaxPlanRevisions = 3
	// CompactThresholdBytes is the tool-result size above which we emit
	// the full payload as a pensive atom and present RO with a short stub.
	// Keeps her attention window lean across growing conversations. 4 KB is
	// a good default: small enough that most useful stubs fit under it,
	// large enough that we do not compact normal plan/verdict outputs.
	DefaultCompactThresholdBytes = 4096

	// rollingWindowAskCoder is how many of the most-recent ask_coder result
	// blocks we keep verbatim in the reviewer's message history. Older
	// ask_coder results get summarized down to a one-line stub preserving
	// the AUTO-COMMITTED file list (which is the only piece the reviewer
	// usually needs from a 5-turn-old coder reply).
	//
	// CALIBRATED FOR: 32K-ctx reviewer (the gemma-4-26b/31b reviewers we
	// run). Smaller windows force more aggressive compaction; larger
	// windows preserve more reasoning trail at the cost of context. Pin
	// the historical regression with TestRollingWindowFitsRematch17 in
	// rolling_compact_test.go: 17 synthetic ask_coder results at the
	// observed sizes from rematch #17 must compact under the 32K cap
	// when window=4. Re-tune ONLY by also re-running that test.
	//
	// Calibrated from rematch #17 (2026-04-26): the reviewer hit the
	// 32K ctx wall at turn 19 with 17 accumulated ask_coder results --
	// if we had kept only the most recent 4 in full and stubbed the
	// rest, the request would have fit. 4 is small enough to reclaim
	// ~10-25KB across a long task and large enough to preserve the
	// immediate reasoning trail the reviewer actually re-reads.
	rollingWindowAskCoder = 4
)

// ModelManager is the subset of *modelmgr.Manager the orchestrator needs.
// Defining it as an interface lets tests swap in a lightweight stub without
// spawning a real llama-server. The concrete *modelmgr.Manager already
// satisfies this surface.
type ModelManager interface {
	EnsureSlot(ctx context.Context, slot modelmgr.Slot) (string, error)
	Active() modelmgr.Slot
	Complete(ctx context.Context, req modelmgr.ChatRequest) (string, error)
}

type Config struct {
	ModelMgr         ModelManager
	Store            *taskstore.Store
	L2               *aegis.L2
	DefaultRepo      string
	ScratchDir       string
	MaxROTurns       int
	MaxCoderRetries  int
	MaxPlanRevisions int
	// CompactThresholdBytes: tool outputs larger than this get auto-emitted
	// as a Pensive atom and replaced with a stub in RO's conversation. Zero
	// = use default. Negative = disable compaction.
	CompactThresholdBytes int
	// MaxLLMCall is the per-Complete() wall-clock deadline. Zero (the
	// default) preserves the previous behavior: NO cap, the run's own
	// budget is the only ceiling. Set to a positive duration to
	// auto-cancel a single Complete() that hangs, so a wedged
	// llama-server is distinguishable from a slow one.
	//
	// IMPORTANT: reasoning models (gemma-4-31B-Opus-Reasoning-Distilled,
	// qwen3.5-27b-claude-distill at --reasoning on) can legitimately
	// take 10+ minutes per turn when the reasoning budget fills.
	// Setting MaxLLMCall < the worst-case reasoning-turn duration will
	// abort healthy runs. Default to zero (off) and only opt in when
	// you have a recovery strategy for hung children. The 7900 XTX +
	// llama-cpp-turboquant doesn't currently surface "wedged" as a
	// discrete state -- detection is wall-clock-only.
	MaxLLMCall time.Duration
	// Legacy fields (deprecated; the new RO loop does not use them, but the
	// daemon may still set them and we accept the values silently to avoid
	// breaking callers).
	MaxReplans int
}

type Orchestrator struct {
	cfg Config

	mu      sync.Mutex
	running map[string]*runState

	vmu           sync.Mutex
	lastVerdict   string
	lastVerdictAt time.Time

	// Daemon-lifetime context: every run goroutine derives its ctx from
	// daemonCtx (via context.WithCancel), so a single cancel() at Shutdown
	// time cuts the entire fleet of in-flight tasks. runWG tracks the
	// goroutines so Shutdown can wait for them to drain.
	daemonCtx    context.Context
	daemonCancel context.CancelFunc
	runWG        sync.WaitGroup
}

// LastVerdict returns the most recent verdict emitted by the Reviewer,
// or "" if none yet.
func (o *Orchestrator) LastVerdict() string {
	o.vmu.Lock()
	defer o.vmu.Unlock()
	return o.lastVerdict
}

// MaxROTurns returns the configured RO turn cap. Surfaced for /status so
// the UI can display "turn N / cap" without duplicating the default-fill
// logic from New().
func (o *Orchestrator) MaxROTurns() int {
	if o == nil {
		return 0
	}
	return o.cfg.MaxROTurns
}

// CorruptTaskCount returns the number of task JSON files List() found
// to be unreadable or invalid (malformed JSON, missing id, etc.).
// Surfaced for /status so operators have visibility into silent task
// drops.
func (o *Orchestrator) CorruptTaskCount() int {
	if o == nil || o.cfg.Store == nil {
		return 0
	}
	return o.cfg.Store.CorruptCount()
}

func (o *Orchestrator) setLastVerdict(v string) {
	o.vmu.Lock()
	defer o.vmu.Unlock()
	o.lastVerdict = v
	o.lastVerdictAt = time.Now()
}

// runState holds per-task state shared between the run goroutine
// (which owns roLoop) and the API-facing methods (Inject, Abort,
// Inspect). Most fields are touched ONLY by the run goroutine; the
// fields with explicit synchronization are called out below.
//
// LOCK ORDER (CRITICAL -- preserve to avoid deadlock):
//
//   1. o.mu  (Orchestrator.mu) -- guards o.running map and global
//                                  daemon-lifetime context.
//   2. st.injectMu                -- guards pendingInject; the
//                                  /tasks/<id>/inject HTTP path holds
//                                  o.mu first to look up the task,
//                                  then takes injectMu to enqueue.
//
// NEVER take o.mu while holding st.injectMu. If a future helper grows
// a path that needs to do that, it must drop injectMu first or risk
// deadlocking against Inject. The run goroutine reads pendingInject
// under injectMu only -- no reverse order anywhere.
type runState struct {
	cancel context.CancelFunc
	task   *taskstore.Task
	trace  *trace.Writer

	// wgInc is true when Submit incremented orchestrator.runWG before
	// launching this goroutine. The defer in run() consults it to decide
	// whether to call Done(); test harnesses that invoke run() directly
	// (without going through Submit) leave it false so we don't underflow
	// the wait group.
	wgInc bool

	// abortRequested is flipped to true by Abort() before it cancels the
	// run ctx. The run goroutine's defer checks this flag to decide
	// between StatusAborted (operator-driven) and StatusFailed (ctx
	// cancelled for another reason, e.g. budget timeout). Writes to
	// task.Status live exclusively on the run goroutine's path, which
	// eliminates the race Abort() used to cause.
	abortRequested atomic.Bool

	// Tool budget counters.
	askCoderCalls      int
	askPlannerCalls    int
	pensiveSearchCalls int
	roTurns            int

	// Test/compile invocation counters. The done() gate uses these to
	// detect "acceptance gaslighting": a model that calls done() with
	// summary="tests pass!" having never actually invoked test.run.
	// The acceptance-tick gate already catches this (no auto-tick =
	// unsatisfied) but its error message was generic. With these
	// counters the gate can say specifically WHICH evidence is
	// missing (the model never ran test.run / compile.run).
	testRunCount     int // increments on every test.run dispatch (success or not)
	testRunPassCount int // increments only when exit 0
	compileRunCount  int
	compileRunPassCount int

	// workOps counts "real work" tool invocations so the done() guard can
	// reject premature termination. Increments on ask_coder, fs.write,
	// test.run, compile.run. Read-only tools (fs.read, fs.search,
	// pensive.search) and ask_planner do NOT count as work -- a planner
	// call alone cannot constitute completing the task.
	workOps int

	// terminating is set true while the goroutine is exiting (after the
	// roLoop returns). Inject must observe this flag under o.mu so it can
	// reject mid-teardown injections cleanly with ErrTerminalTask rather
	// than enqueueing into a struct that is about to be dropped from
	// o.running.
	terminating atomic.Bool

	// Last emitted plan (for context when RO calls ask_coder without
	// passing one, and for L2 persistence).
	lastPlan string

	// Structured plan parsed from the planner's reply. Populated on the
	// first successful ask_planner call. Drives the <CHECKLIST> block
	// injected into every reviewer turn and the done() gate.
	// nil if the planner has not (yet) emitted parseable JSON.
	plan *plan.Plan

	// auditMode is true when the task description starts with the
	// "AUDIT-ONLY:" prefix. The reviewer system prompt is conditionally
	// expanded with the AUDIT-ONLY MODE block, AND the orchestrator
	// disables autoExtractAndCommit so a coder reply that emits "# file:"
	// markers (despite the prompt rule) cannot poison the audited
	// codebase. Same shape as P3-CRIT-1: prompt rules are a soft
	// defense; orchestrator enforcement is the hard one.
	auditMode bool

	// lastChecklistRendered is the most recent <CHECKLIST> block we
	// injected. Used to suppress duplicate injections turn-over-turn
	// when nothing has changed.
	lastChecklistRendered string

	// lastChecklistMsgIdx is the index of the user-role message holding
	// the most-recently-injected <CHECKLIST> block. Tracked so we can
	// REPLACE the previous checklist instead of appending a new one --
	// otherwise every tick produces a new ~4KB user message and the
	// reviewer's context grows linearly with turn count. -1 means no
	// checklist has been injected yet (next injection appends and
	// records the index for future replacement).
	//
	// Invariant: when lastChecklistMsgIdx >= 0, messages[idx].Role
	// MUST be "user" and messages[idx].Content MUST start with
	// "<CHECKLIST>". The injection site asserts this before mutating.
	lastChecklistMsgIdx int

	// Flag: has a test.run failed since the last ask_coder? If so, the next
	// ask_coder should get fs.read + fs.search access.
	lastTestFailed bool

	// User-injected guidance queued for the next RO turn. The RO loop
	// drains this at the top of each iteration, splices entries in as
	// user-role messages, then clears the queue. Protected by injectMu
	// so the /inject API endpoint can append from a different goroutine
	// without racing the RO loop's read.
	injectMu      sync.Mutex
	pendingInject []string

	// Loop-detection state. fsWriteHistory is a tiny ring of the last
	// N (path, content-hash) pairs written by fs.write. If the model
	// emits the SAME pair more than fsWriteRepeatCap times without an
	// intervening non-fs.write tool call, the dispatcher rejects the
	// duplicate and nudges the model to call test.run / ask_coder /
	// done instead. Driven by an observed Gemma-4-26B failure mode
	// (rematch #3 turns 21-44, where the reviewer wrote the same
	// frontend/src/api.ts 23 times in a row without progress).
	fsWriteHistory []fsWriteRecord
	// consecutiveFsWrites counts fs.write calls that have happened
	// without an intervening other tool call. Used as a soft cap to
	// force the reviewer to verify its work.
	consecutiveFsWrites int
}

// fsWriteRecord tracks one fs.write invocation for loop detection.
type fsWriteRecord struct {
	Path        string
	ContentHash uint64
}

// New validates cfg and builds an Orchestrator. Store and ModelMgr are
// required; passing either as nil returns an error instead of deferring the
// SIGSEGV into a spawned run goroutine. Defaults fill in zero-valued budget
// knobs.
func New(cfg Config) (*Orchestrator, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("orchestrator: Store is required")
	}
	if cfg.ModelMgr == nil {
		return nil, fmt.Errorf("orchestrator: ModelMgr is required")
	}
	if cfg.MaxROTurns == 0 {
		cfg.MaxROTurns = DefaultMaxROTurns
	}
	if cfg.MaxCoderRetries == 0 {
		cfg.MaxCoderRetries = DefaultMaxCoderRetries
	}
	if cfg.MaxPlanRevisions == 0 {
		// Legacy Config.MaxReplans can set this if the new knob is zero.
		if cfg.MaxReplans > 0 {
			cfg.MaxPlanRevisions = cfg.MaxReplans
		} else {
			cfg.MaxPlanRevisions = DefaultMaxPlanRevisions
		}
	}
	if cfg.CompactThresholdBytes == 0 {
		cfg.CompactThresholdBytes = DefaultCompactThresholdBytes
	}
	dctx, dcancel := context.WithCancel(context.Background())
	return &Orchestrator{
		cfg:          cfg,
		running:      make(map[string]*runState),
		daemonCtx:    dctx,
		daemonCancel: dcancel,
	}, nil
}

// Shutdown cancels every in-flight run goroutine's context and waits for
// them to drain, up to the supplied timeout. Daemon main() should call this
// before httpSrv.Shutdown so trace files, taskstore writes, and llama-server
// teardown all happen on a clean path rather than racing os.Exit.
//
// Returns nil if all goroutines drain within the timeout, otherwise an error
// with the count still outstanding.
func (o *Orchestrator) Shutdown(timeout time.Duration) error {
	if o == nil || o.daemonCancel == nil {
		return nil
	}
	o.daemonCancel()
	done := make(chan struct{})
	go func() {
		o.runWG.Wait()
		close(done)
	}()
	if timeout <= 0 {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		o.mu.Lock()
		n := len(o.running)
		o.mu.Unlock()
		return fmt.Errorf("orchestrator: %d task(s) still draining after %s", n, timeout)
	}
}

// Submit enqueues a new task. The returned task id is durable across daemon
// restarts.
//
// The returned *taskstore.Task is a deep-copied snapshot, NOT the live
// struct. The live struct is handed to the run goroutine via runState.task
// and is mutated concurrently (Status/LastError/UpdatedAt at minimum). Any
// HTTP serialisation of the returned pointer therefore sees a stable value
// and does not race the run goroutine's writes. See Task.Clone.
//
// Caller-fault errors (empty description, missing repo, bad path) wrap
// ErrInvalidInput so HTTP callers can map them to 400 via errors.Is.
func (o *Orchestrator) Submit(ctx context.Context, description, repoRoot string) (*taskstore.Task, error) {
	if strings.TrimSpace(description) == "" {
		return nil, fmt.Errorf("%w: description required", ErrInvalidInput)
	}
	if repoRoot == "" {
		repoRoot = o.cfg.DefaultRepo
	}
	if repoRoot == "" {
		return nil, fmt.Errorf("%w: no repo root", ErrInvalidInput)
	}
	expanded, err := expandUserPath(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: repo root: %v", ErrInvalidInput, err)
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return nil, fmt.Errorf("%w: repo root: %v", ErrInvalidInput, err)
	}
	// Use a generic message on stat failure: the resolved path may
	// reflect env-var substitutions or absolute paths the operator
	// considers sensitive, and echoing it back into a 400 response leaks
	// information to any HTTP caller. The trace records the original
	// repo_root for postmortem correlation; the wire response stays
	// generic.
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: repo_root is not accessible", ErrInvalidInput)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: repo_root is not a directory", ErrInvalidInput)
	}

	id := newTaskID()
	t := &taskstore.Task{
		ID:          id,
		Description: description,
		RepoRoot:    abs,
		Branch:      fmt.Sprintf("moirai/task-%s", id),
		Status:      taskstore.StatusRunning,
		Phase:       taskstore.PhaseInit,
		CreatedAt:   time.Now().UTC(),
	}
	if err := o.cfg.Store.Save(t); err != nil {
		return nil, err
	}
	tr, err := trace.Open(id)
	if err != nil {
		return nil, err
	}
	t.TracePath = tr.Path()
	o.saveTaskOrLog(t, "trace path attached")

	// Derive the run ctx from the orchestrator's daemon-lifetime ctx so
	// Shutdown(timeout) cancels every in-flight task. Falls back to
	// context.Background() if New() ran without a daemonCtx (older test
	// harnesses).
	parentCtx := context.Background()
	if o.daemonCtx != nil {
		parentCtx = o.daemonCtx
	}
	runCtx, cancel := context.WithCancel(parentCtx)
	st := &runState{cancel: cancel, task: t, trace: tr, lastChecklistMsgIdx: -1}

	o.mu.Lock()
	o.running[id] = st
	o.mu.Unlock()

	// Snapshot BEFORE launching the run goroutine: the goroutine owns all
	// further writes to t, so taking the clone here is race-free. Returning
	// the live pointer would let an HTTP response encoder read Status at the
	// same instant the run goroutine wrote it.
	snapshot := t.Clone()

	o.runWG.Add(1)
	st.wgInc = true
	go o.run(runCtx, st)
	return snapshot, nil
}

// Abort stops a running task cleanly. State persists for postmortem.
//
// Abort is intentionally write-light: it flips st.abortRequested so the run
// goroutine's defer can finalise StatusAborted, then cancels the run ctx.
// It does NOT touch st.task.Status, call Save(), or close the trace writer --
// the run goroutine is the sole writer of those fields, and doing any of that
// here would race the in-flight loop. If the task is not currently tracked in
// o.running (daemon restarted, etc.) we mark the persisted record directly.
func (o *Orchestrator) Abort(id string) error {
	o.mu.Lock()
	st, ok := o.running[id]
	o.mu.Unlock()
	if !ok {
		t, err := o.cfg.Store.Load(id)
		if err != nil {
			// Persisted record doesn't exist either -- surface the not-found
			// sentinel so the HTTP layer can return 404.
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: %s", ErrTaskNotFound, id)
			}
			return err
		}
		if t.Status == taskstore.StatusRunning {
			t.Status = taskstore.StatusAborted
			return o.cfg.Store.Save(t)
		}
		if IsTerminal(t.Status) {
			return fmt.Errorf("%w: task already %s", ErrTerminalTask, t.Status)
		}
		return nil
	}
	st.abortRequested.Store(true)
	st.trace.Emit(trace.KindInfo, map[string]any{"message": "aborted by user"})
	st.cancel()
	return nil
}

// Inject queues a user-authored guidance message for the named running
// task. The RO loop picks it up at the top of its next turn, splicing
// it in as a user-role message so the reviewer sees it before deciding
// the next tool call. Returns an error if the task isn't currently
// running (inject only makes sense for in-flight tasks; aborted /
// finished tasks have nowhere for the message to go).
//
// This is the "steer without restarting" lever: pair with the Phos UI
// composer to nudge a model that's loop-stuck without blowing away its
// context. Safe to call concurrently; the per-task injectMu serializes
// writes against the RO loop's drain.
func (o *Orchestrator) Inject(taskID, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return fmt.Errorf("inject: empty message")
	}
	o.mu.Lock()
	st, ok := o.running[taskID]
	if ok && st.terminating.Load() {
		// Race: roLoop has exited but the goroutine has not yet removed st
		// from o.running. Treat the task as terminal so the HTTP layer
		// returns 409 instead of silently queueing into a state that is
		// about to be discarded.
		o.mu.Unlock()
		return fmt.Errorf("%w: task is finalizing", ErrTerminalTask)
	}
	if ok && st.task != nil && IsTerminal(st.task.Status) {
		// Same window: the run goroutine has marked the task succeeded /
		// failed / aborted but has not yet completed cleanup. Reject so the
		// caller does not silently drop guidance.
		o.mu.Unlock()
		return fmt.Errorf("%w: task already %s", ErrTerminalTask, st.task.Status)
	}
	o.mu.Unlock()
	if !ok {
		// Distinguish "task does not exist at all" from "task exists but
		// isn't running" so the HTTP layer can map the former to 404 and
		// the latter to 409 (when the task hit a terminal state).
		t, err := o.cfg.Store.Load(taskID)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
			}
			return err
		}
		if IsTerminal(t.Status) {
			return fmt.Errorf("%w: task already %s", ErrTerminalTask, t.Status)
		}
		return fmt.Errorf("inject: task %s not running", taskID)
	}
	st.injectMu.Lock()
	st.pendingInject = append(st.pendingInject, message)
	// Cap the queue at maxInjectQueueLen messages OR maxInjectQueueBytes
	// total bytes, whichever fires first. On overflow, drop OLDEST
	// entries so the most recent operator guidance is preserved. Without
	// this cap an attacker (or a buggy auto-resending UI) can grow the
	// slice unbounded while a slow LLM task is in flight, since the RO
	// loop only drains pendingInject at the top of each turn.
	dropped := 0
	for len(st.pendingInject) > maxInjectQueueLen {
		st.pendingInject = st.pendingInject[1:]
		dropped++
	}
	for injectQueueBytes(st.pendingInject) > maxInjectQueueBytes && len(st.pendingInject) > 1 {
		st.pendingInject = st.pendingInject[1:]
		dropped++
	}
	depth := len(st.pendingInject)
	st.injectMu.Unlock()
	st.trace.Emit(trace.KindInfo, map[string]any{
		"inject":    shorten(message, 200),
		"queue_len": depth,
	})
	if dropped > 0 {
		st.trace.Emit(trace.KindInfo, map[string]any{
			"inject_dropped_oldest": dropped,
			"queue_len_after":       depth,
			"reason":                "pending_inject queue cap exceeded",
		})
	}
	return nil
}

// pendingInject queue caps. Tuned conservatively: a real human steering
// session emits a handful of messages over minutes; anything beyond 64
// queued messages or 256KB of pending guidance is almost certainly a
// runaway client.
const (
	maxInjectQueueLen   = 64
	maxInjectQueueBytes = 256 << 10

	// fsWriteMaxBytes caps a single fs.write payload. 4 MB is large enough
	// to cover any realistic source file (the largest file in this repo at
	// the time of writing is the orchestrator at ~50 KB) and small enough
	// to stop a hallucinating model from trying to dump megabytes of
	// generated text into one file.
	fsWriteMaxBytes = 4 << 20

	// fsWriteHistoryLen is the size of the per-task ring buffer that
	// remembers recent (path, content-hash) pairs. Eight entries comfortably
	// covers the "rewrote the same file 3 times by accident" case without
	// burning memory on tasks that legitimately edit one file repeatedly.
	fsWriteHistoryLen = 8

	// fsWriteRepeatCap is how many times the SAME (path, content-hash) pair
	// may appear in fsWriteHistory before the dispatcher rejects further
	// duplicates. 3 means: try, retry, retry, then refuse. Lower would be
	// brittle (a quick rewrite-then-rewrite-correctly is normal); higher
	// wastes turns the way rematch #3 did.
	//
	// CALIBRATED FOR: gemma-4-26b reviewer at IQ4_XS (the model that
	// observed the 23-turn loop). Stronger reviewers may need a lower
	// cap; weaker ones may need higher to avoid premature rejection.
	// Pinned by TestFsWriteRepeatCapStopsRematch3Loop in
	// loop_detect_test.go.
	fsWriteRepeatCap = 3

	// consecutiveFsWriteSoftCap is the threshold for the "you've written
	// a lot in a row, consider verifying" advisory. Triggers once per
	// fs.write past the cap; not a rejection, just a nudge appended to the
	// tool result. 5 is the soft floor.
	consecutiveFsWriteSoftCap = 5
)

// unsatisfiedAcceptanceContextForCoder produces a short prose block
// listing the unsatisfied acceptance items the coder's work needs to
// satisfy, so the coder writes code AGAINST a goal rather than only
// against the reviewer's free-form instruction. Returns empty string
// when no plan is loaded or all acceptance is already ticked, in
// which case the prompt template inserts nothing.
//
// Format keeps the auto-tick shapes (test.run:pass / compile.run:pass /
// file:<path>) verbatim so the coder can read them as concrete success
// criteria, not vibes.
//
// Closes the AAR finding that the Coder is "goal-blind": reviewer's
// GOAL-DRIVEN EXECUTION rule says "name the test that proves it done"
// but the orchestrator never enforced surfacing the test surface to
// the actual model doing the work.
func unsatisfiedAcceptanceContextForCoder(p *plan.Plan) string {
	if p == nil || len(p.Acceptance) == 0 {
		return ""
	}
	var unmet []plan.AcceptanceItem
	for _, a := range p.Acceptance {
		if !a.Satisfied {
			unmet = append(unmet, a)
		}
	}
	if len(unmet) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nActive acceptance criteria your code must satisfy (auto-ticked when the verify shape matches):\n")
	for _, a := range unmet {
		fmt.Fprintf(&b, "  - %s: %s  [verify: %s]\n", a.ID, a.Description, a.Verify)
	}
	b.WriteString("\n")
	return b.String()
}

// emitTaskEndAtom writes a structured atom to Pensive for cross-task
// pattern recognition. Every terminated task (succeeded / failed /
// aborted) gets ONE atom: the abstract shape (verdict, turn count,
// file/acceptance progress) rather than the full task content. Future
// sessions can pensive_recall to find "tasks like this one" and learn
// from their outcomes.
//
// Best-effort: emit failures are logged but never fail the task.
// Engram CLI not on PATH (typical in CI) is silently tolerated.
//
// Closes the design-review gap: the orchestrator emitted compaction
// atoms during runs but never a per-task summary atom. Cross-task
// learning was happening only through ad-hoc charon-emit calls Heph
// made manually; the orchestrator is now the durable emitter.
func (o *Orchestrator) emitTaskEndAtom(ctx context.Context, st *runState, t *taskstore.Task, verdict, summary string) {
	project := "moirai"
	if t != nil && t.RepoRoot != "" {
		project = filepath.Base(t.RepoRoot)
	}
	// Compose a principle line that abstracts the run shape.
	var fd, ft, ad, at int
	if st != nil && st.plan != nil {
		fd, ft, ad, at = st.plan.ProgressCounts()
	}
	var roTurns, askCoder, askPlanner, testPass, compilePass int
	if st != nil {
		roTurns = st.roTurns
		askCoder = st.askCoderCalls
		askPlanner = st.askPlannerCalls
		testPass = st.testRunPassCount
		compilePass = st.compileRunPassCount
	}
	principle := fmt.Sprintf(
		"task %s ended %s after %d reviewer turns (planner=%d coder=%d test.run:pass=%d compile.run:pass=%d files=%d/%d acc=%d/%d)",
		shorten(t.ID, 36), verdict,
		roTurns, askPlanner, askCoder, testPass, compilePass,
		fd, ft, ad, at,
	)
	contextStr := shorten(summary, 800)
	tags := fmt.Sprintf("task-end,verdict:%s,project:%s", verdict, project)
	// Best-effort emission: the run goroutine is exiting; we don't
	// want a slow Engram write to extend the task lifetime, and a
	// failure here is operationally unimportant (the trace JSONL
	// remains the durable record).
	emitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := aegis.PensiveEmit(emitCtx, "discovery", project, principle, contextStr, tags); err != nil {
		log.Printf("orchestrator: pensive task-end atom for %s skipped: %v", t.ID, err)
	}
}

// saveTaskOrLog persists the task and surfaces save errors via
// log.Printf instead of swallowing them. Disk-full / permission
// errors during failure handling are the worst time to be invisible
// (the operator sees "task failed" but the persisted record never
// updated). Closes Shepherd's "Store.Save errors swallowed in
// failure paths" finding. Control flow is unchanged; only error
// surfacing improves.
func (o *Orchestrator) saveTaskOrLog(t *taskstore.Task, sitenote string) {
	if err := o.cfg.Store.Save(t); err != nil {
		log.Printf("orchestrator: failed to persist task %s after %s: %v", t.ID, sitenote, err)
	}
}

// completeWithDeadline wraps ModelMgr.Complete in a per-call
// context.WithTimeout when Config.MaxLLMCall is set. Zero MaxLLMCall
// preserves prior behavior (no cap). Returning the wrapped error
// preserves caller-side cancellation handling; the timeout error
// surfaces as ctx.DeadlineExceeded which the caller can distinguish
// from a child-died error via errors.Is.
//
// Closes Shepherd's "no per-LLM-call deadline" finding while
// remaining safe for reasoning models (default: no cap).
func (o *Orchestrator) completeWithDeadline(ctx context.Context, req modelmgr.ChatRequest) (string, error) {
	if o.cfg.MaxLLMCall <= 0 {
		return o.cfg.ModelMgr.Complete(ctx, req)
	}
	cctx, cancel := context.WithTimeout(ctx, o.cfg.MaxLLMCall)
	defer cancel()
	return o.cfg.ModelMgr.Complete(cctx, req)
}

// isAuditStateFilePath reports whether the given path is a permitted
// audit-mode write target. In AUDIT-ONLY mode the orchestrator allows
// writes ONLY to the two audit state files referenced by the reviewer's
// AUDIT-ONLY MODE prompt -- everything else is refused so the audited
// codebase stays untouched.
//
// We strip "./" and accept either the bare path or the variants the
// model might emit. Path matching is conservative: the file must be
// EXACTLY one of the two allow-listed names (no subdirectories, no
// alternate basenames). The audit dir lives at the repo root.
//
// TODO(rename): both allow-listed paths still reference .agent-router/
// pending the deferred filesystem-rename. When the dir migrates to
// .moirai/, update both branches AND the prompt strings together so
// the prompt and the gate stay synchronized.
func isAuditStateFilePath(path string) bool {
	p := strings.TrimPrefix(strings.TrimSpace(path), "./")
	return p == ".agent-router/checklist.md" || p == ".agent-router/findings.md"
}

// contentHash is FNV-1a 64-bit. We are not collision-resistant against
// adversaries here; the hash is only used to detect "did the model emit
// the EXACT same content twice" and a 64-bit hash is plenty.
func contentHash(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// duplicateWriteCount returns how many times the (path, hash) pair
// appears in the recent history. The dispatcher uses this BEFORE
// performing a write to decide whether to reject as a duplicate; the
// caller appends the new record only on the success path.
func duplicateWriteCount(history []fsWriteRecord, path string, hash uint64) int {
	n := 0
	for _, r := range history {
		if r.Path == path && r.ContentHash == hash {
			n++
		}
	}
	return n
}

func injectQueueBytes(q []string) int {
	n := 0
	for _, s := range q {
		n += len(s)
	}
	return n
}

// Interrupt is a soft interrupt: it queues a user message that tells the
// RO its current line of reasoning is being cut off and asks it to
// reconsider. Unlike Abort, the task keeps running; unlike Inject, the
// message is fixed rather than user-authored. Useful when the reviewer
// is clearly stuck in a no_tool_call loop and needs a hard reset.
func (o *Orchestrator) Interrupt(taskID string) error {
	return o.Inject(taskID, "USER INTERRUPT: stop your current line of reasoning. "+
		"Re-read the task description and emit your next tool call immediately. "+
		"Do not produce additional prose before the tool wrapper.")
}

// InspectResult holds a task record plus its recent trace events.
type InspectResult struct {
	Task   *taskstore.Task `json:"task"`
	Recent []trace.Event   `json:"recent"`
}

func (o *Orchestrator) Inspect(id string) (*InspectResult, error) {
	t, err := o.cfg.Store.Load(id)
	if err != nil {
		// Translate the on-disk "no such file" into ErrTaskNotFound so the
		// HTTP layer can return a clean "task not found: <id>" body rather
		// than leaking the daemon's internal task-store path.
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
		}
		return nil, err
	}
	// ReadTail seeks from EOF; avoids re-reading the entire trace jsonl on
	// every poll as long-running tasks accumulate thousands of events.
	events, _ := trace.ReadTail(id, 20)
	return &InspectResult{Task: t, Recent: events}, nil
}

func (o *Orchestrator) Status() ([]*taskstore.Task, error) {
	return o.cfg.Store.List()
}

// --- main RO loop ----------------------------------------------------------

func (o *Orchestrator) run(ctx context.Context, st *runState) {
	defer func() {
		if st != nil && st.wgInc {
			o.runWG.Done()
		}
	}()
	// Panic recovery: a panic in extractToolCall, regex parsing, a model
	// call, the toolbox, or anywhere else in the long-running RO loop used
	// to crash the entire daemon (HTTP handler panics are caught by net/http
	// but goroutines we spawn ourselves are not). Convert it into a normal
	// task failure with the stack noted on the trace, so the rest of the
	// daemon survives.
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("panic in run goroutine: %v", r)
			fmt.Fprintln(os.Stderr, "orchestrator: "+msg)
			if st.task != nil {
				st.task.Status = taskstore.StatusFailed
				st.task.LastError = msg
				if o.cfg.Store != nil {
					o.saveTaskOrLog(st.task, "ro-loop fatal")
				}
			}
			if st.trace != nil {
				st.trace.Emit(trace.KindError, map[string]any{
					"fatal":  msg,
					"source": "panic_recover",
				})
			}
		}
	}()
	defer func() {
		// Mark as terminating BEFORE removing from o.running so any concurrent
		// Inject sees ErrTerminalTask instead of a transient
		// "running but no task" race window. The flag is observed under
		// o.mu so a Lock() in Inject is enough to serialize.
		st.terminating.Store(true)
		o.mu.Lock()
		delete(o.running, st.task.ID)
		o.mu.Unlock()
		_ = st.trace.Close()
	}()

	t := st.task
	tr := st.trace

	// Load repo config.
	rcfg, hadCfg, err := repoconfig.Load(t.RepoRoot)
	if err != nil {
		o.fail(st, t, tr, fmt.Errorf("load repo config: %w", err))
		return
	}
	tr.Emit(trace.KindInfo, map[string]any{
		"repo_config_loaded": hadCfg,
		"commands":           rcfg.Commands,
		"budget_runtime":     rcfg.Budget.MaxRuntime.String(),
	})

	// Apply wall-clock budget.
	if rcfg.Budget.MaxRuntime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, rcfg.Budget.MaxRuntime)
		defer cancel()
	}

	tb, err := toolbox.New(t.RepoRoot, t.Branch, o.cfg.ScratchDir, rcfg, false)
	if err != nil {
		o.fail(st, t, tr, fmt.Errorf("toolbox: %w", err))
		return
	}

	// Fresh project escape hatch: if the user pointed us at a directory
	// that has never been git-initialized, do the ritual ourselves so the
	// submit form doesn't need "cd && git init && commit" as a prereq.
	// No-op if repo_root is already a git repo.
	if err := tb.EnsureRepo(ctx); err != nil {
		o.fail(st, t, tr, fmt.Errorf("git init: %w", err))
		return
	}

	// Check out working branch.
	if err := tb.GitCheckoutBranch(ctx); err != nil {
		o.fail(st, t, tr, fmt.Errorf("git checkout: %w", err))
		return
	}
	tr.Emit(trace.KindInfo, map[string]any{"branch": t.Branch})

	// Kick off the RO loop.
	t.Phase = taskstore.PhaseCode // closest legacy phase; RO loop unifies planning+coding
	t.ActiveModel = "reviewer"
	o.saveTaskOrLog(t, "ro-loop start")
	tr.Emit(trace.KindPhase, map[string]any{"phase": "ro_loop"})

	summary, ok, err := o.roLoop(ctx, st, tb)
	if err != nil {
		o.fail(st, t, tr, err)
		return
	}
	if !ok {
		o.fail(st, t, tr, fmt.Errorf("ro loop terminated without success: %s", summary))
		return
	}

	// Final commit on branch; never push.
	msg := fmt.Sprintf("moirai: %s\n\nTask: %s\n", shorten(t.Description, 72), t.ID)
	if _, err := tb.GitCommit(ctx, msg); err != nil {
		tr.Emit(trace.KindInfo, map[string]any{"commit_skipped": err.Error()})
	} else {
		tr.Emit(trace.KindInfo, map[string]any{"commit": "created"})
	}

	t.Status = taskstore.StatusSucceeded
	t.Phase = taskstore.PhaseDone
	o.saveTaskOrLog(t, "task succeeded")
	tr.Emit(trace.KindDone, map[string]any{
		"branch":  t.Branch,
		"summary": shorten(summary, 2000),
		"turns":   st.roTurns,
	})

	o.setLastVerdict("succeeded")
	tr.Emit(trace.KindVerdict, map[string]any{
		"phase":   "final",
		"verdict": "succeeded",
		"summary": shorten(summary, 4000),
	})
	if o.cfg.L2 != nil {
		_ = o.cfg.L2.RecordVerdict(t.RepoRoot, t.ID, "final", "succeeded", shorten(summary, 4000))
	}
	o.emitTaskEndAtom(ctx, st, t, "succeeded", summary)
}

// roLoop runs the RO as a tool-call driver until it emits done or fail, or
// until a budget is exhausted.
func (o *Orchestrator) roLoop(ctx context.Context, st *runState, tb *toolbox.Toolbox) (string, bool, error) {
	t := st.task
	tr := st.trace

	// Make sure the reviewer model is loaded.
	if _, err := o.cfg.ModelMgr.EnsureSlot(ctx, modelmgr.SlotReviewer); err != nil {
		return "", false, fmt.Errorf("ensure reviewer: %w", err)
	}
	tr.Emit(trace.KindSwap, map[string]any{"to": "reviewer", "reason": "ro_loop"})

	// One-shot format primer: a synthetic prior exchange that demonstrates
	// the exact envelope the parser expects. Reasoning models (Ministral,
	// Gemma-4, Qwen-distill) tend to drift from the natural-language spec
	// in the system prompt; showing them ONE successful turn pins the
	// format and dramatically reduces no_tool_call nudges in early turns.
	// The synthetic task is a no-op (a hello task with a fake "ok" verdict)
	// so it cannot leak into the real plan; the model just sees the shape.
	// Conditionally render the AUDIT-ONLY MODE block ONLY when the task
	// description begins with the "AUDIT-ONLY:" prefix. Default reviews
	// burn ~70 lines of context on rules that don't apply, and Gemini's
	// AAR pointed out the lost-in-the-middle risk. Stash the flag on
	// runState so autoExtractAndCommit can refuse to write code into
	// the audited project (the prompt rule "do NOT include # file:"
	// is a soft defense; orchestrator enforcement is the hard one).
	st.auditMode = strings.HasPrefix(t.Description, "AUDIT-ONLY:")
	messages := []modelmgr.ChatMessage{
		{Role: "system", Content: roSystemPrompt(st.auditMode)},
		{Role: "user", Content: "Task:\nWrite a hello-world script.\n\nRepo root: /tmp/example\nBranch: example/demo\n\nBegin. Think about what to do first, then emit a single tool call wrapped in <TOOL>...</TOOL>."},
		{Role: "assistant", Content: `Hello-world is small enough that I can dispatch the coder directly without a planner round. I will instruct the coder to produce a single Python script.

<TOOL>{"name": "ask_coder", "args": {"instruction": "Write a Python script src/hello.py that prints \"hello\"."}}</TOOL>`},
		{Role: "user", Content: "<RESULT>(example exchange ends here -- the real task follows)</RESULT>"},
		{Role: "user", Content: fmt.Sprintf(
			"Task:\n%s\n\nRepo root: %s\nBranch: %s\n\nBegin. Think about what to do first, then emit a single tool call wrapped in <TOOL>...</TOOL>.",
			t.Description, t.RepoRoot, t.Branch,
		)},
	}

	for {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		if st.roTurns >= o.cfg.MaxROTurns {
			return fmt.Sprintf("exceeded max RO turns (%d)", o.cfg.MaxROTurns), false, nil
		}
		st.roTurns++

		// Drain any user-injected guidance into the message stream before
		// this turn's LLM call. Each pending entry becomes a user-role
		// message tagged with USER INJECT so the reviewer can distinguish
		// operator steering from orchestrator nudges.
		st.injectMu.Lock()
		if n := len(st.pendingInject); n > 0 {
			for _, m := range st.pendingInject {
				messages = append(messages, modelmgr.ChatMessage{
					Role:    "user",
					Content: "[USER INJECT] " + m,
				})
			}
			st.pendingInject = st.pendingInject[:0]
			st.injectMu.Unlock()
			tr.Emit(trace.KindInfo, map[string]any{
				"inject_drained": n,
				"turn":           st.roTurns,
			})
		} else {
			st.injectMu.Unlock()
		}

		// Ensure reviewer still loaded (ask_planner / ask_coder swap it).
		if o.cfg.ModelMgr.Active() != modelmgr.SlotReviewer {
			if _, err := o.cfg.ModelMgr.EnsureSlot(ctx, modelmgr.SlotReviewer); err != nil {
				return "", false, fmt.Errorf("ro turn %d: ensure reviewer: %w", st.roTurns, err)
			}
			tr.Emit(trace.KindSwap, map[string]any{"to": "reviewer", "reason": "ro_resume"})
		}

		// Inject the live <CHECKLIST> block when the structured plan has
		// state to show AND it has CHANGED since we last injected. This
		// avoids context bloat from re-emitting an unchanged checklist
		// every turn while still keeping the reviewer's view current.
		// Empty when no plan is loaded -- preserves legacy behavior for
		// reviewers that never get a structured plan.
		if cl := st.plan.RenderChecklist(); cl != "" && cl != st.lastChecklistRendered {
			// REPLACE the prior checklist message in place when one
			// exists, otherwise APPEND a new one and record its index.
			// Replacing prevents context bloat: rematch #21 hit ctx
			// overflow at turn 9 with 9 cumulative checklist messages
			// summing to ~30KB. With replacement, exactly one checklist
			// message lives in messages at any time, regardless of turn
			// count. Defense-in-depth: assert the recorded index still
			// points at a checklist (in case of message-array mutation
			// elsewhere); on mismatch, fall back to append + re-record.
			replaced := false
			if st.lastChecklistMsgIdx >= 0 && st.lastChecklistMsgIdx < len(messages) {
				prior := messages[st.lastChecklistMsgIdx]
				if prior.Role == "user" && strings.HasPrefix(prior.Content, "<CHECKLIST>") {
					messages[st.lastChecklistMsgIdx].Content = cl
					replaced = true
				}
			}
			if !replaced {
				messages = append(messages, modelmgr.ChatMessage{
					Role:    "user",
					Content: cl,
				})
				st.lastChecklistMsgIdx = len(messages) - 1
			}
			st.lastChecklistRendered = cl
			// Report tick counts directly. The byte length of cl is a
			// poor proxy because "[ ]" and "[x]" are both 3 bytes --
			// flipping a checkbox doesn't change cl's size, so external
			// observers watching `bytes` can't tell whether the plan is
			// progressing. files_done / acc_done give the real picture.
			fd, ft, ad, at := st.plan.ProgressCounts()
			tr.Emit(trace.KindInfo, map[string]any{
				"checklist_injected": true,
				"turn":               st.roTurns,
				"bytes":              len(cl),
				"files_done":         fd,
				"files_total":        ft,
				"acc_done":           ad,
				"acc_total":          at,
				"replaced":           replaced,
			})
		}

		resp, err := o.completeWithDeadline(ctx, modelmgr.ChatRequest{
			Messages:    messages,
			Temperature: 0.2,
			MaxTokens:   MaxTokensReviewer,
		})
		if err != nil {
			return "", false, fmt.Errorf("ro turn %d: %w", st.roTurns, err)
		}
		tr.Emit(trace.KindLLMCall, map[string]any{
			"role":  "reviewer",
			"turn":  st.roTurns,
			"bytes": len(resp),
			"head":  shorten(resp, 400),
		})

		call, ok, parseErr := extractToolCallChecked(resp)

		// RO-prose-bloat guard: if RO emitted no tool call AND the response is
		// large (>8 KB), truncate it before appending to messages. Unbounded
		// assistant-role prose grew the Observatory run from 40 KB to 131 KB
		// context in 4 turns because Gemma kept re-explaining the same
		// misunderstood fs.write result. We keep a head+tail summary so the
		// RO can still see what it said without burning context on the body.
		assistantContent := resp
		if !ok && len(resp) > 8192 {
			head := shorten(resp[:4096], 4096)
			tail := resp[len(resp)-1024:]
			assistantContent = fmt.Sprintf("%s\n...[truncated %d bytes of no-tool-call prose]...\n%s",
				head, len(resp)-5120, tail)
		}
		messages = append(messages, modelmgr.ChatMessage{Role: "assistant", Content: assistantContent})

		if errors.Is(parseErr, ErrMultipleToolCalls) {
			// Surface a recoverable error to the model rather than silently
			// picking one block and dropping the rest. The model can resend
			// with a single tool call.
			errMsg := "more than one <TOOL>...</TOOL> block in the same response; emit exactly one per turn"
			messages = append(messages, modelmgr.ChatMessage{
				Role:    "user",
				Content: fmt.Sprintf("<ERROR>%s</ERROR>", errMsg),
			})
			tr.Emit(trace.KindInfo, map[string]any{"ro_nudge": "multi_tool_call", "turn": st.roTurns})
			continue
		}

		if !ok {
			// Nudge once, then give up.
			if st.roTurns < o.cfg.MaxROTurns {
				nudge := "No tool call detected. Emit exactly one call wrapped in <TOOL>...</TOOL>. " +
					"Both strict form <TOOL>{\"name\":\"X\",\"args\":{...}}</TOOL> and shorthand " +
					"<TOOL>X args: {...}</TOOL> are accepted. If the last action already succeeded " +
					"(fs.write returns \"OK wrote N bytes ...\"), proceed to the next step; do not " +
					"re-attempt. Skip prose; emit the tool call now."
				messages = append(messages, modelmgr.ChatMessage{
					Role:    "user",
					Content: nudge,
				})
				tr.Emit(trace.KindInfo, map[string]any{"ro_nudge": "no_tool_call", "turn": st.roTurns})
				continue
			}
			return "ro emitted no tool call", false, nil
		}

		// Terminal tools.
		switch call.Name {
		case "done":
			// Reject premature done: a model that emits done before doing
			// any real work has hallucinated success. Require at least one
			// of {ask_coder, fs.write, test.run, compile.run} to have been
			// invoked successfully on this run. Recoverable: we send an
			// error back to the model and let it pick a real next step.
			if st.workOps == 0 {
				errMsg := "cannot call done before performing any work; call ask_planner/ask_coder or fs.write/test.run first"
				tr.Emit(trace.KindToolCall, map[string]any{
					"kind":  "ro_tool_call",
					"name":  "done",
					"error": errMsg,
					"turn":  st.roTurns,
				})
				messages = append(messages, modelmgr.ChatMessage{
					Role:    "user",
					Content: fmt.Sprintf("<ERROR>%s</ERROR>", errMsg),
				})
				continue
			}
			// Acceptance gate: when a structured plan was loaded, refuse
			// done() until all acceptance items are satisfied. This is
			// the hard "red means stop" backstop the harness needs --
			// the reviewer can no longer claim "all done" while the
			// checklist still has unchecked criteria. Plans without
			// acceptance items skip this gate entirely (legacy task
			// mode).
			if unmet := st.plan.UnsatisfiedAcceptance(); len(unmet) > 0 {
				var b strings.Builder
				b.WriteString("cannot call done: the plan's acceptance criteria are not all satisfied. Unsatisfied:\n")
				for _, u := range unmet {
					b.WriteString("  - ")
					b.WriteString(u)
					b.WriteString("\n")
				}
				// Acceptance gaslighting diagnostic: detect the model
				// claiming "tests pass" without ever invoking test.run /
				// compile.run, and call it out specifically. The plan
				// has unmet items with verify="test.run:pass" or
				// "compile.run:pass" -- if the corresponding tool has
				// never been called successfully (or at all), say so.
				needsTestRun := false
				needsCompileRun := false
				for _, a := range st.plan.Acceptance {
					if a.Satisfied {
						continue
					}
					switch a.Verify {
					case "test.run:pass":
						needsTestRun = true
					case "compile.run:pass":
						needsCompileRun = true
					}
				}
				if needsTestRun && st.testRunPassCount == 0 {
					if st.testRunCount == 0 {
						b.WriteString("\nDIAGNOSTIC: test.run has NEVER been called this run. test.run:pass acceptance items cannot tick by intuition; call test.run as a tool to gather real evidence.")
					} else {
						b.WriteString(fmt.Sprintf("\nDIAGNOSTIC: test.run has been called %d time(s) but has not yet exited 0. Inspect the failures, dispatch ask_coder for a fix, then retry test.run.", st.testRunCount))
					}
				}
				if needsCompileRun && st.compileRunPassCount == 0 {
					if st.compileRunCount == 0 {
						b.WriteString("\nDIAGNOSTIC: compile.run has NEVER been called this run. compile.run:pass acceptance items cannot tick by intuition; call compile.run as a tool to gather real evidence.")
					} else {
						b.WriteString(fmt.Sprintf("\nDIAGNOSTIC: compile.run has been called %d time(s) but has not yet exited 0. Inspect the failures, dispatch ask_coder for a fix, then retry compile.run.", st.compileRunCount))
					}
				}
				b.WriteString("\nRun test.run / compile.run / write the missing files (see <CHECKLIST>), then retry done.")
				errMsg := b.String()
				tr.Emit(trace.KindToolCall, map[string]any{
					"kind":             "ro_tool_call",
					"name":             "done",
					"error":            "acceptance not satisfied",
					"unsatisfied":      len(unmet),
					"turn":             st.roTurns,
				})
				messages = append(messages, modelmgr.ChatMessage{
					Role:    "user",
					Content: fmt.Sprintf("<ERROR>%s</ERROR>", errMsg),
				})
				continue
			}
			summary, _ := call.Args["summary"].(string)
			tr.Emit(trace.KindToolCall, map[string]any{
				"kind":    "ro_tool_call",
				"name":    "done",
				"summary": shorten(summary, 400),
				"turn":    st.roTurns,
			})
			return summary, true, nil
		case "fail":
			reason, _ := call.Args["reason"].(string)
			// Sanitize empty / whitespace-only reasons so LastError doesn't
			// end up as "fail: " (a trailing colon with nothing after it).
			if strings.TrimSpace(reason) == "" {
				reason = "task failed (no reason provided)"
			}
			tr.Emit(trace.KindToolCall, map[string]any{
				"kind":   "ro_tool_call",
				"name":   "fail",
				"reason": shorten(reason, 400),
				"turn":   st.roTurns,
			})
			return reason, false, nil
		}

		// Non-terminal: execute the tool and feed the result back.
		result, toolErr := o.executeROTool(ctx, st, tb, call)
		compactedFrom := 0
		if toolErr == nil {
			// Auto-compaction: if the result is large, emit it as a Pensive
			// atom and replace it in RO's conversation with a short stub.
			// Protects RO's attention window from bloating as the task grows.
			// Compaction is skipped for tools that should never bloat:
			// - pensive.search / pensive.emit_atom: already retrievals
			// - ask_planner / ask_coder: their output is the substrate the
			//   reviewer reasons over. Rematch #9 traces show a 52 KB
			//   ask_planner reply being compacted to a 393-byte stub; the
			//   reviewer then panicked and burned its pensive.search budget
			//   trying to find "the plan" that no longer existed in
			//   conversation context. Auto-extract for ask_coder also
			//   handles the file-emission half of its output, so the
			//   reviewer only needs the prose summary, but the prose IS the
			//   reasoning trail and is itself often >4 KB.
			compactable := call.Name != "pensive.search" &&
				call.Name != "pensive.emit_atom" &&
				call.Name != "ask_planner" &&
				call.Name != "ask_coder"
			if compactable && o.cfg.CompactThresholdBytes > 0 && len(result) > o.cfg.CompactThresholdBytes {
				stub, emitErr := o.compactLargeResult(ctx, st, call, result)
				if emitErr == nil {
					compactedFrom = len(result)
					result = stub
				}
				// If emission failed, fall back to the uncompacted payload.
				// Better a slow turn than a lost tool result.
			}
		}
		traceEvt := map[string]any{
			"kind":  "ro_tool_call",
			"name":  call.Name,
			"args":  shortenArgs(call.Args),
			"bytes": len(result),
			"error": errString(toolErr),
			"turn":  st.roTurns,
		}
		if compactedFrom > 0 {
			traceEvt["compacted_from"] = compactedFrom
		}
		tr.Emit(trace.KindToolCall, traceEvt)

		payload := ""
		if toolErr != nil {
			payload = fmt.Sprintf("<ERROR>%s</ERROR>", neutralizeEnvelopeTags(toolErr.Error()))
		} else {
			// SEC-PASS5-004: neutralize <TOOL>...</TOOL> and </RESULT>
			// substrings inside the tool output before wrapping. A planted
			// file containing `</RESULT><TOOL>{...}</TOOL>` would otherwise
			// inject a synthetic tool envelope into the next user-role
			// message; a sufficiently confused model parroting the file
			// content into its next assistant reply could then trip
			// extractToolCallChecked into executing the planted call.
			payload = fmt.Sprintf("<RESULT>%s</RESULT>", neutralizeEnvelopeTags(result))
		}
		messages = append(messages, modelmgr.ChatMessage{Role: "user", Content: payload})

		// Rolling-window compaction of accumulated ask_coder result
		// blocks. The most-recent rollingWindowAskCoder are kept verbatim;
		// older ones are summarized in place. See the constant docstring
		// for calibration notes from rematch #17. Idempotent on the most
		// recent results because they are skipped by the "leave the last
		// N alone" rule.
		if call.Name == "ask_coder" {
			// Always emit a rolling_compact trace event after each ask_coder
			// so observers can confirm the compactor was consulted, even
			// when reclaimed_bytes=0 (window not yet exceeded). Symmetric
			// with the auto_extract / fs_write diagnostic philosophy.
			reclaimed := compactStaleAskCoderResults(messages, rollingWindowAskCoder)
			tr.Emit(trace.KindInfo, map[string]any{
				"rolling_compact": true,
				"reclaimed_bytes": reclaimed,
				"window":          rollingWindowAskCoder,
				"turn":            st.roTurns,
			})
		}
	}
}

// compactStaleAskCoderResults rewrites old ask_coder result messages in
// the conversation history to a one-line stub preserving the AUTO-COMMITTED
// file list when present. The most recent `keep` ask_coder result blocks
// are left verbatim. Returns the total bytes reclaimed. Idempotent: a
// message that is already shorter than its stub form is left alone.
//
// Heuristic identification: an ask_coder result with auto-extracted files
// is a user-role message whose content starts with the literal prefix
// "<RESULT>AUTO-COMMITTED " (autoExtractAndCommit prepends "AUTO-COMMITTED N
// file(s)..." as the leading line of the tool result). We deliberately
// require a PREFIX match rather than a substring contains: substring
// matching false-positives on fs.read results that happen to contain
// "AUTO-COMMITTED" or "# file:" inside their bodies (e.g., the reviewer
// fs.read'ing PLAN.md or a Markdown file with `# file:` headings would
// have its content destroyed by the compactor). Caught by audit pass 3
// (P3-CRIT-1) before it bit a real run.
//
// Ask_coder results without an AUTO-COMMITTED block (no files extracted)
// are NOT compacted: those typically hold reasoning prose the reviewer
// re-reads, and the compactor's stub would lose that prose. They tend
// to be small anyway (auto-extract failed because the coder emitted no
// recognizable fence), so leaving them alone is cheap.
func compactStaleAskCoderResults(messages []modelmgr.ChatMessage, keep int) int {
	if keep < 0 {
		keep = 0
	}
	const askCoderPrefix = "<RESULT>AUTO-COMMITTED "
	// First pass: find indices of ask_coder result messages.
	var idxs []int
	for i, m := range messages {
		if m.Role != "user" {
			continue
		}
		if strings.HasPrefix(m.Content, askCoderPrefix) {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) <= keep {
		return 0
	}
	// Compact everything except the trailing `keep` indices.
	stale := idxs[:len(idxs)-keep]
	reclaimed := 0
	for _, i := range stale {
		orig := messages[i].Content
		stub := stubFromAskCoderResult(orig)
		if len(stub) >= len(orig) {
			// Already as small as we can make it; nothing to reclaim.
			continue
		}
		messages[i].Content = stub
		reclaimed += len(orig) - len(stub)
	}
	return reclaimed
}

// stubFromAskCoderResult condenses an ask_coder <RESULT>...</RESULT> message
// into a short stub preserving the AUTO-COMMITTED file list when present.
// Falls back to a plain "(ask_coder result, N bytes, summarized)" stub if
// no AUTO-COMMITTED summary is found.
func stubFromAskCoderResult(content string) string {
	// Already-stubbed messages are idempotent: stubs are short and start
	// with the marker prefix below.
	if strings.HasPrefix(content, "<RESULT>[stale ask_coder") {
		return content
	}
	originalLen := len(content)
	// Pull the leading AUTO-COMMITTED summary block, if present. It looks
	// like "AUTO-COMMITTED N file(s) from coder reply:\n  - path (bytes, what)\n..."
	// followed by a blank line and the rest of the prose.
	body := content
	body = strings.TrimPrefix(body, "<RESULT>")
	body = strings.TrimSuffix(body, "</RESULT>")
	var summary string
	if strings.HasPrefix(body, "AUTO-COMMITTED ") {
		// Take through the first blank line (or to end if none).
		if idx := strings.Index(body, "\n\n"); idx > 0 {
			summary = body[:idx]
		} else {
			summary = body
		}
	}
	if summary != "" {
		return fmt.Sprintf("<RESULT>[stale ask_coder result, %d bytes summarized]\n%s</RESULT>",
			originalLen, summary)
	}
	return fmt.Sprintf("<RESULT>[stale ask_coder result, %d bytes summarized; no AUTO-COMMITTED block to preserve]</RESULT>",
		originalLen)
}

// installPlanFromReply parses a planner reply for the trailing structured
// JSON plan and, if present and valid, installs it on st.plan. Emits one
// of two trace events:
//   - plan_parsed=true with phase/acceptance counts on success
//   - plan_parse_error=<err> on malformed JSON
// A reply with no JSON at all is NOT an error -- the reviewer continues
// with prose-only plan (legacy behavior, pre-checklist) and st.plan
// stays nil. Extracted from the ask_planner inline path so the contract
// can be exercised end-to-end without spinning up a real planner LLM.
// Closes audit-pass-1 COV-IMP-7.
func installPlanFromReply(st *runState, reply string) {
	parsed, perr := plan.Parse(reply)
	if perr != nil {
		st.trace.Emit(trace.KindInfo, map[string]any{
			"plan_parse_error": perr.Error(),
			"reply_bytes":      len(reply),
		})
		return
	}
	if parsed == nil {
		// No JSON in the reply -- silent legacy path.
		return
	}
	st.plan = parsed
	st.trace.Emit(trace.KindInfo, map[string]any{
		"plan_parsed":      true,
		"phases":           len(parsed.Phases),
		"acceptance_items": len(parsed.Acceptance),
	})
}

// neutralizeEnvelopeTags replaces tag-like substrings inside model-controlled
// tool output that could otherwise impersonate the orchestrator's own
// <RESULT>...</RESULT> / <TOOL>...</TOOL> envelopes. A file deliberately
// planted to contain `</RESULT><TOOL>{...}</TOOL>` would, when handed back
// verbatim, look to a downstream reader (and to a parroting model) like the
// orchestrator itself emitted a synthetic tool call. We rewrite the literal
// tag forms to non-collidable variants. Legitimate prose mentions of the
// tag names survive via the "_LITERAL" suffix, which is unambiguous to a
// human reader and harmless to the regex parser. SEC-PASS5-004.
func neutralizeEnvelopeTags(s string) string {
	if s == "" {
		return s
	}
	if !strings.ContainsAny(s, "<") {
		return s
	}
	// Order matters only for clarity; the replacements don't overlap.
	r := strings.NewReplacer(
		"<TOOL>", "<TOOL_LITERAL>",
		"</TOOL>", "</TOOL_LITERAL>",
		"<RESULT>", "<RESULT_LITERAL>",
		"</RESULT>", "</RESULT_LITERAL>",
		"<ERROR>", "<ERROR_LITERAL>",
		"</ERROR>", "</ERROR_LITERAL>",
	)
	return r.Replace(s)
}

// compactLargeResult emits the full tool result as a Pensive atom and returns
// a short stub referencing it. The stub includes a query that RO can use with
// pensive.search to retrieve the original content on demand. Failure is
// swallowed by the caller; if emission fails we prefer to hand the full
// result back rather than drop content.
func (o *Orchestrator) compactLargeResult(ctx context.Context, st *runState, call toolCall, result string) (string, error) {
	project := "agent-router"
	if st.task != nil && st.task.RepoRoot != "" {
		project = filepath.Base(st.task.RepoRoot)
	}
	topic := fmt.Sprintf("%s output for task %s", call.Name, shorten(st.task.ID, 24))
	tags := fmt.Sprintf("tool:%s,task:%s,compacted,source:ro-compact",
		call.Name, shorten(st.task.ID, 32))
	// Emit as "discovery" so future pensive.search treats this as reusable
	// context. The principle IS the full tool output; context is the topic
	// label we reuse in the retrieval hint below.
	_, err := aegis.PensiveEmit(ctx, "discovery", project, result, topic, tags)
	if err != nil {
		return result, err
	}
	stub := fmt.Sprintf(
		"[compacted: %d bytes of %s output auto-emitted as a pensive atom. "+
			"Project: %s. Topic: %q. Retrieve the full content with "+
			"pensive.search(query=%q, k=1) if you need to re-read it; "+
			"otherwise continue with the summary in your working memory.]",
		len(result), call.Name, project, topic, topic,
	)
	return stub, nil
}

// executeROTool dispatches the RO's non-terminal tool calls. Terminal tools
// (done, fail) are handled inline in roLoop.
func (o *Orchestrator) executeROTool(ctx context.Context, st *runState, tb *toolbox.Toolbox, tc toolCall) (string, error) {
	// Reset the consecutive-fs.write counter whenever any other tool is
	// dispatched. The counter is only useful as "writes since last
	// non-write action"; pensive.search / fs.read / test.run / etc. all
	// count as breaking the streak.
	if tc.Name != "fs.write" {
		st.consecutiveFsWrites = 0
	}
	argStr := func(k string) string {
		v, _ := tc.Args[k].(string)
		return v
	}
	// argHas reports whether the model included the named key at all in
	// args, even with a zero/empty value. Distinguishes "fs.write content:
	// \"\"" (deliberate truncation, allowed) from "fs.write" with no
	// content field at all (malformed call, must reject).
	argHas := func(k string) bool {
		_, ok := tc.Args[k]
		return ok
	}
	// argInt parses an integer from the args map. Uses json.Number-aware
	// handling: float64 inputs are bounds-checked against int64 to refuse
	// silent truncation of values like 1e20 down to a small int. Reject
	// rather than silently clamp -- a model emitting 1e20 is confused, and
	// silently turning that into 0 or some random int is worse than
	// surfacing the error.
	argInt := func(k string, def int) (int, error) {
		raw, ok := tc.Args[k]
		if !ok {
			return def, nil
		}
		switch v := raw.(type) {
		case float64:
			if v != v || v > 9.2233720368547758e18 || v < -9.2233720368547758e18 {
				return 0, fmt.Errorf("arg %q: numeric out of int64 range", k)
			}
			// Reject non-integers to avoid silent truncation surprises.
			if v != float64(int64(v)) {
				return 0, fmt.Errorf("arg %q: expected integer, got %v", k, v)
			}
			return int(v), nil
		case int:
			return v, nil
		case json.Number:
			n, err := v.Int64()
			if err != nil {
				return 0, fmt.Errorf("arg %q: %w", k, err)
			}
			return int(n), nil
		case string:
			if v == "" {
				return def, nil
			}
			var n int
			_, err := fmt.Sscanf(v, "%d", &n)
			if err != nil {
				return 0, fmt.Errorf("arg %q: %w", k, err)
			}
			return n, nil
		}
		return def, nil
	}

	switch tc.Name {
	case "ask_planner":
		if st.askPlannerCalls >= o.cfg.MaxPlanRevisions {
			return "", fmt.Errorf("ask_planner budget exhausted (%d)", o.cfg.MaxPlanRevisions)
		}
		st.askPlannerCalls++
		instr := argStr("instruction")
		if instr == "" {
			return "", fmt.Errorf("ask_planner: instruction required")
		}
		planText, err := o.callPlanner(ctx, st, tb, instr)
		if err != nil {
			return "", err
		}
		st.lastPlan = planText
		st.task.Plan = planText
		if err := o.cfg.Store.Save(st.task); err != nil {
			st.trace.Emit(trace.KindInfo, map[string]any{
				"save_error": err.Error(),
				"context":    "ask_planner persist plan",
			})
		}
		installPlanFromReply(st, planText)
		// ask_planner does NOT count toward workOps: the done() guard wants
		// to see real progress (code, writes, tests), and a planner call alone
		// could otherwise satisfy the guard for a model that emits plan->done
		// with no implementation.
		return planText, nil

	case "ask_coder":
		if st.askCoderCalls >= o.cfg.MaxCoderRetries {
			return "", fmt.Errorf("ask_coder budget exhausted (%d)", o.cfg.MaxCoderRetries)
		}
		st.askCoderCalls++
		instr := argStr("instruction")
		plan := argStr("plan")
		if plan == "" {
			plan = st.lastPlan
		}
		if instr == "" {
			return "", fmt.Errorf("ask_coder: instruction required")
		}
		// Retry-after-test-failure grants fs.read + fs.search access per brief.
		retryMode := st.lastTestFailed
		code, err := o.callCoder(ctx, st, tb, instr, plan, retryMode)
		if err != nil {
			return "", err
		}
		// Clear the retry flag after we've served one retry.
		if retryMode {
			st.lastTestFailed = false
		}
		st.workOps++
		// AUTO-EXTRACT-AND-COMMIT: parse `# file: <path>` markdown-fenced
		// code blocks from the coder's reply and write them via the
		// toolbox. Observed in rematch #5 with Qwen3-Coder-30B-A3B: the
		// coder produced excellent multi-file responses (18-20 KB blocks
		// with proper `# file:` markers), but the reviewer (Gemma-4-26B)
		// hallucinated progress (`I have implemented the API`) and
		// dispatched ask_coder again instead of extracting and writing
		// the files. Doing the extract here removes that failure mode
		// from the reviewer's plate entirely.
		//
		// Write errors are surfaced back to the model in the result so it
		// can decide what to do (most often: ask_coder again with a
		// narrower instruction). Successes are summarized as a leading
		// "AUTO-COMMITTED: ..." line; the full coder reply still follows
		// so the reviewer can read the prose for context.
		commitSummary := autoExtractAndCommit(tb, code, st)
		if commitSummary != "" {
			return commitSummary + "\n\n" + code, nil
		}
		return code, nil

	case "fs.read":
		path := argStr("path")
		// Reject directory reads early so the model doesn't waste a turn on
		// an "is a directory" syscall error. fs.search is the right tool for
		// directory-level inspection. Resolve relative paths against the
		// repo root so the stat covers the same target the toolbox will
		// open.
		if path != "" {
			candidate := path
			if !filepath.IsAbs(candidate) {
				candidate = filepath.Join(tb.RepoRoot, candidate)
			}
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return "", fmt.Errorf("fs.read: path is a directory; use fs.search instead")
			}
		}
		res, err := tb.FSRead(path, 1<<17)
		if err != nil {
			return "", err
		}
		return res.Content, nil

	case "fs.write":
		// Reject malformed calls that omit the content key entirely. An
		// LLM that emits {"path": "foo.go"} (with no content field) used
		// to truncate the target file silently because argStr defaults to
		// "" and that is a legitimate "wipe this file" request. Distinguish
		// "explicitly empty content" (allowed) from "missing key" (rejected).
		if !argHas("content") {
			return "", fmt.Errorf("fs.write: missing required arg: content (use \"\" to write an empty file)")
		}
		content := argStr("content")
		path := argStr("path")
		// AUDIT-ONLY MODE gate: only the audit state files
		// (.agent-router/checklist.md, .agent-router/findings.md) may
		// be written -- the audited codebase MUST stay untouched.
		// The prompt rule "fs.write only for .agent-router/..." is a
		// soft defense; this is the orchestrator's hard one.
		// TODO(rename): the path strings still reference .agent-router/
		// pending the deferred filesystem-rename work.
		if st.auditMode && !isAuditStateFilePath(path) {
			return "", fmt.Errorf("fs.write: in AUDIT-ONLY mode, only .agent-router/checklist.md and .agent-router/findings.md may be written (refused write to %q)", path)
		}
		// Cap individual writes at fsWriteMaxBytes. Larger writes are
		// almost certainly a hallucinated "let me dump the entire codebase
		// into one file" mistake; let the model recover via a useful error
		// rather than corrupting the repo.
		if len(content) > fsWriteMaxBytes {
			return "", fmt.Errorf("fs.write: content exceeds %d-byte cap (got %d); split the file or trim the payload", fsWriteMaxBytes, len(content))
		}
		// Loop detection: reject the same (path, content-hash) pair if it
		// appears more than fsWriteRepeatCap times in the recent history.
		// Observed in rematch #3: Gemma-4-26B got into a 23-turn loop
		// re-writing frontend/src/api.ts with identical content because it
		// kept reading "OK wrote N bytes" as "the previous attempt failed,
		// try again". Surfacing a structured error breaks the loop and
		// nudges the model toward verification (test.run) or termination
		// (done / ask_coder for a different decision).
		hash := contentHash(content)
		if duplicateWriteCount(st.fsWriteHistory, path, hash) >= fsWriteRepeatCap {
			return "", fmt.Errorf("fs.write: rejected duplicate write to %s (you have written this exact content %d+ times in a row). Choose a different action: call test.run to verify, ask_coder for a code revision, or done if the work is complete", path, fsWriteRepeatCap)
		}
		res, err := tb.FSWrite(path, content)
		if err != nil {
			return "", err
		}
		// Record this write in the history ring. Keep at most
		// fsWriteHistoryLen entries; older entries roll off the front.
		st.fsWriteHistory = append(st.fsWriteHistory, fsWriteRecord{Path: res.Path, ContentHash: hash})
		if len(st.fsWriteHistory) > fsWriteHistoryLen {
			st.fsWriteHistory = st.fsWriteHistory[len(st.fsWriteHistory)-fsWriteHistoryLen:]
		}
		st.consecutiveFsWrites++
		// Tick the file off the structured plan (if loaded) and any
		// "verify: file:<path>" acceptance items. No-op when st.plan
		// is nil. Always emit the trace event (even on n=0) so we can
		// distinguish "matcher saw the path but plan paths don't align"
		// from "fs.write didn't run at all". Matches the auto-extract
		// path's diagnostic shape.
		if st.plan != nil {
			n := st.plan.MarkFileWritten(path)
			st.trace.Emit(trace.KindInfo, map[string]any{
				"checklist_ticked": n,
				"path":             path,
				"resolved":         res.Path,
				"source":           "fs_write",
			})
		}
		// The result string is what the RO reads as the tool return. Historical
		// form used `created=%v` which reasoning models misread as failure when
		// overwriting an existing file (the observed loop was Gemma-4-A4B
		// re-emitting a 64KB "the file wasn't created, try again" prose chain
		// for 4+ turns, bloating context until it overflowed 131k). Now we
		// surface an unambiguous status: OK with either "new" or "overwritten".
		what := "new"
		if !res.Created {
			what = "overwritten"
		}
		st.workOps++
		// After a healthy run of fs.writes, prepend a soft nudge to
		// encourage the reviewer to verify before continuing. Stops short
		// of rejection -- the model can ignore the nudge -- but biases
		// toward progress.
		suffix := ""
		if st.consecutiveFsWrites >= consecutiveFsWriteSoftCap {
			suffix = fmt.Sprintf(" (note: %d fs.writes in a row without verification; consider calling test.run or done next)", st.consecutiveFsWrites)
		}
		return fmt.Sprintf("OK wrote %d bytes to %s (%s)%s", res.Bytes, res.Path, what, suffix), nil

	case "fs.search":
		path := argStr("path_glob")
		if path == "" {
			path = argStr("path")
		}
		if path == "" {
			path = "."
		}
		pattern := argStr("pattern")
		// Empty patterns are rejected up-front so we don't surface an
		// opaque ripgrep "regex parse error" downstream. Whitespace-only
		// patterns are also rejected as obvious mistakes.
		if strings.TrimSpace(pattern) == "" {
			return "", fmt.Errorf("fs.search: pattern required (non-empty)")
		}
		hits, err := tb.FSSearch(ctx, pattern, path, 50)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(hits)
		return string(b), nil

	case "test.run":
		st.testRunCount++
		r, err := tb.TestRun(ctx)
		if err != nil {
			// Mark retry flag so next ask_coder gets read access.
			st.lastTestFailed = true
			return "", err
		}
		out := fmt.Sprintf("exit=%d\nstdout:\n%s\nstderr:\n%s",
			r.ExitCode, shorten(r.Stdout, 4000), shorten(r.Stderr, 2000))
		if r.ExitCode != 0 {
			st.lastTestFailed = true
		} else {
			st.lastTestFailed = false
			st.testRunPassCount++
			// Tick acceptance items with verify="test.run:pass". Always
			// emit the trace event when the plan is loaded and the run
			// succeeded -- distinguishes "no acceptance with that verify"
			// (n=0) from "test.run never succeeded". Matches fs.write /
			// auto_extract / rolling_compact diagnostic shape.
			if st.plan != nil {
				n := st.plan.MarkAcceptance("test.run:pass")
				st.trace.Emit(trace.KindInfo, map[string]any{
					"checklist_ticked": n,
					"verify":           "test.run:pass",
					"source":           "test_run",
				})
			}
		}
		st.workOps++
		return out, nil

	case "compile.run":
		st.compileRunCount++
		r, err := tb.CompileRun(ctx)
		if err != nil {
			return "", err
		}
		st.workOps++
		if r.ExitCode == 0 {
			st.compileRunPassCount++
			if st.plan != nil {
				n := st.plan.MarkAcceptance("compile.run:pass")
				st.trace.Emit(trace.KindInfo, map[string]any{
					"checklist_ticked": n,
					"verify":           "compile.run:pass",
					"source":           "compile_run",
				})
			}
		}
		return fmt.Sprintf("exit=%d\nstdout:\n%s\nstderr:\n%s",
			r.ExitCode, shorten(r.Stdout, 4000), shorten(r.Stderr, 2000)), nil

	case "pensive.search":
		// Hard cap: reviewers tend to over-call pensive.search as a
		// reflex (rematch #6 saw 5 calls back-to-back, rematch #7
		// hit 8). After maxPensiveSearchCalls, refuse with a
		// structured nudge so the model picks a different action.
		// Three calls is generous; if the model needs more memory
		// access, it can ask the human via fail() with a clear
		// reason.
		if st.pensiveSearchCalls >= maxPensiveSearchCalls {
			return "", fmt.Errorf("pensive.search: budget exhausted (%d calls already this task). Tool results from earlier turns are still in your context as <RESULT>...</RESULT> blocks; re-read those instead. Pick a different action: ask_coder, fs.write, fs.read, test.run, or done", maxPensiveSearchCalls)
		}
		st.pensiveSearchCalls++
		q := argStr("query")
		k, err := argInt("k", 5)
		if err != nil {
			return "", fmt.Errorf("pensive.search: %w", err)
		}
		project := argStr("project")
		if project == "" {
			project = filepath.Base(st.task.RepoRoot)
		}
		out, perr := aegis.PensiveSearchRaw(ctx, project, q, k)
		err = perr
		if err != nil {
			// Non-fatal: return a string the RO can see, not an error.
			return fmt.Sprintf("(pensive.search unavailable: %v)", err), nil
		}
		return out, nil

	case "pensive.emit_atom":
		// RO's hot-path learning. Writes a reasoning atom back to Engram so
		// future runs can retrieve what RO learned here. Self-gated: RO
		// decides when to emit. See system prompt for the "only non-obvious,
		// transferable lessons" bar.
		kind := argStr("kind")
		if kind == "" {
			kind = "discovery"
		}
		if kind != "discovery" && kind != "failure" && kind != "insight" {
			return "", fmt.Errorf("pensive.emit_atom: kind must be discovery|failure|insight, got %q", kind)
		}
		// "insight" shorthand doesn't exist in engram-emit; map to discovery
		// with a tag that flags it as a cross-project insight.
		engramKind := kind
		if kind == "insight" {
			engramKind = "discovery"
		}
		project := argStr("project")
		if project == "" {
			project = filepath.Base(st.task.RepoRoot)
		}
		principle := argStr("principle")
		if principle == "" {
			return "", fmt.Errorf("pensive.emit_atom: principle required")
		}
		contextStr := argStr("context")
		if contextStr == "" {
			contextStr = argStr("shape")
		}
		tagsCsv := argStr("tags")
		if kind == "insight" {
			if tagsCsv == "" {
				tagsCsv = "cross-project-insight"
			} else {
				tagsCsv = tagsCsv + ",cross-project-insight"
			}
		}
		// Append task provenance so we can filter self-emitted atoms later.
		if st.task != nil && st.task.ID != "" {
			if tagsCsv == "" {
				tagsCsv = "source:ro:" + st.task.ID
			} else {
				tagsCsv = tagsCsv + ",source:ro:" + st.task.ID
			}
		}
		out, err := aegis.PensiveEmit(ctx, engramKind, project, principle, contextStr, tagsCsv)
		if err != nil {
			return fmt.Sprintf("(pensive.emit_atom failed: %v)", err), nil
		}
		return out, nil
	}
	return "", fmt.Errorf("unknown tool: %s", tc.Name)
}

// callPlanner runs a single-turn planner call. The planner has fs.write access
// (per brief) so it can commit PLAN.md directly; we expose that via a one-shot
// tool loop capped at two turns (one fs.write, one terminal).
func (o *Orchestrator) callPlanner(ctx context.Context, st *runState, tb *toolbox.Toolbox, instruction string) (string, error) {
	tr := st.trace
	if _, err := o.cfg.ModelMgr.EnsureSlot(ctx, modelmgr.SlotPlanner); err != nil {
		return "", fmt.Errorf("ensure planner: %w", err)
	}
	tr.Emit(trace.KindSwap, map[string]any{"to": "planner", "reason": "ask_planner"})

	system := plannerSystemPrompt()
	user := fmt.Sprintf(
		"Task:\n%s\n\nRepo root: %s\n\nInstruction from the Reviewer-Orchestrator:\n%s\n\nProduce the plan now. When you are satisfied, optionally commit the plan by emitting <TOOL>{\"name\":\"fs.write\",\"args\":{\"path\":\"PLAN.md\",\"content\":\"...\"}}</TOOL>. Your plan text (the actual numbered steps) must be in the assistant reply itself; the Reviewer-Orchestrator reads it directly.",
		st.task.Description, st.task.RepoRoot, instruction,
	)
	messages := []modelmgr.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}

	// Short inner loop: plan text in turn 1, optional fs.write in turn 2.
	for i := 0; i < 2; i++ {
		resp, err := o.completeWithDeadline(ctx, modelmgr.ChatRequest{
			Messages:    messages,
			Temperature: 0.2,
			MaxTokens:   MaxTokensPlanner,
		})
		if err != nil {
			return "", fmt.Errorf("planner turn %d: %w", i, err)
		}
		tr.Emit(trace.KindLLMCall, map[string]any{
			"role":  "planner",
			"kind":  "p_reply",
			"turn":  i,
			"bytes": len(resp),
			"head":  shorten(resp, 400),
		})
		if call, ok := extractToolCall(resp); ok {
			// Only fs.write is permitted to the planner.
			if call.Name == "fs.write" {
				path, _ := call.Args["path"].(string)
				content, _ := call.Args["content"].(string)
				if _, err := tb.FSWrite(path, content); err != nil {
					tr.Emit(trace.KindToolCall, map[string]any{
						"kind":  "p_tool_call",
						"name":  "fs.write",
						"path":  path,
						"error": err.Error(),
					})
					// Strip the tool call from the plan text we return.
					return stripToolTags(resp), nil
				}
				tr.Emit(trace.KindToolCall, map[string]any{
					"kind":  "p_tool_call",
					"name":  "fs.write",
					"path":  path,
					"bytes": len(content),
				})
				return stripToolTags(resp), nil
			}
			// Planner emitted some other tool; ignore it, return the plan text.
			return stripToolTags(resp), nil
		}
		// No tool call at all is fine: planner is allowed to just return text.
		return resp, nil
	}
	return "", fmt.Errorf("planner: no response produced")
}

// callCoder runs the coder. The coder does NOT emit tool calls by default; it
// returns code as natural text (markdown fences). On retry-after-test-failure
// the coder is granted fs.read + fs.search and the Complete call is allowed
// to iterate a few inner turns to let it read files before producing code.
func (o *Orchestrator) callCoder(ctx context.Context, st *runState, tb *toolbox.Toolbox, instruction, plan string, retryMode bool) (string, error) {
	tr := st.trace
	if _, err := o.cfg.ModelMgr.EnsureSlot(ctx, modelmgr.SlotCoder); err != nil {
		return "", fmt.Errorf("ensure coder: %w", err)
	}
	tr.Emit(trace.KindSwap, map[string]any{"to": "coder", "reason": "ask_coder", "retry_mode": retryMode})

	system := coderSystemPrompt(retryMode)
	// Orchestrator-side enforcement of the reviewer prompt's
	// GOAL-DRIVEN EXECUTION rule ("name the test that proves it"):
	// surface the unsatisfied acceptance items the coder's work needs
	// to make tickable. The reviewer can dispatch ask_coder without
	// remembering to include this; doing it here makes the goal
	// visible to the coder regardless. Empty when no plan is loaded
	// or all acceptance is already satisfied.
	acceptanceCtx := unsatisfiedAcceptanceContextForCoder(st.plan)
	user := fmt.Sprintf(
		"Task:\n%s\n\nRepo root: %s\n\nPlan:\n%s\n%sInstruction from the Reviewer-Orchestrator:\n%s\n\nProduce the code now. Emit each file as a markdown-fenced block. The first line INSIDE the fence must be a comment naming the path, like:\n```python\n# file: src/foo.py\n...code...\n```",
		st.task.Description, st.task.RepoRoot, shorten(plan, 20000), acceptanceCtx, instruction,
	)
	messages := []modelmgr.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}

	maxTurns := 1
	if retryMode {
		maxTurns = 6 // allow fs.read / fs.search interleaved with final code
	}

	for i := 0; i < maxTurns; i++ {
		resp, err := o.completeWithDeadline(ctx, modelmgr.ChatRequest{
			Messages:    messages,
			Temperature: 0.1,
			MaxTokens:   MaxTokensCoder,
		})
		if err != nil {
			return "", fmt.Errorf("coder turn %d: %w", i, err)
		}
		tr.Emit(trace.KindLLMCall, map[string]any{
			"role":  "coder",
			"kind":  "c_reply",
			"turn":  i,
			"bytes": len(resp),
			"head":  shorten(resp, 400),
		})
		messages = append(messages, modelmgr.ChatMessage{Role: "assistant", Content: resp})

		if !retryMode {
			// Normal mode: take the response as final and stop.
			return resp, nil
		}

		// Retry mode: check for a read-only tool call; execute and loop.
		call, ok := extractToolCall(resp)
		if !ok {
			return resp, nil
		}
		switch call.Name {
		case "fs.read":
			path, _ := call.Args["path"].(string)
			res, err := tb.FSRead(path, 1<<17)
			payload := ""
			if err != nil {
				payload = fmt.Sprintf("<ERROR>%s</ERROR>", err.Error())
			} else {
				payload = fmt.Sprintf("<RESULT>%s</RESULT>", res.Content)
			}
			tr.Emit(trace.KindToolCall, map[string]any{
				"kind":  "c_tool_call",
				"name":  "fs.read",
				"path":  path,
				"bytes": len(payload),
				"error": errString(err),
			})
			messages = append(messages, modelmgr.ChatMessage{Role: "user", Content: payload})
			continue
		case "fs.search":
			pat, _ := call.Args["pattern"].(string)
			path, _ := call.Args["path_glob"].(string)
			if path == "" {
				path, _ = call.Args["path"].(string)
			}
			if path == "" {
				path = "."
			}
			hits, err := tb.FSSearch(ctx, pat, path, 50)
			payload := ""
			if err != nil {
				payload = fmt.Sprintf("<ERROR>%s</ERROR>", err.Error())
			} else {
				b, _ := json.Marshal(hits)
				payload = fmt.Sprintf("<RESULT>%s</RESULT>", string(b))
			}
			tr.Emit(trace.KindToolCall, map[string]any{
				"kind":    "c_tool_call",
				"name":    "fs.search",
				"pattern": pat,
				"bytes":   len(payload),
				"error":   errString(err),
			})
			messages = append(messages, modelmgr.ChatMessage{Role: "user", Content: payload})
			continue
		default:
			// Any other tool call is ignored; return the text so far.
			return resp, nil
		}
	}
	return "(coder exhausted retry-mode inner turns without producing final code)", nil
}

// fail finalises a run as failed and persists the error. If the run state
// indicates an operator abort (abortRequested set and the error came from
// ctx cancellation), the task is marked StatusAborted instead -- the run
// goroutine is the sole writer of Status, so this decision happens here
// rather than in Abort() to avoid racing the in-flight loop.
func (o *Orchestrator) fail(st *runState, t *taskstore.Task, tr *trace.Writer, err error) {
	if st != nil && st.abortRequested.Load() {
		t.Status = taskstore.StatusAborted
		t.LastError = err.Error()
		o.saveTaskOrLog(t, "task aborted")
		tr.Emit(trace.KindInfo, map[string]any{"aborted_by_user": true, "reason": err.Error()})
		o.setLastVerdict("aborted")
		tr.Emit(trace.KindVerdict, map[string]any{
			"phase":   "final",
			"verdict": "aborted",
			"reason":  err.Error(),
		})
		o.emitTaskEndAtom(context.Background(), st, t, "aborted", err.Error())
		return
	}
	t.Status = taskstore.StatusFailed
	t.LastError = err.Error()
	o.saveTaskOrLog(t, "task failed")
	tr.Emit(trace.KindError, map[string]any{"fatal": err.Error()})
	o.setLastVerdict("failed")
	tr.Emit(trace.KindVerdict, map[string]any{
		"phase":   "final",
		"verdict": "failed",
		"reason":  err.Error(),
	})
	if o.cfg.L2 != nil {
		_ = o.cfg.L2.RecordVerdict(t.RepoRoot, t.ID, "final", "failed", shorten(err.Error(), 4000))
	}
	// Pensive task-end atom for cross-task pattern recognition. Use
	// Background ctx since fail() runs at the END of the run goroutine
	// where the task ctx may already be cancelled (the cancellation is
	// often what triggered the fail). emitTaskEndAtom wraps a 5s
	// timeout internally so this can't extend the run lifetime.
	o.emitTaskEndAtom(context.Background(), st, t, string(t.Status), err.Error())
}

// --- system prompts --------------------------------------------------------

func plannerSystemPrompt() string {
	return `You are the Planner.
You receive a task description and produce a concise, numbered plan.

STANCE: You are PARANOID ABOUT AMBIGUITY. Every brief has a hidden
interpretation that a junior planner would silently choose. Your job
is to surface it before the Coder starts writing code that's wrong
in a way no test will catch. When the brief is clear, plan tight.
When the brief is vague, name the vague-ness in prose before the
JSON.

Rules:
  - 3 to 12 numbered steps, each an imperative sentence.
  - Identify files to touch and tests to run.
  - Be precise and unambiguous. The Reviewer-Orchestrator reads your plan
    and decides next steps; if a step is ambiguous it will ask you to
    clarify.
  - Your context is large; think thoroughly before writing.
  - You have NO tools. Do not attempt fs.read, fs.search, fs.write,
    test.run, shell.exec, or anything else. Your prose plan + the
    fenced JSON block at the end of your reply IS the entire output.

THINK BEFORE PLANNING (surface assumptions, do not hide confusion):
  - Before emitting the JSON plan, briefly state your top 1-2
    assumptions about the task in prose -- specifically the
    interpretive choices a reasonable engineer might disagree with.
  - If the task is genuinely ambiguous between two interpretations,
    list BOTH options and pick one with a one-line rationale. Don't
    silently choose. The reviewer can correct a stated assumption;
    it cannot correct a hidden one.
  - If a term in the brief is undefined ("real-time", "scalable",
    "production-ready"), name your operational definition.

PATH DISCIPLINE (CRITICAL):
  - Pick ONE canonical layout up front (e.g. "apps/web/..." OR "frontend/..."
    OR "client/..." -- choose, do not mix). The Coder will write files at
    EXACTLY the paths you list; if your JSON paths don't match the paths
    the Coder produces, the live checklist cannot tick them off and the
    Reviewer cannot tell what is done.
  - For monorepo projects, default to "apps/<name>/" for runnable apps
    and "packages/<name>/" for shared libraries unless the task says
    otherwise. State your layout choice explicitly in step 1 of the
    prose plan so the Coder follows it.
  - List files in dependency order: root config (package.json, tsconfig)
    BEFORE app entry points BEFORE leaf modules. The Coder builds top-down.

PHASE GRANULARITY:
  - Each phase produces something testable on its own (a passing build,
    a callable endpoint, a runnable script). The Reviewer should be able
    to verify a phase before moving on.
  - 5 to 12 files per phase is a healthy size. Bigger phases tend to
    starve the Coder's context window; smaller phases burn turns.

STRUCTURED OUTPUT (REQUIRED at end of reply):

After your prose plan, you MUST append a fenced JSON block describing the
plan in machine-readable form. The orchestrator parses it to drive a live
checklist the Reviewer sees on every turn AND to gate the done() tool on
real completion. Without this block, the Reviewer flies blind.

Schema (ALL field names are required; paths are REPO-RELATIVE):

  - phases[].id          short identifier, unique within the plan ("P1", "P2", ...)
  - phases[].name        human-readable phase name
  - phases[].files[].path        repo-relative path the Coder will write
  - phases[].files[].purpose     (optional) one-line note for the Reviewer
  - acceptance[].id          short identifier, unique ("A1", "A2", ...)
  - acceptance[].description human-readable criterion
  - acceptance[].verify      AUTO-TICK SHAPE (see below) -- never empty

Supported "verify" values (PICK ONE; never emit empty string):
  - "file:<repo-path>"     auto-tick when fs.write lands at <repo-path>
  - "test.run:pass"        auto-tick when test.run exits 0
  - "compile.run:pass"     auto-tick when compile.run exits 0

Empty verify ("") deadlocks the done() gate -- there is no manual
claim mechanism. Every acceptance criterion MUST encode as one of the
three auto-tick shapes above. If you cannot encode a criterion as
file:/test.run:pass/compile.run:pass, drop it.

Concrete example (this exact JSON parses cleanly -- copy the shape):

  ` + "```json" + `
  {
    "phases": [
      {
        "id": "P1",
        "name": "Scaffold",
        "files": [
          {"path": "package.json", "purpose": "workspace root"},
          {"path": "src/server.ts"}
        ]
      }
    ],
    "acceptance": [
      {"id": "A1", "description": "tsc --noEmit passes",       "verify": "compile.run:pass"},
      {"id": "A2", "description": "npm test passes",           "verify": "test.run:pass"},
      {"id": "A3", "description": "fixtures/sample.jsonl",     "verify": "file:fixtures/sample.jsonl"}
    ]
  }
  ` + "```" + `

Place the JSON in a fenced code block tagged json:

  ` + "```json" + `
  {"phases":[...], "acceptance":[...]}
  ` + "```" + `

The Reviewer-Orchestrator CANNOT call done() until every acceptance item
is satisfied. Make your acceptance list realistic: 3-8 items typically.

GOAL-DRIVEN ACCEPTANCE (every criterion is a test, not a vibe):
  - Prefer auto-tickable verifies ("test.run:pass", "compile.run:pass",
    "file:<concrete-path>") over informational ("") whenever the
    criterion has a mechanical definition.
  - Informational verify ("") is an escape hatch, not a default. If
    you cannot encode a criterion as one of the auto-tick shapes,
    pause and ask: is the criterion real, or is it vibes?
  - "It should be fast" / "It should be robust" / "It should be
    user-friendly" are NOT acceptance items. Either name a measurable
    proxy ("p99 latency &lt;100ms via test", "tsc --strict passes",
    "lighthouse score >= 90") or drop the criterion.
  - A plan with 3 verifiable acceptance items is stronger than a plan
    with 8 vibes.
`
}

func coderSystemPrompt(retryMode bool) string {
	base := `You are the Coder.
You receive a task description, a plan, and an instruction from the
Reviewer-Orchestrator. You produce the code files required by the plan.

OUTPUT FORMAT (READ CAREFULLY -- non-conformant output gets DROPPED).

Each file you produce MUST be wrapped in a markdown code fence whose
FIRST LINE inside the fence is a "# file:" or "// file:" comment naming
the file path. The orchestrator parses fenced blocks; markers OUTSIDE a
fence, JSON tool-call envelopes, or raw file content without a wrapping
fence ARE NOT EXTRACTED and the file is silently lost.

Concrete one-shot (THIS is the exact shape -- do not deviate):

Reviewer instruction: "Create package.json and src/index.ts."

Your reply (prose first, then ONE fence per file):

I'll create the two requested files.

` + "```json" + `
# file: package.json
{
  "name": "example",
  "version": "0.0.1"
}
` + "```" + `

` + "```typescript" + `
# file: src/index.ts
export function hello() {
  return "hi";
}
` + "```" + `

(end of one-shot)

RULES:
  - One fence per file. The "# file: <path>" comment is the FIRST LINE
    inside the fence (immediately after the opening triple-backtick line).
  - For multi-file responses: emit ONE fence per file, separated by
    BLANK LINES. Do NOT pack multiple files into a single fence body.
  - Path is REPO-RELATIVE. Never absolute. Never starts with "/".
  - Use any language tag after the opening backticks (json, typescript,
    python, go, sh, etc) -- the orchestrator ignores the tag and parses
    the body. The "# file:" comment line works in any language because
    "//" and "#" are both recognized comment syntaxes.
  - You do NOT call fs.write or emit JSON tool-call envelopes like
    {"name":"fs.write","arguments":...}. The orchestrator does NOT
    execute those; it extracts code from fenced blocks only. (RETRY
    MODE below grants read-only fs.read / fs.search; the no-fs.write
    rule still applies in retry mode.)
  - If a path comment is missing the file is discarded. Always include it.

SIMPLICITY FIRST (CRITICAL -- senior engineer test):
  - Minimum code that solves THIS request. Nothing speculative.
  - NO unrequested features. NO "flexibility" the reviewer didn't
    ask for. NO premature error handling for cases the brief
    doesn't mention.
  - Single-use abstraction = no abstraction. Inline it. Wait for
    the second caller before extracting a helper.
  - Would a senior engineer call this overcomplicated? If yes,
    cut until they wouldn't. The reviewer can ask for more; it
    cannot un-pay the cost of yours-truly-clever code.
  - Match the existing project's style. Don't introduce a new
    pattern (a new logger, a new error type, a new module layout)
    unless the plan explicitly told you to.

NESTED FENCES (markdown / shell heredocs whose body contains backtick
fences): use a LONGER outer fence so the inner triple-backtick does not
close the outer. CommonMark accepts 3+ backticks (or 3+ tildes) for a
fence; the closer must use the same character and be at LEAST as long
as the opener.

Concrete: a README.md whose body has an inner shell example needs four
backticks outer:

    ` + "````" + `md
    # file: README.md
    # Project

    ## Install

    ` + "```" + `bash
    npm install
    ` + "```" + `

    Done.
    ` + "````" + `

Or use ~~~ tildes for the outer fence -- they do not share characters
with the inner backtick fence, so no length escalation is needed:

    ~~~md
    # file: README.md
    ` + "```" + `bash
    npm install
    ` + "```" + `
    ~~~
`
	if retryMode {
		base += `
RETRY MODE (your previous code caused a test failure):
  - You have READ-ONLY access to fs.read and fs.search for this retry.
  - Use them to inspect the code the Reviewer-Orchestrator already wrote:
      <TOOL>{"name":"fs.read","args":{"path":"src/foo.py"}}</TOOL>
      <TOOL>{"name":"fs.search","args":{"pattern":"def bar","path":"src"}}</TOOL>
  - After you have enough context, produce a revised set of files in the
    same markdown-fenced format. Do NOT call fs.write; the orchestrator
    writes files.

SURGICAL CHANGES (retry-mode discipline):
  - Touch ONLY what the test failure requires. Do not refactor adjacent
    code. Do not "clean up" the prior structure even if you would have
    written it differently.
  - The retry should be a minimal patch, not a rewrite. If the original
    file was 200 lines and the fix is 3 lines, return the file with
    those 3 lines changed and everything else preserved verbatim.
  - Match the existing style. If the project uses snake_case, your
    additions use snake_case. If it uses tabs, you use tabs.
  - Clean up only YOUR mess (the code your prior turn introduced).
    Pre-existing dead code, unused imports, stale comments STAY.
    They are not in scope.
`
	}
	return base
}

// TODO(rename): the prompt below references the audit-mode checklist
// directory ".agent-router/" by literal path. When the on-disk dir migrates
// to ".moirai/", update the prompt strings here too.
func roSystemPrompt(auditMode bool) string {
	base := `You are the Reviewer-Orchestrator.
You coordinate the Planner (P) and the Coder (C) to complete the user's task.

STANCE: You are the human's GATEKEEPER. The planner produces, the
coder writes, YOU push back. Default to skepticism. A plan that
looks good on first read might be vague where it matters; a coder
output that compiles might have skipped the actual feature. Trust
nothing your subordinates say without a mechanical check (test.run,
compile.run, fs.read of the actual artifact). The done() gate is
the human's last line of defense -- guard it.

OUTPUT FORMAT (read carefully)

Each reply MUST contain EXACTLY one tool call wrapped in <TOOL>...</TOOL>
tags. The canonical form is a single JSON object inside the tags with
"name" and "args" fields:

  I will start by asking the planner for a build plan.
  <TOOL>{"name": "ask_planner", "args": {"instruction": "Lay out a 5-step plan for the task."}}</TOOL>

Hard rules:

  - The <TOOL> and </TOOL> tags are REQUIRED. JSON outside the tags is
    ignored. A bare JSON object with no tags is only accepted if it is
    the FIRST non-whitespace character of your reply.
  - Exactly ONE <TOOL> block per reply. Two or more blocks are rejected
    as ambiguous; the daemon will nudge you to retry with one.
  - The args object must be valid JSON (double-quoted strings, no
    trailing commas).
  - Think in prose BEFORE the tool call, not after. The parser scans
    from the front and does not accept tool calls embedded mid-prose
    if they are not the dominant content.

If a reply is rejected you will receive a nudge:
"No tool call detected. ..." Re-emit the SAME intended call in the
preferred form above. Do not switch tools or strategy on a parse error;
the only fix is the envelope.

Your tools (emit exactly one per turn, wrapped in <TOOL>...</TOOL>):
  ask_planner      args: {"instruction": "..."}
  ask_coder        args: {"instruction": "...", "plan": "..."}
  fs.write         args: {"path": "...", "content": "..."}
  fs.read          args: {"path": "..."}
  fs.search        args: {"pattern": "...", "path_glob": "."}
  test.run         args: {}
  compile.run      args: {}
  pensive.search   args: {"project": "...", "k": 5}
                   (returns the most-recent reasoning atoms for the named
                   project. The query parameter is currently ignored by the
                   pensive backend; results are recency-ordered, not query-
                   matched. Use a higher k if you need broader recall.)
  pensive.emit_atom args: {"kind": "discovery"|"failure"|"insight",
                           "principle": "transferable rule you learned",
                           "context": "situational shape (what the lesson is about)",
                           "tags": "comma,separated"}
  done             args: {"summary": "..."}
  fail             args: {"reason": "..."}

Workflow (you decide the order at each step):
  1. If you need a plan, call ask_planner with a concrete instruction.
  2. If the plan is unclear or wrong, call ask_planner again with
     specific feedback. You have a limited revision budget.
  3. When you have a plan you like, call ask_coder with the plan and any
     extra instructions (e.g. target files, test commands).
  4. The coder returns code as markdown-fenced blocks with a
     "# file: <path>" comment on the first line inside each fence. The
     orchestrator AUTO-COMMITS every such block to disk; you will see a
     leading "AUTO-COMMITTED N file(s)" summary in the result. Do NOT
     re-extract or fs.write those files yourself; they are already on
     disk. Use fs.read or test.run on the next turn to verify.
  5. After writing, call test.run. If tests pass, call done(summary).
     If tests fail, inspect the output. Decide whether to:
        - Call ask_coder again (the coder gets fs.read access on retry)
        - Call ask_planner for a plan revision if the spec is wrong
  6. If nothing progresses, call fail(reason) with a clear explanation.

REVIEW DISCIPLINE (the "review" in Reviewer-Orchestrator -- you are
not just a switchboard):

  IN A FRESH PLAN (after ask_planner returns):
   - Are acceptance items mechanical (file:/test.run:pass/compile.run:pass)
     or vibes? Vibes acceptance is a deferred deadlock; ask the planner
     to encode them as auto-tick shapes or drop the criterion.
   - Are files listed in dependency order? Root configs before app
     entry points before leaf modules. Out-of-order plans burn coder
     turns on missing-import errors.
   - Did the planner pick a canonical layout? If two paths drift between
     plan and coder output, the live checklist can't tick them off.
   - Is the scope what the brief asked for? A 100-file plan for a "tiny
     utility" brief is scope creep -- ask the planner to revise BEFORE
     dispatching the coder.

  IN A CODER OUTPUT (after ask_coder returns):
   - Did the path comments in coder output match the plan's paths? If
     they drift, the checklist misses ticks. Re-dispatch with explicit
     canonical paths in the next ask_coder instruction.
   - Did the coder introduce abstractions the plan didn't ask for? A
     new logger, a new error type, a custom dependency-injection layer?
     That's speculative scope creep. Push back: "remove X; the plan
     didn't request it."
   - In retry mode: did the coder do a surgical patch or a wholesale
     rewrite? A 200-line file rewritten for a 3-line fix violates
     SURGICAL CHANGES. Re-prompt.

  BEFORE done():
   - Did test.run actually fire this run? Check the conversation; do
     not infer "I'm sure tests would pass". The acceptance gate only
     ticks test.run:pass when test.run RUNS.
   - Are the acceptance ticks real or happenstance? compile.run:pass
     means "compiles", not "behaves correctly". For a feature task,
     you want a test that exercises the feature, not just compilation.

Rules:
  - Emit EXACTLY one <TOOL>...</TOOL> per turn. Think in prose before it.
  - After a tool call you will receive <RESULT>...</RESULT> or <ERROR>...</ERROR>.
  - The orchestrator AUTO-COMMITS every fenced "# file:" block from an
    ask_coder result before you see the result. Look for the leading
    "AUTO-COMMITTED N file(s) from coder reply:" summary -- those files
    are ALREADY on disk. Do NOT re-extract them. Do NOT call fs.write
    on the same paths. Move on to verification (test.run / compile.run)
    or the next ask_coder dispatch.
  - Do not call ask_planner or ask_coder after calling done or fail.
  - If test.run exits 0, the code works. If it exits non-zero the code is
    broken; do not emit done until you have a passing test run or you
    explicitly decide to fail.
  - Think before each action; explain your reasoning in the assistant
    message, then emit one tool call.
  - Memory augmentation via pensive: before significant decisions (a
    review verdict, a routing call between planner and coder, committing
    a file that looks unusual), you may call pensive.search to pull
    relevant prior atoms. Retrieval is cheap (single-digit ms).
    DO NOT call pensive.search on the very first turn of a fresh task,
    or to "retrieve" the result of a previous tool call. Tool results
    are already in your context as <RESULT>...</RESULT> blocks; reread
    them. pensive.search is for cross-task/cross-session memory only.
  - Memory accretion via pensive.emit_atom: after a decision where you
    noticed something non-obvious and transferable, emit an atom so a
    future version of you can retrieve the lesson. Emit sparingly and
    only for genuine signal. Routine decisions (approve, reject with
    standard reason) are not worth an atom. Good atoms name a specific
    pattern or pitfall. Bad atoms restate the obvious. Under-emit
    rather than over-emit; the corpus quality matters.

GOAL-DRIVEN EXECUTION (every dispatch carries its own success test):
  - When dispatching the coder for a FIX, prefer the form
    "write a failing test that reproduces this, then make the test
    pass" over a freeform "fix it" instruction. The test makes the
    fix verifiable; the freeform fix becomes vibes.
  - When dispatching for a FEATURE, lead with the test command that
    would prove it done. Hand the coder both: the feature description
    AND the assertion that proves it works.
  - Call test.run BEFORE done() at the end of every phase. The
    acceptance gate REQUIRES test.run:pass-style ticks to satisfy
    "test passes" criteria; that only fires if you actually call
    test.run. A mental "I'm sure the tests pass" does not tick the
    checklist.
  - "Fix the bug"  -> "write a failing test, then make it pass."
  - "Add a feature" -> "name the test that proves it works first."
  - "Make it faster" -> "what's the measurable threshold; what test
    enforces it?"
`
	if !auditMode {
		return base
	}
	return base + `
AUDIT-ONLY MODE (this task description began with "AUDIT-ONLY:")
The codebase already exists. Your job is to FIND BUGS, not write
fixes. Rules for audit mode:

  - DO NOT call ask_coder for code generation. DO NOT include "# file:"
    markers in any output. Auto-extract is your enemy here.
  - You may call ask_coder ONLY with audit framing: instruction must
    start with "AUDIT-ONLY. Persona: <name>." Coder must return a
    plain-text numbered findings list, no code blocks.
  - DO NOT call fs.write to modify project files. fs.write is allowed
    ONLY for ".agent-router/checklist.md" or ".agent-router/findings.md".
  - Use fs.read and fs.search liberally to inspect the code.
  - Use compile.run and test.run to gather mechanical bug evidence
    (tsc errors, lint warnings, failing tests, missing tests).
  - Run the audit cycle below. Each cycle, rotate to a different
    persona — never use the same persona twice in a row.
  - When you have completed the requested number of audit passes
    (default 3, configurable in task brief), call done(summary)
    where the summary is the consolidated bug list with severities.

AUDIT PERSONA ROTATION
Use these five framings, one per pass, never repeat consecutively:

  1. "security-OWASP" — review for injection, prototype pollution,
     deserialization issues, missing auth checks, secrets in code,
     unsafe eval/template paths, input validation gaps.
  2. "ci-flaky-test" — find non-determinism: time-of-day in tests,
     network calls without timeout, shared mutable state, ordering
     dependencies between tests, missing cleanup, flaky selectors.
  3. "junior-dev-clarity" — read each module fresh: where would a
     new contributor get confused? unclear names, hidden coupling,
     functions that do too much, conventions broken silently.
  4. "perf-hot-path" — find O(n^2) where O(n) suffices, sync I/O
     where async expected, repeated DB calls in a loop, allocations
     on a hot path, large objects passed by value.
  5. "ux-edge-cases" — empty state, slow network, browser back
     button, double-submit, malformed input, oversized input,
     missing loading/error states, aria gaps.

Each persona is a redirect of attention, not a different prompt
language. The same model gives different answers when primed with
different roles. Use the persona name verbatim in the ask_coder
instruction so the orchestrator can verify rotation.

MECHANICAL BUG EVIDENCE (run these EVERY audit cycle, before model read)
Before each persona's read, gather mechanical evidence and include
it in the ask_coder context:

  - compile.run output (tsc errors, build failures)
  - test.run output (failing tests, "no test files" warnings)
  - fs.search for: TODO, FIXME, XXX, placeholder, "not implemented"
  - fs.search for known anti-pattern strings appropriate to the language
    (e.g. "any" in TypeScript, "panic\\(" in Go, "rescue Exception"
    in Ruby, "except:" bare in Python). The Persona below dictates which.

Note: fs.search is content-only ripgrep -- it CANNOT list files or
detect duplicate basenames. If you want to find naming-convention
inconsistencies, fs.search for the file-path patterns themselves
(e.g. "from .*helpers" vs "from .*utils") in source code rather
than asking for a directory listing.

These tools find ~70% of real bugs without any model intuition.
The persona-driven read is the +30% that needs reasoning.

CHECKLIST DISCIPLINE
On turn 1 (or turn 2 if turn 1 was pensive.search), your first
fs.write MUST create ".agent-router/checklist.md" with:

  # Audit checklist
  ## Mechanical evidence
  - [ ] compile.run output captured
  - [ ] test.run output captured
  - [ ] fs.search for TODO/placeholder/etc captured
  - [ ] duplicate-basename scan captured
  ## Audit passes
  - [ ] Pass 1: <persona>
  - [ ] Pass 2: <persona>
  - [ ] Pass 3: <persona>
  ## Findings (running list)
  (append after each pass)

On every subsequent turn, fs.read this file BEFORE deciding the
next action. Update it as work completes. The checklist is the
flowchart; do not deviate from it.
`
}

// --- parsing helpers -------------------------------------------------------

func newTaskID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

type toolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// toolRE is tolerant: it accepts <TOOL>{...}</TOOL>, <TOOL>{...}<TOOL>
// (reasoning models sometimes emit two opening tags instead of an open/close
// pair), and even a bare {"name":"...","args":{...}} block. We prefer the
// tagged form when present.
var (
	toolREClosed = regexp.MustCompile(`(?s)<TOOL>\s*(\{.*?\})\s*</TOOL>`)
	toolREOpen   = regexp.MustCompile(`(?s)<TOOL>\s*(\{.*?\})\s*<TOOL>`)
	toolREStart  = regexp.MustCompile(`(?s)<TOOL>\s*(\{.*?\})\s*$`)

	// Shorthand form some reasoning models (Gemma-4-A4B-IQ4_XS observed
	// running the Observatory task) emit instead of the strict JSON form:
	//   <TOOL>ask_coder args: {"instruction": "..."}</TOOL>
	// The model treats the prompt's example as pseudo-syntax rather than
	// literal JSON. Rejecting this was costing us 30+ nudge turns before
	// ctx overflow. The capture is (name, args_json) which we stitch back
	// into a proper toolCall in extractToolCall.
	// `[\w.]+` so `pensive.search` / `fs.write` / `test.run` all match.
	toolREShorthand       = regexp.MustCompile(`(?s)<TOOL>\s*([\w.]+)\s+args\s*:\s*(\{.*?\})\s*</TOOL>`)
	toolREShorthandOpen   = regexp.MustCompile(`(?s)<TOOL>\s*([\w.]+)\s+args\s*:\s*(\{.*?\})\s*<TOOL>`)

	toolTagClose = regexp.MustCompile(`(?s)<TOOL>.*?</TOOL>`)
	toolTagOpen  = regexp.MustCompile(`(?s)<TOOL>.*?<TOOL>`)
)

// maxToolScanBytes caps how much of the model's response we feed to the
// regex engine. The lazy `.*?` patterns are bounded but the overall scan
// is still O(n) in the worst case, and the bare-JSON last-resort walk at
// the end of this function reads the whole string. 256KB is comfortably
// larger than any normal LLM reply we accept; anything larger is either
// a stream that already overflowed an earlier truncation guard or hostile
// input. We trim from the front (terminal tags live near the end).
const maxToolScanBytes = 256 * 1024

// ErrMultipleToolCalls is returned by extractToolCall when the model emitted
// more than one <TOOL>...</TOOL> block in the same response. The RO contract
// is one tool call per turn; surfacing this explicitly lets the caller send a
// recoverable error back to the model rather than silently picking the first
// match and dropping the rest.
var ErrMultipleToolCalls = errors.New("more than one tool call per response")

// extractToolCall returns the first valid tool call in the response, or false
// with no match. Returns (zero, false) and a sentinel error via the dedicated
// multi-call helper when more than one tagged tool block appears.
func extractToolCall(s string) (toolCall, bool) {
	tc, ok, _ := extractToolCallChecked(s)
	return tc, ok
}

// extractToolCallChecked is the multi-call-aware variant. The third return
// value is non-nil when the input contained more than one TOOL block; the
// caller can surface that as a recoverable model error. When err is nil the
// (toolCall, ok) pair has the same semantics as extractToolCall.
func extractToolCallChecked(s string) (toolCall, bool, error) {
	if len(s) > maxToolScanBytes {
		s = s[len(s)-maxToolScanBytes:]
	}
	// Multi-block detection: count strict-form <TOOL>...</TOOL> blocks. If
	// more than one is present, refuse to silently pick the first one.
	if matches := toolTagClose.FindAllStringIndex(s, -1); len(matches) > 1 {
		return toolCall{}, false, ErrMultipleToolCalls
	}
	// Shorthand form: <TOOL>name args: {...}</TOOL>. Check these FIRST so a
	// well-formed shorthand wins over the last-resort bare-JSON scan below.
	for _, re := range []*regexp.Regexp{toolREShorthand, toolREShorthandOpen} {
		m := re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		name := m[1]
		argsJSON := m[2]
		var args map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			if cleaned, ok := balancedObject(argsJSON); ok {
				if err := json.Unmarshal([]byte(cleaned), &args); err != nil {
					continue
				}
			} else {
				continue
			}
		}
		if args == nil {
			args = map[string]any{}
		}
		return toolCall{Name: name, Args: args}, true, nil
	}

	for _, re := range []*regexp.Regexp{toolREClosed, toolREOpen, toolREStart} {
		m := re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		var tc toolCall
		if err := json.Unmarshal([]byte(m[1]), &tc); err != nil {
			// The JSON inside the tags might have a trailing ellipsis or
			// commentary; try one recovery pass with braces-only.
			if cleaned, ok := balancedObject(m[1]); ok {
				if err := json.Unmarshal([]byte(cleaned), &tc); err != nil {
					continue
				}
			} else {
				continue
			}
		}
		if tc.Name == "" {
			continue
		}
		if tc.Args == nil {
			tc.Args = map[string]any{}
		}
		return tc, true, nil
	}
	// Tag-bounded balanced-JSON fallback: any `<TOOL>...</TOOL>` (or `<TOOL>...`
	// open variant) with a balanced JSON object inside, regardless of whether
	// the name is encoded as a JSON `"name"` field or as a leading bareword and
	// the args use `:`, `=`, or quoted-JSON syntax. This catches Gemma /
	// Mistral / GPT-OSS variants the strict regexes miss, e.g.:
	//   <TOOL>ask_planner args='{"instruction":"..."}'</TOOL>
	//   <TOOL>ask_planner args={"instruction":"..."}</TOOL>
	//   <TOOL>ask_planner {"instruction":"..."}</TOOL>
	//   <TOOL>{"name":"ask_planner","args":{...}}</TOOL>  (when lazy regex misfires on nested `}`)
	// Safe vs prose-injection: still requires the literal `<TOOL>` tag, which
	// pass-5 neutralizeEnvelopeTags rewrites out of any tool output before the
	// model ever sees it (so a planted file cannot smuggle `<TOOL>` back in).
	if start := strings.Index(s, "<TOOL>"); start >= 0 {
		inner := s[start+len("<TOOL>"):]
		if end := strings.Index(inner, "</TOOL>"); end >= 0 {
			inner = inner[:end]
		} else if reopen := strings.Index(inner, "<TOOL>"); reopen >= 0 {
			inner = inner[:reopen]
		}
		// Try whole-object form first: inner contains {"name":"X","args":{...}}.
		if obj, ok := balancedObject(inner); ok {
			var tc toolCall
			if err := json.Unmarshal([]byte(obj), &tc); err == nil && tc.Name != "" {
				if tc.Args == nil {
					tc.Args = map[string]any{}
				}
				return tc, true, nil
			}
			// Fall through: maybe the JSON IS the args and the name is a
			// leading bareword. Parse the prefix.
			if name := extractLeadingName(inner); name != "" && isKnownToolName(name) {
				var args map[string]any
				if err := json.Unmarshal([]byte(obj), &args); err == nil {
					if args == nil {
						args = map[string]any{}
					}
					return toolCall{Name: name, Args: args}, true, nil
				}
			}
		}
	}
	// Last resort: only fire if the response begins with `{` after stripping
	// leading whitespace AND no <TOOL> block was found. The previous version
	// scanned the entire response for any "name":... + balanced JSON, which
	// happily executed prose like:
	//   I am thinking about calling { "name": "fail", ... } but I won't
	// Scoping to a leading bare-JSON object eliminates that prose-execution
	// attack surface while still accepting models that emit a strict JSON
	// reply with no tag wrapper.
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if strings.HasPrefix(trimmed, "{") {
		if obj, ok := balancedObject(trimmed); ok {
			var tc toolCall
			if err := json.Unmarshal([]byte(obj), &tc); err == nil && tc.Name != "" {
				if tc.Args == nil {
					tc.Args = map[string]any{}
				}
				return tc, true, nil
			}
		}
	}
	return toolCall{}, false, nil
}

// extractLeadingName returns the first bareword (letters, digits, `.`, `_`)
// at the start of s, with leading whitespace skipped. Empty string if the
// first non-whitespace character is not name-shaped.
func extractLeadingName(s string) string {
	s = strings.TrimLeft(s, " \t\r\n")
	end := 0
	for end < len(s) {
		c := s[end]
		isLetter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if isLetter || isDigit || c == '.' || c == '_' {
			end++
			continue
		}
		break
	}
	return s[:end]
}

// isKnownToolName guards the bareword-prefix branch of the parser so a
// random word followed by a balanced JSON object cannot be coerced into a
// tool dispatch. Keep this list in sync with executeROTool's switch.
func isKnownToolName(name string) bool {
	switch name {
	case "ask_planner", "ask_coder",
		"fs.read", "fs.write", "fs.search",
		"test.run", "compile.run",
		"pensive.search", "pensive.emit_atom",
		"done", "fail":
		return true
	}
	return false
}

// balancedObject walks the string looking for the first balanced {...} and
// returns it. Useful when model output has trailing junk inside the tag.
func balancedObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	for i := start; i < len(s); i++ {
		if s[i] == '{' {
			depth++
		} else if s[i] == '}' {
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// stripToolTags removes any <TOOL>...</TOOL> or <TOOL>...<TOOL> blocks from a
// planner reply so the RO sees plain plan text.
func stripToolTags(s string) string {
	s = toolTagClose.ReplaceAllString(s, "")
	s = toolTagOpen.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// codeFenceRE is retained for completeness but the live extractor uses
// findFenceBlocks() (a line-aware walker) for correct handling of nested
// fences. The non-greedy regex truncates README/markdown bodies at the
// first inner fence -- audit-pass-1 ADV-04. Replaced with a CommonMark-
// style fence walker that matches close fence to open fence by length:
// a 4-backtick outer fence is NOT closed by a 3-backtick inner fence, so
// a coder emitting markdown content can use ```` outer + ``` inner.
var codeFenceRE = regexp.MustCompile("(?s)```[a-zA-Z0-9_+\\-]*\\s*\\n?(.*?)```")

// fileMarkerRE matches the `# file: <path>` or `// file: <path>` line
// the coder uses to name a file inside a code fence. Extracted from
// rematch #5 traces against Qwen3-Coder-30B-A3B and gpt-oss-20b: both
// emit `# file: foo.ts` as the first line inside a fence, sometimes
// preceded by whitespace.
var fileMarkerRE = regexp.MustCompile(`(?m)^\s*(?:#|//)\s*file:\s*([^\s\n]+)\s*$`)

// codeFileExtraction is a parsed (path, content) pair extracted from a
// markdown-fenced code block in the coder's reply.
type codeFileExtraction struct {
	Path    string
	Content string
}

// extractFileBlocks walks a coder reply and returns every (path, content)
// pair where a fence body contains one or more `# file: <path>` markers.
// Files without the marker are skipped (the coder might emit examples or
// snippets the reviewer should read but not commit).
//
// Multi-marker fences: a single fence containing multiple `# file:`
// markers is split into one extraction per marker, BUT only when the
// marker line stands alone as a structural file-boundary -- i.e. it is
// either the first non-blank line of the fence body, OR it is preceded
// by a blank line. This guard avoids false positives where a `# file:`
// substring appears inside a Go raw string, a Markdown body, a heredoc,
// or any other multi-line string literal whose content contains the
// marker pattern but should NOT split the surrounding file.
//
// Pre-fix (single-marker, non-greedy regex): an embedded `# file:` inside
// content silently truncated the file at the embedded marker. Post-multi-
// marker (no boundary check): the embedded marker actively spawned a
// phantom file. Audit pass-2 caught the regression; this revision adds
// the boundary check so neither failure mode applies.
func extractFileBlocks(reply string) []codeFileExtraction {
	bodies := findFenceBlocks(reply)
	out := make([]codeFileExtraction, 0, len(bodies))
	for _, raw := range bodies {
		// Strip a leading UTF-8 BOM from the fence body. Go's `\s` does
		// not match the BOM (0xEF 0xBB 0xBF), so a fence body that
		// begins with one would yield zero marker matches and the file
		// would be silently dropped. Closes audit-pass-3 P3-MIN-1.
		body := strings.TrimPrefix(raw, "\ufeff")
		// Find ALL candidate `# file:` markers, then keep only the ones
		// that look like a true file-boundary line (see fileMarkerIsBoundary).
		allIdxs := fileMarkerRE.FindAllStringIndex(body, -1)
		allSubs := fileMarkerRE.FindAllStringSubmatch(body, -1)
		var markerIdxs [][]int
		var markerSubs [][]string
		for j, idx := range allIdxs {
			if fileMarkerIsBoundary(body, idx[0]) {
				markerIdxs = append(markerIdxs, idx)
				markerSubs = append(markerSubs, allSubs[j])
			}
		}
		if len(markerIdxs) == 0 {
			// Fallback: try to parse the fence body as an OpenAI-style
			// fs.write tool call. Qwen3-Coder-30B-A3B-Instruct (rematch
			// #22) emits structured JSON of the shape:
			//   {"name":"fs.write","arguments":{"path":"...","content":"..."}}
			// instead of the markdown `# file:` marker convention. We
			// recognize this pattern and return it as a single file
			// extraction so the rest of the auto-extract path doesn't
			// need to know about model dialects. Multiple JSON tool
			// calls in one fence are NOT supported -- the model would
			// have to use multi-fence output (which we already handle).
			if extracted, ok := extractFromOpenAIToolCallJSON(body); ok {
				out = append(out, extracted)
			}
			continue
		}
		for j, idx := range markerIdxs {
			path := strings.TrimSpace(markerSubs[j][1])
			if !validExtractionPath(path) {
				continue
			}
			// Content starts after the marker line, ends at the next
			// boundary marker (if any) or the end of body.
			contentStart := idx[1]
			contentEnd := len(body)
			if j+1 < len(markerIdxs) {
				contentEnd = markerIdxs[j+1][0]
			}
			bodyOut := strings.TrimPrefix(body[contentStart:contentEnd], "\n")
			// Trim trailing whitespace on the segment so each extracted
			// file ends cleanly without bleeding into the next marker's
			// leading blank lines (and so the last marker's content
			// doesn't carry the closing fence's trailing whitespace).
			bodyOut = strings.TrimRight(bodyOut, " \t\r\n")
			out = append(out, codeFileExtraction{
				Path:    path,
				Content: bodyOut + "\n", // ensure single trailing newline
			})
		}
	}
	return out
}

// findFenceBlocks walks `reply` line-by-line and returns the body of each
// well-formed markdown code fence, where a fence is "well-formed" by the
// CommonMark rule: the closing fence must use the SAME fence character
// (backtick or tilde) and be AT LEAST as long as the opening fence. This
// gives coders a way to emit markdown / shell-heredoc content (which itself
// contains ``` lines) by using a longer outer fence. A 4-backtick outer
// fence treats embedded 3-backtick lines as content, not as closers.
//
// Closes audit-pass-1 ADV-04. The previous regex-based extractor was
// non-greedy and stopped at the first inner fence, silently truncating
// README.md / docs / shell-heredoc files that contained their own fences.
//
// Returns just the body text (between opener and closer lines, exclusive
// of both). Newlines preserved. Lines INSIDE a fence are not scanned for
// further fences -- once we open at depth 1, we only look for the matching
// close.
func findFenceBlocks(reply string) []string {
	lines := strings.Split(reply, "\n")
	var out []string
	for i := 0; i < len(lines); {
		ch, n, ok := parseFenceOpener(lines[i])
		if !ok {
			i++
			continue
		}
		// Found opener at line i. Scan forward for matching closer.
		bodyStart := i + 1
		bodyEnd := -1
		for j := bodyStart; j < len(lines); j++ {
			if isMatchingFenceCloser(lines[j], ch, n) {
				bodyEnd = j
				break
			}
		}
		if bodyEnd < 0 {
			// Unclosed fence -- skip the opener line and continue scanning.
			i++
			continue
		}
		out = append(out, strings.Join(lines[bodyStart:bodyEnd], "\n"))
		i = bodyEnd + 1
	}
	return out
}

// parseFenceOpener returns (fence-char, count, true) if `line` is a valid
// markdown fence opener: 3+ consecutive backticks or tildes at the start,
// optionally followed by an info string (language tag) and trailing
// whitespace. (false otherwise.)
func parseFenceOpener(line string) (byte, int, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if len(trimmed) < 3 {
		return 0, 0, false
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	count := 0
	for count < len(trimmed) && trimmed[count] == ch {
		count++
	}
	if count < 3 {
		return 0, 0, false
	}
	// Rest of the line (after the fence chars) is the info string. Per
	// CommonMark a backtick opener cannot contain backticks in its info
	// string, but we are forgiving here -- the marker we care about is
	// the language tag, and we don't reject malformed openers.
	return ch, count, true
}

// isMatchingFenceCloser reports whether `line` is a valid closer for an
// opener that used `openerChar` repeated `openerCount` times. Per
// CommonMark the closer must use the same character, be at least as long,
// and have ONLY whitespace after the fence run.
func isMatchingFenceCloser(line string, openerChar byte, openerCount int) bool {
	trimmed := strings.TrimLeft(line, " \t")
	count := 0
	for count < len(trimmed) && trimmed[count] == openerChar {
		count++
	}
	if count < openerCount {
		return false
	}
	rest := trimmed[count:]
	for _, c := range rest {
		if c != ' ' && c != '\t' && c != '\r' {
			return false
		}
	}
	return true
}

// extractFromOpenAIToolCallJSON recognizes the OpenAI-style structured tool
// call format that some coder models (Qwen3-Coder-30B observed in rematch
// #22) emit instead of the documented `# file:` marker convention. It
// accepts both shapes:
//
//	{"name":"fs.write","arguments":{"path":"...","content":"..."}}
//	{"path":"...","content":"..."}                              // bare args
//
// Plus a couple of common variants: "args" instead of "arguments", and
// nested "function" key (older OpenAI tool-call dialect). Returns the
// extracted (path, content) and true on success; false otherwise.
//
// Path validation is the same as marker-extracted files: absolute paths
// and traversal segments are rejected; the returned path is the planner-
// canonical form (relative-to-repo) the toolbox.FSWrite expects.
func extractFromOpenAIToolCallJSON(body string) (codeFileExtraction, bool) {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "{") {
		return codeFileExtraction{}, false
	}
	type argShape struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	type wrapShape struct {
		Name      string    `json:"name"`
		Arguments argShape  `json:"arguments,omitempty"`
		Args      argShape  `json:"args,omitempty"`
		Function  *wrapShape `json:"function,omitempty"`
		// Bare-args fields (when the JSON is JUST args, not wrapped).
		Path    string `json:"path,omitempty"`
		Content string `json:"content,omitempty"`
	}
	var w wrapShape
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		return codeFileExtraction{}, false
	}
	// Drill into nested shapes.
	for w.Function != nil {
		w = *w.Function
	}
	// Pull args from whichever variant the model used.
	var path, content string
	switch {
	case w.Arguments.Path != "" || w.Arguments.Content != "":
		path, content = w.Arguments.Path, w.Arguments.Content
	case w.Args.Path != "" || w.Args.Content != "":
		path, content = w.Args.Path, w.Args.Content
	case w.Path != "" || w.Content != "":
		path, content = w.Path, w.Content
	default:
		return codeFileExtraction{}, false
	}
	// Only honor fs.write-style extractions. If `name` is set and is NOT
	// fs.write, treat as not-a-file. (When name is empty -- the bare-args
	// shape -- we accept by default since the path/content presence is
	// itself the signal.)
	if w.Name != "" && w.Name != "fs.write" {
		return codeFileExtraction{}, false
	}
	// Strip an absolute-path prefix that some models emit (e.g.
	// /home/aegis/Projects/traceforge-ar/apps/server/package.json). The
	// toolbox would resolve absolutes to the same place, but
	// validExtractionPath rejects HasPrefix("/"), so we'd lose otherwise
	// valid extractions. Heuristic: if path is absolute and contains
	// "/Projects/" with a trailing repo-shaped segment, take everything
	// after the first repo segment. More conservative: simply reject and
	// let the model retry. We pick conservative for now (consistent with
	// the auditor's path-discipline philosophy); the planner prompt
	// already pushes models toward relative paths.
	if !validExtractionPath(path) {
		return codeFileExtraction{}, false
	}
	if content == "" {
		return codeFileExtraction{}, false
	}
	// Ensure single trailing newline (consistent with marker extraction).
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return codeFileExtraction{Path: path, Content: content}, true
}

// fileMarkerIsBoundary reports whether the marker at byte position `pos`
// in `body` is in a position that legitimately starts a new file segment.
//
// The fileMarkerRE regex begins with `^\s*` in (?m) mode, which means
// the match position can include leading whitespace that spans the
// preceding `\n`. We first walk pos forward past any whitespace to find
// the actual `#` or `/` character; then we look at the line containing
// that character.
//
// Boundary rules:
//   - First marker (at start of body, ignoring leading whitespace): always
//     a boundary.
//   - Subsequent marker: previous non-whitespace line either does not
//     exist (blank-line separated from the body start) OR consists only
//     of whitespace (blank line separator).
//
// Otherwise the marker is considered embedded in surrounding content
// (string literal, heredoc body, etc.) and is NOT a split point.
func fileMarkerIsBoundary(body string, pos int) bool {
	// 1. Skip leading whitespace from pos to find the actual '#' or '/'
	//    character that starts the marker proper.
	hashPos := pos
	for hashPos < len(body) {
		c := body[hashPos]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			hashPos++
			continue
		}
		break
	}
	// 2. Walk back from hashPos to find the start of the line containing it.
	lineStart := hashPos
	for lineStart > 0 && body[lineStart-1] != '\n' {
		lineStart--
	}
	// 3. If lineStart is at body start (or only whitespace before it):
	//    boundary.
	if lineStart == 0 {
		return true
	}
	// 4. Look at the previous line. The char at lineStart-1 is the
	//    newline ending the previous line.
	prevEnd := lineStart - 1
	prevStart := prevEnd
	for prevStart > 0 && body[prevStart-1] != '\n' {
		prevStart--
	}
	prevLine := body[prevStart:prevEnd]
	// Strip a trailing \r if present (Windows line endings).
	prevLine = strings.TrimRight(prevLine, "\r")
	for _, c := range prevLine {
		if c != ' ' && c != '\t' {
			return false
		}
	}
	return true
}

// validExtractionPath enforces the path-shape constraints the auto-extract
// loop relies on. Centralized so the same checks apply uniformly across
// every marker we find in a fence (including multi-marker splits).
//
// Reject:
//   - empty path
//   - absolute path (HasPrefix "/")
//   - any segment equal to ".." (true traversal; "..." or "a..b" are fine)
//   - control characters (rune < 0x20 except tab — tabs in paths are
//     suspicious anyway, but the path-segment constraint already catches
//     them downstream)
//
// The toolbox FSWrite enforces the same rules, but failing fast here gives
// the reviewer a clear "extraction skipped" signal rather than a generic
// FSWrite error per file.
func validExtractionPath(path string) bool {
	if path == "" {
		return false
	}
	if strings.HasPrefix(path, "/") {
		return false
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return false
		}
	}
	for _, r := range path {
		if r < 0x20 {
			return false
		}
	}
	return true
}

// autoExtractAndCommit parses `# file:`-marked code blocks from the
// coder's reply and commits each one via the toolbox's FSWrite. Returns
// a human-readable summary of what was committed (or empty string if no
// extractable blocks were found). Errors during individual writes are
// included in the summary; the operation is best-effort and does not
// abort on the first error.
//
// Why this exists: the spec assigns extract-and-commit to the reviewer,
// but in practice reviewer models with weaker instruction-following
// (Gemma-4-26B at IQ4_XS, observed in rematch #5) hallucinate progress
// instead of running the extract-and-write loop. Doing it here
// guarantees the artifact lands on disk regardless of reviewer behavior.
// maxFilesPerCoderReply is the per-reply cap on the number of file
// extractions autoExtractAndCommit will commit. Closes audit-pass-1
// ADV-07: a hallucinating coder dumping thousands of fenced blocks
// would have written all of them, bloating git history and stressing
// the filesystem. 64 is plenty for a healthy multi-file phase emit
// (rematch traces show real coders producing 5-15 files per turn at
// peak); anything above the cap is rejected with a structured error
// nudging the reviewer to re-prompt with a smaller scope.
const maxFilesPerCoderReply = 64

func autoExtractAndCommit(tb *toolbox.Toolbox, reply string, st *runState) string {
	// AUDIT-ONLY MODE gate: in audit mode the orchestrator MUST NOT
	// write coder-emitted "# file:" markers into the audited codebase.
	// The reviewer prompt asks the coder for plain-text findings, but
	// a model that violates the rule and emits a fence anyway would
	// otherwise have its output committed to disk. Same shape as
	// P3-CRIT-1 (rolling-compact false-positive on fs.read content):
	// prompt rules are a soft defense; orchestrator enforcement is hard.
	if st != nil && st.auditMode {
		// Surface the gate to the trace so an audit replay can
		// diagnose "why didn't the file appear" without confusion.
		if files := extractFileBlocks(reply); len(files) > 0 && st.trace != nil {
			st.trace.Emit(trace.KindInfo, map[string]any{
				"audit_extract_blocked": true,
				"would_have_written":    len(files),
			})
		}
		return ""
	}
	files := extractFileBlocks(reply)
	if len(files) == 0 {
		return ""
	}
	// ADV-07 cap: refuse a hallucinated megablast.
	var rejectedByCap int
	if len(files) > maxFilesPerCoderReply {
		rejectedByCap = len(files) - maxFilesPerCoderReply
		files = files[:maxFilesPerCoderReply]
	}
	var committed []string
	var failed []string
	for _, f := range files {
		// Cap each individual write at the same byte budget the manual
		// fs.write tool enforces. Coder replies that try to dump
		// megabytes of code into a single file are almost always a
		// hallucination; reject those rather than corrupt the repo.
		if len(f.Content) > fsWriteMaxBytes {
			failed = append(failed, fmt.Sprintf("%s: content exceeds %d-byte cap", f.Path, fsWriteMaxBytes))
			continue
		}
		// Auto-extract loop guard: the explicit fs.write tool path has
		// duplicateWriteCount + fsWriteRepeatCap (line ~1556) to refuse
		// repeated writes of the same (path, content) pair, preventing
		// the rematch-#3 "23-times-the-same-file" loop. Auto-extract
		// previously bypassed that guard entirely; a coder dumping the
		// same package.json on every turn would silently re-overwrite
		// it. Apply the same guard here so loop detection covers BOTH
		// write paths.
		hash := contentHash(f.Content)
		if st != nil {
			if duplicateWriteCount(st.fsWriteHistory, f.Path, hash) >= fsWriteRepeatCap {
				failed = append(failed, fmt.Sprintf(
					"%s: rejected duplicate auto-extract (this content has been written %d+ times). The reviewer should call test.run / ask_coder for a different decision / done",
					f.Path, fsWriteRepeatCap))
				continue
			}
		}
		res, err := tb.FSWrite(f.Path, f.Content)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		// Record this write in the loop-detection ring. Identical to the
		// explicit fs.write path's bookkeeping; both write paths share
		// one history so a model alternating between them still trips
		// the cap.
		if st != nil {
			st.fsWriteHistory = append(st.fsWriteHistory, fsWriteRecord{Path: res.Path, ContentHash: hash})
			if len(st.fsWriteHistory) > fsWriteHistoryLen {
				st.fsWriteHistory = st.fsWriteHistory[len(st.fsWriteHistory)-fsWriteHistoryLen:]
			}
		}
		what := "new"
		if !res.Created {
			what = "overwritten"
		}
		committed = append(committed, fmt.Sprintf("%s (%d bytes, %s)", res.Path, res.Bytes, what))
		// Tick the file off the structured plan (if loaded). Emit a
		// trace event regardless of whether anything ticked, so we can
		// distinguish "auto-extract is calling but plan path doesn't
		// match" (n=0) from "auto-extract isn't running at all" (no
		// event at all). Use the resolved path res.Path -- f.Path may
		// be a planner-style relative path, FSWrite returns the form
		// the planner specified.
		if st != nil && st.plan != nil {
			n := st.plan.MarkFileWritten(f.Path)
			st.trace.Emit(trace.KindInfo, map[string]any{
				"checklist_ticked": n,
				"path":             f.Path,
				"resolved":         res.Path,
				"source":           "auto_extract",
			})
		}
	}
	var b strings.Builder
	if len(committed) > 0 {
		b.WriteString(fmt.Sprintf("AUTO-COMMITTED %d file(s) from coder reply:\n", len(committed)))
		for _, c := range committed {
			b.WriteString("  - ")
			b.WriteString(c)
			b.WriteString("\n")
		}
	}
	if len(failed) > 0 {
		b.WriteString(fmt.Sprintf("AUTO-COMMIT FAILED for %d file(s):\n", len(failed)))
		for _, f := range failed {
			b.WriteString("  - ")
			b.WriteString(f)
			b.WriteString("\n")
		}
	}
	if rejectedByCap > 0 {
		b.WriteString(fmt.Sprintf(
			"AUTO-COMMIT REJECTED %d file(s) above the %d-per-reply cap. "+
				"Re-prompt the coder with a narrower scope (one phase at a time).\n",
			rejectedByCap, maxFilesPerCoderReply))
	}
	return strings.TrimRight(b.String(), "\n")
}

// shortenArgs returns a trimmed copy of args suitable for trace logging.
func shortenArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		switch x := v.(type) {
		case string:
			out[k] = shorten(x, 240)
		default:
			out[k] = v
		}
	}
	return out
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
