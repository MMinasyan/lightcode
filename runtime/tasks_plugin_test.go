package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/plugins/tasks"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

// The tasks-plugin composition tests: the real child-session tool is
// assembled through the composed-scope bridge, its declared description is
// driven with a configured Invocation roster (outside this package only the
// zero Invocation is constructible), its prepared calls forward the launch
// fields through the background bridge, and the composed Harness fixture
// proves permission denial settles only the calling call.

// taskDescriptionPrefix is the fixed model-facing introduction the task tool's
// description opens with.
const taskDescriptionPrefix = "Spawn a subagent to work on a task in the background. The subagent runs in its own context with the toolset defined by its subagent_type. You will be notified when it completes; the result arrives as a message. Continue with your next steps in the meantime."

// taskChildID is the fixture child session id the scripted bridge returns.
const taskChildID = "0a1b2c3d4e5f60718293a4b5c6d7e8f9"

// openTask composes the real tasks plugin through the composed-scope bridge
// and returns its single task tool export.
func openTask(t *testing.T) runtime.Tool {
	t.Helper()
	values, closeScope, err := runtime.ComposeScopeForTest(context.Background(), runtime.ScopeInfo{
		Kind:    runtime.ScopeRuntime,
		DataDir: t.TempDir(),
	}, []runtime.Plugin{tasks.Plugin()})
	if err != nil {
		t.Fatalf("compose tasks plugin: %v", err)
	}
	t.Cleanup(func() { _ = closeScope() })
	value, ok := values["task"]
	if !ok {
		t.Fatalf("composition is missing the tool export %q", "task")
	}
	toolValue, ok := value.(runtime.Tool)
	if !ok {
		t.Fatalf("export %q supplies %T, not a runtime.Tool", "task", value)
	}
	return toolValue
}

// taskContext is the ToolContext the direct task tests prepare with: the
// admitted-input identity, the given Invocation, and the scripted bridge.
func taskContext(t *testing.T, sections map[string]string, background runtime.BackgroundServices) runtime.ToolContext {
	t.Helper()
	invocation, err := runtime.ConfiguredInvocationForTest(sections)
	if err != nil {
		t.Fatalf("ConfiguredInvocationForTest: %v", err)
	}
	return runtime.ToolContext{
		AdmittedEntry: harness.EntryRef{SessionID: testSessionID, EntryID: testEntryID},
		Invocation:    invocation,
		Background:    background,
	}
}

// taskBackground is the scripted BackgroundServices for the direct task
// tests: it records every launch request and returns the scripted outcome.
type taskBackground struct {
	result   harness.LaunchChildResult
	err      error
	launched []harness.LaunchChildRequest
}

func (b *taskBackground) LaunchChild(_ context.Context, req harness.LaunchChildRequest) (harness.LaunchChildResult, error) {
	b.launched = append(b.launched, req)
	return b.result, b.err
}

func (*taskBackground) StartJob(context.Context, string, string, func(context.Context, string) error) error {
	return nil
}

func (*taskBackground) DeliverCompletion(context.Context, string, string, string) error { return nil }

// TestTaskDescriptionComposesFromRoster proves the declared description: the
// fixed prefix, the blank line, the header, and one retained roster line per
// Subagent entry in the loader's All() order over a configured Invocation;
// the retained schema's leaf descriptions and required members; and that a
// child Session identity returns Available false with a still-valid
// definition.
func TestTaskDescriptionComposesFromRoster(t *testing.T) {
	spec := tasks.Plugin().Provides[0]
	invocation, err := runtime.ConfiguredInvocationForTest(nil)
	if err != nil {
		t.Fatalf("ConfiguredInvocationForTest: %v", err)
	}

	root, err := runtime.DescribeToolForTest(spec, invocation, runtime.ToolConstraints{}, harness.SessionIdentity{SessionID: testSessionID})
	if err != nil {
		t.Fatalf("root describe: %v", err)
	}
	if !root.Available {
		t.Fatalf("root advertisement = %+v, want the tool available", root)
	}
	if root.Definition.Name != "task" {
		t.Fatalf("definition name = %q, want %q", root.Definition.Name, "task")
	}

	// The expected text composes from the loader's own roster: builtins
	// first, customs alphabetically, one retained line per Subagent entry.
	cfg, err := agents.Parse([]byte("{}"))
	if err != nil {
		t.Fatalf("agents.Parse: %v", err)
	}
	var want strings.Builder
	want.WriteString(taskDescriptionPrefix)
	want.WriteString("\n\nAvailable subagent types:\n")
	for _, at := range cfg.All() {
		if at.Subagent {
			fmt.Fprintf(&want, "- %s: %s\n", at.Name, at.Description)
		}
	}
	if !strings.Contains(want.String(), "- explore: ") {
		t.Fatalf("fixture roster = %q, want at least one builtin Subagent entry", want.String())
	}
	if root.Definition.Description != want.String() {
		t.Fatalf("description = %q, want %q", root.Definition.Description, want.String())
	}

	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(root.Definition.Parameters, &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("schema type = %q, want object", schema.Type)
	}
	if len(schema.Required) != 2 || schema.Required[0] != "prompt" || schema.Required[1] != "subagent_type" {
		t.Errorf("schema required = %q, want prompt and subagent_type", schema.Required)
	}
	if p := schema.Properties["prompt"]; p.Type != "string" || p.Description != "The task prompt for this subagent." {
		t.Errorf("prompt property = %+v, want the retained leaf description", p)
	}
	if p := schema.Properties["subagent_type"]; p.Type != "string" || p.Description != "The type of subagent to use." {
		t.Errorf("subagent_type property = %+v, want the retained leaf description", p)
	}

	child, err := runtime.DescribeToolForTest(spec, invocation, runtime.ToolConstraints{}, harness.SessionIdentity{
		SessionID:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ParentSessionID: testSessionID,
	})
	if err != nil {
		t.Fatalf("child describe: %v", err)
	}
	if child.Available {
		t.Fatalf("child advertisement = %+v, want the tool unavailable on child Sessions", child)
	}
	if child.Definition.Name != "task" || child.Definition.Description == "" ||
		string(child.Definition.Parameters) != string(root.Definition.Parameters) {
		t.Fatalf("child definition = %+v, want a still-valid definition", child.Definition)
	}
}

// TestTaskPrepareRejectsNonSubagentType proves the roster's Subagent filter
// behind the retained wrapped shape: an existing non-subagent type is
// rejected with the retained inner text, while the nearest sibling — an
// existing Subagent type — prepares its child.launch declaration.
func TestTaskPrepareRejectsNonSubagentType(t *testing.T) {
	tool := openTask(t)
	tc := taskContext(t, nil, &taskBackground{})

	denied := prepare(t, tool, tc, "task", `{"prompt":"do it","subagent_type":"primary"}`)
	if denied.Immediate == nil {
		t.Fatalf("non-subagent prepare = %+v, want an immediate validation error", denied)
	}
	want := `unknown subagent type "primary": agent type is not available as a subagent`
	if denied.Immediate.Result.Status != model.ResultError || denied.Immediate.Result.Content != want {
		t.Fatalf("non-subagent result = %+v, want %q", denied.Immediate.Result, want)
	}

	allowed := prepare(t, tool, tc, "task", `{"prompt":"do it","subagent_type":"secondary"}`)
	if allowed.Immediate != nil {
		t.Fatalf("subagent prepare = %+v, want a prepared plan", allowed.Immediate)
	}
	if len(allowed.Permissions) != 1 || allowed.Permissions[0] != (harness.PermissionRequest{Permission: "child.launch", Target: "secondary"}) {
		t.Fatalf("permissions = %+v, want the one child.launch pair on the type name", allowed.Permissions)
	}
}

// TestTaskLaunchForwardsFields proves the launch request carries every field
// through the bridge: the admitted parent identity, the requested type, the
// prompt as content, a fresh 32-lowercase-hex operation identity, and the
// decoded per-call settings (defaults first, then a configured section), with
// the retained immediate text returning the child session id.
func TestTaskLaunchForwardsFields(t *testing.T) {
	cases := []struct {
		name           string
		sections       map[string]string
		wantConcurrent int
		wantOutput     int
	}{
		{name: "defaults", sections: nil, wantConcurrent: 4, wantOutput: 15360},
		{name: "configured", sections: map[string]string{"tasks": `{"max_concurrent":7,"max_output_bytes":2048}`}, wantConcurrent: 7, wantOutput: 2048},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := openTask(t)
			background := &taskBackground{result: harness.LaunchChildResult{ChildSessionID: taskChildID}}
			ctx := taskContext(t, tc.sections, background)

			plan := prepare(t, tool, ctx, "task", `{"prompt":"do the thing","subagent_type":"secondary"}`)
			if plan.Immediate != nil {
				t.Fatalf("prepare = %+v, want a prepared plan", plan.Immediate)
			}
			outcome := plan.Execute(context.Background())

			wantContent := fmt.Sprintf("Task started in the background with session ID: `%s`. You will be notified when it completes. Continue with your next steps; the result will arrive as a message when it is ready.", taskChildID)
			if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != wantContent {
				t.Fatalf("outcome = %+v, want the retained immediate text with the child session id", outcome.Result)
			}
			if len(background.launched) != 1 {
				t.Fatalf("launches = %d, want exactly one", len(background.launched))
			}
			req := background.launched[0]
			if req.ParentSessionID != testSessionID {
				t.Errorf("ParentSessionID = %q, want %q", req.ParentSessionID, testSessionID)
			}
			if req.AgentType != "secondary" {
				t.Errorf("AgentType = %q, want secondary", req.AgentType)
			}
			if len(req.Content) != 1 || req.Content[0].Kind != model.PartText || req.Content[0].Text != "do the thing" {
				t.Errorf("Content = %+v, want the prompt as one text part", req.Content)
			}
			if !isLowerHexID(req.OperationID) {
				t.Errorf("OperationID = %q, want a fresh 32-lowercase-hex identity", req.OperationID)
			}
			if req.MaxConcurrent != tc.wantConcurrent {
				t.Errorf("MaxConcurrent = %d, want %d", req.MaxConcurrent, tc.wantConcurrent)
			}
			if req.OutputLimit != tc.wantOutput {
				t.Errorf("OutputLimit = %d, want %d", req.OutputLimit, tc.wantOutput)
			}
		})
	}
}

// isLowerHexID reports whether id is a 32-lowercase-hex identity.
func isLowerHexID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// TestTaskLaunchErrorsRenderAsToolErrors proves the uniform tool-error path:
// the cap rejection, the closed-or-stopping rejection, and Harness
// cancellation — every launch error — settle the call as an error result
// carrying the harness's own rejection text, with no other rendering.
func TestTaskLaunchErrorsRenderAsToolErrors(t *testing.T) {
	tool := openTask(t)
	cases := []struct {
		name string
		err  error
	}{
		{"cap", fmt.Errorf("%w: background group is full (1/1). Wait for a background member to complete, then retry", harness.ErrInvalid)},
		{"closed or stopping", fmt.Errorf("%w: background group is closed or stopping", harness.ErrInvalid)},
		{"harness cancellation", context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			background := &taskBackground{err: tc.err}
			ctx := taskContext(t, nil, background)

			plan := prepare(t, tool, ctx, "task", `{"prompt":"do the thing","subagent_type":"secondary"}`)
			if plan.Immediate != nil {
				t.Fatalf("prepare = %+v, want a prepared plan", plan.Immediate)
			}
			outcome := plan.Execute(context.Background())
			if outcome.Result.Status != model.ResultError {
				t.Errorf("status = %q, want %q", outcome.Result.Status, model.ResultError)
			}
			if outcome.Result.Content != tc.err.Error() {
				t.Errorf("content = %q, want the harness rejection text %q", outcome.Result.Content, tc.err.Error())
			}
			if len(background.launched) != 1 {
				t.Fatalf("launches = %d, want the bridge reached exactly once", len(background.launched))
			}
		})
	}
}

// TestComposedTaskPermissionDenialSettlesOnlyTheCall proves R12 through the
// task tool: one configured policy denies child.launch for the secondary type
// — the denied call settles as the fixed denial without reaching the bridge,
// the later allowed call on the Subagent sibling launches and settles as the
// retained immediate text, and the Operation itself still succeeds.
func TestComposedTaskPermissionDenialSettlesOnlyTheCall(t *testing.T) {
	policy := harness.ResolvePermissionPolicy(json.RawMessage(`{"rules":[
		{"permission":"child.launch","target":"secondary","access":"deny"}
	]}`), nil)
	invocation, err := runtime.ConfiguredInvocationForTest(nil)
	if err != nil {
		t.Fatalf("ConfiguredInvocationForTest: %v", err)
	}
	background := &taskBackground{result: harness.LaunchChildResult{ChildSessionID: taskChildID}}
	attempt := 0
	var mu sync.Mutex
	modelFn := func(_ context.Context, _ model.Request) (model.Stream, error) {
		mu.Lock()
		attempt++
		n := attempt
		mu.Unlock()
		switch n {
		case 1:
			return toolCallStream("call-denied", "task", `{"prompt":"denied","subagent_type":"secondary"}`), nil
		case 2:
			return toolCallStream("call-allowed", "task", `{"prompt":"allowed","subagent_type":"explore"}`), nil
		default:
			return stopStream("done"), nil
		}
	}
	th := newToolsHarnessWith(t, modelFn, toolsHarnessOpts{
		advertise:   []string{"task"},
		permissions: policy,
		invocation:  invocation,
		background:  background,
		extraPlugin: tasks.Plugin(),
		extraToolID: "task",
	})
	session := th.createSession()
	th.submit(session, "op-1", "exercise the denied and allowed launches")
	rec := th.awaitSettled(session, "op-1")
	if rec.State.Status != harness.OperationSuccess {
		t.Fatalf("operation settled %q, want success: %+v", rec.State.Status, rec.State.Terminal)
	}

	results := th.readToolResults(session)
	if len(results) != 2 {
		t.Fatalf("committed tool results = %d, want exactly the two calls", len(results))
	}
	denied := results["call-denied"]
	if denied.Status != model.ResultDenied || denied.Content != "Permission denied." {
		t.Fatalf("call-denied = %+v, want the fixed denial", denied)
	}
	allowed := results["call-allowed"]
	if allowed.Status != model.ResultSuccess || !strings.HasPrefix(allowed.Content, "Task started in the background with session ID: `") {
		t.Fatalf("call-allowed = %+v, want the retained immediate text", allowed)
	}
	if len(background.launched) != 1 {
		t.Fatalf("launches = %d, want only the allowed call to reach the bridge", len(background.launched))
	}
	if launch := background.launched[0]; launch.AgentType != "explore" {
		t.Fatalf("launched AgentType = %q, want explore", launch.AgentType)
	}
}
