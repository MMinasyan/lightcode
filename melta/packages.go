package melta

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/safefs"
	"github.com/MMinasyan/lightcode/protocol"
)

// packageSelection is the atomic selected-package record: the extension ID,
// its implementation version, and the immutable package digest it names.
type packageSelection struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

// loadedPackage is one selected package's read executable selection and its
// constructed executable native.
type loadedPackage struct {
	executable executableExtension
	native     extension.Native
}

// packageManager owns the local executable-package layout beneath the one
// data directory. One loaded code set is fixed for the owner lifetime;
// install, list and remove only read or replace next-start selections. Every
// method serializes on mu, so package access is serialized under the owner.
type packageManager struct {
	dataDir string

	mu             sync.Mutex
	loaded         map[string]string
	unusable       map[string]error
	loadedPackages []loadedPackage
}

// newPackageManager constructs the manager before the first load. The loaded
// and unusable maps are populated by load from the selected records.
func newPackageManager(dataDir string) *packageManager {
	return &packageManager{
		dataDir:  dataDir,
		loaded:   make(map[string]string),
		unusable: make(map[string]error),
	}
}

// load reads every selected package once into the owner's fixed loaded set.
// The supplied constructor builds each package's executable native; any
// per-package failure, native construction included, leaves no executable
// selection and is recorded unusable by its hash directory. The owner still
// opens so that a disabled or uninstalled section stays silent.
func (m *packageManager) load(buildNative func(executableExtension) (extension.Native, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	root := filepath.Join(m.dataDir, "extensions")
	directory, err := safefs.OpenDirectory(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	entries, err := directory.ReadDir(-1)
	_ = directory.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !canonicalPackageHash(name) {
			continue
		}
		selected, ok, err := readSelection(filepath.Join(root, name))
		if err != nil {
			m.unusable[name] = err
			continue
		}
		if !ok {
			continue
		}
		if extensionDataHash(selected.ID) != name {
			m.unusable[name] = fmt.Errorf("selected package identity %q does not match its directory", selected.ID)
			continue
		}
		executable, err := loadPackage(m.dataDir, name, selected)
		if err != nil {
			m.unusable[name] = err
			continue
		}
		native, err := buildNative(executable)
		if err != nil {
			m.unusable[name] = err
			continue
		}
		m.loaded[selected.ID] = selected.Version
		m.loadedPackages = append(m.loadedPackages, loadedPackage{executable: executable, native: native})
	}
	sort.Slice(m.loadedPackages, func(i, j int) bool {
		return m.loadedPackages[i].executable.Declaration.ID < m.loadedPackages[j].executable.Declaration.ID
	})
	return nil
}

// loadedExecutables returns the loaded code set in stable extension-ID order.
func (m *packageManager) loadedExecutables() []executableExtension {
	out := make([]executableExtension, len(m.loadedPackages))
	for i, loaded := range m.loadedPackages {
		out[i] = loaded.executable
	}
	return out
}

// loadedNatives returns the constructed executable natives in stable
// extension-ID order.
func (m *packageManager) loadedNatives() []extension.Native {
	out := make([]extension.Native, len(m.loadedPackages))
	for i, loaded := range m.loadedPackages {
		out[i] = loaded.native
	}
	return out
}

// install reads one local source package's manifest and entry point, stages
// exactly those bytes at a content digest, publishes the immutable package,
// and atomically replaces the selected record. It never edits configuration
// or deletes an earlier package copy.
func (m *packageManager) install(sourceDir string) (protocol.ExtensionPackage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sourceDir == "" {
		return protocol.ExtensionPackage{}, fmt.Errorf("package source directory must be non-empty: %w", harness.ErrInvalid)
	}
	sourceRoot, err := os.OpenRoot(sourceDir)
	if err != nil {
		return protocol.ExtensionPackage{}, fmt.Errorf("open package source directory: %v: %w", err, harness.ErrInvalid)
	}
	defer func() { _ = sourceRoot.Close() }()
	manifestBytes, err := sourceRoot.ReadFile("manifest.json")
	if err != nil {
		return protocol.ExtensionPackage{}, fmt.Errorf("read package manifest: %v: %w", err, harness.ErrInvalid)
	}
	var manifest worker.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return protocol.ExtensionPackage{}, fmt.Errorf("decode package manifest: %v: %w", err, harness.ErrInvalid)
	}
	if manifest.ContractVersion != extension.ContractVersion {
		return protocol.ExtensionPackage{}, fmt.Errorf("package contract version %q, want %q: %w", manifest.ContractVersion, extension.ContractVersion, harness.ErrInvalid)
	}
	declaration := manifest.Declaration.Value()
	if err := validatePackageDeclaration(declaration); err != nil {
		return protocol.ExtensionPackage{}, fmt.Errorf("package declaration: %v: %w", err, harness.ErrInvalid)
	}
	if !containedPackageEntryPoint(manifest.EntryPoint) {
		return protocol.ExtensionPackage{}, fmt.Errorf("entry point %q is not a contained package-relative path: %w", manifest.EntryPoint, harness.ErrInvalid)
	}
	entryFile, err := sourceRoot.Open(manifest.EntryPoint)
	if err != nil {
		return protocol.ExtensionPackage{}, fmt.Errorf("open package entry point: %v: %w", err, harness.ErrInvalid)
	}
	if err := validatePackageEntryFile(entryFile, manifest.EntryPoint); err != nil {
		_ = entryFile.Close()
		return protocol.ExtensionPackage{}, err
	}
	entryBytes, err := io.ReadAll(entryFile)
	_ = entryFile.Close()
	if err != nil {
		return protocol.ExtensionPackage{}, fmt.Errorf("read package entry point: %v: %w", err, harness.ErrInvalid)
	}

	extensionDir := filepath.Join(m.dataDir, "extensions", extensionDataHash(declaration.ID))
	packagesDir := filepath.Join(extensionDir, "packages")
	if err := safefs.EnsureDirectory(packagesDir, 0o700); err != nil {
		return protocol.ExtensionPackage{}, err
	}
	stagingID, err := newInstanceID()
	if err != nil {
		return protocol.ExtensionPackage{}, err
	}
	stagingDir := filepath.Join(packagesDir, ".staging-"+stagingID)
	if err := safefs.EnsureDirectory(stagingDir, 0o700); err != nil {
		return protocol.ExtensionPackage{}, err
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()
	if err := writeStagedFile(filepath.Join(stagingDir, "manifest.json"), manifestBytes, 0o600); err != nil {
		return protocol.ExtensionPackage{}, err
	}
	if err := writeStagedFile(filepath.Join(stagingDir, manifest.EntryPoint), entryBytes, 0o700); err != nil {
		return protocol.ExtensionPackage{}, err
	}
	if err := syncStagedDirectories(stagingDir, manifest.EntryPoint); err != nil {
		return protocol.ExtensionPackage{}, err
	}
	digest := packageDigest(manifestBytes, entryBytes)
	packageDir := filepath.Join(packagesDir, digest)
	if err := publishPackage(stagingDir, packageDir); err != nil {
		return protocol.ExtensionPackage{}, err
	}
	if err := syncParentChain(m.dataDir, packagesDir); err != nil {
		return protocol.ExtensionPackage{}, err
	}
	selected, err := json.Marshal(packageSelection{ID: declaration.ID, Version: declaration.Version, Digest: digest})
	if err != nil {
		return protocol.ExtensionPackage{}, err
	}
	if err := atomicfs.Write(filepath.Join(extensionDir, "selected"), selected, 0o600); err != nil {
		return protocol.ExtensionPackage{}, err
	}
	return protocol.ExtensionPackage{
		Id:              declaration.ID,
		LoadedVersion:   m.loaded[declaration.ID],
		SelectedVersion: declaration.Version,
	}, nil
}

// remove deletes only the selected record. Package copies and loaded code are
// never deleted, and an absent selection is already removed.
func (m *packageManager) remove(extensionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if extensionID == "" {
		return fmt.Errorf("extension ID must be non-empty: %w", harness.ErrInvalid)
	}
	extensionDir := filepath.Join(m.dataDir, "extensions", extensionDataHash(extensionID))
	err := safefs.RemoveLeaf(filepath.Join(extensionDir, "selected"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return atomicfs.SyncDir(extensionDir)
}

// list reports every loaded extension and every selected next-start package.
// A missing selected record is the normal not-selected state; any other read
// failure is an error.
func (m *packageManager) list() ([]protocol.ExtensionPackage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	selected := make(map[string]string)
	root := filepath.Join(m.dataDir, "extensions")
	directory, err := safefs.OpenDirectory(root)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if directory != nil {
		entries, err := directory.ReadDir(-1)
		_ = directory.Close()
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || !canonicalPackageHash(name) {
				continue
			}
			selection, ok, err := readSelection(filepath.Join(root, name))
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if extensionDataHash(selection.ID) != name {
				return nil, fmt.Errorf("selected package identity %q does not match its directory", selection.ID)
			}
			selected[selection.ID] = selection.Version
		}
	}
	ids := make([]string, 0, len(m.loaded)+len(selected))
	for id := range m.loaded {
		ids = append(ids, id)
	}
	for id := range selected {
		if _, ok := m.loaded[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	rows := make([]protocol.ExtensionPackage, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, protocol.ExtensionPackage{
			Id:              id,
			LoadedVersion:   m.loaded[id],
			SelectedVersion: selected[id],
		})
	}
	return rows, nil
}

// ListExtensionPackages serves the loaded and selected versions. It is
// Caller-scoped to the whole owner and serialized under it.
func (r *Melta) ListExtensionPackages(ctx context.Context, c Caller) ([]protocol.ExtensionPackage, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return nil, err
	}
	return r.packages.list()
}

// InstallExtensionPackage installs one local source package for the next
// start. It is Caller-scoped to the whole owner and never changes the live
// loaded set.
func (r *Melta) InstallExtensionPackage(ctx context.Context, c Caller, sourceDir string) (protocol.ExtensionPackage, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ExtensionPackage{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ExtensionPackage{}, err
	}
	return r.packages.install(sourceDir)
}

// RemoveExtensionPackage removes the next-start selection. It is Caller-scoped
// to the whole owner and deletes no package copy.
func (r *Melta) RemoveExtensionPackage(ctx context.Context, c Caller, extensionID string) error {
	release, err := r.enter(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return err
	}
	return r.packages.remove(extensionID)
}

// loadPackage opens one selected package's manifest and entry point beneath
// its hash directory, validates that the manifest names exactly the selected
// identity, and returns its executable selection.
func loadPackage(dataDir, hash string, selected packageSelection) (executableExtension, error) {
	packageDir := filepath.Join(dataDir, "extensions", hash, "packages", selected.Digest)
	manifestFile, err := safefs.OpenExisting(filepath.Join(packageDir, "manifest.json"), os.O_RDONLY)
	if err != nil {
		return executableExtension{}, fmt.Errorf("open package manifest: %v", err)
	}
	manifestBytes, err := io.ReadAll(manifestFile)
	_ = manifestFile.Close()
	if err != nil {
		return executableExtension{}, fmt.Errorf("read package manifest: %v", err)
	}
	var manifest worker.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return executableExtension{}, fmt.Errorf("decode package manifest: %v", err)
	}
	if manifest.ContractVersion != extension.ContractVersion {
		return executableExtension{}, fmt.Errorf("package contract version %q, want %q", manifest.ContractVersion, extension.ContractVersion)
	}
	declaration := manifest.Declaration.Value()
	if declaration.ID != selected.ID || declaration.Version != selected.Version {
		return executableExtension{}, fmt.Errorf("package declaration %s@%s does not match selected %s@%s", declaration.ID, declaration.Version, selected.ID, selected.Version)
	}
	entryFile, err := openPackageEntry(packageDir, manifest.EntryPoint)
	if err != nil {
		return executableExtension{}, err
	}
	_ = entryFile.Close()
	return executableExtension{Declaration: declaration, EntryPoint: filepath.Join(packageDir, manifest.EntryPoint)}, nil
}

// validatePackageDeclaration runs the one declaration accepting owner over
// the package's local declaration and schemas. Installation validates the
// package's own shape; dependency resolution remains the configuration
// build's accepting step.
func validatePackageDeclaration(declaration extension.Declaration) error {
	native := extension.Native{
		Declaration: declaration,
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{}, nil
		},
	}
	for _, capability := range declaration.Capabilities {
		if capability.Kind != extension.KindTool {
			continue
		}
		if native.Describe == nil {
			native.Describe = make(map[string]extension.ToolDescribe)
		}
		native.Describe[capability.ID] = func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
			return extension.ToolDescription{}, nil
		}
	}
	_, err := extension.CompileNative(native)
	return err
}

// openPackageEntry opens one contained package-relative entry point without
// following a symlink in any component, and applies the shared regular
// executable check.
func openPackageEntry(packageDir, entryPoint string) (*os.File, error) {
	if !containedPackageEntryPoint(entryPoint) {
		return nil, fmt.Errorf("entry point %q is not a contained package-relative path: %w", entryPoint, harness.ErrInvalid)
	}
	file, err := safefs.OpenExisting(filepath.Join(packageDir, entryPoint), os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("open package entry point: %v: %w", err, harness.ErrInvalid)
	}
	if err := validatePackageEntryFile(file, entryPoint); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// validatePackageEntryFile applies the one entry-point shape rule to an
// already-opened file from either the install source root or the owned
// package directory: it must be a regular executable file.
func validatePackageEntryFile(file *os.File, entryPoint string) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("entry point %q is not a regular file: %w", entryPoint, harness.ErrInvalid)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("entry point %q is not executable: %w", entryPoint, harness.ErrInvalid)
	}
	return nil
}

// containedPackageEntryPoint reports whether one manifest entry point is a
// nonempty local path that cannot escape the package directory.
func containedPackageEntryPoint(value string) bool {
	return value != "" && value != "manifest.json" && filepath.IsLocal(value)
}

// readSelection reads one extension's selected record. A missing record is
// the normal not-selected state.
func readSelection(extensionDir string) (packageSelection, bool, error) {
	file, err := safefs.OpenExisting(filepath.Join(extensionDir, "selected"), os.O_RDONLY)
	if err != nil {
		if os.IsNotExist(err) {
			return packageSelection{}, false, nil
		}
		return packageSelection{}, false, fmt.Errorf("open selected package: %w", err)
	}
	defer file.Close()
	var selected packageSelection
	if err := json.NewDecoder(file).Decode(&selected); err != nil {
		return packageSelection{}, false, fmt.Errorf("decode selected package: %w", err)
	}
	if selected.ID == "" || selected.Version == "" || !canonicalPackageDigest(selected.Digest) {
		return packageSelection{}, false, errors.New("selected package record is incomplete")
	}
	return selected, true, nil
}

// writeStagedFile writes and syncs one staged package file through the
// checked no-follow traversal.
func writeStagedFile(path string, data []byte, perm os.FileMode) error {
	file, _, err := safefs.OpenForWrite(path, perm)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// syncStagedDirectories syncs the staging root and every directory created
// for the entry point's parents, deepest first, so a nested entry point's
// directory chain is durable before the staging rename.
func syncStagedDirectories(stagingDir, entryPoint string) error {
	for dir := filepath.Dir(filepath.Join(stagingDir, entryPoint)); ; dir = filepath.Dir(dir) {
		if err := atomicfs.SyncDir(dir); err != nil {
			return err
		}
		if dir == stagingDir {
			return nil
		}
	}
}

// publishPackage renames the staged copy onto its content digest, reusing an
// existing immutable copy as is. A planted symlink or non-directory at the
// destination is refused instead of followed.
func publishPackage(stagingDir, packageDir string) error {
	published, err := safefs.OpenDirectory(packageDir)
	if err == nil {
		_ = published.Close()
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(stagingDir, packageDir)
}

// syncParentChain makes one published package directory entry durable by
// syncing every directory from dir up to and including dataDir.
func syncParentChain(dataDir, dir string) error {
	for {
		if err := atomicfs.SyncDir(dir); err != nil {
			return err
		}
		if dir == dataDir || dir == filepath.Dir(dir) {
			return nil
		}
		dir = filepath.Dir(dir)
	}
}

// packageDigest is the content digest of the two staged package files. Length
// framing keeps the manifest and entry point bytes unambiguous.
func packageDigest(manifest, entry []byte) string {
	digest := sha256.New()
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(manifest)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(manifest)
	binary.BigEndian.PutUint64(size[:], uint64(len(entry)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(entry)
	return hex.EncodeToString(digest.Sum(nil))
}

// canonicalPackageHash reports whether one extension directory name is the
// lowercase hex SHA-256 identity.
func canonicalPackageHash(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

// canonicalPackageDigest reports whether one selected digest is the
// lowercase hex SHA-256 content digest.
func canonicalPackageDigest(value string) bool {
	return canonicalPackageHash(value)
}
