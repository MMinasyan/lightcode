package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/model"
)

// quickModel is a physical model attempt that completes one turn immediately.
func quickModel() func(context.Context, model.Request) (model.Stream, error) {
	return func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(), nil
	}
}

// parkedModel is a physical model attempt that parks until its release channel
// closes or its execution context is canceled, then answers accordingly: a
// stop's cancel settles the parked Operation as interruption, a released
// model completes the turn normally.
func parkedModel(release chan struct{}) func(context.Context, model.Request) (model.Stream, error) {
	return func(ctx context.Context, _ model.Request) (model.Stream, error) {
		select {
		case <-release:
			return completedTurnStream(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// gatedJobStopper is the fake Jobs seam: StopJob records every call, parks
// while its gate is open, and then runs the delivery callback that stands for
// the ordinary kill-completion path. Callers inspect stopped only after the
// stop that issued it has returned.
type gatedJobStopper struct {
	gate    chan struct{}
	stopped []string
	deliver func(sessionID, jobID string)
}

func (s *gatedJobStopper) StopJob(sessionID, jobID string) {
	s.stopped = append(s.stopped, sessionID+"/"+jobID)
	if s.gate != nil {
		<-s.gate
	}
	if s.deliver != nil {
		s.deliver(sessionID, jobID)
	}
}

// launchGate wraps a store to hold a child launch at one precise post-commit
// window. Read mode parks the launch's second materialization read — the
// child's, which runs after the handoff check and before installation. Tx
// mode parks the launch caller on the first transaction's return — after the
// commit, before the handoff check. Every parked call signals its arrival.
type launchGate struct {
	Storage
	mode        string // "read" or "tx"
	mu          sync.Mutex
	reads       int
	readArrived chan struct{}
	readRelease chan struct{}
	txSeen      bool
	txArrived   chan struct{}
	txRelease   chan struct{}
}

func newLaunchGate(store *graphStorage, mode string) *launchGate {
	return &launchGate{
		Storage:     store,
		mode:        mode,
		readArrived: make(chan struct{}, 1),
		readRelease: make(chan struct{}),
		txArrived:   make(chan struct{}, 1),
		txRelease:   make(chan struct{}),
	}
}

func (g *launchGate) ReadRegisters(ctx context.Context, sessionID string) ([]Register, error) {
	g.mu.Lock()
	g.reads++
	park := g.mode == "read" && g.reads == 2
	g.mu.Unlock()
	if park {
		g.readArrived <- struct{}{}
		<-g.readRelease
	}
	return g.Storage.ReadRegisters(ctx, sessionID)
}

func (g *launchGate) Transact(ctx context.Context, fn func(Transaction) error) error {
	err := g.Storage.Transact(ctx, fn)
	g.mu.Lock()
	park := g.mode == "tx" && !g.txSeen
	g.txSeen = true
	g.mu.Unlock()
	if park {
		g.txArrived <- struct{}{}
		<-g.txRelease
	}
	return err
}

// awaitStopInterval proves a Stop call is held inside its stop interval: the
// interval is installed under the coordinator lock after the snapshot and is
// cleared only at the return, so finding it open with the call's result still
// absent observes the blocking state itself — never a timing window.
func awaitStopInterval(t *testing.T, c *coordinator, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("Stop returned before convergence: %v", err)
		default:
		}
		c.mu.Lock()
		waiting := c.stop != nil
		c.mu.Unlock()
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Stop never entered its stop interval")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// stopResult joins one Stop call with a generous timeout.
func stopResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("Stop did not return")
		return nil
	}
}

// runtimeInputText reads the first runtime-origin input entry's text straight
// from durable storage. Graph validation is deliberately skipped: its
// registers-then-entries reads can tear against a concurrent settlement
// commit and report a committed Operation as corrupt.
func runtimeInputTextRaw(t *testing.T, store *graphStorage, sessionID string) string {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, env := range store.entries[sessionID] {
		if env.Kind != EntryInput {
			continue
		}
		ie, err := decodeInputEntry(env)
		if err != nil {
			t.Fatalf("decode input entry %s: %v", env.ID, err)
		}
		if ie.Origin == InputOriginRuntime {
			return ie.Content[0].Text
		}
	}
	t.Fatalf("session %q carries no runtime-origin input entry", sessionID)
	return ""
}

// inputTexts reads every input entry's text in commit order straight from
// durable storage, without graph validation.
func inputTexts(t *testing.T, store *graphStorage, sessionID string) []string {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	var out []string
	for _, env := range store.entries[sessionID] {
		if env.Kind != EntryInput {
			continue
		}
		ie, err := decodeInputEntry(env)
		if err != nil {
			t.Fatalf("decode input entry %s: %v", env.ID, err)
		}
		out = append(out, ie.Content[0].Text)
	}
	return out
}

// waitQuiet polls one coordinator until its retired execution slot is
// released and no Operation is current: deliveries resolving after this point
// deterministically take the idle-admission mode.
func waitQuiet(t *testing.T, c *coordinator) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		quiet := c.run == nil && c.graph.Session.State.CurrentOperationID == ""
		c.mu.Unlock()
		if quiet {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session never went quiet")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// launchRequestFor is launchRequest against an arbitrary parent.
func launchRequestFor(parentID string) LaunchChildRequest {
	req := launchRequest()
	req.ParentSessionID = parentID
	return req
}

// stopTree is the mixed ownership tree of the stop fixtures: one root with a
// launched child whose execution parks at its model boundary and one job
// whose kill-completion delivery goes through the fake stopper. Releasing the
// tree's channel completes every parked model attempt.
type stopTree struct {
	h           *Harness
	cancel      context.CancelFunc
	store       *graphStorage
	release     chan struct{}
	stopper     *gatedJobStopper
	root        string
	child       string
	childMember *backgroundMember
	jobMember   *backgroundMember
}

func newStopTree(t *testing.T) *stopTree {
	t.Helper()
	store := emptyStore(t)
	release := make(chan struct{})
	stub := newPrepareStub(modelPrepared(parkedModel(release)))
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
	root := createSession(t, h)
	res, err := h.LaunchChildSession(context.Background(), launchRequestFor(root))
	if err != nil {
		cancel()
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
		cancel()
		t.Fatalf("StartJob: %v", err)
	}
	var childMember, jobMember *backgroundMember
	rootC := cachedCoordinator(t, h, root)
	rootC.mu.Lock()
	for _, m := range rootC.group.members {
		if m.kind == memberChild && m.id == res.ChildSessionID {
			childMember = m
		}
		if m.kind == memberJob {
			jobMember = m
		}
	}
	rootC.mu.Unlock()
	if childMember == nil || jobMember == nil {
		cancel()
		t.Fatalf("tree members missing (child %v, job %v)", childMember, jobMember)
	}
	return &stopTree{
		h:           h,
		cancel:      cancel,
		store:       store,
		release:     release,
		stopper:     stopper,
		root:        root,
		child:       res.ChildSessionID,
		childMember: childMember,
		jobMember:   jobMember,
	}
}

// TestStopRootStopsMembersAndReopens proves the root-stop row: the child's
// Operation settles as interruption, the job is killed, both members finish,
// the group reopens, and the root's active Operation and both buffers
// survive — the queued message drains after the reopen.
func TestStopRootStopsMembersAndReopens(t *testing.T) {
	tree := newStopTree(t)
	defer tree.cancel()
	watch := watchSettlements(tree.store) // armed before the stop's child interruption commits
	if _, err := submitText(t, tree.h, tree.root, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if _, err := submitText(t, tree.h, tree.root, "op-2", MessageModeQueued, "queued-msg"); err != nil {
		t.Fatalf("queued submit: %v", err)
	}

	if err := tree.h.Stop(context.Background(), tree.root); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := tree.stopper.stopped; len(got) != 1 || got[0] != tree.root+"/"+jobFixtureID {
		t.Fatalf("StopJob calls = %v, want the tree's job killed", got)
	}
	waitMemberDone(t, tree.childMember)
	waitMemberDone(t, tree.jobMember)
	if rec := settledOperation(t, tree.store, tree.child, "child-op-1"); rec.State.Status != OperationInterruption ||
		rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
		t.Fatalf("child operation state = %+v, want terminal interruption with the contract detail", rec.State)
	}
	rootC := cachedCoordinator(t, tree.h, tree.root)
	rootC.mu.Lock()
	reopened := rootC.bgState == bgOpen
	groupEmpty := rootC.group == nil || len(rootC.group.members) == 0
	queued := len(rootC.queued)
	current := rootC.graph.Session.State.CurrentOperationID
	running := rootC.run != nil
	rootC.mu.Unlock()
	if !reopened || !groupEmpty {
		t.Fatalf("after Stop: reopened=%v groupEmpty=%v, want the group reopened and empty", reopened, groupEmpty)
	}
	if queued != 1 {
		t.Fatalf("root queued buffer after Stop = %d items, want the queued message intact", queued)
	}
	if current != "op-1" || !running {
		t.Fatalf("root operation after Stop = %q (run %v), want op-1 still running", current, running)
	}
	childC := cachedCoordinator(t, tree.h, tree.child)
	childC.mu.Lock()
	closed := childC.bgState == bgClosed
	childC.mu.Unlock()
	if !closed {
		t.Fatalf("stopped child bgState = %q, want permanently closed", childC.bgState)
	}

	close(tree.release) // the parked root Operation completes, absorbing both steered completions at its next boundary
	watch.next()        // the child's interruption, committed during the stop
	watch.next()        // op-1 settles with the steered completions committed as its inputs
	watch.next()        // the queued message admitted after the reopen
	if rec := settledOperation(t, tree.store, tree.root, "op-2"); rec.State.Status != OperationSuccess {
		t.Fatalf("queued operation status = %s, want the preserved queued message admitted and successful", rec.State.Status)
	}
	if rec := settledOperation(t, tree.store, tree.root, "op-1"); rec.State.Status != OperationSuccess {
		t.Fatalf("root operation status = %s, want the steered completions delivered into the running Operation", rec.State.Status)
	}
	// The member-stop order is unspecified (snapshot map iteration), so the
	// two completions may commit in either order; the set is what survives.
	texts := inputTexts(t, tree.store, tree.root)
	sort.Strings(texts)
	want := []string{"Task interrupted.", "hello", "job done", "queued-msg"}
	sort.Strings(want)
	if !reflect.DeepEqual(texts, want) {
		t.Fatalf("root entries = %q, want the surviving buffers delivered", texts)
	}
}

// TestStopChildClosesPermanently proves the leaf-closure row: a child stop
// discards its buffers, interrupts its run, delivers its completion, and
// stays closed — Submit is rejected afterwards.
func TestStopChildClosesPermanently(t *testing.T) {
	store := emptyStore(t)
	release := make(chan struct{})
	stub := newPrepareStub(modelPrepared(parkedModel(release)))
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
	defer cancel()
	defer close(release)
	root := createSession(t, h)
	res, err := h.LaunchChildSession(context.Background(), launchRequestFor(root))
	if err != nil {
		t.Fatalf("LaunchChildSession: %v", err)
	}
	child := res.ChildSessionID
	childC := cachedCoordinator(t, h, child)
	childC.mu.Lock()
	run := childC.run
	childC.mu.Unlock()
	if _, err := submitText(t, h, child, "op-2", MessageModeRegular, "steer-me"); err != nil {
		t.Fatalf("steering submit to the child: %v", err)
	}

	if err := h.Stop(context.Background(), child); err != nil {
		t.Fatalf("Stop(child): %v", err)
	}

	if rec := settledOperation(t, store, child, "child-op-1"); rec.State.Status != OperationInterruption {
		t.Fatalf("child operation status = %s, want the interrupted run", rec.State.Status)
	}
	select {
	case <-run.done:
	default:
		t.Fatalf("the child run did not converge")
	}
	childC.mu.Lock()
	closed := childC.bgState == bgClosed
	buffers := len(childC.steering) + len(childC.queued)
	childC.mu.Unlock()
	if !closed {
		t.Fatalf("child bgState after Stop = %q, want permanently closed", childC.bgState)
	}
	if buffers != 0 {
		t.Fatalf("closed child kept %d buffered items, want the buffers discarded", buffers)
	}
	if _, err := submitText(t, h, child, "op-3", MessageModeRegular, "nope"); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "session is closed to agent work") {
		t.Fatalf("Submit on the closed child = %v, want the closed-to-agent-work rejection", err)
	}
	if got := runtimeInputTextRaw(t, store, root); got != "Task interrupted." {
		t.Fatalf("parent runtime input = %q, want the child completion delivered to the idle parent", got)
	}
}

// TestSubmitRejectsClosedActiveChild proves Submit's own closed-group gate on
// the active branch: a closed child with a durably running Operation rejects
// before any buffering.
func TestSubmitRejectsClosedActiveChild(t *testing.T) {
	store := completionChildGraph(completionParentID, true, OperationRunning, "", "").storage(t)
	addRootSession(t, store, completionParentID)
	h := newTestHarness(t, store, nil)
	childC, err := h.coordinatorFor(context.Background(), completionChildID)
	if err != nil {
		t.Fatalf("coordinatorFor(child): %v", err)
	}
	closeBackgroundMode(childC)
	if _, err := submitText(t, h, completionChildID, "op-x", MessageModeRegular, "hi"); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "session is closed to agent work") {
		t.Fatalf("Submit on the closed active child = %v, want the closed-to-agent-work rejection", err)
	}
}

// TestStopGatedLaunchRunsClosedEntryPath proves the gated initial launch: a
// child Stop during the launch's materialization window captures its parent
// membership and cannot return; releasing the launch runs the closed-entry
// interruption path, completes the delivery to the parent, and releases Stop.
func TestStopGatedLaunchRunsClosedEntryPath(t *testing.T) {
	store := freshSessionStore(t)
	gate := newLaunchGate(store, "read")
	var (
		mu    sync.Mutex
		opens []string
	)
	exec := validExecution()
	exec.Model = quickModel()
	stub := newPrepareStub(PreparedExecution{
		Capture: testCapture(),
		Open: func(_ context.Context, admission OperationAdmission) (Execution, error) {
			mu.Lock()
			opens = append(opens, admission.SessionID)
			mu.Unlock()
			return exec, nil
		},
	})
	h, cancel := newCancelableHarness(t, gate, PreparedExecution{}, stub.prepare)
	defer cancel()

	launchDone := make(chan error, 1)
	var childID string
	go func() {
		res, err := h.LaunchChildSession(context.Background(), launchRequest())
		if err == nil {
			childID = res.ChildSessionID
		}
		launchDone <- err
	}()
	<-gate.readArrived // the launch committed and parks at its materialization read
	for id := range store.registers {
		if id != testSessionID {
			childID = id
		}
	}
	if childID == "" {
		t.Fatalf("the committed child register is missing")
	}

	childC, err := h.coordinatorFor(context.Background(), childID)
	if err != nil {
		t.Fatalf("coordinatorFor(child): %v", err)
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), childID) }()
	awaitStopInterval(t, childC, stopDone) // the parent membership holds the stop

	close(gate.readRelease)
	if err := stopResult(t, stopDone); err != nil {
		t.Fatalf("Stop(child) = %v, want nil", err)
	}
	if err := <-launchDone; err != nil {
		t.Fatalf("LaunchChildSession = %v, want the launch to succeed with the entry interruption", err)
	}
	mu.Lock()
	childOpened := false
	for _, id := range opens {
		if id == childID {
			childOpened = true
		}
	}
	mu.Unlock()
	if childOpened {
		t.Fatalf("the closed child's execution opened; want the entry interruption without an opener call")
	}
	if rec := settledOperation(t, store, childID, "child-op-1"); rec.State.Status != OperationInterruption ||
		rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
		t.Fatalf("child operation state = %+v, want terminal interruption with the contract detail", rec.State)
	}
	if got := runtimeInputTextRaw(t, store, testSessionID); got != "Task interrupted." {
		t.Fatalf("parent runtime input = %q, want the child completion delivered", got)
	}
	if len(storedSignals(t, store, testSessionID)) != 0 {
		t.Fatalf("parent signals = %+v, want the idle parent's admission delivery, not a signal", storedSignals(t, store, testSessionID))
	}
	childC.mu.Lock()
	closed := childC.bgState == bgClosed
	childC.mu.Unlock()
	if !closed {
		t.Fatalf("stopped child bgState = %q, want permanently closed", childC.bgState)
	}
}

// TestStopRacingStartSettlesWithoutStarting proves the racing-start row: a
// child stop that lands between the launch's commit and its installation
// lets the launch's handoff pass (the parent group is untouched) and settles
// the committed child's Operation at the closed entry — without starting it —
// before the delivery completes and releases the stop.
func TestStopRacingStartSettlesWithoutStarting(t *testing.T) {
	store := freshSessionStore(t)
	gate := newLaunchGate(store, "tx")
	var (
		mu    sync.Mutex
		opens []string
	)
	exec := validExecution()
	exec.Model = quickModel()
	stub := newPrepareStub(PreparedExecution{
		Capture: testCapture(),
		Open: func(_ context.Context, admission OperationAdmission) (Execution, error) {
			mu.Lock()
			opens = append(opens, admission.SessionID)
			mu.Unlock()
			return exec, nil
		},
	})
	h, cancel := newCancelableHarness(t, gate, PreparedExecution{}, stub.prepare)
	defer cancel()

	launchDone := make(chan error, 1)
	go func() {
		_, err := h.LaunchChildSession(context.Background(), launchRequest())
		launchDone <- err
	}()
	<-gate.txArrived // the launch committed and parks before its handoff check
	var childID string
	for id := range store.registers {
		if id != testSessionID {
			childID = id
		}
	}
	if childID == "" {
		t.Fatalf("the committed child register is missing")
	}

	childC, err := h.coordinatorFor(context.Background(), childID)
	if err != nil {
		t.Fatalf("coordinatorFor(child): %v", err)
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), childID) }()
	awaitStopInterval(t, childC, stopDone) // the parent membership holds the stop

	close(gate.txRelease)
	if err := stopResult(t, stopDone); err != nil {
		t.Fatalf("Stop(child) = %v, want nil", err)
	}
	if err := <-launchDone; err != nil {
		t.Fatalf("gated launch = %v, want the launch to succeed with the entry interruption", err)
	}
	mu.Lock()
	childOpened := false
	for _, id := range opens {
		if id == childID {
			childOpened = true
		}
	}
	mu.Unlock()
	if childOpened {
		t.Fatalf("the racing child execution opened; want it settled without starting")
	}
	if rec := settledOperation(t, store, childID, "child-op-1"); rec.State.Status != OperationInterruption ||
		rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
		t.Fatalf("child operation state = %+v, want terminal interruption with the contract detail", rec.State)
	}
	if got := runtimeInputTextRaw(t, store, testSessionID); got != "Task interrupted." {
		t.Fatalf("parent runtime input = %q, want the racing start's completion delivered", got)
	}
}

// TestStopChildJoinsReservationAndRun proves the reservation/run join: a stop
// capturing a held admission reservation cannot return until it closes, and
// the installed execution settles at the closed entry.
func TestStopChildJoinsReservationAndRun(t *testing.T) {
	store := completionChildGraph(testSessionID, false, OperationSuccess, "", "").storage(t)
	addRootSession(t, store, testSessionID)
	stub := newPrepareStub(modelPrepared(quickModel()))
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
	defer cancel()
	childC, err := h.coordinatorFor(context.Background(), completionChildID)
	if err != nil {
		t.Fatalf("coordinatorFor(child): %v", err)
	}
	release, err := childC.reserve(context.Background())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	rec, prepared, _, err := h.admitReserved(context.Background(), childC, admissionRequest{
		SessionID:   completionChildID,
		OperationID: "op-2",
		Origin:      InputOriginUser,
		Content:     admissionContent("later"),
	})
	if err != nil || prepared == nil {
		t.Fatalf("admission = (%+v, %+v, %v), want a published new Operation", rec, prepared, err)
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), completionChildID) }()
	awaitStopInterval(t, childC, stopDone) // the held reservation holds the stop

	h.startExecution(childC, "op-2", *prepared) // the ordinary admission installs before releasing
	release()
	if err := stopResult(t, stopDone); err != nil {
		t.Fatalf("Stop(child) = %v, want nil", err)
	}
	if settled := settledOperation(t, store, completionChildID, "op-2"); settled.State.Status != OperationInterruption ||
		settled.State.Terminal == nil || settled.State.Terminal.Detail != executionInterruptedDetail {
		t.Fatalf("installed operation state = %+v, want the closed-entry interruption", settled.State)
	}
	if _, err := submitText(t, h, completionChildID, "op-3", MessageModeRegular, "nope"); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "session is closed to agent work") {
		t.Fatalf("Submit on the closed child = %v, want the closed-to-agent-work rejection", err)
	}
}

// TestStopChildWaitsForLiveJobDelivery proves the live-job row: a child's
// completion delivers only after its live job finishes, no matter how long
// the kill-completion path takes.
func TestStopChildWaitsForLiveJobDelivery(t *testing.T) {
	store := emptyStore(t)
	release := make(chan struct{})
	stub := newPrepareStub(modelPrepared(parkedModel(release)))
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
	defer cancel()
	defer close(release)
	root := createSession(t, h)
	res, err := h.LaunchChildSession(context.Background(), launchRequestFor(root))
	if err != nil {
		t.Fatalf("LaunchChildSession: %v", err)
	}
	child := res.ChildSessionID

	gate := make(chan struct{})
	completions := map[string]string{}
	stopper := &gatedJobStopper{gate: gate, deliver: func(sessionID, jobID string) {
		h.DeliverBackgroundCompletion(context.Background(), sessionID, completions[jobID], "job done")
	}}
	h.deps.Jobs = stopper
	if err := h.StartJob(context.Background(), child, jobFixtureID, func(_ context.Context, completionID string) error {
		completions[jobFixtureID] = completionID
		return nil
	}); err != nil {
		t.Fatalf("StartJob on the child: %v", err)
	}
	rootC := cachedCoordinator(t, h, root)
	rootC.mu.Lock()
	var membership chan struct{}
	for _, m := range rootC.group.members {
		if m.kind == memberChild && m.id == child {
			membership = m.done
		}
	}
	rootC.mu.Unlock()
	if membership == nil {
		t.Fatalf("the child's parent membership is missing")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), child) }()
	awaitStopInterval(t, cachedCoordinator(t, h, child), stopDone)
	if signals := storedSignals(t, store, root); len(signals) != 0 {
		t.Fatalf("parent signals while the job is live = %d, want none", len(signals))
	}
	select {
	case <-membership:
		t.Fatalf("the parent membership closed before the job finished")
	default:
	}

	close(gate) // the job finishes: its kill-completion closes the child, then the child delivers
	if err := stopResult(t, stopDone); err != nil {
		t.Fatalf("Stop(child) = %v, want nil", err)
	}
	if got := runtimeInputTextRaw(t, store, root); got != "Task interrupted." {
		t.Fatalf("parent runtime input = %q, want the child completion delivered after the job finished", got)
	}
	if len(storedSignals(t, store, root)) != 0 {
		t.Fatalf("parent signals = %+v, want the idle parent's admission delivery, not a signal", storedSignals(t, store, root))
	}
	if signals := storedSignals(t, store, child); len(signals) != 1 || signals[0].Content != "job done" {
		t.Fatalf("child signals = %+v, want the job's closed-delivery signal", signals)
	}
	select {
	case <-membership:
	default:
		t.Fatalf("the parent membership did not close")
	}
	if rec := settledOperation(t, store, child, "child-op-1"); rec.State.Status != OperationInterruption {
		t.Fatalf("child operation status = %s, want interruption", rec.State.Status)
	}
}

// insertMalformedChildSession registers one malformed child Session register —
// the corruption-class fixture both error-path stop tests resolve.
func insertMalformedChildSession(t *testing.T, store *graphStorage, sessionID string) {
	t.Helper()
	if err := store.Transact(context.Background(), func(tx Transaction) error {
		_, err := tx.InsertRegister(RegisterDraft{
			Key:     RegisterKey{SessionID: sessionID, Kind: RegisterSession},
			Payload: json.RawMessage(`{"bogus":true}`),
		})
		return err
	}); err != nil {
		t.Fatalf("insert corrupt child register: %v", err)
	}
}

// TestStopConcurrentCallersJoinSameError proves the join row: concurrent
// stops share one interval and every caller receives the same stored error —
// here the corruption-class child failure that Stop claim-finishes without
// delivery.
func TestStopConcurrentCallersJoinSameError(t *testing.T) {
	store := freshSessionStore(t)
	childID := hexID(9)
	insertMalformedChildSession(t, store, childID)
	h, cancel := newCancelableHarness(t, store, modelPrepared(quickModel()), nil)
	defer cancel()
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	member, err := admitLocked(h, c, memberChild, childID, 0)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	gate := make(chan struct{})
	completions := map[string]string{}
	stopper := &gatedJobStopper{gate: gate, deliver: func(sessionID, jobID string) {
		h.DeliverBackgroundCompletion(context.Background(), sessionID, completions[jobID], "job done")
	}}
	h.deps.Jobs = stopper
	if err := h.StartJob(context.Background(), testSessionID, jobFixtureID, func(_ context.Context, completionID string) error {
		completions[jobFixtureID] = completionID
		return nil
	}); err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	// The first stop owns the interval and parks at the gated job. The two
	// joiners call Stop against the provably open interval; the rendezvous
	// polls the interval's observed joiner count under the coordinator lock
	// until both are parked inside it, and only then the gate releases: the
	// joiners cannot return before the owner converges, and an early
	// returning or never joining caller is caught by the count.
	ownerDone := make(chan error, 1)
	go func() { ownerDone <- h.Stop(context.Background(), testSessionID) }()
	c = cachedCoordinator(t, h, testSessionID)
	awaitStopInterval(t, c, ownerDone)
	joinerDone := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			joinerDone <- h.Stop(context.Background(), testSessionID)
		}()
	}
	joinDeadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		joined := 0
		if c.stop != nil {
			joined = c.stop.joiners
		}
		c.mu.Unlock()
		if joined >= 2 {
			break
		}
		if time.Now().After(joinDeadline) {
			t.Fatalf("the Stop joiners never joined the live interval (joined=%d)", joined)
		}
		time.Sleep(2 * time.Millisecond)
	}

	close(gate) // the job finishes: the owner collects the child error and stores it
	err = stopResult(t, ownerDone)
	if err2 := stopResult(t, joinerDone); err2 != err {
		t.Fatalf("joiner = %v, want the same stored error as the owner (%v)", err2, err)
	}
	if err3 := stopResult(t, joinerDone); err3 != err {
		t.Fatalf("joiner = %v, want the same stored error as the owner (%v)", err3, err)
	}
	if err == nil {
		t.Fatalf("the joined error = nil, want the stored child error")
	}
	var corrupt *CorruptionError
	if !errors.As(err, &corrupt) {
		t.Fatalf("joined error = %v, want the corruption class", err)
	}
	waitMemberDone(t, member) // claim-finished without delivery
	if signals := storedSignals(t, store, testSessionID); len(signals) != 0 {
		t.Fatalf("root signals = %+v, want no delivery from the claim-finished child", signals)
	}
	c.mu.Lock()
	reopened := c.bgState == bgOpen
	c.mu.Unlock()
	if !reopened {
		t.Fatalf("root bgState after Stop = %q, want reopened", c.bgState)
	}
}

// TestStopReportsChildErrorAndWaitsForNaturalDelivery proves the error
// collection rows: a corruption-class child failure claim-finishes without
// delivery, while a reported storage-class child error leaves the member to
// its natural path — the stop waits until that path converges.
func TestStopReportsChildErrorAndWaitsForNaturalDelivery(t *testing.T) {
	t.Run("corruption claim-finishes without delivery", func(t *testing.T) {
		store := freshSessionStore(t)
		childID := hexID(9)
		insertMalformedChildSession(t, store, childID)
		h := newTestHarness(t, store, nil)
		c, err := h.coordinatorFor(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("coordinatorFor: %v", err)
		}
		member, err := admitLocked(h, c, memberChild, childID, 0)
		if err != nil {
			t.Fatalf("admission: %v", err)
		}
		err = h.Stop(context.Background(), testSessionID)
		var corrupt *CorruptionError
		if !errors.As(err, &corrupt) {
			t.Fatalf("Stop = %v, want the collected corruption error", err)
		}
		waitMemberDone(t, member)
		if signals := storedSignals(t, store, testSessionID); len(signals) != 0 {
			t.Fatalf("root signals = %+v, want no delivery", signals)
		}
	})

	t.Run("storage error waits for the natural delivery", func(t *testing.T) {
		store := completionChildGraph(testSessionID, false, OperationSuccess, "", "").storage(t)
		addRootSession(t, store, testSessionID)
		stub := newPrepareStub(modelPrepared(quickModel()))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		c, err := h.coordinatorFor(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("coordinatorFor: %v", err)
		}
		member, err := admitLocked(h, c, memberChild, completionChildID, 0)
		if err != nil {
			t.Fatalf("admission: %v", err)
		}

		store.mu.Lock()
		store.registersErr = fmt.Errorf("%w: storage down", ErrStorage) // the child resolution fails
		store.mu.Unlock()
		stopDone := make(chan error, 1)
		go func() { stopDone <- h.Stop(context.Background(), testSessionID) }()
		awaitStopInterval(t, c, stopDone)
		c.mu.Lock()
		claimed := member.claimed
		c.mu.Unlock()
		if claimed {
			t.Fatalf("Stop claim-finished a member whose failure does not prevent natural delivery")
		}

		store.mu.Lock()
		store.registersErr = nil // the transient failure clears: the natural path converges
		store.mu.Unlock()
		childC, err := h.coordinatorFor(context.Background(), completionChildID)
		if err != nil {
			t.Fatalf("coordinatorFor(child): %v", err)
		}
		childC.mu.Lock()
		childC.pendingCompletion = &launchInfo{completionID: member.completionID, outputLimit: 1024}
		childC.mu.Unlock()
		h.childCompletionSettled(childC, "") // the settled child's completion delivers on its own

		err = stopResult(t, stopDone)
		if !errors.Is(err, ErrStorage) {
			t.Fatalf("Stop = %v, want the reported storage-class child error", err)
		}
		waitMemberDone(t, member)
		if got := runtimeInputTextRaw(t, store, testSessionID); got != "Task completed." {
			t.Fatalf("root runtime input = %q, want the natural delivery", got)
		}
		c.mu.Lock()
		reopened := c.bgState == bgOpen
		c.mu.Unlock()
		if !reopened {
			t.Fatalf("root bgState after Stop = %q, want reopened", c.bgState)
		}
	})
}

// TestStopRootRejectsStartWhileStopping proves the gated-member row: a new
// start during a root stop rejects without spawn, and after the stop returns
// the reopened group starts and completes new work normally.
func TestStopRootRejectsStartWhileStopping(t *testing.T) {
	store := emptyStore(t)
	stub := newPrepareStub(modelPrepared(quickModel()))
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
	defer cancel()
	root := createSession(t, h)

	gate := make(chan struct{})
	completions := map[string]string{}
	stopper := &gatedJobStopper{gate: gate, deliver: func(sessionID, jobID string) {
		h.DeliverBackgroundCompletion(context.Background(), sessionID, completions[jobID], "job done")
	}}
	h.deps.Jobs = stopper
	if err := h.StartJob(context.Background(), root, jobFixtureID, func(_ context.Context, completionID string) error {
		completions[jobFixtureID] = completionID
		return nil
	}); err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), root) }()
	awaitStopInterval(t, cachedCoordinator(t, h, root), stopDone) // the parked kill-completion holds the stop

	spawns := 0
	if err := h.StartJob(context.Background(), root, "feedface", func(context.Context, string) error {
		spawns++
		return nil
	}); err == nil || err.Error() != "harness: invalid storage request: background group is closed or stopping" {
		t.Fatalf("start while stopping = %v, want the closed-or-stopping rejection", err)
	}
	if spawns != 0 {
		t.Fatalf("the rejected start spawned %d times", spawns)
	}

	watch := watchSettlements(store) // both kill-completion admissions settle
	close(gate)
	if err := stopResult(t, stopDone); err != nil {
		t.Fatalf("Stop(root) = %v, want nil", err)
	}
	c := cachedCoordinator(t, h, root)
	c.mu.Lock()
	reopened := c.bgState == bgOpen
	c.mu.Unlock()
	if !reopened {
		t.Fatalf("root bgState after Stop = %q, want reopened", c.bgState)
	}
	watch.next() // the first kill-completion admission settled

	var completion2 string
	if err := h.StartJob(context.Background(), root, "feedface", func(_ context.Context, completionID string) error {
		completion2 = completionID
		return nil
	}); err != nil {
		t.Fatalf("StartJob after reopen: %v", err)
	}
	waitQuiet(t, c) // the retiring run releases before the second delivery resolves its mode
	if err := h.DeliverBackgroundCompletion(context.Background(), root, completion2, "job done"); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	watch.next() // the second kill-completion admission
	if rec := settledOperation(t, store, root, completion2); rec.State.Status != OperationSuccess {
		t.Fatalf("new work status = %s, want the reopened group completing it normally", rec.State.Status)
	}
}

// TestStopHeldPreparationRegistersAfterReopen proves the racing-registration
// row: a child preparation held before registration across a completed root
// stop registers successfully against the reopened group. The root keeps a
// running Operation during the window, so both in-window deliveries steer
// without touching the parent register and the launch's publication sees no
// revision drift.
func TestStopHeldPreparationRegistersAfterReopen(t *testing.T) {
	store := emptyStore(t)
	var (
		hold    = make(chan struct{}) // parks the launch's preparation before any registration
		release = make(chan struct{})
		arrived = make(chan struct{}, 1) // the child's preparation entering the stub
	)
	prepare := func(ctx context.Context, req PreparationRequest) (PreparedExecution, error) {
		if req.Session.Identity.ParentSessionID != "" { // the launch's preparation parks; the root's does not
			arrived <- struct{}{}
			<-hold
			return modelPrepared(quickModel()), nil
		}
		return modelPrepared(parkedModel(release)), nil
	}
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, prepare)
	defer cancel()
	defer close(release)
	root := createSession(t, h)
	if _, err := submitText(t, h, root, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}

	gate := make(chan struct{})
	completions := map[string]string{}
	stopper := &gatedJobStopper{gate: gate, deliver: func(sessionID, jobID string) {
		h.DeliverBackgroundCompletion(context.Background(), sessionID, completions[jobID], "job done")
	}}
	h.deps.Jobs = stopper
	if err := h.StartJob(context.Background(), root, jobFixtureID, func(_ context.Context, completionID string) error {
		completions[jobFixtureID] = completionID
		return nil
	}); err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), root) }()
	awaitStopInterval(t, cachedCoordinator(t, h, root), stopDone)

	launchDone := make(chan error, 1)
	go func() {
		_, err := h.LaunchChildSession(context.Background(), launchRequestFor(root))
		launchDone <- err
	}()
	select { // the launch's preparation entered the stub and parks on hold before any registration
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the launch's preparation never entered")
	}

	close(gate) // the stop completes and reopens; the kill-completion steers without a register write
	if err := stopResult(t, stopDone); err != nil {
		t.Fatalf("Stop(root) = %v, want nil", err)
	}
	close(hold) // the held preparation registers against the reopened group
	if err := <-launchDone; err != nil {
		t.Fatalf("held launch after reopen = %v, want success", err)
	}
	c := cachedCoordinator(t, h, root)
	c.mu.Lock()
	reopened := c.bgState == bgOpen
	current := c.graph.Session.State.CurrentOperationID
	c.mu.Unlock()
	if !reopened {
		t.Fatalf("root bgState after the held registration = %q, want reopened", c.bgState)
	}
	if current != "op-1" {
		t.Fatalf("root current operation = %q, want the untouched running Operation", current)
	}
}

// TestArchiveGateRejectsLiveMembers proves the archive gate: a Session with a
// claimed-but-unfinished member is rejected on both lifecycle paths —
// ArchiveSession with the dedicated text, and the sweep's skip.
func TestArchiveGateRejectsLiveMembers(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, nil)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	member, err := admitLocked(h, c, memberJob, jobFixtureID, 0)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if _, ok := claimLocked(h, c, member.completionID); !ok {
		t.Fatalf("the fixture member was not claimed")
	}

	if _, err := h.ArchiveSession(context.Background(), testSessionID); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "session has live background work; stop it first") {
		t.Fatalf("ArchiveSession = %v, want the live-background rejection", err)
	}
	deleted, err := h.Sweep(context.Background(), SweepPolicy{ArchiveAfter: time.Hour}, testTime.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("Sweep deleted %v, want the live member to skip the Session", deleted)
	}
	rec, err := h.ReadSession(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if rec.State.Lifecycle != LifecycleOpen {
		t.Fatalf("swept lifecycle = %q, want the skipped open Session", rec.State.Lifecycle)
	}

	h.finishBackgroundMember(c, member) // the member finishes: both paths proceed
	if _, err := h.ArchiveSession(context.Background(), testSessionID); err != nil {
		t.Fatalf("ArchiveSession after the member finished: %v", err)
	}
}

// TestStopAllMixedTree proves StopAll over a mixed tree: the child is closed
// and interrupted, the job is killed, both members finish, the roots reopen,
// and a second StopAll is a no-op.
func TestStopAllMixedTree(t *testing.T) {
	tree := newStopTree(t)
	defer tree.cancel()
	defer close(tree.release)
	extra := createSession(t, tree.h)

	if err := tree.h.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if got := tree.stopper.stopped; len(got) != 1 || got[0] != tree.root+"/"+jobFixtureID {
		t.Fatalf("StopJob calls = %v, want the tree's job killed", got)
	}
	if rec := settledOperation(t, tree.store, tree.child, "child-op-1"); rec.State.Status != OperationInterruption {
		t.Fatalf("child operation status = %s, want interruption", rec.State.Status)
	}
	childC := cachedCoordinator(t, tree.h, tree.child)
	childC.mu.Lock()
	childClosed := childC.bgState == bgClosed
	childC.mu.Unlock()
	rootC := cachedCoordinator(t, tree.h, tree.root)
	rootC.mu.Lock()
	rootOpen := rootC.bgState == bgOpen
	rootEmpty := rootC.group == nil || len(rootC.group.members) == 0
	extraOpen := tree.h.sessions[extra] != nil
	rootC.mu.Unlock()
	if !childClosed || !rootOpen || !rootEmpty {
		t.Fatalf("after StopAll: childClosed=%v rootOpen=%v rootEmpty=%v", childClosed, rootOpen, rootEmpty)
	}
	if !extraOpen {
		t.Fatalf("the idle root vanished from the registry")
	}

	if err := tree.h.StopAll(context.Background()); err != nil { // idempotent
		t.Fatalf("second StopAll = %v, want nil", err)
	}
}
