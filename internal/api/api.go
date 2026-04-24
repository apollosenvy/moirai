// Package api serves the agent-router HTTP interface.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aegis/agent-router/internal/modelmgr"
	"github.com/aegis/agent-router/internal/models"
	"github.com/aegis/agent-router/internal/orchestrator"
	"github.com/aegis/agent-router/internal/taskstore"
)

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

	var body patchSlotBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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
	// Loaded but idle -- queue until the next natural swap. See DEVIATION in
	// the commit message / SPEC_DEVIATIONS.md for the reasoning.
	s.ModelMgr.SetPending(slot, pending)
	writeJSON(w, 200, map[string]any{"applied": false, "pending": true, "reason": "queued-for-next-swap"})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "GET required"})
		return
	}
	infos, err := models.ListGGUF(s.ModelsDir)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// Include current slot model paths even if outside ModelsDir.
	slots := s.ModelMgr.SlotsView()
	paths := make([]string, 0, len(slots))
	for _, sl := range slots {
		paths = append(paths, sl.ModelPath)
	}
	infos = models.IncludeCurrent(infos, paths)
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
	var activeTask *taskstore.Task
	for _, t := range tasks {
		if string(t.Status) == "running" {
			running++
			if activeTask == nil {
				activeTask = t
			}
		}
	}
	var phase taskstore.Phase
	if activeTask != nil {
		phase = activeTask.Phase
	}
	verdict := s.Orch.LastVerdict()
	nextSlots := orchestrator.NextSlots(phase, verdict, activeTask != nil)
	reviewStage := orchestrator.ReviewStage(phase)

	writeJSON(w, 200, map[string]any{
		"service":              "agent-router",
		"port":                 s.Port,
		"started_at":           s.StartedAt.UTC().Format(time.RFC3339),
		"uptime":               time.Since(s.StartedAt).String(),
		"active_slot":          s.ModelMgr.Active(),
		"active_port":          s.ModelMgr.ActivePort(),
		"task_count":           len(tasks),
		"running":              running,
		"next_slots":           nextSlots,
		"review_stage":         reviewStage,
		"last_verdict":         nullIfEmpty(verdict),
		"turboquant_supported": s.TurboquantSupported,
		"daemon_version":       s.DaemonVersion,
	})
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
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
		res, err := s.Orch.Inspect(id)
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, res)
	case "abort":
		if r.Method != "POST" {
			writeJSON(w, 405, map[string]string{"error": "POST required"})
			return
		}
		if err := s.Orch.Abort(id); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"aborted": id})
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
	var req submitReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.Description == "" {
		writeJSON(w, 400, map[string]string{"error": "description required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	t, err := s.Orch.Submit(ctx, req.Description, req.RepoRoot)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, t)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(w, `{"error": "%s"}`, err.Error())
		return
	}
	w.Write(data)
	w.Write([]byte("\n"))
}
