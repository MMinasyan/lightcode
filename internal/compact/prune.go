package compact

import (
	"encoding/json"
	"fmt"

	openai "github.com/sashabaranov/go-openai"
)

const (
	placeholder    = "[tool output omitted]"
	maxOutputChars = 10_000
)

// Prune reduces token count before summarization:
// 1. All tool result contents replaced with placeholder.
// 2. For read_file calls, the last read of each path is restored.
// 3. Restored outputs truncated if over maxOutputChars.
func Prune(messages []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, len(messages))
	copy(out, messages)

	toolCallIndex := buildToolCallIndex(out)
	lastReadIndex := lastFileReadIndexes(out, toolCallIndex)

	for i := range out {
		if out[i].Role != openai.ChatMessageRoleTool {
			continue
		}
		if _, keep := lastReadIndex[i]; keep {
			if len(out[i].Content) > maxOutputChars {
				out[i].Content = out[i].Content[:maxOutputChars] + fmt.Sprintf("\n[truncated — %d chars total]", len(messages[i].Content))
			}
			continue
		}
		out[i].Content = placeholder
	}

	return out
}

type toolCallInfo struct {
	Name string
	Args string
}

func buildToolCallIndex(msgs []openai.ChatCompletionMessage) map[string]toolCallInfo {
	idx := map[string]toolCallInfo{}
	for _, m := range msgs {
		if m.Role != openai.ChatMessageRoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			idx[tc.ID] = toolCallInfo{Name: tc.Function.Name, Args: tc.Function.Arguments}
		}
	}
	return idx
}

func lastFileReadIndexes(msgs []openai.ChatCompletionMessage, tcIndex map[string]toolCallInfo) map[int]bool {
	lastByPath := map[string]int{}
	for i, m := range msgs {
		if m.Role != openai.ChatMessageRoleTool {
			continue
		}
		info, ok := tcIndex[m.ToolCallID]
		if !ok || info.Name != "read_file" {
			continue
		}
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(info.Args), &args) != nil || args.Path == "" {
			continue
		}
		lastByPath[args.Path] = i
	}
	keep := map[int]bool{}
	for _, idx := range lastByPath {
		keep[idx] = true
	}
	return keep
}
