package message

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
)

func TestTextHelpers(t *testing.T) {
	msg := NewText(RoleUser, "hello")
	msg.AppendText(" world")
	msg.Content = append(msg.Content, ContentPart{Type: ContentPartImageURL, URL: "https://example.com/a.png"})
	msg.AppendText(" again")

	if got := msg.TextContent(); got != "hello world again" {
		t.Fatalf("TextContent = %q, want joined text parts", got)
	}
}

func TestMessageJSONFlattensExtraAndSource(t *testing.T) {
	msg := Message{
		Role:    RoleAssistant,
		Content: []ContentPart{{Type: ContentPartText, Text: "done"}},
		Refusal: "cannot",
		ToolCalls: []ToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: FunctionCall{Name: "read_file", Arguments: `{"path":"foo.txt"}`},
			Extra:    Extra{"extra_content": json.RawMessage(`{"google":{"thought_signature":"sig"}}`)},
		}},
		Extra:        Extra{"reasoning_content": json.RawMessage(`"thinking"`)},
		Source:       coremodel.ModelRef{Provider: "xiaomi", Model: "mimo-v2.5-pro"},
		InternalKind: "kind_demo",
		DisplayMetadata: map[string]any{
			"subagent_session_ids": []map[string]any{{"index": 0, "sessionId": "child"}},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal marshaled message: %v", err)
	}
	if raw["role"] != "assistant" || raw["content"] != "done" || raw["refusal"] != "cannot" || raw["reasoning_content"] != "thinking" {
		t.Fatalf("message JSON = %#v", raw)
	}
	if raw["_lightcode_source"] != "xiaomi/mimo-v2.5-pro" {
		t.Fatalf("_lightcode_source = %#v", raw["_lightcode_source"])
	}
	if raw["_lightcode_internal"] != "kind_demo" {
		t.Fatalf("_lightcode_internal = %#v", raw["_lightcode_internal"])
	}
	if _, ok := raw["_lightcode_display_metadata"].(map[string]any); !ok {
		t.Fatalf("_lightcode_display_metadata missing: %#v", raw)
	}
	toolCalls := raw["tool_calls"].([]any)
	firstTool := toolCalls[0].(map[string]any)
	if _, ok := firstTool["extra_content"].(map[string]any); !ok {
		t.Fatalf("tool call extra not flattened: %#v", firstTool)
	}

	var decoded Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if decoded.Source != msg.Source {
		t.Fatalf("Source = %#v, want %#v", decoded.Source, msg.Source)
	}
	if decoded.InternalKind != "kind_demo" {
		t.Fatalf("InternalKind = %q, want kind_demo", decoded.InternalKind)
	}
	if decoded.DisplayMetadata["subagent_session_ids"] == nil {
		t.Fatalf("DisplayMetadata = %#v, want subagent links", decoded.DisplayMetadata)
	}
	if decoded.Refusal != "cannot" {
		t.Fatalf("Refusal = %q, want cannot", decoded.Refusal)
	}
	if got := string(decoded.Extra["reasoning_content"]); got != `"thinking"` {
		t.Fatalf("reasoning_content = %s", got)
	}
	if got := string(decoded.ToolCalls[0].Extra["extra_content"]); got != `{"google":{"thought_signature":"sig"}}` {
		t.Fatalf("tool extra = %s", got)
	}
}

func TestMessageJSONLoadsOldSessionWithoutSource(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"old"}`), &msg); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if msg.Source != (coremodel.ModelRef{}) {
		t.Fatalf("Source = %#v, want zero", msg.Source)
	}
	if got := msg.TextContent(); got != "old" {
		t.Fatalf("TextContent = %q, want old", got)
	}
}

func TestMessageJSONOmitsPartialSource(t *testing.T) {
	msg := Message{
		Role:    RoleAssistant,
		Content: []ContentPart{{Type: ContentPartText, Text: "done"}},
		Source:  coremodel.ModelRef{Provider: "openai"},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal marshaled message: %v", err)
	}
	if _, ok := raw["_lightcode_source"]; ok {
		t.Fatalf("_lightcode_source present for partial source: %#v", raw)
	}

	var decoded Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if decoded.Source != (coremodel.ModelRef{}) {
		t.Fatalf("decoded Source = %#v, want zero", decoded.Source)
	}
}

func TestContentPartJSONShapes(t *testing.T) {
	msg := Message{
		Role: RoleUser,
		Content: []ContentPart{
			{Type: ContentPartText, Text: "look"},
			{Type: ContentPartImageURL, URL: "https://example.com/image.png"},
			{Type: ContentPartOpaque, Extra: Extra{
				"type":      json.RawMessage(`"thinking"`),
				"thinking":  json.RawMessage(`"work"`),
				"signature": json.RawMessage(`"sig"`),
			}},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if len(decoded.Content) != 3 {
		t.Fatalf("decoded content len = %d", len(decoded.Content))
	}
	if decoded.Content[0].Type != ContentPartText || decoded.Content[0].Text != "look" {
		t.Fatalf("text part = %#v", decoded.Content[0])
	}
	if decoded.Content[1].Type != ContentPartImageURL || decoded.Content[1].URL != "https://example.com/image.png" {
		t.Fatalf("image part = %#v", decoded.Content[1])
	}
	opaque := decoded.Content[2]
	if opaque.Type != ContentPartOpaque {
		t.Fatalf("opaque type = %#v", opaque.Type)
	}
	if got := string(opaque.Extra["type"]); got != `"thinking"` {
		t.Fatalf("opaque type extra = %s", got)
	}
	if got := string(opaque.Extra["signature"]); got != `"sig"` {
		t.Fatalf("opaque signature = %s", got)
	}
}

func TestCanonicalExtraKeysAreNotDuplicatedFromExtra(t *testing.T) {
	msg := Message{
		Role:    RoleAssistant,
		Refusal: "real",
		Extra: Extra{
			"role":              json.RawMessage(`"bad"`),
			"refusal":           json.RawMessage(`"bad"`),
			"_lightcode_debug":  json.RawMessage(`true`),
			"reasoning_content": json.RawMessage(`"ok"`),
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal marshaled message: %v", err)
	}
	if raw["role"] != "assistant" {
		t.Fatalf("role = %#v", raw["role"])
	}
	if raw["refusal"] != "real" {
		t.Fatalf("refusal = %#v", raw["refusal"])
	}
	if _, ok := raw["_lightcode_debug"]; ok {
		t.Fatalf("private key leaked into JSON: %#v", raw)
	}
	if raw["reasoning_content"] != "ok" {
		t.Fatalf("reasoning_content = %#v", raw["reasoning_content"])
	}
}
