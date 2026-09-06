package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// graphStorage is the private Storage substitute for the validator and
// admission fixtures: an in-package fake implementing harness.Storage with
// transactional semantics, failure injection, and no dependency on any
// concrete storage backend. Every transaction applies its mutations to the
// shared state and rolls them back when the callback fails.
type graphStorage struct {
	mu        sync.Mutex
	registers map[string][]Register
	entries   map[string][]Entry

	registersErr error
	entriesErr   error

	// txHook, when set, runs before each transactional mutation step and
	// aborts the transaction with its error when non-nil.
	txHook func(step string) error

	// entryHook, when set, runs before each entry insertion with the draft
	// and aborts the transaction with its error when non-nil. It gives
	// fixtures a deterministic barrier on one committed entry kind.
	entryHook func(draft EntryDraft) error
}

func (s *graphStorage) ReadEntries(_ context.Context, sessionID string, after int64) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readEntriesLocked(sessionID, after)
}

func (s *graphStorage) readEntriesLocked(sessionID string, after int64) ([]Entry, error) {
	if s.entriesErr != nil {
		return nil, s.entriesErr
	}
	var out []Entry
	for _, entry := range s.entries[sessionID] {
		if entry.Sequence > after {
			out = append(out, entry)
		}
	}
	return out, nil
}

func (s *graphStorage) ReadRegister(_ context.Context, key RegisterKey) (Register, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readRegisterLocked(key)
}

func (s *graphStorage) readRegisterLocked(key RegisterKey) (Register, error) {
	for _, reg := range s.registers[key.SessionID] {
		if reg.Key == key {
			return reg, nil
		}
	}
	return Register{}, fmt.Errorf("register %q: %w", key.SessionID, ErrNotFound)
}

func (s *graphStorage) ReadRegisters(_ context.Context, sessionID string) ([]Register, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registersErr != nil {
		return nil, s.registersErr
	}
	regs := s.registers[sessionID]
	out := make([]Register, len(regs)) // owned copy: register values mutate in place under the store lock
	copy(out, regs)
	return out, nil
}

func (s *graphStorage) ListSessionIDs(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for id := range s.registers {
		seen[id] = true
	}
	for id := range s.entries {
		seen[id] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func (s *graphStorage) Transact(ctx context.Context, fn func(Transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &graphTx{store: s}
	if err := fn(tx); err != nil {
		tx.rollback()
		return err
	}
	return nil
}

func (s *graphStorage) sessionRegisterLocked(sessionID string) (Register, bool) {
	for _, reg := range s.registers[sessionID] {
		if reg.Key.Kind == RegisterSession {
			return reg, true
		}
	}
	return Register{}, false
}

func (s *graphStorage) hook(step string) error {
	if s.txHook == nil {
		return nil
	}
	return s.txHook(step)
}

// graphTx is one transaction lifetime over the shared fake state. Mutations
// are journaled as undo closures; Transact replays them in reverse when the
// callback fails.
type graphTx struct {
	store *graphStorage
	undo  []func()
}

func (t *graphTx) rollback() {
	for i := len(t.undo) - 1; i >= 0; i-- {
		t.undo[i]()
	}
}

func (t *graphTx) ReadEntries(sessionID string, after int64) ([]Entry, error) {
	return t.store.readEntriesLocked(sessionID, after)
}

func (t *graphTx) ReadRegister(key RegisterKey) (Register, error) {
	return t.store.readRegisterLocked(key)
}

func (t *graphTx) InsertEntry(draft EntryDraft) (Entry, error) {
	if err := t.store.hook("insert_entry"); err != nil {
		return Entry{}, err
	}
	if t.store.entryHook != nil {
		if err := t.store.entryHook(draft); err != nil {
			return Entry{}, err
		}
	}
	if draft.SessionID == "" || draft.ID == "" || !json.Valid(draft.Payload) {
		return Entry{}, invalidInput("invalid entry draft")
	}
	if _, ok := t.store.sessionRegisterLocked(draft.SessionID); !ok {
		return Entry{}, fmt.Errorf("%w: session %q does not exist", ErrNotFound, draft.SessionID)
	}
	for _, list := range t.store.entries {
		for _, entry := range list {
			if entry.ID == draft.ID {
				return Entry{}, fmt.Errorf("%w: entry id %q already exists", ErrConflict, draft.ID)
			}
		}
	}
	list := t.store.entries[draft.SessionID]
	var highest int64
	for _, entry := range list {
		if entry.Sequence > highest {
			highest = entry.Sequence
		}
	}
	entry := Entry{
		SessionID:   draft.SessionID,
		ID:          draft.ID,
		Sequence:    highest + 1,
		OperationID: draft.OperationID,
		Kind:        draft.Kind,
		CommittedAt: time.Now().UTC(),
		Payload:     draft.Payload,
	}
	t.store.entries[draft.SessionID] = append(list, entry)
	index := len(t.store.entries[draft.SessionID]) - 1
	t.undo = append(t.undo, func() {
		entries := t.store.entries[draft.SessionID]
		t.store.entries[draft.SessionID] = entries[:index]
	})
	return entry, nil
}

func (t *graphTx) InsertRegister(draft RegisterDraft) (Register, error) {
	if err := t.store.hook("insert_register"); err != nil {
		return Register{}, err
	}
	if !json.Valid(draft.Payload) {
		return Register{}, invalidInput("register payload is not valid JSON")
	}
	if draft.Key.Kind == RegisterOperation {
		if _, ok := t.store.sessionRegisterLocked(draft.Key.SessionID); !ok {
			return Register{}, fmt.Errorf("%w: session %q does not exist", ErrNotFound, draft.Key.SessionID)
		}
		for sessionID, regs := range t.store.registers {
			for _, reg := range regs {
				if reg.Key.Kind == RegisterOperation && reg.Key.OperationID == draft.Key.OperationID {
					return Register{}, fmt.Errorf("%w: operation %q already exists in session %q", ErrConflict, draft.Key.OperationID, sessionID)
				}
			}
		}
	}
	for _, reg := range t.store.registers[draft.Key.SessionID] {
		if reg.Key == draft.Key {
			return Register{}, fmt.Errorf("%w: register %v already exists", ErrConflict, draft.Key)
		}
	}
	register := Register{Key: draft.Key, Revision: 1, Payload: draft.Payload}
	t.store.registers[draft.Key.SessionID] = append(t.store.registers[draft.Key.SessionID], register)
	index := len(t.store.registers[draft.Key.SessionID]) - 1
	t.undo = append(t.undo, func() {
		regs := t.store.registers[draft.Key.SessionID]
		t.store.registers[draft.Key.SessionID] = regs[:index]
	})
	return register, nil
}

func (t *graphTx) ReplaceRegister(key RegisterKey, expectedRevision int64, payload json.RawMessage) (Register, error) {
	if err := t.store.hook("replace_register"); err != nil {
		return Register{}, err
	}
	if !json.Valid(payload) {
		return Register{}, invalidInput("register payload is not valid JSON")
	}
	regs := t.store.registers[key.SessionID]
	for i, reg := range regs {
		if reg.Key != key {
			continue
		}
		if reg.Revision != expectedRevision {
			return Register{}, fmt.Errorf("%w: register %v revision %d does not match expected %d", ErrConflict, key, reg.Revision, expectedRevision)
		}
		updated := Register{Key: key, Revision: reg.Revision + 1, Payload: payload}
		t.undo = append(t.undo, func() {
			t.store.registers[key.SessionID][i] = reg
		})
		t.store.registers[key.SessionID][i] = updated
		return updated, nil
	}
	return Register{}, fmt.Errorf("%w: register %v does not exist", ErrNotFound, key)
}

func (t *graphTx) DeleteSession(sessionID string) error {
	if err := t.store.hook("delete_session"); err != nil {
		return err
	}
	regs, ok := t.store.registers[sessionID]
	if !ok {
		return fmt.Errorf("%w: session %q does not exist", ErrNotFound, sessionID)
	}
	entries := t.store.entries[sessionID]
	t.undo = append(t.undo, func() {
		t.store.registers[sessionID] = regs
		t.store.entries[sessionID] = entries
	})
	delete(t.store.registers, sessionID)
	delete(t.store.entries, sessionID)
	return nil
}

// testEntry is one fixture entry: the envelope identity plus exactly one
// typed payload to encode, or a raw payload override for corruption.
type testEntry struct {
	env         Entry
	input       *inputEntry
	assistant   *assistantEntry
	toolResult  *toolResultEntry
	signal      *signalEntry
	settlement  *operationSettlementEntry
	rawOverride json.RawMessage
}

// testGraph assembles one Session fixture and encodes it into graphStorage
// through this commit's landed codecs.
type testGraph struct {
	session            SessionRecord
	sessionRawOverride json.RawMessage
	ops                []OperationRecord
	entries            []testEntry
}

func mustEncode(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("fixture encode: %v", err)
	}
}

// storage encodes every fixture record into a fresh read-only substitute.
func (g *testGraph) storage(t *testing.T) *graphStorage {
	t.Helper()
	out := &graphStorage{registers: map[string][]Register{}, entries: map[string][]Entry{}}
	sessionRaw, err := encodeSessionRegister(g.session)
	mustEncode(t, err)
	if g.sessionRawOverride != nil {
		sessionRaw = g.sessionRawOverride
	}
	out.registers[g.session.Identity.SessionID] = append(out.registers[g.session.Identity.SessionID],
		Register{Key: RegisterKey{SessionID: g.session.Identity.SessionID, Kind: RegisterSession}, Revision: g.session.Revision, Payload: sessionRaw})
	for i := range g.ops {
		op := &g.ops[i]
		opRaw, err := encodeOperationRegister(*op)
		mustEncode(t, err)
		out.registers[g.session.Identity.SessionID] = append(out.registers[g.session.Identity.SessionID],
			Register{Key: RegisterKey{SessionID: g.session.Identity.SessionID, Kind: RegisterOperation, OperationID: op.Admission.OperationID}, Revision: op.Revision, Payload: opRaw})
	}
	for _, entry := range g.entries {
		env := entry.env
		switch {
		case entry.rawOverride != nil:
			env.Payload = entry.rawOverride
		case entry.input != nil:
			raw, err := encodeInputEntry(*entry.input)
			mustEncode(t, err)
			env.Payload = raw
		case entry.assistant != nil:
			raw, err := encodeAssistantEntry(*entry.assistant)
			mustEncode(t, err)
			env.Payload = raw
		case entry.toolResult != nil:
			raw, err := encodeToolResultEntry(*entry.toolResult)
			mustEncode(t, err)
			env.Payload = raw
		case entry.signal != nil:
			raw, err := encodeSignalEntry(*entry.signal)
			mustEncode(t, err)
			env.Payload = raw
		case entry.settlement != nil:
			raw, err := encodeOperationSettlementEntry(*entry.settlement)
			mustEncode(t, err)
			env.Payload = raw
		default:
			t.Fatalf("fixture entry %s has no payload", env.ID)
		}
		out.entries[g.session.Identity.SessionID] = append(out.entries[g.session.Identity.SessionID], env)
	}
	return out
}

// validTestGraph returns a coherent open Session with one running Operation:
// the admitted input entry, a completed assistant entry, and no outstanding
// effects.
func validTestGraph() *testGraph {
	session := validSessionRecord()
	session.State.CurrentOperationID = testOpID
	op := validOperationRecord()
	input := validInputEntry(testOpID)
	input.EntryID = hexID(1)
	op.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(1)}
	assistant := validAssistantEntry(testOpID)
	assistant.EntryID = hexID(2)
	return &testGraph{
		session: session,
		ops:     []OperationRecord{op},
		entries: []testEntry{
			{env: Entry{SessionID: testSessionID, ID: hexID(1), OperationID: testOpID, Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &input},
			{env: Entry{SessionID: testSessionID, ID: hexID(2), OperationID: testOpID, Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &assistant},
		},
	}
}

// validateFixture runs the graph validator over one fixture Session.
func validateFixture(t *testing.T, store *graphStorage, sessionID string) (*sessionGraph, error) {
	t.Helper()
	return validateSessionGraph(context.Background(), store, sessionID)
}

// mustState extracts the state member raw bytes of an encoded session
// register payload.
func mustState(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	obj := wireObject(t, raw)
	state, ok := obj["state"]
	if !ok {
		t.Fatalf("session payload carries no state member")
	}
	return state
}

func wantCorruption(t *testing.T, err error) *CorruptionError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected corruption, got a valid graph")
	}
	var corrupt *CorruptionError
	if !errors.As(err, &corrupt) {
		t.Fatalf("error %v is not a CorruptionError", err)
	}
	if corrupt.SessionID != testSessionID {
		t.Fatalf("corruption names session %q, want %q", corrupt.SessionID, testSessionID)
	}
	return corrupt
}

// TestGraphValidatorAcceptsCoherentSessions proves the validator accepts the
// coherent durable shapes and returns the owned decoded view: the Session
// record, Operation records in read order, and committed-order typed entries.
func TestGraphValidatorAcceptsCoherentSessions(t *testing.T) {
	t.Run("running session", func(t *testing.T) {
		fixture := validTestGraph()
		view, err := validateFixture(t, fixture.storage(t), testSessionID)
		if err != nil {
			t.Fatalf("valid graph rejected: %v", err)
		}
		if view.Session.Revision != validSessionRecord().Revision ||
			view.Session.Identity.SessionID != testSessionID ||
			view.Session.State.CurrentOperationID != testOpID {
			t.Fatalf("view session = %+v", view.Session)
		}
		if len(view.Operations) != 1 || view.Operations[0].Admission.OperationID != testOpID {
			t.Fatalf("view operations = %+v", view.Operations)
		}
		if _, ok := view.Operation(testOpID); !ok {
			t.Fatalf("Operation lookup by identity failed")
		}
		if len(view.Entries) != 2 || view.Entries[0].Input == nil || view.Entries[1].Assistant == nil {
			t.Fatalf("view entries = %+v", view.Entries)
		}
		if view.Entries[0].Envelope.ID != hexID(1) || view.Entries[1].Envelope.ID != hexID(2) {
			t.Fatalf("view entries are not in committed order: %+v", view.Entries)
		}
	})

	t.Run("running model effect", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.ops[0].State.ActiveEffect = &ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(3)}
		view, err := validateFixture(t, fixture.storage(t), testSessionID)
		if err != nil {
			t.Fatalf("valid model-effect graph rejected: %v", err)
		}
		if view.Operations[0].State.ActiveEffect == nil || view.Operations[0].State.ActiveEffect.Kind != EffectModel {
			t.Fatalf("typed view lost the active model effect")
		}
	})

	t.Run("pending tool call", func(t *testing.T) {
		fixture := validTestGraph()
		assistant := validAssistantEntry(testOpID)
		assistant.EntryID = hexID(2)
		call := validToolCallRecord()
		call.ResultEntryID = hexID(3)
		assistant.ToolCalls = []toolCallRecord{call}
		fixture.entries[1].assistant = &assistant
		op := &fixture.ops[0]
		op.State.PendingToolCalls = []PendingToolCall{{
			AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
			CallID:         "call-1",
			ResultEntryID:  hexID(3),
		}}
		view, err := validateFixture(t, fixture.storage(t), testSessionID)
		if err != nil {
			t.Fatalf("valid pending shape rejected: %v", err)
		}
		if len(view.Entries[1].Assistant.ToolCalls) != 1 {
			t.Fatalf("typed assistant payload lost its call: %+v", view.Entries[1])
		}
	})

	t.Run("resolved tool result", func(t *testing.T) {
		fixture := validTestGraph()
		assistant := validAssistantEntry(testOpID)
		assistant.EntryID = hexID(2)
		call := validToolCallRecord()
		call.ResultEntryID = hexID(3)
		assistant.ToolCalls = []toolCallRecord{call}
		fixture.entries[1].assistant = &assistant
		result := validToolResultEntry(testOpID)
		result.EntryID = hexID(3)
		result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
		fixture.entries = append(fixture.entries, testEntry{
			env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 3, CommittedAt: testTime},
			toolResult: &result,
		})
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("valid resolved shape rejected: %v", err)
		}
	})

	t.Run("terminal session with no-output settlement usage", func(t *testing.T) {
		fixture := validTestGraph()
		session := &fixture.session
		session.State.CurrentOperationID = ""
		op := &fixture.ops[0]
		stamped := testTime
		op.State.Status = OperationSuccess
		op.State.SettledAt = &stamped
		op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
		ref := testModelRef()
		settlement := operationSettlementEntry{
			SessionID:   testSessionID,
			EntryID:     hexID(3),
			OperationID: testOpID,
			Status:      OperationSuccess,
			Model:       &ref,
			Usage:       testUsage(5),
		}
		fixture.entries = append(fixture.entries, testEntry{
			env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime},
			settlement: &settlement,
		})
		totals := UsageTotals{ByModel: []ModelUsage{{Model: ref, Usage: *testUsage(5)}}}
		op.State.Usage = totals
		session.State.Usage = totals
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("valid terminal shape rejected: %v", err)
		}
	})

	t.Run("fork prefix copies", func(t *testing.T) {
		fixture := &testGraph{session: validSessionRecord()}
		fixture.session.Identity.SourceSessionID = otherSession()
		fixture.session.Identity.SourceBoundaryEntryID = otherEntry()
		copiedInput := validInputEntry("")
		copiedInput.EntryID = hexID(1)
		copiedAssistant := validAssistantEntry("")
		copiedAssistant.EntryID = hexID(2)
		copiedCall := validToolCallRecord()
		copiedCall.ResultEntryID = hexID(3)
		copiedAssistant.ToolCalls = []toolCallRecord{copiedCall}
		copiedResult := validToolResultEntry("")
		copiedResult.EntryID = hexID(3)
		copiedResult.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
		copiedSignal := validSignalEntry("")
		copiedSignal.EntryID = hexID(4)
		copiedSignal.RelatedOperation = operationRef{SessionID: otherSession(), OperationID: "source-op"}
		fixture.entries = []testEntry{
			{env: Entry{SessionID: testSessionID, ID: hexID(1), Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &copiedInput},
			{env: Entry{SessionID: testSessionID, ID: hexID(2), Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &copiedAssistant},
			{env: Entry{SessionID: testSessionID, ID: hexID(3), Kind: EntryToolResult, Sequence: 3, CommittedAt: testTime}, toolResult: &copiedResult},
			{env: Entry{SessionID: testSessionID, ID: hexID(4), Kind: EntrySignal, Sequence: 4, CommittedAt: testTime}, signal: &copiedSignal},
		}
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("valid fork prefix rejected: %v", err)
		}
	})
}

// TestGraphValidatorSurfacesCorruption proves persisted semantic violations
// surface as CorruptionError for the owning Session while a valid sibling
// stays usable in isolation.
func TestGraphValidatorSurfacesCorruption(t *testing.T) {
	// The valid sibling Session used by every corruption row below.
	siblingID := otherSession()
	sibling := validTestGraph()
	sibling.session.Identity.SessionID = siblingID
	sibling.ops[0].Admission.SessionID = siblingID
	sibling.ops[0].Admission.AdmittedEntry.SessionID = siblingID
	for i := range sibling.entries {
		sibling.entries[i].env.SessionID = siblingID
		if sibling.entries[i].input != nil {
			sibling.entries[i].input.SessionID = siblingID
		}
		if sibling.entries[i].assistant != nil {
			sibling.entries[i].assistant.SessionID = siblingID
		}
	}

	t.Run("valid sibling stays usable", func(t *testing.T) {
		store := sibling.storage(t)
		if _, err := validateFixture(t, store, siblingID); err != nil {
			t.Fatalf("valid sibling rejected: %v", err)
		}
	})

	corruptions := []struct {
		name   string
		mutate func(t *testing.T, fixture *testGraph)
	}{
		{"corrupt entry payload", func(t *testing.T, fixture *testGraph) {
			raw, err := encodeInputEntry(*fixture.entries[0].input)
			mustEncode(t, err)
			fixture.entries[0].rawOverride = setKey(raw, "bogus", json.RawMessage(`1`))
		}},
		{"corrupt session register payload", func(t *testing.T, fixture *testGraph) {
			raw, err := encodeSessionRegister(fixture.session)
			mustEncode(t, err)
			state, err := decodePayloadObject(mustState(t, raw))
			mustEncode(t, err)
			state["lifecycle"] = json.RawMessage(`"stopping"`)
			patchedState, err := json.Marshal(state)
			mustEncode(t, err)
			obj := wireObject(t, raw)
			obj["state"] = patchedState
			patched, err := json.Marshal(obj)
			mustEncode(t, err)
			fixture.sessionRawOverride = patched
		}},
		{"two running operations", func(t *testing.T, fixture *testGraph) {
			second := validOperationRecord()
			second.Admission.OperationID = "op-2"
			second.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(1)}
			fixture.ops = append(fixture.ops, second)
		}},
		{"current operation missing", func(t *testing.T, fixture *testGraph) {
			fixture.session.State.CurrentOperationID = "ghost"
		}},
		{"current operation not running", func(t *testing.T, fixture *testGraph) {
			// Point the Session at a settled operation below by terminating op-1
			// without fixing the current pointer naming it as running.
			stamped := testTime
			fixture.ops[0].State.Status = OperationFailure
			fixture.ops[0].State.SettledAt = &stamped
			fixture.ops[0].State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(9)}, Detail: "boom"}
		}},
		{"running operation unnamed by session", func(t *testing.T, fixture *testGraph) {
			fixture.session.State.CurrentOperationID = ""
		}},
		{"running operation in archived session", func(t *testing.T, fixture *testGraph) {
			stamped := testTime
			fixture.session.State.Lifecycle = LifecycleArchived
			fixture.session.State.ArchivedAt = &stamped
		}},
		{"admitted entry missing", func(t *testing.T, fixture *testGraph) {
			fixture.ops[0].Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: otherEntry()}
		}},
		{"admitted entry wrong kind", func(t *testing.T, fixture *testGraph) {
			fixture.ops[0].Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
		}},
		{"entry owned by unknown operation", func(t *testing.T, fixture *testGraph) {
			input := validInputEntry("ghost")
			input.EntryID = hexID(1)
			fixture.entries[0].input = &input
			fixture.entries[0].env.OperationID = "ghost"
		}},
		{"published call neither pending nor result", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
		}},
		{"pending call with terminal result too", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			result := validToolResultEntry(testOpID)
			result.EntryID = hexID(3)
			result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 3, CommittedAt: testTime},
				toolResult: &result,
			})
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(3),
			}}
		}},
		{"second result breaks reservation identity", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			result := validToolResultEntry(testOpID)
			result.EntryID = hexID(3)
			result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 3, CommittedAt: testTime},
				toolResult: &result,
			})
			dup := validToolResultEntry(testOpID)
			dup.EntryID = hexID(4)
			dup.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime},
				toolResult: &dup,
			})
		}},
		{"result answers unpublished call", func(t *testing.T, fixture *testGraph) {
			result := validToolResultEntry(testOpID)
			result.EntryID = hexID(3)
			result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 3, CommittedAt: testTime},
				toolResult: &result,
			})
		}},
		{"terminal operation without settlement entry", func(t *testing.T, fixture *testGraph) {
			fixture.session.State.CurrentOperationID = ""
			stamped := testTime
			fixture.ops[0].State.Status = OperationSuccess
			fixture.ops[0].State.SettledAt = &stamped
			fixture.ops[0].State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(9)}}
		}},
		{"settlement status disagreement", func(t *testing.T, fixture *testGraph) {
			fixture.session.State.CurrentOperationID = ""
			stamped := testTime
			fixture.ops[0].State.Status = OperationSuccess
			fixture.ops[0].State.SettledAt = &stamped
			fixture.ops[0].State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
			settlement := validSettlementEntry()
			settlement.EntryID = hexID(3)
			settlement.Status = OperationFailure
			settlement.Detail = "boom"
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime},
				settlement: &settlement,
			})
		}},
		{"running operation carries settlement", func(t *testing.T, fixture *testGraph) {
			settlement := validSettlementEntry()
			settlement.EntryID = hexID(3)
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime},
				settlement: &settlement,
			})
		}},
		{"operation usage disagrees with entries", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.Usage = testUsage(4)
			fixture.entries[1].assistant = &assistant
		}},
		{"session usage disagrees with entries", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.Usage = testUsage(4)
			fixture.entries[1].assistant = &assistant
			totals := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: *testUsage(4)}}}
			fixture.ops[0].State.Usage = totals
		}},
		{"duplicate entry identity", func(t *testing.T, fixture *testGraph) {
			fixture.entries[1].env.ID = hexID(1)
		}},
		{"duplicate operation identity", func(t *testing.T, fixture *testGraph) {
			fixture.ops = append(fixture.ops, validOperationRecord())
		}},
		{"stored hook_result record", func(t *testing.T, fixture *testGraph) {
			fixture.entries = append(fixture.entries, testEntry{
				env:         Entry{SessionID: testSessionID, ID: hexID(7), OperationID: testOpID, Kind: EntryHookResult, Sequence: 7, CommittedAt: testTime},
				rawOverride: json.RawMessage(`{}`),
			})
		}},
		{"stored compaction record", func(t *testing.T, fixture *testGraph) {
			fixture.entries = append(fixture.entries, testEntry{
				env:         Entry{SessionID: testSessionID, ID: hexID(7), Kind: EntryCompaction, Sequence: 7, CommittedAt: testTime},
				rawOverride: json.RawMessage(`{}`),
			})
		}},
		{"root with operationless input", func(t *testing.T, fixture *testGraph) {
			copied := validInputEntry("")
			copied.EntryID = hexID(3)
			fixture.entries = append(fixture.entries, testEntry{
				env:   Entry{SessionID: testSessionID, ID: hexID(3), Kind: EntryInput, Sequence: 3, CommittedAt: testTime},
				input: &copied,
			})
		}},
		{"fork with operationless entry after owned", func(t *testing.T, fixture *testGraph) {
			fixture.session.Identity.SourceSessionID = otherSession()
			fixture.session.Identity.SourceBoundaryEntryID = otherEntry()
			copied := validInputEntry("")
			copied.EntryID = hexID(3)
			fixture.entries = append(fixture.entries, testEntry{
				env:   Entry{SessionID: testSessionID, ID: hexID(3), Kind: EntryInput, Sequence: 3, CommittedAt: testTime},
				input: &copied,
			})
		}},
		{"admitted entry owned by another operation", func(t *testing.T, fixture *testGraph) {
			fixture.session.State.CurrentOperationID = "op-2"
			stamped := testTime
			fixture.ops[0].State.Status = OperationSuccess
			fixture.ops[0].State.SettledAt = &stamped
			fixture.ops[0].State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
			settlement := validSettlementEntry()
			settlement.EntryID = hexID(3)
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime},
				settlement: &settlement,
			})
			second := validOperationRecord()
			second.Admission.OperationID = "op-2"
			second.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(1)}
			fixture.ops = append(fixture.ops, second)
		}},
		{"admitted entry that is a copied input", func(t *testing.T, fixture *testGraph) {
			fixture.session.Identity.SourceSessionID = otherSession()
			fixture.session.Identity.SourceBoundaryEntryID = otherEntry()
			copied := validInputEntry("")
			copied.EntryID = hexID(1)
			fixture.entries[0] = testEntry{
				env:   Entry{SessionID: testSessionID, ID: hexID(1), Kind: EntryInput, Sequence: 1, CommittedAt: testTime},
				input: &copied,
			}
		}},
		{"duplicate reservation across two operations", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(6)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(6),
			}}
			secondInput := validInputEntry("op-2")
			secondInput.EntryID = hexID(4)
			secondAssistant := validAssistantEntry("op-2")
			secondAssistant.EntryID = hexID(5)
			secondCall := validToolCallRecord()
			secondCall.ID = "call-9"
			secondCall.ResultEntryID = hexID(6)
			secondAssistant.ToolCalls = []toolCallRecord{secondCall}
			secondResult := validToolResultEntry("op-2")
			secondResult.EntryID = hexID(6)
			secondResult.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(5)}
			secondResult.ToolCallID = "call-9"
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: "op-2", Kind: EntryInput, Sequence: 4, CommittedAt: testTime}, input: &secondInput},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: "op-2", Kind: EntryAssistant, Sequence: 5, CommittedAt: testTime}, assistant: &secondAssistant},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: "op-2", Kind: EntryToolResult, Sequence: 6, CommittedAt: testTime}, toolResult: &secondResult},
			)
			second := validOperationRecord()
			second.Admission.OperationID = "op-2"
			second.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(4)}
			stamped := testTime
			second.State.Status = OperationSuccess
			second.State.SettledAt = &stamped
			second.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(7)}}
			fixture.ops = append(fixture.ops, second)
			settlement := validSettlementEntry()
			settlement.EntryID = hexID(7)
			settlement.OperationID = "op-2"
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(7), OperationID: "op-2", Kind: EntryOperationSettlement, Sequence: 7, CommittedAt: testTime},
				settlement: &settlement,
			})
		}},
		{"reservation id names a committed input entry", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(1)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(1),
			}}
		}},
		{"copied result answering an unpublished call", func(t *testing.T, fixture *testGraph) {
			fixture.session.Identity.SourceSessionID = otherSession()
			fixture.session.Identity.SourceBoundaryEntryID = otherEntry()
			fixture.session.State.CurrentOperationID = ""
			fixture.ops = nil
			fixture.entries = nil
			copiedInput := validInputEntry("")
			copiedInput.EntryID = hexID(1)
			copiedAssistant := validAssistantEntry("")
			copiedAssistant.EntryID = hexID(2)
			copiedResult := validToolResultEntry("")
			copiedResult.EntryID = hexID(3)
			copiedResult.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			fixture.entries = []testEntry{
				{env: Entry{SessionID: testSessionID, ID: hexID(1), Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &copiedInput},
				{env: Entry{SessionID: testSessionID, ID: hexID(2), Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &copiedAssistant},
				{env: Entry{SessionID: testSessionID, ID: hexID(3), Kind: EntryToolResult, Sequence: 3, CommittedAt: testTime}, toolResult: &copiedResult},
			}
		}},
		{"copied result whose id differs from the reservation", func(t *testing.T, fixture *testGraph) {
			fixture.session.Identity.SourceSessionID = otherSession()
			fixture.session.Identity.SourceBoundaryEntryID = otherEntry()
			fixture.session.State.CurrentOperationID = ""
			fixture.ops = nil
			fixture.entries = nil
			copiedInput := validInputEntry("")
			copiedInput.EntryID = hexID(1)
			copiedAssistant := validAssistantEntry("")
			copiedAssistant.EntryID = hexID(2)
			copiedCall := validToolCallRecord()
			copiedCall.ResultEntryID = hexID(9)
			copiedAssistant.ToolCalls = []toolCallRecord{copiedCall}
			copiedResult := validToolResultEntry("")
			copiedResult.EntryID = hexID(3)
			copiedResult.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			fixture.entries = []testEntry{
				{env: Entry{SessionID: testSessionID, ID: hexID(1), Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &copiedInput},
				{env: Entry{SessionID: testSessionID, ID: hexID(2), Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &copiedAssistant},
				{env: Entry{SessionID: testSessionID, ID: hexID(3), Kind: EntryToolResult, Sequence: 3, CommittedAt: testTime}, toolResult: &copiedResult},
			}
		}},
		{"model effect reserves a committed entry", func(t *testing.T, fixture *testGraph) {
			fixture.ops[0].State.ActiveEffect = &ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(1)}
		}},
		{"model effect reserves a tool call reservation", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(3),
			}}
			fixture.ops[0].State.ActiveEffect = &ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(3)}
		}},
		{"terminal op carries a second settlement entry", func(t *testing.T, fixture *testGraph) {
			fixture.session.State.CurrentOperationID = ""
			stamped := testTime
			fixture.ops[0].State.Status = OperationSuccess
			fixture.ops[0].State.SettledAt = &stamped
			fixture.ops[0].State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
			settlement := validSettlementEntry()
			settlement.EntryID = hexID(3)
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime},
				settlement: &settlement,
			})
			second := validSettlementEntry()
			second.EntryID = hexID(4)
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 4, CommittedAt: testTime},
				settlement: &second,
			})
		}},
		{"copied call reservation without a copied result", func(t *testing.T, fixture *testGraph) {
			fixture.session.Identity.SourceSessionID = otherSession()
			fixture.session.Identity.SourceBoundaryEntryID = otherEntry()
			fixture.session.State.CurrentOperationID = ""
			fixture.ops = nil
			fixture.entries = nil
			copiedInput := validInputEntry("")
			copiedInput.EntryID = hexID(1)
			copiedAssistant := validAssistantEntry("")
			copiedAssistant.EntryID = hexID(2)
			copiedCall := validToolCallRecord()
			copiedCall.ResultEntryID = hexID(9)
			copiedAssistant.ToolCalls = []toolCallRecord{copiedCall}
			fixture.entries = []testEntry{
				{env: Entry{SessionID: testSessionID, ID: hexID(1), Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &copiedInput},
				{env: Entry{SessionID: testSessionID, ID: hexID(2), Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &copiedAssistant},
			}
		}},
		{"register and settlement detail disagree", func(t *testing.T, fixture *testGraph) {
			fixture.session.State.CurrentOperationID = ""
			stamped := testTime
			fixture.ops[0].State.Status = OperationFailure
			fixture.ops[0].State.SettledAt = &stamped
			fixture.ops[0].State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}, Detail: "boom"}
			settlement := validSettlementEntry()
			settlement.EntryID = hexID(3)
			settlement.Status = OperationFailure
			settlement.Detail = "other"
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime},
				settlement: &settlement,
			})
		}},
		{"fork-copied assistant keeps usage", func(t *testing.T, fixture *testGraph) {
			fixture.session.Identity.SourceSessionID = otherSession()
			fixture.session.Identity.SourceBoundaryEntryID = otherEntry()
			fixture.session.State.CurrentOperationID = ""
			fixture.ops = nil
			fixture.entries = nil
			copied := validAssistantEntry("")
			copied.EntryID = hexID(1)
			raw, err := encodeAssistantEntry(copied)
			mustEncode(t, err)
			withUsage := setKey(raw, "usage", json.RawMessage(`{"input_tokens":2,"cached_input_tokens":0,"output_tokens":0}`))
			fixture.entries = []testEntry{
				{env: Entry{SessionID: testSessionID, ID: hexID(1), Kind: EntryAssistant, Sequence: 1, CommittedAt: testTime}, rawOverride: withUsage},
			}
		}},
	}
	for _, tc := range corruptions {
		t.Run(tc.name, func(t *testing.T) {
			fixture := validTestGraph()
			tc.mutate(t, fixture)
			store := fixture.storage(t)
			if tc.name == "orphan entries without session register" {
				delete(store.registers, testSessionID)
			}
			wantCorruption(t, func() error {
				_, err := validateFixture(t, store, testSessionID)
				return err
			}())
		})
	}

	t.Run("one corrupt record among valid siblings", func(t *testing.T) {
		broken := validTestGraph()
		raw, err := encodeInputEntry(*broken.entries[0].input)
		mustEncode(t, err)
		broken.entries[0].rawOverride = setKey(raw, "bogus", json.RawMessage(`1`))
		store := broken.storage(t)
		for id, regs := range sibling.storage(t).registers {
			store.registers[id] = append(store.registers[id], regs...)
		}
		for id, entries := range sibling.storage(t).entries {
			store.entries[id] = append(store.entries[id], entries...)
		}
		wantCorruption(t, func() error {
			_, err := validateFixture(t, store, testSessionID)
			return err
		}())
		if _, err := validateFixture(t, store, siblingID); err != nil {
			t.Fatalf("valid sibling session not usable alongside corruption: %v", err)
		}
	})

	t.Run("orphan entries and registers without session register", func(t *testing.T) {
		fixture := validTestGraph()
		store := fixture.storage(t)
		delete(store.registers, testSessionID)
		wantCorruption(t, func() error {
			_, err := validateFixture(t, store, testSessionID)
			return err
		}())
	})

	t.Run("fully absent session passes not-found through", func(t *testing.T) {
		store := &graphStorage{registers: map[string][]Register{}, entries: map[string][]Entry{}}
		_, err := validateFixture(t, store, testSessionID)
		if err == nil || !errors.Is(err, ErrNotFound) {
			t.Fatalf("absent session = %v, want the ErrNotFound class", err)
		}
		var corrupt *CorruptionError
		if errors.As(err, &corrupt) {
			t.Fatalf("absent session misclassified as corruption: %v", err)
		}
	})
}

// TestGraphValidatorPassesStorageFailuresThrough proves storage-service
// failures and the landed not-found class pass through the validator
// unmodified, never reclassified as Session corruption.
func TestGraphValidatorPassesStorageFailuresThrough(t *testing.T) {
	failure := fmt.Errorf("disk on fire")
	fixture := validTestGraph()

	t.Run("read registers failure", func(t *testing.T) {
		store := fixture.storage(t)
		store.registersErr = failure
		_, err := validateFixture(t, store, testSessionID)
		if !errors.Is(err, failure) || err != failure {
			t.Fatalf("registers failure = %v, want the exact error unmodified", err)
		}
		wantNotCorruption(t, err)
	})
	t.Run("read entries failure", func(t *testing.T) {
		store := fixture.storage(t)
		store.entriesErr = failure
		_, err := validateFixture(t, store, testSessionID)
		if err != failure {
			t.Fatalf("entries failure = %v, want the exact error unmodified", err)
		}
		wantNotCorruption(t, err)
	})
	t.Run("addressed-but-absent record", func(t *testing.T) {
		store := fixture.storage(t)
		store.registersErr = fmt.Errorf("session row: %w", ErrNotFound)
		_, err := validateFixture(t, store, testSessionID)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("absent record = %v, want the landed not-found class unchanged", err)
		}
		wantNotCorruption(t, err)
	})
	t.Run("malformed session identity is invalid input", func(t *testing.T) {
		_, err := validateFixture(t, fixture.storage(t), "not-an-id")
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("malformed session id = %v, want ErrInvalid", err)
		}
		wantNotCorruption(t, err)
	})
}

func wantNotCorruption(t *testing.T, err error) {
	t.Helper()
	var corrupt *CorruptionError
	if errors.As(err, &corrupt) {
		t.Fatalf("storage failure reclassified as corruption: %v", err)
	}
}

// TestGraphValidatorForkReferenceRules proves the rewritten-reference rules
// of independently copied fork-prefix entries: copied references resolve
// inside the copied prefix, and an owned signal's related Operation must
// resolve while a copied signal's informational history may not.
func TestGraphValidatorForkReferenceRules(t *testing.T) {
	buildFork := func(t *testing.T) *testGraph {
		t.Helper()
		fork := &testGraph{session: validSessionRecord()}
		fork.session.Identity.SourceSessionID = otherSession()
		fork.session.Identity.SourceBoundaryEntryID = otherEntry()
		copiedInput := validInputEntry("")
		copiedInput.EntryID = hexID(1)
		copiedAssistant := validAssistantEntry("")
		copiedAssistant.EntryID = hexID(2)
		copiedSignal := validSignalEntry("")
		copiedSignal.EntryID = hexID(3)
		copiedSignal.RelatedOperation = operationRef{SessionID: otherSession(), OperationID: "source-op"}
		fork.entries = []testEntry{
			{env: Entry{SessionID: testSessionID, ID: hexID(1), Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &copiedInput},
			{env: Entry{SessionID: testSessionID, ID: hexID(2), Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &copiedAssistant},
			{env: Entry{SessionID: testSessionID, ID: hexID(3), Kind: EntrySignal, Sequence: 3, CommittedAt: testTime}, signal: &copiedSignal},
		}
		return fork
	}

	t.Run("copied tool result resolves inside the prefix", func(t *testing.T) {
		fork := buildFork(t)
		copiedAssistant := validAssistantEntry("")
		copiedAssistant.EntryID = hexID(2)
		call := validToolCallRecord()
		call.ResultEntryID = hexID(4)
		copiedAssistant.ToolCalls = []toolCallRecord{call}
		fork.entries[1].assistant = &copiedAssistant
		copiedResult := validToolResultEntry("")
		copiedResult.EntryID = hexID(4)
		copiedResult.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
		fork.entries = append(fork.entries, testEntry{
			env:        Entry{SessionID: testSessionID, ID: hexID(4), Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime},
			toolResult: &copiedResult,
		})
		if _, err := validateFixture(t, fork.storage(t), testSessionID); err != nil {
			t.Fatalf("valid copied prefix rejected: %v", err)
		}
	})

	t.Run("copied tool result outside the prefix", func(t *testing.T) {
		fork := buildFork(t)
		copiedResult := validToolResultEntry("")
		copiedResult.EntryID = hexID(4)
		copiedResult.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: otherEntry()}
		fork.entries = append(fork.entries, testEntry{
			env:        Entry{SessionID: testSessionID, ID: hexID(4), Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime},
			toolResult: &copiedResult,
		})
		wantCorruption(t, func() error {
			_, err := validateFixture(t, fork.storage(t), testSessionID)
			return err
		}())
	})

	t.Run("copied tool result pointing at an owned assistant", func(t *testing.T) {
		fork := buildFork(t)
		owned := validAssistantEntry(testOpID)
		owned.EntryID = hexID(2)
		fork.entries[1].assistant = &owned
		fork.entries[1].env.OperationID = testOpID
		copiedResult := validToolResultEntry("")
		copiedResult.EntryID = hexID(4)
		copiedResult.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
		fork.entries = append(fork.entries, testEntry{
			env:        Entry{SessionID: testSessionID, ID: hexID(4), Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime},
			toolResult: &copiedResult,
		})
		wantCorruption(t, func() error {
			_, err := validateFixture(t, fork.storage(t), testSessionID)
			return err
		}())
	})

	t.Run("owned signal resolves its related operation", func(t *testing.T) {
		fixture := validTestGraph()
		owned := validSignalEntry(testOpID)
		owned.EntryID = hexID(3)
		fixture.entries = append(fixture.entries, testEntry{
			env:    Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntrySignal, Sequence: 3, CommittedAt: testTime},
			signal: &owned,
		})
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("owned signal with resolvable reference rejected: %v", err)
		}
	})

	t.Run("owned signal with unresolvable operation", func(t *testing.T) {
		fixture := validTestGraph()
		owned := validSignalEntry(testOpID)
		owned.EntryID = hexID(3)
		owned.RelatedOperation = operationRef{SessionID: testSessionID, OperationID: "ghost"}
		fixture.entries = append(fixture.entries, testEntry{
			env:    Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntrySignal, Sequence: 3, CommittedAt: testTime},
			signal: &owned,
		})
		wantCorruption(t, func() error {
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			return err
		}())
	})
}

// TestGraphViewCarriesEnvelopeMetadata proves the decoded view carries the
// envelope fields (revision, sequence, commit time) alongside typed payloads.
func TestGraphViewCarriesEnvelopeMetadata(t *testing.T) {
	fixture := validTestGraph()
	fixture.session.Revision = 42
	fixture.ops[0].Revision = 7
	view, err := validateFixture(t, fixture.storage(t), testSessionID)
	if err != nil {
		t.Fatalf("valid graph rejected: %v", err)
	}
	if view.Session.Revision != 42 {
		t.Fatalf("view session revision = %d", view.Session.Revision)
	}
	if view.Operations[0].Revision != 7 {
		t.Fatalf("view operation revision = %d", view.Operations[0].Revision)
	}
	if !view.Entries[0].Envelope.CommittedAt.Equal(testTime) || view.Entries[0].Envelope.Sequence != 1 {
		t.Fatalf("view entry envelope = %+v", view.Entries[0].Envelope)
	}
}

// Compile-time proof the fixture storage implements the landed read side.
var _ Storage = (*graphStorage)(nil)
