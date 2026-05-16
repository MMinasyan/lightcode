package compact

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/message"
)

func TestPruneCanonicalToolOutputsKeepsLastRead(t *testing.T) {
	messages := []message.Message{
		{
			Role: message.RoleAssistant,
			ToolCalls: []message.ToolCall{
				{ID: "call_read_old", Type: "function", Function: message.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`}},
				{ID: "call_shell", Type: "function", Function: message.FunctionCall{Name: "run_command", Arguments: `{"command":"ls"}`}},
			},
		},
		toolMessage("call_read_old", "read_file", "old read output"),
		toolMessage("call_shell", "run_command", "shell output"),
		{
			Role: message.RoleAssistant,
			ToolCalls: []message.ToolCall{
				{ID: "call_read_new", Type: "function", Function: message.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`}},
			},
		},
		toolMessage("call_read_new", "read_file", "new read output"),
	}

	pruned := Prune(messages)
	if got := pruned[1].TextContent(); got != placeholder {
		t.Fatalf("old read output = %q, want placeholder", got)
	}
	if got := pruned[2].TextContent(); got != placeholder {
		t.Fatalf("shell output = %q, want placeholder", got)
	}
	if got := pruned[4].TextContent(); got != "new read output" {
		t.Fatalf("new read output = %q, want preserved", got)
	}
	if pruned[4].ToolCallID != "call_read_new" || pruned[4].Name != "read_file" {
		t.Fatalf("tool metadata not preserved: %#v", pruned[4])
	}
}

func TestSerializeMessagesUsesVisibleCanonicalContentOnly(t *testing.T) {
	msgs := []message.Message{
		message.NewText(message.RoleUser, "rename foo.txt"),
		{
			Role:    message.RoleAssistant,
			Content: []message.ContentPart{{Type: message.ContentPartText, Text: "I will inspect it."}},
			ToolCalls: []message.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: message.FunctionCall{Name: "read_file", Arguments: `{"path":"foo.txt"}`},
				Extra:    message.Extra{"extra_content": json.RawMessage(`{"secret":"sig"}`)},
			}},
			Extra: message.Extra{"reasoning_content": json.RawMessage(`"hidden thought"`)},
		},
		toolMessage("call_1", "read_file", "file contents"),
	}

	out := serializeMessages(msgs)
	for _, want := range []string{
		"User: rename foo.txt",
		"Assistant: I will inspect it.",
		`Assistant [tool_call]: read_file({"path":"foo.txt"})`,
		"Tool [read_file]: file contents",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("serialized messages missing %q in:\n%s", want, out)
		}
	}
	for _, hidden := range []string{"hidden thought", "reasoning_content", "extra_content", "secret"} {
		if strings.Contains(out, hidden) {
			t.Fatalf("serialized messages leaked %q in:\n%s", hidden, out)
		}
	}
}

func TestCountTokensIgnoresHiddenMetadata(t *testing.T) {
	base := []message.Message{
		message.NewText(message.RoleUser, "hello"),
		{
			Role: message.RoleAssistant,
			ToolCalls: []message.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: message.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`},
			}},
		},
	}
	withExtra := []message.Message{
		base[0],
		base[1],
	}
	withExtra[1].Extra = message.Extra{"reasoning_content": json.RawMessage(`"this hidden metadata is intentionally not counted"`)}
	withExtra[1].ToolCalls[0].Extra = message.Extra{"extra_content": json.RawMessage(`{"signature":"hidden"}`)}

	if got, want := CountTokens(withExtra), CountTokens(base); got != want {
		t.Fatalf("CountTokens with hidden metadata = %d, want %d", got, want)
	}
}

func toolMessage(id, name, content string) message.Message {
	msg := message.NewText(message.RoleTool, content)
	msg.ToolCallID = id
	msg.Name = name
	return msg
}
