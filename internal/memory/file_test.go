package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlugifyAndYamlQuote(t *testing.T) {
	if got := slugify(" Hello, World!! "); got != "hello-world" {
		t.Fatalf("slugify = %q, want hello-world", got)
	}
	if got := slugify("!@#"); got != "" {
		t.Fatalf("slugify punctuation = %q, want empty", got)
	}
	if got := yamlQuote("plain"); got != "plain" {
		t.Fatalf("yamlQuote plain = %q", got)
	}
	if got := yamlQuote("a: b"); got != `"a: b"` {
		t.Fatalf("yamlQuote colon = %q", got)
	}
	if got := yamlQuote("a\"b"); got != `"a\"b"` {
		t.Fatalf("yamlQuote quote = %q", got)
	}
}

func TestSplitSummary(t *testing.T) {
	got := SplitSummary("intro\n## One\nA\n## Two\nB\n")
	if len(got) != 2 || got[0].Name != "One" || got[0].Content != "A" || got[1].Name != "Two" || got[1].Content != "B" {
		t.Fatalf("SplitSummary headings = %+v", got)
	}
	got = SplitSummary("plain text")
	if len(got) != 1 || got[0].Name != "Summary" || got[0].Content != "plain text" {
		t.Fatalf("SplitSummary plain = %+v", got)
	}
	if got := SplitSummary("   "); got != nil {
		t.Fatalf("SplitSummary empty = %+v, want nil", got)
	}
}

func TestWriteReadMemoryFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMemoryFile(dir, "A: Title", "body")
	if err != nil {
		t.Fatalf("WriteMemoryFile: %v", err)
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "20") || !strings.Contains(base, "a-title") || !strings.HasSuffix(base, ".md") {
		t.Fatalf("path = %q, want timestamped slug with .md suffix", path)
	}
	title, content, createdAt, err := ReadMemoryFile(path)
	if err != nil {
		t.Fatalf("ReadMemoryFile: %v", err)
	}
	if title != "A: Title" || strings.TrimSpace(content) != "body" || createdAt == "" {
		t.Fatalf("read = title:%q content:%q created:%q", title, content, createdAt)
	}

	plain := filepath.Join(dir, "plain.md")
	if err := os.WriteFile(plain, []byte("plain body"), 0o600); err != nil {
		t.Fatal(err)
	}
	title, content, createdAt, err = ReadMemoryFile(plain)
	if err != nil {
		t.Fatalf("ReadMemoryFile plain: %v", err)
	}
	if title != "" || content != "plain body" || createdAt != "" {
		t.Fatalf("plain read = %q/%q/%q", title, content, createdAt)
	}
}
