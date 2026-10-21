package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
	"golang.org/x/sys/unix"
)

// --- target code-snapshot fixtures ---

// codeGroupRoot returns one target Operation group's on-disk root.
func codeGroupRoot(r *Runtime, sessionID, entryID string) string {
	return filepath.Join(r.dataDir, "code", sessionID, entryID)
}

// captureCodeMutation drives one real CodeStore capture exactly like a
// mutating tool: the on-disk preimage is captured, content is written, and the
// post-mutation identity is recorded and retained.
func captureCodeMutation(t *testing.T, groupRoot, displayPath, target string, content []byte) {
	t.Helper()
	group, err := snapshot.OpenCodeStore(groupRoot)
	testassert.Require(t, err == nil, "OpenCodeStore(%s): %v", groupRoot, err)
	entryID, _, err := group.SnapshotResolvedEntry(1, displayPath, target)
	testassert.Require(t, err == nil, "SnapshotResolvedEntry(%s): %v", target, err)
	{
		err := os.WriteFile(target, content, 0o600)
		testassert.Require(t, err == nil, "WriteFile(%s): %v", target, err)
	}
	{
		err := group.RecordSnapshotContent(1, entryID, content)
		testassert.Require(t, err == nil, "RecordSnapshotContent(%s): %v", target, err)
	}
	group.RetainSnapshotEntry(1, entryID)
}

// submitCodeOperation submits one regular message and waits for its admitted
// terminal, tolerating the terminal-to-retirement buffering window: a
// buffered submission drains under the same caller ID.
func submitCodeOperation(t *testing.T, r *Runtime, sessionID, operationID, text string) harness.OperationRecord {
	t.Helper()
	_, err := submitRawThroughRuntime(context.Background(), r, sessionID, operationID, text, harness.MessageModeRegular)
	testassert.Require(t, err == nil, "Submit(%q): %v", operationID, err)
	rec := awaitOperation(t, r, sessionID, operationID, harness.OperationSuccess)
	awaitIdleSession(t, r, sessionID)
	return rec
}

// awaitRestorableSession polls one Session until its snapshot satisfies the
// one idle gate the deferred restore reads: no running/reserved/retiring work,
// empty pending FIFOs, and no live background member.
func awaitRestorableSession(t *testing.T, r *Runtime, sessionID string) harness.SessionReadView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap := sessionViewThroughRuntime(t, r, sessionID)
		if !snap.History.ExecutionBusy && len(snap.Steering) == 0 && len(snap.Queued) == 0 && len(snap.Background) == 0 {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %q never reached the restorable state: busy=%v steering=%d queued=%d background=%d",
				sessionID, snap.History.ExecutionBusy, len(snap.Steering), len(snap.Queued), len(snap.Background))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- target rows ---

// TestCodeSnapshotsTargetGroups proves one Session's listed groups: admission
// order (not OperationID order), admitted-entry group identity, only recorded
// groups, and the retained display-path/preimage-existence file shape.
func TestCodeSnapshotsTargetGroups(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "code-groups")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID

		// A valid Session with no recorded groups lists a non-nil empty array
		// before any capture exists.
		initial, err := r.listSessionCodeSnapshots(context.Background(), session)
		testassert.Require(t, err == nil && initial != nil && len(initial) == 0, "initial group list = (%+v, %v), want a non-nil empty list", initial, err)

		// Admission order is op-z, then op-a: OperationID order is reversed.
		first := submitCodeOperation(t, r, session, "op-z", "first")
		second := submitCodeOperation(t, r, session, "op-a", "second")
		empty := submitCodeOperation(t, r, session, "op-m", "empty")
		firstEntry := first.Admission.AdmittedEntry.EntryID
		secondEntry := second.Admission.AdmittedEntry.EntryID
		emptyEntry := empty.Admission.AdmittedEntry.EntryID
		testassert.Require(t, firstEntry != "" && secondEntry != "" && emptyEntry != "", "admitted entries = (%q, %q, %q), want all present", firstEntry, secondEntry, emptyEntry)

		edited := filepath.Join(workspace, "notes.txt")
		testassert.NoError(t, os.WriteFile(edited, []byte("v0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(r, session, firstEntry), edited, edited, []byte("v1"))
		created := filepath.Join(workspace, "created.txt")
		captureCodeMutation(t, codeGroupRoot(r, session, secondEntry), created, created, []byte("new"))
		testassert.NoError(t, os.MkdirAll(codeGroupRoot(r, session, emptyEntry), 0o700))

		groups, err := r.listSessionCodeSnapshots(context.Background(), session)
		testassert.NoError(t, err)
		testassert.Require(t, len(groups) == 2, "groups = %+v, want exactly the two recorded groups", groups)
		testassert.Require(t, groups[0].OperationId == "op-z" && groups[1].OperationId == "op-a", "group order = (%q, %q), want admission order (op-z, op-a)", groups[0].OperationId, groups[1].OperationId)
		testassert.Require(t, groups[0].GroupItemId == projectItemID(session, firstEntry) && groups[1].GroupItemId == projectItemID(session, secondEntry), "group item ids = (%q, %q), want the admitted-entry projections", groups[0].GroupItemId, groups[1].GroupItemId)
		testassert.Require(t, len(groups[0].Files) == 1 && groups[0].Files[0] == (protocol.SnapshotFile{Path: edited, Existed: true}), "first group files = %+v, want the edited display path with an existing preimage", groups[0].Files)
		testassert.Require(t, len(groups[1].Files) == 1 && groups[1].Files[0] == (protocol.SnapshotFile{Path: created, Existed: false}), "second group files = %+v, want the created display path with an absent preimage", groups[1].Files)

		// A returned group list is a caller copy: mutation cannot affect
		// the next read.
		groups[0].OperationId = "mutated"
		groups[0].Files[0].Path = "mutated"
		reread, err := r.listSessionCodeSnapshots(context.Background(), session)
		testassert.Require(t, err == nil && len(reread) == 2 && reread[0].OperationId == "op-z" && reread[0].Files[0] == (protocol.SnapshotFile{Path: edited, Existed: true}), "re-read groups = (%+v, %v), want the immutable first projection", reread, err)
	})
}

// codeRuntime is the code suite's native owner: the solo agent over the
// local model endpoint, the job-stop seam, and a never-ticked sweep stream
// that keeps the automatic lifecycle sweep structurally out of every row.
type codeRuntime struct {
	t     *testing.T
	store harness.Storage
	r     *Runtime
	e     *ownerEnv
}

func openCodeRuntime(t *testing.T, store harness.Storage) *codeRuntime {
	t.Helper()
	// The fixture declares the shared plain solo/worker pair: this
	// composition provides no hooks or tools, and the rows need only ordinary
	// text turns and manual compaction.
	e := newOwnerEnv(t)
	writeServiceFile(t, agents.PathForConfig(e.configPath), projectionAgentsDocument)
	r, _, err := e.openSwept(context.Background(), e.storagePlugin(store), jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, newLifecycleStopper()))
	testassert.NoError(t, err)
	return &codeRuntime{t: t, store: store, r: r, e: e}
}

func (f *codeRuntime) session(name string) string {
	f.t.Helper()
	rec, err := f.r.createSession(context.Background(), "/tmp/compact-"+name, "solo")
	if err != nil {
		f.t.Fatalf("createSession(%s): %v", name, err)
	}
	return rec.Identity.SessionID
}

// submit submits one regular user message, tolerating the buffered
// dispositions of the terminal-to-retirement window: the durable admission
// is the rendezvous, not the terminal state.
func (f *codeRuntime) submit(sessionID, operationID, text string) {
	f.t.Helper()
	if _, err := submitRawThroughRuntime(context.Background(), f.r, sessionID, operationID, text, harness.MessageModeRegular); err != nil {
		f.t.Fatalf("Submit(%s): %v", operationID, err)
	}
}

// compactIdle admits one manual compaction on an idle Session: the
// terminal-to-retirement window clears asynchronously, and the admission
// itself is the rendezvous on the retired run — a rejection committed
// nothing, so the retry is safe.
func (f *codeRuntime) compactIdle(sessionID, operationID string) harness.OperationRecord {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var rec harness.OperationRecord
		err := f.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			var err error
			rec, err = h.Compact(ctx, harness.CompactRequest{SessionID: sessionID, OperationID: operationID})
			return err
		})
		if err == nil {
			return rec
		}
		if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "is not idle") || time.Now().After(deadline) {
			f.t.Fatalf("Compact(%s): %v", operationID, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// submitMode submits one message in the given mode and returns the
// disposition.
func (f *codeRuntime) submitMode(sessionID, operationID, text string, mode harness.MessageMode) harness.SubmitDisposition {
	f.t.Helper()
	res, err := submitRawThroughRuntime(context.Background(), f.r, sessionID, operationID, text, mode)
	if err != nil {
		f.t.Fatalf("Submit(%s): %v", operationID, err)
	}
	return res.Disposition
}

// TestCodeSnapshotsSteeringDerivesOwnGroup proves a delivered steering input
// derives its own rewind group: the buffered head records no group while it
// waits, and after the boundary handoff its own Operation's admitted entry is
// a full group key — listed beside the predecessor's and restorable to the
// predecessor's boundary — over the multi-boundary compact-lifecycle fixture.
func TestCodeSnapshotsSteeringDerivesOwnGroup(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		f := openCodeRuntime(t, store)
		defer func() { _ = f.r.Close(context.Background()) }()
		session := f.session("steering-group")

		// The first request — the one carrying "start work" — parks on the
		// server hold while its turn is served after release; the steering
		// submit buffers behind it.
		gate := make(chan struct{})
		f.e.server.setHold(gate)
		f.submit(session, "op-1", "start work")
		awaitModelArrival(t, f.e)
		testassert.Equal(t, f.submitMode(session, "op-steer", "steering text", harness.MessageModeRegular), harness.DispositionSteering)
		{
			got := sessionViewThroughRuntime(t, f.r, session)
			testassert.Equal(t, len(got.Operations), 1)
		}
		close(gate)
		awaitOperation(t, f.r, session, "op-1", harness.OperationSuccess)
		awaitOperation(t, f.r, session, "op-steer", harness.OperationSuccess) // the steering head's own Operation
		awaitIdleSession(t, f.r, session)

		steered := readOperation(t, f.r, session, "op-steer").Admission.AdmittedEntry.EntryID
		admitted := readOperation(t, f.r, session, "op-1").Admission.AdmittedEntry.EntryID
		testassert.Require(t, admitted != "" && steered != "" && steered != admitted, "admitted/steering entries = (%q, %q), want two distinct admitted inputs", admitted, steered)

		workspace := filepath.Join(f.r.dataDir, "steering-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		admittedFile := filepath.Join(workspace, "admitted.txt")
		testassert.NoError(t, os.WriteFile(admittedFile, []byte("v0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(f.r, session, admitted), admittedFile, admittedFile, []byte("v1"))
		steeringFile := filepath.Join(workspace, "steering.txt")
		testassert.NoError(t, os.WriteFile(steeringFile, []byte("s0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(f.r, session, steered), steeringFile, steeringFile, []byte("s1"))

		awaitRestorableSession(t, f.r, session)
		groups, err := f.r.listSessionCodeSnapshots(context.Background(), session)
		testassert.NoError(t, err)
		testassert.Require(t, len(groups) == 2, "groups = %+v, want the predecessor's and the steering head's own groups", groups)
		byOperation := map[string]protocol.CodeSnapshotGroup{}
		for _, group := range groups {
			byOperation[group.OperationId] = group
		}
		{
			_, ok := byOperation["op-1"]
			testassert.Require(t, ok, "groups = %+v, want the predecessor's admitted-entry group", groups)
		}
		steerGroup, ok := byOperation["op-steer"]
		testassert.Require(t, ok && steerGroup.GroupItemId == projectItemID(session, steered), "steering group = %+v, want the steering head's own admitted-entry group %q", steerGroup, steered)

		// Restoring to the steering head's boundary leaves the predecessor's
		// group untouched; restoring to the predecessor covers both.
		result, err := f.r.revertSessionCode(context.Background(), session, "op-steer")
		testassert.NoError(t, err)
		testassert.Require(t, len(result.Restored) == 1 && result.Restored[0] == steeringFile, "restored = %v, want only the steering head's file", result.Restored)
		{
			data, err := os.ReadFile(admittedFile)
			testassert.Require(t, err == nil && string(data) == "v1", "admitted-entry file = (%q, %v), want it untouched by the steering head's restore", data, err)
		}
		result, err = f.r.revertSessionCode(context.Background(), session, "op-1")
		testassert.NoError(t, err)
		testassert.Require(t, len(result.Restored) == 1 && result.Restored[0] == admittedFile, "restored to the predecessor = %v, want the predecessor's file (the consumed steering group already restored)", result.Restored)
		{
			data, err := os.ReadFile(steeringFile)
			testassert.Require(t, err == nil && string(data) == "s0", "steering-entry file = (%q, %v), want the earlier restore's preimage retained", data, err)
		}
	})
}

// TestCodeSnapshotsBoundaryAndRevertOrder proves the boundary rule and reverse
// admission restore order: manual compaction is invalid, a message boundary
// with no recorded snapshots still rewinds later groups, and an ID-sorted
// implementation cannot pass the same-file preimage chain.
func TestCodeSnapshotsBoundaryAndRevertOrder(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		f := openCodeRuntime(t, store)
		defer func() { _ = f.r.Close(context.Background()) }()
		session := f.session("revert-order")
		workspace := filepath.Join(f.r.dataDir, "revert-order-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))

		f.submit(session, "op-z", "first")
		awaitOperation(t, f.r, session, "op-z", harness.OperationSuccess)
		awaitIdleSession(t, f.r, session)
		f.submit(session, "op-m", "middle")
		awaitOperation(t, f.r, session, "op-m", harness.OperationSuccess)
		awaitIdleSession(t, f.r, session)
		f.submit(session, "op-a", "last")
		awaitOperation(t, f.r, session, "op-a", harness.OperationSuccess)
		awaitIdleSession(t, f.r, session)
		awaitRestorableSession(t, f.r, session)

		firstEntry := readOperation(t, f.r, session, "op-z").Admission.AdmittedEntry.EntryID
		lastEntry := readOperation(t, f.r, session, "op-a").Admission.AdmittedEntry.EntryID

		// The preimage chain: op-z writes v1, op-a writes v2.
		edited := filepath.Join(workspace, "chain.txt")
		testassert.NoError(t, os.WriteFile(edited, []byte("v0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(f.r, session, firstEntry), edited, edited, []byte("v1"))
		created := filepath.Join(workspace, "created.txt")
		captureCodeMutation(t, codeGroupRoot(f.r, session, lastEntry), edited, edited, []byte("v2"))
		captureCodeMutation(t, codeGroupRoot(f.r, session, lastEntry), created, created, []byte("c"))

		// The middle Operation owns no recorded snapshots but is a valid
		// boundary: op-a still rewinds.
		result, err := f.r.revertSessionCode(context.Background(), session, "op-m")
		testassert.NoError(t, err)
		testassert.Require(t, len(result.Restored) == 2 && slices.Contains(result.Restored, edited) && slices.Contains(result.Restored, created), "restored = %v, want the later group's edited and created files", result.Restored)
		{
			data, err := os.ReadFile(edited)
			testassert.Require(t, err == nil && string(data) == "v1", "chain after boundary op-m = (%q, %v), want op-a's restored preimage", data, err)
		}
		{
			_, err := os.Stat(created)
			testassert.Require(t, errors.Is(err, os.ErrNotExist), "created file stat = %v, want restored absence", err)
		}

		// Re-record op-a, then the earliest admitted boundary op-z rewinds the
		// whole chain to v0: a correct reverse-admission visit restores op-a's
		// preimage v1 first and then op-z's v0. An OperationID-sorted pair
		// addresses only op-z and skips on the v1 identity mismatch.
		captureCodeMutation(t, codeGroupRoot(f.r, session, lastEntry), edited, edited, []byte("v2"))
		result, err = f.r.revertSessionCode(context.Background(), session, "op-z")
		testassert.NoError(t, err)
		{
			data, err := os.ReadFile(edited)
			testassert.Require(t, err == nil && string(data) == "v0", "chain after boundary op-z = (%q, %v, restored %v), want the earliest group's preimage", data, err, result.Restored)
		}

		// A manual-compaction Operation has the zero admitted entry: it is not
		// a valid boundary and no group is opened.
		f.compactIdle(session, "compact-op")
		awaitRestorableSession(t, f.r, session)
		groups, err := f.r.listSessionCodeSnapshots(context.Background(), session)
		testassert.NoError(t, err)
		for _, group := range groups {
			testassert.Require(t, group.OperationId != "compact-op", "compact Operation listed a code group: %+v", group)
		}
		for _, boundary := range []string{"compact-op", "", "op-missing"} {
			{
				_, err := f.r.revertSessionCode(context.Background(), session, boundary)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "revert boundary %q = %v, want harness.ErrInvalid", boundary, err)
			}
		}
		{
			data, err := os.ReadFile(edited)
			testassert.Require(t, err == nil && string(data) == "v0", "chain after refused boundaries = (%q, %v), want no mutation", data, err)
		}
	})
}

// TestCodeSnapshotsSkipOnIdentityChange proves the retained CodeStore proof
// survives restore: changed content and a changed canonical path are skipped
// with their display path, never overwritten.
func TestCodeSnapshotsSkipOnIdentityChange(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "skip-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		op := submitCodeOperation(t, r, session, "op-1", "one")
		entry := op.Admission.AdmittedEntry.EntryID

		changed := filepath.Join(workspace, "changed.txt")
		testassert.NoError(t, os.WriteFile(changed, []byte("v0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(r, session, entry), changed, changed, []byte("v1"))
		testassert.NoError(t, os.WriteFile(changed, []byte("external"), 0o600))

		moved := filepath.Join(workspace, "moved.txt")
		testassert.NoError(t, os.WriteFile(moved, []byte("m0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(r, session, entry), moved, moved, []byte("m1"))
		testassert.NoError(t, os.Remove(moved))
		elsewhere := filepath.Join(workspace, "elsewhere.txt")
		testassert.NoError(t, os.WriteFile(elsewhere, []byte("elsewhere"), 0o600))
		testassert.NoError(t, os.Symlink(elsewhere, moved))

		awaitRestorableSession(t, r, session)
		result, err := r.revertSessionCode(context.Background(), session, "op-1")
		testassert.NoError(t, err)
		testassert.Require(t, len(result.Restored) == 0 && len(result.Skipped) == 2, "result = %+v, want both files skipped", result)
		reasons := map[string]string{}
		for _, skip := range result.Skipped {
			reasons[skip.Path] = skip.Reason
		}
		if !strings.Contains(reasons[changed], "content changed") {
			t.Fatalf("changed skip = %q, want the content-identity reason", reasons[changed])
		}
		if !strings.Contains(reasons[moved], "canonical path changed") {
			t.Fatalf("moved skip = %q, want the canonical-identity reason", reasons[moved])
		}
		{
			data, err := os.ReadFile(changed)
			testassert.Require(t, err == nil && string(data) == "external", "changed file = (%q, %v), want the external content untouched", data, err)
		}
		{
			link, err := os.Readlink(moved)
			testassert.Require(t, err == nil && link == elsewhere, "moved file = (%q, %v), want the symlink untouched", link, err)
		}
	})
}

// TestCodeSnapshotsForkAndChildIndependence proves each Session owns its own
// group namespace: reverting a parent touches neither its fork's nor its
// child's recorded files, and each lineage lists only its own group.
func TestCodeSnapshotsForkAndChildIndependence(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "lineage-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		root := projectionSession(t, r, workspace, "solo").Identity.SessionID
		rootOp := submitCodeOperation(t, r, root, "op-root", "root")
		rootEntry := rootOp.Admission.AdmittedEntry.EntryID
		rootFile := filepath.Join(workspace, "root.txt")
		testassert.NoError(t, os.WriteFile(rootFile, []byte("r0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(r, root, rootEntry), rootFile, rootFile, []byte("r1"))

		awaitOperation(t, r, root, "op-root", harness.OperationSuccess) // the settled root execution retires before Fork requires its idle source
		forked := forkThroughRuntime(t, r, root, rootEntry, "op-fork")
		awaitOperation(t, r, forked, "op-fork", harness.OperationSuccess)
		forkOp := readOperation(t, r, forked, "op-fork")
		forkEntry := forkOp.Admission.AdmittedEntry.EntryID
		forkFile := filepath.Join(workspace, "fork.txt")
		testassert.NoError(t, os.WriteFile(forkFile, []byte("f0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(r, forked, forkEntry), forkFile, forkFile, []byte("f1"))

		child := launchChildThroughRuntime(t, r, root, "op-child")
		awaitOperation(t, r, child, "op-child", harness.OperationSuccess)
		childOp := readOperation(t, r, child, "op-child")
		childEntry := childOp.Admission.AdmittedEntry.EntryID
		childFile := filepath.Join(workspace, "child.txt")
		testassert.NoError(t, os.WriteFile(childFile, []byte("c0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(r, child, childEntry), childFile, childFile, []byte("c1"))

		awaitRestorableSession(t, r, root)
		result, err := r.revertSessionCode(context.Background(), root, "op-root")
		testassert.NoError(t, err)
		testassert.Require(t, len(result.Restored) == 1 && result.Restored[0] == rootFile, "root restored = %v, want only the root file", result.Restored)
		{
			data, err := os.ReadFile(rootFile)
			testassert.Require(t, err == nil && string(data) == "r0", "root file = (%q, %v), want the restored preimage", data, err)
		}
		{
			data, err := os.ReadFile(forkFile)
			testassert.Require(t, err == nil && string(data) == "f1", "fork file = (%q, %v), want it untouched", data, err)
		}
		{
			data, err := os.ReadFile(childFile)
			testassert.Require(t, err == nil && string(data) == "c1", "child file = (%q, %v), want it untouched", data, err)
		}
		for _, tc := range []struct{ session, entry, operation string }{
			{forked, forkEntry, "op-fork"},
			{child, childEntry, "op-child"},
		} {
			groups, err := r.listSessionCodeSnapshots(context.Background(), tc.session)
			testassert.Require(t, err == nil, "list %s: %v", tc.operation, err)
			testassert.Require(t, len(groups) == 1 && groups[0].OperationId == tc.operation && groups[0].GroupItemId == projectItemID(tc.session, tc.entry), "%s groups = %+v, want the Session's own group", tc.operation, groups)
		}
	})
}

// TestCodeRevertGateAxes proves the one restore state gate over every
// semantic axis independently: idle passes; ExecutionBusy (running, reserved
// or retiring), either pending FIFO, and a live background member each refuse
// with the conflict class. The separate FIFO axes are the capture rule's own
// observation; the gate consumes their single scalar fact.
func TestCodeRevertGateAxes(t *testing.T) {
	const sessionID = "0123456789abcdef0123456789abcdef"
	base := harness.HistoryReadView{}
	cases := []struct {
		name    string
		mutate  func(*harness.HistoryReadView)
		refused bool
	}{
		{"idle", nil, false},
		{"busy-running-or-reserved-or-retiring", func(v *harness.HistoryReadView) { v.ExecutionBusy = true }, true},
		{"pending-either-fifo", func(v *harness.HistoryReadView) {
			v.HasPending = true
		}, true},
		{"live-background-member", func(v *harness.HistoryReadView) {
			v.HasBackground = true
		}, true},
	}
	for _, tc := range cases {
		snap := base
		if tc.mutate != nil {
			tc.mutate(&snap)
		}
		err := codeRestoreRefusal(sessionID, snap)
		if tc.refused {
			testassert.Require(t, errors.Is(err, harness.ErrConflict), "%s gate = %v, want harness.ErrConflict", tc.name, err)
			continue
		}
		testassert.Require(t, err == nil, "%s gate = %v, want nil", tc.name, err)
	}
}

// TestCodeSnapshotsStateGates proves the two observable state refusals on a
// real Runtime: a running Operation and a live background child both refuse
// restore before any group is opened, and the idle state immediately after
// admits it.
func TestCodeSnapshotsStateGates(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "gates-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID

		gate := make(chan struct{})
		e.server.setHold(gate)
		submitThroughRuntime(t, r, session, "op-1", "running")
		awaitModelArrival(t, e)
		runningEntry := readOperation(t, r, session, "op-1").Admission.AdmittedEntry.EntryID
		file := filepath.Join(workspace, "gated.txt")
		testassert.NoError(t, os.WriteFile(file, []byte("v0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(r, session, runningEntry), file, file, []byte("v1"))

		{
			_, err := r.revertSessionCode(context.Background(), session, "op-1")
			testassert.Require(t, errors.Is(err, harness.ErrConflict), "running restore = %v, want harness.ErrConflict", err)
		}
		{
			data, err := os.ReadFile(file)
			testassert.Require(t, err == nil && string(data) == "v1", "running refusal mutated the file: (%q, %v)", data, err)
		}
		close(gate)
		awaitOperation(t, r, session, "op-1", harness.OperationSuccess)
		awaitRestorableSession(t, r, session)
		result, err := r.revertSessionCode(context.Background(), session, "op-1")
		testassert.Require(t, err == nil && len(result.Restored) == 1, "idle restore = (%+v, %v), want the restored file", result, err)

		// A live background child refuses the parent's restore even while the
		// parent's own Operation state is idle.
		parent := projectionSession(t, r, workspace, "solo").Identity.SessionID
		parentOp := submitCodeOperation(t, r, parent, "op-parent", "parent")
		parentEntry := parentOp.Admission.AdmittedEntry.EntryID
		parentFile := filepath.Join(workspace, "parent.txt")
		testassert.NoError(t, os.WriteFile(parentFile, []byte("p0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(r, parent, parentEntry), parentFile, parentFile, []byte("p1"))
		awaitOperation(t, r, parent, "op-parent", harness.OperationSuccess) // the parent's own execution retires before the child launch
		childGate := make(chan struct{})
		e.server.setHold(childGate)
		child := launchChildThroughRuntime(t, r, parent, "op-live-child")
		awaitModelArrival(t, e)
		{
			children := sessionViewThroughRuntime(t, r, parent).Background
			testassert.Require(t, len(children) != 0, "parent background membership is empty after a live child launch")
		}
		{
			_, err := r.revertSessionCode(context.Background(), parent, "op-parent")
			testassert.Require(t, errors.Is(err, harness.ErrConflict), "live-child restore = %v, want harness.ErrConflict", err)
		}
		{
			data, err := os.ReadFile(parentFile)
			testassert.Require(t, err == nil && string(data) == "p1", "live-child refusal mutated the file: (%q, %v)", data, err)
		}
		close(childGate)
		awaitOperation(t, r, child, "op-live-child", harness.OperationSuccess)
	})
}

// --- concurrent admission and partial traversal ---

// TestCodeRevertPartialTraversalParkedGroup is the partial-traversal fixture:
// a restore parked on a test-owned FIFO saved original completes its parked
// group's restore, and the later-visited bad group returns the partial
// accumulated result with the internal error class — restored content, a
// non-nil skip list, and no target Session id on the error. No production
// hook or CodeStore change backs the fixture.
func TestCodeRevertPartialTraversalParkedGroup(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "fifo-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		first := submitCodeOperation(t, r, session, "op-first", "first")
		second := submitCodeOperation(t, r, session, "op-second", "second")
		awaitRestorableSession(t, r, session)

		// The later-visited (earlier-admitted) group fails on a corrupt meta
		// after the earlier-visited (later-admitted) group restores.
		badEntryDir := filepath.Join(codeGroupRoot(r, session, first.Admission.AdmittedEntry.EntryID), "snapshots", "1", "aaaaaaaaaaaaaaaa")
		testassert.NoError(t, os.MkdirAll(badEntryDir, 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(badEntryDir, "meta.json"), []byte("{"), 0o600))

		target := filepath.Join(workspace, "parked.txt")
		testassert.NoError(t, os.WriteFile(target, []byte("current"), 0o600))
		fifo := seedFIFOCodeEntry(t, codeGroupRoot(r, session, second.Admission.AdmittedEntry.EntryID), target, "current")

		park, outcomes := parkRestore(t, r, session, "op-first", fifo)
		defer park.flush()

		park.release("restored")

		var out restoreOutcome
		select {
		case out = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("the restore never returned after its parked group completed")
		}
		testassert.Require(t, out.err != nil && len(out.result.Restored) == 1 && out.result.Restored[0] == target && out.result.Skipped != nil && out.result.Error != nil && out.result.Error.Code == protocol.Internal && out.result.Error.Message != "" && out.result.Error.SessionId == nil, "restore = %+v, want the later bad group's traversal error; partial result = %+v, want the parked group's accumulated restore and a non-nil skip list; partial error = %+v, want the internal class with no target Session id", out.result, out.result, out.result.Error)
		{
			data, err := os.ReadFile(target)
			testassert.Require(t, err == nil && string(data) == "restored", "parked target = (%q, %v), want the FIFO restore to complete", data, err)
		}
	})
}

// seedFIFOCodeEntry writes one valid modern snapshot entry whose saved
// original is a FIFO, and returns the FIFO path. The canonical path and
// last-write hash mirror the CodeStore's own proof so the retained restore
// reaches restoreFile's plain os.Open.
func seedFIFOCodeEntry(t *testing.T, groupRoot, target, content string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(target)
	testassert.Require(t, err == nil, "EvalSymlinks(%s): %v", target, err)
	entryDir := filepath.Join(groupRoot, "snapshots", "1", sha256Hex16(canonical))
	testassert.NoError(t, os.MkdirAll(entryDir, 0o700))
	fifo := filepath.Join(entryDir, "original")
	{
		err := unix.Mkfifo(fifo, 0o600)
		testassert.Require(t, err == nil, "Mkfifo(%s): %v", fifo, err)
	}
	meta := map[string]any{
		"original_path":  target,
		"canonical_path": canonical,
		"existed":        true,
		"last_write":     map[string]any{"hash": sha256Hex([]byte(content))},
	}
	writeRetainedJSON(t, filepath.Join(entryDir, "meta.json"), meta)
	return fifo
}

// --- artifact interval oracles ---

// restoreOutcome carries one revertSessionCode result across a goroutine.
type restoreOutcome struct {
	result protocol.CodeRevertResult
	err    error
}

// fifoPark holds the writer side of one parked restore: the writer's
// nonblocking open succeeds only once the restore's reader is parked inside
// restoreFile, and release completes that restore's admitted traversal.
type fifoPark struct {
	writerFD int
	fifo     string
}

// parkFIFO starts no restore of its own: it returns the parking writer once
// any restore's reader is parked on the FIFO saved original. The caller
// defers flush so a failing test releases the parked reader before the
// owner's shutdown joins the admitted restores.
func parkFIFO(t *testing.T, fifo string) *fifoPark {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			return &fifoPark{writerFD: fd, fifo: fifo}
		}
		testassert.Require(t, errors.Is(err, unix.ENXIO), "FIFO writer open: %v", err)
		testassert.Require(t, !time.Now().After(deadline), "the restore never parked on the FIFO saved original")
		time.Sleep(5 * time.Millisecond)
	}
}

// parkRestore starts one restore in the background and returns its parking
// writer once the restore is parked on the FIFO saved original. The caller
// defers flush so a failing test releases every parked reader before the
// owner's shutdown joins the admitted restores.
func parkRestore(t *testing.T, r *Runtime, sessionID, boundary, fifo string) (*fifoPark, <-chan restoreOutcome) {
	t.Helper()
	outcomes := make(chan restoreOutcome, 1)
	go func() {
		result, err := r.revertSessionCode(context.Background(), sessionID, boundary)
		outcomes <- restoreOutcome{result: result, err: err}
	}()
	return parkFIFO(t, fifo), outcomes
}

// release completes the parked restore: the FIFO bytes become the restored
// content of its saved original, and closing the held writer ends every
// connected reader.
func (p *fifoPark) release(content string) {
	if p.writerFD < 0 {
		return
	}
	_, _ = unix.Write(p.writerFD, []byte(content))
	_ = unix.Close(p.writerFD)
	p.writerFD = -1
}

// flush tears a failing test's parked reader down: closing the held writer
// gives every connected FIFO reader an end-of-file, so the admitted restores
// finish and the owner's shutdown can join them.
func (p *fifoPark) flush() {
	p.release("released")
}

// TestCodeRevertCrossGroupSkipLedger proves the whole-restore traversal
// ledger: the newest group's externally-changed identity is skipped once, and
// the older group's snapshot of the same canonical identity — recorded through
// a display alias — is never restored, so the external content survives.
func TestCodeRevertCrossGroupSkipLedger(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "ledger-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		first := submitCodeOperation(t, r, session, "op-first", "first")
		second := submitCodeOperation(t, r, session, "op-second", "second")
		awaitRestorableSession(t, r, session)

		file := filepath.Join(workspace, "chain.txt")
		testassert.NoError(t, os.WriteFile(file, []byte("v0"), 0o600))
		// The older group records the file through a display alias: an
		// absolute symlink resolving to the same canonical identity.
		alias := filepath.Join(workspace, "alias.txt")
		testassert.NoError(t, os.Symlink(file, alias))
		captureCodeMutation(t, codeGroupRoot(r, session, first.Admission.AdmittedEntry.EntryID), alias, file, []byte("v1"))
		captureCodeMutation(t, codeGroupRoot(r, session, second.Admission.AdmittedEntry.EntryID), file, file, []byte("v2"))
		// The external edit matches the older group's last-write identity, so
		// only a shared ledger keeps the older group from restoring.
		testassert.NoError(t, os.WriteFile(file, []byte("v1"), 0o600))

		result, err := r.revertSessionCode(context.Background(), session, "op-first")
		testassert.NoError(t, err)
		testassert.Require(t, len(result.Restored) == 0 && len(result.Skipped) == 1 && result.Skipped[0].Path == file, "restored = %v, want no restore after the newer group's skip; skipped = %+v, want exactly one reported skip for %s", result.Restored, result.Skipped, file)
		data, err := os.ReadFile(file)
		testassert.Require(t, err == nil && string(data) == "v1", "file = (%q, %v), want the external v1 preserved", data, err)
	})
}

// TestCodeRewindConflictWhileParked proves two concurrent rewinds of one
// Session conflict: the parked restore owns the whole artifact interval, so
// the second rewind refuses before any directory work and mutates nothing.
func TestCodeRewindConflictWhileParked(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "rewind-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		first := submitCodeOperation(t, r, session, "op-first", "first")
		second := submitCodeOperation(t, r, session, "op-second", "second")
		awaitRestorableSession(t, r, session)

		keep := filepath.Join(workspace, "keep.txt")
		testassert.NoError(t, os.WriteFile(keep, []byte("v0"), 0o600))
		captureCodeMutation(t, codeGroupRoot(r, session, first.Admission.AdmittedEntry.EntryID), keep, keep, []byte("v1"))
		rewind := filepath.Join(workspace, "rewind.txt")
		testassert.NoError(t, os.WriteFile(rewind, []byte("v2"), 0o600))
		fifo := seedFIFOCodeEntry(t, codeGroupRoot(r, session, second.Admission.AdmittedEntry.EntryID), rewind, "v2")

		park, outcomes := parkRestore(t, r, session, "op-first", fifo)

		defer park.flush()

		rewound := make(chan restoreOutcome, 1)
		go func() {
			result, err := r.revertSessionCode(context.Background(), session, "op-first")
			rewound <- restoreOutcome{result: result, err: err}
		}()
		var retry restoreOutcome
		select {
		case retry = <-rewound:
		case <-time.After(5 * time.Second):
			t.Fatal("the second rewind waited behind the parked restore instead of conflicting")
		}
		testassert.Require(t, errors.Is(retry.err, harness.ErrConflict), "second rewind = (%+v, %v), want the conflict class with zero mutation", retry.result, retry.err)
		{
			data, err := os.ReadFile(keep)
			testassert.Require(t, err == nil && string(data) == "v1", "keep = (%q, %v), want it untouched by the refused rewind", data, err)
		}

		park.release("restored")
		var parked restoreOutcome
		select {
		case parked = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("the parked restore never returned after its FIFO completed")
		}
		testassert.Require(t, parked.err == nil && len(parked.result.Restored) == 2, "parked restore = %v; restored = %v, want both groups' preimages", parked.err, parked.result.Restored)
		{
			data, err := os.ReadFile(keep)
			testassert.Require(t, err == nil && string(data) == "v0", "keep = (%q, %v), want the restored preimage v0", data, err)
		}
	})
}

// TestDeleteSessionConflictsWithRestoreInterval proves a committed deletion
// cannot destroy protected state: while a restore owns the artifact interval
// the deletion refuses, the archived Session and its artifacts stay intact,
// and the same deletion succeeds once the restore released the interval.
func TestDeleteSessionConflictsWithRestoreInterval(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "delete-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		op := submitCodeOperation(t, r, session, "op-1", "only")
		awaitRestorableSession(t, r, session)
		file := filepath.Join(workspace, "deleted.txt")
		testassert.NoError(t, os.WriteFile(file, []byte("v1"), 0o600))
		groupRoot := codeGroupRoot(r, session, op.Admission.AdmittedEntry.EntryID)
		fifo := seedFIFOCodeEntry(t, groupRoot, file, "v1")
		{
			_, err := r.archiveSession(context.Background(), session)
			testassert.NoError(t, err)
		}

		park, outcomes := parkRestore(t, r, session, "op-1", fifo)

		defer park.flush()
		{
			err := r.deleteSession(context.Background(), session)
			testassert.Require(t, errors.Is(err, harness.ErrConflict), "delete during a parked restore = %v, want the conflict class", err)
		}
		{
			header := headerThroughRuntime(t, r, session)
			testassert.Equal(t, header.Lifecycle, harness.LifecycleArchived)
		}
		{
			_, err := os.Stat(groupRoot)
			testassert.NoError(t, err)
		}

		park.release("restored")
		var parked restoreOutcome
		select {
		case parked = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("the parked restore never returned after its FIFO completed")
		}
		testassert.Require(t, parked.err == nil, "parked restore = %v", parked.err)
		// The interval, not the Session, blocked the deletion.
		testassert.NoError(t, r.deleteSession(context.Background(), session))
		{
			_, err := os.Stat(groupRoot)
			testassert.Require(t, os.IsNotExist(err), "group root after the deletion = %v, want it removed", err)
		}
	})
}

// TestCodeRestoreIntervalKeepsMetadataTransitionsAvailable proves the
// metadata-only transitions stay available inside a restore interval: they
// acquire no artifact token, so the agent-type change, the archive and the
// reopen all succeed while the restore is parked, and the restore still
// completes with its own result.
func TestCodeRestoreIntervalKeepsMetadataTransitionsAvailable(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "meta-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		op := submitCodeOperation(t, r, session, "op-1", "only")
		awaitRestorableSession(t, r, session)

		keep := filepath.Join(workspace, "keep.txt")
		testassert.NoError(t, os.WriteFile(keep, []byte("v0"), 0o600))
		group := codeGroupRoot(r, session, op.Admission.AdmittedEntry.EntryID)
		captureCodeMutation(t, group, keep, keep, []byte("v1"))
		parked := filepath.Join(workspace, "parked.txt")
		testassert.NoError(t, os.WriteFile(parked, []byte("v1"), 0o600))
		fifo := seedFIFOCodeEntry(t, group, parked, "v1")

		park, outcomes := parkRestore(t, r, session, "op-1", fifo)

		defer park.flush()

		changed, err := r.setSessionAgentType(context.Background(), session, protocol.SetSessionAgentTypeRequest{AgentType: "worker"})
		testassert.NoError(t, err)
		testassert.Equal(t, changed.AgentType, "worker")
		archived, err := r.archiveSession(context.Background(), session)
		testassert.NoError(t, err)
		testassert.Equal(t, archived.Lifecycle, protocol.Archived)
		reopened, err := r.reopenSession(context.Background(), session)
		testassert.NoError(t, err)
		testassert.Require(t, reopened.Lifecycle == protocol.Open && reopened.AgentType == "worker", "reopened header = %+v, want open with the changed selection", reopened)

		park.release("restored")
		var out restoreOutcome
		select {
		case out = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("the parked restore never returned after its FIFO completed")
		}
		testassert.Require(t, out.err == nil && len(out.result.Restored) == 2, "parked restore = %v; restored = %v, want both group entries", out.err, out.result.Restored)
		{
			data, err := os.ReadFile(keep)
			testassert.Require(t, err == nil && string(data) == "v0", "keep = (%q, %v), want the restored preimage v0", data, err)
		}
		{
			data, err := os.ReadFile(parked)
			testassert.Require(t, err == nil && string(data) == "restored", "parked = (%q, %v), want the FIFO restore to complete", data, err)
		}
	})
}

// --- retained pre-cutover rows ---

const (
	retainedConversationCanary = "conversation-must-stay-untouched"
	retainedClaimCanary        = "claim-must-stay-untouched"
)

// seedRetainedSession writes one valid legacy session meta plus conversation
// and claim canaries, returning the session directory.
func seedRetainedSession(t *testing.T, home, workspace, legacyID string) string {
	t.Helper()
	normalized := filepath.Clean(workspace)
	sum := sha256.Sum256([]byte(normalized))
	hash := hex.EncodeToString(sum[:])
	dir := filepath.Join(home, ".lightcode", "projects", "p-"+hash, "sessions", legacyID)
	// The fixture seeds the three proof fields and ordinary extra metadata;
	// the reader interprets only the proof fields.
	writeRetainedJSON(t, filepath.Join(dir, "meta.json"), map[string]any{
		"id":           legacyID,
		"created_at":   "2026-01-01T00:00:00Z",
		"project_path": normalized,
		"project_hash": hash[:16],
		"state":        "active",
	})
	writeServiceFile(t, filepath.Join(dir, "turns", "1", "messages.jsonl"), retainedConversationCanary)
	writeServiceFile(t, filepath.Join(dir, "claims", "claim.json"), retainedClaimCanary)
	return dir
}

func writeRetainedJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	testassert.Require(t, err == nil, "marshal %s: %v", path, err)
	writeServiceFile(t, path, string(data))
}

func rewriteRetainedMeta(t *testing.T, sessionDir string, edit func(map[string]any)) {
	t.Helper()
	path := filepath.Join(sessionDir, "meta.json")
	data, err := os.ReadFile(path)
	testassert.NoError(t, err)
	var meta map[string]any
	testassert.NoError(t, json.Unmarshal(data, &meta))
	edit(meta)
	writeRetainedJSON(t, path, meta)
}

// recordRetainedMutation drives one real CodeStore capture at the given turn
// of a pruned legacy session directory.
func recordRetainedMutation(t *testing.T, sessionDir string, turn int, display, target string, content []byte) {
	t.Helper()
	store, err := snapshot.OpenCodeStore(sessionDir)
	testassert.Require(t, err == nil, "OpenCodeStore(%s): %v", sessionDir, err)
	entryID, _, err := store.SnapshotResolvedEntry(turn, display, target)
	testassert.Require(t, err == nil, "SnapshotResolvedEntry turn %d: %v", turn, err)
	{
		err := os.WriteFile(target, content, 0o600)
		testassert.Require(t, err == nil, "WriteFile(%s): %v", target, err)
	}
	{
		err := store.RecordSnapshotContent(turn, entryID, content)
		testassert.Require(t, err == nil, "RecordSnapshotContent turn %d: %v", turn, err)
	}
	store.RetainSnapshotEntry(turn, entryID)
}

// assertCanariesUntouched verifies the hazardous conversation and claim files
// stayed byte-identical.
func assertCanariesUntouched(t *testing.T, sessionDir string) {
	t.Helper()
	for path, want := range map[string]string{
		filepath.Join(sessionDir, "turns", "1", "messages.jsonl"): retainedConversationCanary,
		filepath.Join(sessionDir, "claims", "claim.json"):         retainedClaimCanary,
	} {
		data, err := os.ReadFile(path)
		testassert.Require(t, err == nil && string(data) == want, "canary %s = (%q, %v), want untouched %q", path, data, err, want)
	}
}

// TestRetainedCodeSnapshotsProof proves the retained path proof and its
// refusals: exact 8-hex IDs, normalized workspace, full-hash project
// directory, and the meta ID/ProjectPath/hash-prefix triple; every mismatch
// fails before any snapshot read, while a valid proof with absent snapshots is
// empty success.
func TestRetainedCodeSnapshotsProof(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "legacy-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		legacyID := "deadbeef"
		home := r.config.loader.Home()
		sessionDir := seedRetainedSession(t, home, workspace, legacyID)

		turns, err := r.listRetainedCodeSnapshots(context.Background(), workspace, legacyID)
		testassert.Require(t, err == nil && turns.Turns != nil && len(turns.Turns) == 0, "empty retained list = (%+v, %v), want a non-nil empty list", turns, err)
		result, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID, AfterTurn: 3})
		testassert.Require(t, err == nil && result.Restored != nil && result.Skipped != nil && len(result.Restored) == 0 && len(result.Skipped) == 0, "empty retained revert = (%+v, %v), want non-nil empty success", result, err)

		// The workspace proof is lexical: a non-normalized spelling of the
		// same root normalizes to the same storage key and meta proof, while
		// a symlinked spelling is a different key and is never resolved.
		{
			turns, err := r.listRetainedCodeSnapshots(context.Background(), workspace+string(filepath.Separator)+".", legacyID)
			testassert.Require(t, err == nil && len(turns.Turns) == 0, "non-normalized workspace spelling = (%+v, %v), want the proven empty success", turns, err)
		}
		symlink := filepath.Join(e.home, "legacy-ws-link")
		testassert.NoError(t, os.Symlink(workspace, symlink))
		{
			_, err := r.listRetainedCodeSnapshots(context.Background(), symlink, legacyID)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "symlinked workspace spelling = %v, want harness.ErrNotFound (lexical proof only)", err)
		}

		{
			_, err := r.listRetainedCodeSnapshots(context.Background(), "", legacyID)
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "empty workspace = %v, want harness.ErrInvalid", err)
		}
		for _, id := range []string{"deadbee", "deadbeef0", "DEADBEEF", "deadbeez"} {
			{
				_, err := r.listRetainedCodeSnapshots(context.Background(), workspace, id)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "legacy id %q = %v, want harness.ErrInvalid", id, err)
			}
		}
		{
			_, err := r.listRetainedCodeSnapshots(context.Background(), workspace, "0123456789abcdef0123456789abcdef")
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "32-hex target id into retained route = %v, want harness.ErrInvalid", err)
		}
		{
			_, err := r.listRetainedCodeSnapshots(context.Background(), workspace, "abcdef01")
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "missing legacy session = %v, want harness.ErrNotFound", err)
		}
		{
			_, err := r.listSessionCodeSnapshots(context.Background(), legacyID)
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "8-hex id into target route = %v, want harness.ErrInvalid", err)
		}

		for _, tc := range []struct {
			name string
			edit func(map[string]any)
		}{
			{"wrong id", func(m map[string]any) { m["id"] = "deadbeee" }},
			{"wrong project path", func(m map[string]any) { m["project_path"] = filepath.Join(workspace, "other") }},
			{"wrong hash prefix", func(m map[string]any) { m["project_hash"] = "0000000000000000" }},
		} {
			metaPath := filepath.Join(sessionDir, "meta.json")
			original, err := os.ReadFile(metaPath)
			testassert.NoError(t, err)
			rewriteRetainedMeta(t, sessionDir, tc.edit)
			{
				_, err := r.listRetainedCodeSnapshots(context.Background(), workspace, legacyID)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "%s = %v, want harness.ErrInvalid", tc.name, err)
			}
			{
				_, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID})
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "%s revert = %v, want harness.ErrInvalid", tc.name, err)
			}
			writeServiceFile(t, metaPath, string(original))
		}
		{
			_, err := os.Stat(sessionDir)
			testassert.NoError(t, err)
		}
		assertCanariesUntouched(t, sessionDir)

		// A large ignored legacy field is still one valid JSON meta document:
		// the proof reads the complete regular file and ignores unknown
		// members, so list and revert succeed with empty snapshots. The
		// malformed sibling stays refused.
		metaPath := filepath.Join(sessionDir, "meta.json")
		original, err := os.ReadFile(metaPath)
		testassert.NoError(t, err)
		normalized := filepath.Clean(workspace)
		sum := sha256.Sum256([]byte(normalized))
		hash := hex.EncodeToString(sum[:])
		large, err := json.Marshal(map[string]any{
			"id":                   legacyID,
			"created_at":           "2026-01-01T00:00:00Z",
			"project_path":         normalized,
			"project_hash":         hash[:16],
			"state":                "active",
			"legacy_ignored_field": strings.Repeat("x", (1<<20)+1),
		})
		testassert.NoError(t, err)
		writeServiceFile(t, metaPath, string(large))
		{
			turns, err := r.listRetainedCodeSnapshots(context.Background(), workspace, legacyID)
			testassert.Require(t, err == nil && len(turns.Turns) == 0, "large ignored meta field list = (%+v, %v), want the proven empty success", turns, err)
		}
		{
			result, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID})
			testassert.Require(t, err == nil && len(result.Restored) == 0, "large ignored meta field revert = (%+v, %v), want the proven empty success", result, err)
		}
		writeServiceFile(t, metaPath, "{ not a json document")
		{
			_, err := r.listRetainedCodeSnapshots(context.Background(), workspace, legacyID)
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "malformed meta list = %v, want harness.ErrInvalid", err)
		}
		{
			_, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "malformed meta revert = %v, want harness.ErrInvalid", err)
		}
		writeServiceFile(t, metaPath, string(original))

		// The full-hash directory is exact: a same-ID session under a wrong
		// project directory is not found.
		wrongRoot := filepath.Join(e.home, "other-ws")
		{
			_, err := r.listRetainedCodeSnapshots(context.Background(), wrongRoot, legacyID)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "wrong workspace root = %v, want harness.ErrNotFound", err)
		}
	})
}

// TestRetainedCodeSnapshotsListAndRevert proves the retained list and restore
// over real CodeStore captures: turn order and display paths, after_turn used
// directly, created-file absence, skip-on-identity, and the untouched
// conversation/claim canaries.
func TestRetainedCodeSnapshotsListAndRevert(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "legacy-list-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		legacyID := "cafebabe"
		home := r.config.loader.Home()
		sessionDir := seedRetainedSession(t, home, workspace, legacyID)

		file := filepath.Join(workspace, "legacy.txt")
		testassert.NoError(t, os.WriteFile(file, []byte("a0"), 0o600))
		recordRetainedMutation(t, sessionDir, 1, file, file, []byte("a1"))
		created := filepath.Join(workspace, "legacy-created.txt")
		recordRetainedMutation(t, sessionDir, 2, file, file, []byte("a2"))
		recordRetainedMutation(t, sessionDir, 2, created, created, []byte("c"))

		turns, err := r.listRetainedCodeSnapshots(context.Background(), workspace, legacyID)
		testassert.NoError(t, err)
		testassert.Require(t, len(turns.Turns) == 2 && turns.Turns[0].Turn == 1 && turns.Turns[1].Turn == 2, "turns = %+v, want the retained order 1, 2", turns.Turns)
		testassert.Require(t, len(turns.Turns[0].Files) == 1 && turns.Turns[0].Files[0] == (protocol.SnapshotFile{Path: file, Existed: true}), "turn 1 files = %+v, want the edited display path", turns.Turns[0].Files)
		testassert.Require(t, len(turns.Turns[1].Files) == 2, "turn 2 files = %+v, want the edited and created entries", turns.Turns[1].Files)
		assertCanariesUntouched(t, sessionDir)

		result, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID, AfterTurn: 1})
		testassert.NoError(t, err)
		testassert.Require(t, len(result.Restored) == 2 && result.Skipped != nil && len(result.Skipped) == 0, "turn 2 result = %+v, want both files restored without skips", result)
		{
			data, err := os.ReadFile(file)
			testassert.Require(t, err == nil && string(data) == "a1", "legacy file after turn 2 revert = (%q, %v), want the turn 1 post-write", data, err)
		}
		{
			_, err := os.Stat(created)
			testassert.Require(t, errors.Is(err, os.ErrNotExist), "created file stat = %v, want restored absence", err)
		}

		result, err = r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID})
		testassert.NoError(t, err)
		testassert.Require(t, len(result.Restored) == 1 && result.Restored[0] == file, "turn 1 result = %+v, want the edited file restored", result)
		{
			data, err := os.ReadFile(file)
			testassert.Require(t, err == nil && string(data) == "a0", "legacy file after turn 1 revert = (%q, %v), want the turn 1 preimage", data, err)
		}
		assertCanariesUntouched(t, sessionDir)

		// A re-recorded turn 1 with an external edit skips instead of
		// overwriting, still reporting the retained display path.
		recordRetainedMutation(t, sessionDir, 1, file, file, []byte("a1"))
		testassert.NoError(t, os.WriteFile(file, []byte("external"), 0o600))
		result, err = r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID})
		testassert.NoError(t, err)
		testassert.Require(t, len(result.Restored) == 0 && len(result.Skipped) == 1 && result.Skipped[0].Path == file && strings.Contains(result.Skipped[0].Reason, "content changed"), "skip result = %+v, want the changed display path skipped", result)
		{
			data, err := os.ReadFile(file)
			testassert.Require(t, err == nil && string(data) == "external", "legacy file after skip = (%q, %v), want the external content", data, err)
		}
	})
}

// TestRetainedCodeSnapshotsPartialTraversal proves a later-visited corrupt
// turn returns the accumulated earlier turn result with the internal error
// class and no legacy 8-hex ID in the generated target Session field.
func TestRetainedCodeSnapshotsPartialTraversal(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "legacy-partial-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		legacyID := "feedface"
		home := r.config.loader.Home()
		sessionDir := seedRetainedSession(t, home, workspace, legacyID)

		file := filepath.Join(workspace, "partial.txt")
		testassert.NoError(t, os.WriteFile(file, []byte("p0"), 0o600))
		recordRetainedMutation(t, sessionDir, 1, file, file, []byte("p1"))
		recordRetainedMutation(t, sessionDir, 2, file, file, []byte("p2"))

		// Corrupt turn 1's meta after recording: turn 2 restores first, then
		// turn 1 errors.
		turnDir := filepath.Join(sessionDir, "snapshots", "1")
		entries, err := os.ReadDir(turnDir)
		testassert.Require(t, err == nil && len(entries) == 1, "turn 1 dirs = (%d, %v), want one entry", len(entries), err)
		testassert.NoError(t, os.WriteFile(filepath.Join(turnDir, entries[0].Name(), "meta.json"), []byte("{"), 0o600))

		result, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID})
		testassert.Require(t, err != nil, "partial revert = %+v, want the traversal error", result)
		testassert.Require(t, len(result.Restored) == 1 && result.Restored[0] == file && result.Skipped != nil && result.Error != nil && result.Error.Code == protocol.Internal && result.Error.Message != "" && result.Error.SessionId == nil, "partial result = %+v, want the later turn's accumulated restore; partial error = %+v, want the internal class with no target Session id", result, result.Error)
		{
			data, err := os.ReadFile(file)
			testassert.Require(t, err == nil && string(data) == "p1", "partial file = (%q, %v), want the completed later-turn restore", data, err)
		}
		assertCanariesUntouched(t, sessionDir)
	})
}

// TestRetainedMountedRestoreConflict proves the retained restore interval
// over the mounted generated client: one parked retained restore holds its
// directory, a concurrent mounted retained restore answers the typed
// conflict, and the parked restore completes with its FIFO preimage while
// the legacy canaries stay untouched.
func TestRetainedMountedRestoreConflict(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)
		ctx := context.Background()

		workspace := filepath.Join(e.home, "retained-mounted-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		legacyID := "c0ffee12"
		sessionDir := seedRetainedSession(t, r.config.loader.Home(), workspace, legacyID)
		file := filepath.Join(workspace, "retained-mounted.txt")
		testassert.NoError(t, os.WriteFile(file, []byte("v1"), 0o600))
		// The turn-1 entry's saved original is a FIFO whose last-write hash
		// matches the file's current content, so the retained restore
		// reaches restoreFile's plain os.Open and parks on the FIFO.
		fifo := seedFIFOCodeEntry(t, sessionDir, file, "v1")

		// One result shape carries both mounted restore attempts — the
		// parked first restore and the concurrent second restore.
		type mountedRetainedResult struct {
			response *protocol.RevertRetainedCodeResponse
			err      error
		}
		request := func() mountedRetainedResult {
			response, err := client.RevertRetainedCodeWithResponse(ctx, protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID})
			return mountedRetainedResult{response: response, err: err}
		}

		// The mounted retained restore parks inside its traversal: its HTTP
		// handler is mid-restore while the client waits.
		parked := make(chan mountedRetainedResult, 1)
		go func() { parked <- request() }()
		// The shared FIFO park returns only once the mounted restore's
		// reader is parked in restoreFile's os.Open.
		park := parkFIFO(t, fifo)
		defer park.flush()

		// A concurrent mounted retained restore answers the typed conflict:
		// the refusal happens before any traversal, so the bounded wait
		// never competes with the parked FIFO.
		secondCh := make(chan mountedRetainedResult, 1)
		go func() { secondCh <- request() }()
		var second mountedRetainedResult
		select {
		case second = <-secondCh:
		case <-time.After(10 * time.Second):
			t.Fatal("the second mounted retained restore never answered, want the typed conflict")
		}
		testassert.Require(t, second.err == nil, "second mounted retained restore: %v, want the typed conflict 409", second.err)
		testassert.Require(t, second.response != nil, "second mounted retained restore = <nil response>, want the typed conflict 409")
		if second.response.JSONDefault == nil || second.response.HTTPResponse.StatusCode != http.StatusConflict {
			body, _ := json.Marshal(second.response)
			t.Fatalf("second mounted retained restore = (%d, %s), want the typed conflict 409", second.response.HTTPResponse.StatusCode, body)
		}
		conflictError, err := second.response.JSONDefault.AsError()
		testassert.Require(t, err == nil && conflictError.Code == protocol.Conflict, "second retained restore error = %+v (%v), want the typed conflict", second.response.JSONDefault, err)

		// The parked mounted retained restore completes with its own result.
		park.release("mounted-restored")
		var out mountedRetainedResult
		select {
		case out = <-parked:
		case <-time.After(10 * time.Second):
			t.Fatal("the parked mounted retained restore never returned after its FIFO completed")
		}
		testassert.Require(t, out.err == nil && out.response != nil && out.response.JSON200 != nil, "mounted retained restore: %v, want the restored result; mounted retained restore = %+v, want the restored result", out.err, out.response)
		testassert.Require(t, len(out.response.JSON200.Restored) == 1 && out.response.JSON200.Restored[0] == file && len(out.response.JSON200.Skipped) == 0, "restored = %+v, want exactly the retained display path", out.response.JSON200)
		{
			data, err := os.ReadFile(file)
			testassert.Require(t, err == nil && string(data) == "mounted-restored", "file = (%q, %v), want the FIFO content", data, err)
		}
		assertCanariesUntouched(t, sessionDir)
	})
}

// --- bounded viewer rows ---

// TestWorkspaceFileReadContainment proves the canonical containment rule and
// the returned canonical identity for relative and absolute contained paths;
// outside-root targets refuse before any read.
func TestWorkspaceFileReadContainment(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "viewer-ws")
		target := filepath.Join(workspace, "docs", "note.txt")
		testassert.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
		testassert.NoError(t, os.WriteFile(target, []byte("hello"), 0o600))
		canonical, err := filepath.EvalSymlinks(target)
		testassert.NoError(t, err)

		relative, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: filepath.Join("docs", "note.txt")})
		testassert.Require(t, err == nil && relative.Content == "hello" && relative.CanonicalPath == canonical && !relative.Truncated, "relative read = (%+v, %v), want the contained canonical view", relative, err)
		absolute, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: target})
		testassert.Require(t, err == nil && absolute.CanonicalPath == canonical, "absolute read = (%+v, %v), want the same canonical target", absolute, err)
		cleaned, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: filepath.Join("docs", "..", "docs", "note.txt")})
		testassert.Require(t, err == nil && cleaned.Content == "hello", "cleaned read = (%+v, %v), want the contained target", cleaned, err)

		outside := filepath.Join(e.home, "outside.txt")
		testassert.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
		{
			_, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: filepath.Join("..", "outside.txt")})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "dotdot read = %v, want harness.ErrInvalid", err)
		}
		{
			_, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: outside})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "absolute outside read = %v, want harness.ErrInvalid", err)
		}
		testassert.NoError(t, os.Symlink(outside, filepath.Join(workspace, "escape")))
		{
			_, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "escape"})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "outside symlink read = %v, want harness.ErrInvalid", err)
		}
		testassert.NoError(t, os.Symlink(target, filepath.Join(workspace, "inside-link")))
		linked, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "inside-link"})
		testassert.Require(t, err == nil && linked.Content == "hello" && linked.CanonicalPath == canonical, "inside symlink read = (%+v, %v), want the resolved contained target", linked, err)
		for _, req := range []protocol.ReadFileRequest{
			{Path: "note.txt"},
			{Workspace: workspace},
		} {
			{
				_, err := r.readWorkspaceFile(context.Background(), req)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "empty member %+v = %v, want harness.ErrInvalid", req, err)
			}
		}
	})
}

// TestWorkspaceFileReadRefusals proves the mandatory viewer refusals:
// sensitive names, hardlinks, non-regular leaves, and missing targets.
func TestWorkspaceFileReadRefusals(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "viewer-refusals")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		for _, sensitive := range []string{".env", "id_rsa", "server.pem"} {
			{
				err := os.WriteFile(filepath.Join(workspace, sensitive), []byte("secret"), 0o600)
				testassert.Require(t, err == nil, "write %s: %v", sensitive, err)
			}
			{
				_, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: sensitive})
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "sensitive %q = %v, want harness.ErrInvalid", sensitive, err)
			}
		}

		original := filepath.Join(workspace, "original.txt")
		testassert.NoError(t, os.WriteFile(original, []byte("linked"), 0o600))
		hardlink := filepath.Join(workspace, "hardlink.txt")
		testassert.NoError(t, os.Link(original, hardlink))
		{
			_, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "hardlink.txt"})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "hardlink read = %v, want harness.ErrInvalid", err)
		}

		fifo := filepath.Join(workspace, "pipe")
		testassert.NoError(t, unix.Mkfifo(fifo, 0o600))
		{
			_, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "pipe"})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "fifo read = %v, want harness.ErrInvalid", err)
		}
		{
			_, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "."})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "directory read = %v, want harness.ErrInvalid", err)
		}
		{
			_, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "missing.txt"})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "missing read = %v, want harness.ErrNotFound", err)
		}
	})
}

// TestViewerSensitivePath proves the viewer helper's sensitive-name
// basename and glob data, moved with the helper into this owner.
func TestViewerSensitivePath(t *testing.T) {
	sensitive := []string{
		"/home/user/.env",
		"/home/user/.netrc",
		"/home/user/.ssh/id_rsa",
		"/home/user/cert.pem",
		"/home/user/key.key",
		"/home/user/credentials.json",
		"/home/user/credentials.yaml",
	}
	for _, p := range sensitive {
		testassert.Require(t, isSensitivePath(p), "isSensitivePath(%q) = false, want true", p)
	}

	normal := []string{
		"/home/user/README.md",
		"/home/user/main.go",
		"/home/user/.gitignore",
	}
	for _, p := range normal {
		testassert.Require(t, !isSensitivePath(p), "isSensitivePath(%q) = true, want false", p)
	}
}

// TestWorkspaceFileReadPrefixRule proves the exact read-prefix rule: 1 MiB of
// raw bytes plus one sentinel decides truncation, invalid byte runs convert
// with U+FFFD, and conversion may expand the returned content beyond the cap
// because no second converted/wire-length cap exists.
func TestWorkspaceFileReadPrefixRule(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "viewer-prefix")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))

		write := func(name string, data []byte) string {
			t.Helper()
			path := filepath.Join(workspace, name)
			{
				err := os.WriteFile(path, data, 0o600)
				testassert.Require(t, err == nil, "write %s: %v", name, err)
			}
			return path
		}

		write("binary.bin", []byte{'A', 0xff, 0xfe, 'B', 0x80, 0x80, 'C'})
		binary, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "binary.bin"})
		testassert.Require(t, err == nil && binary.Content == "A\uFFFDB\uFFFDC" && !binary.Truncated, "binary view = (%+v, %v), want the maximal invalid-run replacements", binary, err)

		write("exact.txt", bytes.Repeat([]byte("a"), maxViewPrefixBytes))
		exact, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "exact.txt"})
		testassert.Require(t, err == nil && !exact.Truncated && len(exact.Content) == maxViewPrefixBytes, "exact view = (%d bytes, truncated %v, err %v), want the untruncated 1 MiB", len(exact.Content), exact.Truncated, err)

		large := append(bytes.Repeat([]byte("a"), maxViewPrefixBytes), []byte("tail")...)
		write("large.txt", large)
		view, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "large.txt"})
		testassert.Require(t, err == nil && view.Truncated && len(view.Content) == maxViewPrefixBytes && !strings.ContainsRune(view.Content, '\uFFFD'), "large view = (%d bytes, truncated %v, err %v), want exactly the first raw MiB", len(view.Content), view.Truncated, err)

		// Truncation precedes conversion: the cap cuts the three-byte rune
		// after its first byte, so the dangling run becomes one replacement.
		midRune := append(bytes.Repeat([]byte("a"), maxViewPrefixBytes-1), []byte("\u20AC extra")...)
		write("mid-rune.txt", midRune)
		cut, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "mid-rune.txt"})
		testassert.Require(t, err == nil && cut.Truncated && cut.Content == strings.Repeat("a", maxViewPrefixBytes-1)+"\uFFFD", "mid-rune view = (%q..., truncated %v, err %v), want the raw cap converted after truncation", cut.Content, cut.Truncated, err)

		// No second converted-length cap: alternating invalid runs convert to
		// four bytes per two raw bytes, so the returned content is 2 MiB for
		// the 1 MiB raw prefix; a wire-length cap would cut it back to 1 MiB.
		pattern := bytes.Repeat([]byte{0xff, 'A'}, maxViewPrefixBytes/2)
		write("invalid.bin", append(pattern, 0x00))
		expanded, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "invalid.bin"})
		testassert.Require(t, err == nil && expanded.Truncated && expanded.Content == strings.Repeat("\uFFFDA", maxViewPrefixBytes/2), "expanded view = (%d bytes, truncated %v, err %v), want 2 MiB of converted raw prefix without a wire cap", len(expanded.Content), expanded.Truncated, err)
	})
}

// --- closure ---

// TestCodeProducersClosedOwner proves every deferred-code and viewer producer
// joins the existing admission gate: after Close each one returns ErrClosed
// without touching storage or the filesystem.
func TestCodeProducersClosedOwner(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		testassert.NoError(t, r.Close(context.Background()))
		ctx := context.Background()
		sessionID := "0123456789abcdef0123456789abcdef"
		workspace := filepath.Join(e.home, "closed-ws")
		{
			_, err := r.listSessionCodeSnapshots(ctx, sessionID)
			testassert.Require(t, errors.Is(err, ErrClosed), "closed list = %v, want ErrClosed", err)
		}
		{
			_, err := r.revertSessionCode(ctx, sessionID, "op")
			testassert.Require(t, errors.Is(err, ErrClosed), "closed revert = %v, want ErrClosed", err)
		}
		{
			_, err := r.listRetainedCodeSnapshots(ctx, workspace, "deadbeef")
			testassert.Require(t, errors.Is(err, ErrClosed), "closed retained list = %v, want ErrClosed", err)
		}
		{
			_, err := r.revertRetainedCode(ctx, protocol.RetainedRevertRequest{Workspace: workspace, SessionId: "deadbeef"})
			testassert.Require(t, errors.Is(err, ErrClosed), "closed retained revert = %v, want ErrClosed", err)
		}
		{
			_, err := r.readWorkspaceFile(ctx, protocol.ReadFileRequest{Workspace: workspace, Path: "x"})
			testassert.Require(t, errors.Is(err, ErrClosed), "closed viewer = %v, want ErrClosed", err)
		}
	})
}

// sha256Hex and sha256Hex16 mirror the retained snapshot identity encodings
// for hand-built fixtures only.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sha256Hex16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// awaitOpenerRegistered polls the owned registry state until the Session's
// artifact interval shows the held token plus one registered waiter — the
// deterministic parked-opener rendezvous.
func awaitOpenerRegistered(t *testing.T, r *Runtime, sessionID string) {
	t.Helper()
	awaitRegistryState(t, r.artifacts, sessionArtifactDir(r.dataDir, sessionID), 2)
}

// awaitNoRegistryEntries polls until the Session's artifact interval entry is
// reclaimed: every lease released and every attempt dropped.
func awaitNoRegistryEntries(t *testing.T, r *Runtime, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.artifacts.mu.Lock()
		entries := len(r.artifacts.entries)
		r.artifacts.mu.Unlock()
		if entries == 0 {
			return
		}
		testassert.Require(t, !time.Now().After(deadline), "%s left %d registry entries, want the reclaimed empty registry", what, entries)
		time.Sleep(5 * time.Millisecond)
	}
}

// TestQueuedDrainOpenerWaitsForRestoreInterval proves the queued-drain entry
// path through the interval: with the restore parked, the direct submit's
// opener registers as the token's waiter and the queued input stays buffered;
// after the restore releases, the first execution runs to its terminal and
// the drain admits the queued input through the same opener site, both
// settling in order with the restore's result intact and the registry
// reclaimed at the end.
func TestQueuedDrainOpenerWaitsForRestoreInterval(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "drain-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		op := submitCodeOperation(t, r, session, "op-1", "only")
		awaitRestorableSession(t, r, session)
		file := filepath.Join(workspace, "queued.txt")
		testassert.NoError(t, os.WriteFile(file, []byte("v1"), 0o600))
		fifo := seedFIFOCodeEntry(t, codeGroupRoot(r, session, op.Admission.AdmittedEntry.EntryID), file, "v1")

		park, outcomes := parkRestore(t, r, session, "op-1", fifo)
		defer park.flush()
		drainCommandModelArrivals(e)

		gate := make(chan struct{})
		var gateOnce sync.Once
		releaseGate := func() { gateOnce.Do(func() { close(gate) }) }
		defer releaseGate()
		e.server.setHold(gate)

		run, err := submitRawThroughRuntime(context.Background(), r, session, "op-run", "run first", harness.MessageModeRegular)
		testassert.Require(t, err == nil, "Submit(op-run): %v", err)
		testassert.Require(t, run.Disposition == harness.DispositionAdmitted, "disposition %q, want admitted", run.Disposition)
		submitQueuedThroughRuntime(t, r, session, "op-queued", "queued behind")

		awaitOpenerRegistered(t, r, session)
		select { // neither execution's model request began inside the interval
		case <-e.server.arrived:
			t.Fatal("a model request began inside the restore interval")
		default:
		}

		park.release("restored")
		var parkedRestore restoreOutcome
		select {
		case parkedRestore = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("the parked restore never returned after its FIFO completed")
		}
		testassert.Require(t, parkedRestore.err == nil && len(parkedRestore.result.Restored) == 1, "parked restore = (%+v, %v)", parkedRestore.result, parkedRestore.err)

		awaitModelArrival(t, e) // the direct submit's opener won the released interval
		releaseGate()
		awaitOperation(t, r, session, "op-run", harness.OperationSuccess)
		awaitOperation(t, r, session, "op-queued", harness.OperationSuccess) // the drain's opener ran
		awaitIdleSession(t, r, session)
		awaitNoRegistryEntries(t, r, "the settled drain")
	})
}

// TestBackgroundWakeOpenerWaitsForRestoreInterval proves the
// background-completion-wake entry path through the interval: with the
// restore parked, a real job's completion delivery admits its completion
// Operation through the idle path, whose opener registers as the token's
// waiter; after the restore releases, the wake's execution runs and settles.
func TestBackgroundWakeOpenerWaitsForRestoreInterval(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, agents.PathForConfig(e.configPath), projectionAgentsDocument)
		stopper := &stubStopper{events: e.events}
		r, err := e.open(context.Background(), e.storagePlugin(store), jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, stopper))
		testassert.NoError(t, err)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "wake-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		op := submitCodeOperation(t, r, session, "op-1", "only")
		awaitRestorableSession(t, r, session)
		file := filepath.Join(workspace, "wake.txt")
		testassert.NoError(t, os.WriteFile(file, []byte("v1"), 0o600))
		fifo := seedFIFOCodeEntry(t, codeGroupRoot(r, session, op.Admission.AdmittedEntry.EntryID), file, "v1")

		park, outcomes := parkRestore(t, r, session, "op-1", fifo)
		defer park.flush()
		drainCommandModelArrivals(e)

		gate := make(chan struct{})
		var gateOnce sync.Once
		releaseGate := func() { gateOnce.Do(func() { close(gate) }) }
		defer releaseGate()
		e.server.setHold(gate)

		var completionID string
		testassert.NoError(t, r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			return h.StartJob(ctx, session, "0a1b2c3d", func(_ context.Context, id string) error {
				completionID = id
				return nil
			})
		}))
		testassert.NoError(t, r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			return h.DeliverBackgroundCompletion(ctx, session, completionID, "job report")
		}))

		awaitOpenerRegistered(t, r, session) // the wake's opener is the registered waiter
		select {
		case <-e.server.arrived:
			t.Fatal("the wake's model request began inside the restore interval")
		default:
		}

		park.release("restored")
		var parkedRestore restoreOutcome
		select {
		case parkedRestore = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("the parked restore never returned after its FIFO completed")
		}
		testassert.Require(t, parkedRestore.err == nil && len(parkedRestore.result.Restored) == 1, "parked restore = (%+v, %v)", parkedRestore.result, parkedRestore.err)

		awaitModelArrival(t, e) // the wake's opener won the released interval
		releaseGate()
		awaitOperation(t, r, session, completionID, harness.OperationSuccess)
		awaitNoRegistryEntries(t, r, "the settled wake")
	})
}

// TestExecutionOpenerFailureReleasesInterval proves the opening-failure row:
// a failed opener releases the Session's artifact token before returning, so
// the registry is reclaimed and the next execution runs normally.
// failingAgentFactoryPlugin is one Agent-scoped plugin whose factory fails
// while armed: the concrete opening-failure row.
func failingAgentFactoryPlugin(fail *atomic.Bool) Plugin {
	return Plugin{
		ID:       "failing-agent",
		Scope:    ScopeAgent,
		Provides: []CapabilitySpec{Spec[any]("failing.agent.cap")},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			if fail.Load() {
				return Instance{}, errors.New("boom")
			}
			return Instance{Values: map[string]any{"failing.agent.cap": "ok"}}, nil
		},
	}
}

func TestExecutionOpenerFailureReleasesInterval(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		var fail atomic.Bool
		fail.Store(true)
		r, err := e.open(context.Background(), e.storagePlugin(store), failingAgentFactoryPlugin(&fail))
		testassert.NoError(t, err)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "openerr-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID

		_, err = submitRawThroughRuntime(context.Background(), r, session, "op-fail", "fail", harness.MessageModeRegular)
		testassert.NoError(t, err)
		{
			rec := awaitOperation(t, r, session, "op-fail", harness.OperationFailure)
			testassert.Require(t, rec.State.Status == harness.OperationFailure, "failed opening settled as %+v, want failure", rec.State)
		}
		awaitNoRegistryEntries(t, r, "the failed opening") // the unwind released the token
		awaitRestorableSession(t, r, session)              // the failed execution's slot retired

		fail.Store(false)
		submitCodeOperation(t, r, session, "op-next", "next") // the next execution runs normally
		awaitNoRegistryEntries(t, r, "the settled successor")
	})
}

// TestExecutionCleanupReleasesIntervalLast proves the release-last ordering:
// while the concrete execution's cleanup phase is held open, the token is
// still held — a restore conflicts — and only after the cleanup and the
// scoped closes complete is the registry reclaimed and the same restore
// admitted.
// parkingAgentClosePlugin is one Agent-scoped plugin whose Instance.Close
// signals its start and then parks on the release gate: the held
// execution-cleanup row.
func parkingAgentClosePlugin(started chan struct{}, gate chan struct{}) Plugin {
	return Plugin{
		ID:       "parking-agent",
		Scope:    ScopeAgent,
		Provides: []CapabilitySpec{Spec[any]("parking.agent.cap")},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"parking.agent.cap": "ok"}, Close: func() error {
				select {
				case started <- struct{}{}:
				default:
				}
				<-gate
				return nil
			}}, nil
		},
	}
}

func TestExecutionCleanupReleasesIntervalLast(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		closeGate := make(chan struct{})
		closeStarted := make(chan struct{}, 1)
		r, err := e.open(context.Background(), e.storagePlugin(store), parkingAgentClosePlugin(closeStarted, closeGate))
		testassert.NoError(t, err)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "cleanup-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID

		var gateOnce sync.Once
		releaseCleanup := func() { gateOnce.Do(func() { close(closeGate) }) }
		defer releaseCleanup() // a failing assertion must not leak a held execution Close
		submitThroughRuntime(t, r, session, "op-1", "run")
		select { // the execution's scoped cleanup started and blocked
		case <-closeStarted:
		case <-time.After(10 * time.Second):
			t.Fatal("the execution cleanup never started")
		}

		// The token is still held through the blocked cleanup and the not-yet-
		// closed scopes: a restore conflicts before any directory work.
		{
			_, err := r.revertSessionCode(context.Background(), session, "op-1")
			testassert.Require(t, errors.Is(err, harness.ErrConflict), "restore during the blocked cleanup = %v, want the conflict class", err)
		}
		assertRegistryState(t, r.artifacts, sessionArtifactDir(r.dataDir, session), 1, 1, true)

		releaseCleanup() // the cleanup completes; the scoped closes and the release follow
		awaitRestorableSession(t, r, session)
		awaitNoRegistryEntries(t, r, "the settled cleanup")
		result, err := r.revertSessionCode(context.Background(), session, "op-1")
		testassert.Require(t, err == nil && result.Restored != nil, "restore after the cleanup = (%+v, %v), want the admitted traversal", result, err)
	})
}

// TestRestoreIntervalCancellationAndShutdownOutcomes proves the cancellation
// rows: a caller canceled before ownership touches no token and no file; an
// opener canceled by an interrupt while parked on the token drops its
// reference and settles without success; and a begun restore finishes its
// admitted result under the managed shutdown, which joins it.
func TestRestoreIntervalCancellationAndShutdownOutcomes(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r) // every early failure joins the ordinary Runtime cleanup
		workspace := filepath.Join(e.home, "cancel-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		op := submitCodeOperation(t, r, session, "op-1", "only")
		awaitRestorableSession(t, r, session)
		file := filepath.Join(workspace, "cancel.txt")
		testassert.NoError(t, os.WriteFile(file, []byte("v1"), 0o600))
		fifo := seedFIFOCodeEntry(t, codeGroupRoot(r, session, op.Admission.AdmittedEntry.EntryID), file, "v1")

		// A canceled caller before ownership: no token, no file effect.
		canceledCtx, cancelCaller := context.WithCancel(context.Background())
		cancelCaller()
		{
			_, err := r.revertSessionCode(canceledCtx, session, "op-1")
			testassert.Require(t, errors.Is(err, context.Canceled), "canceled restore = %v, want the caller's cancellation", err)
		}
		assertRegistryState(t, r.artifacts, sessionArtifactDir(r.dataDir, session), 0, 0, false)

		park, outcomes := parkRestore(t, r, session, "op-1", fifo)
		defer park.flush()
		drainCommandModelArrivals(e)

		_, err := submitRawThroughRuntime(context.Background(), r, session, "op-parked", "parked", harness.MessageModeRegular)
		testassert.NoError(t, err)
		awaitOpenerRegistered(t, r, session)

		// The managed shutdown begins while the opener is parked: its
		// execution context cancels, the parked acquisition fails, and the
		// waiter's reference drops while the restore keeps its own.
		r.beginShutdown()
		awaitRegistryState(t, r.artifacts, sessionArtifactDir(r.dataDir, session), 1)

		// The begun restore finishes its admitted result under the shutdown.
		park.release("restored")
		var parkedRestore restoreOutcome
		select {
		case parkedRestore = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("the begun restore never finished under the shutdown")
		}
		testassert.Require(t, parkedRestore.err == nil && len(parkedRestore.result.Restored) == 1 && parkedRestore.result.Restored[0] == file, "begun restore = (%+v, %v), want its admitted result", parkedRestore.result, parkedRestore.err)
		{
			data, err := os.ReadFile(file)
			testassert.Require(t, err == nil && string(data) == "restored", "file = (%q, %v), want the restore's FIFO content", data, err)
		}
		testassert.NoError(t, r.Close(context.Background()))
	})
}
