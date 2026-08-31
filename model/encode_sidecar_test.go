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

// rtWithLayers returns an owned copy of in with its two persistent sidecar layers replaced by the given ones; caller's originals stay untouched.
func rtWithLayers(in ResolvedTransport, providerLayer, modelLayer Extra) ResolvedTransport {
	out := in
	out.ProviderExtraBody = providerLayer
	out.ModelExtraBody = modelLayer
	return out
}
