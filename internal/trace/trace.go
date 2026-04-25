// Package trace writes JSONL traces to ~/.local/share/agent-router/traces/.
// One file per task. Every LLM call, tool invocation, phase change, and
// verdict is logged. Tail-able with `tail -f` and `jq`.
package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type Kind string

const (
	KindPhase    Kind = "phase"
	KindSwap     Kind = "swap"
	KindLLMCall  Kind = "llm_call"
	KindToolCall Kind = "tool_call"
	KindVerdict  Kind = "verdict"
	KindError    Kind = "error"
	KindInfo     Kind = "info"
	KindDone     Kind = "done"
)

type Event struct {
	TS     string          `json:"ts"`
	TaskID string          `json:"task_id"`
	Kind   Kind            `json:"kind"`
	Data   map[string]any  `json:"data,omitempty"`
	Notes  string          `json:"notes,omitempty"`
	Raw    json.RawMessage `json:"raw,omitempty"`
}

type Writer struct {
	taskID string
	path   string
	mu     sync.Mutex
	f      *os.File
	closed atomic.Bool
}

// Dir returns the default trace directory.
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "agent-router", "traces")
}

// Open creates or appends to a trace file for the given task id.
func Open(taskID string) (*Writer, error) {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s.jsonl", taskID))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Writer{taskID: taskID, path: path, f: f}, nil
}

func (w *Writer) Path() string { return w.path }

// Emit writes a single event. Errors are swallowed after being printed to
// stderr because the tracer must not be able to fail the task. Emit is a
// no-op once Close has been called: two overlapping shutdown paths (the
// run goroutine's defer and a stale Abort call) would otherwise race the
// file descriptor and spam "file already closed" to stderr.
func (w *Writer) Emit(kind Kind, data map[string]any) {
	if w == nil {
		return
	}
	if w.closed.Load() {
		return
	}
	ev := Event{
		TS:     time.Now().UTC().Format(time.RFC3339Nano),
		TaskID: w.taskID,
		Kind:   kind,
		Data:   data,
	}
	line, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trace: marshal: %v\n", err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// Re-check under the lock: a Close() racing with Emit() could have
	// just flipped closed and freed the file descriptor. Without the
	// recheck a surviving writer could still land bytes on a closed fd.
	if w.closed.Load() || w.f == nil {
		return
	}
	if _, err := w.f.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "trace: write: %v\n", err)
	}
}

// Close idempotently finalises the trace file. Safe to call from multiple
// goroutines; subsequent calls are no-ops. Any Emit() still in flight
// finishes under the mutex before the fd is released.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// ReadAll returns every event for the given task id, in order.
//
// Prefer ReadTail for any polling path (e.g. the /tasks/<id> inspect
// handler): ReadAll is O(file size) and reads the entire JSONL even when
// the caller only wants the trailing events. ReadAll is still the right
// call for postmortems and full-task exports.
func ReadAll(taskID string) ([]Event, error) {
	path := filepath.Join(Dir(), fmt.Sprintf("%s.jsonl", taskID))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Event
	var start int
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			if i > start {
				var ev Event
				if err := json.Unmarshal(data[start:i], &ev); err == nil {
					out = append(out, ev)
				}
			}
			start = i + 1
		}
	}
	return out, nil
}

// ReadTail returns up to the last n events for the given task id, in order.
// Seeks from EOF in fixed-size chunks and parses only enough trailing lines
// to satisfy n, so poll-heavy callers (Inspect(), live UIs) avoid quadratic
// I/O as the trace file grows. Callers that want the full history should
// use ReadAll.
func ReadTail(taskID string, n int) ([]Event, error) {
	if n <= 0 {
		return []Event{}, nil
	}
	path := filepath.Join(Dir(), fmt.Sprintf("%s.jsonl", taskID))
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return []Event{}, nil
	}

	// Read backwards in chunks until we have at least n+1 newline boundaries
	// or we've consumed the whole file. We need n+1 because the final line
	// may not end in a newline, and the leading partial line of a chunk
	// isn't trustworthy without the preceding byte.
	const chunkSize int64 = 8192
	var tail []byte
	offset := size
	newlines := 0
	want := n + 1

	for offset > 0 && newlines < want {
		readSize := chunkSize
		if offset < readSize {
			readSize = offset
		}
		offset -= readSize
		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
			return nil, err
		}
		tail = append(buf, tail...)
		newlines = bytes.Count(tail, []byte("\n"))
	}

	// If we didn't consume the entire file, drop the leading (possibly
	// partial) line so we only parse complete records.
	if offset > 0 {
		if idx := bytes.IndexByte(tail, '\n'); idx >= 0 {
			tail = tail[idx+1:]
		}
	}

	lines := bytes.Split(tail, []byte("\n"))
	// Last element after Split on a trailing "\n" is empty; drop it.
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	out := make([]Event, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err == nil {
			out = append(out, ev)
		}
	}
	return out, nil
}
