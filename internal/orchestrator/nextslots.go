package orchestrator

import "github.com/aegis/agent-router/internal/taskstore"

// NextSlots predicts which slot(s) the orchestrator will need next given the
// active task's phase, the most recent review verdict, and whether there's
// an active task at all. See spec §5.4 for the 11-cell table.
func NextSlots(phase taskstore.Phase, verdict string, hasActive bool) []string {
	if !hasActive {
		return []string{}
	}
	switch phase {
	case taskstore.PhasePlan, taskstore.PhaseRevise, taskstore.PhaseCode:
		return []string{"reviewer"}
	case taskstore.PhasePlanReview:
		switch verdict {
		case "":
			return []string{"planner", "coder"}
		case "approved":
			return []string{"coder"}
		case "revise":
			return []string{"planner"}
		}
	case taskstore.PhaseCodeReview:
		switch verdict {
		case "":
			return []string{"planner", "coder"}
		case "approved":
			return []string{}
		case "fix":
			return []string{"coder"}
		case "replan":
			return []string{"planner"}
		}
	}
	return []string{}
}

// ReviewStage collapses Phase into the coarse-grained "plan" / "exec" label
// the UI cares about.
func ReviewStage(phase taskstore.Phase) string {
	switch phase {
	case taskstore.PhasePlanReview:
		return "plan"
	case taskstore.PhaseCodeReview:
		return "exec"
	}
	return ""
}
