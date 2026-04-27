package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aegis/moirai/internal/modelmgr"
	"github.com/aegis/moirai/internal/taskstore"
	"github.com/aegis/moirai/internal/trace"
)

func newTestOrch(t *testing.T) *Orchestrator {
	t.Helper()
	// Isolate trace dir from user $HOME so tests don't pollute real traces.
	t.Setenv("HOME", t.TempDir())
	store, err := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatalf("taskstore: %v", err)
	}
	o, err := New(Config{
		Store:    store,
		ModelMgr: &stubModelMgr{},
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	return o
}

// newRunningTask seeds the orchestrator's running map with a fake runState
// so Inject/Interrupt can find the task id. Returns the id.
func newRunningTask(t *testing.T, o *Orchestrator) string {
	t.Helper()
	id := "test-" + newTaskID()
	task := &taskstore.Task{
		ID:     id,
		Status: taskstore.StatusRunning,
	}
	if err := o.cfg.Store.Save(task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	tr, err := trace.Open(id)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	st := &runState{
		cancel: func() {},
		task:   task,
		trace:  tr,
	}
	o.mu.Lock()
	o.running[id] = st
	o.mu.Unlock()
	t.Cleanup(func() { _ = tr.Close() })
	return id
}

func TestInjectEmptyMessage(t *testing.T) {
	o := newTestOrch(t)
	id := newRunningTask(t, o)
	if err := o.Inject(id, ""); err == nil {
		t.Errorf("expected error for empty message")
	}
	if err := o.Inject(id, "   \n  \t"); err == nil {
		t.Errorf("expected error for whitespace-only message")
	}
}

func TestInjectNotRunning(t *testing.T) {
	o := newTestOrch(t)
	if err := o.Inject("nonexistent-id", "hi"); err == nil {
		t.Errorf("expected error for unknown task id")
	}
}

func TestInjectQueues(t *testing.T) {
	o := newTestOrch(t)
	id := newRunningTask(t, o)
	if err := o.Inject(id, "first message"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := o.Inject(id, "second message"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	o.mu.Lock()
	st := o.running[id]
	o.mu.Unlock()
	st.injectMu.Lock()
	defer st.injectMu.Unlock()
	if len(st.pendingInject) != 2 {
		t.Fatalf("expected 2 pending injects, got %d", len(st.pendingInject))
	}
	if st.pendingInject[0] != "first message" || st.pendingInject[1] != "second message" {
		t.Errorf("unexpected queue contents: %v", st.pendingInject)
	}
	// An inject trace event should have been emitted for each call. Close
	// the writer and count matching records.
	_ = st.trace.Close()
	events, _ := trace.ReadAll(id)
	injectEvents := 0
	for _, ev := range events {
		if _, ok := ev.Data["inject"]; ok {
			injectEvents++
		}
	}
	if injectEvents != 2 {
		t.Errorf("expected 2 inject trace events, got %d", injectEvents)
	}
}

func TestInterruptDelegatesToInject(t *testing.T) {
	o := newTestOrch(t)
	id := newRunningTask(t, o)
	if err := o.Interrupt(id); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	o.mu.Lock()
	st := o.running[id]
	o.mu.Unlock()
	st.injectMu.Lock()
	defer st.injectMu.Unlock()
	if len(st.pendingInject) != 1 {
		t.Fatalf("expected 1 pending inject from Interrupt, got %d", len(st.pendingInject))
	}
	msg := st.pendingInject[0]
	if !strings.Contains(msg, "USER INTERRUPT") {
		t.Errorf("expected USER INTERRUPT sentinel, got %q", msg)
	}
	// Interrupt on unknown task should still error via Inject.
	if err := o.Interrupt("does-not-exist"); err == nil {
		t.Errorf("expected Interrupt to fail on unknown task")
	}
}

// stubModelMgr implements orchestrator.ModelManager with a scriptable
// completion response and message-capture.
type stubModelMgr struct {
	responses []string
	mu        sync.Mutex
	calls     int
	active    modelmgr.Slot
	lastReq   modelmgr.ChatRequest
	messages  [][]modelmgr.ChatMessage // snapshot of req.Messages per call
}

func (s *stubModelMgr) EnsureSlot(ctx context.Context, slot modelmgr.Slot) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = slot
	return "http://stub", nil
}

func (s *stubModelMgr) Active() modelmgr.Slot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *stubModelMgr) Complete(ctx context.Context, req modelmgr.ChatRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Copy messages so subsequent caller appends don't mutate our snapshot.
	msgs := make([]modelmgr.ChatMessage, len(req.Messages))
	copy(msgs, req.Messages)
	s.messages = append(s.messages, msgs)
	s.lastReq = req
	idx := s.calls
	s.calls++
	if idx < len(s.responses) {
		return s.responses[idx], nil
	}
	// Empty-responses guard: under -race with TestSubmitPathExpansion the
	// Submit-spawned run goroutine races into Complete with no preseeded
	// responses, which hit `responses[-1]` and panicked. Return a neutral
	// "done" tool call so the run goroutine exits cleanly rather than
	// crashing the whole test binary.
	if len(s.responses) == 0 {
		return `<TOOL>{"name":"done","args":{"summary":"stub-default"}}</TOOL>`, nil
	}
	return s.responses[len(s.responses)-1], nil
}

// TestROLoopDrainsInject seeds a pendingInject entry, runs a single RO turn
// against a stub ModelMgr that responds with <TOOL>{"name":"done"...}</TOOL>,
// and asserts the injected message appeared as a "[USER INJECT] ..."
// user-role message on the wire, pendingInject was cleared, and the
// inject_drained trace event fired.
func TestROLoopDrainsInject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	stub := &stubModelMgr{
		responses: []string{
			`Thinking... <TOOL>{"name":"done","args":{"summary":"ok"}}</TOOL>`,
		},
	}
	o, err := New(Config{
		Store:    store,
		ModelMgr: stub,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	id := "drain-" + newTaskID()
	task := &taskstore.Task{
		ID:       id,
		Status:   taskstore.StatusRunning,
		RepoRoot: t.TempDir(),
	}
	_ = store.Save(task)
	tr, err := trace.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	st := &runState{
		cancel: func() {},
		task:   task,
		trace:  tr,
		pendingInject: []string{
			"please switch to exploring the config loader",
		},
		// Pre-seed workOps so the done() guard (STATE-FUZZ-002) doesn't
		// reject the test's terminal call. This test exercises inject
		// drain, not the work-before-done invariant.
		workOps: 1,
	}

	// Skip EnsureRepo / toolbox: call roLoop directly with a nil toolbox
	// since the "done" path doesn't exercise tool execution.
	summary, ok, err := o.roLoop(context.Background(), st, nil)
	if err != nil {
		t.Fatalf("roLoop: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true (done tool), got summary=%q", summary)
	}

	// Assert the stub saw a "[USER INJECT] ..." user-role message.
	if len(stub.messages) == 0 {
		t.Fatal("stub saw no Complete calls")
	}
	firstCall := stub.messages[0]
	var sawInject bool
	for _, m := range firstCall {
		if m.Role == "user" && strings.HasPrefix(m.Content, "[USER INJECT] ") {
			sawInject = true
			if !strings.Contains(m.Content, "switch to exploring the config loader") {
				t.Errorf("inject content wrong: %q", m.Content)
			}
		}
	}
	if !sawInject {
		t.Errorf("[USER INJECT] message not found in Complete request; messages=%+v", firstCall)
	}

	// Assert pendingInject was cleared.
	if len(st.pendingInject) != 0 {
		t.Errorf("pendingInject not cleared: %v", st.pendingInject)
	}

	// Assert inject_drained trace event was emitted.
	_ = tr.Close()
	events, _ := trace.ReadAll(id)
	var sawDrain bool
	for _, ev := range events {
		if _, ok := ev.Data["inject_drained"]; ok {
			sawDrain = true
			break
		}
	}
	if !sawDrain {
		t.Errorf("inject_drained event not fired; events=%+v", events)
	}
}

// TestSubmitRejectsEmptyDescription verifies that the Go API enforces the
// "description required" rule independently of the HTTP layer. The HTTP
// handler already rejects, but a direct caller into the orchestrator
// package must not be able to backdoor an empty description.
func TestSubmitRejectsEmptyDescription(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	o, err := New(Config{Store: store, DefaultRepo: home, ModelMgr: &stubModelMgr{}})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	cases := []string{"", "   ", "\t\n", " \t \n "}
	for _, in := range cases {
		_, err := o.Submit(context.Background(), in, home)
		if err == nil {
			t.Errorf("expected error for empty/whitespace description %q", in)
		} else if !strings.Contains(err.Error(), "description required") {
			t.Errorf("expected 'description required' in error, got: %v", err)
		}
	}

	// Sanity: a real description still succeeds.
	if _, err := o.Submit(context.Background(), "real work", home); err != nil {
		t.Errorf("non-empty description rejected: %v", err)
	}
}

// TestSubmitPathExpansion confirms Submit resolves ~, $HOME, and relative
// paths to absolute ones before storing, and that Stat passes on them.
func TestSubmitPathExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Create the target dirs so Stat inside Submit succeeds.
	if err := os.MkdirAll(filepath.Join(home, "Projects", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	relDir := filepath.Join(home, "relative-test")
	if err := os.MkdirAll(relDir, 0o755); err != nil {
		t.Fatal(err)
	}

	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	o, err := New(Config{Store: store, DefaultRepo: home, ModelMgr: &stubModelMgr{}})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"tilde", "~/Projects/x", filepath.Join(home, "Projects", "x")},
		{"absolute", filepath.Join(home, "Projects", "x"), filepath.Join(home, "Projects", "x")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task, err := o.Submit(context.Background(), "desc", c.in)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if task.RepoRoot != c.want {
				t.Errorf("RepoRoot = %q, want %q", task.RepoRoot, c.want)
			}
			if !filepath.IsAbs(task.RepoRoot) {
				t.Errorf("RepoRoot not absolute: %q", task.RepoRoot)
			}
			if _, err := os.Stat(task.RepoRoot); err != nil {
				t.Errorf("RepoRoot does not stat: %v", err)
			}
			// Abort the spawned run goroutine so it doesn't race tmpdir
			// cleanup or panic into an empty stub response slice.
			_ = o.Abort(task.ID)
		})
	}

	// Relative path: ensure it resolves to an absolute path rooted at cwd.
	if err := os.Chdir(relDir); err != nil {
		t.Skipf("cannot chdir for relative test: %v", err)
	}
	defer os.Chdir(cwd)
	task, err := o.Submit(context.Background(), "desc", "./")
	if err != nil {
		t.Fatalf("Submit relative: %v", err)
	}
	if !filepath.IsAbs(task.RepoRoot) {
		t.Errorf("relative RepoRoot not absolute: %q", task.RepoRoot)
	}
	_ = o.Abort(task.ID)

	// Give spawned run goroutines a beat to exit via the cancelled ctx
	// so TempDir cleanup doesn't race against mid-flight EnsureRepo
	// writes. The goroutines all observe ctx early (EnsureSlot + stubs
	// short-circuit) so 100ms is ample.
	time.Sleep(100 * time.Millisecond)
}

// TestAbortDuringRun starts a fake run goroutine that blocks on a stub
// ModelMgr, calls Abort, and asserts the task's final Status is
// StatusAborted. No data race triggers under -race because Abort no
// longer writes task.Status; the run goroutine does via fail().
func TestAbortDuringRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	store, _ := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))

	// Stub Complete blocks on the context; when Abort cancels it, it returns
	// ctx.Err so the run's roLoop returns an error, flowing through fail()
	// which observes abortRequested and marks StatusAborted.
	stub := &blockingModelMgr{
		blocked: make(chan struct{}),
	}
	o, err := New(Config{
		Store:    store,
		ModelMgr: stub,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	// Seed a task in a fresh temp dir so EnsureRepo succeeds.
	repoDir := t.TempDir()
	id := newTaskID()
	task := &taskstore.Task{
		ID:       id,
		Status:   taskstore.StatusRunning,
		RepoRoot: repoDir,
		Branch:   fmt.Sprintf("moirai/task-%s", id),
	}
	_ = store.Save(task)
	tr, err := trace.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	st := &runState{
		cancel: cancel,
		task:   task,
		trace:  tr,
	}
	o.mu.Lock()
	o.running[id] = st
	o.mu.Unlock()

	done := make(chan struct{})
	go func() {
		o.run(ctx, st)
		close(done)
	}()

	// Wait for run to reach the blocking Complete call.
	<-stub.blocked
	// Abort: flip flag, cancel ctx.
	if err := o.Abort(id); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	// run should unwind and exit.
	select {
	case <-done:
	case <-ctxWait():
		t.Fatal("run goroutine did not exit after Abort")
	}

	// Task status must be StatusAborted; LastError may be the ctx err.
	loaded, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Status != taskstore.StatusAborted {
		t.Errorf("expected StatusAborted, got %q (LastError=%q)", loaded.Status, loaded.LastError)
	}
	if !st.abortRequested.Load() {
		t.Errorf("abortRequested flag not set")
	}
}

// blockingModelMgr is a ModelManager stub whose Complete call blocks on
// ctx.Done, signalling 'blocked' once to let tests know we're inside.
type blockingModelMgr struct {
	blocked    chan struct{}
	signalOnce atomic.Bool
}

func (b *blockingModelMgr) EnsureSlot(ctx context.Context, slot modelmgr.Slot) (string, error) {
	return "http://stub", nil
}

func (b *blockingModelMgr) Active() modelmgr.Slot { return modelmgr.SlotReviewer }

func (b *blockingModelMgr) Complete(ctx context.Context, req modelmgr.ChatRequest) (string, error) {
	if b.signalOnce.CompareAndSwap(false, true) {
		close(b.blocked)
	}
	<-ctx.Done()
	return "", ctx.Err()
}

// ctxWait returns a channel that fires after a few seconds, used to bound
// the Abort test.
func ctxWait() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5e9) // 5s
		defer cancel()
		<-ctx.Done()
		close(ch)
	}()
	return ch
}
