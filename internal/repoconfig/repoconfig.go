// Package repoconfig parses .agent-router.toml files at repo roots.
//
// Minimal TOML-ish parser. We only support the subset the spec calls for
// ([commands], [style], [forbidden], [budget]) so we don't pull in a full
// TOML dependency just for 30 lines of config.
package repoconfig

import (
	"bufio"
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
func Load(repoRoot string) (Config, bool, error) {
	path := filepath.Join(repoRoot, ".agent-router.toml")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaults(), false, nil
		}
		return Config{}, false, err
	}
	defer f.Close()

	cfg := defaults()
	cfg.Path = path

	var section string
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
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
			return cfg, true, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := scan.Err(); err != nil {
		return cfg, true, err
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
			n, _ := strconv.Atoi(val)
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
			cfg.Budget.MaxRuntime = d
		case "max_iterations":
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
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

// ForbiddenHit returns true if candidate matches any forbidden path.
func (c *Config) ForbiddenHit(relPath string) bool {
	for _, p := range c.Forbidden.Paths {
		if strings.HasPrefix(relPath, p) {
			return true
		}
	}
	return false
}
