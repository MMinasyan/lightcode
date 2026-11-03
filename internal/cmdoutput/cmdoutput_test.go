package cmdoutput

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestCaptureBelowCapExactOutputNoSpill(t *testing.T) {
	home := t.TempDir()
	c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 1024, MaxLineChars: 80})
	defer c.Close()

	_, _ = c.Stdout().Write([]byte("hello\n"))
	_, _ = c.Stderr().Write([]byte("warn\n"))

	testassert.Equal(t, c.Format(), "hello\nwarn\n")
	assertNoFiles(t, filepath.Join(home, ".lightcode"))
}

func TestCaptureManyLinesAboveCapSpillsFullOutput(t *testing.T) {
	home := t.TempDir()
	c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 40, MaxLineChars: 80})
	defer c.Close()
	full := numberedLines(25)
	_, _ = c.Stdout().Write([]byte(full))

	result := c.Format()
	testassert.Require(t, strings.Contains(result, "line 01") && strings.Contains(result, "line 25") && !strings.Contains(result, "line 11"), "Format() = %q, want first and last lines; Format() = %q, want middle lines omitted", result, result)
	spillPath := extractSpillPath(t, result)
	testassert.Require(t, strings.HasPrefix(spillPath, filepath.Join(home, ".lightcode", "cmd_output_")), "spill path = %q, want cmd_output_ under home", spillPath)
	data, err := os.ReadFile(spillPath)
	testassert.Require(t, err == nil, "ReadFile(%q): %v", spillPath, err)
	if string(data) != full {
		t.Fatalf("spill content = %q, want full output", string(data))
	}
}

func TestCaptureFewLongLinesAboveCapTruncatesAndSpills(t *testing.T) {
	home := t.TempDir()
	c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 10, MaxLineChars: 5})
	defer c.Close()
	full := "abcdefg\n1234567\n"
	_, _ = c.Stdout().Write([]byte(full))

	result := c.Format()
	testassert.Require(t, strings.Contains(result, "abcde... [truncated 7 chars]"), "Format() = %q, want per-line truncation", result)
	data, err := os.ReadFile(extractSpillPath(t, result))
	testassert.NoError(t, err)
	if string(data) != full {
		t.Fatalf("spill = %q, want %q", string(data), full)
	}
}

func TestCaptureSharedBudgetAcrossStdoutAndStderr(t *testing.T) {
	home := t.TempDir()
	c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 6, MaxLineChars: 80})
	defer c.Close()
	_, _ = c.Stdout().Write([]byte("abcd"))
	_, _ = c.Stderr().Write([]byte("efgh"))

	c.mu.Lock()
	retained := c.stdout.buf.Len() + c.stderr.buf.Len()
	c.mu.Unlock()
	testassert.Equal(t, retained, 6)
	result := c.Format()
	spillPath := extractSpillPath(t, result)
	data, err := os.ReadFile(spillPath)
	testassert.NoError(t, err)
	if string(data) != "abcdefgh" {
		t.Fatalf("spill = %q, want stdout then stderr", string(data))
	}
}

func TestCaptureCrossingWriteUsesSingleBudget(t *testing.T) {
	home := t.TempDir()
	c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 5, MaxLineChars: 80})
	defer c.Close()
	_, _ = c.Stdout().Write([]byte("abcdef"))
	_, _ = c.Stderr().Write([]byte("ghij"))

	testassert.Equal(t, c.Len(), 10)
	data, err := os.ReadFile(extractSpillPath(t, c.Format()))
	testassert.NoError(t, err)
	if string(data) != "abcdefghij" {
		t.Fatalf("spill = %q, want full output", string(data))
	}
}

func TestCaptureDisabledCapReturnsFullOutputNoSpill(t *testing.T) {
	home := t.TempDir()
	c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 0, MaxLineChars: 3})
	defer c.Close()
	_, _ = c.Stdout().Write([]byte("abcdefg\n"))
	_, _ = c.Stderr().Write([]byte("1234567\n"))

	testassert.Equal(t, c.Format(), "abcdefg\n1234567\n")
	assertNoFiles(t, filepath.Join(home, ".lightcode"))
}

func TestCaptureRepeatedFormatReusesSpillPath(t *testing.T) {
	home := t.TempDir()
	c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 5, MaxLineChars: 80})
	defer c.Close()
	_, _ = c.Stdout().Write([]byte("first\nsecond\nthird\n"))

	first := c.Format()
	second := c.Format()
	testassert.Equal(t, first, second)
	testassert.Require(t, extractSpillPath(t, first) == extractSpillPath(t, second), "Format() did not reuse spill path")
}

func TestCaptureFinalSpillFailureReturnsExplicitMarker(t *testing.T) {
	home := t.TempDir()
	c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 4, MaxLineChars: 80})
	defer c.Close()
	_, _ = c.Stdout().Write([]byte("abcdefghi"))
	testassert.NoError(t, os.Chmod(filepath.Join(home, ".lightcode"), 0o500))
	defer func() { _ = os.Chmod(filepath.Join(home, ".lightcode"), 0o700) }()

	result := c.Format()
	testassert.Require(t, strings.Contains(result, "could not be saved:") && !strings.Contains(result, "saved to:"), "Format() = %q, want could-not-save marker; Format() = %q, should not claim saved path", result, result)
}

func TestCapturePrivateSpillFailureStaysBounded(t *testing.T) {
	homeFile := filepath.Join(t.TempDir(), "home-file")
	testassert.NoError(t, os.WriteFile(homeFile, []byte("not a dir"), 0o600))
	c := NewCapture(Options{Directory: filepath.Join(homeFile, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 4, MaxLineChars: 4})
	defer c.Close()
	_, _ = c.Stdout().Write([]byte("abcdefghijklmnopqrstuvwxyz"))

	result := c.Format()
	testassert.Require(t, strings.Contains(result, "could not be saved:") && !strings.Contains(result, "mnopqrstuvwxyz"), "Format() = %q, want could-not-save marker; Format() = %q, retained discarded tail after private spill failure", result, result)
}

func TestCaptureVeryLongLineTruncatedWithoutFullRetention(t *testing.T) {
	home := t.TempDir()
	c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 8, MaxLineChars: 5})
	defer c.Close()
	_, _ = c.Stdout().Write([]byte(strings.Repeat("x", 10000)))

	result := c.Format()
	testassert.Require(t, strings.Contains(result, "xxxxx... [truncated 10000 chars]") && len(result) <= 300, "Format() = %q, want long-line truncation; Format() length = %d, want bounded preview", result, len(result))
}

func TestCaptureConcurrentWritersAndFormat(t *testing.T) {
	home := t.TempDir()
	c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 64, MaxLineChars: 20})
	defer c.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = c.Stdout().Write([]byte(fmt.Sprintf("out-%d-%d\n", i, j)))
				_, _ = c.Stderr().Write([]byte(fmt.Sprintf("err-%d-%d\n", i, j)))
				_ = c.Format()
			}
		}(i)
	}
	wg.Wait()
	testassert.Require(t, c.Len() != 0, "Len() = 0, want captured concurrent output")
}

func TestCaptureConcurrentFormatAndClose(t *testing.T) {
	for i := 0; i < 50; i++ {
		home := t.TempDir()
		c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 16, MaxLineChars: 20})
		_, _ = c.Stdout().Write([]byte(strings.Repeat("line\n", 1000)))

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_ = c.Format()
		}()
		go func() {
			defer wg.Done()
			<-start
			c.Close()
		}()
		close(start)
		wg.Wait()
	}
}

func TestCaptureCloseDeletesUnreturnedVisibleSpillAndKeepsReturned(t *testing.T) {
	t.Run("unreturned", func(t *testing.T) {
		home := t.TempDir()
		c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 4, MaxLineChars: 80})
		_, _ = c.Stdout().Write([]byte("abcdefghi"))
		c.Format()
		c.mu.Lock()
		c.visibleReturned = false
		path := c.visiblePath
		c.mu.Unlock()
		c.Close()
		{
			_, err := os.Stat(path)
			testassert.Require(t, os.IsNotExist(err), "visible spill after Close = %v, want removed", err)
		}
	})

	t.Run("returned", func(t *testing.T) {
		home := t.TempDir()
		c := NewCapture(Options{Directory: filepath.Join(home, ".lightcode"), SpillPrefix: "cmd_output_", MaxBytes: 4, MaxLineChars: 80})
		_, _ = c.Stdout().Write([]byte("abcdefghi"))
		path := extractSpillPath(t, c.Format())
		c.Close()
		{
			_, err := os.Stat(path)
			testassert.NoError(t, err)
		}
	})
}

func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %02d\n", i)
	}
	return b.String()
}

func extractSpillPath(t *testing.T, result string) string {
	t.Helper()
	marker := "saved to: "
	idx := strings.LastIndex(result, marker)
	testassert.Require(t, idx >= 0, "result = %q, missing spill marker", result)
	path := result[idx+len(marker):]
	if end := strings.IndexAny(path, "]\n"); end >= 0 {
		path = path[:end]
	}
	return strings.TrimSpace(path)
}

func assertNoFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	testassert.Require(t, err == nil, "ReadDir(%q): %v", dir, err)
	testassert.Require(t, len(entries) == 0, "dir %q has files %v, want empty", dir, entries)
}
