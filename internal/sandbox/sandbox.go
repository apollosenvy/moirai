// Package sandbox wraps command execution in bwrap.
//
// Default policy: network off, writes confined to the repo root plus a small
// scratch dir, /usr and /etc read-only bind-mounted, /tmp isolated. If bwrap
// is missing the sandbox falls back to an unsandboxed exec with a loud
// warning on stderr (for dev use only).
package sandbox

import (
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
	// OutputCap is the per-stream byte limit on captured stdout/stderr.
	// 0 selects the default (defaultOutputCap). The cap matters because
	// the sandbox is the choke-point between an LLM-controlled bash command
	// and the orchestrator's prompt buffer: an unbounded `cat large_file`
	// can blow past the reviewer model's context window in a single tool
	// result. Set to a negative value to disable capping (tests only).
	OutputCap int
}

// defaultOutputCap is 4 KiB per stream. Calibrated against the v3.2
// Causal Canvas run, which hit ctx overflow at turn 16 with cumulative
// bash result accumulation (16 reviewer bash calls @ 16 KiB each = 256
// KiB of stale bash output stacking up beyond the rolling-window
// compactor's reach). 4 KiB stdout + 4 KiB stderr = 8 KiB per call,
// half the v2 size. Enough for failing-test output (a typical Go test
// failure summary is 1-2 KiB) but tight enough that 16 invocations
// fit in the reviewer's 32K context window with room for the system
// prompt + plan + checklist + rolling ask_coder history.
const defaultOutputCap = 4 * 1024

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Sandbox  string // "bwrap" or "passthrough"
	CmdLine  string
	// Truncated is true when stdout or stderr was clipped at OutputCap.
	// The Stdout/Stderr strings carry a "[truncated N bytes]" tail marker
	// when this flag is set so the model sees the truncation in-band.
	Truncated bool
}

// Exec runs argv under bwrap with the given policy. stdin is /dev/null.
func Exec(ctx context.Context, pol Policy, argv []string) (*Result, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("sandbox: empty argv")
	}
	if pol.Timeout == 0 {
		pol.Timeout = 5 * time.Minute
	}
	if pol.OutputCap == 0 {
		pol.OutputCap = defaultOutputCap
	}
	runCtx, cancel := context.WithTimeout(ctx, pol.Timeout)
	defer cancel()

	if _, err := exec.LookPath("bwrap"); err != nil {
		return passthrough(runCtx, pol, argv)
	}
	return bwrapExec(runCtx, pol, argv)
}

// cappingWriter implements io.Writer with a per-instance byte cap. Once the
// cap is exceeded all further writes are silently dropped; total tracks the
// real volume so the truncation tail can report how many bytes were lost.
//
// We accept the writer interface to plug into exec.Cmd.Stdout/Stderr cleanly.
// Note: a process can still write more than `cap` to its pipe -- the OS pipe
// buffer absorbs that. We only cap what the parent ingests.
type cappingWriter struct {
	cap   int
	buf   []byte
	total int
}

func newCappingWriter(cap int) *cappingWriter {
	return &cappingWriter{cap: cap, buf: make([]byte, 0, min(cap, 4096))}
}

func (c *cappingWriter) Write(p []byte) (int, error) {
	c.total += len(p)
	remaining := c.cap - len(c.buf)
	if remaining <= 0 {
		// Already at cap; pretend we accepted the bytes so the child
		// process doesn't get a SIGPIPE and abort early.
		return len(p), nil
	}
	if len(p) <= remaining {
		c.buf = append(c.buf, p...)
		return len(p), nil
	}
	c.buf = append(c.buf, p[:remaining]...)
	return len(p), nil
}

// String returns the captured bytes, with a "[truncated N bytes]" tail when
// the underlying capacity was exceeded.
func (c *cappingWriter) String() string {
	if c.total <= c.cap {
		return string(c.buf)
	}
	return string(c.buf) + fmt.Sprintf("\n... [truncated %d bytes]", c.total-c.cap)
}

// Truncated reports whether any bytes were dropped.
func (c *cappingWriter) Truncated() bool {
	return c.total > c.cap
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
		// /lib and /lib64 are symlinks into /usr/lib on usr-merged distros
		// (Arch, modern Fedora/Debian, Alpine, NixOS). Use --ro-bind-try so
		// the sandbox starts cleanly on hosts where these paths don't exist
		// as standalone directories.
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/sbin", "/sbin",
		"--ro-bind-try", "/opt", "/opt",
	}

	if !pol.AllowNetwork {
		bwArgs = append(bwArgs, "--unshare-net")
	}

	if pol.RepoRoot != "" {
		abs, err := filepath.Abs(pol.RepoRoot)
		if err != nil {
			return nil, fmt.Errorf("sandbox: abs repo root %q: %w", pol.RepoRoot, err)
		}
		bwArgs = append(bwArgs, "--bind", abs, abs, "--chdir", abs)
	}
	if pol.ScratchDir != "" {
		abs, err := filepath.Abs(pol.ScratchDir)
		if err != nil {
			return nil, fmt.Errorf("sandbox: abs scratch dir %q: %w", pol.ScratchDir, err)
		}
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
	stdout := newCappingWriter(pol.OutputCap)
	stderr := newCappingWriter(pol.OutputCap)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	res := &Result{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Sandbox:   "bwrap",
		CmdLine:   strings.Join(argv, " "),
		Truncated: stdout.Truncated() || stderr.Truncated(),
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
	stdout := newCappingWriter(pol.OutputCap)
	stderr := newCappingWriter(pol.OutputCap)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	res := &Result{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Sandbox:   "passthrough",
		CmdLine:   strings.Join(argv, " "),
		Truncated: stdout.Truncated() || stderr.Truncated(),
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
