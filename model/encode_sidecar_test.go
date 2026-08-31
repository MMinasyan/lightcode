package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSidecarLayersShallowMergeInOrder pins rule 6: provider, then model, then runtime extra-body layers shallow-merge over the base in that order with later winning per key; values pass through byte-exact from whichever layer won them. (Reserved keys can never reach this point — their presence rejects encoding first, pinned by TestReservedKeys*.)
func TestSidecarLayersShallowMergeInOrder(t *testing.T) {
	rt := testResolved()

	providerLayer := Extra{"temperature": json.RawMessage(`1`), "top_p": json.RawMessage(`0.5`)}
	modelLayer := Extra{"temperature": json.RawMessage(`2`)}
	runtimeExtras := map[string]json.RawMessage{
		"temperature": json.RawMessage(`3`), "frequency_penalty": json.RawMessage(`0.25`)}

	body, _, err := Encode(rtWithLayers(rt, providerLayer, modelLayer), Request{Messages: []Message{userText("hi")}}, runtimeExtras)
	if err != nil {
		t.Fatalf("Encode returned unexpected error: %v", err)
	}

	obj := decodeObject(t, body)

	want := map[string]string{
		"temperature": `3`, "top_p": `0.5`, "frequency_penalty": `0.25`}

	for k, v := range want {
		if got, ok := obj[k]; !ok || string(got) != v {
			t.Fatalf("body[%s] = %v (present=%v), want exact value %q", k, got, ok, v)
		}
	}

	body2, _, err := Encode(rtWithLayers(rt, Extra{"temperature": json.RawMessage(`1`)}, Extra{"temperature": json.RawMessage(`2`)}), Request{Messages: []Message{userText("hi")}}, nil)
	if err != nil {
		t.Fatalf("second Encode returned unexpected error: %v", err)
	}

	obj2 := decodeObject(t, body2)
	if string(obj2["temperature"]) != `2` {
		t.Fatalf("model layer failed to beat provider layer: temperature = %s, want 2", obj2["temperature"])
	}

	if _, ok := obj2["top_p"]; ok {
		t.Fatalf("second body unexpectedly carries top_p from a different fixture: %s", string(body2))
	}
}

// TestEncodeCallerImmutability pins the plan's ownership clause for direct encoder invocation: Encode deep-copies resolved input, request, and runtime extras on entry, so post-call mutation of caller-owned storage cannot corrupt a previously returned body (owned JSON bytes) nor leak stale aliases into later fresh encodes that must reflect only their current inputs at entry time.
func TestEncodeCallerImmutability(t *testing.T) {
	rt := testResolved()

	sharedRuntime := json.RawMessage(`"runtime-shared-value-1"`)
	sharedProvider := json.RawMessage(`"provider-shared-value-1"`)
	rt.ProviderExtraBody = Extra{"prov_marker": sharedProvider}
	rt.Headers = map[string]string{"X-Shared": "original-header-value"}
	partExtra := Extra{"part_shared": sharedRuntime}
	reqMsg, err := NewMessage(Message{Role: RoleAssistant, Source: targetRef, Content: []ContentPart{{Kind: PartText, Text: "body", Extra: partExtra}}})
	if err != nil {
		t.Fatalf("NewMessage(immuability fixture): %v", err)
	}

	req := Request{Messages: []Message{reqMsg}}

	body1, _, err := Encode(rt, req, map[string]json.RawMessage{"rt_marker": sharedRuntime})
	if err != nil {
		t.Fatalf("Encode returned unexpected error: %v", err)
	}

	if !strings.Contains(string(body1), `"rt_marker":"runtime-shared-value-1"`) {
		t.Fatalf("first encode lost expected runtime marker bytes (pre-mutation baseline broken): %s", string(body1))
	}

	if !strings.Contains(string(body1), `"prov_marker":"provider-shared-value-1"`) {
		t.Fatalf("first encode lost expected provider marker bytes: %s", string(body1))
	}

	copy(sharedRuntime, []byte(`"CORRUPTED--RUNTIME-XXX"`))
	copy(sharedProvider, []byte(`"CORRUPTED--PROVIDER-XXX"`)) // same exact-length discipline at provider scope too (inner text must match its original 23-char length for the identical reason above).

	rt.Headers["X-Shared"] = "mutated-header-value"
	reqMsg.Content[0].Extra["part_shared"] = json.RawMessage(`"CORRUPTED-PART-XX"`)
	body2, _, err := Encode(rt, req, map[string]json.RawMessage{"rt_marker": sharedRuntime})
	if err != nil {
		t.Fatalf("second Encode returned unexpected error: %v", err)
	}

	if !strings.Contains(string(body2), `"rt_marker":"CORRUPTED--RUNTIME-XXX"`) {
		t.Fatalf("second encode did not reflect mutated runtime input: %s", string(body2))
	}

	if strings.Contains(string(body1), "CORRUPTED") {
		t.Fatalf("previously returned body was corrupted by post-call input mutation: %s", string(body1))
	}

	if !strings.Contains(string(body1), `"part_shared":"runtime-shared-value-1"`) {
		t.Fatalf("previously returned body lost its part-scope shared bytes after mutation: %s", string(body1))
	}

	if !strings.Contains(string(body2), `"part_shared":"CORRUPTED-PART-XX"`) {
		t.Fatalf("second encode did not reflect replaced part-scope extra value: %s", string(body2))
	}
}

// TestOutputTokenFieldsNeverEmitted pins rule 4: neither max_tokens nor max_completion_tokens may appear in any successfully encoded body under every tools/usage trigger combination, checked as exact decoded top-level keys (never substring scans) so unrelated extra names cannot mask a leak.
func TestOutputTokenFieldsNeverEmitted(t *testing.T) {

	withTools := Request{Messages: []Message{userText("go")}, Tools: []ToolDefinition{{Name: "noop", Description: "", Parameters: json.RawMessage(`{}`)}}} // minimal valid tool definition (object parameters per the constructor contract; parameter-shape rows are Commit 1's scope, not this encoder test's).

	plainReq, err := NewRequest(withTools)
	if err != nil {
		t.Fatalf("NewRequest(output-token fixture): %v", err)
	}

	cases := []struct {
		name          string
		streamedUsage bool
		useTools      bool
	}{
		{"no-tools-no-usage", false, false}, {"tools-only", true, false}, {"usage-only-no-tools", false, true}, {"both-triggers-on", true, true}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rowRt := testResolved()
			rowRt.StreamedUsage = tc.streamedUsage

			var rowReq Request
			if tc.useTools {
				rowReq = plainReq
			} else {
				rowReq = Request{Messages: []Message{userText("go")}}
			}

			body, _, err := Encode(rowRt, rowReq, nil)
			if err != nil {
				t.Fatalf("Encode returned unexpected error: %v", err)
			}

			obj := decodeObject(t, body)
			for _, key := range []string{"max_tokens", "max_completion_tokens"} {
				if _, ok := obj[key]; ok {
					t.Fatalf("body carries forbidden output-token key %q with triggers {tools:%v usage:%v}: %s", key, tc.useTools, tc.streamedUsage, string(body))
				}
			}
		})
	}
}

// TestSidecarLayersNeverEmitPrivateKeys pins rule 9 at top-level scope: _lightcode_* private persistence fields never reach the wire from any of the three sidecar layers (provider, model, runtime), and stripping them disturbs no other merge — sibling keys still shallow-merge with later layers winning.
func TestSidecarLayersNeverEmitPrivateKeys(t *testing.T) {
	req := Request{Messages: []Message{userText("hi")}} // valid request so only layer contents vary per subtest.

	for _, tc := range []struct { // provider and model scopes: the private entry rides in that scope's own layer alongside one sibling key each.
		name    string
		private Extra  // this scope's full layer (private field plus its merge-healthy sibling).
		wantKey string // the sibling expected to still merge exactly as usual after stripping.
	}{
		{"provider", Extra{"_lightcode_state": json.RawMessage(`1`), "temperature": json.RawMessage(`0.5`)}, "temperature"},
		{"model", Extra{"_lightcode_cursor": json.RawMessage(`"x"`), "top_p": json.RawMessage(`0.9`)}, "top_p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := testResolved() // valid baseline; only this scope's layer differs per subtest (the other scopes stay empty).
			if tc.name == "provider" {
				rt.ProviderExtraBody = tc.private
			} else {
				rt.ModelExtraBody = tc.private
			}

			body, _, err := Encode(rt, req, nil) // the private key must be dropped on its way in; nothing here may fail for it.
			if err != nil {
				t.Fatalf("Encode returned unexpected error for layer %s: %v", tc.name, err)
			}

			obj := decodeObject(t, body)
			for k := range obj { // no private field of any spelling reaches the wire from this scope.
				if strings.HasPrefix(k, "_lightcode_") {
					t.Fatalf("body carries private key %q from layer %s: %s", k, tc.name, string(body))
				}
			}
			if got := obj[tc.wantKey]; len(got) == 0 || string(got) == "" { // the sibling merge must survive stripping exactly as usual.
				t.Fatalf("sibling key %q lost while stripping private fields (layer %s): %s", tc.wantKey, tc.name, string(body))
			}
		})
	}

	t.Run("runtime", func(t *testing.T) { // the per-call scope enters through Encode's own argument rather than a resolved field.
		rt := testResolved()
		runtimeExtras := map[string]json.RawMessage{
			"_lightcode_trace":  json.RawMessage(`[]`),   // private key in the runtime layer itself...
			"frequency_penalty": json.RawMessage(`0.25`), // ...with a sibling that must still merge through unchanged.
		}

		body, _, err := Encode(rt, req, runtimeExtras) // same expectation as every other scope: dropped on entry, no error for it specifically.
		if err != nil {
			t.Fatalf("Encode returned unexpected error for layer runtime: %v", err)
		}

		obj := decodeObject(t, body)
		for k := range obj { // the private floor applies at top level exactly like in message scopes (rule 9 shared).
			if strings.HasPrefix(k, "_lightcode_") {
				t.Fatalf("body carries private key %q from layer runtime: %s", k, string(body))
			}
		}
		if got := obj["frequency_penalty"]; len(got) == 0 || string(got) != `0.25` {
			t.Fatalf("runtime sibling merge disturbed by private stripping: frequency_penalty = %v (body=%s)", obj["frequency_penalty"], string(body))
		}
	})

	t.Run("all-layers-at-once", func(t *testing.T) { // one private key per scope in a single encode — the full top-level surface of rule 9.
		rt := testResolved()
		rt.ProviderExtraBody = Extra{"_lightcode_a": json.RawMessage(`1`)}
		rt.ModelExtraBody = Extra{"_lightcode_b": json.RawMessage(`2`)}
		runtimeExtras := map[string]json.RawMessage{"_lightcode_c": json.RawMessage(`3`)}

		body, _, err := Encode(rt, req, runtimeExtras) // all three must vanish; a valid encode with nothing else to say about them.
		if err != nil {
			t.Fatalf("Encode returned unexpected error for the combined layer set: %v", err)
		}

		obj := decodeObject(t, body)
		for k := range obj { // absence across every scope is what this row exists to pin in one shot.
			if strings.HasPrefix(k, "_lightcode_") {
				t.Fatalf("body carries private key %q: %s", k, string(body))
			}
		}
	})
}

// TestCloneResolvedInputClonesEvenWhenEmpty pins the resolved-input ownership clause for its missing case: every non-nil resolved map and slice is cloned even when empty, so retained collections can never alias caller storage (nil inputs stay nil). Post-clone mutation of the caller's own maps/slices must be invisible in the clone.
func TestCloneResolvedInputClonesEvenWhenEmpty(t *testing.T) {
	headers := map[string]string{}    // non-nil but empty: exactly the shape the len-guarded clones used to skip and pass through aliased.
	dropSet := map[string]bool{}      // same for the drop key set...
	families := map[ModelRef]string{} // ...and the source-family table (MustPreserve is covered below as a slice).

	providerLayer := Extra{} // non-nil empty extras: retained layers must not observe later caller insertions either.
	modelLayer := Extra{}    // both scopes get their own storage so one aliasing bug cannot mask the other's clone state in this row set.

	in := ResolvedTransport{
		Model:             ModelRef{Provider: "openai", Model: "gpt-test"},
		Headers:           headers,
		Drop:              dropSet,
		SourceFamilies:    families,
		MustPreserve:      []string{}, // non-nil empty ordered list (warning order depends on it being cloned as a real copy).
		ProviderExtraBody: providerLayer,
		ModelExtraBody:    modelLayer,
	}

	out := cloneResolvedInput(in) // the retained input must own an independent collection for every non-nil field above.

	headers["X-Late"] = "late"                              // post-clone mutation of caller storage... (must not appear in any cloned map below if cloning really happened).
	dropSet["k-late"] = true                                // ...same discipline on the drop set, whose keys drive replay filtering when retained by a transport.
	families[ModelRef{Provider: "p", Model: "m"}] = "f"     // ...and the family table consulted only for cross-model replay decisions.
	in.MustPreserve = append(in.MustPreserve, "late-field") // slice growth on the caller's own backing storage must not extend the clone (a shared header would let it).
	providerLayer["_lightcode_late"] = json.RawMessage(`1`) // extra-scope insertions... (must never reach retained layers through a shared map reference either).
	modelLayer["k-late"] = json.RawMessage(`2`)             // ...independent per scope, so each field's clone state is judged on its own row rather than masked by siblings.

	if _, ok := out.Headers["X-Late"]; ok {
		t.Fatalf("retained Headers alias caller storage: post-clone insertion %q visible in the clone", "X-Late") // an aliased map would show exactly this key here — the precise leak shape this row exists to catch before it reaches a transport's retained state.
	} else if len(out.Headers) != 0 {
		t.Fatalf("cloned Headers not empty: %#v (input was non-nil but had no entries at clone time)", out.Headers) // only the pre-clone contents belong in an independent copy of this input shape.
	}

	if _, ok := out.Drop["k-late"]; ok || len(out.Drop) != 0 {
		t.Fatalf("retained Drop aliases caller storage or gained a late key: %#v", out.Drop) // one check covers both the leak (key present) and any phantom pre-clone content for this fixture.
	}

	if _, ok := out.SourceFamilies[ModelRef{Provider: "p", Model: "m"}]; ok || len(out.SourceFamilies) != 0 {
		t.Fatalf("retained SourceFamilies alias caller storage or gained a late entry: %#v", out.SourceFamilies) // same combined assertion as Drop for the family table scope.
	}

	if len(out.MustPreserve) != 0 { // the caller's slice really did grow to one entry by now, so any length visible in the clone means a shared header.
		t.Fatalf("retained MustPreserve aliases caller storage: post-clone append visible (len=%d)", len(out.MustPreserve)) // no independent copy can report an appended element — its own head was copied while still empty.
	}

	for _, scope := range []struct {
		name  string
		layer Extra
	}{{"ProviderExtraBody", out.ProviderExtraBody}, {"ModelExtraBody", out.ModelExtraBody}} { // retained extra scopes must be empty (or nil) and never see the late caller insertions above in either direction of aliasing.
		if len(scope.layer) != 0 {
			t.Fatalf("retained %s observed post-clone caller insertion or pre-ghost content: %#v", scope.name, scope.layer) // any entry here means this field's clone state shares storage with the fixture maps above — exactly what the clone-on-entry ownership rule forbids.
		}
	}
}

// rtWithLayers returns an owned copy of in with its two persistent sidecar layers replaced by the given ones; caller's originals stay untouched.
func rtWithLayers(in ResolvedTransport, providerLayer, modelLayer Extra) ResolvedTransport {
	out := in
	out.ProviderExtraBody = providerLayer
	out.ModelExtraBody = modelLayer
	return out
}
