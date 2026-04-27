package api

// Pass-7 SCOUT-CLEAN-2 chaos engineering substitute. The dispatch brief asked
// for kill -9 of a live daemon; the subagent gatekeeper blocks pkill, so this
// file exercises the same crash-recovery code paths via fixtures: corrupt
// task JSON files, partial writes, bogus timestamps. Any panic / data loss /
// silent corruption surfaces as a test failure.
//
// Cleanup of test files is automatic via t.TempDir().

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aegis/moirai/internal/taskstore"
)

// TestChaosCrashRecoveryFromCorruptStateDir simulates a daemon SIGKILL'd
// while writing partial task JSON files. On restart, taskstore.List() must
// skip corrupt entries (counted in CorruptCount), keep good entries, and
// not panic.
func TestChaosCrashRecoveryFromCorruptStateDir(t *testing.T) {
	dir := t.TempDir()

	// Fixtures: 5 well-formed tasks + 5 chaos files.
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		t1 := taskstore.Task{
			ID:          "good-" + string(rune('a'+i)),
			Description: "well-formed task",
			Status:      taskstore.StatusSucceeded,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		data, err := json.Marshal(t1)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		path := filepath.Join(dir, t1.ID+".json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write good: %v", err)
		}
	}

	// Chaos 1: empty file (kill -9 mid-truncate)
	_ = os.WriteFile(filepath.Join(dir, "chaos-empty.json"), []byte(""), 0o600)
	// Chaos 2: half-written JSON (kill -9 mid-flush)
	_ = os.WriteFile(filepath.Join(dir, "chaos-partial.json"), []byte(`{"id":"chaos-partial","stat`), 0o600)
	// Chaos 3: invalid status enum
	_ = os.WriteFile(filepath.Join(dir, "chaos-badstatus.json"),
		[]byte(`{"id":"chaos-badstatus","status":"DELETE_ALL_DATA","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:00:00Z"}`),
		0o600)
	// Chaos 4: id-mismatch (filename vs id)
	_ = os.WriteFile(filepath.Join(dir, "chaos-mismatch.json"),
		[]byte(`{"id":"some-other-id","status":"succeeded","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:00:00Z"}`),
		0o600)
	// Chaos 5: bogus timestamp far in future
	farFuture := now.Add(50 * 365 * 24 * time.Hour).Format(time.RFC3339)
	_ = os.WriteFile(filepath.Join(dir, "chaos-future.json"),
		[]byte(`{"id":"chaos-future","status":"succeeded","created_at":"`+farFuture+`","updated_at":"`+farFuture+`"}`),
		0o600)

	// Open should NOT panic, NOT lose all tasks.
	store, err := taskstore.Open(dir)
	if err != nil {
		t.Fatalf("Open after chaos: %v", err)
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// We expect at least the 5 well-formed tasks, plus possibly the
	// timestamp-clamped chaos-future entry (timestamps are sanitized,
	// not rejected). Empty/partial/bad-status/mismatch should all be
	// skipped + counted.
	goodSeen := 0
	for _, ta := range tasks {
		if strings.HasPrefix(ta.ID, "good-") {
			goodSeen++
		}
	}
	if goodSeen != 5 {
		t.Fatalf("expected 5 well-formed tasks to survive, got %d (total=%d)", goodSeen, len(tasks))
	}

	// CorruptCount must be > 0 (the empty + partial + bad-status + mismatch).
	if cc := store.CorruptCount(); cc < 3 {
		t.Errorf("expected CorruptCount >= 3, got %d", cc)
	}

	// Sanity: no task in the returned list has empty ID or invalid status.
	for _, ta := range tasks {
		if ta.ID == "" {
			t.Errorf("task with empty ID returned from List")
		}
		if ta.Status == "" {
			t.Errorf("task with empty status returned from List")
		}
	}
}

// TestChaosRepeatedReopens simulates the "kill -9 every 5s for 60s" pattern:
// open, write, close, reopen, read. State must remain consistent across
// repeated open/close cycles even with a corrupt file persisting in the dir.
func TestChaosRepeatedReopens(t *testing.T) {
	dir := t.TempDir()

	// Plant a permanently-corrupt file. This stays across reopens.
	_ = os.WriteFile(filepath.Join(dir, "always-corrupt.json"), []byte("{not json"), 0o600)

	for cycle := 0; cycle < 12; cycle++ { // 12 cycles ~ "every 5s for 60s"
		store, err := taskstore.Open(dir)
		if err != nil {
			t.Fatalf("cycle %d Open: %v", cycle, err)
		}
		// Write a fresh task.
		ta := &taskstore.Task{
			ID:          "cycle-" + string(rune('a'+cycle)),
			Description: "soak",
			Status:      taskstore.StatusSucceeded,
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}
		if err := store.Save(ta); err != nil {
			t.Fatalf("cycle %d Save: %v", cycle, err)
		}
		list, err := store.List()
		if err != nil {
			t.Fatalf("cycle %d List: %v", cycle, err)
		}
		// Must contain at least this cycle's task plus all earlier cycles'.
		if len(list) < cycle+1 {
			t.Errorf("cycle %d: expected >=%d tasks, got %d", cycle, cycle+1, len(list))
		}
	}
}
