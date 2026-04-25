// Package taskstore persists task state to disk so the daemon can restart
// mid-task and resume at the last phase boundary.
package taskstore

import (
	"encoding/json"
	"fmt"
	"log"
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

// Clone returns a deep copy of the Task. Call this before returning a Task
// pointer across goroutine boundaries where the caller (e.g. HTTP response
// writer) will read fields concurrently with the run goroutine's writes.
// Strings are immutable in Go so a shallow copy suffices for those; slices
// and maps get fresh backing arrays so later writes to the live task do not
// race the cloned snapshot.
func (t *Task) Clone() *Task {
	if t == nil {
		return nil
	}
	c := *t
	if t.Reviews != nil {
		c.Reviews = make([]string, len(t.Reviews))
		copy(c.Reviews, t.Reviews)
	}
	if t.Meta != nil {
		c.Meta = make(map[string]string, len(t.Meta))
		for k, v := range t.Meta {
			c.Meta[k] = v
		}
	}
	return &c
}

type Store struct {
	dir string
	mu  sync.Mutex

	// In-memory cache keyed by task id. Populated lazily on first List()
	// call (cold start still walks the dir + reads each file once); kept
	// in sync by Save(). Without this, every /tasks GET and every /status
	// poll re-read every JSON file on disk -- O(N) per request, which
	// becomes painful past a few thousand tasks under live UI polling.
	cache       map[string]*Task
	cacheLoaded bool

	// maxScannedMTime is the highest mtime we observed among task JSON
	// files during the most recent List() walk. On subsequent List()
	// calls we scandir for any file whose mtime is greater than this
	// value and rescan only the new/changed entries. Lets out-of-band
	// edits (operator hand-fixes a corrupt task) become visible without
	// forcing every poller to do a full O(N) re-walk.
	maxScannedMTime time.Time

	// dirMtime is the mtime of the tasks directory at the time of the most
	// recent List() walk. On warm List() calls we stat just the dir; if its
	// mtime is unchanged AND nothing was Save()'d in-process since, we can
	// skip the os.ReadDir entirely and serve straight from the sorted cache.
	// Linux updates dir mtime on file create/unlink/rename, so this catches
	// the "new task file appeared" case without an O(N) entry scan.
	dirMtime time.Time

	// sortedCache is the canonical sorted-by-UpdatedAt-desc snapshot of all
	// cached tasks. Rebuilt only when cache contents change (Save, or List
	// detected a new/changed entry on disk). Entries are pointers into the
	// cache map; List() Clones them on the way out at the API boundary so
	// callers don't observe in-flight mutations.
	sortedCache []*Task
	// sortedDirty marks the sortedCache as needing rebuild on the next
	// List() call. Set by Save() and by the rescan path of List() when at
	// least one entry was added or refreshed.
	sortedDirty bool

	// corruptCount tracks the number of task JSON files that were
	// rejected during the cold-start List() walk (unreadable, malformed
	// JSON, or missing id). Exposed via CorruptCount() so operators can
	// see silent drops on /status instead of having to grep daemon
	// logs. Reset only on process restart.
	corruptCount int
}

// validStatuses lists the canonical task statuses. Task records loaded from
// disk that carry a Status outside this set are treated as corrupt and
// skipped (and counted via corruptCount) so a hand-edited file with a typo
// like "successful" instead of "succeeded" doesn't smuggle itself into the
// task list as a phantom row.
var validStatuses = map[Status]struct{}{
	StatusPending:     {},
	StatusRunning:     {},
	StatusAwaitUser:   {},
	StatusSucceeded:   {},
	StatusFailed:      {},
	StatusAborted:     {},
	StatusInterrupted: {},
}

// validStatus reports whether s is a recognised task status enum.
func validStatus(s Status) bool {
	_, ok := validStatuses[s]
	return ok
}

// sanitizeTimestamps clamps obviously-bogus timestamps to the current time
// so out-of-band edits or filesystems with broken clocks don't silently
// shift tasks decades into the future. We log on clamp so operators can
// investigate. Tasks remain queryable; only the timestamps are corrected.
func sanitizeTimestamps(t *Task) {
	now := time.Now().UTC()
	oneYearAhead := now.Add(365 * 24 * time.Hour)
	clamped := false
	if !t.CreatedAt.IsZero() && t.CreatedAt.After(oneYearAhead) {
		t.CreatedAt = now
		clamped = true
	}
	if !t.UpdatedAt.IsZero() && t.UpdatedAt.After(oneYearAhead) {
		t.UpdatedAt = now
		clamped = true
	}
	if !t.UpdatedAt.IsZero() && !t.CreatedAt.IsZero() && t.UpdatedAt.Before(t.CreatedAt) {
		t.UpdatedAt = t.CreatedAt
		clamped = true
	}
	if clamped {
		log.Printf("taskstore: clamped bogus timestamps on task %s", t.ID)
	}
}

func DefaultDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "agent-router", "tasks")
}

func Open(dir string) (*Store, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	// SEC-PASS5-005: state dirs at 0700 so other local users can't enumerate
	// task ids. Task JSON contents (descriptions, errors, plans) can carry
	// content the user pasted in (occasionally credentials/API keys); not
	// world-readable.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Tighten an existing dir created under the legacy 0o755. Best-effort:
	// MkdirAll on an existing dir does NOT chmod, so we Chmod explicitly.
	_ = os.Chmod(dir, 0o700)
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
	// SEC-PASS5-005: 0o600 so task JSON (descriptions/errors/plans, which
	// can echo pasted credentials) is not world-readable on shared hosts.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path(t.ID)); err != nil {
		return err
	}
	// Update the cache with a deep clone so concurrent readers can't see
	// the live struct mutate after this call returns.
	if s.cache != nil {
		s.cache[t.ID] = t.Clone()
		s.sortedDirty = true
	}
	return nil
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
	if !s.cacheLoaded {
		// Cold start: walk the dir, parse each JSON once, populate the
		// cache. Subsequent calls hit memory only.
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return nil, err
		}
		s.cache = make(map[string]*Task, len(entries))
		s.maxScannedMTime = time.Time{}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			id := e.Name()[:len(e.Name())-len(".json")]
			info, ierr := e.Info()
			if ierr == nil && info.ModTime().After(s.maxScannedMTime) {
				s.maxScannedMTime = info.ModTime()
			}
			s.loadAndCacheLocked(id, e.Name())
		}
		s.cacheLoaded = true
		s.sortedDirty = true
		// Capture the directory mtime so warm calls can fast-path on it.
		if di, derr := os.Stat(s.dir); derr == nil {
			s.dirMtime = di.ModTime()
		}
	} else {
		// PERF-1 fast path: stat the tasks directory only. Linux updates
		// dir mtime on entry create/unlink/rename, so an unchanged dirMtime
		// guarantees no new task files appeared, no task files were deleted,
		// and no task files were renamed (the .tmp -> final rename also
		// bumps it). In-process Save()s mark sortedDirty directly and update
		// the cache, so the only thing the dir-mtime check needs to catch
		// is out-of-band changes. If both are stable, we can skip ReadDir
		// entirely and serve the previous sortedCache.
		dirChanged := true
		if di, derr := os.Stat(s.dir); derr == nil {
			if !di.ModTime().After(s.dirMtime) {
				dirChanged = false
			} else {
				s.dirMtime = di.ModTime()
			}
		}
		if dirChanged {
			// Slow path: rescan files whose mtime is newer than the
			// most-recent mtime we've seen. Out-of-band edits (operator
			// hand-fixes a corrupt task) become visible here without a
			// full O(N) re-walk on every poll.
			entries, err := os.ReadDir(s.dir)
			if err == nil {
				newMax := s.maxScannedMTime
				rescanned := 0
				seen := make(map[string]struct{}, len(entries))
				for _, e := range entries {
					if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
						continue
					}
					id := e.Name()[:len(e.Name())-len(".json")]
					seen[id] = struct{}{}
					info, ierr := e.Info()
					if ierr != nil {
						continue
					}
					mt := info.ModTime()
					if !mt.After(s.maxScannedMTime) {
						continue
					}
					if mt.After(newMax) {
						newMax = mt
					}
					s.loadAndCacheLocked(id, e.Name())
					rescanned++
				}
				s.maxScannedMTime = newMax
				// Detect out-of-band deletions: any cached id missing from
				// the directory listing was unlinked behind our back. Drop
				// it so /tasks doesn't surface phantom rows.
				for id := range s.cache {
					if _, ok := seen[id]; !ok {
						delete(s.cache, id)
						rescanned++
					}
				}
				if rescanned > 0 {
					s.sortedDirty = true
				}
			}
		}
	}
	if s.sortedDirty || s.sortedCache == nil {
		// Rebuild the canonical sorted view. This is the only place that
		// pays the O(N log N) cost; warm calls with no changes skip it
		// entirely and return the previously-built slice (after Cloning
		// for caller isolation).
		s.sortedCache = make([]*Task, 0, len(s.cache))
		for _, t := range s.cache {
			s.sortedCache = append(s.sortedCache, t)
		}
		sort.Slice(s.sortedCache, func(i, j int) bool {
			return s.sortedCache[i].UpdatedAt.After(s.sortedCache[j].UpdatedAt)
		})
		s.sortedDirty = false
	}
	// Hand back deep clones so callers (HTTP encoder, run goroutine status
	// reads) don't observe in-flight mutations. Cloning is the only O(N)
	// work on the warm path now; the rest is a single os.Stat.
	out := make([]*Task, len(s.sortedCache))
	for i, t := range s.sortedCache {
		out[i] = t.Clone()
	}
	return out, nil
}

// loadAndCacheLocked parses a single task JSON file, validates it, and
// inserts it into the cache. Caller must hold s.mu. Validation failures
// (unreadable, malformed JSON, empty/mismatched id, invalid status enum)
// increment corruptCount and skip the entry rather than silently surfacing
// a phantom row in /tasks. Bogus timestamps are clamped to current time
// rather than rejected.
func (s *Store) loadAndCacheLocked(id, fileName string) {
	t, err := s.loadLocked(id)
	if err != nil {
		log.Printf("taskstore: skipping unreadable task file %s: %v", fileName, err)
		s.corruptCount++
		return
	}
	// A literal "{}" file unmarshals into a Task with all-empty fields.
	// Treat that as corruption rather than surface a phantom row in
	// /tasks. We also reject id-mismatched files so a manual rename
	// doesn't cause two ids to point at the same record.
	if t.ID == "" || t.ID != id {
		log.Printf("taskstore: skipping invalid task file %s (empty or mismatched id)", fileName)
		s.corruptCount++
		return
	}
	// Validate Status against the canonical enum. A hand-edited file with
	// "successful" instead of "succeeded" used to silently propagate as a
	// phantom status all the way up to the UI; reject these cleanly so
	// the operator sees a corruption count instead of mystery rows.
	if !validStatus(t.Status) {
		log.Printf("taskstore: skipping task file %s (invalid status %q)", fileName, t.Status)
		s.corruptCount++
		return
	}
	sanitizeTimestamps(t)
	s.cache[id] = t
}

// loadLocked is Load minus the mu acquisition; callers that already hold
// s.mu (List populating the cache) use this to avoid a recursive lock.
func (s *Store) loadLocked(id string) (*Task, error) {
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

// CorruptCount returns the number of task JSON files List() has skipped
// because they were unreadable, malformed, or had an empty/mismatched
// id. Cumulative for the lifetime of the process. Surfaces silent drops
// to /status so operators can investigate corruption rather than have
// it disappear into a log line.
func (s *Store) CorruptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.corruptCount
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
