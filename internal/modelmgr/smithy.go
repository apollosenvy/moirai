// Smithy / kernel-anvil integration. llama-cpp-turboquant's MMVQ kernels
// read ~/.cache/smithy/<basename>.json for shape-specific dispatch configs,
// which hit ~1.5-2x decode speedup on 7900 XTX versus stock kernels. If the
// JSON is missing for a given model, we shell out to `kernel-anvil
// gguf-optimize` to profile it. First run per model is slow (tens of
// seconds to a couple minutes); subsequent runs are instant cache hits.
//
// Design notes:
//   - Profile generation runs inside EnsureSlot() before the llama-server
//     spawn, so it serializes naturally with swaps. No concurrent
//     profiler/server contention for GPU.
//   - We never *fail* a slot start on a profile miss. The smithy header has
//     a graceful fallback to stock MMVQ, so a missing profile just means
//     "slower decode," not "broken slot."
//   - AGENT_ROUTER_NO_SMITHY=1 env disables the whole integration (useful
//     for debugging or for builds against upstream llama.cpp without the
//     smithy patch).

package modelmgr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// smithyProfilePath returns the conventional cache location for a model's
// smithy profile: ~/.cache/smithy/<basename-without-ext>.json. Matches the
// layout smithy-config.h auto-discovers when no SMITHY_CONFIG env is set,
// so setting the env explicitly is a redundancy-with-upside: llama-server
// logs which file it loaded, which saves an order of magnitude of debugging
// time when someone asks "is this profile actually in use?"
func smithyProfilePath(modelPath string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	base := filepath.Base(modelPath)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if base == "" {
		return "", fmt.Errorf("could not derive profile name from %q", modelPath)
	}
	return filepath.Join(home, ".cache", "smithy", base+".json"), nil
}

// ensureSmithyProfile returns the path to a smithy profile for modelPath,
// generating it via the kernel-anvil CLI if missing. Returns ("", nil) if
// the integration is disabled or kernel-anvil is not installed; in that
// case the caller should start llama-server without SMITHY_CONFIG and let
// the smithy header fall back to its cache-discovery path (or default.json).
func ensureSmithyProfile(ctx context.Context, modelPath string) (string, error) {
	if os.Getenv("AGENT_ROUTER_NO_SMITHY") == "1" {
		return "", nil
	}

	profile, err := smithyProfilePath(modelPath)
	if err != nil {
		return "", err
	}

	// Fast path: profile already cached.
	if st, err := os.Stat(profile); err == nil && st.Size() > 0 {
		return profile, nil
	}

	// Missing profile. Try kernel-anvil.
	bin, err := exec.LookPath("kernel-anvil")
	if err != nil {
		// Not a hard error -- just means no profile-guided kernels this session.
		return "", nil
	}

	// Guard against runaway: kernel-anvil gguf-optimize on a 26B-class model
	// empirically completes well under a minute, but we leave slack in case
	// RDNA4 / new arches need more sweep iterations.
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(profile), 0o755); err != nil {
		return "", fmt.Errorf("mkdir smithy cache: %w", err)
	}

	cmd := exec.CommandContext(runCtx, bin, "gguf-optimize", modelPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kernel-anvil gguf-optimize failed: %w (stderr tail: %s)",
			err, lastLine(string(out), 400))
	}

	// gguf-optimize writes to ~/.cache/smithy/<basename>.json by default;
	// verify it actually landed there. If the CLI ever changes the output
	// layout this loud failure will surface it.
	if st, err := os.Stat(profile); err != nil || st.Size() == 0 {
		return "", fmt.Errorf("kernel-anvil completed but profile missing at %s", profile)
	}
	return profile, nil
}

func lastLine(s string, max int) string {
	s = strings.TrimRight(s, "\n")
	if idx := strings.LastIndex(s, "\n"); idx >= 0 {
		s = s[idx+1:]
	}
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}
