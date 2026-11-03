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

	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.NoError(t, err)
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
			testassert.Require(t, errors.Is(err, ErrInvalid), "wrong-length job id = %v, want ErrInvalid", err)
		}},
		{"non-hex", "zzzzzzzz", stubJobStopper{}, spawn, func(t *testing.T, err error) {
			testassert.Require(t, errors.Is(err, ErrInvalid), "non-hex job id = %v, want ErrInvalid", err)
		}},
		{"uppercase", "DEADBEEF", stubJobStopper{}, spawn, func(t *testing.T, err error) {
			testassert.Require(t, errors.Is(err, ErrInvalid), "uppercase job id = %v, want ErrInvalid", err)
		}},
		{"nil stopper", jobFixtureID, nil, spawn, func(t *testing.T, err error) {
			testassert.Require(t, errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "no job stopper configured"), "nil stopper = %v, want the no-job-stopper rejection", err)
		}},
		{"nil spawn", jobFixtureID, stubJobStopper{}, nil, func(t *testing.T, err error) {
			testassert.Require(t, errors.Is(err, ErrInvalid), "nil spawn = %v, want ErrInvalid", err)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHarness(t, freshSessionStore(t), nil)
			h.deps.Jobs = tc.jobs
			err := h.StartJob(context.Background(), testSessionID, tc.jobID, tc.spawn)
			testassert.Require(t, err != nil, "StartJob succeeded, want a rejection")
			tc.wantErr(t, err)
			testassert.Equal(t, len(spawned), 0)
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
		testassert.Require(t, err != nil && err.Error() == want.Error(), "archived start = %v, want %v", err, want)
		testassert.Equal(t, len(spawned), 0)
	})

	t.Run("closed group", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), nil)
		h.deps.Jobs = stubJobStopper{}
		c := leaseCoordinator(t, h, testSessionID)
		c.mu.Lock()
		c.bgState = bgStopping
		c.mu.Unlock()
		{
			err := h.StartJob(context.Background(), testSessionID, jobFixtureID, spawn)
			testassert.Require(t, err != nil && err.Error() == "harness: invalid storage request: background group is closed or stopping", "closed-group start = %v, want the closed-or-stopping rejection", err)
		}
		testassert.Equal(t, len(spawned), 0)
		c.mu.Lock()
		hasGroup := c.group != nil
		c.mu.Unlock()
		testassert.Require(t, !hasGroup, "the rejected start created a member")
	})

	t.Run("canceled harness", func(t *testing.T) {
		hctx, cancel := context.WithCancel(context.Background())
		h, err := New(hctx, Dependencies{
			Storage: freshSessionStore(t),
			Prepare: func(context.Context, PreparationRequest) (PreparedExecution, error) {
				return PreparedExecution{}, errors.New("no preparation configured for this test")
			},
		})
		testassert.NoError(t, err)
		h.deps.Jobs = stubJobStopper{}
		cancel()
		{
			err := h.StartJob(context.Background(), testSessionID, jobFixtureID, spawn)
			testassert.Require(t, errors.Is(err, context.Canceled) && err.Error() == "context canceled", "canceled start = %v, want the raw context.Canceled", err)
		}
		testassert.Equal(t, len(spawned), 0)
	})
}

// TestStartJobHandoffLossFinishesMemberOnce proves the handoff failure
// capture: every state outside the valid handoff set fails uniformly — the
// raw harness error when the Harness is closed, the closed-or-stopping
// rejection otherwise — and the member claim-finishes exactly once.
func TestStartJobHandoffLossFinishesMemberOnce(t *testing.T) {
	t.Run("closed group rejects with the closed-or-stopping text", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), nil)
		c := leaseCoordinator(t, h, testSessionID)
		member, err := admitLocked(h, c, memberJob, jobFixtureID, 0)
		testassert.NoError(t, err)
		c.mu.Lock()
		c.bgState = bgStopping
		c.mu.Unlock()
		{
			err := h.backgroundHandoffFailure(c, member)
			testassert.Require(t, err != nil && err.Error() == "harness: invalid storage request: background group is closed or stopping", "handoff loss = %v, want the closed-or-stopping rejection", err)
		}
		h.claimFinishBackgroundMember(c, member.completionID)
		waitMemberDone(t, member)
		c.mu.Lock()
		empty := c.group == nil || len(c.group.members) == 0
		c.mu.Unlock()
		testassert.Require(t, empty, "the finished member stayed in the group")
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
		testassert.NoError(t, err)
		c := leaseCoordinator(t, h, testSessionID)
		member, err := admitLocked(h, c, memberJob, jobFixtureID, 0)
		testassert.NoError(t, err)
		cancel() // the Harness closes after the admission, before the handoff
		{
			err := h.backgroundHandoffFailure(c, member)
			testassert.Require(t, errors.Is(err, context.Canceled) && err.Error() == "context canceled", "handoff loss = %v, want the raw context.Canceled", err)
		}
	})
}

// TestStartJobSpawnFailureFinishesMemberOnce proves a failed spawn returns its
// error unchanged and releases the member exactly once: the spawn transferred
// no live completion obligation.
func TestStartJobSpawnFailureFinishesMemberOnce(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	h.deps.Jobs = stubJobStopper{}
	c := leaseCoordinator(t, h, testSessionID)
	var spawned []string
	spawn := spawnRecord(&spawned, errors.New("spawn down"))
	{
		err := h.StartJob(context.Background(), testSessionID, jobFixtureID, spawn)
		testassert.Require(t, err != nil && err.Error() == "spawn down", "spawn failure = %v, want the unchanged spawn error", err)
	}
	testassert.Equal(t, len(spawned), 1)
	testassert.Require(t, completionIDShape.MatchString(spawned[0]), "spawned completion id %q is not 32 lowercase hex", spawned[0])
	c.mu.Lock()
	empty := c.group == nil || len(c.group.members) == 0
	c.mu.Unlock()
	testassert.Require(t, empty, "the failed spawn left the member live")
}

// TestStartJobDrivesSpawnThroughAdmission proves the success path: StartJob
// admits a job member, passes the handoff, and hands the reserved completion
// identity to the spawn; the member stays live for the delivery.
func TestStartJobDrivesSpawnThroughAdmission(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	h.deps.Jobs = stubJobStopper{}
	c := leaseCoordinator(t, h, testSessionID)
	var spawned []string
	spawn := spawnRecord(&spawned, nil)
	{
		err := h.StartJob(context.Background(), testSessionID, jobFixtureID, spawn)
		testassert.NoError(t, err)
	}
	testassert.Equal(t, len(spawned), 1)
	testassert.Require(t, completionIDShape.MatchString(spawned[0]), "spawned completion id %q is not 32 lowercase hex", spawned[0])
	c.mu.Lock()
	member := c.group.members[spawned[0]]
	c.mu.Unlock()
	testassert.Require(t, member != nil && member.kind == memberJob && member.id == jobFixtureID && !member.claimed, "admitted member = %+v, want the unclaimed job member", member)
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
			{
				_, err := h.LaunchChildSession(context.Background(), req)
				testassert.Require(t, errors.Is(err, ErrInvalid), "%s = %v, want ErrInvalid", tc.name, err)
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
	c := leaseCoordinator(t, h, testSessionID)
	{
		_, err := h.LaunchChildSession(context.Background(), launchRequest())
		testassert.Require(t, errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "child sessions cannot launch child sessions"), "child-parent launch = %v, want the refusal", err)
	}
	testassert.Equal(t, stub.callCount(), 0)
	c.mu.Lock()
	hasGroup := c.group != nil
	c.mu.Unlock()
	testassert.Require(t, !hasGroup, "the refused launch created a member")
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
	c := leaseCoordinator(t, h, testSessionID)
	_, err := h.LaunchChildSession(context.Background(), launchRequest())
	want := invalidInput("session %q is archived; admission requires an open Session", testSessionID)
	testassert.Require(t, err != nil && err.Error() == want.Error(), "archived launch = %v, want %v", err, want)
	c.mu.Lock()
	hasGroup := c.group != nil
	c.mu.Unlock()
	testassert.Require(t, !hasGroup, "the rejected launch created a member")
	{
		n := durableRegisterCount(fixtureStore(t, h))
		testassert.Equal(t, n, 1)
	}
}

// fixtureStore returns the graphStorage behind a harness built by newTestHarness.
func fixtureStore(t *testing.T, h *Harness) *graphStorage {
	t.Helper()
	store, ok := h.deps.Storage.(*graphStorage)
	testassert.Require(t, ok, "the fixture harness does not use a graphStorage")
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
// requested cap the launch is rejected before any member or child exists, and
// once the live member completes a later launch succeeds with a freshly
// generated child identity.
func TestLaunchChildSessionCapRejection(t *testing.T) {
	stub := newPrepareStub(validPrepared())
	h, cancel := newCancelableHarness(t, freshSessionStore(t), PreparedExecution{}, stub.prepare)
	defer cancel()
	c := leaseCoordinator(t, h, testSessionID)
	member, err := admitLocked(h, c, memberChild, "existing-child", 0)
	testassert.NoError(t, err)
	req := launchRequest()
	req.MaxConcurrent = 1
	_, err = h.LaunchChildSession(context.Background(), req)
	want := "harness: invalid storage request: background group is full (1/1). Wait for a background member to complete, then retry"
	testassert.Require(t, err != nil && err.Error() == want, "capped launch = %v, want %q", err, want)
	c.mu.Lock()
	members := len(c.group.members)
	c.mu.Unlock()
	testassert.Equal(t, members, 1)
	{
		n := durableRegisterCount(fixtureStore(t, h))
		testassert.Equal(t, n, 1)
	}

	// Completing the live member frees the cap: the next public launch
	// succeeds and publishes a fresh child identity.
	h.finishBackgroundMember(c, member)
	res, err := h.LaunchChildSession(context.Background(), req)
	testassert.NoError(t, err)
	testassert.Require(t, completionIDShape.MatchString(res.ChildSessionID) && res.ChildSessionID != testSessionID, "launched child session id %q, want a fresh generated identity", res.ChildSessionID)
	{
		_, err := fixtureStore(t, h).ReadRegister(context.Background(), RegisterKey{SessionID: res.ChildSessionID, Kind: RegisterSession})
		testassert.NoError(t, err)
	}
}

// TestLaunchChildSessionPrepareFailure proves a preparation failure publishes
// neither child nor member: the no-bypass sibling of the shared preparation
// path.
func TestLaunchChildSessionPrepareFailure(t *testing.T) {
	stub := newPrepareStub(PreparedExecution{})
	stub.err = errors.New("prepare down")
	h := newTestHarness(t, freshSessionStore(t), stub.prepare)
	c := leaseCoordinator(t, h, testSessionID)
	{
		_, err := h.LaunchChildSession(context.Background(), launchRequest())
		testassert.Require(t, err != nil && err.Error() == "prepare down", "prepare failure = %v, want the unchanged preparation error", err)
	}
	if stub.callCount() != 1 {
		t.Fatalf("prepare calls = %d, want 1", stub.callCount())
	}
	c.mu.Lock()
	hasGroup := c.group != nil
	c.mu.Unlock()
	testassert.Require(t, !hasGroup, "the failed launch created a member")
	{
		n := durableRegisterCount(fixtureStore(t, h))
		testassert.Equal(t, n, 1)
	}
}

// TestLaunchChildSessionPublicationFailure proves a publication failure is
// final: the original error returns, the launch member claim-finishes, and no
// partial child is published or retried.
func TestLaunchChildSessionPublicationFailure(t *testing.T) {
	store := freshSessionStore(t)
	stub := newPrepareStub(validPrepared())
	h := newTestHarness(t, store, stub.prepare)
	c := leaseCoordinator(t, h, testSessionID)
	store.txHook = func(string) error { return fmt.Errorf("%w: storage down", ErrStorage) }
	_, err := h.LaunchChildSession(context.Background(), launchRequest())
	store.txHook = nil
	testassert.Require(t, errors.Is(err, ErrStorage), "publication failure = %v, want the storage class", err)
	c.mu.Lock()
	empty := c.group == nil || len(c.group.members) == 0
	c.mu.Unlock()
	testassert.Require(t, empty, "the failed launch left its member live")
	{
		n := durableRegisterCount(store)
		testassert.Equal(t, n, 1)
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
		c := leaseCoordinator(t, h, testSessionID)
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
			{
				_, err := h.ChangeAgentType(context.Background(), testSessionID, "other")
				testassert.Check(t, err == nil, "ChangeAgentType: %v", err)
			}
		})
		testassert.Require(t, errors.Is(err, ErrConflict), "drifted launch = %v, want the conflict class", err)
		rec, err := h.ReadSessionHeader(context.Background(), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.Revision.DurableRevision == 2 && rec.CurrentAgentType == "other", "subsequent read = revision %d agent %q, want the rematerialized register", rec.Revision.DurableRevision, rec.CurrentAgentType)
		{
			n := durableRegisterCount(store)
			testassert.Equal(t, n, 1)
		}
	})

	t.Run("storage-side advance returns the conflict without retry", func(t *testing.T) {
		h, store, stub, c := newDriftHarness(t)
		err := runLaunch(t, h, stub, func() { advanceSessionRevision(t, store, testSessionID) })
		testassert.Require(t, errors.Is(err, ErrConflict), "drifted launch = %v, want the conflict class", err)
		rec, err := h.ReadSessionHeader(context.Background(), testSessionID)
		testassert.NoError(t, err)
		testassert.Equal(t, rec.Revision.DurableRevision, 2)
		c.mu.Lock()
		empty := c.group == nil || len(c.group.members) == 0
		c.mu.Unlock()
		testassert.Require(t, empty, "the drifted launch left its member live")
		{
			n := durableRegisterCount(store)
			testassert.Equal(t, n, 1)
		}
	})

	t.Run("failed rematerialization returns its error with the member finished", func(t *testing.T) {
		h, store, stub, c := newDriftHarness(t)
		err := runLaunch(t, h, stub, func() {
			advanceSessionRevision(t, store, testSessionID)
			{
				err := store.Transact(context.Background(), func(tx Transaction) error {
					key := RegisterKey{SessionID: testSessionID, Kind: RegisterSession}
					reg, err := tx.ReadRegister(key)
					if err != nil {
						return err
					}
					_, err = tx.ReplaceRegister(key, reg.Revision, json.RawMessage(`{"bogus":true}`))
					return err
				})
				testassert.Check(t, err == nil, "corrupt register: %v", err)
			}
		})
		var corrupt *CorruptionError
		testassert.Require(t, errors.As(err, &corrupt), "failed-rematerialization launch = %v, want the corruption class", err)
		c.mu.Lock()
		empty := c.group == nil || len(c.group.members) == 0
		c.mu.Unlock()
		testassert.Require(t, empty, "the failed launch left its member live")
		{
			n := durableRegisterCount(store)
			testassert.Equal(t, n, 1)
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
	c := leaseCoordinator(t, h, testSessionID)
	store.txHook = func(string) error { // the stop lands between admission and handoff
		c.mu.Lock()
		c.bgState = bgStopping
		c.mu.Unlock()
		return nil
	}
	_, err := h.LaunchChildSession(context.Background(), launchRequest())
	store.txHook = nil
	testassert.Require(t, err != nil && err.Error() == "harness: invalid storage request: background group is closed or stopping", "rejected launch = %v, want the closed-or-stopping rejection", err)

	var childID string
	for id := range store.registers {
		if id != testSessionID {
			childID = id
		}
	}
	testassert.Require(t, childID != "", "the committed child register is missing")
	rec := settledOperation(t, store, childID, "child-op-1")
	testassert.Equal(t, rec.State.Status, OperationInterruption)
	childRec, err := h.ReadSessionHeader(context.Background(), childID)
	testassert.NoError(t, err)
	testassert.Equal(t, childRec.CurrentOperationID, "")
	// input, interruption signal, settlement
	{
		n := storedEntryCount(store, childID)
		testassert.Equal(t, n, 3)
	}
	{
		n := storedEntryCount(store, testSessionID)
		testassert.Equal(t, n, 0)
	}
	{
		signals := storedSignals(t, store, testSessionID)
		testassert.Require(t, len(signals) == 0, "the rejected launch delivered %+v, want no completion", signals)
	}
	c.mu.Lock()
	empty := c.group == nil || len(c.group.members) == 0
	c.mu.Unlock()
	testassert.Require(t, empty, "the rejected launch left its member live")
	{
		n := len(script.seen())
		testassert.Equal(t, n, 0)
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
		testassert.Require(t, childID != "", "the committed child register is missing")
		{
			rec := settledOperation(t, store, childID, "child-op-1")
			testassert.Equal(t, rec.State.Status, OperationRunning)
		}
		reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: childID, Kind: RegisterSession})
		testassert.NoError(t, err)
		childRec, err := decodeSessionRegister(reg)
		testassert.NoError(t, err)
		testassert.Equal(t, childRec.State.CurrentOperationID, "child-op-1")
		c.mu.Lock()
		empty := c.group == nil || len(c.group.members) == 0
		c.mu.Unlock()
		testassert.Require(t, empty, "the failed start left its member live")
		{
			n := storedEntryCount(store, testSessionID)
			testassert.Equal(t, n, 0)
		}
		{
			n := len(script.seen())
			testassert.Equal(t, n, 0)
		}
		h.mu.Lock()
		latched := h.storageFailure != nil
		h.mu.Unlock()
		testassert.Require(t, latched, "the storage-class start failure was not latched")
	}

	t.Run("child materialization failure", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript()
		stub := newPrepareStub(modelPrepared(script.model))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		c := leaseCoordinator(t, h, testSessionID)

		store.registersErr = fmt.Errorf("%w: storage down", ErrStorage) // the child materialization read fails
		_, err := h.LaunchChildSession(context.Background(), launchRequest())
		store.registersErr = nil
		testassert.Require(t, errors.Is(err, ErrStorage), "materialization failure = %v, want the storage class", err)
		assertRecoveryState(t, h, store, c, script)
	})

	t.Run("terminal-commit failure after a rejected handoff", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript()
		stub := newPrepareStub(modelPrepared(script.model))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		c := leaseCoordinator(t, h, testSessionID)

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
		_, err := h.LaunchChildSession(context.Background(), launchRequest())
		store.txHook = nil
		testassert.Require(t, errors.Is(err, ErrStorage), "settlement failure = %v, want the storage class", err)
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
	c := leaseCoordinator(t, h, testSessionID)

	res, err := h.LaunchChildSession(context.Background(), launchRequest())
	testassert.NoError(t, err)
	testassert.Require(t, completionIDShape.MatchString(res.ChildSessionID) && res.ChildSessionID != testSessionID, "child id = %q, want a fresh 32-hex identity", res.ChildSessionID)

	call := stub.lastCall()
	testassert.Equal(t, call.req.RequestKind, RequestKindMessage)
	ident := call.req.Session.Identity
	testassert.Require(t, ident.SessionID == res.ChildSessionID && ident.ParentSessionID == testSessionID && ident.Workspace == "/tmp/works" && !ident.CreatedAt.IsZero() && ident.SourceSessionID == "" && ident.SourceBoundaryEntryID == "", "prepared identity = %+v, want the complete child lineage", ident)
	testassert.Equal(t, call.req.Session.AgentType, "child-agent")

	childRec, err := h.ReadSessionHeader(context.Background(), res.ChildSessionID)
	testassert.NoError(t, err)
	testassert.Require(t, childRec.Identity.ParentSessionID == testSessionID && childRec.Identity.Workspace == "/tmp/works" && childRec.Identity.CreatedAt.Equal(ident.CreatedAt) && childRec.Identity.SourceSessionID == "" && (childRec.Lifecycle == LifecycleOpen && childRec.CurrentAgentType == "child-agent" && childRec.CurrentOperationID == "child-op-1" && childRec.ArchivedAt == nil), "durable child identity = %+v, want the prepared identity; durable child state = %+v, want the initial launch state", childRec.Identity, childRec)
	op := settledOperation(t, store, res.ChildSessionID, "child-op-1")
	testassert.Require(t, op.State.Status == OperationRunning && op.Admission.RequestKind == RequestKindMessage && op.Admission.AgentType == "child-agent", "durable child operation = %+v, want the running plugin admission", op)
	capture := op.Admission.Execution
	want := testCapture()
	testassert.Require(t, reflect.DeepEqual(capture, want), "durable child capture = %+v, want the sentinel capture %+v", capture, want)
	graph, err := validateFixture(t, store, res.ChildSessionID)
	testassert.NoError(t, err)
	testassert.Require(t, len(graph.Entries) == 1 && graph.Entries[0].Input != nil && graph.Entries[0].Input.Origin == InputOriginPlugin && graph.Entries[0].Input.Content[0].Text == "child task", "child entries = %+v, want the plugin-origin input", graph.Entries)

	c.mu.Lock()
	members := len(c.group.members)
	var member *backgroundMember
	for _, m := range c.group.members {
		member = m
	}
	c.mu.Unlock()
	testassert.Require(t, members == 1 && member != nil && member.kind == memberChild && member.id == res.ChildSessionID && !member.claimed, "parent members = %d (%+v), want the unclaimed child member", members, member)
	child := cachedCoordinator(t, h, res.ChildSessionID)
	child.mu.Lock()
	pending := child.pendingCompletion
	child.mu.Unlock()
	testassert.Require(t, pending != nil && pending.completionID == member.completionID && pending.outputLimit == 1024, "child pending completion = %+v, want the launch info under the reserved identity", pending)

	select {
	case <-script.arrived: // the child execution started and parked at its model boundary
	case <-time.After(2 * time.Second):
		t.Fatalf("the child execution did not start")
	}

	child.mu.Lock()
	run := child.run
	child.mu.Unlock()
	testassert.Require(t, run != nil, "the child execution is not installed")
	script.releaseGate() // let the child run converge before the harness cancels
	cancel()
	select {
	case <-run.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("the child execution did not converge")
	}
}
