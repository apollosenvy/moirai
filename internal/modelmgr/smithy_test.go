package modelmgr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSmithyProfilePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := []struct {
		in   string
		want string
	}{
		{"/home/u/Models/gemma-4-26B-A4B-it-IQ4_XS.gguf",
			filepath.Join(home, ".cache", "smithy", "gemma-4-26B-A4B-it-IQ4_XS.json")},
		{"/abs/path/Qwen3-8B-Q4_K_M.gguf",
			filepath.Join(home, ".cache", "smithy", "Qwen3-8B-Q4_K_M.json")},
		{"relative.gguf",
			filepath.Join(home, ".cache", "smithy", "relative.json")},
	}
	for _, c := range cases {
		got, err := smithyProfilePath(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s -> %s, want %s", c.in, got, c.want)
		}
	}
}

func TestEnsureSmithyProfileDisabled(t *testing.T) {
	t.Setenv("AGENT_ROUTER_NO_SMITHY", "1")
	got, err := ensureSmithyProfile(context.Background(), "/nope/ignore.gguf")
	if err != nil {
		t.Fatalf("disabled should be no-error, got %v", err)
	}
	if got != "" {
		t.Errorf("disabled should return empty, got %q", got)
	}
}
