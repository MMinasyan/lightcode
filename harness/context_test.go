package harness

import (
	"context"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// newProjectionFixture validates the fixture graph into a live coordinator so
// the context source projects from the landed validated view.
func newProjectionFixture(t *testing.T, fixture *testGraph) (*Harness, *coordinator) {
	t.Helper()
	h := newTestHarness(t, fixture.storage(t), nil)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return h, c
}

// projectionAssistant returns one assistant fixture entry with the given
// identity and text.
func projectionAssistant(entryID, text string, sequence int64) testEntry {
	v := validAssistantEntry(testOpID)
	v.EntryID = entryID
	v.Content = []model.ContentPart{{Kind: model.PartText, Text: text}}
	return testEntry{
		env:       Entry{SessionID: testSessionID, ID: entryID, OperationID: testOpID, Kind: EntryAssistant, Sequence: sequence, CommittedAt: testTime},
		assistant: &v,
	}
}

// projectionCompaction returns one compaction fixture entry with the given
// identity, boundary, and sequence.
func projectionCompaction(entryID, boundaryID string, sequence int64) testEntry {
	v := validCompactionEntry(testOpID)
	v.EntryID = entryID
	v.BoundaryEntryID = boundaryID
	return testEntry{
		env:        Entry{SessionID: testSessionID, ID: entryID, OperationID: testOpID, Kind: EntryCompaction, Sequence: sequence, CommittedAt: testTime},
		compaction: &v,
	}
}

// projectionFollowUp returns one settled follow-up message Operation and its
// admitted input entry with the given identities and text — the split
// Operation shape every later delivered input takes, so fixture graphs keep
// the one admitted input per message Operation rule.
func projectionFollowUp(entryID, operationID, text, settlementID string, sequence, settlementSequence int64) (testEntry, testEntry, OperationRecord) {
	input := validInputEntry(operationID)
	input.EntryID = entryID
	input.Content = []model.ContentPart{{Kind: model.PartText, Text: text}}
	stamped := testTime
	settlement := validSettlementEntry()
	settlement.EntryID = settlementID
	settlement.OperationID = operationID
	settlement.Status = OperationSuccess
	op := validOperationRecord()
	op.Admission.OperationID = operationID
	op.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: entryID}
	op.State.Status = OperationSuccess
	op.State.SettledAt = &stamped
	op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: settlementID}}
	return testEntry{
			env:   Entry{SessionID: testSessionID, ID: entryID, OperationID: operationID, Kind: EntryInput, Sequence: sequence, CommittedAt: testTime},
			input: &input,
		}, testEntry{
			env:        Entry{SessionID: testSessionID, ID: settlementID, OperationID: operationID, Kind: EntryOperationSettlement, Sequence: settlementSequence, CommittedAt: testTime},
			settlement: &settlement,
		}, op
}

// projectionMessages calls the context source once and returns its messages.
// The bare run is the first-request projection shape: these fixtures carry no
// steering, so no handoff is ever attempted.
func projectionMessages(t *testing.T, h *Harness, c *coordinator) []model.Message {
	t.Helper()
	msgs, err := h.contextSource(c, &activeExecution{execCtx: context.Background()}, testOpID)(context.Background())
	if err != nil {
		t.Fatalf("context source: %v", err)
	}
	return msgs
}

// wantSummaryMessage builds the exact summary message the projection must
// carry for the given compaction payload.
func wantSummaryMessage(t *testing.T, compaction compactionEntry) model.Message {
	t.Helper()
	msg, err := model.NewMessage(model.Message{
		Role:    model.RoleAssistant,
		Source:  compaction.Model,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\n" + compaction.Summary + "\n\n[End of summary. Continue from here.]"}},
	})
	if err != nil {
		t.Fatalf("summary message: %v", err)
	}
	return msg
}

// assertProjectionEqual compares the projected messages against the expected
// list by value: the all-exported struct and the exact expected slice carry
// the complete assertion, including the absence of any second summary.
func assertProjectionEqual(t *testing.T, got, want []model.Message) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projected messages = %+v, want %+v", got, want)
	}
}

// TestContextSourceSummarizedProjection proves the summarized projection: with
// the Session register's CompactionEntryID set, the projected messages are
// exactly the system message, one summary message carrying the payload's
// verbatim framing and model identity, and the post-boundary entries in
// sequence order — nothing at or before the boundary projects.
func TestContextSourceSummarizedProjection(t *testing.T) {
	fixture := validTestGraph()
	fixture.entries = append(fixture.entries, projectionCompaction(hexID(3), hexID(2), 3))
	followInput, followSettlement, followOp := projectionFollowUp(hexID(4), "op-2", "second", hexID(6), 4, 6)
	later := projectionAssistant(hexID(5), "later", 5)
	later.env.OperationID = "op-2"
	later.assistant.OperationID = "op-2"
	fixture.entries = append(fixture.entries, followInput, later, followSettlement)
	fixture.ops = append(fixture.ops, followOp)
	fixture.session.State.CompactionEntryID = hexID(3)
	h, c := newProjectionFixture(t, fixture)

	got := projectionMessages(t, h, c)
	want := []model.Message{
		mustSystemMessage(t, "system"),
		wantSummaryMessage(t, *fixture.entries[2].compaction),
		mustInputMessage(t, "second"),
		mustAssistantMessage(t, "later"),
	}
	assertProjectionEqual(t, got, want)
}

// TestContextSourceUncompactedProjectionGolden proves the empty-field
// projection is byte-identical to the pre-change behavior: the full history
// projects and the committed compaction entry (present in the graph, unnamed
// by the register) still projects no message.
func TestContextSourceUncompactedProjectionGolden(t *testing.T) {
	fixture := validTestGraph()
	fixture.entries = append(fixture.entries, projectionCompaction(hexID(3), hexID(2), 3))
	h, c := newProjectionFixture(t, fixture)

	got := projectionMessages(t, h, c)
	want := []model.Message{
		mustSystemMessage(t, "system"),
		mustInputMessage(t, "hello"),
		mustAssistantMessage(t, "hi"),
	}
	assertProjectionEqual(t, got, want)
}

// TestContextSourceTwoSequentialCompactions proves two sequential compaction
// entries: only the register-named entry's summary projects — the earlier
// compaction entry produces no second summary — and the entries after the
// named boundary alone project.
func TestContextSourceTwoSequentialCompactions(t *testing.T) {
	fixture := validTestGraph()
	fixture.entries = append(fixture.entries, projectionCompaction(hexID(3), hexID(2), 3))
	secondInput, secondSettlement, secondOp := projectionFollowUp(hexID(4), "op-2", "second", hexID(7), 4, 7)
	fixture.entries = append(fixture.entries, secondInput, projectionCompaction(hexID(5), hexID(4), 5))
	thirdInput, thirdSettlement, thirdOp := projectionFollowUp(hexID(6), "op-3", "third", hexID(8), 6, 8)
	fixture.entries = append(fixture.entries, thirdInput, thirdSettlement, secondSettlement)
	fixture.ops = append(fixture.ops, secondOp, thirdOp)
	fixture.session.State.CompactionEntryID = hexID(5)
	second := *fixture.entries[4].compaction
	second.Summary = "Second summary."
	second.Model = model.ModelRef{Provider: "other", Model: "sum-2"}
	fixture.entries[4].compaction = &second
	h, c := newProjectionFixture(t, fixture)

	got := projectionMessages(t, h, c)
	want := []model.Message{
		mustSystemMessage(t, "system"),
		wantSummaryMessage(t, second),
		mustInputMessage(t, "third"),
	}
	assertProjectionEqual(t, got, want)
}

// projectionSignal returns one operationless background_completion signal
// fixture entry with the given identity and sequence — the exact shape a
// closed-path completion commits.
func projectionSignal(entryID string, sequence int64) testEntry {
	v := validBackgroundCompletionEntry()
	v.EntryID = entryID
	return testEntry{
		env:    Entry{SessionID: testSessionID, ID: entryID, Kind: EntrySignal, Sequence: sequence, CommittedAt: testTime},
		signal: &v,
	}
}

// TestContextSourceProjectsEntriesAfterTheBoundary proves the frozen-boundary
// projection rule: the named compaction entry's summary projects, then exactly
// the entries after the sequence of the boundary entry its payload records —
// an entry committed between the boundary target and the compaction entry
// still projects — and a compaction-kind boundary target hides every message
// kind, leaving the summary alone.
func TestContextSourceProjectsEntriesAfterTheBoundary(t *testing.T) {
	t.Run("the entry between the boundary target and the compaction projects", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.entries = append(fixture.entries, projectionSignal(hexID(3), 3), projectionCompaction(hexID(4), hexID(2), 4))
		laterInput, laterSettlement, laterOp := projectionFollowUp(hexID(5), "op-2", "later", hexID(6), 5, 6)
		fixture.entries = append(fixture.entries, laterInput, laterSettlement)
		fixture.ops = append(fixture.ops, laterOp)
		fixture.session.State.CompactionEntryID = hexID(4)
		h, c := newProjectionFixture(t, fixture)

		got := projectionMessages(t, h, c)
		want := []model.Message{
			mustSystemMessage(t, "system"),
			wantSummaryMessage(t, *fixture.entries[3].compaction),
			mustInputMessage(t, "<system-signal>Task completed.</system-signal>"),
			mustInputMessage(t, "later"),
		}
		assertProjectionEqual(t, got, want)
	})

	t.Run("a compaction-kind boundary target projects only the summary", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.entries = append(fixture.entries,
			projectionCompaction(hexID(3), hexID(2), 3),
			projectionCompaction(hexID(4), hexID(3), 4),
		)
		fixture.session.State.CompactionEntryID = hexID(4)
		second := *fixture.entries[3].compaction
		second.Summary = "Second summary."
		second.Model = model.ModelRef{Provider: "other", Model: "sum-2"}
		fixture.entries[3].compaction = &second
		h, c := newProjectionFixture(t, fixture)

		got := projectionMessages(t, h, c)
		want := []model.Message{
			mustSystemMessage(t, "system"),
			wantSummaryMessage(t, second),
		}
		assertProjectionEqual(t, got, want)
	})
}

// mustSystemMessage builds the expected system message for the text.
func mustSystemMessage(t *testing.T, text string) model.Message {
	t.Helper()
	msg, err := model.NewMessage(model.Message{
		Role:    model.RoleSystem,
		Content: []model.ContentPart{{Kind: model.PartText, Text: text}},
	})
	if err != nil {
		t.Fatalf("system message: %v", err)
	}
	return msg
}

// mustInputMessage builds the expected user message for the input text.
func mustInputMessage(t *testing.T, text string) model.Message {
	t.Helper()
	msg, err := model.NewMessage(model.Message{
		Role:    model.RoleUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: text}},
	})
	if err != nil {
		t.Fatalf("input message: %v", err)
	}
	return msg
}

// mustAssistantMessage builds the expected assistant message for the text.
func mustAssistantMessage(t *testing.T, text string) model.Message {
	t.Helper()
	msg, err := model.NewMessage(model.Message{
		Role:    model.RoleAssistant,
		Source:  testModelRef(),
		Content: []model.ContentPart{{Kind: model.PartText, Text: text}},
	})
	if err != nil {
		t.Fatalf("assistant message: %v", err)
	}
	return msg
}
