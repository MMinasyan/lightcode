package agents

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

func openInstance(t *testing.T, options Options, section string) extension.Instance {
	t.Helper()
	instance, err := Native(options).Open(context.Background(), extension.InstanceContext{Section: json.RawMessage(section)})
	testassert.NoError(t, err)
	return instance
}

func callService(t *testing.T, instance extension.Instance, capability, method, arguments string) extension.Result {
	t.Helper()
	handler := instance.Capabilities[capability].Service[method]
	testassert.Require(t, handler != nil, "capability %q has no %q handler", capability, method)
	result, err := handler(context.Background(), extension.CallContext{}, json.RawMessage(arguments))
	testassert.NoError(t, err)
	testassert.Require(t, result.Failure == nil, "%s/%s returned failure %+v", capability, method, result.Failure)
	return result
}

// TestNativeResolverListsAndResolvesOwnedInputs proves the schema-bound
// resolver's list/resolve pair over the supplied definition format: list order
// and descriptions, complete resolved inputs including the trimmed write dir,
// a wholly absent model, and the unknown-ID failure.
func TestNativeResolverListsAndResolvesOwnedInputs(t *testing.T) {
	options := Options{
		CapabilityIDs: []string{"cap-a", "cap-b"},
		ToolIDs:       []string{"plan_tool", "read_file"},
	}
	instance := openInstance(t, options, `{"enabled":true,"definitions":{
		"primary":{"model":"prov/m"},
		"plan":{"model":"prov/m","system_prompt":"simple","prompt":"plan prompt","tools":["plan_tool","plan_tool","read_file"],"capabilities":["cap-b","cap-a"],"readonly":true,"write_dir":"  /tmp/plan  ","description":"planner","subagent":true},
		"modelless":{"system_prompt":"none"}
	}}`)

	listed := callService(t, instance, DefinitionsCapabilityID, "list", `{}`)
	summaries, err := extension.DecodeAgentDefinitionList(listed.Value)
	testassert.NoError(t, err)
	var names []string
	for _, summary := range summaries {
		names = append(names, summary.ID)
	}
	testassert.Require(t, reflect.DeepEqual(names, []string{"primary", "secondary", "explore", "review", "compact", "modelless", "plan"}), "list order = %q", names)
	for _, summary := range summaries {
		if summary.ID == "plan" {
			testassert.Equal(t, summary.Description, "planner")
		}
	}

	resolved := callService(t, instance, DefinitionsCapabilityID, "resolve", `{"id":"plan"}`)
	resolution, err := extension.DecodeAgentDefinitionResolution(resolved.Value)
	testassert.NoError(t, err)
	testassert.Require(t, resolution.Description == "planner" && resolution.Subagent, "plan metadata = %+v", resolution)
	inputs := resolution.Inputs
	testassert.Require(t, inputs.Model == (model.ModelRef{Provider: "prov", Model: "m"}) && inputs.Instructions == "plan prompt" && inputs.AssemblyMode == "simple", "plan inputs = %+v", inputs)
	testassert.Require(t, reflect.DeepEqual(inputs.Tools, []string{"plan_tool", "plan_tool", "read_file"}) && reflect.DeepEqual(inputs.Capabilities, []string{"cap-b", "cap-a"}), "plan selections = %v/%v", inputs.Tools, inputs.Capabilities)
	testassert.Require(t, inputs.Constraints.Readonly && inputs.Constraints.WriteDir == "/tmp/plan", "plan constraints = %+v, want the trimmed author write dir", inputs.Constraints)
	testassert.Require(t, inputs.Strategy.ID == "" && len(inputs.Strategy.Settings) == 0, "plan strategy = %+v, want the absent selection", inputs.Strategy)

	withoutModel := openInstance(t, options, `{"enabled":true,"definitions":{"modelless":{"system_prompt":"none"}}}`)
	modelless := callService(t, withoutModel, DefinitionsCapabilityID, "resolve", `{"id":"modelless"}`)
	absent, err := extension.DecodeAgentDefinitionResolution(modelless.Value)
	testassert.NoError(t, err)
	testassert.Require(t, absent.Inputs.Model.IsZero(), "modelless inputs = %+v, want the wholly absent model", absent.Inputs)

	handler := instance.Capabilities[DefinitionsCapabilityID].Service["resolve"]
	_, err = handler(context.Background(), extension.CallContext{}, json.RawMessage(`{"id":"ghost"}`))
	testassert.Require(t, err != nil && strings.Contains(err.Error(), `unknown agent type "ghost"`), "unknown resolve = %v", err)
}

// TestNativeSetModelTransformsLatestSection proves the supplied pure edit
// consumes the callback's latest owning section, preserves every other
// member and definition field, and retains the known-definition/model
// invalid-input failures.
func TestNativeSetModelTransformsLatestSection(t *testing.T) {
	instance := openInstance(t, Options{}, `{"enabled":true}`)
	handler := instance.Capabilities[ConfigurationCapabilityID].Service["set_model"]
	section := json.RawMessage(`{"enabled":true,"task":{"max_concurrent":7},"unknown":9007199254740993,"definitions":{"worker":{"system_prompt":"simple","description":"kept","tools":[]},"other":{"model":"p/old"}}}`)
	result, err := handler(context.Background(), extension.CallContext{Section: section}, json.RawMessage(`{"definition_id":"worker","model":"prov/m"}`))
	testassert.NoError(t, err)
	testassert.Require(t, result.Failure == nil, "set failure = %+v", result.Failure)
	var transformed map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(result.Value, &transformed))
	testassert.Equal(t, strings.TrimSpace(string(transformed["task"])), `{"max_concurrent":7}`)
	testassert.Equal(t, strings.TrimSpace(string(transformed["unknown"])), `9007199254740993`)
	var definitions map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(transformed["definitions"], &definitions))
	testassert.Equal(t, strings.TrimSpace(string(definitions["worker"])), `{"description":"kept","model":"prov/m","system_prompt":"simple","tools":[]}`)
	testassert.Equal(t, strings.TrimSpace(string(definitions["other"])), `{"model":"p/old"}`)

	cleared, err := handler(context.Background(), extension.CallContext{Section: result.Value}, json.RawMessage(`{"definition_id":"worker","model":""}`))
	testassert.NoError(t, err)
	testassert.NoError(t, json.Unmarshal(cleared.Value, &transformed))
	testassert.NoError(t, json.Unmarshal(transformed["definitions"], &definitions))
	testassert.Equal(t, strings.TrimSpace(string(definitions["worker"])), `{"description":"kept","system_prompt":"simple","tools":[]}`)

	for _, row := range []struct {
		name      string
		arguments string
	}{
		{"unknown definition", `{"definition_id":"ghost","model":"prov/m"}`},
		{"malformed model", `{"definition_id":"worker","model":"noslash"}`},
	} {
		section := json.RawMessage(`{"enabled":true,"definitions":{"worker":{"system_prompt":"simple"}}}`)
		_, err := handler(context.Background(), extension.CallContext{Section: section}, json.RawMessage(row.arguments))
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "%s = %v, want harness.ErrInvalid", row.name, err)
	}
	{
		// A builtin overlay with the wrong JSON type stays a builtin in the
		// resolver's view, and the authoring write refuses the non-object entry.
		_, err := handler(context.Background(), extension.CallContext{Section: json.RawMessage(`{"enabled":true,"definitions":{"primary":5}}`)}, json.RawMessage(`{"definition_id":"primary","model":"prov/m"}`))
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "non-object entry = %v, want harness.ErrInvalid", err)
	}
	{
		_, err := handler(context.Background(), extension.CallContext{Section: json.RawMessage(`5`)}, json.RawMessage(`{"definition_id":"worker","model":"prov/m"}`))
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "non-object section = %v, want harness.ErrInvalid", err)
	}
}

// TestNativeTaskAdvertisesAndLaunchesOwnDefinitionsOnly proves the task tool
// filters the roster by its own extension and resolver: a foreign provider's
// same-named entry is never advertised or selectable, and the selected full
// reference crosses the child-launch call while the permission target stays
// the definition ID.
func TestNativeTaskAdvertisesAndLaunchesOwnDefinitionsOnly(t *testing.T) {
	native := Native(Options{})
	tool := openInstance(t, Options{}, `{"enabled":true}`).Capabilities[TaskCapabilityID].Tool
	roster := []protocol.Agent{
		{Name: "twin", Subagent: true, Description: "foreign twin", Definition: protocol.DefinitionRef{ExtensionId: "guest", CapabilityId: "guest.definitions", DefinitionId: "twin"}},
		{Name: "twin", Subagent: true, Description: "own twin", Definition: protocol.DefinitionRef{ExtensionId: ExtensionID, CapabilityId: DefinitionsCapabilityID, DefinitionId: "twin"}},
		{Name: "private", Subagent: false, Definition: protocol.DefinitionRef{ExtensionId: ExtensionID, CapabilityId: DefinitionsCapabilityID, DefinitionId: "private"}},
	}
	description, err := native.Describe[TaskCapabilityID](context.Background(), extension.DescribeInput{Session: harness.SessionIdentity{SessionID: "s"}, AgentTypes: roster})
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(description.Definition.Description, "- twin: own twin") && !strings.Contains(description.Definition.Description, "foreign twin"), "task description = %q, want only the own resolver's entry", description.Definition.Description)

	call, err := model.NewToolCall(model.ToolCall{ID: "call-1", Name: TaskCapabilityID, Arguments: json.RawMessage(`{"prompt":"do it","subagent_type":"twin"}`)})
	testassert.NoError(t, err)
	var requested json.RawMessage
	tc := extension.ToolContext{AgentTypes: roster, Calls: extension.CallContext{Call: func(_ context.Context, binding, method string, arguments json.RawMessage) (extension.Result, error) {
		testassert.Equal(t, binding, "child_launch")
		testassert.Equal(t, method, "launch")
		requested = append(json.RawMessage(nil), arguments...)
		return extension.Result{Value: json.RawMessage(`{"child_session_id":"child-1"}`)}, nil
	}}}
	plan := tool.Prepare(context.Background(), tc, call)
	testassert.Require(t, plan.Immediate == nil && len(plan.Permissions) == 1, "prepare = %+v", plan)
	testassert.Require(t, plan.Permissions[0] == (harness.PermissionRequest{Permission: harness.PermissionChildLaunch, Target: "twin"}), "permission pair = %+v, want the definition ID target", plan.Permissions[0])
	outcome := tool.Execute(context.Background(), tc, plan)
	testassert.Equal(t, outcome.Result.Status, model.ResultSuccess)
	var launched struct {
		Definition harness.DefinitionRef `json:"definition"`
	}
	testassert.NoError(t, json.Unmarshal(requested, &launched))
	testassert.Require(t, launched.Definition == (harness.DefinitionRef{ExtensionID: ExtensionID, CapabilityID: DefinitionsCapabilityID, DefinitionID: "twin"}), "child launch definition = %+v, want the own source-qualified reference", launched.Definition)

	foreignOnly := []protocol.Agent{{Name: "foreign", Subagent: true, Definition: protocol.DefinitionRef{ExtensionId: "guest", CapabilityId: "guest.definitions", DefinitionId: "foreign"}}}
	foreignCall, err := model.NewToolCall(model.ToolCall{ID: "call-2", Name: TaskCapabilityID, Arguments: json.RawMessage(`{"prompt":"do it","subagent_type":"foreign"}`)})
	testassert.NoError(t, err)
	denied := tool.Prepare(context.Background(), extension.ToolContext{AgentTypes: foreignOnly}, foreignCall)
	testassert.Require(t, denied.Immediate != nil && denied.Immediate.Result.Status == model.ResultError, "foreign-only prepare = %+v, want an immediate refusal", denied)
	testassert.Require(t, string(denied.Immediate.Result.Content) == `unknown subagent type "foreign": unknown agent type "foreign"`, "foreign-only refusal = %q", denied.Immediate.Result.Content)
}

// TestNativeDeclarationExposesTheFixedResolverAndEdit proves the compiled
// declaration carries the fixed resolver contract, the pure configuration
// service and edit, and the task tool.
func TestNativeDeclarationExposesTheFixedResolverAndEdit(t *testing.T) {
	compiled, err := extension.Compile([]extension.Native{Native(Options{})})
	testassert.NoError(t, err)
	resolver, ok := compiled.Capability(DefinitionsCapabilityID)
	testassert.Require(t, ok && resolver.ContractID == extension.ContractAgentDefinitions && resolver.ContractVersion == extension.ContractVersion && resolver.Kind == extension.KindService, "resolver = %+v", resolver)
	configuration, ok := compiled.Capability(ConfigurationCapabilityID)
	testassert.Require(t, ok && configuration.Kind == extension.KindService && configuration.Methods["set_model"].Mode == extension.ModePure, "configuration = %+v", configuration)
	declaration, ok := compiled.Declaration(ExtensionID)
	testassert.Require(t, ok && len(declaration.Declaration.Edits) == 1 && declaration.Declaration.Edits[0].ID == ModelEditID && declaration.Declaration.Edits[0].Method == (extension.MethodKey{CapabilityID: ConfigurationCapabilityID, Method: "set_model"}), "edits = %+v", declaration.Declaration.Edits)
}

// TestNativeOwnsConstructorOptionsAtAcceptance proves the constructor copies
// the supplied universe/default slices exactly once, when Native accepts
// them: caller mutation before the initial Open and before a second direct
// Open cannot change accepted validation or inherited defaults. This is two
// direct Opens, not a Runtime replacing reload.
func TestNativeOwnsConstructorOptionsAtAcceptance(t *testing.T) {
	capabilities := []string{"cap-a"}
	tools := []string{"read_file"}
	defaults := []string{"cap-a"}
	native := Native(Options{CapabilityIDs: capabilities, ToolIDs: tools, DefaultCapabilityIDs: defaults})
	capabilities[0] = "mutated-cap"
	tools[0] = "ghost_tool"
	defaults[0] = "mutated-default"

	section := json.RawMessage(`{"enabled":true,"definitions":{"worker":{"tools":["read_file"],"capabilities":["cap-a"]}}}`)
	first, err := native.Open(context.Background(), extension.InstanceContext{Section: section})
	testassert.NoError(t, err)
	worker := callService(t, first, DefinitionsCapabilityID, "resolve", `{"id":"worker"}`)
	workerInputs, err := extension.DecodeAgentDefinitionResolution(worker.Value)
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(workerInputs.Inputs.Tools, []string{"read_file"}) && reflect.DeepEqual(workerInputs.Inputs.Capabilities, []string{"cap-a"}), "worker inputs = %+v, want the accepted universe", workerInputs.Inputs)
	primary := callService(t, first, DefinitionsCapabilityID, "resolve", `{"id":"primary"}`)
	primaryInputs, err := extension.DecodeAgentDefinitionResolution(primary.Value)
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(primaryInputs.Inputs.Capabilities, []string{"cap-a"}), "primary defaults = %v, want the accepted constructor selection", primaryInputs.Inputs.Capabilities)

	// A second direct Open reopens the same native after further caller
	// mutation; the accepted constructor data is still unchanged.
	capabilities[0] = "other-cap"
	tools[0] = "other_tool"
	defaults[0] = "other-default"
	second, err := native.Open(context.Background(), extension.InstanceContext{Section: section})
	testassert.NoError(t, err)
	reloaded := callService(t, second, DefinitionsCapabilityID, "resolve", `{"id":"worker"}`)
	reloadedInputs, err := extension.DecodeAgentDefinitionResolution(reloaded.Value)
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(reloadedInputs.Inputs.Tools, []string{"read_file"}) && reflect.DeepEqual(reloadedInputs.Inputs.Capabilities, []string{"cap-a"}), "reloaded worker inputs = %+v, want the accepted universe", reloadedInputs.Inputs)
}
