package compact

import (
	openai "github.com/sashabaranov/go-openai"
	"github.com/pkoukk/tiktoken-go"
)

// CountTokens estimates the token count of messages using cl100k_base.
// Returns the raw count — callers apply a safety margin.
func CountTokens(messages []openai.ChatCompletionMessage) int {
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		return estimateByChars(messages)
	}
	total := 0
	for _, m := range messages {
		total += len(enc.Encode(m.Content, nil, nil))
		for _, tc := range m.ToolCalls {
			total += len(enc.Encode(tc.Function.Name, nil, nil))
			total += len(enc.Encode(tc.Function.Arguments, nil, nil))
		}
		total += 4 // per-message overhead (role, separators)
	}
	return total
}

func estimateByChars(messages []openai.ChatCompletionMessage) int {
	chars := 0
	for _, m := range messages {
		chars += len(m.Content)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return chars / 3
}
