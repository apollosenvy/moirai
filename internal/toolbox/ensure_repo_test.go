package toolbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aegis/moirai/internal/repoconfig"
)

// TestEnsureRepoInitsFreshDir exercises the "A/B parity with Forge" code
// path: user points at a directory that was never `git init`'d, orchestrator
// should turn it into a valid repo with an initial commit on its own.
func TestEnsureRepoInitsFreshDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Isolate from user gpgsign / commit.gpgsign etc that would otherwise
	// make git commit fail in restricted test environments.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := t.TempDir()
	// Seed a file to ensure it gets captured in the initial commit.
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tb, err := New(dir, "moirai/task-x", t.TempDir(), repoconfig.Config{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.EnsureRepo(context.Background()); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	// Verify .git exists.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf(".git missing after EnsureRepo: %v", err)
	}
	// Verify HEAD resolves (commit landed).
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("HEAD rev-parse failed: %v", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatal("empty HEAD after EnsureRepo")
	}
	// Verify the seeded file is tracked.
	ls, _ := exec.Command("git", "-C", dir, "ls-files").Output()
	if !strings.Contains(string(ls), "hello.txt") {
		t.Errorf("hello.txt not tracked: %q", ls)
	}
}

// TestEnsureRepoIdempotent confirms calling EnsureRepo on an already-init'd
// repo is a no-op and doesn't churn the commit log.
func TestEnsureRepoIdempotent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := t.TempDir()

	// Pre-initialize.
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"commit", "--allow-empty", "-m", "pre-existing"},
	} {
		if err := exec.Command("git", append([]string{"-C", dir}, args...)...).Run(); err != nil {
			t.Fatalf("pre-init %v: %v", args, err)
		}
	}
	before, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()

	tb, _ := New(dir, "moirai/task-y", t.TempDir(), repoconfig.Config{}, false)
	if err := tb.EnsureRepo(context.Background()); err != nil {
		t.Fatalf("EnsureRepo on existing repo: %v", err)
	}

	after, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if string(before) != string(after) {
		t.Errorf("HEAD moved on idempotent call: %q -> %q", before, after)
	}
}

// TestEnsureRepoPreservesExistingIdentity confirms EnsureRepo never stomps
// on a pre-configured user.email. Seeds a fresh .git with user.email set,
// then runs EnsureRepo and asserts the email survives.
func TestEnsureRepoPreservesExistingIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := t.TempDir()

	// Pre-init with a real identity.
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test-user"},
	} {
		if err := exec.Command("git", append([]string{"-C", dir}, args...)...).Run(); err != nil {
			t.Fatalf("pre-init %v: %v", args, err)
		}
	}
	// Remove .git to simulate the "fresh dir" path, but keep memory of what
	// an EnsureRepo run would have configured. We re-init and set identity
	// ourselves before calling EnsureRepo to verify the ensure path doesn't
	// clobber an existing user.email.
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "init").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "config", "user.email", "test@example.com").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "config", "user.name", "test-user").Run(); err != nil {
		t.Fatal(err)
	}

	tb, err := New(dir, "moirai/task-z", t.TempDir(), repoconfig.Config{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.EnsureRepo(context.Background()); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	out, _ := exec.Command("git", "-C", dir, "config", "user.email").Output()
	if strings.TrimSpace(string(out)) != "test@example.com" {
		t.Fatalf("user.email stomped; got %q want test@example.com", strings.TrimSpace(string(out)))
	}
	nameOut, _ := exec.Command("git", "-C", dir, "config", "user.name").Output()
	if strings.TrimSpace(string(nameOut)) != "test-user" {
		t.Fatalf("user.name stomped; got %q want test-user", strings.TrimSpace(string(nameOut)))
	}
}

// TestEnsureRepoEmptyDir confirms an entirely empty dir still produces a
// valid HEAD via --allow-empty so subsequent branch checkouts succeed.
func TestEnsureRepoEmptyDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := t.TempDir()

	tb, err := New(dir, "moirai/task-empty", t.TempDir(), repoconfig.Config{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.EnsureRepo(context.Background()); err != nil {
		t.Fatalf("EnsureRepo on empty dir: %v", err)
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("HEAD rev-parse failed on empty-dir repo: %v", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatal("empty HEAD after EnsureRepo on empty dir")
	}
}
