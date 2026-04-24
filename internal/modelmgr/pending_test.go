package modelmgr

import "testing"

func TestSetPendingChanges(t *testing.T) {
	m, _ := New(Config{
		LlamaServerBin: "/bin/true",
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/tmp/a.gguf", Port: 9000, CtxSize: 8192},
		},
	})
	pending := PendingChanges{CtxSize: 16384}
	m.SetPending(SlotPlanner, pending)
	got, ok := m.GetPending(SlotPlanner)
	if !ok || got.CtxSize != 16384 {
		t.Errorf("expected pending CtxSize=16384, got %+v ok=%v", got, ok)
	}
}

func TestClearPendingChanges(t *testing.T) {
	m, _ := New(Config{
		LlamaServerBin: "/bin/true",
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/tmp/a.gguf", Port: 9000},
		},
	})
	m.SetPending(SlotPlanner, PendingChanges{CtxSize: 16384})
	m.ClearPending(SlotPlanner)
	_, ok := m.GetPending(SlotPlanner)
	if ok {
		t.Errorf("expected pending cleared, still present")
	}
}

func TestApplyPendingUpdatesConfig(t *testing.T) {
	m, _ := New(Config{
		LlamaServerBin: "/bin/true",
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/tmp/a.gguf", Port: 9000, CtxSize: 8192, KvCache: "f16"},
		},
	})
	m.SetPending(SlotPlanner, PendingChanges{CtxSize: 16384, KvCache: "turbo3"})
	applied := m.ApplyPending(SlotPlanner)
	if !applied {
		t.Fatalf("expected ApplyPending to return true")
	}
	cfg := m.cfg.Models[SlotPlanner]
	if cfg.CtxSize != 16384 || cfg.KvCache != "turbo3" {
		t.Errorf("expected config mutation, got %+v", cfg)
	}
	if _, ok := m.GetPending(SlotPlanner); ok {
		t.Errorf("expected pending cleared after apply")
	}
}
