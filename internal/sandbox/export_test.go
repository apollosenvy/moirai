package sandbox

import (
	"path/filepath"
	"testing"
)

// buildArgvForTest reproduces the bwrap argv that bwrapExec would
// construct for the given policy and argv. Used by tests in this package
// to assert flag presence without spawning the bwrap process.
//
// Keep in sync with bwrapExec; if a regression slips into the production
// path that doesn't break this synthesis, the integration test
// TestBwrapArgvStructure (which actually runs bwrap) will catch it.
func buildArgvForTest(t *testing.T, pol Policy, argv []string) []string {
	t.Helper()
	bwArgs := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-uts",
		"--unshare-ipc",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/etc", "/etc",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/sbin", "/sbin",
		"--ro-bind-try", "/opt", "/opt",
	}
	if !pol.AllowNetwork {
		bwArgs = append(bwArgs, "--unshare-net")
	}
	if pol.RepoRoot != "" {
		abs, _ := filepath.Abs(pol.RepoRoot)
		bwArgs = append(bwArgs, "--bind", abs, abs, "--chdir", abs)
	}
	if pol.ScratchDir != "" {
		abs, _ := filepath.Abs(pol.ScratchDir)
		bwArgs = append(bwArgs, "--bind", abs, abs)
	}
	bwArgs = append(bwArgs, "--")
	bwArgs = append(bwArgs, argv...)
	return bwArgs
}
