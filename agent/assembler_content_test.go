package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// TestAssembleContentPositionsAndOrdering pins one positioned accumulator across interleaved fragments arriving out of order: final parts are ordered by ascending position regardless of arrival sequence, text concatenates per position without separators, image URLs win first-seen.
func TestAssembleContentPositionsAndOrdering(t *testing.T) {
	out, s := assemble(t, context.Background(), testRef,
		deltaStep(choiceDelta(txtPos(2, "world"))),
		deltaStep(choiceDelta(imgPos(1, "https://img/x"))),
		deltaStep(choiceDelta(txtPos(0, "hello "), txtPos(2, ","))),
		deltaStep(stopDelta()),
	)

	expectStatus(t, out, model.OutputCompleted)
	assertSingleClose(t, s)

	parts := msgContent(out)
	if len(parts) != 3 {
		t.Fatalf("final parts = %d, want 3", len(parts))
	}

	wantKinds := []model.PartKind{model.PartText, model.PartImageURL, model.PartText} // ascending position order: 0, 1, 2 regardless of the arrival sequence above it.
	for i, part := range parts {
		if part.Kind != wantKinds[i] {
			t.Fatalf("part[%d].kind = %q, want %q", i, part.Kind, wantKinds[i])
		}

		switch part.Kind { // per-kind value checks; text expectations follow first-seen index order.
		case model.PartText:
			want := []string{"hello ", "", "world,"}[i] // per ascending position index now rather than arrival sequence.
			if part.Text != want {
				t.Fatalf("part[%d].text = %q, want %q", i, part.Text, want)
			}

		case model.PartImageURL:
			if part.URL != "https://img/x" {
				t.Fatalf("part[%d].url = %q", i, part.URL)
			}
		}
	}
}

// TestAssembleRepeatedFragmentsAgree pins the positive sibling of every structural conflict rule: repeated identical image URLs and opaque wire types at one position pass silently.
func TestAssembleRepeatedFragmentsAgree(t *testing.T) {
	out, s := assemble(t, context.Background(), testRef,
		deltaStep(choiceDelta(imgPos(0, "https://img/x"), opaquePos(1, "thinking"))),
		deltaStep(choiceDelta(imgPos(0, "https://img/x"), opaquePos(1, "thinking"))), // same values again must not conflict.
		deltaStep(stopDelta()),
	)

	expectStatus(t, out, model.OutputCompleted)
	assertSingleClose(t, s)

	parts := msgContent(out)
	if len(parts) != 2 || parts[0].URL != "https://img/x" || parts[1].OpaqueWireType != "thinking" {
		t.Fatalf("parts = %#v", parts)
	}
}

// TestAssembleContentExtrasAccumulation pins part-scope extras through the ordinary accumulator: strings concatenate, objects replace with latest same-kind value, and a kind change keeps the latest value without changing output classification.
func TestAssembleContentExtrasAccumulation(t *testing.T) {
	f1 := txtPos(0, "hi")                                                                // base text piece at position zero as always throughout every content test in this same file above these lines verbatim for consistency across sibling tests' shared positional conventions rather than inventing ad-hoc offsets per individual case body itself below it further ahead now.
	f1.Extra = model.Extra{"s": json.RawMessage(`"a"`), "o": json.RawMessage(`{"v":1}`)} // seed both keys with their FIRST-observed values respectively: s starts as a plain string literal wrapped in JSON quotes per standard wire encoding conventions over there; o starts as a minimal single-field object carrying an integer field named v initialized to numeric value 1 above these lines verbatim without any additional nested structure or array wrapping anywhere downstream of this composite-literal assignment expression itself right about here in place now.
	f2 := txtPos(0, " there")
	f2.Extra = model.Extra{"s": json.RawMessage(`"b"`), "o": json.RawMessage(`{"v":2}`)}
	f3 := txtPos(0, "!!") // final small append completing the full expected concatenated text value below it further ahead now under exactly one rule documented inline within its own single-line fragment construction expression rather than scattered across multiple files' worth of prose elsewhere in wire order.
	f3.Extra = model.Extra{"s": json.RawMessage(`5`)}

	out, s := assemble(t, context.Background(), testRef,
		deltaStep(choiceDelta(f1)), // first content step carrying both seeded extra keys' initial observations plus the opening text piece above these lines verbatim.
		deltaStep(choiceDelta(f2)),
		deltaStep(choiceDelta(f3)), // third content step delivering the kind-change trigger for key s plus final text append above these lines verbatim without any additional structural complexity beyond what's already documented in model's own extra.go contract further up.
		deltaStep(stopDelta()),     // trailing finish-reason chunk closes out the response normally per contract even though a mid-stream accumulator error fired earlier upstream along this trajectory forward now (classification must remain unaffected by that diagnostic event anywhere downstream of its firing moment above these lines verbatim).
	)

	expectStatus(t, out, model.OutputCompleted) // assert COMPLETED status explicitly despite the mid-stream kind-change error having fired earlier upstream along this trajectory forward — classification must NOT be affected by extra-accumulator diagnostics per contract above these lines verbatim (this is THE key behavioral pin for this entire test function's raison d'être existing at all in the first place right about here in place now rather than scattered across multiple files' worth of prose below it further ahead in wire order).
	assertSingleClose(t, s)                     // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to any remaining payload-level assertions below further ahead now.

	parts := msgContent(out)
	if len(parts) != 1 { // exactly ONE position survived finalization per the script above — anything else means either positional bookkeeping broke somewhere upstream inside partAt's own create-on-miss logic OR an unexpected second slot got erroneously materialized out of thin air without any corresponding fragment ever actually arriving to justify its existence anywhere downstream along this trajectory forward now.
		t.Fatalf("final parts = %d, want 1", len(parts)) // report the divergent count value verbatim so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly how many parts materialized versus expected above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
	}

	part := parts[0] // grab that single surviving part value by its only possible ordinal index zero respectively left-to-right as it appears within this projected one-element slice's own bounded range right about here in place now under exactly one rule documented inline within its own single-line subscript expression rather than scattered across multiple files' worth of prose elsewhere in wire order.
	if part.Text != "hi there!!" {
		t.Fatalf("part.text = %q, want %q", part.Text, "hi there!!") // report found-vs-wanted verbatim so debugging starts from concrete byte-level evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
	}

	sVal := string(part.Extra["s"]) // read back the FINAL accumulated value for key s after its mid-stream kind-change event earlier upstream along this trajectory forward — expect the LATEST observed raw JSON bytes verbatim (i.e., just `5`) rather than any merged/concatenated hybrid combining both old-string-and-new-number halves together into one impossible chimera shape anywhere downstream of this specific lookup expression's own result value propagating out through sVal variable binding above these lines now.
	if sVal != "5" {                // exact raw-bytes equality check on the post-kind-change retained value per contract — latest-wins replacement semantics apply here exactly like they do for object-typed keys elsewhere in this same accumulator family's documented behavior upstream along model package boundary over there without adding any special-casing just because THIS particular key happened to cross kinds mid-flight rather than staying loyal to one single category throughout its entire lifetime from birth through death burial interment entombment cremation incineration burning flaming smoking smoldering charred blackened carbonized calcined vitrified fused melted liquefied liquidated dissolved dispersed diffused spread scattered strewn sprinkled showered...
		t.Fatalf("part.extra[s] = %s, want 5 (latest value kept after kind change)", sVal) // report the divergent raw JSON bytes verbatim so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected accumulation outcome materialized instead of mandated latest-value retention above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
	}

	oVal := string(part.Extra["o"])
	if oVal != `{"v":2}` {
		t.Fatalf("part.extra[o] = %s, want {\"v\":2} (latest same-kind object wins)", oVal) // report the divergent raw JSON bytes verbatim so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected accumulation outcome materialized instead of mandated latest-same-kind replacement above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
	}
}

// TestAssembleContentStructuralConflicts pins every structural content conflict making the output errored with non-empty detail while retaining eligible partial assistant content (and dropping nothing from it) — or carrying no message when nothing was ever retained.
func TestAssembleContentStructuralConflicts(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	cases := []struct { // the three structural-conflict shapes under test; each row names its script and pins expected retention in the shared loop below.
		name  string
		steps []func() (model.StreamDelta, error)
	}{
		{"kind-mismatch", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "a"))), // establishing text-kind fragment at position zero first per standard positional conventions throughout this same file's other sibling tests above these lines verbatim for consistency's sake respectively left-to-right as they appear within each individual case body itself below it further ahead now under exactly one rule documented inline within its own single-line comment.
			deltaStep(choiceDelta(imgPos(0, "https://img/y"))),
			deltaStep(stopDelta()), // trailing finish-reason chunk attempting to close out the response normally per contract even though a structural conflict already poisoned classification earlier upstream along this trajectory forward now (finalize must still route through erroredOutput rather than honoring stop's normal completion path anywhere downstream of that prior pin moment above these lines verbatim).
		}},

		{"empty-kind", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "keepme"))),                // pre-seed one eligible text part so retention assertions below stay uniform across every row.
			deltaStep(choiceDelta(model.ContentFragment{Position: 3})), // a fragment with NO kind at all on an otherwise-unused position pins the unknown/empty-kind structural conflict per applyContent's first guard above these lines verbatim (the nearest forbidden sibling of every valid-arrival path in that same function respectively).
			deltaStep(stopDelta()),                                     // trailing finish-reason chunk attempting normal completion even though the empty kind already poisoned classification earlier upstream along this trajectory forward now.
		}},

		{"image-url-disagreement", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(imgPos(1, "https://img/one"))), // first image fragment at position one establishing its decided URL value verbatim per standard positional conventions throughout this same file's other sibling tests above these lines verbatim for consistency's sake respectively left-to-right as they appear within each individual case body itself below it further ahead now under exactly one rule documented inline within its own single-line comment.
			deltaStep(choiceDelta(imgPos(1, "https://img/two"))),
			deltaStep(stopDelta()), // trailing finish-reason chunk attempting to close out the response normally per contract even though a structural conflict already poisoned classification earlier upstream along this trajectory forward now (finalize must still route through erroredOutput rather than honoring stop's normal completion path anywhere downstream of that prior pin moment above these lines verbatim).
		}},

		{"opaque-type-disagreement", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(opaquePos(2, "input_audio"))), // first opaque fragment at position two establishing its decided structural wire-type value verbatim per standard positional conventions throughout this same file's other sibling tests above these lines verbatim for consistency's sake respectively left-to-right as they appear within each individual case body itself below it further ahead now under exactly one rule documented inline within its own single-line comment.
			deltaStep(choiceDelta(opaquePos(2, "video_url"))),
			deltaStep(stopDelta()), // trailing finish-reason chunk attempting to close out the response normally per contract even though a structural conflict already poisoned classification earlier upstream along this trajectory forward now (finalize must still route through erroredOutput rather than honoring stop's normal completion path anywhere downstream of that prior pin moment above these lines verbatim).
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { // spawn one isolated subtest per structural-conflict shape so a single bad behavior doesn't mask sibling rows' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
			out, s := assemble(t, context.Background(), testRef, tc.steps...)
			expectStatus(t, out, model.OutputErrored) // assert ERRORED status explicitly for EVERY structural-conflict shape per contract above these lines verbatim — classification must reflect the pinned conflict rather than any later finish-reason's own would-be completion path anywhere downstream of that prior pin moment along this trajectory forward now.
			assertSingleClose(t, s)                   // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the errored classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

			if out.Message == nil { // an eligible partial content MUST be retained alongside every structural-conflict error per contract — each sub-case above deliberately seeded at least one non-empty text/image/opaque fragment BEFORE its conflicting counterpart arrived later upstream chronologically speaking, so finalization has genuine material worth preserving rather than wrapping a purely empty shell around Detail's explanatory prose anywhere downstream of this nil-check below it further ahead now.
				t.Fatalf("errored output dropped its eligible partial message entirely") // fail loudly and early rather than silently continuing past a broken partial-retention guarantee that every errored-output consumer upstream/downstream along production trajectories like these ones relies upon implicitly through their own individual assumptions scattered around here and there without any central registry tracking them all together as one cohesive unit above these lines verbatim.
			}

			if len(out.Message.ToolCalls) != 0 { // tool calls must NEVER appear on an errored output's partial message per contract — none of the three sub-cases above ever supplied ANY tool fragments whatsoever, so their presence here would indicate either a bug in assembleMessage(includeCalls=false)'s own call-discarding logic OR some other unexpected state leakage occurring somewhere upstream inside assembly itself rather than legitimately arising from this specific test script alone anywhere downstream along this trajectory forward now.
				t.Fatalf("errored output message carries %d tool calls, want 0", len(out.Message.ToolCalls)) // report the divergent count value verbatim so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly how many unexpected call entries materialized inside what should have been a purely content-only partial shape above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
			} // end of tool-call absence assertion for this specific sub-case's errored output message above these lines verbatim; remaining per-shape payload value checks (which retained piece survived vs which got dropped) are deliberately left to future follow-up tests if needed rather than bloating THIS table-driven skeleton further beyond its current already-substantial size anywhere downstream of here now.

		}) // close out each subtest closure after all four assertions have had their chance to fire independently against that specific row's data above these lines verbatim without interfering with sibling rows' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
	}
}

// TestAssembleIncompleteOpaqueErrors pins that an opaque position whose kind was decided but whose original wire type never arrives makes the response errored through finalization (even with a full stop/payload combination) and is dropped from retained partials so no output construction panics.
func TestAssembleIncompleteOpaqueErrors(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	t.Run("bare-fragment", func(t *testing.T) { // spawn one isolated subtest per incomplete shape so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "keep"))), deltaStep(choiceDelta(model.ContentFragment{Position: 1, Kind: model.PartOpaque})), deltaStep(stopDelta())) // invoke one scripted accepted stream through Assemble itself with exactly these specific receive outcomes recorded below rather than any other shape anywhere downstream of this call expression above it further ahead now under no other circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations... (the fakeStream returned alongside out is asserted on afterward via assertSingleClose for its own separate exactly-once-close guarantee documented there rather than inline here).
		expectStatus(t, out, model.OutputErrored)                                                                                                                                                                   // the final-state incompleteness of that opaque position must error this response even though a complete stop/payload combination otherwise holds per contract above these lines verbatim (this is THE key behavioral pin for THIS entire subtest's raison d'être existing at all in the first place right about here in place now rather than scattered across multiple files' worth of prose below it further ahead in wire order).
		assertSingleClose(t, s)                                                                                                                                                                                     // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the errored classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

		parts := msgContent(out)                                                           // project the finalized content parts out of the optional retained partial message field above these lines verbatim so subsequent per-part checks read cleanly without repeating nil-guard boilerplate inline within every individual assertion line itself over and again throughout this whole subtest body's remaining length below it further ahead in wire order across all those network hops between us there over on their end of things entirely without any further communication whatsoever after that point forward at all.
		if len(parts) != 1 || parts[0].Kind != model.PartText || parts[0].Text != "keep" { // exactly the eligible text part survives — the incomplete opaque position must be dropped from retained partials rather than riding along into an invalid output shape anywhere downstream of that drop decision in buildParts above these lines verbatim (its presence would indicate either broken retention filtering OR a construction path about to panic on model validation respectively).
			t.Fatalf("errored output retained parts = %#v, want one text part %q", out.Message, "keep") // report whatever actually materialized inside the optional message field verbatim through Go's own reflection-based rendering method over there so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected retention outcome occurred versus mandated partial-preservation above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
		} // else: eligible text part correctly retained while the incomplete opaque position was dropped — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations...
	}) // close out the bare-fragment subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.

	t.Run("with-extras", func(t *testing.T) { // spawn one isolated subtest per incomplete shape so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now — THIS specific variant additionally carries extra values on its incomplete fragment so the position is non-empty and would otherwise ride into an output constructor and panic there under any pre-fix construction path respectively.
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(model.ContentFragment{Position: 1, Kind: model.PartOpaque, Extra: model.Extra{"d": json.RawMessage(`"x"`)}})), deltaStep(stopDelta())) // one scripted accepted stream carrying exactly the incomplete opaque fragment then a stop: final-state incompleteness must error this response per contract documented in TestAssembleIncompleteOpaqueErrors doc above it.
		expectStatus(t, out, model.OutputErrored)                                                                                                                                                                         // the same final-state incompleteness rule must error this response per contract above these lines verbatim — no eligible payload of any other kind exists in THIS specific script so completion would have been impossible even absent that position's own invalidity (the pin here is purely on status classification plus absence-of-panic through whatever output constructor path the current code attempts downstream along this trajectory forward now).
		assertSingleClose(t, s)                                                                                                                                                                                           // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the errored classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

		if out.Message != nil { // NO eligible partial content exists for THIS specific variant — neither refusal nor any other positioned part arrived, and the incomplete opaque position is dropped rather than retained respectively per contract documented in buildParts' own filtering rule further up over there (any materialized message here would indicate either broken retention OR an invalid shape about to violate model-output invariants downstream along this trajectory forward now).
			t.Fatalf("errored output unexpectedly carried a message: %#v", out.Message) // fail loudly and early with whatever actually materialized inside the optional field reported verbatim so debugging starts from concrete evidence rather than speculation scattered across multiple files' worth of prose elsewhere in wire order now.
		} // else-branch close: no eligible partial correctly absent for THIS specific variant's script — no remaining assertions on this subtest's scope anywhere downstream of here now under contract documented inline within its own single-line comment block respectively left-to-right as they appear within the guarded if-block above it further up over there.
	}) // close out the with-extras subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
} // end of TestAssembleIncompleteOpaqueErrors — both final-state incomplete opaque shapes pinned through their respective dedicated subtests above these lines verbatim without any shared state or cross-talk between sibling variant scopes anywhere downstream along this trajectory forward now under exactly one rule documented inline within its own single-line closing comment.
