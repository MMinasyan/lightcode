package tool

import (
	"context"
	"strings"
	"testing"
)

func TestPendingQueueStageLenAndStagedOrder(t *testing.T) {
	queue := NewPendingQueue()
	first := pendingCall("edit_file", "call-1", "a.txt")
	second := pendingCall("write_file", "call-2", "b.txt")

	if queue.Len() != 0 {
		t.Fatalf("new queue Len = %d, want 0", queue.Len())
	}

	queue.Stage(first)
	queue.Stage(second)

	if queue.Len() != 2 {
		t.Fatalf("Len after staging = %d, want 2", queue.Len())
	}
	staged := queue.Staged()
	if len(staged) != 2 {
		t.Fatalf("Staged length = %d, want 2", len(staged))
	}
	if staged[0].ToolCallID != "call-1" || staged[1].ToolCallID != "call-2" {
		t.Fatalf("staged order = %+v, want call-1 then call-2", staged)
	}
}

func TestPendingQueueStagedReturnsCopy(t *testing.T) {
	queue := NewPendingQueue()
	queue.Stage(pendingCall("edit_file", "call-1", "a.txt"))

	staged := queue.Staged()
	staged[0] = pendingCall("write_file", "mutated", "b.txt")
	staged = append(staged, pendingCall("edit_file", "extra", "c.txt"))

	again := queue.Staged()
	if queue.Len() != 1 || len(again) != 1 {
		t.Fatalf("queue mutated through Staged copy: Len=%d Staged=%+v", queue.Len(), again)
	}
	if again[0].ToolName != "edit_file" || again[0].ToolCallID != "call-1" {
		t.Fatalf("stored call = %+v, want original edit_file call-1", again[0])
	}
}

func TestPendingQueueDiscardClearsStagedCalls(t *testing.T) {
	queue := NewPendingQueue()
	queue.Stage(pendingCall("edit_file", "call-1", "a.txt"))
	queue.Stage(pendingCall("write_file", "call-2", "b.txt"))

	queue.Discard()

	if queue.Len() != 0 {
		t.Fatalf("Len after Discard = %d, want 0", queue.Len())
	}
	if staged := queue.Staged(); len(staged) != 0 {
		t.Fatalf("Staged after Discard = %+v, want empty", staged)
	}
}

func TestExecutePendingToolMetadataAndNoopExecute(t *testing.T) {
	tool := ExecutePending{}

	if tool.Name() != "execute_pending" {
		t.Fatalf("Name = %q, want execute_pending", tool.Name())
	}
	if desc := tool.Description(); !strings.Contains(desc, "Execute all staged") || !strings.Contains(desc, "discard") {
		t.Fatalf("Description = %q, want pending execution guidance", desc)
	}
	schema := tool.ParametersSchema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v, want object", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v, want map", schema["properties"])
	}
	if _, ok := props["action"]; !ok {
		t.Fatalf("schema properties = %#v, want action property", props)
	}

	result, err := tool.Execute(context.Background(), map[string]any{"action": "apply"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "No pending edits to execute." {
		t.Fatalf("Execute result = %q, want no-op message", result)
	}
}

func TestFormatBatchResultEmpty(t *testing.T) {
	if got := FormatBatchResult(nil); got != "No pending edits to execute." {
		t.Fatalf("FormatBatchResult(nil) = %q, want no pending message", got)
	}
}

func TestFormatBatchResultSuccessAndErrors(t *testing.T) {
	results := []BatchResult{
		{ToolName: "edit_file", Result: "Edited a.txt (1 replacement, lines 1-1).", Success: true},
		{ToolName: "write_file", Error: "denied by user"},
		{ToolName: "edit_file", Result: "Edited b.txt (2 replacements, lines 1-2).", Success: true},
	}

	got := FormatBatchResult(results)
	want := strings.Join([]string{
		"Executed 3 staged edits:",
		"1. Edited a.txt (1 replacement, lines 1-1).",
		"2. write_file: denied by user",
		"3. Edited b.txt (2 replacements, lines 1-2).",
	}, "\n")
	if got != want {
		t.Fatalf("FormatBatchResult = %q, want %q", got, want)
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("FormatBatchResult has trailing newline: %q", got)
	}
}

func pendingCall(toolName, id, path string) StagedCall {
	return StagedCall{
		ToolName:   toolName,
		ToolCallID: id,
		Params: map[string]any{
			"path": path,
		},
	}
}
