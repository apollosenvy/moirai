// Package api serves the moirai HTTP interface.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aegis/moirai/internal/modelmgr"
	"github.com/aegis/moirai/internal/models"
	"github.com/aegis/moirai/internal/orchestrator"
)

// orchestratorErrorStatus maps an orchestrator error to the HTTP status the
// client should see. Callers pattern-match on the sentinel values rather than
// on error text so the mapping stays stable if error strings are reworded.
func orchestratorErrorStatus(err error) int {
	switch {
	case errors.Is(err, orchestrator.ErrInvalidInput):
		return 400
	case errors.Is(err, orchestrator.ErrTaskNotFound):
		return 404
	case errors.Is(err, orchestrator.ErrTerminalTask):
		return 409
	default:
		return 500
	}
}

//go:embed index.html
var indexHTML []byte

type Server struct {
	Orch      *orchestrator.Orchestrator
	ModelMgr  *modelmgr.Manager
	StartedAt time.Time
	Port      int

	// ModelsDir is the filesystem directory scanned by GET /models for GGUF
	// files. Slot-active model paths from outside this dir are merged in.
	ModelsDir string

	// TurboquantSupported is reported via /status so the UI can gate the
	// turbo3/turbo4 KV options. Set by daemon main() after DetectTurboquant.
	TurboquantSupported bool

	// DaemonVersion appears in /status for UI display / debug.
	DaemonVersion string

	// ReadyFlag is flipped to true once the daemon has finished model-manager
	// initialization, orchestrator task replay, and turboquant detection.
	// Daemon main() should call s.ReadyFlag.Store(true) as its last startup step.
	ReadyFlag atomic.Bool
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/tasks", s.handleTasks)
	mux.HandleFunc("/tasks/", s.handleTasksByID)
	mux.HandleFunc("/submit", s.handleSubmit)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)
	mux.HandleFunc("/slots", s.handleSlots)
	mux.HandleFunc("/slots/", s.handleSlotsByID)
	mux.HandleFunc("/models", s.handleModels)
	mux.HandleFunc("/", s.handleUI)
	return mux
}

func (s *Server) handleSlots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "GET required"})
		return
	}
	writeJSON(w, 200, s.ModelMgr.SlotsView())
}

type patchSlotBody struct {
	ModelPath string `json:"model_path,omitempty"`
	CtxSize   int    `json:"ctx_size,omitempty"`
	KvCache   string `json:"kv_cache,omitempty"`
}

var validKvValues = map[string]bool{
	"":       true,
	"f16":    true,
	"q8_0":   true,
	"q5_1":   true,
	"q4_0":   true,
	"turbo3": true,
	"turbo4": true,
}

func validatePatchBody(b patchSlotBody) error {
	if b.KvCache != "" && !validKvValues[b.KvCache] {
		return fmt.Errorf("invalid kv_cache %q", b.KvCache)
	}
	if b.CtxSize != 0 {
		if b.CtxSize < 8192 || b.CtxSize > 2097152 {
			return fmt.Errorf("ctx_size out of range [8192, 2097152]: %d", b.CtxSize)
		}
		if b.CtxSize%8192 != 0 {
			return fmt.Errorf("ctx_size must be multiple of 8192")
		}
	}
	if b.ModelPath != "" {
		if !strings.HasSuffix(b.ModelPath, ".gguf") {
			return fmt.Errorf("model_path must end in .gguf")
		}
		if _, err := os.Stat(b.ModelPath); err != nil {
			return fmt.Errorf("model_path not found: %v", err)
		}
	}
	return nil
}

func (s *Server) handleSlotsByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		writeJSON(w, 405, map[string]string{"error": "PATCH required"})
		return
	}
	slotName := strings.TrimPrefix(r.URL.Path, "/slots/")
	if slotName == "" || strings.Contains(slotName, "/") {
		writeJSON(w, 400, map[string]string{"error": "invalid slot"})
		return
	}
	slot := modelmgr.Slot(slotName)
	// Validate slot exists.
	found := false
	views := s.ModelMgr.SlotsView()
	var this modelmgr.SlotView
	for _, v := range views {
		if v.Slot == slot {
			found = true
			this = v
			break
		}
	}
	if !found {
		writeJSON(w, 404, map[string]string{"error": "unknown slot"})
		return
	}

	// Cap PATCH body at 64 KiB. The patchSlotBody surface is four small
	// fields; anything larger is either a hostile/buggy client or an
	// accidental large paste. /submit and /inject already cap their own
	// bodies; this closes the asymmetry flagged in the pass-3 audit.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body patchSlotBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := validatePatchBody(body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	pending := modelmgr.PendingChanges{
		ModelPath: body.ModelPath,
		CtxSize:   body.CtxSize,
		KvCache:   body.KvCache,
	}

	if !this.Loaded {
		// Cold slot -- apply to config immediately, no restart needed.
		s.ModelMgr.SetPending(slot, pending)
		s.ModelMgr.ApplyPending(slot)
		writeJSON(w, 200, map[string]any{"applied": true, "pending": false, "reason": "ok"})
		return
	}
	if this.Generating {
		// Busy -- queue.
		s.ModelMgr.SetPending(slot, pending)
		writeJSON(w, 200, map[string]any{"applied": false, "pending": true, "reason": "busy"})
		return
	}
	// Loaded but idle -- queue until the next natural swap. See DEVIATION
	// note in the originating commit for the reasoning.
	s.ModelMgr.SetPending(slot, pending)
	writeJSON(w, 200, map[string]any{"applied": false, "pending": true, "reason": "queued-for-next-swap"})
}

// modelsCache memoises the most recent ListGGUF + IncludeCurrent merge so
// /models hits don't re-scan ModelsDir on every UI poll. PERF-2.
var (
	modelsMu        sync.Mutex
	modelsCacheAt   time.Time
	modelsCacheData []models.Info
	modelsCacheKey  string
)

const modelsCacheTTL = 30 * time.Second

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "GET required"})
		return
	}
	// Build the cache key from ModelsDir + the current slot paths so we
	// invalidate whenever a slot swap changes which paths get merged in.
	slots := s.ModelMgr.SlotsView()
	paths := make([]string, 0, len(slots))
	for _, sl := range slots {
		paths = append(paths, sl.ModelPath)
	}
	key := s.ModelsDir + "|" + strings.Join(paths, "|")

	modelsMu.Lock()
	if modelsCacheKey == key && time.Since(modelsCacheAt) < modelsCacheTTL && modelsCacheData != nil {
		out := modelsCacheData
		modelsMu.Unlock()
		writeJSON(w, 200, out)
		return
	}
	modelsMu.Unlock()

	infos, err := models.ListGGUF(s.ModelsDir)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	infos = models.IncludeCurrent(infos, paths)

	modelsMu.Lock()
	modelsCacheKey = key
	modelsCacheAt = time.Now()
	modelsCacheData = infos
	modelsMu.Unlock()

	writeJSON(w, 200, infos)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.ReadyFlag.Load() {
		writeJSON(w, 200, map[string]any{"ready": true})
		return
	}
	writeJSON(w, 503, map[string]any{"ready": false, "waiting_on": []string{"initializing"}})
}

// handleUI serves the embedded single-page dashboard. Only responds at the
// root path; anything else gets a 404 so the SPA doesn't swallow API typos.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	w.Write(indexHTML)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	tasks, _ := s.Orch.Status()
	running := 0
	for _, t := range tasks {
		if string(t.Status) == "running" {
			running++
		}
	}
	verdict := s.Orch.LastVerdict()
	vramUsed, vramTotal := readVRAM()

	writeJSON(w, 200, map[string]any{
		"service":              "agent-router",
		"port":                 s.Port,
		"started_at":           s.StartedAt.UTC().Format(time.RFC3339),
		"uptime":               time.Since(s.StartedAt).String(),
		"active_slot":          s.ModelMgr.Active(),
		"active_port":          s.ModelMgr.ActivePort(),
		"task_count":           len(tasks),
		"running":              running,
		"last_verdict":         nullIfEmpty(verdict),
		"turboquant_supported": s.TurboquantSupported,
		"daemon_version":       s.DaemonVersion,
		"max_ro_turns":         s.Orch.MaxROTurns(),
		"vram_used_mb":         vramUsed,
		"vram_total_mb":        vramTotal,
		"corrupt_task_count":   s.Orch.CorruptTaskCount(),
	})
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// vramCache memoises the most recent rocm-smi parse so /status (which
// is polled aggressively by the UI) doesn't fork+exec the tool on every
// hit. Cache TTL is 5s -- long enough to absorb the 3s UI poll cadence,
// short enough that operator-visible VRAM swings on slot swaps don't
// look frozen.
var (
	vramMu        sync.Mutex
	vramAt        time.Time
	vramUsedCache int
	vramTotCache  int
)

const vramCacheTTL = 5 * time.Second

// readVRAM returns (used_mb, total_mb) from rocm-smi. Returns (0, 0) on
// any failure (binary missing, parse error, GPU not present) so /status
// degrades cleanly on machines without the tool.
func readVRAM() (int, int) {
	vramMu.Lock()
	defer vramMu.Unlock()
	if time.Since(vramAt) < vramCacheTTL {
		return vramUsedCache, vramTotCache
	}
	used, total := queryROCmSMI()
	vramUsedCache = used
	vramTotCache = total
	vramAt = time.Now()
	return used, total
}

func queryROCmSMI() (int, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "rocm-smi", "--showmeminfo", "vram", "--csv").Output()
	if err != nil {
		return 0, 0
	}
	// Output is CSV with a header; the data row contains:
	//   device,VRAM Total Memory (B),VRAM Total Used Memory (B)
	// Find the first non-header row that has at least 3 fields and parse.
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(strings.ToLower(line), "device") {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		totalB, err1 := strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 64)
		usedB, err2 := strconv.ParseUint(strings.TrimSpace(fields[2]), 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		return int(usedB / (1024 * 1024)), int(totalB / (1024 * 1024))
	}
	return 0, 0
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "GET required"})
		return
	}
	tasks, err := s.Orch.Status()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, tasks)
}

func (s *Server) handleTasksByID(w http.ResponseWriter, r *http.Request) {
	// /tasks/<id> or /tasks/<id>/abort
	path := r.URL.Path[len("/tasks/"):]
	id := path
	action := ""
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			id = path[:i]
			action = path[i+1:]
			break
		}
	}
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "missing task id"})
		return
	}
	switch action {
	case "":
		if r.Method != http.MethodGet {
			writeJSON(w, 405, map[string]string{"error": "GET required"})
			return
		}
		res, err := s.Orch.Inspect(id)
		if err != nil {
			// Inspect now returns ErrTaskNotFound for missing tasks; map it
			// to a clean 404 body without leaking the on-disk path.
			if errors.Is(err, orchestrator.ErrTaskNotFound) {
				writeJSON(w, 404, map[string]string{"error": fmt.Sprintf("task not found: %s", id)})
				return
			}
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, res)
	case "abort":
		if r.Method != "POST" {
			writeJSON(w, 405, map[string]string{"error": "POST required"})
			return
		}
		if err := s.Orch.Abort(id); err != nil {
			// Map not-found to 404; terminal-state collisions to 409;
			// anything else is a client-ish fault (invalid id shape) so 400
			// remains a reasonable default.
			status := 400
			switch {
			case errors.Is(err, orchestrator.ErrTaskNotFound):
				status = 404
			case errors.Is(err, orchestrator.ErrTerminalTask):
				status = 409
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"aborted": id})
	case "interrupt":
		if r.Method != "POST" {
			writeJSON(w, 405, map[string]string{"error": "POST required"})
			return
		}
		if err := s.Orch.Interrupt(id); err != nil {
			status := 400
			switch {
			case errors.Is(err, orchestrator.ErrTaskNotFound):
				status = 404
			case errors.Is(err, orchestrator.ErrTerminalTask):
				status = 409
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"interrupted": id})
	case "inject":
		if r.Method != "POST" {
			writeJSON(w, 405, map[string]string{"error": "POST required"})
			return
		}
		// Cap inject body at 256 KiB. A realistic nudge is a handful of
		// lines; anything larger is either pasted context (belongs in the
		// task description) or malformed input. MaxBytesReader surfaces
		// its own error via the decoder when the cap is hit.
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		var body struct {
			Message string `json:"message"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if err := s.Orch.Inject(id, body.Message); err != nil {
			status := 400
			switch {
			case errors.Is(err, orchestrator.ErrTaskNotFound):
				status = 404
			case errors.Is(err, orchestrator.ErrTerminalTask):
				status = 409
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"injected": id})
	default:
		writeJSON(w, 404, map[string]string{"error": "unknown action"})
	}
}

type submitReq struct {
	Description string `json:"description"`
	RepoRoot    string `json:"repo_root"`
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "POST required"})
		return
	}
	// Cap submit body at 256 KiB. Even the most verbose task description
	// plus a repo_root fits comfortably; oversize bodies are either bugs
	// or abuse.
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var req submitReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	// Trim before the empty-check so whitespace-only descriptions (" ",
	// "\t\n") match the orchestrator's own guard and surface as 400 rather
	// than being passed through and bounced back as 500.
	if strings.TrimSpace(req.Description) == "" {
		writeJSON(w, 400, map[string]string{"error": "description required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	t, err := s.Orch.Submit(ctx, req.Description, req.RepoRoot)
	if err != nil {
		writeJSON(w, orchestratorErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, t)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// PERF-9: stream encode rather than allocate the full payload first.
	// On /tasks at 10k tasks the marshalled JSON is ~5MB; json.Marshal +
	// Write allocates that buffer per request, which is the largest
	// allocator-pressure source under live UI polling. Encoder.Encode also
	// writes a trailing newline for free, so we drop the explicit "\n".
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		// Headers are already flushed; best we can do is append an error
		// line. The status code is whatever the caller set (often 200 by
		// the time we get here), so the client will see a malformed body
		// rather than an HTTP error -- that's fine because Encode failures
		// are nearly always upstream client-disconnects, not encoder bugs.
		fmt.Fprintf(w, `{"error": "%s"}`+"\n", err.Error())
		return
	}
}
