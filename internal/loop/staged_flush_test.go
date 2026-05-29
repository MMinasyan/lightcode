package loop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/message"
	"github.com/MMinasyan/lightcode/internal/provider"
	"github.com/MMinasyan/lightcode/internal/tool"
)

// --- test doubles ---------------------------------------------------------

// stagedStubTool is a minimal tool registered so dispatch's registry.Get
// succeeds. For staged edit_file/write_file calls Execute is never reached
// (dispatch returns "Staged." before executing). For non-pending tools it
// returns a fixed result, or ErrDenied when denied is set.
type stagedStubTool struct {
	name   string
	result string
	denied bool
}

func (s stagedStubTool) Name() string                     { return s.name }
func (s stagedStubTool) Description() string              { return s.name }
func (s stagedStubTool) ParametersSchema() map[string]any { return map[string]any{"type": "object"} }
func (s stagedStubTool) Execute(context.Context, map[string]any) (string, error) {
	if s.denied {
		return "", tool.ErrDenied
	}
	return s.result, nil
}

// fakeFlushExecutor returns canned BatchResults for staged calls, mapping each
// staged ToolCallID to a configured result so flushPendingQueue emits real
// per-tool ToolCallEnd events without touching the filesystem.
type fakeFlushExecutor struct {
	resultByID map[string]tool.BatchResult
}

func (f fakeFlushExecutor) ExecutePending(_ context.Context, staged []tool.StagedCall) []tool.BatchResult {
	out := make([]tool.BatchResult, 0, len(staged))
	for _, s := range staged {
		if r, ok := f.resultByID[s.ToolCallID]; ok {
			r.ToolName = s.ToolName
			r.ToolCallID = s.ToolCallID
			out = append(out, r)
			continue
		}
		out = append(out, tool.BatchResult{ToolName: s.ToolName, ToolCallID: s.ToolCallID, Success: true, Result: "applied " + s.ToolCallID})
	}
	return out
}

// sseServer serves one SSE body per request, indexed by call count.
func sseServer(t *testing.T, bodies ...string) *httptest.Server {
	t.Helper()
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		i := n
		n++
		if i >= len(bodies) {
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, bodies[i])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func loopForServer(srv *httptest.Server, registry *tool.Registry) *Loop {
	prov := &catalog.Provider{
		ID:        "test",
		Transport: catalog.Transport{BaseURL: srv.URL + "/v1"},
		Models:    map[string]*catalog.Model{"model-a": {ID: "model-a"}},
	}
	client := provider.New(prov, prov.Models["model-a"], "")
	return New(client, registry, "system")
}

// editToolCallChunk renders an assistant SSE chunk that calls edit_file with
// pending=true (a staged edit). id is the tool_call id.
func editToolCallChunk(id string) string {
	args := `{\"path\":\"file.txt\",\"old_string\":\"a\",\"new_string\":\"b\",\"pending\":true}`
	return `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"` + id +
		`","type":"function","function":{"name":"edit_file","arguments":"` + args + `"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

// toolCallChunk renders an assistant SSE chunk calling a named tool with no args.
func toolCallChunk(id, name string) string {
	return `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"` + id +
		`","type":"function","function":{"name":"` + name + `","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

func twoToolCallChunk(id1, name1, args1, id2, name2, args2 string) string {
	return `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[` +
		`{"index":0,"id":"` + id1 + `","type":"function","function":{"name":"` + name1 + `","arguments":"` + args1 + `"}},` +
		`{"index":1,"id":"` + id2 + `","type":"function","function":{"name":"` + name2 + `","arguments":"` + args2 + `"}}` +
		`]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

func threeToolCallChunk(id1, name1, args1, id2, name2, args2, id3, name3, args3 string) string {
	return `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[` +
		`{"index":0,"id":"` + id1 + `","type":"function","function":{"name":"` + name1 + `","arguments":"` + args1 + `"}},` +
		`{"index":1,"id":"` + id2 + `","type":"function","function":{"name":"` + name2 + `","arguments":"` + args2 + `"}},` +
		`{"index":2,"id":"` + id3 + `","type":"function","function":{"name":"` + name3 + `","arguments":"` + args3 + `"}}` +
		`]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

func textChunk(content string) string {
	return `data: {"choices":[{"delta":{"role":"assistant","content":"` + content + `"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

func drainEvents(ch chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// --- tests ----------------------------------------------------------------

func TestStagedAutoFlushPersistsWrapperAndEmitsRealResults(t *testing.T) {
	srv := sseServer(t, editToolCallChunk("call_1"), textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(stagedStubTool{name: "edit_file"})
	lp := loopForServer(srv, registry)
	lp.SetPendingExecutor(fakeFlushExecutor{resultByID: map[string]tool.BatchResult{
		"call_1": {Success: true, Result: "Edited file.txt (1 replacement, lines 1-2)."},
	}})
	events := make(chan Event, 32)
	lp.SetEvents(events)

	if _, err := lp.Run(context.Background(), "edit it"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Persisted history: the "Staged." RoleTool stays for schema validity, and a
	// RoleUser <staged-flush> wrapper carries the real result for reload.
	msgs := lp.Messages()
	var stagedTool, wrapper *message.Message
	for i := range msgs {
		m := msgs[i]
		if m.Role == message.RoleTool && m.ToolCallID == "call_1" {
			stagedTool = &msgs[i]
		}
		if _, ok := ParseStagedFlushMessage(m); ok {
			wrapper = &msgs[i]
		}
	}
	if stagedTool == nil || stagedTool.TextContent() != "Staged." {
		t.Fatalf("expected RoleTool \"Staged.\" for call_1, got %#v", stagedTool)
	}
	if wrapper == nil {
		t.Fatalf("expected a <staged-flush> RoleUser wrapper in history: %#v", msgs)
	}
	entries, ok := ParseStagedFlushMessage(*wrapper)
	if !ok || len(entries) != 1 || entries[0].ID != "call_1" ||
		entries[0].Result != "Edited file.txt (1 replacement, lines 1-2)." || entries[0].IsError {
		t.Fatalf("wrapper entries = %#v", entries)
	}
	if wrapper.InternalKind != stagedFlushInternalKind {
		t.Fatalf("wrapper InternalKind = %q, want %q", wrapper.InternalKind, stagedFlushInternalKind)
	}

	// Live events: ToolCallStart, then the "Staged." end, then the real end.
	evs := drainEvents(events)
	var ends []Event
	sawStart := false
	for _, ev := range evs {
		if ev.Kind == ToolCallStart && ev.ToolCallID == "call_1" {
			sawStart = true
		}
		if ev.Kind == ToolCallEnd && ev.ToolCallID == "call_1" {
			ends = append(ends, ev)
		}
	}
	if !sawStart {
		t.Fatal("missing ToolCallStart for call_1")
	}
	if len(ends) != 2 {
		t.Fatalf("ToolCallEnd count for call_1 = %d, want 2 (Staged. then real)", len(ends))
	}
	if ends[0].Result != "Staged." {
		t.Fatalf("first end = %q, want \"Staged.\"", ends[0].Result)
	}
	if ends[1].Result != "Edited file.txt (1 replacement, lines 1-2)." {
		t.Fatalf("second (flush) end = %q, want real result", ends[1].Result)
	}
	if ends[1].Args == "" || ends[1].Args != ends[0].Args {
		t.Fatalf("late staged-flush args = %q, want original args %q", ends[1].Args, ends[0].Args)
	}
}

func TestStagedFlushSchemaValidMessageSequence(t *testing.T) {
	srv := sseServer(t, editToolCallChunk("call_1"), textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(stagedStubTool{name: "edit_file"})
	lp := loopForServer(srv, registry)
	lp.SetPendingExecutor(fakeFlushExecutor{resultByID: map[string]tool.BatchResult{
		"call_1": {Success: true, Result: "Edited file.txt (1 replacement, lines 1-2)."},
	}})
	lp.SetEvents(make(chan Event, 32))
	if _, err := lp.Run(context.Background(), "edit it"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Every assistant tool_call id must have a contiguous RoleTool response
	// immediately after the assistant message; the <staged-flush> RoleUser
	// summary follows the tool responses (it is not a tool response itself).
	msgs := lp.Messages()
	for i, m := range msgs {
		if m.Role != message.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		for j, tc := range m.ToolCalls {
			resp := msgs[i+1+j]
			if resp.Role != message.RoleTool || resp.ToolCallID != tc.ID {
				t.Fatalf("tool_call %s lacks a contiguous RoleTool response; got %#v", tc.ID, resp)
			}
		}
	}
	// The wrapper is RoleUser, never a RoleTool (so it is not mistaken for a
	// tool response for any tool_call_id).
	for _, m := range msgs {
		if _, ok := ParseStagedFlushMessage(m); ok && m.Role != message.RoleUser {
			t.Fatalf("staged-flush wrapper has role %q, want user", m.Role)
		}
	}
}

func TestExecutePendingReturnsSummaryAndPersistsWrapper(t *testing.T) {
	// One assistant message stages an edit and calls execute_pending.
	body := twoToolCallChunk(
		"call_1", "edit_file", `{\"path\":\"file.txt\",\"old_string\":\"a\",\"new_string\":\"b\",\"pending\":true}`,
		"call_2", "execute_pending", `{}`,
	)
	srv := sseServer(t, body, textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(stagedStubTool{name: "edit_file"})
	registry.Register(tool.ExecutePending{})
	lp := loopForServer(srv, registry)
	lp.SetPendingExecutor(fakeFlushExecutor{resultByID: map[string]tool.BatchResult{
		"call_1": {Success: true, Result: "Edited file.txt (1 replacement, lines 1-2)."},
	}})
	lp.SetEvents(make(chan Event, 32))
	if _, err := lp.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := lp.Messages()
	var execResult string
	var execInternalKind string
	var wrapperFound bool
	for _, m := range msgs {
		if m.Role == message.RoleTool && m.ToolCallID == "call_2" {
			execResult = m.TextContent()
			execInternalKind = m.InternalKind
		}
		if _, ok := ParseStagedFlushMessage(m); ok {
			wrapperFound = true
		}
	}
	if execResult != "Applied 1 staged edits." {
		t.Fatalf("execute_pending result = %q, want \"Applied 1 staged edits.\"", execResult)
	}
	if execInternalKind != "" {
		t.Fatalf("successful execute_pending InternalKind = %q, want empty", execInternalKind)
	}
	if !wrapperFound {
		t.Fatalf("expected a <staged-flush> wrapper after execute_pending: %#v", msgs)
	}
}

func TestExecutePendingAllFailedReturnsErrorSummary(t *testing.T) {
	body := twoToolCallChunk(
		"call_1", "edit_file", `{\"path\":\"file.txt\",\"old_string\":\"a\",\"new_string\":\"b\",\"pending\":true}`,
		"call_2", "execute_pending", `{}`,
	)
	srv := sseServer(t, body, textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(stagedStubTool{name: "edit_file"})
	registry.Register(tool.ExecutePending{})
	lp := loopForServer(srv, registry)
	lp.SetPendingExecutor(fakeFlushExecutor{resultByID: map[string]tool.BatchResult{
		"call_1": {Error: "edit failed"},
	}})
	events := make(chan Event, 32)
	lp.SetEvents(events)
	if _, err := lp.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := lp.Messages()
	var execResult string
	var execInternalKind string
	for _, m := range msgs {
		if m.Role == message.RoleTool && m.ToolCallID == "call_2" {
			execResult = m.TextContent()
			execInternalKind = m.InternalKind
		}
	}
	if execResult != "Failed to apply 1 staged edits." {
		t.Fatalf("execute_pending result = %q, want failure summary", execResult)
	}
	if execInternalKind != toolResultErrorKind {
		t.Fatalf("failed execute_pending InternalKind = %q, want %q", execInternalKind, toolResultErrorKind)
	}
	var sawExecuteError bool
	for _, ev := range drainEvents(events) {
		if ev.Kind == ToolCallEnd && ev.ToolCallID == "call_2" {
			sawExecuteError = ev.IsError && ev.Result == "Failed to apply 1 staged edits."
		}
	}
	if !sawExecuteError {
		t.Fatal("execute_pending ToolCallEnd should be marked error for an all-failed batch")
	}
}

func TestExecutePendingMixedResultsReturnsErrorSummary(t *testing.T) {
	body := threeToolCallChunk(
		"call_1", "edit_file", `{\"path\":\"file.txt\",\"old_string\":\"a\",\"new_string\":\"b\",\"pending\":true}`,
		"call_2", "write_file", `{\"path\":\"other.txt\",\"content\":\"x\",\"pending\":true}`,
		"call_3", "execute_pending", `{}`,
	)
	srv := sseServer(t, body, textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(stagedStubTool{name: "edit_file"})
	registry.Register(stagedStubTool{name: "write_file"})
	registry.Register(tool.ExecutePending{})
	lp := loopForServer(srv, registry)
	lp.SetPendingExecutor(fakeFlushExecutor{resultByID: map[string]tool.BatchResult{
		"call_1": {Success: true, Result: "Edited file.txt (1 replacement, lines 1-2)."},
		"call_2": {Error: "write failed"},
	}})
	events := make(chan Event, 32)
	lp.SetEvents(events)
	if _, err := lp.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := lp.Messages()
	var execResult string
	var execInternalKind string
	for _, m := range msgs {
		if m.Role == message.RoleTool && m.ToolCallID == "call_3" {
			execResult = m.TextContent()
			execInternalKind = m.InternalKind
		}
	}
	if execResult != "Applied 1 staged edits; 1 failed." {
		t.Fatalf("execute_pending result = %q, want mixed summary", execResult)
	}
	if execInternalKind != toolResultErrorKind {
		t.Fatalf("mixed execute_pending InternalKind = %q, want %q", execInternalKind, toolResultErrorKind)
	}
	var sawExecuteError bool
	for _, ev := range drainEvents(events) {
		if ev.Kind == ToolCallEnd && ev.ToolCallID == "call_3" {
			sawExecuteError = ev.IsError && ev.Result == "Applied 1 staged edits; 1 failed."
		}
	}
	if !sawExecuteError {
		t.Fatal("execute_pending ToolCallEnd should be marked error for a mixed batch")
	}
}

func TestFlushBeforeNonPendingToolHasNoPrefix(t *testing.T) {
	body := twoToolCallChunk(
		"call_1", "edit_file", `{\"path\":\"file.txt\",\"old_string\":\"a\",\"new_string\":\"b\",\"pending\":true}`,
		"call_2", "noop", `{}`,
	)
	srv := sseServer(t, body, textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(stagedStubTool{name: "edit_file"})
	registry.Register(stagedStubTool{name: "noop", result: "noop output"})
	lp := loopForServer(srv, registry)
	lp.SetPendingExecutor(fakeFlushExecutor{resultByID: map[string]tool.BatchResult{
		"call_1": {Success: true, Result: "Edited file.txt (1 replacement, lines 1-2)."},
	}})
	lp.SetEvents(make(chan Event, 32))
	if _, err := lp.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := lp.Messages()
	var noopResult string
	var wrapperFound bool
	for _, m := range msgs {
		if m.Role == message.RoleTool && m.ToolCallID == "call_2" {
			noopResult = m.TextContent()
		}
		if _, ok := ParseStagedFlushMessage(m); ok {
			wrapperFound = true
		}
	}
	if noopResult != "noop output" {
		t.Fatalf("non-pending tool result = %q, want exactly \"noop output\" (no batch prefix)", noopResult)
	}
	if !wrapperFound {
		t.Fatalf("expected a <staged-flush> wrapper for the flushed edit: %#v", msgs)
	}
}

func TestStagedFlushWrapperPersistedBeforeDeniedReturn(t *testing.T) {
	body := twoToolCallChunk(
		"call_1", "edit_file", `{\"path\":\"file.txt\",\"old_string\":\"a\",\"new_string\":\"b\",\"pending\":true}`,
		"call_2", "danger", `{}`,
	)
	srv := sseServer(t, body, textChunk("unreached"))
	registry := tool.NewRegistry()
	registry.Register(stagedStubTool{name: "edit_file"})
	registry.Register(stagedStubTool{name: "danger", denied: true})
	lp := loopForServer(srv, registry)
	lp.SetPendingExecutor(fakeFlushExecutor{resultByID: map[string]tool.BatchResult{
		"call_1": {Success: true, Result: "Edited file.txt (1 replacement, lines 1-2)."},
	}})
	lp.SetEvents(make(chan Event, 32))

	got, err := lp.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "Tool denied by user." {
		t.Fatalf("Run returned %q, want denied message", got)
	}

	// Even though Run returned on the denied path, the wrapper for the edit
	// flushed before the denied tool must be in history (the real result was
	// already emitted live, so reload must match).
	msgs := lp.Messages()
	var entries []StagedFlushEntry
	for _, m := range msgs {
		if e, ok := ParseStagedFlushMessage(m); ok {
			entries = e
		}
	}
	if len(entries) != 1 || entries[0].ID != "call_1" {
		t.Fatalf("expected staged-flush wrapper for call_1 persisted before denied return; entries=%#v", entries)
	}
}

func TestBuildAndParseStagedFlushRoundTrip(t *testing.T) {
	results := []tool.BatchResult{
		{ToolCallID: "a", Result: "ok a"},
		{ToolCallID: "b", Error: "boom"},
	}
	wrapper := BuildStagedFlush(results)
	if !strings.HasPrefix(wrapper, "<staged-flush>") || !strings.HasSuffix(wrapper, "</staged-flush>") {
		t.Fatalf("wrapper not marker-bounded: %q", wrapper)
	}
	entries, ok := ParseStagedFlush(wrapper)
	if !ok || len(entries) != 2 {
		t.Fatalf("parse round-trip: ok=%v entries=%#v", ok, entries)
	}
	if entries[0] != (StagedFlushEntry{ID: "a", Result: "ok a", IsError: false}) {
		t.Fatalf("entry[0] = %#v", entries[0])
	}
	if entries[1] != (StagedFlushEntry{ID: "b", Result: "boom", IsError: true}) {
		t.Fatalf("entry[1] = %#v (error string should win, IsError true)", entries[1])
	}

	// Empty results render no wrapper.
	if got := BuildStagedFlush(nil); got != "" {
		t.Fatalf("BuildStagedFlush(nil) = %q, want empty", got)
	}
	// Non-wrappers are not detected; malformed inner JSON is a wrapper with nil entries.
	if _, ok := ParseStagedFlush("just a user message"); ok {
		t.Fatal("plain text reported as staged-flush wrapper")
	}
	if e, ok := ParseStagedFlush("<staged-flush>not json</staged-flush>"); !ok || e != nil {
		t.Fatalf("malformed wrapper: ok=%v entries=%#v (want ok=true, nil entries)", ok, e)
	}
}

func TestEmptyExecutePendingKeepsNoPendingMessageAndNoWrapper(t *testing.T) {
	srv := sseServer(t, toolCallChunk("call_1", "execute_pending"), textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(tool.ExecutePending{})
	lp := loopForServer(srv, registry)
	lp.SetEvents(make(chan Event, 16))
	if _, err := lp.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := lp.Messages()
	var execResult string
	for _, m := range msgs {
		if m.Role == message.RoleTool && m.ToolCallID == "call_1" {
			execResult = m.TextContent()
		}
		if _, ok := ParseStagedFlushMessage(m); ok {
			t.Fatalf("no wrapper expected for empty execute_pending: %#v", msgs)
		}
	}
	if execResult != "No pending edits to execute." {
		t.Fatalf("empty execute_pending result = %q, want \"No pending edits to execute.\"", execResult)
	}
}
