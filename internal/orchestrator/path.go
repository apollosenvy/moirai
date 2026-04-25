package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// expandUserPath resolves shell-like conveniences in a user-supplied path
// before it reaches filepath.Abs. Go's stdlib doesn't do tilde expansion;
// without this, submit forms that contain "~/Projects/x" end up stat'ing
// bogus concatenated paths relative to the daemon's cwd.
//
// Supported substitutions:
//
//   ~           -> $HOME
//   ~/sub       -> $HOME/sub
//
// Only a leading ~ is expanded. Any other $VAR / ${VAR} token in the
// supplied path is intentionally NOT expanded -- doing so via
// os.ExpandEnv would let an unauthenticated HTTP caller exfiltrate
// arbitrary daemon environment values (HF_TOKEN, AWS_SECRET_ACCESS_KEY,
// PATH, etc.) by submitting repo_root="$HF_TOKEN" and reading the
// expanded value back from the os.Stat error message returned in the
// 400 response. Leaving $VAR literal means the subsequent stat/open
// fails naturally without leaking env values.
//
// We explicitly do NOT handle ~user/... -- that needs /etc/passwd
// lookups, and in practice the daemon runs as a single user so $HOME
// is always the right answer.
//
// The returned path is run through filepath.Clean so traversal segments
// like "~/../.." collapse to their canonical form. NOTE: this is a
// normalisation, not a sandbox. The final path is NOT restricted to
// live inside $HOME; absolute paths from the caller are still accepted
// and returned verbatim. Callers that need containment must enforce it
// themselves (e.g. by checking filepath.HasPrefix against an allowed
// root after this function returns).
func expandUserPath(p string) (string, error) {
	if p == "" {
		return p, nil
	}
	out := p
	if out == "~" || strings.HasPrefix(out, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve ~: %w", err)
		}
		if out == "~" {
			return filepath.Clean(home), nil
		}
		out = home + out[1:]
	}
	return filepath.Clean(out), nil
}
