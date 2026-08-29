package engine

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/engine/modelclient"
	"github.com/MMinasyan/lightcode/internal/engine/tool"
)

// --- leak-recovery test doubles ------------------------------------------

func leakAdaptation() *adaptation.Adaptation {
	return &adaptation.Adaptation{Name: "leaky", LeakPattern: regexp.MustCompile("LEAKED_CALL")}
}

func contentStream(content string) *sliceModelStream {
	return &sliceModelStream{deltas: []modelclient.StreamDelta{{
		Role: string(message.RoleAssistant), Content: content, HasChoice: true,
	}}}
}

func refusalStream(refusal string) *sliceModelStream {
	return &sliceModelStream{deltas: []modelclient.StreamDelta{{
		Role: string(message.RoleAssistant), Refusal: refusal, HasChoice: true,
	}}}
}

func mixedStream(content, id, name, args string) *sliceModelStream {
	return &sliceModelStream{deltas: []modelclient.StreamDelta{{
		Role:    string(message.RoleAssistant),
		Content: content,
		ToolCalls: []openai.ToolCall{{
			ID:       id,
			Type:     openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: name, Arguments: args},
		}},
		FinishReason: openai.FinishReasonToolCalls,
		HasChoice:    true,
	}}}
}

// cancelAfterContentStream emits one content delta, then cancels ctx and reports
// context.Canceled so consumeStream returns cancelled=true with that content.
type cancelAfterContentStream struct {
	content string
	sent    bool
	cancel  context.CancelFunc
}

func (s *cancelAfterContentStream) Recv() (modelclient.StreamDelta, error) {
	if !s.sent {
		s.sent = true
		return modelclient.StreamDelta{Role: string(message.RoleAssistant), Content: s.content, HasChoice: true}, nil
	}
	s.cancel()
	return modelclient.StreamDelta{}, context.Canceled
}

func (*cancelAfterContentStream) Close() error { return nil }

type cancellingClient struct {
	ref     coremodel.ModelRef
	content string
	cancel  context.CancelFunc
}

func (c *cancellingClient) ChatStream(context.Context, modelclient.ChatRequest) (modelclient.ChatStream, error) {
	return &cancelAfterContentStream{content: c.content, cancel: c.cancel}, nil
}
func (c *cancellingClient) ProtocolWarnings([]message.Message) []modelclient.ProtocolWarning {
	return nil
}
func (c *cancellingClient) Model() string                { return c.ref.Model }
func (c *cancellingClient) ModelRef() coremodel.ModelRef { return c.ref }

func historyHasSignal(msgs []message.Message, payload string) bool {
	for _, m := range msgs {
		if m.InternalKind == systemSignalInternalKind && strings.Contains(m.TextContent(), payload) {
			return true
		}
	}
	return false
}

func storeHasSignal(store *fakeStore, payload string) bool {
	for _, raw := range store.messages {
		var m message.Message
		if json.Unmarshal(raw, &m) == nil && m.InternalKind == systemSignalInternalKind && strings.Contains(m.TextContent(), payload) {
			return true
		}
	}
	return false
}

func historyHasToolResult(msgs []message.Message, id, content string) bool {
	for _, m := range msgs {
		if m.Role == message.RoleTool && m.ToolCallID == id && m.TextContent() == content {
			return true
		}
	}
	return false
}

// --- tests ----------------------------------------------------------------

// 1. Pure leak: signal queued (wake+persist), loop continues, signal drained into
// history before the second request and persisted, clean call dispatches.
func TestLeakRecoveryPureLeakQueuesSignalAndContinues(t *testing.T) {
	client := &sequenceModelClient{
		ref:     coremodel.ModelRef{Provider: "p", Model: "m"},
		streams: []modelclient.ChatStream{contentStream("LEAKED_CALL not parsed"), contentStream("all done")},
	}
	lp := New(client, tool.NewRegistry(), "system")
	store := &fakeStore{}
	lp.SetStore(store)
	lp.SetActiveAdaptation(leakAdaptation())

	out, err := lp.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "all done" {
		t.Fatalf("Run = %q, want \"all done\" (clean call dispatched)", out)
	}
	if len(client.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (loop continued past the leak)", len(client.requests))
	}
	if !historyHasSignal(client.requests[1].Messages, leakRecoverySignal) {
		t.Fatal("second request history missing the drained leak signal")
	}
	if !storeHasSignal(store, leakRecoverySignal) {
		t.Fatal("leak signal not persisted to the store")
	}
	if lp.HasPendingWakeSignal() {
		t.Fatal("a wake signal remains queued after it was drained")
	}
}

// 2. Mixed-then-allowed: the valid structured call executes, the signal is queued
// and rides (is drained before) the next request.
func TestLeakRecoveryMixedAllowedExecutesAndQueues(t *testing.T) {
	client := &sequenceModelClient{
		ref: coremodel.ModelRef{Provider: "p", Model: "m"},
		streams: []modelclient.ChatStream{
			mixedStream("LEAKED_CALL plus a real call", "call_1", "read_file", `{}`),
			contentStream("done"),
		},
	}
	registry := tool.NewRegistry()
	registry.Register(simpleStubTool{name: "read_file", result: "file contents"})
	lp := New(client, registry, "system")
	lp.SetActiveAdaptation(leakAdaptation())

	out, err := lp.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Fatalf("Run = %q, want done", out)
	}
	if len(client.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(client.requests))
	}
	if !historyHasToolResult(lp.Messages(), "call_1", "file contents") {
		t.Fatal("the valid structured call did not execute")
	}
	if !historyHasSignal(client.requests[1].Messages, leakRecoverySignal) {
		t.Fatal("leak signal not drained before the second request")
	}
}

// 3. Mixed-then-denied: Run returns the denial and no wake signal remains, so the
// turn-end defer fires no spurious autonomous turn.
func TestLeakRecoveryMixedDeniedQueuesNothing(t *testing.T) {
	client := &sequenceModelClient{
		ref: coremodel.ModelRef{Provider: "p", Model: "m"},
		streams: []modelclient.ChatStream{
			mixedStream("LEAKED_CALL plus a denied call", "call_1", "write_file", `{}`),
		},
	}
	registry := tool.NewRegistry()
	registry.Register(simpleStubTool{name: "write_file", denied: true})
	lp := New(client, registry, "system")
	lp.SetActiveAdaptation(leakAdaptation())

	out, err := lp.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "Tool denied by user." {
		t.Fatalf("Run = %q, want \"Tool denied by user.\"", out)
	}
	if lp.HasPendingWakeSignal() {
		t.Fatal("a wake signal is queued after a denied turn (would fire a spurious autonomous turn)")
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want 1 (turn ended on denial)", len(client.requests))
	}
}

// 4. Cancelled: a cancelled turn whose content matches queues no signal.
func TestLeakRecoveryCancelledQueuesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancellingClient{ref: coremodel.ModelRef{Provider: "p", Model: "m"}, content: "LEAKED_CALL", cancel: cancel}
	lp := New(client, tool.NewRegistry(), "system")
	lp.SetActiveAdaptation(leakAdaptation())

	_, _ = lp.Run(ctx, "go")
	if lp.HasPendingWakeSignal() {
		t.Fatal("a wake signal is queued after a cancelled turn")
	}
}

// 5. Refusal-only message containing tool-call-shaped text queues no signal (scan
// is content-only, never Refusal).
func TestLeakRecoveryRefusalNotScanned(t *testing.T) {
	client := &sequenceModelClient{
		ref:     coremodel.ModelRef{Provider: "p", Model: "m"},
		streams: []modelclient.ChatStream{refusalStream("LEAKED_CALL")},
	}
	lp := New(client, tool.NewRegistry(), "system")
	lp.SetActiveAdaptation(leakAdaptation())

	if _, err := lp.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lp.HasPendingWakeSignal() {
		t.Fatal("a refusal containing tool-call-shaped text misfired the leak scan")
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want 1 (refusal ends the turn)", len(client.requests))
	}
}

// 8. Chronic leaker: Run returns the maxIterations cap error and no wake signal
// remains (the final-iteration leak queues nothing).
func TestLeakRecoveryChronicLeakerHitsCapNoStrand(t *testing.T) {
	streams := make([]modelclient.ChatStream, maxIterations)
	for i := range streams {
		streams[i] = contentStream("LEAKED_CALL every time")
	}
	client := &sequenceModelClient{ref: coremodel.ModelRef{Provider: "p", Model: "m"}, streams: streams}
	lp := New(client, tool.NewRegistry(), "system")
	lp.SetActiveAdaptation(leakAdaptation())

	_, err := lp.Run(context.Background(), "go")
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("Run err = %v, want the maxIterations cap error", err)
	}
	if lp.HasPendingWakeSignal() {
		t.Fatal("a wake signal is queued after the cap (final-iteration leak must queue nothing)")
	}
	if len(client.requests) != maxIterations {
		t.Fatalf("requests = %d, want %d", len(client.requests), maxIterations)
	}
}

// 9. nil pattern: a zero-tool message ends the turn exactly as today (master invariant).
func TestLeakRecoveryNoPatternUnchanged(t *testing.T) {
	client := &sequenceModelClient{
		ref:     coremodel.ModelRef{Provider: "p", Model: "m"},
		streams: []modelclient.ChatStream{contentStream("LEAKED_CALL but no pattern set")},
	}
	lp := New(client, tool.NewRegistry(), "system")
	// No SetActiveAdaptation: activeAdapt is nil, so the leak scan never runs.

	out, err := lp.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "LEAKED_CALL but no pattern set" {
		t.Fatalf("Run = %q, want the content verbatim (turn ends as today)", out)
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(client.requests))
	}
	if lp.HasPendingWakeSignal() {
		t.Fatal("a signal was queued with no leak pattern")
	}
}
