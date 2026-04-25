package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aegis/agent-router/internal/modelmgr"
	"github.com/aegis/agent-router/internal/orchestrator"
	"github.com/aegis/agent-router/internal/taskstore"
	"github.com/aegis/agent-router/internal/trace"
)

// stubModelMgr is a no-op ModelManager for tests that don't exercise a real
// llama-server swap. Satisfies orchestrator.ModelManager.
type stubModelMgr struct{}

func (s *stubModelMgr) EnsureSlot(ctx context.Context, slot modelmgr.Slot) (string, error) {
	return "http://stub", nil
}
func (s *stubModelMgr) Active() modelmgr.Slot { return "" }
func (s *stubModelMgr) Complete(ctx context.Context, req modelmgr.ChatRequest) (string, error) {
	return "", nil
}

// newAPIServerWithRunning wires a real Orchestrator with one running task
// so we can exercise /tasks/<id>/interrupt and /tasks/<id>/inject without a
// live llama-server.
func newAPIServerWithRunning(t *testing.T) (*Server, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store, err := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatalf("taskstore: %v", err)
	}
	orch, err := orchestrator.New(orchestrator.Config{
		Store:    store,
		ModelMgr: &stubModelMgr{},
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	id, err := orchestrator.SeedRunningForTest(orch, store)
	if err != nil {
		t.Fatalf("seed running: %v", err)
	}

	s := newTestServer(true)
	s.Orch = orch
	return s, id
}

func TestAPIInterruptEndpoint(t *testing.T) {
	s, id := newAPIServerWithRunning(t)

	// POST /tasks/<id>/interrupt on a running task -> 200.
	req := httptest.NewRequest("POST", "/tasks/"+id+"/interrupt", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["interrupted"] != id {
		t.Errorf("expected interrupted=%q, got %v", id, body)
	}

	// POST on unknown task -> 404 (ErrTaskNotFound is wrapped by the
	// orchestrator and mapped at the HTTP boundary).
	req = httptest.NewRequest("POST", "/tasks/nope-not-a-real-id/interrupt", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 404 {
		t.Errorf("unknown-task expected 404, got %d", w.Code)
	}

	// GET on interrupt -> 405.
	req = httptest.NewRequest("GET", "/tasks/"+id+"/interrupt", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 405 {
		t.Errorf("GET /interrupt expected 405, got %d", w.Code)
	}
}

func TestAPIInjectEndpoint(t *testing.T) {
	s, id := newAPIServerWithRunning(t)

	// POST with {message:"hi"} -> 200.
	req := httptest.NewRequest("POST", "/tasks/"+id+"/inject",
		bytes.NewReader([]byte(`{"message":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200 for valid inject, got %d body=%s", w.Code, w.Body.String())
	}

	// Empty body -> 400 (empty message is rejected by Inject).
	req = httptest.NewRequest("POST", "/tasks/"+id+"/inject",
		bytes.NewReader([]byte(`{"message":""}`)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("empty message expected 400, got %d", w.Code)
	}

	// Malformed JSON -> 400.
	req = httptest.NewRequest("POST", "/tasks/"+id+"/inject",
		bytes.NewReader([]byte(`not json at all`)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("malformed JSON expected 400, got %d", w.Code)
	}

	// POST on unknown task -> 404 (the persisted record doesn't exist;
	// orchestrator wraps ErrTaskNotFound which the HTTP layer maps).
	req = httptest.NewRequest("POST", "/tasks/never-ran/inject",
		bytes.NewReader([]byte(`{"message":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 404 {
		t.Errorf("inject on unknown task expected 404, got %d", w.Code)
	}
}

func TestAPITasksByIDInspectRequiresGET(t *testing.T) {
	s, id := newAPIServerWithRunning(t)
	// Seed a trace so Inspect would succeed for the id, but POST should be
	// rejected before we get that far.
	tr, _ := trace.Open(id)
	defer tr.Close()

	req := httptest.NewRequest("POST", "/tasks/"+id, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 405 {
		t.Errorf("POST /tasks/<id> (no action) expected 405, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAPIUnknownAction(t *testing.T) {
	s, id := newAPIServerWithRunning(t)
	req := httptest.NewRequest("POST", "/tasks/"+id+"/bogus", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 404 {
		t.Errorf("unknown action expected 404, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unknown action") {
		t.Errorf("expected error body to mention 'unknown action', got %s", w.Body.String())
	}
}
