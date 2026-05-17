package provider

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func FuzzParseChunk(f *testing.F) {
	for _, path := range []string{
		"testdata/zai_reasoning_content.sse",
		"testdata/openrouter_reasoning_details.sse",
		"testdata/gemini_tool_extra.sse",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatalf("read fixture %s: %v", path, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			f.Add([]byte(line))
		}
	}

	for _, seed := range []string{
		`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3},"choices":[{"index":0,"delta":{"role":"assistant","content":"hello","refusal":"no","reasoning":"hidden","reasoning_details":[{"type":"reasoning.text","text":"a"}],"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"},"extra_content":{"google":{"thought_signature":"sig"}}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"id":"call_omitted","type":"function","function":{"name":"read_file","arguments":"{}"},"extra_content":{"source":"omitted"}},{"index":0,"id":"call_explicit","type":"function","function":{"name":"write_file","arguments":"{}"},"extra_content":{"source":"explicit"}}]}}]}`,
		`{"choices":[{"delta":{"content":[{"type":"text","text":"visible","signature":"text-sig"},{"type":"thinking","thinking":"hidden","signature":"opaque-sig"}]}}]}`,
		`{not-json}`,
		`{}`,
		`[]`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		delta, err := ParseChunk(json.RawMessage(raw))
		if err != nil {
			return
		}
		if delta.ToolCallExtra != nil && len(delta.ToolCalls) == 0 {
			t.Fatalf("tool-call extras without parsed tool calls: %#v", delta.ToolCallExtra)
		}
		if len(delta.ToolCallExtra) > 0 {
			valid := map[int]bool{}
			for i := range delta.ToolCalls {
				if delta.ToolCalls[i].Index != nil {
					valid[*delta.ToolCalls[i].Index] = true
				}
			}
			for key := range delta.ToolCallExtra {
				if !valid[key] {
					t.Fatalf("ToolCallExtra key %d has no matching tool-call index", key)
				}
			}
		}
		if delta.ContentPartExtra != nil && len(delta.ContentParts) == 0 {
			t.Fatalf("content-part extras without parsed content parts: %#v", delta.ContentPartExtra)
		}
	})
}
