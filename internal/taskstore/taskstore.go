// Package taskstore persists task state to disk so the daemon can restart
// mid-task and resume at the last phase boundary.
package taskstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusRunning    Status = "running"
	StatusAwaitUser  Status = "awaiting_user"
	StatusSucceeded  Status = "succeeded"
	StatusFailed     Status = "failed"
	StatusAborted    Status = "aborted"
	StatusInterrupted Status = "interrupted"
)

type Phase string

const (
	PhaseInit       Phase = "init"
	PhasePlan       Phase = "planning"
	PhasePlanReview Phase = "plan_review"
	PhaseCode       Phase = "coding"
	PhaseCodeReview Phase = "code_review"
	PhaseRevise     Phase = "revise"
	PhaseDone       Phase = "done"
)

type Task struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	RepoRoot    string            `json:"repo_root"`
	Branch      string            `json:"branch"`
	Status      Status            `json:"status"`
	Phase       Phase             `json:"phase"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	Iterations  int               `json:"iterations"`
	Replans     int               `json:"replans"`
	ActiveModel string            `json:"active_model"`
	LastError   string            `json:"last_error,omitempty"`
	Plan        string            `json:"plan,omitempty"`
	Reviews     []string          `json:"reviews,omitempty"`
	TracePath   string            `json:"trace_path,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
}

type Store struct {
	dir string
	mu  sync.Mutex
}

func DefaultDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "agent-router", "tasks")
}

func Open(dir string) (*Store, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func (s *Store) Save(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(t.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(t.ID))
}

func (s *Store) Load(id string) (*Task, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) List() ([]*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []*Task
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".json")]
		t, err := s.Load(id)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// MarkInterrupted is called on daemon startup for any task still in Running.
// Caller decides whether to resume it.
func (s *Store) MarkInterrupted() error {
	tasks, err := s.List()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.Status == StatusRunning {
			t.Status = StatusInterrupted
			t.LastError = fmt.Sprintf("daemon restarted at %s", time.Now().UTC().Format(time.RFC3339))
			if err := s.Save(t); err != nil {
				return err
			}
		}
	}
	return nil
}
