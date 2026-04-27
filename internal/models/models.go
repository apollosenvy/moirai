// Package models enumerates GGUF model files available to moirai.
//
// head_dim and detected_ctx_max are best-effort: if we can't parse the GGUF
// header, we leave them zero and the caller decides how to render that.
package models

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Info describes a single GGUF model file on disk.
type Info struct {
	Path           string `json:"path"`
	Name           string `json:"name"`       // basename without .gguf
	SizeBytes      int64  `json:"size_bytes"`
	HeadDim        int    `json:"head_dim,omitempty"` // 0 if unknown
	DetectedCtxMax int    `json:"detected_ctx_max,omitempty"`
	TurboquantSafe bool   `json:"turboquant_safe"` // head_dim != 128
}

// ListGGUF scans dir (non-recursive) for *.gguf files.
func ListGGUF(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Info{}, nil
		}
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		stat, err := os.Stat(full)
		if err != nil {
			continue
		}
		info := Info{
			Path:      full,
			Name:      strings.TrimSuffix(e.Name(), ".gguf"),
			SizeBytes: stat.Size(),
		}
		// head_dim parsing is not implemented; GGUF KV metadata parsing is
		// out of scope for this package. Until it lands, we assume every
		// enumerated model is turboquant-safe. Known-bad head_dim=128 models
		// are rejected via explicit manifest, not via this enumerator.
		info.TurboquantSafe = info.HeadDim != 128
		out = append(out, info)
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
