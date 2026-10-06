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
// public snapshot.
func snapshotPair(t *testing.T, h *Harness, sessionID string) SessionRevision {
	t.Helper()
	snap, err := h.SnapshotSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("SnapshotSession: %v", err)
	}
	return SessionRevision{DurableRevision: snap.Session.Revision, LocalRevision: snap.LocalRevision}
}

// wantPairAt asserts that one Session's current revision pair equals the
// expected value; the pair is read after the producer released its lock, the
// same publication-time rule the passive publishers use.
func wantPairAt(t *testing.T, h *Harness, sessionID string, durable int64, local uint64) {
	t.Helper()
	want := SessionRevision{DurableRevision: durable, LocalRevision: local}
	if got := snapshotPair(t, h, sessionID); got != want {
		t.Fatalf("session pair = %+v, want %+v", got, want)
	}
}

// awaitHarnessQuiet waits until one Session has no current Operation and no
// installed run, the deterministic post-terminal barrier.
func awaitHarnessQuiet(t *testing.T, h *Harness, sessionID string) SessionSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap, err := h.SnapshotSession(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("SnapshotSession: %v", err)
		}
		if !snap.ExecutionBusy && snap.Session.State.CurrentOperationID == "" {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s never went quiet", sessionID)
		}
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
	if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err != nil {
		t.Fatalf("model effect: %v", err)
	}
	plan := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
	}
	if _, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-1")); err != nil {
		t.Fatalf("tool effect: %v", err)
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
			if fact.SessionID != sessionID || fact.OperationID != "" || fact.JobID != "" ||
				fact.Position != 0 || fact.Content != "" || fact.CallID != "" || fact.Ordinal != 0 ||
				fact.Name != "" || fact.Status != "" {
				t.Fatalf("invalidation fact carries unrelated fields: %+v", fact)
			}
		case FactTextDelta:
			deltas++
			if fact.SessionID != sessionID || fact.OperationID != testOpID || fact.Position != 0 ||
				fact.Content != "done" || fact.JobID != "" || fact.CallID != "" || fact.Ordinal != 0 ||
				fact.Name != "" || fact.Status != "" {
				t.Fatalf("text_delta fact carries unrelated fields: %+v", fact)
			}
		case FactToolStarted:
			started++
			if fact.SessionID != sessionID || fact.OperationID != testOpID || fact.CallID != "call-1" ||
				fact.Ordinal != 0 || fact.Name != "echo" || fact.JobID != "" || fact.Position != 0 ||
				fact.Content != "" || fact.Status != "" {
				t.Fatalf("tool_started fact carries unrelated fields: %+v", fact)
			}
		case FactToolFinished:
			finished++
			if fact.SessionID != sessionID || fact.OperationID != testOpID || fact.CallID != "call-1" ||
				fact.Status != model.ResultSuccess || fact.JobID != "" || fact.Position != 0 ||
				fact.Content != "" || fact.Ordinal != 0 || fact.Name != "" {
				t.Fatalf("tool_finished fact carries unrelated fields: %+v", fact)
			}
		default:
			t.Fatalf("fact kind %q is outside the closed set", fact.Kind)
		}
	}
	if invalidations < 2 || deltas != 1 || started != 1 || finished != 1 {
		t.Fatalf("fact counts = invalidations %d deltas %d started %d finished %d", invalidations, deltas, started, finished)
	}
}

// TestObserveReservationClocks proves each reservation acquisition and release
// emits one local-only invalidation and nothing durable.
func TestObserveReservationClocks(t *testing.T) {
	h, _, c, sessionID := newEffectHarness(t, nil)
	col := observeHarness(h)
	base := snapshotPair(t, h, sessionID)

	release, err := h.reserve(context.Background(), c)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	release()

	facts := col.invalidations()
	if len(facts) != 2 {
		t.Fatalf("reservation facts = %d, want exactly the acquisition and release", len(facts))
	}
	wantPairAt(t, h, sessionID, base.DurableRevision, base.LocalRevision+2)
	for _, fact := range facts {
		if fact.SessionID != sessionID || fact.JobID != "" || fact.OperationID != "" {
			t.Fatalf("reservation fact carries unrelated identity: %+v", fact)
		}
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
		if _, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello")); disposition != DispositionAdmitted {
			t.Fatalf("disposition %q, want admitted", disposition)
		}
		c := cachedCoordinator(t, h, testSessionID)
		col := observeHarness(h)
		base := snapshotPair(t, h, testSessionID)

		h.startExecution(c, testOpID, modelPrepared(script.model))
		facts := col.invalidations()
		if len(facts) != 1 {
			t.Fatalf("installation facts = %d, want one", len(facts))
		}
		wantPairAt(t, h, testSessionID, base.DurableRevision, base.LocalRevision+1) // the run slot's installation

		c.mu.Lock()
		run := c.run
		c.mu.Unlock()
		if run == nil {
			t.Fatalf("the run slot was not installed")
		}
		script.releaseGate()
		select {
		case <-run.done:
		case <-time.After(10 * time.Second):
			t.Fatalf("the execution never converged")
		}
		final := snapshotPair(t, h, testSessionID)
		if final.DurableRevision <= base.DurableRevision || final.LocalRevision <= base.LocalRevision {
			t.Fatalf("converged pair = %+v, want the model result's durable advance and the drain's local publications over %+v", final, base)
		}
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
		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("op-1 submit: %v", err)
		}
		if _, err := submitText(t, h, session, "op-2", MessageModeQueued, "queued"); err != nil {
			t.Fatalf("op-2 submit: %v", err)
		}
		c := cachedCoordinator(t, h, session)
		col := observeHarness(h)
		base := snapshotPair(t, h, session)

		close(gate1)
		c.mu.Lock()
		run1 := c.run
		c.mu.Unlock()
		if run1 == nil {
			t.Fatalf("run1 was not installed")
		}
		select {
		case <-run1.done:
		case <-time.After(10 * time.Second):
			t.Fatalf("run1 never converged")
		}
		// The queued successor was installed by the drain and is parked at
		// gate2: the predecessor's tail must not have cleared its slot.
		snap, err := h.SnapshotSession(context.Background(), session)
		if err != nil {
			t.Fatalf("SnapshotSession: %v", err)
		}
		if !snap.ExecutionBusy {
			t.Fatalf("the drain-installed successor slot was cleared by its predecessor")
		}
		close(gate2)
		final := awaitHarnessQuiet(t, h, session)
		facts := col.invalidations()
		if len(facts) < 2 {
			t.Fatalf("successor facts = %d, want at least the successor's retire and the predecessor's release", len(facts))
		}
		if final.Session.Revision <= base.DurableRevision || final.LocalRevision <= base.LocalRevision {
			t.Fatalf("successor pair = %+v, want the successor's durable admission and its local publications over %+v", final, base)
		}
	})
}

// TestObserveBufferClocks proves the buffer clocks: the active enqueue, the
// steering pop with its durable delivery, the drain pop, and the Harness-loss
// discard each emit exactly when the counter advances.
func TestObserveBufferClocks(t *testing.T) {
	t.Run("active enqueue and steering pop", func(t *testing.T) {
		h, _, c, sessionID := newEffectHarness(t, nil)
		col := observeHarness(h)
		base := snapshotPair(t, h, sessionID)

		// The active route holds the reservation across the enqueue.
		if _, err := h.Submit(context.Background(), SubmitRequest{
			SessionID: sessionID, OperationID: "op-steer", Origin: InputOriginUser,
			Content: admissionContent("steer"), Mode: MessageModeRegular,
		}); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		facts := col.invalidations()
		if len(facts) != 3 {
			t.Fatalf("active submit facts = %d, want reserve, enqueue and release", len(facts))
		}
		wantPairAt(t, h, sessionID, base.DurableRevision, base.LocalRevision+3)

		col.reset()
		h.drainSteering(context.Background(), c, testOpID)
		facts = col.invalidations()
		if len(facts) != 2 {
			t.Fatalf("steering delivery facts = %d, want the pop and the durable input commit", len(facts))
		}
		wantPairAt(t, h, sessionID, base.DurableRevision+1, base.LocalRevision+4) // pop then durable steering input
	})

	t.Run("drain pop and admission", func(t *testing.T) {
		store := freshSessionStore(t)
		stub := newPrepareStub(parkedPrepared())
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer func() {
			cancel()
			_ = h.Wait(context.Background())
		}()
		if _, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello")); disposition != DispositionAdmitted {
			t.Fatalf("disposition %q, want admitted", disposition)
		}
		c := cachedCoordinator(t, h, testSessionID)
		if _, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{terminal: OperationSuccess}); err != nil {
			t.Fatalf("terminal settlement: %v", err)
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

		h.drainBuffers(c, run)
		facts := col.invalidations()
		if len(facts) != 5 {
			t.Fatalf("drain facts = %d, want reservation, pop, admission, installation and release", len(facts))
		}
		wantPairAt(t, h, testSessionID, base.DurableRevision+1, base.LocalRevision+4) // one durable admission, four local publications
	})

	t.Run("harness-loss discard", func(t *testing.T) {
		hctx, cancel := context.WithCancel(context.Background())
		h, err := New(hctx, Dependencies{
			Storage: freshSessionStore(t),
			Prepare: func(context.Context, PreparationRequest) (PreparedExecution, error) {
				return modelPrepared(nil), nil
			},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello")); disposition != DispositionAdmitted {
			t.Fatalf("disposition %q, want admitted", disposition)
		}
		c := cachedCoordinator(t, h, testSessionID)
		if _, err := h.Submit(context.Background(), SubmitRequest{
			SessionID: testSessionID, OperationID: "op-steer", Origin: InputOriginUser,
			Content: admissionContent("steer"), Mode: MessageModeRegular,
		}); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		col := observeHarness(h)
		base := snapshotPair(t, h, testSessionID)
		cancel()

		h.drainSteering(context.Background(), c, testOpID)
		facts := col.invalidations()
		if len(facts) != 1 {
			t.Fatalf("discard facts = %d, want exactly the buffer discard", len(facts))
		}
		wantPairAt(t, h, testSessionID, base.DurableRevision, base.LocalRevision+1)

		h.drainSteering(context.Background(), c, testOpID) // an empty discard emits nothing
		if got := len(col.invalidations()); got != 1 {
			t.Fatalf("empty discard advanced the facts to %d", got)
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
		if err := h.StartJob(context.Background(), testSessionID, jobFixtureID, func(_ context.Context, id string) error {
			completionID = id
			return nil
		}); err != nil {
			t.Fatalf("StartJob: %v", err)
		}
		facts := col.invalidations()
		if len(facts) != 1 || facts[0].SessionID != testSessionID || facts[0].JobID != jobFixtureID {
			t.Fatalf("job admission facts = %+v, want one job-attributed invalidation", facts)
		}

		col.reset()
		if err := h.DeliverBackgroundCompletion(context.Background(), testSessionID, completionID, "job done"); err != nil {
			t.Fatalf("DeliverBackgroundCompletion: %v", err)
		}
		facts = col.invalidations()
		last := facts[len(facts)-1]
		if last.SessionID != testSessionID || last.JobID != jobFixtureID {
			t.Fatalf("job finish fact = %+v, want the job-attributed finish", last)
		}
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
		if err != nil {
			t.Fatalf("LaunchChildSession: %v", err)
		}
		child := res.ChildSessionID
		facts := col.invalidations()
		var sawParentAdmit, sawChild bool
		childFacts := 0
		for _, fact := range facts {
			if fact.JobID != "" {
				t.Fatalf("child flow fact carries a Job identity: %+v", fact)
			}
			if fact.SessionID == testSessionID {
				sawParentAdmit = true
			}
			if fact.SessionID == child {
				sawChild = true
				childFacts++
			}
		}
		if !sawParentAdmit || !sawChild || childFacts < 2 {
			t.Fatalf("child flow facts = %+v (parent %v child %v childFacts %d)", facts, sawParentAdmit, sawChild, childFacts)
		}
		// The child's own publications: the durable creation and its run
		// installation. The pair is read after the producer released its lock.
		if childPair := snapshotPair(t, h, child); childPair != (SessionRevision{DurableRevision: 2, LocalRevision: 1}) {
			t.Fatalf("child pair = %+v, want the durable creation and one local installation", childPair)
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
		if member == nil {
			t.Fatalf("the launched child member is absent")
		}
		col.reset()
		h.claimFinishBackgroundMember(rootC, member.completionID)
		waitMemberDone(t, member)
		facts = col.invalidations()
		if len(facts) != 1 || facts[0].SessionID != testSessionID || facts[0].JobID != "" {
			t.Fatalf("child member finish facts = %+v, want one unattributed invalidation", facts)
		}
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
		if err != nil {
			t.Fatalf("LaunchChildSession: %v", err)
		}
		completions := map[string]string{}
		stopper := &gatedJobStopper{deliver: func(sessionID, jobID string) {
			h.DeliverBackgroundCompletion(context.Background(), sessionID, completions[jobID], "job done")
		}}
		h.deps.Jobs = stopper
		if err := h.StartJob(context.Background(), root, jobFixtureID, func(_ context.Context, completionID string) error {
			completions[jobFixtureID] = completionID
			return nil
		}); err != nil {
			t.Fatalf("StartJob: %v", err)
		}
		col.reset()

		if err := h.Stop(context.Background(), root); err != nil {
			t.Fatalf("Stop: %v", err)
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
		if !childClosed || !jobFinished || !rootReopened {
			t.Fatalf("stop facts = %+v (child %v job %v reopen %v)", facts, childClosed, jobFinished, rootReopened)
		}
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
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		facts := col.invalidations()
		if len(facts) != 1 {
			t.Fatalf("create facts = %d, want one", len(facts))
		}
		wantPairAt(t, h, created.Identity.SessionID, 1, 0)
	})

	t.Run("agent type and lifecycle", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), nil)
		col := observeHarness(h)
		if _, err := h.ChangeAgentType(context.Background(), testSessionID, "planner"); err != nil {
			t.Fatalf("ChangeAgentType: %v", err)
		}
		if _, err := h.ArchiveSession(context.Background(), testSessionID); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if _, err := h.ReopenSession(context.Background(), testSessionID); err != nil {
			t.Fatalf("ReopenSession: %v", err)
		}
		if _, err := h.ArchiveSession(context.Background(), testSessionID); err != nil {
			t.Fatalf("second ArchiveSession: %v", err)
		}
		facts := col.invalidations()
		if len(facts) != 4 {
			t.Fatalf("lifecycle facts = %d, want four durable advances", len(facts))
		}
		wantPairAt(t, h, testSessionID, 5, 0)
		if _, err := h.ArchiveSession(context.Background(), testSessionID); err != nil {
			t.Fatalf("no-write archive: %v", err)
		}
		if got := len(col.invalidations()); got != 4 {
			t.Fatalf("no-write archive advanced the facts to %d", got)
		}
	})

	t.Run("admission", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(validPrepared()).prepare)
		col := observeHarness(h)
		if _, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello")); disposition != DispositionAdmitted {
			t.Fatalf("disposition %q, want admitted", disposition)
		}
		facts := col.invalidations()
		if len(facts) != 3 {
			t.Fatalf("admission facts = %d, want reservation, admission and release", len(facts))
		}
		wantPairAt(t, h, testSessionID, 2, 2)
	})

	t.Run("model result, compaction and steering", func(t *testing.T) {
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}
		h, _, c, sessionID := newEffectHarness(t, modelFn)
		col := observeHarness(h)
		base := snapshotPair(t, h, sessionID)
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err != nil {
			t.Fatalf("model effect: %v", err)
		}
		before := snapshotPair(t, h, sessionID)
		if got := col.invalidations(); len(got) == 0 {
			t.Fatalf("model result emitted no invalidation")
		}
		if before.DurableRevision <= base.DurableRevision {
			t.Fatalf("model result pair = %+v, want a durable advance over %+v", before, base)
		}

		col.reset()
		compactionBase := snapshotPair(t, h, sessionID)
		if err := h.commitCompaction(c, testOpID, testCapture(), "summary", 1, nil, false); err != nil {
			t.Fatalf("commitCompaction: %v", err)
		}
		facts := col.invalidations()
		if len(facts) != 1 {
			t.Fatalf("compaction facts = %d, want one", len(facts))
		}
		after := snapshotPair(t, h, sessionID)
		if after.DurableRevision <= compactionBase.DurableRevision {
			t.Fatalf("compaction pair = %+v, want a durable advance over %+v", after, compactionBase)
		}

		col.reset()
		steeringBase := snapshotPair(t, h, sessionID)
		if err := h.commitSteeringInput(context.Background(), c, testOpID, InputOriginUser, admissionContent("steer")); err != nil {
			t.Fatalf("commitSteeringInput: %v", err)
		}
		facts = col.invalidations()
		if len(facts) != 1 {
			t.Fatalf("steering facts = %d, want one", len(facts))
		}
		after = snapshotPair(t, h, sessionID)
		if after.DurableRevision <= steeringBase.DurableRevision {
			t.Fatalf("steering pair = %+v, want a durable advance over %+v", after, steeringBase)
		}
	})

	t.Run("fork destination", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript(turn())
		script.gate = make(chan struct{})
		stub := newPrepareStub(modelPrepared(script.model))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		defer script.releaseGate() // a parked destination execution always unblocks
		if _, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello")); disposition != DispositionAdmitted {
			t.Fatalf("disposition %q, want admitted", disposition)
		}
		c := cachedCoordinator(t, h, testSessionID)
		if _, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{terminal: OperationSuccess}); err != nil {
			t.Fatalf("terminal settlement: %v", err)
		}
		snap, err := h.SnapshotSession(context.Background(), testSessionID)
		if err != nil || len(snap.Facts) == 0 || snap.Facts[0].Kind != EntryInput {
			t.Fatalf("source facts = %+v err %v", snap.Facts, err)
		}
		col := observeHarness(h)
		res, err := h.Fork(context.Background(), ForkRequest{
			SourceSessionID: testSessionID,
			BoundaryEntryID: snap.Facts[0].EntryID,
			OperationID:     "fork-op-1",
			Content:         admissionContent("forked"),
		})
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		dest := res.Session.Identity.SessionID
		destFacts := 0
		for _, fact := range col.invalidations() {
			if fact.SessionID == dest {
				destFacts++
			}
		}
		if destFacts < 2 {
			t.Fatalf("fork destination facts = %d, want the durable creation and the local installation", destFacts)
		}
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
			if op.Admission.OperationID == "fork-op-1" {
				settled = op.State.Status == OperationSuccess && op.State.Terminal != nil
			}
		}
		if !settled {
			t.Fatalf("fork destination operations = %+v, want the fork Operation settled success", final.Operations)
		}
		if final.Session.Revision <= 2 || final.LocalRevision <= 1 {
			t.Fatalf("completed destination pair = {%d %d}, want the settlement's durable advance and the drain's local publications over the held {2 1}",
				final.Session.Revision, final.LocalRevision)
		}
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
	if _, err := h.Submit(context.Background(), SubmitRequest{
		SessionID: testSessionID, OperationID: "op-1", Origin: InputOriginUser,
		Content: admissionContent("hello"), Mode: MessageModeRegular,
	}); err == nil {
		t.Fatalf("the failing admission succeeded")
	}
	store.txHook = nil
	facts := col.invalidations()
	if len(facts) != 2 {
		t.Fatalf("rollback facts = %d, want exactly the reservation and release", len(facts))
	}
	// The two facts are the reservation and its release: their publication
	// pairs are only readable after the producer released its lock, and the
	// final pair proves no durable hint landed.
	if got := snapshotPair(t, h, testSessionID); got.DurableRevision != base.DurableRevision {
		t.Fatalf("the rolled-back admission advanced the durable revision to %d", got.DurableRevision)
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
		if _, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello")); disposition != DispositionAdmitted {
			t.Fatalf("disposition %q, want admitted", disposition)
		}
		c := cachedCoordinator(t, h, testSessionID)
		col := observeHarness(h)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(script.model, nil), testCapture()), nil); err != nil {
				t.Errorf("model effect: %v", err)
			}
		}()
		<-script.arrived // the intent committed and the physical attempt started
		if got := col.snapshot(); len(got) != 0 {
			t.Fatalf("the model intent emitted %+v", got)
		}
		script.releaseGate()
		<-done
	})

	t.Run("tool intent", func(t *testing.T) {
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		h, _, c, _ := newEffectHarness(t, modelFn)
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err != nil {
			t.Fatalf("model effect: %v", err)
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
			if _, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-1")); err != nil {
				t.Errorf("tool effect: %v", err)
			}
		}()
		<-executeStarted // the tool intent committed and the executor started
		facts := col.snapshot()
		if len(facts) != 1 || facts[0].Kind != FactToolStarted {
			t.Fatalf("tool intent facts = %+v, want only the started fact", facts)
		}
		close(release)
		<-done
	})
}

// TestObserveNoWriteAndDelete proves the no-write paths and deletion emit
// nothing, and deletion leaves no tombstone.
func TestObserveNoWriteAndDelete(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), newPrepareStub(validPrepared()).prepare)
	col := observeHarness(h)
	if err := h.Interrupt(context.Background(), testSessionID); err != nil { // idle interrupt
		t.Fatalf("idle Interrupt: %v", err)
	}
	if _, err := h.ReopenSession(context.Background(), testSessionID); err != nil { // reopen open
		t.Fatalf("no-write ReopenSession: %v", err)
	}
	if err := h.Stop(context.Background(), testSessionID); err != nil { // empty root stop
		t.Fatalf("empty Stop: %v", err)
	}
	if got := col.snapshot(); len(got) != 0 {
		t.Fatalf("no-write paths emitted %+v", got)
	}

	if _, err := h.ArchiveSession(context.Background(), testSessionID); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}
	col.reset()
	if err := h.DeleteSession(context.Background(), testSessionID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if got := col.snapshot(); len(got) != 0 {
		t.Fatalf("deletion emitted %+v", got)
	}
}

// TestObserveSuppression proves a delayed producer's emission after deletion
// or corruption is suppressed.
func TestObserveSuppression(t *testing.T) {
	t.Run("deleted", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(validPrepared()).prepare)
		if _, err := h.ArchiveSession(context.Background(), testSessionID); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		c := cachedCoordinator(t, h, testSessionID)
		col := observeHarness(h)
		if err := h.DeleteSession(context.Background(), testSessionID); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		h.observeInvalidation(c)
		if got := col.snapshot(); len(got) != 0 {
			t.Fatalf("the deleted coordinator emitted %+v", got)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(parkedPrepared()).prepare)
		if _, err := h.ReadSessionHeader(context.Background(), testSessionID); err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		c := cachedCoordinator(t, h, testSessionID)
		col := observeHarness(h)
		h.markCorrupt(testSessionID, corruptSession(testSessionID, "injected corruption"))
		h.observeInvalidation(c)
		if got := col.snapshot(); len(got) != 0 {
			t.Fatalf("the corrupt coordinator emitted %+v", got)
		}
	})
}

// TestObserveCurrentPair proves the cached-only publication read samples the
// pair current after later publications, rather than a pair captured at a
// producer's lock time.
func TestObserveCurrentPair(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), newPrepareStub(parkedPrepared()).prepare)
	if _, err := h.ReadSessionHeader(context.Background(), testSessionID); err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	c := cachedCoordinator(t, h, testSessionID)
	col := observeHarness(h)

	// A producer's publication whose emission is delayed.
	c.mu.Lock()
	c.bumpLocalRevision()
	c.mu.Unlock()
	// A later publication lands before the delayed emission.
	release, err := h.reserve(context.Background(), c)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	release()

	col.reset()
	h.observeInvalidation(c) // the delayed producer's emission
	facts := col.invalidations()
	if len(facts) != 1 {
		t.Fatalf("delayed emission facts = %d, want one", len(facts))
	}
	// The cached-only publication read used by passive publishers samples
	// the pair current at publication time, after the later publications
	// above landed.
	current := snapshotPair(t, h, testSessionID)
	identity, pair, ok := h.ReadObservation(testSessionID)
	if !ok || identity.SessionID != testSessionID || pair != current {
		t.Fatalf("ReadObservation = %+v %+v %v, want the current pair %+v", identity, pair, ok, current)
	}
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
		if _, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{}); err != nil {
			t.Errorf("first commit: %v", err)
		}
	}()
	<-firstArrived // the first commit emitted and its callback is parked

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		if err := h.commitSteeringInput(context.Background(), c, testOpID, InputOriginUser, admissionContent("steer")); err != nil {
			t.Errorf("second commit: %v", err)
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
	if len(got) != 2 {
		t.Fatalf("concurrent commit facts = %+v, want two", got)
	}
	for i, fact := range got {
		if fact.Kind != FactInvalidation || fact.SessionID != sessionID {
			t.Fatalf("fact %d lost its owner attribution: %+v", i, fact)
		}
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
	if _, err := p.h.SnapshotSession(context.Background(), fact.SessionID); err != nil {
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

	if _, err := h.Submit(context.Background(), SubmitRequest{
		SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser,
		Content: admissionContent("hello"), Mode: MessageModeRegular,
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	awaitHarnessQuiet(t, h, testSessionID)

	probe.mu.Lock()
	violations, readErrs := probe.violations, append([]error(nil), probe.readErrs...)
	probe.mu.Unlock()
	if violations != 0 {
		t.Fatalf("the observer ran inside a storage transaction %d times", violations)
	}
	if len(readErrs) != 0 {
		t.Fatalf("the observer's reentrant reads failed: %v", readErrs)
	}
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
		if _, err := wrapper.Recv(); err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if len(*got) != 1 || (*got)[0] != (emitted{kind: FactTextDelta, position: 0, content: "a"}) {
			t.Fatalf("emitted = %+v, want only the nonempty text fragment", *got)
		}
	})

	t.Run("refusal only", func(t *testing.T) {
		inner := streamOf(model.StreamDelta{HasChoice: true, Role: "assistant", RefusalFragment: "I cannot"})
		wrapper, got := collect(t, inner)
		if _, err := wrapper.Recv(); err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if len(*got) != 1 || (*got)[0] != (emitted{kind: FactRefusalDelta, position: 0, content: "I cannot"}) {
			t.Fatalf("emitted = %+v, want only the nonempty refusal fragment without a position", *got)
		}
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
		if _, err := wrapper.Recv(); err != nil {
			t.Fatalf("Recv: %v", err)
		}
		want := []emitted{
			{kind: FactTextDelta, position: 0, content: "working"},
			{kind: FactTextDelta, position: 2, content: "harder"},
			{kind: FactRefusalDelta, position: 0, content: "but no"},
		}
		if !slices.Equal(*got, want) {
			t.Fatalf("emitted = %+v, want the content-text emissions before the refusal %+v", *got, want)
		}
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
		if _, err := wrapper.Recv(); err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if len(*got) != 1 || (*got)[0].kind != FactTextDelta {
			t.Fatalf("emitted = %+v, want the text fragment and no refusal emission", *got)
		}
	})

	t.Run("original delta and error preserved", func(t *testing.T) {
		delta := model.StreamDelta{HasChoice: true, ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "x"}}}
		inner := &duplexStream{delta: delta, err: errors.New("read failed")}
		wrapper, got := collect(t, inner)
		returned, err := wrapper.Recv()
		if err == nil || err.Error() != "read failed" {
			t.Fatalf("error = %v, want the original read failure", err)
		}
		if len(returned.ContentFragments) != 1 || &returned.ContentFragments[0] != &delta.ContentFragments[0] {
			t.Fatalf("the original delta was not preserved: %+v", returned)
		}
		if len(*got) != 0 {
			t.Fatalf("a delta returned with an error emitted %+v", *got)
		}
	})

	t.Run("refusal with an error emits none", func(t *testing.T) {
		inner := &duplexStream{delta: model.StreamDelta{HasChoice: true, RefusalFragment: "no"}, err: errors.New("read failed")}
		wrapper, got := collect(t, inner)
		if _, err := wrapper.Recv(); err == nil {
			t.Fatal("Recv: want the original read failure")
		}
		if len(*got) != 0 {
			t.Fatalf("a refusal delta returned with an error emitted %+v", *got)
		}
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
			if _, err := wrapper.Recv(); err != nil {
				t.Fatalf("case %d Recv: %v", i, err)
			}
			if len(*got) != 0 {
				t.Fatalf("case %d emitted %+v", i, *got)
			}
		}
	})

	t.Run("close delegates once", func(t *testing.T) {
		inner := streamOf()
		wrapper, _ := collect(t, inner)
		if err := wrapper.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if inner.closes != 1 {
			t.Fatalf("close count = %d, want one delegation", inner.closes)
		}
	})

	t.Run("assembly close stays exactly once through the wrapper", func(t *testing.T) {
		stream := completedTurnStream()
		modelFn := func(context.Context, model.Request) (model.Stream, error) { return stream, nil }
		h, _, c, _ := newEffectHarness(t, modelFn)
		observeHarness(h)
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err != nil {
			t.Fatalf("model effect: %v", err)
		}
		if stream.closes != 1 {
			t.Fatalf("accepted stream close count = %d, want one", stream.closes)
		}
	})

	t.Run("eof emits none", func(t *testing.T) {
		inner := streamOf()
		wrapper, got := collect(t, inner)
		if _, err := wrapper.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("EOF = %v", err)
		}
		if len(*got) != 0 {
			t.Fatalf("EOF emitted %+v", *got)
		}
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
	exec := effectExecution(nil, nil)
	exec.CompactModel = compactFn
	effect := h.compactModelEffect(c, testOpID, exec, testCapture(), &usageAccumulator{})
	if _, err := invokeModelEffect(t, effect, nil); err != nil {
		t.Fatalf("compact model effect: %v", err)
	}
	deltas := 0
	for _, fact := range col.snapshot() {
		if fact.Kind != FactTextDelta {
			continue
		}
		deltas++
		if fact.SessionID != sessionID || fact.OperationID != testOpID || fact.Position != 0 || fact.Content != "done" {
			t.Fatalf("compact delta fact = %+v, want the enclosing Operation attribution", fact)
		}
	}
	if deltas != 1 {
		t.Fatalf("compact text deltas = %d, want one", deltas)
	}
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
	if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err != nil {
		t.Fatalf("model effect: %v", err)
	}
	var refusals []HarnessFact
	for _, fact := range col.snapshot() {
		if fact.Kind != FactRefusalDelta {
			continue
		}
		refusals = append(refusals, fact)
	}
	want := []string{"I cannot", " help with that."}
	if len(refusals) != len(want) {
		t.Fatalf("refusal facts = %+v, want one per fragment %v", refusals, want)
	}
	for i, fact := range refusals {
		if fact.SessionID != sessionID || fact.OperationID != testOpID || fact.Content != want[i] ||
			fact.Position != 0 || fact.JobID != "" || fact.CallID != "" || fact.Ordinal != 0 ||
			fact.Name != "" || fact.Status != "" {
			t.Fatalf("refusal fact %d = %+v, want the enclosed Operation attribution and no position", i, fact)
		}
	}
	// Final refusal retention: the committed assistant entry keeps the
	// fragments joined in arrival order.
	snap, err := h.SnapshotSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("SnapshotSession: %v", err)
	}
	retained := false
	for _, fact := range snap.Facts {
		if fact.Assistant == nil {
			continue
		}
		if fact.Assistant.Refusal != "I cannot help with that." {
			t.Fatalf("committed assistant refusal = %q, want the joined fragments", fact.Assistant.Refusal)
		}
		retained = true
	}
	if !retained {
		t.Fatalf("no committed assistant fact in %+v", snap.Facts)
	}
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
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err != nil {
			t.Fatalf("model effect: %v", err)
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
				got, err := h.toolEffect(c, testOpID, effectExecution(nil, tc.plan), testCapture())(ctx, testToolCall("call-1"))
				if err != nil {
					t.Fatalf("tool effect: %v", err)
				}
				if got.Status != tc.status {
					t.Fatalf("committed status = %s, want %s", got.Status, tc.status)
				}
				facts := col.snapshot()
				var startedAt, finishedAt = -1, -1
				for i, fact := range facts {
					switch fact.Kind {
					case FactToolStarted:
						if startedAt >= 0 {
							t.Fatalf("the call started more than once: %+v", facts)
						}
						startedAt = i
						if fact.CallID != "call-1" || fact.Ordinal != 0 || fact.Name != "echo" || fact.OperationID != testOpID {
							t.Fatalf("started fact = %+v", fact)
						}
					case FactToolFinished:
						if finishedAt >= 0 {
							t.Fatalf("the call finished more than once: %+v", facts)
						}
						finishedAt = i
						if fact.CallID != "call-1" || fact.Status != tc.status || fact.OperationID != testOpID {
							t.Fatalf("finished fact = %+v, want status %s", fact, tc.status)
						}
					case FactInvalidation:
					default:
						t.Fatalf("unexpected progress kind %q in %+v", fact.Kind, facts)
					}
				}
				if startedAt < 0 || finishedAt < 0 || startedAt > finishedAt {
					t.Fatalf("lifecycle order = started %d finished %d in %+v", startedAt, finishedAt, facts)
				}
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
		got, err := h.toolEffect(c, testOpID, effectExecution(nil, nil), testCapture())(context.Background(), testToolCall("call-1"))
		if err != nil || got.Status != model.ResultError {
			t.Fatalf("unavailable tool = %+v err %v, want the error result", got, err)
		}
		facts := col.snapshot()
		if len(facts) != 3 || facts[0].Kind != FactToolStarted || facts[1].Kind != FactInvalidation || facts[2].Kind != FactToolFinished {
			t.Fatalf("advertisement-gate facts = %+v, want started, the result commit, then finished", facts)
		}
	})

	t.Run("synthetic unstarted result finishes without started", func(t *testing.T) {
		h, _, c, _ := newEffectHarness(t, nil)
		publishStream(t, h, c, completedTurnStream(testToolCall("call-1"), testToolCall("call-2")))
		successPlan := func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
		}
		if _, err := h.toolEffect(c, testOpID, effectExecution(nil, successPlan), testCapture())(context.Background(), testToolCall("call-1")); err != nil {
			t.Fatalf("call-1 dispatch: %v", err)
		}
		col := observeHarness(h)
		if _, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{terminal: OperationInterruption, detail: "stopped"}); err != nil {
			t.Fatalf("terminal settlement: %v", err)
		}
		facts := col.snapshot()
		var finished int
		for _, fact := range facts {
			switch fact.Kind {
			case FactToolStarted:
				t.Fatalf("a synthetic unstarted result emitted a started fact: %+v", fact)
			case FactToolFinished:
				finished++
				if fact.CallID != "call-2" || fact.Status != model.ResultInterrupted {
					t.Fatalf("synthetic finished fact = %+v", fact)
				}
			}
		}
		if finished != 1 {
			t.Fatalf("synthetic finished facts = %d, want one; facts %+v", finished, facts)
		}
	})

	t.Run("hooks have no progress kind", func(t *testing.T) {
		h, _, c, _ := newEffectHarness(t, nil)
		publishCall(t, h, c, testToolCall("call-1"))
		exec := effectExecution(nil, func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
		})
		exec.ToolHooks = []ToolArgumentsHook{{ID: "h1", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			return nil, errors.New("hook boom")
		}}}
		col := observeHarness(h)
		got, err := h.toolEffect(c, testOpID, exec, testCapture())(context.Background(), testToolCall("call-1"))
		if err != nil || got.Status != model.ResultError {
			t.Fatalf("hook-settled tool = %+v err %v, want the error result", got, err)
		}
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
		if started != 1 || finished != 1 || invalidations != 2 { // the hook result and the tool result
			t.Fatalf("hook flow counts = started %d finished %d invalidations %d", started, finished, invalidations)
		}
	})
}

// TestObserveRematerializationNoHint proves a foreign-revision rematerialization
// emits no replay hint.
func TestObserveRematerializationNoHint(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, newPrepareStub(parkedPrepared()).prepare)
	if _, err := h.ReadSessionHeader(context.Background(), testSessionID); err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	col := observeHarness(h)
	advanceSessionRevision(t, store, testSessionID) // the foreign writer
	if _, err := h.ChangeAgentType(context.Background(), testSessionID, "planner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign-revision change = %v, want the conflict class", err)
	}
	if got := col.snapshot(); len(got) != 0 {
		t.Fatalf("rematerialization emitted %+v", got)
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
	if _, err := h.Submit(context.Background(), SubmitRequest{
		SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser,
		Content: admissionContent("hello"), Mode: MessageModeRegular,
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	awaitHarnessQuiet(t, h, testSessionID)
	if len(col.snapshot()) == 0 {
		t.Fatalf("the first owner emitted no facts")
	}
	cancel()
	if err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := Recover(context.Background(), store); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	sink := &factCollector{}
	restarted, err := New(context.Background(), Dependencies{Storage: store, Prepare: stub.prepare, Observe: sink.observe})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := restarted.ReadSessionHeader(context.Background(), testSessionID); err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if _, err := restarted.SnapshotSession(context.Background(), testSessionID); err != nil {
		t.Fatalf("SnapshotSession: %v", err)
	}
	if _, err := restarted.ListSessions(context.Background()); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("the new owner's reads replayed %+v", got)
	}
	if _, err := restarted.ChangeAgentType(context.Background(), testSessionID, "planner"); err != nil {
		t.Fatalf("ChangeAgentType: %v", err)
	}
	if got := sink.invalidations(); len(got) != 1 {
		t.Fatalf("the new owner's first publication emitted %+v", got)
	}
}

// observationCountingStorage fails every read and counts every access: the
// cached-only observation read must perform no storage work at all.
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

// TestReadObservation pins the cached-only observation read: a warm valid
// coordinator returns the identity (with its Workspace) and the current pair;
// a cache miss, a deleted, and a corrupt Session report false without any
// storage access; a cold Session is never materialized by the read.
func TestReadObservation(t *testing.T) {
	t.Run("valid warm coordinator", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(validPrepared()).prepare)
		if _, err := h.ReadSessionHeader(context.Background(), testSessionID); err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		identity, revision, ok := h.ReadObservation(testSessionID)
		if !ok {
			t.Fatal("ReadObservation on a warm coordinator reported unavailable")
		}
		if identity.SessionID != testSessionID {
			t.Fatalf("identity session = %q, want %q", identity.SessionID, testSessionID)
		}
		if identity.Workspace == "" {
			t.Fatal("identity carries no workspace")
		}
		if want := snapshotPair(t, h, testSessionID); revision != want {
			t.Fatalf("pair = %+v, want the current pair %+v", revision, want)
		}
	})

	t.Run("cache miss performs no storage work", func(t *testing.T) {
		store := &observationCountingStorage{Storage: freshSessionStore(t)}
		h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
		if _, _, ok := h.ReadObservation(testSessionID); ok {
			t.Fatal("ReadObservation on a never-materialized Session reported available")
		}
		if store.calls != 0 {
			t.Fatalf("storage calls on a cache miss = %d, want zero", store.calls)
		}
		// The read left the registry cold: a repeat miss performs no work and
		// the Session stays unmaterialized.
		if _, _, ok := h.ReadObservation(testSessionID); ok {
			t.Fatal("repeat ReadObservation materialized the Session")
		}
		if store.calls != 0 {
			t.Fatalf("storage calls after repeat = %d, want zero", store.calls)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(validPrepared()).prepare)
		if _, err := h.ArchiveSession(context.Background(), testSessionID); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if err := h.DeleteSession(context.Background(), testSessionID); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		if _, _, ok := h.ReadObservation(testSessionID); ok {
			t.Fatal("ReadObservation after deletion reported available")
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(parkedPrepared()).prepare)
		if _, err := h.ReadSessionHeader(context.Background(), testSessionID); err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		h.markCorrupt(testSessionID, corruptSession(testSessionID, "injected corruption"))
		if _, _, ok := h.ReadObservation(testSessionID); ok {
			t.Fatal("ReadObservation on a corrupt Session reported available")
		}
	})
}
