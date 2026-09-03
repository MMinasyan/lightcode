// Package harness defines the public durable storage contract consumed by the
// future Harness layer. Storage sees generic envelope fields and opaque JSON
// payload bytes: it validates envelope structure and JSON syntax, while payload
// semantics and recovery meaning belong to later Harness work.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// EntryKind closes the set of entry envelope kinds.
type EntryKind string

const (
	EntryInput               EntryKind = "input"
	EntryAssistant           EntryKind = "assistant"
	EntryToolResult          EntryKind = "tool_result"
	EntrySignal              EntryKind = "signal"
	EntryHookResult          EntryKind = "hook_result"
	EntryCompaction          EntryKind = "compaction"
	EntryOperationSettlement EntryKind = "operation_settlement"
)

// RegisterKind closes the set of register envelope kinds.
type RegisterKind string

const (
	RegisterSession   RegisterKind = "session"
	RegisterOperation RegisterKind = "operation"
)

// EntryDraft is a new entry offered for insertion. It carries no caller-supplied
// sequence or commit time: the transaction assigns both inside its lifetime.
type EntryDraft struct {
	SessionID string
	ID        string

	// OperationID is optional: independently copied fork-prefix entries omit
	// source ownership.
	OperationID string

	Kind EntryKind

	// Payload must be syntactically valid JSON and stays semantically opaque
	// for storage.
	Payload json.RawMessage
}

// Entry is one immutable committed entry. It is never replaced or deleted
// independently.
type Entry struct {
	SessionID   string
	ID          string
	Sequence    int64
	OperationID string
	Kind        EntryKind
	CommittedAt time.Time
	Payload     json.RawMessage
}

// RegisterKey addresses one Session or Operation register. A Session-register
// key has an empty OperationID; an Operation-register key requires one.
type RegisterKey struct {
	SessionID   string
	Kind        RegisterKind
	OperationID string
}

// RegisterDraft is a new register offered for insertion.
type RegisterDraft struct {
	Key RegisterKey

	// Payload must be syntactically valid JSON and stays semantically opaque
	// for storage.
	Payload json.RawMessage
}

// Register is one mutable register. Replacement changes the complete opaque
// payload and never merges fields.
type Register struct {
	Key      RegisterKey
	Revision int64
	Payload  json.RawMessage
}

// Storage is the read side plus the single mutation path. Every returned value
// owns its payload bytes: caller mutation cannot change stored state or another
// returned value. All implementations preserve context.Canceled and
// context.DeadlineExceeded when applicable.
type Storage interface {
	// ReadEntries returns entries of one Session with sequence strictly greater
	// than after, in ascending sequence; zero reads from the beginning.
	ReadEntries(context.Context, string, int64) ([]Entry, error)

	// ReadRegister returns exactly the addressed Session or Operation register.
	ReadRegister(context.Context, RegisterKey) (Register, error)

	// ReadRegisters returns the Session register first, then Operation
	// registers ordered by Operation identity.
	ReadRegisters(context.Context, string) ([]Register, error)

	// ListSessionIDs returns the sorted union of Session identities present in
	// entry or register envelopes.
	ListSessionIDs(context.Context) ([]string, error)

	// Transact is the only mutation path; the callback is the complete
	// transaction lifetime and may address multiple Sessions.
	Transact(context.Context, func(Transaction) error) error
}

// Transaction is one serialized mutation lifetime. Using a retained Transaction
// after its callback returns is invalid.
type Transaction interface {
	ReadEntries(string, int64) ([]Entry, error)

	ReadRegister(RegisterKey) (Register, error)

	// InsertEntry assigns the next strictly increasing per-Session sequence and
	// UTC commit time inside the transaction.
	InsertEntry(EntryDraft) (Entry, error)

	// InsertRegister requires absence and assigns revision 1 inside the
	// transaction.
	InsertRegister(RegisterDraft) (Register, error)

	// ReplaceRegister requires the exact current revision supplied as its
	// second argument, replaces the whole payload, and assigns the next
	// revision inside the transaction.
	ReplaceRegister(RegisterKey, int64, json.RawMessage) (Register, error)

	// DeleteSession atomically removes one Session register, every Operation
	// register, and every entry with the same Session identity.
	DeleteSession(string) error
}

// Error outcome classes shared by every implementation.
var (
	// ErrInvalid wraps invalid caller envelopes, malformed keys, and use of an
	// expired transaction.
	ErrInvalid = errors.New("harness: invalid storage request")

	// ErrNotFound wraps missing addressed records.
	ErrNotFound = errors.New("harness: storage record not found")

	// ErrConflict wraps duplicate identities or registers and stale revisions.
	ErrConflict = errors.New("harness: storage conflict")

	// ErrCorrupt is unwrapped by CorruptionError.
	ErrCorrupt = errors.New("harness: corrupt storage envelope")

	// ErrIncompatible is unwrapped by IncompatibleSchemaError.
	ErrIncompatible = errors.New("harness: incompatible storage schema")

	// ErrStorage wraps database, transaction, or integrity failures that cannot
	// be classified above.
	ErrStorage = errors.New("harness: storage failure")
)

// CorruptionError reports one malformed persisted envelope owned by one
// Session. Invalid caller input rejected before persistence is not durable
// corruption and never takes this shape.
type CorruptionError struct {
	SessionID string
	Detail    string
}

func (e *CorruptionError) Error() string {
	return fmt.Sprintf("harness: corrupt envelope in session %q: %s", e.SessionID, e.Detail)
}

func (e *CorruptionError) Unwrap() error { return ErrCorrupt }

// IncompatibleSchemaError reports an unsupported or noncanonical database
// schema found while opening storage.
type IncompatibleSchemaError struct {
	Found     int
	Supported int
	Detail    string
}

func (e *IncompatibleSchemaError) Error() string {
	return fmt.Sprintf("harness: incompatible schema version %d (supported %d): %s", e.Found, e.Supported, e.Detail)
}

func (e *IncompatibleSchemaError) Unwrap() error { return ErrIncompatible }
