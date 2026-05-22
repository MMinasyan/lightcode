package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/loop"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/subagent"
	"github.com/MMinasyan/lightcode/internal/tool"
)

func TestForwardEventsTagsEvents(t *testing.T) {
	tagged := make(chan TaggedLoopEvent, 3)
	task := &taskTool{taggedEvents: tagged}
	source := make(chan loop.Event, 3)
	source <- loop.Event{Kind: loop.ToolCallStart, ToolName: "read_file"}
	source <- loop.Event{Kind: loop.TextDelta, Result: "hello"}
	source <- loop.Event{Kind: loop.ToolCallEnd, ToolName: "read_file", Result: "done"}
	close(source)

	task.forwardEvents(source, 2, "subsession", "parent-call")
	close(tagged)

	var got []TaggedLoopEvent
	for ev := range tagged {
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Fatalf("tagged events = %d, want 3", len(got))
	}
	for i, ev := range got {
		if ev.SessionID != "subsession" || ev.TaskIndex != 2 || ev.ToolCallID != "parent-call" {
			t.Fatalf("event[%d] tag = session:%q index:%d call:%q", i, ev.SessionID, ev.TaskIndex, ev.ToolCallID)
		}
	}
	if got[0].Event.Kind != loop.ToolCallStart || got[1].Event.Kind != loop.TextDelta || got[2].Event.Kind != loop.ToolCallEnd {
		t.Fatalf("event order = %+v, want forwarded FIFO order", got)
	}
}

func TestForwardEventsNoopWhenTaggedEventsNil(t *testing.T) {
	task := &taskTool{}
	source := make(chan loop.Event, 1)
	source <- loop.Event{Kind: loop.TextDelta, Result: "ignored"}
	close(source)
	task.forwardEvents(source, 0, "", "")
}

func TestForwardEventsBurst(t *testing.T) {
	const count = 200
	tagged := make(chan TaggedLoopEvent)
	source := make(chan loop.Event)
	task := &taskTool{taggedEvents: tagged}

	var forwardDone sync.WaitGroup
	forwardDone.Add(1)
	go func() {
		defer forwardDone.Done()
		task.forwardEvents(source, 1, "session", "parent")
	}()

	var received []TaggedLoopEvent
	var drainDone sync.WaitGroup
	drainDone.Add(1)
	go func() {
		defer drainDone.Done()
		for ev := range tagged {
			received = append(received, ev)
		}
	}()

	for i := 0; i < count; i++ {
		source <- loop.Event{Kind: loop.TextDelta, Result: "x"}
	}
	close(source)
	forwardDone.Wait()
	close(tagged)
	drainDone.Wait()

	if len(received) != count {
		t.Fatalf("forwarded burst events = %d, want %d", len(received), count)
	}
	for i, ev := range received {
		if ev.SessionID != "session" || ev.TaskIndex != 1 || ev.ToolCallID != "parent" || ev.Event.Kind != loop.TextDelta {
			t.Fatalf("event[%d] = %+v, want stable tags", i, ev)
		}
	}
}

func TestTaskToolMetadataAndValidation(t *testing.T) {
	loader := subagent.NewLoader(t.TempDir(), t.TempDir())
	task := newTaskTool(taskToolConfig{Loader: loader})
	if task.maxConcurrent != 4 {
		t.Fatalf("default maxConcurrent = %d, want 4", task.maxConcurrent)
	}
	if task.Name() != "task" {
		t.Fatalf("Name = %q, want task", task.Name())
	}
	if !strings.Contains(task.Description(), "explore") {
		t.Fatalf("Description = %q, want builtin subagent types", task.Description())
	}
	schema := task.ParametersSchema()
	if required, ok := schema["required"].([]string); !ok || !reflect.DeepEqual(required, []string{"tasks"}) {
		t.Fatalf("schema required = %#v, want [tasks]", schema["required"])
	}

	if _, err := task.Execute(context.Background(), map[string]any{}); err == nil {
		t.Fatal("Execute without tasks error = nil, want error")
	}
	if _, err := task.Execute(context.Background(), map[string]any{"tasks": []any{}}); err == nil {
		t.Fatal("Execute with empty tasks error = nil, want error")
	}
	if _, err := task.Execute(context.Background(), map[string]any{"tasks": "bad"}); err == nil {
		t.Fatal("Execute with invalid tasks error = nil, want error")
	}
}

func TestTaskToolReadOnlyAndRegistryRouting(t *testing.T) {
	cases := []struct {
		name  string
		tools []string
		want  bool
	}{
		{name: "read only", tools: []string{"read_file", "run_command"}, want: true},
		{name: "write file", tools: []string{"read_file", "write_file"}, want: false},
		{name: "edit file", tools: []string{"edit_file"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadOnlyType(subagent.AgentType{Tools: tc.tools}); got != tc.want {
				t.Fatalf("isReadOnlyType(%v) = %v, want %v", tc.tools, got, tc.want)
			}
		})
	}

	base := tool.NewRegistry()
	base.Register(stubTaskTool{name: "read_file"})
	base.Register(stubTaskTool{name: "task"})
	task := &taskTool{baseRegistry: base}
	registry := task.buildRegistry(subagent.AgentType{Tools: []string{"read_file", "task"}})
	if _, ok := registry.Get("read_file"); !ok {
		t.Fatal("buildRegistry missing allowed read_file tool")
	}
	if _, ok := registry.Get("task"); ok {
		t.Fatal("buildRegistry included recursive task tool")
	}
}

func TestTaskToolWrapsReadOnlyRunCommandWithParentPermission(t *testing.T) {
	var checkedTool, checkedArg string
	var asked bool
	task := &taskTool{
		check: func(toolName, arg string) permission.Decision {
			checkedTool, checkedArg = toolName, arg
			return permission.DecisionDeny
		},
		ask: func(context.Context, permission.Request) permission.ResponseAction {
			asked = true
			return permission.ResponseAllow
		},
	}

	registry := task.buildRegistry(subagent.AgentType{Tools: []string{"run_command"}})
	runCommand, ok := registry.Get("run_command")
	if !ok {
		t.Fatal("buildRegistry missing run_command")
	}

	_, err := runCommand.Execute(context.Background(), map[string]any{"command": "echo allowed"})
	if !errors.Is(err, tool.ErrDenied) {
		t.Fatalf("Execute error = %v, want ErrDenied", err)
	}
	if checkedTool != "run_command" || checkedArg != "echo allowed" {
		t.Fatalf("permission check = %q/%q, want run_command/echo allowed", checkedTool, checkedArg)
	}
	if asked {
		t.Fatal("ask called after explicit deny")
	}
}

func TestTaskToolReadOnlyRunCommandCanAskAndExecute(t *testing.T) {
	var checkedTool, checkedArg string
	var askedTool, askedArg string
	task := &taskTool{
		check: func(toolName, arg string) permission.Decision {
			checkedTool, checkedArg = toolName, arg
			return permission.DecisionAsk
		},
		ask: func(_ context.Context, req permission.Request) permission.ResponseAction {
			askedTool, askedArg = req.ToolName, req.Arg
			return permission.ResponseAllow
		},
	}

	registry := task.buildRegistry(subagent.AgentType{Tools: []string{"run_command"}})
	runCommand, ok := registry.Get("run_command")
	if !ok {
		t.Fatal("buildRegistry missing run_command")
	}

	result, err := runCommand.Execute(context.Background(), map[string]any{"command": "echo subagent-ok"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "subagent-ok") {
		t.Fatalf("Execute result = %q, want command output", result)
	}
	if checkedTool != "run_command" || checkedArg != "echo subagent-ok" {
		t.Fatalf("permission check = %q/%q, want run_command/echo subagent-ok", checkedTool, checkedArg)
	}
	if askedTool != checkedTool || askedArg != checkedArg {
		t.Fatalf("ask = %q/%q, want %q/%q", askedTool, askedArg, checkedTool, checkedArg)
	}
}

func TestTaskToolReadOnlyRunCommandFailsClosedWithoutPermissionGate(t *testing.T) {
	task := &taskTool{}

	registry := task.buildRegistry(subagent.AgentType{Tools: []string{"run_command"}})
	runCommand, ok := registry.Get("run_command")
	if !ok {
		t.Fatal("buildRegistry missing run_command")
	}

	_, err := runCommand.Execute(context.Background(), map[string]any{"command": "echo allowed"})
	if !errors.Is(err, tool.ErrDenied) {
		t.Fatalf("Execute error = %v, want ErrDenied", err)
	}
}

func TestTaskToolStateAndSessionID(t *testing.T) {
	task := &taskTool{}
	cancelled := false
	task.updateParentState("provider", "model", func() { cancelled = true })
	task.setSubModel("other/model")

	task.mu.Lock()
	providerName, model, subModel, cancel := task.providerName, task.model, task.subModel, task.cancelParent
	task.mu.Unlock()
	if providerName != "provider" || model != "model" || subModel != "other/model" {
		t.Fatalf("task state = %q/%q sub:%q, want provider/model sub other/model", providerName, model, subModel)
	}
	cancel()
	if !cancelled {
		t.Fatal("cancelParent was not preserved")
	}

	id1, id2 := genSessionID(), genSessionID()
	if len(id1) != 8 || len(id2) != 8 || id1 == id2 {
		t.Fatalf("genSessionID = %q, %q; want distinct 8-char IDs", id1, id2)
	}
}

type stubTaskTool struct{ name string }

func (s stubTaskTool) Name() string                     { return s.name }
func (s stubTaskTool) Description() string              { return "stub" }
func (s stubTaskTool) ParametersSchema() map[string]any { return map[string]any{"type": "object"} }
func (s stubTaskTool) Execute(context.Context, map[string]any) (string, error) {
	return "ok", nil
}

// A non-read-only subagent type that includes "run_command" must resolve
// to the parent's permission-wrapped run_command from baseRegistry, not
// an unwrapped instance.
func TestPR11Closure_NonReadOnlySubagentRunCommandIsPermissionWrapped(t *testing.T) {
	var sentinelCheckCalled bool
	sentinel := stubTaskTool{name: "run_command"}
	wrapped := tool.WrapWithPermission(
		sentinel,
		func(toolName, arg string) permission.Decision {
			sentinelCheckCalled = true
			return permission.DecisionAllow
		},
		func(context.Context, permission.Request) permission.ResponseAction {
			return permission.ResponseDeny
		},
	)

	base := tool.NewRegistry()
	base.Register(wrapped)
	base.Register(stubTaskTool{name: "write_file"})

	task := &taskTool{baseRegistry: base}
	// Non-read-only type: tool list includes write_file.
	at := subagent.AgentType{Tools: []string{"run_command", "write_file"}}
	if isReadOnlyType(at) {
		t.Fatal("test setup: at must be non-read-only")
	}
	registry := task.buildRegistry(at)
	runCommand, ok := registry.Get("run_command")
	if !ok {
		t.Fatal("buildRegistry missing run_command for non-read-only subagent")
	}

	_, err := runCommand.Execute(context.Background(), map[string]any{"command": "echo ok"})
	if err != nil {
		// We don't care whether the inner sentinel succeeds; we just need its
		// permission gate to have run.
		_ = err
	}
	if !sentinelCheckCalled {
		t.Fatal("permission check on wrapped run_command was not invoked; resolved tool was not the baseRegistry-wrapped variant")
	}
}

// Agent.seenSessions must be safe under concurrent dispatch + reset.
func TestPR11Closure_SeenSessionsNoRace(t *testing.T) {
	agent := &Agent{seenSessions: map[string]bool{}}
	var wg sync.WaitGroup
	const N = 100
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			agent.dispatchTaggedEvent(TaggedLoopEvent{SessionID: "sub", Event: loop.Event{Kind: loop.ToolCallStart}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			agent.mu.Lock()
			agent.seenSessions = map[string]bool{}
			agent.mu.Unlock()
		}
	}()
	wg.Wait()
}
