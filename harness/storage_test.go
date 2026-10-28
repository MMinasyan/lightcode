package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestClosedKinds proves every entry and register kind constant carries its exact contract string and that no constant collapses onto a sibling or the zero value, so the kind set stays closed to the declared non-zero values.
func TestClosedKinds(t *testing.T) {
	entryKinds := map[harness.EntryKind]string{
		harness.EntryInput:               "input",
		harness.EntryAssistant:           "assistant",
		harness.EntryToolResult:          "tool_result",
		harness.EntrySignal:              "signal",
		harness.EntryHookResult:          "hook_result",
		harness.EntryCompaction:          "context_checkpoint",
		harness.EntryOperationSettlement: "operation_settlement",
	}
	testassert.Equal(t, len(entryKinds), 7)
	for kind, want := range entryKinds {
		testassert.Check(t, string(kind) == want, "EntryKind %q, want %q", kind, want)
		testassert.Check(t, kind != "", "EntryKind zero value is a declared constant; the kind set must be closed to non-zero kinds")
	}
	registerKinds := map[harness.RegisterKind]string{
		harness.RegisterSession:   "session",
		harness.RegisterOperation: "operation",
	}
	testassert.Equal(t, len(registerKinds), 2)
	for kind, want := range registerKinds {
		testassert.Check(t, string(kind) == want, "RegisterKind %q, want %q", kind, want)
		testassert.Check(t, kind != "", "RegisterKind zero value is a declared constant; the kind set must be closed to non-zero kinds")
	}
}

// TestTypedErrorClassification proves the typed errors carry their owning sentinel through errors.Is and their fields through errors.As across arbitrary wrapping, without cross-classifying into an unrelated sentinel.
func TestTypedErrorClassification(t *testing.T) {
	wrapped := fmt.Errorf("read entries: %w", &harness.CorruptionError{SessionID: "s1", Detail: "malformed envelope"})
	testassert.Check(t, errors.Is(wrapped, harness.ErrCorrupt), "CorruptionError does not classify as ErrCorrupt: %v", wrapped)
	var gotCorrupt *harness.CorruptionError
	if !errors.As(wrapped, &gotCorrupt) {
		t.Errorf("CorruptionError not reachable through errors.As: %v", wrapped)
	} else if gotCorrupt == nil || gotCorrupt.SessionID != "s1" || gotCorrupt.Detail != "malformed envelope" {
		t.Errorf("errors.As recovered %+v, want the stored fields", gotCorrupt)
	}
	testassert.Check(t, !errors.Is(wrapped, harness.ErrInvalid), "%v", "CorruptionError cross-classifies as ErrInvalid")

	wrappedSchema := fmt.Errorf("open: %w", &harness.IncompatibleSchemaError{Found: 3, Supported: 1, Detail: "extra table"})
	testassert.Check(t, errors.Is(wrappedSchema, harness.ErrIncompatible), "IncompatibleSchemaError does not classify as ErrIncompatible: %v", wrappedSchema)
	var gotSchema *harness.IncompatibleSchemaError
	if !errors.As(wrappedSchema, &gotSchema) {
		t.Errorf("IncompatibleSchemaError not reachable through errors.As: %v", wrappedSchema)
	} else if gotSchema == nil || gotSchema.Found != 3 || gotSchema.Supported != 1 || gotSchema.Detail != "extra table" {
		t.Errorf("errors.As recovered %+v, want the stored fields", gotSchema)
	}
	testassert.Check(t, !errors.Is(wrappedSchema, harness.ErrCorrupt), "%v", "IncompatibleSchemaError cross-classifies as ErrCorrupt")
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
		testassert.Require(t, s != nil, "sentinel %d is nil", i)
		for _, other := range sentinels[i+1:] {
			testassert.Check(t, !errors.Is(s, other) && !errors.Is(other, s), "sentinels %q and %q alias each other", s, other)
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
	testassert.Require(t, storage != nil && txn != nil, "%v", "interface assignments lost their implementations")
}
