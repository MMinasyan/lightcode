// External-package passive-observation suite: the public Harness observer over
// both storage implementations, covering the committed invalidation sequence,
// transient text/tool progress, observer failure containment, and restart
// without replay.
package harness_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// publicFactSink is the external suite's synchronous passive observer.
type publicFactSink struct {
	mu    sync.Mutex
	facts []harness.HarnessFact
}

func (s *publicFactSink) observe(fact harness.HarnessFact) {
	s.mu.Lock()
	s.facts = append(s.facts, fact)
	s.mu.Unlock()
}

func (s *publicFactSink) snapshot() []harness.HarnessFact {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]harness.HarnessFact(nil), s.facts...)
}

func (s *publicFactSink) invalidations() []harness.HarnessFact {
	var out []harness.HarnessFact
	for _, fact := range s.snapshot() {
		if fact.Kind == harness.FactInvalidation {
			out = append(out, fact)
		}
	}
	return out
}

// publicNoopStopper is the external suite's Jobs seam.
type publicNoopStopper struct{}

func (publicNoopStopper) StopJob(string, string) {}

// publicChildLaunch is the suite's one child launch request over a root.
func publicChildLaunch(root string) harness.LaunchChildRequest {
	return harness.LaunchChildRequest{
		ParentSessionID: root,
		AgentType:       "coder",
		Content:         []model.ContentPart{{Kind: model.PartText, Text: "child"}},
		OperationID:     "child-op-1",
		MaxConcurrent:   2,
		OutputLimit:     1024,
	}
}

// publicImmediateTool is the suite's default tool plan: one immediate success.
func publicImmediateTool(_ context.Context, call model.ToolCall) harness.PreparedTool {
	return harness.PreparedTool{
		Permissions: publicPermission,
		Immediate:   &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}},
	}
}

// newObservedPublicHarness builds one Harness over the store with the public
// fixture's capture shape and execution behavior plus the given observer and
// Jobs seam.
func newObservedPublicHarness(t *testing.T, store harness.Storage, script *scriptModel, stopper harness.JobStopper, observe func(harness.HarnessFact)) (*harness.Harness, context.CancelFunc) {
	t.Helper()
	return newObservedPublicHarnessWithTool(t, store, script, stopper, observe, publicImmediateTool)
}

// newObservedPublicHarnessWithTool is the harness builder with the one
// supplied concrete tool plan, so fixtures can park an executor.
func newObservedPublicHarnessWithTool(t *testing.T, store harness.Storage, script *scriptModel, stopper harness.JobStopper, observe func(harness.HarnessFact), tool func(context.Context, model.ToolCall) harness.PreparedTool) (*harness.Harness, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := harness.New(ctx, harness.Dependencies{
		Storage: store,
		Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: publicCapture(),
				Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
					exec := publicExecution(script.effect, script.effect)
					exec.Tool = tool
					return exec, nil
				},
			}, nil
		},
		Jobs:    stopper,
		Observe: observe,
	})
	if err != nil {
		cancel()
		t.Fatalf("New: %v", err)
	}
	return h, cancel
}

// awaitPublicQuiet waits until one Session is quiet and the observer has
// delivered at least one publication, the deterministic post-terminal barrier.
func awaitPublicQuiet(t *testing.T, h *harness.Harness, session string, sink *publicFactSink) harness.SessionReadView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		view, err := h.ReadSessionView(context.Background(), session)
		testassert.NoError(t, err)
		if !view.History.ExecutionBusy && view.Header.CurrentOperationID == "" && len(sink.invalidations()) > 0 {
			return view
		}
		testassert.Require(t, !time.Now().After(deadline), "session %s never reached the observed quiet pair", session)
		time.Sleep(time.Millisecond)
	}
}

// publicChildSummary is the child fixture's projected summary turn: its
// message text as framed by the projection.
const publicChildSummary = "[Previous conversation summary]\n\nchild summary\n\n[End of summary. Continue from here.]"

// TestPublicLaunchChildSessionAutomaticCompaction retains the launched-child
// trigger oracle on both stores and extends it with the frozen-checkpoint
// steering row: the first child turn reports the conversation usage that
// overflows the captured window, the child's actual compact request parks
// inside the checkpoint, a user steering head submitted during the freeze
// stays pending with no successor scoped, the compaction summary and usage
// commit once under the preceding child Operation, and the successor then owns
// its caller identity/input and receives the summary projection. Completion
// delivery, terminal publication, scoped Close and run retirement are
// separate observations.
func TestPublicLaunchChildSessionAutomaticCompaction(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx, cancel := context.WithCancel(context.Background())
		sink := &publicFactSink{}
		first := strings.Repeat("seed ", 80)
		huge := strings.Repeat("word ", 4000)
		capture := compactManualCapture()
		capture.ContextWindow, capture.OutputReserve = 4096, 2048
		var mu sync.Mutex
		var prepares []harness.PreparationRequest
		type requestObservation struct {
			session, operation string
			request            model.Request
		}
		var requests []requestObservation
		var compactRequests []model.Request
		compactArrived := make(chan struct{})
		compactGate := make(chan struct{})
		parentRequest := make(chan string, 1)
		parentClose := make(chan string, 1)
		root := ""
		h, err := harness.New(ctx, harness.Dependencies{
			Storage: store,
			Observe: sink.observe,
			Prepare: func(_ context.Context, req harness.PreparationRequest) (harness.PreparedExecution, error) {
				mu.Lock()
				prepares = append(prepares, req)
				ownedCapture := capture
				mu.Unlock()
				return harness.PreparedExecution{
					Capture: ownedCapture,
					Open: func(_ context.Context, admission harness.OperationAdmission) (harness.Execution, error) {
						return harness.Execution{
							Model: func(_ context.Context, req model.Request) (model.Stream, error) {
								mu.Lock()
								requests = append(requests, requestObservation{admission.SessionID, admission.OperationID, req})
								mu.Unlock()
								if admission.SessionID == root {
									parentRequest <- admission.OperationID
									return summaryTurnStream("parent complete", model.Usage{}), nil
								}
								if admission.OperationID == "child-op-1" {
									// The report that will overflow the
									// checkpoint: window 4096 minus reserve
									// 2048.
									return summaryTurnStream("child answer", model.Usage{InputTokens: 5000, CachedInputTokens: 100, OutputTokens: 50}), nil
								}
								return summaryTurnStream("child answer", model.Usage{}), nil
							},
							CompactModel: func(_ context.Context, req model.Request) (model.Stream, error) {
								mu.Lock()
								compactRequests = append(compactRequests, req)
								first := len(compactRequests) == 1
								mu.Unlock()
								if first {
									close(compactArrived) // the actual compact request parks inside the frozen checkpoint
									<-compactGate
								}
								return summaryTurnStream("child summary", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
							},
							Tool:          publicImmediateTool,
							NormalizeTool: publicNormalize,
							Close: func() error {
								if admission.SessionID == root {
									parentClose <- admission.OperationID
								}
								return nil
							},
						}, nil
					},
				}, nil
			},
		})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		defer func() {
			cancel()
			{
				err := h.Wait(context.Background())
				testassert.Check(t, err == nil, "Wait: %v", err)
			}
		}()
		root = createSession(t, h)
		launch := publicChildLaunch(root)
		launch.Content = []model.ContentPart{{Kind: model.PartText, Text: first}}
		res, err := h.LaunchChildSession(context.Background(), launch)
		testassert.NoError(t, err)
		child := res.ChildSessionID
		{
			rec := awaitTerminal(t, h, child, "child-op-1")
			testassert.Require(t, rec.State.Status == harness.OperationSuccess, "first child operation: %+v", rec.State)
		}
		// A terminal child register does not prove that its completion reached
		// the parent. Join the actual parent execution and its lifetime.
		var completionID string
		select {
		case completionID = <-parentRequest:
		case <-time.After(2 * time.Second):
			t.Fatal("first child completion never reached the parent model boundary")
		}
		testassert.Require(t, completionID != "" && completionID != launch.OperationID, "parent completion identity = %q", completionID)
		{
			rec := awaitTerminal(t, h, root, completionID)
			testassert.Require(t, rec.State.Status == harness.OperationSuccess && rec.Admission.RequestKind == harness.RequestKindMessage, "parent completion operation: %+v", rec)
		}
		select {
		case closed := <-parentClose:
			testassert.Equal(t, closed, completionID)
		case <-time.After(2 * time.Second):
			t.Fatal("parent completion execution never closed")
		}
		parentQuiet := awaitPublicQuiet(t, h, root, sink)
		testassert.Require(t, len(parentQuiet.Background) == 0, "first child membership not retired: %+v", parentQuiet.Background)
		awaitPublicQuiet(t, h, child, sink)
		parentBefore := snapshotSession(t, store, root)

		mu.Lock()
		var firstRequest model.Request
		var foundFirst, foundParent bool
		for _, observed := range requests {
			if observed.session == child && observed.operation == "child-op-1" {
				firstRequest, foundFirst = observed.request, true
			}
			if observed.session == root && observed.operation == completionID {
				got := texts(observed.request)
				foundParent = len(got) == 2 && got[1] == "child answer"
			}
		}
		testassert.Check(t, len(compactRequests) == 0, "ordinary first child turn compacted")
		mu.Unlock()
		testassert.Require(t, foundFirst && foundParent, "missing first child or actual parent completion request: child=%v parent=%v", foundFirst, foundParent)
		{
			got := texts(firstRequest)
			testassert.Require(t, len(got) == 2 && got[1] == first, "first child projection = %q", got)
		}
		mu.Lock()
		window, compactRef := capture.ContextWindow, capture.Compact.Model
		mu.Unlock()
		{
			result, err := submit(t, h, child, "child-op-2", harness.MessageModeRegular, huge)
			testassert.Require(t, err == nil && result.Disposition == harness.DispositionAdmitted, "second child submit = %+v, %v", result, err)
		}
		select {
		case <-compactArrived: // the overflowing turn is inside its frozen compact checkpoint
		case <-time.After(10 * time.Second):
			t.Fatal("the child compact request never arrived")
		}
		// a user steering head submitted during the frozen checkpoint stays
		// pending: no compact piece exits at a boundary and no successor is scoped
		steered, err := submit(t, h, child, "child-op-3", harness.MessageModeRegular, "steered child")
		testassert.Require(t, err == nil && steered.Disposition == harness.DispositionSteering && steered.Operation == nil, "checkpoint steering submit = %+v, %v", steered, err)
		frozen, err := h.ReadSessionView(context.Background(), child)
		testassert.NoError(t, err)
		testassert.Require(t, len(frozen.Steering) == 1 && frozen.Steering[0].OperationID == "child-op-3", "steering during the checkpoint = %+v, want the head pending", frozen.Steering)
		for _, op := range frozen.Operations {
			testassert.Require(t, op.OperationID != "child-op-3", "the pending head was scoped as an Operation inside the frozen checkpoint")
		}
		close(compactGate) // the pieces complete and the turn settles

		secondOperation := awaitTerminal(t, h, child, "child-op-2")
		{
			rec := secondOperation
			testassert.Require(t, rec.State.Status == harness.OperationSuccess && rec.Admission.Execution.ContextWindow == window, "second child capture/terminal: %+v", rec)
		}
		thirdOperation := awaitSettled(t, h, child, "child-op-3")
		testassert.Require(t, thirdOperation.State.Status == harness.OperationSuccess, "checkpoint successor terminal: %+v", thirdOperation.State)
		childQuiet := awaitPublicQuiet(t, h, child, sink)
		mu.Lock()
		observedPieces := append([]model.Request(nil), compactRequests...)
		observedRequests := append([]requestObservation(nil), requests...)
		observedPrepares := append([]harness.PreparationRequest(nil), prepares...)
		mu.Unlock()
		testassert.Equal(t, len(observedRequests), 4)
		testassert.Equal(t, len(observedPieces), 1)
		var rebuilt model.Request
		var successor model.Request
		rebuiltCalls, successorCalls := 0, 0
		for _, observed := range observedRequests {
			if observed.session == child && observed.operation == "child-op-2" {
				rebuilt, rebuiltCalls = observed.request, rebuiltCalls+1
			}
			if observed.session == child && observed.operation == "child-op-3" {
				successor, successorCalls = observed.request, successorCalls+1
			}
		}
		got := texts(rebuilt)
		testassert.Require(t, rebuiltCalls == 1 && len(got) == 2 && got[1] == publicChildSummary && rebuilt.Messages[1].Source == compactRef, "child rebuilt calls/projection = %d/%q", rebuiltCalls, got)
		successorGot := texts(successor)
		testassert.Require(t, successorCalls == 1 && len(successorGot) == 4 && successorGot[1] == got[1] && successorGot[2] == "child answer" && successorGot[3] == "steered child", "checkpoint successor calls/projection = %d/%v, want the summary, the post-checkpoint turn, then its own admitted input", successorCalls, successorGot)
		childFacts := childQuiet.History.HistoryFacts(historyIDsOf(childQuiet.History.HistoryHeaders()))
		childBoundaryReg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: child, Kind: harness.RegisterSession})
		testassert.NoError(t, err)
		var childSessionState struct {
			State struct {
				CompactionEntryID string `json:"compaction_entry_id"`
			} `json:"state"`
		}
		{
			err := json.Unmarshal(childBoundaryReg.Payload, &childSessionState)
			testassert.NoError(t, err)
		}
		steerOwned := false
		for _, fact := range childFacts {
			if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 &&
				fact.Input.Content[0].Text == "steered child" {
				testassert.Equal(t, fact.OperationID, "child-op-3")
				steerOwned = true
			}
		}
		testassert.Require(t, steerOwned, "the checkpoint steering head never committed under its own Operation")
		compactions := 0
		for _, fact := range childFacts {
			if fact.Kind == harness.EntryCompaction {
				comp := fact.Compaction
				testassert.Require(t, comp != nil, "%v", "compaction fact has no payload")
				compactions++
				testassert.Require(t, comp.OperationID == "child-op-2" && comp.Summary == "child summary" && comp.Usage != nil && *comp.Usage == (harness.UsageCount{InputTokens: 1, OutputTokens: 1}) && childSessionState.State.CompactionEntryID == comp.EntryID, "child compaction/projection = %+v", comp)
			}
		}
		testassert.Equal(t, compactions, 1)
		pieceCount := func(totals harness.UsageTotals) bool {
			for _, usage := range totals.ByModel {
				if usage.Model == compactRef {
					return usage.Usage == (harness.UsageCount{InputTokens: 1, OutputTokens: 1})
				}
			}
			return false
		}
		testassert.Require(t, pieceCount(secondOperation.State.Usage) && pieceCount(childQuiet.TotalUsage), "child compact usage missing: operation=%+v session=%+v", secondOperation.State.Usage, childQuiet.TotalUsage)
		testassert.Require(t, len(observedPrepares) == 4 && observedPrepares[0].Session.Identity.SessionID == child && observedPrepares[1].Session.Identity.SessionID == root && observedPrepares[2].Session.Identity.SessionID == child && observedPrepares[3].Session.Identity.SessionID == child, "shared preparation lineage = %+v", observedPrepares)
		for _, preparation := range observedPrepares {
			testassert.Equal(t, preparation.RequestKind, harness.RequestKindMessage)
		}
		assertForkSourceUnchanged(t, store, root, parentBefore)
	})
}

// TestObservePublicBothStores proves the public observer over memory and
// SQLite: invalidation, text and tool progress carry their Session/Operation
// identities, the cached publication read returns the authoritative snapshot
// pair, and a restarted owner replays nothing.
func TestObservePublicBothStores(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		sink := &publicFactSink{}
		script := newScriptModel(publicTurn("call-1"), publicTurn())
		h, cancel := newObservedPublicHarness(t, store, script, nil, sink.observe)
		defer func() {
			cancel()
			_ = h.Wait(context.Background())
		}()
		session := createSession(t, h)
		{
			_, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		{
			rec := awaitTerminal(t, h, session, "op-1")
			testassert.Equal(t, rec.State.Status, harness.OperationSuccess)
		}
		snap := awaitPublicQuiet(t, h, session, sink)

		var (
			deltas   int
			started  int
			finished int
		)
		for _, fact := range sink.snapshot() {
			testassert.Require(t, fact.SessionID == session, "fact names a foreign session: %+v", fact)
			switch fact.Kind {
			case harness.FactInvalidation:
			case harness.FactTextDelta:
				deltas++
				testassert.Require(t, fact.OperationID == "op-1" && fact.Position >= 0 && fact.Content != "", "text delta fact = %+v", fact)
			case harness.FactToolStarted:
				started++
				testassert.Require(t, fact.OperationID == "op-1" && fact.CallID == "call-1" && fact.Ordinal == 0 && fact.Name == "echo", "tool started fact = %+v", fact)
			case harness.FactToolFinished:
				finished++
				testassert.Require(t, fact.OperationID == "op-1" && fact.CallID == "call-1" && fact.Status == model.ResultSuccess, "tool finished fact = %+v", fact)
			default:
				t.Fatalf("fact kind %q is outside the closed set", fact.Kind)
			}
		}
		if len(sink.invalidations()) == 0 {
			t.Fatalf("no invalidation was delivered: %+v", sink.snapshot())
		}
		current := harness.SessionRevision{DurableRevision: snap.Header.Revision.DurableRevision, LocalRevision: snap.Header.Revision.LocalRevision}
		{
			_, pair, ok := h.ReadObservation(session)
			testassert.Require(t, ok && pair == current, "cached publication read = %+v %v, quiet snapshot = %+v", pair, ok, current)
		}
		testassert.Require(t, deltas == 2 && started == 1 && finished == 1, "progress counts = deltas %d started %d finished %d", deltas, started, finished)

		// Restart: recovery and the new owner's reads replay nothing.
		cancel()
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
		}
		{
			err := harness.Recover(context.Background(), store)
			testassert.NoError(t, err)
		}
		restartSink := &publicFactSink{}
		restarted, restartCancel := newObservedPublicHarness(t, store, newScriptModel(), nil, restartSink.observe)
		defer restartCancel()
		{
			_, err := restarted.ListSessions(context.Background())
			testassert.NoError(t, err)
		}
		{
			_, err := restarted.ReadSessionHeader(context.Background(), session)
			testassert.NoError(t, err)
		}
		{
			_, err := restarted.ReadHistoryView(context.Background(), session)
			testassert.NoError(t, err)
		}
		{
			got := restartSink.snapshot()
			testassert.Require(t, len(got) == 0, "the restarted owner replayed %+v", got)
		}
		{
			_, err := restarted.ChangeAgentType(context.Background(), session, "planner")
			testassert.NoError(t, err)
		}
		{
			got := restartSink.invalidations()
			testassert.Require(t, len(got) == 1, "the restarted owner's first publication emitted %+v", got)
		}
		restartCancel()
		_ = restarted.Wait(context.Background())
	})
}

// TestObservePublicObserverFailures proves nil, panicking, and saturated
// bounded observers cannot change settlement over both stores.
func TestObservePublicObserverFailures(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		modes := []struct {
			name    string
			observe func(harness.HarnessFact)
		}{
			{name: "nil", observe: nil},
			{name: "panicking", observe: func(harness.HarnessFact) { panic("observer down") }},
			{name: "saturated", observe: func() func(harness.HarnessFact) {
				dropped := make(chan harness.HarnessFact, 1) // never drained
				return func(fact harness.HarnessFact) {
					select {
					case dropped <- fact:
					default:
					}
				}
			}()},
		}
		for _, mode := range modes {
			t.Run(mode.name, func(t *testing.T) {
				script := newScriptModel(publicTurn())
				h, cancel := newObservedPublicHarness(t, store, script, nil, mode.observe)
				defer func() {
					cancel()
					_ = h.Wait(context.Background())
				}()
				session := createSession(t, h)
				{
					_, err := submit(t, h, session, "op-"+mode.name, harness.MessageModeRegular, "hello")
					testassert.NoError(t, err)
				}
				{
					rec := awaitTerminal(t, h, session, "op-"+mode.name)
					testassert.Equal(t, rec.State.Status, harness.OperationSuccess)
				}
			})
		}
	})
}

// TestObservePublicJobAndChild proves job member admission and finish name
// the Job and advance the owning-state pair as expected, while child member
// and child creation facts never name a Job, over both stores.
func TestObservePublicJobAndChild(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		sink := &publicFactSink{}
		script := newScriptModel(publicTurn(), publicTurn(), publicTurn(), publicTurn())
		script.gate = make(chan struct{}) // park the completion execution at its model boundary
		h, cancel := newObservedPublicHarness(t, store, script, publicNoopStopper{}, sink.observe)
		defer func() {
			cancel()
			_ = h.Wait(context.Background())
		}()
		root := createSession(t, h)

		beforeAdmit, err := h.ReadSessionHeader(context.Background(), root)
		testassert.NoError(t, err)
		var completionID string
		{
			err := h.StartJob(context.Background(), root, "deadbeef", func(_ context.Context, id string) error {
				completionID = id
				return nil
			})
			testassert.NoError(t, err)
		}
		jobFacts := jobInvalidations(sink, root)
		testassert.Require(t, len(jobFacts) == 1, "job admission facts = %+v, want one job-attributed invalidation", jobFacts)
		afterAdmit, err := h.ReadSessionHeader(context.Background(), root)
		testassert.NoError(t, err)
		testassert.Require(t, afterAdmit.Revision.DurableRevision == beforeAdmit.Revision.DurableRevision && afterAdmit.Revision.LocalRevision == beforeAdmit.Revision.LocalRevision+1, "job admission pair = %d/%d, want one local publication over %d/%d", afterAdmit.Revision.DurableRevision, afterAdmit.Revision.LocalRevision, beforeAdmit.Revision.DurableRevision, beforeAdmit.Revision.LocalRevision)

		beforeFinish, err := h.ReadSessionHeader(context.Background(), root)
		testassert.NoError(t, err)
		{
			err := h.DeliverBackgroundCompletion(context.Background(), root, completionID, "job done")
			testassert.NoError(t, err)
		}
		afterFinish, err := h.ReadSessionHeader(context.Background(), root)
		testassert.NoError(t, err)
		jobFacts = jobInvalidations(sink, root)
		testassert.Require(t, len(jobFacts) == 2, "job facts = %+v, want the admission and the finish", jobFacts)
		testassert.Require(t, afterFinish.Revision.DurableRevision > beforeFinish.Revision.DurableRevision, "job finish pair = %d/%d, want a durable advance past the before read %d", afterFinish.Revision.DurableRevision, afterFinish.Revision.LocalRevision, beforeFinish.Revision.DurableRevision)
		script.releaseGate()
		{
			rec := awaitTerminal(t, h, root, completionID)
			testassert.Equal(t, rec.State.Status, harness.OperationSuccess)
		}

		before := len(sink.snapshot())
		res, err := h.LaunchChildSession(context.Background(), publicChildLaunch(root))
		testassert.NoError(t, err)
		var childCreate bool
		for _, fact := range sink.snapshot()[before:] {
			testassert.Require(t, fact.JobID == "", "child flow fact carries a Job identity: %+v", fact)
			if fact.SessionID == res.ChildSessionID {
				childCreate = true
			}
		}
		if !childCreate {
			t.Fatalf("the child creation emitted no fact: %+v", sink.snapshot())
		}
		for _, fact := range sink.snapshot() {
			testassert.Require(t, fact.Kind == harness.FactInvalidation || fact.JobID == "", "non-invalidation fact carries a Job identity: %+v", fact)
		}
	})
}

// jobInvalidations returns the invalidations attributed to one Session's Job.
func jobInvalidations(sink *publicFactSink, session string) []harness.HarnessFact {
	var out []harness.HarnessFact
	for _, fact := range sink.invalidations() {
		if fact.SessionID == session && fact.JobID == "deadbeef" {
			out = append(out, fact)
		}
	}
	return out
}

// observedBlockedStreamFixture builds one observed Harness whose accepted
// stream yields one positioned delta and then blocks on its exhausted hook
// before the EOF report, returning an idempotent release and the arrival
// channel.
func observedBlockedStreamFixture(t *testing.T, store harness.Storage) (*publicFactSink, *harness.Harness, context.CancelFunc, <-chan struct{}, func()) {
	t.Helper()
	sink := &publicFactSink{}
	arrived := make(chan struct{})
	released := make(chan struct{})
	var (
		arriveOnce  sync.Once
		releaseOnce sync.Once
	)
	release := func() { releaseOnce.Do(func() { close(released) }) }
	stream := &publicScriptStream{
		deltas: []model.StreamDelta{{
			HasChoice:        true,
			Role:             "assistant",
			FinishReason:     "stop",
			ContentFragments: []model.ContentFragment{{Position: 3, Kind: model.PartText, Text: "partial"}},
		}},
		onExhausted: func() {
			arriveOnce.Do(func() {
				close(arrived)
				<-released
			})
		},
	}
	h, cancel := newObservedPublicHarness(t, store, newScriptModel(publicAttempt{stream: stream}), nil, sink.observe)
	return sink, h, cancel, arrived, release
}

// assertRunningTextDelta asserts one text_delta fact with the expected
// Session/Operation identity, position and content while the Operation is
// still running.
func assertRunningTextDelta(t *testing.T, h *harness.Harness, sink *publicFactSink, session, operation string) {
	t.Helper()
	rec, err := h.ReadOperation(context.Background(), session, operation)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == harness.OperationRunning && rec.State.Terminal == nil, "operation state = %+v, want still running", rec.State)
	var deltas int
	for _, fact := range sink.snapshot() {
		if fact.Kind != harness.FactTextDelta {
			continue
		}
		deltas++
		testassert.Require(t, fact.SessionID == session && fact.OperationID == operation && fact.Position == 3 && fact.Content == "partial", "text delta fact = %+v", fact)
	}
	testassert.Equal(t, deltas, 1)
}

// TestObservePublicAcceptedStreamRootAndChild proves a nonempty accepted-stream
// delta emits its text_delta fact while the Operation is still running, for a
// root and a child Session, over both stores.
func TestObservePublicAcceptedStreamRootAndChild(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		t.Run("root", func(t *testing.T) {
			sink, h, cancel, arrived, release := observedBlockedStreamFixture(t, store)
			defer func() {
				release()
				cancel()
				_ = h.Wait(context.Background())
			}()
			session := createSession(t, h)
			{
				_, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello")
				testassert.NoError(t, err)
			}
			<-arrived
			assertRunningTextDelta(t, h, sink, session, "op-1")
			release()
			{
				rec := awaitTerminal(t, h, session, "op-1")
				testassert.Equal(t, rec.State.Status, harness.OperationSuccess)
			}
		})
		t.Run("child", func(t *testing.T) {
			sink, h, cancel, arrived, release := observedBlockedStreamFixture(t, store)
			defer func() {
				release()
				cancel()
				_ = h.Wait(context.Background())
			}()
			root := createSession(t, h)
			res, err := h.LaunchChildSession(context.Background(), publicChildLaunch(root))
			testassert.NoError(t, err)
			<-arrived
			assertRunningTextDelta(t, h, sink, res.ChildSessionID, "child-op-1")
			release()
			{
				rec := awaitTerminal(t, h, res.ChildSessionID, "child-op-1")
				testassert.Equal(t, rec.State.Status, harness.OperationSuccess)
			}
		})
	})
}

// observedBlockedToolFixture builds one observed Harness whose executor
// signals its arrival and then blocks, returning the arrival channel and an
// idempotent release.
func observedBlockedToolFixture(t *testing.T, store harness.Storage) (*publicFactSink, *harness.Harness, context.CancelFunc, <-chan struct{}, func()) {
	t.Helper()
	sink := &publicFactSink{}
	execStarted := make(chan struct{})
	released := make(chan struct{})
	var (
		startOnce   sync.Once
		releaseOnce sync.Once
	)
	release := func() { releaseOnce.Do(func() { close(released) }) }
	tool := func(_ context.Context, call model.ToolCall) harness.PreparedTool {
		return harness.PreparedTool{
			Permissions: publicPermission,
			Execute: func(context.Context) harness.ToolOutcome {
				startOnce.Do(func() { close(execStarted) })
				<-released
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
			},
		}
	}
	script := newScriptModel(publicTurn("call-1"), publicTurn())
	h, cancel := newObservedPublicHarnessWithTool(t, store, script, nil, sink.observe, tool)
	return sink, h, cancel, execStarted, release
}

// assertRunningToolStarted asserts one tool_started fact with the expected
// identity and no finished fact while the executor is blocked.
func assertRunningToolStarted(t *testing.T, h *harness.Harness, sink *publicFactSink, session, operation string) {
	t.Helper()
	rec, err := h.ReadOperation(context.Background(), session, operation)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == harness.OperationRunning && rec.State.Terminal == nil, "operation state = %+v, want still running", rec.State)
	var started, finished int
	for _, fact := range sink.snapshot() {
		switch fact.Kind {
		case harness.FactToolStarted:
			started++
			testassert.Require(t, fact.SessionID == session && fact.OperationID == operation && fact.CallID == "call-1" && fact.Ordinal == 0 && fact.Name == "echo", "tool started fact = %+v", fact)
		case harness.FactToolFinished:
			finished++
		}
	}
	testassert.Require(t, started == 1 && finished == 0, "tool progress while blocked = started %d finished %d, want one started and none finished", started, finished)
}

// assertOneToolFinished asserts exactly one finished fact with the expected
// identity after the committed result.
func assertOneToolFinished(t *testing.T, sink *publicFactSink, session, operation string) {
	t.Helper()
	var finished int
	for _, fact := range sink.snapshot() {
		if fact.Kind != harness.FactToolFinished {
			continue
		}
		finished++
		testassert.Require(t, fact.SessionID == session && fact.OperationID == operation && fact.CallID == "call-1" && fact.Status == model.ResultSuccess, "tool finished fact = %+v", fact)
	}
	testassert.Equal(t, finished, 1)
}

// TestObservePublicToolProgressRootAndChild proves one tool_started fact while
// the executor is blocked and exactly one tool_finished fact after the
// committed result, for a root and a child Session, over both stores.
func TestObservePublicToolProgressRootAndChild(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		t.Run("root", func(t *testing.T) {
			sink, h, cancel, execStarted, release := observedBlockedToolFixture(t, store)
			defer func() {
				release()
				cancel()
				_ = h.Wait(context.Background())
			}()
			session := createSession(t, h)
			{
				_, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello")
				testassert.NoError(t, err)
			}
			<-execStarted
			assertRunningToolStarted(t, h, sink, session, "op-1")
			release()
			{
				rec := awaitTerminal(t, h, session, "op-1")
				testassert.Equal(t, rec.State.Status, harness.OperationSuccess)
			}
			assertOneToolFinished(t, sink, session, "op-1")
		})
		t.Run("child", func(t *testing.T) {
			sink, h, cancel, execStarted, release := observedBlockedToolFixture(t, store)
			defer func() {
				release()
				cancel()
				_ = h.Wait(context.Background())
			}()
			root := createSession(t, h)
			res, err := h.LaunchChildSession(context.Background(), publicChildLaunch(root))
			testassert.NoError(t, err)
			<-execStarted
			assertRunningToolStarted(t, h, sink, res.ChildSessionID, "child-op-1")
			release()
			{
				rec := awaitTerminal(t, h, res.ChildSessionID, "child-op-1")
				testassert.Equal(t, rec.State.Status, harness.OperationSuccess)
			}
			assertOneToolFinished(t, sink, res.ChildSessionID, "child-op-1")
		})
	})
}
