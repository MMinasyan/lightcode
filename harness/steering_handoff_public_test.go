package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

// TestSteeringHandoffStoreAxes exercises the durable and recovery axes of the
// steering handoff over both real storage implementations: process loss after
// the handoff settlement but before the successor admission loses the buffer
// while the committed success survives, loss after admission leaves the
// running successor to the restart recovery's normal interruption with no
// replay, a manual compaction drains its steering only after its own
// terminal, and an extra Operation-owned input is persisted corruption every
// reader reports instead of an admitted rewind boundary.
func TestSteeringHandoffStoreAxes(t *testing.T) {
	t.Run("loss after the settlement and before admission loses the buffer", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			prepareGate := make(chan struct{})
			releasePrepare := sync.OnceFunc(func() { close(prepareGate) })
			defer releasePrepare()
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			// preparation call 0 is op-1's admission; the drain's admission of
			// the steering head parks on its gate with the buffer selected but
			// unadopted
			f.prepareHook = func(call int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
				if call == 1 {
					<-prepareGate
				}
				return scriptPrepared(script), nil
			}
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare // op-1's admission preparation
			<-script.arrived
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "s1"); err != nil {
				t.Fatalf("steering submit: %v", err)
			}
			script.releaseGate()
			awaitTerminal(t, f.h, session, "op-1") // the handoff's quiet success committed
			<-f.prepare                            // the drain's admission preparation is parked

			f.cancel()       // the loss begins while the buffer is selected but unadopted
			releasePrepare() // the parked admission aborts on the dying Harness context
			if err := f.h.Wait(context.Background()); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
			if err != nil || rec.State.Status != harness.OperationSuccess {
				t.Fatalf("op-1 = %+v err %v, want the committed handoff success preserved", rec, err)
			}
			if _, err := f.h.ReadOperation(context.Background(), session, "op-2"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("op-2 = %v, want no successor admitted from the lost buffer", err)
			}
			snap, err := f.h.SnapshotSession(context.Background(), session)
			if err != nil {
				t.Fatalf("snapshot after the loss: %v", err)
			}
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 && fact.Input.Content[0].Text == "s1" {
					t.Fatalf("the lost buffer committed input entry %s", fact.EntryID)
				}
			}
		})
	})

	t.Run("loss after admission leaves the running successor to recovery", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			probe := &rollbackProbeStore{Storage: store}
			var (
				mu    sync.Mutex
				calls int
			)
			firstArrived := make(chan struct{})
			successorArrived := make(chan struct{})
			gate1 := make(chan struct{})
			releaseGate1 := sync.OnceFunc(func() { close(gate1) })
			defer releaseGate1()
			modelFn := func(ctx context.Context, req model.Request) (model.Stream, error) {
				mu.Lock()
				calls++
				n := calls
				mu.Unlock()
				if n == 1 {
					close(firstArrived)
					<-gate1
					return publicTurnStream(), nil
				}
				close(successorArrived) // the admitted successor parks mid-execution
				select {
				case <-ctx.Done():
				case <-time.After(15 * time.Second):
					return nil, errors.New("the parked successor was never canceled")
				}
				return nil, ctx.Err()
			}
			f := newPublicFixture(t, probe, newScriptModel(), modelFn)
			defer f.close()
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-firstArrived
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "s1"); err != nil {
				t.Fatalf("steering submit: %v", err)
			}
			releaseGate1()
			awaitTerminal(t, f.h, session, "op-1") // the handoff's quiet success
			<-successorArrived                     // the successor is durably admitted and parked

			// The dying owner's live cancellation settlement must fail and
			// roll back: its retained intent keeps the successor running for
			// the restart recovery instead of rewriting the terminal.
			probe.failReplace = true
			f.cancel()
			if err := f.h.Wait(context.Background()); err != nil && !errors.Is(err, errInjectedRollback) {
				t.Fatalf("Wait: %v", err)
			}
			probe.failReplace = false // the gate is removed before the recovery writes

			running := newPublicFixture(t, store, newScriptModel(), nil) // fresh durable state, not the old cached coordinator
			rec, err := running.h.ReadOperation(ctx, session, "op-2")
			if err != nil || rec.State.Status != harness.OperationRunning {
				t.Fatalf("successor after the loss = %+v err %v, want the running state left for recovery", rec, err)
			}
			running.close()

			if err := harness.Recover(ctx, store); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			fresh := newPublicFixture(t, store, newScriptModel(), nil) // a new reader observes the recovery's writes
			defer fresh.close()
			rec, err = fresh.h.ReadOperation(ctx, session, "op-2")
			if err != nil || rec.State.Status != harness.OperationInterruption ||
				rec.State.Terminal == nil || rec.State.Terminal.Detail != recoveredInterruptionDetail {
				t.Fatalf("recovered successor = %+v err %v, want the standard recovery interruption", rec, err)
			}
			// No replay: the parked call is the successor's only model call and
			// the recovery added exactly its one settlement.
			mu.Lock()
			got := calls
			mu.Unlock()
			if got != 2 {
				t.Fatalf("model calls = %d, want the predecessor's and the parked successor's with no replay", got)
			}
			snap, err := fresh.h.SnapshotSession(ctx, session)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			settlements := 0
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntryOperationSettlement && fact.OperationID == "op-2" {
					settlements++
				}
			}
			if settlements != 1 {
				t.Fatalf("successor settlements = %d, want exactly the recovery's one", settlements)
			}
		})
	})

	t.Run("manual compaction drains its steering only after its terminal", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			script := newScriptModel()
			compactGate := make(chan struct{})
			releaseCompact := sync.OnceFunc(func() { close(compactGate) })
			defer releaseCompact()
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare // op-1's admission preparation
			<-script.arrived
			awaitTerminal(t, f.h, session, "op-1")
			awaitStoreQuiet(t, f.h, session) // the terminal-to-retirement window closes before the manual compact

			script.gate = compactGate // the compact summarizer parks while the steering submits
			compactDone := make(chan struct{})
			go func() {
				defer close(compactDone)
				if _, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "compact-1"}); err != nil {
					t.Errorf("Compact: %v", err)
				}
			}()
			<-f.prepare                                                                                  // the compact admission is preparing
			<-script.arrived                                                                             // the compact summarizer is parked
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "s1"); err != nil { // buffered while the compaction runs
				t.Fatalf("steering submit during the compaction: %v", err)
			}
			snap, err := f.h.SnapshotSession(ctx, session)
			if err != nil {
				t.Fatalf("snapshot during the compaction: %v", err)
			}
			if len(snap.Steering) != 1 || snap.Steering[0].OperationID != "op-2" {
				t.Fatalf("steering during the compaction = %+v, want the submit buffered", snap.Steering)
			}
			releaseCompact()
			<-compactDone
			awaitTerminal(t, f.h, session, "compact-1")
			<-f.prepare // the steering head's own admission preparation
			<-script.arrived
			awaitTerminal(t, f.h, session, "op-2")
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}

			snap, err = f.h.SnapshotSession(ctx, session)
			if err != nil {
				t.Fatalf("final snapshot: %v", err)
			}
			compactions := 0
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntryCompaction {
					compactions++
				}
				if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 && fact.Input.Content[0].Text == "s1" && fact.OperationID != "op-2" {
					t.Fatalf("the steering input is owned by %q, want its own Operation op-2", fact.OperationID)
				}
			}
			if compactions != 1 {
				t.Fatalf("compaction entries = %d, want exactly one summary", compactions)
			}

			// the silent handoff adds no usage: every usage-bearing entry is
			// one actual model turn, so the compact Operation carries exactly
			// its summary turn and the successor exactly its own turn
			wantTurn := harness.UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}
			singleTurn := func(totals harness.UsageTotals) bool {
				return len(totals.ByModel) == 1 && totals.ByModel[0].Usage == wantTurn
			}
			compactRec, err := f.h.ReadOperation(ctx, session, "compact-1")
			if err != nil || !singleTurn(compactRec.State.Usage) {
				t.Fatalf("compact usage = %+v err %v, want exactly the summary turn's counts", compactRec.State.Usage, err)
			}
			steerRec, err := f.h.ReadOperation(ctx, session, "op-2")
			if err != nil || !singleTurn(steerRec.State.Usage) {
				t.Fatalf("successor usage = %+v err %v, want exactly its own turn's counts with no handoff contribution", steerRec.State.Usage, err)
			}
			if len(snap.Session.State.Usage.ByModel) != 1 || snap.Session.State.Usage.ByModel[0].Usage != (harness.UsageCount{InputTokens: 9, CachedInputTokens: 3, OutputTokens: 6}) {
				t.Fatalf("session usage = %+v, want exactly the three actual turns' sum (9/3/6)", snap.Session.State.Usage)
			}
		})
	})

	t.Run("an extra owned input is persisted corruption every reader reports", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			script := newScriptModel(publicTurn())
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			awaitTerminal(t, f.h, session, "op-1")
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}

			// inject the forbidden shape directly: a second input owned by the
			// message Operation that is not its admitted entry
			extraID := strings.Repeat("a", 32)
			payload, err := json.Marshal(harness.InputEntry{
				SessionID:   session,
				EntryID:     extraID,
				OperationID: "op-1",
				Origin:      harness.InputOriginUser,
				Content:     []model.ContentPart{{Kind: model.PartText, Text: "extra"}},
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := store.Transact(ctx, func(tx harness.Transaction) error {
				_, err := tx.InsertEntry(harness.EntryDraft{
					SessionID: session, ID: extraID, OperationID: "op-1",
					Kind: harness.EntryInput, Payload: payload,
				})
				return err
			}); err != nil {
				t.Fatalf("inject the extra owned input: %v", err)
			}

			fresh := newPublicFixture(t, store, newScriptModel(), nil) // a new reader revalidates the persisted graph
			defer fresh.close()
			if _, err := fresh.h.SnapshotSession(ctx, session); !errors.Is(err, harness.ErrCorrupt) {
				t.Fatalf("SnapshotSession = %v, want the typed corruption", err)
			}
			if _, _, err := fresh.h.ReadSessionHistory(ctx, session); !errors.Is(err, harness.ErrCorrupt) {
				t.Fatalf("ReadSessionHistory = %v, want the typed corruption", err)
			}
			if err := harness.Recover(ctx, store); err != nil {
				t.Fatalf("Recover = %v, want the corrupt Session left unchanged", err)
			}
		})
	})
}

// TestSteeringHandoffStoreControlAxes exercises the reservation, control and
// failure axes of the handoff over both real stores: the carried reservation
// spans the handoff window so neither Archive nor Fork can overtake a
// selected handoff (each observed consulting its own live context and
// canceled explicitly); a non-storage settlement-publication failure settles
// the ordinary failure with the successor delivered afterwards; and an
// ErrStorage settlement failure leaves the running quiet predecessor for
// recovery with the head retained, no successor, the retained storage error
// in Wait, and the standard restart interruption without replay. The real
// revision/rematerialization discriminator rows live in
// TestSteeringHandoffRevisionDiscriminator.
func TestSteeringHandoffStoreControlAxes(t *testing.T) {
	t.Run("reservation spans the handoff window and no control overtakes it", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			prepareGate := make(chan struct{})
			releasePrepare := sync.OnceFunc(func() { close(prepareGate) })
			defer releasePrepare()
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			f.prepareHook = func(call int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
				if call == 1 { // the successor's admission preparation parks
					<-prepareGate
				}
				return scriptPrepared(script), nil
			}
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare
			<-script.arrived
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "s1"); err != nil {
				t.Fatalf("steering submit: %v", err)
			}
			script.releaseGate()
			awaitSettled(t, f.h, session, "op-1") // the handoff's quiet success
			<-f.prepare                           // the successor's preparation is parked behind the carried reservation

			snap, err := f.h.SnapshotSession(context.Background(), session)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			if !snap.ExecutionBusy {
				t.Fatalf("handoff window = idle, want the carried reservation held")
			}
			boundary := readOperationIDEntry(t, f.h, session, "op-1")

			// Each overtaking transition runs on its own live cancellable
			// context: waitContext observes the context being consulted while
			// the reservation is held, the explicit cancel is the only thing
			// that ends the wait, and the call reports the cancellation.
			runControl := func(name string, call func(ctx context.Context) error) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				waiter := &waitContext{Context: ctx, consulted: make(chan struct{})}
				done := make(chan error, 1)
				go func() { done <- call(waiter) }()
				select {
				case <-waiter.consulted: // the transition is parked consulting its context behind the reservation
				case <-time.After(10 * time.Second):
					t.Fatalf("%s never consulted its context in the reservation wait", name)
				}
				select { // still in flight at the moment of the cancel: only the cancel ends it
				case err := <-done:
					t.Fatalf("%s returned %v before its cancel while the reservation was held", name, err)
				default:
				}
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("%s after cancel = %v, want the reported cancellation", name, err)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("%s never returned after its cancel", name)
				}
			}
			runControl("archive", func(ctx context.Context) error {
				_, err := f.h.ArchiveSession(ctx, session)
				return err
			})
			runControl("fork", func(ctx context.Context) error {
				_, err := f.h.Fork(ctx, harness.ForkRequest{SourceSessionID: session, BoundaryEntryID: boundary, OperationID: "fork-1", Content: []model.ContentPart{{Kind: model.PartText, Text: "f"}}})
				return err
			})

			releasePrepare()
			<-script.arrived // the successor's model request began before its terminal is read
			awaitSettled(t, f.h, session, "op-2")
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	})

	t.Run("a settlement-publication failure settles ordinary failure and the successor follows", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			probe := &rollbackProbeStore{Storage: store}
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			f := newPublicFixture(t, probe, script, nil)
			defer f.close()
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare
			<-script.arrived
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "s1"); err != nil {
				t.Fatalf("steering submit: %v", err)
			}
			probe.failSettlementOp, probe.failSettlementErr = "op-1", errors.New("injected settlement failure")
			script.releaseGate()

			// the observed failure terminal: the handoff's quiet-success
			// publication failed once and the outer policy settled ordinary
			// failure
			rec := awaitSettled(t, f.h, session, "op-1")
			if rec.State.Status != harness.OperationFailure ||
				rec.State.Terminal == nil || !strings.Contains(rec.State.Terminal.Detail, "injected settlement failure") {
				t.Fatalf("op-1 = %+v, want the ordinary failure settlement of the failed handoff publication", rec.State)
			}
			if !probe.settlementFired() {
				t.Fatalf("the settlement arm never fired")
			}
			<-script.arrived // the successor's own model request before its terminal
			awaitSettled(t, f.h, session, "op-2")
			snap, err := f.h.SnapshotSession(ctx, session)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			settlements := 0
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntryOperationSettlement && fact.OperationID == "op-1" {
					settlements++
				}
				if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 && fact.Input.Content[0].Text == "s1" && fact.OperationID != "op-2" {
					t.Fatalf("the delivered input is owned by %q, want the successor op-2", fact.OperationID)
				}
			}
			if settlements != 1 {
				t.Fatalf("op-1 settlements = %d, want exactly the one ordinary failure with no handoff retry", settlements)
			}
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	})

	t.Run("a storage settlement failure leaves the running predecessor to recovery", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			probe := &rollbackProbeStore{Storage: store}
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			f := newPublicFixture(t, probe, script, nil)
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare
			<-script.arrived
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "s1"); err != nil {
				t.Fatalf("steering submit: %v", err)
			}
			probe.failSettlementOp = "op-1"
			probe.failSettlementErr = fmt.Errorf("%w: injected settlement storage failure", harness.ErrStorage)
			script.releaseGate()

			// the deterministic rendezvous is the arm firing: the handoff's own
			// settlement transaction ran and failed, leaving the committed
			// running state for recovery (ExecutionBusy never clears here —
			// the durable current Operation stays set by contract)
			deadline := time.Now().Add(10 * time.Second)
			for !probe.settlementFired() {
				if time.Now().After(deadline) {
					t.Fatalf("the handoff settlement never ran against the armed probe")
				}
				time.Sleep(time.Millisecond)
			}
			if !probe.settlementFired() {
				t.Fatalf("the settlement arm never fired")
			}
			snap, err := f.h.SnapshotSession(ctx, session)
			if err != nil {
				t.Fatalf("snapshot after the failed publication: %v", err)
			}
			if snap.Session.State.CurrentOperationID != "op-1" {
				t.Fatalf("current operation = %q, want the running predecessor retained", snap.Session.State.CurrentOperationID)
			}
			if len(snap.Steering) != 1 || snap.Steering[0].OperationID != "op-2" {
				t.Fatalf("steering after the failed publication = %+v, want the head retained", snap.Steering)
			}
			if rec, err := f.h.ReadOperation(ctx, session, "op-1"); err != nil || rec.State.Status != harness.OperationRunning ||
				rec.State.ActiveEffect != nil || len(rec.State.PendingToolCalls) != 0 {
				t.Fatalf("op-1 = %+v err %v, want the running quiet state for recovery", rec, err)
			}
			if _, err := f.h.ReadOperation(ctx, session, "op-2"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("op-2 register = %v, want no successor admitted", err)
			}
			f.cancel()
			if err := f.h.Wait(context.Background()); err == nil || !errors.Is(err, harness.ErrStorage) {
				t.Fatalf("Wait = %v, want the retained first storage failure", err)
			}
			after, err := f.h.SnapshotSession(ctx, session)
			if err != nil {
				t.Fatalf("snapshot after the loss: %v", err)
			}
			if len(after.Steering) != 0 {
				t.Fatalf("steering after the loss = %+v, want the buffer discarded without delivery", after.Steering)
			}
			for _, fact := range after.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && strings.Contains(fact.Input.Content[0].Text, "s1") {
					t.Fatalf("the lost head committed input entry %s", fact.EntryID)
				}
			}

			// the restart recovery settles the standard interruption without
			// replay: one assistant contribution and one settlement in the
			// fresh durable state
			if err := harness.Recover(ctx, store); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			state := recoverOpStateAt(t, store, session, "op-1")
			if state.Status != "interruption" || state.ActiveEffect != nil || state.Terminal == nil ||
				state.Terminal.Detail != recoveredInterruptionDetail {
				t.Fatalf("recovered op-1 = %+v, want the standard recovery interruption", state)
			}
			entries, err := store.ReadEntries(ctx, session, 0)
			if err != nil {
				t.Fatalf("read entries: %v", err)
			}
			assistants, settlements := 0, 0
			for _, entry := range entries {
				switch {
				case entry.Kind == harness.EntryAssistant && entry.OperationID == "op-1":
					assistants++
				case entry.Kind == harness.EntryOperationSettlement && entry.OperationID == "op-1":
					settlements++
				case entry.Kind == harness.EntryInput && strings.Contains(string(entry.Payload), "s1"):
					t.Fatalf("the lost head committed entry %s", entry.ID)
				}
			}
			if assistants != 1 || settlements != 1 {
				t.Fatalf("op-1 durable tail = %d assistants, %d settlements, want exactly one of each with no replay", assistants, settlements)
			}
		})
	})
}

// readOperationIDEntry returns the admitted entry identity of one Operation
// through the public read.
func readOperationIDEntry(t *testing.T, h *harness.Harness, sessionID, operationID string) string {
	t.Helper()
	rec, err := h.ReadOperation(context.Background(), sessionID, operationID)
	if err != nil {
		t.Fatalf("ReadOperation(%s): %v", operationID, err)
	}
	return rec.Admission.AdmittedEntry.EntryID
}

// steeringStopper is the gated Jobs seam of the public closure rows: StopJob
// records its arrival (the deterministic observation that the stop's group
// state is already published), parks on its gate, then runs the delivery
// callback that stands for the ordinary kill-completion path.
type steeringStopper struct {
	gate    chan struct{}
	arrived chan struct{}
	stopped []string
	deliver func(sessionID, jobID string)
}

func (s *steeringStopper) StopJob(sessionID, jobID string) {
	s.stopped = append(s.stopped, sessionID+"/"+jobID)
	s.arrived <- struct{}{}
	<-s.gate
	if s.deliver != nil {
		s.deliver(sessionID, jobID)
	}
}

// TestSteeringHandoffInterruptAndClosureOverStores proves the interrupt and
// closure axes of the handoff over both real stores through public
// operations: an interrupt before the handoff keeps the real interruption
// while the buffered head drains as its own successor; an interrupt landing
// between the handoff settlement and the successor's parked admission never
// rewrites the durable success nor emits an interruption signal; a root Stop
// parks in its stopping interval while the handoff and the successor both
// succeed, and its delivered completion is ordinary successor work; a child's
// permanent closure discards its buffered heads, settles interrupted, and
// delivers its parent completion only after its own convergence.
func TestSteeringHandoffInterruptAndClosureOverStores(t *testing.T) {
	t.Run("interrupt before the handoff keeps the real interruption", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-script.arrived // the predecessor is parked at its model boundary
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "s1"); err != nil {
				t.Fatalf("steering submit: %v", err)
			}
			if err := f.h.Interrupt(context.Background(), session); err != nil {
				t.Fatalf("Interrupt: %v", err)
			}
			script.releaseGate()

			rec := awaitSettled(t, f.h, session, "op-1")
			if rec.State.Status != harness.OperationInterruption || rec.State.Terminal == nil ||
				rec.State.Terminal.Detail != "agent interrupted" {
				t.Fatalf("op-1 = %+v, want the real interruption with the contract detail", rec.State)
			}
			successor := awaitSettled(t, f.h, session, "op-2")
			if successor.State.Status != harness.OperationSuccess {
				t.Fatalf("op-2 settled %q, want the head drained as its own successor", successor.State.Status)
			}
			snap, err := f.h.SnapshotSession(context.Background(), session)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 &&
					fact.Input.Content[0].Text == "s1" && fact.OperationID != "op-2" {
					t.Fatalf("the drained input is owned by %q, want op-2", fact.OperationID)
				}
			}
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	})

	t.Run("interrupt after the settlement never rewrites the durable success", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			prepareGate := make(chan struct{})
			releasePrepare := sync.OnceFunc(func() { close(prepareGate) })
			defer releasePrepare()
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			f.prepareHook = func(call int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
				if call == 1 { // the successor's admission preparation parks
					<-prepareGate
				}
				return scriptPrepared(script), nil
			}
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare
			<-script.arrived
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "s1"); err != nil {
				t.Fatalf("steering submit: %v", err)
			}
			script.releaseGate()
			awaitSettled(t, f.h, session, "op-1") // the handoff's quiet success committed
			<-f.prepare                           // the successor's admission is parked mid-preparation

			if err := f.h.Interrupt(context.Background(), session); err != nil {
				t.Fatalf("Interrupt between the handoff and the successor admission: %v", err)
			}
			releasePrepare()
			successor := awaitSettled(t, f.h, session, "op-2")
			if successor.State.Status != harness.OperationSuccess {
				t.Fatalf("op-2 settled %q, want the successor to run normally", successor.State.Status)
			}
			preserved := awaitSettled(t, f.h, session, "op-1")
			if preserved.State.Status != harness.OperationSuccess || preserved.State.Terminal == nil || preserved.State.Terminal.Detail != "" {
				t.Fatalf("op-1 = %+v after the interrupt, want the silent success preserved", preserved.State)
			}
			snap, err := f.h.SnapshotSession(context.Background(), session)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntrySignal {
					t.Fatalf("the interrupt after the handoff emitted a signal entry %s", fact.EntryID)
				}
			}
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	})

	t.Run("a parked root stop never blocks the handoff or the successor", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			stopper := &steeringStopper{gate: make(chan struct{}), arrived: make(chan struct{}, 1)}
			var (
				completionID string
				owner        *harness.Harness
			)
			stopper.deliver = func(sessionID, jobID string) {
				_ = owner.DeliverBackgroundCompletion(context.Background(), sessionID, completionID, "job done")
			}
			h, cancel := newObservedPublicHarness(t, store, script, stopper, func(harness.HarnessFact) {})
			owner = h
			defer cancel()
			session := createSession(t, h)

			if _, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-script.arrived
			if _, err := submit(t, h, session, "op-2", harness.MessageModeRegular, "s1"); err != nil {
				t.Fatalf("steering submit: %v", err)
			}
			if err := h.StartJob(context.Background(), session, "0000000a", func(_ context.Context, id string) error {
				completionID = id // the member stays claimed and live: only the stop's delivery finishes it
				return nil
			}); err != nil {
				t.Fatalf("StartJob: %v", err)
			}
			if completionID == "" {
				t.Fatalf("the job spawn never received its completion identity")
			}

			stopped := make(chan error, 1)
			go func() { stopped <- h.Stop(context.Background(), session) }()
			select {
			case <-stopper.arrived: // the stop is inside its stopping interval: StopJob proves the group state published
			case <-time.After(10 * time.Second):
				t.Fatalf("the stop never reached its StopJob")
			}
			script.releaseGate() // the handoff and the successor run while the stop stays parked

			if rec := awaitSettled(t, h, session, "op-1"); rec.State.Status != harness.OperationSuccess {
				t.Fatalf("op-1 settled %q during the parked stop, want the handoff success", rec.State.Status)
			}
			if rec := awaitSettled(t, h, session, "op-2"); rec.State.Status != harness.OperationSuccess {
				t.Fatalf("op-2 settled %q during the parked stop, want the successor admitted", rec.State.Status)
			}
			close(stopper.gate) // the delivery runs; the stop converges
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatalf("Stop: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("the root stop never converged")
			}
			if rec := awaitSettled(t, h, session, completionID); rec.State.Status != harness.OperationSuccess {
				t.Fatalf("the delivered completion settled %q, want ordinary successor work", rec.State.Status)
			}
			cancel() // Wait joins only after the Harness context ends
			if err := h.Wait(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("Wait: %v", err)
			}
		})
	})

	t.Run("a child permanent closure discards its heads without a successor", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			modelGate := make(chan struct{})
			releaseModel := sync.OnceFunc(func() { close(modelGate) })
			defer releaseModel()
			modelFn := func(ctx context.Context, _ model.Request) (model.Stream, error) {
				select {
				case <-modelGate:
					return publicTurnStream(), nil
				case <-ctx.Done(): // the closure's cancel settles the parked Operation as interruption
					return nil, ctx.Err()
				}
			}
			f := newPublicFixture(t, store, newScriptModel(), modelFn)
			defer f.close()
			parent := createSession(t, f.h)

			launched, err := f.h.LaunchChildSession(ctx, harness.LaunchChildRequest{
				ParentSessionID: parent, AgentType: "coder",
				Content:     []model.ContentPart{{Kind: model.PartText, Text: "child work"}},
				OperationID: "child-op-1", MaxConcurrent: 2, OutputLimit: 4096,
			})
			if err != nil {
				t.Fatalf("LaunchChildSession: %v", err)
			}
			child := launched.ChildSessionID
			<-f.prepare // the child's admission prepared: its execution is parked at the model gate
			// the parent receives no completion while the child runs
			early, err := f.h.SnapshotSession(ctx, parent)
			if err != nil {
				t.Fatalf("early parent snapshot: %v", err)
			}
			for _, fact := range early.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && fact.Input.Origin == harness.InputOriginRuntime {
					t.Fatalf("the parent received a completion while the child ran: %+v", fact.Input)
				}
			}
			if _, err := submit(t, f.h, child, "child-op-2", harness.MessageModeRegular, "steer-child"); err != nil {
				t.Fatalf("child steering submit: %v", err)
			}
			if err := f.h.Stop(context.Background(), child); err != nil {
				t.Fatalf("Stop(child): %v", err)
			}
			releaseModel()

			rec := awaitSettled(t, f.h, child, "child-op-1")
			if rec.State.Status != harness.OperationInterruption {
				t.Fatalf("child op settled %q, want the closure's interruption", rec.State.Status)
			}
			awaitStoreQuiet(t, f.h, parent) // the parent's completion delivery settles after the child converged
			parentSnap, err := f.h.SnapshotSession(ctx, parent)
			if err != nil {
				t.Fatalf("parent snapshot: %v", err)
			}
			completionText := ""
			for _, fact := range parentSnap.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && fact.Input.Origin == harness.InputOriginRuntime {
					completionText = fact.Input.Content[0].Text
				}
			}
			if completionText != "Task interrupted." {
				t.Fatalf("parent completion = %q, want the interrupted child's completion delivered after convergence", completionText)
			}
			childSnap, err := f.h.SnapshotSession(ctx, child)
			if err != nil {
				t.Fatalf("child snapshot: %v", err)
			}
			for _, fact := range childSnap.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 &&
					fact.Input.Content[0].Text == "steer-child" {
					t.Fatalf("the closed child's discarded head committed input entry %s", fact.EntryID)
				}
			}
			if _, err := f.h.ReadOperation(ctx, child, "child-op-2"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("child-op-2 register = %v, want no successor admitted", err)
			}
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	})
}

// readGateStore wraps one public store with a one-shot ReadRegisters gate:
// once armed, the next ReadRegisters signals its arrival and waits for the
// release before delegating, so the test holds the real rematerialization
// interval open outside the store lock while ordinary transactions commit the
// variant state.
type readGateStore struct {
	harness.Storage
	mu      sync.Mutex
	armed   bool
	arrived chan struct{}
	release chan struct{}
}

func (s *readGateStore) arm() {
	s.mu.Lock()
	s.armed = true
	s.mu.Unlock()
}

func (s *readGateStore) ReadRegisters(ctx context.Context, sessionID string) ([]harness.Register, error) {
	s.mu.Lock()
	armed := s.armed
	s.armed = false
	s.mu.Unlock()
	if armed {
		close(s.arrived)
		<-s.release
	}
	return s.Storage.ReadRegisters(ctx, sessionID)
}

// foreignPublicRevision advances one Session register's revision through one
// ordinary public transaction with its payload bytes unchanged: the committed
// foreign revision the handoff's transaction must race against.
func foreignPublicRevision(t *testing.T, store harness.Storage, sessionID string) {
	t.Helper()
	key := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}
	if err := store.Transact(context.Background(), func(tx harness.Transaction) error {
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		_, err = tx.ReplaceRegister(key, reg.Revision, reg.Payload)
		return err
	}); err != nil {
		t.Fatalf("foreign revision: %v", err)
	}
}

// publicOperationWire is the public register wire shape with every section
// that round-trips through exported tags decoded, and the custom usage totals
// carried verbatim as raw bytes — the exported UsageTotals type does not
// round-trip through its struct tags.
type publicOperationWire struct {
	Admission json.RawMessage          `json:"admission"`
	State     publicOperationStateWire `json:"state"`
}

type publicOperationStateWire struct {
	Status           harness.OperationState     `json:"status"`
	StartedAt        time.Time                  `json:"started_at"`
	SettledAt        *time.Time                 `json:"settled_at,omitempty"`
	ActiveEffect     *harness.ActiveEffect      `json:"active_effect,omitempty"`
	PendingToolCalls []harness.PendingToolCall  `json:"pending_tool_calls"`
	Usage            json.RawMessage            `json:"usage"`
	Terminal         *harness.OperationTerminal `json:"terminal,omitempty"`
}

type publicSessionWire struct {
	Identity json.RawMessage        `json:"identity"`
	State    publicSessionStateWire `json:"state"`
}

type publicSessionStateWire struct {
	Lifecycle          harness.SessionLifecycle `json:"lifecycle"`
	ArchivedAt         *time.Time               `json:"archived_at,omitempty"`
	CurrentAgentType   string                   `json:"current_agent_type"`
	CurrentOperationID string                   `json:"current_operation_id,omitempty"`
	CompactionEntryID  string                   `json:"compaction_entry_id,omitempty"`
	Usage              json.RawMessage          `json:"usage"`
	LastActivity       time.Time                `json:"last_activity"`
}

// foreignPublicActiveEffect commits one retained model-effect intent on the
// running Operation through one ordinary public transaction, preserving the
// admission bytes verbatim and mutating only the exported tagged state
// section.
func foreignPublicActiveEffect(t *testing.T, store harness.Storage, sessionID, operationID, resultID string) {
	t.Helper()
	key := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID}
	if err := store.Transact(context.Background(), func(tx harness.Transaction) error {
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		var wire publicOperationWire
		if err := json.Unmarshal(reg.Payload, &wire); err != nil {
			return err
		}
		wire.State.ActiveEffect = &harness.ActiveEffect{Kind: harness.EffectModel, ResultEntryID: resultID}
		payload, err := json.Marshal(wire)
		if err != nil {
			return err
		}
		_, err = tx.ReplaceRegister(key, reg.Revision, payload)
		return err
	}); err != nil {
		t.Fatalf("foreign retained intent: %v", err)
	}
}

// foreignPublicSettlement commits one coherent foreign quiet success through
// one ordinary public transaction: the actual settlement entry, the terminal
// Operation register with its admission bytes preserved, and the cleared
// Session current Operation.
func foreignPublicSettlement(t *testing.T, store harness.Storage, sessionID, operationID, settlementID string) {
	t.Helper()
	if err := store.Transact(context.Background(), func(tx harness.Transaction) error {
		settlement, err := json.Marshal(harness.OperationSettlementEntry{
			SessionID: sessionID, EntryID: settlementID, OperationID: operationID,
			Status: harness.OperationSuccess,
		})
		if err != nil {
			return err
		}
		if _, err := tx.InsertEntry(harness.EntryDraft{
			SessionID: sessionID, ID: settlementID, OperationID: operationID,
			Kind: harness.EntryOperationSettlement, Payload: settlement,
		}); err != nil {
			return err
		}
		opKey := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID}
		reg, err := tx.ReadRegister(opKey)
		if err != nil {
			return err
		}
		var wire publicOperationWire
		if err := json.Unmarshal(reg.Payload, &wire); err != nil {
			return err
		}
		stamped := time.Now().UTC()
		wire.State.Status = harness.OperationSuccess
		wire.State.SettledAt = &stamped
		wire.State.ActiveEffect = nil
		wire.State.Terminal = &harness.OperationTerminal{
			SettlementEntry: harness.EntryRef{SessionID: sessionID, EntryID: settlementID},
		}
		payload, err := json.Marshal(wire)
		if err != nil {
			return err
		}
		if _, err := tx.ReplaceRegister(opKey, reg.Revision, payload); err != nil {
			return err
		}
		sessionKey := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}
		sreg, err := tx.ReadRegister(sessionKey)
		if err != nil {
			return err
		}
		var swire publicSessionWire
		if err := json.Unmarshal(sreg.Payload, &swire); err != nil {
			return err
		}
		swire.State.CurrentOperationID = ""
		sp, err := json.Marshal(swire)
		if err != nil {
			return err
		}
		_, err = tx.ReplaceRegister(sessionKey, sreg.Revision, sp)
		return err
	}); err != nil {
		t.Fatalf("foreign settlement: %v", err)
	}
}

// TestSteeringHandoffRevisionDiscriminator proves the revision/rematerialization
// discriminator over both real stores through public operations: the real
// handoff at an admitted quiet boundary fails against a separately committed
// foreign revision, the shared publisher's real rematerialization is held open
// by a read gate outside the store lock, and the quiet/active/already-settled
// state committed during that interval drives the existing outer settlement
// policy — ordinary failure, running state for recovery, and no rewrite —
// without any handoff retry.
func TestSteeringHandoffRevisionDiscriminator(t *testing.T) {
	run := func(t *testing.T, store harness.Storage, vary func(t *testing.T, store harness.Storage, sessionID string)) (*harness.Harness, context.CancelFunc, string) {
		t.Helper()
		gate := &readGateStore{Storage: store, arrived: make(chan struct{}), release: make(chan struct{})}
		script := newScriptModel(publicTurn())
		script.gate = make(chan struct{})
		var (
			mu      sync.Mutex
			armed   bool
			session string
		)
		observe := func(fact harness.HarnessFact) {
			if fact.Kind != harness.FactInvalidation {
				return
			}
			mu.Lock()
			fire := armed
			armed = false
			mu.Unlock()
			if fire { // the turn's result publication is the last durable advance before the handoff
				foreignPublicRevision(t, store, session)
			}
		}
		h, cancel := newObservedPublicHarness(t, gate, script, nil, observe)
		session = createSession(t, h)
		if _, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived
		if _, err := submit(t, h, session, "op-2", harness.MessageModeRegular, "s1"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		mu.Lock()
		armed = true
		mu.Unlock()
		gate.arm()
		script.releaseGate()

		select {
		case <-gate.arrived: // the real rematerialization is in flight
		case <-time.After(10 * time.Second):
			t.Fatalf("the handoff's rematerialization never ran")
		}
		vary(t, store, session)
		close(gate.release)
		return h, cancel, session
	}

	settlementCount := func(t *testing.T, store harness.Storage, sessionID, operationID string) int {
		t.Helper()
		entries, err := store.ReadEntries(context.Background(), sessionID, 0)
		if err != nil {
			t.Fatalf("read entries: %v", err)
		}
		count := 0
		for _, entry := range entries {
			if entry.Kind == harness.EntryOperationSettlement && entry.OperationID == operationID {
				count++
			}
		}
		return count
	}

	t.Run("a rematerialized quiet state settles ordinary failure", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			h, cancel, session := run(t, store, func(t *testing.T, _ harness.Storage, _ string) {})
			defer cancel()

			rec := awaitSettled(t, h, session, "op-1")
			if rec.State.Status != harness.OperationFailure || rec.State.Terminal == nil ||
				!strings.Contains(rec.State.Terminal.Detail, "changed concurrently") {
				t.Fatalf("op-1 = %+v, want the ordinary failure settlement of the propagated conflict", rec.State)
			}
			if got := settlementCount(t, store, session, "op-1"); got != 1 {
				t.Fatalf("op-1 settlements = %d, want exactly one with no handoff retry", got)
			}
			awaitSettled(t, h, session, "op-2") // the head drains as the successor
			snap, err := h.SnapshotSession(context.Background(), session)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 &&
					fact.Input.Content[0].Text == "s1" && fact.OperationID != "op-2" {
					t.Fatalf("the delivered input is owned by %q, want op-2", fact.OperationID)
				}
			}
		})
	})

	t.Run("a rematerialized retained active effect leaves running state for recovery", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			resultID := strings.Repeat("b", 32)
			h, cancel, session := run(t, store, func(t *testing.T, s harness.Storage, sid string) {
				foreignPublicActiveEffect(t, s, sid, "op-1", resultID)
			})

			// the outer policy writes nothing behind a retained active effect:
			// join the run's convergence through Wait. ExecutionBusy embeds
			// the durable CurrentOperationID, which stays "op-1" here, so it
			// is never the retirement signal for this case.
			cancel()
			if err := h.Wait(context.Background()); err != nil {
				t.Fatalf("Wait: %v", err)
			}

			// the lost buffers are discarded without delivery and no successor
			// was admitted behind the still-current Operation
			after, err := h.SnapshotSession(context.Background(), session)
			if err != nil {
				t.Fatalf("snapshot after the loss: %v", err)
			}
			if len(after.Steering) != 0 || len(after.Queued) != 0 {
				t.Fatalf("buffers after the loss = %+v / %+v, want both discarded", after.Steering, after.Queued)
			}
			for _, fact := range after.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 && fact.Input.Content[0].Text == "s1" {
					t.Fatalf("the lost head committed input entry %s", fact.EntryID)
				}
			}
			if _, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: session, Kind: harness.RegisterOperation, OperationID: "op-2"}); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("op-2 register = %v, want no successor admitted", err)
			}

			// the raw durable state: the running retained intent, no settlement
			state := recoverOpStateAt(t, store, session, "op-1")
			if state.Status != "running" || state.ActiveEffect == nil || state.ActiveEffect.ResultEntryID != resultID {
				t.Fatalf("op-1 = %+v, want the running retained intent left for recovery", state)
			}
			if got := settlementCount(t, store, session, "op-1"); got != 0 {
				t.Fatalf("op-1 settlements = %d, want none: no rewrite behind a retained active effect", got)
			}

			if err := harness.Recover(context.Background(), store); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			state = recoverOpStateAt(t, store, session, "op-1")
			if state.Status != "interruption" || state.ActiveEffect != nil || state.Terminal == nil ||
				state.Terminal.Detail != recoveredInterruptionDetail {
				t.Fatalf("recovered op-1 = %+v, want the standard recovery interruption", state)
			}
			if got := settlementCount(t, store, session, "op-1"); got != 1 {
				t.Fatalf("op-1 settlements after recovery = %d, want exactly the recovery's one", got)
			}
		})
	})

	t.Run("a rematerialized already-settled state is never rewritten", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			settlementID := strings.Repeat("c", 32)
			h, cancel, session := run(t, store, func(t *testing.T, s harness.Storage, sid string) {
				foreignPublicSettlement(t, s, sid, "op-1", settlementID)
			})
			defer cancel()

			awaitSettled(t, h, session, "op-2") // the cleared current Operation lets the successor admit
			state := recoverOpStateAt(t, store, session, "op-1")
			if state.Status != "success" || state.Terminal == nil || state.Terminal.Detail != "" ||
				state.Terminal.SettlementEntry.EntryID != settlementID {
				t.Fatalf("op-1 = %+v, want the foreign settlement untouched", state)
			}
			if got := settlementCount(t, store, session, "op-1"); got != 1 {
				t.Fatalf("op-1 settlements = %d, want exactly the foreign one and no rewrite", got)
			}
			rec := awaitSettled(t, h, session, "op-2")
			if rec.State.Status != harness.OperationSuccess {
				t.Fatalf("op-2 settled %q, want the successor delivered after the foreign settle", rec.State.Status)
			}
			snap, err := h.SnapshotSession(context.Background(), session)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 &&
					fact.Input.Content[0].Text == "s1" && fact.OperationID != "op-2" {
					t.Fatalf("the delivered input is owned by %q, want op-2", fact.OperationID)
				}
			}
		})
	})
}

// TestSteeringChildCompletionEmptySuccessOverStores proves the completion
// rendering over a clean child handoff on both real stores: the child's
// earlier assistant text is never aggregated — the latest settlement, a
// successful textless successor whose output is a valid refusal-only
// completion, renders exactly the empty-success fallback — and the parent
// completion is withheld until the successor retires.
func TestSteeringChildCompletionEmptySuccessOverStores(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		var (
			mu    sync.Mutex
			calls int
		)
		firstArrived := make(chan struct{})
		firstGate := make(chan struct{})
		successorArrived := make(chan struct{})
		successorGate := make(chan struct{})
		modelFn := func(ctx context.Context, _ model.Request) (model.Stream, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			switch n {
			case 1:
				close(firstArrived)
				<-firstGate
				return summaryTurnStream("earlier text", model.Usage{InputTokens: 2, OutputTokens: 1}), nil
			case 2:
				close(successorArrived)
				select {
				case <-successorGate:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				// the successful textless successor: a valid refusal-only output
				return &publicScriptStream{deltas: []model.StreamDelta{
					{HasChoice: true, Role: "assistant", RefusalFragment: "no", FinishReason: "stop"},
					{Usage: &model.Usage{InputTokens: 2, OutputTokens: 1}},
				}}, nil
			default:
				return publicTurnStream(), nil // the parent's completion operation
			}
		}
		f := newPublicFixture(t, store, newScriptModel(), modelFn)
		defer f.close()
		parent := createSession(t, f.h)

		launched, err := f.h.LaunchChildSession(ctx, harness.LaunchChildRequest{
			ParentSessionID: parent, AgentType: "coder",
			Content:     []model.ContentPart{{Kind: model.PartText, Text: "child work"}},
			OperationID: "child-op-1", MaxConcurrent: 2, OutputLimit: 4096,
		})
		if err != nil {
			t.Fatalf("LaunchChildSession: %v", err)
		}
		child := launched.ChildSessionID
		select {
		case <-firstArrived: // the child's first turn is parked
		case <-time.After(10 * time.Second):
			t.Fatalf("the child's first turn never started")
		}
		if _, err := submit(t, f.h, child, "child-op-2", harness.MessageModeRegular, "continue"); err != nil {
			t.Fatalf("child steering submit: %v", err)
		}
		close(firstGate) // the first turn completes with steering waiting: the clean handoff

		first := awaitSettled(t, f.h, child, "child-op-1")
		if first.State.Status != harness.OperationSuccess || first.State.Terminal == nil || first.State.Terminal.Detail != "" {
			t.Fatalf("child-op-1 = %+v, want the handoff's silent success", first.State)
		}
		select {
		case <-successorArrived: // the textless successor is parked mid-execution
		case <-time.After(10 * time.Second):
			t.Fatalf("the successor's turn never started")
		}

		// the parent completion is withheld while the successor runs: the
		// parent carries no runtime-origin input yet
		parked, err := f.h.SnapshotSession(ctx, parent)
		if err != nil {
			t.Fatalf("parked parent snapshot: %v", err)
		}
		for _, fact := range parked.Facts {
			if fact.Kind == harness.EntryInput && fact.Input != nil && fact.Input.Origin == harness.InputOriginRuntime {
				t.Fatalf("the parent received its completion before the successor retired: %+v", fact.Input)
			}
		}

		close(successorGate) // the textless successor settles and retires
		second := awaitSettled(t, f.h, child, "child-op-2")
		if second.State.Status != harness.OperationSuccess {
			t.Fatalf("child-op-2 = %+v, want the textless successor's success", second.State)
		}
		deadline := time.Now().Add(10 * time.Second)
		var completion *harness.HistoryFact
		for completion == nil {
			snap, err := f.h.SnapshotSession(ctx, parent)
			if err != nil {
				t.Fatalf("parent snapshot: %v", err)
			}
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && fact.Input.Origin == harness.InputOriginRuntime && completion == nil {
					completion = &fact
				}
			}
			if completion == nil {
				if time.Now().After(deadline) {
					t.Fatalf("the parent completion never arrived after the successor retired")
				}
				time.Sleep(time.Millisecond)
			}
		}
		awaitSettled(t, f.h, parent, completion.OperationID)
		if got := completion.Input.Content[0].Text; got != "Task completed." {
			t.Fatalf("parent completion = %q, want exactly the empty-success fallback of the latest settlement", got)
		}

		// the earlier assistant text survives child-owned and is never
		// aggregated into the completion
		childSnap, err := f.h.SnapshotSession(ctx, child)
		if err != nil {
			t.Fatalf("child snapshot: %v", err)
		}
		earlier := false
		for _, fact := range childSnap.Facts {
			if fact.Kind == harness.EntryAssistant && fact.Assistant != nil && fact.OperationID == "child-op-1" &&
				len(fact.Assistant.Content) > 0 && fact.Assistant.Content[0].Text == "earlier text" {
				earlier = true
			}
		}
		if !earlier {
			t.Fatalf("the child's earlier assistant text is missing from its own history")
		}
		if strings.Contains(completion.Input.Content[0].Text, "earlier text") {
			t.Fatalf("parent completion = %q, want no earlier-text aggregation", completion.Input.Content[0].Text)
		}
	})
}

// TestSteeringAdmissionDuplicateAndFailure proves the duplicate-and-failure
// admission rows over both real stores: regular steering heads sharing one
// caller identity with changed payloads stay non-idempotent while buffered
// and the first admitted payload wins with the duplicate final-dropping
// once; a failed preparation of the selected head drops exactly that head
// while the next proceeds; and an identity that wins admission elsewhere
// during the selected head's parked preparation — a terminal compact
// Operation in the same Session, or an ordinary Submit under that identity
// in another Session — rejects the head through the landed identity rules
// with no alias, after which the next head still proceeds. Only graph-valid
// committed state is injected, and the foreign graph is validated.
func TestSteeringAdmissionDuplicateAndFailure(t *testing.T) {
	t.Run("duplicate identities keep the first payload and drop the duplicate once", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare
			<-script.arrived
			if res, err := submit(t, f.h, session, "op-dup", harness.MessageModeRegular, "first"); err != nil || res.Disposition != harness.DispositionSteering {
				t.Fatalf("first head = %+v err %v, want buffered steering", res, err)
			}
			if res, err := submit(t, f.h, session, "op-dup", harness.MessageModeRegular, "changed"); err != nil || res.Disposition != harness.DispositionSteering {
				t.Fatalf("duplicate head = %+v err %v, want buffered steering with no deduplication", res, err)
			}
			script.releaseGate()

			if rec := awaitSettled(t, f.h, session, "op-1"); rec.State.Status != harness.OperationSuccess {
				t.Fatalf("op-1 settled %q, want the handoff success", rec.State.Status)
			}
			winner := awaitSettled(t, f.h, session, "op-dup") // the first payload's own Operation
			if winner.State.Status != harness.OperationSuccess {
				t.Fatalf("op-dup settled %q, want the first admitted payload's success", winner.State.Status)
			}
			awaitStoreQuiet(t, f.h, session)
			snap, err := f.h.SnapshotSession(ctx, session)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			firstOwned := false
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 {
					if fact.Input.Content[0].Text == "changed" {
						t.Fatalf("the changed duplicate payload committed input entry %s", fact.EntryID)
					}
					if fact.Input.Content[0].Text == "first" && fact.OperationID == "op-dup" {
						firstOwned = true
					}
				}
			}
			if !firstOwned {
				t.Fatalf("the first payload never committed under its own Operation")
			}
			if len(snap.Steering) != 0 {
				t.Fatalf("steering after the final drop = %+v, want the duplicate dropped once", snap.Steering)
			}
		})
	})

	t.Run("the selected head's failed preparation drops only that head", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			f.prepareHook = func(call int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
				if call == 1 { // the selected head's admission preparation fails
					return harness.PreparedExecution{}, errors.New("selected head preparation broke")
				}
				return scriptPrepared(script), nil
			}
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare
			<-script.arrived
			if _, err := submit(t, f.h, session, "op-fail", harness.MessageModeRegular, "will-fail"); err != nil {
				t.Fatalf("failing head submit: %v", err)
			}
			if _, err := submit(t, f.h, session, "op-next", harness.MessageModeRegular, "next"); err != nil {
				t.Fatalf("next head submit: %v", err)
			}
			script.releaseGate()

			if rec := awaitSettled(t, f.h, session, "op-1"); rec.State.Status != harness.OperationSuccess {
				t.Fatalf("op-1 settled %q, want the handoff success", rec.State.Status)
			}
			if _, err := f.h.ReadOperation(context.Background(), session, "op-fail"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("op-fail register = %v, want the failed head dropped with nothing retained", err)
			}
			next := awaitSettled(t, f.h, session, "op-next")
			if next.State.Status != harness.OperationSuccess {
				t.Fatalf("op-next settled %q, want the next head admitted after the failure", next.State.Status)
			}
			snap, err := f.h.SnapshotSession(context.Background(), session)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 && fact.Input.Content[0].Text == "will-fail" {
					t.Fatalf("the failed head committed input entry %s", fact.EntryID)
				}
			}
		})
	})

	seedCompactWinner := func(t *testing.T, store harness.Storage, sessionID, operationID, settlementID string) {
		t.Helper()
		if err := store.Transact(context.Background(), func(tx harness.Transaction) error {
			settlement, err := json.Marshal(harness.OperationSettlementEntry{
				SessionID: sessionID, EntryID: settlementID, OperationID: operationID,
				Status: harness.OperationSuccess,
			})
			if err != nil {
				return err
			}
			if _, err := tx.InsertEntry(harness.EntryDraft{
				SessionID: sessionID, ID: settlementID, OperationID: operationID,
				Kind: harness.EntryOperationSettlement, Payload: settlement,
			}); err != nil {
				return err
			}
			// build the winner's admission from the real register's exact
			// codec bytes: only the identity, kind and admitted-entry members
			// differ from the live capture's wire form
			live, err := tx.ReadRegister(harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: "op-1"})
			if err != nil {
				return err
			}
			var liveWire publicOperationWire
			if err := json.Unmarshal(live.Payload, &liveWire); err != nil {
				return err
			}
			var admissionDoc map[string]json.RawMessage
			if err := json.Unmarshal(liveWire.Admission, &admissionDoc); err != nil {
				return err
			}
			admissionDoc["operation_id"], err = json.Marshal(operationID)
			if err != nil {
				return err
			}
			admissionDoc["request_kind"], err = json.Marshal(harness.RequestKindCompact)
			if err != nil {
				return err
			}
			delete(admissionDoc, "admitted_entry") // the codec requires it absent for the compact kind
			admission, err := json.Marshal(admissionDoc)
			if err != nil {
				return err
			}
			stamped := time.Now().UTC()
			wire := publicOperationWire{
				Admission: admission,
				State: publicOperationStateWire{
					Status:           harness.OperationSuccess,
					StartedAt:        stamped,
					SettledAt:        &stamped,
					Usage:            json.RawMessage(`{"by_model":[]}`),
					PendingToolCalls: []harness.PendingToolCall{},
					Terminal: &harness.OperationTerminal{
						SettlementEntry: harness.EntryRef{SessionID: sessionID, EntryID: settlementID},
					},
				},
			}
			payload, err := json.Marshal(wire)
			if err != nil {
				return err
			}
			_, err = tx.InsertRegister(harness.RegisterDraft{
				Key:     harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID},
				Payload: payload,
			})
			return err
		}); err != nil {
			t.Fatalf("compact winner: %v", err)
		}
	}

	t.Run("a wrong-kind winner during the parked preparation rejects without alias", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			prepareGate := make(chan struct{})
			releasePrepare := sync.OnceFunc(func() { close(prepareGate) })
			defer releasePrepare()
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			f.prepareHook = func(call int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
				if call == 1 { // the selected head's admission preparation parks
					<-prepareGate
				}
				return scriptPrepared(script), nil
			}
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare
			<-script.arrived
			if _, err := submit(t, f.h, session, "op-won", harness.MessageModeRegular, "wrong-kind head"); err != nil {
				t.Fatalf("selected head submit: %v", err)
			}
			if _, err := submit(t, f.h, session, "op-after", harness.MessageModeRegular, "after"); err != nil {
				t.Fatalf("next head submit: %v", err)
			}
			script.releaseGate()
			awaitSettled(t, f.h, session, "op-1") // the handoff settled
			<-f.prepare                           // the selected head's preparation is parked

			settlementID := strings.Repeat("d", 32)
			seedCompactWinner(t, store, session, "op-won", settlementID) // graph-valid committed winner
			releasePrepare()

			if rec := awaitSettled(t, f.h, session, "op-after"); rec.State.Status != harness.OperationSuccess {
				t.Fatalf("op-after settled %q, want the next head proceed after the wrong-kind rejection", rec.State.Status)
			}
			awaitStoreQuiet(t, f.h, session)
			// the winner stays exactly its committed compact self: no message
			// alias, no input entry, the one foreign settlement
			wire := recoverOpStateAt(t, store, session, "op-won")
			if wire.Status != "success" || wire.Terminal == nil || wire.Terminal.SettlementEntry.EntryID != settlementID {
				t.Fatalf("op-won = %+v, want the compact winner untouched", wire)
			}
			snap, err := f.h.SnapshotSession(ctx, session)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			for _, fact := range snap.Facts {
				if fact.Kind == harness.EntryInput && fact.OperationID == "op-won" {
					t.Fatalf("the wrong-kind head aliased onto the compact winner with input entry %s", fact.EntryID)
				}
				if fact.Kind == harness.EntryOperationSettlement && fact.OperationID == "op-won" && fact.EntryID != settlementID {
					t.Fatalf("an extra settlement %s appeared under the winner", fact.EntryID)
				}
			}
		})
	})

	t.Run("a foreign winner during the parked preparation rejects without alias", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			prepareGate := make(chan struct{})
			releasePrepare := sync.OnceFunc(func() { close(prepareGate) })
			defer releasePrepare()
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			f.prepareHook = func(call int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
				if call == 1 { // the selected head's admission preparation parks
					<-prepareGate
				}
				return scriptPrepared(script), nil
			}
			session := createSession(t, f.h)
			other := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare
			<-script.arrived
			if _, err := submit(t, f.h, session, "op-taken", harness.MessageModeRegular, "foreign head"); err != nil {
				t.Fatalf("selected head submit: %v", err)
			}
			if _, err := submit(t, f.h, session, "op-after", harness.MessageModeRegular, "after"); err != nil {
				t.Fatalf("next head submit: %v", err)
			}
			script.releaseGate()
			awaitSettled(t, f.h, session, "op-1") // the handoff settled
			<-f.prepare                           // the selected head's preparation is parked

			// an ordinary Submit under that identity wins in another Session
			if res, err := submit(t, f.h, other, "op-taken", harness.MessageModeRegular, "won elsewhere"); err != nil || res.Disposition != harness.DispositionAdmitted {
				t.Fatalf("foreign winning submit = %+v err %v, want admitted", res, err)
			}
			releasePrepare()

			if rec := awaitSettled(t, f.h, session, "op-after"); rec.State.Status != harness.OperationSuccess {
				t.Fatalf("op-after settled %q, want the next head proceed after the foreign rejection", rec.State.Status)
			}
			awaitStoreQuiet(t, f.h, session)
			// no alias: this Session never gained an Operation or input under
			// the taken identity, and the foreign Session's graph validates
			snap, err := f.h.SnapshotSession(ctx, session)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			for _, op := range snap.Operations {
				if op.Admission.OperationID == "op-taken" {
					t.Fatalf("the rejected head aliased as an Operation in this Session")
				}
			}
			if _, err := f.h.ReadOperation(ctx, other, "op-taken"); err != nil {
				t.Fatalf("the foreign winner = %v, want its ordinary success readable and its graph validated", err)
			}
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	})
}
