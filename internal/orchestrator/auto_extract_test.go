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
