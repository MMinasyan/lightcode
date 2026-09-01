package model

import (
	"errors"
	"fmt"
)

// Role is a canonical conversation role. The closed set is system, user, assistant, and tool; "developer" exists only as a resolved wire system-role value in the encoder layer, never as a canonical role here.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

func validRole(r Role) bool {
	switch r {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		return true
	default:
		return false
	}
}

// Sentinels for message construction/validator failures. Constructors wrap them with context describing the violating field or value.
var (
	ErrInvalidRole      = errors.New("invalid role")                     // closed conversation role violated.
	ErrMissingSource    = errors.New("missing source model identity")    // assistant requires a complete, nonzero ModelRef; zero and partial identities are both rejected.
	ErrUnexpectedSource = errors.New("source not allowed for this role") // system/user/tool messages require the zero source identity; any present or partial ref is invalid.
	ErrForbiddenField   = errors.New("field not allowed here")           // refusal/assistant tool calls on a non-assistant, tool-call id outside tool messages, and content-part kind/exclusivity violations.
	ErrMissingField     = errors.New("required field missing")           // empty required values: opaque wire type, embedded call id/name, tool message without its result call id.
)

// PartKind is the closed canonical kind of a content part: text, image_url, or opaque. Opaque parts preserve their original provider wire type structurally; that structural value never enters the generic extra accumulator.
type PartKind string

const (
	PartText     PartKind = "text"      // carries only Text.
	PartImageURL PartKind = "image_url" // carries only URL.
	PartOpaque   PartKind = "opaque"    // requires a non-empty OpaqueWireType; the original wire object's fields stay in Extra.
)

// ContentPart is one canonical content item of an ordered message body. Each kind owns exactly its own field: text uses Text, image_url uses URL, opaque requires OpaqueWireType and carries no Text/URL.
type ContentPart struct {
	Kind           PartKind
	Text           string
	URL            string
	OpaqueWireType string // original provider wire type for opaque parts only; structural, never accumulated as an extra.
	Extra          Extra  // raw extras; deep-copied at every accepting boundary.
}

func validateContentPart(p ContentPart) error {
	switch p.Kind {
	case PartText:
		if p.URL != "" || p.OpaqueWireType != "" {
			return fmt.Errorf("%w: text part must not carry a URL or opaque wire type", ErrForbiddenField)
		}
	case PartImageURL:
		if p.Text != "" || p.OpaqueWireType != "" {
			return fmt.Errorf("%w: image_url part must not carry text or an opaque wire type", ErrForbiddenField)
		}
	case PartOpaque:
		if p.Text != "" || p.URL != "" {
			return fmt.Errorf("%w: opaque part must not carry text or a URL; its original wire fields belong in Extra", ErrForbiddenField)
		}
		if p.OpaqueWireType == "" {
			return fmt.Errorf("%w: opaque part requires a non-empty original wire type", ErrMissingField)
		}
	default:
		return fmt.Errorf("%w: content part kind %q is not one of text, image_url or opaque", ErrForbiddenField, p.Kind)
	}
	if err := validateExtraValues(p.Extra); err != nil {
		return err
	}
	return nil
}

// NewContentPart validates in and returns an independent deep copy with its extras cloned. Validation rejects unknown kinds, cross-kind field combinations, opaque parts without their original wire type, and extra values that are not valid JSON; empty kind-specific content is allowed because finalization omits empty parts later.
func NewContentPart(in ContentPart) (ContentPart, error) {
	if err := validateContentPart(in); err != nil {
		return ContentPart{}, err
	}
	out := in
	out.Extra = in.Extra.Clone()
	return out, nil
}

// Message is the provider-neutral canonical conversation message. It carries only model-visible fields: no internal kind, display metadata, durable identity, or protocol persistence state; those belong to their owning layers and never enter this value. Messages have no general JSON wire contract — all provider wire encoding is produced by the encoder layer from these values.
type Message struct {
	Role       Role          // closed canonical role.
	Content    []ContentPart // ordered content parts (may be empty).
	Refusal    string        // assistant-only; empty means absent.
	ToolCalls  []ToolCall    // assistant-only, in model order.
	ToolCallID string        // tool-only: the original call id this result answers; required for tool messages.
	Name       string        // optional canonical name field allowed on any role.
	Extra      Extra         // raw extras; deep-copied at every accepting boundary.
	Source     ModelRef      // assistant requires a complete identity; system/user/tool require the zero value (a partial ref is invalid wherever nonzero or zero is required).
}

func validateMessage(m Message) error {
	if !validRole(m.Role) {
		return fmt.Errorf("%w: %q", ErrInvalidRole, m.Role)
	}
	switch m.Role {
	case RoleAssistant:
		if !m.Source.complete() {
			return fmt.Errorf("%w: assistant message requires a complete source model identity, got %s", ErrMissingSource, describeRef(m.Source))
		}
	default: // system/user/tool require the zero source; a partial ref is not zero and is invalid too.
		if !m.Source.IsZero() {
			return fmt.Errorf("%w: role %q requires the zero source model identity", ErrUnexpectedSource, m.Role)
		}
	}

	if (m.Refusal != "" || len(m.ToolCalls) > 0) && m.Role != RoleAssistant {
		return fmt.Errorf("%w: refusal and tool calls are assistant-only fields on role %q", ErrForbiddenField, m.Role)
	}
	switch m.Role {
	case RoleTool:
		if m.ToolCallID == "" {
			return fmt.Errorf("%w: tool message requires the original call id it answers", ErrMissingField)
		}
	default:
		if m.ToolCallID != "" {
			return fmt.Errorf("%w: a tool-result call id is only carried by tool messages (role %q)", ErrForbiddenField, m.Role)
		}
	}

	for i, part := range m.Content {
		if err := validateContentPart(part); err != nil {
			return fmt.Errorf("content[%d]: %w", i, err)
		}
	}
	for i, call := range m.ToolCalls {
		if call.ID == "" || call.Name == "" {
			return fmt.Errorf("%w: assistant tool calls require a non-empty id and name (call %d)", ErrMissingField, i)
		}
		if err := validateExtraValues(call.Extra); err != nil {
			return fmt.Errorf("tool_calls[%d]: %w", i, err)
		}
	}
	if err := validateExtraValues(m.Extra); err != nil {
		return err
	}
	return nil
}

func describeRef(r ModelRef) string {
	switch {
	case r.IsZero():
		return "zero"
	default:
		return fmt.Sprintf("partial (%q/%q)", r.Provider, r.Model)
	}
}

// NewMessage validates in and returns an independent owned copy: content parts are re-validated and their extras deep-copied, tool calls keep byte-owned argument copies (arguments stay raw bytes without JSON validation), message extras are cloned. Validation enforces the closed role set, per-role source identity rules (assistant requires a complete ref; system/user/tool require zero), assistant-only refusal/calls, tool-only required call id, and part-level field combinations.
func NewMessage(in Message) (Message, error) {
	if err := validateMessage(in); err != nil {
		return Message{}, err
	}
	return cloneOwnedMessage(in), nil
}

// TextContent returns all text parts joined in order without separators; non-text parts are skipped. Retained legacy helper semantics: the joining is a plain concatenation of part texts in content order.
func (m Message) TextContent() string {
	var out string
	for _, part := range m.Content {
		if part.Kind == PartText {
			out += part.Text
		}
	}
	return out
}

// AppendText appends text to the last content part when it is a text part, otherwise creates one. An empty argument is a no-op that never creates a part. Retained legacy helper semantics verbatim: callers mutate their owned message directly; ownership rules apply at construction boundaries (NewMessage/NewRequest), so append into an unvalidated literal does not bypass role/source validation as long as the final value still passes it.
func (m *Message) AppendText(text string) {
	if text == "" {
		return
	}
	if len(m.Content) > 0 && m.Content[len(m.Content)-1].Kind == PartText {
		m.Content[len(m.Content)-1].Text += text
		return
	}
	m.Content = append(m.Content, ContentPart{Kind: PartText, Text: text})
}
