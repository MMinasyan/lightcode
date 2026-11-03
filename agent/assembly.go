package agent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/MMinasyan/lightcode/model"
)

func Assemble(ctx context.Context, source model.ModelRef, stream model.Stream) (model.Output, error) {
	if ctx == nil || !nonzeroSource(source) || stream == nil { // reject invalid invocations before touching the stream; ownership of a supplied non-nil stream still transfers here, so release it once.
		err := newBoundaryViolation("agent", "assembler requires a non-nil run context, nonzero source identity and accepted stream")
		if stream != nil {
			_ = stream.Close() // cleanup-only: never replaces an output (there is none) or the violation itself.
		}
		return model.Output{}, err
	}

	st := newAssemblyState(source)
	defer func() { _ = stream.Close() }() // exactly-once close on every return path below; a post-consumption close error is cleanup-only and never replaces the output.
	return st.assemble(ctx, stream), nil
}

func nonzeroSource(r model.ModelRef) bool { return r.Provider != "" && r.Model != "" }

type assemblyState struct {
	source         model.ModelRef // complete identity for every finalized output of this stream.
	refusal        string         // concatenated refusal fragments in arrival order; empty means absent at finalization time.
	role           string         // first supplied non-empty raw role; later values must agree with it or the conflict is pinned.
	finishReason   string         // first accepted non-empty finish reason, kept verbatim for classification.
	conflictDetail string         // set once, in wire order: whichever structural violation fired earliest decides any errored detail.
	established    bool           // a successful explicit finish/payload combination was observed; read errors and cancellation after it no longer downgrade completion (semantic conflicts still do).
	usage          *model.Usage   // last reported usage wins by value replacement per arrival order; nil means unknown/not reported.
	msgExtra       *model.ExtraAccumulator
	content        map[int]*partAcc // one positioned content accumulator keyed by fragment position, first-seen order below.
	contentOrder   []int            // positions in first-seen order so final parts stay ordered deterministically regardless of map iteration randomness.
	toolDeltas     map[int]*callAcc // normalized-position-keyed call accumulators under the retained correlation rules.
	toolIDs        map[string]int   // non-empty call ID to its single normalized position; a conflicting mapping is an errored conflict.
	nextToolIdx    int              // lowest free slot cursor for new calls, advancing past claimed slots only.
	lastToolIdx    int              // last correlated normalized position (-1 until the first tool fragment).
}

func newAssemblyState(source model.ModelRef) *assemblyState {
	return &assemblyState{source: source, msgExtra: model.NewExtraAccumulator(), lastToolIdx: -1}
}

func (st *assemblyState) assemble(ctx context.Context, stream model.Stream) model.Output {
	var readErr error // nil means clean termination through EOF; set exactly once on an accepted-stream read failure.
	for {
		delta, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break // normal termination ([DONE] or raw body EOF); processed before checking cancellation.
		}
		if err != nil { // non-EOF read error: no further reads happen; finalization decides interrupted vs errored from the run context at this instant.
			readErr = err
			break
		}
		owned, verr := model.NewStreamDelta(delta) // Revalidate and own direct stream implementations at the Agent boundary.
		if verr != nil {
			st.noteConflict("received stream delta rejected: %v", verr)
			continue
		}
		st.applyDelta(owned)
	}

	return st.finalize(ctx, readErr)
}

func (st *assemblyState) applyDelta(d model.StreamDelta) {
	if d.Usage != nil { // last reported usage wins by value replacement; parser normalization already produced final signed-int counts upstream of this point.
		u := *d.Usage
		st.usage = &u
	}

	if !d.HasChoice { // choice-less deltas carry only trailing usage (e.g. a final chunk after the finished response).
		return
	}

	if d.Role != "" { // raw wire role carried through unnormalized; first value decides, later values must agree with it or the conflict is pinned in wire order.
		st.noteRole(d.Role)
	}

	if fr := d.FinishReason; fr != "" { // at most one non-empty finish reason is accepted: identical repeats pass silently, any different value pins a conflict even when completion was already established earlier.
		switch {
		case st.finishReason == "":
			st.finishReason = fr
		case fr != st.finishReason:
			st.noteConflict("conflicting finish reasons %q then %q", st.finishReason, fr)
		}
	}

	st.refusal += d.RefusalFragment // refusal fragments concatenate in arrival order without separators.

	for key, value := range d.MessageExtra { // message-scope extras through the ordinary five-kind accumulator; an Extra error never changes output classification — latest kept, accumulation continues.
		if err := st.msgExtra.Add(key, value); err != nil {
			_ = err
		}
	}

	for _, frag := range d.ContentFragments { // string-form and array-form content flow through this one positioned path; conflicts are pinned, not raised as Go errors.
		st.applyContent(frag)
	}

	if len(d.ToolFragments) > 0 { // retained index/ID/anonymous-continuation correlation for this event's fragments.
		st.applyToolEvent(d.ToolFragments)
	}

	st.checkEstablishment() // re-checked on every delta so a late half of the finish/payload combination can still establish before stream end.
}

func (st *assemblyState) noteRole(r string) {
	if st.role == "" { // no supplied role yet — record the first value without checking anything else on it right now.
		st.role = r
		return
	}
	if r != st.role {
		st.noteConflict("conflicting response roles %q then %q", st.role, r)
	}
}

func (st *assemblyState) noteConflict(format string, args ...any) {
	if st.conflictDetail == "" { // set-once semantics: later conflicts are ignored so finalization reports exactly what happened first.
		st.conflictDetail = fmt.Sprintf(format, args...)
	}
}
