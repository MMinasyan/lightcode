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

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/message"
	"github.com/MMinasyan/lightcode/internal/provider"
	"github.com/MMinasyan/lightcode/internal/tool"
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

func TestConsumeStreamHandlesSparseToolCallIndices(t *testing.T) {
	stream := provider.NewStream(io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"first","arguments":"{}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":2,"id":"call_2","type":"function","function":{"name":"third","arguments":"{}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))))
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
	stream := provider.NewStream(io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_omitted","type":"function","function":{"name":"read_file","arguments":"{}"}},{"index":0,"id":"call_explicit","type":"function","function":{"name":"write_file","arguments":"{}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))))
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

func TestEmitDropsTelemetryWhenChannelFull(t *testing.T) {
	ch := make(chan Event, 1)
	ch <- Event{Kind: TextDelta, Result: "already full"}
	loop := &Loop{}
	loop.SetEvents(ch)

	for i := 0; i < 10; i++ {
		loop.emit(Event{Kind: TextDelta, Result: "drop"})
	}
	if loop.droppedEvents != 10 {
		t.Fatalf("droppedEvents = %d, want 10", loop.droppedEvents)
	}

	<-ch
	loop.emit(Event{Kind: TextDelta, Result: "next"})
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

func TestQueueSignalPayloadWrapsRawPayloadWhenDrained(t *testing.T) {
	lp := New(nil, nil, "system")
	lp.QueueSignalPayload(`raw <payload> & data`)
	lp.DrainQueuedSignals()

	msgs := lp.Messages()
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want system plus signal", len(msgs))
	}
	got := msgs[1].TextContent()
	want := `<system-signal>raw &lt;payload&gt; &amp; data</system-signal>`
	if got != want {
		t.Fatalf("signal message = %q, want %q", got, want)
	}
}

func TestResetHistoryClearsQueuedSignals(t *testing.T) {
	lp := New(nil, nil, "system")
	lp.QueueSignalPayload("old session signal")
	lp.ResetHistory()
	lp.DrainQueuedSignals()

	msgs := lp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want only system after reset: %#v", len(msgs), msgs)
	}
}

func TestAppendUserMessageDrainsQueuedSignalsBeforeUserTurn(t *testing.T) {
	lp := New(nil, nil, "system")
	lp.QueueSignalPayload("idle signal")
	lp.AppendUserMessage(1, "next prompt")

	msgs := lp.Messages()
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want system, signal, user: %#v", len(msgs), msgs)
	}
	if msgs[1].Role != message.RoleUser || msgs[1].TextContent() != `<system-signal>idle signal</system-signal>` {
		t.Fatalf("messages[1] = %#v, want queued signal before user turn", msgs[1])
	}
	if msgs[2].Role != message.RoleUser || msgs[2].TextContent() != "next prompt" {
		t.Fatalf("messages[2] = %#v, want user prompt after queued signal", msgs[2])
	}
}

func TestQueuedSignalDuringToolExecutionDrainsAfterToolResult(t *testing.T) {
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
		lp.QueueSignalPayload("async <event>")
	}})
	lp = New(client, registry, "system")

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
		t.Fatalf("wire signal message = %#v, want queued signal after tool result", signalMsg)
	}
}

func TestValidateStagedWriteRequiresStringContent(t *testing.T) {
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
			err := validateStagedCall("write_file", tc.params)
			if err == nil || err.Error() != "write_file: content must be a string" {
				t.Fatalf("validateStagedCall error = %v, want content type error", err)
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

func TestValidateStagedWriteAllowsEmptyContent(t *testing.T) {
	err := validateStagedCall("write_file", map[string]any{
		"path":    "file.txt",
		"content": "",
	})
	if err != nil {
		t.Fatalf("validateStagedCall returned error for empty content: %v", err)
	}
}

func TestConsumeStreamDoesNotMergeToolCallsWhenProviderOmitsIndices(t *testing.T) {
	stream := provider.NewStream(io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"A.md\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"B.md\",\"content\":\"hi\"}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))))
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
	stream := provider.NewStream(io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\""}},{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"A.md\"}"}},{"function":{"arguments":"B.md\",\"content\":\"hi\"}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))))
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
	stream := provider.NewStream(io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"A.md\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"B.md\",\"content\":\""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"hi\"}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))))
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
	stream := provider.NewStream(io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"think "}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_content":"more","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"},"extra_content":{"google":{"thought_signature":"sig"}}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))))
	client := provider.New(&catalog.Provider{ID: "test"}, &catalog.Model{ID: "model-a"}, "")
	loop := &Loop{client: client, trace: io.Discard}

	msg, cancelled, err := loop.consumeStream(context.Background(), stream)
	if err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	if cancelled {
		t.Fatal("did not expect cancellation")
	}
	if msg.Source != (catalog.ModelRef{Provider: "test", Model: "model-a"}) {
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
	stream := provider.NewStream(io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"A.md\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"B.md\",\"content\":\""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"hi\"}"},"extra_content":{"google":{"thought_signature":"sig2"}}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))))
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
	stream := provider.NewStream(io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","reasoning_details":[{"type":"reasoning.text","text":"hidden"}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))))
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
	if old.Source != (catalog.ModelRef{}) {
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
	if got.Source != (catalog.ModelRef{}) {
		t.Fatalf("loaded source = %#v, want empty", got.Source)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool calls not loaded canonically: %#v", got.ToolCalls)
	}
}

func TestPersistMessageWritesCanonicalShapeWithSource(t *testing.T) {
	client := provider.New(&catalog.Provider{ID: "test-provider"}, &catalog.Model{ID: "test-model"}, "")
	lp := New(client, nil, "system")
	store := &fakeStore{turn: 4}
	lp.SetStore(store)

	stream := provider.NewStream(io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"ok"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))))
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
		stream := provider.NewStream(io.NopCloser(strings.NewReader(fixture)))
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
