// Package sandbox wraps command execution in bwrap.
//
// Default policy: network off, writes confined to the repo root plus a small
// scratch dir, /usr and /etc read-only bind-mounted, /tmp isolated. If bwrap
// is missing the sandbox falls back to an unsandboxed exec with a loud
// warning on stderr (for dev use only).
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Policy struct {
	RepoRoot      string
	ScratchDir    string
	AllowNetwork  bool
	AllowHomeRead bool
	ExtraRO       []string // additional paths to read-only bind
	ExtraRW       []string // additional paths to rw bind
	Timeout       time.Duration
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Sandbox  string // "bwrap" or "passthrough"
	CmdLine  string
}

// Exec runs argv under bwrap with the given policy. stdin is /dev/null.
func Exec(ctx context.Context, pol Policy, argv []string) (*Result, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("sandbox: empty argv")
	}
	if pol.Timeout == 0 {
		pol.Timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, pol.Timeout)
	defer cancel()

	if _, err := exec.LookPath("bwrap"); err != nil {
		return passthrough(runCtx, pol, argv)
	}
	return bwrapExec(runCtx, pol, argv)
}

func bwrapExec(ctx context.Context, pol Policy, argv []string) (*Result, error) {
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
		_ = os.MkdirAll(abs, 0o755)
		bwArgs = append(bwArgs, "--bind", abs, abs)
	}
	if pol.AllowHomeRead {
		if home, err := os.UserHomeDir(); err == nil {
			bwArgs = append(bwArgs, "--ro-bind", home, home)
		}
	}
	for _, p := range pol.ExtraRO {
		if _, err := os.Stat(p); err == nil {
			bwArgs = append(bwArgs, "--ro-bind", p, p)
		}
	}
	for _, p := range pol.ExtraRW {
		if _, err := os.Stat(p); err == nil {
			bwArgs = append(bwArgs, "--bind", p, p)
		}
	}

	bwArgs = append(bwArgs, "--")
	bwArgs = append(bwArgs, argv...)

	cmd := exec.CommandContext(ctx, "bwrap", bwArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := &Result{
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
		Sandbox: "bwrap",
		CmdLine: strings.Join(argv, " "),
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	if err != nil && !res.TimedOut {
		return res, err
	}
	return res, nil
}

func passthrough(ctx context.Context, pol Policy, argv []string) (*Result, error) {
	fmt.Fprintln(os.Stderr, "sandbox: bwrap not found, running command unsandboxed")
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if pol.RepoRoot != "" {
		cmd.Dir = pol.RepoRoot
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := &Result{
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
		Sandbox: "passthrough",
		CmdLine: strings.Join(argv, " "),
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	if err != nil && !res.TimedOut {
		return res, err
	}
	return res, nil
}
