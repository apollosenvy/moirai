// Package toolbox exposes the coder-side tool set: fs/git/test/compile/shell.
//
// Every operation is scoped to a repo root. File writes are diff-tracked
// through a local git branch. Shell commands route through the sandbox.
// Forbidden paths from .agent-router.toml are enforced here, not just as a
// suggestion to the model.
// TODO(rename): consider migrating to .moirai.toml in a future commit;
// orphaning existing per-repo config files in production today.
package toolbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aegis/moirai/internal/repoconfig"
	"github.com/aegis/moirai/internal/sandbox"
)

type Toolbox struct {
	RepoRoot     string
	Branch       string // the working branch (moirai/task-<id>)
	Cfg          repoconfig.Config
	ScratchDir   string
	AllowNetwork bool
}

// New returns a toolbox rooted at the given absolute repo path.
func New(repoRoot, branch, scratchDir string, cfg repoconfig.Config, allowNet bool) (*Toolbox, error) {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("toolbox: repo root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("toolbox: repo root is not a directory: %s", abs)
	}
	return &Toolbox{
		RepoRoot:     abs,
		Branch:       branch,
		Cfg:          cfg,
		ScratchDir:   scratchDir,
		AllowNetwork: allowNet,
	}, nil
}

// --- Path guard ---------------------------------------------------------

// resolveInsideRepo enforces the in-repo invariant for fs.read / fs.write /
// fs.search. Symlink-aware: a symlink inside the repo pointing outside the
// repo (e.g. `ln -s /etc /repo/leak`) used to pass the prefix check because
// only the literal string was compared. We now resolve the real on-disk
// target via filepath.EvalSymlinks before re-applying the prefix test.
//
// For fs.write to a non-existing path we walk up to the nearest existing
// ancestor, EvalSymlinks that, then rejoin the still-non-existent tail.
// This catches the case where a malicious symlink lives at an intermediate
// directory along the write target path.
//
// TOCTOU note (SEC-PASS5-006): there is a window between the EvalSymlinks
// resolution here and the actual os.ReadFile/os.WriteFile that re-traverses
// the path. A concurrent process replacing a leaf with a symlink in that
// window would have the open follow the new target. The strict fix is
// openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS) which removes the race
// entirely, but it requires reworking every os.* file op below. The daemon
// is local-only, single-process, with one model active at a time, so the
// practical exploit surface is small. Documented as accepted residual
// risk; revisit if multi-tenant or concurrent-task-on-same-repo scenarios
// land.
func (t *Toolbox) resolveInsideRepo(relOrAbs string) (string, error) {
	var abs string
	if filepath.IsAbs(relOrAbs) {
		abs = filepath.Clean(relOrAbs)
	} else {
		abs = filepath.Join(t.RepoRoot, relOrAbs)
	}
	absClean := filepath.Clean(abs)

	// Resolve the canonical repo root once. The Toolbox stores RepoRoot as
	// an already-Abs'd path (see New), but if the repo root itself is a
	// symlink we want to compare against its real target.
	rootReal, err := filepath.EvalSymlinks(t.RepoRoot)
	if err != nil {
		// If even the repo root cannot be resolved (deleted between Toolbox
		// construction and use), fall back to the literal path so the
		// downstream prefix check still catches obvious traversal.
		rootReal = t.RepoRoot
	}
	rootReal = filepath.Clean(rootReal)

	// Resolve the candidate path. If it doesn't exist (legitimate fs.write
	// of a new file), walk up to the nearest existing ancestor, resolve
	// that, and rejoin the unresolved tail so the prefix check still has
	// real bytes to inspect.
	resolved, err := evalSymlinksAllowMissing(absClean)
	if err != nil {
		return "", fmt.Errorf("toolbox: resolve %q: %w", relOrAbs, err)
	}

	if !strings.HasPrefix(resolved+string(os.PathSeparator), rootReal+string(os.PathSeparator)) && resolved != rootReal {
		return "", fmt.Errorf("toolbox: path %q outside repo root", relOrAbs)
	}
	rel, err := filepath.Rel(rootReal, resolved)
	if err != nil {
		return "", err
	}
	if t.Cfg.ForbiddenHit(rel) {
		return "", fmt.Errorf("toolbox: path %q is in [forbidden].paths", rel)
	}
	// Return the resolved path so subsequent os.ReadFile / os.WriteFile
	// operate on the canonical target rather than re-following symlinks
	// (a TOCTOU window between this check and the open is still possible
	// but the attack surface is dramatically reduced).
	return resolved, nil
}

// evalSymlinksAllowMissing is filepath.EvalSymlinks with a non-existent-tail
// fallback: walk up until we find an ancestor that exists, resolve it, then
// rejoin the un-walked components. Useful for fs.write of new files where
// the leaf path won't resolve but we still need to vet the directory chain.
func evalSymlinksAllowMissing(p string) (string, error) {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(real), nil
	}
	dir := p
	tail := ""
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root without finding an existing ancestor.
			// Just clean the original path; the prefix check will reject it
			// if it doesn't sit under the repo root.
			return filepath.Clean(p), nil
		}
		base := filepath.Base(dir)
		if tail == "" {
			tail = base
		} else {
			tail = filepath.Join(base, tail)
		}
		dir = parent
		real, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Clean(filepath.Join(real, tail)), nil
		}
	}
}

// --- fs tools -----------------------------------------------------------

type FSReadResult struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Bytes   int    `json:"bytes"`
}

func (t *Toolbox) FSRead(path string, maxBytes int) (*FSReadResult, error) {
	abs, err := t.resolveInsideRepo(path)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		data = append(data[:maxBytes], []byte(fmt.Sprintf("\n... [truncated %d bytes]", len(data)-maxBytes))...)
	}
	rel, _ := filepath.Rel(t.RepoRoot, abs)
	return &FSReadResult{Path: rel, Content: string(data), Bytes: len(data)}, nil
}

type FSWriteResult struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	Created bool   `json:"created"`
}

func (t *Toolbox) FSWrite(path, content string) (*FSWriteResult, error) {
	abs, err := t.resolveInsideRepo(path)
	if err != nil {
		return nil, err
	}
	created := false
	if _, err := os.Stat(abs); os.IsNotExist(err) {
		created = true
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, err
	}
	// Repo files: model-authored content lands in a user-owned working tree.
	// Default umask handles user/group, but pass 0o644 explicitly because
	// these are repo source files (publicly readable by intent), distinct
	// from daemon state files which use 0o600.
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(t.RepoRoot, abs)
	return &FSWriteResult{Path: rel, Bytes: len(content), Created: created}, nil
}

type FSSearchHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// fsSearchPatternMaxBytes caps the size of a model-emitted ripgrep pattern.
// 4 KiB is generous for any legitimate regex; larger inputs are either
// hallucinated payload dumps or DoS attempts at the rg parser.
const fsSearchPatternMaxBytes = 4096

func (t *Toolbox) FSSearch(ctx context.Context, pattern, path string, maxHits int) ([]FSSearchHit, error) {
	// Reject empty patterns up-front. Orchestrator already guards this
	// (TestFSSearchEmptyPatternRejected) but defense-in-depth: the toolbox
	// should never ship an empty positional to rg.
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("fs.search: empty pattern")
	}
	if len(pattern) > fsSearchPatternMaxBytes {
		return nil, fmt.Errorf("fs.search: pattern exceeds %d-byte cap (got %d)", fsSearchPatternMaxBytes, len(pattern))
	}
	searchRoot, err := t.resolveInsideRepo(path)
	if err != nil {
		return nil, err
	}
	if maxHits <= 0 {
		maxHits = 100
	}
	// SEC-PASS5-002: insert "--" before the model-controlled pattern so that a
	// pattern starting with "-" (e.g. "--pre=/bin/sh", "--files",
	// "--search-zip") is treated as a literal regex by ripgrep, not as a
	// flag. Without the terminator, model-controlled flags reach rg's parser
	// unchecked.
	args := []string{"--no-heading", "--line-number", "--color=never", "--max-count", "5", "--", pattern, searchRoot}
	res, err := sandbox.Exec(ctx, sandbox.Policy{
		RepoRoot:     t.RepoRoot,
		ScratchDir:   t.ScratchDir,
		AllowNetwork: false,
	}, append([]string{"rg"}, args...))
	if err != nil {
		return nil, err
	}
	// rg returns 1 when no matches; both are fine.
	//
	// SEC-PASS5-001: ripgrep walks the search root recursively and has no
	// awareness of the [forbidden].paths list in .agent-router.toml (TODO(rename): .moirai.toml). Hits
	// from forbidden subdirectories must be dropped here, post-parse, even
	// though resolveInsideRepo already vetted the search root itself. A
	// pattern like "BEGIN PRIVATE KEY" rooted at "." would otherwise
	// exfiltrate secrets the model is forbidden from reading via fs.read.
	rootReal, rrErr := filepath.EvalSymlinks(t.RepoRoot)
	if rrErr != nil {
		rootReal = t.RepoRoot
	}
	rootReal = filepath.Clean(rootReal)
	var hits []FSSearchHit
	for _, line := range strings.Split(res.Stdout, "\n") {
		if line == "" {
			continue
		}
		// path:line:text
		first := strings.IndexByte(line, ':')
		if first < 0 {
			continue
		}
		second := strings.IndexByte(line[first+1:], ':')
		if second < 0 {
			continue
		}
		second += first + 1
		p := line[:first]
		lnStr := line[first+1 : second]
		txt := line[second+1:]
		var ln int
		fmt.Sscanf(lnStr, "%d", &ln)
		// Compute the rel path against the canonical repo root. rg may emit
		// either an absolute path (when given an absolute searchRoot) or a
		// relative one; Rel handles both.
		rel, relErr := filepath.Rel(rootReal, p)
		if relErr != nil {
			// Fall back to t.RepoRoot in case rg's path is relative to the
			// non-canonical alias of the repo root.
			rel, _ = filepath.Rel(t.RepoRoot, p)
		}
		// Forbidden-path post-filter. Cheap and load-bearing: keeps secret
		// material out of the model's view even when ripgrep walked over it.
		if t.Cfg.ForbiddenHit(rel) {
			continue
		}
		hits = append(hits, FSSearchHit{Path: rel, Line: ln, Text: txt})
		if len(hits) >= maxHits {
			break
		}
	}
	return hits, nil
}

// --- git tools ----------------------------------------------------------

func (t *Toolbox) git(ctx context.Context, args ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", t.RepoRoot}, args...)...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		return out.String(), errb.String(), -1, err
	}
	return out.String(), errb.String(), code, nil
}

func (t *Toolbox) GitStatus(ctx context.Context) (string, error) {
	out, _, _, err := t.git(ctx, "status", "--porcelain=v1")
	return out, err
}

func (t *Toolbox) GitDiff(ctx context.Context, staged bool) (string, error) {
	args := []string{"diff"}
	if staged {
		args = append(args, "--staged")
	}
	out, _, _, err := t.git(ctx, args...)
	return out, err
}

func (t *Toolbox) GitBranch(ctx context.Context) (string, error) {
	out, _, _, err := t.git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

// EnsureRepo guarantees RepoRoot is a git repository. If it already is,
// no-op. If not, runs `git init` + commits any existing files as an
// initial snapshot so subsequent `git checkout -b` has a base to branch
// from. Idempotent.
//
// This is the "fresh project, no git yet" escape hatch. It matches what
// users expect when they hand the agent an empty or never-versioned
// directory: do the init ritual for them, don't make them context-switch
// to a terminal before submitting a task. Matches Forge's behavior so
// A/B comparisons between the two agent systems start from parity
// instead of a "did you remember to git init" stumble.
//
// An identity is configured at the local-repo level only if the user has
// no global identity, so we don't stomp on existing git config. The
// synthesized identity ("moirai@localhost") makes the initial
// commit possible; real commits from the agent later still use the
// user's global identity if they set one afterward.
func (t *Toolbox) EnsureRepo(ctx context.Context) error {
	if _, _, code, err := t.git(ctx, "rev-parse", "--git-dir"); err == nil && code == 0 {
		return nil
	}

	if _, errStr, code, err := t.git(ctx, "init"); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("git init: %s", errStr)
	}

	// Seed identity only if neither env vars nor global config provide one.
	// `git config --get` returns code 1 when the key is unset.
	if _, _, code, _ := t.git(ctx, "config", "user.email"); code != 0 {
		_, _, _, _ = t.git(ctx, "config", "user.email", "moirai@localhost")
	}
	if _, _, code, _ := t.git(ctx, "config", "user.name"); code != 0 {
		_, _, _, _ = t.git(ctx, "config", "user.name", "moirai")
	}

	// Stage everything that's already there (including .gitignore if any)
	// and make the root commit. --allow-empty so an empty directory still
	// becomes a valid repo with HEAD pointing somewhere.
	if _, errStr, code, err := t.git(ctx, "add", "-A"); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("git add (init): %s", errStr)
	}
	if _, errStr, code, err := t.git(ctx, "commit", "--allow-empty", "-m", "moirai: initial snapshot"); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("git commit (init): %s", errStr)
	}
	return nil
}

// GitCheckoutBranch creates the agent working branch if missing, then checks
// it out. Leaves any uncommitted changes as-is (they carry onto the branch).
func (t *Toolbox) GitCheckoutBranch(ctx context.Context) error {
	// Does the branch exist?
	_, _, code, err := t.git(ctx, "rev-parse", "--verify", "--quiet", t.Branch)
	if err != nil {
		return err
	}
	if code == 0 {
		_, errStr, code, _ := t.git(ctx, "checkout", t.Branch)
		if code != 0 {
			return fmt.Errorf("git checkout %s: %s", t.Branch, errStr)
		}
		return nil
	}
	_, errStr, code, _ := t.git(ctx, "checkout", "-b", t.Branch)
	if code != 0 {
		return fmt.Errorf("git checkout -b %s: %s", t.Branch, errStr)
	}
	return nil
}

// GitCommit stages and commits with the given message. Refuses to run `push`,
// `reset --hard`, or amend; those aren't exposed at all.
func (t *Toolbox) GitCommit(ctx context.Context, message string) (string, error) {
	if message == "" {
		return "", fmt.Errorf("git commit: empty message")
	}
	// Em-dash / en-dash filtering was previously enforced here. Removed:
	// the rule exists to police human-authored AEGIS prose, NOT model-
	// authored commit messages. A user description that contains U+2014
	// would round-trip through the RO model and self-DoS the task here
	// (commit fails for a reason the operator did not introduce). See
	// SPEC_DEVIATIONS.md "Em dash in commit messages" for context.
	// TODO(rename): consider migrating to .moirai.toml in a future commit;
	// orphaning existing per-repo config files in production today.
	if _, _, code, err := t.git(ctx, "add", "-A", ":(exclude).agent-router.toml"); err != nil {
		return "", err
	} else if code != 0 {
		return "", fmt.Errorf("git add failed")
	}
	out, errStr, code, err := t.git(ctx, "commit", "-m", message)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("git commit: %s", errStr)
	}
	return out, nil
}

// --- test + compile + shell --------------------------------------------

type ExecResult = sandbox.Result

func (t *Toolbox) TestRun(ctx context.Context) (*ExecResult, error) {
	if t.Cfg.Commands.Test == "" {
		return nil, fmt.Errorf("no test command configured")
	}
	return t.shellRun(ctx, t.Cfg.Commands.Test)
}

func (t *Toolbox) CompileRun(ctx context.Context) (*ExecResult, error) {
	if t.Cfg.Commands.Compile == "" {
		return nil, fmt.Errorf("no compile command configured")
	}
	return t.shellRun(ctx, t.Cfg.Commands.Compile)
}

func (t *Toolbox) ShellExec(ctx context.Context, cmdLine string) (*ExecResult, error) {
	return t.shellRun(ctx, cmdLine)
}

func (t *Toolbox) shellRun(ctx context.Context, cmdLine string) (*ExecResult, error) {
	pol := sandbox.Policy{
		RepoRoot:     t.RepoRoot,
		ScratchDir:   t.ScratchDir,
		AllowNetwork: t.AllowNetwork,
	}
	return sandbox.Exec(ctx, pol, []string{"sh", "-c", cmdLine})
}
