package harness

import (
	"sync"

	"github.com/MMinasyan/lightcode/model"
	"github.com/pkoukk/tiktoken-go"
)

// loadEncoding is the one encoding-acquisition seam: a plain function so a
// failed acquisition is retried on the next estimate instead of latching.
// Tests override it to force the characters/3 fallback deterministically.
var loadEncoding = func() (*tiktoken.Tiktoken, error) {
	return tiktoken.GetEncoding("cl100k_base")
}

// cachedEncoderMu guards the cached encoder, and cachedEncoder holds the one
// successfully loaded cl100k_base encoding — tiktoken-go caches nothing and
// a per-call acquisition costs tens of milliseconds on every token estimate.
var (
	cachedEncoderMu sync.Mutex
	cachedEncoder   *tiktoken.Tiktoken
)

// cachedEncoding returns the process's encoder: a cache hit returns it, a
// miss consults the seam and latches only its success — a failed load
// returns its error without caching and is retried on the next estimate.
func cachedEncoding() (*tiktoken.Tiktoken, error) {
	cachedEncoderMu.Lock()
	defer cachedEncoderMu.Unlock()
	if cachedEncoder != nil {
		return cachedEncoder, nil
	}
	enc, err := loadEncoding()
	if err != nil {
		return nil, err
	}
	cachedEncoder = enc
	return enc, nil
}

// estimateTokens estimates the token count of the assembled context using
// cl100k_base, falling back to characters/3 when the encoder is unavailable.
// Per message: the message's text content plus each tool call's name and
// arguments, plus 4. The system prompt is estimated as plain text.
func estimateTokens(systemPrompt string, messages []model.Message) int {
	enc, err := cachedEncoding()
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
