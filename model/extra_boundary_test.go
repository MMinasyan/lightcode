package model

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestAcceptingBoundariesRejectInvalidExtraJSON pins that every accepting
// boundary rejects Extra values whose raw bytes are not complete, syntactically
// valid JSON — both malformed nonempty bytes and empty raw values, which are
// absent data rather than a value and must never reach stored state where
// finalization could count them as payload. The union keeps each original
// boundary/raw-value pair; the two shapes are not a cross product. Raw
// tool-call arguments remain the only malformed-JSON exception (pinned in the
// ToolCall cases).
func TestAcceptingBoundariesRejectInvalidExtraJSON(t *testing.T) {
	malformed := func() json.RawMessage { return json.RawMessage("not json") }
	empty := func() json.RawMessage { return json.RawMessage{} }
	extra := func(raw func() json.RawMessage) Extra { return Extra{"raw": raw()} }

	cases := []struct {
		name string
		raw  func() json.RawMessage
		call func(Extra) error
	}{
		{"content part boundary rejects", malformed, func(e Extra) error {
			_, err := NewContentPart(ContentPart{Kind: PartText, Text: "a", Extra: e})
			return err
		}},
		{"content part boundary rejects empty value", empty, func(e Extra) error {
			_, err := NewContentPart(ContentPart{Kind: PartText, Text: "a", Extra: e})
			return err
		}},
		{"tool call boundary rejects", malformed, func(e Extra) error {
			_, err := NewToolCall(ToolCall{ID: "c1", Name: "f", Extra: e})
			return err
		}},
		{"tool call boundary rejects empty value", empty, func(e Extra) error {
			_, err := NewToolCall(ToolCall{ID: "c1", Name: "f", Extra: e})
			return err
		}},
		{"message boundary rejects", malformed, func(e Extra) error {
			_, err := NewMessage(Message{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "hi"}}, Extra: e})
			return err
		}},
		{"message boundary rejects empty value", empty, func(e Extra) error {
			_, err := NewMessage(Message{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "hi"}}, Extra: e})
			return err
		}},
		{"request boundary rejects message extra", malformed, func(e Extra) error {
			_, err := NewRequest(Request{Messages: []Message{{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "hi"}}, Extra: e}}})
			return err
		}},
		{"request boundary rejects part extra", malformed, func(e Extra) error {
			_, err := NewRequest(Request{Messages: []Message{{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Extra: e}}}}})
			return err
		}},
		{"request boundary rejects embedded tool call empty value", empty, func(e Extra) error {
			_, err := NewRequest(Request{Messages: []Message{{Role: RoleAssistant, Source: fullRef, ToolCalls: []ToolCall{{ID: "c1", Name: "f", Extra: e}}}}})
			return err
		}},
		{"output boundary rejects", malformed, func(e Extra) error {
			_, err := NewOutput(Output{Status: OutputErrored, Source: fullRef, Detail: "d", Message: &Message{Role: RoleAssistant, Source: fullRef, Extra: e}})
			return err
		}},
		{"output boundary rejects empty value", empty, func(e Extra) error {
			_, err := NewOutput(Output{Status: OutputErrored, Source: fullRef, Detail: "d", Message: &Message{Role: RoleAssistant, Source: fullRef, Extra: e}})
			return err
		}},
		{"delta boundary rejects content fragment scope", malformed, func(e Extra) error {
			_, err := NewStreamDelta(StreamDelta{ContentFragments: []ContentFragment{{Position: 0, Kind: PartText, Extra: e}}})
			return err
		}},
		{"delta boundary rejects content fragment scope empty value", empty, func(e Extra) error {
			_, err := NewStreamDelta(StreamDelta{ContentFragments: []ContentFragment{{Position: 0, Kind: PartText, Extra: e}}})
			return err
		}},
		{"delta boundary rejects tool call fragment scope", malformed, func(e Extra) error {
			_, err := NewStreamDelta(StreamDelta{ToolFragments: []ToolCallFragment{{ID: "c1", Name: "f", Extra: e}}})
			return err
		}},
		{"delta boundary rejects tool call fragment scope empty value", empty, func(e Extra) error {
			_, err := NewStreamDelta(StreamDelta{ToolFragments: []ToolCallFragment{{ID: "c1", Name: "f", Extra: e}}})
			return err
		}},
		{"delta boundary rejects message extra scope", malformed, func(e Extra) error {
			_, err := NewStreamDelta(StreamDelta{MessageExtra: e})
			return err
		}},
		{"delta boundary rejects message extra scope empty value", empty, func(e Extra) error {
			_, err := NewStreamDelta(StreamDelta{MessageExtra: e})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(extra(tc.raw))
			testassert.Require(t, err != nil, "%s accepted invalid extra bytes", tc.name)
		})
	}

	// The null literal is complete JSON: construction accepts it and the
	// Message boundary retains the extra entry (finalization drops it later).
	nullIsCompleteJSON := Extra{"n": json.RawMessage(`null`)}
	{
		_, err := NewContentPart(ContentPart{Kind: PartText, Text: "a", Extra: nullIsCompleteJSON})
		testassert.NoError(t, err)
	}
	{
		m, err := NewMessage(Message{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "hi"}}, Extra: nullIsCompleteJSON})
		testassert.Require(t, err == nil && len(m.Extra) == 1, "NewMessage rejected or dropped a null extra value (err %v): %#v", err, m.Extra)
	}
	{
		_, err := NewStreamDelta(StreamDelta{MessageExtra: nullIsCompleteJSON})
		testassert.NoError(t, err)
	}
}
