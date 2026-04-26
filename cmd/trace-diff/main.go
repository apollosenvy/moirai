// trace-diff is the comparison companion to trace-summary and trace-tail.
// Given two rematch trace files (typically before-fix and after-fix), it
// emits a side-by-side comparison of the metrics that matter for the
// rematch protocol: turn count, file/acceptance ticks, failure mode,
// time-per-turn, replace-vs-append distribution, and tool-call breakdown.
//
// Usage:
//
//	trace-diff <baseline.jsonl> <candidate.jsonl>
//
// Designed for the post-rematch question "did this change make the
// rematch run further?" -- the protocol's success metric is "the new
// failure mode is different from the old failure mode", which trace-diff
// surfaces by showing both fail_reason values side-by-side.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

type event struct {
	TS     string         `json:"ts"`
	TaskID string         `json:"task_id"`
	Kind   string         `json:"kind"`
	Data   map[string]any `json:"data,omitempty"`
}

type stats struct {
	taskID                string
	first, last           time.Time
	events                int
	toolCounts            map[string]int
	tickSources           map[string]int
	llmCallsByRole        map[string]int
	rollingReclaim        int64
	rollingEvents         int
	turns                 int
	filesDoneMax          int
	filesTotal            int
	accDoneMax            int
	accTotal              int
	checklistInjections   int
	checklistReplace      int
	checklistAppend       int
	planParsed            bool
	planPhases            int
	planAccItems          int
	failed                bool
	failReason            string
	checklistBytesLast    int
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: trace-diff <baseline.jsonl> <candidate.jsonl>")
		os.Exit(2)
	}
	a, err := readStats(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "baseline:", err)
		os.Exit(1)
	}
	b, err := readStats(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "candidate:", err)
		os.Exit(1)
	}
	emitDiff(os.Stdout, a, b)
}

func readStats(path string) (*stats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := &stats{
		toolCounts:     map[string]int{},
		tickSources:    map[string]int{},
		llmCallsByRole: map[string]int{},
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		s.events++
		if s.taskID == "" {
			s.taskID = e.TaskID
		}
		if t, err := time.Parse(time.RFC3339Nano, e.TS); err == nil {
			if s.first.IsZero() {
				s.first = t
			}
			s.last = t
		}
		switch e.Kind {
		case "tool_call":
			if name, ok := e.Data["name"].(string); ok {
				s.toolCounts[name]++
			}
		case "llm_call":
			role, _ := e.Data["role"].(string)
			s.llmCallsByRole[role]++
		case "info":
			if v, ok := numAsInt(e.Data["checklist_ticked"]); ok && v > 0 {
				src, _ := e.Data["source"].(string)
				if src == "" {
					src = "unknown"
				}
				s.tickSources[src] += v
			}
			if _, ok := e.Data["plan_parsed"].(bool); ok {
				s.planParsed = true
				if p, ok := numAsInt(e.Data["phases"]); ok {
					s.planPhases = p
				}
				if a, ok := numAsInt(e.Data["acceptance_items"]); ok {
					s.planAccItems = a
				}
			}
			if _, ok := e.Data["checklist_injected"].(bool); ok {
				s.checklistInjections++
				if r, ok := e.Data["replaced"].(bool); ok {
					if r {
						s.checklistReplace++
					} else {
						s.checklistAppend++
					}
				}
				if fd, ok := numAsInt(e.Data["files_done"]); ok && fd > s.filesDoneMax {
					s.filesDoneMax = fd
				}
				if ft, ok := numAsInt(e.Data["files_total"]); ok && ft > s.filesTotal {
					s.filesTotal = ft
				}
				if ad, ok := numAsInt(e.Data["acc_done"]); ok && ad > s.accDoneMax {
					s.accDoneMax = ad
				}
				if at, ok := numAsInt(e.Data["acc_total"]); ok && at > s.accTotal {
					s.accTotal = at
				}
				if b, ok := numAsInt(e.Data["bytes"]); ok {
					s.checklistBytesLast = b
				}
			}
			if _, ok := e.Data["rolling_compact"].(bool); ok {
				s.rollingEvents++
				if rb, ok := numAsInt(e.Data["reclaimed_bytes"]); ok {
					s.rollingReclaim += int64(rb)
				}
			}
		case "error":
			s.failed = true
			if r, ok := e.Data["fatal"].(string); ok {
				s.failReason = r
			}
		case "verdict":
			if v, ok := e.Data["verdict"].(string); ok && (v == "failed" || v == "aborted") {
				s.failed = true
			}
			if r, ok := e.Data["reason"].(string); ok && s.failReason == "" {
				s.failReason = r
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	s.turns = s.llmCallsByRole["reviewer"]
	return s, nil
}

func emitDiff(w io.Writer, a, b *stats) {
	col := func(label, baseline, candidate string) {
		fmt.Fprintf(w, "  %-22s %-30s %-30s\n", label, baseline, candidate)
	}
	header := func(s string) {
		fmt.Fprintln(w, s)
		fmt.Fprintln(w, strings.Repeat("=", len(s)))
	}

	header("REMATCH DIFF")
	fmt.Fprintln(w)
	col("metric", "baseline", "candidate")
	fmt.Fprintln(w, "  ----------------------- ------------------------------ ------------------------------")
	col("task_id", short(a.taskID, 28), short(b.taskID, 28))
	col("events", fmt.Sprint(a.events), withDelta(a.events, b.events))
	col("duration", a.last.Sub(a.first).Truncate(time.Second).String(), b.last.Sub(b.first).Truncate(time.Second).String())
	col("outcome", outcomeOf(a), outcomeOf(b))
	col("fail_reason", short(a.failReason, 28), short(b.failReason, 28))
	col("plan parsed", planLabel(a), planLabel(b))
	col("reviewer turns", fmt.Sprint(a.turns), withDelta(a.turns, b.turns))
	col("files ticked", fmt.Sprintf("%d/%d", a.filesDoneMax, a.filesTotal),
		fmt.Sprintf("%d/%d (%s)", b.filesDoneMax, b.filesTotal, deltaSign(a.filesDoneMax, b.filesDoneMax)))
	col("acceptance ticked", fmt.Sprintf("%d/%d", a.accDoneMax, a.accTotal),
		fmt.Sprintf("%d/%d (%s)", b.accDoneMax, b.accTotal, deltaSign(a.accDoneMax, b.accDoneMax)))
	col("checklist injects", fmt.Sprintf("%d (r=%d a=%d)", a.checklistInjections, a.checklistReplace, a.checklistAppend),
		fmt.Sprintf("%d (r=%d a=%d)", b.checklistInjections, b.checklistReplace, b.checklistAppend))
	col("checklist last bytes", fmt.Sprint(a.checklistBytesLast), withDelta(a.checklistBytesLast, b.checklistBytesLast))
	col("rolling compactions", fmt.Sprint(a.rollingEvents), withDelta(a.rollingEvents, b.rollingEvents))
	col("rolling reclaimed", fmt.Sprint(a.rollingReclaim), fmt.Sprint(b.rollingReclaim))
	if a.turns > 0 && b.turns > 0 {
		ad := a.last.Sub(a.first) / time.Duration(a.turns)
		bd := b.last.Sub(b.first) / time.Duration(b.turns)
		col("time per turn", ad.Truncate(time.Second).String(), bd.Truncate(time.Second).String())
	}
	fmt.Fprintln(w)

	header("TICK SOURCES")
	fmt.Fprintln(w)
	keys := unionKeys(a.tickSources, b.tickSources)
	for _, k := range keys {
		col(k+":", fmt.Sprint(a.tickSources[k]), withDelta(a.tickSources[k], b.tickSources[k]))
	}
	fmt.Fprintln(w)

	header("TOOL CALLS")
	fmt.Fprintln(w)
	keys = unionKeys(a.toolCounts, b.toolCounts)
	for _, k := range keys {
		col(k+":", fmt.Sprint(a.toolCounts[k]), withDelta(a.toolCounts[k], b.toolCounts[k]))
	}
	fmt.Fprintln(w)
}

func unionKeys(a, b map[string]int) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		// Sort by max(a[k], b[k]) descending so the loudest changes come first.
		ai := max(a[out[i]], b[out[i]])
		aj := max(a[out[j]], b[out[j]])
		if ai != aj {
			return ai > aj
		}
		return out[i] < out[j]
	})
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func numAsInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func withDelta(a, b int) string {
	delta := b - a
	if delta == 0 {
		return fmt.Sprintf("%d (==)", b)
	}
	if delta > 0 {
		return fmt.Sprintf("%d (+%d)", b, delta)
	}
	return fmt.Sprintf("%d (%d)", b, delta)
}

func deltaSign(a, b int) string {
	if b > a {
		return fmt.Sprintf("+%d", b-a)
	}
	if b < a {
		return fmt.Sprintf("%d", b-a)
	}
	return "=="
}

func outcomeOf(s *stats) string {
	if s.failed {
		return "FAILED"
	}
	return "ok"
}

func planLabel(s *stats) string {
	if !s.planParsed {
		return "no"
	}
	return fmt.Sprintf("yes (%dp/%da)", s.planPhases, s.planAccItems)
}
