package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentcfg "github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/config"
	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/lsp"
	"github.com/MMinasyan/lightcode/internal/memory"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/tool"
)

type taskTestRequest struct {
	Messages []map[string]any `json:"messages"`
}

func readTaskTestRequest(t *testing.T, r *http.Request) taskTestRequest {
	t.Helper()
	if r.Method != http.MethodPost || (r.URL.Path != "/chat/completions" && r.URL.Path != "/v1/chat/completions") {
		t.Fatalf("request = %s %s, want POST /chat/completions", r.Method, r.URL.Path)
	}
	var req taskTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return req
}

func taskTestMessageContent(req taskTestRequest) string {
	var b strings.Builder
	for _, msg := range req.Messages {
		if content, ok := msg["content"].(string); ok {
			b.WriteString(content)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func TestForwardEventsTagsEvents(t *testing.T) {
	tagged := make(chan TaggedLoopEvent, 3)
	task := &taskTool{taggedEvents: tagged}
	source := make(chan loop.Event, 3)
	source <- loop.Event{Kind: loop.ToolCallStart, ToolName: "read_file"}
	source <- loop.Event{Kind: loop.TextDelta, Result: "hello"}
	source <- loop.Event{Kind: loop.ToolCallEnd, ToolName: "read_file", Result: "done"}
	close(source)

	task.forwardEvents(source, 2, "subsession", "parent-session", "project-a", "parent-call")
	close(tagged)

	var got []TaggedLoopEvent
	for ev := range tagged {
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Fatalf("tagged events = %d, want 3", len(got))
	}
	for i, ev := range got {
		if ev.SessionID != "subsession" || ev.ParentSessionID != "parent-session" || ev.ProjectID != "project-a" || ev.TaskIndex != 2 || ev.ToolCallID != "parent-call" {
			t.Fatalf("event[%d] tag = session:%q parent:%q project:%q index:%d call:%q", i, ev.SessionID, ev.ParentSessionID, ev.ProjectID, ev.TaskIndex, ev.ToolCallID)
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
	task.forwardEvents(source, 0, "", "", "", "")
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
		task.forwardEvents(source, 1, "session", "root", "project", "parent")
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
		if ev.SessionID != "session" || ev.ParentSessionID != "root" || ev.ProjectID != "project" || ev.TaskIndex != 1 || ev.ToolCallID != "parent" || ev.Event.Kind != loop.TextDelta {
			t.Fatalf("event[%d] = %+v, want stable tags", i, ev)
		}
	}
}

func TestDrainPendingLoopEventsDrainsTaggedSubagentEvents(t *testing.T) {
	a := &Agent{}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	a.ensureSessionMapLocked()
	parentUnit := &session{rt: rt}
	a.sessions["parent-session"] = parentUnit
	rt.mu.Unlock()
	rt.taggedEvents = make(chan TaggedLoopEvent, 2)
	var got []Event
	a.SetEventHandler(func(ev Event) {
		got = append(got, ev)
	})

	rt.taggedEvents <- TaggedLoopEvent{
		SessionID:       "child-session",
		ParentSessionID: "parent-session",
		ProjectID:       "project-a",
		TaskIndex:       1,
		ToolCallID:      "parent-task",
		Event:           loop.Event{Kind: loop.ToolCallStart, ToolCallID: "child-tool", ToolName: "read_file"},
	}
	a.drainPendingLoopEvents()

	if len(got) != 2 {
		t.Fatalf("events = %#v, want subagent start and tool start", got)
	}
	if got[0].Kind != EventSubagentStart || got[0].SubagentSessionID != "child-session" || got[0].ParentSessionID != "parent-session" || got[0].ProjectID != "project-a" {
		t.Fatalf("event[0] = %+v, want subagent start", got[0])
	}
	if got[1].Kind != EventToolCallStart || got[1].SubagentSessionID != "child-session" || got[1].ParentSessionID != "parent-session" || got[1].ProjectID != "project-a" || got[1].ToolCallID != "child-tool" {
		t.Fatalf("event[1] = %+v, want child tool start", got[1])
	}
}

func TestTaskToolMetadataAndValidation(t *testing.T) {
	agents := mustParseTaskAgents(t, `{}`)
	task := newTaskTool(taskToolConfig{AgentTypes: agents})
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

func TestTaskToolAgentTypeResolutionUsesAgentsConfig(t *testing.T) {
	agents := mustParseTaskAgents(t, `{
		"primary": { "model": "test/test-model" },
		"worker": {
			"model": "test/alt-model",
			"tools": ["read_file"],
			"prompt": "Worker prompt.",
			"description": "Worker",
			"subagent": true
		}
	}`)
	task := newTaskTool(taskToolConfig{AgentTypes: agents})

	at, err := task.resolveAgentType("worker")
	if err != nil {
		t.Fatalf("resolveAgentType worker: %v", err)
	}
	if at.Model != "test/alt-model" || !at.Subagent {
		t.Fatalf("worker resolution = %#v, want explicit child model and subagent=true", at)
	}
	if _, err := task.resolveAgentType("primary"); err == nil {
		t.Fatal("resolveAgentType primary returned nil error, want non-subagent rejection")
	}
}

func TestTaskToolAgentTypeResolutionInheritsPrimaryModel(t *testing.T) {
	agents := mustParseTaskAgents(t, `{
		"primary": { "model": "test/test-model" },
		"worker": {
			"tools": ["read_file"],
			"prompt": "Worker prompt.",
			"description": "Worker",
			"subagent": true
		}
	}`)
	task := newTaskTool(taskToolConfig{AgentTypes: agents})

	at, err := task.resolveAgentType("worker")
	if err != nil {
		t.Fatalf("resolveAgentType worker: %v", err)
	}
	if at.Model != "test/test-model" {
		t.Fatalf("worker model = %q, want inherited primary model", at.Model)
	}
}

func TestTaskToolReadOnlyAndRegistryRouting(t *testing.T) {
	cases := []struct {
		name string
		at   agentcfg.Resolved
		want bool
	}{
		{name: "explicit read only", at: agentcfg.Resolved{Tools: []string{"read_file", "run_command"}, Readonly: true}, want: true},
		{name: "tools do not infer read only", at: agentcfg.Resolved{Tools: []string{"read_file", "run_command"}}, want: false},
		{name: "write file", at: agentcfg.Resolved{Tools: []string{"read_file", "write_file"}}, want: false},
		{name: "edit file", at: agentcfg.Resolved{Tools: []string{"edit_file"}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadOnlyType(tc.at); got != tc.want {
				t.Fatalf("isReadOnlyType(%+v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}

	task := &taskTool{}
	registry := task.buildRegistry(agentcfg.Resolved{Tools: []string{"read_file", "task"}, Readonly: true}, parentMutationScope{}, nil)
	if _, ok := registry.Get("read_file"); !ok {
		t.Fatal("buildRegistry missing allowed read_file tool")
	}
	if _, ok := registry.Get("task"); ok {
		t.Fatal("buildRegistry included recursive task tool")
	}
}

func TestTaskToolWriteDirKeepsWriteToolsConfinedAndRunCommandReadOnly(t *testing.T) {
	writeDir := t.TempDir()
	task := &taskTool{
		check: func(string, string) permission.Decision { return permission.DecisionAllow },
	}
	at := agentcfg.Resolved{
		Tools:    []string{"write_file", "edit_file", "apply_patch", "execute_pending", "run_command"},
		Readonly: true,
		WriteDir: writeDir,
	}

	registry := task.buildRegistry(at, parentMutationScope{}, nil)
	for _, name := range []string{"write_file", "edit_file", "apply_patch", "execute_pending", "run_command"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("write_dir registry missing %s; names=%v", name, taskRegistryToolNames(registry))
		}
	}
	runCommand, _ := registry.Get("run_command")
	if _, err := runCommand.Execute(context.Background(), map[string]any{"command": "touch should-not-run"}); err == nil {
		t.Fatal("write_dir read-only run_command accepted a write-style command; want read-only rejection")
	}
}

func TestTaskToolBuiltInExploreUsesResolvedCapabilities(t *testing.T) {
	agents := mustParseTaskAgents(t, `{}`)
	task := &taskTool{
		check:      func(string, string) permission.Decision { return permission.DecisionAllow },
		lspManager: lsp.NewManager(t.TempDir(), t.TempDir()),
	}
	at, err := agents.Resolve("explore", agentcfg.ResolveContext{})
	if err != nil {
		t.Fatalf("resolve explore: %v", err)
	}
	if !at.Readonly || !at.LSP {
		t.Fatalf("test setup: explore resolved readonly=%v lsp=%v, want both true", at.Readonly, at.LSP)
	}

	registry := task.buildRegistry(at, parentMutationScope{}, nil)
	for _, name := range []string{"write_file", "edit_file", "apply_patch"} {
		if _, ok := registry.Get(name); ok {
			t.Fatalf("explore registry contains %s; names=%v", name, taskRegistryToolNames(registry))
		}
		if registry.Advertises(name, nil) {
			t.Fatalf("explore advertises %s; names=%v", name, taskAdvertisedToolNames(registry))
		}
	}
	for _, name := range []string{"read_file", "run_command", "diagnostics", "workspace_symbol"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("explore registry missing %s; names=%v", name, taskRegistryToolNames(registry))
		}
	}

	runCommand, _ := registry.Get("run_command")
	if _, err := runCommand.Execute(context.Background(), map[string]any{"command": "touch should-not-run"}); err == nil {
		t.Fatal("explore run_command accepted a write-style command; want read-only rejection")
	}
}

func TestTaskToolCustomLSPCapabilityControlsLSPTools(t *testing.T) {
	agents := mustParseTaskAgents(t, `{
		"primary": { "model": "test/test-model" },
		"with_lsp": {
			"tools": ["read_file"],
			"lsp": true,
			"prompt": "With LSP.",
			"description": "With LSP",
			"subagent": true
		},
		"without_lsp": {
			"tools": ["read_file"],
			"lsp": false,
			"prompt": "Without LSP.",
			"description": "Without LSP",
			"subagent": true
		}
	}`)
	task := &taskTool{lspManager: lsp.NewManager(t.TempDir(), t.TempDir())}

	withLSP, err := agents.Resolve("with_lsp", agentcfg.ResolveContext{})
	if err != nil {
		t.Fatalf("resolve with_lsp: %v", err)
	}
	withRegistry := task.buildRegistry(withLSP, parentMutationScope{}, nil)
	for _, name := range []string{"diagnostics", "workspace_symbol"} {
		if _, ok := withRegistry.Get(name); !ok {
			t.Fatalf("lsp:true registry missing %s; names=%v", name, taskRegistryToolNames(withRegistry))
		}
	}

	withoutLSP, err := agents.Resolve("without_lsp", agentcfg.ResolveContext{})
	if err != nil {
		t.Fatalf("resolve without_lsp: %v", err)
	}
	withoutRegistry := task.buildRegistry(withoutLSP, parentMutationScope{}, nil)
	for _, name := range []string{"diagnostics", "workspace_symbol"} {
		if _, ok := withoutRegistry.Get(name); ok {
			t.Fatalf("lsp:false registry contains %s; names=%v", name, taskRegistryToolNames(withoutRegistry))
		}
	}
}

func TestTaskToolMemoryCapabilityControlsMemoryTools(t *testing.T) {
	task := &taskTool{memoryStore: memory.NewStore(nil, t.TempDir(), t.TempDir())}

	withMemory := task.buildRegistry(agentcfg.Resolved{
		Tools:  []string{"read_file"},
		Memory: true,
	}, parentMutationScope{}, nil)
	for _, name := range []string{"save_memory", "search_memory", "search_history"} {
		if _, ok := withMemory.Get(name); !ok {
			t.Fatalf("memory:true registry missing %s; names=%v", name, taskRegistryToolNames(withMemory))
		}
	}

	withoutMemory := task.buildRegistry(agentcfg.Resolved{
		Tools:  []string{"read_file"},
		Memory: false,
	}, parentMutationScope{}, nil)
	for _, name := range []string{"save_memory", "search_memory", "search_history"} {
		if _, ok := withoutMemory.Get(name); ok {
			t.Fatalf("memory:false registry contains %s; names=%v", name, taskRegistryToolNames(withoutMemory))
		}
	}

	onlyMemory := task.buildRegistry(agentcfg.Resolved{
		Tools:  []string{},
		Memory: true,
	}, parentMutationScope{}, nil)
	for _, name := range []string{"save_memory", "search_memory", "search_history"} {
		if _, ok := onlyMemory.Get(name); !ok {
			t.Fatalf("tools:[] memory:true registry missing %s; names=%v", name, taskRegistryToolNames(onlyMemory))
		}
	}
	for _, name := range []string{"read_file", "write_file", "edit_file", "run_command", "process", "sleep", "diagnostics", "workspace_symbol"} {
		if _, ok := onlyMemory.Get(name); ok {
			t.Fatalf("tools:[] memory:true registry contains non-memory tool %s; names=%v", name, taskRegistryToolNames(onlyMemory))
		}
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

	registry := task.buildRegistry(agentcfg.Resolved{Tools: []string{"run_command"}, Readonly: true}, parentMutationScope{}, nil)
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

	registry := task.buildRegistry(agentcfg.Resolved{Tools: []string{"run_command"}, Readonly: true}, parentMutationScope{}, nil)
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

	registry := task.buildRegistry(agentcfg.Resolved{Tools: []string{"run_command"}, Readonly: true}, parentMutationScope{}, nil)
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
	task.updateParentState(func() { cancelled = true })

	task.mu.Lock()
	cancel := task.cancelParent
	task.mu.Unlock()
	if cancel == nil {
		t.Fatal("cancelParent was not preserved")
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
	for _, s := range sessions {
		if s.ID == subEv.SubagentSessionID {
			t.Fatalf("child session %q should not be listed in active sessions", subEv.SubagentSessionID)
		}
	}

	childMsgs, err := a.SessionMessagesFor(subEv.SubagentSessionID)
	if err != nil {
		t.Fatalf("SessionMessagesFor child: %v", err)
	}
	if len(childMsgs) == 0 {
		t.Fatal("child transcript is empty, expected child messages")
	}

	parentMsgs := a.SessionMessages()
	var taskRow *DisplayMessage
	for i := range parentMsgs {
		msg := parentMsgs[i]
		if msg.Type == "user" && msg.Content == "inspect child" {
			t.Fatal("parent transcript contains child user prompt")
		}
		if msg.Type == "assistant" && msg.Content == "CHILD_ONLY" {
			t.Fatal("parent transcript contains child assistant row")
		}
		if msg.Type == "tool" && msg.Name == "task" {
			taskRow = &parentMsgs[i]
		}
	}
	if taskRow == nil {
		t.Fatalf("parent transcript missing task tool row: %#v", parentMsgs)
	}
	if len(taskRow.SubagentSessionIDs) != 1 {
		t.Fatalf("task subagent links = %#v, want one link", taskRow.SubagentSessionIDs)
	}
	if taskRow.SubagentSessionIDs[0].Index != 0 || taskRow.SubagentSessionIDs[0].SessionID != subEv.SubagentSessionID {
		t.Fatalf("task subagent link = %#v, want index 0 session %q", taskRow.SubagentSessionIDs[0], subEv.SubagentSessionID)
	}

	if got := a.SessionCurrent().ID; got != parentID {
		t.Fatalf("SessionMessagesFor switched current session to %q, want parent %q", got, parentID)
	}
	if !hasDisplayMessage(childMsgs, "user", "inspect child") {
		t.Fatalf("child transcript read by id missing task prompt: %#v", childMsgs)
	}
	if !hasDisplayMessage(childMsgs, "assistant", "CHILD_ONLY") {
		t.Fatalf("child transcript read by id missing assistant result: %#v", childMsgs)
	}

	if err := a.SessionSwitch(subEv.SubagentSessionID); err == nil {
		t.Fatal("SessionSwitch child should reject internal session")
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
	writeTaskAgentTypes(t, a, `"editor": {
		"description": "test editor",
		"tools": ["read_file", "edit_file", "execute_pending"],
		"prompt": "Test editor.",
		"subagent": true
	}`)
	// Bootstrap the session before mutating permissions: the live unit's
	// permission policy captures a.cfg at build time, and writeTaskAgentTypes's
	// Reload replaces a.cfg with a fresh pointer, so the Allow edit must land on
	// the config the live policy holds.
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	a.cfg.Permissions.Allow = []string{"read_file(/**)", "edit_file(/**)", "write_file(/**)"}
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
	if _, err := a.ApplyTurnActionForSession(a.SessionCurrent().ID, res.Turn, TurnActionRevertCode, false); err != nil {
		t.Fatalf("ApplyTurnActionForSession revert_code: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "old" {
		t.Fatalf("target after parent revert = %q, %v; want old", got, err)
	}
}

func TestParentTurnSnapshotStoreSerializesConcurrentSamePathWrites(t *testing.T) {
	root := t.TempDir()
	parentStore, err := snapshot.NewForSessionsRoot(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := parentStore.BeginNewSession(root); err != nil {
		t.Fatal(err)
	}
	turn := parentStore.BeginTurn()
	if turn != 1 {
		t.Fatalf("BeginTurn = %d, want 1", turn)
	}
	path := filepath.Join(root, "target.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &blockingParentTurnSnapshotStore{
		parentTurnSnapshotStore: parentTurnSnapshotStore{store: parentStore, turn: turn},
		firstRecordStarted:      make(chan struct{}),
		releaseFirstRecord:      make(chan struct{}),
		secondLockAttempt:       make(chan struct{}),
	}
	writeTool := tool.NewWriteFileWithSnapshot(store, nil, config.ToolsConfig{})

	firstDone := make(chan error, 1)
	go func() {
		_, err := writeTool.Execute(context.Background(), map[string]any{
			"path":    path,
			"content": "first",
		})
		firstDone <- err
	}()

	<-store.firstRecordStarted
	secondDone := make(chan error, 1)
	go func() {
		_, err := writeTool.Execute(context.Background(), map[string]any{
			"path":    path,
			"content": "second",
		})
		secondDone <- err
	}()

	secondFinishedBeforeRelease := false
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second write before first release: %v", err)
		}
		secondFinishedBeforeRelease = true
	case <-store.secondLockAttempt:
	case <-time.After(time.Second):
		t.Fatal("second write neither completed nor reached the parent-turn snapshot lock")
	}

	close(store.releaseFirstRecord)
	if err := <-firstDone; err != nil {
		t.Fatalf("first write: %v", err)
	}
	if !secondFinishedBeforeRelease {
		if err := <-secondDone; err != nil {
			t.Fatalf("second write: %v", err)
		}
	}

	if got, err := os.ReadFile(path); err != nil || string(got) != "second" {
		t.Fatalf("target after concurrent writes = %q, %v; want second", got, err)
	}
	affected, err := parentStore.RevertCode(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected.Restored) != 1 || affected.Restored[0] != path {
		t.Fatalf("RevertCode restored = %v, want %s", affected.Restored, path)
	}
	if len(affected.Skipped) != 0 {
		t.Fatalf("RevertCode skipped = %+v, want none", affected.Skipped)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "before" {
		t.Fatalf("target after parent revert = %q, %v; want before", got, err)
	}
}

func TestTaskToolAllDeniedSubagentsCancelParentTurn(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeTaskToolCallResponse(w, "call_task", "task", `{"tasks":[{"prompt":"run denied command","subagent_type":"explore"}]}`)
		case 2:
			writeTaskToolCallResponse(w, "call_cmd", "run_command", `{"command":"echo denied"}`)
		default:
			t.Fatalf("unexpected provider call")
		}
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.SetEventHandler(func(ev Event) {
		cap.handler(ev)
		if ev.Kind == EventPermissionRequest && ev.PermReq != nil {
			// The request event is delivered under the gate mutex, so respond off the
			// callback goroutine (as a real adapter does) rather than re-entering it.
			id := ev.PermReq.ID
			go func() { _ = a.RespondPermission(id, false) }()
		}
	})
	a.Init(ctx)

	if _, err := a.Submit(ctx, "delegate denied"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntilEventOrderTurnEndCount(t, cap, 1)

	var turnEnd Event
	for _, ev := range cap.snapshot() {
		if ev.Kind == EventTurnEnd {
			turnEnd = ev
		}
	}
	if !turnEnd.Cancelled {
		t.Fatalf("turn_end cancelled = false, events: %#v", cap.snapshot())
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want no parent follow-up after all-denied task", calls.Load())
	}
}

func TestTaskToolChildBackgroundProcessStaysChildScoped(t *testing.T) {
	var calls atomic.Int32
	var parentSawChildProcess atomic.Bool
	var a *Agent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := readTaskTestRequest(t, r)
		switch calls.Add(1) {
		case 1:
			writeTaskToolCallResponse(w, "call_task", "task", `{"tasks":[{"prompt":"run background command","subagent_type":"runner"}]}`)
		case 2:
			writeTaskToolCallResponse(w, "call_bg", "run_command", `{"command":"sleep 0.2; echo child-bg-marker","background":true}`)
		case 3:
			if a != nil && strings.Contains(a.procMgr.List(), "child-bg-marker") {
				parentSawChildProcess.Store(true)
			}
			writeTaskToolCallResponse(w, "call_sleep", "sleep", `{"seconds":1}`)
		case 4:
			if !strings.Contains(taskTestMessageContent(req), "child-bg-marker") {
				t.Fatalf("child follow-up messages missing background completion: %#v", req.Messages)
			}
			writeTextResponse(w, "child background done")
		case 5:
			if !strings.Contains(taskTestMessageContent(req), "child background done") {
				t.Fatalf("parent follow-up messages missing child result: %#v", req.Messages)
			}
			writeTextResponse(w, "parent done")
		default:
			t.Fatalf("unexpected provider call")
		}
	}))
	defer server.Close()

	a = newEventOrderAgent(t, server.URL+"/v1")
	writeTaskAgentTypes(t, a, `"runner": {
		"description": "test runner",
		"tools": ["run_command", "sleep", "process", "write_file"],
		"prompt": "Test runner.",
		"subagent": true
	}`)
	cap := &eventCapture{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.SetEventHandler(func(ev Event) {
		cap.handler(ev)
		if ev.Kind == EventPermissionRequest && ev.PermReq != nil {
			// The request event is delivered under the gate mutex, so respond off the
			// callback goroutine (as a real adapter does) rather than re-entering it.
			id := ev.PermReq.ID
			go func() { _ = a.RespondPermission(id, true) }()
		}
	})
	a.Init(ctx)

	if _, err := a.Submit(ctx, "delegate background"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntilEventOrderTurnEndCount(t, cap, 1)

	if parentSawChildProcess.Load() {
		t.Fatal("parent process manager listed child background process")
	}
	events := cap.snapshot()
	turnEndIndex := -1
	var childBackgroundEvent bool
	for i, ev := range events {
		if ev.Kind == EventTurnEnd {
			turnEndIndex = i
		}
		if ev.Kind == EventBackgroundProcessComplete {
			if ev.SubagentSessionID == "" {
				t.Fatalf("background completion event was parent-scoped: %+v", ev)
			}
			childBackgroundEvent = true
		}
	}
	if !childBackgroundEvent {
		t.Fatalf("missing child background completion event: %#v", events)
	}
	if turnEndIndex < 0 {
		t.Fatalf("missing turn_end event: %#v", events)
	}
	for _, ev := range events[turnEndIndex+1:] {
		if ev.SubagentSessionID != "" || ev.Kind == EventSubagentStart {
			t.Fatalf("subagent event arrived after parent turn_end: %+v in %#v", ev, events)
		}
	}
	for _, msg := range a.SessionMessages() {
		if msg.Type == "background_process" {
			t.Fatalf("parent transcript contains child background row: %#v", msg)
		}
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

func taskResolvedType(tools []string) agentcfg.Resolved {
	return agentcfg.Resolved{Tools: tools}
}

func taskRegistryToolNames(r *tool.Registry) []string {
	var names []string
	for _, name := range []string{
		"read_file", "write_file", "edit_file", "apply_patch", "run_command",
		"execute_pending", "process", "sleep", "task", "save_memory", "search_memory",
		"search_history", "diagnostics", "workspace_symbol",
	} {
		if _, ok := r.Get(name); ok {
			names = append(names, name)
		}
	}
	return names
}

func taskAdvertisedToolNames(r *tool.Registry) []string {
	var names []string
	for _, tl := range r.AdvertisedTools(nil) {
		if tl.Function != nil {
			names = append(names, tl.Function.Name)
		}
	}
	return names
}

func mustParseTaskAgents(t *testing.T, content string) *agentcfg.Config {
	t.Helper()
	cfg, err := agentcfg.Parse([]byte(content))
	if err != nil {
		t.Fatalf("parse agents config: %v", err)
	}
	return cfg
}

func writeTaskAgentTypes(t *testing.T, a *Agent, entries string) {
	t.Helper()
	writeAgentsTestConfig(t, a.configPath, `{
		"primary": { "model": "test/test-model" },
		`+entries+`
	}`)
	if err := a.Reload(); err != nil {
		t.Fatalf("reload task agent types: %v", err)
	}
}

type blockingParentTurnSnapshotStore struct {
	parentTurnSnapshotStore
	mu                 sync.Mutex
	records            int
	lockAttempts       int
	firstRecordStarted chan struct{}
	releaseFirstRecord chan struct{}
	secondLockAttempt  chan struct{}
}

func (s *blockingParentTurnSnapshotStore) LockSnapshotMutation(turn int, entryID string) (func(), error) {
	s.mu.Lock()
	s.lockAttempts++
	attempt := s.lockAttempts
	s.mu.Unlock()

	if attempt == 2 {
		close(s.secondLockAttempt)
	}
	return s.parentTurnSnapshotStore.LockSnapshotMutation(turn, entryID)
}

func (s *blockingParentTurnSnapshotStore) RecordSnapshotContent(turn int, entryID string, content []byte) error {
	s.mu.Lock()
	s.records++
	record := s.records
	s.mu.Unlock()

	if record == 1 {
		close(s.firstRecordStarted)
		<-s.releaseFirstRecord
	}
	return s.parentTurnSnapshotStore.RecordSnapshotContent(turn, entryID, content)
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

	at := taskResolvedType([]string{"run_command", "write_file"})
	if isReadOnlyType(at) {
		t.Fatal("test setup: at must be non-read-only")
	}
	registry := task.buildRegistry(at, parentMutationScope{}, nil)
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
	rt := agent.ensureRuntime()
	agent.session = &session{}
	agent.session.seenSessions = map[string]bool{}
	var wg sync.WaitGroup
	const N = 100
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			agent.dispatchTaggedEvent(TaggedLoopEvent{SessionID: "sub", ProjectID: "project", Event: loop.Event{Kind: loop.ToolCallStart}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			rt.mu.Lock()
			agent.session.seenSessions = map[string]bool{}
			rt.mu.Unlock()
		}
	}()
	wg.Wait()
}

// TestSessionClaimLifecycleContract proves a live task-child session holds the
// cross-process session claim for the whole subagent turn: a second process
// must be refused the child's claim while it runs, and must acquire it once the
// child closes. It drives the real task-tool construction (runSubagent's
// NewForSessionsRoot with the task tool's project context) through a live
// subagent turn and contends with a real second process, following the
// subprocess convention of the snapshot claim tests.
func TestSessionClaimLifecycleContract(t *testing.T) {
	if root := os.Getenv("LIGHTCODE_CLAIM_LIVENESS_PROJECTS_ROOT"); root != "" {
		// Child process: attempt the claim on the given session.
		_, ok, err := snapshot.AcquireSessionClaim(
			root,
			os.Getenv("LIGHTCODE_CLAIM_LIVENESS_PROJECT_ID"),
			os.Getenv("LIGHTCODE_CLAIM_LIVENESS_SESSION_ID"),
		)
		if err != nil {
			os.Exit(3) // unexpected error, not a contention verdict
		}
		if !ok {
			os.Exit(2) // refused: another process holds the claim
		}
		os.Exit(0) // acquired
	}

	childHeld := make(chan struct{})
	releaseChild := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeTaskToolCallResponse(w, "call_task", "task", `{"tasks":[{"prompt":"claim probe","subagent_type":"explore"}]}`)
		case 2:
			// The child's first provider request means its store is active and
			// holds the session claim; keep it live until the test is done.
			close(childHeld)
			<-releaseChild
			writeTextResponse(w, "child done")
		case 3:
			writeTextResponse(w, "parent done")
		default:
			t.Fatalf("unexpected provider call %d", calls.Load())
		}
	}))
	defer server.Close()
	// Release a blocked child turn on any exit path before server.Close, so a
	// failed assertion cannot deadlock cleanup on the still-open request.
	defer func() {
		select {
		case <-releaseChild:
		default:
			close(releaseChild)
		}
	}()

	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	proj, err := a.projects.Current()
	if err != nil || proj == nil {
		t.Fatalf("current project: %v", err)
	}
	parentID := a.SessionCurrent().ID

	submitDone := make(chan error, 1)
	go func() {
		_, err := a.Submit(ctx, "claim probe")
		submitDone <- err
	}()

	select {
	case <-childHeld:
	case <-time.After(10 * time.Second):
		t.Fatal("task-child never reached its live turn")
	}

	// The child session is published in the project sessions root; it is the
	// only session directory there other than the parent's.
	sessionsRoot := a.projects.SessionsRoot(proj.ID)
	childID := ""
	deadline := time.Now().Add(5 * time.Second)
	for childID == "" && time.Now().Before(deadline) {
		entries, err := os.ReadDir(sessionsRoot)
		if err != nil {
			t.Fatalf("read sessions root: %v", err)
		}
		for _, e := range entries {
			if e.IsDir() && e.Name() != parentID {
				childID = e.Name()
				break
			}
		}
		if childID == "" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if childID == "" {
		t.Fatal("task-child session directory not found")
	}

	attemptClaim := func() (int, string) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSessionClaimLifecycleContract$")
		cmd.Env = append(os.Environ(),
			"LIGHTCODE_CLAIM_LIVENESS_PROJECTS_ROOT="+a.projects.Root(),
			"LIGHTCODE_CLAIM_LIVENESS_PROJECT_ID="+proj.ID,
			"LIGHTCODE_CLAIM_LIVENESS_SESSION_ID="+childID,
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return 0, ""
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), ""
		}
		return -1, fmt.Sprintf("%v\n%s", err, out)
	}

	// While the child runs, a second process must be refused the claim.
	if code, detail := attemptClaim(); code != 2 {
		t.Fatalf("second process claim while child live = exit %d %s, want refusal (2)", code, detail)
	}

	close(releaseChild)
	if err := <-submitDone; err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntilEventOrderTurnEndCount(t, cap, 1)

	// Once the child closed, the same claim is acquirable again.
	if code, detail := attemptClaim(); code != 0 {
		t.Fatalf("second process claim after child close = exit %d %s, want acquisition (0)", code, detail)
	}
}
