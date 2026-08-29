package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/process"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

func TestSessionMessagesDoesNotRecoverCompleteActiveTurn(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1")
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	turn := a.store.BeginTurn()
	if turn == 0 {
		t.Fatal("BeginTurn returned 0")
	}
	msgs := []message.Message{
		message.NewText(message.RoleUser, "active prompt"),
		{
			Role: message.RoleAssistant,
			ToolCalls: []message.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: message.FunctionCall{
					Name:      "read_file",
					Arguments: `{"path":"x.txt"}`,
				},
			}},
		},
		toolResult("call_1", "read_file", "ok"),
	}
	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := a.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	display := a.SessionMessages()
	for _, m := range display {
		if m.Type == "user" && m.Content == "active prompt" {
			t.Fatalf("SessionMessages returned active incomplete turn: %#v", display)
		}
	}
	completePath := filepath.Join(a.store.Dir(), "turns", "1", "complete")
	if _, err := os.Stat(completePath); !os.IsNotExist(err) {
		t.Fatalf("SessionMessages mutated active turn complete marker, stat err = %v", err)
	}
}

func TestSessionMessagesReadOnlyAfterCompactionBoundaryDoesNotRecoverActiveTurn(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1")
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	turn1 := a.store.BeginTurn()
	completeMsg := message.NewText(message.RoleUser, "before compaction")
	data, err := json.Marshal(completeMsg)
	if err != nil {
		t.Fatalf("marshal complete: %v", err)
	}
	if err := a.store.AppendMessage(turn1, data); err != nil {
		t.Fatalf("AppendMessage complete: %v", err)
	}
	if err := a.store.MarkTurnComplete(turn1); err != nil {
		t.Fatalf("MarkTurnComplete: %v", err)
	}
	if err := a.store.SaveCompaction(snapshot.CompactionRecord{BoundaryTurn: turn1, Summary: "summary"}); err != nil {
		t.Fatalf("SaveCompaction: %v", err)
	}

	turn2 := a.store.BeginTurn()
	msgs := []message.Message{
		message.NewText(message.RoleUser, "active after compaction"),
		{
			Role: message.RoleAssistant,
			ToolCalls: []message.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: message.FunctionCall{
					Name:      "read_file",
					Arguments: `{"path":"x.txt"}`,
				},
			}},
		},
		toolResult("call_1", "read_file", "ok"),
	}
	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal active: %v", err)
		}
		if err := a.store.AppendMessage(turn2, data); err != nil {
			t.Fatalf("AppendMessage active: %v", err)
		}
	}

	display := a.SessionMessages()
	for _, m := range display {
		if m.Type == "user" && m.Content == "active after compaction" {
			t.Fatalf("SessionMessages returned active incomplete post-compaction turn: %#v", display)
		}
	}
	completePath := filepath.Join(a.store.Dir(), "turns", "2", "complete")
	if _, err := os.Stat(completePath); !os.IsNotExist(err) {
		t.Fatalf("SessionMessages mutated active post-compaction turn complete marker, stat err = %v", err)
	}
}

func TestSessionMessagesForReadOnlyDoesNotRecoverIncompleteOtherSession(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1")
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	parentID := a.SessionCurrent().ID
	childStore, err := snapshot.NewForSessionsRoot(a.store.Root(), "", "")
	if err != nil {
		t.Fatalf("NewForSessionsRoot: %v", err)
	}
	if err := childStore.BeginChildSession(a.ProjectRoot(), parentID); err != nil {
		t.Fatalf("BeginChildSession: %v", err)
	}
	childID := childStore.SessionID()
	turn := childStore.BeginTurn()
	msgs := []message.Message{
		message.NewText(message.RoleUser, "incomplete child prompt"),
		{
			Role: message.RoleAssistant,
			ToolCalls: []message.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: message.FunctionCall{
					Name:      "read_file",
					Arguments: `{"path":"child.txt"}`,
				},
			}},
		},
		toolResult("call_1", "read_file", "ok"),
	}
	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal child active: %v", err)
		}
		if err := childStore.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage child active: %v", err)
		}
	}

	display, err := a.SessionMessagesFor(childID)
	if err != nil {
		t.Fatalf("SessionMessagesFor child: %v", err)
	}
	for _, m := range display {
		if m.Type == "user" && m.Content == "incomplete child prompt" {
			t.Fatalf("SessionMessagesFor returned incomplete child turn: %#v", display)
		}
	}
	completePath := filepath.Join(childStore.Dir(), "turns", "1", "complete")
	if _, err := os.Stat(completePath); !os.IsNotExist(err) {
		t.Fatalf("SessionMessagesFor mutated child incomplete turn complete marker, stat err = %v", err)
	}
	if got := a.SessionCurrent().ID; got != parentID {
		t.Fatalf("SessionMessagesFor switched current session to %q, want %q", got, parentID)
	}
}

func TestSessionMessagesRendersCanonicalMessagesWithoutExtra(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession returned error: %v", err)
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
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	output := "final <output> & marker"
	payload := backgroundTerminalPayload(process.ExitEvent{
		ID:       "bg-abc",
		Command:  `printf "a < b"`,
		Reason:   process.ExitReasonError,
		ExitCode: 7,
	}, output)
	msg := loop.NewSystemSignalMessage(payload)
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

func TestSessionMessagesRendersGenericSystemSignalWithPrefix(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()

	cases := []struct {
		payload string
		want    string
	}{
		{"Request interrupted by user", "System: Request interrupted by user"},
		{`Model switched to openai/gpt-5`, "System: Model switched to openai/gpt-5"},
		{"LSP \"gopls\" is now available\nfor *.go files", "System: LSP \"gopls\" is now available for *.go files"},
		{"a <special> & b", "System: a <special> & b"},
	}
	for _, c := range cases {
		msg := loop.NewSystemSignalMessage(c.payload)
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := a.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete: %v", err)
	}

	display := a.SessionMessages()
	if len(display) != len(cases) {
		t.Fatalf("display len = %d, want %d: %#v", len(display), len(cases), display)
	}
	for i, c := range cases {
		got := display[i]
		if got.Type != "system" || got.Content != c.want {
			t.Fatalf("case %d: display = %#v, want type=system content=%q", i, got, c.want)
		}
	}
}

func TestSessionMessagesRendersLiteralSystemSignalUserText(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	backgroundLiteral := loop.SystemSignal(backgroundTerminalPayload(process.ExitEvent{
		ID:       "bg-abc",
		Command:  "printf done",
		Reason:   process.ExitReasonCompleted,
		ExitCode: 0,
	}, "done"))
	contents := []string{
		`<system-signal>Request interrupted by user</system-signal>`,
		backgroundLiteral,
	}
	for _, content := range contents {
		msg := message.NewText(message.RoleUser, content)
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := a.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete: %v", err)
	}

	display := a.SessionMessages()
	if len(display) != len(contents) {
		t.Fatalf("display messages len = %d, want %d: %#v", len(display), len(contents), display)
	}
	for i, content := range contents {
		if display[i].Type != "user" || display[i].Content != content {
			t.Fatalf("literal system-signal display %d = %#v", i, display[i])
		}
	}
}

func TestSessionMessagesUsesPersistedToolResultErrorMarker(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	result := toolResult("call_1", "run_command", "plain failure output")
	loop.MarkToolResultError(&result)
	msgs := []message.Message{
		message.NewText(message.RoleUser, "run it"),
		{
			Role:      message.RoleAssistant,
			ToolCalls: []message.ToolCall{{ID: "call_1", Type: "function", Function: message.FunctionCall{Name: "run_command", Arguments: `{"command":"false"}`}}},
		},
		result,
	}
	for _, m := range msgs {
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := a.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete: %v", err)
	}

	display := a.SessionMessages()
	if len(display) != 2 {
		t.Fatalf("display messages len = %d, want 2: %#v", len(display), display)
	}
	got := display[1]
	if got.Type != "tool" || got.ID != "call_1" || !got.Done || got.Success || got.Result != "plain failure output" {
		t.Fatalf("tool error display = %#v", got)
	}
}

func TestSessionMessagesRendersLiteralStagedFlushUserText(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	turn := a.store.BeginTurn()
	contents := []string{
		`<staged-flush>{"results":[]}</staged-flush>`,
		`<staged-flush>not valid json</staged-flush>`,
	}
	for _, content := range contents {
		msg := message.NewText(message.RoleUser, content)
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := a.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete: %v", err)
	}

	display := a.SessionMessages()
	if len(display) != len(contents) {
		t.Fatalf("display len = %d, want %d: %#v", len(display), len(contents), display)
	}
	for i, content := range contents {
		got := display[i]
		if got.Type != "user" || got.Content != content {
			t.Fatalf("literal staged-flush user row %d = %#v", i, got)
		}
	}
}

func toolResult(id, name, content string) message.Message {
	msg := message.NewText(message.RoleTool, content)
	msg.ToolCallID = id
	msg.Name = name
	return msg
}
