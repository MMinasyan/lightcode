package agent

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/loop"
	"github.com/MMinasyan/lightcode/internal/message"
	"github.com/MMinasyan/lightcode/internal/process"
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

func TestSessionMessagesRendersBackgroundProcessSystemSignal(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	output := "final <output> & marker"
	payload := backgroundTerminalPayload(process.ExitEvent{
		ID:       "bg-abc",
		Command:  `printf "a < b"`,
		Reason:   process.ExitReasonError,
		ExitCode: 7,
	}, output)
	msg := message.NewText(message.RoleUser, loop.SystemSignal(payload))
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if err := a.store.AppendMessage(turn, data); err != nil {
		t.Fatalf("AppendMessage returned error: %v", err)
	}
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete returned error: %v", err)
	}

	display := a.SessionMessages()
	if len(display) != 1 {
		t.Fatalf("display messages len = %d, want 1: %#v", len(display), display)
	}
	got := display[0]
	if got.Type != "background_process" || got.ID != "bg-abc" || !got.Done || got.Success {
		t.Fatalf("background display basics = %#v", got)
	}
	if got.Result != output || got.BackgroundProcess == nil {
		t.Fatalf("background display output/payload = %#v", got)
	}
	if got.BackgroundProcess.Command != `printf "a < b"` || got.BackgroundProcess.Reason != "error" || got.BackgroundProcess.ExitCode != 7 || got.BackgroundProcess.Output != output {
		t.Fatalf("background display payload = %#v", got.BackgroundProcess)
	}
}

func toolResult(id, name, content string) message.Message {
	msg := message.NewText(message.RoleTool, content)
	msg.ToolCallID = id
	msg.Name = name
	return msg
}

func appendStoredSignal(t *testing.T, a *Agent, turn int, payload string) {
	t.Helper()
	msg := message.NewText(message.RoleUser, loop.SystemSignal(payload))
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if err := a.store.AppendMessage(turn, data); err != nil {
		t.Fatalf("AppendMessage returned error: %v", err)
	}
}

func TestSessionMessagesRendersGenericSystemSignalWithPrefix(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	appendStoredSignal(t, a, turn, "LSP gopls became ready")
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete returned error: %v", err)
	}

	display := a.SessionMessages()
	if len(display) != 1 || display[0].Type != "system" {
		t.Fatalf("display = %#v", display)
	}
	if display[0].Content != "System: LSP gopls became ready" {
		t.Fatalf("content = %q", display[0].Content)
	}
}

func TestSessionMessagesUnescapesAndCollapsesGenericSignal(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	appendStoredSignal(t, a, turn, "warn:\n\t<thing>\n  &noise  spaced")
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete returned error: %v", err)
	}

	display := a.SessionMessages()
	if len(display) != 1 {
		t.Fatalf("display = %#v", display)
	}
	if got := display[0].Content; got != "System: warn: <thing> &noise spaced" {
		t.Fatalf("content = %q", got)
	}
}

func TestSessionMessagesInterruptedSignalGetsSystemPrefix(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	appendStoredSignal(t, a, turn, "Request interrupted by user")
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete returned error: %v", err)
	}

	display := a.SessionMessages()
	if len(display) != 1 || display[0].Content != "System: Request interrupted by user" {
		t.Fatalf("display = %#v", display)
	}
}

func TestSessionMessagesModelSwitchSignalGetsSystemPrefix(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	appendStoredSignal(t, a, turn, "Model switched to openai/gpt-4o")
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete returned error: %v", err)
	}

	display := a.SessionMessages()
	if len(display) != 1 || display[0].Content != "System: Model switched to openai/gpt-4o" {
		t.Fatalf("display = %#v", display)
	}
}
