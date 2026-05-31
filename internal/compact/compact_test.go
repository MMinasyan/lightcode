package compact

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/coremodel"
	"github.com/MMinasyan/lightcode/internal/message"
	"github.com/MMinasyan/lightcode/internal/modelclient"
)

type fakeSummarizer struct {
	ref     coremodel.ModelRef
	content string
}

func (f fakeSummarizer) Chat(context.Context, modelclient.ChatRequest) (modelclient.ChatResponse, error) {
	return modelclient.ChatResponse{Content: f.content, HasChoice: true}, nil
}

func (f fakeSummarizer) Model() string {
	return f.ref.Model
}

func (f fakeSummarizer) ModelRef() coremodel.ModelRef {
	return f.ref
}

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

func TestSummarizeIterativeResultHasSummarizerRef(t *testing.T) {
	client := fakeSummarizer{
		ref:     coremodel.ModelRef{Provider: "test-provider", Model: "summarizer-model"},
		content: "summary",
	}

	result, err := Run(context.Background(), []message.Message{message.NewText(message.RoleUser, "please summarize")}, Config{
		SummarizerClient: client,
		ContextWindow:    1,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "summary" {
		t.Fatalf("Summary = %q, want summary", result.Summary)
	}
	if got, want := result.SummarizerRef, (coremodel.ModelRef{Provider: "test-provider", Model: "summarizer-model"}); got != want {
		t.Fatalf("SummarizerRef = %#v, want %#v", got, want)
	}
}

func BenchmarkPruneCanonicalToolOutputs(b *testing.B) {
	messages := make([]message.Message, 0, 300)
	for i := 0; i < 100; i++ {
		readID := fmt.Sprintf("call_read_%d", i)
		runID := fmt.Sprintf("call_run_%d", i)
		path := fmt.Sprintf("file_%d.txt", i%10)
		messages = append(messages,
			message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{
				{ID: readID, Type: "function", Function: message.FunctionCall{Name: "read_file", Arguments: fmt.Sprintf(`{"path":"%s"}`, path)}},
				{ID: runID, Type: "function", Function: message.FunctionCall{Name: "run_command", Arguments: `{"command":"go test ./..."}`}},
			}},
			toolMessage(readID, "read_file", strings.Repeat("read output ", 200)),
			toolMessage(runID, "run_command", strings.Repeat("command output ", 200)),
		)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pruned := Prune(messages)
		if len(pruned) != len(messages) {
			b.Fatalf("pruned len = %d, want %d", len(pruned), len(messages))
		}
	}
}

func toolMessage(id, name, content string) message.Message {
	msg := message.NewText(message.RoleTool, content)
	msg.ToolCallID = id
	msg.Name = name
	return msg
}
