// Bound callback and plan-resource contracts: producer-issued callback
// identities, plan release on every acquired-plan exit, and declared tool
// execution knowledge.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// recordedCallback is one captured producer-issued callback identity.
type recordedCallback struct {
	kind CallbackKind
	info CallbackInfo
}

// callbackLog captures every callback identity one fixture boundary observes.
type callbackLog struct {
	t     *testing.T
	items []recordedCallback
}

func (l *callbackLog) record(ctx context.Context, kind CallbackKind) CallbackInfo {
	l.t.Helper()
	info, ok := CallbackInfoFor(ctx)
	testassert.Require(l.t, ok, "callback %s carries no producer-issued identity", kind)
	testassert.Equal(l.t, info.Kind, kind)
	l.items = append(l.items, recordedCallback{kind: kind, info: info})
	return info
}

// find returns the one captured identity of one kind.
func (l *callbackLog) find(kind CallbackKind) (CallbackInfo, bool) {
	var found CallbackInfo
	count := 0
	for _, item := range l.items {
		if item.kind == kind {
			found = item.info
			count++
		}
	}
	return found, count == 1
}

// findAll returns every captured identity of one kind in order.
func (l *callbackLog) findAll(kind CallbackKind) []CallbackInfo {
	var out []CallbackInfo
	for _, item := range l.items {
		if item.kind == kind {
			out = append(out, item.info)
		}
	}
	return out
}

// callbackStrategy is the two-pass replacement policy of the identity
// fixture: one tool-free model plan folds into one authored checkpoint.
type callbackStrategy struct {
	log *callbackLog
}

func (s callbackStrategy) Plan(ctx context.Context, _ PlanInput) (PlanOutput, error) {
	s.log.record(ctx, CallbackStrategyPlan)
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "one pass"}}}}},
	}}, nil
}

func (s callbackStrategy) Consume(ctx context.Context, in ConsumeInput) (PlanOutput, error) {
	s.log.record(ctx, CallbackStrategyConsume)
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

// TestCallbackInfoMatchesActualBoundIdentities proves the callback identity
// row end to end over one real execution: every behavioral boundary receives
// the actual stored request/pending-call/effect identities of its own
// invocation, only tool execution carries accepted authority, strategy
// callbacks use their own maintenance producer, and the accepted model stream
// stays live until its real Close.
func TestCallbackInfoMatchesActualBoundIdentities(t *testing.T) {
	log := &callbackLog{t: t}
	capture := testCapture()
	capture.Strategy = StrategyCapture{ID: "test.callback/v1"}
	conversationRounds := 0
	var stream *liveCheckStream
	var closeOrder []string
	exec := Execution{
		Strategy: &StrategyBinding{ID: "test.callback/v1", Strategy: callbackStrategy{log: log}},
		OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
			if request.Purpose == InvocationPurposeMaintenance {
				return InvocationExecution{
					Model: func(ctx context.Context, _ model.Request) (model.Stream, error) {
						log.record(ctx, CallbackModelAttempt)
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
					log.record(ctx, CallbackModelAttempt)
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
				Tool: func(ctx context.Context, call model.ToolCall) PreparedTool {
					log.record(ctx, CallbackToolPrepare)
					return PreparedTool{
						Permissions: fixturePermission,
						Execute: func(ctx context.Context) ToolOutcome {
							log.record(ctx, CallbackToolExecute)
							return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}, Evidence: ExecutionObserved}
						},
						Release: func(ctx context.Context) error {
							log.record(ctx, CallbackToolRelease)
							return nil
						},
					}
				},
				NormalizeTool: func(ctx context.Context, call model.ToolCall) (json.RawMessage, error) {
					log.record(ctx, CallbackNormalize)
					return objectNormalize(ctx, call)
				},
				RequestHooks: []RequestHook{
					{ID: "req.pure", ExtensionID: "req-ext", Mode: HookModePure, Run: func(ctx context.Context, _ RequestHookInput) (RequestConsequence, error) {
						log.record(ctx, CallbackRequestHook)
						return RequestConsequence{}, nil
					}},
					{ID: "req.effect", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(ctx context.Context, _ RequestHookInput) (RequestConsequence, error) {
						log.record(ctx, CallbackRequestHook)
						return RequestConsequence{}, nil
					}},
				},
				ToolHooks: []ToolArgumentsHook{
					{ID: "arg.pure", ExtensionID: "arg-ext", Mode: HookModePure, Run: func(ctx context.Context, call model.ToolCall) (json.RawMessage, error) {
						log.record(ctx, CallbackArgumentHook)
						return model.CloneRaw(call.Arguments), nil
					}},
					{ID: "arg.effect", ExtensionID: "arg-ext", Mode: HookModeEffectful, Run: func(ctx context.Context, call model.ToolCall) (json.RawMessage, error) {
						log.record(ctx, CallbackArgumentHook)
						return model.CloneRaw(call.Arguments), nil
					}},
				},
				ToolOutputHooks: []ToolOutputHook{
					{ID: "out.one", ExtensionID: "out-ext", Run: func(ctx context.Context, in ToolOutputInput) (string, error) {
						log.record(ctx, CallbackToolOutput)
						return in.Content + " shaped", nil
					}},
				},
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
	h, store, c, sessionID := newAdmittedHarness(t, context.Background(), prepared)
	err := executeDirect(h, c, h.ctx, testOpID, prepared)
	testassert.NoError(t, err)

	info, ok := CallbackInfoFor(context.Background())
	testassert.Require(t, !ok && info.Kind == "", "a caller context outside the execution carried a callback identity")

	var conversationAttempts []CallbackInfo
	var maintenanceAttempts []CallbackInfo
	for _, attempt := range log.findAll(CallbackModelAttempt) {
		if attempt.Purpose == InvocationPurposeMaintenance {
			maintenanceAttempts = append(maintenanceAttempts, attempt)
			continue
		}
		conversationAttempts = append(conversationAttempts, attempt)
	}
	testassert.Require(t, len(conversationAttempts) >= 1 && len(maintenanceAttempts) >= 1, "model attempts = %+v, want the conversation rounds and the maintenance producers", log.items)
	maintenanceAttempt := maintenanceAttempts[len(maintenanceAttempts)-1]
	conversationAttempt := conversationAttempts[0]
	for _, attempt := range conversationAttempts {
		testassert.Equal(t, attempt.InvocationID, conversationAttempt.InvocationID)
		testassert.Equal(t, attempt.Purpose, InvocationPurposeConversation)
		testassert.Equal(t, attempt.SessionID, sessionID)
		testassert.Equal(t, attempt.OperationID, testOpID)
		testassert.Require(t, attempt.RequestID != "" && attempt.EffectID != "", "conversation attempt = %+v, want its producing request and reserved effect identity", attempt)
	}
	testassert.Require(t, maintenanceAttempt.InvocationID != "" && maintenanceAttempt.InvocationID != conversationAttempt.InvocationID, "maintenance attempt = %+v, want its own issued invocation", maintenanceAttempt)
	testassert.Require(t, maintenanceAttempt.RequestID != "" && maintenanceAttempt.RequestID != conversationAttempt.RequestID, "maintenance request = %q, want the maintenance producer, never the conversation's", maintenanceAttempt.RequestID)

	normalizations := log.findAll(CallbackNormalize)
	testassert.Require(t, len(normalizations) == 1, "normalizations = %+v, want the one producing-assembly normalization", normalizations)
	normalize := normalizations[0]
	testassert.Require(t, normalize.RequestID == conversationAttempt.RequestID && normalize.ToolCallID == "call-1" && normalize.CapabilityID == "echo", "normalize identity = %+v, want the producing conversation request and assembled call", normalize)
	testassert.Require(t, normalize.EffectID == "" && len(normalize.AuthorizedPermissions) == 0, "normalize identity = %+v, want no action/effect grant", normalize)

	var requestHooks []CallbackInfo
	for _, hook := range log.findAll(CallbackRequestHook) {
		if hook.RequestID == conversationAttempt.RequestID {
			requestHooks = append(requestHooks, hook)
		}
	}
	testassert.Require(t, len(requestHooks) == 2, "request hooks = %+v, want the pure and effectful boundaries of the producing round", requestHooks)
	for _, hook := range requestHooks {
		testassert.Require(t, hook.RequestID == conversationAttempt.RequestID && hook.InvocationID == conversationAttempt.InvocationID && hook.ExtensionID == "req-ext", "request hook identity = %+v, want the current conversation request and invocation", hook)
	}
	pureRequest := requestHooks[0]
	effectRequest := requestHooks[1]
	testassert.Require(t, pureRequest.CapabilityID == "req.pure" && pureRequest.EffectID == "", "pure request hook = %+v, want its declared identity and no effect", pureRequest)
	testassert.Require(t, effectRequest.CapabilityID == "req.effect" && effectRequest.EffectID != "", "effectful request hook = %+v, want its own committed reservation", effectRequest)

	argumentHooks := log.findAll(CallbackArgumentHook)
	testassert.Require(t, len(argumentHooks) == 2, "argument hooks = %+v, want the pure and effectful boundaries", argumentHooks)
	for _, hook := range argumentHooks {
		testassert.Require(t, hook.RequestID == conversationAttempt.RequestID && hook.ToolCallID == "call-1" && hook.ExtensionID == "arg-ext", "argument hook identity = %+v, want the producing assistant request and pending call", hook)
	}
	pureArg, effectArg := argumentHooks[0], argumentHooks[1]
	testassert.Require(t, pureArg.CapabilityID == "arg.pure" && pureArg.EffectID == "", "pure argument hook = %+v, want no effect", pureArg)
	testassert.Require(t, effectArg.CapabilityID == "arg.effect" && effectArg.EffectID != "", "effectful argument hook = %+v, want its own committed reservation", effectArg)

	prepare, ok := log.find(CallbackToolPrepare)
	testassert.Require(t, ok, "tool prepare identity missing")
	testassert.Require(t, prepare.RequestID == conversationAttempt.RequestID && prepare.ToolCallID == "call-1" && prepare.CapabilityID == "echo" && prepare.EffectID == "" && len(prepare.AuthorizedPermissions) == 0,
		"tool prepare identity = %+v, want the producing request and pending call with no action grant", prepare)
	execute, ok := log.find(CallbackToolExecute)
	testassert.Require(t, ok, "tool execute identity missing")
	testassert.Require(t, execute.RequestID == prepare.RequestID && execute.ToolCallID == prepare.ToolCallID && execute.CapabilityID == "echo" && execute.EffectID != "",
		"tool execute identity = %+v, want the committed concrete-tool reservation", execute)
	testassert.Require(t, reflect.DeepEqual(execute.AuthorizedPermissions, fixturePermission), "tool execute permissions = %+v, want the accepted authorized pairs", execute.AuthorizedPermissions)
	releaseInfo, ok := log.find(CallbackToolRelease)
	testassert.Require(t, ok, "tool release identity missing")
	testassert.Require(t, releaseInfo.RequestID == execute.RequestID && releaseInfo.ToolCallID == execute.ToolCallID && releaseInfo.EffectID == "" && len(releaseInfo.AuthorizedPermissions) == 0,
		"tool release identity = %+v, want the same attribution with no action grant", releaseInfo)
	output, ok := log.find(CallbackToolOutput)
	testassert.Require(t, ok, "tool output identity missing")
	testassert.Require(t, output.RequestID == execute.RequestID && output.ToolCallID == execute.ToolCallID && output.CapabilityID == "out.one" && output.ExtensionID == "out-ext" && output.EffectID == "" && len(output.AuthorizedPermissions) == 0,
		"tool output identity = %+v, want its actual declared hook identity, the same attribution and no action grant", output)

	plans := log.findAll(CallbackStrategyPlan)
	consumes := log.findAll(CallbackStrategyConsume)
	testassert.Require(t, len(plans) >= 1 && len(plans) == len(consumes), "strategy callbacks = %d plans / %d consumes, want one procedure pair per round", len(plans), len(consumes))
	planInfo, consume := plans[0], consumes[0]
	testassert.Require(t, planInfo.Purpose == InvocationPurposeMaintenance && planInfo.InvocationID == "" && planInfo.RequestID != "" && planInfo.CapabilityID == "test.callback/v1",
		"strategy plan identity = %+v, want the procedure's own producer and no invocation yet", planInfo)
	testassert.Require(t, consume.Purpose == InvocationPurposeMaintenance && consume.InvocationID != "" && consume.RequestID == consume.InvocationID && consume.CapabilityID == "test.callback/v1" && consume.EffectID == "",
		"strategy consume identity = %+v, want the completed maintenance invocation and producer", consume)
	testassert.Require(t, planInfo.RequestID != consume.RequestID, "plan producer = %q, consume producer = %q, want distinct actual producers", planInfo.RequestID, consume.RequestID)
	testassert.Require(t, consume.InvocationID == maintenanceAttempts[0].InvocationID && consume.RequestID == maintenanceAttempts[0].RequestID,
		"strategy consume identity = %+v, want the first completed maintenance invocation %+v", consume, maintenanceAttempts[0])

	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var assistant *assistantEntry
	var toolResult *toolResultEntry
	requestHookIDs := map[string]string{}
	toolHookIDs := map[string]string{}
	for i := range graph.Entries {
		entry := graph.Entries[i]
		switch {
		case entry.Assistant != nil:
			for _, call := range entry.Assistant.ToolCalls {
				if call.ID == "call-1" {
					assistant = entry.Assistant
				}
			}
		case entry.ToolResult != nil:
			toolResult = entry.ToolResult
		case entry.HookResult != nil:
			if entry.HookResult.RequestID != "" {
				requestHookIDs[entry.HookResult.HookID+"|"+entry.HookResult.RequestID] = entry.HookResult.EntryID
			}
			if entry.HookResult.ToolCallID != "" {
				toolHookIDs[entry.HookResult.HookID+"|"+entry.HookResult.ToolCallID] = entry.HookResult.EntryID
			}
		}
	}
	testassert.Require(t, assistant != nil && assistant.RequestID == conversationAttempt.RequestID, "assistant request = %+v, want the producing conversation request %q", assistant, conversationAttempt.RequestID)
	testassert.Require(t, toolResult != nil && toolResult.EntryID == execute.EffectID, "tool result = %+v, want the committed execute reservation %q", toolResult, execute.EffectID)
	testassert.Equal(t, requestHookIDs["req.effect|"+effectRequest.RequestID], effectRequest.EffectID)
	testassert.Equal(t, toolHookIDs["arg.effect|"+effectArg.ToolCallID], effectArg.EffectID)
	testassert.Require(t, stream != nil && stream.closes == 1, "accepted stream closes = %d, want exactly one real Close", stream.closes)
	testassert.Require(t, reflect.DeepEqual(closeOrder, []string{"maintenance-close", "conversation-dispatch", "maintenance-close", "conversation-dispatch", "conversation-close"}),
		"disposal order = %v, want every automatic final maintenance stage closed before its continuation and the conversation invocation closed at completion", closeOrder)
}

// planReleaseRecorder captures one acquired plan's execute and release
// callback identities.
type planReleaseRecorder struct {
	executeCalls int
	executeInfo  CallbackInfo
	releaseCalls int
	releaseInfo  CallbackInfo
}

// TestToolPlanReleaseAndEvidence proves the acquired-plan contract: every
// exit after acquisition registers and runs its release exactly once, after
// the plan's settlement or discard, on a cancellation-free context carrying
// tool_release metadata and no action grant; and one complete ToolOutcome
// reaches the canonical result with the producer's declared knowledge, never
// inferred from the result's shape.
func TestToolPlanReleaseAndEvidence(t *testing.T) {
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
		plan        func(ctx context.Context, cancel context.CancelFunc, rec *planReleaseRecorder, call model.ToolCall) PreparedTool
		failIntent  bool
		failResult  bool
		wantStatus  model.ToolResultStatus
		wantContent string
		wantEvid    ExecutionEvidence
		wantExecute bool
		wantRelease int
	}
	rows := []row{
		{
			name: "immediate success releases after its settlement",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}, Evidence: ExecutionObserved},
					Release: func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultSuccess, wantContent: "done", wantEvid: ExecutionObserved, wantRelease: 1,
		},
		{
			name: "immediate error releases and stays not started",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "boom"}, Evidence: ExecutionNotStarted},
					Release: func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultError, wantContent: "boom", wantEvid: ExecutionNotStarted, wantRelease: 1,
		},
		{
			name: "immediate success with contradictory knowledge is the invalid replacement",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}, Evidence: ExecutionNotStarted},
					Release: func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultError, wantContent: invalidToolResultContent, wantEvid: ExecutionNotStarted, wantRelease: 1,
		},
		{
			name: "malformed plan shape releases before its rejection",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission,
					Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}, Evidence: ExecutionObserved},
					Execute:   func(context.Context) ToolOutcome { return ToolOutcome{} },
					Release:   func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultError, wantContent: invalidToolResultContent, wantEvid: ExecutionNotStarted, wantRelease: 1,
		},
		{
			name: "denied executor releases without running",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Execute: func(context.Context) ToolOutcome { return ToolOutcome{} },
					Release: func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultDenied, wantContent: PermissionDeniedContent, wantEvid: ExecutionNotStarted, wantRelease: 1,
		},
		{
			name: "observed executor success keeps its knowledge",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}, Evidence: ExecutionObserved}
				}, Release: func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultSuccess, wantContent: "ran", wantEvid: ExecutionObserved, wantExecute: true, wantRelease: 1,
		},
		{
			name: "declared not_started executor outcome reaches the canonical result",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "refused before handoff"}, Evidence: ExecutionNotStarted}
				}, Release: func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultError, wantContent: "refused before handoff", wantEvid: ExecutionNotStarted, wantExecute: true, wantRelease: 1,
		},
		{
			name: "declared unknown executor outcome carries the uncertainty prefix",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "may have run"}, Evidence: ExecutionUnknown}
				}, Release: func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultError, wantContent: unknownToolResultPrefix + "\n\n" + "may have run", wantEvid: ExecutionUnknown, wantExecute: true, wantRelease: 1,
		},
		{
			name: "malformed executor result is unknown and releases",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: "another-call", Status: model.ResultSuccess, Content: "ran"}, Evidence: ExecutionObserved}
				}, Release: func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultError, wantContent: unknownToolResultPrefix + "\n\n" + invalidToolResultContent, wantEvid: ExecutionUnknown, wantExecute: true, wantRelease: 1,
		},
		{
			name: "missing executor knowledge is unknown and releases",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
				}, Release: func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultError, wantContent: unknownToolResultPrefix + "\n\n" + invalidToolResultContent, wantEvid: ExecutionUnknown, wantExecute: true, wantRelease: 1,
		},
		{
			name: "success with not_started knowledge is the invalid replacement",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}, Evidence: ExecutionNotStarted}
				}, Release: func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultError, wantContent: unknownToolResultPrefix + "\n\n" + invalidToolResultContent, wantEvid: ExecutionUnknown, wantExecute: true, wantRelease: 1,
		},
		{
			name: "cancellation before start releases after its interrupted settlement",
			plan: func(_ context.Context, cancel context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				cancel()
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "must not run"}, Evidence: ExecutionObserved}
				}, Release: func(context.Context) error { return nil }}
			},
			wantStatus: model.ResultInterrupted, wantContent: interruptedToolResultContent, wantEvid: ExecutionNotStarted, wantRelease: 1,
		},
		{
			name: "tool result publication failure discards the acquired plan with its release",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}, Evidence: ExecutionObserved}
				}, Release: func(context.Context) error { return nil }}
			},
			failResult: true, wantExecute: true, wantRelease: 1,
		},
		{
			name: "failed intent publication discards the acquired plan with its release",
			plan: func(_ context.Context, _ context.CancelFunc, _ *planReleaseRecorder, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}, Evidence: ExecutionObserved}
				}, Release: func(context.Context) error { return nil }}
			},
			failIntent: true,
		},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := &planReleaseRecorder{}
			var planPermissions []PermissionRequest
			var store *graphStorage
			var sessionID string
			resultAttempted, attemptedResultID := false, ""
			preparedRec := preparedExecuting(effectExecution(modelFn(), func(ctx context.Context, call model.ToolCall) PreparedTool {
				plan := tc.plan(ctx, cancel, rec, call)
				planPermissions = append([]PermissionRequest(nil), plan.Permissions...)
				if plan.Execute != nil {
					inner := plan.Execute
					plan.Execute = func(ctx context.Context) ToolOutcome {
						rec.executeCalls++
						rec.executeInfo = recordCallback(ctx, t, CallbackToolExecute)
						return inner(ctx)
					}
				}
				if plan.Release != nil {
					inner := plan.Release
					plan.Release = func(ctx context.Context) error {
						rec.releaseCalls++
						rec.releaseInfo = recordCallback(ctx, t, CallbackToolRelease)
						testassert.Require(t, ctx.Err() == nil, "release context was canceled: %v", ctx.Err())
						testassert.Require(t, len(rec.releaseInfo.AuthorizedPermissions) == 0 && rec.releaseInfo.EffectID == "", "release carried action/effect authority: %+v", rec.releaseInfo)
						graph, gerr := validateFixture(t, store, sessionID)
						testassert.NoError(t, gerr)
						var published *toolResultEntry
						for i := range graph.Entries {
							if graph.Entries[i].ToolResult != nil && graph.Entries[i].ToolResult.ToolCallID == call.ID {
								published = graph.Entries[i].ToolResult
							}
						}
						switch {
						case tc.failIntent:
							testassert.Require(t, published == nil, "release saw a tool result after a failed intent: %+v", published)
						case tc.failResult:
							testassert.Require(t, resultAttempted && published == nil, "release ran before the result publication attempt or saw a published result: attempted=%v result=%+v", resultAttempted, published)
						default:
							testassert.Require(t, published != nil && published.Status == tc.wantStatus && published.Content == tc.wantContent && published.Evidence == tc.wantEvid,
								"release observed result %+v, want the settled %s/%q/%s", published, tc.wantStatus, tc.wantContent, tc.wantEvid)
						}
						return inner(ctx)
					}
				}
				return plan
			}))
			preparedRec.Capture = testCapture()
			h, st, c, sid := newAdmittedHarness(t, context.Background(), preparedRec)
			store, sessionID = st, sid
			if tc.failIntent {
				store.replaceHook = func(key RegisterKey, payload json.RawMessage) error {
					if key.Kind != RegisterOperation {
						return nil
					}
					next, derr := decodeOperationRegister(Register{Key: key, Payload: payload})
					if derr == nil && next.State.ActiveEffect != nil && next.State.ActiveEffect.Kind == EffectTool && next.State.ActiveEffect.ToolCallID == "call-1" {
						return fmt.Errorf("injected intent failure: %w", ErrStorage)
					}
					return nil
				}
			}
			if tc.failResult {
				store.entryHook = func(draft EntryDraft) error {
					if draft.Kind == EntryToolResult && draft.OperationID == testOpID {
						resultAttempted = true
						attemptedResultID = draft.ID
						return fmt.Errorf("injected tool result failure: %w", ErrStorage)
					}
					return nil
				}
			}
			err := executeDirect(h, c, ctx, testOpID, preparedRec)
			if tc.failIntent {
				testassert.Require(t, err != nil && isPublicationFailure(err), "execute = %v, want the marked intent publication failure", err)
				testassert.Equal(t, rec.executeCalls, 0)
				testassert.Equal(t, rec.releaseCalls, 1)
				testassert.Require(t, !resultAttempted, "the failed intent row attempted a result publication")
				return
			}
			if tc.failResult {
				testassert.Require(t, err != nil && isPublicationFailure(err), "execute = %v, want the marked tool result publication failure", err)
				testassert.Equal(t, rec.executeCalls, 1)
				testassert.Equal(t, rec.releaseCalls, 1)
				testassert.Require(t, resultAttempted && attemptedResultID != "", "the tool result publication was never attempted")
				graph, verr := validateFixture(t, store, sessionID)
				testassert.NoError(t, verr)
				for i := range graph.Entries {
					testassert.Require(t, graph.Entries[i].ToolResult == nil, "a discarded plan published tool result %s", graph.Entries[i].Envelope.ID)
					testassert.Require(t, graph.Entries[i].Settlement == nil, "the failed result publication fabricated a terminal write")
				}
				state := operationStateOf(t, graph, testOpID)
				testassert.Require(t, state.Status == OperationRunning && state.ActiveEffect != nil && state.ActiveEffect.Kind == EffectTool && state.ActiveEffect.ToolCallID == "call-1",
					"operation state = %+v, want the committed tool intent retained for recovery", state)
				testassert.Require(t, len(state.PendingToolCalls) == 1 && state.PendingToolCalls[0].CallID == "call-1" && state.ActiveEffect.ResultEntryID == attemptedResultID,
					"pending state = %+v, want the discarded plan's reserved result identity %s retained", state.PendingToolCalls, attemptedResultID)
				return
			}
			testassert.NoError(t, err)
			testassert.Equal(t, rec.executeCalls > 0, tc.wantExecute)
			testassert.Equal(t, rec.releaseCalls, tc.wantRelease)

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
			if tc.wantExecute && planPermissions != nil {
				testassert.Require(t, reflect.DeepEqual(rec.executeInfo.AuthorizedPermissions, planPermissions), "execute permissions = %+v, want the accepted authorized pairs %+v", rec.executeInfo.AuthorizedPermissions, planPermissions)
			}
			if rec.releaseCalls == 1 {
				testassert.Require(t, rec.releaseInfo.Kind == CallbackToolRelease && rec.releaseInfo.EffectID == "" && len(rec.releaseInfo.AuthorizedPermissions) == 0,
					"release identity = %+v, want no action/effect grant", rec.releaseInfo)
			}
		})
	}
}

// recordCallback captures one callback identity without failing the fixture.
func recordCallback(ctx context.Context, t *testing.T, kind CallbackKind) CallbackInfo {
	t.Helper()
	info, ok := CallbackInfoFor(ctx)
	testassert.Require(t, ok && info.Kind == kind, "callback %s carries identity %+v (present %v)", kind, info, ok)
	return info
}

// TestToolPlanCleanupFailureIsRetained proves a release failure never rewrites
// the settled result and is retained through Wait alongside the storage cause
// channel.
func TestToolPlanCleanupFailureIsRetained(t *testing.T) {
	store := emptyStore(t)
	releaseEntered := make(chan struct{})
	canceledAfter := make(chan struct{})
	releaseChecked := make(chan struct{})
	releaseGate := make(chan struct{})
	releaseErr := errors.New("release broke")
	releaseLive := false
	rounds := 0
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		rounds++
		if rounds == 1 {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		return completedTurnStream(), nil
	}
	prepared := preparedExecuting(effectExecution(modelFn, func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission,
			Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}, Evidence: ExecutionObserved},
			Release: func(ctx context.Context) error {
				close(releaseEntered)
				<-canceledAfter // the Harness/owned work is canceled while the release is parked
				releaseLive = ctx.Err() == nil
				close(releaseChecked)
				<-releaseGate
				return releaseErr
			}}
	}))
	prepared.Capture = testCapture()
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	{
		_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	<-releaseEntered // the immediate result is already canonical; Release is parked on the live cleanup context

	graph, err := validateFixture(t, store, session)
	testassert.NoError(t, err)
	results := 0
	for i := range graph.Entries {
		if graph.Entries[i].ToolResult == nil {
			continue
		}
		results++
		testassert.Require(t, graph.Entries[i].ToolResult.Status == model.ResultSuccess && graph.Entries[i].ToolResult.Content == "done",
			"committed result before release = %+v, want the settled success", graph.Entries[i].ToolResult)
	}
	testassert.Equal(t, results, 1)

	cancel()
	close(canceledAfter)
	<-releaseChecked
	testassert.Require(t, releaseLive, "release context was canceled with the Harness: %v", releaseErr)

	waited := make(chan error, 1)
	go func() { waited <- h.Wait(context.Background()) }()
	select {
	case waitErr := <-waited:
		t.Fatalf("Wait returned while the plan release was parked: %v", waitErr)
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseGate)
	select {
	case waitErr := <-waited:
		testassert.Require(t, errors.Is(waitErr, releaseErr), "Wait = %v, want the retained release cleanup cause", waitErr)
	case <-time.After(10 * time.Second):
		t.Fatal("Wait never returned after the plan release unblocked")
	}

	graph, err = validateFixture(t, store, session)
	testassert.NoError(t, err)
	results = 0
	for i := range graph.Entries {
		if graph.Entries[i].ToolResult == nil {
			continue
		}
		results++
		testassert.Require(t, graph.Entries[i].ToolResult.Status == model.ResultSuccess && graph.Entries[i].ToolResult.Content == "done",
			"committed result after the cleanup failure = %+v, want the success never rewritten", graph.Entries[i].ToolResult)
	}
	testassert.Equal(t, results, 1)
}

// newCompactOperationFixture builds the settled-conversation Session with one
// running compact-kind Operation over the given capture.
func newCompactOperationFixture(t *testing.T, capture ExecutionCapture) (*Harness, *graphStorage, *coordinator, string) {
	t.Helper()
	fixture := validTestGraph()
	stamp := testTime
	fixture.ops[0].State.Status, fixture.ops[0].State.SettledAt = OperationSuccess, &stamp
	fixture.ops[0].State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
	settlement := validSettlementEntry()
	settlement.EntryID = hexID(3)
	fixture.entries = append(fixture.entries, testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime}, settlement: &settlement})
	compact := validOperationRecord()
	compact.Admission.OperationID, compact.Admission.RequestKind, compact.Admission.AdmittedEntry = "compact-op", RequestKindCompact, EntryRef{}
	compact.Admission.Execution = capture
	fixture.ops = append(fixture.ops, compact)
	fixture.session.State.CurrentOperationID = "compact-op"
	store := fixture.storage(t)
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
			reservedAtClose, settledAtClose, failureAtClose := false, false, false
			modelFn := func(context.Context, model.Request) (model.Stream, error) {
				return summaryStream("manual summary", model.Usage{InputTokens: 2}), nil
			}
			exec := Execution{
				Strategy: &StrategyBinding{ID: "test.manual/v1", Strategy: callbackStrategy{log: &callbackLog{t: t}}},
				OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
					return InvocationExecution{
						Model:         modelFn,
						Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
						NormalizeTool: objectNormalize,
						Close: func() error {
							events = append(events, "maintenance-close")
							c.mu.Lock()
							reservedAtClose = c.reserved != nil
							c.mu.Unlock()
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
			testassert.Require(t, reservedAtClose, "the final manual disposal ran outside the completion reservation")
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
		store := emptyStore(t)
		prepared := PreparedExecution{Capture: testCapture(), Open: func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		}}
		h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
			return prepared, nil
		})
		session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
		testassert.NoError(t, err)
		c := leaseCoordinator(t, h, session.Identity.SessionID)
		{
			_, disposition := mustAdmitWithoutExecution(t, h, session.Identity.SessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		{
			res, err := submitText(t, h, session.Identity.SessionID, "op-2", MessageModeQueued, "successor")
			testassert.NoError(t, err)
			testassert.Equal(t, res.Disposition, DispositionQueued)
		}
		err = executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.Require(t, errors.Is(err, ErrStorage), "execute = %v, want the ordinary callback cause retained", err)
		rec, err := h.ReadOperation(context.Background(), session.Identity.SessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == hookErr.Error(),
			"operation state = %+v, want the quiet failure settlement carrying the ordinary cause", rec.State)
		// The buffered successor proceeds instead of being stranded behind a
		// running Operation.
		awaitOperationTerminal(t, store, session.Identity.SessionID, "op-2", OperationFailure)
	})

	t.Run("strategy sibling storage cause settles its own failure", func(t *testing.T) {
		planErr := fmt.Errorf("strategy transport: %w", ErrStorage)
		store := emptyStore(t)
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
		h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
			return prepared, nil
		})
		session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
		testassert.NoError(t, err)
		c := leaseCoordinator(t, h, session.Identity.SessionID)
		{
			_, disposition := mustAdmitWithoutExecution(t, h, session.Identity.SessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		completeErr := executeDirect(h, c, context.Background(), testOpID, prepared)
		testassert.Require(t, errors.Is(completeErr, ErrStorage), "maintenance = %v, want the strategy's own cause", completeErr)
		rec, err := h.ReadOperation(context.Background(), session.Identity.SessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == planErr.Error(),
			"operation state = %+v, want the strategy failure settlement", rec.State)
	})
}

// TestWaitJoinsRetainedPublicationAndCleanupBeforeDisposal proves the Wait
// row: the first publication failure that stopped admitted work is retained
// before the blocking closer, a later execution's publication failure never
// replaces it, Wait joins the execution cleanup cause, and Wait returns only
// after that execution has actually disposed.
func TestWaitJoinsRetainedPublicationAndCleanupBeforeDisposal(t *testing.T) {
	store := emptyStore(t)
	scriptA := newModelScript()
	scriptB := newModelScript()
	var targetMu sync.Mutex
	var target string
	setTarget := func(next string) {
		targetMu.Lock()
		target = next
		targetMu.Unlock()
	}
	var errA, errB, cleanupErr *testCause
	errA = &testCause{text: "first publication failure"}
	errB = &testCause{text: "later publication failure"}
	cleanupErr = &testCause{text: "blocked closer broke"}
	store.txHook = func(step string) error {
		if step != "replace_register" {
			return nil
		}
		targetMu.Lock()
		defer targetMu.Unlock()
		switch target {
		case "A":
			target = ""
			return fmt.Errorf("%w: %w", ErrStorage, errA)
		case "B":
			target = ""
			return fmt.Errorf("%w: %w", ErrStorage, errB)
		}
		return nil
	}
	closeArrived := make(chan struct{}, 1)
	closeGate := make(chan struct{})
	secondClosed := make(chan struct{}, 1)
	preparedFor := func(script *modelScript, parkCloser bool) PreparedExecution {
		exec := effectExecution(script.model, nil)
		exec.Close = func() error {
			if parkCloser {
				closeArrived <- struct{}{}
				<-closeGate
				return cleanupErr
			}
			secondClosed <- struct{}{}
			return nil
		}
		return preparedExecuting(exec)
	}
	hctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var firstSessionID string
	h, err := New(hctx, Dependencies{
		Storage: store,
		Prepare: func(_ context.Context, req PreparationRequest) (PreparedExecution, error) {
			if req.Session.Identity.SessionID == firstSessionID {
				return preparedFor(scriptA, true), nil
			}
			return preparedFor(scriptB, false), nil
		},
	})
	testassert.NoError(t, err)
	sessionA, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	firstSessionID = sessionA.Identity.SessionID
	sessionB, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)

	{
		_, err := submitText(t, h, sessionA.Identity.SessionID, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	<-scriptA.arrived
	setTarget("A")
	scriptA.releaseGate()
	<-closeArrived // A's first publication failed and its blocking closer is entered

	{
		_, err := submitText(t, h, sessionB.Identity.SessionID, "op-b", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	coordB := cachedCoordinator(t, h, sessionB.Identity.SessionID)
	coordB.mu.Lock()
	runB := coordB.run
	coordB.mu.Unlock()
	testassert.Require(t, runB != nil, "the admitted B run slot was not installed")
	<-scriptB.arrived
	setTarget("B")
	scriptB.releaseGate()
	// B's failure publication is recorded before its (unblocked) closer runs;
	// the later cause is retained well before A disposes. Await the actual
	// installed run's completion and retirement, not merely its closer entry.
	<-secondClosed
	select {
	case <-runB.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the failed B run never completed")
	}
	coordB.mu.Lock()
	retiredB := coordB.run == nil
	coordB.mu.Unlock()
	testassert.Require(t, retiredB, "the failed B run slot was not retired before A's final closer released")

	waited := make(chan error, 1)
	go func() { waited <- h.Wait(context.Background()) }()
	cancel()
	close(closeGate)
	waitErr := <-waited
	testassert.Require(t, waitErr != nil && errors.Is(waitErr, errA) && errors.Is(waitErr, cleanupErr),
		"Wait = %v, want both the first publication cause and the joined cleanup cause", waitErr)
	testassert.Require(t, !errors.Is(waitErr, errB), "Wait = %v, want the later publication cause never to replace the first", waitErr)
	{
		rec := settledOperation(t, store, sessionB.Identity.SessionID, "op-b")
		testassert.Require(t, rec.State.Status == OperationRunning, "later operation = %+v, want its marked publication failure retained for recovery", rec.State)
	}
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
		var executed CallbackInfo
		exec := effectExecution(modelFn, func(ctx context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: permissions, Execute: func(ctx context.Context) ToolOutcome {
				executed, _ = CallbackInfoFor(ctx)
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
		testassert.Require(t, reflect.DeepEqual(executed.AuthorizedPermissions, want), "execute authorized pairs = %+v, want the frozen acquisition %+v", executed.AuthorizedPermissions, want)
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
		immediate := &ToolOutcome{Evidence: ExecutionObserved, Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "original"}, Metadata: metadata}
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
	log    *callbackLog
	passes int
}

func (s *multiPassStrategy) Plan(ctx context.Context, _ PlanInput) (PlanOutput, error) {
	s.log.record(ctx, CallbackStrategyPlan)
	return s.modelPlan()
}

func (s *multiPassStrategy) Consume(ctx context.Context, in ConsumeInput) (PlanOutput, error) {
	s.log.record(ctx, CallbackStrategyConsume)
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
	log := &callbackLog{t: t}
	strategy := &multiPassStrategy{log: log}
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

	// Actual strategy identities: the initial plan carries the procedure's
	// own producer, each consume the completed maintenance producer and its
	// invocation; both distinct.
	plans := log.findAll(CallbackStrategyPlan)
	consumes := log.findAll(CallbackStrategyConsume)
	testassert.Require(t, len(plans) == 1 && len(consumes) == 2, "strategy callbacks = %d plans / %d consumes, want one plan and two consumes", len(plans), len(consumes))
	testassert.Require(t, plans[0].InvocationID == "" && plans[0].Purpose == InvocationPurposeMaintenance && plans[0].RequestID != "", "initial plan identity = %+v, want the procedure producer with no invocation", plans[0])
	testassert.Require(t, consumes[0].InvocationID != "" && consumes[0].RequestID == consumes[0].InvocationID && consumes[1].InvocationID != "" && consumes[1].RequestID != consumes[0].RequestID,
		"consume identities = %+v, want distinct actual maintenance invocations", consumes)
	testassert.Require(t, consumes[0].RequestID == first.RequestID && consumes[1].RequestID == second.RequestID, "consume producers = %q/%q, want the published fact producers", consumes[0].RequestID, consumes[1].RequestID)
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
	fixture := validTestGraph()
	stamp := testTime
	fixture.ops[0].State.Status, fixture.ops[0].State.SettledAt = OperationSuccess, &stamp
	fixture.ops[0].State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
	settlement := validSettlementEntry()
	settlement.EntryID = hexID(3)
	fixture.entries = append(fixture.entries, testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime}, settlement: &settlement})
	compact := validOperationRecord()
	compact.Admission.OperationID, compact.Admission.RequestKind, compact.Admission.AdmittedEntry = "compact-op", RequestKindCompact, EntryRef{}
	compact.Admission.Execution = capture
	fixture.ops = append(fixture.ops, compact)
	fixture.session.State.CurrentOperationID = "compact-op"
	store := fixture.storage(t)

	hctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scriptB := newModelScript()
	scriptB.gate = make(chan struct{})
	bClosed := make(chan struct{}, 1)
	var sessionB string
	h, err := New(hctx, Dependencies{
		Storage: store,
		Prepare: func(_ context.Context, req PreparationRequest) (PreparedExecution, error) {
			if req.Session.Identity.SessionID != sessionB {
				return PreparedExecution{}, errors.New("unexpected preparation")
			}
			return PreparedExecution{Capture: testCapture(), Open: func(context.Context, OperationAdmission) (Execution, error) {
				return Execution{
					OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
						return invocationExecution(scriptB.model, nil), nil
					},
					Close: func() error {
						bClosed <- struct{}{}
						return nil
					},
				}, nil
			}}, nil
		},
	})
	testassert.NoError(t, err)
	cA := leaseCoordinator(t, h, testSessionID)
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	sessionB = session.Identity.SessionID

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
	aCloseArrived := make(chan struct{}, 1)
	aCloseGate := make(chan struct{})
	aClosed := make(chan struct{}, 1)
	execA := Execution{
		Strategy: &StrategyBinding{ID: "test.manual/v1", Strategy: callbackStrategy{log: &callbackLog{t: t}}},
		OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
			return InvocationExecution{
				Model: func(context.Context, model.Request) (model.Stream, error) {
					return summaryStream("manual summary", model.Usage{InputTokens: 1}), nil
				},
				Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
				NormalizeTool: objectNormalize,
				Close: func() error {
					aCloseArrived <- struct{}{}
					<-aCloseGate
					return cleanupErr
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
	// Admit and park the later Session's execution first.
	{
		_, err := submitText(t, h, sessionB, "op-b", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	<-scriptB.arrived
	cB := cachedCoordinator(t, h, sessionB)
	cB.mu.Lock()
	runB := cB.run
	cB.mu.Unlock()
	testassert.Require(t, runB != nil, "the admitted B run slot was not installed")
	// Fail the manual checkpoint publication and block its final closer.
	aDone := make(chan error, 1)
	go func() { aDone <- executeDirect(h, cA, hctx, "compact-op", preparedA) }()
	<-aCloseArrived
	// Fail the later execution's result publication and await its retirement.
	scriptB.releaseGate()
	<-bClosed
	select {
	case <-runB.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the failed B run never retired")
	}
	cB.mu.Lock()
	retiredB := cB.run == nil
	cB.mu.Unlock()
	testassert.Require(t, retiredB, "B still owns its run slot before the manual closer releases")
	// Only now release the manual closer, after Wait has begun.
	waited := make(chan error, 1)
	go func() { waited <- h.Wait(context.Background()) }()
	cancel()
	close(aCloseGate)
	waitErr := <-waited
	testassert.Require(t, errors.Is(waitErr, errA) && errors.Is(waitErr, cleanupErr), "Wait = %v, want the first publication cause and the joined cleanup cause", waitErr)
	testassert.Require(t, !errors.Is(waitErr, errB), "Wait = %v, want the later execution's cause never to replace the first", waitErr)
	select {
	case <-aClosed:
	default:
		t.Fatal("Wait returned before the manual execution disposal")
	}
	{
		err := <-aDone
		testassert.Require(t, errors.Is(err, errA), "manual execution = %v, want the retained checkpoint cause", err)
	}
}
