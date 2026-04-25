package modelmgr

import (
	"testing"
)

func TestBuildArgsIncludesKvCacheFlags(t *testing.T) {
	cfg := ModelConfig{
		ModelPath: "/tmp/fake.gguf",
		CtxSize:   8192,
		Port:      8001,
		KvCache:   "turbo3",
	}
	args := buildLlamaArgs(cfg)
	has := func(flag, value string) bool {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == flag && args[i+1] == value {
				return true
			}
		}
		return false
	}
	if !has("-ctk", "turbo3") {
		t.Errorf("expected -ctk turbo3 in args, got %v", args)
	}
	if !has("-ctv", "turbo3") {
		t.Errorf("expected -ctv turbo3 in args, got %v", args)
	}
}

// TestBuildArgsDedupsKvFlagsAgainstExtraArgs covers the pass-3 finding:
// when ExtraArgs already supplies -ctk / -ctv (e.g. the default turboArgs),
// the KvCache field must NOT add a duplicate copy. llama-server takes the
// LAST occurrence of a flag, so a duplicate from KvCache placed BEFORE
// ExtraArgs would be silently overridden. The fix dedups on insertion.
func TestBuildArgsDedupsKvFlagsAgainstExtraArgs(t *testing.T) {
	cfg := ModelConfig{
		ModelPath: "/tmp/fake.gguf",
		CtxSize:   8192,
		Port:      8001,
		KvCache:   "q4_0", // user-facing override
		ExtraArgs: []string{"-ctk", "q8_0", "-ctv", "turbo3", "-fa", "on"},
	}
	args := buildLlamaArgs(cfg)
	count := func(flag string) int {
		n := 0
		for _, a := range args {
			if a == flag {
				n++
			}
		}
		return n
	}
	if got := count("-ctk"); got != 1 {
		t.Errorf("expected exactly one -ctk after dedup, got %d (args=%v)", got, args)
	}
	if got := count("-ctv"); got != 1 {
		t.Errorf("expected exactly one -ctv after dedup, got %d (args=%v)", got, args)
	}
	// ExtraArgs values should win (last-flag wins behaviour preserved).
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-ctk" && args[i+1] != "q8_0" {
			t.Errorf("expected -ctk q8_0 from ExtraArgs, got %q", args[i+1])
		}
		if args[i] == "-ctv" && args[i+1] != "turbo3" {
			t.Errorf("expected -ctv turbo3 from ExtraArgs, got %q", args[i+1])
		}
	}
}

// TestBuildArgsKvOnlyCtkInExtra exercises the asymmetric case where
// ExtraArgs has -ctk but not -ctv. The dedup must skip -ctk and still add
// -ctv from KvCache.
func TestBuildArgsKvOnlyCtkInExtra(t *testing.T) {
	cfg := ModelConfig{
		ModelPath: "/tmp/fake.gguf",
		Port:      8001,
		KvCache:   "q4_0",
		ExtraArgs: []string{"-ctk", "q8_0"},
	}
	args := buildLlamaArgs(cfg)
	ctkCount, ctvCount := 0, 0
	var ctvVal string
	for i, a := range args {
		if a == "-ctk" {
			ctkCount++
		}
		if a == "-ctv" {
			ctvCount++
			if i+1 < len(args) {
				ctvVal = args[i+1]
			}
		}
	}
	if ctkCount != 1 {
		t.Errorf("expected -ctk exactly once, got %d (args=%v)", ctkCount, args)
	}
	if ctvCount != 1 {
		t.Errorf("expected -ctv exactly once, got %d (args=%v)", ctvCount, args)
	}
	if ctvVal != "q4_0" {
		t.Errorf("expected -ctv q4_0 from KvCache, got %q", ctvVal)
	}
}

func TestBuildArgsOmitsKvFlagsWhenEmpty(t *testing.T) {
	cfg := ModelConfig{
		ModelPath: "/tmp/fake.gguf",
		CtxSize:   8192,
		Port:      8001,
		KvCache:   "",
	}
	args := buildLlamaArgs(cfg)
	for _, a := range args {
		if a == "-ctk" || a == "-ctv" {
			t.Errorf("did not expect KV flags when KvCache is empty, got %v", args)
		}
	}
}
