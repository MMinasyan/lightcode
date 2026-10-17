package harness

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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
// unrelated state: the metadata header and the history capture allocate the
// same whether a small or a far larger group of unrelated Operations, queued
// raw payloads, and usage rows is present, a fixed selected entry-ID subset's
// facts allocate the same whether a small or a far larger group of unselected
// payload-bearing entries is present, and neither the header nor the history
// capture copies added history entries. A history component extracted from an
// eagerly copied hydration aggregate — or a facts accessor that cloned every
// payload before filtering — would scale with the unrelated groups and fail
// the equalities.
func TestNarrowReadsCopyOnlyTheirOwnState(t *testing.T) {
	h := newTestHarness(t, factFixtureGraph().storage(t), nil)
	ctx := context.Background()
	if _, err := h.ReadSessionHeader(ctx, testSessionID); err != nil {
		t.Fatalf("warm ReadSessionHeader: %v", err)
	}
	if _, err := h.ReadHistoryView(ctx, testSessionID); err != nil {
		t.Fatalf("warm ReadHistoryView: %v", err)
	}
	c := cachedCoordinator(t, h, testSessionID)
	measure := func() (header, history float64) {
		header = testing.AllocsPerRun(200, func() {
			if _, err := h.ReadSessionHeader(ctx, testSessionID); err != nil {
				t.Fatalf("ReadSessionHeader: %v", err)
			}
		})
		history = testing.AllocsPerRun(200, func() {
			if _, err := h.ReadHistoryView(ctx, testSessionID); err != nil {
				t.Fatalf("ReadHistoryView: %v", err)
			}
		})
		return header, history
	}
	headerSmall, historySmall := measure()

	// The controlled unrelated groups: every queued member carries a raw
	// Extra payload the history capture must not copy, and every appended
	// Operation is unused by the read.
	addUnrelated := func(operations, payloads int) {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i := 0; i < operations; i++ {
			c.graph.Operations = append(c.graph.Operations, validOperationRecord())
		}
		for i := 0; i < payloads; i++ {
			c.queued = append(c.queued, &pendingMessage{
				operationID: "q",
				origin:      InputOriginUser,
				content: []model.ContentPart{{
					Kind:  model.PartText,
					Text:  "queued",
					Extra: model.Extra{"raw": json.RawMessage(`{"n":[1,1.0,9007199254740993]}`)},
				}},
			})
		}
		c.graph.Session.State.Usage.ByModel = append(c.graph.Session.State.Usage.ByModel, make([]ModelUsage, payloads)...)
	}
	// The selected subset is fixed across the variants; the appended entries
	// are unrelated unselected payloads the facts accessor must not clone.
	selectedIDs := []string{hexID(1), hexID(2)}
	addUnselectedHistory := func(count int) {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i := 0; i < count; i++ {
			id := hexID(1000 + i)
			c.graph.Entries = append(c.graph.Entries, graphEntry{
				Envelope: Entry{ID: id, Sequence: int64(1000 + i), Kind: EntryInput, CommittedAt: testTime},
				Input: &inputEntry{
					EntryID: id, OperationID: "unselected", Origin: InputOriginUser,
					Content: []model.ContentPart{{
						Kind:  model.PartText,
						Text:  "unselected",
						Extra: model.Extra{"raw": json.RawMessage(`{"n":[1,1.0,9007199254740993]}`)},
					}},
				},
			})
		}
	}
	factsMeasure := func() float64 {
		view, err := h.ReadHistoryView(ctx, testSessionID)
		if err != nil {
			t.Fatalf("ReadHistoryView: %v", err)
		}
		return testing.AllocsPerRun(200, func() {
			if got := view.HistoryFacts(selectedIDs); len(got) != len(selectedIDs) {
				t.Fatalf("selected facts = %d, want the fixed %d", len(got), len(selectedIDs))
			}
		})
	}
	factsSmall := factsMeasure()
	addUnrelated(8, 8)
	addUnselectedHistory(8)
	headerMid, historyMid := measure()
	factsMid := factsMeasure()
	addUnrelated(2000, 2000)
	addUnselectedHistory(2000)
	headerLarge, historyLarge := measure()
	factsLarge := factsMeasure()
	if headerMid != headerSmall || headerLarge != headerSmall {
		t.Fatalf("header allocations = %v after the small group and %v after the large group, want the unchanged %v",
			headerMid, headerLarge, headerSmall)
	}
	if historyMid != historySmall || historyLarge != historySmall {
		t.Fatalf("history allocations = %v after the small group and %v after the large group, want the unchanged %v",
			historyMid, historyLarge, historySmall)
	}
	if factsMid != factsSmall || factsLarge != factsSmall {
		t.Fatalf("selected-facts allocations = %v after the small group and %v after the large group, want the unchanged %v",
			factsMid, factsLarge, factsSmall)
	}

	// Added history entries may grow the callers that request facts, but
	// neither the metadata header nor the history capture copies them.
	c.mu.Lock()
	for i := 0; i < 2000; i++ {
		c.graph.Entries = append(c.graph.Entries, c.graph.Entries[0])
	}
	c.mu.Unlock()
	headerWithHistory, historyWithHistory := measure()
	if headerWithHistory != headerSmall {
		t.Fatalf("header allocations = %v after adding history entries, want the unchanged %v", headerWithHistory, headerSmall)
	}
	if historyWithHistory != historySmall {
		t.Fatalf("history capture allocations = %v after adding history entries, want the unchanged %v", historyWithHistory, historySmall)
	}
}

// readSessionView reads one Session's composed hydration view.
func readSessionView(t *testing.T, h *Harness, sessionID string) SessionReadView {
	t.Helper()
	view, err := h.ReadSessionView(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadSessionView(%s): %v", sessionID, err)
	}
	return view
}

// readSessionFacts reads every committed fact of one Session's history.
func readSessionFacts(t *testing.T, h *Harness, sessionID string) []HistoryFact {
	t.Helper()
	view, err := h.ReadHistoryView(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadHistoryView(%s): %v", sessionID, err)
	}
	return view.HistoryFacts(historyHeaderIDs(view.HistoryHeaders()))
}

// historyHeaderIDs collects one history capture's fact-contributing entry
// identities in sequence order: the request the fact accessor consumes.
func historyHeaderIDs(headers []HistoryEntryHeader) []string {
	ids := make([]string, 0, len(headers))
	for _, header := range headers {
		ids = append(ids, header.EntryID)
	}
	return ids
}

// TestHistoryViewFactUnionAndOwnership proves the public fact union: one
// typed member per committed entry selected by kind, hook_result excluded
// from the facts while its validation stays active, ascending sequence order,
// and full deep ownership of every payload value. The composed view's
// hydration groups stay coherent and its Operation summaries stay sorted.
func TestHistoryViewFactUnionAndOwnership(t *testing.T) {
	t.Run("every payload kind with hook evidence excluded", func(t *testing.T) {
		h := newTestHarness(t, factFixtureGraph().storage(t), nil)
		view, err := h.ReadHistoryView(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("ReadHistoryView: %v", err)
		}
		headers := view.HistoryHeaders()
		for i, header := range headers {
			if header.EntryID == "" || header.Sequence == 0 {
				t.Fatalf("header[%d] = %+v, want the committed envelope identity", i, header)
			}
			if i > 0 && header.Sequence <= headers[i-1].Sequence {
				t.Fatalf("header[%d] sequence %d does not follow %d, want ascending sequence order", i, header.Sequence, headers[i-1].Sequence)
			}
			if header.Kind != EntryInput && header.InputOrigin != "" {
				t.Fatalf("header[%d] = %+v, want no input origin on a non-input kind", i, header)
			}
		}
		facts := view.HistoryFacts(historyHeaderIDs(headers))

		wantKinds := []EntryKind{
			EntryInput, EntryAssistant, EntryToolResult, EntryAssistant, EntrySignal,
			EntryOperationSettlement, EntryCompaction, EntryOperationSettlement, EntrySignal,
		}
		if len(facts) != len(wantKinds) {
			t.Fatalf("facts = %d entries, want %d (hook_result excluded)", len(facts), len(wantKinds))
		}
		for i, fact := range facts {
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
		if facts[0].Input == nil || facts[0].Input.Content[0].Extra["part"] == nil {
			t.Fatalf("input fact lost its content part extra")
		}
		firstAssistant := facts[1].Assistant
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
		if facts[2].ToolResult == nil || string(facts[2].ToolResult.Metadata) != `{"n":1}` ||
			facts[2].ToolResult.AssistantEntry.EntryID != hexID(2) {
			t.Fatalf("tool result fact = %+v", facts[2])
		}
		if facts[3].Assistant == nil || facts[3].Assistant.Usage == nil ||
			*facts[3].Assistant.Usage != (UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}) {
			t.Fatalf("completing assistant fact = %+v", facts[3])
		}
		if facts[4].Signal == nil || facts[4].Signal.Signal != SignalInterruption ||
			facts[4].Signal.RelatedOperation == nil ||
			facts[4].Signal.RelatedOperation.OperationID != testOpID {
			t.Fatalf("owned signal fact = %+v", facts[4])
		}
		if facts[5].Settlement == nil || facts[5].Settlement.Status != OperationSuccess {
			t.Fatalf("settlement fact = %+v", facts[5])
		}
		if facts[6].Compaction == nil || facts[6].Compaction.BoundaryEntryID != hexID(2) ||
			facts[6].Compaction.Usage == nil {
			t.Fatalf("compaction fact = %+v", facts[6])
		}
		if facts[8].Signal == nil || facts[8].Signal.RelatedMember == nil ||
			facts[8].Signal.OperationID != "" {
			t.Fatalf("background completion fact = %+v", facts[8])
		}

		// The composed view is coherent: sorted Operation summaries, no busy
		// state, a fresh coordinator's zero local revision, and the owned
		// register totals.
		sessionView, err := h.ReadSessionView(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("ReadSessionView: %v", err)
		}
		if len(sessionView.Operations) != 2 ||
			sessionView.Operations[0].OperationID != testOpID ||
			sessionView.Operations[1].OperationID != "op-compact" {
			t.Fatalf("operations = %+v", sessionView.Operations)
		}
		if sessionView.History.ExecutionBusy || sessionView.History.Revision.LocalRevision != 0 ||
			len(sessionView.Steering) != 0 || len(sessionView.Queued) != 0 || len(sessionView.Background) != 0 {
			t.Fatalf("session view state = busy %v local %d steering %d queued %d background %d",
				sessionView.History.ExecutionBusy, sessionView.History.Revision.LocalRevision,
				len(sessionView.Steering), len(sessionView.Queued), len(sessionView.Background))
		}
		wantUsage := UsageTotals{ByModel: []ModelUsage{{
			Model: testModelRef(),
			Usage: UsageCount{InputTokens: 4, CachedInputTokens: 2, OutputTokens: 3},
		}}}
		if !reflect.DeepEqual(sessionView.TotalUsage, wantUsage) {
			t.Fatalf("total usage = %+v, want the register totals %+v", sessionView.TotalUsage, wantUsage)
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
		view, err := h.ReadHistoryView(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("ReadHistoryView: %v", err)
		}
		facts := view.HistoryFacts(historyHeaderIDs(view.HistoryHeaders()))
		if len(facts) != 5 {
			t.Fatalf("fork facts = %d, want 5", len(facts))
		}
		for i := 0; i < 3; i++ {
			if facts[i].OperationID != "" {
				t.Fatalf("copied fact[%d] carries operation %q, want none", i, facts[i].OperationID)
			}
		}
		for i := 3; i < 5; i++ {
			if facts[i].OperationID != testOpID {
				t.Fatalf("owned fact[%d] operation = %q, want %q", i, facts[i].OperationID, testOpID)
			}
		}
		if facts[2].ToolResult == nil || facts[2].ToolResult.ToolCallID != "call-1" {
			t.Fatalf("copied tool result fact = %+v", facts[2])
		}
	})

	t.Run("hook evidence is validated, never skipped", func(t *testing.T) {
		graph := factFixtureGraph()
		graph.entries[2].rawOverride = json.RawMessage(`{}`) // the hook_result entry's payload loses every required field
		h := newTestHarness(t, graph.storage(t), nil)
		_, err := h.ReadHistoryView(context.Background(), testSessionID)
		var corrupt *CorruptionError
		if !errors.As(err, &corrupt) || corrupt.SessionID != testSessionID {
			t.Fatalf("history read over a corrupt hook entry = %v, want the owning Session's corruption error", err)
		}
	})
}

// TestHistoryViewAccessorsPure proves the history accessors' value semantics
// over constructed immutable entries: headers carry sequence-ordered value
// metadata with hook evidence excluded, facts cover exactly the requested
// identities in sequence order with owned payloads, and the context
// measurement scans backwards to the captured compaction boundary.
func TestHistoryViewAccessorsPure(t *testing.T) {
	inputEntryValue := graphEntry{
		Envelope: Entry{ID: "input-1", Sequence: 1, Kind: EntryInput, CommittedAt: testTime},
		Input: &inputEntry{
			EntryID: "input-1", Origin: InputOriginUser,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "hi"}},
		},
	}
	hook := graphEntry{
		Envelope:   Entry{ID: "hook-1", Sequence: 2, Kind: EntryHookResult, CommittedAt: testTime},
		HookResult: &hookResultEntry{EntryID: "hook-1", Status: hookSucceeded},
	}
	assistantValue := graphEntry{
		Envelope: Entry{ID: "assistant-1", Sequence: 3, Kind: EntryAssistant, CommittedAt: testTime},
		Assistant: &assistantEntry{
			EntryID: "assistant-1", Status: model.OutputCompleted,
			Usage: &UsageCount{InputTokens: 7, CachedInputTokens: 2, OutputTokens: 1},
		},
	}
	view := HistoryReadView{prefix: []graphEntry{inputEntryValue, hook, assistantValue}}

	wantHeaders := []HistoryEntryHeader{
		{EntryID: "input-1", Sequence: 1, Kind: EntryInput, InputOrigin: InputOriginUser},
		{EntryID: "assistant-1", Sequence: 3, Kind: EntryAssistant},
	}
	if headers := view.HistoryHeaders(); !reflect.DeepEqual(headers, wantHeaders) {
		t.Fatalf("headers = %+v, want %+v (hook evidence excluded)", headers, wantHeaders)
	}

	facts := view.HistoryFacts([]string{"assistant-1", "missing", "input-1", "input-1", "hook-1"})
	if len(facts) != 2 || facts[0].EntryID != "input-1" || facts[1].EntryID != "assistant-1" {
		t.Fatalf("facts = %+v, want the deduplicated sequence-ordered input and assistant", facts)
	}
	if facts[0].Input == nil || facts[0].Input.Content[0].Text != "hi" || facts[0].Kind != EntryInput {
		t.Fatalf("input fact = %+v, want the requested owned input payload", facts[0])
	}
	facts[0].Input.Content[0].Text = "mutated"
	if again := view.HistoryFacts([]string{"input-1"}); again[0].Input.Content[0].Text != "hi" {
		t.Fatal("a mutation of the returned fact reached the captured entry")
	}
	if empty := view.HistoryFacts(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("empty facts = %+v, want the non-nil empty result", empty)
	}

	assistantAt := func(id string, seq int64, usage *UsageCount) graphEntry {
		return graphEntry{
			Envelope:  Entry{ID: id, Sequence: seq, Kind: EntryAssistant, CommittedAt: testTime},
			Assistant: &assistantEntry{EntryID: id, Status: model.OutputCompleted, Usage: usage},
		}
	}
	settlementAt := func(id string, seq int64, usage *UsageCount) graphEntry {
		return graphEntry{
			Envelope:   Entry{ID: id, Sequence: seq, Kind: EntryOperationSettlement, CommittedAt: testTime},
			Settlement: &operationSettlementEntry{EntryID: id, Status: OperationSuccess, Usage: usage},
		}
	}
	compactionAt := func(id string, seq int64) graphEntry {
		return graphEntry{
			Envelope:   Entry{ID: id, Sequence: seq, Kind: EntryCompaction, CommittedAt: testTime},
			Compaction: &compactionEntry{EntryID: id, Summary: "summary", Usage: &UsageCount{InputTokens: 9, CachedInputTokens: 9, OutputTokens: 9}},
		}
	}
	count := func(v int64) *UsageCount { return &UsageCount{InputTokens: v} }
	measurement := []struct {
		name         string
		compactionID string
		entries      []graphEntry
		want         *UsageCount
	}{
		{"no measurement", "", []graphEntry{inputEntryValue}, nil},
		{
			"latest assistant wins",
			"",
			[]graphEntry{assistantAt("a1", 1, count(100)), assistantAt("a2", 2, count(200))},
			count(200),
		},
		{
			"latest settlement wins",
			"",
			[]graphEntry{assistantAt("a1", 1, count(100)), settlementAt("s1", 2, count(300))},
			count(300),
		},
		{
			"nil usage transparent",
			"",
			[]graphEntry{assistantAt("a1", 1, count(100)), assistantAt("a2", 2, nil)},
			count(100),
		},
		{
			"compaction resets to zero",
			"c1",
			[]graphEntry{assistantAt("a1", 1, count(100)), compactionAt("c1", 2), assistantAt("a2", 3, nil)},
			nil,
		},
		{
			"boundary hides earlier measurement",
			"c1",
			[]graphEntry{assistantAt("a1", 1, count(100)), compactionAt("c1", 2), assistantAt("a2", 3, count(50))},
			count(50),
		},
		{
			"settlement measured after boundary",
			"c1",
			[]graphEntry{assistantAt("a1", 1, count(100)), compactionAt("c1", 2), assistantAt("a2", 3, nil), settlementAt("s1", 4, count(300))},
			count(300),
		},
		{
			"compaction usage stays totals-only",
			"c1",
			[]graphEntry{compactionAt("c1", 1)},
			nil,
		},
	}
	for _, tc := range measurement {
		scanned := HistoryReadView{prefix: tc.entries, compactionEntryID: tc.compactionID}
		got := scanned.LatestContextUsage()
		switch {
		case tc.want == nil && got != nil:
			t.Fatalf("%s: measurement = %+v, want none", tc.name, got)
		case tc.want != nil && (got == nil || *got != *tc.want):
			t.Fatalf("%s: measurement = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	scanned := HistoryReadView{prefix: []graphEntry{assistantAt("a1", 1, count(100))}}
	owned := scanned.LatestContextUsage()
	owned.InputTokens = 999
	if fresh := scanned.LatestContextUsage(); fresh.InputTokens != 100 {
		t.Fatalf("a mutation of the returned measurement reached the captured entry: %+v", fresh)
	}
}

// TestHistoryViewRetainedCaptureStable proves a retained narrow capture keeps
// answering from its own immutable prefix and captured scalars while the live
// coordinator moves on: appended entries — including an assistant and the
// result entry it reserves — never appear, a replaced Operation record, a
// moved compaction boundary, and advanced revisions never change the accessor
// answers, while a fresh read of the same Session does observe the new state.
func TestHistoryViewRetainedCaptureStable(t *testing.T) {
	h := newTestHarness(t, factFixtureGraph().storage(t), nil)
	ctx := context.Background()
	view, err := h.ReadHistoryView(ctx, testSessionID)
	if err != nil {
		t.Fatalf("ReadHistoryView: %v", err)
	}
	beforeHeaders := view.HistoryHeaders()
	beforeIDs := historyHeaderIDs(beforeHeaders)
	beforeFacts := view.HistoryFacts(beforeIDs)
	beforeUsage := view.LatestContextUsage()
	beforeRevision := view.Revision
	if len(beforeHeaders) == 0 || len(beforeFacts) == 0 {
		t.Fatal("fixture has no committed history")
	}

	const futureAssistantID = "fffffffffffffffffffffffffffffff1"
	const futureResultID = "fffffffffffffffffffffffffffffff2"
	c := cachedCoordinator(t, h, testSessionID)
	c.mu.Lock()
	c.graph.Operations = append(c.graph.Operations, validOperationRecord())
	c.graph.Session.Revision++
	c.localRev++
	c.graph.Session.State.CompactionEntryID = futureAssistantID
	c.graph.Entries = append(c.graph.Entries,
		graphEntry{
			Envelope: Entry{ID: futureAssistantID, Sequence: 900, Kind: EntryAssistant, CommittedAt: testTime},
			Assistant: &assistantEntry{
				EntryID: futureAssistantID, Status: model.OutputCompleted,
				Usage:     &UsageCount{InputTokens: 4242},
				ToolCalls: []toolCallRecord{{ID: "call-future", Name: "read", ResultEntryID: futureResultID}},
			},
		},
		graphEntry{
			Envelope: Entry{ID: futureResultID, Sequence: 901, Kind: EntryToolResult, CommittedAt: testTime},
			ToolResult: &toolResultEntry{
				EntryID: futureResultID, OperationID: "op-future", ToolCallID: "call-future",
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: futureAssistantID},
				Status:         model.ResultSuccess,
			},
		},
	)
	c.mu.Unlock()

	if after := view.HistoryHeaders(); !reflect.DeepEqual(after, beforeHeaders) {
		t.Fatalf("retained headers changed after the live appends: %+v vs %+v", after, beforeHeaders)
	}
	if after := view.HistoryFacts(beforeIDs); !reflect.DeepEqual(after, beforeFacts) {
		t.Fatalf("retained facts changed after the live appends: %+v vs %+v", after, beforeFacts)
	}
	if future := view.HistoryFacts([]string{futureAssistantID, futureResultID}); len(future) != 0 {
		t.Fatalf("retained facts returned appended entries: %+v", future)
	}
	if got := view.LatestContextUsage(); !reflect.DeepEqual(got, beforeUsage) {
		t.Fatalf("retained context measurement = %+v, want the captured %+v", got, beforeUsage)
	}
	if view.Revision != beforeRevision {
		t.Fatalf("retained revision = %+v, want the captured %+v", view.Revision, beforeRevision)
	}

	// The same live state seen fresh: the appended entries are real.
	fresh, err := h.ReadHistoryView(ctx, testSessionID)
	if err != nil {
		t.Fatalf("fresh ReadHistoryView: %v", err)
	}
	freshHeaders := fresh.HistoryHeaders()
	if len(freshHeaders) != len(beforeHeaders)+2 {
		t.Fatalf("fresh headers = %d, want the %d captured plus the two appended", len(freshHeaders), len(beforeHeaders))
	}
	if fresh.Revision.DurableRevision != beforeRevision.DurableRevision+1 {
		t.Fatalf("fresh revision = %+v, want the appended advance over %+v", fresh.Revision, beforeRevision)
	}
	if facts := fresh.HistoryFacts([]string{futureAssistantID, futureResultID}); len(facts) != 2 {
		t.Fatalf("fresh facts = %+v, want the two appended entries", facts)
	}
}

// TestSessionReadViewMemberPublications proves the background member rows of
// the local revision: an admission publishes the membership view and advances
// the counter once, a finish publishes the removal and advances it once, and
// a repeated finish is a no-op publication.
func TestSessionReadViewMemberPublications(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	read := func() SessionReadView {
		t.Helper()
		view, err := h.ReadSessionView(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("ReadSessionView: %v", err)
		}
		return view
	}
	before := read()
	if len(before.Background) != 0 || before.History.Revision.LocalRevision != 0 {
		t.Fatalf("baseline = %+v", before)
	}

	member, err := admitLocked(h, c, memberJob, "job-1", 0)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	afterAdmit := read()
	if len(afterAdmit.Background) != 1 || afterAdmit.Background[0] != (BackgroundMemberView{Kind: "job", ID: "job-1"}) {
		t.Fatalf("background after admission = %+v", afterAdmit.Background)
	}
	if afterAdmit.History.Revision.LocalRevision != before.History.Revision.LocalRevision+1 {
		t.Fatalf("local revision after admission = %d, want %d", afterAdmit.History.Revision.LocalRevision, before.History.Revision.LocalRevision+1)
	}

	claimed, ok := claimLocked(h, c, member.completionID)
	if !ok || claimed != member {
		t.Fatalf("claim failed")
	}
	h.finishBackgroundMember(c, claimed)
	afterFinish := read()
	if len(afterFinish.Background) != 0 {
		t.Fatalf("background after finish = %+v", afterFinish.Background)
	}
	if afterFinish.History.Revision.LocalRevision != afterAdmit.History.Revision.LocalRevision+1 {
		t.Fatalf("local revision after finish = %d, want %d", afterFinish.History.Revision.LocalRevision, afterAdmit.History.Revision.LocalRevision+1)
	}

	h.finishBackgroundMember(c, claimed) // already removed: a no-op publication
	afterRepeat := read()
	if afterRepeat.History.Revision.LocalRevision != afterFinish.History.Revision.LocalRevision {
		t.Fatalf("repeat finish advanced the revision to %d, want %d", afterRepeat.History.Revision.LocalRevision, afterFinish.History.Revision.LocalRevision)
	}
}

// TestSnapshotSessionSteeringDeliveryPublications proves the steering
// delivery's publication shape through the successor admission: the selected
// head stays buffered while its delivery attempt runs, a failed attempt drops
// it exactly once as the final outcome with no durable entry and no register
// advance, a parked delivery leaves snapshots available — the mid-admission
// read observes the coherent pending state, and the post-adoption read
// observes the entirely-new state with the removal riding the durable advance
// — and the pending fact follows each FIFO's own nonemptiness independently of
// the busy fact.
func TestSnapshotSessionSteeringDeliveryPublications(t *testing.T) {
	t.Run("failed delivery is a final dropped publication", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript(turn(testToolCall("call-1")))
		script.gate = make(chan struct{})
		h, cancel := newCancelableHarness(t, store, steeringPrepared(script), nil)
		defer cancel()
		defer script.releaseGate()
		session := createSession(t, h)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		receiveBounded(t, script.arrived, "first model boundary")
		baseline := readSessionView(t, h, session)

		if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "steer-me"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		buffered := readSessionView(t, h, session)
		if len(buffered.Steering) != 1 || buffered.Steering[0].OperationID != "op-2" ||
			buffered.Steering[0].Content[0].Text != "steer-me" {
			t.Fatalf("steering buffer after enqueue = %+v", buffered.Steering)
		}
		// Producer-level pending fact: the steering FIFO's own nonemptiness
		// alone raises HasPending (no queued member was submitted).
		if !buffered.History.HasPending || len(buffered.Queued) != 0 {
			t.Fatalf("steering-only pending fact = %v (steering %d, queued %d), want true from the steering FIFO alone",
				buffered.History.HasPending, len(buffered.Steering), len(buffered.Queued))
		}
		// one Submit's publications: the reservation, the enqueue, and its release
		if buffered.History.Revision.LocalRevision != baseline.History.Revision.LocalRevision+3 {
			t.Fatalf("enqueue revision = %d, want %d", buffered.History.Revision.LocalRevision, baseline.History.Revision.LocalRevision+3)
		}

		// The successor admission's entry insert fails once: the selected head
		// stays buffered through the attempt, is dropped exactly once as the
		// attempt's final outcome, and the register never adopts it.
		var once sync.Once
		deliveryAttempted := make(chan struct{})
		store.entryHook = func(draft EntryDraft) error {
			// the successor admission commits its admitted input entry, so the
			// delivery is recognized by its content
			if !strings.Contains(string(draft.Payload), "steer-me") {
				return nil
			}
			once.Do(func() { close(deliveryAttempted) })
			return errors.New("delivery failed")
		}
		script.releaseGate() // the boundary exits through the token; the drain's admission fails
		receiveBounded(t, deliveryAttempted, "failed steering delivery")
		awaitHarnessQuiet(t, h, session) // the drop settled and the drain converged

		after := readSessionView(t, h, session)
		if len(after.Steering) != 0 {
			t.Fatalf("steering buffer after the failed attempt = %+v, want the dropped head gone", after.Steering)
		}
		// Both FIFOs empty: the pending fact is false.
		if after.History.HasPending || len(after.Queued) != 0 {
			t.Fatalf("empty pending fact = %v (steering %d, queued %d), want false with both FIFOs empty",
				after.History.HasPending, len(after.Steering), len(after.Queued))
		}
		for _, fact := range readSessionFacts(t, h, session) {
			if fact.Input != nil && fact.Input.Content[0].Text == "steer-me" {
				t.Fatalf("the failed delivery committed entry %s", fact.EntryID)
			}
		}
		// the completion owner's reservation, the drop, the retirement and
		// the release: the predecessor's settlement is the one durable advance
		if after.History.Revision.LocalRevision != buffered.History.Revision.LocalRevision+4 {
			t.Fatalf("local revision after the failed attempt = %d, want %d (the reservation, the drop, the retirement and the release)",
				after.History.Revision.LocalRevision, buffered.History.Revision.LocalRevision+4)
		}
		sessionKey := RegisterKey{SessionID: session, Kind: RegisterSession}
		if after.Header.Revision.DurableRevision != registerRevision(t, store, session, sessionKey) {
			t.Fatalf("read revision %d disagrees with the durable register %d",
				after.Header.Revision.DurableRevision, registerRevision(t, store, session, sessionKey))
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
		if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationSuccess {
			t.Fatalf("op-1 = %q, want the quiet success preserved through the failed delivery", rec.State.Status)
		}
		if storedOperationExists(store, session, "op-2") {
			t.Fatalf("the failed delivery admitted the successor Operation")
		}
	})

	t.Run("the pending fact follows each FIFO independently of the busy fact", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript(turn(testToolCall("call-1")))
		script.gate = make(chan struct{})
		h, cancel := newCancelableHarness(t, store, steeringPrepared(script), nil)
		defer cancel()
		defer script.releaseGate()
		session := createSession(t, h)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		receiveBounded(t, script.arrived, "first model boundary")

		// Busy with both FIFOs empty: the run's own busy fact is not pending
		// work, so HasPending stays false while ExecutionBusy is true.
		busy := readSessionView(t, h, session)
		if !busy.History.ExecutionBusy || busy.History.HasPending || len(busy.Steering) != 0 || len(busy.Queued) != 0 {
			t.Fatalf("busy empty view = busy %v pending %v steering %d queued %d, want busy with no pending work",
				busy.History.ExecutionBusy, busy.History.HasPending, len(busy.Steering), len(busy.Queued))
		}

		// Queued-only: the queued FIFO's own nonemptiness alone raises the
		// pending fact with the steering FIFO still empty.
		if _, err := submitText(t, h, session, "op-2", MessageModeQueued, "queue-me"); err != nil {
			t.Fatalf("queued submit: %v", err)
		}
		queued := readSessionView(t, h, session)
		if len(queued.Steering) != 0 || len(queued.Queued) != 1 || !queued.History.HasPending {
			t.Fatalf("queued-only view = pending %v steering %d queued %d, want true from the queued FIFO alone",
				queued.History.HasPending, len(queued.Steering), len(queued.Queued))
		}
	})

	t.Run("a parked delivery keeps snapshots coherent until the adoption", func(t *testing.T) {
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
		baseline := readSessionView(t, h, session)
		if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "steer-me"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		buffered := readSessionView(t, h, session)
		if len(buffered.Steering) != 1 || buffered.History.Revision.LocalRevision != baseline.History.Revision.LocalRevision+3 {
			t.Fatalf("buffered view = %+v, want one steering item advanced by the Submit's three publications", buffered)
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

		// The parked admission runs under the completion owner's reservation:
		// the drain holds it from the settlement through the successor's
		// installation, so no competing admission can interleave.
		c.mu.Lock()
		held := c.reserved != nil
		c.mu.Unlock()
		if !held {
			releaseDelivery()
			receiveBounded(t, deliverySettled, "released steering transaction")
			t.Fatal("the parked delivery lost the completion owner's reservation")
		}

		// A read during the parked admission observes the pre-adoption state
		// exactly: the completion owner has settled the predecessor, the selected head
		// is still pending, and nothing of the successor is half-applied.
		type snapResult struct {
			view SessionReadView
			err  error
		}
		parkedSnap := make(chan snapResult, 1)
		go func() {
			view, err := h.ReadSessionView(context.Background(), session)
			parkedSnap <- snapResult{view, err}
		}()
		p := receiveBounded(t, parkedSnap, "mid-admission read")
		if p.err != nil {
			t.Fatalf("read during the parked admission: %v", p.err)
		}
		if len(p.view.Steering) != 1 || p.view.Steering[0].OperationID != "op-2" {
			t.Fatalf("mid-admission steering = %+v, want the selected head still pending", p.view.Steering)
		}
		if p.view.Header.CurrentOperationID != "" {
			t.Fatalf("mid-admission current operation = %q, want the settled predecessor cleared", p.view.Header.CurrentOperationID)
		}
		for _, op := range p.view.Operations {
			if op.OperationID == "op-2" {
				t.Fatalf("the parked admission exposed a half-applied successor Operation")
			}
		}
		releaseDelivery()
		receiveBounded(t, script.arrived, "post-adoption model boundary")
		// The arrival proves successor installation and request start only;
		// the completion owner's deferred release publishes its local
		// revision after the drain returns, so the exact publication count
		// reads only after that release.
		releaseWait, stopWait := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopWait()
		if err := c.waitIdle(releaseWait); err != nil {
			t.Fatalf("waiting for the completion owner's release: %v", err)
		}
		post := readSessionView(t, h, session)
		if len(post.Steering) != 0 {
			t.Fatalf("steering buffer after the adoption = %+v", post.Steering)
		}
		found := false
		for _, fact := range readSessionFacts(t, h, session) {
			if fact.Input != nil && fact.Input.Content[0].Text == "steer-me" {
				found = true
				if fact.OperationID != "op-2" {
					t.Fatalf("the adopted delivery's input is owned by %q, want its own Operation op-2", fact.OperationID)
				}
			}
		}
		if !found {
			t.Fatalf("the adopted delivery's input fact is missing")
		}
		if post.Header.Revision.DurableRevision <= beforeAdoption {
			t.Fatalf("read revision %d did not advance past the pre-adoption register %d",
				post.Header.Revision.DurableRevision, beforeAdoption)
		}
		// The selected head stayed buffered through the parked attempt; the
		// durable adoption advanced only the register — the removal rode it,
		// publishing nothing of its own. The completion owner's reservation,
		// the successor installation and its release are the delivery cycle's
		// local publications, the last visible only after the release waited
		// for above.
		if post.History.Revision.LocalRevision != buffered.History.Revision.LocalRevision+3 {
			t.Fatalf("post-adoption local revision = %d, want %d (the reservation, the installation and the release)",
				post.History.Revision.LocalRevision, buffered.History.Revision.LocalRevision+3)
		}
		if !post.History.ExecutionBusy {
			t.Fatalf("post-adoption read = busy %v, want the admitted successor Operation", post.History.ExecutionBusy)
		}
	})
}

// TestSnapshotOwnerWaitsForReservationRelease proves the release barrier the
// steering-delivery owner's exact publication counts depend on: waitIdle is
// the reservation release wait — it reports a bounded caller context instead
// of hanging while the reservation is held and returns once released — and
// that release is exactly one local publication, so a count read before it
// observes one publication fewer.
func TestSnapshotOwnerWaitsForReservationRelease(t *testing.T) {
	store := emptyStore(t)
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, nil)
	defer cancel()
	session := createSession(t, h)
	c := cachedCoordinator(t, h, session)

	release, err := h.reserve(context.Background(), c)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	held, err := h.ReadSessionView(context.Background(), session)
	if err != nil {
		t.Fatalf("read under the held reservation: %v", err)
	}
	bounded, stopWait := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stopWait()
	if err := c.waitIdle(bounded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitIdle under a held reservation = %v, want the bounded caller deadline", err)
	}
	release()
	if err := c.waitIdle(context.Background()); err != nil {
		t.Fatalf("waitIdle after the release: %v", err)
	}
	free, err := h.ReadSessionView(context.Background(), session)
	if err != nil {
		t.Fatalf("read after the release: %v", err)
	}
	if free.History.Revision.LocalRevision != held.History.Revision.LocalRevision+1 {
		t.Fatalf("local revision across the release = %d, want %d: the release is one publication, so counts read after it",
			free.History.Revision.LocalRevision, held.History.Revision.LocalRevision+1)
	}
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
	for _, tc := range []struct {
		mode MessageMode
		want SubmitDisposition
	}{
		{MessageModeQueued, DispositionQueued},
		{MessageModeRegular, DispositionSteering},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
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
				result, err := submitText(t, h, session, "buffered", tc.mode, text)
				if err != nil || result.Disposition != tc.want {
					t.Fatalf("buffer %q = %+v, %v, want disposition %s", text, result, err, tc.want)
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
		})
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
