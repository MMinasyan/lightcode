package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// parkedPrepared is one prepared execution whose model attempt parks until its
// execution context is canceled, so fixtures converge on cancel without a
// leaked goroutine.
func parkedPrepared() PreparedExecution {
	return modelPrepared(parkedModel(make(chan struct{})))
}

// factCollector is the fixtures' synchronous passive observer: it records
// every delivered fact under its own mutex and never blocks a producer.
type factCollector struct {
	mu    sync.Mutex
	facts []HarnessFact
}

func (c *factCollector) observe(fact HarnessFact) {
	c.mu.Lock()
	c.facts = append(c.facts, fact)
	c.mu.Unlock()
}

func (c *factCollector) snapshot() []HarnessFact {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]HarnessFact(nil), c.facts...)
}

func (c *factCollector) reset() {
	c.mu.Lock()
	c.facts = nil
	c.mu.Unlock()
}

func (c *factCollector) invalidations() []HarnessFact {
	var out []HarnessFact
	for _, fact := range c.snapshot() {
		if fact.Kind == FactInvalidation {
			out = append(out, fact)
		}
	}
	return out
}

// observeHarness installs one recording observer before any work starts. The
// Harness copies its dependencies at construction, so the write is safe: no
// producer runs until the test calls one.
func observeHarness(h *Harness) *factCollector {
	col := &factCollector{}
	h.deps.Observe = col.observe
	return col
}

// snapshotPair reads one Session's current revision pair through the landed
// metadata read.
func snapshotPair(t *testing.T, h *Harness, sessionID string) SessionRevision {
	t.Helper()
	header, err := h.ReadSessionHeader(context.Background(), sessionID)
	testassert.NoError(t, err)
	return header.Revision
}

// wantPairAt asserts that one Session's current revision pair equals the
// expected value; the pair is read after the producer released its lock, the
// same publication-time rule the passive publishers use.
func wantPairAt(t *testing.T, h *Harness, sessionID string, durable int64, local uint64) {
	t.Helper()
	want := SessionRevision{DurableRevision: durable, LocalRevision: local}
	{
		got := snapshotPair(t, h, sessionID)
		testassert.Equal(t, got, want)
	}
}

// awaitHarnessQuiet waits until one Session has no current Operation and no
// installed run, the deterministic post-terminal barrier.
func awaitHarnessQuiet(t *testing.T, h *Harness, sessionID string) SessionReadView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		view, err := h.ReadSessionView(context.Background(), sessionID)
		testassert.NoError(t, err)
		if !view.History.ExecutionBusy && view.Header.CurrentOperationID == "" {
			return view
		}
		testassert.Require(t, !time.Now().After(deadline), "session %s never went quiet", sessionID)
		time.Sleep(time.Millisecond)
	}
}

// TestObserveFactKinds proves the closed union: every kind carries exactly the
// members it requires and no other member, produced by real model and tool
// boundaries.
func TestObserveFactKinds(t *testing.T) {
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(testToolCall("call-1")), nil
	}
	h, _, c, sessionID := newEffectHarness(t, modelFn)
	col := observeHarness(h)
	{
		_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
	plan := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
	}
	{
		_, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), invocationExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
	}

	var (
		invalidations int
		deltas        int
		started       int
		finished      int
	)
	for _, fact := range col.snapshot() {
		switch fact.Kind {
		case FactInvalidation:
			invalidations++
			testassert.Require(t, fact.SessionID == sessionID && fact.OperationID == "" && fact.JobID == "" && fact.Position == 0 && fact.Content == "" && fact.CallID == "" && fact.Ordinal == 0 && fact.Name == "" && fact.Status == "", "invalidation fact carries unrelated fields: %+v", fact)
		case FactTextDelta:
			deltas++
			testassert.Require(t, fact.SessionID == sessionID && fact.OperationID == testOpID && fact.Position == 0 && fact.Content == "done" && fact.JobID == "" && fact.CallID == "" && fact.Ordinal == 0 && fact.Name == "" && fact.Status == "", "text_delta fact carries unrelated fields: %+v", fact)
		case FactToolStarted:
			started++
			testassert.Require(t, fact.SessionID == sessionID && fact.OperationID == testOpID && fact.CallID == "call-1" && fact.Ordinal == 0 && fact.Name == "echo" && fact.JobID == "" && fact.Position == 0 && fact.Content == "" && fact.Status == "", "tool_started fact carries unrelated fields: %+v", fact)
		case FactToolFinished:
			finished++
			testassert.Require(t, fact.SessionID == sessionID && fact.OperationID == testOpID && fact.CallID == "call-1" && fact.Status == model.ResultSuccess && fact.JobID == "" && fact.Position == 0 && fact.Content == "" && fact.Ordinal == 0 && fact.Name == "", "tool_finished fact carries unrelated fields: %+v", fact)
		default:
			t.Fatalf("fact kind %q is outside the closed set", fact.Kind)
		}
	}
	testassert.Require(t, invalidations >= 2 && deltas == 1 && started == 1 && finished == 1, "fact counts = invalidations %d deltas %d started %d finished %d", invalidations, deltas, started, finished)
}

// TestObserveReservationClocks proves each reservation acquisition and release
// emits one local-only invalidation and nothing durable.
func TestObserveReservationClocks(t *testing.T) {
	h, _, c, sessionID := newEffectHarness(t, nil)
	col := observeHarness(h)
	base := snapshotPair(t, h, sessionID)

	release, err := h.reserve(context.Background(), c)
	testassert.NoError(t, err)
	release()

	facts := col.invalidations()
	testassert.Equal(t, len(facts), 2)
	wantPairAt(t, h, sessionID, base.DurableRevision, base.LocalRevision+2)
	for _, fact := range facts {
		testassert.Require(t, fact.SessionID == sessionID && fact.JobID == "" && fact.OperationID == "", "reservation fact carries unrelated identity: %+v", fact)
	}
}

// TestObserveRunLifecycle proves the run slot's installation and the drain's
// retirement each emit once, and that a drain-installed successor's slot is
// never cleared or emitted by its predecessor.
func TestObserveRunLifecycle(t *testing.T) {
	t.Run("install and retire", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript(turn())
		script.gate = make(chan struct{})
		stub := newPrepareStub(modelPrepared(script.model))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		{
			_, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		c := cachedCoordinator(t, h, testSessionID)
		col := observeHarness(h)
		base := snapshotPair(t, h, testSessionID)

		h.startExecution(c, testOpID, modelPrepared(script.model))
		facts := col.invalidations()
		testassert.Equal(t, len(facts), 1)
		wantPairAt(t, h, testSessionID, base.DurableRevision, base.LocalRevision+1) // the run slot's installation

		c.mu.Lock()
		run := c.run
		c.mu.Unlock()
		testassert.Require(t, run != nil, "the run slot was not installed")
		script.releaseGate()
		select {
		case <-run.done:
		case <-time.After(10 * time.Second):
			t.Fatalf("the execution never converged")
		}
		final := snapshotPair(t, h, testSessionID)
		testassert.Require(t, final.DurableRevision > base.DurableRevision && final.LocalRevision > base.LocalRevision, "converged pair = %+v, want the model result's durable advance and the drain's local publications over %+v", final, base)
	})

	t.Run("successor slot survives the predecessor", func(t *testing.T) {
		store := freshSessionStore(t)
		var calls atomic.Int32
		gate1 := make(chan struct{})
		gate2 := make(chan struct{})
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			switch calls.Add(1) {
			case 1:
				<-gate1
			case 2:
				<-gate2
			}
			return completedTurnStream(), nil
		}
		stub := newPrepareStub(modelPrepared(modelFn))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		session := createSession(t, h)
		{
			_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		{
			_, err := submitText(t, h, session, "op-2", MessageModeQueued, "queued")
			testassert.NoError(t, err)
		}
		c := cachedCoordinator(t, h, session)
		col := observeHarness(h)
		base := snapshotPair(t, h, session)

		close(gate1)
		c.mu.Lock()
		run1 := c.run
		c.mu.Unlock()
		testassert.Require(t, run1 != nil, "run1 was not installed")
		select {
		case <-run1.done:
		case <-time.After(10 * time.Second):
			t.Fatalf("run1 never converged")
		}
		// The queued successor was installed by the drain and is parked at
		// gate2: the predecessor's tail must not have cleared its slot.
		view, err := h.ReadSessionView(context.Background(), session)
		testassert.NoError(t, err)
		testassert.Require(t, view.History.ExecutionBusy, "the drain-installed successor slot was cleared by its predecessor")
		close(gate2)
		final := awaitHarnessQuiet(t, h, session)
		facts := col.invalidations()
		testassert.Require(t, len(facts) >= 2, "successor facts = %d, want at least the successor's retire and the predecessor's release", len(facts))
		testassert.Require(t, final.Header.Revision.DurableRevision > base.DurableRevision && final.Header.Revision.LocalRevision > base.LocalRevision, "successor pair = %+v, want the successor's durable admission and its local publications over %+v", final.Header.Revision, base)
	})
}

// TestObserveBufferClocks proves the buffer clocks: the active enqueue, the
// drain's admission, and the Harness-loss discard each emit exactly when the
// counter advances. A delivery's head selection publishes nothing: the
// removal rides the durable adoption, or is the failed attempt's final
// outcome under its own hold.
func TestObserveBufferClocks(t *testing.T) {
	t.Run("active enqueue holds the reservation across the enqueue", func(t *testing.T) {
		h, _, _, sessionID := newEffectHarness(t, nil)
		col := observeHarness(h)
		base := snapshotPair(t, h, sessionID)

		// The active route holds the reservation across the enqueue.
		{
			_, err := h.Submit(context.Background(), SubmitRequest{
				SessionID: sessionID, OperationID: "op-steer", Origin: InputOriginUser,
				Content: admissionContent("steer"), Mode: MessageModeRegular,
			})
			testassert.NoError(t, err)
		}
		facts := col.invalidations()
		testassert.Equal(t, len(facts), 3)
		wantPairAt(t, h, sessionID, base.DurableRevision, base.LocalRevision+3)
	})

	t.Run("drain selection and admission", func(t *testing.T) {
		store := freshSessionStore(t)
		stub := newPrepareStub(parkedPrepared())
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer func() {
			cancel()
			_ = h.Wait(context.Background())
		}()
		{
			_, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		c := cachedCoordinator(t, h, testSessionID)
		{
			_, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{terminal: OperationSuccess})
			testassert.NoError(t, err)
		}
		col := observeHarness(h)

		// The process-local queued publication the active route would make.
		c.mu.Lock()
		c.queued = append(c.queued, &pendingMessage{operationID: "op-2", origin: InputOriginUser, content: admissionContent("queued")})
		c.bumpLocalRevision()
		c.run = &activeExecution{done: make(chan struct{})} // the pre-terminal run the real flow would have retired
		run := c.run
		c.mu.Unlock()
		base := snapshotPair(t, h, testSessionID)
		col.reset() // isolate the drain's own publications

		release, err := h.reserve(h.ctx, c)
		testassert.NoError(t, err)
		h.drainBuffers(c, run)
		release()
		facts := col.invalidations()
		testassert.Equal(t, len(facts), 4)
		wantPairAt(t, h, testSessionID, base.DurableRevision+1, base.LocalRevision+3) // one durable admission — the removal rode it — and three local publications
	})

	t.Run("harness-loss discard", func(t *testing.T) {
		hctx, cancel := context.WithCancel(context.Background())
		h, err := New(hctx, Dependencies{
			Storage: freshSessionStore(t),
			Prepare: func(context.Context, PreparationRequest) (PreparedExecution, error) {
				return modelPrepared(nil), nil
			},
		})
		testassert.NoError(t, err)
		{
			_, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		c := cachedCoordinator(t, h, testSessionID)
		{
			_, err := h.Submit(context.Background(), SubmitRequest{
				SessionID: testSessionID, OperationID: "op-steer", Origin: InputOriginUser,
				Content: admissionContent("steer"), Mode: MessageModeRegular,
			})
			testassert.NoError(t, err)
		}
		col := observeHarness(h)
		base := snapshotPair(t, h, testSessionID)
		cancel()

		c.mu.Lock()
		c.run = &activeExecution{done: make(chan struct{})} // the pre-terminal run the real flow would have retired
		run := c.run
		c.mu.Unlock()
		release, err := h.reserve(h.ctx, c)
		testassert.NoError(t, err)
		h.drainBuffers(c, run) // the drain's loss gate discards both buffers
		release()
		facts := col.invalidations()
		testassert.Equal(t, len(facts), 3)
		wantPairAt(t, h, testSessionID, base.DurableRevision, base.LocalRevision+3)

		release, err = h.reserve(h.ctx, c)
		testassert.NoError(t, err)
		h.drainBuffers(c, run) // an empty discard emits only the reservation cycle
		release()
		{
			got := len(col.invalidations())
			testassert.Equal(t, got, 5)
		}
	})
}

// TestObserveBackgroundClocks proves the member and group clocks: job member
// admission and finish name the Job, child member admission and finish and
// every group transition carry no Job identity, and the stop publications are
// emitted.
func TestObserveBackgroundClocks(t *testing.T) {
	t.Run("job member admit and finish", func(t *testing.T) {
		h, cancel := newCancelableHarness(t, freshSessionStore(t), PreparedExecution{}, newPrepareStub(parkedPrepared()).prepare)
		defer func() {
			cancel()
			_ = h.Wait(context.Background())
		}()
		h.deps.Jobs = stubJobStopper{}
		col := observeHarness(h)

		var completionID string
		{
			err := h.StartJob(context.Background(), testSessionID, jobFixtureID, func(_ context.Context, id string) error {
				completionID = id
				return nil
			})
			testassert.NoError(t, err)
		}
		facts := col.invalidations()
		testassert.Require(t, len(facts) == 1 && facts[0].SessionID == testSessionID && facts[0].JobID == jobFixtureID, "job admission facts = %+v, want one job-attributed invalidation", facts)

		col.reset()
		{
			err := h.DeliverBackgroundCompletion(context.Background(), testSessionID, completionID, "job done")
			testassert.NoError(t, err)
		}
		facts = col.invalidations()
		last := facts[len(facts)-1]
		testassert.Require(t, last.SessionID == testSessionID && last.JobID == jobFixtureID, "job finish fact = %+v, want the job-attributed finish", last)
	})

	t.Run("child member admit, create and finish carry no Job", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript(turn())
		script.gate = make(chan struct{})
		stub := newPrepareStub(modelPrepared(script.model))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		col := observeHarness(h)

		res, err := h.LaunchChildSession(context.Background(), launchRequest())
		testassert.NoError(t, err)
		child := res.ChildSessionID
		facts := col.invalidations()
		var sawParentAdmit, sawChild bool
		childFacts := 0
		for _, fact := range facts {
			testassert.Require(t, fact.JobID == "", "child flow fact carries a Job identity: %+v", fact)
			if fact.SessionID == testSessionID {
				sawParentAdmit = true
			}
			if fact.SessionID == child {
				sawChild = true
				childFacts++
			}
		}
		testassert.Require(t, sawParentAdmit && sawChild && childFacts >= 2, "child flow facts = %+v (parent %v child %v childFacts %d)", facts, sawParentAdmit, sawChild, childFacts)
		// The child's own publications: the durable creation and its run
		// installation. The pair is read after the producer released its lock.
		{
			childPair := snapshotPair(t, h, child)
			testassert.Equal(t, childPair, (SessionRevision{DurableRevision: 2, LocalRevision: 1}))
		}

		rootC := cachedCoordinator(t, h, testSessionID)
		var member *backgroundMember
		rootC.mu.Lock()
		for _, candidate := range rootC.group.members {
			if candidate.kind == memberChild && candidate.id == child {
				member = candidate
			}
		}
		rootC.mu.Unlock()
		testassert.Require(t, member != nil, "the launched child member is absent")
		col.reset()
		h.claimFinishBackgroundMember(rootC, member.completionID)
		waitMemberDone(t, member)
		facts = col.invalidations()
		testassert.Require(t, len(facts) == 1 && facts[0].SessionID == testSessionID && facts[0].JobID == "", "child member finish facts = %+v, want one unattributed invalidation", facts)
	})

	t.Run("stop publications", func(t *testing.T) {
		store := emptyStore(t)
		release := make(chan struct{})
		stub := newPrepareStub(modelPrepared(parkedModel(release)))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		col := observeHarness(h)
		root := createSession(t, h)
		res, err := h.LaunchChildSession(context.Background(), launchRequestFor(root))
		testassert.NoError(t, err)
		completions := map[string]string{}
		stopper := &gatedJobStopper{deliver: func(sessionID, jobID string) {
			h.DeliverBackgroundCompletion(context.Background(), sessionID, completions[jobID], "job done")
		}}
		h.deps.Jobs = stopper
		{
			err := h.StartJob(context.Background(), root, jobFixtureID, func(_ context.Context, completionID string) error {
				completions[jobFixtureID] = completionID
				return nil
			})
			testassert.NoError(t, err)
		}
		col.reset()

		{
			err := h.Stop(context.Background(), root)
			testassert.NoError(t, err)
		}
		facts := col.invalidations()
		var childClosed, jobFinished, rootReopened bool
		for _, fact := range facts {
			if fact.SessionID == res.ChildSessionID && fact.JobID == "" {
				childClosed = true
			}
			if fact.SessionID == root && fact.JobID == jobFixtureID {
				jobFinished = true
			}
			if fact.SessionID == root && fact.JobID == "" {
				rootReopened = true
			}
		}
		testassert.Require(t, childClosed && jobFinished && rootReopened, "stop facts = %+v (child %v job %v reopen %v)", facts, childClosed, jobFinished, rootReopened)
		close(release)
	})
}

// TestObserveDurableAdvances proves every durable Session advance emits its
// invalidation after adoption and lock release, and that the current pair
// reaches the expected owning-state values.
func TestObserveDurableAdvances(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		h := newTestHarness(t, emptyStore(t), nil)
		col := observeHarness(h)
		created, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
		testassert.NoError(t, err)
		facts := col.invalidations()
		testassert.Equal(t, len(facts), 1)
		wantPairAt(t, h, created.Identity.SessionID, 1, 0)
	})

	t.Run("agent type and lifecycle", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), nil)
		col := observeHarness(h)
		{
			_, err := h.ChangeAgentType(context.Background(), testSessionID, "planner")
			testassert.NoError(t, err)
		}
		{
			_, err := h.ArchiveSession(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		{
			_, err := h.ReopenSession(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		{
			_, err := h.ArchiveSession(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		facts := col.invalidations()
		testassert.Equal(t, len(facts), 4)
		wantPairAt(t, h, testSessionID, 5, 0)
		{
			_, err := h.ArchiveSession(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		{
			got := len(col.invalidations())
			testassert.Equal(t, got, 4)
		}
	})

	t.Run("admission", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(validPrepared()).prepare)
		col := observeHarness(h)
		{
			_, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		facts := col.invalidations()
		testassert.Equal(t, len(facts), 3)
		wantPairAt(t, h, testSessionID, 2, 2)
	})

	t.Run("model result and compaction", func(t *testing.T) {
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}
		h, _, c, sessionID := newEffectHarness(t, modelFn)
		col := observeHarness(h)
		base := snapshotPair(t, h, sessionID)
		{
			_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
			testassert.NoError(t, err)
		}
		before := snapshotPair(t, h, sessionID)
		{
			got := col.invalidations()
			testassert.Require(t, len(got) != 0, "model result emitted no invalidation")
		}
		testassert.Require(t, before.DurableRevision > base.DurableRevision, "model result pair = %+v, want a durable advance over %+v", before, base)

		col.reset()
		compactionBase := snapshotPair(t, h, sessionID)
		{
			canonical, _, _, cErr := h.projectCanonical(c, testOpID)
			testassert.NoError(t, cErr)
			err := h.commitCompaction(c, testOpID, testCapture(), hexID(8),
				&maintenanceFacts{input: cloneRequestMessages(canonical), consumed: RequestEvidence{}},
				"summary", 1, nil, false)
			testassert.NoError(t, err)
		}
		facts := col.invalidations()
		testassert.Equal(t, len(facts), 1)
		after := snapshotPair(t, h, sessionID)
		testassert.Require(t, after.DurableRevision > compactionBase.DurableRevision, "compaction pair = %+v, want a durable advance over %+v", after, compactionBase)
	})

	t.Run("fork destination", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript(turn())
		script.gate = make(chan struct{})
		stub := newPrepareStub(modelPrepared(script.model))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		defer script.releaseGate() // a parked destination execution always unblocks
		{
			_, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		c := cachedCoordinator(t, h, testSessionID)
		{
			_, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{terminal: OperationSuccess})
			testassert.NoError(t, err)
		}
		sourceFacts := readSessionFacts(t, h, testSessionID)
		testassert.Require(t, len(sourceFacts) != 0 && sourceFacts[0].Kind == EntryInput, "source facts = %+v", sourceFacts)
		col := observeHarness(h)
		res, err := h.Fork(context.Background(), ForkRequest{
			SourceSessionID: testSessionID,
			BoundaryEntryID: sourceFacts[0].EntryID,
			OperationID:     "fork-op-1",
			Content:         admissionContent("forked"),
		})
		testassert.NoError(t, err)
		dest := res.Session.Identity.SessionID
		destFacts := 0
		for _, fact := range col.invalidations() {
			if fact.SessionID == dest {
				destFacts++
			}
		}
		testassert.Require(t, destFacts >= 2, "fork destination facts = %d, want the durable creation and the local installation", destFacts)
		// The destination's execution is held at the model gate, so the
		// exact pair cannot race its settlement.
		wantPairAt(t, h, dest, 2, 1)
		// Release: the destination completes its fork turn, settles, drains
		// and retires; the completed state proves the Operation ended in
		// success with the durable advance past the held window.
		script.releaseGate()
		final := awaitHarnessQuiet(t, h, dest)
		settled := false
		for _, op := range final.Operations {
			if op.OperationID == "fork-op-1" {
				settled = op.Status == OperationSuccess && op.SettledAt != nil
			}
		}
		testassert.Require(t, settled, "fork destination operations = %+v, want the fork Operation settled success", final.Operations)
		testassert.Require(t, final.Header.Revision.DurableRevision > 2 && final.Header.Revision.LocalRevision > 1, "completed destination pair = {%d %d}, want the settlement's durable advance and the drain's local publications over the held {2 1}", final.Header.Revision.DurableRevision, final.Header.Revision.LocalRevision)
	})
}

// TestObserveRollbackNoDurableHint proves a failed durable transaction emits
// no durable hint while the real reservation bumps still notify.
func TestObserveRollbackNoDurableHint(t *testing.T) {
	store := freshSessionStore(t)
	stub := newPrepareStub(validPrepared())
	h := newTestHarness(t, store, stub.prepare)
	col := observeHarness(h)
	base := snapshotPair(t, h, testSessionID)
	store.txHook = func(step string) error {
		if step == "replace_register" {
			return errors.New("store down")
		}
		return nil
	}
	{
		_, err := h.Submit(context.Background(), SubmitRequest{
			SessionID: testSessionID, OperationID: "op-1", Origin: InputOriginUser,
			Content: admissionContent("hello"), Mode: MessageModeRegular,
		})
		testassert.Require(t, err != nil, "the failing admission succeeded")
	}
	store.txHook = nil
	facts := col.invalidations()
	testassert.Equal(t, len(facts), 2)
	// The two facts are the reservation and its release: their publication
	// pairs are only readable after the producer released its lock, and the
	// final pair proves no durable hint landed.
	{
		got := snapshotPair(t, h, testSessionID)
		testassert.Equal(t, got.DurableRevision, base.DurableRevision)
	}
}

// TestObserveIntentOnly proves an Operation-register-only effect intent emits
// no invalidation.
func TestObserveIntentOnly(t *testing.T) {
	t.Run("model intent", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript(turn())
		script.gate = make(chan struct{})
		stub := newPrepareStub(modelPrepared(script.model))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		{
			_, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		c := cachedCoordinator(t, h, testSessionID)
		col := observeHarness(h)
		done := make(chan struct{})
		go func() {
			defer close(done)
			{
				_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(script.model, nil), invocationExecution(script.model, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
				testassert.Check(t, err == nil, "model effect: %v", err)
			}
		}()
		<-script.arrived // the intent committed and the physical attempt started
		{
			got := col.snapshot()
			testassert.Require(t, len(got) == 0, "the model intent emitted %+v", got)
		}
		script.releaseGate()
		<-done
	})

	t.Run("tool intent", func(t *testing.T) {
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		h, _, c, _ := newEffectHarness(t, modelFn)
		{
			_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
			testassert.NoError(t, err)
		}
		executeStarted := make(chan struct{})
		release := make(chan struct{})
		plan := func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
				close(executeStarted)
				<-release
				return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
			}}
		}
		col := observeHarness(h)
		done := make(chan struct{})
		go func() {
			defer close(done)
			{
				_, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), invocationExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
				testassert.Check(t, err == nil, "tool effect: %v", err)
			}
		}()
		<-executeStarted // the tool intent committed and the executor started
		facts := col.snapshot()
		testassert.Require(t, len(facts) == 1 && facts[0].Kind == FactToolStarted, "tool intent facts = %+v, want only the started fact", facts)
		close(release)
		<-done
	})
}

// TestObserveNoWriteAndDelete proves the no-write paths and deletion emit
// nothing, and deletion leaves no tombstone.
func TestObserveNoWriteAndDelete(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), newPrepareStub(validPrepared()).prepare)
	col := observeHarness(h)
	// idle interrupt
	{
		err := h.Interrupt(context.Background(), testSessionID)
		testassert.NoError(t, err)
	}
	// reopen open
	{
		_, err := h.ReopenSession(context.Background(), testSessionID)
		testassert.NoError(t, err)
	}
	// empty root stop
	{
		err := h.Stop(context.Background(), testSessionID)
		testassert.NoError(t, err)
	}
	{
		got := col.snapshot()
		testassert.Require(t, len(got) == 0, "no-write paths emitted %+v", got)
	}

	{
		_, err := h.ArchiveSession(context.Background(), testSessionID)
		testassert.NoError(t, err)
	}
	col.reset()
	{
		err := h.DeleteSession(context.Background(), testSessionID)
		testassert.NoError(t, err)
	}
	{
		got := col.snapshot()
		testassert.Require(t, len(got) == 0, "deletion emitted %+v", got)
	}
}

// TestObserveSuppression proves a delayed producer's emission after deletion
// or corruption is suppressed.
func TestObserveSuppression(t *testing.T) {
	t.Run("deleted", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(validPrepared()).prepare)
		{
			_, err := h.ArchiveSession(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		c := cachedCoordinator(t, h, testSessionID)
		col := observeHarness(h)
		{
			err := h.DeleteSession(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		h.observeInvalidation(c)
		{
			got := col.snapshot()
			testassert.Require(t, len(got) == 0, "the deleted coordinator emitted %+v", got)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(parkedPrepared()).prepare)
		{
			_, err := h.ReadSessionHeader(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		c := cachedCoordinator(t, h, testSessionID)
		col := observeHarness(h)
		h.markCorrupt(testSessionID, corruptSession(testSessionID, "injected corruption"))
		h.observeInvalidation(c)
		{
			got := col.snapshot()
			testassert.Require(t, len(got) == 0, "the corrupt coordinator emitted %+v", got)
		}
	})
}

// TestObserveCurrentPair proves the passive publication read samples the
// pair current after later publications, rather than a pair captured at a
// producer's lock time.
func TestObserveCurrentPair(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), newPrepareStub(parkedPrepared()).prepare)
	// a real history use establishes the cell
	{
		_, err := h.ReadSessionView(context.Background(), testSessionID)
		testassert.NoError(t, err)
	}
	c := cachedCoordinator(t, h, testSessionID)
	col := observeHarness(h)

	// A producer's publication whose emission is delayed.
	c.mu.Lock()
	c.bumpLocalRevision()
	c.mu.Unlock()
	// A later publication lands before the delayed emission.
	release, err := h.reserve(context.Background(), c)
	testassert.NoError(t, err)
	release()

	col.reset()
	h.observeInvalidation(c) // the delayed producer's emission
	facts := col.invalidations()
	testassert.Equal(t, len(facts), 1)
	// The passive publication read used by passive publishers samples
	// the pair current at publication time, after the later publications
	// above landed.
	current := snapshotPair(t, h, testSessionID)
	identity, pair, ok := h.ReadObservation(testSessionID)
	testassert.Require(t, ok && identity.SessionID == testSessionID && pair == current, "ReadObservation = %+v %+v %v, want the current pair %+v", identity, pair, ok, current)
}

// TestObserveConcurrentCommits proves one parked observer callback holds no
// lock: a concurrent durable publication completes while the callback waits,
// both facts keep their own Session identity, and the current pair reaches
// the expected owning-state value after both commits.
func TestObserveConcurrentCommits(t *testing.T) {
	h, _, c, sessionID := newEffectHarness(t, nil)
	before := snapshotPair(t, h, sessionID)

	col := &factCollector{}
	firstArrived := make(chan struct{})
	releaseFirst := make(chan struct{})
	var first atomic.Bool
	h.deps.Observe = func(fact HarnessFact) {
		col.observe(fact)
		if first.CompareAndSwap(false, true) {
			close(firstArrived)
			<-releaseFirst
		}
	}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		{
			_, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{})
			testassert.Check(t, err == nil, "first commit: %v", err)
		}
	}()
	<-firstArrived // the first commit emitted and its callback is parked

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		{
			_, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{})
			testassert.Check(t, err == nil, "second commit: %v", err)
		}
	}()
	select {
	case <-secondDone: // the concurrent publication completed while the callback was parked
	case <-time.After(10 * time.Second):
		t.Fatalf("the concurrent publication did not complete while the observer was parked")
	}
	close(releaseFirst)
	<-firstDone

	got := col.snapshot()
	testassert.Require(t, len(got) == 2, "concurrent commit facts = %+v, want two", got)
	for i, fact := range got {
		testassert.Require(t, fact.Kind == FactInvalidation && fact.SessionID == sessionID, "fact %d lost its owner attribution: %+v", i, fact)
	}
	// Both commits durably advanced; the pair is read after both producers
	// released their locks, so the publisher's current-pair rule is proven
	// against the concurrent interleaving.
	wantPairAt(t, h, sessionID, before.DurableRevision+2, before.LocalRevision)
}

// transactionProbe wraps one Storage and records whether a transaction is
// currently executing, so the observer can prove it never runs inside one.
type transactionProbe struct {
	Storage
	mu         sync.Mutex
	inTx       bool
	violations int
	readErrs   []error
	h          *Harness
}

func (p *transactionProbe) Transact(ctx context.Context, fn func(Transaction) error) error {
	p.mu.Lock()
	p.inTx = true
	p.mu.Unlock()
	err := p.Storage.Transact(ctx, fn)
	p.mu.Lock()
	p.inTx = false
	p.mu.Unlock()
	return err
}

func (p *transactionProbe) observe(fact HarnessFact) {
	p.mu.Lock()
	inTx := p.inTx
	p.mu.Unlock()
	if inTx {
		p.mu.Lock()
		p.violations++
		p.mu.Unlock()
	}
	if _, err := p.h.ReadSessionHeader(context.Background(), fact.SessionID); err != nil {
		p.mu.Lock()
		p.readErrs = append(p.readErrs, err)
		p.mu.Unlock()
	}
	if _, err := p.h.ReadSessionView(context.Background(), fact.SessionID); err != nil {
		p.mu.Lock()
		p.readErrs = append(p.readErrs, err)
		p.mu.Unlock()
	}
}

// TestObserveReentrancy proves the callback may read the Harness and never runs
// inside a storage transaction or a state lock.
func TestObserveReentrancy(t *testing.T) {
	probe := &transactionProbe{Storage: freshSessionStore(t)}
	stub := newPrepareStub(modelPrepared(quickModel()))
	h, cancel := newCancelableHarness(t, probe, PreparedExecution{}, stub.prepare)
	defer cancel()
	probe.h = h
	h.deps.Observe = probe.observe

	{
		_, err := h.Submit(context.Background(), SubmitRequest{
			SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser,
			Content: admissionContent("hello"), Mode: MessageModeRegular,
		})
		testassert.NoError(t, err)
	}
	awaitHarnessQuiet(t, h, testSessionID)

	probe.mu.Lock()
	violations, readErrs := probe.violations, append([]error(nil), probe.readErrs...)
	probe.mu.Unlock()
	testassert.Equal(t, violations, 0)
	testassert.Require(t, len(readErrs) == 0, "the observer's reentrant reads failed: %v", readErrs)
}

// duplexStream returns one delta and one error from the same read.
type duplexStream struct {
	delta model.StreamDelta
	err   error
}

func (s *duplexStream) Recv() (model.StreamDelta, error) { return s.delta, s.err }
func (s *duplexStream) Close() error                     { return nil }

// TestObserveStreamFilter proves the accepted-stream wrapper's filter,
// preservation, and close delegation.
func TestObserveStreamFilter(t *testing.T) {
	type emitted struct {
		kind     HarnessFactKind
		position int
		content  string
	}
	collect := func(t *testing.T, inner model.Stream) (*observedStream, *[]emitted) {
		t.Helper()
		var got []emitted
		wrapper := observedStream{inner: inner, emit: func(kind HarnessFactKind, position int, content string) {
			got = append(got, emitted{kind: kind, position: position, content: content})
		}}
		return &wrapper, &got
	}

	t.Run("text only", func(t *testing.T) {
		inner := streamOf(model.StreamDelta{HasChoice: true, ContentFragments: []model.ContentFragment{
			{Position: 0, Kind: model.PartText, Text: "a"},
			{Position: 1, Kind: model.PartText, Text: ""},
			{Position: 2, Kind: model.PartImageURL, URL: "u"},
		}})
		wrapper, got := collect(t, inner)
		{
			_, err := wrapper.Recv()
			testassert.NoError(t, err)
		}
		testassert.Require(t, len(*got) == 1 && (*got)[0] == (emitted{kind: FactTextDelta, position: 0, content: "a"}), "emitted = %+v, want only the nonempty text fragment", *got)
	})

	t.Run("refusal only", func(t *testing.T) {
		inner := streamOf(model.StreamDelta{HasChoice: true, Role: "assistant", RefusalFragment: "I cannot"})
		wrapper, got := collect(t, inner)
		{
			_, err := wrapper.Recv()
			testassert.NoError(t, err)
		}
		testassert.Require(t, len(*got) == 1 && (*got)[0] == (emitted{kind: FactRefusalDelta, position: 0, content: "I cannot"}), "emitted = %+v, want only the nonempty refusal fragment without a position", *got)
	})

	t.Run("mixed delta emits content text before refusal", func(t *testing.T) {
		inner := streamOf(model.StreamDelta{
			HasChoice:       true,
			Role:            "assistant",
			RefusalFragment: "but no",
			ContentFragments: []model.ContentFragment{
				{Position: 0, Kind: model.PartText, Text: "working"},
				{Position: 1, Kind: model.PartImageURL, URL: "u"},
				{Position: 2, Kind: model.PartText, Text: "harder"},
			},
		})
		wrapper, got := collect(t, inner)
		{
			_, err := wrapper.Recv()
			testassert.NoError(t, err)
		}
		want := []emitted{
			{kind: FactTextDelta, position: 0, content: "working"},
			{kind: FactTextDelta, position: 2, content: "harder"},
			{kind: FactRefusalDelta, position: 0, content: "but no"},
		}
		testassert.Require(t, slices.Equal(*got, want), "emitted = %+v, want the content-text emissions before the refusal %+v", *got, want)
	})

	t.Run("empty refusal emits only the text", func(t *testing.T) {
		inner := streamOf(model.StreamDelta{
			HasChoice:       true,
			RefusalFragment: "",
			ContentFragments: []model.ContentFragment{
				{Position: 0, Kind: model.PartText, Text: "kept"},
			},
		})
		wrapper, got := collect(t, inner)
		{
			_, err := wrapper.Recv()
			testassert.NoError(t, err)
		}
		testassert.Require(t, len(*got) == 1 && (*got)[0].kind == FactTextDelta, "emitted = %+v, want the text fragment and no refusal emission", *got)
	})

	t.Run("original delta and error preserved", func(t *testing.T) {
		delta := model.StreamDelta{HasChoice: true, ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "x"}}}
		inner := &duplexStream{delta: delta, err: errors.New("read failed")}
		wrapper, got := collect(t, inner)
		returned, err := wrapper.Recv()
		testassert.Require(t, err != nil && err.Error() == "read failed", "error = %v, want the original read failure", err)
		testassert.Require(t, len(returned.ContentFragments) == 1 && &returned.ContentFragments[0] == &delta.ContentFragments[0], "the original delta was not preserved: %+v", returned)
		testassert.Require(t, len(*got) == 0, "a delta returned with an error emitted %+v", *got)
	})

	t.Run("refusal with an error emits none", func(t *testing.T) {
		inner := &duplexStream{delta: model.StreamDelta{HasChoice: true, RefusalFragment: "no"}, err: errors.New("read failed")}
		wrapper, got := collect(t, inner)
		{
			_, err := wrapper.Recv()
			testassert.Require(t, err != nil, "%v", "Recv: want the original read failure")
		}
		testassert.Require(t, len(*got) == 0, "a refusal delta returned with an error emitted %+v", *got)
	})

	t.Run("choiceless empty nontext and invalid emit none", func(t *testing.T) {
		cases := []model.StreamDelta{
			{ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "x"}}}, // no choice
			{RefusalFragment: "no"}, // refusal without a choice
			{HasChoice: true},       // empty
			{HasChoice: true, ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartImageURL, URL: "u"}}},                      // non-text
			{HasChoice: true, ContentFragments: []model.ContentFragment{{Position: -1, Kind: model.PartText, Text: "x"}}},                        // invalid position
			{HasChoice: true, ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "x", URL: "u"}}},               // cross-kind
			{HasChoice: true, RefusalFragment: "no", ContentFragments: []model.ContentFragment{{Position: -1, Kind: model.PartText, Text: "x"}}}, // invalid with refusal
		}
		for i, delta := range cases {
			wrapper, got := collect(t, streamOf(delta))
			{
				_, err := wrapper.Recv()
				testassert.Require(t, err == nil, "case %d Recv: %v", i, err)
			}
			testassert.Require(t, len(*got) == 0, "case %d emitted %+v", i, *got)
		}
	})

	t.Run("valid delta pairs with choice-less and usage-only twins", func(t *testing.T) {
		textDelta := model.StreamDelta{
			HasChoice:       true,
			RefusalFragment: "but no",
			ContentFragments: []model.ContentFragment{
				{Position: 3, Kind: model.PartText, Text: "working"},
			},
		}
		choiceless := textDelta
		choiceless.HasChoice = false // the same fragments and refusal without a choice.
		usageOnly := model.StreamDelta{Usage: &model.Usage{InputTokens: 1, OutputTokens: 2}}
		rows := []struct {
			name      string
			delta     model.StreamDelta
			want      []emitted
			checkOrig func(t *testing.T, returned model.StreamDelta)
		}{
			{
				name:  "choice-bearing emits text then refusal",
				delta: textDelta,
				want:  []emitted{{kind: FactTextDelta, position: 3, content: "working"}, {kind: FactRefusalDelta, position: 0, content: "but no"}},
				checkOrig: func(t *testing.T, returned model.StreamDelta) {
					testassert.Require(t, &returned.ContentFragments[0] == &textDelta.ContentFragments[0], "%v", "the original delta was not returned unchanged")
				},
			},
			{
				name:  "same delta choice-less emits none",
				delta: choiceless,
				checkOrig: func(t *testing.T, returned model.StreamDelta) {
					testassert.Require(t, &returned.ContentFragments[0] == &choiceless.ContentFragments[0], "%v", "the original choice-less delta was not returned unchanged")
				},
			},
			{
				name:  "usage-only choice-less emits none and still passes through",
				delta: usageOnly,
				checkOrig: func(t *testing.T, returned model.StreamDelta) {
					testassert.Require(t, returned.Usage == usageOnly.Usage, "%v", "the original usage pointer was not returned unchanged")
				},
			},
			{
				name:  "invalid tool position suppresses valid-looking text",
				delta: model.StreamDelta{HasChoice: true, ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "x"}}, ToolFragments: []model.ToolCallFragment{{ID: "c", Name: "f", Position: &[]int{-2}[0]}}},
			},
			{
				name:  "malformed tool extra suppresses valid-looking text",
				delta: model.StreamDelta{HasChoice: true, ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "x"}}, ToolFragments: []model.ToolCallFragment{{ID: "c", Name: "f", Extra: model.Extra{"k": json.RawMessage(`{oops`)}}}},
			},
		}
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				wrapper, got := collect(t, streamOf(row.delta))
				returned, err := wrapper.Recv()
				testassert.NoError(t, err)
				testassert.Require(t, slices.Equal(*got, row.want), "emitted = %+v, want %+v", *got, row.want)
				if row.checkOrig != nil {
					row.checkOrig(t, returned)
				}
			})
		}
	})

	t.Run("close delegates once", func(t *testing.T) {
		inner := streamOf()
		wrapper, _ := collect(t, inner)
		{
			err := wrapper.Close()
			testassert.NoError(t, err)
		}
		testassert.Equal(t, inner.closes, 1)
	})

	t.Run("assembly close stays exactly once through the wrapper", func(t *testing.T) {
		stream := completedTurnStream()
		modelFn := func(context.Context, model.Request) (model.Stream, error) { return stream, nil }
		h, _, c, _ := newEffectHarness(t, modelFn)
		observeHarness(h)
		{
			_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
			testassert.NoError(t, err)
		}
		testassert.Equal(t, stream.closes, 1)
	})

	t.Run("eof emits none", func(t *testing.T) {
		inner := streamOf()
		wrapper, got := collect(t, inner)
		{
			_, err := wrapper.Recv()
			testassert.Require(t, errors.Is(err, io.EOF), "EOF = %v", err)
		}
		testassert.Require(t, len(*got) == 0, "EOF emitted %+v", *got)
	})
}

// TestObserveCompactAttribution proves a compact model stream's deltas are
// attributed to the enclosing Operation.
func TestObserveCompactAttribution(t *testing.T) {
	compactFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(), nil
	}
	h, _, c, sessionID := newEffectHarness(t, nil)
	col := observeHarness(h)
	exec := effectExecution(compactFn, nil)
	var reported *UsageCount
	effect := h.compactModelEffect(c, testOpID, invocationExecution(compactFn, nil), exec.Retry, testCapture(), hexID(8), &maintenanceFacts{}, nil, &reported)
	{
		_, err := invokeModelEffect(t, effect, defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
	deltas := 0
	for _, fact := range col.snapshot() {
		if fact.Kind != FactTextDelta {
			continue
		}
		deltas++
		testassert.Require(t, fact.SessionID == sessionID && fact.OperationID == testOpID && fact.Position == 0 && fact.Content == "done", "compact delta fact = %+v, want the enclosing Operation attribution", fact)
	}
	testassert.Equal(t, deltas, 1)
}

// TestObserveRefusalFacts proves a real accepted refusal stream emits one
// refusal fact per nonempty fragment in arrival order — attributed to the
// enclosing Operation, carrying the content and no position — and that the
// committed assistant entry retains the assembled refusal: the live facts
// never replace the durable value.
func TestObserveRefusalFacts(t *testing.T) {
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return streamOf(
			model.StreamDelta{HasChoice: true, Role: "assistant", RefusalFragment: "I cannot"},
			model.StreamDelta{HasChoice: true, RefusalFragment: " help with that."},
			model.StreamDelta{HasChoice: true, FinishReason: "stop"},
		), nil
	}
	h, _, c, sessionID := newEffectHarness(t, modelFn)
	col := observeHarness(h)
	{
		_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
	var refusals []HarnessFact
	for _, fact := range col.snapshot() {
		if fact.Kind != FactRefusalDelta {
			continue
		}
		refusals = append(refusals, fact)
	}
	want := []string{"I cannot", " help with that."}
	testassert.Require(t, len(refusals) == len(want), "refusal facts = %+v, want one per fragment %v", refusals, want)
	for i, fact := range refusals {
		testassert.Require(t, fact.SessionID == sessionID && fact.OperationID == testOpID && fact.Content == want[i] && fact.Position == 0 && fact.JobID == "" && fact.CallID == "" && fact.Ordinal == 0 && fact.Name == "" && fact.Status == "", "refusal fact %d = %+v, want the enclosed Operation attribution and no position", i, fact)
	}
	// Final refusal retention: the committed assistant entry keeps the
	// fragments joined in arrival order.
	facts := readSessionFacts(t, h, sessionID)
	retained := false
	for _, fact := range facts {
		if fact.Assistant == nil {
			continue
		}
		testassert.Equal(t, fact.Assistant.Refusal, "I cannot help with that.")
		retained = true
	}
	testassert.Require(t, retained, "no committed assistant fact in %+v", facts)
}

// TestObserveToolLifecycle proves the tool lifecycle: one started fact at the
// live dispatcher entry before validation/hooks/permission/execution, one
// finished fact after the shared commit for all four statuses, and a finished
// fact without a started one for a terminal settlement's synthetic unstarted
// result. Hooks have no progress kind.
func TestObserveToolLifecycle(t *testing.T) {
	publishStream := func(t *testing.T, h *Harness, c *coordinator, stream model.Stream) {
		t.Helper()
		modelFn := func(context.Context, model.Request) (model.Stream, error) { return stream, nil }
		{
			_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
			testassert.NoError(t, err)
		}
	}
	publishCall := func(t *testing.T, h *Harness, c *coordinator, call model.ToolCall) {
		t.Helper()
		publishStream(t, h, c, completedTurnStream(call))
	}

	t.Run("statuses share one lifecycle", func(t *testing.T) {
		successPlan := func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
		}
		errorPlan := func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "boom"}}}
		}
		deniedPlan := func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Execute: func(context.Context) ToolOutcome {
				return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
			}}
		}
		cases := []struct {
			name   string
			plan   func(context.Context, model.ToolCall) PreparedTool
			status model.ToolResultStatus
			cancel bool
		}{
			{name: "success", plan: successPlan, status: model.ResultSuccess},
			{name: "error", plan: errorPlan, status: model.ResultError},
			{name: "denied", plan: deniedPlan, status: model.ResultDenied},
			{name: "interrupted", plan: successPlan, status: model.ResultInterrupted, cancel: true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h, _, c, _ := newEffectHarness(t, nil)
				publishCall(t, h, c, testToolCall("call-1"))
				col := observeHarness(h)
				ctx := context.Background()
				if tc.cancel {
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					ctx = canceled
				}
				got, err := h.toolEffect(c, testOpID, effectExecution(nil, tc.plan), invocationExecution(nil, tc.plan), testCapture())(ctx, testToolCall("call-1"), []string{"echo"})
				testassert.NoError(t, err)
				testassert.Equal(t, got.Status, tc.status)
				facts := col.snapshot()
				var startedAt, finishedAt = -1, -1
				for i, fact := range facts {
					switch fact.Kind {
					case FactToolStarted:
						testassert.Require(t, startedAt < 0, "the call started more than once: %+v", facts)
						startedAt = i
						testassert.Require(t, fact.CallID == "call-1" && fact.Ordinal == 0 && fact.Name == "echo" && fact.OperationID == testOpID, "started fact = %+v", fact)
					case FactToolFinished:
						testassert.Require(t, finishedAt < 0, "the call finished more than once: %+v", facts)
						finishedAt = i
						testassert.Require(t, fact.CallID == "call-1" && fact.Status == tc.status && fact.OperationID == testOpID, "finished fact = %+v, want status %s", fact, tc.status)
					case FactInvalidation:
					default:
						t.Fatalf("unexpected progress kind %q in %+v", fact.Kind, facts)
					}
				}
				testassert.Require(t, startedAt >= 0 && finishedAt >= 0 && startedAt <= finishedAt, "lifecycle order = started %d finished %d in %+v", startedAt, finishedAt, facts)
			})
		}
	})

	t.Run("started precedes the advertisement gate", func(t *testing.T) {
		h, _, c, _ := newEffectHarness(t, nil)
		position := 0
		stream := streamOf(model.StreamDelta{
			HasChoice:    true,
			Role:         "assistant",
			FinishReason: "tool_calls",
			ContentFragments: []model.ContentFragment{{
				Position: 0, Kind: model.PartText, Text: "done",
			}},
			ToolFragments: []model.ToolCallFragment{{
				Position: &position, ID: "call-1", Name: "ghost", ArgumentFragment: `{"x":1}`,
			}},
		}, usageDelta(model.Usage{InputTokens: 1, OutputTokens: 1}))
		publishStream(t, h, c, stream)
		col := observeHarness(h)
		got, err := h.toolEffect(c, testOpID, effectExecution(nil, nil), invocationExecution(nil, nil), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.Require(t, err == nil && got.Status == model.ResultError, "unavailable tool = %+v err %v, want the error result", got, err)
		facts := col.snapshot()
		testassert.Require(t, len(facts) == 3 && facts[0].Kind == FactToolStarted && facts[1].Kind == FactInvalidation && facts[2].Kind == FactToolFinished, "advertisement-gate facts = %+v, want started, the result commit, then finished", facts)
	})

	t.Run("synthetic unstarted result finishes without started", func(t *testing.T) {
		h, _, c, _ := newEffectHarness(t, nil)
		publishStream(t, h, c, completedTurnStream(testToolCall("call-1"), testToolCall("call-2")))
		successPlan := func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
		}
		{
			_, err := h.toolEffect(c, testOpID, effectExecution(nil, successPlan), invocationExecution(nil, successPlan), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
			testassert.NoError(t, err)
		}
		col := observeHarness(h)
		{
			_, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{terminal: OperationInterruption, detail: "stopped"})
			testassert.NoError(t, err)
		}
		facts := col.snapshot()
		var finished int
		for _, fact := range facts {
			switch fact.Kind {
			case FactToolStarted:
				t.Fatalf("a synthetic unstarted result emitted a started fact: %+v", fact)
			case FactToolFinished:
				finished++
				testassert.Require(t, fact.CallID == "call-2" && fact.Status == model.ResultInterrupted, "synthetic finished fact = %+v", fact)
			}
		}
		testassert.Require(t, finished == 1, "synthetic finished facts = %d, want one; facts %+v", finished, facts)
	})

	t.Run("hooks have no progress kind", func(t *testing.T) {
		h, _, c, _ := newEffectHarness(t, nil)
		publishCall(t, h, c, testToolCall("call-1"))
		toolFn := func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
		}
		hooks := []ToolArgumentsHook{{ID: "h1", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			return nil, errors.New("hook boom")
		}}}
		exec := Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
			return InvocationExecution{Tool: toolFn, NormalizeTool: objectNormalize, ToolHooks: hooks}, nil
		}}
		col := observeHarness(h)
		got, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.Require(t, err == nil && got.Status == model.ResultError, "hook-settled tool = %+v err %v, want the error result", got, err)
		var started, finished, invalidations int
		for _, fact := range col.snapshot() {
			switch fact.Kind {
			case FactToolStarted:
				started++
			case FactToolFinished:
				finished++
			case FactInvalidation:
				invalidations++
			default:
				t.Fatalf("hook flow emitted progress kind %q", fact.Kind)
			}
		}
		// the hook result and the tool result
		testassert.Require(t, started == 1 && finished == 1 && invalidations == 2, "hook flow counts = started %d finished %d invalidations %d", started, finished, invalidations)
	})
}

// TestObserveRematerializationNoHint proves a foreign-revision rematerialization
// emits no replay hint.
func TestObserveRematerializationNoHint(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, newPrepareStub(parkedPrepared()).prepare)
	leaseCoordinator(t, h, testSessionID) // the controlled warm view the foreign write races
	// a real history use under the held lease
	{
		_, err := h.ReadHistoryView(context.Background(), testSessionID)
		testassert.NoError(t, err)
	}
	col := observeHarness(h)
	advanceSessionRevision(t, store, testSessionID) // the foreign writer
	{
		_, err := h.ChangeAgentType(context.Background(), testSessionID, "planner")
		testassert.Require(t, errors.Is(err, ErrConflict), "foreign-revision change = %v, want the conflict class", err)
	}
	{
		got := col.snapshot()
		testassert.Require(t, len(got) == 0, "rematerialization emitted %+v", got)
	}
}

// TestObserveRestartNoReplay proves recovery and a new owner's reads emit no
// replayed facts.
func TestObserveRestartNoReplay(t *testing.T) {
	store := freshSessionStore(t)
	stub := newPrepareStub(modelPrepared(quickModel()))
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
	defer func() {
		cancel()
		_ = h.Wait(context.Background())
	}()
	col := observeHarness(h)
	{
		_, err := h.Submit(context.Background(), SubmitRequest{
			SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser,
			Content: admissionContent("hello"), Mode: MessageModeRegular,
		})
		testassert.NoError(t, err)
	}
	awaitHarnessQuiet(t, h, testSessionID)
	testassert.Require(t, len(col.snapshot()) != 0, "the first owner emitted no facts")
	cancel()
	{
		err := h.Wait(context.Background())
		testassert.NoError(t, err)
	}
	{
		err := Recover(context.Background(), store)
		testassert.NoError(t, err)
	}

	sink := &factCollector{}
	restarted, err := New(context.Background(), Dependencies{Storage: store, Prepare: stub.prepare, Observe: sink.observe})
	testassert.NoError(t, err)
	{
		_, err := restarted.ReadSessionHeader(context.Background(), testSessionID)
		testassert.NoError(t, err)
	}
	{
		_, err := restarted.ReadHistoryView(context.Background(), testSessionID)
		testassert.NoError(t, err)
	}
	{
		_, err := restarted.ListSessions(context.Background())
		testassert.NoError(t, err)
	}
	{
		got := sink.snapshot()
		testassert.Require(t, len(got) == 0, "the new owner's reads replayed %+v", got)
	}
	{
		_, err := restarted.ChangeAgentType(context.Background(), testSessionID, "planner")
		testassert.NoError(t, err)
	}
	{
		got := sink.invalidations()
		testassert.Require(t, len(got) == 1, "the new owner's first publication emitted %+v", got)
	}
}

// observationCountingStorage fails every read and counts every access: the
// passive observation read must perform no storage work at all.
type observationCountingStorage struct {
	Storage
	calls int
}

func (s *observationCountingStorage) ReadEntries(ctx context.Context, sessionID string, after int64) ([]Entry, error) {
	s.calls++
	return s.Storage.ReadEntries(ctx, sessionID, after)
}

func (s *observationCountingStorage) ReadRegister(ctx context.Context, key RegisterKey) (Register, error) {
	s.calls++
	return s.Storage.ReadRegister(ctx, key)
}

func (s *observationCountingStorage) ReadRegisters(ctx context.Context, sessionID string) ([]Register, error) {
	s.calls++
	return s.Storage.ReadRegisters(ctx, sessionID)
}

// TestReadObservation pins the passive observation read: a materialized
// coordinator returns its retained identity (with its Workspace) and the
// current pair whether or not its graph is resident; a never-materialized,
// deleted, and corrupt Session report false without any storage access; a
// cold Session is never materialized by the read.
func TestReadObservation(t *testing.T) {
	t.Run("valid coordinator with a released graph", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(validPrepared()).prepare)
		{
			_, err := h.ReadSessionView(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		cell := cachedCoordinator(t, h, testSessionID)
		cell.mu.Lock()
		graphless := cell.graph == nil
		cell.mu.Unlock()
		testassert.Require(t, graphless, "the read's graph stayed resident, want the released state")
		identity, revision, ok := h.ReadObservation(testSessionID)
		testassert.Require(t, ok, "%v", "ReadObservation on a released established coordinator reported unavailable")
		testassert.Require(t, identity.SessionID == testSessionID && identity.Workspace != "", "got %v, want %v; %v", identity.SessionID, testSessionID, "identity carries no workspace")
		{
			want := snapshotPair(t, h, testSessionID)
			testassert.Equal(t, revision, want)
		}
	})

	t.Run("never materialized performs no storage work", func(t *testing.T) {
		store := &observationCountingStorage{Storage: freshSessionStore(t)}
		h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
		{
			_, _, ok := h.ReadObservation(testSessionID)
			testassert.Require(t, !ok, "%v", "ReadObservation on a never-materialized Session reported available")
		}
		testassert.Equal(t, store.calls, 0)
		// The read left the registry cold: a repeat miss performs no work and
		// the Session stays unmaterialized.
		{
			_, _, ok := h.ReadObservation(testSessionID)
			testassert.Require(t, !ok, "%v", "repeat ReadObservation materialized the Session")
		}
		testassert.Equal(t, store.calls, 0)
	})

	t.Run("deleted", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(validPrepared()).prepare)
		{
			_, err := h.ArchiveSession(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		{
			err := h.DeleteSession(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		{
			_, _, ok := h.ReadObservation(testSessionID)
			testassert.Require(t, !ok, "%v", "ReadObservation after deletion reported available")
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(parkedPrepared()).prepare)
		{
			_, err := h.ReadSessionHeader(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		h.markCorrupt(testSessionID, corruptSession(testSessionID, "injected corruption"))
		{
			_, _, ok := h.ReadObservation(testSessionID)
			testassert.Require(t, !ok, "%v", "ReadObservation on a corrupt Session reported available")
		}
	})
}
