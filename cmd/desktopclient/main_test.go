package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDesktopClientPreviewArgParsing proves the preview host accepts only
// its two flags and rejects every positional or unknown argument before any
// Wails composition runs.
func TestDesktopClientPreviewArgParsing(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		assetsDir, runtimeExecutable, err := parseArgs(nil, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("parseArgs: %v", err)
		}
		if assetsDir != defaultAssetsDir || runtimeExecutable != "" {
			t.Fatalf("defaults = (%q, %q)", assetsDir, runtimeExecutable)
		}
	})

	t.Run("explicit flags", func(t *testing.T) {
		assetsDir, runtimeExecutable, err := parseArgs([]string{"--assets-dir", "/tmp/assets", "--runtime-executable", "/tmp/runtime"}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("parseArgs: %v", err)
		}
		if assetsDir != "/tmp/assets" || runtimeExecutable != "/tmp/runtime" {
			t.Fatalf("parsed = (%q, %q)", assetsDir, runtimeExecutable)
		}
	})

	t.Run("rejected arguments", func(t *testing.T) {
		rejected := [][]string{
			{"runtime", "--data-dir", "/tmp/data", "--config", "/tmp/config.json"},
			{"--bogus"},
			{"--assets-dir", "/tmp/assets", "extra"},
			{"runtime"},
			{"--runtime-executable"},
		}
		for _, args := range rejected {
			if _, _, err := parseArgs(args, &bytes.Buffer{}); err == nil {
				t.Fatalf("parseArgs(%q) = no error, want exit 2", args)
			}
		}
	})
}

// TestDesktopClientPreviewContenderExits proves a contender invocation of
// the preview's nonexistent runtime command exits with code 2 instead of
// opening a preview window.
func TestDesktopClientPreviewContenderExits(t *testing.T) {
	if code := run([]string{"runtime", "--data-dir", "/tmp/data", "--config", "/tmp/config.json"}); code != 2 {
		t.Fatalf("contender run = exit %d, want 2", code)
	}
	if code := run([]string{"--unknown-flag"}); code != 2 {
		t.Fatalf("unknown flag run = exit %d, want 2", code)
	}
	// A malformed invocation leaves no state behind: parseArgs reports the
	// exact rejection text.
	_, _, err := parseArgs([]string{"runtime"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("positional rejection = %v", err)
	}
}
