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
