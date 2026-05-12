// Package message defines Lightcode's canonical conversation message model.
package message

import (
	"encoding/json"
	"fmt"

	"github.com/MMinasyan/lightcode/internal/catalog"
)

// Role is a canonical conversation role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentPartType is the canonical type of a message content part.
type ContentPartType string

const (
	ContentPartText     ContentPartType = "text"
	ContentPartImageURL ContentPartType = "image_url"
	ContentPartOpaque   ContentPartType = "opaque"
)

// Message is Lightcode's provider-independent message shape.
type Message struct {
	Role       Role
	Content    []ContentPart
	ToolCalls  []ToolCall
	ToolCallID string
	Name       string
	Extra      Extra
	Source     catalog.ModelRef
}

// ContentPart is one canonical content item. Opaque parts preserve the full
// provider wire object in Extra.
type ContentPart struct {
	Type  ContentPartType
	Text  string
	URL   string
	Extra Extra
}

// ToolCall is an assistant tool call with optional provider-specific data.
type ToolCall struct {
	ID       string
	Type     string
	Function FunctionCall
	Extra    Extra
}

// FunctionCall is the OpenAI-compatible function call payload.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// NewText returns a message with one text content part.
func NewText(role Role, text string) Message {
	var msg Message
	msg.Role = role
	if text != "" {
		msg.Content = []ContentPart{{Type: ContentPartText, Text: text}}
	}
	return msg
}

// TextContent returns all text parts joined in order.
func (m Message) TextContent() string {
	var out string
	for _, part := range m.Content {
		if part.Type == ContentPartText {
			out += part.Text
		}
	}
	return out
}

// AppendText appends text to the last text part, or creates one.
func (m *Message) AppendText(text string) {
	if text == "" {
		return
	}
	if len(m.Content) > 0 && m.Content[len(m.Content)-1].Type == ContentPartText {
		m.Content[len(m.Content)-1].Text += text
		return
	}
	m.Content = append(m.Content, ContentPart{Type: ContentPartText, Text: text})
}

// MarshalJSON serializes messages in the OpenAI-compatible shape, with Extra
// flattened as provider wire siblings and Lightcode source kept private.
func (m Message) MarshalJSON() ([]byte, error) {
	obj := map[string]json.RawMessage{}
	if m.Extra != nil {
		for key, value := range m.Extra {
			if isMessageField(key) || isLightcodeField(key) {
				continue
			}
			obj[key] = cloneRaw(value)
		}
	}
	if m.Role != "" {
		mustSet(obj, "role", string(m.Role))
	}
	if len(m.Content) > 0 {
		content, err := marshalContent(m.Content)
		if err != nil {
			return nil, err
		}
		obj["content"] = content
	}
	if len(m.ToolCalls) > 0 {
		data, err := json.Marshal(m.ToolCalls)
		if err != nil {
			return nil, err
		}
		obj["tool_calls"] = data
	}
	if m.ToolCallID != "" {
		mustSet(obj, "tool_call_id", m.ToolCallID)
	}
	if m.Name != "" {
		mustSet(obj, "name", m.Name)
	}
	if m.Source.String() != "" {
		mustSet(obj, "_lightcode_source", m.Source.String())
	}
	return json.Marshal(obj)
}

// UnmarshalJSON loads both canonical fields and unknown provider wire siblings.
func (m *Message) UnmarshalJSON(data []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}

	var out Message
	for key, value := range obj {
		switch key {
		case "role":
			var role Role
			if err := json.Unmarshal(value, &role); err != nil {
				return fieldError(key, err)
			}
			out.Role = role
		case "content":
			content, err := unmarshalContent(value)
			if err != nil {
				return fieldError(key, err)
			}
			out.Content = content
		case "tool_calls":
			if err := json.Unmarshal(value, &out.ToolCalls); err != nil {
				return fieldError(key, err)
			}
		case "tool_call_id":
			if err := json.Unmarshal(value, &out.ToolCallID); err != nil {
				return fieldError(key, err)
			}
		case "name":
			if err := json.Unmarshal(value, &out.Name); err != nil {
				return fieldError(key, err)
			}
		case "_lightcode_source":
			var source string
			if err := json.Unmarshal(value, &source); err != nil {
				return fieldError(key, err)
			}
			if source != "" {
				ref, err := catalog.ParseModelRef(source)
				if err != nil {
					return fieldError(key, err)
				}
				out.Source = ref
			}
		default:
			if isLightcodeField(key) {
				continue
			}
			if out.Extra == nil {
				out.Extra = Extra{}
			}
			out.Extra[key] = cloneRaw(value)
		}
	}
	*m = out
	return nil
}

// MarshalJSON serializes content parts as OpenAI-compatible content objects.
func (p ContentPart) MarshalJSON() ([]byte, error) {
	obj := map[string]json.RawMessage{}
	if p.Extra != nil {
		for key, value := range p.Extra {
			if isLightcodeField(key) {
				continue
			}
			obj[key] = cloneRaw(value)
		}
	}
	switch p.Type {
	case ContentPartText:
		mustSet(obj, "type", string(ContentPartText))
		mustSet(obj, "text", p.Text)
	case ContentPartImageURL:
		mustSet(obj, "type", string(ContentPartImageURL))
		mustSet(obj, "image_url", map[string]string{"url": p.URL})
	case ContentPartOpaque:
		if len(obj) == 0 {
			mustSet(obj, "type", string(ContentPartOpaque))
		}
	case "":
		if p.Text != "" {
			mustSet(obj, "type", string(ContentPartText))
			mustSet(obj, "text", p.Text)
		}
	default:
		mustSet(obj, "type", string(p.Type))
	}
	return json.Marshal(obj)
}

// UnmarshalJSON loads a content part and keeps non-canonical fields in Extra.
func (p *ContentPart) UnmarshalJSON(data []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	var typ string
	if raw := obj["type"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &typ); err != nil {
			return fieldError("type", err)
		}
	}
	out := ContentPart{Type: ContentPartType(typ)}
	switch out.Type {
	case ContentPartText:
		if raw := obj["text"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &out.Text); err != nil {
				return fieldError("text", err)
			}
		}
	case ContentPartImageURL:
		if raw := obj["image_url"]; len(raw) > 0 {
			url, err := decodeImageURL(raw)
			if err != nil {
				return fieldError("image_url", err)
			}
			out.URL = url
		}
	default:
		out.Type = ContentPartOpaque
	}
	for key, value := range obj {
		if out.Type != ContentPartOpaque && isContentPartField(key) {
			continue
		}
		if isLightcodeField(key) {
			continue
		}
		if out.Extra == nil {
			out.Extra = Extra{}
		}
		out.Extra[key] = cloneRaw(value)
	}
	*p = out
	return nil
}

// MarshalJSON serializes tool calls with Extra flattened as siblings.
func (tc ToolCall) MarshalJSON() ([]byte, error) {
	obj := map[string]json.RawMessage{}
	if tc.Extra != nil {
		for key, value := range tc.Extra {
			if isToolCallField(key) || isLightcodeField(key) {
				continue
			}
			obj[key] = cloneRaw(value)
		}
	}
	if tc.ID != "" {
		mustSet(obj, "id", tc.ID)
	}
	if tc.Type != "" {
		mustSet(obj, "type", tc.Type)
	}
	if tc.Function.Name != "" || tc.Function.Arguments != "" {
		mustSet(obj, "function", tc.Function)
	}
	return json.Marshal(obj)
}

// UnmarshalJSON loads a tool call and keeps non-canonical fields in Extra.
func (tc *ToolCall) UnmarshalJSON(data []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	var out ToolCall
	for key, value := range obj {
		switch key {
		case "id":
			if err := json.Unmarshal(value, &out.ID); err != nil {
				return fieldError(key, err)
			}
		case "type":
			if err := json.Unmarshal(value, &out.Type); err != nil {
				return fieldError(key, err)
			}
		case "function":
			if err := json.Unmarshal(value, &out.Function); err != nil {
				return fieldError(key, err)
			}
		case "index":
			continue
		default:
			if isLightcodeField(key) {
				continue
			}
			if out.Extra == nil {
				out.Extra = Extra{}
			}
			out.Extra[key] = cloneRaw(value)
		}
	}
	*tc = out
	return nil
}

func marshalContent(parts []ContentPart) (json.RawMessage, error) {
	if len(parts) == 1 && parts[0].Type == ContentPartText && len(parts[0].Extra) == 0 {
		return json.Marshal(parts[0].Text)
	}
	return json.Marshal(parts)
}

func unmarshalContent(raw json.RawMessage) ([]ContentPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []ContentPart{{Type: ContentPartText, Text: text}}, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	return parts, nil
}

func decodeImageURL(raw json.RawMessage) (string, error) {
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.URL, nil
	}
	var url string
	if err := json.Unmarshal(raw, &url); err != nil {
		return "", err
	}
	return url, nil
}

func mustSet(obj map[string]json.RawMessage, key string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	obj[key] = data
}

func fieldError(field string, err error) error {
	return fmt.Errorf("%s: %w", field, err)
}

func isMessageField(key string) bool {
	switch key {
	case "role", "content", "tool_calls", "tool_call_id", "name", "_lightcode_source":
		return true
	default:
		return false
	}
}

func isToolCallField(key string) bool {
	switch key {
	case "id", "type", "function", "index":
		return true
	default:
		return false
	}
}

func isContentPartField(key string) bool {
	switch key {
	case "type", "text", "image_url":
		return true
	default:
		return false
	}
}
