package model

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

var fullRef = ModelRef{Provider: "openai", Model: "gpt-5.4-mini"}

func call(id, name string) ToolCall { return ToolCall{ID: id, Name: name} }

func mustMessage(t *testing.T, m Message) Message {
	t.Helper()
	out, err := NewMessage(m)
	if err != nil {
		t.Fatalf("NewMessage(role=%s) returned error: %v", m.Role, err)
	}
	return out
}

// TestNewMessageRoleSourceMatrix pins the complete role x source matrix.
func TestNewMessageRoleSourceMatrix(t *testing.T) {
	valid := []struct {
		name string
		msg  Message
	}{
		{name: "system zero", msg: Message{Role: RoleSystem}},
		{name: "user zero", msg: Message{Role: RoleUser, Name: "u"}},
		{name: "assistant complete source", msg: Message{Role: RoleAssistant, Source: fullRef}},
		{name: "tool with call id and content", msg: Message{Role: RoleTool, ToolCallID: "call_1", Content: []ContentPart{{Kind: PartText, Text: "ok"}}}},
	}
	for _, tc := range valid {
		t.Run(tc.name+" accepts", func(t *testing.T) {
			out, err := NewMessage(tc.msg)
			if err != nil {
				t.Fatalf("NewMessage returned error: %v", err)
			}
			if out.Role != tc.msg.Role || out.Source != tc.msg.Source || out.ToolCallID != tc.msg.ToolCallID || out.Name != tc.msg.Name {
				t.Fatalf("fields not preserved: %#v", out)
			}
		})
	}

	cases := []struct {
		name   string
		msg    Message
		wantIs error
	}{
		{name: "empty role invalid", msg: Message{Role: ""}, wantIs: ErrInvalidRole},
		{name: "developer is wire-only, not a canonical role", msg: Message{Role: Role("developer")}, wantIs: ErrInvalidRole},
		{name: "assistant zero source rejected", msg: Message{Role: RoleAssistant, Source: ModelRef{}}, wantIs: ErrMissingSource},
		{name: "assistant partial provider-only source rejected", msg: Message{Role: RoleAssistant, Source: ModelRef{Provider: "openai"}}, wantIs: ErrMissingSource},
		{name: "system nonzero source rejected", msg: Message{Role: RoleSystem, Source: fullRef}, wantIs: ErrUnexpectedSource},
		{name: "user partial model-only source rejected", msg: Message{Role: RoleUser, Source: ModelRef{Model: "gpt"}}, wantIs: ErrUnexpectedSource},
	}
	for _, tc := range cases {
		t.Run(tc.name+" rejects", func(t *testing.T) {
			if _, err := NewMessage(tc.msg); !errors.Is(err, tc.wantIs) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
		})
	}

	for i, bogus := range []string{"", "developer", "SYSTEM", "tool ", "" + string(rune('x'))} {
		if _, err := NewMessage(Message{Role: Role(bogus)}); !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("case %d role=%q error = %v, want ErrInvalidRole", i, bogus, err)
		}
	}
}

// TestNewMessageFixesEveryField pins that all message fields survive construction.
func TestNewMessageFixesEveryField(t *testing.T) {
	in := Message{
		Role:      RoleAssistant,
		Source:    fullRef,
		Name:      "worker",
		Content:   []ContentPart{{Kind: PartText, Text: "done"}},
		Refusal:   "cannot do that",
		ToolCalls: []ToolCall{call("call_1", "read_file")},
		Extra:     Extra{"reasoning_content": json.RawMessage(`"thinking"`)},
	}
	m, err := NewMessage(in)
	if err != nil {
		t.Fatalf("NewMessage returned error: %v", err)
	}
	if m.Role != RoleAssistant || m.Source != fullRef || m.Name != "worker" || m.Refusal != "cannot do that" {
		t.Fatalf("scalar fields not preserved: %#v", m)
	}
	if len(m.Content) != 1 || m.Content[0].Kind != PartText || m.Content[0].Text != "done" {
		t.Fatalf("content = %#v", m.Content)
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "call_1" || m.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool calls = %#v", m.ToolCalls)
	}
	if got := string(m.Extra["reasoning_content"]); got != `"thinking"` {
		t.Fatalf("extra = %s", got)
	}

	// Deep ownership: mutating caller inputs after construction must not touch the returned message.
	in.Content[0].Text = "MUTATED"
	if m.Content[0].Text != "done" {
		t.Fatal("returned content part is not an independent copy")
	}
	raw := in.Extra["reasoning_content"]
	if len(raw) > 3 {
		raw[1] = 'X' // corrupt the caller's copy after construction.
	}
	if got := string(m.Extra["reasoning_content"]); !isExactJSONString(got, "thinking") {
		t.Fatalf("returned extra shares bytes with input: %s", m.Extra["reasoning_content"])
	}

	out := mustMessage(t, Message{Role: RoleUser})
	if out.Extra != nil || len(out.Content) != 0 || out.ToolCalls != nil {
		t.Fatalf("zero-value message should stay empty: %#v", out)
	}
}

func isExactJSONString(raw, s string) bool { return raw == `"`+s+`"` }

// TestNewMessageRoleFieldCombinations pins the assistant-only and tool-only field rules.
func TestNewMessageRoleFieldCombinations(t *testing.T) {
	valid := []struct {
		name string
		msg  Message
	}{
		{name: "assistant refusal", msg: Message{Role: RoleAssistant, Source: fullRef, Refusal: "no"}},
		{name: "assistant tool calls and name", msg: Message{Role: RoleAssistant, Source: fullRef, Name: "n", ToolCalls: []ToolCall{call("c1", "f")}}},
		{name: "system with canonical name only", msg: Message{Role: RoleSystem, Name: "sys"}},
	}
	for _, tc := range valid {
		t.Run(tc.name+" accepts", func(t *testing.T) {
			if _, err := NewMessage(tc.msg); err != nil {
				t.Fatalf("NewMessage returned error: %v", err)
			}
		})
	}

	cases := []struct {
		name   string
		msg    Message
		wantIs error
	}{
		{name: "system refusal forbidden", msg: Message{Role: RoleSystem, Refusal: "x"}, wantIs: ErrForbiddenField},
		{name: "tool message with call id but refusal still forbidden", msg: Message{Role: RoleTool, ToolCallID: "c1", Refusal: "x"}, wantIs: ErrForbiddenField},
		{name: "user tool calls forbidden", msg: Message{Role: RoleUser, ToolCalls: []ToolCall{call("c1", "f")}}, wantIs: ErrForbiddenField},
		{name: "tool message call id required", msg: Message{Role: RoleTool}, wantIs: ErrMissingField},
		{name: "assistant call id forbidden", msg: Message{Role: RoleAssistant, Source: fullRef, ToolCallID: "c1"}, wantIs: ErrForbiddenField},
		{name: "user call id forbidden", msg: Message{Role: RoleUser, ToolCallID: "c1"}, wantIs: ErrForbiddenField},
	}
	for _, tc := range cases {
		t.Run(tc.name+" rejects", func(t *testing.T) {
			if _, err := NewMessage(tc.msg); !errors.Is(err, tc.wantIs) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
		})
	}

}

func TestNewMessageRejectsInvalidEmbeddedPartsAndCalls(t *testing.T) {
	if _, err := NewMessage(Message{Role: RoleUser, Content: []ContentPart{{Kind: PartText, URL: "https://x"}}}); !errors.Is(err, ErrForbiddenField) {
		t.Fatalf("text part carrying a URL error = %v, want ErrForbiddenField", err)
	}
	if _, err := NewMessage(Message{Role: RoleAssistant, Source: fullRef, ToolCalls: []ToolCall{{ID: "", Name: "f"}}}); !errors.Is(err, ErrMissingField) {
		t.Fatalf("assistant call with empty id error = %v, want ErrMissingField", err)
	}
	if _, err := NewMessage(Message{Role: RoleAssistant, Source: fullRef, ToolCalls: []ToolCall{{ID: "c1", Name: ""}}}); !errors.Is(err, ErrMissingField) {
		t.Fatalf("assistant call with empty name error = %v, want ErrMissingField", err)
	}
	if _, err := NewMessage(Message{Role: RoleUser, Content: []ContentPart{{Kind: PartOpaque}}}); !errors.Is(err, ErrMissingField) {
		t.Fatalf("opaque part without wire type error = %v, want ErrMissingField", err)
	}
}

func TestNewContentPartKindsAndExclusivity(t *testing.T) {
	valid := []struct {
		name string
		part ContentPart
	}{
		{name: "text", part: ContentPart{Kind: PartText, Text: "hi"}},
		{name: "empty text allowed for later finalization omission", part: ContentPart{Kind: PartText}},
		{name: "image url only", part: ContentPart{Kind: PartImageURL, URL: "https://example.com/a.png"}},
		{name: "opaque wire type with extras", part: ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking", Extra: Extra{"data": json.RawMessage(`"x"`)}}},
	}
	for _, tc := range valid {
		t.Run(tc.name+" accepts", func(t *testing.T) {
			out, err := NewContentPart(tc.part)
			if err != nil {
				t.Fatalf("NewContentPart returned error: %v", err)
			}
			if out.Kind != tc.part.Kind || out.Text != tc.part.Text || out.URL != tc.part.URL || out.OpaqueWireType != tc.part.OpaqueWireType {
				t.Fatalf("part not preserved: %#v", out)
			}
		})
	}

	cases := []struct {
		name string
		part ContentPart
	}{
		{name: "unknown kind rejected", part: ContentPart{Kind: PartKind("image")}},
		{name: "empty kind rejected", part: ContentPart{Text: "hi"}},
		{name: "text with url rejected", part: ContentPart{Kind: PartText, Text: "a", URL: "b"}},
		{name: "url without image kind rejected", part: ContentPart{URL: "https://x"}},
		{name: "opaque missing wire type rejected", part: ContentPart{Kind: PartOpaque}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" rejects", func(t *testing.T) {
			if _, err := NewContentPart(tc.part); !errors.Is(err, ErrForbiddenField) && !errors.Is(err, ErrMissingField) {
				t.Fatalf("error = %v, want ErrForbiddenField or ErrMissingField", err)
			}
		})
	}

	opaqueIn := ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking"}
	outOpaq, err := NewContentPart(opaqueIn)
	if err != nil || outOpaq.Kind != PartOpaque || outOpaq.OpaqueWireType != "thinking" {
		t.Fatalf("opaque part = %#v (err %v)", outOpaq, err)
	}

	srcRaw := json.RawMessage(`"x"`)
	partIn := ContentPart{Kind: PartText, Text: "a", Extra: Extra{"k": srcRaw}}
	out, err := NewContentPart(partIn)
	if err != nil {
		t.Fatalf("NewContentPart returned error: %v", err)
	}
	srcRaw[1] = 'X' // corrupt the caller's extra bytes after construction.
	storedExtra := out.Extra["k"]
	if string(storedExtra) != `"x"` || &storedExtra[0] == &srcRaw[0] {
		t.Fatalf("part extra not deep copied at accepting boundary: %s", storedExtra)
	}
}

// TestMessageHasOnlyContractFields pins that Message, Output, Request and the
// stream values carry exactly their contract fields: no internal kind, display
// metadata, or _lightcode_* persistence state.
func TestMessageHasOnlyContractFields(t *testing.T) {
	expectExactStruct(t, "Message", Message{}, []string{"Role", "Content", "Refusal", "ToolCalls", "ToolCallID", "Name", "Extra", "Source"})
	expectExactStruct(t, "Output", Output{}, []string{"Status", "Source", "Message", "Usage", "Detail"})
	expectExactStruct(t, "Request", Request{}, []string{"Messages", "Tools"})

	expectExactStruct(t, "StreamDelta", StreamDelta{}, []string{"HasChoice", "Role", "RefusalFragment", "ContentFragments", "ToolFragments", "FinishReason", "Usage", "MessageExtra"})

	expectExactStruct(t, "ContentFragment", ContentFragment{}, []string{"Position", "Kind", "Text", "URL", "OpaqueWireType", "Extra"})        // position is the one accumulation identity across deltas; opaque wire type stays structural and never enters extras.
	expectExactStruct(t, "ToolCallFragment", ToolCallFragment{}, []string{"Position", "ID", "WireType", "Name", "ArgumentFragment", "Extra"}) // optional position stays a nilable *int normalized by the fixed parser before deltas reach consumers.

	expectExactStruct(t, "Usage", Usage{}, []string{"InputTokens", "CachedInputTokens", "OutputTokens"}) // signed Go ints only — no totals, clamping, or overflow state lives on this value.

	expectExactStruct(t, "ToolDefinition", ToolDefinition{}, []string{"Name", "Description", "Parameters"}) // exactness pins the absence of any type or strict field.
	expectExactStruct(t, "ToolCall", ToolCall{}, []string{"ID", "Name", "Arguments", "Extra"})              // exactness pins that no wire-type field exists on canonical calls.
}

func expectExactStruct(t *testing.T, typeName string, v any, wantNames []string) {
	t.Helper()
	got := structFieldNames(v)
	for _, w := range wantNames {
		if !got[w] {
			t.Fatalf("%s missing contract field %q: %#v", typeName, w, got)
		}
		delete(got, w)
	}
	for extra := range got {
		t.Fatalf("%s has non-contract field %q (persistence/display state must be absent)", typeName, extra)
	}
	if len(wantNames) != 0 && reflect.ValueOf(v).Type().NumField() != len(wantNames)+len(got) {
		t.Fatalf("%s field count mismatch: %#v", typeName, got)
	}
}

func structFieldNames(v any) map[string]bool {
	names := map[string]bool{}
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.PkgPath != "" { // unexported fields are not part of the value contract surface.
			continue
		}
		names[f.Name] = true
	}
	return names
}

// TestTextHelpers ports the retained text-accumulation semantics.
func TestTextHelpers(t *testing.T) {
	msg := mustMessage(t, Message{Role: RoleUser})
	img, err := NewContentPart(ContentPart{Kind: PartImageURL, URL: "https://example.com/a.png"})
	if err != nil {
		t.Fatalf("NewContentPart returned error: %v", err)
	}

	msg.Content = append(msg.Content, ContentPart{Kind: PartText, Text: "hello"})
	msg.AppendText(" world")
	msg.Content = append(msg.Content, img)
	msg.AppendText(" again")

	if got := msg.TextContent(); got != "hello world again" {
		t.Fatalf("TextContent = %q, want joined text parts in order", got)
	}

	empty := mustMessage(t, Message{Role: RoleUser})
	empty.AppendText("")
	if len(empty.Content) != 0 {
		t.Fatal(`AppendText("") created a content part`)
	}

	mixed := mustMessage(t, Message{Role: RoleAssistant, Source: fullRef})
	img2, _ := NewContentPart(ContentPart{Kind: PartImageURL, URL: "u"})
	opaque, _ := NewContentPart(ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking"})
	mixed.Content = []ContentPart{{Kind: PartText, Text: "a"}, img2, opaque}
	if got := mixed.TextContent(); got != "a" {
		t.Fatalf("non-text parts must be skipped by TextContent: %q", got)
	}

	appendedToLastOnly := mustMessage(t, Message{Role: RoleUser})
	txt, _ := NewContentPart(ContentPart{Kind: PartText, Text: "AB"})
	img3, _ := NewContentPart(ContentPart{Kind: PartImageURL, URL: "u2"})
	appendedToLastOnly.Content = []ContentPart{txt}
	appendedToLastOnly.AppendText("CDE") // extends the trailing text part.
	if appendedToLastOnly.Content[0].Text != "ABCDE" {
		t.Fatalf("AppendText must extend the trailing text part: %#v", appendedToLastOnly.Content)
	}
	appendedToLastOnly.Content = append(appendedToLastOnly.Content, img3)
	appendedToLastOnly.AppendText("FGH") // creates a new text part after non-text.
	if len(appendedToLastOnly.Content) != 3 || appendedToLastOnly.Content[2].Kind != PartText || appendedToLastOnly.Content[2].Text != "FGH" {
		t.Fatalf("AppendText must create a text part when the last is not text: %#v", appendedToLastOnly.Content)
	}
	if got := appendedToLastOnly.TextContent(); got != "ABCDEFGH" {
		t.Fatalf("joined = %q, want parts concatenated in order without separators (legacy semantics)", got)
	}
}
