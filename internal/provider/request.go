package provider

import (
	"encoding/json"
	"net/http"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/message"
	openai "github.com/sashabaranov/go-openai"
)

// requestConfig holds all inputs for building a chat completion request body.
type requestConfig struct {
	provider      *catalog.Provider
	model         *catalog.Model
	messages      []message.Message
	tools         []openai.Tool
	runtimeExtras map[string]any
}

// requestMessages converts canonical messages to the current OpenAI-compatible
// request shape. Broad opaque Extra replay stays out of this path until Phase 5.
func requestMessages(messages []message.Message, role catalog.SystemRole) []openai.ChatCompletionMessage {
	if len(messages) == 0 {
		return []openai.ChatCompletionMessage{}
	}
	if role == "" {
		role = catalog.RoleSystem
	}
	out := make([]openai.ChatCompletionMessage, 0, len(messages))
	for _, msg := range messages {
		wire := canonicalRequestMessage(msg)
		if wire.Role == string(message.RoleSystem) {
			wire.Role = string(role)
		}
		out = append(out, wire)
	}
	return out
}

func canonicalRequestMessage(msg message.Message) openai.ChatCompletionMessage {
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
			out.ToolCalls = append(out.ToolCalls, openai.ToolCall{
				ID:   tc.ID,
				Type: openai.ToolType(tc.Type),
				Function: openai.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
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

// buildRequestBody assembles the JSON body for a streaming chat completion.
func buildRequestBody(cfg requestConfig) (map[string]any, error) {
	if cfg.provider == nil {
		return nil, ErrIncompleteModel
	}
	if cfg.model == nil || cfg.model.ID == "" {
		return nil, ErrIncompleteModel
	}
	if keys := reservedSidecarKeys(cfg.provider.ExtraBody, cfg.model.ExtraBody, cfg.runtimeExtras); len(keys) > 0 {
		return nil, &ReservedKeyError{Keys: keys}
	}

	body := map[string]any{
		"model":    cfg.model.ID,
		"messages": requestMessages(cfg.messages, effectiveSystemRole(cfg.provider, cfg.model)),
		"stream":   true,
		"n":        1,
	}
	if len(cfg.tools) > 0 {
		body["tools"] = cfg.tools
		body["tool_choice"] = "auto"
	}
	if cfg.model.UsageInStream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if cfg.model.MaxOutputTokens > 0 {
		body[maxTokensField(cfg.provider)] = cfg.model.MaxOutputTokens
	}
	return catalog.ShallowMergeBody(body, cfg.provider.ExtraBody, cfg.model.ExtraBody, cfg.runtimeExtras), nil
}

// effectiveSystemRole returns the system role to use: model overrides provider,
// provider overrides default "system".
func effectiveSystemRole(provider *catalog.Provider, model *catalog.Model) catalog.SystemRole {
	if model != nil && model.SystemRole != "" {
		return model.SystemRole
	}
	if provider != nil && provider.SystemRole != "" {
		return provider.SystemRole
	}
	return catalog.RoleSystem
}

// maxTokensField returns the JSON key for max output tokens
// ("max_tokens" by default, or provider-configured override).
func maxTokensField(provider *catalog.Provider) string {
	if provider != nil && provider.MaxTokensField == "max_completion_tokens" {
		return "max_completion_tokens"
	}
	return "max_tokens"
}

// buildHeaders returns HTTP headers for the chat completion request.
func buildHeaders(provider *catalog.Provider, apiKey string) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	if apiKey != "" {
		headers.Set("Authorization", "Bearer "+apiKey)
	}
	if provider != nil {
		for key, value := range provider.Transport.Headers {
			headers.Set(key, value)
		}
	}
	return headers
}

func reservedSidecarKeys(layers ...map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for _, layer := range layers {
		for _, key := range catalog.CheckReservedKeys(layer) {
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	return out
}
