package modelmgr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
)

// TestChatRequestJSONWireShape locks down the llama.cpp-compatible field
// names on ChatRequest. These are load-bearing: llama-server parses them
// by name. Any rename breaks the live daemon.
func TestChatRequestJSONWireShape(t *testing.T) {
	req := ChatRequest{
		Messages:    []ChatMessage{{Role: "user", Content: "hi"}},
		Temperature: 0.5,
		TopK:        40,
		TopP:        0.9,
		MinP:        0.05,
		MaxTokens:   1024,
		Stream:      true,
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Unmarshal to a map so we can enumerate keys.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	wantKeys := []string{
		"temperature", "top_k", "top_p", "min_p",
		"max_tokens", "stream", "messages",
	}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("expected key %q in marshalled ChatRequest, got %v", k, keys(m))
		}
	}
	// omitempty: a zero-valued ChatRequest should collapse numeric fields
	// out of the JSON but still carry `stream` (no omitempty, matches
	// llama.cpp expectation).
	zero := ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}
	zeroData, _ := json.Marshal(zero)
	var zeroMap map[string]json.RawMessage
	_ = json.Unmarshal(zeroData, &zeroMap)
	for _, k := range []string{"temperature", "top_k", "top_p", "min_p", "max_tokens"} {
		if _, ok := zeroMap[k]; ok {
			t.Errorf("expected key %q omitted at zero value, still present: %s", k, zeroData)
		}
	}
	if _, ok := zeroMap["stream"]; !ok {
		t.Errorf("stream must stay in the wire form even at zero: %s", zeroData)
	}
	if _, ok := zeroMap["messages"]; !ok {
		t.Errorf("messages must stay in the wire form: %s", zeroData)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCompleteAppliesSlotSampling verifies that slot-default sampling
// params fill in for zero fields on a ChatRequest, and that caller-set
// values are preserved. Uses a httptest server as a fake llama-server.
func TestCompleteAppliesSlotSampling(t *testing.T) {
	var last atomic.Value // stores map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		last.Store(parsed)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	// Parse the httptest port out of the URL so we can wire it into the
	// manager as if it were the active llama-server.
	u, _ := url.Parse(srv.URL)
	portNum, _ := strconv.Atoi(u.Port())

	m, err := New(Config{
		LlamaServerBin: "/bin/true",
		Models: map[Slot]ModelConfig{
			SlotPlanner: {
				Slot:      SlotPlanner,
				ModelPath: "/tmp/planner.gguf",
				Port:      portNum,
				Sampling: Sampling{
					Temperature: 0.42,
					TopK:        17,
					TopP:        0.81,
					MinP:        0.07,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Force the manager into a "slot active on httptest port" state without
	// spawning a real llama-server.
	m.mu.Lock()
	m.activeSlot = SlotPlanner
	m.port = portNum
	m.mu.Unlock()

	// Zero-valued sampling -> slot defaults apply.
	ctx := context.Background()
	if _, err := m.Complete(ctx, ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got := last.Load().(map[string]any)
	if got["temperature"].(float64) != 0.42 {
		t.Errorf("expected slot default temperature 0.42, got %v", got["temperature"])
	}
	if int(got["top_k"].(float64)) != 17 {
		t.Errorf("expected slot default top_k 17, got %v", got["top_k"])
	}
	if got["top_p"].(float64) != 0.81 {
		t.Errorf("expected slot default top_p 0.81, got %v", got["top_p"])
	}
	if got["min_p"].(float64) != 0.07 {
		t.Errorf("expected slot default min_p 0.07, got %v", got["min_p"])
	}

	// Caller-set values override slot defaults.
	if _, err := m.Complete(ctx, ChatRequest{
		Messages:    []ChatMessage{{Role: "user", Content: "hi"}},
		Temperature: 0.99,
		TopK:        5,
		TopP:        0.33,
		MinP:        0.22,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got = last.Load().(map[string]any)
	if got["temperature"].(float64) != 0.99 {
		t.Errorf("caller temperature not preserved: %v", got["temperature"])
	}
	if int(got["top_k"].(float64)) != 5 {
		t.Errorf("caller top_k not preserved: %v", got["top_k"])
	}
	if got["top_p"].(float64) != 0.33 {
		t.Errorf("caller top_p not preserved: %v", got["top_p"])
	}
	if got["min_p"].(float64) != 0.22 {
		t.Errorf("caller min_p not preserved: %v", got["min_p"])
	}
	// Stream must always be forced false on the wire (Complete() sets it).
	if got["stream"].(bool) != false {
		t.Errorf("stream should be coerced false, got %v", got["stream"])
	}
	// Messages must survive.
	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Errorf("messages not preserved: %v", got["messages"])
	}
}
