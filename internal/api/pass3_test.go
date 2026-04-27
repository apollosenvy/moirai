package api

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aegis/moirai/internal/orchestrator"
	"github.com/aegis/moirai/internal/taskstore"
)

// TestSubmitWhitespaceDescReturns400 covers pass-3 B4: a description that's
// only whitespace must surface as 400, not 500. The HTTP handler trims
// before its own guard, and the orchestrator returns ErrInvalidInput which
// maps to 400.
func TestSubmitWhitespaceDescReturns400(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := taskstore.Open(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatalf("taskstore: %v", err)
	}
	orch, err := orchestrator.New(orchestrator.Config{
		Store:       store,
		DefaultRepo: t.TempDir(),
		ModelMgr:    &stubModelMgr{},
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	s := newTestServer(true)
	s.Orch = orch

	for _, body := range []string{
		`{"description":"   "}`,
		`{"description":"\t\n "}`,
		`{"description":""}`,
	} {
		req := httptest.NewRequest("POST", "/submit", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != 400 {
			t.Errorf("body=%s expected 400, got %d body=%s", body, w.Code, w.Body.String())
		}
	}
}

// TestSubmitMissingRepoReturns400 covers the B4 bonus: missing/invalid repo
// path is a caller fault and should render as 400, not 500.
func TestSubmitMissingRepoReturns400(t *testing.T) {
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
	s := newTestServer(true)
	s.Orch = orch

	// No DefaultRepo configured, no repo_root field in body -> "no repo
	// root" which wraps ErrInvalidInput -> 400.
	req := httptest.NewRequest("POST", "/submit",
		bytes.NewReader([]byte(`{"description":"hello"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("missing repo expected 400, got %d body=%s", w.Code, w.Body.String())
	}

	// repo_root that doesn't exist on disk -> 400.
	req = httptest.NewRequest("POST", "/submit",
		bytes.NewReader([]byte(`{"description":"hi","repo_root":"/nonexistent/deepmonkey"}`)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("bad repo path expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestAbortUnknownTaskReturns404 covers pass-3 B5: aborting an id the
// orchestrator has no record of must surface as 404, not 400.
func TestAbortUnknownTaskReturns404(t *testing.T) {
	s, _ := newAPIServerWithRunning(t)
	req := httptest.NewRequest("POST", "/tasks/nonexistent-id/abort", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 404 {
		t.Errorf("abort unknown expected 404, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not found") {
		t.Errorf("expected body to mention 'not found', got %s", w.Body.String())
	}
}
