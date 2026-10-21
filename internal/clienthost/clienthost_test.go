package clienthost_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.NoError(t, err)
	wantDataDir := filepath.Join(home, ".lightcode")
	testassert.Require(t, options.DataDir == wantDataDir && filepath.IsAbs(options.DataDir) && options.ConfigPath == filepath.Join(wantDataDir, "config.json"), "DataDir = %q, want absolute %q; ConfigPath = %q, want the data directory's config.json", options.DataDir, wantDataDir, options.ConfigPath)
	executable, err := os.Executable()
	testassert.NoError(t, err)
	testassert.Require(t, options.Executable == executable && len(options.PrefixArgs) == 0, "got %v, want %v; PrefixArgs = %q, want none", options.Executable, executable, options.PrefixArgs)

	configured := filepath.Join(home, "elsewhere", "custom.json")
	t.Setenv("LIGHTCODE_CONFIG", configured)
	options, err = clienthost.DefaultOptions()
	testassert.NoError(t, err)
	testassert.Require(t, options.ConfigPath == configured && options.DataDir == wantDataDir, "got %v, want %v; got %v, want %v", options.ConfigPath, configured, options.DataDir, wantDataDir)

	relative := filepath.Join("relative", "config.json")
	t.Setenv("LIGHTCODE_CONFIG", relative)
	options, err = clienthost.DefaultOptions()
	testassert.NoError(t, err)
	wantConfigured, err := filepath.Abs(relative)
	testassert.NoError(t, err)
	testassert.Equal(t, options.ConfigPath, wantConfigured)

	t.Setenv("HOME", "")
	{
		_, err := clienthost.DefaultOptions()
		testassert.Require(t, err != nil, "DefaultOptions without HOME reported success")
	}
}
