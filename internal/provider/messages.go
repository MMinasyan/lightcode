package provider

import (
	"strings"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/message"
)

// SerializeMessages converts canonical messages to the OpenAI-compatible
// message maps used in provider request bodies.
func SerializeMessages(history []message.Message, target *catalog.Model, provider *catalog.Provider) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(history))
	systemRole := effectiveSystemRole(provider, target)
	for _, msg := range history {
		obj, err := serializeMessage(msg, systemRole)
		if err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	return out, nil
}

func serializeMessage(msg message.Message, systemRole catalog.SystemRole) (map[string]any, error) {
	obj := rawExtraMap(msg.Extra)
	role := string(msg.Role)
	if msg.Role == message.RoleSystem {
		if systemRole == "" {
			systemRole = catalog.RoleSystem
		}
		role = string(systemRole)
	}
	if role != "" {
		obj["role"] = role
	}
	if len(msg.Content) > 0 {
		content, err := serializeContent(msg.Content)
		if err != nil {
			return nil, err
		}
		obj["content"] = content
	}
	if msg.Refusal != "" {
		obj["refusal"] = msg.Refusal
	}
	if len(msg.ToolCalls) > 0 {
		toolCalls := make([]map[string]any, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			toolCalls = append(toolCalls, serializeToolCall(tc))
		}
		obj["tool_calls"] = toolCalls
	}
	if msg.ToolCallID != "" {
		obj["tool_call_id"] = msg.ToolCallID
	}
	if msg.Name != "" {
		obj["name"] = msg.Name
	}
	return obj, nil
}

func serializeContent(parts []message.ContentPart) (any, error) {
	if len(parts) == 1 && parts[0].Type == message.ContentPartText && len(parts[0].Extra) == 0 {
		return parts[0].Text, nil
	}
	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		obj := rawExtraMap(part.Extra)
		switch part.Type {
		case message.ContentPartText:
			obj["type"] = string(message.ContentPartText)
			obj["text"] = part.Text
		case message.ContentPartImageURL:
			obj["type"] = string(message.ContentPartImageURL)
			obj["image_url"] = map[string]any{"url": part.URL}
		case message.ContentPartOpaque:
			if _, ok := obj["type"]; !ok {
				obj["type"] = string(message.ContentPartOpaque)
			}
		default:
			obj["type"] = string(part.Type)
		}
		out = append(out, obj)
	}
	return out, nil
}

func serializeToolCall(tc message.ToolCall) map[string]any {
	obj := rawExtraMap(tc.Extra)
	if tc.ID != "" {
		obj["id"] = tc.ID
	}
	if tc.Type != "" {
		obj["type"] = tc.Type
	}
	if tc.Function.Name != "" || tc.Function.Arguments != "" {
		obj["function"] = map[string]any{
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
		}
	}
	return obj
}

func rawExtraMap(extra message.Extra) map[string]any {
	out := map[string]any{}
	for key, value := range extra {
		if strings.HasPrefix(key, "_lightcode_") {
			continue
		}
		out[key] = message.CloneRaw(value)
	}
	return out
}
