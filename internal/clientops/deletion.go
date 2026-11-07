package clientops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/MMinasyan/lightcode/protocol"
)

// DecodeDeletionResult validates and decodes the one DeleteSession success
// document at the accepting client boundary. The closed shape is exact: one
// object carrying only cleanup_failures, one non-null array of failure objects
// whose code/message are present strings and whose optional extension_id, when
// present, is a nonempty string. Local storage failures carry no owner;
// external unavailable failures require one. Protocol-defined arbitrary
// message strings stay unchanged.
func DecodeDeletionResult(body []byte) (protocol.DeletionResult, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]json.RawMessage
	if err := decoder.Decode(&root); err != nil {
		return protocol.DeletionResult{}, fmt.Errorf("deletion result is not one JSON object: %w", err)
	}
	if root == nil {
		return protocol.DeletionResult{}, fmt.Errorf("deletion result is not one JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return protocol.DeletionResult{}, fmt.Errorf("deletion result carries trailing data")
	}
	if len(root) != 1 {
		return protocol.DeletionResult{}, fmt.Errorf("deletion result must carry exactly cleanup_failures")
	}
	rawFailures, ok := root["cleanup_failures"]
	if !ok || isJSONNull(rawFailures) {
		return protocol.DeletionResult{}, fmt.Errorf("deletion result is missing non-null cleanup_failures")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(rawFailures, &entries); err != nil || entries == nil {
		return protocol.DeletionResult{}, fmt.Errorf("cleanup_failures must be a JSON array")
	}
	failures := make([]protocol.CleanupFailure, 0, len(entries))
	for i, entry := range entries {
		failure, err := decodeCleanupFailure(entry)
		if err != nil {
			return protocol.DeletionResult{}, fmt.Errorf("cleanup_failures[%d]: %w", i, err)
		}
		failures = append(failures, failure)
	}
	return protocol.DeletionResult{CleanupFailures: failures}, nil
}

// decodeCleanupFailure validates one closed failure object.
func decodeCleanupFailure(raw json.RawMessage) (protocol.CleanupFailure, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return protocol.CleanupFailure{}, fmt.Errorf("failure must be one JSON object")
	}
	for name := range members {
		switch name {
		case "code", "message", "extension_id":
		default:
			return protocol.CleanupFailure{}, fmt.Errorf("unknown member %q", name)
		}
	}
	rawCode, ok := members["code"]
	if !ok || isJSONNull(rawCode) {
		return protocol.CleanupFailure{}, fmt.Errorf("failure is missing non-null code")
	}
	var code string
	if err := json.Unmarshal(rawCode, &code); err != nil {
		return protocol.CleanupFailure{}, fmt.Errorf("failure code must be a string")
	}
	closed := protocol.CleanupFailureCode(code)
	if !closed.Valid() {
		return protocol.CleanupFailure{}, fmt.Errorf("failure code %q is outside the closed set", code)
	}
	rawMessage, ok := members["message"]
	if !ok || isJSONNull(rawMessage) {
		return protocol.CleanupFailure{}, fmt.Errorf("failure is missing non-null message")
	}
	var message string
	if err := json.Unmarshal(rawMessage, &message); err != nil {
		return protocol.CleanupFailure{}, fmt.Errorf("failure message must be a string")
	}
	var extensionID *string
	if rawOwner, present := members["extension_id"]; present {
		if isJSONNull(rawOwner) {
			return protocol.CleanupFailure{}, fmt.Errorf("failure extension_id must not be null")
		}
		var owner string
		if err := json.Unmarshal(rawOwner, &owner); err != nil || owner == "" {
			return protocol.CleanupFailure{}, fmt.Errorf("failure extension_id must be a nonempty string")
		}
		extensionID = &owner
	}
	switch closed {
	case protocol.CleanupFailureCodeStorage:
		if extensionID != nil {
			return protocol.CleanupFailure{}, fmt.Errorf("local storage failure must not carry extension_id")
		}
	case protocol.CleanupFailureCodeUnavailable:
		if extensionID == nil {
			return protocol.CleanupFailure{}, fmt.Errorf("external unavailable failure requires extension_id")
		}
	default:
		return protocol.CleanupFailure{}, fmt.Errorf("failure code %q is outside the closed set", code)
	}
	return protocol.CleanupFailure{Code: closed, ExtensionId: extensionID, Message: message}, nil
}
