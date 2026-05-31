package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/coremodel"
	"github.com/MMinasyan/lightcode/internal/message"
	"github.com/MMinasyan/lightcode/internal/modelclient"
	"github.com/MMinasyan/lightcode/internal/provider"
	"github.com/MMinasyan/lightcode/internal/tool"
	openai "github.com/sashabaranov/go-openai"
)

type fakeStore struct {
	turn     int
	messages [][]byte
}

func (s *fakeStore) AppendMessage(turn int, msg []byte) error {
	s.messages = append(s.messages, append([]byte(nil), msg...))
	return nil
}

func (s *fakeStore) MarkTurnComplete(turn int) error { return nil }
func (s *fakeStore) TouchActivity() error            { return nil }
func (s *fakeStore) CurrentTurn() int                { return s.turn }

type parsedTestStream struct {
	stream *provider.Stream
}

func newParsedTestStream(body string) parsedTestStream {
	return parsedTestStream{stream: provider.NewStream(io.NopCloser(strings.NewReader(body)))}
}

func (s parsedTestStream) Recv() (modelclient.StreamDelta, error) {
	chunk, err := s.stream.Recv()
	if err != nil {
		return modelclient.StreamDelta{}, err
	}
	delta, err := provider.ParseChunk(chunk.Raw)
	if err != nil {
		return modelclient.StreamDelta{}, modelclient.ChunkParseError{Err: err}
	}
	return delta, nil
}

func (s parsedTestStream) Close() error {
	return s.stream.Close()
}

type fakeModelClient struct {
	ref      coremodel.ModelRef
	model    string
	stream   modelclient.ChatStream
	warnings []modelclient.ProtocolWarning
	requests []modelclient.ChatRequest
}

func (c *fakeModelClient) ChatStream(_ context.Context, req modelclient.ChatRequest) (modelclient.ChatStream, error) {
	c.requests = append(c.requests, req)
	return c.stream, nil
}

func (c *fakeModelClient) ProtocolWarnings([]message.Message) []modelclient.ProtocolWarning {
	return c.warnings
}

func (c *fakeModelClient) Model() string {
	return c.model
}

func (c *fakeModelClient) ModelRef() coremodel.ModelRef {
	return c.ref
}

type sequenceModelClient struct {
	ref      coremodel.ModelRef
	streams  []modelclient.ChatStream
	requests []modelclient.ChatRequest
}

func (c *sequenceModelClient) ChatStream(_ context.Context, req modelclient.ChatRequest) (modelclient.ChatStream, error) {
	c.requests = append(c.requests, req)
	if len(c.streams) == 0 {
		return nil, fmt.Errorf("unexpected model request")
	}
	stream := c.streams[0]
	c.streams = c.streams[1:]
	return stream, nil
}

func (c *sequenceModelClient) ProtocolWarnings([]message.Message) []modelclient.ProtocolWarning {
	return nil
}

func (c *sequenceModelClient) Model() string { return c.ref.Model }

func (c *sequenceModelClient) ModelRef() coremodel.ModelRef { return c.ref }

type sliceModelStream struct {
	deltas []modelclient.StreamDelta
	next   int
}

func (s *sliceModelStream) Recv() (modelclient.StreamDelta, error) {
	if s.next >= len(s.deltas) {
		return modelclient.StreamDelta{}, io.EOF
	}
	delta := s.deltas[s.next]
	s.next++
	return delta, nil
}

func (*sliceModelStream) Close() error { return nil }

type checkpointRecorder struct {
	loop  *Loop
	calls []ContextTransformCheckpoint
	seen  [][]message.Message
}

func (r *checkpointRecorder) BeforeModelRequest(_ context.Context, checkpoint ContextTransformCheckpoint) (ContextTransformResult, error) {
	r.calls = append(r.calls, checkpoint)
	r.seen = append(r.seen, append([]message.Message(nil), r.loop.Messages()...))
	return ContextTransformResult{}, nil
}

func TestRunStreamUsesModelClientInterface(t *testing.T) {
	ref := coremodel.ModelRef{Provider: "fake-provider", Model: "fake-chat"}
	client := &fakeModelClient{
		ref:   ref,
		model: ref.Model,
		warnings: []modelclient.ProtocolWarning{{
			Kind:     "test_warning",
			Message:  "warning text",
			Provider: ref.Provider,
			Model:    ref.Model,
		}},
		stream: &sliceModelStream{deltas: []modelclient.StreamDelta{{
			Role:      string(message.RoleAssistant),
			Content:   "hello",
			HasChoice: true,
			Usage: &openai.Usage{
				PromptTokens:     7,
				CompletionTokens: 3,
			},
		}}},
	}
	lp := New(client, tool.NewRegistry(), "system")
	events := make(chan Event, 4)
	lp.SetEvents(events)

	msg, cancelled, err := lp.runStream(context.Background())
	if err != nil {
		t.Fatalf("runStream returned error: %v", err)
	}
	if cancelled {
		t.Fatal("runStream returned cancelled")
	}
	if got := msg.TextContent(); got != "hello" {
		t.Fatalf("assistant text = %q, want hello", got)
	}
	if msg.Source != ref {
		t.Fatalf("source = %#v, want %#v", msg.Source, ref)
	}
	if len(client.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(client.requests))
	}
	if len(client.requests[0].Messages) != 1 || client.requests[0].Messages[0].Role != message.RoleSystem {
		t.Fatalf("request messages = %#v", client.requests[0].Messages)
	}

	var sawWarning, sawUsage bool
	for i := 0; i < cap(events); i++ {
		select {
		case ev := <-events:
			switch ev.Kind {
			case Warning:
				sawWarning = ev.Result == "warning text"
			case Usage:
				sawUsage = ev.Model == ref.Model && ev.ModelRef == ref && ev.Input == 7 && ev.Output == 3
			}
		default:
			i = cap(events)
		}
	}
	if !sawWarning {
		t.Fatal("did not receive protocol warning event from model client")
	}
	if !sawUsage {
		t.Fatal("did not receive usage event with full ModelRef")
	}
}

func TestContextTransformerRunsBeforeFollowUpModelRequest(t *testing.T) {
	ref := coremodel.ModelRef{Provider: "fake-provider", Model: "fake-chat"}
	client := &sequenceModelClient{
		ref: ref,
		streams: []modelclient.ChatStream{
			&sliceModelStream{deltas: []modelclient.StreamDelta{{
				Role: string(message.RoleAssistant),
				ToolCalls: []openai.ToolCall{{
					ID:   "call_1",
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      "queue_signal",
						Arguments: `{}`,
					},
				}},
				FinishReason: openai.FinishReasonToolCalls,
				HasChoice:    true,
			}}},
			&sliceModelStream{deltas: []modelclient.StreamDelta{{
				Role:      string(message.RoleAssistant),
				Content:   "done",
				HasChoice: true,
			}}},
		},
	}
	registry := tool.NewRegistry()
	registry.Register(queueSignalTool{queue: func() {}})
	lp := New(client, registry, "system")
	recorder := &checkpointRecorder{loop: lp}
	lp.SetContextTransformer(recorder)

	if _, err := lp.Run(context.Background(), "use tool"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(client.requests))
	}
	if len(recorder.calls) < 2 {
		t.Fatalf("transformer calls = %d, want at least initial and follow-up", len(recorder.calls))
	}
	followUpSeen := recorder.seen[len(recorder.seen)-1]
	var sawToolResult bool
	for _, msg := range followUpSeen {
		if msg.Role == message.RoleTool && msg.ToolCallID == "call_1" && msg.TextContent() == "tool result" {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Fatalf("follow-up checkpoint did not see tool result; messages=%#v", followUpSeen)
	}
}

func TestNormalizeAssistantToolCallsUsesRegisteredNormalizer(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(tool.Sleep{})
	lp := &Loop{registry: registry}
	msg := message.Message{
		Role: message.RoleAssistant,
		ToolCalls: []message.ToolCall{{
			ID:       "call_sleep",
			Type:     "function",
			Function: message.FunctionCall{Name: "sleep", Arguments: `{"seconds":0}`},
		}},
	}

	got := lp.normalizeAssistantToolCalls(msg)
	if got.ToolCalls[0].Function.Arguments != `{"seconds":1}` {
		t.Fatalf("normalized args = %q, want seconds clamped", got.ToolCalls[0].Function.Arguments)
	}
}

func TestConsumeStreamHandlesSparseToolCallIndices(t *testing.T) {
	stream := newParsedTestStream(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"first","arguments":"{}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":2,"id":"call_2","type":"function","function":{"name":"third","arguments":"{}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	loop := &Loop{}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("consumeStream panicked on sparse tool call indices: %v", r)
		}
	}()

	msg, cancelled, err := loop.consumeStream(context.Background(), stream)
	if err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	if cancelled {
		t.Fatal("did not expect cancellation")
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %#v", len(msg.ToolCalls), msg.ToolCalls)
	}
	if msg.ToolCalls[0].ID != "call_0" || msg.ToolCalls[1].ID != "call_2" {
		t.Fatalf("tool calls not preserved in index order: %#v", msg.ToolCalls)
	}
}

func TestConsumeStreamAvoidsOmittedIndexCollisionWithExplicitIndex(t *testing.T) {
	stream := newParsedTestStream(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_omitted","type":"function","function":{"name":"read_file","arguments":"{}"}},{"index":0,"id":"call_explicit","type":"function","function":{"name":"write_file","arguments":"{}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	loop := &Loop{}

	msg, cancelled, err := loop.consumeStream(context.Background(), stream)
	if err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	if cancelled {
		t.Fatal("did not expect cancellation")
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %#v", len(msg.ToolCalls), msg.ToolCalls)
	}
	ids := map[string]bool{}
	for _, call := range msg.ToolCalls {
		ids[call.ID] = true
	}
	if !ids["call_omitted"] || !ids["call_explicit"] {
		t.Fatalf("tool call IDs not preserved separately: %#v", msg.ToolCalls)
	}
}

func TestEmitBlocksOnFullChannelForTranscriptEvents(t *testing.T) {
	ch := make(chan Event, 1)
	lp := &Loop{}
	lp.SetEvents(ch)

	transcriptKinds := []EventKind{
		TextDelta,
		ToolCallStart,
		ToolCallEnd,
		BackgroundProcessComplete,
		UserMessageDisplay,
		GenericSystemSignalDisplay,
	}
	for _, kind := range transcriptKinds {
		ch <- Event{Kind: Warning, Result: "filler"}
		done := make(chan struct{})
		go func(k EventKind) {
			lp.emit(Event{Kind: k, Result: "transcript"})
			close(done)
		}(kind)
		select {
		case <-done:
			t.Fatalf("emit(%v) returned while channel full — transcript event was dropped instead of blocking", kind)
		case <-time.After(20 * time.Millisecond):
		}
		<-ch
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("emit(%v) did not unblock after channel drained", kind)
		}
		got := <-ch
		if got.Kind != kind || got.Result != "transcript" {
			t.Fatalf("delivered event = %#v, want kind %v", got, kind)
		}
	}
	if lp.droppedEvents != 0 {
		t.Fatalf("droppedEvents = %d, want 0 for transcript-only emits", lp.droppedEvents)
	}
}

func TestEmitFlushesDroppedWarningBeforeTranscriptEvent(t *testing.T) {
	ch := make(chan Event, 4)
	lp := &Loop{droppedEvents: 7}
	lp.SetEvents(ch)

	lp.emit(Event{Kind: UserMessageDisplay, Turn: 5, Result: "hello"})

	first := <-ch
	if first.Kind != Warning || !strings.Contains(first.Result, "dropped 7 events") {
		t.Fatalf("expected dropped-events warning first, got %#v", first)
	}
	second := <-ch
	if second.Kind != UserMessageDisplay || second.Result != "hello" {
		t.Fatalf("expected transcript event second, got %#v", second)
	}
	if lp.droppedEvents != 0 {
		t.Fatalf("droppedEvents not reset after flush: %d", lp.droppedEvents)
	}
}

func TestEmitDropsTelemetryWhenChannelFull(t *testing.T) {
	ch := make(chan Event, 1)
	ch <- Event{Kind: Usage, Model: "filler"}
	loop := &Loop{}
	loop.SetEvents(ch)

	for i := 0; i < 10; i++ {
		loop.emit(Event{Kind: Usage, Model: "drop"})
	}
	if loop.droppedEvents != 10 {
		t.Fatalf("droppedEvents = %d, want 10", loop.droppedEvents)
	}

	<-ch
	loop.emit(Event{Kind: Usage, Model: "next"})
	if loop.droppedEvents != 1 {
		t.Fatalf("droppedEvents after warning = %d, want 1", loop.droppedEvents)
	}
	select {
	case ev := <-ch:
		if ev.Kind != Warning || !strings.Contains(ev.Result, "dropped 10 events") {
			t.Fatalf("warning event = %#v", ev)
		}
	default:
		t.Fatal("expected dropped-event warning")
	}
}

func TestSystemSignalEscapesAndWrapsPayload(t *testing.T) {
	got := SystemSignal(`a & b < c > d`)
	want := `<system-signal>a &amp; b &lt; c &gt; d</system-signal>`
	if got != want {
		t.Fatalf("SystemSignal() = %q, want %q", got, want)
	}
}

func TestSystemSignalEscapesLiteralClosingTag(t *testing.T) {
	got := SystemSignal(`before </system-signal> after`)
	inner := strings.TrimPrefix(strings.TrimSuffix(got, "</system-signal>"), "<system-signal>")
	if strings.Contains(inner, "</system-signal>") {
		t.Fatalf("SystemSignal() = %q, contains unescaped closing tag inside wrapper", got)
	}
	if !strings.Contains(inner, "&lt;/system-signal&gt;") {
		t.Fatalf("SystemSignal() = %q, missing escaped closing tag", got)
	}
}

func TestParseSystemSignalMessageRequiresInternalMarker(t *testing.T) {
	marked := NewSystemSignalMessage(`raw <payload> & data`)
	payload, ok := ParseSystemSignalMessage(marked)
	if !ok || payload != `raw <payload> & data` {
		t.Fatalf("marked system signal parse = %q, %v", payload, ok)
	}

	literal := message.NewText(message.RoleUser, SystemSignal(`raw <payload> & data`))
	if payload, ok := ParseSystemSignalMessage(literal); ok {
		t.Fatalf("literal user text parsed as system signal: %q", payload)
	}
}

func TestPendingSignalWrapsRawPayloadWhenDrained(t *testing.T) {
	lp := New(nil, nil, "system")
	lp.AddPendingSignal(PendingSignal{Payload: `raw <payload> & data`})
	lp.DrainPendingSignalsForModel(0)

	msgs := lp.Messages()
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want system plus signal", len(msgs))
	}
	got := msgs[1].TextContent()
	want := `<system-signal>raw &lt;payload&gt; &amp; data</system-signal>`
	if got != want {
		t.Fatalf("signal message = %q, want %q", got, want)
	}
	if msgs[1].InternalKind != systemSignalInternalKind {
		t.Fatalf("signal InternalKind = %q, want %q", msgs[1].InternalKind, systemSignalInternalKind)
	}
}

func TestPendingBackgroundProcessSignalEmitsDisplayWhenDrained(t *testing.T) {
	lp := New(nil, nil, "system")
	events := make(chan Event, 4)
	lp.SetEvents(events)
	lp.AddPendingSignal(PendingSignal{
		Payload: "Background process bg-1 finished",
		BackgroundProcess: &BackgroundProcessDisplay{
			ID:       "bg-1",
			Command:  "printf ok",
			Reason:   "completed",
			ExitCode: 0,
			Output:   "ok",
		},
	})
	lp.DrainPendingSignalsForModel(7)

	msgs := lp.Messages()
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want system plus signal", len(msgs))
	}
	if got := msgs[1].TextContent(); got != `<system-signal>Background process bg-1 finished</system-signal>` {
		t.Fatalf("signal message = %q", got)
	}
	if msgs[1].InternalKind != systemSignalInternalKind {
		t.Fatalf("signal InternalKind = %q, want %q", msgs[1].InternalKind, systemSignalInternalKind)
	}
	select {
	case ev := <-events:
		if ev.Kind != BackgroundProcessComplete {
			t.Fatalf("event kind = %v, want BackgroundProcessComplete", ev.Kind)
		}
		if ev.Turn != 7 || ev.IsError || ev.Result != "ok" {
			t.Fatalf("event = %#v, want turn 7 success output ok", ev)
		}
		if ev.BackgroundProcess == nil || ev.BackgroundProcess.ID != "bg-1" || ev.BackgroundProcess.Command != "printf ok" || ev.BackgroundProcess.Reason != "completed" || ev.BackgroundProcess.ExitCode != 0 || ev.BackgroundProcess.Output != "ok" {
			t.Fatalf("background process event = %#v", ev.BackgroundProcess)
		}
	default:
		t.Fatal("expected background process display event")
	}
}

func TestPlainPendingSignalEmitsGenericSystemSignalDisplay(t *testing.T) {
	lp := New(nil, nil, "system")
	events := make(chan Event, 4)
	lp.SetEvents(events)
	lp.AddPendingSignal(PendingSignal{Payload: "Model switched to test/test-model"})
	lp.DrainPendingSignalsForModel(3)

	select {
	case ev := <-events:
		if ev.Kind != GenericSystemSignalDisplay {
			t.Fatalf("event kind = %v, want GenericSystemSignalDisplay", ev.Kind)
		}
		if ev.Turn != 3 {
			t.Fatalf("event turn = %d, want 3", ev.Turn)
		}
		if ev.Result != "Model switched to test/test-model" {
			t.Fatalf("event result = %q, want collapsed payload", ev.Result)
		}
	default:
		t.Fatal("expected GenericSystemSignalDisplay for plain pending signal")
	}
	select {
	case ev := <-events:
		if ev.Kind == BackgroundProcessComplete {
			t.Fatalf("unexpected background-process event for plain signal: %#v", ev)
		}
	default:
	}
}

func TestResetHistoryClearsPendingSignals(t *testing.T) {
	lp := New(nil, nil, "system")
	lp.AddPendingSignal(PendingSignal{Payload: "old session signal", Wake: true})
	lp.ResetHistory()
	lp.DrainPendingSignalsForModel(0)

	msgs := lp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want only system after reset: %#v", len(msgs), msgs)
	}
}

func TestAppendUserMessageEmitsUserMessageDisplay(t *testing.T) {
	lp := New(nil, nil, "system")
	events := make(chan Event, 4)
	lp.SetEvents(events)
	lp.AppendUserMessage(7, "hi there")

	select {
	case ev := <-events:
		if ev.Kind != UserMessageDisplay || ev.Turn != 7 || ev.Result != "hi there" {
			t.Fatalf("event = %#v, want UserMessageDisplay turn=7 result=hi there", ev)
		}
	default:
		t.Fatal("expected UserMessageDisplay event")
	}
}

func TestRunEmitsUserMessageDisplayAfterDrain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	prov := &catalog.Provider{
		ID:        "test",
		Transport: catalog.Transport{BaseURL: server.URL + "/v1"},
		Models:    map[string]*catalog.Model{"model-a": {ID: "model-a"}},
	}
	client := provider.New(prov, prov.Models["model-a"], "")
	lp := New(provider.NewAdapter(client), tool.NewRegistry(), "system")
	events := make(chan Event, 32)
	lp.SetEvents(events)
	lp.AddPendingSignal(PendingSignal{Payload: "Model switched to test/model-a"})

	if _, err := lp.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var order []EventKind
	for {
		select {
		case ev := <-events:
			if ev.Kind == GenericSystemSignalDisplay || ev.Kind == UserMessageDisplay {
				order = append(order, ev.Kind)
			}
		default:
			if len(order) != 2 || order[0] != GenericSystemSignalDisplay || order[1] != UserMessageDisplay {
				t.Fatalf("display event order = %v, want signal then user", order)
			}
			return
		}
	}
}

func TestEnsureInterruptedSignalIsIdempotentAndEmitsOnce(t *testing.T) {
	lp := New(nil, nil, "system")
	events := make(chan Event, 4)
	lp.SetEvents(events)

	lp.EnsureInterruptedSignal(2)
	lp.EnsureInterruptedSignal(2)
	lp.EnsureInterruptedSignal(2)

	msgs := lp.Messages()
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want one system + one signal", len(msgs))
	}
	if msgs[1].TextContent() != interruptedSignal {
		t.Fatalf("last message = %q, want interrupted signal", msgs[1].TextContent())
	}
	if msgs[1].InternalKind != systemSignalInternalKind {
		t.Fatalf("interrupted signal InternalKind = %q, want %q", msgs[1].InternalKind, systemSignalInternalKind)
	}

	count := 0
	for {
		select {
		case ev := <-events:
			if ev.Kind == GenericSystemSignalDisplay && ev.Result == "Request interrupted by user" && ev.Turn == 2 {
				count++
			}
		default:
			if count != 1 {
				t.Fatalf("GenericSystemSignalDisplay emit count = %d, want 1", count)
			}
			return
		}
	}
}

func TestEnsureInterruptedSignalIgnoresLiteralInterruptedWrapper(t *testing.T) {
	lp := New(nil, nil, "system")
	literal := message.NewText(message.RoleUser, interruptedSignal)
	lp.messages = append(lp.messages, literal)

	lp.EnsureInterruptedSignal(2)

	msgs := lp.Messages()
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want system plus literal plus real signal", len(msgs))
	}
	if msgs[1].InternalKind != "" {
		t.Fatalf("literal InternalKind = %q, want empty", msgs[1].InternalKind)
	}
	if msgs[2].TextContent() != interruptedSignal || msgs[2].InternalKind != systemSignalInternalKind {
		t.Fatalf("real interrupted signal = %#v", msgs[2])
	}
}

func TestAppendUserMessageDoesNotDrainPendingSignals(t *testing.T) {
	lp := New(nil, nil, "system")
	lp.AddPendingSignal(PendingSignal{Payload: "idle signal", Wake: true})
	lp.AppendUserMessage(1, "next prompt")

	msgs := lp.Messages()
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want system plus user only: %#v", len(msgs), msgs)
	}
	if msgs[1].Role != message.RoleUser || msgs[1].TextContent() != "next prompt" {
		t.Fatalf("messages[1] = %#v, want user prompt", msgs[1])
	}
	if !lp.HasPendingWakeSignal() {
		t.Fatal("pending wake signal was drained by AppendUserMessage")
	}
}

func TestPendingSignalDuringToolExecutionDrainsAfterToolResult(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []map[string]any
		count    int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		n := count
		count++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 0:
			fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"queue_signal","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		case 1:
			fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	prov := &catalog.Provider{
		ID: "test",
		Transport: catalog.Transport{
			BaseURL: server.URL + "/v1",
		},
		Models: map[string]*catalog.Model{
			"model-a": {ID: "model-a"},
		},
	}
	client := provider.New(prov, prov.Models["model-a"], "")
	registry := tool.NewRegistry()
	var lp *Loop
	registry.Register(queueSignalTool{queue: func() {
		lp.AddPendingSignal(PendingSignal{
			Payload: "async <event>",
			Wake:    true,
			BackgroundProcess: &BackgroundProcessDisplay{
				ID:       "bg-1",
				Command:  "printf async",
				Reason:   "completed",
				ExitCode: 0,
				Output:   "async",
			},
		})
	}})
	lp = New(provider.NewAdapter(client), registry, "system")
	events := make(chan Event, 16)
	lp.SetEvents(events)

	got, err := lp.Run(context.Background(), "start")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "done" {
		t.Fatalf("Run returned %q, want done", got)
	}

	msgs := lp.Messages()
	if len(msgs) != 6 {
		t.Fatalf("messages len = %d, want 6: %#v", len(msgs), msgs)
	}
	if msgs[2].Role != message.RoleAssistant || len(msgs[2].ToolCalls) != 1 {
		t.Fatalf("messages[2] = %#v, want assistant tool call", msgs[2])
	}
	if msgs[3].Role != message.RoleTool || msgs[3].ToolCallID != "call_1" {
		t.Fatalf("messages[3] = %#v, want tool result for call_1", msgs[3])
	}
	if msgs[4].Role != message.RoleUser {
		t.Fatalf("messages[4] role = %q, want user system signal", msgs[4].Role)
	}
	wantSignal := `<system-signal>async &lt;event&gt;</system-signal>`
	if gotSignal := msgs[4].TextContent(); gotSignal != wantSignal {
		t.Fatalf("messages[4] = %q, want %q", gotSignal, wantSignal)
	}
	if msgs[4].InternalKind != systemSignalInternalKind {
		t.Fatalf("messages[4] InternalKind = %q, want %q", msgs[4].InternalKind, systemSignalInternalKind)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests len = %d, want 2", len(requests))
	}
	wireMessages, ok := requests[1]["messages"].([]any)
	if !ok {
		t.Fatalf("second request messages = %#v", requests[1]["messages"])
	}
	if len(wireMessages) < 5 {
		t.Fatalf("second request messages len = %d, want at least 5", len(wireMessages))
	}
	assistantMsg := wireMessages[len(wireMessages)-3].(map[string]any)
	toolMsg := wireMessages[len(wireMessages)-2].(map[string]any)
	signalMsg := wireMessages[len(wireMessages)-1].(map[string]any)
	if assistantMsg["role"] != "assistant" || assistantMsg["tool_calls"] == nil {
		t.Fatalf("wire assistant message = %#v, want tool call", assistantMsg)
	}
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" {
		t.Fatalf("wire tool message = %#v, want tool result", toolMsg)
	}
	if signalMsg["role"] != "user" || signalMsg["content"] != wantSignal {
		t.Fatalf("wire signal message = %#v, want pending signal after tool result", signalMsg)
	}

	var (
		toolEndIndex = -1
		bgIndex      = -1
		eventIndex   int
	)
	for {
		select {
		case ev := <-events:
			if ev.Kind == ToolCallEnd {
				toolEndIndex = eventIndex
			}
			if ev.Kind == BackgroundProcessComplete {
				bgIndex = eventIndex
			}
			eventIndex++
		default:
			if toolEndIndex < 0 || bgIndex < 0 || bgIndex <= toolEndIndex {
				t.Fatalf("event order toolEnd=%d background=%d", toolEndIndex, bgIndex)
			}
			return
		}
	}
}

func TestWakeSignalDuringTextModelCallStartsNextModelRequest(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []map[string]any
		count    int
		lp       *Loop
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		n := count
		count++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 0:
			lp.AddPendingSignal(PendingSignal{Payload: "background complete", Wake: true})
			fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":"initial"},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		case 1:
			fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":"after signal"},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	prov := &catalog.Provider{
		ID: "test",
		Transport: catalog.Transport{
			BaseURL: server.URL + "/v1",
		},
		Models: map[string]*catalog.Model{
			"model-a": {ID: "model-a"},
		},
	}
	client := provider.New(prov, prov.Models["model-a"], "")
	lp = New(provider.NewAdapter(client), tool.NewRegistry(), "system")

	got, err := lp.Run(context.Background(), "start")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "after signal" {
		t.Fatalf("Run returned %q, want after signal", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests len = %d, want 2", len(requests))
	}
	wireMessages, ok := requests[1]["messages"].([]any)
	if !ok || len(wireMessages) == 0 {
		t.Fatalf("second request messages = %#v", requests[1]["messages"])
	}
	last := wireMessages[len(wireMessages)-1].(map[string]any)
	if last["role"] != "user" || last["content"] != `<system-signal>background complete</system-signal>` {
		t.Fatalf("second request last message = %#v, want wake signal", last)
	}
}

func TestStageableWriteRequiresStringContent(t *testing.T) {
	writer := tool.NewWriteFile(nil, config.ToolsConfig{})
	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{
			name: "missing",
			params: map[string]any{
				"path": "file.txt",
			},
		},
		{
			name: "non-string",
			params: map[string]any{
				"path":    "file.txt",
				"content": 12,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := json.Marshal(tc.params)
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}
			err = writer.ValidateStaged(context.Background(), args)
			if err == nil || err.Error() != "write_file: content must be a string" {
				t.Fatalf("ValidateStaged error = %v, want content type error", err)
			}
		})
	}
}

type queueSignalTool struct {
	queue func()
}

func (queueSignalTool) Name() string { return "queue_signal" }

func (queueSignalTool) Description() string { return "queue a signal" }

func (queueSignalTool) ParametersSchema() map[string]any {
	return map[string]any{"type": "object"}
}

func (t queueSignalTool) Execute(context.Context, map[string]any) (string, error) {
	t.queue()
	return "tool result", nil
}

func TestStageableWriteAllowsEmptyContent(t *testing.T) {
	writer := tool.NewWriteFile(nil, config.ToolsConfig{})
	args, err := json.Marshal(map[string]any{
		"path":    "file.txt",
		"content": "",
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	if err := writer.ValidateStaged(context.Background(), args); err != nil {
		t.Fatalf("ValidateStaged returned error for empty content: %v", err)
	}
}

func TestConsumeStreamDoesNotMergeToolCallsWhenProviderOmitsIndices(t *testing.T) {
	stream := newParsedTestStream(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"A.md\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"B.md\",\"content\":\"hi\"}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	loop := &Loop{}

	msg, cancelled, err := loop.consumeStream(context.Background(), stream)
	if err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	if cancelled {
		t.Fatal("did not expect cancellation")
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %#v", len(msg.ToolCalls), msg.ToolCalls)
	}
	if got := msg.ToolCalls[0]; got.ID != "call_1" || got.Function.Name != "read_file" || got.Function.Arguments != `{"path":"A.md"}` {
		t.Fatalf("first tool call = %#v", got)
	}
	if got := msg.ToolCalls[1]; got.ID != "call_2" || got.Function.Name != "write_file" || got.Function.Arguments != `{"path":"B.md","content":"hi"}` {
		t.Fatalf("second tool call = %#v", got)
	}
}

func TestConsumeStreamUsesPositionForAnonymousToolCallContinuations(t *testing.T) {
	stream := newParsedTestStream(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\""}},{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"A.md\"}"}},{"function":{"arguments":"B.md\",\"content\":\"hi\"}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	loop := &Loop{}

	msg, cancelled, err := loop.consumeStream(context.Background(), stream)
	if err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	if cancelled {
		t.Fatal("did not expect cancellation")
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %#v", len(msg.ToolCalls), msg.ToolCalls)
	}
	if got := msg.ToolCalls[0]; got.ID != "call_1" || got.Function.Name != "read_file" || got.Function.Arguments != `{"path":"A.md"}` {
		t.Fatalf("first tool call = %#v", got)
	}
	if got := msg.ToolCalls[1]; got.ID != "call_2" || got.Function.Name != "write_file" || got.Function.Arguments != `{"path":"B.md","content":"hi"}` {
		t.Fatalf("second tool call = %#v", got)
	}
}

func TestConsumeStreamUsesLastToolForSingletonAnonymousContinuation(t *testing.T) {
	stream := newParsedTestStream(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"A.md\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"B.md\",\"content\":\""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"hi\"}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	loop := &Loop{}

	msg, cancelled, err := loop.consumeStream(context.Background(), stream)
	if err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	if cancelled {
		t.Fatal("did not expect cancellation")
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %#v", len(msg.ToolCalls), msg.ToolCalls)
	}
	if got := msg.ToolCalls[0]; got.ID != "call_1" || got.Function.Name != "read_file" || got.Function.Arguments != `{"path":"A.md"}` {
		t.Fatalf("first tool call = %#v", got)
	}
	if got := msg.ToolCalls[1]; got.ID != "call_2" || got.Function.Name != "write_file" || got.Function.Arguments != `{"path":"B.md","content":"hi"}` {
		t.Fatalf("second tool call = %#v", got)
	}
}

func TestConsumeStreamCapturesMessageAndToolExtras(t *testing.T) {
	stream := newParsedTestStream(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"think "}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_content":"more","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"},"extra_content":{"google":{"thought_signature":"sig"}}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	client := provider.New(&catalog.Provider{ID: "test"}, &catalog.Model{ID: "model-a"}, "")
	loop := &Loop{client: provider.NewAdapter(client), trace: io.Discard}

	msg, cancelled, err := loop.consumeStream(context.Background(), stream)
	if err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	if cancelled {
		t.Fatal("did not expect cancellation")
	}
	if msg.Source != (coremodel.ModelRef{Provider: "test", Model: "model-a"}) {
		t.Fatalf("source = %#v", msg.Source)
	}
	if got := string(msg.Extra["reasoning_content"]); got != `"think more"` {
		t.Fatalf("reasoning_content = %s", got)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool calls = %#v", msg.ToolCalls)
	}
	if got := string(msg.ToolCalls[0].Extra["extra_content"]); got != `{"google":{"thought_signature":"sig"}}` {
		t.Fatalf("tool extra = %s", got)
	}
}

func TestConsumeStreamRemapsToolExtraForOmittedIndexContinuation(t *testing.T) {
	stream := newParsedTestStream(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"A.md\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"B.md\",\"content\":\""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"hi\"}"},"extra_content":{"google":{"thought_signature":"sig2"}}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	loop := &Loop{trace: io.Discard}

	msg, cancelled, err := loop.consumeStream(context.Background(), stream)
	if err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	if cancelled {
		t.Fatal("did not expect cancellation")
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %#v", len(msg.ToolCalls), msg.ToolCalls)
	}
	if _, ok := msg.ToolCalls[0].Extra["extra_content"]; ok {
		t.Fatalf("first tool call got continuation extra: %#v", msg.ToolCalls[0].Extra)
	}
	if got := string(msg.ToolCalls[1].Extra["extra_content"]); got != `{"google":{"thought_signature":"sig2"}}` {
		t.Fatalf("second tool extra = %s", got)
	}
}

func TestConsumeStreamAcceptsExtraOnlyAssistantMessage(t *testing.T) {
	stream := newParsedTestStream(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","reasoning_details":[{"type":"reasoning.text","text":"hidden"}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	loop := &Loop{trace: io.Discard}

	msg, cancelled, err := loop.consumeStream(context.Background(), stream)
	if err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	if cancelled {
		t.Fatal("did not expect cancellation")
	}
	if len(msg.Content) != 0 || len(msg.ToolCalls) != 0 {
		t.Fatalf("visible payload = content %#v tool calls %#v, want none", msg.Content, msg.ToolCalls)
	}
	if got := string(msg.Extra["reasoning_details"]); got != `[{"type":"reasoning.text","text":"hidden"}]` {
		t.Fatalf("reasoning_details = %s", got)
	}
}

func TestLoadHistoryAcceptsOldMessageJSONWithEmptySource(t *testing.T) {
	var old message.Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"old text","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]}`), &old); err != nil {
		t.Fatalf("unmarshal old message: %v", err)
	}
	if old.Source != (coremodel.ModelRef{}) {
		t.Fatalf("old message source = %#v, want empty", old.Source)
	}

	lp := New(nil, nil, "system")
	lp.LoadHistory([][]message.Message{{message.NewText(message.RoleUser, "hello"), old}})

	history := lp.Messages()
	if len(history) != 3 {
		t.Fatalf("history length = %d, want 3", len(history))
	}
	got := history[2]
	if got.TextContent() != "old text" {
		t.Fatalf("assistant text = %q, want old text", got.TextContent())
	}
	if got.Source != (coremodel.ModelRef{}) {
		t.Fatalf("loaded source = %#v, want empty", got.Source)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool calls not loaded canonically: %#v", got.ToolCalls)
	}
}

func TestPersistMessageWritesCanonicalShapeWithSource(t *testing.T) {
	client := provider.New(&catalog.Provider{ID: "test-provider"}, &catalog.Model{ID: "test-model"}, "")
	lp := New(provider.NewAdapter(client), nil, "system")
	store := &fakeStore{turn: 4}
	lp.SetStore(store)

	stream := newParsedTestStream(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"ok"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	msg, cancelled, err := lp.consumeStream(context.Background(), stream)
	if err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	if cancelled {
		t.Fatal("did not expect cancellation")
	}

	lp.persistMessage(4, msg)
	if len(store.messages) != 1 {
		t.Fatalf("persisted messages = %d, want 1", len(store.messages))
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(store.messages[0], &raw); err != nil {
		t.Fatalf("unmarshal persisted message: %v", err)
	}
	if string(raw["role"]) != `"assistant"` {
		t.Fatalf("role = %s, want assistant", raw["role"])
	}
	if string(raw["content"]) != `"ok"` {
		t.Fatalf("content = %s, want ok", raw["content"])
	}
	if string(raw["_lightcode_source"]) != `"test-provider/test-model"` {
		t.Fatalf("_lightcode_source = %s, want test-provider/test-model", raw["_lightcode_source"])
	}
}

func BenchmarkConsumeStream(b *testing.B) {
	fixture := largeSSEFixture()
	loop := &Loop{trace: io.Discard}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stream := newParsedTestStream(fixture)
		msg, cancelled, err := loop.consumeStream(context.Background(), stream)
		if err != nil {
			b.Fatalf("consume stream: %v", err)
		}
		if cancelled {
			b.Fatal("did not expect cancellation")
		}
		if got, want := len(msg.ToolCalls), 40; got != want {
			b.Fatalf("tool calls = %d, want %d", got, want)
		}
	}
}

func largeSSEFixture() string {
	var b strings.Builder
	b.WriteString(`data: {"choices":[{"delta":{"role":"assistant","content":"start"}}]}`)
	b.WriteString("\n\n")
	for i := 0; i < 40; i++ {
		suffix := string([]rune{rune('a' + i/26), rune('a' + i%26)})
		b.WriteString(`data: {"choices":[{"delta":{"content":" chunk","tool_calls":[{"id":"call_`)
		b.WriteString(suffix)
		b.WriteString(`","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"file_`)
		b.WriteString(suffix)
		b.WriteString(`.go\"}"}}]}}]}`)
		b.WriteString("\n\n")
	}
	for i := 0; i < 120; i++ {
		b.WriteString(`data: {"choices":[{"delta":{"content":" more text"}}]}`)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}
