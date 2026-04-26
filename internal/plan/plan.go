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
	return &p, nil
}

// lastBalancedObject scans backward for the start of the rightmost balanced
// JSON object in s. Returns the substring including its enclosing braces, or
// "" if no balanced object is found.
func lastBalancedObject(s string) string {
	end := strings.LastIndexByte(s, '}')
	if end < 0 {
		return ""
	}
	depth := 0
	for i := end; i >= 0; i-- {
		switch s[i] {
		case '}':
			depth++
		case '{':
			depth--
			if depth == 0 {
				return s[i : end+1]
			}
		}
	}
	return ""
}

// MarkFileWritten ticks every FileSpec whose Path equals path (or matches
// after path normalization). Returns the number of items ticked.
//
// Path normalization: we accept both "src/foo.go" and "./src/foo.go" and
// "/abs/repo/src/foo.go" if repoRoot is provided. The orchestrator passes
// the resolved path it actually wrote, so it's the planner's path that
// determines the canonical form.
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
	return n
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

// RenderChecklist produces the <CHECKLIST>...</CHECKLIST> block injected
// before every reviewer turn. Empty plan returns "" so the caller can skip
// injection.
func (p *Plan) RenderChecklist() string {
	if p == nil || (len(p.Phases) == 0 && len(p.Acceptance) == 0) {
		return ""
	}
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
	if len(p.Acceptance) > 0 {
		if len(p.Phases) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Acceptance criteria (tick = verified):\n")
		for _, a := range p.Acceptance {
			mark := "[ ]"
			if a.Satisfied {
				mark = "[x]"
			}
			fmt.Fprintf(&b, "  %s %s: %s\n", mark, a.ID, a.Description)
		}
	}
	// Quick summary so the reviewer can see progress at a glance.
	totalFiles, doneFiles := 0, 0
	for _, ph := range p.Phases {
		for _, f := range ph.Files {
			totalFiles++
			if f.Satisfied {
				doneFiles++
			}
		}
	}
	totalAcc, doneAcc := len(p.Acceptance), 0
	for _, a := range p.Acceptance {
		if a.Satisfied {
			doneAcc++
		}
	}
	fmt.Fprintf(&b, "\nProgress: %d/%d files, %d/%d acceptance.\n",
		doneFiles, totalFiles, doneAcc, totalAcc)
	b.WriteString("</CHECKLIST>")
	return b.String()
}

// normalizePath strips "./" prefix and trailing slashes; lowercases path
// separators. Does NOT resolve to absolute -- that's the orchestrator's
// job at write time.
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/")
	// We DO NOT lowercase the whole path; case-sensitivity matters on
	// most filesystems and the planner specifies casing intentionally.
	return p
}
