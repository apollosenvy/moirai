package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
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
