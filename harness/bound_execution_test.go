// Bound execution contracts: invocation close ordering and declared tool
// execution knowledge.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// callbackStrategy is the two-pass replacement policy of the identity
// fixture: one tool-free model plan folds into one authored checkpoint.
type callbackStrategy struct{}

func (s callbackStrategy) Plan(_ PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "one pass"}}}}},
	}}, nil
}

func (s callbackStrategy) Consume(in ConsumeInput) (PlanOutput, error) {
	msg, err := model.NewMessage(model.Message{Role: model.RoleAssistant, Source: in.Output.Source, Content: []model.ContentPart{{Kind: model.PartText, Text: "bound summary"}}})
	if err != nil {
		return PlanOutput{}, err
	}
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{
		BoundaryEntryID: in.Input.BoundaryEntryID,
		Items:           []CheckpointItem{{Context: &msg}},
	}}, nil
}

// liveCheckStream proves the accepted model stream stays readable until its
// real Close: every read observes a live callback context.
type liveCheckStream struct {
	t      *testing.T
	ctx    context.Context
	data   []model.StreamDelta
	i      int
	closes int
}

func (s *liveCheckStream) Recv() (model.StreamDelta, error) {
	if s.ctx.Err() != nil {
		s.t.Errorf("model stream context died before its Close: %v", s.ctx.Err())
	}
	if s.i >= len(s.data) {
		return model.StreamDelta{}, io.EOF
	}
	d := s.data[s.i]
	s.i++
	return d, nil
}

func (s *liveCheckStream) Close() error { s.closes++; return nil }

// TestInvocationCloseOrderAndStreamLifetime proves the invocation ownership
// rule end to end over one real execution: an automatic procedure's final
// maintenance stage closes before the conversation continues, and the accepted
// model stream stays live until its real Close.
func TestInvocationCloseOrderAndStreamLifetime(t *testing.T) {
	capture := testCapture()
	capture.Strategy = StrategyCapture{ID: "test.callback/v1"}
	conversationRounds := 0
	var stream *liveCheckStream
	var closeOrder []string
	exec := Execution{
		Strategy: &StrategyBinding{ID: "test.callback/v1", Strategy: callbackStrategy{}},
		OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
			if request.Purpose == InvocationPurposeMaintenance {
				return InvocationExecution{
					Model: func(context.Context, model.Request) (model.Stream, error) {
						return summaryStream("bound summary", model.Usage{InputTokens: 1}), nil
					},
					Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
					NormalizeTool: objectNormalize,
					Close: func() error {
						closeOrder = append(closeOrder, "maintenance-close")
						return nil
					},
				}, nil
			}
			return InvocationExecution{
				Model: func(ctx context.Context, _ model.Request) (model.Stream, error) {
					closeOrder = append(closeOrder, "conversation-dispatch")
					conversationRounds++
					if conversationRounds == 1 {
						stream = &liveCheckStream{t: t, ctx: ctx, data: []model.StreamDelta{
							turnDelta("tool_calls", "working", testToolCall("call-1")),
						}}
						return stream, nil
					}
					return completedTurnStream(), nil
				},
				Tool: func(_ context.Context, call model.ToolCall) PreparedTool {
					return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
						return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}, Evidence: ExecutionObserved}
					}}
				},
				NormalizeTool: objectNormalize,
				Close: func() error {
					closeOrder = append(closeOrder, "conversation-close")
					return nil
				},
			}, nil
		},
	}
	prepared := PreparedExecution{Capture: capture, Open: func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	}}
	h, _, c, _ := newAdmittedHarness(t, context.Background(), prepared)
	testassert.NoError(t, executeDirect(h, c, h.ctx, testOpID, prepared))
	testassert.Require(t, conversationRounds >= 2, "conversation rounds = %d, want the tool round and its continuation", conversationRounds)
	testassert.Require(t, stream != nil && stream.closes == 1, "accepted stream closes = %d, want exactly one real Close", stream.closes)
	testassert.Require(t, reflect.DeepEqual(closeOrder, []string{"maintenance-close", "conversation-dispatch", "maintenance-close", "conversation-dispatch", "conversation-close"}),
		"disposal order = %v, want every automatic final maintenance stage closed before its continuation and the conversation invocation closed at completion", closeOrder)
}

// planReleaseRecorder captures one acquired plan's execute calls.
type planReleaseRecorder struct {
	executeCalls int
}

// executing returns the ordinary executor callback for one declared result.
func executing(callID string, status model.ToolResultStatus, content string, evidence ExecutionEvidence) func(context.Context) ToolOutcome {
	return func(context.Context) ToolOutcome {
		return ToolOutcome{Result: model.ToolResult{CallID: callID, Status: status, Content: content}, Evidence: evidence}
	}
}

// TestToolPlanEvidence proves one complete ToolOutcome reaches the canonical
// result with the producer's declared knowledge, never inferred from the
// result's shape, and that a supplied immediate plan's evidence is ignored in
// favour of the fixed success/other-status derivation.
func TestToolPlanEvidence(t *testing.T) {
	modelFn := func() func(context.Context, model.Request) (model.Stream, error) {
		rounds := 0
		return func(context.Context, model.Request) (model.Stream, error) {
			rounds++
			if rounds == 1 {
				return completedTurnStream(testToolCall("call-1")), nil
			}
			return completedTurnStream(), nil
		}
	}
	type row struct {
		name        string
		plan        PreparedTool
		stimulus    func(context.Context, context.CancelFunc, PreparedTool) PreparedTool
		wantStatus  model.ToolResultStatus
		wantContent string
		wantEvid    ExecutionEvidence
		wantExecute bool
	}
	rows := []row{
		{
			name:       "immediate success settles once",
			plan:       PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"}}},
			wantStatus: model.ResultSuccess, wantContent: "done", wantEvid: ExecutionObserved,
		},
		{
			name:       "immediate error stays not started",
			plan:       PreparedTool{Immediate: &ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: "boom"}}},
			wantStatus: model.ResultError, wantContent: "boom", wantEvid: ExecutionNotStarted,
		},
		{
			name:       "supplied immediate evidence is ignored",
			plan:       PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"}, Evidence: ExecutionNotStarted}},
			wantStatus: model.ResultSuccess, wantContent: "done", wantEvid: ExecutionObserved,
		},
		{
			name: "malformed plan shape is rejected before execution",
			plan: PreparedTool{Permissions: fixturePermission,
				Immediate: &ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"}},
				Execute:   func(context.Context) ToolOutcome { return ToolOutcome{} }},
			wantStatus: model.ResultError, wantContent: invalidToolResultContent, wantEvid: ExecutionNotStarted,
		},
		{
			name:       "denied executor does not run",
			plan:       PreparedTool{Execute: func(context.Context) ToolOutcome { return ToolOutcome{} }},
			wantStatus: model.ResultDenied, wantContent: PermissionDeniedContent, wantEvid: ExecutionNotStarted,
		},
		{
			name:       "observed executor success keeps its knowledge",
			plan:       PreparedTool{Permissions: fixturePermission, Execute: executing("call-1", model.ResultSuccess, "ran", ExecutionObserved)},
			wantStatus: model.ResultSuccess, wantContent: "ran", wantEvid: ExecutionObserved, wantExecute: true,
		},
		{
			name:       "declared not_started executor outcome reaches the canonical result",
			plan:       PreparedTool{Permissions: fixturePermission, Execute: executing("call-1", model.ResultError, "refused before handoff", ExecutionNotStarted)},
			wantStatus: model.ResultError, wantContent: "refused before handoff", wantEvid: ExecutionNotStarted, wantExecute: true,
		},
		{
			name:       "declared unknown executor outcome carries the uncertainty prefix",
			plan:       PreparedTool{Permissions: fixturePermission, Execute: executing("call-1", model.ResultError, "may have run", ExecutionUnknown)},
			wantStatus: model.ResultError, wantContent: unknownToolResultPrefix + "\n\n" + "may have run", wantEvid: ExecutionUnknown, wantExecute: true,
		},
		{
			name:       "malformed executor result is unknown",
			plan:       PreparedTool{Permissions: fixturePermission, Execute: executing("another-call", model.ResultSuccess, "ran", ExecutionObserved)},
			wantStatus: model.ResultError, wantContent: unknownToolResultPrefix + "\n\n" + invalidToolResultContent, wantEvid: ExecutionUnknown, wantExecute: true,
		},
		{
			name: "missing executor knowledge is unknown",
			plan: PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
				return ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"}}
			}},
			wantStatus: model.ResultError, wantContent: unknownToolResultPrefix + "\n\n" + invalidToolResultContent, wantEvid: ExecutionUnknown, wantExecute: true,
		},
		{
			name:       "success with not_started knowledge is the invalid replacement",
			plan:       PreparedTool{Permissions: fixturePermission, Execute: executing("call-1", model.ResultSuccess, "ran", ExecutionNotStarted)},
			wantStatus: model.ResultError, wantContent: unknownToolResultPrefix + "\n\n" + invalidToolResultContent, wantEvid: ExecutionUnknown, wantExecute: true,
		},
		{
			name: "cancellation before start settles interrupted",
			plan: PreparedTool{Permissions: fixturePermission, Execute: executing("call-1", model.ResultSuccess, "must not run", ExecutionObserved)},
			stimulus: func(_ context.Context, cancel context.CancelFunc, plan PreparedTool) PreparedTool {
				cancel()
				return plan
			},
			wantStatus: model.ResultInterrupted, wantContent: interruptedToolResultContent, wantEvid: ExecutionNotStarted,
		},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := &planReleaseRecorder{}
			var store *graphStorage
			var sessionID string
			preparedRec := preparedExecuting(effectExecution(modelFn(), func(ctx context.Context, call model.ToolCall) PreparedTool {
				plan := tc.plan
				if tc.stimulus != nil {
					plan = tc.stimulus(ctx, cancel, plan)
				}
				if plan.Execute != nil {
					inner := plan.Execute
					plan.Execute = func(ctx context.Context) ToolOutcome {
						rec.executeCalls++
						return inner(ctx)
					}
				}
				return plan
			}))
			preparedRec.Capture = testCapture()
			h, st, c, sid := newAdmittedHarness(t, context.Background(), preparedRec)
			store, sessionID = st, sid
			err := executeDirect(h, c, ctx, testOpID, preparedRec)
			testassert.NoError(t, err)
			testassert.Equal(t, rec.executeCalls > 0, tc.wantExecute)

			graph, gerr := validateFixture(t, store, sessionID)
			testassert.NoError(t, gerr)
			var result *toolResultEntry
			for i := range graph.Entries {
				if graph.Entries[i].ToolResult != nil {
					result = graph.Entries[i].ToolResult
				}
			}
			testassert.Require(t, result != nil, "the call produced no canonical tool result")
			testassert.Require(t, result.Status == tc.wantStatus && result.Content == tc.wantContent && result.Evidence == tc.wantEvid,
				"canonical result = %+v, want status %s content %q knowledge %s", result, tc.wantStatus, tc.wantContent, tc.wantEvid)
		})
	}
}

// newCompactOperationStore builds the settled-conversation Session with one
// running compact-kind Operation over the given capture.
func newCompactOperationStore(t *testing.T, capture ExecutionCapture) *graphStorage {
	t.Helper()
	fixture := validTestGraph()
	stamp := testTime
	fixture.ops[0].State.Status, fixture.ops[0].State.SettledAt = OperationSuccess, &stamp
	fixture.ops[0].State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
	settlement := validSettlementEntry()
	settlement.EntryID = hexID(3)
	fixture.entries = append(fixture.entries, testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime}, settlement: &settlement})
	compact := compactOperationRecord("compact-op")
	compact.Admission.Execution = capture
	fixture.ops = append(fixture.ops, compact)
	fixture.session.State.CurrentOperationID = "compact-op"
	return fixture.storage(t)
}

// newCompactOperationFixture builds that store's ordinary Harness and leased
// coordinator for the manual-compaction rows.
func newCompactOperationFixture(t *testing.T, capture ExecutionCapture) (*Harness, *graphStorage, *coordinator, string) {
	t.Helper()
	store := newCompactOperationStore(t, capture)
	h := newTestHarness(t, store, nil)
	c := leaseCoordinator(t, h, testSessionID)
	return h, store, c, testSessionID
}

// contextParentKey is the retained parent value the direct-execution context
// ownership row observes.
type contextParentKey struct{}

// TestExecuteDirectOwnsCancellableChildContext proves the direct fixture's
// execution interval: callbacks run under a real cancellable child of the
// supplied parent, that context stays live while the accepted stream is in
// use, the owner cancels it at retirement, and the caller's parent stays live.
func TestExecuteDirectOwnsCancellableChildContext(t *testing.T) {
	parent := context.WithValue(context.Background(), contextParentKey{}, "parent-value")
	h, _, c, _ := newEffectHarness(t, nil)
	var callbackCtx context.Context
	var streamLive bool
	// Observe the context while its accepted stream is actually being read.
	var ctxDuringStream context.Context
	exec := Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		return InvocationExecution{
			Model: func(ctx context.Context, _ model.Request) (model.Stream, error) {
				callbackCtx = ctx
				testassert.Equal(t, ctx.Value(contextParentKey{}), "parent-value")
				ctxDuringStream = ctx
				return &checkingStream{ctx: ctx, inner: streamOf(turnDelta("stop", "done")), onRead: func(live bool) { streamLive = live }}, nil
			},
			Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
			NormalizeTool: objectNormalize,
		}, nil
	}}
	testassert.NoError(t, executeDirect(h, c, parent, testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})))
	testassert.Require(t, callbackCtx != nil && ctxDuringStream != nil, "the model callback did not run")
	testassert.Require(t, streamLive, "the accepted stream was read after its execution context was canceled")
	testassert.Require(t, ctxDuringStream.Err() == context.Canceled, "retained callback context = %v, want the owner's retirement cancellation", ctxDuringStream.Err())
	testassert.Require(t, parent.Err() == nil, "the supplied parent was canceled by retirement: %v", parent.Err())
}

// checkingStream wraps one fixture stream and reports the execution context
// state on each read.
type checkingStream struct {
	ctx    context.Context
	inner  model.Stream
	onRead func(live bool)
}

func (s *checkingStream) Recv() (model.StreamDelta, error) {
	s.onRead(s.ctx.Err() == nil)
	return s.inner.Recv()
}

func (s *checkingStream) Close() error { return s.inner.Close() }

// TestManualMaintenanceDisposalFollowsCompletionPublication proves the manual
// completion ordering: the operation's own publication and terminal consume
// the maintenance result first, the enclosing completion reservation is
// already held and the actual failure retained before the final manual
// maintenance and execution resources dispose, and the disposal order is
// maintenance then execution.
func TestManualMaintenanceDisposalFollowsCompletionPublication(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failEntry bool
	}{
		{name: "manual final disposal follows its published consumption"},
		{name: "manual final disposal keeps the retained publication failure", failEntry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := testCapture()
			capture.Strategy = StrategyCapture{ID: "test.manual/v1"}
			h, store, c, sessionID := newCompactOperationFixture(t, capture)
			var events []string
			settledAtClose, failureAtClose := false, false
			modelFn := func(context.Context, model.Request) (model.Stream, error) {
				return summaryStream("manual summary", model.Usage{InputTokens: 2}), nil
			}
			exec := Execution{
				Strategy: &StrategyBinding{ID: "test.manual/v1", Strategy: callbackStrategy{}},
				OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
					return InvocationExecution{
						Model:         modelFn,
						Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
						NormalizeTool: objectNormalize,
						Close: func() error {
							events = append(events, "maintenance-close")
							rec, err := h.ReadOperation(context.Background(), sessionID, "compact-op")
							testassert.NoError(t, err)
							settledAtClose = rec.State.Terminal != nil
							h.mu.Lock()
							failureAtClose = h.storageFailure != nil
							h.mu.Unlock()
							return nil
						},
					}, nil
				},
				Close: func() error {
					events = append(events, "execution-close")
					return nil
				},
			}
			prepared := PreparedExecution{Capture: capture, Open: func(context.Context, OperationAdmission) (Execution, error) {
				return exec, nil
			}}
			if tc.failEntry {
				store.txHook = func(step string) error {
					if step == "insert_entry" {
						return fmt.Errorf("injected checkpoint failure: %w", ErrStorage)
					}
					return nil
				}
			} else {
				store.txHook = func(step string) error {
					if step == "insert_entry" {
						events = append(events, "checkpoint-published")
					}
					return nil
				}
			}
			err := executeDirect(h, c, h.ctx, "compact-op", prepared)
			if tc.failEntry {
				testassert.Require(t, err != nil && isPublicationFailure(err), "execute = %v, want the marked checkpoint publication failure", err)
				testassert.Require(t, failureAtClose, "the retained failure was not latched before the final manual disposal")
				testassert.Require(t, !settledAtClose, "the failed publication settled the Operation before its disposal")
			} else {
				testassert.NoError(t, err)
				testassert.Require(t, settledAtClose, "the Operation terminal was not published before the final manual disposal")
				testassert.Require(t, !failureAtClose, "a successful publication retained a cleanup failure before disposal")
			}
			testassert.Require(t, events[len(events)-1] == "execution-close", "event order = %v, want the execution disposed after the maintenance stage", events)
			if tc.failEntry {
				testassert.Require(t, reflect.DeepEqual(events, []string{"maintenance-close", "execution-close"}),
					"event order = %v, want the retained failure and final manual close after the failed publication", events)
			} else {
				// The checkpoint entry and the manual success settlement
				// publish in one transaction before the manual final close.
				testassert.Require(t, reflect.DeepEqual(events, []string{"checkpoint-published", "checkpoint-published", "maintenance-close", "execution-close"}),
					"event order = %v, want the publication strictly before the manual final close", events)
			}
		})
	}
}

// TestQuietCompletionSettlesUnmarkedStorageCallbackError proves the unified
// quiet completion gate: an ordinary callback failure that wraps ErrStorage
// without a publication boundary still settles the quiet running Operation as
// failure and lets the buffered successor proceed, while the maintenance
// sibling route settles its own ordinary storage-shaped failure.
func TestQuietCompletionSettlesUnmarkedStorageCallbackError(t *testing.T) {
	t.Run("ordinary request-hook storage cause settles failure and drains", func(t *testing.T) {
		hookErr := fmt.Errorf("hook transport: %w", ErrStorage)
		hook := RequestHook{ID: "req.storage", ExtensionID: "req-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			return RequestConsequence{}, hookErr
		}}
		exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			t.Error("a failed request hook dispatched a conversation request")
			return nil, nil
		}, nil, hook)
		prepared := PreparedExecution{Capture: testCapture(), Open: func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		}}
		h, store, c, sessionID := newAdmittedHarness(t, context.Background(), prepared)
		{
			res, err := submitText(t, h, sessionID, "op-2", MessageModeQueued, "successor")
			testassert.NoError(t, err)
			testassert.Equal(t, res.Disposition, DispositionQueued)
		}
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.Require(t, errors.Is(err, ErrStorage), "execute = %v, want the ordinary callback cause retained", err)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == hookErr.Error(),
			"operation state = %+v, want the quiet failure settlement carrying the ordinary cause", rec.State)
		// The buffered successor proceeds instead of being stranded behind a
		// running Operation.
		awaitOperationTerminal(t, store, sessionID, "op-2", OperationFailure)
	})

	t.Run("strategy sibling storage cause settles its own failure", func(t *testing.T) {
		planErr := fmt.Errorf("strategy transport: %w", ErrStorage)
		capture := testCapture()
		capture.Strategy = StrategyCapture{ID: "test.storage/v1"}
		exec := Execution{
			Strategy: &StrategyBinding{ID: "test.storage/v1", Strategy: &errStrategy{cause: planErr}},
			OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
				return invocationExecution(func(context.Context, model.Request) (model.Stream, error) {
					return completedTurnStream(), nil
				}, nil), nil
			},
		}
		prepared := PreparedExecution{Capture: capture, Open: func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		}}
		h, _, c, sessionID := newAdmittedHarness(t, context.Background(), prepared)
		completeErr := executeDirect(h, c, context.Background(), testOpID, prepared)
		testassert.Require(t, errors.Is(completeErr, ErrStorage), "maintenance = %v, want the strategy's own cause", completeErr)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == planErr.Error(),
			"operation state = %+v, want the strategy failure settlement", rec.State)
	})
}

// waitPublicationRace is the shared two-Session F9 schedule: B is admitted and
// parked before A's first publication fails, B's result publication fails and
// retires while A's selected final closer is blocked, then Wait is released.
type waitPublicationRace struct {
	t             *testing.T
	h             *Harness
	store         *graphStorage
	cancel        context.CancelFunc
	sessionA      string
	sessionB      string
	scriptB       *modelScript
	bClosed       chan struct{}
	bOperationID  string
	aCloseArrived chan struct{}
	aCloseGate    chan struct{}
	aClosed       chan struct{}
	startA        func()
	errA          error
	errB          error
	cleanupErr    error
}

// newWaitPublicationRace owns the shared two-Session construction: the Harness
// lifetime, B's ordinary prepared execution and Session, B's model script and
// closer, and A's shared parking channels. The caller supplies the actual store,
// A's preparation callback, and the three distinct expected causes; an empty
// aSessionID creates a new A Session, while an existing fixture A Session is
// reused. A's actual invocation/closer stimulus stays with the named test.
func newWaitPublicationRace(t *testing.T, store *graphStorage, aSessionID string, prepareA func(context.Context, PreparationRequest) (PreparedExecution, error), errA, errB, cleanupErr error) waitPublicationRace {
	t.Helper()
	scriptB := newModelScript()
	scriptB.gate = make(chan struct{})
	bClosed := make(chan struct{}, 1)
	aCloseArrived := make(chan struct{}, 1)
	aCloseGate := make(chan struct{})
	preparedB := func() PreparedExecution {
		exec := effectExecution(scriptB.model, nil)
		exec.Close = func() error {
			bClosed <- struct{}{}
			return nil
		}
		return preparedExecuting(exec)
	}
	hctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var sessionB string
	h, err := New(hctx, Dependencies{
		Storage: store,
		Prepare: func(ctx context.Context, req PreparationRequest) (PreparedExecution, error) {
			if req.Session.Identity.SessionID == sessionB {
				return preparedB(), nil
			}
			return prepareA(ctx, req)
		},
	})
	testassert.NoError(t, err)
	if aSessionID == "" {
		sessionA, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
		testassert.NoError(t, err)
		aSessionID = sessionA.Identity.SessionID
	}
	sessionBRecord, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	sessionB = sessionBRecord.Identity.SessionID
	return waitPublicationRace{
		t:             t,
		h:             h,
		store:         store,
		cancel:        cancel,
		sessionA:      aSessionID,
		sessionB:      sessionB,
		scriptB:       scriptB,
		bClosed:       bClosed,
		aCloseArrived: aCloseArrived,
		aCloseGate:    aCloseGate,
		errA:          errA,
		errB:          errB,
		cleanupErr:    cleanupErr,
	}
}

// runWaitPublicationRace executes the shared schedule and the common Wait
// observations; variants supply only their actual A execution, fault and
// closer channels.
func runWaitPublicationRace(race waitPublicationRace) {
	t := race.t
	_, err := submitText(t, race.h, race.sessionB, race.bOperationID, MessageModeRegular, "hello")
	testassert.NoError(t, err)
	<-race.scriptB.arrived
	cB := cachedCoordinator(t, race.h, race.sessionB)
	cB.mu.Lock()
	runB := cB.run
	cB.mu.Unlock()
	testassert.Require(t, runB != nil, "the admitted B run slot was not installed")
	race.startA()
	<-race.aCloseArrived
	race.scriptB.releaseGate()
	<-race.bClosed
	select {
	case <-runB.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the failed B run never completed")
	}
	cB.mu.Lock()
	retiredB := cB.run == nil
	cB.mu.Unlock()
	testassert.Require(t, retiredB, "the failed B run slot was not retired before A's final closer released")
	waited := make(chan error, 1)
	go func() { waited <- race.h.Wait(context.Background()) }()
	race.cancel()
	close(race.aCloseGate)
	waitErr := <-waited
	testassert.Require(t, waitErr != nil && errors.Is(waitErr, race.errA) && errors.Is(waitErr, race.cleanupErr),
		"Wait = %v, want both the first publication cause and the joined cleanup cause", waitErr)
	testassert.Require(t, !errors.Is(waitErr, race.errB), "Wait = %v, want the later publication cause never to replace the first", waitErr)
	rec := settledOperation(t, race.store, race.sessionB, race.bOperationID)
	testassert.Require(t, rec.State.Status == OperationRunning, "later operation = %+v, want its marked publication failure retained for recovery", rec.State)
	if race.aClosed != nil {
		select {
		case <-race.aClosed:
		default:
			t.Fatal("Wait returned before the manual execution disposal")
		}
	}
}

// TestWaitJoinsRetainedPublicationAndCleanupBeforeDisposal proves the Wait
// row: the first publication failure that stopped admitted work is retained
// before the blocking closer, a later execution's publication failure never
// replaces it, Wait joins the execution cleanup cause, and Wait returns only
// after that execution has actually disposed.
func TestWaitJoinsRetainedPublicationAndCleanupBeforeDisposal(t *testing.T) {
	store := emptyStore(t)
	scriptA := newModelScript()
	errA := &testCause{text: "first publication failure"}
	errB := &testCause{text: "later publication failure"}
	cleanupErr := &testCause{text: "blocked closer broke"}
	var race waitPublicationRace
	race = newWaitPublicationRace(t, store, "", func(context.Context, PreparationRequest) (PreparedExecution, error) {
		exec := effectExecution(scriptA.model, nil)
		exec.Close = func() error {
			race.aCloseArrived <- struct{}{}
			<-race.aCloseGate
			return race.cleanupErr
		}
		return preparedExecuting(exec), nil
	}, errA, errB, cleanupErr)

	// The conversation model-result publication is selected by its exact
	// Session/Operation key and by the decoded prior/proposed Operation state:
	// the prior state holds the committed conversation EffectModel reservation,
	// and the proposed state clears that same effect. The reserved assistant
	// entry has already been inserted when this replace hook fires.
	nonmatching := 0
	store.replaceHook = func(key RegisterKey, payload json.RawMessage) error {
		if key.Kind != RegisterOperation {
			nonmatching++
			return nil
		}
		var target error
		switch {
		case key.SessionID == race.sessionA && key.OperationID == "op-1":
			target = race.errA
		case key.SessionID == race.sessionB && key.OperationID == "op-b":
			target = race.errB
		default:
			nonmatching++
			return nil
		}
		prior, err := store.readRegisterLocked(key)
		if err != nil {
			t.Errorf("read prior operation register %v: %v", key, err)
			return err
		}
		current, err := decodeOperationRegister(prior)
		if err != nil {
			t.Errorf("decode prior operation register %v: %v", key, err)
			return err
		}
		proposed, err := decodeOperationRegister(Register{Key: key, Payload: payload})
		if err != nil {
			t.Errorf("decode proposed operation register %v: %v", key, err)
			return err
		}
		if current.State.ActiveEffect == nil || current.State.ActiveEffect.Kind != EffectModel ||
			current.State.ActiveEffect.Purpose != InvocationPurposeConversation ||
			current.State.ActiveEffect.ResultEntryID == "" || proposed.State.ActiveEffect != nil {
			nonmatching++
			return nil
		}
		entries, err := store.readEntriesLocked(key.SessionID, 0)
		if err != nil {
			t.Errorf("read entries for %v: %v", key, err)
			return err
		}
		inserted := false
		for _, entry := range entries {
			if entry.Kind == EntryAssistant && entry.OperationID == key.OperationID &&
				entry.ID == current.State.ActiveEffect.ResultEntryID {
				inserted = true
				break
			}
		}
		if !inserted {
			t.Errorf("conversation result %v fired before reserved assistant entry %q was inserted", key, current.State.ActiveEffect.ResultEntryID)
			return fmt.Errorf("reserved assistant entry %q was not inserted", current.State.ActiveEffect.ResultEntryID)
		}
		return fmt.Errorf("%w: %w", ErrStorage, target)
	}

	race.bOperationID = "op-b"
	race.startA = func() {
		_, err := submitText(t, race.h, race.sessionA, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
		<-scriptA.arrived
		scriptA.releaseGate()
	}
	runWaitPublicationRace(race)
	testassert.Require(t, nonmatching > 0, "the keyed conversation hook observed no nonmatching replacement")
}

// testCause is one distinct comparable failure identity for the Wait joins.
type testCause struct{ text string }

func (c *testCause) Error() string { return c.text }

// TestToolOutputShapingCannotChangeKnowledge proves shaping is text only: a
// pure output hook over an unknown executor outcome changes the model-facing
// text base while the stored knowledge stays the producer's declared unknown.
func TestToolOutputShapingCannotChangeKnowledge(t *testing.T) {
	rounds := 0
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		rounds++
		if rounds == 1 {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		return completedTurnStream(), nil
	}
	prepared := preparedExecuting(Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		inv := invocationExecution(modelFn, func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
				return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "raw failure"}, Evidence: ExecutionUnknown}
			}}
		})
		inv.ToolOutputHooks = []ToolOutputHook{{ID: "shape.one", ExtensionID: "shape-ext", Run: func(_ context.Context, in ToolOutputInput) (string, error) {
			testassert.Equal(t, in.Outcome.Evidence, ExecutionUnknown)
			return in.Content + " (shaped)", nil
		}}}
		return inv, nil
	}})
	prepared.Capture = testCapture()
	h, store, c, sessionID := newAdmittedHarness(t, context.Background(), prepared)
	testassert.NoError(t, executeDirect(h, c, h.ctx, testOpID, prepared))
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var result *toolResultEntry
	for i := range graph.Entries {
		if graph.Entries[i].ToolResult != nil {
			result = graph.Entries[i].ToolResult
		}
	}
	testassert.Require(t, result != nil && result.Evidence == ExecutionUnknown && result.Status == model.ResultError,
		"result = %+v, want the producer's unknown knowledge unchanged by shaping", result)
	testassert.Require(t, result.Content == unknownToolResultPrefix+"\n\nraw failure (shaped)" && result.OriginalContent == "raw failure" && result.ShapingFailure == "",
		"result = %+v, want the shaped base rendered once under the producer's unknown prefix", result)
}

// admitPrepared admits one Operation with the actual prepared execution and
// returns the committed record plus the adopted prepared execution.
func admitPrepared(t *testing.T, h *Harness, c *coordinator, sessionID, operationID string, prepared PreparedExecution) (OperationRecord, PreparedExecution) {
	t.Helper()
	release, err := h.reserve(context.Background(), c)
	testassert.NoError(t, err)
	defer release()
	rec, adopted, disposition, err := h.admitReserved(context.Background(), c, admissionRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Kind:        RequestKindMessage,
		Origin:      InputOriginUser,
		Content:     admissionContent("hello"),
	})
	testassert.NoError(t, err)
	testassert.Equal(t, disposition, DispositionAdmitted)
	testassert.Require(t, adopted != nil, "admission did not transfer the prepared execution")
	return rec, *adopted
}

// TestExecutionAdoptsCommittedCapture proves capture ownership: the execution
// dispatches the capture committed with the Operation, never a producer value
// reread from the prepared execution. A preparation that mutates its retained
// capture containers while the opener runs cannot change the dispatch.
func TestExecutionAdoptsCommittedCapture(t *testing.T) {
	capture := testCapture()
	var sent model.Request
	mutated := false
	exec := Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		return InvocationExecution{
			Model: func(_ context.Context, request model.Request) (model.Stream, error) {
				sent = request
				return completedTurnStream(), nil
			},
			Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
			NormalizeTool: objectNormalize,
		}, nil
	}}
	prepared := preparedExecutionFor(capture, func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})
	store := emptyStore(t)
	// The producer mutates its retained capture containers during admission
	// publication, after the Harness-owned capture copy was taken.
	store.entryHook = func(draft EntryDraft) error {
		if draft.Kind == EntryInput && draft.OperationID == testOpID {
			capture.Tools[0].Name = "mutated"
			capture.SystemPrompt = "mutated"
			mutated = true
		}
		return nil
	}
	h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return prepared, nil
	})
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	c := leaseCoordinator(t, h, session.Identity.SessionID)
	rec, adopted := admitPrepared(t, h, c, session.Identity.SessionID, testOpID, prepared)
	testassert.NoError(t, executeDirect(h, c, h.ctx, rec.Admission.OperationID, adopted))
	testassert.Require(t, mutated, "the producer mutation never ran")
	testassert.Require(t, len(sent.Tools) == 1 && sent.Tools[0].Name == "echo", "model advertisement = %+v, want the committed capture's tool, never the mutated producer value", sent.Tools)
	testassert.Require(t, sent.Messages[0].TextContent() == "system", "model prompt = %q, want the committed capture's prompt, never the mutated producer value", sent.Messages[0].TextContent())
}

// TestPreparedPlanSnapshotFreezesMutableData proves plan ownership: the
// permission pairs and immediate outcome are frozen immediately after
// acquisition, so a permission evaluator that synchronously mutates the
// producer-retained containers cannot change evaluation, the authorized
// pairs or the committed result/metadata.
func TestPreparedPlanSnapshotFreezesMutableData(t *testing.T) {
	t.Run("permission pairs are frozen before evaluation", func(t *testing.T) {
		rounds := 0
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			rounds++
			if rounds == 1 {
				return completedTurnStream(testToolCall("call-1")), nil
			}
			return completedTurnStream(), nil
		}
		permissions := []PermissionRequest{{Permission: PermissionCommandRun, Target: "original"}}
		var evaluated []PermissionRequest
		exec := effectExecution(modelFn, func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: permissions, Execute: func(context.Context) ToolOutcome {
				return ToolOutcome{Evidence: ExecutionObserved, Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
			}}
		})
		exec.Permissions = permissionEvaluatorFunc(func(_ context.Context, action PreparedAction) (PermissionDecision, error) {
			evaluated = append([]PermissionRequest(nil), action.Permissions...)
			permissions[0] = PermissionRequest{Permission: PermissionFileWrite, Target: "/etc/passwd"}
			return PermissionAllow, nil
		})
		prepared := preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		})
		store := emptyStore(t)
		h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
			return prepared, nil
		})
		session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
		testassert.NoError(t, err)
		c := leaseCoordinator(t, h, session.Identity.SessionID)
		rec, adopted := admitPrepared(t, h, c, session.Identity.SessionID, testOpID, prepared)
		testassert.NoError(t, executeDirect(h, c, h.ctx, rec.Admission.OperationID, adopted))
		want := []PermissionRequest{{Permission: PermissionCommandRun, Target: "original"}}
		testassert.Require(t, reflect.DeepEqual(evaluated, want), "evaluated pairs = %+v, want the frozen acquisition %+v", evaluated, want)
	})

	t.Run("immediate content and metadata are frozen at acceptance", func(t *testing.T) {
		rounds := 0
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			rounds++
			if rounds == 1 {
				return completedTurnStream(testToolCall("call-1")), nil
			}
			return completedTurnStream(), nil
		}
		permissions := []PermissionRequest{{Permission: PermissionCommandRun, Target: "original"}}
		metadata := json.RawMessage(`{"k":1}`)
		immediate := &ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "original"}, Metadata: metadata}
		exec := effectExecution(modelFn, func(context.Context, model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: permissions, Immediate: immediate}
		})
		exec.Permissions = permissionEvaluatorFunc(func(context.Context, PreparedAction) (PermissionDecision, error) {
			immediate.Result.Content = "mutated"
			metadata[0] = 'X'
			return PermissionAllow, nil
		})
		prepared := preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		})
		store := emptyStore(t)
		h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
			return prepared, nil
		})
		session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
		testassert.NoError(t, err)
		c := leaseCoordinator(t, h, session.Identity.SessionID)
		rec, adopted := admitPrepared(t, h, c, session.Identity.SessionID, testOpID, prepared)
		testassert.NoError(t, executeDirect(h, c, h.ctx, rec.Admission.OperationID, adopted))
		graph, err := validateFixture(t, store, session.Identity.SessionID)
		testassert.NoError(t, err)
		var result *toolResultEntry
		for i := range graph.Entries {
			if graph.Entries[i].ToolResult != nil {
				result = graph.Entries[i].ToolResult
			}
		}
		testassert.Require(t, result != nil && result.Status == model.ResultSuccess && result.Content == "original", "result = %+v, want the frozen immediate content, never the evaluator mutation", result)
		testassert.Require(t, string(result.Metadata) == `{"k":1}`, "metadata = %s, want the frozen immediate metadata", result.Metadata)
	})
}

// permissionEvaluatorFunc adapts one function to PermissionEvaluator.
type permissionEvaluatorFunc func(context.Context, PreparedAction) (PermissionDecision, error)

func (f permissionEvaluatorFunc) Evaluate(ctx context.Context, action PreparedAction) (PermissionDecision, error) {
	return f(ctx, action)
}

// multiPassStrategy plans two maintenance models, then ends at none; it never
// commits or closes anything itself.
type multiPassStrategy struct {
	plans  int
	passes int
}

func (s *multiPassStrategy) Plan(_ PlanInput) (PlanOutput, error) {
	s.plans++
	return s.modelPlan()
}

func (s *multiPassStrategy) Consume(_ ConsumeInput) (PlanOutput, error) {
	s.passes++
	if s.passes < 2 {
		return s.modelPlan()
	}
	return PlanOutput{Kind: PlanNone}, nil
}

func (s *multiPassStrategy) modelPlan() (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: fmt.Sprintf("pass %d", s.passes+1)}}}}},
	}}, nil
}

// TestAdmittedMultiModelStrategyNoneThenOrdinaryTerminal proves the complete
// replacement-strategy qualification through the actual admitted owner: two
// maintenance models run under their own producers, the procedure ends at
// none, and the ordinary conversation terminal publishes the accumulated
// ordered facts exactly once with distinct actual IDs and absent/zero report
// distinctions preserved.
func TestAdmittedMultiModelStrategyNoneThenOrdinaryTerminal(t *testing.T) {
	capture := triggerUsageCapture(4)
	capture.Strategy = StrategyCapture{ID: "test.multipass/v1"}
	h, store, c, sessionID := newEffectHarness(t, nil)
	// The reported conversation measurement that triggers automatic
	// maintenance.
	{
		seedPrepared := preparedExecuting(effectExecution(func(context.Context, model.Request) (model.Stream, error) {
			return streamOf(turnDelta("stop", "seed"), usageDelta(model.Usage{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 1})), nil
		}, nil))
		testassert.NoError(t, executeDirect(h, c, h.ctx, testOpID, seedPrepared))
	}
	const subject = "op-2"
	strategy := &multiPassStrategy{}
	maintenanceCalls := 0
	var events []string
	exec := Execution{
		Strategy: &StrategyBinding{ID: "test.multipass/v1", Strategy: strategy},
		OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
			if request.Purpose == InvocationPurposeMaintenance {
				maintenanceCalls++
				pass := maintenanceCalls
				var usage *model.Usage
				if pass == 1 {
					usage = &model.Usage{InputTokens: 5}
				}
				return InvocationExecution{
					Model: func(context.Context, model.Request) (model.Stream, error) {
						if usage != nil {
							return streamOf(turnDelta("stop", fmt.Sprintf("pass %d", pass)), usageDelta(*usage)), nil
						}
						return streamOf(turnDelta("stop", fmt.Sprintf("pass %d", pass))), nil
					},
					Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
					NormalizeTool: objectNormalize,
					Close: func() error {
						events = append(events, fmt.Sprintf("maintenance-close-%d", pass))
						return nil
					},
				}, nil
			}
			return InvocationExecution{
				Model: func(context.Context, model.Request) (model.Stream, error) {
					events = append(events, "conversation-dispatch")
					return streamOf(turnDelta("stop", "final answer"), usageDelta(model.Usage{InputTokens: 2, OutputTokens: 1})), nil
				},
				Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
				NormalizeTool: objectNormalize,
			}, nil
		},
	}
	prepared := preparedExecutionFor(capture, func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})
	h.deps.Prepare = func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return prepared, nil
	}
	subjectRecord, adopted := admitPrepared(t, h, c, sessionID, subject, prepared)
	testassert.NoError(t, executeDirect(h, c, h.ctx, subjectRecord.Admission.OperationID, adopted))
	testassert.Equal(t, maintenanceCalls, 2)

	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var settlement *operationSettlementEntry
	for i := range graph.Entries {
		if entry := graph.Entries[i].Settlement; entry != nil && entry.OperationID == subject {
			settlement = entry
		}
	}
	testassert.Require(t, settlement != nil && settlement.Status == OperationSuccess && settlement.CompactionUsage == false, "settlement = %+v, want the ordinary conversation success terminal", settlement)
	testassert.Require(t, len(settlement.Facts) == 2, "settlement facts = %+v, want both maintenance producers in incurred order", settlement.Facts)
	first, second := settlement.Facts[0], settlement.Facts[1]
	testassert.Require(t, first.RequestID != "" && second.RequestID != "" && first.RequestID != second.RequestID, "fact producers = %q/%q, want distinct actual maintenance IDs", first.RequestID, second.RequestID)
	testassert.Require(t, first.Model == testCompactCapture().Model && second.Model == testCompactCapture().Model, "fact models = %+v/%+v, want the compact model", first.Model, second.Model)
	testassert.Require(t, first.Usage != nil && *first.Usage == (UsageCount{InputTokens: 5}), "first fact usage = %+v, want the reported counts", first.Usage)
	testassert.Require(t, second.Usage == nil, "second fact usage = %+v, want the absent report kept absent", second.Usage)
	testassert.Require(t, first.Input[0].TextContent() == "pass 1" && second.Input[0].TextContent() == "pass 2", "fact inputs = %+v, want the actual ordered maintenance requests", settlement.Facts)
	for _, entry := range graph.Entries {
		testassert.Require(t, entry.Compaction == nil, "a none-ending procedure published a checkpoint")
	}
	// Usage charges exactly once: the conversation's own reports plus the one
	// reporting maintenance producer.
	rec, err := h.ReadOperation(context.Background(), sessionID, subject)
	testassert.NoError(t, err)
	wantUsage := UsageTotals{ByModel: []ModelUsage{
		{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 5}},
		{Model: testModelRef(), Usage: UsageCount{InputTokens: 2, OutputTokens: 1}},
	}}
	testassert.Require(t, usageTotalsEqual(rec.State.Usage, wantUsage), "operation usage = %+v, want the exactly-once charge", rec.State.Usage)
	wantSession := UsageTotals{ByModel: []ModelUsage{
		{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 5}},
		{Model: testModelRef(), Usage: UsageCount{InputTokens: 12, CachedInputTokens: 2, OutputTokens: 2}},
	}}
	testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, wantSession), "session usage = %+v, want the subject charge beside the seed's own report", graph.Session.State.Usage)

	// The strategy performed exactly one plan and two consumes over the two
	// maintenance models before the conversation continued.
	testassert.Require(t, strategy.plans == 1 && strategy.passes == 2, "strategy callbacks = %d plans / %d consumes, want one plan and two consumes", strategy.plans, strategy.passes)
	testassert.Require(t, reflect.DeepEqual(events, []string{"maintenance-close-1", "maintenance-close-2", "conversation-dispatch"}),
		"disposal order = %v, want every automatic final maintenance stage closed before the conversation dispatch", events)
}

// TestWaitRetainsManualPublicationBeforeLaterExecution proves the F9 schedule
// on the actual owners: a second Session is admitted and parked first, the
// manual checkpoint publication of the first Session fails and its final
// maintenance closer blocks after the failure is latched, the later
// Session's own result publication fails and retires, and only then is the
// manual closer released. Wait returns the first cause and the joined cleanup
// cause, never the later execution's cause, and only after disposal.
func TestWaitRetainsManualPublicationBeforeLaterExecution(t *testing.T) {
	capture := testCapture()
	capture.Strategy = StrategyCapture{ID: "test.manual/v1"}
	store := newCompactOperationStore(t, capture)

	errA := fmt.Errorf("%w: manual checkpoint failed", ErrStorage)
	errB := fmt.Errorf("%w: later result failed", ErrStorage)
	cleanupErr := errors.New("manual closer broke")
	store.entryHook = func(draft EntryDraft) error {
		switch {
		case draft.Kind == EntryCompaction && draft.OperationID == "compact-op":
			return errA
		case draft.Kind == EntryAssistant && draft.OperationID == "op-b":
			return errB
		}
		return nil
	}
	race := newWaitPublicationRace(t, store, testSessionID, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return PreparedExecution{}, errors.New("unexpected preparation")
	}, errA, errB, cleanupErr)
	cA := leaseCoordinator(t, race.h, testSessionID)
	aClosed := make(chan struct{}, 1)
	execA := Execution{
		Strategy: &StrategyBinding{ID: "test.manual/v1", Strategy: callbackStrategy{}},
		OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
			return InvocationExecution{
				Model: func(context.Context, model.Request) (model.Stream, error) {
					return summaryStream("manual summary", model.Usage{InputTokens: 1}), nil
				},
				Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
				NormalizeTool: objectNormalize,
				Close: func() error {
					race.aCloseArrived <- struct{}{}
					<-race.aCloseGate
					return race.cleanupErr
				},
			}, nil
		},
		Close: func() error {
			aClosed <- struct{}{}
			return nil
		},
	}
	preparedA := preparedExecutionFor(capture, func(context.Context, OperationAdmission) (Execution, error) {
		return execA, nil
	})
	race.aClosed = aClosed
	race.bOperationID = "op-b"
	race.startA = func() {
		race.h.startExecution(cA, "compact-op", preparedA)
	}
	runWaitPublicationRace(race)
}

// TestWaitRetainsAutomaticPublicationBeforeLaterExecution proves the same
// schedule on the automatic owner: the first Session's automatic maintenance
// checkpoint publication fails and its final maintenance closer blocks after
// the failure is latched, the later Session's own result publication fails and
// retires, and only then is the maintenance closer released. Wait returns the
// first cause and the joined cleanup cause, never the later execution's cause.
func TestWaitRetainsAutomaticPublicationBeforeLaterExecution(t *testing.T) {
	capture := testCapture()
	capture.Strategy = StrategyCapture{ID: "test.automatic/v1"}
	store := emptyStore(t)

	errA := fmt.Errorf("%w: automatic checkpoint failed", ErrStorage)
	errB := fmt.Errorf("%w: later result failed", ErrStorage)
	cleanupErr := errors.New("automatic closer broke")
	store.entryHook = func(draft EntryDraft) error {
		switch {
		case draft.Kind == EntryCompaction && draft.OperationID == "op-1":
			return errA
		case draft.Kind == EntryAssistant && draft.OperationID == "op-b":
			return errB
		}
		return nil
	}
	var race waitPublicationRace
	race = newWaitPublicationRace(t, store, "", func(context.Context, PreparationRequest) (PreparedExecution, error) {
		execA := Execution{
			Strategy: &StrategyBinding{ID: "test.automatic/v1", Strategy: callbackStrategy{}},
			OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
				if request.Purpose == InvocationPurposeMaintenance {
					return InvocationExecution{
						Model: func(context.Context, model.Request) (model.Stream, error) {
							return summaryStream("automatic summary", model.Usage{InputTokens: 1}), nil
						},
						Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
						NormalizeTool: objectNormalize,
						Close: func() error {
							race.aCloseArrived <- struct{}{}
							<-race.aCloseGate
							return race.cleanupErr
						},
					}, nil
				}
				return InvocationExecution{
					Model:         func(context.Context, model.Request) (model.Stream, error) { return completedTurnStream(), nil },
					Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
					NormalizeTool: objectNormalize,
				}, nil
			},
		}
		return preparedExecutionFor(capture, func(context.Context, OperationAdmission) (Execution, error) {
			return execA, nil
		}), nil
	}, errA, errB, cleanupErr)
	race.bOperationID = "op-b"
	race.startA = func() {
		_, err := submitText(t, race.h, race.sessionA, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	runWaitPublicationRace(race)
}
