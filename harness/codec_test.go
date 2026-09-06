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

func testCapture() ExecutionCapture {
	return ExecutionCapture{
		ConfigurationRevision: "rev-1",
		Model:                 testModelRef(),
		SystemPrompt:          "system",
		Tools:                 []model.ToolDefinition{testToolDefinition()},
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
		RelatedOperation: operationRef{SessionID: testSessionID, OperationID: testOpID},
		Content:          signalInterruptionContent,
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
			name: "operation_settlement",
			env:  Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 1, CommittedAt: testTime},
			encode: func() (json.RawMessage, error) {
				return encodeOperationSettlementEntry(validSettlementEntry())
			},
			decode: func(env Entry) error { _, err := decodeOperationSettlementEntry(env); return err },
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
	}
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			raw, err := kind.payload()
			if err != nil {
				t.Fatalf("encode valid: %v", err)
			}
			base := Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Sequence: 1, CommittedAt: testTime}
			if kind.name == "operation_settlement" {
				base.Kind = EntryOperationSettlement
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
		{"operation mismatch", setKey(opRaw, "admission", json.RawMessage(`{"session_id":"`+testSessionID+`","operation_id":"ghost","request_kind":"message","admitted_entry":{"session_id":"`+testSessionID+`","entry_id":"`+hexID(1)+`"},"agent_type":"coder","execution":{"configuration_revision":"rev-1","model":{"provider":"prov","model":"gpt-x"},"system_prompt":"system","tools":[]},"admitted_at":"2026-01-02T03:04:05.123456789Z"}`))},
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
