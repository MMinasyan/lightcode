// Package fixture is the one native extension definition shared by the
// in-process host and the executable worker fixture. Both transports serve
// this exact code, so host tests compare their semantic results instead of two
// separate implementations.
package fixture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

const (
	// EventLogEnv names the optional append-only event log the fixture writes
	// for host tests. Entries carry identities and digests, never secret
	// values.
	EventLogEnv = "FIXTURE_EVENT_LOG"
	// MetadataPaddingEnv optionally pads the primary tool's advertised
	// parameters so the large-metadata transport path can be exercised.
	MetadataPaddingEnv = "FIXTURE_METADATA_PADDING"

	componentHarness    = "fixture.component"
	componentWorkspace  = "fixture.workspace"
	componentOperation  = "fixture.operation"
	componentInvocation = "fixture.invocation"

	toolCapability           = "fixture.tool"
	workspaceToolCapability  = "fixture.tool.workspace"
	operationToolCapability  = "fixture.tool.operation"
	invocationToolCapability = "fixture.tool.invocation"
	hooksCapability          = "fixture.hooks"
	strategyCapability       = "fixture.strategy"
	serviceCapability        = "fixture.service"
	cleanupCapability        = "fixture.cleanup"
)

var eventMu sync.Mutex

// fixtureVersion is the fixture declaration's release version. Tests may
// stamp a distinct version with -ldflags to observe loaded-versus-selected
// package lifecycles.
var fixtureVersion = "1.0.0"

func record(format string, arguments ...any) {
	path := os.Getenv(EventLogEnv)
	if path == "" {
		return
	}
	line := fmt.Sprintf(format, arguments...)
	eventMu.Lock()
	defer eventMu.Unlock()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G703 -- the event-log path is supplied by the test that launched this fixture.
	if err != nil {
		return
	}
	_, _ = file.WriteString(line + "\n")
	_ = file.Close()
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// LogEnvironment records the process environment variable names, never their
// values. The worker fixture calls it at process start so host tests can
// observe the spawn projection directly from the real process.
func LogEnvironment() {
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && name != "" {
			record("env:%s", name)
		}
	}
}

// Declaration is the fixture's fixed extension declaration.
func Declaration() extension.Declaration {
	prompt, _ := extension.HostCapability(extension.ContractPrompt)
	credentials, _ := extension.HostCapability(extension.ContractCredentials)
	data, _ := extension.HostCapability(extension.ContractData)
	childLaunch, _ := extension.HostCapability(extension.ContractChildLaunch)
	return extension.Declaration{
		ID:           "fixture",
		Version:      fixtureVersion,
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"token":{"type":"string"}},"additionalProperties":false}`),
		Secrets:      []extension.SecretRef{{Field: "/token", Lifetime: extension.SecretOperation}},
		Components: []extension.Component{
			{
				ID:       componentHarness,
				Lifetime: extension.LifetimeHarness,
				Provides: []string{toolCapability, hooksCapability, strategyCapability, serviceCapability, cleanupCapability},
				Requires: []extension.Binding{
					{Name: "prompt", Provider: extension.HostProvider, CapabilityID: extension.ContractPrompt, Definition: prompt},
					{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: credentials},
					{Name: "data", Provider: extension.HostProvider, CapabilityID: extension.ContractData, Definition: data},
					{Name: "child_launch", Provider: extension.HostProvider, CapabilityID: extension.ContractChildLaunch, Definition: childLaunch},
				},
			},
			{ID: componentWorkspace, Lifetime: extension.LifetimeWorkspace, Provides: []string{workspaceToolCapability}},
			{ID: componentOperation, Lifetime: extension.LifetimeOperation, Provides: []string{operationToolCapability}},
			{ID: componentInvocation, Lifetime: extension.LifetimeInvocation, Provides: []string{invocationToolCapability}},
		},
		Capabilities: []extension.Capability{
			extension.ToolCapability(toolCapability),
			extension.ToolCapability(workspaceToolCapability),
			extension.ToolCapability(operationToolCapability),
			extension.ToolCapability(invocationToolCapability),
			extension.HookCapability(hooksCapability,
				extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure),
				extension.SingleHook(extension.BoundaryRequest, extension.HookModeEffectful),
				extension.SingleHook(extension.BoundaryBeforeTool, extension.HookModePure),
				extension.SingleHook(extension.BoundaryAfterTool, extension.HookModePure),
			),
			extension.StrategyCapability(strategyCapability, true),
			extension.ServiceCapability(serviceCapability, "fixture.service/v1",
				extension.ServiceMethod("assemble", extension.ModeServiceIO,
					`{"type":"object","required":["identity","instructions","mode"],"properties":{"identity":{"type":"string"},"instructions":{"type":"string"},"mode":{"type":"string","enum":["none","simple","full"]}},"additionalProperties":false}`,
					`{"type":"string"}`, "", ""),
				extension.ServiceMethod("echo", extension.ModeServiceIO,
					`{"type":"object"}`, `{"type":"object"}`, "", ""),
				extension.ServiceMethod("retain", extension.ModeServiceIO,
					`{"type":"object","additionalProperties":false}`, `{"type":"null"}`, "", ""),
				extension.ServiceMethod("use_retained", extension.ModeServiceIO,
					`{"type":"object","additionalProperties":false}`, `{"type":"null"}`, "", ""),
			),
			extension.CleanupCapability(cleanupCapability),
		},
	}
}

// Native is the fixture implementation both transports serve.
func Native() extension.Native {
	return extension.Native{
		Declaration: Declaration(),
		Describe: map[string]extension.ToolDescribe{
			toolCapability:           describeTool(toolCapability, "fixture tool"),
			workspaceToolCapability:  describeTool(workspaceToolCapability, "fixture workspace tool"),
			operationToolCapability:  describeTool(operationToolCapability, "fixture operation tool"),
			invocationToolCapability: describeTool(invocationToolCapability, "fixture invocation tool"),
		},
		Open: open,
	}
}

func describeTool(id, description string) extension.ToolDescribe {
	return func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
		record("describe:%s", id)
		parameters := json.RawMessage(`{"type":"object"}`)
		if id == toolCapability {
			if padding, err := strconv.Atoi(os.Getenv(MetadataPaddingEnv)); err == nil && padding > 0 {
				parameters = json.RawMessage(`{"type":"object","description":"` + strings.Repeat("m", padding) + `"}`)
			}
		}
		return extension.ToolDescription{
			Definition: model.ToolDefinition{Name: id, Description: description, Parameters: parameters},
			Available:  true,
		}, nil
	}
}

func open(_ context.Context, info extension.InstanceContext) (extension.Instance, error) {
	record("open:%s:%s", info.Lifetime, info.ComponentID)
	instance := extension.Instance{Close: func(context.Context, extension.CallContext) error {
		record("close:%s:%s", info.Lifetime, info.ComponentID)
		return nil
	}}
	switch info.ComponentID {
	case componentHarness:
		service := &serviceState{}
		instance.Capabilities = map[string]extension.Implementation{
			toolCapability:     {Tool: fixtureTool{capability: toolCapability}},
			hooksCapability:    {Preparation: prepareHook, Request: requestHook, BeforeTool: beforeToolHook, AfterTool: afterToolHook},
			strategyCapability: {Strategy: fixtureStrategy{}},
			serviceCapability:  {Service: service.handlers()},
			cleanupCapability:  {Service: map[string]extension.Handler{"cleanup": cleanupHandler}},
		}
	case componentWorkspace:
		instance.Capabilities = map[string]extension.Implementation{
			workspaceToolCapability: {Tool: fixtureTool{capability: workspaceToolCapability}},
		}
	case componentOperation:
		instance.Capabilities = map[string]extension.Implementation{
			operationToolCapability: {Tool: fixtureTool{capability: operationToolCapability}},
		}
	case componentInvocation:
		instance.Capabilities = map[string]extension.Implementation{
			invocationToolCapability: {Tool: fixtureTool{capability: invocationToolCapability}},
		}
	default:
		return extension.Instance{}, fmt.Errorf("fixture: unknown component %q", info.ComponentID)
	}
	return instance, nil
}

type serviceState struct {
	mu       sync.Mutex
	retained func(context.Context, string, string, json.RawMessage) (extension.Result, error)
}

func (s *serviceState) handlers() map[string]extension.Handler {
	return map[string]extension.Handler{
		"assemble":     assembleHandler,
		"echo":         s.echoHandler,
		"retain":       s.retainHandler,
		"use_retained": s.useRetainedHandler,
	}
}

func assembleHandler(ctx context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
	record("service:assemble")
	return call.Call(ctx, "prompt", "assemble", json.RawMessage(`{"identity":"fixture","instructions":"","mode":"none"}`))
}

func (s *serviceState) echoHandler(_ context.Context, _ extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
	record("service:echo:%s", digest(arguments))
	return extension.Result{Value: append(json.RawMessage(nil), arguments...)}, nil
}

func (s *serviceState) retainHandler(_ context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
	s.mu.Lock()
	s.retained = call.Call
	s.mu.Unlock()
	record("service:retain")
	return extension.Result{Value: json.RawMessage("null")}, nil
}

func (s *serviceState) useRetainedHandler(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
	s.mu.Lock()
	retained := s.retained
	s.mu.Unlock()
	if retained == nil {
		return extension.Result{}, errors.New("fixture: no retained call")
	}
	record("service:use_retained")
	return retained(ctx, "prompt", "assemble", json.RawMessage(`{"identity":"fixture","instructions":"","mode":"none"}`))
}

func cleanupHandler(_ context.Context, call extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(arguments, &request); err != nil {
		return extension.Result{}, err
	}
	record("cleanup:%s;call-session=%s", request.SessionID, strconv.Quote(call.SessionID))
	return extension.Result{Value: json.RawMessage("null")}, nil
}

type fixtureTool struct {
	capability string
}

func (t fixtureTool) Normalize(toolContext extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	_, request, _ := unwrapArguments(call.Arguments)
	if request.Mode == "normalize_io" {
		if _, err := toolContext.Calls.Call(context.Background(), "data", "cache_namespace", json.RawMessage(`{}`)); err != nil {
			record("tool:normalize:io-refused:%s", t.capability)
			return nil, errors.New("fixture: service I/O refused")
		}
		record("tool:normalize:io-granted:%s", t.capability)
	}
	record("tool:normalize:%s:%s", t.capability, digest(call.Arguments))
	return append(json.RawMessage(nil), call.Arguments...), nil
}

func (t fixtureTool) Prepare(ctx context.Context, toolContext extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	record("tool:prepare:%s:%s", t.capability, digest(call.Arguments))
	_, request, _ := unwrapArguments(call.Arguments)
	if request.Mode == "prepare_io" {
		if _, err := toolContext.Calls.Call(ctx, "data", "cache_namespace", json.RawMessage(`{}`)); err != nil {
			record("tool:prepare:io-refused:%s", t.capability)
			return extension.ToolPlan{Immediate: &harness.ToolOutcome{
				Result:   model.ToolResult{CallID: toolContext.Calls.ToolCallID, Status: model.ResultError, Content: "fixture: service I/O refused"},
				Evidence: harness.ExecutionNotStarted,
			}}
		}
		record("tool:prepare:io-granted:%s", t.capability)
	}
	target := filepath.Join(toolContext.Workspace, "fixture.txt")
	if request.Mode == "invalid_result" {
		target = filepath.Join(toolContext.Workspace, "other.txt")
	}
	plan := extension.ToolPlan{
		Permissions:        []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: target}},
		CanonicalWorkspace: toolContext.Workspace,
		Data:               append([]byte(nil), call.Arguments...),
	}
	switch request.Mode {
	case "invalid_plan":
		plan.Permissions = nil
	case "child":
		plan.Permissions = append(plan.Permissions, harness.PermissionRequest{
			Permission: harness.PermissionChildLaunch,
			Target:     (harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: "child_agent"}).PermissionTarget(),
		})
	}
	return plan
}

type toolRequest struct {
	Mode string `json:"mode"`
	Hold string `json:"hold"`
}

func (t fixtureTool) Execute(ctx context.Context, toolContext extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	record("tool:execute:%s:%s", t.capability, digest(plan.Data))
	original, request, err := unwrapArguments(plan.Data)
	if err != nil {
		return toolError(toolContext, err)
	}
	switch request.Mode {
	case "corpus":
		return t.corpus(toolContext, plan, original)
	case "credentials", "credentials_hold":
		return t.credentials(ctx, toolContext, request)
	case "data":
		return t.data(ctx, toolContext)
	case "child":
		return t.child(ctx, toolContext)
	case "invalid_result":
		return harness.ToolOutcome{
			Result:   model.ToolResult{CallID: "wrong-call", Status: model.ResultSuccess, Content: "invalid result"},
			Evidence: harness.ExecutionObserved,
		}
	case "cooperative":
		record("tool:cooperative")
		<-ctx.Done()
		return toolSuccess(toolContext, "cooperative")
	default:
		return toolSuccess(toolContext, "fixture result")
	}
}

func (t fixtureTool) corpus(toolContext extension.ToolContext, plan extension.ToolPlan, original json.RawMessage) harness.ToolOutcome {
	content := digest(plan.Data) + ":" + digest(original)
	record("tool:corpus:%s", content)
	return toolSuccess(toolContext, content)
}

// unwrapArguments returns the model-visible arguments a before-tool hook
// wrapped and the request mode parsed from them.
func unwrapArguments(raw json.RawMessage) (json.RawMessage, toolRequest, error) {
	var wrapper struct {
		Hooked string `json:"hooked"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper.Hooked != "" {
		original := json.RawMessage(wrapper.Hooked)
		var request toolRequest
		if err := json.Unmarshal(original, &request); err != nil {
			return original, toolRequest{}, err
		}
		return original, request, nil
	}
	var request toolRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return raw, toolRequest{}, err
	}
	return raw, request, nil
}

func (t fixtureTool) credentials(ctx context.Context, toolContext extension.ToolContext, request toolRequest) harness.ToolOutcome {
	if request.Mode == "credentials_hold" {
		record("credential:entered:%s", t.capability)
		if err := waitForFile(ctx, request.Hold); err != nil {
			return toolError(toolContext, err)
		}
	}
	result, err := toolContext.Calls.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/token"}`))
	if err != nil {
		return toolError(toolContext, err)
	}
	var value struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(result.Value, &value); err != nil {
		return toolError(toolContext, err)
	}
	content := "credential:" + digest([]byte(value.Value))
	record("credential:result:%s", content)
	return toolSuccess(toolContext, content)
}

func (t fixtureTool) data(ctx context.Context, toolContext extension.ToolContext) harness.ToolOutcome {
	session, err := toolContext.Calls.Call(ctx, "data", "session_namespace", json.RawMessage(`{}`))
	if err != nil {
		return toolError(toolContext, err)
	}
	cache, err := toolContext.Calls.Call(ctx, "data", "cache_namespace", json.RawMessage(`{}`))
	if err != nil {
		return toolError(toolContext, err)
	}
	claim, err := toolContext.Calls.Call(ctx, "data", "claim_external_data", json.RawMessage(`{}`))
	if err != nil {
		return toolError(toolContext, err)
	}
	sessionDir, err := namespaceDirectory(session)
	if err != nil {
		return toolError(toolContext, err)
	}
	cacheDir, err := namespaceDirectory(cache)
	if err != nil {
		return toolError(toolContext, err)
	}
	if string(claim.Value) != "null" {
		return toolError(toolContext, fmt.Errorf("fixture: unexpected claim value %s", claim.Value))
	}
	record("data:session:%s", sessionDir)
	record("data:cache:%s", cacheDir)
	return toolSuccess(toolContext, "session="+sessionDir+";cache="+cacheDir+";claim=claimed")
}

func (t fixtureTool) child(ctx context.Context, toolContext extension.ToolContext) harness.ToolOutcome {
	arguments, err := json.Marshal(map[string]any{
		"definition":     harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: "child_agent"},
		"prompt":         "child task",
		"operation_id":   "child-op",
		"max_concurrent": 1,
		"output_limit":   1024,
	})
	if err != nil {
		return toolError(toolContext, err)
	}
	result, err := toolContext.Calls.Call(ctx, "child_launch", "launch", arguments)
	if err != nil {
		return toolError(toolContext, err)
	}
	var launched struct {
		ChildSessionID string `json:"child_session_id"`
	}
	if err := json.Unmarshal(result.Value, &launched); err != nil {
		return toolError(toolContext, err)
	}
	record("child:%s", launched.ChildSessionID)
	return toolSuccess(toolContext, "child="+launched.ChildSessionID)
}

func namespaceDirectory(result extension.Result) (string, error) {
	var value struct {
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(result.Value, &value); err != nil {
		return "", err
	}
	if value.Directory == "" {
		return "", errors.New("fixture: empty namespace directory")
	}
	return value.Directory, nil
}

func waitForFile(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("fixture: no hold path")
	}
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func toolSuccess(toolContext extension.ToolContext, content string) harness.ToolOutcome {
	return harness.ToolOutcome{
		Result:   model.ToolResult{CallID: toolContext.Calls.ToolCallID, Status: model.ResultSuccess, Content: content},
		Evidence: harness.ExecutionObserved,
	}
}

func toolError(toolContext extension.ToolContext, err error) harness.ToolOutcome {
	return harness.ToolOutcome{
		Result:   model.ToolResult{CallID: toolContext.Calls.ToolCallID, Status: model.ResultError, Content: err.Error()},
		Evidence: harness.ExecutionObserved,
	}
}

func prepareHook(_ context.Context, _ extension.CallContext, input extension.PreparationInput) (extension.PreparationConsequence, error) {
	names := make([]string, 0, len(input.Tools))
	for _, tool := range input.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	payload := input.Model.Provider + "/" + input.Model.Model + ":" + strings.Join(names, ",")
	prompt := "fixture-preparation:" + digest([]byte(payload))
	record("hook:prepare:%s->%s", payload, prompt)
	return extension.PreparationConsequence{Prompt: &prompt}, nil
}

func requestHook(_ context.Context, _ extension.CallContext, input harness.RequestHookInput) (harness.RequestConsequence, error) {
	names := make([]string, 0, len(input.Tools))
	visible := make([]string, 0, len(input.Tools))
	for index, tool := range input.Tools {
		names = append(names, tool.Name)
		if index%2 == 0 {
			visible = append(visible, tool.Name)
		}
	}
	record("hook:request:in=%s->out=%s", strings.Join(names, ","), strings.Join(visible, ","))
	return harness.RequestConsequence{ToolNames: visible}, nil
}

func beforeToolHook(_ context.Context, _ extension.CallContext, call model.ToolCall) (json.RawMessage, error) {
	wrapped := json.RawMessage(`{"hooked":` + strconv.Quote(string(call.Arguments)) + `}`)
	record("hook:before_tool:in=%s->out=%s", strconv.Quote(string(call.Arguments)), strconv.Quote(string(wrapped)))
	return wrapped, nil
}

func afterToolHook(_ context.Context, _ extension.CallContext, input harness.ToolOutputInput) (string, error) {
	shaped := "fixture-shaped:" + digest([]byte(input.Content)) + ":" + input.Content
	record("hook:after_tool:in=%s->out=%s", strconv.Quote(input.Content), strconv.Quote(shaped))
	return shaped, nil
}

type fixtureStrategy struct{}

func (fixtureStrategy) Plan(input harness.PlanInput) (harness.PlanOutput, error) {
	messages, err := json.Marshal(input.Messages)
	if err != nil {
		return harness.PlanOutput{}, err
	}
	names := make([]string, 0, len(input.Tools))
	for _, tool := range input.Tools {
		names = append(names, tool.Name)
	}
	payload := fmt.Sprintf("instructions=%s;context_window=%d;tools=%s;messages=%s", input.Instructions, input.ContextWindow, strings.Join(names, ","), digest(messages))
	state := json.RawMessage(`{"payload":"` + digest([]byte(payload)) + `","context_window":` + strconv.Itoa(input.ContextWindow) + `}`)
	content := "fixture-plan:" + digest([]byte(payload))
	record("strategy:plan:%s->state=%s", payload, state)
	return harness.PlanOutput{Kind: harness.PlanModel, Model: &harness.ModelPlan{
		Request: model.Request{
			Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: content}}}},
			Tools:    append([]model.ToolDefinition(nil), input.Tools...),
		},
		State: state,
	}}, nil
}

func (fixtureStrategy) Consume(input harness.ConsumeInput) (harness.PlanOutput, error) {
	record("strategy:consume:state=%s;output=%s;detail=%s", input.State, input.Output.Status, input.Output.Detail)
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}
