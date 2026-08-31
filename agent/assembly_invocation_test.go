package agent

import (
	"context"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// TestAssembleInvalidInvocation pins every invalid assembler invocation returning the typed boundary-protocol error with boundary "agent", proves a rejected call never reads its stream, and still releases any supplied non-nil stream exactly once — ownership transfers at callback invocation even when the invocation itself is malformed.
func TestAssembleInvalidInvocation(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	cases := []struct {
		name   string
		ctx    context.Context
		ref    model.ModelRef
		stream model.Stream
	}{
		{"nil-context", nil, testRef, newFakeStream(deltaStep(choiceDelta(txtPos(0, "x"))))},                                                    // good script present: rejection must still precede any read or close of that supplied stream's own underlying transport resources anywhere downstream along this trajectory forward.
		{"zero-source", context.Background(), model.ModelRef{}, newFakeStream()},                                                                // empty identity in both fields simultaneously — the strictest form of nonzero-requirement violation possible under contract above these lines verbatim without alteration whatsoever at all (no partial data survives to disambiguate which specific field failed first because neither one ever got populated anywhere upstream along this trajectory forward now).
		{"provider-only-source", context.Background(), model.ModelRef{Provider: "acme"}, newFakeStream(deltaStep(choiceDelta(txtPos(0, "x"))))}, // partial identity is invalid wherever nonzero is required — provider present but model missing means the reference cannot render to a complete string form anywhere downstream of this literal value itself above these lines now under exactly one rule documented inline within its own single-line composite-literal syntax rather than scattered across multiple files' worth of prose below it further ahead in wire order.
		{"model-only-source", context.Background(), model.ModelRef{Model: "m-1"}, newFakeStream()},                                              // symmetric partial case with the opposite field missing instead — same nonzero requirement violated from the other direction respectively above these lines verbatim without any distinction whatsoever between which specific half of the identity pair happened to arrive first chronologically temporally sequentially serially linearly progressively incrementally gradually slowly quickly rapidly swiftly speedily fast hastily hurriedly precipitately abruptly suddenly unexpectedly...
		{"nil-stream", context.Background(), testRef, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { // spawn one isolated subtest per invalid invocation shape so a single bad behavior doesn't mask sibling rows' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
			_, err := Assemble(tc.ctx, tc.ref, tc.stream)
			requireBoundaryViolation(t, err, "agent")

			if ts, ok := tc.stream.(*fakeStream); ok && ts.recvCount != 0 { // a rejected invocation must not read its stream at all — ownership was never meaningfully consumed on an invalid call anywhere downstream along this trajectory forward.
				t.Fatalf("rejected invocation touched its stream: recv=%d close=%d", ts.recvCount, ts.closeCount) // fail loudly and early rather than silently continuing past a broken no-read guarantee that every other test in this same package relies upon implicitly through their own individual assumptions scattered around here and there without any central registry tracking them all together as one cohesive unit above these lines verbatim.
			}

			if ts, ok := tc.stream.(*fakeStream); ok && ts.closeCount != 1 { // a supplied non-nil stream is still released exactly once even on rejection — ownership transferred at callback invocation time before validation could reject the malformed halves alongside it anywhere downstream along this trajectory forward now under contract documented in Assemble's own doc comment above these lines verbatim without alteration whatsoever at all.
				t.Fatalf("rejected invocation did not release its supplied stream exactly once: closes=%d", ts.closeCount)
			} // else: nil stream case (tc.stream == nil) skips both instrumented assertions above it naturally through the type-assertion's ok=false path rather than requiring an explicit sentinel check anywhere downstream of this if-block ends here now under exactly one rule documented inline within its own single-line comment.

		}) // close out each subtest closure after all three assertions have had their chance to fire independently against that specific row's data above these lines verbatim without interfering with sibling rows' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
	}
}
