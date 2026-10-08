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

// requireEqualConstructorVerdict asserts the differential: the owning
// constructor and the validation-only validator return the same acceptance
// verdict with byte-identical error detail for one input.
func requireEqualConstructorVerdict(t *testing.T, name string, newErr, validateErr error) {
	t.Helper()
	if (newErr == nil) != (validateErr == nil) {
		t.Fatalf("%s: New error = %v, Validate error = %v; verdicts diverge", name, newErr, validateErr)
	}
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
			if (newErr != nil) != row.wantErr {
				t.Fatalf("fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			}
			if row.errContains != "" && !strings.Contains(newErr.Error(), row.errContains) {
				t.Fatalf("fixture %s reached the wrong rule: %v, want a failure naming %q", row.name, newErr, row.errContains)
			}
		})
	}

	// The validator mutates nothing: an independently built value-equal
	// sibling compares equal after the call, nested reference types included.
	in := richOutputFixture()
	before := richOutputFixture()
	if err := ValidateOutput(in); err != nil {
		t.Fatalf("ValidateOutput rejected the rich fixture: %v", err)
	}
	if !reflect.DeepEqual(in, before) {
		t.Fatal("ValidateOutput mutated its input")
	}
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
			if (newErr != nil) != row.wantErr {
				t.Fatalf("fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			}
		})
	}

	// The validator mutates nothing, nested fragments and extras included.
	in := richDeltaFixture()
	before := richDeltaFixture()
	if err := ValidateStreamDelta(in); err != nil {
		t.Fatalf("ValidateStreamDelta rejected the rich fixture: %v", err)
	}
	if !reflect.DeepEqual(in, before) {
		t.Fatal("ValidateStreamDelta mutated its input")
	}
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
			if (newErr != nil) != row.wantErr {
				t.Fatalf("fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			}
			if row.name == "id precedence over malformed extra" && !strings.Contains(newErr.Error(), "non-empty id and name") {
				t.Fatalf("precedence row reported the wrong failure: %v", newErr)
			}
		})
	}

	// The validator mutates nothing, argument bytes and extras included.
	in := richToolCallFixture()
	before := richToolCallFixture()
	if err := ValidateToolCall(in); err != nil {
		t.Fatalf("ValidateToolCall rejected the rich fixture: %v", err)
	}
	if !reflect.DeepEqual(in, before) {
		t.Fatal("ValidateToolCall mutated its input")
	}
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
