package harness

import (
	"context"
	"math"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// Read-time approximate cost estimate oracles over the shared committed
// history fixture: every original producer is priced exactly once under its
// owning Operation's captured pricing, unknown is contagious, and copied or
// informational history never charges.

func rate(v float64) *float64 { return &v }

// conversationPricing/compactPricing are the fixture's captured rate pair:
// the conversation model at 1/2/4/8 per million and the effective compact
// model (cprov/compact-x) at 0.5/0.25/0.125/0.0625 per million.
func conversationPricing() *ModelPricing {
	return &ModelPricing{Input: rate(1), Output: rate(2), CacheRead: rate(4), CacheWrite: rate(8)}
}

func compactPricing() *ModelPricing {
	return &ModelPricing{Input: rate(0.5), Output: rate(0.25), CacheRead: rate(0.125), CacheWrite: rate(0.0625)}
}

// recomputeFixtureUsage rewrites the operation and Session register usage
// totals so they exactly agree with the fixture's usage-bearing entries, the
// same agreement the graph validator enforces.
func recomputeFixtureUsage(g *testGraph) {
	sessionCounts := make(map[model.ModelRef]UsageCount)
	perOperation := make(map[string]map[model.ModelRef]UsageCount)
	record := func(operationID string, ref model.ModelRef, counts UsageCount) {
		if operationID != "" {
			opCounts := perOperation[operationID]
			if opCounts == nil {
				opCounts = make(map[model.ModelRef]UsageCount)
				perOperation[operationID] = opCounts
			}
			opCounts[ref] = mustAddUsage(opCounts[ref], counts)
		}
		sessionCounts[ref] = mustAddUsage(sessionCounts[ref], counts)
	}
	for _, entry := range g.entries {
		if entry.assistant != nil && entry.assistant.Usage != nil {
			record(entry.env.OperationID, entry.assistant.Source, *entry.assistant.Usage)
		}
		if entry.settlement != nil && entry.settlement.Usage != nil {
			record(entry.env.OperationID, *entry.settlement.Model, *entry.settlement.Usage)
		}
		var facts []maintenanceFact
		if entry.compaction != nil {
			facts = entry.compaction.Facts
		} else if entry.settlement != nil {
			facts = entry.settlement.Facts
		}
		for _, fact := range facts {
			if fact.Usage != nil {
				record(entry.env.OperationID, fact.Model, *fact.Usage)
			}
		}
	}
	materialize := func(counts map[model.ModelRef]UsageCount) UsageTotals {
		out := UsageTotals{ByModel: make([]ModelUsage, 0, len(counts))}
		for ref, usage := range counts {
			out.ByModel = append(out.ByModel, ModelUsage{Model: ref, Usage: usage})
		}
		sortUsageTotals(out.ByModel)
		return out
	}
	for i := range g.ops {
		g.ops[i].State.Usage = materialize(perOperation[g.ops[i].Admission.OperationID])
	}
	g.session.State.Usage = materialize(sessionCounts)
}

func mustAddUsage(a, b UsageCount) UsageCount {
	sum, err := addUsageCount(a, b)
	if err != nil {
		panic(err)
	}
	return sum
}

// readEstimates materializes the fixture through the public read and returns
// the Session estimate plus each Operation summary's estimate, keyed by
// Operation identity.
func readEstimates(t *testing.T, g *testGraph) (map[string]*float64, *float64) {
	t.Helper()
	recomputeFixtureUsage(g)
	h := newTestHarness(t, g.storage(t), nil)
	view, err := h.ReadSessionView(context.Background(), testSessionID)
	testassert.NoError(t, err)
	estimates := make(map[string]*float64, len(view.Operations))
	for _, summary := range view.Operations {
		estimates[summary.OperationID] = summary.EstimatedCostUSD
	}
	return estimates, view.EstimatedCostUSD
}

func ptrUSD(v float64) *float64 { return &v }

// assertEstimate compares approximate float64 estimates: an unknown estimate
// is a nil pointer, and a known one must match within the approximation the
// contract itself declares.
func assertEstimate(t *testing.T, label string, got *float64, want *float64) {
	t.Helper()
	switch {
	case want == nil && got == nil:
	case want == nil:
		t.Fatalf("%s estimate = %v, want unknown (nil)", label, *got)
	case got == nil:
		t.Fatalf("%s estimate = unknown, want %v", label, *want)
	default:
		testassert.Require(t, math.Abs(*got-*want) <= 1e-9, "%s estimate = %v, want %v", label, *got, *want)
	}
}

func TestSessionReadViewCostEstimates(t *testing.T) {
	compactRef := testCompactCapture().Model
	type estimateCase struct {
		name        string
		mutate      func(g *testGraph)
		wantOp      map[string]*float64
		wantSession *float64
	}
	cases := []estimateCase{
		{
			// Original assistant producers and one checkpoint fact are priced
			// exactly once each under the captures' own rates; the plain
			// success settlements name no model producer and add nothing; the
			// operationless background_completion signal contributes nothing.
			name: "reported producers priced once under captured rates",
			mutate: func(g *testGraph) {
				for _, entry := range g.entries {
					if entry.assistant != nil && entry.assistant.EntryID == hexID(2) {
						entry.assistant.Usage = &UsageCount{InputTokens: 10, CachedInputTokens: 10, OutputTokens: 10}
					}
				}
			},
			// op1: (10+40+20)/1e6 + (3+4+4)/1e6 = 81e-6; op-compact: (1+4+2)/1e6 = 7e-6.
			wantOp:      map[string]*float64{testOpID: ptrUSD(81e-6), "op-compact": ptrUSD(7e-6)},
			wantSession: ptrUSD(88e-6),
		},
		{
			name: "one reportless assistant producer makes its operation and the session unknown",
			// The fixture's first assistant reports no usage: op1 is unknown
			// even though its second assistant and the compact fact are known,
			// and the Session sum is never presented as a partial-known total.
			mutate:  func(*testGraph) {},
			wantOp:  map[string]*float64{testOpID: nil, "op-compact": ptrUSD(7e-6)},
			wantSession: nil,
		},
		{
			name: "no-output settlement model with usage is priced once under the conversation rate",
			mutate: func(g *testGraph) {
				for _, entry := range g.entries {
					switch entry.env.ID {
					case hexID(2):
						entry.assistant.Usage = &UsageCount{InputTokens: 10, CachedInputTokens: 10, OutputTokens: 10}
					case hexID(7): // the terminal settlement carries its model producer's usage
						ref := testModelRef()
						entry.settlement.Model = &ref
						entry.settlement.Usage = &UsageCount{InputTokens: 1, CachedInputTokens: 1, OutputTokens: 1}
					}
				}
			},
			wantOp:      map[string]*float64{testOpID: ptrUSD(88e-6), "op-compact": ptrUSD(7e-6)},
			wantSession: ptrUSD(95e-6),
		},
		{
			name: "no-output settlement model without its usage report stays unknown, not zero",
			mutate: func(g *testGraph) {
				for _, entry := range g.entries {
					switch entry.env.ID {
					case hexID(2):
						entry.assistant.Usage = &UsageCount{InputTokens: 10, CachedInputTokens: 10, OutputTokens: 10}
					case hexID(7): // the model producer is named but reports no usage
						ref := testModelRef()
						entry.settlement.Model = &ref
					}
				}
			},
			wantOp:      map[string]*float64{testOpID: nil, "op-compact": ptrUSD(7e-6)},
			wantSession: nil,
		},
		{
			// The same model identity priced by two captures proves the
			// estimate reads captures, never one shared current price; the
			// Runtime config-reload sibling proves current-rate edits never
			// reprice an earlier capture.
			name: "two operations on the same model price through their own captures",
			mutate: func(g *testGraph) {
				g.ops[0].Admission.Execution.Pricing = &ModelPricing{Input: rate(1)}
				g.ops[1].Admission.Execution.Pricing = &ModelPricing{Input: rate(3)}
				for _, entry := range g.entries {
					switch entry.env.ID {
					case hexID(2):
						entry.assistant.Usage = &UsageCount{InputTokens: 1000000}
					case hexID(5):
						entry.assistant.Usage = &UsageCount{}
					case hexID(8):
						entry.compaction.Facts[0].Usage = &UsageCount{InputTokens: 1000000}
					}
				}
			},
			wantOp:      map[string]*float64{testOpID: ptrUSD(1), "op-compact": ptrUSD(3)},
			wantSession: ptrUSD(4),
		},
		{
			// A maintenance producer is priced by its own model identity:
			// compact-model facts take the captured compact rates (not the
			// conversation rates), and facts settling on the checkpoint and
			// on the settlement each count exactly once.
			name: "maintenance facts use the effective maintenance model's own captured price",
			mutate: func(g *testGraph) {
				input := []model.Message{{Role: model.RoleSystem, Content: []model.ContentPart{{Kind: model.PartText, Text: "compact prompt"}}}}
				for _, entry := range g.entries {
					switch entry.env.ID {
					case hexID(2):
						entry.assistant.Usage = &UsageCount{InputTokens: 10, CachedInputTokens: 10, OutputTokens: 10}
					case hexID(8):
						entry.compaction.Facts[0].Usage = &UsageCount{InputTokens: 1000000} // conversation identity at the conversation rate: 1
						entry.compaction.Facts = append(entry.compaction.Facts, maintenanceFact{
							RequestID: "maintenance-two",
							Model:     compactRef,
							Input:     input,
							Usage:     &UsageCount{InputTokens: 1000000}, // compact identity at the compact rate: 0.5
						})
					case hexID(9):
						entry.settlement.Facts = []maintenanceFact{{
							RequestID: "maintenance-three",
							Model:     compactRef,
							Input:     input,
							Usage:     &UsageCount{InputTokens: 2000000}, // the settlement-carried fact adds 1
						}}
					}
				}
			},
			wantOp:      map[string]*float64{testOpID: ptrUSD(81e-6), "op-compact": ptrUSD(2.5)},
			wantSession: ptrUSD(2.500081),
		},
		{
			name: "a maintenance fact model matching no captured model is unknown, not priced as another model",
			mutate: func(g *testGraph) {
				for _, entry := range g.entries {
					switch entry.env.ID {
					case hexID(2):
						entry.assistant.Usage = &UsageCount{InputTokens: 10, CachedInputTokens: 10, OutputTokens: 10}
					case hexID(8):
						entry.compaction.Facts[0].Model = model.ModelRef{Provider: "prov", Model: "unpriced"}
					}
				}
			},
			wantOp:      map[string]*float64{testOpID: ptrUSD(81e-6), "op-compact": nil},
			wantSession: nil,
		},
		{
			name: "a reported fact followed by a reportless fact yields unknown, not a partial total",
			mutate: func(g *testGraph) {
				for _, entry := range g.entries {
					switch entry.env.ID {
					case hexID(2):
						entry.assistant.Usage = &UsageCount{InputTokens: 10, CachedInputTokens: 10, OutputTokens: 10}
					case hexID(8):
						entry.compaction.Facts = append(entry.compaction.Facts, maintenanceFact{
							RequestID: "maintenance-two",
							Model:     compactRef,
							Input:     entry.compaction.Facts[0].Input,
						}) // usage absent: unknown, never skipped in favour of the known sibling
					}
				}
			},
			wantOp:      map[string]*float64{testOpID: ptrUSD(81e-6), "op-compact": nil},
			wantSession: nil,
		},
		{
			name: "present all-zero reports with absent pricing are a known zero",
			mutate: func(g *testGraph) {
				for i := range g.ops {
					g.ops[i].Admission.Execution.Pricing = nil
					g.ops[i].Admission.Execution.Compact.Pricing = nil
				}
				for _, entry := range g.entries {
					switch entry.env.ID {
					case hexID(2), hexID(5):
						entry.assistant.Usage = &UsageCount{}
					case hexID(8):
						entry.compaction.Facts[0].Usage = &UsageCount{}
					}
				}
			},
			// No capture prices anything here, yet every report is present and
			// zero: both operations and the Session are known zeros.
			wantOp:      map[string]*float64{testOpID: ptrUSD(0), "op-compact": ptrUSD(0)},
			wantSession: ptrUSD(0),
		},
		{
			name: "present zero rates price nonzero counts to a known zero",
			mutate: func(g *testGraph) {
				zero := &ModelPricing{Input: rate(0), Output: rate(0), CacheRead: rate(0)}
				for i := range g.ops {
					g.ops[i].Admission.Execution.Pricing = zero
					g.ops[i].Admission.Execution.Compact.Pricing = zero
				}
				for _, entry := range g.entries {
					if entry.assistant != nil && entry.assistant.EntryID == hexID(2) {
						entry.assistant.Usage = &UsageCount{InputTokens: 10, CachedInputTokens: 10, OutputTokens: 10}
					}
				}
			},
			wantOp:      map[string]*float64{testOpID: ptrUSD(0), "op-compact": ptrUSD(0)},
			wantSession: ptrUSD(0),
		},
		{
			name: "a present rate for the only nonzero counter prices the producer; its absent counterparts are irrelevant",
			mutate: func(g *testGraph) {
				for i := range g.ops {
					g.ops[i].Admission.Execution.Pricing = &ModelPricing{CacheRead: rate(4)}
					g.ops[i].Admission.Execution.Compact.Pricing = &ModelPricing{CacheRead: rate(4)}
				}
				for _, entry := range g.entries {
					switch entry.env.ID {
					case hexID(2):
						entry.assistant.Usage = &UsageCount{CachedInputTokens: 10}
					case hexID(5):
						entry.assistant.Usage = &UsageCount{CachedInputTokens: 1}
					case hexID(8):
						entry.compaction.Facts[0].Usage = &UsageCount{CachedInputTokens: 1}
					}
				}
			},
			wantOp:      map[string]*float64{testOpID: ptrUSD(44e-6), "op-compact": ptrUSD(4e-6)},
			wantSession: ptrUSD(48e-6),
		},
		{
			name: "a nonzero counter with its rate missing is unknown even beside a present cache rate",
			mutate: func(g *testGraph) {
				for i := range g.ops {
					g.ops[i].Admission.Execution.Pricing = &ModelPricing{CacheRead: rate(4)}
					g.ops[i].Admission.Execution.Compact.Pricing = &ModelPricing{CacheRead: rate(4)}
				}
				for _, entry := range g.entries {
					switch entry.env.ID {
					case hexID(2):
						entry.assistant.Usage = &UsageCount{InputTokens: 10, CachedInputTokens: 10, OutputTokens: 10}
					case hexID(8):
						entry.compaction.Facts[0].Usage = &UsageCount{CachedInputTokens: 1}
					}
				}
			},
			wantOp:      map[string]*float64{testOpID: nil, "op-compact": ptrUSD(4e-6)},
			wantSession: nil,
		},
		{
			name: "a captured cache-write rate alone never invents a charged count",
			mutate: func(g *testGraph) {
				for i := range g.ops {
					g.ops[i].Admission.Execution.Pricing = &ModelPricing{CacheWrite: rate(9)}
					g.ops[i].Admission.Execution.Compact.Pricing = &ModelPricing{CacheWrite: rate(9)}
				}
				for _, entry := range g.entries {
					switch entry.env.ID {
					case hexID(2), hexID(5):
						entry.assistant.Usage = &UsageCount{}
					case hexID(8):
						entry.compaction.Facts[0].Usage = &UsageCount{}
					}
				}
			},
			// Only all-zero reports are priced here: the captured cache-write
			// rate contributes to no count, so the totals are known zeros.
			wantOp:      map[string]*float64{testOpID: ptrUSD(0), "op-compact": ptrUSD(0)},
			wantSession: ptrUSD(0),
		},
		{
			name: "a non-finite per-producer estimate is unknown",
			mutate: func(g *testGraph) {
				for i := range g.ops {
					g.ops[i].Admission.Execution.Pricing = &ModelPricing{Input: rate(1e300)}
					g.ops[i].Admission.Execution.Compact.Pricing = nil
				}
				for _, entry := range g.entries {
					switch entry.env.ID {
					case hexID(2):
						entry.assistant.Usage = &UsageCount{InputTokens: math.MaxInt64} // the product overflows to +Inf
					case hexID(5):
						entry.assistant.Usage = &UsageCount{}
					case hexID(8):
						entry.compaction.Facts[0].Usage = &UsageCount{}
					}
				}
			},
			wantOp:      map[string]*float64{testOpID: nil, "op-compact": ptrUSD(0)},
			wantSession: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := factFixtureGraph()
			tc.mutate(g)
			estimates, session := readEstimates(t, g)
			testassert.Require(t, len(estimates) == 2, "fixture view = %d operations, want the two fixture operations", len(estimates))
			for operationID, want := range tc.wantOp {
				assertEstimate(t, "operation "+operationID, estimates[operationID], want)
			}
			assertEstimate(t, "session", session, tc.wantSession)
		})
	}
}

// TestCopiedForkPrefixAndCompletionReportsChargeNothing proves the copied
// prefix and the informational completion signal never contribute a cost or a
// token: a zero-cost fork whose copied entries report no usage stays a known
// zero instead of turning unknown, and the register totals stay empty.
func TestCopiedForkPrefixAndCompletionReportsChargeNothing(t *testing.T) {
	fork := &testGraph{session: validSessionRecord()}
	fork.session.Identity.SourceSessionID = otherSession()
	fork.session.Identity.SourceBoundaryEntryID = otherEntry()
	fork.session.State.CheckpointEntryID = hexID(3)
	copiedInput := validInputEntry("")
	copiedInput.EntryID = hexID(1)
	copiedInput.OperationID = ""
	copiedAssistant := validAssistantEntry("")
	copiedAssistant.EntryID = hexID(2)
	copiedAssistant.RequestID = hexID(2)
	copiedAssistant.OperationID = "" // copied prefix entries carry no source usage
	copiedCheckpoint := validCompactionEntry("")
	copiedCheckpoint.EntryID = hexID(3)
	copiedCheckpoint.OperationID = ""
	copiedCheckpoint.Facts[0].Usage = nil // the copied maintenance fact keeps its model and loses its usage
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
		{env: entry(hexID(3), "", EntryCompaction, 3), compaction: &copiedCheckpoint},
		{env: entry(hexID(4), testOpID, EntryInput, 4), input: &ownedInput},
		{env: entry(hexID(5), testOpID, EntryOperationSettlement, 5), settlement: &settlement},
	}
	estimates, session := readEstimates(t, fork)
	// The copied nil-usage assistant and checkpoint fact add nothing: the
	// owned operation has no model producer, so the estimate stays a known
	// zero rather than the copied absent reports making it unknown.
	assertEstimate(t, "operation "+testOpID, estimates[testOpID], ptrUSD(0))
	assertEstimate(t, "session", session, ptrUSD(0))
}

// TestCapturePricingReadOwnership proves a read-owned capture's rate pointees
// are independent: mutating a rate returned by one read reaches neither the
// coordinator nor a later read, and the composed view's estimate stays
// computed from the retained capture.
func TestCapturePricingReadOwnership(t *testing.T) {
	g := factFixtureGraph()
	h := newTestHarness(t, g.storage(t), nil)
	ctx := context.Background()
	first, err := h.ReadOperation(ctx, testSessionID, testOpID)
	testassert.NoError(t, err)
	pricing := first.Admission.Execution.Pricing
	compactPricing := first.Admission.Execution.Compact.Pricing
	testassert.Require(t, pricing != nil && compactPricing != nil && pricing.Input != nil && compactPricing.Input != nil,
		"fixture capture = %+v, want captured conversation and compact pricing", first.Admission.Execution)
	input, output, cacheRead, cacheWrite, compactInput := *pricing.Input, *pricing.Output, *pricing.CacheRead, *pricing.CacheWrite, *compactPricing.Input
	*pricing.Input = 999
	*pricing.Output = 999
	*pricing.CacheRead = 999
	*pricing.CacheWrite = 999
	*compactPricing.Input = 999
	second, err := h.ReadOperation(ctx, testSessionID, testOpID)
	testassert.NoError(t, err)
	got := second.Admission.Execution.Pricing
	gotCompact := second.Admission.Execution.Compact.Pricing
	testassert.Require(t, got != nil && *got.Input == input && *got.Output == output && *got.CacheRead == cacheRead && *got.CacheWrite == cacheWrite,
		"a mutation of one read's conversation rate pointees reached the retained capture: %+v", got)
	testassert.Require(t, gotCompact != nil && *gotCompact.Input == compactInput,
		"a mutation of one read's compact rate pointee reached the retained capture: %+v", gotCompact)
	view, err := h.ReadSessionView(ctx, testSessionID)
	testassert.NoError(t, err)
	testassert.Require(t, view.EstimatedCostUSD == nil, "the mutated read did not leave the Session estimate unknown like the retained graph says: %v", view.EstimatedCostUSD)
}
