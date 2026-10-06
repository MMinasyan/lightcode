package model

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// richEquivalenceRequest builds the fixture request through the public constructors, failing the test instead of panicking.
func richEquivalenceRequest(t *testing.T, assistantSource ModelRef) Request {
	t.Helper()
	assistant, err := NewMessage(Message{
		Role:    RoleAssistant,
		Source:  assistantSource,
		Content: []ContentPart{{Kind: PartText, Text: "working on it"}},
		ToolCalls: []ToolCall{func() ToolCall {
			call, err := NewToolCall(ToolCall{ID: "call_eq_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`), Extra: Extra{"call_meta": json.RawMessage(`1`)}})
			if err != nil {
				t.Fatalf("NewToolCall fixture: %v", err)
			}
			return call
		}()},
		Extra: Extra{"provider_meta": json.RawMessage(`{"v":2}`)},
	})
	if err != nil {
		t.Fatalf("NewMessage assistant fixture: %v", err)
	}
	return Request{
		Messages: []Message{
			mustMsg(t, Message{Role: RoleSystem, Content: []ContentPart{{Kind: PartText, Text: "system setup"}}}),
			userText("hi"),
			assistant,
			mustMsg(t, Message{Role: RoleTool, ToolCallID: "call_eq_1", Content: []ContentPart{{Kind: PartText, Text: "done"}}}),
		},
		Tools: []ToolDefinition{{Name: "read_file", Description: "reads", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
	}
}

// TestEncodeAndStreamProduceIdenticalBodyAndWarnings pins M1a equality over the same valid input: for every axis (keyed/keyless credentials, system/user/developer wire roles, same-model and cross-model replay retention, all three sidecar layers populated), one direct Encode call and one Stream call over a loopback endpoint must produce byte-identical bodies and deep-equal ordered warnings. Divergence would mean the transport path and the encoder boundary disagree on encoding or ownership.
func TestEncodeAndStreamProduceIdenticalBodyAndWarnings(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	runtimeExtras := map[string]json.RawMessage{"runtime_flag": json.RawMessage(`true`), "seed": json.RawMessage(`7`)}

	cases := []struct {
		name          string
		rt            ResolvedTransport
		assistantSrc  ModelRef
		runtimeExtras map[string]json.RawMessage
		wantWarnings  int
	}{
		{
			name:         "keylessDefaultSystemRoleSameModelReplay",
			rt:           testResolved(),
			assistantSrc: targetRef, // same identity: extras kept, must-preserve warning fires.
			wantWarnings: 1,
		},
		{
			name: "keyedUserRole",
			rt: func() ResolvedTransport {
				rt := testResolved()
				rt.APIKey = "secret-eq-key"
				rt.WireSystemRole = "user"
				return rt
			}(),
			assistantSrc: targetRef,
			wantWarnings: 1,
		},
		{
			name: "developerRole",
			rt: func() ResolvedTransport {
				rt := testResolved()
				rt.WireSystemRole = "developer"
				return rt
			}(),
			assistantSrc: targetRef,
			wantWarnings: 1,
		},
		{
			name: "crossModelSameFamilyKeepsExtras",
			rt: func() ResolvedTransport {
				rt := testResolved()
				rt.SourceFamilies = map[ModelRef]string{otherSameProv: "openai-compatible"}
				rt.ProtocolFamily = "openai-compatible"
				return rt
			}(),
			assistantSrc: otherSameProv, // different model, equal non-empty family pair: extras kept.
			wantWarnings: 1,
		},
		{
			name:         "crossProviderStripsExtras",
			rt:           testResolved(),
			assistantSrc: foreignModel, // different provider: extras stripped, no warning.
			wantWarnings: 0,
		},
		{
			name: "allThreeSidecarLayersPopulated",
			rt: func() ResolvedTransport {
				rt := testResolved()
				rt.ProviderExtraBody = Extra{"temperature": json.RawMessage(`0.1`), "top_p": json.RawMessage(`0.9`)}
				rt.ModelExtraBody = Extra{"temperature": json.RawMessage(`0.5`)}
				return rt
			}(),
			assistantSrc:  targetRef,
			runtimeExtras: runtimeExtras,
			wantWarnings:  1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := richEquivalenceRequest(t, tc.assistantSrc)

			rowRt := tc.rt
			rowRt.BaseURL = server.URL // endpoint pinned pre-construction per suite convention.
			rowRt.MustPreserve = []string{"reasoning_details"}

			wantBody, wantWarnings, err := Encode(rowRt, req, tc.runtimeExtras)
			if err != nil {
				t.Fatalf("Encode returned error for valid input: %v", err)
			}
			if len(wantWarnings) != tc.wantWarnings {
				t.Fatalf("fixture premise: Encode warnings = %d, want %d (%#v)", len(wantWarnings), tc.wantWarnings, wantWarnings)
			}

			tr := mustTransport(t, rowRt)
			stream, gotWarnings, err := tr.Stream(context.Background(), req, tc.runtimeExtras)
			if err != nil {
				t.Fatalf("Stream returned error for valid input: %v", err)
			}
			defer stream.Close()

			if !bytes.Equal(capturedBody, wantBody) {
				t.Fatalf("posted body differs from Encode output:\ngot  %s\nwant %s", capturedBody, wantBody)
			}
			if !reflect.DeepEqual(gotWarnings, wantWarnings) {
				t.Fatalf("warnings differ: Stream %#v, Encode %#v", gotWarnings, wantWarnings)
			}
		})
	}
}

// TestCallerMutationAfterConstructionKeepsNextStream pins M1b independence: after construction, mutating every caller-owned value (original headers, both resolved extra layers and their raw bytes, the drop set, the must-preserve order and its original backing storage, the source-family table, and a previously returned Encode body) must leave the next Stream unchanged — same posted body, same ordered warnings as the pre-mutation baseline. The stimuli target the actual caller and returned values, never HTTP observation copies. Two missing must-preserve fields make the ordered warning pair observable, and the must-preserve mutation writes through the caller's original backing storage in place (element write plus order reversal, no append — a reallocating append would not exercise the backing-isolation obligation).
func TestCallerMutationAfterConstructionKeepsNextStream(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	rt := testResolved()
	rt.BaseURL = server.URL
	rt.Headers = map[string]string{"X-Probe": "original"}
	rt.ProviderExtraBody = Extra{"temperature": json.RawMessage(`0.2`), "top_p": json.RawMessage(`0.7`)}
	rt.ModelExtraBody = Extra{"frequency_penalty": json.RawMessage(`0.3`)}
	rt.MustPreserve = []string{"reasoning_details", "tool_call_id"} // two missing fields: one ordered warning each, so the order itself is observable across the mutation below.
	rt.Drop = map[string]bool{"internal_note": true}
	rt.SourceFamilies = map[ModelRef]string{otherSameProv: "openai-compatible"}
	rt.ProtocolFamily = "openai-compatible"

	req := richEquivalenceRequest(t, otherSameProv) // cross-model same family: kept extras, one warning.
	extras := map[string]json.RawMessage{"runtime_flag": json.RawMessage(`true`)}

	// Pre-mutation baselines: one direct Encode body and one Stream exchange.
	wantBody, wantWarnings, err := Encode(rt, req, extras)
	if err != nil {
		t.Fatalf("Encode baseline failed: %v", err)
	}
	if len(wantWarnings) != 2 || wantWarnings[0].Field != "reasoning_details" || wantWarnings[1].Field != "tool_call_id" ||
		wantWarnings[0].MessageIndex != 2 || wantWarnings[1].MessageIndex != 2 { // premise: the immutable expectation really is the ordered pair, so the comparison below cannot pass vacuously (the assistant is the third message, index 2).
		t.Fatalf("fixture premise: Encode ordered warnings = %#v; want reasoning_details then tool_call_id at message index 2", wantWarnings)
	}
	baseline := append([]byte(nil), wantBody...) // independent copy as the comparison reference; wantBody itself is mutated below as a returned-value stimulus.

	tr := mustTransport(t, rt)
	stream, gotWarnings, err := tr.Stream(context.Background(), req, extras)
	if err != nil {
		t.Fatalf("baseline Stream failed: %v", err)
	} else if err := stream.Close(); err != nil {
		t.Fatalf("baseline stream Close failed: %v", err)
	} else if !bytes.Equal(capturedBody, baseline) || !reflect.DeepEqual(gotWarnings, wantWarnings) {
		t.Fatalf("baseline Stream drifted from Encode: body equal=%v warnings %#v", bytes.Equal(capturedBody, baseline), gotWarnings)
	}

	// Mutation stimuli on actual caller values after construction:
	rt.Headers["X-Probe"] = "mutated"                                               // header entry rewrite...
	rt.Headers["X-Late"] = "late"                                                   // ...and insertion.
	rt.ProviderExtraBody["temperature"] = json.RawMessage(`9`)                      // provider layer value replacement.
	rt.ProviderExtraBody["top_p"] = json.RawMessage(`CORRUPTED--BYTES`)             // malformed bytes in the caller's layer must not matter at all — the retained layer was already validated and cloned at construction.
	delete(rt.ModelExtraBody, "frequency_penalty")                                  // key deletion from the caller's own layer.
	rt.MustPreserve[0] = "mutated-in-place"                                         // element write through the caller's original backing storage...
	rt.MustPreserve[0], rt.MustPreserve[1] = rt.MustPreserve[1], rt.MustPreserve[0] // ...and in-place order reversal: no append, no reallocation — exactly the aliasing shape a shared retained slice would leak through.
	rt.Drop["late-key"] = true                                                      // drop-set insertion.
	rt.SourceFamilies[otherSameProv] = "changed-family"                             // family table rewrite.

	// And on an actual returned value: rewrite the previously returned Encode body in place.
	copy(wantBody, []byte(`XXXX`))

	stream, gotWarnings, err = tr.Stream(context.Background(), req, extras)
	if err != nil {
		t.Fatalf("post-mutation Stream failed: %v", err)
	} else if err := stream.Close(); err != nil {
		t.Fatalf("post-mutation stream Close failed: %v", err)
	}

	if !bytes.Equal(capturedBody, baseline) {
		t.Fatalf("post-mutation Stream body changed:\ngot  %s\nwant %s", capturedBody, baseline)
	}
	if !reflect.DeepEqual(gotWarnings, wantWarnings) {
		t.Fatalf("post-mutation Stream warnings changed: got %#v, want %#v", gotWarnings, wantWarnings)
	}
}
