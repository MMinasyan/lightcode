package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// reservedKeys is the closed set from rule 5; it must match production order-independently.
var reservedKeySet = []string{"model", "messages", "tools", "tool_choice", "stream", "stream_options", "max_tokens", "max_completion_tokens", "n"}

func TestReservedKeysRejectedInEverySidecarLayer(t *testing.T) { // rule 5: presence in ANY of the three layers rejects encoding.
	req := Request{Messages: []Message{userText("hi")}} // a valid request so only reserved keys can fail it.
	forgedValue := json.RawMessage(`"forged"`)          // one value reused across all subtests; Encode must copy, not alias.

	type layerCase struct {
		name  string
		apply func(*ResolvedTransport) map[string]json.RawMessage // returns the runtime extras for this case (nil when unused).
	}
	cases := []layerCase{
		{"provider", func(rt *ResolvedTransport) map[string]json.RawMessage { rt.ProviderExtraBody = Extra{}; return nil }},
		{"model", func(rt *ResolvedTransport) map[string]json.RawMessage { rt.ModelExtraBody = Extra{}; return nil }},
		{"runtime", func(_ *ResolvedTransport) map[string]json.RawMessage { return map[string]json.RawMessage{} }},
	}

	for _, tc := range cases {
		for _, key := range reservedKeySet { // every layer x every reserved key.
			t.Run(tc.name+"/"+key, func(t *testing.T) {
				rt := testResolved() // valid baseline; only the one forged key differs per subtest.
				runtimeExtras := tc.apply(&rt)
				switch { // place the single forged key into this case's live layer.
				case runtimeExtras != nil:
					runtimeExtras[key] = json.RawMessage(forgedValue)
				default:
					if rt.ProviderExtraBody == nil && rt.ModelExtraBody == nil { // unreachable; guards against a broken table above.
						t.Fatal("table produced no live layer")
					} else if rt.ProviderExtraBody != nil {
						rt.ProviderExtraBody[key] = json.RawMessage(forgedValue)
					} else {
						rt.ModelExtraBody[key] = json.RawMessage(forgedValue)
					}
				}

				body, _, err := Encode(rt, req, runtimeExtras) // must reject before producing a body.
				if err == nil {                                // no error at all is the failure mode under test.
					t.Fatalf("reserved key %q in layer %s was not rejected (body=%s)", key, tc.name, string(body))
				}
				var reserved *reservedKeyError // typed identity with exactly this one offending key.
				if !errors.As(err, &reserved) || !errors.Is(err, ErrReservedKeys) {
					t.Fatalf("error is not the typed/sentinel reserved-key error: %v", err)
				}
				if len(reserved.keys) != 1 || reserved.keys[0] != key { // one and only this key.
					t.Fatalf("keys = %#v want [%s]", reserved.keys, key)
				}

			})
		}
	}

	forgedValue[1] = 'X'                                           // corrupt the shared fixture after all subtests; none may have aliased it into retained state.
	if _, _, err := Encode(testResolved(), req, nil); err != nil { // a clean encode still succeeds afterwards (no cross-call state).
		t.Fatalf("clean encode failed after rejection storm: %v", err)
	}
}

// TestReservedErrorIdentifiesEveryPresentKey pins rule 5's multi-key identification across all three layers at once.
func TestReservedErrorIdentifiesEveryPresentKey(t *testing.T) { // distinct keys per layer, one duplicate on purpose.
	req := Request{Messages: []Message{userText("hi")}}

	rt := testResolved() // provider n; model stream_options + model (identity field); runtime max_tokens (+n duplicate).
	rt.ProviderExtraBody = Extra{"n": json.RawMessage(`7`)}
	rt.ModelExtraBody = Extra{"stream_options": json.RawMessage(`{}`), "model": json.RawMessage(`"x"`)}
	runtimeExtras := map[string]json.RawMessage{"max_tokens": json.RawMessage(`1`), "n": json.RawMessage(`2`)} // n duplicated from the provider layer.

	body, _, err := Encode(rt, req, runtimeExtras) // one error identifies every present reserved key exactly once each (set semantics).

	var reserved *reservedKeyError // typed shape carries the full distinct set.
	if !errors.As(err, &reserved) || !errors.Is(err, ErrReservedKeys) {
		t.Fatalf("expected one typed reserved-key error, got %v (body=%s)", err, string(body))
	}

	got := map[string]int{} // duplicates must collapse; every distinct present key appears.
	for _, k := range reserved.keys {
		if got[k]++; got[k] > 1 {
			t.Fatalf("key %q reported more than once: %#v", k, reserved.keys)
		}
	}
	wantSet := map[string]bool{"n": true, "stream_options": true, "model": true, "max_tokens": true} // four distinct keys across three layers.
	if len(reserved.keys) != len(wantSet) {                                                          // absent keys (messages/tools/tool_choice/stream/max_completion_tokens) are not reported.
		t.Fatalf("keys = %#v want exactly %d distinct present reserved keys", reserved.keys, len(wantSet))
	}

	for k := range got { // every reported key is one of the four expected ones.
		if !wantSet[k] {
			t.Fatalf("unexpected key %q in error: %#v", k, reserved.keys)
		}
	}

	msg := err.Error() // text identifies each present key (order itself is diagnostic and not pinned).
	for _, k := range []string{"n", "stream_options", "model", "max_tokens"} {
		if !strings.Contains(msg, `"`+k+`"`) && !strings.Contains(msg, k) {
			t.Fatalf("error text does not identify %q: %s", k, msg)
		}
	}

	if len(body) != 0 { // a rejected encode returns no body bytes (diagnostics and outcome are separate from rejection).
		t.Fatalf("rejection produced a partial body: %s", string(body))
	}
}

// TestReservedKeysIdentifiedDespiteMalformedValues pins rule 5's identification guarantee when any layer also carries an invalid JSON value: one presence-only reserved-key pass across all three layers runs before raw-value validation, so malformed values cannot hide present reserved keys.
func TestReservedKeysIdentifiedDespiteMalformedValues(t *testing.T) {
	req := Request{Messages: []Message{userText("hi")}}

	t.Run("valid-reserved-plus-malformed-sibling-layer", func(t *testing.T) {
		rt := testResolved()                                                        // baseline layers are empty; only the two forged entries below differ.
		rt.ProviderExtraBody = Extra{"max_tokens": json.RawMessage(`1`)}            // presence is what must be identified, regardless of sibling values elsewhere.
		runtimeExtras := map[string]json.RawMessage{"junk_key": []byte("not-json")} // malformed non-reserved value in a different layer than the reserved one.

		body, _, err := Encode(rt, req, runtimeExtras) // rejection names the present reserved key even though another layer fails raw-value validation too.
		var reserved *reservedKeyError                 // typed shape carries exactly the distinct present keys.
		if !errors.As(err, &reserved) || !errors.Is(err, ErrReservedKeys) {
			t.Fatalf("want typed reserved-key error despite the malformed sibling value, got %v (body=%s)", err, string(body))
		}
		if len(reserved.keys) != 1 || reserved.keys[0] != "max_tokens" { // one and only this key; the malformed non-reserved entry is not a reservation.
			t.Fatalf("keys = %#v want [max_tokens]", reserved.keys)
		}
	})

	t.Run("malformed-reserved-value-plus-second-layer-key", func(t *testing.T) {
		rt := testResolved()                                           // baseline layers are empty; only the two forged entries below differ.
		rt.ModelExtraBody = Extra{"stream_options": []byte("{broken")} // reserved key whose own value is malformed — identification must not depend on parsing it.
		runtimeExtras := map[string]json.RawMessage{"n": json.RawMessage(`2`)}

		body, _, err := Encode(rt, req, runtimeExtras) // both present keys identified in one pass before any raw-value validation runs.
		var reserved *reservedKeyError                 // typed shape carries the full distinct set regardless of value validity.
		if !errors.As(err, &reserved) || !errors.Is(err, ErrReservedKeys) {
			t.Fatalf("want typed reserved-key error despite the malformed reserved value, got %v (body=%s)", err, string(body))
		}
		got := map[string]int{} // set comparison with a duplicate check; key order is diagnostic formatting only.
		for _, k := range reserved.keys {
			if got[k]++; got[k] > 1 {
				t.Fatalf("key %q reported more than once: %#v", k, reserved.keys)
			}
		}
		wantSet := map[string]bool{"stream_options": true, "n": true} // exactly the two present keys; nothing else.
		if len(got) != len(wantSet) {
			t.Fatalf("keys = %#v want exactly %d distinct present reserved keys", reserved.keys, len(wantSet))
		}
		for k := range got {
			if !wantSet[k] {
				t.Fatalf("unexpected key %q in error: %#v", k, reserved.keys)
			}
		}
	})
}
