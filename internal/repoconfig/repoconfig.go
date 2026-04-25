// Package repoconfig parses .agent-router.toml files at repo roots.
//
// Minimal TOML-ish parser. We only support the subset the spec calls for
// ([commands], [style], [forbidden], [budget]) so we don't pull in a full
// TOML dependency just for 30 lines of config.
package repoconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Commands struct {
	Test    string
	Compile string
	Lint    string
}

type Style struct {
	Language   string
	LineLength int
	Notes      string
}

type Forbidden struct {
	Paths []string
}

type Budget struct {
	MaxRuntime    time.Duration
	MaxIterations int
}

type Config struct {
	Commands  Commands
	Style     Style
	Forbidden Forbidden
	Budget    Budget
	Path      string // the file we loaded
}

// Default values applied when a key is missing.
func defaults() Config {
	return Config{
		Budget: Budget{
			MaxRuntime:    30 * time.Minute,
			MaxIterations: 6,
		},
	}
}

// Load looks for .agent-router.toml at repoRoot. If missing, returns defaults
// with ok=false.
//
// Reads the whole file at once rather than streaming with bufio.Scanner --
// the previous Scanner-based path inherited the default 64KB line cap and
// returned bufio.ErrTooLong on a long forbidden.paths entry or notes string,
// which then bubbled up to the orchestrator and failed the entire task.
// 30-line config files do not need streaming; one ReadFile is fine.
func Load(repoRoot string) (Config, bool, error) {
	path := filepath.Join(repoRoot, ".agent-router.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaults(), false, nil
		}
		return Config{}, false, err
	}

	cfg := defaults()
	cfg.Path = path

	var section string
	for i, raw := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.TrimSpace(stripComment(val))
		if err := apply(&cfg, section, key, val); err != nil {
			return cfg, true, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
	}
	return cfg, true, nil
}

func stripComment(s string) string {
	inStr := false
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			inStr = !inStr
		} else if s[i] == '#' && !inStr {
			return s[:i]
		}
	}
	return s
}

func apply(cfg *Config, section, key, val string) error {
	switch section {
	case "commands":
		s := unquote(val)
		switch key {
		case "test":
			cfg.Commands.Test = s
		case "compile":
			cfg.Commands.Compile = s
		case "lint":
			cfg.Commands.Lint = s
		}
	case "style":
		switch key {
		case "language":
			cfg.Style.Language = unquote(val)
		case "line_length":
			n, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("style.line_length: invalid int %q: %w", val, err)
			}
			if n < 0 {
				return fmt.Errorf("style.line_length must be >= 0, got %d", n)
			}
			cfg.Style.LineLength = n
		case "notes":
			cfg.Style.Notes = unquote(val)
		}
	case "forbidden":
		if key == "paths" {
			cfg.Forbidden.Paths = parseStringArray(val)
		}
	case "budget":
		switch key {
		case "max_runtime":
			d, err := parseDuration(unquote(val))
			if err != nil {
				return err
			}
			if d <= 0 {
				return fmt.Errorf("budget.max_runtime must be > 0, got %s", d)
			}
			cfg.Budget.MaxRuntime = d
		case "max_iterations":
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			if n <= 0 {
				return fmt.Errorf("budget.max_iterations must be > 0, got %d", n)
			}
			cfg.Budget.MaxIterations = n
		}
	}
	return nil
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// parseStringArray handles `["a", "b", "c"]` on a single line.
func parseStringArray(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, unquote(p))
	}
	return out
}

// parseDuration supports Go's time.ParseDuration plus `30m`, `2h`, `1d`.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// ForbiddenHit returns true if relPath matches any forbidden path.
//
// Matching is path-component aware: "sec" forbids the directory "sec/" and
// the literal file "sec", but does NOT match "secrets/" or "security_audit.md".
// The previous string-prefix implementation accidentally collapsed those
// distinctions and over-blocked legitimate paths. Trailing-separator
// normalization plus exact-equality covers both directory and file forbids.
func (c *Config) ForbiddenHit(relPath string) bool {
	// Normalise to forward slashes so config rules are portable across OSes.
	rel := filepath.ToSlash(filepath.Clean(relPath))
	for _, raw := range c.Forbidden.Paths {
		p := filepath.ToSlash(filepath.Clean(raw))
		if p == "" || p == "." {
			continue
		}
		if rel == p {
			return true
		}
		// Component boundary: relPath must continue with a separator after
		// the forbidden prefix. Plain HasPrefix("secrets", "sec") would
		// match incorrectly; HasPrefix("secrets/", "sec/") does not.
		if strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}
