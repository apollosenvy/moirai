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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aegis/agent-router/internal/aegis"
	"github.com/aegis/agent-router/internal/modelmgr"
	"github.com/aegis/agent-router/internal/repoconfig"
	"github.com/aegis/agent-router/internal/taskstore"
	"github.com/aegis/agent-router/internal/toolbox"
	"github.com/aegis/agent-router/internal/trace"
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
)

// --- shared budgets --------------------------------------------------------

const (
	// MaxTokensAll is the max_tokens sent on every ChatRequest. Consistent
	// 24576 across P, C, RO per the brief.
	MaxTokensAll = 24576

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
}

// LastVerdict returns the most recent verdict emitted by the Reviewer,
// or "" if none yet.
func (o *Orchestrator) LastVerdict() string {
	o.vmu.Lock()
	defer o.vmu.Unlock()
	return o.lastVerdict
}

func (o *Orchestrator) setLastVerdict(v string) {
	o.vmu.Lock()
	defer o.vmu.Unlock()
	o.lastVerdict = v
	o.lastVerdictAt = time.Now()
}

type runState struct {
	cancel context.CancelFunc
	task   *taskstore.Task
	trace  *trace.Writer

	// abortRequested is flipped to true by Abort() before it cancels the
	// run ctx. The run goroutine's defer checks this flag to decide
	// between StatusAborted (operator-driven) and StatusFailed (ctx
	// cancelled for another reason, e.g. budget timeout). Writes to
	// task.Status live exclusively on the run goroutine's path, which
	// eliminates the race Abort() used to cause.
	abortRequested atomic.Bool

	// Tool budget counters.
	askCoderCalls    int
	askPlannerCalls  int
	roTurns          int

	// Last emitted plan (for context when RO calls ask_coder without
	// passing one, and for L2 persistence).
	lastPlan string
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
	return &Orchestrator{
		cfg:     cfg,
		running: make(map[string]*runState),
	}, nil
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
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("%w: repo root: %v", ErrInvalidInput, err)
	}

	id := newTaskID()
	t := &taskstore.Task{
		ID:          id,
		Description: description,
		RepoRoot:    abs,
		Branch:      fmt.Sprintf("agent-router/task-%s", id),
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
	_ = o.cfg.Store.Save(t)

	runCtx, cancel := context.WithCancel(context.Background())
	st := &runState{cancel: cancel, task: t, trace: tr}

	o.mu.Lock()
	o.running[id] = st
	o.mu.Unlock()

	// Snapshot BEFORE launching the run goroutine: the goroutine owns all
	// further writes to t, so taking the clone here is race-free. Returning
	// the live pointer would let an HTTP response encoder read Status at the
	// same instant the run goroutine wrote it.
	snapshot := t.Clone()

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
	o.mu.Unlock()
	if !ok {
		return fmt.Errorf("inject: task %s not running", taskID)
	}
	st.injectMu.Lock()
	st.pendingInject = append(st.pendingInject, message)
	depth := len(st.pendingInject)
	st.injectMu.Unlock()
	st.trace.Emit(trace.KindInfo, map[string]any{
		"inject":    shorten(message, 200),
		"queue_len": depth,
	})
	return nil
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
	_ = o.cfg.Store.Save(t)
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
	msg := fmt.Sprintf("agent-router: %s\n\nTask: %s\n", shorten(t.Description, 72), t.ID)
	if _, err := tb.GitCommit(ctx, msg); err != nil {
		tr.Emit(trace.KindInfo, map[string]any{"commit_skipped": err.Error()})
	} else {
		tr.Emit(trace.KindInfo, map[string]any{"commit": "created"})
	}

	t.Status = taskstore.StatusSucceeded
	t.Phase = taskstore.PhaseDone
	_ = o.cfg.Store.Save(t)
	tr.Emit(trace.KindDone, map[string]any{
		"branch":  t.Branch,
		"summary": shorten(summary, 2000),
		"turns":   st.roTurns,
	})

	if o.cfg.L2 != nil {
		o.setLastVerdict("succeeded")
		_ = o.cfg.L2.RecordVerdict(t.RepoRoot, t.ID, "final", "succeeded", shorten(summary, 4000))
	}
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

	messages := []modelmgr.ChatMessage{
		{Role: "system", Content: roSystemPrompt()},
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

		resp, err := o.cfg.ModelMgr.Complete(ctx, modelmgr.ChatRequest{
			Messages:    messages,
			Temperature: 0.2,
			MaxTokens:   MaxTokensAll,
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

		call, ok := extractToolCall(resp)

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
			// Compaction is skipped for tools that should never bloat
			// (done/fail are terminal; pensive.search is already a retrieval
			// so compacting it defeats the purpose).
			compactable := call.Name != "pensive.search" && call.Name != "pensive.emit_atom"
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
			payload = fmt.Sprintf("<ERROR>%s</ERROR>", toolErr.Error())
		} else {
			payload = fmt.Sprintf("<RESULT>%s</RESULT>", result)
		}
		messages = append(messages, modelmgr.ChatMessage{Role: "user", Content: payload})
	}
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
	argStr := func(k string) string {
		v, _ := tc.Args[k].(string)
		return v
	}
	argInt := func(k string, def int) int {
		switch v := tc.Args[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case string:
			var n int
			_, _ = fmt.Sscanf(v, "%d", &n)
			if n == 0 {
				return def
			}
			return n
		}
		return def
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
		plan, err := o.callPlanner(ctx, st, tb, instr)
		if err != nil {
			return "", err
		}
		st.lastPlan = plan
		st.task.Plan = plan
		_ = o.cfg.Store.Save(st.task)
		return plan, nil

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
		return code, nil

	case "fs.read":
		res, err := tb.FSRead(argStr("path"), 1<<17)
		if err != nil {
			return "", err
		}
		return res.Content, nil

	case "fs.write":
		res, err := tb.FSWrite(argStr("path"), argStr("content"))
		if err != nil {
			return "", err
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
		return fmt.Sprintf("OK wrote %d bytes to %s (%s)", res.Bytes, res.Path, what), nil

	case "fs.search":
		path := argStr("path_glob")
		if path == "" {
			path = argStr("path")
		}
		if path == "" {
			path = "."
		}
		hits, err := tb.FSSearch(ctx, argStr("pattern"), path, 50)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(hits)
		return string(b), nil

	case "test.run":
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
		}
		return out, nil

	case "compile.run":
		r, err := tb.CompileRun(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("exit=%d\nstdout:\n%s\nstderr:\n%s",
			r.ExitCode, shorten(r.Stdout, 4000), shorten(r.Stderr, 2000)), nil

	case "pensive.search":
		q := argStr("query")
		k := argInt("k", 5)
		project := argStr("project")
		if project == "" {
			project = filepath.Base(st.task.RepoRoot)
		}
		out, err := aegis.PensiveSearchRaw(ctx, project, q, k)
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
		resp, err := o.cfg.ModelMgr.Complete(ctx, modelmgr.ChatRequest{
			Messages:    messages,
			Temperature: 0.2,
			MaxTokens:   MaxTokensAll,
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
	user := fmt.Sprintf(
		"Task:\n%s\n\nRepo root: %s\n\nPlan:\n%s\n\nInstruction from the Reviewer-Orchestrator:\n%s\n\nProduce the code now. Emit each file as a markdown-fenced block. The first line INSIDE the fence must be a comment naming the path, like:\n```python\n# file: src/foo.py\n...code...\n```",
		st.task.Description, st.task.RepoRoot, shorten(plan, 20000), instruction,
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
		resp, err := o.cfg.ModelMgr.Complete(ctx, modelmgr.ChatRequest{
			Messages:    messages,
			Temperature: 0.1,
			MaxTokens:   MaxTokensAll,
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
		_ = o.cfg.Store.Save(t)
		tr.Emit(trace.KindInfo, map[string]any{"aborted_by_user": true, "reason": err.Error()})
		return
	}
	t.Status = taskstore.StatusFailed
	t.LastError = err.Error()
	_ = o.cfg.Store.Save(t)
	tr.Emit(trace.KindError, map[string]any{"fatal": err.Error()})
}

// --- system prompts --------------------------------------------------------

func plannerSystemPrompt() string {
	return `You are the Planner.
You receive a task description and produce a concise, numbered plan.
Rules:
  - 3 to 12 numbered steps, each an imperative sentence.
  - Identify files to touch and tests to run.
  - Be precise and unambiguous. The Reviewer-Orchestrator reads your plan
    and decides next steps; if a step is ambiguous it will ask you to
    clarify.
  - Your context is large; think thoroughly before writing.
  - When done, you MAY commit the plan by emitting exactly one tool call:
      <TOOL>{"name":"fs.write","args":{"path":"PLAN.md","content":"..."}}</TOOL>
    This is optional; the plan text in your reply itself is what matters.
  - You have NO other tools. Do not attempt fs.read, fs.search, test.run,
    shell.exec, or anything else. Only fs.write to PLAN.md is allowed.
`
}

func coderSystemPrompt(retryMode bool) string {
	base := `You are the Coder.
You receive a task description, a plan, and an instruction from the
Reviewer-Orchestrator. You produce the code files required by the plan.
Rules:
  - Emit each file as a markdown-fenced code block.
  - The FIRST LINE inside the fence must be a comment naming the file path:
      ` + "```python" + `
      # file: src/foo.py
      ...code...
      ` + "```" + `
  - The Reviewer-Orchestrator extracts each file and commits it to disk.
    You do NOT call fs.write, fs.read, fs.search, or any tool in normal
    mode. Just produce the code.
  - If a path comment is missing the file is discarded. Always include it.
  - Keep implementations tight; no dead code, no speculative abstractions.
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
`
	}
	return base
}

func roSystemPrompt() string {
	return `You are the Reviewer-Orchestrator.
You coordinate the Planner (P) and the Coder (C) to complete the user's task.

Your tools (emit exactly one per turn, wrapped in <TOOL>...</TOOL>):
  ask_planner      args: {"instruction": "..."}
  ask_coder        args: {"instruction": "...", "plan": "..."}
  fs.write         args: {"path": "...", "content": "..."}
  fs.read          args: {"path": "..."}
  fs.search        args: {"pattern": "...", "path_glob": "."}
  test.run         args: {}
  compile.run      args: {}
  pensive.search   args: {"query": "...", "k": 5}
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
     "# file: <path>" comment on the first line inside each fence.
     Extract each file and call fs.write to commit it.
  5. After writing, call test.run. If tests pass, call done(summary).
     If tests fail, inspect the output. Decide whether to:
        - Call ask_coder again (the coder gets fs.read access on retry)
        - Call ask_planner for a plan revision if the spec is wrong
  6. If nothing progresses, call fail(reason) with a clear explanation.

Rules:
  - Emit EXACTLY one <TOOL>...</TOOL> per turn. Think in prose before it.
  - After a tool call you will receive <RESULT>...</RESULT> or <ERROR>...</ERROR>.
  - When you see fenced code blocks in an ask_coder result, extract each
    file and write it with fs.write. Use the "# file: <path>" comment to
    determine the path; the fence language tag (` + "```python" + `,
    ` + "```go" + `) is informational only.
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
  - Memory accretion via pensive.emit_atom: after a decision where you
    noticed something non-obvious and transferable, emit an atom so a
    future version of you can retrieve the lesson. Emit sparingly and
    only for genuine signal. Routine decisions (approve, reject with
    standard reason) are not worth an atom. Good atoms name a specific
    pattern or pitfall. Bad atoms restate the obvious. Under-emit
    rather than over-emit; the corpus quality matters.
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

func extractToolCall(s string) (toolCall, bool) {
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
		return toolCall{Name: name, Args: args}, true
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
		return tc, true
	}
	// Last resort: scan for a top-level JSON object that has a "name" key.
	if idx := strings.Index(s, `"name"`); idx >= 0 {
		start := strings.LastIndexByte(s[:idx], '{')
		if start >= 0 {
			depth := 0
			for i := start; i < len(s); i++ {
				if s[i] == '{' {
					depth++
				} else if s[i] == '}' {
					depth--
					if depth == 0 {
						var tc toolCall
						if err := json.Unmarshal([]byte(s[start:i+1]), &tc); err == nil && tc.Name != "" {
							if tc.Args == nil {
								tc.Args = map[string]any{}
							}
							return tc, true
						}
						break
					}
				}
			}
		}
	}
	return toolCall{}, false
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
