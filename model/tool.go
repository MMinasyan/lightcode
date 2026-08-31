package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ToolDefinition is a function tool advertised to the model: name, description, and parameters as raw JSON. Only function tools exist in this phase; there is no configurable type field and no strict flag. Parameters must be valid JSON whose top-level value is an object; the schema dialect itself is not validated here (concrete-tool contracts own that later).
type ToolDefinition struct {
	Name        string          // non-empty tool name, unique within one request.
	Description string          // model-visible description text.
	Parameters  json.RawMessage // JSON object as raw bytes; deep-copied at the accepting boundary.
}

// ErrInvalidParameters is returned when a tool definition's parameters are not valid JSON with an object top-level value (malformed, array, primitive, or null).
var ErrInvalidParameters = errors.New("tool parameters must be a JSON object")

func validateToolDefinition(d ToolDefinition) error {
	if d.Name == "" {
		return fmt.Errorf("%w: tool definition name", ErrMissingField)
	}
	// Syntax-only validation, deliberately without decoding: parameter content may contain numbers Go's float64 cannot represent (e.g. 1e1000), which a decode-based check would wrongly reject.
	if trimmed := bytes.TrimSpace(d.Parameters); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%w: top-level value must be an object", ErrInvalidParameters)
	}
	if !json.Valid(d.Parameters) {
		return fmt.Errorf("%w: malformed JSON parameters", ErrInvalidParameters)
	}
	return nil
}

// NewToolDefinition validates in and returns an independent copy with the parameters bytes cloned. Validation rejects empty names, malformed parameter JSON, and non-object top levels; schema-dialect content is deliberately not checked here.
func NewToolDefinition(in ToolDefinition) (ToolDefinition, error) {
	if err := validateToolDefinition(in); err != nil {
		return ToolDefinition{}, err
	}
	out := in
	out.Parameters = cloneRaw(in.Parameters)
	return out, nil
}

// ToolCall is a completed assistant tool call: stable id, concrete name, and raw argument bytes. The final canonical call carries no wire-type field (the stream assembler treats an omitted type as the function default and strips it during finalization). Arguments are retained byte-for-byte without JSON validation — empty, malformed, null, array, primitive, object, or schema-invalid arguments all remain caller-owned tool preparation/validation input; they are cloned across ownership boundaries.
type ToolCall struct {
	ID        string          // stable non-empty call identity for result correlation.
	Name      string          // concrete tool name the model selected.
	Arguments json.RawMessage // completed wire argument bytes, preserved verbatim and never decoded into a map here.
	Extra     Extra           // raw extras; deep-copied at every accepting boundary.
}

// NewToolCall validates in (non-empty id and name) and returns an independent copy: arguments are copied byte-for-byte without any JSON validation — the stated exception to complete-JSON requirements — and extras are cloned.
func NewToolCall(in ToolCall) (ToolCall, error) {
	if in.ID == "" || in.Name == "" {
		return ToolCall{}, fmt.Errorf("%w: tool call requires a non-empty id and name", ErrMissingField)
	}
	if err := validateExtraValues(in.Extra); err != nil { // arguments stay raw; only extras are JSON-checked.
		return ToolCall{}, err
	}
	out := in
	out.Arguments = cloneRaw(in.Arguments)
	out.Extra = in.Extra.Clone()
	return out, nil
}

// Request is the logical model request: owned messages and owned tool definitions only. Provider endpoint, credentials, headers, request defaults, runtime extras, retry state, and model adaptation are transport concerns and never fields of a logical request.
type Request struct {
	Messages []Message        // ordered conversation context; each message validated at construction.
	Tools    []ToolDefinition // advertised function tools; names unique within the request.
}

// ErrDuplicateToolName is returned when two tool definitions in one request share a name.
var ErrDuplicateToolName = errors.New("duplicate tool definition name")

// NewRequest validates and deep-copies every message (role/source rules, field combinations, part kinds) and tool definition (name, object-shaped parameters), rejects duplicate tool names, and returns an owned request independent of the input slices and bytes.
func NewRequest(in Request) (Request, error) {
	out := Request{Messages: make([]Message, 0, len(in.Messages)), Tools: make([]ToolDefinition, 0, len(in.Tools))}

	for i, msg := range in.Messages {
		if err := validateMessage(msg); err != nil {
			return Request{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, cloneOwnedMessage(msg))
	}

	seen := make(map[string]bool, len(in.Tools))
	for i, def := range in.Tools {
		if err := validateToolDefinition(def); err != nil {
			return Request{}, fmt.Errorf("tools[%d]: %w", i, err)
		}
		if seen[def.Name] {
			return Request{}, fmt.Errorf("%w: %q", ErrDuplicateToolName, def.Name)
		}
		seen[def.Name] = true
		out.Tools = append(out.Tools, ToolDefinition{
			Name:        def.Name,
			Description: def.Description,
			Parameters:  cloneRaw(def.Parameters),
		})
	}
	return out, nil
}

// ToolResultStatus is the closed status of one settled tool result: success, error, denied, or interrupted.
type ToolResultStatus string

const (
	ResultSuccess     ToolResultStatus = "success"     // content may be empty; output bounds are owned by each concrete tool contract.
	ResultError       ToolResultStatus = "error"       // requires non-empty model-visible content.
	ResultDenied      ToolResultStatus = "denied"      // requires non-empty model-visible content (e.g. a permission denial).
	ResultInterrupted ToolResultStatus = "interrupted" // requires non-empty model-visible content; stops the current batch in the agent loop.
)

// ErrInvalidStatus is returned when a tool-result or output status value falls outside its closed set.
var ErrInvalidStatus = errors.New("invalid status")

func validToolResultStatus(s ToolResultStatus) bool {
	switch s {
	case ResultSuccess, ResultError, ResultDenied, ResultInterrupted:
		return true
	default:
		return false
	}
}

// ToolResult is one settled concrete-tool outcome visible to the model: the original call id it answers, its closed status, and the model-visible content string. Error/denied/interrupted statuses require non-empty content; success may carry none. The caller guarantees content is already bounded by its concrete tool contract — no second output limiter exists here.
type ToolResult struct {
	CallID  string           // original call id this result answers; non-empty only.
	Status  ToolResultStatus // closed enum: success / error / denied / interrupted.
	Content string           // model-visible content text, bounded by the concrete tool contract upstream.
}

// NewToolResult validates in — non-empty call id, status within the closed set, and non-empty content for every non-success status — and returns it as-is: results carry no reference-typed fields needing ownership transfer, so validation is their whole boundary work.
func NewToolResult(in ToolResult) (ToolResult, error) {
	if in.CallID == "" {
		return ToolResult{}, fmt.Errorf("%w: tool result requires the original call id", ErrMissingField)
	}
	if !validToolResultStatus(in.Status) {
		return ToolResult{}, fmt.Errorf("%w: %q is not one of success, error, denied or interrupted", ErrInvalidStatus, in.Status)
	}
	if in.Content == "" && in.Status != ResultSuccess {
		return ToolResult{}, fmt.Errorf("%w: status %s requires non-empty model-visible content", ErrMissingField, in.Status)
	}
	return in, nil
}

// cloneOwnedMessage deep-copies a message whose validation already passed at the current boundary. It is shared by NewMessage and other accepting constructors so every ownership transfer copies parts, calls, argument bytes, and extras independently of the input.
func cloneOwnedMessage(in Message) Message {
	out := in
	if len(out.Content) > 0 {
		out.Content = make([]ContentPart, len(in.Content))
		for i, part := range in.Content {
			part.Extra = part.Extra.Clone()
			out.Content[i] = part
		}
	}
	if len(out.ToolCalls) > 0 {
		out.ToolCalls = make([]ToolCall, len(in.ToolCalls))
		for i, call := range in.ToolCalls {
			call.Arguments = cloneRaw(call.Arguments)
			call.Extra = call.Extra.Clone()
			out.ToolCalls[i] = call
		}
	}
	if out.Extra != nil {
		out.Extra = in.Extra.Clone()
	}
	return out
}
