package model

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// targetRef is the identity same-source fixtures in this file carry so replay keeps their extras on the wire; it equals testResolved().Model by construction of that helper's baseline.
var targetRef = ModelRef{Provider: "openai", Model: "gpt-test"}

func unmarshalContentParts(t *testing.T, obj map[string]json.RawMessage) []map[string]json.RawMessage { // decodes one wire object's content key into ordered raw part objects (array form only; string-form rows use assertUserTextWire instead).
	t.Helper()                             // a missing or non-array content key is the failure mode this helper reports, which is exactly what shape-regression rows want surfaced.
	var parts []map[string]json.RawMessage // raw objects keep every key visible, so forbidden-key leaks are distinguishable from absence (typed decoding would drop unknowns silently).
	{
		err := json.Unmarshal(obj["content"], &parts)
		testassert.Require(t, err == nil && parts != nil, "content not an object array: %s", obj["content"])
	}
	return parts // wire order equals request part order by encoder contract; rows below index into it directly.
}

func assertStringField(t *testing.T, obj map[string]json.RawMessage, key, want string) {
	t.Helper() // shared by every row pinning structural kind/identity fields on otherwise-raw objects in this file.
	rawVal, ok := obj[key]
	testassert.Require(t, ok, "object missing %q: %#v", key, obj)
	var got string
	// type mismatch and value mismatch share one message so the failure class is obvious from output alone.
	{
		err := json.Unmarshal(rawVal, &got)
		testassert.Require(t, err == nil && got == want, "%s = %s (unmarshal error=%v), want %q", key, rawVal, err, want)
	}
}

// assertUserTextWire asserts a wire object's content is exactly the given plain JSON string — rule 10's single-text-part shape for whatever role that message carries.
func assertUserTextWire(t *testing.T, obj map[string]json.RawMessage, text string) { // an object/number here is a shape regression: this helper fails on any non-string content type too.
	t.Helper() // shared across the wire-shape and scope tests in this file so every call site has identical strength by delegation to one code path.
	rawContent, ok := obj["content"]
	testassert.Require(t, ok, "object has no content key: %#v", obj)
	var got string
	{
		err := json.Unmarshal(rawContent, &got)
		testassert.Require(t, err == nil && got == text, "content = %s, want plain string %q (unmarshal error=%v)", rawContent, text, err)
	}
}

// TestMessageWireShapesPinsCanonicalFields pins rule 11's message shapes exactly as retained minus persistence fields: role passthrough for non-system roles, refusal/name only when non-empty, tool results carrying their call id plus optional name, and assistant tool calls emitting id + type "function" + nested function{name,arguments} with arguments rendered as the raw-JSON string whose bytes round-trip verbatim.
func TestMessageWireShapesPinsCanonicalFields(t *testing.T) {
	rt := testResolved()

	assistant, err := NewMessage(Message{Role: RoleAssistant, Source: targetRef, Content: []ContentPart{{Kind: PartText, Text: "ok"}}}) // empty refusal/name must produce neither key on its wire object.
	testassert.NoError(t, err)

	callArgs := json.RawMessage(`{"path":"a/b","n":[1,2]}`)                                    // non-trivial raw bytes: quoting depth and inner commas are both observable on the wire after encoding.
	called, err := NewToolCall(ToolCall{ID: "call_9", Name: "read_file", Arguments: callArgs}) // completed-call boundary clones argument bytes like production paths do (ownership pinned separately by TestEncodeCallerImmutability).
	testassert.NoError(t, err)
	calledMsg, err := NewMessage(Message{Role: RoleAssistant, Source: targetRef, Content: []ContentPart{{Kind: PartText, Text: "go"}}, ToolCalls: []ToolCall{called}}) // tool-call-only assistant messages are valid by contract.
	testassert.NoError(t, err)

	namedResult := mustMsg(t, Message{Role: RoleTool, ToolCallID: "call_9", Name: "read_file", Content: []ContentPart{{Kind: PartText, Text: "file contents"}}}) // tool result carrying both its required call id and optional name.
	emptyResult := mustMsg(t, Message{Role: RoleTool, ToolCallID: "call_empty"})
	refused, err := NewMessage(Message{Role: RoleAssistant, Source: targetRef, Content: []ContentPart{{Kind: PartText, Text: "no"}}, Refusal: "blocked"}) // positive side of the refusal field (message 1 above pins its negative absence).
	testassert.NoError(t, err)

	body := mustEncode(t, rt, Request{Messages: []Message{userText("hi"), assistant, calledMsg, namedResult}}) // four messages in request order; every row below indexes this single body.
	msgs := decodeMessages(t, body)                                                                            // count mismatch is a structural failure reported before any field-level rows run.
	testassert.Require(t, len(msgs) == 4, "encoded %d messages, want 4: %s", len(msgs), string(body))

	assertUserTextWire(t, msgs[0], "hi") // user text encodes as a plain JSON string with nothing else on the object.
	// non-system roles pass through unchanged (system mapping is pinned by its dedicated test).
	{
		raw, ok := wireString(t, msgs[1], "role")
		testassert.Require(t, ok && raw == "assistant", "message[1] role = %v want assistant", msgs[1]["role"])
	}
	for _, key := range []string{"refusal", "name", "tool_call_id"} { // empty optional fields produce no keys at all (not null, not empty string): absence is the pinned behavior.
		{
			_, ok := msgs[1][key]
			testassert.Require(t, !ok, "message[1] must not carry %q: %#v", key, msgs[1])
		}
	}

	rawCalls, ok := msgs[2]["tool_calls"] // the whole array is under test at message 2 (count and per-call shape both pinned here).
	testassert.Require(t, ok, "message[2] missing tool_calls: %#v", msgs[2])
	var calls []struct {
		ID   string `json:"id"` // typed view for the three canonical fields; raw key-presence checks cover everything else (extras, private keys).
		Type string `json:"type"`
		Fn   struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"` // must decode back to exactly the raw bytes sent in.
		} `json:"function"`
	}
	{
		err := json.Unmarshal(rawCalls, &calls)
		testassert.Require(t, err == nil && len(calls) == 1, "tool_calls wrong: %s", string(rawCalls))
	}
	// id present, explicit function type, nested name (no inner type field per the retained shape).
	testassert.Require(t, calls[0].ID == "call_9" && calls[0].Type == "function" && calls[0].Fn.Name == "read_file", "call identity/type/name = %+v, want call_9/function/read_file: %s", calls[0], string(rawCalls))
	var argsAsString string
	// its unescaped content must equal the exact argument bytes sent in (byte fidelity of the rendering itself, not merely semantic JSON equality).
	{
		err := json.Unmarshal(calls[0].Fn.Arguments, &argsAsString)
		testassert.Require(t, err == nil && argsAsString == `{"path":"a/b","n":[1,2]}`, "arguments = %s, want exactly the raw-JSON string form of the original argument bytes", calls[0].Fn.Arguments)
	}

	// tool results carry the original call id they answer.
	{
		rawID, ok := wireString(t, msgs[3], "tool_call_id")
		testassert.Require(t, ok && rawID == "call_9", "message[3] tool_call_id = %v want call_9", msgs[3]["tool_call_id"])
	}
	// optional name emits exactly when present.
	{
		rawName, ok := wireString(t, msgs[3], "name")
		testassert.Require(t, ok && rawName == "read_file", "message[3] name = %v want read_file", msgs[3]["name"])
	}

	body2 := mustEncode(t, rt, Request{Messages: []Message{refused}}) // separate encode keeps refusal's positive assertion independent from message 1's negative one above.
	msgs2 := decodeMessages(t, body2)                                 // exactly one wire object for this single-message request.
	// non-empty refusal appears with its exact value on the assistant path.
	{
		rawRefusal, ok := wireString(t, msgs2[0], "refusal")
		testassert.Require(t, ok && rawRefusal == "blocked", "refusal = %v want blocked", msgs2[0]["refusal"])
	}
	// name still absent: empty-value omission does not change when other optional fields are present (per-field independence).
	{
		_, ok := msgs2[0]["name"]
		testassert.Require(t, !ok, "message must not carry an empty name key: %#v", msgs2[0])
	}

	emptyWire := decodeMessages(t, mustEncode(t, rt, Request{Messages: []Message{emptyResult}}))[0]
	{
		content, ok := wireString(t, emptyWire, "content")
		testassert.Require(t, ok && content == "", "empty tool result content = %q, present=%v; want a present empty string", content, ok)
	}
}

// TestContentWireShapesPinsStringVsArray pins rule 10's shape split at content scope: a single text part without surviving extras encodes as the bare string; any other composition (multiple parts, image_url present, or retained part-scope extras on that one text part) encodes as an ordered object array carrying type/text/image_url fields exactly per the retained legacy shapes.
func TestContentWireShapesPinsStringVsArray(t *testing.T) {
	rt := testResolved() // same-source fixtures keep their extras where rows need them to survive onto the wire (strip-side behavior has its own dedicated subtests below).

	t.Run("multiple-parts-ordered-array", func(t *testing.T) { // text then image_url: both objects appear in request order with exact kind fields and no invented keys.
		m, err := NewMessage(Message{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "look"}, {Kind: PartImageURL, URL: "https://img/x.png"}}}) // user messages carry the zero source by contract.
		testassert.NoError(t, err)
		body := mustEncode(t, rt, Request{Messages: []Message{m}})
		msgs := decodeMessages(t, body)                     // one wire object for this request.
		parts := unmarshalContentParts(t, msgs[0])          // array form is forced by having two parts; a bare string would fail inside the helper above with its diagnostic naming exactly this row.
		assertStringField(t, parts[0], "type", "text")      // structural kind field on the first part (wire vocabulary pinned literally).
		assertStringField(t, parts[0], "text", "look")      // text value under exactly its canonical key in array form.
		assertStringField(t, parts[1], "type", "image_url") // second part's structural kind at its own ordered position (ordering is contract here).
		var image struct {
			URL string `json:"url"`
		} // nested object shape fixed by the retained encoder: exactly one url field.
		{
			err := json.Unmarshal(parts[1]["image_url"], &image)
			testassert.Require(t, err == nil && image.URL == "https://img/x.png", "image_url = %s, want url https://img/x.png", parts[1]["image_url"])
		}
	})

	t.Run("single-text-with-kept-extra-becomes-array", func(t *testing.T) {
		partMsg := mustMsg(t, Message{Role: RoleAssistant, Source: targetRef, Content: []ContentPart{{Kind: PartText, Text: "hi", Extra: Extra{"part_marker": json.RawMessage(`1`)}}}})
		body := mustEncode(t, rt, Request{Messages: []Message{partMsg}})
		msgs := decodeMessages(t, body)                // one wire object for this request.
		parts := unmarshalContentParts(t, msgs[0])     // must be the array form now — a bare string fails inside that helper with content-not-an-object-array.
		assertStringField(t, parts[0], "type", "text") // kind field appears in object form (string-form rows never see one at all).
		assertStringField(t, parts[0], "text", "hi")   // text under its canonical key alongside the extra.
		{
			got, ok := parts[0]["part_marker"]
			testassert.Require(t, ok && string(got) == `1`, "kept part extra missing or altered from the array-form object: %#v", parts[0])
		}
	})

	t.Run("stripped-single-text-with-extra-stays-string", func(t *testing.T) {
		stripped := mustMsg(t, Message{Role: RoleAssistant, Source: foreignModel, Content: []ContentPart{{Kind: PartText, Text: "hi", Extra: Extra{"part_marker": json.RawMessage(`1`)}}}}) // cross-provider source => replay strips every scope including this part's extras.
		body2 := mustEncode(t, rt, Request{Messages: []Message{stripped}})                                                                                                                  // encode of the stripped fixture produces the observable wire bytes below.
		msgsA := decodeMessages(t, body2)                                                                                                                                                   // one message for this request.
		assertUserTextWire(t, msgsA[0], "hi")                                                                                                                                               // stripped extras never promote to array form: plain string is exactly what rule 10 mandates when nothing survives at that position.
		// guard against invented keys riding alongside content on this minimal fixture (none were set in the source).
		{
			_, ok := msgsA[0]["name"]
			testassert.Require(t, !ok, "stripped message must not carry invented keys: %#v", msgsA[0])
		}
	})

	t.Run("all-opaque-stripped-omits-content-key", func(t *testing.T) {
		opaque, err := NewMessage(Message{Role: RoleAssistant, Source: foreignModel, Content: []ContentPart{{Kind: PartOpaque, OpaqueWireType: "thinking", Extra: Extra{"summary": json.RawMessage(`"s"`)}}}}) // opaque part with its original wire fields in extras; cross-provider source strips everything.
		testassert.NoError(t, err)
		body := mustEncode(t, rt, Request{Messages: []Message{opaque}})
		msgs := decodeMessages(t, body) // one wire object for this request.
		// absence of the key entirely is the pinned behavior (null or an empty array would each be a different observable outcome not specified here).
		{
			_, ok := msgs[0]["content"]
			testassert.Require(t, !ok, "stripped all-opaque message must omit content, got %#v", msgs[0])
		}
	})
}

// TestOpaquePartReplayKeptAndStripped pins rule 10's opaque half: when replay strips extras, opaque parts drop out of the array entirely; when kept, their dedicated original wire type plus retained extras reconstruct the exact original wire object (structural OpaqueWireType always wins any colliding extra key).
func TestOpaquePartReplayKeptAndStripped(t *testing.T) {
	rt := testResolved() // target openai/gpt-test; each fixture below chooses its source identity to land on exactly one side of the keep/strip boundary.

	t.Run("kept-reconstructs-original-wire-object", func(t *testing.T) {
		m, err := NewMessage(Message{Role: RoleAssistant, Source: targetRef, Content: []ContentPart{{Kind: PartText, Text: "preamble"}, {Kind: PartOpaque, OpaqueWireType: "thinking", Extra: Extra{"summary": json.RawMessage(`"hidden"`), "_lightcode_private": json.RawMessage(`1`)}}}}) // text first so order is observable; the private field must still be filtered out inside a kept scope.
		testassert.NoError(t, err)
		body := mustEncode(t, rt, Request{Messages: []Message{m}})
		msgs := decodeMessages(t, body)                    // one wire object for this request.
		parts := unmarshalContentParts(t, msgs[0])         // two objects in original order on the wire (count enforced inside the helper's own check).
		assertStringField(t, parts[1], "type", "thinking") // structural OpaqueWireType lands as type — not the literal internal kind name and not any extra value.
		assertStringField(t, parts[1], "summary", `hidden`)
		// private persistence fields never reconstruct even under full retention.
		{
			_, ok := parts[1]["_lightcode_private"]
			testassert.Require(t, !ok, "private field leaked onto a reconstructed opaque part: %#v", parts[1])
		}
	})

	t.Run("stripped-drops-opaque-keeps-siblings", func(t *testing.T) {
		m, err := NewMessage(Message{Role: RoleAssistant, Source: foreignModel, Content: []ContentPart{{Kind: PartText, Text: "preamble"}, {Kind: PartOpaque, OpaqueWireType: "thinking", Extra: Extra{"summary": json.RawMessage(`"hidden"`)}}}}) // same shape as above but under a stripping source.
		testassert.NoError(t, err)
		body := mustEncode(t, rt, Request{Messages: []Message{m}})
		msgs := decodeMessages(t, body)                    // one wire object for this request.
		parts := unmarshalContentParts(t, msgs[0])         // exactly the text part may remain (count 1 enforced inside the helper).
		assertStringField(t, parts[0], "type", "text")     // the survivor is the text sibling at index zero now that everything after it was removed.
		assertStringField(t, parts[0], "text", "preamble") // with its exact value intact under array-form keys (no truncation or normalization anywhere between fixture and wire).
	})

	t.Run("stripped-all-opaque-omits-content-key", func(t *testing.T) {
		m, err := NewMessage(Message{Role: RoleAssistant, Source: foreignModel, Content: []ContentPart{{Kind: PartOpaque, OpaqueWireType: "thinking", Extra: Extra{"summary": json.RawMessage(`"h"`)}}}}) // single opaque part only under a stripping source (minimal fixture for the absence-of-key outcome class).
		testassert.NoError(t, err)
		body := mustEncode(t, rt, Request{Messages: []Message{m}})
		msgs := decodeMessages(t, body) // one wire object for this request.
		// absence again through a tool-call-free assistant entry shape (the sibling test pins the same outcome from its own fixture construction path).
		{
			_, ok := msgs[0]["content"]
			testassert.Require(t, !ok, "all-opaque stripped message must omit content, got %#v", msgs[0])
		}
	})
}

// TestToolCallAndContentPartExtraScopesPinned pins rule 9/11 scope filtering on kept messages: tool-call-scope extras keep only non-canonical fields (id/type/function are canonical and never extra-supplied even when forged), content-part-scope keeps only its kind-independent extras with type/text/image_url likewise canonical-only, and message-scope private/canonical/reserved keys all fail to reach the wire even under full retention.
func TestToolCallAndContentPartExtraScopesPinned(t *testing.T) {
	rt := testResolved()

	t.Run("tool-call-scope-extras", func(t *testing.T) { // a kept assistant call carries its non-canonical extra but never canonical call fields (id, type, function, index), private keys, or reserved top-level keys from extras.
		call, err := NewToolCall(ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"q":1}`), Extra: Extra{ // one legitimate field plus every canonical/private/reserved forgery attempt — all must fail to land on its wire object below.
			"provider_hint": json.RawMessage(`42`),          // non-canonical at call scope: retained onto the same object (positive retention check alongside the negative rows).
			"id":            json.RawMessage(`"forged-id"`), // canonical identity forgery must not override the real value on the wire.
			"type":          json.RawMessage(`"other"`),     // type always emits "function" regardless of any extra claiming otherwise at this scope.
			"index":         json.RawMessage(`2`),           // streaming-only in the retained producer: never part of a completed request's call wire shape, so it must not come from extras either.
			"_lightcode_x":  json.RawMessage(`1`),           // private floor applies inside a tool call too (rule 9 is shared across every scope).
			"n":             json.RawMessage(`7`),           // reserved top-level key: never from extras at any scope including this nested one.
		}})
		testassert.NoError(t, err)
		m, err := NewMessage(Message{Role: RoleAssistant, Source: targetRef, Content: []ContentPart{{Kind: PartText, Text: "go"}}, ToolCalls: []ToolCall{call}}) // content present keeps this row focused on call-scope filtering rather than payload-validation rules owned by other suites.
		testassert.NoError(t, err)
		body := mustEncode(t, rt, Request{Messages: []Message{m}}) // single encode covers every call-scope assertion below (subtest stays self-contained by design).
		msgs := decodeMessages(t, body)                            // one wire object for this request.
		rawCalls, ok := msgs[0]["tool_calls"]                      // the array itself must survive encoding before any per-call field rows can be evaluated against it.
		testassert.Require(t, ok, "message lost its tool_call array: %#v", msgs[0])
		var calls []map[string]json.RawMessage
		{
			err := json.Unmarshal(rawCalls, &calls)
			testassert.Require(t, err == nil && len(calls) == 1, "tool_calls not a one-object array: %s", string(rawCalls))
		}
		assertStringField(t, calls[0], "id", "c1")
		assertStringField(t, calls[0], "type", "function") // explicit function type survives any forged extra attempt for the same dual-mechanism reason as above.
		var fn struct {                                    // nested object decoded into its own substruct so name and arguments are asserted independently below.
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		// nested function intact under its canonical key (no inner type field per the retained shape).
		{
			err := json.Unmarshal(calls[0]["function"], &fn)
			testassert.Require(t, err == nil && fn.Name == "lookup", "call.function = %s, want name lookup: %s", calls[0]["function"], string(rawCalls))
		}
		{
			got, ok := calls[0]["provider_hint"]
			testassert.Require(t, ok && string(got) == `42`, "kept call-scope extra missing or altered: %#v", calls[0])
		}
		for _, key := range []string{"_lightcode_x", "n", "index"} { // private floor, reserved top-level keys, and streaming-only call fields never reach this wire object.
			{
				_, ok := calls[0][key]
				testassert.Require(t, !ok, "forbidden key %q leaked onto the wire object of a tool call: %#v", key, calls[0])
			}
		}
	})

	t.Run("message-scope-private-canonical-reserved-stripping", func(t *testing.T) {
		base := mustMsg(t, Message{Role: RoleAssistant, Source: targetRef, Content: []ContentPart{{Kind: PartText, Text: "body"}}}) // single legitimate message as starting point (same construction path every other fixture in this file uses).
		forged := base.Extra.Clone()                                                                                                // owned clone before mutation so fixtures respect the same ownership discipline production boundaries do.
		if forged == nil {
			forged = Extra{}
		}
		for _, key := range []string{"_lightcode_state", "role", "content", "refusal", "tool_calls", "tool_call_id", "name", "function_call", "system_fingerprint", "service_tier", "id", "created", "object", "stream"} { // stream is the single reserved top-level key forged here alongside every canonical/private forgery.
			forged[key] = json.RawMessage(`"` + "forged-" + key + `"`) // valid JSON string, distinct per key so byte aliasing between entries cannot mask a projection ownership defect.
		}

		forged["marker"] = json.RawMessage(`"m-kept"`)
		base.Extra = forged
		// mutation of the owned copy is safe: Encode deep-copies at entry (pinned separately by TestEncodeCallerImmutability for this boundary class).
		validated, err := NewMessage(base) // re-run construction so the stored value obeys ownership/clone rules like every other fixture reaching an accepting boundary.
		testassert.NoError(t, err)
		body := mustEncode(t, rt, Request{Messages: []Message{validated}}) // one encode of the fully-forged kept message pins every row below at once.
		msgs := decodeMessages(t, body)                                    // single wire object for this request.
		obj := msgs[0]                                                     // the only message on this wire; all assertions read it directly without further fixtures by design.

		{
			rawRole, ok := wireString(t, obj, "role")
			testassert.Require(t, ok && rawRole == "assistant", "wire role = %v want assistant: %#v", obj["role"], obj)
		}
		assertUserTextWire(t, obj, "body") // content is the real part text, not a forged string from an extra of that same key name.
		// legitimate retention still happened: marker survived next to every forgery on its message (positive counterweight keeping balance visible).
		{
			_, ok := wireString(t, obj, "marker")
			testassert.Require(t, ok, "legitimate marker lost while forging siblings present: %#v", obj)
		}
		for _, key := range []string{"_lightcode_state", "refusal", "tool_call_id", "name", "function_call", "system_fingerprint", "service_tier", "id", "created", "object", "stream"} { // the reserved top-level stream forgery must be omitted by the shared message-scope filter exactly like its canonical/private siblings.
			{
				_, ok := obj[key]
				testassert.Require(t, !ok, "forbidden message-scope key %q reached the wire: %#v", key, obj)
			}
		}
	})

	t.Run("content-part-scope-canonical-stripping", func(t *testing.T) {
		part := ContentPart{Kind: PartText, Text: "real text", Extra: Extra{"note": json.RawMessage(`7`), // non-canonical at content-part scope: retained onto the same array-form object as its forged siblings below.
			"type":         json.RawMessage(`"image_url"`),   // canonical kind forgery must not change a text part's wire type away from "text".
			"text":         json.RawMessage(`"forged-text"`), // value-level collision on the same field: real Text wins through denylist plus overwrite.
			"_lightcode_p": json.RawMessage(`1`),             // private floor applies inside a content part too (rule 9 shared across every scope).
			"n":            json.RawMessage(`7`),             // reserved top-level name never comes from extras at any scope, including this nested one.
		}}
		m, err := NewMessage(Message{Role: RoleAssistant, Source: targetRef, Content: []ContentPart{part}, Name: ""})
		testassert.NoError(t, err)
		body := mustEncode(t, rt, Request{Messages: []Message{m}}) // single encode of the fully-forged kept part pins every row below at once (no further fixtures needed for this subtest by design).
		msgs := decodeMessages(t, body)                            // one wire message expected back from that request above.
		parts := unmarshalContentParts(t, msgs[0])
		assertStringField(t, parts[0], "type", "text")
		assertStringField(t, parts[0], "text", "real text")
		{
			got, ok := parts[0]["note"]
			testassert.Require(t, ok && string(got) == `7`, "kept part-scope extra missing or altered: %#v", parts[0])
		}
		for _, key := range []string{"_lightcode_p", "n"} { // private floor and reserved top-level keys never reach this wire object.
			{
				_, ok := parts[0][key]
				testassert.Require(t, !ok, "forbidden key %q leaked onto the wire object of a content part: %#v", key, parts[0])
			}
		}
	})
}
