package model

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestWireSystemRoleMapping pins rule 7: canonical system messages encode with the resolved wire role exactly; every other canonical role is unchanged no matter what that value is. Empty resolves to "system"; any non-empty value outside {system, user, developer} rejects encoding before producing a body (ErrInvalidWireSystemRole).
func TestWireSystemRoleMapping(t *testing.T) {
	sys := mustMsg(t, Message{Role: RoleSystem, Content: []ContentPart{{Kind: PartText, Text: "be brief"}}}) // system is the only role whose wire value may differ from its stored one.

	cases := []struct {
		name string // row label for subtest output readability (no semantic content beyond that).
		role string // resolved WireSystemRole input under test ("": unset baseline handled by defaulting logic).
		want string // exact wire role value expected on the system message for this input.
	}{
		{"empty-defaults-to-system", "", "system"},                        // unset behaves exactly like an explicit system role (no error, no invented behavior beyond the stated default itself).
		{"explicit-system-unchanged", "system", "system"},                 // identity mapping for the most common production configuration value.
		{"developer-maps-system-messages-only", "developer", "developer"}, // system adopts developer; non-system roles below stay unchanged on this same encode call (both halves of rule 7 from one body).
		{"user-maps-system-messages-only", "user", "user"},                // system adopts user; every other role likewise unchanged alongside it.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := testResolved()
			rt.WireSystemRole = tc.role

			req := Request{Messages: []Message{sys, userText("hello"), mustMsg(t, Message{Role: RoleAssistant, Source: targetRef, Content: []ContentPart{{Kind: PartText, Text: "hi"}}})}}
			body, _, err := Encode(rt, req, nil)
			testassert.Require(t, err == nil, "Encode returned unexpected error for wire role %q: %v", tc.role, err)

			msgs := decodeMessages(t, body)
			{
				raw, ok := wireString(t, msgs[0], "role")
				testassert.Require(t, ok && raw == tc.want, "system wire role = %q, want %q", msgs[0]["role"], tc.want)
			}

			{
				raw, ok := wireString(t, msgs[1], "role")
				testassert.Require(t, ok && raw == "user", "user wire role = %q, want unchanged user", msgs[1]["role"])
			}

			{
				raw, ok := wireString(t, msgs[2], "role")
				testassert.Require(t, ok && raw == "assistant", "assistant wire role = %q, want unchanged assistant", msgs[2]["role"])
			}
		})
	}

	t.Run("invalid-wire-role-rejected", func(t *testing.T) {
		for _, bad := range []string{"tool", "SYSTEM", "Developer ", "systemx"} {
			rt := testResolved()
			rt.WireSystemRole = bad
			_, _, err := Encode(rt, Request{Messages: []Message{sys}}, nil)
			testassert.Require(t, err != nil && errors.Is(err, ErrInvalidWireSystemRole), "Encode accepted invalid wire role %q; error %v does not wrap ErrInvalidWireSystemRole", bad, err)
		}
	})
}

// TestProtocolWarningsOrderAndSuppression pins rule 12: must-preserve warnings fire exactly for replay-kept assistant-with-tool-calls messages missing each listed field, ordered by message index then the MustPreserve list within that message; raw extra presence suppresses per-field emission on its own without affecting sibling fields.
func TestProtocolWarningsOrderAndSuppression(t *testing.T) {
	rt := testResolved()
	rt.MustPreserve = []string{"preserve_a", "preserve_b"}
	mkAssistant := func(source ModelRef, extraKeys ...string) Message {
		call, err := NewToolCall(ToolCall{ID: "c-warn", Name: "lookup", Arguments: json.RawMessage(`{}`)}) // completed-call boundary clones argument bytes like production paths do.
		testassert.NoError(t, err)
		msg := Message{Role: RoleAssistant, Source: source, Content: []ContentPart{{Kind: PartText, Text: "x"}}, ToolCalls: []ToolCall{call}} // assistant carries a completed tool call so rule 12's must-preserve branch applies to this fixture.
		if len(extraKeys) > 0 {                                                                                                               // fresh Message has no Extra map; writing into an absent one panics and NewMessage does not pre-allocate it here.
			msg.Extra = Extra{}
		}

		for _, k := range extraKeys {
			msg.Extra[k] = json.RawMessage(`1`) // opaque numeric marker kept inert by the warning path; only its presence is observed below (per-field suppression).
		}
		out, err := NewMessage(msg)
		if err != nil {
			t.Fatalf("NewMessage(warning fixture source %s): %v", source.String(), err)
		}
		return out
	}

	msgs := []Message{
		mkAssistant(targetRef, "preserve_a"), mkAssistant(targetRef),
		mkAssistant(foreignModel, "preserve_a", "preserve_b")}

	req, err := NewRequest(Request{Messages: msgs})
	testassert.NoError(t, err)

	body, warnings, err := Encode(rt, req, nil)
	testassert.NoError(t, err)

	testassert.Require(t, body != nil, "Encode returned nil body on success path (warnings=%d)", len(warnings))

	testassert.Require(t, len(warnings) == 3, "got %d warnings, want exactly 3: %#v", len(warnings), warnings)

	type expectedWarning struct {
		field string
		idx   int
	}

	want := []expectedWarning{
		{field: "preserve_b", idx: 0}, {field: "preserve_a", idx: 1}, {field: "preserve_b", idx: 1}}

	for i, w := range warnings {
		testassert.Require(t, w.Kind == WarningMustPreserveMissing, "warnings[%d].Kind = %q, want %q", i, w.Kind, WarningMustPreserveMissing)

		if w.Target != rt.Model {
			t.Fatalf("warnings[%d].Target = %s, want %s", i, w.Target.String(), rt.Model.String())
		}

		testassert.Require(t, w.Field == want[i].field && w.MessageIndex == want[i].idx && w.MessageIndex >= 0, "warnings[%d] = {field:%q idx:%d}, want {field:%q idx:%d}; warnings[%d].MessageIndex negative: %d", i, w.Field, w.MessageIndex, want[i].field, want[i].idx, i, w.MessageIndex)
	}

	t.Run("no-warnings-when-mustpreserve-unset", func(t *testing.T) {
		rt2 := testResolved()
		reqNoWarnings, err := NewRequest(Request{Messages: []Message{mkAssistant(targetRef)}})
		testassert.NoError(t, err)

		_, warnings2, err := Encode(rt2, reqNoWarnings, nil)
		testassert.NoError(t, err)

		testassert.Require(t, len(warnings2) == 0, "got %d warnings with unset must-preserve list, want 0: %#v", len(warnings2), warnings2)
	})
}
