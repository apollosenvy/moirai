package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// expandUserPath resolves shell-like conveniences in a user-supplied path
// before it reaches filepath.Abs. Go's stdlib doesn't do tilde expansion or
// env var expansion; without this, submit forms that contain "~/Projects/x"
// or "$HOME/Projects/x" end up stat'ing bogus concatenated paths relative
// to the daemon's cwd.
//
// Supported substitutions (all are shell conventions users expect):
//
//   ~           -> $HOME
//   ~/sub       -> $HOME/sub
//   $HOME/sub   -> value of HOME env var (and any other plain $VAR)
//   ${VAR}/sub  -> value of VAR with the braced form
//
// Only ~ at path start is expanded (matching shell behavior); tilde in the
// middle of a path segment is treated as a literal. We explicitly do NOT
// handle ~user/... -- that needs /etc/passwd lookups, and in practice the
// daemon runs as a single user so $HOME is always the right answer.
//
// The returned path is run through filepath.Clean so traversal segments
// like "~/../.." collapse to their canonical form. NOTE: this is a
// normalisation, not a sandbox. The final path is NOT restricted to live
// inside $HOME; absolute paths from the caller are still accepted and
// returned verbatim. Callers that need containment must enforce it
// themselves (e.g. by checking filepath.HasPrefix against an allowed
// root after this function returns).
func expandUserPath(p string) (string, error) {
	if p == "" {
		return p, nil
	}
	// Env expansion first so ${HOME} and $HOME both resolve before we look
	// for the literal "~" prefix. os.ExpandEnv is a pure string op and
	// leaves unrecognized $VAR tokens as empty.
	out := os.ExpandEnv(p)

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
