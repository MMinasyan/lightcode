package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// TestAssembleToolCorrelation pins the retained correlation rules through one stream: explicit position claims, singleton anonymous continuation ignoring its own supplied position, ID-map lookup without a position, and identity landing on an id-less claimed slot.
func TestAssembleToolCorrelation(t *testing.T) {
	out, s := assemble(t, context.Background(), testRef,
		deltaStep(toolDelta(model.ToolCallFragment{Position: posI(0), ID: "a", Name: "fnA"})),       // claims slot 0 with identity a and name fnA per the explicit-position claim rule
		deltaStep(toolDelta(model.ToolCallFragment{Position: posI(9), ArgumentFragment: `{"x":1`})), // singleton anonymous fragment whose own supplied position must be IGNORED per the retained rule — it continues slot 0 regardless of that stale positional signal
		deltaStep(toolDelta(model.ToolCallFragment{Name: "fnB"})),
		deltaStep(toolDelta(model.ToolCallFragment{ID: "a", ArgumentFragment: `}`})), // non-anonymous fragment with an ID but NO position correlates through the recorded ID map back to slot 0
		deltaStep(toolDelta(model.ToolCallFragment{Position: posI(1), ID: "b"})),     // explicit position 1 already holds an entry with no decided identity yet — the fragment lands there and decides its stable id
		deltaStep(toolCallsDelta()), // trailing finish-reason chunk closes out the response normally per contract since both calls carry non-empty ids AND names at finalization time
	)

	expectStatus(t, out, model.OutputCompleted)
	assertSingleClose(t, s)

	calls := msgCalls(out)
	// exactly TWO valid calls survived finalization per the script above — anything else means either correlation misrouted a fragment somewhere upstream inside correlateToolFragment's own branch logic OR an unexpected third slot got erroneously materialized out of thin air

	testassert.Equal(t, len(calls), 2)

	// first call (slot 0) must carry identity a, name fnA and the concatenated argument pieces from both its contributing fragments
	testassert.Require(t, calls[0].ID == "a" && calls[0].Name == "fnA" && string(calls[0].Arguments) == `{"x":1}`, "call[0] = %#v", calls[0])

	// second call (slot 1) must carry identity b (landed via the explicit-position claim above), name fnB and NO arguments at all per contract — none of its contributing fragments ever carried an ArgumentFragment value
	testassert.Require(t, calls[1].ID == "b" && calls[1].Name == "fnB" && string(calls[1].Arguments) == "", "call[1] = %#v", calls[1])
}

// TestAssembleToolIdentityInvariant pins the one-to-one identity/position invariant: the same non-empty call ID arriving at a different normalized position than its recorded mapping is an errored conflict
func TestAssembleToolIdentityInvariant(t *testing.T) {
	out, s := assemble(t, context.Background(), testRef,
		deltaStep(toolDelta(model.ToolCallFragment{Position: posI(0), ID: "a", Name: "n"})),             // first arrival of identity a records its single normalized position 0 per the retained correlation rules
		deltaStep(toolDelta(model.ToolCallFragment{Position: posI(1), ID: "a", ArgumentFragment: "x"})), // SAME identity a arriving at DIFFERENT position 1 later
		deltaStep(stopDelta()), // trailing finish-reason chunk attempting to close out the response normally per contract even though an invariant violation already poisoned classification
	)

	expectStatus(t, out, model.OutputErrored) // for the identity-invariant conflict
	assertSingleClose(t, s)

	// NO eligible partial content was ever accumulated by this script — neither refusal nor any positioned content part nor message-scope extras arrived before or after the conflict fired, so finalization must carry no message at all

	// usage was never reported by any delta in this script per the fixture

	testassert.Require(t, out.Message == nil && out.Usage == nil, "errored output unexpectedly carried a message: %#v; errored output unexpectedly carried usage: %#v", out.Message, out.Usage)
}

// TestAssembleToolWireTypeRules pins omitted, equal-repeated and invalid supplied tool-call wire types through the closed set: omission means function by normalization upstream; identical repeats pass silently; any other value is an errored conflict at arrival.
func TestAssembleToolWireTypeRules(t *testing.T) {
	cases := []struct {
		name string
		wt   string
	}{
		{"omitted", ""},                       // no supplied wire type at all — normalization upstream already settled it to function before assembly ever sees the fragment
		{"function-equal-repeat", "function"}, // explicitly supplied value equal to the normalized default arriving twice at the same position — identical repeats must pass silently under any closed-set agreement rule
		{"invalid-value", "custom"},           // deliberately non-function wire type value — ANY deviation from the sole valid supplied constant is an errored structural conflict at arrival time
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var frag model.ToolCallFragment // build the single tool fragment
			frag.ID = "a"                   // identity field always present and non-empty so finalization can produce a valid call in the positive rows
			frag.Name = "n"                 // name field always present and non-empty for the same reason as identity
			if tc.wt != "" {
				frag.WireType = tc.wt
			}

			out, s := assemble(t, context.Background(), testRef,
				deltaStep(toolDelta(frag)),
				deltaStep(toolCallsDelta()),
			)

			if tc.wt == "custom" {
				expectStatus(t, out, model.OutputErrored) // Any non-function supplied wire type conflicts at arrival.
				assertSingleClose(t, s)                   // Error paths still release the stream exactly once.
			} else {
				expectStatus(t, out, model.OutputCompleted)
				assertSingleClose(t, s)

				calls := msgCalls(out)
				testassert.Require(t, len(calls) == 1 && calls[0].ID == "a" && calls[0].Name == "n", "calls = %#v", calls)
			}
		})
	}
}

// TestAssembleToolPositionKeepsFirstIdentity pins the position-to-ID side of the one-to-one identity invariant: a different non-empty ID arriving at an already-identified position never rebinds that slot — it correlates to a fresh normalized position, leaving each position mapped to at most one identity.
func TestAssembleToolPositionKeepsFirstIdentity(t *testing.T) {
	out, s := assemble(t, context.Background(), testRef,
		deltaStep(toolDelta(model.ToolCallFragment{Position: posI(0), ID: "a", Name: "fnA"})), // position 0 records identity a.
		deltaStep(toolDelta(model.ToolCallFragment{Position: posI(0), ID: "b", Name: "fnB"})), // same position, different identity: the reverse-direction pressure on the invariant.
		deltaStep(toolCallsDelta()),
	)

	expectStatus(t, out, model.OutputCompleted) // neither mapping is torn down — the conflict-free outcome is the invariant holding.
	assertSingleClose(t, s)

	calls := msgCalls(out)
	testassert.Require(t, len(calls) == 2 && calls[0].ID == "a" && calls[0].Name == "fnA" && calls[1].ID == "b" && calls[1].Name == "fnB", "calls = %#v, want a@position0 then b@fresh-position1", calls)
}

// TestAssembleToolWireTypeRepeats pins the repeated-supply side of the wire-type rule: an equal repeated type passes silently through correlation, and a differing repeated type errors as a conflicting supply
func TestAssembleToolWireTypeRepeats(t *testing.T) {
	cases := []struct {
		name     string
		second   string
		wantDone bool
	}{
		{"equal-repeat", "function", true},      // same supplied type on a later fragment of one slot adds no information and must not conflict.
		{"conflicting-repeat", "custom", false}, // a different later type contradicts the decided one — errored, never replaced.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f1 := model.ToolCallFragment{Position: posI(0), ID: "a", Name: "n", WireType: "function"} // first fragment decides the slot's supplied type.
			f2 := model.ToolCallFragment{ArgumentFragment: "{}", WireType: tc.second}                 // singleton anonymous continuation lands on the same slot carrying the repeat value.

			out, s := assemble(t, context.Background(), testRef,
				deltaStep(toolDelta(f1)),
				deltaStep(toolDelta(f2)),
				deltaStep(toolCallsDelta()),
			)
			assertSingleClose(t, s)

			if tc.wantDone {
				expectStatus(t, out, model.OutputCompleted)
				calls := msgCalls(out)
				testassert.Require(t, len(calls) == 1 && calls[0].ID == "a" && string(calls[0].Arguments) == "{}", "calls = %#v, want one slot-0 call carrying the continuation arguments", calls)
			} else {
				expectStatus(t, out, model.OutputErrored)
			}
		})
	}
}

// TestAssembleMissingToolName pins that a call accumulating an ID but no name is dropped from the finalized calls — and with finish reason tool_calls plus zero surviving valid calls, the output errors for lack of any usable payload.
func TestAssembleMissingToolName(t *testing.T) {
	out, s := assemble(t, context.Background(), testRef,
		deltaStep(toolDelta(model.ToolCallFragment{ID: "a"})),                // ID-only fragment — identity lands but NO name ever arrives at any point in this entire stream's lifetime from opening through termination
		deltaStep(toolDelta(model.ToolCallFragment{ArgumentFragment: "{}"})), // singleton anonymous continuation appends its argument piece onto slot 0's running total per retained rules
		deltaStep(toolCallsDelta()),                                          // trailing finish-reason chunk demanding one or more VALID calls at finalization time per contract — zero survive here because the sole accumulated slot lacks its mandatory half (the name)
	)

	expectStatus(t, out, model.OutputErrored)
	assertSingleClose(t, s)

	// No eligible partial content or usage was ever accumulated by this script.
	testassert.Require(t, out.Message == nil && out.Usage == nil, "errored output unexpectedly carried a message: %#v; errored output unexpectedly carried usage: %#v", out.Message, out.Usage)
}

// TestAssembleMalformedToolArguments pins that argument fragments are concatenated VERBATIM with no JSON validation or decoding anywhere on the assembly path: malformed raw arguments still complete normally when their call carries both identity and name.
func TestAssembleMalformedToolArguments(t *testing.T) {
	out, s := assemble(t, context.Background(), testRef,
		deltaStep(toolDelta(model.ToolCallFragment{ID: "a", Name: "n"})),                  // Identity plus name arrive first.
		deltaStep(toolDelta(model.ToolCallFragment{ArgumentFragment: `{not valid json`})), // Malformed argument bytes append verbatim.
		deltaStep(toolCallsDelta()),
	)

	expectStatus(t, out, model.OutputCompleted) // Malformed arguments do not prevent completion when the call is structurally valid.
	assertSingleClose(t, s)

	calls := msgCalls(out)

	testassert.Require(t, len(calls) == 1 && calls[0].ID == "a" && calls[0].Name == "n", "calls = %#v", calls)

	testassert.Equal(t, string(calls[0].Arguments), `{not valid json`)
}

// TestAssembleIncompleteToolCallErrors pins that any assembled call block left without a complete non-empty identity plus name errors the response when its finish reason demands calls — an incomplete sibling must never be silently dropped just because another call is valid.
func TestAssembleIncompleteToolCallErrors(t *testing.T) {
	out, s := assemble(t, context.Background(), testRef, deltaStep(toolDelta(model.ToolCallFragment{Position: posI(0), ID: "a", Name: "fnA"})), deltaStep(toolDelta(model.ToolCallFragment{Name: "fnB"})), deltaStep(toolCallsDelta()))
	expectStatus(t, out, model.OutputErrored) // The incomplete second block errors the response.
	assertSingleClose(t, s)

	testassert.Require(t, out.Message == nil, "errored output unexpectedly carried a message: %#v", out.Message)
}

// TestAssembleToolExtraAccumulation pins call-scope extras through the ordinary per-position accumulator: values concatenate/replace by kind across fragments landing on one slot, and an Extra error never changes output classification — latest kept, accumulation continues.
func TestAssembleToolExtraAccumulation(t *testing.T) {
	f1 := model.ToolCallFragment{Position: posI(0), ID: "a", Name: "n"} // base call fragment establishing slot zero's identity plus name per standard conventions throughout this same file's other sibling tests
	f1.Extra = model.Extra{"k": json.RawMessage(`"s"`)}                 // seed the call-scope key with a plain string value
	f2 := model.ToolCallFragment{ArgumentFragment: "{}"}                // singleton anonymous continuation carrying NO identity/name fields of its own — it lands on slot zero through the retained last-correlated-call rule
	f2.Extra = model.Extra{"k": json.RawMessage(`"t"`)}                 // second observation for the SAME key — a further plain string piece meant to concatenate with its first half

	out, s := assemble(t, context.Background(), testRef, deltaStep(toolDelta(f1)), deltaStep(toolDelta(f2)), deltaStep(toolCallsDelta()))
	expectStatus(t, out, model.OutputCompleted)
	assertSingleClose(t, s)

	calls := msgCalls(out)
	testassert.Require(t, len(calls) == 1 && calls[0].ID == "a" && string(calls[0].Arguments) == "{}", "calls = %#v", calls)
	// Same-kind string observations concatenate into one JSON string.
	testassert.Require(t, string(calls[0].Extra["k"]) == `"st"`, "call extra k = %s, want \"st\"", calls[0].Extra["k"])
}

// TestAssembleObservedGhostToolSlotsError pins that every normalized tool-call slot observed during correlation is final-state validated: a materialized slot whose fragments carried no id, name, arguments or extras at all must error the response exactly like an unfinished block does — one valid sibling call cannot hide it. Two shapes are pinned: a bare positional claim arriving before any correlated data exists anywhere in this stream (the fragment lands with nothing to continue and still records its normalized slot), and the same empty observation nested inside a multi-fragment event alongside the first real call of the payload.
func TestAssembleObservedGhostToolSlotsError(t *testing.T) { // every observed normalized tool-call slot must end as a complete valid call under this contract; nothing about these shapes is provider-specific
	t.Run("bare-positional-ghost", func(t *testing.T) {
		out, s := assemble(t, context.Background(), testRef, deltaStep(toolDelta(model.ToolCallFragment{Position: posI(5)})), deltaStep(toolDelta(toolFrag("a", "fnA")))) // An anonymous positional claim materializes an empty observed slot.
		expectStatus(t, out, model.OutputErrored)                                                                                                                         // The empty observed slot errors even beside a valid sibling call.
		assertSingleClose(t, s)

		testassert.Require(t, out.Message == nil, "errored output unexpectedly carried a message: %#v", out.Message)
	})

	t.Run("ghost-inside-multi-fragment-event", func(t *testing.T) {
		out, s := assemble(t, context.Background(), testRef, deltaStep(toolDelta(model.ToolCallFragment{Position: posI(5)}, model.ToolCallFragment{ID: "b", Name: "fnB"})), deltaStep(toolCallsDelta())) // The anonymous positional claim resolves to a fresh empty slot.
		expectStatus(t, out, model.OutputErrored)                                                                                                                                                        // The valid sibling cannot rescue the observed empty slot.
		assertSingleClose(t, s)

		testassert.Require(t, out.Message == nil, "errored output unexpectedly carried a message: %#v", out.Message)
	})
}
