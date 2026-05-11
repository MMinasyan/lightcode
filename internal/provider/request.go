package provider

import (
	"net/http"

	"github.com/MMinasyan/lightcode/internal/catalog"
	openai "github.com/sashabaranov/go-openai"
)

// requestConfig holds all inputs for building a chat completion request body.
type requestConfig struct {
	provider      *catalog.Provider
	model         *catalog.Model
	messages      []openai.ChatCompletionMessage
	tools         []openai.Tool
	runtimeExtras map[string]any
}

// applySystemRole walks messages and rewrites system-role messages to use
// the effective system role from model/provider defaults.
func applySystemRole(messages []openai.ChatCompletionMessage, role catalog.SystemRole) []openai.ChatCompletionMessage {
	if len(messages) == 0 {
		return []openai.ChatCompletionMessage{}
	}
	if role == "" {
		role = catalog.RoleSystem
	}
	adjusted := make([]openai.ChatCompletionMessage, len(messages))
	copy(adjusted, messages)
	for i := range adjusted {
		if adjusted[i].Role == "system" {
			adjusted[i].Role = string(role)
		}
	}
	return adjusted
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
		"messages": applySystemRole(cfg.messages, effectiveSystemRole(cfg.provider, cfg.model)),
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
