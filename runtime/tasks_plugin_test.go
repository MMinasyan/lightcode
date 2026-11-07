package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/plugins/tasks"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

const taskDescriptionPrefix = "Spawn a subagent to work on a task in the background. The subagent runs in its own context with the toolset defined by its subagent_type. You will be notified when it completes; the result arrives as a message. Continue with your next steps in the meantime."

const taskChildID = "0a1b2c3d4e5f60718293a4b5c6d7e8f9"

func openTask(t *testing.T, section string) extension.Tool {
	t.Helper()
	instance, err := tasks.Native().Open(context.Background(), extension.InstanceContext{Section: json.RawMessage(section)})
	testassert.NoError(t, err)
	tool := instance.Capabilities["task"].Tool
	testassert.Require(t, tool != nil, "task capability has no tool implementation")
	return tool
}

type taskLaunch struct {
	binding   string
	method    string
	arguments json.RawMessage
	err       error
	childID   string
}

func (l *taskLaunch) context(t *testing.T, sections map[string]string) extension.ToolContext {
	t.Helper()
	invocation, err := runtime.ConfiguredInvocationForTest("", "", "", sections)
	testassert.NoError(t, err)
	call := func(_ context.Context, binding, method string, arguments json.RawMessage) (extension.Result, error) {
		l.binding, l.method, l.arguments = binding, method, append(json.RawMessage(nil), arguments...)
		if l.err != nil {
			return extension.Result{}, l.err
		}
		value, _ := json.Marshal(struct {
			ChildSessionID string `json:"child_session_id"`
		}{ChildSessionID: l.childID})
		return extension.Result{Value: value}, nil
	}
	return extension.ToolContext{
		Session:       harness.SessionIdentity{SessionID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		AdmittedEntry: harness.EntryRef{SessionID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", EntryID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		Config:        json.RawMessage(sections["agents"]),
		AgentTypes:    invocation.AgentTypes(),
		Calls:         extension.CallContext{Call: call},
	}
}

func taskCall(t *testing.T, args string) model.ToolCall {
	t.Helper()
	call, err := model.NewToolCall(model.ToolCall{ID: "call-1", Name: "task", Arguments: json.RawMessage(args)})
	testassert.NoError(t, err)
	return call
}

func TestTaskDescriptionComposesFromRoster(t *testing.T) {
	invocation, err := runtime.ConfiguredInvocationForTest("", "", "", nil)
	testassert.NoError(t, err)
	describe := tasks.Native().Describe["task"]
	root, err := describe(context.Background(), extension.DescribeInput{Session: harness.SessionIdentity{SessionID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, AgentTypes: invocation.AgentTypes()})
	testassert.NoError(t, err)
	testassert.Require(t, root.Available && root.Definition.Name == "task", "root advertisement = %+v", root)
	cfg, err := agents.ParseWithCapabilities([]byte("{}"), nil, nil, nil)
	testassert.NoError(t, err)
	var want strings.Builder
	want.WriteString(taskDescriptionPrefix)
	want.WriteString("\n\nAvailable subagent types:\n")
	for _, at := range cfg.All() {
		if at.Subagent {
			fmt.Fprintf(&want, "- %s: %s\n", at.Name, at.Description)
		}
	}
	testassert.Equal(t, root.Definition.Description, want.String())
	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	testassert.NoError(t, json.Unmarshal(root.Definition.Parameters, &schema))
	testassert.Check(t, schema.Type == "object" && len(schema.Required) == 2, "schema = %+v, want the retained shape", schema)
	child, err := describe(context.Background(), extension.DescribeInput{Session: harness.SessionIdentity{SessionID: "cccccccccccccccccccccccccccccccc", ParentSessionID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, AgentTypes: invocation.AgentTypes()})
	testassert.NoError(t, err)
	testassert.Require(t, !child.Available && child.Definition.Name == "task", "child advertisement = %+v, want unavailable", child)
}

func TestTaskPrepareAndLaunch(t *testing.T) {
	tool := openTask(t, `{"enabled":true,"task":{"max_concurrent":7,"max_output_bytes":2048}}`)
	launch := &taskLaunch{childID: taskChildID}
	tc := launch.context(t, nil)
	call := taskCall(t, `{"prompt":"do the thing","subagent_type":"secondary"}`)
	plan := tool.Prepare(context.Background(), tc, call)
	testassert.Require(t, plan.Immediate == nil && len(plan.Permissions) == 1, "Prepare = %+v, want the one child.launch plan", plan)
	testassert.Equal(t, plan.Permissions[0], harness.PermissionRequest{Permission: harness.PermissionChildLaunch, Target: "secondary"})
	outcome := tool.Execute(context.Background(), tc, plan)
	wantContent := fmt.Sprintf("Task started in the background with session ID: `%s`. You will be notified when it completes. Continue with your next steps; the result will arrive as a message when it is ready.", taskChildID)
	testassert.Require(t, outcome.Result.Status == model.ResultSuccess && outcome.Result.CallID == "call-1" && outcome.Result.Content == wantContent, "outcome = %+v, want the committed call identity and started text", outcome.Result)
	testassert.Require(t, launch.binding == "child_launch" && launch.method == "launch", "binding = %s/%s", launch.binding, launch.method)
	var arguments struct {
		AgentType     string `json:"agent_type"`
		Prompt        string `json:"prompt"`
		OperationID   string `json:"operation_id"`
		MaxConcurrent int    `json:"max_concurrent"`
		OutputLimit   int    `json:"output_limit"`
	}
	testassert.NoError(t, json.Unmarshal(launch.arguments, &arguments))
	testassert.Check(t, arguments.AgentType == "secondary" && arguments.Prompt == "do the thing" && len(arguments.OperationID) == 32, "arguments = %+v", arguments)
	testassert.Check(t, arguments.MaxConcurrent == 7 && arguments.OutputLimit == 2048, "arguments = %+v, want the configured limits", arguments)
}

func TestTaskPrepareRejectsNonSubagentType(t *testing.T) {
	tool := openTask(t, "")
	launch := &taskLaunch{childID: taskChildID}
	tc := launch.context(t, nil)
	plan := tool.Prepare(context.Background(), tc, taskCall(t, `{"prompt":"do it","subagent_type":"secondary"}`))
	testassert.Require(t, plan.Immediate == nil, "subagent prepare = %+v", plan)
	denied := tool.Prepare(context.Background(), tc, taskCall(t, `{"prompt":"do it","subagent_type":"primary"}`))
	testassert.Require(t, denied.Immediate != nil, "non-subagent prepare = %+v", denied)
	testassert.Equal(t, denied.Immediate.Result.Content, `unknown subagent type "primary": agent type is not available as a subagent`)
}

func TestTaskLaunchErrorsRenderAsToolErrors(t *testing.T) {
	tool := openTask(t, "")
	for _, row := range []struct {
		name string
		err  error
	}{{"closed", fmt.Errorf("closed: %w", harness.ErrInvalid)}, {"canceled", context.Canceled}} {
		t.Run(row.name, func(t *testing.T) {
			launch := &taskLaunch{childID: taskChildID, err: row.err}
			tc := launch.context(t, nil)
			plan := tool.Prepare(context.Background(), tc, taskCall(t, `{"prompt":"do the thing","subagent_type":"secondary"}`))
			testassert.Require(t, plan.Immediate == nil, "Prepare = %+v", plan)
			outcome := tool.Execute(context.Background(), tc, plan)
			testassert.Check(t, outcome.Result.Status == model.ResultError && outcome.Result.Content == row.err.Error(), "outcome = %+v, want the launch error", outcome.Result)
		})
	}
}
