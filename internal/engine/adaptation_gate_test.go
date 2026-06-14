package engine

import (
	"context"
	"testing"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/engine/modelclient"
	"github.com/MMinasyan/lightcode/internal/engine/tool"
	runtimetool "github.com/MMinasyan/lightcode/internal/tool"
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
// all.
func TestAdaptationFiltersAdvertisedTools(t *testing.T) {
	newRegistry := func() *tool.Registry {
		r := tool.NewRegistry()
		r.Register(stagedStubTool{name: "edit_file"})
		r.Register(runtimetool.ExecutePending{})
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
		if !requestHasTool(req, "execute_pending") {
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
		if !requestHasTool(req, "edit_file") || !requestHasTool(req, "execute_pending") {
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

// TestGateBlocksExcludedExecutePendingBeforeCoordinator proves the gate intercepts an
// excluded execute_pending ahead of the pending coordinator and the registry, WITH a
// staged edit present. If the gate did not precede the coordinator, call_2 would flush
// the staged edit and return "Applied 1 staged edits."; instead the gate returns the
// error, so the call reaches neither the coordinator (1101) nor the registry — the
// "does NOT flush staged edits" guarantee, scoped to the call. The staged edit is
// committed intent and still flushes at turn end via the separate flushPendingAtTurnEnd
// path (asserted below as the <staged-flush> wrapper), which the gate does not, and
// should not, suppress.
func TestGateBlocksExcludedExecutePendingBeforeCoordinator(t *testing.T) {
	body := twoToolCallChunk(
		"call_1", "edit_file", `{\"path\":\"file.txt\",\"old_string\":\"a\",\"new_string\":\"b\",\"pending\":true}`,
		"call_2", "execute_pending", `{}`,
	)
	srv := sseServer(t, body, textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(stagedStubTool{name: "edit_file"})
	registry.Register(runtimetool.ExecutePending{})
	lp := loopForServer(srv, registry)
	setTestPendingExecutor(lp, registry, fakeFlushExecutor{resultByID: map[string]tool.BatchResult{
		"call_1": {Success: true, Result: "Edited file.txt."},
	}})
	lp.SetActiveAdaptation(&adaptation.Adaptation{ExcludeTools: []string{"execute_pending"}})
	lp.SetEvents(make(chan Event, 32))
	if _, err := lp.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var execResult string
	var turnEndEntries []StagedFlushEntry
	for _, m := range lp.Messages() {
		if m.Role == message.RoleTool && m.ToolCallID == "call_2" {
			execResult = m.TextContent()
		}
		if entries, ok := ParseStagedFlushMessage(m); ok {
			turnEndEntries = entries
		}
	}
	// The gated call returns the gate error, never the coordinator's flush summary.
	if execResult != `error: tool "execute_pending" is not available` {
		t.Fatalf("execute_pending result = %q, want gate error (coordinator/registry not reached)", execResult)
	}
	// The staged edit was not flushed by the gated call, but is preserved and flushed
	// at turn end as committed intent — proving the gate suppresses only the call.
	if len(turnEndEntries) != 1 || turnEndEntries[0].ID != "call_1" {
		t.Fatalf("turn-end staged-flush entries = %#v, want one entry for call_1", turnEndEntries)
	}
}

// TestCoordinatorFlushesStagedEditsUnderAdaptation proves the coordinator stays intact:
// a staged edit followed by an ALLOWED execute_pending flushes via the coordinator
// ("Applied 1 staged edits." + a <staged-flush> wrapper) under both baseline and an
// active-but-non-excluding adaptation. The "Applied" summary is producible only by the
// coordinator (ExecutePending.Execute() is a no-op returning "No pending edits to
// execute."), so this distinguishes the coordinator from the registry path; the
// coordinator flushes at the call, emptying the queue before turn end (no turn-end
// flush). An active adaptation never replaces the coordinator with the no-op tool.
func TestCoordinatorFlushesStagedEditsUnderAdaptation(t *testing.T) {
	body := twoToolCallChunk(
		"call_1", "edit_file", `{\"path\":\"file.txt\",\"old_string\":\"a\",\"new_string\":\"b\",\"pending\":true}`,
		"call_2", "execute_pending", `{}`,
	)
	run := func(t *testing.T, adapt *adaptation.Adaptation) (result string, wrapper bool) {
		srv := sseServer(t, body, textChunk("done"))
		registry := tool.NewRegistry()
		registry.Register(stagedStubTool{name: "edit_file"})
		registry.Register(runtimetool.ExecutePending{})
		lp := loopForServer(srv, registry)
		setTestPendingExecutor(lp, registry, fakeFlushExecutor{resultByID: map[string]tool.BatchResult{
			"call_1": {Success: true, Result: "Edited file.txt."},
		}})
		lp.SetActiveAdaptation(adapt)
		lp.SetEvents(make(chan Event, 32))
		if _, err := lp.Run(context.Background(), "go"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, m := range lp.Messages() {
			if m.Role == message.RoleTool && m.ToolCallID == "call_2" {
				result = m.TextContent()
			}
			if _, ok := ParseStagedFlushMessage(m); ok {
				wrapper = true
			}
		}
		return result, wrapper
	}

	for _, tc := range []struct {
		name  string
		adapt *adaptation.Adaptation
	}{
		{"baseline", nil},
		{"active non-excluding adaptation", &adaptation.Adaptation{ExcludeTools: []string{"read_file"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, wrapper := run(t, tc.adapt)
			if got != "Applied 1 staged edits." {
				t.Fatalf("execute_pending result = %q, want \"Applied 1 staged edits.\" (coordinator flush)", got)
			}
			if !wrapper {
				t.Fatal("expected a <staged-flush> wrapper from the coordinator flush")
			}
		})
	}
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
	registry.Register(stagedStubTool{name: "edit_file"})
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

// TestGateBlocksPendingStagingForExcludedTool proves an excluded tool called with
// pending=true is rejected before staging (result is the gate error, not "Staged.").
func TestGateBlocksPendingStagingForExcludedTool(t *testing.T) {
	srv := sseServer(t, editToolCallChunk("call_1"), textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(stagedStubTool{name: "edit_file"})
	lp := loopForServer(srv, registry)
	lp.SetActiveAdaptation(&adaptation.Adaptation{ExcludeTools: []string{"edit_file"}})
	lp.SetEvents(make(chan Event, 16))
	if _, err := lp.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := toolResult(t, lp, "call_1"); got != `error: tool "edit_file" is not available` {
		t.Fatalf("staged excluded tool result = %q, want gate error (not \"Staged.\")", got)
	}
}

// TestGateLeavesUnknownToolMessageUnchanged proves an unregistered tool name falls
// through to the existing unknown-tool path (byte-identity under baseline), not the
// gate's "not available" message.
func TestGateLeavesUnknownToolMessageUnchanged(t *testing.T) {
	srv := sseServer(t, toolCallChunk("call_1", "ghost_tool"), textChunk("done"))
	registry := tool.NewRegistry()
	registry.Register(stagedStubTool{name: "edit_file"})
	lp := loopForServer(srv, registry)
	lp.SetEvents(make(chan Event, 16))
	if _, err := lp.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := toolResult(t, lp, "call_1"); got != `error: unknown tool "ghost_tool"` {
		t.Fatalf("unknown tool result = %q, want unknown-tool message", got)
	}
}
