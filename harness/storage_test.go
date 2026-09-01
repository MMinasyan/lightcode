package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
)

// TestClosedKinds proves every entry and register kind constant carries its exact contract string and that no constant collapses onto a sibling or the zero value, so the kind set stays closed to the declared non-zero values.
func TestClosedKinds(t *testing.T) {
	entryKinds := map[harness.EntryKind]string{
		harness.EntryInput:               "input",
		harness.EntryAssistant:           "assistant",
		harness.EntryToolResult:          "tool_result",
		harness.EntrySignal:              "signal",
		harness.EntryHookResult:          "hook_result",
		harness.EntryCompaction:          "compaction",
		harness.EntryOperationSettlement: "operation_settlement",
	}
	if len(entryKinds) != 7 {
		t.Fatalf("entry kind constants collapse to %d distinct values, want 7", len(entryKinds))
	}
	for kind, want := range entryKinds {
		if string(kind) != want {
			t.Errorf("EntryKind %q, want %q", kind, want)
		}
		if kind == "" {
			t.Errorf("EntryKind zero value is a declared constant; the kind set must be closed to non-zero kinds")
		}
	}
	registerKinds := map[harness.RegisterKind]string{
		harness.RegisterSession:   "session",
		harness.RegisterOperation: "operation",
	}
	if len(registerKinds) != 2 {
		t.Fatalf("register kind constants collapse to %d distinct values, want 2", len(registerKinds))
	}
	for kind, want := range registerKinds {
		if string(kind) != want {
			t.Errorf("RegisterKind %q, want %q", kind, want)
		}
		if kind == "" {
			t.Errorf("RegisterKind zero value is a declared constant; the kind set must be closed to non-zero kinds")
		}
	}
}

// TestTypedErrorClassification proves the typed errors carry their owning sentinel through errors.Is and their fields through errors.As across arbitrary wrapping, without cross-classifying into an unrelated sentinel.
func TestTypedErrorClassification(t *testing.T) {
	wrapped := fmt.Errorf("read entries: %w", &harness.CorruptionError{SessionID: "s1", Detail: "malformed envelope"})
	if !errors.Is(wrapped, harness.ErrCorrupt) {
		t.Errorf("CorruptionError does not classify as ErrCorrupt: %v", wrapped)
	}
	var gotCorrupt *harness.CorruptionError
	if !errors.As(wrapped, &gotCorrupt) {
		t.Errorf("CorruptionError not reachable through errors.As: %v", wrapped)
	} else if gotCorrupt == nil || gotCorrupt.SessionID != "s1" || gotCorrupt.Detail != "malformed envelope" {
		t.Errorf("errors.As recovered %+v, want the stored fields", gotCorrupt)
	}
	if errors.Is(wrapped, harness.ErrInvalid) {
		t.Error("CorruptionError cross-classifies as ErrInvalid")
	}

	wrappedSchema := fmt.Errorf("open: %w", &harness.IncompatibleSchemaError{Found: 3, Supported: 1, Detail: "extra table"})
	if !errors.Is(wrappedSchema, harness.ErrIncompatible) {
		t.Errorf("IncompatibleSchemaError does not classify as ErrIncompatible: %v", wrappedSchema)
	}
	var gotSchema *harness.IncompatibleSchemaError
	if !errors.As(wrappedSchema, &gotSchema) {
		t.Errorf("IncompatibleSchemaError not reachable through errors.As: %v", wrappedSchema)
	} else if gotSchema == nil || gotSchema.Found != 3 || gotSchema.Supported != 1 || gotSchema.Detail != "extra table" {
		t.Errorf("errors.As recovered %+v, want the stored fields", gotSchema)
	}
	if errors.Is(wrappedSchema, harness.ErrCorrupt) {
		t.Error("IncompatibleSchemaError cross-classifies as ErrCorrupt")
	}
}

// TestErrorSentinels proves all six outcome sentinels exist and stay pairwise distinct, so no implementation can alias two outcome classes.
func TestErrorSentinels(t *testing.T) {
	sentinels := []error{
		harness.ErrInvalid,
		harness.ErrNotFound,
		harness.ErrConflict,
		harness.ErrCorrupt,
		harness.ErrIncompatible,
		harness.ErrStorage,
	}
	for i, s := range sentinels {
		if s == nil {
			t.Fatalf("sentinel %d is nil", i)
		}
		for _, other := range sentinels[i+1:] {
			if errors.Is(s, other) || errors.Is(other, s) {
				t.Errorf("sentinels %q and %q alias each other", s, other)
			}
		}
	}
}

// shapeStorage and shapeTransaction implement the declared method sets exactly; the assignments below fail compilation the moment the public interface shape drifts.
type shapeStorage struct{}

func (shapeStorage) ReadEntries(context.Context, string, int64) ([]harness.Entry, error) {
	return nil, nil
}

func (shapeStorage) ReadRegister(context.Context, harness.RegisterKey) (harness.Register, error) {
	return harness.Register{}, nil
}

func (shapeStorage) ReadRegisters(context.Context, string) ([]harness.Register, error) {
	return nil, nil
}

func (shapeStorage) ListSessionIDs(context.Context) ([]string, error) { return nil, nil }

func (shapeStorage) Transact(context.Context, func(harness.Transaction) error) error { return nil }

type shapeTransaction struct{}

func (shapeTransaction) ReadEntries(string, int64) ([]harness.Entry, error) { return nil, nil }

func (shapeTransaction) ReadRegister(harness.RegisterKey) (harness.Register, error) {
	return harness.Register{}, nil
}

func (shapeTransaction) InsertEntry(harness.EntryDraft) (harness.Entry, error) {
	return harness.Entry{}, nil
}

func (shapeTransaction) InsertRegister(harness.RegisterDraft) (harness.Register, error) {
	return harness.Register{}, nil
}

func (shapeTransaction) ReplaceRegister(harness.RegisterKey, int64, json.RawMessage) (harness.Register, error) {
	return harness.Register{}, nil
}

func (shapeTransaction) DeleteSession(string) error { return nil }

var (
	_ harness.Storage     = shapeStorage{}
	_ harness.Transaction = shapeTransaction{}
)

// TestInterfaceShape holds the compile-time method-set proof; the runtime body keeps the check reported as an ordinary passing test.
func TestInterfaceShape(t *testing.T) {
	var storage harness.Storage = shapeStorage{}
	var txn harness.Transaction = shapeTransaction{}
	if storage == nil || txn == nil {
		t.Fatal("interface assignments lost their implementations")
	}
}
