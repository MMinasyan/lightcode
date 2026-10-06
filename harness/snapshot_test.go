package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// factFixtureGraph assembles one validated root Session whose committed
// history carries every fact kind except hook_result's absence: an input with
// a part extra, an assistant publishing one tool call with call extra and
// normalized arguments, a settled hook result, the terminal tool result with
// tool-owned metadata, the completing assistant with usage, an owned
// interruption signal, the success settlement, a compact Operation's
// settlement and its operation-owned compaction entry carrying usage, and an
// operationless background_completion signal. The hook_result entry proves
// the excluded evidence kind.
func factFixtureGraph() *testGraph {
	session := validSessionRecord()
	session.State.CompactionEntryID = hexID(8)
	session.State.Usage = UsageTotals{ByModel: []ModelUsage{{
		Model: testModelRef(),
		Usage: UsageCount{InputTokens: 4, CachedInputTokens: 2, OutputTokens: 3},
	}}}

	op1 := validOperationRecord()
	op1.State.Status = OperationSuccess
	op1.State.ActiveEffect = nil
	op1.State.PendingToolCalls = []PendingToolCall{}
	settled := testTime
	op1.State.SettledAt = &settled
	op1.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(7)}}
	op1.State.Usage = UsageTotals{ByModel: []ModelUsage{{
		Model: testModelRef(),
		Usage: UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2},
	}}}

	op2 := OperationRecord{
		Admission: OperationAdmission{
			SessionID:   testSessionID,
			OperationID: "op-compact",
			RequestKind: RequestKindCompact,
			AgentType:   "coder",
			Execution:   testCapture(),
			AdmittedAt:  testTime,
		},
		State: OperationCurrentState{
			Status:           OperationSuccess,
			StartedAt:        testTime,
			SettledAt:        &settled,
			PendingToolCalls: []PendingToolCall{},
			Usage:            UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: UsageCount{InputTokens: 1, CachedInputTokens: 1, OutputTokens: 1}}}},
			Terminal:         &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(9)}},
		},
	}

	input := validInputEntry(testOpID)
	input.EntryID = hexID(1)
	input.Content[0].Extra = model.Extra{"part": json.RawMessage(`"v"`)}

	assistant1 := validAssistantEntry(testOpID)
	assistant1.EntryID = hexID(2)
	assistant1.Extra = model.Extra{"reasoning": json.RawMessage(`{"deep":[1]}`)}
	call := validToolCallRecord()
	call.ResultEntryID = testResultID
	call.Extra = model.Extra{"call": json.RawMessage(`"x"`)}
	call.NormalizedArguments = json.RawMessage(`{"x":1}`)
	assistant1.ToolCalls = []toolCallRecord{call}

	hook := validHookResultEntry(hookSucceeded)
	hook.EntryID = hexID(3)

	result := validToolResultEntry(testOpID)
	result.EntryID = testResultID
	result.AssistantEntry.EntryID = hexID(2)
	result.Metadata = json.RawMessage(`{"n":1}`)

	assistant2 := validAssistantEntry(testOpID)
	assistant2.EntryID = hexID(5)
	assistant2.ToolCalls = nil
	assistant2.Usage = &UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}

	signal := validSignalEntry(testOpID)
	signal.EntryID = hexID(6)

	settlement1 := validSettlementEntry()
	settlement1.EntryID = hexID(7)

	compaction := validCompactionEntry("op-compact")
	compaction.EntryID = hexID(8)
	compaction.BoundaryEntryID = hexID(2)
	compaction.Usage = &UsageCount{InputTokens: 1, CachedInputTokens: 1, OutputTokens: 1}

	settlement2 := validSettlementEntry()
	settlement2.EntryID = hexID(9)
	settlement2.OperationID = "op-compact"

	background := validBackgroundCompletionEntry()
	background.EntryID = hexID(10)

	entry := func(id string, operationID string, kind EntryKind, seq int64) Entry {
		return Entry{SessionID: testSessionID, ID: id, OperationID: operationID, Kind: kind, Sequence: seq, CommittedAt: testTime}
	}
	return &testGraph{
		session: session,
		ops:     []OperationRecord{op1, op2},
		entries: []testEntry{
			{env: entry(hexID(1), testOpID, EntryInput, 1), input: &input},
			{env: entry(hexID(2), testOpID, EntryAssistant, 2), assistant: &assistant1},
			{env: entry(hexID(3), testOpID, EntryHookResult, 3), hookResult: &hook},
			{env: entry(testResultID, testOpID, EntryToolResult, 4), toolResult: &result},
			{env: entry(hexID(5), testOpID, EntryAssistant, 5), assistant: &assistant2},
			{env: entry(hexID(6), testOpID, EntrySignal, 6), signal: &signal},
			{env: entry(hexID(7), testOpID, EntryOperationSettlement, 7), settlement: &settlement1},
			{env: entry(hexID(8), "op-compact", EntryCompaction, 8), compaction: &compaction},
			{env: entry(hexID(9), "op-compact", EntryOperationSettlement, 9), settlement: &settlement2},
			{env: entry(hexID(10), "", EntrySignal, 10), signal: &background},
		},
	}
}

// onlyMember asserts exactly one payload member of one fact is present and
// returns the EntryKind that member's presence implies.
func onlyMember(t *testing.T, fact HistoryFact) EntryKind {
	t.Helper()
	present := 0
	var kind EntryKind
	for member, isKind := range map[EntryKind]bool{
		EntryInput:               fact.Input != nil,
		EntryAssistant:           fact.Assistant != nil,
		EntryToolResult:          fact.ToolResult != nil,
		EntrySignal:              fact.Signal != nil,
		EntryCompaction:          fact.Compaction != nil,
		EntryOperationSettlement: fact.Settlement != nil,
	} {
		if isKind {
			present++
			kind = member
		}
	}
	if present != 1 {
		t.Fatalf("fact %s carries %d payload members, want exactly one", fact.EntryID, present)
	}
	return kind
}

// FactIdentity is one expected committed fact's envelope identity.
type FactIdentity struct {
	EntryID     string
	OperationID string
	Kind        EntryKind
}

// FactFixture is the owning fact fixture's committed history exposed to the
// external storage-axis test: the Session identity, each expected committed
// entry's envelope identity and kind in order (hook evidence excluded), and
// the hook entry identity that must never appear.
type FactFixture struct {
	SessionID   string
	Identities  []FactIdentity
	HookEntryID string
}

// seedTestGraph writes one fixture graph into any Storage through the public
// transaction contract, preserving each key's record and entry order.
func seedTestGraph(t *testing.T, store Storage, g *testGraph) {
	t.Helper()
	src := g.storage(t)
	err := store.Transact(context.Background(), func(tx Transaction) error {
		for _, regs := range src.registers {
			for _, reg := range regs {
				if _, err := tx.InsertRegister(RegisterDraft{Key: reg.Key, Payload: reg.Payload}); err != nil {
					return err
				}
			}
		}
		for sessionID, entries := range src.entries {
			for _, entry := range entries {
				if _, err := tx.InsertEntry(EntryDraft{
					SessionID:   sessionID,
					ID:          entry.ID,
					OperationID: entry.OperationID,
					Kind:        entry.Kind,
					Payload:     entry.Payload,
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed fixture graph: %v", err)
	}
}

// SeedFactFixture writes the owning fact fixture graph into any Storage and
// returns its expected committed-history envelopes for the external
// storage-axis oracle.
func SeedFactFixture(t *testing.T, store Storage) FactFixture {
	t.Helper()
	g := factFixtureGraph()
	seedTestGraph(t, store, g)
	fixture := FactFixture{SessionID: g.session.Identity.SessionID}
	for _, entry := range g.entries {
		if entry.env.Kind == EntryHookResult {
			fixture.HookEntryID = entry.env.ID
			continue
		}
		fixture.Identities = append(fixture.Identities, FactIdentity{
			EntryID:     entry.env.ID,
			OperationID: entry.env.OperationID,
			Kind:        entry.env.Kind,
		})
	}
	return fixture
}

// TestNarrowReadsCopyOnlyTheirOwnState proves the narrow producers copy no
// unrelated state: warm header and history allocations do not grow when
// Operations, FIFO members, and usage rows are added, the header stays
// constant when history entries are added, and the history capture copies
// only its facts.
func TestNarrowReadsCopyOnlyTheirOwnState(t *testing.T) {
	h := newTestHarness(t, factFixtureGraph().storage(t), nil)
	ctx := context.Background()
	if _, err := h.ReadSessionHeader(ctx, testSessionID); err != nil {
		t.Fatalf("warm ReadSessionHeader: %v", err)
	}
	measure := func() (header, history float64) {
		header = testing.AllocsPerRun(200, func() {
			if _, err := h.ReadSessionHeader(ctx, testSessionID); err != nil {
				t.Fatalf("ReadSessionHeader: %v", err)
			}
		})
		history = testing.AllocsPerRun(200, func() {
			if _, _, err := h.ReadSessionHistory(ctx, testSessionID); err != nil {
				t.Fatalf("ReadSessionHistory: %v", err)
			}
		})
		return header, history
	}
	headerBefore, historyBefore := measure()

	h.mu.Lock()
	c := h.sessions[testSessionID]
	h.mu.Unlock()
	if c == nil {
		t.Fatal("the warmed coordinator is absent")
	}
	c.mu.Lock()
	for i := 0; i < 2000; i++ {
		c.graph.Operations = append(c.graph.Operations, validOperationRecord())
		c.steering = append(c.steering, &pendingMessage{operationID: "s", origin: InputOriginUser, content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}})
		c.queued = append(c.queued, &pendingMessage{operationID: "q", origin: InputOriginUser, content: []model.ContentPart{{Kind: model.PartText, Text: "y"}}})
	}
	c.graph.Session.State.Usage.ByModel = append(c.graph.Session.State.Usage.ByModel, make([]ModelUsage, 5000)...)
	c.mu.Unlock()

	headerAfter, historyAfter := measure()
	if headerAfter != headerBefore {
		t.Fatalf("header allocations = %v after adding Operations/FIFOs/usage, want the unchanged %v", headerAfter, headerBefore)
	}
	if historyAfter != historyBefore {
		t.Fatalf("history allocations = %v after adding Operations/FIFOs/usage, want the unchanged %v", historyAfter, historyBefore)
	}

	// Added history entries may grow the history copy's own allocations, but
	// the metadata header never copies them.
	c.mu.Lock()
	for i := 0; i < 2000; i++ {
		c.graph.Entries = append(c.graph.Entries, c.graph.Entries[0])
	}
	c.mu.Unlock()
	headerWithHistory, _ := measure()
	if headerWithHistory != headerBefore {
		t.Fatalf("header allocations = %v after adding history entries, want the unchanged %v", headerWithHistory, headerBefore)
	}
}

// TestSnapshotSessionFactUnionAndOwnership proves the public fact union: one
// typed member per committed entry selected by kind, hook_result excluded
// from the facts while its validation stays active, ascending sequence order,
// and full deep ownership of every payload value.
func TestSnapshotSessionFactUnionAndOwnership(t *testing.T) {
	t.Run("every payload kind with hook evidence excluded", func(t *testing.T) {
		h := newTestHarness(t, factFixtureGraph().storage(t), nil)
		snap, err := h.SnapshotSession(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("SnapshotSession: %v", err)
		}

		wantKinds := []EntryKind{
			EntryInput, EntryAssistant, EntryToolResult, EntryAssistant, EntrySignal,
			EntryOperationSettlement, EntryCompaction, EntryOperationSettlement, EntrySignal,
		}
		if len(snap.Facts) != len(wantKinds) {
			t.Fatalf("facts = %d entries, want %d (hook_result excluded)", len(snap.Facts), len(wantKinds))
		}
		for i, fact := range snap.Facts {
			if fact.Kind != wantKinds[i] {
				t.Fatalf("fact[%d].Kind = %q, want %q", i, fact.Kind, wantKinds[i])
			}
			if kind := onlyMember(t, fact); kind != fact.Kind {
				t.Fatalf("fact[%d]'s single payload member implies kind %q, want its Kind %q", i, kind, fact.Kind)
			}
			if fact.EntryID == "" || fact.Sequence == 0 || fact.CommittedAt.IsZero() {
				t.Fatalf("fact[%d] loses its envelope identity: %+v", i, fact)
			}
		}
		if snap.Facts[0].Input == nil || snap.Facts[0].Input.Content[0].Extra["part"] == nil {
			t.Fatalf("input fact lost its content part extra")
		}
		firstAssistant := snap.Facts[1].Assistant
		if firstAssistant == nil || firstAssistant.Extra["reasoning"] == nil ||
			len(firstAssistant.ToolCalls) != 1 {
			t.Fatalf("assistant fact lost its extra or call: %+v", firstAssistant)
		}
		call := firstAssistant.ToolCalls[0]
		if call.ID != "call-1" || call.Ordinal != 0 || call.Name != "echo" ||
			call.ResultEntryID != testResultID || call.Extra["call"] == nil ||
			string(call.NormalizedArguments) != `{"x":1}` {
			t.Fatalf("tool call record = %+v", call)
		}
		if snap.Facts[2].ToolResult == nil || string(snap.Facts[2].ToolResult.Metadata) != `{"n":1}` ||
			snap.Facts[2].ToolResult.AssistantEntry.EntryID != hexID(2) {
			t.Fatalf("tool result fact = %+v", snap.Facts[2])
		}
		if snap.Facts[3].Assistant == nil || snap.Facts[3].Assistant.Usage == nil ||
			*snap.Facts[3].Assistant.Usage != (UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}) {
			t.Fatalf("completing assistant fact = %+v", snap.Facts[3])
		}
		if snap.Facts[4].Signal == nil || snap.Facts[4].Signal.Signal != SignalInterruption ||
			snap.Facts[4].Signal.RelatedOperation == nil ||
			snap.Facts[4].Signal.RelatedOperation.OperationID != testOpID {
			t.Fatalf("owned signal fact = %+v", snap.Facts[4])
		}
		if snap.Facts[5].Settlement == nil || snap.Facts[5].Settlement.Status != OperationSuccess {
			t.Fatalf("settlement fact = %+v", snap.Facts[5])
		}
		if snap.Facts[6].Compaction == nil || snap.Facts[6].Compaction.BoundaryEntryID != hexID(2) ||
			snap.Facts[6].Compaction.Usage == nil {
			t.Fatalf("compaction fact = %+v", snap.Facts[6])
		}
		if snap.Facts[8].Signal == nil || snap.Facts[8].Signal.RelatedMember == nil ||
			snap.Facts[8].Signal.OperationID != "" {
			t.Fatalf("background completion fact = %+v", snap.Facts[8])
		}

		// The warm read is complete and coherent: sorted terminal operations,
		// no busy state, and a fresh coordinator's zero local revision.
		if len(snap.Operations) != 2 ||
			snap.Operations[0].Admission.OperationID != testOpID ||
			snap.Operations[1].Admission.OperationID != "op-compact" {
			t.Fatalf("operations = %+v", snap.Operations)
		}
		if snap.ExecutionBusy || snap.LocalRevision != 0 || len(snap.Steering) != 0 || len(snap.Queued) != 0 || len(snap.Background) != 0 {
			t.Fatalf("snapshot state = busy %v local %d steering %d queued %d background %d",
				snap.ExecutionBusy, snap.LocalRevision, len(snap.Steering), len(snap.Queued), len(snap.Background))
		}
		if snap.Session.State.CompactionEntryID != hexID(8) {
			t.Fatalf("session record = %+v", snap.Session)
		}
	})

	t.Run("copied fork prefix facts carry no operation identity", func(t *testing.T) {
		fork := &testGraph{session: validSessionRecord()}
		fork.session.Identity.SourceSessionID = otherSession()
		fork.session.Identity.SourceBoundaryEntryID = otherEntry()
		copiedInput := validInputEntry("")
		copiedInput.EntryID = hexID(1)
		copiedInput.OperationID = ""
		copiedAssistant := validAssistantEntry("")
		copiedAssistant.EntryID = hexID(2)
		copiedAssistant.OperationID = ""
		call := validToolCallRecord()
		call.ResultEntryID = hexID(3)
		copiedAssistant.ToolCalls = []toolCallRecord{call}
		copiedResult := validToolResultEntry("")
		copiedResult.EntryID = hexID(3)
		copiedResult.OperationID = ""
		copiedResult.AssistantEntry.EntryID = hexID(2)
		ownedInput := validInputEntry(testOpID)
		ownedInput.EntryID = hexID(4)
		settlement := validSettlementEntry()
		settlement.EntryID = hexID(5)
		op := validOperationRecord()
		op.State.Status = OperationSuccess
		op.State.ActiveEffect = nil
		op.State.PendingToolCalls = []PendingToolCall{}
		settled := testTime
		op.State.SettledAt = &settled
		op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(5)}}
		op.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(4)}
		fork.ops = []OperationRecord{op}
		entry := func(id, operationID string, kind EntryKind, seq int64) Entry {
			return Entry{SessionID: testSessionID, ID: id, OperationID: operationID, Kind: kind, Sequence: seq, CommittedAt: testTime}
		}
		fork.entries = []testEntry{
			{env: entry(hexID(1), "", EntryInput, 1), input: &copiedInput},
			{env: entry(hexID(2), "", EntryAssistant, 2), assistant: &copiedAssistant},
			{env: entry(hexID(3), "", EntryToolResult, 3), toolResult: &copiedResult},
			{env: entry(hexID(4), testOpID, EntryInput, 4), input: &ownedInput},
			{env: entry(hexID(5), testOpID, EntryOperationSettlement, 5), settlement: &settlement},
		}
		h := newTestHarness(t, fork.storage(t), nil)
		snap, err := h.SnapshotSession(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("SnapshotSession: %v", err)
		}
		if len(snap.Facts) != 5 {
			t.Fatalf("fork facts = %d, want 5", len(snap.Facts))
		}
		for i := 0; i < 3; i++ {
			if snap.Facts[i].OperationID != "" {
				t.Fatalf("copied fact[%d] carries operation %q, want none", i, snap.Facts[i].OperationID)
			}
		}
		for i := 3; i < 5; i++ {
			if snap.Facts[i].OperationID != testOpID {
				t.Fatalf("owned fact[%d] operation = %q, want %q", i, snap.Facts[i].OperationID, testOpID)
			}
		}
		if snap.Facts[2].ToolResult == nil || snap.Facts[2].ToolResult.ToolCallID != "call-1" {
			t.Fatalf("copied tool result fact = %+v", snap.Facts[2])
		}
	})

	t.Run("hook evidence is validated, never skipped", func(t *testing.T) {
		graph := factFixtureGraph()
		graph.entries[2].rawOverride = json.RawMessage(`{}`) // the hook_result entry's payload loses every required field
		h := newTestHarness(t, graph.storage(t), nil)
		_, err := h.SnapshotSession(context.Background(), testSessionID)
		var corrupt *CorruptionError
		if !errors.As(err, &corrupt) || corrupt.SessionID != testSessionID {
			t.Fatalf("snapshot over a corrupt hook entry = %v, want the owning Session's corruption error", err)
		}
	})
}

// TestSnapshotSessionMemberPublications proves the background member rows of
// the local revision: an admission publishes the membership view and advances
// the counter once, a finish publishes the removal and advances it once, and
// a repeated finish is a no-op publication.
func TestSnapshotSessionMemberPublications(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	before, err := h.SnapshotSession(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("baseline snapshot: %v", err)
	}
	if len(before.Background) != 0 || before.LocalRevision != 0 {
		t.Fatalf("baseline = %+v", before)
	}

	member, err := admitLocked(h, c, memberJob, "job-1", 0)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	afterAdmit, err := h.SnapshotSession(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("snapshot after admission: %v", err)
	}
	if len(afterAdmit.Background) != 1 || afterAdmit.Background[0] != (BackgroundMemberView{Kind: "job", ID: "job-1"}) {
		t.Fatalf("background after admission = %+v", afterAdmit.Background)
	}
	if afterAdmit.LocalRevision != before.LocalRevision+1 {
		t.Fatalf("local revision after admission = %d, want %d", afterAdmit.LocalRevision, before.LocalRevision+1)
	}

	claimed, ok := claimLocked(h, c, member.completionID)
	if !ok || claimed != member {
		t.Fatalf("claim failed")
	}
	h.finishBackgroundMember(c, claimed)
	afterFinish, err := h.SnapshotSession(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("snapshot after finish: %v", err)
	}
	if len(afterFinish.Background) != 0 {
		t.Fatalf("background after finish = %+v", afterFinish.Background)
	}
	if afterFinish.LocalRevision != afterAdmit.LocalRevision+1 {
		t.Fatalf("local revision after finish = %d, want %d", afterFinish.LocalRevision, afterAdmit.LocalRevision+1)
	}

	h.finishBackgroundMember(c, claimed) // already removed: a no-op publication
	afterRepeat, err := h.SnapshotSession(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("snapshot after repeat finish: %v", err)
	}
	if afterRepeat.LocalRevision != afterFinish.LocalRevision {
		t.Fatalf("repeat finish advanced the revision to %d, want %d", afterRepeat.LocalRevision, afterFinish.LocalRevision)
	}
}

// TestSnapshotSessionSteeringDeliveryPublications proves the steering
// delivery's publication shape: the selected head stays buffered while its
// delivery attempt runs, a failed attempt drops it exactly once as the final
// outcome with no durable entry and no register advance, and a parked
// delivery blocks any snapshot until the adoption completes, which then
// observes the entirely-new state with the removal riding the durable
// advance.
func TestSnapshotSessionSteeringDeliveryPublications(t *testing.T) {
	t.Run("failed delivery is a final dropped publication", func(t *testing.T) {
		store := emptyStore(t)
		// the second attempt parks inside its assembly, so the post-attempt
		// state stays stable until the subtest's assertions complete
		release2 := make(chan struct{})
		defer close(release2)
		script := newModelScript(turn(testToolCall("call-1")), modelAttempt{stream: &parkingStream{release: release2}})
		script.gate = make(chan struct{})
		h, cancel := newCancelableHarness(t, store, steeringPrepared(script), nil)
		defer cancel()
		defer script.releaseGate()
		session := createSession(t, h)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		receiveBounded(t, script.arrived, "first model boundary")
		baseline, err := h.SnapshotSession(context.Background(), session)
		if err != nil {
			t.Fatalf("baseline snapshot: %v", err)
		}

		if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "steer-me"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		buffered, err := h.SnapshotSession(context.Background(), session)
		if err != nil {
			t.Fatalf("snapshot after enqueue: %v", err)
		}
		if len(buffered.Steering) != 1 || buffered.Steering[0].OperationID != "op-2" ||
			buffered.Steering[0].Content[0].Text != "steer-me" {
			t.Fatalf("steering buffer after enqueue = %+v", buffered.Steering)
		}
		// one Submit's publications: the reservation, the enqueue, and its release
		if buffered.LocalRevision != baseline.LocalRevision+3 {
			t.Fatalf("enqueue revision = %d, want %d", buffered.LocalRevision, baseline.LocalRevision+3)
		}

		// The steering delivery's entry insert fails once: the selected head
		// stays buffered through the attempt, is dropped exactly once as the
		// attempt's final outcome, the register does not adopt the entry, and
		// the Operation continues.
		var once sync.Once
		deliveryAttempted := make(chan struct{})
		store.entryHook = func(draft EntryDraft) error {
			// a steered input commits under the running Operation's identity,
			// so the delivery is recognized by its content
			if !strings.Contains(string(draft.Payload), "steer-me") {
				return nil
			}
			once.Do(func() { close(deliveryAttempted) })
			return errors.New("delivery failed")
		}
		script.releaseGate()
		receiveBounded(t, deliveryAttempted, "failed steering delivery")
		receiveBounded(t, script.arrived, "post-failure model boundary")

		after, err := h.SnapshotSession(context.Background(), session)
		if err != nil {
			t.Fatalf("snapshot after failed delivery: %v", err)
		}
		if len(after.Steering) != 0 {
			t.Fatalf("steering buffer after the failed attempt = %+v, want the dropped head gone", after.Steering)
		}
		for _, fact := range after.Facts {
			if fact.Input != nil && fact.Input.Content[0].Text == "steer-me" {
				t.Fatalf("the failed delivery committed entry %s", fact.EntryID)
			}
		}
		if after.LocalRevision != buffered.LocalRevision+1 {
			t.Fatalf("local revision after the failed attempt = %d, want %d (the drop is the failed attempt's final publication)",
				after.LocalRevision, buffered.LocalRevision+1)
		}
		sessionKey := RegisterKey{SessionID: session, Kind: RegisterSession}
		if after.Session.Revision != registerRevision(t, store, session, sessionKey) {
			t.Fatalf("snapshot revision %d disagrees with the durable register %d",
				after.Session.Revision, registerRevision(t, store, session, sessionKey))
		}
		entries, err := store.ReadEntries(context.Background(), session, 0)
		if err != nil {
			t.Fatalf("read entries: %v", err)
		}
		for _, entry := range entries {
			if strings.Contains(string(entry.Payload), "steer-me") {
				t.Fatalf("the failed delivery's entry %s reached storage", entry.ID)
			}
		}
	})

	t.Run("a parked delivery blocks snapshots until the adoption", func(t *testing.T) {
		store := emptyStore(t)
		// the second attempt parks inside its assembly, so the post-adoption
		// state stays stable until the subtest's assertions complete: no
		// terminal commit and no successor drain can publish while reading
		release2 := make(chan struct{})
		defer close(release2)
		script := newModelScript(turn(testToolCall("call-1")), modelAttempt{stream: &parkingStream{release: release2}})
		script.gate = make(chan struct{})
		h, cancel := newCancelableHarness(t, store, steeringPrepared(script), nil)
		defer cancel()
		defer script.releaseGate()
		session := createSession(t, h)
		c := cachedCoordinator(t, h, session)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		receiveBounded(t, script.arrived, "first model boundary")
		baseline, err := h.SnapshotSession(context.Background(), session)
		if err != nil {
			t.Fatalf("baseline snapshot: %v", err)
		}
		if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "steer-me"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		buffered, err := h.SnapshotSession(context.Background(), session)
		if err != nil || len(buffered.Steering) != 1 || buffered.LocalRevision != baseline.LocalRevision+3 {
			t.Fatalf("buffered snapshot = %+v err %v, want one steering item advanced by the Submit's three publications", buffered, err)
		}

		deliveryStarted := make(chan struct{})
		release := make(chan struct{})
		releaseDelivery := sync.OnceFunc(func() { close(release) })
		defer releaseDelivery()
		deliverySettled := make(chan struct{})
		var once sync.Once
		store.entryHook = func(draft EntryDraft) error {
			// a steered input commits under the running Operation's identity,
			// so the delivery is recognized by its content
			if !strings.Contains(string(draft.Payload), "steer-me") {
				return nil
			}
			once.Do(func() {
				close(deliveryStarted)
				<-release
				close(deliverySettled)
			})
			return nil
		}
		beforeAdoption := registerRevision(t, store, session, RegisterKey{SessionID: session, Kind: RegisterSession})
		script.releaseGate()

		receiveBounded(t, deliveryStarted, "parked steering delivery")

		// The parked producer must hold the coordinator mutex across the
		// parked transaction: a free mutex here means the critical section
		// was lost around the park. On this deterministic fixture the main
		// test and the one parked producer are the only runners, so the check
		// cannot pass spuriously.
		if c.mu.TryLock() {
			c.mu.Unlock()
			releaseDelivery()
			receiveBounded(t, deliverySettled, "released steering transaction")
			receiveBounded(t, script.arrived, "successor model boundary")
			t.Fatal("the parked delivery transaction does not hold the coordinator mutex")
		}

		// The reader starts while the mutex is provably held and completes
		// only after the adoption: its body must be the entirely-new state.
		type snapResult struct {
			snap SessionSnapshot
			err  error
		}
		results := make(chan snapResult, 1)
		go func() {
			snap, err := h.SnapshotSession(context.Background(), session)
			results <- snapResult{snap, err}
		}()
		releaseDelivery()
		receiveBounded(t, script.arrived, "post-adoption model boundary")
		r := receiveBounded(t, results, "post-adoption snapshot")
		if r.err != nil {
			t.Fatalf("snapshot after the adoption: %v", r.err)
		}
		if len(r.snap.Steering) != 0 {
			t.Fatalf("steering buffer after the adoption = %+v", r.snap.Steering)
		}
		found := false
		for _, fact := range r.snap.Facts {
			if fact.Input != nil && fact.Input.Content[0].Text == "steer-me" {
				found = true
			}
		}
		if !found {
			t.Fatalf("the adopted delivery's input fact is missing")
		}
		if r.snap.Session.Revision <= beforeAdoption {
			t.Fatalf("snapshot revision %d did not advance past the pre-adoption register %d",
				r.snap.Session.Revision, beforeAdoption)
		}
		// The selected head stayed buffered through the parked attempt; the
		// durable adoption advanced only the register — the removal rode it,
		// so no local publication fired.
		if r.snap.LocalRevision != buffered.LocalRevision {
			t.Fatalf("post-adoption local revision = %d, want %d", r.snap.LocalRevision, buffered.LocalRevision)
		}
		if !r.snap.ExecutionBusy {
			t.Fatalf("post-adoption snapshot = busy %v, want the still-running Operation", r.snap.ExecutionBusy)
		}
	})
}

func TestBufferedDeliverySelectionIdentity(t *testing.T) {
	for _, kind := range []string{"steering", "queued"} {
		t.Run(kind, func(t *testing.T) {
			c := &coordinator{}
			c.mu.Lock()
			defer c.mu.Unlock()
			fifo := &c.queued
			if kind == "steering" {
				fifo = &c.steering
			}
			selected := &pendingMessage{operationID: "same-id", content: admissionContent("same-content")}
			next := &pendingMessage{operationID: "same-id", content: admissionContent("same-content")}
			*fifo = []*pendingMessage{selected, next}
			if c.dropBufferedLocked(nil) || len(*fifo) != 2 {
				t.Fatal("ordinary admission removed a pending head")
			}
			if !c.dropBufferedLocked(selected) || len(*fifo) != 1 || (*fifo)[0] != next {
				t.Fatal("delivery failed to preserve its appended sibling")
			}
			if c.dropBufferedLocked(selected) {
				t.Fatal("repeated completion removed the next head")
			}
			*fifo = []*pendingMessage{selected}
			c.discardBuffers()
			*fifo = []*pendingMessage{next}
			if c.dropBufferedLocked(selected) || len(*fifo) != 1 || (*fifo)[0] != next {
				t.Fatal("discarded selection removed a replacement with the same payload")
			}
		})
	}
}

func TestBufferedDuplicateDeliveryFinal(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript(turn(), turn())
	script.gate = make(chan struct{})
	h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
	defer cancel()
	defer script.releaseGate()
	session := createSession(t, h)
	if _, err := submitText(t, h, session, "initial", MessageModeRegular, "hello"); err != nil {
		t.Fatal(err)
	}
	receiveBounded(t, script.arrived, "initial model boundary")
	for _, text := range []string{"first", "duplicate"} {
		result, err := submitText(t, h, session, "queued", MessageModeQueued, text)
		if err != nil || result.Disposition != DispositionQueued {
			t.Fatalf("buffer %q = %+v, %v", text, result, err)
		}
	}
	script.releaseGate()
	quiet := awaitHarnessQuiet(t, h, session)
	if len(quiet.Steering) != 0 || len(quiet.Queued) != 0 || len(quiet.Operations) != 2 {
		t.Fatalf("duplicate did not retire exactly once: %+v", quiet)
	}
	if got := strings.Join(entryTexts(t, store, session), ","); got != "hello,first" {
		t.Fatalf("committed inputs = %q, want only the first buffered payload", got)
	}
}

// steeringPrepared returns the delivery fixtures' prepared execution: the
// scripted model over an immediate-success tool plan, so a published call
// settles at its boundary and the next model boundary drains the steering.
func steeringPrepared(script *modelScript) PreparedExecution {
	tool := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{
			Permissions: fixturePermission,
			Immediate:   &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}},
		}
	}
	return preparedExecuting(effectExecution(script.model, tool))
}

// parkingStream is one accepted stream whose first read parks until its
// release channel closes, then completes one turn.
type parkingStream struct {
	release chan struct{}
	done    bool
}

func (s *parkingStream) Recv() (model.StreamDelta, error) {
	if !s.done {
		<-s.release
		s.done = true
	}
	return model.StreamDelta{
		HasChoice:        true,
		Role:             "assistant",
		ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "done"}},
		FinishReason:     "stop",
	}, nil
}

func (s *parkingStream) Close() error { return nil }
