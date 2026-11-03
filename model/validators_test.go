package model

// Validation-only validator differential: every model validator must accept
// and reject exactly what its owning constructor does — same error classes,
// precedence and detail — while retaining and mutating nothing. The
// allocation benchmarks at the bottom are the evidence that the read-only path
// no longer pays the ownership copy.

import (
	"encoding/json"
	"errors"
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

// matchesAnySentinel reports whether err matches one of the acceptable
// sentinels a row's original owner asserted.
func matchesAnySentinel(err error, sentinels ...error) bool {
	for _, sentinel := range sentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
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
