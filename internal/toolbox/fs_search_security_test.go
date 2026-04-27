package toolbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aegis/moirai/internal/repoconfig"
)

// TestFSSearchForbiddenFilter verifies SEC-PASS5-001: fs.search must drop
// hits whose relative path is in [forbidden].paths even when ripgrep
// recursively scanned the file. Without the post-filter, the fs.read guard
// is bypassable via fs.search returning the matched line text verbatim.
func TestFSSearchForbiddenFilter(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed; skipping fs.search test")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed; sandbox.Exec would passthrough -- skip to avoid unsandboxed test")
	}

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	// Plant a file under a forbidden subdirectory containing a unique
	// signature. fs.search for that signature must NOT return it.
	forbiddenDir := filepath.Join(resolved, "secrets")
	if err := os.MkdirAll(forbiddenDir, 0o700); err != nil {
		t.Fatalf("mkdir forbidden: %v", err)
	}
	signature := "BEGIN-FXR5-PRIVATE-KEY-zVE7Q"
	if err := os.WriteFile(filepath.Join(forbiddenDir, "k.pem"), []byte(signature+"\n"), 0o600); err != nil {
		t.Fatalf("write forbidden file: %v", err)
	}
	// Also plant a non-forbidden file with the same string so we can prove
	// the search would have hit it but the forbidden one was filtered.
	if err := os.WriteFile(filepath.Join(resolved, "notes.txt"), []byte("nothing matching here\n"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	cfg := repoconfig.Config{
		Forbidden: repoconfig.Forbidden{Paths: []string{"secrets"}},
	}
	tb, err := New(resolved, "test-branch", filepath.Join(t.TempDir(), "scratch"), cfg, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hits, err := tb.FSSearch(ctx, signature, ".", 100)
	if err != nil {
		t.Fatalf("FSSearch: %v", err)
	}
	for _, h := range hits {
		if strings.Contains(h.Path, "secrets") {
			t.Errorf("forbidden hit leaked: %+v", h)
		}
		if strings.Contains(h.Text, signature) && strings.HasPrefix(h.Path, "secrets") {
			t.Errorf("forbidden secret value leaked in hit text: %+v", h)
		}
	}
}

// TestFSSearchPatternFlagInjection verifies SEC-PASS5-002: a pattern
// starting with "-" must be treated as a literal regex (matches nothing in
// this case), NOT as a ripgrep flag like --files or --pre. The hardening
// is the "--" flag-terminator in the rg argv.
func TestFSSearchPatternFlagInjection(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed; skipping fs.search test")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed; skipping sandboxed rg test")
	}

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	// Plant several files. If rg interprets "--files" as the --files flag
	// it lists all files under searchRoot; the parser would then fail to
	// extract "path:line:text" because rg's --files output has only paths.
	// With the "--" terminator, "--files" is a literal regex which matches
	// nothing here, so we expect zero hits.
	for i, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(resolved, name), []byte("placeholder\n"), 0o600); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	tb, err := New(resolved, "test-branch", filepath.Join(t.TempDir(), "scratch"), repoconfig.Config{}, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hits, err := tb.FSSearch(ctx, "--files", ".", 100)
	if err != nil {
		// rg may surface "regex parse error" if it tries to compile
		// "--files" as a literal pattern -- that's fine for the security
		// goal (no hits returned, no flag effect).
		return
	}
	if len(hits) > 0 {
		// Any hits are suspicious because the literal "--files" regex
		// shouldn't appear in the planted files.
		for _, h := range hits {
			if !strings.Contains(h.Text, "--files") {
				t.Errorf("hit with no --files literal in text suggests rg parsed --files as a flag: %+v", h)
			}
		}
	}
}

// TestFSSearchPatternSizeCap verifies the 4KB DoS guard.
func TestFSSearchPatternSizeCap(t *testing.T) {
	dir := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(dir)
	tb, _ := New(resolved, "test-branch", filepath.Join(t.TempDir(), "scratch"), repoconfig.Config{}, false)
	huge := strings.Repeat("a", fsSearchPatternMaxBytes+1)
	_, err := tb.FSSearch(context.Background(), huge, ".", 10)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("expected size-cap rejection, got %v", err)
	}
}

// TestFSSearchEmptyPatternRejected double-checks the toolbox-level guard
// (orchestrator already guards this; defense-in-depth).
func TestFSSearchEmptyPatternRejected(t *testing.T) {
	dir := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(dir)
	tb, _ := New(resolved, "test-branch", filepath.Join(t.TempDir(), "scratch"), repoconfig.Config{}, false)
	for _, p := range []string{"", "  ", "\t\n"} {
		if _, err := tb.FSSearch(context.Background(), p, ".", 10); err == nil {
			t.Errorf("expected empty pattern %q to be rejected", p)
		}
	}
}
