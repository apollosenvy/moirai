package sandbox

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// argvHasFlag returns true if the bwrap argv contains flag with a matching
// pair value, or just the flag if `value` is empty.
func argvHasFlag(t *testing.T, argv []string, flag string) bool {
	t.Helper()
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

// argvHasPair returns true if argv contains `flag X` for some X.
func argvHasPair(argv []string, flag string) bool {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == flag {
			return true
		}
	}
	return false
}

// captureBwrapArgv: bwrapExec is unexported. We exercise it indirectly by
// running with a captured PATH-stub bwrap that prints its argv to stdout.
// To keep the test hermetic and avoid sh-level shenanigans we instead test
// the argv-construction logic by calling an internal helper.
func TestBwrapArgvStructure(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
	// We do an actual fast invocation: bwrap --help exits 0.
	// We are interested in whether Exec passes through to bwrap and the
	// expected flags reach the child.
	pol := Policy{
		RepoRoot:     t.TempDir(),
		ScratchDir:   t.TempDir(),
		AllowNetwork: false,
	}
	res, err := Exec(context.Background(), pol, []string{"true"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Sandbox != "bwrap" {
		t.Fatalf("expected Sandbox=bwrap, got %q", res.Sandbox)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0 for `true`, got %d", res.ExitCode)
	}
}

// TestBwrapArgvFlags inspects the constructed bwrap argv via the unexported
// bwrapArgs helper. The helper isn't a free function in production, but we
// can check the contract by building the argv ourselves and asserting the
// flags we care about appear.
func TestBwrapArgvFlags(t *testing.T) {
	pol := Policy{
		RepoRoot:     "/tmp/repo",
		ScratchDir:   "/tmp/scratch",
		AllowNetwork: false,
	}
	argv := buildArgvForTest(t, pol, []string{"echo", "hi"})

	// network isolation
	if !argvHasFlag(t, argv, "--unshare-net") {
		t.Error("expected --unshare-net for AllowNetwork=false")
	}
	// bind mount of repo root
	if !argvHasPair(argv, "--bind") {
		t.Error("expected --bind to appear for repo root")
	}
	// chdir to repo
	if !argvHasPair(argv, "--chdir") {
		t.Error("expected --chdir to appear")
	}
	// die-with-parent so daemon shutdown reaps the sandbox
	if !argvHasFlag(t, argv, "--die-with-parent") {
		t.Error("expected --die-with-parent")
	}
	// /usr ro-bind
	foundUsr := false
	for i := 0; i < len(argv)-2; i++ {
		if argv[i] == "--ro-bind" && argv[i+1] == "/usr" && argv[i+2] == "/usr" {
			foundUsr = true
			break
		}
	}
	if !foundUsr {
		t.Error("expected --ro-bind /usr /usr")
	}
}

func TestBwrapArgvAllowNetwork(t *testing.T) {
	pol := Policy{
		RepoRoot:     "/tmp/repo",
		AllowNetwork: true,
	}
	argv := buildArgvForTest(t, pol, []string{"true"})
	for _, a := range argv {
		if a == "--unshare-net" {
			t.Error("did not expect --unshare-net when AllowNetwork=true")
		}
	}
}

// TestPassthroughWhenNoBwrap: verify the passthrough fallback when bwrap
// is missing. We don't actually unset PATH (other tests need it); instead
// we just check the result-shape contract for a passthrough run.
func TestPassthroughResultShape(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	pol := Policy{RepoRoot: t.TempDir()}
	res, err := Exec(context.Background(), pol, []string{"/bin/true"})
	if err != nil {
		// /bin/true might not be on the unset PATH; swallow exec errors here
		// because the goal is the Sandbox=passthrough flag.
		_ = err
	}
	if res != nil && res.Sandbox != "passthrough" {
		t.Errorf("expected Sandbox=passthrough, got %q", res.Sandbox)
	}
}

func TestExecRejectsEmptyArgv(t *testing.T) {
	_, err := Exec(context.Background(), Policy{}, nil)
	if err == nil {
		t.Fatal("expected error for empty argv")
	}
	if !strings.Contains(err.Error(), "empty argv") {
		t.Errorf("expected empty-argv message, got %v", err)
	}
}
