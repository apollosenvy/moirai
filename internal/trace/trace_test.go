package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func withTempTraceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// Dir() resolves $HOME each call; no further wiring needed.
	// TODO(rename): mirrors trace.Dir() filesystem path; migrate together.
	return filepath.Join(dir, ".local", "share", "agent-router", "traces")
}

// TestTraceReadTail crafts a multi-line jsonl and asserts ReadTail returns
// exactly the last N events in order.
func TestTraceReadTail(t *testing.T) {
	withTempTraceDir(t)
	taskID := "tail-task"
	w, err := Open(taskID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 200; i++ {
		w.Emit(KindInfo, map[string]any{"i": i})
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events, err := ReadTail(taskID, 20)
	if err != nil {
		t.Fatalf("ReadTail: %v", err)
	}
	if len(events) != 20 {
		t.Fatalf("expected 20 events, got %d", len(events))
	}
	for idx, ev := range events {
		want := 180 + idx
		got, ok := ev.Data["i"].(float64)
		if !ok {
			t.Fatalf("event %d: expected numeric i, got %T %v", idx, ev.Data["i"], ev.Data["i"])
		}
		if int(got) != want {
			t.Fatalf("event %d: expected i=%d, got %d", idx, want, int(got))
		}
	}
}

// TestTraceReadTailSmallFile handles the case where the file has fewer
// lines than n.
func TestTraceReadTailSmallFile(t *testing.T) {
	withTempTraceDir(t)
	taskID := "small-task"
	w, err := Open(taskID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 3; i++ {
		w.Emit(KindInfo, map[string]any{"i": i})
	}
	_ = w.Close()

	events, err := ReadTail(taskID, 20)
	if err != nil {
		t.Fatalf("ReadTail: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
}

// TestTraceEmitAfterClose asserts post-Close Emit calls are silent no-ops,
// not panics or "file already closed" stderr spam.
func TestTraceEmitAfterClose(t *testing.T) {
	withTempTraceDir(t)
	w, err := Open("closed-task")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Redirect stderr so we can detect noisy behavior.
	r, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = wPipe
	defer func() { os.Stderr = oldStderr }()

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double-close is idempotent.
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Emit after close must not panic.
	w.Emit(KindInfo, map[string]any{"after": "close"})
	w.Emit(KindError, map[string]any{"still": "silent"})

	wPipe.Close()
	os.Stderr = oldStderr

	var sb strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	if strings.Contains(sb.String(), "file already closed") {
		t.Fatalf("Emit after Close leaked stderr spam: %q", sb.String())
	}
}

// TestTraceConcurrentCloseAndEmit stresses the Close/Emit race to make
// sure the sync.Once + mu combination survives.
func TestTraceConcurrentCloseAndEmit(t *testing.T) {
	withTempTraceDir(t)
	w, err := Open("race-task")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			w.Emit(KindInfo, map[string]any{"i": i})
		}
	}()
	go func() {
		defer wg.Done()
		_ = w.Close()
	}()
	wg.Wait()
	// If we got here without panic/leak the race is handled.
}

// TestTraceReadAllRoundtrip ensures ReadAll still returns the full history.
func TestTraceReadAllRoundtrip(t *testing.T) {
	withTempTraceDir(t)
	taskID := "roundtrip"
	w, err := Open(taskID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 5; i++ {
		w.Emit(KindInfo, map[string]any{"i": i, "msg": fmt.Sprintf("event-%d", i)})
	}
	_ = w.Close()

	events, err := ReadAll(taskID)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}
	// Confirm the LLM_CALL wire invariant is preserved: data.head stays
	// nested under Data, not lifted to a top-level field.
	w2, _ := Open("wire-shape")
	w2.Emit(KindLLMCall, map[string]any{"head": "HELLO"})
	_ = w2.Close()
	allEvents, err := ReadAll("wire-shape")
	if err != nil {
		t.Fatalf("ReadAll wire-shape: %v", err)
	}
	if len(allEvents) != 1 {
		t.Fatalf("expected 1 event for wire-shape, got %d", len(allEvents))
	}
	if allEvents[0].Data["head"] != "HELLO" {
		t.Fatalf("expected head nested in data, got %+v", allEvents[0])
	}
	// Also double-check the raw JSON has "data":{"head":...}, not a
	// top-level "head".
	path := filepath.Join(Dir(), "wire-shape.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(raw))
	if !strings.Contains(line, `"data":{`) || !strings.Contains(line, `"head":"HELLO"`) {
		t.Fatalf("wire-shape trace line does not contain nested data.head: %s", line)
	}
	if strings.Contains(line, `"head":"HELLO","kind"`) {
		t.Fatalf("wire-shape trace line has head at top level: %s", line)
	}
	// Sanity: unmarshal it the same way downstream tooling does.
	var ev Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Data["head"] != "HELLO" {
		t.Fatalf("unmarshalled: head missing from data: %+v", ev)
	}
}
