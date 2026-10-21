package runtime

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// The passive event bus rows: the current-pair sample inside the publication
// section, root/child scope tagging, the two-healthy/one-saturated order with
// no replay, the job member's job-scoped hints against internal transitions,
// and the factory-bound plugin warning ingress.

// TestObservationPausedFactEmitsCurrentRevision proves a notification delayed
// behind a paused publication carries the Session's CURRENT revision pair,
// never the producer's stale committed pair: an archive and a reopen commit
// while the section is paused, and both queued invalidations then publish the
// final pair — the first one's own committed pair was strictly older.
func TestObservationPausedFactEmitsCurrentRevision(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		e := newOwnerEnv(t)
		r, err := e.open(ctx, e.storagePlugin(store))
		testassert.NoError(t, err)
		defer func() {
			{
				err := r.Close(context.Background())
				testassert.Check(t, err == nil, "Close: %v", err)
			}
		}()
		sub, err := r.Subscribe(64)
		testassert.NoError(t, err)
		session, err := r.createSession(ctx, e.dataDir, "solo")
		testassert.NoError(t, err)
		sessionID := session.Identity.SessionID

		// The creation's own invalidation is consumed first: everything after
		// it belongs to the paused sequence.
		creation, ok := nextEvent(t, sub)
		if !ok || eventKind(t, creation) != "session_changed" {
			t.Fatalf("creation event = %s (ok=%v), want the session's first invalidation", eventJSON(t, creation), ok)
		}

		_, release := pauseObservation(t, r.obs, scopeEvent(protocol.ScopeOpened, ScopeInfo{Kind: ScopeWorkspace, Workspace: "/ws-paused-synthetic"}))
		archiveDone := make(chan error, 1)
		go func() {
			archiveDone <- r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ArchiveSession(ctx, sessionID)
				return err
			})
		}()
		// The durable commits are public while their invalidation
		// publications wait behind the paused section.
		awaitLifecycle := func(want harness.SessionLifecycle) {
			t.Helper()
			deadline := time.Now().Add(10 * time.Second)
			for sessionViewThroughRuntime(t, r, sessionID).Header.Lifecycle != want {
				testassert.Require(t, !time.Now().After(deadline), "the session never reached lifecycle %q while its publication was paused", want)
				time.Sleep(5 * time.Millisecond)
			}
		}
		awaitLifecycle(harness.LifecycleArchived)
		reopenDone := make(chan error, 1)
		go func() {
			reopenDone <- r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ReopenSession(ctx, sessionID)
				return err
			})
		}()
		awaitLifecycle(harness.LifecycleOpen)
		release()
		testassert.NoError(t, <-archiveDone)
		testassert.NoError(t, <-reopenDone)

		pausedEvent, ok := nextEvent(t, sub)
		if !ok || !equalEvent(pausedEvent, scopeEvent(protocol.ScopeOpened, ScopeInfo{Kind: ScopeWorkspace, Workspace: "/ws-paused-synthetic"})) {
			t.Fatalf("first event after release = %s (ok=%v), want the paused publication's own event", eventJSON(t, pausedEvent), ok)
		}
		// Both queued invalidations — the archive's own committed pair was
		// the intermediate revision — publish the CURRENT final pair.
		final := snapshotPairOf(t, r, sessionID)
		for range 2 {
			event, ok := nextEvent(t, sub)
			if !ok || eventKind(t, event) != "session_changed" {
				t.Fatalf("queued invalidation = %s (ok=%v), want the session invalidation", eventJSON(t, event), ok)
			}
			body, err := event.AsSessionChangedEvent()
			testassert.NoError(t, err)
			testassert.Equal(t, body.SessionRevision, final)
			invalidation, err := body.Scope.AsSessionScope()
			testassert.NoError(t, err)
			testassert.Equal(t, invalidation.SessionId, sessionID)
		}
		assertNoEvent(t, sub)
	})
}

// TestObservationRootChildScopeTagging proves the root and the child Session
// events carry their own identity with the shared Workspace, and every
// progress event names the real running Operation.
func TestObservationRootChildScopeTagging(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		bg := openBackgroundRuntime(t, store, newLifecycleStopper())
		defer func() {
			if err := bg.r.Close(context.Background()); err != nil {
				bg.t.Errorf("Close: %v", err)
			}
		}()
		sub, err := bg.r.Subscribe(256)
		testassert.NoError(t, err)
		root := bg.session("root-ws")
		child := bg.launchChild(root, "child work", "child-op-1", 1)
		bg.submitMode(root, "root-op-1", "hello", harness.MessageModeRegular)
		{
			rec := awaitOperation(t, bg.r, root, "root-op-1", harness.OperationSuccess)
			testassert.Equal(t, rec.State.Status, harness.OperationSuccess)
		}
		awaitOperation(t, bg.r, child, "child-op-1", harness.OperationSuccess)
		awaitRestorableSession(t, bg.r, root)

		workspace := sessionViewThroughRuntime(t, bg.r, root).Header.Identity.Workspace
		rootInvalidations := 0
		childInvalidations := 0
		childProgress := 0
		rootProgress := 0
		for {
			select {
			case event, ok := <-sub.Events():
				testassert.Require(t, ok, "the subscription closed before the drain finished")
				switch eventKind(t, event) {
				case "session_changed":
					body, err := event.AsSessionChangedEvent()
					testassert.NoError(t, err)
					invalidationSession, _ := scopeSessionIdentity(t, body.Scope)
					switch invalidationSession {
					case root:
						rootInvalidations++
					case child:
						childInvalidations++
					default:
						t.Fatalf("invalidation for a foreign session %+v", body.Scope)
					}
					testassert.Require(t, scopeKind(t, body.Scope) == "session", "invalidation scope = %+v, want the session granularity", body.Scope)
					invalidation, err := body.Scope.AsSessionScope()
					testassert.NoError(t, err)
					testassert.Require(t, deref(invalidation.Workspace) == workspace, "invalidation scope = %+v, want the shared workspace", body.Scope)
				case "text_delta":
					body, err := event.AsTextDeltaEvent()
					testassert.NoError(t, err)
					progress, err := body.Scope.AsOperationScope()
					testassert.NoError(t, err)
					if progress.SessionId == child {
						testassert.Require(t, progress.OperationId == "child-op-1", "child progress scope = %+v, want the child's real operation", body.Scope)
						childProgress++
						continue
					}
					// The root session's deltas include the child-completion
					// delivery's own follow-up turn: its operation is real
					// but caller-generated, so only the session identity is
					// pinned here.
					testassert.Require(t, progress.SessionId == root && progress.OperationId != "", "progress scope = %+v, want the root session's real operation", body.Scope)
					rootProgress++
				case "tool_started", "tool_finished":
					t.Fatalf("unexpected tool progress without a tool call: %s", eventJSON(t, event))
				case "scope_opened", "scope_closed":
					body, err := event.AsScopeEvent()
					testassert.NoError(t, err)
					if kind := scopeKind(t, body.Scope); kind == "operation" || kind == "agent" {
						shortSession, _ := scopeSessionIdentity(t, body.Scope)
						testassert.Require(t, shortSession == root || shortSession == child, "short scope event for a foreign session: %+v", body.Scope)
					}
				default:
					// configuration and warning events carry no session attribution.
				}
				continue
			default:
			}
			break
		}
		testassert.Require(t, rootInvalidations != 0 && childInvalidations != 0, "root/child invalidations = %d/%d, want real committed hints for both sessions", rootInvalidations, childInvalidations)
		testassert.Require(t, rootProgress != 0 && childProgress != 0, "root/child progress = %d/%d, want the real progress truth for both sessions", rootProgress, childProgress)
	})
}

// TestObservationTwoHealthyOrderOneSaturatedNoReplay proves two healthy
// subscribers observe one identical committed event order, a saturated
// subscriber is removed alone, and a subscriber arriving after committed work
// receives no replay of the past.
func TestObservationTwoHealthyOrderOneSaturatedNoReplay(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		bg := openBackgroundRuntime(t, store, newLifecycleStopper())
		defer func() {
			if err := bg.r.Close(context.Background()); err != nil {
				bg.t.Errorf("Close: %v", err)
			}
		}()
		first, err := bg.r.Subscribe(256)
		testassert.NoError(t, err)
		second, err := bg.r.Subscribe(256)
		testassert.NoError(t, err)
		saturated, err := bg.r.Subscribe(1)
		testassert.NoError(t, err)

		session := bg.session("order-ws")
		bg.submitMode(session, "op-1", "hello", harness.MessageModeRegular)
		awaitOperation(t, bg.r, session, "op-1", harness.OperationSuccess)
		awaitRestorableSession(t, bg.r, session)
		{
			_, err := bg.r.Reload(ctx)
			testassert.NoError(t, err)
		}

		// The saturated subscriber is removed on its second publication.
		{
			_, ok := nextEvent(t, saturated)
			testassert.Require(t, ok, "the saturated subscriber never received its first buffered event")
		}
		{
			_, ok := nextEvent(t, saturated)
			testassert.Require(t, !ok, "the saturated subscriber stayed open past its capacity, want it removed and closed")
		}
		saturated.Close()

		// A late subscriber receives no replay of the past work.
		late, err := bg.r.Subscribe(64)
		testassert.NoError(t, err)
		assertNoEvent(t, late)
		late.Close()

		// New work reaches both healthy subscribers in one order; the two
		// healthy sequences are byte-identical.
		bg.submitMode(session, "op-2", "again", harness.MessageModeRegular)
		awaitOperation(t, bg.r, session, "op-2", harness.OperationSuccess)
		awaitRestorableSession(t, bg.r, session)

		drain := func(sub *Subscription) []Event {
			var out []Event
			for {
				select {
				case event, ok := <-sub.Events():
					if !ok {
						return out
					}
					out = append(out, event)
					continue
				default:
				}
				return out
			}
		}
		firstSeq := drain(first)
		secondSeq := drain(second)
		testassert.Require(t, len(firstSeq) != 0 && slices.EqualFunc(firstSeq, secondSeq, equalEvent), "the two healthy subscribers observed different orders: %d events vs %d", len(firstSeq), len(secondSeq))
	})
}

// TestObservationJobMemberCurrentPairAndInternalSilence proves the job
// member's admission publishes exactly one job-scoped session_changed with
// the current revision pair, a parked internal start (the spawn held while
// membership is unchanged) publishes nothing further and advances no
// revision, and the member's finish publishes the job-scoped hint with the
// new current pair after the completion delivery's real session hints.
func TestObservationJobMemberCurrentPairAndInternalSilence(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		bg := openBackgroundRuntime(t, store, newLifecycleStopper())
		defer func() {
			if err := bg.r.Close(context.Background()); err != nil {
				bg.t.Errorf("Close: %v", err)
			}
		}()
		sub, err := bg.r.Subscribe(256)
		testassert.NoError(t, err)
		session := bg.session("job-ws")
		workspace := sessionViewThroughRuntime(t, bg.r, session).Header.Identity.Workspace

		// One live job member whose spawn parks after signaling: the member
		// is admitted (the job-scoped hint) while the internal start — the
		// spawn — is held.
		spawnArrived := make(chan struct{})
		spawnRelease := make(chan struct{})
		var completionMu sync.Mutex
		completionID := ""
		var spawnOnce sync.Once
		releaseSpawn := func() { spawnOnce.Do(func() { close(spawnRelease) }) }
		// Failure-safe convergence: release the parked internal start and
		// finish the member when the explicit delivery has not run, so the
		// deferred Close always joins the group. On the success path the
		// member is already finished and the delivery no-ops.
		defer func() {
			releaseSpawn()
			completionMu.Lock()
			id := completionID
			completionMu.Unlock()
			if id == "" {
				return
			}
			if err := bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				return h.DeliverBackgroundCompletion(ctx, session, id, "job output")
			}); err != nil {
				bg.t.Errorf("cleanup delivery: %v", err)
			}
		}()
		go func() {
			err := startJobThrough(bg.r, session, "11111111", func(_ context.Context, id string) error {
				completionMu.Lock()
				completionID = id
				completionMu.Unlock()
				close(spawnArrived)
				<-spawnRelease
				return nil
			})
			if err != nil {
				bg.t.Errorf("StartJob: %v", err)
			}
		}()
		select {
		case <-spawnArrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the job's internal start never reached its barrier")
		}

		// The session creation's own invalidation precedes the job
		// admission: session granularity, no Job identity.
		creation, ok := nextEvent(t, sub)
		if !ok || eventKind(t, creation) != "session_changed" {
			t.Fatalf("creation event = %s (ok=%v), want the session's first invalidation", eventJSON(t, creation), ok)
		}
		creationBody, err := creation.AsSessionChangedEvent()
		testassert.NoError(t, err)
		testassert.Require(t, scopeKind(t, creationBody.Scope) == "session", "creation event scope = %+v, want the plain session granularity", creationBody.Scope)
		creationScope, err := creationBody.Scope.AsSessionScope()
		testassert.NoError(t, err)
		testassert.Require(t, creationScope.JobId == nil, "creation event scope = %+v, want the plain session granularity", creationBody.Scope)

		// The admission's job-scoped hint with the CURRENT pair: after the
		// admission nothing else runs on the Session, so the pair equals the
		// independent snapshot taken after the hint arrived.
		hint, ok := nextEvent(t, sub)
		if !ok || eventKind(t, hint) != "session_changed" {
			t.Fatalf("job admission event = %s (ok=%v), want the job-scoped invalidation", eventJSON(t, hint), ok)
		}
		body, err := hint.AsSessionChangedEvent()
		testassert.NoError(t, err)
		testassert.Require(t, scopeKind(t, body.Scope) == "job", "job admission scope = %+v, want the job scope with the owning session", body.Scope)
		jobScope, err := body.Scope.AsJobScope()
		testassert.NoError(t, err)
		testassert.Require(t, jobScope.JobId == "11111111" && jobScope.SessionId == session && deref(jobScope.Workspace) == workspace, "job admission scope = %+v, want the job scope with the owning session", body.Scope)
		{
			admitPair := snapshotPairOf(t, bg.r, session)
			testassert.Equal(t, body.SessionRevision, admitPair)
		}

		// The parked internal start: membership unchanged, no duplicate
		// revisioned hint, no revision advance.
		assertNoEvent(t, sub)
		testassert.Equal(t, snapshotPairOf(t, bg.r, session), body.SessionRevision)

		// Release the internal start and deliver the completion: the real
		// session-scoped hints of the delivery's commits arrive, then the
		// member finish's job-scoped hint with the new current pair.
		releaseSpawn()
		completionMu.Lock()
		deliverID := completionID
		completionMu.Unlock()
		testassert.NoError(t, bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			return h.DeliverBackgroundCompletion(ctx, session, deliverID, "job output")
		}))
		deadline := time.Now().Add(10 * time.Second)
		finishPair := protocol.SessionRevision{}
		for {
			event, ok := nextEvent(t, sub)
			testassert.Require(t, ok, "the subscription closed before the job finish hint")
			if eventKind(t, event) != "session_changed" {
				continue
			}
			body, err := event.AsSessionChangedEvent()
			testassert.NoError(t, err)
			if scopeKind(t, body.Scope) == "job" {
				finishHint, err := body.Scope.AsJobScope()
				testassert.NoError(t, err)
				if finishHint.JobId == "11111111" {
					finishPair = body.SessionRevision
					break
				}
			}
			testassert.Require(t, scopeKind(t, body.Scope) == "session", "completion-delivery hint scope = %+v, want the session granularity", body.Scope)
			testassert.Require(t, !time.Now().After(deadline), "the job finish hint never arrived")
		}
		// The completion's own turn may still be settling when the hint is
		// observed, so the finish pair is pinned to the two invariants that
		// hold deterministically: it advanced past the admission pair, and
		// it never lies beyond the Session's current state.
		afterPair := snapshotPairOf(t, bg.r, session)
		testassert.Require(t, lessPair(t, finishPair, afterPair) || finishPair == afterPair, "job finish pair = %+v, beyond the Session's current state %+v", finishPair, afterPair)
		testassert.Require(t, !lessPair(t, finishPair, body.SessionRevision), "job finish pair = %+v, not advanced past the admission pair %+v", finishPair, body.SessionRevision)
	})
}

// TestObservationPluginReportsLandWithRealAttribution proves the composition
// binds each Runtime-scoped factory's copied ScopeInfo to its own registered
// plugin ID: two factories in one scope report distinct real sources, each
// report lands only in its own global plugin group with the runtime-scoped
// warning_changed event, identical reports dedupe without a second event,
// every Session's hydration and the unfiltered read carry both groups, and
// reports after the warning store's closure are ignored.
func TestObservationPluginReportsLandWithRealAttribution(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		e := newOwnerEnv(t)
		reports := make(map[string]func(kind, message string))
		reporterPlugin := func(id string) Plugin {
			return Plugin{
				ID:       id,
				Scope:    ScopeRuntime,
				Provides: []CapabilitySpec{Spec[any]("reporter." + id)},
				Open: func(_ context.Context, info ScopeInfo, _ Bindings) (Instance, error) {
					reports[id] = info.ReportWarning
					e.events.add("open:" + id)
					return Instance{Values: map[string]any{"reporter." + id: id}}, nil
				},
			}
		}
		r, err := e.open(ctx, e.storagePlugin(store), reporterPlugin("alpha"), reporterPlugin("beta"))
		testassert.NoError(t, err)
		defer func() {
			{
				err := r.Close(context.Background())
				testassert.Check(t, err == nil, "Close: %v", err)
			}
		}()
		testassert.Require(t, reports["alpha"] != nil && reports["beta"] != nil, "the Runtime scope carried no per-plugin ReportWarning closure")
		session, err := r.createSession(ctx, e.dataDir, "solo")
		testassert.NoError(t, err)
		other, err := r.createSession(ctx, e.dataDir, "solo")
		testassert.NoError(t, err)
		sub, err := r.Subscribe(64)
		testassert.NoError(t, err)

		// One report: the reporting plugin's own global group gains the entry
		// with the runtime-scoped warning event. The initial configuration
		// publication may already have advanced the store, so the expected
		// revisions are relative to the store's current value.
		baseRevision, _ := r.warnings.snapshot()
		reports["alpha"]("install_failed", "alpha must be installed")
		event, ok := nextEvent(t, sub)
		if !ok || eventKind(t, event) != "warning_changed" {
			t.Fatalf("report event = %s (ok=%v), want the warning_changed hint", eventJSON(t, event), ok)
		}
		body, err := event.AsWarningChangedEvent()
		testassert.NoError(t, err)
		if scopeKind(t, body.Scope) != "runtime" {
			t.Fatalf("report event scope kind = %q, want the runtime scope", scopeKind(t, body.Scope))
		}
		testassert.Require(t, body.WarningsRevision.Revision == strconv.FormatUint(baseRevision+1, 10), "report event revision = %q, want the first advance", body.WarningsRevision.Revision)
		assertNoEvent(t, sub)

		// The second factory's report is attributed to its own registration
		// ID: the two closures never share one source.
		reports["beta"]("server_unavailable", "beta is unavailable")
		event, ok = nextEvent(t, sub)
		if !ok || eventKind(t, event) != "warning_changed" {
			t.Fatalf("second report event = %s (ok=%v), want the warning_changed hint", eventJSON(t, event), ok)
		}
		body, err = event.AsWarningChangedEvent()
		testassert.NoError(t, err)
		testassert.Require(t, body.WarningsRevision.Revision == strconv.FormatUint(baseRevision+2, 10), "second report event revision = %q, want the second advance", body.WarningsRevision.Revision)

		// An identical report dedupes: no second entry, no second event.
		reports["alpha"]("install_failed", "alpha must be installed")
		assertNoEvent(t, sub)

		for _, sessionID := range []string{session.Identity.SessionID, other.Identity.SessionID} {
			hydration, err := r.buildHydration(ctx, sessionID)
			testassert.Require(t, err == nil, "buildHydration(%s): %v", sessionID, err)
			testassert.Require(t, warningOf(hydration.Warnings, "plugin:alpha", "install_failed", "") != nil && warningOf(hydration.Warnings, "plugin:beta", "server_unavailable", "") != nil, "hydration of %s misses a plugin group: %+v", sessionID, hydration.Warnings)
		}
		all, err := r.getWarnings(ctx)
		testassert.NoError(t, err)
		testassert.Require(t, warningOf(all.Warnings, "plugin:alpha", "install_failed", "") != nil && warningOf(all.Warnings, "plugin:beta", "server_unavailable", "") != nil, "the unfiltered read misses a plugin group: %+v", all.Warnings)
		counts := map[string]int{}
		for _, warning := range all.Warnings {
			counts[string(warning.Source)]++
		}
		testassert.Require(t, counts["plugin:alpha"] == 1 && counts["plugin:beta"] == 1, "plugin entries after the identical re-report = %v, want one deduped value per factory", counts)

		// Reports after the warning store's closure are ignored.
		revision, _ := r.warnings.snapshot()
		r.warnings.close()
		reports["alpha"]("install_failed", "a late report")
		assertNoEvent(t, sub)
		{
			after, _ := r.warnings.snapshot()
			testassert.Equal(t, after, revision)
		}
	})
}

// TestObservationStopAllJobFinishHints proves the StopAll publication path on
// the real producer -> adapter -> subscription chain over a Session whose
// graph is not resident: the stopping invalidation is queued by the time
// StopAll enters the job stopper, and the member finish's job-scoped hint
// carries the owning identity and an advanced pair that authoritative
// hydration agrees with.
func TestObservationStopAllJobFinishHints(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		stopper := newLifecycleStopper()
		bg := openBackgroundRuntime(t, store, stopper)
		defer func() {
			if err := bg.r.Close(context.Background()); err != nil {
				bg.t.Errorf("Close: %v", err)
			}
		}()
		sub, err := bg.r.Subscribe(256)
		testassert.NoError(t, err)
		jobSession := bg.session("stopall-job")
		workspace := sessionViewThroughRuntime(t, bg.r, jobSession).Header.Identity.Workspace
		completionID := bg.startJob(jobSession, "0a1b2c3d")
		stopper.addDelivery(jobSession, completionID, "job report")

		// Twelve other idle Sessions are created after the job: under the
		// pre-fix retention bound the job Session's graph is evicted, and
		// under the one release rule it is released with its last use anyway.
		for i := 0; i < 12; i++ {
			bg.session(fmt.Sprintf("idle-%02d", i))
		}

		// Every queued hint before the stop belongs to the setup.
		drainQueued := func() []Event {
			var out []Event
			for {
				select {
				case event, ok := <-sub.Events():
					testassert.Require(t, ok, "the subscription closed before the drain finished")
					out = append(out, event)
					continue
				default:
				}
				return out
			}
		}
		drainQueued()

		arrived := make(chan struct{})
		gate := make(chan struct{})
		releaseGate := sync.OnceFunc(func() { close(gate) })
		stopper.armGate(arrived, gate)
		// LIFO: this release runs before the deferred Runtime.Close, so a row
		// that fails before releasing the stopper never strands StopAll behind
		// the parked Job stop while Close waits for admitted work.
		defer releaseGate()
		stopDone := make(chan error, 1)
		go func() {
			stopDone <- bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				return h.StopAll(ctx)
			})
		}()
		select {
		case <-arrived: // StopAll published the stopping invalidation before entering the stopper
		case <-time.After(10 * time.Second):
			t.Fatal("the job stop never reached the stopper")
		}

		// The stopping invalidation is already queued: no polling.
		var stopping protocol.SessionRevision
		for _, event := range drainQueued() {
			if eventKind(t, event) != "session_changed" {
				continue
			}
			body, err := event.AsSessionChangedEvent()
			testassert.NoError(t, err)
			sessionID, ok := scopeSessionIdentity(t, body.Scope)
			if !ok || sessionID != jobSession {
				continue
			}
			testassert.Require(t, scopeKind(t, body.Scope) == "session", "stopping invalidation scope = %+v, want the session granularity", body.Scope)
			stopping = body.SessionRevision
		}
		testassert.Require(t, stopping.DurableRevision != "", "the owning Session's stopping invalidation was not queued when the stopper was entered")

		releaseGate() // the delivery runs: the member finishes and StopAll converges
		select {
		case err := <-stopDone:
			testassert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("StopAll never converged")
		}

		// The member finish's job-scoped hint with its owning identity and an
		// advanced pair.
		finishPair := protocol.SessionRevision{}
		for {
			event, ok := nextEvent(t, sub)
			testassert.Require(t, ok, "the subscription closed before the job finish hint")
			if eventKind(t, event) != "session_changed" {
				continue
			}
			body, err := event.AsSessionChangedEvent()
			testassert.NoError(t, err)
			if scopeKind(t, body.Scope) != "job" {
				continue
			}
			jobScope, err := body.Scope.AsJobScope()
			testassert.NoError(t, err)
			if jobScope.JobId != "0a1b2c3d" {
				continue
			}
			testassert.Require(t, jobScope.SessionId == jobSession && deref(jobScope.Workspace) == workspace, "job finish hint scope = %+v, want the owning session %q", body.Scope, jobSession)
			finishPair = body.SessionRevision
			break
		}
		testassert.Require(t, lessPair(t, stopping, finishPair), "job finish pair = %+v, want it advanced past the stopping pair %+v", finishPair, stopping)

		// Authoritative hydration agrees: no member and a revision not older
		// than the hint.
		hydration, err := bg.r.buildHydration(ctx, jobSession)
		testassert.NoError(t, err)
		testassert.Require(t, len(hydration.Background) == 0, "hydration background = %+v, want no member after the stop", hydration.Background)
		testassert.Require(t, !lessPair(t, hydration.SessionRevision, finishPair), "hydration revision %+v is older than the job finish hint %+v", hydration.SessionRevision, finishPair)
	})
}
