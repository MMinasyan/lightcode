// Public producer-identity and capture-ownership rows for captured pricing:
// the shared terminal settlement keeps a reportless conversation model
// producer visible, and a preparation's pricing pointers never alias the
// retained durable capture.
package harness_test

import (
	"context"
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

func pricingRate(v float64) *float64 { return &v }

// TestTerminalModelProducerIdentityAtSharedSettlement drives the public
// execution through the shared terminal settlement: a conversation model
// effect that settles with no assistant payload and no usage report keeps its
// model producer identity on the settlement entry, while a failure with no
// live model intent stays a no-producer settlement — the read-time estimator
// distinguishes the absent report (unknown) from the absent producer (zero).
func TestTerminalModelProducerIdentityAtSharedSettlement(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		t.Run("reportless model failure names its producer", func(t *testing.T) {
			script := newScriptModel(publicFail("provider unavailable"))
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			session := createSession(t, f.h)
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
			record := awaitTerminal(t, f.h, session, "op-1")
			testassert.Require(t, record.State.Status == harness.OperationFailure, "settled record = %+v, want terminal failure", record)

			settlement := settlementFact(t, f, session)
			testassert.Require(t, settlement.Model != nil && *settlement.Model == publicModelRef && settlement.Usage == nil,
				"settlement entry = %+v, want the live conversation model identity beside an absent usage report", settlement)
		})
		t.Run("failure with no model intent names no producer", func(t *testing.T) {
			script := newScriptModel()
			f := newPublicFixture(t, store, script, nil)
			f.prepareHook = func(_ int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
				return harness.PreparedExecution{
					Capture: publicCapture(),
					Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
						return harness.Execution{}, errors.New("execution open failed")
					},
				}, nil
			}
			defer f.close()
			session := createSession(t, f.h)
			_, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
			record := awaitTerminal(t, f.h, session, "op-2")
			testassert.Require(t, record.State.Terminal != nil, "settled record = %+v, want a terminal settlement", record)

			settlement := settlementFact(t, f, session)
			testassert.Require(t, settlement.Model == nil && settlement.Usage == nil,
				"settlement entry = %+v, want no model producer for a failure that never opened a model intent", settlement)
			testassert.Require(t, len(script.seen()) == 0, "the failed opener still dispatched %d model requests", len(script.seen()))
		})
	})
}

// settlementFact returns the committed operation_settlement entry payload of
// one settled Session through the public history read.
func settlementFact(t *testing.T, f *publicFixture, sessionID string) *harness.OperationSettlementEntry {
	t.Helper()
	view, err := f.h.ReadHistoryView(context.Background(), sessionID)
	testassert.NoError(t, err)
	facts := view.HistoryFacts(historyIDsOf(view.HistoryHeaders()))
	for i := range facts {
		if facts[i].Settlement != nil {
			return facts[i].Settlement
		}
	}
	t.Fatalf("session %s committed no settlement entry: %d facts", sessionID, len(facts))
	return nil
}

// TestAcceptedCapturePricingIsOwnedByTheHarness proves the admission retains
// an independent copy of the prepared capture's pricing: a preparation that
// later mutates its own rate pointees reaches neither the durable capture nor
// a later read.
func TestAcceptedCapturePricingIsOwnedByTheHarness(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel(publicTurn())
		f := newPublicFixture(t, store, script, nil)
		accepted := publicCapture()
		accepted.Pricing = &harness.ModelPricing{Input: pricingRate(1), Output: pricingRate(2)}
		accepted.Compact.Pricing = &harness.ModelPricing{CacheRead: pricingRate(0.25)}
		f.prepareHook = func(_ int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: accepted,
				Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
					return publicExecution(script.effect, script.effect), nil
				},
			}, nil
		}
		defer f.close()
		session := createSession(t, f.h)
		_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
		testassert.NoError(t, err)
		awaitTerminal(t, f.h, session, "op-1")

		// Mutating the accepted capture's rate pointees after admission must
		// not reach the retained capture.
		*accepted.Pricing.Input = 999
		*accepted.Pricing.Output = 999
		*accepted.Compact.Pricing.CacheRead = 999
		record, err := f.h.ReadOperation(context.Background(), session, "op-1")
		testassert.NoError(t, err)
		got := record.Admission.Execution.Pricing
		gotCompact := record.Admission.Execution.Compact.Pricing
		testassert.Require(t, got != nil && *got.Input == 1 && *got.Output == 2, "retained conversation pricing = %+v, want the admitted rates owned at admission", got)
		testassert.Require(t, gotCompact != nil && *gotCompact.CacheRead == 0.25, "retained compact pricing = %+v, want the admitted rate owned at admission", gotCompact)
	})
}
