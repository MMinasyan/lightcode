package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestParseStreamChunkRaw pins the fixed parser contract row by row: top-level chunk shape typing, usage normalization arithmetic with its single floor at zero, first-choice-only selection, raw pass-through of role/finish reasons and tool-call name/argument pieces, strict typing for every present non-null canonical field while null stays absent at each scope, closed content-kind classification with byte-for-byte extra retention under the retained denylists, per-event lowest-unused-index normalization for omitted tool indexes (with supplied duplicates tolerated), and protocol errors naming their exact offending shape for any wrong type.
func TestParseStreamChunkRaw(t *testing.T) { // one table because every row exercises the same pure entry point with only its payload shape varying... (shared harness keeps each row's intent readable as a single data line plus targeted post-checks).

	type parseRow struct {
		name   string          // wire scenario this payload represents.
		raw    json.RawMessage // exact input bytes for one chunk event.
		errSub string          // non-empty: failure expected and its message must contain exactly this fragment (sentinel reachability asserted separately below in every error case).
		check  func(t *testing.T, d StreamDelta)
	}

	for _, r := range []parseRow{
		// --- top-level shape and choice selection -----------------------------------------

		{name: "emptyObjectIsZeroDelta", raw: json.RawMessage(`{}`), check: func(t *testing.T, d StreamDelta) { // the most degenerate valid chunk: nothing present at all must produce a fully-zero delta rather than any error or invented data.
			if d.HasChoice || d.Usage != nil || len(d.ContentFragments) > 0 || len(d.ToolFragments) > 0 || len(d.MessageExtra) > 0 {
				t.Fatalf("zero-payload delta = %#v; want every field at its zero value", d) // full rendering so any single invented byte is visible immediately.
			}
		}},

		{name: "choicelessUsageEventHasNoChoiceMarker", raw: json.RawMessage(`{"usage":{"prompt_tokens":10,"completion_tokens":4}}`), check: func(t *testing.T, d StreamDelta) { // a trailing usage-only chunk carries no choice at all — HasChoice must stay false so assemblers can distinguish "no delta data" from an empty-but-present one.
			if d.HasChoice || len(d.ContentFragments) > 0 {
				t.Fatalf("usage-only delta = %#v; want no choice and no fragments", d) // conflating these two shapes would corrupt assembly's continuation logic downstream.
			} else if !d.UsageMatches(10, 0, 4) {
				t.Fatalf("usage from usage-only event = %+v; want input=10 cached=0 output=4 (no details object means zero cache)", d.Usage) // this row doubles as the normalization baseline for every arithmetic case below.
			}
		}},

		{name: "usageNullStaysAbsent", raw: json.RawMessage(`{"choices":[],"usage":null}`), check: func(t *testing.T, d StreamDelta) { // explicit null usage is indistinguishable from absence per the fixed rule — inventing a zero Usage pointer here would make callers unable to tell "reported nothing" from "not reported at all".
			if d.Usage != nil || d.HasChoice {
				t.Fatalf("null-usage delta = %#v; want no usage and no choice marker", d) // full shape rendered for immediate triage.
			}
		}},

		{name: "topLevelScalarRejectedAsProtocolError", raw: json.RawMessage(`123`), errSub: "did not decode into the OpenAI-compatible chunk shape"},

		{name: "choicesWrongTypeNamesTheField", raw: json.RawMessage(`{"choices":5}`), errSub: `at field choices`}, // a number where an array of choice objects is required fails at unmarshal with the field name recovered from the stdlib type error — callers see exactly which wire key broke.

		{name: "choiceElementNotAnObjectRejected", raw: json.RawMessage(`{"choices":[42]}`), errSub: `choices[0] is not a JSON object`}, // scalars or arrays at choices[0] are not OpenAI-compatible; the element position is named so multi-choice payloads pin down WHICH slot failed.

		{name: "onlyFirstChoiceIsUsed", raw: json.RawMessage(`{"choices":[{"delta":{"content":"first"}},{"delta":{"content":"second"}}]}`), check: func(t *testing.T, d StreamDelta) { // requests fix n:1 so the second choice must be ignored entirely — reading it would invent content no consumer asked for and break accumulation identity.
			if len(d.ContentFragments) != 1 || !d.FragIsText(0, "first") {
				t.Fatalf("fragments = %#v; want exactly the FIRST choice's single text fragment", d.ContentFragments) // full slice rendered so an accidental second-fragment inclusion is visible without re-running.
			} else if len(d.MessageExtra) > 0 || !d.UsageIsNil() {
				t.Fatalf("non-choice data invented from a two-content payload: %#v; want nothing but the first fragment", d) // belt-and-braces that no other field absorbed any of the ignored choice's values indirectly through later edits.
			}
		}},

		// --- usage normalization arithmetic -------------------------------------------------

		{name: "uncachedInputIsPromptMinusCachedWithFloorAtZero", raw: json.RawMessage(`{"usage":{"prompt_tokens":3,"completion_tokens":7,"total_tokens":99,"prompt_tokens_details":{"cached_tokens":5}}}`), check: func(t *testing.T, d StreamDelta) { // the only clamp in this arithmetic is max(prompt-cached, 0): here it fires because cached exceeds prompt — total tokens have no field and must not influence anything.
			if !d.UsageMatches(0, 5, 7) {
				t.Fatalf("clamped usage = %+v; want input=0 (floored from -2), cached=5, output=7", d.Usage) // full value rendered so which of the three fields drifted is obvious at a glance.
			} else if d.Usage.InputTokens < 0 {
				t.Fatalf("negative uncached tokens escaped the floor: %+v", d.Usage) // explicit re-check even though UsageMatches above implies it — future edits to that helper must not silently drop this guarantee from view here.
			}
		}},

		{name: "usageDetailsAbsentMeansZeroCache", raw: json.RawMessage(`{"usage":{"prompt_tokens":8,"completion_tokens":2}}`), check: func(t *testing.T, d StreamDelta) { // absent details object normalizes to zero cached — the retained producer rule that a missing sub-object is NOT an error and contributes nothing.
			if !d.UsageMatches(8, 0, 2) {
				t.Fatalf("no-details usage = %+v; want input=8 (full prompt), cached=0, output=2", d.Usage) // full rendering for immediate triage of any arithmetic drift in this direction too.
			}
		}},

		{name: "negativeCachedAndOutputPassThroughUnchanged", raw: json.RawMessage(`{"usage":{"prompt_tokens":5,"completion_tokens":-2,"prompt_tokens_details":{"cached_tokens":-3}}}`), check: func(t *testing.T, d StreamDelta) { // the retained signed-int producer clamps ONLY the uncached derivation: cached and output keep every bit of their as-reported value, negative included (no range or clamping policy may be invented on either).
			if !d.UsageMatches(8, -3, -2) { // input = 5 - (-3) = 8 by signed arithmetic; the two negative fields must survive byte-for-byte as ints.
				t.Fatalf("signed usage = %+v; want input=8 while cached=-3 and output=-2 pass through unchanged", d.Usage) // all three values rendered so a clamp added to either negative field — or removed from the uncached one — is visible at a glance.
			}
		}},

		{name: "floorAppliesToUncachedInputOnlyWhileOtherFieldsStayNegative", raw: json.RawMessage(`{"usage":{"prompt_tokens":1,"completion_tokens":-6,"prompt_tokens_details":{"cached_tokens":4}}}`), check: func(t *testing.T, d StreamDelta) { // the single max(prompt-cached, 0) floor fires here (1-4 → 0) while cached=4 and output=-6 remain untouched — proving the clamp's reach is exactly one field even when every other value on the same object is out of the nonnegative range.
			if !d.UsageMatches(0, 4, -6) {
				t.Fatalf("floored usage = %+v; want input=0 (floored from -3) with cached=4 and output=-6 untouched by any clamp", d.Usage) // full rendering pins each field's exact survival, so an over-reaching clamp trips this row on whichever field it touched.
			}
		}},

		// --- raw pass-through fields --------------------------------------------------------

		{name: "roleAndFinishReasonPassThroughVerbatim", raw: json.RawMessage(`{"choices":[{"delta":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`), check: func(t *testing.T, d StreamDelta) { // both values are RAW wire strings — normalization (developer→system etc.) is assembler work and must never happen at parse time or downstream replay would double-normalize.
			if !d.HasChoice || d.Role != "assistant" || d.FinishReason != "stop" {
				t.Fatalf("pass-through delta = %#v; want role=assistant finish_reason=stop verbatim", d) // full shape rendered so any accidental transformation is visible in one line of output.
			}
		}},

		{name: "nullRoleRefusalAndFinishReasonStayAbsent", raw: json.RawMessage(`{"choices":[{"delta":{"role":null,"refusal":null},"finish_reason":null}]}`), check: func(t *testing.T, d StreamDelta) { // null means absent at every string-typed canonical field — a present-null coerced to "" would be indistinguishable from genuinely-supplied empty values in later assembly.
			if !d.HasChoice || d.Role != "" || d.RefusalFragment != "" || d.FinishReason != "" {
				t.Fatalf("null-fields delta = %#v; want all three string fields at their absent-empty value", d) // full rendering so any single null→value coercion is attributable to its exact field.
			}
		}},

		{name: "wrongTypedFinishReasonRejectedNamingItsField", raw: json.RawMessage(`{"choices":[{"delta":{"content":"x"},"finish_reason":3}]}`), errSub: `choice.finish_reason has the wrong type (want string)`}, // present non-null values must carry their OpenAI-compatible JSON-string type — numbers are malformed input, never silent drops.

		{name: "wrongTypedRefusalRejectedNamingItsField", raw: json.RawMessage(`{"choices":[{"delta":{"refusal":true}}]}`), errSub: `choice.delta.refusal has the wrong type (want JSON string)`}, // same strictness at refusal scope with its exact field path named for wire-side diagnosis.

		{name: "wrongTypedRoleRejectedNamingItsField", raw: json.RawMessage(`{"choices":[{"delta":{"role":7}}]}`), errSub: `choice.delta.role has the wrong type (want JSON string)`}, // role is a delta-scope string with identical rules — pinning it here keeps all three string fields' strictness provably uniform rather than spot-checked.

		{name: "wrongTypedNameRejectedNamingItsField", raw: json.RawMessage(`{"choices":[{"delta":{"name":5}}]}`), errSub: `choice.delta.name has the wrong type (want JSON string)`}, // name is canonical at message-delta scope yet carries no StreamDelta field: its type must still be enforced before the exclusion drops it, or wrong-typed wire data would pass as silence.

		{name: "wrongTypedToolCallIDRejectedNamingItsField", raw: json.RawMessage(`{"choices":[{"delta":{"tool_call_id":[]}}]}`), errSub: `choice.delta.tool_call_id has the wrong type (want JSON string)`}, // same rule as name: canonical-but-fieldless members are type-checked, never silently absorbed into extras or dropped unverified.

		{name: "wrongTypedFunctionCallRejectedNamingItsField", raw: json.RawMessage(`{"choices":[{"delta":{"function_call":"read_file"}}]}`), errSub: `choice.delta.function_call has the wrong type (want JSON object)`}, // function_call is the object-typed member of the fieldless trio — a bare string where the legacy {name,arguments} object belongs is a protocol violation, not ignorable noise.

		{name: "nullNameToolCallIDAndFunctionCallStayAbsent", raw: json.RawMessage(`{"choices":[{"delta":{"name":null,"tool_call_id":null,"function_call":null,"custom_field":1}}]}`), check: func(t *testing.T, d StreamDelta) { // null remains absent for every fieldless canonical member — the type checks must not turn null into a failure, and the exclusion must not let null re-enter extras.
			if len(d.MessageExtra) != 1 || !d.ExtraEquals(d.MessageExtra, "custom_field", `1`) {
				t.Fatalf("message extras = %#v; want exactly custom_field retained beside the null trio", d.MessageExtra) // full map rendered so any null-key retention or sibling loss is visible in one line.
			} else if _, ok := d.MessageExtra["name"]; ok {
				t.Fatalf("null name leaked into message extras: %#v", d.MessageExtra) // the absent-by-rule direction pinned explicitly rather than implied by map length alone.
			} else if _, ok := d.MessageExtra["tool_call_id"]; ok {
				t.Fatalf("null tool_call_id leaked into message extras: %#v", d.MessageExtra) // same per-key pin for the second fieldless member.
			} else if _, ok := d.MessageExtra["function_call"]; ok {
				t.Fatalf("null function_call leaked into message extras: %#v", d.MessageExtra) // third member pinned for the same reason as its siblings.
			}
		}},

		{name: "contentWrongTypeRejected", raw: json.RawMessage(`{"choices":[{"delta":{"content":42}}]}`), errSub: `delta.content has a wrong type (want JSON string or array of part objects)`}, // neither wire form matches a bare number — rejecting loudly beats inventing either representation downstream.

		{name: "stringContentBecomesOnePositionZeroTextFragment", raw: json.RawMessage(`{"choices":[{"delta":{"content":"hello world"}}]}`), check: func(t *testing.T, d StreamDelta) { // the string form is exactly one text fragment at position zero — its identity is fixed by the wire shape itself rather than any provider-supplied index.
			if len(d.ContentFragments) != 1 || !d.FragIsText(0, "hello world") {
				t.Fatalf("string-form fragments = %#v; want exactly one text fragment at position zero with that literal value", d.ContentFragments) // full slice rendered so count/position/kind/text drift is all visible in a single failure line.
			} else if len(d.MessageExtra) > 0 {
				t.Fatalf("unexpected message extras from an all-canonical delta: %#v", d.MessageExtra) // negative anchor proving canonical-key suppression works even when nothing non-canonical exists to trigger the code path by contrast... (the positive retention row below covers the affirmative side).
			}
		}},

		{name: "presentEmptyStringContentStillProducesAFragment", raw: json.RawMessage(`{"choices":[{"delta":{"content":""}}]}`), check: func(t *testing.T, d StreamDelta) { // present-but-empty string content is NOT absent — it still yields one empty text fragment at position zero (the distinction matters for providers that signal stream phases via deliberate empty pieces).
			if len(d.ContentFragments) != 1 || !d.FragIsText(0, "") {
				t.Fatalf("empty-string-form fragments = %#v; want exactly ONE fragment with an EMPTY text piece at position zero", d.ContentFragments) // full rendering so "dropped entirely" vs "wrong kind/position" failures are distinguishable without re-running under verbose mode.
			}
		}},

		{name: "arrayFormUsesWireIndicesAsPositionsWithClosedKindsAndExtras", raw: json.RawMessage(`{"choices":[{"delta":{"content":[{"type":"text","text":"visible"},{"type":"image_url","image_url":{"url":"http://x/y.png"}},{"type":"thinking","thoughts":"hidden","signature":"sig"}]}}]}`), check: func(t *testing.T, d StreamDelta) { // THE polymorphic content row: three parts in wire order exercising all three closed kinds plus opaque extra retention — positions are exactly the array indices and nothing else.
			if len(d.ContentFragments) != 3 || !d.FragIsText(0, "visible") || !d.FragIsImageURL(1, "http://x/y.png") { // first two kinds asserted structurally before the opaque one below so any positional shift is attributed to its earliest visible symptom rather than discovered late in this row's output.
				t.Fatalf("array-form fragments = %#v; want text@0 then image_url@1 with that exact URL value", d.ContentFragments) // full slice rendered for immediate triage of count/position/kind/url drift.
			} else if !d.FragIsOpaque(2, "thinking") || len(d.ContentFragments[2].Extra) != 2 {
				t.Fatalf("opaque fragment = %#v; want kind=PartOpaque wire-type=thinking at position two with both non-canonical fields retained", d.ContentFragments[2]) // count included so an accidental extra key (or missing one) fails as loudly as a wrong value would.
			} else if !d.ExtraEquals(d.ContentFragments[2].Extra, "thoughts", `"hidden"`) || !d.ExtraEquals(d.ContentFragments[2].Extra, "signature", `"sig"`) { // byte-for-byte retention: the raw JSON literal (quoted string) is what stays in extras — decoding or re-encoding anything here would corrupt opaque replay.
				t.Fatalf("opaque part extras = %#v; want both fields retained byte-for-byte as their original wire literals", d.ContentFragments[2].Extra) // full map rendered so which of the two values drifted (or was dropped/added) is immediately visible alongside its key name for direct comparison against the payload above.
			} else if _, ok := d.ContentFragments[1].Extra["url"]; ok { // image_url's consumed subfield must not ALSO leak into extras at part scope — url belongs to the fragment struct, duplication would make replay ambiguous about which copy is authoritative... (denylist discipline pinned from this side rather than assuming it from reading source).
				t.Fatalf("image_url part leaked its consumed field into extras: %#v", d.ContentFragments[1].Extra) // naming the exact key makes any future denylist regression attributable without re-deriving which scope owns that value.
			} else if len(d.MessageExtra) > 0 {
				t.Fatalf("delta-scope extras unexpectedly populated from a content-only payload: %#v", d.MessageExtra) // another negative anchor for the same suppression rule this row's positive checks exercise one level up in scope... (keeping both scopes' silence asserted keeps their code paths independently pinned rather than cross-covering by accident).
			}
		}},

		{name: "opaquePartRetainsEveryFieldExceptTypeInExtras", raw: json.RawMessage(`{"choices":[{"delta":{"content":[{"type":"thinking","text":"raw-text-field","image_url":{"url":"http://x"},"thoughts":"h"}]}}]}`), check: func(t *testing.T, d StreamDelta) { // opaque parts keep the wire type ONLY in OpaqueWireType and preserve EVERY other field — including names such as text and image_url — byte-for-byte in part extras... (unlike canonical kinds where those keys are consumed into struct values, an unclassifiable kind has no consuming fields so dropping them would lose provider data that replay needs intact).
			if len(d.ContentFragments) != 1 || !d.FragIsOpaque(0, "thinking") {
				t.Fatalf("opaque fragments = %#v; want one opaque fragment at position zero with wire type thinking", d.ContentFragments) // precondition shape check before the extra-content assertions below so a wrong count/kind fails first with full slice rendering.
			} else if got := d.ContentFragments[0]; len(got.Extra) != 3 {
				t.Fatalf("opaque part extras = %#v; want ALL three non-type fields retained (text, image_url, thoughts)", got.Extra) // count pinned first so under-retention of any specific key fails loudly before value-level checks could pass vacuously on a smaller map.
			} else if !d.ExtraEquals(got.Extra, "text", `"raw-text-field"`) {
				t.Fatalf("opaque part extras = %#v; want the text field retained byte-for-byte as its original wire literal inside extras (not consumed into any struct value)", got.Extra) // naming WHICH key is missing makes a future re-introduction of canonical-kind denylists at this scope immediately attributable to exactly that exclusion.
			} else if !d.ExtraEquals(got.Extra, "image_url", `{"url":"http://x"}`) {
				t.Fatalf("opaque part extras = %#v; want the image_url field retained byte-for-byte as its original wire object literal inside extras (not consumed into any struct value)", got.Extra) // nested-object fidelity asserted through exact bytes since decoding-and-re-encoding would be an equally silent corruption of replay data.
			} else if !d.ExtraEquals(got.Extra, "thoughts", `"h"`) {
				t.Fatalf("opaque part extras = %#v; want the provider-specific field retained alongside its canonical-named siblings without any preferential dropping of either class", got.Extra) // all three keys asserted individually (not just by count above) so each named-key and unnamed-key retention path is independently pinned against partial-regression shapes.
			} else if _, ok := got.Extra["type"]; ok {
				t.Fatalf("structural wire type leaked into opaque part extras: %#v; only OpaqueWireType carries it", got.Extra) // the one field that must NOT be an extra is named explicitly — its absence here plus presence in FragIsOpaque above together pin exactly where classification data lives.
			} else if _, derr := NewStreamDelta(d); derr != nil { // accepting-boundary compatibility re-verified for this exact shape: canonical-named extras inside opaque parts must pass the same validation every other parsed delta does... (guarding against a future tightening of that boundary breaking THIS parser's output specifically).
				t.Fatalf("opaque part with text/image_url extras fails NewStreamDelta acceptance: %v", derr) // full cause rendered so which validator rejected this legitimate shape is named directly for triage without re-running under verbose mode.
			}
		}},

		{name: "contentPartWithoutAUsableTypeIsRejected", raw: json.RawMessage(`{"choices":[{"delta":{"content":[{"text":"x"}]}}]}`), errSub: `carries no usable wire type (field "type" must be present as a JSON string)`}, // unclassifiable parts cannot enter the closed kind set — unlike legacy pass-through, malformed shape here is an error rather than guessed data flowing downstream into assembly.

		{name: "contentPartWithWrongTypedTextRejected", raw: json.RawMessage(`{"choices":[{"delta":{"content":[{"type":"text","text":5}]}}]}`), errSub: `has the wrong type`}, // present non-null text must be its JSON-string wire type — numbers are protocol violations, never coerced values.

		{name: "imageUrlMustBeAnObjectWhenPresent", raw: json.RawMessage(`{"choices":[{"delta":{"content":[{"type":"image_url","image_url":"http://direct"}]}}]}`), errSub: `field "image_url" has a wrong type (want JSON object)`}, // the OpenAI shape nests url inside an object — a bare string is malformed input at this scope and fails before any subfield reading happens.

		{name: "nullImageUrlLeavesAnEmptyURLFragment", raw: json.RawMessage(`{"choices":[{"delta":{"content":[{"type":"image_url","image_url":null}]}}]}`), check: func(t *testing.T, d StreamDelta) { // null image_url is absent-by-rule and yields an empty-URL fragment rather than inventing a value or erroring — same tolerance as every other optional subfield in this parser.
			if len(d.ContentFragments) != 1 || !d.FragIsImageURL(0, "") {
				t.Fatalf("null-image-url fragments = %#v; want one image_url fragment at position zero with an EMPTY url value", d.ContentFragments) // full rendering so the empty-vs-missing distinction is visible in output rather than requiring a debugger session to confirm which branch produced what.
			}
		}},

		{name: "textPartNonCanonicalFieldsBecomeExtrasVerbatim", raw: json.RawMessage(`{"choices":[{"delta":{"content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}}]}`), check: func(t *testing.T, d StreamDelta) { // non-canonical part-scope keys (denylist is exactly type/text/image_url) are retained byte-for-byte — provider-specific annotations like cache control must survive round-trips for replay fidelity.
			frag := d.ContentFragments[0]                                          // single fragment makes direct field access below readable without index bookkeeping noise in failure messages... (this row's payload deliberately keeps everything else at its simplest legal shape so ONLY the extra-retention behavior varies from sibling rows).
			if _, ok := frag.Extra["cache_control"]; !ok || len(frag.Extra) != 1 { // count assertion included so an accidental type/text/image_url leak (denylist violation in the OTHER direction — over-retention of canonical keys) fails just as loudly as under-retention would.
				t.Fatalf("text part extras = %#v; want exactly cache_control retained and nothing else", frag.Extra) // full map rendered for immediate triage of which key was missing or extra.
			} else if !d.ExtraEquals(frag.Extra, "cache_control", `{"type":"ephemeral"}`) {
				t.Fatalf("retained extra value = %s; want byte-identical original wire literal", frag.Extra["cache_control"]) // byte-for-byte comparison (not decoded-re-encoded) because any normalization here would corrupt opaque replay in ways invisible until much later phases... (this is THE property every Extra field in this package must satisfy).
			} else if !frag.IsText("a") {
				t.Fatalf("fragment kind/text drifted alongside extra retention: %#v; want the text piece itself intact", frag) // cheap belt-and-braces that adding extras didn't accidentally consume or alter the canonical value sharing the same part object... (one shared payload exercising two behaviors keeps this table from growing a duplicate row for marginal coverage).
			}
		}},

		{name: "messageScopeDenylistExcludesFieldlessCanonicalKeysToo", raw: json.RawMessage(`{"choices":[{"delta":{"name":"n","tool_call_id":"t1","function_call":{"x":1},"custom_field":{"a":[1,2]}}}]}`), check: func(t *testing.T, d StreamDelta) { // message-scope extras exclude ALL denylist members — including name/tool_call_id/function_call which have NO StreamDelta field at all (pure exclusion keys)... pinning exactly the trap a naive "has corresponding struct field" implementation would fall into.
			if len(d.MessageExtra) != 1 || !d.ExtraEquals(d.MessageExtra, "custom_field", `{"a":[1,2]}`) { // custom_field is the only non-canonical key present and must be retained byte-for-byte... (its nested array value makes any accidental decode/re-encode drift visible through whitespace or ordering changes in the comparison below).
				t.Fatalf("message extras = %#v; want exactly custom_field with its original wire literal intact", d.MessageExtra) // full map rendered so which of the three exclusion keys leaked — or whether the retained one was mangled — is immediately attributable from output alone.
			} else if _, ok := d.MessageExtra["name"]; ok {
				t.Fatalf("denylist key name escaped into message extras: %#v", d.MessageExtra) // explicit per-key negative check even though len()==1 above already implies it... (making each excluded member individually named in a failure keeps future denylist edits from silently re-adding one without this row noticing which specific key regressed).
			} else if _, ok := d.MessageExtra["tool_call_id"]; ok {
				t.Fatalf("denylist key tool_call_id escaped into message extras: %#v", d.MessageExtra) // same rationale as the name check above — both are fieldless canonical members whose only observable contract IS this exclusion.
			} else if _, ok := d.MessageExtra["function_call"]; ok {
				t.Fatalf("denylist key function_call escaped into message extras: %#v", d.MessageExtra) // third and final member of the same trap class... (all three checked individually because a future "simplify to has-struct-field" rewrite would re-introduce exactly these, not some other keys).
			}
		}},

		{name: "toolCallAbsentOrNullMeansNoFragments", raw: json.RawMessage(`{"choices":[{"delta":{"content":"x","tool_calls":null}}]}`), check: func(t *testing.T, d StreamDelta) { // absent and null tool_calls are both "no data" — inventing an empty-but-present slice would make downstream correlation logic unable to distinguish these from a genuinely-empty provider array... (nil here is the only correct representation of absence at this boundary).
			if len(d.ToolFragments) != 0 || d.ToolFragments != nil { // length check catches any non-nil content; the explicit nil comparison pins the REPRESENTATION itself rather than merely its emptiness... (representation matters because assembly code may branch on nil-ness specifically for allocation decisions later in this phase's remaining commits).
				t.Fatalf("null tool_calls produced fragments: %#v", d.ToolFragments) // full slice rendered so an accidental single-empty-fragment construction is visible with all its zero fields rather than hiding behind a mere count mismatch.
			} else if len(d.ContentFragments) != 1 {
				t.Fatalf("sibling content fragment lost alongside null tool_calls check: %#v; want the text piece still delivered normally", d.ContentFragments) // negative-coupling guard proving that rejecting/ignoring one optional field does not disturb its siblings' parsing... (cheap insurance against a shared early-return path being introduced later between these two scopes).
			}
		}},

		{name: "omittedToolIndexesNormalizeToLowestUnusedPerEvent", raw: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{}, {"index":2}]}}]}`), check: func(t *testing.T, d StreamDelta) { // omitted index in wire position zero claims the lowest unused slot (0 here since 2 is already claimed by its sibling)... this row pins that normalization happens PER EVENT before any delta leaves the parser — consumers never see nil positions from parsed output.
			if len(d.ToolFragments) != 2 || d.ToolFragments[0].Position == nil || *d.ToolFragments[0].Position != 0 {
				t.Fatalf("normalized fragments = %#v; want first (omitted) call at exactly position zero", d.ToolFragments) // full rendering so which fragment got the wrong slot is visible alongside its sibling's for direct wire-order comparison without re-running under verbose mode.
			} else if *d.ToolFragments[1].Position != 2 {
				t.Fatalf("explicit index altered during normalization: %#v; want second call to KEEP its supplied value of two", d.ToolFragments) // explicit values must pass through untouched — the fill rule only fills omissions and never renumbers anything a provider deliberately placed.
			} else if _, ok := d.MessageExtra["tool_calls"]; ok {
				t.Fatalf("canonical tool_calls key leaked into message extras: %#v", d.MessageExtra) // denylist at delta scope applies to consumed fields too — pinning from this side keeps the exclusion provable even though no other row isolates it for this specific canonical member.
			}
		}},

		{name: "fillCursorStartsAtLowestUnusedNotWireOrder", raw: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"index":3}, {}]}}]}`), check: func(t *testing.T, d StreamDelta) { // the omitted call sits AFTER an explicit index-3 one in wire order — its normalized position must still be 0 (lowest unused overall for this event), proving the cursor scans from zero rather than continuing after previously-seen values... (this is THE distinction between "lowest unused" and "next-after-last" that a careless implementation would conflate).
			if len(d.ToolFragments) != 2 || *d.ToolFragments[0].Position != 3 {
				t.Fatalf("first fragment = %#v; want the explicit call to keep its supplied position three", d.ToolFragments) // full rendering for immediate triage of any off-by-one in how this row's payload maps onto expected slots.
			} else if *d.ToolFragments[1].Position != 0 {
				t.Fatalf("omitted fragment normalized to %d; want exactly zero (lowest unused slot regardless of where the explicit claim sits)", *d.ToolFragments[1].Position) // names both actual and wanted values explicitly because this specific confusion direction (getting 4 instead of 0) is plausible enough from a wrong cursor start that its failure message must leave no ambiguity about which rule broke.
			} else if *d.ToolFragments[0].Position == *d.ToolFragments[1].Position { // impossible given the two checks above but stated as an independent invariant anyway... (defense-in-depth against future edits restructuring pass order such that one of the explicit comparisons above starts comparing a mutated copy rather than distinct values).
				t.Fatalf("both fragments claimed position %d; they must occupy DISTINCT slots within one event", *d.ToolFragments[1].Position) // full value rendered so any shared-slot regression names its collision point directly in output.
			}
		}},

		{name: "duplicateSuppliedIndexesAreToleratedAndKeptVerbatim", raw: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"index":1},{"index":1}]}}]}`), check: func(t *testing.T, d StreamDelta) { // duplicate explicit indexes are MALFORMED-but-tolerated input (the assembler correlates them later by index/ID) — the parser must keep both fragments at their supplied position rather than erroring or renumbering either... (this exact tolerance is load-bearing for providers observed in legacy fixtures that repeat an id across continuation chunks).
			if len(d.ToolFragments) != 2 || *d.ToolFragments[0].Position != 1 || *d.ToolFragments[1].Position != 1 { // both positions asserted explicitly (not just equality between them) so a wrong-but-consistent renumbering like [0,0] fails loudly rather than passing an "equal to each other" check by accident...
				t.Fatalf("duplicate-index fragments = %#v; want BOTH calls keeping their supplied position one verbatim", d.ToolFragments) // full rendering makes the exact retained values visible for direct comparison against this row's payload literal without re-running.
			} else if _, ok := d.MessageExtra["tool_calls"]; ok {
				t.Fatalf("canonical tool_calls key leaked into message extras despite duplicate handling: %#v", d.MessageExtra) // same delta-scope denylist pin as sibling rows — included here specifically because the fill-rule code path is where a lazy implementation might "forget" to skip consumed keys when it special-cases duplicates... (cheap row-specific insurance that costs one line).
			}
		}},

		{name: "negativeToolIndexRejectedAsWrongType", raw: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"index":-1}]}}]}`), errSub: `tool_calls[0].index has the wrong type (want non-negative integer)`}, // negative values fail at typing scope with their element position named — no range policy beyond "non-negative" exists in this parser.

		{name: "fractionalToolIndexRejectedAsWrongType", raw: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"index":2.5}]}}]}`), errSub: `tool_calls[0].index has the wrong type (want non-negative integer)`}, // fractional literals are not integers regardless of their numeric value — spelling-level strictness retained from legacy's ensure-index rule which only accepts plain whole-number wire values.

		{name: "exponentSpelledToolIndexRejectedAsWrongType", raw: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"index":1e1}]}}]}`), errSub: `tool_calls[0].index has the wrong type (want non-negative integer)`}, // even numerically-whole exponent spellings like 1e1 (=10) fail — OpenAI-compatible indexes arrive as plain decimal literals on every observed wire shape and anything else is malformed input rather than a formatting variant to coerce.

		{name: "stringToolIndexRejectedAsWrongType", raw: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"index":"0"}]}}]}`), errSub: `tool_calls[0].index has the wrong type (want non-negative integer)`}, // quoted numbers are strings — a separate JSON type from number and rejected identically to other string-typed-at-number-field cases... (pinning this keeps "it parses as an int if you try" implementations out through their own test failure here rather than later in assembly).

		{name: "toolCallCanonicalFieldsAndExtrasSplitCorrectly", raw: json.RawMessage(`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"},"extra_content":{"google":{"thought_signature":"sig"}}}]}}]}`), check: func(t *testing.T, d StreamDelta) { // the richest single-call row in this table: every canonical call-scope field plus one non-canonical extra — asserting exact values at struct level AND byte-for-byte retention of the nested Google-specific payload (legacy fixture shape ported inline as an authoritative real-world example).
			if len(d.ToolFragments) != 1 {
				t.Fatalf("fragment count = %d; want exactly one for this single-call payload", len(d.ToolFragments)) // early exit with context rather than letting the detailed per-field checks below run against a wrong-shaped slice and produce confusing secondary failures in output... (one guard before deep inspection keeps triage linear).
			} else if frag := d.ToolFragments[0]; *frag.Position != 0 || frag.ID != "call_1" || frag.WireType != "function" || frag.Name != "read_file" { // four identity fields in one condition with distinct names in the message below so whichever drifted is attributable without re-running under verbose mode... (grouping keeps this table's row count from exploding for what is fundamentally ONE payload shape being verified holistically).
				t.Fatalf("canonical call fields = pos %v id %q type %q name %q; want 0/call_1/function/read_file verbatim", *frag.Position, frag.ID, frag.WireType, frag.Name) // every actual value rendered alongside its field label so the mismatch dimension is self-evident from output alone.
			} else if d.ToolFragments[0].ArgumentFragment != `{"path":"README.md"}` { // argument pieces pass through VERBATIM with no JSON validation at any point on this path — a provider's syntactically-broken partial arguments must survive parsing intact for assembly to concatenate and diagnose later... (validating here would corrupt mid-stream continuation exactly where legacy behavior proved providers DO send incremental broken-then-repaired sequences).
				t.Fatalf("argument fragment = %q; want byte-for-byte verbatim retention of the raw wire string", d.ToolFragments[0].ArgumentFragment) // quoted rendering so whitespace/escaping drift in either direction is visible character-by-character against this row's payload literal.
			} else if len(d.ToolFragments[0].Extra) != 1 || !d.ExtraEquals(d.ToolFragments[0].Extra, "extra_content", `{"google":{"thought_signature":"sig"}}`) { // the sole non-canonical key must be retained byte-for-byte — nested object values included without any decoding or re-encoding... (this exact payload shape comes from a real Gemini fixture where thought signatures are load-bearing for multi-turn tool replay).
				t.Fatalf("call extras = %#v; want exactly extra_content with its original nested wire literal intact", d.ToolFragments[0].Extra) // full map rendered so which of the two expectations failed (count vs value fidelity) is immediately distinguishable in aggregated test output.
			} else if _, ok := d.ToolFragments[0].Extra["index"]; ok { // call-scope denylist member index must NOT appear as an extra — it participates only in position normalization above... (pinning its exclusion explicitly rather than relying on len()==1 alone because a future "keep everything for debuggability" edit would add exactly this key first).
				t.Fatalf("consumed canonical field index leaked into call extras: %#v", d.ToolFragments[0].Extra) // naming the specific key keeps any denylist regression attributable to its exact member without re-deriving scope ownership from source.
			} else if _, ok := d.ToolFragments[0].Extra["id"]; ok {
				t.Fatalf("consumed canonical field id leaked into call extras: %#v", d.ToolFragments[0].Extra) // same rationale applied to the second member of this denylist... (each named individually because a partial-fix regression could plausibly exclude some members while leaking others).
			} else if _, ok := d.MessageExtra["role"]; ok {
				t.Fatalf("delta-scope canonical key role leaked into message extras: %#v", d.MessageExtra) // cross-scope negative anchor tying this row's delta-level field (present here as "assistant") to the SAME suppression rule its tool-call siblings exercise one level down... (one payload verifying both scopes' denylists keeps coverage dense without a separate duplicate-payload row).
			} else if !d.HasChoice || d.Role != "assistant" {
				t.Fatalf("sibling role field lost in this rich payload: %#v; want it delivered verbatim alongside the call", d) // final belt-and-braces that adding tool-call depth to a delta does not disturb its other canonical fields' parsing... (cheap insurance against shared early-exit paths between scopes being introduced by later edits).
			}
		}},

		{name: "wrongTypedFunctionObjectRejectedNamingItsField", raw: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"function":"read_file"}]}}]}`), errSub: `tool_calls[0].function has a wrong type (want JSON object)`}, // a bare string where the {name,arguments} object is required fails at structural typing before any subfield reading — element position named for direct wire diagnosis.

		{name: "nullFunctionLeavesEmptyNameAndArgumentFragments", raw: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"id":"call_x","function":null}]}}]}`), check: func(t *testing.T, d StreamDelta) { // null function is absent-by-rule yielding empty name AND argument pieces (not an error and not invented values)... pinning both halves of the pair explicitly because a partial-absence bug could plausibly zero one while retaining garbage in the other.
			if len(d.ToolFragments) != 1 || d.ToolFragments[0].Name != "" || d.ToolFragments[0].ArgumentFragment != "" { // grouped with distinct rendering below so which half (if either) drifted is attributable without re-running... (the id field above also doubles as proof that sibling canonical fields in the same call object survive this null-function row's payload intact).
				t.Fatalf("null-function fragment = %#v; want BOTH name and argument pieces at their absent-empty values", d.ToolFragments[0]) // full struct rendered so an accidental non-zero value in either string field is visible alongside every other zero for direct visual diffing against expectation.
			} else if d.ToolFragments[0].ID != "call_x" {
				t.Fatalf("sibling id lost next to null function: %#v; want call_x retained verbatim", d.ToolFragments[0]) // isolation proof that absence handling for ONE field does not zero its siblings within the same raw object... (the kind of cross-field corruption a shared "clear on error" code path would produce).
			} else if *d.ToolFragments[0].Position != 0 {
				t.Fatalf("omitted index normalization changed alongside null function handling: position %d; want zero as lowest unused", *d.ToolFragments[0].Position) // this call supplied NO explicit index — pinning its normalized value here proves the fill rule ran correctly even in a payload whose focus is elsewhere... (one row covering two behaviors keeps table growth bounded while coverage stays complete).
			}
		}},

		{name: "toolCallsNotAnArrayOfObjectsRejected", raw: json.RawMessage(`{"choices":[{"delta":{"tool_calls":"not-an-array"}}]}`), errSub: `delta.tool_calls is not an array of JSON objects`}, // the top-level typing of this field fails before per-element processing — a string/number/object here is malformed wire input rather than parseable data under any interpretation.
	} {
		t.Run(r.name, func(t *testing.T) { // subtest per row so a single mismatch names its scenario directly without re-running the whole table.
			d, err := parseStreamChunkRaw(r.raw)
			if r.errSub != "" {
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("parse = %v; want an error wrapping ErrProtocol (fragment %q)", err, r.errSub) // the umbrella sentinel is the contract identity callers rely on — a bare stdlib leak here would break every upstream classification.
				} else if !strings.Contains(err.Error(), r.errSub) {
					t.Fatalf("error = %q; want it to name the offending shape via fragment %q", err, r.errSub) // field-level naming is what makes provider-side wire bugs diagnosable from one log line without re-dumping payloads.
				}
				return
			}
			if err != nil {
				t.Fatalf("parse failed unexpectedly: %v", err) // success rows must stay silent — any failure here means the strictness rules over-reached beyond their documented scope.
			}
			r.check(t, d)
		})
	}

	if _, err := NewTransport(ResolvedTransport{Model: ModelRef{Provider: "openai"}}); !errors.Is(err, ErrInvalidModelRef) { // closing sanity anchor mirroring sibling files' convention — construction-time validation surface unchanged since this table was written... (kept deliberately cheap and local rather than extracted into a shared helper because each call site's context makes its failure message most useful when phrased for THAT row set specifically).
		t.Fatalf("premise broken: incomplete model identity no longer rejected at NewTransport") // same explicit framing as equivalent anchors elsewhere so grep finds every such guard consistently across the suite.
	}
}

// UsageMatches compares all three normalized usage fields at once — they derive from ONE arithmetic expression so partial checks would miss cross-field drift between siblings... (test-scope helper keeping each row's assertion a single readable condition rather than a triple of individual comparisons).
func (d StreamDelta) UsageMatches(input, cached, output int) bool { // nil-safe: a missing usage object fails every comparison at once with the same message shape as value mismatches do.
	return d.Usage != nil && d.Usage.InputTokens == input && d.Usage.CachedInputTokens == cached && d.Usage.OutputTokens == output
}

// FragIsText checks one specific fragment's identity in its exact expected slot — position AND kind AND text together, since any single dimension drifting (e.g. right value at wrong index) is equally a framing/normalization bug worth failing on first sight rather than discovering later... (indexing d directly keeps each call site readable without extracting yet another helper per assertion shape).
func (d StreamDelta) FragIsText(pos int, text string) bool { // nil-safe for the zero-length case: out-of-range positions simply report false so a wrong COUNT fails its own len check above with full slice rendering rather than panicking inside this comparison.
	if pos < 0 || pos >= len(d.ContentFragments) {
		return false
	}
	f := d.ContentFragments[pos] // single access point for all three fields below keeps the condition order deterministic in any future edits to this helper's body... (reading them separately would risk one dimension being checked against a different fragment than its siblings by accident).
	return f.Position == pos && f.Kind == PartText && f.Text == text
}

// FragIsImageURL is the sibling of FragIsText for image_url parts — same structural identity check with URL instead of Text... (keeping both helpers parallel means each row reads symmetrically when a payload exercises several part kinds in wire order).
func (d StreamDelta) FragIsImageURL(pos int, url string) bool { // nil-safe exactly like its text sibling above so count mismatches fail their len checks rather than panicking inside these comparisons.
	if pos < 0 || pos >= len(d.ContentFragments) {
		return false
	}
	f := d.ContentFragments[pos] // same single-access discipline as the text version for identical triage-readability reasons... (one field read per dimension keeps any future reordering of conditions from silently comparing mismatched dimensions against each other).
	return f.Position == pos && f.Kind == PartImageURL && f.URL == url
}

// FragIsOpaque checks an opaque-kind fragment's position, structural wire type, and that it carries NO text or URL values — the last clause is what distinguishes a correctly-classified opaque part from one whose canonical fields leaked across kinds... (cross-kind field exclusivity belongs to NewStreamDelta's validation but asserting it here too keeps THIS table self-sufficient about classification correctness even before any accepting boundary runs).
func (d StreamDelta) FragIsOpaque(pos int, wireType string) bool { // nil-safe like its siblings for the same count-mismatch-fails-its-len-check reason... (the three helpers share one body shape deliberately so their failure modes are identical and predictable across every row that uses them).
	if pos < 0 || pos >= len(d.ContentFragments) {
		return false
	}
	f := d.ContentFragments[pos] // single access point again for the same deterministic-condition-ordering rationale as both siblings above... (no behavioral difference from re-reading per field, just one fewer way to misattribute a failure dimension when triaging output).
	return f.Position == pos && f.Kind == PartOpaque && f.OpaqueWireType == wireType && f.Text == "" && f.URL == ""
}

// ExtraEquals compares ONE extra key's retained value byte-for-byte against its expected original wire literal — the comparison is on raw JSON bytes (not decoded values) because that exact fidelity IS the contract... (a helper rather than inline string() casts at every call site keeps each row readable while still failing with full map rendering from its Fatalf above when this returns false).
func (d StreamDelta) ExtraEquals(e Extra, key, wantLiteral string) bool { // nil-safe: a missing extra reports the same "not equal" outcome as any other mismatch so callers need no separate presence check on top of value checks... (presence is always independently pinned by len() assertions at call sites where count matters; this helper answers ONLY whether that one key's bytes match).
	got, ok := e[key] // single lookup shared by both the presence test and the byte comparison below... (two lookups would just duplicate work for zero clarity gain here since map access cost is irrelevant at test scale anyway).
	return ok && string(got) == wantLiteral
}

// UsageIsNil exists purely so rows whose payload contains no usage data can assert its absence with the same one-line style as their positive siblings... (without it those checks would need an inline "d.Usage != nil" clause that reads backwards next to a Fatalf message saying usage SHOULD be absent).
func (d StreamDelta) UsageIsNil() bool { return d.Usage == nil } // trivially small and deliberately so — any future growth of this check beyond pure absence is a signal the helper has outlived its purpose and should become an inline expression again.

// IsText checks whether ONE fragment value receiver carries exactly one text piece at position zero with that literal... (used by single-fragment rows where addressing through FragIsText would add index bookkeeping noise without adding any behavioral difference since there is only ever one element to inspect).
func (f ContentFragment) IsText(text string) bool {
	return f.Position == 0 && f.Kind == PartText && f.Text == text
} // deliberately NOT reusing the slice-level helper above because its receiver shape differs and forcing a conversion at every call site would obscure which fragment is actually being asserted on.
