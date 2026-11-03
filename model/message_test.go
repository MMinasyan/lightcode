package model

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

var fullRef = ModelRef{Provider: "openai", Model: "gpt-5.4-mini"}

func call(id, name string) ToolCall { return ToolCall{ID: id, Name: name} }

func mustMessage(t *testing.T, m Message) Message {
	t.Helper()
	out, err := NewMessage(m)
	testassert.Require(t, err == nil, "NewMessage(role=%s) returned error: %v", m.Role, err)
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
			testassert.NoError(t, err)
			testassert.Require(t, out.Role == tc.msg.Role && out.Source == tc.msg.Source && out.ToolCallID == tc.msg.ToolCallID && out.Name == tc.msg.Name, "fields not preserved: %#v", out)
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
			{
				_, err := NewMessage(tc.msg)
				testassert.Require(t, errors.Is(err, tc.wantIs), "error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
		})
	}

	for i, bogus := range []string{"", "developer", "SYSTEM", "tool ", "" + string(rune('x'))} {
		{
			_, err := NewMessage(Message{Role: Role(bogus)})
			testassert.Require(t, errors.Is(err, ErrInvalidRole), "case %d role=%q error = %v, want ErrInvalidRole", i, bogus, err)
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
	testassert.NoError(t, err)
	testassert.Require(t, m.Role == RoleAssistant && m.Source == fullRef && m.Name == "worker" && m.Refusal == "cannot do that" && (len(m.Content) == 1 && m.Content[0].Kind == PartText && m.Content[0].Text == "done") && (len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "call_1" && m.ToolCalls[0].Name == "read_file"), "scalar fields not preserved: %#v; content = %#v; tool calls = %#v", m, m.Content, m.ToolCalls)
	{
		got := string(m.Extra["reasoning_content"])
		testassert.Equal(t, got, `"thinking"`)
	}

	// Deep ownership: mutating caller inputs after construction must not touch the returned message.
	in.Content[0].Text = "MUTATED"
	testassert.Require(t, m.Content[0].Text == "done", "%v", "returned content part is not an independent copy")
	raw := in.Extra["reasoning_content"]
	if len(raw) > 3 {
		raw[1] = 'X' // corrupt the caller's copy after construction.
	}
	{
		got := string(m.Extra["reasoning_content"])
		testassert.Require(t, isExactJSONString(got, "thinking"), "returned extra shares bytes with input: %s", m.Extra["reasoning_content"])
	}

	out := mustMessage(t, Message{Role: RoleUser})
	testassert.Require(t, out.Extra == nil && len(out.Content) == 0 && out.ToolCalls == nil, "zero-value message should stay empty: %#v", out)
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
			{
				_, err := NewMessage(tc.msg)
				testassert.NoError(t, err)
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
			{
				_, err := NewMessage(tc.msg)
				testassert.Require(t, errors.Is(err, tc.wantIs), "error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
		})
	}

}

func TestNewMessageRejectsInvalidEmbeddedPartsAndCalls(t *testing.T) {
	{
		_, err := NewMessage(Message{Role: RoleUser, Content: []ContentPart{{Kind: PartText, URL: "https://x"}}})
		testassert.Require(t, errors.Is(err, ErrForbiddenField), "text part carrying a URL error = %v, want ErrForbiddenField", err)
	}
	{
		_, err := NewMessage(Message{Role: RoleAssistant, Source: fullRef, ToolCalls: []ToolCall{{ID: "", Name: "f"}}})
		testassert.Require(t, errors.Is(err, ErrMissingField), "assistant call with empty id error = %v, want ErrMissingField", err)
	}
	{
		_, err := NewMessage(Message{Role: RoleAssistant, Source: fullRef, ToolCalls: []ToolCall{{ID: "c1", Name: ""}}})
		testassert.Require(t, errors.Is(err, ErrMissingField), "assistant call with empty name error = %v, want ErrMissingField", err)
	}
	{
		_, err := NewMessage(Message{Role: RoleUser, Content: []ContentPart{{Kind: PartOpaque}}})
		testassert.Require(t, errors.Is(err, ErrMissingField), "opaque part without wire type error = %v, want ErrMissingField", err)
	}
}

func TestNewContentPartKindsAndExclusivity(t *testing.T) {
	// One inventory for the ContentPart accepting boundary: the cross-kind,
	// unknown-kind, missing-wire-type and Extra-value shapes, each with its
	// original expected verdict. Both entry points must match every row.
	rows := []struct {
		name      string
		part      ContentPart
		wantErr   bool
		wantIsAny []error
	}{
		{"text", ContentPart{Kind: PartText, Text: "hi"}, false, nil},
		{"rich text", ContentPart{Kind: PartText, Text: "hello", Extra: Extra{"k": json.RawMessage(`"v"`)}}, false, nil},
		{"empty text allowed for later finalization omission", ContentPart{Kind: PartText}, false, nil},
		{"image url only", ContentPart{Kind: PartImageURL, URL: "https://example.com/a.png"}, false, nil},
		{"opaque wire type with extras", ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking", Extra: Extra{"data": json.RawMessage(`"x"`)}}, false, nil},
		{"opaque with nested extra", ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking", Extra: Extra{"t": json.RawMessage(`{"n":1}`)}}, false, nil},
		{"null extra value", ContentPart{Kind: PartText, Text: "a", Extra: Extra{"n": json.RawMessage(`null`)}}, false, nil},
		{"unknown kind rejected", ContentPart{Kind: PartKind("image")}, true, []error{ErrForbiddenField, ErrMissingField}},
		{"unknown named kind rejected", ContentPart{Kind: PartKind("audio"), Text: "a"}, true, nil},
		{"empty kind rejected", ContentPart{Text: "hi"}, true, []error{ErrForbiddenField, ErrMissingField}},
		{"text with url rejected", ContentPart{Kind: PartText, Text: "a", URL: "b"}, true, []error{ErrForbiddenField, ErrMissingField}},
		{"text carrying opaque wire type rejected", ContentPart{Kind: PartText, Text: "a", OpaqueWireType: "thinking"}, true, nil},
		{"image carrying text rejected", ContentPart{Kind: PartImageURL, URL: "u", Text: "a"}, true, nil},
		{"image carrying opaque wire type rejected", ContentPart{Kind: PartImageURL, URL: "u", OpaqueWireType: "thinking"}, true, nil},
		{"url without image kind rejected", ContentPart{URL: "https://x"}, true, []error{ErrForbiddenField, ErrMissingField}},
		{"opaque carrying text rejected", ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking", Text: "a"}, true, nil},
		{"opaque carrying url rejected", ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking", URL: "u"}, true, nil},
		{"opaque missing wire type rejected", ContentPart{Kind: PartOpaque}, true, []error{ErrMissingField}},
		{"malformed extra", ContentPart{Kind: PartText, Text: "a", Extra: Extra{"raw": json.RawMessage("not json")}}, true, nil},
		{"empty extra value", ContentPart{Kind: PartText, Text: "a", Extra: Extra{"e": json.RawMessage{}}}, true, nil},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			out, newErr := NewContentPart(row.part)
			validateErr := ValidateContentPart(row.part)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			if len(row.wantIsAny) > 0 {
				testassert.Require(t, matchesAnySentinel(newErr, row.wantIsAny...) && matchesAnySentinel(validateErr, row.wantIsAny...), "fixture %s: errors = %v / %v, want one of %v", row.name, newErr, validateErr, row.wantIsAny)
			}
			if !row.wantErr {
				testassert.Require(t, out.Kind == row.part.Kind && out.Text == row.part.Text && out.URL == row.part.URL && out.OpaqueWireType == row.part.OpaqueWireType, "part not preserved: %#v", out)
			}
		})
	}

	// The validator mutates nothing: a value-equal sibling still compares
	// equal after the call, extras included.
	in := ContentPart{Kind: PartImageURL, URL: "https://example.com/a.png", Extra: Extra{"detail": json.RawMessage(`"high"`)}}
	before := ContentPart{Kind: PartImageURL, URL: "https://example.com/a.png", Extra: Extra{"detail": json.RawMessage(`"high"`)}}
	{
		err := ValidateContentPart(in)
		testassert.NoError(t, err)
	}
	testassert.Require(t, reflect.DeepEqual(in, before), "%v", "ValidateContentPart mutated its input")

	opaqueIn := ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking"}
	outOpaq, err := NewContentPart(opaqueIn)
	testassert.Require(t, err == nil && outOpaq.Kind == PartOpaque && outOpaq.OpaqueWireType == "thinking", "opaque part = %#v (err %v)", outOpaq, err)

	srcRaw := json.RawMessage(`"x"`)
	partIn := ContentPart{Kind: PartText, Text: "a", Extra: Extra{"k": srcRaw}}
	out, err := NewContentPart(partIn)
	testassert.NoError(t, err)
	srcRaw[1] = 'X' // corrupt the caller's extra bytes after construction.
	storedExtra := out.Extra["k"]
	testassert.Require(t, string(storedExtra) == `"x"` && &storedExtra[0] != &srcRaw[0], "part extra not deep copied at accepting boundary: %s", storedExtra)
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
		testassert.Require(t, got[w], "%s missing contract field %q: %#v", typeName, w, got)
		delete(got, w)
	}
	for extra := range got {
		t.Fatalf("%s has non-contract field %q (persistence/display state must be absent)", typeName, extra)
	}
	testassert.Require(t, len(wantNames) == 0 || reflect.ValueOf(v).Type().NumField() == len(wantNames)+len(got), "%s field count mismatch: %#v", typeName, got)
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
	testassert.NoError(t, err)

	msg.Content = append(msg.Content, ContentPart{Kind: PartText, Text: "hello"})
	msg.AppendText(" world")
	msg.Content = append(msg.Content, img)
	msg.AppendText(" again")

	{
		got := msg.TextContent()
		testassert.Equal(t, got, "hello world again")
	}

	empty := mustMessage(t, Message{Role: RoleUser})
	empty.AppendText("")
	testassert.Require(t, len(empty.Content) == 0, "%v", `AppendText("") created a content part`)

	mixed := mustMessage(t, Message{Role: RoleAssistant, Source: fullRef})
	img2, _ := NewContentPart(ContentPart{Kind: PartImageURL, URL: "u"})
	opaque, _ := NewContentPart(ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking"})
	mixed.Content = []ContentPart{{Kind: PartText, Text: "a"}, img2, opaque}
	{
		got := mixed.TextContent()
		testassert.Equal(t, got, "a")
	}

	appendedToLastOnly := mustMessage(t, Message{Role: RoleUser})
	txt, _ := NewContentPart(ContentPart{Kind: PartText, Text: "AB"})
	img3, _ := NewContentPart(ContentPart{Kind: PartImageURL, URL: "u2"})
	appendedToLastOnly.Content = []ContentPart{txt}
	appendedToLastOnly.AppendText("CDE") // extends the trailing text part.
	testassert.Require(t, appendedToLastOnly.Content[0].Text == "ABCDE", "AppendText must extend the trailing text part: %#v", appendedToLastOnly.Content)
	appendedToLastOnly.Content = append(appendedToLastOnly.Content, img3)
	appendedToLastOnly.AppendText("FGH") // creates a new text part after non-text.
	testassert.Require(t, len(appendedToLastOnly.Content) == 3 && appendedToLastOnly.Content[2].Kind == PartText && appendedToLastOnly.Content[2].Text == "FGH", "AppendText must create a text part when the last is not text: %#v", appendedToLastOnly.Content)
	{
		got := appendedToLastOnly.TextContent()
		testassert.Equal(t, got, "ABCDEFGH")
	}
}
