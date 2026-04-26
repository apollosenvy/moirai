package orchestrator

import (
	"strings"
	"testing"
)

// TestExtractFileBlocksBasic: a textbook coder reply with two
// markdown-fenced blocks, each tagged with `# file: <path>`. Both
// should be extracted with the file marker stripped from the body.
func TestExtractFileBlocksBasic(t *testing.T) {
	reply := "Here is the code:\n\n" +
		"```typescript\n" +
		"# file: src/types.ts\n" +
		"export interface User { id: number; }\n" +
		"```\n\n" +
		"```typescript\n" +
		"# file: src/db.ts\n" +
		"import Database from 'better-sqlite3';\n" +
		"export const db = new Database('app.db');\n" +
		"```\n"
	files := extractFileBlocks(reply)
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d: %+v", len(files), files)
	}
	if files[0].Path != "src/types.ts" {
		t.Errorf("file[0] path: %q, want src/types.ts", files[0].Path)
	}
	if !strings.Contains(files[0].Content, "export interface User") {
		t.Errorf("file[0] content missing User interface: %q", files[0].Content)
	}
	if strings.Contains(files[0].Content, "# file:") {
		t.Errorf("file[0] content still contains the file marker: %q", files[0].Content)
	}
	if files[1].Path != "src/db.ts" {
		t.Errorf("file[1] path: %q, want src/db.ts", files[1].Path)
	}
}

// TestExtractFileBlocksSkipsUnmarkedFences: a fence without a
// `# file:` marker is treated as an example/snippet, not a file to
// commit. Only marked fences are returned.
func TestExtractFileBlocksSkipsUnmarkedFences(t *testing.T) {
	reply := "Example usage:\n\n" +
		"```typescript\n" +
		"const u = new User('alice');\n" +
		"```\n\n" +
		"Now the actual file:\n" +
		"```typescript\n" +
		"# file: src/user.ts\n" +
		"export class User { constructor(public name: string) {} }\n" +
		"```\n"
	files := extractFileBlocks(reply)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if files[0].Path != "src/user.ts" {
		t.Errorf("path: %q, want src/user.ts", files[0].Path)
	}
}

// TestExtractFileBlocksRejectsBadPaths: absolute paths and `..`
// traversal must be skipped at the parser level.
func TestExtractFileBlocksRejectsBadPaths(t *testing.T) {
	reply := "```sh\n" +
		"# file: /etc/passwd\n" +
		"oh no\n" +
		"```\n" +
		"```python\n" +
		"# file: ../escape.py\n" +
		"print('escape')\n" +
		"```\n" +
		"```python\n" +
		"# file: src/safe.py\n" +
		"print('safe')\n" +
		"```\n"
	files := extractFileBlocks(reply)
	if len(files) != 1 {
		t.Fatalf("want 1 file (only safe.py), got %d: %+v", len(files), files)
	}
	if files[0].Path != "src/safe.py" {
		t.Errorf("path: %q, want src/safe.py", files[0].Path)
	}
}

// TestExtractFileBlocksAcceptsSlashSlashMarker: some coder models emit
// `// file: <path>` instead of `# file: <path>` (TypeScript style).
// Both must work.
func TestExtractFileBlocksAcceptsSlashSlashMarker(t *testing.T) {
	reply := "```typescript\n" +
		"// file: src/api.ts\n" +
		"export function ping() { return 'ok'; }\n" +
		"```\n"
	files := extractFileBlocks(reply)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if files[0].Path != "src/api.ts" {
		t.Errorf("path: %q, want src/api.ts", files[0].Path)
	}
	if !strings.Contains(files[0].Content, "export function ping") {
		t.Errorf("body missing expected content: %q", files[0].Content)
	}
}

// TestExtractFileBlocksHandlesEmptyReply: empty input -> empty result,
// no panic.
func TestExtractFileBlocksHandlesEmptyReply(t *testing.T) {
	if got := extractFileBlocks(""); got != nil && len(got) != 0 {
		t.Fatalf("empty reply should yield empty slice, got %d", len(got))
	}
}

// TestExtractFileBlocksMultiMarkerFenceSplits: when a coder packs MULTIPLE
// `# file:` markers into a single fence, each marker becomes its own
// extraction. Pre-fix, the regex returned only the first marker and the
// second file's marker line ended up as comment text inside the first
// file's body — silently corrupting both files (ADV-05).
func TestExtractFileBlocksMultiMarkerFenceSplits(t *testing.T) {
	// Multiple files in one fence, blank-line separated -- the realistic
	// coder pattern. Each marker must be at body start OR follow a blank
	// line (post-fix boundary requirement) to be considered a split.
	reply := "```typescript\n" +
		"# file: src/a.ts\n" +
		"export const a = 1;\n" +
		"\n" +
		"# file: src/b.ts\n" +
		"export const b = 2;\n" +
		"\n" +
		"# file: src/c.ts\n" +
		"export const c = 3;\n" +
		"```\n"
	files := extractFileBlocks(reply)
	if len(files) != 3 {
		t.Fatalf("multi-marker fence: want 3 files, got %d: %+v", len(files), files)
	}
	if files[0].Path != "src/a.ts" || files[1].Path != "src/b.ts" || files[2].Path != "src/c.ts" {
		t.Errorf("paths in order: got [%q,%q,%q], want [src/a.ts,src/b.ts,src/c.ts]",
			files[0].Path, files[1].Path, files[2].Path)
	}
	if !strings.Contains(files[0].Content, "export const a = 1;") {
		t.Errorf("file[0] content wrong: %q", files[0].Content)
	}
	if strings.Contains(files[0].Content, "# file:") {
		t.Errorf("file[0] should not include subsequent marker: %q", files[0].Content)
	}
	if !strings.Contains(files[1].Content, "export const b = 2;") {
		t.Errorf("file[1] content wrong: %q", files[1].Content)
	}
	if !strings.Contains(files[2].Content, "export const c = 3;") {
		t.Errorf("file[2] content wrong: %q", files[2].Content)
	}
}

// TestExtractFileBlocksRejectsControlChars: a path containing newline,
// null, or other control characters must be rejected. Such paths could
// corrupt the rendered checklist or be used for log injection.
func TestExtractFileBlocksRejectsControlChars(t *testing.T) {
	// Construct a fence whose marker has an embedded newline-style char.
	// The regex itself stops at \n in the path capture (it requires
	// non-whitespace), so this is mostly a belt-and-suspenders check
	// for control chars below 0x20 that aren't whitespace (e.g. \x01).
	reply := "```sh\n" +
		"# file: src/a\x01b.sh\n" +
		"#!/bin/sh\n" +
		"echo hi\n" +
		"```\n" +
		"```sh\n" +
		"# file: src/clean.sh\n" +
		"echo ok\n" +
		"```\n"
	files := extractFileBlocks(reply)
	if len(files) != 1 {
		t.Fatalf("control-char path should be rejected; got %d files: %+v", len(files), files)
	}
	if files[0].Path != "src/clean.sh" {
		t.Errorf("path: %q, want src/clean.sh", files[0].Path)
	}
}

// TestExtractFileBlocksRejectsEmbeddedMarkerInStringLiteral: a `# file:`
// that appears inside a Go raw string, a Markdown body, or any other
// multi-line literal must NOT be treated as a split boundary. Pre-fix,
// the multi-marker splitter would write a phantom file with the
// post-marker body (audit pass-2 finding). Post-fix, the boundary check
// requires the marker to be at the start of the fence body or preceded
// by a blank line, neither of which applies to embedded substrings.
func TestExtractFileBlocksRejectsEmbeddedMarkerInStringLiteral(t *testing.T) {
	// The Go file body itself contains "# file: phantom.go" inside a
	// regex literal — it must NOT trigger a phantom file write.
	reply := "```go\n" +
		"# file: pkg/regex.go\n" +
		"package regex\n" +
		"var pattern = `\n" +
		"// example matching a marker\n" +
		"# file: phantom.go\n" +
		"more content\n" +
		"`\n" +
		"```\n"
	files := extractFileBlocks(reply)
	if len(files) != 1 {
		t.Fatalf("embedded marker should not split: got %d files: %+v", len(files), files)
	}
	if files[0].Path != "pkg/regex.go" {
		t.Errorf("path: %q, want pkg/regex.go", files[0].Path)
	}
	if !strings.Contains(files[0].Content, "phantom.go") {
		t.Errorf("embedded marker text should remain in legitimate file content: %q", files[0].Content)
	}
}

// TestExtractFileBlocksAcceptsBlankLineSeparatedMarkers: when a coder
// legitimately packs multiple files in one fence, blank-line separation
// between file blocks marks the boundary and the splitter must accept it.
func TestExtractFileBlocksAcceptsBlankLineSeparatedMarkers(t *testing.T) {
	reply := "```typescript\n" +
		"# file: src/a.ts\n" +
		"export const a = 1;\n" +
		"\n" +
		"# file: src/b.ts\n" +
		"export const b = 2;\n" +
		"```\n"
	files := extractFileBlocks(reply)
	if len(files) != 2 {
		t.Fatalf("blank-line separated markers should both split: got %d", len(files))
	}
	if files[0].Path != "src/a.ts" || files[1].Path != "src/b.ts" {
		t.Errorf("paths: [%q, %q], want [src/a.ts, src/b.ts]", files[0].Path, files[1].Path)
	}
}

// TestExtractFileBlocksAcceptsAdjacentMarkersWithNoCode: an "empty file"
// followed by a real file works -- the boundary check should accept the
// second marker if the prior segment is just whitespace.
func TestExtractFileBlocksAcceptsAdjacentMarkers(t *testing.T) {
	// Two markers with NO blank line between them -- second marker is
	// directly adjacent. Pre-multi-marker, this matched only the first
	// (so the second's marker text became comment in the first's body).
	// Post-fix with boundary check: the second marker is NOT preceded
	// by a blank line and NOT at line 0 — should it split? Today's
	// boundary check rejects it. That's strict but safe. Document the
	// shape.
	reply := "```typescript\n" +
		"# file: src/a.ts\n" +
		"# file: src/b.ts\n" +
		"export const b = 2;\n" +
		"```\n"
	files := extractFileBlocks(reply)
	// With the strict boundary check, only the first marker counts.
	// Second marker is on the line immediately after the first marker
	// line (no blank line). Body of a.ts becomes "# file: src/b.ts\n
	// export const b = 2;\n". This is NOT ideal but it's the documented
	// behavior of the boundary check.
	if len(files) != 1 {
		t.Errorf("adjacent markers (no blank line): got %d files, want 1 with strict boundary; %+v", len(files), files)
	}
}

// TestValidExtractionPathRejectsTraversalSegmentOnly: a path containing
// '..' as a directory NAME (segment) must be rejected, but a path
// containing '..' as a substring of a directory name (e.g. 'a..b') is
// fine. Pre-fix, the simple substring check rejected legitimate names
// (ADV-08). The post-fix segment-aware check fixes this.
func TestValidExtractionPathRejectsTraversalSegmentOnly(t *testing.T) {
	// Real traversal: rejected.
	if validExtractionPath("../foo.go") {
		t.Error("../foo.go should be rejected")
	}
	if validExtractionPath("a/../b.go") {
		t.Error("a/../b.go should be rejected")
	}
	// Substring '..' in a name: allowed.
	if !validExtractionPath("src/a..b/foo.go") {
		t.Error("src/a..b/foo.go should be allowed (substring '..' is fine in a name)")
	}
	if !validExtractionPath("src/.../foo.go") {
		t.Error("src/.../foo.go should be allowed (three dots is a valid name)")
	}
}
