package modelmgr

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectTurboquantForceEnv(t *testing.T) {
	t.Setenv("AGENT_ROUTER_FORCE_TURBOQUANT", "1")
	got := DetectTurboquant("/bin/true") // /bin/true --help prints nothing useful
	if !got {
		t.Errorf("expected forced true via env")
	}
}

func TestDetectTurboquantGrepBoth(t *testing.T) {
	// Fake --help output via a tiny script.
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-llama")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
echo "--cache-type-k f16, q8_0, q5_1, q4_0, turbo3, turbo4"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	if !DetectTurboquant(script) {
		t.Errorf("expected true when --help mentions both turbo3 and turbo4")
	}

	scriptOnlyOne := filepath.Join(dir, "fake-llama-vanilla")
	if err := os.WriteFile(scriptOnlyOne, []byte(`#!/bin/sh
echo "--cache-type-k f16, q8_0, q5_1, q4_0"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	if DetectTurboquant(scriptOnlyOne) {
		t.Errorf("expected false when --help lacks turbo3/turbo4")
	}
}
