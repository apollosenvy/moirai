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
	t.Setenv("AR_TEST_ROOT", "/tmp/ar-test")
	// Ensure NOPE_NOT_SET is actually unset for the "unknown var collapses
	// to empty" assertion to hold. t.Setenv guarantees restoration.
	t.Setenv("NOPE_NOT_SET", "")
	os.Unsetenv("NOPE_NOT_SET")

	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"~", home},
		{"~/Projects/observatory", home + "/Projects/observatory"},
		{"$HOME/Projects/x", home + "/Projects/x"},
		{"${HOME}/Projects/x", home + "/Projects/x"},
		{"$AR_TEST_ROOT/sub", "/tmp/ar-test/sub"},
		// ~ not at start is a literal
		{"/tmp/~user/x", "/tmp/~user/x"},
		// Unknown var collapses to empty (matches os.ExpandEnv)
		{"$NOPE_NOT_SET/x", "/x"},
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
