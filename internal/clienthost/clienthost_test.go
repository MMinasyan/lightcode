package clienthost_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/clienthost"
)

// TestDefaultOptions pins the shared default resolution: the home data
// directory, the configured or data-directory config path, the running
// executable, and no prefix arguments. LIGHTCODE_CONFIG wins over the data
// directory's config.json without relocating the data directory, and a
// relative configured path normalizes to an absolute one.
func TestDefaultOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LIGHTCODE_CONFIG", "")

	options, err := clienthost.DefaultOptions()
	if err != nil {
		t.Fatalf("DefaultOptions: %v", err)
	}
	wantDataDir := filepath.Join(home, ".lightcode")
	if options.DataDir != wantDataDir || !filepath.IsAbs(options.DataDir) {
		t.Fatalf("DataDir = %q, want absolute %q", options.DataDir, wantDataDir)
	}
	if options.ConfigPath != filepath.Join(wantDataDir, "config.json") {
		t.Fatalf("ConfigPath = %q, want the data directory's config.json", options.ConfigPath)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if options.Executable != executable {
		t.Fatalf("Executable = %q, want the running executable %q", options.Executable, executable)
	}
	if len(options.PrefixArgs) != 0 {
		t.Fatalf("PrefixArgs = %q, want none", options.PrefixArgs)
	}

	configured := filepath.Join(home, "elsewhere", "custom.json")
	t.Setenv("LIGHTCODE_CONFIG", configured)
	options, err = clienthost.DefaultOptions()
	if err != nil {
		t.Fatalf("DefaultOptions with LIGHTCODE_CONFIG: %v", err)
	}
	if options.ConfigPath != configured {
		t.Fatalf("ConfigPath = %q, want the configured %q", options.ConfigPath, configured)
	}
	if options.DataDir != wantDataDir {
		t.Fatalf("LIGHTCODE_CONFIG relocated DataDir to %q", options.DataDir)
	}

	relative := filepath.Join("relative", "config.json")
	t.Setenv("LIGHTCODE_CONFIG", relative)
	options, err = clienthost.DefaultOptions()
	if err != nil {
		t.Fatalf("DefaultOptions with a relative LIGHTCODE_CONFIG: %v", err)
	}
	wantConfigured, err := filepath.Abs(relative)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	if options.ConfigPath != wantConfigured {
		t.Fatalf("relative ConfigPath = %q, want normalized %q", options.ConfigPath, wantConfigured)
	}

	t.Setenv("HOME", "")
	if _, err := clienthost.DefaultOptions(); err == nil {
		t.Fatal("DefaultOptions without HOME reported success")
	}
}
