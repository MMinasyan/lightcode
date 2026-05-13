package compact

import (
	"encoding/json"
	"fmt"

	"github.com/MMinasyan/lightcode/internal/message"
)

const (
	placeholder    = "[tool output omitted]"
	maxOutputChars = 10_000
)

// Prune reduces token count before summarization:
// 1. All tool result contents replaced with placeholder.
// 2. For read_file calls, the last read of each path is restored.
// 3. Restored outputs truncated if over maxOutputChars.
func Prune(messages []message.Message) []message.Message {
	out := make([]message.Message, len(messages))
	copy(out, messages)

	toolCallIndex := buildToolCallIndex(out)
	lastReadIndex := lastFileReadIndexes(out, toolCallIndex)

	for i := range out {
		if out[i].Role != message.RoleTool {
			continue
		}
		if _, keep := lastReadIndex[i]; keep {
			content := out[i].TextContent()
			if len(content) > maxOutputChars {
				setTextContent(&out[i], content[:maxOutputChars]+fmt.Sprintf("\n[truncated — %d chars total]", len(messages[i].TextContent())))
			}
			continue
		}
		setTextContent(&out[i], placeholder)
	}

	return out
}

type toolCallInfo struct {
	Name string
	Args string
}

func buildToolCallIndex(msgs []message.Message) map[string]toolCallInfo {
	idx := map[string]toolCallInfo{}
	for _, m := range msgs {
		if m.Role != message.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			idx[tc.ID] = toolCallInfo{Name: tc.Function.Name, Args: tc.Function.Arguments}
		}
	}
	return idx
}

func lastFileReadIndexes(msgs []message.Message, tcIndex map[string]toolCallInfo) map[int]bool {
	lastByPath := map[string]int{}
	for i, m := range msgs {
		if m.Role != message.RoleTool {
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

func setTextContent(msg *message.Message, content string) {
	msg.Content = nil
	msg.AppendText(content)
}
