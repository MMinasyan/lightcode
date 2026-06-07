package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureEmbeddedModelDirUsesLightcodeCache(t *testing.T) {
	home := t.TempDir()

	got, err := ensureEmbeddedModelDir(home)
	if err != nil {
		t.Fatalf("ensureEmbeddedModelDir: %v", err)
	}

	want := filepath.Join(home, ".lightcode", "cache", "models", modelVersion)
	if got != want {
		t.Fatalf("model dir = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, "/tmp/lightcode-model-") {
		t.Fatalf("model dir must not live under /tmp/lightcode-model-: %s", got)
	}

	for _, name := range requiredCacheFiles {
		if _, err := os.Stat(filepath.Join(got, name)); err != nil {
			t.Fatalf("required cache file %q missing: %v", name, err)
		}
	}
	versionData, err := os.ReadFile(filepath.Join(got, ".version"))
	if err != nil {
		t.Fatalf("read .version: %v", err)
	}
	if string(versionData) != modelVersion {
		t.Fatalf(".version = %q, want %q", string(versionData), modelVersion)
	}
}

func TestEnsureEmbeddedModelDirReusesCompleteCache(t *testing.T) {
	home := t.TempDir()

	first, err := ensureEmbeddedModelDir(home)
	if err != nil {
		t.Fatalf("first ensureEmbeddedModelDir: %v", err)
	}
	versionPath := filepath.Join(first, ".version")
	beforeInfo, err := os.Stat(versionPath)
	if err != nil {
		t.Fatalf("stat .version before reuse: %v", err)
	}

	second, err := ensureEmbeddedModelDir(home)
	if err != nil {
		t.Fatalf("second ensureEmbeddedModelDir: %v", err)
	}
	if second != first {
		t.Fatalf("second call returned different dir: got %q, want %q", second, first)
	}

	afterInfo, err := os.Stat(versionPath)
	if err != nil {
		t.Fatalf("stat .version after reuse: %v", err)
	}
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatalf("reuse rewrote .version (mtime changed from %s to %s)", beforeInfo.ModTime(), afterInfo.ModTime())
	}
}

func TestEnsureEmbeddedModelDirRepairsIncompleteCache(t *testing.T) {
	home := t.TempDir()

	modelDir := filepath.Join(home, ".lightcode", "cache", "models", modelVersion)
	if err := os.MkdirAll(modelDir, 0700); err != nil {
		t.Fatalf("seed incomplete dir: %v", err)
	}
	// Leave only one of the three required files — the cache is incomplete.
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatalf("seed config.json: %v", err)
	}

	got, err := ensureEmbeddedModelDir(home)
	if err != nil {
		t.Fatalf("repair ensureEmbeddedModelDir: %v", err)
	}
	if got != modelDir {
		t.Fatalf("repaired dir = %q, want %q", got, modelDir)
	}
	for _, name := range requiredCacheFiles {
		info, err := os.Stat(filepath.Join(got, name))
		if err != nil {
			t.Fatalf("after repair, %q missing: %v", name, err)
		}
		// The seeded config.json was a 2-byte stub; the repaired one must be
		// the real embedded bundle (much larger).
		if name == "config.json" && info.Size() <= 2 {
			t.Fatalf("after repair, config.json still looks like the seeded stub (size=%d)", info.Size())
		}
	}
	versionData, err := os.ReadFile(filepath.Join(got, ".version"))
	if err != nil {
		t.Fatalf("read .version after repair: %v", err)
	}
	if string(versionData) != modelVersion {
		t.Fatalf(".version after repair = %q, want %q", string(versionData), modelVersion)
	}
}
