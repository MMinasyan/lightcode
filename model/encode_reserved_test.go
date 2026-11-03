package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
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
				// no error at all is the failure mode under test.
				testassert.Require(t, err != nil, "reserved key %q in layer %s was not rejected (body=%s)", key, tc.name, string(body))
				var reserved *ReservedKeyError // typed identity with exactly this one offending key.
				testassert.Require(t, errors.As(err, &reserved) && errors.Is(err, ErrReservedKeys), "error is not the typed/sentinel reserved-key error: %v", err)
				// one and only this key.
				testassert.Require(t, len(reserved.Keys) == 1 && reserved.Keys[0] == key, "keys = %#v want [%s]", reserved.Keys, key)

			})
		}
	}

	forgedValue[1] = 'X' // corrupt the shared fixture after all subtests; none may have aliased it into retained state.
	// a clean encode still succeeds afterwards (no cross-call state).
	{
		_, _, err := Encode(testResolved(), req, nil)
		testassert.NoError(t, err)
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

	var reserved *ReservedKeyError // typed shape carries the full distinct set.
	testassert.Require(t, errors.As(err, &reserved) && errors.Is(err, ErrReservedKeys), "expected one typed reserved-key error, got %v (body=%s)", err, string(body))

	got := map[string]int{} // duplicates must collapse; every distinct present key appears.
	for _, k := range reserved.Keys {
		got[k]++
		testassert.Require(t, got[k] <= 1, "key %q reported more than once: %#v", k, reserved.Keys)
	}
	wantSet := map[string]bool{"n": true, "stream_options": true, "model": true, "max_tokens": true} // four distinct keys across three layers.
	// absent keys (messages/tools/tool_choice/stream/max_completion_tokens) are not reported.
	testassert.Require(t, len(reserved.Keys) == len(wantSet), "keys = %#v want exactly %d distinct present reserved keys", reserved.Keys, len(wantSet))

	for k := range got { // every reported key is one of the four expected ones.
		testassert.Require(t, wantSet[k], "unexpected key %q in error: %#v", k, reserved.Keys)
	}

	msg := err.Error() // text identifies each present key (order itself is diagnostic and not pinned).
	// THE retained legacy rendering pinned exactly: unreserved prefix plus bare key names, never the sentinel's own text as a prefix and never quoted keys.
	{
		prefix := "reserved keys in extra body: "
		testassert.Require(t, strings.HasPrefix(msg, prefix), "error text lost the retained legacy prefix %q: %s", prefix, msg)
	}
	for _, k := range []string{"n", "stream_options", "model", "max_tokens"} {
		// each present key appears bare in the joined list (order between keys is diagnostic and not pinned).
		testassert.Require(t, strings.Contains(msg, k), "error text does not identify %q: %s", k, msg)
	}

	// a rejected encode returns no body bytes (diagnostics and outcome are separate from rejection).
	testassert.Require(t, len(body) == 0, "rejection produced a partial body: %s", string(body))
}

// TestReservedKeysIdentifiedDespiteMalformedValues pins rule 5's identification guarantee when any layer also carries an invalid JSON value: one presence-only reserved-key pass across all three layers runs before raw-value validation, so malformed values cannot hide present reserved keys.
func TestReservedKeysIdentifiedDespiteMalformedValues(t *testing.T) {
	req := Request{Messages: []Message{userText("hi")}}

	t.Run("valid-reserved-plus-malformed-sibling-layer", func(t *testing.T) {
		rt := testResolved()                                                        // baseline layers are empty; only the two forged entries below differ.
		rt.ProviderExtraBody = Extra{"max_tokens": json.RawMessage(`1`)}            // presence is what must be identified, regardless of sibling values elsewhere.
		runtimeExtras := map[string]json.RawMessage{"junk_key": []byte("not-json")} // malformed non-reserved value in a different layer than the reserved one.

		body, _, err := Encode(rt, req, runtimeExtras) // rejection names the present reserved key even though another layer fails raw-value validation too.
		var reserved *ReservedKeyError                 // typed shape carries exactly the distinct present keys.
		testassert.Require(t, errors.As(err, &reserved) && errors.Is(err, ErrReservedKeys), "want typed reserved-key error despite the malformed sibling value, got %v (body=%s)", err, string(body))
		// one and only this key; the malformed non-reserved entry is not a reservation.
		testassert.Require(t, len(reserved.Keys) == 1 && reserved.Keys[0] == "max_tokens", "keys = %#v want [max_tokens]", reserved.Keys)
	})

	t.Run("malformed-reserved-value-plus-second-layer-key", func(t *testing.T) {
		rt := testResolved()                                           // baseline layers are empty; only the two forged entries below differ.
		rt.ModelExtraBody = Extra{"stream_options": []byte("{broken")} // reserved key whose own value is malformed — identification must not depend on parsing it.
		runtimeExtras := map[string]json.RawMessage{"n": json.RawMessage(`2`)}

		body, _, err := Encode(rt, req, runtimeExtras) // both present keys identified in one pass before any raw-value validation runs.
		var reserved *ReservedKeyError                 // typed shape carries the full distinct set regardless of value validity.
		testassert.Require(t, errors.As(err, &reserved) && errors.Is(err, ErrReservedKeys), "want typed reserved-key error despite the malformed reserved value, got %v (body=%s)", err, string(body))
		got := map[string]int{} // set comparison with a duplicate check; key order is diagnostic formatting only.
		for _, k := range reserved.Keys {
			got[k]++
			testassert.Require(t, got[k] <= 1, "key %q reported more than once: %#v", k, reserved.Keys)
		}
		wantSet := map[string]bool{"stream_options": true, "n": true} // exactly the two present keys; nothing else.
		testassert.Require(t, len(got) == len(wantSet), "keys = %#v want exactly %d distinct present reserved keys", reserved.Keys, len(wantSet))
		for k := range got {
			testassert.Require(t, wantSet[k], "unexpected key %q in error: %#v", k, reserved.Keys)
		}
	})
}

// TestReservedBodyKeysReturnsOwnedCopyInFixedOrder pins the one exported
// ownership surface: the exact ordered set, as a copy the caller owns. The
// encoder's private checks keep their own set, so a mutated copy neither
// changes later returns nor weakens wire rejection.
func TestReservedBodyKeysReturnsOwnedCopyInFixedOrder(t *testing.T) {
	want := []string{"model", "messages", "tools", "tool_choice", "stream", "stream_options", "max_tokens", "max_completion_tokens", "n"}
	got := ReservedBodyKeys()
	testassert.Require(t, len(got) == len(want), "ReservedBodyKeys() = %#v, want %d keys", got, len(want))
	for i := range want {
		testassert.Require(t, got[i] == want[i], "ReservedBodyKeys()[%d] = %q, want %q (exact order)", i, got[i], want[i])
	}

	got[0] = "temperature" // the returned slice is the caller's to modify.
	again := ReservedBodyKeys()
	testassert.Require(t, len(again) == len(want) && again[0] == want[0], "ReservedBodyKeys() after caller mutation = %#v, want a fresh %#v", again, want)

	rt := testResolved() // the key the caller renamed in its copy still rejects at the wire boundary.
	rt.ProviderExtraBody = Extra{"model": json.RawMessage(`"forged"`)}
	body, _, err := Encode(rt, Request{Messages: []Message{userText("hi")}}, nil)
	var reserved *ReservedKeyError
	testassert.Require(t, errors.As(err, &reserved) && errors.Is(err, ErrReservedKeys) && len(body) == 0, "encode after caller mutation = (%s, %v), want the typed rejection with no body", string(body), err)
	testassert.Require(t, len(reserved.Keys) == 1 && reserved.Keys[0] == "model", "keys = %#v, want [model]", reserved.Keys)
}
