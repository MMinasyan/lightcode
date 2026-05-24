package loop

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/message"
	"github.com/MMinasyan/lightcode/internal/provider"
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

func TestAppendSignalPayloadWrapsRawPayload(t *testing.T) {
	lp := New(nil, nil, "system")
	lp.AppendSignalPayload(`raw <payload> & data`)

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
