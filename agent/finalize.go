package agent

import (
	"context"
	"fmt"
	"sort"

	"github.com/MMinasyan/lightcode/model"
)

func (st *assemblyState) checkEstablishment() {
	if st.established || st.finishReason == "" {
		return
	}

	calls := st.buildCalls()
	switch st.finishReason {
	case "stop":
		if len(calls) == 0 && st.hasPayload() { // both halves hold right now — mark establishment immediately so later read errors and cancellation cannot downgrade this output any further along the wire from here on.
			st.established = true
		}

	case "tool_calls":
		if len(calls) > 0 {
			st.established = true
		}

	default:
		return
	}
}

func (st *assemblyState) finalize(ctx context.Context, readErr error) model.Output {
	if r := st.role; r != "" && model.Role(r) != model.RoleAssistant {
		st.noteConflict("response role %q is not assistant", r)
	}

	st.pinIncompleteOpaque()   // final-state content completeness gates every downstream classification path before any of them can complete or retain this response.
	st.pinIncompleteToolCall() // every observed normalized tool-call slot must end structurally complete — an unfinished one, with data in it or without, errors the whole response instead of being silently dropped when another valid call exists.

	if st.conflictDetail != "" {
		return st.erroredOutput(st.conflictDetail)
	}

	if readErr != nil { // a failed tail observation only downgrades streams that never established themselves — for those already completed on an earlier finish/payload combination the shared matrix must still agree with final state before completion is preserved respectively per contract documented inline within its own single-line comment further up over there now.
		switch {
		case st.established: // trust the flag only after re-running exactly the verdict cleanTermination applies below it — a late-arriving mismatch such as tool calls under an established stop errors with that matrix's own wording rather than silently completing or misclassifying into plain read-failure territory anywhere downstream of this shortcut moment along its trajectory forward now.
			if ok, detail := st.finishStateVerdict(); !ok { // the finally observed semantic state no longer matches its own finish reason — completion is not preserved through either a live read error OR a pre-existing cancellation when that divergence exists respectively per contract above these lines verbatim left-to-right as they appear in wire order over there.
				return st.erroredOutput(detail) // the mismatch itself explains this non-successful shape more precisely than any attribution to the failed tail observation could — no second diagnostic is needed alongside it anywhere downstream of that precedence decision further up now under exactly one shared rule documented inline within its own single-line comment rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all.
			}

			return st.completedOutput() // final semantic state still matches the reason that established it — completion is preserved through both read errors AND cancellation under exactly one shared rule documented here now (the positive sibling of the mismatch branch immediately above it in this same case body respectively left-to-right as they appear within these two statements further up over there).

		case ctx.Err() != nil:
			return st.interruptedOutput(ctx)

		default:
			return st.erroredOutput(fmt.Sprintf("stream read failed: %v", readErr))
		}
	}

	return st.cleanTermination()
}

// pinIncompleteOpaque pins a final-state conflict for any content position whose opaque kind never received its structural wire type. Such a part can neither complete an output nor ride into a retained partial, so the response must error through the shared detail route no matter how cleanly the stream otherwise terminated or what later read errors arrive after establishment.
func (st *assemblyState) pinIncompleteOpaque() {
	for _, pos := range st.contentOrder { // arrival order suffices: noteConflict keeps whichever position reports first and nothing downstream depends on which one that is.
		if acc := st.content[pos]; acc.kind == model.PartOpaque && acc.opaque == "" {
			st.noteConflict("content at position %d is an incomplete opaque part: no wire type arrived", pos)
			return // set-once keeps the first pinned conflict; a second pin would be dead weight.
		}
	}
}

// pinIncompleteToolCall pins a final-state conflict for every observed normalized tool-call slot that does not end structurally complete — whether it accumulated only part of its identity or nothing at all: an unfinished block must error the response rather than vanish silently when another valid call happens to exist in the same payload.
func (st *assemblyState) pinIncompleteToolCall() {
	if len(st.toolDeltas) == 0 {
		return
	}

	order := make([]int, 0, len(st.toolDeltas)) // sorted slots keep the reported conflict position deterministic across runs even though the underlying state is a map.
	for pos := range st.toolDeltas {
		order = append(order, pos)
	}
	sort.Ints(order)

	for _, pos := range order {
		if entry := st.toolDeltas[pos]; entry.id == "" || entry.name == "" { // every slot materialized during correlation is an observation of a tool call; at final state it must be complete in identity and name regardless of what its fragments carried or omitted.
			st.noteConflict("tool call at position %d is incomplete: missing id or name", pos)
			return // first unfinished block wins; later ones add nothing to classification.
		}
	}
}

// finishStateVerdict applies the one shared finish/payload consistency matrix over a single observed semantic snapshot, reporting whether completion holds for it and — when not — exactly the wording an errored output carries on that same shape: cleanTermination and final-state revalidation both consume this helper so those two paths cannot drift apart from each other.
func (st *assemblyState) finishStateVerdict() (bool, string) {
	calls := len(st.buildCalls())

	switch st.finishReason { // the closed reason vocabulary decides which half of the matrix applies to this snapshot before any payload check runs below it in wire order respectively left-to-right as they appear here now.
	case "": // an absent explicit reason completes whenever any eligible payload or valid calls exist at all — the truly empty response is its only errored shape on this branch above these lines verbatim (the fallback row of the same shared matrix rather than a separate special case anywhere downstream along this trajectory forward now).
		if calls > 0 || st.hasPayload() {
			return true, ""
		}

		return false, "empty response: no eligible content, refusal or tool calls"

	case "stop": // one payload-bearing message with zero valid calls is the only stop shape this matrix accepts on this path respectively per contract documented inline within its own single-line comment further up over there.
		if calls == 0 && st.hasPayload() {
			return true, ""
		}

		if calls > 0 {
			return false, "finish reason stop with present tool calls is a payload mismatch"
		}

		return false, "finish reason stop requires payload through content, refusal or finalized message extras"

	case "tool_calls": // at least one valid call must justify this finish reason on its own before any other state may rescue it respectively per contract above these lines verbatim without duplication elsewhere in wire order now.
		if calls > 0 {
			return true, ""
		}

		return false, "finish reason tool_calls requires one or more valid tool calls"

	default: // every other explicit value is unsupported under this closed finish-reason set rather than silently mapped onto some near-neighbor branch anywhere downstream along this trajectory forward now.
		return false, fmt.Sprintf("unsupported finish reason %q", st.finishReason)
	}
}

func (st *assemblyState) cleanTermination() model.Output { // the single consumer of the shared matrix on cleanly terminated streams: completion when its verdict holds, otherwise exactly the same errored shape carrying that own wording respectively per contract documented inline within finishStateVerdict above these lines verbatim.
	ok, detail := st.finishStateVerdict()

	if ok {
		return st.completedOutput()
	}

	return st.erroredOutput(detail)
}

func (st *assemblyState) completedOutput() model.Output {
	msg := st.assembleMessage(true)
	out, err := model.NewOutput(model.Output{Status: model.OutputCompleted, Source: st.source, Message: &msg, Usage: st.usage})
	if err != nil {
		panic(fmt.Sprintf("internal assembler invariant violated: completed output failed model-output validation: %v", err))
	}

	return out
}

func (st *assemblyState) erroredOutput(detail string) model.Output {
	return st.nonCompletedOutput(model.OutputErrored, detail)
}

// interruptedOutput classifies a read failure observed while the run context was already cancelled: same shape rules as an errored output but closed under the interrupted status.
func (st *assemblyState) interruptedOutput(ctx context.Context) model.Output {
	return st.nonCompletedOutput(model.OutputInterrupted, fmt.Sprintf("stream interrupted: %v", ctx.Err()))
}

// nonCompletedOutput builds one tool-call-free partial-retaining errored or interrupted output through the shared construction path; a validation failure here is an assembler invariant violation.
func (st *assemblyState) nonCompletedOutput(status model.OutputStatus, detail string) model.Output {
	msg := st.assembleMessage(false)
	var msgPtr *model.Message
	if msgHasEligiblePartialContent(msg) {
		m := msg
		msgPtr = &m
	}

	out, err := model.NewOutput(model.Output{Status: status, Source: st.source, Message: msgPtr, Usage: st.usage, Detail: detail})
	if err != nil {
		panic(fmt.Sprintf("internal assembler invariant violated: %s output failed model-output validation: %v", status, err))
	}

	return out
}
