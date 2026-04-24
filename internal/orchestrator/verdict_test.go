package orchestrator

import (
	"testing"
	"time"
)

func TestLastVerdictDefaultsEmpty(t *testing.T) {
	o := &Orchestrator{}
	if v := o.LastVerdict(); v != "" {
		t.Errorf("expected empty verdict initially, got %q", v)
	}
}

func TestRecordAndReadVerdict(t *testing.T) {
	o := &Orchestrator{}
	o.setLastVerdict("approved")
	if v := o.LastVerdict(); v != "approved" {
		t.Errorf("expected approved, got %q", v)
	}
	// Verdicts are overwritten, not accumulated.
	o.setLastVerdict("revise")
	if v := o.LastVerdict(); v != "revise" {
		t.Errorf("expected revise, got %q", v)
	}
	_ = time.Now()
}
