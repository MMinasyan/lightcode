// Package storage implements the public harness durable-storage contract. The
// in-memory store is the test implementation behind the shared storage
// conformance suite; it is not a production backend.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/harness"
)

// NewMemory returns the test-only in-memory implementation of the harness
// storage contract. Committed state lives in process memory and every
// transaction clones the complete store; nothing survives the process.
func NewMemory() *Memory {
	return &Memory{state: newMemoryState()}
}

// Memory keeps every committed envelope in maps guarded by one RWMutex. A
// transaction takes the writer lock, executes the callback against a clone of
// the complete state, and replaces the committed pointer only after a
// successful callback; discarding the clone is the rollback. The O(n) clone is
// deliberate test-only simplicity, not a concurrency design for Runtime.
type Memory struct {
	mu    sync.RWMutex
	state *memoryState
}

var _ harness.Storage = (*Memory)(nil)

func (m *Memory) ReadEntries(ctx context.Context, sessionID string, after int64) ([]harness.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.state.readEntries(sessionID, after)
}

func (m *Memory) ReadRegister(ctx context.Context, key harness.RegisterKey) (harness.Register, error) {
	if err := ctx.Err(); err != nil {
		return harness.Register{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return harness.Register{}, err
	}
	return m.state.readRegister(key)
}

func (m *Memory) ReadRegisters(ctx context.Context, sessionID string) ([]harness.Register, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.state.readRegisters(sessionID)
}

func (m *Memory) ListSessionIDs(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.state.listSessionIDs(), nil
}

// Transact is the only mutation path. The callback runs under the writer lock
// against a private clone of committed state; the clone replaces committed
// state only when the callback returns nil without a latched mutation failure
// or pre-commit cancellation. Every other outcome — callback error, panic,
// cancellation — discards the clone, which is the rollback, and the panic then
// propagates.
func (m *Memory) Transact(ctx context.Context, fn func(harness.Transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	txn := &memoryTxn{state: m.state.clone()}
	defer func() { txn.closed = true }()
	if err := fn(txn); err != nil {
		return err
	}
	if txn.latched != nil {
		return txn.latched
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.state = txn.state
	return nil
}

// memoryState is one committed or staged snapshot of every envelope. The same
// read and mutation rules serve committed reads and transaction execution.
type memoryState struct {
	// entries maps session identity to that session's entries in ascending
	// sequence order.
	entries   map[string][]harness.Entry
	registers map[harness.RegisterKey]harness.Register
}

func newMemoryState() *memoryState {
	return &memoryState{
		entries:   make(map[string][]harness.Entry),
		registers: make(map[harness.RegisterKey]harness.Register),
	}
}

func (s *memoryState) clone() *memoryState {
	clone := &memoryState{
		entries:   make(map[string][]harness.Entry, len(s.entries)),
		registers: make(map[harness.RegisterKey]harness.Register, len(s.registers)),
	}
	for sessionID, list := range s.entries {
		clones := make([]harness.Entry, len(list))
		for i, entry := range list {
			clones[i] = cloneEntry(entry)
		}
		clone.entries[sessionID] = clones
	}
	for key, register := range s.registers {
		clone.registers[key] = cloneRegister(register)
	}
	return clone
}

func (s *memoryState) readEntries(sessionID string, after int64) ([]harness.Entry, error) {
	if sessionID == "" {
		return nil, invalidf("session identity is empty")
	}
	if after < 0 {
		return nil, invalidf("after %d is negative", after)
	}
	_, hasRegister := s.registers[harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}]
	list := s.entries[sessionID]
	if !hasRegister {
		if s.sessionOrphaned(sessionID) {
			return nil, orphanCorruption(sessionID)
		}
		return nil, notfoundf("session %q has no envelopes", sessionID)
	}
	out := make([]harness.Entry, 0, len(list))
	for _, entry := range list {
		if entry.Sequence <= after {
			continue
		}
		if err := validateStoredEntry(entry); err != nil {
			return nil, err
		}
		out = append(out, cloneEntry(entry))
	}
	return out, nil
}

func (s *memoryState) readRegister(key harness.RegisterKey) (harness.Register, error) {
	if err := validateRegisterKey(key); err != nil {
		return harness.Register{}, err
	}
	if s.sessionOrphaned(key.SessionID) {
		return harness.Register{}, orphanCorruption(key.SessionID)
	}
	register, ok := s.registers[key]
	if !ok {
		return harness.Register{}, notfoundf("register %v does not exist", key)
	}
	if err := validateStoredRegister(register); err != nil {
		return harness.Register{}, err
	}
	return cloneRegister(register), nil
}

func (s *memoryState) readRegisters(sessionID string) ([]harness.Register, error) {
	if sessionID == "" {
		return nil, invalidf("session identity is empty")
	}
	if s.sessionOrphaned(sessionID) {
		return nil, orphanCorruption(sessionID)
	}
	sessionRegister, ok := s.registers[harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}]
	if !ok {
		return nil, notfoundf("session %q has no envelopes", sessionID)
	}
	if err := validateStoredRegister(sessionRegister); err != nil {
		return nil, err
	}
	var operations []harness.Register
	for key, register := range s.registers {
		if key.SessionID == sessionID && key.Kind == harness.RegisterOperation {
			operations = append(operations, register)
		}
	}
	sort.Slice(operations, func(i, j int) bool {
		return operations[i].Key.OperationID < operations[j].Key.OperationID
	})
	out := make([]harness.Register, 0, 1+len(operations))
	out = append(out, cloneRegister(sessionRegister))
	for _, register := range operations {
		if err := validateStoredRegister(register); err != nil {
			return nil, err
		}
		out = append(out, cloneRegister(register))
	}
	return out, nil
}

func (s *memoryState) listSessionIDs() []string {
	seen := make(map[string]bool)
	for sessionID := range s.entries {
		seen[sessionID] = true
	}
	for key := range s.registers {
		seen[key.SessionID] = true
	}
	ids := make([]string, 0, len(seen))
	for sessionID := range seen {
		ids = append(ids, sessionID)
	}
	sort.Strings(ids)
	return ids
}

func (s *memoryState) deleteSession(sessionID string) error {
	if sessionID == "" {
		return invalidf("session identity is empty")
	}
	sessionKey := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}
	if _, ok := s.registers[sessionKey]; ok {
		delete(s.registers, sessionKey)
		for key := range s.registers {
			if key.SessionID == sessionID {
				delete(s.registers, key)
			}
		}
		delete(s.entries, sessionID)
		return nil
	}
	if s.sessionOrphaned(sessionID) {
		return orphanCorruption(sessionID)
	}
	return notfoundf("session %q does not exist", sessionID)
}

func (s *memoryState) insertEntry(draft harness.EntryDraft) (harness.Entry, error) {
	if err := validateEntryDraft(draft); err != nil {
		return harness.Entry{}, err
	}
	if _, ok := s.registers[harness.RegisterKey{SessionID: draft.SessionID, Kind: harness.RegisterSession}]; !ok {
		if s.sessionOrphaned(draft.SessionID) {
			return harness.Entry{}, orphanCorruption(draft.SessionID)
		}
		return harness.Entry{}, notfoundf("session %q does not exist", draft.SessionID)
	}
	if s.hasEntryID(draft.ID) {
		return harness.Entry{}, conflictf("entry id %q already exists", draft.ID)
	}
	list := s.entries[draft.SessionID]
	highest := maxSequence(list)
	if highest == math.MaxInt64 {
		return harness.Entry{}, storagef("session %q entry sequence would overflow", draft.SessionID)
	}
	entry := harness.Entry{
		SessionID:   draft.SessionID,
		ID:          draft.ID,
		Sequence:    highest + 1,
		OperationID: draft.OperationID,
		Kind:        draft.Kind,
		CommittedAt: time.Now().UTC(),
		Payload:     clonePayload(draft.Payload),
	}
	s.entries[draft.SessionID] = append(list, entry)
	return cloneEntry(entry), nil
}

func (s *memoryState) insertRegister(draft harness.RegisterDraft) (harness.Register, error) {
	if err := validateRegisterKey(draft.Key); err != nil {
		return harness.Register{}, err
	}
	if !json.Valid(draft.Payload) {
		return harness.Register{}, invalidf("register payload is not valid JSON")
	}
	if draft.Key.Kind == harness.RegisterOperation {
		if _, ok := s.registers[harness.RegisterKey{SessionID: draft.Key.SessionID, Kind: harness.RegisterSession}]; !ok {
			if s.sessionOrphaned(draft.Key.SessionID) {
				return harness.Register{}, orphanCorruption(draft.Key.SessionID)
			}
			return harness.Register{}, notfoundf("session %q does not exist", draft.Key.SessionID)
		}
		if s.hasOperationID(draft.Key.OperationID) {
			return harness.Register{}, conflictf("operation %q already exists", draft.Key.OperationID)
		}
	} else if s.sessionOrphaned(draft.Key.SessionID) {
		// Inserting the missing session register must not repair or hide
		// persisted dependents that outlived their register.
		return harness.Register{}, orphanCorruption(draft.Key.SessionID)
	}
	if _, exists := s.registers[draft.Key]; exists {
		return harness.Register{}, conflictf("register %v already exists", draft.Key)
	}
	register := harness.Register{Key: draft.Key, Revision: 1, Payload: clonePayload(draft.Payload)}
	s.registers[draft.Key] = register
	return cloneRegister(register), nil
}

func (s *memoryState) replaceRegister(key harness.RegisterKey, expectedRevision int64, payload json.RawMessage) (harness.Register, error) {
	if err := validateRegisterKey(key); err != nil {
		return harness.Register{}, err
	}
	if expectedRevision <= 0 {
		return harness.Register{}, invalidf("expected revision %d is not positive", expectedRevision)
	}
	if !json.Valid(payload) {
		return harness.Register{}, invalidf("register payload is not valid JSON")
	}
	current, ok := s.registers[key]
	if !ok {
		if s.sessionOrphaned(key.SessionID) {
			return harness.Register{}, orphanCorruption(key.SessionID)
		}
		return harness.Register{}, notfoundf("register %v does not exist", key)
	}
	if current.Revision != expectedRevision {
		return harness.Register{}, conflictf("register %v revision %d does not match expected %d", key, current.Revision, expectedRevision)
	}
	if current.Revision == math.MaxInt64 {
		return harness.Register{}, storagef("register %v revision would overflow", key)
	}
	current.Revision++
	current.Payload = clonePayload(payload)
	s.registers[key] = current
	return cloneRegister(current), nil
}

// sessionOrphaned reports whether dependent records exist for one session
// without its session register. Such state is corruption, never an empty or
// absent session.
func (s *memoryState) sessionOrphaned(sessionID string) bool {
	if _, ok := s.registers[harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}]; ok {
		return false
	}
	if len(s.entries[sessionID]) > 0 {
		return true
	}
	for key := range s.registers {
		if key.SessionID == sessionID {
			return true
		}
	}
	return false
}

func (s *memoryState) hasEntryID(id string) bool {
	for _, list := range s.entries {
		for _, entry := range list {
			if entry.ID == id {
				return true
			}
		}
	}
	return false
}

func (s *memoryState) hasOperationID(operationID string) bool {
	for key := range s.registers {
		if key.Kind == harness.RegisterOperation && key.OperationID == operationID {
			return true
		}
	}
	return false
}

// maxSequence returns the highest committed sequence of one session's entries,
// zero when the session has none.
func maxSequence(list []harness.Entry) int64 {
	var highest int64
	for _, entry := range list {
		if entry.Sequence > highest {
			highest = entry.Sequence
		}
	}
	return highest
}

// memoryTxn executes against a private clone of committed state. A mutation
// failure of conflict or storage class latches the first such error so an
// ignored conflict or overflow still fails the transaction; every other error
// is the callback's to handle.
type memoryTxn struct {
	state   *memoryState
	latched error
	closed  bool
}

var _ harness.Transaction = (*memoryTxn)(nil)

func (t *memoryTxn) guard() error {
	if t.closed {
		return invalidf("transaction has expired")
	}
	return nil
}

func (t *memoryTxn) latch(err error) {
	if t.latched == nil && (errors.Is(err, harness.ErrConflict) || errors.Is(err, harness.ErrStorage)) {
		t.latched = err
	}
}

func (t *memoryTxn) ReadEntries(sessionID string, after int64) ([]harness.Entry, error) {
	if err := t.guard(); err != nil {
		return nil, err
	}
	return t.state.readEntries(sessionID, after)
}

func (t *memoryTxn) ReadRegister(key harness.RegisterKey) (harness.Register, error) {
	if err := t.guard(); err != nil {
		return harness.Register{}, err
	}
	return t.state.readRegister(key)
}

func (t *memoryTxn) InsertEntry(draft harness.EntryDraft) (harness.Entry, error) {
	if err := t.guard(); err != nil {
		return harness.Entry{}, err
	}
	entry, err := t.state.insertEntry(draft)
	if err != nil {
		t.latch(err)
		return harness.Entry{}, err
	}
	return entry, nil
}

func (t *memoryTxn) InsertRegister(draft harness.RegisterDraft) (harness.Register, error) {
	if err := t.guard(); err != nil {
		return harness.Register{}, err
	}
	register, err := t.state.insertRegister(draft)
	if err != nil {
		t.latch(err)
		return harness.Register{}, err
	}
	return register, nil
}

func (t *memoryTxn) ReplaceRegister(key harness.RegisterKey, expectedRevision int64, payload json.RawMessage) (harness.Register, error) {
	if err := t.guard(); err != nil {
		return harness.Register{}, err
	}
	register, err := t.state.replaceRegister(key, expectedRevision, payload)
	if err != nil {
		t.latch(err)
		return harness.Register{}, err
	}
	return register, nil
}

func (t *memoryTxn) DeleteSession(sessionID string) error {
	if err := t.guard(); err != nil {
		return err
	}
	return t.state.deleteSession(sessionID)
}

func validateEntryDraft(draft harness.EntryDraft) error {
	if draft.SessionID == "" {
		return invalidf("entry draft session identity is empty")
	}
	if draft.ID == "" {
		return invalidf("entry draft identity is empty")
	}
	if !validEntryKind(draft.Kind) {
		return invalidf("entry kind %q is not one of the closed kinds", draft.Kind)
	}
	if !json.Valid(draft.Payload) {
		return invalidf("entry payload is not valid JSON")
	}
	return nil
}

func validateRegisterKey(key harness.RegisterKey) error {
	if key.SessionID == "" {
		return invalidf("register key session identity is empty")
	}
	switch key.Kind {
	case harness.RegisterSession:
		if key.OperationID != "" {
			return invalidf("session register key must not carry an operation identity")
		}
	case harness.RegisterOperation:
		if key.OperationID == "" {
			return invalidf("operation register key requires an operation identity")
		}
	default:
		return invalidf("register kind %q is not one of the closed kinds", key.Kind)
	}
	return nil
}

func validateStoredEntry(entry harness.Entry) error {
	if entry.SessionID == "" {
		return &harness.CorruptionError{Detail: "stored entry has an empty session identity"}
	}
	if entry.ID == "" {
		return &harness.CorruptionError{SessionID: entry.SessionID, Detail: "stored entry has an empty identity"}
	}
	if !validEntryKind(entry.Kind) {
		return &harness.CorruptionError{SessionID: entry.SessionID, Detail: fmt.Sprintf("stored entry %q kind %q is not one of the closed kinds", entry.ID, entry.Kind)}
	}
	if entry.Sequence <= 0 {
		return &harness.CorruptionError{SessionID: entry.SessionID, Detail: fmt.Sprintf("stored entry %q sequence %d is not positive", entry.ID, entry.Sequence)}
	}
	if entry.CommittedAt.IsZero() {
		return &harness.CorruptionError{SessionID: entry.SessionID, Detail: fmt.Sprintf("stored entry %q has a zero commit time", entry.ID)}
	}
	if !json.Valid(entry.Payload) {
		return &harness.CorruptionError{SessionID: entry.SessionID, Detail: fmt.Sprintf("stored entry %q payload is not valid JSON", entry.ID)}
	}
	return nil
}

func validateStoredRegister(register harness.Register) error {
	if register.Key.SessionID == "" {
		return &harness.CorruptionError{Detail: "stored register has an empty session identity"}
	}
	if !validRegisterKind(register.Key.Kind) {
		return &harness.CorruptionError{SessionID: register.Key.SessionID, Detail: fmt.Sprintf("stored register kind %q is not one of the closed kinds", register.Key.Kind)}
	}
	if register.Key.Kind == harness.RegisterSession && register.Key.OperationID != "" {
		return &harness.CorruptionError{SessionID: register.Key.SessionID, Detail: "stored session register carries an operation identity"}
	}
	if register.Key.Kind == harness.RegisterOperation && register.Key.OperationID == "" {
		return &harness.CorruptionError{SessionID: register.Key.SessionID, Detail: "stored operation register has an empty operation identity"}
	}
	if register.Revision <= 0 {
		return &harness.CorruptionError{SessionID: register.Key.SessionID, Detail: fmt.Sprintf("stored register revision %d is not positive", register.Revision)}
	}
	if !json.Valid(register.Payload) {
		return &harness.CorruptionError{SessionID: register.Key.SessionID, Detail: "stored register payload is not valid JSON"}
	}
	return nil
}

func validEntryKind(kind harness.EntryKind) bool {
	switch kind {
	case harness.EntryInput,
		harness.EntryAssistant,
		harness.EntryToolResult,
		harness.EntrySignal,
		harness.EntryHookResult,
		harness.EntryCompaction,
		harness.EntryOperationSettlement:
		return true
	}
	return false
}

func validRegisterKind(kind harness.RegisterKind) bool {
	switch kind {
	case harness.RegisterSession, harness.RegisterOperation:
		return true
	}
	return false
}

func orphanCorruption(sessionID string) error {
	return &harness.CorruptionError{SessionID: sessionID, Detail: "dependent records exist without a session register"}
}

func clonePayload(payload json.RawMessage) json.RawMessage {
	clone := make(json.RawMessage, len(payload))
	copy(clone, payload)
	return clone
}

func cloneEntry(entry harness.Entry) harness.Entry {
	entry.Payload = clonePayload(entry.Payload)
	return entry
}

func cloneRegister(register harness.Register) harness.Register {
	register.Payload = clonePayload(register.Payload)
	return register
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", harness.ErrInvalid, fmt.Sprintf(format, args...))
}

func notfoundf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", harness.ErrNotFound, fmt.Sprintf(format, args...))
}

func conflictf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", harness.ErrConflict, fmt.Sprintf(format, args...))
}

func storagef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", harness.ErrStorage, fmt.Sprintf(format, args...))
}
