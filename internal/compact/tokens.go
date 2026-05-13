package compact

import (
	"github.com/MMinasyan/lightcode/internal/message"
	"github.com/pkoukk/tiktoken-go"
)

// CountTokens estimates the token count of messages using cl100k_base.
// Returns the raw count — callers apply a safety margin.
func CountTokens(messages []message.Message) int {
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		return estimateByChars(messages)
	}
	total := 0
	for _, m := range messages {
		total += len(enc.Encode(m.TextContent(), nil, nil))
		for _, tc := range m.ToolCalls {
			total += len(enc.Encode(tc.Function.Name, nil, nil))
			total += len(enc.Encode(tc.Function.Arguments, nil, nil))
		}
		total += 4 // per-message overhead (role, separators)
	}
	return total
}

func estimateByChars(messages []message.Message) int {
	chars := 0
	for _, m := range messages {
		chars += len(m.TextContent())
		for _, tc := range m.ToolCalls {
			chars += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return chars / 3
}
