package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/message"
	openai "github.com/sashabaranov/go-openai"
)

func TestSerializeMessagesEmitsExtrasAndSystemRole(t *testing.T) {
	source := catalog.ModelRef{Provider: "test", Model: "model-a"}
	history := []message.Message{
		message.NewText(message.RoleSystem, "instructions"),
		{
			Role:    message.RoleAssistant,
			Content: []message.ContentPart{{Type: message.ContentPartText, Text: "using tool"}},
			Refusal: "cannot",
			ToolCalls: []message.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: message.FunctionCall{Name: "lookup", Arguments: `{}`},
				Extra:    message.Extra{"extra_content": json.RawMessage(`{"google":{"thought_signature":"sig"}}`)},
			}},
			Extra: message.Extra{
				"reasoning_content": json.RawMessage(`"think"`),
				"_lightcode_debug":  json.RawMessage(`true`),
			},
			Source: source,
		},
	}
	prov := testProvider("https://example.com/v1", "KEY_ENV", "model-a")
	model := prov.Models["model-a"]
	model.SystemRole = catalog.RoleDeveloper

	out, err := SerializeMessages(history, model, prov)
	if err != nil {
		t.Fatalf("SerializeMessages returned error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("messages len = %d", len(out))
	}
	if out[0]["role"] != "developer" || out[0]["content"] != "instructions" {
		t.Fatalf("system message = %#v", out[0])
	}
	assistant := out[1]
	if string(assistant["reasoning_content"].(json.RawMessage)) != `"think"` {
		t.Fatalf("reasoning_content = %#v", assistant["reasoning_content"])
	}
	if assistant["refusal"] != "cannot" {
		t.Fatalf("refusal = %#v", assistant["refusal"])
	}
	if _, ok := assistant["_lightcode_debug"]; ok {
		t.Fatalf("private key leaked: %#v", assistant)
	}
	toolCalls := assistant["tool_calls"].([]map[string]any)
	extra := toolCalls[0]["extra_content"].(json.RawMessage)
	if string(extra) != `{"google":{"thought_signature":"sig"}}` {
		t.Fatalf("tool extra = %s", extra)
	}
}

func TestSerializeMessagesFiltersExtrasBySourceAndDenylist(t *testing.T) {
	prov := testProvider("https://example.com/v1", "KEY_ENV", "model-a")
	model := prov.Models["model-a"]
	sameSource := catalog.ModelRef{Provider: "test", Model: "model-a"}
	otherModel := catalog.ModelRef{Provider: "test", Model: "model-b"}
	otherProvider := catalog.ModelRef{Provider: "other", Model: "model-a"}

	history := []message.Message{
		{
			Role:    message.RoleAssistant,
			Content: []message.ContentPart{{Type: message.ContentPartText, Text: "same"}},
			Extra: message.Extra{
				"reasoning_details":  json.RawMessage(`[{"type":"reasoning.text","text":"keep"}]`),
				"system_fingerprint": json.RawMessage(`"drop"`),
				"model":              json.RawMessage(`"drop"`),
				"_lightcode_source":  json.RawMessage(`"drop"`),
			},
			Source: sameSource,
		},
		{Role: message.RoleAssistant, Extra: message.Extra{"reasoning_details": json.RawMessage(`[{"text":"drop"}]`)}, Source: otherModel},
		{Role: message.RoleAssistant, Extra: message.Extra{"reasoning_details": json.RawMessage(`[{"text":"drop"}]`)}, Source: otherProvider},
		{Role: message.RoleAssistant, Extra: message.Extra{"reasoning_details": json.RawMessage(`[{"text":"drop"}]`)}},
	}

	out, err := SerializeMessages(history, model, prov)
	if err != nil {
		t.Fatalf("SerializeMessages returned error: %v", err)
	}
	if string(out[0]["reasoning_details"].(json.RawMessage)) != `[{"type":"reasoning.text","text":"keep"}]` {
		t.Fatalf("same-source extra = %#v", out[0])
	}
	for _, key := range []string{"system_fingerprint", "model", "_lightcode_source"} {
		if _, ok := out[0][key]; ok {
			t.Fatalf("denied key %q leaked in %#v", key, out[0])
		}
	}
	for i := 1; i < len(out); i++ {
		if _, ok := out[i]["reasoning_details"]; ok {
			t.Fatalf("cross-scope extra leaked in message %d: %#v", i, out[i])
		}
	}
}

func TestSerializeMessagesUsesProtocolMetadataFamilyAndDrop(t *testing.T) {
	prov := testProvider("https://example.com/v1", "KEY_ENV", "model-a")
	model := prov.Models["model-a"]
	model.ProtocolMetadata = &catalog.ProtocolMetadata{Family: "reasoning-family", Drop: []string{"drop_me"}}
	prov.Models["model-b"] = &catalog.Model{ID: "model-b", ProtocolMetadata: &catalog.ProtocolMetadata{Family: "reasoning-family"}}
	prov.Models["model-c"] = &catalog.Model{ID: "model-c", ProtocolMetadata: &catalog.ProtocolMetadata{Family: "other-family"}}

	history := []message.Message{
		{
			Role: message.RoleAssistant,
			Extra: message.Extra{
				"reasoning_details": json.RawMessage(`[{"text":"same-family"}]`),
				"drop_me":           json.RawMessage(`"drop"`),
			},
			Source: catalog.ModelRef{Provider: "test", Model: "model-b"},
		},
		{
			Role:   message.RoleAssistant,
			Extra:  message.Extra{"reasoning_details": json.RawMessage(`[{"text":"other-family"}]`)},
			Source: catalog.ModelRef{Provider: "test", Model: "model-c"},
		},
	}

	out, err := SerializeMessages(history, model, prov)
	if err != nil {
		t.Fatalf("SerializeMessages returned error: %v", err)
	}
	if string(out[0]["reasoning_details"].(json.RawMessage)) != `[{"text":"same-family"}]` {
		t.Fatalf("same-family extra = %#v", out[0])
	}
	if _, ok := out[0]["drop_me"]; ok {
		t.Fatalf("drop field leaked: %#v", out[0])
	}
	if _, ok := out[1]["reasoning_details"]; ok {
		t.Fatalf("different-family extra leaked: %#v", out[1])
	}
}

func TestProtocolWarningsReportsMissingMustPreserve(t *testing.T) {
	prov := testProvider("https://example.com/v1", "KEY_ENV", "model-a")
	model := prov.Models["model-a"]
	model.ProtocolMetadata = &catalog.ProtocolMetadata{MustPreserve: []string{"reasoning_content"}}

	warnings := ProtocolWarnings([]message.Message{
		{
			Role: message.RoleAssistant,
			ToolCalls: []message.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: message.FunctionCall{Name: "lookup", Arguments: `{}`},
			}},
			Source: catalog.ModelRef{Provider: "test", Model: "model-a"},
		},
		{
			Role:      message.RoleAssistant,
			ToolCalls: []message.ToolCall{{ID: "call_2", Type: "function"}},
			Extra:     message.Extra{"reasoning_content": json.RawMessage(`"ok"`)},
			Source:    catalog.ModelRef{Provider: "test", Model: "model-a"},
		},
	}, model, prov)

	if len(warnings) != 1 {
		t.Fatalf("warnings = %#v, want one missing-field warning", warnings)
	}
	if warnings[0].Kind != "protocol_must_preserve_missing" || warnings[0].Field != "reasoning_content" || warnings[0].MessageIndex != 0 {
		t.Fatalf("warning = %#v", warnings[0])
	}
}

func TestSerializeMessagesContentPartExtrasAndOpaqueParts(t *testing.T) {
	prov := testProvider("https://example.com/v1", "KEY_ENV", "model-a")
	model := prov.Models["model-a"]
	source := catalog.ModelRef{Provider: "test", Model: "model-a"}

	out, err := SerializeMessages([]message.Message{{
		Role: message.RoleAssistant,
		Content: []message.ContentPart{
			{
				Type:  message.ContentPartText,
				Text:  "visible",
				Extra: message.Extra{"signature": json.RawMessage(`"text-sig"`)},
			},
			{
				Type: message.ContentPartOpaque,
				Extra: message.Extra{
					"type":      json.RawMessage(`"thinking"`),
					"thinking":  json.RawMessage(`"hidden"`),
					"signature": json.RawMessage(`"opaque-sig"`),
				},
			},
		},
		Source: source,
	}}, model, prov)
	if err != nil {
		t.Fatalf("SerializeMessages returned error: %v", err)
	}
	parts := out[0]["content"].([]map[string]any)
	if len(parts) != 2 {
		t.Fatalf("content parts = %#v", parts)
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "visible" || string(parts[0]["signature"].(json.RawMessage)) != `"text-sig"` {
		t.Fatalf("text part = %#v", parts[0])
	}
	if string(parts[1]["type"].(json.RawMessage)) != `"thinking"` || string(parts[1]["thinking"].(json.RawMessage)) != `"hidden"` || string(parts[1]["signature"].(json.RawMessage)) != `"opaque-sig"` {
		t.Fatalf("opaque part = %#v", parts[1])
	}

	out, err = SerializeMessages([]message.Message{{
		Role:    message.RoleAssistant,
		Content: []message.ContentPart{{Type: message.ContentPartText, Text: "visible", Extra: message.Extra{"signature": json.RawMessage(`"drop"`)}}},
		Source:  catalog.ModelRef{Provider: "other", Model: "model-a"},
	}}, model, prov)
	if err != nil {
		t.Fatalf("SerializeMessages returned error: %v", err)
	}
	if out[0]["content"] != "visible" {
		t.Fatalf("cross-provider text content = %#v, want string visible", out[0]["content"])
	}
}

func TestParseChunkCapturesReasoningAndToolCallExtras(t *testing.T) {
	raw := json.RawMessage(`{"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7},"choices":[{"index":0,"delta":{"role":"assistant","content":"hi","refusal":"no","reasoning":"thinking","reasoning_details":[{"type":"reasoning.text","text":"a"}],"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"},"extra_content":{"google":{"thought_signature":"sig"}}}]},"finish_reason":"tool_calls"}]}`)

	delta, err := ParseChunk(raw)
	if err != nil {
		t.Fatalf("ParseChunk returned error: %v", err)
	}
	if !delta.HasChoice || delta.Role != "assistant" || delta.Content != "hi" || delta.Refusal != "no" {
		t.Fatalf("delta = %#v", delta)
	}
	if delta.FinishReason != openai.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q", delta.FinishReason)
	}
	if delta.Usage == nil || delta.Usage.PromptTokens != 3 || delta.Usage.CompletionTokens != 4 {
		t.Fatalf("usage = %#v", delta.Usage)
	}
	if len(delta.ToolCalls) != 1 || delta.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool calls = %#v", delta.ToolCalls)
	}
	if string(delta.MessageExtra["reasoning"]) != `"thinking"` {
		t.Fatalf("reasoning = %s", delta.MessageExtra["reasoning"])
	}
	if string(delta.MessageExtra["reasoning_details"]) != `[{"type":"reasoning.text","text":"a"}]` {
		t.Fatalf("reasoning_details = %s", delta.MessageExtra["reasoning_details"])
	}
	if string(delta.ToolCallExtra[0]["extra_content"]) != `{"google":{"thought_signature":"sig"}}` {
		t.Fatalf("tool call extra = %#v", delta.ToolCallExtra)
	}
	if _, ok := delta.MessageExtra["content"]; ok {
		t.Fatalf("canonical content captured as extra: %#v", delta.MessageExtra)
	}
}

func TestParseChunkToolCallExtraIndexingAvoidsExplicitCollision(t *testing.T) {
	raw := json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"id":"call_omitted","type":"function","function":{"name":"read_file","arguments":"{}"},"extra_content":{"source":"omitted"}},{"index":0,"id":"call_explicit","type":"function","function":{"name":"write_file","arguments":"{}"},"extra_content":{"source":"explicit"}}]}}]}`)

	delta, err := ParseChunk(raw)
	if err != nil {
		t.Fatalf("ParseChunk returned error: %v", err)
	}
	if len(delta.ToolCalls) != 2 {
		t.Fatalf("tool calls = %#v", delta.ToolCalls)
	}
	if delta.ToolCalls[0].Index == nil || delta.ToolCalls[1].Index == nil {
		t.Fatalf("tool call indexes were not assigned: %#v", delta.ToolCalls)
	}
	first, second := *delta.ToolCalls[0].Index, *delta.ToolCalls[1].Index
	if first == second {
		t.Fatalf("indexes collided: %#v", delta.ToolCalls)
	}
	if first != 1 || second != 0 {
		t.Fatalf("indexes = {%d, %d}, want {1, 0}", first, second)
	}
	if string(delta.ToolCallExtra[1]["extra_content"]) != `{"source":"omitted"}` {
		t.Fatalf("omitted-index extra = %#v", delta.ToolCallExtra)
	}
	if string(delta.ToolCallExtra[0]["extra_content"]) != `{"source":"explicit"}` {
		t.Fatalf("explicit-index extra = %#v", delta.ToolCallExtra)
	}
}

func TestParseChunkCapturesContentPartExtras(t *testing.T) {
	raw := json.RawMessage(`{"choices":[{"delta":{"content":[{"type":"text","text":"visible","signature":"text-sig"},{"type":"thinking","thinking":"hidden","signature":"opaque-sig"}]}}]}`)

	delta, err := ParseChunk(raw)
	if err != nil {
		t.Fatalf("ParseChunk returned error: %v", err)
	}
	if len(delta.ContentParts) != 2 {
		t.Fatalf("content parts = %#v", delta.ContentParts)
	}
	if delta.ContentParts[0].Type != message.ContentPartText || delta.ContentParts[0].Text != "visible" || string(delta.ContentParts[0].Extra["signature"]) != `"text-sig"` {
		t.Fatalf("text part = %#v", delta.ContentParts[0])
	}
	if delta.ContentParts[1].Type != message.ContentPartOpaque || string(delta.ContentParts[1].Extra["type"]) != `"thinking"` || string(delta.ContentParts[1].Extra["thinking"]) != `"hidden"` {
		t.Fatalf("opaque part = %#v", delta.ContentParts[1])
	}
	if string(delta.ContentPartExtra[1]["signature"]) != `"opaque-sig"` {
		t.Fatalf("content part extra = %#v", delta.ContentPartExtra)
	}
}

func TestParseChunkFixtures(t *testing.T) {
	tests := []struct {
		name              string
		path              string
		wantMessageExtra  map[string]string
		wantToolCallExtra map[int]map[string]string
	}{
		{
			name: "zai reasoning_content",
			path: "testdata/zai_reasoning_content.sse",
			wantMessageExtra: map[string]string{
				"reasoning_content": `"I should inspect the file first."`,
			},
		},
		{
			name: "openrouter reasoning_details",
			path: "testdata/openrouter_reasoning_details.sse",
			wantMessageExtra: map[string]string{
				"reasoning":         `"I should inspect the request."`,
				"reasoning_details": `[{"type":"reasoning.text","text":"I should inspect the request."},{"type":"reasoning.encrypted","data":"encrypted-block","format":"google-gemini-v1","index":0}]`,
			},
		},
		{
			name: "gemini tool extra",
			path: "testdata/gemini_tool_extra.sse",
			wantToolCallExtra: map[int]map[string]string{
				0: {"extra_content": `{"google":{"thought_signature":"thought-signature"}}`},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgExtra, toolExtra := parseFixtureExtras(t, tt.path)
			for key, want := range tt.wantMessageExtra {
				if got := string(msgExtra[key]); got != want {
					t.Fatalf("%s = %s, want %s", key, got, want)
				}
			}
			for idx, fields := range tt.wantToolCallExtra {
				for key, want := range fields {
					if got := string(toolExtra[idx][key]); got != want {
						t.Fatalf("tool[%d].%s = %s, want %s", idx, key, got, want)
					}
				}
			}
		})
	}
}

func parseFixtureExtras(t *testing.T, path string) (message.Extra, map[int]message.Extra) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	msgAcc := message.NewExtraAccumulator()
	toolAcc := map[int]*message.ExtraAccumulator{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		delta, err := ParseChunk(json.RawMessage(line))
		if err != nil {
			t.Fatalf("ParseChunk(%s): %v", path, err)
		}
		for key, value := range delta.MessageExtra {
			if err := msgAcc.Add(key, value); err != nil {
				t.Fatalf("message extra %s: %v", key, err)
			}
		}
		for idx, extra := range delta.ToolCallExtra {
			acc := toolAcc[idx]
			if acc == nil {
				acc = message.NewExtraAccumulator()
				toolAcc[idx] = acc
			}
			for key, value := range extra {
				if err := acc.Add(key, value); err != nil {
					t.Fatalf("tool extra %d.%s: %v", idx, key, err)
				}
			}
		}
	}
	tools := map[int]message.Extra{}
	for idx, acc := range toolAcc {
		tools[idx] = acc.Extra()
	}
	return msgAcc.Extra(), tools
}

func BenchmarkSerializeMessages(b *testing.B) {
	prov := testProvider("https://example.com/v1", "KEY_ENV", "model-a")
	model := prov.Models["model-a"]
	model.ProtocolMetadata = &catalog.ProtocolMetadata{Family: "reasoning-family", Drop: []string{"drop_me"}}
	history := make([]message.Message, 0, 100)
	source := catalog.ModelRef{Provider: "test", Model: "model-a"}
	for i := 0; i < 25; i++ {
		history = append(history,
			message.NewText(message.RoleUser, fmt.Sprintf("request %d", i)),
			message.Message{
				Role:    message.RoleAssistant,
				Content: []message.ContentPart{{Type: message.ContentPartText, Text: fmt.Sprintf("answer %d", i)}},
				ToolCalls: []message.ToolCall{{
					ID:       fmt.Sprintf("call_%d", i),
					Type:     "function",
					Function: message.FunctionCall{Name: "read_file", Arguments: fmt.Sprintf(`{"path":"file_%d.go"}`, i)},
					Extra:    message.Extra{"extra_content": json.RawMessage(`{"google":{"thought_signature":"sig"}}`)},
				}},
				Extra:  message.Extra{"reasoning_content": json.RawMessage(`"thinking"`)},
				Source: source,
			},
			message.Message{Role: message.RoleTool, ToolCallID: fmt.Sprintf("call_%d", i), Name: "read_file", Content: []message.ContentPart{{Type: message.ContentPartText, Text: "file contents"}}},
			message.NewText(message.RoleAssistant, fmt.Sprintf("done %d", i)),
		)
	}
	if len(history) != 100 {
		b.Fatalf("history len = %d, want 100", len(history))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := SerializeMessages(history, model, prov)
		if err != nil {
			b.Fatalf("SerializeMessages: %v", err)
		}
		if len(out) != len(history) {
			b.Fatalf("serialized len = %d, want %d", len(out), len(history))
		}
	}
}
