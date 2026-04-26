// trace-summary reads an agent-router trace.jsonl and produces a one-page
// rematch health report: turn count, file/acceptance tick rate, failure
// mode, time-per-turn, source breakdown for ticks. Written for the
// rematch-protocol post-mortem cycle so a session reviewing rematch
// outcomes doesn't have to grep + python3 the trace by hand.
//
// Usage:
//
//	trace-summary <path/to/trace.jsonl>
//	trace-summary - < trace.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

type event struct {
	TS     string                 `json:"ts"`
	TaskID string                 `json:"task_id"`
	Kind   string                 `json:"kind"`
	Data   map[string]any         `json:"data,omitempty"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: trace-summary <trace.jsonl>  (use - for stdin)")
		os.Exit(2)
	}
	var r io.Reader
	if os.Args[1] == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(os.Args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "open:", err)
			os.Exit(1)
		}
		defer f.Close()
		r = f
	}
	if err := summarize(r, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "summarize:", err)
		os.Exit(1)
	}
}

func summarize(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)

	var (
		taskID                 string
		first, last            time.Time
		eventCount             int
		kindCounts             = map[string]int{}
		toolCounts             = map[string]int{}
		tickSources            = map[string]int{}
		acceptanceVerifyTicks  = map[string]int{}
		nudgeReasons           = map[string]int{}
		swapReasons            = map[string]int{}
		llmCallsByRole         = map[string]int{}
		llmBytesByRole         = map[string]int{}
		rollingCompactCount    int
		rollingCompactReclaim  int64
		fileTicksMax           int
		filesTotal             int
		accDoneMax             int
		accTotal               int
		checklistInjections    int
		checklistReplaceCount  int
		checklistAppendCount   int
		lastChecklistBytes     int
		minChecklistBytes      = -1
		maxChecklistBytes      int
		planParsed             bool
		planPhases             int
		planAcceptance         int
		failed                 bool
		failedReason           string
	)

	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		eventCount++
		if taskID == "" {
			taskID = e.TaskID
		}
		if t, err := time.Parse(time.RFC3339Nano, e.TS); err == nil {
			if first.IsZero() {
				first = t
			}
			last = t
		}
		kindCounts[e.Kind]++

		switch e.Kind {
		case "tool_call":
			if name, ok := e.Data["name"].(string); ok {
				toolCounts[name]++
			}
		case "swap":
			if r, ok := e.Data["reason"].(string); ok {
				swapReasons[r]++
			}
		case "llm_call":
			role, _ := e.Data["role"].(string)
			llmCallsByRole[role]++
			if b, ok := numAsInt(e.Data["bytes"]); ok {
				llmBytesByRole[role] += b
			}
		case "info":
			if v, ok := numAsInt(e.Data["checklist_ticked"]); ok {
				src, _ := e.Data["source"].(string)
				if src == "" {
					src = "unknown"
				}
				if v > 0 {
					tickSources[src] += v
				}
				if verify, ok := e.Data["verify"].(string); ok {
					acceptanceVerifyTicks[verify] += v
				}
			}
			if _, ok := e.Data["plan_parsed"].(bool); ok {
				planParsed = true
				if p, ok := numAsInt(e.Data["phases"]); ok {
					planPhases = p
				}
				if a, ok := numAsInt(e.Data["acceptance_items"]); ok {
					planAcceptance = a
				}
			}
			if _, ok := e.Data["checklist_injected"].(bool); ok {
				checklistInjections++
				if r, ok := e.Data["replaced"].(bool); ok {
					if r {
						checklistReplaceCount++
					} else {
						checklistAppendCount++
					}
				}
				if b, ok := numAsInt(e.Data["bytes"]); ok {
					lastChecklistBytes = b
					if minChecklistBytes < 0 || b < minChecklistBytes {
						minChecklistBytes = b
					}
					if b > maxChecklistBytes {
						maxChecklistBytes = b
					}
				}
				if fd, ok := numAsInt(e.Data["files_done"]); ok && fd > fileTicksMax {
					fileTicksMax = fd
				}
				if ft, ok := numAsInt(e.Data["files_total"]); ok && ft > filesTotal {
					filesTotal = ft
				}
				if ad, ok := numAsInt(e.Data["acc_done"]); ok && ad > accDoneMax {
					accDoneMax = ad
				}
				if at, ok := numAsInt(e.Data["acc_total"]); ok && at > accTotal {
					accTotal = at
				}
			}
			if _, ok := e.Data["rolling_compact"].(bool); ok {
				rollingCompactCount++
				if rb, ok := numAsInt(e.Data["reclaimed_bytes"]); ok {
					rollingCompactReclaim += int64(rb)
				}
			}
			if r, ok := e.Data["ro_nudge"].(string); ok {
				nudgeReasons[r]++
			}
		case "error":
			failed = true
			if reason, ok := e.Data["fatal"].(string); ok {
				failedReason = reason
			}
		case "verdict":
			if v, ok := e.Data["verdict"].(string); ok {
				if v == "failed" || v == "aborted" {
					failed = true
				}
			}
			if r, ok := e.Data["reason"].(string); ok && failedReason == "" {
				failedReason = r
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}

	dur := last.Sub(first)
	turns := llmCallsByRole["reviewer"]

	fmt.Fprintln(w, "REMATCH SUMMARY")
	fmt.Fprintln(w, "===============")
	fmt.Fprintf(w, "task_id:       %s\n", taskID)
	fmt.Fprintf(w, "events:        %d\n", eventCount)
	fmt.Fprintf(w, "duration:      %s (start %s)\n", dur.Truncate(time.Second), first.Format(time.RFC3339))
	fmt.Fprintf(w, "outcome:       %s\n", outcomeOf(failed, failedReason))
	if failedReason != "" {
		fmt.Fprintf(w, "fail_reason:   %s\n", truncate(failedReason, 200))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "PLAN")
	fmt.Fprintln(w, "----")
	if planParsed {
		fmt.Fprintf(w, "parsed:        yes (%d phases, %d acceptance items)\n", planPhases, planAcceptance)
	} else {
		fmt.Fprintln(w, "parsed:        no")
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "PROGRESS")
	fmt.Fprintln(w, "--------")
	fmt.Fprintf(w, "reviewer turns:  %d\n", turns)
	if filesTotal > 0 {
		pct := float64(fileTicksMax) / float64(filesTotal) * 100
		fmt.Fprintf(w, "files ticked:    %d/%d (%.0f%%)\n", fileTicksMax, filesTotal, pct)
	}
	if accTotal > 0 {
		pct := float64(accDoneMax) / float64(accTotal) * 100
		fmt.Fprintf(w, "acceptance:      %d/%d (%.0f%%)\n", accDoneMax, accTotal, pct)
	}
	if turns > 0 && dur > 0 {
		fmt.Fprintf(w, "time per turn:   %s\n", (dur / time.Duration(turns)).Truncate(time.Second))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "CHECKLIST")
	fmt.Fprintln(w, "---------")
	fmt.Fprintf(w, "injections:    %d (replace=%d, append=%d)\n", checklistInjections, checklistReplaceCount, checklistAppendCount)
	if minChecklistBytes >= 0 {
		fmt.Fprintf(w, "bytes:         min=%d max=%d last=%d\n", minChecklistBytes, maxChecklistBytes, lastChecklistBytes)
	}
	fmt.Fprintln(w)

	if len(tickSources) > 0 {
		fmt.Fprintln(w, "TICK SOURCES")
		fmt.Fprintln(w, "------------")
		printSortedByValueDesc(w, tickSources)
		fmt.Fprintln(w)
	}

	if len(toolCounts) > 0 {
		fmt.Fprintln(w, "TOOL CALLS")
		fmt.Fprintln(w, "----------")
		printSortedByValueDesc(w, toolCounts)
		fmt.Fprintln(w)
	}

	if rollingCompactCount > 0 {
		fmt.Fprintln(w, "COMPACTION")
		fmt.Fprintln(w, "----------")
		fmt.Fprintf(w, "rolling events: %d (total reclaimed: %d bytes)\n", rollingCompactCount, rollingCompactReclaim)
		fmt.Fprintln(w)
	}

	if len(nudgeReasons) > 0 {
		fmt.Fprintln(w, "NUDGES")
		fmt.Fprintln(w, "------")
		printSortedByValueDesc(w, nudgeReasons)
		fmt.Fprintln(w)
	}

	if len(swapReasons) > 0 {
		fmt.Fprintln(w, "SWAPS")
		fmt.Fprintln(w, "-----")
		printSortedByValueDesc(w, swapReasons)
		fmt.Fprintln(w)
	}

	return nil
}

func numAsInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// sortedByValueDesc returns map entries sorted by value descending. Returned
// as a slice of pairs so the caller can iterate in order.
func sortedByValueDesc(m map[string]int) []struct {
	K string
	V int
} {
	type kv struct {
		K string
		V int
	}
	pairs := make([]kv, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].V > pairs[j].V })
	out := make([]struct {
		K string
		V int
	}, len(pairs))
	for i, p := range pairs {
		out[i] = struct {
			K string
			V int
		}{p.K, p.V}
	}
	return out
}

func printSortedByValueDesc(w io.Writer, m map[string]int) {
	for _, p := range sortedByValueDesc(m) {
		fmt.Fprintf(w, "  %-30s %d\n", p.K+":", p.V)
	}
}

func outcomeOf(failed bool, reason string) string {
	if !failed {
		return "succeeded or running"
	}
	if reason != "" {
		return "FAILED"
	}
	return "FAILED"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
