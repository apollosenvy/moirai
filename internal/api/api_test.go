package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aegis/agent-router/internal/modelmgr"
)

func newTestServer(ready bool) *Server {
	s := &Server{
		StartedAt: time.Now(),
		Port:      5984,
	}
	if ready {
		s.ReadyFlag.Store(true)
	}
	return s
}

func TestHealthAlways200(t *testing.T) {
	s := newTestServer(false)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestReadyReturns503BeforeReady(t *testing.T) {
	s := newTestServer(false)
	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 503 {
		t.Errorf("expected 503 before ready, got %d", w.Code)
	}
}

func TestReadyReturns200WhenReady(t *testing.T) {
	s := newTestServer(true)
	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200 when ready, got %d", w.Code)
	}
}

// Confirm we're actually using atomic.Bool.
var _ = atomic.Bool{}

// Ensure package import doesn't get flagged unused if we trim later.
var _ = http.StatusOK

func TestModelsEndpointReturnsGGUFs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha.gguf", "beta.gguf", "skip.txt"} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		f.Truncate(1024)
		f.Close()
	}
	s := newTestServer(true)
	s.ModelsDir = dir
	mgr, err := modelmgr.New(modelmgr.Config{
		LlamaServerBin: "/bin/true",
		Models: map[modelmgr.Slot]modelmgr.ModelConfig{
			modelmgr.SlotPlanner: {Slot: modelmgr.SlotPlanner, ModelPath: "/tmp/x.gguf", Port: 9001},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.ModelMgr = mgr

	req := httptest.NewRequest("GET", "/models", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "alpha") || !strings.Contains(body, "beta") {
		t.Errorf("expected alpha+beta in body, got %s", body)
	}
	if strings.Contains(body, "skip") {
		t.Errorf("expected non-gguf filtered out, got %s", body)
	}
}
