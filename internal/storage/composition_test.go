package storage_test

// The storage foundation composition test composes both inactive storage
// implementations exactly as a future Harness caller will own them: from
// outside the storage package, through the public harness.Storage contract
// only, with no current Session, adapter, or production path. Memory needs no
// resource management; the SQLite store is closed through its concrete type
// because backend lifecycle intentionally is not part of the public storage
// interface.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestStorageComposition constructs memory and a temporary-file SQLite store
// through the public harness.Storage contract, runs one minimal transaction/
// read round trip on each, and closes the concrete SQLite resource.
func TestStorageComposition(t *testing.T) {
	backends := []struct {
		name  string
		open  func(t *testing.T) harness.Storage
		close func(t *testing.T, store harness.Storage)
	}{
		{
			name: "memory",
			open: func(t *testing.T) harness.Storage {
				return storage.NewMemory()
			},
		},
		{
			name: "sqlite",
			open: func(t *testing.T) harness.Storage {
				store, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "storage.db"))
				testassert.NoError(t, err)
				return store
			},
			close: func(t *testing.T, store harness.Storage) {
				sqlite, ok := store.(*storage.SQLite)
				if !ok {
					t.Fatalf("sqlite backend built %T, want *storage.SQLite", store)
					return
				}
				{
					err := sqlite.Close()
					testassert.Check(t, err == nil, "Close: %v", err)
				}
			},
		},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			ctx := context.Background()
			store := backend.open(t)
			if backend.close != nil {
				defer backend.close(t, store)
			}

			err := store.Transact(ctx, func(tx harness.Transaction) error {
				if _, err := tx.InsertRegister(harness.RegisterDraft{
					Key:     harness.RegisterKey{SessionID: "s1", Kind: harness.RegisterSession},
					Payload: json.RawMessage(`{"state":"active"}`),
				}); err != nil {
					return err
				}
				_, err := tx.InsertEntry(harness.EntryDraft{
					SessionID: "s1",
					ID:        "e1",
					Kind:      harness.EntryInput,
					Payload:   json.RawMessage(`{"text":"hello"}`),
				})
				return err
			})
			testassert.NoError(t, err)

			entries, err := store.ReadEntries(ctx, "s1", 0)
			testassert.NoError(t, err)
			testassert.Require(t, len(entries) == 1 && entries[0].ID == "e1" && entries[0].Sequence == 1 && entries[0].Kind == harness.EntryInput && string(entries[0].Payload) == `{"text":"hello"}`, "round-trip entries = %+v, want one input entry e1 at sequence 1 with its original payload", entries)

			register, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: "s1", Kind: harness.RegisterSession})
			testassert.NoError(t, err)
			testassert.Require(t, register.Revision == 1 && string(register.Payload) == `{"state":"active"}`, "round-trip register = %+v, want revision 1 with its original payload", register)

			ids, err := store.ListSessionIDs(ctx)
			testassert.NoError(t, err)
			testassert.Require(t, len(ids) == 1 && ids[0] == "s1", "ListSessionIDs = %v, want [s1]", ids)
		})
	}
}
