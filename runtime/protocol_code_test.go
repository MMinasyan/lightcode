package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/model"
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
	if err != nil {
		t.Fatalf("OpenCodeStore(%s): %v", groupRoot, err)
	}
	entryID, _, err := group.SnapshotResolvedEntry(1, displayPath, target)
	if err != nil {
		t.Fatalf("SnapshotResolvedEntry(%s): %v", target, err)
	}
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", target, err)
	}
	if err := group.RecordSnapshotContent(1, entryID, content); err != nil {
		t.Fatalf("RecordSnapshotContent(%s): %v", target, err)
	}
	group.RetainSnapshotEntry(1, entryID)
}

// submitCodeOperation submits one regular message and waits for its admitted
// terminal, tolerating the terminal-to-retirement buffering window: a
// buffered submission drains under the same caller ID.
func submitCodeOperation(t *testing.T, r *Runtime, sessionID, operationID, text string) harness.OperationRecord {
	t.Helper()
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		_, err := h.Submit(ctx, harness.SubmitRequest{
			SessionID:   sessionID,
			OperationID: operationID,
			Origin:      harness.InputOriginUser,
			Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
			Mode:        harness.MessageModeRegular,
		})
		return err
	})
	if err != nil {
		t.Fatalf("Submit(%q): %v", operationID, err)
	}
	rec := awaitOperation(t, r, sessionID, operationID, harness.OperationSuccess)
	awaitIdleSession(t, r, sessionID)
	return rec
}

// awaitRestorableSession polls one Session until its snapshot satisfies the
// one idle gate the deferred restore reads: no running/reserved/retiring work,
// empty pending FIFOs, and no live background member.
func awaitRestorableSession(t *testing.T, r *Runtime, sessionID string) harness.SessionSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap := snapshotThroughRuntime(t, r, sessionID)
		if !snap.ExecutionBusy && len(snap.Steering) == 0 && len(snap.Queued) == 0 && len(snap.Background) == 0 {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %q never reached the restorable state: busy=%v steering=%d queued=%d background=%d",
				sessionID, snap.ExecutionBusy, len(snap.Steering), len(snap.Queued), len(snap.Background))
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
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID

		// A valid Session with no recorded groups lists a non-nil empty array
		// before any capture exists.
		initial, err := r.listSessionCodeSnapshots(context.Background(), session)
		if err != nil || initial == nil || len(initial) != 0 {
			t.Fatalf("initial group list = (%+v, %v), want a non-nil empty list", initial, err)
		}

		// Admission order is op-z, then op-a: OperationID order is reversed.
		first := submitCodeOperation(t, r, session, "op-z", "first")
		second := submitCodeOperation(t, r, session, "op-a", "second")
		empty := submitCodeOperation(t, r, session, "op-m", "empty")
		firstEntry := first.Admission.AdmittedEntry.EntryID
		secondEntry := second.Admission.AdmittedEntry.EntryID
		emptyEntry := empty.Admission.AdmittedEntry.EntryID
		if firstEntry == "" || secondEntry == "" || emptyEntry == "" {
			t.Fatalf("admitted entries = (%q, %q, %q), want all present", firstEntry, secondEntry, emptyEntry)
		}

		edited := filepath.Join(workspace, "notes.txt")
		if err := os.WriteFile(edited, []byte("v0"), 0o600); err != nil {
			t.Fatalf("write preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(r, session, firstEntry), edited, edited, []byte("v1"))
		created := filepath.Join(workspace, "created.txt")
		captureCodeMutation(t, codeGroupRoot(r, session, secondEntry), created, created, []byte("new"))
		if err := os.MkdirAll(codeGroupRoot(r, session, emptyEntry), 0o700); err != nil {
			t.Fatalf("mkdir empty group: %v", err)
		}

		groups, err := r.listSessionCodeSnapshots(context.Background(), session)
		if err != nil {
			t.Fatalf("listSessionCodeSnapshots: %v", err)
		}
		if len(groups) != 2 {
			t.Fatalf("groups = %+v, want exactly the two recorded groups", groups)
		}
		if groups[0].OperationId != "op-z" || groups[1].OperationId != "op-a" {
			t.Fatalf("group order = (%q, %q), want admission order (op-z, op-a)", groups[0].OperationId, groups[1].OperationId)
		}
		if groups[0].GroupItemId != projectItemID(session, firstEntry) || groups[1].GroupItemId != projectItemID(session, secondEntry) {
			t.Fatalf("group item ids = (%q, %q), want the admitted-entry projections", groups[0].GroupItemId, groups[1].GroupItemId)
		}
		if len(groups[0].Files) != 1 || groups[0].Files[0] != (protocol.SnapshotFile{Path: edited, Existed: true}) {
			t.Fatalf("first group files = %+v, want the edited display path with an existing preimage", groups[0].Files)
		}
		if len(groups[1].Files) != 1 || groups[1].Files[0] != (protocol.SnapshotFile{Path: created, Existed: false}) {
			t.Fatalf("second group files = %+v, want the created display path with an absent preimage", groups[1].Files)
		}
	})
}

// TestCodeSnapshotsSteeringSharesAdmittedGroup proves a real drained steering
// input that shares its owning Operation does not create or displace the
// group: the group key is the Operation's own admitted entry, never the later
// steering entry, over the multi-boundary compact-lifecycle fixture.
func TestCodeSnapshotsSteeringSharesAdmittedGroup(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		f := openCompactLifecycle(t, store)
		defer func() { _ = f.r.Close(context.Background()) }()
		session := f.session("steering-group")

		parked := make(chan struct{}, 1)
		gate := make(chan struct{})
		f.setConvScript(session, func(_ context.Context, req model.Request) (model.Stream, error) {
			if strings.Contains(requestText(req), "start work") {
				select {
				case parked <- struct{}{}:
				default:
				}
				<-gate
			}
			return lifecycleTextTurn("resumed"), nil
		})
		f.submit(session, "op-1", "start work")
		<-parked
		if got := f.submitMode(session, "op-steer", "steering text", harness.MessageModeRegular); got != harness.DispositionSteering {
			t.Fatalf("steering submit = %q, want steering", got)
		}
		close(gate)
		awaitOperation(t, f.r, session, "op-1", harness.OperationSuccess)
		awaitIdleSession(t, f.r, session)

		admitted := readOperation(t, f.r, session, "op-1").Admission.AdmittedEntry.EntryID
		var steering string
		for _, fact := range snapshotThroughRuntime(t, f.r, session).Facts {
			if fact.Kind == harness.EntryInput && fact.OperationID == "op-1" && fact.EntryID != admitted {
				steering = fact.EntryID
			}
		}
		if admitted == "" || steering == "" {
			t.Fatalf("admitted/steering entries = (%q, %q), want a real same-Operation steering input", admitted, steering)
		}

		workspace := filepath.Join(f.r.dataDir, "steering-ws")
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		admittedFile := filepath.Join(workspace, "admitted.txt")
		if err := os.WriteFile(admittedFile, []byte("v0"), 0o600); err != nil {
			t.Fatalf("write admitted preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(f.r, session, admitted), admittedFile, admittedFile, []byte("v1"))
		steeringFile := filepath.Join(workspace, "steering.txt")
		if err := os.WriteFile(steeringFile, []byte("s0"), 0o600); err != nil {
			t.Fatalf("write steering preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(f.r, session, steering), steeringFile, steeringFile, []byte("s1"))

		awaitRestorableSession(t, f.r, session)
		groups, err := f.r.listSessionCodeSnapshots(context.Background(), session)
		if err != nil {
			t.Fatalf("listSessionCodeSnapshots: %v", err)
		}
		if len(groups) != 1 || groups[0].OperationId != "op-1" || groups[0].GroupItemId != projectItemID(session, admitted) {
			t.Fatalf("groups = %+v, want exactly the admitted-entry group %q", groups, admitted)
		}

		result, err := f.r.revertSessionCode(context.Background(), session, "op-1")
		if err != nil {
			t.Fatalf("revertSessionCode: %v", err)
		}
		if len(result.Restored) != 1 || result.Restored[0] != admittedFile {
			t.Fatalf("restored = %v, want only the admitted group's file", result.Restored)
		}
		if data, err := os.ReadFile(steeringFile); err != nil || string(data) != "s1" {
			t.Fatalf("steering-entry file = (%q, %v), want it untouched: the steering entry is not a group key", data, err)
		}
	})
}

// TestCodeSnapshotsBoundaryAndRevertOrder proves the boundary rule and reverse
// admission restore order: manual compaction is invalid, a message boundary
// with no recorded snapshots still rewinds later groups, and an ID-sorted
// implementation cannot pass the same-file preimage chain.
func TestCodeSnapshotsBoundaryAndRevertOrder(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		f := openCompactLifecycle(t, store)
		defer func() { _ = f.r.Close(context.Background()) }()
		session := f.session("revert-order")
		workspace := filepath.Join(f.r.dataDir, "revert-order-ws")
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}

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
		if err := os.WriteFile(edited, []byte("v0"), 0o600); err != nil {
			t.Fatalf("write chain preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(f.r, session, firstEntry), edited, edited, []byte("v1"))
		created := filepath.Join(workspace, "created.txt")
		captureCodeMutation(t, codeGroupRoot(f.r, session, lastEntry), edited, edited, []byte("v2"))
		captureCodeMutation(t, codeGroupRoot(f.r, session, lastEntry), created, created, []byte("c"))

		// The middle Operation owns no recorded snapshots but is a valid
		// boundary: op-a still rewinds.
		result, err := f.r.revertSessionCode(context.Background(), session, "op-m")
		if err != nil {
			t.Fatalf("revert boundary op-m: %v", err)
		}
		if len(result.Restored) != 2 || !containsString(result.Restored, edited) || !containsString(result.Restored, created) {
			t.Fatalf("restored = %v, want the later group's edited and created files", result.Restored)
		}
		if data, err := os.ReadFile(edited); err != nil || string(data) != "v1" {
			t.Fatalf("chain after boundary op-m = (%q, %v), want op-a's restored preimage", data, err)
		}
		if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created file stat = %v, want restored absence", err)
		}

		// Re-record op-a, then the earliest admitted boundary op-z rewinds the
		// whole chain to v0: a correct reverse-admission visit restores op-a's
		// preimage v1 first and then op-z's v0. An OperationID-sorted pair
		// addresses only op-z and skips on the v1 identity mismatch.
		captureCodeMutation(t, codeGroupRoot(f.r, session, lastEntry), edited, edited, []byte("v2"))
		result, err = f.r.revertSessionCode(context.Background(), session, "op-z")
		if err != nil {
			t.Fatalf("revert boundary op-z: %v", err)
		}
		if data, err := os.ReadFile(edited); err != nil || string(data) != "v0" {
			t.Fatalf("chain after boundary op-z = (%q, %v, restored %v), want the earliest group's preimage", data, err, result.Restored)
		}

		// A manual-compaction Operation has the zero admitted entry: it is not
		// a valid boundary and no group is opened.
		f.compactIdle(session, "compact-op")
		awaitRestorableSession(t, f.r, session)
		groups, err := f.r.listSessionCodeSnapshots(context.Background(), session)
		if err != nil {
			t.Fatalf("list after compact: %v", err)
		}
		for _, group := range groups {
			if group.OperationId == "compact-op" {
				t.Fatalf("compact Operation listed a code group: %+v", group)
			}
		}
		for _, boundary := range []string{"compact-op", "", "op-missing"} {
			if _, err := f.r.revertSessionCode(context.Background(), session, boundary); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("revert boundary %q = %v, want harness.ErrInvalid", boundary, err)
			}
		}
		if data, err := os.ReadFile(edited); err != nil || string(data) != "v0" {
			t.Fatalf("chain after refused boundaries = (%q, %v), want no mutation", data, err)
		}
	})
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestCodeSnapshotsSkipOnIdentityChange proves the retained CodeStore proof
// survives restore: changed content and a changed canonical path are skipped
// with their display path, never overwritten.
func TestCodeSnapshotsSkipOnIdentityChange(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "skip-ws")
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		op := submitCodeOperation(t, r, session, "op-1", "one")
		entry := op.Admission.AdmittedEntry.EntryID

		changed := filepath.Join(workspace, "changed.txt")
		if err := os.WriteFile(changed, []byte("v0"), 0o600); err != nil {
			t.Fatalf("write changed preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(r, session, entry), changed, changed, []byte("v1"))
		if err := os.WriteFile(changed, []byte("external"), 0o600); err != nil {
			t.Fatalf("external edit: %v", err)
		}

		moved := filepath.Join(workspace, "moved.txt")
		if err := os.WriteFile(moved, []byte("m0"), 0o600); err != nil {
			t.Fatalf("write moved preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(r, session, entry), moved, moved, []byte("m1"))
		if err := os.Remove(moved); err != nil {
			t.Fatalf("remove moved: %v", err)
		}
		elsewhere := filepath.Join(workspace, "elsewhere.txt")
		if err := os.WriteFile(elsewhere, []byte("elsewhere"), 0o600); err != nil {
			t.Fatalf("write elsewhere: %v", err)
		}
		if err := os.Symlink(elsewhere, moved); err != nil {
			t.Fatalf("symlink moved: %v", err)
		}

		awaitRestorableSession(t, r, session)
		result, err := r.revertSessionCode(context.Background(), session, "op-1")
		if err != nil {
			t.Fatalf("revertSessionCode: %v", err)
		}
		if len(result.Restored) != 0 || len(result.Skipped) != 2 {
			t.Fatalf("result = %+v, want both files skipped", result)
		}
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
		if data, err := os.ReadFile(changed); err != nil || string(data) != "external" {
			t.Fatalf("changed file = (%q, %v), want the external content untouched", data, err)
		}
		if link, err := os.Readlink(moved); err != nil || link != elsewhere {
			t.Fatalf("moved file = (%q, %v), want the symlink untouched", link, err)
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
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		root := projectionSession(t, r, workspace, "solo").Identity.SessionID
		rootOp := submitCodeOperation(t, r, root, "op-root", "root")
		rootEntry := rootOp.Admission.AdmittedEntry.EntryID
		rootFile := filepath.Join(workspace, "root.txt")
		if err := os.WriteFile(rootFile, []byte("r0"), 0o600); err != nil {
			t.Fatalf("write root preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(r, root, rootEntry), rootFile, rootFile, []byte("r1"))

		e.prep.awaitCleanups(1) // the settled root execution retires before Fork requires its idle source
		forked := forkThroughRuntime(t, r, root, rootEntry, "op-fork")
		e.prep.awaitCleanups(2)
		forkOp := readOperation(t, r, forked, "op-fork")
		forkEntry := forkOp.Admission.AdmittedEntry.EntryID
		forkFile := filepath.Join(workspace, "fork.txt")
		if err := os.WriteFile(forkFile, []byte("f0"), 0o600); err != nil {
			t.Fatalf("write fork preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(r, forked, forkEntry), forkFile, forkFile, []byte("f1"))

		child := launchChildThroughRuntime(t, r, root, "op-child")
		e.prep.awaitCleanups(3)
		childOp := readOperation(t, r, child, "op-child")
		childEntry := childOp.Admission.AdmittedEntry.EntryID
		childFile := filepath.Join(workspace, "child.txt")
		if err := os.WriteFile(childFile, []byte("c0"), 0o600); err != nil {
			t.Fatalf("write child preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(r, child, childEntry), childFile, childFile, []byte("c1"))

		awaitRestorableSession(t, r, root)
		result, err := r.revertSessionCode(context.Background(), root, "op-root")
		if err != nil {
			t.Fatalf("revert root: %v", err)
		}
		if len(result.Restored) != 1 || result.Restored[0] != rootFile {
			t.Fatalf("root restored = %v, want only the root file", result.Restored)
		}
		if data, err := os.ReadFile(rootFile); err != nil || string(data) != "r0" {
			t.Fatalf("root file = (%q, %v), want the restored preimage", data, err)
		}
		if data, err := os.ReadFile(forkFile); err != nil || string(data) != "f1" {
			t.Fatalf("fork file = (%q, %v), want it untouched", data, err)
		}
		if data, err := os.ReadFile(childFile); err != nil || string(data) != "c1" {
			t.Fatalf("child file = (%q, %v), want it untouched", data, err)
		}
		for _, tc := range []struct{ session, entry, operation string }{
			{forked, forkEntry, "op-fork"},
			{child, childEntry, "op-child"},
		} {
			groups, err := r.listSessionCodeSnapshots(context.Background(), tc.session)
			if err != nil {
				t.Fatalf("list %s: %v", tc.operation, err)
			}
			if len(groups) != 1 || groups[0].OperationId != tc.operation || groups[0].GroupItemId != projectItemID(tc.session, tc.entry) {
				t.Fatalf("%s groups = %+v, want the Session's own group", tc.operation, groups)
			}
		}
	})
}

// TestCodeRevertGateAxes proves the one restore state gate over every
// semantic axis independently: idle passes; ExecutionBusy (running, reserved
// or retiring), either pending FIFO, and a live background member each refuse
// with the conflict class.
func TestCodeRevertGateAxes(t *testing.T) {
	base := harness.SessionSnapshot{Session: harness.SessionRecord{Identity: harness.SessionIdentity{SessionID: "0123456789abcdef0123456789abcdef"}}}
	cases := []struct {
		name    string
		mutate  func(*harness.SessionSnapshot)
		refused bool
	}{
		{"idle", nil, false},
		{"busy-running-or-reserved-or-retiring", func(s *harness.SessionSnapshot) { s.ExecutionBusy = true }, true},
		{"pending-steering", func(s *harness.SessionSnapshot) {
			s.Steering = []harness.PendingInput{{OperationID: "op"}}
		}, true},
		{"pending-queued", func(s *harness.SessionSnapshot) {
			s.Queued = []harness.PendingInput{{OperationID: "op"}}
		}, true},
		{"live-background-member", func(s *harness.SessionSnapshot) {
			s.Background = []harness.BackgroundMemberView{{Kind: "child", ID: "child"}}
		}, true},
	}
	for _, tc := range cases {
		snap := base
		if tc.mutate != nil {
			tc.mutate(&snap)
		}
		err := codeRestoreRefusal(snap)
		if tc.refused {
			if !errors.Is(err, harness.ErrConflict) {
				t.Fatalf("%s gate = %v, want harness.ErrConflict", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s gate = %v, want nil", tc.name, err)
		}
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
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID

		gate := make(chan struct{})
		e.prep.modelGate = gate
		submitThroughRuntime(t, r, session, "op-1", "running")
		awaitModelArrival(t, e)
		runningEntry := readOperation(t, r, session, "op-1").Admission.AdmittedEntry.EntryID
		file := filepath.Join(workspace, "gated.txt")
		if err := os.WriteFile(file, []byte("v0"), 0o600); err != nil {
			t.Fatalf("write preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(r, session, runningEntry), file, file, []byte("v1"))

		if _, err := r.revertSessionCode(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrConflict) {
			t.Fatalf("running restore = %v, want harness.ErrConflict", err)
		}
		if data, err := os.ReadFile(file); err != nil || string(data) != "v1" {
			t.Fatalf("running refusal mutated the file: (%q, %v)", data, err)
		}
		close(gate)
		e.prep.awaitCleanups(1)
		awaitRestorableSession(t, r, session)
		result, err := r.revertSessionCode(context.Background(), session, "op-1")
		if err != nil || len(result.Restored) != 1 {
			t.Fatalf("idle restore = (%+v, %v), want the restored file", result, err)
		}

		// A live background child refuses the parent's restore even while the
		// parent's own Operation state is idle.
		parent := projectionSession(t, r, workspace, "solo").Identity.SessionID
		parentOp := submitCodeOperation(t, r, parent, "op-parent", "parent")
		parentEntry := parentOp.Admission.AdmittedEntry.EntryID
		parentFile := filepath.Join(workspace, "parent.txt")
		if err := os.WriteFile(parentFile, []byte("p0"), 0o600); err != nil {
			t.Fatalf("write parent preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(r, parent, parentEntry), parentFile, parentFile, []byte("p1"))
		e.prep.awaitCleanups(2) // the parent's own execution retires before the child launch
		childGate := make(chan struct{})
		e.prep.modelGate = childGate
		_ = launchChildThroughRuntime(t, r, parent, "op-live-child")
		awaitModelArrival(t, e)
		if children := snapshotThroughRuntime(t, r, parent).Background; len(children) == 0 {
			t.Fatal("parent background membership is empty after a live child launch")
		}
		if _, err := r.revertSessionCode(context.Background(), parent, "op-parent"); !errors.Is(err, harness.ErrConflict) {
			t.Fatalf("live-child restore = %v, want harness.ErrConflict", err)
		}
		if data, err := os.ReadFile(parentFile); err != nil || string(data) != "p1" {
			t.Fatalf("live-child refusal mutated the file: (%q, %v)", data, err)
		}
		close(childGate)
		e.prep.awaitCleanups(3)
	})
}

// --- concurrent admission and partial traversal ---

// TestCodeRevertConcurrentAdmission proves restore holds no reservation with a
// test-owned FIFO saved original: the restore parks on the FIFO reader, a
// gated Operation is admitted while it is parked, and the later-visited bad
// group returns the partial result after the parked group's restore. No
// production hook or CodeStore change backs the fixture.
func TestCodeRevertConcurrentAdmission(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "fifo-ws")
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		first := submitCodeOperation(t, r, session, "op-first", "first")
		second := submitCodeOperation(t, r, session, "op-second", "second")
		awaitRestorableSession(t, r, session)

		// The later-visited (earlier-admitted) group fails on a corrupt meta
		// after the earlier-visited (later-admitted) group restores.
		badEntryDir := filepath.Join(codeGroupRoot(r, session, first.Admission.AdmittedEntry.EntryID), "snapshots", "1", "aaaaaaaaaaaaaaaa")
		if err := os.MkdirAll(badEntryDir, 0o700); err != nil {
			t.Fatalf("mkdir bad entry: %v", err)
		}
		if err := os.WriteFile(filepath.Join(badEntryDir, "meta.json"), []byte("{"), 0o600); err != nil {
			t.Fatalf("write bad meta: %v", err)
		}

		target := filepath.Join(workspace, "parked.txt")
		if err := os.WriteFile(target, []byte("current"), 0o600); err != nil {
			t.Fatalf("write parked target: %v", err)
		}
		fifo := seedFIFOCodeEntry(t, codeGroupRoot(r, session, second.Admission.AdmittedEntry.EntryID), target, "current")

		gate := make(chan struct{})
		var gateOnce sync.Once
		releaseGate := func() { gateOnce.Do(func() { close(gate) }) }
		e.prep.modelGate = gate
		var writerFD = -1
		writerReleased := false
		defer func() {
			if writerFD >= 0 {
				_ = unix.Close(writerFD)
				return
			}
			if !writerReleased { // a failed row must still release any parked reader
				go func() {
					localFD, err := unix.Open(fifo, unix.O_WRONLY, 0)
					if err == nil {
						_, _ = unix.Write(localFD, []byte("released"))
						_ = unix.Close(localFD)
					}
				}()
			}
		}()
		defer releaseGate()

		type outcome struct {
			result protocol.CodeRevertResult
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := r.revertSessionCode(context.Background(), session, "op-first")
			done <- outcome{result: result, err: err}
		}()

		// The nonblocking writer open succeeds only once the restore reader
		// is already parked in restoreFile's os.Open.
		deadline := time.Now().Add(10 * time.Second)
		for {
			fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0)
			if err == nil {
				writerFD = fd
				break
			}
			if !errors.Is(err, unix.ENXIO) {
				t.Fatalf("FIFO writer open: %v", err)
			}
			if time.Now().After(deadline) {
				t.Fatal("the restore never parked on the FIFO saved original")
			}
			time.Sleep(5 * time.Millisecond)
		}

		// Admission proceeds while the restore is parked: a reservation would
		// hold this Submit behind the filesystem work.
		admitted := make(chan error, 1)
		go func() {
			admitted <- r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
				res, err := h.Submit(ctx, harness.SubmitRequest{
					SessionID:   session,
					OperationID: "op-admit",
					Origin:      harness.InputOriginUser,
					Content:     []model.ContentPart{{Kind: model.PartText, Text: "while parked"}},
					Mode:        harness.MessageModeRegular,
				})
				if err != nil {
					return err
				}
				if res.Disposition != harness.DispositionAdmitted {
					return fmt.Errorf("disposition %q, want admitted", res.Disposition)
				}
				return nil
			})
		}()
		select {
		case err := <-admitted:
			if err != nil {
				t.Fatalf("admission during a parked restore: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("admission blocked during a parked restore: the restore is not a reservation")
		}

		if _, err := unix.Write(writerFD, []byte("restored")); err != nil {
			t.Fatalf("FIFO write: %v", err)
		}
		if err := unix.Close(writerFD); err != nil {
			t.Fatalf("FIFO close: %v", err)
		}
		writerFD = -1
		writerReleased = true

		var out outcome
		select {
		case out = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the restore never returned after its parked group completed")
		}
		if out.err == nil {
			t.Fatalf("restore = %+v, want the later bad group's traversal error", out.result)
		}
		if len(out.result.Restored) != 1 || out.result.Restored[0] != target || out.result.Skipped == nil {
			t.Fatalf("partial result = %+v, want the parked group's accumulated restore and a non-nil skip list", out.result)
		}
		if out.result.Error == nil || out.result.Error.Code != protocol.Internal || out.result.Error.Message == "" || out.result.Error.SessionId != nil {
			t.Fatalf("partial error = %+v, want the internal class with no target Session id", out.result.Error)
		}
		if data, err := os.ReadFile(target); err != nil || string(data) != "restored" {
			t.Fatalf("parked target = (%q, %v), want the FIFO restore to complete", data, err)
		}
		releaseGate()
		awaitOperation(t, r, session, "op-admit", harness.OperationSuccess)
		awaitIdleSession(t, r, session)
	})
}

// seedFIFOCodeEntry writes one valid modern snapshot entry whose saved
// original is a FIFO, and returns the FIFO path. The canonical path and
// last-write hash mirror the CodeStore's own proof so the retained restore
// reaches restoreFile's plain os.Open.
func seedFIFOCodeEntry(t *testing.T, groupRoot, target, content string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", target, err)
	}
	entryDir := filepath.Join(groupRoot, "snapshots", "1", sha256Hex16(canonical))
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatalf("mkdir FIFO entry: %v", err)
	}
	fifo := filepath.Join(entryDir, "original")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo(%s): %v", fifo, err)
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
	writeRetainedJSON(t, filepath.Join(dir, "meta.json"), snapshot.SessionMeta{
		ID:          legacyID,
		CreatedAt:   "2026-01-01T00:00:00Z",
		ProjectPath: normalized,
		ProjectHash: hash[:16],
		State:       snapshot.StateActive,
	})
	writeServiceFile(t, filepath.Join(dir, "turns", "1", "messages.jsonl"), retainedConversationCanary)
	writeServiceFile(t, filepath.Join(dir, "claims", "claim.json"), retainedClaimCanary)
	return dir
}

func writeRetainedJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	writeServiceFile(t, path, string(data))
}

func rewriteRetainedMeta(t *testing.T, sessionDir string, edit func(*snapshot.SessionMeta)) {
	t.Helper()
	path := filepath.Join(sessionDir, "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var meta snapshot.SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	edit(&meta)
	writeRetainedJSON(t, path, meta)
}

// recordRetainedMutation drives one real CodeStore capture at the given turn
// of a pruned legacy session directory.
func recordRetainedMutation(t *testing.T, sessionDir string, turn int, display, target string, content []byte) {
	t.Helper()
	store, err := snapshot.OpenCodeStore(sessionDir)
	if err != nil {
		t.Fatalf("OpenCodeStore(%s): %v", sessionDir, err)
	}
	entryID, _, err := store.SnapshotResolvedEntry(turn, display, target)
	if err != nil {
		t.Fatalf("SnapshotResolvedEntry turn %d: %v", turn, err)
	}
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", target, err)
	}
	if err := store.RecordSnapshotContent(turn, entryID, content); err != nil {
		t.Fatalf("RecordSnapshotContent turn %d: %v", turn, err)
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
		if err != nil || string(data) != want {
			t.Fatalf("canary %s = (%q, %v), want untouched %q", path, data, err, want)
		}
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
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		legacyID := "deadbeef"
		home := r.config.loader.Home()
		sessionDir := seedRetainedSession(t, home, workspace, legacyID)

		turns, err := r.listRetainedCodeSnapshots(context.Background(), workspace, legacyID)
		if err != nil || turns.Turns == nil || len(turns.Turns) != 0 {
			t.Fatalf("empty retained list = (%+v, %v), want a non-nil empty list", turns, err)
		}
		result, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID, AfterTurn: 3})
		if err != nil || result.Restored == nil || result.Skipped == nil || len(result.Restored) != 0 || len(result.Skipped) != 0 {
			t.Fatalf("empty retained revert = (%+v, %v), want non-nil empty success", result, err)
		}

		if _, err := r.listRetainedCodeSnapshots(context.Background(), "", legacyID); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("empty workspace = %v, want harness.ErrInvalid", err)
		}
		for _, id := range []string{"deadbee", "deadbeef0", "DEADBEEF", "deadbeez"} {
			if _, err := r.listRetainedCodeSnapshots(context.Background(), workspace, id); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("legacy id %q = %v, want harness.ErrInvalid", id, err)
			}
		}
		if _, err := r.listRetainedCodeSnapshots(context.Background(), workspace, "0123456789abcdef0123456789abcdef"); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("32-hex target id into retained route = %v, want harness.ErrInvalid", err)
		}
		if _, err := r.listRetainedCodeSnapshots(context.Background(), workspace, "abcdef01"); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("missing legacy session = %v, want harness.ErrNotFound", err)
		}
		if _, err := r.listSessionCodeSnapshots(context.Background(), legacyID); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("8-hex id into target route = %v, want harness.ErrInvalid", err)
		}

		for _, tc := range []struct {
			name string
			edit func(*snapshot.SessionMeta)
		}{
			{"wrong id", func(m *snapshot.SessionMeta) { m.ID = "deadbeee" }},
			{"wrong project path", func(m *snapshot.SessionMeta) { m.ProjectPath = filepath.Join(workspace, "other") }},
			{"wrong hash prefix", func(m *snapshot.SessionMeta) { m.ProjectHash = "0000000000000000" }},
		} {
			metaPath := filepath.Join(sessionDir, "meta.json")
			original, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatalf("read original meta: %v", err)
			}
			rewriteRetainedMeta(t, sessionDir, tc.edit)
			if _, err := r.listRetainedCodeSnapshots(context.Background(), workspace, legacyID); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("%s = %v, want harness.ErrInvalid", tc.name, err)
			}
			if _, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID}); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("%s revert = %v, want harness.ErrInvalid", tc.name, err)
			}
			writeServiceFile(t, metaPath, string(original))
		}
		if _, err := os.Stat(sessionDir); err != nil {
			t.Fatalf("retained session dir vanished: %v", err)
		}
		assertCanariesUntouched(t, sessionDir)

		// A large ignored legacy field is still one valid JSON meta document:
		// the proof reads the complete regular file and ignores unknown
		// members, so list and revert succeed with empty snapshots. The
		// malformed sibling stays refused.
		metaPath := filepath.Join(sessionDir, "meta.json")
		original, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("read original meta: %v", err)
		}
		normalized := filepath.Clean(workspace)
		sum := sha256.Sum256([]byte(normalized))
		hash := hex.EncodeToString(sum[:])
		large, err := json.Marshal(map[string]any{
			"id":                   legacyID,
			"created_at":           "2026-01-01T00:00:00Z",
			"project_path":         normalized,
			"project_hash":         hash[:16],
			"state":                snapshot.StateActive,
			"legacy_ignored_field": strings.Repeat("x", (1<<20)+1),
		})
		if err != nil {
			t.Fatalf("marshal large meta: %v", err)
		}
		writeServiceFile(t, metaPath, string(large))
		if turns, err := r.listRetainedCodeSnapshots(context.Background(), workspace, legacyID); err != nil || len(turns.Turns) != 0 {
			t.Fatalf("large ignored meta field list = (%+v, %v), want the proven empty success", turns, err)
		}
		if result, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID}); err != nil || len(result.Restored) != 0 {
			t.Fatalf("large ignored meta field revert = (%+v, %v), want the proven empty success", result, err)
		}
		writeServiceFile(t, metaPath, "{ not a json document")
		if _, err := r.listRetainedCodeSnapshots(context.Background(), workspace, legacyID); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("malformed meta list = %v, want harness.ErrInvalid", err)
		}
		if _, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("malformed meta revert = %v, want harness.ErrInvalid", err)
		}
		writeServiceFile(t, metaPath, string(original))

		// The full-hash directory is exact: a same-ID session under a wrong
		// project directory is not found.
		wrongRoot := filepath.Join(e.home, "other-ws")
		if _, err := r.listRetainedCodeSnapshots(context.Background(), wrongRoot, legacyID); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("wrong workspace root = %v, want harness.ErrNotFound", err)
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
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		legacyID := "cafebabe"
		home := r.config.loader.Home()
		sessionDir := seedRetainedSession(t, home, workspace, legacyID)

		file := filepath.Join(workspace, "legacy.txt")
		if err := os.WriteFile(file, []byte("a0"), 0o600); err != nil {
			t.Fatalf("write turn1 preimage: %v", err)
		}
		recordRetainedMutation(t, sessionDir, 1, file, file, []byte("a1"))
		created := filepath.Join(workspace, "legacy-created.txt")
		recordRetainedMutation(t, sessionDir, 2, file, file, []byte("a2"))
		recordRetainedMutation(t, sessionDir, 2, created, created, []byte("c"))

		turns, err := r.listRetainedCodeSnapshots(context.Background(), workspace, legacyID)
		if err != nil {
			t.Fatalf("listRetainedCodeSnapshots: %v", err)
		}
		if len(turns.Turns) != 2 || turns.Turns[0].Turn != 1 || turns.Turns[1].Turn != 2 {
			t.Fatalf("turns = %+v, want the retained order 1, 2", turns.Turns)
		}
		if len(turns.Turns[0].Files) != 1 || turns.Turns[0].Files[0] != (protocol.SnapshotFile{Path: file, Existed: true}) {
			t.Fatalf("turn 1 files = %+v, want the edited display path", turns.Turns[0].Files)
		}
		if len(turns.Turns[1].Files) != 2 {
			t.Fatalf("turn 2 files = %+v, want the edited and created entries", turns.Turns[1].Files)
		}
		assertCanariesUntouched(t, sessionDir)

		result, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID, AfterTurn: 1})
		if err != nil {
			t.Fatalf("revert after_turn 1: %v", err)
		}
		if len(result.Restored) != 2 || result.Skipped == nil || len(result.Skipped) != 0 {
			t.Fatalf("turn 2 result = %+v, want both files restored without skips", result)
		}
		if data, err := os.ReadFile(file); err != nil || string(data) != "a1" {
			t.Fatalf("legacy file after turn 2 revert = (%q, %v), want the turn 1 post-write", data, err)
		}
		if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created file stat = %v, want restored absence", err)
		}

		result, err = r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID})
		if err != nil {
			t.Fatalf("revert after_turn 0: %v", err)
		}
		if len(result.Restored) != 1 || result.Restored[0] != file {
			t.Fatalf("turn 1 result = %+v, want the edited file restored", result)
		}
		if data, err := os.ReadFile(file); err != nil || string(data) != "a0" {
			t.Fatalf("legacy file after turn 1 revert = (%q, %v), want the turn 1 preimage", data, err)
		}
		assertCanariesUntouched(t, sessionDir)

		// A re-recorded turn 1 with an external edit skips instead of
		// overwriting, still reporting the retained display path.
		recordRetainedMutation(t, sessionDir, 1, file, file, []byte("a1"))
		if err := os.WriteFile(file, []byte("external"), 0o600); err != nil {
			t.Fatalf("external edit: %v", err)
		}
		result, err = r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID})
		if err != nil {
			t.Fatalf("skip revert: %v", err)
		}
		if len(result.Restored) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Path != file || !strings.Contains(result.Skipped[0].Reason, "content changed") {
			t.Fatalf("skip result = %+v, want the changed display path skipped", result)
		}
		if data, err := os.ReadFile(file); err != nil || string(data) != "external" {
			t.Fatalf("legacy file after skip = (%q, %v), want the external content", data, err)
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
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		legacyID := "feedface"
		home := r.config.loader.Home()
		sessionDir := seedRetainedSession(t, home, workspace, legacyID)

		file := filepath.Join(workspace, "partial.txt")
		if err := os.WriteFile(file, []byte("p0"), 0o600); err != nil {
			t.Fatalf("write preimage: %v", err)
		}
		recordRetainedMutation(t, sessionDir, 1, file, file, []byte("p1"))
		recordRetainedMutation(t, sessionDir, 2, file, file, []byte("p2"))

		// Corrupt turn 1's meta after recording: turn 2 restores first, then
		// turn 1 errors.
		turnDir := filepath.Join(sessionDir, "snapshots", "1")
		entries, err := os.ReadDir(turnDir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("turn 1 dirs = (%d, %v), want one entry", len(entries), err)
		}
		if err := os.WriteFile(filepath.Join(turnDir, entries[0].Name(), "meta.json"), []byte("{"), 0o600); err != nil {
			t.Fatalf("corrupt meta: %v", err)
		}

		result, err := r.revertRetainedCode(context.Background(), protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID})
		if err == nil {
			t.Fatalf("partial revert = %+v, want the traversal error", result)
		}
		if len(result.Restored) != 1 || result.Restored[0] != file || result.Skipped == nil {
			t.Fatalf("partial result = %+v, want the later turn's accumulated restore", result)
		}
		if result.Error == nil || result.Error.Code != protocol.Internal || result.Error.Message == "" || result.Error.SessionId != nil {
			t.Fatalf("partial error = %+v, want the internal class with no target Session id", result.Error)
		}
		if data, err := os.ReadFile(file); err != nil || string(data) != "p1" {
			t.Fatalf("partial file = (%q, %v), want the completed later-turn restore", data, err)
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
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatalf("mkdir docs: %v", err)
		}
		if err := os.WriteFile(target, []byte("hello"), 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		canonical, err := filepath.EvalSymlinks(target)
		if err != nil {
			t.Fatalf("EvalSymlinks: %v", err)
		}

		relative, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: filepath.Join("docs", "note.txt")})
		if err != nil || relative.Content != "hello" || relative.CanonicalPath != canonical || relative.Truncated {
			t.Fatalf("relative read = (%+v, %v), want the contained canonical view", relative, err)
		}
		absolute, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: target})
		if err != nil || absolute.CanonicalPath != canonical {
			t.Fatalf("absolute read = (%+v, %v), want the same canonical target", absolute, err)
		}
		cleaned, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: filepath.Join("docs", "..", "docs", "note.txt")})
		if err != nil || cleaned.Content != "hello" {
			t.Fatalf("cleaned read = (%+v, %v), want the contained target", cleaned, err)
		}

		outside := filepath.Join(e.home, "outside.txt")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatalf("write outside: %v", err)
		}
		if _, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: filepath.Join("..", "outside.txt")}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("dotdot read = %v, want harness.ErrInvalid", err)
		}
		if _, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: outside}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("absolute outside read = %v, want harness.ErrInvalid", err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
			t.Fatalf("symlink escape: %v", err)
		}
		if _, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "escape"}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("outside symlink read = %v, want harness.ErrInvalid", err)
		}
		if err := os.Symlink(target, filepath.Join(workspace, "inside-link")); err != nil {
			t.Fatalf("symlink inside: %v", err)
		}
		linked, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "inside-link"})
		if err != nil || linked.Content != "hello" || linked.CanonicalPath != canonical {
			t.Fatalf("inside symlink read = (%+v, %v), want the resolved contained target", linked, err)
		}
		for _, req := range []protocol.ReadFileRequest{
			{Path: "note.txt"},
			{Workspace: workspace},
		} {
			if _, err := r.readWorkspaceFile(context.Background(), req); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("empty member %+v = %v, want harness.ErrInvalid", req, err)
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
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		for _, sensitive := range []string{".env", "id_rsa", "server.pem"} {
			if err := os.WriteFile(filepath.Join(workspace, sensitive), []byte("secret"), 0o600); err != nil {
				t.Fatalf("write %s: %v", sensitive, err)
			}
			if _, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: sensitive}); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("sensitive %q = %v, want harness.ErrInvalid", sensitive, err)
			}
		}

		original := filepath.Join(workspace, "original.txt")
		if err := os.WriteFile(original, []byte("linked"), 0o600); err != nil {
			t.Fatalf("write original: %v", err)
		}
		hardlink := filepath.Join(workspace, "hardlink.txt")
		if err := os.Link(original, hardlink); err != nil {
			t.Fatalf("hardlink: %v", err)
		}
		if _, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "hardlink.txt"}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("hardlink read = %v, want harness.ErrInvalid", err)
		}

		fifo := filepath.Join(workspace, "pipe")
		if err := unix.Mkfifo(fifo, 0o600); err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
		if _, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "pipe"}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("fifo read = %v, want harness.ErrInvalid", err)
		}
		if _, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "."}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("directory read = %v, want harness.ErrInvalid", err)
		}
		if _, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "missing.txt"}); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("missing read = %v, want harness.ErrNotFound", err)
		}
	})
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
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}

		write := func(name string, data []byte) string {
			t.Helper()
			path := filepath.Join(workspace, name)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			return path
		}

		write("binary.bin", []byte{'A', 0xff, 0xfe, 'B', 0x80, 0x80, 'C'})
		binary, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "binary.bin"})
		if err != nil || binary.Content != "A\uFFFDB\uFFFDC" || binary.Truncated {
			t.Fatalf("binary view = (%+v, %v), want the maximal invalid-run replacements", binary, err)
		}

		write("exact.txt", bytes.Repeat([]byte("a"), maxViewPrefixBytes))
		exact, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "exact.txt"})
		if err != nil || exact.Truncated || len(exact.Content) != maxViewPrefixBytes {
			t.Fatalf("exact view = (%d bytes, truncated %v, err %v), want the untruncated 1 MiB", len(exact.Content), exact.Truncated, err)
		}

		large := append(bytes.Repeat([]byte("a"), maxViewPrefixBytes), []byte("tail")...)
		write("large.txt", large)
		view, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "large.txt"})
		if err != nil || !view.Truncated || len(view.Content) != maxViewPrefixBytes || strings.ContainsRune(view.Content, '\uFFFD') {
			t.Fatalf("large view = (%d bytes, truncated %v, err %v), want exactly the first raw MiB", len(view.Content), view.Truncated, err)
		}

		// Truncation precedes conversion: the cap cuts the three-byte rune
		// after its first byte, so the dangling run becomes one replacement.
		midRune := append(bytes.Repeat([]byte("a"), maxViewPrefixBytes-1), []byte("\u20AC extra")...)
		write("mid-rune.txt", midRune)
		cut, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "mid-rune.txt"})
		if err != nil || !cut.Truncated || cut.Content != strings.Repeat("a", maxViewPrefixBytes-1)+"\uFFFD" {
			t.Fatalf("mid-rune view = (%q..., truncated %v, err %v), want the raw cap converted after truncation", cut.Content, cut.Truncated, err)
		}

		// No second converted-length cap: alternating invalid runs convert to
		// four bytes per two raw bytes, so the returned content is 2 MiB for
		// the 1 MiB raw prefix; a wire-length cap would cut it back to 1 MiB.
		pattern := bytes.Repeat([]byte{0xff, 'A'}, maxViewPrefixBytes/2)
		write("invalid.bin", append(pattern, 0x00))
		expanded, err := r.readWorkspaceFile(context.Background(), protocol.ReadFileRequest{Workspace: workspace, Path: "invalid.bin"})
		if err != nil || !expanded.Truncated || expanded.Content != strings.Repeat("\uFFFDA", maxViewPrefixBytes/2) {
			t.Fatalf("expanded view = (%d bytes, truncated %v, err %v), want 2 MiB of converted raw prefix without a wire cap", len(expanded.Content), expanded.Truncated, err)
		}
	})
}

// --- closure ---

// TestCodeProducersClosedOwner proves every deferred-code and viewer producer
// joins the existing admission gate: after Close each one returns ErrClosed
// without touching storage or the filesystem.
func TestCodeProducersClosedOwner(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		ctx := context.Background()
		sessionID := "0123456789abcdef0123456789abcdef"
		workspace := filepath.Join(e.home, "closed-ws")
		if _, err := r.listSessionCodeSnapshots(ctx, sessionID); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed list = %v, want ErrClosed", err)
		}
		if _, err := r.revertSessionCode(ctx, sessionID, "op"); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed revert = %v, want ErrClosed", err)
		}
		if _, err := r.listRetainedCodeSnapshots(ctx, workspace, "deadbeef"); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed retained list = %v, want ErrClosed", err)
		}
		if _, err := r.revertRetainedCode(ctx, protocol.RetainedRevertRequest{Workspace: workspace, SessionId: "deadbeef"}); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed retained revert = %v, want ErrClosed", err)
		}
		if _, err := r.readWorkspaceFile(ctx, protocol.ReadFileRequest{Workspace: workspace, Path: "x"}); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed viewer = %v, want ErrClosed", err)
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
