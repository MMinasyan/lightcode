package model

import (
	"encoding/json"
	"testing"
)

// keptAssistant returns a same-source assistant message with an optional tool call.
func keptAssistant(t *testing.T, source ModelRef, withCall bool) Message {
	t.Helper() // fixtures go through the public constructor like real callers do.
	msg := Message{Role: RoleAssistant, Content: []ContentPart{{Kind: PartText, Text: "same"}}, Source: source}
	if withCall {
		c, _ := NewToolCall(ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)})
		msg.ToolCalls = []ToolCall{c}
	}
	out, err := NewMessage(msg)
	if err != nil {
		t.Fatalf("NewMessage failed for source %s: %v", source.String(), err)
	}
	return out
}

// withExtra returns a validated copy of msg carrying one extra marker field.
func withExtra(t *testing.T, in Message, key string) Message {
	in.Extra = Extra{key: json.RawMessage(`"kept"`)} // re-run construction so ownership/clone rules match production paths for every fixture in this file (NewMessage deep-copies extras at its accepting boundary).
	out, err := NewMessage(in)
	if err != nil {
		t.Fatalf("NewMessage with extra %q failed: %v", key, err)
	}
	return out
}

// TestReplayPolicyMatrix pins rule 8 across every axis of the keep/strip decision; target is openai/gpt-test and each row varies exactly one input.
func TestReplayPolicyMatrix(t *testing.T) {
	cases := []struct {
		name         string
		source       ModelRef            // assistant source identity under test.
		fams         map[ModelRef]string // SourceFamilies data for this encode call.
		targetFamily string              // target protocol family metadata piece.
		wantKeep     bool                // whether the marker extra survives onto the wire object.
	}{
		{"same-model-no-family-data", ModelRef{Provider: "openai", Model: "gpt-test"}, nil, "", true},
		{"cross-model-equal-families", otherSameProv, map[ModelRef]string{otherSameProv: "fam-t"}, "fam-t", true},
		{"cross-model-source-unknown", ModelRef{Provider: "openai", Model: "gpt-x"}, nil, "fam-t", false},
		{"target-family-empty-strips", otherSameProv, map[ModelRef]string{otherSameProv: "fam-a"}, "", false}, {"source-family-blank-entry", otherSameProv, map[ModelRef]string{otherSameProv: "  "}, "fam-t", false}, // whitespace is not a family; exact non-empty equality only.
		{"cross-provider-equal-families-strips", foreignModel, map[ModelRef]string{foreignModel: "fam-t"}, "fam-t", false}, // equal families across providers still strip (provider identity is the first gate; family equality only applies within one provider).
		{"same-provider-different-families-strips", otherSameProv, map[ModelRef]string{otherSameProv: "fam-a"}, "fam-b", false}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := testResolved() // baseline identity; the row supplies family data and target metadata.
			rt.SourceFamilies = tc.fams
			rt.ProtocolFamily = tc.targetFamily
			msg := withExtra(t, keptAssistant(t, tc.source, false), "marker")

			body := mustEncode(t, rt, Request{Messages: []Message{msg}})
			objs := decodeMessages(t, body) // exactly one message for this fixture on every row.
			if _, ok := objs[0]["marker"]; (ok && !tc.wantKeep) || (!ok && tc.wantKeep) {
				t.Fatalf("marker present=%v want keep=%v: %#v", ok, tc.wantKeep, objs[0]) // wire presence is the whole observable policy outcome here.
			}
		})
	}
}

// TestTargetDropSetAppliesAtEveryRetainedScope pins rule 8's final clause: whenever extras are kept, the target drop set removes its keys from every retained scope (message, content part, tool call); non-dropped sibling keys survive alongside them. Both retention branches (same-model and cross-model-equal-family) must honor the same drop set; under strip nothing is emitted anyway so drops have no extra observable effect there.
func TestTargetDropSetAppliesAtEveryRetainedScope(t *testing.T) {
	cases := []struct {
		name   string
		source ModelRef // assistant source identity selecting which keep branch of the replay policy applies to this fixture's extras during encoding below.
		fams   map[ModelRef]string
	}{
		{"same-model-kept", targetRef, nil},
		{"cross-model-equal-families-kept", otherSameProv, map[ModelRef]string{otherSameProv: "fam-t"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rtRow := testResolved()
			rtRow.Drop = map[string]bool{"drop_me": true}
			rtRow.SourceFamilies = tc.fams
			rtRow.ProtocolFamily = "fam-t"
			msg := withExtra(t, keptAssistant(t, tc.source, false), "drop_me")
			for i, part := range msg.Content {
				part.Extra = Extra{"drop_me": json.RawMessage(`1`), "keep_me": json.RawMessage(`2`)}
				msg.Content[i] = part
			}

			call, err := NewToolCall(ToolCall{ID: "c-drop", Name: "lookup", Arguments: json.RawMessage(`{}`), Extra: Extra{"drop_me": json.RawMessage(`3`), "keep_me": json.RawMessage(`4`)}})
			if err != nil {
				t.Fatalf("NewToolCall(drop-scope fixture): %v", err)
			}

			msg.ToolCalls = append(msg.ToolCalls, call)

			msg.Extra["keep_me"] = json.RawMessage(`"m-kept"`)
			validated, err := NewMessage(msg)
			if err != nil {
				t.Fatalf("NewMessage(drop-scope message): %v", err)
			}

			body := mustEncode(t, rtRow, Request{Messages: []Message{validated}})
			msgs := decodeMessages(t, body)
			obj := msgs[0]

			if _, ok := obj["drop_me"]; ok {
				t.Fatalf("dropped message-scope key survived onto the wire: %#v", obj)
			}

			if _, ok := wireString(t, obj, "keep_me"); !ok {
				t.Fatalf("non-dropped message-scope key lost during drop-set application: %#v", obj)
			}

			parts := unmarshalContentParts(t, obj)
			if _, ok := parts[0]["drop_me"]; ok {
				t.Fatalf("dropped content-part-scope key survived onto the wire: %#v", parts[0])
			}

			if _, ok := parts[0]["keep_me"]; !ok {
				t.Fatalf("non-dropped content-part-scope key lost during drop-set application: %#v", parts[0])
			}

			var calls []map[string]json.RawMessage
			if err := json.Unmarshal(obj["tool_calls"], &calls); err != nil || len(calls) != 1 {
				t.Fatalf("tool_calls not a one-object array: %s", obj["tool_calls"])
			}

			if _, ok := calls[0]["drop_me"]; ok {
				t.Fatalf("dropped tool-call-scope key survived onto the wire: %#v", calls[0])
			}

			if _, ok := calls[0]["keep_me"]; !ok {
				t.Fatalf("non-dropped tool-call-scope key lost during drop-set application: %#v", calls[0])
			}
		})
	}
}
