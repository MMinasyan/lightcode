package harness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/model"
)

// The graph-lifetime rows pin the one retention rule: a Session graph is
// resident exactly while a use holds it and is released at the final use
// release, while the stable cell, its retained identity and revision, and
// every local field survive. The rows cover the rule's semantic axes: the
// final release leaves no idle graph, overlapping uses retain until the
// actual last release, execution and successor uses stay pinned, metadata
// enumeration and StopAll leave no resident corpus, sticky corruption
// survives, a failed first load stays retryable, deletion and refresh still
// answer, a cancelled member finish settles the child completion, and a
// graphless child still renders its completion.
//
// Every fixture that observes a released graph holds no inspection lease
// across the release window; a lease a fixture does hold is explicit and is
// released by the fixture itself.

// TestFinalReleaseLeavesNoIdleGraph proves the rule and the survival of every
// local fact: a released graph is gone while its cell, local revisions,
// groups, and buffers stay, and the graphless cell remains fully usable
// through its own re-materialization, a metadata archive adoption, and a live
// member's completion delivery.
func TestFinalReleaseLeavesNoIdleGraph(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, newPrepareStub(modelPrepared(quickModel())).prepare)
	h.deps.Jobs = &gatedJobStopper{}
	ctx := context.Background()

	idle := createSession(t, h)

	// The first subject carries a live job member.
	subject := createSession(t, h)
	var completionID string
	if err := h.StartJob(ctx, subject, jobFixtureID, func(_ context.Context, id string) error {
		completionID = id
		return nil
	}); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	subjectC := cachedCoordinator(t, h, subject)
	subjectC.mu.Lock()
	subjectRev, subjectMembers := subjectC.localRev, len(subjectC.group.members)
	subjectC.mu.Unlock()
	if subjectMembers != 1 {
		t.Fatalf("job member count = %d, want one", subjectMembers)
	}

	// The second subject carries a running Operation that owns no execution
	// and one buffered steering item beside it, driven under the fixture's own
	// explicit lease.
	buffered := createSession(t, h)
	bufferedC, bufferedLease := leaseCoordinatorNow(t, h, buffered)
	reservation, err := h.reserve(ctx, bufferedC)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	_, prepared, disposition, err := h.admitReserved(ctx, bufferedC, admissionRequest{
		SessionID:   buffered,
		OperationID: "op-running",
		Kind:        RequestKindMessage,
		Origin:      InputOriginUser,
		Content:     admissionContent("first"),
	})
	reservation()
	if err != nil || disposition != DispositionAdmitted {
		t.Fatalf("admission = %q err %v, want a new admission", disposition, err)
	}
	if prepared == nil {
		t.Fatalf("the admission returned no prepared execution")
	}
	if _, err := h.Submit(ctx, SubmitRequest{
		SessionID:   buffered,
		OperationID: "op-steer",
		Origin:      InputOriginUser,
		Content:     admissionContent("steer"),
		Mode:        MessageModeRegular,
	}); err != nil {
		t.Fatalf("steering submit: %v", err)
	}
	bufferedC.mu.Lock()
	bufferedRev, bufferedSteering := bufferedC.localRev, len(bufferedC.steering)
	bufferedC.mu.Unlock()
	if bufferedSteering != 1 {
		t.Fatalf("steering items = %d, want one", bufferedSteering)
	}
	bufferedLease() // the explicit lease ends: the graph releases with it

	// Every released graph is gone; the cells are the same objects and keep
	// every local fact.
	for _, row := range []struct {
		id       string
		cell     *coordinator
		revision uint64
		members  int
		steering int
	}{
		{subject, subjectC, subjectRev, 1, 0},
		{buffered, bufferedC, bufferedRev, 0, 1},
	} {
		if again := cachedCoordinator(t, h, row.id); again != row.cell {
			t.Fatalf("the released cell of %q was replaced", row.id)
		}
		row.cell.mu.Lock()
		graph := row.cell.graph
		revision, members, steering := row.cell.localRev, 0, len(row.cell.steering)
		if row.cell.group != nil {
			members = len(row.cell.group.members)
		}
		row.cell.mu.Unlock()
		if graph != nil {
			t.Fatalf("the released graph of %q stayed resident", row.id)
		}
		if revision != row.revision {
			t.Fatalf("local revision of %q = %d, want the retained %d", row.id, revision, row.revision)
		}
		if members != row.members || steering != row.steering {
			t.Fatalf("local facts of %q = %d members, %d steering items, want %d and %d",
				row.id, members, steering, row.members, row.steering)
		}
	}
	idleC := cachedCoordinator(t, h, idle)
	idleC.mu.Lock()
	idleGraph := idleC.graph
	idleC.mu.Unlock()
	if idleGraph != nil {
		t.Fatalf("the idle Session's released graph stayed resident")
	}

	// A graphless Session stays usable: a real history use re-materializes and
	// releases its graph, and a metadata archive adoption commits against the
	// same cell.
	if _, err := h.ReadHistoryView(ctx, idle); err != nil {
		t.Fatalf("ReadHistoryView of the graphless Session: %v", err)
	}
	idleC.mu.Lock()
	regrown := idleC.graph
	idleC.mu.Unlock()
	if regrown != nil {
		t.Fatalf("the real history use left its graph resident after the call")
	}
	archived, err := h.ArchiveSession(ctx, idle)
	if err != nil || archived.State.Lifecycle != LifecycleArchived {
		t.Fatalf("ArchiveSession of the graphless Session = %+v err %v, want the committed archive", archived, err)
	}

	// The live member's completion still delivers from the graphless cell; the
	// delivered execution's own use releases as it settles, leaving no
	// resident graph behind.
	if err := h.DeliverBackgroundCompletion(ctx, subject, completionID, "job done"); err != nil {
		t.Fatalf("DeliverBackgroundCompletion from the graphless cell: %v", err)
	}
	if got := runtimeInputTextRaw(t, store, subject); got != "job done" {
		t.Fatalf("delivered completion text = %q, want the job's own content", got)
	}
	subjectC.mu.Lock()
	remaining := 0
	if subjectC.group != nil {
		remaining = len(subjectC.group.members)
	}
	subjectC.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("the finished job member stayed live: %d members", remaining)
	}
	deadline := time.Now().Add(10 * time.Second) // the delivered execution's own pin ends asynchronously
	for {
		subjectC.mu.Lock()
		graph := subjectC.graph
		subjectC.mu.Unlock()
		if graph == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the delivered execution left its graph resident")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestOverlappingUsesRetainTheGraphUntilTheFinalRelease proves the one
// retention decision at the release boundary: an overlapping use keeps the
// graph resident through an earlier release, and the actual last release
// clears it while the cell, its retained identity, and its revision pair stay.
func TestOverlappingUsesRetainTheGraphUntilTheFinalRelease(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, nil)
	ctx := context.Background()

	cell, lease := leaseCoordinatorNow(t, h, testSessionID) // the interval the test controls
	if _, err := h.ReadHistoryView(ctx, testSessionID); err != nil {
		t.Fatalf("ReadHistoryView: %v", err)
	}
	cell.mu.Lock()
	resident := cell.graph != nil
	cell.mu.Unlock()
	if !resident {
		t.Fatalf("the overlapping real use's release cleared the graph although the controlled use was still held")
	}

	lease()
	cell.mu.Lock()
	cleared, identity, revision := cell.graph == nil, cell.identity, cell.durableRevision
	cell.mu.Unlock()
	if !cleared {
		t.Fatalf("the final release left the graph resident")
	}
	if identity.SessionID != testSessionID || revision <= 0 {
		t.Fatalf("the released cell = identity %+v revision %d, want the retained established values", identity, revision)
	}
	if _, pair, ok := h.ReadObservation(testSessionID); !ok || pair.DurableRevision != revision {
		t.Fatalf("ReadObservation of the released cell = %+v %v, want the retained revision %d", pair, ok, revision)
	}
}

// TestLatchedCellSurvivesItsGraphRelease proves the availability rule at the
// release boundary: a warm Session whose cell latches corruption through a
// real Fork discovery keeps the sticky latch and its stable cell when the
// graph is released, later reads answer the corruption class, and a metadata
// transition that latches a warm borrowed cell leaves no resident graph
// either.
func TestLatchedCellSurvivesItsGraphRelease(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
	ctx := context.Background()

	source := createSession(t, h)
	sourceCell, sourceLease := leaseCoordinatorNow(t, h, source) // the interval whose release the test controls
	sourceCell.mu.Lock()
	residentBefore := sourceCell.graph != nil
	sourceCell.mu.Unlock()
	if !residentBefore {
		t.Fatalf("the source's graph was not resident before the discovery")
	}

	// The real discovery is Fork's store-wide transaction failing on the
	// foreign-corrupted durable register, which latches the marker on the warm
	// cell without a load.
	foreignCorruptRegister(t, store, source)
	if _, err := h.Fork(ctx, ForkRequest{
		SourceSessionID: source,
		BoundaryEntryID: hexID(1),
		OperationID:     "op-fork-latched",
		Content:         admissionContent("fork"),
	}); !isCorruption(err) {
		t.Fatalf("Fork over the corrupt register = %v, want the typed corruption", err)
	}
	h.mu.Lock()
	latched := sourceCell.corru
	h.mu.Unlock()
	if latched == nil {
		t.Fatalf("the Fork discovery did not latch the source cell")
	}

	sourceLease() // the latched interval's genuine final release
	sourceCell.mu.Lock()
	graph := sourceCell.graph
	sourceCell.mu.Unlock()
	h.mu.Lock()
	registered := h.sessions[source] == sourceCell
	h.mu.Unlock()
	if !registered || sourceCell.corru == nil {
		t.Fatalf("the latched cell did not survive its release (registered %v)", registered)
	}
	if graph != nil {
		t.Fatalf("the latched graph stayed resident after its release")
	}
	if _, err := h.ReadSessionHeader(ctx, source); !isCorruption(err) {
		t.Fatalf("the latched Session read = %v, want the sticky corruption class", err)
	}

	// A metadata transition that latches a warm cell leaves it without a
	// resident graph too: the latch lands through the transition itself.
	borrowStore := emptyStore(t)
	borrowHarness := newTestHarness(t, borrowStore, nil)
	borrowedSource := createSession(t, borrowHarness)
	if _, err := borrowHarness.ReadHistoryView(ctx, borrowedSource); err != nil {
		t.Fatalf("ReadHistoryView: %v", err)
	}
	foreignCorruptRegister(t, borrowStore, borrowedSource)
	if _, err := borrowHarness.ArchiveSession(ctx, borrowedSource); !isCorruption(err) {
		t.Fatalf("metadata transition over the corrupt register = %v, want the typed corruption", err)
	}
	borrowHarness.mu.Lock()
	borrowedCell := borrowHarness.sessions[borrowedSource]
	borrowHarness.mu.Unlock()
	if borrowedCell == nil || borrowedCell.corru == nil {
		t.Fatalf("the metadata transition did not latch the borrowed cell")
	}
	borrowedCell.mu.Lock()
	borrowedGraph := borrowedCell.graph
	borrowedCell.mu.Unlock()
	if borrowedGraph != nil {
		t.Fatalf("the metadata transition left the latched graph resident")
	}
	if _, err := borrowHarness.ReadSessionHeader(ctx, borrowedSource); !isCorruption(err) {
		t.Fatalf("the latched borrowed Session read = %v, want the sticky corruption class", err)
	}
}

// TestStopAllExcludesNeverLoadedLatchedCells proves the stop snapshot's
// exclusion: a never-loaded cell that latched a cold corruption is not a stop
// candidate — StopAll neither loads it nor promotes its error — while an
// established root with a live member still stops and converges.
func TestStopAllExcludesNeverLoadedLatchedCells(t *testing.T) {
	store := freshSessionStore(t)
	foreignCorruptRegister(t, store, testSessionID)
	h := newTestHarness(t, store, newPrepareStub(modelPrepared(quickModel())).prepare)
	ctx := context.Background()

	if _, err := h.ReadSessionHeader(ctx, testSessionID); !isCorruption(err) {
		t.Fatalf("cold corrupt lookup = %v, want the corruption class", err)
	}
	h.mu.Lock()
	latchedCell := h.sessions[testSessionID]
	established := latchedCell != nil && latchedCell.everLoaded
	h.mu.Unlock()
	if latchedCell == nil || latchedCell.corru == nil {
		t.Fatalf("the cold corruption did not latch on a stable cell")
	}
	if established {
		t.Fatalf("the latched never-loaded cell was marked established")
	}

	completions := map[string]string{}
	h.deps.Jobs = &gatedJobStopper{deliver: func(sessionID, jobID string) {
		_ = h.DeliverBackgroundCompletion(context.Background(), sessionID, completions[jobID], "job done")
	}}
	root := createSession(t, h)
	if err := h.StartJob(ctx, root, jobFixtureID, func(_ context.Context, completionID string) error {
		completions[jobFixtureID] = completionID
		return nil
	}); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if err := h.StopAll(ctx); err != nil {
		t.Fatalf("StopAll = %v, want nil: the never-loaded latched cell owns no stop", err)
	}
	if got := h.deps.Jobs.(*gatedJobStopper).stopped; len(got) != 1 || got[0] != root+"/"+jobFixtureID {
		t.Fatalf("StopJob calls = %v, want the established root's one member", got)
	}
	if got := runtimeInputTextRaw(t, store, root); got != "job done" {
		t.Fatalf("root completion text = %q, want the stopped member's delivery", got)
	}
	h.mu.Lock()
	survived := h.sessions[testSessionID] == latchedCell && latchedCell.corru != nil
	h.mu.Unlock()
	if !survived {
		t.Fatalf("StopAll dropped the latched never-loaded cell")
	}
	rootCell := cachedCoordinator(t, h, root)
	rootCell.mu.Lock()
	members := 0
	if rootCell.group != nil {
		members = len(rootCell.group.members)
	}
	rootCell.mu.Unlock()
	if members != 0 {
		t.Fatalf("the established root's member stayed live: %d", members)
	}
}

// cancelledReadStore makes the private fake storage honor a canceled context
// on its reads exactly like the shipped stores do, so a cold load under a
// canceled context fails instead of succeeding against the in-process fake.
type cancelledReadStore struct {
	Storage
}

func (s cancelledReadStore) ReadRegisters(ctx context.Context, sessionID string) ([]Register, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.Storage.ReadRegisters(ctx, sessionID)
}

func (s cancelledReadStore) ReadEntries(ctx context.Context, sessionID string, after int64) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.Storage.ReadEntries(ctx, sessionID, after)
}

// cancelledCompletionFixture is the shared cancelled child-with-Job
// completion fixture: the child and parent cells with the child's job member
// and pending completion installed, the parent's graph pinned by the test's
// own controlled lease, and the Harness already canceled. The child's own
// admission lease is released before the cancellation, so the settlement
// meets the production graphless child; the parent's graph and eight more
// idle borrowed Sessions are the older entries a pre-fix fallback eviction
// skips, making the child's delivery release the eviction victim.
type cancelledCompletionFixture struct {
	h      *Harness
	child  *coordinator
	parent *coordinator
	job    *backgroundMember
}

func newCancelledCompletionFixture(t *testing.T, storage Storage, store *graphStorage) *cancelledCompletionFixture {
	t.Helper()
	ctx := context.Background()
	addRootSession(t, store, completionParentID)
	h, cancel := newCancelableHarness(t, storage, PreparedExecution{}, nil)
	t.Cleanup(cancel)
	if _, err := h.ReadHistoryView(ctx, completionChildID); err != nil {
		t.Fatalf("child ReadHistoryView: %v", err)
	}
	if _, err := h.ReadHistoryView(ctx, completionParentID); err != nil {
		t.Fatalf("parent ReadHistoryView: %v", err)
	}
	child := cachedCoordinator(t, h, completionChildID)
	parent := cachedCoordinator(t, h, completionParentID)

	// The parent's own controlled lease pins its graph through the
	// settlement; the child's covers only the admissions.
	parentLease, parentRelease := leaseCoordinatorNow(t, h, completionParentID)
	t.Cleanup(parentRelease)
	childLease, childRelease := leaseCoordinatorNow(t, h, completionChildID)
	parentMember, err := admitLocked(h, parentLease, memberChild, completionChildID, 0)
	if err != nil {
		t.Fatalf("child member admission: %v", err)
	}
	jobMember, err := admitLocked(h, childLease, memberJob, jobFixtureID, 0)
	if err != nil {
		t.Fatalf("job member admission: %v", err)
	}
	childLease.mu.Lock()
	childLease.pendingCompletion = &launchInfo{completionID: parentMember.completionID, outputLimit: 1024}
	childLease.mu.Unlock()
	childRelease()

	borrowed := make([]func(), 0, 8)
	for i := 0; i < 8; i++ {
		id := createSession(t, h)
		_, release := leaseCoordinatorNow(t, h, id)
		borrowed = append(borrowed, release)
	}
	t.Cleanup(func() {
		for _, release := range borrowed {
			release()
		}
	})

	cancel()
	return &cancelledCompletionFixture{h: h, child: child, parent: parent, job: jobMember}
}

// TestCancelledMemberFinishSettlesTheChildCompletion proves the loss scenario
// the member-finish ordering and the settlement context close together: a
// child Session owns a live Job member and its own pending completion, the
// Harness is canceled, and the child's graph is not resident when the member
// finish triggers the settlement. The child's terminal completion still
// renders and reaches the parent as its durable closed-path record instead of
// being abandoned; a storage failure on that settlement's graph reload is
// retained and reported by Wait, with no retry, fallback write, or silent
// abandonment.
func TestCancelledMemberFinishSettlesTheChildCompletion(t *testing.T) {
	t.Run("the settlement delivers the durable closed-path record", func(t *testing.T) {
		store := completionChildGraph(completionParentID, false, OperationSuccess, "", "child text").storage(t)
		f := newCancelledCompletionFixture(t, cancelledReadStore{Storage: store}, store)

		// The observer is installed after the admissions, so the collected
		// facts are the finish-time ones.
		col := observeHarness(f.h)
		if err := f.h.DeliverBackgroundCompletion(context.Background(), completionChildID, f.job.completionID, "job done"); err != nil {
			t.Fatalf("job completion delivery: %v", err)
		}
		jobFacts := 0
		for _, fact := range col.invalidations() {
			if fact.SessionID == completionChildID && fact.JobID == jobFixtureID {
				jobFacts++
			}
		}
		if jobFacts != 1 {
			t.Fatalf("child job invalidations = %d, want the member finish's one fact from the graphless cell", jobFacts)
		}
		signals := storedSignals(t, store, completionParentID)
		if len(signals) != 1 || signals[0].Content != "child text" {
			t.Fatalf("parent signals = %+v, want the child's rendered completion as the closed-path record", signals)
		}
		if childSignals := storedSignals(t, store, completionChildID); len(childSignals) != 1 || childSignals[0].Content != "job done" {
			t.Fatalf("child signals = %+v, want the job's own completion record", childSignals)
		}
		f.parent.mu.Lock()
		remaining := 0
		if f.parent.group != nil {
			remaining = len(f.parent.group.members)
		}
		f.parent.mu.Unlock()
		if remaining != 0 {
			t.Fatalf("the child member stayed live after the settlement: %d members", remaining)
		}
		f.child.mu.Lock()
		pending := f.child.pendingCompletion
		f.child.mu.Unlock()
		if pending != nil {
			t.Fatalf("the pending completion was not consumed: %+v", pending)
		}
	})

	t.Run("the settlement's storage failure is retained for Wait", func(t *testing.T) {
		store := completionChildGraph(completionParentID, false, OperationSuccess, "", "child text").storage(t)
		f := newCancelledCompletionFixture(t, store, store)

		// The entry hook runs under the store lock and arms the following
		// graph reload's register read with a distinct storage failure on the
		// child's operationless Job-completion signal insertion. The inserting
		// transaction reads the singular register and finishes; the settlement's
		// reload uses ReadRegisters and fails.
		distinct := fmt.Errorf("%w: child register reload down", ErrStorage)
		store.entryHook = func(draft EntryDraft) error {
			if draft.SessionID == completionChildID && draft.Kind == EntrySignal {
				store.registersErr = distinct
			}
			return nil
		}

		// The observer pauses the producer after the abandoned parent member's
		// done closes, so Wait can be asserted while the producer is still
		// parked after that finish.
		arrived := make(chan struct{})
		releaseObserver := make(chan struct{})
		var once sync.Once
		f.h.deps.Observe = func(fact HarnessFact) {
			if fact.Kind != FactInvalidation || fact.SessionID != completionParentID {
				return
			}
			once.Do(func() { close(arrived) })
			<-releaseObserver
		}

		producer := make(chan error, 1)
		go func() {
			producer <- f.h.DeliverBackgroundCompletion(context.Background(), completionChildID, f.job.completionID, "job done")
		}()
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the abandoned parent member never finished")
		}

		// The storage cause is retained before the abandonment finishes the
		// parent member; the public wait reports it while the producer is
		// still parked after that finish.
		if err := f.h.Wait(context.Background()); !errors.Is(err, ErrStorage) || !strings.Contains(err.Error(), "child register reload down") {
			t.Fatalf("Wait while the producer is parked = %v, want the retained storage cause", err)
		}

		close(releaseObserver)
		select {
		case err := <-producer:
			if err != nil {
				t.Fatalf("job completion delivery: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the producer never joined")
		}

		// One attempt, no retry and no fallback write: the child's own signal
		// is durable, the obligation is abandoned, and the parent received
		// nothing.
		if childSignals := storedSignals(t, store, completionChildID); len(childSignals) != 1 || childSignals[0].Content != "job done" {
			t.Fatalf("child signals = %+v, want the job's own completion record", childSignals)
		}
		if parentSignals := storedSignals(t, store, completionParentID); len(parentSignals) != 0 {
			t.Fatalf("parent signals = %+v, want none after the abandoned settlement", parentSignals)
		}
		f.child.mu.Lock()
		pending := f.child.pendingCompletion
		f.child.mu.Unlock()
		if pending != nil {
			t.Fatalf("the abandoned obligation survived: %+v", pending)
		}
	})
}

// TestStopAllClosesAnIdleGraphlessChildWithoutLoading proves the
// classification rule: an established idle child whose graph was released is
// closed by StopAll through its retained identity and local handles — no graph
// load and no storage read — while the parent's established cell stays.
func TestStopAllClosesAnIdleGraphlessChildWithoutLoading(t *testing.T) {
	store := completionChildGraph(completionParentID, false, OperationSuccess, "", "").storage(t)
	addRootSession(t, store, completionParentID)
	counting := &observationCountingStorage{Storage: store}
	h := newTestHarness(t, counting, nil)
	ctx := context.Background()

	if _, err := h.ReadHistoryView(ctx, completionParentID); err != nil {
		t.Fatalf("parent ReadHistoryView: %v", err)
	}
	if _, err := h.ReadHistoryView(ctx, completionChildID); err != nil {
		t.Fatalf("child ReadHistoryView: %v", err)
	}
	h.mu.Lock()
	child := h.sessions[completionChildID]
	h.mu.Unlock()
	child.mu.Lock()
	graphless := child.graph == nil
	child.mu.Unlock()
	if !graphless {
		t.Fatalf("the idle child's graph stayed resident after its read")
	}

	before := counting.calls
	if err := h.StopAll(ctx); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if counting.calls != before {
		t.Fatalf("StopAll performed %d storage reads, want none: an idle child closes through its local handles", counting.calls-before)
	}
	child.mu.Lock()
	closed := child.bgState == bgClosed
	child.mu.Unlock()
	if !closed {
		t.Fatalf("the idle graphless child was not closed by StopAll")
	}
}

// TestMetadataListingLeavesNoResidentCorpus proves the metadata working set
// under the one rule: a fifteen-Session listing leaves no resident graph, a
// real history use releases the graph it loads, and every cell survives.
func TestMetadataListingLeavesNoResidentCorpus(t *testing.T) {
	h := newTestHarness(t, emptyStore(t), nil)
	ctx := context.Background()

	ids := make([]string, 0, 15)
	for i := 0; i < 15; i++ {
		ids = append(ids, createSession(t, h))
	}
	assertNoResidentGraphs := func(step string) {
		t.Helper()
		h.mu.Lock()
		cells := make([]*coordinator, 0, len(ids))
		for _, id := range ids {
			cells = append(cells, h.sessions[id])
		}
		h.mu.Unlock()
		for i, c := range cells {
			if c == nil {
				t.Fatalf("%s dropped the established cell of %q", step, ids[i])
			}
			c.mu.Lock()
			graph := c.graph
			c.mu.Unlock()
			if graph != nil {
				t.Fatalf("%s left the graph of %q resident", step, ids[i])
			}
		}
	}
	assertNoResidentGraphs("the creation series")

	headers, err := h.ListSessions(ctx)
	if err != nil || len(headers) != len(ids) {
		t.Fatalf("ListSessions = %d rows err %v, want all %d", len(headers), err, len(ids))
	}
	assertNoResidentGraphs("the metadata listing")

	if _, err := h.ReadHistoryView(ctx, ids[0]); err != nil {
		t.Fatalf("ReadHistoryView: %v", err)
	}
	assertNoResidentGraphs("the real history use")
}

// TestFailedFirstLookupLeavesNoCell proves the never-loaded cleanup and the
// sticky corruption latch: a storage failure removes the cell on its final
// failed release, so the next attempt retries storage, while a persisted
// semantic violation latches on the stable cell and later attempts answer
// from the latch without touching storage again.
func TestFailedFirstLookupLeavesNoCell(t *testing.T) {
	ctx := context.Background()

	store := freshSessionStore(t)
	h := newTestHarness(t, store, nil)
	store.registersErr = fmt.Errorf("%w: storage down", ErrStorage)
	if _, err := h.ReadSessionHeader(ctx, testSessionID); !errors.Is(err, ErrStorage) {
		t.Fatalf("failed first lookup = %v, want the storage class", err)
	}
	store.registersErr = nil
	h.mu.Lock()
	cell := h.sessions[testSessionID]
	h.mu.Unlock()
	if cell != nil {
		t.Fatalf("a failed never-loaded lookup left its cell behind")
	}
	if _, err := h.ReadSessionHeader(ctx, testSessionID); err != nil {
		t.Fatalf("retry after the failed lookup: %v", err)
	}

	corrupt := freshSessionStore(t)
	foreignCorruptRegister(t, corrupt, testSessionID)
	counting := &observationCountingStorage{Storage: corrupt}
	latched := newTestHarness(t, counting, nil)
	if _, err := latched.ReadSessionHeader(ctx, testSessionID); !isCorruption(err) {
		t.Fatalf("corrupt first lookup = %v, want the corruption class", err)
	}
	latched.mu.Lock()
	stuck := latched.sessions[testSessionID]
	latched.mu.Unlock()
	if stuck == nil {
		t.Fatalf("the corruption did not latch on the stable cell")
	}
	after := counting.calls
	for i := 0; i < 2; i++ {
		if _, err := latched.ReadSessionHeader(ctx, testSessionID); !isCorruption(err) {
			t.Fatalf("latched corruption read %d = %v, want the corruption class", i, err)
		}
	}
	if counting.calls != after {
		t.Fatalf("the latched corruption re-read storage %d times, want none", counting.calls-after)
	}
}

// TestStopAllDoesNotLoadIdleCorpus proves the shutdown enumeration row: an
// idle established corpus is stopped without a single storage read, every cell
// survives the stop, and no graph is left resident.
func TestStopAllDoesNotLoadIdleCorpus(t *testing.T) {
	counting := &observationCountingStorage{Storage: emptyStore(t)}
	h := newTestHarness(t, counting, nil)
	ctx := context.Background()

	ids := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		ids = append(ids, createSession(t, h))
	}
	before := counting.calls
	if err := h.StopAll(ctx); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if counting.calls != before {
		t.Fatalf("StopAll over an idle corpus performed %d storage reads, want none", counting.calls-before)
	}
	h.mu.Lock()
	cells := make([]*coordinator, 0, len(ids))
	for _, id := range ids {
		cells = append(cells, h.sessions[id])
	}
	h.mu.Unlock()
	for i, c := range cells {
		if c == nil {
			t.Fatalf("StopAll dropped the established cell of %q", ids[i])
		}
		c.mu.Lock()
		graph := c.graph
		c.mu.Unlock()
		if graph != nil {
			t.Fatalf("StopAll left the graph of %q resident", ids[i])
		}
	}
	if _, err := h.ReadSessionHeader(ctx, ids[0]); err != nil {
		t.Fatalf("read after StopAll: %v", err)
	}
}

// TestSweepCandidateScanOverAColdCorpus proves the sweep's candidate path over
// a cold corpus: enumeration loads no graph and creates no cell, each
// candidate's one transition validates exactly that Session, and the scan
// archives every candidate.
func TestSweepCandidateScanOverAColdCorpus(t *testing.T) {
	store := emptyStore(t)
	ids := make([]string, 0, 15)
	for i := 0; i < 15; i++ {
		id := hexID(100 + i)
		addRootSession(t, store, id)
		ids = append(ids, id)
	}
	counting := &observationCountingStorage{Storage: store}
	h := newTestHarness(t, counting, nil)
	ctx := context.Background()

	candidates, err := h.ListSessionCandidates(ctx)
	if err != nil || !reflect.DeepEqual(candidates, ids) {
		t.Fatalf("candidates = %v err %v, want the sorted %v", candidates, err, ids)
	}
	if counting.calls != 0 {
		t.Fatalf("candidate enumeration performed %d storage reads, want none", counting.calls)
	}
	h.mu.Lock()
	cells := len(h.sessions)
	h.mu.Unlock()
	if cells != 0 {
		t.Fatalf("candidate enumeration created %d cells, want none", cells)
	}

	policy := SweepPolicy{ArchiveAfter: time.Hour, DeleteAfterArchive: time.Hour}
	now := time.Now().UTC()
	for _, id := range candidates {
		commit, err := h.SweepSession(ctx, id, policy, now)
		if err != nil || commit {
			t.Fatalf("sweep %q = commit %v err %v, want the archive without a deletion", id, commit, err)
		}
	}
	if want := 2 * len(ids); counting.calls != want {
		t.Fatalf("the candidate scan read storage %d times, want the one register and one entry read per candidate (%d)", counting.calls, want)
	}
	for _, id := range ids {
		header, err := h.ReadSessionHeader(ctx, id)
		if err != nil || header.Lifecycle != LifecycleArchived {
			t.Fatalf("Session %q after its sweep = %+v err %v, want archived", id, header, err)
		}
	}
}

// TestExecutionUsePinsTheGraphThroughSuccessorDrain proves the execution
// retention: an installed run keeps its Session's graph resident, and the
// post-terminal drain's successor continues on that same pinned graph until
// both Operations settle and the Session goes idle, when the final release
// clears it.
func TestExecutionUsePinsTheGraphThroughSuccessorDrain(t *testing.T) {
	store := emptyStore(t)
	first := make(chan struct{})
	second := make(chan struct{})
	secondArrived := make(chan struct{}, 1)
	var attempts atomic.Int32
	modelFn := func(ctx context.Context, _ model.Request) (model.Stream, error) {
		if attempts.Add(1) == 1 {
			select {
			case <-first:
				return completedTurnStream(), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		select {
		case secondArrived <- struct{}{}:
		default:
		}
		select {
		case <-second:
			return completedTurnStream(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	h := newTestHarness(t, store, newPrepareStub(modelPrepared(modelFn)).prepare)

	session := createSession(t, h)
	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "first"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if _, err := submitText(t, h, session, "op-2", MessageModeQueued, "queued"); err != nil {
		t.Fatalf("queued submit: %v", err)
	}
	cell := cachedCoordinator(t, h, session)
	cell.mu.Lock()
	pinned := cell.graph
	cell.mu.Unlock()
	if pinned == nil {
		t.Fatalf("the running Session's graph was not resident")
	}

	close(first) // the first turn settles and its drain admits the queued successor
	select {
	case <-secondArrived: // the successor is parked at its model boundary
	case <-time.After(10 * time.Second):
		t.Fatal("the successor never reached its model boundary")
	}
	cell.mu.Lock()
	successorGraph := cell.graph
	cell.mu.Unlock()
	if successorGraph != pinned {
		t.Fatalf("the successor continued on a different graph than the pinned one")
	}

	close(second)
	awaitHarnessQuiet(t, h, session)
	for _, id := range []string{"op-1", "op-2"} {
		if rec := settledOperation(t, store, session, id); rec.State.Status != OperationSuccess {
			t.Fatalf("operation %q = %+v, want the settled success", id, rec.State)
		}
	}
	cell.mu.Lock()
	stillResident := cell.graph
	cell.mu.Unlock()
	if stillResident != nil {
		t.Fatalf("the Session's graph stayed resident after both Operations settled")
	}
}

// TestChildCompletionRendersThroughAGraphlessChild proves the pending child
// completion's own re-materialization: with the child's released graph gone,
// the settlement trigger loads it for rendering and the parent still receives
// the completion.
func TestChildCompletionRendersThroughAGraphlessChild(t *testing.T) {
	store := completionChildGraph(completionParentID, false, OperationSuccess, "", "child text").storage(t)
	addRootSession(t, store, completionParentID)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
	ctx := context.Background()

	// Warm both Sessions with real reads; each graph is released with its call.
	if _, err := h.ReadHistoryView(ctx, completionParentID); err != nil {
		t.Fatalf("parent ReadHistoryView: %v", err)
	}
	if _, err := h.ReadHistoryView(ctx, completionChildID); err != nil {
		t.Fatalf("child ReadHistoryView: %v", err)
	}
	child := cachedCoordinator(t, h, completionChildID)
	parent := leaseCoordinator(t, h, completionParentID) // the fixture's explicit parent pin
	child.mu.Lock()
	childGraph := child.graph
	child.mu.Unlock()
	if childGraph != nil {
		t.Fatalf("the child's released graph stayed resident")
	}

	member, err := admitLocked(h, parent, memberChild, completionChildID, 0)
	if err != nil {
		t.Fatalf("child member admission: %v", err)
	}
	child.mu.Lock()
	child.pendingCompletion = &launchInfo{completionID: member.completionID, outputLimit: 1024}
	child.mu.Unlock()

	h.childCompletionSettled(child, "")
	if got := runtimeInputTextRaw(t, store, completionParentID); got != "child text" {
		t.Fatalf("parent completion text = %q, want the child's rendered settlement", got)
	}
	parent.mu.Lock()
	remaining := 0
	if parent.group != nil {
		remaining = len(parent.group.members)
	}
	parent.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("the finished child member stayed live: %d members", remaining)
	}
}
