package harness

import (
	"github.com/MMinasyan/lightcode/model"
	"github.com/pkoukk/tiktoken-go"
)

// encodingFor is the one encoding-acquisition seam: nil-free in production
// and overridden by tests to force the characters/3 fallback deterministically.
var encodingFor = tiktoken.GetEncoding

// estimateTokens estimates the token count of the assembled context using
// cl100k_base, falling back to characters/3 when the encoder is unavailable.
// Per message: the message's text content plus each tool call's name and
// arguments, plus 4. The system prompt is estimated as plain text.
func estimateTokens(systemPrompt string, messages []model.Message) int {
	enc, err := encodingFor("cl100k_base")
	if err != nil {
		chars := len(systemPrompt)
		for _, m := range messages {
			chars += len(m.TextContent())
			for _, tc := range m.ToolCalls {
				chars += len(tc.Name) + len(tc.Arguments)
			}
		}
		return chars / 3
	}
	total := len(enc.Encode(systemPrompt, nil, nil))
	for _, m := range messages {
		total += len(enc.Encode(m.TextContent(), nil, nil))
		for _, tc := range m.ToolCalls {
			total += len(enc.Encode(tc.Name, nil, nil))
			total += len(enc.Encode(string(tc.Arguments), nil, nil))
		}
		total += 4 // per-message overhead (role, separators)
	}
	return total
}
