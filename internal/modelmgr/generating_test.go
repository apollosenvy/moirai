package modelmgr

import (
	"testing"
)

func TestGeneratingDefaultsFalse(t *testing.T) {
	m, err := New(Config{
		LlamaServerBin: "/bin/true",
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/tmp/x.gguf", Port: 9000},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.IsGenerating() {
		t.Errorf("expected IsGenerating() to return false by default")
	}
}

func TestMarkGeneratingTransitions(t *testing.T) {
	m, _ := New(Config{
		LlamaServerBin: "/bin/true",
		Models: map[Slot]ModelConfig{
			SlotPlanner: {Slot: SlotPlanner, ModelPath: "/tmp/x.gguf", Port: 9000},
		},
	})
	done := m.markGenerating()
	if !m.IsGenerating() {
		t.Errorf("expected IsGenerating() true after markGenerating")
	}
	done()
	if m.IsGenerating() {
		t.Errorf("expected IsGenerating() false after done()")
	}
}
