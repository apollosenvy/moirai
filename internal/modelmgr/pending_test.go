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

func TestPendingClearsOnApply(t *testing.T) {
	m, _ := New(Config{
		LlamaServerBin: "/bin/true",
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/tmp/a.gguf", Port: 9000, CtxSize: 8192},
		},
	})
	m.SetPending(SlotPlanner, PendingChanges{CtxSize: 16384, KvCache: "turbo3"})
	if _, ok := m.GetPending(SlotPlanner); !ok {
		t.Fatal("expected pending set")
	}
	// Simulate natural transition -- the SlotsView should still show pending until
	// ApplyPending is called (EnsureSlot calls it when swapping away).
	views := m.SlotsView()
	var pv SlotView
	for _, v := range views {
		if v.Slot == SlotPlanner {
			pv = v
		}
	}
	if pv.PendingChanges == nil {
		t.Errorf("expected pending visible in SlotsView")
	}

	// Apply.
	applied := m.ApplyPending(SlotPlanner)
	if !applied {
		t.Fatal("expected ApplyPending to return true")
	}
	if _, ok := m.GetPending(SlotPlanner); ok {
		t.Errorf("pending not cleared after ApplyPending")
	}
	views = m.SlotsView()
	for _, v := range views {
		if v.Slot == SlotPlanner && v.PendingChanges != nil {
			t.Errorf("SlotsView still reports PendingChanges after apply")
		}
		if v.Slot == SlotPlanner && v.CtxSize != 16384 {
			t.Errorf("config not updated: got CtxSize=%d, want 16384", v.CtxSize)
		}
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
