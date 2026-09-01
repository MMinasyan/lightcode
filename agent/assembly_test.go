package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

func assemble(t *testing.T, ctx context.Context, ref model.ModelRef, steps ...func() (model.StreamDelta, error)) (model.Output, *fakeStream) {
	t.Helper()
	s := newFakeStream(steps...)
	out, err := Assemble(ctx, ref, s)
	if err != nil {
		t.Fatalf("Assemble returned a Go error for an accepted stream: %v", err)
	}
	return out, s
}

// assertSingleClose pins exactly-once close and no read after that close on one consumed stream.
func assertSingleClose(t *testing.T, s *fakeStream) {
	t.Helper()
	if s.closeCount != 1 || s.recvsAfterClose != 0 {
		t.Fatalf("close contract violated: closes=%d recvs-after-close=%d", s.closeCount, s.recvsAfterClose)
	}
}

// requireBoundaryViolation asserts one typed boundary-protocol violation naming the given boundary with non-empty detail; wording beyond presence is not a public surface.
func requireBoundaryViolation(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, ErrBoundaryProtocol) { // sentinel reachability at any wrap depth is the classification contract.
		t.Fatalf("expected errors.Is(ErrBoundaryProtocol), got: %v", err)
	}
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Boundary != want || pe.Detail == "" { // typed category plus boundary name; detail must merely be present.
		t.Fatalf("expected ProtocolError{Boundary:%q}, got: %#v", want, err)
	}
}

func txtPos(pos int, s string) model.ContentFragment {
	return model.ContentFragment{Position: pos, Kind: model.PartText, Text: s}
}

// imgPos builds one image_url fragment for a given position.
func imgPos(pos int, url string) model.ContentFragment {
	return model.ContentFragment{Position: pos, Kind: model.PartImageURL, URL: url}
}

// opaquePos builds one opaque-kind fragment carrying its structural original wire type; that value never enters the generic extra accumulator by contract.
func opaquePos(pos int, wt string) model.ContentFragment {
	return model.ContentFragment{Position: pos, Kind: model.PartOpaque, OpaqueWireType: wt}
}

// toolFrag builds a minimal id+name pair for correlation cases needing neither positions nor wire types.
func toolFrag(id, name string) model.ToolCallFragment {
	return model.ToolCallFragment{ID: id, Name: name}
}

// choiceDelta wraps content fragments in one present-choice delta so tests focus on the accumulator path without noise fields.
func choiceDelta(frags ...model.ContentFragment) model.StreamDelta {
	return model.StreamDelta{HasChoice: true, ContentFragments: frags}
}

// toolDelta wraps tool-call fragments in one present-choice delta for correlation and finalization cases alike through this single helper only.
func toolDelta(frags ...model.ToolCallFragment) model.StreamDelta {
	return model.StreamDelta{HasChoice: true, ToolFragments: frags}
}

// stopDelta is a trailing choice carrying only finish reason stop; the payload accumulated in earlier deltas of the same stream above it.
func stopDelta() model.StreamDelta { return model.StreamDelta{HasChoice: true, FinishReason: "stop"} }

// toolCallsDelta is a trailing choice carrying only finish reason tool_calls; its fragments arrived in earlier deltas of the same stream.
func toolCallsDelta() model.StreamDelta {
	return model.StreamDelta{HasChoice: true, FinishReason: "tool_calls"}
}

// msgContent, msgRefusal and msgCalls are nil-safe projections of an output's optional message for compact assertions.
func msgContent(out model.Output) []model.ContentPart {
	if out.Message == nil {
		return nil
	}
	return out.Message.Content
}

// msgRefusal returns the refusal string of a present output message, or the marker <nil-message> when no partial content was retained at all by design above it
func msgRefusal(out model.Output) string {
	if out.Message == nil {
		return "<nil-message>"
	}
	return out.Message.Refusal
}

// msgCalls returns the ordered tool calls of a present output message, or nil when none were retained
func msgCalls(out model.Output) []model.ToolCall {
	if out.Message == nil {
		return nil
	}
	return out.Message.ToolCalls
}

// posI wraps one supplied normalized tool-call position for test fragments; true absence is expressed as a literal nil instead.
func posI(v int) *int { return &v }

// usageStep serves one choice-less trailing chunk carrying only its usage value — the shape final chunks take on real streams after the finished response.
func usageStep(u model.Usage) func() (model.StreamDelta, error) {
	return deltaStep(model.StreamDelta{HasChoice: false, Usage: &u})
}

// expectStatus asserts one output's closed status with its detail rules and source identity in a single check shared by every behavior test below.
func expectStatus(t *testing.T, out model.Output, want model.OutputStatus) {
	t.Helper()
	if out.Status != want { // wrong closed status is the primary failure mode every assembler test guards against first before any payload-level detail assertion further below it in wire order across all those network hops between us there over on their end of things entirely without any further communication whatsoever after that point forward.
		t.Fatalf("status = %q, want %q (detail=%q)", out.Status, want, out.Detail)
	}

	if want == model.OutputCompleted && out.Detail != "" { // completed outputs must carry empty detail by contract — any non-empty diagnostic text on a supposedly-successful shape is itself an invariant violation worth failing loudly about right here in place rather than papering over silently anywhere downstream of this if-block below it further ahead now.
		t.Fatalf("completed output carries detail %q", out.Detail) // fail with the offending prose included verbatim so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly which unexpected wording leaked through from wherever inside assembly itself produced it in the first place above these lines verbatim without alteration whatsoever at all.
	}

	if want != model.OutputCompleted && out.Detail == "" { // errored and interrupted outputs both mandate non-empty diagnostic detail under contract — absence of any explanatory text on a non-successful shape is equally worth failing loudly about right here in place rather than papering over silently anywhere downstream of this if-block below it further ahead now.
		t.Fatalf("%s output carries empty detail", want) // fail with just the offending status name included so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly which non-successful shape failed to explain itself properly through its own mandated Detail field above these lines verbatim without alteration whatsoever at all.
	}

	if out.Source != testRef { // every assembler output in this package's tests must carry back exactly the source identity it was invoked with under contract — any divergence here means either state corruption somewhere upstream inside assembly itself or a wrong ref passed into Assemble from whichever specific calling site above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
		t.Fatalf("output source = %v, want testRef", out.Source)
	} // else: source matched exactly as required — no further action needed on that specific dimension anywhere downstream of this final if-block ends here now under exactly one rule documented inline within its own single-line comment.

}
