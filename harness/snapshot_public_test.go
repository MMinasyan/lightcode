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
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// historyIDsOf collects value-metadata header identities in sequence order:
// the request the narrow fact accessor consumes.
func historyIDsOf(headers []harness.HistoryEntryHeader) []string {
	ids := make([]string, 0, len(headers))
	for _, header := range headers {
		ids = append(ids, header.EntryID)
	}
	return ids
}

// readHistoryFacts reads one Session's complete committed fact set through the
// narrow history view: the value-metadata headers select the entry identities
// the facts accessor consumes.
func readHistoryFacts(t *testing.T, h *harness.Harness, sessionID string) []harness.HistoryFact {
	t.Helper()
	view, err := h.ReadHistoryView(context.Background(), sessionID)
	testassert.Require(t, err == nil, "ReadHistoryView(%s): %v", sessionID, err)
	return view.HistoryFacts(historyIDsOf(view.HistoryHeaders()))
}

// snapshotTurnPrepared returns the snapshot rows' prepared execution: the
// scripted model over an immediate-success tool plan whose result carries
// tool-owned metadata, so the committed history covers the assistant,
// tool-call, and tool-result payload shapes.
func snapshotTurnPrepared(script *scriptModel) harness.PreparedExecution {
	return harness.PreparedExecution{
		Capture: publicCapture(),
		Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
			return publicExecutionOver(script.effect, script.effect, func(_ context.Context, call model.ToolCall) harness.PreparedTool {
				return harness.PreparedTool{Permissions: publicPermission, Immediate: &harness.ToolOutcome{
					Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call"},
					Metadata: json.RawMessage(`{"n":[1,1.0],"s":"<>&"}`),
				}}
			}), nil
		},
	}
}

// TestPublicReadSessionHistoryOwnedCapture proves the narrow history
// producer over both stores: exact envelope identity/kind/order of every
// committed entry with hook evidence excluded, the revision pair of the same
// capture, and nested returned-value ownership against a serialized
// expectation captured before mutation.
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
		testassert.NoError(t, err)
		defer func() {
			cancel()
			_ = h.Wait(context.Background())
		}()

		view, err := h.ReadHistoryView(ctx, fixture.SessionID)
		testassert.NoError(t, err)
		facts := view.HistoryFacts(historyIDsOf(view.HistoryHeaders()))
		testassert.Require(t, fixture.HookEntryID != "", "%v", "the owning fixture lost its hook evidence entry")
		testassert.Equal(t, len(facts), len(fixture.Identities))
		for i, want := range fixture.Identities {
			got := facts[i]
			testassert.Require(t, got.EntryID == want.EntryID && got.OperationID == want.OperationID && got.Kind == want.Kind && got.EntryID != fixture.HookEntryID, "fact %d = (%s/%s/%s), want the fixture envelope (%s/%s/%s); fact %d leaked the hook evidence entry", i, got.EntryID, got.OperationID, got.Kind, want.EntryID, want.OperationID, want.Kind, i)
		}
		testassert.Require(t, view.Revision.LocalRevision == 0 && view.Revision.DurableRevision > 0, "history revision pair = %+v, want the durable register revision with a fresh coordinator's zero local counter", view.Revision)

		// The serialized expectation is captured BEFORE any mutation: the
		// returned values are owned, so a fresh narrow read must still
		// serialize to it.
		expected, err := json.Marshal(facts)
		testassert.NoError(t, err)
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
		again, err := h.ReadHistoryView(ctx, fixture.SessionID)
		testassert.NoError(t, err)
		got, err := json.Marshal(again.HistoryFacts(historyIDsOf(again.HistoryHeaders())))
		testassert.NoError(t, err)
		testassert.Require(t, bytes.Equal(got, expected), "%v", "a mutation of the returned history reached the owner or another read")
	})
}

// TestPublicSnapshotSessionReadsAndOwns proves the composed owned read over
// both stores: sorted warm Operations, the typed fact union in ascending
// sequence, the metadata header, and the quiet busy fact.
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
		{
			_, err := submit(t, f.h, session, "op-z", harness.MessageModeRegular, "hello op-z")
			testassert.NoError(t, err)
		}
		<-script.arrived // the published call's turn
		<-script.arrived // the completing turn
		awaitTerminal(t, f.h, session, "op-z")
		{
			_, err := submit(t, f.h, session, "op-a", harness.MessageModeRegular, "hello op-a")
			testassert.NoError(t, err)
		}
		<-script.arrived // op-a's completing turn
		awaitTerminal(t, f.h, session, "op-a")
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}

		pristine, err := f.h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		testassert.Require(t, len(pristine.Operations) == 2 && pristine.Operations[0].OperationID == "op-a" && pristine.Operations[1].OperationID == "op-z" && (!pristine.History.ExecutionBusy && pristine.History.Revision.LocalRevision != 0) && (pristine.Header.Identity.SessionID == session && pristine.Header.Lifecycle == harness.LifecycleOpen), "operations = %+v, want lexicographic order over the warm append order; quiet view = busy %v local %d; session header = %+v", pristine.Operations, pristine.History.ExecutionBusy, pristine.History.Revision.LocalRevision, pristine.Header)

		wantKinds := []harness.EntryKind{
			harness.EntryInput, harness.EntryAssistant, harness.EntryToolResult, harness.EntryAssistant,
			harness.EntryOperationSettlement, harness.EntryInput, harness.EntryAssistant, harness.EntryOperationSettlement,
		}
		pristineFacts := pristine.History.HistoryFacts(historyIDsOf(pristine.History.HistoryHeaders()))
		testassert.Equal(t, len(pristineFacts), len(wantKinds))
		for i, fact := range pristineFacts {
			testassert.Require(t, fact.Kind == wantKinds[i] && (fact.OperationID != "" && fact.EntryID != "" && fact.Sequence != 0 && !fact.CommittedAt.IsZero()), "fact[%d].Kind = %q, want %q; fact[%d] loses its envelope identity: %+v", i, fact.Kind, wantKinds[i], i, fact)
		}
		assistant := pristineFacts[1].Assistant
		testassert.Require(t, assistant != nil && len(assistant.ToolCalls) == 1 && assistant.ToolCalls[0].ID == "call-1" && assistant.ToolCalls[0].ResultEntryID != "", "assistant fact = %+v", assistant)
		result := pristineFacts[2].ToolResult
		// the durable codec's JSON encoding escapes the raw metadata's markup
		testassert.Require(t, result != nil && result.ToolCallID == "call-1" && string(result.Metadata) == `{"n":[1,1.0],"s":"\u003c\u003e\u0026"}`, "tool result fact = %+v", result)
		for i, seq := range []int64{1, 2, 3, 4, 5, 6, 7, 8} {
			testassert.Require(t, pristineFacts[i].Sequence == seq, "fact[%d].Sequence = %d, want ascending %d", i, pristineFacts[i].Sequence, seq)
		}

		listed, err := f.h.ListSessions(ctx)
		testassert.Require(t, err == nil && len(listed) == 1 && listed[0].Identity.SessionID == session, "ListSessions = %+v err %v", listed, err)
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
		// preparation call 0 is op-1's admission; its Close parks before the
		// post-terminal drain's admission, which parks on the successor gate so
		// the selected-but-unresolved delivery state stays stable. The steering
		// item leads the drain: the settled predecessor's completion admits it
		// before the queued head under its own identity. The no-tool turn is
		// the natural completion this same path serves.
		prepGate := make(chan struct{})
		drainGate := make(chan struct{})
		closeArrived := make(chan struct{})
		closeGate := make(chan struct{})
		var closeOnce sync.Once
		var preparedCalls atomic.Int32
		releasePrep := sync.OnceFunc(func() { close(prepGate) })
		releaseDrain := sync.OnceFunc(func() { close(drainGate) })
		releaseClose := sync.OnceFunc(func() { close(closeGate) })
		defer releasePrep()
		defer releaseDrain()
		defer releaseClose()
		defer script.releaseGate()
		f.prepareHook = func(call int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			preparedCalls.Add(1)
			switch call {
			case 0:
				<-prepGate
				base := scriptPrepared(script)
				return harness.PreparedExecution{
					Capture: base.Capture,
					Open: func(ctx context.Context, admission harness.OperationAdmission) (harness.Execution, error) {
						exec, err := base.Open(ctx, admission)
						if err != nil {
							return exec, err
						}
						exec.Close = func() error { // the predecessor's Close parks before the successor's preparation, then fails
							closeOnce.Do(func() {
								close(closeArrived)
								<-closeGate
							})
							return errors.New("cleanup broke")
						}
						return exec, nil
					},
				}, nil
			case 1:
				<-drainGate
			}
			return scriptPrepared(script), nil
		}
		session := createSession(t, f.h)

		baseline, err := f.h.ReadSessionView(ctx, session)
		testassert.Require(t, err == nil && baseline.History.Revision.LocalRevision == 0 && !baseline.History.ExecutionBusy, "baseline = %+v err %v", baseline, err)

		// The idle admission holds the reservation across its preparation.
		submitDone := make(chan struct{})
		go func() {
			defer close(submitDone)
			{
				_, err := f.h.Submit(ctx, harness.SubmitRequest{
					SessionID: session, OperationID: "op-1", Origin: harness.InputOriginUser,
					Content: []model.ContentPart{{Kind: model.PartText, Text: "hello"}}, Mode: harness.MessageModeRegular,
				})
				testassert.Check(t, err == nil, "submit op-1: %v", err)
			}
		}()
		<-f.prepare // the preparation entered and parks on its gate; the reservation is held
		reserved, err := f.h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		if !reserved.History.ExecutionBusy || len(reserved.Operations) != 0 || len(reserved.History.HistoryHeaders()) != 0 {
			t.Fatalf("read during preparation = busy %v ops %d facts %d, want the entirely-old state with the busy reservation",
				reserved.History.ExecutionBusy, len(reserved.Operations), len(reserved.History.HistoryHeaders()))
		}
		testassert.Equal(t, reserved.History.Revision.LocalRevision, baseline.History.Revision.LocalRevision+1)
		releasePrep() // op-1's preparation proceeds
		<-submitDone
		<-script.arrived // the run is parked at its boundary
		installed, err := f.h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		// the run slot's installation and the reservation release
		testassert.Require(t, installed.History.ExecutionBusy && len(installed.Operations) == 1 && installed.Operations[0].OperationID == "op-1" && installed.History.Revision.LocalRevision == reserved.History.Revision.LocalRevision+2, "read after installation = %+v; got %v, want %v", installed, installed.History.Revision.LocalRevision, reserved.History.Revision.LocalRevision+2)

		// Active routing buffers by mode: each Submit's reservation, enqueue,
		// and release are three publications.
		{
			_, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1")
			testassert.NoError(t, err)
		}
		{
			_, err := submit(t, f.h, session, "op-3", harness.MessageModeRegular, "steer-1")
			testassert.NoError(t, err)
		}
		buffered, err := f.h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		testassert.Require(t, len(buffered.Steering) == 1 && buffered.Steering[0].OperationID == "op-3" && len(buffered.Queued) == 1 && buffered.Queued[0].OperationID == "op-2" && buffered.History.Revision.LocalRevision == installed.History.Revision.LocalRevision+6, "buffers after the submits = %+v / %+v; got %v, want %v", buffered.Steering, buffered.Queued, buffered.History.Revision.LocalRevision, installed.History.Revision.LocalRevision+6)
		buffered.Steering[0].Content[0].Text = "mutated"
		buffered.Queued[0].Content[0].Text = "mutated"
		reread, err := f.h.ReadSessionView(ctx, session)
		testassert.Require(t, err == nil && reread.Steering[0].Content[0].Text == "steer-1" && reread.Queued[0].Content[0].Text == "queued-1", "a pending input's content mutation reached the coordinator: %q / %q err %v", reread.Steering[0].Content[0].Text, reread.Queued[0].Content[0].Text, err)

		// Releasing the boundary settles op-1 quietly through the one
		// completion owner; its Close parks before the drain, so success is
		// readable with the heads still pending and no successor prepared,
		// and Archive/Fork reach the held reservation's wait.
		script.releaseGate()
		<-closeArrived // the predecessor's Close parks
		parkedClose, err := f.h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		testassert.Equal(t, parkedClose.Header.CurrentOperationID, "")
		for _, op := range parkedClose.Operations {
			testassert.Require(t, op.OperationID != "op-1" || op.Status == harness.OperationSuccess, "op-1 = %q at the parked Close, want the quiet success readable", op.Status)
		}
		testassert.Require(t, len(parkedClose.Steering) == 1 && parkedClose.Steering[0].OperationID == "op-3" && len(parkedClose.Queued) == 1 && parkedClose.Queued[0].OperationID == "op-2", "heads at the parked Close = %+v / %+v, want both still pending", parkedClose.Steering, parkedClose.Queued)
		{
			got := preparedCalls.Load()
			testassert.Equal(t, got, 1)
		}
		// Archive and Fork reach the held reservation's wait — observed
		// through each call's own waitContext barrier — and their own live
		// contexts cancel the wait; the timeouts only bound hangs.
		archiveBase, archiveCancel := context.WithCancel(ctx)
		defer archiveCancel()
		archiveWait := &waitContext{Context: archiveBase, consulted: make(chan struct{})}
		archiveDone := make(chan error, 1)
		go func() {
			_, err := f.h.ArchiveSession(archiveWait, session)
			archiveDone <- err
		}()
		select {
		case <-archiveWait.consulted: // parked consulting its context in the reservation wait
		case <-time.After(10 * time.Second):
			t.Fatal("Archive never reached the reservation wait")
		}
		select {
		case err := <-archiveDone:
			t.Fatalf("Archive returned %v while the reservation was held, want the wait", err)
		default: // still waiting
		}
		archiveCancel()
		select {
		case err := <-archiveDone:
			testassert.Require(t, errors.Is(err, context.Canceled), "Archive at the held reservation = %v, want the canceled wait", err)
		case <-time.After(10 * time.Second):
			t.Fatal("Archive never returned after its cancellation")
		}
		boundary := ""
		parkedCloseFacts := parkedClose.History.HistoryFacts(historyIDsOf(parkedClose.History.HistoryHeaders()))
		for _, fact := range parkedCloseFacts {
			if fact.Kind == harness.EntryInput && fact.Input != nil && fact.OperationID == "op-1" {
				boundary = fact.EntryID
			}
		}
		testassert.Require(t, boundary != "", "the predecessor's admitted entry is missing from the parked read")
		forkBase, forkCancel := context.WithCancel(ctx)
		defer forkCancel()
		forkWait := &waitContext{Context: forkBase, consulted: make(chan struct{})}
		forkDone := make(chan error, 1)
		go func() {
			_, err := f.h.Fork(forkWait, harness.ForkRequest{
				SourceSessionID: session, BoundaryEntryID: boundary, OperationID: "fork-1",
				Content: []model.ContentPart{{Kind: model.PartText, Text: "fork work"}},
			})
			forkDone <- err
		}()
		select {
		case <-forkWait.consulted: // parked consulting its context in the reservation wait
		case <-time.After(10 * time.Second):
			t.Fatal("Fork never reached the reservation wait")
		}
		select {
		case err := <-forkDone:
			t.Fatalf("Fork returned %v while the reservation was held, want the wait", err)
		default: // still waiting
		}
		forkCancel()
		select {
		case err := <-forkDone:
			testassert.Require(t, errors.Is(err, context.Canceled), "Fork at the held reservation = %v, want the canceled wait", err)
		case <-time.After(10 * time.Second):
			t.Fatal("Fork never returned after its cancellation")
		}
		releaseClose() // completion resumes: the drain selects the steering head and its admission parks
		<-f.prepare    // the drain admission of the steering head (op-3) is preparing
		parked, err := f.h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		testassert.Require(t, len(parked.Steering) == 1 && parked.Steering[0].OperationID == "op-3" && (len(parked.Queued) == 1 && parked.Queued[0].OperationID == "op-2") && parked.Header.CurrentOperationID == "", "steering buffer during the parked delivery = %+v, want the selected head still pending until adoption; queued buffer during the parked delivery = %+v, want the queued head pending behind the steering head; got %v, want %v", parked.Steering, parked.Queued, parked.Header.CurrentOperationID, "")
		for _, op := range parked.Operations {
			testassert.Require(t, op.OperationID != "op-2" && op.OperationID != "op-3" && (op.OperationID != "op-1" || op.Status == harness.OperationSuccess), "the pending item %s committed before its delivery completed; op-1 = %q during the parked delivery, want the quiet success", op.OperationID, op.Status)
		}
		// the one completion owner's reservation is the only local publication
		// between the buffering and the parked admission: one reservation and
		// one release span the whole delivery cycle
		testassert.Equal(t, parked.History.Revision.LocalRevision, buffered.History.Revision.LocalRevision+1)
		releaseDrain()   // the steering head admits, runs, and settles; the queued head follows
		<-script.arrived // the admitted steering Operation's execution started
		awaitTerminal(t, f.h, session, "op-3")
		<-f.prepare      // the queued head's admission preparation
		<-script.arrived // the queued Operation's execution started
		awaitTerminal(t, f.h, session, "op-2")
		{
			err := converge(t, f)
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "cleanup broke"), "Wait = %v, want the retained cleanup failure", err)
		}
		{
			rec, err := f.h.ReadOperation(ctx, session, "op-1")
			testassert.Require(t, err == nil && rec.State.Status == harness.OperationSuccess, "op-1 after Wait = %+v err %v, want the settlement preserved", rec, err)
		}
		final, err := f.h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		testassert.Require(t, !final.History.ExecutionBusy && len(final.Steering) == 0 && len(final.Queued) == 0 && len(final.Operations) == 3, "final read = busy %v steering %d queued %d; got %v, want %v", final.History.ExecutionBusy, len(final.Steering), len(final.Queued), len(final.Operations), 3)
		for _, fact := range final.History.HistoryFacts(historyIDsOf(final.History.HistoryHeaders())) {
			if fact.Kind != harness.EntryInput || fact.Input == nil || len(fact.Input.Content) == 0 {
				continue
			}
			switch fact.Input.Content[0].Text {
			case "hello":
				testassert.Equal(t, fact.OperationID, "op-1")
			case "steer-1":
				testassert.Equal(t, fact.OperationID, "op-3")
			case "queued-1":
				testassert.Equal(t, fact.OperationID, "op-2")
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
			{
				_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
				testassert.NoError(t, err)
			}
			<-script.arrived
			{
				_, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1")
				testassert.NoError(t, err)
			}
			buffered, err := f.h.ReadSessionView(ctx, session)
			testassert.Require(t, err == nil && len(buffered.Queued) == 1, "buffered read = %+v err %v", buffered, err)

			f.cancel()              // the Harness loss closes admission and discards the buffers
			go script.releaseGate() // the parked attempt unblocks; its effect settles and the run converges
			// joins every in-flight execution
			{
				err := f.h.Wait(context.Background())
				testassert.NoError(t, err)
			}
			after, err := f.h.ReadSessionView(ctx, session)
			testassert.NoError(t, err)
			// the discard publication plus the loss path's reservation,
			// release, and run-slot retirement
			testassert.Require(t, len(after.Queued) == 0 && len(after.Steering) == 0 && after.History.Revision.LocalRevision == buffered.History.Revision.LocalRevision+4, "buffers after the loss = %+v / %+v; got %v, want %v", after.Steering, after.Queued, after.History.Revision.LocalRevision, buffered.History.Revision.LocalRevision+4)
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
		f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
		f.defaultPrep.Capture = compactManualCapture()
		defer f.close()
		session := createSession(t, f.h)
		{
			_, err := f.h.Submit(ctx, harness.SubmitRequest{
				SessionID: session, OperationID: "op-1", Origin: harness.InputOriginUser,
				Content: []model.ContentPart{{Kind: model.PartText, Text: "hello"}}, Mode: harness.MessageModeRegular,
			})
			testassert.NoError(t, err)
		}
		awaitTerminal(t, f.h, session, "op-1")
		awaitQuiet(t, f.h, session) // the retiring run's drain publications land before the stable baseline
		before, err := f.h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)

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
		duringCompact, err := f.h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		testassert.Require(t, duringCompact.History.ExecutionBusy, "read during the compact preparation = busy %v, want the held reservation", duringCompact.History.ExecutionBusy)
		for _, op := range duringCompact.Operations {
			testassert.Require(t, op.OperationID != "comp-1", "the compact Operation appeared before its commit")
		}
		testassert.Require(t, duringCompact.History.Revision.LocalRevision > before.History.Revision.LocalRevision, "reservation revision = %d, want an advance past %d", duringCompact.History.Revision.LocalRevision, before.History.Revision.LocalRevision)
		releaseCompactGate()
		{
			err := <-compactDone
			testassert.NoError(t, err)
		}
		awaitTerminal(t, f.h, session, "comp-1")
		after, err := f.h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		var compaction *harness.CompactionEntry
		for _, fact := range after.History.HistoryFacts(historyIDsOf(after.History.HistoryHeaders())) {
			if fact.Compaction != nil {
				compaction = fact.Compaction
			}
		}
		testassert.Require(t, compaction != nil && compaction.Summary == "Summary." && compaction.Model == compactModelRef, "compaction fact = %+v", compaction)
		found := false
		for _, op := range after.Operations {
			if op.OperationID == "comp-1" {
				found = true
			}
		}
		testassert.Require(t, found, "the compact Operation is missing from the read")

		awaitQuiet(t, f.h, session) // the retiring compact run drains and releases before the lifecycle transitions
		{
			_, err := f.h.ArchiveSession(ctx, session)
			testassert.NoError(t, err)
		}
		{
			err := f.h.DeleteSession(ctx, session)
			testassert.NoError(t, err)
		}
		{
			_, err := f.h.ReadSessionView(ctx, session)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "read after deletion = %v, want the not-found class", err)
		}
		listed, err := f.h.ListSessions(ctx)
		testassert.Require(t, err == nil && len(listed) == 0, "ListSessions after deletion = %+v err %v", listed, err)
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
// with typed direct errors, the one-time validation of a corrupt candidate
// that then stays latched, metadata listings that revalidate without
// retaining the corpus, deletion omission, and failure propagation.
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
		{
			err := json.Unmarshal(reg.Payload, &obj)
			testassert.NoError(t, err)
		}
		obj["state"] = json.RawMessage(`{"lifecycle":"bogus"}`)
		bad, err := json.Marshal(obj)
		testassert.NoError(t, err)
		key := harness.RegisterKey{SessionID: first, Kind: harness.RegisterSession}
		{
			err := store.Transact(ctx, func(tx harness.Transaction) error {
				_, err := tx.ReplaceRegister(key, reg.Revision, bad)
				return err
			})
			testassert.NoError(t, err)
		}

		// A malformed stored identity: non-empty, not the durable 32-hex shape.
		{
			err := store.Transact(ctx, func(tx harness.Transaction) error {
				_, err := tx.InsertRegister(harness.RegisterDraft{
					Key:     harness.RegisterKey{SessionID: "corrupt-id", Kind: harness.RegisterSession},
					Payload: json.RawMessage(`{}`),
				})
				return err
			})
			testassert.NoError(t, err)
		}

		counting := &countingValidationStorage{Storage: store, register: map[string]int{}, entries: map[string]int{}}
		lister := newPublicFixture(t, counting, newScriptModel(), nil)
		defer lister.close()

		listed, err := lister.h.ListSessions(ctx)
		testassert.NoError(t, err)
		want := []string{second, third}
		sort.Strings(want) // storage returns sorted identities; creation order is random
		testassert.Require(t, len(listed) == 2 && listed[0].Identity.SessionID == want[0] && listed[1].Identity.SessionID == want[1], "ListSessions = %+v, want the valid siblings in the sorted listing order", listed)
		{
			_, err := lister.h.ReadSessionHeader(ctx, first)
			testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "corrupt direct read = %v, want the corruption error", err)
		}
		{
			_, err := lister.h.ReadSessionHeader(ctx, "corrupt-id")
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "malformed direct read = %v, want the invalid class", err)
		}

		// A listing is metadata-only: each candidate's graph is validated once
		// for that listing and discarded when the candidate's call releases it,
		// so a repeat listing revalidates the corpus instead of retaining it,
		// and the latched corrupt candidate is never re-read at all.
		registersBefore, entriesBefore := counting.counts(second)
		{
			_, err := lister.h.ListSessions(ctx)
			testassert.NoError(t, err)
		}
		registersAfter, entriesAfter := counting.counts(second)
		testassert.Require(t, registersAfter == registersBefore+1 && entriesAfter == entriesBefore+1, "the listed valid Session graph reads = %d/%d -> %d/%d, want exactly one revalidation per listing", registersBefore, entriesBefore, registersAfter, entriesAfter)
		corruptRegistersBefore, _ := counting.counts(first)
		{
			_, err := lister.h.ListSessions(ctx)
			testassert.NoError(t, err)
		}
		corruptRegistersAfter, _ := counting.counts(first)
		testassert.Require(t, corruptRegistersAfter == corruptRegistersBefore, "the latched corrupt Session was revalidated %d times, want none: the corruption sticks to its cell", corruptRegistersAfter-corruptRegistersBefore)
		{
			malformedRegisters, malformedEntries := counting.counts("corrupt-id")
			testassert.Require(t, malformedRegisters == 0 && malformedEntries == 0, "the malformed identity reached storage (%d/%d), want the shape filter to drop it before any read", malformedRegisters, malformedEntries)
		}

		// Deletion removes the row from the listing while the direct read
		// reports the typed absence.
		{
			_, err := lister.h.ArchiveSession(ctx, second)
			testassert.NoError(t, err)
		}
		{
			err := lister.h.DeleteSession(ctx, second)
			testassert.NoError(t, err)
		}
		{
			_, err := lister.h.ArchiveSession(ctx, third)
			testassert.NoError(t, err)
		}
		{
			err := lister.h.DeleteSession(ctx, third)
			testassert.NoError(t, err)
		}
		listed, err = lister.h.ListSessions(ctx)
		testassert.Require(t, err == nil && len(listed) == 0, "ListSessions after deletion = %+v err %v", listed, err)
		{
			_, err := lister.h.ReadSessionHeader(ctx, second)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "deleted direct read = %v, want the not-found class", err)
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
		{
			_, err := probe.h.ListSessions(ctx)
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "enumeration failed"), "ListSessions with a failing enumeration = %v, want the propagated failure", err)
		}
		failing.listErr = nil
		failing.entriesErrFor[session] = errors.New("entry read failed")
		{
			_, err := probe.h.ListSessions(ctx)
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "entry read failed"), "ListSessions with a failing materialization = %v, want the propagated failure", err)
		}
		failing.entriesErrFor[session] = nil

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		{
			_, err := probe.h.ListSessions(canceled)
			testassert.Require(t, err != nil, "ListSessions with a canceled context succeeded, want the propagated cancellation")
		}
	})
}

// TestPublicSnapshotSessionDeletionRace proves the deletion/read race: every
// concurrent read either returns exactly the independent archived baseline
// bytes and history answers or the typed not-found class, never a mixed or
// corrupted read. The marshaled view cannot see the unexported entry prefix,
// so the history accessors are compared explicitly.
func TestPublicSnapshotSessionDeletionRace(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		script := newScriptModel(publicTurn())
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)
		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived
		awaitTerminal(t, f.h, session, "op-1")
		awaitQuiet(t, f.h, session)
		{
			_, err := f.h.ArchiveSession(ctx, session)
			testassert.NoError(t, err)
		}
		baseline, err := f.h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		expected, err := json.Marshal(baseline)
		testassert.NoError(t, err)
		expectedHeaders := baseline.History.HistoryHeaders()
		expectedFacts := baseline.History.HistoryFacts(historyIDsOf(expectedHeaders))
		testassert.Require(t, len(expectedHeaders) != 0 && len(expectedFacts) != 0, "%v", "the archived baseline carries no committed history")

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
					view, err := f.h.ReadSessionView(ctx, session)
					if err != nil {
						if !errors.Is(err, harness.ErrNotFound) {
							errs <- err
							return
						}
						continue
					}
					body, merr := json.Marshal(view)
					if merr != nil {
						errs <- merr
						return
					}
					if !bytes.Equal(expected, body) { // every successful read equals the complete archived state
						errs <- errors.New("a concurrent read observed a different body than the archived baseline")
						return
					}
					if !reflect.DeepEqual(view.History.HistoryHeaders(), expectedHeaders) ||
						!reflect.DeepEqual(view.History.HistoryFacts(historyIDsOf(view.History.HistoryHeaders())), expectedFacts) {
						errs <- errors.New("a concurrent read observed different history answers than the archived baseline")
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
			t.Fatalf("concurrent read failed: %v", err)
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
			return publicExecution(parkingModelFn(gate, arrived), parkingModelFn(gate, arrived)), nil
		},
	}
}

// gatedBackgroundFixture builds the Jobs-seamed public fixture whose first
// admission's execution parks on the first gate and every later admission's
// on the later gate; every model invocation signals arrived.
func gatedBackgroundFixture(t *testing.T, store harness.Storage, stopper harness.JobStopper, script *scriptModel, first, later, arrived chan struct{}) *publicFixture {
	t.Helper()
	f := newPublicFixtureWithJobs(t, store, stopper, script, nil, nil)
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

		baseline, err := h.ReadSessionView(ctx, session)
		testassert.Require(t, err == nil && baseline.History.Revision.LocalRevision == 0 && len(baseline.Background) == 0, "baseline = %+v err %v", baseline, err)

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
		admitted, err := h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		testassert.Require(t, len(admitted.Background) == 1 && admitted.Background[0] == (harness.BackgroundMemberView{Kind: "job", ID: "000000aa"}) && admitted.History.Revision.LocalRevision == baseline.History.Revision.LocalRevision+1, "background during the spawn = %+v; got %v, want %v", admitted.Background, admitted.History.Revision.LocalRevision, baseline.History.Revision.LocalRevision+1)
		close(spawnRelease)
		{
			err := <-startErr
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "spawn down"), "StartJob error = %v, want the spawn failure", err)
		}
		cleaned, err := h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		testassert.Require(t, len(cleaned.Background) == 0 && cleaned.History.Revision.LocalRevision == admitted.History.Revision.LocalRevision+1, "background after the failed start = %+v; got %v, want %v", cleaned.Background, cleaned.History.Revision.LocalRevision, admitted.History.Revision.LocalRevision+1)

		// An active Session steers a delivered completion under the
		// completion identity; the member finishes after the delivery.
		{
			_, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-arrived // the admitted Operation reached its parked boundary
		active, err := h.ReadSessionView(ctx, session)
		testassert.Require(t, err == nil && active.History.ExecutionBusy, "active read = %+v err %v", active, err)
		secondCompletion := ""
		{
			err := h.StartJob(ctx, session, "000000bb", func(_ context.Context, completionID string) error {
				secondCompletion = completionID
				return nil
			})
			testassert.NoError(t, err)
		}
		withMember, err := h.ReadSessionView(ctx, session)
		testassert.Require(t, err == nil && len(withMember.Background) == 1 && withMember.Background[0].ID == "000000bb", "background after the start = %+v err %v", withMember.Background, err)
		beforeDelivery := withMember.History.Revision.LocalRevision
		{
			err := h.DeliverBackgroundCompletion(ctx, session, secondCompletion, "bg answer")
			testassert.NoError(t, err)
		}
		delivered, err := h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		// the enqueue and the finish
		testassert.Require(t, len(delivered.Background) == 0 && (len(delivered.Steering) == 1 && delivered.Steering[0].OperationID == secondCompletion && delivered.Steering[0].Origin == harness.InputOriginRuntime && delivered.Steering[0].Content[0].Text == "bg answer") && delivered.History.Revision.LocalRevision == beforeDelivery+2, "background after the delivery = %+v; steering after the delivery = %+v; got %v, want %v", delivered.Background, delivered.Steering, delivered.History.Revision.LocalRevision, beforeDelivery+2)

		// Converge: the cancellation settles the parked Operation and
		// discards the buffered completion.
		f.cancel()
		go close(parentGate)
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
		}
		final, err := h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		testassert.Require(t, !final.History.ExecutionBusy && len(final.Steering) == 0 && len(final.Background) == 0, "final read = busy %v steering %d background %d", final.History.ExecutionBusy, len(final.Steering), len(final.Background))
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

		{
			_, err := submit(t, h, root, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-arrived // the parent's Operation is parked at its boundary
		parentBefore, err := h.ReadSessionView(ctx, root)
		testassert.Require(t, err == nil && parentBefore.History.Revision.LocalRevision == 3, "parent before the launch = %+v err %v", parentBefore, err)

		res, err := h.LaunchChildSession(ctx, harness.LaunchChildRequest{
			ParentSessionID: root,
			AgentType:       "coder",
			Content:         []model.ContentPart{{Kind: model.PartText, Text: "child work"}},
			OperationID:     "child-op-1",
			MaxConcurrent:   1,
			OutputLimit:     1024,
		})
		testassert.NoError(t, err)
		<-arrived // the child's Operation is parked at its boundary
		parent, err := h.ReadSessionView(ctx, root)
		testassert.NoError(t, err)
		testassert.Require(t, len(parent.Background) == 1 && parent.Background[0] == (harness.BackgroundMemberView{Kind: "child", ID: res.ChildSessionID}) && parent.History.Revision.LocalRevision == parentBefore.History.Revision.LocalRevision+1, "parent background = %+v; got %v, want %v", parent.Background, parent.History.Revision.LocalRevision, parentBefore.History.Revision.LocalRevision+1)
		child, err := h.ReadSessionView(ctx, res.ChildSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, child.History.ExecutionBusy && child.History.Revision.LocalRevision == 1 && len(child.Operations) == 1 && child.Operations[0].OperationID == "child-op-1", "child read = busy %v rev %d ops %+v", child.History.ExecutionBusy, child.History.Revision.LocalRevision, child.Operations)
		childFacts := child.History.HistoryFacts(historyIDsOf(child.History.HistoryHeaders()))
		testassert.Require(t, len(childFacts) == 1 && childFacts[0].Kind == harness.EntryInput, "child facts = %+v", childFacts)

		{
			_, err := submit(t, h, res.ChildSessionID, "op-2", harness.MessageModeRegular, "steer-me")
			testassert.NoError(t, err)
		}
		buffered, err := h.ReadSessionView(ctx, res.ChildSessionID)
		testassert.Require(t, err == nil && len(buffered.Steering) == 1 && buffered.History.Revision.LocalRevision == child.History.Revision.LocalRevision+3, "child buffered read = %+v err %v", buffered, err)

		{
			err := h.Stop(ctx, res.ChildSessionID)
			testassert.NoError(t, err)
		}
		closed, err := h.ReadSessionView(ctx, res.ChildSessionID)
		testassert.NoError(t, err)
		// the closure publication plus the canceled run's retirement drain
		testassert.Require(t, len(closed.Steering) == 0 && !closed.History.ExecutionBusy && closed.History.Revision.LocalRevision == buffered.History.Revision.LocalRevision+4, "closed child = steering %d busy %v; got %v, want %v", len(closed.Steering), closed.History.ExecutionBusy, closed.History.Revision.LocalRevision, buffered.History.Revision.LocalRevision+4)
		closedFacts := closed.History.HistoryFacts(historyIDsOf(closed.History.HistoryHeaders()))
		testassert.Require(t, len(closedFacts) == 3 && closedFacts[0].Kind == harness.EntryInput && closedFacts[1].Kind == harness.EntrySignal && closedFacts[2].Kind == harness.EntryOperationSettlement && closedFacts[2].Settlement.Status == harness.OperationInterruption, "closed child facts = %+v", closedFacts)
		// the repeat stop is a no-op
		{
			err := h.Stop(ctx, res.ChildSessionID)
			testassert.NoError(t, err)
		}
		repeated, err := h.ReadSessionView(ctx, res.ChildSessionID)
		testassert.Require(t, err == nil && repeated.History.Revision.LocalRevision == closed.History.Revision.LocalRevision, "repeat stop revision = %d err %v, want %d", repeated.History.Revision.LocalRevision, err, closed.History.Revision.LocalRevision)

		// The child's completion steered into the active parent and finished
		// the parent's member.
		parent, err = h.ReadSessionView(ctx, root)
		testassert.NoError(t, err)
		// admit, enqueue, finish
		testassert.Require(t, len(parent.Background) == 0 && (len(parent.Steering) == 1 && parent.Steering[0].Origin == harness.InputOriginRuntime && parent.Steering[0].Content[0].Text == "Task interrupted.") && parent.History.Revision.LocalRevision == parentBefore.History.Revision.LocalRevision+3, "parent background after the finish = %+v; parent steering after the delivery = %+v; got %v, want %v", parent.Background, parent.Steering, parent.History.Revision.LocalRevision, parentBefore.History.Revision.LocalRevision+3)

		f.cancel()
		go close(parentGate)
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
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

		{
			_, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-arrived
		before, err := h.ReadSessionView(ctx, session)
		testassert.Require(t, err == nil && before.History.Revision.LocalRevision == 3, "before the start = %+v err %v", before, err)

		{
			err := h.StartJob(ctx, session, "deadbeef", func(_ context.Context, completionID string) error {
				mu.Lock()
				completions["deadbeef"] = completionID
				mu.Unlock()
				return nil
			})
			testassert.NoError(t, err)
		}
		started, err := h.ReadSessionView(ctx, session)
		testassert.Require(t, err == nil && len(started.Background) == 1 && started.Background[0] == (harness.BackgroundMemberView{Kind: "job", ID: "deadbeef"}), "after the start = %+v err %v", started, err)
		testassert.Equal(t, started.History.Revision.LocalRevision, before.History.Revision.LocalRevision+1)

		{
			err := h.Stop(ctx, session)
			testassert.NoError(t, err)
		}
		stopped, err := h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		// the stopping publication, the completion enqueue, the member finish,
		// and the reopen publication
		testassert.Require(t, len(stopped.Background) == 0 && (len(stopped.Steering) == 1 && stopped.Steering[0].Origin == harness.InputOriginRuntime && stopped.Steering[0].Content[0].Text == "job done") && stopped.History.Revision.LocalRevision == started.History.Revision.LocalRevision+4, "background after the stop = %+v; steering after the stop = %+v; got %v, want %v", stopped.Background, stopped.Steering, stopped.History.Revision.LocalRevision, started.History.Revision.LocalRevision+4)
		// the repeat stop is a no-op
		{
			err := h.Stop(ctx, session)
			testassert.NoError(t, err)
		}
		repeated, err := h.ReadSessionView(ctx, session)
		testassert.Require(t, err == nil && repeated.History.Revision.LocalRevision == stopped.History.Revision.LocalRevision, "repeat stop revision = %d err %v, want %d", repeated.History.Revision.LocalRevision, err, stopped.History.Revision.LocalRevision)

		f.cancel()
		go close(parentGate)
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
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
		{
			_, err := f.h.ArchiveSession(ctx, victim)
			testassert.NoError(t, err)
		}
		{
			_, err := f.h.ArchiveSession(ctx, sibling)
			testassert.NoError(t, err)
		}
		survivor, err := f.h.ReadSessionHeader(ctx, sibling)
		testassert.NoError(t, err)

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
		{
			err := f.h.DeleteSession(ctx, victim)
			testassert.NoError(t, err)
		}
		close(wrapped.listRelease)
		result := <-listDone
		testassert.Require(t, result.err == nil && (len(result.rows) == 1 && result.rows[0].Identity.SessionID == sibling), "ListSessions: %v; stale listing = %+v, want only the surviving sibling", result.err, result.rows)
		testassert.Require(t, reflect.DeepEqual(result.rows[0], survivor), "the surviving row = %+v, want the complete archived record %+v", result.rows[0], survivor)
		{
			_, err := f.h.ReadSessionHeader(ctx, victim)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "the disappeared row's direct read = %v, want the typed not-found class", err)
		}
	})
}

// TestPublicSnapshotSessionColdReadFailures proves a never-materialized
// Session's direct hydration read propagates the injected storage failure and
// the canceled caller context exactly, with no partial body. The cold fixture
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
		view, err := f.h.ReadSessionView(ctx, cold)
		testassert.Require(t, errors.Is(err, entriesFailure), "cold read over a failing entry read = %v, want the exact injected storage failure", err)
		testassert.Require(t, reflect.DeepEqual(view, harness.SessionReadView{}), "failing cold read = %+v, want no partial body", view)
		delete(wrapped.entriesErrFor, cold)

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		view, err = f.h.ReadSessionView(canceled, cold)
		testassert.Require(t, errors.Is(err, context.Canceled), "cold read over a canceled context = %v, want the exact context error", err)
		testassert.Require(t, reflect.DeepEqual(view, harness.SessionReadView{}), "canceled cold read = %+v, want no partial body", view)
	})
}

// awaitQuiet waits until one Session's execution slot has retired after its
// terminal settlement, bounding the wait: the coordinator-local publications
// of the post-terminal drain must land before a stable baseline is taken.
func awaitQuiet(t *testing.T, h *harness.Harness, session string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		view, err := h.ReadSessionView(context.Background(), session)
		testassert.NoError(t, err)
		if !view.History.ExecutionBusy {
			return
		}
		testassert.Require(t, !time.Now().After(deadline), "session %s never went quiet within the wait bound", session)
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
		f := newPublicFixtureWithJobs(t, store, snapshotJobStopper{}, script, nil, nil)
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
					return publicExecutionOver(script.effect, script.effect, tool), nil
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
			{
				_, err := h.Submit(ctx, harness.SubmitRequest{
					SessionID: session, OperationID: operationID, Origin: harness.InputOriginUser,
					Content: extraPart(text), Mode: mode,
				})
				testassert.Require(t, err == nil, "submit %s: %v", operationID, err)
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

		parked, err := h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		parkedExpected, err := json.Marshal(parked)
		testassert.NoError(t, err)
		testassert.Require(t, parked.History.ExecutionBusy && len(parked.Operations) == 2, "parked read = busy %v ops %d", parked.History.ExecutionBusy, len(parked.Operations))
		parkedOp, err := h.ReadOperation(ctx, session, "op-2")
		testassert.NoError(t, err)
		testassert.Require(t, parkedOp.State.ActiveEffect != nil && parkedOp.State.ActiveEffect.Kind == harness.EffectTool && len(parkedOp.State.PendingToolCalls) == 1, "parked operation state = %+v, want the active tool effect and its pending call", parkedOp.State)
		testassert.Require(t, len(parked.Steering) == 1 && len(parked.Queued) == 1, "parked buffers = %+v / %+v", parked.Steering, parked.Queued)

		mutatedParked, err := h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		mutatedParked.Steering[0].Content[0].Text = "mutated"
		mutatedParked.Steering[0].Content[0].Extra["q"][6] = '3'
		mutatedParked.Steering[0].Content[0].Extra["p"] = json.RawMessage(`"mutated"`)
		mutatedParked.Steering[0].Content = append(mutatedParked.Steering[0].Content, model.ContentPart{Kind: model.PartText, Text: "forged"})
		mutatedParked.Steering = append(mutatedParked.Steering, harness.PendingInput{OperationID: "forged"})
		mutatedParked.Queued[0].Content[0].Text = "mutated"
		mutatedParked.Queued[0].Content[0].Extra["q"][6] = '3'
		mutatedParked.Queued[0].Content[0].Extra["p"] = json.RawMessage(`"mutated"`)
		mutatedParked.Queued = append(mutatedParked.Queued, harness.PendingInput{OperationID: "forged"})
		mutatedParkedOp, err := h.ReadOperation(ctx, session, "op-2")
		testassert.NoError(t, err)
		mutatedParkedOp.State.ActiveEffect.Kind = "mutated"
		mutatedParkedOp.State.ActiveEffect.ResultEntryID = "mutated"
		mutatedParkedOp.State.ActiveEffect.ToolCallID = "mutated"
		mutatedParkedOp.State.PendingToolCalls[0].CallID = "mutated"
		mutatedParkedOp.State.PendingToolCalls[0].ResultEntryID = "mutated"
		mutatedParkedOp.State.PendingToolCalls = append(mutatedParkedOp.State.PendingToolCalls, harness.PendingToolCall{CallID: "forged"})

		freshParked, err := h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		freshParkedBytes, err := json.Marshal(freshParked)
		testassert.NoError(t, err)
		testassert.Require(t, bytes.Equal(parkedExpected, freshParkedBytes), "a mutation of the parked read reached the coordinator or another read")
		freshParkedOp, err := h.ReadOperation(ctx, session, "op-2")
		testassert.NoError(t, err)
		testassert.Require(t, freshParkedOp.State.ActiveEffect.Kind == harness.EffectTool && len(freshParkedOp.State.PendingToolCalls) == 1 && freshParkedOp.State.ActiveEffect.ToolCallID != "mutated", "a mutation of the parked Operation reached the coordinator: %+v", freshParkedOp.State)

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
		{
			err := h.Interrupt(ctx, session)
			testassert.NoError(t, err)
		}
		script.releaseGate()
		awaitTerminal(t, h, session, "op-5")
		awaitQuiet(t, h, session) // the retiring run's drain publications land before the archived baseline

		// A live job member whose completion is delivered under harness loss
		// commits the operationless background_completion signal.
		completionID := ""
		{
			err := h.StartJob(ctx, session, "deadbeef", func(_ context.Context, id string) error {
				completionID = id
				return nil
			})
			testassert.NoError(t, err)
		}
		f.cancel()
		{
			err := h.DeliverBackgroundCompletion(ctx, session, completionID, "lost work")
			testassert.NoError(t, err)
		}

		// Archive stamps the Session's archived time.
		{
			_, err := h.ArchiveSession(ctx, session)
			testassert.NoError(t, err)
		}

		baseline, err := h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		expected, err := json.Marshal(baseline)
		testassert.NoError(t, err)

		// Every applicable reference must be present in the fixture.
		testassert.Require(t, baseline.Header.ArchivedAt != nil && len(baseline.TotalUsage.ByModel) != 0, "session fixture lost its archived time or usage: %+v", baseline.Header)
		baselineFacts := baseline.History.HistoryFacts(historyIDsOf(baseline.History.HistoryHeaders()))
		expectedFacts, err := json.Marshal(baselineFacts)
		testassert.NoError(t, err)
		kinds := map[harness.EntryKind]int{}
		var relatedOperation, relatedMember, settlementWithUsage, assistantReferences, resultMetadata bool
		for _, fact := range baselineFacts {
			kinds[fact.Kind]++
			switch fact.Kind {
			case harness.EntryInput:
				testassert.Require(t, fact.Input != nil && len(fact.Input.Content) != 0 && len(fact.Input.Content[0].Extra) != 0, "input fact fixture lost its extra-bearing content: %+v", fact)
			case harness.EntryAssistant:
				testassert.Require(t, fact.Assistant != nil, "assistant fact fixture lost its payload: %+v", fact)
				assistantReferences = assistantReferences || (len(fact.Assistant.Extra) > 0 && len(fact.Assistant.ToolCalls) > 0 &&
					len(fact.Assistant.ToolCalls[0].Extra) > 0 && len(fact.Assistant.ToolCalls[0].NormalizedArguments) > 0 &&
					fact.Assistant.Usage != nil)
			case harness.EntryToolResult:
				testassert.Require(t, fact.ToolResult != nil, "tool result fact fixture lost its payload: %+v", fact)
				resultMetadata = resultMetadata || len(fact.ToolResult.Metadata) > 0
			case harness.EntrySignal:
				testassert.Require(t, fact.Signal != nil, "signal fact fixture lost its payload: %+v", fact)
				relatedOperation = relatedOperation || fact.Signal.RelatedOperation != nil
				relatedMember = relatedMember || fact.Signal.RelatedMember != nil
			case harness.EntryCompaction:
				testassert.Require(t, fact.Compaction != nil && fact.Compaction.Usage != nil, "compaction fact fixture lost its usage: %+v", fact.Compaction)
			case harness.EntryOperationSettlement:
				testassert.Require(t, fact.Settlement != nil, "settlement fact fixture lost its payload: %+v", fact)
				settlementWithUsage = settlementWithUsage || (fact.Settlement.Model != nil && fact.Settlement.Usage != nil)
			}
		}
		testassert.Require(t, assistantReferences, "fixture never produced an assistant fact with its full nested reference set")
		testassert.Require(t, resultMetadata, "fixture never produced a tool result carrying tool-owned metadata")
		for _, kind := range []harness.EntryKind{
			harness.EntryInput, harness.EntryAssistant, harness.EntryToolResult,
			harness.EntrySignal, harness.EntryCompaction, harness.EntryOperationSettlement,
		} {
			testassert.Require(t, kinds[kind] != 0, "fixture never produced a %s fact", kind)
		}
		testassert.Require(t, relatedOperation && relatedMember, "fixture never produced both signal reference shapes (related operation %v, related member %v)", relatedOperation, relatedMember)
		testassert.Require(t, settlementWithUsage, "fixture never produced a settlement carrying its model and usage pointers")
		var recordExpected []byte
		for _, op := range baseline.Operations {
			testassert.Require(t, op.SettledAt != nil, "operation fixture lost its settlement pointer: %+v", op)
			record, err := h.ReadOperation(ctx, session, op.OperationID)
			testassert.Require(t, err == nil, "ReadOperation(%s): %v", op.OperationID, err)
			testassert.Require(t, record.State.SettledAt != nil && record.State.Terminal != nil && (len(record.Admission.Execution.Tools) != 0 && len(record.Admission.Execution.Tools[0].Parameters) != 0 && len(record.Admission.Execution.Capabilities) != 0), "operation fixture lost its terminal pointers: %+v; operation fixture lost its execution references: %+v", record.State, record.Admission.Execution)
			if recordExpected == nil {
				recordExpected, err = json.Marshal(record)
				testassert.NoError(t, err)
			}
		}

		// The full mutation matrix on a second returned read. The hydration
		// groups ride the composed view; full execution captures stay at
		// ReadOperation.
		mutated, err := h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		mutated.Header.Identity.Workspace = "mutated"
		*mutated.Header.ArchivedAt = baseline.Header.ArchivedAt.Add(time.Hour)
		mutated.TotalUsage.ByModel[0].Model.Provider = "mutated"
		mutated.TotalUsage.ByModel[0].Usage.InputTokens = 999
		mutated.TotalUsage.ByModel = append(mutated.TotalUsage.ByModel, harness.ModelUsage{})
		mutated.Header.CurrentAgentType = "mutated"
		for i := range mutated.Operations {
			op := &mutated.Operations[i]
			op.Model.Provider = "mutated"
			*op.SettledAt = op.SettledAt.Add(2 * time.Hour)
			op.Detail = "mutated"
			op.Usage.ByModel[0].Usage.OutputTokens = 999
			op.Usage.ByModel = append(op.Usage.ByModel, harness.ModelUsage{})
		}
		mutated.Operations = append(mutated.Operations, harness.OperationSummary{})

		mutatedRecord, err := h.ReadOperation(ctx, session, baseline.Operations[0].OperationID)
		testassert.NoError(t, err)
		mutatedRecord.Admission.Execution.Model.Provider = "mutated"
		mutatedRecord.Admission.Execution.Tools[0].Parameters[9] = 'p'
		mutatedRecord.Admission.Execution.Tools = append(mutatedRecord.Admission.Execution.Tools, model.ToolDefinition{})
		mutatedRecord.Admission.Execution.Capabilities[0] = "mutated"
		mutatedRecord.Admission.Execution.Capabilities = append(mutatedRecord.Admission.Execution.Capabilities, "forged")
		mutatedRecord.Admission.Execution.Compact.SystemPrompt = "mutated"
		*mutatedRecord.State.SettledAt = mutatedRecord.State.SettledAt.Add(2 * time.Hour)
		mutatedRecord.State.Terminal.Detail = "mutated"
		mutatedRecord.State.Terminal.SettlementEntry.EntryID = "mutated"
		mutatedRecord.State.Usage.ByModel[0].Usage.OutputTokens = 999
		mutatedRecord.State.Usage.ByModel = append(mutatedRecord.State.Usage.ByModel, harness.ModelUsage{})
		mutatedRecord.State.PendingToolCalls = append(mutatedRecord.State.PendingToolCalls, harness.PendingToolCall{CallID: "forged"})

		freshRecord, err := h.ReadOperation(ctx, session, baseline.Operations[0].OperationID)
		testassert.NoError(t, err)
		freshRecordBytes, err := json.Marshal(freshRecord)
		testassert.NoError(t, err)
		testassert.Require(t, bytes.Equal(recordExpected, freshRecordBytes), "a mutation of one returned Operation reached the coordinator or another read")

		for i := range baselineFacts {
			fact := &baselineFacts[i]
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
		baselineFacts = append(baselineFacts, harness.HistoryFact{EntryID: "forged"})

		again, err := h.ReadSessionView(ctx, session)
		testassert.NoError(t, err)
		freshBytes, err := json.Marshal(again)
		testassert.NoError(t, err)
		testassert.Require(t, bytes.Equal(expected, freshBytes), "a mutation of one read reached the coordinator or another read")
		freshFacts := readHistoryFacts(t, h, session)
		freshFactsBytes, err := json.Marshal(freshFacts)
		testassert.NoError(t, err)
		testassert.Require(t, bytes.Equal(expectedFacts, freshFactsBytes), "a mutation of one returned fact set reached the coordinator or another read")

		// The mutated read and fact set must actually differ from the
		// expectations, so the comparisons above cannot pass on untouched data.
		mutatedBytes, err := json.Marshal(mutated)
		testassert.NoError(t, err)
		testassert.Require(t, !bytes.Equal(expected, mutatedBytes), "the mutation matrix changed nothing; the ownership comparison is vacuous")
		mutatedFactsBytes, err := json.Marshal(baselineFacts)
		testassert.NoError(t, err)
		testassert.Require(t, !bytes.Equal(expectedFacts, mutatedFactsBytes), "the fact mutation matrix changed nothing; the ownership comparison is vacuous")
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
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
		view, err := h.ReadSessionView(context.Background(), session)
		testassert.NoError(t, err)
		if !view.History.ExecutionBusy && view.Header.CurrentOperationID == "" {
			return
		}
		testassert.Require(t, !time.Now().After(deadline), "session %s never reached the quiet pair", session)
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
		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived
		awaitTerminal(t, f.h, session, "op-1")
		{
			_, err := f.h.ArchiveSession(ctx, session)
			testassert.NoError(t, err)
		}
		awaitStoreQuiet(t, f.h, session)

		rows, err := f.h.ListSessions(ctx)
		testassert.Require(t, err == nil && len(rows) == 1, "ListSessions = %+v err %v", rows, err)
		testassert.Require(t, rows[0].ArchivedAt != nil, "row fixture lost its archived time: %+v", rows[0])
		expected, err := json.Marshal(rows)
		testassert.NoError(t, err)

		mutated, err := f.h.ListSessions(ctx)
		testassert.Require(t, err == nil && len(mutated) == 1, "second ListSessions = %+v err %v", mutated, err)
		*mutated[0].ArchivedAt = mutated[0].ArchivedAt.Add(time.Hour)
		mutated[0].Identity.Workspace = "mutated"
		mutated[0].Lifecycle = "mutated"
		mutated[0].CurrentAgentType = "mutated"
		mutated = append(mutated, harness.SessionHeader{})

		fresh, err := f.h.ListSessions(ctx)
		testassert.Require(t, err == nil && len(fresh) == 1, "fresh ListSessions = %+v err %v", fresh, err)
		freshBytes, err := json.Marshal(fresh)
		testassert.NoError(t, err)
		testassert.Require(t, bytes.Equal(expected, freshBytes), "a mutation of one listing reached the coordinator or another listing")
		mutatedBytes, err := json.Marshal(mutated)
		testassert.NoError(t, err)
		testassert.Require(t, !bytes.Equal(expected, mutatedBytes), "the row mutation changed nothing; the ownership comparison is vacuous")
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
		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived
		awaitTerminal(t, f.h, session, "op-1")
		awaitQuiet(t, f.h, session)
		{
			_, err := f.h.ArchiveSession(context.Background(), session)
			testassert.NoError(t, err)
		}
		baseline, err := f.h.ReadSessionView(context.Background(), session)
		testassert.NoError(t, err)
		expected, err := json.Marshal(baseline)
		testassert.NoError(t, err)
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
				_, err := h.ReadSessionView(context.Background(), session)
				reads <- err
			}()
			close(gate.release)
			{
				err := <-deleteDone
				testassert.NoError(t, err)
			}
			{
				err := <-reads
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "read across the held delete = %v, want the typed not-found class", err)
			}
			{
				_, err := h.ReadSessionView(context.Background(), session)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "post-delete read = %v, want the typed not-found class", err)
			}
			listed, err := h.ListSessions(context.Background())
			testassert.Require(t, err == nil && len(listed) == 0, "post-delete listing = %+v err %v", listed, err)
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
			{
				err := <-deleteDone
				testassert.Require(t, err != nil && strings.Contains(err.Error(), "delete aborted"), "DeleteSession = %v, want the injected rollback failure", err)
			}

			after, err := h.ReadSessionView(context.Background(), session)
			testassert.NoError(t, err)
			fresh, err := json.Marshal(after)
			testassert.NoError(t, err)
			testassert.Require(t, bytes.Equal(expected, fresh), "the rolled-back deletion changed the readable state")
			listed, err := h.ListSessions(context.Background())
			testassert.Require(t, err == nil && len(listed) == 1 && listed[0].Identity.SessionID == session, "post-rollback listing = %+v err %v", listed, err)
		})
	})
}
