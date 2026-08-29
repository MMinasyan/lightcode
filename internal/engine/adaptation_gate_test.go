package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/engine/modelclient"
	"github.com/MMinasyan/lightcode/internal/engine/tool"
	"github.com/MMinasyan/lightcode/internal/provider"
)

func textOnlyStream(content string) *sliceModelStream {
	return &sliceModelStream{deltas: []modelclient.StreamDelta{{
		Role:      string(message.RoleAssistant),
		Content:   content,
		HasChoice: true,
	}}}
}

func requestHasTool(req modelclient.ChatRequest, name string) bool {
	for _, t := range req.Tools {
		if t.Function != nil && t.Function.Name == name {
			return true
		}
	}
	return false
}

// hiddenStubTool is a DefaultHidden tool: withheld from the baseline advertisement,
// surfaced only when an adaptation includes it. Execute returns a fixed result.
type hiddenStubTool struct {
	name   string
	result string
}

func (h hiddenStubTool) Name() string                     { return h.name }
func (h hiddenStubTool) Description() string              { return h.name }
func (h hiddenStubTool) ParametersSchema() map[string]any { return map[string]any{"type": "object"} }
func (h hiddenStubTool) Execute(context.Context, map[string]any) (string, error) {
	return h.result, nil
}
func (h hiddenStubTool) DefaultHidden() bool { return true }

// TestAdaptationFiltersAdvertisedTools proves the request's Tools array reflects
// the active adaptation: excluded tools are dropped, others kept; baseline keeps
// all. Retained generic tools stand in for the removed staged-execution toolset.
func TestAdaptationFiltersAdvertisedTools(t *testing.T) {
	newRegistry := func() *tool.Registry {
		r := tool.NewRegistry()
		r.Register(simpleStubTool{name: "edit_file"})
		r.Register(simpleStubTool{name: "read_file"})
		return r
	}

	t.Run("excluded tool absent, others kept", func(t *testing.T) {
		client := &sequenceModelClient{
			ref:     coremodel.ModelRef{Provider: "p", Model: "m"},
			streams: []modelclient.ChatStream{textOnlyStream("done")},
		}
		lp := New(client, newRegistry(), "system")
		lp.SetActiveAdaptation(&adaptation.Adaptation{ExcludeTools: []string{"edit_file"}})
		if _, err := lp.Run(context.Background(), "hi"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		req := client.requests[0]
		if requestHasTool(req, "edit_file") {
			t.Fatal("advertised tools still contain excluded edit_file")
		}
		if !requestHasTool(req, "read_file") {
			t.Fatal("advertised tools dropped a non-excluded tool")
		}
	})

	t.Run("baseline advertises all", func(t *testing.T) {
		client := &sequenceModelClient{
			ref:     coremodel.ModelRef{Provider: "p", Model: "m"},
			streams: []modelclient.ChatStream{textOnlyStream("done")},
		}
		lp := New(client, newRegistry(), "system")
		if _, err := lp.Run(context.Background(), "hi"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		req := client.requests[0]
		if !requestHasTool(req, "edit_file") || !requestHasTool(req, "read_file") {
			t.Fatalf("baseline advertisement missing tools: %+v", req.Tools)
		}
	})
}

// toolResult returns the RoleTool message text for the given tool_call id.
func toolResult(t *testing.T, lp *Loop, id string) string {
	t.Helper()
	for _, m := range lp.Messages() {
		if m.Role == message.RoleTool && m.ToolCallID == id {
			return m.TextContent()
		}
	}
	t.Fatalf("no tool result for %q", id)
	return ""
}

// TestGateBlocksDefaultHiddenToolUnlessIncluded proves the dispatch gate (not just
// the advertisement filter) blocks a DefaultHidden tool under baseline and allows it
// when the active adaptation includes it.
func TestGateBlocksDefaultHiddenToolUnlessIncluded(t *testing.T) {
	run := func(t *testing.T, adapt *adaptation.Adaptation) string {
		srv := sseServer(t, toolCallChunk("call_1", "apply_patch"), textChunk("done"))
		registry := tool.NewRegistry()
		registry.Register(hiddenStubTool{name: "apply_patch", result: "patched"})
		lp := loopForServer(srv, registry)
		lp.SetActiveAdaptation(adapt)
		lp.SetEvents(make(chan Event, 16))
		if _, err := lp.Run(context.Background(), "go"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return toolResult(t, lp, "call_1")
	}

	t.Run("hidden under baseline -> blocked", func(t *testing.T) {
		if got := run(t, nil); got != `error: tool "apply_patch" is not available` {
			t.Fatalf("hidden tool result = %q, want gate error", got)
		}
	})
	t.Run("hidden + include -> dispatched", func(t *testing.T) {
		if got := run(t, &adaptation.Adaptation{IncludeTools: []string{"apply_patch"}}); got != "patched" {
			t.Fatalf("included hidden tool result = %q, want execution result", got)
		}
	})
}

// TestGateBlockedToolEmitsErrorRow proves a gate-blocked tool emits the same
// ToolCallStart -> ToolCallEnd(IsError) row shape as the unknown-tool path.
func TestGateBlockedToolEmitsErrorRow(t *testing.T) {
	srv := sseServer(t, toolCallChunk("call_1", "edit_file"), textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(simpleStubTool{name: "edit_file"})
	lp := loopForServer(srv, registry)
	lp.SetActiveAdaptation(&adaptation.Adaptation{ExcludeTools: []string{"edit_file"}})
	events := make(chan Event, 16)
	lp.SetEvents(events)
	if _, err := lp.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	startIdx, endIdx := -1, -1
	for i, ev := range drainEvents(events) {
		if ev.Kind == ToolCallStart && ev.ToolCallID == "call_1" {
			startIdx = i
		}
		if ev.Kind == ToolCallEnd && ev.ToolCallID == "call_1" {
			endIdx = i
			if !ev.IsError {
				t.Fatal("blocked tool ToolCallEnd not marked IsError")
			}
			if ev.Result != `error: tool "edit_file" is not available` {
				t.Fatalf("blocked tool result = %q", ev.Result)
			}
		}
	}
	if startIdx == -1 || endIdx == -1 || startIdx > endIdx {
		t.Fatalf("expected ToolCallStart before ToolCallEnd for call_1; start=%d end=%d", startIdx, endIdx)
	}
}

// TestGateLeavesUnknownToolMessageUnchanged proves an unregistered tool name falls
// through to the existing unknown-tool path (byte-identity under baseline), not the
// gate's "not available" message.
func TestGateLeavesUnknownToolMessageUnchanged(t *testing.T) {
	srv := sseServer(t, toolCallChunk("call_1", "ghost_tool"), textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(simpleStubTool{name: "edit_file"})
	lp := loopForServer(srv, registry)
	lp.SetEvents(make(chan Event, 16))
	if _, err := lp.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := toolResult(t, lp, "call_1"); got != `error: unknown tool "ghost_tool"` {
		t.Fatalf("unknown tool result = %q, want unknown-tool message", got)
	}
}

// simpleStubTool is a minimal tool registered so dispatch's registry.Get
// succeeds; Execute returns a fixed result, or ErrDenied when denied is set.
type simpleStubTool struct {
	name   string
	result string
	denied bool
}

func (s simpleStubTool) Name() string                     { return s.name }
func (s simpleStubTool) Description() string              { return s.name }
func (s simpleStubTool) ParametersSchema() map[string]any { return map[string]any{"type": "object"} }
func (s simpleStubTool) Execute(context.Context, map[string]any) (string, error) {
	if s.denied {
		return "", tool.ErrDenied
	}
	return s.result, nil
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
	return New(provider.NewAdapter(client), registry, "system")
}

// toolCallChunk renders an assistant SSE chunk calling a named tool with no args.
func toolCallChunk(id, name string) string {
	return `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"` + id +
		`","type":"function","function":{"name":"` + name + `","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
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
