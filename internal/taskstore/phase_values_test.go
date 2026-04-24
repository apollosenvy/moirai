package taskstore

import "testing"

// These string values are part of the JSON wire protocol. If you change them,
// update phos/ui/src/lib/phases.ts Phase type literals to match, or the UI
// next-slots derivation will mis-predict silently.
func TestPhaseWireValues(t *testing.T) {
	cases := map[Phase]string{
		PhaseInit:       "init",
		PhasePlan:       "planning",
		PhasePlanReview: "plan_review",
		PhaseCode:       "coding",
		PhaseCodeReview: "code_review",
		PhaseRevise:     "revise",
		PhaseDone:       "done",
	}
	for p, want := range cases {
		if string(p) != want {
			t.Errorf("Phase %q drifted: got %q, want %q. Update phos/ui/src/lib/phases.ts if intentional.", want, string(p), want)
		}
	}
}
