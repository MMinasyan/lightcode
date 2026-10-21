package model

// Validation-only validator differential: ValidateOutput, ValidateStreamDelta
// and ValidateToolCall must accept and reject exactly what their owning
// constructors do — same error classes, precedence and detail — while
// retaining and mutating nothing. The allocation benchmarks at the bottom are
// the evidence that the read-only path no longer pays the ownership copy.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// richOutputFixture builds one deterministic completed output carrying every
// nested reference type: content parts with extras, tool-call argument bytes
// and extras, message extras, and a usage pointer. It sits on the package's
// shared complete source identity, the same one assistantMsg uses, so the
// output's Source always matches its message's Source and every differential
// row reaches the rule it names.
func richOutputFixture() Output {
	msg := Message{
		Role:   RoleAssistant,
		Source: fullRef,
		Content: []ContentPart{
			{Kind: PartText, Text: "working on it"},
			{Kind: PartImageURL, URL: "https://example.com/a.png", Extra: Extra{"detail": json.RawMessage(`"high"`)}},
			{Kind: PartOpaque, OpaqueWireType: "thinking", Extra: Extra{"t": json.RawMessage(`{"n":1}`)}},
		},
		ToolCalls: []ToolCall{{
			ID:        "call_1",
			Name:      "read_file",
			Arguments: json.RawMessage(`{"path":"a/b.txt"}`),
			Extra:     Extra{"sig": json.RawMessage(`"s"`), "meta": json.RawMessage(`{"m":2}`)},
		}},
		Extra: Extra{"reasoning_content": json.RawMessage(`"r"`)},
	}
	return Output{
		Status:  OutputCompleted,
		Source:  fullRef,
		Message: &msg,
		Usage:   &Usage{InputTokens: 12, CachedInputTokens: 3, OutputTokens: 45},
	}
}

// richDeltaFixture builds one deterministic choice-bearing delta carrying
// positioned fragments with extras, a pointer tool position, raw argument
// fragments, a refusal, usage, and message extras.
func richDeltaFixture() StreamDelta {
	pos := 7
	return StreamDelta{
		HasChoice:       true,
		Role:            "assistant",
		RefusalFragment: "maybe not",
		ContentFragments: []ContentFragment{
			{Position: 0, Kind: PartText, Text: "alpha", Extra: Extra{"k": json.RawMessage(`"v"`)}},
			{Position: 1, Kind: PartImageURL, URL: "https://example.com/a.png"},
			{Position: 2, Kind: PartOpaque, Extra: Extra{"o": json.RawMessage(`1`)}},
		},
		ToolFragments: []ToolCallFragment{
			{Position: &pos, ID: "c1", Name: "fn", ArgumentFragment: `{"a":`, Extra: Extra{"t": json.RawMessage(`2`)}},
			{ID: "c1", ArgumentFragment: "1}"},
		},
		FinishReason: "stop",
		Usage:        &Usage{InputTokens: 5, OutputTokens: 7},
		MessageExtra: Extra{"m": json.RawMessage(`3`)},
	}
}

// richToolCallFixture builds one deterministic completed call with raw
// argument bytes and a multi-key Extra map.
func richToolCallFixture() ToolCall {
	return ToolCall{
		ID:        "call_1",
		Name:      "read_file",
		Arguments: json.RawMessage(`{"path":"a/b.txt","opts":{"recursive":true,"limit":10}}`),
		Extra:     Extra{"sig": json.RawMessage(`"s"`), "meta": json.RawMessage(`{"m":2}`)},
	}
}

var benchRichOutput = richOutputFixture()
var benchRichDelta = richDeltaFixture()
var benchRichToolCall = richToolCallFixture()
var benchRichRequest = Request{
	Messages: []Message{
		{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "hello", Extra: Extra{"u": json.RawMessage(`"v"`)}}}},
		{
			Role:    RoleAssistant,
			Source:  fullRef,
			Content: []ContentPart{{Kind: PartText, Text: "working"}, {Kind: PartImageURL, URL: "https://example.com/a.png", Extra: Extra{"detail": json.RawMessage(`"high"`)}}},
			ToolCalls: []ToolCall{
				richToolCallFixture(),
			},
			Extra: Extra{"reasoning_content": json.RawMessage(`"r"`)},
		},
	},
	Tools: []ToolDefinition{
		{Name: "read_file", Description: "reads", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		{Name: "echo", Description: "echoes", Parameters: json.RawMessage(`{"type":"object"}`)},
	},
}

// requireEqualConstructorVerdict asserts the differential: the owning
// constructor and the validation-only validator return the same acceptance
// verdict with byte-identical error detail for one input.
func requireEqualConstructorVerdict(t *testing.T, name string, newErr, validateErr error) {
	t.Helper()
	testassert.Require(t, (newErr == nil) == (validateErr == nil), "%s: New error = %v, Validate error = %v; verdicts diverge", name, newErr, validateErr)
	if newErr != nil && newErr.Error() != validateErr.Error() {
		t.Fatalf("%s: New detail = %q, Validate detail = %q", name, newErr.Error(), validateErr.Error())
	}
}

// TestValidateOutputMatchesNewOutput pins the Output differential over valid
// and invalid shapes: identical verdicts and detail, and an input the
// validator leaves exactly as it received it.
func TestValidateOutputMatchesNewOutput(t *testing.T) {
	textMsg := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartText, Text: "done"}} })
	payloadMsg := assistantMsg(t, func(m *Message) {
		m.Content = []ContentPart{{Kind: PartText, Text: "x"}}
		c, _ := NewToolCall(ToolCall{ID: "c1", Name: "f", Arguments: json.RawMessage(`{oops`)})
		m.ToolCalls = []ToolCall{c}
	})
	userRoleMsg := mustMessage(t, Message{Role: RoleUser})
	noPayloadMsg := assistantMsg(t, nil)

	// Every row carrying an assistantMsg-built message sits on fullRef — the
	// same complete identity that message already carries — so the Output
	// domain's matched-source premise holds and the row reaches the rule it
	// names instead of stopping at ErrSourceMismatch. errContains pins that
	// actual rule; the mismatch row is the one intentional exception.
	rows := []struct {
		name        string
		out         Output
		wantErr     bool
		errContains string
	}{
		{"rich completed", richOutputFixture(), false, ""},
		{"errored without message", Output{Status: OutputErrored, Source: fullRef, Detail: "boom"}, false, ""},
		{"interrupted with partial", Output{Status: OutputInterrupted, Source: fullRef, Message: &textMsg, Detail: "stopped"}, false, ""},
		{"invalid status", Output{Status: OutputStatus("done"), Source: fullRef, Detail: "d"}, true, "invalid status"},
		{"zero source", Output{Status: OutputCompleted, Message: &textMsg}, true, "complete source model identity"},
		{"completed without message", Output{Status: OutputCompleted, Source: fullRef}, true, "requires one assistant message"},
		{"completed without payload", Output{Status: OutputCompleted, Source: fullRef, Message: &noPayloadMsg}, true, "requires an assistant payload"},
		{"completed with detail", Output{Status: OutputCompleted, Source: fullRef, Message: &textMsg, Detail: "boom"}, true, "must carry empty detail"},
		{"errored without detail", Output{Status: OutputErrored, Source: fullRef}, true, "requires non-empty detail"},
		{"interrupted with tool calls", Output{Status: OutputInterrupted, Source: fullRef, Message: &payloadMsg, Detail: "stopped"}, true, "must not carry tool calls"},
		{"message source mismatch", func() Output {
			msg := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartText, Text: "x"}} })
			return Output{Status: OutputCompleted, Source: ModelRef{Provider: "acme", Model: "other"}, Message: &msg}
		}(), true, "differs from output source"},
		{"non-assistant message", Output{Status: OutputCompleted, Source: fullRef, Message: &userRoleMsg}, true, "carry only assistant messages"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, newErr := NewOutput(row.out)
			validateErr := ValidateOutput(row.out)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			testassert.Require(t, row.errContains == "" || strings.Contains(newErr.Error(), row.errContains), "fixture %s reached the wrong rule: %v, want a failure naming %q", row.name, newErr, row.errContains)
		})
	}

	// The validator mutates nothing: an independently built value-equal
	// sibling compares equal after the call, nested reference types included.
	in := richOutputFixture()
	before := richOutputFixture()
	{
		err := ValidateOutput(in)
		testassert.NoError(t, err)
	}
	testassert.Require(t, reflect.DeepEqual(in, before), "%v", "ValidateOutput mutated its input")
}

// TestValidateStreamDeltaMatchesNewStreamDelta pins the StreamDelta
// differential over valid and invalid shapes with identical verdicts and
// detail, and an untouched input.
func TestValidateStreamDeltaMatchesNewStreamDelta(t *testing.T) {
	neg := -1
	rows := []struct {
		name    string
		delta   StreamDelta
		wantErr bool
	}{
		{"rich choice-bearing", richDeltaFixture(), false},
		{"empty delta", StreamDelta{HasChoice: true}, false},
		{"usage only", StreamDelta{HasChoice: true, Usage: &Usage{InputTokens: 1}}, false},
		{"negative content position", StreamDelta{HasChoice: true, ContentFragments: []ContentFragment{{Position: -1, Kind: PartText, Text: "x"}}}, true},
		{"cross-kind fragment", StreamDelta{HasChoice: true, ContentFragments: []ContentFragment{{Position: 0, Kind: PartText, Text: "x", URL: "u"}}}, true},
		{"unknown kind", StreamDelta{HasChoice: true, ContentFragments: []ContentFragment{{Position: 0, Kind: PartKind("audio"), Text: "x"}}}, true},
		{"negative tool pointer position", StreamDelta{HasChoice: true, ToolFragments: []ToolCallFragment{{ID: "c", Name: "f", Position: &neg}}}, true},
		{"malformed tool extra", StreamDelta{HasChoice: true, ToolFragments: []ToolCallFragment{{ID: "c", Name: "f", Extra: Extra{"k": json.RawMessage(`{oops`)}}}}, true},
		{"malformed message extra", StreamDelta{HasChoice: true, MessageExtra: Extra{"m": json.RawMessage(`[1`)}}, true},
		{"valid-looking text with an invalid sibling", StreamDelta{HasChoice: true, RefusalFragment: "no", ContentFragments: []ContentFragment{{Position: -1, Kind: PartText, Text: "x"}}}, true},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, newErr := NewStreamDelta(row.delta)
			validateErr := ValidateStreamDelta(row.delta)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
		})
	}

	// The validator mutates nothing, nested fragments and extras included.
	in := richDeltaFixture()
	before := richDeltaFixture()
	{
		err := ValidateStreamDelta(in)
		testassert.NoError(t, err)
	}
	testassert.Require(t, reflect.DeepEqual(in, before), "%v", "ValidateStreamDelta mutated its input")
}

// TestValidateToolCallMatchesNewToolCall pins the ToolCall differential: the
// id/name check precedes the Extra check under both entry points, raw
// arguments of every JSON shape stay accepted, and the validator leaves the
// input untouched.
func TestValidateToolCallMatchesNewToolCall(t *testing.T) {
	rows := []struct {
		name    string
		call    ToolCall
		wantErr bool
	}{
		{"rich", richToolCallFixture(), false},
		{"object arguments", ToolCall{ID: "c", Name: "f", Arguments: json.RawMessage(`{"a":1}`)}, false},
		{"empty arguments", ToolCall{ID: "c", Name: "f"}, false},
		{"malformed arguments", ToolCall{ID: "c", Name: "f", Arguments: json.RawMessage(`{oops`)}, false},
		{"null arguments", ToolCall{ID: "c", Name: "f", Arguments: json.RawMessage(`null`)}, false},
		{"array arguments", ToolCall{ID: "c", Name: "f", Arguments: json.RawMessage(`[1,2]`)}, false},
		{"primitive arguments", ToolCall{ID: "c", Name: "f", Arguments: json.RawMessage(`"x"`)}, false},
		{"empty id", ToolCall{Name: "f"}, true},
		{"empty name", ToolCall{ID: "c"}, true},
		{"malformed extra", ToolCall{ID: "c", Name: "f", Extra: Extra{"k": json.RawMessage(`{oops`)}}, true},
		{"id precedence over malformed extra", ToolCall{Name: "f", Extra: Extra{"k": json.RawMessage(`{oops`)}}, true},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, newErr := NewToolCall(row.call)
			validateErr := ValidateToolCall(row.call)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			testassert.Require(t, row.name != "id precedence over malformed extra" || strings.Contains(newErr.Error(), "non-empty id and name"), "precedence row reported the wrong failure: %v", newErr)
		})
	}

	// The validator mutates nothing, argument bytes and extras included.
	in := richToolCallFixture()
	before := richToolCallFixture()
	{
		err := ValidateToolCall(in)
		testassert.NoError(t, err)
	}
	testassert.Require(t, reflect.DeepEqual(in, before), "%v", "ValidateToolCall mutated its input")
}

// TestValidateContentPartMatchesNewContentPart pins the ContentPart
// differential over the cross-kind, unknown-kind, missing-wire-type and Extra
// boundary shapes: identical verdicts and detail, and an input the validator
// leaves exactly as it received it.
func TestValidateContentPartMatchesNewContentPart(t *testing.T) {
	rows := []struct {
		name    string
		part    ContentPart
		wantErr bool
	}{
		{"rich text", ContentPart{Kind: PartText, Text: "hello", Extra: Extra{"k": json.RawMessage(`"v"`)}}, false},
		{"empty text", ContentPart{Kind: PartText}, false},
		{"image url", ContentPart{Kind: PartImageURL, URL: "https://example.com/a.png"}, false},
		{"opaque with extra", ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking", Extra: Extra{"t": json.RawMessage(`{"n":1}`)}}, false},
		{"text carrying url", ContentPart{Kind: PartText, Text: "a", URL: "u"}, true},
		{"text carrying opaque wire type", ContentPart{Kind: PartText, Text: "a", OpaqueWireType: "thinking"}, true},
		{"image carrying text", ContentPart{Kind: PartImageURL, URL: "u", Text: "a"}, true},
		{"image carrying opaque wire type", ContentPart{Kind: PartImageURL, URL: "u", OpaqueWireType: "thinking"}, true},
		{"opaque carrying text", ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking", Text: "a"}, true},
		{"opaque carrying url", ContentPart{Kind: PartOpaque, OpaqueWireType: "thinking", URL: "u"}, true},
		{"opaque without wire type", ContentPart{Kind: PartOpaque}, true},
		{"unknown kind", ContentPart{Kind: PartKind("audio"), Text: "a"}, true},
		{"malformed extra", ContentPart{Kind: PartText, Text: "a", Extra: Extra{"raw": json.RawMessage("not json")}}, true},
		{"empty extra value", ContentPart{Kind: PartText, Text: "a", Extra: Extra{"e": json.RawMessage{}}}, true},
		{"null extra value", ContentPart{Kind: PartText, Text: "a", Extra: Extra{"n": json.RawMessage(`null`)}}, false},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, newErr := NewContentPart(row.part)
			validateErr := ValidateContentPart(row.part)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
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
}

// TestValidateToolDefinitionMatchesNewToolDefinition pins the ToolDefinition
// differential: the name check precedes the parameter check, the parameter
// shape is syntax-only (a huge number literal stays accepted), and an input
// with parameter bytes is left exactly as the validator received it.
func TestValidateToolDefinitionMatchesNewToolDefinition(t *testing.T) {
	rows := []struct {
		name    string
		def     ToolDefinition
		wantErr bool
	}{
		{"object parameters", ToolDefinition{Name: "f", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}, false},
		{"object parameters with whitespace", ToolDefinition{Name: "f", Parameters: json.RawMessage("  {\"a\": 1}  ")}, false},
		{"huge number literal is syntax-valid", ToolDefinition{Name: "f", Parameters: json.RawMessage(`{"n":1e1000}`)}, false},
		{"empty name", ToolDefinition{Parameters: json.RawMessage(`{}`)}, true},
		{"empty name precedes malformed parameters", ToolDefinition{Parameters: json.RawMessage(`{oops`)}, true},
		{"empty parameters", ToolDefinition{Name: "f"}, true},
		{"null parameters", ToolDefinition{Name: "f", Parameters: json.RawMessage(`null`)}, true},
		{"array parameters", ToolDefinition{Name: "f", Parameters: json.RawMessage(`[1]`)}, true},
		{"primitive parameters", ToolDefinition{Name: "f", Parameters: json.RawMessage(`"x"`)}, true},
		{"malformed parameters", ToolDefinition{Name: "f", Parameters: json.RawMessage(`{oops`)}, true},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, newErr := NewToolDefinition(row.def)
			validateErr := ValidateToolDefinition(row.def)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
		})
	}

	// The validator mutates nothing, parameter bytes included.
	in := ToolDefinition{Name: "echo", Description: "d", Parameters: json.RawMessage(`{"deep":{"a":[1,2]}}`)}
	before := ToolDefinition{Name: "echo", Description: "d", Parameters: json.RawMessage(`{"deep":{"a":[1,2]}}`)}
	{
		err := ValidateToolDefinition(in)
		testassert.NoError(t, err)
	}
	testassert.Require(t, reflect.DeepEqual(in, before), "%v", "ValidateToolDefinition mutated its input")
}

// TestValidateRequestMatchesNewRequest pins the Request differential over the
// message and tool passes: identical verdicts and detail, the message pass
// first in index order, the per-tool validation ahead of the duplicate-name
// rejection inside the tools pass, and an input the validator leaves exactly
// as it received it.
func TestValidateRequestMatchesNewRequest(t *testing.T) {
	validMessage := func() Message {
		return assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartText, Text: "hi"}} })
	}
	validTool := func(name string) ToolDefinition {
		return ToolDefinition{Name: name, Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}
	}

	rows := []struct {
		name     string
		req      Request
		wantErr  bool
		prefix   string // required leading text of the shared detail
		contains string // required substring of the shared detail
	}{
		{"valid messages and tools", Request{Messages: []Message{validMessage()}, Tools: []ToolDefinition{validTool("echo")}}, false, "", ""},
		{"empty request", Request{}, false, "", ""},
		{"tools only capture shape", Request{Tools: []ToolDefinition{validTool("echo")}}, false, "", ""},
		{"invalid message", Request{Messages: []Message{{Role: Role("bogus")}}}, true, "messages[0]:", ""},
		{"invalid message part", Request{Messages: []Message{{Role: RoleUser, Content: []ContentPart{{Kind: PartText, URL: "u"}}}}}, true, "messages[0]: content[0]:", ""},
		{"message error beats tool error", Request{Messages: []Message{{Role: Role("bogus")}}, Tools: []ToolDefinition{{}}}, true, "messages[0]:", ""},
		{"invalid tool", Request{Tools: []ToolDefinition{{Name: "f"}}}, true, "tools[0]:", ""},
		{"second tool invalid after a valid first", Request{Tools: []ToolDefinition{validTool("a"), {Name: "f"}}}, true, "tools[1]:", ""},
		{"duplicate name", Request{Tools: []ToolDefinition{validTool("echo"), validTool("echo")}}, true, "", "duplicate tool definition name"},
		{"duplicate reported after the second tool's own validation", Request{Tools: []ToolDefinition{validTool("echo"), {Name: "echo", Parameters: json.RawMessage(`{oops`)}}}, true, "tools[1]:", "malformed JSON parameters"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, newErr := NewRequest(row.req)
			validateErr := ValidateRequest(row.req)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			testassert.Require(t, (row.prefix == "" || strings.HasPrefix(newErr.Error(), row.prefix)) && (row.contains == "" || strings.Contains(newErr.Error(), row.contains)), "fixture %s error = %q, want the %q prefix; fixture %s reached the wrong rule: %v, want a failure naming %q", row.name, newErr, row.prefix, row.name, newErr, row.contains)
		})
	}

	// The validator mutates nothing: a value-equal sibling still compares
	// equal after the call, nested reference types included.
	in := Request{
		Messages: []Message{validMessage()},
		Tools:    []ToolDefinition{validTool("echo")},
	}
	before := Request{
		Messages: []Message{validMessage()},
		Tools:    []ToolDefinition{validTool("echo")},
	}
	{
		err := ValidateRequest(in)
		testassert.NoError(t, err)
	}
	testassert.Require(t, reflect.DeepEqual(in, before), "%v", "ValidateRequest mutated its input")
}

// Benchmarks: the validation-only path against the owning constructor over
// the same rich fixtures. The delta between each pair is the ownership work a
// read-only check no longer pays; the constructor values are the baseline
// evidence.
func BenchmarkValidateOutput(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := ValidateOutput(benchRichOutput); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNewOutput(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := NewOutput(benchRichOutput); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidateStreamDelta(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := ValidateStreamDelta(benchRichDelta); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNewStreamDelta(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := NewStreamDelta(benchRichDelta); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidateToolCall(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := ValidateToolCall(benchRichToolCall); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNewToolCall(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := NewToolCall(benchRichToolCall); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidateRequest(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := ValidateRequest(benchRichRequest); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNewRequest(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := NewRequest(benchRichRequest); err != nil {
			b.Fatal(err)
		}
	}
}
