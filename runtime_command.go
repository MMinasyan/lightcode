package main

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/melta"
	"github.com/MMinasyan/lightcode/runtime"
)

//go:embed runtime_identity.md
var runtimeIdentity string

func installedExtensions() ([]string, []string) {
	var ids []string
	for _, plugin := range builtin.Plugins() {
		ids = append(ids, plugin.Declaration.ID)
	}
	return ids, builtin.DefaultCapabilityIDs()
}

// runRuntime runs the shared managed MELTA owner and its thin protocol host in
// the foreground for the lifetime of the signal context. It supplies the
// current application paths and the first-run configuration document, opens
// the owner with the host withdrawal callback, attaches the protocol and
// publishes the discovery record once, then waits for the shutdown signal and
// joins the managed cleanup. Contention exits quietly
// without initialization; every startup, publication and cleanup error keeps
// its identity for exit 1.
func runRuntime(ctx context.Context, dataDir, configPath string) (err error) {
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dataRoot := filepath.Join(home, ".lightcode")
	locations := melta.Locations{
		GlobalRulesDir:     dataRoot,
		DiscoveryCacheDir:  filepath.Join(dataRoot, "cache", "discovery"),
		WorkspacePolicyDir: filepath.Join(dataRoot, "projects"),
	}
	if os.Getenv("LIGHTCODE_DEBUG_WIRE") == "1" {
		locations.WireDebugDir = filepath.Join(dataRoot, "debug", "wire")
	}
	if err := runtime.InitializeFirstRunConfiguration(absConfigPath); err != nil {
		return err
	}
	host := runtime.NewProtocolHost()
	owner, err := melta.Open(ctx, melta.Options{
		DataDir:       absDataDir,
		Configuration: melta.NewFileConfigurationSource(absConfigPath),
		Credentials:   melta.NewEnvironmentCredentialSource(filepath.Join(dataRoot, ".env")),
		Locations:     locations,
		Extensions:    builtin.Plugins(),
		Identity:      runtimeIdentity,
		BeforeRelease: host.BeforeRelease,
	})
	if errors.Is(err, melta.ErrOwned) {
		// Another process owns the data directory; this contender
		// initializes nothing and exits without a diagnostic.
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		// The required cleanup always runs on its own wait — the canceled
		// signal context never bounds it — and its result joins the return.
		err = errors.Join(err, owner.Close(context.Background()))
	}()
	server, err := host.OpenProtocol(ctx, owner)
	if err != nil {
		return err
	}
	if err := server.PublishDiscovery(filepath.Join(absDataDir, "runtime.json")); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}
