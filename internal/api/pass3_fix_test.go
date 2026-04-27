package api

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aegis/moirai/internal/modelmgr"
	"github.com/aegis/moirai/internal/orchestrator"
	"github.com/aegis/moirai/internal/taskstore"
)

// TestInspectNotFoundDoesNotLeakPath covers the pass-3 IMPORTANT finding:
// GET /tasks/<unknown-id> previously bubbled up the raw os.Open error
// ("open /home/.../tasks/badid.json: no such file or directory"), leaking
// the daemon's on-disk layout. After the fix the body is a clean
// "task not found: <id>" with no leading slash from the data dir.
func TestInspectNotFoundDoesNotLeakPath(t *testing.T) {
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

	req := httptest.NewRequest("GET", "/tasks/never-existed", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "/home/") || strings.Contains(body, ".local/share") ||
		strings.Contains(body, ".json") || strings.Contains(body, "no such file") {
		t.Errorf("404 body leaks filesystem layout: %s", body)
	}
	if !strings.Contains(body, "task not found") {
		t.Errorf("expected 'task not found' in body, got %s", body)
	}
}

// TestAbortTerminalReturns409 -- pass-3 MINOR finding. Calling /abort on a
// task already in succeeded/failed/aborted state used to return 200
// silently. Now: 409 Conflict with a body that names the current status.
func TestAbortTerminalReturns409(t *testing.T) {
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

	for _, status := range []taskstore.Status{
		taskstore.StatusSucceeded, taskstore.StatusFailed, taskstore.StatusAborted,
	} {
		id, err := orchestrator.SeedTerminalForTest(store, status)
		if err != nil {
			t.Fatalf("seed terminal %s: %v", status, err)
		}
		req := httptest.NewRequest("POST", "/tasks/"+id+"/abort", nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != 409 {
			t.Errorf("status=%s expected 409, got %d body=%s", status, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), string(status)) {
			t.Errorf("status=%s expected body to mention %q, got %s", status, status, w.Body.String())
		}
	}
}

// TestInjectTerminalReturns409 -- companion to abort: /inject on a terminal
// task should also surface 409, not 200.
func TestInjectTerminalReturns409(t *testing.T) {
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

	id, err := orchestrator.SeedTerminalForTest(store, taskstore.StatusSucceeded)
	if err != nil {
		t.Fatalf("seed terminal: %v", err)
	}
	req := httptest.NewRequest("POST", "/tasks/"+id+"/inject",
		bytes.NewReader([]byte(`{"message":"too late"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 409 {
		t.Errorf("expected 409 for inject on terminal task, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestSubmitDisallowsUnknownFields covers pass-3 EDGE-10 fix: the /submit
// JSON decoder is now strict, surfacing typo'd keys as 400 instead of
// silently dropping them.
func TestSubmitDisallowsUnknownFields(t *testing.T) {
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

	// `descirption` (typo) used to be silently dropped, leaving the real
	// description blank and bouncing as 400 ("description required") --
	// confusing because the operator sent SOMETHING. Strict decode rejects
	// the unknown field directly.
	req := httptest.NewRequest("POST", "/submit",
		bytes.NewReader([]byte(`{"descirption":"hi","repo_root":""}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400 for unknown field, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestSlotsPatchBodyTooLargeReturns400 covers pass-3 EDGE-1: PATCH /slots/<id>
// must enforce a body size cap consistent with /submit and /inject. We build
// a real (cold) modelmgr.Manager so the handler walks past the slot lookup
// and into the body-decode path. MaxBytesReader surfaces an error via the
// decoder when the cap is hit, yielding 400.
func TestSlotsPatchBodyTooLargeReturns400(t *testing.T) {
	mm, err := modelmgr.New(modelmgr.Config{
		LlamaServerBin: "/bin/true", // never actually invoked in this test
		LogDir:         t.TempDir(),
		Models: map[modelmgr.Slot]modelmgr.ModelConfig{
			modelmgr.SlotPlanner: {
				Slot:      modelmgr.SlotPlanner,
				ModelPath: "/tmp/fake.gguf",
				Port:      8001,
			},
		},
	})
	if err != nil {
		t.Fatalf("modelmgr.New: %v", err)
	}
	s := newTestServer(true)
	s.ModelMgr = mm

	// 128KB body -- exceeds the 64KB cap -- should yield 400.
	huge := bytes.Repeat([]byte("a"), 128<<10)
	req := httptest.NewRequest("PATCH", "/slots/"+string(modelmgr.SlotPlanner),
		bytes.NewReader(huge))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400 from oversize PATCH body, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestSlotsPatchDisallowsUnknownFields covers EDGE-10 fix on PATCH:
// strict decode surfaces typo'd field names as 400 instead of silently
// dropping them.
func TestSlotsPatchDisallowsUnknownFields(t *testing.T) {
	mm, err := modelmgr.New(modelmgr.Config{
		LlamaServerBin: "/bin/true",
		LogDir:         t.TempDir(),
		Models: map[modelmgr.Slot]modelmgr.ModelConfig{
			modelmgr.SlotPlanner: {
				Slot:      modelmgr.SlotPlanner,
				ModelPath: "/tmp/fake.gguf",
				Port:      8001,
			},
		},
	})
	if err != nil {
		t.Fatalf("modelmgr.New: %v", err)
	}
	s := newTestServer(true)
	s.ModelMgr = mm

	req := httptest.NewRequest("PATCH", "/slots/"+string(modelmgr.SlotPlanner),
		bytes.NewReader([]byte(`{"kv_cahce":"q4_0"}`))) // typo
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400 for unknown PATCH field, got %d body=%s", w.Code, w.Body.String())
	}
}
