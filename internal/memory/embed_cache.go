package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	embeddedModelName            = "bge-small-en-v1.5"
	embeddedModelManifestMaxSize = 64 * 1024
)

type embeddedModelFile struct {
	Name string
	Data []byte
}

type embeddedModelFileManifest struct {
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
}

type embeddedModelManifest struct {
	Name       string                               `json:"name"`
	BundleHash string                               `json:"bundle_hash"`
	Files      map[string]embeddedModelFileManifest `json:"files"`
}

var embeddedModelFiles = []embeddedModelFile{
	{Name: "model.onnx", Data: modelOnnx},
	{Name: "tokenizer.json", Data: modelTokenizer},
	{Name: "config.json", Data: modelConfig},
}

var (
	writeEmbeddedModelFile  = os.WriteFile
	publishEmbeddedModelDir = publishEmbeddedModelDirNoReplace
)

func ensureEmbeddedModelDir(home string) (string, error) {
	finalDir := embeddedModelCacheDir(home)
	if err := ensureEmbeddedModelAncestors(home); err != nil {
		return "", err
	}
	info, err := os.Lstat(finalDir)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("embedded model cache dir %s is a symlink", finalDir)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("embedded model cache dir %s is not a directory", finalDir)
		}
		if err := validateFinalEmbeddedModelDir(home, finalDir); err != nil {
			return "", err
		}
		return finalDir, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	cacheRoot := embeddedModelCacheRoot(home)
	stagingDir, err := os.MkdirTemp(cacheRoot, "."+embeddedModelBundleHash()+"-tmp-*")
	if err != nil {
		return "", err
	}
	stagingOwned := true
	defer func() {
		if stagingOwned {
			_ = os.RemoveAll(stagingDir)
		}
	}()

	for _, f := range embeddedModelFiles {
		if err := writeEmbeddedModelFile(filepath.Join(stagingDir, f.Name), f.Data, 0o600); err != nil {
			return "", err
		}
	}
	manifestData, err := json.MarshalIndent(expectedEmbeddedModelManifest(), "", "  ")
	if err != nil {
		return "", err
	}
	manifestData = append(manifestData, '\n')
	if err := writeEmbeddedModelFile(filepath.Join(stagingDir, "manifest.json"), manifestData, 0o600); err != nil {
		return "", err
	}
	if err := validateEmbeddedModelContentDir(stagingDir); err != nil {
		return "", err
	}

	if err := publishEmbeddedModelDir(stagingDir, finalDir); err != nil {
		if validateErr := validateFinalEmbeddedModelDir(home, finalDir); validateErr == nil {
			_ = os.RemoveAll(stagingDir)
			stagingOwned = false
			return finalDir, nil
		}
		return "", err
	}
	stagingOwned = false
	if err := validateFinalEmbeddedModelDir(home, finalDir); err != nil {
		return "", err
	}
	return finalDir, nil
}

func embeddedModelCacheRoot(home string) string {
	return filepath.Join(home, ".lightcode", "cache", "models", embeddedModelName)
}

func embeddedModelCacheDir(home string) string {
	return filepath.Join(embeddedModelCacheRoot(home), embeddedModelBundleHash())
}

func ensureEmbeddedModelAncestors(home string) error {
	paths := []string{
		filepath.Join(home, ".lightcode"),
		filepath.Join(home, ".lightcode", "cache"),
		filepath.Join(home, ".lightcode", "cache", "models"),
		embeddedModelCacheRoot(home),
	}
	for _, path := range paths {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("embedded model cache ancestor %s is a symlink", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("embedded model cache ancestor %s is not a directory", path)
		}
	}
	return nil
}

func validateFinalEmbeddedModelDir(home, dir string) error {
	expected := filepath.Clean(embeddedModelCacheDir(home))
	if filepath.Clean(dir) != expected {
		return fmt.Errorf("embedded model cache dir %s is not expected final path %s", dir, expected)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("embedded model cache dir %s is a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("embedded model cache dir %s is not a directory", dir)
	}
	return validateEmbeddedModelContentDir(dir)
}

func validateEmbeddedModelContentDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	expectedNames := map[string]bool{
		"model.onnx":     true,
		"tokenizer.json": true,
		"config.json":    true,
		"manifest.json":  true,
	}
	if len(entries) != len(expectedNames) {
		return fmt.Errorf("embedded model cache %s has %d entries, want %d", dir, len(entries), len(expectedNames))
	}
	for _, entry := range entries {
		if !expectedNames[entry.Name()] {
			return fmt.Errorf("embedded model cache %s has unexpected entry %s", dir, entry.Name())
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("embedded model cache file %s is a symlink", filepath.Join(dir, entry.Name()))
		}
	}

	manifestBytes, err := readRegularFileNoFollowMax(filepath.Join(dir, "manifest.json"), embeddedModelManifestMaxSize)
	if err != nil {
		return err
	}
	var manifest embeddedModelManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return err
	}
	expected := expectedEmbeddedModelManifest()
	if manifest.Name != expected.Name {
		return fmt.Errorf("embedded model manifest name %q, want %q", manifest.Name, expected.Name)
	}
	if manifest.BundleHash != expected.BundleHash {
		return fmt.Errorf("embedded model manifest bundle hash %q, want %q", manifest.BundleHash, expected.BundleHash)
	}
	if len(manifest.Files) != len(expected.Files) {
		return fmt.Errorf("embedded model manifest file count %d, want %d", len(manifest.Files), len(expected.Files))
	}
	for _, f := range embeddedModelFiles {
		want, ok := expected.Files[f.Name]
		if !ok {
			return fmt.Errorf("internal embedded model manifest missing %s", f.Name)
		}
		got, ok := manifest.Files[f.Name]
		if !ok {
			return fmt.Errorf("embedded model manifest missing %s", f.Name)
		}
		if got != want {
			return fmt.Errorf("embedded model manifest for %s = %+v, want %+v", f.Name, got, want)
		}
		data, err := readRegularFileNoFollowMax(filepath.Join(dir, f.Name), int64(want.Size))
		if err != nil {
			return err
		}
		if len(data) != want.Size || sha256Hex(data) != want.SHA256 {
			return fmt.Errorf("embedded model cache file %s does not match embedded bytes", f.Name)
		}
	}
	return nil
}

func expectedEmbeddedModelManifest() embeddedModelManifest {
	files := make(map[string]embeddedModelFileManifest, len(embeddedModelFiles))
	for _, f := range embeddedModelFiles {
		files[f.Name] = embeddedModelFileManifest{
			SHA256: sha256Hex(f.Data),
			Size:   len(f.Data),
		}
	}
	return embeddedModelManifest{
		Name:       embeddedModelName,
		BundleHash: embeddedModelBundleHash(),
		Files:      files,
	}
}

func embeddedModelBundleHash() string {
	return embeddedModelBundleHashForFiles(embeddedModelFiles)
}

func embeddedModelBundleHashForFiles(files []embeddedModelFile) string {
	h := sha256.New()
	for _, f := range files {
		_, _ = h.Write([]byte(f.Name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(f.Data)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readRegularFileNoFollowMax(path string, maxSize int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("embedded model cache file %s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("embedded model cache file %s is not a regular file", path)
	}
	if maxSize < 0 {
		return nil, fmt.Errorf("embedded model cache file %s has invalid max size %d", path, maxSize)
	}
	if info.Size() > maxSize {
		return nil, fmt.Errorf("embedded model cache file %s is %d bytes, want at most %d", path, info.Size(), maxSize)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("embedded model cache file %s exceeds %d bytes", path, maxSize)
	}
	return data, nil
}

func publishEmbeddedModelDirNoReplace(stagingDir, finalDir string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, stagingDir, unix.AT_FDCWD, finalDir, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "renameat2", Old: stagingDir, New: finalDir, Err: err}
	}
	return nil
}
