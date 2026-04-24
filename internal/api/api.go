// Package api serves the agent-router HTTP interface.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/aegis/agent-router/internal/modelmgr"
	"github.com/aegis/agent-router/internal/orchestrator"
)

//go:embed index.html
var indexHTML []byte

type Server struct {
	Orch      *orchestrator.Orchestrator
	ModelMgr  *modelmgr.Manager
	StartedAt time.Time
	Port      int

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
	writeJSON(w, 200, map[string]any{
		"service":      "agent-router",
		"port":         s.Port,
		"started_at":   s.StartedAt.UTC().Format(time.RFC3339),
		"uptime":       time.Since(s.StartedAt).String(),
		"active_slot":  s.ModelMgr.Active(),
		"active_port":  s.ModelMgr.ActivePort(),
		"task_count":   len(tasks),
		"running":      running,
	})
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
