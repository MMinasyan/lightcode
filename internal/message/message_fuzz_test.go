package message

import (
	"encoding/json"
	"testing"
)

func FuzzMessageJSON(f *testing.F) {
	for _, seed := range []string{
		`{"role":"user","content":"hello"}`,
		`{"role":"assistant","content":"done","refusal":"cannot","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"foo.txt\"}"},"extra_content":{"google":{"thought_signature":"sig"}}}],"reasoning_content":"thinking","_lightcode_source":"xiaomi/mimo-v2.5-pro"}`,
		`{"role":"assistant","content":"old"}`,
		`{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://example.com/image.png"}},{"type":"thinking","thinking":"work","signature":"sig"}]}`,
		`{"role":"assistant","refusal":"real","reasoning_content":"ok"}`,
		`{"role":"tool","tool_call_id":"call_1","name":"read_file","content":"result"}`,
		`{not-json}`,
		`{}`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		encoded, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("Marshal after successful Unmarshal returned error: %v", err)
		}
		var normalized Message
		if err := json.Unmarshal(encoded, &normalized); err != nil {
			t.Fatalf("Unmarshal of marshaled message returned error: %v", err)
		}
		encodedAgain, err := json.Marshal(normalized)
		if err != nil {
			t.Fatalf("Marshal after normalized Unmarshal returned error: %v", err)
		}
		var roundTripped Message
		if err := json.Unmarshal(encodedAgain, &roundTripped); err != nil {
			t.Fatalf("Unmarshal of second marshaled message returned error: %v", err)
		}
		assertCanonicalMessageEqual(t, normalized, roundTripped)
	})
}

func assertCanonicalMessageEqual(t *testing.T, want, got Message) {
	t.Helper()
	if want.Role != got.Role {
		t.Fatalf("Role = %q, want %q", got.Role, want.Role)
	}
	if want.Refusal != got.Refusal {
		t.Fatalf("Refusal = %q, want %q", got.Refusal, want.Refusal)
	}
	if want.ToolCallID != got.ToolCallID {
		t.Fatalf("ToolCallID = %q, want %q", got.ToolCallID, want.ToolCallID)
	}
	if want.Name != got.Name {
		t.Fatalf("Name = %q, want %q", got.Name, want.Name)
	}
	if want.Source.String() != got.Source.String() {
		t.Fatalf("Source = %q, want %q", got.Source.String(), want.Source.String())
	}
	if len(want.Content) != len(got.Content) {
		t.Fatalf("Content length = %d, want %d", len(got.Content), len(want.Content))
	}
	for i := range want.Content {
		if want.Content[i].Type != got.Content[i].Type {
			t.Fatalf("Content[%d].Type = %q, want %q", i, got.Content[i].Type, want.Content[i].Type)
		}
		if want.Content[i].Text != got.Content[i].Text {
			t.Fatalf("Content[%d].Text = %q, want %q", i, got.Content[i].Text, want.Content[i].Text)
		}
		if want.Content[i].URL != got.Content[i].URL {
			t.Fatalf("Content[%d].URL = %q, want %q", i, got.Content[i].URL, want.Content[i].URL)
		}
	}
	if len(want.ToolCalls) != len(got.ToolCalls) {
		t.Fatalf("ToolCalls length = %d, want %d", len(got.ToolCalls), len(want.ToolCalls))
	}
	for i := range want.ToolCalls {
		if want.ToolCalls[i].ID != got.ToolCalls[i].ID {
			t.Fatalf("ToolCalls[%d].ID = %q, want %q", i, got.ToolCalls[i].ID, want.ToolCalls[i].ID)
		}
		if want.ToolCalls[i].Type != got.ToolCalls[i].Type {
			t.Fatalf("ToolCalls[%d].Type = %q, want %q", i, got.ToolCalls[i].Type, want.ToolCalls[i].Type)
		}
		if want.ToolCalls[i].Function.Name != got.ToolCalls[i].Function.Name {
			t.Fatalf("ToolCalls[%d].Function.Name = %q, want %q", i, got.ToolCalls[i].Function.Name, want.ToolCalls[i].Function.Name)
		}
		if want.ToolCalls[i].Function.Arguments != got.ToolCalls[i].Function.Arguments {
			t.Fatalf("ToolCalls[%d].Function.Arguments = %q, want %q", i, got.ToolCalls[i].Function.Arguments, want.ToolCalls[i].Function.Arguments)
		}
	}
}
