package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

func TestAssembleRejectsInvalidStreamDeltas(t *testing.T) {
	malformed := json.RawMessage(`{`)
	cases := []struct {
		name  string
		delta model.StreamDelta
	}{
		{"negative-content-position", choiceDelta(model.ContentFragment{Position: -1, Kind: model.PartText, Text: "x"})},
		{"negative-tool-position", toolDelta(model.ToolCallFragment{Position: posI(-1), ID: "a", Name: "n"})},
		{"cross-kind-content-field", choiceDelta(model.ContentFragment{Kind: model.PartText, Text: "x", URL: "https://example.com"})},
		{"malformed-content-extra", choiceDelta(model.ContentFragment{Kind: model.PartText, Text: "x", Extra: model.Extra{"k": malformed}})},
		{"malformed-tool-extra", toolDelta(model.ToolCallFragment{ID: "a", Name: "n", Extra: model.Extra{"k": malformed}})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, stream := assemble(t, context.Background(), testRef, deltaStep(tc.delta))
			expectStatus(t, out, model.OutputErrored)
			assertSingleClose(t, stream)
		})
	}
}

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
	testassert.Equal(t, len(parts), 3)

	wantKinds := []model.PartKind{model.PartText, model.PartImageURL, model.PartText} // ascending position order: 0, 1, 2 regardless of the arrival sequence above it.
	for i, part := range parts {
		testassert.Require(t, part.Kind == wantKinds[i], "part[%d].kind = %q, want %q", i, part.Kind, wantKinds[i])

		switch part.Kind { // per-kind value checks; text expectations follow first-seen index order.
		case model.PartText:
			want := []string{"hello ", "", "world,"}[i] // arrival-independent expectation
			testassert.Require(t, part.Text == want, "part[%d].text = %q, want %q", i, part.Text, want)

		case model.PartImageURL:
			testassert.Require(t, part.URL == "https://img/x", "part[%d].url = %q", i, part.URL)
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
	testassert.Require(t, len(parts) == 2 && parts[0].URL == "https://img/x" && parts[1].OpaqueWireType == "thinking", "parts = %#v", parts)
}

// TestAssembleRepeatedOpaqueExtrasAccumulate pins the shared-accumulator rule for repeated opaque fragments: fragments at one position with the same non-empty structural wire type keep flowing through the ordinary Extra accumulator (strings concatenate, objects replace with latest), and only a different structural type records a conflict.
func TestAssembleRepeatedOpaqueExtrasAccumulate(t *testing.T) {
	f1 := opaquePos(0, "thinking")
	f1.Extra = model.Extra{"d": json.RawMessage(`"a"`), "o": json.RawMessage(`{"v":1}`)}
	f2 := opaquePos(0, "thinking") // identical repeated structural type adds nothing to check and must not short-circuit the fragment's extras below it.
	f2.Extra = model.Extra{"d": json.RawMessage(`"b"`), "o": json.RawMessage(`{"v":2}`)}

	out, s := assemble(t, context.Background(), testRef,
		deltaStep(choiceDelta(f1)),
		deltaStep(choiceDelta(f2)),
		deltaStep(stopDelta()),
	)

	expectStatus(t, out, model.OutputCompleted)
	assertSingleClose(t, s)

	parts := msgContent(out)
	testassert.Require(t, len(parts) == 1 && parts[0].Kind == model.PartOpaque && parts[0].OpaqueWireType == "thinking", "parts = %#v, want one opaque part with wire type thinking", parts)

	{
		got := string(parts[0].Extra["d"])
		testassert.Equal(t, got, `"ab"`)
	}

	{
		got := string(parts[0].Extra["o"])
		testassert.Equal(t, got, `{"v":2}`)
	}
}

// TestAssembleContentExtrasAccumulation pins part-scope extras through the ordinary accumulator: strings concatenate, objects replace with latest same-kind value, and a kind change keeps the latest value without changing output classification.
func TestAssembleContentExtrasAccumulation(t *testing.T) {
	f1 := txtPos(0, "hi")
	f1.Extra = model.Extra{"s": json.RawMessage(`"a"`), "o": json.RawMessage(`{"v":1}`)}
	f2 := txtPos(0, " there")
	f2.Extra = model.Extra{"s": json.RawMessage(`"b"`), "o": json.RawMessage(`{"v":2}`)}
	f3 := txtPos(0, "!!")
	f3.Extra = model.Extra{"s": json.RawMessage(`5`)}

	out, s := assemble(t, context.Background(), testRef,
		deltaStep(choiceDelta(f1)),
		deltaStep(choiceDelta(f2)),
		deltaStep(choiceDelta(f3)),
		deltaStep(stopDelta()), // a finish reason after a kind-change diagnostic must not change classification.
	)

	expectStatus(t, out, model.OutputCompleted)
	assertSingleClose(t, s)

	parts := msgContent(out)
	testassert.Equal(t, len(parts), 1)

	part := parts[0]
	testassert.Equal(t, part.Text, "hi there!!")
	// A key that changes JSON kind retains its latest value.
	testassert.Equal(t, string(part.Extra["s"]), "5")

	testassert.Equal(t, string(part.Extra["o"]), `{"v":2}`)
}

// TestAssembleMessageExtrasKindChangeKeepsLatest pins the message-scope sibling of the part-scope extras rule: a key changing JSON kind stores the latest value through the ordinary accumulator, its error is never surfaced, and the output classification stays untouched.
func TestAssembleMessageExtrasKindChangeKeepsLatest(t *testing.T) {
	d1 := model.StreamDelta{HasChoice: true, MessageExtra: model.Extra{"s": json.RawMessage(`"a"`)}}
	d2 := model.StreamDelta{HasChoice: true, MessageExtra: model.Extra{"s": json.RawMessage(`5`)}} // string→number kind change: latest value wins as the new baseline.

	out, s := assemble(t, context.Background(), testRef,
		deltaStep(d1),
		deltaStep(d2),
		deltaStep(stopDelta()),
	)

	expectStatus(t, out, model.OutputCompleted) // an Extra accumulator diagnostic never changes output classification at message scope either.
	assertSingleClose(t, s)

	testassert.Require(t, out.Message != nil, "completed output carries no message at all")

	{
		got := string(out.Message.Extra["s"])
		testassert.Equal(t, got, "5")
	}
}

// TestAssembleUnknownKindFragmentsAreRejectedAtAdmission pins the closed-kind admission gate at the top of the one positioned accumulator path: any kind outside text, image_url and opaque — empty or not — pins one structural conflict and stores nothing on its position, neither kind fields nor extras, while the nearest known-kind sibling with the same extras shape still completes.
func TestAssembleUnknownKindFragmentsAreRejectedAtAdmission(t *testing.T) {
	unknownWithExtras := model.ContentFragment{Position: 1, Kind: "audio", Extra: model.Extra{"d": json.RawMessage(`"x"`)}}
	unknownBare := model.ContentFragment{Position: 1, Kind: "audio"}
	knownWithExtras := model.ContentFragment{Position: 1, Kind: model.PartOpaque, OpaqueWireType: "thinking", Extra: model.Extra{"d": json.RawMessage(`"x"`)}}

	cases := []struct {
		name      string
		steps     []func() (model.StreamDelta, error)
		wantDone  bool
		wantTexts []string // eligible retained text parts on an errored output; nil means nothing was retained at all.
	}{
		{"unknown-kind-with-extras", []func() (model.StreamDelta, error){ // the invalid fragment is the stream's only data: classification must error through the shared conflict route — never panic through the output constructor — and the position must hold nothing.
			deltaStep(choiceDelta(unknownWithExtras)),
			deltaStep(stopDelta()),
		}, false, nil},

		{"unknown-kind-alongside-valid-text", []func() (model.StreamDelta, error){ // a bare unknown-kind fragment beside a fully valid text part must still error while retention keeps only the eligible valid text.
			deltaStep(choiceDelta(txtPos(0, "hi"))),
			deltaStep(choiceDelta(unknownBare)),
			deltaStep(stopDelta()),
		}, false, []string{"hi"}},

		{"known-kind-with-extras-sibling", []func() (model.StreamDelta, error){ // nearest forbidden-sibling counter-check: the same fragment shape with a known kind keeps accumulating and completing normally.
			deltaStep(choiceDelta(knownWithExtras)),
			deltaStep(stopDelta()),
		}, true, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, s := assemble(t, context.Background(), testRef, tc.steps...)
			assertSingleClose(t, s)

			if tc.wantDone {
				expectStatus(t, out, model.OutputCompleted)
				parts := msgContent(out)
				testassert.Require(t, len(parts) == 1 && parts[0].Kind == model.PartOpaque && parts[0].OpaqueWireType == "thinking" && string(parts[0].Extra["d"]) == `"x"`, "completed parts = %#v, want the known-kind part with its stored extras", parts)
				return
			}

			expectStatus(t, out, model.OutputErrored)
			if tc.wantTexts == nil { // invalid-only stream: nothing may have been stored, so no message at all may ride the errored output.
				testassert.Require(t, out.Message == nil, "errored output unexpectedly carried a message: %#v", out.Message)
				return
			}

			parts := msgContent(out)
			testassert.Require(t, len(parts) == len(tc.wantTexts), "errored retained parts = %#v, want only the eligible valid text", out.Message)
			for i, want := range tc.wantTexts {
				testassert.Require(t, parts[i].Kind == model.PartText && parts[i].Text == want, "retained part[%d] = %#v, want text %q", i, parts[i], want)
			}
		})
	}
}

// TestAssembleContentStructuralConflicts pins every structural content conflict making the output errored with non-empty detail while retaining eligible partial assistant content (and dropping nothing from it) — or carrying no message when nothing was ever retained.
func TestAssembleContentStructuralConflicts(t *testing.T) {
	cases := []struct { // the three structural-conflict shapes under test; each row names its script and pins expected retention in the shared loop below.
		name  string
		steps []func() (model.StreamDelta, error)
	}{
		{"kind-mismatch", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "a"))), // Text kind establishes position zero.
			deltaStep(choiceDelta(imgPos(0, "https://img/y"))),
			deltaStep(stopDelta()), // trailing finish-reason chunk attempting to close out the response normally per contract even though a structural conflict already poisoned classification
		}},

		{"empty-kind", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "keepme"))),                // pre-seed one eligible text part so retention assertions below stay uniform across every row.
			deltaStep(choiceDelta(model.ContentFragment{Position: 3})), // a fragment with NO kind at all on an otherwise-unused position pins the unknown/empty-kind structural conflict per applyContent's first guard
			deltaStep(stopDelta()),                                     // trailing finish-reason chunk attempting normal completion even though the empty kind already poisoned classification
		}},

		{"image-url-disagreement", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(imgPos(1, "https://img/one"))), // First URL decides the position.
			deltaStep(choiceDelta(imgPos(1, "https://img/two"))),
			deltaStep(stopDelta()), // trailing finish-reason chunk attempting to close out the response normally per contract even though a structural conflict already poisoned classification
		}},

		{"opaque-type-disagreement", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(opaquePos(2, "input_audio"))), // First wire type decides the position.
			deltaStep(choiceDelta(opaquePos(2, "video_url"))),
			deltaStep(stopDelta()), // trailing finish-reason chunk attempting to close out the response normally per contract even though a structural conflict already poisoned classification
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, s := assemble(t, context.Background(), testRef, tc.steps...)
			expectStatus(t, out, model.OutputErrored)
			assertSingleClose(t, s)

			// Eligible partial content is retained alongside every structural-conflict error.
			testassert.Require(t, out.Message != nil, "errored output dropped its eligible partial message entirely")
			// Tool calls never ride an errored output's partial message.
			testassert.Equal(t, len(out.Message.ToolCalls), 0)
		})
	}
}

// TestAssembleIncompleteOpaqueErrors pins that an opaque position whose kind was decided but whose original wire type never arrives makes the response errored through finalization (even with a full stop/payload combination) and is dropped from retained partials so no output construction panics.
func TestAssembleIncompleteOpaqueErrors(t *testing.T) {
	t.Run("bare-fragment", func(t *testing.T) {
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "keep"))), deltaStep(choiceDelta(model.ContentFragment{Position: 1, Kind: model.PartOpaque})), deltaStep(stopDelta()))
		expectStatus(t, out, model.OutputErrored) // Incomplete final-state opaque data errors despite the stop/payload pair.
		assertSingleClose(t, s)

		parts := msgContent(out)
		// Only the eligible text part survives; the incomplete opaque position is dropped.
		testassert.Require(t, len(parts) == 1 && parts[0].Kind == model.PartText && parts[0].Text == "keep", "errored output retained parts = %#v, want one text part %q", out.Message, "keep")
	})

	t.Run("with-extras", func(t *testing.T) {
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(model.ContentFragment{Position: 1, Kind: model.PartOpaque, Extra: model.Extra{"d": json.RawMessage(`"x"`)}})), deltaStep(stopDelta()))
		expectStatus(t, out, model.OutputErrored) // Incomplete opaque data errors even when it carries extras.
		assertSingleClose(t, s)

		// No eligible partial content exists, so no message may ride the errored output.
		testassert.Require(t, out.Message == nil, "errored output unexpectedly carried a message: %#v", out.Message)
	})
}
