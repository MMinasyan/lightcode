package provider

import (
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/engine/modelclient"
)

// ProtocolWarning describes a non-fatal protocol metadata diagnostic.
type ProtocolWarning = modelclient.ProtocolWarning

// SerializeMessages converts canonical messages to the OpenAI-compatible
// message maps used in provider request bodies.
func SerializeMessages(history []message.Message, target *catalog.Model, provider *catalog.Provider) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(history))
	systemRole := effectiveSystemRole(provider, target)
	for _, msg := range history {
		policy := replayPolicyFor(msg, target, provider)
		obj, err := serializeMessage(msg, systemRole, policy)
		if err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	return out, nil
}

// ProtocolWarnings returns request-build diagnostics for protocol metadata.
func ProtocolWarnings(history []message.Message, target *catalog.Model, provider *catalog.Provider) []ProtocolWarning {
	meta := protocolMetadata(target)
	if meta == nil || len(meta.MustPreserve) == 0 || provider == nil || target == nil {
		return nil
	}
	var out []ProtocolWarning
	for i, msg := range history {
		if msg.Role != message.RoleAssistant || len(msg.ToolCalls) == 0 {
			continue
		}
		if !replayPolicyFor(msg, target, provider).keep {
			continue
		}
		for _, field := range meta.MustPreserve {
			if _, ok := msg.Extra[field]; ok {
				continue
			}
			out = append(out, ProtocolWarning{
				Kind:         "protocol_must_preserve_missing",
				Provider:     provider.ID,
				Model:        target.ID,
				Field:        field,
				MessageIndex: i,
				Message:      fmt.Sprintf("%s/%s assistant message %d has tool calls but is missing protocol metadata field %q", provider.ID, target.ID, i, field),
			})
		}
	}
	return out
}

func serializeMessage(msg message.Message, systemRole catalog.SystemRole, policy replayPolicy) (map[string]any, error) {
	obj := rawExtraMap(msg.Extra, policy, messageExtraAllowed)
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
		content, ok, err := serializeContent(msg.Content, policy)
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
			toolCalls = append(toolCalls, serializeToolCall(tc, policy))
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

func serializeContent(parts []message.ContentPart, policy replayPolicy) (any, bool, error) {
	if len(parts) == 1 && parts[0].Type == message.ContentPartText {
		extra := rawExtraMap(parts[0].Extra, policy, contentPartExtraAllowed)
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
		obj := rawExtraMap(part.Extra, policy, allowed)
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
		if part.Type == message.ContentPartOpaque && !policy.keep {
			continue
		}
		out = append(out, obj)
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	return out, true, nil
}

func serializeToolCall(tc message.ToolCall, policy replayPolicy) map[string]any {
	obj := rawExtraMap(tc.Extra, policy, toolCallExtraAllowed)
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

type replayPolicy struct {
	keep bool
	drop map[string]bool
}

func replayPolicyFor(msg message.Message, target *catalog.Model, provider *catalog.Provider) replayPolicy {
	policy := replayPolicy{drop: protocolDropSet(target)}
	if target == nil || provider == nil {
		return policy
	}
	if msg.Source.Provider == "" || msg.Source.Provider != provider.ID {
		return policy
	}
	if msg.Source.Model == target.ID {
		policy.keep = true
		return policy
	}
	source := provider.Models[msg.Source.Model]
	sourceFamily := protocolFamily(source)
	targetFamily := protocolFamily(target)
	if sourceFamily != "" && sourceFamily == targetFamily {
		policy.keep = true
	}
	return policy
}

func protocolMetadata(model *catalog.Model) *catalog.ProtocolMetadata {
	if model == nil {
		return nil
	}
	return model.ProtocolMetadata
}

func protocolFamily(model *catalog.Model) string {
	meta := protocolMetadata(model)
	if meta == nil {
		return ""
	}
	return meta.Family
}

func protocolDropSet(model *catalog.Model) map[string]bool {
	meta := protocolMetadata(model)
	if meta == nil || len(meta.Drop) == 0 {
		return nil
	}
	out := make(map[string]bool, len(meta.Drop))
	for _, key := range meta.Drop {
		out[key] = true
	}
	return out
}

func rawExtraMap(extra message.Extra, policy replayPolicy, allowed func(string) bool) map[string]any {
	out := map[string]any{}
	if !policy.keep {
		return out
	}
	for key, value := range extra {
		if policy.drop[key] || !allowed(key) {
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
