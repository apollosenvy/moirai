package modelmgr

import (
	"strings"
	"testing"
)

// TestBuildLlamaArgsDefaultsFitOff: every spawn should pass `-fit off`
// unless the operator explicitly opted into the auto-sizing probe via
// ExtraArgs. The default-on behavior in upstream llama-server crashes
// inside ggml_reshape_3d for gpt-oss-20b's openai_moe_iswa graph during
// the fit-time temporary llama_context construction (observed
// 2026-04-25 against llama-cpp-turboquant build 9041 / 7f320bb89). We
// already supply explicit -c and -ngl and have no use for the probe.
func TestBuildLlamaArgsDefaultsFitOff(t *testing.T) {
	args := buildLlamaArgs(ModelConfig{
		Slot:      SlotPlanner,
		ModelPath: "/tmp/x.gguf",
		Port:      19999,
	})
	want := flagPair("-fit", "off")
	if !flagPairPresent(args, want) {
		t.Fatalf("expected `-fit off` in default args, got: %s",
			strings.Join(args, " "))
	}
}

// TestBuildLlamaArgsHonorsExplicitFit: if the operator put `-fit on` in
// ExtraArgs they want the probe back. Don't override them; let
// llama-server's last-flag-wins parsing pick `-fit on`.
func TestBuildLlamaArgsHonorsExplicitFit(t *testing.T) {
	args := buildLlamaArgs(ModelConfig{
		Slot:      SlotPlanner,
		ModelPath: "/tmp/x.gguf",
		Port:      19999,
		ExtraArgs: []string{"-fit", "on"},
	})
	// The implementation skips the auto `-fit off` insertion when
	// ExtraArgs already supplies a -fit / --fit flag, so the args
	// should contain ONLY `-fit on`, not both.
	offCount := 0
	onCount := 0
	for i, a := range args {
		if (a == "-fit" || a == "--fit") && i+1 < len(args) {
			switch args[i+1] {
			case "off":
				offCount++
			case "on":
				onCount++
			}
		}
	}
	if offCount != 0 {
		t.Errorf("operator passed -fit on but auto-`-fit off` still injected: args=%s",
			strings.Join(args, " "))
	}
	if onCount != 1 {
		t.Errorf("operator passed -fit on but it was lost: args=%s",
			strings.Join(args, " "))
	}
}

// TestBuildLlamaArgsHonorsExplicitFitLong: same as above but operator
// uses --fit (long form). Operator preference is still honored.
func TestBuildLlamaArgsHonorsExplicitFitLong(t *testing.T) {
	args := buildLlamaArgs(ModelConfig{
		Slot:      SlotPlanner,
		ModelPath: "/tmp/x.gguf",
		Port:      19999,
		ExtraArgs: []string{"--fit", "on"},
	})
	for i, a := range args {
		if a == "-fit" && i+1 < len(args) && args[i+1] == "off" {
			t.Errorf("operator passed --fit on but auto-`-fit off` still injected: args=%s",
				strings.Join(args, " "))
			return
		}
	}
}

// flagPair builds a 2-element subslice [flag, value] for searching.
func flagPair(flag, value string) [2]string {
	return [2]string{flag, value}
}

// flagPairPresent returns true if `args` contains the given [flag, value]
// adjacency.
func flagPairPresent(args []string, want [2]string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == want[0] && args[i+1] == want[1] {
			return true
		}
	}
	return false
}
