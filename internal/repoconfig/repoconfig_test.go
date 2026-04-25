package repoconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".agent-router.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, ok, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ok {
		t.Error("expected ok=false for missing file")
	}
	if cfg.Budget.MaxRuntime != 30*time.Minute {
		t.Errorf("expected default MaxRuntime=30m, got %v", cfg.Budget.MaxRuntime)
	}
}

func TestLoadParsesAllSections(t *testing.T) {
	body := `# A comment
[commands]
test = "go test ./..."
compile = "go build ./..."
lint = "golangci-lint run"

[style]
language = "go"
line_length = 100
notes = "tabs not spaces"

[forbidden]
paths = ["secrets", "vendor", "node_modules"]

[budget]
max_runtime = "45m"
max_iterations = 10
`
	dir := writeConfig(t, body)
	cfg, ok, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Error("expected ok=true")
	}
	if cfg.Commands.Test != "go test ./..." {
		t.Errorf("Test: %q", cfg.Commands.Test)
	}
	if cfg.Style.LineLength != 100 {
		t.Errorf("LineLength: %d", cfg.Style.LineLength)
	}
	if cfg.Budget.MaxRuntime != 45*time.Minute {
		t.Errorf("MaxRuntime: %v", cfg.Budget.MaxRuntime)
	}
	if cfg.Budget.MaxIterations != 10 {
		t.Errorf("MaxIterations: %d", cfg.Budget.MaxIterations)
	}
	if len(cfg.Forbidden.Paths) != 3 {
		t.Errorf("Forbidden paths: %v", cfg.Forbidden.Paths)
	}
}

func TestLoadHandlesLongLine(t *testing.T) {
	// Make a forbidden.paths line >64KB to confirm we don't hit
	// bufio.Scanner's default buffer.
	var entries []string
	for i := 0; i < 8000; i++ {
		entries = append(entries, `"x"`)
	}
	body := "[forbidden]\npaths = [" + strings.Join(entries, ", ") + "]\n"
	dir := writeConfig(t, body)
	cfg, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load with long line: %v", err)
	}
	if len(cfg.Forbidden.Paths) != 8000 {
		t.Errorf("expected 8000 paths, got %d", len(cfg.Forbidden.Paths))
	}
}

func TestLoadMalformedDurationReturnsError(t *testing.T) {
	body := "[budget]\nmax_runtime = \"not-a-duration\"\n"
	dir := writeConfig(t, body)
	if _, _, err := Load(dir); err == nil {
		t.Error("expected error for malformed duration")
	}
}

func TestLoadAllowsMissingOptionalFields(t *testing.T) {
	body := "[commands]\ntest = \"go test\"\n"
	dir := writeConfig(t, body)
	cfg, ok, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Error("expected ok=true")
	}
	if cfg.Commands.Test != "go test" {
		t.Errorf("Test: %q", cfg.Commands.Test)
	}
	// Defaults still apply.
	if cfg.Budget.MaxRuntime != 30*time.Minute {
		t.Errorf("default MaxRuntime missing")
	}
}

func TestForbiddenHitMatchesExactPath(t *testing.T) {
	cfg := &Config{Forbidden: Forbidden{Paths: []string{"secrets"}}}
	if !cfg.ForbiddenHit("secrets") {
		t.Error("expected hit on exact match")
	}
	if !cfg.ForbiddenHit("secrets/key.pem") {
		t.Error("expected hit on subpath of forbidden dir")
	}
}

func TestForbiddenHitDoesNotMatchAcrossComponents(t *testing.T) {
	cfg := &Config{Forbidden: Forbidden{Paths: []string{"sec"}}}
	if cfg.ForbiddenHit("secrets/foo") {
		t.Error("'sec' must not match 'secrets/foo'")
	}
	if cfg.ForbiddenHit("security_audit.md") {
		t.Error("'sec' must not match 'security_audit.md'")
	}
	if !cfg.ForbiddenHit("sec") {
		t.Error("'sec' should still match the exact directory name 'sec'")
	}
	if !cfg.ForbiddenHit("sec/note.txt") {
		t.Error("'sec' should match 'sec/note.txt'")
	}
}

func TestForbiddenHitHandlesEmptyConfig(t *testing.T) {
	cfg := &Config{}
	if cfg.ForbiddenHit("anything") {
		t.Error("empty config should never hit")
	}
}

// TestLoadRejectsNegativeBudgets covers pass-3 EDGE-5: a negative or zero
// max_runtime / max_iterations would deadline a task instantly. The loader
// must surface a parse error with the offending line number.
func TestLoadRejectsNegativeBudgets(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"[budget]\nmax_runtime = \"-30m\"\n", "max_runtime"},
		{"[budget]\nmax_runtime = \"0s\"\n", "max_runtime"},
		{"[budget]\nmax_iterations = -1\n", "max_iterations"},
		{"[budget]\nmax_iterations = 0\n", "max_iterations"},
	}
	for _, tc := range cases {
		dir := writeConfig(t, tc.body)
		_, _, err := Load(dir)
		if err == nil {
			t.Errorf("body=%q expected error, got nil", tc.body)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("body=%q error %q should mention %q", tc.body, err, tc.want)
		}
		// Error must include a line number prefix from the path: format.
		if !strings.Contains(err.Error(), ":2:") && !strings.Contains(err.Error(), ":") {
			t.Errorf("body=%q expected error to include line number, got %q", tc.body, err)
		}
	}
}

// TestLoadRejectsBadIntInStyle covers EDGE-4: bad int in [style]/line_length
// used to silently zero out the field. Now it surfaces an error.
func TestLoadRejectsBadIntInStyle(t *testing.T) {
	dir := writeConfig(t, "[style]\nline_length = abc\n")
	_, _, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for non-numeric line_length, got nil")
	}
	if !strings.Contains(err.Error(), "line_length") {
		t.Errorf("error %q should mention line_length", err)
	}
}

// TestLoadErrorIncludesLineNumber confirms the loader threads the line
// number into the error path so operators can pinpoint the offending row.
func TestLoadErrorIncludesLineNumber(t *testing.T) {
	body := "# header comment\n# second comment\n[budget]\nmax_iterations = -5\n"
	dir := writeConfig(t, body)
	_, _, err := Load(dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), ":4:") {
		t.Errorf("expected error to mention line 4, got %q", err)
	}
}
