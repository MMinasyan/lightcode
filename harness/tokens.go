package harness

import (
	_ "embed"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"

	"github.com/MMinasyan/lightcode/model"
	"github.com/pkoukk/tiktoken-go"
)

// cl100kBaseData is the bundled cl100k_base mergeable-rank table; see
// data/LICENSE for its notice.
//
//go:embed data/cl100k_base.tiktoken
var cl100kBaseData []byte

// localEncoding memoizes the cl100k_base construction over the bundled data.
// The data is fixed at build time, so a failed construction cannot repair
// itself during a process lifetime: success and failure are both latched.
var localEncoding = sync.OnceValues(func() (*tiktoken.Tiktoken, error) {
	return newLocalEncoding(cl100kBaseData)
})

// newLocalEncoding constructs the pinned tiktoken-go cl100k_base encoding from
// one rank table: every nonempty line is split into its base64 token and
// decimal rank, decoding and conversion failures are returned as they come,
// and the pinned library's construction rejects an inconsistent table. The
// bundled digest-pinned asset is the only production input, so no further
// table validation is imposed.
func newLocalEncoding(data []byte) (*tiktoken.Tiktoken, error) {
	ranks := make(map[string]int)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		tokenText, rankText, _ := strings.Cut(line, " ")
		token, err := base64.StdEncoding.DecodeString(tokenText)
		if err != nil {
			return nil, err
		}
		rank, err := strconv.Atoi(rankText)
		if err != nil {
			return nil, err
		}
		ranks[string(token)] = rank
	}
	encoding := &tiktoken.Encoding{
		Name:           tiktoken.MODEL_CL100K_BASE,
		PatStr:         `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`,
		MergeableRanks: ranks,
		SpecialTokens: map[string]int{
			tiktoken.ENDOFTEXT:   100257,
			tiktoken.FIM_PREFIX:  100258,
			tiktoken.FIM_MIDDLE:  100259,
			tiktoken.FIM_SUFFIX:  100260,
			tiktoken.ENDOFPROMPT: 100276,
		},
	}
	bpe, err := tiktoken.NewCoreBPE(encoding.MergeableRanks, encoding.SpecialTokens, encoding.PatStr)
	if err != nil {
		return nil, err
	}
	specialTokensSet := make(map[string]any, len(encoding.SpecialTokens))
	for token := range encoding.SpecialTokens {
		specialTokensSet[token] = true
	}
	return tiktoken.NewTiktoken(bpe, encoding, specialTokensSet), nil
}

// estimateTokensWithEncoding owns the token arithmetic over one encoder: per
// message, the message's text content plus each tool call's name and raw
// arguments, plus 4; the system prompt is estimated as plain text. A nil
// encoder selects the characters/3 fallback over the same accumulated text.
func estimateTokensWithEncoding(systemPrompt string, messages []model.Message, enc *tiktoken.Tiktoken) int {
	if enc == nil {
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

// estimateTokens estimates the token count of the assembled context with the
// local cl100k_base encoding; a construction failure yields a nil encoder,
// which selects the characters/3 fallback.
func estimateTokens(systemPrompt string, messages []model.Message) int {
	enc, _ := localEncoding()
	return estimateTokensWithEncoding(systemPrompt, messages, enc)
}
