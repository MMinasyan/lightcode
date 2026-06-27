package memory

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/options"
	"golang.org/x/sys/unix"
)

func TestEmbeddedModelBundleHashIsContentBased(t *testing.T) {
	files := []embeddedModelFile{
		{Name: "model.onnx", Data: []byte("model")},
		{Name: "tokenizer.json", Data: []byte("tokenizer")},
		{Name: "config.json", Data: []byte("config")},
	}
	base := embeddedModelBundleHashForFiles(files)

	changedBytes := []embeddedModelFile{
		{Name: "model.onnx", Data: []byte("different")},
		{Name: "tokenizer.json", Data: []byte("tokenizer")},
		{Name: "config.json", Data: []byte("config")},
	}
	if got := embeddedModelBundleHashForFiles(changedBytes); got == base {
		t.Fatal("bundle hash did not change after file bytes changed")
	}

	changedName := []embeddedModelFile{
		{Name: "renamed.onnx", Data: []byte("model")},
		{Name: "tokenizer.json", Data: []byte("tokenizer")},
		{Name: "config.json", Data: []byte("config")},
	}
	if got := embeddedModelBundleHashForFiles(changedName); got == base {
		t.Fatal("bundle hash did not change after file name changed")
	}
}

func TestEnsureEmbeddedModelDirCreatesCache(t *testing.T) {
	useSmallEmbeddedModel(t)
	resetEmbeddedModelSeams(t)

	home := t.TempDir()
	dir, err := ensureEmbeddedModelDir(home)
	if err != nil {
		t.Fatalf("ensureEmbeddedModelDir: %v", err)
	}
	if want := embeddedModelCacheDir(home); dir != want {
		t.Fatalf("cache dir = %q, want %q", dir, want)
	}
	if strings.Contains(dir, "lightcode-model-") {
		t.Fatalf("cache dir uses old temp prefix: %s", dir)
	}
	if err := validateFinalEmbeddedModelDir(home, dir); err != nil {
		t.Fatalf("validate final cache: %v", err)
	}
	if got := filepath.Dir(dir); got != embeddedModelCacheRoot(home) {
		t.Fatalf("cache parent = %q, want %q", got, embeddedModelCacheRoot(home))
	}
}

func TestEnsureEmbeddedModelDirReusesValidCache(t *testing.T) {
	useSmallEmbeddedModel(t)
	resetEmbeddedModelSeams(t)

	home := t.TempDir()
	dir, err := ensureEmbeddedModelDir(home)
	if err != nil {
		t.Fatalf("ensureEmbeddedModelDir first: %v", err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	past := time.Unix(123, 0)
	if err := os.Chtimes(manifestPath, past, past); err != nil {
		t.Fatal(err)
	}

	again, err := ensureEmbeddedModelDir(home)
	if err != nil {
		t.Fatalf("ensureEmbeddedModelDir second: %v", err)
	}
	if again != dir {
		t.Fatalf("second cache dir = %q, want %q", again, dir)
	}
	info, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("manifest was rewritten, mtime = %v want %v", info.ModTime(), past)
	}
}

func TestEnsureEmbeddedModelDirHandlesExistingFinalAfterRace(t *testing.T) {
	useSmallEmbeddedModel(t)
	resetEmbeddedModelSeams(t)

	home := t.TempDir()
	publishEmbeddedModelDir = func(stagingDir, finalDir string) error {
		copyDir(t, stagingDir, finalDir)
		return &os.LinkError{Op: "renameat2", Old: stagingDir, New: finalDir, Err: unix.EEXIST}
	}

	dir, err := ensureEmbeddedModelDir(home)
	if err != nil {
		t.Fatalf("ensureEmbeddedModelDir: %v", err)
	}
	if dir != embeddedModelCacheDir(home) {
		t.Fatalf("cache dir = %q, want final", dir)
	}
	if err := validateFinalEmbeddedModelDir(home, dir); err != nil {
		t.Fatalf("validate final cache: %v", err)
	}
	if dirs := stagingDirs(t, home); len(dirs) != 0 {
		t.Fatalf("staging dirs remain after race loser cleanup: %v", dirs)
	}
}

func TestEnsureEmbeddedModelDirDoesNotReplaceExistingEmptyFinal(t *testing.T) {
	useSmallEmbeddedModel(t)
	resetEmbeddedModelSeams(t)

	home := t.TempDir()
	publishEmbeddedModelDir = func(stagingDir, finalDir string) error {
		if err := os.Mkdir(finalDir, 0o700); err != nil {
			return err
		}
		return &os.LinkError{Op: "renameat2", Old: stagingDir, New: finalDir, Err: unix.EEXIST}
	}

	if _, err := ensureEmbeddedModelDir(home); err == nil {
		t.Fatal("ensureEmbeddedModelDir returned nil error for empty invalid final")
	}
	entries, err := os.ReadDir(embeddedModelCacheDir(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty final was replaced or mutated; entries = %v", entryNames(entries))
	}
	if dirs := stagingDirs(t, home); len(dirs) != 0 {
		t.Fatalf("staging dirs remain after publish failure: %v", dirs)
	}
}

func TestEnsureEmbeddedModelDirFailsOnInvalidFinalCache(t *testing.T) {
	useSmallEmbeddedModel(t)
	resetEmbeddedModelSeams(t)

	home := t.TempDir()
	if err := ensureEmbeddedModelAncestors(home); err != nil {
		t.Fatal(err)
	}
	finalDir := embeddedModelCacheDir(home)
	if err := os.Mkdir(finalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	badManifest := []byte("{bad")
	if err := os.WriteFile(filepath.Join(finalDir, "manifest.json"), badManifest, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureEmbeddedModelDir(home); err == nil {
		t.Fatal("ensureEmbeddedModelDir returned nil error for invalid final")
	}
	got, err := os.ReadFile(filepath.Join(finalDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(badManifest) {
		t.Fatalf("invalid final manifest was mutated: %q", got)
	}
	if dirs := stagingDirs(t, home); len(dirs) != 0 {
		t.Fatalf("staging was created for existing invalid final: %v", dirs)
	}
}

func TestValidateEmbeddedModelFinalRejectsInvalidContent(t *testing.T) {
	useSmallEmbeddedModel(t)
	resetEmbeddedModelSeams(t)

	cases := []struct {
		name   string
		mutate func(t *testing.T, home, dir string)
	}{
		{
			name: "malformed manifest",
			mutate: func(t *testing.T, _, dir string) {
				writeFile(t, filepath.Join(dir, "manifest.json"), []byte("{bad"))
			},
		},
		{
			name: "oversized manifest",
			mutate: func(t *testing.T, _, dir string) {
				writeFile(t, filepath.Join(dir, "manifest.json"), bytesOfSize(embeddedModelManifestMaxSize+1))
			},
		},
		{
			name: "wrong model name",
			mutate: func(t *testing.T, _, dir string) {
				m := expectedEmbeddedModelManifest()
				m.Name = "wrong"
				writeManifest(t, dir, m)
			},
		},
		{
			name: "wrong bundle hash",
			mutate: func(t *testing.T, _, dir string) {
				m := expectedEmbeddedModelManifest()
				m.BundleHash = "wrong"
				writeManifest(t, dir, m)
			},
		},
		{
			name: "missing file",
			mutate: func(t *testing.T, _, dir string) {
				removePath(t, filepath.Join(dir, "model.onnx"))
			},
		},
		{
			name: "changed file with rewritten self consistent manifest",
			mutate: func(t *testing.T, _, dir string) {
				changed := cloneEmbeddedModelFiles()
				changed[0].Data = []byte("different model bytes")
				writeFile(t, filepath.Join(dir, changed[0].Name), changed[0].Data)
				writeManifest(t, dir, manifestForFiles(changed))
			},
		},
		{
			name: "oversized model file",
			mutate: func(t *testing.T, _, dir string) {
				writeFile(t, filepath.Join(dir, "model.onnx"), bytesOfSize(len(embeddedModelFiles[0].Data)+1))
			},
		},
		{
			name: "correct file bytes wrong manifest hash",
			mutate: func(t *testing.T, _, dir string) {
				m := expectedEmbeddedModelManifest()
				got := m.Files["model.onnx"]
				got.SHA256 = strings.Repeat("0", 64)
				m.Files["model.onnx"] = got
				writeManifest(t, dir, m)
			},
		},
		{
			name: "correct file bytes wrong manifest size",
			mutate: func(t *testing.T, _, dir string) {
				m := expectedEmbeddedModelManifest()
				got := m.Files["model.onnx"]
				got.Size++
				m.Files["model.onnx"] = got
				writeManifest(t, dir, m)
			},
		},
		{
			name: "missing manifest file entry",
			mutate: func(t *testing.T, _, dir string) {
				m := expectedEmbeddedModelManifest()
				delete(m.Files, "config.json")
				writeManifest(t, dir, m)
			},
		},
		{
			name: "extra manifest file entry",
			mutate: func(t *testing.T, _, dir string) {
				m := expectedEmbeddedModelManifest()
				m.Files["extra.bin"] = embeddedModelFileManifest{SHA256: strings.Repeat("1", 64), Size: 1}
				writeManifest(t, dir, m)
			},
		},
		{
			name: "non regular expected entry",
			mutate: func(t *testing.T, _, dir string) {
				removePath(t, filepath.Join(dir, "tokenizer.json"))
				if err := os.Mkdir(filepath.Join(dir, "tokenizer.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlinked model",
			mutate: func(t *testing.T, home, dir string) {
				replaceWithSymlink(t, home, dir, "model.onnx")
			},
		},
		{
			name: "symlinked tokenizer",
			mutate: func(t *testing.T, home, dir string) {
				replaceWithSymlink(t, home, dir, "tokenizer.json")
			},
		},
		{
			name: "symlinked config",
			mutate: func(t *testing.T, home, dir string) {
				replaceWithSymlink(t, home, dir, "config.json")
			},
		},
		{
			name: "extra on disk entry",
			mutate: func(t *testing.T, _, dir string) {
				writeFile(t, filepath.Join(dir, "extra.bin"), []byte("extra"))
			},
		},
		{
			name: "symlinked final hash path",
			mutate: func(t *testing.T, home, dir string) {
				target := filepath.Join(home, "valid-target")
				writeEmbeddedModelContent(t, target)
				removePath(t, dir)
				if err := os.Symlink(target, dir); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "non directory final hash path",
			mutate: func(t *testing.T, _, dir string) {
				removePath(t, dir)
				writeFile(t, dir, []byte("not a directory"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dir := createValidFinalCache(t, home)
			tc.mutate(t, home, dir)
			if err := validateFinalEmbeddedModelDir(home, dir); err == nil {
				t.Fatal("validateFinalEmbeddedModelDir returned nil error")
			}
		})
	}
}

func TestEnsureEmbeddedModelDirRejectsSymlinkAndNonDirectoryAncestors(t *testing.T) {
	useSmallEmbeddedModel(t)
	resetEmbeddedModelSeams(t)

	for _, ancestor := range []struct {
		name string
		path func(home string) string
	}{
		{name: ".lightcode", path: func(home string) string { return filepath.Join(home, ".lightcode") }},
		{name: ".lightcode/cache", path: func(home string) string { return filepath.Join(home, ".lightcode", "cache") }},
		{name: "cache/models", path: func(home string) string { return filepath.Join(home, ".lightcode", "cache", "models") }},
		{name: "model cache root", path: func(home string) string { return embeddedModelCacheRoot(home) }},
	} {
		t.Run(ancestor.name+"/symlink", func(t *testing.T) {
			home := t.TempDir()
			path := ancestor.path(home)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(home, "symlink-target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			if _, err := ensureEmbeddedModelDir(home); err == nil {
				t.Fatal("ensureEmbeddedModelDir returned nil error for symlink ancestor")
			}
			if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
				t.Fatalf("symlink target entries = %v err=%v, want empty target", entryNames(entries), err)
			}
		})

		t.Run(ancestor.name+"/file", func(t *testing.T) {
			home := t.TempDir()
			path := ancestor.path(home)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, path, []byte("not a directory"))
			if _, err := ensureEmbeddedModelDir(home); err == nil {
				t.Fatal("ensureEmbeddedModelDir returned nil error for non-directory ancestor")
			}
		})
	}
}

func TestEnsureEmbeddedModelDirCleansStagingOnFailure(t *testing.T) {
	useSmallEmbeddedModel(t)
	resetEmbeddedModelSeams(t)

	home := t.TempDir()
	writeEmbeddedModelFile = func(path string, data []byte, perm fs.FileMode) error {
		if filepath.Base(path) == "tokenizer.json" {
			return errors.New("forced write failure")
		}
		return os.WriteFile(path, data, perm)
	}

	if _, err := ensureEmbeddedModelDir(home); err == nil {
		t.Fatal("ensureEmbeddedModelDir returned nil error for forced write failure")
	}
	if dirs := stagingDirs(t, home); len(dirs) != 0 {
		t.Fatalf("staging dirs remain after write failure: %v", dirs)
	}
}

func TestNewEmbedderInitFailureDoesNotDeleteCacheDir(t *testing.T) {
	useSmallEmbeddedModel(t)
	resetEmbeddedModelSeams(t)

	home := t.TempDir()
	newGoSession = func(context.Context, ...options.WithOption) (*hugot.Session, error) {
		return nil, errors.New("forced session failure")
	}

	if _, err := NewEmbedder(home); err == nil {
		t.Fatal("NewEmbedder returned nil error for forced session failure")
	}
	if err := validateFinalEmbeddedModelDir(home, embeddedModelCacheDir(home)); err != nil {
		t.Fatalf("final cache after NewEmbedder failure did not validate: %v", err)
	}
}

func TestEmbedderCloseDoesNotDeleteCacheDir(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "sentinel")
	writeFile(t, sentinel, []byte("keep"))

	e := &Embedder{modelDir: dir}
	e.Close()
	e.Close()

	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
		t.Fatalf("sentinel after Close = %q err=%v, want keep", got, err)
	}
}

func TestProductionMemoryCodeDoesNotUseLightcodeModelTempDir(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	var violations []string

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "build", "node_modules", "vendor":
				return filepath.SkipDir
			}
			if path == filepath.Join(root, "frontend", "wailsjs") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		rel, _ := filepath.Rel(root, path)
		if strings.Contains(text, "lightcode-model-") {
			violations = append(violations, rel+": contains lightcode-model-")
		}
		if strings.HasPrefix(rel, filepath.Join("internal", "memory")+string(os.PathSeparator)) {
			for _, pattern := range []string{`MkdirTemp("",`, `MkdirTemp(os.TempDir(),`, `os.TempDir()`, `"/tmp/`, "`/tmp/"} {
				if strings.Contains(text, pattern) {
					violations = append(violations, rel+": contains "+pattern)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("old temp model materialization patterns found:\n%s", strings.Join(violations, "\n"))
	}
}

func useSmallEmbeddedModel(t *testing.T) {
	t.Helper()
	old := embeddedModelFiles
	embeddedModelFiles = []embeddedModelFile{
		{Name: "model.onnx", Data: []byte("small model")},
		{Name: "tokenizer.json", Data: []byte(`{"tokenizer":true}`)},
		{Name: "config.json", Data: []byte(`{"config":true}`)},
	}
	t.Cleanup(func() {
		embeddedModelFiles = old
	})
}

func resetEmbeddedModelSeams(t *testing.T) {
	t.Helper()
	oldWrite := writeEmbeddedModelFile
	oldPublish := publishEmbeddedModelDir
	oldSession := newGoSession
	oldPipeline := newFeatureExtractionPipeline
	t.Cleanup(func() {
		writeEmbeddedModelFile = oldWrite
		publishEmbeddedModelDir = oldPublish
		newGoSession = oldSession
		newFeatureExtractionPipeline = oldPipeline
	})
}

func createValidFinalCache(t *testing.T, home string) string {
	t.Helper()
	dir, err := ensureEmbeddedModelDir(home)
	if err != nil {
		t.Fatalf("ensureEmbeddedModelDir: %v", err)
	}
	return dir
}

func writeEmbeddedModelContent(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range embeddedModelFiles {
		writeFile(t, filepath.Join(dir, f.Name), f.Data)
	}
	writeManifest(t, dir, expectedEmbeddedModelManifest())
}

func writeManifest(t *testing.T, dir string, manifest embeddedModelManifest) {
	t.Helper()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	writeFile(t, filepath.Join(dir, "manifest.json"), data)
}

func manifestForFiles(files []embeddedModelFile) embeddedModelManifest {
	manifest := embeddedModelManifest{
		Name:       embeddedModelName,
		BundleHash: embeddedModelBundleHashForFiles(files),
		Files:      make(map[string]embeddedModelFileManifest, len(files)),
	}
	for _, f := range files {
		manifest.Files[f.Name] = embeddedModelFileManifest{
			SHA256: sha256Hex(f.Data),
			Size:   len(f.Data),
		}
	}
	return manifest
}

func cloneEmbeddedModelFiles() []embeddedModelFile {
	out := make([]embeddedModelFile, len(embeddedModelFiles))
	for i, f := range embeddedModelFiles {
		out[i] = embeddedModelFile{Name: f.Name, Data: append([]byte(nil), f.Data...)}
	}
	return out
}

func replaceWithSymlink(t *testing.T, home, dir, name string) {
	t.Helper()
	target := filepath.Join(home, "symlink-target-"+strings.ReplaceAll(name, ".", "-"))
	writeFile(t, target, []byte("target"))
	path := filepath.Join(dir, name)
	removePath(t, path)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func stagingDirs(t *testing.T, home string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(embeddedModelCacheRoot(home), ".*-tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			copyDir(t, srcPath, dstPath)
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			t.Fatal(err)
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dstPath, data, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func bytesOfSize(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = 'x'
	}
	return data
}

func removePath(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

func entryNames(entries []fs.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
