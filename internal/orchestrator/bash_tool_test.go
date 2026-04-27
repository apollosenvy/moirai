package orchestrator

import (
	"strings"
	"testing"
)

// TestIsKnownToolNameAcceptsBash pins the parser whitelist: a model emitting
// <TOOL>{"name":"bash","args":...}</TOOL> in bash-only mode must reach the
// dispatcher. Without this entry the parser silently rejects every bash
// envelope and the whole bash-only mode falls through to "model emitted
// nothing parseable", which spins the reviewer in a no-op turn loop.
func TestIsKnownToolNameAcceptsBash(t *testing.T) {
	if !isKnownToolName("bash") {
		t.Error("isKnownToolName(\"bash\") = false, want true (bash-only mode)")
	}
	// Negative: a typo'd name still falls through. Without this the test
	// would pass even if isKnownToolName accepted everything.
	if isKnownToolName("bash.run") || isKnownToolName("Bash") || isKnownToolName("shell") {
		t.Error("isKnownToolName accepted a non-bash typo")
	}
}

// TestConfigBashOnlyHelpers pins the Config.IsBashOnly / BashFenced contract
// the prompt selector and dispatcher both depend on. If the spelling of the
// config value drifts (e.g. "bash_only" vs "bash-only") the helpers still
// agree; the dispatcher and prompts can't disagree about which mode is on.
func TestConfigBashOnlyHelpers(t *testing.T) {
	cases := []struct {
		surface string
		emit    string
		bashy   bool
		fenced  bool
	}{
		{"", "", false, false},                 // legacy default
		{"legacy", "", false, false},           // explicit legacy
		{"bash-only", "", true, false},         // bash + tool-call default
		{"bash-only", "toolcall", true, false}, // bash + explicit toolcall
		{"bash-only", "fenced", true, true},    // bash + fenced
		{"bash-only", "weird", true, false},    // unknown emit -> not fenced
		{"BashOnly", "", false, false},         // wrong spelling -> legacy
	}
	for _, tc := range cases {
		c := Config{ToolSurface: tc.surface, BashEmitMode: tc.emit}
		if got := c.IsBashOnly(); got != tc.bashy {
			t.Errorf("IsBashOnly(surface=%q) = %v, want %v", tc.surface, got, tc.bashy)
		}
		if got := c.BashFenced(); got != tc.fenced {
			t.Errorf("BashFenced(surface=%q,emit=%q) = %v, want %v", tc.surface, tc.emit, got, tc.fenced)
		}
	}
}

// TestExtractBashFenceBlocksMatchesBashFamily pins the language-tag filter:
// ```bash, ```sh, ```shell (case-insensitive) all qualify; other tags do
// not. A reply with mixed fence types must yield only the bash bodies.
func TestExtractBashFenceBlocksMatchesBashFamily(t *testing.T) {
	reply := "Here is the plan.\n\n" +
		"```bash\necho hello\n```\n\n" +
		"```python\nprint('not bash')\n```\n\n" +
		"```sh\nls -la\n```\n\n" +
		"```SHELL\npwd\n```\n\n" +
		"```\nno tag should not match\n```\n"
	got := extractBashFenceBlocks(reply)
	want := []string{"echo hello", "ls -la", "pwd"}
	if len(got) != len(want) {
		t.Fatalf("extracted %d blocks, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestExtractBashFenceBlocksHandlesHeredocBody pins the heredoc-survives-
// extraction contract: a fence body containing a heredoc with internal
// newlines and quotes must come out byte-for-byte. Critical because the
// bash-only mode's whole file-write path goes through `cat <<EOF` heredocs.
func TestExtractBashFenceBlocksHandlesHeredocBody(t *testing.T) {
	body := "cat > docs/plans/PLAN.md <<'EOF'\n# Plan\n\nLine 1\n```inline fence```\nLine 3\nEOF"
	reply := "````bash\n" + body + "\n````\n"
	got := extractBashFenceBlocks(reply)
	if len(got) != 1 {
		t.Fatalf("expected 1 block, got %d: %v", len(got), got)
	}
	if got[0] != body {
		t.Errorf("body mismatch:\n got: %q\nwant: %q", got[0], body)
	}
}

// TestSynthesizeBashToolCallReturnsBashName pins the synthesized toolCall
// shape so the dispatcher's "case bash" branch can run the result without
// any special-casing.
func TestSynthesizeBashToolCallReturnsBashName(t *testing.T) {
	reply := "```bash\necho one\n```\n"
	tc, ok := synthesizeBashToolCallFromFences(reply)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if tc.Name != "bash" {
		t.Errorf("synthesized Name = %q, want %q", tc.Name, "bash")
	}
	cmd, _ := tc.Args["command"].(string)
	if cmd != "echo one" {
		t.Errorf("synthesized command = %q, want %q", cmd, "echo one")
	}
}

// TestSynthesizeBashToolCallJoinsMultipleFences pins the multi-fence
// concatenation contract: separate fences become newline-separated bash
// statements, with comment markers so the trace can see fence boundaries.
func TestSynthesizeBashToolCallJoinsMultipleFences(t *testing.T) {
	reply := "First do this:\n\n```bash\necho A\n```\n\nThen this:\n\n```bash\necho B\n```\n"
	tc, ok := synthesizeBashToolCallFromFences(reply)
	if !ok {
		t.Fatal("expected ok=true")
	}
	cmd, _ := tc.Args["command"].(string)
	if !strings.Contains(cmd, "echo A") || !strings.Contains(cmd, "echo B") {
		t.Errorf("missing one of the fence bodies: %q", cmd)
	}
	if !strings.Contains(cmd, "# --- fence 1/2 ---") || !strings.Contains(cmd, "# --- fence 2/2 ---") {
		t.Errorf("missing fence boundary markers: %q", cmd)
	}
}

// TestSynthesizeBashToolCallNoFencesReturnsFalse pins the negative path: a
// reply with zero bash fences must NOT synthesize a tool call. The
// orchestrator's parser path falls back to the standard <TOOL> envelope
// extractor in that case.
func TestSynthesizeBashToolCallNoFencesReturnsFalse(t *testing.T) {
	reply := "I am thinking about this. Let me try ```python\nx=1\n```"
	if _, ok := synthesizeBashToolCallFromFences(reply); ok {
		t.Error("expected ok=false for reply with no bash fence")
	}
}

// TestDuplicateBashCountMatchesExactHash pins the loop-detector keying:
// only commands with EXACTLY the same content-hash count as duplicates.
// A model legitimately iterating (different sed expression, different
// heredoc body) should NOT trip the detector. Closes the v3.3 cmd/root.go
// failure shape (Gemma rewrote the same buggy file 8 times) without
// false-positive on healthy iteration.
func TestDuplicateBashCountMatchesExactHash(t *testing.T) {
	h1 := contentHash("cat foo.go")
	h2 := contentHash("cat foo.go") // identical input, identical hash
	h3 := contentHash("cat -n foo.go")
	if h1 != h2 {
		t.Fatal("contentHash should be deterministic on identical input")
	}
	if h1 == h3 {
		t.Fatal("different inputs should produce different hashes")
	}
	hist := []bashCmdRecord{
		{Command: "cat foo.go", ContentHash: h1},
		{Command: "cat foo.go", ContentHash: h1},
		{Command: "cat -n foo.go", ContentHash: h3},
		{Command: "cat foo.go", ContentHash: h1},
	}
	if got := duplicateBashCount(hist, h1); got != 3 {
		t.Errorf("duplicateBashCount(h1) = %d, want 3", got)
	}
	if got := duplicateBashCount(hist, h3); got != 1 {
		t.Errorf("duplicateBashCount(h3) = %d, want 1", got)
	}
	// Negative: a fresh hash should match nothing.
	if got := duplicateBashCount(hist, contentHash("ls")); got != 0 {
		t.Errorf("duplicateBashCount(unknown) = %d, want 0", got)
	}
}

// TestBashRepeatCapIsThree pins the calibrated value so a refactor that
// drops it to 1 (over-aggressive) or raises it to 10 (under-aggressive)
// surfaces in CI rather than in the next stuck v3 run. 3 is the value
// chosen against the v3.3 cmd/root.go incident.
func TestBashRepeatCapIsThree(t *testing.T) {
	if bashRepeatCap != 3 {
		t.Errorf("bashRepeatCap = %d, want 3 (v3.3 calibration)", bashRepeatCap)
	}
}

// TestExtractBashWriteTargetCommonPatterns pins the heuristic against the
// shapes the v3.x runs actually produced. cat>, cat>>, tee, sed-i.
func TestExtractBashWriteTargetCommonPatterns(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"cat > main.go <<'EOF'\npackage main\nEOF", "main.go"},
		{"cat >> docs/review/REVIEW.md <<'EOF'\n## Turn 2\nEOF", "docs/review/REVIEW.md"},
		{"cat > cmd/root.go << 'EOF'\npackage cmd\nEOF", "cmd/root.go"},
		{"tee output.txt <<<hello", "output.txt"},
		{"tee -a log.txt", "log.txt"},
		{"sed -i 's/foo/bar/g' main.go", "main.go"},
		// Negative: no write, no target.
		{"cat main.go", ""},
		{"go build ./...", ""},
		{"grep -rn pattern .", ""},
		{"ls -R", ""},
	}
	for _, tc := range cases {
		got := extractBashWriteTarget(tc.cmd)
		if got != tc.want {
			t.Errorf("extractBashWriteTarget(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

// TestConsecutiveBashWritesToTarget pins the streak counter the
// path-target detector uses. It must STOP at the first non-matching
// invocation, not count occurrences scattered through the history.
func TestConsecutiveBashWritesToTarget(t *testing.T) {
	hist := []bashCmdRecord{
		{Command: "cat > foo.go <<EOF\nx\nEOF"},
		{Command: "cat > foo.go <<EOF\ny\nEOF"},
		{Command: "go build ./..."}, // BREAKS the streak
		{Command: "cat > foo.go <<EOF\nz\nEOF"},
		{Command: "cat > foo.go <<EOF\nw\nEOF"},
		{Command: "cat > foo.go <<EOF\nv\nEOF"},
	}
	// Last 3 records all write to foo.go. The `go build` breaks the streak,
	// so consecutive should be 3 not 5.
	if got := consecutiveBashWritesToTarget(hist, "foo.go"); got != 3 {
		t.Errorf("consecutiveBashWritesToTarget = %d, want 3", got)
	}
	// A different target -> 0.
	if got := consecutiveBashWritesToTarget(hist, "bar.go"); got != 0 {
		t.Errorf("different target should yield 0, got %d", got)
	}
	// Empty target -> 0 (defensive).
	if got := consecutiveBashWritesToTarget(hist, ""); got != 0 {
		t.Errorf("empty target should yield 0, got %d", got)
	}
}

// TestBashSameTargetCapIsFour pins the v3.4 calibration value.
func TestBashSameTargetCapIsFour(t *testing.T) {
	if bashSameTargetCap != 4 {
		t.Errorf("bashSameTargetCap = %d, want 4 (v3.4 calibration)", bashSameTargetCap)
	}
}

// TestBashCmdHistoryRingCap pins the runState.bashCmdHistory bound: a
// runaway model that emits 200 bash invocations must NOT grow runState
// unboundedly. The ring is capped at bashCmdHistoryLen entries.
func TestBashCmdHistoryRingCap(t *testing.T) {
	// Direct simulation of the appendring logic from executeROTool's
	// "case bash" branch. We replicate it here rather than spawning the
	// full dispatcher to keep the unit test hermetic.
	st := &runState{}
	for i := 0; i < bashCmdHistoryLen*3; i++ {
		st.bashCmdHistory = append(st.bashCmdHistory, bashCmdRecord{Command: "echo n"})
		if len(st.bashCmdHistory) > bashCmdHistoryLen {
			st.bashCmdHistory = st.bashCmdHistory[len(st.bashCmdHistory)-bashCmdHistoryLen:]
		}
	}
	if got := len(st.bashCmdHistory); got != bashCmdHistoryLen {
		t.Errorf("bashCmdHistory length after 3x cap appends = %d, want %d", got, bashCmdHistoryLen)
	}
}
