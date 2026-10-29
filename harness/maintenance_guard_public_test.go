// Public-surface coverage for the maintenance completion guard: a model-free
// none/checkpoint procedure that reaches its publication without any Engine
// cancellation checkpoint selects interruption versus prospective success
// under the publication hold — a parked successful Open and a parked Plan
// released under an Interrupt settle the ordinary interruption with the
// incurred facts, preserve the previous checkpoint, and never dispatch the
// waiting conversation, while the nearest successful sibling and a
// cancellation arriving after the committed success never rewrite it.
package harness_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// modelFreeStrategy is the model-free pure planning shape: Plan answers
// immediately (optionally parked) with one none or checkpoint plan, so the
// procedure reaches its completion boundary with no Engine cancellation
// checkpoint behind it. The park arms only after the fixture's history is in
// place, so the preparing conversation never parks.
type modelFreeStrategy struct {
	checkpoint bool
	armed      *atomic.Bool  // parks only while armed
	parked     chan struct{} // signals Plan entry when parking
	release    chan struct{} // Plan blocks on it when parking
}

func (s *modelFreeStrategy) Plan(in harness.PlanInput) (harness.PlanOutput, error) {
	if s.parked != nil && s.armed.CompareAndSwap(true, false) {
		s.parked <- struct{}{}
		<-s.release
	}
	if !s.checkpoint {
		return harness.PlanOutput{Kind: harness.PlanNone}, nil
	}
	zero := 0
	authored, err := model.NewMessage(model.Message{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "authored context"}}})
	if err != nil {
		return harness.PlanOutput{}, err
	}
	return harness.PlanOutput{Kind: harness.PlanCheckpoint, Checkpoint: &harness.CheckpointPlan{
		BoundaryEntryID: in.BoundaryEntryID,
		Items:           []harness.CheckpointItem{{Retain: &zero}, {Context: &authored}},
	}}, nil
}

func (s *modelFreeStrategy) Consume(harness.ConsumeInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

// factBearingStrategy incurs one accepted model pass and folds its completed
// output into a checkpoint through the pure folding stage, optionally parked.
type factBearingStrategy struct {
	armed    *atomic.Bool
	parked   chan struct{}
	release  chan struct{}
	planText string
}

func (s *factBearingStrategy) Plan(harness.PlanInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanModel, Model: &harness.ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "maintenance pass"}}}}},
	}}, nil
}

func (s *factBearingStrategy) Consume(in harness.ConsumeInput) (harness.PlanOutput, error) {
	if s.parked != nil && s.armed.CompareAndSwap(true, false) {
		s.parked <- struct{}{}
		<-s.release
	}
	zero := 0
	authored, err := model.NewMessage(model.Message{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: s.planText}}})
	if err != nil {
		return harness.PlanOutput{}, err
	}
	return harness.PlanOutput{Kind: harness.PlanCheckpoint, Checkpoint: &harness.CheckpointPlan{
		BoundaryEntryID: in.Input.BoundaryEntryID,
		Items:           []harness.CheckpointItem{{Retain: &zero}, {Context: &authored}},
	}}, nil
}

// modelFreePrep arms the fixture's preparation with the model-free binding
// and an Open that parks the compact Operation on the given channel before
// returning success.
func modelFreePrep(f *publicFixture, strategy harness.ContextStrategy, openParked, openRelease chan struct{}) {
	f.defaultPrep.Capture = compactManualCapture()
	f.defaultPrep.Capture.Strategy = harness.StrategyCapture{ID: "test.strategy/v1"}
	open := f.defaultPrep.Open
	f.defaultPrep.Open = func(ctx context.Context, admission harness.OperationAdmission) (harness.Execution, error) {
		if openParked != nil && admission.RequestKind == harness.RequestKindCompact {
			openParked <- struct{}{}
			<-openRelease
		}
		exec, err := open(ctx, admission)
		if err != nil {
			return exec, err
		}
		exec.Strategy = &harness.StrategyBinding{ID: "test.strategy/v1", Strategy: strategy}
		return exec, nil
	}
}

// compactAsync admits one Compact Operation like compactWhenIdle but from
// the caller's own goroutine: only the transient idle-guard rejection
// retries, bounded by the wait bound.
func compactAsync(t *testing.T, h *harness.Harness, session, operation string, done chan struct{}) {
	t.Helper()
	go func() {
		defer close(done)
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, err := h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: operation})
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), "is not idle; admission requires an idle Session") || time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

// requireInterruptedCompact asserts the compact Operation settled the
// ordinary interruption with no new checkpoint entry and no model dispatch.
func requireInterruptedCompact(t *testing.T, f *publicFixture, store harness.Storage, session, operation string, compact *scriptModel) {
	t.Helper()
	rec := awaitTerminal(t, f.h, session, operation)
	testassert.Require(t, rec.State.Status == harness.OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == "agent interrupted",
		"compact operation = %+v, want the ordinary interruption settlement", rec.State)
	entries, err := store.ReadEntries(context.Background(), session, 0)
	testassert.NoError(t, err)
	for _, entry := range entries {
		testassert.Require(t, entry.Kind != harness.EntryCompaction || entry.OperationID != operation, "entry %s published a checkpoint under the interruption", entry.ID)
	}
	testassert.Equal(t, len(compact.seen()), 0) // no model dispatch and no replay
}

// TestPublicManualCompactParkedOpenInterruptSetsInterruption proves the
// parked successful Open under an Interrupt: the model-free none and
// checkpoint procedures settle the ordinary interruption, publish no
// checkpoint, and never dispatch a model.
func TestPublicManualCompactParkedOpenInterruptSetsInterruption(t *testing.T) {
	for _, shape := range []struct {
		name       string
		checkpoint bool
	}{
		{"model-free none", false},
		{"model-free checkpoint", true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, store harness.Storage) {
				conversation := newScriptModel(publicTurn())
				compact := newScriptModel(summaryAttempt("unused"))
				f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
				openParked, openRelease := make(chan struct{}, 1), make(chan struct{})
				modelFreePrep(f, &modelFreeStrategy{checkpoint: shape.checkpoint}, openParked, openRelease)
				defer f.close()
				session := createSession(t, f.h)
				idleHistory(t, f, session)

				done := make(chan struct{})
				compactAsync(t, f.h, session, "c-1", done)
				select {
				case <-openParked:
				case <-time.After(10 * time.Second):
					t.Fatal("the execution opener never parked")
				}
				testassert.NoError(t, f.h.Interrupt(context.Background(), session))
				close(openRelease) // the parked successful Open returns under the interruption
				<-done

				requireInterruptedCompact(t, f, store, session, "c-1", compact)
			})
		})
	}
}

// TestPublicManualCompactParkedPlanInterruptBeforeCommit proves the parked
// pure planning stage released under an Interrupt: the plan result reaches
// its completion boundary and selects the ordinary interruption, with no new
// checkpoint and no model dispatch.
func TestPublicManualCompactParkedPlanInterruptBeforeCommit(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("unused"))
		f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
		armed := &atomic.Bool{}
		strategy := &modelFreeStrategy{checkpoint: true, armed: armed, parked: make(chan struct{}, 1), release: make(chan struct{})}
		modelFreePrep(f, strategy, nil, nil)
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)
		armed.Store(true)

		done := make(chan struct{})
		compactAsync(t, f.h, session, "c-1", done)
		select {
		case <-strategy.parked:
		case <-time.After(10 * time.Second):
			t.Fatal("the pure planning stage never parked")
		}
		testassert.NoError(t, f.h.Interrupt(context.Background(), session))
		close(strategy.release) // the plan result commits after the interruption
		<-done

		requireInterruptedCompact(t, f, store, session, "c-1", compact)
	})
}

// TestPublicManualCompactNearestSuccessfulSibling proves the nearest sibling
// of the interrupted publication: the same parked plan without an
// interruption publishes the checkpoint and settles the manual success.
func TestPublicManualCompactNearestSuccessfulSibling(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("unused"))
		f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
		armed := &atomic.Bool{}
		strategy := &modelFreeStrategy{checkpoint: true, armed: armed, parked: make(chan struct{}, 1), release: make(chan struct{})}
		modelFreePrep(f, strategy, nil, nil)
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)
		armed.Store(true)

		done := make(chan struct{})
		compactAsync(t, f.h, session, "c-1", done)
		select {
		case <-strategy.parked:
		case <-time.After(10 * time.Second):
			t.Fatal("the pure planning stage never parked")
		}
		close(strategy.release)
		<-done

		rec := awaitTerminal(t, f.h, session, "c-1")
		testassert.Require(t, rec.State.Status == harness.OperationSuccess, "compact operation = %+v, want the prospective success published", rec.State)
		entries, err := store.ReadEntries(context.Background(), session, 0)
		testassert.NoError(t, err)
		owned := 0
		for _, entry := range entries {
			if entry.Kind == harness.EntryCompaction && entry.OperationID == "c-1" {
				owned++
			}
		}
		testassert.Equal(t, owned, 1)
	})
}

// TestPublicManualCompactAcceptedFactsSurviveGuardInterruption proves the
// accepted model usage and facts survive when a later maintenance-success
// guard selects interruption: one model pass is accepted, and the checkpoint
// its folding stage returns settles the ordinary interruption with those
// facts instead.
func TestPublicManualCompactAcceptedFactsSurviveGuardInterruption(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("unused"), summaryAttempt("unused"))
		f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
		armed := &atomic.Bool{}
		strategy := &factBearingStrategy{armed: armed, parked: make(chan struct{}, 1), release: make(chan struct{}), planText: "authored context"}
		modelFreePrep(f, strategy, nil, nil)
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)
		armed.Store(true)

		done := make(chan struct{})
		compactAsync(t, f.h, session, "c-1", done)
		select {
		case <-strategy.parked:
		case <-time.After(10 * time.Second):
			t.Fatal("the pure folding stage never parked")
		}
		testassert.NoError(t, f.h.Interrupt(context.Background(), session))
		close(strategy.release) // the fold returns its checkpoint after the interruption
		<-done

		rec := awaitTerminal(t, f.h, session, "c-1")
		testassert.Require(t, rec.State.Status == harness.OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == "agent interrupted",
			"compact operation = %+v, want the ordinary interruption settlement", rec.State)
		entries, err := store.ReadEntries(context.Background(), session, 0)
		testassert.NoError(t, err)
		var settlement string
		for _, entry := range entries {
			testassert.Require(t, entry.Kind != harness.EntryCompaction || entry.OperationID != "c-1", "entry %s published a checkpoint under the interruption", entry.ID)
			if entry.Kind == harness.EntryOperationSettlement && entry.OperationID == "c-1" {
				settlement = string(entry.Payload)
			}
		}
		testassert.Require(t, strings.Contains(settlement, `"facts"`) && strings.Contains(settlement, `"input_tokens":10`) && strings.Contains(settlement, `"cached_input_tokens":2`) && strings.Contains(settlement, `"output_tokens":5`),
			"interruption settlement %s, want the accepted model facts and usage preserved", settlement)
	})
}

// TestPublicManualCompactSuccessSurvivesLaterCancellation proves a
// cancellation arriving after the committed success never rewrites it.
func TestPublicManualCompactSuccessSurvivesLaterCancellation(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("unused"))
		f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
		modelFreePrep(f, &modelFreeStrategy{checkpoint: true}, nil, nil)
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)

		compactWhenIdle(t, f.h, session, "c-1")
		rec := awaitTerminal(t, f.h, session, "c-1")
		testassert.Require(t, rec.State.Status == harness.OperationSuccess, "compact operation = %+v, want the committed success", rec.State)

		testassert.NoError(t, f.h.Interrupt(context.Background(), session)) // idle Session: no rewrite possible
		after := awaitTerminal(t, f.h, session, "c-1")
		testassert.Require(t, after.State.Status == harness.OperationSuccess, "compact operation = %+v, want the committed success preserved", after.State)
		entries, err := store.ReadEntries(context.Background(), session, 0)
		testassert.NoError(t, err)
		settlements := 0
		for _, entry := range entries {
			if entry.Kind == harness.EntryOperationSettlement && entry.OperationID == "c-1" {
				settlements++
				testassert.Require(t, strings.Contains(string(entry.Payload), `"status":"success"`), "settlement payload %s, want the preserved success", entry.Payload)
			}
		}
		testassert.Equal(t, settlements, 1)
	})
}
