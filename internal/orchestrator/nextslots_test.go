package orchestrator

import (
	"testing"

	"github.com/aegis/agent-router/internal/taskstore"
)

func TestNextSlotsTable(t *testing.T) {
	cases := []struct {
		name    string
		phase   taskstore.Phase
		verdict string
		want    []string
	}{
		{"no active task", "", "", []string{}},
		{"write -> reviewer", taskstore.PhasePlan, "", []string{"reviewer"}},
		{"revise -> reviewer", taskstore.PhaseRevise, "", []string{"reviewer"}},
		{"execute -> reviewer", taskstore.PhaseCode, "", []string{"reviewer"}},
		{"plan_review no verdict -> both", taskstore.PhasePlanReview, "", []string{"planner", "coder"}},
		{"plan_review approved -> coder", taskstore.PhasePlanReview, "approved", []string{"coder"}},
		{"plan_review revise -> planner", taskstore.PhasePlanReview, "revise", []string{"planner"}},
		{"code_review no verdict -> both", taskstore.PhaseCodeReview, "", []string{"planner", "coder"}},
		{"code_review approved -> done", taskstore.PhaseCodeReview, "approved", []string{}},
		{"code_review fix -> coder", taskstore.PhaseCodeReview, "fix", []string{"coder"}},
		{"code_review replan -> planner", taskstore.PhaseCodeReview, "replan", []string{"planner"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NextSlots(c.phase, c.verdict, c.phase != "")
			if len(got) != len(c.want) {
				t.Fatalf("len mismatch: got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("[%d] got %q want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestReviewStage(t *testing.T) {
	if ReviewStage(taskstore.PhasePlanReview) != "plan" {
		t.Errorf("plan_review should produce 'plan'")
	}
	if ReviewStage(taskstore.PhaseCodeReview) != "exec" {
		t.Errorf("code_review should produce 'exec'")
	}
	if ReviewStage(taskstore.PhasePlan) != "" {
		t.Errorf("non-review phase should produce ''")
	}
}
