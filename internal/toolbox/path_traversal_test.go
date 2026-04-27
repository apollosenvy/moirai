package toolbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aegis/moirai/internal/repoconfig"
)

// makeToolbox builds a Toolbox rooted at a fresh tempdir. We do NOT call
// EnsureRepo; the path-resolver tests only exercise resolveInsideRepo's
// in/out classification.
func makeToolbox(t *testing.T) (*Toolbox, string) {
	t.Helper()
	dir := t.TempDir()
	// EvalSymlinks the temp dir so the toolbox stores the canonical form.
	// On macOS /tmp is itself a symlink to /private/tmp; without this the
	// resolved-vs-stored prefix check would fail spuriously.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	tb, err := New(resolved, "test-branch", filepath.Join(t.TempDir(), "scratch"), repoconfig.Config{}, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tb, resolved
}

func TestResolveRejectsDotDotEscape(t *testing.T) {
	tb, _ := makeToolbox(t)
	_, err := tb.resolveInsideRepo("../etc/passwd")
	if err == nil {
		t.Fatal("expected error for ../ traversal, got nil")
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Errorf("expected outside-repo error, got: %v", err)
	}
}

func TestResolveRejectsAbsolutePath(t *testing.T) {
	tb, _ := makeToolbox(t)
	_, err := tb.resolveInsideRepo("/etc/passwd")
	if err == nil {
		t.Fatal("expected error for absolute outside path, got nil")
	}
}

func TestResolveAcceptsRelativeInside(t *testing.T) {
	tb, root := makeToolbox(t)
	abs, err := tb.resolveInsideRepo("subdir/file.txt")
	if err != nil {
		t.Fatalf("expected ok for in-repo path, got %v", err)
	}
	if !strings.HasPrefix(abs, root) {
		t.Errorf("resolved path %q not under root %q", abs, root)
	}
}

// TestResolveRejectsSymlinkEscapeOutside creates `linkout` inside the repo
// pointing at /tmp/<elsewhere>. fs.read of `linkout/file` should be rejected
// because the resolved target lives outside repoRoot.
func TestResolveRejectsSymlinkEscapeOutside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	tb, root := makeToolbox(t)

	// Create a target dir outside the repo.
	outside := t.TempDir()
	outsideReal, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatalf("EvalSymlinks outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outsideReal, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}

	// Symlink inside the repo pointing outside.
	linkPath := filepath.Join(root, "leak")
	if err := os.Symlink(outsideReal, linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// Read via the symlink should be rejected.
	_, err = tb.resolveInsideRepo("leak/secret.txt")
	if err == nil {
		t.Fatal("expected reject for symlinked-outside read, got nil")
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Errorf("expected outside-repo error, got: %v", err)
	}
}

// TestResolveAcceptsSymlinkInsideRepo: a symlink whose target ALSO lives
// inside the repo is fine.
func TestResolveAcceptsSymlinkInsideRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	tb, root := makeToolbox(t)

	target := filepath.Join(root, "inside-target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "ok.txt"), []byte("yes"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(root, "ok-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if _, err := tb.resolveInsideRepo("ok-link/ok.txt"); err != nil {
		t.Errorf("expected ok for in-repo symlink, got %v", err)
	}
}

// TestResolveAllowsCreateOfNewFile: fs.write target may not exist yet. The
// resolver must walk up to the nearest existing ancestor, EvalSymlinks that,
// and verify the still-non-existent leaf stays inside the repo.
func TestResolveAllowsCreateOfNewFile(t *testing.T) {
	tb, _ := makeToolbox(t)
	if _, err := tb.resolveInsideRepo("brand/new/file.txt"); err != nil {
		t.Errorf("expected ok for new-file path, got %v", err)
	}
}

// TestResolveRejectsCreateThroughSymlinkedAncestor: a symlinked DIRECTORY
// whose target is outside the repo must not be usable as a write parent.
func TestResolveRejectsCreateThroughSymlinkedAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	tb, root := makeToolbox(t)
	outside := t.TempDir()
	outsideReal, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := os.Symlink(outsideReal, filepath.Join(root, "evil")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	// Write target nested under the bad symlink.
	_, err = tb.resolveInsideRepo("evil/new-file.txt")
	if err == nil {
		t.Fatal("expected reject for write through symlinked-out ancestor, got nil")
	}
}

// TestResolveRejectsForbidden: the [forbidden].paths list is enforced too.
func TestResolveRejectsForbidden(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	tb, err := New(resolved, "test-branch", filepath.Join(t.TempDir(), "scratch"), repoconfig.Config{
		Forbidden: repoconfig.Forbidden{Paths: []string{"secrets"}},
	}, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tb.resolveInsideRepo("secrets/key.txt"); err == nil {
		t.Error("expected forbidden-path rejection")
	}
	// "sec" is NOT a prefix of "secrets" once we use component matching.
	if _, err := tb.resolveInsideRepo("section/foo.txt"); err != nil {
		t.Errorf("expected ok for component-distinct path, got %v", err)
	}
}
