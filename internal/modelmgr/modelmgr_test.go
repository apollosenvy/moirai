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
