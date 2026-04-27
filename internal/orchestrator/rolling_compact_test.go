package orchestrator

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aegis/moirai/internal/modelmgr"
)

// makeAskCoderResultMsg builds a fake user-role message looking like an
// ask_coder result. The result_id is just to make the contents distinguishable
// across messages so we can assert which ones got rewritten.
func makeAskCoderResultMsg(resultID, fileName string, prosePadding int) modelmgr.ChatMessage {
	body := fmt.Sprintf("AUTO-COMMITTED 1 file(s) from coder reply:\n  - %s (1234 bytes, new)\n\n```typescript\n# file: %s\n%s\n```",
		fileName, fileName, strings.Repeat("// padding line\n", prosePadding))
	return modelmgr.ChatMessage{
		Role:    "user",
		Content: fmt.Sprintf("<RESULT>%s</RESULT>", body) + " /*tag:" + resultID + "*/",
	}
}

// TestRollingWindowFitsRematch17 pins rollingWindowAskCoder=4 against the
// historical rematch #17 ctx-overflow shape. The reviewer hit the 32K
// context wall at turn 19 with 17 accumulated ask_coder results. With
// rolling-window compaction at the current value, the same 17-message
// trajectory must compact under a generous 32K * 80% headroom budget --
// otherwise the cap is too high (window too large -> compaction is
// insufficient) and we'll repeat rematch #17.
//
// Test setup mirrors the observed rematch #17 sizes: each ask_coder
// result was on the order of 2KB (a few hundred bytes of AUTO-COMMITTED
// header + ~1.5KB of code prose padding). 17 of them at full size sum
// to ~34KB -- WELL over the 32K reviewer ctx. Compaction must reduce
// this to fit a healthy headroom for the prompt + checklist + tools.
func TestRollingWindowFitsRematch17(t *testing.T) {
	const (
		rematch17AskCoderCount = 17    // observed in trace
		rematch17AskCoderBytes = 2048  // mean per-result size
		ctxBudgetReviewer      = 32768 // gemma-4 / qwen reviewer ctx
		// 80% of ctx leaves room for the system prompt (~6KB), the
		// checklist (~4KB), tool envelope, and ongoing prose. The
		// ask_coder block alone shouldn't claim more than this.
		askCoderBudget = ctxBudgetReviewer * 80 / 100
	)

	// Build 17 ask_coder result messages. The padding count maps
	// roughly to historical sizes (~16 bytes per padding line + ~200
	// header bytes -> ~2KB total).
	var messages []modelmgr.ChatMessage
	for i := 0; i < rematch17AskCoderCount; i++ {
		// 110 padding lines @ ~16 bytes each ~= 1760 bytes plus header
		messages = append(messages, makeAskCoderResultMsg(
			fmt.Sprintf("r%d", i),
			fmt.Sprintf("apps/web/src/component%d.ts", i),
			110,
		))
	}

	// Pre-compaction sanity: the historical shape DOES exceed the budget.
	preTotal := 0
	for _, m := range messages {
		preTotal += len(m.Content)
	}
	if preTotal < ctxBudgetReviewer {
		t.Skipf("synthetic ask_coder messages too small (%d) to reproduce rematch #17 -- bump the padding constant", preTotal)
	}
	t.Logf("pre-compaction total: %d bytes (%.0f%% of %d ctx)",
		preTotal, 100.0*float64(preTotal)/ctxBudgetReviewer, ctxBudgetReviewer)

	reclaimed := compactStaleAskCoderResults(messages, rollingWindowAskCoder)
	if reclaimed <= 0 {
		t.Fatalf("compactor reclaimed 0 bytes from %d ask_coder messages -- regression on rollingWindowAskCoder",
			rematch17AskCoderCount)
	}

	postTotal := preTotal - reclaimed
	t.Logf("post-compaction total: %d bytes (%.0f%% of %d ctx); reclaimed %d",
		postTotal, 100.0*float64(postTotal)/ctxBudgetReviewer, ctxBudgetReviewer, reclaimed)

	if postTotal > askCoderBudget {
		t.Errorf("post-compaction ask_coder total (%d) exceeds budget (%d, 80%% of %d ctx). "+
			"rollingWindowAskCoder=%d is too large -- this trajectory will reproduce rematch #17",
			postTotal, askCoderBudget, ctxBudgetReviewer, rollingWindowAskCoder)
	}
}

func TestCompactStaleAskCoderResults_NoOpUnderWindow(t *testing.T) {
	// 3 ask_coder results, window of 4. None should be touched.
	messages := []modelmgr.ChatMessage{
		{Role: "system", Content: "you are reviewer"},
		{Role: "user", Content: "task description"},
		makeAskCoderResultMsg("a", "src/a.ts", 50),
		{Role: "assistant", Content: "next tool call"},
		makeAskCoderResultMsg("b", "src/b.ts", 50),
		{Role: "assistant", Content: "next tool call"},
		makeAskCoderResultMsg("c", "src/c.ts", 50),
	}
	preLens := make([]int, len(messages))
	for i, m := range messages {
		preLens[i] = len(m.Content)
	}
	reclaimed := compactStaleAskCoderResults(messages, 4)
	if reclaimed != 0 {
		t.Errorf("expected 0 reclaimed bytes, got %d", reclaimed)
	}
	for i, m := range messages {
		if len(m.Content) != preLens[i] {
			t.Errorf("message %d unexpectedly changed size: %d -> %d", i, preLens[i], len(m.Content))
		}
	}
}

func TestCompactStaleAskCoderResults_KeepsLastN(t *testing.T) {
	// 6 ask_coder results, window 3. Oldest 3 should be stubbed.
	var messages []modelmgr.ChatMessage
	messages = append(messages, modelmgr.ChatMessage{Role: "system", Content: "system"})
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("r%d", i)
		messages = append(messages, makeAskCoderResultMsg(id, fmt.Sprintf("src/file%d.ts", i), 60))
		messages = append(messages, modelmgr.ChatMessage{Role: "assistant", Content: "next"})
	}
	preLens := make([]int, len(messages))
	for i, m := range messages {
		preLens[i] = len(m.Content)
	}
	reclaimed := compactStaleAskCoderResults(messages, 3)
	if reclaimed <= 0 {
		t.Fatalf("expected >0 reclaimed bytes, got %d", reclaimed)
	}
	// Find the ask_coder result indices in the (mutated) messages array
	// and verify which ones got shortened.
	resultIdxs := []int{}
	for i, m := range messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "<RESULT>") {
			resultIdxs = append(resultIdxs, i)
		}
	}
	if len(resultIdxs) != 6 {
		t.Fatalf("expected to find 6 RESULT messages, found %d", len(resultIdxs))
	}
	stale := resultIdxs[:3]
	fresh := resultIdxs[3:]
	for _, i := range stale {
		if len(messages[i].Content) >= preLens[i] {
			t.Errorf("stale result at %d should have shrunk: %d -> %d", i, preLens[i], len(messages[i].Content))
		}
		if !strings.Contains(messages[i].Content, "[stale ask_coder") {
			t.Errorf("stale result at %d should carry stub marker: %q", i, messages[i].Content)
		}
		// AUTO-COMMITTED summary should still be visible in stub.
		if !strings.Contains(messages[i].Content, "AUTO-COMMITTED") {
			t.Errorf("stub at %d lost AUTO-COMMITTED summary: %q", i, messages[i].Content)
		}
	}
	for _, i := range fresh {
		if len(messages[i].Content) != preLens[i] {
			t.Errorf("fresh result at %d unexpectedly changed: %d -> %d", i, preLens[i], len(messages[i].Content))
		}
	}
}

func TestCompactStaleAskCoderResults_Idempotent(t *testing.T) {
	// Second call with same window should reclaim 0 because already stubbed.
	var messages []modelmgr.ChatMessage
	for i := 0; i < 6; i++ {
		messages = append(messages, makeAskCoderResultMsg(fmt.Sprintf("r%d", i), fmt.Sprintf("f%d.ts", i), 60))
	}
	first := compactStaleAskCoderResults(messages, 2)
	if first <= 0 {
		t.Fatalf("first pass expected >0 reclaimed, got %d", first)
	}
	second := compactStaleAskCoderResults(messages, 2)
	if second != 0 {
		t.Errorf("second pass should reclaim 0, got %d", second)
	}
}

func TestCompactStaleAskCoderResults_IgnoresNonAskCoder(t *testing.T) {
	// Result messages WITHOUT AUTO-COMMITTED or # file: should not be
	// classified as ask_coder. They should NOT be stubbed even if they
	// are old.
	messages := []modelmgr.ChatMessage{
		{Role: "user", Content: "<RESULT>(test.run output): tests passed</RESULT>"},
		{Role: "user", Content: "<RESULT>(fs.read output): file contents here</RESULT>"},
		makeAskCoderResultMsg("a", "src/a.ts", 60),
		makeAskCoderResultMsg("b", "src/b.ts", 60),
		makeAskCoderResultMsg("c", "src/c.ts", 60),
	}
	preLens := make([]int, len(messages))
	for i, m := range messages {
		preLens[i] = len(m.Content)
	}
	// Window 2 -- only the very oldest ask_coder should be stubbed (the
	// non-coder RESULTs at indexes 0, 1 are NOT counted).
	reclaimed := compactStaleAskCoderResults(messages, 2)
	if reclaimed <= 0 {
		t.Fatalf("expected reclaim, got %d", reclaimed)
	}
	// Indices 0, 1 should be unchanged.
	if len(messages[0].Content) != preLens[0] || len(messages[1].Content) != preLens[1] {
		t.Errorf("non-ask_coder results should not be stubbed")
	}
	// Index 2 (oldest ask_coder) should be stubbed.
	if len(messages[2].Content) >= preLens[2] {
		t.Errorf("oldest ask_coder should be stubbed: was %d now %d", preLens[2], len(messages[2].Content))
	}
	// Indices 3, 4 (recent ask_coder) should be unchanged.
	if len(messages[3].Content) != preLens[3] || len(messages[4].Content) != preLens[4] {
		t.Errorf("recent ask_coder results should be untouched")
	}
}

// TestCompactStaleAskCoderResults_DoesNotDestroyFsReadWithFileMarker
// pins audit-pass-3's CRITICAL finding (P3-CRIT-1): the previous heuristic
// (substring "AUTO-COMMITTED" or "# file:") false-positived on fs.read
// results that returned content containing those substrings (e.g.
// reading PLAN.md or a Markdown file with `# file:` headings). The
// compactor would silently destroy the fs.read content. The post-fix
// heuristic requires a strict prefix `<RESULT>AUTO-COMMITTED ` and
// must NOT touch other RESULT-wrapped messages.
func TestCompactStaleAskCoderResults_DoesNotDestroyFsReadWithFileMarker(t *testing.T) {
	// Build a conversation: 5 real ask_coder results (with AUTO-COMMITTED
	// prefix), plus 2 fs.read results whose CONTENT contains "# file:" or
	// "AUTO-COMMITTED" as substrings. The fs.read results must NOT be
	// stubbed even though their content contains the old heuristic
	// substrings.
	var messages []modelmgr.ChatMessage
	for i := 0; i < 5; i++ {
		messages = append(messages, makeAskCoderResultMsg(fmt.Sprintf("a%d", i), fmt.Sprintf("src/f%d.ts", i), 60))
	}
	// fs.read result that mentions # file: in its body content.
	fsReadEvil := modelmgr.ChatMessage{
		Role:    "user",
		Content: "<RESULT>(fs.read of PLAN.md):\n# Project Plan\n\n# file: package.json\n```json\n{}\n```\n</RESULT>",
	}
	messages = append(messages, fsReadEvil)
	// fs.read result that mentions AUTO-COMMITTED in its body
	// (legitimate -- maybe a previous turn's auto-commit summary captured
	// in a log file the reviewer fs.read'd).
	fsReadEvil2 := modelmgr.ChatMessage{
		Role:    "user",
		Content: "<RESULT>(fs.read of /tmp/log.txt):\nlast turn AUTO-COMMITTED 5 files; debugging\n</RESULT>",
	}
	messages = append(messages, fsReadEvil2)

	preEvil1 := messages[5].Content
	preEvil2 := messages[6].Content

	reclaimed := compactStaleAskCoderResults(messages, 2)
	if reclaimed <= 0 {
		t.Fatalf("expected >0 reclaimed bytes from 5 ask_coder results with window 2, got %d", reclaimed)
	}

	// Critical: fs.read results must NOT have been mutated.
	if messages[5].Content != preEvil1 {
		t.Errorf("fs.read result with embedded '# file:' was destroyed: now=%q", messages[5].Content)
	}
	if messages[6].Content != preEvil2 {
		t.Errorf("fs.read result with embedded 'AUTO-COMMITTED' was destroyed: now=%q", messages[6].Content)
	}
}

func TestStubFromAskCoderResult_PreservesAutoCommitted(t *testing.T) {
	original := "<RESULT>AUTO-COMMITTED 2 file(s) from coder reply:\n  - src/a.ts (123 bytes, new)\n  - src/b.ts (456 bytes, new)\n\n```typescript\n# file: src/a.ts\nconsole.log('a');\n```\n```typescript\n# file: src/b.ts\nconsole.log('b');\n```\nThe coder also wrote a long prose explanation that we don't need to keep verbatim.</RESULT>"
	stub := stubFromAskCoderResult(original)
	if !strings.Contains(stub, "[stale ask_coder result") {
		t.Errorf("stub missing marker: %q", stub)
	}
	if !strings.Contains(stub, "AUTO-COMMITTED") || !strings.Contains(stub, "src/a.ts") || !strings.Contains(stub, "src/b.ts") {
		t.Errorf("stub lost the AUTO-COMMITTED file list: %q", stub)
	}
	if len(stub) >= len(original) {
		t.Errorf("stub should be shorter: orig=%d stub=%d", len(original), len(stub))
	}
}

func TestStubFromAskCoderResult_NoSummaryFallback(t *testing.T) {
	// A result that has `# file:` but no AUTO-COMMITTED prefix (extract
	// failed but fingerprint still matches).
	original := "<RESULT>```typescript\n# file: src/q.ts\nfunction q(){return 1}\n```</RESULT>"
	stub := stubFromAskCoderResult(original)
	if !strings.Contains(stub, "[stale ask_coder result") {
		t.Errorf("stub missing marker: %q", stub)
	}
	// We did NOT invent a fake summary -- there is no "AUTO-COMMITTED N file(s)"
	// pattern in the stub. The phrase "no AUTO-COMMITTED block" is fine; that's
	// the fallback diagnostic.
	if strings.Contains(stub, "AUTO-COMMITTED 1 file") || strings.Contains(stub, "AUTO-COMMITTED 2 file") {
		t.Errorf("stub should not invent a summary that wasn't in the original: %q", stub)
	}
	// Stub should be markedly shorter than original.
	if len(stub) >= len(original)*2 {
		t.Errorf("fallback stub is suspiciously long: orig=%d stub=%d", len(original), len(stub))
	}
}

func TestStubFromAskCoderResult_AlreadyStubbed(t *testing.T) {
	stubbed := "<RESULT>[stale ask_coder result, 1234 bytes summarized]\nAUTO-COMMITTED 1 file(s)...</RESULT>"
	out := stubFromAskCoderResult(stubbed)
	if out != stubbed {
		t.Errorf("already-stubbed message should be returned unchanged; got %q", out)
	}
}
