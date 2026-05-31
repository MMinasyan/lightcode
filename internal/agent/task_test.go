package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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

	task := &taskTool{}
	registry := task.buildRegistry(subagent.AgentType{Tools: []string{"read_file", "task"}}, parentMutationScope{})
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

	registry := task.buildRegistry(subagent.AgentType{Tools: []string{"run_command"}}, parentMutationScope{})
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

	registry := task.buildRegistry(subagent.AgentType{Tools: []string{"run_command"}}, parentMutationScope{})
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

	registry := task.buildRegistry(subagent.AgentType{Tools: []string{"run_command"}}, parentMutationScope{})
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
}

func TestTaskToolPersistsInspectableChildSession(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeTaskToolCallResponse(w, "call_task", "task", `{"tasks":[{"prompt":"inspect child","subagent_type":"explore"}]}`)
		case 2:
			writeTextResponse(w, "CHILD_ONLY")
		case 3:
			writeTextResponse(w, "PARENT_DONE")
		default:
			t.Fatalf("unexpected provider call")
		}
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)
	if _, err := a.Submit(ctx, "delegate"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntilEventOrderTurnEndCount(t, cap, 1)

	parentID := a.SessionCurrent().ID
	subEv := findSubagentStart(t, cap)
	if subEv.SubagentSessionID == "" || subEv.SubagentSessionID == parentID {
		t.Fatalf("subagent session id = %q, parent = %q", subEv.SubagentSessionID, parentID)
	}

	sessions, err := a.SessionList("active")
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}
	var foundChild bool
	for _, s := range sessions {
		if s.ID == subEv.SubagentSessionID {
			foundChild = true
			if s.ParentSessionID != parentID {
				t.Fatalf("child ParentSessionID = %q, want %q", s.ParentSessionID, parentID)
			}
		}
	}
	if !foundChild {
		t.Fatalf("child session %q not listed in active sessions: %#v", subEv.SubagentSessionID, sessions)
	}

	for _, msg := range a.SessionMessages() {
		if msg.Type == "user" && msg.Content == "inspect child" {
			t.Fatal("parent transcript contains child user prompt")
		}
		if msg.Type == "assistant" && msg.Content == "CHILD_ONLY" {
			t.Fatal("parent transcript contains child assistant row")
		}
	}

	if err := a.SessionSwitch(subEv.SubagentSessionID); err != nil {
		t.Fatalf("SessionSwitch child: %v", err)
	}
	child := a.SessionCurrent()
	if child.ParentSessionID != parentID {
		t.Fatalf("current child ParentSessionID = %q, want %q", child.ParentSessionID, parentID)
	}
	childMsgs := a.SessionMessages()
	if !hasDisplayMessage(childMsgs, "user", "inspect child") {
		t.Fatalf("child transcript missing task prompt: %#v", childMsgs)
	}
	if !hasDisplayMessage(childMsgs, "assistant", "CHILD_ONLY") {
		t.Fatalf("child transcript missing assistant result: %#v", childMsgs)
	}
}

func TestTaskToolChildStagedEditUsesParentTurnSnapshot(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeTaskToolCallResponse(w, "call_task", "task", `{"tasks":[{"prompt":"edit target","subagent_type":"editor"}]}`)
		case 2:
			writeTaskToolCallResponse(w, "call_read", "read_file", `{"path":"target.txt"}`)
		case 3:
			writeTaskToolCallResponse(w, "call_edit", "edit_file", `{"path":"target.txt","old_string":"old","new_string":"new","pending":true}`)
		case 4:
			writeTextResponse(w, "child edited")
		case 5:
			writeTextResponse(w, "parent done")
		default:
			t.Fatalf("unexpected provider call")
		}
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	a.cfg.Permissions.Allow = []string{"read_file(/**)", "edit_file(/**)", "write_file(/**)"}
	writeProjectSubagentType(t, a.projectRoot, "editor", []string{"read_file", "edit_file", "execute_pending"})
	target := filepath.Join(a.projectRoot, "target.txt")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)
	res, err := a.Submit(ctx, "delegate edit")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntilEventOrderTurnEndCount(t, cap, 1)
	if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
		t.Fatalf("target after child staged edit = %q, %v; want new", got, err)
	}
	if _, err := a.ApplyTurnAction(res.Turn, TurnActionRevertCode, false); err != nil {
		t.Fatalf("ApplyTurnAction revert_code: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "old" {
		t.Fatalf("target after parent revert = %q, %v; want old", got, err)
	}
}

func writeTaskToolCallResponse(w http.ResponseWriter, callID, name, args string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`+"\n\n", callID, name, args)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func findSubagentStart(t *testing.T, cap *eventCapture) Event {
	t.Helper()
	for _, ev := range cap.snapshot() {
		if ev.Kind == EventSubagentStart {
			return ev
		}
	}
	t.Fatalf("missing subagent start event: %#v", cap.snapshot())
	return Event{}
}

func hasDisplayMessage(msgs []DisplayMessage, typ, content string) bool {
	for _, msg := range msgs {
		if msg.Type == typ && msg.Content == content {
			return true
		}
	}
	return false
}

func writeProjectSubagentType(t *testing.T, projectRoot, name string, tools []string) {
	t.Helper()
	dir := filepath.Join(projectRoot, ".lightcode", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\nname: %s\ndescription: test %s\ntools:\n", name, name)
	for _, toolName := range tools {
		fmt.Fprintf(&b, "  - %s\n", toolName)
	}
	fmt.Fprint(&b, "---\nTest subagent.")
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write subagent type: %v", err)
	}
}

type stubTaskTool struct{ name string }

func (s stubTaskTool) Name() string                     { return s.name }
func (s stubTaskTool) Description() string              { return "stub" }
func (s stubTaskTool) ParametersSchema() map[string]any { return map[string]any{"type": "object"} }
func (s stubTaskTool) Execute(context.Context, map[string]any) (string, error) {
	return "ok", nil
}

// A non-read-only subagent type that includes "run_command" must build a
// fresh permission-wrapped run_command, not reuse a parent registry object.
func TestSubagentRunCommandUsesFreshPermissionWrappedTool(t *testing.T) {
	var checkedTool, checkedArg string
	task := &taskTool{
		check: func(toolName, arg string) permission.Decision {
			checkedTool, checkedArg = toolName, arg
			return permission.DecisionAllow
		},
	}

	at := subagent.AgentType{Tools: []string{"run_command", "write_file"}}
	if isReadOnlyType(at) {
		t.Fatal("test setup: at must be non-read-only")
	}
	registry := task.buildRegistry(at, parentMutationScope{})
	runCommand, ok := registry.Get("run_command")
	if !ok {
		t.Fatal("buildRegistry missing run_command for non-read-only subagent")
	}

	_, err := runCommand.Execute(context.Background(), map[string]any{"command": "echo ok"})
	if err != nil {
		t.Fatalf("run_command Execute: %v", err)
	}
	if checkedTool != "run_command" || checkedArg != "echo ok" {
		t.Fatalf("permission check = %q/%q, want run_command/echo ok", checkedTool, checkedArg)
	}
}

// Agent.seenSessions must be safe under concurrent dispatch + reset.
func TestPR11Closure_SeenSessionsNoRace(t *testing.T) {
	agent := &Agent{}
	agent.ensureRuntime().seenSessions = map[string]bool{}
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
			agent.ensureRuntime().mu.Lock()
			agent.ensureRuntime().seenSessions = map[string]bool{}
			agent.ensureRuntime().mu.Unlock()
		}
	}()
	wg.Wait()
}
