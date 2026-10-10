package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/model"
)

// TestSteeringCompletionAndRewind proves the completion and rewind delivery
// contract
// on the mounted Runtime over both stores: a background completion delivered
// to an active parent steers and, after the boundary handoff, its reserved
// completion identity becomes the successor Operation and its own rewind
// boundary; a code restore is refused while the steering input is buffered;
// and a second Session's steering chain records into its own Session's code
// tree, never the first Session's.
func TestSteeringCompletionAndRewind(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, agents.PathForConfig(e.configPath), projectionAgentsDocument)
		stopper := &stubStopper{events: e.events}
		r, err := e.open(context.Background(), e.storagePlugin(store), jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, stopper))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "steer-ws")
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID

		drainCommandModelArrivals(e)
		gate := make(chan struct{})
		e.server.setHold(gate)
		submitCodeActive := func(sessionID, operationID, text string) harness.OperationRecord {
			t.Helper()
			var admitted harness.OperationRecord
			if err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
				res, err := h.Submit(ctx, harness.SubmitRequest{
					SessionID:   sessionID,
					OperationID: operationID,
					Origin:      harness.InputOriginUser,
					Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
					Mode:        harness.MessageModeRegular,
				})
				if err != nil {
					return err
				}
				if res.Disposition != harness.DispositionAdmitted || res.Operation == nil {
					return errors.New("submit was not admitted")
				}
				admitted = *res.Operation
				return nil
			}); err != nil {
				t.Fatalf("Submit(%q): %v", operationID, err)
			}
			return admitted
		}
		op := submitCodeActive(session, "op-1", "only")
		awaitModelArrival(t, e) // the active model call is parked

		var completionID string
		if err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			return h.StartJob(ctx, session, "0a1b2c3d", func(_ context.Context, id string) error {
				completionID = id
				return nil
			})
		}); err != nil {
			t.Fatalf("StartJob: %v", err)
		}
		if err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			return h.DeliverBackgroundCompletion(ctx, session, completionID, "job report")
		}); err != nil {
			t.Fatalf("DeliverBackgroundCompletion: %v", err)
		}

		// The buffered steering head refuses the whole restore before any
		// group is opened: the handoff window carries live pending input.
		if _, err := r.revertSessionCode(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrConflict) {
			t.Fatalf("restore during the buffered steering = %v, want the idle-gate conflict", err)
		}

		releaseCommandGate(gate)
		completionOp := awaitOperation(t, r, session, completionID, harness.OperationSuccess) // the reserved completion identity is the successor
		if got := completionOp.Admission.AdmittedEntry.EntryID; got == "" || got == op.Admission.AdmittedEntry.EntryID {
			t.Fatalf("completion admitted entry = %q, want its own admitted input", got)
		}
		awaitIdleSession(t, r, session)

		snap := snapshotThroughRuntime(t, r, session)
		found := false
		for _, fact := range snap.Facts {
			if fact.Kind == harness.EntryInput && fact.OperationID == completionID {
				if fact.Input == nil || fact.Input.Origin != harness.InputOriginRuntime {
					t.Fatalf("completion input = %+v, want the runtime origin", fact.Input)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("the delivered completion left no input fact under its reserved identity")
		}

		// Every owned user input is a rewind boundary: the completion's own
		// group restores to its boundary while the predecessor's stays.
		firstEntry := op.Admission.AdmittedEntry.EntryID
		completionEntry := completionOp.Admission.AdmittedEntry.EntryID
		firstFile := filepath.Join(workspace, "first.txt")
		if err := os.WriteFile(firstFile, []byte("v0"), 0o600); err != nil {
			t.Fatalf("write first preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(r, session, firstEntry), firstFile, firstFile, []byte("v1"))
		completionFile := filepath.Join(workspace, "completion.txt")
		if err := os.WriteFile(completionFile, []byte("c0"), 0o600); err != nil {
			t.Fatalf("write completion preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(r, session, completionEntry), completionFile, completionFile, []byte("c1"))

		awaitRestorableSession(t, r, session)
		result, err := r.revertSessionCode(context.Background(), session, completionID)
		if err != nil {
			t.Fatalf("revert to the completion boundary: %v", err)
		}
		if len(result.Restored) != 1 || result.Restored[0] != completionFile {
			t.Fatalf("restored = %v, want only the completion group's file", result.Restored)
		}
		if data, err := os.ReadFile(firstFile); err != nil || string(data) != "v1" {
			t.Fatalf("first group's file = (%q, %v), want it untouched by the completion-boundary restore", data, err)
		}

		// A second Session's steering chain records only under its own
		// Session's code tree: the sessions' groups never mix.
		otherWorkspace := filepath.Join(e.home, "steer-ws-2")
		if err := os.MkdirAll(otherWorkspace, 0o700); err != nil {
			t.Fatalf("mkdir second workspace: %v", err)
		}
		other := projectionSession(t, r, otherWorkspace, "solo").Identity.SessionID
		otherGate := make(chan struct{})
		e.server.setHold(otherGate)
		otherOp := submitCodeActive(other, "other-1", "other run")
		awaitModelArrival(t, e)
		var steerDisposition harness.SubmitDisposition
		if err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			res, err := h.Submit(ctx, harness.SubmitRequest{
				SessionID:   other,
				OperationID: "other-2",
				Origin:      harness.InputOriginUser,
				Content:     []model.ContentPart{{Kind: model.PartText, Text: "other steer"}},
				Mode:        harness.MessageModeRegular,
			})
			if err != nil {
				return err
			}
			steerDisposition = res.Disposition
			return nil
		}); err != nil {
			t.Fatalf("second session steering submit: %v", err)
		}
		if steerDisposition != harness.DispositionSteering {
			t.Fatalf("second session steering submit = %q, want steering", steerDisposition)
		}
		otherFile := filepath.Join(otherWorkspace, "other.txt")
		if err := os.WriteFile(otherFile, []byte("o0"), 0o600); err != nil {
			t.Fatalf("write other preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(r, other, otherOp.Admission.AdmittedEntry.EntryID), otherFile, otherFile, []byte("o1"))
		releaseCommandGate(otherGate)
		awaitOperation(t, r, other, "other-1", harness.OperationSuccess)
		awaitOperation(t, r, other, "other-2", harness.OperationSuccess)
		awaitIdleSession(t, r, other)

		if _, err := os.Stat(filepath.Join(r.dataDir, "code", other, otherOp.Admission.AdmittedEntry.EntryID)); err != nil {
			t.Fatalf("second session's own group: %v", err)
		}
		if _, err := os.Stat(filepath.Join(r.dataDir, "code", session, otherOp.Admission.AdmittedEntry.EntryID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("second session's group leaked into the first Session's tree: %v", err)
		}
	})
}
