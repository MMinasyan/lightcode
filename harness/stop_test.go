package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
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

// readCount reports how many register reads the gate served.
func (g *launchGate) readCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads
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
		testassert.Require(t, !time.Now().After(deadline), "Stop never entered its stop interval")
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

// awaitStopJoiners waits until count distinct current-test goroutines are
// parked in the concurrent-join receive: each must be blocked in a channel
// receive with (*Harness).stopCoordinator as its top application frame and
// must carry this test's wrapper. The owner parked in its gated StopJob —
// its stopCoordinator frame sits deeper — and a goroutine merely started by
// the test do not match, so the wait proves real joining before release.
func awaitStopJoiners(t *testing.T, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 1<<20)
	for {
		stacks := string(buf[:goruntime.Stack(buf, true)])
		seen := 0
		for _, stack := range strings.Split(stacks, "\n\n") {
			if stopJoinerStack(stack, t.Name()) {
				seen++
			}
		}
		if seen >= count {
			return
		}
		testassert.Require(t, !time.Now().After(deadline), "%d concurrent Stop callers: only %d parked in the join receive", count, seen)
		goruntime.Gosched()
	}
}

// stopJoinerStack reports whether one goroutine dump is a concurrent joiner
// of the current test's Stop: parked in a channel receive with
// stopCoordinator as the top application frame and created by the named test.
// No source line is consulted; a deeper stopCoordinator frame (the owner
// under gatedJobStopper) and a caller that has only started never match.
func stopJoinerStack(stack, testName string) bool {
	lines := strings.Split(stack, "\n")
	if len(lines) < 2 {
		return false
	}
	if !strings.Contains(lines[0], "chan receive") { // the join receive itself, not a started caller
		return false
	}
	if !strings.Contains(lines[1], "(*Harness).stopCoordinator") { // the top application frame
		return false
	}
	return strings.Contains(stack, testName) // this test's wrapper, not another Stop stack
}

// waitStopCallers bounds the cleanup join of every started Stop caller: the
// fixture gate is released before this runs, so each unblocked caller
// converges and Done fires; one held past the bound is reported, never hung
// on, and the owner is never left parked.
func waitStopCallers(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("a Stop caller did not converge during cleanup")
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
		testassert.Require(t, err == nil, "decode input entry %s: %v", env.ID, err)
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
		testassert.Require(t, err == nil, "decode input entry %s: %v", env.ID, err)
		out = append(out, ie.Content[0].Text)
	}
	return out
}

// waitQuiet polls one coordinator until its retired execution slot is
// released and no Operation is current: a graphless cell (its last use
// released) is quiet by construction, and a resident graph reports the
// durable current Operation. Deliveries resolving after this point
// deterministically take the idle-admission mode.
func waitQuiet(t *testing.T, c *coordinator) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		quiet := c.run == nil && (c.graph == nil || c.graph.Session.State.CurrentOperationID == "")
		c.mu.Unlock()
		if quiet {
			return
		}
		testassert.Require(t, !time.Now().After(deadline), "the session never went quiet")
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
	{
		_, err := submitText(t, tree.h, tree.root, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	{
		_, err := submitText(t, tree.h, tree.root, "op-2", MessageModeQueued, "queued-msg")
		testassert.NoError(t, err)
	}

	{
		err := tree.h.Stop(context.Background(), tree.root)
		testassert.NoError(t, err)
	}

	{
		got := tree.stopper.stopped
		testassert.Require(t, len(got) == 1 && got[0] == tree.root+"/"+jobFixtureID, "StopJob calls = %v, want the tree's job killed", got)
	}
	waitMemberDone(t, tree.childMember)
	waitMemberDone(t, tree.jobMember)
	{
		rec := settledOperation(t, tree.store, tree.child, "child-op-1")
		testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "child operation state = %+v, want terminal interruption with the contract detail", rec.State)
	}
	rootC := cachedCoordinator(t, tree.h, tree.root)
	rootC.mu.Lock()
	reopened := rootC.bgState == bgOpen
	groupEmpty := rootC.group == nil || len(rootC.group.members) == 0
	queued := len(rootC.queued)
	current := rootC.graph.Session.State.CurrentOperationID
	running := rootC.run != nil
	rootC.mu.Unlock()
	testassert.Require(t, reopened && groupEmpty, "after Stop: reopened=%v groupEmpty=%v, want the group reopened and empty", reopened, groupEmpty)
	testassert.Equal(t, queued, 1)
	testassert.Require(t, current == "op-1" && running, "root operation after Stop = %q (run %v), want op-1 still running", current, running)
	childC := cachedCoordinator(t, tree.h, tree.child)
	childC.mu.Lock()
	closed := childC.bgState == bgClosed
	childC.mu.Unlock()
	if !closed {
		t.Fatalf("stopped child bgState = %q, want permanently closed", childC.bgState)
	}

	close(tree.release) // the parked root Operation completes; the steered completions drain as successors
	watch.next()        // the child's interruption, committed during the stop
	watch.next()        // op-1 settles quiet success at its boundary
	watch.next()        // the first steered completion's own Operation settles
	watch.next()        // the second steered completion's own Operation settles
	watch.next()        // the queued message admitted after the reopen
	{
		rec := settledOperation(t, tree.store, tree.root, "op-2")
		testassert.Equal(t, rec.State.Status, OperationSuccess)
	}
	{
		rec := settledOperation(t, tree.store, tree.root, "op-1")
		testassert.Equal(t, rec.State.Status, OperationSuccess)
	}
	// The member-stop order is unspecified (snapshot map iteration), so the
	// two completions may commit in either order; the set is what survives.
	texts := inputTexts(t, tree.store, tree.root)
	sort.Strings(texts)
	want := []string{"Task interrupted.", "hello", "job done", "queued-msg"}
	sort.Strings(want)
	testassert.Require(t, reflect.DeepEqual(texts, want), "root entries = %q, want the surviving buffers delivered", texts)
	// Each delivered completion owns its own Operation: op-1 keeps exactly its
	// admitted input and the completions arrive through ordinary admission.
	rootGraph, err := validateFixture(t, tree.store, tree.root)
	testassert.NoError(t, err)
	for _, entry := range rootGraph.Entries {
		if entry.Input == nil || len(entry.Input.Content) == 0 {
			continue
		}
		switch entry.Input.Content[0].Text {
		case "hello":
			testassert.Equal(t, entry.Envelope.OperationID, "op-1")
		case "queued-msg":
			testassert.Equal(t, entry.Envelope.OperationID, "op-2")
		default:
			if entry.Envelope.OperationID == "op-1" || entry.Input.Origin != InputOriginRuntime {
				t.Fatalf("completion input %q owned by %q (%q), want its own successor Operation with runtime origin", entry.Input.Content[0].Text, entry.Envelope.OperationID, entry.Input.Origin)
			}
		}
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
	testassert.NoError(t, err)
	child := res.ChildSessionID
	childC := cachedCoordinator(t, h, child)
	childC.mu.Lock()
	run := childC.run
	childC.mu.Unlock()
	{
		_, err := submitText(t, h, child, "op-2", MessageModeRegular, "steer-me")
		testassert.NoError(t, err)
	}

	{
		err := h.Stop(context.Background(), child)
		testassert.NoError(t, err)
	}

	{
		rec := settledOperation(t, store, child, "child-op-1")
		testassert.Equal(t, rec.State.Status, OperationInterruption)
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
	testassert.Equal(t, buffers, 0)
	testassert.Require(t, !storedOperationExists(store, child, "op-2"), "the closed child's discarded head admitted a successor")
	{
		_, err := submitText(t, h, child, "op-3", MessageModeRegular, "nope")
		testassert.Require(t, errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "session is closed to agent work"), "Submit on the closed child = %v, want the closed-to-agent-work rejection", err)
	}
	{
		got := runtimeInputTextRaw(t, store, root)
		testassert.Equal(t, got, "Task interrupted.")
	}
}

// TestSubmitRejectsClosedActiveChild proves Submit's own closed-group gate on
// the active branch: a closed child with a durably running Operation rejects
// before any buffering.
func TestSubmitRejectsClosedActiveChild(t *testing.T) {
	store := completionChildGraph(completionParentID, true, OperationRunning, "", "").storage(t)
	addRootSession(t, store, completionParentID)
	h := newTestHarness(t, store, nil)
	childC := leaseCoordinator(t, h, completionChildID)
	closeBackgroundMode(childC)
	{
		_, err := submitText(t, h, completionChildID, "op-x", MessageModeRegular, "hi")
		testassert.Require(t, errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "session is closed to agent work"), "Submit on the closed active child = %v, want the closed-to-agent-work rejection", err)
	}
}

// TestStopGatedLaunchWaitsForMaterialization proves the materialization-window
// row under the stable-cell protocol: a child Stop that lands while the
// launch's child load is parked shares the child's one stable cell and waits
// for that load — it neither skips the addressed child nor adds more than its
// own one reload — and once released the child is permanently closed, its
// Operation reaches a terminal, and the parent receives the completion before
// the stop returns. The closed-entry interruption outcome itself is pinned by
// TestStopRacingStartSettlesWithoutStarting, whose stop completes before the
// launch resumes.
func TestStopGatedLaunchWaitsForMaterialization(t *testing.T) {
	store := freshSessionStore(t)
	gate := newLaunchGate(store, "read")
	exec := validExecution()
	exec.Model = quickModel()
	stub := newPrepareStub(PreparedExecution{
		Capture: testCapture(),
		Open: func(_ context.Context, _ OperationAdmission) (Execution, error) {
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
	testassert.Require(t, childID != "", "the committed child register is missing")

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), childID) }()
	select {
	case err := <-stopDone:
		t.Fatalf("Stop(child) = %v, want it held behind the parked materialization", err)
	case <-time.After(200 * time.Millisecond):
	}
	sharedLoads := gate.readCount() // the parent's load plus the parked child's: the stop must not add a third
	testassert.Equal(t, sharedLoads, 2)

	close(gate.readRelease)
	{
		err := stopResult(t, stopDone)
		testassert.NoError(t, err)
	}
	{
		err := <-launchDone
		testassert.NoError(t, err)
	}
	// The stop's own acquisition joins the launch's just-published graph when
	// it arrives in time, or — under the one release rule — loads the child
	// once itself; it never loads more than that one.
	{
		got := gate.readCount()
		testassert.Require(t, got == sharedLoads || got == sharedLoads+1, "register reads after the stop joined = %d, want at most the stop's own one load beyond %d", got, sharedLoads)
	}
	{
		rec := settledOperation(t, store, childID, "child-op-1")
		testassert.Require(t, rec.State.Status != OperationRunning && rec.State.Terminal != nil, "child operation state = %+v, want a terminal settlement", rec.State)
	}
	{
		got := runtimeInputTextRaw(t, store, testSessionID)
		testassert.Require(t, got != "", "parent runtime input = %q, want the child completion delivered", got)
	}
	if len(storedSignals(t, store, testSessionID)) != 0 {
		t.Fatalf("parent signals = %+v, want the idle parent's admission delivery, not a signal", storedSignals(t, store, testSessionID))
	}
	childC := cachedCoordinator(t, h, childID)
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
	testassert.Require(t, childID != "", "the committed child register is missing")

	childC := leaseCoordinator(t, h, childID)
	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), childID) }()
	awaitStopInterval(t, childC, stopDone) // the parent membership holds the stop

	close(gate.txRelease)
	{
		err := stopResult(t, stopDone)
		testassert.NoError(t, err)
	}
	{
		err := <-launchDone
		testassert.NoError(t, err)
	}
	mu.Lock()
	childOpened := false
	for _, id := range opens {
		if id == childID {
			childOpened = true
		}
	}
	mu.Unlock()
	testassert.Require(t, !childOpened, "the racing child execution opened; want it settled without starting")
	{
		rec := settledOperation(t, store, childID, "child-op-1")
		testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "child operation state = %+v, want terminal interruption with the contract detail", rec.State)
	}
	{
		got := runtimeInputTextRaw(t, store, testSessionID)
		testassert.Equal(t, got, "Task interrupted.")
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
	childC := leaseCoordinator(t, h, completionChildID)
	release, err := h.reserve(context.Background(), childC)
	testassert.NoError(t, err)
	rec, prepared, _, err := h.admitReserved(context.Background(), childC, admissionRequest{
		SessionID:   completionChildID,
		OperationID: "op-2",
		Kind:        RequestKindMessage,
		Origin:      InputOriginUser,
		Content:     admissionContent("later"),
	})
	testassert.Require(t, err == nil && prepared != nil, "admission = (%+v, %+v, %v), want a published new Operation", rec, prepared, err)

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), completionChildID) }()
	awaitStopInterval(t, childC, stopDone) // the held reservation holds the stop

	h.startExecution(childC, "op-2", *prepared) // the ordinary admission installs before releasing
	release()
	{
		err := stopResult(t, stopDone)
		testassert.NoError(t, err)
	}
	{
		settled := settledOperation(t, store, completionChildID, "op-2")
		testassert.Require(t, settled.State.Status == OperationInterruption && settled.State.Terminal != nil && settled.State.Terminal.Detail == executionInterruptedDetail, "installed operation state = %+v, want the closed-entry interruption", settled.State)
	}
	{
		_, err := submitText(t, h, completionChildID, "op-3", MessageModeRegular, "nope")
		testassert.Require(t, errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "session is closed to agent work"), "Submit on the closed child = %v, want the closed-to-agent-work rejection", err)
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
	testassert.NoError(t, err)
	child := res.ChildSessionID

	gate := make(chan struct{})
	completions := map[string]string{}
	stopper := &gatedJobStopper{gate: gate, deliver: func(sessionID, jobID string) {
		h.DeliverBackgroundCompletion(context.Background(), sessionID, completions[jobID], "job done")
	}}
	h.deps.Jobs = stopper
	{
		err := h.StartJob(context.Background(), child, jobFixtureID, func(_ context.Context, completionID string) error {
			completions[jobFixtureID] = completionID
			return nil
		})
		testassert.NoError(t, err)
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
	testassert.Require(t, membership != nil, "the child's parent membership is missing")

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), child) }()
	awaitStopInterval(t, cachedCoordinator(t, h, child), stopDone)
	{
		signals := storedSignals(t, store, root)
		testassert.Equal(t, len(signals), 0)
	}
	select {
	case <-membership:
		t.Fatalf("the parent membership closed before the job finished")
	default:
	}

	close(gate) // the job finishes: its kill-completion closes the child, then the child delivers
	{
		err := stopResult(t, stopDone)
		testassert.NoError(t, err)
	}
	{
		got := runtimeInputTextRaw(t, store, root)
		testassert.Equal(t, got, "Task interrupted.")
	}
	if len(storedSignals(t, store, root)) != 0 {
		t.Fatalf("parent signals = %+v, want the idle parent's admission delivery, not a signal", storedSignals(t, store, root))
	}
	{
		signals := storedSignals(t, store, child)
		testassert.Require(t, len(signals) == 1 && signals[0].Content == "job done", "child signals = %+v, want the job's closed-delivery signal", signals)
	}
	select {
	case <-membership:
	default:
		t.Fatalf("the parent membership did not close")
	}
	{
		rec := settledOperation(t, store, child, "child-op-1")
		testassert.Equal(t, rec.State.Status, OperationInterruption)
	}
}

// insertMalformedChildSession registers one malformed child Session register —
// the corruption-class fixture both error-path stop tests resolve.
func insertMalformedChildSession(t *testing.T, store *graphStorage, sessionID string) {
	t.Helper()
	{
		err := store.Transact(context.Background(), func(tx Transaction) error {
			_, err := tx.InsertRegister(RegisterDraft{
				Key:     RegisterKey{SessionID: sessionID, Kind: RegisterSession},
				Payload: json.RawMessage(`{"bogus":true}`),
			})
			return err
		})
		testassert.NoError(t, err)
	}
}

// TestStopConcurrentCallersJoinSameError proves the join row: concurrent
// stops share one interval and every joining caller receives the same stored
// error — here the corruption-class child failure that Stop claim-finishes
// without delivery — while a later public Stop after retirement is not a
// joiner and observes the reopened empty root as a nil no-op. The stopper
// stops the job member exactly once, the corrupt child register stays
// byte-identical, and the root admits and completes ordinary work after
// convergence.
func TestStopConcurrentCallersJoinSameError(t *testing.T) {
	store := freshSessionStore(t)
	childID := hexID(9)
	insertMalformedChildSession(t, store, childID)
	h, cancel := newCancelableHarness(t, store, modelPrepared(quickModel()), nil)
	defer cancel()
	c := leaseCoordinator(t, h, testSessionID)
	member, err := admitLocked(h, c, memberChild, childID, 0)
	testassert.NoError(t, err)
	corruptKey := RegisterKey{SessionID: childID, Kind: RegisterSession}
	corruptBefore, err := store.ReadRegister(context.Background(), corruptKey)
	testassert.NoError(t, err)
	gate := make(chan struct{})
	completions := map[string]string{}
	stopper := &gatedJobStopper{gate: gate, deliver: func(sessionID, jobID string) {
		h.DeliverBackgroundCompletion(context.Background(), sessionID, completions[jobID], "job done")
	}}
	h.deps.Jobs = stopper
	{
		err := h.StartJob(context.Background(), testSessionID, jobFixtureID, func(_ context.Context, completionID string) error {
			completions[jobFixtureID] = completionID
			return nil
		})
		testassert.NoError(t, err)
	}

	// The first stop owns the interval and parks at the gated job. Every
	// concurrent caller below starts while that interval is provably open.
	// The bounded stack wait then requires the two joiners actually parked
	// in the join receive — not merely announced — before the gate releases.
	// A later public Stop runs synchronously after retirement: it is not a
	// joiner and observes the reopened empty root as a nil no-op —
	// goroutine start is not a join.
	ownerDone := make(chan error, 1)
	joinerDone := make(chan error, 2)
	var (
		releaseGate sync.Once
		stopWG      sync.WaitGroup
	)
	stopWG.Add(1)
	go func() {
		defer stopWG.Done()
		ownerDone <- h.Stop(context.Background(), testSessionID)
	}()
	defer func() { // a failed phase still releases the fixture gate once and joins the started Stop callers
		releaseGate.Do(func() { close(gate) })
		waitStopCallers(t, &stopWG)
	}()
	awaitStopInterval(t, cachedCoordinator(t, h, testSessionID), ownerDone)
	for i := 0; i < 2; i++ {
		stopWG.Add(1)
		go func() {
			defer stopWG.Done()
			joinerDone <- h.Stop(context.Background(), testSessionID)
		}()
	}
	awaitStopJoiners(t, 2)

	releaseGate.Do(func() { close(gate) }) // the job finishes: the owner collects the child error and stores it
	err = stopResult(t, ownerDone)
	{
		err2 := stopResult(t, joinerDone)
		testassert.Equal(t, err2, err)
	}
	{
		err3 := stopResult(t, joinerDone)
		testassert.Equal(t, err3, err)
	}
	{
		errLate := h.Stop(context.Background(), testSessionID) // a post-retirement public Stop is not a joiner
		testassert.NoError(t, errLate)                         // the empty reopened root stops as a nil no-op
	}
	testassert.Require(t, err != nil, "the joined error = nil, want the stored child error")
	var corrupt *CorruptionError
	testassert.Require(t, errors.As(err, &corrupt), "joined error = %v, want the corruption class", err)
	waitMemberDone(t, member) // claim-finished without delivery
	{
		signals := storedSignals(t, store, testSessionID)
		testassert.Require(t, len(signals) == 0, "root signals = %+v, want no delivery from the claim-finished child", signals)
	}
	c.mu.Lock()
	reopened := c.bgState == bgOpen
	c.mu.Unlock()
	if !reopened {
		t.Fatalf("root bgState after Stop = %q, want reopened", c.bgState)
	}
	{
		stopped := stopper.stopped
		testassert.Require(t, len(stopped) == 1 && stopped[0] == testSessionID+"/"+jobFixtureID, "StopJob calls = %q, want exactly the one job member's stop", stopped)
	}
	corruptAfter, err := store.ReadRegister(context.Background(), corruptKey)
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(corruptBefore, corruptAfter), "the corrupt child register changed: before %+v after %+v", corruptBefore, corruptAfter)

	// The root lifecycle is usable after convergence: once the delivered
	// job completion's own Operation retires, the next ordinary input
	// admits and completes.
	waitQuiet(t, c)
	{
		_, err := submitText(t, h, testSessionID, "op-root", MessageModeRegular, "after stop")
		testassert.NoError(t, err)
	}
	awaitOperationTerminal(t, store, testSessionID, "op-root", OperationSuccess)
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
		c := leaseCoordinator(t, h, testSessionID)
		member, err := admitLocked(h, c, memberChild, childID, 0)
		testassert.NoError(t, err)
		err = h.Stop(context.Background(), testSessionID)
		var corrupt *CorruptionError
		testassert.Require(t, errors.As(err, &corrupt), "Stop = %v, want the collected corruption error", err)
		waitMemberDone(t, member)
		{
			signals := storedSignals(t, store, testSessionID)
			testassert.Require(t, len(signals) == 0, "root signals = %+v, want no delivery", signals)
		}
	})

	t.Run("storage error waits for the natural delivery", func(t *testing.T) {
		store := completionChildGraph(testSessionID, false, OperationSuccess, "", "").storage(t)
		addRootSession(t, store, testSessionID)
		stub := newPrepareStub(modelPrepared(quickModel()))
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
		defer cancel()
		c := leaseCoordinator(t, h, testSessionID)
		member, err := admitLocked(h, c, memberChild, completionChildID, 0)
		testassert.NoError(t, err)

		store.mu.Lock()
		store.registersErr = fmt.Errorf("%w: storage down", ErrStorage) // the child resolution fails
		store.mu.Unlock()
		stopDone := make(chan error, 1)
		go func() { stopDone <- h.Stop(context.Background(), testSessionID) }()
		awaitStopInterval(t, c, stopDone)
		c.mu.Lock()
		claimed := member.claimed
		c.mu.Unlock()
		testassert.Require(t, !claimed, "Stop claim-finished a member whose failure does not prevent natural delivery")

		store.mu.Lock()
		store.registersErr = nil // the transient failure clears: the natural path converges
		store.mu.Unlock()
		childC := leaseCoordinator(t, h, completionChildID)
		childC.mu.Lock()
		childC.pendingCompletion = &launchInfo{completionID: member.completionID, outputLimit: 1024}
		childC.mu.Unlock()
		h.childCompletionSettled(childC, "") // the settled child's completion delivers on its own

		err = stopResult(t, stopDone)
		testassert.Require(t, errors.Is(err, ErrStorage), "Stop = %v, want the reported storage-class child error", err)
		waitMemberDone(t, member)
		{
			got := runtimeInputTextRaw(t, store, testSessionID)
			testassert.Equal(t, got, "Task completed.")
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
	{
		err := h.StartJob(context.Background(), root, jobFixtureID, func(_ context.Context, completionID string) error {
			completions[jobFixtureID] = completionID
			return nil
		})
		testassert.NoError(t, err)
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), root) }()
	awaitStopInterval(t, cachedCoordinator(t, h, root), stopDone) // the parked kill-completion holds the stop

	spawns := 0
	{
		err := h.StartJob(context.Background(), root, "feedface", func(context.Context, string) error {
			spawns++
			return nil
		})
		testassert.Require(t, err != nil && err.Error() == "harness: invalid storage request: background group is closed or stopping", "start while stopping = %v, want the closed-or-stopping rejection", err)
	}
	testassert.Equal(t, spawns, 0)

	watch := watchSettlements(store) // both kill-completion admissions settle
	close(gate)
	{
		err := stopResult(t, stopDone)
		testassert.NoError(t, err)
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
	{
		err := h.StartJob(context.Background(), root, "feedface", func(_ context.Context, completionID string) error {
			completion2 = completionID
			return nil
		})
		testassert.NoError(t, err)
	}
	waitQuiet(t, c) // the retiring run releases before the second delivery resolves its mode
	{
		err := h.DeliverBackgroundCompletion(context.Background(), root, completion2, "job done")
		testassert.NoError(t, err)
	}
	watch.next() // the second kill-completion admission
	{
		rec := settledOperation(t, store, root, completion2)
		testassert.Equal(t, rec.State.Status, OperationSuccess)
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
	)
	prepare := func(ctx context.Context, req PreparationRequest) (PreparedExecution, error) {
		if req.Session.Identity.ParentSessionID != "" { // the launch's preparation parks; the root's does not
			<-hold
			return modelPrepared(quickModel()), nil
		}
		return modelPrepared(parkedModel(release)), nil
	}
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, prepare)
	defer cancel()
	defer close(release)
	root := createSession(t, h)
	{
		_, err := submitText(t, h, root, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}

	gate := make(chan struct{})
	completions := map[string]string{}
	stopper := &gatedJobStopper{gate: gate, deliver: func(sessionID, jobID string) {
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

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), root) }()
	awaitStopInterval(t, cachedCoordinator(t, h, root), stopDone)

	launchDone := make(chan error, 1)
	go func() {
		_, err := h.LaunchChildSession(context.Background(), launchRequestFor(root))
		launchDone <- err
	}()
	select { // the preparation is held before any registration
	case err := <-launchDone:
		t.Fatalf("the held launch returned early: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(gate) // the stop completes and reopens; the kill-completion steers without a register write
	{
		err := stopResult(t, stopDone)
		testassert.NoError(t, err)
	}
	close(hold) // the held preparation registers against the reopened group
	{
		err := <-launchDone
		testassert.NoError(t, err)
	}
	c := cachedCoordinator(t, h, root)
	c.mu.Lock()
	reopened := c.bgState == bgOpen
	current := c.graph.Session.State.CurrentOperationID
	c.mu.Unlock()
	if !reopened {
		t.Fatalf("root bgState after the held registration = %q, want reopened", c.bgState)
	}
	testassert.Equal(t, current, "op-1")
}

// TestArchiveGateRejectsLiveMembers proves the archive gate: a Session with a
// claimed-but-unfinished member is rejected on both lifecycle paths —
// ArchiveSession with the dedicated text, and the sweep's skip.
func TestArchiveGateRejectsLiveMembers(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, nil)
	c := leaseCoordinator(t, h, testSessionID)
	member, err := admitLocked(h, c, memberJob, jobFixtureID, 0)
	testassert.NoError(t, err)
	{
		_, ok := claimLocked(h, c, member.completionID)
		testassert.Require(t, ok, "the fixture member was not claimed")
	}

	{
		_, err := h.ArchiveSession(context.Background(), testSessionID)
		testassert.Require(t, errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "session has live background work; stop it first"), "ArchiveSession = %v, want the live-background rejection", err)
	}
	deleted, err := h.SweepSession(context.Background(), testSessionID, SweepPolicy{ArchiveAfter: time.Hour}, testTime.Add(2*time.Hour))
	testassert.NoError(t, err)
	testassert.Require(t, !deleted, "SweepSession deleted the Session, want the live member to skip it")
	rec, err := h.ReadSessionHeader(context.Background(), testSessionID)
	testassert.NoError(t, err)
	testassert.Equal(t, rec.Lifecycle, LifecycleOpen)

	h.finishBackgroundMember(c, member) // the member finishes: both paths proceed
	{
		_, err := h.ArchiveSession(context.Background(), testSessionID)
		testassert.NoError(t, err)
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

	{
		err := tree.h.StopAll(context.Background())
		testassert.NoError(t, err)
	}
	{
		got := tree.stopper.stopped
		testassert.Require(t, len(got) == 1 && got[0] == tree.root+"/"+jobFixtureID, "StopJob calls = %v, want the tree's job killed", got)
	}
	{
		rec := settledOperation(t, tree.store, tree.child, "child-op-1")
		testassert.Equal(t, rec.State.Status, OperationInterruption)
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
	testassert.Require(t, childClosed && rootOpen && rootEmpty, "after StopAll: childClosed=%v rootOpen=%v rootEmpty=%v", childClosed, rootOpen, rootEmpty)
	testassert.Require(t, extraOpen, "the idle root vanished from the registry")

	// idempotent
	{
		err := tree.h.StopAll(context.Background())
		testassert.NoError(t, err)
	}
}
