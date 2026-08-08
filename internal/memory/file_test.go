package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
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

// TestWriteMemoryFilePublishesThroughAtomicfs proves the memory writer
// publishes its record through atomicfs.CreateExclusive: both of the
// publisher's sync hooks are observed — the temp file is synced through the
// file hook before publication, and the destination directory after it.
// Memory files are written by more than one process, so they must be
// published atomically — never torn — and the publisher itself is covered by
// its own tests, so a swap to a plain os.WriteFile bypass would leave those
// green. Requiring both hooks means even a direct write followed by a
// directory sync fails: the temp was never synced through the file hook. Two
// things stay uncovered and are named rather than implied away: the retry
// loop has no seam (the nonce comes from crypto/rand inline, so a name
// collision cannot be forced), and two saves sharing a second, a title and a
// nonce would collide — precisely what the retry is for; it is merely too
// unlikely to provoke.
func TestWriteMemoryFilePublishesThroughAtomicfs(t *testing.T) {
	dir := t.TempDir()
	var synced []string
	atomicfs.SyncDirFunc = func(d string) error {
		synced = append(synced, d)
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
	tempSyncs := 0
	atomicfs.SyncFileFunc = func(*os.File) error { tempSyncs++; return nil }
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil })

	if _, err := WriteMemoryFile(dir, "Title", "body"); err != nil {
		t.Fatalf("WriteMemoryFile: %v", err)
	}
	if tempSyncs != 1 {
		t.Fatalf("temp syncs = %d, want 1: the memory file's temp was not synced through the atomicfs file hook before publication", tempSyncs)
	}
	// CreateExclusive syncs the ancestor chain before publication and the
	// destination directory after it, so the memories dir is the last sync.
	if len(synced) == 0 || synced[len(synced)-1] != dir {
		t.Fatalf("dir syncs = %v, want the memories dir %q synced last after publication", synced, dir)
	}
}
