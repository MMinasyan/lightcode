package model

import (
	"encoding/json"
	"errors"
	"testing"
)

// FuzzParseStreamChunk exercises the fixed parser's invariants over arbitrary bytes: it must never panic, every failure is a protocol error wrapping ErrProtocol with nothing else leaking through untyped, and every successful parse yields a delta that passes its own accepting boundary (NewStreamDelta) — nonnegative positions throughout, closed content kinds, well-formed extras at all scopes. Seeds are the retained legacy fixture payloads ported inline as authoritative real-world wire shapes plus minimal malformed inputs so interesting coverage starts from corpus rather than cold mutation.
func FuzzParseStreamChunk(f *testing.F) { // fuzzing a pure function needs no server or state — only input bytes and invariants on its output... (seeds below are the entire "real world" this parser was ported to handle, kept inline so nothing external is required for corpus bootstrapping).

	for _, seed := range []string{
		// z.ai thinking-mode fixture: reasoning_content at delta scope flows into message-scope extras while tool-call fragments normalize normally... (the exact wire shape that motivated extra retention in the first place).
		`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"I should inspect "}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"the file first.","tool_calls":[{"index":0,"id":"call_read","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]},"finish_reason":"tool_calls"}]}`,

		// OpenRouter reasoning_details fixture: array-typed non-canonical delta fields plus a later tool-call continuation chunk sharing the same call id... (continuation correlation is assembly work, but parsing both halves must stay lossless).
		`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning":"I should inspect the request.","reasoning_details":[{"type":"reasoning.text","text":"I should inspect the request."},{"type":"reasoning.encrypted","data":"encrypted-block","format":"google-gemini-v1","index":0}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_list","type":"function","function":{"name":"run_command","arguments":"{\"command\":\"ls\"}"}}]},"finish_reason":"tool_calls"}]}`,

		// Gemini thought-signature fixture: nested google-specific extra at tool-call scope that must survive byte-for-byte for multi-turn replay... (the canonical example of why extras are raw JSON rather than decoded values).
		`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Example City\"}"},"extra_content":{"google":{"thought_signature":"thought-signature"}}}]},"finish_reason":"tool_calls"}]}`,

		// Rich single chunk covering usage, refusal, reasoning extras AND a tool call with an omitted index in the same delta... (maximum realistic field density from legacy seeds).
		`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3},"choices":[{"index":0,"delta":{"role":"assistant","content":"hello","refusal":"no","reasoning":"hidden","reasoning_details":[{"type":"reasoning.text","text":"a"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"},"extra_content":{"google":{"thought_signature":"sig"}}}]},"finish_reason":"tool_calls"}]}`,

		// Omitted-then-explicit index mix: normalization must assign 0 to the first call and keep the explicit one untouched... (the per-event fill rule under mixed supply patterns).
		`{"choices":[{"delta":{"tool_calls":[{"id":"call_omitted","type":"function","function":{"name":"read_file","arguments":"{}"},"extra_content":{"source":"omitted"}},{"index":0,"id":"call_explicit","type":"function","function":{"name":"write_file","arguments":"{}"},"extra_content":{"source":"explicit"}}]}}]}`,

		// Array-form content mixing a canonical text part and an opaque provider-specific one in wire order... (kind classification plus position fidelity across both forms at once).
		`{"choices":[{"delta":{"content":[{"type":"text","text":"visible","signature":"text-sig"},{"type":"thinking","thinking":"hidden","signature":"opaque-sig"}]}}]}`,

		// Malformed seeds: invalid JSON, empty object (valid but degenerate), and a wrong top-level shape — all three must fail or succeed through the same typed paths without panicking on any mutation of these bytes.
		`{not-json}`,
		`{}`,
		`[]`,

		// Minimal structural probes around every strict-typing boundary: nulls at each canonical field, wrong types where strings are required... (mutations from here reach interesting type-error branches quickly rather than wandering the whole input space first).
		`{"choices":[{"delta":{"role":null,"content":null,"refusal":null},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":-1}]}]}}`,
		`{"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":9}}}`, // cached exceeding prompt: exercises the floor clamp under mutation.
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, raw []byte) { // one pure entry point call per corpus case — no I/O, no state between cases... (the parser is shared by the stream decoder and direct consumers so its invariants must hold standalone exactly like this).
		delta, err := parseStreamChunkRaw(json.RawMessage(raw))

		if err != nil {
			if !errors.Is(err, ErrProtocol) { // every failure path in this function wraps the umbrella sentinel — a raw stdlib error escaping here would break upstream classification for ALL stream-level failures at once... (this single check is what makes "typed protocol errors only" an executable guarantee rather than a code-review promise).
				t.Fatalf("parseStreamChunkRaw returned non-protocol error: %v", err) // full cause rendered so the exact un-wrapped path that leaked it is visible alongside its input bytes for direct reproduction.
			}
			return // successful-parse invariants below do not apply to failed parses — their contract IS the sentinel above and nothing more at this boundary... (further shape assertions on a zero delta would just re-encode what "error returned" already implies).
		}

		if _, derr := NewStreamDelta(delta); derr != nil { // THE accepting-boundary invariant: parser output must always be constructible through the public validation path — any drift between internal normalization and external contract (negative positions, open kinds, malformed extras) surfaces here instead of at some later assembly stage where it would be far harder to attribute... (this is what keeps "parser satisfies its own consumer's entry point" continuously proven rather than assumed after each edit).
			t.Fatalf("successfully-parsed delta fails NewStreamDelta validation: %v", derr) // both the validating error and implicit input bytes via f.Fuzz reproduction make this immediately actionable without additional instrumentation.
		}

		if delta.Usage != nil && (delta.Usage.InputTokens < 0 || delta.Usage.CachedInputTokens < 0 || delta.Usage.OutputTokens < 0) { // usage normalization's single floor guarantees nonnegative uncached input; cached/output pass through as-reported signed ints which real providers keep non-negative... pinning all three here catches any arithmetic regression in either direction (a clamp removed OR a new accidental one introduced on the wrong field).
			t.Fatalf("normalized usage carries negative values: %+v", delta.Usage) // full struct rendered so whichever of the three fields went negative is named directly against its original wire input for comparison.
		}

		for i, frag := range delta.ContentFragments { // content positions are nonnegative by construction (array index / fixed zero); NewStreamDelta above already re-validates this but naming it here keeps THIS target's coverage explicit independent of that helper ever changing... (defense-in-depth across two validation layers is cheap insurance against one being weakened while the other compensates silently).
			if frag.Position < 0 {
				t.Fatalf("content fragment[%d] has negative position %d", i, frag.Position) // index included so multi-fragment payloads attribute their failure to its exact wire slot rather than requiring a re-run with verbose output enabled.
			}
		}

		for i, frag := range delta.ToolFragments { // tool-call positions are ALWAYS concrete non-nil values from parsed output (omission is normalized before any delta leaves the parser) — pinning nil-ness here catches a regression where normalization gets skipped for some mutated input shape table tests don't cover...
			if frag.Position == nil || *frag.Position < 0 { // grouped as one condition since both halves describe THE SAME contract ("concrete nonnegative position always") from complementary angles: absence-of-normalization vs negative-value-escape.
				t.Fatalf("tool call fragment[%d] lacks a concrete normalized position (nil or negative)", i) // names the exact invariant violated so future readers find its source rule quickly instead of re-deriving scope containment relationships from scratch during triage.
			}
		}

		if len(delta.ToolFragments) > 0 && !delta.HasChoice { // tool fragments only ever accompany a present choice (they parse from inside that single first-choice's delta object) — the checkable direction of this correlation: fragments WITHOUT their enclosing choice marker would mean normalization ran on a scope whose parent was reported absent.
			t.Fatalf("tool call fragments present without any choice in delta output") // names the exact invariant violated for direct attribution to its source rule during triage.
		}
	})
}
