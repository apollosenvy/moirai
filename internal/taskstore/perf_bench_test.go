package taskstore

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkColdListN measures cold-start List() cost across a state-dir
// pre-populated with N task JSON files. Reports nanoseconds per call. Run
// with -benchtime=1x to sample once with each N.
func benchColdList(b *testing.B, n int) {
	dir := b.TempDir()
	store, err := Open(filepath.Join(dir, "tasks"))
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	for i := 0; i < n; i++ {
		t := &Task{
			ID:        fmt.Sprintf("task-%06d", i),
			Status:    StatusSucceeded,
			Phase:     PhaseDone,
			CreatedAt: time.Now().UTC(),
		}
		if err := store.Save(t); err != nil {
			b.Fatalf("save: %v", err)
		}
	}
	// Reset cache so each iteration measures cold start.
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		store.cache = nil
		store.cacheLoaded = false
		store.maxScannedMTime = time.Time{}
		b.StartTimer()
		_, err := store.List()
		if err != nil {
			b.Fatalf("list: %v", err)
		}
	}
}

func BenchmarkColdList100(b *testing.B)   { benchColdList(b, 100) }
func BenchmarkColdList1000(b *testing.B)  { benchColdList(b, 1000) }
func BenchmarkColdList5000(b *testing.B)  { benchColdList(b, 5000) }
func BenchmarkColdList10000(b *testing.B) { benchColdList(b, 10000) }

// BenchmarkWarmListN measures warm-path (cache-hit, mtime-unchanged) List
// cost. This is the hot path under live UI polling.
func benchWarmList(b *testing.B, n int) {
	dir := b.TempDir()
	store, err := Open(filepath.Join(dir, "tasks"))
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	for i := 0; i < n; i++ {
		t := &Task{
			ID:        fmt.Sprintf("task-%06d", i),
			Status:    StatusSucceeded,
			Phase:     PhaseDone,
			CreatedAt: time.Now().UTC(),
		}
		if err := store.Save(t); err != nil {
			b.Fatalf("save: %v", err)
		}
	}
	// Prime cache.
	if _, err := store.List(); err != nil {
		b.Fatalf("prime: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.List()
		if err != nil {
			b.Fatalf("list: %v", err)
		}
	}
}

func BenchmarkWarmList100(b *testing.B)   { benchWarmList(b, 100) }
func BenchmarkWarmList1000(b *testing.B)  { benchWarmList(b, 1000) }
func BenchmarkWarmList5000(b *testing.B)  { benchWarmList(b, 5000) }
func BenchmarkWarmList10000(b *testing.B) { benchWarmList(b, 10000) }

// TestWarmListUnder1msAt10k pins the PERF-1 success criterion: with the
// dir-mtime fast path and the cached sortedCache, a warm List() at 10k
// tasks should comfortably beat 1ms even on a contended runner. We
// measure the best of 5 calls so a single GC blip can't fail the test.
func TestWarmListUnder1msAt10k(t *testing.T) {
	if testing.Short() {
		t.Skip("perf assertion: skip under -short")
	}
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "tasks"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const n = 10000
	for i := 0; i < n; i++ {
		task := &Task{
			ID:        fmt.Sprintf("task-%06d", i),
			Status:    StatusSucceeded,
			Phase:     PhaseDone,
			CreatedAt: time.Now().UTC(),
		}
		if err := store.Save(task); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	// Prime: first List() rebuilds the sorted cache.
	if _, err := store.List(); err != nil {
		t.Fatalf("prime: %v", err)
	}
	var best time.Duration = time.Hour
	for i := 0; i < 5; i++ {
		start := time.Now()
		if _, err := store.List(); err != nil {
			t.Fatalf("warm list %d: %v", i, err)
		}
		d := time.Since(start)
		if d < best {
			best = d
		}
	}
	// Limit. Race detector adds 5-10x allocator overhead; loosen the bound
	// when raceEnabled (set in race_on_test.go) to avoid spurious failures.
	limit := time.Millisecond
	if raceEnabled {
		limit = 10 * time.Millisecond
	}
	if best >= limit {
		t.Errorf("warm List() at 10k tasks: best=%v, expected <%v (PERF-1 target, race=%v)", best, limit, raceEnabled)
	}
	t.Logf("warm List() at 10k tasks: best of 5 = %v (limit %v, race=%v)", best, limit, raceEnabled)
}
