package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// releaseCommandGate closes one test gate at most once: an explicit close and
// the deferred cleanup can both run, and a failing assertion never leaves a
// parked model blocking the owner's shutdown.
func releaseCommandGate(gate chan struct{}) {
	select {
	case <-gate:
	default:
		close(gate)
	}
}

// drainCommandModelArrivals consumes every already-signaled controlled model
// arrival so a later wait observes the new admission's own first call.
func drainCommandModelArrivals(e *ownerEnv) {
	for {
		select {
		case <-e.server.arrived:
		default:
			return
		}
	}
}

// commandSubmitRequest builds one regular-mode submit body with one text part.
func commandSubmitRequest(t *testing.T, operationID, text string) protocol.SubmitRequest {
	t.Helper()
	return protocol.SubmitRequest{
		OperationId: operationID,
		Mode:        protocol.SubmitRequestModeRegular,
		Content:     []protocol.ContentPart{textContentPart(t, text, nil)},
	}
}

// commandInputFact returns the committed user input fact owned by one
// Operation of one Session view.
func commandInputFact(t *testing.T, view harness.SessionReadView, operationID string) harness.HistoryFact {
	t.Helper()
	for _, fact := range historyFactsOf(view.History) {
		if fact.Kind == harness.EntryInput && fact.OperationID == operationID {
			return fact
		}
	}
	t.Fatalf("session %q has no committed input fact for operation %q", view.Header.Identity.SessionID, operationID)
	return harness.HistoryFact{}
}

// commandUserInputEntry returns the committed user-origin input entry
// identity owned by one Operation of one Session view.
func commandUserInputEntry(t *testing.T, view harness.SessionReadView, operationID string) string {
	t.Helper()
	for _, fact := range historyFactsOf(view.History) {
		if fact.Kind == harness.EntryInput && fact.OperationID == operationID && fact.Input.Origin == harness.InputOriginUser {
			return fact.EntryID
		}
	}
	t.Fatalf("session %q has no committed user input entry for operation %q", view.Header.Identity.SessionID, operationID)
	return ""
}

// TestSessionCommandCreateHeader covers C1, C2 (create half) and C3 (create
// half): the protocol-shaped wrapper returns the real post-transition
// snapshot header, the empty workspace wraps the shared ErrInvalid sentinel,
// and an unknown Agent type stays selectable until the next preparation.
func TestSessionCommandCreateHeader(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)

		workspace := filepath.Join(e.home, "command-ws")
		header, err := r.createSessionHeader(ctx, workspace, "solo")
		testassert.NoError(t, err)
		snap := sessionViewThroughRuntime(t, r, header.SessionId)
		if !reflect.DeepEqual(header, projectSession(snap.Header)) {
			t.Fatalf("create header = %+v, want the real post-transition read projection %+v", header, projectSession(snap.Header))
		}
		if header.SessionRevision != wireRevision(snap.Header.Revision) {
			t.Fatalf("create header revision = %+v, want the read pair %+v", header.SessionRevision, wireRevision(snap.Header.Revision))
		}
		testassert.Require(t, header.Workspace == workspace && header.AgentType == "solo" && header.Lifecycle == protocol.Open, "create header = %+v, want workspace %q solo open", header, workspace)

		unclean := filepath.Join(e.home, "command-ws", "..", "command-ws")
		uncleanHeader, err := r.createSessionHeader(ctx, unclean, "solo")
		testassert.NoError(t, err)
		testassert.Equal(t, uncleanHeader.Workspace, workspace)

		{
			_, err := r.createSessionHeader(ctx, "", "solo")
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "empty workspace header = %v, want harness.ErrInvalid", err)
		}
		{
			_, err := r.createSession(ctx, "", "solo")
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "empty workspace record = %v, want harness.ErrInvalid", err)
		}

		ghost, err := r.createSessionHeader(ctx, filepath.Join(e.home, "ghost-ws"), "ghost")
		testassert.NoError(t, err)
		testassert.Equal(t, ghost.AgentType, "ghost")
		{
			_, err := r.submitSession(ctx, ghost.SessionId, commandSubmitRequest(t, "op-ghost", "hi"))
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "submit on unknown agent type = %v, want the preparation-time harness.ErrInvalid", err)
		}
		ghostSnap := sessionViewThroughRuntime(t, r, ghost.SessionId)
		testassert.Require(t, len(ghostSnap.Operations) == 0 && ghostSnap.Header.CurrentOperationID == "", "unknown-type preparation failure committed state: %+v", ghostSnap.Header)
	})
}

// TestSessionCommandAgentType covers C2 (set half) and C3 (set half): the
// selection rule is unchanged, the empty name stays invalid, and the returned
// header is the real post-transition snapshot.
func TestSessionCommandAgentType(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)

		rec, err := r.createSession(ctx, filepath.Join(e.home, "type-ws"), "solo")
		testassert.NoError(t, err)
		sessionID := rec.Identity.SessionID

		header, err := r.setSessionAgentType(ctx, sessionID, protocol.SetSessionAgentTypeRequest{AgentType: "ghost"})
		testassert.NoError(t, err)
		testassert.Equal(t, header.AgentType, "ghost")
		snap := sessionViewThroughRuntime(t, r, sessionID)
		if !reflect.DeepEqual(header, projectSession(snap.Header)) || header.SessionRevision != wireRevision(snap.Header.Revision) {
			t.Fatalf("ghost header = %+v, want the real read projection %+v", header, projectSession(snap.Header))
		}

		worker, err := r.setSessionAgentType(ctx, sessionID, protocol.SetSessionAgentTypeRequest{AgentType: "worker"})
		testassert.NoError(t, err)
		workerSnap := sessionViewThroughRuntime(t, r, sessionID)
		if !reflect.DeepEqual(worker, projectSession(workerSnap.Header)) || worker.SessionRevision != wireRevision(workerSnap.Header.Revision) {
			t.Fatalf("worker header = %+v, want the real read projection %+v", worker, projectSession(workerSnap.Header))
		}
		testassert.Require(t, worker.SessionRevision.DurableRevision != header.SessionRevision.DurableRevision, "durable revision did not advance across the committed change: %+v", worker.SessionRevision)

		{
			_, err := r.setSessionAgentType(ctx, sessionID, protocol.SetSessionAgentTypeRequest{})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "empty agent type = %v, want harness.ErrInvalid", err)
		}
		after := sessionViewThroughRuntime(t, r, sessionID)
		testassert.Equal(t, after.Header.CurrentAgentType, "worker")

		{
			_, err := r.setSessionAgentType(ctx, "ffffffffffffffffffffffffffffffff", protocol.SetSessionAgentTypeRequest{AgentType: "worker"})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "set on absent session = %v, want harness.ErrNotFound", err)
		}
	})
}

// TestSessionCommandSubmitModes covers C4, C5 (buffer and op-nil), C6 (submit
// half), C7 (both stores) and the origin oracle: idle modes admit, active
// modes buffer with no invented Operation, retries resolve existing, the
// buffered steering head arrives through its own Operation after the boundary
// handoff, and cross-Session Operation identity reuse is rejected by the
// landed rule.
func TestSessionCommandSubmitModes(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)

		first, err := r.createSession(ctx, filepath.Join(e.home, "modes-a"), "solo")
		testassert.NoError(t, err)
		sessionID := first.Identity.SessionID

		admitted, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-1", "run"))
		testassert.NoError(t, err)
		testassert.Require(t, admitted.Disposition == protocol.SubmitResultDispositionAdmitted && admitted.Operation != nil, "idle regular result = %+v, want admitted with an Operation", admitted)
		testassert.Require(t, admitted.Operation.OperationId == "op-1" && admitted.Operation.Status == protocol.OperationStatusRunning, "idle regular operation = %+v, want op-1 running", admitted.Operation)
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		awaitRestorableSession(t, r, sessionID)

		retry, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-1", "run"))
		testassert.NoError(t, err)
		testassert.Require(t, retry.Disposition == protocol.SubmitResultDispositionExisting && retry.Operation != nil, "same-ID retry = %+v, want existing with the first Operation", retry)
		testassert.Require(t, retry.Operation.OperationId == "op-1" && retry.Operation.AdmittedAt.Equal(admitted.Operation.AdmittedAt), "same-ID retry operation = %+v, want the first-writer admission", retry.Operation)

		queuedIdle, err := r.submitSession(ctx, sessionID, protocol.SubmitRequest{
			OperationId: "op-q-idle",
			Mode:        protocol.SubmitRequestModeQueued,
			Content:     []protocol.ContentPart{textContentPart(t, "queued while idle", nil)},
		})
		testassert.NoError(t, err)
		testassert.Require(t, queuedIdle.Disposition == protocol.SubmitResultDispositionAdmitted && queuedIdle.Operation != nil, "idle queued result = %+v, want ordinary admission", queuedIdle)
		awaitOperation(t, r, sessionID, "op-q-idle", harness.OperationSuccess)
		awaitRestorableSession(t, r, sessionID)

		drainCommandModelArrivals(e)
		gate := make(chan struct{})
		defer releaseCommandGate(gate)
		e.server.setHold(gate)
		active, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-active", "active run"))
		testassert.NoError(t, err)
		testassert.Require(t, active.Disposition == protocol.SubmitResultDispositionAdmitted && active.Operation != nil, "active admission = %+v, want admitted", active)
		<-e.server.arrived // the active model call is parked before the buffers are filled
		steering, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-steer", "steer"))
		testassert.NoError(t, err)
		testassert.Require(t, steering.Disposition == protocol.SubmitResultDispositionSteering && steering.Operation == nil, "steering result = %+v, want steering with nil Operation", steering)
		queued, err := r.submitSession(ctx, sessionID, protocol.SubmitRequest{
			OperationId: "op-queued",
			Mode:        protocol.SubmitRequestModeQueued,
			Content:     []protocol.ContentPart{textContentPart(t, "queued", nil)},
		})
		testassert.NoError(t, err)
		testassert.Require(t, queued.Disposition == protocol.SubmitResultDispositionQueued && queued.Operation == nil, "queued result = %+v, want queued with nil Operation", queued)

		buffered := sessionViewThroughRuntime(t, r, sessionID)
		testassert.Require(t, len(buffered.Steering) == 1 && buffered.Steering[0].OperationID == "op-steer" && buffered.Steering[0].Origin == harness.InputOriginUser && len(buffered.Queued) == 1 && buffered.Queued[0].OperationID == "op-queued" && buffered.Queued[0].Origin == harness.InputOriginUser, "steering buffer = %+v, want the caller-minted op-steer user item; queued buffer = %+v, want the caller-minted op-queued user item", buffered.Steering, buffered.Queued)
		for _, op := range buffered.Operations {
			testassert.Require(t, op.OperationID != "op-steer" && op.OperationID != "op-queued", "buffered input %q was durably admitted before the drain", op.OperationID)
		}

		steeringCommitted := false
		close(gate)
		awaitOperation(t, r, sessionID, "op-active", harness.OperationSuccess)
		awaitOperation(t, r, sessionID, "op-steer", harness.OperationSuccess) // the steering head's own Operation
		awaitOperation(t, r, sessionID, "op-queued", harness.OperationSuccess)
		awaitIdleSession(t, r, sessionID)
		drained := sessionViewThroughRuntime(t, r, sessionID)
		for _, fact := range historyFactsOf(drained.History) {
			if fact.Kind != harness.EntryInput || fact.OperationID != "op-steer" {
				continue
			}
			testassert.Equal(t, fact.Input.Origin, harness.InputOriginUser)
			if fact.Input.Content[0].Text == "steer" {
				steeringCommitted = true
			}
		}
		testassert.Require(t, steeringCommitted, "the steering input never committed as its own Operation's admitted entry")
		activeRec := readOperation(t, r, sessionID, "op-active")
		testassert.Require(t, activeRec.State.Status == harness.OperationSuccess && activeRec.State.Terminal != nil && activeRec.State.Terminal.Detail == "", "active operation = %+v, want the handoff's quiet success", activeRec)

		second, err := r.createSession(ctx, filepath.Join(e.home, "modes-b"), "solo")
		testassert.NoError(t, err)
		_, err = r.submitSession(ctx, second.Identity.SessionID, commandSubmitRequest(t, "op-1", "reuse"))
		testassert.Require(t, errors.Is(err, harness.ErrInvalid) && strings.Contains(err.Error(), "another session"), "cross-Session Operation ID reuse = %v, want the landed invalid rule", err)
		secondSnap := sessionViewThroughRuntime(t, r, second.Identity.SessionID)
		testassert.Require(t, len(secondSnap.Operations) == 0, "cross-Session reuse committed an Operation: %+v", secondSnap.Operations)
	})
}

// TestSessionCommandSubmitContent covers C8: all three content variants and
// raw-valued extras reach the committed input exactly, while every malformed
// body wraps harness.ErrInvalid and commits nothing.
func TestSessionCommandSubmitContent(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)

		rec, err := r.createSession(ctx, filepath.Join(e.home, "content-ws"), "solo")
		testassert.NoError(t, err)
		sessionID := rec.Identity.SessionID

		textExtra := protocol.JSONObject{"n": json.RawMessage("42")}
		imageExtra := protocol.JSONObject{"ratio": json.RawMessage("7.50")}
		opaqueExtra := protocol.JSONObject{
			"n":   json.RawMessage("42"),
			"obj": json.RawMessage(`{"a":[1,true,"x"]}`),
			"s":   json.RawMessage(`"opaque-tag"`),
		}
		var text, image, opaque protocol.ContentPart
		testassert.NoError(t, text.FromTextPart(protocol.TextPart{Kind: protocol.TextPartKindText, Text: "hello", Extra: &textExtra}))
		testassert.NoError(t, image.FromImageURLPart(protocol.ImageURLPart{Kind: protocol.ImageUrl, Url: "https://example.test/x.png", Extra: &imageExtra}))
		testassert.NoError(t, opaque.FromOpaquePart(protocol.OpaquePart{Kind: protocol.Opaque, OpaqueWireType: "audio", Extra: &opaqueExtra}))

		res, err := r.submitSession(ctx, sessionID, protocol.SubmitRequest{
			OperationId: "op-content",
			Mode:        protocol.SubmitRequestModeRegular,
			Content:     []protocol.ContentPart{text, image, opaque},
		})
		testassert.NoError(t, err)
		testassert.Require(t, res.Disposition == protocol.SubmitResultDispositionAdmitted, "content submit = %+v, want admitted", res)
		awaitOperation(t, r, sessionID, "op-content", harness.OperationSuccess)

		snap := sessionViewThroughRuntime(t, r, sessionID)
		fact := commandInputFact(t, snap, "op-content")
		want := []model.ContentPart{
			{Kind: model.PartText, Text: "hello", Extra: model.Extra{"n": json.RawMessage("42")}},
			{Kind: model.PartImageURL, URL: "https://example.test/x.png", Extra: model.Extra{"ratio": json.RawMessage("7.50")}},
			{Kind: model.PartOpaque, OpaqueWireType: "audio", Extra: model.Extra{
				"n":   json.RawMessage("42"),
				"obj": json.RawMessage(`{"a":[1,true,"x"]}`),
				"s":   json.RawMessage(`"opaque-tag"`),
			}},
		}
		testassert.Require(t, reflect.DeepEqual(fact.Input.Content, want) && fact.Input.Origin == harness.InputOriginUser, "committed content = %#v, want raw-preserving %#v; got %v, want %v", fact.Input.Content, want, fact.Input.Origin, harness.InputOriginUser)

		unknown := json.RawMessage(`{"kind":"audio","url":"x"}`)
		var unknownPart protocol.ContentPart
		testassert.NoError(t, json.Unmarshal(unknown, &unknownPart))
		var missingKind protocol.ContentPart
		testassert.NoError(t, json.Unmarshal([]byte(`{"text":"x"}`), &missingKind))
		var badDecode protocol.ContentPart
		testassert.NoError(t, json.Unmarshal([]byte(`{"kind":"text","text":12}`), &badDecode))
		var missingWireType protocol.ContentPart
		testassert.NoError(t, missingWireType.FromOpaquePart(protocol.OpaquePart{Kind: protocol.Opaque}))

		for name, part := range map[string]protocol.ContentPart{
			"unknown kind":     unknownPart,
			"missing kind":     missingKind,
			"decode failure":   badDecode,
			"model validation": missingWireType,
		} {
			_, err := r.submitSession(ctx, sessionID, protocol.SubmitRequest{
				OperationId: "op-bad-" + strings.ReplaceAll(name, " ", "-"),
				Mode:        protocol.SubmitRequestModeRegular,
				Content:     []protocol.ContentPart{part},
			})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "%s = %v, want harness.ErrInvalid", name, err)
		}
		{
			_, err := r.submitSession(ctx, sessionID, protocol.SubmitRequest{
				OperationId: "op-bad-mode",
				Mode:        protocol.SubmitRequestMode("weird"),
				Content:     []protocol.ContentPart{textContentPart(t, "x", nil)},
			})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "invalid mode = %v, want the Harness mode validator", err)
		}
		{
			_, err := r.submitSession(ctx, sessionID, protocol.SubmitRequest{
				Mode:    protocol.SubmitRequestModeRegular,
				Content: []protocol.ContentPart{textContentPart(t, "x", nil)},
			})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "empty operation id = %v, want harness.ErrInvalid", err)
		}

		after := sessionViewThroughRuntime(t, r, sessionID)
		testassert.Require(t, len(after.Operations) == 1 && after.Operations[0].OperationID == "op-content", "malformed submits committed state: %+v", after.Operations)
	})
}

// TestSessionCommandCompact covers C9: the compact producer projects the
// Harness record, retries resolve existing, and the idle guard stays
// Harness-owned.
func TestSessionCommandCompact(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)

		rec, err := r.createSession(ctx, filepath.Join(e.home, "compact-cmd-ws"), "solo")
		testassert.NoError(t, err)
		sessionID := rec.Identity.SessionID
		{
			_, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-seed", "seed"))
			testassert.NoError(t, err)
		}
		awaitOperation(t, r, sessionID, "op-seed", harness.OperationSuccess)
		awaitRestorableSession(t, r, sessionID)

		op, err := r.compactSession(ctx, sessionID, protocol.CompactRequest{OperationId: "op-compact"})
		testassert.NoError(t, err)
		testassert.Require(t, op.OperationId == "op-compact" && op.RequestKind == protocol.Compact, "compact operation = %+v, want op-compact compact", op)
		retry, err := r.compactSession(ctx, sessionID, protocol.CompactRequest{OperationId: "op-compact"})
		testassert.NoError(t, err)
		testassert.Require(t, retry.OperationId == op.OperationId && retry.AdmittedAt.Equal(op.AdmittedAt), "compact retry = %+v, want the first-writer Operation", retry)
		awaitOperation(t, r, sessionID, "op-compact", harness.OperationSuccess)
		awaitIdleSession(t, r, sessionID)

		gate := make(chan struct{})
		defer releaseCommandGate(gate)
		e.server.setHold(gate)
		{
			_, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-run", "run"))
			testassert.NoError(t, err)
		}
		{
			_, err := r.compactSession(ctx, sessionID, protocol.CompactRequest{OperationId: "op-compact-busy"})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "non-idle compact = %v, want the Harness idle guard", err)
		}
		close(gate)
		awaitOperation(t, r, sessionID, "op-run", harness.OperationSuccess)
		awaitIdleSession(t, r, sessionID)
	})
}

// TestSessionCommandFork covers C6 (fork half), C10, C11 and corrections
// A1–A3: the boundary item resolves against the source snapshot, a valid
// source never falls back to a destination scan, a live-source fork performs
// exactly the Harness's own enumeration, and the destination header/revision
// compare strictly while the destination's first model effect is held, then
// the same request retries to the identical destination after the source is
// deleted while a mismatched boundary or operation stays typed unavailable.
func TestSessionCommandFork(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		counting := &countingStore{Storage: store}
		r, e := openProjectionRuntime(t, counting)
		defer closeProjectionRuntime(r)

		source, err := r.createSession(ctx, filepath.Join(e.home, "fork-src"), "solo")
		testassert.NoError(t, err)
		sourceID := source.Identity.SessionID
		{
			_, err := r.submitSession(ctx, sourceID, commandSubmitRequest(t, "op-src", "boundary"))
			testassert.NoError(t, err)
		}
		awaitOperation(t, r, sourceID, "op-src", harness.OperationSuccess)
		awaitIdleSession(t, r, sourceID)

		sourceSnap := sessionViewThroughRuntime(t, r, sourceID)
		boundaryEntry := commandUserInputEntry(t, sourceSnap, "op-src")
		boundaryItem := projectItemID(sourceID, boundaryEntry)
		assistantEntry := ""
		for _, fact := range historyFactsOf(sourceSnap.History) {
			if fact.Kind == harness.EntryAssistant {
				assistantEntry = fact.EntryID
			}
		}
		testassert.Require(t, assistantEntry != "", "fixture committed no assistant entry")

		// A valid source resolves its own boundary: neither invalid row may
		// enumerate destinations as a fallback.
		for name, req := range map[string]protocol.ForkRequest{
			"nonexistent item": {BoundaryItemId: "no-such-item", OperationId: "op-bad-a", Content: []protocol.ContentPart{textContentPart(t, "fork", nil)}},
			"non-user item":    {BoundaryItemId: projectItemID(sourceID, assistantEntry), OperationId: "op-bad-b", Content: []protocol.ContentPart{textContentPart(t, "fork", nil)}},
		} {
			before := counting.listCount()
			{
				_, err := r.forkSession(ctx, sourceID, req)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "%s fork = %v, want harness.ErrInvalid", name, err)
			}
			{
				got := counting.listCount() - before
				testassert.Require(t, got == 0, "%s fork issued %d session enumerations, want none from a valid source", name, got)
			}
		}

		// Arm the destination's first model effect before fork preparation and
		// hold it through the response and every header comparison; the
		// deferred release runs before the deferred Close, so no failure path
		// can leave the owner parked.
		drainCommandModelArrivals(e)
		gate := make(chan struct{})
		defer releaseCommandGate(gate)
		e.server.setHold(gate)

		req := protocol.ForkRequest{
			BoundaryItemId: boundaryItem,
			OperationId:    "op-fork",
			Content:        []protocol.ContentPart{textContentPart(t, "forked", nil)},
		}
		before := counting.listCount()
		result, err := r.forkSession(ctx, sourceID, req)
		testassert.NoError(t, err)
		testassert.Equal(t, counting.listCount()-before, 1)
		destID := result.Session.SessionId
		testassert.Require(t, destID != sourceID && result.Session.SourceSessionId != nil && *result.Session.SourceSessionId == sourceID, "fork destination = %+v, want independent lineage from %q", result.Session, sourceID)
		testassert.Require(t, result.Operation.OperationId == "op-fork" && result.Operation.Status == protocol.OperationStatusRunning, "fork operation = %+v, want running op-fork", result.Operation)
		<-e.server.arrived // rendezvous: the destination's first model effect is parked
		{
			status, err := operationStatusOf(store, destID, "op-fork")
			testassert.Require(t, err == nil && status == harness.OperationRunning, "fork header rendezvous = (%q, %v), want the destination still running", status, err)
		}
		destSnap := sessionViewThroughRuntime(t, r, destID)
		if !reflect.DeepEqual(result.Session, projectSession(destSnap.Header)) || result.Session.SessionRevision != wireRevision(destSnap.Header.Revision) {
			t.Fatalf("fork header = %+v, want the real read projection %+v", result.Session, projectSession(destSnap.Header))
		}

		retry, err := r.forkSession(ctx, sourceID, req)
		testassert.NoError(t, err)
		testassert.Require(t, reflect.DeepEqual(retry.Session, result.Session) && retry.Operation.OperationId == "op-fork", "fork retry = (%+v,%+v), want the identical first destination %+v", retry.Session, retry.Operation, result)

		{
			header, err := r.archiveSession(ctx, sourceID)
			testassert.Require(t, err == nil && header.Lifecycle == protocol.Archived, "archive source = (%+v, %v), want archived", header, err)
		}
		testassert.NoError(t, r.deleteSession(ctx, sourceID))

		afterDelete, err := r.forkSession(ctx, sourceID, req)
		testassert.NoError(t, err)
		testassert.Require(t, reflect.DeepEqual(afterDelete.Session, result.Session) && afterDelete.Operation.OperationId == "op-fork", "post-deletion retry = (%+v,%+v), want the identical destination %+v", afterDelete.Session, afterDelete.Operation, result)
		{
			_, err := r.forkSession(ctx, sourceID, protocol.ForkRequest{
				BoundaryItemId: projectItemID(sourceID, assistantEntry),
				OperationId:    "op-fork",
				Content:        []protocol.ContentPart{textContentPart(t, "forked", nil)},
			})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "mismatched boundary after source deletion = %v, want typed source unavailability", err)
		}
		{
			_, err := r.forkSession(ctx, sourceID, protocol.ForkRequest{
				BoundaryItemId: boundaryItem,
				OperationId:    "op-other",
				Content:        []protocol.ContentPart{textContentPart(t, "forked", nil)},
			})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "unknown operation after source deletion = %v, want typed source unavailability", err)
		}
		// The gate held the destination through every comparison: it must
		// still be running at the release point.
		{
			status, err := operationStatusOf(store, destID, "op-fork")
			testassert.Require(t, err == nil && status == harness.OperationRunning, "destination settled before release: (%q, %v), want still running", status, err)
		}

		releaseCommandGate(gate)
		awaitOperation(t, r, destID, "op-fork", harness.OperationSuccess)
		awaitRestorableSession(t, r, destID)

		sessions, err := r.listSessions(ctx, protocol.ListSessionsParams{Workspace: filepath.Join(e.home, "fork-src"), Lifecycle: protocol.Open})
		testassert.NoError(t, err)
		testassert.Require(t, len(sessions) == 1 && sessions[0].SessionId == destID, "workspace sessions = %+v, want exactly the destination %q", sessions, destID)
	})
}

// TestSessionCommandForkCorruptSource covers correction A2's cold-corruption
// sibling: an unavailable source still retries to its valid committed
// destination, while a request with no matching destination lineage returns
// the original source error unchanged.
func TestSessionCommandForkCorruptSource(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		first, e := openProjectionRuntime(t, store)
		source, err := first.createSession(ctx, filepath.Join(e.home, "corrupt-fork-src"), "solo")
		testassert.NoError(t, err)
		sourceID := source.Identity.SessionID
		{
			_, err := first.submitSession(ctx, sourceID, commandSubmitRequest(t, "op-src", "boundary"))
			testassert.NoError(t, err)
		}
		awaitOperation(t, first, sourceID, "op-src", harness.OperationSuccess)
		awaitIdleSession(t, first, sourceID)
		sourceSnap := sessionViewThroughRuntime(t, first, sourceID)
		boundaryEntry := commandUserInputEntry(t, sourceSnap, "op-src")
		boundaryItem := projectItemID(sourceID, boundaryEntry)
		req := protocol.ForkRequest{
			BoundaryItemId: boundaryItem,
			OperationId:    "op-fork",
			Content:        []protocol.ContentPart{textContentPart(t, "forked", nil)},
		}
		result, err := first.forkSession(ctx, sourceID, req)
		testassert.NoError(t, err)
		destID := result.Session.SessionId
		awaitOperation(t, first, destID, "op-fork", harness.OperationSuccess)
		awaitRestorableSession(t, first, destID)
		closeProjectionRuntime(first)

		// The source register is corrupted while no Runtime is warm, so the
		// fresh owner materializes it cold and returns its typed corruption.
		corruptSessionRegister(t, store, sourceID)

		second, _ := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(second)
		retry, err := second.forkSession(ctx, sourceID, req)
		testassert.NoError(t, err)
		testassert.Require(t, retry.Session.SessionId == destID && retry.Operation.OperationId == "op-fork", "corrupt-source retry = (%q,%q), want the committed destination %q", retry.Session.SessionId, retry.Operation.OperationId, destID)

		// Nearest forbidden sibling: with no matching destination lineage the
		// original source corruption returns, never nil and never a fabricated
		// success.
		for name, bad := range map[string]protocol.ForkRequest{
			"unknown operation":   {BoundaryItemId: boundaryItem, OperationId: "op-absent", Content: req.Content},
			"mismatched boundary": {BoundaryItemId: projectItemID(sourceID, "ffffffffffffffffffffffffffffffff"), OperationId: "op-fork", Content: req.Content},
		} {
			{
				_, err := second.forkSession(ctx, sourceID, bad)
				testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "%s corrupt-source fork = %v, want the original source corruption", name, err)
			}
		}
	})
}

// TestSessionCommandLifecycle covers C3 (archive half), C12 and C14: archive
// and reopen return real snapshot headers even after a local revision
// publication, a running Session cannot be archived, and deletion removes
// exactly the Session's own artifact tree.
func TestSessionCommandLifecycle(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)

		rec, err := r.createSession(ctx, filepath.Join(e.home, "life-ws"), "solo")
		testassert.NoError(t, err)
		sessionID := rec.Identity.SessionID

		// Publish a local revision: a steering enqueue and its handoff delivery
		// cycle both count, so a fake LocalRevision 0 header is distinguishable.
		drainCommandModelArrivals(e)
		gate := make(chan struct{})
		defer releaseCommandGate(gate)
		e.server.setHold(gate)
		{
			_, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-1", "run"))
			testassert.NoError(t, err)
		}
		<-e.server.arrived // the active model call is parked before the steering enqueue
		{
			_, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-steer", "steer"))
			testassert.NoError(t, err)
		}
		close(gate)
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		awaitRestorableSession(t, r, sessionID)

		before := sessionViewThroughRuntime(t, r, sessionID)
		testassert.Require(t, before.Header.Revision.LocalRevision != 0, "fixture published no coordinator-local revision")

		archived, err := r.archiveSession(ctx, sessionID)
		testassert.NoError(t, err)
		testassert.Require(t, archived.Lifecycle == protocol.Archived && archived.ArchivedAt != nil, "archive header = %+v, want archived with a timestamp", archived)
		archivedSnap := sessionViewThroughRuntime(t, r, sessionID)
		if !reflect.DeepEqual(archived, projectSession(archivedSnap.Header)) || archived.SessionRevision != wireRevision(archivedSnap.Header.Revision) {
			t.Fatalf("archive header = %+v, want the real read projection %+v", archived, projectSession(archivedSnap.Header))
		}
		testassert.Equal(t, archived.SessionRevision.LocalRevision, wireRevision(before.Header.Revision).LocalRevision)

		reopened, err := r.reopenSession(ctx, sessionID)
		testassert.NoError(t, err)
		testassert.Require(t, reopened.Lifecycle == protocol.Open && reopened.ArchivedAt == nil, "reopen header = %+v, want open without a timestamp", reopened)
		reopenedSnap := sessionViewThroughRuntime(t, r, sessionID)
		if !reflect.DeepEqual(reopened, projectSession(reopenedSnap.Header)) || reopened.SessionRevision != wireRevision(reopenedSnap.Header.Revision) {
			t.Fatalf("reopen header = %+v, want the real read projection %+v", reopened, projectSession(reopenedSnap.Header))
		}

		running, err := r.createSession(ctx, filepath.Join(e.home, "life-running"), "solo")
		testassert.NoError(t, err)
		runningGate := make(chan struct{})
		defer releaseCommandGate(runningGate)
		e.server.setHold(runningGate)
		{
			_, err := r.submitSession(ctx, running.Identity.SessionID, commandSubmitRequest(t, "op-running", "run"))
			testassert.NoError(t, err)
		}
		{
			_, err := r.archiveSession(ctx, running.Identity.SessionID)
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "archive running = %v, want the Harness running gate", err)
		}
		close(runningGate)
		awaitOperation(t, r, running.Identity.SessionID, "op-running", harness.OperationSuccess)
		awaitIdleSession(t, r, running.Identity.SessionID)

		ownTree := filepath.Join(r.dataDir, "code", sessionID)
		siblingTree := filepath.Join(r.dataDir, "code", "ffffffffffffffffffffffffffffffff")
		for _, tree := range []string{ownTree, siblingTree} {
			{
				err := os.MkdirAll(filepath.Join(tree, "snapshots", "1"), 0o700)
				testassert.Require(t, err == nil, "seed artifact tree %s: %v", tree, err)
			}
			{
				err := os.WriteFile(filepath.Join(tree, "snapshots", "1", "f"), []byte("x"), 0o600)
				testassert.Require(t, err == nil, "seed artifact file %s: %v", tree, err)
			}
		}
		openSession, err := r.createSession(ctx, filepath.Join(e.home, "life-open"), "solo")
		testassert.NoError(t, err)
		{
			err := r.deleteSession(ctx, openSession.Identity.SessionID)
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "delete open Session = %v, want the archived-only rule", err)
		}
		{
			_, err := os.Stat(filepath.Join(r.dataDir, "code", openSession.Identity.SessionID))
			testassert.Require(t, os.IsNotExist(err), "open delete created or removed artifacts: %v", err)
		}

		{
			_, err := r.archiveSession(ctx, sessionID)
			testassert.NoError(t, err)
		}
		testassert.NoError(t, r.deleteSession(ctx, sessionID))
		{
			_, err := os.Stat(ownTree)
			testassert.Require(t, os.IsNotExist(err), "own artifact tree survived deletion: %v", err)
		}
		{
			_, err := os.Stat(filepath.Join(siblingTree, "snapshots", "1", "f"))
			testassert.NoError(t, err)
		}
		{
			err := headerErrorThroughRuntime(ctx, r, sessionID)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "deleted by-ID read = %v, want harness.ErrNotFound", err)
		}
		testassert.NoError(t, r.deleteSession(ctx, sessionID))
	})
}

// TestSessionCommandArchiveLiveBackground covers C13: the Harness live-member
// gate is reachable through the facade, and a converging stop makes the same
// archive succeed.
func TestSessionCommandArchiveLiveBackground(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		bg := openBackgroundRuntime(t, store, newLifecycleStopper())
		defer func() {
			{
				err := bg.r.Close(ctx)
				testassert.Check(t, err == nil, "Close: %v", err)
			}
		}()

		root := bg.session("archive-bg")
		childStarted := make(chan struct{}, 1)
		bg.e.server.setScript(func(ctx context.Context, body string) []string {
			if lastUserText(body) != "child work" {
				return nil // every other turn: the default completed turn
			}
			select {
			case childStarted <- struct{}{}:
			default:
			}
			<-ctx.Done() // the child's model request parks until its run cancels
			return nil
		})
		child := bg.launchChild(root, "child work", "op-child", 5)

		{
			_, err := bg.r.archiveSession(ctx, root)
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "archive with live background = %v, want the Harness live-member gate", err)
		}
		<-childStarted // the child's execution is open before the stop races it
		testassert.NoError(t, bg.r.stopSession(ctx, root))
		awaitOperation(t, bg.r, child, "op-child", harness.OperationInterruption)
		awaitIdleSession(t, bg.r, root)

		header, err := bg.r.archiveSession(ctx, root)
		testassert.NoError(t, err)
		testassert.Require(t, header.Lifecycle == protocol.Archived, "archive after stop = %+v, want archived", header)
	})
}

// TestSessionCommandInterruptStop covers C15: interrupt returns before the
// terminal settlement, child stop converges and permanently closes admission,
// and a root stop converges members without touching the root Operation or
// its pending buffer.
func TestSessionCommandInterruptStop(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()

		t.Run("interrupt returns before the terminal settlement", func(t *testing.T) {
			bg := openBackgroundRuntime(t, store, newLifecycleStopper())
			defer func() { _ = bg.r.Close(ctx) }()
			root := bg.session("interrupt-root")
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			defer releaseCommandGate(release)
			bg.e.server.setScript(func(sctx context.Context, body string) []string {
				if lastUserText(body) != "run" {
					return nil
				}
				select {
				case started <- struct{}{}:
				default:
				}
				select { // the model request parks until the gate or the run's cancellation
				case <-release:
					return textTurnEvents("done")
				case <-sctx.Done():
					return nil
				}
			})
			{
				_, err := bg.r.submitSession(ctx, root, commandSubmitRequest(t, "op-int", "run"))
				testassert.NoError(t, err)
			}
			<-started
			testassert.NoError(t, bg.r.interruptSession(ctx, root))
			{
				status, err := operationStatusOf(store, root, "op-int")
				testassert.Require(t, err == nil && status == harness.OperationRunning, "interrupt waited for the terminal: status = (%q, %v), want still running", status, err)
			}
			close(release)
			awaitOperation(t, bg.r, root, "op-int", harness.OperationInterruption)
			testassert.NoError(t, bg.r.interruptSession(ctx, root))
		})

		t.Run("child stop converges and permanently closes admission", func(t *testing.T) {
			bg := openBackgroundRuntime(t, store, newLifecycleStopper())
			defer func() { _ = bg.r.Close(ctx) }()
			root := bg.session("child-stop-root")
			childStarted := make(chan struct{}, 1)
			bg.e.server.setScript(func(sctx context.Context, body string) []string {
				if lastUserText(body) != "child work" {
					return nil
				}
				select {
				case childStarted <- struct{}{}:
				default:
				}
				<-sctx.Done() // the child's model request parks until its run cancels
				return nil
			})
			child := bg.launchChild(root, "child work", "op-child", 5)
			<-childStarted // the child's execution is open before the stop races it
			testassert.NoError(t, bg.r.stopSession(ctx, child))
			awaitOperation(t, bg.r, child, "op-child", harness.OperationInterruption)
			{
				_, err := bg.r.submitSession(ctx, child, commandSubmitRequest(t, "op-after", "late"))
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "submit to stopped child = %v, want the permanent closure", err)
			}
		})

		t.Run("root stop converges members and leaves the root untouched", func(t *testing.T) {
			bg := openBackgroundRuntime(t, store, newLifecycleStopper())
			defer func() { _ = bg.r.Close(ctx) }()
			root := bg.session("member-stop-root")
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			defer releaseCommandGate(release)
			childStarted := make(chan struct{}, 1)
			bg.e.server.setScript(func(sctx context.Context, body string) []string {
				switch lastUserText(body) {
				case "run": // the root's own model request parks on the gate
					select {
					case started <- struct{}{}:
					default:
					}
					select {
					case <-release:
						return textTurnEvents("done")
					case <-sctx.Done():
						return nil
					}
				case "child work": // the child's request parks until its run cancels
					select {
					case childStarted <- struct{}{}:
					default:
					}
					<-sctx.Done()
					return nil
				}
				return nil
			})
			{
				_, err := bg.r.submitSession(ctx, root, commandSubmitRequest(t, "op-root", "run"))
				testassert.NoError(t, err)
			}
			<-started
			{
				res, err := bg.r.submitSession(ctx, root, protocol.SubmitRequest{
					OperationId: "op-wait",
					Mode:        protocol.SubmitRequestModeQueued,
					Content:     []protocol.ContentPart{textContentPart(t, "queued", nil)},
				})
				testassert.Require(t, err == nil && res.Disposition == protocol.SubmitResultDispositionQueued, "queued submit = (%+v, %v), want queued", res, err)
			}
			child := bg.launchChild(root, "child work", "op-member-child", 5)
			<-childStarted

			testassert.NoError(t, bg.r.stopSession(ctx, root))
			awaitOperation(t, bg.r, child, "op-member-child", harness.OperationInterruption)
			{
				status, err := operationStatusOf(store, root, "op-root")
				testassert.Require(t, err == nil && status == harness.OperationRunning, "root stop touched the root Operation: status = (%q, %v), want still running", status, err)
			}
			snap := sessionViewThroughRuntime(t, bg.r, root)
			testassert.Require(t, len(snap.Queued) == 1 && snap.Queued[0].OperationID == "op-wait", "root stop touched the pending buffer: %+v", snap.Queued)

			close(release)
			awaitOperation(t, bg.r, root, "op-root", harness.OperationSuccess)
			awaitOperation(t, bg.r, root, "op-wait", harness.OperationSuccess)
			awaitIdleSession(t, bg.r, root)
		})
	})
}

// TestSessionCommandPreparationError covers C16: a preparation failure
// returns before admission, commits nothing, and does not poison the Session.
func TestSessionCommandPreparationError(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		bg := openBackgroundRuntime(t, store, newLifecycleStopper())
		defer func() { _ = bg.r.Close(ctx) }()

		root := bg.session("prep-fail")
		failure := errors.New("controlled preparation failure")
		bg.hook.failNext(failure)

		res, err := bg.r.submitSession(ctx, root, commandSubmitRequest(t, "op-fail", "run"))
		testassert.Require(t, errors.Is(err, failure), "failing preparation = %v, want the injected error", err)
		testassert.Require(t, res.Disposition == "" && res.Operation == nil, "failing preparation result = %+v, want the zero result", res)
		snap := sessionViewThroughRuntime(t, bg.r, root)
		testassert.Require(t, len(snap.Operations) == 0 && snap.Header.CurrentOperationID == "" && len(snap.History.HistoryHeaders()) == 0, "failing preparation committed state: %+v", snap)

		ok, err := bg.r.submitSession(ctx, root, commandSubmitRequest(t, "op-ok", "run"))
		testassert.NoError(t, err)
		testassert.Require(t, ok.Disposition == protocol.SubmitResultDispositionAdmitted && ok.Operation != nil, "submit after failure = %+v, want admitted", ok)
		awaitOperation(t, bg.r, root, "op-ok", harness.OperationSuccess)
	})
}

// TestSessionCommandClosedOwner covers C17 (after-close half): every command
// producer joins the existing admission gate and returns ErrClosed.
func TestSessionCommandClosedOwner(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		testassert.NoError(t, r.Close(context.Background()))
		ctx := context.Background()
		sessionID := "0123456789abcdef0123456789abcdef"
		workspace := filepath.Join(e.home, "closed-ws")
		req := commandSubmitRequest(t, "op-closed", "x")

		{
			_, err := r.createSessionHeader(ctx, workspace, "solo")
			testassert.Require(t, errors.Is(err, ErrClosed), "closed create header = %v, want ErrClosed", err)
		}
		{
			_, err := r.createSession(ctx, workspace, "solo")
			testassert.Require(t, errors.Is(err, ErrClosed), "closed create = %v, want ErrClosed", err)
		}
		{
			_, err := r.listSessions(ctx, protocol.ListSessionsParams{Workspace: workspace, Lifecycle: protocol.Open})
			testassert.Require(t, errors.Is(err, ErrClosed), "closed list = %v, want ErrClosed", err)
		}
		{
			err := headerErrorThroughRuntime(ctx, r, sessionID)
			testassert.Require(t, errors.Is(err, ErrClosed), "closed read = %v, want ErrClosed", err)
		}
		{
			_, err := r.submitSession(ctx, sessionID, req)
			testassert.Require(t, errors.Is(err, ErrClosed), "closed submit = %v, want ErrClosed", err)
		}
		{
			_, err := r.compactSession(ctx, sessionID, protocol.CompactRequest{OperationId: "op-closed"})
			testassert.Require(t, errors.Is(err, ErrClosed), "closed compact = %v, want ErrClosed", err)
		}
		{
			_, err := r.forkSession(ctx, sessionID, protocol.ForkRequest{BoundaryItemId: "x", OperationId: "op-closed", Content: req.Content})
			testassert.Require(t, errors.Is(err, ErrClosed), "closed fork = %v, want ErrClosed", err)
		}
		{
			_, err := r.setSessionAgentType(ctx, sessionID, protocol.SetSessionAgentTypeRequest{AgentType: "solo"})
			testassert.Require(t, errors.Is(err, ErrClosed), "closed agent type = %v, want ErrClosed", err)
		}
		{
			_, err := r.archiveSession(ctx, sessionID)
			testassert.Require(t, errors.Is(err, ErrClosed), "closed archive = %v, want ErrClosed", err)
		}
		{
			_, err := r.reopenSession(ctx, sessionID)
			testassert.Require(t, errors.Is(err, ErrClosed), "closed reopen = %v, want ErrClosed", err)
		}
		{
			err := r.interruptSession(ctx, sessionID)
			testassert.Require(t, errors.Is(err, ErrClosed), "closed interrupt = %v, want ErrClosed", err)
		}
		{
			err := r.stopSession(ctx, sessionID)
			testassert.Require(t, errors.Is(err, ErrClosed), "closed stop = %v, want ErrClosed", err)
		}
		{
			err := r.deleteSession(ctx, sessionID)
			testassert.Require(t, errors.Is(err, ErrClosed), "closed delete = %v, want ErrClosed", err)
		}
	})
}

// TestSessionCommandCommitJoinsClose covers C17 (before/at-commit half): an
// admitted submit parked after its commit returns its response while Close
// waits, and Close completes only after the release.
func TestSessionCommandCommitJoinsClose(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		gated := newGatedStore(store)
		bg := openBackgroundRuntime(t, gated, newLifecycleStopper())
		defer func() { _ = bg.r.Close(ctx) }()

		root := bg.session("join-close")
		defer gated.releaseGate()
		gated.armPostCommit(1)
		type outcome struct {
			res protocol.SubmitResult
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			res, err := bg.r.submitSession(ctx, root, commandSubmitRequest(t, "op-commit", "run"))
			done <- outcome{res: res, err: err}
		}()
		select {
		case <-gated.parked:
		case <-time.After(10 * time.Second):
			t.Fatal("the admitted submit never committed")
		}

		closeDone := make(chan error, 1)
		go func() { closeDone <- bg.r.Close(ctx) }()
		deadline := time.Now().Add(10 * time.Second)
		for {
			release, err := bg.r.enter(ctx)
			if errors.Is(err, ErrClosed) {
				break
			}
			release()
			testassert.Require(t, !time.Now().After(deadline), "Close never closed admission")
			time.Sleep(time.Millisecond)
		}
		select {
		case <-done:
			t.Fatal("the admitted submit returned before its transaction gate released")
		default:
		}
		select {
		case <-closeDone:
			t.Fatal("Close returned while the admitted submit response was pending")
		default:
		}

		gated.releaseGate()
		got := <-done
		testassert.Require(t, got.err == nil && got.res.Operation != nil && got.res.Operation.OperationId == "op-commit", "admitted submit after gate release = %v, want the committed response; admitted submit result = %+v, want the committed Operation", got.err, got.res)
		testassert.NoError(t, <-closeDone)
	})
}
