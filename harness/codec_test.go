package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
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
		Model:        model.ModelRef{Provider: "cprov", Model: "compact-x"},
		SystemPrompt: "summarize",
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
		Strategy:              StrategyCapture{ID: IncludedStrategyID, RequiresContextWindow: true},
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
		SessionID:       testSessionID,
		EntryID:         testEntryID,
		OperationID:     operationID,
		Status:          model.OutputCompleted,
		Source:          testModelRef(),
		Content:         []model.ContentPart{model.ContentPart{Kind: model.PartText, Text: "hi"}},
		ToolCalls:       []toolCallRecord{},
		RequestID:       "request-one",
		AdvertisedNames: []string{},
		Evidence:        &RequestEvidence{BoundaryEntryID: hexID(1)},
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
		Evidence:       ExecutionObserved,
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

func validCompactionEntry(operationID string) contextCheckpointEntry {
	return contextCheckpointEntry{
		SessionID:   testSessionID,
		EntryID:     testEntryID,
		OperationID: operationID,
		StrategyID:  IncludedStrategyID,
		Messages: []model.Message{
			{Role: model.RoleAssistant, Source: testModelRef(), Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\nSummary of the earlier conversation.\n\n[End of summary. Continue from here.]"}}},
		},
		BoundaryEntryID:       hexID(2),
		ConfigurationRevision: "rev-1",
		Facts: []maintenanceFact{{
			RequestID: "maintenance-one",
			Model:     testModelRef(),
			Input: []model.Message{
				{Role: model.RoleSystem, Content: []model.ContentPart{{Kind: model.PartText, Text: "compact prompt"}}},
				{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "transcript"}}},
			},
		}},
		ConsumedRequestID: "request-two",
		Evidence:          &RequestEvidence{BoundaryEntryID: hexID(1)},
	}
}

func validHookResultEntry(status hookResultStatus) hookResultEntry {
	v := hookResultEntry{
		SessionID:            testSessionID,
		EntryID:              testEntryID,
		OperationID:          testOpID,
		HookID:               "cap.hook.one",
		ExtensionID:          "ext.one",
		ToolCallID:           "call-1",
		Status:               status,
		Evidence:             ExecutionObserved,
		InputArgumentsBase64: base64.StdEncoding.EncodeToString([]byte(`{"x":0}`)),
	}
	switch status {
	case hookSucceeded:
		v.Arguments = json.RawMessage(`{"x":1}`)
	default:
		v.Error = "hook broke"
	}
	return v
}

// validRequestBoundHookResult returns one valid request-bound hook result: its
// consumed input is the closed evidence record and its binding is a request
// identity, never a tool call.
func validRequestBoundHookResult(status hookResultStatus) hookResultEntry {
	v := hookResultEntry{
		SessionID:     testSessionID,
		EntryID:       testEntryID,
		OperationID:   testOpID,
		HookID:        "cap.hook.one",
		ExtensionID:   "ext.one",
		RequestID:     "request-one",
		Status:        status,
		Evidence:      ExecutionObserved,
		InputEvidence: &RequestEvidence{BoundaryEntryID: hexID(1)},
	}
	switch status {
	case hookSucceeded:
		consequence := RequestConsequence{}
		v.Consequence = &consequence
	default:
		v.Error = "hook broke"
	}
	return v
}

// --- wire mutation helpers --------------------------------------------------

func wireObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	obj, err := decodePayloadObject(raw)
	testassert.NoError(t, err)
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
				v.Facts[0].Usage = testUsage(3)
				return encodeContextCheckpointEntry(v)
			},
			decode: func(env Entry) error { _, err := decodeContextCheckpointEntry(env); return err },
		},
		{
			name: "compaction operationless fork copy",
			env:  Entry{SessionID: testSessionID, ID: testEntryID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime},
			encode: func() (json.RawMessage, error) {
				return encodeContextCheckpointEntry(validCompactionEntry(""))
			},
			decode: func(env Entry) error { _, err := decodeContextCheckpointEntry(env); return err },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := tc.encode()
			testassert.NoError(t, err)
			env := tc.env
			env.Payload = raw
			{
				err := tc.decode(env)
				testassert.NoError(t, err)
			}
			// A second decode of the same bytes stays stable.
			{
				err := tc.decode(env)
				testassert.NoError(t, err)
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
				v.Facts[0].Usage = testUsage(1)
				return encodeContextCheckpointEntry(v)
			},
			decode: func(env Entry) error { _, err := decodeContextCheckpointEntry(env); return err },
		},
	}
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			raw, err := kind.payload()
			testassert.NoError(t, err)
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
					{
						err := kind.decode(env)
						testassert.Require(t, err != nil, "expected rejection")
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
	testassert.NoError(t, err)
	env.Payload = raw
	{
		_, err := decodeAssistantEntry(env)
		testassert.NoError(t, err)
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
	{
		_, err := decodeAssistantEntry(env)
		testassert.Require(t, err != nil, "operationless assistant with stored usage must be rejected")
	}
}

// TestHookResultStatusRules proves the hook-result status-owned member rules:
// success requires exactly one complete JSON object argument and forbids an
// error; error and interrupted require a non-empty error and forbid
// arguments; unknown statuses reject; and a hook result without an owning
// Operation identity rejects.
func TestHookResultStatusRules(t *testing.T) {
	t.Run("success rules", func(t *testing.T) {
		{
			_, err := encodeHookResultEntry(validHookResultEntry(hookSucceeded))
			testassert.NoError(t, err)
		}
		nonObject := validHookResultEntry(hookSucceeded)
		nonObject.Arguments = json.RawMessage(`[1,2]`)
		{
			_, err := encodeHookResultEntry(nonObject)
			testassert.Require(t, err != nil, "array arguments must reject")
		}
		nullArguments := validHookResultEntry(hookSucceeded)
		nullArguments.Arguments = json.RawMessage(`null`)
		{
			_, err := encodeHookResultEntry(nullArguments)
			testassert.Require(t, err != nil, "null arguments must reject")
		}
		withError := validHookResultEntry(hookSucceeded)
		withError.Error = "boom"
		{
			_, err := encodeHookResultEntry(withError)
			testassert.Require(t, err != nil, "success with an error must reject")
		}
	})
	t.Run("failure and interruption rules", func(t *testing.T) {
		for _, status := range []hookResultStatus{hookFailed, hookInterrupted} {
			{
				_, err := encodeHookResultEntry(validHookResultEntry(status))
				testassert.Require(t, err == nil, "encode valid %s: %v", status, err)
			}
			empty := validHookResultEntry(status)
			empty.Error = ""
			{
				_, err := encodeHookResultEntry(empty)
				testassert.Require(t, err != nil, "%s without an error must reject", status)
			}
			withArguments := validHookResultEntry(status)
			withArguments.Arguments = json.RawMessage(`{}`)
			{
				_, err := encodeHookResultEntry(withArguments)
				testassert.Require(t, err != nil, "%s with arguments must reject", status)
			}
		}
	})
	t.Run("unknown status", func(t *testing.T) {
		{
			_, err := encodeHookResultEntry(validHookResultEntry("ok"))
			testassert.Require(t, err != nil, "unknown status must reject")
		}
	})
	t.Run("decode-side rejections", func(t *testing.T) {
		raw, err := encodeHookResultEntry(validHookResultEntry(hookSucceeded))
		testassert.NoError(t, err)
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
				testassert.NoError(t, err)
				return setKey(failedRaw, "arguments", json.RawMessage(`{}`))
			}()},
		} {
			t.Run(mutation.name, func(t *testing.T) {
				env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryHookResult, Sequence: 1, CommittedAt: testTime, Payload: mutation.payload}
				{
					_, err := decodeHookResultEntry(env)
					testassert.Require(t, err != nil, "expected rejection")
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
	valid.State.ActiveEffect = &ActiveEffect{Kind: EffectHook, ResultEntryID: hexID(9), ToolCallID: "call-1", HookID: "cap.hook.one", ExtensionID: "cap-a", InputArgumentsBase64: base64.StdEncoding.EncodeToString([]byte(`{"x":1}`))}
	{
		_, err := encodeOperationRegister(valid)
		testassert.NoError(t, err)
	}

	for _, tc := range []struct {
		name    string
		effect  ActiveEffect
		wantErr string
	}{
		{"hook effect without a hook id", ActiveEffect{Kind: EffectHook, ResultEntryID: hexID(9), ToolCallID: "call-1", ExtensionID: "cap-a", InputArgumentsBase64: base64.StdEncoding.EncodeToString([]byte(`{"x":1}`))}, "hook active effect requires its hook id"},
		{"hook effect without a tool call id", ActiveEffect{Kind: EffectHook, ResultEntryID: hexID(9), HookID: "cap.hook.one", ExtensionID: "cap-a"}, "hook active effect requires its request or tool call binding"},
		{"hook effect on a non-pending call", ActiveEffect{Kind: EffectHook, ResultEntryID: hexID(9), ToolCallID: "call-9", HookID: "cap.hook.one", ExtensionID: "cap-a", InputArgumentsBase64: base64.StdEncoding.EncodeToString([]byte(`{"x":1}`))}, "tool-bound hook active effect must address the matching first pending call"},
		{"model effect with a hook id", ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(9), HookID: "cap.hook.one"}, "model active effect carries no call, hook, or extension identity"},
		{"tool effect with a hook id", ActiveEffect{Kind: EffectTool, ResultEntryID: testResultID, ToolCallID: "call-1", HookID: "cap.hook.one"}, "tool active effect carries no hook identity"},
		{"unknown kind", ActiveEffect{Kind: "widget", ResultEntryID: hexID(9), ToolCallID: "call-1", HookID: "cap.hook.one"}, `active effect kind "widget" is not one of model, tool or hook`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := op
			bad.State.ActiveEffect = &tc.effect
			{
				_, err := encodeOperationRegister(bad)
				testassert.Require(t, err != nil && strings.Contains(err.Error(), tc.wantErr), "encode = %v, want %q", err, tc.wantErr)
			}
			// decode-side: the persisted active_effect member with the same
			// shape rejects through decodeOperationRegister; a null hook_id
			// member (absence encodes by omission) rejects too.
			raw, err := encodeOperationRegister(op)
			testassert.NoError(t, err)
			effectJSON, merr := json.Marshal(tc.effect)
			testassert.NoError(t, merr)
			if tc.name == "hook effect without a hook id" {
				effectJSON = json.RawMessage(`{"kind":"hook","result_entry_id":"` + hexID(9) + `","tool_call_id":"call-1","hook_id":null,"hook_extension_id":"cap-a","input_arguments_base64":"eyJ4IjoxfQ=="}`)
			}
			stateObj := mustState(t, raw)
			patchedState := setKey(stateObj, "active_effect", effectJSON)
			full := setKey(raw, "state", patchedState)
			{
				_, err := decodeOperationRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: full})
				wantDecode := tc.wantErr
				if tc.name == "hook effect without a hook id" {
					// The persisted null member is rejected by name at the strict
					// member codec before the value rule — the same targeted rule.
					wantDecode = `member "hook_id" must not be null`
				}
				testassert.Require(t, err != nil && strings.Contains(err.Error(), wantDecode), "decode = %v, want the %q rule to reject %s", err, wantDecode, tc.name)
			}
		})
	}
	t.Run("non-tool bindings forbid the input_arguments_base64 member outright", func(t *testing.T) {
		effects := []struct {
			name   string
			effect ActiveEffect
		}{
			{"request-bound hook intent", ActiveEffect{Kind: EffectHook, ResultEntryID: hexID(9), RequestID: "request-one", HookID: "cap.hook.one", ExtensionID: "cap-a", Evidence: &RequestEvidence{BoundaryEntryID: hexID(1)}}},
			{"conversation model intent", ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(9), RequestID: "request-one", Model: &model.ModelRef{Provider: "prov", Model: "m"}, Purpose: InvocationPurposeConversation, Evidence: &RequestEvidence{BoundaryEntryID: hexID(1)}}},
			{"maintenance model intent", ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(9), RequestID: "maintenance-one", Model: &model.ModelRef{Provider: "prov", Model: "compact"}, Purpose: InvocationPurposeMaintenance}},
			{"concrete tool intent", ActiveEffect{Kind: EffectTool, ResultEntryID: hexID(9), ToolCallID: "call-1", FinalArguments: json.RawMessage(`{}`)}},
		}
		for _, tc := range effects {
			raw, err := encodeActiveEffect(tc.effect)
			testassert.NoError(t, err)
			obj := wireObject(t, raw)
			for _, mutation := range []struct {
				name  string
				value json.RawMessage
			}{
				{"present-empty", json.RawMessage(`""`)},
				{"null", json.RawMessage(`null`)},
				{"wrong type", json.RawMessage(`5`)},
				{"canonical base64 bytes", json.RawMessage(`"eyJ4IjoxfQ=="`)},
			} {
				obj["input_arguments_base64"] = mutation.value
				_, derr := decodeActiveEffect(obj)
				testassert.Require(t, derr != nil && strings.Contains(derr.Error(), `forbids the "input_arguments_base64" member`),
					"%s wire %s member decode = %v, want the membership rejection", tc.name, mutation.name, derr)
			}
		}
		// Nearest positive siblings: the tool-bound hook intent keeps its
		// required member, present-empty and opaque malformed-JSON bytes
		// included.
		empty := ActiveEffect{Kind: EffectHook, ResultEntryID: hexID(9), ToolCallID: "call-1", HookID: "cap.hook.one", ExtensionID: "cap-a"}
		raw, err := encodeActiveEffect(empty)
		testassert.NoError(t, err)
		testassert.Require(t, strings.Contains(string(raw), `"input_arguments_base64":""`), "tool-bound empty consumed bytes must stay an encoded required member: %s", raw)
		{
			decoded, derr := decodeActiveEffect(wireObject(t, raw))
			testassert.Require(t, derr == nil && decoded.InputArgumentsBase64 == "", "tool-bound present-empty decode: %v", derr)
		}
		opaque := empty
		opaque.InputArgumentsBase64 = base64.StdEncoding.EncodeToString([]byte("not json"))
		raw, err = encodeActiveEffect(opaque)
		testassert.NoError(t, err)
		{
			decoded, derr := decodeActiveEffect(wireObject(t, raw))
			testassert.Require(t, derr == nil && decoded.InputArgumentsBase64 == opaque.InputArgumentsBase64, "tool-bound opaque malformed-JSON bytes decode: %v", derr)
		}
	})
}

// TestRegisterPayloadRoundTrip proves both register payloads round-trip and
// keep revision as envelope metadata only.
func TestRegisterPayloadRoundTrip(t *testing.T) {
	session := validSessionRecord()
	sessionRaw, err := encodeSessionRegister(session)
	testassert.NoError(t, err)
	sessionReg := Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Revision: 7, Payload: sessionRaw}
	decoded, err := decodeSessionRegister(sessionReg)
	testassert.NoError(t, err)
	testassert.Require(t, decoded.Revision == 7 && decoded.Identity == session.Identity && (decoded.State.Lifecycle == session.State.Lifecycle && decoded.State.CurrentAgentType == session.State.CurrentAgentType && decoded.State.CurrentOperationID == session.State.CurrentOperationID && decoded.State.ArchivedAt == nil && decoded.State.LastActivity.Equal(session.State.LastActivity) && usageTotalsEqual(decoded.State.Usage, session.State.Usage)), "decoded session register %+v does not round-trip; decoded session state %+v does not round-trip", decoded, decoded.State)
	testassert.Require(t, !strings.Contains(string(sessionRaw), `"revision"`), "revision leaked into the session payload: %s", sessionRaw)

	operation := validOperationRecord()
	opRaw, err := encodeOperationRegister(operation)
	testassert.NoError(t, err)
	opReg := Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Revision: 3, Payload: opRaw}
	decodedOp, err := decodeOperationRegister(opReg)
	testassert.NoError(t, err)
	testassert.Require(t, decodedOp.Revision == 3 && decodedOp.Admission.SessionID == operation.Admission.SessionID && decodedOp.Admission.OperationID == operation.Admission.OperationID && decodedOp.Admission.AgentType == operation.Admission.AgentType && decodedOp.Admission.AdmittedAt.Equal(operation.Admission.AdmittedAt) && decodedOp.State.Status == operation.State.Status && len(decodedOp.State.PendingToolCalls) == len(operation.State.PendingToolCalls), "decoded operation register does not round-trip: %+v", decodedOp)
	testassert.Require(t, !strings.Contains(string(opRaw), `"revision"`), "revision leaked into the operation payload: %s", opRaw)
}

// TestOperationStateEncodingShape pins the state section's encoding shape
// directly: an absent pending array encodes as the required non-null empty
// array, optional records encode by omission, and a model active effect never
// carries the tool-effect-only tool_call_id key.
func TestOperationStateEncodingShape(t *testing.T) {
	v := validOperationRecord()
	v.State.PendingToolCalls = nil // nil is a valid decoded shape for a quiet running Operation
	raw, err := encodeOperationRegister(v)
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(string(raw), `"pending_tool_calls":null`), "pending_tool_calls encoded as null: %s", raw)
	obj := wireObject(t, raw)
	stateObj, err := objectMember(obj, "state", true)
	testassert.NoError(t, err)
	pending, err := arrayMember(stateObj, "pending_tool_calls", true)
	testassert.NoError(t, err)
	testassert.Require(t, len(pending) == 0, "pending_tool_calls = %v, want the empty array", pending)
	for _, optional := range []string{"settled_at", "active_effect", "terminal"} {
		{
			_, present := stateObj[optional]
			testassert.Require(t, !present, "running state carries optional member %q", optional)
		}
	}
	{
		_, err := decodeOperationRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: raw})
		testassert.NoError(t, err)
	}

	effect := validOperationRecord()
	mdl := testModelRef()
	effect.State.ActiveEffect = &ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(3), RequestID: "request-one", Model: &mdl, Purpose: InvocationPurposeConversation, Evidence: &RequestEvidence{BoundaryEntryID: hexID(1)}} // model effect: no tool call id
	effect.State.Request = &RunningRequest{RequestID: "request-one", BoundaryEntryID: hexID(1)}
	raw, err = encodeOperationRegister(effect)
	testassert.NoError(t, err)
	stateObj, err = objectMember(wireObject(t, raw), "state", true)
	testassert.NoError(t, err)
	effectObj, err := objectMember(stateObj, "active_effect", true)
	testassert.NoError(t, err)
	{
		_, present := effectObj["tool_call_id"]
		testassert.Require(t, !present, "model active effect carries tool_call_id: %s", raw)
	}
}

// TestRegisterPayloadRejectsInvalidWire proves the register payloads enforce
// exact keys, containers, and envelope-key identity agreement.
func TestRegisterPayloadRejectsInvalidWire(t *testing.T) {
	sessionRaw, err := encodeSessionRegister(validSessionRecord())
	testassert.NoError(t, err)
	opRaw, err := encodeOperationRegister(validOperationRecord())
	testassert.NoError(t, err)
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
			{
				_, err := decodeSessionRegister(reg)
				testassert.Require(t, err != nil, "expected rejection")
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
		{"operation mismatch", setKey(opRaw, "admission", json.RawMessage(`{"session_id":"`+testSessionID+`","operation_id":"ghost","request_kind":"message","admitted_entry":{"session_id":"`+testSessionID+`","entry_id":"`+hexID(1)+`"},"agent_type":"coder","execution":{"configuration_revision":"rev-1","model":{"provider":"prov","model":"gpt-x"},"context_window":4096,"output_reserve":2048,"system_prompt":"system","tools":[],"readonly":false,"write_dir":"","compact":{"model":{"provider":"cprov","model":"compact-x"},"system_prompt":"summarize"}},"admitted_at":"2026-01-02T03:04:05.123456789Z"}`))},
	}
	for _, m := range opMutations {
		t.Run("operation/"+m.name, func(t *testing.T) {
			reg := Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: m.payload}
			{
				_, err := decodeOperationRegister(reg)
				testassert.Require(t, err != nil, "expected rejection")
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
	testassert.NoError(t, err)
	testassert.Require(t, string(raw) == `{"provider":"a/b","model":"c/d"}`, "durable model ref wire = %s, want the two-field object", raw)
	obj, err := decodePayloadObject(raw)
	testassert.NoError(t, err)
	ref, err := decodeModelRef(obj)
	testassert.NoError(t, err)
	testassert.Require(t, ref.Provider == "a/b" && ref.Model == "c/d", "slashed ref decoded as %q/%q", ref.Provider, ref.Model)

	{
		_, err := encodeModelRef(model.ModelRef{})
		testassert.Require(t, errors.Is(err, ErrInvalid), "zero ref encode = %v, want ErrInvalid", err)
	}
	{
		_, err := encodeModelRef(model.ModelRef{Provider: "prov"})
		testassert.Require(t, errors.Is(err, ErrInvalid), "partial ref encode = %v, want ErrInvalid", err)
	}
	for _, bad := range []json.RawMessage{
		json.RawMessage(`"prov/gpt-x"`),        // combined-string coercion
		json.RawMessage(`{}`),                  // zero
		json.RawMessage(`{"provider":"prov"}`), // partial
		json.RawMessage(`null`),
		json.RawMessage(`{"provider":"","model":"m"}`), // empty fields
	} {
		{
			_, err := decodeModelRefFromRaw(bad)
			testassert.Require(t, err != nil, "model ref %s must be rejected", bad)
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
	testassert.NoError(t, err)
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: rawWire}
	decoded, err := decodeAssistantEntry(env)
	testassert.NoError(t, err)
	got, err := base64.StdEncoding.Strict().DecodeString(decoded.ToolCalls[0].ArgumentsBase64)
	testassert.NoError(t, err)
	testassert.Require(t, string(got) == string(raw), "arguments round-tripped as %x, want %x", got, raw)
}

// TestUsageAccounting proves checked signed usage addition: positive and
// negative counts sum, overflow returns the storage-failure class with no
// partial result, and totals stay unique and lexicographically sorted.
func TestUsageAccounting(t *testing.T) {
	sum, err := addUsageCount(UsageCount{InputTokens: 5, OutputTokens: -2}, UsageCount{InputTokens: 7, CachedInputTokens: 3, OutputTokens: 2})
	testassert.NoError(t, err)
	testassert.Equal(t, sum, (UsageCount{InputTokens: 12, CachedInputTokens: 3, OutputTokens: 0}))
	maxed := UsageCount{InputTokens: math.MaxInt64}
	if out, err := addUsageCount(maxed, UsageCount{InputTokens: 1}); !errors.Is(err, ErrStorage) {
		t.Fatalf("overflow = (%+v, %v), want ErrStorage", out, err)
	} else if out != (UsageCount{}) {
		t.Fatalf("overflow published a partial result %+v", out)
	}
	{
		out, err := addUsageCount(UsageCount{InputTokens: math.MinInt64}, UsageCount{InputTokens: -1})
		testassert.Require(t, errors.Is(err, ErrStorage), "negative overflow = (%+v, %v), want ErrStorage", out, err)
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
	testassert.NoError(t, err)
	want := []ModelUsage{
		{Model: model.ModelRef{Provider: "a", Model: "b"}, Usage: UsageCount{}},
		{Model: model.ModelRef{Provider: "a", Model: "x"}, Usage: UsageCount{InputTokens: 2}},
		{Model: model.ModelRef{Provider: "z", Model: "m"}, Usage: UsageCount{InputTokens: 1, OutputTokens: 4}},
	}
	testassert.Equal(t, len(merged.ByModel), len(want))
	for i := range want {
		testassert.Require(t, merged.ByModel[i] == want[i], "merged[%d] = %+v, want %+v", i, merged.ByModel[i], want[i])
	}
	{
		_, err := addUsageTotals(UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: maxed}}}, UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: UsageCount{InputTokens: 1}}}})
		testassert.Require(t, errors.Is(err, ErrStorage), "totals overflow = %v, want ErrStorage", err)
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
		{
			err := validateUsageTotals(bad)
			testassert.Require(t, err != nil, "totals %+v must be rejected", bad)
		}
	}
}

// TestToolCallNormalizedArgumentsNullRejected proves the null-literal rule
// for raw JSON members is shared by both codec sides: decode rejects the null
// literal through the codec-wide member rule, so encode must reject it too,
// while a non-null JSON value round-trips verbatim.
func TestToolCallNormalizedArgumentsNullRejected(t *testing.T) {
	entry := validAssistantEntry(testOpID)
	entry.AdvertisedNames = []string{"echo"}
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
	testassert.NoError(t, err)
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: raw}
	decoded, err := decodeAssistantEntry(env)
	testassert.NoError(t, err)
	testassert.Require(t, string(decoded.ToolCalls[0].NormalizedArguments) == `{"x":1}`, "normalized_arguments round-tripped as %s", decoded.ToolCalls[0].NormalizedArguments)
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
	entry.AdvertisedNames = []string{"echo"}
	call := validToolCallRecord()
	call.NormalizedArguments = json.RawMessage(wrong)
	entry.ToolCalls = []toolCallRecord{call}
	{
		_, err := encodeAssistantEntry(entry)
		testassert.Require(t, errors.Is(err, ErrInvalid), "encode normalized_arguments %s = %v, want the ErrInvalid class", wrong, err)
	}

	// decode side: rewrite one valid encoded payload's member to the wrong
	// kind and prove decode rejects it.
	valid := validAssistantEntry(testOpID)
	valid.AdvertisedNames = []string{"echo"}
	vcall := validToolCallRecord()
	vcall.NormalizedArguments = json.RawMessage(`{"x":1}`)
	valid.ToolCalls = []toolCallRecord{vcall}
	raw, err := encodeAssistantEntry(valid)
	testassert.NoError(t, err)
	obj := map[string]json.RawMessage{}
	{
		err := json.Unmarshal(raw, &obj)
		testassert.NoError(t, err)
	}
	items := []map[string]json.RawMessage{}
	{
		err := json.Unmarshal(obj["tool_calls"], &items)
		testassert.Require(t, err == nil && len(items) == 1, "encoded tool_calls = %s (%v)", obj["tool_calls"], err)
	}
	items[0]["normalized_arguments"] = json.RawMessage(wrong)
	edited, err := json.Marshal(items)
	testassert.NoError(t, err)
	obj["tool_calls"] = edited
	out, err := json.Marshal(obj)
	testassert.NoError(t, err)
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: out}
	{
		_, err := decodeAssistantEntry(env)
		testassert.Require(t, err != nil, "decode normalized_arguments %s must fail", wrong)
	}
}

// TestToolResultEvidenceRules proves the closed execution-evidence member of
// one tool-result payload: every valid value round-trips verbatim with no
// status coupling, and a missing, null or unknown evidence member is
// rejected by both codec sides with the invalid-input class.
func TestToolResultEvidenceRules(t *testing.T) {
	for _, evidence := range []ExecutionEvidence{ExecutionNotStarted, ExecutionObserved, ExecutionUnknown} {
		v := validToolResultEntry(testOpID)
		v.Evidence = evidence
		raw, err := encodeToolResultEntry(v)
		testassert.NoError(t, err)
		env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryToolResult, Payload: raw}
		decoded, err := decodeToolResultEntry(env)
		testassert.NoError(t, err)
		testassert.Equal(t, decoded.Evidence, evidence)
	}

	invalid := validToolResultEntry(testOpID)
	invalid.Evidence = ExecutionEvidence("pending")
	{
		_, err := encodeToolResultEntry(invalid)
		testassert.Require(t, errors.Is(err, ErrInvalid), "encode unknown evidence = %v, want the ErrInvalid class", err)
	}

	raw, err := encodeToolResultEntry(validToolResultEntry(testOpID))
	testassert.NoError(t, err)
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryToolResult, Payload: raw}
	for _, tc := range []struct {
		name    string
		payload json.RawMessage
	}{
		{"missing evidence", dropKey(raw, "evidence")},
		{"null evidence", setKey(raw, "evidence", json.RawMessage(`null`))},
		{"unknown evidence", setKey(raw, "evidence", json.RawMessage(`"pending"`))},
	} {
		env.Payload = tc.payload
		{
			_, err := decodeToolResultEntry(env)
			testassert.Require(t, err != nil, "decode %s must fail", tc.name)
		}
	}
}

// TestToolResultShapingMembers proves the durable output-shaping members of
// one tool-result payload: original_content is a required member mirroring
// content's member semantics — an empty original is valid beside an empty
// success and both contents satisfy the unchanged status rule — while
// shaping_failure encodes by omission, rejects an empty present member, and
// bounds its diagnostic to valid UTF-8 within the durable bound on both
// codec sides.
func TestToolResultShapingMembers(t *testing.T) {
	roundTrip := func(t *testing.T, v toolResultEntry) toolResultEntry {
		t.Helper()
		raw, err := encodeToolResultEntry(v)
		testassert.NoError(t, err)
		decoded, err := decodeToolResultEntry(Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryToolResult, Payload: raw})
		testassert.NoError(t, err)
		return decoded
	}

	// Both members round-trip verbatim; a missing optional member decodes to
	// the zero value and encodes by omission.
	withBoth := validToolResultEntry(testOpID)
	withBoth.OriginalContent = "raw output"
	withBoth.ShapingFailure = "shaping aborted"
	{
		decoded := roundTrip(t, withBoth)
		testassert.Equal(t, decoded.OriginalContent, "raw output")
		testassert.Equal(t, decoded.ShapingFailure, "shaping aborted")
	}
	{
		bare := roundTrip(t, validToolResultEntry(testOpID))
		testassert.Equal(t, bare.OriginalContent, "")
		testassert.Equal(t, bare.ShapingFailure, "")
		raw, err := encodeToolResultEntry(validToolResultEntry(testOpID))
		testassert.NoError(t, err)
		obj := wireObject(t, raw)
		_, present := obj["original_content"]
		testassert.Require(t, present, "encoded result %s carries no original_content member", raw)
		_, present = obj["shaping_failure"]
		testassert.Require(t, !present, "encoded result %s carries an absent shaping_failure member", raw)
	}
	// An empty original content round-trips beside an empty success.
	{
		empty := validToolResultEntry(testOpID)
		empty.Content = ""
		decoded := roundTrip(t, empty)
		testassert.Equal(t, decoded.OriginalContent, "")
	}

	// The membership and status-shape rules: original_content is required and
	// satisfies the status rule like content, and a present shaping_failure
	// must be non-empty.
	failure := validToolResultEntry(testOpID)
	failure.Status = model.ResultError
	failure.Content = "real failure"
	failure.OriginalContent = "real failure"
	failureRaw, err := encodeToolResultEntry(failure)
	testassert.NoError(t, err)
	for _, tc := range []struct {
		name    string
		payload json.RawMessage
	}{
		{"missing original_content", dropKey(failureRaw, "original_content")},
		{"null original_content", setKey(failureRaw, "original_content", json.RawMessage(`null`))},
		{"empty original content on a non-success status", setKey(failureRaw, "original_content", json.RawMessage(`""`))},
		{"empty present shaping_failure", setKey(failureRaw, "shaping_failure", json.RawMessage(`""`))},
	} {
		env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryToolResult, Payload: tc.payload}
		{
			_, err := decodeToolResultEntry(env)
			testassert.Require(t, err != nil, "decode %s must fail", tc.name)
		}
	}
	{
		bad := validToolResultEntry(testOpID)
		bad.Status = model.ResultError
		bad.Content = "real failure"
		_, err := encodeToolResultEntry(bad) // empty original beside a non-success status
		testassert.Require(t, errors.Is(err, ErrInvalid), "encode empty original on a non-success status = %v, want the ErrInvalid class", err)
	}

	// The bounded rule: oversized and invalid-UTF-8 diagnostics are rejected
	// by both codec sides; the exact bound round-trips.
	v := withBoth
	v.ShapingFailure = strings.Repeat("x", maxToolDiagnosticBytes+1)
	{
		_, err := encodeToolResultEntry(v)
		testassert.Require(t, errors.Is(err, ErrInvalid), "encode oversized failure = %v, want the ErrInvalid class", err)
	}
	v.ShapingFailure = string([]byte{0xff, 0xfe})
	{
		_, err := encodeToolResultEntry(v)
		testassert.Require(t, errors.Is(err, ErrInvalid), "encode invalid-UTF-8 failure = %v, want the ErrInvalid class", err)
	}
	v.ShapingFailure = strings.Repeat("x", maxToolDiagnosticBytes)
	{
		decoded := roundTrip(t, v)
		testassert.Equal(t, decoded.ShapingFailure, v.ShapingFailure)
	}
	{
		raw, err := encodeToolResultEntry(v)
		testassert.NoError(t, err)
		env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryToolResult, Payload: setKey(raw, "shaping_failure", json.RawMessage(`"`+strings.Repeat("x", maxToolDiagnosticBytes+1)+`"`))}
		{
			_, err := decodeToolResultEntry(env)
			testassert.Require(t, err != nil, "decode oversized persisted failure must fail")
		}
	}
}

// TestToolCallNormalizationFailureRules proves the nested normalization-
// failure member: its bounded valid-UTF-8 diagnostic round-trips verbatim,
// absence encodes by omission, and it is mutually exclusive with
// normalized_arguments on both codec sides. The closed assistant-level rule
// over the advertised names accepts exactly one of the two members for an
// advertised call and neither for an unadvertised one, in every combination.
func TestToolCallNormalizationFailureRules(t *testing.T) {
	const diag = "arguments must be one non-null JSON object"
	encodeAssistant := func(t *testing.T, v assistantEntry) (json.RawMessage, map[string]json.RawMessage) {
		t.Helper()
		raw, err := encodeAssistantEntry(v)
		testassert.NoError(t, err)
		obj := wireObject(t, raw)
		items := []map[string]json.RawMessage{}
		testassert.NoError(t, json.Unmarshal(obj["tool_calls"], &items))
		testassert.Require(t, len(items) == 1, "encoded tool_calls = %s", obj["tool_calls"])
		return raw, items[0]
	}

	// One bounded diagnostic round-trips verbatim and encodes by omission
	// when absent.
	advertised := validAssistantEntry(testOpID)
	advertised.AdvertisedNames = []string{"echo"}
	failed := validToolCallRecord()
	failed.NormalizationFailure = diag
	advertised.ToolCalls = []toolCallRecord{failed}
	raw, member := encodeAssistant(t, advertised)
	{
		_, present := member["normalization_failure"]
		testassert.Require(t, present, "encoded call %s carries no normalization_failure member", member)
		_, present = member["normalized_arguments"]
		testassert.Require(t, !present, "encoded call %s carries both normalization members", member)
	}
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: raw}
	decoded, err := decodeAssistantEntry(env)
	testassert.NoError(t, err)
	testassert.Equal(t, decoded.ToolCalls[0].NormalizationFailure, diag)
	normalized := validToolCallRecord()
	normalized.NormalizedArguments = json.RawMessage(`{"x":1}`)
	advertised.ToolCalls = []toolCallRecord{normalized}
	_, member = encodeAssistant(t, advertised)
	{
		_, present := member["normalization_failure"]
		testassert.Require(t, !present, "encoded normalized call %s carries the failure member", member)
	}

	// The exactly-one/neither rule over the advertised names: every other
	// combination is rejected before encoding.
	for _, tc := range []struct {
		name       string
		advertised []string
		call       toolCallRecord
	}{
		{"advertised call with neither member", []string{"echo"}, validToolCallRecord()},
		{"advertised call with both members", []string{"echo"}, func() toolCallRecord {
			c := validToolCallRecord()
			c.NormalizedArguments = json.RawMessage(`{"x":1}`)
			c.NormalizationFailure = diag
			return c
		}()},
		{"unadvertised call with a failure member", []string{"ghost"}, failed},
		{"unadvertised call with a normalized member", []string{"ghost"}, normalized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := validAssistantEntry(testOpID)
			v.AdvertisedNames = tc.advertised
			v.ToolCalls = []toolCallRecord{tc.call}
			{
				_, err := encodeAssistantEntry(v)
				testassert.Require(t, errors.Is(err, ErrInvalid), "encode = %v, want the ErrInvalid class", err)
			}
		})
	}

	// The bounded rule: empty, oversized and invalid-UTF-8 diagnostics are
	// rejected by both codec sides; the exact bound decodes.
	t.Run("bounded diagnostic", func(t *testing.T) {
		v := validAssistantEntry(testOpID)
		v.AdvertisedNames = []string{"echo"}
		call := validToolCallRecord()
		call.NormalizationFailure = strings.Repeat("x", maxToolDiagnosticBytes+1)
		v.ToolCalls = []toolCallRecord{call}
		{
			_, err := encodeAssistantEntry(v)
			testassert.Require(t, errors.Is(err, ErrInvalid), "encode oversized diagnostic = %v, want the ErrInvalid class", err)
		}
		call.NormalizationFailure = string([]byte{0xff, 0xfe})
		v.ToolCalls = []toolCallRecord{call}
		{
			_, err := encodeAssistantEntry(v)
			testassert.Require(t, errors.Is(err, ErrInvalid), "encode invalid-UTF-8 diagnostic = %v, want the ErrInvalid class", err)
		}
		call.NormalizationFailure = strings.Repeat("x", maxToolDiagnosticBytes)
		v.ToolCalls = []toolCallRecord{call}
		raw, err := encodeAssistantEntry(v)
		testassert.NoError(t, err)
		env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: raw}
		decoded, err := decodeAssistantEntry(env)
		testassert.NoError(t, err)
		testassert.Equal(t, decoded.ToolCalls[0].NormalizationFailure, call.NormalizationFailure)

		// The persisted member rejects an empty string and an oversized value.
		obj := map[string]json.RawMessage{}
		testassert.NoError(t, json.Unmarshal(raw, &obj))
		items := []map[string]json.RawMessage{}
		testassert.NoError(t, json.Unmarshal(obj["tool_calls"], &items))
		for _, tc := range []struct {
			name   string
			member json.RawMessage
		}{
			{"empty present member", json.RawMessage(`""`)},
			{"oversized member", json.RawMessage(`"` + strings.Repeat("x", maxToolDiagnosticBytes+1) + `"`)},
			{"both members", json.RawMessage(`"diag"`)},
		} {
			items[0]["normalization_failure"] = tc.member
			if tc.name == "both members" {
				items[0]["normalized_arguments"] = json.RawMessage(`{"x":1}`)
			}
			edited, err := json.Marshal(items)
			testassert.NoError(t, err)
			obj["tool_calls"] = edited
			out, err := json.Marshal(obj)
			testassert.NoError(t, err)
			env.Payload = out
			{
				_, err := decodeAssistantEntry(env)
				testassert.Require(t, err != nil, "decode %s must fail", tc.name)
			}
			delete(items[0], "normalized_arguments")
		}
	})
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
	testassert.NoError(t, err)
	{
		_, present := wireObject(t, raw)["metadata"]
		testassert.Require(t, present, "encoded payload %s carries no metadata member", raw)
	}
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryToolResult, Payload: raw}
	decoded, err := decodeToolResultEntry(env)
	testassert.NoError(t, err)
	testassert.Require(t, string(decoded.Metadata) == string(valid), "metadata round-tripped as %s, want the stored bytes verbatim", decoded.Metadata)
	decoded.Metadata[0] = '[' // mutating the decoded copy must not touch stored state
	again, err := decodeToolResultEntry(env)
	testassert.NoError(t, err)
	testassert.Require(t, string(again.Metadata) == string(valid), "metadata after mutating a decoded copy = %s, want the stored bytes", again.Metadata)

	// Absence encodes by omission: no metadata member appears at all.
	bare, err := encodeToolResultEntry(validToolResultEntry(testOpID))
	testassert.NoError(t, err)
	{
		_, present := wireObject(t, bare)["metadata"]
		testassert.Require(t, !present, "metadata-less payload encodes the member: %s", bare)
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
			{
				_, err := encodeToolResultEntry(v)
				testassert.Require(t, errors.Is(err, ErrInvalid), "encode error = %v, want the ErrInvalid class", err)
			}
		})
	}

	// A persisted payload that violates the member rules stays undecodable:
	// null and oversized members are rejected, while a value exactly at the
	// bound decodes.
	rawValid := raw
	{
		_, err := decodeToolResultEntry(Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryToolResult, Payload: rawValid})
		testassert.NoError(t, err)
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
			testassert.NoError(t, err)
			continue
		}
		testassert.Require(t, err != nil, "persisted %s metadata decoded, want rejection", tc.name)
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
	testassert.Require(t, string(got) == want, "owned metadata = %s, want the durable encoded bytes %s", got, want)
	testassert.Require(t, string(raw) == input, "caller input changed to %s, want it unchanged", raw)
	raw[1] = 'x' // mutating the caller's buffer cannot alter the returned bytes
	testassert.Require(t, string(got) == want, "owned metadata after mutating the caller = %s, want the independent encoded bytes %s", got, want)
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
		{"settlement usage without model", func() error {
			v := validSettlementEntry()
			v.Status = OperationFailure
			v.Detail = "d"
			v.Usage = testUsage(1)
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
			v.State.ActiveEffect = &ActiveEffect{Kind: EffectTool, ResultEntryID: hexID(3), ToolCallID: "call-1", FinalArguments: json.RawMessage(`{"x":1}`)}
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
			testassert.Require(t, err != nil && errors.Is(err, ErrInvalid), "expected rejection; rejection = %v, want the ErrInvalid class", err)
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
	testassert.NoError(t, err)
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: raw}
	first, err := decodeAssistantEntry(env)
	testassert.NoError(t, err)
	second, err := decodeAssistantEntry(env)
	testassert.NoError(t, err)

	first.Extra["k"] = json.RawMessage(`"mutated"`)
	first.Content = append(first.Content, model.ContentPart{Kind: model.PartText, Text: "extra"})
	first.ToolCalls[0].ID = "mutated"
	testassert.Require(t, string(second.Extra["k"]) == `{"deep":[1]}` && len(second.Content) == 1 && second.ToolCalls[0].ID == "call-1", "decoded values alias each other")

	// Mutating the caller's payload buffer after decode must never reach the
	// decoded owned values.
	env.Payload[0] = ' '
	testassert.Require(t, string(second.Extra["k"]) == `{"deep":[1]}` && second.Content[0].Text == "hi", "decoded value aliased the caller payload buffer")
}

// TestAdoptedFormsAcrossInsertAndFork extends the result/fork ownership
// coverage over the encoded forms the shared codec adopts: raw Extra bytes
// survive decode-after-insert verbatim, deeply nested argument and metadata
// forms survive unchanged, numeric lexemes stay exact, and invalid UTF-8 in
// typed text is replaced with U+FFFD while ordinary multi-byte text is
// preserved. Producer buffers are clobbered after commit to pin the adopted
// bytes as independent of them.
func TestAdoptedFormsAcrossInsertAndFork(t *testing.T) {
	const (
		verbatimExtra   = `{"path":"a/b","list":[1,2]}`
		nestedExtra     = `{"deep":{"a":[{"b":1},[[2],"c"]]},"keep":true}`
		lexemeExtra     = `{"n":[1,1.0,1e0,9007199254740993,-0]}`
		nestedArguments = `{"deep":{"a":[{"b":1},[[2],"c"]]},"n":[1,1.0,1e0,9007199254740993]}`
		nestedMetadata  = `{"marker":[{"tool":{"nested":[1,{"x":[2,3]}]}}]}`
	)
	// wantReplacedText pins one U+FFFD per invalid byte with the ordinary
	// multi-byte text around it untouched.
	const wantReplacedText = "a\uFFFD\uFFFDb héllo 世界"

	freshRaw := func(s string) json.RawMessage { return json.RawMessage(append([]byte(nil), s...)) }
	invalidText := func() string {
		raw := append([]byte("a"), 0xff, 0xfe)
		return string(append(raw, []byte("b héllo 世界")...))
	}
	invalidRawString := func() json.RawMessage { // one JSON string literal holding the same invalid bytes
		raw := append([]byte(`"a`), 0xff, 0xfe)
		return append(raw, []byte("b héllo 世界\"")...)
	}
	wantInvalidRaw := string(invalidRawString()) // a copy, independent of the producer buffers clobbered below

	assistantFixture := func(entryID, resultID string) assistantEntry {
		call := validToolCallRecord()
		call.ResultEntryID = resultID
		call.Extra = model.Extra{"nested": freshRaw(nestedExtra), "lexemes": freshRaw(lexemeExtra)}
		call.NormalizedArguments = freshRaw(nestedArguments)
		failed := validToolCallRecord()
		failed.ID = "call-2"
		failed.Ordinal = 1
		failed.ResultEntryID = hexID(5)
		failed.NormalizationFailure = "arguments must be one non-null JSON object"
		v := validAssistantEntry(testOpID)
		v.EntryID = entryID
		v.AdvertisedNames = []string{"echo"}
		v.Content = []model.ContentPart{
			{Kind: model.PartText, Text: invalidText(), Extra: model.Extra{"raw": freshRaw(verbatimExtra), "utf8": invalidRawString()}},
			{Kind: model.PartOpaque, OpaqueWireType: "thinking", Extra: model.Extra{"nested": freshRaw(nestedExtra), "lexemes": freshRaw(lexemeExtra)}},
		}
		v.Extra = model.Extra{"msg": freshRaw(nestedExtra)}
		v.ToolCalls = []toolCallRecord{call, failed}
		return v
	}
	resultFixture := func(entryID, assistantID string) toolResultEntry {
		v := validToolResultEntry(testOpID)
		v.EntryID = entryID
		v.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: assistantID}
		v.Evidence = ExecutionUnknown
		v.Metadata = freshRaw(nestedMetadata)
		v.OriginalContent = "raw output"
		v.ShapingFailure = "shaping aborted"
		return v
	}
	// Ownership oracle: clobber the producer's raw buffers in place.
	clobber := func(raws ...json.RawMessage) {
		for _, raw := range raws {
			for i := range raw {
				raw[i] = ' '
			}
		}
	}
	clobberAssistant := func(v assistantEntry) {
		clobber(v.Content[0].Extra["raw"], v.Content[0].Extra["utf8"], v.Content[1].Extra["nested"], v.Content[1].Extra["lexemes"], v.Extra["msg"], v.ToolCalls[0].Extra["nested"], v.ToolCalls[0].NormalizedArguments)
	}
	assertAssistantAdopted := func(t *testing.T, got *assistantEntry) {
		t.Helper()
		testassert.Require(t, got != nil, "%v", "adopted assistant entry is missing")
		testassert.Equal(t, got.Content[0].Text, wantReplacedText)
		testassert.Require(t, string(got.Content[0].Extra["raw"]) == verbatimExtra, "adopted raw Extra = %q, want it verbatim", got.Content[0].Extra["raw"])
		testassert.Require(t, string(got.Content[0].Extra["utf8"]) == wantInvalidRaw, "adopted raw UTF-8 Extra = %q, want the raw bytes verbatim", got.Content[0].Extra["utf8"])
		testassert.Require(t, string(got.Content[1].Extra["nested"]) == nestedExtra, "adopted nested content extra = %q, want the nested form unchanged", got.Content[1].Extra["nested"])
		testassert.Require(t, string(got.Content[1].Extra["lexemes"]) == lexemeExtra, "adopted numeric lexemes = %q, want 9007199254740993 and its siblings exact", got.Content[1].Extra["lexemes"])
		testassert.Require(t, string(got.Extra["msg"]) == nestedExtra, "adopted message extra = %q, want the nested form unchanged", got.Extra["msg"])
		testassert.Require(t, string(got.ToolCalls[0].Extra["nested"]) == nestedExtra, "adopted call extra = %q, want the nested form unchanged", got.ToolCalls[0].Extra["nested"])
		testassert.Require(t, string(got.ToolCalls[0].NormalizedArguments) == nestedArguments, "adopted normalized arguments = %q, want the nested form with exact lexemes unchanged", got.ToolCalls[0].NormalizedArguments)
		testassert.Require(t, got.ToolCalls[1].NormalizationFailure == "arguments must be one non-null JSON object", "adopted normalization failure = %q, want the stored diagnostic verbatim", got.ToolCalls[1].NormalizationFailure)
	}
	assertResultAdopted := func(t *testing.T, got *toolResultEntry) {
		t.Helper()
		testassert.Require(t, got != nil, "%v", "adopted tool result entry is missing")
		testassert.Require(t, string(got.Metadata) == nestedMetadata, "adopted metadata = %q, want the nested form with exact lexemes unchanged", got.Metadata)
		testassert.Equal(t, string(got.Evidence), string(ExecutionUnknown))
		testassert.Require(t, got.OriginalContent == "raw output" && got.ShapingFailure == "shaping aborted", "adopted shaping members = %q/%q, want the producer's values verbatim", got.OriginalContent, got.ShapingFailure)
	}

	t.Run("decode after insert", func(t *testing.T) {
		store := freshSessionStore(t)
		assistant := assistantFixture(hexID(2), hexID(3))
		result := resultFixture(hexID(3), hexID(2))
		var adoptedAssistant, adoptedResult graphEntry
		err := store.Transact(context.Background(), func(tx Transaction) error {
			payload, err := encodeAssistantEntry(assistant)
			if err != nil {
				return err
			}
			if adoptedAssistant, err = insertAndAdopt(tx, testSessionID, EntryDraft{SessionID: testSessionID, ID: hexID(2), OperationID: testOpID, Kind: EntryAssistant, Payload: payload}); err != nil {
				return err
			}
			resultPayload, err := encodeToolResultEntry(result)
			if err != nil {
				return err
			}
			adoptedResult, err = insertAndAdopt(tx, testSessionID, EntryDraft{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Payload: resultPayload})
			return err
		})
		testassert.NoError(t, err)
		clobberAssistant(assistant)
		clobber(result.Metadata)
		assertAssistantAdopted(t, adoptedAssistant.Assistant)
		assertResultAdopted(t, adoptedResult.ToolResult)
	})

	t.Run("fork copy", func(t *testing.T) {
		session := validSessionRecord()
		session.State.CurrentOperationID = testOpID
		op := validOperationRecord()
		input := validInputEntry(testOpID)
		input.EntryID = hexID(1)
		op.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(1)}
		boundary := validInputEntry(testOpID)
		boundary.EntryID = hexID(4)
		assistant := assistantFixture(hexID(2), hexID(3))
		result := resultFixture(hexID(3), hexID(2))
		fixture := &testGraph{
			session: session,
			ops:     []OperationRecord{op},
			entries: []testEntry{
				{env: Entry{SessionID: testSessionID, ID: hexID(1), OperationID: testOpID, Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &input},
				{env: Entry{SessionID: testSessionID, ID: hexID(2), OperationID: testOpID, Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &assistant},
				{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryToolResult, Sequence: 3, CommittedAt: testTime}, toolResult: &result},
				{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryInput, Sequence: 4, CommittedAt: testTime}, input: &boundary},
			},
		}
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		var commit forkCommit
		err := store.Transact(context.Background(), func(tx Transaction) error {
			return h.forkTransaction(tx, hexID(9), fixture.session, ForkRequest{
				SourceSessionID: testSessionID,
				BoundaryEntryID: hexID(4),
				OperationID:     "fork-op-1",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "continue"}},
			}, nil, testCapture(), &commit)
		})
		testassert.NoError(t, err)
		clobberAssistant(assistant)
		clobber(result.Metadata)

		copied := 0
		for i := range commit.entries {
			entry := commit.entries[i]
			switch {
			case entry.Assistant != nil:
				copied++
				assertAssistantAdopted(t, entry.Assistant)
			case entry.ToolResult != nil:
				copied++
				assertResultAdopted(t, entry.ToolResult)
			}
		}
		testassert.Equal(t, copied, 2)
	})
}

// TestTimestampRules proves durable register timestamps: encoded from UTC in
// the landed RFC3339Nano layout, zero rejected, and decoded offsets other
// than UTC rejected.
func TestTimestampRules(t *testing.T) {
	stamped, err := encodeTime(testTime)
	testassert.NoError(t, err)
	testassert.Equal(t, stamped, "2026-01-02T03:04:05.123456789Z")
	{
		_, err := encodeTime(time.Time{})
		testassert.Require(t, err != nil, "zero timestamp encode must fail")
	}
	{
		_, err := encodeTime(time.Date(12000, 1, 2, 3, 4, 5, 0, time.UTC))
		testassert.Require(t, err != nil, "timestamp with year 12000 encode must fail")
	}
	decoded, err := decodeTime(stamped)
	testassert.NoError(t, err)
	testassert.Require(t, decoded.UTC().Equal(testTime), "decoded timestamp = %v", decoded)
	{
		_, err := decodeTime("2026-01-02T03:04:05.123456789+02:00")
		testassert.Require(t, err != nil, "non-UTC offset must be rejected")
	}
	{
		_, err := decodeTime("0001-01-01T00:00:00Z")
		testassert.Require(t, err != nil, "zero timestamp must be rejected")
	}
}

// TestIDShapes proves durable identity validation: session, entry, and result
// IDs are exactly 32 lowercase hexadecimal characters; Operation and tool-call
// identities are non-empty opaque strings.
func TestIDShapes(t *testing.T) {
	{
		err := validateHexID(hexID(1), "id")
		testassert.NoError(t, err)
	}
	for _, bad := range []string{"", strings.Repeat("A", 32), strings.Repeat("g", 32), hexID(1) + "0", hexID(1)[:31]} {
		{
			err := validateHexID(bad, "id")
			testassert.Require(t, err != nil, "id %q must be rejected", bad)
		}
	}
	{
		err := validateOperationIdentity("", "operation id")
		testassert.Require(t, err != nil, "empty operation id must be rejected")
	}
	{
		err := validateOperationIdentity("any opaque caller identity", "operation id")
		testassert.NoError(t, err)
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
		testassert.NoError(t, err)
		return raw
	}

	for _, empty := range [][]string{nil, {}} {
		obj, err := decodePayloadObject(encode(t, empty))
		testassert.NoError(t, err)
		{
			_, present := obj["capabilities"]
			testassert.Require(t, !present, "empty capability selection %#v leaked the capabilities member", empty)
		}
	}

	ordered := []string{"retired-plugin.capability", "cap-mid", "cap-last"}
	obj, err := decodePayloadObject(encode(t, ordered))
	testassert.NoError(t, err)
	{
		got := string(obj["capabilities"])
		testassert.Equal(t, got, `["retired-plugin.capability","cap-mid","cap-last"]`)
	}
	decoded, err := decodeExecutionCapture(obj)
	testassert.NoError(t, err)
	testassert.Require(t, len(decoded.Capabilities) == 3 && decoded.Capabilities[0] == "retired-plugin.capability" && decoded.Capabilities[1] == "cap-mid" && decoded.Capabilities[2] == "cap-last", "decoded capabilities = %q, want the selected order", decoded.Capabilities)
	decoded.Capabilities[0] = "tampered"
	again, err := decodeExecutionCapture(obj)
	testassert.Require(t, err == nil && again.Capabilities[0] == "retired-plugin.capability", "decoded capabilities alias the payload bytes: %q, %v", again.Capabilities, err)

	captureWith := func(capabilities string) map[string]json.RawMessage {
		members, err := decodePayloadObject(encode(t, nil))
		testassert.NoError(t, err)
		if capabilities != "" {
			members["capabilities"] = json.RawMessage(capabilities)
		}
		return members
	}
	{
		got, err := decodeExecutionCapture(captureWith(""))
		testassert.Require(t, err == nil && got.Capabilities == nil, "omitted capabilities = %q, %v, want none", got.Capabilities, err)
	}
	{
		got, err := decodeExecutionCapture(captureWith("[]"))
		testassert.Require(t, err == nil && got.Capabilities == nil, "empty-array capabilities = %q, %v, want none", got.Capabilities, err)
	}
	for _, bad := range []string{"null", `["cap-a","cap-a"]`, `[""]`, `[5]`, `"cap-a"`, `["cap-a",null]`} {
		{
			got, err := decodeExecutionCapture(captureWith(bad))
			testassert.Require(t, err != nil, "capabilities %s decoded to %q, want rejection", bad, got.Capabilities)
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
		testassert.NoError(t, err)
		members, err := decodePayloadObject(raw)
		testassert.NoError(t, err)
		return members
	}

	explicit := encode(t, testCapture())
	{
		got := string(explicit["readonly"])
		testassert.Equal(t, got, "false")
	}
	{
		got := string(explicit["write_dir"])
		testassert.Equal(t, got, `""`)
	}

	v := testCapture()
	v.Readonly = true
	v.WriteDir = "/w/sub"
	decoded, err := decodeExecutionCapture(encode(t, v))
	testassert.NoError(t, err)
	testassert.Require(t, decoded.Readonly == true && decoded.WriteDir == "/w/sub", "round-tripped members = %v %q, want true and %q", decoded.Readonly, decoded.WriteDir, "/w/sub")

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
			{
				got, err := decodeExecutionCapture(members)
				testassert.Require(t, err != nil, "capture decoded to %+v, want rejection", got)
			}
		})
	}
}

// TestExecutionCaptureCompactionMembers pins the durable compaction
// configuration members: context_window, output_reserve and compact are
// required conversation members encoded always, the nested compact object
// carries exactly the model and system_prompt keys with the model in the
// durable two-field form, everything round-trips unchanged, a capture missing
// a member, carrying a non-positive conversation window or reserve, an
// incomplete compact model identity, an empty compact prompt, or a compact
// object carrying a retired budget member is invalid on both the encode and
// decode paths.
func TestExecutionCaptureCompactionMembers(t *testing.T) {
	encode := func(t *testing.T, v ExecutionCapture) map[string]json.RawMessage {
		t.Helper()
		raw, err := encodeExecutionCapture(v)
		testassert.NoError(t, err)
		members, err := decodePayloadObject(raw)
		testassert.NoError(t, err)
		return members
	}

	members := encode(t, testCapture())
	{
		got := string(members["context_window"])
		testassert.Equal(t, got, "4096")
	}
	{
		got := string(members["output_reserve"])
		testassert.Equal(t, got, "2048")
	}
	compactObj, err := decodePayloadObject(members["compact"])
	testassert.NoError(t, err)
	{
		err := rejectUnknownMembers(compactObj, "model", "system_prompt")
		testassert.NoError(t, err)
	}
	for _, key := range []string{"model", "system_prompt"} {
		{
			_, present := compactObj[key]
			testassert.Require(t, present, "encoded compact object misses the %q member", key)
		}
	}
	compactModelObj, err := decodePayloadObject(compactObj["model"])
	testassert.NoError(t, err)
	compactModel, err := decodeModelRef(compactModelObj)
	testassert.NoError(t, err)
	if compactModel != (model.ModelRef{Provider: "cprov", Model: "compact-x"}) {
		t.Fatalf("encoded compact model = %s, want cprov/compact-x", compactModel.String())
	}

	decoded, err := decodeExecutionCapture(members)
	testassert.NoError(t, err)
	testassert.Require(t, decoded.ContextWindow == 4096 && decoded.OutputReserve == 2048, "round-tripped windows = %d %d, want 4096 and 2048", decoded.ContextWindow, decoded.OutputReserve)
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
		{"empty compact prompt", func(v *ExecutionCapture) { v.Compact.SystemPrompt = "" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			v := testCapture()
			mutation.mutate(&v)
			{
				_, err := encodeExecutionCapture(v)
				testassert.Require(t, err != nil, "capture with %s encoded, want rejection", mutation.name)
			}
		})
	}

	for _, missing := range []string{"context_window", "output_reserve", "compact"} {
		m := encode(t, testCapture())
		delete(m, missing)
		{
			got, err := decodeExecutionCapture(m)
			testassert.Require(t, err != nil, "capture missing %q decoded to %+v, want rejection", missing, got)
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
		{
			got, err := decodeExecutionCapture(m)
			testassert.Require(t, err != nil, "capture with %s decoded to %+v, want rejection", mutation.name, got)
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
		{"retired compact context window", func(m map[string]json.RawMessage) { m["context_window"] = json.RawMessage(`2048`) }},
		{"retired compact output reserve", func(m map[string]json.RawMessage) { m["output_reserve"] = json.RawMessage(`1024`) }},
	}
	for _, mutation := range compactMutations {
		t.Run("wire "+mutation.name, func(t *testing.T) {
			m := encode(t, testCapture())
			obj, err := decodePayloadObject(m["compact"])
			testassert.NoError(t, err)
			mutation.mutat(obj)
			raw, err := json.Marshal(obj)
			testassert.NoError(t, err)
			m["compact"] = raw
			{
				got, err := decodeExecutionCapture(m)
				testassert.Require(t, err != nil, "capture with %s decoded to %+v, want rejection", mutation.name, got)
			}
		})
	}
}

// TestOperationSettlementCompactionUsageMarker pins the usage provenance
// marker's wire shape: false encodes by omission, true encodes and round-trips
// unchanged, an omitted member decodes as false, a marker beside no usage is
// inert and valid, and null and non-boolean marker values are rejected through
// the member reader.
func TestOperationSettlementCompactionUsageMarker(t *testing.T) {
	roundTrip := func(t *testing.T, v operationSettlementEntry) operationSettlementEntry {
		t.Helper()
		raw, err := encodeOperationSettlementEntry(v)
		testassert.NoError(t, err)
		decoded, err := decodeOperationSettlementEntry(Entry{
			SessionID:   v.SessionID,
			ID:          v.EntryID,
			OperationID: v.OperationID,
			Kind:        EntryOperationSettlement,
			Payload:     raw,
		})
		testassert.NoError(t, err)
		return decoded
	}

	unmarked := validSettlementEntry()
	raw, err := encodeOperationSettlementEntry(unmarked)
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(string(raw), "compaction_usage"), "false marker encoded as a member: %s", raw)
	{
		got := roundTrip(t, unmarked)
		testassert.Require(t, !got.CompactionUsage, "round-tripped omitted marker = true, want the omitted/false equivalence")
	}

	ref := testModelRef()
	usage := &UsageCount{InputTokens: 4, CachedInputTokens: 2, OutputTokens: 6}
	marked := validSettlementEntry()
	marked.Status = OperationFailure
	marked.Detail = "summary failure"
	marked.Model = &ref
	marked.Usage = usage
	marked.CompactionUsage = true
	raw, err = encodeOperationSettlementEntry(marked)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(string(raw), `"compaction_usage":true`), "marked settlement payload %s, want the true member", raw)
	{
		got := roundTrip(t, marked)
		testassert.Require(t, reflect.DeepEqual(got, marked), "round-tripped settlement = %+v, want %+v", got, marked)
	}

	inert := validSettlementEntry()
	inert.Status = OperationInterruption
	inert.Detail = "interrupted"
	inert.CompactionUsage = true
	{
		got := roundTrip(t, inert)
		testassert.Require(t, got.CompactionUsage && got.Model == nil && got.Usage == nil, "round-tripped marker-without-usage = %+v, want the inert true marker with no usage", got)
	}

	for _, row := range []struct {
		name  string
		value string
	}{
		{"null marker", "null"},
		{"number marker", "1"},
		{"string marker", `"true"`},
	} {
		t.Run(row.name, func(t *testing.T) {
			unmarkedRaw, err := encodeOperationSettlementEntry(unmarked)
			testassert.NoError(t, err)
			members, err := decodePayloadObject(unmarkedRaw)
			testassert.NoError(t, err)
			members["compaction_usage"] = json.RawMessage(row.value)
			raw, err := json.Marshal(members)
			testassert.NoError(t, err)
			{
				_, err := decodeOperationSettlementEntry(Entry{
					SessionID:   unmarked.SessionID,
					ID:          unmarked.EntryID,
					OperationID: unmarked.OperationID,
					Kind:        EntryOperationSettlement,
					Payload:     raw,
				})
				testassert.Require(t, err != nil, "settlement with %s decoded, want rejection", row.name)
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
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(string(childRaw), `"parent_session_id":"`+otherSession()+`"`), "child identity payload lacks parent_session_id: %s", childRaw)
	decoded, err := decodeSessionRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Payload: childRaw})
	testassert.NoError(t, err)
	if decoded.Identity.ParentSessionID != otherSession() {
		t.Fatalf("decoded parent session id = %q, want %q", decoded.Identity.ParentSessionID, otherSession())
	}

	rootRaw, err := encodeSessionRegister(validSessionRecord())
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(string(rootRaw), "parent_session_id"), "root identity carries parent_session_id: %s", rootRaw)

	fork := validSessionRecord()
	fork.Identity.SourceSessionID = otherSession()
	fork.Identity.SourceBoundaryEntryID = otherEntry()
	forkRaw, err := encodeSessionRegister(fork)
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(string(forkRaw), "parent_session_id"), "fork identity carries parent_session_id: %s", forkRaw)
	{
		_, err := decodeSessionRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Payload: forkRaw})
		testassert.NoError(t, err)
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
			{
				_, err := encodeSessionIdentity(tc.value)
				testassert.Require(t, errors.Is(err, ErrInvalid), "error = %v, want ErrInvalid", err)
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
			testassert.NoError(t, err)
			tc.mutate(obj)
			{
				_, err := decodeSessionIdentity(obj)
				testassert.Require(t, err != nil, "expected rejection")
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
	testassert.NoError(t, err)
	env := Entry{SessionID: testSessionID, ID: testEntryID, Kind: EntrySignal, Sequence: 1, CommittedAt: testTime}
	env.Payload = raw
	decoded, err := decodeSignalEntry(env)
	testassert.NoError(t, err)
	testassert.Require(t, decoded.OperationID == "" && decoded.RelatedOperation == nil && (decoded.RelatedMember != nil && decoded.RelatedMember.Kind == "child" && decoded.RelatedMember.ID == childID), "completion signal is not operationless: %+v; decoded related member = %+v, want child %q", decoded, decoded.RelatedMember, childID)
	{
		_, present := wireObject(t, raw)["related_operation"]
		testassert.Require(t, !present, "completion signal carries related_operation: %s", raw)
	}

	jobRaw, err := encodeSignalEntry(func() signalEntry {
		v := validBackgroundCompletionEntry()
		v.RelatedMember = &relatedMember{Kind: "job", ID: jobID}
		return v
	}())
	testassert.NoError(t, err)
	env.Payload = jobRaw
	{
		_, err := decodeSignalEntry(env)
		testassert.NoError(t, err)
	}

	// The fixed kinds reject a related_member member.
	fixedRaw, err := encodeSignalEntry(validSignalEntry(testOpID))
	testassert.NoError(t, err)
	fixedEnv := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntrySignal, Sequence: 1, CommittedAt: testTime}
	fixedEnv.Payload = setKey(fixedRaw, "related_member", json.RawMessage(`{"kind":"child","id":"`+childID+`"}`))
	{
		_, err := decodeSignalEntry(fixedEnv)
		testassert.Require(t, err != nil, "fixed signal with related_member must be rejected")
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
			{
				_, err := encodeSignalEntry(tc.value)
				testassert.Require(t, errors.Is(err, ErrInvalid), "error = %v, want ErrInvalid", err)
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
			{
				_, err := decodeSignalEntry(env)
				testassert.Require(t, err != nil, "expected rejection")
			}
		})
	}
}

// TestCompactionEntryPayloadRules proves the compaction entry payload's
// closed shape: required members with exact keys, the optional operation
// identity and usage members, the fork-prefix usage rule, and the value
// rules enforced on both codec sides.
func TestCompactionEntryPayloadRules(t *testing.T) {
	t.Run("round trips owned with and without fact usage", func(t *testing.T) {
		for _, withUsage := range []bool{false, true} {
			v := validCompactionEntry(testOpID)
			if withUsage {
				v.Facts[0].Usage = &UsageCount{InputTokens: 11, CachedInputTokens: 4, OutputTokens: 7}
			}
			raw, err := encodeContextCheckpointEntry(v)
			testassert.Require(t, err == nil, "encode (usage %v): %v", withUsage, err)
			env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime, Payload: raw}
			got, err := decodeContextCheckpointEntry(env)
			testassert.Require(t, err == nil, "decode (usage %v): %v", withUsage, err)
			testassert.Require(t, (got.Facts[0].Usage == nil) == (v.Facts[0].Usage == nil), "decoded fact usage presence %+v, want %+v", got.Facts[0].Usage, v.Facts[0].Usage)
			if withUsage && *got.Facts[0].Usage != *v.Facts[0].Usage {
				t.Fatalf("decoded fact usage %+v, want %+v", *got.Facts[0].Usage, *v.Facts[0].Usage)
			}
			if !reflect.DeepEqual(got, v) {
				t.Fatalf("decoded compaction entry %+v, want %+v", got, v)
			}
			again, err := encodeContextCheckpointEntry(got)
			testassert.Require(t, err == nil, "re-encode (usage %v): %v", withUsage, err)
			testassert.Require(t, string(again) == string(raw), "re-encoded bytes %s, want %s", again, raw)
		}
	})

	t.Run("round trips operationless fork copy", func(t *testing.T) {
		raw, err := encodeContextCheckpointEntry(validCompactionEntry(""))
		testassert.NoError(t, err)
		env := Entry{SessionID: testSessionID, ID: testEntryID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime, Payload: raw}
		got, err := decodeContextCheckpointEntry(env)
		testassert.NoError(t, err)
		testassert.Require(t, got.OperationID == "" && got.Facts[0].Usage == nil, "operationless decode = %+v", got)
	})

	t.Run("encode rejects invalid values", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*contextCheckpointEntry)
		}{
			{"non-hex session id", func(v *contextCheckpointEntry) { v.SessionID = "ghost" }},
			{"non-hex entry id", func(v *contextCheckpointEntry) { v.EntryID = "ghost" }},
			{"non-hex boundary entry id", func(v *contextCheckpointEntry) { v.BoundaryEntryID = "ghost" }},
			{"empty strategy id", func(v *contextCheckpointEntry) { v.StrategyID = "" }},
			{"empty messages", func(v *contextCheckpointEntry) { v.Messages = nil }},
			{"unpaired tool call", func(v *contextCheckpointEntry) {
				v.Messages = []model.Message{{Role: model.RoleAssistant, Source: testModelRef(), ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}}}}
			}},
			{"unpaired tool result", func(v *contextCheckpointEntry) {
				v.Messages = []model.Message{{Role: model.RoleTool, ToolCallID: "call-1", Content: []model.ContentPart{{Kind: model.PartText, Text: "result"}}}}
			}},
			{"empty boundary entry id", func(v *contextCheckpointEntry) { v.BoundaryEntryID = "" }},
			{"incomplete fact model", func(v *contextCheckpointEntry) { v.Facts[0].Model = model.ModelRef{Provider: "prov"} }},
			{"empty configuration revision", func(v *contextCheckpointEntry) { v.ConfigurationRevision = "" }},
			{"operationless with fact usage", func(v *contextCheckpointEntry) { v.OperationID = ""; v.Facts[0].Usage = testUsage(1) }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				v := validCompactionEntry(testOpID)
				tc.mutate(&v)
				_, err := encodeContextCheckpointEntry(v)
				testassert.Require(t, err != nil && errors.Is(err, ErrInvalid), "expected rejection; error %v is not ErrInvalid", err)
			})
		}
	})

	t.Run("decode rejects invalid wire", func(t *testing.T) {
		raw, err := encodeContextCheckpointEntry(validCompactionEntry(testOpID))
		testassert.NoError(t, err)
		env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime}
		rejections := []struct {
			name    string
			payload json.RawMessage
		}{
			{"missing strategy id", dropKey(raw, "strategy_id")},
			{"empty strategy id", setKey(raw, "strategy_id", json.RawMessage(`""`))},
			{"missing messages", dropKey(raw, "messages")},
			{"empty messages", setKey(raw, "messages", json.RawMessage(`[]`))},
			{"message with unpaired tool call", setKey(raw, "messages", json.RawMessage(`[{"role":"assistant","source":{"provider":"prov","model":"gpt-x"},"content":[{"kind":"text","text":"hi"}],"tool_calls":[{"id":"call-1","name":"echo","arguments_base64":"e30="}]}]`))},
			{"message tool call with empty id", setKey(raw, "messages", json.RawMessage(`[{"role":"assistant","source":{"provider":"prov","model":"gpt-x"},"tool_calls":[{"id":"","name":"echo","arguments_base64":"e30="}]}]`))},
			{"message tool call with non-base64 arguments", setKey(raw, "messages", json.RawMessage(`[{"role":"assistant","source":{"provider":"prov","model":"gpt-x"},"tool_calls":[{"id":"call-1","name":"echo","arguments_base64":"!!!"}]}]`))},
			{"missing boundary entry id", dropKey(raw, "boundary_entry_id")},
			{"empty boundary entry id", setKey(raw, "boundary_entry_id", json.RawMessage(`""`))},
			{"null boundary entry id", setKey(raw, "boundary_entry_id", json.RawMessage(`null`))},
			{"non-hex boundary entry id", setKey(raw, "boundary_entry_id", json.RawMessage(`"zz"`))},
			{"missing facts", dropKey(raw, "facts")},
			{"null facts", setKey(raw, "facts", json.RawMessage(`null`))},
			{"wrong container facts", setKey(raw, "facts", json.RawMessage(`0`))},
			{"fact missing request id", setKey(raw, "facts", json.RawMessage(`[{"model":{"provider":"prov","model":"gpt-x"},"input":[{"role":"user","content":[{"kind":"text","text":"x"}]}]}]`))},
			{"fact incomplete model", setKey(raw, "facts", json.RawMessage(`[{"request_id":"maintenance-one","model":{"provider":"prov"},"input":[{"role":"user","content":[{"kind":"text","text":"x"}]}]}]`))},
			{"fact missing input", setKey(raw, "facts", json.RawMessage(`[{"request_id":"maintenance-one","model":{"provider":"prov","model":"gpt-x"}}]`))},
			{"fact input wrong shape", setKey(raw, "facts", json.RawMessage(`[{"request_id":"maintenance-one","model":{"provider":"prov","model":"gpt-x"},"input":0}]`))},
			{"missing configuration revision", dropKey(raw, "configuration_revision")},
			{"empty configuration revision", setKey(raw, "configuration_revision", json.RawMessage(`""`))},
			{"null configuration revision", setKey(raw, "configuration_revision", json.RawMessage(`null`))},
			{"null operation id", setKey(raw, "operation_id", json.RawMessage(`null`))},
			{"empty operation id", setKey(raw, "operation_id", json.RawMessage(`""`))},
			{"missing entry id", dropKey(raw, "entry_id")},
		}
		for _, tc := range rejections {
			t.Run(tc.name, func(t *testing.T) {
				env.Payload = tc.payload
				{
					_, err := decodeContextCheckpointEntry(env)
					testassert.Require(t, err != nil, "expected rejection")
				}
			})
		}
	})

	t.Run("operationless with stored fact usage is rejected", func(t *testing.T) {
		env := Entry{SessionID: testSessionID, ID: testEntryID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime}
		raw, err := encodeContextCheckpointEntry(validCompactionEntry(""))
		testassert.NoError(t, err)
		env.Payload = setKey(raw, "facts", json.RawMessage(`[{"request_id":"maintenance-one","model":{"provider":"prov","model":"gpt-x"},"input":[{"role":"user","content":[{"kind":"text","text":"x"}]}],"usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":0}}]`))
		{
			_, err := decodeContextCheckpointEntry(env)
			testassert.Require(t, err != nil, "operationless compaction with stored fact usage must be rejected")
		}
	})
}

// TestSessionStateCheckpointEntryIDRules proves the Session state's optional
// compaction_entry_id member: omission-encoded, hex-validated when present,
// and rejected as null, empty, or a non-hex string on both codec sides.
func TestSessionStateCheckpointEntryIDRules(t *testing.T) {
	t.Run("round trips with the field set", func(t *testing.T) {
		rec := validSessionRecord()
		rec.State.CheckpointEntryID = hexID(3)
		raw, err := encodeSessionRegister(rec)
		testassert.NoError(t, err)
		reg := Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Revision: 1, Payload: raw}
		got, err := decodeSessionRegister(reg)
		testassert.NoError(t, err)
		if got.State.CheckpointEntryID != hexID(3) {
			t.Fatalf("decoded CheckpointEntryID %q, want %q", got.State.CheckpointEntryID, hexID(3))
		}
	})

	t.Run("omits the member when empty", func(t *testing.T) {
		raw, err := encodeSessionRegister(validSessionRecord())
		testassert.NoError(t, err)
		state := wireObject(t, mustState(t, raw))
		{
			_, present := state["checkpoint_entry_id"]
			testassert.Require(t, !present, "empty CheckpointEntryID must encode by omission")
		}
	})

	patchState := func(t *testing.T, mutate func(obj map[string]json.RawMessage)) json.RawMessage {
		t.Helper()
		raw, err := encodeSessionRegister(validSessionRecord())
		testassert.NoError(t, err)
		obj := wireObject(t, raw)
		state, err := decodePayloadObject(mustState(t, raw))
		testassert.NoError(t, err)
		mutate(state)
		patchedState, err := json.Marshal(state)
		testassert.NoError(t, err)
		obj["state"] = patchedState
		patched, err := json.Marshal(obj)
		testassert.NoError(t, err)
		return patched
	}

	t.Run("decode rejections", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(obj map[string]json.RawMessage)
		}{
			{"non-hex id", func(obj map[string]json.RawMessage) { obj["checkpoint_entry_id"] = json.RawMessage(`"ghost"`) }},
			{"empty id", func(obj map[string]json.RawMessage) { obj["checkpoint_entry_id"] = json.RawMessage(`""`) }},
			{"null id", func(obj map[string]json.RawMessage) { obj["checkpoint_entry_id"] = json.RawMessage(`null`) }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				reg := Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Revision: 1, Payload: patchState(t, tc.mutate)}
				{
					_, err := decodeSessionRegister(reg)
					testassert.Require(t, err != nil, "expected rejection")
				}
			})
		}
	})

	t.Run("encode rejections", func(t *testing.T) {
		rec := validSessionRecord()
		rec.State.CheckpointEntryID = "ghost"
		{
			_, err := encodeSessionRegister(rec)
			testassert.Require(t, errors.Is(err, ErrInvalid), "error %v, want ErrInvalid", err)
		}
	})
}

// TestHookResultKnowledgeAndBindingDomains proves the shared hook-knowledge
// and binding-input codec rules over both hook families: failure and
// interruption settle with any of the three knowledge values, success requires
// observed, absence and an invalid enum reject on both codec sides; a tool
// binding carries only its opaque consumed bytes (canonical-strict, valid
// empty allowed), a request binding carries only its consumed evidence record,
// and every cross-binding member rejects.
func TestHookResultKnowledgeAndBindingDomains(t *testing.T) {
	t.Run("knowledge values settle on failure and interruption", func(t *testing.T) {
		for _, knowledge := range []ExecutionEvidence{ExecutionNotStarted, ExecutionObserved, ExecutionUnknown} {
			for _, status := range []hookResultStatus{hookFailed, hookInterrupted} {
				v := validHookResultEntry(status)
				v.Evidence = knowledge
				{
					raw, err := encodeHookResultEntry(v)
					testassert.Require(t, err == nil, "encode %s with %s: %v", status, knowledge, err)
					decoded, derr := decodeHookResultEntry(Entry{SessionID: testSessionID, ID: v.EntryID, OperationID: testOpID, Kind: EntryHookResult, Payload: raw})
					testassert.Require(t, derr == nil && decoded.Evidence == knowledge, "round-trip %s: %v", knowledge, derr)
				}
			}
		}
	})
	t.Run("success requires observed; invalid and absent knowledge reject", func(t *testing.T) {
		for _, knowledge := range []ExecutionEvidence{ExecutionNotStarted, ExecutionUnknown, ExecutionEvidence("all"), ""} {
			v := validHookResultEntry(hookSucceeded)
			v.Evidence = knowledge
			{
				_, err := encodeHookResultEntry(v)
				testassert.Require(t, err != nil, "success with knowledge %q must reject", knowledge)
			}
		}
		bad := validHookResultEntry(hookFailed)
		bad.Evidence = ExecutionEvidence("all")
		{
			raw, err := encodeHookResultEntry(bad)
			testassert.Require(t, err != nil && raw == nil, "invalid enum must reject at encode")
		}
		failed := validHookResultEntry(hookFailed)
		failedRaw, err := encodeHookResultEntry(failed)
		testassert.NoError(t, err)
		{
			_, derr := decodeHookResultEntry(Entry{SessionID: testSessionID, ID: failed.EntryID, OperationID: testOpID, Kind: EntryHookResult, Payload: setKey(failedRaw, "evidence", json.RawMessage(`"all"`))})
			testassert.Require(t, derr != nil, "decode-side invalid knowledge enum must reject")
		}
		{
			_, derr := decodeHookResultEntry(Entry{SessionID: testSessionID, ID: failed.EntryID, OperationID: testOpID, Kind: EntryHookResult, Payload: setKey(failedRaw, "evidence", nil)})
			testassert.Require(t, derr != nil, "decode-side absent knowledge must reject")
		}
		{
			decoded, derr := decodeHookResultEntry(Entry{SessionID: testSessionID, ID: failed.EntryID, OperationID: testOpID, Kind: EntryHookResult, Payload: setKey(failedRaw, "evidence", json.RawMessage(`"unknown"`))})
			testassert.Require(t, derr == nil && decoded.Evidence == ExecutionUnknown, "decode-side unknown: %v", derr)
		}
	})
	t.Run("tool binding carries only opaque consumed bytes", func(t *testing.T) {
		empty := validHookResultEntry(hookFailed)
		empty.InputArgumentsBase64 = "" // a valid empty opaque input survives
		{
			raw, err := encodeHookResultEntry(empty)
			testassert.NoError(t, err)
			decoded, derr := decodeHookResultEntry(Entry{SessionID: testSessionID, ID: empty.EntryID, OperationID: testOpID, Kind: EntryHookResult, Payload: raw})
			testassert.Require(t, derr == nil && decoded.InputArgumentsBase64 == "", "empty consumed bytes round-trip: %v", derr)
		}
		{
			_, err := decodeHookResultEntry(Entry{SessionID: testSessionID, ID: empty.EntryID, OperationID: testOpID, Kind: EntryHookResult, Payload: setKey(func() json.RawMessage {
				raw, err := encodeHookResultEntry(empty)
				testassert.NoError(t, err)
				return raw
			}(), "input_arguments_base64", json.RawMessage(`"!!!"`))})
			testassert.Require(t, err != nil, "non-canonical consumed bytes must reject")
		}
		crossed := validHookResultEntry(hookSucceeded)
		crossed.InputEvidence = &RequestEvidence{BoundaryEntryID: hexID(1)}
		{
			_, err := encodeHookResultEntry(crossed)
			testassert.Require(t, err != nil, "a tool-bound result carrying consumed evidence must reject")
		}
	})
	t.Run("request binding carries only consumed evidence", func(t *testing.T) {
		{
			_, err := encodeHookResultEntry(validRequestBoundHookResult(hookSucceeded))
			testassert.NoError(t, err)
		}
		missing := validRequestBoundHookResult(hookFailed)
		missing.InputEvidence = nil
		{
			_, err := encodeHookResultEntry(missing)
			testassert.Require(t, err != nil, "a request-bound result without consumed evidence must reject")
		}
		bytes := validRequestBoundHookResult(hookFailed)
		bytes.InputArgumentsBase64 = base64.StdEncoding.EncodeToString([]byte(`{}`))
		{
			_, err := encodeHookResultEntry(bytes)
			testassert.Require(t, err != nil, "a request-bound result carrying argument bytes must reject")
		}
		// Wire level: the member is forbidden outright for a non-tool
		// binding. The check runs on raw membership before the base64 codec,
		// so every present value — including the string that decodes to the
		// struct's zero — rejects through the same membership rule.
		reqRaw, rerr := encodeHookResultEntry(validRequestBoundHookResult(hookFailed))
		testassert.NoError(t, rerr)
		for _, tc := range []struct {
			name  string
			value json.RawMessage
		}{
			{"present-empty", json.RawMessage(`""`)},
			{"null", json.RawMessage(`null`)},
			{"wrong type", json.RawMessage(`5`)},
			{"canonical base64 bytes", json.RawMessage(`"eyJ4IjoxfQ=="`)},
		} {
			_, derr := decodeHookResultEntry(Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryHookResult, Payload: setKey(reqRaw, "input_arguments_base64", tc.value)})
			testassert.Require(t, derr != nil && strings.Contains(derr.Error(), `forbids the "input_arguments_base64" member`),
				"request-bound %s member decode = %v, want the membership rejection", tc.name, derr)
		}
		{
			// Nearest positive sibling: a tool-bound result keeps its opaque
			// malformed-JSON consumed bytes exactly.
			opaque := validHookResultEntry(hookFailed)
			opaque.InputArgumentsBase64 = base64.StdEncoding.EncodeToString([]byte("not json"))
			raw, err := encodeHookResultEntry(opaque)
			testassert.NoError(t, err)
			decoded, derr := decodeHookResultEntry(Entry{SessionID: testSessionID, ID: opaque.EntryID, OperationID: testOpID, Kind: EntryHookResult, Payload: raw})
			testassert.Require(t, derr == nil && decoded.InputArgumentsBase64 == opaque.InputArgumentsBase64,
				"tool-bound opaque malformed-JSON bytes round-trip: %v", derr)
		}
		args := validRequestBoundHookResult(hookSucceeded)
		args.Arguments = json.RawMessage(`{}`)
		{
			_, err := encodeHookResultEntry(args)
			testassert.Require(t, err != nil, "a request-bound result carrying arguments must reject")
		}
		both := validHookResultEntry(hookSucceeded)
		both.RequestID = "request-one"
		{
			_, err := encodeHookResultEntry(both)
			testassert.Require(t, err != nil, "a result binding both a call and a request must reject")
		}
		neither := validHookResultEntry(hookSucceeded)
		neither.ToolCallID = ""
		{
			_, err := encodeHookResultEntry(neither)
			testassert.Require(t, err != nil, "a result binding neither must reject")
		}
	})
	t.Run("declared knowledge reaches the validator unchanged and is never coerced", func(t *testing.T) {
		declared := &EffectError{Evidence: ExecutionEvidence("all"), Err: errors.New("binding broke")}
		testassert.Equal(t, declaredExecutionKnowledge(declared), ExecutionEvidence("all"))
		unknown := &EffectError{Evidence: ExecutionUnknown, Err: errors.New("binding broke")}
		testassert.Equal(t, declaredExecutionKnowledge(unknown), ExecutionUnknown)
		testassert.Equal(t, declaredExecutionKnowledge(errors.New("native failure")), ExecutionObserved)
	})
}

// TestAssistantEntryRequestEvidenceRequiredMembers proves the assistant
// payload's required ownership members on both codec sides: the producing
// request identity, the advertised view (present-empty included; null and
// absence reject), and the final effective request evidence with its required
// canonical boundary; decoded values stay deep-owned from the payload bytes.
func TestAssistantEntryRequestEvidenceRequiredMembers(t *testing.T) {
	raw, err := encodeAssistantEntry(validAssistantEntry(testOpID))
	testassert.NoError(t, err)
	env := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: raw}
	decoded, err := decodeAssistantEntry(env)
	testassert.NoError(t, err)
	testassert.Require(t, decoded.RequestID != "" && decoded.AdvertisedNames != nil && len(decoded.AdvertisedNames) == 0 &&
		decoded.Evidence != nil && decoded.Evidence.BoundaryEntryID == hexID(1), "decoded assistant = %+v, want the request identity, the present-empty view, and the consumed evidence", decoded)
	for _, name := range []string{"request_id", "advertised_names", "evidence"} {
		{
			_, derr := decodeAssistantEntry(Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: setKey(raw, name, nil)})
			testassert.Require(t, derr != nil, "absence of %s must reject", name)
		}
	}
	{
		_, derr := decodeAssistantEntry(Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: setKey(raw, "advertised_names", json.RawMessage(`null`))})
		testassert.Require(t, derr != nil, "a null advertised_names must reject")
	}
	{
		_, derr := decodeAssistantEntry(Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: setKey(raw, "evidence", json.RawMessage(`{"checkpoint_entry_id":""}`))})
		testassert.Require(t, derr != nil, "evidence without its required canonical boundary must reject")
	}
	// Present-empty prompt contribution survives the wire as present.
	v := validAssistantEntry(testOpID)
	empty := ""
	v.Evidence.Prompt = &empty
	v.Evidence.ToolNames = []string{}
	raw, err = encodeAssistantEntry(v)
	testassert.NoError(t, err)
	decoded, err = decodeAssistantEntry(Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: raw})
	testassert.NoError(t, err)
	testassert.Require(t, decoded.Evidence.Prompt != nil && *decoded.Evidence.Prompt == "", "prompt contribution = %+v, want the present-empty prompt retained", decoded.Evidence.Prompt)
	testassert.Require(t, decoded.Evidence.ToolNames != nil && len(decoded.Evidence.ToolNames) == 0, "tool view = %+v, want the present-empty view retained", decoded.Evidence.ToolNames)
	// Decoded evidence is deep-owned: mutating it cannot reach a later decode.
	decoded.Evidence.BoundaryEntryID = hexID(9)
	if len(decoded.Evidence.HookIDs) > 0 {
		decoded.Evidence.HookIDs[0].HookID = "mutated"
	}
	again, err := decodeAssistantEntry(Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryAssistant, Payload: raw})
	testassert.NoError(t, err)
	testassert.Require(t, again.Evidence.BoundaryEntryID == hexID(1) && again.Evidence.Prompt != nil && *again.Evidence.Prompt == "", "payload bytes changed by a decoded mutation")
}

// TestRequestEvidenceContributionMessageDomain proves the shared message gate
// at the evidence boundary: a contribution carrying every permitted shared
// message field round-trips byte-faithfully through the evidence codec, and
// tool execution members reject outright on both codec sides — contributions
// never become historical tool execution.
func TestRequestEvidenceContributionMessageDomain(t *testing.T) {
	owned, err := model.NewMessage(model.Message{
		Role:    model.RoleAssistant,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "carried"}},
		Refusal: "declined part",
		Name:    "contributor",
		Source:  testModelRef(),
		Extra:   model.Extra{"note": json.RawMessage(`{"kept":1}`)},
	})
	testassert.NoError(t, err)
	v := RequestEvidence{
		BoundaryEntryID:   hexID(1),
		CheckpointEntryID: hexID(2),
		Messages:          []model.Message{owned},
		HookIDs:           []HookIdentity{{ExtensionID: "ext.one", HookID: "hook.one"}},
	}
	raw, err := encodeRequestEvidence(v)
	testassert.NoError(t, err)
	decoded, err := decodeRequestEvidence(raw)
	testassert.NoError(t, err)
	testassert.Require(t, len(decoded.Messages) == 1, "decoded messages = %+v, want the recorded contribution", decoded.Messages)
	m := decoded.Messages[0]
	testassert.Require(t, m.Role == model.RoleAssistant && m.TextContent() == "carried" && m.Refusal == "declined part" &&
		m.Name == "contributor" && m.Source == testModelRef() && string(m.Extra["note"]) == `{"kept":1}`,
		"decoded contribution = %+v, want every permitted shared field retained", m)
	withCalls, cerr := model.NewMessage(model.Message{
		Role:      model.RoleAssistant,
		Content:   []model.ContentPart{{Kind: model.PartText, Text: "sneaky"}},
		Source:    testModelRef(),
		ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}},
	})
	testassert.Require(t, cerr == nil, "constructor: %v", cerr)
	bad := v
	bad.Messages = []model.Message{withCalls}
	{
		_, err := encodeRequestEvidence(bad)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "tool calls"), "encode-side tool-call contribution: %v", err)
	}
	withCallID, cerr := model.NewMessage(model.Message{Role: model.RoleTool, Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}, ToolCallID: "call-1"})
	testassert.Require(t, cerr == nil, "constructor: %v", cerr)
	bad2 := v
	bad2.Messages = []model.Message{withCallID}
	{
		_, err := encodeRequestEvidence(bad2)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "call id"), "encode-side tool-result call id: %v", err)
	}
	{
		badRaw := setKey(raw, "messages", json.RawMessage(`[{"role":"assistant","content":[{"kind":"text","text":"wire"}],"tool_calls":[{"id":"c","name":"echo","arguments":"{}"}]}]`))
		_, err := decodeRequestEvidence(badRaw)
		testassert.Require(t, err != nil, "decode-side tool-call contribution must reject")
	}
}

// TestRequestConsequencePresenceDomain proves the consequence codec's presence
// semantics: a present-empty prompt and a present-empty or full tool selection
// survive the wire exactly, absent members stay absent, a success result
// missing its consequence rejects, and failure states never carry one.
func TestRequestConsequencePresenceDomain(t *testing.T) {
	empty := ""
	v := validRequestBoundHookResult(hookSucceeded)
	v.Consequence = &RequestConsequence{Prompt: &empty, ToolNames: []string{}}
	raw, err := encodeHookResultEntry(v)
	testassert.NoError(t, err)
	decoded, err := decodeHookResultEntry(Entry{SessionID: testSessionID, ID: v.EntryID, OperationID: testOpID, Kind: EntryHookResult, Payload: raw})
	testassert.NoError(t, err)
	testassert.Require(t, decoded.Consequence != nil && decoded.Consequence.Prompt != nil && *decoded.Consequence.Prompt == "" &&
		decoded.Consequence.ToolNames != nil && len(decoded.Consequence.ToolNames) == 0 && decoded.Consequence.Messages == nil,
		"decoded consequence = %+v, want present-empty prompt and present-empty selection retained", decoded.Consequence)
	missing := v
	missing.Consequence = nil
	{
		_, err := encodeHookResultEntry(missing)
		testassert.Require(t, err != nil, "a success result without its consequence must reject")
	}
	carrying := validRequestBoundHookResult(hookInterrupted)
	consequence := RequestConsequence{}
	carrying.Consequence = &consequence
	{
		_, err := encodeHookResultEntry(carrying)
		testassert.Require(t, err != nil, "an interrupted result carrying a consequence must reject")
	}
}

// TestMaintenanceConsumedRequestIdentityDomain proves the consumed source's
// codec domain on both record families: absence is the valid manual shape, a
// present value must be a non-empty identity (empty and null reject on the
// wire, and a value set on the Go side encodes), and a present identity
// round-trips unchanged.
func TestMaintenanceConsumedRequestIdentityDomain(t *testing.T) {
	t.Run("compaction", func(t *testing.T) {
		absent := validCompactionEntry(testOpID)
		absent.ConsumedRequestID = ""
		raw, err := encodeContextCheckpointEntry(absent)
		testassert.NoError(t, err)
		testassert.Require(t, !strings.Contains(string(raw), "consumed_request_id"), "encode-side absence: %s", raw)
		decoded, err := decodeContextCheckpointEntry(Entry{SessionID: testSessionID, ID: absent.EntryID, OperationID: testOpID, Kind: EntryCompaction, Payload: raw})
		testassert.NoError(t, err)
		testassert.Equal(t, decoded.ConsumedRequestID, "")
		source := validCompactionEntry(testOpID)
		source.ConsumedRequestID = "request-source"
		raw, err = encodeContextCheckpointEntry(source)
		testassert.NoError(t, err)
		decoded, err = decodeContextCheckpointEntry(Entry{SessionID: testSessionID, ID: source.EntryID, OperationID: testOpID, Kind: EntryCompaction, Payload: raw})
		testassert.NoError(t, err)
		testassert.Equal(t, decoded.ConsumedRequestID, "request-source")
		{
			_, derr := decodeContextCheckpointEntry(Entry{SessionID: testSessionID, ID: source.EntryID, OperationID: testOpID, Kind: EntryCompaction, Payload: setKey(raw, "consumed_request_id", json.RawMessage(`""`))})
			testassert.Require(t, derr != nil, "a present-empty consumed source must reject")
		}
		{
			_, derr := decodeContextCheckpointEntry(Entry{SessionID: testSessionID, ID: source.EntryID, OperationID: testOpID, Kind: EntryCompaction, Payload: setKey(raw, "consumed_request_id", json.RawMessage(`null`))})
			testassert.Require(t, derr != nil, "a null consumed source must reject")
		}
	})
	t.Run("settlement", func(t *testing.T) {
		base := validSettlementEntry()
		base.RequestID = "request-producer"
		base.ConsumedRequestID = "request-source"
		base.Evidence = &RequestEvidence{BoundaryEntryID: hexID(1)}
		base.CompactionUsage = true // the maintenance provenance the consumed identity belongs to
		raw, err := encodeOperationSettlementEntry(base)
		testassert.NoError(t, err)
		{
			// Forbidden siblings: a consumed source attached to no evidence,
			// or to an unmarked (non-maintenance) settlement.
			noEvidence := base
			noEvidence.Evidence = nil
			_, cerr := encodeOperationSettlementEntry(noEvidence)
			testassert.Require(t, cerr != nil && strings.Contains(cerr.Error(), "maintenance evidence provenance"), "encode = %v, want the source-evidence relation to reject", cerr)
			unmarked := base
			unmarked.CompactionUsage = false
			_, cerr = encodeOperationSettlementEntry(unmarked)
			testassert.Require(t, cerr != nil && strings.Contains(cerr.Error(), "maintenance evidence provenance"), "encode = %v, want the unmarked settlement to reject a consumed source", cerr)
			valid := base
			valid.ConsumedRequestID = ""
			_, verr := encodeOperationSettlementEntry(valid)
			testassert.NoError(t, verr) // the ordinary evidence-only maintenance settlement stays valid
		}
		env := Entry{SessionID: testSessionID, ID: base.EntryID, OperationID: testOpID, Kind: EntryOperationSettlement, Payload: raw}
		decoded, err := decodeOperationSettlementEntry(env)
		testassert.NoError(t, err)
		testassert.Equal(t, decoded.ConsumedRequestID, "request-source")
		{
			_, derr := decodeOperationSettlementEntry(Entry{SessionID: testSessionID, ID: base.EntryID, OperationID: testOpID, Kind: EntryOperationSettlement, Payload: setKey(raw, "consumed_request_id", json.RawMessage(`""`))})
			testassert.Require(t, derr != nil, "a present-empty consumed source must reject")
		}
		{
			_, derr := decodeOperationSettlementEntry(Entry{SessionID: testSessionID, ID: base.EntryID, OperationID: testOpID, Kind: EntryOperationSettlement, Payload: setKey(raw, "consumed_request_id", json.RawMessage(`null`))})
			testassert.Require(t, derr != nil, "a null consumed source must reject")
		}
		{
			plain := base
			plain.ConsumedRequestID = ""
			plainRaw, err := encodeOperationSettlementEntry(plain)
			testassert.NoError(t, err)
			testassert.Require(t, !strings.Contains(string(plainRaw), "consumed_request_id"), "encode-side absence: %s", plainRaw)
		}
	})
}

// TestForbiddenInputArgumentsMembershipReasonUniformity pins the correction's
// axis at the owner: for every binding that forbids the input_arguments_base64
// member, every present value — present-empty, null, a wrong type, or even
// canonical base64 — rejects through the same membership rule, never through
// base64 validation or a later field-value rule.
func TestForbiddenInputArgumentsMembershipReasonUniformity(t *testing.T) {
	values := []struct {
		name  string
		value json.RawMessage
	}{
		{"present-empty", json.RawMessage(`""`)},
		{"null", json.RawMessage(`null`)},
		{"wrong type", json.RawMessage(`5`)},
		{"canonical base64 bytes", json.RawMessage(`"eyJ4IjoxfQ=="`)},
	}
	{
		raw, err := encodeHookResultEntry(validRequestBoundHookResult(hookFailed))
		testassert.NoError(t, err)
		for _, c := range values {
			_, derr := decodeHookResultEntry(Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryHookResult, Payload: setKey(raw, "input_arguments_base64", c.value)})
			testassert.Require(t, derr != nil && strings.Contains(derr.Error(), `hook result binding forbids the "input_arguments_base64" member`),
				"request-bound hook result %s member = %v, want the membership rule", c.name, derr)
		}
	}
	bases := []struct {
		name   string
		effect ActiveEffect
	}{
		{"request-bound hook intent", ActiveEffect{Kind: EffectHook, ResultEntryID: hexID(9), RequestID: "request-one", HookID: "cap.hook.one", ExtensionID: "cap-a", Evidence: &RequestEvidence{BoundaryEntryID: hexID(1)}}},
		{"conversation model intent", ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(9), RequestID: "request-one", Model: &model.ModelRef{Provider: "prov", Model: "m"}, Purpose: InvocationPurposeConversation, Evidence: &RequestEvidence{BoundaryEntryID: hexID(1)}}},
		{"maintenance model intent", ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(9), RequestID: "maintenance-one", Model: &model.ModelRef{Provider: "prov", Model: "compact"}, Purpose: InvocationPurposeMaintenance}},
		{"concrete tool intent", ActiveEffect{Kind: EffectTool, ResultEntryID: hexID(9), ToolCallID: "call-1", FinalArguments: json.RawMessage(`{}`)}},
	}
	for _, b := range bases {
		raw, err := encodeActiveEffect(b.effect)
		testassert.NoError(t, err)
		for _, c := range values {
			_, derr := decodeActiveEffect(wireObject(t, setKey(raw, "input_arguments_base64", c.value)))
			testassert.Require(t, derr != nil && strings.Contains(derr.Error(), `active effect binding forbids the "input_arguments_base64" member`),
				"%s %s member = %v, want the membership rule", b.name, c.name, derr)
		}
	}
}

// TestStrategyCaptureSettingsRawAcceptance pins the strategy settings as
// arbitrary owned raw JSON under one accepted set across write and read: the
// syntax-only acceptance admits null and large numbers, an accepted value
// keeps its original bytes apart from the ordinary enclosing encoding, and
// absence stays absent. A value accepted at admission survives the cold read
// and recovery unchanged.
func TestStrategyCaptureSettingsRawAcceptance(t *testing.T) {
	encode := func(t *testing.T, settings json.RawMessage) json.RawMessage {
		t.Helper()
		v := testCapture()
		v.Strategy = StrategyCapture{ID: "test.strategy/v1", Settings: settings}
		raw, err := encodeExecutionCapture(v)
		testassert.NoError(t, err)
		return raw
	}
	with := func(settings string) map[string]json.RawMessage {
		obj, err := decodePayloadObject(encode(t, nil))
		testassert.NoError(t, err)
		if settings != "" {
			obj["strategy"] = json.RawMessage(`{"id":"test.strategy/v1","settings":` + settings + `,"requires_context_window":false}`)
		}
		return obj
	}

	for _, accepted := range []string{"null", "1e999", `{"nested":[1,{"a":"b"}]}`, "[]"} {
		obj := with(accepted)
		got, err := decodeExecutionCapture(obj)
		testassert.Require(t, err == nil && string(got.Strategy.Settings) == accepted, "settings %s = %s err %v, want the original accepted bytes", accepted, got.Strategy.Settings, err)
		// The accepted bytes re-encode and decode unchanged apart from the
		// ordinary enclosing encoding, and never alias the payload.
		again, err := decodeExecutionCapture(obj)
		testassert.Require(t, err == nil && string(again.Strategy.Settings) == accepted, "second decode settings = %s err %v, want the same bytes", again.Strategy.Settings, err)
		got.Strategy.Settings[0] = 'x'
		third, err := decodeExecutionCapture(obj)
		testassert.Require(t, err == nil && string(third.Strategy.Settings) == accepted, "decoded settings alias the payload bytes: %s", third.Strategy.Settings)
	}

	// Absent settings stays absent on both codec sides.
	{
		got, err := decodeExecutionCapture(with(""))
		testassert.Require(t, err == nil && got.Strategy.Settings == nil, "omitted settings = %s err %v, want absent", got.Strategy.Settings, err)
		v := testCapture()
		v.Strategy = StrategyCapture{ID: "test.strategy/v1"}
		raw, err := encodeExecutionCapture(v)
		testassert.NoError(t, err)
		obj, err := decodePayloadObject(raw)
		testassert.NoError(t, err)
		strategy, present := obj["strategy"]
		testassert.Require(t, present, "the strategy member is required")
		members, err := decodePayloadObject(strategy)
		testassert.NoError(t, err)
		_, has := members["settings"]
		testassert.Require(t, !has, "absent settings leaked the settings member")
	}

	// Malformed inputs fail the same syntax-only acceptance on both sides.
	for _, malformed := range []string{"{", `"unterminated`, "01", "NaN", "undefined"} {
		_, err := decodeExecutionCapture(with(malformed))
		testassert.Require(t, err != nil, "malformed settings %s was accepted", malformed)
		v := testCapture()
		v.Strategy = StrategyCapture{ID: "test.strategy/v1", Settings: json.RawMessage(malformed)}
		_, err = encodeExecutionCapture(v)
		testassert.Require(t, err != nil, "malformed settings %s was accepted on the validating side", malformed)
	}
}

// TestStrategyCaptureSettingsSurviveColdReadAndRecovery proves a settings
// value accepted at admission never corrupts through the durable cold read
// and quiescent recovery.
func TestStrategyCaptureSettingsSurviveColdReadAndRecovery(t *testing.T) {
	prepared := modelPrepared(nil)
	prepared.Capture.Strategy = StrategyCapture{ID: IncludedStrategyID, RequiresContextWindow: true, Settings: json.RawMessage(`1e999`)}
	h, store, _, sessionID := newAdmittedHarness(t, context.Background(), prepared)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, string(rec.Admission.Execution.Strategy.Settings) == "1e999", "warm read settings = %s, want the admitted bytes", rec.Admission.Execution.Strategy.Settings)

	testassert.NoError(t, Recover(context.Background(), store))
	rec, err = h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, string(rec.Admission.Execution.Strategy.Settings) == "1e999", "post-recovery settings = %s, want the admitted bytes", rec.Admission.Execution.Strategy.Settings)
}

// TestOperationStateUnreportedMaintenanceCodec proves the incomplete-maintenance
// marker's durable wire: it round-trips on a running and on a terminal
// Operation state, a false marker encodes as nothing, and a malformed member
// value rejects the decode.
func TestOperationStateUnreportedMaintenanceCodec(t *testing.T) {
	roundTrip := func(t *testing.T, v OperationRecord) OperationCurrentState {
		t.Helper()
		raw, err := encodeOperationRegister(v)
		testassert.NoError(t, err)
		decoded, err := decodeOperationRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: raw})
		testassert.NoError(t, err)
		testassert.Require(t, decoded.State.Status == v.State.Status && decoded.State.UnreportedMaintenance == v.State.UnreportedMaintenance &&
			(decoded.State.Terminal == nil) == (v.State.Terminal == nil) && len(decoded.State.PendingToolCalls) == len(v.State.PendingToolCalls),
			"round-tripped state = %+v, want %+v", decoded.State, v.State)
		if v.State.SettledAt != nil {
			testassert.Require(t, decoded.State.SettledAt != nil && decoded.State.SettledAt.Equal(*v.State.SettledAt), "round-tripped settled_at = %v, want %v", decoded.State.SettledAt, v.State.SettledAt)
		}
		if v.State.Terminal != nil {
			testassert.Require(t, decoded.State.Terminal.Detail == v.State.Terminal.Detail && decoded.State.Terminal.SettlementEntry == v.State.Terminal.SettlementEntry, "round-tripped terminal = %+v, want %+v", decoded.State.Terminal, v.State.Terminal)
		}
		return decoded.State
	}

	running := validOperationRecord()
	running.State.UnreportedMaintenance = true
	raw, err := encodeOperationRegister(running)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(string(raw), `"unreported_maintenance":true`), "running state with the marker %s, want the true member", raw)
	roundTrip(t, running)

	terminal := validOperationRecord()
	settled := testTime
	terminal.State.Status = OperationFailure
	terminal.State.SettledAt = &settled
	terminal.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(5)}, Detail: "summary failure"}
	terminal.State.UnreportedMaintenance = true
	raw, err = encodeOperationRegister(terminal)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(string(raw), `"unreported_maintenance":true`), "terminal state with the marker %s, want the true member", raw)
	roundTrip(t, terminal)

	plain := validOperationRecord()
	raw, err = encodeOperationRegister(plain)
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(string(raw), "unreported_maintenance"), "false marker encoded as a member: %s", raw)
	testassert.Require(t, !roundTrip(t, plain).UnreportedMaintenance, "round-tripped omitted marker = true, want the omitted/false equivalence")

	for _, row := range []struct {
		name  string
		value string
	}{
		{"null marker", "null"},
		{"number marker", "1"},
		{"string marker", `"true"`},
	} {
		t.Run(row.name, func(t *testing.T) {
			members, err := decodePayloadObject(raw)
			testassert.NoError(t, err)
			state := wireObject(t, members["state"])
			state["unreported_maintenance"] = json.RawMessage(row.value)
			members["state"] = mustRawJSON(state)
			tampered, err := json.Marshal(members)
			testassert.NoError(t, err)
			_, err = decodeOperationRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: tampered})
			testassert.Require(t, err != nil, "operation register with %s decoded, want rejection", row.name)
		})
	}
}
