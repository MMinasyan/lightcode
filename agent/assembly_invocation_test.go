package agent

import (
	"context"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// TestAssembleInvalidInvocation pins every invalid assembler invocation returning the typed boundary-protocol error with boundary "agent", proves a rejected call never reads its stream, and still releases any supplied non-nil stream exactly once — ownership transfers at callback invocation even when the invocation itself is malformed.
func TestAssembleInvalidInvocation(t *testing.T) {
	cases := []struct {
		name   string
		ctx    context.Context
		ref    model.ModelRef
		stream model.Stream
	}{
		{"nil-context", nil, testRef, newFakeStream(deltaStep(choiceDelta(txtPos(0, "x"))))},                                                    // good script present: rejection must still precede any read or close of that supplied stream's own underlying transport resources
		{"zero-source", context.Background(), model.ModelRef{}, newFakeStream()},                                                                // empty identity in both fields simultaneously — the strictest form of nonzero-requirement violation possible
		{"provider-only-source", context.Background(), model.ModelRef{Provider: "acme"}, newFakeStream(deltaStep(choiceDelta(txtPos(0, "x"))))}, // partial identity is invalid wherever nonzero is required — provider present but model missing means the reference cannot render to a complete string form
		{"model-only-source", context.Background(), model.ModelRef{Model: "m-1"}, newFakeStream()},                                              // symmetric partial case with the opposite field missing.
		{"nil-stream", context.Background(), testRef, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Assemble(tc.ctx, tc.ref, tc.stream)
			requireBoundaryViolation(t, err, "agent")

			if ts, ok := tc.stream.(*fakeStream); ok && ts.recvCount != 0 { // a rejected invocation must not read its stream at all — ownership was never meaningfully consumed on an invalid call
				t.Fatalf("rejected invocation touched its stream: recv=%d close=%d", ts.recvCount, ts.closeCount)
			}

			if ts, ok := tc.stream.(*fakeStream); ok && ts.closeCount != 1 { // a supplied non-nil stream is still released exactly once even on rejection — ownership transferred at callback invocation time before validation could reject the malformed halves alongside it
				t.Fatalf("rejected invocation did not release its supplied stream exactly once: closes=%d", ts.closeCount)
			}

		})
	}
}
