// Package toolbox exposes the coder-side tool set: fs/git/test/compile/shell.
//
// Every operation is scoped to a repo root. File writes are diff-tracked
// through a local git branch. Shell commands route through the sandbox.
// Forbidden paths from .agent-router.toml are enforced here, not just as a
// suggestion to the model.
package toolbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aegis/agent-router/internal/repoconfig"
	"github.com/aegis/agent-router/internal/sandbox"
)

type Toolbox struct {
	RepoRoot     string
	Branch       string // the working branch (agent-router/task-<id>)
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

func (t *Toolbox) resolveInsideRepo(relOrAbs string) (string, error) {
	var abs string
	if filepath.IsAbs(relOrAbs) {
		abs = filepath.Clean(relOrAbs)
	} else {
		abs = filepath.Join(t.RepoRoot, relOrAbs)
	}
	absClean := filepath.Clean(abs)
	if !strings.HasPrefix(absClean+string(os.PathSeparator), t.RepoRoot+string(os.PathSeparator)) && absClean != t.RepoRoot {
		return "", fmt.Errorf("toolbox: path %q outside repo root", relOrAbs)
	}
	rel, err := filepath.Rel(t.RepoRoot, absClean)
	if err != nil {
		return "", err
	}
	if t.Cfg.ForbiddenHit(rel) {
		return "", fmt.Errorf("toolbox: path %q is in [forbidden].paths", rel)
	}
	return absClean, nil
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

func (t *Toolbox) FSSearch(ctx context.Context, pattern, path string, maxHits int) ([]FSSearchHit, error) {
	searchRoot, err := t.resolveInsideRepo(path)
	if err != nil {
		return nil, err
	}
	if maxHits <= 0 {
		maxHits = 100
	}
	args := []string{"--no-heading", "--line-number", "--color=never", "--max-count", "5", pattern, searchRoot}
	res, err := sandbox.Exec(ctx, sandbox.Policy{
		RepoRoot:     t.RepoRoot,
		ScratchDir:   t.ScratchDir,
		AllowNetwork: false,
	}, append([]string{"rg"}, args...))
	if err != nil {
		return nil, err
	}
	// rg returns 1 when no matches; both are fine.
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
		rel, _ := filepath.Rel(t.RepoRoot, p)
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
	if strings.Contains(message, "---") {
		// em-dash check (covers both the double hyphen rendering we actually ship
		// and the literal character, guarding against rogue model output)
		return "", fmt.Errorf("git commit: suspicious em-dash-like sequence in message")
	}
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
