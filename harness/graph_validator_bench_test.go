package harness

import (
	"context"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// benchValidatorFixture assembles a coherent Session of n terminal message
// Operations plus one running Operation, each owning the entry kinds a real
// Operation commits: its input, an assistant entry publishing one tool call,
// the call's terminal result, and the Operation's settlement. The running
// Operation carries one unresolved published call, so the running-side checks
// stay on the measured path too.
func benchValidatorFixture(n int) *testGraph {
	fixture := &testGraph{session: validSessionRecord()}
	stamped := testTime
	entries := make([]testEntry, 0, 4*n+2)
	for i := 0; i < n; i++ {
		opID := fmt.Sprintf("op-%d", i)
		seq := int64(4*i + 1)
		op := validOperationRecord()
		op.Admission.OperationID = opID
		op.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(int(seq))}
		op.State.Status = OperationSuccess
		op.State.SettledAt = &stamped
		op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(int(seq + 3))}}
		fixture.ops = append(fixture.ops, op)

		input := validInputEntry(opID)
		input.EntryID = hexID(int(seq))
		assistant := validAssistantEntry(opID)
		assistant.EntryID = hexID(int(seq + 1))
		call := validToolCallRecord()
		call.ID = fmt.Sprintf("call-%d", i)
		call.ResultEntryID = hexID(int(seq + 2))
		assistant.ToolCalls = []toolCallRecord{call}
		result := validToolResultEntry(opID)
		result.EntryID = hexID(int(seq + 2))
		result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(int(seq + 1))}
		result.ToolCallID = call.ID
		settlement := validSettlementEntry()
		settlement.EntryID = hexID(int(seq + 3))
		settlement.OperationID = opID
		entries = append(entries,
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(int(seq)), OperationID: opID, Kind: EntryInput, Sequence: seq, CommittedAt: testTime}, input: &input},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(int(seq + 1)), OperationID: opID, Kind: EntryAssistant, Sequence: seq + 1, CommittedAt: testTime}, assistant: &assistant},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(int(seq + 2)), OperationID: opID, Kind: EntryToolResult, Sequence: seq + 2, CommittedAt: testTime}, toolResult: &result},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(int(seq + 3)), OperationID: opID, Kind: EntryOperationSettlement, Sequence: seq + 3, CommittedAt: testTime}, settlement: &settlement},
		)
	}

	runSeq := int64(4*n + 1)
	running := validOperationRecord()
	running.Admission.OperationID = "op-running"
	running.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(int(runSeq))}
	running.State.PendingToolCalls = []PendingToolCall{{
		AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(int(runSeq + 1))},
		CallID:         "call-running",
		ResultEntryID:  hexID(int(runSeq + 2)),
	}}
	fixture.ops = append(fixture.ops, running)
	fixture.session.State.CurrentOperationID = "op-running"

	runInput := validInputEntry("op-running")
	runInput.EntryID = hexID(int(runSeq))
	runAssistant := validAssistantEntry("op-running")
	runAssistant.EntryID = hexID(int(runSeq + 1))
	runCall := validToolCallRecord()
	runCall.ID = "call-running"
	runCall.ResultEntryID = hexID(int(runSeq + 2))
	runAssistant.ToolCalls = []toolCallRecord{runCall}
	entries = append(entries,
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(int(runSeq)), OperationID: "op-running", Kind: EntryInput, Sequence: runSeq, CommittedAt: testTime}, input: &runInput},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(int(runSeq + 1)), OperationID: "op-running", Kind: EntryAssistant, Sequence: runSeq + 1, CommittedAt: testTime}, assistant: &runAssistant},
	)
	fixture.entries = entries
	return fixture
}

// BenchmarkGraphValidatorOperationShapes measures the invariant traversal at
// 250, 1,000 and 4,000 Operations. It is scaling evidence only: no threshold
// and no CI gate. Each size is decoded once through the real storage path, and
// the loop measures validateGraphInvariants, the pass whose per-Operation
// scans read each Operation's entry group instead of rescanning the whole
// history.
func BenchmarkGraphValidatorOperationShapes(b *testing.B) {
	for _, n := range []int{250, 1000, 4000} {
		b.Run(fmt.Sprintf("%d_operations", n), func(b *testing.B) {
			fixture := benchValidatorFixture(n)
			graph, err := validateSessionGraph(context.Background(), fixture.storage(b), testSessionID)
			if err != nil {
				b.Fatalf("benchmark fixture rejected: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := validateGraphInvariants(testSessionID, graph); err != nil {
					b.Fatalf("benchmark graph rejected: %v", err)
				}
			}
		})
	}
}

// benchValidatorUsageFixture extends benchValidatorFixture so every terminal
// Operation's settlement carries usage under its own model identity: K = n
// distinct model identities, one per Operation, and a Session total with one
// entry per Operation. It exercises the usage-total accumulation over distinct
// models, where model identities change between Operations.
func benchValidatorUsageFixture(n int) *testGraph {
	fixture := benchValidatorFixture(n)
	sessionTotals := UsageTotals{ByModel: make([]ModelUsage, 0, n)}
	for i := 0; i < n; i++ {
		ref := model.ModelRef{Provider: "bench", Model: fmt.Sprintf("model-%d", i)}
		counts := UsageCount{InputTokens: int64(i + 1)}
		settlement := fixture.entries[4*i+3].settlement
		settlement.Model = &ref
		settlement.Usage = &counts
		fixture.ops[i].State.Usage = UsageTotals{ByModel: []ModelUsage{{Model: ref, Usage: counts}}}
		sessionTotals.ByModel = append(sessionTotals.ByModel, ModelUsage{Model: ref, Usage: counts})
	}
	sortUsageTotals(sessionTotals.ByModel)
	fixture.session.State.Usage = sessionTotals
	return fixture
}

// BenchmarkGraphValidatorUsageShapes measures the same invariant traversal at
// 250, 1,000 and 4,000 Operations with per-Operation settlement usage under a
// distinct model identity per Operation, so the Session-total accumulation
// walks n distinct models. Scaling evidence only: no threshold and no CI
// gate.
func BenchmarkGraphValidatorUsageShapes(b *testing.B) {
	for _, n := range []int{250, 1000, 4000} {
		b.Run(fmt.Sprintf("%d_operations", n), func(b *testing.B) {
			fixture := benchValidatorUsageFixture(n)
			graph, err := validateSessionGraph(context.Background(), fixture.storage(b), testSessionID)
			if err != nil {
				b.Fatalf("benchmark fixture rejected: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := validateGraphInvariants(testSessionID, graph); err != nil {
					b.Fatalf("benchmark graph rejected: %v", err)
				}
			}
		})
	}
}
