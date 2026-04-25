package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandUserPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	t.Setenv("HOME", home)
	// Set a sentinel value we can use to confirm that $VAR substitution
	// is NOT performed (security: env-var exfil via repo_root). If
	// expandUserPath ever re-introduces os.ExpandEnv, the assertions
	// below will fail.
	t.Setenv("AR_TEST_SECRET", "leaked-value")

	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"~", home},
		{"~/Projects/observatory", home + "/Projects/observatory"},
		// ~ not at start is a literal
		{"/tmp/~user/x", "/tmp/~user/x"},
		// $VAR / ${VAR} tokens are intentionally left LITERAL so their
		// values cannot be exfiltrated through error messages.
		{"$HOME/Projects/x", "$HOME/Projects/x"},
		{"${HOME}/Projects/x", "${HOME}/Projects/x"},
		{"$AR_TEST_SECRET", "$AR_TEST_SECRET"},
		{"${AR_TEST_SECRET}/sub", "${AR_TEST_SECRET}/sub"},
	}
	for _, c := range cases {
		got, err := expandUserPath(c.in)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExpandUserPathNormalisesTraversal checks that filepath.Clean collapses
// "~/../.." down to the canonical form of $HOME/../.. -- which for a normal
// home like /home/user resolves to "/". This is a normalisation only; see
// the doc comment on expandUserPath for why it is NOT a sandbox.
func TestExpandUserPathNormalisesTraversal(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	t.Setenv("HOME", home)

	got, err := expandUserPath("~/../..")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Clean(home + "/../..")
	if got != want {
		t.Errorf("~/../.. -> %q, want %q", got, want)
	}

	// Also verify a mid-path traversal is cleaned (a/b/../c -> a/c).
	got, err = expandUserPath("~/Projects/../other")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want = filepath.Clean(home + "/other")
	if got != want {
		t.Errorf("~/Projects/../other -> %q, want %q", got, want)
	}
}
