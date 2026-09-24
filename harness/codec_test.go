package harness

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/model"
)

// Fixture identities: durable 32-lowercase-hex session, entry, and result IDs
// plus opaque Operation identities. Random generation lands with Session
// creation, so fixtures spell identities literally.
func hexID(n int) string { return fmt.Sprintf("%032x", n) }

const (
	testSessionID = "000000000000000000000000000000aa"
	testEntryID   = "000000000000000000000000000000bb"
	testOpID      = "op-1"
	testResultID  = "000000000000000000000000000000cc"
)

var testTime = time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)

func testModelRef() model.ModelRef { return model.ModelRef{Provider: "prov", Model: "gpt-x"} }

func testToolDefinition() model.ToolDefinition {
	return model.ToolDefinition{Name: "echo", Description: "echoes", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func testCompactCapture() CompactCapture {
	return CompactCapture{
		Model:         model.ModelRef{Provider: "cprov", Model: "compact-x"},
		ContextWindow: 2048,
		OutputReserve: 1024,
		SystemPrompt:  "summarize",
	}
}

func testCapture() ExecutionCapture {
	return ExecutionCapture{
		ConfigurationRevision: "rev-1",
		Model:                 testModelRef(),
		ContextWindow:         4096,
		OutputReserve:         2048,
		SystemPrompt:          "system",
		Tools:                 []model.ToolDefinition{testToolDefinition()},
		Compact:               testCompactCapture(),
	}
}

func testUsage(n int64) *UsageCount {
	return &UsageCount{InputTokens: n, CachedInputTokens: n, OutputTokens: n}
}

// --- valid value builders -------------------------------------------------

func validSessionRecord() SessionRecord {
	return SessionRecord{
		Revision: 1,
		Identity: SessionIdentity{SessionID: testSessionID, Workspace: "/tmp/works", CreatedAt: testTime},
		State: SessionState{
			Lifecycle:        LifecycleOpen,
			CurrentAgentType: "coder",
			Usage:            UsageTotals{},
			LastActivity:     testTime,
		},
	}
}

func validOperationRecord() OperationRecord {
	return OperationRecord{
		Revision: 1,
		Admission: OperationAdmission{
			SessionID:     testSessionID,
			OperationID:   testOpID,
			RequestKind:   RequestKindMessage,
			AdmittedEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(1)},
			AgentType:     "coder",
			Execution:     testCapture(),
			AdmittedAt:    testTime,
		},
		State: OperationCurrentState{
			Status:           OperationRunning,
			StartedAt:        testTime,
			PendingToolCalls: []PendingToolCall{},
			Usage:            UsageTotals{},
		},
	}
}

func validInputEntry(operationID string) inputEntry {
	return inputEntry{
		SessionID:   testSessionID,
		EntryID:     testEntryID,
		OperationID: operationID,
		Origin:      InputOriginUser,
		Content:     []model.ContentPart{model.ContentPart{Kind: model.PartText, Text: "hello"}},
	}
}

func validToolCallRecord() toolCallRecord {
	return toolCallRecord{
		ID:              "call-1",
		Ordinal:         0,
		Name:            "echo",
		ArgumentsBase64: base64.StdEncoding.EncodeToString([]byte(`{"x":1}`)),
		ResultEntryID:   testResultID,
	}
}

func validAssistantEntry(operationID string) assistantEntry {
	return assistantEntry{
		SessionID:   testSessionID,
		EntryID:     testEntryID,
		OperationID: operationID,
		Status:      model.OutputCompleted,
		Source:      testModelRef(),
		Content:     []model.ContentPart{model.ContentPart{Kind: model.PartText, Text: "hi"}},
		ToolCalls:   []toolCallRecord{},
	}
}

func validToolResultEntry(operationID string) toolResultEntry {
	return toolResultEntry{
		SessionID:      testSessionID,
		EntryID:        testEntryID,
		OperationID:    operationID,
		AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
		ToolCallID:     "call-1",
		Status:         model.ResultSuccess,
		Content:        "done",
	}
}

func validSignalEntry(operationID string) signalEntry {
	return signalEntry{
		SessionID:        testSessionID,
		EntryID:          testEntryID,
		OperationID:      operationID,
		Signal:           SignalInterruption,
		RelatedOperation: &operationRef{SessionID: testSessionID, OperationID: testOpID},
		Content:          signalInterruptionContent,
	}
}

// validBackgroundCompletionEntry returns one valid operationless
// background_completion signal entry recording a child member's completion.
func validBackgroundCompletionEntry() signalEntry {
	return signalEntry{
		SessionID:     testSessionID,
		EntryID:       testEntryID,
		Signal:        SignalBackgroundCompletion,
		RelatedMember: &relatedMember{Kind: "child", ID: otherSession()},
		Content:       "Task completed.",
	}
}

func validSettlementEntry() operationSettlementEntry {
	return operationSettlementEntry{
		SessionID:   testSessionID,
		EntryID:     testEntryID,
		OperationID: testOpID,
		Status:      OperationSuccess,
	}
}

func validCompactionEntry(operationID string) compactionEntry {
	return compactionEntry{
		SessionID:             testSessionID,
		EntryID:               testEntryID,
		OperationID:           operationID,
		Summary:               "Summary of the earlier conversation.",
		BoundaryEntryID:       hexID(2),
		Model:                 testModelRef(),
		ConfigurationRevision: "rev-1",
	}
}

func validHookResultEntry(status hookResultStatus) hookResultEntry {
	v := hookResultEntry{
		SessionID:   testSessionID,
		EntryID:     testEntryID,
		OperationID: testOpID,
		HookID:      "cap.hook.one",
		ToolCallID:  "call-1",
		Status:      status,
	}
	switch status {
	case hookSucceeded:
		v.Arguments = json.RawMessage(`{"x":1}`)
	default:
		v.Error = "hook broke"
	}
	return v
}

// --- wire mutation helpers --------------------------------------------------

func wireObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	obj, err := decodePayloadObject(raw)
	if err != nil {
		t.Fatalf("fixture payload is not a strict object: %v", err)
	}
	return obj
}

func setKey(raw json.RawMessage, key string, value json.RawMessage) json.RawMessage {
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		panic(err)
	}
	obj[key] = value
	out, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return out
}

func renameKey(raw json.RawMessage, from, to string) json.RawMessage {
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		panic(err)
	}
	if v, ok := obj[from]; ok {
		delete(obj, from)
		obj[to] = v
	}
	out, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return out
}

func dropKey(raw json.RawMessage, key string) json.RawMessage {
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		panic(err)
	}
	delete(obj, key)
	out, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return out
}

func otherSession() string { return strings.Repeat("ff", 16) }
func otherEntry() string   { return strings.Repeat("ee", 16) }

func entryEnv(payload json.RawMessage) Entry {
	return Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryInput, Sequence: 1, CommittedAt: testTime, Payload: payload}
}

// TestEntryPayloadRoundTrip proves every entry payload kind encodes, decodes
// back to an equal owned value, and re-encodes to identical wire bytes.
func TestEntryPayloadRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		env    Entry
		encode func() (json.RawMessage, error)
		decode func(Entry) error
	}{
		{
			name:   "input",
			env:    entryEnv(nil),
			encode: func() (json.RawMessage, error) { return encodeInputEntry(validInputEntry(testOpID)) },
			decode: func(env Entry) error { _, err := decodeInputEntry(env); return err },
		},
		{
			name: "assistant",
			env:  Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Sequence: 1, CommittedAt: testTime},
			encode: func() (json.RawMessage, error) {
				v := validAssistantEntry(testOpID)
				v.Usage = testUsage(3)
				return encodeAssistantEntry(v)
			},
			decode: func(env Entry) error { _, err := decodeAssistantEntry(env); return err },
		},
		{
			name: "assistant operationless fork copy",
			env:  Entry{SessionID: testSessionID, ID: testEntryID, Kind: EntryAssistant, Sequence: 1, CommittedAt: testTime},
			encode: func() (json.RawMessage, error) {
				return encodeAssistantEntry(validAssistantEntry(""))
			},
			decode: func(env Entry) error { _, err := decodeAssistantEntry(env); return err },
		},
		{
			name: "tool_result",
			env:  Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryToolResult, Sequence: 1, CommittedAt: testTime},
			encode: func() (json.RawMessage, error) {
				return encodeToolResultEntry(validToolResultEntry(testOpID))
			},
			decode: func(env Entry) error { _, err := decodeToolResultEntry(env); return err },
		},
		{
			name: "signal",
			env:  Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntrySignal, Sequence: 1, CommittedAt: testTime},
			encode: func() (json.RawMessage, error) {
				return encodeSignalEntry(validSignalEntry(testOpID))
			},
			decode: func(env Entry) error { _, err := decodeSignalEntry(env); return err },
		},
		{
			name: "hook_result success",
			env:  Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryHookResult, Sequence: 1, CommittedAt: testTime},
			encode: func() (json.RawMessage, error) {
				return encodeHookResultEntry(validHookResultEntry(hookSucceeded))
			},
			decode: func(env Entry) error { _, err := decodeHookResultEntry(env); return err },
		},
		{
			name: "hook_result interrupted",
			env:  Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryHookResult, Sequence: 1, CommittedAt: testTime},
			encode: func() (json.RawMessage, error) {
				return encodeHookResultEntry(validHookResultEntry(hookInterrupted))
			},
			decode: func(env Entry) error { _, err := decodeHookResultEntry(env); return err },
		},
		{
			name: "operation_settlement",
			env:  Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 1, CommittedAt: testTime},
			encode: func() (json.RawMessage, error) {
				return encodeOperationSettlementEntry(validSettlementEntry())
			},
			decode: func(env Entry) error { _, err := decodeOperationSettlementEntry(env); return err },
		},
		{
			name: "compaction",
			env:  Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime},
			encode: func() (json.RawMessage, error) {
				v := validCompactionEntry(testOpID)
				v.Usage = testUsage(3)
				return encodeCompactionEntry(v)
			},
			decode: func(env Entry) error { _, err := decodeCompactionEntry(env); return err },
		},
		{
			name: "compaction operationless fork copy",
			env:  Entry{SessionID: testSessionID, ID: testEntryID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime},
			encode: func() (json.RawMessage, error) {
				return encodeCompactionEntry(validCompactionEntry(""))
			},
			decode: func(env Entry) error { _, err := decodeCompactionEntry(env); return err },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := tc.encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			env := tc.env
			env.Payload = raw
			if err := tc.decode(env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// A second decode of the same bytes stays stable.
			if err := tc.decode(env); err != nil {
				t.Fatalf("second decode: %v", err)
			}
		})
	}
}

// TestEntryPayloadRejectsInvalidWire proves the shared codec rules on every
// entry payload kind: exact case-sensitive keys only, no unknown or miscased
// members, no null or wrong-container members, and payload/envelope identity
// agreement.
func TestEntryPayloadRejectsInvalidWire(t *testing.T) {
	type kindCase struct {
		name       string
		container  string // a required object/array member to null out
		wrongValue json.RawMessage
		payload    func() (json.RawMessage, error)
		decode     func(Entry) error
	}
	kinds := []kindCase{
		{
			name:       "input",
			container:  "content",
			wrongValue: json.RawMessage(`{}`),
			payload:    func() (json.RawMessage, error) { return encodeInputEntry(validInputEntry(testOpID)) },
			decode:     func(env Entry) error { _, err := decodeInputEntry(env); return err },
		},
		{
			name:       "assistant",
			container:  "source",
			wrongValue: json.RawMessage(`"prov/gpt-x"`),
			payload:    func() (json.RawMessage, error) { return encodeAssistantEntry(validAssistantEntry(testOpID)) },
			decode:     func(env Entry) error { _, err := decodeAssistantEntry(env); return err },
		},
		{
			name:       "tool_result",
			container:  "assistant_entry",
			wrongValue: json.RawMessage(`"x"`),
			payload:    func() (json.RawMessage, error) { return encodeToolResultEntry(validToolResultEntry(testOpID)) },
			decode:     func(env Entry) error { _, err := decodeToolResultEntry(env); return err },
		},
		{
			name:       "signal",
			container:  "related_operation",
			wrongValue: json.RawMessage(`[]`),
			payload:    func() (json.RawMessage, error) { return encodeSignalEntry(validSignalEntry(testOpID)) },
			decode:     func(env Entry) error { _, err := decodeSignalEntry(env); return err },
		},
		{
			name:       "hook_result",
			container:  "status",
			wrongValue: json.RawMessage(`[]`),
			payload:    func() (json.RawMessage, error) { return encodeHookResultEntry(validHookResultEntry(hookSucceeded)) },
			decode:     func(env Entry) error { _, err := decodeHookResultEntry(env); return err },
		},
		{
			name:       "operation_settlement",
			container:  "usage",
			wrongValue: json.RawMessage(`0`),
			payload: func() (json.RawMessage, error) {
				v := validSettlementEntry()
				v.Status = OperationFailure
				v.Detail = "boom"
				v.Model = new(model.ModelRef)
				*v.Model = testModelRef()
				v.Usage = testUsage(1)
				return encodeOperationSettlementEntry(v)
			},
			decode: func(env Entry) error { _, err := decodeOperationSettlementEntry(env); return err },
		},
		{
			name:       "compaction",
			container:  "model",
			wrongValue: json.RawMessage(`"prov/gpt-x"`),
			payload: func() (json.RawMessage, error) {
				v := validCompactionEntry(testOpID)
				v.Usage = testUsage(1)
				return encodeCompactionEntry(v)
			},
			decode: func(env Entry) error { _, err := decodeCompactionEntry(env); return err },
		},
	}
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			raw, err := kind.payload()
			if err != nil {
				t.Fatalf("encode valid: %v", err)
			}
			base := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Sequence: 1, CommittedAt: testTime}
			switch kind.name {
			case "operation_settlement":
				base.Kind = EntryOperationSettlement
			case "compaction":
				base.Kind = EntryCompaction
			}
			mutations := []struct {
				name    string
				payload json.RawMessage
			}{
				{"unknown key", setKey(raw, "bogus", json.RawMessage(`1`))},
				{"miscased key", renameKey(raw, "session_id", "Session_ID")},
				{"null container", setKey(raw, kind.container, json.RawMessage(`null`))},
				{"wrong container", setKey(raw, kind.container, kind.wrongValue)},
				{"session mismatch", setKey(raw, "session_id", json.RawMessage(`"`+otherSession()+`"`))},
				{"entry mismatch", setKey(raw, "entry_id", json.RawMessage(`"`+otherEntry()+`"`))},
				{"operation mismatch", setKey(raw, "operation_id", json.RawMessage(`"ghost"`))},
				{"trailing value", append(append([]byte{}, raw...), []byte(` {"x":1}`)...)},
			}
			for _, m := range mutations {
				t.Run(m.name, func(t *testing.T) {
					env := base
					env.Payload = m.payload
					if err := kind.decode(env); err == nil {
						t.Fatalf("expected rejection")
					}
				})
			}
		})
	}
}

// TestEntryPayloadOperationlessRules proves the operationless entry rules:
// the four fork-copyable kinds may omit owning Operation identity, and an
// independently copied assistant carries no source usage.
func TestEntryPayloadOperationlessRules(t *testing.T) {
	env := Entry{SessionID: testSessionID, ID: testEntryID, Kind: EntryAssistant, Sequence: 1, CommittedAt: testTime}
	raw, err := encodeAssistantEntry(validAssistantEntry(""))
	if err != nil {
		t.Fatalf("encode operationless assistant: %v", err)
	}
	env.Payload = raw
	if _, err := decodeAssistantEntry(env); err != nil {
		t.Fatalf("operationless assistant without usage must decode: %v", err)
	}

	withUsage := validAssistantEntry("")
	withUsage.Usage = testUsage(2)
	if _, err := encodeAssistantEntry(withUsage); err == nil {
		t.Fatalf("encode of operationless assistant with usage must fail")
	} else if !errors.Is(err, ErrInvalid) {
		t.Fatalf("operationless usage rejection must use ErrInvalid, got %v", err)
	}
	forced := setKey(raw, "usage", json.RawMessage(`{"input_tokens":1,"cached_input_tokens":0,"output_tokens":0}`))
	env.Payload = forced
	if _, err := decodeAssistantEntry(env); err == nil {
		t.Fatalf("operationless assistant with stored usage must be rejected")
	}
}

// TestHookResultStatusRules proves the hook-result status-owned member rules:
// success requires exactly one complete JSON object argument and forbids an
// error; error and interrupted require a non-empty error and forbid
// arguments; unknown statuses reject; and a hook result without an owning
// Operation identity rejects.
func TestHookResultStatusRules(t *testing.T) {
	t.Run("success rules", func(t *testing.T) {
		if _, err := encodeHookResultEntry(validHookResultEntry(hookSucceeded)); err != nil {
			t.Fatalf("encode valid success: %v", err)
		}
		nonObject := validHookResultEntry(hookSucceeded)
		nonObject.Arguments = json.RawMessage(`[1,2]`)
		if _, err := encodeHookResultEntry(nonObject); err == nil {
			t.Fatalf("array arguments must reject")
		}
		nullArguments := validHookResultEntry(hookSucceeded)
		nullArguments.Arguments = json.RawMessage(`null`)
		if _, err := encodeHookResultEntry(nullArguments); err == nil {
			t.Fatalf("null arguments must reject")
		}
		withError := validHookResultEntry(hookSucceeded)
		withError.Error = "boom"
		if _, err := encodeHookResultEntry(withError); err == nil {
			t.Fatalf("success with an error must reject")
		}
	})
	t.Run("failure and interruption rules", func(t *testing.T) {
		for _, status := range []hookResultStatus{hookFailed, hookInterrupted} {
			if _, err := encodeHookResultEntry(validHookResultEntry(status)); err != nil {
				t.Fatalf("encode valid %s: %v", status, err)
			}
			empty := validHookResultEntry(status)
			empty.Error = ""
			if _, err := encodeHookResultEntry(empty); err == nil {
				t.Fatalf("%s without an error must reject", status)
			}
			withArguments := validHookResultEntry(status)
			withArguments.Arguments = json.RawMessage(`{}`)
			if _, err := encodeHookResultEntry(withArguments); err == nil {
				t.Fatalf("%s with arguments must reject", status)
			}
		}
	})
	t.Run("unknown status", func(t *testing.T) {
		if _, err := encodeHookResultEntry(validHookResultEntry("ok")); err == nil {
			t.Fatalf("unknown status must reject")
		}
	})
	t.Run("decode-side rejections", func(t *testing.T) {
		raw, err := encodeHookResultEntry(validHookResultEntry(hookSucceeded))
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		for _, mutation := range []struct {
			name    string
			payload json.RawMessage
		}{
			{"empty hook id", setKey(raw, "hook_id", json.RawMessage(`""`))},
			{"missing hook id", setKey(raw, "hook_id", nil)},
			{"empty call id", setKey(raw, "tool_call_id", json.RawMessage(`""`))},
			{"missing operation", setKey(raw, "operation_id", nil)},
			{"arguments swapped onto failure", func() json.RawMessage {
				failed := validHookResultEntry(hookFailed)
				failedRaw, err := encodeHookResultEntry(failed)
				if err != nil {
					t.Fatalf("encode failed: %v", err)
				}
				return setKey(failedRaw, "arguments", json.RawMessage(`{}`))
			}()},
		} {
			t.Run(mutation.name, func(t *testing.T) {
				env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryHookResult, Sequence: 1, CommittedAt: testTime, Payload: mutation.payload}
				if _, err := decodeHookResultEntry(env); err == nil {
					t.Fatalf("expected rejection")
				}
			})
		}
	})
}

// TestHookActiveEffectCodecRules proves the active-effect codec rules: a hook
// effect requires its hook ID and the matching first pending call, while the
// model and tool effects forbid a hook ID.
func TestHookActiveEffectCodecRules(t *testing.T) {
	op := validOperationRecord()
	assistant := validAssistantEntry(testOpID)
	call := validToolCallRecord()
	call.ResultEntryID = testResultID
	assistant.ToolCalls = []toolCallRecord{call}
	op.State.PendingToolCalls = []PendingToolCall{{
		AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: testEntryID},
		CallID:         "call-1",
		ResultEntryID:  testResultID,
	}}

	valid := op
	valid.State.ActiveEffect = &ActiveEffect{Kind: EffectHook, ResultEntryID: hexID(9), ToolCallID: "call-1", HookID: "cap.hook.one"}
	if _, err := encodeOperationRegister(valid); err != nil {
		t.Fatalf("encode valid hook effect: %v", err)
	}

	for _, tc := range []struct {
		name    string
		effect  ActiveEffect
		wantErr string
	}{
		{"hook effect without a hook id", ActiveEffect{Kind: EffectHook, ResultEntryID: hexID(9), ToolCallID: "call-1"}, "hook active effect requires its hook id"},
		{"hook effect without a tool call id", ActiveEffect{Kind: EffectHook, ResultEntryID: hexID(9), HookID: "cap.hook.one"}, "hook active effect requires its tool call id"},
		{"hook effect on a non-pending call", ActiveEffect{Kind: EffectHook, ResultEntryID: hexID(9), ToolCallID: "call-9", HookID: "cap.hook.one"}, "hook active effect must address the matching first pending call"},
		{"model effect with a hook id", ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(9), HookID: "cap.hook.one"}, "model active effect omits the hook id"},
		{"tool effect with a hook id", ActiveEffect{Kind: EffectTool, ResultEntryID: testResultID, ToolCallID: "call-1", HookID: "cap.hook.one"}, "tool active effect omits the hook id"},
		{"unknown kind", ActiveEffect{Kind: "widget", ResultEntryID: hexID(9), ToolCallID: "call-1", HookID: "cap.hook.one"}, `active effect kind "widget" is not one of model, tool or hook`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := op
			bad.State.ActiveEffect = &tc.effect
			if _, err := encodeOperationRegister(bad); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("encode = %v, want %q", err, tc.wantErr)
			}
			// decode-side: the persisted active_effect member with the same
			// shape rejects through decodeOperationRegister; a null hook_id
			// member (absence encodes by omission) rejects too.
			raw, err := encodeOperationRegister(op)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			effectJSON, merr := json.Marshal(tc.effect)
			if merr != nil {
				t.Fatalf("marshal effect: %v", merr)
			}
			if tc.name == "hook effect without a hook id" {
				effectJSON = json.RawMessage(`{"kind":"hook","result_entry_id":"` + hexID(9) + `","tool_call_id":"call-1","hook_id":null}`)
			}
			stateObj := mustState(t, raw)
			patchedState := setKey(stateObj, "active_effect", effectJSON)
			full := setKey(raw, "state", patchedState)
			if _, err := decodeOperationRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: full}); err == nil {
				t.Fatalf("expected rejection of %s", tc.name)
			}
		})
	}
}

// TestRegisterPayloadRoundTrip proves both register payloads round-trip and
// keep revision as envelope metadata only.
func TestRegisterPayloadRoundTrip(t *testing.T) {
	session := validSessionRecord()
	sessionRaw, err := encodeSessionRegister(session)
	if err != nil {
		t.Fatalf("encode session register: %v", err)
	}
	sessionReg := Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Revision: 7, Payload: sessionRaw}
	decoded, err := decodeSessionRegister(sessionReg)
	if err != nil {
		t.Fatalf("decode session register: %v", err)
	}
	if decoded.Revision != 7 || decoded.Identity != session.Identity {
		t.Fatalf("decoded session register %+v does not round-trip", decoded)
	}
	if decoded.State.Lifecycle != session.State.Lifecycle ||
		decoded.State.CurrentAgentType != session.State.CurrentAgentType ||
		decoded.State.CurrentOperationID != session.State.CurrentOperationID ||
		decoded.State.ArchivedAt != nil ||
		!decoded.State.LastActivity.Equal(session.State.LastActivity) ||
		!usageTotalsEqual(decoded.State.Usage, session.State.Usage) {
		t.Fatalf("decoded session state %+v does not round-trip", decoded.State)
	}
	if strings.Contains(string(sessionRaw), `"revision"`) {
		t.Fatalf("revision leaked into the session payload: %s", sessionRaw)
	}

	operation := validOperationRecord()
	opRaw, err := encodeOperationRegister(operation)
	if err != nil {
		t.Fatalf("encode operation register: %v", err)
	}
	opReg := Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Revision: 3, Payload: opRaw}
	decodedOp, err := decodeOperationRegister(opReg)
	if err != nil {
		t.Fatalf("decode operation register: %v", err)
	}
	if decodedOp.Revision != 3 ||
		decodedOp.Admission.SessionID != operation.Admission.SessionID ||
		decodedOp.Admission.OperationID != operation.Admission.OperationID ||
		decodedOp.Admission.AgentType != operation.Admission.AgentType ||
		!decodedOp.Admission.AdmittedAt.Equal(operation.Admission.AdmittedAt) ||
		decodedOp.State.Status != operation.State.Status ||
		len(decodedOp.State.PendingToolCalls) != len(operation.State.PendingToolCalls) {
		t.Fatalf("decoded operation register does not round-trip: %+v", decodedOp)
	}
	if strings.Contains(string(opRaw), `"revision"`) {
		t.Fatalf("revision leaked into the operation payload: %s", opRaw)
	}
}

// TestOperationStateEncodingShape pins the state section's encoding shape
// directly: an absent pending array encodes as the required non-null empty
// array, optional records encode by omission, and a model active effect never
// carries the tool-effect-only tool_call_id key.
func TestOperationStateEncodingShape(t *testing.T) {
	v := validOperationRecord()
	v.State.PendingToolCalls = nil // nil is a valid decoded shape for a quiet running Operation
	raw, err := encodeOperationRegister(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(raw), `"pending_tool_calls":null`) {
		t.Fatalf("pending_tool_calls encoded as null: %s", raw)
	}
	obj := wireObject(t, raw)
	stateObj, err := objectMember(obj, "state", true)
	if err != nil {
		t.Fatalf("state member: %v", err)
	}
	pending, err := arrayMember(stateObj, "pending_tool_calls", true)
	if err != nil {
		t.Fatalf("pending_tool_calls: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending_tool_calls = %v, want the empty array", pending)
	}
	for _, optional := range []string{"settled_at", "active_effect", "terminal"} {
		if _, present := stateObj[optional]; present {
			t.Fatalf("running state carries optional member %q", optional)
		}
	}
	if _, err := decodeOperationRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: raw}); err != nil {
		t.Fatalf("nil-pending state does not round-trip: %v", err)
	}

	effect := validOperationRecord()
	effect.State.ActiveEffect = &ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(3)} // model effect: no tool call id
	raw, err = encodeOperationRegister(effect)
	if err != nil {
		t.Fatalf("encode model effect: %v", err)
	}
	stateObj, err = objectMember(wireObject(t, raw), "state", true)
	if err != nil {
		t.Fatalf("state member: %v", err)
	}
	effectObj, err := objectMember(stateObj, "active_effect", true)
	if err != nil {
		t.Fatalf("active_effect: %v", err)
	}
	if _, present := effectObj["tool_call_id"]; present {
		t.Fatalf("model active effect carries tool_call_id: %s", raw)
	}
}

// TestRegisterPayloadRejectsInvalidWire proves the register payloads enforce
// exact keys, containers, and envelope-key identity agreement.
func TestRegisterPayloadRejectsInvalidWire(t *testing.T) {
	sessionRaw, err := encodeSessionRegister(validSessionRecord())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	opRaw, err := encodeOperationRegister(validOperationRecord())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	sessionMutations := []struct {
		name    string
		payload json.RawMessage
	}{
		{"unknown key", setKey(sessionRaw, "bogus", json.RawMessage(`1`))},
		{"miscased key", renameKey(sessionRaw, "identity", "Identity")},
		{"null identity", setKey(sessionRaw, "identity", json.RawMessage(`null`))},
		{"wrong identity container", setKey(sessionRaw, "identity", json.RawMessage(`[]`))},
		{"null state", setKey(sessionRaw, "state", json.RawMessage(`null`))},
		{"identity mismatch", setKey(sessionRaw, "identity", json.RawMessage(`{"session_id":"`+otherSession()+`","workspace":"/tmp/works","created_at":"2026-01-02T03:04:05.123456789Z"}`))},
	}
	for _, m := range sessionMutations {
		t.Run("session/"+m.name, func(t *testing.T) {
			reg := Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Payload: m.payload}
			if _, err := decodeSessionRegister(reg); err == nil {
				t.Fatalf("expected rejection")
			}
		})
	}
	opMutations := []struct {
		name    string
		payload json.RawMessage
	}{
		{"unknown key", setKey(opRaw, "bogus", json.RawMessage(`1`))},
		{"miscased key", renameKey(opRaw, "admission", "Admission")},
		{"null admission", setKey(opRaw, "admission", json.RawMessage(`null`))},
		{"wrong state container", setKey(opRaw, "state", json.RawMessage(`[]`))},
		{"operation mismatch", setKey(opRaw, "admission", json.RawMessage(`{"session_id":"`+testSessionID+`","operation_id":"ghost","request_kind":"message","admitted_entry":{"session_id":"`+testSessionID+`","entry_id":"`+hexID(1)+`"},"agent_type":"coder","execution":{"configuration_revision":"rev-1","model":{"provider":"prov","model":"gpt-x"},"context_window":4096,"output_reserve":2048,"system_prompt":"system","tools":[],"readonly":false,"write_dir":"","compact":{"model":{"provider":"cprov","model":"compact-x"},"context_window":2048,"output_reserve":1024,"system_prompt":"summarize"}},"admitted_at":"2026-01-02T03:04:05.123456789Z"}`))},
	}
	for _, m := range opMutations {
		t.Run("operation/"+m.name, func(t *testing.T) {
			reg := Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: m.payload}
			if _, err := decodeOperationRegister(reg); err == nil {
				t.Fatalf("expected rejection")
			}
		})
	}
}

// TestModelRefDurableShape proves the durable two-field model identity: refs
// with slashes in either field round-trip exactly, combined-string and
// zero/partial forms are rejected, and the wire stays the private two-field
// object.
func TestModelRefDurableShape(t *testing.T) {
	raw, err := encodeModelRef(model.ModelRef{Provider: "a/b", Model: "c/d"})
	if err != nil {
		t.Fatalf("encode slashed ref: %v", err)
	}
	if string(raw) != `{"provider":"a/b","model":"c/d"}` {
		t.Fatalf("durable model ref wire = %s, want the two-field object", raw)
	}
	obj, err := decodePayloadObject(raw)
	if err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	ref, err := decodeModelRef(obj)
	if err != nil {
		t.Fatalf("decode slashed ref: %v", err)
	}
	if ref.Provider != "a/b" || ref.Model != "c/d" {
		t.Fatalf("slashed ref decoded as %q/%q", ref.Provider, ref.Model)
	}

	if _, err := encodeModelRef(model.ModelRef{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero ref encode = %v, want ErrInvalid", err)
	}
	if _, err := encodeModelRef(model.ModelRef{Provider: "prov"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("partial ref encode = %v, want ErrInvalid", err)
	}
	for _, bad := range []json.RawMessage{
		json.RawMessage(`"prov/gpt-x"`),        // combined-string coercion
		json.RawMessage(`{}`),                  // zero
		json.RawMessage(`{"provider":"prov"}`), // partial
		json.RawMessage(`null`),
		json.RawMessage(`{"provider":"","model":"m"}`), // empty fields
	} {
		if _, err := decodeModelRefFromRaw(bad); err == nil {
			t.Fatalf("model ref %s must be rejected", bad)
		}
	}
}

// decodeModelRefFromRaw decodes a model reference from its raw member bytes.
func decodeModelRefFromRaw(raw json.RawMessage) (model.ModelRef, error) {
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return model.ModelRef{}, err
	}
	return decodeModelRef(obj)
}

// TestToolArgumentsRoundTripByteExact proves raw tool-call arguments persist
// as base64 and round-trip malformed and non-UTF-8 bytes exactly, with no
// JSON validation applied to them.
func TestToolArgumentsRoundTripByteExact(t *testing.T) {
	raw := []byte{0xff, 0xfe, 0x00, '{', '"', 0x80, '}'}
	call := validToolCallRecord()
	call.ArgumentsBase64 = base64.StdEncoding.EncodeToString(raw)
	entry := validAssistantEntry(testOpID)
	entry.ToolCalls = []toolCallRecord{call}
	rawWire, err := encodeAssistantEntry(entry)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: rawWire}
	decoded, err := decodeAssistantEntry(env)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, err := base64.StdEncoding.Strict().DecodeString(decoded.ToolCalls[0].ArgumentsBase64)
	if err != nil {
		t.Fatalf("stored arguments_base64 does not decode: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("arguments round-tripped as %x, want %x", got, raw)
	}
}

// TestUsageAccounting proves checked signed usage addition: positive and
// negative counts sum, overflow returns the storage-failure class with no
// partial result, and totals stay unique and lexicographically sorted.
func TestUsageAccounting(t *testing.T) {
	sum, err := addUsageCount(UsageCount{InputTokens: 5, OutputTokens: -2}, UsageCount{InputTokens: 7, CachedInputTokens: 3, OutputTokens: 2})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if sum != (UsageCount{InputTokens: 12, CachedInputTokens: 3, OutputTokens: 0}) {
		t.Fatalf("signed sum = %+v", sum)
	}
	maxed := UsageCount{InputTokens: math.MaxInt64}
	if out, err := addUsageCount(maxed, UsageCount{InputTokens: 1}); !errors.Is(err, ErrStorage) {
		t.Fatalf("overflow = (%+v, %v), want ErrStorage", out, err)
	} else if out != (UsageCount{}) {
		t.Fatalf("overflow published a partial result %+v", out)
	}
	if out, err := addUsageCount(UsageCount{InputTokens: math.MinInt64}, UsageCount{InputTokens: -1}); !errors.Is(err, ErrStorage) {
		t.Fatalf("negative overflow = (%+v, %v), want ErrStorage", out, err)
	}

	a := UsageTotals{ByModel: []ModelUsage{
		{Model: model.ModelRef{Provider: "z", Model: "m"}, Usage: UsageCount{InputTokens: 1}},
	}}
	b := UsageTotals{ByModel: []ModelUsage{
		{Model: model.ModelRef{Provider: "a", Model: "x"}, Usage: UsageCount{InputTokens: 2}},
		{Model: model.ModelRef{Provider: "z", Model: "m"}, Usage: UsageCount{OutputTokens: 4}},
		{Model: model.ModelRef{Provider: "a", Model: "b"}, Usage: UsageCount{}},
	}}
	merged, err := addUsageTotals(a, b)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	want := []ModelUsage{
		{Model: model.ModelRef{Provider: "a", Model: "b"}, Usage: UsageCount{}},
		{Model: model.ModelRef{Provider: "a", Model: "x"}, Usage: UsageCount{InputTokens: 2}},
		{Model: model.ModelRef{Provider: "z", Model: "m"}, Usage: UsageCount{InputTokens: 1, OutputTokens: 4}},
	}
	if len(merged.ByModel) != len(want) {
		t.Fatalf("merged %d entries, want %d", len(merged.ByModel), len(want))
	}
	for i := range want {
		if merged.ByModel[i] != want[i] {
			t.Fatalf("merged[%d] = %+v, want %+v", i, merged.ByModel[i], want[i])
		}
	}
	if _, err := addUsageTotals(UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: maxed}}}, UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: UsageCount{InputTokens: 1}}}}); !errors.Is(err, ErrStorage) {
		t.Fatalf("totals overflow = %v, want ErrStorage", err)
	}

	// Decoded totals reject duplicates and unsorted orders.
	for _, bad := range []UsageTotals{
		{ByModel: []ModelUsage{
			{Model: model.ModelRef{Provider: "a", Model: "x"}, Usage: UsageCount{}},
			{Model: model.ModelRef{Provider: "a", Model: "x"}, Usage: UsageCount{}},
		}},
		{ByModel: []ModelUsage{
			{Model: model.ModelRef{Provider: "b", Model: "x"}, Usage: UsageCount{}},
			{Model: model.ModelRef{Provider: "a", Model: "x"}, Usage: UsageCount{}},
		}},
	} {
		if err := validateUsageTotals(bad); err == nil {
			t.Fatalf("totals %+v must be rejected", bad)
		}
	}
}

// TestToolCallNormalizedArgumentsNullRejected proves the null-literal rule
// for raw JSON members is shared by both codec sides: decode rejects the null
// literal through the codec-wide member rule, so encode must reject it too,
// while a non-null JSON value round-trips verbatim.
func TestToolCallNormalizedArgumentsNullRejected(t *testing.T) {
	entry := validAssistantEntry(testOpID)
	call := validToolCallRecord()
	call.NormalizedArguments = json.RawMessage("null")
	entry.ToolCalls = []toolCallRecord{call}
	if _, err := encodeAssistantEntry(entry); err == nil {
		t.Fatalf("encoding normalized_arguments null must fail")
	} else if !errors.Is(err, ErrInvalid) {
		t.Fatalf("rejection = %v, want the ErrInvalid class", err)
	}

	call.NormalizedArguments = json.RawMessage(`{"x":1}`)
	entry.ToolCalls = []toolCallRecord{call}
	raw, err := encodeAssistantEntry(entry)
	if err != nil {
		t.Fatalf("encode non-null normalized_arguments: %v", err)
	}
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: raw}
	decoded, err := decodeAssistantEntry(env)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(decoded.ToolCalls[0].NormalizedArguments) != `{"x":1}` {
		t.Fatalf("normalized_arguments round-tripped as %s", decoded.ToolCalls[0].NormalizedArguments)
	}
}

// TestToolCallNormalizedArgumentsObjectOnly proves the normalized_arguments
// member is one complete JSON object on both codec sides: a persisted
// non-object member (an array representative — every wrong kind fails the
// same object check) is rejected by decode, so it surfaces as Session
// corruption and never reaches the tool boundary as executable input, and
// rejected by encode with the invalid-input class. The explicit null case is
// shared with TestToolCallNormalizedArgumentsNullRejected.
func TestToolCallNormalizedArgumentsObjectOnly(t *testing.T) {
	const wrong = `[1,2]`
	entry := validAssistantEntry(testOpID)
	call := validToolCallRecord()
	call.NormalizedArguments = json.RawMessage(wrong)
	entry.ToolCalls = []toolCallRecord{call}
	if _, err := encodeAssistantEntry(entry); !errors.Is(err, ErrInvalid) {
		t.Fatalf("encode normalized_arguments %s = %v, want the ErrInvalid class", wrong, err)
	}

	// decode side: rewrite one valid encoded payload's member to the wrong
	// kind and prove decode rejects it.
	valid := validAssistantEntry(testOpID)
	vcall := validToolCallRecord()
	vcall.NormalizedArguments = json.RawMessage(`{"x":1}`)
	valid.ToolCalls = []toolCallRecord{vcall}
	raw, err := encodeAssistantEntry(valid)
	if err != nil {
		t.Fatalf("encode valid assistant: %v", err)
	}
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("encoded payload is not an object: %v", err)
	}
	items := []map[string]json.RawMessage{}
	if err := json.Unmarshal(obj["tool_calls"], &items); err != nil || len(items) != 1 {
		t.Fatalf("encoded tool_calls = %s (%v)", obj["tool_calls"], err)
	}
	items[0]["normalized_arguments"] = json.RawMessage(wrong)
	edited, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal edited tool_calls: %v", err)
	}
	obj["tool_calls"] = edited
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal edited payload: %v", err)
	}
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: out}
	if _, err := decodeAssistantEntry(env); err == nil {
		t.Fatalf("decode normalized_arguments %s must fail", wrong)
	}
}

// TestToolResultMetadataRules proves the durable metadata member of one
// tool-result payload: one well-formed bounded value round-trips byte-
// identical into an owned copy, absence encodes by omission, and null,
// malformed, or oversized values fail their encode with the invalid-input
// class.
func TestToolResultMetadataRules(t *testing.T) {
	valid := json.RawMessage(`{"kind":"editpreview","n":[1,2]}`)
	v := validToolResultEntry(testOpID)
	v.Metadata = valid
	raw, err := encodeToolResultEntry(v)
	if err != nil {
		t.Fatalf("encode metadata: %v", err)
	}
	if _, present := wireObject(t, raw)["metadata"]; !present {
		t.Fatalf("encoded payload %s carries no metadata member", raw)
	}
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryToolResult, Payload: raw}
	decoded, err := decodeToolResultEntry(env)
	if err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if string(decoded.Metadata) != string(valid) {
		t.Fatalf("metadata round-tripped as %s, want the stored bytes verbatim", decoded.Metadata)
	}
	decoded.Metadata[0] = '[' // mutating the decoded copy must not touch stored state
	again, err := decodeToolResultEntry(env)
	if err != nil {
		t.Fatalf("second decode: %v", err)
	}
	if string(again.Metadata) != string(valid) {
		t.Fatalf("metadata after mutating a decoded copy = %s, want the stored bytes", again.Metadata)
	}

	// Absence encodes by omission: no metadata member appears at all.
	bare, err := encodeToolResultEntry(validToolResultEntry(testOpID))
	if err != nil {
		t.Fatalf("encode result without metadata: %v", err)
	}
	if _, present := wireObject(t, bare)["metadata"]; present {
		t.Fatalf("metadata-less payload encodes the member: %s", bare)
	}

	rejected := []struct {
		name     string
		metadata json.RawMessage
	}{
		{"null metadata", json.RawMessage(`null`)},
		{"malformed metadata", json.RawMessage(`{broken`)},
		{"oversized metadata", json.RawMessage(`"` + strings.Repeat("x", maxToolMetadataBytes) + `"`)},
		{"raw-at-bound metadata whose HTML-escaped durable encoding exceeds the bound", json.RawMessage(`"` + strings.Repeat("<", maxToolMetadataBytes-2) + `"`)},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			v := validToolResultEntry(testOpID)
			v.Metadata = tc.metadata
			if _, err := encodeToolResultEntry(v); !errors.Is(err, ErrInvalid) {
				t.Fatalf("encode error = %v, want the ErrInvalid class", err)
			}
		})
	}

	// A persisted payload that violates the member rules stays undecodable:
	// null and oversized members are rejected, while a value exactly at the
	// bound decodes.
	rawValid := raw
	if _, err := decodeToolResultEntry(Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryToolResult, Payload: rawValid}); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	for _, tc := range []struct {
		name     string
		metadata json.RawMessage
	}{
		{"null", json.RawMessage(`null`)},
		{"oversized", json.RawMessage(`"` + strings.Repeat("x", maxToolMetadataBytes+1) + `"`)},
		{"at the bound", json.RawMessage(`"` + strings.Repeat("x", maxToolMetadataBytes-2) + `"`)},
	} {
		env := env
		env.Payload = setKey(raw, "metadata", tc.metadata)
		_, err := decodeToolResultEntry(env)
		if tc.name == "at the bound" {
			if err != nil {
				t.Fatalf("metadata exactly at the bound rejected: %v", err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("persisted %s metadata decoded, want rejection", tc.name)
		}
	}
}

// TestOwnedToolMetadataEncodedForm proves the shared owning helper returns
// the exact durable encoding json.Marshal produces for the raw input —
// whitespace compacts, HTML characters escape, numeric lexemes stay exact —
// never a clone of the caller's raw bytes: the caller's buffer stays
// unchanged and the returned bytes are independent of it.
func TestOwnedToolMetadataEncodedForm(t *testing.T) {
	const input = `{ "n" : [1, 1.0, 1e0, 9007199254740993], "s" : "<>&" }`
	const want = `{"n":[1,1.0,1e0,9007199254740993],"s":"\u003c\u003e\u0026"}`
	raw := json.RawMessage(input)
	got := ownedToolMetadata(raw)
	if string(got) != want {
		t.Fatalf("owned metadata = %s, want the durable encoded bytes %s", got, want)
	}
	if string(raw) != input {
		t.Fatalf("caller input changed to %s, want it unchanged", raw)
	}
	raw[1] = 'x' // mutating the caller's buffer cannot alter the returned bytes
	if string(got) != want {
		t.Fatalf("owned metadata after mutating the caller = %s, want the independent encoded bytes %s", got, want)
	}
}

// TestCodecRejectsInvalidValues proves validate-before-encoding: every
// invalid durable value fails its encode with the invalid-input class, and
// the unsupported kinds use it before persistence while their stored records
// would surface as corruption.
func TestCodecRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		encode func() error
	}{
		{"input bad origin", func() error {
			v := validInputEntry(testOpID)
			v.Origin = InputOrigin("pending")
			_, err := encodeInputEntry(v)
			return err
		}},
		{"assistant empty partial", func() error {
			v := validAssistantEntry(testOpID)
			v.Status = model.OutputErrored
			v.Content = []model.ContentPart{}
			_, err := encodeAssistantEntry(v)
			return err
		}},
		{"assistant errored with calls", func() error {
			v := validAssistantEntry(testOpID)
			v.Status = model.OutputErrored
			v.Refusal = "no"
			v.ToolCalls = []toolCallRecord{validToolCallRecord()}
			_, err := encodeAssistantEntry(v)
			return err
		}},
		{"assistant call ordinal mismatch", func() error {
			v := validAssistantEntry(testOpID)
			call := validToolCallRecord()
			call.Ordinal = 1
			v.ToolCalls = []toolCallRecord{call}
			_, err := encodeAssistantEntry(v)
			return err
		}},
		{"assistant duplicate call ids", func() error {
			v := validAssistantEntry(testOpID)
			call := validToolCallRecord()
			v.ToolCalls = []toolCallRecord{call, call}
			_, err := encodeAssistantEntry(v)
			return err
		}},
		{"assistant incomplete source", func() error {
			v := validAssistantEntry(testOpID)
			v.Source = model.ModelRef{Provider: "prov"}
			_, err := encodeAssistantEntry(v)
			return err
		}},
		{"tool result denied without content", func() error {
			v := validToolResultEntry(testOpID)
			v.Status = model.ResultDenied
			v.Content = ""
			_, err := encodeToolResultEntry(v)
			return err
		}},
		{"signal wrong content", func() error {
			v := validSignalEntry(testOpID)
			v.Signal = SignalModelFailureContinuation
			_, err := encodeSignalEntry(v)
			return err
		}},
		{"settlement running status", func() error {
			v := validSettlementEntry()
			v.Status = OperationRunning
			_, err := encodeOperationSettlementEntry(v)
			return err
		}},
		{"settlement model without usage", func() error {
			v := validSettlementEntry()
			v.Status = OperationFailure
			v.Detail = "d"
			ref := testModelRef()
			v.Model = &ref
			_, err := encodeOperationSettlementEntry(v)
			return err
		}},
		{"session root with half lineage", func() error {
			v := validSessionRecord()
			v.Identity.SourceSessionID = hexID(9)
			_, err := encodeSessionRegister(v)
			return err
		}},
		{"session unclean workspace", func() error {
			v := validSessionRecord()
			v.Identity.Workspace = "/tmp/works/../works"
			_, err := encodeSessionRegister(v)
			return err
		}},
		{"session relative workspace", func() error {
			v := validSessionRecord()
			v.Identity.Workspace = "relative/path"
			_, err := encodeSessionRegister(v)
			return err
		}},
		{"session open with archived_at", func() error {
			v := validSessionRecord()
			stamped := testTime
			v.State.ArchivedAt = &stamped
			_, err := encodeSessionRegister(v)
			return err
		}},
		{"session archived without archived_at", func() error {
			v := validSessionRecord()
			v.State.Lifecycle = LifecycleArchived
			_, err := encodeSessionRegister(v)
			return err
		}},
		{"session empty agent type", func() error {
			v := validSessionRecord()
			v.State.CurrentAgentType = ""
			_, err := encodeSessionRegister(v)
			return err
		}},
		{"operation running with terminal", func() error {
			v := validOperationRecord()
			stamped := testTime
			v.State.SettledAt = &stamped
			v.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(5)}}
			_, err := encodeOperationRegister(v)
			return err
		}},
		{"operation success with detail", func() error {
			v := validOperationRecord()
			stamped := testTime
			v.State.Status = OperationSuccess
			v.State.SettledAt = &stamped
			v.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(5)}, Detail: "d"}
			_, err := encodeOperationRegister(v)
			return err
		}},
		{"operation failure without detail", func() error {
			v := validOperationRecord()
			stamped := testTime
			v.State.Status = OperationFailure
			v.State.SettledAt = &stamped
			v.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(5)}}
			_, err := encodeOperationRegister(v)
			return err
		}},
		{"operation model effect with call id", func() error {
			v := validOperationRecord()
			v.State.ActiveEffect = &ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(3), ToolCallID: "call-1"}
			_, err := encodeOperationRegister(v)
			return err
		}},
		{"operation tool effect without first pending call", func() error {
			v := validOperationRecord()
			v.State.ActiveEffect = &ActiveEffect{Kind: EffectTool, ResultEntryID: hexID(3), ToolCallID: "call-1"}
			_, err := encodeOperationRegister(v)
			return err
		}},
		{"operation duplicate pending results", func() error {
			v := validOperationRecord()
			v.State.PendingToolCalls = []PendingToolCall{
				{AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)}, CallID: "c1", ResultEntryID: hexID(3)},
				{AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)}, CallID: "c2", ResultEntryID: hexID(3)},
			}
			_, err := encodeOperationRegister(v)
			return err
		}},
		{"capture incomplete model", func() error {
			v := testCapture()
			v.Model = model.ModelRef{Provider: "prov"}
			_, err := encodeExecutionCapture(v)
			return err
		}},
		{"capture duplicate tool names", func() error {
			v := testCapture()
			v.Tools = []model.ToolDefinition{testToolDefinition(), testToolDefinition()}
			_, err := encodeExecutionCapture(v)
			return err
		}},
		{"capture empty revision", func() error {
			v := testCapture()
			v.ConfigurationRevision = ""
			_, err := encodeExecutionCapture(v)
			return err
		}},
		{"capture duplicate capability name", func() error {
			v := testCapture()
			v.Capabilities = []string{"cap-a", "cap-b", "cap-a"}
			_, err := encodeExecutionCapture(v)
			return err
		}},
		{"capture empty capability name", func() error {
			v := testCapture()
			v.Capabilities = []string{""}
			_, err := encodeExecutionCapture(v)
			return err
		}},
		{"tool active effect with mismatched reservation", func() error {
			v := validOperationRecord()
			v.State.ActiveEffect = &ActiveEffect{Kind: EffectTool, ResultEntryID: hexID(3), ToolCallID: "call-1"}
			v.State.PendingToolCalls = []PendingToolCall{{
				AssistantEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(2)},
				CallID:         "call-1",
				ResultEntryID:  hexID(4),
			}}
			_, err := encodeOperationRegister(v)
			return err
		}},
		{"active effect with non-hex result id", func() error {
			v := validOperationRecord()
			v.State.ActiveEffect = &ActiveEffect{Kind: EffectModel, ResultEntryID: "ghost-result"}
			_, err := encodeOperationRegister(v)
			return err
		}},
		{"terminal record with garbage settlement reference", func() error {
			v := validOperationRecord()
			stamped := testTime
			v.State.Status = OperationSuccess
			v.State.SettledAt = &stamped
			v.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: "garbage", EntryID: hexID(3)}}
			_, err := encodeOperationRegister(v)
			return err
		}},
		{"success settlement carrying detail", func() error {
			v := validSettlementEntry()
			v.Detail = "stray"
			_, err := encodeOperationSettlementEntry(v)
			return err
		}},
		{"failure settlement missing detail", func() error {
			v := validSettlementEntry()
			v.Status = OperationFailure
			_, err := encodeOperationSettlementEntry(v)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.encode()
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("rejection = %v, want the ErrInvalid class", err)
			}
		})
	}
}

// TestCodecOwnedValues proves decoded values are independent owned copies:
// mutating one decode result never reaches another, and payload bytes are
// cloned away from the caller's buffer.
func TestCodecOwnedValues(t *testing.T) {
	entry := validAssistantEntry(testOpID)
	entry.Extra = model.Extra{"k": json.RawMessage(`{"deep":[1]}`)}
	entry.ToolCalls = []toolCallRecord{validToolCallRecord()}
	raw, err := encodeAssistantEntry(entry)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: raw}
	first, err := decodeAssistantEntry(env)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	second, err := decodeAssistantEntry(env)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	first.Extra["k"] = json.RawMessage(`"mutated"`)
	first.Content = append(first.Content, model.ContentPart{Kind: model.PartText, Text: "extra"})
	first.ToolCalls[0].ID = "mutated"
	if string(second.Extra["k"]) != `{"deep":[1]}` || len(second.Content) != 1 || second.ToolCalls[0].ID != "call-1" {
		t.Fatalf("decoded values alias each other")
	}

	// Mutating the caller's payload buffer after decode must never reach the
	// decoded owned values.
	env.Payload[0] = ' '
	if string(second.Extra["k"]) != `{"deep":[1]}` || second.Content[0].Text != "hi" {
		t.Fatalf("decoded value aliased the caller payload buffer")
	}
}

// TestTimestampRules proves durable register timestamps: encoded from UTC in
// the landed RFC3339Nano layout, zero rejected, and decoded offsets other
// than UTC rejected.
func TestTimestampRules(t *testing.T) {
	stamped, err := encodeTime(testTime)
	if err != nil {
		t.Fatalf("encodeTime: %v", err)
	}
	if stamped != "2026-01-02T03:04:05.123456789Z" {
		t.Fatalf("encoded timestamp = %q", stamped)
	}
	if _, err := encodeTime(time.Time{}); err == nil {
		t.Fatalf("zero timestamp encode must fail")
	}
	if _, err := encodeTime(time.Date(12000, 1, 2, 3, 4, 5, 0, time.UTC)); err == nil {
		t.Fatalf("timestamp with year 12000 encode must fail")
	}
	decoded, err := decodeTime(stamped)
	if err != nil {
		t.Fatalf("decodeTime: %v", err)
	}
	if !decoded.UTC().Equal(testTime) {
		t.Fatalf("decoded timestamp = %v", decoded)
	}
	if _, err := decodeTime("2026-01-02T03:04:05.123456789+02:00"); err == nil {
		t.Fatalf("non-UTC offset must be rejected")
	}
	if _, err := decodeTime("0001-01-01T00:00:00Z"); err == nil {
		t.Fatalf("zero timestamp must be rejected")
	}
}

// TestIDShapes proves durable identity validation: session, entry, and result
// IDs are exactly 32 lowercase hexadecimal characters; Operation and tool-call
// identities are non-empty opaque strings.
func TestIDShapes(t *testing.T) {
	if err := validateHexID(hexID(1), "id"); err != nil {
		t.Fatalf("valid hex id rejected: %v", err)
	}
	for _, bad := range []string{"", strings.Repeat("A", 32), strings.Repeat("g", 32), hexID(1) + "0", hexID(1)[:31]} {
		if err := validateHexID(bad, "id"); err == nil {
			t.Fatalf("id %q must be rejected", bad)
		}
	}
	if err := validateOperationIdentity("", "operation id"); err == nil {
		t.Fatalf("empty operation id must be rejected")
	}
	if err := validateOperationIdentity("any opaque caller identity", "operation id"); err != nil {
		t.Fatalf("opaque operation id rejected: %v", err)
	}
}

// TestExecutionCaptureCapabilities pins the durable capability selection: an
// empty selection encodes by omission; omission and the empty array both
// decode to none while null is invalid; present names must be nonempty and
// unique and keep the selected order; and a historical capture carrying
// plugin-defined names decodes without any plugin definition.
func TestExecutionCaptureCapabilities(t *testing.T) {
	encode := func(t *testing.T, capabilities []string) json.RawMessage {
		t.Helper()
		v := testCapture()
		v.Capabilities = capabilities
		raw, err := encodeExecutionCapture(v)
		if err != nil {
			t.Fatalf("encodeExecutionCapture: %v", err)
		}
		return raw
	}

	for _, empty := range [][]string{nil, {}} {
		obj, err := decodePayloadObject(encode(t, empty))
		if err != nil {
			t.Fatalf("decodePayloadObject: %v", err)
		}
		if _, present := obj["capabilities"]; present {
			t.Fatalf("empty capability selection %#v leaked the capabilities member", empty)
		}
	}

	ordered := []string{"retired-plugin.capability", "cap-mid", "cap-last"}
	obj, err := decodePayloadObject(encode(t, ordered))
	if err != nil {
		t.Fatalf("decodePayloadObject: %v", err)
	}
	if got := string(obj["capabilities"]); got != `["retired-plugin.capability","cap-mid","cap-last"]` {
		t.Fatalf("encoded capabilities member = %s", got)
	}
	decoded, err := decodeExecutionCapture(obj)
	if err != nil {
		t.Fatalf("historical capture without plugin definitions must decode: %v", err)
	}
	if len(decoded.Capabilities) != 3 || decoded.Capabilities[0] != "retired-plugin.capability" ||
		decoded.Capabilities[1] != "cap-mid" || decoded.Capabilities[2] != "cap-last" {
		t.Fatalf("decoded capabilities = %q, want the selected order", decoded.Capabilities)
	}
	decoded.Capabilities[0] = "tampered"
	again, err := decodeExecutionCapture(obj)
	if err != nil || again.Capabilities[0] != "retired-plugin.capability" {
		t.Fatalf("decoded capabilities alias the payload bytes: %q, %v", again.Capabilities, err)
	}

	captureWith := func(capabilities string) map[string]json.RawMessage {
		members, err := decodePayloadObject(encode(t, nil))
		if err != nil {
			t.Fatalf("decodePayloadObject: %v", err)
		}
		if capabilities != "" {
			members["capabilities"] = json.RawMessage(capabilities)
		}
		return members
	}
	if got, err := decodeExecutionCapture(captureWith("")); err != nil || got.Capabilities != nil {
		t.Fatalf("omitted capabilities = %q, %v, want none", got.Capabilities, err)
	}
	if got, err := decodeExecutionCapture(captureWith("[]")); err != nil || got.Capabilities != nil {
		t.Fatalf("empty-array capabilities = %q, %v, want none", got.Capabilities, err)
	}
	for _, bad := range []string{"null", `["cap-a","cap-a"]`, `[""]`, `[5]`, `"cap-a"`, `["cap-a",null]`} {
		if got, err := decodeExecutionCapture(captureWith(bad)); err == nil {
			t.Fatalf("capabilities %s decoded to %q, want rejection", bad, got.Capabilities)
		}
	}
}

// TestExecutionCapturePermissionMembers pins the durable permission capability
// members: readonly and write_dir are required members encoded always,
// including their false and empty values, they round-trip unchanged, and a
// capture missing either member, carrying null, or with a wrong-typed member
// is invalid.
func TestExecutionCapturePermissionMembers(t *testing.T) {
	encode := func(t *testing.T, v ExecutionCapture) map[string]json.RawMessage {
		t.Helper()
		raw, err := encodeExecutionCapture(v)
		if err != nil {
			t.Fatalf("encodeExecutionCapture: %v", err)
		}
		members, err := decodePayloadObject(raw)
		if err != nil {
			t.Fatalf("decodePayloadObject: %v", err)
		}
		return members
	}

	explicit := encode(t, testCapture())
	if got := string(explicit["readonly"]); got != "false" {
		t.Fatalf("encoded readonly member = %s, want the explicit false", got)
	}
	if got := string(explicit["write_dir"]); got != `""` {
		t.Fatalf("encoded write_dir member = %s, want the explicit empty string", got)
	}

	v := testCapture()
	v.Readonly = true
	v.WriteDir = "/w/sub"
	decoded, err := decodeExecutionCapture(encode(t, v))
	if err != nil {
		t.Fatalf("decodeExecutionCapture: %v", err)
	}
	if decoded.Readonly != true || decoded.WriteDir != "/w/sub" {
		t.Fatalf("round-tripped members = %v %q, want true and %q", decoded.Readonly, decoded.WriteDir, "/w/sub")
	}

	for _, mutation := range []struct {
		name    string
		members func(map[string]json.RawMessage)
	}{
		{"missing readonly", func(m map[string]json.RawMessage) { delete(m, "readonly") }},
		{"missing write_dir", func(m map[string]json.RawMessage) { delete(m, "write_dir") }},
		{"null readonly", func(m map[string]json.RawMessage) { m["readonly"] = json.RawMessage("null") }},
		{"null write_dir", func(m map[string]json.RawMessage) { m["write_dir"] = json.RawMessage("null") }},
		{"string readonly", func(m map[string]json.RawMessage) { m["readonly"] = json.RawMessage(`"true"`) }},
		{"number write_dir", func(m map[string]json.RawMessage) { m["write_dir"] = json.RawMessage(`5`) }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			members := encode(t, v)
			mutation.members(members)
			if got, err := decodeExecutionCapture(members); err == nil {
				t.Fatalf("capture decoded to %+v, want rejection", got)
			}
		})
	}
}

// TestExecutionCaptureCompactionMembers pins the durable compaction
// configuration members: context_window, output_reserve and compact are
// required members encoded always, the nested compact object carries exactly
// the model, context_window, output_reserve and system_prompt keys with the
// model in the durable two-field form, everything round-trips unchanged, and
// a capture missing a member, carrying a non-positive window or reserve, an
// incomplete compact model identity, or an empty compact prompt is invalid on
// both the encode and decode paths.
func TestExecutionCaptureCompactionMembers(t *testing.T) {
	encode := func(t *testing.T, v ExecutionCapture) map[string]json.RawMessage {
		t.Helper()
		raw, err := encodeExecutionCapture(v)
		if err != nil {
			t.Fatalf("encodeExecutionCapture: %v", err)
		}
		members, err := decodePayloadObject(raw)
		if err != nil {
			t.Fatalf("decodePayloadObject: %v", err)
		}
		return members
	}

	members := encode(t, testCapture())
	if got := string(members["context_window"]); got != "4096" {
		t.Fatalf("encoded context_window member = %s, want 4096", got)
	}
	if got := string(members["output_reserve"]); got != "2048" {
		t.Fatalf("encoded output_reserve member = %s, want 2048", got)
	}
	compactObj, err := decodePayloadObject(members["compact"])
	if err != nil {
		t.Fatalf("decodePayloadObject(compact): %v", err)
	}
	if err := rejectUnknownMembers(compactObj, "model", "context_window", "output_reserve", "system_prompt"); err != nil {
		t.Fatalf("encoded compact object carries unexpected members: %v", err)
	}
	for _, key := range []string{"model", "context_window", "output_reserve", "system_prompt"} {
		if _, present := compactObj[key]; !present {
			t.Fatalf("encoded compact object misses the %q member", key)
		}
	}
	if got := string(compactObj["context_window"]); got != "2048" {
		t.Fatalf("encoded compact context_window member = %s, want 2048", got)
	}
	if got := string(compactObj["output_reserve"]); got != "1024" {
		t.Fatalf("encoded compact output_reserve member = %s, want 1024", got)
	}
	compactModelObj, err := decodePayloadObject(compactObj["model"])
	if err != nil {
		t.Fatalf("decodePayloadObject(compact.model): %v", err)
	}
	compactModel, err := decodeModelRef(compactModelObj)
	if err != nil {
		t.Fatalf("encoded compact model is not a durable model reference: %v", err)
	}
	if compactModel != (model.ModelRef{Provider: "cprov", Model: "compact-x"}) {
		t.Fatalf("encoded compact model = %s, want cprov/compact-x", compactModel.String())
	}

	decoded, err := decodeExecutionCapture(members)
	if err != nil {
		t.Fatalf("decodeExecutionCapture: %v", err)
	}
	if decoded.ContextWindow != 4096 || decoded.OutputReserve != 2048 {
		t.Fatalf("round-tripped windows = %d %d, want 4096 and 2048", decoded.ContextWindow, decoded.OutputReserve)
	}
	if decoded.Compact != testCompactCapture() {
		t.Fatalf("round-tripped compact = %+v, want %+v", decoded.Compact, testCompactCapture())
	}

	for _, mutation := range []struct {
		name   string
		mutate func(*ExecutionCapture)
	}{
		{"zero context window", func(v *ExecutionCapture) { v.ContextWindow = 0 }},
		{"negative context window", func(v *ExecutionCapture) { v.ContextWindow = -5 }},
		{"zero output reserve", func(v *ExecutionCapture) { v.OutputReserve = 0 }},
		{"negative output reserve", func(v *ExecutionCapture) { v.OutputReserve = -1 }},
		{"empty compact model", func(v *ExecutionCapture) { v.Compact.Model = model.ModelRef{} }},
		{"incomplete compact model", func(v *ExecutionCapture) { v.Compact.Model = model.ModelRef{Provider: "cprov"} }},
		{"zero compact window", func(v *ExecutionCapture) { v.Compact.ContextWindow = 0 }},
		{"negative compact window", func(v *ExecutionCapture) { v.Compact.ContextWindow = -2 }},
		{"zero compact reserve", func(v *ExecutionCapture) { v.Compact.OutputReserve = 0 }},
		{"negative compact reserve", func(v *ExecutionCapture) { v.Compact.OutputReserve = -3 }},
		{"empty compact prompt", func(v *ExecutionCapture) { v.Compact.SystemPrompt = "" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			v := testCapture()
			mutation.mutate(&v)
			if _, err := encodeExecutionCapture(v); err == nil {
				t.Fatalf("capture with %s encoded, want rejection", mutation.name)
			}
		})
	}

	for _, missing := range []string{"context_window", "output_reserve", "compact"} {
		m := encode(t, testCapture())
		delete(m, missing)
		if got, err := decodeExecutionCapture(m); err == nil {
			t.Fatalf("capture missing %q decoded to %+v, want rejection", missing, got)
		}
	}
	for _, mutation := range []struct {
		name  string
		mutat func(map[string]json.RawMessage)
	}{
		{"null context window", func(m map[string]json.RawMessage) { m["context_window"] = json.RawMessage("null") }},
		{"string context window", func(m map[string]json.RawMessage) { m["context_window"] = json.RawMessage(`"4096"`) }},
		{"null compact", func(m map[string]json.RawMessage) { m["compact"] = json.RawMessage("null") }},
		{"string compact", func(m map[string]json.RawMessage) { m["compact"] = json.RawMessage(`"c"`) }},
	} {
		m := encode(t, testCapture())
		mutation.mutat(m)
		if got, err := decodeExecutionCapture(m); err == nil {
			t.Fatalf("capture with %s decoded to %+v, want rejection", mutation.name, got)
		}
	}

	compactMutations := []struct {
		name  string
		mutat func(map[string]json.RawMessage)
	}{
		{"missing compact model", func(m map[string]json.RawMessage) { delete(m, "model") }},
		{"null compact model", func(m map[string]json.RawMessage) { m["model"] = json.RawMessage("null") }},
		{"string compact model", func(m map[string]json.RawMessage) { m["model"] = json.RawMessage(`"cprov/compact-x"`) }},
		{"unknown compact member", func(m map[string]json.RawMessage) { m["extra"] = json.RawMessage(`1`) }},
		{"missing compact prompt", func(m map[string]json.RawMessage) { delete(m, "system_prompt") }},
		{"null compact prompt", func(m map[string]json.RawMessage) { m["system_prompt"] = json.RawMessage("null") }},
		{"missing compact window", func(m map[string]json.RawMessage) { delete(m, "context_window") }},
	}
	for _, mutation := range compactMutations {
		t.Run("wire "+mutation.name, func(t *testing.T) {
			m := encode(t, testCapture())
			obj, err := decodePayloadObject(m["compact"])
			if err != nil {
				t.Fatalf("decodePayloadObject: %v", err)
			}
			mutation.mutat(obj)
			raw, err := json.Marshal(obj)
			if err != nil {
				t.Fatalf("marshal compact: %v", err)
			}
			m["compact"] = raw
			if got, err := decodeExecutionCapture(m); err == nil {
				t.Fatalf("capture with %s decoded to %+v, want rejection", mutation.name, got)
			}
		})
	}
}

// TestSessionIdentityParentLineage proves the child lineage member's wire
// shape: parent_session_id encodes by omission, round-trips on a child, is
// absent on a root and on a fork, and every invalid shape is rejected on both
// the encode and decode paths with the existing classes.
func TestSessionIdentityParentLineage(t *testing.T) {
	child := validSessionRecord()
	child.Identity.ParentSessionID = otherSession()
	childRaw, err := encodeSessionRegister(child)
	if err != nil {
		t.Fatalf("encode child identity: %v", err)
	}
	if !strings.Contains(string(childRaw), `"parent_session_id":"`+otherSession()+`"`) {
		t.Fatalf("child identity payload lacks parent_session_id: %s", childRaw)
	}
	decoded, err := decodeSessionRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Payload: childRaw})
	if err != nil {
		t.Fatalf("decode child identity: %v", err)
	}
	if decoded.Identity.ParentSessionID != otherSession() {
		t.Fatalf("decoded parent session id = %q, want %q", decoded.Identity.ParentSessionID, otherSession())
	}

	rootRaw, err := encodeSessionRegister(validSessionRecord())
	if err != nil {
		t.Fatalf("encode root identity: %v", err)
	}
	if strings.Contains(string(rootRaw), "parent_session_id") {
		t.Fatalf("root identity carries parent_session_id: %s", rootRaw)
	}

	fork := validSessionRecord()
	fork.Identity.SourceSessionID = otherSession()
	fork.Identity.SourceBoundaryEntryID = otherEntry()
	forkRaw, err := encodeSessionRegister(fork)
	if err != nil {
		t.Fatalf("encode fork identity: %v", err)
	}
	if strings.Contains(string(forkRaw), "parent_session_id") {
		t.Fatalf("fork identity carries parent_session_id: %s", forkRaw)
	}
	if _, err := decodeSessionRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Payload: forkRaw}); err != nil {
		t.Fatalf("decode fork identity: %v", err)
	}

	// Encode-side rejections use the existing invalid-input class.
	encodeRejections := []struct {
		name  string
		value SessionIdentity
	}{
		{"non-hex parent", SessionIdentity{SessionID: testSessionID, Workspace: "/tmp/works", CreatedAt: testTime, ParentSessionID: "not-hex"}},
		{"parent with fork lineage", SessionIdentity{SessionID: testSessionID, Workspace: "/tmp/works", CreatedAt: testTime, ParentSessionID: otherSession(), SourceSessionID: otherSession(), SourceBoundaryEntryID: otherEntry()}},
		{"parent with source session only", SessionIdentity{SessionID: testSessionID, Workspace: "/tmp/works", CreatedAt: testTime, ParentSessionID: otherSession(), SourceSessionID: otherSession()}},
		{"parent with boundary only", SessionIdentity{SessionID: testSessionID, Workspace: "/tmp/works", CreatedAt: testTime, ParentSessionID: otherSession(), SourceBoundaryEntryID: otherEntry()}},
	}
	for _, tc := range encodeRejections {
		t.Run("encode/"+tc.name, func(t *testing.T) {
			if _, err := encodeSessionIdentity(tc.value); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
		})
	}

	// Decode-side wire rejections on the child identity object.
	decodeRejections := []struct {
		name   string
		mutate func(map[string]json.RawMessage)
	}{
		{"empty parent", func(m map[string]json.RawMessage) { m["parent_session_id"] = json.RawMessage(`""`) }},
		{"null parent", func(m map[string]json.RawMessage) { m["parent_session_id"] = json.RawMessage(`null`) }},
		{"wrong container parent", func(m map[string]json.RawMessage) { m["parent_session_id"] = json.RawMessage(`5`) }},
		{"non-hex parent", func(m map[string]json.RawMessage) {
			m["parent_session_id"] = json.RawMessage(`"0123456789abcdef0123456789abcdef0"`)
		}},
		{"uppercase parent", func(m map[string]json.RawMessage) {
			m["parent_session_id"] = json.RawMessage(`"` + strings.Repeat("F", 32) + `"`)
		}},
		{"parent with source session", func(m map[string]json.RawMessage) {
			m["source_session_id"] = json.RawMessage(`"` + otherSession() + `"`)
		}},
		{"parent with boundary", func(m map[string]json.RawMessage) {
			m["source_boundary_entry_id"] = json.RawMessage(`"` + otherEntry() + `"`)
		}},
		{"parent with fork lineage", func(m map[string]json.RawMessage) {
			m["source_session_id"] = json.RawMessage(`"` + otherSession() + `"`)
			m["source_boundary_entry_id"] = json.RawMessage(`"` + otherEntry() + `"`)
		}},
	}
	for _, tc := range decodeRejections {
		t.Run("decode/"+tc.name, func(t *testing.T) {
			obj, err := objectMember(wireObject(t, childRaw), "identity", true)
			if err != nil {
				t.Fatalf("identity member: %v", err)
			}
			tc.mutate(obj)
			if _, err := decodeSessionIdentity(obj); err == nil {
				t.Fatalf("expected rejection")
			}
		})
	}
}

// TestBackgroundCompletionSignalRules proves the background_completion
// subtype's correlation rule: related_member is required with a valid kind and
// a kind-shaped id, related_operation and an owning Operation identity are
// forbidden, content is producer-bounded non-empty, the fixed kinds reject a
// related_member, and every invalid shape is rejected on both the encode and
// decode paths.
func TestBackgroundCompletionSignalRules(t *testing.T) {
	childID := otherSession()
	jobID := "0a1b2c3d"

	raw, err := encodeSignalEntry(validBackgroundCompletionEntry())
	if err != nil {
		t.Fatalf("encode child completion: %v", err)
	}
	env := Entry{SessionID: testSessionID, ID: testEntryID, Kind: EntrySignal, Sequence: 1, CommittedAt: testTime}
	env.Payload = raw
	decoded, err := decodeSignalEntry(env)
	if err != nil {
		t.Fatalf("decode child completion: %v", err)
	}
	if decoded.OperationID != "" || decoded.RelatedOperation != nil {
		t.Fatalf("completion signal is not operationless: %+v", decoded)
	}
	if decoded.RelatedMember == nil || decoded.RelatedMember.Kind != "child" || decoded.RelatedMember.ID != childID {
		t.Fatalf("decoded related member = %+v, want child %q", decoded.RelatedMember, childID)
	}
	if _, present := wireObject(t, raw)["related_operation"]; present {
		t.Fatalf("completion signal carries related_operation: %s", raw)
	}

	jobRaw, err := encodeSignalEntry(func() signalEntry {
		v := validBackgroundCompletionEntry()
		v.RelatedMember = &relatedMember{Kind: "job", ID: jobID}
		return v
	}())
	if err != nil {
		t.Fatalf("encode job completion: %v", err)
	}
	env.Payload = jobRaw
	if _, err := decodeSignalEntry(env); err != nil {
		t.Fatalf("decode job completion: %v", err)
	}

	// The fixed kinds reject a related_member member.
	fixedRaw, err := encodeSignalEntry(validSignalEntry(testOpID))
	if err != nil {
		t.Fatalf("encode fixed signal: %v", err)
	}
	fixedEnv := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntrySignal, Sequence: 1, CommittedAt: testTime}
	fixedEnv.Payload = setKey(fixedRaw, "related_member", json.RawMessage(`{"kind":"child","id":"`+childID+`"}`))
	if _, err := decodeSignalEntry(fixedEnv); err == nil {
		t.Fatalf("fixed signal with related_member must be rejected")
	}

	// Encode-side rejections on the subtype value.
	encodeRejections := []struct {
		name  string
		value signalEntry
	}{
		{"missing related member", func() signalEntry { v := validBackgroundCompletionEntry(); v.RelatedMember = nil; return v }()},
		{"wrong kind", func() signalEntry { v := validBackgroundCompletionEntry(); v.RelatedMember.Kind = "team"; return v }()},
		{"empty id", func() signalEntry { v := validBackgroundCompletionEntry(); v.RelatedMember.ID = ""; return v }()},
		{"non-hex child id", func() signalEntry { v := validBackgroundCompletionEntry(); v.RelatedMember.ID = "ghost"; return v }()},
		{"job-length child id", func() signalEntry { v := validBackgroundCompletionEntry(); v.RelatedMember.ID = jobID; return v }()},
		{"child-length job id", func() signalEntry {
			v := validBackgroundCompletionEntry()
			v.RelatedMember = &relatedMember{Kind: "job", ID: childID}
			return v
		}()},
		{"uppercase job id", func() signalEntry {
			v := validBackgroundCompletionEntry()
			v.RelatedMember = &relatedMember{Kind: "job", ID: "0A1B2C3D"}
			return v
		}()},
		{"related operation present", func() signalEntry {
			v := validBackgroundCompletionEntry()
			v.RelatedOperation = &operationRef{SessionID: testSessionID, OperationID: testOpID}
			return v
		}()},
		{"operation owned", func() signalEntry { v := validBackgroundCompletionEntry(); v.OperationID = testOpID; return v }()},
		{"empty content", func() signalEntry { v := validBackgroundCompletionEntry(); v.Content = ""; return v }()},
	}
	for _, tc := range encodeRejections {
		t.Run("encode/"+tc.name, func(t *testing.T) {
			if _, err := encodeSignalEntry(tc.value); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
		})
	}

	// Decode-side wire rejections.
	relatedMemberWire := func(kind, id string) string {
		return fmt.Sprintf(`{"kind":%q,"id":%q}`, kind, id)
	}
	decodeRejections := []struct {
		name    string
		payload json.RawMessage
	}{
		{"null related member", setKey(raw, "related_member", json.RawMessage(`null`))},
		{"wrong container related member", setKey(raw, "related_member", json.RawMessage(`[]`))},
		{"wrong kind", setKey(raw, "related_member", json.RawMessage(relatedMemberWire("team", childID)))},
		{"missing id", setKey(raw, "related_member", json.RawMessage(`{"kind":"child"}`))},
		{"empty id", setKey(raw, "related_member", json.RawMessage(relatedMemberWire("child", "")))},
		{"non-hex child id", setKey(raw, "related_member", json.RawMessage(relatedMemberWire("child", "ghost")))},
		{"job-length child id", setKey(raw, "related_member", json.RawMessage(relatedMemberWire("child", jobID)))},
		{"child-length job id", setKey(raw, "related_member", json.RawMessage(relatedMemberWire("job", childID)))},
		{"unknown member inside related member", setKey(raw, "related_member", json.RawMessage(`{"kind":"child","id":"`+childID+`","bogus":1}`))},
		{"related operation present", setKey(raw, "related_operation", json.RawMessage(`{"session_id":"`+testSessionID+`","operation_id":"`+testOpID+`"}`))},
		{"operation owned", setKey(raw, "operation_id", json.RawMessage(`"`+testOpID+`"`))},
		{"empty content", setKey(raw, "content", json.RawMessage(`""`))},
	}
	for _, tc := range decodeRejections {
		t.Run("decode/"+tc.name, func(t *testing.T) {
			env.Payload = tc.payload
			if _, err := decodeSignalEntry(env); err == nil {
				t.Fatalf("expected rejection")
			}
		})
	}
}

// TestCompactionEntryPayloadRules proves the compaction entry payload's
// closed shape: required members with exact keys, the optional operation
// identity and usage members, the fork-prefix usage rule, and the value
// rules enforced on both codec sides.
func TestCompactionEntryPayloadRules(t *testing.T) {
	t.Run("round trips owned with and without usage", func(t *testing.T) {
		for _, withUsage := range []bool{false, true} {
			v := validCompactionEntry(testOpID)
			if withUsage {
				v.Usage = &UsageCount{InputTokens: 11, CachedInputTokens: 4, OutputTokens: 7}
			}
			raw, err := encodeCompactionEntry(v)
			if err != nil {
				t.Fatalf("encode (usage %v): %v", withUsage, err)
			}
			env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime, Payload: raw}
			got, err := decodeCompactionEntry(env)
			if err != nil {
				t.Fatalf("decode (usage %v): %v", withUsage, err)
			}
			if (got.Usage == nil) != (v.Usage == nil) {
				t.Fatalf("decoded usage presence %+v, want %+v", got.Usage, v.Usage)
			}
			if withUsage && *got.Usage != *v.Usage {
				t.Fatalf("decoded usage %+v, want %+v", *got.Usage, *v.Usage)
			}
			got.Usage = v.Usage // pointer identity is irrelevant; the counts compared above
			if got != v {
				t.Fatalf("decoded %+v, want %+v", got, v)
			}
			again, err := encodeCompactionEntry(got)
			if err != nil {
				t.Fatalf("re-encode (usage %v): %v", withUsage, err)
			}
			if string(again) != string(raw) {
				t.Fatalf("re-encoded bytes %s, want %s", again, raw)
			}
		}
	})

	t.Run("round trips operationless fork copy", func(t *testing.T) {
		raw, err := encodeCompactionEntry(validCompactionEntry(""))
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		env := Entry{SessionID: testSessionID, ID: testEntryID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime, Payload: raw}
		got, err := decodeCompactionEntry(env)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.OperationID != "" || got.Usage != nil {
			t.Fatalf("operationless decode = %+v", got)
		}
	})

	t.Run("encode rejects invalid values", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*compactionEntry)
		}{
			{"non-hex session id", func(v *compactionEntry) { v.SessionID = "ghost" }},
			{"non-hex entry id", func(v *compactionEntry) { v.EntryID = "ghost" }},
			{"non-hex boundary entry id", func(v *compactionEntry) { v.BoundaryEntryID = "ghost" }},
			{"empty summary", func(v *compactionEntry) { v.Summary = "" }},
			{"empty boundary entry id", func(v *compactionEntry) { v.BoundaryEntryID = "" }},
			{"incomplete model", func(v *compactionEntry) { v.Model = model.ModelRef{Provider: "prov"} }},
			{"empty configuration revision", func(v *compactionEntry) { v.ConfigurationRevision = "" }},
			{"operationless with usage", func(v *compactionEntry) { v.OperationID = ""; v.Usage = testUsage(1) }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				v := validCompactionEntry(testOpID)
				tc.mutate(&v)
				_, err := encodeCompactionEntry(v)
				if err == nil {
					t.Fatalf("expected rejection")
				}
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("error %v is not ErrInvalid", err)
				}
			})
		}
	})

	t.Run("decode rejects invalid wire", func(t *testing.T) {
		raw, err := encodeCompactionEntry(validCompactionEntry(testOpID))
		if err != nil {
			t.Fatalf("encode valid: %v", err)
		}
		env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime}
		rejections := []struct {
			name    string
			payload json.RawMessage
		}{
			{"missing summary", dropKey(raw, "summary")},
			{"empty summary", setKey(raw, "summary", json.RawMessage(`""`))},
			{"null summary", setKey(raw, "summary", json.RawMessage(`null`))},
			{"missing boundary entry id", dropKey(raw, "boundary_entry_id")},
			{"empty boundary entry id", setKey(raw, "boundary_entry_id", json.RawMessage(`""`))},
			{"null boundary entry id", setKey(raw, "boundary_entry_id", json.RawMessage(`null`))},
			{"non-hex boundary entry id", setKey(raw, "boundary_entry_id", json.RawMessage(`"zz"`))},
			{"missing model", dropKey(raw, "model")},
			{"null model", setKey(raw, "model", json.RawMessage(`null`))},
			{"incomplete model", setKey(raw, "model", json.RawMessage(`{"provider":"prov"}`))},
			{"missing configuration revision", dropKey(raw, "configuration_revision")},
			{"empty configuration revision", setKey(raw, "configuration_revision", json.RawMessage(`""`))},
			{"null configuration revision", setKey(raw, "configuration_revision", json.RawMessage(`null`))},
			{"null operation id", setKey(raw, "operation_id", json.RawMessage(`null`))},
			{"empty operation id", setKey(raw, "operation_id", json.RawMessage(`""`))},
			{"missing entry id", dropKey(raw, "entry_id")},
			{"null usage", setKey(raw, "usage", json.RawMessage(`null`))},
			{"wrong container usage", setKey(raw, "usage", json.RawMessage(`0`))},
			{"incomplete usage", setKey(raw, "usage", json.RawMessage(`{"input_tokens":1}`))},
		}
		for _, tc := range rejections {
			t.Run(tc.name, func(t *testing.T) {
				env.Payload = tc.payload
				if _, err := decodeCompactionEntry(env); err == nil {
					t.Fatalf("expected rejection")
				}
			})
		}
	})

	t.Run("operationless with stored usage is rejected", func(t *testing.T) {
		env := Entry{SessionID: testSessionID, ID: testEntryID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime}
		raw, err := encodeCompactionEntry(validCompactionEntry(""))
		if err != nil {
			t.Fatalf("encode operationless: %v", err)
		}
		env.Payload = setKey(raw, "usage", json.RawMessage(`{"input_tokens":1,"cached_input_tokens":0,"output_tokens":0}`))
		if _, err := decodeCompactionEntry(env); err == nil {
			t.Fatalf("operationless compaction with stored usage must be rejected")
		}
	})
}

// TestSessionStateCompactionEntryIDRules proves the Session state's optional
// compaction_entry_id member: omission-encoded, hex-validated when present,
// and rejected as null, empty, or a non-hex string on both codec sides.
func TestSessionStateCompactionEntryIDRules(t *testing.T) {
	t.Run("round trips with the field set", func(t *testing.T) {
		rec := validSessionRecord()
		rec.State.CompactionEntryID = hexID(3)
		raw, err := encodeSessionRegister(rec)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		reg := Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Revision: 1, Payload: raw}
		got, err := decodeSessionRegister(reg)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.State.CompactionEntryID != hexID(3) {
			t.Fatalf("decoded CompactionEntryID %q, want %q", got.State.CompactionEntryID, hexID(3))
		}
	})

	t.Run("omits the member when empty", func(t *testing.T) {
		raw, err := encodeSessionRegister(validSessionRecord())
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		state := wireObject(t, mustState(t, raw))
		if _, present := state["compaction_entry_id"]; present {
			t.Fatalf("empty CompactionEntryID must encode by omission")
		}
	})

	patchState := func(t *testing.T, mutate func(obj map[string]json.RawMessage)) json.RawMessage {
		t.Helper()
		raw, err := encodeSessionRegister(validSessionRecord())
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		obj := wireObject(t, raw)
		state, err := decodePayloadObject(mustState(t, raw))
		if err != nil {
			t.Fatalf("decode state: %v", err)
		}
		mutate(state)
		patchedState, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("marshal state: %v", err)
		}
		obj["state"] = patchedState
		patched, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return patched
	}

	t.Run("decode rejections", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(obj map[string]json.RawMessage)
		}{
			{"non-hex id", func(obj map[string]json.RawMessage) { obj["compaction_entry_id"] = json.RawMessage(`"ghost"`) }},
			{"empty id", func(obj map[string]json.RawMessage) { obj["compaction_entry_id"] = json.RawMessage(`""`) }},
			{"null id", func(obj map[string]json.RawMessage) { obj["compaction_entry_id"] = json.RawMessage(`null`) }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				reg := Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Revision: 1, Payload: patchState(t, tc.mutate)}
				if _, err := decodeSessionRegister(reg); err == nil {
					t.Fatalf("expected rejection")
				}
			})
		}
	})

	t.Run("encode rejections", func(t *testing.T) {
		rec := validSessionRecord()
		rec.State.CompactionEntryID = "ghost"
		if _, err := encodeSessionRegister(rec); !errors.Is(err, ErrInvalid) {
			t.Fatalf("error %v, want ErrInvalid", err)
		}
	})
}
