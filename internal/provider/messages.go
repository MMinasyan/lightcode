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
		obj, err := serializeMessage(msg, systemRole, shouldReplayExtra(msg, target, provider))
		if err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	return out, nil
}

func serializeMessage(msg message.Message, systemRole catalog.SystemRole, keepExtra bool) (map[string]any, error) {
	obj := rawExtraMap(msg.Extra, keepExtra, messageExtraAllowed)
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
		content, ok, err := serializeContent(msg.Content, keepExtra)
		if err != nil {
			return nil, err
		}
		if ok {
			obj["content"] = content
		}
	}
	if msg.Refusal != "" {
		obj["refusal"] = msg.Refusal
	}
	if len(msg.ToolCalls) > 0 {
		toolCalls := make([]map[string]any, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			toolCalls = append(toolCalls, serializeToolCall(tc, keepExtra))
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

func serializeContent(parts []message.ContentPart, keepExtra bool) (any, bool, error) {
	if len(parts) == 1 && parts[0].Type == message.ContentPartText {
		extra := rawExtraMap(parts[0].Extra, keepExtra, contentPartExtraAllowed)
		if len(extra) == 0 {
			return parts[0].Text, true, nil
		}
	}
	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		allowed := contentPartExtraAllowed
		if part.Type == message.ContentPartOpaque {
			allowed = opaqueContentPartExtraAllowed
		}
		obj := rawExtraMap(part.Extra, keepExtra, allowed)
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
		if part.Type == message.ContentPartOpaque && !keepExtra {
			continue
		}
		out = append(out, obj)
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	return out, true, nil
}

func serializeToolCall(tc message.ToolCall, keepExtra bool) map[string]any {
	obj := rawExtraMap(tc.Extra, keepExtra, toolCallExtraAllowed)
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

func shouldReplayExtra(msg message.Message, target *catalog.Model, provider *catalog.Provider) bool {
	if target == nil || provider == nil {
		return false
	}
	return msg.Source.Provider != "" &&
		msg.Source.Provider == provider.ID &&
		msg.Source.Model == target.ID
}

func rawExtraMap(extra message.Extra, keep bool, allowed func(string) bool) map[string]any {
	out := map[string]any{}
	if !keep {
		return out
	}
	for key, value := range extra {
		if !allowed(key) {
			continue
		}
		out[key] = message.CloneRaw(value)
	}
	return out
}

func globalExtraAllowed(key string) bool {
	if key == "" || strings.HasPrefix(key, "_lightcode_") {
		return false
	}
	for _, reserved := range catalog.ReservedKeys {
		if key == reserved {
			return false
		}
	}
	return true
}

func messageExtraAllowed(key string) bool {
	if !globalExtraAllowed(key) {
		return false
	}
	switch key {
	case "role", "content", "refusal", "tool_calls", "tool_call_id", "name", "function_call",
		"system_fingerprint", "service_tier", "id", "created", "object", "model":
		return false
	default:
		return true
	}
}

func toolCallExtraAllowed(key string) bool {
	if !globalExtraAllowed(key) {
		return false
	}
	switch key {
	case "id", "type", "function", "index":
		return false
	default:
		return true
	}
}

func contentPartExtraAllowed(key string) bool {
	if !globalExtraAllowed(key) {
		return false
	}
	switch key {
	case "type", "text", "image_url":
		return false
	default:
		return true
	}
}

func opaqueContentPartExtraAllowed(key string) bool {
	return globalExtraAllowed(key)
}
