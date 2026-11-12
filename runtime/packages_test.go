package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Syntheresis/melta/extension"
	"github.com/Syntheresis/melta/extension/worker"
	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/protocol"
)

// TestProtocolMountedPackageOperations proves the generated schema-first
// operations reach the owner's package manager over the mounted listener:
// install, list and remove.
func TestProtocolMountedPackageOperations(t *testing.T) {
	ctx := context.Background()
	_, _, server := mountedOwner(t, ctx, testOwnerOptions(t))
	client := mountedClient(t, server)

	declaration := extension.Declaration{
		ID:           "wire",
		Version:      "1.0.0",
		ConfigSchema: json.RawMessage(`{"type":"object"}`),
		Components: []extension.Component{{
			ID:       "wire.component",
			Lifetime: extension.LifetimeHarness,
			Provides: []string{"wire.tool"},
		}},
		Capabilities: []extension.Capability{extension.ToolCapability("wire.tool")},
	}
	manifest, err := worker.EncodeManifest(declaration, "worker")
	testassert.NoError(t, err)
	source := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), manifest, 0o600))
	testassert.NoError(t, os.WriteFile(filepath.Join(source, "worker"), []byte("#!/bin/sh\nexit 0\n"), 0o700))

	installed, err := client.InstallExtensionPackageWithResponse(ctx, protocol.InstallExtensionPackageJSONRequestBody{SourceDir: source})
	testassert.NoError(t, err)
	testassert.Require(t, installed.JSON200 != nil, "install returned no package: %s", installed.Body)
	testassert.Equal(t, installed.JSON200.Id, "wire")
	testassert.Equal(t, installed.JSON200.LoadedVersion, "")
	testassert.Equal(t, installed.JSON200.SelectedVersion, "1.0.0")

	listed, err := client.ListExtensionPackagesWithResponse(ctx)
	testassert.NoError(t, err)
	testassert.Require(t, listed.JSON200 != nil && len(*listed.JSON200) == 1, "list = %s, want one package", listed.Body)
	testassert.Equal(t, (*listed.JSON200)[0].SelectedVersion, "1.0.0")

	removed, err := client.RemoveExtensionPackageWithResponse(ctx, &protocol.RemoveExtensionPackageParams{Id: "wire"})
	testassert.NoError(t, err)
	testassert.Equal(t, removed.StatusCode(), 204)

	listed, err = client.ListExtensionPackagesWithResponse(ctx)
	testassert.NoError(t, err)
	testassert.Require(t, listed.JSON200 != nil && len(*listed.JSON200) == 0, "list after remove = %s, want no remaining selection", listed.Body)
}
