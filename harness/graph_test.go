package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
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
	hookResult  *hookResultEntry
	settlement  *operationSettlementEntry
	compaction  *contextCheckpointEntry
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

// storage encodes every fixture record into a fresh read-only substitute.
func (g *testGraph) storage(t testing.TB) *graphStorage {
	t.Helper()
	out := &graphStorage{registers: map[string][]Register{}, entries: map[string][]Entry{}}
	sessionRaw, err := encodeSessionRegister(g.session)
	testassert.NoError(t, err)
	if g.sessionRawOverride != nil {
		sessionRaw = g.sessionRawOverride
	}
	out.registers[g.session.Identity.SessionID] = append(out.registers[g.session.Identity.SessionID],
		Register{Key: RegisterKey{SessionID: g.session.Identity.SessionID, Kind: RegisterSession}, Revision: g.session.Revision, Payload: sessionRaw})
	for i := range g.ops {
		op := &g.ops[i]
		opRaw, err := encodeOperationRegister(*op)
		testassert.NoError(t, err)
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
			testassert.NoError(t, err)
			env.Payload = raw
		case entry.assistant != nil:
			raw, err := encodeAssistantEntry(*entry.assistant)
			testassert.NoError(t, err)
			env.Payload = raw
		case entry.toolResult != nil:
			raw, err := encodeToolResultEntry(*entry.toolResult)
			testassert.NoError(t, err)
			env.Payload = raw
		case entry.signal != nil:
			raw, err := encodeSignalEntry(*entry.signal)
			testassert.NoError(t, err)
			env.Payload = raw
		case entry.hookResult != nil:
			raw, err := encodeHookResultEntry(*entry.hookResult)
			testassert.NoError(t, err)
			env.Payload = raw
		case entry.settlement != nil:
			raw, err := encodeOperationSettlementEntry(*entry.settlement)
			testassert.NoError(t, err)
			env.Payload = raw
		case entry.compaction != nil:
			raw, err := encodeContextCheckpointEntry(*entry.compaction)
			testassert.NoError(t, err)
			env.Payload = raw
		default:
			t.Fatalf("fixture entry %s has no payload", env.ID)
		}
		out.entries[g.session.Identity.SessionID] = append(out.entries[g.session.Identity.SessionID], env)
	}
	return out
}

// conversationModelEffect returns a coherent request-bound conversation
// model active effect over the given result reservation with its published
// running request state, matching the validTestGraph canonical references.
func conversationModelEffect(resultID string) (*ActiveEffect, *RunningRequest) {
	evidence := &RequestEvidence{BoundaryEntryID: hexID(1)}
	effect := &ActiveEffect{
		Kind:          EffectModel,
		ResultEntryID: resultID,
		RequestID:     "request-active",
		Model:         &model.ModelRef{Provider: "prov", Model: "gpt-x"},
		Purpose:       InvocationPurposeConversation,
		Evidence:      evidence,
	}
	return effect, &RunningRequest{RequestID: "request-active", BoundaryEntryID: hexID(1)}
}

// toolBoundHookEffect returns a coherent tool-bound hook active effect for
// the fixture pending call "call-1".
func toolBoundHookEffect(resultID, hookID string) *ActiveEffect {
	return &ActiveEffect{
		Kind:                 EffectHook,
		ResultEntryID:        resultID,
		ToolCallID:           "call-1",
		HookID:               hookID,
		ExtensionID:          "cap-a",
		InputArgumentsBase64: base64.StdEncoding.EncodeToString([]byte(`{"x":1}`)),
	}
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
	assistant.RequestID = hexID(2)
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
	testassert.Require(t, ok, "session payload carries no state member")
	return state
}

func wantCorruption(t *testing.T, err error) *CorruptionError {
	t.Helper()
	testassert.Require(t, err != nil, "expected corruption, got a valid graph")
	var corrupt *CorruptionError
	testassert.Require(t, errors.As(err, &corrupt), "error %v is not a CorruptionError", err)
	testassert.Equal(t, corrupt.SessionID, testSessionID)
	return corrupt
}

// TestGraphValidatorAcceptsCoherentSessions proves the validator accepts the
// coherent durable shapes and returns the owned decoded view: the Session
// record, Operation records in read order, and committed-order typed entries.
func TestGraphValidatorAcceptsCoherentSessions(t *testing.T) {
	// singleCallGraph builds the coherent fixture whose one published
	// assistant call stays pending with the supplied reserved result entry ID.
	singleCallGraph := func(resultEntryID string) *testGraph {
		fixture := validTestGraph()
		assistant := validAssistantEntry(testOpID)
		assistant.EntryID = hexID(2)
		assistant.RequestID = hexID(2)
		call := validToolCallRecord()
		call.ResultEntryID = resultEntryID
		assistant.ToolCalls = []toolCallRecord{call}
		fixture.entries[1].assistant = &assistant
		fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
			AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
			CallID:         "call-1",
			ResultEntryID:  resultEntryID,
		}}
		return fixture
	}

	t.Run("running session", func(t *testing.T) {
		fixture := validTestGraph()
		view, err := validateFixture(t, fixture.storage(t), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, view.Session.Revision == validSessionRecord().Revision && view.Session.Identity.SessionID == testSessionID && view.Session.State.CurrentOperationID == testOpID, "view session = %+v", view.Session)
		testassert.Require(t, len(view.Operations) == 1 && view.Operations[0].Admission.OperationID == testOpID, "view operations = %+v", view.Operations)
		{
			_, ok := view.Operation(testOpID)
			testassert.Require(t, ok, "Operation lookup by identity failed")
		}
		testassert.Require(t, len(view.Entries) == 2 && view.Entries[0].Input != nil && view.Entries[1].Assistant != nil, "view entries = %+v", view.Entries)
		testassert.Require(t, view.Entries[0].Envelope.ID == hexID(1) && view.Entries[1].Envelope.ID == hexID(2), "view entries are not in committed order: %+v", view.Entries)
	})

	t.Run("request hook provenance with interleaved entry-less pure identities", func(t *testing.T) { // Entry-less pure identities are the informational exception to the
		// complete-order rule: they may interleave with the result-backed
		// effectful identities at the hook's consumed boundary and at the
		// answering assistant.
		fixture := validTestGraph()
		hook := validRequestBoundHookResult(hookSucceeded)
		hook.EntryID = hexID(3)
		hook.RequestID = "request-one"
		hook.InputEvidence = &RequestEvidence{BoundaryEntryID: hexID(1), HookIDs: []HookIdentity{{ExtensionID: "ext.pure", HookID: "cap.pure.one"}}}
		assistant := validAssistantEntry(testOpID)
		assistant.EntryID = hexID(2)
		assistant.RequestID = "request-one"
		assistant.Evidence.HookIDs = []HookIdentity{{ExtensionID: "ext.pure", HookID: "cap.pure.one"}, {ExtensionID: "ext.one", HookID: "cap.hook.one"}}
		assistantRow := fixture.entries[1]
		assistantRow.assistant = &assistant
		assistantRow.env.Sequence = 3
		fixture.entries = []testEntry{
			fixture.entries[0],
			{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 2, CommittedAt: testTime}, hookResult: &hook},
			assistantRow,
		}
		view, err := validateFixture(t, fixture.storage(t), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, len(view.Entries) == 3 && view.Entries[1].HookResult != nil, "view entries = %+v, want the committed hook before its answering assistant", view.Entries)
	})

	// The accepted tool-selection reconciliation keeps three valid shapes:
	// interleaved entry-less pure identities, an interrupted chain whose
	// settled result keeps its prior successful contributors, and a quiet-gap
	// recovery settlement that invents neither arguments nor provenance.
	toolProvenanceGraph := func(t *testing.T) *testGraph {
		fixture := validTestGraph()
		assistant := validAssistantEntry(testOpID)
		assistant.EntryID = hexID(2)
		assistant.RequestID = hexID(2)
		call := validToolCallRecord()
		call.ResultEntryID = hexID(3)
		assistant.ToolCalls = []toolCallRecord{call}
		fixture.entries[1].assistant = &assistant
		return fixture
	}
	t.Run("accepted tool selection with interleaved entry-less pure identities", func(t *testing.T) {
		fixture := toolProvenanceGraph(t)
		hook := validHookResultEntry(hookSucceeded)
		hook.EntryID = hexID(5)
		result := validToolResultEntry(testOpID)
		result.EntryID = hexID(3)
		result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
		result.NormalizedArguments = json.RawMessage(`{"x":1}`)
		result.HookIDs = []HookIdentity{{ExtensionID: "ext.pure", HookID: "cap.pure.one"}, {ExtensionID: "ext.one", HookID: "cap.hook.one"}}
		fixture.entries = append(fixture.entries,
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &hook},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime}, toolResult: &result},
		)
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("accepted selection with interleaved pure identities: %v", err)
		}
	})
	t.Run("interrupted hook chain keeps prior successful contributors", func(t *testing.T) {
		fixture := toolProvenanceGraph(t)
		first := validHookResultEntry(hookSucceeded)
		first.EntryID = hexID(5)
		second := validHookResultEntry(hookInterrupted)
		second.EntryID = hexID(6)
		second.HookID = "cap.hook.two"
		result := validToolResultEntry(testOpID)
		result.EntryID = hexID(3)
		result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
		result.Status = model.ResultInterrupted
		result.Content = "agent interrupted"
		result.OriginalContent = "agent interrupted"
		result.NormalizedArguments = json.RawMessage(`{"x":1}`)
		result.HookIDs = []HookIdentity{{ExtensionID: "ext.one", HookID: "cap.hook.one"}}
		fixture.entries = append(fixture.entries,
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &first},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntryHookResult, Sequence: 4, CommittedAt: testTime}, hookResult: &second},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 5, CommittedAt: testTime}, toolResult: &result},
		)
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("interrupted chain keeping prior successful contributors: %v", err)
		}
	})
	t.Run("quiet-gap settlement without accepted selection", func(t *testing.T) {
		fixture := toolProvenanceGraph(t)
		hook := validHookResultEntry(hookSucceeded)
		hook.EntryID = hexID(5)
		result := validToolResultEntry(testOpID)
		result.EntryID = hexID(3)
		result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
		result.Status = model.ResultInterrupted
		result.Content = "agent interrupted"
		result.OriginalContent = "agent interrupted"
		fixture.entries = append(fixture.entries,
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &hook},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime}, toolResult: &result},
		)
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("quiet-gap settlement without accepted selection: %v", err)
		}
	})

	t.Run("running request summary-only boundary equal to its checkpoint", func(t *testing.T) {
		comp := validCompactionEntry(testOpID)
		comp.EntryID = hexID(5)
		fixture := validTestGraph()
		fixture.entries = append(fixture.entries, testEntry{
			env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryCompaction, Sequence: 3, CommittedAt: testTime}, compaction: &comp,
		})
		fixture.session.State.CheckpointEntryID = hexID(5)
		fixture.ops[0].State.Request = &RunningRequest{RequestID: "request-active", CheckpointEntryID: hexID(5), BoundaryEntryID: hexID(5)}
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("summary-only running request boundary: %v", err)
		}
	})

	t.Run("running request names a later canonical tail", func(t *testing.T) {
		comp := validCompactionEntry(testOpID)
		comp.EntryID = hexID(5)
		signal := validSignalEntry(testOpID)
		signal.EntryID = hexID(6)
		fixture := validTestGraph()
		fixture.entries = append(fixture.entries,
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryCompaction, Sequence: 3, CommittedAt: testTime}, compaction: &comp},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntrySignal, Sequence: 4, CommittedAt: testTime}, signal: &signal},
		)
		fixture.session.State.CheckpointEntryID = hexID(5)
		fixture.ops[0].State.Request = &RunningRequest{RequestID: "request-active", CheckpointEntryID: hexID(5), BoundaryEntryID: hexID(6)}
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("later canonical tail boundary: %v", err)
		}
	})

	t.Run("manual maintenance record needs no consumed source", func(t *testing.T) {
		fixture := validTestGraph()
		stamped := testTime
		second := validOperationRecord()
		second.Admission.OperationID = "op-2"
		second.Admission.RequestKind = RequestKindCompact
		second.Admission.AdmittedEntry = EntryRef{} // a compact Operation admits no message entry of its own
		second.State.Status = OperationSuccess
		second.State.SettledAt = &stamped
		second.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(4)}}
		fixture.ops = append(fixture.ops, second)
		comp := validCompactionEntry("op-2")
		comp.EntryID = hexID(3)
		comp.ConsumedRequestID = "" // the dedicated compact Operation's own maintenance carries no conversation source
		settlement := validSettlementEntry()
		settlement.EntryID = hexID(4)
		settlement.OperationID = "op-2"
		fixture.entries = append(fixture.entries,
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: "op-2", Kind: EntryCompaction, Sequence: 3, CommittedAt: testTime}, compaction: &comp},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: "op-2", Kind: EntryOperationSettlement, Sequence: 4, CommittedAt: testTime}, settlement: &settlement},
		)
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("manual compaction record without a consumed source: %v", err)
		}
	})

	t.Run("running model effect", func(t *testing.T) {
		fixture := validTestGraph()
		effect, request := conversationModelEffect(hexID(3))
		fixture.ops[0].State.ActiveEffect = effect
		fixture.ops[0].State.Request = request
		view, err := validateFixture(t, fixture.storage(t), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, view.Operations[0].State.ActiveEffect != nil && view.Operations[0].State.ActiveEffect.Kind == EffectModel, "typed view lost the active model effect")
	})

	t.Run("pending tool call", func(t *testing.T) {
		fixture := singleCallGraph(hexID(3))
		view, err := validateFixture(t, fixture.storage(t), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, len(view.Entries[1].Assistant.ToolCalls) == 1, "typed assistant payload lost its call: %+v", view.Entries[1])
	})

	t.Run("resolved tool result", func(t *testing.T) {
		fixture := validTestGraph()
		assistant := validAssistantEntry(testOpID)
		assistant.EntryID = hexID(2)
		assistant.RequestID = hexID(2)
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
		{
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			testassert.NoError(t, err)
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
		{
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			testassert.NoError(t, err)
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
		copiedAssistant.RequestID = hexID(2)
		copiedCall := validToolCallRecord()
		copiedCall.ResultEntryID = hexID(3)
		copiedAssistant.ToolCalls = []toolCallRecord{copiedCall}
		copiedResult := validToolResultEntry("")
		copiedResult.EntryID = hexID(3)
		copiedResult.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
		copiedSignal := validSignalEntry("")
		copiedSignal.EntryID = hexID(4)
		copiedSignal.RelatedOperation = &operationRef{SessionID: otherSession(), OperationID: "source-op"}
		fixture.entries = []testEntry{
			{env: Entry{SessionID: testSessionID, ID: hexID(1), Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &copiedInput},
			{env: Entry{SessionID: testSessionID, ID: hexID(2), Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &copiedAssistant},
			{env: Entry{SessionID: testSessionID, ID: hexID(3), Kind: EntryToolResult, Sequence: 3, CommittedAt: testTime}, toolResult: &copiedResult},
			{env: Entry{SessionID: testSessionID, ID: hexID(4), Kind: EntrySignal, Sequence: 4, CommittedAt: testTime}, signal: &copiedSignal},
		}
		{
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("settled hook results", func(t *testing.T) {
		fixture := singleCallGraph(hexID(4))
		first := hookResultEntry{
			SessionID: testSessionID, EntryID: hexID(3), OperationID: testOpID,
			HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookSucceeded,
			Evidence: ExecutionObserved, Arguments: json.RawMessage(`{"x":1}`),
		}
		second := hookResultEntry{
			SessionID: testSessionID, EntryID: hexID(5), OperationID: testOpID,
			HookID: "cap.hook.two", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookFailed,
			Evidence: ExecutionObserved, Error: "hook broke",
		}
		fixture.entries = append(fixture.entries,
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &first},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 5, CommittedAt: testTime}, hookResult: &second},
		)
		view, err := validateFixture(t, fixture.storage(t), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, len(view.Entries) == 4 && view.Entries[2].HookResult != nil && view.Entries[3].HookResult != nil, "typed view lost the hook results: %+v", view.Entries)
	})

	t.Run("running hook effect", func(t *testing.T) {
		fixture := singleCallGraph(hexID(3))
		fixture.ops[0].State.ActiveEffect = toolBoundHookEffect(hexID(9), "cap.hook.one")
		view, err := validateFixture(t, fixture.storage(t), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, view.Operations[0].State.ActiveEffect != nil && view.Operations[0].State.ActiveEffect.Kind == EffectHook, "typed view lost the active hook effect")
	})

	t.Run("distinct hook identities containing separators", func(t *testing.T) {
		fixture := validTestGraph()
		assistant := validAssistantEntry(testOpID)
		assistant.EntryID = hexID(2)
		assistant.RequestID = hexID(2)
		callOne, callTwo := validToolCallRecord(), validToolCallRecord()
		callOne.ID, callTwo.ID = "c", "b\x00c"
		callOne.Ordinal, callTwo.Ordinal = 0, 1
		callOne.ResultEntryID, callTwo.ResultEntryID = hexID(4), hexID(5)
		assistant.ToolCalls = []toolCallRecord{callOne, callTwo}
		fixture.entries[1].assistant = &assistant
		// Legal sequential history: the first call settles before the second
		// call's hook runs, so the second call stays the only pending one.
		fixture.ops[0].State.PendingToolCalls = []PendingToolCall{
			{AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)}, CallID: "b\x00c", ResultEntryID: hexID(5)},
		}
		one := hookResultEntry{SessionID: testSessionID, EntryID: hexID(3), OperationID: testOpID, HookID: "a\x00b", ExtensionID: "ext.one", ToolCallID: "c", Status: hookSucceeded, Evidence: ExecutionObserved, Arguments: json.RawMessage(`{}`)}
		two := hookResultEntry{SessionID: testSessionID, EntryID: hexID(6), OperationID: testOpID, HookID: "a", ExtensionID: "ext.one", ToolCallID: "b\x00c", Status: hookSucceeded, Evidence: ExecutionObserved, Arguments: json.RawMessage(`{}`)}
		firstResult := validToolResultEntry(testOpID)
		firstResult.EntryID = hexID(4)
		firstResult.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
		firstResult.ToolCallID = "c"
		fixture.entries = append(fixture.entries,
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &one},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime}, toolResult: &firstResult},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntryHookResult, Sequence: 6, CommittedAt: testTime}, hookResult: &two},
		)
		{
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			testassert.NoError(t, err)
		}
	})
}

func TestGraphValidatorSessionUsageOverflowAcrossOperations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		first    int64
		overflow bool
	}{
		{name: "representable session total", first: math.MaxInt64 - 1},
		{name: "session-only overflow", first: math.MaxInt64, overflow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := benchValidatorFixture(2)
			ref := testModelRef()
			for i, count := range []int64{tc.first, 1} {
				usage := UsageCount{InputTokens: count}
				fixture.entries[4*i+3].settlement.Model = &ref
				fixture.entries[4*i+3].settlement.Usage = &usage
				fixture.ops[i].State.Usage = UsageTotals{ByModel: []ModelUsage{{Model: ref, Usage: usage}}}
			}
			fixture.session.State.Usage = UsageTotals{ByModel: []ModelUsage{{Model: ref, Usage: UsageCount{InputTokens: math.MaxInt64}}}}
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			if tc.overflow {
				corrupt := wantCorruption(t, err)
				testassert.Require(t, strings.Contains(corrupt.Error(), "session usage overflows:"), "overflow = %v, want the Session accumulator's rejection", corrupt)
			} else if err != nil {
				t.Fatalf("representable total rejected: %v", err)
			}
		})
	}
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
		{
			_, err := validateFixture(t, store, siblingID)
			testassert.NoError(t, err)
		}
	})

	corruptions := []struct {
		name   string
		mutate func(t *testing.T, fixture *testGraph)
	}{
		{"corrupt entry payload", func(t *testing.T, fixture *testGraph) {
			raw, err := encodeInputEntry(*fixture.entries[0].input)
			testassert.NoError(t, err)
			fixture.entries[0].rawOverride = setKey(raw, "bogus", json.RawMessage(`1`))
		}},
		{"corrupt session register payload", func(t *testing.T, fixture *testGraph) {
			raw, err := encodeSessionRegister(fixture.session)
			testassert.NoError(t, err)
			state, err := decodePayloadObject(mustState(t, raw))
			testassert.NoError(t, err)
			state["lifecycle"] = json.RawMessage(`"stopping"`)
			patchedState, err := json.Marshal(state)
			testassert.NoError(t, err)
			obj := wireObject(t, raw)
			obj["state"] = patchedState
			patched, err := json.Marshal(obj)
			testassert.NoError(t, err)
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
		{"extra owned input under a running message operation", func(t *testing.T, fixture *testGraph) {
			extra := validInputEntry(testOpID)
			extra.EntryID = hexID(4)
			fixture.entries = append(fixture.entries, testEntry{
				env:   Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryInput, Sequence: 4, CommittedAt: testTime},
				input: &extra,
			})
		}},
		{"extra owned input under a terminal message operation", func(t *testing.T, fixture *testGraph) {
			fixture.session.State.CurrentOperationID = ""
			op := &fixture.ops[0]
			stamped := testTime
			op.State.Status = OperationSuccess
			op.State.SettledAt = &stamped
			op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
			settlement := operationSettlementEntry{SessionID: testSessionID, EntryID: hexID(3), OperationID: testOpID, Status: OperationSuccess}
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime}, settlement: &settlement})
			extra := validInputEntry(testOpID)
			extra.EntryID = hexID(4)
			fixture.entries = append(fixture.entries, testEntry{
				env:   Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryInput, Sequence: 4, CommittedAt: testTime},
				input: &extra,
			})
		}},
		{"input owned by a compact operation", func(t *testing.T, fixture *testGraph) {
			fixture.session.State.CurrentOperationID = ""
			op := &fixture.ops[0]
			op.Admission.RequestKind = RequestKindCompact
			op.Admission.AdmittedEntry = EntryRef{}
			stamped := testTime
			op.State.Status = OperationSuccess
			op.State.SettledAt = &stamped
			op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
			settlement := operationSettlementEntry{SessionID: testSessionID, EntryID: hexID(3), OperationID: testOpID, Status: OperationSuccess}
			fixture.entries = []testEntry{
				{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime}, settlement: &settlement},
			}
			extra := validInputEntry(testOpID)
			extra.EntryID = hexID(4)
			fixture.entries = append(fixture.entries, testEntry{
				env:   Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryInput, Sequence: 4, CommittedAt: testTime},
				input: &extra,
			})
		}},
		{"published call neither pending nor result", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
		}},
		{"pending call with terminal result too", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
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
			assistant.RequestID = hexID(2)
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
			assistant.RequestID = hexID(2)
			assistant.Usage = testUsage(4)
			fixture.entries[1].assistant = &assistant
		}},
		{"session usage disagrees with entries", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			assistant.Usage = testUsage(4)
			fixture.entries[1].assistant = &assistant
			totals := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: *testUsage(4)}}}
			fixture.ops[0].State.Usage = totals
		}},
		{"usage accumulation overflows", func(t *testing.T, fixture *testGraph) {
			first := validAssistantEntry(testOpID)
			first.EntryID = hexID(2)
			first.Usage = &UsageCount{InputTokens: math.MaxInt64}
			fixture.entries[1].assistant = &first
			second := validAssistantEntry(testOpID)
			second.EntryID = hexID(3)
			second.RequestID = "request-two"
			second.Usage = &UsageCount{InputTokens: 1}
			fixture.entries = append(fixture.entries, testEntry{
				env:       Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryAssistant, Sequence: 3, CommittedAt: testTime},
				assistant: &second,
			})
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
			assistant.RequestID = hexID(2)
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
			secondAssistant.RequestID = hexID(5)
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
			assistant.RequestID = hexID(2)
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
			copiedAssistant.RequestID = hexID(2)
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
			copiedAssistant.RequestID = hexID(2)
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
			effect, request := conversationModelEffect(hexID(1))
			fixture.ops[0].State.ActiveEffect = effect
			fixture.ops[0].State.Request = request
		}},
		{"model effect reserves a tool call reservation", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(3),
			}}
			effect, request := conversationModelEffect(hexID(3))
			fixture.ops[0].State.ActiveEffect = effect
			fixture.ops[0].State.Request = request
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
			copiedAssistant.RequestID = hexID(2)
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
			testassert.NoError(t, err)
			withUsage := setKey(raw, "usage", json.RawMessage(`{"input_tokens":2,"cached_input_tokens":0,"output_tokens":0}`))
			fixture.entries = []testEntry{
				{env: Entry{SessionID: testSessionID, ID: hexID(1), Kind: EntryAssistant, Sequence: 1, CommittedAt: testTime}, rawOverride: withUsage},
			}
		}},
		{"hook result before its publishing assistant", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(4)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(4),
			}}
			hook := hookResultEntry{SessionID: testSessionID, EntryID: hexID(3), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookSucceeded, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Arguments: json.RawMessage(`{}`)}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 2, CommittedAt: testTime}, hookResult: &hook,
			})
		}},
		{"hook result after its call's terminal tool result", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(3),
			}}
			result := validToolResultEntry(testOpID)
			result.EntryID = hexID(3)
			result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime}, toolResult: &result,
			})
			hook := hookResultEntry{SessionID: testSessionID, EntryID: hexID(5), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookSucceeded, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Arguments: json.RawMessage(`{}`)}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 5, CommittedAt: testTime}, hookResult: &hook,
			})
		}},
		{"duplicate hook result for one call", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(4)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(4),
			}}
			first := hookResultEntry{SessionID: testSessionID, EntryID: hexID(3), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookSucceeded, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Arguments: json.RawMessage(`{}`)}
			second := hookResultEntry{SessionID: testSessionID, EntryID: hexID(5), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookSucceeded, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Arguments: json.RawMessage(`{}`)}
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &first},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 5, CommittedAt: testTime}, hookResult: &second},
			)
		}},
		{"active hook effect repeats a settled hook", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(3),
			}}
			hook := hookResultEntry{SessionID: testSessionID, EntryID: hexID(4), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookSucceeded, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Arguments: json.RawMessage(`{}`)}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryHookResult, Sequence: 4, CommittedAt: testTime}, hookResult: &hook,
			})
			fixture.ops[0].State.ActiveEffect = &ActiveEffect{
				Kind:                 EffectHook,
				ResultEntryID:        hexID(9),
				ToolCallID:           "call-1",
				HookID:               "cap.hook.one",
				ExtensionID:          "ext.one", // the same full HookIdentity as the settled hook result
				InputArgumentsBase64: base64.StdEncoding.EncodeToString([]byte(`{"x":1}`)),
			}
		}},
		{"hook result answers unpublished call", func(t *testing.T, fixture *testGraph) {
			hook := hookResultEntry{SessionID: testSessionID, EntryID: hexID(3), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-9", Status: hookSucceeded, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Arguments: json.RawMessage(`{}`)}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &hook,
			})
		}},
		{"hook for a later call while an earlier call is unresolved", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			callOne, callTwo := validToolCallRecord(), validToolCallRecord()
			callOne.ID, callTwo.ID = "call-1", "call-2"
			callOne.Ordinal, callTwo.Ordinal = 0, 1
			callOne.ResultEntryID, callTwo.ResultEntryID = hexID(4), hexID(5)
			assistant.ToolCalls = []toolCallRecord{callOne, callTwo}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{
				{AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)}, CallID: "call-1", ResultEntryID: hexID(4)},
				{AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)}, CallID: "call-2", ResultEntryID: hexID(5)},
			}
			later := hookResultEntry{SessionID: testSessionID, EntryID: hexID(3), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-2", Status: hookSucceeded, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Arguments: json.RawMessage(`{}`)}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &later,
			})
		}},
		{"hook for a later call before the earlier call's result", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			callOne, callTwo := validToolCallRecord(), validToolCallRecord()
			callOne.ID, callTwo.ID = "call-1", "call-2"
			callOne.Ordinal, callTwo.Ordinal = 0, 1
			callOne.ResultEntryID, callTwo.ResultEntryID = hexID(4), hexID(5)
			assistant.ToolCalls = []toolCallRecord{callOne, callTwo}
			fixture.entries[1].assistant = &assistant
			// The first call's result is committed only after the second
			// call's hook — the pending list is consistent, so the historical
			// first-pending ordering is the violated invariant.
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{
				{AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)}, CallID: "call-2", ResultEntryID: hexID(5)},
			}
			later := hookResultEntry{SessionID: testSessionID, EntryID: hexID(3), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-2", Status: hookSucceeded, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Arguments: json.RawMessage(`{}`)}
			lateResult := validToolResultEntry(testOpID)
			lateResult.EntryID = hexID(4)
			lateResult.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &later},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryToolResult, Sequence: 6, CommittedAt: testTime}, toolResult: &lateResult},
			)
		}},
		{"hook active effect reserves a committed entry", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(3),
			}}
			fixture.ops[0].State.ActiveEffect = toolBoundHookEffect(hexID(1), "cap.hook.one")
		}},
		{"hook active effect reserves a tool call reservation", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(3),
			}}
			fixture.ops[0].State.ActiveEffect = toolBoundHookEffect(hexID(3), "cap.hook.one")
		}},
		{"hook result after a failed hook for the same call", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(4)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(4),
			}}
			failed := hookResultEntry{SessionID: testSessionID, EntryID: hexID(3), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookFailed, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Error: "hook broke"}
			later := hookResultEntry{SessionID: testSessionID, EntryID: hexID(5), OperationID: testOpID, HookID: "cap.hook.two", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookSucceeded, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Arguments: json.RawMessage(`{}`)}
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &failed},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 5, CommittedAt: testTime}, hookResult: &later},
			)
		}},
		{"active hook effect after a failed hook", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(3),
			}}
			failed := hookResultEntry{SessionID: testSessionID, EntryID: hexID(4), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookFailed, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Error: "hook broke"}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryHookResult, Sequence: 4, CommittedAt: testTime}, hookResult: &failed,
			})
			fixture.ops[0].State.ActiveEffect = toolBoundHookEffect(hexID(9), "cap.hook.two")
		}},
		{"active tool effect after a failed hook", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(3),
			}}
			failed := hookResultEntry{SessionID: testSessionID, EntryID: hexID(4), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookFailed, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Error: "hook broke"}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryHookResult, Sequence: 4, CommittedAt: testTime}, hookResult: &failed,
			})
			fixture.ops[0].State.ActiveEffect = &ActiveEffect{Kind: EffectTool, ResultEntryID: hexID(3), ToolCallID: "call-1", FinalArguments: json.RawMessage(`{"x":1}`)}
		}},
		{"interrupted hook with an error tool result", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			hook := hookResultEntry{SessionID: testSessionID, EntryID: hexID(4), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookInterrupted, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Error: "agent interrupted"}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryHookResult, Sequence: 4, CommittedAt: testTime}, hookResult: &hook,
			})
			result := validToolResultEntry(testOpID)
			result.EntryID = hexID(3)
			result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			result.Status = model.ResultError
			result.Content = "hook broke"
			result.OriginalContent = "hook broke"
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 5, CommittedAt: testTime}, toolResult: &result,
			})
		}},
		{"error hook with a success tool result", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			failed := hookResultEntry{SessionID: testSessionID, EntryID: hexID(4), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookFailed, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Error: "hook broke"}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryHookResult, Sequence: 4, CommittedAt: testTime}, hookResult: &failed,
			})
			result := validToolResultEntry(testOpID)
			result.EntryID = hexID(3)
			result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 5, CommittedAt: testTime}, toolResult: &result,
			})
		}},
		{"assistant evidence names its own entry", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			assistant.Evidence.BoundaryEntryID = hexID(2)
			fixture.entries[1].assistant = &assistant
		}},
		{"assistant evidence names a hook result as its boundary", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(4)
			assistant.ToolCalls = []toolCallRecord{call}
			assistant.Evidence.BoundaryEntryID = hexID(3)
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(4),
			}}
			hook := hookResultEntry{SessionID: testSessionID, EntryID: hexID(3), OperationID: testOpID, HookID: "cap.hook.one", ExtensionID: "ext.one", ToolCallID: "call-1", Status: hookSucceeded, Evidence: ExecutionObserved, InputArgumentsBase64: "e30=", Arguments: json.RawMessage(`{}`)}
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &hook,
			})
		}},
		{"assistant evidence names a settlement entry as its boundary", func(t *testing.T, fixture *testGraph) {
			// The settlement belongs to a coherently settled second Operation;
			// the corruption is exclusively the reference kind.
			assistant := fixture.entries[1].assistant
			assistant.Evidence.BoundaryEntryID = hexID(7)
			secondInput := validInputEntry("op-2")
			secondInput.EntryID = hexID(5)
			stamped := testTime
			second := validOperationRecord()
			second.Admission.OperationID = "op-2"
			second.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(5)}
			second.State.Status = OperationSuccess
			second.State.SettledAt = &stamped
			second.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(7)}}
			fixture.session.State.CurrentOperationID = ""
			fixture.ops[0].State.Status = OperationSuccess
			fixture.ops[0].State.SettledAt = &stamped
			fixture.ops[0].State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(6)}}
			own := validSettlementEntry()
			own.EntryID = hexID(6)
			own.OperationID = testOpID
			settlement := validSettlementEntry()
			settlement.EntryID = hexID(7)
			settlement.OperationID = "op-2"
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: "op-2", Kind: EntryInput, Sequence: 3, CommittedAt: testTime}, input: &secondInput},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 4, CommittedAt: testTime}, settlement: &own},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(7), OperationID: "op-2", Kind: EntryOperationSettlement, Sequence: 5, CommittedAt: testTime}, settlement: &settlement},
			)
			fixture.ops = append(fixture.ops, second)
		}},
		{"assistant evidence names a later entry as its boundary", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			assistant.Evidence.BoundaryEntryID = hexID(3) // a projectable entry committed after the assistant
			fixture.entries[1].assistant = &assistant
			signal := validSignalEntry(testOpID)
			signal.EntryID = hexID(3)
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntrySignal, Sequence: 3, CommittedAt: testTime}, signal: &signal,
			})
		}},
		{"assistant evidence names a foreign boundary entry", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			assistant.Evidence.BoundaryEntryID = hexID(7)
			fixture.entries[1].assistant = &assistant
		}},
		{"assistant evidence checkpoint names a non-compaction entry", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			assistant.Evidence.CheckpointEntryID = hexID(1)
			fixture.entries[1].assistant = &assistant
		}},
		{"active effect evidence references outside the Session", func(t *testing.T, fixture *testGraph) {
			effect, request := conversationModelEffect(hexID(3))
			effect.Evidence.BoundaryEntryID = hexID(7)
			request.BoundaryEntryID = hexID(7)
			fixture.ops[0].State.ActiveEffect = effect
			fixture.ops[0].State.Request = request
		}},
		{"maintenance producer settles on two checkpoint entries", func(t *testing.T, fixture *testGraph) {
			one := validCompactionEntry(testOpID)
			one.EntryID = hexID(3)
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryCompaction, Sequence: 3, CommittedAt: testTime}, compaction: &one,
			})
			two := validCompactionEntry(testOpID)
			two.EntryID = hexID(4)
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryCompaction, Sequence: 4, CommittedAt: testTime}, compaction: &two,
			})
			fixture.session.State.CheckpointEntryID = hexID(4)
		}},
		{"assistant provenance omits a committed request hook", func(t *testing.T, fixture *testGraph) {
			hook := validRequestBoundHookResult(hookSucceeded)
			hook.EntryID = hexID(3)
			hook.RequestID = "request-one"
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = "request-one"
			assistantRow := fixture.entries[1]
			assistantRow.assistant = &assistant
			assistantRow.env.Sequence = 3
			fixture.entries = []testEntry{
				fixture.entries[0],
				{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 2, CommittedAt: testTime}, hookResult: &hook},
				assistantRow,
			}
		}},
		{"assistant provenance names committed request hooks out of order", func(t *testing.T, fixture *testGraph) {
			first := validRequestBoundHookResult(hookSucceeded)
			first.EntryID = hexID(3)
			first.RequestID = "request-one"
			second := validRequestBoundHookResult(hookSucceeded)
			second.EntryID = hexID(4)
			second.RequestID = "request-one"
			second.HookID = "cap.hook.two"
			second.InputEvidence = &RequestEvidence{BoundaryEntryID: hexID(1), HookIDs: []HookIdentity{{ExtensionID: "ext.one", HookID: "cap.hook.one"}}}
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = "request-one"
			assistant.Evidence.HookIDs = []HookIdentity{{ExtensionID: "ext.one", HookID: "cap.hook.two"}, {ExtensionID: "ext.one", HookID: "cap.hook.one"}}
			assistantRow := fixture.entries[1]
			assistantRow.assistant = &assistant
			assistantRow.env.Sequence = 4
			fixture.entries = []testEntry{
				fixture.entries[0],
				{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 2, CommittedAt: testTime}, hookResult: &first},
				{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &second},
				assistantRow,
			}
		}},
		{"active request hook names itself in its consumed provenance", func(t *testing.T, fixture *testGraph) {
			fixture.ops[0].State.ActiveEffect = &ActiveEffect{
				Kind: EffectHook, ResultEntryID: hexID(9), HookID: "cap.hook.two", ExtensionID: "ext.two", RequestID: "request-active",
				Evidence: &RequestEvidence{BoundaryEntryID: hexID(1), HookIDs: []HookIdentity{{ExtensionID: "ext.two", HookID: "cap.hook.two"}}},
			}
			fixture.ops[0].State.Request = &RunningRequest{RequestID: "request-active", BoundaryEntryID: hexID(1)}
		}},
		{"active request hook provenance omits a committed request hook", func(t *testing.T, fixture *testGraph) {
			hook := validRequestBoundHookResult(hookSucceeded)
			hook.EntryID = hexID(3)
			hook.RequestID = "request-one"
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 2, CommittedAt: testTime}, hookResult: &hook,
			})
			fixture.ops[0].State.ActiveEffect = &ActiveEffect{
				Kind: EffectHook, ResultEntryID: hexID(9), HookID: "cap.hook.two", ExtensionID: "ext.two", RequestID: "request-one",
				Evidence: &RequestEvidence{BoundaryEntryID: hexID(1)},
			}
			fixture.ops[0].State.Request = &RunningRequest{RequestID: "request-one", BoundaryEntryID: hexID(1), HookResults: []EntryRef{{SessionID: testSessionID, EntryID: hexID(3)}}}
		}},
		{"running request hook results reference committed hooks out of order", func(t *testing.T, fixture *testGraph) {
			first := validRequestBoundHookResult(hookSucceeded)
			first.EntryID = hexID(3)
			first.RequestID = "request-one"
			second := validRequestBoundHookResult(hookSucceeded)
			second.EntryID = hexID(4)
			second.RequestID = "request-one"
			second.HookID = "cap.hook.two"
			second.InputEvidence = &RequestEvidence{BoundaryEntryID: hexID(1), HookIDs: []HookIdentity{{ExtensionID: "ext.one", HookID: "cap.hook.one"}}}
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryHookResult, Sequence: 2, CommittedAt: testTime}, hookResult: &first},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &second},
			)
			fixture.ops[0].State.ActiveEffect = &ActiveEffect{
				Kind: EffectHook, ResultEntryID: hexID(9), HookID: "cap.hook.three", ExtensionID: "ext.three", RequestID: "request-one",
				Evidence: &RequestEvidence{BoundaryEntryID: hexID(1), HookIDs: []HookIdentity{{ExtensionID: "ext.one", HookID: "cap.hook.one"}, {ExtensionID: "ext.one", HookID: "cap.hook.two"}}},
			}
			fixture.ops[0].State.Request = &RunningRequest{RequestID: "request-one", BoundaryEntryID: hexID(1), HookResults: []EntryRef{{SessionID: testSessionID, EntryID: hexID(4)}, {SessionID: testSessionID, EntryID: hexID(3)}}}
		}},
		{"settled request evidence names a hook result committed later", func(t *testing.T, fixture *testGraph) {
			stamped := testTime
			hook := validRequestBoundHookResult(hookSucceeded)
			hook.EntryID = hexID(4)
			hook.RequestID = "request-one"
			settlement := validSettlementEntry()
			settlement.EntryID = hexID(3)
			settlement.OperationID = testOpID
			settlement.Status = OperationSuccess
			settlement.RequestID = "request-one"
			settlement.Evidence = &RequestEvidence{BoundaryEntryID: hexID(1), HookIDs: []HookIdentity{{ExtensionID: "ext.one", HookID: "cap.hook.one"}}}
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 4, CommittedAt: testTime}, settlement: &settlement},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryHookResult, Sequence: 5, CommittedAt: testTime}, hookResult: &hook},
			)
			fixture.ops[0].State.Status = OperationSuccess
			fixture.ops[0].State.SettledAt = &stamped
			fixture.ops[0].State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
			fixture.session.State.CurrentOperationID = "" // a settled Operation is no longer the Session's current one
		}},
		{"tool result provenance omits a committed successful hook effect", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			hook := validHookResultEntry(hookSucceeded)
			hook.EntryID = hexID(5)
			result := validToolResultEntry(testOpID)
			result.EntryID = hexID(3)
			result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			result.NormalizedArguments = json.RawMessage(`{"x":1}`)
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &hook},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime}, toolResult: &result},
			)
		}},
		{"tool result provenance names its hook effects out of order", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			first := validHookResultEntry(hookSucceeded)
			first.EntryID = hexID(5)
			second := validHookResultEntry(hookSucceeded)
			second.EntryID = hexID(6)
			second.HookID = "cap.hook.two"
			result := validToolResultEntry(testOpID)
			result.EntryID = hexID(3)
			result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			result.NormalizedArguments = json.RawMessage(`{"x":1}`)
			result.HookIDs = []HookIdentity{{ExtensionID: "ext.one", HookID: "cap.hook.two"}, {ExtensionID: "ext.one", HookID: "cap.hook.one"}}
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &first},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntryHookResult, Sequence: 4, CommittedAt: testTime}, hookResult: &second},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 5, CommittedAt: testTime}, toolResult: &result},
			)
		}},
		{"active tool effect provenance omits committed hook effects", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(3),
			}}
			hook := validHookResultEntry(hookSucceeded)
			hook.EntryID = hexID(5)
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &hook,
			})
			fixture.ops[0].State.ActiveEffect = &ActiveEffect{
				Kind: EffectTool, ResultEntryID: hexID(3), ToolCallID: "call-1",
				FinalArguments: json.RawMessage(`{"x":1}`),
			}
		}},
		{"tool result provenance names a hook effect committed after the result", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			result := validToolResultEntry(testOpID)
			result.EntryID = hexID(3)
			result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
			result.NormalizedArguments = json.RawMessage(`{"x":1}`)
			result.HookIDs = []HookIdentity{{ExtensionID: "ext.one", HookID: "cap.hook.one"}}
			hook := validHookResultEntry(hookSucceeded)
			hook.EntryID = hexID(5)
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime}, toolResult: &result},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 5, CommittedAt: testTime}, hookResult: &hook},
			)
		}},
		{"active tool effect provenance names its hook effects out of order", func(t *testing.T, fixture *testGraph) {
			assistant := validAssistantEntry(testOpID)
			assistant.EntryID = hexID(2)
			assistant.RequestID = hexID(2)
			call := validToolCallRecord()
			call.ResultEntryID = hexID(3)
			assistant.ToolCalls = []toolCallRecord{call}
			fixture.entries[1].assistant = &assistant
			fixture.ops[0].State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(3),
			}}
			first := validHookResultEntry(hookSucceeded)
			first.EntryID = hexID(5)
			second := validHookResultEntry(hookSucceeded)
			second.EntryID = hexID(6)
			second.HookID = "cap.hook.two"
			fixture.entries = append(fixture.entries,
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 3, CommittedAt: testTime}, hookResult: &first},
				testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntryHookResult, Sequence: 4, CommittedAt: testTime}, hookResult: &second},
			)
			fixture.ops[0].State.ActiveEffect = &ActiveEffect{
				Kind: EffectTool, ResultEntryID: hexID(3), ToolCallID: "call-1",
				FinalArguments:  json.RawMessage(`{"x":1}`),
				ContributionIDs: []HookIdentity{{ExtensionID: "ext.one", HookID: "cap.hook.two"}, {ExtensionID: "ext.one", HookID: "cap.hook.one"}},
			}
		}},
		{"running request state names a boundary its checkpoint covered", func(t *testing.T, fixture *testGraph) {
			comp := validCompactionEntry(testOpID)
			comp.EntryID = hexID(5)
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryCompaction, Sequence: 3, CommittedAt: testTime}, compaction: &comp,
			})
			fixture.session.State.CheckpointEntryID = hexID(5)
			fixture.ops[0].State.Request = &RunningRequest{RequestID: "request-active", CheckpointEntryID: hexID(5), BoundaryEntryID: hexID(1)}
		}},
		{"active request hook effect references a boundary its checkpoint covered", func(t *testing.T, fixture *testGraph) {
			comp := validCompactionEntry(testOpID)
			comp.EntryID = hexID(5)
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryCompaction, Sequence: 3, CommittedAt: testTime}, compaction: &comp,
			})
			fixture.session.State.CheckpointEntryID = hexID(5)
			fixture.ops[0].State.ActiveEffect = &ActiveEffect{
				Kind: EffectHook, ResultEntryID: hexID(9), HookID: "cap.hook.one", ExtensionID: "ext.one", RequestID: "request-active",
				Evidence: &RequestEvidence{CheckpointEntryID: hexID(5), BoundaryEntryID: hexID(1)},
			}
			fixture.ops[0].State.Request = &RunningRequest{RequestID: "request-active", CheckpointEntryID: hexID(5), BoundaryEntryID: hexID(1)}
		}},
		{"automatic maintenance record names no consumed conversation request", func(t *testing.T, fixture *testGraph) {
			comp := validCompactionEntry(testOpID)
			comp.EntryID = hexID(3)
			comp.ConsumedRequestID = ""
			fixture.entries = append(fixture.entries, testEntry{
				env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryCompaction, Sequence: 3, CommittedAt: testTime}, compaction: &comp,
			})
		}},
	}
	// The input-ownership rows share ONE generic typed-corruption diagnostic:
	// any per-case wording is a regression this rejects.
	ownershipDetail := fmt.Sprintf("input entry %s is not its owning operation's admitted input", hexID(4))
	ownership := map[string]bool{
		"extra owned input under a running message operation":  true,
		"extra owned input under a terminal message operation": true,
		"input owned by a compact operation":                   true,
	}
	// The usage-overflow row pins the preserved overflow diagnostic: the
	// Operation-total overflow check surfaces before the Session-total one at
	// the same iteration position, in the storage-failure class.
	usageOverflowDetail := map[string]string{
		"usage accumulation overflows": fmt.Sprintf("usage of operation %q overflows: harness: storage failure: input token count overflows int64", testOpID),
	}
	// The evidence-reference rows pin the exact reference rule each mutation
	// targets, so an incidental earlier corruption is a regression.
	evidenceDetail := map[string]string{
		"assistant evidence names its own entry":                                  fmt.Sprintf("references boundary entry %s at or after its own sequence", hexID(2)),
		"assistant evidence names a hook result as its boundary":                  "of non-canonical kind hook_result",
		"assistant evidence names a settlement entry as its boundary":             "of non-canonical kind operation_settlement",
		"assistant evidence names a later entry as its boundary":                  "at or after its own sequence",
		"assistant evidence names a foreign boundary entry":                       "outside the Session",
		"assistant evidence checkpoint names a non-compaction entry":              "names no in-session compaction entry",
		"active effect evidence references outside the Session":                   "outside the Session",
		"maintenance producer settles on two checkpoint entries":                  "settles maintenance producer",
		"assistant provenance omits a committed request hook":                     "omits committed hook result",
		"assistant provenance names committed request hooks out of order":         "out of order",
		"active request hook names itself in its consumed provenance":             "names itself in its consumed provenance",
		"active request hook provenance omits a committed request hook":           "omits committed hook result",
		"running request hook results reference committed hooks out of order":     "out of committed entry order",
		"settled request evidence names a hook result committed later":            "at or after its own sequence",
		"tool result provenance omits a committed successful hook effect":         "omits committed hook result",
		"tool result provenance names its hook effects out of order":              "out of order",
		"active tool effect provenance omits committed hook effects":              "omits committed hook result",
		"tool result provenance names a hook effect committed after the result":   "follows that call's terminal tool result",
		"active tool effect provenance names its hook effects out of order":       "out of order",
		"running request state names a boundary its checkpoint covered":           "already covered by checkpoint entry",
		"active request hook effect references a boundary its checkpoint covered": "already covered by checkpoint entry",
		"automatic maintenance record names no consumed conversation request":     "names no consumed conversation request",
	}
	for _, tc := range corruptions {
		t.Run(tc.name, func(t *testing.T) {
			fixture := validTestGraph()
			tc.mutate(t, fixture)
			store := fixture.storage(t)
			corrupt := wantCorruption(t, func() error {
				_, err := validateFixture(t, store, testSessionID)
				return err
			}())
			testassert.Require(t, !ownership[tc.name] || corrupt.Detail == ownershipDetail, "ownership corruption detail = %q, want the one generic %q", corrupt.Detail, ownershipDetail)
			{
				want, ok := usageOverflowDetail[tc.name]
				testassert.Require(t, !ok || corrupt.Detail == want, "overflow corruption detail = %q, want %q", corrupt.Detail, want)
			}
			{
				want, ok := evidenceDetail[tc.name]
				testassert.Require(t, !ok || strings.Contains(corrupt.Detail, want), "evidence corruption detail = %q, want the %q rule", corrupt.Detail, want)
			}
		})
	}

	t.Run("one corrupt record among valid siblings", func(t *testing.T) {
		broken := validTestGraph()
		raw, err := encodeInputEntry(*broken.entries[0].input)
		testassert.NoError(t, err)
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
		{
			_, err := validateFixture(t, store, siblingID)
			testassert.NoError(t, err)
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
		testassert.Require(t, err != nil && errors.Is(err, ErrNotFound), "absent session = %v, want the ErrNotFound class", err)
		var corrupt *CorruptionError
		testassert.Require(t, !errors.As(err, &corrupt), "absent session misclassified as corruption: %v", err)
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
		testassert.Require(t, errors.Is(err, failure) && err == failure, "registers failure = %v, want the exact error unmodified", err)
		wantNotCorruption(t, err)
	})
	t.Run("read entries failure", func(t *testing.T) {
		store := fixture.storage(t)
		store.entriesErr = failure
		_, err := validateFixture(t, store, testSessionID)
		testassert.Equal(t, err, failure)
		wantNotCorruption(t, err)
	})
	t.Run("addressed-but-absent record", func(t *testing.T) {
		store := fixture.storage(t)
		store.registersErr = fmt.Errorf("session row: %w", ErrNotFound)
		_, err := validateFixture(t, store, testSessionID)
		testassert.Require(t, errors.Is(err, ErrNotFound), "absent record = %v, want the landed not-found class unchanged", err)
		wantNotCorruption(t, err)
	})
	t.Run("malformed session identity is invalid input", func(t *testing.T) {
		_, err := validateFixture(t, fixture.storage(t), "not-an-id")
		testassert.Require(t, errors.Is(err, ErrInvalid), "malformed session id = %v, want ErrInvalid", err)
		wantNotCorruption(t, err)
	})
}

func wantNotCorruption(t *testing.T, err error) {
	t.Helper()
	var corrupt *CorruptionError
	testassert.Require(t, !errors.As(err, &corrupt), "storage failure reclassified as corruption: %v", err)
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
		copiedAssistant.RequestID = hexID(2)
		copiedSignal := validSignalEntry("")
		copiedSignal.EntryID = hexID(3)
		copiedSignal.RelatedOperation = &operationRef{SessionID: otherSession(), OperationID: "source-op"}
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
		copiedAssistant.RequestID = hexID(2)
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
		{
			_, err := validateFixture(t, fork.storage(t), testSessionID)
			testassert.NoError(t, err)
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
		_, err := validateFixture(t, fork.storage(t), testSessionID)
		wantCorruption(t, err)
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
		_, err := validateFixture(t, fork.storage(t), testSessionID)
		wantCorruption(t, err)
	})

	t.Run("owned signal resolves its related operation", func(t *testing.T) {
		fixture := validTestGraph()
		owned := validSignalEntry(testOpID)
		owned.EntryID = hexID(3)
		fixture.entries = append(fixture.entries, testEntry{
			env:    Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntrySignal, Sequence: 3, CommittedAt: testTime},
			signal: &owned,
		})
		{
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("owned signal with unresolvable operation", func(t *testing.T) {
		fixture := validTestGraph()
		owned := validSignalEntry(testOpID)
		owned.EntryID = hexID(3)
		owned.RelatedOperation = &operationRef{SessionID: testSessionID, OperationID: "ghost"}
		fixture.entries = append(fixture.entries, testEntry{
			env:    Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntrySignal, Sequence: 3, CommittedAt: testTime},
			signal: &owned,
		})
		_, err := validateFixture(t, fixture.storage(t), testSessionID)
		wantCorruption(t, err)
	})
}

// TestGraphValidatorBackgroundCompletionOwnership proves the fork-prefix
// ownership exception: every non-fork Session — root or child — may carry
// operationless background_completion entries, in a fork such entries are
// exempt from prefix ordering, and every other operationless entry keeps the
// existing rules.
func TestGraphValidatorBackgroundCompletionOwnership(t *testing.T) {
	completion := func(entryID string, sequence int64) testEntry {
		sig := validBackgroundCompletionEntry()
		sig.EntryID = entryID
		return testEntry{env: Entry{SessionID: testSessionID, ID: entryID, Kind: EntrySignal, Sequence: sequence, CommittedAt: testTime}, signal: &sig}
	}

	t.Run("root with operationless completion validates", func(t *testing.T) {
		fixture := &testGraph{session: validSessionRecord()}
		fixture.entries = []testEntry{completion(hexID(9), 1)}
		{
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("child with operationless completion validates", func(t *testing.T) {
		fixture := &testGraph{session: validSessionRecord()}
		fixture.session.Identity.ParentSessionID = otherSession()
		fixture.entries = []testEntry{completion(hexID(9), 1)}
		{
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("fork completion exempt from prefix ordering", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.session.Identity.SourceSessionID = otherSession()
		fixture.session.Identity.SourceBoundaryEntryID = otherEntry()
		fixture.entries = append(fixture.entries, completion(hexID(9), 3))
		{
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("operationless fixed-kind signal on a child corrupts", func(t *testing.T) {
		fixture := &testGraph{session: validSessionRecord()}
		fixture.session.Identity.ParentSessionID = otherSession()
		copied := validSignalEntry("")
		copied.EntryID = hexID(9)
		fixture.entries = []testEntry{
			{env: Entry{SessionID: testSessionID, ID: hexID(9), Kind: EntrySignal, Sequence: 1, CommittedAt: testTime}, signal: &copied},
		}
		_, err := validateFixture(t, fixture.storage(t), testSessionID)
		corrupt := wantCorruption(t, err)
		testassert.Require(t, strings.Contains(corrupt.Detail, "root Session carries independently copied entry"), "corruption detail = %q, want the copied-entry rule", corrupt.Detail)
	})
}

// TestGraphViewCarriesEnvelopeMetadata proves the decoded view carries the
// envelope fields (revision, sequence, commit time) alongside typed payloads.
func TestGraphViewCarriesEnvelopeMetadata(t *testing.T) {
	fixture := validTestGraph()
	fixture.session.Revision = 42
	fixture.ops[0].Revision = 7
	view, err := validateFixture(t, fixture.storage(t), testSessionID)
	testassert.NoError(t, err)
	testassert.Equal(t, view.Session.Revision, 42)
	testassert.Equal(t, view.Operations[0].Revision, 7)
	testassert.Require(t, view.Entries[0].Envelope.CommittedAt.Equal(testTime) && view.Entries[0].Envelope.Sequence == 1, "view entry envelope = %+v", view.Entries[0].Envelope)
}

// Compile-time proof the fixture storage implements the landed read side.
var _ Storage = (*graphStorage)(nil)

// TestGraphValidatorCompactionRules proves the compaction-entry validation
// rules: a committed compaction entry with the projection state validates,
// dangling projections and boundary references corrupt, no per-Operation
// compaction count exists, copied prefix entries validate on a fork while a
// root Session rejects them, and the decoded view carries the typed payload.
func TestGraphValidatorCompactionRules(t *testing.T) {
	compactionAt := func(entryID, boundaryID, operationID string, sequence int64) testEntry {
		v := validCompactionEntry(operationID)
		v.EntryID = entryID
		v.Facts[0].RequestID = "maintenance-" + entryID
		v.BoundaryEntryID = boundaryID
		return testEntry{
			env:        Entry{SessionID: testSessionID, ID: entryID, OperationID: operationID, Kind: EntryCompaction, Sequence: sequence, CommittedAt: testTime},
			compaction: &v,
		}
	}

	t.Run("committed compaction entry with projection state validates", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.entries = append(fixture.entries, compactionAt(hexID(3), hexID(2), testOpID, 3))
		fixture.session.State.CheckpointEntryID = hexID(3)
		view, err := validateFixture(t, fixture.storage(t), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, len(view.Entries) == 3 && view.Entries[2].Compaction != nil, "decoded view does not carry the compaction payload")
		if view.Entries[2].Compaction.BoundaryEntryID != hexID(2) {
			t.Fatalf("decoded boundary %q, want %q", view.Entries[2].Compaction.BoundaryEntryID, hexID(2))
		}
	})

	t.Run("dangling projection reference corrupts", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.session.State.CheckpointEntryID = otherEntry()
		_, err := validateFixture(t, fixture.storage(t), testSessionID)
		corrupt := wantCorruption(t, err)
		testassert.Require(t, strings.Contains(corrupt.Detail, "checkpoint"), "corruption detail = %q, want the checkpoint reference rule", corrupt.Detail)
	})

	t.Run("projection naming a non-compaction entry corrupts", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.session.State.CheckpointEntryID = hexID(2)
		_, err := validateFixture(t, fixture.storage(t), testSessionID)
		wantCorruption(t, err)
	})

	t.Run("boundary naming no in-session entry corrupts", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.entries = append(fixture.entries, compactionAt(hexID(3), otherEntry(), testOpID, 3))
		_, err := validateFixture(t, fixture.storage(t), testSessionID)
		wantCorruption(t, err)
	})

	t.Run("boundary at the compaction sequence corrupts", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.entries = append(fixture.entries, compactionAt(hexID(3), hexID(3), testOpID, 3))
		_, err := validateFixture(t, fixture.storage(t), testSessionID)
		wantCorruption(t, err)
	})

	t.Run("boundary after the compaction sequence corrupts", func(t *testing.T) {
		fixture := validTestGraph()
		signal := validSignalEntry(testOpID)
		signal.EntryID = hexID(4)
		fixture.entries = append(fixture.entries,
			compactionAt(hexID(3), hexID(4), testOpID, 3),
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntrySignal, Sequence: 4, CommittedAt: testTime}, signal: &signal},
		)
		_, err := validateFixture(t, fixture.storage(t), testSessionID)
		wantCorruption(t, err)
	})

	t.Run("two compaction entries of one operation validate", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.entries = append(fixture.entries,
			compactionAt(hexID(3), hexID(2), testOpID, 3),
			compactionAt(hexID(4), hexID(3), testOpID, 4),
		)
		fixture.session.State.CheckpointEntryID = hexID(4)
		{
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("compaction entries of different operations validate", func(t *testing.T) {
		fixture := &testGraph{
			session: validSessionRecord(),
			ops: func() []OperationRecord {
				first := validOperationRecord()
				stamped := testTime
				first.State.Status = OperationSuccess
				first.State.SettledAt = &stamped
				first.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(4)}}
				second := validOperationRecord()
				second.Admission.OperationID = "op-2"
				second.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(5)}
				return []OperationRecord{first, second}
			}(),
		}
		fixture.session.State.CurrentOperationID = "op-2"
		fixture.session.State.CheckpointEntryID = hexID(6)
		input := validInputEntry(testOpID)
		input.EntryID = hexID(1)
		assistant := validAssistantEntry(testOpID)
		assistant.EntryID = hexID(2)
		assistant.RequestID = hexID(2)
		settlement := validSettlementEntry()
		settlement.EntryID = hexID(4)
		secondInput := validInputEntry("op-2")
		secondInput.EntryID = hexID(5)
		fixture.entries = []testEntry{
			{env: Entry{SessionID: testSessionID, ID: hexID(1), OperationID: testOpID, Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &input},
			{env: Entry{SessionID: testSessionID, ID: hexID(2), OperationID: testOpID, Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &assistant},
			compactionAt(hexID(3), hexID(2), testOpID, 3),
			{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 4, CommittedAt: testTime}, settlement: &settlement},
			{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: "op-2", Kind: EntryInput, Sequence: 5, CommittedAt: testTime}, input: &secondInput},
			compactionAt(hexID(6), hexID(5), "op-2", 6),
		}
		{
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("copied compaction entry validates in a fork prefix", func(t *testing.T) {
		fixture := &testGraph{session: validSessionRecord()}
		fixture.session.Identity.SourceSessionID = otherSession()
		fixture.session.Identity.SourceBoundaryEntryID = otherEntry()
		copiedAssistant := validAssistantEntry("")
		copiedAssistant.EntryID = hexID(2)
		copiedAssistant.RequestID = hexID(2)
		fixture.entries = []testEntry{
			{env: Entry{SessionID: testSessionID, ID: hexID(1), Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: func() *inputEntry { v := validInputEntry(""); v.EntryID = hexID(1); return &v }()},
			{env: Entry{SessionID: testSessionID, ID: hexID(2), Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &copiedAssistant},
			compactionAt(hexID(3), hexID(2), "", 3),
		}
		{
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("root session rejects an operationless compaction entry", func(t *testing.T) {
		fixture := &testGraph{session: validSessionRecord()}
		fixture.entries = []testEntry{compactionAt(hexID(3), hexID(2), "", 1)}
		_, err := validateFixture(t, fixture.storage(t), testSessionID)
		corrupt := wantCorruption(t, err)
		testassert.Require(t, strings.Contains(corrupt.Detail, "root Session carries independently copied entry"), "corruption detail = %q, want the copied-entry rule", corrupt.Detail)
	})
}

// TestGraphValidatorCompactionUsage proves the usage accounting: a compaction
// entry's usage joins the recomputed Operation and Session totals, and a
// copied fork-prefix compaction entry carries none.
func TestGraphValidatorCompactionUsage(t *testing.T) {
	otherRef := model.ModelRef{Provider: "other", Model: "m2"}

	cases := []struct {
		name    string
		mutate  func(t *testing.T, fixture *testGraph, compaction *contextCheckpointEntry)
		corrupt bool
	}{
		{
			name: "compaction usage joins the totals",
			mutate: func(t *testing.T, fixture *testGraph, compaction *contextCheckpointEntry) {
				assistantUsage := UsageCount{InputTokens: 1, CachedInputTokens: 2, OutputTokens: 3}
				compactionUsage := UsageCount{InputTokens: 4, CachedInputTokens: 5, OutputTokens: 6}
				assistant := validAssistantEntry(testOpID)
				assistant.EntryID = hexID(2)
				assistant.RequestID = hexID(2)
				assistant.Usage = &assistantUsage
				fixture.entries[1].assistant = &assistant
				compaction.Facts[0].Usage = &compactionUsage
				merged, err := addUsageTotals(
					UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: assistantUsage}}},
					UsageTotals{ByModel: []ModelUsage{{Model: otherRef, Usage: compactionUsage}}},
				)
				testassert.NoError(t, err)
				fixture.ops[0].State.Usage = merged
				fixture.session.State.Usage = merged
			},
		},
		{
			name: "compaction usage missing from the totals corrupts",
			mutate: func(t *testing.T, fixture *testGraph, compaction *contextCheckpointEntry) {
				compaction.Facts[0].Usage = testUsage(5)
			},
			corrupt: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := validTestGraph()
			compaction := validCompactionEntry(testOpID)
			compaction.EntryID = hexID(3)
			compaction.BoundaryEntryID = hexID(2)
			compaction.Facts[0].Model = otherRef
			tc.mutate(t, fixture, &compaction)
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryCompaction, Sequence: 3, CommittedAt: testTime},
				compaction: &compaction,
			})
			_, err := validateFixture(t, fixture.storage(t), testSessionID)
			if tc.corrupt {
				wantCorruption(t, err)
				return
			}
			testassert.NoError(t, err)
		})
	}
}

// TestForkCopyCompactionEntries proves the fork copy of compaction entries:
// fresh identities, cleared operation ownership and usage, the boundary
// rewritten into the copied prefix, the revision kept, the destination
// projection field naming the last copied compaction entry, an empty field
// for a prefix without compaction entries, and an unchanged source.
func TestForkCopyCompactionEntries(t *testing.T) {
	const destID = "ffffffffffffffffffffffffffffffff"
	const forkOpID = "fork-op-1"
	boundaryID := hexID(5)

	buildSource := func(t *testing.T, withCompaction bool) *testGraph {
		t.Helper()
		fixture := validTestGraph()
		if withCompaction {
			first := validCompactionEntry(testOpID)
			first.EntryID = hexID(3)
			first.BoundaryEntryID = hexID(2)
			first.Facts[0].Usage = testUsage(5)
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryCompaction, Sequence: 3, CommittedAt: testTime},
				compaction: &first,
			})
			second := validCompactionEntry(testOpID)
			second.EntryID = hexID(4)
			second.BoundaryEntryID = hexID(3)
			setSummaryMessage(&second, "Second summary.", testModelRef())
			fixture.entries = append(fixture.entries, testEntry{
				env:        Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryCompaction, Sequence: 4, CommittedAt: testTime},
				compaction: &second,
			})
		}
		boundary := validInputEntry(testOpID)
		boundary.EntryID = boundaryID
		fixture.entries = append(fixture.entries, testEntry{
			env:   Entry{SessionID: testSessionID, ID: boundaryID, OperationID: testOpID, Kind: EntryInput, Sequence: 5, CommittedAt: testTime},
			input: &boundary,
		})
		return fixture
	}

	runFork := func(t *testing.T, fixture *testGraph) *forkCommit {
		t.Helper()
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		var commit forkCommit
		err := store.Transact(context.Background(), func(tx Transaction) error {
			return h.forkTransaction(tx, destID, fixture.session, ForkRequest{
				SourceSessionID: testSessionID,
				BoundaryEntryID: boundaryID,
				OperationID:     forkOpID,
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "continue"}},
			}, nil, testCapture(), &commit)
		})
		testassert.NoError(t, err)
		return &commit
	}

	t.Run("copied compaction entries and projection field", func(t *testing.T) {
		commit := runFork(t, buildSource(t, true))

		var copiedAssistantID string
		var copiedCompactions []*contextCheckpointEntry
		var copiedCompactionIDs []string
		for i := range commit.entries {
			entry := commit.entries[i]
			switch {
			case entry.Assistant != nil:
				copiedAssistantID = entry.Envelope.ID
			case entry.Compaction != nil:
				copiedCompactions = append(copiedCompactions, entry.Compaction)
				copiedCompactionIDs = append(copiedCompactionIDs, entry.Envelope.ID)
			}
		}
		testassert.Equal(t, len(copiedCompactions), 2)
		testassert.Require(t, copiedCompactionIDs[0] != hexID(3) && copiedCompactionIDs[1] != hexID(4), "copied compaction identities %v are not both fresh", copiedCompactionIDs)
		for i, v := range copiedCompactions {
			testassert.Require(t, v.SessionID == destID && v.EntryID == copiedCompactionIDs[i], "copied compaction identity %s/%s, want destination identities", v.SessionID, v.EntryID)
			testassert.Require(t, v.ConsumedRequestID == "request-two", "copied compaction %s consumed source = %q, want the source identity carried unchanged and informational", v.EntryID, v.ConsumedRequestID)
			testassert.Equal(t, v.OperationID, "")
			for _, fact := range v.Facts {
				if fact.Usage != nil {
					t.Fatalf("copied compaction carries source usage %+v", *fact.Usage)
				}
			}
			testassert.Equal(t, v.Facts[0].Model, testModelRef())
			testassert.Equal(t, v.ConfigurationRevision, "rev-1")
		}
		testassert.Require(t, summaryText(t, copiedCompactions[0]) == "Summary of the earlier conversation." && summaryText(t, copiedCompactions[1]) == "Second summary.", "copied summaries %q and %q did not survive", summaryText(t, copiedCompactions[0]), summaryText(t, copiedCompactions[1]))
		testassert.Equal(t, copiedCompactions[0].BoundaryEntryID, copiedAssistantID)
		testassert.Equal(t, copiedCompactions[1].BoundaryEntryID, copiedCompactionIDs[0])
		testassert.Equal(t, commit.session.State.CheckpointEntryID, copiedCompactionIDs[1])
	})

	t.Run("copied messages preserve the complete shared domain through cold reads", func(t *testing.T) {
		arguments := []byte{0x7b, 0xff, 0x00, 0x7d} // not valid JSON
		fixture := buildSource(t, true)
		for i := range fixture.entries {
			if fixture.entries[i].compaction == nil || fixture.entries[i].env.ID != hexID(3) {
				continue
			}
			fixture.entries[i].compaction.Messages = []model.Message{
				{
					Role:      model.RoleAssistant,
					Source:    testModelRef(),
					Content:   []model.ContentPart{{Kind: model.PartText, Text: "calling"}},
					Refusal:   "declined",
					Name:      "named",
					Extra:     model.Extra{"vendor": json.RawMessage(`1`)},
					ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: arguments, Extra: model.Extra{"call": json.RawMessage(`"x"`)}}},
				},
				{Role: model.RoleTool, ToolCallID: "call-1", Content: []model.ContentPart{{Kind: model.PartText, Text: "done"}}},
			}
		}
		const destID = "ffffffffffffffffffffffffffffffff"
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		var commit forkCommit
		err := store.Transact(context.Background(), func(tx Transaction) error {
			return h.forkTransaction(tx, destID, fixture.session, ForkRequest{
				SourceSessionID: testSessionID,
				BoundaryEntryID: boundaryID,
				OperationID:     forkOpID,
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "continue"}},
			}, nil, testCapture(), &commit)
		})
		testassert.NoError(t, err)

		var copied *contextCheckpointEntry
		for i := range commit.entries {
			if e := commit.entries[i].Compaction; e != nil && copied == nil {
				copied = e
			}
		}
		testassert.Require(t, copied != nil && len(copied.Messages) == 2, "copied messages = %+v, want the retained tool exchange", copied)
		testassert.Require(t, len(copied.Messages[0].ToolCalls) == 1 && bytes.Equal(copied.Messages[0].ToolCalls[0].Arguments, arguments) && copied.Messages[1].ToolCallID == "call-1",
			"copied tool exchange = %+v, want the exact raw argument bytes and pairing", copied.Messages)
		testassert.Require(t, copied.Messages[0].Refusal == "declined" && copied.Messages[0].Name == "named" && copied.Messages[0].Extra["vendor"] != nil,
			"copied message fields = %+v, want refusal, name and extras preserved", copied.Messages[0])

		// The cold read of the fork's stored payload preserves the same
		// admitted fields byte-for-byte.
		stored, err := store.ReadEntries(context.Background(), destID, 0)
		testassert.NoError(t, err)
		for _, entry := range stored {
			if entry.Kind != EntryCompaction || entry.ID != copied.EntryID {
				continue
			}
			decoded, derr := decodeContextCheckpointEntry(entry)
			testassert.NoError(t, derr)
			testassert.Require(t, reflect.DeepEqual(decoded.Messages, copied.Messages), "cold-read messages = %+v, want the copied values byte-for-byte", decoded.Messages)
		}
	})

	t.Run("prefix without compaction entries yields an empty field", func(t *testing.T) {
		commit := runFork(t, buildSource(t, false))
		for _, entry := range commit.entries {
			testassert.Require(t, entry.Compaction == nil, "prefix without compaction entries copied one")
		}
		testassert.Equal(t, commit.session.State.CheckpointEntryID, "")
	})

	t.Run("source session is unchanged", func(t *testing.T) {
		fixture := buildSource(t, true)
		store := fixture.storage(t)
		before, err := store.ReadEntries(context.Background(), testSessionID, 0)
		testassert.NoError(t, err)
		runFork(t, fixture)
		after, err := store.ReadEntries(context.Background(), testSessionID, 0)
		testassert.NoError(t, err)
		testassert.Equal(t, len(after), len(before))
		for i := range before {
			testassert.Require(t, after[i].ID == before[i].ID && string(after[i].Payload) == string(before[i].Payload), "source entry %d changed", i)
		}
		reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: testSessionID, Kind: RegisterSession})
		testassert.NoError(t, err)
		testassert.Equal(t, reg.Revision, fixture.session.Revision)
	})
}

// TestGraphValidatorGroupsEntriesByOperation pins per-Operation grouping over
// an interleaved committed history: each Operation's publication, result,
// hook, and settlement checks see exactly their own entries in sequence order,
// and one Operation's entries can neither satisfy nor corrupt another's
// checks. The running Operation publishes its two calls from two separate
// assistant entries whose group order must stay sequence order for its pending
// list to agree.
func TestGraphValidatorGroupsEntriesByOperation(t *testing.T) {
	fixture := &testGraph{session: validSessionRecord()}
	fixture.session.State.CurrentOperationID = testOpID
	running := validOperationRecord()
	running.State.PendingToolCalls = []PendingToolCall{
		{AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}, CallID: "a1", ResultEntryID: hexID(9)},
		{AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(6)}, CallID: "a2", ResultEntryID: hexID(10)},
	}
	terminal := validOperationRecord()
	terminal.Admission.OperationID = "op-2"
	terminal.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
	stamped := testTime
	terminal.State.Status = OperationSuccess
	terminal.State.SettledAt = &stamped
	terminal.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(8)}}
	fixture.ops = []OperationRecord{running, terminal}

	inputA := validInputEntry(testOpID)
	inputA.EntryID = hexID(1)
	inputB := validInputEntry("op-2")
	inputB.EntryID = hexID(2)
	assistantA1 := validAssistantEntry(testOpID)
	assistantA1.EntryID = hexID(3)
	assistantA1.RequestID = hexID(3)
	callA1 := validToolCallRecord()
	callA1.ID, callA1.ResultEntryID = "a1", hexID(9)
	assistantA1.ToolCalls = []toolCallRecord{callA1}
	assistantA2 := validAssistantEntry(testOpID)
	assistantA2.EntryID = hexID(6)
	assistantA2.RequestID = hexID(6)
	callA2 := validToolCallRecord()
	callA2.ID, callA2.ResultEntryID = "a2", hexID(10)
	assistantA2.ToolCalls = []toolCallRecord{callA2}
	assistantB := validAssistantEntry("op-2")
	assistantB.EntryID = hexID(4)
	assistantB.RequestID = hexID(4)
	callB := validToolCallRecord()
	callB.ID, callB.ResultEntryID = "b1", hexID(7)
	assistantB.ToolCalls = []toolCallRecord{callB}
	hookA := validHookResultEntry(hookSucceeded)
	hookA.EntryID = hexID(5)
	hookA.ToolCallID = "a1"
	resultB := validToolResultEntry("op-2")
	resultB.EntryID = hexID(7)
	resultB.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(4)}
	resultB.ToolCallID = "b1"
	settlementB := validSettlementEntry()
	settlementB.EntryID = hexID(8)
	settlementB.OperationID = "op-2"

	fixture.entries = []testEntry{
		{env: Entry{SessionID: testSessionID, ID: hexID(1), OperationID: testOpID, Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &inputA},
		{env: Entry{SessionID: testSessionID, ID: hexID(2), OperationID: "op-2", Kind: EntryInput, Sequence: 2, CommittedAt: testTime}, input: &inputB},
		{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryAssistant, Sequence: 3, CommittedAt: testTime}, assistant: &assistantA1},
		{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: "op-2", Kind: EntryAssistant, Sequence: 4, CommittedAt: testTime}, assistant: &assistantB},
		{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryHookResult, Sequence: 5, CommittedAt: testTime}, hookResult: &hookA},
		{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntryAssistant, Sequence: 6, CommittedAt: testTime}, assistant: &assistantA2},
		{env: Entry{SessionID: testSessionID, ID: hexID(7), OperationID: "op-2", Kind: EntryToolResult, Sequence: 7, CommittedAt: testTime}, toolResult: &resultB},
		{env: Entry{SessionID: testSessionID, ID: hexID(8), OperationID: "op-2", Kind: EntryOperationSettlement, Sequence: 8, CommittedAt: testTime}, settlement: &settlementB},
	}
	{
		_, err := validateFixture(t, fixture.storage(t), testSessionID)
		testassert.NoError(t, err)
	}
}

// TestBuildCutSafetyVerdicts pins the precomputed cut-safety table over one
// indexed immutable entry sequence with multiple calls and cuts: a call
// published before a cut with its reserved result beyond it makes that cut
// and every later one unsafe, a missing result keeps all subsequent cuts
// unsafe, and cuts at or after every resolved pair are safe again — verdicts
// over the carried calls, with no timing.
func TestBuildCutSafetyVerdicts(t *testing.T) {
	callA := validAssistantEntry(testOpID)
	callA.EntryID = hexID(2)
	callA.RequestID = hexID(2)
	callA.ToolCalls = []toolCallRecord{validToolCallRecord()}
	callA.ToolCalls[0].ID = "call-a"
	callA.ToolCalls[0].ResultEntryID = hexID(5)
	callB := validAssistantEntry(testOpID)
	callB.EntryID = hexID(3)
	callB.RequestID = hexID(3)
	callB.ToolCalls = []toolCallRecord{validToolCallRecord()}
	callB.ToolCalls[0].ID = "call-b"
	callB.ToolCalls[0].ResultEntryID = hexID(4)
	resultB := validToolResultEntry(testOpID)
	resultB.EntryID = hexID(4)
	resultB.ToolCallID = "call-b"
	resultA := validToolResultEntry(testOpID)
	resultA.EntryID = hexID(5)
	resultA.ToolCallID = "call-a"
	callC := validAssistantEntry(testOpID)
	callC.EntryID = hexID(6)
	callC.RequestID = hexID(6)
	callC.ToolCalls = []toolCallRecord{validToolCallRecord()}
	callC.ToolCalls[0].ID = "call-c"
	callC.ToolCalls[0].ResultEntryID = hexID(9) // never committed

	input := validInputEntry(testOpID)
	input.EntryID = hexID(1)
	cutOne := validCompactionEntry(testOpID)
	cutOne.EntryID = hexID(7)
	cutTwo := validCompactionEntry(testOpID)
	cutTwo.EntryID = hexID(8)
	entries := []graphEntry{
		{Envelope: Entry{ID: hexID(1), Kind: EntryInput, Sequence: 1}, Input: &input},
		{Envelope: Entry{ID: hexID(2), Kind: EntryAssistant, Sequence: 2}, Assistant: &callA},
		{Envelope: Entry{ID: hexID(3), Kind: EntryAssistant, Sequence: 3}, Assistant: &callB},
		{Envelope: Entry{ID: hexID(4), Kind: EntryToolResult, Sequence: 4}, ToolResult: &resultB},
		{Envelope: Entry{ID: hexID(5), Kind: EntryToolResult, Sequence: 5}, ToolResult: &resultA},
		{Envelope: Entry{ID: hexID(6), Kind: EntryAssistant, Sequence: 6}, Assistant: &callC},
		{Envelope: Entry{ID: hexID(7), Kind: EntryCompaction, Sequence: 7}, Compaction: &cutOne},
		{Envelope: Entry{ID: hexID(8), Kind: EntryCompaction, Sequence: 8}, Compaction: &cutTwo},
	}
	unsafe := buildCutSafety(entries)

	// Cuts and their verdicts: (boundary entry, offender; empty = safe).
	rows := []struct{ boundary, offender string }{
		{hexID(1), ""},       // before any call is published
		{hexID(2), "call-a"}, // call-a's result sits beyond the cut
		{hexID(3), "call-a"}, // both published calls resolve beyond the cut
		{hexID(4), "call-a"}, // call-b resolved, call-a still beyond
		{hexID(5), ""},       // every published call resolved at or before
		{hexID(6), "call-c"}, // call-c's result is missing
		{hexID(7), "call-c"}, // the first checkpoint cut stays unsafe
		{hexID(8), "call-c"}, // a missing result keeps subsequent cuts unsafe
	}
	for _, row := range rows {
		offender, present := unsafe[row.boundary]
		if row.offender == "" {
			testassert.Require(t, !present, "cut %s = %q, want a safe cut", row.boundary, offender)
			continue
		}
		testassert.Require(t, present && offender == row.offender, "cut %s = %q, want the offending call %q", row.boundary, offender, row.offender)
	}
}
