package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// spawnRecord wraps one spawn callback with a plain call counter that also
// keeps the received completion identities in call order.
func spawnRecord(calls *[]string, err error) func(context.Context, string) error {
	return func(_ context.Context, completionID string) error {
		*calls = append(*calls, completionID)
		return err
	}
}

// launchRequest is the launch fixtures' default valid request over the
// freshSessionStore parent.
func launchRequest() LaunchChildRequest {
	return LaunchChildRequest{
		ParentSessionID: testSessionID,
		AgentType:       "child-agent",
		Content:         admissionContent("child task"),
		OperationID:     "child-op-1",
		MaxConcurrent:   2,
		OutputLimit:     1024,
	}
}

// advanceSessionRevision advances one Session's durable register revision once
// without changing its payload: the storage-side foreign writer.
func advanceSessionRevision(t *testing.T, store *graphStorage, sessionID string) {
	t.Helper()
	err := store.Transact(context.Background(), func(tx Transaction) error {
		key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		_, err = tx.ReplaceRegister(key, reg.Revision, reg.Payload)
		return err
	})
	if err != nil {
		t.Fatalf("foreign revision advance: %v", err)
	}
}

// TestStartJobRejectsBeforeSpawn proves every StartJob rejection happens
// before the spawn callback runs: the identity, dependency, and callback
// inputs first, then the archived and closed group states and a canceled
// Harness — each with its existing error class and no member created.
func TestStartJobRejectsBeforeSpawn(t *testing.T) {
	var spawned []string
	spawn := spawnRecord(&spawned, nil)
	cases := []struct {
		name    string
		jobID   string
		jobs    JobStopper
		spawn   func(context.Context, string) error
		wantErr func(t *testing.T, err error)
	}{
		{"wrong length", "abc", stubJobStopper{}, spawn, func(t *testing.T, err error) {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("wrong-length job id = %v, want ErrInvalid", err)
			}
		}},
		{"non-hex", "zzzzzzzz", stubJobStopper{}, spawn, func(t *testing.T, err error) {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("non-hex job id = %v, want ErrInvalid", err)
			}
		}},
		{"uppercase", "DEADBEEF", stubJobStopper{}, spawn, func(t *testing.T, err error) {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("uppercase job id = %v, want ErrInvalid", err)
			}
		}},
		{"nil stopper", jobFixtureID, nil, spawn, func(t *testing.T, err error) {
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no job stopper configured") {
				t.Fatalf("nil stopper = %v, want the no-job-stopper rejection", err)
			}
		}},
		{"nil spawn", jobFixtureID, stubJobStopper{}, nil, func(t *testing.T, err error) {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("nil spawn = %v, want ErrInvalid", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHarness(t, freshSessionStore(t), nil)
			h.deps.Jobs = tc.jobs
			err := h.StartJob(context.Background(), testSessionID, tc.jobID, tc.spawn)
			if err == nil {
				t.Fatalf("StartJob succeeded, want a rejection")
			}
			tc.wantErr(t, err)
			if len(spawned) != 0 {
				t.Fatalf("the rejected start spawned %d times", len(spawned))
			}
		})
	}

	t.Run("archived parent", func(t *testing.T) {
		archived := validSessionRecord()
		now := time.Now().UTC()
		archived.State.Lifecycle = LifecycleArchived
		archived.State.ArchivedAt = &now
		h := newTestHarness(t, (&testGraph{session: archived}).storage(t), nil)
		h.deps.Jobs = stubJobStopper{}
		err := h.StartJob(context.Background(), testSessionID, jobFixtureID, spawn)
		want := invalidInput("session %q is archived; admission requires an open Session", testSessionID)
		if err == nil || err.Error() != want.Error() {
			t.Fatalf("archived start = %v, want %v", err, want)
		}
		if len(spawned) != 0 {
			t.Fatalf("the archived start spawned")
		}
	})

	t.Run("closed group", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), nil)
		h.deps.Jobs = stubJobStopper{}
		c, err := h.coordinatorFor(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("coordinatorFor: %v", err)
		}
		c.mu.Lock()
		c.bgState = bgStopping
		c.mu.Unlock()
		if err := h.StartJob(context.Background(), testSessionID, jobFixtureID, spawn); err == nil ||
			err.Error() != "harness: invalid storage request: background group is closed or stopping" {
			t.Fatalf("closed-group start = %v, want the closed-or-stopping rejection", err)
		}
		if len(spawned) != 0 {
			t.Fatalf("the closed-group start spawned")
		}
		c.mu.Lock()
		hasGroup := c.group != nil
		c.mu.Unlock()
		if hasGroup {
			t.Fatalf("the rejected start created a member")
		}
	})

	t.Run("canceled harness", func(t *testing.T) {
		hctx, cancel := context.WithCancel(context.Background())
		h, err := New(hctx, Dependencies{
			Storage: freshSessionStore(t),
			Prepare: func(context.Context, PreparationRequest) (PreparedExecution, error) {
				return PreparedExecution{}, errors.New("no preparation configured for this test")
			},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		h.deps.Jobs = stubJobStopper{}
		cancel()
		if err := h.StartJob(context.Background(), testSessionID, jobFixtureID, spawn); !errors.Is(err, context.Canceled) || err.Error() != "context canceled" {
			t.Fatalf("canceled start = %v, want the raw context.Canceled", err)
		}
		if len(spawned) != 0 {
			t.Fatalf("the canceled start spawned")
		}
	})
}

// TestStartJobHandoffLossFinishesMemberOnce proves the handoff failure
// capture: every state outside the valid handoff set fails uniformly — the
// raw harness error when the Harness is closed, the closed-or-stopping
// rejection otherwise — and the member claim-finishes exactly once.
func TestStartJobHandoffLossFinishesMemberOnce(t *testing.T) {
	t.Run("closed group rejects with the closed-or-stopping text", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), nil)
		c, err := h.coordinatorFor(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("coordinatorFor: %v", err)
		}
		member, err := admitLocked(h, c, memberJob, jobFixtureID, 0)
		if err != nil {
			t.Fatalf("admission: %v", err)
		}
		c.mu.Lock()
		c.bgState = bgStopping
		c.mu.Unlock()
		if err := h.backgroundHandoffFailure(c, member); err == nil ||
			err.Error() != "harness: invalid storage request: background group is closed or stopping" {
			t.Fatalf("handoff loss = %v, want the closed-or-stopping rejection", err)
		}
		h.claimFinishBackgroundMember(c, member.completionID)
		waitMemberDone(t, member)
		c.mu.Lock()
		empty := c.group == nil || len(c.group.members) == 0
		c.mu.Unlock()
		if !empty {
			t.Fatalf("the finished member stayed in the group")
		}
		h.claimFinishBackgroundMember(c, member.completionID) // a second ending is a no-op
	})

	t.Run("closed harness rejects with the raw context error", func(t *testing.T) {
		hctx, cancel := context.WithCancel(context.Background())
		h, err := New(hctx, Dependencies{
			Storage: freshSessionStore(t),
			Prepare: func(context.Context, PreparationRequest) (PreparedExecution, error) {
				return PreparedExecution{}, errors.New("no preparation configured for this test")
			},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		c, err := h.coordinatorFor(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("coordinatorFor: %v", err)
		}
		member, err := admitLocked(h, c, memberJob, jobFixtureID, 0)
		if err != nil {
			t.Fatalf("admission: %v", err)
		}
		cancel() // the Harness closes after the admission, before the handoff
		if err := h.backgroundHandoffFailure(c, member); !errors.Is(err, context.Canceled) || err.Error() != "context canceled" {
			t.Fatalf("handoff loss = %v, want the raw context.Canceled", err)
		}
	})
}

// TestStartJobSpawnFailureFinishesMemberOnce proves a failed spawn returns its
// error unchanged and releases the member exactly once: the spawn transferred
// no live completion obligation.
func TestStartJobSpawnFailureFinishesMemberOnce(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	h.deps.Jobs = stubJobStopper{}
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	var spawned []string
	spawn := spawnRecord(&spawned, errors.New("spawn down"))
	if err := h.StartJob(context.Background(), testSessionID, jobFixtureID, spawn); err == nil || err.Error() != "spawn down" {
		t.Fatalf("spawn failure = %v, want the unchanged spawn error", err)
	}
	if len(spawned) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(spawned))
	}
	if !completionIDShape.MatchString(spawned[0]) {
		t.Fatalf("spawned completion id %q is not 32 lowercase hex", spawned[0])
	}
	c.mu.Lock()
	empty := c.group == nil || len(c.group.members) == 0
	c.mu.Unlock()
	if !empty {
		t.Fatalf("the failed spawn left the member live")
	}
}

// TestStartJobDrivesSpawnThroughAdmission proves the success path: StartJob
// admits a job member, passes the handoff, and hands the reserved completion
// identity to the spawn; the member stays live for the delivery.
func TestStartJobDrivesSpawnThroughAdmission(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	h.deps.Jobs = stubJobStopper{}
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	var spawned []string
	spawn := spawnRecord(&spawned, nil)
	if err := h.StartJob(context.Background(), testSessionID, jobFixtureID, spawn); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if len(spawned) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(spawned))
	}
	if !completionIDShape.MatchString(spawned[0]) {
		t.Fatalf("spawned completion id %q is not 32 lowercase hex", spawned[0])
	}
	c.mu.Lock()
	member := c.group.members[spawned[0]]
	c.mu.Unlock()
	if member == nil || member.kind != memberJob || member.id != jobFixtureID || member.claimed {
		t.Fatalf("admitted member = %+v, want the unclaimed job member", member)
	}
	h.claimFinishBackgroundMember(c, member.completionID) // test hygiene
	waitMemberDone(t, member)
}

// TestLaunchChildSessionRejectsInvalidRequests proves the input validation:
// the cap and output-limit ranges, the parent identity, and the submitted
// inputs reject before any preparation or member exists.
func TestLaunchChildSessionRejectsInvalidRequests(t *testing.T) {
	stub := newPrepareStub(validPrepared())
	h := newTestHarness(t, freshSessionStore(t), stub.prepare)
	cases := []struct {
		name string
		mut  func(r *LaunchChildRequest)
	}{
		{"zero cap", func(r *LaunchChildRequest) { r.MaxConcurrent = 0 }},
		{"cap above 20", func(r *LaunchChildRequest) { r.MaxConcurrent = 21 }},
		{"output limit below 1024", func(r *LaunchChildRequest) { r.OutputLimit = 1023 }},
		{"output limit above 1048576", func(r *LaunchChildRequest) { r.OutputLimit = 1048577 }},
		{"non-hex parent", func(r *LaunchChildRequest) { r.ParentSessionID = "parent" }},
		{"empty operation id", func(r *LaunchChildRequest) { r.OperationID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := launchRequest()
			tc.mut(&req)
			if _, err := h.LaunchChildSession(context.Background(), req); !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s = %v, want ErrInvalid", tc.name, err)
			}
		})
	}
	if stub.callCount() != 0 {
		t.Fatalf("the rejected launches prepared %d times", stub.callCount())
	}
}

// TestLaunchChildSessionRefusesChildParent proves the recursion rule: a child
// Session cannot launch a child.
func TestLaunchChildSessionRefusesChildParent(t *testing.T) {
	childParent := validSessionRecord()
	childParent.Identity.ParentSessionID = hexID(2)
	stub := newPrepareStub(validPrepared())
	h := newTestHarness(t, (&testGraph{session: childParent}).storage(t), stub.prepare)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	if _, err := h.LaunchChildSession(context.Background(), launchRequest()); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "child sessions cannot launch child sessions") {
		t.Fatalf("child-parent launch = %v, want the refusal", err)
	}
	if stub.callCount() != 0 {
		t.Fatalf("the refused launch prepared")
	}
	c.mu.Lock()
	hasGroup := c.group != nil
	c.mu.Unlock()
	if hasGroup {
		t.Fatalf("the refused launch created a member")
	}
}

// TestLaunchChildSessionArchivedParent proves the archived-parent rejection:
// admission requires an open parent and creates no member or child.
func TestLaunchChildSessionArchivedParent(t *testing.T) {
	archived := validSessionRecord()
	now := time.Now().UTC()
	archived.State.Lifecycle = LifecycleArchived
	archived.State.ArchivedAt = &now
	stub := newPrepareStub(validPrepared())
	h := newTestHarness(t, (&testGraph{session: archived}).storage(t), stub.prepare)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	_, err = h.LaunchChildSession(context.Background(), launchRequest())
	want := invalidInput("session %q is archived; admission requires an open Session", testSessionID)
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("archived launch = %v, want %v", err, want)
	}
	c.mu.Lock()
	hasGroup := c.group != nil
	c.mu.Unlock()
	if hasGroup {
		t.Fatalf("the rejected launch created a member")
	}
	if n := durableRegisterCount(fixtureStore(t, h)); n != 1 {
		t.Fatalf("durable registers = %d, want only the parent session register", n)
	}
}

// fixtureStore returns the graphStorage behind a harness built by newTestHarness.
func fixtureStore(t *testing.T, h *Harness) *graphStorage {
	t.Helper()
	store, ok := h.deps.Storage.(*graphStorage)
	if !ok {
		t.Fatalf("the fixture harness does not use a graphStorage")
	}
	return store
}

// durableRegisterCount reads the store's total durable register count across
// every Session.
func durableRegisterCount(store *graphStorage) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	n := 0
	for _, regs := range store.registers {
		n += len(regs)
	}
	return n
}

// TestLaunchChildSessionCapRejection proves the child concurrency cap: at the
// requested cap the launch is rejected before any member or child exists.
func TestLaunchChildSessionCapRejection(t *testing.T) {
	stub := newPrepareStub(validPrepared())
	h := newTestHarness(t, freshSessionStore(t), stub.prepare)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	if _, err := admitLocked(h, c, memberChild, "existing-child", 0); err != nil {
		t.Fatalf("pre-admission: %v", err)
	}
	req := launchRequest()
	req.MaxConcurrent = 1
	_, err = h.LaunchChildSession(context.Background(), req)
	want := "harness: invalid storage request: background group is full (1/1). Wait for a background member to complete, then retry"
	if err == nil || err.Error() != want {
		t.Fatalf("capped launch = %v, want %q", err, want)
	}
	c.mu.Lock()
	members := len(c.group.members)
	c.mu.Unlock()
	if members != 1 {
		t.Fatalf("group holds %d members, want only the pre-admitted one", members)
	}
	if n := durableRegisterCount(fixtureStore(t, h)); n != 1 {
		t.Fatalf("the rejected launch published %d registers, want only the parent", n)
	}
}

// TestLaunchChildSessionPrepareFailure proves a preparation failure publishes
// neither child nor member: the no-bypass sibling of the shared preparation
// path.
func TestLaunchChildSessionPrepareFailure(t *testing.T) {
	stub := newPrepareStub(PreparedExecution{})
	stub.err = errors.New("prepare down")
	h := newTestHarness(t, freshSessionStore(t), stub.prepare)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	if _, err := h.LaunchChildSession(context.Background(), launchRequest()); err == nil || err.Error() != "prepare down" {
		t.Fatalf("prepare failure = %v, want the unchanged preparation error", err)
	}
	if stub.callCount() != 1 {
		t.Fatalf("prepare calls = %d, want 1", stub.callCount())
	}
	c.mu.Lock()
	hasGroup := c.group != nil
	c.mu.Unlock()
	if hasGroup {
		t.Fatalf("the failed launch created a member")
	}
	if n := durableRegisterCount(fixtureStore(t, h)); n != 1 {
		t.Fatalf("the failed launch published %d registers, want only the parent", n)
	}
}

// TestLaunchChildSessionPublicationFailure proves a publication failure is
// final: the original error returns, the launch member claim-finishes, and no
// partial child is published or retried.
func TestLaunchChildSessionPublicationFailure(t *testing.T) {
	store := freshSessionStore(t)
	stub := newPrepareStub(validPrepared())
	h := newTestHarness(t, store, stub.prepare)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	store.txHook = func(string) error { return fmt.Errorf("%w: storage down", ErrStorage) }
	_, err = h.LaunchChildSession(context.Background(), launchRequest())
	store.txHook = nil
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("publication failure = %v, want the storage class", err)
	}
	c.mu.Lock()
	empty := c.group == nil || len(c.group.members) == 0
	c.mu.Unlock()
	if !empty {
		t.Fatalf("the failed launch left its member live")
	}
	if n := durableRegisterCount(store); n != 1 {
		t.Fatalf("the failed launch published %d registers, want only the parent", n)
	}
}

// TestLaunchChildSessionRevisionDrift proves the publication guard: a parent
// revision drift returns the conflict without retry, subsequent reads use the
// rematerialized register, and a failed rematerialization returns its own
// error with the launch member finished.
func TestLaunchChildSessionRevisionDrift(t *testing.T) {
	newDriftHarness := func(t *testing.T) (*Harness, *graphStorage, *prepareStub, *coordinator) {
		t.Helper()
		store := freshSessionStore(t)
		stub := newPrepareStub(validPrepared())
		stub.gate = make(chan struct{})
		h := newTestHarness(t, store, stub.prepare)
		c, err := h.coordinatorFor(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("coordinatorFor: %v", err)
		}
		return h, store, stub, c
	}
	runLaunch := func(t *testing.T, h *Harness, stub *prepareStub, mutate func()) error {
		t.Helper()
		type outcome struct {
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			_, err := h.LaunchChildSession(context.Background(), launchRequest())
			done <- outcome{err: err}
		}()
		<-stub.arrived // the view is captured; the launch parks in preparation
		mutate()
		close(stub.gate)
		select {
		case out := <-done:
			return out.err
		case <-time.After(2 * time.Second):
			t.Fatalf("the drifted launch did not return")
			return nil
		}
	}

	t.Run("same-Harness advance returns the conflict on the rematerialized register", func(t *testing.T) {
		h, store, stub, _ := newDriftHarness(t)
		err := runLaunch(t, h, stub, func() {
			if _, err := h.ChangeAgentType(context.Background(), testSessionID, "other"); err != nil {
				t.Errorf("ChangeAgentType: %v", err)
			}
		})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("drifted launch = %v, want the conflict class", err)
		}
		rec, err := h.ReadSession(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if rec.Revision != 2 || rec.State.CurrentAgentType != "other" {
			t.Fatalf("subsequent read = revision %d agent %q, want the rematerialized register", rec.Revision, rec.State.CurrentAgentType)
		}
		if n := durableRegisterCount(store); n != 1 {
			t.Fatalf("the drifted launch published %d registers, want only the parent", n)
		}
	})

	t.Run("storage-side advance returns the conflict without retry", func(t *testing.T) {
		h, store, stub, c := newDriftHarness(t)
		err := runLaunch(t, h, stub, func() { advanceSessionRevision(t, store, testSessionID) })
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("drifted launch = %v, want the conflict class", err)
		}
		rec, err := h.ReadSession(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if rec.Revision != 2 {
			t.Fatalf("subsequent read revision = %d, want the rematerialized 2", rec.Revision)
		}
		c.mu.Lock()
		empty := c.group == nil || len(c.group.members) == 0
		c.mu.Unlock()
		if !empty {
			t.Fatalf("the drifted launch left its member live")
		}
		if n := durableRegisterCount(store); n != 1 {
			t.Fatalf("the drifted launch published %d registers, want only the parent", n)
		}
	})

	t.Run("failed rematerialization returns its error with the member finished", func(t *testing.T) {
		h, store, stub, c := newDriftHarness(t)
		err := runLaunch(t, h, stub, func() {
			advanceSessionRevision(t, store, testSessionID)
			if err := store.Transact(context.Background(), func(tx Transaction) error {
				key := RegisterKey{SessionID: testSessionID, Kind: RegisterSession}
				reg, err := tx.ReadRegister(key)
				if err != nil {
					return err
				}
				_, err = tx.ReplaceRegister(key, reg.Revision, json.RawMessage(`{"bogus":true}`))
				return err
			}); err != nil {
				t.Errorf("corrupt register: %v", err)
			}
		})
		var corrupt *CorruptionError
		if !errors.As(err, &corrupt) {
			t.Fatalf("failed-rematerialization launch = %v, want the corruption class", err)
		}
		c.mu.Lock()
		empty := c.group == nil || len(c.group.members) == 0
		c.mu.Unlock()
		if !empty {
			t.Fatalf("the failed launch left its member live")
		}
		if n := durableRegisterCount(store); n != 1 {
			t.Fatalf("the failed launch published %d registers, want only the parent", n)
		}
	})
}

// TestLaunchChildSessionSettlesRejectedHandoff proves a committed child whose
// handoff was rejected settles its Operation as interruption without starting
// the child execution and without delivering a completion: the parent member
// finishes and the parent records nothing.
func TestLaunchChildSessionSettlesRejectedHandoff(t *testing.T) {
	store := freshSessionStore(t)
	script := newModelScript()
	stub := newPrepareStub(modelPrepared(script.model))
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
	defer cancel()
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	store.txHook = func(string) error { // the stop lands between admission and handoff
		c.mu.Lock()
		c.bgState = bgStopping
		c.mu.Unlock()
		return nil
	}
	_, err = h.LaunchChildSession(context.Background(), launchRequest())
	store.txHook = nil
	if err == nil || err.Error() != "harness: invalid storage request: background group is closed or stopping" {
		t.Fatalf("rejected launch = %v, want the closed-or-stopping rejection", err)
	}

	var childID string
	for id := range store.registers {
		if id != testSessionID {
			childID = id
		}
	}
	if childID == "" {
		t.Fatalf("the committed child register is missing")
	}
	rec := settledOperation(t, store, childID, "child-op-1")
	if rec.State.Status != OperationInterruption {
		t.Fatalf("child operation settled %q, want interruption", rec.State.Status)
	}
	childRec, err := h.ReadSession(context.Background(), childID)
	if err != nil {
		t.Fatalf("ReadSession(child): %v", err)
	}
	if childRec.State.CurrentOperationID != "" {
		t.Fatalf("settled child still runs operation %q", childRec.State.CurrentOperationID)
	}
	if n := storedEntryCount(store, childID); n != 3 { // input, interruption signal, settlement
		t.Fatalf("child entries = %d, want input, interruption signal and settlement", n)
	}
	if n := storedEntryCount(store, testSessionID); n != 0 {
		t.Fatalf("parent entries = %d, want no completion delivery", n)
	}
	if signals := storedSignals(t, store, testSessionID); len(signals) != 0 {
		t.Fatalf("the rejected launch delivered %+v, want no completion", signals)
	}
	c.mu.Lock()
	empty := c.group == nil || len(c.group.members) == 0
	c.mu.Unlock()
	if !empty {
		t.Fatalf("the rejected launch left its member live")
	}
	if n := len(script.seen()); n != 0 {
		t.Fatalf("the child execution ran %d model attempts, want none", n)
	}
}

// TestLaunchChildSessionStartFailureLeavesChildForRecovery proves the
// recovery/no-delivery rule: a child materialization failure and a failed
// terminal interruption both leave the committed child's Operation running
// for recovery, close the parent member without delivery, and latch the
// storage class.
func TestLaunchChildSessionStartFailureLeavesChildForRecovery(t *testing.T) {
	assertRecoveryState := func(t *testing.T, h *Harness, store *graphStorage, c *coordinator, script *modelScript) {
		t.Helper()
		var childID string
		for id := range store.registers {
			if id != testSessionID {
				childID = id
			}
		}
		if childID == "" {
			t.Fatalf("the committed child register is missing")
		}
		if rec := settledOperation(t, store, childID, "child-op-1"); rec.State.Status != OperationRunning {
			t.Fatalf("child operation status = %q, want running for recovery", rec.State.Status)
		}
		reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: childID, Kind: RegisterSession})
		if err != nil {
			t.Fatalf("read child register: %v", err)
		}
		childRec, err := decodeSessionRegister(reg)
		if err != nil {
			t.Fatalf("decode child register: %v", err)
		}
		if childRec.State.CurrentOperationID != "child-op-1" {
			t.Fatalf("child current operation = %q, want the running launch operation", childRec.State.CurrentOperationID)
		}
		c.mu.Lock()
		empty := c.group == nil || len(c.group.members) == 0
		c.mu.Unlock()
		if !empty {
			t.Fatalf("the failed start left its member live")
		}
		if n := storedEntryCount(store, testSessionID); n != 0 {
			t.Fatalf("parent entries = %d, want no delivery", n)
		}
		if n := len(script.seen()); n != 0 {
			t.Fatalf("the child execution ran %d model attempts, want none", n)
		}
		h.mu.Lock()
		latched := h.storageFailure != nil
		h.mu.Unlock()
		if !latched {
			t.Fatalf("the storage-class start failure was not latched")
		}
	}

	t.Run("child materialization failure", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript()
		stub := newPrepareStub(modelPrepared(script.model))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		c, err := h.coordinatorFor(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("coordinatorFor: %v", err)
		}

		store.registersErr = fmt.Errorf("%w: storage down", ErrStorage) // the child materialization read fails
		_, err = h.LaunchChildSession(context.Background(), launchRequest())
		store.registersErr = nil
		if !errors.Is(err, ErrStorage) {
			t.Fatalf("materialization failure = %v, want the storage class", err)
		}
		assertRecoveryState(t, h, store, c, script)
	})

	t.Run("terminal-commit failure after a rejected handoff", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript()
		stub := newPrepareStub(modelPrepared(script.model))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		c, err := h.coordinatorFor(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("coordinatorFor: %v", err)
		}

		// The launch transaction fires exactly four mutation hooks (child
		// session insert_register, input insert_entry, operation
		// insert_register, parent replace_register): the first also lands the
		// stop between admission and handoff, and every later hook — the
		// settlement transaction's — fails.
		calls := 0
		store.txHook = func(string) error {
			calls++
			if calls == 1 {
				c.mu.Lock()
				c.bgState = bgStopping
				c.mu.Unlock()
			}
			if calls > 4 {
				return fmt.Errorf("%w: settlement down", ErrStorage)
			}
			return nil
		}
		_, err = h.LaunchChildSession(context.Background(), launchRequest())
		store.txHook = nil
		if !errors.Is(err, ErrStorage) {
			t.Fatalf("settlement failure = %v, want the storage class", err)
		}
		assertRecoveryState(t, h, store, c, script)
	})
}

// TestLaunchChildSessionStartsChild proves the success path: the shared
// preparation receives the complete child identity, the sentinel capture is
// the durable child admission, the pending completion installs under the
// reserved identity, and the child execution starts.
func TestLaunchChildSessionStartsChild(t *testing.T) {
	store := freshSessionStore(t)
	script := newModelScript()
	script.gate = make(chan struct{})
	stub := newPrepareStub(modelPrepared(script.model))
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
	defer cancel()
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}

	res, err := h.LaunchChildSession(context.Background(), launchRequest())
	if err != nil {
		t.Fatalf("LaunchChildSession: %v", err)
	}
	if !completionIDShape.MatchString(res.ChildSessionID) || res.ChildSessionID == testSessionID {
		t.Fatalf("child id = %q, want a fresh 32-hex identity", res.ChildSessionID)
	}

	call := stub.lastCall()
	if call.req.RequestKind != RequestKindMessage {
		t.Fatalf("prepare request kind = %q, want %q", call.req.RequestKind, RequestKindMessage)
	}
	ident := call.req.Session.Identity
	if ident.SessionID != res.ChildSessionID || ident.ParentSessionID != testSessionID ||
		ident.Workspace != "/tmp/works" || ident.CreatedAt.IsZero() ||
		ident.SourceSessionID != "" || ident.SourceBoundaryEntryID != "" {
		t.Fatalf("prepared identity = %+v, want the complete child lineage", ident)
	}
	if call.req.Session.AgentType != "child-agent" {
		t.Fatalf("prepared agent type = %q, want child-agent", call.req.Session.AgentType)
	}

	childRec, err := h.ReadSession(context.Background(), res.ChildSessionID)
	if err != nil {
		t.Fatalf("ReadSession(child): %v", err)
	}
	if childRec.Identity.ParentSessionID != testSessionID || childRec.Identity.Workspace != "/tmp/works" ||
		!childRec.Identity.CreatedAt.Equal(ident.CreatedAt) || childRec.Identity.SourceSessionID != "" {
		t.Fatalf("durable child identity = %+v, want the prepared identity", childRec.Identity)
	}
	if childRec.State.Lifecycle != LifecycleOpen || childRec.State.CurrentAgentType != "child-agent" ||
		childRec.State.CurrentOperationID != "child-op-1" || childRec.State.ArchivedAt != nil {
		t.Fatalf("durable child state = %+v, want the initial launch state", childRec.State)
	}
	op := settledOperation(t, store, res.ChildSessionID, "child-op-1")
	if op.State.Status != OperationRunning || op.Admission.RequestKind != RequestKindMessage ||
		op.Admission.AgentType != "child-agent" {
		t.Fatalf("durable child operation = %+v, want the running plugin admission", op)
	}
	capture := op.Admission.Execution
	want := testCapture()
	if !reflect.DeepEqual(capture, want) {
		t.Fatalf("durable child capture = %+v, want the sentinel capture %+v", capture, want)
	}
	graph, err := validateFixture(t, store, res.ChildSessionID)
	if err != nil {
		t.Fatalf("child graph: %v", err)
	}
	if len(graph.Entries) != 1 || graph.Entries[0].Input == nil ||
		graph.Entries[0].Input.Origin != InputOriginPlugin || graph.Entries[0].Input.Content[0].Text != "child task" {
		t.Fatalf("child entries = %+v, want the plugin-origin input", graph.Entries)
	}

	c.mu.Lock()
	members := len(c.group.members)
	var member *backgroundMember
	for _, m := range c.group.members {
		member = m
	}
	c.mu.Unlock()
	if members != 1 || member == nil || member.kind != memberChild || member.id != res.ChildSessionID || member.claimed {
		t.Fatalf("parent members = %d (%+v), want the unclaimed child member", members, member)
	}
	child := cachedCoordinator(t, h, res.ChildSessionID)
	child.mu.Lock()
	pending := child.pendingCompletion
	child.mu.Unlock()
	if pending == nil || pending.completionID != member.completionID || pending.outputLimit != 1024 {
		t.Fatalf("child pending completion = %+v, want the launch info under the reserved identity", pending)
	}

	select {
	case <-script.arrived: // the child execution started and parked at its model boundary
	case <-time.After(2 * time.Second):
		t.Fatalf("the child execution did not start")
	}

	script.releaseGate() // let the child run converge before the harness cancels
	child.mu.Lock()
	run := child.run
	child.mu.Unlock()
	if run == nil {
		t.Fatalf("the child execution is not installed")
	}
	cancel()
	select {
	case <-run.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("the child execution did not converge")
	}
}
