package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/agent"
)

func TestDispatchInitializeAndUnknownMethod(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: float64(1), Method: "initialize"})
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: float64(2), Method: "missing/method"})

	lines := responseLines(t, out.String(), 2)
	var initResp Response
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("initialize response json: %v", err)
	}
	if initResp.JSONRPC != "2.0" || initResp.Error != nil {
		t.Fatalf("initialize response = %+v", initResp)
	}
	result, ok := initResp.Result.(map[string]any)
	if !ok || result["protocolVersion"].(float64) != 1 {
		t.Fatalf("initialize result = %#v", initResp.Result)
	}

	var errResp Response
	if err := json.Unmarshal([]byte(lines[1]), &errResp); err != nil {
		t.Fatalf("unknown response json: %v", err)
	}
	if errResp.Error == nil || errResp.Error.Code != -32601 || !strings.Contains(errResp.Error.Message, "missing/method") {
		t.Fatalf("unknown method response = %+v", errResp)
	}
}

func TestWireHelpers(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.respond("id-1", map[string]any{"ok": true})
	r.respondError("id-2", -32602, "bad params")
	r.sendNotification(Notification{JSONRPC: "2.0", Method: "agent/test", Params: map[string]any{"x": 1}})

	lines := responseLines(t, out.String(), 3)
	if !strings.Contains(lines[0], `"jsonrpc":"2.0"`) || !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("response is not newline-terminated JSON-RPC: %q", out.String())
	}
	var resp Response
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("error response json: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 || resp.Error.Message != "bad params" {
		t.Fatalf("error response = %+v", resp)
	}
	var notif Notification
	if err := json.Unmarshal([]byte(lines[2]), &notif); err != nil {
		t.Fatalf("notification json: %v", err)
	}
	if notif.Method != "agent/test" {
		t.Fatalf("notification = %+v", notif)
	}
}

func TestHandleEventNotifications(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, Result: "hello"})
	r.handleEvent(agent.Event{Kind: agent.EventToolCallStart, ToolCallID: "tc1", ToolName: "read_file", Args: `{"path":"x"}`})
	r.handleEvent(agent.Event{Kind: agent.EventToolCallEnd, ToolCallID: "tc1", ToolName: "read_file", Result: "done"})
	r.handleEvent(agent.Event{
		Kind:   agent.EventBackgroundProcessComplete,
		Result: "done",
		BackgroundProcess: &agent.BackgroundProcessDisplay{
			ID:       "bg-1",
			Command:  "printf done",
			Reason:   "completed",
			ExitCode: 0,
			Output:   "done",
		},
	})
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, Result: "skip", SubagentSessionID: "sub"})

	lines := responseLines(t, out.String(), 4)
	wantMethods := []string{"agent/message_chunk", "agent/tool_start", "agent/tool_result", "agent/background_process_complete"}
	for i, want := range wantMethods {
		var got Notification
		if err := json.Unmarshal([]byte(lines[i]), &got); err != nil {
			t.Fatalf("notification[%d] json: %v", i, err)
		}
		if got.Method != want {
			t.Fatalf("notification[%d].Method = %q, want %q", i, got.Method, want)
		}
	}
}

func TestPermissionRespondMissingAction(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.handlePermissionRespond(Request{JSONRPC: "2.0", ID: "p", Params: json.RawMessage(`{"id":"perm"}`)})
	lines := responseLines(t, out.String(), 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 || resp.Error.Message != "missing permission action" {
		t.Fatalf("permission missing-action response = %+v", resp)
	}
}

func responseLines(t *testing.T, output string, want int) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != want {
		t.Fatalf("output lines = %d, want %d: %q", len(lines), want, output)
	}
	return lines
}
