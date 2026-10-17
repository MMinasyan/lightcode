package harness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// TestRetentionBoundsInactiveGraphs proves the inactive-graph bound and the
// survival of every local fact: with more than eight idle Sessions the oldest
// graphs are cleared while their cells, local revisions, groups, and buffers
// stay, an evicted Session remains fully usable through its own
// re-materialization, a metadata archive adoption still commits against the
// evicted cell, and a live member's completion still delivers under pressure.
func TestRetentionBoundsInactiveGraphs(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, newPrepareStub(modelPrepared(quickModel())).prepare)
	h.deps.Jobs = &gatedJobStopper{}
	ctx := context.Background()

	// The idle subject is created first, so the pressure below evicts it.
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
	// and one buffered steering item beside it.
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
	bufferedLease()

	// Eight more idle Sessions fill the inactive cache and push the idle
	// graphs out; the live member's own Session stays pinned and out of the
	// inactive set.
	newest := make([]string, 0, inactiveGraphLimit)
	for i := 0; i < inactiveGraphLimit; i++ {
		newest = append(newest, createSession(t, h))
	}
	if got := cachedGraphIDs(h); !reflect.DeepEqual(got, newest) {
		t.Fatalf("inactive cache = %v, want the eight newest identities %v", got, newest)
	}
	for _, evicted := range []string{idle, buffered} {
		c := cachedCoordinator(t, h, evicted)
		h.mu.Lock()
		cached := c.cached
		h.mu.Unlock()
		if cached {
			t.Fatalf("the oldest inactive graph of %q was not evicted", evicted)
		}
	}
	h.mu.Lock()
	subjectCached, subjectNode := subjectC.cached, subjectC.lruNode
	h.mu.Unlock()
	if !subjectCached || subjectNode != nil {
		t.Fatalf("the live member's Session = cached %v node %v, want a pinned graph outside the inactive set", subjectCached, subjectNode)
	}

	// The cells are the same objects and keep every local fact.
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
			t.Fatalf("the evicted cell of %q was replaced", row.id)
		}
		row.cell.mu.Lock()
		revision, members, steering := row.cell.localRev, 0, len(row.cell.steering)
		if row.cell.group != nil {
			members = len(row.cell.group.members)
		}
		row.cell.mu.Unlock()
		if revision != row.revision {
			t.Fatalf("local revision of %q = %d, want the retained %d", row.id, revision, row.revision)
		}
		if members != row.members || steering != row.steering {
			t.Fatalf("local facts of %q = %d members, %d steering items, want %d and %d",
				row.id, members, steering, row.members, row.steering)
		}
	}

	// An evicted Session stays usable: a real history use re-materializes and
	// caches its graph, and a metadata archive adoption commits against the
	// same cell.
	if _, err := h.ReadHistoryView(ctx, idle); err != nil {
		t.Fatalf("ReadHistoryView of the evicted Session: %v", err)
	}
	archived, err := h.ArchiveSession(ctx, idle)
	if err != nil || archived.State.Lifecycle != LifecycleArchived {
		t.Fatalf("ArchiveSession of the evicted Session = %+v err %v, want the committed archive", archived, err)
	}

	// The live member's completion still delivers, and its finish ends the pin:
	// the Session's graph becomes the newest inactive cache entry.
	if err := h.DeliverBackgroundCompletion(ctx, subject, completionID, "job done"); err != nil {
		t.Fatalf("DeliverBackgroundCompletion under pressure: %v", err)
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
	if got := cachedGraphIDs(h); got[len(got)-1] != subject {
		deadline := time.Now().Add(10 * time.Second) // the delivered execution's own pin ends asynchronously
		for {
			got = cachedGraphIDs(h)
			if len(got) > 0 && got[len(got)-1] == subject {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("inactive cache after the member finish = %v, want %q as the newest entry", got, subject)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// TestLatchedGraphLeavesTheInactiveCache proves the availability rule at the
// cache boundary: a warm Session whose cell latches corruption through a real
// Fork discovery leaves the inactive cache on its genuine release — its cell,
// its latch, and its local state stay — and the unavailable graph cannot evict
// a valid sibling's cached history.
func TestLatchedGraphLeavesTheInactiveCache(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
	ctx := context.Background()

	source := createSession(t, h) // cached first, so it is the oldest entry
	older := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		older = append(older, createSession(t, h))
	}
	sourceCell, sourceLease := leaseCoordinatorNow(t, h, source) // the interval whose genuine release the test controls
	h.mu.Lock()
	cachedBefore := sourceCell.cached
	h.mu.Unlock()
	if !cachedBefore {
		t.Fatalf("the source's graph was not cached before the discovery")
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

	// One more idle Session fills the cache while the latched cell's node is
	// gone, and the latched interval's final genuine release then runs.
	createSession(t, h)
	sourceLease()

	h.mu.Lock()
	cachedAfter, node, registered := sourceCell.cached, sourceCell.lruNode, h.sessions[source] == sourceCell
	olderCached := false
	if c := h.sessions[older[0]]; c != nil {
		olderCached = c.cached && c.lruNode != nil
	}
	h.mu.Unlock()
	if !registered || sourceCell.corru == nil {
		t.Fatalf("the latched cell did not survive its release (registered %v)", registered)
	}
	if !olderCached {
		t.Fatalf("the unavailable graph evicted the oldest valid cached history %q", older[0])
	}
	if got := cachedGraphIDs(h); len(got) != inactiveGraphLimit {
		t.Fatalf("inactive cache = %v, want the bound %d", got, inactiveGraphLimit)
	}
	if cachedAfter || node != nil {
		t.Fatalf("the latched graph stayed cached after its release (cached %v node %v)", cachedAfter, node)
	}
	if _, err := h.ReadSessionHeader(ctx, source); !isCorruption(err) {
		t.Fatalf("the latched Session read = %v, want the sticky corruption class", err)
	}

	// A metadata borrow of a latched cached graph leaves it out of the cache
	// too: the latch lands through the metadata transition itself, so the
	// borrow's own release must not preserve the unavailable graph in place.
	borrowStore := emptyStore(t)
	borrowHarness := newTestHarness(t, borrowStore, nil)
	borrowedSource := createSession(t, borrowHarness)
	if _, err := borrowHarness.ReadHistoryView(ctx, borrowedSource); err != nil { // cached with its node in place
		t.Fatalf("ReadHistoryView: %v", err)
	}
	foreignCorruptRegister(t, borrowStore, borrowedSource)
	if _, err := borrowHarness.ArchiveSession(ctx, borrowedSource); !isCorruption(err) {
		t.Fatalf("metadata transition over the corrupt register = %v, want the typed corruption", err)
	}
	borrowHarness.mu.Lock()
	borrowedCell := borrowHarness.sessions[borrowedSource]
	latchedBorrow, cachedBorrow, nodeBorrow := borrowedCell.corru, borrowedCell.cached, borrowedCell.lruNode
	borrowHarness.mu.Unlock()
	if borrowedCell == nil || latchedBorrow == nil {
		t.Fatalf("the metadata transition did not latch the borrowed cell")
	}
	if cachedBorrow || nodeBorrow != nil {
		t.Fatalf("the metadata borrow kept the latched graph cached (cached %v node %v)", cachedBorrow, nodeBorrow)
	}
	if got := cachedGraphIDs(borrowHarness); len(got) != 0 {
		t.Fatalf("inactive cache = %v, want the latched graph out of it", got)
	}
}

// TestBorrowedCacheEvictsTheJustReleasedGraph proves the bound's fallback
// victim: with all eight older cached entries borrowed, a ninth Session's
// genuine release makes its own graph the eviction victim while every borrowed
// graph survives.
func TestBorrowedCacheEvictsTheJustReleasedGraph(t *testing.T) {
	h := newTestHarness(t, emptyStore(t), nil)

	borrowed := make([]string, 0, inactiveGraphLimit)
	releases := make([]func(), 0, inactiveGraphLimit)
	for i := 0; i < inactiveGraphLimit; i++ {
		id := createSession(t, h)
		_, release := leaseCoordinatorNow(t, h, id) // a metadata borrow of every cached graph
		borrowed = append(borrowed, id)
		releases = append(releases, release)
	}
	if got := cachedGraphIDs(h); !reflect.DeepEqual(got, borrowed) {
		t.Fatalf("inactive cache = %v, want all eight borrowed entries %v", got, borrowed)
	}

	ninth := createSession(t, h) // the genuine release of a ninth graph
	h.mu.Lock()
	ninthCell := h.sessions[ninth]
	victim := ninthCell != nil && !ninthCell.cached && ninthCell.lruNode == nil
	h.mu.Unlock()
	if !victim {
		t.Fatalf("the just-released ninth graph was not the eviction victim")
	}
	for _, id := range borrowed {
		h.mu.Lock()
		c := h.sessions[id]
		survived := c != nil && c.cached && c.lruNode != nil
		h.mu.Unlock()
		if !survived {
			t.Fatalf("the borrowed cached graph of %q was evicted", id)
		}
	}
	if got := cachedGraphIDs(h); !reflect.DeepEqual(got, borrowed) {
		t.Fatalf("inactive cache after the fallback eviction = %v, want the borrowed %v", got, borrowed)
	}
	for _, release := range releases {
		release()
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

// TestStopAllClosesAnIdleEvictedChildWithoutLoading proves the classification
// rule: an established idle child whose graph cache pressure evicted is closed
// by StopAll through its retained identity and local handles — no graph load
// and no storage read — while the parent's established cell stays.
func TestStopAllClosesAnIdleEvictedChildWithoutLoading(t *testing.T) {
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
	for i := 0; i < inactiveGraphLimit; i++ {
		createSession(t, h)
	}
	h.mu.Lock()
	child := h.sessions[completionChildID]
	evicted := child != nil && !child.cached
	h.mu.Unlock()
	if !evicted {
		t.Fatalf("the idle child's graph was not evicted under pressure")
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
		t.Fatalf("the idle evicted child was not closed by StopAll")
	}
}

// TestMetadataListingDoesNotRetainOrPromote proves the metadata working set: a
// fifteen-Session listing neither retains nor promotes the corpus, a real
// history use caches its graph as the newest inactive entry, and a metadata
// borrow of that cached graph keeps its position.
func TestMetadataListingDoesNotRetainOrPromote(t *testing.T) {
	h := newTestHarness(t, emptyStore(t), nil)
	ctx := context.Background()

	ids := make([]string, 0, 15)
	for i := 0; i < 15; i++ {
		ids = append(ids, createSession(t, h))
	}
	before := cachedGraphIDs(h)
	if len(before) != inactiveGraphLimit {
		t.Fatalf("inactive cache = %d entries, want the bound %d", len(before), inactiveGraphLimit)
	}
	if !reflect.DeepEqual(before, ids[7:]) {
		t.Fatalf("inactive cache = %v, want the newest eight identities %v", before, ids[7:])
	}

	headers, err := h.ListSessions(ctx)
	if err != nil || len(headers) != len(ids) {
		t.Fatalf("ListSessions = %d rows err %v, want all %d", len(headers), err, len(ids))
	}
	if got := cachedGraphIDs(h); !reflect.DeepEqual(got, before) {
		t.Fatalf("listing changed the cached set: %v -> %v", before, got)
	}
	h.mu.Lock()
	for _, id := range ids[:7] {
		if c := h.sessions[id]; c != nil && c.cached {
			h.mu.Unlock()
			t.Fatalf("listing retained the graph of the uncached Session %q", id)
		}
	}
	h.mu.Unlock()

	// A real history use caches its graph as the newest entry, evicting the
	// oldest one, and a metadata borrow afterwards keeps that position.
	oldest := ids[0]
	if _, err := h.ReadHistoryView(ctx, oldest); err != nil {
		t.Fatalf("ReadHistoryView: %v", err)
	}
	want := append(append([]string{}, before[1:]...), oldest)
	if got := cachedGraphIDs(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the real history use the cache = %v, want %v", got, want)
	}
	if _, err := h.ReadSessionHeader(ctx, oldest); err != nil {
		t.Fatalf("ReadSessionHeader: %v", err)
	}
	if got := cachedGraphIDs(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("a metadata borrow moved the cached set: %v -> %v", want, got)
	}
}

// TestOverlappingHistoryRetainsAMetadataLoad proves the shared-interval rule
// on cells that start without a graph: a graph a metadata use loaded is kept
// when a real history use overlapped that interval, while a metadata-only
// interval discards the load it made itself.
func TestOverlappingHistoryRetainsAMetadataLoad(t *testing.T) {
	store := freshSessionStore(t)
	aloneID := hexID(9)
	addRootSession(t, store, aloneID)
	h := newTestHarness(t, store, nil)
	ctx := context.Background()

	metaCell, metaRelease := leaseCoordinatorNow(t, h, testSessionID)
	if _, err := h.ReadHistoryView(ctx, testSessionID); err != nil { // the overlapping real use
		t.Fatalf("ReadHistoryView: %v", err)
	}
	metaRelease() // the metadata interval releases last
	h.mu.Lock()
	kept := metaCell.cached
	h.mu.Unlock()
	if !kept {
		t.Fatalf("an overlapping real history use did not keep the metadata load")
	}

	aloneCell, aloneRelease := leaseCoordinatorNow(t, h, aloneID)
	aloneRelease()
	h.mu.Lock()
	discarded := !aloneCell.cached
	h.mu.Unlock()
	if !discarded {
		t.Fatalf("a metadata-only interval retained the graph it loaded")
	}
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
// idle established corpus is stopped without a single storage read, and every
// cell survives the stop.
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
	for _, id := range ids {
		if h.sessions[id] == nil {
			h.mu.Unlock()
			t.Fatalf("StopAll dropped the established cell of %q", id)
		}
	}
	h.mu.Unlock()
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

// TestExecutionPinsSurvivePressureAndSuccessorDrain proves the execution
// retention under pressure: an installed run keeps its Session's graph out of
// the inactive set, cache pressure cannot evict it, and the post-terminal
// drain's successor continues on the same pinned graph until both Operations
// settle and the Session goes idle.
func TestExecutionPinsSurvivePressureAndSuccessorDrain(t *testing.T) {
	store := emptyStore(t)
	release := make(chan struct{})
	h := newTestHarness(t, store, newPrepareStub(modelPrepared(parkedModel(release))).prepare)

	session := createSession(t, h)
	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "first"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if _, err := submitText(t, h, session, "op-2", MessageModeQueued, "queued"); err != nil {
		t.Fatalf("queued submit: %v", err)
	}
	cell := cachedCoordinator(t, h, session)

	for i := 0; i < inactiveGraphLimit; i++ {
		createSession(t, h)
	}
	h.mu.Lock()
	pinned, node := cell.cached, cell.lruNode
	h.mu.Unlock()
	if !pinned || node != nil {
		t.Fatalf("the running Session = cached %v node %v, want a pinned graph outside the inactive set", pinned, node)
	}

	close(release) // the first turn settles and its drain admits the queued successor
	awaitHarnessQuiet(t, h, session)
	for _, id := range []string{"op-1", "op-2"} {
		if rec := settledOperation(t, store, session, id); rec.State.Status != OperationSuccess {
			t.Fatalf("operation %q = %+v, want the settled success", id, rec.State)
		}
	}
	h.mu.Lock()
	stillCached := cell.cached
	h.mu.Unlock()
	if !stillCached {
		t.Fatalf("the Session's graph was evicted although its execution interval was cache-eligible")
	}
	if got := cachedGraphIDs(h); got[len(got)-1] != session {
		t.Fatalf("inactive cache after the drain = %v, want %q as the newest entry", got, session)
	}
}

// TestChildCompletionRendersThroughAnEvictedGraph proves the pending child
// completion's own retention: with the child's idle graph evicted by cache
// pressure, the settlement trigger re-materializes it for rendering and the
// parent still receives the completion.
func TestChildCompletionRendersThroughAnEvictedGraph(t *testing.T) {
	store := completionChildGraph(completionParentID, false, OperationSuccess, "", "child text").storage(t)
	addRootSession(t, store, completionParentID)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
	ctx := context.Background()

	// Warm both Sessions with real reads, so the pressure below evicts idle
	// graphs only.
	if _, err := h.ReadHistoryView(ctx, completionParentID); err != nil {
		t.Fatalf("parent ReadHistoryView: %v", err)
	}
	if _, err := h.ReadHistoryView(ctx, completionChildID); err != nil {
		t.Fatalf("child ReadHistoryView: %v", err)
	}
	child := cachedCoordinator(t, h, completionChildID)
	parent := cachedCoordinator(t, h, completionParentID)
	member, err := admitLocked(h, parent, memberChild, completionChildID, 0)
	if err != nil {
		t.Fatalf("child member admission: %v", err)
	}
	child.mu.Lock()
	child.pendingCompletion = &launchInfo{completionID: member.completionID, outputLimit: 1024}
	child.mu.Unlock()

	for i := 0; i < inactiveGraphLimit; i++ {
		createSession(t, h)
	}
	h.mu.Lock()
	evicted := !child.cached
	h.mu.Unlock()
	if !evicted {
		t.Fatalf("the idle child graph was not evicted under pressure")
	}

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
