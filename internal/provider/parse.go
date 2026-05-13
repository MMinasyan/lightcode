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
}

// ParseChunk parses one raw SSE JSON payload into the Phase 2 canonical-field
// view. Extra capture is intentionally stubbed until Phase 5.
func ParseChunk(raw json.RawMessage) (StreamDelta, error) {
	var typed openai.ChatCompletionStreamResponse
	if err := json.Unmarshal(raw, &typed); err != nil {
		return StreamDelta{}, err
	}
	out := StreamDelta{Usage: typed.Usage}
	if len(typed.Choices) == 0 {
		return out, nil
	}
	choice := typed.Choices[0]
	out.HasChoice = true
	out.FinishReason = choice.FinishReason
	out.Role = choice.Delta.Role
	out.Content = choice.Delta.Content
	out.Refusal = choice.Delta.Refusal
	out.ToolCalls = choice.Delta.ToolCalls
	return out, nil
}
