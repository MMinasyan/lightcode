package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

func nativeService(id, contract string, lifetime extension.Lifetime, handler extension.Handler, requires ...extension.Binding) extension.Native {
	capability := extension.ServiceCapability(id, contract, extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, `{"type":"object","properties":{"reason":{"type":"string"}},"required":["reason"]}`, `{"type":"string"}`))
	componentID := id + "-c"
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           id,
			Version:      "2",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"value":{"type":"integer"}},"required":["enabled"],"additionalProperties":false}`),
			Components:   []extension.Component{{ID: componentID, Lifetime: lifetime, Provides: []string{id}, Requires: requires}},
			Capabilities: []extension.Capability{capability},
		},
		Open: func(ctx context.Context, _ extension.InstanceContext) (extension.Instance, error) {
			if err := ctx.Err(); err != nil {
				return extension.Instance{}, err
			}
			return extension.Instance{Capabilities: map[string]extension.Implementation{id: {Service: map[string]extension.Handler{"call": handler}}}}, nil
		},
	}
}

func TestNativeToolDescribeAtAllLifetimesWithoutFactories(t *testing.T) {
	var opens [4]atomic.Int64
	lifetimes := []extension.Lifetime{extension.LifetimeHarness, extension.LifetimeWorkspace, extension.LifetimeOperation, extension.LifetimeInvocation}
	natives := make([]extension.Native, 0, len(lifetimes))
	for i, lifetime := range lifetimes {
		id := string(lifetime) + "_tool"
		native := testNative(id, lifetime, []extension.Capability{extension.ToolCapability(id)}, nil,
			func(context.Context, extension.InstanceContext) (extension.Instance, error) {
				opens[i].Add(1)
				return extension.Instance{Capabilities: map[string]extension.Implementation{id: {Tool: noopNativeTool{}}}}, nil
			})
		native.Describe[id] = func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
			return extension.ToolDescription{Definition: model.ToolDefinition{Name: id, Description: id, Parameters: json.RawMessage(`{"type":"object"}`)}, Available: true}, nil
		}
		natives = append(natives, native)
	}
	c := mustComposition(t,
		natives[0], natives[1], natives[2], natives[3],
	)
	for i, lifetime := range lifetimes {
		id := string(lifetime) + "_tool"
		declaration, ok := c.toolSpec(id)
		testassert.Require(t, ok, "tool %q has no declaration", id)
		description, err := declaration.describe(context.Background(), Invocation{}, model.ModelRef{Provider: "p", Model: "m"}, harness.ToolConstraints{}, harness.SessionIdentity{SessionID: testSessionID}, harness.AgentInputs{})
		testassert.NoError(t, err)
		testassert.Require(t, description.Available && description.Definition.Name == id, "description = %+v", description)
		testassert.Equal(t, opens[i].Load(), int64(0))
	}

}

type noopNativeTool struct{}

func (noopNativeTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (noopNativeTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: "call", Status: model.ResultSuccess, Content: "ok"}}}
}
func (noopNativeTool) Execute(context.Context, extension.ToolContext, extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Result: model.ToolResult{CallID: "call", Status: model.ResultSuccess, Content: "ok"}}
}

func TestNativeToolPlanOwnershipAndDeclaredFailureDispatch(t *testing.T) {
	provider := nativeService("provider", "test.provider", extension.LifetimeHarness, func(_ context.Context, call extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
		testassert.Require(t, call.Caller.ComponentID == "tool-c", "caller = %+v, want the originating tool component", call.Caller)
		if string(arguments) == `{"fail":true}` {
			return extension.Result{Failure: &extension.Failure{Code: "declared", Message: "provider refused", Data: json.RawMessage(`{"reason":"because"}`), Partial: json.RawMessage(`"partial"`)}}, nil
		}
		return extension.Result{Value: json.RawMessage(`{"ok":true}`)}, nil
	})
	binding := extension.Binding{Name: "dep", Provider: "provider/provider-c", CapabilityID: "provider", Definition: provider.Declaration.Capabilities[0].Definition()}
	toolNative := extension.Native{
		Declaration: extension.Declaration{
			ID:           "tool",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"],"additionalProperties":false}`),
			Components:   []extension.Component{{ID: "tool-c", Lifetime: extension.LifetimeHarness, Provides: []string{"tool"}, Requires: []extension.Binding{binding}}},
			Capabilities: []extension.Capability{extension.ToolCapability("tool")},
		},
		Describe: map[string]extension.ToolDescribe{"tool": func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
			return extension.ToolDescription{Definition: model.ToolDefinition{Name: "tool", Parameters: json.RawMessage(`{"type":"object"}`)}, Available: true}, nil
		}},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"tool": {Tool: planTool{}}}}, nil
		},
	}
	c := mustComposition(t, provider, toolNative)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	sel := selection{scopes: []*scope{rt}, invocation: Invocation{}}
	tool, err := c.bindTool(sel, "tool")
	testassert.NoError(t, err)
	tc := ToolContext{Session: harness.SessionIdentity{SessionID: testSessionID}, OperationID: "op", InvocationID: "inv", Purpose: harness.InvocationPurposeConversation, Workspace: "/w", AdmittedEntry: harness.EntryRef{SessionID: testSessionID, EntryID: testEntryID}, Invocation: Invocation{}}
	call := model.ToolCall{ID: "call-1", Name: "tool", Arguments: json.RawMessage(`{"fail":true}`)}
	plan := tool.Prepare(context.Background(), tc, call)
	testassert.Require(t, plan.Execute != nil && len(plan.Permissions) == 1, "plan = %+v, want the executable plan", plan)
	outcome := plan.Execute(context.Background())
	testassert.Require(t, outcome.Result.Status == model.ResultSuccess && outcome.Result.Content == "declared:provider refused", "outcome = %+v, want the declared failure mapped through dispatch", outcome.Result)
}

// planTool prepares one plan carrying opaque Data and, at Execute, calls its
// declared service with that Data.
type planTool struct{}

func (planTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}

func (planTool) Prepare(_ context.Context, _ extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: "test.call", Target: "*"}}, Data: append([]byte("opaque:"), call.Arguments...)}
}

func (planTool) Execute(ctx context.Context, tc extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	if string(plan.Data) != `opaque:{"fail":true}` {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: "plan data changed"}}
	}
	result, err := tc.Calls.Call(ctx, "dep", "call", json.RawMessage(`{"fail":true}`))
	if err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	if result.Failure == nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: "no failure"}}
	}
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: result.Failure.Code + ":" + result.Failure.Message}}
}

func TestNativeCloseUsesFreshLifecycleViewOnlyForLongerProviders(t *testing.T) {
	var (
		mu             sync.Mutex
		closeCalls     int
		retained       extension.CallContext
		callerAccepted bool
	)
	provider := nativeService("provider", "test.provider", extension.LifetimeHarness, func(_ context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		mu.Lock()
		callerAccepted = call.Caller.ComponentID == "consumer-c"
		mu.Unlock()
		if call.Caller.ComponentID != "consumer-c" {
			return extension.Result{}, fmt.Errorf("foreign caller %q", call.Caller.ComponentID)
		}
		return extension.Result{Value: json.RawMessage(`{"ok":true}`)}, nil
	})
	binding := extension.Binding{Name: "dep", Provider: "provider/provider-c", CapabilityID: "provider", Definition: provider.Declaration.Capabilities[0].Definition()}
	consumerCapability := extension.ServiceCapability("consumer", "test.consumer", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	consumer := extension.Native{
		Declaration: extension.Declaration{
			ID:           "consumer",
			Version:      "3",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "consumer-c", Lifetime: extension.LifetimeOperation, Provides: []string{"consumer"}, Requires: []extension.Binding{binding}}},
			Capabilities: []extension.Capability{consumerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{
				Capabilities: map[string]extension.Implementation{"consumer": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
					return extension.Result{Value: json.RawMessage(`{}`)}, nil
				}}}},
				Close: func(ctx context.Context, calls extension.CallContext) error {
					mu.Lock()
					closeCalls++
					retained = calls
					mu.Unlock()
					result, err := calls.Call(ctx, "dep", "call", json.RawMessage(`{}`))
					if err != nil {
						return err
					}
					if result.Failure != nil {
						return context.Canceled
					}
					return nil
				},
			}, nil
		},
	}
	sibling := nativeService("sibling", "test.sibling", extension.LifetimeOperation, func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{Value: json.RawMessage(`{}`)}, nil
	}, binding)
	c := mustComposition(t, provider, consumer, sibling)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	op := mustOpenScope(t, c, context.Background(), ScopeInfo{Kind: ScopeOperation, DataDir: "/d", Workspace: "/w", SessionID: testSessionID, OperationID: "op"}, []*scope{rt})
	if siblingValue, ok := op.capabilities["sibling"]; ok {
		view, calls := c.newView(siblingValue, context.Background(), callIdentity{session: harness.SessionIdentity{SessionID: testSessionID}, revision: "7"})
		if _, err := calls.Call(context.Background(), "dep", "call", json.RawMessage(`{}`)); err == nil {
			t.Fatal("a foreign sibling read another instance's provider resource")
		}
		view.end()
	}
	testassert.NoError(t, op.close())
	mu.Lock()
	testassert.Equal(t, closeCalls, 1)
	testassert.Require(t, callerAccepted, "the longer-lived provider did not see the originating consumer identity")
	mu.Unlock()
	_, err := retained.Call(context.Background(), "dep", "call", json.RawMessage(`{}`))
	testassert.Require(t, err != nil, "a retained close view remained callable after Close returned")
}

func TestNativeImplementationProvenanceIncludesRootsAndDependencies(t *testing.T) {
	provider := nativeService("provider", "test.provider", extension.LifetimeHarness, func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{Value: json.RawMessage(`{"ok":true}`)}, nil
	})
	binding := extension.Binding{Name: "dep", Provider: "provider/provider-c", CapabilityID: "provider", Definition: provider.Declaration.Capabilities[0].Definition()}
	consumer := nativeService("consumer", "test.consumer", extension.LifetimeHarness, func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{Value: json.RawMessage(`{"ok":true}`)}, nil
	}, binding)
	resolver := nativeService("guestdefs", "test.resolver", extension.LifetimeHarness, func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{Value: json.RawMessage(`{}`)}, nil
	}, binding)
	resolver.Declaration.Version = "3"
	unused := nativeService("agents", "test.agents", extension.LifetimeHarness, func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{Value: json.RawMessage(`{}`)}, nil
	})
	c := mustComposition(t, provider, consumer, resolver, unused)
	sel := selection{
		selected:   []string{"consumer"},
		definition: &harness.DefinitionRef{ExtensionID: "guestdefs", CapabilityID: "guestdefs", DefinitionID: "named"},
		invocation: Invocation{snapshot: &configuration{generation: 7, extensions: map[string]json.RawMessage{
			"consumer":  json.RawMessage(`{"enabled":true,"value":9}`),
			"guestdefs": json.RawMessage(`{"enabled":true,"resolver":true}`),
		}}},
	}
	got := implementationProvenance(c, sel)
	testassert.Require(t, len(got) == 3, "provenance = %+v, want the selected consumer, its provider dependency, and the concrete resolver's dependency closure", got)
	testassert.Equal(t, got[0].ExtensionID, "provider")
	testassert.Equal(t, got[1].ExtensionID, "consumer")
	testassert.Equal(t, string(got[1].Settings), `{"enabled":true,"value":9}`)
	testassert.Equal(t, got[2].ExtensionID, "guestdefs")
	testassert.Equal(t, got[2].Version, "3")
	testassert.Equal(t, string(got[2].Settings), `{"enabled":true,"resolver":true}`)
	for _, capture := range got {
		testassert.Require(t, capture.ExtensionID != "agents", "unused Agents root appeared in provenance: %+v", got)
	}
}

func TestNativeStrategyBindingUsesDeclaredMetadata(t *testing.T) {
	strategyCapability := extension.StrategyCapability("native_strategy", false)
	native := extension.Native{
		Declaration: extension.Declaration{
			ID:           "strategyext",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "strategy-c", Lifetime: extension.LifetimeOperation, Provides: []string{"native_strategy"}}},
			Capabilities: []extension.Capability{strategyCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"native_strategy": {Strategy: includedNoopStrategy{}}}}, nil
		},
	}
	c := mustComposition(t, native)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	op := mustOpenScope(t, c, context.Background(), ScopeInfo{Kind: ScopeOperation, DataDir: "/d", Workspace: "/w", SessionID: testSessionID, OperationID: "op"}, []*scope{rt})
	defer func() { testassert.NoError(t, op.close()) }()
	binding, err := resolveStrategyBinding(c, []*scope{rt, op}, harness.StrategySelection{ID: "native_strategy"})
	testassert.NoError(t, err)
	testassert.Require(t, binding.ID == "native_strategy" && !binding.RequiresContextWindow && binding.Strategy != nil, "binding = %+v, want the declared metadata and implementation", binding)
	if _, err := resolveStrategyBinding(c, []*scope{rt, op}, harness.StrategySelection{ID: "ghost"}); err == nil {
		t.Fatal("resolveStrategyBinding accepted an undeclared strategy")
	}
}

type includedNoopStrategy struct{}

func (includedNoopStrategy) Plan(harness.PlanInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

func (includedNoopStrategy) Consume(harness.ConsumeInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

func TestNativeOpenViewExpiresWhenOpenReturns(t *testing.T) {
	base := nativeService("openbase", "test.openbase", extension.LifetimeHarness, func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{Value: json.RawMessage(`{}`)}, nil
	})
	binding := extension.Binding{Name: "dep", Provider: "openbase/openbase-c", CapabilityID: "openbase", Definition: base.Declaration.Capabilities[0].Definition()}
	consumer := func(id string, live *atomic.Bool) extension.Native {
		capability := extension.ServiceCapability(id, "test."+id, extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
		native := testNative(id, extension.LifetimeHarness, []extension.Capability{capability}, []extension.Binding{binding}, nil)
		native.Open = func(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
			// The declared binding is valid while this Open view is live.
			if _, err := instance.Call(ctx, "dep", "call", json.RawMessage(`{}`)); err != nil {
				return extension.Instance{}, err
			}
			live.Store(true)
			return extension.Instance{Capabilities: map[string]extension.Implementation{id: {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
				return extension.Result{Value: json.RawMessage(`{}`)}, nil
			}}}}}, nil
		}
		return native
	}
	var firstLive, secondLive atomic.Bool
	var retained extension.InstanceContext
	first := consumer("openconsumer", &firstLive)
	open := first.Open
	first.Open = func(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
		retained = instance
		return open(ctx, instance)
	}
	second := consumer("openconsumer2", &secondLive)
	c := mustComposition(t, base, first, second)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	testassert.Require(t, firstLive.Load() && secondLive.Load(), "declared Open bindings = first %v second %v, want both live", firstLive.Load(), secondLive.Load())
	if _, err := retained.Call(context.Background(), "dep", "call", json.RawMessage(`{}`)); !errors.Is(err, extension.ErrClosed) {
		t.Fatalf("a retained Open view call = %v, want the expired view refusal while the provider remains live", err)
	}
}
func TestNativeCallOwnsOriginAndJoinsBeforeReturn(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	provider := nativeService("authprovider", "test.auth", extension.LifetimeHarness, func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return extension.Result{}, ctx.Err()
	})
	binding := extension.Binding{Name: "dep", Provider: "authprovider/authprovider-c", CapabilityID: "authprovider", Definition: provider.Declaration.Capabilities[0].Definition()}
	forger := extension.Native{
		Declaration: extension.Declaration{
			ID:           "caller",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "caller-c", Lifetime: extension.LifetimeHarness, Provides: []string{"caller"}, Requires: []extension.Binding{binding}}},
			Capabilities: []extension.Capability{extension.ToolCapability("caller")},
		},
		Describe: map[string]extension.ToolDescribe{"caller": testDescribeTool("caller")},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"caller": {Tool: originTool{started: started}}}}, nil
		},
	}
	c := mustComposition(t, provider, forger)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	tool, err := c.bindTool(selection{scopes: []*scope{rt}, invocation: Invocation{}}, "caller")
	testassert.NoError(t, err)
	plan := tool.Prepare(context.Background(), ToolContext{}, model.ToolCall{ID: "call", Name: "caller", Arguments: json.RawMessage(`{}`)})
	testassert.Require(t, plan.Execute != nil, "plan = %+v, want execution", plan)
	outcome := plan.Execute(context.Background())
	testassert.Require(t, outcome.Result.Status == model.ResultSuccess, "outcome = %+v", outcome.Result)
	select {
	case <-finished:
	default:
		t.Fatal("the substituted-background nested call was not canceled and joined when the callback returned")
	}
}

type originTool struct {
	started chan struct{}
}

func (originTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (originTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{}
}
func (t originTool) Execute(ctx context.Context, tc extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	go func() {
		_, _ = tc.Calls.Call(context.Background(), "dep", "call", json.RawMessage(`{}`))
	}()
	<-t.started
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "ok"}}
}

func TestNativeCloseReachesLongerProviderAfterOwnerCancellation(t *testing.T) {
	var called atomic.Bool
	provider := nativeService("ownerprovider", "test.owner", extension.LifetimeHarness, func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		called.Store(true)
		return extension.Result{Value: json.RawMessage(`{}`)}, nil
	})
	binding := extension.Binding{Name: "dep", Provider: "ownerprovider/ownerprovider-c", CapabilityID: "ownerprovider", Definition: provider.Declaration.Capabilities[0].Definition()}
	consumerCapability := extension.ServiceCapability("ownerconsumer", "test.owner.consumer", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	consumer := extension.Native{
		Declaration: extension.Declaration{
			ID:           "ownerconsumer",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "ownerconsumer-c", Lifetime: extension.LifetimeWorkspace, Provides: []string{"ownerconsumer"}, Requires: []extension.Binding{binding}}},
			Capabilities: []extension.Capability{consumerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{
				Capabilities: map[string]extension.Implementation{"ownerconsumer": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
					return extension.Result{Value: json.RawMessage(`{}`)}, nil
				}}}},
				Close: func(ctx context.Context, calls extension.CallContext) error {
					_, err := calls.Call(ctx, "dep", "call", json.RawMessage(`{}`))
					return err
				},
			}, nil
		},
	}
	c := mustComposition(t, provider, consumer)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	ws := mustOpenScope(t, c, context.Background(), workspaceScopeInfo("/w"), []*scope{rt})
	rt.cancel() // the owner canceled ordinary work before retirement
	testassert.NoError(t, ws.close())
	testassert.Require(t, called.Load(), "workspace Close did not reach the still-open longer-lived provider")
	testassert.NoError(t, rt.close())
}

type lifetimeTool struct {
	id       string
	executes *atomic.Int64
	valid    *atomic.Int64
}

func (t lifetimeTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (t lifetimeTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}}}
}
func (t lifetimeTool) Execute(_ context.Context, tc extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	t.executes.Add(1)
	if tc.Calls.ToolCallID != "" && tc.Calls.RequestID != "" && tc.Calls.EffectID != "" && tc.Calls.Revision != "" && len(tc.Calls.Section) != 0 {
		t.valid.Add(1)
	}
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: t.id}}
}

func TestNativeToolsAdmitAuthorizeExecuteAndDisposeAtAllLifetimes(t *testing.T) {
	lifetimes := []extension.Lifetime{extension.LifetimeHarness, extension.LifetimeWorkspace, extension.LifetimeOperation, extension.LifetimeInvocation}
	var opens, closes, executes, valid [4]atomic.Int64
	native := extension.Native{
		Declaration: extension.Declaration{
			ID:           "lifetimes",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Capabilities: make([]extension.Capability, 0, len(lifetimes)),
			Components:   make([]extension.Component, 0, len(lifetimes)),
		},
		Describe: map[string]extension.ToolDescribe{},
	}
	for _, lifetime := range lifetimes {
		id := string(lifetime) + "_tool"
		native.Declaration.Capabilities = append(native.Declaration.Capabilities, extension.ToolCapability(id))
		native.Declaration.Components = append(native.Declaration.Components, extension.Component{ID: id + "-c", Lifetime: lifetime, Provides: []string{id}})
		native.Describe[id] = testDescribeTool(id)
	}
	// Open is one closure over the loop because the host calls one function
	// for every component.
	native.Open = func(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
		for i, lifetime := range lifetimes {
			id := string(lifetime) + "_tool"
			if instance.ComponentID != id+"-c" {
				continue
			}
			opens[i].Add(1)
			tool := lifetimeTool{id: id, executes: &executes[i], valid: &valid[i]}
			return extension.Instance{Capabilities: map[string]extension.Implementation{id: {Tool: tool}}, Close: func(context.Context, extension.CallContext) error {
				closes[i].Add(1)
				return nil
			}}, nil
		}
		return extension.Instance{}, fmt.Errorf("unknown component %q", instance.ComponentID)
	}
	e := newOwnerEnv(t)
	e.fixtureStore(storage.NewMemory())
	enableFixturePlugin(t, e.configPath, "lifetimes")
	writeAgentDefinitions(t, e.configPath, `{"lifetime_agent":{"model":"prov/m","system_prompt":"simple","tools":["harness_tool","workspace_tool","operation_tool","invocation_tool"]}}`)
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		return []string{
			`{"choices":[{"delta":{"role":"assistant","tool_calls":[` +
				`{"index":0,"id":"call-h","type":"function","function":{"name":"harness_tool","arguments":"{}"}},` +
				`{"index":1,"id":"call-w","type":"function","function":{"name":"workspace_tool","arguments":"{}"}},` +
				`{"index":2,"id":"call-o","type":"function","function":{"name":"operation_tool","arguments":"{}"}},` +
				`{"index":3,"id":"call-i","type":"function","function":{"name":"invocation_tool","arguments":"{}"}}` +
				`]},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		}
	})
	ctx := context.Background()
	r, err := e.open(ctx, native)
	testassert.NoError(t, err)
	session, err := r.createSession(ctx, e.dataDir, "lifetime_agent")
	testassert.NoError(t, err)
	for i := range lifetimes {
		testassert.Equal(t, executes[i].Load(), int64(0))
	}
	testassert.Equal(t, opens[2].Load(), int64(0))
	testassert.Equal(t, opens[3].Load(), int64(0))
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-lifetimes", "run")
	awaitOperation(t, r, session.Identity.SessionID, "op-lifetimes", harness.OperationSuccess)
	deadline := time.Now().Add(10 * time.Second)
	for closes[3].Load() != 1 || closes[2].Load() != 1 {
		testassert.Require(t, time.Now().Before(deadline), "terminal disposal never completed: invocation %d operation %d", closes[3].Load(), closes[2].Load())
		time.Sleep(10 * time.Millisecond)
	}
	for i := range lifetimes {
		testassert.Require(t, opens[i].Load() == 1, "%s opens = %d, want one admitted execution", lifetimes[i], opens[i].Load())
		testassert.Require(t, executes[i].Load() == 1, "%s executes = %d, want one committed result", lifetimes[i], executes[i].Load())
		testassert.Require(t, valid[i].Load() == 1, "%s call context lost its actual request/tool/effect identity or section", lifetimes[i])
	}
	testassert.Equal(t, closes[3].Load(), int64(1))
	testassert.Equal(t, closes[2].Load(), int64(1))
	testassert.Equal(t, closes[0].Load(), int64(0))
	testassert.NoError(t, r.Close(ctx))
	for i := range lifetimes {
		testassert.Require(t, closes[i].Load() == 1, "%s closes = %d, want one disposal", lifetimes[i], closes[i].Load())
	}
}

type authorityProbe struct {
	normalizeErr error
	prepareErr   error
	executeErr   error
	target       string
	plan         extension.ToolPlan
}

func childLaunchArguments(target string) json.RawMessage {
	return childLaunchRefArguments(harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: target}, "op")
}

func childLaunchRefArguments(ref harness.DefinitionRef, operationID string) json.RawMessage {
	value, _ := json.Marshal(map[string]any{
		"definition": map[string]string{
			"extension_id":  ref.ExtensionID,
			"capability_id": ref.CapabilityID,
			"definition_id": ref.DefinitionID,
		},
		"prompt":         "x",
		"operation_id":   operationID,
		"max_concurrent": 1,
		"output_limit":   1024,
	})
	return value
}

func (p *authorityProbe) Normalize(tc extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	_, p.normalizeErr = tc.Calls.Call(context.Background(), "child_launch", "launch", childLaunchArguments("allowed"))
	return call.Arguments, nil
}
func (p *authorityProbe) Prepare(_ context.Context, tc extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	_, p.prepareErr = tc.Calls.Call(context.Background(), "child_launch", "launch", childLaunchArguments("allowed"))
	p.plan = extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionChildLaunch, Target: (harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: "allowed"}).PermissionTarget()}}, Data: []byte("allowed")}
	return p.plan
}
func (p *authorityProbe) Execute(ctx context.Context, tc extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	_, p.executeErr = tc.Calls.Call(ctx, "child_launch", "launch", childLaunchArguments(p.target))
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "ok"}}
}

func TestNativeCallbackAuthorityCannotLaunchBeforeExecute(t *testing.T) {
	childLaunch, ok := extension.HostCapability(extension.ContractChildLaunch)
	testassert.Require(t, ok, "host child_launch definition is missing")
	binding := extension.Binding{Name: "child_launch", Provider: extension.HostProvider, CapabilityID: extension.ContractChildLaunch, Definition: childLaunch}
	probe := &authorityProbe{target: "allowed"}
	var closeErr error
	native := testNative("authority", extension.LifetimeHarness, []extension.Capability{extension.ToolCapability("probe")}, []extension.Binding{binding}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"probe": {Tool: probe}}, Close: func(_ context.Context, calls extension.CallContext) error {
			_, closeErr = calls.Call(context.Background(), "child_launch", "launch", childLaunchArguments("allowed"))
			return nil
		}}, nil
	})
	native.Describe["probe"] = testDescribeTool("probe")
	c := mustComposition(t, native)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	tool, err := c.bindTool(selection{scopes: []*scope{rt}, invocation: Invocation{}}, "probe")
	testassert.NoError(t, err)
	tc := ToolContext{Session: harness.SessionIdentity{SessionID: testSessionID}, Invocation: Invocation{}}
	_, err = tool.Normalize(context.Background(), tc, model.ToolCall{ID: "call", Name: "probe", Arguments: json.RawMessage(`{}`)})
	testassert.NoError(t, err)
	plan := tool.Prepare(context.Background(), tc, model.ToolCall{ID: "call", Name: "probe", Arguments: json.RawMessage(`{}`)})
	testassert.Require(t, plan.Execute != nil, "plan = %+v, want an executor", plan)
	plan.Execute(context.Background())
	probe.target = "other"
	plan.Execute(context.Background())
	testassert.NoError(t, rt.close())
	testassert.Require(t, probe.normalizeErr != nil && strings.Contains(probe.normalizeErr.Error(), "pure callback cannot invoke"), "Normalize launch error = %v, want the purity refusal", probe.normalizeErr)
	testassert.Require(t, probe.prepareErr != nil && strings.Contains(probe.prepareErr.Error(), "pure callback cannot invoke"), "Prepare launch error = %v, want the purity refusal", probe.prepareErr)
	testassert.Require(t, probe.executeErr != nil && strings.Contains(probe.executeErr.Error(), "not authorized"), "Execute launch error = %v, want the frozen exact target refusal", probe.executeErr)
	testassert.Require(t, closeErr != nil && strings.Contains(closeErr.Error(), "not authorized"), "Close launch error = %v, want no lifecycle execute authority", closeErr)
}

func TestNativeDeniedPlanDoesNotExecute(t *testing.T) {
	e := newOwnerEnv(t)
	writeServiceFile(t, e.configPath, strings.TrimSuffix(ownerConfigDocument(e.server.URL), "}")+`,"permissions":{"rules":[{"permission":"command.run","target":"denied","access":"deny"}]}}`)
	writeAgentDefinitions(t, e.configPath, `{"denied_agent":{"model":"prov/m","system_prompt":"simple","tools":["denied_tool"]}}`)
	enableFixturePlugin(t, e.configPath, "deniedext")
	e.fixtureStore(storage.NewMemory())
	var executes atomic.Int64
	native := testNative("deniedext", extension.LifetimeHarness, []extension.Capability{extension.ToolCapability("denied_tool")}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"denied_tool": {Tool: deniedTool{executes: &executes}}}}, nil
	})
	native.Describe["denied_tool"] = testDescribeTool("denied_tool")
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		return toolCallTurnEvents("call-denied", "denied_tool", `{}`)
	})
	ctx := context.Background()
	r, err := e.open(ctx, native)
	testassert.NoError(t, err)
	session, err := r.createSession(ctx, e.dataDir, "denied_agent")
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-denied", "run")
	awaitOperation(t, r, session.Identity.SessionID, "op-denied", harness.OperationSuccess)
	testassert.Equal(t, executes.Load(), int64(0))
	testassert.NoError(t, r.Close(ctx))
}

type deniedTool struct{ executes *atomic.Int64 }

func (deniedTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (deniedTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "denied"}}}
}
func (t deniedTool) Execute(context.Context, extension.ToolContext, extension.ToolPlan) harness.ToolOutcome {
	t.executes.Add(1)
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "must not run"}}
}

func TestNativeCallbackViewCancelsAndJoinsThroughHarness(t *testing.T) {
	e := newOwnerEnv(t)
	enableFixturePlugin(t, e.configPath, "origin")
	writeAgentDefinitions(t, e.configPath, `{"origin_agent":{"model":"prov/m","system_prompt":"simple","tools":["origin_tool"]}}`)
	e.fixtureStore(storage.NewMemory())
	started := make(chan struct{})
	finished := make(chan struct{})
	providerCapability := extension.ServiceCapability("origin.provider", "test.origin.provider", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	binding := extension.Binding{Name: "dep", Provider: "origin/origin-provider-c", CapabilityID: "origin.provider", Definition: providerCapability.Definition()}
	native := extension.Native{
		Declaration: extension.Declaration{
			ID:           "origin",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components: []extension.Component{
				{ID: "origin-provider-c", Lifetime: extension.LifetimeHarness, Provides: []string{"origin.provider"}},
				{ID: "origin-tool-c", Lifetime: extension.LifetimeHarness, Provides: []string{"origin_tool"}, Requires: []extension.Binding{binding}},
			},
			Capabilities: []extension.Capability{providerCapability, extension.ToolCapability("origin_tool")},
		},
		Describe: map[string]extension.ToolDescribe{"origin_tool": testDescribeTool("origin_tool")},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{}, nil
		},
	}
	native.Open = func(_ context.Context, instance extension.InstanceContext) (extension.Instance, error) {
		switch instance.ComponentID {
		case "origin-provider-c":
			return extension.Instance{Capabilities: map[string]extension.Implementation{"origin.provider": {Service: map[string]extension.Handler{"call": func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
				close(started)
				<-ctx.Done()
				close(finished)
				return extension.Result{}, ctx.Err()
			}}}}}, nil
		case "origin-tool-c":
			return extension.Instance{Capabilities: map[string]extension.Implementation{"origin_tool": {Tool: harnessOriginTool{started: started}}}}, nil
		}
		return extension.Instance{}, fmt.Errorf("unknown component %q", instance.ComponentID)
	}
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		return toolCallTurnEvents("call-origin", "origin_tool", `{}`)
	})
	ctx := context.Background()
	r, err := e.open(ctx, native)
	testassert.NoError(t, err)
	session, err := r.createSession(ctx, e.dataDir, "origin_agent")
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-origin", "run")
	awaitOperation(t, r, session.Identity.SessionID, "op-origin", harness.OperationSuccess)
	select {
	case <-finished:
	default:
		t.Fatal("the in-flight nested call was not canceled and joined before the callback returned")
	}
	testassert.NoError(t, r.Close(ctx))
}

type harnessOriginTool struct{ started chan struct{} }

func (harnessOriginTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (harnessOriginTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}}}
}
func (t harnessOriginTool) Execute(ctx context.Context, tc extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	go func() {
		_, _ = tc.Calls.Call(context.Background(), "dep", "call", json.RawMessage(`{}`))
	}()
	<-t.started
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "ok"}}
}

type ownershipProbe struct {
	inputAfter  string
	resultValue string
}

func (p *ownershipProbe) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (p *ownershipProbe) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}}}
}
func (p *ownershipProbe) Execute(ctx context.Context, tc extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	arguments := []byte(`{}`)
	result, err := tc.Calls.Call(ctx, "dep", "call", arguments)
	if err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	p.inputAfter = string(arguments)
	p.resultValue = string(result.Value)
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "ok"}}
}

func TestNativeServiceArgumentsAndResultAreOwned(t *testing.T) {
	providerCapability := extension.ServiceCapability("own.provider", "test.own.provider", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	kept := []byte(`{"ok":true}`)
	provider := extension.Native{
		Declaration: extension.Declaration{
			ID:           "own",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "own-provider-c", Lifetime: extension.LifetimeHarness, Provides: []string{"own.provider"}}},
			Capabilities: []extension.Capability{providerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"own.provider": {Service: map[string]extension.Handler{"call": func(_ context.Context, _ extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
				if len(arguments) != 0 {
					arguments[0] = 'x'
				}
				return extension.Result{Value: kept}, nil
			}}}}}, nil
		},
	}
	binding := extension.Binding{Name: "dep", Provider: "own/own-provider-c", CapabilityID: "own.provider", Definition: providerCapability.Definition()}
	probe := &ownershipProbe{}
	caller := extension.Native{
		Declaration: extension.Declaration{
			ID:           "caller",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "caller-c", Lifetime: extension.LifetimeHarness, Provides: []string{"caller"}, Requires: []extension.Binding{binding}}},
			Capabilities: []extension.Capability{extension.ToolCapability("caller")},
		},
		Describe: map[string]extension.ToolDescribe{"caller": testDescribeTool("caller")},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"caller": {Tool: probe}}}, nil
		},
	}
	c := mustComposition(t, provider, caller)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	tool, err := c.bindTool(selection{scopes: []*scope{rt}, invocation: Invocation{}}, "caller")
	testassert.NoError(t, err)
	plan := tool.Prepare(context.Background(), ToolContext{}, model.ToolCall{ID: "call", Name: "caller", Arguments: json.RawMessage(`{}`)})
	plan.Execute(context.Background())
	// The provider's result bytes were mutated after return; the accepted value
	// must remain the original owned bytes.
	kept[1] = '!'
	testassert.Equal(t, probe.inputAfter, `{}`)
	testassert.Equal(t, probe.resultValue, `{"ok":true}`)
}

type narrowHarness struct {
	handler func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error)
}

func (n *narrowHarness) native() extension.Native {
	capability := extension.ServiceCapability("narrow.provider", "test.narrow.provider", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           "narrowprovider",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "narrowprovider-c", Lifetime: extension.LifetimeHarness, Provides: []string{"narrow.provider"}}},
			Capabilities: []extension.Capability{capability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"narrow.provider": {Service: map[string]extension.Handler{"call": func(ctx context.Context, call extension.CallContext, args json.RawMessage) (extension.Result, error) {
				return n.handler(ctx, call, args)
			}}}}}, nil
		},
	}
}

func narrowCaller(provider extension.Native) extension.Native {
	capability := extension.ServiceCapability("narrow.caller", "test.narrow.caller", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	binding := extension.Binding{Name: "dep", Provider: "narrowprovider/narrowprovider-c", CapabilityID: "narrow.provider", Definition: provider.Declaration.Capabilities[0].Definition()}
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           "narrowcaller",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "narrowcaller-c", Lifetime: extension.LifetimeHarness, Provides: []string{"narrow.caller"}, Requires: []extension.Binding{binding}}},
			Capabilities: []extension.Capability{capability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"narrow.caller": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
				return extension.Result{Value: json.RawMessage(`{}`)}, nil
			}}}}}, nil
		},
	}
}

func TestNativeSuppliedContextNarrowsButCannotDetachOrigin(t *testing.T) {
	handler := &narrowHarness{}
	provider := handler.native()
	caller := narrowCaller(provider)
	c := mustComposition(t, provider, caller)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	value := rt.capabilities["narrow.caller"]

	t.Run("already-canceled supplied context refuses before dispatch", func(t *testing.T) {
		var calls atomic.Int64
		handler.handler = func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
			calls.Add(1)
			return extension.Result{Value: json.RawMessage(`{}`)}, nil
		}
		view, _ := c.newView(value, context.Background(), callIdentity{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := view.call(ctx, "dep", "call", json.RawMessage(`{}`))
		view.end()
		testassert.Require(t, errors.Is(err, context.Canceled), "call error = %v, want the supplied cancellation", err)
		testassert.Equal(t, calls.Load(), int64(0))
	})

	t.Run("cancellation during an admitted call cancels while origin stays live", func(t *testing.T) {
		started := make(chan struct{})
		canceled := make(chan struct{})
		handler.handler = func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return extension.Result{}, ctx.Err()
		}
		view, _ := c.newView(value, context.Background(), callIdentity{})
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { _, err := view.call(ctx, "dep", "call", json.RawMessage(`{}`)); result <- err }()
		<-started
		cancel()
		err := <-result
		testassert.Require(t, errors.Is(err, context.Canceled), "call error = %v, want the supplied cancellation", err)
		<-canceled
		testassert.Require(t, view.ctx.Err() == nil, "supplied cancellation detached the host origin")
		view.end()
	})

	t.Run("background cannot detach an expired origin", func(t *testing.T) {
		started := make(chan struct{})
		canceled := make(chan struct{})
		handler.handler = func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return extension.Result{}, ctx.Err()
		}
		view, _ := c.newView(value, context.Background(), callIdentity{})
		result := make(chan error, 1)
		go func() { _, err := view.call(context.Background(), "dep", "call", json.RawMessage(`{}`)); result <- err }()
		<-started
		view.end()
		err := <-result
		testassert.Require(t, err != nil, "Background call survived origin expiry")
		<-canceled
		if _, err := view.call(context.Background(), "dep", "call", json.RawMessage(`{}`)); err == nil {
			t.Fatal("retained view remained callable")
		}
	})
}

func TestNativeSuppliedCancellationPropagatesTransitively(t *testing.T) {
	innerCanceled := make(chan struct{})
	innerEntered := make(chan struct{})
	innerCapability := extension.ServiceCapability("inner.provider", "test.inner.provider", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	inner := extension.Native{
		Declaration: extension.Declaration{
			ID:           "inner",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "inner-c", Lifetime: extension.LifetimeHarness, Provides: []string{"inner.provider"}}},
			Capabilities: []extension.Capability{innerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"inner.provider": {Service: map[string]extension.Handler{"call": func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
				close(innerEntered)
				<-ctx.Done()
				close(innerCanceled)
				return extension.Result{}, ctx.Err()
			}}}}}, nil
		},
	}
	innerBinding := extension.Binding{Name: "inner", Provider: "inner/inner-c", CapabilityID: "inner.provider", Definition: innerCapability.Definition()}
	providerCapability := extension.ServiceCapability("outer.provider", "test.outer.provider", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	outer := extension.Native{
		Declaration: extension.Declaration{
			ID:           "outer",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "outer-c", Lifetime: extension.LifetimeHarness, Provides: []string{"outer.provider"}, Requires: []extension.Binding{innerBinding}}},
			Capabilities: []extension.Capability{providerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"outer.provider": {Service: map[string]extension.Handler{"call": func(ctx context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
				_, err := call.Call(context.Background(), "inner", "call", json.RawMessage(`{}`))
				return extension.Result{}, err
			}}}}}, nil
		},
	}
	callerCapability := extension.ServiceCapability("outer.caller", "test.outer.caller", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	outerBinding := extension.Binding{Name: "dep", Provider: "outer/outer-c", CapabilityID: "outer.provider", Definition: providerCapability.Definition()}
	caller := extension.Native{
		Declaration: extension.Declaration{
			ID:           "outercaller",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "outercaller-c", Lifetime: extension.LifetimeHarness, Provides: []string{"outer.caller"}, Requires: []extension.Binding{outerBinding}}},
			Capabilities: []extension.Capability{callerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"outer.caller": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
				return extension.Result{Value: json.RawMessage(`{}`)}, nil
			}}}}}, nil
		},
	}
	c := mustComposition(t, inner, outer, caller)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	view, _ := c.newView(rt.capabilities["outer.caller"], context.Background(), callIdentity{})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := view.call(ctx, "dep", "call", json.RawMessage(`{}`)); result <- err }()
	<-innerEntered
	cancel()
	err := <-result
	testassert.Require(t, errors.Is(err, context.Canceled), "transitive call error = %v", err)
	select {
	case <-innerCanceled:
	default:
		t.Fatal("nested Background call did not inherit the supplied cancellation")
	}
	view.end()
}

func TestNativeLifecycleCallStillHonorsSuppliedContext(t *testing.T) {
	handler := &narrowHarness{}
	handler.handler = func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{Value: json.RawMessage(`{}`)}, nil
	}
	provider := handler.native()
	callerCapability := extension.ServiceCapability("lifecycle.caller", "test.lifecycle.caller", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	binding := extension.Binding{Name: "dep", Provider: "narrowprovider/narrowprovider-c", CapabilityID: "narrow.provider", Definition: provider.Declaration.Capabilities[0].Definition()}
	caller := extension.Native{
		Declaration: extension.Declaration{
			ID:           "lifecyclecaller",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "lifecyclecaller-c", Lifetime: extension.LifetimeWorkspace, Provides: []string{"lifecycle.caller"}, Requires: []extension.Binding{binding}}},
			Capabilities: []extension.Capability{callerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"lifecycle.caller": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
				return extension.Result{Value: json.RawMessage(`{}`)}, nil
			}}}}}, nil
		},
	}
	c := mustComposition(t, provider, caller)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	ws := mustOpenScope(t, c, context.Background(), workspaceScopeInfo("/w"), []*scope{rt})
	defer func() { testassert.NoError(t, ws.close()) }()
	view, _ := c.newView(ws.capabilities["lifecycle.caller"], context.Background(), callIdentity{grant: nativeGrant{lifecycle: true}})
	if _, err := view.call(context.Background(), "dep", "call", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("lifecycle call with live Background: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := view.call(ctx, "dep", "call", json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("lifecycle call with canceled supplied context = %v", err)
	}
	view.end()
}

func TestNativeLifecycleProviderUsesOwnSameScopeDependency(t *testing.T) {
	var depCalls atomic.Int64
	depCapability := extension.ServiceCapability("life.dep", "test.life.dep", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	dep := extension.Native{
		Declaration: extension.Declaration{
			ID:           "lifedep",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "lifedep-c", Lifetime: extension.LifetimeHarness, Provides: []string{"life.dep"}}},
			Capabilities: []extension.Capability{depCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"life.dep": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
				depCalls.Add(1)
				return extension.Result{Value: json.RawMessage(`{}`)}, nil
			}}}}}, nil
		},
	}
	depBinding := extension.Binding{Name: "inner", Provider: "lifedep/lifedep-c", CapabilityID: "life.dep", Definition: depCapability.Definition()}
	providerCapability := extension.ServiceCapability("life.provider", "test.life.provider", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	provider := extension.Native{
		Declaration: extension.Declaration{
			ID:           "lifeprovider",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "lifeprovider-c", Lifetime: extension.LifetimeHarness, Provides: []string{"life.provider"}, Requires: []extension.Binding{depBinding}}},
			Capabilities: []extension.Capability{providerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"life.provider": {Service: map[string]extension.Handler{"call": func(ctx context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
				return call.Call(ctx, "inner", "call", json.RawMessage(`{}`))
			}}}}}, nil
		},
	}
	callerCapability := extension.ServiceCapability("life.caller", "test.life.caller", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	providerBinding := extension.Binding{Name: "dep", Provider: "lifeprovider/lifeprovider-c", CapabilityID: "life.provider", Definition: providerCapability.Definition()}
	caller := extension.Native{
		Declaration: extension.Declaration{
			ID:           "lifecaller",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "lifecaller-c", Lifetime: extension.LifetimeOperation, Provides: []string{"life.caller"}, Requires: []extension.Binding{providerBinding}}},
			Capabilities: []extension.Capability{callerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"life.caller": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
				return extension.Result{Value: json.RawMessage(`{}`)}, nil
			}}}}}, nil
		},
	}
	c := mustComposition(t, dep, provider, caller)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	op := mustOpenScope(t, c, context.Background(), ScopeInfo{Kind: ScopeOperation, DataDir: "/d", Workspace: "/w", SessionID: testSessionID, OperationID: "op"}, []*scope{rt})
	defer func() { testassert.NoError(t, op.close()) }()
	view, _ := c.newView(op.capabilities["life.caller"], context.Background(), callIdentity{grant: nativeGrant{lifecycle: true}})
	if _, err := view.call(context.Background(), "dep", "call", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("lifecycle provider same-scope dependency: %v", err)
	}
	testassert.Equal(t, depCalls.Load(), int64(1))
	view.end()
}

func TestNativeLifecycleCallHonorsCancellationDuringBlockedHandler(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	handler := &narrowHarness{}
	handler.handler = func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return extension.Result{}, ctx.Err()
	}
	provider := handler.native()
	callerCapability := extension.ServiceCapability("life.cancel.caller", "test.life.cancel", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	binding := extension.Binding{Name: "dep", Provider: "narrowprovider/narrowprovider-c", CapabilityID: "narrow.provider", Definition: provider.Declaration.Capabilities[0].Definition()}
	caller := extension.Native{
		Declaration: extension.Declaration{
			ID:           "lifecancelcaller",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "lifecancelcaller-c", Lifetime: extension.LifetimeOperation, Provides: []string{"life.cancel.caller"}, Requires: []extension.Binding{binding}}},
			Capabilities: []extension.Capability{callerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				"life.cancel.caller": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
					return extension.Result{Value: json.RawMessage(`{}`)}, nil
				}}},
			}}, nil
		},
	}
	c := mustComposition(t, provider, caller)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	op := mustOpenScope(t, c, context.Background(), ScopeInfo{Kind: ScopeOperation, DataDir: "/d", Workspace: "/w", SessionID: testSessionID, OperationID: "op"}, []*scope{rt})
	defer func() { testassert.NoError(t, op.close()) }()
	view, _ := c.newView(op.capabilities["life.cancel.caller"], context.Background(), callIdentity{grant: nativeGrant{lifecycle: true}})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := view.call(ctx, "dep", "call", json.RawMessage(`{}`)); result <- err }()
	<-started
	cancel()
	err := <-result
	testassert.Require(t, errors.Is(err, context.Canceled), "blocked lifecycle handler error = %v, want cancellation", err)
	<-canceled
	view.end()
}

func TestNativeClosingScopeRefusesOwnScopeDependency(t *testing.T) {
	var bCalls atomic.Int64
	bCapability := testServiceCapability("closing.b")
	b := testNative("closingb", extension.LifetimeHarness, []extension.Capability{bCapability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"closing.b": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
			bCalls.Add(1)
			return extension.Result{Value: json.RawMessage(`{}`)}, nil
		}}}}}, nil
	})
	binding := extension.Binding{Name: "dep", Provider: "closingb/closingb#component", CapabilityID: "closing.b", Definition: bCapability.Definition()}
	aCapability := testServiceCapability("closing.a")
	a := testNative("closinga", extension.LifetimeHarness, []extension.Capability{aCapability}, []extension.Binding{binding}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"closing.a": testServiceImplementation()}, Close: func(ctx context.Context, calls extension.CallContext) error {
			_, err := calls.Call(context.Background(), "dep", "call", json.RawMessage(`{}`))
			return err
		}}, nil
	})
	c := mustComposition(t, a, b)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	err := rt.close()
	testassert.Require(t, errors.Is(err, ErrClosed), "close error = %v, want the closing-scope refusal", err)
	testassert.Equal(t, bCalls.Load(), int64(0))
}

// childTargetProbe prepares one declared permission target and launches one
// complete reference through the declared host service.
type childTargetProbe struct {
	target    string
	launch    harness.DefinitionRef
	executes  atomic.Int64
	childID   string
	launchErr error
}

func (p *childTargetProbe) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}

func (p *childTargetProbe) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionChildLaunch, Target: p.target}}}
}

func (p *childTargetProbe) Execute(ctx context.Context, tc extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	p.executes.Add(1)
	result, err := tc.Calls.Call(ctx, "child_launch", "launch", childLaunchRefArguments(p.launch, "op-child"))
	if err != nil {
		p.launchErr = err
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: tc.Calls.ToolCallID, Status: model.ResultError, Content: err.Error()}}
	}
	var launched struct {
		ChildSessionID string `json:"child_session_id"`
	}
	if err := json.Unmarshal(result.Value, &launched); err != nil {
		p.launchErr = err
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: tc.Calls.ToolCallID, Status: model.ResultError, Content: err.Error()}}
	}
	p.childID = launched.ChildSessionID
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: tc.Calls.ToolCallID, Status: model.ResultSuccess, Content: "launched " + launched.ChildSessionID}}
}

// TestNativeChildLaunchRequiresCompleteCanonicalTarget proves the real native
// plan -> Harness authorization -> host child launch path authorizes the
// complete canonical target: an own reference and an explicitly prepared
// foreign reference admit exactly that reference, while a same-ID foreign
// substitution and a bare definition-ID target admit no child.
func TestNativeChildLaunchRequiresCompleteCanonicalTarget(t *testing.T) {
	own := harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: "shared"}
	foreign := harness.DefinitionRef{ExtensionID: "guestdefs", CapabilityID: "guestdefs.definitions", DefinitionID: "shared"}
	for _, row := range []struct {
		name       string
		target     string
		launch     harness.DefinitionRef
		wantLaunch bool
		wantRef    harness.DefinitionRef
	}{
		{name: "exact own canonical target", target: own.PermissionTarget(), launch: own, wantLaunch: true, wantRef: own},
		{name: "canonical own target rejects same-ID foreign reference", target: own.PermissionTarget(), launch: foreign},
		{name: "bare definition ID target authorizes nothing", target: own.DefinitionID, launch: own},
		{name: "bare target admits no same-ID foreign reference", target: own.DefinitionID, launch: foreign},
		{name: "explicit foreign canonical target", target: foreign.PermissionTarget(), launch: foreign, wantLaunch: true, wantRef: foreign},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := newOwnerEnv(t)
			store := storage.NewMemory()
			e.fixtureStore(store)
			enableFixturePlugin(t, e.configPath, "grant")
			enableFixturePlugin(t, e.configPath, "guestdefs")
			writeAgentDefinitions(t, e.configPath, `{"grant_agent":{"model":"prov/m","system_prompt":"simple","tools":["grant_tool"]},"shared":{"model":"prov/m","system_prompt":"simple","subagent":true}}`)
			e.server.setScript(func(_ context.Context, body string) []string {
				if lastMessageRole(body) == "tool" {
					return textTurnEvents("done")
				}
				if lastUserText(body) == "run" {
					return toolCallTurnEvents("call-target", "grant_tool", `{}`)
				}
				return textTurnEvents("child done")
			})
			probe := &childTargetProbe{target: row.target, launch: row.launch}
			ctx := context.Background()
			r, err := e.open(ctx, childGrantNative(probe), guestResolverNativeDefinition(t, "shared"))
			testassert.NoError(t, err)
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			session, err := r.createSession(ctx, e.dataDir, "grant_agent")
			testassert.NoError(t, err)
			submitThroughRuntime(t, r, session.Identity.SessionID, "op-target", "run")
			awaitOperation(t, r, session.Identity.SessionID, "op-target", harness.OperationSuccess)
			testassert.Require(t, probe.executes.Load() == 1, "probe Execute calls = %d, want one", probe.executes.Load())
			if !row.wantLaunch {
				testassert.Require(t, probe.launchErr != nil && strings.Contains(probe.launchErr.Error(), "not authorized by the prepared plan"), "launch error = %v, want the frozen-target refusal", probe.launchErr)
				testassert.Require(t, probe.childID == "", "child id = %q, want no admitted child", probe.childID)
				testassert.Equal(t, bridgeChildCount(t, r, store), 0)
				return
			}
			testassert.NoError(t, probe.launchErr)
			testassert.Require(t, len(probe.childID) == 32, "child id = %q (launch error %v), want a committed child", probe.childID, probe.launchErr)
			header, err := r.harness.ReadSessionHeader(ctx, probe.childID)
			testassert.NoError(t, err)
			testassert.Require(t, header.AgentSelection.Definition != nil && *header.AgentSelection.Definition == row.wantRef, "child session selection = %+v, want %+v", header.AgentSelection.Definition, row.wantRef)
			operation, err := r.harness.ReadOperation(ctx, probe.childID, "op-child")
			testassert.NoError(t, err)
			testassert.Require(t, operation.Admission.AgentSelection.Definition != nil && *operation.Admission.AgentSelection.Definition == row.wantRef, "child operation selection = %+v, want %+v", operation.Admission.AgentSelection.Definition, row.wantRef)
		})
	}
}

// childGrantProbe launches through the declared host service: it first
// mutates the plan the native executor received, then its own retained plan
// buffer, and finally asks for the originally declared target. The Harness
// authorization and the private frozen grant must both be immune to those
// mutations.
type childGrantProbe struct {
	executes    atomic.Int64
	mutatedErr  error
	retainedErr error
	childID     string
	retained    extension.ToolPlan
}

func (p *childGrantProbe) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}

func (p *childGrantProbe) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	p.retained = extension.ToolPlan{
		Permissions: []harness.PermissionRequest{{Permission: harness.PermissionChildLaunch, Target: (harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: "secondary"}).PermissionTarget()}},
		Data:        []byte("secondary"),
	}
	return p.retained
}

func (p *childGrantProbe) Execute(ctx context.Context, tc extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	p.executes.Add(1)
	plan.Permissions[0].Target = "other"
	_, p.mutatedErr = tc.Calls.Call(ctx, "child_launch", "launch", childLaunchArguments("other"))
	p.retained.Permissions[0].Target = "other"
	_, p.retainedErr = tc.Calls.Call(ctx, "child_launch", "launch", childLaunchArguments("other"))
	result, err := tc.Calls.Call(ctx, "child_launch", "launch", childLaunchArguments("secondary"))
	if err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	var launched struct {
		ChildSessionID string `json:"child_session_id"`
	}
	if err := json.Unmarshal(result.Value, &launched); err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	p.childID = launched.ChildSessionID
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "launched " + launched.ChildSessionID}}
}

func childGrantNative(probe extension.Tool) extension.Native {
	childLaunch, _ := extension.HostCapability(extension.ContractChildLaunch)
	native := testNative("grant", extension.LifetimeHarness, []extension.Capability{extension.ToolCapability("grant_tool")}, []extension.Binding{{
		Name: "child_launch", Provider: extension.HostProvider, CapabilityID: extension.ContractChildLaunch, Definition: childLaunch,
	}}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"grant_tool": {Tool: probe}}}, nil
	})
	native.Describe["grant_tool"] = testDescribeTool("grant_tool")
	return native
}

// TestNativeChildLaunchGrantIgnoresPlanMutation proves the real Harness path:
// the declared target is admitted, the same caller's mutation of the executor
// plan and of its retained plan buffer cannot widen the private grant, and the
// actual child Session exists.
func TestNativeChildLaunchGrantIgnoresPlanMutation(t *testing.T) {
	e := newOwnerEnv(t)
	probe := &childGrantProbe{}
	enableFixturePlugin(t, e.configPath, "grant")
	writeAgentDefinitions(t, e.configPath, `{"grant_agent":{"model":"prov/m","system_prompt":"simple","tools":["grant_tool"]},"secondary":{"model":"prov/m","system_prompt":"simple","subagent":true}}`)
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		if lastUserText(body) == "run" {
			return toolCallTurnEvents("call-grant", "grant_tool", `{}`)
		}
		return textTurnEvents("child done")
	})
	ctx := context.Background()
	r, err := e.open(ctx, childGrantNative(probe))
	testassert.NoError(t, err)
	session, err := r.createSession(ctx, e.dataDir, "grant_agent")
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-grant", "run")
	awaitOperation(t, r, session.Identity.SessionID, "op-grant", harness.OperationSuccess)
	testassert.Require(t, probe.mutatedErr != nil && strings.Contains(probe.mutatedErr.Error(), "not authorized by the prepared plan"), "mutated executor-plan launch = %v, want the frozen-target refusal", probe.mutatedErr)
	testassert.Require(t, probe.retainedErr != nil && strings.Contains(probe.retainedErr.Error(), "not authorized by the prepared plan"), "mutated retained-plan launch = %v, want the frozen-target refusal", probe.retainedErr)
	testassert.Require(t, len(probe.childID) == 32 && probe.executes.Load() == 1, "child id = %q (executes %d), want one admitted launch", probe.childID, probe.executes.Load())
	err = r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		_, err := h.ReadSessionHeader(ctx, probe.childID)
		return err
	})
	testassert.NoError(t, err)
	testassert.NoError(t, r.Close(ctx))
}

// TestNativeChildLaunchDeniedPlanDoesNotExecute proves the deny side of the
// same real Harness boundary: a declared plan denied by the configured policy
// never reaches the executor, and the next model request receives the fixed
// denied result instead.
func TestNativeChildLaunchDeniedPlanDoesNotExecute(t *testing.T) {
	e := newOwnerEnv(t)
	writeServiceFile(t, e.configPath, strings.TrimSuffix(ownerConfigDocument(e.server.URL), "}")+`,"permissions":{"rules":[{"permission":"child.launch","target":"agents/agents.definitions/secondary","access":"deny"}]}}`)
	enableFixturePlugin(t, e.configPath, "grant")
	writeAgentDefinitions(t, e.configPath, `{"grant_agent":{"model":"prov/m","system_prompt":"simple","tools":["grant_tool"]},"secondary":{"model":"prov/m","system_prompt":"simple","subagent":true}}`)
	probe := &childGrantProbe{}
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		return toolCallTurnEvents("call-grant", "grant_tool", `{}`)
	})
	ctx := context.Background()
	r, err := e.open(ctx, childGrantNative(probe))
	testassert.NoError(t, err)
	session, err := r.createSession(ctx, e.dataDir, "grant_agent")
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-grant", "run")
	awaitOperation(t, r, session.Identity.SessionID, "op-grant", harness.OperationSuccess)
	testassert.Equal(t, probe.executes.Load(), int64(0))
	testassert.Require(t, e.server.requests() == 2, "model requests = %d, want the tool-call turn and its denied follow-up", e.server.requests())
	testassert.Require(t, strings.Contains(e.server.bodyAt(1), harness.PermissionDeniedContent), "denied follow-up request = %s, want the fixed denied content", e.server.bodyAt(1))
	testassert.NoError(t, r.Close(ctx))
}

// normalizeBlockTool normalizes by calling its declared pure provider with a
// detached author context: only the view origin can ever cancel that call.
type normalizeBlockTool struct{}

func (normalizeBlockTool) Normalize(tc extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	_, err := tc.Calls.Call(context.Background(), "dep", "wait", json.RawMessage(`{}`))
	return nil, err
}
func (normalizeBlockTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionCommandRun, Target: "test"}}}
}
func (normalizeBlockTool) Execute(context.Context, extension.ToolContext, extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "ok"}}
}

// TestNativeNormalizeUsesInvocationCancellation proves Normalize's view origin
// is the actual invocation context, not the component's Harness/Workspace
// scope: cancelling the invocation interrupts a waiting pure provider.
func TestNativeNormalizeUsesInvocationCancellation(t *testing.T) {
	for _, lifetime := range []extension.Lifetime{extension.LifetimeHarness, extension.LifetimeWorkspace} {
		t.Run(string(lifetime), func(t *testing.T) {
			started := make(chan struct{})
			providerCapability := extension.ServiceCapability("norm.provider", "test.norm.provider", extension.ServiceMethod("wait", extension.ModePure, `{}`, `{}`, "", ""))
			provider := testNative("normprovider", extension.LifetimeHarness, []extension.Capability{providerCapability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
				return extension.Instance{Capabilities: map[string]extension.Implementation{"norm.provider": {Service: map[string]extension.Handler{"wait": func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
					close(started)
					<-ctx.Done()
					return extension.Result{}, ctx.Err()
				}}}}}, nil
			})
			binding := extension.Binding{Name: "dep", Provider: "normprovider/normprovider#component", CapabilityID: "norm.provider", Definition: providerCapability.Definition()}
			tool := testNative("normtool", lifetime, []extension.Capability{extension.ToolCapability("norm_tool")}, []extension.Binding{binding}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
				return extension.Instance{Capabilities: map[string]extension.Implementation{"norm_tool": {Tool: normalizeBlockTool{}}}}, nil
			})
			tool.Describe["norm_tool"] = testDescribeTool("norm_tool")
			c := mustComposition(t, provider, tool)
			rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
			defer func() { testassert.NoError(t, rt.close()) }()
			scopes := []*scope{rt}
			if lifetime == extension.LifetimeWorkspace {
				ws := mustOpenScope(t, c, context.Background(), workspaceScopeInfo("/w"), []*scope{rt})
				defer func() { testassert.NoError(t, ws.close()) }()
				scopes = append(scopes, ws)
			}
			bound, err := c.bindTool(selection{scopes: scopes, invocation: Invocation{}}, "norm_tool")
			testassert.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := bound.Normalize(ctx, ToolContext{}, model.ToolCall{ID: "call-norm", Name: "norm_tool", Arguments: json.RawMessage(`{}`)})
				done <- err
			}()
			<-started
			cancel()
			select {
			case err := <-done:
				testassert.Require(t, errors.Is(err, context.Canceled), "Normalize error = %v, want invocation cancellation", err)
			case <-time.After(10 * time.Second):
				t.Fatal("Normalize did not observe invocation cancellation while the pure provider waited")
			}
		})
	}
}

// ctxProviderTool calls its declared provider with a detached author context:
// only the admitted provider view can cancel the waiting handler.
type ctxProviderTool struct{}

func (ctxProviderTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (ctxProviderTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionCommandRun, Target: "test"}}}
}
func (ctxProviderTool) Execute(ctx context.Context, tc extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	if _, err := tc.Calls.Call(context.Background(), "dep", "wait", json.RawMessage(`{}`)); err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "ok"}}
}

// TestNativeHandlerReceivesAdmittedProviderContext proves the provider handler
// runs on the admitted provider view, not on the caller's context: cancelling
// the provider scope interrupts a handler whose caller context stays live.
func TestNativeHandlerReceivesAdmittedProviderContext(t *testing.T) {
	started := make(chan struct{})
	released := make(chan struct{})
	providerCapability := extension.ServiceCapability("ctx.provider", "test.ctx.provider", extension.ServiceMethod("wait", extension.ModeServiceIO, `{}`, `{}`, "", ""))
	provider := testNative("ctxprovider", extension.LifetimeHarness, []extension.Capability{providerCapability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"ctx.provider": {Service: map[string]extension.Handler{"wait": func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
			close(started)
			<-ctx.Done()
			close(released)
			return extension.Result{}, ctx.Err()
		}}}}}, nil
	})
	binding := extension.Binding{Name: "dep", Provider: "ctxprovider/ctxprovider#component", CapabilityID: "ctx.provider", Definition: providerCapability.Definition()}
	caller := testNative("ctxcaller", extension.LifetimeWorkspace, []extension.Capability{extension.ToolCapability("ctx_tool")}, []extension.Binding{binding}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"ctx_tool": {Tool: ctxProviderTool{}}}}, nil
	})
	caller.Describe["ctx_tool"] = testDescribeTool("ctx_tool")
	c := mustComposition(t, provider, caller)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	ws := mustOpenScope(t, c, context.Background(), workspaceScopeInfo("/w"), []*scope{rt})
	bound, err := c.bindTool(selection{scopes: []*scope{rt, ws}, invocation: Invocation{}}, "ctx_tool")
	testassert.NoError(t, err)
	plan := bound.Prepare(context.Background(), ToolContext{}, model.ToolCall{ID: "call-ctx", Name: "ctx_tool", Arguments: json.RawMessage(`{}`)})
	testassert.Require(t, plan.Execute != nil, "plan = %+v, want an executor", plan)
	done := make(chan harness.ToolOutcome, 1)
	go func() { done <- plan.Execute(context.Background()) }()
	<-started
	rt.cancel()
	select {
	case outcome := <-done:
		testassert.Require(t, outcome.Result.Status == model.ResultError && outcome.Result.Content == context.Canceled.Error(), "outcome = %+v, want the provider cancellation", outcome.Result)
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not observe its admitted provider cancellation")
	}
	select {
	case <-released:
	default:
		t.Fatal("the handler returned without observing the provider context cancellation")
	}
	testassert.NoError(t, ws.close())
}

type registrationTool struct{ executes *atomic.Int64 }

func (t registrationTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (registrationTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionCommandRun, Target: "test"}}}
}
func (t registrationTool) Execute(context.Context, extension.ToolContext, extension.ToolPlan) harness.ToolOutcome {
	t.executes.Add(1)
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "owned"}}
}

// TestNativeRegistrationDataIsOwned proves the Runtime's accepted
// registration data is deep-owned: the caller may mutate every nested
// declaration slice/byte buffer and the Describe map after Open, and a
// replacing reload still compiles and advertises the original registration.
func TestNativeRegistrationDataIsOwned(t *testing.T) {
	e := newOwnerEnv(t)
	enableFixturePlugin(t, e.configPath, "owned_tool")
	writeAgentDefinitions(t, e.configPath, `{"owned_agent":{"model":"prov/m","system_prompt":"simple","tools":["owned_tool"]}}`)
	e.fixtureStore(storage.NewMemory())
	var executes atomic.Int64
	native := testToolNative("owned_tool", extension.LifetimeHarness, registrationTool{executes: &executes}, func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
		return extension.ToolDescription{
			Definition: model.ToolDefinition{Name: "owned_tool", Description: "original registration description", Parameters: json.RawMessage(`{"type":"object"}`)},
			Available:  true,
		}, nil
	})
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		return toolCallTurnEvents("call-owned", "owned_tool", `{}`)
	})
	ctx := context.Background()
	r, err := e.open(ctx, native)
	testassert.NoError(t, err)
	// Caller-retained registration data is mutated after acceptance.
	native.Declaration.Capabilities[0].ID = "mutated_tool"
	native.Declaration.Components[0].Provides[0] = "mutated_tool"
	native.Declaration.ConfigSchema = json.RawMessage(`{"type":`)
	native.Declaration.Components[0].Requires = append(native.Declaration.Components[0].Requires, extension.Binding{Name: "ghost"})
	native.Describe["owned_tool"] = func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
		return extension.ToolDescription{Definition: model.ToolDefinition{Name: "owned_tool", Description: "mutated description", Parameters: json.RawMessage(`{}`)}, Available: false}, nil
	}
	native.Describe["ghost_tool"] = native.Describe["owned_tool"]
	// A replacing reload changes the active section bytes and must compile the
	// owned registration, not the caller's mutated value.
	data, err := os.ReadFile(e.configPath)
	testassert.NoError(t, err)
	replaced := strings.Replace(string(data), `{"enabled":true}`, `{"enabled":true }`, 1)
	testassert.Require(t, replaced != string(data), "fixture config carried no enabled section to replace")
	writeServiceFile(t, e.configPath, replaced)
	revision, err := r.reload(ctx, protocolCaller())
	testassert.NoError(t, err)
	testassert.Require(t, revision != "", "Reload returned no revision")
	session, err := r.createSession(ctx, e.dataDir, "owned_agent")
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-owned", "run")
	awaitOperation(t, r, session.Identity.SessionID, "op-owned", harness.OperationSuccess)
	testassert.Equal(t, executes.Load(), int64(1))
	testassert.Require(t, e.server.requests() >= 2, "model requests = %d, want the tool call and follow-up", e.server.requests())
	testassert.Require(t, strings.Contains(e.server.bodyAt(0), "original registration description"), "advertised tool = %s, want the owned original description", e.server.bodyAt(0))
	testassert.Require(t, !strings.Contains(e.server.bodyAt(0), "mutated description") && !strings.Contains(e.server.bodyAt(0), "ghost_tool"), "advertised tools = %s, want no caller-mutated registration", e.server.bodyAt(0))
	testassert.NoError(t, r.Close(ctx))
}

// TestNativeAcceptedInstanceServiceMapIsOwned proves the accepted instance's
// Capability/Service map containers are copied at Open: a caller-retained
// service map deletion cannot remove or replace the accepted binding
// implementation.
func TestNativeAcceptedInstanceServiceMapIsOwned(t *testing.T) {
	providerCapability := extension.ServiceCapability("inst.provider", "test.inst.provider", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	var calls atomic.Int64
	retained := map[string]extension.Handler{}
	provider := testNative("instprovider", extension.LifetimeHarness, []extension.Capability{providerCapability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		retained["call"] = func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
			calls.Add(1)
			return extension.Result{Value: json.RawMessage(`{}`)}, nil
		}
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"inst.provider": {Service: retained},
		}}, nil
	})
	binding := extension.Binding{Name: "dep", Provider: "instprovider/instprovider#component", CapabilityID: "inst.provider", Definition: providerCapability.Definition()}
	caller := testToolNative("instcaller", extension.LifetimeHarness, instanceMapTool{}, testDescribeTool("instcaller"))
	caller.Declaration.Components[0].Requires = []extension.Binding{binding}
	c := mustComposition(t, provider, caller)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	// The provider instance is open; caller retention mutates its map container.
	retained["call"] = func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{}, errors.New("mutated handler")
	}
	delete(retained, "call")
	retained["other"] = func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{Value: json.RawMessage(`{}`)}, nil
	}
	tool, err := c.bindTool(selection{scopes: []*scope{rt}, invocation: Invocation{}}, "instcaller")
	testassert.NoError(t, err)
	plan := tool.Prepare(context.Background(), ToolContext{}, model.ToolCall{ID: "call-inst", Name: "instcaller", Arguments: json.RawMessage(`{}`)})
	testassert.Require(t, plan.Execute != nil, "plan = %+v, want execution", plan)
	outcome := plan.Execute(context.Background())
	testassert.Require(t, outcome.Result.Status == model.ResultSuccess, "outcome = %+v, want the accepted service implementation, not the caller-mutated map", outcome.Result)
	testassert.Equal(t, calls.Load(), int64(1))
}

type instanceMapTool struct{}

func (instanceMapTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (instanceMapTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionCommandRun, Target: "test"}}}
}
func (instanceMapTool) Execute(ctx context.Context, tc extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	if _, err := tc.Calls.Call(ctx, "dep", "call", json.RawMessage(`{}`)); err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "ok"}}
}

// TestNativeCloseReceivesFreshFiveSecondDeadline proves each Close callback
// runs on a fresh cooperative deadline in the retained five-second magnitude,
// rather than on the process Background.
func TestNativeCloseReceivesFreshFiveSecondDeadline(t *testing.T) {
	var mu sync.Mutex
	var deadlines []time.Duration
	var hasDeadline []bool
	options := []string{"close.a", "close.b"}
	native := extension.Native{
		Declaration: extension.Declaration{
			ID:           "closedeadline",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components: []extension.Component{
				{ID: "close-a-c", Lifetime: extension.LifetimeHarness, Provides: []string{"close.a"}},
				{ID: "close-b-c", Lifetime: extension.LifetimeHarness, Provides: []string{"close.b"}},
			},
			Capabilities: []extension.Capability{
				extension.ServiceCapability("close.a", "test.close.a", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", "")),
				extension.ServiceCapability("close.b", "test.close.b", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", "")),
			},
		},
		Open: func(_ context.Context, info extension.InstanceContext) (extension.Instance, error) {
			index := 0
			if info.ComponentID == "close-b-c" {
				index = 1
			}
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				options[index]: {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
					return extension.Result{Value: json.RawMessage(`{}`)}, nil
				}}},
			}, Close: func(ctx context.Context, _ extension.CallContext) error {
				deadline, ok := ctx.Deadline()
				mu.Lock()
				hasDeadline = append(hasDeadline, ok)
				if ok {
					deadlines = append(deadlines, time.Until(deadline))
				}
				mu.Unlock()
				return nil
			}}, nil
		},
	}
	c := mustComposition(t, native)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	testassert.NoError(t, rt.close())
	mu.Lock()
	defer mu.Unlock()
	testassert.Require(t, len(hasDeadline) == 2, "close callbacks = %d, want both", len(hasDeadline))
	for i, ok := range hasDeadline {
		testassert.Require(t, ok, "closer %d received no deadline", i)
	}
	for i, remaining := range deadlines {
		testassert.Require(t, remaining > 0 && remaining <= 5*time.Second, "closer %d remaining deadline = %v, want the retained five-second cooperative bound", i, remaining)
	}
}

// TestNativeCloseCooperativeDeadlineRunsRemainingCleanup proves the deadline
// is cooperative and per callback: a timed-out handler gains cancellation,
// the closer's own returned error survives the expired deadline, every
// remaining closer still runs with a fresh bound, and all errors join.
func TestNativeCloseCooperativeDeadlineRunsRemainingCleanup(t *testing.T) {
	previous := nativeCloseDeadline
	nativeCloseDeadline = 50 * time.Millisecond
	t.Cleanup(func() { nativeCloseDeadline = previous })

	var mu sync.Mutex
	var ran []string
	var remainingAtStart []time.Duration
	providerCapability := extension.ServiceCapability("coop.provider", "test.coop.provider", extension.ServiceMethod("wait", extension.ModeServiceIO, `{}`, `{}`, "", ""))
	provider := testNative("coopprovider", extension.LifetimeHarness, []extension.Capability{providerCapability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"coop.provider": {Service: map[string]extension.Handler{"wait": func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
			<-ctx.Done()
			return extension.Result{}, ctx.Err()
		}}}}}, nil
	})
	binding := extension.Binding{Name: "dep", Provider: "coopprovider/coopprovider#component", CapabilityID: "coop.provider", Definition: providerCapability.Definition()}
	native := extension.Native{
		Declaration: extension.Declaration{
			ID:           "coopcloser",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components: []extension.Component{{
				ID: "coop-c", Lifetime: extension.LifetimeWorkspace, Provides: []string{"coop.close"}, Requires: []extension.Binding{binding},
			}},
			Capabilities: []extension.Capability{extension.ServiceCapability("coop.close", "test.coop.close", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{}`, "", ""))},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"coop.close": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
				return extension.Result{Value: json.RawMessage(`{}`)}, nil
			}}}}, Close: func(ctx context.Context, calls extension.CallContext) error {
				deadline, _ := ctx.Deadline()
				mu.Lock()
				remainingAtStart = append(remainingAtStart, time.Until(deadline))
				mu.Unlock()
				_, callErr := calls.Call(ctx, "dep", "wait", json.RawMessage(`{}`))
				mu.Lock()
				ran = append(ran, "cooperative")
				mu.Unlock()
				if !errors.Is(callErr, context.DeadlineExceeded) {
					return fmt.Errorf("cooperative close call = %v, want the expired cooperative deadline", callErr)
				}
				return errors.New("cooperative cleanup result")
			}}, nil
		},
	}
	// Two independent sibling closers: both must still run after the first
	// deadline expires, and both retain their own returned errors.
	second := testNative("secondcloser", extension.LifetimeWorkspace, []extension.Capability{extension.ServiceCapability("second.close", "test.second.close", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{}`, "", ""))}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"second.close": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
			return extension.Result{Value: json.RawMessage(`{}`)}, nil
		}}}}, Close: func(ctx context.Context, _ extension.CallContext) error {
			deadline, _ := ctx.Deadline()
			mu.Lock()
			remainingAtStart = append(remainingAtStart, time.Until(deadline))
			ran = append(ran, "second")
			mu.Unlock()
			return errors.New("second cleanup result")
		}}, nil
	})
	third := testNative("thirdcloser", extension.LifetimeWorkspace, []extension.Capability{extension.ServiceCapability("third.close", "test.third.close", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{}`, "", ""))}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"third.close": {Service: map[string]extension.Handler{"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
			return extension.Result{Value: json.RawMessage(`{}`)}, nil
		}}}}, Close: func(context.Context, extension.CallContext) error {
			mu.Lock()
			ran = append(ran, "third")
			mu.Unlock()
			return nil
		}}, nil
	})
	c := mustComposition(t, provider, native, second, third)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	ws := mustOpenScope(t, c, context.Background(), workspaceScopeInfo("/w"), []*scope{rt})
	err := ws.close()
	testassert.Require(t, errors.Is(err, context.DeadlineExceeded) || strings.Contains(fmt.Sprint(err), "cooperative cleanup result"), "close error = %v, want the closer's returned cleanup result preserved", err)
	testassert.Require(t, strings.Contains(fmt.Sprint(err), "cooperative cleanup result") && strings.Contains(fmt.Sprint(err), "second cleanup result"), "close error = %v, want both closer results joined", err)
	mu.Lock()
	defer mu.Unlock()
	testassert.Require(t, len(ran) == 3 && slices.Contains(ran, "cooperative") && slices.Contains(ran, "second") && slices.Contains(ran, "third"), "close callbacks run = %v, want every remaining closer", ran)
	for i, remaining := range remainingAtStart {
		testassert.Require(t, remaining > 0 && remaining <= nativeCloseDeadline, "closer %d remaining deadline = %v, want its own fresh bound %v", i, remaining, nativeCloseDeadline)
	}
}

// TestNativeDescribeDefinitionBytesAreOwned proves a Describe callback's
// returned tool definition is owned before preparation hooks run: a hook
// mutating the producer's retained parameter bytes cannot alter the captured
// advertised definition.
func TestNativeDescribeDefinitionBytesAreOwned(t *testing.T) {
	e := newOwnerEnv(t)
	enableFixturePlugin(t, e.configPath, "metaext")
	writeAgentDefinitions(t, e.configPath, `{"meta_agent":{"model":"prov/m","system_prompt":"simple","tools":["meta_tool"],"capabilities":["prepare.meta"]}}`)
	e.fixtureStore(storage.NewMemory())
	params := []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	mutated := false
	native := testNative("metaext", extension.LifetimeHarness, []extension.Capability{extension.ToolCapability("meta_tool")}, nil, nil)
	native.Describe = map[string]extension.ToolDescribe{"meta_tool": func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
		return extension.ToolDescription{Definition: model.ToolDefinition{Name: "meta_tool", Description: "meta description", Parameters: params}, Available: true}, nil
	}}
	native.Declaration.Capabilities = append(native.Declaration.Capabilities, extension.HookCapability("prepare.meta", extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure)))
	native.Declaration.Components[0].Provides = append(native.Declaration.Components[0].Provides, "prepare.meta")
	open := native.Open
	native.Open = func(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
		if instance.ComponentID != native.Declaration.Components[0].ID {
			return open(ctx, instance)
		}
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"meta_tool": {Tool: noopNativeTool{}},
			"prepare.meta": {Preparation: func(context.Context, extension.CallContext, extension.PreparationInput) (extension.PreparationConsequence, error) {
				if !mutated {
					mutated = true
					params[2] = 'x'
				}
				return extension.PreparationConsequence{}, nil
			}},
		}}, nil
	}
	e.server.setScript(func(_ context.Context, _ string) []string { return textTurnEvents("done") })
	ctx := context.Background()
	r, err := e.open(ctx, native)
	testassert.NoError(t, err)
	session, err := r.createSession(ctx, e.dataDir, "meta_agent")
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-meta", "hello")
	rec := awaitOperation(t, r, session.Identity.SessionID, "op-meta", harness.OperationSuccess)
	testassert.Require(t, mutated, "the preparation hook did not mutate the producer's retained parameter bytes")
	testassert.Require(t, len(rec.Admission.Execution.Tools) == 1 && string(rec.Admission.Execution.Tools[0].Parameters) == `{"type":"object","properties":{"path":{"type":"string"}}}`, "captured parameters = %s, want the owned original definition bytes", rec.Admission.Execution.Tools[0].Parameters)
	testassert.NoError(t, r.Close(ctx))
}

type invalidImmediateTool struct{}

func (invalidImmediateTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (invalidImmediateTool) Prepare(_ context.Context, _ extension.ToolContext, _ model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionCommandRun, Target: "test"}}, Immediate: &harness.ToolOutcome{Result: model.ToolResult{Status: model.ResultSuccess, Content: "immediate without identity"}}}
}
func (invalidImmediateTool) Execute(context.Context, extension.ToolContext, extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "must not run"}}
}

type invalidResultTool struct{}

func (invalidResultTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (invalidResultTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionCommandRun, Target: "test"}}}
}
func (invalidResultTool) Execute(context.Context, extension.ToolContext, extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "result without identity"}}
}

// TestNativeInvalidPreparedAndReturnedResultsAreSettledByHarness proves the
// real native producer's malformed outcomes reach the shared Harness
// settlement: an immediate success without the actual call identity settles
// as the internal-validation result without execution, and an executor
// success without identity settles as unknown rather than being stamped.
func TestNativeInvalidPreparedAndReturnedResultsAreSettledByHarness(t *testing.T) {
	for _, row := range []struct {
		name    string
		tool    extension.Tool
		want    string
		execut  int64
		resetFn func(*atomic.Int64)
	}{
		{name: "invalid immediate", tool: invalidImmediateTool{}, want: "Tool call failed internal validation."},
		{name: "invalid executor result", tool: invalidResultTool{}, want: "Execution outcome unknown"},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := newOwnerEnv(t)
			enableFixturePlugin(t, e.configPath, "invalid_result")
			writeAgentDefinitions(t, e.configPath, `{"invalid_agent":{"model":"prov/m","system_prompt":"simple","tools":["invalid_tool"]}}`)
			e.fixtureStore(storage.NewMemory())
			native := testNative("invalid_result", extension.LifetimeHarness, []extension.Capability{extension.ToolCapability("invalid_tool")}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
				return extension.Instance{Capabilities: map[string]extension.Implementation{"invalid_tool": {Tool: row.tool}}}, nil
			})
			native.Describe["invalid_tool"] = testDescribeTool("invalid_tool")
			e.server.setScript(func(_ context.Context, body string) []string {
				if lastMessageRole(body) == "tool" {
					return textTurnEvents("done")
				}
				return toolCallTurnEvents("call-invalid", "invalid_tool", `{}`)
			})
			ctx := context.Background()
			r, err := e.open(ctx, native)
			testassert.NoError(t, err)
			session, err := r.createSession(ctx, e.dataDir, "invalid_agent")
			testassert.NoError(t, err)
			submitThroughRuntime(t, r, session.Identity.SessionID, "op-invalid", "run")
			awaitOperation(t, r, session.Identity.SessionID, "op-invalid", harness.OperationSuccess)
			testassert.Require(t, e.server.requests() >= 2, "model requests = %d, want the call and follow-up", e.server.requests())
			testassert.Require(t, strings.Contains(e.server.bodyAt(1), row.want), "follow-up request = %s, want %q", e.server.bodyAt(1), row.want)
			testassert.NoError(t, r.Close(ctx))
		})
	}
}

// publicationIntentStore injects one failure at the real Harness tool-effect
// intent publication, leaving the durable running state for recovery.
type publicationIntentStore struct {
	harness.Storage
	armed atomic.Bool
	fired atomic.Bool
}

type publicationIntentTx struct {
	harness.Transaction
	owner *publicationIntentStore
}

func (t publicationIntentTx) ReplaceRegister(key harness.RegisterKey, revision int64, payload json.RawMessage) (harness.Register, error) {
	if t.owner.armed.Load() && key.Kind == harness.RegisterOperation && bytes.Contains(payload, []byte(`"kind":"tool"`)) {
		t.owner.fired.Store(true)
		return harness.Register{}, errors.New("injected tool intent publication failure")
	}
	return t.Transaction.ReplaceRegister(key, revision, payload)
}

func (s *publicationIntentStore) Transact(ctx context.Context, fn func(harness.Transaction) error) error {
	return s.Storage.Transact(ctx, func(tx harness.Transaction) error {
		return fn(publicationIntentTx{Transaction: tx, owner: s})
	})
}

type intentFailureTool struct{ executes *atomic.Int64 }

func (t intentFailureTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (intentFailureTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionCommandRun, Target: "test"}}}
}
func (t intentFailureTool) Execute(context.Context, extension.ToolContext, extension.ToolPlan) harness.ToolOutcome {
	t.executes.Add(1)
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "must not run"}}
}

// TestNativeToolIntentPublicationFailureRetainsRunningState proves the real
// native tool reaches the shared Harness publication boundary: a failed tool
// intent commits no execution, no result, and leaves the running Operation for
// recovery rather than fabricating a terminal.
func TestNativeToolIntentPublicationFailureRetainsRunningState(t *testing.T) {
	e := newOwnerEnv(t)
	enableFixturePlugin(t, e.configPath, "intentext")
	writeAgentDefinitions(t, e.configPath, `{"intent_agent":{"model":"prov/m","system_prompt":"simple","tools":["intent_tool"]}}`)
	store := &publicationIntentStore{Storage: storage.NewMemory()}
	e.fixtureStore(store)
	var executes atomic.Int64
	native := testNative("intentext", extension.LifetimeHarness, []extension.Capability{extension.ToolCapability("intent_tool")}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"intent_tool": {Tool: intentFailureTool{executes: &executes}}}}, nil
	})
	native.Describe["intent_tool"] = testDescribeTool("intent_tool")
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		return toolCallTurnEvents("call-intent", "intent_tool", `{}`)
	})
	ctx := context.Background()
	r, err := e.open(ctx, native)
	testassert.NoError(t, err)
	session, err := r.createSession(ctx, e.dataDir, "intent_agent")
	testassert.NoError(t, err)
	store.armed.Store(true)
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-intent", "run")
	deadline := time.Now().Add(10 * time.Second)
	for !store.fired.Load() {
		testassert.Require(t, time.Now().Before(deadline), "the tool intent publication was never attempted")
		time.Sleep(5 * time.Millisecond)
	}
	rec, err := r.harness.ReadOperation(ctx, session.Identity.SessionID, "op-intent")
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == harness.OperationRunning && rec.State.Terminal == nil && rec.State.ActiveEffect == nil, "operation after failed tool intent = %+v, want the running state retained without a fabricated terminal", rec.State)
	testassert.Equal(t, executes.Load(), int64(0))
	err = r.Close(ctx)
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "injected tool intent publication failure"), "Close error = %v, want the retained publication failure", err)
}

// TestNativeImplementationSettingsPreserveRawBytesCold proves a rich active
// extension section reaches durable implementation provenance through the
// canonical framing and survives a cold Harness read byte-for-byte:
// noncompact whitespace, HTML, escape spelling, exact large/exponent numbers
// and credential-reference names are all retained.
func TestNativeImplementationSettingsPreserveRawBytesCold(t *testing.T) {
	e := newOwnerEnv(t)
	section := `{"enabled":true, "api_key_env" : "OPENAI_API_KEY", "html":"<&>", "escaped":"\u003c", "large":9007199254740993, "exp":1e400}`
	config := strings.TrimSuffix(ownerConfigDocument(e.server.URL), "}") +
		`,"extensions":{"agents":{"enabled":true,"definitions":{"settings_agent":{"model":"prov/m","system_prompt":"simple","tools":["provsettings"]}}},"provsettings":` + section + `}}`
	writeServiceFile(t, e.configPath, config)
	e.fixtureStore(storage.NewMemory())
	var executes atomic.Int64
	native := testToolNative("provsettings", extension.LifetimeHarness, registrationTool{executes: &executes}, testDescribeTool("provsettings"))
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		return toolCallTurnEvents("call-settings", "provsettings", `{}`)
	})
	ctx := context.Background()
	r, err := e.open(ctx, native)
	testassert.NoError(t, err)
	session, err := r.createSession(ctx, e.dataDir, "settings_agent")
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-settings", "run")
	rec := awaitOperation(t, r, session.Identity.SessionID, "op-settings", harness.OperationSuccess)
	find := func(capture harness.ExecutionCapture) []byte {
		for _, implementation := range capture.Implementations {
			if implementation.ExtensionID == "provsettings" {
				return implementation.Settings
			}
		}
		return nil
	}
	testassert.Require(t, string(find(rec.Admission.Execution)) == section, "admission implementation settings = %q, want the exact section bytes", find(rec.Admission.Execution))
	testassert.NoError(t, r.Close(ctx))
	r2, err := e.open(ctx, native)
	testassert.NoError(t, err)
	var cold harness.OperationRecord
	err = r2.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		var readErr error
		cold, readErr = h.ReadOperation(ctx, session.Identity.SessionID, "op-settings")
		return readErr
	})
	testassert.NoError(t, err)
	testassert.Require(t, string(find(cold.Admission.Execution)) == section, "cold implementation settings = %q, want the exact section bytes", find(cold.Admission.Execution))
	testassert.NoError(t, r2.Close(ctx))
}

type normalizeIdentity struct {
	requestID  string
	toolCallID string
	effectID   string
}

type normalizeIdentityState struct {
	mu       sync.Mutex
	native   []normalizeIdentity
	provider []normalizeIdentity
}

type normalizeIdentityTool struct {
	state *normalizeIdentityState
}

func (t normalizeIdentityTool) Normalize(tc extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	t.state.mu.Lock()
	t.state.native = append(t.state.native, normalizeIdentity{requestID: tc.Calls.RequestID, toolCallID: tc.Calls.ToolCallID, effectID: tc.Calls.EffectID})
	t.state.mu.Unlock()
	if _, err := tc.Calls.Call(context.Background(), "dep", "call", json.RawMessage(`{}`)); err != nil {
		return nil, err
	}
	return call.Arguments, nil
}
func (t normalizeIdentityTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionCommandRun, Target: "test"}}}
}
func (t normalizeIdentityTool) Execute(_ context.Context, tc extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: tc.Calls.ToolCallID, Status: model.ResultSuccess, Content: "ok"}}
}

// TestNativeNormalizeUsesCompleteIdentityCarrier proves the native adapter
// consumes the Harness callback identity for both the original and repaired
// normalization, reaching the native tool and its declared pure service with
// the exact producing/invocation request, tool call, and empty pure effect ID.
func TestNativeNormalizeUsesCompleteIdentityCarrier(t *testing.T) {
	e := newOwnerEnv(t)
	config := strings.TrimSuffix(ownerConfigDocument(e.server.URL), "}") +
		`,"extensions":{"agents":{"enabled":true,"definitions":{"identity_agent":{"model":"prov/m","system_prompt":"simple","tools":["identity_tool"],"capabilities":["repair_identity"]}}},"identityext":{"enabled":true},"identprovider":{"enabled":true}}}`
	writeServiceFile(t, e.configPath, config)
	e.fixtureStore(storage.NewMemory())
	state := &normalizeIdentityState{}
	providerCapability := extension.ServiceCapability("ident.provider", "test.ident.provider", extension.PureMethod("call", `{}`, `{}`))
	provider := extension.Native{
		Declaration: extension.Declaration{
			ID:           "identprovider",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "identprovider-c", Lifetime: extension.LifetimeHarness, Provides: []string{"ident.provider"}}},
			Capabilities: []extension.Capability{providerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"ident.provider": {Service: map[string]extension.Handler{"call": func(_ context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
				state.mu.Lock()
				state.provider = append(state.provider, normalizeIdentity{requestID: call.RequestID, toolCallID: call.ToolCallID, effectID: call.EffectID})
				state.mu.Unlock()
				return extension.Result{Value: json.RawMessage(`{}`)}, nil
			}}}}}, nil
		},
	}
	binding := extension.Binding{Name: "dep", Provider: "identprovider/identprovider-c", CapabilityID: "ident.provider", Definition: providerCapability.Definition()}
	consumer := extension.Native{
		Declaration: extension.Declaration{
			ID:           "identityext",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components:   []extension.Component{{ID: "identityext-c", Lifetime: extension.LifetimeHarness, Provides: []string{"identity_tool", "repair_identity"}, Requires: []extension.Binding{binding}}},
			Capabilities: []extension.Capability{extension.ToolCapability("identity_tool"), extension.HookCapability("repair_identity", extension.SingleHook(extension.BoundaryBeforeTool, extension.HookModePure))},
		},
		Describe: map[string]extension.ToolDescribe{"identity_tool": testDescribeTool("identity_tool")},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				"identity_tool": {Tool: normalizeIdentityTool{state: state}},
				"repair_identity": {BeforeTool: func(context.Context, extension.CallContext, model.ToolCall) (json.RawMessage, error) {
					return json.RawMessage(`{"repaired":true}`), nil
				}},
			}}, nil
		},
	}
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		return toolCallTurnEvents("call-ident", "identity_tool", `{}`)
	})
	ctx := context.Background()
	r, err := e.open(ctx, provider, consumer)
	testassert.NoError(t, err)
	session, err := r.createSession(ctx, e.dataDir, "identity_agent")
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-identity", "run")
	awaitOperation(t, r, session.Identity.SessionID, "op-identity", harness.OperationSuccess)
	state.mu.Lock()
	nativeIDs := append([]normalizeIdentity(nil), state.native...)
	providerIDs := append([]normalizeIdentity(nil), state.provider...)
	state.mu.Unlock()
	testassert.Require(t, len(nativeIDs) == 2 && len(providerIDs) == 2, "normalize identities = native %v provider %v, want original and repaired calls", nativeIDs, providerIDs)
	for i := range nativeIDs {
		testassert.Require(t, nativeIDs[i].requestID != "" && nativeIDs[i].toolCallID == "call-ident" && nativeIDs[i].effectID == "", "native normalize identity %d = %+v, want request, call-ident and no pure effect", i, nativeIDs[i])
		testassert.Require(t, providerIDs[i] == nativeIDs[i], "provider identity %d = %+v, want the native caller identity %+v", i, providerIDs[i], nativeIDs[i])
	}
	testassert.Require(t, nativeIDs[0].requestID != nativeIDs[1].requestID, "original/repaired request identities = %+v, want the producing request and the invocation request", nativeIDs)
	testassert.NoError(t, r.Close(ctx))
}

// TestNativeEmptyProvidesComponentOpensAndDisposesOnce proves a lifecycle-only
// component may provide no capabilities while still declaring and receiving
// a normal dependency; it opens with an empty capability map and disposes
// once. A wrong extra returned binding is still rejected at the same
// instance-validation boundary.
func TestNativeEmptyProvidesComponentOpensAndDisposesOnce(t *testing.T) {
	providerCapability := extension.ServiceCapability("emptyprovider", "test.emptyprovider", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{}`, "", ""))
	provider := testNative("emptyprovider", extension.LifetimeHarness, []extension.Capability{providerCapability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"emptyprovider": {Service: map[string]extension.Handler{"call": testServiceHandler()}}}}, nil
	})
	binding := extension.Binding{Name: "dep", Provider: "emptyprovider/emptyprovider#component", CapabilityID: "emptyprovider", Definition: providerCapability.Definition()}
	var opens, closes atomic.Int64
	empty := testNative("emptycomponent", extension.LifetimeHarness, nil, []extension.Binding{binding}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		opens.Add(1)
		return extension.Instance{Capabilities: map[string]extension.Implementation{}, Close: func(context.Context, extension.CallContext) error {
			closes.Add(1)
			return nil
		}}, nil
	})
	empty.Declaration.Components[0].Provides = nil
	c := mustComposition(t, provider, empty)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	testassert.Equal(t, opens.Load(), int64(1))
	testassert.NoError(t, rt.close())
	testassert.Equal(t, closes.Load(), int64(1))

	wrongCloses := atomic.Int64{}
	wrong := testNative("wrongempty", extension.LifetimeHarness, nil, []extension.Binding{binding}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"ghost": {Service: map[string]extension.Handler{"call": testServiceHandler()}}}, Close: func(context.Context, extension.CallContext) error {
			wrongCloses.Add(1)
			return nil
		}}, nil
	})
	wrong.Declaration.Components[0].Provides = nil
	wrongComposition := mustComposition(t, provider, wrong)
	if _, err := wrongComposition.openScope(context.Background(), runtimeScopeInfo(), nil); !errors.Is(err, ErrComposition) {
		t.Fatalf("wrong empty-component instance = %v, want the composition rejection", err)
	}
	testassert.Equal(t, wrongCloses.Load(), int64(1))
}
