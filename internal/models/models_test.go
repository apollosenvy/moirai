package models

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListGGUFScansDirectory(t *testing.T) {
	dir := t.TempDir()
	// Create a fake gguf and a non-gguf to be ignored.
	must := func(p string, size int) {
		f, err := os.Create(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			_ = f.Truncate(int64(size))
		}
		f.Close()
	}
	must("alpha.gguf", 1024)
	must("beta.gguf", 2048)
	must("ignored.txt", 10)

	infos, err := ListGGUF(dir)
	if err != nil {
		t.Fatalf("ListGGUF: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(infos), infos)
	}
	names := map[string]bool{}
	for _, i := range infos {
		names[i.Name] = true
		if i.SizeBytes == 0 {
			t.Errorf("expected size > 0, got %d for %s", i.SizeBytes, i.Name)
		}
		if i.Path == "" {
			t.Errorf("expected non-empty path")
		}
	}
	if !names["alpha"] || !names["beta"] {
		t.Errorf("missing expected names: %v", names)
	}
}

func TestListGGUFEmptyDir(t *testing.T) {
	dir := t.TempDir()
	infos, err := ListGGUF(dir)
	if err != nil {
		t.Fatalf("ListGGUF on empty: %v", err)
	}
	if len(infos) != 0 {
		t.Errorf("expected 0 entries, got %d", len(infos))
	}
}

func TestListGGUFNonexistentDir(t *testing.T) {
	infos, err := ListGGUF("/nonexistent/path/that/should/not/exist")
	if err != nil {
		// OK -- returning error is acceptable.
		return
	}
	if len(infos) != 0 {
		t.Errorf("expected empty result for nonexistent dir, got %d", len(infos))
	}
}

// TestListGGUFRecursesSubdirs covers the 2026-04-28 recursion change. Real
// stables nest by family (~/Models/gemma/<quant>.gguf) and the previous
// non-recursive scan returned an empty picker. Mirrors that layout.
func TestListGGUFRecursesSubdirs(t *testing.T) {
	dir := t.TempDir()
	must := func(rel string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(full)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Truncate(1024)
		f.Close()
	}
	must("flat.gguf")                    // depth 0
	must("gemma/gemma-31b.gguf")         // depth 1
	must("Qwen3-Coder/IQ4_NL/q.gguf")    // depth 2
	must("README.md")                    // not a gguf, ignored
	must(".cache/should-skip.gguf")      // hidden dir, must be skipped

	infos, err := ListGGUF(dir)
	if err != nil {
		t.Fatalf("ListGGUF: %v", err)
	}
	names := map[string]bool{}
	for _, i := range infos {
		names[i.Name] = true
	}
	want := []string{"flat", "gemma-31b", "q"}
	for _, w := range want {
		if !names[w] {
			t.Errorf("expected %q in results, got %v", w, names)
		}
	}
	if names["should-skip"] {
		t.Errorf("hidden-dir gguf should NOT have been included: %v", names)
	}
	if len(infos) != 3 {
		t.Errorf("expected 3 entries (flat + 2 nested), got %d: %+v", len(infos), names)
	}
}

// TestListGGUFMaxDepth verifies the depth cap blocks runaway recursion if
// someone points models_dir at $HOME or /. Anything past maxScanDepth (4) is
// skipped, period.
func TestListGGUFMaxDepth(t *testing.T) {
	dir := t.TempDir()
	// Create one file at exactly maxScanDepth and one beyond it.
	deep := dir
	for i := 0; i < maxScanDepth; i++ {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "edge.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tooDeep := filepath.Join(deep, "d", "deeper")
	if err := os.MkdirAll(tooDeep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tooDeep, "out_of_reach.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	infos, err := ListGGUF(dir)
	if err != nil {
		t.Fatalf("ListGGUF: %v", err)
	}
	names := map[string]bool{}
	for _, i := range infos {
		names[i.Name] = true
	}
	if !names["edge"] {
		t.Errorf("expected edge.gguf at depth=maxScanDepth to be found, got %v", names)
	}
	if names["out_of_reach"] {
		t.Errorf("out_of_reach.gguf past maxScanDepth must be skipped, got %v", names)
	}
}
