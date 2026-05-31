package agent

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/internal/editpreview"
	"github.com/MMinasyan/lightcode/internal/loop"
	"github.com/MMinasyan/lightcode/internal/message"
	"github.com/MMinasyan/lightcode/internal/process"
	"github.com/MMinasyan/lightcode/internal/tool"
)

// appendStagedEditTurn persists one turn that stages an edit_file call (RoleTool
// "Staged.") and then records a <staged-flush> wrapper carrying the real result,
// mirroring what the loop persists on auto-flush. wrapper is built from results.
func appendStagedEditTurn(t *testing.T, a *Agent, editArgs string, results []tool.BatchResult) {
	t.Helper()
	turn := a.store.BeginTurn()
	wrapper, ok := loop.NewStagedFlushMessage(results)
	if !ok {
		t.Fatal("NewStagedFlushMessage returned false")
	}
	msgs := []message.Message{
		message.NewText(message.RoleUser, "do edit"),
		{
			Role:      message.RoleAssistant,
			ToolCalls: []message.ToolCall{{ID: "call_1", Type: "function", Function: message.FunctionCall{Name: "edit_file", Arguments: editArgs}}},
		},
		toolResult("call_1", "edit_file", "Staged."),
		wrapper,
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
}

func appendLegacyUnmarkedStagedEditTurn(t *testing.T, a *Agent, editArgs string, results []tool.BatchResult) {
	t.Helper()
	turn := a.store.BeginTurn()
	msgs := []message.Message{
		message.NewText(message.RoleUser, "do edit"),
		{
			Role:      message.RoleAssistant,
			ToolCalls: []message.ToolCall{{ID: "call_1", Type: "function", Function: message.FunctionCall{Name: "edit_file", Arguments: editArgs}}},
		},
		toolResult("call_1", "edit_file", "Staged."),
		message.NewText(message.RoleUser, loop.BuildStagedFlush(results)),
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
}

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
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
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
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
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
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
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

func TestSessionMessagesTreatsLegacyExecutePendingFailureSummariesAsErrors(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	msgs := []message.Message{
		message.NewText(message.RoleUser, "apply"),
		{
			Role:      message.RoleAssistant,
			ToolCalls: []message.ToolCall{{ID: "call_1", Type: "function", Function: message.FunctionCall{Name: "execute_pending", Arguments: `{}`}}},
		},
		toolResult("call_1", "execute_pending", "Applied 1 staged edits; 1 failed."),
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
	if got.Type != "tool" || got.Success {
		t.Fatalf("legacy execute_pending failure display = %#v", got)
	}
}

func TestSessionMessagesAppliesStagedFlushOverlayToToolStub(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	args := `{"path":"file.txt","old_string":"a","new_string":"b"}`
	realResult := "Edited file.txt (1 replacement, lines 1-2)."
	wantMeta := editpreview.MetadataFromArgs(args, realResult)
	if wantMeta == nil {
		t.Fatal("test setup: expected non-nil edit_preview metadata for this args+result")
	}
	wantMeta["source"] = "persisted"
	appendStagedEditTurn(t, a, args, []tool.BatchResult{{ToolCallID: "call_1", Result: realResult, Metadata: wantMeta}})

	display := a.SessionMessages()
	// Expect exactly: user "do edit", then the tool row with the REAL result —
	// the <staged-flush> wrapper produces no row.
	if len(display) != 2 {
		t.Fatalf("display len = %d, want 2: %#v", len(display), display)
	}
	if display[0].Type != "user" || display[0].Content != "do edit" {
		t.Fatalf("display[0] = %#v", display[0])
	}
	tr := display[1]
	if tr.Type != "tool" || tr.ID != "call_1" || !tr.Done || !tr.Success {
		t.Fatalf("tool row basics = %#v", tr)
	}
	if tr.Result != realResult {
		t.Fatalf("tool row result = %q, want real result (not \"Staged.\")", tr.Result)
	}
	if tr.Metadata["source"] != "persisted" {
		t.Fatalf("tool row metadata source = %#v, want persisted wrapper metadata", tr.Metadata["source"])
	}
	wantPreview, ok := editpreview.FromMetadata(wantMeta)
	if !ok {
		t.Fatal("test setup: expected preview from metadata")
	}
	gotPreview, ok := editpreview.FromMetadata(tr.Metadata)
	if !ok {
		t.Fatalf("tool row metadata did not contain edit preview: %#v", tr.Metadata)
	}
	if !reflect.DeepEqual(gotPreview, wantPreview) {
		t.Fatalf("tool row preview = %#v, want %#v", gotPreview, wantPreview)
	}
	// No row should be a fake user bubble from the wrapper.
	for _, m := range display {
		if m.Type == "user" && m.Content != "do edit" {
			t.Fatalf("unexpected user row from wrapper: %#v", m)
		}
	}
}

func TestSessionMessagesAppliesLegacyUnmarkedStagedFlushOverlay(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	args := `{"path":"file.txt","old_string":"a","new_string":"b"}`
	realResult := "Edited file.txt (1 replacement, lines 1-2)."
	appendLegacyUnmarkedStagedEditTurn(t, a, args, []tool.BatchResult{{ToolCallID: "call_1", Result: realResult}})

	display := a.SessionMessages()
	if len(display) != 2 {
		t.Fatalf("display len = %d, want 2: %#v", len(display), display)
	}
	tr := display[1]
	if tr.Type != "tool" || tr.ID != "call_1" || tr.Result != realResult {
		t.Fatalf("legacy staged-flush overlay = %#v", tr)
	}
}

func TestSessionMessagesStagedFlushOrphanIDIgnored(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	// Wrapper references an unknown tool_call id; the real call_1 has no entry.
	args := `{"path":"file.txt","old_string":"a","new_string":"b"}`
	appendStagedEditTurn(t, a, args, []tool.BatchResult{{ToolCallID: "call_99", Result: "orphan"}})

	display := a.SessionMessages()
	if len(display) != 2 {
		t.Fatalf("display len = %d, want 2: %#v", len(display), display)
	}
	tr := display[1]
	// call_1 stub is untouched by the orphan entry: stays at "Staged.".
	if tr.Type != "tool" || tr.ID != "call_1" || tr.Result != "Staged." {
		t.Fatalf("orphan should not touch call_1; got %#v", tr)
	}
}

func TestSessionMessagesStagedFlushDoesNotOverlayAcrossTurns(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	turn1 := a.store.BeginTurn()
	msgs := []message.Message{
		message.NewText(message.RoleUser, "first turn"),
		{
			Role:      message.RoleAssistant,
			ToolCalls: []message.ToolCall{{ID: "call_dup", Type: "function", Function: message.FunctionCall{Name: "edit_file", Arguments: `{"path":"file.txt","old_string":"a","new_string":"b"}`}}},
		},
		toolResult("call_dup", "edit_file", "Staged."),
	}
	for _, m := range msgs {
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := a.store.AppendMessage(turn1, data); err != nil {
			t.Fatalf("AppendMessage turn1: %v", err)
		}
	}
	if err := a.store.MarkTurnComplete(turn1); err != nil {
		t.Fatalf("MarkTurnComplete turn1: %v", err)
	}

	turn2 := a.store.BeginTurn()
	wrapper, ok := loop.NewStagedFlushMessage([]tool.BatchResult{{ToolCallID: "call_dup", Result: "late result"}})
	if !ok {
		t.Fatal("NewStagedFlushMessage returned false")
	}
	data, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("marshal wrapper: %v", err)
	}
	if err := a.store.AppendMessage(turn2, data); err != nil {
		t.Fatalf("AppendMessage turn2: %v", err)
	}
	if err := a.store.MarkTurnComplete(turn2); err != nil {
		t.Fatalf("MarkTurnComplete turn2: %v", err)
	}

	display := a.SessionMessages()
	if len(display) != 2 {
		t.Fatalf("display len = %d, want 2: %#v", len(display), display)
	}
	tr := display[1]
	if tr.Type != "tool" || tr.ID != "call_dup" || tr.Result != "Staged." {
		t.Fatalf("later orphan wrapper must not rewrite earlier turn tool row; got %#v", tr)
	}
}

func TestSessionMessagesMalformedStagedFlushLeavesStubUntouched(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	turn := a.store.BeginTurn()
	wrapper, ok := loop.NewStagedFlushMessage([]tool.BatchResult{{ToolCallID: "call_1", Result: "unused"}})
	if !ok {
		t.Fatal("NewStagedFlushMessage returned false")
	}
	wrapper.Content[0].Text = "<staged-flush>not valid json</staged-flush>"
	msgs := []message.Message{
		message.NewText(message.RoleUser, "do edit"),
		{
			Role:      message.RoleAssistant,
			ToolCalls: []message.ToolCall{{ID: "call_1", Type: "function", Function: message.FunctionCall{Name: "edit_file", Arguments: `{"path":"file.txt","old_string":"a","new_string":"b"}`}}},
		},
		toolResult("call_1", "edit_file", "Staged."),
		wrapper,
	}
	for _, m := range msgs {
		data, _ := json.Marshal(m)
		if err := a.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete: %v", err)
	}

	display := a.SessionMessages()
	// Malformed wrapper: no row appended, stub stays at "Staged.".
	if len(display) != 2 {
		t.Fatalf("display len = %d, want 2: %#v", len(display), display)
	}
	if display[1].Type != "tool" || display[1].Result != "Staged." {
		t.Fatalf("malformed wrapper should leave stub at \"Staged.\"; got %#v", display[1])
	}
}

func TestSessionMessagesRendersLiteralStagedFlushUserText(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	appendStagedEditTurn(t, a, `{"path":"file.txt","old_string":"a","new_string":"b"}`, []tool.BatchResult{{ToolCallID: "call_1", Result: "Edited file.txt (1 replacement, lines 1-2)."}})
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
	wantLen := 2 + len(contents)
	if len(display) != wantLen {
		t.Fatalf("display len = %d, want %d: %#v", len(display), wantLen, display)
	}
	for i, content := range contents {
		got := display[2+i]
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
