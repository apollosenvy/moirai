package orchestrator

import (
	"github.com/aegis/agent-router/internal/taskstore"
	"github.com/aegis/agent-router/internal/trace"
)

// SeedRunningForTest registers a fake running task on the orchestrator and
// persists a matching record in the provided store. Returns the generated
// task id. For test use only -- do not call from production code; it skips
// the run goroutine and toolbox init. Kept in a non-_test.go file so it can
// be reused from other packages' test binaries (the api package needs it).
func SeedRunningForTest(o *Orchestrator, store *taskstore.Store) (string, error) {
	id := "test-" + newTaskID()
	task := &taskstore.Task{
		ID:     id,
		Status: taskstore.StatusRunning,
	}
	if err := store.Save(task); err != nil {
		return "", err
	}
	tr, err := trace.Open(id)
	if err != nil {
		return "", err
	}
	st := &runState{
		cancel: func() {},
		task:   task,
		trace:  tr,
	}
	o.mu.Lock()
	o.running[id] = st
	o.mu.Unlock()
	return id, nil
}
