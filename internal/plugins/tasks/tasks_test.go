package tasks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

// TestValidateConfigSettingsRangesAndShapes proves the settings decoder the
// plugin declares and per-call preparation share: absent/blank/null input and
// null members keep the defaults, unknown members are ignored, both retained
// ranges accept their exact boundaries and reject everything outside with the
// retained texts, and malformed or wrong-typed input carries the
// invalid-settings wrap.
func TestValidateConfigSettingsRangesAndShapes(t *testing.T) {
	validate := Plugin().ValidateConfig
	if validate == nil {
		t.Fatal("Plugin().ValidateConfig is nil, want the settings validator")
	}
	keepDefaults := []json.RawMessage{
		nil,
		json.RawMessage(""),
		json.RawMessage("  \n"),
		json.RawMessage("null"),
		json.RawMessage(`{}`),
		json.RawMessage(`{"max_concurrent":null,"max_output_bytes":null}`),
		json.RawMessage(`{"unknown_member":1,"max_output_bytes":null}`),
	}
	for _, raw := range keepDefaults {
		if err := validate(raw); err != nil {
			t.Errorf("ValidateConfig(%s) = %v, want nil", raw, err)
		}
		s, err := decodeSettings(raw)
		if err != nil {
			t.Fatalf("decodeSettings(%s): %v", raw, err)
		}
		if s != (settings{MaxConcurrent: 4, MaxOutputBytes: 15360}) {
			t.Errorf("decodeSettings(%s) = %+v, want the defaults 4 and 15360", raw, s)
		}
	}
	inRange := []json.RawMessage{
		json.RawMessage(`{"max_concurrent":1,"max_output_bytes":1024}`),
		json.RawMessage(`{"max_concurrent":20,"max_output_bytes":1048576}`),
		json.RawMessage(`{"max_concurrent":7}`),
		json.RawMessage(`{"max_output_bytes":2048}`),
	}
	for _, raw := range inRange {
		if err := validate(raw); err != nil {
			t.Errorf("ValidateConfig(%s) = %v, want nil", raw, err)
		}
	}
	rejected := []struct {
		raw  json.RawMessage
		want string
	}{
		{json.RawMessage(`{"max_concurrent":0}`), "tasks: max_concurrent must be between 1 and 20"},
		{json.RawMessage(`{"max_concurrent":21}`), "tasks: max_concurrent must be between 1 and 20"},
		{json.RawMessage(`{"max_output_bytes":1023}`), "tasks: max_output_bytes must be between 1024 and 1048576"},
		{json.RawMessage(`{"max_output_bytes":1048577}`), "tasks: max_output_bytes must be between 1024 and 1048576"},
	}
	for _, tc := range rejected {
		err := validate(tc.raw)
		if err == nil || err.Error() != tc.want {
			t.Errorf("ValidateConfig(%s) = %v, want exactly %q", tc.raw, err, tc.want)
		}
	}
	wrongType := []json.RawMessage{
		json.RawMessage(`{"max_output_bytes":"many"}`),
		json.RawMessage(`{"max_concurrent":"4"}`),
		json.RawMessage(`{"max_concurrent":4.5}`),
		json.RawMessage(`[]`),
	}
	for _, raw := range wrongType {
		err := validate(raw)
		if err == nil || !strings.HasPrefix(err.Error(), "tasks: invalid settings: ") {
			t.Errorf("ValidateConfig(%s) = %v, want the tasks invalid-settings wrap", raw, err)
		}
	}
}

// taskCall builds one completed task call for the direct preparation tests.
func taskCall(t *testing.T, args string) model.ToolCall {
	t.Helper()
	call, err := model.NewToolCall(model.ToolCall{ID: "call-1", Name: "task", Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("NewToolCall: %v", err)
	}
	return call
}

// TestNormalizeTaskArguments proves the member validator: private
// `_lightcode_` fields are stripped, present prompt and subagent_type must be
// strings, every other member is preserved, and non-object arguments are
// rejected — Prepare owns the semantics.
func TestNormalizeTaskArguments(t *testing.T) {
	got, err := taskTool{}.Normalize(runtime.ToolContext{}, taskCall(t, `{"prompt":"do it","subagent_type":"secondary","extra":42,"_lightcode_probe":"secret"}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	var args map[string]any
	if err := json.Unmarshal(got, &args); err != nil {
		t.Fatalf("normalized arguments: %v", err)
	}
	if _, ok := args["_lightcode_probe"]; ok {
		t.Errorf("normalized arguments retained the private field: %s", got)
	}
	if args["prompt"] != "do it" || args["subagent_type"] != "secondary" {
		t.Errorf("normalized arguments = %s, want the members preserved", got)
	}
	if extra, ok := args["extra"].(float64); !ok || extra != 42 {
		t.Errorf("normalized arguments = %s, want the unrelated member preserved", got)
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
		tool := taskTool{}
		_, err := tool.Normalize(runtime.ToolContext{}, taskCall(t, tc.args))
		if err == nil || err.Error() != tc.want {
			t.Errorf("Normalize(%s) error = %v, want exactly %q", tc.args, err, tc.want)
		}
	}
}

// TestPrepareRejectsMissingAndUnknown proves the retained shape errors: the
// missing-parameter texts arrive before any resolution, and an unknown type
// name returns the retained wrapped shape over the roster resolver's error.
func TestPrepareRejectsMissingAndUnknown(t *testing.T) {
	// The zero Invocation carries an empty roster, so every name is unknown
	// and the shape checks run before resolution.
	tc := runtime.ToolContext{AdmittedEntry: harness.EntryRef{
		SessionID: "0123456789abcdef0123456789abcdef",
		EntryID:   "fedcba9876543210fedcba9876543210",
	}}
	prepare := func(args string) string {
		t.Helper()
		plan := taskTool{}.Prepare(context.Background(), tc, taskCall(t, args))
		if plan.Immediate == nil {
			t.Fatalf("Prepare(%s) = %+v, want an immediate validation error", args, plan)
		}
		if plan.Permissions != nil || plan.Execute != nil {
			t.Errorf("Prepare(%s) declared execution behind a validation error", args)
		}
		if plan.Immediate.Result.Status != model.ResultError {
			t.Errorf("Prepare(%s) status = %q, want %q", args, plan.Immediate.Result.Status, model.ResultError)
		}
		return plan.Immediate.Result.Content
	}
	if got := prepare(`{}`); got != "missing 'prompt' parameter" {
		t.Errorf("missing prompt content = %q, want %q", got, "missing 'prompt' parameter")
	}
	if got := prepare(`{"prompt":"do it"}`); got != "missing 'subagent_type' parameter" {
		t.Errorf("missing subagent_type content = %q, want %q", got, "missing 'subagent_type' parameter")
	}
	if got := prepare(`{"prompt":"do it","subagent_type":"ghost"}`); got != `unknown subagent type "ghost": unknown agent type "ghost"` {
		t.Errorf("unknown type content = %q, want the retained wrapped shape over the roster resolver's error", got)
	}
}
