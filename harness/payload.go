package harness

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/MMinasyan/lightcode/model"
)

// This file owns the exact durable JSON shapes of every entry and register
// payload. Each codec validates the value before encoding and after decoding,
// enforces payload/envelope identity agreement at its decode entry point, and
// returns independent owned values on both sides.

// optionalNonEmptyString reads one string member that encodes by omission:
// when present it must be a non-empty string, and absence yields the zero
// value.
func optionalNonEmptyString(obj map[string]json.RawMessage, key string) (string, error) {
	if _, present := obj[key]; !present {
		return "", nil
	}
	s, err := stringMember(obj, key, true)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", fmt.Errorf("member %q must be non-empty when present", key)
	}
	return s, nil
}

// encodeEntryRef renders one entry reference with exact keys and durable
// identity shapes.
func encodeEntryRef(ref EntryRef) (json.RawMessage, error) {
	if err := validateHexID(ref.SessionID, "entry reference session id"); err != nil {
		return nil, invalidInput("%v", err)
	}
	if err := validateHexID(ref.EntryID, "entry reference entry id"); err != nil {
		return nil, invalidInput("%v", err)
	}
	return json.Marshal(struct {
		SessionID string `json:"session_id"`
		EntryID   string `json:"entry_id"`
	}{SessionID: ref.SessionID, EntryID: ref.EntryID})
}

// decodeEntryRef reads one entry reference with exact keys.
func decodeEntryRef(obj map[string]json.RawMessage) (EntryRef, error) {
	if err := rejectUnknownMembers(obj, "session_id", "entry_id"); err != nil {
		return EntryRef{}, err
	}
	sessionID, err := stringMember(obj, "session_id", true)
	if err != nil {
		return EntryRef{}, err
	}
	entryID, err := stringMember(obj, "entry_id", true)
	if err != nil {
		return EntryRef{}, err
	}
	ref := EntryRef{SessionID: sessionID, EntryID: entryID}
	if err := validateHexID(ref.SessionID, "entry reference session id"); err != nil {
		return EntryRef{}, err
	}
	if err := validateHexID(ref.EntryID, "entry reference entry id"); err != nil {
		return EntryRef{}, err
	}
	return ref, nil
}

// encodeOperationRef renders one operation reference with exact keys and
// durable identity shapes.
func encodeOperationRef(ref operationRef) (json.RawMessage, error) {
	if err := validateHexID(ref.SessionID, "operation reference session id"); err != nil {
		return nil, invalidInput("%v", err)
	}
	if err := validateOperationIdentity(ref.OperationID, "operation reference operation id"); err != nil {
		return nil, invalidInput("%v", err)
	}
	return json.Marshal(ref)
}

// decodeOperationRef reads one operation reference with exact keys.
func decodeOperationRef(obj map[string]json.RawMessage) (operationRef, error) {
	if err := rejectUnknownMembers(obj, "session_id", "operation_id"); err != nil {
		return operationRef{}, err
	}
	sessionID, err := stringMember(obj, "session_id", true)
	if err != nil {
		return operationRef{}, err
	}
	operationID, err := stringMember(obj, "operation_id", true)
	if err != nil {
		return operationRef{}, err
	}
	ref := operationRef{SessionID: sessionID, OperationID: operationID}
	if err := validateHexID(ref.SessionID, "operation reference session id"); err != nil {
		return operationRef{}, err
	}
	if err := validateOperationIdentity(ref.OperationID, "operation reference operation id"); err != nil {
		return operationRef{}, err
	}
	return ref, nil
}

// decodeEntryEnvelope validates the envelope identity fields every entry
// payload repeats.
func decodeEntryEnvelope(env Entry) error {
	if err := validateHexID(env.SessionID, "envelope session id"); err != nil {
		return err
	}
	if err := validateHexID(env.ID, "envelope entry id"); err != nil {
		return err
	}
	if env.OperationID != "" {
		return validateOperationIdentity(env.OperationID, "envelope operation id")
	}
	return nil
}

// signalContent returns the contract-fixed model-visible content of one
// signal kind.
func signalContent(kind SignalKind) string {
	switch kind {
	case SignalInterruption:
		return signalInterruptionContent
	case SignalModelFailureContinuation:
		return signalModelFailureContinuationContent
	default:
		return ""
	}
}

// encodeInputEntry renders one input entry payload.
func encodeInputEntry(v inputEntry) (json.RawMessage, error) {
	if err := validateInputEntry(v); err != nil {
		return nil, invalidInput("input entry: %v", err)
	}
	content := make([]json.RawMessage, 0, len(v.Content))
	for _, part := range v.Content {
		raw, err := encodeContentPart(part)
		if err != nil {
			return nil, err
		}
		content = append(content, raw)
	}
	wire, err := json.Marshal(struct {
		SessionID   string            `json:"session_id"`
		EntryID     string            `json:"entry_id"`
		OperationID string            `json:"operation_id,omitempty"`
		Origin      string            `json:"origin"`
		Content     []json.RawMessage `json:"content"`
	}{
		SessionID:   v.SessionID,
		EntryID:     v.EntryID,
		OperationID: v.OperationID,
		Origin:      string(v.Origin),
		Content:     content,
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// decodeInputEntry reads one input entry payload and enforces its agreement
// with the addressed envelope identity.
func decodeInputEntry(env Entry) (inputEntry, error) {
	if err := decodeEntryEnvelope(env); err != nil {
		return inputEntry{}, err
	}
	obj, err := decodePayloadObject(env.Payload)
	if err != nil {
		return inputEntry{}, err
	}
	if err := rejectUnknownMembers(obj, "session_id", "entry_id", "operation_id", "origin", "content"); err != nil {
		return inputEntry{}, err
	}
	sessionID, err := stringMember(obj, "session_id", true)
	if err != nil {
		return inputEntry{}, err
	}
	entryID, err := stringMember(obj, "entry_id", true)
	if err != nil {
		return inputEntry{}, err
	}
	operationID, err := optionalNonEmptyString(obj, "operation_id")
	if err != nil {
		return inputEntry{}, err
	}
	origin, err := stringMember(obj, "origin", true)
	if err != nil {
		return inputEntry{}, err
	}
	contentRaw, err := arrayMember(obj, "content", true)
	if err != nil {
		return inputEntry{}, err
	}
	if sessionID != env.SessionID {
		return inputEntry{}, fmt.Errorf("payload session id %q does not agree with the envelope", sessionID)
	}
	if entryID != env.ID {
		return inputEntry{}, fmt.Errorf("payload entry id %q does not agree with the envelope", entryID)
	}
	if operationID != env.OperationID {
		return inputEntry{}, fmt.Errorf("payload operation id %q does not agree with the envelope", operationID)
	}
	v := inputEntry{SessionID: sessionID, EntryID: entryID, OperationID: operationID, Origin: InputOrigin(origin)}
	for i, raw := range contentRaw {
		part, err := decodeContentPart(raw)
		if err != nil {
			return inputEntry{}, fmt.Errorf("content[%d]: %w", i, err)
		}
		v.Content = append(v.Content, part)
	}
	if err := validateInputEntry(v); err != nil {
		return inputEntry{}, err
	}
	return v, nil
}

// validateInputEntry enforces the closed input-entry shape.
func validateInputEntry(v inputEntry) error {
	if err := validateHexID(v.SessionID, "session id"); err != nil {
		return err
	}
	if err := validateHexID(v.EntryID, "entry id"); err != nil {
		return err
	}
	if v.OperationID != "" {
		if err := validateOperationIdentity(v.OperationID, "operation id"); err != nil {
			return err
		}
	}
	switch v.Origin {
	case InputOriginUser, InputOriginRuntime, InputOriginPlugin:
	default:
		return fmt.Errorf("input origin %q is not one of user, runtime or plugin", v.Origin)
	}
	for i, part := range v.Content {
		if _, err := model.NewContentPart(part); err != nil {
			return fmt.Errorf("content[%d]: %w", i, err)
		}
	}
	return nil
}

// encodeToolCallRecord renders one assistant tool-call record. Raw arguments
// persist as the base64 standard-alphabet string so malformed JSON and
// arbitrary bytes round-trip exactly.
func encodeToolCallRecord(c toolCallRecord) (json.RawMessage, error) {
	if err := validateToolCallRecord(c); err != nil {
		return nil, invalidInput("tool call: %v", err)
	}
	obj := map[string]json.RawMessage{}
	var err error
	if obj["id"], err = json.Marshal(c.ID); err != nil {
		return nil, err
	}
	if obj["ordinal"], err = json.Marshal(c.Ordinal); err != nil {
		return nil, err
	}
	if obj["name"], err = json.Marshal(c.Name); err != nil {
		return nil, err
	}
	if obj["arguments_base64"], err = json.Marshal(c.ArgumentsBase64); err != nil {
		return nil, err
	}
	if len(c.Extra) > 0 {
		if obj["extra"], err = marshalExtra(c.Extra); err != nil {
			return nil, err
		}
	}
	if len(c.NormalizedArguments) > 0 {
		obj["normalized_arguments"] = model.CloneRaw(c.NormalizedArguments)
	}
	if obj["result_entry_id"], err = json.Marshal(c.ResultEntryID); err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}

// decodeToolCallRecord reads one assistant tool-call record with exact keys.
func decodeToolCallRecord(raw json.RawMessage) (toolCallRecord, error) {
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return toolCallRecord{}, fmt.Errorf("tool call: %w", err)
	}
	if err := rejectUnknownMembers(obj, "id", "ordinal", "name", "arguments_base64", "extra", "normalized_arguments", "result_entry_id"); err != nil {
		return toolCallRecord{}, err
	}
	var c toolCallRecord
	if c.ID, err = stringMember(obj, "id", true); err != nil {
		return toolCallRecord{}, err
	}
	if c.Ordinal, err = int64Member(obj, "ordinal", true); err != nil {
		return toolCallRecord{}, err
	}
	if c.Name, err = stringMember(obj, "name", true); err != nil {
		return toolCallRecord{}, err
	}
	if c.ArgumentsBase64, err = stringMember(obj, "arguments_base64", true); err != nil {
		return toolCallRecord{}, err
	}
	if c.Extra, err = extraMember(obj, "extra", false); err != nil {
		return toolCallRecord{}, err
	}
	if c.NormalizedArguments, err = rawJSONMember(obj, "normalized_arguments", false); err != nil {
		return toolCallRecord{}, err
	}
	if c.ResultEntryID, err = stringMember(obj, "result_entry_id", true); err != nil {
		return toolCallRecord{}, err
	}
	return c, nil
}

// validateToolCallRecord enforces the closed tool-call shape: non-empty
// opaque call identity and name, strictly base64-encoded raw arguments, and a
// durable reserved result identity. Ordinal agreement with the array position
// is enforced by the owning assistant entry.
func validateToolCallRecord(c toolCallRecord) error {
	if err := validateOperationIdentity(c.ID, "tool call id"); err != nil {
		return err
	}
	if err := validateOperationIdentity(c.Name, "tool call name"); err != nil {
		return err
	}
	if _, err := base64.StdEncoding.Strict().DecodeString(c.ArgumentsBase64); err != nil {
		return fmt.Errorf("tool call arguments_base64 is not canonical base64: %w", err)
	}
	if !validExtraValues(c.Extra) {
		return errors.New("tool call extra values must be complete valid JSON")
	}
	if len(c.NormalizedArguments) > 0 {
		if !json.Valid(c.NormalizedArguments) {
			return errors.New("tool call normalized_arguments must be valid JSON")
		}
		if trimmed := bytes.TrimSpace(c.NormalizedArguments); bytes.Equal(trimmed, []byte("null")) {
			return errors.New("tool call normalized_arguments must not be null")
		}
	}
	return validateHexID(c.ResultEntryID, "tool call reserved result entry id")
}

// encodeAssistantEntry renders one assistant entry payload.
func encodeAssistantEntry(v assistantEntry) (json.RawMessage, error) {
	if err := validateAssistantEntry(v); err != nil {
		return nil, invalidInput("assistant entry: %v", err)
	}
	source, err := encodeModelRef(v.Source)
	if err != nil {
		return nil, err
	}
	content := make([]json.RawMessage, 0, len(v.Content))
	for _, part := range v.Content {
		raw, err := encodeContentPart(part)
		if err != nil {
			return nil, err
		}
		content = append(content, raw)
	}
	calls := make([]json.RawMessage, 0, len(v.ToolCalls))
	for _, call := range v.ToolCalls {
		raw, err := encodeToolCallRecord(call)
		if err != nil {
			return nil, err
		}
		calls = append(calls, raw)
	}
	var extra json.RawMessage
	if len(v.Extra) > 0 {
		if extra, err = marshalExtra(v.Extra); err != nil {
			return nil, err
		}
	}
	wire, err := json.Marshal(struct {
		SessionID   string            `json:"session_id"`
		EntryID     string            `json:"entry_id"`
		OperationID string            `json:"operation_id,omitempty"`
		Status      string            `json:"status"`
		Source      json.RawMessage   `json:"source"`
		Content     []json.RawMessage `json:"content"`
		Refusal     string            `json:"refusal,omitempty"`
		Extra       json.RawMessage   `json:"extra,omitempty"`
		ToolCalls   []json.RawMessage `json:"tool_calls"`
		Usage       *UsageCount       `json:"usage,omitempty"`
	}{
		SessionID:   v.SessionID,
		EntryID:     v.EntryID,
		OperationID: v.OperationID,
		Status:      string(v.Status),
		Source:      source,
		Content:     content,
		Refusal:     v.Refusal,
		Extra:       extra,
		ToolCalls:   calls,
		Usage:       v.Usage,
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// decodeAssistantEntry reads one assistant entry payload and enforces its
// agreement with the addressed envelope identity. An independently copied
// fork-prefix assistant carries no source usage.
func decodeAssistantEntry(env Entry) (assistantEntry, error) {
	if err := decodeEntryEnvelope(env); err != nil {
		return assistantEntry{}, err
	}
	obj, err := decodePayloadObject(env.Payload)
	if err != nil {
		return assistantEntry{}, err
	}
	if err := rejectUnknownMembers(obj, "session_id", "entry_id", "operation_id", "status", "source", "content", "refusal", "extra", "tool_calls", "usage"); err != nil {
		return assistantEntry{}, err
	}
	sessionID, err := stringMember(obj, "session_id", true)
	if err != nil {
		return assistantEntry{}, err
	}
	entryID, err := stringMember(obj, "entry_id", true)
	if err != nil {
		return assistantEntry{}, err
	}
	operationID, err := optionalNonEmptyString(obj, "operation_id")
	if err != nil {
		return assistantEntry{}, err
	}
	status, err := stringMember(obj, "status", true)
	if err != nil {
		return assistantEntry{}, err
	}
	sourceObj, err := objectMember(obj, "source", true)
	if err != nil {
		return assistantEntry{}, err
	}
	source, err := decodeModelRef(sourceObj)
	if err != nil {
		return assistantEntry{}, fmt.Errorf("member %q: %w", "source", err)
	}
	contentRaw, err := arrayMember(obj, "content", true)
	if err != nil {
		return assistantEntry{}, err
	}
	refusal, err := optionalNonEmptyString(obj, "refusal")
	if err != nil {
		return assistantEntry{}, err
	}
	extra, err := extraMember(obj, "extra", false)
	if err != nil {
		return assistantEntry{}, err
	}
	callsRaw, err := arrayMember(obj, "tool_calls", true)
	if err != nil {
		return assistantEntry{}, err
	}
	var usage *UsageCount
	if _, present := obj["usage"]; present {
		usageObj, err := objectMember(obj, "usage", true)
		if err != nil {
			return assistantEntry{}, err
		}
		counts, err := decodeUsageCount(usageObj)
		if err != nil {
			return assistantEntry{}, fmt.Errorf("member %q: %w", "usage", err)
		}
		usage = &counts
	}
	if sessionID != env.SessionID {
		return assistantEntry{}, fmt.Errorf("payload session id %q does not agree with the envelope", sessionID)
	}
	if entryID != env.ID {
		return assistantEntry{}, fmt.Errorf("payload entry id %q does not agree with the envelope", entryID)
	}
	if operationID != env.OperationID {
		return assistantEntry{}, fmt.Errorf("payload operation id %q does not agree with the envelope", operationID)
	}
	v := assistantEntry{
		SessionID:   sessionID,
		EntryID:     entryID,
		OperationID: operationID,
		Status:      model.OutputStatus(status),
		Source:      source,
		Refusal:     refusal,
		Extra:       extra,
		Usage:       usage,
	}
	for i, raw := range contentRaw {
		part, err := decodeContentPart(raw)
		if err != nil {
			return assistantEntry{}, fmt.Errorf("content[%d]: %w", i, err)
		}
		v.Content = append(v.Content, part)
	}
	for i, raw := range callsRaw {
		call, err := decodeToolCallRecord(raw)
		if err != nil {
			return assistantEntry{}, fmt.Errorf("tool_calls[%d]: %w", i, err)
		}
		v.ToolCalls = append(v.ToolCalls, call)
	}
	if env.OperationID == "" && v.Usage != nil {
		return assistantEntry{}, errors.New("independently copied fork-prefix assistant entry carries no source usage")
	}
	if err := validateAssistantEntry(v); err != nil {
		return assistantEntry{}, err
	}
	return v, nil
}

// validateAssistantEntry enforces the closed assistant-entry shape: closed
// output status, complete source identity, unique tool calls with zero-based
// ordinals and unique reserved results, no calls on errored or interrupted
// output, and an eligible model-visible payload.
func validateAssistantEntry(v assistantEntry) error {
	if err := validateHexID(v.SessionID, "session id"); err != nil {
		return err
	}
	if err := validateHexID(v.EntryID, "entry id"); err != nil {
		return err
	}
	if v.OperationID != "" {
		if err := validateOperationIdentity(v.OperationID, "operation id"); err != nil {
			return err
		}
	} else if v.Usage != nil {
		return errors.New("independently copied fork-prefix assistant entry carries no source usage")
	}
	if !validOutputStatus(v.Status) {
		return fmt.Errorf("assistant status %q is not one of completed, errored or interrupted", v.Status)
	}
	if v.Source.Provider == "" || v.Source.Model == "" {
		return fmt.Errorf("assistant source %q must be a complete model identity", v.Source.String())
	}
	for i, part := range v.Content {
		if _, err := model.NewContentPart(part); err != nil {
			return fmt.Errorf("content[%d]: %w", i, err)
		}
	}
	seenCalls := make(map[string]bool, len(v.ToolCalls))
	seenResults := make(map[string]bool, len(v.ToolCalls))
	for i, call := range v.ToolCalls {
		if call.Ordinal != int64(i) {
			return fmt.Errorf("tool_calls[%d]: ordinal %d must equal the zero-based array position", i, call.Ordinal)
		}
		if err := validateToolCallRecord(call); err != nil {
			return fmt.Errorf("tool_calls[%d]: %w", i, err)
		}
		if seenCalls[call.ID] {
			return fmt.Errorf("tool_calls[%d]: call id %q is not unique", i, call.ID)
		}
		seenCalls[call.ID] = true
		if seenResults[call.ResultEntryID] {
			return fmt.Errorf("tool_calls[%d]: reserved result %q is not unique", i, call.ResultEntryID)
		}
		seenResults[call.ResultEntryID] = true
	}
	if v.Status != model.OutputCompleted && len(v.ToolCalls) > 0 {
		return fmt.Errorf("%s assistant entry carries no tool calls", v.Status)
	}
	if !assistantPayloadEligible(v) {
		return errors.New("assistant entry carries no eligible model-visible payload (finalized content part, refusal, finalized extra, or, for completed output only, tool calls)")
	}
	return nil
}

// assistantPayloadEligible reports whether an assistant entry carries a
// model-visible payload under the finalization view: a non-empty finalized
// content part, a non-empty refusal, at least one finalized non-null extra
// value, or, for completed output only, at least one tool call.
func assistantPayloadEligible(v assistantEntry) bool {
	if v.Refusal != "" {
		return true
	}
	for _, part := range v.Content {
		if part.Text != "" || part.URL != "" || part.OpaqueWireType != "" || len(part.Extra.Finalize()) > 0 {
			return true
		}
	}
	if len(v.Extra.Finalize()) > 0 {
		return true
	}
	return v.Status == model.OutputCompleted && len(v.ToolCalls) > 0
}

// encodeToolResultEntry renders one tool-result entry payload.
func encodeToolResultEntry(v toolResultEntry) (json.RawMessage, error) {
	if err := validateToolResultEntry(v); err != nil {
		return nil, invalidInput("tool result entry: %v", err)
	}
	assistantEntry, err := encodeEntryRef(v.AssistantEntry)
	if err != nil {
		return nil, err
	}
	wire, err := json.Marshal(struct {
		SessionID      string          `json:"session_id"`
		EntryID        string          `json:"entry_id"`
		OperationID    string          `json:"operation_id,omitempty"`
		AssistantEntry json.RawMessage `json:"assistant_entry"`
		ToolCallID     string          `json:"tool_call_id"`
		Status         string          `json:"status"`
		Content        string          `json:"content"`
	}{
		SessionID:      v.SessionID,
		EntryID:        v.EntryID,
		OperationID:    v.OperationID,
		AssistantEntry: assistantEntry,
		ToolCallID:     v.ToolCallID,
		Status:         string(v.Status),
		Content:        v.Content,
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// decodeToolResultEntry reads one tool-result entry payload and enforces its
// agreement with the addressed envelope identity.
func decodeToolResultEntry(env Entry) (toolResultEntry, error) {
	if err := decodeEntryEnvelope(env); err != nil {
		return toolResultEntry{}, err
	}
	obj, err := decodePayloadObject(env.Payload)
	if err != nil {
		return toolResultEntry{}, err
	}
	if err := rejectUnknownMembers(obj, "session_id", "entry_id", "operation_id", "assistant_entry", "tool_call_id", "status", "content"); err != nil {
		return toolResultEntry{}, err
	}
	sessionID, err := stringMember(obj, "session_id", true)
	if err != nil {
		return toolResultEntry{}, err
	}
	entryID, err := stringMember(obj, "entry_id", true)
	if err != nil {
		return toolResultEntry{}, err
	}
	operationID, err := optionalNonEmptyString(obj, "operation_id")
	if err != nil {
		return toolResultEntry{}, err
	}
	assistantObj, err := objectMember(obj, "assistant_entry", true)
	if err != nil {
		return toolResultEntry{}, err
	}
	assistantEntry, err := decodeEntryRef(assistantObj)
	if err != nil {
		return toolResultEntry{}, fmt.Errorf("member %q: %w", "assistant_entry", err)
	}
	toolCallID, err := stringMember(obj, "tool_call_id", true)
	if err != nil {
		return toolResultEntry{}, err
	}
	status, err := stringMember(obj, "status", true)
	if err != nil {
		return toolResultEntry{}, err
	}
	content, err := stringMember(obj, "content", true)
	if err != nil {
		return toolResultEntry{}, err
	}
	if sessionID != env.SessionID {
		return toolResultEntry{}, fmt.Errorf("payload session id %q does not agree with the envelope", sessionID)
	}
	if entryID != env.ID {
		return toolResultEntry{}, fmt.Errorf("payload entry id %q does not agree with the envelope", entryID)
	}
	if operationID != env.OperationID {
		return toolResultEntry{}, fmt.Errorf("payload operation id %q does not agree with the envelope", operationID)
	}
	v := toolResultEntry{
		SessionID:      sessionID,
		EntryID:        entryID,
		OperationID:    operationID,
		AssistantEntry: assistantEntry,
		ToolCallID:     toolCallID,
		Status:         model.ToolResultStatus(status),
		Content:        content,
	}
	if err := validateToolResultEntry(v); err != nil {
		return toolResultEntry{}, err
	}
	return v, nil
}

// validateToolResultEntry enforces the closed tool-result shape: durable
// identities, non-empty original call id, and the landed Agent result rules
// re-enforced through the landed constructor.
func validateToolResultEntry(v toolResultEntry) error {
	if err := validateHexID(v.SessionID, "session id"); err != nil {
		return err
	}
	if err := validateHexID(v.EntryID, "entry id"); err != nil {
		return err
	}
	if v.OperationID != "" {
		if err := validateOperationIdentity(v.OperationID, "operation id"); err != nil {
			return err
		}
	}
	if _, err := model.NewToolResult(model.ToolResult{CallID: v.ToolCallID, Status: v.Status, Content: v.Content}); err != nil {
		return err
	}
	return nil
}

// encodeSignalEntry renders one signal entry payload.
func encodeSignalEntry(v signalEntry) (json.RawMessage, error) {
	if err := validateSignalEntry(v); err != nil {
		return nil, invalidInput("signal entry: %v", err)
	}
	related, err := encodeOperationRef(v.RelatedOperation)
	if err != nil {
		return nil, err
	}
	wire, err := json.Marshal(struct {
		SessionID        string          `json:"session_id"`
		EntryID          string          `json:"entry_id"`
		OperationID      string          `json:"operation_id,omitempty"`
		Signal           string          `json:"signal"`
		RelatedOperation json.RawMessage `json:"related_operation"`
		Content          string          `json:"content"`
	}{
		SessionID:        v.SessionID,
		EntryID:          v.EntryID,
		OperationID:      v.OperationID,
		Signal:           string(v.Signal),
		RelatedOperation: related,
		Content:          v.Content,
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// decodeSignalEntry reads one signal entry payload and enforces its agreement
// with the addressed envelope identity.
func decodeSignalEntry(env Entry) (signalEntry, error) {
	if err := decodeEntryEnvelope(env); err != nil {
		return signalEntry{}, err
	}
	obj, err := decodePayloadObject(env.Payload)
	if err != nil {
		return signalEntry{}, err
	}
	if err := rejectUnknownMembers(obj, "session_id", "entry_id", "operation_id", "signal", "related_operation", "content"); err != nil {
		return signalEntry{}, err
	}
	sessionID, err := stringMember(obj, "session_id", true)
	if err != nil {
		return signalEntry{}, err
	}
	entryID, err := stringMember(obj, "entry_id", true)
	if err != nil {
		return signalEntry{}, err
	}
	operationID, err := optionalNonEmptyString(obj, "operation_id")
	if err != nil {
		return signalEntry{}, err
	}
	signal, err := stringMember(obj, "signal", true)
	if err != nil {
		return signalEntry{}, err
	}
	relatedObj, err := objectMember(obj, "related_operation", true)
	if err != nil {
		return signalEntry{}, err
	}
	related, err := decodeOperationRef(relatedObj)
	if err != nil {
		return signalEntry{}, fmt.Errorf("member %q: %w", "related_operation", err)
	}
	content, err := stringMember(obj, "content", true)
	if err != nil {
		return signalEntry{}, err
	}
	if sessionID != env.SessionID {
		return signalEntry{}, fmt.Errorf("payload session id %q does not agree with the envelope", sessionID)
	}
	if entryID != env.ID {
		return signalEntry{}, fmt.Errorf("payload entry id %q does not agree with the envelope", entryID)
	}
	if operationID != env.OperationID {
		return signalEntry{}, fmt.Errorf("payload operation id %q does not agree with the envelope", operationID)
	}
	v := signalEntry{
		SessionID:        sessionID,
		EntryID:          entryID,
		OperationID:      operationID,
		Signal:           SignalKind(signal),
		RelatedOperation: related,
		Content:          content,
	}
	if err := validateSignalEntry(v); err != nil {
		return signalEntry{}, err
	}
	return v, nil
}

// validateSignalEntry enforces the closed signal shape: closed kind and the
// contract-fixed content for that kind.
func validateSignalEntry(v signalEntry) error {
	if err := validateHexID(v.SessionID, "session id"); err != nil {
		return err
	}
	if err := validateHexID(v.EntryID, "entry id"); err != nil {
		return err
	}
	if v.OperationID != "" {
		if err := validateOperationIdentity(v.OperationID, "operation id"); err != nil {
			return err
		}
	}
	switch v.Signal {
	case SignalInterruption, SignalModelFailureContinuation:
	default:
		return fmt.Errorf("signal kind %q is not one of interruption or model_failure_continuation", v.Signal)
	}
	if want := signalContent(v.Signal); v.Content != want {
		return fmt.Errorf("signal content %q is not the fixed content of kind %s", v.Content, v.Signal)
	}
	if err := validateHexID(v.RelatedOperation.SessionID, "related operation session id"); err != nil {
		return err
	}
	return validateOperationIdentity(v.RelatedOperation.OperationID, "related operation id")
}

// encodeOperationSettlementEntry renders one operation-settlement entry
// payload.
func encodeOperationSettlementEntry(v operationSettlementEntry) (json.RawMessage, error) {
	if err := validateOperationSettlementEntry(v); err != nil {
		return nil, invalidInput("operation settlement entry: %v", err)
	}
	var modelRaw json.RawMessage
	if v.Model != nil {
		raw, err := encodeModelRef(*v.Model)
		if err != nil {
			return nil, err
		}
		modelRaw = raw
	}
	wire, err := json.Marshal(struct {
		SessionID   string          `json:"session_id"`
		EntryID     string          `json:"entry_id"`
		OperationID string          `json:"operation_id"`
		Status      string          `json:"status"`
		Detail      string          `json:"detail,omitempty"`
		Model       json.RawMessage `json:"model,omitempty"`
		Usage       *UsageCount     `json:"usage,omitempty"`
	}{
		SessionID:   v.SessionID,
		EntryID:     v.EntryID,
		OperationID: v.OperationID,
		Status:      string(v.Status),
		Detail:      v.Detail,
		Model:       modelRaw,
		Usage:       v.Usage,
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// decodeOperationSettlementEntry reads one operation-settlement entry payload
// and enforces its agreement with the addressed envelope identity. The kind
// is never operationless.
func decodeOperationSettlementEntry(env Entry) (operationSettlementEntry, error) {
	if err := decodeEntryEnvelope(env); err != nil {
		return operationSettlementEntry{}, err
	}
	obj, err := decodePayloadObject(env.Payload)
	if err != nil {
		return operationSettlementEntry{}, err
	}
	if err := rejectUnknownMembers(obj, "session_id", "entry_id", "operation_id", "status", "detail", "model", "usage"); err != nil {
		return operationSettlementEntry{}, err
	}
	sessionID, err := stringMember(obj, "session_id", true)
	if err != nil {
		return operationSettlementEntry{}, err
	}
	entryID, err := stringMember(obj, "entry_id", true)
	if err != nil {
		return operationSettlementEntry{}, err
	}
	operationID, err := stringMember(obj, "operation_id", true)
	if err != nil {
		return operationSettlementEntry{}, err
	}
	status, err := stringMember(obj, "status", true)
	if err != nil {
		return operationSettlementEntry{}, err
	}
	detail, err := optionalNonEmptyString(obj, "detail")
	if err != nil {
		return operationSettlementEntry{}, err
	}
	var ref *model.ModelRef
	if _, present := obj["model"]; present {
		modelObj, err := objectMember(obj, "model", true)
		if err != nil {
			return operationSettlementEntry{}, err
		}
		decoded, err := decodeModelRef(modelObj)
		if err != nil {
			return operationSettlementEntry{}, fmt.Errorf("member %q: %w", "model", err)
		}
		ref = &decoded
	}
	var usage *UsageCount
	if _, present := obj["usage"]; present {
		usageObj, err := objectMember(obj, "usage", true)
		if err != nil {
			return operationSettlementEntry{}, err
		}
		counts, err := decodeUsageCount(usageObj)
		if err != nil {
			return operationSettlementEntry{}, fmt.Errorf("member %q: %w", "usage", err)
		}
		usage = &counts
	}
	if sessionID != env.SessionID {
		return operationSettlementEntry{}, fmt.Errorf("payload session id %q does not agree with the envelope", sessionID)
	}
	if entryID != env.ID {
		return operationSettlementEntry{}, fmt.Errorf("payload entry id %q does not agree with the envelope", entryID)
	}
	if operationID != env.OperationID {
		return operationSettlementEntry{}, fmt.Errorf("payload operation id %q does not agree with the envelope", operationID)
	}
	v := operationSettlementEntry{
		SessionID:   sessionID,
		EntryID:     entryID,
		OperationID: operationID,
		Status:      OperationState(status),
		Detail:      detail,
		Model:       ref,
		Usage:       usage,
	}
	if err := validateOperationSettlementEntry(v); err != nil {
		return operationSettlementEntry{}, err
	}
	return v, nil
}

// validateOperationSettlementEntry enforces the closed settlement shape: a
// non-empty owning Operation identity, terminal status with the contract
// detail rule (success empty, failure and interruption non-empty), and the
// model identity present exactly when the settlement carries usage.
func validateOperationSettlementEntry(v operationSettlementEntry) error {
	if err := validateHexID(v.SessionID, "session id"); err != nil {
		return err
	}
	if err := validateHexID(v.EntryID, "entry id"); err != nil {
		return err
	}
	if err := validateOperationIdentity(v.OperationID, "operation id"); err != nil {
		return err
	}
	switch v.Status {
	case OperationSuccess:
		if v.Detail != "" {
			return errors.New("success settlement requires empty detail")
		}
	case OperationFailure, OperationInterruption:
		if v.Detail == "" {
			return fmt.Errorf("%s settlement requires non-empty detail", v.Status)
		}
	default:
		return fmt.Errorf("settlement status %q is not terminal", v.Status)
	}
	if (v.Model == nil) != (v.Usage == nil) {
		return errors.New("settlement model is present exactly when the settlement carries usage")
	}
	return nil
}

// encodeUnsupportedEntry rejects persistence of the landed envelope kinds
// that have no valid Phase 3 payload or producer: rejecting before persistence
// uses the invalid-input class, while a stored record of these kinds surfaces
// as corruption through the graph validator.
func encodeUnsupportedEntry(kind EntryKind) (json.RawMessage, error) {
	return nil, invalidInput("entry kind %q has no durable payload in this phase", kind)
}

// encodeUsageCountWire renders one usage count with all three signed counts
// present.
func encodeUsageCountWire(u UsageCount) (json.RawMessage, error) {
	return json.Marshal(u)
}

// decodeUsageCount reads one usage count with exact keys and all three signed
// counts required.
func decodeUsageCount(obj map[string]json.RawMessage) (UsageCount, error) {
	if err := rejectUnknownMembers(obj, "input_tokens", "cached_input_tokens", "output_tokens"); err != nil {
		return UsageCount{}, err
	}
	input, err := int64Member(obj, "input_tokens", true)
	if err != nil {
		return UsageCount{}, err
	}
	cached, err := int64Member(obj, "cached_input_tokens", true)
	if err != nil {
		return UsageCount{}, err
	}
	output, err := int64Member(obj, "output_tokens", true)
	if err != nil {
		return UsageCount{}, err
	}
	return UsageCount{InputTokens: input, CachedInputTokens: cached, OutputTokens: output}, nil
}

// encodeUsageTotalsWire renders one canonical totals value: a non-null array
// of model/usage pairs.
func encodeUsageTotalsWire(t UsageTotals) (json.RawMessage, error) {
	if err := validateUsageTotals(t); err != nil {
		return nil, invalidInput("usage totals: %v", err)
	}
	items := make([]json.RawMessage, 0, len(t.ByModel))
	for _, mu := range t.ByModel {
		modelRaw, err := encodeModelRef(mu.Model)
		if err != nil {
			return nil, err
		}
		usageRaw, err := encodeUsageCountWire(mu.Usage)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(struct {
			Model json.RawMessage `json:"model"`
			Usage json.RawMessage `json:"usage"`
		}{Model: modelRaw, Usage: usageRaw})
		if err != nil {
			return nil, err
		}
		items = append(items, raw)
	}
	return json.Marshal(struct {
		ByModel []json.RawMessage `json:"by_model"`
	}{ByModel: items})
}

// decodeUsageTotals reads one totals value with exact keys and re-enforces
// uniqueness and lexicographic sort by provider, then model.
func decodeUsageTotals(obj map[string]json.RawMessage) (UsageTotals, error) {
	if err := rejectUnknownMembers(obj, "by_model"); err != nil {
		return UsageTotals{}, err
	}
	items, err := arrayMember(obj, "by_model", true)
	if err != nil {
		return UsageTotals{}, err
	}
	var t UsageTotals
	for i, raw := range items {
		entryObj, err := decodePayloadObject(raw)
		if err != nil {
			return UsageTotals{}, fmt.Errorf("by_model[%d]: %w", i, err)
		}
		if err := rejectUnknownMembers(entryObj, "model", "usage"); err != nil {
			return UsageTotals{}, fmt.Errorf("by_model[%d]: %w", i, err)
		}
		modelObj, err := objectMember(entryObj, "model", true)
		if err != nil {
			return UsageTotals{}, fmt.Errorf("by_model[%d]: %w", i, err)
		}
		ref, err := decodeModelRef(modelObj)
		if err != nil {
			return UsageTotals{}, fmt.Errorf("by_model[%d]: %w", i, err)
		}
		usageObj, err := objectMember(entryObj, "usage", true)
		if err != nil {
			return UsageTotals{}, fmt.Errorf("by_model[%d]: %w", i, err)
		}
		usage, err := decodeUsageCount(usageObj)
		if err != nil {
			return UsageTotals{}, fmt.Errorf("by_model[%d].usage: %w", i, err)
		}
		t.ByModel = append(t.ByModel, ModelUsage{Model: ref, Usage: usage})
	}
	if err := validateUsageTotals(t); err != nil {
		return UsageTotals{}, err
	}
	return t, nil
}

// validateWorkspace accepts a Workspace only when filepath.Abs resolves it to
// exactly the supplied string: the caller-normalized absolute clean value is
// stored unchanged, and Harness does not normalize it.
func validateWorkspace(w string) error {
	resolved, err := filepath.Abs(w)
	if err != nil || resolved != w {
		return fmt.Errorf("workspace %q is not a caller-normalized absolute path", w)
	}
	return nil
}

// encodeSessionRegister renders one Session register payload, exactly
// {"identity":...,"state":...}. The record's revision is envelope metadata
// and never enters the payload.
func encodeSessionRegister(rec SessionRecord) (json.RawMessage, error) {
	identity, err := encodeSessionIdentity(rec.Identity)
	if err != nil {
		return nil, err
	}
	state, err := encodeSessionState(rec.State)
	if err != nil {
		return nil, err
	}
	wire, err := json.Marshal(struct {
		Identity json.RawMessage `json:"identity"`
		State    json.RawMessage `json:"state"`
	}{Identity: identity, State: state})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// encodeSessionIdentity validates and renders the identity section.
func encodeSessionIdentity(v SessionIdentity) (json.RawMessage, error) {
	if err := validateSessionIdentity(v); err != nil {
		return nil, invalidInput("session identity: %v", err)
	}
	createdAt, err := encodeTime(v.CreatedAt)
	if err != nil {
		return nil, err
	}
	wire, err := json.Marshal(struct {
		SessionID             string `json:"session_id"`
		Workspace             string `json:"workspace"`
		CreatedAt             string `json:"created_at"`
		SourceSessionID       string `json:"source_session_id,omitempty"`
		SourceBoundaryEntryID string `json:"source_boundary_entry_id,omitempty"`
	}{
		SessionID:             v.SessionID,
		Workspace:             v.Workspace,
		CreatedAt:             createdAt,
		SourceSessionID:       v.SourceSessionID,
		SourceBoundaryEntryID: v.SourceBoundaryEntryID,
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// encodeSessionState validates and renders the state section.
func encodeSessionState(v SessionState) (json.RawMessage, error) {
	if err := validateSessionState(v); err != nil {
		return nil, invalidInput("session state: %v", err)
	}
	lastActivity, err := encodeTime(v.LastActivity)
	if err != nil {
		return nil, err
	}
	var archivedAt *string
	if v.ArchivedAt != nil {
		stamped, err := encodeTime(*v.ArchivedAt)
		if err != nil {
			return nil, err
		}
		archivedAt = &stamped
	}
	usage, err := encodeUsageTotalsWire(v.Usage)
	if err != nil {
		return nil, err
	}
	wire, err := json.Marshal(struct {
		Lifecycle          string          `json:"lifecycle"`
		ArchivedAt         *string         `json:"archived_at,omitempty"`
		CurrentAgentType   string          `json:"current_agent_type"`
		CurrentOperationID string          `json:"current_operation_id,omitempty"`
		Usage              json.RawMessage `json:"usage"`
		LastActivity       string          `json:"last_activity"`
	}{
		Lifecycle:          string(v.Lifecycle),
		ArchivedAt:         archivedAt,
		CurrentAgentType:   v.CurrentAgentType,
		CurrentOperationID: v.CurrentOperationID,
		Usage:              usage,
		LastActivity:       lastActivity,
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// decodeSessionRegister reads one Session register payload and enforces its
// agreement with the addressed envelope key.
func decodeSessionRegister(reg Register) (SessionRecord, error) {
	if reg.Key.Kind != RegisterSession {
		return SessionRecord{}, fmt.Errorf("register key kind %q is not %q", reg.Key.Kind, RegisterSession)
	}
	if err := validateHexID(reg.Key.SessionID, "register key session id"); err != nil {
		return SessionRecord{}, err
	}
	if reg.Key.OperationID != "" {
		return SessionRecord{}, errors.New("session register key carries an operation identity")
	}
	obj, err := decodePayloadObject(reg.Payload)
	if err != nil {
		return SessionRecord{}, err
	}
	if err := rejectUnknownMembers(obj, "identity", "state"); err != nil {
		return SessionRecord{}, err
	}
	identityObj, err := objectMember(obj, "identity", true)
	if err != nil {
		return SessionRecord{}, err
	}
	stateObj, err := objectMember(obj, "state", true)
	if err != nil {
		return SessionRecord{}, err
	}
	identity, err := decodeSessionIdentity(identityObj)
	if err != nil {
		return SessionRecord{}, fmt.Errorf("member %q: %w", "identity", err)
	}
	if identity.SessionID != reg.Key.SessionID {
		return SessionRecord{}, fmt.Errorf("payload session id %q does not agree with the register key", identity.SessionID)
	}
	state, err := decodeSessionState(stateObj)
	if err != nil {
		return SessionRecord{}, fmt.Errorf("member %q: %w", "state", err)
	}
	return SessionRecord{Revision: reg.Revision, Identity: identity, State: state}, nil
}

// decodeSessionIdentity reads the identity section with exact keys.
func decodeSessionIdentity(obj map[string]json.RawMessage) (SessionIdentity, error) {
	if err := rejectUnknownMembers(obj, "session_id", "workspace", "created_at", "source_session_id", "source_boundary_entry_id"); err != nil {
		return SessionIdentity{}, err
	}
	sessionID, err := stringMember(obj, "session_id", true)
	if err != nil {
		return SessionIdentity{}, err
	}
	workspace, err := stringMember(obj, "workspace", true)
	if err != nil {
		return SessionIdentity{}, err
	}
	createdAt, err := stringMember(obj, "created_at", true)
	if err != nil {
		return SessionIdentity{}, err
	}
	sourceSessionID, err := optionalNonEmptyString(obj, "source_session_id")
	if err != nil {
		return SessionIdentity{}, err
	}
	sourceBoundaryEntryID, err := optionalNonEmptyString(obj, "source_boundary_entry_id")
	if err != nil {
		return SessionIdentity{}, err
	}
	stamped, err := decodeTime(createdAt)
	if err != nil {
		return SessionIdentity{}, fmt.Errorf("member %q: %w", "created_at", err)
	}
	v := SessionIdentity{
		SessionID:             sessionID,
		Workspace:             workspace,
		CreatedAt:             stamped,
		SourceSessionID:       sourceSessionID,
		SourceBoundaryEntryID: sourceBoundaryEntryID,
	}
	if err := validateSessionIdentity(v); err != nil {
		return SessionIdentity{}, err
	}
	return v, nil
}

// validateSessionIdentity enforces the closed identity shape: durable
// identities, caller-normalized workspace, and the root/fork lineage rule
// (a root omits both source fields; a fork requires both).
func validateSessionIdentity(v SessionIdentity) error {
	if err := validateHexID(v.SessionID, "session id"); err != nil {
		return err
	}
	if err := validateWorkspace(v.Workspace); err != nil {
		return err
	}
	if v.CreatedAt.IsZero() {
		return errors.New("created_at must not be zero")
	}
	if (v.SourceSessionID == "") != (v.SourceBoundaryEntryID == "") {
		return errors.New("source lineage requires both source_session_id and source_boundary_entry_id")
	}
	if v.SourceSessionID != "" {
		if err := validateHexID(v.SourceSessionID, "source session id"); err != nil {
			return err
		}
		if err := validateHexID(v.SourceBoundaryEntryID, "source boundary entry id"); err != nil {
			return err
		}
	}
	return nil
}

// decodeSessionState reads the state section with exact keys.
func decodeSessionState(obj map[string]json.RawMessage) (SessionState, error) {
	if err := rejectUnknownMembers(obj, "lifecycle", "archived_at", "current_agent_type", "current_operation_id", "usage", "last_activity"); err != nil {
		return SessionState{}, err
	}
	lifecycle, err := stringMember(obj, "lifecycle", true)
	if err != nil {
		return SessionState{}, err
	}
	currentAgentType, err := stringMember(obj, "current_agent_type", true)
	if err != nil {
		return SessionState{}, err
	}
	currentOperationID, err := optionalNonEmptyString(obj, "current_operation_id")
	if err != nil {
		return SessionState{}, err
	}
	usageObj, err := objectMember(obj, "usage", true)
	if err != nil {
		return SessionState{}, err
	}
	usage, err := decodeUsageTotals(usageObj)
	if err != nil {
		return SessionState{}, fmt.Errorf("member %q: %w", "usage", err)
	}
	lastActivity, err := stringMember(obj, "last_activity", true)
	if err != nil {
		return SessionState{}, err
	}
	lastStamped, err := decodeTime(lastActivity)
	if err != nil {
		return SessionState{}, fmt.Errorf("member %q: %w", "last_activity", err)
	}
	v := SessionState{
		Lifecycle:          SessionLifecycle(lifecycle),
		CurrentAgentType:   currentAgentType,
		CurrentOperationID: currentOperationID,
		Usage:              usage,
		LastActivity:       lastStamped,
	}
	if _, present := obj["archived_at"]; present {
		archived, err := stringMember(obj, "archived_at", true)
		if err != nil {
			return SessionState{}, err
		}
		stamped, err := decodeTime(archived)
		if err != nil {
			return SessionState{}, fmt.Errorf("member %q: %w", "archived_at", err)
		}
		v.ArchivedAt = &stamped
	}
	if err := validateSessionState(v); err != nil {
		return SessionState{}, err
	}
	return v, nil
}

// validateSessionState enforces the closed state shape: closed lifecycle with
// archived_at present exactly when archived, non-empty Agent type, and a
// non-empty current Operation identity when present.
func validateSessionState(v SessionState) error {
	switch v.Lifecycle {
	case LifecycleOpen, LifecycleArchived:
	default:
		return fmt.Errorf("session lifecycle %q is not one of open or archived", v.Lifecycle)
	}
	if v.Lifecycle == LifecycleArchived && v.ArchivedAt == nil {
		return errors.New("archived session requires archived_at")
	}
	if v.Lifecycle == LifecycleOpen && v.ArchivedAt != nil {
		return errors.New("open session must not carry archived_at")
	}
	if v.CurrentAgentType == "" {
		return errors.New("current_agent_type must be non-empty")
	}
	if v.CurrentOperationID != "" {
		if err := validateOperationIdentity(v.CurrentOperationID, "current operation id"); err != nil {
			return err
		}
	}
	if v.LastActivity.IsZero() {
		return errors.New("last_activity must not be zero")
	}
	return validateUsageTotals(v.Usage)
}

// encodeOperationRegister renders one Operation register payload, exactly
// {"admission":...,"state":...}. The record's revision is envelope metadata
// and never enters the payload.
func encodeOperationRegister(rec OperationRecord) (json.RawMessage, error) {
	admission, err := encodeOperationAdmission(rec.Admission)
	if err != nil {
		return nil, err
	}
	state, err := encodeOperationState(rec.State)
	if err != nil {
		return nil, err
	}
	wire, err := json.Marshal(struct {
		Admission json.RawMessage `json:"admission"`
		State     json.RawMessage `json:"state"`
	}{Admission: admission, State: state})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// encodeOperationAdmission validates and renders the immutable admission
// section.
func encodeOperationAdmission(v OperationAdmission) (json.RawMessage, error) {
	if err := validateOperationAdmission(v); err != nil {
		return nil, invalidInput("operation admission: %v", err)
	}
	admittedEntry, err := encodeEntryRef(v.AdmittedEntry)
	if err != nil {
		return nil, err
	}
	execution, err := encodeExecutionCapture(v.Execution)
	if err != nil {
		return nil, err
	}
	admittedAt, err := encodeTime(v.AdmittedAt)
	if err != nil {
		return nil, err
	}
	wire, err := json.Marshal(struct {
		SessionID     string          `json:"session_id"`
		OperationID   string          `json:"operation_id"`
		RequestKind   string          `json:"request_kind"`
		AdmittedEntry json.RawMessage `json:"admitted_entry"`
		AgentType     string          `json:"agent_type"`
		Execution     json.RawMessage `json:"execution"`
		AdmittedAt    string          `json:"admitted_at"`
	}{
		SessionID:     v.SessionID,
		OperationID:   v.OperationID,
		RequestKind:   string(v.RequestKind),
		AdmittedEntry: admittedEntry,
		AgentType:     v.AgentType,
		Execution:     execution,
		AdmittedAt:    admittedAt,
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// encodeExecutionCapture validates and renders the durable capture.
func encodeExecutionCapture(v ExecutionCapture) (json.RawMessage, error) {
	if err := validateExecutionCapture(v); err != nil {
		return nil, invalidInput("execution capture: %v", err)
	}
	modelRaw, err := encodeModelRef(v.Model)
	if err != nil {
		return nil, err
	}
	tools := make([]json.RawMessage, 0, len(v.Tools))
	for _, tool := range v.Tools {
		raw, err := encodeToolDefinition(tool)
		if err != nil {
			return nil, err
		}
		tools = append(tools, raw)
	}
	wire, err := json.Marshal(struct {
		ConfigurationRevision string            `json:"configuration_revision"`
		Model                 json.RawMessage   `json:"model"`
		SystemPrompt          string            `json:"system_prompt"`
		Tools                 []json.RawMessage `json:"tools"`
	}{
		ConfigurationRevision: v.ConfigurationRevision,
		Model:                 modelRaw,
		SystemPrompt:          v.SystemPrompt,
		Tools:                 tools,
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// decodeOperationRegister reads one Operation register payload and enforces
// its agreement with the addressed envelope key.
func decodeOperationRegister(reg Register) (OperationRecord, error) {
	if reg.Key.Kind != RegisterOperation {
		return OperationRecord{}, fmt.Errorf("register key kind %q is not %q", reg.Key.Kind, RegisterOperation)
	}
	if err := validateHexID(reg.Key.SessionID, "register key session id"); err != nil {
		return OperationRecord{}, err
	}
	if err := validateOperationIdentity(reg.Key.OperationID, "register key operation id"); err != nil {
		return OperationRecord{}, err
	}
	obj, err := decodePayloadObject(reg.Payload)
	if err != nil {
		return OperationRecord{}, err
	}
	if err := rejectUnknownMembers(obj, "admission", "state"); err != nil {
		return OperationRecord{}, err
	}
	admissionObj, err := objectMember(obj, "admission", true)
	if err != nil {
		return OperationRecord{}, err
	}
	stateObj, err := objectMember(obj, "state", true)
	if err != nil {
		return OperationRecord{}, err
	}
	admission, err := decodeOperationAdmission(admissionObj)
	if err != nil {
		return OperationRecord{}, fmt.Errorf("member %q: %w", "admission", err)
	}
	if admission.SessionID != reg.Key.SessionID {
		return OperationRecord{}, fmt.Errorf("payload session id %q does not agree with the register key", admission.SessionID)
	}
	if admission.OperationID != reg.Key.OperationID {
		return OperationRecord{}, fmt.Errorf("payload operation id %q does not agree with the register key", admission.OperationID)
	}
	state, err := decodeOperationState(stateObj)
	if err != nil {
		return OperationRecord{}, fmt.Errorf("member %q: %w", "state", err)
	}
	return OperationRecord{Revision: reg.Revision, Admission: admission, State: state}, nil
}

// decodeOperationAdmission reads the admission section with exact keys.
func decodeOperationAdmission(obj map[string]json.RawMessage) (OperationAdmission, error) {
	if err := rejectUnknownMembers(obj, "session_id", "operation_id", "request_kind", "admitted_entry", "agent_type", "execution", "admitted_at"); err != nil {
		return OperationAdmission{}, err
	}
	sessionID, err := stringMember(obj, "session_id", true)
	if err != nil {
		return OperationAdmission{}, err
	}
	operationID, err := stringMember(obj, "operation_id", true)
	if err != nil {
		return OperationAdmission{}, err
	}
	requestKind, err := stringMember(obj, "request_kind", true)
	if err != nil {
		return OperationAdmission{}, err
	}
	admittedObj, err := objectMember(obj, "admitted_entry", true)
	if err != nil {
		return OperationAdmission{}, err
	}
	admittedEntry, err := decodeEntryRef(admittedObj)
	if err != nil {
		return OperationAdmission{}, fmt.Errorf("member %q: %w", "admitted_entry", err)
	}
	agentType, err := stringMember(obj, "agent_type", true)
	if err != nil {
		return OperationAdmission{}, err
	}
	executionObj, err := objectMember(obj, "execution", true)
	if err != nil {
		return OperationAdmission{}, err
	}
	execution, err := decodeExecutionCapture(executionObj)
	if err != nil {
		return OperationAdmission{}, fmt.Errorf("member %q: %w", "execution", err)
	}
	admittedAt, err := stringMember(obj, "admitted_at", true)
	if err != nil {
		return OperationAdmission{}, err
	}
	stamped, err := decodeTime(admittedAt)
	if err != nil {
		return OperationAdmission{}, fmt.Errorf("member %q: %w", "admitted_at", err)
	}
	v := OperationAdmission{
		SessionID:     sessionID,
		OperationID:   operationID,
		RequestKind:   RequestKind(requestKind),
		AdmittedEntry: admittedEntry,
		AgentType:     agentType,
		Execution:     execution,
		AdmittedAt:    stamped,
	}
	if err := validateOperationAdmission(v); err != nil {
		return OperationAdmission{}, err
	}
	return v, nil
}

// validateOperationAdmission enforces the closed admission shape: durable
// identities, the single message request kind, non-empty Agent type, and a
// complete valid capture.
func validateOperationAdmission(v OperationAdmission) error {
	if err := validateHexID(v.SessionID, "session id"); err != nil {
		return err
	}
	if err := validateOperationIdentity(v.OperationID, "operation id"); err != nil {
		return err
	}
	if v.RequestKind != RequestKindMessage {
		return fmt.Errorf("request kind %q is not %q", v.RequestKind, RequestKindMessage)
	}
	if err := validateHexID(v.AdmittedEntry.SessionID, "admitted entry session id"); err != nil {
		return err
	}
	if err := validateHexID(v.AdmittedEntry.EntryID, "admitted entry id"); err != nil {
		return err
	}
	if v.AgentType == "" {
		return errors.New("agent_type must be non-empty")
	}
	if v.AdmittedAt.IsZero() {
		return errors.New("admitted_at must not be zero")
	}
	return validateExecutionCapture(v.Execution)
}

// decodeExecutionCapture reads the durable capture with exact keys, unique
// tool names, and preserved tool order.
func decodeExecutionCapture(obj map[string]json.RawMessage) (ExecutionCapture, error) {
	if err := rejectUnknownMembers(obj, "configuration_revision", "model", "system_prompt", "tools"); err != nil {
		return ExecutionCapture{}, err
	}
	revision, err := stringMember(obj, "configuration_revision", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	modelObj, err := objectMember(obj, "model", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	ref, err := decodeModelRef(modelObj)
	if err != nil {
		return ExecutionCapture{}, fmt.Errorf("member %q: %w", "model", err)
	}
	systemPrompt, err := stringMember(obj, "system_prompt", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	toolsRaw, err := arrayMember(obj, "tools", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	v := ExecutionCapture{ConfigurationRevision: revision, Model: ref, SystemPrompt: systemPrompt}
	for i, raw := range toolsRaw {
		tool, err := decodeToolDefinition(raw)
		if err != nil {
			return ExecutionCapture{}, fmt.Errorf("tools[%d]: %w", i, err)
		}
		v.Tools = append(v.Tools, tool)
	}
	if err := validateExecutionCapture(v); err != nil {
		return ExecutionCapture{}, err
	}
	return v, nil
}

// validateExecutionCapture enforces the closed capture shape: non-empty
// stable revision, complete model identity, and unique tool names in
// preserved order through the landed request constructor.
func validateExecutionCapture(v ExecutionCapture) error {
	if v.ConfigurationRevision == "" {
		return errors.New("configuration_revision must be non-empty")
	}
	if v.Model.Provider == "" || v.Model.Model == "" {
		return fmt.Errorf("model %q must be a complete model identity", v.Model.String())
	}
	if _, err := model.NewRequest(model.Request{Tools: v.Tools}); err != nil {
		return err
	}
	return nil
}

// encodeOperationState validates and renders the mutable state section.
func encodeOperationState(v OperationCurrentState) (json.RawMessage, error) {
	if err := validateOperationState(v); err != nil {
		return nil, invalidInput("operation state: %v", err)
	}
	startedAt, err := encodeTime(v.StartedAt)
	if err != nil {
		return nil, err
	}
	var settledAt *string
	if v.SettledAt != nil {
		stamped, err := encodeTime(*v.SettledAt)
		if err != nil {
			return nil, err
		}
		settledAt = &stamped
	}
	var activeEffect *activeEffectWire
	if v.ActiveEffect != nil {
		activeEffect = &activeEffectWire{
			Kind:          string(v.ActiveEffect.Kind),
			ResultEntryID: v.ActiveEffect.ResultEntryID,
			ToolCallID:    v.ActiveEffect.ToolCallID,
		}
	}
	pending := make([]pendingToolCallWire, 0, len(v.PendingToolCalls))
	for _, call := range v.PendingToolCalls {
		pending = append(pending, pendingToolCallWire{
			AssistantEntry: entryRefWire{SessionID: call.AssistantEntry.SessionID, EntryID: call.AssistantEntry.EntryID},
			CallID:         call.CallID,
			ResultEntryID:  call.ResultEntryID,
		})
	}
	usage, err := encodeUsageTotalsWire(v.Usage)
	if err != nil {
		return nil, err
	}
	var terminal *operationTerminalWire
	if v.Terminal != nil {
		terminal = &operationTerminalWire{
			SettlementEntry: entryRefWire{SessionID: v.Terminal.SettlementEntry.SessionID, EntryID: v.Terminal.SettlementEntry.EntryID},
			Detail:          v.Terminal.Detail,
		}
	}
	wire, err := json.Marshal(struct {
		Status           string                 `json:"status"`
		StartedAt        string                 `json:"started_at"`
		SettledAt        *string                `json:"settled_at,omitempty"`
		ActiveEffect     *activeEffectWire      `json:"active_effect,omitempty"`
		PendingToolCalls []pendingToolCallWire  `json:"pending_tool_calls"`
		Usage            json.RawMessage        `json:"usage"`
		Terminal         *operationTerminalWire `json:"terminal,omitempty"`
	}{
		Status:           string(v.Status),
		StartedAt:        startedAt,
		SettledAt:        settledAt,
		ActiveEffect:     activeEffect,
		PendingToolCalls: pending,
		Usage:            usage,
		Terminal:         terminal,
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

type entryRefWire struct {
	SessionID string `json:"session_id"`
	EntryID   string `json:"entry_id"`
}

type activeEffectWire struct {
	Kind          string `json:"kind"`
	ResultEntryID string `json:"result_entry_id"`
	ToolCallID    string `json:"tool_call_id,omitempty"`
}

type pendingToolCallWire struct {
	AssistantEntry entryRefWire `json:"assistant_entry"`
	CallID         string       `json:"call_id"`
	ResultEntryID  string       `json:"result_entry_id"`
}

type operationTerminalWire struct {
	SettlementEntry entryRefWire `json:"settlement_entry"`
	Detail          string       `json:"detail,omitempty"`
}

// decodeOperationState reads the state section with exact keys.
func decodeOperationState(obj map[string]json.RawMessage) (OperationCurrentState, error) {
	if err := rejectUnknownMembers(obj, "status", "started_at", "settled_at", "active_effect", "pending_tool_calls", "usage", "terminal"); err != nil {
		return OperationCurrentState{}, err
	}
	status, err := stringMember(obj, "status", true)
	if err != nil {
		return OperationCurrentState{}, err
	}
	startedAt, err := stringMember(obj, "started_at", true)
	if err != nil {
		return OperationCurrentState{}, err
	}
	started, err := decodeTime(startedAt)
	if err != nil {
		return OperationCurrentState{}, fmt.Errorf("member %q: %w", "started_at", err)
	}
	usageObj, err := objectMember(obj, "usage", true)
	if err != nil {
		return OperationCurrentState{}, err
	}
	usage, err := decodeUsageTotals(usageObj)
	if err != nil {
		return OperationCurrentState{}, fmt.Errorf("member %q: %w", "usage", err)
	}
	v := OperationCurrentState{
		Status:    OperationState(status),
		StartedAt: started,
		Usage:     usage,
	}
	if _, present := obj["settled_at"]; present {
		settled, err := stringMember(obj, "settled_at", true)
		if err != nil {
			return OperationCurrentState{}, err
		}
		stamped, err := decodeTime(settled)
		if err != nil {
			return OperationCurrentState{}, fmt.Errorf("member %q: %w", "settled_at", err)
		}
		v.SettledAt = &stamped
	}
	if _, present := obj["active_effect"]; present {
		effectObj, err := objectMember(obj, "active_effect", true)
		if err != nil {
			return OperationCurrentState{}, err
		}
		effect, err := decodeActiveEffect(effectObj)
		if err != nil {
			return OperationCurrentState{}, fmt.Errorf("member %q: %w", "active_effect", err)
		}
		v.ActiveEffect = &effect
	}
	pendingRaw, err := arrayMember(obj, "pending_tool_calls", true)
	if err != nil {
		return OperationCurrentState{}, err
	}
	for i, raw := range pendingRaw {
		call, err := decodePendingToolCall(raw)
		if err != nil {
			return OperationCurrentState{}, fmt.Errorf("pending_tool_calls[%d]: %w", i, err)
		}
		v.PendingToolCalls = append(v.PendingToolCalls, call)
	}
	if _, present := obj["terminal"]; present {
		terminalObj, err := objectMember(obj, "terminal", true)
		if err != nil {
			return OperationCurrentState{}, err
		}
		terminal, err := decodeOperationTerminal(terminalObj)
		if err != nil {
			return OperationCurrentState{}, fmt.Errorf("member %q: %w", "terminal", err)
		}
		v.Terminal = &terminal
	}
	if err := validateOperationState(v); err != nil {
		return OperationCurrentState{}, err
	}
	return v, nil
}

// decodeActiveEffect reads one active effect with exact keys.
func decodeActiveEffect(obj map[string]json.RawMessage) (ActiveEffect, error) {
	if err := rejectUnknownMembers(obj, "kind", "result_entry_id", "tool_call_id"); err != nil {
		return ActiveEffect{}, err
	}
	kind, err := stringMember(obj, "kind", true)
	if err != nil {
		return ActiveEffect{}, err
	}
	resultEntryID, err := stringMember(obj, "result_entry_id", true)
	if err != nil {
		return ActiveEffect{}, err
	}
	toolCallID, err := optionalNonEmptyString(obj, "tool_call_id")
	if err != nil {
		return ActiveEffect{}, err
	}
	v := ActiveEffect{Kind: EffectKind(kind), ResultEntryID: resultEntryID, ToolCallID: toolCallID}
	if err := validateHexID(v.ResultEntryID, "active effect result entry id"); err != nil {
		return ActiveEffect{}, err
	}
	return v, nil
}

// decodePendingToolCall reads one pending call with exact keys.
func decodePendingToolCall(raw json.RawMessage) (PendingToolCall, error) {
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return PendingToolCall{}, fmt.Errorf("pending tool call: %w", err)
	}
	if err := rejectUnknownMembers(obj, "assistant_entry", "call_id", "result_entry_id"); err != nil {
		return PendingToolCall{}, err
	}
	assistantObj, err := objectMember(obj, "assistant_entry", true)
	if err != nil {
		return PendingToolCall{}, err
	}
	assistantEntry, err := decodeEntryRef(assistantObj)
	if err != nil {
		return PendingToolCall{}, fmt.Errorf("member %q: %w", "assistant_entry", err)
	}
	callID, err := stringMember(obj, "call_id", true)
	if err != nil {
		return PendingToolCall{}, err
	}
	resultEntryID, err := stringMember(obj, "result_entry_id", true)
	if err != nil {
		return PendingToolCall{}, err
	}
	v := PendingToolCall{AssistantEntry: assistantEntry, CallID: callID, ResultEntryID: resultEntryID}
	if err := validateOperationIdentity(v.CallID, "pending call id"); err != nil {
		return PendingToolCall{}, err
	}
	if err := validateHexID(v.ResultEntryID, "pending call reserved result entry id"); err != nil {
		return PendingToolCall{}, err
	}
	return v, nil
}

// decodeOperationTerminal reads one terminal section with exact keys.
func decodeOperationTerminal(obj map[string]json.RawMessage) (OperationTerminal, error) {
	if err := rejectUnknownMembers(obj, "settlement_entry", "detail"); err != nil {
		return OperationTerminal{}, err
	}
	settlementObj, err := objectMember(obj, "settlement_entry", true)
	if err != nil {
		return OperationTerminal{}, err
	}
	settlementEntry, err := decodeEntryRef(settlementObj)
	if err != nil {
		return OperationTerminal{}, fmt.Errorf("member %q: %w", "settlement_entry", err)
	}
	detail, err := optionalNonEmptyString(obj, "detail")
	if err != nil {
		return OperationTerminal{}, err
	}
	return OperationTerminal{SettlementEntry: settlementEntry, Detail: detail}, nil
}

// validateOperationState enforces the closed state shape: running forbids the
// settlement time and terminal section; terminal states require both, forbid
// active and pending effects, carry the contract detail rule, and name a
// durable settlement entry; a tool active effect addresses the matching first
// pending call under its reserved result identity; active effects carry
// durable result identities; pending calls have unique identities and
// reserved results.
func validateOperationState(v OperationCurrentState) error {
	switch v.Status {
	case OperationRunning, OperationSuccess, OperationFailure, OperationInterruption:
	default:
		return fmt.Errorf("operation status %q is not one of running, success, failure or interruption", v.Status)
	}
	if v.StartedAt.IsZero() {
		return errors.New("started_at must not be zero")
	}
	if v.Status == OperationRunning {
		if v.SettledAt != nil || v.Terminal != nil {
			return errors.New("running operation forbids the settlement time and terminal section")
		}
	} else {
		if v.SettledAt == nil || v.Terminal == nil {
			return errors.New("terminal operation requires both the settlement time and terminal section")
		}
		if v.ActiveEffect != nil {
			return errors.New("terminal operation forbids an active effect")
		}
		if len(v.PendingToolCalls) > 0 {
			return errors.New("terminal operation forbids pending tool calls")
		}
		if v.SettledAt.IsZero() {
			return errors.New("settled_at must not be zero")
		}
		if err := validateHexID(v.Terminal.SettlementEntry.SessionID, "terminal settlement entry session id"); err != nil {
			return err
		}
		if err := validateHexID(v.Terminal.SettlementEntry.EntryID, "terminal settlement entry id"); err != nil {
			return err
		}
		switch v.Status {
		case OperationSuccess:
			if v.Terminal.Detail != "" {
				return errors.New("success operation requires empty terminal detail")
			}
		case OperationFailure, OperationInterruption:
			if v.Terminal.Detail == "" {
				return fmt.Errorf("%s operation requires non-empty terminal detail", v.Status)
			}
		}
	}
	if v.ActiveEffect != nil {
		if err := validateHexID(v.ActiveEffect.ResultEntryID, "active effect result entry id"); err != nil {
			return err
		}
		switch v.ActiveEffect.Kind {
		case EffectModel:
			if v.ActiveEffect.ToolCallID != "" {
				return errors.New("model active effect omits the tool call id")
			}
		case EffectTool:
			if v.ActiveEffect.ToolCallID == "" {
				return errors.New("tool active effect requires its tool call id")
			}
			if len(v.PendingToolCalls) == 0 || v.ActiveEffect.ToolCallID != v.PendingToolCalls[0].CallID {
				return errors.New("tool active effect must address the matching first pending call")
			}
			if v.ActiveEffect.ResultEntryID != v.PendingToolCalls[0].ResultEntryID {
				return errors.New("tool active effect must reserve the first pending call's result identity")
			}
		default:
			return fmt.Errorf("active effect kind %q is not one of model or tool", v.ActiveEffect.Kind)
		}
	}
	seenCalls := make(map[string]bool, len(v.PendingToolCalls))
	seenResults := make(map[string]bool, len(v.PendingToolCalls))
	for i, call := range v.PendingToolCalls {
		if err := validateHexID(call.AssistantEntry.SessionID, "pending call assistant entry session id"); err != nil {
			return err
		}
		if err := validateHexID(call.AssistantEntry.EntryID, "pending call assistant entry id"); err != nil {
			return err
		}
		if err := validateOperationIdentity(call.CallID, "pending call id"); err != nil {
			return err
		}
		if err := validateHexID(call.ResultEntryID, "pending call reserved result entry id"); err != nil {
			return err
		}
		if seenCalls[call.CallID] {
			return fmt.Errorf("pending_tool_calls[%d]: call id %q is not unique", i, call.CallID)
		}
		seenCalls[call.CallID] = true
		if seenResults[call.ResultEntryID] {
			return fmt.Errorf("pending_tool_calls[%d]: reserved result %q is not unique", i, call.ResultEntryID)
		}
		seenResults[call.ResultEntryID] = true
	}
	return validateUsageTotals(v.Usage)
}
