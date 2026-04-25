package taskstore

// Regression tests for FIXER-4 audit fixes (pass 4):
//   REC-3: invalid Status on disk must be skipped (corruption count++).
//   REC-4: bogus timestamps must be clamped, not rejected.
//   C4-001: List() warm path must pick up new files via mtime tracking.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestListRejectsInvalidStatus(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Hand-write a task file with a bogus status.
	bad := `{
  "id": "bad-1",
  "description": "test",
  "status": "successful",
  "phase": "done"
}`
	if err := os.WriteFile(filepath.Join(dir, "bad-1.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 valid tasks, got %d (%+v)", len(tasks), tasks)
	}
	if s.CorruptCount() == 0 {
		t.Errorf("expected corruptCount > 0 after bogus status load")
	}
}

func TestListSanitizesBogusTimestamps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// CreatedAt is decades in the future; UpdatedAt is before CreatedAt.
	// Both should be clamped, but the task remains queryable.
	bad := `{
  "id": "ts-1",
  "description": "future task",
  "status": "running",
  "phase": "init",
  "created_at": "2999-01-01T00:00:00Z",
  "updated_at": "2025-01-01T00:00:00Z"
}`
	if err := os.WriteFile(filepath.Join(dir, "ts-1.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task (sanitized, not skipped), got %d", len(tasks))
	}
	t0 := tasks[0]
	now := time.Now().UTC()
	if t0.CreatedAt.After(now.Add(time.Hour)) {
		t.Errorf("CreatedAt not clamped: %v", t0.CreatedAt)
	}
	if t0.UpdatedAt.Before(t0.CreatedAt) {
		t.Errorf("UpdatedAt < CreatedAt after sanitize: created=%v updated=%v", t0.CreatedAt, t0.UpdatedAt)
	}
}

func TestListMTimeWarmRescan(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// First List(): empty.
	tasks, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected empty initial list, got %d", len(tasks))
	}

	// Drop a new task file in directly (simulating an out-of-band edit
	// or a parallel process). The mtime must trigger a rescan on the
	// next List() call.
	good := `{
  "id": "warm-1",
  "description": "out-of-band fix",
  "status": "succeeded",
  "phase": "done",
  "created_at": "2025-04-01T00:00:00Z",
  "updated_at": "2025-04-01T00:00:00Z"
}`
	// Sleep briefly to ensure the new file's mtime is strictly after
	// the empty-dir scan we just performed (mtime resolution can be
	// 1s on some filesystems).
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "warm-1.json"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	tasks, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "warm-1" {
		t.Errorf("expected warm-1 task on second List, got %+v", tasks)
	}
}
