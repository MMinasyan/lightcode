// External-package public Harness suite: the coherent Session snapshot and
// Session listing rows through public operations only, over both storage
// implementations.
package harness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

// snapshotTurnPrepared returns the snapshot rows' prepared execution: the
// scripted model over an immediate-success tool plan whose result carries
// tool-owned metadata, so the committed history covers the assistant,
// tool-call, and tool-result payload shapes.
func snapshotTurnPrepared(script *scriptModel) harness.PreparedExecution {
	return harness.PreparedExecution{
		Capture: publicCapture(),
		Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
			return harness.Execution{
				Model:         script.effect,
				CompactModel:  script.effect,
				NormalizeTool: publicNormalize,
				Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
					return harness.PreparedTool{Permissions: publicPermission, Immediate: &harness.ToolOutcome{
						Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call"},
						Metadata: json.RawMessage(`{"n":[1,1.0],"s":"<>&"}`),
					}}
				},
			}, nil
		},
	}
}

// TestPublicReadSessionHistoryOwnedCapture proves the direct narrow history
// producer over both stores: exact envelope identity/kind/order of every
// committed entry with hook evidence excluded, the revision pair sampled in
// the same hold as the full snapshot pair, cross-producer equality with the
// full snapshot's facts, and nested returned-value ownership against a
// serialized expectation captured before mutation.
func TestPublicReadSessionHistoryOwnedCapture(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		fixture := harness.SeedFactFixture(t, store)
		hctx, cancel := context.WithCancel(ctx)
		h, err := harness.New(hctx, harness.Dependencies{
			Storage: store,
			Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
				return harness.PreparedExecution{}, errors.New("the narrow read fixture never prepares")
			},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer func() {
			cancel()
			_ = h.Wait(context.Background())
		}()

		facts, pair, err := h.ReadSessionHistory(ctx, fixture.SessionID)
		if err != nil {
			t.Fatalf("ReadSessionHistory: %v", err)
		}
		if fixture.HookEntryID == "" {
			t.Fatal("the owning fixture lost its hook evidence entry")
		}
		if len(facts) != len(fixture.Identities) {
			t.Fatalf("history = %d facts, want the %d non-hook entries", len(facts), len(fixture.Identities))
		}
		for i, want := range fixture.Identities {
			got := facts[i]
			if got.EntryID != want.EntryID || got.OperationID != want.OperationID || got.Kind != want.Kind {
				t.Fatalf("fact %d = (%s/%s/%s), want the fixture envelope (%s/%s/%s)",
					i, got.EntryID, got.OperationID, got.Kind, want.EntryID, want.OperationID, want.Kind)
			}
			if got.EntryID == fixture.HookEntryID {
				t.Fatalf("fact %d leaked the hook evidence entry", i)
			}
		}

		snap, err := h.SnapshotSession(ctx, fixture.SessionID)
		if err != nil {
			t.Fatalf("SnapshotSession: %v", err)
		}
		wantPair := harness.SessionRevision{DurableRevision: snap.Session.Revision, LocalRevision: snap.LocalRevision}
		if pair != wantPair {
			t.Fatalf("history pair = %+v, want the full snapshot's %+v", pair, wantPair)
		}
		if !reflect.DeepEqual(facts, snap.Facts) {
			t.Fatal("the narrow history producer and the full snapshot disagree on the committed facts")
		}

		// The serialized expectation is captured BEFORE any mutation: the
		// returned values are owned, so both a fresh narrow read and a fresh
		// full snapshot must still serialize to it.
		expected, err := json.Marshal(facts)
		if err != nil {
			t.Fatalf("marshal the baseline history: %v", err)
		}
		for i := range facts {
			fact := &facts[i]
			if fact.Input != nil && len(fact.Input.Content) > 0 {
				fact.Input.Content[0].Text = "mutated"
				if fact.Input.Content[0].Extra == nil {
					fact.Input.Content[0].Extra = model.Extra{}
				}
				fact.Input.Content[0].Extra["part"] = json.RawMessage(`"mutated"`)
			}
			if fact.Assistant != nil {
				if len(fact.Assistant.Content) > 0 {
					fact.Assistant.Content[0].Text = "mutated"
				}
				if fact.Assistant.Extra == nil {
					fact.Assistant.Extra = model.Extra{}
				}
				fact.Assistant.Extra["reasoning"] = json.RawMessage(`null`)
				if fact.Assistant.Usage != nil {
					fact.Assistant.Usage.InputTokens = 999
				}
				if len(fact.Assistant.ToolCalls) > 0 {
					fact.Assistant.ToolCalls[0].NormalizedArguments = json.RawMessage(`{"mutated":true}`)
					if fact.Assistant.ToolCalls[0].Extra == nil {
						fact.Assistant.ToolCalls[0].Extra = model.Extra{}
					}
					fact.Assistant.ToolCalls[0].Extra["call"] = json.RawMessage(`null`)
				}
			}
			if fact.ToolResult != nil {
				fact.ToolResult.Metadata = json.RawMessage(`"mutated"`)
			}
			if fact.Signal != nil && fact.Signal.RelatedMember != nil {
				fact.Signal.RelatedMember.ID = "mutated"
			}
			if fact.Compaction != nil && fact.Compaction.Usage != nil {
				fact.Compaction.Usage.InputTokens = 999
			}
			if fact.Settlement != nil {
				if fact.Settlement.Model != nil {
					fact.Settlement.Model.Model = "mutated"
				}
				if fact.Settlement.Usage != nil {
					fact.Settlement.Usage.InputTokens = 999
				}
			}
		}
		again, _, err := h.ReadSessionHistory(ctx, fixture.SessionID)
		if err != nil {
			t.Fatalf("ReadSessionHistory after mutation: %v", err)
		}
		got, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("marshal the fresh history: %v", err)
		}
		if !bytes.Equal(got, expected) {
			t.Fatal("a mutation of the returned history reached the owner or another read")
		}
		snapAfter, err := h.SnapshotSession(ctx, fixture.SessionID)
		if err != nil {
			t.Fatalf("SnapshotSession after mutation: %v", err)
		}
		snapGot, err := json.Marshal(snapAfter.Facts)
		if err != nil {
			t.Fatalf("marshal the fresh snapshot facts: %v", err)
		}
		if !bytes.Equal(snapGot, expected) {
			t.Fatal("a mutation of the returned history reached the full snapshot producer")
		}
	})
}

// TestPublicSnapshotSessionReadsAndOwns proves the complete owned read over
// both stores: sorted warm Operations, the typed fact union in ascending
// sequence, the durable Session record, and the quiet busy fact.
func TestPublicSnapshotSessionReadsAndOwns(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		script := newScriptModel(publicTurn("call-1"), publicTurn(), publicTurn())
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		f.prepareHook = func(_ int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			return snapshotTurnPrepared(script), nil
		}
		session := createSession(t, f.h)

		// Two admissions in non-lexicographic identity order: the warm graph
		// appends, so only the returned copy's sort proves the contract.
		// op-z publishes one call and completes on its second turn; op-a is
		// admitted only after op-z's terminal committed, so its routing is
		// deterministic.
		if _, err := submit(t, f.h, session, "op-z", harness.MessageModeRegular, "hello op-z"); err != nil {
			t.Fatalf("submit op-z: %v", err)
		}
		<-script.arrived // the published call's turn
		<-script.arrived // the completing turn
		awaitTerminal(t, f.h, session, "op-z")
		if _, err := submit(t, f.h, session, "op-a", harness.MessageModeRegular, "hello op-a"); err != nil {
			t.Fatalf("submit op-a: %v", err)
		}
		<-script.arrived // op-a's completing turn
		awaitTerminal(t, f.h, session, "op-a")
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		pristine, err := f.h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("SnapshotSession: %v", err)
		}
		if len(pristine.Operations) != 2 ||
			pristine.Operations[0].Admission.OperationID != "op-a" ||
			pristine.Operations[1].Admission.OperationID != "op-z" {
			t.Fatalf("operations = %+v, want lexicographic order over the warm append order", pristine.Operations)
		}
		if pristine.ExecutionBusy || pristine.LocalRevision == 0 {
			t.Fatalf("quiet snapshot = busy %v local %d", pristine.ExecutionBusy, pristine.LocalRevision)
		}
		if pristine.Session.Identity.SessionID != session || pristine.Session.State.Lifecycle != harness.LifecycleOpen {
			t.Fatalf("session record = %+v", pristine.Session)
		}

		wantKinds := []harness.EntryKind{
			harness.EntryInput, harness.EntryAssistant, harness.EntryToolResult, harness.EntryAssistant,
			harness.EntryOperationSettlement, harness.EntryInput, harness.EntryAssistant, harness.EntryOperationSettlement,
		}
		if len(pristine.Facts) != len(wantKinds) {
			t.Fatalf("facts = %d, want %d", len(pristine.Facts), len(wantKinds))
		}
		for i, fact := range pristine.Facts {
			if fact.Kind != wantKinds[i] {
				t.Fatalf("fact[%d].Kind = %q, want %q", i, fact.Kind, wantKinds[i])
			}
			if fact.OperationID == "" || fact.EntryID == "" || fact.Sequence == 0 || fact.CommittedAt.IsZero() {
				t.Fatalf("fact[%d] loses its envelope identity: %+v", i, fact)
			}
		}
		assistant := pristine.Facts[1].Assistant
		if assistant == nil || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call-1" ||
			assistant.ToolCalls[0].ResultEntryID == "" {
			t.Fatalf("assistant fact = %+v", assistant)
		}
		result := pristine.Facts[2].ToolResult
		// the durable codec's JSON encoding escapes the raw metadata's markup
		if result == nil || result.ToolCallID != "call-1" ||
			string(result.Metadata) != `{"n":[1,1.0],"s":"\u003c\u003e\u0026"}` {
			t.Fatalf("tool result fact = %+v", result)
		}
		for i, seq := range []int64{1, 2, 3, 4, 5, 6, 7, 8} {
			if pristine.Facts[i].Sequence != seq {
				t.Fatalf("fact[%d].Sequence = %d, want ascending %d", i, pristine.Facts[i].Sequence, seq)
			}
		}

		listed, err := f.h.ListSessions(ctx)
		if err != nil || len(listed) != 1 || listed[0].Identity.SessionID != session {
			t.Fatalf("ListSessions = %+v err %v", listed, err)
		}
	})
}

// TestPublicSnapshotSessionPendingPublications proves the pending FIFO and
// ExecutionBusy rows over both stores: each Submit's reservation, buffer
// enqueue, and release advance the local revision; a delivery's selected head
// stays pending until its attempt resolves — a parked queued admission is
// still listed while it prepares, and the adoption removes it in the same
// critical section; Harness loss discards the buffers.
func TestPublicSnapshotSessionPendingPublications(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		script := newScriptModel(publicTurn())
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		// preparation call 0 is op-1's admission; the post-terminal drain's
		// admission parks on its gate so the selected-but-unresolved delivery
		// state stays stable. The steering item leads the drain: the waiting
		// steering item hands the settled Operation off at the natural-success
		// boundary, and the drain admits it before the queued head under its
		// own identity.
		prepGate := make(chan struct{})
		drainGate := make(chan struct{})
		releasePrep := sync.OnceFunc(func() { close(prepGate) })
		releaseDrain := sync.OnceFunc(func() { close(drainGate) })
		defer releasePrep()
		defer releaseDrain()
		defer script.releaseGate()
		f.prepareHook = func(call int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			switch call {
			case 0:
				<-prepGate
			case 1:
				<-drainGate
			}
			return scriptPrepared(script), nil
		}
		session := createSession(t, f.h)

		baseline, err := f.h.SnapshotSession(ctx, session)
		if err != nil || baseline.LocalRevision != 0 || baseline.ExecutionBusy {
			t.Fatalf("baseline = %+v err %v", baseline, err)
		}

		// The idle admission holds the reservation across its preparation.
		submitDone := make(chan struct{})
		go func() {
			defer close(submitDone)
			if _, err := f.h.Submit(ctx, harness.SubmitRequest{
				SessionID: session, OperationID: "op-1", Origin: harness.InputOriginUser,
				Content: []model.ContentPart{{Kind: model.PartText, Text: "hello"}}, Mode: harness.MessageModeRegular,
			}); err != nil {
				t.Errorf("submit op-1: %v", err)
			}
		}()
		<-f.prepare // the preparation entered and parks on its gate; the reservation is held
		reserved, err := f.h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("snapshot during preparation: %v", err)
		}
		if !reserved.ExecutionBusy || len(reserved.Operations) != 0 || len(reserved.Facts) != 0 {
			t.Fatalf("snapshot during preparation = busy %v ops %d facts %d, want the entirely-old state with the busy reservation",
				reserved.ExecutionBusy, len(reserved.Operations), len(reserved.Facts))
		}
		if reserved.LocalRevision != baseline.LocalRevision+1 {
			t.Fatalf("reservation revision = %d, want %d", reserved.LocalRevision, baseline.LocalRevision+1)
		}
		releasePrep() // op-1's preparation proceeds
		<-submitDone
		<-script.arrived // the run is parked at its boundary
		installed, err := f.h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("snapshot after installation: %v", err)
		}
		if !installed.ExecutionBusy || len(installed.Operations) != 1 || installed.Operations[0].Admission.OperationID != "op-1" {
			t.Fatalf("snapshot after installation = %+v", installed)
		}
		if installed.LocalRevision != reserved.LocalRevision+2 { // the run slot's installation and the reservation release
			t.Fatalf("installed revision = %d, want %d", installed.LocalRevision, reserved.LocalRevision+2)
		}

		// Active routing buffers by mode: each Submit's reservation, enqueue,
		// and release are three publications.
		if _, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1"); err != nil {
			t.Fatalf("queued submit: %v", err)
		}
		if _, err := submit(t, f.h, session, "op-3", harness.MessageModeRegular, "steer-1"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		buffered, err := f.h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("snapshot after buffering: %v", err)
		}
		if len(buffered.Steering) != 1 || buffered.Steering[0].OperationID != "op-3" ||
			len(buffered.Queued) != 1 || buffered.Queued[0].OperationID != "op-2" {
			t.Fatalf("buffers after the submits = %+v / %+v", buffered.Steering, buffered.Queued)
		}
		if buffered.LocalRevision != installed.LocalRevision+6 {
			t.Fatalf("buffered revision = %d, want %d", buffered.LocalRevision, installed.LocalRevision+6)
		}
		buffered.Steering[0].Content[0].Text = "mutated"
		buffered.Queued[0].Content[0].Text = "mutated"
		reread, err := f.h.SnapshotSession(ctx, session)
		if err != nil || reread.Steering[0].Content[0].Text != "steer-1" || reread.Queued[0].Content[0].Text != "queued-1" {
			t.Fatalf("a pending input's content mutation reached the coordinator: %q / %q err %v",
				reread.Steering[0].Content[0].Text, reread.Queued[0].Content[0].Text, err)
		}

		// Releasing the boundary settles op-1 through the natural-success
		// handoff: the waiting steering item is selected by the post-terminal
		// drain — before the queued head — and the drain's admission parks in
		// its preparation with both heads still pending.
		script.releaseGate()
		<-f.prepare // the drain admission of the steering head (op-3) is preparing
		parked, err := f.h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("snapshot during the parked delivery: %v", err)
		}
		if len(parked.Steering) != 1 || parked.Steering[0].OperationID != "op-3" {
			t.Fatalf("steering buffer during the parked delivery = %+v, want the selected head still pending until adoption", parked.Steering)
		}
		if len(parked.Queued) != 1 || parked.Queued[0].OperationID != "op-2" {
			t.Fatalf("queued buffer during the parked delivery = %+v, want the queued head pending behind the steering head", parked.Queued)
		}
		if parked.Session.State.CurrentOperationID != "" {
			t.Fatalf("current operation during the parked delivery = %q, want the handed-off predecessor settled", parked.Session.State.CurrentOperationID)
		}
		for _, op := range parked.Operations {
			if op.Admission.OperationID == "op-2" || op.Admission.OperationID == "op-3" {
				t.Fatalf("the pending item %s committed before its delivery completed", op.Admission.OperationID)
			}
			if op.Admission.OperationID == "op-1" && op.State.Status != harness.OperationSuccess {
				t.Fatalf("op-1 = %q during the parked delivery, want the handoff's quiet success", op.State.Status)
			}
		}
		// the handoff's carried reservation is the only local publication
		// between the buffering and the parked admission: the drain consumes
		// it instead of acquiring a second one
		if parked.LocalRevision != buffered.LocalRevision+1 {
			t.Fatalf("parked revision = %d, want %d", parked.LocalRevision, buffered.LocalRevision+1)
		}
		releaseDrain()   // the steering head admits, runs, and settles; the queued head follows
		<-script.arrived // the admitted steering Operation's execution started
		awaitTerminal(t, f.h, session, "op-3")
		<-f.prepare      // the queued head's admission preparation
		<-script.arrived // the queued Operation's execution started
		awaitTerminal(t, f.h, session, "op-2")
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		final, err := f.h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("final snapshot: %v", err)
		}
		if final.ExecutionBusy || len(final.Steering) != 0 || len(final.Queued) != 0 {
			t.Fatalf("final snapshot = busy %v steering %d queued %d", final.ExecutionBusy, len(final.Steering), len(final.Queued))
		}
		if len(final.Operations) != 3 {
			t.Fatalf("final operations = %d, want the three split Operations", len(final.Operations))
		}
		for _, fact := range final.Facts {
			if fact.Kind != harness.EntryInput || fact.Input == nil || len(fact.Input.Content) == 0 {
				continue
			}
			switch fact.Input.Content[0].Text {
			case "hello":
				if fact.OperationID != "op-1" {
					t.Fatalf("admitted input owned by %q, want op-1", fact.OperationID)
				}
			case "steer-1":
				if fact.OperationID != "op-3" {
					t.Fatalf("steering input owned by %q, want its own Operation op-3", fact.OperationID)
				}
			case "queued-1":
				if fact.OperationID != "op-2" {
					t.Fatalf("queued input owned by %q, want op-2", fact.OperationID)
				}
			}
		}
	})

	t.Run("harness loss discards the buffers as one publication", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			script := newScriptModel(publicTurn())
			script.gate = make(chan struct{})
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			session := createSession(t, f.h)
			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("submit: %v", err)
			}
			<-script.arrived
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1"); err != nil {
				t.Fatalf("queued submit: %v", err)
			}
			buffered, err := f.h.SnapshotSession(ctx, session)
			if err != nil || len(buffered.Queued) != 1 {
				t.Fatalf("buffered snapshot = %+v err %v", buffered, err)
			}

			f.cancel()                                             // the Harness loss closes admission and discards the buffers
			go script.releaseGate()                                // the parked attempt unblocks; its effect settles and the run converges
			if err := f.h.Wait(context.Background()); err != nil { // joins every in-flight execution
				t.Fatalf("Wait: %v", err)
			}
			after, err := f.h.SnapshotSession(ctx, session)
			if err != nil {
				t.Fatalf("snapshot after the loss: %v", err)
			}
			if len(after.Queued) != 0 || len(after.Steering) != 0 {
				t.Fatalf("buffers after the loss = %+v / %+v", after.Steering, after.Queued)
			}
			// the discard publication plus the loss path's reservation,
			// release, and run-slot retirement
			if after.LocalRevision != buffered.LocalRevision+4 {
				t.Fatalf("discard revision = %d, want %d", after.LocalRevision, buffered.LocalRevision+4)
			}
		})
	})
}

// TestPublicSnapshotSessionCompactAndDeletion proves the compact barrier's
// old-or-new read, the compaction fact, and the deleted Session's typed
// absence from both the direct read and the listing.
func TestPublicSnapshotSessionCompactAndDeletion(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("Summary."))
		f := newCompactManualFixture(t, store, conversation, compact)
		defer f.close()
		session := createSession(t, f.h)
		if _, err := f.h.Submit(ctx, harness.SubmitRequest{
			SessionID: session, OperationID: "op-1", Origin: harness.InputOriginUser,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "hello"}}, Mode: harness.MessageModeRegular,
		}); err != nil {
			t.Fatalf("submit: %v", err)
		}
		awaitTerminal(t, f.h, session, "op-1")
		awaitQuiet(t, f.h, session) // the retiring run's drain publications land before the stable baseline
		before, err := f.h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("snapshot before the compact: %v", err)
		}

		compactPrepStarted := make(chan struct{})
		compactGate := make(chan struct{})
		releaseCompactGate := sync.OnceFunc(func() { close(compactGate) })
		// registered after the fixture close, so the parked preparation is
		// released before the fixture's cancellation and the store close on
		// every exit.
		defer releaseCompactGate()
		f.prepareHook = func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error) {
			if req.RequestKind == harness.RequestKindCompact {
				compactPrepStarted <- struct{}{}
				<-compactGate
			}
			return f.defaultPrep, nil
		}
		compactDone := make(chan error, 1)
		go func() {
			_, err := f.h.Compact(ctx, harness.CompactRequest{SessionID: session, OperationID: "comp-1"})
			compactDone <- err
		}()
		select {
		case <-compactPrepStarted: // the compact preparation is parked on its gate holding the reservation
		case err := <-compactDone: // a rejection before the preparation reports instead of parking the test forever
			t.Fatalf("Compact returned before its preparation started: %v", err)
		}
		duringCompact, err := f.h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("snapshot during the compact preparation: %v", err)
		}
		if !duringCompact.ExecutionBusy {
			t.Fatalf("snapshot during the compact preparation = busy %v, want the held reservation", duringCompact.ExecutionBusy)
		}
		for _, op := range duringCompact.Operations {
			if op.Admission.OperationID == "comp-1" {
				t.Fatalf("the compact Operation appeared before its commit")
			}
		}
		if duringCompact.LocalRevision <= before.LocalRevision {
			t.Fatalf("reservation revision = %d, want an advance past %d", duringCompact.LocalRevision, before.LocalRevision)
		}
		releaseCompactGate()
		if err := <-compactDone; err != nil {
			t.Fatalf("Compact: %v", err)
		}
		awaitTerminal(t, f.h, session, "comp-1")
		after, err := f.h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("snapshot after the compact: %v", err)
		}
		var compaction *harness.CompactionEntry
		for _, fact := range after.Facts {
			if fact.Compaction != nil {
				compaction = fact.Compaction
			}
		}
		if compaction == nil || compaction.Summary != "Summary." || compaction.Model != compactModelRef {
			t.Fatalf("compaction fact = %+v", compaction)
		}
		found := false
		for _, op := range after.Operations {
			if op.Admission.OperationID == "comp-1" {
				found = true
			}
		}
		if !found {
			t.Fatalf("the compact Operation is missing from the snapshot")
		}

		if _, err := f.h.ArchiveSession(ctx, session); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if err := f.h.DeleteSession(ctx, session); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		if _, err := f.h.SnapshotSession(ctx, session); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("snapshot after deletion = %v, want the not-found class", err)
		}
		listed, err := f.h.ListSessions(ctx)
		if err != nil || len(listed) != 0 {
			t.Fatalf("ListSessions after deletion = %+v err %v", listed, err)
		}
	})
}

// countingValidationStorage counts one Session's graph reads: the cache rows
// observe a warm coordinator's zero re-reads and a cold corrupt Session's
// revalidation on every list.
type countingValidationStorage struct {
	harness.Storage
	mu       sync.Mutex
	register map[string]int
	entries  map[string]int
}

func (s *countingValidationStorage) ReadRegisters(ctx context.Context, sessionID string) ([]harness.Register, error) {
	s.mu.Lock()
	s.register[sessionID]++
	s.mu.Unlock()
	return s.Storage.ReadRegisters(ctx, sessionID)
}

func (s *countingValidationStorage) ReadEntries(ctx context.Context, sessionID string, after int64) ([]harness.Entry, error) {
	s.mu.Lock()
	s.entries[sessionID]++
	s.mu.Unlock()
	return s.Storage.ReadEntries(ctx, sessionID, after)
}

func (s *countingValidationStorage) counts(sessionID string) (registers int, entries int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.register[sessionID], s.entries[sessionID]
}

// failingListStorage injects enumeration and materialization failures, and
// optionally parks one enumeration after the underlying store has returned its
// identity set but before the listing consumes it.
type failingListStorage struct {
	harness.Storage
	listErr       error
	entriesErrFor map[string]error
	listed        chan struct{} // non-nil arms the one-shot enumeration gate
	listRelease   chan struct{}
}

func (s *failingListStorage) ListSessionIDs(ctx context.Context) ([]string, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	ids, err := s.Storage.ListSessionIDs(ctx)
	if err != nil {
		return nil, err
	}
	if s.listed != nil {
		s.listed <- struct{}{}
		<-s.listRelease
	}
	return ids, err
}

func (s *failingListStorage) ReadEntries(ctx context.Context, sessionID string, after int64) ([]harness.Entry, error) {
	if err := s.entriesErrFor[sessionID]; err != nil {
		return nil, err
	}
	return s.Storage.ReadEntries(ctx, sessionID, after)
}

// TestPublicListSessionsValidationAndFailures proves the listing rows over
// both stores: sorted available records, corrupt and malformed rows omitted
// with typed direct errors, cold-corrupt revalidation, warm reads without
// history re-reads, deletion omission, and failure propagation.
func TestPublicListSessionsValidationAndFailures(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		f := newPublicFixture(t, store, newScriptModel(), nil)
		defer f.close()
		first := createSession(t, f.h)
		second := createSession(t, f.h)
		third := createSession(t, f.h)

		// Corrupt one sibling's register directly through the storage contract.
		reg := sessionRegister(t, store, first)
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(reg.Payload, &obj); err != nil {
			t.Fatalf("unmarshal session payload: %v", err)
		}
		obj["state"] = json.RawMessage(`{"lifecycle":"bogus"}`)
		bad, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal session payload: %v", err)
		}
		key := harness.RegisterKey{SessionID: first, Kind: harness.RegisterSession}
		if err := store.Transact(ctx, func(tx harness.Transaction) error {
			_, err := tx.ReplaceRegister(key, reg.Revision, bad)
			return err
		}); err != nil {
			t.Fatalf("corrupt the register: %v", err)
		}

		// A malformed stored identity: non-empty, not the durable 32-hex shape.
		if err := store.Transact(ctx, func(tx harness.Transaction) error {
			_, err := tx.InsertRegister(harness.RegisterDraft{
				Key:     harness.RegisterKey{SessionID: "corrupt-id", Kind: harness.RegisterSession},
				Payload: json.RawMessage(`{}`),
			})
			return err
		}); err != nil {
			t.Fatalf("plant the malformed row: %v", err)
		}

		counting := &countingValidationStorage{Storage: store, register: map[string]int{}, entries: map[string]int{}}
		lister := newPublicFixture(t, counting, newScriptModel(), nil)
		defer lister.close()

		listed, err := lister.h.ListSessions(ctx)
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		want := []string{second, third}
		sort.Strings(want) // storage returns sorted identities; creation order is random
		if len(listed) != 2 || listed[0].Identity.SessionID != want[0] || listed[1].Identity.SessionID != want[1] {
			t.Fatalf("ListSessions = %+v, want the valid siblings in the sorted listing order", listed)
		}
		if _, err := lister.h.SnapshotSession(ctx, first); !errors.Is(err, harness.ErrCorrupt) {
			t.Fatalf("corrupt direct read = %v, want the corruption error", err)
		}
		if _, err := lister.h.SnapshotSession(ctx, "corrupt-id"); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("malformed direct read = %v, want the invalid class", err)
		}

		// A cold corrupt Session revalidates on every list; the warm valid
		// Session's history is never reread.
		registersBefore, entriesBefore := counting.counts(second)
		if _, err := lister.h.ListSessions(ctx); err != nil {
			t.Fatalf("second ListSessions: %v", err)
		}
		registersAfter, entriesAfter := counting.counts(second)
		if registersAfter != registersBefore || entriesAfter != entriesBefore {
			t.Fatalf("the warm valid Session reread its graph (%d/%d -> %d/%d)",
				registersBefore, entriesBefore, registersAfter, entriesAfter)
		}
		corruptRegistersBefore, _ := counting.counts(first)
		if _, err := lister.h.ListSessions(ctx); err != nil {
			t.Fatalf("third ListSessions: %v", err)
		}
		corruptRegistersAfter, _ := counting.counts(first)
		if corruptRegistersAfter != corruptRegistersBefore+1 {
			t.Fatalf("the cold corrupt Session was revalidated %d times in one list, want exactly one attempt",
				corruptRegistersAfter-corruptRegistersBefore)
		}

		// Deletion removes the row from the listing while the direct read
		// reports the typed absence.
		if _, err := lister.h.ArchiveSession(ctx, second); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if err := lister.h.DeleteSession(ctx, second); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		if _, err := lister.h.ArchiveSession(ctx, third); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if err := lister.h.DeleteSession(ctx, third); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		listed, err = lister.h.ListSessions(ctx)
		if err != nil || len(listed) != 0 {
			t.Fatalf("ListSessions after deletion = %+v err %v", listed, err)
		}
		if _, err := lister.h.SnapshotSession(ctx, second); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("deleted direct read = %v, want the not-found class", err)
		}
	})

	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		f := newPublicFixture(t, store, newScriptModel(), nil)
		defer f.close()
		session := createSession(t, f.h)

		failing := &failingListStorage{Storage: store, entriesErrFor: map[string]error{}}
		failing.listErr = errors.New("enumeration failed")
		probe := newPublicFixture(t, failing, newScriptModel(), nil)
		defer probe.close()
		if _, err := probe.h.ListSessions(ctx); err == nil || !strings.Contains(err.Error(), "enumeration failed") {
			t.Fatalf("ListSessions with a failing enumeration = %v, want the propagated failure", err)
		}
		failing.listErr = nil
		failing.entriesErrFor[session] = errors.New("entry read failed")
		if _, err := probe.h.ListSessions(ctx); err == nil || !strings.Contains(err.Error(), "entry read failed") {
			t.Fatalf("ListSessions with a failing materialization = %v, want the propagated failure", err)
		}
		failing.entriesErrFor[session] = nil

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := probe.h.ListSessions(canceled); err == nil {
			t.Fatalf("ListSessions with a canceled context succeeded, want the propagated cancellation")
		}
	})
}

// TestPublicSnapshotSessionDeletionRace proves the deletion/read race: every
// concurrent snapshot either returns exactly the independent archived
// baseline bytes or the typed not-found class, never a mixed or corrupted
// read.
func TestPublicSnapshotSessionDeletionRace(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		script := newScriptModel(publicTurn())
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)
		if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		<-script.arrived
		awaitTerminal(t, f.h, session, "op-1")
		awaitQuiet(t, f.h, session)
		if _, err := f.h.ArchiveSession(ctx, session); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		baseline, err := f.h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("archived baseline snapshot: %v", err)
		}
		expected, err := json.Marshal(baseline)
		if err != nil {
			t.Fatalf("marshal the archived baseline: %v", err)
		}

		const readers = 4
		const reads = 50
		var wg sync.WaitGroup
		errs := make(chan error, readers*reads+1)
		stop := make(chan struct{})
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < reads; j++ {
					select {
					case <-stop:
						return
					default:
					}
					snap, err := f.h.SnapshotSession(ctx, session)
					if err != nil {
						if !errors.Is(err, harness.ErrNotFound) {
							errs <- err
							return
						}
						continue
					}
					body, merr := json.Marshal(snap)
					if merr != nil {
						errs <- merr
						return
					}
					if !bytes.Equal(expected, body) { // every successful read equals the complete archived state
						errs <- errors.New("a concurrent read observed a different body than the archived baseline")
						return
					}
				}
			}()
		}
		if err := f.h.DeleteSession(ctx, session); err != nil {
			errs <- err
		}
		close(stop)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent snapshot failed: %v", err)
		}
	})
}

// snapshotJobStopper is the background rows' Jobs seam: StopJob runs the
// delivery callback that stands for the ordinary kill-completion path.
type snapshotJobStopper struct {
	deliver func(sessionID, jobID string)
}

func (s snapshotJobStopper) StopJob(sessionID, jobID string) {
	if s.deliver != nil {
		s.deliver(sessionID, jobID)
	}
}

// parkingModelFn is one physical model attempt that signals its arrival,
// parks until its gate closes or its execution context is canceled, then
// answers accordingly: a released gate completes the turn, a stop's cancel
// settles the interruption without any output.
func parkingModelFn(gate, arrived chan struct{}) func(context.Context, model.Request) (model.Stream, error) {
	return func(ctx context.Context, _ model.Request) (model.Stream, error) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		select {
		case <-gate:
			return publicTurnStream(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// backgroundPrepared returns the background rows' prepared execution whose
// model parks on the given gate and signals each invocation.
func backgroundPrepared(gate, arrived chan struct{}) harness.PreparedExecution {
	return harness.PreparedExecution{
		Capture: publicCapture(),
		Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
			return harness.Execution{
				Model:         parkingModelFn(gate, arrived),
				CompactModel:  parkingModelFn(gate, arrived),
				NormalizeTool: publicNormalize,
				Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
					return harness.PreparedTool{Immediate: &harness.ToolOutcome{
						Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no tools"},
					}}
				},
			}, nil
		},
	}
}

// gatedBackgroundFixture builds the Jobs-seamed public fixture whose first
// admission's execution parks on the first gate and every later admission's
// on the later gate; every model invocation signals arrived.
func gatedBackgroundFixture(t *testing.T, store harness.Storage, stopper harness.JobStopper, script *scriptModel, first, later, arrived chan struct{}) *publicFixture {
	t.Helper()
	f := newPublicFixtureWithJobs(t, store, stopper, script, nil)
	calls := 0
	f.prepareHook = func(_ int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
		calls++
		gate := first
		if calls > 1 {
			gate = later
		}
		return backgroundPrepared(gate, arrived), nil
	}
	return f
}

// TestPublicSnapshotJobMemberRevisions proves the job-member rows: an
// admission publishes the membership view and advances the local revision
// once, a failed start's cleanup finishes the member and advances it once,
// and a delivered completion to the active Session enqueues its steering item
// and finishes the member.
func TestPublicSnapshotJobMemberRevisions(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		parentGate := make(chan struct{})
		laterGate := make(chan struct{})
		arrived := make(chan struct{}, 16)
		script := newScriptModel()
		f := gatedBackgroundFixture(t, store, snapshotJobStopper{}, script, parentGate, laterGate, arrived)
		defer f.close()
		h := f.h
		session := createSession(t, h)

		baseline, err := h.SnapshotSession(ctx, session)
		if err != nil || baseline.LocalRevision != 0 || len(baseline.Background) != 0 {
			t.Fatalf("baseline = %+v err %v", baseline, err)
		}

		// A spawn that fails still cleans its member up: one admission
		// publication and one finish publication.
		spawnEntered := make(chan struct{})
		spawnRelease := make(chan struct{})
		startErr := make(chan error, 1)
		go func() {
			startErr <- h.StartJob(ctx, session, "000000aa", func(_ context.Context, completionID string) error {
				close(spawnEntered)
				<-spawnRelease
				return errors.New("spawn down")
			})
		}()
		<-spawnEntered
		admitted, err := h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("snapshot during the spawn: %v", err)
		}
		if len(admitted.Background) != 1 || admitted.Background[0] != (harness.BackgroundMemberView{Kind: "job", ID: "000000aa"}) {
			t.Fatalf("background during the spawn = %+v", admitted.Background)
		}
		if admitted.LocalRevision != baseline.LocalRevision+1 {
			t.Fatalf("admission revision = %d, want %d", admitted.LocalRevision, baseline.LocalRevision+1)
		}
		close(spawnRelease)
		if err := <-startErr; err == nil || !strings.Contains(err.Error(), "spawn down") {
			t.Fatalf("StartJob error = %v, want the spawn failure", err)
		}
		cleaned, err := h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("snapshot after the failed start: %v", err)
		}
		if len(cleaned.Background) != 0 {
			t.Fatalf("background after the failed start = %+v", cleaned.Background)
		}
		if cleaned.LocalRevision != admitted.LocalRevision+1 {
			t.Fatalf("cleanup revision = %d, want %d", cleaned.LocalRevision, admitted.LocalRevision+1)
		}

		// An active Session steers a delivered completion under the
		// completion identity; the member finishes after the delivery.
		if _, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		<-arrived // the admitted Operation reached its parked boundary
		active, err := h.SnapshotSession(ctx, session)
		if err != nil || !active.ExecutionBusy {
			t.Fatalf("active snapshot = %+v err %v", active, err)
		}
		secondCompletion := ""
		if err := h.StartJob(ctx, session, "000000bb", func(_ context.Context, completionID string) error {
			secondCompletion = completionID
			return nil
		}); err != nil {
			t.Fatalf("StartJob: %v", err)
		}
		withMember, err := h.SnapshotSession(ctx, session)
		if err != nil || len(withMember.Background) != 1 || withMember.Background[0].ID != "000000bb" {
			t.Fatalf("background after the start = %+v err %v", withMember.Background, err)
		}
		beforeDelivery := withMember.LocalRevision
		if err := h.DeliverBackgroundCompletion(ctx, session, secondCompletion, "bg answer"); err != nil {
			t.Fatalf("DeliverBackgroundCompletion: %v", err)
		}
		delivered, err := h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("snapshot after the delivery: %v", err)
		}
		if len(delivered.Background) != 0 {
			t.Fatalf("background after the delivery = %+v", delivered.Background)
		}
		if len(delivered.Steering) != 1 || delivered.Steering[0].OperationID != secondCompletion ||
			delivered.Steering[0].Origin != harness.InputOriginRuntime || delivered.Steering[0].Content[0].Text != "bg answer" {
			t.Fatalf("steering after the delivery = %+v", delivered.Steering)
		}
		if delivered.LocalRevision != beforeDelivery+2 { // the enqueue and the finish
			t.Fatalf("delivery revision = %d, want %d", delivered.LocalRevision, beforeDelivery+2)
		}

		// Converge: the cancellation settles the parked Operation and
		// discards the buffered completion.
		f.cancel()
		go close(parentGate)
		if err := h.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		final, err := h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("final snapshot: %v", err)
		}
		if final.ExecutionBusy || len(final.Steering) != 0 || len(final.Background) != 0 {
			t.Fatalf("final snapshot = busy %v steering %d background %d", final.ExecutionBusy, len(final.Steering), len(final.Background))
		}
	})
}

// TestPublicSnapshotChildStopRevisions proves the child-closure rows: the
// permanent closure and its buffer discard are one publication, the canceled
// run's retirement and drain follow, a delivered completion steers into the
// active parent, and a repeated stop is a no-op publication.
func TestPublicSnapshotChildStopRevisions(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		parentGate := make(chan struct{})
		childGate := make(chan struct{})
		arrived := make(chan struct{}, 16)
		script := newScriptModel()
		f := gatedBackgroundFixture(t, store, snapshotJobStopper{}, script, parentGate, childGate, arrived)
		defer f.close()
		h := f.h
		root := createSession(t, h)

		if _, err := submit(t, h, root, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		<-arrived // the parent's Operation is parked at its boundary
		parentBefore, err := h.SnapshotSession(ctx, root)
		if err != nil || parentBefore.LocalRevision != 3 {
			t.Fatalf("parent before the launch = %+v err %v", parentBefore, err)
		}

		res, err := h.LaunchChildSession(ctx, harness.LaunchChildRequest{
			ParentSessionID: root,
			AgentType:       "coder",
			Content:         []model.ContentPart{{Kind: model.PartText, Text: "child work"}},
			OperationID:     "child-op-1",
			MaxConcurrent:   1,
			OutputLimit:     1024,
		})
		if err != nil {
			t.Fatalf("LaunchChildSession: %v", err)
		}
		<-arrived // the child's Operation is parked at its boundary
		parent, err := h.SnapshotSession(ctx, root)
		if err != nil {
			t.Fatalf("parent snapshot: %v", err)
		}
		if len(parent.Background) != 1 || parent.Background[0] != (harness.BackgroundMemberView{Kind: "child", ID: res.ChildSessionID}) {
			t.Fatalf("parent background = %+v", parent.Background)
		}
		if parent.LocalRevision != parentBefore.LocalRevision+1 {
			t.Fatalf("admission revision = %d, want %d", parent.LocalRevision, parentBefore.LocalRevision+1)
		}
		child, err := h.SnapshotSession(ctx, res.ChildSessionID)
		if err != nil {
			t.Fatalf("child snapshot: %v", err)
		}
		if !child.ExecutionBusy || child.LocalRevision != 1 || len(child.Operations) != 1 ||
			child.Operations[0].Admission.OperationID != "child-op-1" {
			t.Fatalf("child snapshot = busy %v rev %d ops %+v", child.ExecutionBusy, child.LocalRevision, child.Operations)
		}
		if len(child.Facts) != 1 || child.Facts[0].Kind != harness.EntryInput {
			t.Fatalf("child facts = %+v", child.Facts)
		}

		if _, err := submit(t, h, res.ChildSessionID, "op-2", harness.MessageModeRegular, "steer-me"); err != nil {
			t.Fatalf("child steering submit: %v", err)
		}
		buffered, err := h.SnapshotSession(ctx, res.ChildSessionID)
		if err != nil || len(buffered.Steering) != 1 || buffered.LocalRevision != child.LocalRevision+3 {
			t.Fatalf("child buffered snapshot = %+v err %v", buffered, err)
		}

		if err := h.Stop(ctx, res.ChildSessionID); err != nil {
			t.Fatalf("Stop(child): %v", err)
		}
		closed, err := h.SnapshotSession(ctx, res.ChildSessionID)
		if err != nil {
			t.Fatalf("child snapshot after the stop: %v", err)
		}
		if len(closed.Steering) != 0 || closed.ExecutionBusy {
			t.Fatalf("closed child = steering %d busy %v", len(closed.Steering), closed.ExecutionBusy)
		}
		// the closure publication plus the canceled run's retirement drain
		if closed.LocalRevision != buffered.LocalRevision+4 {
			t.Fatalf("closure revision = %d, want %d", closed.LocalRevision, buffered.LocalRevision+4)
		}
		if len(closed.Facts) != 3 || closed.Facts[0].Kind != harness.EntryInput ||
			closed.Facts[1].Kind != harness.EntrySignal ||
			closed.Facts[2].Kind != harness.EntryOperationSettlement ||
			closed.Facts[2].Settlement.Status != harness.OperationInterruption {
			t.Fatalf("closed child facts = %+v", closed.Facts)
		}
		if err := h.Stop(ctx, res.ChildSessionID); err != nil { // the repeat stop is a no-op
			t.Fatalf("repeat Stop(child): %v", err)
		}
		repeated, err := h.SnapshotSession(ctx, res.ChildSessionID)
		if err != nil || repeated.LocalRevision != closed.LocalRevision {
			t.Fatalf("repeat stop revision = %d err %v, want %d", repeated.LocalRevision, err, closed.LocalRevision)
		}

		// The child's completion steered into the active parent and finished
		// the parent's member.
		parent, err = h.SnapshotSession(ctx, root)
		if err != nil {
			t.Fatalf("parent snapshot after the child stop: %v", err)
		}
		if len(parent.Background) != 0 {
			t.Fatalf("parent background after the finish = %+v", parent.Background)
		}
		if len(parent.Steering) != 1 || parent.Steering[0].Origin != harness.InputOriginRuntime ||
			parent.Steering[0].Content[0].Text != "Task interrupted." {
			t.Fatalf("parent steering after the delivery = %+v", parent.Steering)
		}
		if parent.LocalRevision != parentBefore.LocalRevision+3 { // admit, enqueue, finish
			t.Fatalf("parent revision after the child stop = %d, want %d", parent.LocalRevision, parentBefore.LocalRevision+3)
		}

		f.cancel()
		go close(parentGate)
		if err := h.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

// TestPublicSnapshotRootStopRevisions proves the root-stop rows: the stopping
// publication, the killed job's delivered completion steering into the active
// Session, the member finish, and the reopen publication — with a repeated
// stop advancing nothing.
func TestPublicSnapshotRootStopRevisions(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		parentGate := make(chan struct{})
		laterGate := make(chan struct{})
		arrived := make(chan struct{}, 16)
		var (
			mu          sync.Mutex
			completions = map[string]string{}
		)
		var hh *harness.Harness
		stopper := snapshotJobStopper{deliver: func(sessionID, jobID string) {
			mu.Lock()
			completionID := completions[jobID]
			mu.Unlock()
			_ = hh.DeliverBackgroundCompletion(context.Background(), sessionID, completionID, "job done")
		}}
		script := newScriptModel()
		f := gatedBackgroundFixture(t, store, stopper, script, parentGate, laterGate, arrived)
		defer f.close()
		h := f.h
		hh = h
		session := createSession(t, h)

		if _, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		<-arrived
		before, err := h.SnapshotSession(ctx, session)
		if err != nil || before.LocalRevision != 3 {
			t.Fatalf("before the start = %+v err %v", before, err)
		}

		if err := h.StartJob(ctx, session, "deadbeef", func(_ context.Context, completionID string) error {
			mu.Lock()
			completions["deadbeef"] = completionID
			mu.Unlock()
			return nil
		}); err != nil {
			t.Fatalf("StartJob: %v", err)
		}
		started, err := h.SnapshotSession(ctx, session)
		if err != nil || len(started.Background) != 1 || started.Background[0] != (harness.BackgroundMemberView{Kind: "job", ID: "deadbeef"}) {
			t.Fatalf("after the start = %+v err %v", started, err)
		}
		if started.LocalRevision != before.LocalRevision+1 {
			t.Fatalf("admission revision = %d, want %d", started.LocalRevision, before.LocalRevision+1)
		}

		if err := h.Stop(ctx, session); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		stopped, err := h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("snapshot after the stop: %v", err)
		}
		if len(stopped.Background) != 0 {
			t.Fatalf("background after the stop = %+v", stopped.Background)
		}
		if len(stopped.Steering) != 1 || stopped.Steering[0].Origin != harness.InputOriginRuntime ||
			stopped.Steering[0].Content[0].Text != "job done" {
			t.Fatalf("steering after the stop = %+v", stopped.Steering)
		}
		// the stopping publication, the completion enqueue, the member finish,
		// and the reopen publication
		if stopped.LocalRevision != started.LocalRevision+4 {
			t.Fatalf("stop revision = %d, want %d", stopped.LocalRevision, started.LocalRevision+4)
		}
		if err := h.Stop(ctx, session); err != nil { // the repeat stop is a no-op
			t.Fatalf("repeat Stop: %v", err)
		}
		repeated, err := h.SnapshotSession(ctx, session)
		if err != nil || repeated.LocalRevision != stopped.LocalRevision {
			t.Fatalf("repeat stop revision = %d err %v, want %d", repeated.LocalRevision, err, stopped.LocalRevision)
		}

		f.cancel()
		go close(parentGate)
		if err := h.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

// matrixTurnStream assembles to one completed turn carrying message-level
// extra, one published call with call-scope extra, and reported usage.
func matrixTurnStream(callID string) *publicScriptStream {
	position := 0
	return &publicScriptStream{deltas: []model.StreamDelta{
		{
			HasChoice:        true,
			Role:             "assistant",
			ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "done"}},
			MessageExtra:     model.Extra{"m": json.RawMessage(`{"deep":[1]}`)},
			ToolFragments: []model.ToolCallFragment{{
				Position:         &position,
				ID:               callID,
				Name:             "echo",
				ArgumentFragment: `{"x":1}`,
				Extra:            model.Extra{"c": json.RawMessage(`"v"`)},
			}},
		},
		{Usage: &model.Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}},
	}}
}

// usageOnlyFailedStream assembles to one payload-less errored output whose
// reported usage rides the consuming settlement entry: no assistant entry is
// written and the settlement carries the model identity and usage pointers.
func usageOnlyFailedStream() *publicScriptStream {
	return &publicScriptStream{
		deltas: []model.StreamDelta{{Usage: &model.Usage{InputTokens: 5, CachedInputTokens: 1, OutputTokens: 7}}},
		err:    errors.New("no output"),
	}
}

// TestPublicListSessionsStaleEnumeratedRow proves the listing interprets its
// enumerated identity set against the current truth: an archived Session
// that disappears after the store already returned the identities is
// omitted while its valid sibling fully survives.
func TestPublicListSessionsStaleEnumeratedRow(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		wrapped := &failingListStorage{Storage: store, entriesErrFor: map[string]error{}}
		f := newPublicFixture(t, wrapped, newScriptModel(), nil)
		defer f.close()
		victim := createSession(t, f.h)
		sibling := createSession(t, f.h)
		if _, err := f.h.ArchiveSession(ctx, victim); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if _, err := f.h.ArchiveSession(ctx, sibling); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		survivor, err := f.h.ReadSessionHeader(ctx, sibling)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}

		// Arm the one-shot gate: the store has already returned both
		// identities, and the listing parks before consuming them.
		wrapped.listed = make(chan struct{})
		wrapped.listRelease = make(chan struct{})
		type listResult struct {
			rows []harness.SessionHeader
			err  error
		}
		listDone := make(chan listResult, 1)
		go func() {
			rows, err := f.h.ListSessions(ctx)
			listDone <- listResult{rows, err}
		}()
		<-wrapped.listed
		if err := f.h.DeleteSession(ctx, victim); err != nil {
			t.Fatalf("DeleteSession across the parked enumeration: %v", err)
		}
		close(wrapped.listRelease)
		result := <-listDone
		if result.err != nil {
			t.Fatalf("ListSessions: %v", result.err)
		}
		if len(result.rows) != 1 || result.rows[0].Identity.SessionID != sibling {
			t.Fatalf("stale listing = %+v, want only the surviving sibling", result.rows)
		}
		if !reflect.DeepEqual(result.rows[0], survivor) {
			t.Fatalf("the surviving row = %+v, want the complete archived record %+v", result.rows[0], survivor)
		}
		if _, err := f.h.SnapshotSession(ctx, victim); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("the disappeared row's direct read = %v, want the typed not-found class", err)
		}
	})
}

// TestPublicSnapshotSessionColdReadFailures proves a never-materialized
// Session's direct snapshot propagates the injected storage failure and the
// canceled caller context exactly, with no partial body. The cold fixture
// executes its storage reads: the entries failure surfaces from the entry
// read and the cancellation from the first register read.
func TestPublicSnapshotSessionColdReadFailures(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		wrapped := &failingListStorage{Storage: store, entriesErrFor: map[string]error{}}
		f := newPublicFixture(t, wrapped, newScriptModel(), nil)
		defer f.close()
		cold := seedArchivedSession(t, wrapped) // seeded straight through storage: never materialized

		entriesFailure := errors.New("entry read failed")
		wrapped.entriesErrFor[cold] = entriesFailure
		snap, err := f.h.SnapshotSession(ctx, cold)
		if !errors.Is(err, entriesFailure) {
			t.Fatalf("cold snapshot over a failing entry read = %v, want the exact injected storage failure", err)
		}
		if !reflect.DeepEqual(snap, harness.SessionSnapshot{}) {
			t.Fatalf("failing cold snapshot = %+v, want no partial body", snap)
		}
		delete(wrapped.entriesErrFor, cold)

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		snap, err = f.h.SnapshotSession(canceled, cold)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cold snapshot over a canceled context = %v, want the exact context error", err)
		}
		if !reflect.DeepEqual(snap, harness.SessionSnapshot{}) {
			t.Fatalf("canceled cold snapshot = %+v, want no partial body", snap)
		}
	})
}

// awaitQuiet waits until one Session's execution slot has retired after its
// terminal settlement, bounding the wait: the coordinator-local publications
// of the post-terminal drain must land before a stable baseline is taken.
func awaitQuiet(t *testing.T, h *harness.Harness, session string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap, err := h.SnapshotSession(context.Background(), session)
		if err != nil {
			t.Fatalf("SnapshotSession: %v", err)
		}
		if !snap.ExecutionBusy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s never went quiet within the wait bound", session)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPublicSnapshotOwnershipMatrix proves deep ownership of every returned
// reference over both stores against immutable serialized expectations: the
// baseline bytes are taken before any mutation, a second returned snapshot is
// modified across its full nested-field matrix, and a fresh read must still
// serialize to the untouched expectation. The fixtures hold every applicable
// reference non-nil; a missing reference fails the fixture instead of
// skipping the mutation.
func TestPublicSnapshotOwnershipMatrix(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		script := newScriptModel(
			publicAttempt{stream: matrixTurnStream("call-1")}, // op-1 turn 1: message extra, call extra, usage
			publicAttempt{stream: usageOnlyFailedStream()},    // op-1 turn 2: payload-less failure with usage on the settlement
			publicTurn("call-2"),                              // op-2 turn 1: publishes the parking call
			publicTurn(),                                      // op-2 turn 2 after the steered input
			publicTurn(),                                      // the queued drain admission's turn
			summaryAttempt("Summary."),                        // the manual compaction's summarizer turn
			publicTurn(),                                      // the interrupted Operation's turn
		)
		parkEntered := make(chan struct{})
		parkRelease := make(chan struct{})
		f := newPublicFixtureWithJobs(t, store, snapshotJobStopper{}, script, nil)
		f.prepareHook = func(call int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			capture := publicCapture()
			capture.Capabilities = []string{"cap.a", "cap.b"}
			immediateTool := func(_ context.Context, call model.ToolCall) harness.PreparedTool {
				return harness.PreparedTool{Permissions: publicPermission, Immediate: &harness.ToolOutcome{
					Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call"},
					Metadata: json.RawMessage(`{"n":[1,1.0]}`),
				}}
			}
			parkingTool := func(_ context.Context, call model.ToolCall) harness.PreparedTool {
				return harness.PreparedTool{Permissions: publicPermission, Execute: func(execCtx context.Context) harness.ToolOutcome {
					close(parkEntered)
					select {
					case <-parkRelease:
						return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran parked"}}
					case <-execCtx.Done():
						return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultInterrupted, Content: "canceled"}}
					}
				}}
			}
			tool := immediateTool
			if call == 1 { // the second admission runs the parking tool plan
				tool = parkingTool
			}
			return harness.PreparedExecution{
				Capture: capture,
				Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
					return harness.Execution{
						Model:         script.effect,
						CompactModel:  script.effect,
						NormalizeTool: publicNormalize,
						Tool:          tool,
					}, nil
				},
			}, nil
		}
		defer f.close()
		h := f.h
		session := createSession(t, h)

		extraPart := func(text string) []model.ContentPart {
			return []model.ContentPart{{
				Kind:  model.PartText,
				Text:  text,
				Extra: model.Extra{"p": json.RawMessage(`"v"`), "q": json.RawMessage(`{"n":[2]}`)},
			}}
		}
		submitExtra := func(operationID, text string, mode harness.MessageMode) {
			t.Helper()
			if _, err := h.Submit(ctx, harness.SubmitRequest{
				SessionID: session, OperationID: operationID, Origin: harness.InputOriginUser,
				Content: extraPart(text), Mode: mode,
			}); err != nil {
				t.Fatalf("submit %s: %v", operationID, err)
			}
		}

		// op-1 settles as a payload-less failure whose settlement carries the
		// model identity and usage pointers.
		submitExtra("op-1", "hello", harness.MessageModeRegular)
		<-script.arrived // the published call's turn
		<-script.arrived // the failing boundary turn
		awaitTerminal(t, h, session, "op-1")

		// op-2 publishes a call whose tool execution parks: the running
		// Operation holds a non-nil active effect and a pending call while
		// both buffers carry extra-bearing items.
		submitExtra("op-2", "second", harness.MessageModeRegular)
		<-script.arrived
		<-parkEntered
		submitExtra("op-3", "steer-me", harness.MessageModeRegular)
		submitExtra("op-4", "queue-me", harness.MessageModeQueued)

		parked, err := h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("parked snapshot: %v", err)
		}
		parkedExpected, err := json.Marshal(parked)
		if err != nil {
			t.Fatalf("marshal the parked baseline: %v", err)
		}
		if !parked.ExecutionBusy || len(parked.Operations) != 2 {
			t.Fatalf("parked snapshot = busy %v ops %d", parked.ExecutionBusy, len(parked.Operations))
		}
		parkedOp := parked.Operations[1].State // the sorted copy places op-2 after op-1
		if parkedOp.ActiveEffect == nil || parkedOp.ActiveEffect.Kind != harness.EffectTool ||
			len(parkedOp.PendingToolCalls) != 1 {
			t.Fatalf("parked operation state = %+v, want the active tool effect and its pending call", parkedOp)
		}
		if len(parked.Steering) != 1 || len(parked.Queued) != 1 {
			t.Fatalf("parked buffers = %+v / %+v", parked.Steering, parked.Queued)
		}

		mutatedParked, err := h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("parked mutation snapshot: %v", err)
		}
		mutatedParked.Operations[1].State.ActiveEffect.Kind = "mutated"
		mutatedParked.Operations[1].State.ActiveEffect.ResultEntryID = "mutated"
		mutatedParked.Operations[1].State.ActiveEffect.ToolCallID = "mutated"
		mutatedParked.Operations[1].State.PendingToolCalls[0].CallID = "mutated"
		mutatedParked.Operations[1].State.PendingToolCalls[0].ResultEntryID = "mutated"
		mutatedParked.Operations[1].State.PendingToolCalls = append(mutatedParked.Operations[1].State.PendingToolCalls, harness.PendingToolCall{CallID: "forged"})
		mutatedParked.Steering[0].Content[0].Text = "mutated"
		mutatedParked.Steering[0].Content[0].Extra["q"][6] = '3'
		mutatedParked.Steering[0].Content[0].Extra["p"] = json.RawMessage(`"mutated"`)
		mutatedParked.Steering[0].Content = append(mutatedParked.Steering[0].Content, model.ContentPart{Kind: model.PartText, Text: "forged"})
		mutatedParked.Steering = append(mutatedParked.Steering, harness.PendingInput{OperationID: "forged"})
		mutatedParked.Queued[0].Content[0].Text = "mutated"
		mutatedParked.Queued[0].Content[0].Extra["q"][6] = '3'
		mutatedParked.Queued[0].Content[0].Extra["p"] = json.RawMessage(`"mutated"`)
		mutatedParked.Queued = append(mutatedParked.Queued, harness.PendingInput{OperationID: "forged"})

		freshParked, err := h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("parked fresh read: %v", err)
		}
		freshParkedBytes, err := json.Marshal(freshParked)
		if err != nil {
			t.Fatalf("marshal the parked fresh read: %v", err)
		}
		if !bytes.Equal(parkedExpected, freshParkedBytes) {
			t.Fatalf("a mutation of the parked snapshot reached the coordinator or another snapshot")
		}

		// Release the parked tool: op-2's completed turn continues for the
		// waiting steering item, settles, and the queued item drains.
		close(parkRelease)
		<-script.arrived // op-2's continuation turn
		<-script.arrived // the queued operation's turn
		awaitTerminal(t, h, session, "op-2")
		awaitTerminal(t, h, session, "op-4")

		// The manual compaction commits its usage-bearing entry; the
		// bounded retry spans the retiring run's release window, like the
		// landed manual-compact rows.
		compactWhenIdle(t, h, session, "comp-1")
		<-script.arrived // the summarizer turn
		awaitTerminal(t, h, session, "comp-1")

		// A parked completed turn interrupts into the typed interruption
		// signal, whose related-operation reference the matrix mutates.
		script.gate = make(chan struct{})
		submitExtra("op-5", "fifth", harness.MessageModeRegular)
		<-script.arrived
		if err := h.Interrupt(ctx, session); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		script.releaseGate()
		awaitTerminal(t, h, session, "op-5")
		awaitQuiet(t, h, session) // the retiring run's drain publications land before the archived baseline

		// A live job member whose completion is delivered under harness loss
		// commits the operationless background_completion signal.
		completionID := ""
		if err := h.StartJob(ctx, session, "deadbeef", func(_ context.Context, id string) error {
			completionID = id
			return nil
		}); err != nil {
			t.Fatalf("StartJob: %v", err)
		}
		f.cancel()
		if err := h.DeliverBackgroundCompletion(ctx, session, completionID, "lost work"); err != nil {
			t.Fatalf("DeliverBackgroundCompletion: %v", err)
		}

		// Archive stamps the Session's archived time.
		if _, err := h.ArchiveSession(ctx, session); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}

		baseline, err := h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("final snapshot: %v", err)
		}
		expected, err := json.Marshal(baseline)
		if err != nil {
			t.Fatalf("marshal the baseline: %v", err)
		}

		// Every applicable reference must be present in the fixture.
		if baseline.Session.State.ArchivedAt == nil || len(baseline.Session.State.Usage.ByModel) == 0 {
			t.Fatalf("session fixture lost its archived time or usage: %+v", baseline.Session.State)
		}
		kinds := map[harness.EntryKind]int{}
		var relatedOperation, relatedMember, settlementWithUsage, assistantReferences, resultMetadata bool
		for _, fact := range baseline.Facts {
			kinds[fact.Kind]++
			switch fact.Kind {
			case harness.EntryInput:
				if fact.Input == nil || len(fact.Input.Content) == 0 || len(fact.Input.Content[0].Extra) == 0 {
					t.Fatalf("input fact fixture lost its extra-bearing content: %+v", fact)
				}
			case harness.EntryAssistant:
				if fact.Assistant == nil {
					t.Fatalf("assistant fact fixture lost its payload: %+v", fact)
				}
				assistantReferences = assistantReferences || (len(fact.Assistant.Extra) > 0 && len(fact.Assistant.ToolCalls) > 0 &&
					len(fact.Assistant.ToolCalls[0].Extra) > 0 && len(fact.Assistant.ToolCalls[0].NormalizedArguments) > 0 &&
					fact.Assistant.Usage != nil)
			case harness.EntryToolResult:
				if fact.ToolResult == nil {
					t.Fatalf("tool result fact fixture lost its payload: %+v", fact)
				}
				resultMetadata = resultMetadata || len(fact.ToolResult.Metadata) > 0
			case harness.EntrySignal:
				if fact.Signal == nil {
					t.Fatalf("signal fact fixture lost its payload: %+v", fact)
				}
				relatedOperation = relatedOperation || fact.Signal.RelatedOperation != nil
				relatedMember = relatedMember || fact.Signal.RelatedMember != nil
			case harness.EntryCompaction:
				if fact.Compaction == nil || fact.Compaction.Usage == nil {
					t.Fatalf("compaction fact fixture lost its usage: %+v", fact.Compaction)
				}
			case harness.EntryOperationSettlement:
				if fact.Settlement == nil {
					t.Fatalf("settlement fact fixture lost its payload: %+v", fact)
				}
				settlementWithUsage = settlementWithUsage || (fact.Settlement.Model != nil && fact.Settlement.Usage != nil)
			}
		}
		if !assistantReferences {
			t.Fatalf("fixture never produced an assistant fact with its full nested reference set")
		}
		if !resultMetadata {
			t.Fatalf("fixture never produced a tool result carrying tool-owned metadata")
		}
		for _, kind := range []harness.EntryKind{
			harness.EntryInput, harness.EntryAssistant, harness.EntryToolResult,
			harness.EntrySignal, harness.EntryCompaction, harness.EntryOperationSettlement,
		} {
			if kinds[kind] == 0 {
				t.Fatalf("fixture never produced a %s fact", kind)
			}
		}
		if !relatedOperation || !relatedMember {
			t.Fatalf("fixture never produced both signal reference shapes (related operation %v, related member %v)", relatedOperation, relatedMember)
		}
		if !settlementWithUsage {
			t.Fatalf("fixture never produced a settlement carrying its model and usage pointers")
		}
		for _, op := range baseline.Operations {
			if op.State.SettledAt == nil || op.State.Terminal == nil {
				t.Fatalf("operation fixture lost its terminal pointers: %+v", op.State)
			}
			if len(op.Admission.Execution.Tools) == 0 || len(op.Admission.Execution.Tools[0].Parameters) == 0 ||
				len(op.Admission.Execution.Capabilities) == 0 {
				t.Fatalf("operation fixture lost its execution references: %+v", op.Admission.Execution)
			}
		}

		// The full mutation matrix on a second returned snapshot.
		mutated, err := h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("mutation snapshot: %v", err)
		}
		mutated.Session.Identity.Workspace = "mutated"
		*mutated.Session.State.ArchivedAt = baseline.Session.State.ArchivedAt.Add(time.Hour)
		mutated.Session.State.Usage.ByModel[0].Model.Provider = "mutated"
		mutated.Session.State.Usage.ByModel[0].Usage.InputTokens = 999
		mutated.Session.State.Usage.ByModel = append(mutated.Session.State.Usage.ByModel, harness.ModelUsage{})
		mutated.Session.State.CurrentAgentType = "mutated"
		mutated.Session.State.CompactionEntryID = "mutated"
		for i := range mutated.Operations {
			op := &mutated.Operations[i]
			op.Admission.Execution.Model.Provider = "mutated"
			op.Admission.Execution.Tools[0].Parameters[9] = 'p'
			op.Admission.Execution.Tools = append(op.Admission.Execution.Tools, model.ToolDefinition{})
			op.Admission.Execution.Capabilities[0] = "mutated"
			op.Admission.Execution.Capabilities = append(op.Admission.Execution.Capabilities, "forged")
			op.Admission.Execution.Compact.SystemPrompt = "mutated"
			*op.State.SettledAt = op.State.SettledAt.Add(2 * time.Hour)
			op.State.Terminal.Detail = "mutated"
			op.State.Terminal.SettlementEntry.EntryID = "mutated"
			op.State.Usage.ByModel[0].Usage.OutputTokens = 999
			op.State.Usage.ByModel = append(op.State.Usage.ByModel, harness.ModelUsage{})
			op.State.PendingToolCalls = append(op.State.PendingToolCalls, harness.PendingToolCall{CallID: "forged"})
		}
		mutated.Operations = append(mutated.Operations, harness.OperationRecord{})
		for i := range mutated.Facts {
			fact := &mutated.Facts[i]
			switch {
			case fact.Input != nil:
				fact.Input.Content[0].Text = "mutated"
				fact.Input.Content[0].Extra["p"] = json.RawMessage(`"mutated"`)
				fact.Input.Content[0].Extra["q"][6] = '3'
				fact.Input.Content = append(fact.Input.Content, model.ContentPart{Kind: model.PartText, Text: "forged"})
			case fact.Assistant != nil:
				if len(fact.Assistant.Content) > 0 {
					fact.Assistant.Content[0].Text = "mutated"
					fact.Assistant.Content = append(fact.Assistant.Content, model.ContentPart{Kind: model.PartText, Text: "forged"})
				}
				if len(fact.Assistant.Extra) > 0 {
					fact.Assistant.Extra["m"][10] = '2'
					fact.Assistant.Extra["m"] = json.RawMessage(`"mutated"`)
				}
				if len(fact.Assistant.ToolCalls) > 0 {
					fact.Assistant.ToolCalls[0].ID = "mutated"
					fact.Assistant.ToolCalls[0].ArgumentsBase64 = "mutated"
					if len(fact.Assistant.ToolCalls[0].Extra) > 0 {
						fact.Assistant.ToolCalls[0].Extra["c"][1] = 'x'
						fact.Assistant.ToolCalls[0].Extra["c"] = json.RawMessage(`"mutated"`)
					}
					if len(fact.Assistant.ToolCalls[0].NormalizedArguments) > 0 {
						fact.Assistant.ToolCalls[0].NormalizedArguments[5] = '2'
					}
					fact.Assistant.ToolCalls = append(fact.Assistant.ToolCalls, harness.ToolCallRecord{})
				}
				if fact.Assistant.Usage != nil {
					*fact.Assistant.Usage = harness.UsageCount{InputTokens: 999}
				}
			case fact.ToolResult != nil:
				if len(fact.ToolResult.Metadata) > 0 {
					fact.ToolResult.Metadata[7] = '2'
				}
				fact.ToolResult.AssistantEntry.EntryID = "mutated"
				fact.ToolResult.Content = "mutated"
			case fact.Signal != nil:
				if fact.Signal.RelatedOperation != nil {
					fact.Signal.RelatedOperation.SessionID = "mutated"
					fact.Signal.RelatedOperation.OperationID = "mutated"
				}
				if fact.Signal.RelatedMember != nil {
					fact.Signal.RelatedMember.Kind = "mutated"
					fact.Signal.RelatedMember.ID = "mutated"
				}
				fact.Signal.Content = "mutated"
			case fact.Compaction != nil:
				fact.Compaction.Summary = "mutated"
				fact.Compaction.BoundaryEntryID = "mutated"
				fact.Compaction.Model.Provider = "mutated"
				fact.Compaction.ConfigurationRevision = "mutated"
				*fact.Compaction.Usage = harness.UsageCount{InputTokens: 999}
			case fact.Settlement != nil:
				if fact.Settlement.Model != nil {
					fact.Settlement.Model.Provider = "mutated"
				}
				if fact.Settlement.Usage != nil {
					*fact.Settlement.Usage = harness.UsageCount{InputTokens: 999}
				}
				fact.Settlement.Detail = "mutated"
			}
		}
		mutated.Facts = append(mutated.Facts, harness.HistoryFact{EntryID: "forged"})

		again, err := h.SnapshotSession(ctx, session)
		if err != nil {
			t.Fatalf("fresh read: %v", err)
		}
		freshBytes, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("marshal the fresh read: %v", err)
		}
		if !bytes.Equal(expected, freshBytes) {
			t.Fatalf("a mutation of one snapshot reached the coordinator or another snapshot")
		}

		// The mutated snapshot must actually differ from the expectation, so
		// the comparison above cannot pass on untouched data.
		mutatedBytes, err := json.Marshal(mutated)
		if err != nil {
			t.Fatalf("marshal the mutated snapshot: %v", err)
		}
		if bytes.Equal(expected, mutatedBytes) {
			t.Fatalf("the mutation matrix changed nothing; the ownership comparison is vacuous")
		}
		if err := h.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

// awaitStoreQuiet waits until one Session holds no current Operation, no
// installed run, and no reservation: the post-terminal retirement is a
// coordinator-local publication, so a pair-sensitive baseline must be taken
// only after it lands.
func awaitStoreQuiet(t *testing.T, h *harness.Harness, session string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap, err := h.SnapshotSession(context.Background(), session)
		if err != nil {
			t.Fatalf("SnapshotSession: %v", err)
		}
		if !snap.ExecutionBusy && snap.Session.State.CurrentOperationID == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s never reached the quiet pair", session)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPublicListSessionsRowOwnership proves the listing rows are owned
// copies against an immutable serialized expectation: mutating a returned
// row's archived time, identity, lifecycle metadata, and slice length must
// never reach the coordinator or another listing. Usage totals are not a
// listing-row member; their ownership is covered by the snapshot matrix.
func TestPublicListSessionsRowOwnership(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		script := newScriptModel(publicTurn())
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)
		if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		<-script.arrived
		awaitTerminal(t, f.h, session, "op-1")
		if _, err := f.h.ArchiveSession(ctx, session); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		awaitStoreQuiet(t, f.h, session)

		rows, err := f.h.ListSessions(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("ListSessions = %+v err %v", rows, err)
		}
		if rows[0].ArchivedAt == nil {
			t.Fatalf("row fixture lost its archived time: %+v", rows[0])
		}
		expected, err := json.Marshal(rows)
		if err != nil {
			t.Fatalf("marshal the baseline rows: %v", err)
		}

		mutated, err := f.h.ListSessions(ctx)
		if err != nil || len(mutated) != 1 {
			t.Fatalf("second ListSessions = %+v err %v", mutated, err)
		}
		*mutated[0].ArchivedAt = mutated[0].ArchivedAt.Add(time.Hour)
		mutated[0].Identity.Workspace = "mutated"
		mutated[0].Lifecycle = "mutated"
		mutated[0].CurrentAgentType = "mutated"
		mutated = append(mutated, harness.SessionHeader{})

		fresh, err := f.h.ListSessions(ctx)
		if err != nil || len(fresh) != 1 {
			t.Fatalf("fresh ListSessions = %+v err %v", fresh, err)
		}
		freshBytes, err := json.Marshal(fresh)
		if err != nil {
			t.Fatalf("marshal the fresh rows: %v", err)
		}
		if !bytes.Equal(expected, freshBytes) {
			t.Fatalf("a mutation of one listing reached the coordinator or another listing")
		}
		mutatedBytes, err := json.Marshal(mutated)
		if err != nil {
			t.Fatalf("marshal the mutated rows: %v", err)
		}
		if bytes.Equal(expected, mutatedBytes) {
			t.Fatalf("the row mutation changed nothing; the ownership comparison is vacuous")
		}
	})
}

// deleteGateStorage parks one Session's deleting transaction inside its own
// lifetime: the Harness holds the coordinator mutex across the whole
// transaction, so a reader started after the park cannot observe any state
// until the gate releases and the commit (or rollback) has landed.
type deleteGateStorage struct {
	harness.Storage
	session string
	fail    bool
	started chan struct{}
	release chan struct{}
}

func (s *deleteGateStorage) Transact(ctx context.Context, fn func(harness.Transaction) error) error {
	return s.Storage.Transact(ctx, func(tx harness.Transaction) error {
		return fn(gatedDeleteTransaction{Transaction: tx, gate: s})
	})
}

type gatedDeleteTransaction struct {
	harness.Transaction
	gate *deleteGateStorage
}

func (t gatedDeleteTransaction) DeleteSession(sessionID string) error {
	if sessionID != t.gate.session {
		return t.Transaction.DeleteSession(sessionID)
	}
	t.gate.started <- struct{}{} // the rendezvous: the caller proceeds only once the transaction is parked
	<-t.gate.release
	if t.gate.fail {
		return errors.New("delete aborted by the fixture")
	}
	return t.Transaction.DeleteSession(sessionID)
}

// TestPublicSnapshotDeleteCoherence proves the deletion publication over both
// stores: a held deleting transaction blocks every snapshot until the
// commit-and-invalidation lands (typed not-found, never a partial body), a
// rolled-back deletion leaves the complete old state readable and listed, and
// every successful concurrent race read equals the independent archived
// baseline bytes.
func TestPublicSnapshotDeleteCoherence(t *testing.T) {
	seedArchived := func(t *testing.T, wrapped harness.Storage, script *scriptModel) (*harness.Harness, string, []byte, context.CancelFunc) {
		t.Helper()
		f := newPublicFixture(t, wrapped, script, nil)
		session := createSession(t, f.h)
		if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		<-script.arrived
		awaitTerminal(t, f.h, session, "op-1")
		awaitQuiet(t, f.h, session)
		if _, err := f.h.ArchiveSession(context.Background(), session); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		baseline, err := f.h.SnapshotSession(context.Background(), session)
		if err != nil {
			t.Fatalf("archived baseline snapshot: %v", err)
		}
		expected, err := json.Marshal(baseline)
		if err != nil {
			t.Fatalf("marshal the archived baseline: %v", err)
		}
		return f.h, session, expected, f.close
	}

	t.Run("a held delete blocks reads until the commit and invalidation", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(publicTurn())
			gate := &deleteGateStorage{Storage: store, started: make(chan struct{}), release: make(chan struct{})}
			h, session, _, closeFixture := seedArchived(t, gate, script)
			defer closeFixture()
			gate.session = session

			deleteDone := make(chan error, 1)
			go func() {
				deleteDone <- h.DeleteSession(context.Background(), session)
			}()
			<-gate.started // the deleting transaction is parked, holding the coordinator mutex

			// The reader starts only after the park: the mutex is held from
			// here until the commit and its invalidation have landed, so no
			// partial body can ever be observed.
			reads := make(chan error, 1)
			go func() {
				_, err := h.SnapshotSession(context.Background(), session)
				reads <- err
			}()
			close(gate.release)
			if err := <-deleteDone; err != nil {
				t.Fatalf("DeleteSession: %v", err)
			}
			if err := <-reads; !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("snapshot across the held delete = %v, want the typed not-found class", err)
			}
			if _, err := h.SnapshotSession(context.Background(), session); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("post-delete snapshot = %v, want the typed not-found class", err)
			}
			listed, err := h.ListSessions(context.Background())
			if err != nil || len(listed) != 0 {
				t.Fatalf("post-delete listing = %+v err %v", listed, err)
			}
		})
	})

	t.Run("a rolled-back delete leaves the complete old state", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(publicTurn())
			gate := &deleteGateStorage{Storage: store, fail: true, started: make(chan struct{}), release: make(chan struct{})}
			h, session, expected, closeFixture := seedArchived(t, gate, script)
			defer closeFixture()
			gate.session = session

			deleteDone := make(chan error, 1)
			go func() {
				deleteDone <- h.DeleteSession(context.Background(), session)
			}()
			<-gate.started
			close(gate.release)
			if err := <-deleteDone; err == nil || !strings.Contains(err.Error(), "delete aborted") {
				t.Fatalf("DeleteSession = %v, want the injected rollback failure", err)
			}

			after, err := h.SnapshotSession(context.Background(), session)
			if err != nil {
				t.Fatalf("snapshot after the rollback: %v", err)
			}
			fresh, err := json.Marshal(after)
			if err != nil {
				t.Fatalf("marshal the post-rollback read: %v", err)
			}
			if !bytes.Equal(expected, fresh) {
				t.Fatalf("the rolled-back deletion changed the readable state")
			}
			listed, err := h.ListSessions(context.Background())
			if err != nil || len(listed) != 1 || listed[0].Identity.SessionID != session {
				t.Fatalf("post-rollback listing = %+v err %v", listed, err)
			}
		})
	})
}
