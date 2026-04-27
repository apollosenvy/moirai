// Package plan parses structured plans the planner emits at the end of its
// reply, and tracks live progress against them. The orchestrator uses Plan
// to render a <CHECKLIST> block injected into every reviewer turn, and to
// gate the done() tool on all acceptance items being satisfied.
//
// Design intent: the reviewer's hardest job is "what's left to do?" Holding
// that state in the conversation context fails after about 8K tokens. Move
// the bookkeeping into runState; render it deterministically every turn.
//
// Why not parse Markdown PLAN.md instead of JSON? Markdown is fragile for
// machine consumption -- model emits "Phase 1:" vs "## Phase 1" vs "1." vs
// random reformats per turn. JSON in a fenced code block is unambiguous and
// the planner is reliable enough to produce it (Qwen3.5-27B Opus-distill
// already produces clean structured output; demanding JSON is no extra
// burden).
package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Plan is the structured roadmap a planner emits for a task. The orchestrator
// stores one Plan per runState. Files in Phases get ticked off as fs.write
// succeeds; AcceptanceItems get ticked off when the orchestrator can show
// objective evidence (test.run passed, compile.run passed, file presence,
// etc.).
type Plan struct {
	// Phases group related files into logical build chunks (e.g. "scaffold",
	// "data model", "API", "UI"). The reviewer is encouraged but not
	// required to complete phases in order.
	Phases []Phase `json:"phases"`

	// Acceptance is the checklist of objective criteria a task must
	// satisfy before done() is allowed. Each item should be testable
	// (a file exists, a test passes, an endpoint responds 200).
	Acceptance []AcceptanceItem `json:"acceptance"`
}

// Phase is a named group of files the planner expects the coder to produce
// together. Phases are display-only; nothing in the orchestrator enforces
// phase order.
type Phase struct {
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Files []FileSpec `json:"files"`
}

// FileSpec names a single file the plan expects to exist on disk by task
// completion. Path is repo-relative. Satisfied is set true by the
// orchestrator's MarkFileWritten when a successful fs.write lands at that
// path.
type FileSpec struct {
	Path      string `json:"path"`
	Purpose   string `json:"purpose,omitempty"`
	Satisfied bool   `json:"-"` // not serialized; live state
}

// AcceptanceItem is a checklist criterion that gates done(). Description is
// human-readable; Verify is an optional structured matcher the orchestrator
// can run automatically. If Verify is empty, the item must be ticked off
// manually by a tool result the orchestrator's matcher recognizes.
type AcceptanceItem struct {
	ID          string `json:"id"`
	Description string `json:"description"`

	// Verify (optional) tells the orchestrator how to auto-tick this
	// item. Supported forms:
	//   "file:<path>"       -> satisfied when fs.write lands at <path>
	//   "test.run:pass"     -> satisfied when test.run exits 0 and stdout
	//                          contains real test output (not the
	//                          "no tests found" pattern)
	//   "compile.run:pass"  -> satisfied when compile.run exits 0
	// Empty Verify means the criterion is informational only; the
	// reviewer must claim it via a future "claim_acceptance" tool call.
	Verify string `json:"verify,omitempty"`

	Satisfied bool `json:"-"`
}

// jsonBlockRE matches a fenced code block tagged json (or an unfenced JSON
// block at the end of a reply). We accept both because reasoning models
// sometimes drop the fence.
var jsonBlockRE = regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(\\{.*?\\})\\s*```")

// Parse extracts a Plan from a planner reply. The reply may contain prose
// before the JSON; only the LAST balanced JSON object is parsed. Returns
// (nil, nil) if no JSON-looking block is present (caller can decide whether
// to retry); returns (nil, err) if a JSON block is present but malformed.
func Parse(reply string) (*Plan, error) {
	candidates := jsonBlockRE.FindAllStringSubmatch(reply, -1)
	var raw string
	if len(candidates) > 0 {
		// Use the last matched block. Earlier blocks are often examples.
		raw = candidates[len(candidates)-1][1]
	} else {
		// No fence: try to find a balanced JSON object at the tail.
		raw = lastBalancedObject(reply)
		if raw == "" {
			return nil, nil
		}
	}
	var p Plan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("plan: json unmarshal: %w", err)
	}
	if len(p.Phases) == 0 && len(p.Acceptance) == 0 {
		return nil, fmt.Errorf("plan: parsed JSON has neither phases nor acceptance")
	}
	// Strip out FileSpec entries with malformed paths so the rendered
	// checklist and the matcher only ever see usable values. Same rules
	// applied to acceptance items with verify="file:..." (the trimmed
	// path must look like a relative repo path, not absolute, traversal,
	// empty, or control-char-laden). Closes adversarial findings ADV-01,
	// ADV-02, ADV-03, ADV-06: an empty/absolute/traversal verify or path
	// could either bypass the done() gate (if used as verify="file:")
	// or corrupt the rendered checklist (if rendered with literal \n).
	for pi := range p.Phases {
		filtered := p.Phases[pi].Files[:0]
		for _, f := range p.Phases[pi].Files {
			if validPlanPath(f.Path) {
				filtered = append(filtered, f)
			}
		}
		p.Phases[pi].Files = filtered
	}
	// Drop duplicate Acceptance IDs at Parse time. A planner emitting two
	// items with the same ID makes MarkAcceptanceByID ambiguous (which
	// one to tick?) and the rendered checklist confusing. Closes audit-
	// pass-1 ADV-13. We keep the FIRST occurrence and drop subsequent
	// duplicates -- the planner's first listing is presumed canonical.
	seenAccID := make(map[string]bool, len(p.Acceptance))
	// Track verify strings so we can reject duplicates: a planner emitting
	// two items with verify="bash:go test ./...:pass" lets one passing
	// invocation tick BOTH at once, the v2 verify-vocabulary inflation
	// finding. The bash:<exact cmd>:pass shape forces per-item granularity
	// in language; this filter forces it in code. Same logic for legacy
	// shapes (test.run:pass, compile.run:pass): if two items share the
	// same verify, only the first survives. The model gets feedback via
	// the parse error if EVERY item collapses, or via fewer-than-emitted
	// acceptance items if just some did -- the renderChecklist progress
	// counter will surface the discrepancy.
	//
	// "file:<path>" verify shapes are NOT deduped because two acceptance
	// items legitimately may both reference the same file (one for
	// presence, one for content shape -- the second would carry a
	// different verify shape, but if a planner emits two file:foo.go
	// items, that's a typo we accept silently rather than rejecting the
	// whole plan; the suffix-uniqueness matcher and the user-facing
	// checklist render absorb the redundancy).
	seenVerify := make(map[string]bool, len(p.Acceptance))
	filteredAcc := p.Acceptance[:0]
	for _, a := range p.Acceptance {
		if !validAcceptanceVerify(a.Verify) {
			continue
		}
		if a.ID != "" && seenAccID[a.ID] {
			continue
		}
		if !strings.HasPrefix(a.Verify, "file:") && seenVerify[a.Verify] {
			// Drop the duplicate: the FIRST occurrence wins, the
			// planner's later listing is presumed redundant.
			continue
		}
		if a.ID != "" {
			seenAccID[a.ID] = true
		}
		seenVerify[a.Verify] = true
		filteredAcc = append(filteredAcc, a)
	}
	p.Acceptance = filteredAcc
	// Drop duplicate FileSpec.Path entries within the same phase. Across
	// phases we tolerate duplicates because phases are display-only and
	// a model legitimately listing the same shared types file in two
	// phases is fine. Within a phase, duplicates are typo bugs.
	for pi := range p.Phases {
		seenPath := make(map[string]bool, len(p.Phases[pi].Files))
		dedup := p.Phases[pi].Files[:0]
		for _, f := range p.Phases[pi].Files {
			n := normalizePath(f.Path)
			if seenPath[n] {
				continue
			}
			seenPath[n] = true
			dedup = append(dedup, f)
		}
		p.Phases[pi].Files = dedup
	}
	// Post-filter sanity: if every file across every phase was rejected
	// AND no acceptance items survived, the plan has no usable content.
	// Empty plan would otherwise install a Plan with empty file list,
	// which renders an empty checklist and lets done() pass without any
	// verification.
	totalFiles := 0
	for _, ph := range p.Phases {
		totalFiles += len(ph.Files)
	}
	if totalFiles == 0 && len(p.Acceptance) == 0 {
		return nil, fmt.Errorf("plan: every entry was rejected by path/verify validation")
	}
	return &p, nil
}

// validPlanPath returns true if path is a usable repo-relative path:
// non-empty, not absolute, no '..' segment, no control characters.
// Paths that fail validation are dropped at Parse time so the matcher
// and checklist never have to defend against them downstream.
func validPlanPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	if strings.HasPrefix(path, "/") {
		return false
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return false
		}
	}
	for _, r := range path {
		if r < 0x20 {
			return false
		}
	}
	return true
}

// validAcceptanceVerify returns true if the verify string is a recognized
// shape we know how to auto-tick. Any "file:..." verify must carry a
// validPlanPath payload so the gate can never be tricked by an absolute
// or empty target. Unknown prefixes are rejected so a typo (e.g.
// "test.run:fail") doesn't silently install an acceptance item that
// never ticks. Closes ADV-01, ADV-02, ADV-12.
//
// Empty verify ("") is REJECTED post-AAR: the original semantics ("manual
// claim later") had no implementation -- there is no claim_acceptance
// tool, so any verify="" item permanently deadlocked the done() gate.
// Per the new prompt contract every acceptance must encode as
// file:/test.run:pass/compile.run:pass; vague items get dropped.
func validAcceptanceVerify(verify string) bool {
	if strings.HasPrefix(verify, "file:") {
		return validPlanPath(strings.TrimPrefix(verify, "file:"))
	}
	// bash:<command>:pass -- ticks when the orchestrator's bash dispatcher
	// records an exit-0 invocation with that exact command string. The
	// "command" payload is intentionally NOT path-validated: it can contain
	// arbitrary shell (e.g. `go test ./internal/foo/...` or `cd web && pnpm
	// test --run`) and the only contract is "the exact bytes the planner
	// wrote here must match the exact bytes the bash tool executed." This
	// is stricter than test.run:pass / compile.run:pass: there it was
	// "any test passed", which let the planner inflate one passing run
	// across many acceptance items (the v2 verify-vocabulary inflation
	// finding). The bash shape forces per-item granularity.
	if strings.HasPrefix(verify, "bash:") && strings.HasSuffix(verify, ":pass") {
		// At minimum the payload between the prefix and suffix must be
		// non-empty. Reject `bash::pass` (zero-length command) so a planner
		// typo doesn't install an item that pseudo-ticks on every empty
		// invocation.
		body := strings.TrimSuffix(strings.TrimPrefix(verify, "bash:"), ":pass")
		if strings.TrimSpace(body) == "" {
			return false
		}
		// Reject `:` (the bash null op): it always returns exit 0, so an
		// item using it pseudo-ticks the moment any bash call lands. The
		// planner likely meant a real command.
		if strings.TrimSpace(body) == ":" {
			return false
		}
		// Reject the degenerate case where TrimSuffix didn't actually
		// match -- when verify is `bash:pass`, TrimPrefix gives `pass`
		// and TrimSuffix(`:pass`) is a no-op (no leading colon), so body
		// becomes `pass`. That technically passes the non-empty check but
		// represents a planner who wrote the literal token `pass` thinking
		// it meant the suffix marker. Detect by reconstructing and
		// requiring at least one real internal `:` separator: a real
		// `bash:<cmd>:pass` always has TWO colons, so verify must have
		// at least 2.
		if strings.Count(verify, ":") < 2 {
			return false
		}
		return true
	}
	switch verify {
	case "test.run:pass", "compile.run:pass":
		return true
	}
	return false
}

// lastBalancedObject scans for the rightmost top-level balanced JSON
// object in s. Returns the substring including enclosing braces, or ""
// if no balanced object is found.
//
// Unlike a naive backward brace counter, this implementation respects JSON
// string-literal boundaries so that '{' or '}' characters appearing INSIDE
// a string value (e.g. an Acceptance description like "use { for opening")
// are not counted as structural braces. Closes audit-pass-1 ADV-14.
//
// Strategy: scan FORWARD through the entire string, tracking depth and
// in-string state. Whenever depth opens from 0 to 1, mark a candidate
// start. Whenever depth closes from 1 back to 0, that's a complete top-
// level object; remember its (start, end). At end-of-input return the
// LAST recorded (start, end) span. This pairs the OUTERMOST braces of
// the rightmost top-level object, which matches the "trailing JSON wins"
// contract callers depend on (planner reasoning models append the real
// plan at the end of their reply, sometimes after example JSON earlier
// in the response).
func lastBalancedObject(s string) string {
	depth := 0
	inString := false
	curStart := -1
	bestStart, bestEnd := -1, -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch c {
			case '\\':
				// Skip the escaped char so \" doesn't terminate the
				// string. \n / \uXXXX / \\ etc. are byte-skipped too;
				// we don't need full unescape semantics here.
				i++
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				curStart = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && curStart >= 0 {
				bestStart, bestEnd = curStart, i
				curStart = -1
			}
			if depth < 0 {
				// Stray '}' before any '{' -- reset and continue.
				depth = 0
				curStart = -1
			}
		}
	}
	if bestStart < 0 {
		return ""
	}
	return s[bestStart : bestEnd+1]
}

// MarkFileWritten ticks every FileSpec whose Path equals path (or matches
// after path normalization). Returns the number of items ticked.
//
// Path normalization: we accept both "src/foo.go" and "./src/foo.go" and
// "/abs/repo/src/foo.go" if repoRoot is provided. The orchestrator passes
// the resolved path it actually wrote, so it's the planner's path that
// determines the canonical form.
//
// Suffix-uniqueness fallback: if no exact match found, we look for FileSpecs
// where one path is a strict suffix of the other (split on '/'), AND that
// suffix relationship is uniquely satisfied by exactly one unticked FileSpec.
// This handles the common drift of planner saying `web/package.json` while
// the coder writes `apps/web/package.json` (or vice versa). Uniqueness is
// required because basename-only match would tick `package.json` ambiguously
// across a workspace with multiple of them.
func (p *Plan) MarkFileWritten(path string) int {
	if p == nil {
		return 0
	}
	norm := normalizePath(path)
	n := 0
	for pi := range p.Phases {
		for fi := range p.Phases[pi].Files {
			f := &p.Phases[pi].Files[fi]
			if f.Satisfied {
				continue
			}
			if normalizePath(f.Path) == norm {
				f.Satisfied = true
				n++
			}
		}
	}
	// Also tick any acceptance item with Verify="file:<path>"
	for ai := range p.Acceptance {
		a := &p.Acceptance[ai]
		if a.Satisfied {
			continue
		}
		if strings.HasPrefix(a.Verify, "file:") {
			want := strings.TrimPrefix(a.Verify, "file:")
			if normalizePath(want) == norm {
				a.Satisfied = true
				n++
			}
		}
	}
	// Fallback: only run suffix-uniqueness if exact match found nothing.
	// Conservative -- avoids accidentally ticking the wrong FileSpec when
	// the planner already specified canonical paths and got an exact hit.
	if n == 0 {
		n += p.markFileSuffixUnique(norm)
	}
	return n
}

// markFileSuffixUnique scans for an unticked FileSpec whose normalized path
// is a suffix-of-segments of the written path or vice versa, AND that no
// OTHER unticked FileSpec satisfies the same relationship. Returns 1 on a
// unique match, 0 otherwise.
func (p *Plan) markFileSuffixUnique(writtenNorm string) int {
	type cand struct {
		phaseIdx, fileIdx int
		acceptIdx         int // -1 if Phase, else acceptance index
	}
	var cands []cand
	for pi := range p.Phases {
		for fi := range p.Phases[pi].Files {
			f := &p.Phases[pi].Files[fi]
			if f.Satisfied {
				continue
			}
			pn := normalizePath(f.Path)
			if pathSegmentSuffix(writtenNorm, pn) || pathSegmentSuffix(pn, writtenNorm) {
				cands = append(cands, cand{phaseIdx: pi, fileIdx: fi, acceptIdx: -1})
			}
		}
	}
	for ai := range p.Acceptance {
		a := &p.Acceptance[ai]
		if a.Satisfied || !strings.HasPrefix(a.Verify, "file:") {
			continue
		}
		want := normalizePath(strings.TrimPrefix(a.Verify, "file:"))
		if pathSegmentSuffix(writtenNorm, want) || pathSegmentSuffix(want, writtenNorm) {
			cands = append(cands, cand{phaseIdx: -1, fileIdx: -1, acceptIdx: ai})
		}
	}
	if len(cands) != 1 {
		return 0
	}
	c := cands[0]
	if c.acceptIdx >= 0 {
		p.Acceptance[c.acceptIdx].Satisfied = true
	} else {
		p.Phases[c.phaseIdx].Files[c.fileIdx].Satisfied = true
	}
	return 1
}

// pathSegmentSuffix reports whether b is a suffix of a when split on '/'.
// Both must already be normalized. We require segment alignment (not raw
// string suffix) to avoid matching "ackage.json" against "package.json".
// A path is NOT a suffix of itself for this purpose -- exact equality is
// caller's responsibility (and is handled by exact match before this runs).
func pathSegmentSuffix(a, b string) bool {
	if a == "" || b == "" || a == b {
		return false
	}
	aSeg := strings.Split(a, "/")
	bSeg := strings.Split(b, "/")
	if len(bSeg) >= len(aSeg) {
		return false
	}
	// Require b to align with the trailing |b| segments of a.
	off := len(aSeg) - len(bSeg)
	for i, seg := range bSeg {
		if aSeg[off+i] != seg {
			return false
		}
	}
	return true
}

// MarkFilesPresent ticks every plan file (and every "file:<path>" acceptance
// item) whose target path now exists on disk under repoRoot. Returns the
// number of newly-satisfied items.
//
// This is the bash-only-mode counterpart to MarkFileWritten: in legacy mode
// fs.write tells us exactly which path was just touched, so MarkFileWritten
// is path-driven. In bash-only mode the model writes via heredoc inside a
// single bash invocation that may touch any number of paths; we don't know
// which without parsing the command. So we scan the plan's declared files
// after every successful bash invocation and tick whatever's present.
//
// Cost: O(files) stat calls per bash invocation. With the bashCmdHistoryLen
// cap of 64 and typical plans of ~20 files, that's ~1280 stats per worst-
// case run -- noise compared to the LLM call cost. The repo is local,
// stat is cached at the kernel level. Fine.
//
// Skips files already satisfied so we don't re-tick. Skips paths that
// would resolve outside the repo (the planner's path is interpreted as
// relative to repoRoot; an absolute or traversing path is silently
// ignored here, since validPlanPath already vetted it at parse time).
func (p *Plan) MarkFilesPresent(repoRoot string) int {
	if p == nil || repoRoot == "" {
		return 0
	}
	// Defer the os import to a tiny helper so the test for plan.Parse
	// doesn't need a temp dir just because Parse exists in this package.
	stat := planFileStat
	n := 0
	for pi := range p.Phases {
		for fi := range p.Phases[pi].Files {
			f := &p.Phases[pi].Files[fi]
			if f.Satisfied {
				continue
			}
			if stat(repoRoot, f.Path) {
				f.Satisfied = true
				n++
			}
		}
	}
	for ai := range p.Acceptance {
		a := &p.Acceptance[ai]
		if a.Satisfied || !strings.HasPrefix(a.Verify, "file:") {
			continue
		}
		want := strings.TrimPrefix(a.Verify, "file:")
		if stat(repoRoot, want) {
			a.Satisfied = true
			n++
		}
	}
	return n
}

// planFileStat returns true when joining repoRoot + relPath yields an
// existing file (not a directory). Function-valued so tests can swap in
// a hermetic stub without touching the filesystem.
var planFileStat = func(repoRoot, relPath string) bool {
	if relPath == "" {
		return false
	}
	full := filepath.Join(repoRoot, relPath)
	info, err := os.Stat(full)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// MarkAcceptance ticks acceptance items whose Verify field matches verifyKey
// (e.g. "test.run:pass" or "compile.run:pass"). Returns count.
func (p *Plan) MarkAcceptance(verifyKey string) int {
	if p == nil {
		return 0
	}
	n := 0
	for ai := range p.Acceptance {
		a := &p.Acceptance[ai]
		if a.Satisfied {
			continue
		}
		if a.Verify == verifyKey {
			a.Satisfied = true
			n++
		}
	}
	return n
}

// MarkAcceptanceByID ticks the acceptance item with the matching ID. Used by
// a future "claim_acceptance" tool the reviewer can call when an item has
// no automatic verifier. Returns true if an item was ticked.
func (p *Plan) MarkAcceptanceByID(id string) bool {
	if p == nil {
		return false
	}
	for ai := range p.Acceptance {
		a := &p.Acceptance[ai]
		if a.ID == id && !a.Satisfied {
			a.Satisfied = true
			return true
		}
	}
	return false
}

// UnsatisfiedAcceptance returns the descriptions of acceptance items that
// have not yet been ticked. Used by the done() gate to refuse premature
// termination with a useful message.
func (p *Plan) UnsatisfiedAcceptance() []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, a := range p.Acceptance {
		if !a.Satisfied {
			out = append(out, a.Description)
		}
	}
	return out
}

// ProgressCounts returns (filesDone, filesTotal, accDone, accTotal) for
// the current Plan state. Used by the orchestrator's trace events so
// observers can watch tick progression directly without parsing the
// rendered checklist text (which is byte-neutral on tick because both
// "[ ]" and "[x]" are 3 bytes).
func (p *Plan) ProgressCounts() (filesDone, filesTotal, accDone, accTotal int) {
	if p == nil {
		return 0, 0, 0, 0
	}
	for _, ph := range p.Phases {
		for _, f := range ph.Files {
			filesTotal++
			if f.Satisfied {
				filesDone++
			}
		}
	}
	accTotal = len(p.Acceptance)
	for _, a := range p.Acceptance {
		if a.Satisfied {
			accDone++
		}
	}
	return
}

// renderCompactThreshold is the file count above which RenderChecklist
// switches to compact mode: drops Purpose comments to save bytes, and
// collapses fully-satisfied phases to a single summary line. Calibrated
// from rematch #18: a 100-file plan rendered in full produces a 7320-
// byte checklist injected every reviewer turn -- after 30 turns that
// adds ~150KB to context, blowing the 32K reviewer cap. Compact mode
// drops it to ~3KB initial, shrinking further as phases complete.
const renderCompactThreshold = 50

// RenderChecklist produces the <CHECKLIST>...</CHECKLIST> block injected
// before every reviewer turn. Empty plan returns "" so the caller can skip
// injection. Switches to compact mode when the plan has more than
// renderCompactThreshold files.
func (p *Plan) RenderChecklist() string {
	if p == nil || (len(p.Phases) == 0 && len(p.Acceptance) == 0) {
		return ""
	}
	_, totalFiles, _, _ := p.ProgressCounts()
	if totalFiles > renderCompactThreshold {
		return p.renderChecklistCompact()
	}
	return p.renderChecklistFull()
}

// renderChecklistFull is the original full-fidelity rendering: every
// FileSpec gets a [x]/[ ] line plus Purpose comment if present. Used
// for plans up to renderCompactThreshold files.
func (p *Plan) renderChecklistFull() string {
	var b strings.Builder
	b.WriteString("<CHECKLIST>\n")
	if len(p.Phases) > 0 {
		b.WriteString("Files to produce (tick = on disk):\n")
		for _, ph := range p.Phases {
			fmt.Fprintf(&b, "  Phase %s -- %s\n", ph.ID, ph.Name)
			for _, f := range ph.Files {
				mark := "[ ]"
				if f.Satisfied {
					mark = "[x]"
				}
				if f.Purpose != "" {
					fmt.Fprintf(&b, "    %s %s -- %s\n", mark, f.Path, f.Purpose)
				} else {
					fmt.Fprintf(&b, "    %s %s\n", mark, f.Path)
				}
			}
		}
	}
	p.renderAcceptance(&b)
	p.renderProgressFooter(&b)
	b.WriteString("</CHECKLIST>")
	return b.String()
}

// renderChecklistCompact drops file Purpose comments and collapses fully-
// satisfied phases to a one-line summary ("Phase P1 -- Scaffold (9/9) [x]
// done"). Phases with any unsatisfied file render in full but without
// Purpose. Acceptance is always rendered in full because it is small (and
// each item is the meat of the done() gate).
func (p *Plan) renderChecklistCompact() string {
	var b strings.Builder
	b.WriteString("<CHECKLIST>\n")
	if len(p.Phases) > 0 {
		b.WriteString("Files to produce (tick = on disk; phase summary lines collapse fully-done phases):\n")
		for _, ph := range p.Phases {
			done, total := 0, len(ph.Files)
			for _, f := range ph.Files {
				if f.Satisfied {
					done++
				}
			}
			// Skip phases with zero files entirely in compact mode --
			// they add no meaningful information ("Phase Pn -- name (0/0)"
			// is just clutter). Closes audit-pass-3 P3-MIN-3.
			if total == 0 {
				continue
			}
			if done == total {
				fmt.Fprintf(&b, "  Phase %s -- %s (%d/%d) [x] done\n", ph.ID, ph.Name, done, total)
				continue
			}
			fmt.Fprintf(&b, "  Phase %s -- %s (%d/%d)\n", ph.ID, ph.Name, done, total)
			for _, f := range ph.Files {
				mark := "[ ]"
				if f.Satisfied {
					mark = "[x]"
				}
				fmt.Fprintf(&b, "    %s %s\n", mark, f.Path)
			}
		}
	}
	p.renderAcceptance(&b)
	p.renderProgressFooter(&b)
	b.WriteString("</CHECKLIST>")
	return b.String()
}

func (p *Plan) renderAcceptance(b *strings.Builder) {
	if len(p.Acceptance) == 0 {
		return
	}
	if len(p.Phases) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("Acceptance criteria (tick = verified):\n")
	for _, a := range p.Acceptance {
		mark := "[ ]"
		if a.Satisfied {
			mark = "[x]"
		}
		fmt.Fprintf(b, "  %s %s: %s\n", mark, a.ID, a.Description)
	}
}

func (p *Plan) renderProgressFooter(b *strings.Builder) {
	fd, ft, ad, at := p.ProgressCounts()
	fmt.Fprintf(b, "\nProgress: %d/%d files, %d/%d acceptance.\n", fd, ft, ad, at)
}

// normalizePath strips "./" prefix, trailing slashes, and normalizes
// separators (backslash -> forward, runs of slashes collapsed). Does
// NOT resolve to absolute -- that's the orchestrator's job at write
// time. Closes audit-pass-1 ADV-09 (backslash separators -- if a model
// emits Windows-style paths, we still match) and ADV-10 (double slashes
// from naive base+relative joins still match).
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	// ADV-09: convert backslash separators to forward.
	p = strings.ReplaceAll(p, "\\", "/")
	// ADV-10: collapse runs of forward slashes to a single slash. We do
	// this by repeated replacement (cheap; paths are short).
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/")
	// We DO NOT lowercase the whole path; case-sensitivity matters on
	// most filesystems and the planner specifies casing intentionally.
	return p
}
