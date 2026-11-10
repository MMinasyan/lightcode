package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
	"github.com/MMinasyan/lightcode/protocol"
)

func testSession() harness.SessionIdentity {
	return harness.SessionIdentity{
		SessionID: "session-1",
		Workspace: "/workspace",
		CreatedAt: time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
	}
}

func testAgent() protocol.Agent {
	return protocol.Agent{
		Name:         "agent-1",
		Description:  "an agent",
		Model:        "provider/model",
		Tools:        []string{"tool-a"},
		Capabilities: []string{"cap-a"},
		Readonly:     true,
		Subagent:     false,
		WriteDir:     "/workspace/write",
		Definition:   protocol.DefinitionRef{ExtensionId: "ext", CapabilityId: "cap", DefinitionId: "def"},
	}
}

func testCallContext() extension.CallContext {
	return extension.CallContext{
		ExtensionID:  "ext",
		ComponentID:  "component",
		InstanceID:   "instance",
		Lifetime:     extension.LifetimeOperation,
		Scope:        extension.ScopeIdentity{Kind: extension.LifetimeOperation, Revision: "rev-1", Workspace: "/workspace", SessionID: "session-1", OperationID: "op-1"},
		Revision:     "rev-1",
		Section:      json.RawMessage("{\n  \"setting\": true\n}"),
		SessionID:    "session-1",
		OperationID:  "op-1",
		InvocationID: "inv-1",
		Purpose:      "conversation",
		RequestID:    "request-1",
		ToolCallID:   "call-1",
		EffectID:     "effect-1",
		Caller:       extension.CallerIdentity{ExtensionID: "caller", ComponentID: "caller-component", InstanceID: "caller-instance"},
	}
}

func testToolContext() extension.ToolContext {
	return extension.ToolContext{
		Session:       testSession(),
		Model:         model.ModelRef{Provider: "provider", Model: "model"},
		Revision:      "rev-1",
		Workspace:     "/workspace",
		AdmittedEntry: harness.EntryRef{SessionID: "session-1", EntryID: "entry-1"},
		Constraints:   harness.ToolConstraints{Readonly: true, WriteDir: "/workspace/write"},
		Config:        json.RawMessage("{\n  \"config\": [1, 2]\n}"),
		AgentTypes:    []protocol.Agent{testAgent()},
		Calls:         testCallContext(),
	}
}

func testToolCall() model.ToolCall {
	return model.ToolCall{
		ID:        "call-1",
		Name:      "echo",
		Arguments: json.RawMessage("not json\n{\"still\":\"bytes\"}"),
		Extra:     model.Extra{"extra": json.RawMessage("{\n  \"n\": 1\n}")},
	}
}

func testMessage() model.Message {
	return model.Message{
		Role: model.RoleAssistant,
		Content: []model.ContentPart{
			{Kind: model.PartText, Text: "hello"},
			{Kind: model.PartOpaque, OpaqueWireType: "custom", Extra: model.Extra{"raw": json.RawMessage("{\n  \"x\": 2\n}")}},
		},
		ToolCalls: []model.ToolCall{testToolCall()},
		Name:      "assistant-name",
		Extra:     model.Extra{"meta": json.RawMessage(`{"k":true}`)},
		Source:    model.ModelRef{Provider: "provider", Model: "model"},
	}
}

func testToolDefinition(name string) model.ToolDefinition {
	return model.ToolDefinition{Name: name, Description: name + " description", Parameters: json.RawMessage("{\n  \"type\": \"object\"\n}")}
}

func testToolOutcome() harness.ToolOutcome {
	return harness.ToolOutcome{
		Result:   model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"},
		Metadata: json.RawMessage("{\n  \"metadata\": [true, false]\n}"),
		Evidence: harness.ExecutionObserved,
	}
}

func testToolPlan() extension.ToolPlan {
	immediate := testToolOutcome()
	return extension.ToolPlan{
		Permissions:        []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "/workspace/file"}},
		CanonicalWorkspace: "/workspace",
		CanonicalWriteDir:  "/workspace/write",
		Immediate:          &immediate,
		Data:               []byte("line one\nline two\x00\xff"),
	}
}

func testPreparationInput() extension.PreparationInput {
	return extension.PreparationInput{
		Session:     testSession(),
		Model:       model.ModelRef{Provider: "provider", Model: "model"},
		Constraints: harness.ToolConstraints{Readonly: false, WriteDir: "/workspace/write"},
		Assembly: prompt.AssemblyInput{
			Identity:     "identity",
			Instructions: "instructions",
			Mode:         prompt.ModeFull,
			Rules: prompt.Rules{
				GlobalText:    "global",
				WorkspaceText: "workspace",
				Warnings:      []prompt.Warning{{Kind: "rules_not_found", Message: "missing"}},
			},
			Workspace:     "/workspace",
			SessionStart:  time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
			OS:            "linux",
			Shell:         "bash",
			OSDescription: "Linux",
		},
		Prompt:   "prompt",
		Catalog:  []model.ToolDefinition{testToolDefinition("catalog-tool")},
		Tools:    []model.ToolDefinition{testToolDefinition("visible-tool")},
		Settings: json.RawMessage("{\n  \"hook\": 1\n}"),
	}
}

func testRequestHookInput() harness.RequestHookInput {
	return harness.RequestHookInput{
		RequestID:         "request-1",
		CanonicalMessages: []model.Message{testMessage()},
		Messages:          []model.Message{testMessage()},
		CheckpointEntryID: "checkpoint-1",
		BoundaryEntryID:   "boundary-1",
		CapturedPrompt:    "captured",
		Prompt:            "prompt",
		Catalog:           []model.ToolDefinition{testToolDefinition("catalog-tool")},
		Tools:             []model.ToolDefinition{testToolDefinition("visible-tool")},
	}
}

func testPlanInput() harness.PlanInput {
	return harness.PlanInput{
		Trigger:           "automatic",
		Messages:          []model.Message{testMessage()},
		CheckpointEntryID: "checkpoint-1",
		BoundaryEntryID:   "boundary-1",
		Instructions:      "instructions",
		Tools:             []model.ToolDefinition{testToolDefinition("tool")},
		Contributions: &harness.RequestEvidence{
			CheckpointEntryID: "checkpoint-1",
			BoundaryEntryID:   "boundary-1",
			Prompt:            "prompt",
			Messages:          []model.Message{testMessage()},
			ToolNames:         []string{"tool"},
			HookIDs:           []harness.HookIdentity{{ExtensionID: "ext", HookID: "hook"}},
		},
		Measurement:   &harness.UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 3},
		ContextWindow: 1000,
		OutputReserve: 100,
		Settings:      json.RawMessage("{\n  \"strategy\": \"settings\"\n}"),
	}
}

func testModelOutput() model.Output {
	message := testMessage()
	return model.Output{
		Status:  model.OutputCompleted,
		Source:  model.ModelRef{Provider: "provider", Model: "model"},
		Message: &message,
		Usage:   &model.Usage{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 3},
		Detail:  "",
	}
}

func methodSchema(t *testing.T, capability extension.Capability, name string, field string) json.RawMessage {
	t.Helper()
	for _, method := range capability.Methods {
		if method.Name != name {
			continue
		}
		switch field {
		case "input":
			return method.Input
		case "success":
			return method.Success
		}
	}
	t.Fatalf("capability %q has no method %q field %q", capability.ID, name, field)
	return nil
}

func methodError(t *testing.T, capability extension.Capability, name string) json.RawMessage {
	t.Helper()
	for _, method := range capability.Methods {
		if method.Name == name {
			return method.Error
		}
	}
	t.Fatalf("capability %q has no method %q", capability.ID, name)
	return nil
}

func validateAgainstSchema(t *testing.T, schema json.RawMessage, value json.RawMessage) {
	t.Helper()
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		t.Fatalf("decoding schema: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("melta:///codec-schema", document); err != nil {
		t.Fatalf("adding schema: %v", err)
	}
	compiled, err := compiler.Compile("melta:///codec-schema")
	if err != nil {
		t.Fatalf("compiling schema: %v", err)
	}
	decoded, err := jsonschema.UnmarshalJSON(bytes.NewReader(value))
	if err != nil {
		t.Fatalf("decoding value: %v", err)
	}
	if err := compiled.Validate(decoded); err != nil {
		t.Fatalf("value does not validate against the method schema: %v\nvalue: %s", err, value)
	}
}

func assertNoNullMembers(t *testing.T, raw json.RawMessage) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("walking encoded value: %v (%s)", err, raw)
		}
		if token == nil {
			t.Fatalf("encoded value contains a JSON null: %s", raw)
		}
	}
}

func roundTrip[T any](t *testing.T, name string, schema json.RawMessage, value T, encode func(T) (json.RawMessage, error), decode func(json.RawMessage) (T, error)) json.RawMessage {
	t.Helper()
	encoded, err := encode(value)
	if err != nil {
		t.Fatalf("%s: encoding: %v", name, err)
	}
	assertNoNullMembers(t, encoded)
	if len(schema) != 0 {
		validateAgainstSchema(t, schema, encoded)
	}
	decodedValue, err := decode(encoded)
	if err != nil {
		t.Fatalf("%s: decoding: %v", name, err)
	}
	if !reflect.DeepEqual(decodedValue, value) {
		t.Fatalf("%s: round trip differs\n got: %#v\nwant: %#v", name, decodedValue, value)
	}
	return encoded
}

func TestCodecsRoundTripAndValidate(t *testing.T) {
	tool := extension.ToolCapability("tool-1")
	toolContext := testToolContext()
	toolCall := testToolCall()
	plan := testToolPlan()
	outcome := testToolOutcome()

	roundTrip(t, "describe input", methodSchema(t, tool, "describe", "input"), extension.DescribeInput{
		Session:     testSession(),
		Model:       model.ModelRef{Provider: "provider", Model: "model"},
		Constraints: harness.ToolConstraints{Readonly: true, WriteDir: "/workspace/write"},
		Revision:    "rev-1",
		Config:      json.RawMessage("{\n  \"describe\": 1\n}"),
		AgentTypes:  []protocol.Agent{testAgent()},
		Inputs: harness.AgentInputs{
			Model:        model.ModelRef{Provider: "provider", Model: "model"},
			Instructions: "instructions",
			AssemblyMode: "full",
			Tools:        []string{"tool-a"},
			Capabilities: []string{"cap-a"},
			Constraints:  harness.ToolConstraints{Readonly: true, WriteDir: "/workspace/write"},
			Strategy:     harness.StrategySelection{ID: "strategy", Settings: json.RawMessage("{\n  \"s\": 1\n}")},
		},
	}, worker.EncodeDescribeParams, worker.DecodeDescribeParams)

	roundTrip(t, "describe result", methodSchema(t, tool, "describe", "success"), extension.ToolDescription{
		Definition: testToolDefinition("tool-1"),
		Available:  true,
	}, worker.EncodeDescribeResult, worker.DecodeDescribeResult)

	type contextCall struct {
		Context extension.ToolContext
		Call    model.ToolCall
	}
	roundTrip(t, "normalize input", methodSchema(t, tool, "normalize", "input"), contextCall{Context: toolContext, Call: toolCall}, func(v contextCall) (json.RawMessage, error) {
		return worker.EncodeNormalizeParams(v.Context, v.Call)
	}, func(raw json.RawMessage) (contextCall, error) {
		context_, call, err := worker.DecodeNormalizeParams(raw)
		return contextCall{Context: context_, Call: call}, err
	})
	roundTrip(t, "normalize result", methodSchema(t, tool, "normalize", "success"), toolCall.Arguments, worker.EncodeArguments, worker.DecodeArguments)

	roundTrip(t, "prepare input", methodSchema(t, tool, "prepare", "input"), contextCall{Context: toolContext, Call: toolCall}, func(v contextCall) (json.RawMessage, error) {
		return worker.EncodePrepareParams(v.Context, v.Call)
	}, func(raw json.RawMessage) (contextCall, error) {
		context_, call, err := worker.DecodePrepareParams(raw)
		return contextCall{Context: context_, Call: call}, err
	})
	roundTrip(t, "prepare result", methodSchema(t, tool, "prepare", "success"), plan, worker.EncodePrepareResult, worker.DecodePrepareResult)

	type contextPlan struct {
		Context extension.ToolContext
		Plan    extension.ToolPlan
	}
	roundTrip(t, "execute input", methodSchema(t, tool, "execute", "input"), contextPlan{Context: toolContext, Plan: plan}, func(v contextPlan) (json.RawMessage, error) {
		return worker.EncodeExecuteParams(v.Context, v.Plan)
	}, func(raw json.RawMessage) (contextPlan, error) {
		context_, plan, err := worker.DecodeExecuteParams(raw)
		return contextPlan{Context: context_, Plan: plan}, err
	})
	roundTrip(t, "execute result", methodSchema(t, tool, "execute", "success"), outcome, worker.EncodeExecuteResult, worker.DecodeExecuteResult)

	prepareHook := extension.HookCapability("hook-prepare", extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure))
	roundTrip(t, "prepare hook input", methodSchema(t, prepareHook, "prepare", "input"), testPreparationInput(), worker.EncodePrepareHookParams, worker.DecodePrepareHookParams)
	roundTrip(t, "prepare hook result", methodSchema(t, prepareHook, "prepare", "success"), extension.PreparationConsequence{
		Prompt:       stringPointer("new prompt"),
		Descriptions: map[string]string{"tool-a": "description"},
		ToolNames:    []string{"tool-a"},
	}, worker.EncodePrepareHookResult, worker.DecodePrepareHookResult)

	requestHook := extension.HookCapability("hook-request", extension.SingleHook(extension.BoundaryRequest, extension.HookModePure))
	roundTrip(t, "request hook input", methodSchema(t, requestHook, "request", "input"), testRequestHookInput(), worker.EncodeRequestHookParams, worker.DecodeRequestHookParams)
	roundTrip(t, "request hook result", methodSchema(t, requestHook, "request", "success"), harness.RequestConsequence{
		Prompt:    stringPointer("replaced"),
		Messages:  []model.Message{testMessage()},
		ToolNames: []string{"tool-a"},
	}, worker.EncodeRequestHookResult, worker.DecodeRequestHookResult)

	beforeTool := extension.HookCapability("hook-before", extension.SingleHook(extension.BoundaryBeforeTool, extension.HookModePure))
	roundTrip(t, "before-tool hook input", methodSchema(t, beforeTool, "before_tool", "input"), toolCall, worker.EncodeBeforeToolHookParams, worker.DecodeBeforeToolHookParams)
	roundTrip(t, "before-tool hook result", methodSchema(t, beforeTool, "before_tool", "success"), toolCall.Arguments, worker.EncodeArguments, worker.DecodeArguments)

	afterTool := extension.HookCapability("hook-after", extension.SingleHook(extension.BoundaryAfterTool, extension.HookModePure))
	roundTrip(t, "after-tool hook input", methodSchema(t, afterTool, "after_tool", "input"), harness.ToolOutputInput{Outcome: outcome, Content: "visible"}, worker.EncodeAfterToolHookParams, worker.DecodeAfterToolHookParams)
	roundTrip(t, "after-tool hook result", methodSchema(t, afterTool, "after_tool", "success"), "shaped", worker.EncodeAfterToolHookResult, worker.DecodeAfterToolHookResult)

	strategy := extension.StrategyCapability("strategy-1", true)
	roundTrip(t, "plan input", methodSchema(t, strategy, "plan", "input"), testPlanInput(), worker.EncodePlanParams, worker.DecodePlanParams)
	roundTrip(t, "plan result", methodSchema(t, strategy, "plan", "success"), harness.PlanOutput{
		Kind: harness.PlanModel,
		Model: &harness.ModelPlan{
			Request: model.Request{
				Messages: []model.Message{testMessage()},
				Tools:    []model.ToolDefinition{testToolDefinition("tool")},
			},
			State: json.RawMessage("{\n  \"state\": true\n}"),
		},
	}, worker.EncodePlanResult, worker.DecodePlanResult)
	roundTrip(t, "plan result none", methodSchema(t, strategy, "plan", "success"), harness.PlanOutput{Kind: harness.PlanNone}, worker.EncodePlanResult, worker.DecodePlanResult)
	retain := 2
	roundTrip(t, "plan result checkpoint", methodSchema(t, strategy, "plan", "success"), harness.PlanOutput{
		Kind: harness.PlanCheckpoint,
		Checkpoint: &harness.CheckpointPlan{
			BoundaryEntryID: "boundary-1",
			Items: []harness.CheckpointItem{
				{Retain: &retain},
				{Context: messagePointer(testMessage())},
			},
		},
	}, worker.EncodePlanResult, worker.DecodePlanResult)
	roundTrip(t, "consume input", methodSchema(t, strategy, "consume", "input"), harness.ConsumeInput{
		Input:  testPlanInput(),
		State:  json.RawMessage("{\n  \"local\": 1\n}"),
		Output: testModelOutput(),
	}, worker.EncodeConsumeParams, worker.DecodeConsumeParams)
	roundTrip(t, "consume result", methodSchema(t, strategy, "consume", "success"), harness.PlanOutput{Kind: harness.PlanNone}, worker.EncodePlanResult, worker.DecodePlanResult)

	service := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceMethod("call", extension.ModeServiceIO,
			`{"type":"object","required":["input"],"properties":{"input":{"type":"string"}},"additionalProperties":false}`,
			`{"type":"object","required":["output"],"properties":{"output":{"type":"string"}},"additionalProperties":false}`,
			`{"type":"object","required":["reason"],"properties":{"reason":{"type":"string"}},"additionalProperties":false}`,
			`{"type":"object","required":["partial"],"properties":{"partial":{"type":"string"}},"additionalProperties":false}`))
	serviceMethod := service.Methods[0]
	successResult := extension.Result{Value: json.RawMessage("{\n  \"output\": \"value\"\n}")}
	encoded := roundTrip(t, "service success result", nil, successResult, worker.EncodeServiceResult, worker.DecodeServiceResult)
	var successWire struct {
		Value worker.JSONBytes `json:"value"`
	}
	if err := json.Unmarshal(encoded, &successWire); err != nil {
		t.Fatalf("decoding service result envelope: %v", err)
	}
	validateAgainstSchema(t, serviceMethod.Success, json.RawMessage(successWire.Value))

	failureResult := extension.Result{Failure: &extension.Failure{
		Code:    "failed",
		Message: "failure message",
		Data:    json.RawMessage("{\n  \"reason\": \"because\"\n}"),
		Partial: json.RawMessage("{\n  \"partial\": \"kept\"\n}"),
	}}
	encoded = roundTrip(t, "service failure result", nil, failureResult, worker.EncodeServiceResult, worker.DecodeServiceResult)
	var failureWire struct {
		Failure struct {
			Data    worker.JSONBytes `json:"data"`
			Partial worker.JSONBytes `json:"partial"`
		} `json:"failure"`
	}
	if err := json.Unmarshal(encoded, &failureWire); err != nil {
		t.Fatalf("decoding service failure envelope: %v", err)
	}
	validateAgainstSchema(t, serviceMethod.Error, json.RawMessage(failureWire.Failure.Data))
	validateAgainstSchema(t, serviceMethod.Partial, json.RawMessage(failureWire.Failure.Partial))
}

// TestWireShapesUseSchemaForm proves the codec renders Go values in the fixed
// schemas' shape: model identities are objects, extras are ordered base64
// entry arrays, timestamps are RFC 3339 strings, and opaque members are base64.
func TestWireShapesUseSchemaForm(t *testing.T) {
	input := extension.DescribeInput{
		Session:     testSession(),
		Model:       model.ModelRef{Provider: "provider", Model: "model"},
		Constraints: harness.ToolConstraints{Readonly: true, WriteDir: "/workspace/write"},
		Revision:    "rev-1",
		Config:      json.RawMessage("{\n  \"x\": 1\n}"),
		AgentTypes:  []protocol.Agent{testAgent()},
		Inputs: harness.AgentInputs{
			Model:        model.ModelRef{Provider: "provider", Model: "model"},
			Instructions: "instructions",
			AssemblyMode: "full",
			Constraints:  harness.ToolConstraints{WriteDir: "/workspace/write"},
			Strategy:     harness.StrategySelection{ID: "strategy"},
		},
	}
	encoded, err := worker.EncodeDescribeParams(input)
	if err != nil {
		t.Fatalf("encoding describe input: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &members); err != nil {
		t.Fatalf("decoding describe input: %v", err)
	}
	var modelRef map[string]string
	if err := json.Unmarshal(members["model"], &modelRef); err != nil {
		t.Fatalf("model is not a provider/model object: %v (%s)", err, members["model"])
	}
	if modelRef["provider"] != "provider" || modelRef["model"] != "model" {
		t.Fatalf("model object = %v", modelRef)
	}
	var config string
	if err := json.Unmarshal(members["config"], &config); err != nil {
		t.Fatalf("config is not a base64 string: %v (%s)", err, members["config"])
	}
	var session map[string]json.RawMessage
	if err := json.Unmarshal(members["session"], &session); err != nil {
		t.Fatalf("decoding session: %v", err)
	}
	var createdAt string
	if err := json.Unmarshal(session["created_at"], &createdAt); err != nil {
		t.Fatalf("created_at is not a string: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		t.Fatalf("created_at %q is not RFC 3339: %v", createdAt, err)
	}
	var inputs map[string]json.RawMessage
	if err := json.Unmarshal(members["inputs"], &inputs); err != nil {
		t.Fatalf("decoding inputs: %v", err)
	}
	var inputsModel map[string]string
	if err := json.Unmarshal(inputs["model"], &inputsModel); err != nil {
		t.Fatalf("inputs model is not a provider/model object: %v", err)
	}

	call := model.ToolCall{
		ID:        "call-1",
		Name:      "echo",
		Arguments: json.RawMessage("line\nbytes"),
		Extra:     model.Extra{"b": json.RawMessage(`{"n":2}`), "a": json.RawMessage(`"<&>"`)},
	}
	encoded, err = worker.EncodeNormalizeParams(testToolContext(), call)
	if err != nil {
		t.Fatalf("encoding normalize input: %v", err)
	}
	if err := json.Unmarshal(encoded, &members); err != nil {
		t.Fatalf("decoding normalize input: %v", err)
	}
	var callMembers map[string]json.RawMessage
	if err := json.Unmarshal(members["call"], &callMembers); err != nil {
		t.Fatalf("decoding call: %v", err)
	}
	var extras []struct {
		Key   worker.JSONBytes `json:"key"`
		Value worker.JSONBytes `json:"value"`
	}
	if err := json.Unmarshal(callMembers["extra"], &extras); err != nil {
		t.Fatalf("extra is not a base64 entry array: %v (%s)", err, callMembers["extra"])
	}
	if len(extras) != 2 || string(extras[0].Key) != "a" || string(extras[0].Value) != `"<&>"` || string(extras[1].Key) != "b" {
		t.Fatalf("extra entries = %+v", extras)
	}
	var arguments worker.JSONBytes
	if err := json.Unmarshal(callMembers["arguments"], &arguments); err != nil {
		t.Fatalf("arguments is not base64: %v", err)
	}
	if string(arguments) != "line\nbytes" {
		t.Fatalf("arguments bytes = %q", arguments)
	}
}

func TestEncodeRejectsRequiredModelRefWithoutSchemaForm(t *testing.T) {
	_, err := worker.EncodeDescribeParams(extension.DescribeInput{
		Session: testSession(),
		Inputs: harness.AgentInputs{
			Model:        model.ModelRef{Provider: "provider", Model: "model"},
			Instructions: "instructions",
			AssemblyMode: "full",
			Constraints:  harness.ToolConstraints{},
			Strategy:     harness.StrategySelection{ID: "strategy"},
		},
	})
	if err == nil {
		t.Fatal("empty describe model encoded without an error")
	}
	toolContext := testToolContext()
	toolContext.Model = model.ModelRef{}
	if _, err := worker.EncodeNormalizeParams(toolContext, testToolCall()); err == nil {
		t.Fatal("empty tool-context model encoded without an error")
	}
}

func TestOptionalRequirednessFollowsSchemas(t *testing.T) {
	decodeMembers := func(raw json.RawMessage, key string) map[string]json.RawMessage {
		t.Helper()
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatalf("decoding %s: %v", key, err)
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(object[key], &nested); err != nil {
			t.Fatalf("decoding nested %s: %v", key, err)
		}
		return nested
	}

	encoded, err := worker.EncodeDescribeParams(extension.DescribeInput{
		Session: testSession(),
		Model:   model.ModelRef{Provider: "provider", Model: "model"},
		Inputs:  harness.AgentInputs{},
	})
	if err != nil {
		t.Fatalf("zero optional agentInputs.model rejected: %v", err)
	}
	inputs := decodeMembers(encoded, "inputs")
	if _, present := inputs["model"]; present {
		t.Fatalf("zero optional agentInputs.model was encoded: %s", inputs["model"])
	}
	_, err = worker.EncodeDescribeParams(extension.DescribeInput{
		Session: testSession(),
		Model:   model.ModelRef{Provider: "provider", Model: "model"},
		Inputs:  harness.AgentInputs{Model: model.ModelRef{Provider: "provider"}},
	})
	if err == nil {
		t.Fatal("partial optional agentInputs.model encoded without an error")
	}

	hookInput := testRequestHookInput()
	hookInput.CanonicalMessages = []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{}}}
	encoded, err = worker.EncodeRequestHookParams(hookInput)
	if err != nil {
		t.Fatalf("zero optional message.source rejected: %v", err)
	}
	var hookMembers map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &hookMembers); err != nil {
		t.Fatalf("decoding request hook input: %v", err)
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(hookMembers["canonical_messages"], &messages); err != nil {
		t.Fatalf("decoding canonical messages: %v", err)
	}
	if _, present := messages[0]["source"]; present {
		t.Fatalf("zero optional message.source was encoded: %s", messages[0]["source"])
	}
	hookInput.CanonicalMessages[0].Source = model.ModelRef{Provider: "provider"}
	if _, err := worker.EncodeRequestHookParams(hookInput); err == nil {
		t.Fatal("partial optional message.source encoded without an error")
	}

	encoded, err = worker.EncodeDescribeParams(extension.DescribeInput{
		Session:    testSession(),
		Model:      model.ModelRef{Provider: "provider", Model: "model"},
		AgentTypes: []protocol.Agent{{Name: "agent"}},
		Inputs:     harness.AgentInputs{},
	})
	if err != nil {
		t.Fatalf("zero optional agent.definition rejected: %v", err)
	}
	var describeMembers map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &describeMembers); err != nil {
		t.Fatalf("decoding describe input: %v", err)
	}
	var agents []map[string]json.RawMessage
	if err := json.Unmarshal(describeMembers["agent_types"], &agents); err != nil {
		t.Fatalf("decoding agents: %v", err)
	}
	if _, present := agents[0]["definition"]; present {
		t.Fatalf("zero optional agent.definition was encoded: %s", agents[0]["definition"])
	}
	_, err = worker.EncodeDescribeParams(extension.DescribeInput{
		Session:    testSession(),
		Model:      model.ModelRef{Provider: "provider", Model: "model"},
		AgentTypes: []protocol.Agent{{Definition: protocol.DefinitionRef{ExtensionId: "ext"}}},
		Inputs:     harness.AgentInputs{},
	})
	if err == nil {
		t.Fatal("partial optional agent.definition encoded without an error")
	}
}

// TestPresencePreservedExactly proves the members consumers distinguish keep
// absent and present-empty forms apart.
func TestPresencePreservedExactly(t *testing.T) {
	members := func(raw json.RawMessage) map[string]json.RawMessage {
		t.Helper()
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatalf("decoding object: %v", err)
		}
		return object
	}
	assertOptionalString := func(name string, value *string, encode func(extension.PreparationConsequence) (json.RawMessage, error)) {
		t.Helper()
		raw, err := encode(extension.PreparationConsequence{Prompt: value})
		if err != nil {
			t.Fatalf("%s: encoding: %v", name, err)
		}
		_, present := members(raw)["prompt"]
		if (value != nil) != present {
			t.Fatalf("%s: prompt presence = %v, want %v (%s)", name, present, value != nil, raw)
		}
		decoded, err := worker.DecodePrepareHookResult(raw)
		if err != nil {
			t.Fatalf("%s: decoding: %v", name, err)
		}
		if (decoded.Prompt != nil) != (value != nil) || (value != nil && *decoded.Prompt != *value) {
			t.Fatalf("%s: decoded prompt = %v", name, decoded.Prompt)
		}
	}
	emptyPrompt := ""
	assertOptionalString("absent preparation prompt", nil, worker.EncodePrepareHookResult)
	assertOptionalString("present empty preparation prompt", &emptyPrompt, worker.EncodePrepareHookResult)

	assertToolNames := func(name string, names []string, encode func(extension.PreparationConsequence) (json.RawMessage, error), decode func(json.RawMessage) (extension.PreparationConsequence, error)) {
		t.Helper()
		raw, err := encode(extension.PreparationConsequence{ToolNames: names})
		if err != nil {
			t.Fatalf("%s: encoding: %v", name, err)
		}
		if _, present := members(raw)["tool_names"]; (names != nil) != present {
			t.Fatalf("%s: tool_names presence = %v, want %v (%s)", name, present, names != nil, raw)
		}
		decoded, err := decode(raw)
		if err != nil {
			t.Fatalf("%s: decoding: %v", name, err)
		}
		if (decoded.ToolNames != nil) != (names != nil) {
			t.Fatalf("%s: decoded tool_names = %#v", name, decoded.ToolNames)
		}
		if names != nil && len(decoded.ToolNames) != 0 {
			t.Fatalf("%s: decoded empty tool_names = %#v", name, decoded.ToolNames)
		}
	}
	assertToolNames("absent preparation tool names", nil, worker.EncodePrepareHookResult, worker.DecodePrepareHookResult)
	assertToolNames("empty preparation tool names", []string{}, worker.EncodePrepareHookResult, worker.DecodePrepareHookResult)

	requestEmpty := ""
	encodeRequest := func(prompt *string, names []string) json.RawMessage {
		t.Helper()
		raw, err := worker.EncodeRequestHookResult(harness.RequestConsequence{Prompt: prompt, ToolNames: names})
		if err != nil {
			t.Fatalf("encoding request consequence: %v", err)
		}
		return raw
	}
	if _, present := members(encodeRequest(nil, nil))["prompt"]; present {
		t.Fatal("absent request prompt was encoded")
	}
	if _, present := members(encodeRequest(&requestEmpty, nil))["prompt"]; !present {
		t.Fatal("present empty request prompt was dropped")
	}
	if _, present := members(encodeRequest(nil, nil))["tool_names"]; present {
		t.Fatal("absent request tool names were encoded")
	}
	if _, present := members(encodeRequest(nil, []string{}))["tool_names"]; !present {
		t.Fatal("present empty request tool names were dropped")
	}
	decodedAbsent, err := worker.DecodeRequestHookResult(encodeRequest(nil, nil))
	if err != nil {
		t.Fatalf("decoding absent request consequence: %v", err)
	}
	if decodedAbsent.ToolNames != nil || decodedAbsent.Prompt != nil {
		t.Fatalf("absent request members decoded as %#v", decodedAbsent)
	}
	decodedEmpty, err := worker.DecodeRequestHookResult(encodeRequest(&requestEmpty, []string{}))
	if err != nil {
		t.Fatalf("decoding empty request consequence: %v", err)
	}
	if decodedEmpty.ToolNames == nil || decodedEmpty.Prompt == nil || *decodedEmpty.Prompt != "" {
		t.Fatalf("empty request members decoded as %#v", decodedEmpty)
	}

	plan := extension.ToolPlan{Permissions: []harness.PermissionRequest{}}
	raw, err := worker.EncodePrepareResult(plan)
	if err != nil {
		t.Fatalf("encoding absent immediate: %v", err)
	}
	if _, present := members(raw)["immediate"]; present {
		t.Fatal("absent immediate was encoded")
	}
	immediate := harness.ToolOutcome{}
	plan.Immediate = &immediate
	raw, err = worker.EncodePrepareResult(plan)
	if err != nil {
		t.Fatalf("encoding present immediate: %v", err)
	}
	decodedPlan, err := worker.DecodePrepareResult(raw)
	if err != nil {
		t.Fatalf("decoding present immediate: %v", err)
	}
	if decodedPlan.Immediate == nil {
		t.Fatal("present immediate decoded as absent")
	}

	absentPlan := harness.PlanInput{Messages: []model.Message{}, Tools: []model.ToolDefinition{}}
	absentRaw, err := worker.EncodePlanParams(absentPlan)
	if err != nil {
		t.Fatalf("encoding absent plan members: %v", err)
	}
	if _, present := members(absentRaw)["contributions"]; present {
		t.Fatal("absent contributions were encoded")
	}
	if _, present := members(absentRaw)["measurement"]; present {
		t.Fatal("absent measurement was encoded")
	}
	zeroMeasurement := &harness.UsageCount{}
	absentPlan.Contributions = &harness.RequestEvidence{ToolNames: []string{}}
	absentPlan.Measurement = zeroMeasurement
	presentRaw, err := worker.EncodePlanParams(absentPlan)
	if err != nil {
		t.Fatalf("encoding present plan members: %v", err)
	}
	decodedPlanInput, err := worker.DecodePlanParams(presentRaw)
	if err != nil {
		t.Fatalf("decoding present plan members: %v", err)
	}
	if decodedPlanInput.Contributions == nil || decodedPlanInput.Measurement == nil {
		t.Fatalf("present plan members decoded as %#v", decodedPlanInput)
	}

	for _, output := range []harness.PlanOutput{
		{Kind: harness.PlanNone},
		{Kind: harness.PlanModel, Model: &harness.ModelPlan{Request: model.Request{Messages: []model.Message{}, Tools: []model.ToolDefinition{}}}},
		{Kind: harness.PlanCheckpoint, Checkpoint: &harness.CheckpointPlan{BoundaryEntryID: "boundary", Items: []harness.CheckpointItem{{Retain: intPointer(0)}, {Context: messagePointer(testMessage())}}}},
	} {
		encoded, err := worker.EncodePlanResult(output)
		if err != nil {
			t.Fatalf("encoding plan output %s: %v", output.Kind, err)
		}
		decoded, err := worker.DecodePlanResult(encoded)
		if err != nil {
			t.Fatalf("decoding plan output %s: %v", output.Kind, err)
		}
		if (decoded.Model != nil) != (output.Model != nil) || (decoded.Checkpoint != nil) != (output.Checkpoint != nil) {
			t.Fatalf("plan output %s variant presence changed: %#v", output.Kind, decoded)
		}
		if output.Checkpoint != nil {
			if decoded.Checkpoint.Items[0].Retain == nil || *decoded.Checkpoint.Items[0].Retain != 0 {
				t.Fatalf("retain 0 decoded as %#v", decoded.Checkpoint.Items[0].Retain)
			}
			if decoded.Checkpoint.Items[1].Context == nil {
				t.Fatal("checkpoint context decoded as absent")
			}
		}
	}

	zeroOutput := model.Output{Status: model.OutputCompleted, Source: model.ModelRef{Provider: "p", Model: "m"}, Detail: ""}
	message := testMessage()
	zeroUsage := model.Usage{}
	zeroOutput.Message = &message
	zeroOutput.Usage = &zeroUsage
	consume := harness.ConsumeInput{Input: harness.PlanInput{Messages: []model.Message{}, Tools: []model.ToolDefinition{}}, Output: zeroOutput}
	encoded, err := worker.EncodeConsumeParams(consume)
	if err != nil {
		t.Fatalf("encoding consume input: %v", err)
	}
	decodedConsume, err := worker.DecodeConsumeParams(encoded)
	if err != nil {
		t.Fatalf("decoding consume input: %v", err)
	}
	if decodedConsume.Output.Message == nil || decodedConsume.Output.Usage == nil {
		t.Fatalf("present output message/usage decoded as %#v", decodedConsume.Output)
	}
	absentConsume := harness.ConsumeInput{
		Input:  harness.PlanInput{Messages: []model.Message{}, Tools: []model.ToolDefinition{}},
		Output: model.Output{Status: model.OutputCompleted, Source: model.ModelRef{Provider: "p", Model: "m"}},
	}
	encoded, err = worker.EncodeConsumeParams(absentConsume)
	if err != nil {
		t.Fatalf("encoding absent consume output members: %v", err)
	}
	decodedConsume, err = worker.DecodeConsumeParams(encoded)
	if err != nil {
		t.Fatalf("decoding absent consume output members: %v", err)
	}
	if decodedConsume.Output.Message != nil || decodedConsume.Output.Usage != nil {
		t.Fatalf("absent output message/usage decoded as %#v", decodedConsume.Output)
	}
}

// TestResultEnvelopePresence proves result arms keep absent and present-empty
// bytes distinct while failure payloads follow zero-length-is-absent.
func TestResultEnvelopePresence(t *testing.T) {
	raw, err := worker.EncodeServiceResult(extension.Result{Value: json.RawMessage{}})
	if err != nil {
		t.Fatalf("encoding present empty value: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if got := string(members["value"]); got != `""` {
		t.Fatalf("present empty value wire = %s, want the empty base64 string", got)
	}
	decoded, err := worker.DecodeServiceResult(raw)
	if err != nil {
		t.Fatalf("decoding present empty value: %v", err)
	}
	if decoded.Value == nil || len(decoded.Value) != 0 {
		t.Fatalf("present empty value decoded as %#v", decoded.Value)
	}

	raw, err = worker.EncodeServiceResult(extension.Result{Failure: &extension.Failure{
		Code:    "invalid",
		Message: "bad",
		Data:    json.RawMessage{},
		Partial: json.RawMessage{},
	}})
	if err != nil {
		t.Fatalf("encoding zero-length failure payloads: %v", err)
	}
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decoding failure result: %v", err)
	}
	var failure map[string]json.RawMessage
	if err := json.Unmarshal(members["failure"], &failure); err != nil {
		t.Fatalf("decoding failure: %v", err)
	}
	if _, present := failure["data"]; present {
		t.Fatalf("zero-length failure data was encoded: %s", members["failure"])
	}
	if _, present := failure["partial"]; present {
		t.Fatalf("zero-length failure partial was encoded: %s", members["failure"])
	}
	decoded, err = worker.DecodeServiceResult(raw)
	if err != nil {
		t.Fatalf("decoding zero-length failure payloads: %v", err)
	}
	if decoded.Failure == nil || decoded.Failure.Data != nil || decoded.Failure.Partial != nil {
		t.Fatalf("zero-length failure payloads decoded as %#v", decoded.Failure)
	}

	raw, err = worker.EncodeServiceResult(extension.Result{Failure: &extension.Failure{Code: "invalid", Message: "bad"}})
	if err != nil {
		t.Fatalf("encoding absent failure payloads: %v", err)
	}
	members = nil
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decoding failure result: %v", err)
	}
	failure = nil
	if err := json.Unmarshal(members["failure"], &failure); err != nil {
		t.Fatalf("decoding failure: %v", err)
	}
	if _, present := failure["data"]; present {
		t.Fatalf("absent failure data was encoded: %s", members["failure"])
	}
	if _, present := failure["partial"]; present {
		t.Fatalf("absent failure partial was encoded: %s", members["failure"])
	}
}

func TestServiceResultRequiresExactlyOneArm(t *testing.T) {
	if _, err := worker.EncodeServiceResult(extension.Result{}); err == nil {
		t.Fatal("empty service result encoded without error")
	}
	if _, err := worker.EncodeServiceResult(extension.Result{Value: json.RawMessage("null"), Failure: &extension.Failure{Code: "x", Message: "y"}}); err == nil {
		t.Fatal("service result with both arms encoded without error")
	}
	for _, raw := range []string{`{}`, `{"value":"bnVsbA==","failure":{"code":"x","message":"y"}}`} {
		if _, err := worker.DecodeServiceResult(json.RawMessage(raw)); err == nil {
			t.Fatalf("service result %s decoded without error", raw)
		}
	}
	for _, raw := range []string{`{"failure":{"code":"","message":"y"}}`, `{"failure":{"code":"x","message":""}}`} {
		if _, err := worker.DecodeServiceResult(json.RawMessage(raw)); err == nil {
			t.Fatalf("service result %s decoded without error", raw)
		}
	}
}

func TestHookFailureFromError(t *testing.T) {
	if worker.HookFailureFromError(nil) != nil {
		t.Fatal("nil error produced a hook failure")
	}
	for _, row := range []struct {
		name     string
		err      error
		status   worker.HookStatus
		evidence harness.ExecutionEvidence
	}{
		{"plain", errors.New("boom"), worker.HookStatusError, harness.ExecutionObserved},
		{"canceled", context.Canceled, worker.HookStatusError, harness.ExecutionObserved},
		{"deadline", context.DeadlineExceeded, worker.HookStatusError, harness.ExecutionObserved},
		{"wrapped canceled", errors.Join(errors.New("wrapped"), context.Canceled), worker.HookStatusError, harness.ExecutionObserved},
	} {
		failure := worker.HookFailureFromError(row.err)
		if failure.Status != row.status || failure.Evidence != row.evidence {
			t.Fatalf("%s failure = %+v, want status %s evidence %s", row.name, failure, row.status, row.evidence)
		}
	}
}

func TestDeclarationWireFormPreservesSchemaBytes(t *testing.T) {
	declaration := extension.Declaration{
		ID:           "ext",
		Version:      "1.2.3",
		ConfigSchema: json.RawMessage("{\n  \"type\": \"object\",\n  \"properties\": {}\n}"),
		Secrets:      []extension.SecretRef{{Field: "/token", Lifetime: extension.SecretActivation}},
		Components: []extension.Component{{
			ID:       "component",
			Lifetime: extension.LifetimeHarness,
			Provides: []string{"tool-1", "service-1"},
			Requires: []extension.Binding{{
				Name:         "dep",
				Provider:     "other",
				CapabilityID: "other-service",
				Definition: extension.CapabilityDefinition{
					ContractID:      "other.service",
					ContractVersion: "1",
					Kind:            extension.KindService,
					Methods: []extension.Method{{
						Name:    "call",
						Mode:    extension.ModeServiceIO,
						Input:   json.RawMessage("{\n  \"type\": \"object\"\n}"),
						Success: json.RawMessage("{\n  \"type\": \"null\"\n}"),
					}},
				},
			}},
		}},
		Capabilities: []extension.Capability{
			extension.ToolCapability("tool-1"),
			extension.ServiceCapability("service-1", "service/v1", extension.ServiceIOMethod("call",
				`{"type":"object","additionalProperties":false}`,
				`{"type":"null"}`)),
		},
	}
	wire := worker.NewDeclaration(declaration)
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("encoding declaration: %v", err)
	}
	assertNoNullMembers(t, encoded)
	var decoded worker.Declaration
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decoding declaration: %v", err)
	}
	if got := decoded.Value(); !reflect.DeepEqual(got, declaration) {
		t.Fatalf("declaration round trip differs\n got: %#v\nwant: %#v", got, declaration)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &members); err != nil {
		t.Fatalf("decoding declaration object: %v", err)
	}
	var configSchema string
	if err := json.Unmarshal(members["config_schema"], &configSchema); err != nil {
		t.Fatalf("config schema is not one base64 string: %v", err)
	}
	var decodedConfig worker.JSONBytes
	if err := json.Unmarshal(members["config_schema"], &decodedConfig); err != nil {
		t.Fatalf("decoding config schema bytes: %v", err)
	}
	if !bytes.Equal(decodedConfig, declaration.ConfigSchema) {
		t.Fatalf("config schema bytes changed: %q", decodedConfig)
	}
}

func TestDeclarationWireFormNeverEmitsNull(t *testing.T) {
	declaration := extension.Declaration{
		ID:           "empty",
		Version:      "1",
		ConfigSchema: json.RawMessage(`{"type":"object"}`),
		Components:   []extension.Component{{ID: "component", Lifetime: extension.LifetimeHarness, Provides: []string{}}},
		Capabilities: []extension.Capability{},
	}
	encoded, err := json.Marshal(worker.NewDeclaration(declaration))
	if err != nil {
		t.Fatalf("encoding empty declaration: %v", err)
	}
	assertNoNullMembers(t, encoded)
}

func TestJSONBytesRejectsNonCanonicalBase64(t *testing.T) {
	for _, raw := range []string{`"not base64!"`, `"e30"`, `"e30==="`, `"e31="`, `null`} {
		var value worker.JSONBytes
		if err := json.Unmarshal([]byte(raw), &value); err == nil {
			t.Fatalf("JSONBytes accepted %s", raw)
		}
	}
	var value worker.JSONBytes
	if err := json.Unmarshal([]byte(`"`+`bGluZTEKbGluZTI=`+`"`), &value); err != nil {
		t.Fatalf("decoding canonical base64: %v", err)
	}
	if string(value) != "line1\nline2" {
		t.Fatalf("decoded bytes = %q", value)
	}
}

func TestErrorClassUnwrapping(t *testing.T) {
	for _, row := range []struct {
		class    worker.Class
		sentinel error
	}{
		{worker.ClassInvalid, harness.ErrInvalid},
		{worker.ClassNotFound, harness.ErrNotFound},
		{worker.ClassConflict, harness.ErrConflict},
		{worker.ClassIncompatible, harness.ErrIncompatible},
		{worker.ClassCorrupt, harness.ErrCorrupt},
		{worker.ClassStorage, harness.ErrStorage},
		{worker.ClassClosed, extension.ErrClosed},
	} {
		err := &worker.Error{Class: row.class, Message: "detail"}
		if !errors.Is(err, row.sentinel) {
			t.Fatalf("%s does not unwrap to %v", row.class, row.sentinel)
		}
	}
	for _, class := range []worker.Class{worker.ClassDenied, worker.ClassCancelled, worker.ClassUnavailable, worker.ClassConfiguration, worker.ClassInternal} {
		err := &worker.Error{Class: class, Message: "detail"}
		if errors.Is(err, harness.ErrInvalid) || errors.Is(err, harness.ErrStorage) || errors.Is(err, extension.ErrClosed) {
			t.Fatalf("%s unwraps to an unrelated sentinel", class)
		}
	}
}

func TestValidateResultEnvelopeShared(t *testing.T) {
	for _, row := range []struct {
		name   string
		result extension.Result
		ok     bool
	}{
		{"success", extension.Result{Value: json.RawMessage("null")}, true},
		{"present empty success", extension.Result{Value: json.RawMessage{}}, true},
		{"failure", extension.Result{Failure: &extension.Failure{Code: "x", Message: "y"}}, true},
		{"both", extension.Result{Value: json.RawMessage("null"), Failure: &extension.Failure{Code: "x", Message: "y"}}, false},
		{"neither", extension.Result{}, false},
		{"empty code", extension.Result{Failure: &extension.Failure{Code: "", Message: "y"}}, false},
		{"empty message", extension.Result{Failure: &extension.Failure{Code: "x", Message: ""}}, false},
	} {
		err := extension.ValidateResultEnvelope(row.result)
		if (err == nil) != row.ok {
			t.Fatalf("%s: ValidateResultEnvelope = %v, want ok=%v", row.name, err, row.ok)
		}
	}
}

func stringPointer(v string) *string { return &v }

func intPointer(v int) *int { return &v }

func messagePointer(v model.Message) *model.Message { return &v }

func TestDecodersRejectNullMembers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    json.RawMessage
		decode func(json.RawMessage) error
	}{
		{"request prompt", json.RawMessage(`{"prompt":null}`), func(raw json.RawMessage) error {
			_, err := worker.DecodeRequestHookResult(raw)
			return err
		}},
		{"prepare tool names", json.RawMessage(`{"tool_names":null}`), func(raw json.RawMessage) error {
			_, err := worker.DecodePrepareHookResult(raw)
			return err
		}},
		{"tool plan immediate", json.RawMessage(`{"permissions":[],"immediate":null}`), func(raw json.RawMessage) error {
			_, err := worker.DecodePrepareResult(raw)
			return err
		}},
		{"failure data", json.RawMessage(`{"failure":{"code":"x","message":"y","data":null}}`), func(raw json.RawMessage) error {
			_, err := worker.DecodeServiceResult(raw)
			return err
		}},
		{"failure partial", json.RawMessage(`{"failure":{"code":"x","message":"y","partial":null}}`), func(raw json.RawMessage) error {
			_, err := worker.DecodeServiceResult(raw)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.decode(tc.raw); err == nil {
				t.Fatalf("%s decoded without an error", tc.raw)
			}
		})
	}
}

func TestHostContextEncodersMatchCallbackValues(t *testing.T) {
	call := extension.CallContext{
		ExtensionID:  "extension",
		ComponentID:  "component",
		InstanceID:   "instance",
		Lifetime:     extension.LifetimeOperation,
		Scope:        extension.ScopeIdentity{Kind: extension.LifetimeOperation, Revision: "7", Workspace: "workspace", SessionID: "session", OperationID: "operation", InvocationID: "invocation", Purpose: "conversation"},
		Revision:     "7",
		Section:      json.RawMessage(`{"enabled":true}`),
		SessionID:    "session",
		OperationID:  "operation",
		InvocationID: "invocation",
		Purpose:      "conversation",
		RequestID:    "request",
		ToolCallID:   "call",
		EffectID:     "effect",
		Caller:       extension.CallerIdentity{ExtensionID: "caller", ComponentID: "caller-component", InstanceID: "caller-instance"},
	}
	wire := worker.NewContext("token", call)
	if wire.Token != "token" {
		t.Fatalf("token = %q", wire.Token)
	}
	if wire.ExtensionID != call.ExtensionID || wire.ComponentID != call.ComponentID || wire.InstanceID != call.InstanceID ||
		wire.Lifetime != call.Lifetime || !reflect.DeepEqual(wire.Scope, call.Scope) || wire.Revision != call.Revision ||
		!bytes.Equal(wire.Section, call.Section) || wire.SessionID != call.SessionID || wire.OperationID != call.OperationID ||
		wire.InvocationID != call.InvocationID || wire.Purpose != call.Purpose || wire.RequestID != call.RequestID ||
		wire.ToolCallID != call.ToolCallID || wire.EffectID != call.EffectID ||
		wire.Caller == nil || wire.Caller.ExtensionID != call.Caller.ExtensionID || wire.Caller.ComponentID != call.Caller.ComponentID || wire.Caller.InstanceID != call.Caller.InstanceID {
		t.Fatalf("wire context = %#v", wire)
	}

	instance := extension.InstanceContext{
		ExtensionID: "extension",
		ComponentID: "component",
		Version:     "1.0.0",
		InstanceID:  "instance",
		Lifetime:    extension.LifetimeHarness,
		Scope:       extension.ScopeIdentity{Kind: extension.LifetimeHarness, Revision: "3"},
		Revision:    "3",
		Section:     json.RawMessage(`{"enabled":true}`),
	}
	instanceWire := worker.NewInstanceContext("instance-token", instance)
	if instanceWire.Token != "instance-token" || instanceWire.InstanceID != "instance" || !bytes.Equal(instanceWire.Section, instance.Section) {
		t.Fatalf("instance context = %+v", instanceWire)
	}
}
