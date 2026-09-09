// Package sqlite declares Lightcode's production durable-session-storage
// plugin. It is an ordinary composition unit selected through static
// registration: the same Plugin/Instance contract an externally supplied
// capability implements, with no additional Runtime authority.
package sqlite

import (
	"context"
	"path/filepath"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/runtime"
)

// sessionStoreID is the capability ID under which the single export is
// declared.
const sessionStoreID = "session_store"

// Plugin returns the Runtime-scoped plugin whose single export is declared
// exactly as harness.Storage, the private Core storage binding identified by
// type. It has no dependencies and no settings validator.
func Plugin() runtime.Plugin {
	return runtime.Plugin{
		ID:    "sqlite",
		Scope: runtime.ScopeRuntime,
		Provides: []runtime.CapabilitySpec{
			runtime.Spec[harness.Storage](sessionStoreID),
		},
		Open: open,
	}
}

// open checks the scope context, opens the SQLite database derived from the
// owner data root, and returns the declared store plus its Close method.
func open(ctx context.Context, info runtime.ScopeInfo, _ runtime.Bindings) (runtime.Instance, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Instance{}, err
	}
	store, err := storage.OpenSQLite(filepath.Join(info.DataDir, "lightcode.db"))
	if err != nil {
		return runtime.Instance{}, err
	}
	return runtime.Instance{
		Values: map[string]any{sessionStoreID: store},
		Close:  store.Close,
	}, nil
}
