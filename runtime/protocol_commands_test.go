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
// Operation of one snapshot.
func commandInputFact(t *testing.T, snap harness.SessionSnapshot, operationID string) harness.HistoryFact {
	t.Helper()
	for _, fact := range snap.Facts {
		if fact.Kind == harness.EntryInput && fact.OperationID == operationID {
			return fact
		}
	}
	t.Fatalf("session %q has no committed input fact for operation %q", snap.Session.Identity.SessionID, operationID)
	return harness.HistoryFact{}
}

// commandUserInputEntry returns the committed user-origin input entry
// identity owned by one Operation of one snapshot.
func commandUserInputEntry(t *testing.T, snap harness.SessionSnapshot, operationID string) string {
	t.Helper()
	for _, fact := range snap.Facts {
		if fact.Kind == harness.EntryInput && fact.OperationID == operationID && fact.Input.Origin == harness.InputOriginUser {
			return fact.EntryID
		}
	}
	t.Fatalf("session %q has no committed user input entry for operation %q", snap.Session.Identity.SessionID, operationID)
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
		if err != nil {
			t.Fatalf("createSessionHeader: %v", err)
		}
		snap := snapshotThroughRuntime(t, r, header.SessionId)
		if !reflect.DeepEqual(header, projectSession(snapshotHeader(snap))) {
			t.Fatalf("create header = %+v, want the real post-transition snapshot projection %+v", header, projectSession(snapshotHeader(snap)))
		}
		if header.SessionRevision != wireRevision(snapshotRevision(snap)) {
			t.Fatalf("create header revision = %+v, want the snapshot pair %+v", header.SessionRevision, wireRevision(snapshotRevision(snap)))
		}
		if header.Workspace != workspace || header.AgentType != "solo" || header.Lifecycle != protocol.Open {
			t.Fatalf("create header = %+v, want workspace %q solo open", header, workspace)
		}

		unclean := filepath.Join(e.home, "command-ws", "..", "command-ws")
		uncleanHeader, err := r.createSessionHeader(ctx, unclean, "solo")
		if err != nil {
			t.Fatalf("createSessionHeader(unclean): %v", err)
		}
		if uncleanHeader.Workspace != workspace {
			t.Fatalf("unclean workspace = %q, want the lexical %q", uncleanHeader.Workspace, workspace)
		}

		if _, err := r.createSessionHeader(ctx, "", "solo"); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("empty workspace header = %v, want harness.ErrInvalid", err)
		}
		if _, err := r.createSession(ctx, "", "solo"); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("empty workspace record = %v, want harness.ErrInvalid", err)
		}

		ghost, err := r.createSessionHeader(ctx, filepath.Join(e.home, "ghost-ws"), "ghost")
		if err != nil {
			t.Fatalf("createSessionHeader(ghost) = %v, want the nonempty-only selection rule", err)
		}
		if ghost.AgentType != "ghost" {
			t.Fatalf("ghost header agent type = %q, want ghost", ghost.AgentType)
		}
		if _, err := r.submitSession(ctx, ghost.SessionId, commandSubmitRequest(t, "op-ghost", "hi")); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("submit on unknown agent type = %v, want the preparation-time harness.ErrInvalid", err)
		}
		ghostSnap := snapshotThroughRuntime(t, r, ghost.SessionId)
		if len(ghostSnap.Operations) != 0 || ghostSnap.Session.State.CurrentOperationID != "" {
			t.Fatalf("unknown-type preparation failure committed state: %+v", ghostSnap.Session.State)
		}
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
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sessionID := rec.Identity.SessionID

		header, err := r.setSessionAgentType(ctx, sessionID, protocol.SetSessionAgentTypeRequest{AgentType: "ghost"})
		if err != nil {
			t.Fatalf("setSessionAgentType(ghost) = %v, want the nonempty-only selection rule", err)
		}
		if header.AgentType != "ghost" {
			t.Fatalf("ghost header agent type = %q, want ghost", header.AgentType)
		}
		snap := snapshotThroughRuntime(t, r, sessionID)
		if !reflect.DeepEqual(header, projectSession(snapshotHeader(snap))) || header.SessionRevision != wireRevision(snapshotRevision(snap)) {
			t.Fatalf("ghost header = %+v, want the real snapshot projection %+v", header, projectSession(snapshotHeader(snap)))
		}

		worker, err := r.setSessionAgentType(ctx, sessionID, protocol.SetSessionAgentTypeRequest{AgentType: "worker"})
		if err != nil {
			t.Fatalf("setSessionAgentType(worker): %v", err)
		}
		workerSnap := snapshotThroughRuntime(t, r, sessionID)
		if !reflect.DeepEqual(worker, projectSession(snapshotHeader(workerSnap))) || worker.SessionRevision != wireRevision(snapshotRevision(workerSnap)) {
			t.Fatalf("worker header = %+v, want the real snapshot projection %+v", worker, projectSession(snapshotHeader(workerSnap)))
		}
		if worker.SessionRevision.DurableRevision == header.SessionRevision.DurableRevision {
			t.Fatalf("durable revision did not advance across the committed change: %+v", worker.SessionRevision)
		}

		if _, err := r.setSessionAgentType(ctx, sessionID, protocol.SetSessionAgentTypeRequest{}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("empty agent type = %v, want harness.ErrInvalid", err)
		}
		after := snapshotThroughRuntime(t, r, sessionID)
		if after.Session.State.CurrentAgentType != "worker" {
			t.Fatalf("agent type after empty refusal = %q, want worker", after.Session.State.CurrentAgentType)
		}

		if _, err := r.setSessionAgentType(ctx, "ffffffffffffffffffffffffffffffff", protocol.SetSessionAgentTypeRequest{AgentType: "worker"}); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("set on absent session = %v, want harness.ErrNotFound", err)
		}
	})
}

// TestSessionCommandSubmitModes covers C4, C5 (buffer and op-nil), C6 (submit
// half), C7 (both stores) and the origin oracle: idle modes admit, active
// modes buffer with no invented Operation, retries resolve existing, and
// cross-Session Operation identity reuse is rejected by the landed rule.
func TestSessionCommandSubmitModes(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)

		first, err := r.createSession(ctx, filepath.Join(e.home, "modes-a"), "solo")
		if err != nil {
			t.Fatalf("createSession(a): %v", err)
		}
		sessionID := first.Identity.SessionID

		admitted, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-1", "run"))
		if err != nil {
			t.Fatalf("idle regular submit: %v", err)
		}
		if admitted.Disposition != protocol.SubmitResultDispositionAdmitted || admitted.Operation == nil {
			t.Fatalf("idle regular result = %+v, want admitted with an Operation", admitted)
		}
		if admitted.Operation.OperationId != "op-1" || admitted.Operation.Status != protocol.OperationStatusRunning {
			t.Fatalf("idle regular operation = %+v, want op-1 running", admitted.Operation)
		}
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		awaitRestorableSession(t, r, sessionID)

		retry, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-1", "run"))
		if err != nil {
			t.Fatalf("same-ID retry: %v", err)
		}
		if retry.Disposition != protocol.SubmitResultDispositionExisting || retry.Operation == nil {
			t.Fatalf("same-ID retry = %+v, want existing with the first Operation", retry)
		}
		if retry.Operation.OperationId != "op-1" || !retry.Operation.AdmittedAt.Equal(admitted.Operation.AdmittedAt) {
			t.Fatalf("same-ID retry operation = %+v, want the first-writer admission", retry.Operation)
		}

		queuedIdle, err := r.submitSession(ctx, sessionID, protocol.SubmitRequest{
			OperationId: "op-q-idle",
			Mode:        protocol.SubmitRequestModeQueued,
			Content:     []protocol.ContentPart{textContentPart(t, "queued while idle", nil)},
		})
		if err != nil {
			t.Fatalf("idle queued submit: %v", err)
		}
		if queuedIdle.Disposition != protocol.SubmitResultDispositionAdmitted || queuedIdle.Operation == nil {
			t.Fatalf("idle queued result = %+v, want ordinary admission", queuedIdle)
		}
		awaitOperation(t, r, sessionID, "op-q-idle", harness.OperationSuccess)
		awaitRestorableSession(t, r, sessionID)

		drainCommandModelArrivals(e)
		gate := make(chan struct{})
		defer releaseCommandGate(gate)
		e.server.setHold(gate)
		active, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-active", "active run"))
		if err != nil {
			t.Fatalf("active regular submit: %v", err)
		}
		if active.Disposition != protocol.SubmitResultDispositionAdmitted || active.Operation == nil {
			t.Fatalf("active admission = %+v, want admitted", active)
		}
		<-e.server.arrived // the active model call is parked before the buffers are filled
		steering, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-steer", "steer"))
		if err != nil {
			t.Fatalf("active steering submit: %v", err)
		}
		if steering.Disposition != protocol.SubmitResultDispositionSteering || steering.Operation != nil {
			t.Fatalf("steering result = %+v, want steering with nil Operation", steering)
		}
		queued, err := r.submitSession(ctx, sessionID, protocol.SubmitRequest{
			OperationId: "op-queued",
			Mode:        protocol.SubmitRequestModeQueued,
			Content:     []protocol.ContentPart{textContentPart(t, "queued", nil)},
		})
		if err != nil {
			t.Fatalf("active queued submit: %v", err)
		}
		if queued.Disposition != protocol.SubmitResultDispositionQueued || queued.Operation != nil {
			t.Fatalf("queued result = %+v, want queued with nil Operation", queued)
		}

		buffered := snapshotThroughRuntime(t, r, sessionID)
		if len(buffered.Steering) != 1 || buffered.Steering[0].OperationID != "op-steer" || buffered.Steering[0].Origin != harness.InputOriginUser {
			t.Fatalf("steering buffer = %+v, want the caller-minted op-steer user item", buffered.Steering)
		}
		if len(buffered.Queued) != 1 || buffered.Queued[0].OperationID != "op-queued" || buffered.Queued[0].Origin != harness.InputOriginUser {
			t.Fatalf("queued buffer = %+v, want the caller-minted op-queued user item", buffered.Queued)
		}
		for _, op := range buffered.Operations {
			if op.Admission.OperationID == "op-steer" || op.Admission.OperationID == "op-queued" {
				t.Fatalf("buffered input %q was durably admitted before the drain", op.Admission.OperationID)
			}
		}

		steeringCommitted := false
		close(gate)
		awaitOperation(t, r, sessionID, "op-active", harness.OperationSuccess)
		awaitOperation(t, r, sessionID, "op-queued", harness.OperationSuccess)
		awaitIdleSession(t, r, sessionID)
		drained := snapshotThroughRuntime(t, r, sessionID)
		for _, op := range drained.Operations {
			if op.Admission.OperationID == "op-steer" {
				t.Fatal("the steering item was admitted as a new Operation instead of continuing the active one")
			}
		}
		for _, fact := range drained.Facts {
			if fact.Kind != harness.EntryInput || fact.OperationID != "op-active" {
				continue
			}
			if fact.Input.Origin != harness.InputOriginUser {
				t.Fatalf("committed input origin = %q, want user (the wire carries none)", fact.Input.Origin)
			}
			if fact.Input.Content[0].Text == "steer" {
				steeringCommitted = true
			}
		}
		if !steeringCommitted {
			t.Fatal("the steering input never committed under its owning Operation after the drain")
		}

		second, err := r.createSession(ctx, filepath.Join(e.home, "modes-b"), "solo")
		if err != nil {
			t.Fatalf("createSession(b): %v", err)
		}
		_, err = r.submitSession(ctx, second.Identity.SessionID, commandSubmitRequest(t, "op-1", "reuse"))
		if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "another session") {
			t.Fatalf("cross-Session Operation ID reuse = %v, want the landed invalid rule", err)
		}
		secondSnap := snapshotThroughRuntime(t, r, second.Identity.SessionID)
		if len(secondSnap.Operations) != 0 {
			t.Fatalf("cross-Session reuse committed an Operation: %+v", secondSnap.Operations)
		}
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
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sessionID := rec.Identity.SessionID

		textExtra := protocol.JSONObject{"n": json.RawMessage("42")}
		imageExtra := protocol.JSONObject{"ratio": json.RawMessage("7.50")}
		opaqueExtra := protocol.JSONObject{
			"n":   json.RawMessage("42"),
			"obj": json.RawMessage(`{"a":[1,true,"x"]}`),
			"s":   json.RawMessage(`"opaque-tag"`),
		}
		var text, image, opaque protocol.ContentPart
		if err := text.FromTextPart(protocol.TextPart{Kind: protocol.TextPartKindText, Text: "hello", Extra: &textExtra}); err != nil {
			t.Fatalf("text part: %v", err)
		}
		if err := image.FromImageURLPart(protocol.ImageURLPart{Kind: protocol.ImageUrl, Url: "https://example.test/x.png", Extra: &imageExtra}); err != nil {
			t.Fatalf("image part: %v", err)
		}
		if err := opaque.FromOpaquePart(protocol.OpaquePart{Kind: protocol.Opaque, OpaqueWireType: "audio", Extra: &opaqueExtra}); err != nil {
			t.Fatalf("opaque part: %v", err)
		}

		res, err := r.submitSession(ctx, sessionID, protocol.SubmitRequest{
			OperationId: "op-content",
			Mode:        protocol.SubmitRequestModeRegular,
			Content:     []protocol.ContentPart{text, image, opaque},
		})
		if err != nil {
			t.Fatalf("content submit: %v", err)
		}
		if res.Disposition != protocol.SubmitResultDispositionAdmitted {
			t.Fatalf("content submit = %+v, want admitted", res)
		}
		awaitOperation(t, r, sessionID, "op-content", harness.OperationSuccess)

		snap := snapshotThroughRuntime(t, r, sessionID)
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
		if !reflect.DeepEqual(fact.Input.Content, want) {
			t.Fatalf("committed content = %#v, want raw-preserving %#v", fact.Input.Content, want)
		}
		if fact.Input.Origin != harness.InputOriginUser {
			t.Fatalf("committed content origin = %q, want user", fact.Input.Origin)
		}

		unknown := json.RawMessage(`{"kind":"audio","url":"x"}`)
		var unknownPart protocol.ContentPart
		if err := json.Unmarshal(unknown, &unknownPart); err != nil {
			t.Fatalf("build unknown union: %v", err)
		}
		var missingKind protocol.ContentPart
		if err := json.Unmarshal([]byte(`{"text":"x"}`), &missingKind); err != nil {
			t.Fatalf("build kind-less union: %v", err)
		}
		var badDecode protocol.ContentPart
		if err := json.Unmarshal([]byte(`{"kind":"text","text":12}`), &badDecode); err != nil {
			t.Fatalf("build decode-failing union: %v", err)
		}
		var missingWireType protocol.ContentPart
		if err := missingWireType.FromOpaquePart(protocol.OpaquePart{Kind: protocol.Opaque}); err != nil {
			t.Fatalf("build wire-type-less opaque: %v", err)
		}

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
			if !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("%s = %v, want harness.ErrInvalid", name, err)
			}
		}
		if _, err := r.submitSession(ctx, sessionID, protocol.SubmitRequest{
			OperationId: "op-bad-mode",
			Mode:        protocol.SubmitRequestMode("weird"),
			Content:     []protocol.ContentPart{textContentPart(t, "x", nil)},
		}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("invalid mode = %v, want the Harness mode validator", err)
		}
		if _, err := r.submitSession(ctx, sessionID, protocol.SubmitRequest{
			Mode:    protocol.SubmitRequestModeRegular,
			Content: []protocol.ContentPart{textContentPart(t, "x", nil)},
		}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("empty operation id = %v, want harness.ErrInvalid", err)
		}

		after := snapshotThroughRuntime(t, r, sessionID)
		if len(after.Operations) != 1 || after.Operations[0].Admission.OperationID != "op-content" {
			t.Fatalf("malformed submits committed state: %+v", after.Operations)
		}
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
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sessionID := rec.Identity.SessionID
		if _, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-seed", "seed")); err != nil {
			t.Fatalf("seed submit: %v", err)
		}
		awaitOperation(t, r, sessionID, "op-seed", harness.OperationSuccess)
		awaitRestorableSession(t, r, sessionID)

		op, err := r.compactSession(ctx, sessionID, protocol.CompactRequest{OperationId: "op-compact"})
		if err != nil {
			t.Fatalf("compactSession: %v", err)
		}
		if op.OperationId != "op-compact" || op.RequestKind != protocol.Compact {
			t.Fatalf("compact operation = %+v, want op-compact compact", op)
		}
		retry, err := r.compactSession(ctx, sessionID, protocol.CompactRequest{OperationId: "op-compact"})
		if err != nil {
			t.Fatalf("compact retry: %v", err)
		}
		if retry.OperationId != op.OperationId || !retry.AdmittedAt.Equal(op.AdmittedAt) {
			t.Fatalf("compact retry = %+v, want the first-writer Operation", retry)
		}
		awaitOperation(t, r, sessionID, "op-compact", harness.OperationSuccess)
		awaitIdleSession(t, r, sessionID)

		gate := make(chan struct{})
		defer releaseCommandGate(gate)
		e.server.setHold(gate)
		if _, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-run", "run")); err != nil {
			t.Fatalf("active submit: %v", err)
		}
		if _, err := r.compactSession(ctx, sessionID, protocol.CompactRequest{OperationId: "op-compact-busy"}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("non-idle compact = %v, want the Harness idle guard", err)
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
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sourceID := source.Identity.SessionID
		if _, err := r.submitSession(ctx, sourceID, commandSubmitRequest(t, "op-src", "boundary")); err != nil {
			t.Fatalf("source submit: %v", err)
		}
		awaitOperation(t, r, sourceID, "op-src", harness.OperationSuccess)
		awaitIdleSession(t, r, sourceID)

		sourceSnap := snapshotThroughRuntime(t, r, sourceID)
		boundaryEntry := commandUserInputEntry(t, sourceSnap, "op-src")
		boundaryItem := projectItemID(sourceID, boundaryEntry)
		assistantEntry := ""
		for _, fact := range sourceSnap.Facts {
			if fact.Kind == harness.EntryAssistant {
				assistantEntry = fact.EntryID
			}
		}
		if assistantEntry == "" {
			t.Fatal("fixture committed no assistant entry")
		}

		// A valid source resolves its own boundary: neither invalid row may
		// enumerate destinations as a fallback.
		for name, req := range map[string]protocol.ForkRequest{
			"nonexistent item": {BoundaryItemId: "no-such-item", OperationId: "op-bad-a", Content: []protocol.ContentPart{textContentPart(t, "fork", nil)}},
			"non-user item":    {BoundaryItemId: projectItemID(sourceID, assistantEntry), OperationId: "op-bad-b", Content: []protocol.ContentPart{textContentPart(t, "fork", nil)}},
		} {
			before := counting.listCount()
			if _, err := r.forkSession(ctx, sourceID, req); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("%s fork = %v, want harness.ErrInvalid", name, err)
			}
			if got := counting.listCount() - before; got != 0 {
				t.Fatalf("%s fork issued %d session enumerations, want none from a valid source", name, got)
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
		if err != nil {
			t.Fatalf("forkSession: %v", err)
		}
		if got := counting.listCount() - before; got != 1 {
			t.Fatalf("live-source fork issued %d session enumerations, want exactly the Harness first-writer lookup", got)
		}
		destID := result.Session.SessionId
		if destID == sourceID || result.Session.SourceSessionId == nil || *result.Session.SourceSessionId != sourceID {
			t.Fatalf("fork destination = %+v, want independent lineage from %q", result.Session, sourceID)
		}
		if result.Operation.OperationId != "op-fork" || result.Operation.Status != protocol.OperationStatusRunning {
			t.Fatalf("fork operation = %+v, want running op-fork", result.Operation)
		}
		<-e.server.arrived // rendezvous: the destination's first model effect is parked
		if status, err := operationStatusOf(store, destID, "op-fork"); err != nil || status != harness.OperationRunning {
			t.Fatalf("fork header rendezvous = (%q, %v), want the destination still running", status, err)
		}
		destSnap := snapshotThroughRuntime(t, r, destID)
		if !reflect.DeepEqual(result.Session, projectSession(snapshotHeader(destSnap))) || result.Session.SessionRevision != wireRevision(snapshotRevision(destSnap)) {
			t.Fatalf("fork header = %+v, want the real snapshot projection %+v", result.Session, projectSession(snapshotHeader(destSnap)))
		}

		retry, err := r.forkSession(ctx, sourceID, req)
		if err != nil {
			t.Fatalf("fork same-ID retry: %v", err)
		}
		if !reflect.DeepEqual(retry.Session, result.Session) || retry.Operation.OperationId != "op-fork" {
			t.Fatalf("fork retry = (%+v,%+v), want the identical first destination %+v", retry.Session, retry.Operation, result)
		}

		if header, err := r.archiveSession(ctx, sourceID); err != nil || header.Lifecycle != protocol.Archived {
			t.Fatalf("archive source = (%+v, %v), want archived", header, err)
		}
		if err := r.deleteSession(ctx, sourceID); err != nil {
			t.Fatalf("delete source: %v", err)
		}

		afterDelete, err := r.forkSession(ctx, sourceID, req)
		if err != nil {
			t.Fatalf("fork retry after source deletion: %v", err)
		}
		if !reflect.DeepEqual(afterDelete.Session, result.Session) || afterDelete.Operation.OperationId != "op-fork" {
			t.Fatalf("post-deletion retry = (%+v,%+v), want the identical destination %+v", afterDelete.Session, afterDelete.Operation, result)
		}
		if _, err := r.forkSession(ctx, sourceID, protocol.ForkRequest{
			BoundaryItemId: projectItemID(sourceID, assistantEntry),
			OperationId:    "op-fork",
			Content:        []protocol.ContentPart{textContentPart(t, "forked", nil)},
		}); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("mismatched boundary after source deletion = %v, want typed source unavailability", err)
		}
		if _, err := r.forkSession(ctx, sourceID, protocol.ForkRequest{
			BoundaryItemId: boundaryItem,
			OperationId:    "op-other",
			Content:        []protocol.ContentPart{textContentPart(t, "forked", nil)},
		}); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("unknown operation after source deletion = %v, want typed source unavailability", err)
		}
		// The gate held the destination through every comparison: it must
		// still be running at the release point.
		if status, err := operationStatusOf(store, destID, "op-fork"); err != nil || status != harness.OperationRunning {
			t.Fatalf("destination settled before release: (%q, %v), want still running", status, err)
		}

		releaseCommandGate(gate)
		awaitOperation(t, r, destID, "op-fork", harness.OperationSuccess)
		awaitRestorableSession(t, r, destID)

		sessions, err := r.listSessions(ctx, protocol.ListSessionsParams{Workspace: filepath.Join(e.home, "fork-src"), Lifecycle: protocol.Open})
		if err != nil {
			t.Fatalf("listSessions: %v", err)
		}
		if len(sessions) != 1 || sessions[0].SessionId != destID {
			t.Fatalf("workspace sessions = %+v, want exactly the destination %q", sessions, destID)
		}
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
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sourceID := source.Identity.SessionID
		if _, err := first.submitSession(ctx, sourceID, commandSubmitRequest(t, "op-src", "boundary")); err != nil {
			t.Fatalf("source submit: %v", err)
		}
		awaitOperation(t, first, sourceID, "op-src", harness.OperationSuccess)
		awaitIdleSession(t, first, sourceID)
		sourceSnap := snapshotThroughRuntime(t, first, sourceID)
		boundaryEntry := commandUserInputEntry(t, sourceSnap, "op-src")
		boundaryItem := projectItemID(sourceID, boundaryEntry)
		req := protocol.ForkRequest{
			BoundaryItemId: boundaryItem,
			OperationId:    "op-fork",
			Content:        []protocol.ContentPart{textContentPart(t, "forked", nil)},
		}
		result, err := first.forkSession(ctx, sourceID, req)
		if err != nil {
			t.Fatalf("forkSession: %v", err)
		}
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
		if err != nil {
			t.Fatalf("corrupt-source retry: %v", err)
		}
		if retry.Session.SessionId != destID || retry.Operation.OperationId != "op-fork" {
			t.Fatalf("corrupt-source retry = (%q,%q), want the committed destination %q", retry.Session.SessionId, retry.Operation.OperationId, destID)
		}

		// Nearest forbidden sibling: with no matching destination lineage the
		// original source corruption returns, never nil and never a fabricated
		// success.
		for name, bad := range map[string]protocol.ForkRequest{
			"unknown operation":   {BoundaryItemId: boundaryItem, OperationId: "op-absent", Content: req.Content},
			"mismatched boundary": {BoundaryItemId: projectItemID(sourceID, "ffffffffffffffffffffffffffffffff"), OperationId: "op-fork", Content: req.Content},
		} {
			if _, err := second.forkSession(ctx, sourceID, bad); !errors.Is(err, harness.ErrCorrupt) {
				t.Fatalf("%s corrupt-source fork = %v, want the original source corruption", name, err)
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
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sessionID := rec.Identity.SessionID

		// Publish a local revision: a steering enqueue and its drain pop both
		// count, so a fake LocalRevision 0 header is distinguishable.
		drainCommandModelArrivals(e)
		gate := make(chan struct{})
		defer releaseCommandGate(gate)
		e.server.setHold(gate)
		if _, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-1", "run")); err != nil {
			t.Fatalf("active submit: %v", err)
		}
		<-e.server.arrived // the active model call is parked before the steering enqueue
		if _, err := r.submitSession(ctx, sessionID, commandSubmitRequest(t, "op-steer", "steer")); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		close(gate)
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		awaitRestorableSession(t, r, sessionID)

		before := snapshotThroughRuntime(t, r, sessionID)
		if before.LocalRevision == 0 {
			t.Fatal("fixture published no coordinator-local revision")
		}

		archived, err := r.archiveSession(ctx, sessionID)
		if err != nil {
			t.Fatalf("archiveSession: %v", err)
		}
		if archived.Lifecycle != protocol.Archived || archived.ArchivedAt == nil {
			t.Fatalf("archive header = %+v, want archived with a timestamp", archived)
		}
		archivedSnap := snapshotThroughRuntime(t, r, sessionID)
		if !reflect.DeepEqual(archived, projectSession(snapshotHeader(archivedSnap))) || archived.SessionRevision != wireRevision(snapshotRevision(archivedSnap)) {
			t.Fatalf("archive header = %+v, want the real snapshot projection %+v", archived, projectSession(snapshotHeader(archivedSnap)))
		}
		if archived.SessionRevision.LocalRevision != wireRevision(snapshotRevision(before)).LocalRevision {
			t.Fatalf("archive header local revision = %q, want the real %q", archived.SessionRevision.LocalRevision, wireRevision(snapshotRevision(before)).LocalRevision)
		}

		reopened, err := r.reopenSession(ctx, sessionID)
		if err != nil {
			t.Fatalf("reopenSession: %v", err)
		}
		if reopened.Lifecycle != protocol.Open || reopened.ArchivedAt != nil {
			t.Fatalf("reopen header = %+v, want open without a timestamp", reopened)
		}
		reopenedSnap := snapshotThroughRuntime(t, r, sessionID)
		if !reflect.DeepEqual(reopened, projectSession(snapshotHeader(reopenedSnap))) || reopened.SessionRevision != wireRevision(snapshotRevision(reopenedSnap)) {
			t.Fatalf("reopen header = %+v, want the real snapshot projection %+v", reopened, projectSession(snapshotHeader(reopenedSnap)))
		}

		running, err := r.createSession(ctx, filepath.Join(e.home, "life-running"), "solo")
		if err != nil {
			t.Fatalf("createSession(running): %v", err)
		}
		runningGate := make(chan struct{})
		defer releaseCommandGate(runningGate)
		e.server.setHold(runningGate)
		if _, err := r.submitSession(ctx, running.Identity.SessionID, commandSubmitRequest(t, "op-running", "run")); err != nil {
			t.Fatalf("running submit: %v", err)
		}
		if _, err := r.archiveSession(ctx, running.Identity.SessionID); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("archive running = %v, want the Harness running gate", err)
		}
		close(runningGate)
		awaitOperation(t, r, running.Identity.SessionID, "op-running", harness.OperationSuccess)
		awaitIdleSession(t, r, running.Identity.SessionID)

		ownTree := filepath.Join(r.dataDir, "code", sessionID)
		siblingTree := filepath.Join(r.dataDir, "code", "ffffffffffffffffffffffffffffffff")
		for _, tree := range []string{ownTree, siblingTree} {
			if err := os.MkdirAll(filepath.Join(tree, "snapshots", "1"), 0o700); err != nil {
				t.Fatalf("seed artifact tree %s: %v", tree, err)
			}
			if err := os.WriteFile(filepath.Join(tree, "snapshots", "1", "f"), []byte("x"), 0o600); err != nil {
				t.Fatalf("seed artifact file %s: %v", tree, err)
			}
		}
		openSession, err := r.createSession(ctx, filepath.Join(e.home, "life-open"), "solo")
		if err != nil {
			t.Fatalf("createSession(open): %v", err)
		}
		if err := r.deleteSession(ctx, openSession.Identity.SessionID); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("delete open Session = %v, want the archived-only rule", err)
		}
		if _, err := os.Stat(filepath.Join(r.dataDir, "code", openSession.Identity.SessionID)); !os.IsNotExist(err) {
			t.Fatalf("open delete created or removed artifacts: %v", err)
		}

		if _, err := r.archiveSession(ctx, sessionID); err != nil {
			t.Fatalf("re-archive: %v", err)
		}
		if err := r.deleteSession(ctx, sessionID); err != nil {
			t.Fatalf("deleteSession: %v", err)
		}
		if _, err := os.Stat(ownTree); !os.IsNotExist(err) {
			t.Fatalf("own artifact tree survived deletion: %v", err)
		}
		if _, err := os.Stat(filepath.Join(siblingTree, "snapshots", "1", "f")); err != nil {
			t.Fatalf("sibling artifact tree was touched: %v", err)
		}
		if err := headerErrorThroughRuntime(ctx, r, sessionID); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("deleted by-ID read = %v, want harness.ErrNotFound", err)
		}
		if err := r.deleteSession(ctx, sessionID); err != nil {
			t.Fatalf("repeat delete = %v, want idempotent success", err)
		}
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
			if err := bg.r.Close(ctx); err != nil {
				t.Errorf("Close: %v", err)
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

		if _, err := bg.r.archiveSession(ctx, root); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("archive with live background = %v, want the Harness live-member gate", err)
		}
		<-childStarted // the child's execution is open before the stop races it
		if err := bg.r.stopSession(ctx, root); err != nil {
			t.Fatalf("stopSession: %v", err)
		}
		awaitOperation(t, bg.r, child, "op-child", harness.OperationInterruption)
		awaitIdleSession(t, bg.r, root)

		header, err := bg.r.archiveSession(ctx, root)
		if err != nil {
			t.Fatalf("archive after stop: %v", err)
		}
		if header.Lifecycle != protocol.Archived {
			t.Fatalf("archive after stop = %+v, want archived", header)
		}
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
			if _, err := bg.r.submitSession(ctx, root, commandSubmitRequest(t, "op-int", "run")); err != nil {
				t.Fatalf("submit: %v", err)
			}
			<-started
			if err := bg.r.interruptSession(ctx, root); err != nil {
				t.Fatalf("interruptSession: %v", err)
			}
			if status, err := operationStatusOf(store, root, "op-int"); err != nil || status != harness.OperationRunning {
				t.Fatalf("interrupt waited for the terminal: status = (%q, %v), want still running", status, err)
			}
			close(release)
			awaitOperation(t, bg.r, root, "op-int", harness.OperationInterruption)
			if err := bg.r.interruptSession(ctx, root); err != nil {
				t.Fatalf("idle interrupt = %v, want nil", err)
			}
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
			if err := bg.r.stopSession(ctx, child); err != nil {
				t.Fatalf("stopSession(child): %v", err)
			}
			awaitOperation(t, bg.r, child, "op-child", harness.OperationInterruption)
			if _, err := bg.r.submitSession(ctx, child, commandSubmitRequest(t, "op-after", "late")); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("submit to stopped child = %v, want the permanent closure", err)
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
			if _, err := bg.r.submitSession(ctx, root, commandSubmitRequest(t, "op-root", "run")); err != nil {
				t.Fatalf("root submit: %v", err)
			}
			<-started
			if res, err := bg.r.submitSession(ctx, root, protocol.SubmitRequest{
				OperationId: "op-wait",
				Mode:        protocol.SubmitRequestModeQueued,
				Content:     []protocol.ContentPart{textContentPart(t, "queued", nil)},
			}); err != nil || res.Disposition != protocol.SubmitResultDispositionQueued {
				t.Fatalf("queued submit = (%+v, %v), want queued", res, err)
			}
			child := bg.launchChild(root, "child work", "op-member-child", 5)
			<-childStarted

			if err := bg.r.stopSession(ctx, root); err != nil {
				t.Fatalf("stopSession(root): %v", err)
			}
			awaitOperation(t, bg.r, child, "op-member-child", harness.OperationInterruption)
			if status, err := operationStatusOf(store, root, "op-root"); err != nil || status != harness.OperationRunning {
				t.Fatalf("root stop touched the root Operation: status = (%q, %v), want still running", status, err)
			}
			snap := snapshotThroughRuntime(t, bg.r, root)
			if len(snap.Queued) != 1 || snap.Queued[0].OperationID != "op-wait" {
				t.Fatalf("root stop touched the pending buffer: %+v", snap.Queued)
			}

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
		if !errors.Is(err, failure) {
			t.Fatalf("failing preparation = %v, want the injected error", err)
		}
		if res.Disposition != "" || res.Operation != nil {
			t.Fatalf("failing preparation result = %+v, want the zero result", res)
		}
		snap := snapshotThroughRuntime(t, bg.r, root)
		if len(snap.Operations) != 0 || snap.Session.State.CurrentOperationID != "" || len(snap.Facts) != 0 {
			t.Fatalf("failing preparation committed state: %+v", snap)
		}

		ok, err := bg.r.submitSession(ctx, root, commandSubmitRequest(t, "op-ok", "run"))
		if err != nil {
			t.Fatalf("submit after failure: %v", err)
		}
		if ok.Disposition != protocol.SubmitResultDispositionAdmitted || ok.Operation == nil {
			t.Fatalf("submit after failure = %+v, want admitted", ok)
		}
		awaitOperation(t, bg.r, root, "op-ok", harness.OperationSuccess)
	})
}

// TestSessionCommandClosedOwner covers C17 (after-close half): every command
// producer joins the existing admission gate and returns ErrClosed.
func TestSessionCommandClosedOwner(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		ctx := context.Background()
		sessionID := "0123456789abcdef0123456789abcdef"
		workspace := filepath.Join(e.home, "closed-ws")
		req := commandSubmitRequest(t, "op-closed", "x")

		if _, err := r.createSessionHeader(ctx, workspace, "solo"); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed create header = %v, want ErrClosed", err)
		}
		if _, err := r.createSession(ctx, workspace, "solo"); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed create = %v, want ErrClosed", err)
		}
		if _, err := r.listSessions(ctx, protocol.ListSessionsParams{Workspace: workspace, Lifecycle: protocol.Open}); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed list = %v, want ErrClosed", err)
		}
		if err := headerErrorThroughRuntime(ctx, r, sessionID); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed read = %v, want ErrClosed", err)
		}
		if _, err := r.submitSession(ctx, sessionID, req); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed submit = %v, want ErrClosed", err)
		}
		if _, err := r.compactSession(ctx, sessionID, protocol.CompactRequest{OperationId: "op-closed"}); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed compact = %v, want ErrClosed", err)
		}
		if _, err := r.forkSession(ctx, sessionID, protocol.ForkRequest{BoundaryItemId: "x", OperationId: "op-closed", Content: req.Content}); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed fork = %v, want ErrClosed", err)
		}
		if _, err := r.setSessionAgentType(ctx, sessionID, protocol.SetSessionAgentTypeRequest{AgentType: "solo"}); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed agent type = %v, want ErrClosed", err)
		}
		if _, err := r.archiveSession(ctx, sessionID); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed archive = %v, want ErrClosed", err)
		}
		if _, err := r.reopenSession(ctx, sessionID); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed reopen = %v, want ErrClosed", err)
		}
		if err := r.interruptSession(ctx, sessionID); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed interrupt = %v, want ErrClosed", err)
		}
		if err := r.stopSession(ctx, sessionID); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed stop = %v, want ErrClosed", err)
		}
		if err := r.deleteSession(ctx, sessionID); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed delete = %v, want ErrClosed", err)
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
			if time.Now().After(deadline) {
				t.Fatal("Close never closed admission")
			}
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
		if got.err != nil {
			t.Fatalf("admitted submit after gate release = %v, want the committed response", got.err)
		}
		if got.res.Operation == nil || got.res.Operation.OperationId != "op-commit" {
			t.Fatalf("admitted submit result = %+v, want the committed Operation", got.res)
		}
		if err := <-closeDone; err != nil {
			t.Fatalf("Close after the admitted response: %v", err)
		}
	})
}
