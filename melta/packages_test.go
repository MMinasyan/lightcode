package melta

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

func openPackageOwner(t *testing.T, dataDir, configPath string) *Melta {
	t.Helper()
	owner, err := open(context.Background(), options{
		DataDir:       dataDir,
		Configuration: NewFileConfigurationSource(configPath),
		Credentials:   NewEnvironmentCredentialSource(filepath.Join(dataDir, ".env")),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return owner
}

func packageCaller(t *testing.T, owner *Melta) Caller {
	t.Helper()
	caller, err := owner.Caller(CallerContext{Origin: harness.InputOriginUser, Scope: harnessScope})
	if err != nil {
		t.Fatalf("caller: %v", err)
	}
	return caller
}

func readSelectedRecord(t *testing.T, dataDir, extensionID string) packageSelection {
	t.Helper()
	selected, ok, err := readSelection(filepath.Join(dataDir, "extensions", extensionDataHash(extensionID)))
	if err != nil || !ok {
		t.Fatalf("read selected record = %+v, %v, want a selection", selected, err)
	}
	return selected
}

func packageRow(t *testing.T, owner *Melta, caller Caller, extensionID string) protocol.ExtensionPackage {
	t.Helper()
	rows, err := owner.ListExtensionPackages(context.Background(), caller)
	if err != nil {
		t.Fatalf("list packages: %v", err)
	}
	for _, row := range rows {
		if row.Id == extensionID {
			return row
		}
	}
	t.Fatalf("package %q is not listed: %+v", extensionID, rows)
	return protocol.ExtensionPackage{}
}

// TestPackageInstallRejectsEscapingAndSymlinkedEntryPoints proves installation
// reads only a contained regular executable entry point and never writes the
// selected record for an escaping, absolute or symlinked source leaf.
func TestPackageInstallRejectsEscapingAndSymlinkedEntryPoints(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("package paths use the Linux protected filesystem traversal")
	}
	declaration := toolOnlyDeclaration()
	writeSource := func(t *testing.T, entryPoint string) string {
		t.Helper()
		source := t.TempDir()
		if err := os.WriteFile(filepath.Join(source, "manifest.json"), manifestWithEntry(t, declaration, entryPoint), 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		return source
	}

	for _, test := range []struct {
		name       string
		entryPoint string
	}{
		{name: "parent escape", entryPoint: "../escaped-worker"},
		{name: "absolute", entryPoint: "/tmp/escaped-worker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataDir := t.TempDir()
			source := writeSource(t, test.entryPoint)
			outside := filepath.Join(filepath.Dir(source), "escaped-worker")
			if err := os.WriteFile(outside, []byte("outside"), 0o700); err != nil {
				t.Fatalf("outside sentinel: %v", err)
			}
			_, err := newPackageManager(dataDir).install(source)
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "install entry point %q = %v, want invalid", test.entryPoint, err)
			mustNotExist(t, filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "selected"), "an escaping entry point created a selection")
			if data, err := os.ReadFile(outside); err != nil || string(data) != "outside" {
				t.Fatalf("outside sentinel = %q, %v", data, err)
			}
		})
	}

	t.Run("symlinked entry point", func(t *testing.T) {
		dataDir := t.TempDir()
		source := writeSource(t, "worker")
		outsideDir := t.TempDir()
		outside := filepath.Join(outsideDir, "outside-worker")
		if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatalf("outside executable: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(source, "worker")); err != nil {
			t.Fatalf("symlink entry point: %v", err)
		}
		_, err := newPackageManager(dataDir).install(source)
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "install symlinked entry point = %v, want invalid", err)
		mustNotExist(t, filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "selected"), "a symlinked entry point created a selection")
		if data, err := os.ReadFile(outside); err != nil || string(data) != "#!/bin/sh\nexit 0\n" {
			t.Fatalf("outside executable = %q, %v", data, err)
		}
	})

	t.Run("not executable", func(t *testing.T) {
		dataDir := t.TempDir()
		source := writeSource(t, "worker")
		if err := os.WriteFile(filepath.Join(source, "worker"), []byte("plain"), 0o600); err != nil {
			t.Fatalf("write non-executable: %v", err)
		}
		_, err := newPackageManager(dataDir).install(source)
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "install non-executable entry point = %v, want invalid", err)
		mustNotExist(t, filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "selected"), "a non-executable entry point created a selection")
	})
}

// manifestWithEntry encodes one deliberately explicit entry point.
func manifestWithEntry(t *testing.T, declaration extension.Declaration, entryPoint string) []byte {
	t.Helper()
	encoded, err := json.Marshal(worker.Manifest{
		ContractVersion: extension.ContractVersion,
		EntryPoint:      entryPoint,
		Declaration:     worker.NewDeclaration(declaration),
	})
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return encoded
}

// TestPackageInstallRejectsPlantedDestinationSymlink proves a symlinked
// package destination is refused instead of followed into an outside tree.
func TestPackageInstallRejectsPlantedDestinationSymlink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("package paths use the Linux protected filesystem traversal")
	}
	declaration := toolOnlyDeclaration()
	dataDir := t.TempDir()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), manifestWithEntry(t, declaration, "worker"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "worker"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write entry point: %v", err)
	}
	extensionDir := filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID))
	if err := os.MkdirAll(extensionDir, 0o700); err != nil {
		t.Fatalf("create extension directory: %v", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(extensionDir, "packages")); err != nil {
		t.Fatalf("plant destination symlink: %v", err)
	}
	_, err := newPackageManager(dataDir).install(source)
	testassert.Require(t, err != nil, "install with a symlinked destination succeeded")
	entries, readErr := os.ReadDir(outside)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("outside destination = %v, %v, want untouched", entries, readErr)
	}
	mustNotExist(t, filepath.Join(extensionDir, "selected"), "a symlinked destination created a selection")
}

// TestPackageInstallPublicationFailureKeepsPreviousSelection proves a failed
// selected-record publication leaves the previous selection intact.
func TestPackageInstallPublicationFailureKeepsPreviousSelection(t *testing.T) {
	declaration := toolOnlyDeclaration()
	dataDir := t.TempDir()
	v1 := t.TempDir()
	if err := writePackageSourceFiles(t, v1, declaration, "1.0.0", "v1-worker"); err != nil {
		t.Fatalf("write v1 source: %v", err)
	}
	if _, err := newPackageManager(dataDir).install(v1); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	selectedPath := filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "selected")
	before, err := os.ReadFile(selectedPath)
	if err != nil {
		t.Fatalf("read selected: %v", err)
	}
	v2 := t.TempDir()
	if err := writePackageSourceFiles(t, v2, declaration, "2.0.0", "v2-worker"); err != nil {
		t.Fatalf("write v2 source: %v", err)
	}

	previousSync := atomicfs.SyncFileFunc
	atomicfs.SyncFileFunc = func(*os.File) error { return errors.New("injected publication failure") }
	_, err = newPackageManager(dataDir).install(v2)
	atomicfs.SyncFileFunc = previousSync
	testassert.Require(t, err != nil, "install with a failed publication succeeded")
	after, err := os.ReadFile(selectedPath)
	if err != nil {
		t.Fatalf("read selected after failure: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("selected record = %s, want the previous %s", after, before)
	}
	testassert.Equal(t, readSelectedRecord(t, dataDir, declaration.ID).Version, "1.0.0")

	if _, err := newPackageManager(dataDir).install(v2); err != nil {
		t.Fatalf("install v2 after restoring publication: %v", err)
	}
	testassert.Equal(t, readSelectedRecord(t, dataDir, declaration.ID).Version, "2.0.0")
}

// TestPackageInstallSyncsStagingDirectoriesAndParentChain proves a nested
// entry point's created directories are each synced before the staging rename,
// and the published parent chain is always synced before the selection write.
func TestPackageInstallSyncsStagingDirectoriesAndParentChain(t *testing.T) {
	declaration := toolOnlyDeclaration()
	dataDir := t.TempDir()
	source := t.TempDir()
	if err := writePackageSourceFiles(t, source, declaration, "1.0.0", filepath.Join("bin", "nested", "worker")); err != nil {
		t.Fatalf("write source: %v", err)
	}
	var mu sync.Mutex
	var synced []string
	previous := atomicfs.SyncDirFunc
	atomicfs.SyncDirFunc = func(dir string) error {
		mu.Lock()
		synced = append(synced, dir)
		mu.Unlock()
		return nil
	}
	defer func() { atomicfs.SyncDirFunc = previous }()
	if _, err := newPackageManager(dataDir).install(source); err != nil {
		t.Fatalf("install: %v", err)
	}
	mu.Lock()
	trace := append([]string(nil), synced...)
	mu.Unlock()
	extensionDir := filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID))
	packagesDir := filepath.Join(extensionDir, "packages")
	testassert.Require(t, len(trace) == 8, "syncs = %v, want three staging directories, the parent chain, and the selection directory", trace)
	testassert.Require(t, strings.HasPrefix(trace[0], packagesDir+string(filepath.Separator)+".staging-") && strings.HasSuffix(trace[0], filepath.Join("bin", "nested")), "first sync = %q, want the nested staging directory", trace[0])
	testassert.Equal(t, trace[1], filepath.Dir(trace[0]))
	testassert.Equal(t, trace[2], filepath.Dir(trace[1]))
	testassert.Equal(t, trace[3], packagesDir)
	testassert.Equal(t, trace[4], extensionDir)
	testassert.Equal(t, trace[5], filepath.Join(dataDir, "extensions"))
	testassert.Equal(t, trace[6], dataDir)
	testassert.Equal(t, trace[7], extensionDir)

	mu.Lock()
	synced = nil
	mu.Unlock()
	if _, err := newPackageManager(dataDir).install(source); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	mu.Lock()
	trace = append([]string(nil), synced...)
	mu.Unlock()
	testassert.Require(t, len(trace) == 8, "reused-copy syncs = %v, want the staging chain and the parent chain regardless of reuse", trace)
	testassert.Require(t, strings.HasPrefix(trace[0], packagesDir+string(filepath.Separator)+".staging-") && strings.HasSuffix(trace[0], filepath.Join("bin", "nested")), "reinstall first sync = %q, want the nested staging directory", trace[0])
	testassert.Equal(t, trace[1], filepath.Dir(trace[0]))
	testassert.Equal(t, trace[2], filepath.Dir(trace[1]))
	testassert.Equal(t, trace[3], packagesDir)
	testassert.Equal(t, trace[4], extensionDir)
	testassert.Equal(t, trace[5], filepath.Join(dataDir, "extensions"))
	testassert.Equal(t, trace[6], dataDir)
	testassert.Equal(t, trace[7], extensionDir)
}

// TestPackageRemoveSyncsExtensionDirectory proves a successful selection
// unlink is followed by the extension directory sync.
func TestPackageRemoveSyncsExtensionDirectory(t *testing.T) {
	declaration := toolOnlyDeclaration()
	dataDir := t.TempDir()
	source := t.TempDir()
	if err := writePackageSourceFiles(t, source, declaration, "1.0.0", "worker"); err != nil {
		t.Fatalf("write source: %v", err)
	}
	manager := newPackageManager(dataDir)
	if _, err := manager.install(source); err != nil {
		t.Fatalf("install: %v", err)
	}
	var synced []string
	previous := atomicfs.SyncDirFunc
	atomicfs.SyncDirFunc = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	defer func() { atomicfs.SyncDirFunc = previous }()
	if err := manager.remove(declaration.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	extensionDir := filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID))
	testassert.Require(t, len(synced) == 1 && synced[0] == extensionDir, "syncs = %v, want the extension directory", synced)
	mustNotExist(t, filepath.Join(extensionDir, "selected"), "remove retained the selected record")
}

// writePackageSourceFiles writes one self-contained source package with a
// distinct declaration version and an executable placeholder entry point.
func writePackageSourceFiles(t *testing.T, dir string, declaration extension.Declaration, version, entry string) error {
	t.Helper()
	versioned := declaration
	versioned.Version = version
	manifest, err := worker.EncodeManifest(versioned, entry)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o600); err != nil {
		return err
	}
	entryPath := filepath.Join(dir, entry)
	if err := os.MkdirAll(filepath.Dir(entryPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(entryPath, []byte("#!/bin/sh\nexit 0\n"), 0o700)
}

// TestPackageInstallRemoveReinstallPreservesConfiguration proves package
// lifecycle operations never rewrite the configuration document.
func TestPackageInstallRemoveReinstallPreservesConfiguration(t *testing.T) {
	declaration := toolOnlyDeclaration()
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, `{"extensions":{"peer":{"enabled":false},"uninstalled":{"enabled":true,"unknown":9007199254740993}}}`)
	v1 := t.TempDir()
	if err := writePackageSourceFiles(t, v1, declaration, "1.0.0", "v1-worker"); err != nil {
		t.Fatalf("write v1 source: %v", err)
	}
	if _, err := newPackageManager(dataDir).install(v1); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	owner := openPackageOwner(t, dataDir, configPath)
	caller := packageCaller(t, owner)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read configuration: %v", err)
	}
	testassert.NoError(t, owner.RemoveExtensionPackage(context.Background(), caller, declaration.ID))
	afterRemove, err := os.ReadFile(configPath)
	if err != nil || string(before) != string(afterRemove) {
		t.Fatalf("configuration after remove = %q, %v, want %q", afterRemove, err, before)
	}
	v2 := t.TempDir()
	if err := writePackageSourceFiles(t, v2, declaration, "2.0.0", "v2-worker"); err != nil {
		t.Fatalf("write v2 source: %v", err)
	}
	row, err := owner.InstallExtensionPackage(context.Background(), caller, v2)
	if err != nil {
		t.Fatalf("install v2: %v", err)
	}
	testassert.Equal(t, row.Id, declaration.ID)
	afterInstall, err := os.ReadFile(configPath)
	if err != nil || string(before) != string(afterInstall) {
		t.Fatalf("configuration after install = %q, %v, want %q", afterInstall, err, before)
	}
	testassert.NoError(t, owner.Close(context.Background()))
}

// TestPackageLoadedSelectedVersionsAcrossRestart proves installation changes
// only the next-start selection; restart selects the installed code.
func TestPackageLoadedSelectedVersionsAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, `{"extensions":{"fixture":{"enabled":false}}}`)
	v1 := buildExecutableFixtureVersion(t, "1.0.0")
	v2 := buildExecutableFixtureVersion(t, "2.0.0")
	declaration := fixtureExecutableDeclaration(t, v1)
	if _, err := newPackageManager(dataDir).install(executablePackageSource(t, executableExtension{Declaration: declaration, EntryPoint: v1})); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	owner := openPackageOwner(t, dataDir, configPath)
	caller := packageCaller(t, owner)
	row := packageRow(t, owner, caller, "fixture")
	testassert.Equal(t, row.LoadedVersion, "1.0.0")
	testassert.Equal(t, row.SelectedVersion, "1.0.0")
	installed, err := owner.InstallExtensionPackage(context.Background(), caller, executablePackageSource(t, executableExtension{Declaration: fixtureExecutableDeclaration(t, v2), EntryPoint: v2}))
	if err != nil {
		t.Fatalf("install v2: %v", err)
	}
	testassert.Equal(t, installed.LoadedVersion, "1.0.0")
	testassert.Equal(t, installed.SelectedVersion, "2.0.0")
	row = packageRow(t, owner, caller, "fixture")
	testassert.Equal(t, row.LoadedVersion, "1.0.0")
	testassert.Equal(t, row.SelectedVersion, "2.0.0")
	testassert.NoError(t, owner.Close(context.Background()))

	restarted := openPackageOwner(t, dataDir, configPath)
	restartedCaller := packageCaller(t, restarted)
	row = packageRow(t, restarted, restartedCaller, "fixture")
	testassert.Equal(t, row.LoadedVersion, "2.0.0")
	testassert.Equal(t, row.SelectedVersion, "2.0.0")
	testassert.NoError(t, restarted.Close(context.Background()))
}

// TestPackageDisabledLoadedUpdateThenReloadUsesLoadedCopy proves a reload
// after an update activates the loaded code, not the newly selected bytes.
func TestPackageDisabledLoadedUpdateThenReloadUsesLoadedCopy(t *testing.T) {
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, `{"extensions":{"fixture":{"enabled":false}}}`)
	v1 := buildExecutableFixtureVersion(t, "1.0.0")
	v2 := buildExecutableFixtureVersion(t, "2.0.0")
	declaration := fixtureExecutableDeclaration(t, v1)
	if _, err := newPackageManager(dataDir).install(executablePackageSource(t, executableExtension{Declaration: declaration, EntryPoint: v1})); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	owner := openPackageOwner(t, dataDir, configPath)
	caller := packageCaller(t, owner)
	if _, err := owner.InstallExtensionPackage(context.Background(), caller, executablePackageSource(t, executableExtension{Declaration: fixtureExecutableDeclaration(t, v2), EntryPoint: v2})); err != nil {
		t.Fatalf("install v2: %v", err)
	}
	writeServiceFile(t, configPath, `{"extensions":{"fixture":{"enabled":true}}}`)
	if _, err := owner.Reload(context.Background(), caller); err != nil {
		t.Fatalf("reload: %v", err)
	}
	row := packageRow(t, owner, caller, "fixture")
	testassert.Equal(t, row.LoadedVersion, "1.0.0")
	testassert.Equal(t, row.SelectedVersion, "2.0.0")
	snapshot := owner.config.current()
	compiled, ok := snapshot.composition.compiled.Declaration("fixture")
	testassert.Require(t, ok, "reload did not compose the loaded fixture")
	testassert.Equal(t, compiled.Declaration.Version, "1.0.0")
	spec, ok := snapshot.composition.toolSpec("fixture.tool")
	testassert.Require(t, ok, "loaded fixture tool is not composed")
	reference := model.ModelRef{Provider: "provider", Model: "model"}
	description, err := spec.describe(context.Background(), Invocation{snapshot: snapshot}, reference, harness.ToolConstraints{}, harness.SessionIdentity{SessionID: "session-1", Workspace: "/workspace"}, harness.AgentInputs{Model: reference})
	if err != nil {
		t.Fatalf("describe loaded fixture: %v", err)
	}
	testassert.Equal(t, description.Definition.Name, "fixture.tool")
	testassert.NoError(t, owner.Close(context.Background()))
}

// TestPackageUnusableSelectionRejectsEnabledBuild proves a selected package
// that fails to load is ignored while disabled and rejects an enabled build.
func TestPackageUnusableSelectionRejectsEnabledBuild(t *testing.T) {
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	declaration := toolOnlyDeclaration()
	source := t.TempDir()
	if err := writePackageSourceFiles(t, source, declaration, "1.0.0", "worker"); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := newPackageManager(dataDir).install(source); err != nil {
		t.Fatalf("install: %v", err)
	}
	selected := readSelectedRecord(t, dataDir, declaration.ID)
	entryPath := filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "packages", selected.Digest, "worker")
	if err := os.Remove(entryPath); err != nil {
		t.Fatalf("remove package entry point: %v", err)
	}

	writeServiceFile(t, configPath, `{"extensions":{"peer":{"enabled":true}}}`)
	_, err := open(context.Background(), options{
		DataDir:       dataDir,
		Configuration: NewFileConfigurationSource(configPath),
		Credentials:   NewEnvironmentCredentialSource(filepath.Join(dataDir, ".env")),
	})
	testassert.Require(t, errors.Is(err, ErrConfiguration), "open with an unusable enabled package = %v, want configuration failure", err)

	writeServiceFile(t, configPath, `{"extensions":{"peer":{"enabled":false}}}`)
	owner := openPackageOwner(t, dataDir, configPath)
	caller := packageCaller(t, owner)
	row := packageRow(t, owner, caller, declaration.ID)
	testassert.Equal(t, row.LoadedVersion, "")
	testassert.Equal(t, row.SelectedVersion, "1.0.0")
	testassert.NoError(t, owner.Close(context.Background()))
}

// TestPackageListReadFailureIsError proves only a missing selection is the
// ordinary not-selected state; another read failure is an error.
func TestPackageListReadFailureIsError(t *testing.T) {
	dataDir := t.TempDir()
	declaration := toolOnlyDeclaration()
	source := t.TempDir()
	if err := writePackageSourceFiles(t, source, declaration, "1.0.0", "worker"); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := newPackageManager(dataDir).install(source); err != nil {
		t.Fatalf("install: %v", err)
	}
	selectedPath := filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "selected")
	if err := os.WriteFile(selectedPath, []byte(`{"id":"peer"}`), 0o600); err != nil {
		t.Fatalf("corrupt selection: %v", err)
	}
	_, err := newPackageManager(dataDir).list()
	testassert.Require(t, err != nil, "list with a corrupt selection succeeded")
}

// TestPackageRemovePathShapedIDHasNoDeletionAuthority proves the extension ID
// is hashed for every path, so a path-shaped ID cannot delete outside the
// extension directory or another package's selection.
func TestPackageRemovePathShapedIDHasNoDeletionAuthority(t *testing.T) {
	dataDir := t.TempDir()
	declaration := toolOnlyDeclaration()
	source := t.TempDir()
	if err := writePackageSourceFiles(t, source, declaration, "1.0.0", "worker"); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := newPackageManager(dataDir).install(source); err != nil {
		t.Fatalf("install: %v", err)
	}
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, `{"extensions":{"peer":{"enabled":false}}}`)
	owner := openPackageOwner(t, dataDir, configPath)
	caller := packageCaller(t, owner)
	outside := filepath.Join(dataDir, "extensions", "sentinel")
	if err := os.WriteFile(outside, []byte("sentinel"), 0o600); err != nil {
		t.Fatalf("outside sentinel: %v", err)
	}
	testassert.NoError(t, owner.RemoveExtensionPackage(context.Background(), caller, "../../sentinel"))
	if data, err := os.ReadFile(outside); err != nil || string(data) != "sentinel" {
		t.Fatalf("outside sentinel = %q, %v", data, err)
	}
	testassert.Equal(t, readSelectedRecord(t, dataDir, declaration.ID).Version, "1.0.0")
	testassert.NoError(t, owner.RemoveExtensionPackage(context.Background(), caller, declaration.ID))
	mustNotExist(t, filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "selected"), "remove retained the selected record")
	row := packageRow(t, owner, caller, declaration.ID)
	testassert.Equal(t, row.LoadedVersion, "1.0.0")
	testassert.Equal(t, row.SelectedVersion, "")
	testassert.NoError(t, owner.Close(context.Background()))
}

// TestPackageCommandsRequireOwnerScope proves the three package commands are
// owner-scoped and reject empty caller inputs as invalid.
func TestPackageCommandsRequireOwnerScope(t *testing.T) {
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, `{}`)
	owner := openPackageOwner(t, dataDir, configPath)
	narrow, err := owner.Caller(CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeSession, SessionID: canonicalTestSessionID}})
	if err != nil {
		t.Fatalf("narrow caller: %v", err)
	}
	_, err = owner.ListExtensionPackages(context.Background(), narrow)
	testassert.Require(t, errors.Is(err, ErrDenied), "narrow list = %v, want denied", err)
	_, err = owner.InstallExtensionPackage(context.Background(), narrow, t.TempDir())
	testassert.Require(t, errors.Is(err, ErrDenied), "narrow install = %v, want denied", err)
	err = owner.RemoveExtensionPackage(context.Background(), narrow, "peer")
	testassert.Require(t, errors.Is(err, ErrDenied), "narrow remove = %v, want denied", err)
	caller := packageCaller(t, owner)
	_, err = owner.InstallExtensionPackage(context.Background(), caller, "")
	testassert.Require(t, errors.Is(err, harness.ErrInvalid), "empty source install = %v, want invalid", err)
	err = owner.RemoveExtensionPackage(context.Background(), caller, "")
	testassert.Require(t, errors.Is(err, harness.ErrInvalid), "empty ID remove = %v, want invalid", err)
	testassert.NoError(t, owner.Close(context.Background()))
}

// canonicalTestSessionID is one shape-valid Session identity for narrow
// caller-grant tests.
const canonicalTestSessionID = "0123456789abcdef0123456789abcdef"
