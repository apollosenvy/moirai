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
