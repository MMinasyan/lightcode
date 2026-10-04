// External-package passive-observation suite: the public Harness observer over
// both storage implementations, covering the committed invalidation sequence,
// transient text/tool progress, observer failure containment, and restart
// without replay.
package harness_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
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
					return harness.Execution{
						Model:         script.effect,
						CompactModel:  script.effect,
						NormalizeTool: publicNormalize,
						Tool:          tool,
					}, nil
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
func awaitPublicQuiet(t *testing.T, h *harness.Harness, session string, sink *publicFactSink) harness.SessionSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap, err := h.SnapshotSession(context.Background(), session)
		if err != nil {
			t.Fatalf("SnapshotSession: %v", err)
		}
		if !snap.ExecutionBusy && snap.Session.State.CurrentOperationID == "" && len(sink.invalidations()) > 0 {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s never reached the observed quiet pair", session)
		}
		time.Sleep(time.Millisecond)
	}
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
		if _, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		if rec := awaitTerminal(t, h, session, "op-1"); rec.State.Status != harness.OperationSuccess {
			t.Fatalf("operation status = %s, want success", rec.State.Status)
		}
		snap := awaitPublicQuiet(t, h, session, sink)

		var (
			deltas   int
			started  int
			finished int
		)
		for _, fact := range sink.snapshot() {
			if fact.SessionID != session {
				t.Fatalf("fact names a foreign session: %+v", fact)
			}
			switch fact.Kind {
			case harness.FactInvalidation:
			case harness.FactTextDelta:
				deltas++
				if fact.OperationID != "op-1" || fact.Position < 0 || fact.Content == "" {
					t.Fatalf("text delta fact = %+v", fact)
				}
			case harness.FactToolStarted:
				started++
				if fact.OperationID != "op-1" || fact.CallID != "call-1" || fact.Ordinal != 0 || fact.Name != "echo" {
					t.Fatalf("tool started fact = %+v", fact)
				}
			case harness.FactToolFinished:
				finished++
				if fact.OperationID != "op-1" || fact.CallID != "call-1" || fact.Status != model.ResultSuccess {
					t.Fatalf("tool finished fact = %+v", fact)
				}
			default:
				t.Fatalf("fact kind %q is outside the closed set", fact.Kind)
			}
		}
		if len(sink.invalidations()) == 0 {
			t.Fatalf("no invalidation was delivered: %+v", sink.snapshot())
		}
		current := harness.SessionRevision{DurableRevision: snap.Session.Revision, LocalRevision: snap.LocalRevision}
		if _, pair, ok := h.ReadObservation(session); !ok || pair != current {
			t.Fatalf("cached publication read = %+v %v, quiet snapshot = %+v", pair, ok, current)
		}
		if deltas != 2 || started != 1 || finished != 1 {
			t.Fatalf("progress counts = deltas %d started %d finished %d", deltas, started, finished)
		}

		// Restart: recovery and the new owner's reads replay nothing.
		cancel()
		if err := h.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if err := harness.Recover(context.Background(), store); err != nil {
			t.Fatalf("Recover: %v", err)
		}
		restartSink := &publicFactSink{}
		restarted, restartCancel := newObservedPublicHarness(t, store, newScriptModel(), nil, restartSink.observe)
		defer restartCancel()
		if _, err := restarted.ListSessions(context.Background()); err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		if _, err := restarted.ReadSessionHeader(context.Background(), session); err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if _, err := restarted.SnapshotSession(context.Background(), session); err != nil {
			t.Fatalf("SnapshotSession: %v", err)
		}
		if got := restartSink.snapshot(); len(got) != 0 {
			t.Fatalf("the restarted owner replayed %+v", got)
		}
		if _, err := restarted.ChangeAgentType(context.Background(), session, "planner"); err != nil {
			t.Fatalf("ChangeAgentType: %v", err)
		}
		if got := restartSink.invalidations(); len(got) != 1 {
			t.Fatalf("the restarted owner's first publication emitted %+v", got)
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
				if _, err := submit(t, h, session, "op-"+mode.name, harness.MessageModeRegular, "hello"); err != nil {
					t.Fatalf("submit: %v", err)
				}
				if rec := awaitTerminal(t, h, session, "op-"+mode.name); rec.State.Status != harness.OperationSuccess {
					t.Fatalf("operation status = %s, want success", rec.State.Status)
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

		beforeAdmit, err := h.SnapshotSession(context.Background(), root)
		if err != nil {
			t.Fatalf("SnapshotSession before the job admission: %v", err)
		}
		var completionID string
		if err := h.StartJob(context.Background(), root, "deadbeef", func(_ context.Context, id string) error {
			completionID = id
			return nil
		}); err != nil {
			t.Fatalf("StartJob: %v", err)
		}
		jobFacts := jobInvalidations(sink, root)
		if len(jobFacts) != 1 {
			t.Fatalf("job admission facts = %+v, want one job-attributed invalidation", jobFacts)
		}
		afterAdmit, err := h.SnapshotSession(context.Background(), root)
		if err != nil {
			t.Fatalf("SnapshotSession after the job admission: %v", err)
		}
		if afterAdmit.Session.Revision != beforeAdmit.Session.Revision || afterAdmit.LocalRevision != beforeAdmit.LocalRevision+1 {
			t.Fatalf("job admission pair = %d/%d, want one local publication over %d/%d",
				afterAdmit.Session.Revision, afterAdmit.LocalRevision, beforeAdmit.Session.Revision, beforeAdmit.LocalRevision)
		}

		beforeFinish, err := h.SnapshotSession(context.Background(), root)
		if err != nil {
			t.Fatalf("SnapshotSession before the job finish: %v", err)
		}
		if err := h.DeliverBackgroundCompletion(context.Background(), root, completionID, "job done"); err != nil {
			t.Fatalf("DeliverBackgroundCompletion: %v", err)
		}
		afterFinish, err := h.SnapshotSession(context.Background(), root)
		if err != nil {
			t.Fatalf("SnapshotSession after the job finish: %v", err)
		}
		jobFacts = jobInvalidations(sink, root)
		if len(jobFacts) != 2 {
			t.Fatalf("job facts = %+v, want the admission and the finish", jobFacts)
		}
		if afterFinish.Session.Revision <= beforeFinish.Session.Revision {
			t.Fatalf("job finish pair = %d/%d, want a durable advance past the before snapshot %d",
				afterFinish.Session.Revision, afterFinish.LocalRevision, beforeFinish.Session.Revision)
		}
		script.releaseGate()
		if rec := awaitTerminal(t, h, root, completionID); rec.State.Status != harness.OperationSuccess {
			t.Fatalf("completion operation status = %s, want success", rec.State.Status)
		}

		before := len(sink.snapshot())
		res, err := h.LaunchChildSession(context.Background(), publicChildLaunch(root))
		if err != nil {
			t.Fatalf("LaunchChildSession: %v", err)
		}
		var childCreate bool
		for _, fact := range sink.snapshot()[before:] {
			if fact.JobID != "" {
				t.Fatalf("child flow fact carries a Job identity: %+v", fact)
			}
			if fact.SessionID == res.ChildSessionID {
				childCreate = true
			}
		}
		if !childCreate {
			t.Fatalf("the child creation emitted no fact: %+v", sink.snapshot())
		}
		for _, fact := range sink.snapshot() {
			if fact.Kind != harness.FactInvalidation && fact.JobID != "" {
				t.Fatalf("non-invalidation fact carries a Job identity: %+v", fact)
			}
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
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != harness.OperationRunning || rec.State.Terminal != nil {
		t.Fatalf("operation state = %+v, want still running", rec.State)
	}
	var deltas int
	for _, fact := range sink.snapshot() {
		if fact.Kind != harness.FactTextDelta {
			continue
		}
		deltas++
		if fact.SessionID != session || fact.OperationID != operation || fact.Position != 3 || fact.Content != "partial" {
			t.Fatalf("text delta fact = %+v", fact)
		}
	}
	if deltas != 1 {
		t.Fatalf("text delta facts = %d, want one", deltas)
	}
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
			if _, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("submit: %v", err)
			}
			<-arrived
			assertRunningTextDelta(t, h, sink, session, "op-1")
			release()
			if rec := awaitTerminal(t, h, session, "op-1"); rec.State.Status != harness.OperationSuccess {
				t.Fatalf("operation status = %s, want success", rec.State.Status)
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
			if err != nil {
				t.Fatalf("LaunchChildSession: %v", err)
			}
			<-arrived
			assertRunningTextDelta(t, h, sink, res.ChildSessionID, "child-op-1")
			release()
			if rec := awaitTerminal(t, h, res.ChildSessionID, "child-op-1"); rec.State.Status != harness.OperationSuccess {
				t.Fatalf("child operation status = %s, want success", rec.State.Status)
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
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != harness.OperationRunning || rec.State.Terminal != nil {
		t.Fatalf("operation state = %+v, want still running", rec.State)
	}
	var started, finished int
	for _, fact := range sink.snapshot() {
		switch fact.Kind {
		case harness.FactToolStarted:
			started++
			if fact.SessionID != session || fact.OperationID != operation || fact.CallID != "call-1" || fact.Ordinal != 0 || fact.Name != "echo" {
				t.Fatalf("tool started fact = %+v", fact)
			}
		case harness.FactToolFinished:
			finished++
		}
	}
	if started != 1 || finished != 0 {
		t.Fatalf("tool progress while blocked = started %d finished %d, want one started and none finished", started, finished)
	}
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
		if fact.SessionID != session || fact.OperationID != operation || fact.CallID != "call-1" || fact.Status != model.ResultSuccess {
			t.Fatalf("tool finished fact = %+v", fact)
		}
	}
	if finished != 1 {
		t.Fatalf("tool finished facts = %d, want one", finished)
	}
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
			if _, err := submit(t, h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("submit: %v", err)
			}
			<-execStarted
			assertRunningToolStarted(t, h, sink, session, "op-1")
			release()
			if rec := awaitTerminal(t, h, session, "op-1"); rec.State.Status != harness.OperationSuccess {
				t.Fatalf("operation status = %s, want success", rec.State.Status)
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
			if err != nil {
				t.Fatalf("LaunchChildSession: %v", err)
			}
			<-execStarted
			assertRunningToolStarted(t, h, sink, res.ChildSessionID, "child-op-1")
			release()
			if rec := awaitTerminal(t, h, res.ChildSessionID, "child-op-1"); rec.State.Status != harness.OperationSuccess {
				t.Fatalf("child operation status = %s, want success", rec.State.Status)
			}
			assertOneToolFinished(t, sink, res.ChildSessionID, "child-op-1")
		})
	})
}
