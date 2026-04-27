package taskstore

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tasks")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	s := newStore(t)
	task := &Task{
		ID:          "T-1",
		Description: "do the thing",
		RepoRoot:    "/tmp/repo",
		Branch:      "moirai/task-T-1",
		Status:      StatusRunning,
		Phase:       PhaseInit,
	}
	if err := s.Save(task); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load("T-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ID != "T-1" || got.Description != "do the thing" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("expected UpdatedAt to be set by Save")
	}
}

func TestListReturnsAllAndSortsByUpdatedAt(t *testing.T) {
	s := newStore(t)
	for i, id := range []string{"A", "B", "C"} {
		task := &Task{ID: id, Description: id, Status: StatusRunning}
		if err := s.Save(task); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
		// Tiny gap so UpdatedAt sorts deterministically.
		_ = i
		time.Sleep(2 * time.Millisecond)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(list))
	}
	// Most recently saved (C) should sort first.
	if list[0].ID != "C" {
		t.Errorf("expected newest task first, got %s", list[0].ID)
	}
}

func TestListUsesCacheAfterFirstCall(t *testing.T) {
	s := newStore(t)
	task := &Task{ID: "X", Description: "x", Status: StatusRunning}
	if err := s.Save(task); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// First List populates cache; subsequent Saves should keep it consistent.
	if _, err := s.List(); err != nil {
		t.Fatalf("List: %v", err)
	}
	task.Description = "x updated"
	if err := s.Save(task); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List 2: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 task, got %d", len(list))
	}
	if list[0].Description != "x updated" {
		t.Errorf("expected updated description in cache, got %q", list[0].Description)
	}
}

func TestPersistenceAcrossStoreReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Save(&Task{ID: "P", Status: StatusSucceeded}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reopen.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open2: %v", err)
	}
	got, err := s2.Load("P")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != StatusSucceeded {
		t.Errorf("expected Status=succeeded, got %q", got.Status)
	}
}

func TestConcurrentWritesAreSerialized(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			task := &Task{ID: "C", Description: "c", Status: StatusRunning}
			_ = s.Save(task)
			_, _ = s.List()
		}(i)
	}
	wg.Wait()
	// If we made it here without panic / race detector firing, the mutex
	// is doing its job.
	got, err := s.Load("C")
	if err != nil {
		t.Fatalf("Load after concurrent: %v", err)
	}
	if got.ID != "C" {
		t.Errorf("unexpected id %q", got.ID)
	}
}

func TestMarkInterruptedFlipsRunning(t *testing.T) {
	s := newStore(t)
	if err := s.Save(&Task{ID: "R", Status: StatusRunning}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Save(&Task{ID: "D", Status: StatusSucceeded}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.MarkInterrupted(); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}
	r, _ := s.Load("R")
	d, _ := s.Load("D")
	if r.Status != StatusInterrupted {
		t.Errorf("R: expected interrupted, got %q", r.Status)
	}
	if d.Status != StatusSucceeded {
		t.Errorf("D: expected succeeded (unchanged), got %q", d.Status)
	}
}
