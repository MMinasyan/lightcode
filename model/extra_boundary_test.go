package model

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestAcceptingBoundariesRejectMalformedExtraJSON pins that every accepting boundary rejects Extra values whose raw bytes are not syntactically valid JSON, while the shared validation path keeps deep-copy ownership on success. Raw tool-call arguments remain the only malformed-JSON exception (pinned in ToolCall tests).
func TestAcceptingBoundariesRejectMalformedExtraJSON(t *testing.T) {
	bad := Extra{"raw": json.RawMessage("not json")}

	t.Run("content part boundary rejects", func(t *testing.T) {
		{
			_, err := NewContentPart(ContentPart{Kind: PartText, Text: "a", Extra: bad})
			testassert.Require(t, err != nil, "%v", "NewContentPart accepted malformed extra bytes")
		}
	})

	t.Run("tool call boundary rejects", func(t *testing.T) {
		{
			_, err := NewToolCall(ToolCall{ID: "c1", Name: "f", Extra: bad})
			testassert.Require(t, err != nil, "%v", "NewToolCall accepted malformed extra bytes")
		}
	})

	msgIn := Message{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "hi"}}, Extra: bad}
	t.Run("message boundary rejects", func(t *testing.T) {
		{
			_, err := NewMessage(msgIn)
			testassert.Require(t, err != nil, "%v", "NewMessage accepted malformed extra bytes")
		}
	})

	reqMsgs := []Message{{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "hi"}}, Extra: bad}}
	reqPartsBad := Request{Messages: []Message{{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Extra: bad}}}}}
	t.Run("request boundary rejects message extra", func(t *testing.T) {
		{
			_, err := NewRequest(Request{Messages: reqMsgs})
			testassert.Require(t, err != nil, "%v", "NewRequest accepted malformed message extra bytes")
		}
	})
	t.Run("request boundary rejects part extra", func(t *testing.T) {
		{
			_, err := NewRequest(reqPartsBad)
			testassert.Require(t, err != nil, "%v", "NewRequest accepted malformed content-part extra bytes")
		}
	})

	outMsg := assistantMsg(t, func(m *Message) { m.Extra = bad })
	outIn := Output{Status: OutputErrored, Source: fullRef, Message: &outMsg, Detail: "d"}
	t.Run("output boundary rejects", func(t *testing.T) {
		{
			_, err := NewOutput(outIn)
			testassert.Require(t, err != nil, "%v", "NewOutput accepted malformed message extra bytes")
		}
	})

	deltaCases := []struct {
		name string
		d    StreamDelta
	}{
		{name: "content fragment scope", d: StreamDelta{ContentFragments: []ContentFragment{{Position: 0, Kind: PartText, Extra: bad}}}},
		{name: "tool call fragment scope", d: StreamDelta{ToolFragments: []ToolCallFragment{{ID: "c1", Name: "f", Extra: bad}}}},
		{name: "message extra scope", d: StreamDelta{MessageExtra: bad}},
	}
	for _, tc := range deltaCases {
		t.Run("delta boundary rejects "+tc.name, func(t *testing.T) {
			{
				_, err := NewStreamDelta(tc.d)
				testassert.Require(t, err != nil, "NewStreamDelta accepted malformed extra bytes at %s", tc.name)
			}
		})
	}

	validExtra := Extra{"k": json.RawMessage(`"v"`)}
	validIn2 := ContentPart{Kind: PartText, Text: "a", Extra: validExtra}
	validPart, err := NewContentPart(validIn2)
	testassert.Require(t, err == nil && validPart.Kind == PartText, "NewContentPart rejected a part with valid extra bytes (err %v): %#v", err, validPart)

	srcRaw := []byte(`"v"`)
	goodIn := ContentPart{Kind: PartText, Text: "b", Extra: Extra{"k": json.RawMessage(srcRaw)}}
	outGood, err := NewContentPart(goodIn)
	testassert.NoError(t, err)
	srcRaw[1] = 'X' // corrupt caller bytes after a successful validation+copy.
	stored := string(outGood.Extra["k"])
	testassert.Require(t, stored[:2] == `"v`, "deep-copy ownership lost on the valid path: %s", outGood.Extra["k"])
}

// TestAcceptingBoundariesRejectEmptyExtraValues pins that an empty json.RawMessage value is not complete JSON and is therefore rejected at every accepting boundary (it must never reach stored state where finalization could count it as payload), while the null literal — which IS complete JSON — still passes construction.
func TestAcceptingBoundariesRejectEmptyExtraValues(t *testing.T) {
	empty := Extra{"e": json.RawMessage{}}

	{
		_, err := NewContentPart(ContentPart{Kind: PartText, Text: "a", Extra: empty})
		testassert.Require(t, err != nil, "%v", "NewContentPart accepted an empty extra value")
	}
	{
		_, err := NewToolCall(ToolCall{ID: "c1", Name: "f", Extra: empty})
		testassert.Require(t, err != nil, "%v", "NewToolCall accepted an empty extra value")
	}
	msgIn := Message{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "hi"}}, Extra: empty}
	{
		_, err := NewMessage(msgIn)
		testassert.Require(t, err != nil, "%v", "NewMessage accepted a message with an empty extra value")
	}
	reqCallMsg := assistantMsg(t, func(m *Message) { m.ToolCalls = []ToolCall{{ID: "c1", Name: "f", Extra: empty}} })
	{
		_, err := NewRequest(Request{Messages: []Message{reqCallMsg}})
		testassert.Require(t, err != nil, "%v", "NewRequest accepted an embedded tool call with an empty extra value")
	}
	outMsg := assistantMsg(t, func(m *Message) { m.Extra = empty })
	outIn := Output{Status: OutputErrored, Source: fullRef, Message: &outMsg, Detail: "d"}
	{
		_, err := NewOutput(outIn)
		testassert.Require(t, err != nil, "%v", "NewOutput accepted a message with an empty extra value")
	}
	deltaCases := []struct {
		name string
		d    StreamDelta
	}{
		{name: "content fragment scope", d: StreamDelta{ContentFragments: []ContentFragment{{Position: 0, Kind: PartText, Extra: empty}}}},
		{name: "tool call fragment scope", d: StreamDelta{ToolFragments: []ToolCallFragment{{ID: "c1", Name: "f", Extra: empty}}}},
		{name: "message extra scope", d: StreamDelta{MessageExtra: empty}},
	}
	for _, tc := range deltaCases {
		t.Run("delta boundary rejects "+tc.name, func(t *testing.T) {
			{
				_, err := NewStreamDelta(tc.d)
				testassert.Require(t, err != nil, "NewStreamDelta accepted an empty extra value at %s", tc.name)
			}
		})
	}

	nullIsCompleteJSON := Extra{"n": json.RawMessage(`null`)} // null is complete JSON; construction accepts it, finalization drops it.
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
