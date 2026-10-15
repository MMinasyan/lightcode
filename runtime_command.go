package main

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/runtime"
)

// runRuntime runs the shared managed Runtime owner in the foreground for the
// lifetime of the signal context. It opens the owner over the normalized
// paths with the shipped plugin set, attaches the protocol and publishes the
// discovery record once, then waits for the shutdown signal and joins the
// managed cleanup. Contention exits quietly without initialization; every
// startup, publication and cleanup error keeps its identity for exit 1.
func runRuntime(ctx context.Context, dataDir, configPath string) (err error) {
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	owner, err := runtime.Open(ctx, runtime.Options{
		DataDir:    absDataDir,
		ConfigPath: absConfigPath,
		Plugins:    builtin.Plugins(),
	})
	if errors.Is(err, runtime.ErrOwned) {
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
	server, err := owner.OpenProtocol(ctx)
	if err != nil {
		return err
	}
	if err := server.PublishDiscovery(filepath.Join(absDataDir, "runtime.json")); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}
