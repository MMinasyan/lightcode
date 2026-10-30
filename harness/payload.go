package harness

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"unicode/utf8"

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
		if err := model.ValidateContentPart(part); err != nil {
			return fmt.Errorf("content[%d]: %w", i, err)
		}
	}
	return nil
}

// encodeToolCallRecord renders one assistant tool-call record directly from
// the validated tagged value. Raw arguments persist as the base64
// standard-alphabet string so malformed JSON and arbitrary bytes round-trip
// exactly; optional members omit when empty.
func encodeToolCallRecord(c toolCallRecord) (json.RawMessage, error) {
	if err := validateToolCallRecord(c); err != nil {
		return nil, invalidInput("tool call: %v", err)
	}
	return json.Marshal(c)
}

// decodeToolCallRecord reads one assistant tool-call record with exact keys.
func decodeToolCallRecord(raw json.RawMessage) (toolCallRecord, error) {
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return toolCallRecord{}, fmt.Errorf("tool call: %w", err)
	}
	if err := rejectUnknownMembers(obj, "id", "ordinal", "name", "arguments_base64", "extra", "normalized_arguments", "normalization_failure", "result_entry_id"); err != nil {
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
	if c.NormalizationFailure, err = optionalNonEmptyString(obj, "normalization_failure"); err != nil {
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
// is enforced by the owning assistant entry. A normalization failure is
// mutually exclusive with a successful normalization: when present it is a
// non-empty valid-UTF-8 diagnostic within the durable diagnostic bound.
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
		if _, err := decodePayloadObject(c.NormalizedArguments); err != nil {
			return fmt.Errorf("tool call normalized_arguments must be one complete JSON object: %v", err)
		}
	}
	if c.NormalizationFailure != "" {
		if len(c.NormalizedArguments) > 0 {
			return errors.New("tool call carries both normalized_arguments and normalization_failure")
		}
		if !utf8.ValidString(c.NormalizationFailure) || len(c.NormalizationFailure) > maxToolDiagnosticBytes {
			return fmt.Errorf("tool call normalization_failure must be valid UTF-8 within the %d byte durable diagnostic bound", maxToolDiagnosticBytes)
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
	evidence, err := encodeRequestEvidence(*v.Evidence)
	if err != nil {
		return nil, err
	}
	advertised := v.AdvertisedNames
	if advertised == nil {
		advertised = []string{}
	}
	wire, err := json.Marshal(struct {
		SessionID       string            `json:"session_id"`
		EntryID         string            `json:"entry_id"`
		OperationID     string            `json:"operation_id,omitempty"`
		Status          string            `json:"status"`
		Source          json.RawMessage   `json:"source"`
		Content         []json.RawMessage `json:"content"`
		Refusal         string            `json:"refusal,omitempty"`
		Extra           json.RawMessage   `json:"extra,omitempty"`
		ToolCalls       []json.RawMessage `json:"tool_calls"`
		Usage           *UsageCount       `json:"usage,omitempty"`
		RequestID       string            `json:"request_id"`
		AdvertisedNames []string          `json:"advertised_names"`
		Evidence        json.RawMessage   `json:"evidence"`
	}{
		SessionID:       v.SessionID,
		EntryID:         v.EntryID,
		OperationID:     v.OperationID,
		Status:          string(v.Status),
		Source:          source,
		Content:         content,
		Refusal:         v.Refusal,
		Extra:           extra,
		ToolCalls:       calls,
		Usage:           v.Usage,
		RequestID:       v.RequestID,
		AdvertisedNames: advertised,
		Evidence:        evidence,
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
	if err := rejectUnknownMembers(obj, "session_id", "entry_id", "operation_id", "status", "source", "content", "refusal", "extra", "tool_calls", "usage", "request_id", "advertised_names", "evidence"); err != nil {
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
	requestID, err := stringMember(obj, "request_id", true)
	if err != nil {
		return assistantEntry{}, err
	}
	advertisedNames, err := optionalStringMemberList(obj, "advertised_names")
	if err != nil {
		return assistantEntry{}, err
	}
	if advertisedNames == nil {
		return assistantEntry{}, errors.New(`required member "advertised_names" is missing`)
	}
	evidence, err := decodeRequestEvidence(obj["evidence"])
	if err != nil {
		return assistantEntry{}, fmt.Errorf("member %q: %w", "evidence", err)
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
		SessionID:       sessionID,
		EntryID:         entryID,
		OperationID:     operationID,
		Status:          model.OutputStatus(status),
		Source:          source,
		Refusal:         refusal,
		Extra:           extra,
		Usage:           usage,
		RequestID:       requestID,
		AdvertisedNames: advertisedNames,
		Evidence:        &evidence,
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
// output, the closed per-call normalization rule over the advertised names,
// and an eligible model-visible payload.
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
		if err := model.ValidateContentPart(part); err != nil {
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
	if err := validateOperationIdentity(v.RequestID, "assistant request id"); err != nil {
		return err
	}
	seenNames := make(map[string]bool, len(v.AdvertisedNames))
	for _, name := range v.AdvertisedNames {
		if name == "" {
			return errors.New("assistant advertised_names carries an empty name")
		}
		if seenNames[name] {
			return fmt.Errorf("assistant advertised_names has duplicate name %q", name)
		}
		seenNames[name] = true
	}
	// One closed normalization rule over the producing request's advertised
	// names: an advertised call carries exactly one of the successful
	// normalization or its bounded failure diagnostic, and an unadvertised
	// call carries neither.
	for i, call := range v.ToolCalls {
		normalized, failed := len(call.NormalizedArguments) > 0, call.NormalizationFailure != ""
		switch {
		case seenNames[call.Name]:
			if normalized == failed {
				return fmt.Errorf("tool_calls[%d]: advertised call %q must carry exactly one of normalized_arguments or normalization_failure", i, call.ID)
			}
		case normalized || failed:
			return fmt.Errorf("tool_calls[%d]: unadvertised call %q must carry neither normalized_arguments nor normalization_failure", i, call.ID)
		}
	}
	if v.Evidence == nil {
		return errors.New("assistant entry requires its effective request evidence")
	}
	if err := validateRequestEvidence(*v.Evidence); err != nil {
		return err
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
		if model.ContentPartHasPayload(part) {
			return true
		}
	}
	if v.Extra.HasValues() {
		return true
	}
	return v.Status == model.OutputCompleted && len(v.ToolCalls) > 0
}

// maxToolMetadataBytes is the durable bound of one tool-result metadata
// value: the Harness enforces well-formedness and this size only, never the
// plugin-owned semantics of the value.
const maxToolMetadataBytes = 1 << 20 // 1 MiB

// ownedToolMetadata is the one shared metadata mechanism: it returns the
// owned bytes json.Marshal produces for one candidate tool_result metadata
// member — one complete well-formed non-null JSON value whose durable
// encoding (the compacted, HTML-escaped bytes json.Marshal produces for a
// json.RawMessage) stays within the bound. Empty, malformed, null, or
// oversized input returns nil. The bound applies to the persisted
// representation, not to the caller's raw input, and the returned buffer
// never aliases the caller.
func ownedToolMetadata(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil || string(encoded) == "null" || len(encoded) > maxToolMetadataBytes {
		return nil
	}
	return encoded
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
	hookIDs, err := encodeHookIdentities(v.HookIDs)
	if err != nil {
		return nil, err
	}
	var normalized json.RawMessage
	if len(v.NormalizedArguments) > 0 {
		normalized = model.CloneRaw(v.NormalizedArguments)
	}
	wire, err := json.Marshal(struct {
		SessionID           string          `json:"session_id"`
		EntryID             string          `json:"entry_id"`
		OperationID         string          `json:"operation_id,omitempty"`
		AssistantEntry      json.RawMessage `json:"assistant_entry"`
		ToolCallID          string          `json:"tool_call_id"`
		Status              string          `json:"status"`
		Content             string          `json:"content"`
		OriginalContent     string          `json:"original_content"`
		ShapingFailure      string          `json:"shaping_failure,omitempty"`
		Evidence            string          `json:"evidence"`
		Metadata            json.RawMessage `json:"metadata,omitempty"`
		NormalizedArguments json.RawMessage `json:"normalized_arguments,omitempty"`
		HookIDs             json.RawMessage `json:"hook_ids,omitempty"`
	}{
		SessionID:           v.SessionID,
		EntryID:             v.EntryID,
		OperationID:         v.OperationID,
		AssistantEntry:      assistantEntry,
		ToolCallID:          v.ToolCallID,
		Status:              string(v.Status),
		Content:             v.Content,
		OriginalContent:     v.OriginalContent,
		ShapingFailure:      v.ShapingFailure,
		Evidence:            string(v.Evidence),
		Metadata:            v.Metadata,
		NormalizedArguments: normalized,
		HookIDs:             hookIDs,
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
	if err := rejectUnknownMembers(obj, "session_id", "entry_id", "operation_id", "assistant_entry", "tool_call_id", "status", "content", "original_content", "shaping_failure", "evidence", "metadata", "normalized_arguments", "hook_ids"); err != nil {
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
	metadata, err := rawJSONMember(obj, "metadata", false)
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
	originalContent, err := stringMember(obj, "original_content", true)
	if err != nil {
		return toolResultEntry{}, err
	}
	shapingFailure, err := optionalNonEmptyString(obj, "shaping_failure")
	if err != nil {
		return toolResultEntry{}, err
	}
	evidence, err := stringMember(obj, "evidence", true)
	if err != nil {
		return toolResultEntry{}, err
	}
	normalizedArguments, err := rawJSONMember(obj, "normalized_arguments", false)
	if err != nil {
		return toolResultEntry{}, err
	}
	hookIDs, err := decodeHookIdentities(obj, "hook_ids")
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
		SessionID:           sessionID,
		EntryID:             entryID,
		OperationID:         operationID,
		AssistantEntry:      assistantEntry,
		ToolCallID:          toolCallID,
		Status:              model.ToolResultStatus(status),
		Content:             content,
		OriginalContent:     originalContent,
		ShapingFailure:      shapingFailure,
		Evidence:            ExecutionEvidence(evidence),
		Metadata:            metadata,
		NormalizedArguments: normalizedArguments,
		HookIDs:             hookIDs,
	}
	if err := validateToolResultEntry(v); err != nil {
		return toolResultEntry{}, err
	}
	return v, nil
}

// validateToolResultEntry enforces the closed tool-result shape: durable
// identities, non-empty original call id, the landed Agent result rules
// re-enforced through the landed constructor for the final content and the
// original content alike, one closed execution-evidence value with no status
// coupling, a bounded valid-UTF-8 shaping failure when present, and a present
// metadata member accepted by the one shared owning helper; omitted metadata
// stays valid.
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
	if _, err := model.NewToolResult(model.ToolResult{CallID: v.ToolCallID, Status: v.Status, Content: v.OriginalContent}); err != nil {
		return err
	}
	if v.ShapingFailure != "" {
		if !utf8.ValidString(v.ShapingFailure) || len(v.ShapingFailure) > maxToolDiagnosticBytes {
			return fmt.Errorf("tool result shaping_failure must be valid UTF-8 within the %d byte durable diagnostic bound", maxToolDiagnosticBytes)
		}
	}
	if !validExecutionEvidence(v.Evidence) {
		return fmt.Errorf("tool result evidence %q is not one of not_started, observed or unknown", v.Evidence)
	}
	if len(v.Metadata) > 0 && len(ownedToolMetadata(v.Metadata)) == 0 {
		return fmt.Errorf("tool result metadata must be one well-formed non-null JSON value within the %d byte durable bound", maxToolMetadataBytes)
	}
	if len(v.NormalizedArguments) > 0 {
		if _, err := decodePayloadObject(v.NormalizedArguments); err != nil {
			return errors.New("tool result normalized_arguments must be one complete JSON object")
		}
	}
	if len(v.HookIDs) > 0 && len(v.NormalizedArguments) == 0 {
		return errors.New("tool result hook provenance requires its accepted normalized arguments")
	}
	return validateHookIdentities(v.HookIDs)
}

// encodeHookResultEntry renders one hook-result entry payload through the
// closed member codecs: nested evidence and consequence values ride the shared
// RequestEvidence and RequestConsequence codecs, never struct marshaling.
func encodeHookResultEntry(v hookResultEntry) (json.RawMessage, error) {
	if err := validateHookResultEntry(v); err != nil {
		return nil, invalidInput("hook result entry: %v", err)
	}
	obj := map[string]json.RawMessage{
		"session_id":   mustMarshal(v.SessionID),
		"entry_id":     mustMarshal(v.EntryID),
		"operation_id": mustMarshal(v.OperationID),
		"hook_id":      mustMarshal(v.HookID),
		"extension_id": mustMarshal(v.ExtensionID),
		"status":       mustMarshal(string(v.Status)),
		"evidence":     mustMarshal(string(v.Evidence)),
	}
	if v.ToolCallID != "" {
		obj["tool_call_id"] = mustMarshal(v.ToolCallID)
	}
	if v.RequestID != "" {
		obj["request_id"] = mustMarshal(v.RequestID)
	}
	if v.Status == hookSucceeded {
		// A successful effectful execution always carries its binding-owned
		// consequence record.
		if v.ToolCallID != "" {
			obj["arguments"] = model.CloneRaw(v.Arguments)
		} else {
			raw, err := encodeRequestConsequence(*v.Consequence)
			if err != nil {
				return nil, err
			}
			obj["consequence"] = raw
		}
	}
	if v.Error != "" {
		obj["error"] = mustMarshal(v.Error)
	}
	if v.ToolCallID != "" {
		obj["input_arguments_base64"] = mustMarshal(v.InputArgumentsBase64)
	} else {
		raw, err := encodeRequestEvidence(*v.InputEvidence)
		if err != nil {
			return nil, err
		}
		obj["input_evidence"] = raw
	}
	return json.Marshal(obj)
}

// decodeHookResultEntry reads one hook-result entry payload and enforces its
// agreement with the addressed envelope identity. A hook result is never
// independently copied: its owning Operation identity is required.
func decodeHookResultEntry(env Entry) (hookResultEntry, error) {
	if err := decodeEntryEnvelope(env); err != nil {
		return hookResultEntry{}, err
	}
	obj, err := decodePayloadObject(env.Payload)
	if err != nil {
		return hookResultEntry{}, err
	}
	if err := rejectUnknownMembers(obj, "session_id", "entry_id", "operation_id", "hook_id", "extension_id", "tool_call_id", "request_id", "status", "evidence", "arguments", "error", "input_arguments_base64", "input_evidence", "consequence"); err != nil {
		return hookResultEntry{}, err
	}
	sessionID, err := stringMember(obj, "session_id", true)
	if err != nil {
		return hookResultEntry{}, err
	}
	entryID, err := stringMember(obj, "entry_id", true)
	if err != nil {
		return hookResultEntry{}, err
	}
	operationID, err := stringMember(obj, "operation_id", true)
	if err != nil {
		return hookResultEntry{}, err
	}
	hookID, err := stringMember(obj, "hook_id", true)
	if err != nil {
		return hookResultEntry{}, err
	}
	extensionID, err := stringMember(obj, "extension_id", true)
	if err != nil {
		return hookResultEntry{}, err
	}
	toolCallID, err := optionalNonEmptyString(obj, "tool_call_id")
	if err != nil {
		return hookResultEntry{}, err
	}
	requestID, err := optionalNonEmptyString(obj, "request_id")
	if err != nil {
		return hookResultEntry{}, err
	}
	status, err := stringMember(obj, "status", true)
	if err != nil {
		return hookResultEntry{}, err
	}
	evidence, err := stringMember(obj, "evidence", true)
	if err != nil {
		return hookResultEntry{}, err
	}
	arguments, err := rawJSONMember(obj, "arguments", false)
	if err != nil {
		return hookResultEntry{}, err
	}
	errorText, err := optionalNonEmptyString(obj, "error")
	if err != nil {
		return hookResultEntry{}, err
	}
	// The "input_arguments_base64" member is permitted only on a tool-bound
	// hook result: reject its raw membership outright for every other binding
	// before any base64 decoding, whatever the member's value.
	if toolCallID == "" {
		if _, present := obj["input_arguments_base64"]; present {
			return hookResultEntry{}, errors.New(`hook result binding forbids the "input_arguments_base64" member`)
		}
	}
	inputArgumentsBase64, err := optionalStoredBase64(obj, "input_arguments_base64")
	if err != nil {
		return hookResultEntry{}, err
	}
	if toolCallID != "" && requestID == "" {
		if _, present := obj["input_arguments_base64"]; !present {
			return hookResultEntry{}, errors.New(`tool-bound hook result requires the "input_arguments_base64" member`)
		}
	}
	var inputEvidence *RequestEvidence
	if _, present := obj["input_evidence"]; present {
		decoded, err := decodeRequestEvidence(obj["input_evidence"])
		if err != nil {
			return hookResultEntry{}, fmt.Errorf("member %q: %w", "input_evidence", err)
		}
		inputEvidence = &decoded
	}
	var consequence *RequestConsequence
	if _, present := obj["consequence"]; present {
		decoded, err := decodeRequestConsequence(obj["consequence"])
		if err != nil {
			return hookResultEntry{}, fmt.Errorf("member %q: %w", "consequence", err)
		}
		consequence = &decoded
	}
	if sessionID != env.SessionID {
		return hookResultEntry{}, fmt.Errorf("payload session id %q does not agree with the envelope", sessionID)
	}
	if entryID != env.ID {
		return hookResultEntry{}, fmt.Errorf("payload entry id %q does not agree with the envelope", entryID)
	}
	if operationID != env.OperationID {
		return hookResultEntry{}, fmt.Errorf("payload operation id %q does not agree with the envelope", operationID)
	}
	v := hookResultEntry{
		SessionID:            sessionID,
		EntryID:              entryID,
		OperationID:          operationID,
		HookID:               hookID,
		ExtensionID:          extensionID,
		ToolCallID:           toolCallID,
		RequestID:            requestID,
		Status:               hookResultStatus(status),
		Evidence:             ExecutionEvidence(evidence),
		Arguments:            arguments,
		Error:                errorText,
		InputArgumentsBase64: inputArgumentsBase64,
		InputEvidence:        inputEvidence,
		Consequence:          consequence,
	}
	if err := validateHookResultEntry(v); err != nil {
		return hookResultEntry{}, err
	}
	return v, nil
}

// optionalStoredBase64 reads one base64 string member whose presence is
// meaningful and whose value may encode empty bytes; it must be canonical
// standard-alphabet base64.
func optionalStoredBase64(obj map[string]json.RawMessage, key string) (string, error) {
	if _, present := obj[key]; !present {
		return "", nil
	}
	s, err := stringMember(obj, key, true)
	if err != nil {
		return "", err
	}
	if _, err := base64.StdEncoding.Strict().DecodeString(s); err != nil {
		return "", fmt.Errorf("member %q is not canonical base64: %w", key, err)
	}
	return s, nil
}

// validateHookResultEntry enforces the closed hook-result shape: durable
// identities with a required owning Operation, non-empty hook and extension
// identities, exactly one binding (tool call or request), the closed status
// and knowledge enums, and the binding-owned member rules — a success carries
// observed knowledge and its binding consequence (the one normalized object
// for a tool binding, the closed record for a request binding) and no error,
// while every other status carries a non-empty error and no consequence. The
// consumed input member is binding-owned and unconditional: canonical base64
// bytes (empty allowed) for a tool binding, the closed evidence record for a
// request binding.
func validateHookResultEntry(v hookResultEntry) error {
	if err := validateHexID(v.SessionID, "session id"); err != nil {
		return err
	}
	if err := validateHexID(v.EntryID, "entry id"); err != nil {
		return err
	}
	if err := validateOperationIdentity(v.OperationID, "operation id"); err != nil {
		return err
	}
	if v.HookID == "" {
		return errors.New("hook id must be non-empty")
	}
	if v.ExtensionID == "" {
		return errors.New("hook extension id must be non-empty")
	}
	if (v.ToolCallID == "") == (v.RequestID == "") {
		return errors.New("hook result binds exactly one of a tool call or a request")
	}
	if v.ToolCallID != "" {
		if err := validateOperationIdentity(v.ToolCallID, "hook result tool call id"); err != nil {
			return err
		}
		if v.InputEvidence != nil {
			return errors.New("tool-bound hook result carries no evidence input")
		}
		if _, err := base64.StdEncoding.Strict().DecodeString(v.InputArgumentsBase64); err != nil {
			return errors.New("hook result input_arguments_base64 is not canonical base64")
		}
		if v.Consequence != nil {
			return errors.New("tool-bound hook result carries no request consequence")
		}
	} else {
		if err := validateOperationIdentity(v.RequestID, "hook result request id"); err != nil {
			return err
		}
		if v.InputArgumentsBase64 != "" {
			return errors.New("request-bound hook result carries no argument bytes input")
		}
		if v.InputEvidence == nil {
			return errors.New("request-bound hook result requires its consumed input evidence")
		}
		if err := validateRequestEvidence(*v.InputEvidence); err != nil {
			return fmt.Errorf("input_evidence: %w", err)
		}
		if len(v.Arguments) > 0 {
			return errors.New("request-bound hook result carries no arguments")
		}
	}
	switch v.Status {
	case hookSucceeded:
		if !validExecutionEvidence(v.Evidence) || v.Evidence != ExecutionObserved {
			return fmt.Errorf("successful hook result requires %s execution knowledge", ExecutionObserved)
		}
		if v.Error != "" {
			return errors.New("successful hook result must not carry an error")
		}
		if v.ToolCallID != "" {
			if _, err := decodePayloadObject(v.Arguments); err != nil {
				return fmt.Errorf("successful hook result arguments must be one complete JSON object: %v", err)
			}
		} else if v.Consequence == nil {
			return errors.New("successful request-bound hook result requires its consequence")
		} else if err := validateRequestConsequenceShape(*v.Consequence); err != nil {
			return fmt.Errorf("consequence: %w", err)
		}
	case hookFailed, hookInterrupted:
		if !validExecutionEvidence(v.Evidence) {
			return fmt.Errorf("hook result evidence %q is not one of not_started, observed or unknown", v.Evidence)
		}
		if v.Error == "" {
			return fmt.Errorf("%s hook result requires a non-empty error", v.Status)
		}
		if len(v.Arguments) > 0 {
			return fmt.Errorf("%s hook result must not carry arguments", v.Status)
		}
		if v.Consequence != nil {
			return fmt.Errorf("%s hook result must not carry a consequence", v.Status)
		}
	default:
		return fmt.Errorf("hook result status %q is not one of success, error or interrupted", v.Status)
	}
	return nil
}

// encodeSignalEntry renders one signal entry payload directly from the
// validated tagged value; its related-operation identity was validated as
// part of that value.
func encodeSignalEntry(v signalEntry) (json.RawMessage, error) {
	if err := validateSignalEntry(v); err != nil {
		return nil, invalidInput("signal entry: %v", err)
	}
	return json.Marshal(v)
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
	if err := rejectUnknownMembers(obj, "session_id", "entry_id", "operation_id", "signal", "related_operation", "related_member", "content"); err != nil {
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
	relatedOperationObj, err := objectMember(obj, "related_operation", false)
	if err != nil {
		return signalEntry{}, err
	}
	var related *operationRef
	if relatedOperationObj != nil {
		ref, err := decodeOperationRef(relatedOperationObj)
		if err != nil {
			return signalEntry{}, fmt.Errorf("member %q: %w", "related_operation", err)
		}
		related = &ref
	}
	relatedMemberObj, err := objectMember(obj, "related_member", false)
	if err != nil {
		return signalEntry{}, err
	}
	var member *relatedMember
	if relatedMemberObj != nil {
		if err := rejectUnknownMembers(relatedMemberObj, "kind", "id"); err != nil {
			return signalEntry{}, fmt.Errorf("member %q: %w", "related_member", err)
		}
		kind, err := stringMember(relatedMemberObj, "kind", true)
		if err != nil {
			return signalEntry{}, fmt.Errorf("member %q: %w", "related_member", err)
		}
		id, err := stringMember(relatedMemberObj, "id", true)
		if err != nil {
			return signalEntry{}, fmt.Errorf("member %q: %w", "related_member", err)
		}
		member = &relatedMember{Kind: kind, ID: id}
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
		RelatedMember:    member,
		Content:          content,
	}
	if err := validateSignalEntry(v); err != nil {
		return signalEntry{}, err
	}
	return v, nil
}

// validateSignalEntry enforces the closed signal shape: the closed kind, the
// kind's correlation rule, and the contract-fixed content for the fixed
// kinds. A background_completion signal is always operationless and carries
// its related background member instead; its content is bounded by the
// producer, never re-validated here beyond non-emptiness.
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
		if v.RelatedMember != nil {
			return fmt.Errorf("signal kind %s must not carry related_member", v.Signal)
		}
		if v.RelatedOperation == nil {
			return fmt.Errorf("signal kind %s requires related_operation", v.Signal)
		}
		if want := signalContent(v.Signal); v.Content != want {
			return fmt.Errorf("signal content %q is not the fixed content of kind %s", v.Content, v.Signal)
		}
		if err := validateHexID(v.RelatedOperation.SessionID, "related operation session id"); err != nil {
			return err
		}
		return validateOperationIdentity(v.RelatedOperation.OperationID, "related operation id")
	case SignalBackgroundCompletion:
		if v.OperationID != "" {
			return fmt.Errorf("background_completion signal must not carry an operation identity")
		}
		if v.RelatedOperation != nil {
			return fmt.Errorf("background_completion signal must not carry related_operation")
		}
		if v.RelatedMember == nil {
			return fmt.Errorf("background_completion signal requires related_member")
		}
		switch v.RelatedMember.Kind {
		case "child":
			if err := validateHexID(v.RelatedMember.ID, "related member id"); err != nil {
				return err
			}
		case "job":
			if err := validateJobID(v.RelatedMember.ID, "related member id"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("related member kind %q is not one of child or job", v.RelatedMember.Kind)
		}
		if v.Content == "" {
			return errors.New("background_completion signal requires non-empty content")
		}
		return nil
	default:
		return fmt.Errorf("signal kind %q is not one of interruption, model_failure_continuation or background_completion", v.Signal)
	}
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
	var evidenceRaw json.RawMessage
	if v.Evidence != nil {
		raw, err := encodeRequestEvidence(*v.Evidence)
		if err != nil {
			return nil, err
		}
		evidenceRaw = raw
	}
	var factsRaw json.RawMessage
	if len(v.Facts) > 0 {
		raw, err := encodeMaintenanceFacts(v.Facts)
		if err != nil {
			return nil, invalidInput("operation settlement entry: %v", err)
		}
		factsRaw = raw
	}
	wire, err := json.Marshal(struct {
		SessionID         string          `json:"session_id"`
		EntryID           string          `json:"entry_id"`
		OperationID       string          `json:"operation_id"`
		Status            string          `json:"status"`
		Detail            string          `json:"detail,omitempty"`
		Model             json.RawMessage `json:"model,omitempty"`
		Usage             *UsageCount     `json:"usage,omitempty"`
		CompactionUsage   bool            `json:"compaction_usage,omitempty"`
		RequestID         string          `json:"request_id,omitempty"`
		ConsumedRequestID string          `json:"consumed_request_id,omitempty"`
		Evidence          json.RawMessage `json:"evidence,omitempty"`
		Facts             json.RawMessage `json:"facts,omitempty"`
	}{
		SessionID:         v.SessionID,
		EntryID:           v.EntryID,
		OperationID:       v.OperationID,
		Status:            string(v.Status),
		Detail:            v.Detail,
		Model:             modelRaw,
		Usage:             v.Usage,
		CompactionUsage:   v.CompactionUsage,
		RequestID:         v.RequestID,
		ConsumedRequestID: v.ConsumedRequestID,
		Evidence:          evidenceRaw,
		Facts:             factsRaw,
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
	if err := rejectUnknownMembers(obj, "session_id", "entry_id", "operation_id", "status", "detail", "model", "usage", "compaction_usage", "request_id", "consumed_request_id", "evidence", "facts"); err != nil {
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
	compactionUsage, err := boolMember(obj, "compaction_usage", false)
	if err != nil {
		return operationSettlementEntry{}, err
	}
	requestID, err := optionalNonEmptyString(obj, "request_id")
	if err != nil {
		return operationSettlementEntry{}, err
	}
	consumedRequestID, err := optionalNonEmptyString(obj, "consumed_request_id")
	if err != nil {
		return operationSettlementEntry{}, err
	}
	var evidence *RequestEvidence
	if _, present := obj["evidence"]; present {
		decoded, err := decodeRequestEvidence(obj["evidence"])
		if err != nil {
			return operationSettlementEntry{}, fmt.Errorf("member %q: %w", "evidence", err)
		}
		evidence = &decoded
	}
	var facts []maintenanceFact
	if factsRaw, err := member(obj, "facts", false); err != nil {
		return operationSettlementEntry{}, err
	} else if factsRaw != nil {
		decoded, err := decodeMaintenanceFacts(factsRaw)
		if err != nil {
			return operationSettlementEntry{}, fmt.Errorf("member %q: %w", "facts", err)
		}
		facts = decoded
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
		SessionID:         sessionID,
		EntryID:           entryID,
		OperationID:       operationID,
		Status:            OperationState(status),
		Detail:            detail,
		Model:             ref,
		Usage:             usage,
		CompactionUsage:   compactionUsage,
		RequestID:         requestID,
		ConsumedRequestID: consumedRequestID,
		Evidence:          evidence,
		Facts:             facts,
	}
	if err := validateOperationSettlementEntry(v); err != nil {
		return operationSettlementEntry{}, err
	}
	return v, nil
}

// validateOperationSettlementEntry enforces the closed settlement shape: a
// non-empty owning Operation identity, terminal status with the contract
// detail rule (success empty, failure and interruption non-empty), the
// usage-requires-model identity relation (a model identity alone marks a
// no-output terminal model producer whose usage report was absent),
// maintenance facts settling beside any ordinary usage on every terminal kind
// with the maintenance producer witness required beside the maintenance
// provenance marker, and a consumed source identity present only beside
// maintenance evidence provenance.
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
	if v.ConsumedRequestID != "" {
		if err := validateOperationIdentity(v.ConsumedRequestID, "settlement consumed request id"); err != nil {
			return err
		}
		if v.Evidence == nil || !v.CompactionUsage {
			return errors.New("settlement consumed request identity requires maintenance evidence provenance")
		}
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
	if v.Model == nil && v.Usage != nil {
		return errors.New("settlement usage requires its model identity")
	}
	if v.Evidence != nil && v.RequestID == "" {
		return errors.New("settlement evidence requires its request identity")
	}
	if v.Evidence != nil {
		if err := validateRequestEvidence(*v.Evidence); err != nil {
			return err
		}
	}
	if len(v.Facts) > 0 {
		if v.CompactionUsage && v.RequestID == "" {
			return errors.New("settlement maintenance facts require their request identity")
		}
		if err := validateMaintenanceFacts(v.Facts); err != nil {
			return err
		}
	}
	return nil
}

// validateMaintenanceFacts enforces one ordered maintenance fact list: unique
// producer request identities and each fact's closed shape.
func validateMaintenanceFacts(facts []maintenanceFact) error {
	seen := make(map[string]bool, len(facts))
	for i, fact := range facts {
		if err := validateMaintenanceFact(fact); err != nil {
			return fmt.Errorf("facts[%d]: %w", i, err)
		}
		if seen[fact.RequestID] {
			return fmt.Errorf("facts[%d]: maintenance producer %q repeats", i, fact.RequestID)
		}
		seen[fact.RequestID] = true
	}
	return nil
}

// validateMaintenanceFact enforces one maintenance fact: the producer's
// non-empty request identity, a complete producing model identity, and the
// maintenance input rules over the actual tool-free logical request.
func validateMaintenanceFact(fact maintenanceFact) error {
	if err := validateOperationIdentity(fact.RequestID, "maintenance request id"); err != nil {
		return err
	}
	if fact.Model.Provider == "" || fact.Model.Model == "" {
		return fmt.Errorf("model %q must be a complete model identity", fact.Model.String())
	}
	return validateMaintenanceInput(fact.Input)
}

// encodeMaintenanceFacts renders one ordered maintenance fact list.
func encodeMaintenanceFacts(facts []maintenanceFact) (json.RawMessage, error) {
	if err := validateMaintenanceFacts(facts); err != nil {
		return nil, invalidInput("%v", err)
	}
	items := make([]json.RawMessage, 0, len(facts))
	for _, fact := range facts {
		modelRaw, err := encodeModelRef(fact.Model)
		if err != nil {
			return nil, err
		}
		inputRaw, err := encodeMaintenanceInput(fact.Input)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(struct {
			RequestID string          `json:"request_id"`
			Model     json.RawMessage `json:"model"`
			Input     json.RawMessage `json:"input"`
			Usage     *UsageCount     `json:"usage,omitempty"`
		}{
			RequestID: fact.RequestID,
			Model:     modelRaw,
			Input:     inputRaw,
			Usage:     fact.Usage,
		})
		if err != nil {
			return nil, err
		}
		items = append(items, raw)
	}
	return mustMarshal(items), nil
}

// decodeMaintenanceFacts reads one ordered maintenance fact list.
func decodeMaintenanceFacts(raw json.RawMessage) ([]maintenanceFact, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("maintenance facts must be a JSON array: %w", err)
	}
	facts := make([]maintenanceFact, 0, len(items))
	for i, item := range items {
		fact, err := decodeMaintenanceFact(item)
		if err != nil {
			return nil, fmt.Errorf("facts[%d]: %w", i, err)
		}
		facts = append(facts, fact)
	}
	if err := validateMaintenanceFacts(facts); err != nil {
		return nil, err
	}
	return facts, nil
}

// decodeMaintenanceFact reads one maintenance fact with exact keys.
func decodeMaintenanceFact(raw json.RawMessage) (maintenanceFact, error) {
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return maintenanceFact{}, err
	}
	if err := rejectUnknownMembers(obj, "request_id", "model", "input", "usage"); err != nil {
		return maintenanceFact{}, err
	}
	requestID, err := stringMember(obj, "request_id", true)
	if err != nil {
		return maintenanceFact{}, err
	}
	modelObj, err := objectMember(obj, "model", true)
	if err != nil {
		return maintenanceFact{}, err
	}
	source, err := decodeModelRef(modelObj)
	if err != nil {
		return maintenanceFact{}, fmt.Errorf("member %q: %w", "model", err)
	}
	inputRaw, err := member(obj, "input", true)
	if err != nil {
		return maintenanceFact{}, err
	}
	input, err := decodeMaintenanceInput(inputRaw)
	if err != nil {
		return maintenanceFact{}, fmt.Errorf("member %q: %w", "input", err)
	}
	var usage *UsageCount
	if _, present := obj["usage"]; present {
		usageObj, err := objectMember(obj, "usage", true)
		if err != nil {
			return maintenanceFact{}, err
		}
		counts, err := decodeUsageCount(usageObj)
		if err != nil {
			return maintenanceFact{}, fmt.Errorf("member %q: %w", "usage", err)
		}
		usage = &counts
	}
	fact := maintenanceFact{RequestID: requestID, Model: source, Input: input, Usage: usage}
	if err := validateMaintenanceFact(fact); err != nil {
		return maintenanceFact{}, err
	}
	return fact, nil
}

// encodeContextCheckpointEntry renders one context checkpoint entry payload.
func encodeContextCheckpointEntry(v contextCheckpointEntry) (json.RawMessage, error) {
	if err := validateContextCheckpointEntry(v); err != nil {
		return nil, invalidInput("context checkpoint entry: %v", err)
	}
	evidenceRaw, err := encodeRequestEvidence(*v.Evidence)
	if err != nil {
		return nil, err
	}
	factsRaw, err := encodeMaintenanceFacts(v.Facts)
	if err != nil {
		return nil, err
	}
	messages := make([]json.RawMessage, 0, len(v.Messages))
	for _, m := range v.Messages {
		raw, err := encodeMessageValue(m)
		if err != nil {
			return nil, err
		}
		messages = append(messages, raw)
	}
	return json.Marshal(struct {
		SessionID             string          `json:"session_id"`
		EntryID               string          `json:"entry_id"`
		OperationID           string          `json:"operation_id,omitempty"`
		StrategyID            string          `json:"strategy_id"`
		Messages              json.RawMessage `json:"messages"`
		BoundaryEntryID       string          `json:"boundary_entry_id"`
		ConfigurationRevision string          `json:"configuration_revision"`
		Facts                 json.RawMessage `json:"facts"`
		ConsumedRequestID     string          `json:"consumed_request_id,omitempty"`
		Evidence              json.RawMessage `json:"evidence"`
	}{
		SessionID:             v.SessionID,
		EntryID:               v.EntryID,
		OperationID:           v.OperationID,
		StrategyID:            v.StrategyID,
		Messages:              mustMarshal(messages),
		BoundaryEntryID:       v.BoundaryEntryID,
		ConfigurationRevision: v.ConfigurationRevision,
		Facts:                 factsRaw,
		ConsumedRequestID:     v.ConsumedRequestID,
		Evidence:              evidenceRaw,
	})
}

// decodeContextCheckpointEntry reads one context checkpoint entry payload and
// enforces its agreement with the addressed envelope identity.
func decodeContextCheckpointEntry(env Entry) (contextCheckpointEntry, error) {
	if err := decodeEntryEnvelope(env); err != nil {
		return contextCheckpointEntry{}, err
	}
	obj, err := decodePayloadObject(env.Payload)
	if err != nil {
		return contextCheckpointEntry{}, err
	}
	if err := rejectUnknownMembers(obj, "session_id", "entry_id", "operation_id", "strategy_id", "messages", "boundary_entry_id", "configuration_revision", "facts", "consumed_request_id", "evidence"); err != nil {
		return contextCheckpointEntry{}, err
	}
	sessionID, err := stringMember(obj, "session_id", true)
	if err != nil {
		return contextCheckpointEntry{}, err
	}
	entryID, err := stringMember(obj, "entry_id", true)
	if err != nil {
		return contextCheckpointEntry{}, err
	}
	operationID, err := optionalNonEmptyString(obj, "operation_id")
	if err != nil {
		return contextCheckpointEntry{}, err
	}
	strategyID, err := stringMember(obj, "strategy_id", true)
	if err != nil {
		return contextCheckpointEntry{}, err
	}
	messagesRaw, err := arrayMember(obj, "messages", true)
	if err != nil {
		return contextCheckpointEntry{}, err
	}
	var messages []model.Message
	for i, raw := range messagesRaw {
		m, err := decodeMessageValue(raw)
		if err != nil {
			return contextCheckpointEntry{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		messages = append(messages, m)
	}
	boundaryEntryID, err := stringMember(obj, "boundary_entry_id", true)
	if err != nil {
		return contextCheckpointEntry{}, err
	}
	configurationRevision, err := stringMember(obj, "configuration_revision", true)
	if err != nil {
		return contextCheckpointEntry{}, err
	}
	factsRaw, err := member(obj, "facts", true)
	if err != nil {
		return contextCheckpointEntry{}, err
	}
	facts, err := decodeMaintenanceFacts(factsRaw)
	if err != nil {
		return contextCheckpointEntry{}, fmt.Errorf("member %q: %w", "facts", err)
	}
	consumedRequestID, err := optionalNonEmptyString(obj, "consumed_request_id")
	if err != nil {
		return contextCheckpointEntry{}, err
	}
	evidence, err := decodeRequestEvidence(obj["evidence"])
	if err != nil {
		return contextCheckpointEntry{}, fmt.Errorf("member %q: %w", "evidence", err)
	}
	if sessionID != env.SessionID {
		return contextCheckpointEntry{}, fmt.Errorf("payload session id %q does not agree with the envelope", sessionID)
	}
	if entryID != env.ID {
		return contextCheckpointEntry{}, fmt.Errorf("payload entry id %q does not agree with the envelope", entryID)
	}
	if operationID != env.OperationID {
		return contextCheckpointEntry{}, fmt.Errorf("payload operation id %q does not agree with the envelope", operationID)
	}
	v := contextCheckpointEntry{
		SessionID:             sessionID,
		EntryID:               entryID,
		OperationID:           operationID,
		StrategyID:            strategyID,
		Messages:              messages,
		BoundaryEntryID:       boundaryEntryID,
		ConfigurationRevision: configurationRevision,
		Facts:                 facts,
		ConsumedRequestID:     consumedRequestID,
		Evidence:              &evidence,
	}
	if err := validateContextCheckpointEntry(v); err != nil {
		return contextCheckpointEntry{}, err
	}
	return v, nil
}

// validateContextCheckpointEntry enforces the closed context checkpoint shape:
// durable identities, the owning Operation identity when present, a non-empty
// strategy identity, non-empty owned shared-schema messages accepted through
// the shared message rule, a non-empty durable boundary reference, a
// non-empty stable configuration revision, the ordered maintenance facts, and
// no source usage on an operationless independently copied entry.
func validateContextCheckpointEntry(v contextCheckpointEntry) error {
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
	} else {
		for _, fact := range v.Facts {
			if fact.Usage != nil {
				return errors.New("independently copied fork-prefix context checkpoint entry carries no source usage")
			}
		}
	}
	if v.StrategyID == "" {
		return errors.New("strategy_id must be non-empty")
	}
	if len(v.Messages) == 0 {
		return errors.New("checkpoint messages must not be empty")
	}
	// The complete shared message domain applies to the self-contained
	// materialized messages; retained sources may carry real tool exchanges,
	// subject to complete call/result pairing across the set.
	for i, m := range v.Messages {
		if _, err := model.NewMessage(m); err != nil {
			return fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	if err := validateToolPairing(v.Messages); err != nil {
		return err
	}
	if err := validateHexID(v.BoundaryEntryID, "boundary entry id"); err != nil {
		return err
	}
	if v.ConfigurationRevision == "" {
		return errors.New("configuration_revision must be non-empty")
	}
	if err := validateMaintenanceFacts(v.Facts); err != nil {
		return err
	}
	if v.ConsumedRequestID != "" {
		if err := validateOperationIdentity(v.ConsumedRequestID, "checkpoint consumed request id"); err != nil {
			return err
		}
	}
	if v.Evidence == nil {
		return errors.New("context checkpoint entry requires its consumed contribution evidence")
	}
	return validateRequestEvidence(*v.Evidence)
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
		ParentSessionID       string `json:"parent_session_id,omitempty"`
	}{
		SessionID:             v.SessionID,
		Workspace:             v.Workspace,
		CreatedAt:             createdAt,
		SourceSessionID:       v.SourceSessionID,
		SourceBoundaryEntryID: v.SourceBoundaryEntryID,
		ParentSessionID:       v.ParentSessionID,
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
		CheckpointEntryID  string          `json:"checkpoint_entry_id,omitempty"`
		Usage              json.RawMessage `json:"usage"`
		LastActivity       string          `json:"last_activity"`
	}{
		Lifecycle:          string(v.Lifecycle),
		ArchivedAt:         archivedAt,
		CurrentAgentType:   v.CurrentAgentType,
		CurrentOperationID: v.CurrentOperationID,
		CheckpointEntryID:  v.CheckpointEntryID,
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
	if err := rejectUnknownMembers(obj, "session_id", "workspace", "created_at", "source_session_id", "source_boundary_entry_id", "parent_session_id"); err != nil {
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
	parentSessionID, err := optionalNonEmptyString(obj, "parent_session_id")
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
		ParentSessionID:       parentSessionID,
	}
	if err := validateSessionIdentity(v); err != nil {
		return SessionIdentity{}, err
	}
	return v, nil
}

// validateSessionIdentity enforces the closed identity shape: durable
// identities, caller-normalized workspace, the root/fork lineage rule (a root
// omits both source fields; a fork requires both), and the child lineage rule
// (parent_session_id is exclusive with both source fields).
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
	if v.ParentSessionID != "" {
		if v.SourceSessionID != "" || v.SourceBoundaryEntryID != "" {
			return errors.New("parent_session_id is mutually exclusive with source_session_id and source_boundary_entry_id")
		}
		if err := validateHexID(v.ParentSessionID, "parent session id"); err != nil {
			return err
		}
	}
	return nil
}

// decodeSessionState reads the state section with exact keys.
func decodeSessionState(obj map[string]json.RawMessage) (SessionState, error) {
	if err := rejectUnknownMembers(obj, "lifecycle", "archived_at", "current_agent_type", "current_operation_id", "checkpoint_entry_id", "usage", "last_activity"); err != nil {
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
	checkpointEntryID, err := optionalNonEmptyString(obj, "checkpoint_entry_id")
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
		CheckpointEntryID:  checkpointEntryID,
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
// archived_at present exactly when archived, non-empty Agent type, a
// non-empty current Operation identity when present, and a hex-validated
// compaction entry reference when present.
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
	if v.CheckpointEntryID != "" {
		if err := validateHexID(v.CheckpointEntryID, "checkpoint entry id"); err != nil {
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
// section. The message kind renders its admitted entry; the compact kind
// admits no input entry and omits the member.
func encodeOperationAdmission(v OperationAdmission) (json.RawMessage, error) {
	if err := validateOperationAdmission(v); err != nil {
		return nil, invalidInput("operation admission: %v", err)
	}
	var admittedEntry json.RawMessage
	if v.RequestKind == RequestKindMessage {
		rendered, err := encodeEntryRef(v.AdmittedEntry)
		if err != nil {
			return nil, err
		}
		admittedEntry = rendered
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
		AdmittedEntry json.RawMessage `json:"admitted_entry,omitempty"`
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
	var pricingRaw json.RawMessage
	if v.Pricing != nil {
		if pricingRaw, err = encodeModelPricing(*v.Pricing); err != nil {
			return nil, err
		}
	}
	compactModelRaw, err := encodeModelRef(v.Compact.Model)
	if err != nil {
		return nil, err
	}
	var compactPricingRaw json.RawMessage
	if v.Compact.Pricing != nil {
		if compactPricingRaw, err = encodeModelPricing(*v.Compact.Pricing); err != nil {
			return nil, err
		}
	}
	compactRaw, err := json.Marshal(struct {
		Model        json.RawMessage `json:"model"`
		SystemPrompt string          `json:"system_prompt"`
		Pricing      json.RawMessage `json:"pricing,omitempty"`
	}{
		Model:        compactModelRaw,
		SystemPrompt: v.Compact.SystemPrompt,
		Pricing:      compactPricingRaw,
	})
	if err != nil {
		return nil, err
	}
	strategyRaw, err := encodeStrategyCapture(v.Strategy)
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
		ContextWindow         int               `json:"context_window"`
		OutputReserve         int               `json:"output_reserve"`
		SystemPrompt          string            `json:"system_prompt"`
		Tools                 []json.RawMessage `json:"tools"`
		Capabilities          []string          `json:"capabilities,omitempty"`
		Readonly              bool              `json:"readonly"`
		WriteDir              string            `json:"write_dir"`
		Pricing               json.RawMessage   `json:"pricing,omitempty"`
		Compact               json.RawMessage   `json:"compact"`
		Strategy              json.RawMessage   `json:"strategy"`
	}{
		ConfigurationRevision: v.ConfigurationRevision,
		Model:                 modelRaw,
		ContextWindow:         v.ContextWindow,
		OutputReserve:         v.OutputReserve,
		SystemPrompt:          v.SystemPrompt,
		Tools:                 tools,
		Capabilities:          v.Capabilities,
		Readonly:              v.Readonly,
		WriteDir:              v.WriteDir,
		Pricing:               pricingRaw,
		Compact:               compactRaw,
		Strategy:              strategyRaw,
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
	// The message kind requires its present, validated admitted entry; the
	// compact kind admits no input entry and requires the member absent.
	var admittedEntry EntryRef
	switch RequestKind(requestKind) {
	case RequestKindMessage:
		admittedObj, err := objectMember(obj, "admitted_entry", true)
		if err != nil {
			return OperationAdmission{}, err
		}
		admittedEntry, err = decodeEntryRef(admittedObj)
		if err != nil {
			return OperationAdmission{}, fmt.Errorf("member %q: %w", "admitted_entry", err)
		}
	case RequestKindCompact:
		if _, present := obj["admitted_entry"]; present {
			return OperationAdmission{}, fmt.Errorf("member %q must be absent for request kind %q", "admitted_entry", requestKind)
		}
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
// identities, the message or compact request kind, the kind's admitted-entry
// rule, non-empty Agent type, and a complete valid capture.
func validateOperationAdmission(v OperationAdmission) error {
	if err := validateHexID(v.SessionID, "session id"); err != nil {
		return err
	}
	if err := validateOperationIdentity(v.OperationID, "operation id"); err != nil {
		return err
	}
	switch v.RequestKind {
	case RequestKindMessage:
		if err := validateHexID(v.AdmittedEntry.SessionID, "admitted entry session id"); err != nil {
			return err
		}
		if err := validateHexID(v.AdmittedEntry.EntryID, "admitted entry id"); err != nil {
			return err
		}
	case RequestKindCompact:
		if v.AdmittedEntry != (EntryRef{}) {
			return errors.New("compact operation admission carries an admitted entry")
		}
	default:
		return fmt.Errorf("request kind %q is not one of %q or %q", v.RequestKind, RequestKindMessage, RequestKindCompact)
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
// tool names, and preserved tool and capability order. The permission
// capability members are required, including their false and empty values.
func decodeExecutionCapture(obj map[string]json.RawMessage) (ExecutionCapture, error) {
	if err := rejectUnknownMembers(obj, "configuration_revision", "model", "context_window", "output_reserve", "system_prompt", "tools", "capabilities", "readonly", "write_dir", "pricing", "compact", "strategy"); err != nil {
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
	contextWindow, err := int64Member(obj, "context_window", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	outputReserve, err := int64Member(obj, "output_reserve", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	systemPrompt, err := stringMember(obj, "system_prompt", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	toolsRaw, err := arrayMember(obj, "tools", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	readonly, err := boolMember(obj, "readonly", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	writeDir, err := stringMember(obj, "write_dir", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	pricing, err := decodeOptionalModelPricing(obj, "pricing")
	if err != nil {
		return ExecutionCapture{}, err
	}
	compactObj, err := objectMember(obj, "compact", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	compact, err := decodeCompactCapture(compactObj)
	if err != nil {
		return ExecutionCapture{}, fmt.Errorf("member %q: %w", "compact", err)
	}
	strategyObj, err := objectMember(obj, "strategy", true)
	if err != nil {
		return ExecutionCapture{}, err
	}
	strategy, err := decodeStrategyCapture(strategyObj)
	if err != nil {
		return ExecutionCapture{}, fmt.Errorf("member %q: %w", "strategy", err)
	}
	v := ExecutionCapture{
		ConfigurationRevision: revision,
		Model:                 ref,
		ContextWindow:         int(contextWindow),
		OutputReserve:         int(outputReserve),
		SystemPrompt:          systemPrompt,
		Readonly:              readonly,
		WriteDir:              writeDir,
		Pricing:               pricing,
		Compact:               compact,
		Strategy:              strategy,
	}
	for i, raw := range toolsRaw {
		tool, err := decodeToolDefinition(raw)
		if err != nil {
			return ExecutionCapture{}, fmt.Errorf("tools[%d]: %w", i, err)
		}
		v.Tools = append(v.Tools, tool)
	}
	capabilitiesRaw, err := arrayMember(obj, "capabilities", false)
	if err != nil {
		return ExecutionCapture{}, err
	}
	for i, raw := range capabilitiesRaw {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			return ExecutionCapture{}, fmt.Errorf("capabilities[%d]: %w", i, err)
		}
		v.Capabilities = append(v.Capabilities, name)
	}
	if len(v.Capabilities) == 0 { // omission and the empty array both mean none
		v.Capabilities = nil
	}
	if err := validateExecutionCapture(v); err != nil {
		return ExecutionCapture{}, err
	}
	return v, nil
}

// decodeCompactCapture reads the durable compaction configuration's exact
// keys: the durable two-field model identity, the system prompt, and the
// optional captured pricing. Their values are validated by the enclosing
// capture validation.
func decodeCompactCapture(obj map[string]json.RawMessage) (CompactCapture, error) {
	if err := rejectUnknownMembers(obj, "model", "system_prompt", "pricing"); err != nil {
		return CompactCapture{}, err
	}
	modelObj, err := objectMember(obj, "model", true)
	if err != nil {
		return CompactCapture{}, err
	}
	ref, err := decodeModelRef(modelObj)
	if err != nil {
		return CompactCapture{}, fmt.Errorf("member %q: %w", "model", err)
	}
	systemPrompt, err := stringMember(obj, "system_prompt", true)
	if err != nil {
		return CompactCapture{}, err
	}
	pricing, err := decodeOptionalModelPricing(obj, "pricing")
	if err != nil {
		return CompactCapture{}, err
	}
	v := CompactCapture{Model: ref, SystemPrompt: systemPrompt, Pricing: pricing}
	return v, nil
}

// encodeStrategyCapture renders the resolved selected strategy input.
func encodeStrategyCapture(v StrategyCapture) (json.RawMessage, error) {
	if err := validateStrategyCapture(v); err != nil {
		return nil, invalidInput("strategy capture: %v", err)
	}
	return json.Marshal(struct {
		ID                    string          `json:"id"`
		Settings              json.RawMessage `json:"settings,omitempty"`
		RequiresContextWindow bool            `json:"requires_context_window"`
	}{ID: v.ID, Settings: v.Settings, RequiresContextWindow: v.RequiresContextWindow})
}

// decodeStrategyCapture reads the resolved selected strategy input with
// exact keys. Its closed relation to the captured window is checked by the
// enclosing capture validation without loading any strategy code.
func decodeStrategyCapture(obj map[string]json.RawMessage) (StrategyCapture, error) {
	if err := rejectUnknownMembers(obj, "id", "settings", "requires_context_window"); err != nil {
		return StrategyCapture{}, err
	}
	id, err := stringMember(obj, "id", true)
	if err != nil {
		return StrategyCapture{}, err
	}
	requires, err := boolMember(obj, "requires_context_window", true)
	if err != nil {
		return StrategyCapture{}, err
	}
	v := StrategyCapture{ID: id, RequiresContextWindow: requires}
	// Settings are arbitrary owned raw JSON read by presence under the same
	// syntax-only acceptance as the validating side: an accepted value —
	// including null and large numbers — keeps its original bytes, and
	// absence stays absent.
	if settings, present := obj["settings"]; present {
		if !json.Valid(settings) {
			return StrategyCapture{}, errors.New("settings must be valid JSON")
		}
		v.Settings = model.CloneRaw(settings)
	}
	return v, nil
}

// validateStrategyCapture enforces the selected strategy input's closed
// shape: a non-empty stable binding identity and well-formed non-secret
// settings.
func validateStrategyCapture(v StrategyCapture) error {
	if v.ID == "" {
		return errors.New("id must be non-empty")
	}
	if len(v.Settings) > 0 && !json.Valid(v.Settings) {
		return errors.New("settings must be valid JSON")
	}
	return nil
}

// validateExecutionCapture enforces the closed capture shape: non-empty
// stable revision, complete model identity, the closed context-window
// requirement relation (a required known window must be positive; zero means
// unknown and is permitted only when not required), positive output reserve,
// a complete valid compaction configuration and strategy input, unique tool
// names in preserved order through the shared request validator, and
// non-empty unique capability IDs in preserved order. Capability names are
// checked for shape only: no plugin definition is consulted, so a historical
// capture stays readable when plugins change.
func validateExecutionCapture(v ExecutionCapture) error {
	if v.ConfigurationRevision == "" {
		return errors.New("configuration_revision must be non-empty")
	}
	if v.Model.Provider == "" || v.Model.Model == "" {
		return fmt.Errorf("model %q must be a complete model identity", v.Model.String())
	}
	if v.Strategy.RequiresContextWindow && v.ContextWindow <= 0 {
		return fmt.Errorf("context_window %d must be positive for the selected strategy", v.ContextWindow)
	}
	if v.ContextWindow < 0 {
		return fmt.Errorf("context_window %d must not be negative", v.ContextWindow)
	}
	if v.OutputReserve <= 0 {
		return fmt.Errorf("output_reserve %d must be positive", v.OutputReserve)
	}
	if err := validateCompactCapture(v.Compact); err != nil {
		return fmt.Errorf("compact: %w", err)
	}
	if err := validateOptionalModelPricing(v.Pricing); err != nil {
		return fmt.Errorf("pricing: %w", err)
	}
	if err := validateStrategyCapture(v.Strategy); err != nil {
		return fmt.Errorf("strategy: %w", err)
	}
	if err := model.ValidateRequest(model.Request{Tools: v.Tools}); err != nil {
		return err
	}
	seen := make(map[string]bool, len(v.Capabilities))
	for i, capability := range v.Capabilities {
		if capability == "" {
			return fmt.Errorf("capabilities[%d]: name must be non-empty", i)
		}
		if seen[capability] {
			return fmt.Errorf("capabilities[%d]: duplicate name %q", i, capability)
		}
		seen[capability] = true
	}
	return nil
}

// validateCompactCapture enforces the compaction configuration's closed
// shape: a complete model identity, a non-empty compact system prompt, and
// present pricing that carries only finite rates.
func validateCompactCapture(v CompactCapture) error {
	if v.Model.Provider == "" || v.Model.Model == "" {
		return fmt.Errorf("model %q must be a complete model identity", v.Model.String())
	}
	if v.SystemPrompt == "" {
		return errors.New("system_prompt must be non-empty")
	}
	return validateOptionalModelPricing(v.Pricing)
}

// encodeModelPricing validates and renders one captured pricing value: each
// present rate is a JSON number, absence encodes by omission, and a present
// zero stays present. Validation runs first, so a non-finite Go float rate
// can never produce invalid JSON.
func encodeModelPricing(v ModelPricing) (json.RawMessage, error) {
	if err := validateModelPricing(v); err != nil {
		return nil, invalidInput("model pricing: %v", err)
	}
	return json.Marshal(v)
}

// decodeOptionalModelPricing reads one optional pricing object member: an
// omitted member means unknown pricing, and a present object decodes its
// nullable rates — an omitted rate stays absent, a present zero stays a
// present zero, and null is rejected like every other codec null.
func decodeOptionalModelPricing(obj map[string]json.RawMessage, key string) (*ModelPricing, error) {
	if _, present := obj[key]; !present {
		return nil, nil
	}
	pricingObj, err := objectMember(obj, key, true)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownMembers(pricingObj, "input", "output", "cache_read"); err != nil {
		return nil, fmt.Errorf("member %q: %w", key, err)
	}
	v := ModelPricing{}
	for _, field := range []struct {
		key  string
		slot **float64
	}{
		{"input", &v.Input},
		{"output", &v.Output},
		{"cache_read", &v.CacheRead},
	} {
		raw, err := member(pricingObj, field.key, false)
		if err != nil {
			return nil, fmt.Errorf("member %q: %w", key, err)
		}
		if raw == nil {
			continue
		}
		var rate float64
		if err := json.Unmarshal(raw, &rate); err != nil {
			return nil, fmt.Errorf("member %q: rate %q must be a JSON number: %w", key, field.key, err)
		}
		value := rate
		*field.slot = &value
	}
	if err := validateModelPricing(v); err != nil {
		return nil, fmt.Errorf("member %q: %w", key, err)
	}
	return &v, nil
}

// validateOptionalModelPricing reports a present pricing value's rate rules;
// a nil value means unknown pricing and is always valid.
func validateOptionalModelPricing(v *ModelPricing) error {
	if v == nil {
		return nil
	}
	return validateModelPricing(*v)
}

// validateModelPricing enforces the closed rate rules: every present rate is
// a finite float64. Absent rates mean unknown, and no nonnegativity policy
// is imposed beyond finiteness.
func validateModelPricing(v ModelPricing) error {
	for _, field := range []struct {
		key  string
		rate *float64
	}{
		{"input", v.Input},
		{"output", v.Output},
		{"cache_read", v.CacheRead},
	} {
		if field.rate != nil && (math.IsNaN(*field.rate) || math.IsInf(*field.rate, 0)) {
			return fmt.Errorf("rate %q must be finite", field.key)
		}
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
	usage, err := encodeUsageTotalsWire(v.Usage)
	if err != nil {
		return nil, err
	}
	pending := v.PendingToolCalls
	if pending == nil { // required non-null array: absence encodes as the empty array
		pending = []PendingToolCall{}
	}
	var activeEffect json.RawMessage
	if v.ActiveEffect != nil {
		raw, err := encodeActiveEffect(*v.ActiveEffect)
		if err != nil {
			return nil, err
		}
		activeEffect = raw
	}
	var runningRequest json.RawMessage
	if v.Request != nil {
		raw, err := encodeRunningRequest(*v.Request)
		if err != nil {
			return nil, err
		}
		runningRequest = raw
	}
	wire, err := json.Marshal(struct {
		Status                string             `json:"status"`
		StartedAt             string             `json:"started_at"`
		SettledAt             *string            `json:"settled_at,omitempty"`
		ActiveEffect          json.RawMessage    `json:"active_effect,omitempty"`
		RunningRequest        json.RawMessage    `json:"running_request,omitempty"`
		PendingToolCalls      []PendingToolCall  `json:"pending_tool_calls"`
		Usage                 json.RawMessage    `json:"usage"`
		Terminal              *OperationTerminal `json:"terminal,omitempty"`
		UnreportedMaintenance bool               `json:"unreported_maintenance,omitempty"`
	}{
		Status:                string(v.Status),
		StartedAt:             startedAt,
		SettledAt:             settledAt,
		ActiveEffect:          activeEffect,
		RunningRequest:        runningRequest,
		PendingToolCalls:      pending,
		Usage:                 usage,
		Terminal:              v.Terminal,
		UnreportedMaintenance: v.UnreportedMaintenance,
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// decodeOperationState reads the state section with exact keys.
func decodeOperationState(obj map[string]json.RawMessage) (OperationCurrentState, error) {
	if err := rejectUnknownMembers(obj, "status", "started_at", "settled_at", "active_effect", "running_request", "pending_tool_calls", "usage", "terminal", "unreported_maintenance"); err != nil {
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
	unreported, err := boolMember(obj, "unreported_maintenance", false)
	if err != nil {
		return OperationCurrentState{}, fmt.Errorf("member %q: %w", "unreported_maintenance", err)
	}
	v := OperationCurrentState{
		Status:                OperationState(status),
		StartedAt:             started,
		Usage:                 usage,
		UnreportedMaintenance: unreported,
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
	if _, present := obj["running_request"]; present {
		stateObj, err := objectMember(obj, "running_request", true)
		if err != nil {
			return OperationCurrentState{}, err
		}
		state, err := decodeRunningRequest(stateObj)
		if err != nil {
			return OperationCurrentState{}, fmt.Errorf("member %q: %w", "running_request", err)
		}
		v.Request = &state
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

// encodeActiveEffect renders one active effect through the closed member
// codecs. The per-kind requirements are enforced by validateActiveEffect at
// both codec entries.
func encodeActiveEffect(v ActiveEffect) (json.RawMessage, error) {
	if err := validateActiveEffect(v); err != nil {
		return nil, invalidInput("active effect: %v", err)
	}
	obj := map[string]json.RawMessage{
		"kind":            mustMarshal(string(v.Kind)),
		"result_entry_id": mustMarshal(v.ResultEntryID),
	}
	if v.ToolCallID != "" {
		obj["tool_call_id"] = mustMarshal(v.ToolCallID)
	}
	if v.HookID != "" {
		obj["hook_id"] = mustMarshal(v.HookID)
	}
	if v.RequestID != "" {
		obj["request_id"] = mustMarshal(v.RequestID)
	}
	if v.Model != nil {
		raw, err := encodeModelRef(*v.Model)
		if err != nil {
			return nil, err
		}
		obj["model"] = raw
	}
	if v.Purpose != "" {
		obj["purpose"] = mustMarshal(string(v.Purpose))
	}
	if v.Evidence != nil {
		raw, err := encodeRequestEvidence(*v.Evidence)
		if err != nil {
			return nil, err
		}
		obj["evidence"] = raw
	}
	if v.ExtensionID != "" {
		obj["hook_extension_id"] = mustMarshal(v.ExtensionID)
	}
	if v.InputArgumentsBase64 != "" {
		obj["input_arguments_base64"] = mustMarshal(v.InputArgumentsBase64)
	} else if v.Kind == EffectHook && v.HookID != "" && v.ToolCallID != "" {
		// An argument hook's consumed bytes are a binding-required member even
		// when the consumed selection is empty.
		obj["input_arguments_base64"] = mustMarshal("")
	}
	if len(v.FinalArguments) > 0 {
		obj["final_arguments"] = model.CloneRaw(v.FinalArguments)
	}
	if len(v.ContributionIDs) > 0 {
		raw, err := encodeHookIdentities(v.ContributionIDs)
		if err != nil {
			return nil, err
		}
		obj["hook_ids"] = raw
	}
	return json.Marshal(obj)
}

// decodeActiveEffect reads one active effect with exact keys.
func decodeActiveEffect(obj map[string]json.RawMessage) (ActiveEffect, error) {
	if err := rejectUnknownMembers(obj, "kind", "result_entry_id", "tool_call_id", "hook_id", "request_id", "model", "purpose", "evidence", "hook_extension_id", "input_arguments_base64", "final_arguments", "hook_ids"); err != nil {
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
	hookID, err := optionalNonEmptyString(obj, "hook_id")
	if err != nil {
		return ActiveEffect{}, err
	}
	requestID, err := optionalNonEmptyString(obj, "request_id")
	if err != nil {
		return ActiveEffect{}, err
	}
	var modelRef *model.ModelRef
	if _, present := obj["model"]; present {
		modelObj, err := objectMember(obj, "model", true)
		if err != nil {
			return ActiveEffect{}, err
		}
		decoded, err := decodeModelRef(modelObj)
		if err != nil {
			return ActiveEffect{}, fmt.Errorf("member %q: %w", "model", err)
		}
		modelRef = &decoded
	}
	purpose, err := optionalNonEmptyString(obj, "purpose")
	if err != nil {
		return ActiveEffect{}, err
	}
	var evidence *RequestEvidence
	if _, present := obj["evidence"]; present {
		decoded, err := decodeRequestEvidence(obj["evidence"])
		if err != nil {
			return ActiveEffect{}, fmt.Errorf("member %q: %w", "evidence", err)
		}
		evidence = &decoded
	}
	extensionID, err := optionalNonEmptyString(obj, "hook_extension_id")
	if err != nil {
		return ActiveEffect{}, err
	}
	// The "input_arguments_base64" member is permitted only on a tool-bound
	// hook effect intent: model, tool, and request-bound hook effects reject
	// its raw membership outright before any base64 decoding, whatever the
	// member's value.
	if EffectKind(kind) != EffectHook || toolCallID == "" {
		if _, present := obj["input_arguments_base64"]; present {
			return ActiveEffect{}, errors.New(`active effect binding forbids the "input_arguments_base64" member`)
		}
	}
	inputArgumentsBase64, err := optionalStoredBase64(obj, "input_arguments_base64")
	if err != nil {
		return ActiveEffect{}, err
	}
	if EffectKind(kind) == EffectHook && hookID != "" && toolCallID != "" && requestID == "" {
		if _, present := obj["input_arguments_base64"]; !present {
			return ActiveEffect{}, errors.New(`tool-bound hook active effect requires the "input_arguments_base64" member`)
		}
	}
	finalArguments, err := rawJSONMember(obj, "final_arguments", false)
	if err != nil {
		return ActiveEffect{}, err
	}
	contributionIDs, err := decodeHookIdentities(obj, "hook_ids")
	if err != nil {
		return ActiveEffect{}, err
	}
	v := ActiveEffect{
		Kind:                 EffectKind(kind),
		ResultEntryID:        resultEntryID,
		ToolCallID:           toolCallID,
		HookID:               hookID,
		RequestID:            requestID,
		Model:                modelRef,
		Purpose:              InvocationPurpose(purpose),
		Evidence:             evidence,
		ExtensionID:          extensionID,
		InputArgumentsBase64: inputArgumentsBase64,
		FinalArguments:       finalArguments,
		ContributionIDs:      contributionIDs,
	}
	if err := validateActiveEffect(v); err != nil {
		return ActiveEffect{}, err
	}
	return v, nil
}

// validateActiveEffect enforces the per-kind closed effect shape. A model
// effect carries its request identity, actual model, and closed purpose; a
// conversation model effect also requires its effective input evidence and a
// maintenance effect forbids it. A tool effect requires its consumed
// normalized selection as one complete JSON object with optional contributing
// hook provenance. A hook effect carries its extension identity and exactly
// one binding: a tool-bound hook its opaque consumed bytes (empty allowed), a
// request-bound hook its consumed evidence record. Cross-kind identity and
// input members are forbidden for the other kinds.
func validateActiveEffect(v ActiveEffect) error {
	if err := validateHexID(v.ResultEntryID, "active effect result entry id"); err != nil {
		return err
	}
	switch v.Kind {
	case EffectModel:
		if v.ToolCallID != "" || v.HookID != "" || v.ExtensionID != "" {
			return errors.New("model active effect carries no call, hook, or extension identity")
		}
		if v.InputArgumentsBase64 != "" || len(v.FinalArguments) > 0 || len(v.ContributionIDs) > 0 {
			return errors.New("model active effect carries no input or provenance members")
		}
		if v.RequestID == "" {
			return errors.New("model active effect requires its request identity")
		}
		if v.Model == nil {
			return errors.New("model active effect requires its actual model")
		}
		if v.Model.Provider == "" || v.Model.Model == "" {
			return errors.New("model active effect carries a complete model identity")
		}
		switch v.Purpose {
		case InvocationPurposeConversation:
			if v.Evidence == nil {
				return errors.New("conversation model active effect requires its effective input evidence")
			}
		case InvocationPurposeMaintenance:
			if v.Evidence != nil {
				return errors.New("maintenance model active effect carries no conversation evidence")
			}
		default:
			return fmt.Errorf("model active effect purpose %q is not one of conversation or maintenance", v.Purpose)
		}
		if v.Evidence != nil {
			if err := validateRequestEvidence(*v.Evidence); err != nil {
				return fmt.Errorf("evidence: %w", err)
			}
		}
	case EffectTool:
		if v.HookID != "" || v.ExtensionID != "" {
			return errors.New("tool active effect carries no hook identity")
		}
		if v.RequestID != "" || v.Purpose != "" || v.Model != nil || v.Evidence != nil {
			return errors.New("tool active effect carries no request identity or evidence")
		}
		if v.InputArgumentsBase64 != "" {
			return errors.New("tool active effect carries no consumed hook bytes")
		}
		if v.ToolCallID == "" {
			return errors.New("tool active effect requires its tool call id")
		}
		if _, err := decodePayloadObject(v.FinalArguments); err != nil {
			return errors.New("tool active effect requires its consumed normalized selection as one complete JSON object")
		}
		if err := validateHookIdentities(v.ContributionIDs); err != nil {
			return err
		}
	case EffectHook:
		if v.RequestID != "" && v.ToolCallID != "" {
			return errors.New("hook active effect binds exactly one of a request or a tool call")
		}
		if v.RequestID == "" && v.ToolCallID == "" {
			return errors.New("hook active effect requires its request or tool call binding")
		}
		if v.HookID == "" {
			return errors.New("hook active effect requires its hook id")
		}
		if v.ExtensionID == "" {
			return errors.New("hook active effect requires its extension identity")
		}
		if v.Model != nil || v.Purpose != "" {
			return errors.New("hook active effect carries no model identity")
		}
		if v.RequestID != "" {
			if v.Evidence == nil {
				return errors.New("request-bound hook active effect requires its consumed input evidence")
			}
			if err := validateRequestEvidence(*v.Evidence); err != nil {
				return fmt.Errorf("evidence: %w", err)
			}
			if v.InputArgumentsBase64 != "" || len(v.FinalArguments) > 0 || len(v.ContributionIDs) > 0 {
				return errors.New("request-bound hook active effect carries no argument members")
			}
		} else {
			if v.Evidence != nil {
				return errors.New("tool-bound hook active effect carries no evidence input")
			}
			if _, err := base64.StdEncoding.Strict().DecodeString(v.InputArgumentsBase64); err != nil {
				return errors.New("hook active effect input_arguments_base64 is not canonical base64")
			}
			if len(v.FinalArguments) > 0 || len(v.ContributionIDs) > 0 {
				return errors.New("hook active effect carries no tool selection members")
			}
		}
	default:
		return fmt.Errorf("active effect kind %q is not one of model, tool or hook", v.Kind)
	}
	return nil
}

// encodeRunningRequest renders one suspended conversation request state.
func encodeRunningRequest(v RunningRequest) (json.RawMessage, error) {
	if err := validateRunningRequest(v); err != nil {
		return nil, invalidInput("running request: %v", err)
	}
	return json.Marshal(v)
}

// decodeRunningRequest reads one suspended conversation request state with
// exact keys.
func decodeRunningRequest(obj map[string]json.RawMessage) (RunningRequest, error) {
	if err := rejectUnknownMembers(obj, "request_id", "checkpoint_entry_id", "boundary_entry_id", "hook_results"); err != nil {
		return RunningRequest{}, err
	}
	requestID, err := stringMember(obj, "request_id", true)
	if err != nil {
		return RunningRequest{}, err
	}
	checkpointEntryID, err := optionalNonEmptyString(obj, "checkpoint_entry_id")
	if err != nil {
		return RunningRequest{}, err
	}
	boundaryEntryID, err := stringMember(obj, "boundary_entry_id", true)
	if err != nil {
		return RunningRequest{}, err
	}
	var hookResults []EntryRef
	refsRaw, err := arrayMember(obj, "hook_results", false)
	if err != nil {
		return RunningRequest{}, err
	}
	for i, raw := range refsRaw {
		refObj, err := decodePayloadObject(raw)
		if err != nil {
			return RunningRequest{}, fmt.Errorf("hook_results[%d]: %w", i, err)
		}
		ref, err := decodeEntryRef(refObj)
		if err != nil {
			return RunningRequest{}, fmt.Errorf("hook_results[%d]: %w", i, err)
		}
		hookResults = append(hookResults, ref)
	}
	v := RunningRequest{
		RequestID:         requestID,
		CheckpointEntryID: checkpointEntryID,
		BoundaryEntryID:   boundaryEntryID,
		HookResults:       hookResults,
	}
	if err := validateRunningRequest(v); err != nil {
		return RunningRequest{}, err
	}
	return v, nil
}

// validateRunningRequest enforces the closed running-request shape: the bound
// request identity, the required canonical boundary reference, an optional
// checkpoint reference, and committed request-hook result references.
func validateRunningRequest(v RunningRequest) error {
	if err := validateOperationIdentity(v.RequestID, "running request id"); err != nil {
		return err
	}
	if err := validateHexID(v.BoundaryEntryID, "running request boundary entry id"); err != nil {
		return err
	}
	if v.CheckpointEntryID != "" {
		if err := validateHexID(v.CheckpointEntryID, "running request checkpoint entry id"); err != nil {
			return err
		}
	}
	seen := make(map[EntryRef]bool, len(v.HookResults))
	for i, ref := range v.HookResults {
		if err := validateHexID(ref.SessionID, "running request hook result session id"); err != nil {
			return fmt.Errorf("hook_results[%d]: %w", i, err)
		}
		if err := validateHexID(ref.EntryID, "running request hook result entry id"); err != nil {
			return fmt.Errorf("hook_results[%d]: %w", i, err)
		}
		if seen[ref] {
			return fmt.Errorf("hook_results[%d]: reference %s is not unique", i, ref.EntryID)
		}
		seen[ref] = true
	}
	return nil
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
		if err := validateActiveEffect(*v.ActiveEffect); err != nil {
			return err
		}
		switch v.ActiveEffect.Kind {
		case EffectTool:
			if len(v.PendingToolCalls) == 0 || v.ActiveEffect.ToolCallID != v.PendingToolCalls[0].CallID {
				return errors.New("tool active effect must address the matching first pending call")
			}
			if v.ActiveEffect.ResultEntryID != v.PendingToolCalls[0].ResultEntryID {
				return errors.New("tool active effect must reserve the first pending call's result identity")
			}
		case EffectHook:
			if v.ActiveEffect.ToolCallID != "" {
				if len(v.PendingToolCalls) == 0 || v.ActiveEffect.ToolCallID != v.PendingToolCalls[0].CallID {
					return errors.New("tool-bound hook active effect must address the matching first pending call")
				}
			}
		}
		// One frozen request association rule covers every request-bound
		// conversation intent: a conversation model effect and a request-bound
		// hook effect each require the published running request with the same
		// request identity and identical frozen canonical references. A
		// maintenance model effect is the explicit exception: its producer
		// identity may differ from the suspended conversation state, and it
		// never forces them equal.
		if v.ActiveEffect.Kind == EffectModel && v.ActiveEffect.Purpose == InvocationPurposeConversation ||
			v.ActiveEffect.Kind == EffectHook && v.ActiveEffect.RequestID != "" {
			if v.Request == nil {
				return errors.New("request-bound active effect requires the published running request state")
			}
			if v.Request.RequestID != v.ActiveEffect.RequestID {
				return errors.New("request-bound active effect carries a request identity foreign to the published running request")
			}
			if v.Request.CheckpointEntryID != v.ActiveEffect.Evidence.CheckpointEntryID ||
				v.Request.BoundaryEntryID != v.ActiveEffect.Evidence.BoundaryEntryID {
				return errors.New("request-bound active effect references diverge from the published running request")
			}
		}
	}
	if v.Status != OperationRunning && v.Request != nil {
		return errors.New("terminal operation forbids the running request state")
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

// encodeMessageValue renders one complete permitted model.Message value with
// exactly the role-owned durable members: role, the zero source omitted, the
// content parts through the shared part codec, refusal, name, raw extras, the
// assistant tool calls with their raw arguments persisted as canonical base64
// so malformed JSON and arbitrary bytes round-trip exactly, and the tool
// result call id. Domain-specific restrictions — the tool-free contribution,
// maintenance-input and authored-context rules — stay at their accepting
// owners; this shared codec carries the complete materialized message domain.
func encodeMessageValue(m model.Message) (json.RawMessage, error) {
	if _, err := model.NewMessage(m); err != nil {
		return nil, invalidInput("message: %v", err)
	}
	obj := map[string]json.RawMessage{}
	var err error
	if obj["role"], err = json.Marshal(string(m.Role)); err != nil {
		return nil, err
	}
	if !m.Source.IsZero() {
		source, err := encodeModelRef(m.Source)
		if err != nil {
			return nil, err
		}
		obj["source"] = source
	}
	if len(m.Content) > 0 {
		parts := make([]json.RawMessage, 0, len(m.Content))
		for _, part := range m.Content {
			raw, err := encodeContentPart(part)
			if err != nil {
				return nil, err
			}
			parts = append(parts, raw)
		}
		obj["content"] = mustMarshal(parts)
	}
	if m.Refusal != "" {
		obj["refusal"] = mustMarshal(m.Refusal)
	}
	if m.Name != "" {
		obj["name"] = mustMarshal(m.Name)
	}
	if len(m.Extra) > 0 {
		raw, err := marshalExtra(m.Extra)
		if err != nil {
			return nil, err
		}
		obj["extra"] = raw
	}
	if len(m.ToolCalls) > 0 {
		calls := make([]json.RawMessage, 0, len(m.ToolCalls))
		for _, call := range m.ToolCalls {
			raw, err := encodeMessageToolCall(call)
			if err != nil {
				return nil, err
			}
			calls = append(calls, raw)
		}
		obj["tool_calls"] = mustMarshal(calls)
	}
	if m.ToolCallID != "" {
		obj["tool_call_id"] = mustMarshal(m.ToolCallID)
	}
	return json.Marshal(obj)
}

// encodeMessageToolCall renders one message-carried tool call with its raw
// argument bytes persisted as the base64 standard-alphabet string.
func encodeMessageToolCall(call model.ToolCall) (json.RawMessage, error) {
	if err := model.ValidateToolCall(call); err != nil {
		return nil, invalidInput("message tool call: %v", err)
	}
	obj := map[string]json.RawMessage{
		"id":               mustMarshal(call.ID),
		"name":             mustMarshal(call.Name),
		"arguments_base64": mustMarshal(base64.StdEncoding.EncodeToString(call.Arguments)),
	}
	if len(call.Extra) > 0 {
		raw, err := marshalExtra(call.Extra)
		if err != nil {
			return nil, err
		}
		obj["extra"] = raw
	}
	return json.Marshal(obj)
}

// decodeMessageToolCall reads one message-carried tool call with exact keys,
// restoring the raw argument bytes from their base64 representation.
func decodeMessageToolCall(raw json.RawMessage) (model.ToolCall, error) {
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return model.ToolCall{}, fmt.Errorf("message tool call: %w", err)
	}
	if err := rejectUnknownMembers(obj, "id", "name", "arguments_base64", "extra"); err != nil {
		return model.ToolCall{}, err
	}
	id, err := stringMember(obj, "id", true)
	if err != nil {
		return model.ToolCall{}, err
	}
	name, err := stringMember(obj, "name", true)
	if err != nil {
		return model.ToolCall{}, err
	}
	encoded, err := stringMember(obj, "arguments_base64", true)
	if err != nil {
		return model.ToolCall{}, err
	}
	arguments, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return model.ToolCall{}, fmt.Errorf("message tool call arguments_base64 is not canonical base64: %w", err)
	}
	call := model.ToolCall{ID: id, Name: name, Arguments: arguments}
	if call.Extra, err = extraMember(obj, "extra", false); err != nil {
		return model.ToolCall{}, err
	}
	owned, err := model.NewToolCall(call)
	if err != nil {
		return model.ToolCall{}, err
	}
	return owned, nil
}

// decodeMessageValue reads one message value with exact keys and
// reconstructs it through the shared message constructor, which re-enforces
// the closed role set, per-role source rules, and deep ownership. A null name
// is rejected; an empty string or absence is the unnamed zero value.
func decodeMessageValue(raw json.RawMessage) (model.Message, error) {
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return model.Message{}, fmt.Errorf("message: %w", err)
	}
	if err := rejectUnknownMembers(obj, "role", "source", "content", "refusal", "name", "extra", "tool_calls", "tool_call_id"); err != nil {
		return model.Message{}, err
	}
	role, err := stringMember(obj, "role", true)
	if err != nil {
		return model.Message{}, err
	}
	var source model.ModelRef
	if _, present := obj["source"]; present {
		sourceObj, err := objectMember(obj, "source", true)
		if err != nil {
			return model.Message{}, err
		}
		if source, err = decodeModelRef(sourceObj); err != nil {
			return model.Message{}, fmt.Errorf("member %q: %w", "source", err)
		}
	}
	contentRaw, err := arrayMember(obj, "content", false)
	if err != nil {
		return model.Message{}, err
	}
	var m model.Message
	m.Role = model.Role(role)
	m.Source = source
	for i, raw := range contentRaw {
		part, err := decodeContentPart(raw)
		if err != nil {
			return model.Message{}, fmt.Errorf("content[%d]: %w", i, err)
		}
		m.Content = append(m.Content, part)
	}
	if m.Refusal, err = optionalNonEmptyString(obj, "refusal"); err != nil {
		return model.Message{}, err
	}
	if m.Name, err = stringMember(obj, "name", false); err != nil {
		return model.Message{}, err
	}
	if m.Extra, err = extraMember(obj, "extra", false); err != nil {
		return model.Message{}, err
	}
	callsRaw, err := arrayMember(obj, "tool_calls", false)
	if err != nil {
		return model.Message{}, err
	}
	for i, raw := range callsRaw {
		call, err := decodeMessageToolCall(raw)
		if err != nil {
			return model.Message{}, fmt.Errorf("tool_calls[%d]: %w", i, err)
		}
		m.ToolCalls = append(m.ToolCalls, call)
	}
	if m.ToolCallID, err = optionalNonEmptyString(obj, "tool_call_id"); err != nil {
		return model.Message{}, err
	}
	owned, err := model.NewMessage(m)
	if err != nil {
		return model.Message{}, err
	}
	return owned, nil
}

// validateContributionMessage is the shared acceptance rule for every message
// that crosses the request-contribution and maintenance-input boundary: the
// complete permitted model.Message domain applies, and tool execution members
// are rejected outright — contributions never become historical tool
// execution. The returned value is the constructor's deep-owned copy.
func validateContributionMessage(m model.Message) (model.Message, error) {
	if len(m.ToolCalls) > 0 {
		return model.Message{}, errors.New("contribution messages carry no tool calls")
	}
	if m.ToolCallID != "" {
		return model.Message{}, errors.New("contribution messages carry no tool-result call id")
	}
	return model.NewMessage(m)
}

// mustMarshal marshals a value that cannot fail under the codec's own
// validation discipline.
func mustMarshal(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return raw
}

// validateHookIdentities enforces one recorded contribution list: non-empty
// extension and hook identities and unique pairs in preserved order.
func validateHookIdentities(list []HookIdentity) error {
	seen := make(map[HookIdentity]bool, len(list))
	for i, id := range list {
		if id.ExtensionID == "" {
			return fmt.Errorf("hook_ids[%d]: extension id must be non-empty", i)
		}
		if id.HookID == "" {
			return fmt.Errorf("hook_ids[%d]: hook id must be non-empty", i)
		}
		if seen[id] {
			return fmt.Errorf("hook_ids[%d]: hook identity %q/%q is not unique", i, id.ExtensionID, id.HookID)
		}
		seen[id] = true
	}
	return nil
}

// encodeHookIdentities renders one contribution list; an empty list encodes
// by absence.
func encodeHookIdentities(list []HookIdentity) (json.RawMessage, error) {
	if len(list) == 0 {
		return nil, nil
	}
	if err := validateHookIdentities(list); err != nil {
		return nil, invalidInput("%v", err)
	}
	return json.Marshal(list)
}

// decodeHookIdentities reads one contribution list with exact keys. A present
// empty array decodes to absence: additive provenance lists equal absent when
// they carry nothing.
func decodeHookIdentities(obj map[string]json.RawMessage, key string) ([]HookIdentity, error) {
	raw, err := arrayMember(obj, key, false)
	if err != nil {
		return nil, err
	}
	list := make([]HookIdentity, 0, len(raw))
	for i, item := range raw {
		itemObj, err := decodePayloadObject(item)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", key, i, err)
		}
		if err := rejectUnknownMembers(itemObj, "extension_id", "hook_id"); err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", key, i, err)
		}
		extensionID, err := stringMember(itemObj, "extension_id", true)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", key, i, err)
		}
		hookID, err := stringMember(itemObj, "hook_id", true)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", key, i, err)
		}
		list = append(list, HookIdentity{ExtensionID: extensionID, HookID: hookID})
	}
	if err := validateHookIdentities(list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list, nil
}

// optionalStringMember reads one string member whose presence is meaningful
// and whose value may be empty: absence means unchanged, an empty string is a
// recorded empty selection, and null is rejected.
func optionalStringMember(obj map[string]json.RawMessage, key string) (*string, error) {
	if _, present := obj[key]; !present {
		return nil, nil
	}
	s, err := stringMember(obj, key, true)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// encodeRequestEvidence renders one closed request-evidence record.
func encodeRequestEvidence(v RequestEvidence) (json.RawMessage, error) {
	if err := validateRequestEvidence(v); err != nil {
		return nil, invalidInput("request evidence: %v", err)
	}
	obj := map[string]json.RawMessage{
		"boundary_entry_id": mustMarshal(v.BoundaryEntryID),
	}
	if v.CheckpointEntryID != "" {
		obj["checkpoint_entry_id"] = mustMarshal(v.CheckpointEntryID)
	}
	if v.Prompt != nil {
		obj["prompt"] = mustMarshal(*v.Prompt)
	}
	if len(v.Messages) > 0 {
		messages := make([]json.RawMessage, 0, len(v.Messages))
		for _, m := range v.Messages {
			raw, err := encodeMessageValue(m)
			if err != nil {
				return nil, err
			}
			messages = append(messages, raw)
		}
		obj["messages"] = mustMarshal(messages)
	}
	if v.ToolNames != nil {
		obj["tool_names"] = mustMarshal(v.ToolNames)
	}
	if len(v.HookIDs) > 0 {
		raw, err := encodeHookIdentities(v.HookIDs)
		if err != nil {
			return nil, err
		}
		obj["hook_ids"] = raw
	}
	return json.Marshal(obj)
}

// decodeRequestEvidence reads one closed request-evidence record with exact
// keys and re-validates it. Present-empty tool_names survive as the recorded
// selection; empty messages and hook_ids arrays decode to absence.
func decodeRequestEvidence(raw json.RawMessage) (RequestEvidence, error) {
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return RequestEvidence{}, fmt.Errorf("request evidence: %w", err)
	}
	if err := rejectUnknownMembers(obj, "checkpoint_entry_id", "boundary_entry_id", "prompt", "messages", "tool_names", "hook_ids"); err != nil {
		return RequestEvidence{}, err
	}
	boundaryEntryID, err := stringMember(obj, "boundary_entry_id", true)
	if err != nil {
		return RequestEvidence{}, err
	}
	checkpointEntryID, err := optionalNonEmptyString(obj, "checkpoint_entry_id")
	if err != nil {
		return RequestEvidence{}, err
	}
	prompt, err := optionalStringMember(obj, "prompt")
	if err != nil {
		return RequestEvidence{}, err
	}
	var v RequestEvidence
	v.CheckpointEntryID = checkpointEntryID
	v.BoundaryEntryID = boundaryEntryID
	v.Prompt = prompt
	messagesRaw, err := arrayMember(obj, "messages", false)
	if err != nil {
		return RequestEvidence{}, err
	}
	for i, item := range messagesRaw {
		m, err := decodeMessageValue(item)
		if err != nil {
			return RequestEvidence{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		v.Messages = append(v.Messages, m)
	}
	toolNames, err := optionalStringMemberList(obj, "tool_names")
	if err != nil {
		return RequestEvidence{}, err
	}
	v.ToolNames = toolNames
	hookIDs, err := decodeHookIdentities(obj, "hook_ids")
	if err != nil {
		return RequestEvidence{}, err
	}
	v.HookIDs = hookIDs
	if err := validateRequestEvidence(v); err != nil {
		return RequestEvidence{}, err
	}
	return v, nil
}

// validateRequestEvidence enforces the closed evidence shape: the canonical
// boundary reference is required and durable-shaped, a present checkpoint
// reference is durable-shaped, tool names are non-empty and unique, recorded
// contributions are validated messages, and hook provenance is unique.
func validateRequestEvidence(v RequestEvidence) error {
	if err := validateHexID(v.BoundaryEntryID, "evidence boundary entry id"); err != nil {
		return err
	}
	if v.CheckpointEntryID != "" {
		if err := validateHexID(v.CheckpointEntryID, "evidence checkpoint entry id"); err != nil {
			return err
		}
	}
	for i, m := range v.Messages {
		if _, err := validateContributionMessage(m); err != nil {
			return fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	if v.ToolNames != nil {
		seen := make(map[string]bool, len(v.ToolNames))
		for _, name := range v.ToolNames {
			if name == "" {
				return errors.New("evidence tool_names carries an empty name")
			}
			if seen[name] {
				return fmt.Errorf("evidence tool_names has duplicate name %q", name)
			}
			seen[name] = true
		}
	}
	return validateHookIdentities(v.HookIDs)
}

// optionalStringMemberList reads one string-list member: absence decodes to
// nil, and a present-empty array stays a meaningful present-empty selection.
func optionalStringMemberList(obj map[string]json.RawMessage, key string) ([]string, error) {
	if _, present := obj[key]; !present {
		return nil, nil
	}
	items, err := arrayMember(obj, key, true)
	if err != nil {
		return nil, err
	}
	list := make([]string, 0, len(items))
	for i, raw := range items {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s[%d] must be a JSON string: %w", key, i, err)
		}
		if s == "" {
			return nil, fmt.Errorf("%s[%d] must be non-empty", key, i)
		}
		list = append(list, s)
	}
	return list, nil
}

// encodeRequestConsequence renders one closed request consequence. Prompt
// presence (including empty) and ToolNames presence (including the full or
// empty selection) survive exactly; empty Messages encode by absence.
func encodeRequestConsequence(v RequestConsequence) (json.RawMessage, error) {
	if err := validateRequestConsequenceShape(v); err != nil {
		return nil, invalidInput("request consequence: %v", err)
	}
	obj := map[string]json.RawMessage{}
	if v.Prompt != nil {
		obj["prompt"] = mustMarshal(*v.Prompt)
	}
	if len(v.Messages) > 0 {
		messages := make([]json.RawMessage, 0, len(v.Messages))
		for _, m := range v.Messages {
			raw, err := encodeMessageValue(m)
			if err != nil {
				return nil, err
			}
			messages = append(messages, raw)
		}
		obj["messages"] = mustMarshal(messages)
	}
	if v.ToolNames != nil {
		obj["tool_names"] = mustMarshal(v.ToolNames)
	}
	return json.Marshal(obj)
}

// decodeRequestConsequence reads one closed request consequence with exact
// keys. A present empty tool_names is the recorded no-advertisement
// selection; a present full list stays the recorded full selection.
func decodeRequestConsequence(raw json.RawMessage) (RequestConsequence, error) {
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return RequestConsequence{}, fmt.Errorf("request consequence: %w", err)
	}
	if err := rejectUnknownMembers(obj, "prompt", "messages", "tool_names"); err != nil {
		return RequestConsequence{}, err
	}
	prompt, err := optionalStringMember(obj, "prompt")
	if err != nil {
		return RequestConsequence{}, err
	}
	var v RequestConsequence
	v.Prompt = prompt
	messagesRaw, err := arrayMember(obj, "messages", false)
	if err != nil {
		return RequestConsequence{}, err
	}
	for i, item := range messagesRaw {
		m, err := decodeMessageValue(item)
		if err != nil {
			return RequestConsequence{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		v.Messages = append(v.Messages, m)
	}
	toolNames, err := optionalStringMemberList(obj, "tool_names")
	if err != nil {
		return RequestConsequence{}, err
	}
	v.ToolNames = toolNames
	if err := validateRequestConsequenceShape(v); err != nil {
		return RequestConsequence{}, err
	}
	return v, nil
}

// validateRequestConsequenceShape enforces the consequence's member shapes:
// recorded contributions are validated messages and tool names are non-empty
// and unique. Catalog membership is decided at the applying boundary, which
// alone knows the captured catalog.
func validateRequestConsequenceShape(v RequestConsequence) error {
	for i, m := range v.Messages {
		if _, err := validateContributionMessage(m); err != nil {
			return fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	seen := make(map[string]bool, len(v.ToolNames))
	for _, name := range v.ToolNames {
		if name == "" {
			return errors.New("consequence tool_names carries an empty name")
		}
		if seen[name] {
			return fmt.Errorf("consequence tool_names has duplicate name %q", name)
		}
		seen[name] = true
	}
	return nil
}

// validateMaintenanceInput enforces one maintenance settlement/checkpoint
// input: the actual logical maintenance request messages under the shared
// Request message domain — the request is tool-free by its zero advertised
// tools, not by a restriction on its historical messages, which may carry
// authentic tool exchanges. The shared Request permits zero messages.
func validateMaintenanceInput(messages []model.Message) error {
	for i, m := range messages {
		if _, err := model.NewMessage(m); err != nil {
			return fmt.Errorf("input[%d]: %w", i, err)
		}
	}
	return nil
}

// encodeMaintenanceInput renders one maintenance input message list.
func encodeMaintenanceInput(messages []model.Message) (json.RawMessage, error) {
	if err := validateMaintenanceInput(messages); err != nil {
		return nil, invalidInput("%v", err)
	}
	items := make([]json.RawMessage, 0, len(messages))
	for _, m := range messages {
		raw, err := encodeMessageValue(m)
		if err != nil {
			return nil, err
		}
		items = append(items, raw)
	}
	return mustMarshal(items), nil
}

// decodeMaintenanceInput reads one maintenance input message list.
func decodeMaintenanceInput(raw json.RawMessage) ([]model.Message, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("maintenance input must be a JSON array: %w", err)
	}
	messages := make([]model.Message, 0, len(items))
	for i, item := range items {
		m, err := decodeMessageValue(item)
		if err != nil {
			return nil, fmt.Errorf("input[%d]: %w", i, err)
		}
		messages = append(messages, m)
	}
	if err := validateMaintenanceInput(messages); err != nil {
		return nil, err
	}
	return messages, nil
}
