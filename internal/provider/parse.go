package provider

import (
	"encoding/json"

	"github.com/MMinasyan/lightcode/internal/message"
	openai "github.com/sashabaranov/go-openai"
)

// StreamDelta is the provider-owned parsed view of one streaming chunk.
type StreamDelta struct {
	Role         string
	Content      string
	Refusal      string
	ToolCalls    []openai.ToolCall
	FinishReason openai.FinishReason
	Usage        *openai.Usage
	HasChoice    bool

	MessageExtra     message.Extra
	ToolCallExtra    map[int]message.Extra
	ContentPartExtra map[int]message.Extra
	ContentParts     []message.ContentPart
}

type rawStreamResponse struct {
	Choices []rawStreamChoice `json:"choices"`
	Usage   *openai.Usage     `json:"usage,omitempty"`
}

type rawStreamChoice struct {
	Delta        map[string]json.RawMessage `json:"delta"`
	FinishReason openai.FinishReason        `json:"finish_reason"`
}

// ParseChunk parses one raw SSE JSON payload into canonical deltas plus
// provider-specific Extra captured at message, tool-call, and content scopes.
func ParseChunk(raw json.RawMessage) (StreamDelta, error) {
	var parsed rawStreamResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return StreamDelta{}, err
	}
	out := StreamDelta{Usage: parsed.Usage}
	if len(parsed.Choices) == 0 {
		return out, nil
	}
	choice := parsed.Choices[0]
	out.HasChoice = true
	out.FinishReason = choice.FinishReason
	delta := choice.Delta
	if len(delta) == 0 {
		return out, nil
	}
	out.Role = rawString(delta["role"])
	out.Refusal = rawString(delta["refusal"])
	out.MessageExtra = extraFromRawObject(delta, isCanonicalMessageDeltaField)

	if rawContent := delta["content"]; len(rawContent) > 0 {
		if content, ok := decodeRawString(rawContent); ok {
			out.Content = content
		} else {
			var parts []message.ContentPart
			if err := json.Unmarshal(rawContent, &parts); err != nil {
				return StreamDelta{}, err
			}
			out.ContentParts = parts
			for i, part := range parts {
				if len(part.Extra) == 0 {
					continue
				}
				if out.ContentPartExtra == nil {
					out.ContentPartExtra = map[int]message.Extra{}
				}
				out.ContentPartExtra[i] = part.Extra.Clone()
			}
		}
	}

	if rawToolCalls := delta["tool_calls"]; len(rawToolCalls) > 0 {
		var calls []openai.ToolCall
		if err := json.Unmarshal(rawToolCalls, &calls); err != nil {
			return StreamDelta{}, err
		}
		out.ToolCalls = calls
		var rawCalls []map[string]json.RawMessage
		if err := json.Unmarshal(rawToolCalls, &rawCalls); err != nil {
			return StreamDelta{}, err
		}
		for pos, rawCall := range rawCalls {
			extra := extraFromRawObject(rawCall, isCanonicalToolCallDeltaField)
			if len(extra) == 0 {
				continue
			}
			idx := toolCallIndex(rawCall, pos)
			if out.ToolCallExtra == nil {
				out.ToolCallExtra = map[int]message.Extra{}
			}
			out.ToolCallExtra[idx] = extra
		}
	}
	return out, nil
}

func toolCallIndex(raw map[string]json.RawMessage, fallback int) int {
	if value, ok := raw["index"]; ok {
		var idx int
		if err := json.Unmarshal(value, &idx); err == nil {
			return idx
		}
	}
	return fallback
}

func rawString(raw json.RawMessage) string {
	value, _ := decodeRawString(raw)
	return value
}

func decodeRawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func extraFromRawObject(raw map[string]json.RawMessage, canonical func(string) bool) message.Extra {
	if len(raw) == 0 {
		return nil
	}
	extra := message.Extra{}
	for key, value := range raw {
		if canonical(key) {
			continue
		}
		extra[key] = message.CloneRaw(value)
	}
	if len(extra) == 0 {
		return nil
	}
	return extra
}

func isCanonicalMessageDeltaField(key string) bool {
	switch key {
	case "role", "content", "name", "tool_calls", "tool_call_id", "refusal", "function_call":
		return true
	default:
		return false
	}
}

func isCanonicalToolCallDeltaField(key string) bool {
	switch key {
	case "id", "type", "function", "index":
		return true
	default:
		return false
	}
}
