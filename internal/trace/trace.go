// Package trace writes JSONL traces to ~/.local/share/agent-router/traces/.
// One file per task. Every LLM call, tool invocation, phase change, and
// verdict is logged. Tail-able with `tail -f` and `jq`.
package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
// stderr because the tracer must not be able to fail the task.
func (w *Writer) Emit(kind Kind, data map[string]any) {
	if w == nil {
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
	if _, err := w.f.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "trace: write: %v\n", err)
	}
}

func (w *Writer) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	return w.f.Close()
}

// ReadAll returns every event for the given task id, in order.
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
