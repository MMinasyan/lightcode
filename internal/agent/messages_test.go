package agent

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/message"
)

func TestSessionMessagesRendersCanonicalMessagesWithoutExtra(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()

	msgs := []message.Message{
		message.NewText(message.RoleUser, "inspect foo.txt"),
		{
			Role:    message.RoleAssistant,
			Content: []message.ContentPart{{Type: message.ContentPartText, Text: "I will read it."}},
			ToolCalls: []message.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: message.FunctionCall{Name: "read_file", Arguments: `{"path":"foo.txt"}`},
				Extra:    message.Extra{"extra_content": json.RawMessage(`{"signature":"hidden"}`)},
			}},
			Extra: message.Extra{"reasoning_content": json.RawMessage(`"hidden thought"`)},
		},
		toolResult("call_1", "read_file", "contents"),
	}
	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		if err := a.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage returned error: %v", err)
		}
	}
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete returned error: %v", err)
	}

	display := a.SessionMessages()
	if len(display) != 3 {
		t.Fatalf("display messages len = %d, want 3: %#v", len(display), display)
	}
	if display[0].Type != "user" || display[0].Content != "inspect foo.txt" {
		t.Fatalf("user display = %#v", display[0])
	}
	if display[1].Type != "assistant" || display[1].Content != "I will read it." {
		t.Fatalf("assistant display = %#v", display[1])
	}
	if display[2].Type != "tool" || display[2].Name != "read_file" || display[2].Result != "contents" || !display[2].Done {
		t.Fatalf("tool display = %#v", display[2])
	}
	for _, item := range display {
		if item.Content == "hidden thought" || item.Result == "hidden thought" {
			t.Fatalf("hidden metadata leaked into display: %#v", display)
		}
	}
}

func toolResult(id, name, content string) message.Message {
	msg := message.NewText(message.RoleTool, content)
	msg.ToolCallID = id
	msg.Name = name
	return msg
}
