// trace-tail is the rematch-watch companion to trace-summary. It tails a
// trace.jsonl file as new events stream in and pretty-prints each one in
// a single-line color-coded form so a human can monitor a live rematch
// without a separate dashboard.
//
// Usage:
//
//	trace-tail <path/to/trace.jsonl>
//
// Highlights:
//
//   - swap events show the destination model + reason (e.g. "→ planner / ask_planner")
//   - tool_call events show the tool name and a short args summary
//   - llm_call events show role + bytes + a 60-char head preview
//   - info events with checklist_injected show "ck N/total bytes=X (replace|append)"
//   - info events with checklist_ticked show "tick path=X source=Y"
//   - error / verdict events are highlighted
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type event struct {
	TS     string         `json:"ts"`
	TaskID string         `json:"task_id"`
	Kind   string         `json:"kind"`
	Data   map[string]any `json:"data,omitempty"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: trace-tail <trace.jsonl>")
		os.Exit(2)
	}
	path := os.Args[1]

	// Open and read everything available, then poll for new lines. We
	// don't seek to end -- the user usually wants the full history first
	// so they understand the run state when they joined.
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<16)
	buf := make([]byte, 0, 4096)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			buf = append(buf, line...)
			if line[len(line)-1] == '\n' {
				printEvent(buf)
				buf = buf[:0]
			}
		}
		if err == io.EOF {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
	}
}

func printEvent(raw []byte) {
	var e event
	if err := json.Unmarshal(raw, &e); err != nil {
		return
	}
	ts := e.TS
	if t, err := time.Parse(time.RFC3339Nano, e.TS); err == nil {
		ts = t.Format("15:04:05")
	}
	d := e.Data
	if d == nil {
		d = map[string]any{}
	}
	switch e.Kind {
	case "swap":
		to, _ := d["to"].(string)
		reason, _ := d["reason"].(string)
		fmt.Printf("%s SWAP   → %s  (%s)\n", ts, to, reason)
	case "phase":
		phase, _ := d["phase"].(string)
		fmt.Printf("%s PHASE  %s\n", ts, phase)
	case "llm_call":
		role, _ := d["role"].(string)
		bytes, _ := numAsInt(d["bytes"])
		head, _ := d["head"].(string)
		fmt.Printf("%s LLM    %-9s %5dB  %q\n", ts, role, bytes, shorten(head, 80))
	case "tool_call":
		name, _ := d["name"].(string)
		err, _ := d["error"].(string)
		errStr := ""
		if err != "" {
			errStr = " ERR=" + shorten(err, 40)
		}
		bytes, _ := numAsInt(d["bytes"])
		argSummary := summarizeArgs(d["args"])
		fmt.Printf("%s TOOL   %-13s %5dB %s%s\n", ts, name, bytes, argSummary, errStr)
	case "info":
		if _, ok := d["checklist_injected"].(bool); ok {
			fd, _ := numAsInt(d["files_done"])
			ft, _ := numAsInt(d["files_total"])
			ad, _ := numAsInt(d["acc_done"])
			at, _ := numAsInt(d["acc_total"])
			bytes, _ := numAsInt(d["bytes"])
			turn, _ := numAsInt(d["turn"])
			rep := "append"
			if r, _ := d["replaced"].(bool); r {
				rep = "replace"
			}
			fmt.Printf("%s CKLST  turn=%d files=%d/%d acc=%d/%d bytes=%d %s\n",
				ts, turn, fd, ft, ad, at, bytes, rep)
			return
		}
		if _, ok := d["plan_parsed"].(bool); ok {
			phases, _ := numAsInt(d["phases"])
			acc, _ := numAsInt(d["acceptance_items"])
			fmt.Printf("%s PLAN   parsed phases=%d acceptance=%d\n", ts, phases, acc)
			return
		}
		if n, ok := numAsInt(d["checklist_ticked"]); ok {
			path, _ := d["path"].(string)
			src, _ := d["source"].(string)
			verify, _ := d["verify"].(string)
			tag := path
			if tag == "" {
				tag = verify
			}
			fmt.Printf("%s TICK   n=%d src=%-12s %s\n", ts, n, src, tag)
			return
		}
		if _, ok := d["rolling_compact"].(bool); ok {
			rb, _ := numAsInt(d["reclaimed_bytes"])
			turn, _ := numAsInt(d["turn"])
			fmt.Printf("%s ROLL   turn=%d reclaimed=%d bytes\n", ts, turn, rb)
			return
		}
		if reason, ok := d["ro_nudge"].(string); ok {
			turn, _ := numAsInt(d["turn"])
			fmt.Printf("%s NUDGE  turn=%d  %s\n", ts, turn, reason)
			return
		}
		if drained, ok := numAsInt(d["inject_drained"]); ok {
			fmt.Printf("%s INJ    drained=%d\n", ts, drained)
			return
		}
		// Unknown info -- emit a brief summary.
		fmt.Printf("%s INFO   %s\n", ts, shortenJSON(d, 100))
	case "error":
		fatal, _ := d["fatal"].(string)
		fmt.Printf("%s ERROR  %s\n", ts, shorten(fatal, 200))
	case "verdict":
		verdict, _ := d["verdict"].(string)
		reason, _ := d["reason"].(string)
		fmt.Printf("%s VERDICT %s  reason=%s\n", ts, verdict, shorten(reason, 200))
	case "done":
		fmt.Printf("%s DONE   %s\n", ts, shortenJSON(d, 80))
	default:
		fmt.Printf("%s %-7s %s\n", ts, strings.ToUpper(e.Kind), shortenJSON(d, 80))
	}
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

func shorten(s string, n int) string {
	if len(s) <= n {
		return strings.ReplaceAll(s, "\n", " ")
	}
	return strings.ReplaceAll(s[:n], "\n", " ") + "..."
}

func shortenJSON(m map[string]any, n int) string {
	b, _ := json.Marshal(m)
	return shorten(string(b), n)
}

func summarizeArgs(args any) string {
	m, ok := args.(map[string]any)
	if !ok {
		return ""
	}
	if name, ok := m["name"].(string); ok {
		return "name=" + name
	}
	if path, ok := m["path"].(string); ok {
		return "path=" + path
	}
	if instr, ok := m["instruction"].(string); ok {
		return "instr=" + shorten(instr, 60)
	}
	if q, ok := m["query"].(string); ok {
		return "query=" + shorten(q, 60)
	}
	return shortenJSON(m, 60)
}
