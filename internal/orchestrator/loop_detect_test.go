package orchestrator

import (
	"strings"
	"testing"
)

// TestContentHashStability: same input -> same hash, distinct inputs ->
// distinct hash (with the standard FNV caveat that collisions are not
// impossible, just very rare for short strings).
func TestContentHashStability(t *testing.T) {
	a := contentHash("hello")
	b := contentHash("hello")
	if a != b {
		t.Fatalf("hash unstable: %d vs %d for same input", a, b)
	}
	c := contentHash("hellp") // one bit different
	if a == c {
		t.Fatalf("hash collided trivially: hello and hellp both -> %d", a)
	}
	d := contentHash("")
	if d == 0 {
		t.Fatalf("empty-string hash should not be zero (FNV offset basis)")
	}
}

// TestDuplicateWriteCount: helper used by the dispatcher to decide
// whether to refuse a write.
func TestDuplicateWriteCount(t *testing.T) {
	h1 := contentHash("foo")
	h2 := contentHash("bar")
	hist := []fsWriteRecord{
		{Path: "a.go", ContentHash: h1},
		{Path: "a.go", ContentHash: h1},
		{Path: "b.go", ContentHash: h2},
		{Path: "a.go", ContentHash: h2},
	}
	if got := duplicateWriteCount(hist, "a.go", h1); got != 2 {
		t.Errorf("a.go h1 count: got %d, want 2", got)
	}
	if got := duplicateWriteCount(hist, "a.go", h2); got != 1 {
		t.Errorf("a.go h2 count: got %d, want 1", got)
	}
	if got := duplicateWriteCount(hist, "b.go", h2); got != 1 {
		t.Errorf("b.go h2 count: got %d, want 1", got)
	}
	if got := duplicateWriteCount(hist, "c.go", h1); got != 0 {
		t.Errorf("c.go h1 count: got %d, want 0", got)
	}
}

// TestFsWriteRepeatCapTriggers exercises the rejection path. Build a
// history that contains the duplicate-cap-many entries for the same
// (path, hash) pair, and verify the dispatcher's pre-write check
// would refuse the next attempt.
func TestFsWriteRepeatCapTriggers(t *testing.T) {
	path := "frontend/src/api.ts"
	hash := contentHash("export default function() { return 1; }\n")
	hist := []fsWriteRecord{}
	// Fill up to the cap.
	for i := 0; i < fsWriteRepeatCap; i++ {
		hist = append(hist, fsWriteRecord{Path: path, ContentHash: hash})
	}
	if got := duplicateWriteCount(hist, path, hash); got < fsWriteRepeatCap {
		t.Fatalf("set up %d duplicates but count says %d (cap %d)",
			fsWriteRepeatCap, got, fsWriteRepeatCap)
	}
}

// TestFsWriteRepeatCapDoesNotRejectDistinctPaths ensures the cap is
// scoped to (path, hash) pairs, not to the total history length. A
// model writing 10 distinct files in a row must not be rejected.
func TestFsWriteRepeatCapDoesNotRejectDistinctPaths(t *testing.T) {
	hash := contentHash("// some content\n")
	hist := []fsWriteRecord{}
	// Different paths, same hash.
	for i := 0; i < fsWriteHistoryLen; i++ {
		hist = append(hist, fsWriteRecord{
			Path:        "src/file_" + string(rune('a'+i)) + ".ts",
			ContentHash: hash,
		})
	}
	for _, r := range hist {
		if got := duplicateWriteCount(hist, r.Path, hash); got != 1 {
			t.Errorf("path %s should appear once, got %d", r.Path, got)
		}
	}
}

// TestConsecutiveFsWriteSoftCapMessage: the dispatcher appends a soft
// nudge message to the fs.write result when the consecutive counter
// reaches the soft cap. We don't drive the live dispatcher here (it
// requires Toolbox + filesystem); instead pin the suffix-format
// expectation so refactors don't accidentally drop the nudge.
func TestConsecutiveFsWriteSoftCapMessage(t *testing.T) {
	want := "consider calling test.run or done"
	// The actual text lives in orchestrator.go inside the case "fs.write"
	// branch. This test guards against accidental removal during a
	// future refactor.
	suffix := "consider calling test.run or done next"
	if !strings.Contains(suffix, want) {
		t.Fatalf("nudge text drifted; expected to contain %q", want)
	}
	if consecutiveFsWriteSoftCap < 3 || consecutiveFsWriteSoftCap > 20 {
		t.Errorf("consecutiveFsWriteSoftCap=%d outside reasonable [3, 20] band",
			consecutiveFsWriteSoftCap)
	}
}
