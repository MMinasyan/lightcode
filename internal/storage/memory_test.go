package storage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
)

// TestMemoryConformance runs the shared storage conformance suite against the
// in-memory implementation.
func TestMemoryConformance(t *testing.T) {
	runConformance(t, memoryBackend())
}

// TestMemoryReadCancellationUnderWriterLock proves a read blocked by the
// memory writer lock observes cancellation while it waits: memory serializes
// reads behind an open transaction, so the context must be rechecked after
// the lock is acquired and before the state is read. The read's deadline is
// arranged to expire strictly while the writer is still held. This blocking
// behavior is memory-specific; backends whose reads do not wait on the writer
// are not required to reproduce it.
func TestMemoryReadCancellationUnderWriterLock(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()
	confCreateSession(t, ctx, store, "s1")

	inside := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- store.Transact(ctx, func(txn harness.Transaction) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside // the writer transaction holds the memory lock

	readCtx, cancelRead := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelRead()
	readDone := make(chan error, 4)
	reads := map[string]func() error{
		"ReadEntries":    func() error { _, err := store.ReadEntries(readCtx, "s1", 0); return err },
		"ReadRegister":   func() error { _, err := store.ReadRegister(readCtx, confSessionKey("s1")); return err },
		"ReadRegisters":  func() error { _, err := store.ReadRegisters(readCtx, "s1"); return err },
		"ListSessionIDs": func() error { _, err := store.ListSessionIDs(readCtx); return err },
	}
	for _, read := range reads {
		go func(read func() error) {
			readDone <- read()
		}(read)
	}

	<-readCtx.Done() // the deadline expires while the reads still wait for the writer
	close(release)

	if err := <-first; err != nil {
		t.Fatalf("writer transaction: %v", err)
	}
	for range reads {
		if err := <-readDone; !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("blocked read error = %v, want context.DeadlineExceeded", err)
		}
	}
}

// fixtureTime is a deterministic valid commit time for fixture-injected
// envelopes.
var fixtureTime = time.Unix(1_700_000_000, 0).UTC()

// memoryBackend wires the in-memory implementation into the shared suite. Its
// fixtures mutate private committed state under the store lock, standing in
// for the raw durable-state mutations a file-backed backend performs.
func memoryBackend() conformanceBackend {
	return conformanceBackend{
		name:     "memory",
		newStore: func(t *testing.T) harness.Storage { return NewMemory() },
		corruptEntry: func(t *testing.T, store harness.Storage, sessionID string) {
			m := store.(*Memory)
			m.mu.Lock()
			defer m.mu.Unlock()
			sequence := maxSequence(m.state.entries[sessionID]) + 1
			m.state.entries[sessionID] = append(m.state.entries[sessionID], harness.Entry{
				SessionID: sessionID,
				ID:        "corrupt-entry",
				Sequence:  sequence,
				Kind:      harness.EntryInput,
				// Valid in every envelope field except the zero commit time.
				CommittedAt: time.Time{},
				Payload:     json.RawMessage(`{"entry":"corrupt"}`),
			})
		},
		corruptRegister: func(t *testing.T, store harness.Storage, sessionID string) {
			m := store.(*Memory)
			m.mu.Lock()
			defer m.mu.Unlock()
			key := confSessionKey(sessionID)
			m.state.registers[key] = harness.Register{Key: key, Revision: 1, Payload: json.RawMessage(`{"session":`)}
		},
		orphanEntry: func(t *testing.T, store harness.Storage, sessionID string) {
			m := store.(*Memory)
			m.mu.Lock()
			defer m.mu.Unlock()
			m.state.entries[sessionID] = []harness.Entry{{
				SessionID:   sessionID,
				ID:          "orphan-entry",
				Sequence:    1,
				Kind:        harness.EntryInput,
				CommittedAt: fixtureTime,
				Payload:     json.RawMessage(`{"entry":"orphan"}`),
			}}
		},
		orphanOperationRegister: func(t *testing.T, store harness.Storage, sessionID string) {
			m := store.(*Memory)
			m.mu.Lock()
			defer m.mu.Unlock()
			key := confOpKey(sessionID, "orphan-op")
			m.state.registers[key] = harness.Register{Key: key, Revision: 1, Payload: json.RawMessage(`{"operation":"orphan-op"}`)}
		},
		malformedSessionRegister: func(t *testing.T, store harness.Storage, sessionID string) {
			m := store.(*Memory)
			m.mu.Lock()
			defer m.mu.Unlock()
			// A session-register envelope carrying an operation identity is
			// not a valid parent for any dependent of the session.
			key := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession, OperationID: "forged"}
			m.state.registers[key] = harness.Register{Key: key, Revision: 1, Payload: json.RawMessage(`{"session":"forged"}`)}
		},
		exhaustSequence: func(t *testing.T, store harness.Storage, sessionID string) {
			m := store.(*Memory)
			m.mu.Lock()
			defer m.mu.Unlock()
			m.state.entries[sessionID] = append(m.state.entries[sessionID], harness.Entry{
				SessionID:   sessionID,
				ID:          "max-sequence-entry",
				Sequence:    math.MaxInt64,
				Kind:        harness.EntryInput,
				CommittedAt: fixtureTime,
				Payload:     json.RawMessage(`{"entry":"max"}`),
			})
		},
		exhaustRevision: func(t *testing.T, store harness.Storage, sessionID string) {
			m := store.(*Memory)
			m.mu.Lock()
			defer m.mu.Unlock()
			key := confSessionKey(sessionID)
			register := m.state.registers[key]
			register.Revision = math.MaxInt64
			m.state.registers[key] = register
		},
	}
}
