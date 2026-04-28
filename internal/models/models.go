// Package models enumerates GGUF model files available to moirai.
//
// head_dim and detected_ctx_max are best-effort: if we can't parse the GGUF
// header, we leave them zero and the caller decides how to render that.
package models

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxScanDepth caps how deep ListGGUF will recurse from the scan root.
// Real model stables nest 1-2 levels (e.g. ~/Models/<family>/<quant>.gguf).
// 4 is generous headroom without letting a misconfigured root (someone
// pointing models_dir at $HOME) walk an entire filesystem tree.
const maxScanDepth = 4

// Info describes a single GGUF model file on disk.
type Info struct {
	Path           string `json:"path"`
	Name           string `json:"name"`       // basename without .gguf
	SizeBytes      int64  `json:"size_bytes"`
	HeadDim        int    `json:"head_dim,omitempty"` // 0 if unknown
	DetectedCtxMax int    `json:"detected_ctx_max,omitempty"`
	TurboquantSafe bool   `json:"turboquant_safe"` // head_dim != 128
}

// ListGGUF scans dir recursively (up to maxScanDepth levels) for *.gguf files.
//
// Recursion was added 2026-04-28: real model stables nest by family
// (~/Models/gemma/*.gguf, ~/Models/Qwen3-Coder-30B-A3B/*.gguf etc.) and the
// previous non-recursive scan returned an empty picker for any non-flat
// layout, causing the UI's slot-swap dropdown to silently miss most of the
// stable. Symlinks (file or directory) are NOT followed: WalkDir visits a
// symlink without dereferencing it, which prevents loops and keeps the depth
// counter honest. Hidden directories (name beginning with `.`) are skipped
// to avoid traversing version-control internals.
func ListGGUF(dir string) ([]Info, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return []Info{}, nil
		}
		return nil, err
	}
	var out []Info
	rootClean := filepath.Clean(dir)
	walkErr := filepath.WalkDir(rootClean, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A single unreadable subtree (permissions, broken symlink target)
			// must not abort the whole scan. Skip the offender and keep going.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			// Depth check relative to root. Counting separators after stripping
			// the root prefix is robust to trailing slashes and Clean()'s
			// normalisation of `./` and `../`.
			rel, _ := filepath.Rel(rootClean, path)
			if rel == "." {
				return nil
			}
			depth := strings.Count(rel, string(filepath.Separator)) + 1
			if depth > maxScanDepth {
				return fs.SkipDir
			}
			// Skip hidden directories (.git, .cache, etc.) -- they cannot
			// contain user-curated GGUFs and slow down deep stables.
			if strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}

		// Files: only *.gguf, case-insensitive.
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".gguf") {
			return nil
		}
		stat, statErr := os.Stat(path)
		if statErr != nil {
			return nil
		}
		out = append(out, Info{
			Path:      path,
			Name:      strings.TrimSuffix(d.Name(), ".gguf"),
			SizeBytes: stat.Size(),
			// head_dim parsing is not implemented; GGUF KV metadata parsing
			// is out of scope for this package. Until it lands, we assume
			// every enumerated model is turboquant-safe. Known-bad
			// head_dim=128 models are rejected via explicit manifest, not
			// via this enumerator.
			TurboquantSafe: true,
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

// IncludeCurrent merges slot-active paths into infos, deduped. Use this when
// the active ModelPaths live outside `dir` and you still want them listed.
func IncludeCurrent(infos []Info, paths []string) []Info {
	seen := map[string]bool{}
	for _, i := range infos {
		seen[i.Path] = true
	}
	for _, p := range paths {
		if seen[p] || p == "" {
			continue
		}
		stat, err := os.Stat(p)
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(p), ".gguf")
		infos = append(infos, Info{
			Path:           p,
			Name:           name,
			SizeBytes:      stat.Size(),
			TurboquantSafe: true,
		})
		seen[p] = true
	}
	return infos
}

// FormatSizeGB renders SizeBytes in "%.1f GB" for UI display.
func FormatSizeGB(bytes int64) string {
	return fmt.Sprintf("%.1f GB", float64(bytes)/1e9)
}
