package provider

import (
	"encoding/json"
	"strings"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/message"
	openai "github.com/sashabaranov/go-openai"
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

// OpenAIToCanonical converts an SDK message into Lightcode's canonical shape.
// It is a temporary shim for packages that still hold go-openai messages.
func OpenAIToCanonical(msg openai.ChatCompletionMessage) message.Message {
	out := message.Message{
		Role:       message.Role(msg.Role),
		Refusal:    msg.Refusal,
		ToolCallID: msg.ToolCallID,
		Name:       msg.Name,
	}
	if len(msg.MultiContent) > 0 {
		for _, part := range msg.MultiContent {
			switch part.Type {
			case openai.ChatMessagePartTypeText:
				out.Content = append(out.Content, message.ContentPart{Type: message.ContentPartText, Text: part.Text})
			case openai.ChatMessagePartTypeImageURL:
				url := ""
				if part.ImageURL != nil {
					url = part.ImageURL.URL
				}
				out.Content = append(out.Content, message.ContentPart{Type: message.ContentPartImageURL, URL: url})
			default:
				out.Content = append(out.Content, message.ContentPart{
					Type: message.ContentPartOpaque,
					Extra: message.Extra{
						"type": mustRaw(string(part.Type)),
					},
				})
			}
		}
	} else if msg.Content != "" {
		out.Content = []message.ContentPart{{Type: message.ContentPartText, Text: msg.Content}}
	}
	if len(msg.ToolCalls) > 0 {
		out.ToolCalls = make([]message.ToolCall, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, openAIToolCallToCanonical(tc))
		}
	}
	if msg.ReasoningContent != "" {
		out.Extra = message.Extra{"reasoning_content": mustRaw(msg.ReasoningContent)}
	}
	return out
}

// CanonicalToOpenAI converts a canonical message into the SDK message shape.
// It is a temporary shim for packages that have not moved to message.Message.
func CanonicalToOpenAI(msg message.Message) openai.ChatCompletionMessage {
	out := openai.ChatCompletionMessage{
		Role:       string(msg.Role),
		Refusal:    msg.Refusal,
		ToolCallID: msg.ToolCallID,
		Name:       msg.Name,
	}
	if len(msg.Content) == 1 && msg.Content[0].Type == message.ContentPartText && len(msg.Content[0].Extra) == 0 {
		out.Content = msg.Content[0].Text
	} else if len(msg.Content) > 0 {
		out.MultiContent = make([]openai.ChatMessagePart, 0, len(msg.Content))
		for _, part := range msg.Content {
			switch part.Type {
			case message.ContentPartText:
				out.MultiContent = append(out.MultiContent, openai.ChatMessagePart{Type: openai.ChatMessagePartTypeText, Text: part.Text})
			case message.ContentPartImageURL:
				out.MultiContent = append(out.MultiContent, openai.ChatMessagePart{
					Type:     openai.ChatMessagePartTypeImageURL,
					ImageURL: &openai.ChatMessageImageURL{URL: part.URL},
				})
			}
		}
	}
	if len(msg.ToolCalls) > 0 {
		out.ToolCalls = make([]openai.ToolCall, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, canonicalToolCallToOpenAI(tc))
		}
	}
	if raw := msg.Extra["reasoning_content"]; len(raw) > 0 {
		var reasoning string
		if json.Unmarshal(raw, &reasoning) == nil {
			out.ReasoningContent = reasoning
		}
	}
	return out
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

func openAIToolCallToCanonical(tc openai.ToolCall) message.ToolCall {
	return message.ToolCall{
		ID:   tc.ID,
		Type: string(tc.Type),
		Function: message.FunctionCall{
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		},
	}
}

func canonicalToolCallToOpenAI(tc message.ToolCall) openai.ToolCall {
	return openai.ToolCall{
		ID:   tc.ID,
		Type: openai.ToolType(tc.Type),
		Function: openai.FunctionCall{
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		},
	}
}

func mustRaw(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
