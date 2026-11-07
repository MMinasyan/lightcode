package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// TestDecodeSettingsRangesAndShapes proves the settings decoder the tool's
// preparation owns: absent/blank/null input and null members keep the
// defaults, unknown members are ignored, both retained ranges accept their
// exact boundaries and reject everything outside with the retained texts, and
// malformed or wrong-typed input carries the invalid-settings wrap.
func TestDecodeSettingsRangesAndShapes(t *testing.T) {
	keepDefaults := []json.RawMessage{
		nil,
		json.RawMessage(""),
		json.RawMessage("  \n"),
		json.RawMessage("null"),
		json.RawMessage(`{}`),
		json.RawMessage(`{"task":{"max_concurrent":null,"max_output_bytes":null}}`),
		json.RawMessage(`{"task":{"unknown_member":1,"max_output_bytes":null}}`),
	}
	for _, raw := range keepDefaults {
		s, err := decodeSettings(raw)
		testassert.Require(t, err == nil, "decodeSettings(%s): %v", raw, err)
		testassert.Check(t, s == (settings{MaxConcurrent: 4, MaxOutputBytes: 15360}), "decodeSettings(%s) = %+v, want the defaults 4 and 15360", raw, s)
	}
	inRange := []json.RawMessage{
		json.RawMessage(`{"task":{"max_concurrent":1,"max_output_bytes":1024}}`),
		json.RawMessage(`{"task":{"max_concurrent":20,"max_output_bytes":1048576}}`),
		json.RawMessage(`{"task":{"max_concurrent":7}}`),
		json.RawMessage(`{"task":{"max_output_bytes":2048}}`),
	}
	for _, raw := range inRange {
		_, err := decodeSettings(raw)
		testassert.Check(t, err == nil, "decodeSettings(%s) = %v, want nil", raw, err)
	}
	rejected := []struct {
		raw  json.RawMessage
		want string
	}{
		{json.RawMessage(`{"task":{"max_concurrent":0}}`), "tasks: max_concurrent must be between 1 and 20"},
		{json.RawMessage(`{"task":{"max_concurrent":21}}`), "tasks: max_concurrent must be between 1 and 20"},
		{json.RawMessage(`{"task":{"max_output_bytes":1023}}`), "tasks: max_output_bytes must be between 1024 and 1048576"},
		{json.RawMessage(`{"task":{"max_output_bytes":1048577}}`), "tasks: max_output_bytes must be between 1024 and 1048576"},
	}
	for _, tc := range rejected {
		_, err := decodeSettings(tc.raw)
		testassert.Check(t, err != nil && err.Error() == tc.want, "decodeSettings(%s) = %v, want exactly %q", tc.raw, err, tc.want)
	}
	wrongType := []json.RawMessage{
		json.RawMessage(`{"task":{"max_output_bytes":"many"}}`),
		json.RawMessage(`{"task":{"max_concurrent":"4"}}`),
		json.RawMessage(`{"task":{"max_concurrent":4.5}}`),
		json.RawMessage(`{"task":{"max_concurrent":4.0}}`),
		json.RawMessage(`{"task":{"max_concurrent":4e0}}`),
		json.RawMessage(`[]`),
	}
	for _, raw := range wrongType {
		_, err := decodeSettings(raw)
		testassert.Check(t, err != nil && strings.HasPrefix(err.Error(), "tasks: invalid settings: "), "decodeSettings(%s) = %v, want the tasks invalid-settings wrap", raw, err)
	}
	{
		s, err := decodeSettings(json.RawMessage(`{"task":{"MAX_CONCURRENT":1}}`))
		testassert.Require(t, err == nil, "decodeSettings with a mixed-case member name: %v", err)
		testassert.Check(t, s.MaxConcurrent == 1, "MaxConcurrent = %d, want 1 from the mixed-case member name", s.MaxConcurrent)
	}
}

// openTool opens the shipped task native with the captured extension section,
// mirroring the real composition handoff: settings are decoded and retained
// at Open, never at Prepare.
func openTool(t *testing.T, section string) extension.Tool {
	t.Helper()
	instance, err := Native(Options{}).Open(context.Background(), extension.InstanceContext{Section: json.RawMessage(section)})
	testassert.NoError(t, err)
	tool := instance.Capabilities[TaskCapabilityID].Tool
	testassert.Require(t, tool != nil, "task capability has no tool implementation")
	return tool
}

// taskCall builds one completed task call for the direct preparation tests.
func taskCall(t *testing.T, args string) model.ToolCall {
	t.Helper()
	call, err := model.NewToolCall(model.ToolCall{ID: "call-1", Name: "task", Arguments: json.RawMessage(args)})
	testassert.NoError(t, err)
	return call
}

// TestNormalizeTaskArguments proves the member validator.
func TestNormalizeTaskArguments(t *testing.T) {
	got, err := taskTool{}.Normalize(extension.ToolContext{}, taskCall(t, `{"prompt":"do it","subagent_type":"secondary","extra":42,"_lightcode_probe":"secret"}`))
	testassert.NoError(t, err)
	var args map[string]any
	testassert.NoError(t, json.Unmarshal(got, &args))
	{
		_, ok := args["_lightcode_probe"]
		testassert.Check(t, !ok, "normalized arguments retained the private field: %s", got)
	}
	testassert.Check(t, args["prompt"] == "do it" && args["subagent_type"] == "secondary", "normalized arguments = %s, want the members preserved", got)
	{
		extra, ok := args["extra"].(float64)
		testassert.Check(t, ok && extra == 42, "normalized arguments = %s, want the unrelated member preserved", got)
	}
	rejected := []struct {
		args string
		want string
	}{
		{`{"prompt":5,"subagent_type":"secondary"}`, "task: prompt must be a string"},
		{`{"prompt":"do it","subagent_type":7}`, "task: subagent_type must be a string"},
		{`{"prompt":null,"subagent_type":"secondary"}`, "task: prompt must be a string"},
		{`null`, "arguments must be a JSON object"},
	}
	for _, tc := range rejected {
		_, err := taskTool{}.Normalize(extension.ToolContext{}, taskCall(t, tc.args))
		testassert.Check(t, err != nil && err.Error() == tc.want, "Normalize(%s) error = %v, want exactly %q", tc.args, err, tc.want)
	}
}

// TestPrepareRejectsMissingAndUnknown proves the retained shape errors.
func TestPrepareRejectsMissingAndUnknown(t *testing.T) {
	tool := openTool(t, "")
	tc := extension.ToolContext{}
	prepare := func(args string) extension.ToolPlan {
		t.Helper()
		plan := tool.Prepare(context.Background(), tc, taskCall(t, args))
		testassert.Require(t, plan.Immediate != nil, "Prepare(%s) = %+v, want an immediate validation error", args, plan)
		testassert.Check(t, plan.Permissions == nil && plan.Data == nil, "Prepare(%s) declared execution behind a validation error", args)
		testassert.Check(t, plan.Immediate.Result.Status == model.ResultError, "Prepare(%s) status = %q, want %q", args, plan.Immediate.Result.Status, model.ResultError)
		return plan
	}
	{
		got := prepare(`{}`).Immediate.Result.Content
		testassert.Check(t, got == "missing 'prompt' parameter", "missing prompt content = %q, want %q", got, "missing 'prompt' parameter")
	}
	{
		got := prepare(`{"prompt":"do it"}`).Immediate.Result.Content
		testassert.Check(t, got == "missing 'subagent_type' parameter", "missing subagent_type content = %q, want %q", got, "missing 'subagent_type' parameter")
	}
	{
		got := prepare(`{"prompt":"do it","subagent_type":"ghost"}`).Immediate.Result.Content
		testassert.Check(t, got == `unknown subagent type "ghost": unknown agent type "ghost"`, "unknown type content = %q, want the retained wrapped shape over the roster resolver's error", got)
	}
}

func TestExecuteUsesDeclaredChildLaunchBinding(t *testing.T) {
	tool := openTool(t, `{"task":{"max_concurrent":9,"max_output_bytes":4096}}`)
	call := taskCall(t, `{"prompt":"do it","subagent_type":"secondary"}`)
	tc := extension.ToolContext{AgentTypes: []protocol.Agent{{Name: "secondary", Subagent: true, Definition: protocol.DefinitionRef{ExtensionId: ExtensionID, CapabilityId: DefinitionsCapabilityID, DefinitionId: "secondary"}}}}
	plan := tool.Prepare(context.Background(), tc, call)
	testassert.Require(t, plan.Immediate == nil && plan.Data != nil, "Prepare = %+v, want an executable plan", plan)

	var requestedBinding, requestedMethod string
	var requestedArguments json.RawMessage
	view := func(_ context.Context, binding, method string, arguments json.RawMessage) (extension.Result, error) {
		requestedBinding, requestedMethod = binding, method
		requestedArguments = append(json.RawMessage(nil), arguments...)
		return extension.Result{Value: json.RawMessage(`{"child_session_id":"child-1"}`)}, nil
	}
	tc.Calls = extension.CallContext{Call: view}
	outcome := tool.Execute(context.Background(), tc, plan)
	testassert.Require(t, requestedBinding == "child_launch" && requestedMethod == "launch", "request = %s/%s, want the declared child_launch.launch binding", requestedBinding, requestedMethod)
	var arguments struct {
		Definition    harness.DefinitionRef `json:"definition"`
		Prompt        string                `json:"prompt"`
		OperationID   string                `json:"operation_id"`
		MaxConcurrent int                   `json:"max_concurrent"`
		OutputLimit   int                   `json:"output_limit"`
	}
	testassert.NoError(t, json.Unmarshal(requestedArguments, &arguments))
	testassert.Check(t, arguments.Definition == (harness.DefinitionRef{ExtensionID: ExtensionID, CapabilityID: DefinitionsCapabilityID, DefinitionID: "secondary"}) && arguments.Prompt == "do it" && len(arguments.OperationID) == 32, "launch arguments = %+v, want the validated child facts and a minted operation ID", arguments)
	testassert.Check(t, arguments.MaxConcurrent == 9 && arguments.OutputLimit == 4096, "launch arguments = %+v, want the settings retained at Open", arguments)
	testassert.Check(t, outcome.Result.Status == model.ResultSuccess && outcome.Result.CallID == "call-1" && strings.Contains(outcome.Result.Content, "child-1"), "outcome = %+v, want the call identity and child session ID", outcome.Result)
}

// TestOpenRetainsTaskSettings proves the single Open-time decode: absent,
// null and default member forms keep the retained defaults, an integer object
// retains its values, and non-integer lexemes the config schema admits as
// integers are rejected before the candidate publishes, with no coercion.
func TestOpenRetainsTaskSettings(t *testing.T) {
	prepare := func(t *testing.T, tool extension.Tool) (int, int) {
		t.Helper()
		tc := extension.ToolContext{AgentTypes: []protocol.Agent{{Name: "secondary", Subagent: true, Definition: protocol.DefinitionRef{ExtensionId: ExtensionID, CapabilityId: DefinitionsCapabilityID, DefinitionId: "secondary"}}}}
		plan := tool.Prepare(context.Background(), tc, taskCall(t, `{"prompt":"do it","subagent_type":"secondary"}`))
		testassert.Require(t, plan.Immediate == nil && plan.Data != nil, "Prepare = %+v, want an executable plan", plan)
		var data struct {
			MaxConcurrent  int `json:"max_concurrent"`
			MaxOutputBytes int `json:"max_output_bytes"`
		}
		testassert.NoError(t, json.Unmarshal(plan.Data, &data))
		return data.MaxConcurrent, data.MaxOutputBytes
	}
	for _, row := range []struct {
		name    string
		section string
	}{
		{"absent section", ""},
		{"null task", `{"enabled":true,"task":null}`},
		{"absent members", `{"enabled":true,"task":{}}`},
		{"null members", `{"enabled":true,"task":{"max_concurrent":null,"max_output_bytes":null}}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			concurrent, output := prepare(t, openTool(t, row.section))
			testassert.Check(t, concurrent == 4 && output == 15360, "retained limits = %d/%d, want the defaults 4/15360", concurrent, output)
		})
	}
	{
		concurrent, output := prepare(t, openTool(t, `{"enabled":true,"task":{"max_concurrent":7,"max_output_bytes":2048}}`))
		testassert.Check(t, concurrent == 7 && output == 2048, "retained limits = %d/%d, want 7/2048", concurrent, output)
	}
	for _, section := range []string{
		`{"task":{"max_concurrent":1.0}}`,
		`{"task":{"max_concurrent":1e0}}`,
		`{"task":{"max_output_bytes":2048.0}}`,
	} {
		instance, err := Native(Options{}).Open(context.Background(), extension.InstanceContext{Section: json.RawMessage(section)})
		testassert.Check(t, err != nil && strings.HasPrefix(err.Error(), "tasks: invalid settings: "), "open(%s) = %v, want the invalid-settings rejection", section, err)
		testassert.Check(t, instance.Capabilities == nil, "open(%s) published an instance after a rejected setting", section)
	}
}

// TestConfigSchemaKeepsNullDefaults proves the shipped config schema admits
// the retained absent/null/default forms and integer members while refusing a
// non-object task document.
func TestConfigSchemaKeepsNullDefaults(t *testing.T) {
	compiled, err := extension.Compile([]extension.Native{Native(Options{})})
	testassert.NoError(t, err)
	for _, section := range []string{
		`{"enabled":true}`,
		`{"enabled":true,"task":null}`,
		`{"enabled":true,"task":{}}`,
		`{"enabled":true,"task":{"max_concurrent":null,"max_output_bytes":null}}`,
		`{"enabled":true,"task":{"max_concurrent":7,"max_output_bytes":2048}}`,
	} {
		testassert.Check(t, compiled.ValidateSection(ExtensionID, json.RawMessage(section)) == nil, "config schema refused the retained form %s", section)
	}
	testassert.Check(t, compiled.ValidateSection(ExtensionID, json.RawMessage(`{"enabled":true,"task":5}`)) != nil, "config schema accepted a non-object task document")
}
