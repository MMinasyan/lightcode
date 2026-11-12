package melta

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/Syntheresis/melta/extension"
	"github.com/Syntheresis/melta/extension/worker"
	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/atomicfs"
	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/model"
	"github.com/Syntheresis/melta/protocol"
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

	t.Run("symlinked intermediate directory outside", func(t *testing.T) {
		dataDir := t.TempDir()
		source := writeSource(t, filepath.Join("bin", "worker"))
		outsideDir := t.TempDir()
		outside := filepath.Join(outsideDir, "outside-worker")
		if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatalf("outside executable: %v", err)
		}
		if err := os.Symlink(outsideDir, filepath.Join(source, "bin")); err != nil {
			t.Fatalf("symlink intermediate directory: %v", err)
		}
		_, err := newPackageManager(dataDir).install(source)
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "install symlinked intermediate directory = %v, want invalid", err)
		mustNotExist(t, filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "selected"), "a symlinked intermediate directory created a selection")
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

// TestPackageInstallAcceptsSymlinkedSourceAncestor proves the operator's
// source root is opened once as a root, so a symlinked path to the package
// directory is accepted while the two consumed files stay root-confined.
func TestPackageInstallAcceptsSymlinkedSourceAncestor(t *testing.T) {
	declaration := toolOnlyDeclaration()
	realRoot := t.TempDir()
	source := filepath.Join(realRoot, "package")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := writePackageSourceFiles(t, source, declaration, "1.0.0", "worker"); err != nil {
		t.Fatalf("write source: %v", err)
	}
	aliasRoot := t.TempDir()
	alias := filepath.Join(aliasRoot, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatalf("symlink source ancestor: %v", err)
	}
	dataDir := t.TempDir()
	if _, err := newPackageManager(dataDir).install(filepath.Join(alias, "package")); err != nil {
		t.Fatalf("install through a symlinked source ancestor: %v", err)
	}
	testassert.Equal(t, readSelectedRecord(t, dataDir, declaration.ID).Version, "1.0.0")
}

// TestPackageInstallAcceptsHardlinkedEntryPoint proves a hardlinked source
// entry point is not given a second refusal rule beyond the root's path
// containment.
func TestPackageInstallAcceptsHardlinkedEntryPoint(t *testing.T) {
	declaration := toolOnlyDeclaration()
	outside := filepath.Join(t.TempDir(), "outside-worker")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("outside executable: %v", err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), manifestWithEntry(t, declaration, "worker"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.Link(outside, filepath.Join(source, "worker")); err != nil {
		t.Fatalf("hardlink entry point: %v", err)
	}
	dataDir := t.TempDir()
	if _, err := newPackageManager(dataDir).install(source); err != nil {
		t.Fatalf("install hardlinked entry point: %v", err)
	}
	testassert.Equal(t, readSelectedRecord(t, dataDir, declaration.ID).Version, "1.0.0")
}

// TestPackageInstallAcceptsEntrySymlinkInsideSource proves a source entry
// symlink that resolves inside the opened root is accepted.
func TestPackageInstallAcceptsEntrySymlinkInsideSource(t *testing.T) {
	declaration := toolOnlyDeclaration()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), manifestWithEntry(t, declaration, "worker"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "real-worker"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("real entry point: %v", err)
	}
	if err := os.Symlink("real-worker", filepath.Join(source, "worker")); err != nil {
		t.Fatalf("inside entry symlink: %v", err)
	}
	dataDir := t.TempDir()
	if _, err := newPackageManager(dataDir).install(source); err != nil {
		t.Fatalf("install entry symlink inside source: %v", err)
	}
	testassert.Equal(t, readSelectedRecord(t, dataDir, declaration.ID).Version, "1.0.0")
}

// TestPackageInstallRejectsBindingSchemaExternalReference proves installation
// compiles every binding-definition schema, so a non-local $ref is refused
// before any selection exists.
func TestPackageInstallRejectsBindingSchemaExternalReference(t *testing.T) {
	credentials, ok := extension.HostCapability(extension.ContractCredentials)
	testassert.Require(t, ok, "credentials host capability is missing")
	credentials.Methods[0].Input = json.RawMessage(`{"$ref":"https://example.invalid/schema.json"}`)
	declaration := extension.Declaration{
		ID:           "bindingref",
		Version:      "1.0.0",
		ConfigSchema: json.RawMessage(`{"type":"object"}`),
		Components: []extension.Component{{
			ID:       "bindingref.component",
			Lifetime: extension.LifetimeHarness,
			Requires: []extension.Binding{{
				Name:         "credentials",
				Provider:     extension.HostProvider,
				CapabilityID: extension.ContractCredentials,
				Definition:   credentials,
			}},
		}},
	}
	dataDir := t.TempDir()
	source := executablePackageSource(t, executableExtension{Declaration: declaration, EntryPoint: "worker"})
	_, err := newPackageManager(dataDir).install(source)
	testassert.Require(t, errors.Is(err, harness.ErrInvalid), "install binding schema external reference = %v, want invalid", err)
	mustNotExist(t, filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "selected"), "a binding schema external reference created a selection")
}

// TestPackageSelectedDeclarationFailureIsUnusable proves a selected copy
// whose declaration fails compilation at load is recorded unusable: an
// enabled section rejects the build and a disabled one is ignored.
func TestPackageSelectedDeclarationFailureIsUnusable(t *testing.T) {
	declaration := toolOnlyDeclaration()
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	source := t.TempDir()
	if err := writePackageSourceFiles(t, source, declaration, "1.0.0", "worker"); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := newPackageManager(dataDir).install(source); err != nil {
		t.Fatalf("install: %v", err)
	}
	selected := readSelectedRecord(t, dataDir, declaration.ID)
	manifestPath := filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "packages", selected.Copy, "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read installed manifest: %v", err)
	}
	var manifest worker.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode installed manifest: %v", err)
	}
	manifest.Declaration.ConfigSchema = worker.JSONBytes("not-json")
	corrupt, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode corrupt manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt manifest: %v", err)
	}

	writeServiceFile(t, configPath, `{"extensions":{"peer":{"enabled":true}}}`)
	_, err = open(context.Background(), options{
		DataDir:       dataDir,
		Configuration: NewFileConfigurationSource(configPath),
		Credentials:   NewEnvironmentCredentialSource(filepath.Join(dataDir, ".env")),
	})
	testassert.Require(t, errors.Is(err, ErrConfiguration), "open with an enabled uncompilable package = %v, want configuration failure", err)

	writeServiceFile(t, configPath, `{"extensions":{"peer":{"enabled":false}}}`)
	owner := openPackageOwner(t, dataDir, configPath)
	caller := packageCaller(t, owner)
	row := packageRow(t, owner, caller, declaration.ID)
	testassert.Equal(t, row.LoadedVersion, "")
	testassert.Equal(t, row.SelectedVersion, "1.0.0")
	testassert.NoError(t, owner.Close(context.Background()))
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
// selected-record publication leaves the previous selection intact and removes
// the partial copy.
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
	first := readSelectedRecord(t, dataDir, declaration.ID)
	testassert.Equal(t, first.Version, "1.0.0")
	entries, readErr := os.ReadDir(filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "packages"))
	testassert.NoError(t, readErr)
	testassert.Require(t, len(entries) == 1 && entries[0].Name() == first.Copy, "package copies = %v, want only the selected copy %q", entries, first.Copy)

	if _, err := newPackageManager(dataDir).install(v2); err != nil {
		t.Fatalf("install v2 after restoring publication: %v", err)
	}
	testassert.Equal(t, readSelectedRecord(t, dataDir, declaration.ID).Version, "2.0.0")
}

// TestPackageInstallCreatesDistinctCopies proves install, update and
// reinstall each write their own copy and select the latest one.
func TestPackageInstallCreatesDistinctCopies(t *testing.T) {
	declaration := toolOnlyDeclaration()
	dataDir := t.TempDir()
	v1 := t.TempDir()
	if err := writePackageSourceFiles(t, v1, declaration, "1.0.0", "v1-worker"); err != nil {
		t.Fatalf("write v1 source: %v", err)
	}
	v2 := t.TempDir()
	if err := writePackageSourceFiles(t, v2, declaration, "2.0.0", "v2-worker"); err != nil {
		t.Fatalf("write v2 source: %v", err)
	}
	manager := newPackageManager(dataDir)
	copies := map[string]bool{}
	for _, step := range []struct {
		source  string
		version string
	}{
		{v1, "1.0.0"},
		{v2, "2.0.0"},
		{v2, "2.0.0"},
	} {
		if _, err := manager.install(step.source); err != nil {
			t.Fatalf("install %s: %v", step.version, err)
		}
		selected := readSelectedRecord(t, dataDir, declaration.ID)
		testassert.Equal(t, selected.Version, step.version)
		testassert.Require(t, !copies[selected.Copy], "install reused copy %q", selected.Copy)
		copies[selected.Copy] = true
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "packages"))
	testassert.NoError(t, err)
	testassert.Require(t, len(entries) == len(copies), "package copies = %v, want one per install", entries)
	for _, entry := range entries {
		testassert.Require(t, copies[entry.Name()], "unexpected copy directory %q", entry.Name())
	}
}

// TestPackageInstallSyncsCopyDirectoriesAndParentChain proves a nested entry
// point's created directories and its copy directory are each synced before
// the parent chain and the selection write.
func TestPackageInstallSyncsCopyDirectoriesAndParentChain(t *testing.T) {
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
	check := func(t *testing.T, trace []string, copyID string) {
		t.Helper()
		extensionDir := filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID))
		packagesDir := filepath.Join(extensionDir, "packages")
		packageDir := filepath.Join(packagesDir, copyID)
		testassert.Require(t, len(trace) == 8, "syncs = %v, want the entry point's directories, the copy directory, the parent chain, and the selection directory", trace)
		testassert.Equal(t, trace[0], filepath.Join(packageDir, "bin", "nested"))
		testassert.Equal(t, trace[1], filepath.Join(packageDir, "bin"))
		testassert.Equal(t, trace[2], packageDir)
		testassert.Equal(t, trace[3], packagesDir)
		testassert.Equal(t, trace[4], extensionDir)
		testassert.Equal(t, trace[5], filepath.Join(dataDir, "extensions"))
		testassert.Equal(t, trace[6], dataDir)
		testassert.Equal(t, trace[7], extensionDir)
	}
	if _, err := newPackageManager(dataDir).install(source); err != nil {
		t.Fatalf("install: %v", err)
	}
	first := readSelectedRecord(t, dataDir, declaration.ID)
	mu.Lock()
	trace := append([]string(nil), synced...)
	mu.Unlock()
	check(t, trace, first.Copy)

	mu.Lock()
	synced = nil
	mu.Unlock()
	if _, err := newPackageManager(dataDir).install(source); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	second := readSelectedRecord(t, dataDir, declaration.ID)
	mu.Lock()
	trace = append([]string(nil), synced...)
	mu.Unlock()
	testassert.Require(t, first.Copy != second.Copy, "reinstall reused copy %q", second.Copy)
	check(t, trace, second.Copy)
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

// TestInstalledWorkerPublicExtensionAcceptance exercises an actually installed
// worker through the public extension surface: discovery, concrete Harness- and
// Session-target invocations, exact declared results, package removal, a
// deliberate restart over the same store and configuration, retained history
// without replay, and Session-data cleanup that preserves the extension cache
// and configuration bytes.
func TestInstalledWorkerPublicExtensionAcceptance(t *testing.T) {
	binary := buildExecutableFixture(t)
	declaration := fixtureExecutableDeclaration(t, binary)
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		enableFixturePlugin(t, e.configPath, "fixture")
		e.server.setScript(func(_ context.Context, body string) []string {
			if lastUserText(body) == "worker work" {
				return textTurnEvents("worker done")
			}
			return textTurnEvents("done")
		})
		owner, err := e.openExecutablesWith(ctx, []executableExtension{{Declaration: declaration, EntryPoint: binary}})
		if err != nil {
			t.Fatalf("open installed fixture: %v", err)
		}
		defer func() { _ = owner.Close(ctx) }()
		caller := testCaller(owner)

		catalog, err := owner.ListExtensionCapabilities(ctx, caller)
		testassert.NoError(t, err)
		var entry *protocol.ExtensionCapabilityCatalog
		for i := range catalog {
			if catalog[i].ExtensionId == "fixture" {
				entry = &catalog[i]
				break
			}
		}
		testassert.Require(t, entry != nil, "catalog omitted the installed worker: %+v", catalog)
		var discoveredCommands, discoveredService bool
		for _, capability := range entry.Capabilities {
			switch capability.CapabilityId {
			case "fixture.commands":
				discoveredCommands = capability.ContractId == "fixture.commands/v1" && capability.ContractVersion == extension.ContractVersion && capability.Kind == protocol.ExtensionCapabilityKindService
			case "fixture.service":
				discoveredService = capability.ContractId == "fixture.service/v1" && capability.ContractVersion == extension.ContractVersion && capability.Kind == protocol.ExtensionCapabilityKindService
			}
		}
		testassert.Require(t, discoveredCommands && discoveredService, "catalog capabilities = %+v", entry.Capabilities)
		var sawCommand, sawNamespaces, sawFailure, sawRaw bool
		for _, exposed := range entry.Exposed {
			switch exposed.Method {
			case "command":
				sawCommand = exposed.CapabilityId == "fixture.commands" && exposed.Kind == protocol.SessionWork && exposed.Target == protocol.ExtensionExposureTargetSession
			case "namespaces":
				sawNamespaces = exposed.CapabilityId == "fixture.commands" && exposed.Kind == protocol.Query && exposed.Target == protocol.ExtensionExposureTargetSession
			case "fail":
				sawFailure = exposed.CapabilityId == "fixture.service" && exposed.Kind == protocol.Query && exposed.Target == protocol.ExtensionExposureTargetHarness
			case "raw":
				sawRaw = exposed.CapabilityId == "fixture.service" && exposed.Kind == protocol.Query && exposed.Target == protocol.ExtensionExposureTargetHarness
			}
		}
		testassert.Require(t, sawCommand && sawNamespaces && sawFailure && sawRaw, "catalog exposures = %+v", entry.Exposed)

		rawValue := "{\n  \"big\": 9007199254740993,\n  \"escaped\": \"line\\n\\t\\\"quote\\\" \\\\ slash\"\n}"
		raw, err := owner.InvokeExtension(ctx, caller, fixtureRequest("fixture", "fixture.service", "fixture.service/v1", "raw", harnessTarget(), rawValue, "req-worker-raw"))
		testassert.NoError(t, err)
		testassert.Require(t, raw.Failure == nil && string(raw.Value) == rawValue, "raw worker result = %+v, want the exact raw bytes", raw)

		failure, err := owner.InvokeExtension(ctx, caller, fixtureRequest("fixture", "fixture.service", "fixture.service/v1", "fail", harnessTarget(), `{}`, "req-worker-fail"))
		testassert.NoError(t, err)
		wantData := "{\n  \"big\": 9007199254740993\n}"
		wantPartial := `["partial", null, 1]`
		testassert.Require(t, failure.Failure != nil && failure.Failure.Code == "guest_code" && failure.Failure.Message == "guest message" && string(failure.Failure.Data) == wantData && string(failure.Failure.Partial) == wantPartial && failure.Value == nil, "declared worker failure = %+v, want exact data/partial", failure.Failure)

		selection := &protocol.AgentSelection{Inputs: &protocol.AgentInputs{
			Model:        "prov/m",
			Instructions: "worker public extension",
			AssemblyMode: protocol.AgentAssemblyNone,
			Tools:        []string{},
			Capabilities: []string{},
			Constraints:  protocol.ToolConstraints{},
			Strategy:     protocol.StrategySelection{Id: "", Settings: "{}"},
		}}
		session, err := owner.CreateSession(ctx, caller, e.workspace("worker-public"), selection)
		testassert.NoError(t, err)
		sessionCaller, err := owner.Caller(CallerContext{
			Origin: harness.InputOriginUser,
			Scope:  CallerScope{Kind: CallerScopeSession, SessionID: session.SessionId},
		})
		testassert.NoError(t, err)

		namespaces, err := owner.InvokeExtension(ctx, sessionCaller, fixtureRequest("fixture", "fixture.commands", "fixture.commands/v1", "namespaces", sessionTarget(session.SessionId), `{}`, "req-worker-namespaces"))
		testassert.NoError(t, err)
		testassert.Require(t, namespaces.Failure == nil, "namespaces failure = %+v", namespaces.Failure)
		var directories struct {
			SessionDirectory string `json:"session_directory"`
			CacheDirectory   string `json:"cache_directory"`
		}
		testassert.NoError(t, json.Unmarshal(namespaces.Value, &directories))
		testassert.Require(t, directories.SessionDirectory != "" && directories.CacheDirectory != "", "namespaces = %s", namespaces.Value)
		testassert.NoError(t, os.WriteFile(filepath.Join(directories.SessionDirectory, "session.sentinel"), []byte("session"), 0o600))
		testassert.NoError(t, os.WriteFile(filepath.Join(directories.CacheDirectory, "cache.sentinel"), []byte("cache"), 0o600))

		submitted, err := owner.InvokeExtension(ctx, sessionCaller, fixtureRequest("fixture", "fixture.commands", "fixture.commands/v1", "command", sessionTarget(session.SessionId), `{"method":"submit","arguments":{"operation_id":"op-worker-extension","mode":"regular","content":[{"kind":"text","text":"worker work"}]}}`, "req-worker-submit"))
		testassert.NoError(t, err)
		testassert.Require(t, submitted.Failure == nil, "submit failure = %+v", submitted.Failure)
		awaitOperation(t, owner, session.SessionId, "op-worker-extension", harness.OperationSuccess)

		page, err := owner.GetHistory(ctx, caller, session.SessionId, nil)
		testassert.NoError(t, err)
		input, ok := workerExtensionInput(page, "op-worker-extension")
		testassert.Require(t, ok && input.Origin == protocol.InputOriginPlugin && input.ExtensionId != nil && *input.ExtensionId == "fixture", "admitted input attribution = %+v", input)

		configBefore, err := os.ReadFile(e.configPath)
		testassert.NoError(t, err)
		testassert.NoError(t, owner.RemoveExtensionPackage(ctx, caller, "fixture"))
		configAfterRemoval, err := os.ReadFile(e.configPath)
		testassert.NoError(t, err)
		testassert.Require(t, string(configAfterRemoval) == string(configBefore), "package removal rewrote the configuration")
		testassert.NoError(t, owner.Close(ctx))

		restarted, err := e.openExecutablesWith(ctx, nil)
		if err != nil {
			t.Fatalf("reopen after package removal: %v", err)
		}
		defer func() { _ = restarted.Close(ctx) }()
		restartedCaller := testCaller(restarted)
		catalog, err = restarted.ListExtensionCapabilities(ctx, restartedCaller)
		testassert.NoError(t, err)
		for _, candidate := range catalog {
			testassert.Require(t, candidate.ExtensionId != "fixture", "removed worker is still cataloged: %+v", candidate)
		}
		_, err = restarted.InvokeExtension(ctx, restartedCaller, fixtureRequest("fixture", "fixture.commands", "fixture.commands/v1", "namespaces", sessionTarget(session.SessionId), `{}`, "req-worker-removed"))
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "invocation after package removal = %v, want not-found", err)

		page, err = restarted.GetHistory(ctx, restartedCaller, session.SessionId, nil)
		testassert.NoError(t, err)
		input, ok = workerExtensionInput(page, "op-worker-extension")
		testassert.Require(t, ok && input.ExtensionId != nil && *input.ExtensionId == "fixture", "reopened history = %+v, want the retained extension attribution", input)
		if data, err := os.ReadFile(filepath.Join(directories.SessionDirectory, "session.sentinel")); err != nil || string(data) != "session" {
			t.Fatalf("Session sentinel after removal = (%q, %v), want preserved", data, err)
		}

		_, err = restarted.ArchiveSession(ctx, restartedCaller, session.SessionId)
		testassert.NoError(t, err)
		deletion, err := restarted.DeleteSession(ctx, restartedCaller, session.SessionId)
		testassert.NoError(t, err)
		testassert.Require(t, len(deletion.CleanupFailures) == 0, "deletion cleanup failures = %+v", deletion.CleanupFailures)
		mustNotExist(t, filepath.Join(e.dataDir, "code", session.SessionId), "the deleted Session tree")
		if data, err := os.ReadFile(filepath.Join(directories.CacheDirectory, "cache.sentinel")); err != nil || string(data) != "cache" {
			t.Fatalf("cache after Session deletion = (%q, %v), want preserved", data, err)
		}
		configAfter, err := os.ReadFile(e.configPath)
		testassert.NoError(t, err)
		testassert.Require(t, string(configAfter) == string(configBefore), "Session deletion or restart rewrote the configuration")
		testassert.NoError(t, restarted.Close(ctx))
	})
}

// workerExtensionInput finds the admitted input of one Operation in a history
// page.
func workerExtensionInput(page protocol.HistoryPage, operationID string) (protocol.InputItem, bool) {
	for _, item := range page.Items {
		input, err := item.AsInputItem()
		if err != nil {
			continue
		}
		if input.OperationId != nil && *input.OperationId == operationID {
			return input, true
		}
	}
	return protocol.InputItem{}, false
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

// TestPackageLifecycleUsesCanonicalDataRoot proves package load, install,
// list, restart and remove all use the canonical root captured at open when
// the operator-supplied DataDir is a symlink: the real and alias paths contend
// for one owner, and repointing the alias cannot redirect later selections.
func TestPackageLifecycleUsesCanonicalDataRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("package paths use the Linux protected filesystem traversal")
	}
	realRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "data")
	testassert.NoError(t, os.Symlink(realRoot, alias))
	replacement := t.TempDir()
	configPath := filepath.Join(realRoot, "config.json")
	writeServiceFile(t, configPath, `{"extensions":{"fixture":{"enabled":false}}}`)
	v1 := buildExecutableFixtureVersion(t, "1.0.0")
	installExecutablePackage(t, realRoot, executableExtension{Declaration: fixtureExecutableDeclaration(t, v1), EntryPoint: v1})

	owner := openPackageOwner(t, alias, configPath)
	want := canonicalFixturePath(t, realRoot)
	testassert.Equal(t, owner.dataDir, want)
	caller := packageCaller(t, owner)
	row := packageRow(t, owner, caller, "fixture")
	testassert.Equal(t, row.LoadedVersion, "1.0.0")
	testassert.Equal(t, row.SelectedVersion, "1.0.0")

	// The real path and the alias are one ownership identity.
	{
		_, err := open(context.Background(), options{
			DataDir:       realRoot,
			Configuration: NewFileConfigurationSource(configPath),
			Credentials:   NewEnvironmentCredentialSource(filepath.Join(realRoot, ".env")),
		})
		testassert.Require(t, errors.Is(err, ErrOwned), "real-path contender open = %v, want ErrOwned", err)
	}

	// Repointing the operator's alias cannot redirect the open owner's writes.
	testassert.NoError(t, os.Remove(alias))
	testassert.NoError(t, os.Symlink(replacement, alias))
	v2 := buildExecutableFixtureVersion(t, "2.0.0")
	installed, err := owner.InstallExtensionPackage(context.Background(), caller, executablePackageSource(t, executableExtension{Declaration: fixtureExecutableDeclaration(t, v2), EntryPoint: v2}))
	testassert.NoError(t, err)
	testassert.Equal(t, installed.LoadedVersion, "1.0.0")
	testassert.Equal(t, installed.SelectedVersion, "2.0.0")
	testassert.Equal(t, owner.dataDir, want)
	testassert.Equal(t, readSelectedRecord(t, realRoot, "fixture").Version, "2.0.0")
	{
		entries, err := os.ReadDir(replacement)
		testassert.NoError(t, err)
		testassert.Require(t, len(entries) == 0, "repointed alias target = %v, want no redirected package writes", entries)
	}
	testassert.NoError(t, owner.Close(context.Background()))

	restarted := openPackageOwner(t, realRoot, configPath)
	restartedCaller := packageCaller(t, restarted)
	row = packageRow(t, restarted, restartedCaller, "fixture")
	testassert.Equal(t, row.LoadedVersion, "2.0.0")
	testassert.Equal(t, row.SelectedVersion, "2.0.0")
	testassert.NoError(t, restarted.RemoveExtensionPackage(context.Background(), restartedCaller, "fixture"))
	mustNotExist(t, filepath.Join(realRoot, "extensions", extensionDataHash("fixture"), "selected"), "remove retained the selected record")
	{
		entries, err := os.ReadDir(replacement)
		testassert.NoError(t, err)
		testassert.Require(t, len(entries) == 0, "repointed alias target = %v, want no redirected selection writes", entries)
	}
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
	entryPath := filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "packages", selected.Copy, "worker")
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
