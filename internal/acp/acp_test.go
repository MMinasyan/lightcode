package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/agent"
	lcconfig "github.com/MMinasyan/lightcode/internal/config"
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
	r.handleEvent(agent.Event{Kind: agent.EventToolCallEnd, ToolCallID: "tc1", ToolName: "read_file", Args: `{"path":"x"}`, Result: "done"})
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
	r.handleEvent(agent.Event{Kind: agent.EventWarning, Warnings: []agent.PromptWarning{{Kind: "catalog_discovery_failure", Message: "test: failed"}}})
	r.handleEvent(agent.Event{Kind: agent.EventWarning})
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, Result: "skip", SubagentSessionID: "sub"})

	lines := responseLines(t, out.String(), 6)
	wantMethods := []string{"agent/message_chunk", "agent/tool_start", "agent/tool_result", "agent/background_process_complete", "agent/warnings", "agent/warnings"}
	for i, want := range wantMethods {
		var got Notification
		if err := json.Unmarshal([]byte(lines[i]), &got); err != nil {
			t.Fatalf("notification[%d] json: %v", i, err)
		}
		if got.Method != want {
			t.Fatalf("notification[%d].Method = %q, want %q", i, got.Method, want)
		}
		if got.Method == "agent/warnings" && i == 4 {
			data, err := json.Marshal(got.Params)
			if err != nil {
				t.Fatalf("warning params marshal: %v", err)
			}
			var warnings []agent.PromptWarning
			if err := json.Unmarshal(data, &warnings); err != nil {
				t.Fatalf("warning params json: %v", err)
			}
			if len(warnings) != 1 || warnings[0].Kind != "catalog_discovery_failure" || warnings[0].Message != "test: failed" {
				t.Fatalf("warning params = %#v, want kind/message", warnings)
			}
		}
		if got.Method == "agent/tool_result" {
			params, ok := got.Params.(map[string]any)
			if !ok || params["args"] != `{"path":"x"}` {
				t.Fatalf("tool_result params = %#v, want args", got.Params)
			}
		}
		if got.Method == "agent/warnings" && i == 5 {
			data, err := json.Marshal(got.Params)
			if err != nil {
				t.Fatalf("empty warning params marshal: %v", err)
			}
			if string(data) != "[]" {
				t.Fatalf("empty warning params = %s, want []", data)
			}
		}
	}
}

func TestHandleEventFiltersSession(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.setCurrentSessionID("session-a")
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: "session-b", Result: "skip"})
	if out.Len() != 0 {
		t.Fatalf("wrong-session event was emitted: %q", out.String())
	}
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: "session-a", Result: "keep"})
	lines := responseLines(t, out.String(), 1)
	assertACPNotificationMethod(t, lines[0], "agent/message_chunk")

	out.Reset()
	r.setCurrentSessionID("")
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: "session-a", Result: "skip"})
	if out.Len() != 0 {
		t.Fatalf("empty-current event was emitted: %q", out.String())
	}
}

func TestHandleEventNotifiesUserMessageAndSystemSignal(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}

	r.handleEvent(agent.Event{Kind: agent.EventUserMessageDisplay, Turn: 4, Result: "hello"})
	r.handleEvent(agent.Event{Kind: agent.EventGenericSystemSignal, Turn: 4, Result: "Model switched to x/y"})

	lines := responseLines(t, out.String(), 2)
	var um Notification
	if err := json.Unmarshal([]byte(lines[0]), &um); err != nil {
		t.Fatalf("user_message json: %v", err)
	}
	if um.Method != "agent/user_message" {
		t.Fatalf("user_message method = %q", um.Method)
	}
	umData, _ := json.Marshal(um.Params)
	if !strings.Contains(string(umData), `"content":"hello"`) || !strings.Contains(string(umData), `"turn":4`) {
		t.Fatalf("user_message params = %s", umData)
	}

	var ss Notification
	if err := json.Unmarshal([]byte(lines[1]), &ss); err != nil {
		t.Fatalf("system_signal json: %v", err)
	}
	if ss.Method != "agent/system_signal" {
		t.Fatalf("system_signal method = %q", ss.Method)
	}
	ssData, _ := json.Marshal(ss.Params)
	if !strings.Contains(string(ssData), `"content":"System: Model switched to x/y"`) {
		t.Fatalf("system_signal params = %s", ssData)
	}
}

func TestHandleEventNotifiesQueueChanged(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}

	r.handleEvent(agent.Event{
		Kind:         agent.EventQueueChanged,
		Queue:        []agent.QueuedItem{{ID: "q-1", Content: "hi"}},
		QueueVersion: 2,
	})
	r.handleEvent(agent.Event{
		Kind:         agent.EventQueueChanged,
		QueueVersion: 3,
	})
	lines := responseLines(t, out.String(), 2)
	var qc Notification
	if err := json.Unmarshal([]byte(lines[0]), &qc); err != nil {
		t.Fatalf("queue_changed json: %v", err)
	}
	if qc.Method != "agent/queue_changed" {
		t.Fatalf("queue_changed method = %q", qc.Method)
	}
	data, _ := json.Marshal(qc.Params)
	if !strings.Contains(string(data), `"version":2`) || !strings.Contains(string(data), `"content":"hi"`) {
		t.Fatalf("queue_changed params = %s", data)
	}
	var empty Notification
	if err := json.Unmarshal([]byte(lines[1]), &empty); err != nil {
		t.Fatalf("empty queue_changed json: %v", err)
	}
	emptyData, _ := json.Marshal(empty.Params)
	if !strings.Contains(string(emptyData), `"version":3`) || !strings.Contains(string(emptyData), `"items":[]`) {
		t.Fatalf("empty queue_changed params = %s", emptyData)
	}
}

func TestHandleEventTurnEndIncludesCancelled(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}

	r.handleEvent(agent.Event{Kind: agent.EventTurnEnd, Turn: 3, Cancelled: true})
	r.handleEvent(agent.Event{Kind: agent.EventTurnEnd, Turn: 5, Cancelled: false})

	lines := responseLines(t, out.String(), 2)
	for i, expectCancelled := range []bool{true, false} {
		var n Notification
		if err := json.Unmarshal([]byte(lines[i]), &n); err != nil {
			t.Fatalf("turn_end[%d] json: %v", i, err)
		}
		if n.Method != "agent/turn_end" {
			t.Fatalf("turn_end[%d] method = %q", i, n.Method)
		}
		data, _ := json.Marshal(n.Params)
		want := `"cancelled":false`
		if expectCancelled {
			want = `"cancelled":true`
		}
		if !strings.Contains(string(data), want) {
			t.Fatalf("turn_end[%d] params = %s, want %s", i, data, want)
		}
	}
}

func TestHandleEventCompactionEndPushesSessionChanged(t *testing.T) {
	var out bytes.Buffer
	a := newACPTestAgent(t)
	_ = appendACPUserTurn(t, a, "seed")
	r := &Runner{agent: a, out: &out}
	r.setCurrentSessionID(a.SessionCurrent().ID)

	r.handleEvent(agent.Event{Kind: agent.EventCompactionEnd, RefreshSession: true})

	lines := responseLines(t, out.String(), 2)
	assertACPNotificationMethod(t, lines[0], "agent/compaction_end")
	assertACPNotificationMethod(t, lines[1], "agent/session_changed")
}

func TestHandleEventActiveCompactionDefersSessionChangedUntilTurnEnd(t *testing.T) {
	a := newACPTestAgent(t)
	_ = appendACPUserTurn(t, a, "complete before compaction")
	turn := a.Store().BeginTurn()
	for _, raw := range []string{
		`{"role":"user","content":"active prompt"}`,
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}`,
		`{"role":"tool","tool_call_id":"call_1","name":"read_file","content":"ok"}`,
	} {
		if err := a.Store().AppendMessage(turn, []byte(raw)); err != nil {
			t.Fatalf("AppendMessage active: %v", err)
		}
	}
	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.setCurrentSessionID(a.SessionCurrent().ID)

	r.handleEvent(agent.Event{Kind: agent.EventCompactionEnd})
	lines := responseLines(t, out.String(), 1)
	assertACPNotificationMethod(t, lines[0], "agent/compaction_end")

	if err := a.Store().MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete active: %v", err)
	}
	out.Reset()
	r.handleEvent(agent.Event{Kind: agent.EventTurnEnd, Turn: turn, RefreshSession: true})
	lines = responseLines(t, out.String(), 2)
	assertACPNotificationMethod(t, lines[0], "agent/turn_end")
	assertACPNotificationMethod(t, lines[1], "agent/session_changed")
	if !strings.Contains(lines[1], "active prompt") {
		t.Fatalf("session_changed after turn_end omitted completed active turn: %s", lines[1])
	}
}

func TestDispatchWarningsCurrentReturnsCurrentWarningSnapshot(t *testing.T) {
	a := newACPWarningTestAgent(t)
	if len(a.CurrentWarnings()) == 0 {
		t.Fatal("warning test agent has empty startup warning snapshot")
	}
	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}

	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "warnings", Method: "warnings/current"})

	lines := responseLines(t, out.String(), 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("warnings/current error = %+v", resp.Error)
	}
	warnings := promptWarningsFromResponse(t, resp)
	if !hasPromptWarningKind(warnings, "catalog_discovery_failure") {
		t.Fatalf("warnings = %#v, want catalog_discovery_failure", warnings)
	}
}

func TestDispatchWarningsCurrentReturnsEmptyArrayForNoWarnings(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{agent: newACPTestAgent(t), out: &out}

	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "warnings", Method: "warnings/current"})

	lines := responseLines(t, out.String(), 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("empty warnings result = %s, want []", data)
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

func TestHandleTurnActionACPRevertCodeReturnsResultWithoutSessionChanged(t *testing.T) {
	a := newACPTestAgent(t)
	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}

	_ = appendACPUserTurn(t, a, "first")
	r.setCurrentSessionID(a.SessionCurrent().ID)
	path := filepath.Join(a.ProjectRoot(), "created.txt")
	clickedTurn := appendACPUserTurnWithSnapshot(t, a, "create file", path, "created\n")

	r.handleRevertCode(Request{
		JSONRPC: "2.0",
		ID:      "revert-code",
		Params:  json.RawMessage(`{"turn":` + itoa(clickedTurn) + `,"alsoRevertCode":true}`),
	})

	lines := responseLines(t, out.String(), 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	result := turnActionResultFromResponse(t, resp)
	if resp.Error != nil || result.Action != agent.TurnActionRevertCode || result.Turn != clickedTurn || result.TargetTurn != clickedTurn-1 || result.SessionChanged {
		t.Fatalf("response/result = %+v %#v, want revert_code result without session change", resp, result)
	}
	if len(result.Messages) != 0 {
		t.Fatalf("revert_code messages len = %d, want no hydrated messages", len(result.Messages))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file still exists after revert code; stat err=%v", err)
	}
}

func TestHandleTurnActionACPRevertHistoryPropagatesAlsoRevertCode(t *testing.T) {
	a := newACPTestAgent(t)
	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}

	_ = appendACPUserTurn(t, a, "first")
	r.setCurrentSessionID(a.SessionCurrent().ID)
	path := filepath.Join(a.ProjectRoot(), "created.txt")
	clickedTurn := appendACPUserTurnWithSnapshot(t, a, "create file", path, "created\n")
	_ = appendACPUserTurn(t, a, "after")

	r.handleRevertHistory(Request{
		JSONRPC: "2.0",
		ID:      "revert-history",
		Params:  json.RawMessage(`{"turn":` + itoa(clickedTurn) + `,"alsoRevertCode":true}`),
	})

	lines := responseLines(t, out.String(), 2)
	assertACPNotificationMethod(t, lines[0], "agent/session_changed")
	var resp Response
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	result := turnActionResultFromResponse(t, resp)
	if resp.Error != nil || result.Action != agent.TurnActionRevertHistory || result.TargetTurn != clickedTurn-1 || !result.SessionChanged || result.Prefill != "create file" {
		t.Fatalf("response/result = %+v %#v, want revert_history result", resp, result)
	}
	if got := acpUserMessageContents(result.Messages); !equalStringSlices(got, []string{"first"}) {
		t.Fatalf("result messages = %q, want truncated history", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file still exists after history+code revert; stat err=%v", err)
	}
}

func TestHandleTurnActionACPForkReturnsResultAndSessionChanged(t *testing.T) {
	a := newACPTestAgent(t)
	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}

	_ = appendACPUserTurn(t, a, "first")
	r.setCurrentSessionID(a.SessionCurrent().ID)
	clickedTurn := appendACPUserTurn(t, a, "fork point")
	_ = appendACPUserTurn(t, a, "after")
	beforeID := a.SessionCurrent().ID

	r.handleSessionFork(Request{
		JSONRPC: "2.0",
		ID:      "fork",
		Params:  json.RawMessage(`{"turn":` + itoa(clickedTurn) + `}`),
	})

	lines := responseLines(t, out.String(), 2)
	assertACPNotificationMethod(t, lines[0], "agent/session_changed")
	var resp Response
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	result := turnActionResultFromResponse(t, resp)
	if resp.Error != nil || result.Action != agent.TurnActionFork || result.TargetTurn != clickedTurn || !result.SessionChanged || result.Session.ID == "" || result.Session.ID == beforeID {
		t.Fatalf("response/result = %+v %#v, want fork result with new session", resp, result)
	}
	if got := acpUserMessageContents(result.Messages); !equalStringSlices(got, []string{"first", "fork point"}) {
		t.Fatalf("fork messages = %q, want selected turn included", got)
	}
}

func TestHandleTurnActionACPInvalidParams(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{agent: newACPTestAgent(t), out: &out}

	r.handleRevertHistory(Request{JSONRPC: "2.0", ID: "bad", Params: json.RawMessage(`{`)})

	lines := responseLines(t, out.String(), 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 || resp.Error.Message != "invalid params" {
		t.Fatalf("invalid params response = %+v, want -32602 invalid params", resp)
	}
}

func TestHandleSessionMessagesByIDDoesNotSwitchCurrentSession(t *testing.T) {
	a := newACPTestAgent(t)
	firstTurn := appendACPUserTurn(t, a, "first session")
	firstID := a.SessionCurrent().ID
	if firstID == "" || firstTurn == 0 {
		t.Fatalf("first session id/turn = %q/%d", firstID, firstTurn)
	}
	if err := a.SessionNew(); err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	appendACPUserTurn(t, a, "second session")
	currentID := a.SessionCurrent().ID
	if currentID == "" || currentID == firstID {
		t.Fatalf("current session id = %q, first = %q", currentID, firstID)
	}

	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.setCurrentSessionID(currentID)
	r.handleSessionMessages(Request{
		JSONRPC: "2.0",
		ID:      "messages-by-id",
		Params:  json.RawMessage(`{"id":"` + firstID + `"}`),
	})

	lines := responseLines(t, out.String(), 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("session/messages by id error = %+v", resp.Error)
	}
	msgs := displayMessagesFromResponse(t, resp)
	if got := acpUserMessageContents(msgs); !equalStringSlices(got, []string{"first session"}) {
		t.Fatalf("messages for first session = %q", got)
	}
	if got := a.SessionCurrent().ID; got != currentID {
		t.Fatalf("SessionMessagesFor switched current session to %q, want %q", got, currentID)
	}

	out.Reset()
	r.handleSessionMessages(Request{JSONRPC: "2.0", ID: "current-messages"})
	lines = responseLines(t, out.String(), 1)
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("current response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("session/messages current error = %+v", resp.Error)
	}
	msgs = displayMessagesFromResponse(t, resp)
	if got := acpUserMessageContents(msgs); !equalStringSlices(got, []string{"second session"}) {
		t.Fatalf("current messages = %q", got)
	}
}

func TestACPPromptSelectsSession(t *testing.T) {
	a := newACPTestAgent(t)
	_ = appendACPUserTurn(t, a, "first")
	firstID := a.SessionCurrent().ID
	if firstID == "" {
		t.Fatal("missing first session")
	}
	if err := a.SessionNew(); err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	_ = appendACPUserTurn(t, a, "second")
	secondID := a.SessionCurrent().ID
	if secondID == "" || secondID == firstID {
		t.Fatalf("second session id = %q, first = %q", secondID, firstID)
	}

	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.setCurrentSessionID(firstID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.handleSessionPrompt(ctx, Request{
		JSONRPC: "2.0",
		ID:      "prompt",
		Params:  json.RawMessage(`{"session_id":"` + secondID + `","content":"hello"}`),
	})
	if got := r.currentSessionSummary().ID; got != secondID {
		t.Fatalf("current session = %q, want %q", got, secondID)
	}
}

func TestACPNewSetsCurrent(t *testing.T) {
	a := newACPTestAgent(t)
	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.handleSessionNew(Request{JSONRPC: "2.0", ID: "new"})
	if got := r.currentSessionSummary().ID; got == "" {
		t.Fatal("new session did not set current")
	}

	out.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.handleSessionPrompt(ctx, Request{
		JSONRPC: "2.0",
		ID:      "prompt",
		Params:  json.RawMessage(`{"content":"hello"}`),
	})
	lines := responseLines(t, out.String(), 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("prompt response json: %v", err)
	}
	if resp.Error != nil && strings.Contains(resp.Error.Message, "no current session") {
		t.Fatalf("prompt after new = %+v", resp.Error)
	}
}

func TestACPStaleCurrent(t *testing.T) {
	a := newACPTestAgent(t)
	_ = appendACPUserTurn(t, a, "gone")
	id := a.SessionCurrent().ID
	if id == "" {
		t.Fatal("missing session id")
	}
	if _, err := a.SessionDelete(id); err != nil {
		t.Fatalf("SessionDelete: %v", err)
	}

	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.setCurrentSessionID(id)
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "current", Method: "session/current"})
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "messages", Method: "session/messages"})

	lines := responseLines(t, out.String(), 2)
	var current Response
	if err := json.Unmarshal([]byte(lines[0]), &current); err != nil {
		t.Fatalf("current json: %v", err)
	}
	data, err := json.Marshal(current.Result)
	if err != nil {
		t.Fatalf("current marshal: %v", err)
	}
	var summary agent.SessionSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("current result json: %v", err)
	}
	if summary.ID != "" {
		t.Fatalf("stale current id = %q, want empty", summary.ID)
	}

	var messages Response
	if err := json.Unmarshal([]byte(lines[1]), &messages); err != nil {
		t.Fatalf("messages json: %v", err)
	}
	if messages.Error == nil {
		t.Fatalf("stale current messages response = %+v, want error", messages)
	}
}

func TestACPStaleEvent(t *testing.T) {
	a := newACPTestAgent(t)
	_ = appendACPUserTurn(t, a, "gone")
	id := a.SessionCurrent().ID
	if id == "" {
		t.Fatal("missing session id")
	}
	if _, err := a.SessionDelete(id); err != nil {
		t.Fatalf("SessionDelete: %v", err)
	}

	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.setCurrentSessionID(id)
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: id, Result: "skip"})
	if out.Len() != 0 {
		t.Fatalf("stale event was emitted: %q", out.String())
	}
	if got := r.currentSessionSummary().ID; got != "" {
		t.Fatalf("stale current id = %q, want empty", got)
	}
}

func TestACPClearRemovedCurrent(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Runner, Request)
	}{
		{name: "archive", run: func(r *Runner, req Request) { r.handleSessionArchive(req) }},
		{name: "delete", run: func(r *Runner, req Request) { r.handleSessionDelete(req) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newACPTestAgent(t)
			firstID, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession first: %v", err)
			}
			if _, err := a.AppendUserMessageToSession(firstID, "first"); err != nil {
				t.Fatalf("append first: %v", err)
			}
			secondID, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession second: %v", err)
			}
			if _, err := a.AppendUserMessageToSession(secondID, "second"); err != nil {
				t.Fatalf("append second: %v", err)
			}

			var out bytes.Buffer
			r := &Runner{agent: a, out: &out}
			r.setCurrentSessionID(firstID)
			tc.run(r, Request{
				JSONRPC: "2.0",
				ID:      tc.name,
				Params:  json.RawMessage(`{"id":"` + firstID + `"}`),
			})
			if got := r.currentSessionSummary().ID; got != "" {
				t.Fatalf("current after %s = %q, want empty", tc.name, got)
			}
			out.Reset()
			r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "snapshots", Method: "snapshot/list"})
			lines := responseLines(t, out.String(), 1)
			var snapshots Response
			if err := json.Unmarshal([]byte(lines[0]), &snapshots); err != nil {
				t.Fatalf("snapshot response json: %v", err)
			}
			if snapshots.Error == nil {
				t.Fatalf("%s snapshot/list response = %+v, want error", tc.name, snapshots)
			}
			out.Reset()
			r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "compact", Method: "compact"})
			lines = responseLines(t, out.String(), 1)
			var compact Response
			if err := json.Unmarshal([]byte(lines[0]), &compact); err != nil {
				t.Fatalf("compact response json: %v", err)
			}
			if compact.Error == nil {
				t.Fatalf("%s compact response = %+v, want error", tc.name, compact)
			}
			out.Reset()
			r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: firstID, Result: "skip"})
			if strings.Contains(out.String(), "skip") {
				t.Fatalf("%s left old session event visible: %q", tc.name, out.String())
			}
			if current := a.SessionCurrent().ID; current != secondID {
				t.Fatalf("backend current after %s = %q, want %q", tc.name, current, secondID)
			}
		})
	}
}

func TestACPHandlersUseSharedTurnActionContract(t *testing.T) {
	src := mustReadACPSource(t)
	helper := extractSourceFunc(t, src, "func (r *Runner) handleTurnAction(")
	if !strings.Contains(helper, ".ApplyTurnActionForSession(") {
		t.Fatal("handleTurnAction must call ApplyTurnActionForSession")
	}
	for _, forbidden := range []string{".ForkSession(", ".RevertCode(", ".RevertHistory("} {
		if strings.Contains(helper, forbidden) {
			t.Fatalf("handleTurnAction must not call low-level %s", forbidden)
		}
	}

	wrappers := map[string]string{
		"func (r *Runner) handleSessionFork(":   "r.handleTurnAction(req, agent.TurnActionFork)",
		"func (r *Runner) handleRevertCode(":    "r.handleTurnAction(req, agent.TurnActionRevertCode)",
		"func (r *Runner) handleRevertHistory(": "r.handleTurnAction(req, agent.TurnActionRevertHistory)",
	}
	for signature, want := range wrappers {
		body := extractSourceFunc(t, src, signature)
		if !strings.Contains(body, want) {
			t.Fatalf("%s must delegate with %q; body:\n%s", signature, want, body)
		}
	}

	compact := extractSourceFunc(t, src, "func (r *Runner) handleCompact(")
	if strings.Contains(compact, "pushSessionChanged(") {
		t.Fatalf("handleCompact must leave session refresh to EventCompactionEnd; body:\n%s", compact)
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

func newACPTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	return newACPTestAgentWithProvider(t, "http://127.0.0.1:9/v1", false)
}

func newACPWarningTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	a := newACPTestAgentWithProvider(t, server.URL+"/v1", true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)
	return a
}

func newACPTestAgentWithProvider(t *testing.T, baseURL string, discovery bool) *agent.Agent {
	t.Helper()
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test-key")
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "`+baseURL+`", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": `+strconv.FormatBool(discovery)+`,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "default_model": "test/test-model"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := lcconfig.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := agent.New(agent.Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

func appendACPUserTurn(t *testing.T, a *agent.Agent, content string) int {
	t.Helper()
	turn, err := a.AppendUserMessage(content)
	if err != nil {
		t.Fatalf("AppendUserMessage: %v", err)
	}
	return turn
}

func appendACPUserTurnWithSnapshot(t *testing.T, a *agent.Agent, content, path, after string) int {
	t.Helper()
	turn := appendACPUserTurn(t, a, content)
	entryID, _, err := a.Store().SnapshotResolvedEntry(turn, path, path)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatalf("write changed file: %v", err)
	}
	if err := a.Store().RecordSnapshotContent(turn, entryID, []byte(after)); err != nil {
		t.Fatalf("RecordSnapshotContent: %v", err)
	}
	return turn
}

func assertACPNotificationMethod(t *testing.T, line, method string) {
	t.Helper()
	var notif Notification
	if err := json.Unmarshal([]byte(line), &notif); err != nil {
		t.Fatalf("notification json: %v", err)
	}
	if notif.Method != method {
		t.Fatalf("notification method = %q, want %q", notif.Method, method)
	}
}

func turnActionResultFromResponse(t *testing.T, resp Response) agent.TurnActionResult {
	t.Helper()
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var result agent.TurnActionResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal turn action result: %v", err)
	}
	return result
}

func displayMessagesFromResponse(t *testing.T, resp Response) []agent.DisplayMessage {
	t.Helper()
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var messages []agent.DisplayMessage
	if err := json.Unmarshal(data, &messages); err != nil {
		t.Fatalf("unmarshal display messages: %v", err)
	}
	return messages
}

func promptWarningsFromResponse(t *testing.T, resp Response) []agent.PromptWarning {
	t.Helper()
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var warnings []agent.PromptWarning
	if err := json.Unmarshal(data, &warnings); err != nil {
		t.Fatalf("unmarshal warnings: %v", err)
	}
	return warnings
}

func acpUserMessageContents(messages []agent.DisplayMessage) []string {
	var out []string
	for _, msg := range messages {
		if msg.Type == "user" {
			out = append(out, msg.Content)
		}
	}
	return out
}

func hasPromptWarningKind(warnings []agent.PromptWarning, kind string) bool {
	for _, warning := range warnings {
		if warning.Kind == kind {
			return true
		}
	}
	return false
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func mustReadACPSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("acp.go")
	if err != nil {
		t.Fatalf("read acp.go: %v", err)
	}
	return string(data)
}

func extractSourceFunc(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("missing function signature %q", signature)
	}
	brace := strings.IndexByte(src[start:], '{')
	if brace < 0 {
		t.Fatalf("missing body for signature %q", signature)
	}
	depth := 0
	for i := start + brace; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("unterminated function %q", signature)
	return ""
}
