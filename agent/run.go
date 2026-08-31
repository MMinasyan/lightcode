package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/MMinasyan/lightcode/model"
)

// maxModelEffects is the fixed cap on logical model-effect invocations per run; tool effects never consume it.
const maxModelEffects = 25

// The two contract-fixed terminal details: Agent-observed run-context cancellation and interrupted tool results interrupt under detailInterrupted, and model-effect cap exhaustion fails under detailCap. Every other diagnostic wording stays free.
const (
	detailInterrupted = "agent interrupted"
	detailCap         = "agent exceeded 25 model effects"
)

// Run performs one caller-driven Agent invocation over the three supplied boundaries: it validates every entry value before anything runs, then repeatedly obtains a fresh context snapshot, invokes one counted model effect under per-effect callback discipline (expected identity before assembly, exactly one accepted callback behind every settled output and none behind an outputless settlement, with any callback failure remembered and never ignorable), and dispatches the resulting tool batch sequentially in assembled order until a terminal result settles. One cancellation invariant governs the whole loop: the run context is checked before every caller effect starts, observed cancellation returns an interruption that never discards an already-settled authoritative value, and a non-nil caller error comes back verbatim with no terminal result (a cancellation-shaped error out of an active effect is instead a boundary-protocol violation, because active effects settle cancellation through their settlement). Agent retains no state after the invocation returns and owns no retry, persistence, events, signals, adaptation, tool-lookup, or history behavior.
func Run(ctx context.Context, inv Invocation) (TerminalResult, error) {
	if ctx == nil { // rejected outright: a nil run context is never normalized to any other context.
		return TerminalResult{}, newBoundaryViolation("agent", "run requires a non-nil run context")
	}
	if !nonzeroSource(inv.ExpectedModel) {
		return TerminalResult{}, newBoundaryViolation("agent", "run requires a nonzero expected model identity")
	}
	if inv.Context == nil || inv.ModelEffect == nil || inv.ToolEffect == nil {
		return TerminalResult{}, newBoundaryViolation("agent", "run requires non-nil context, model and tool effect functions")
	}
	advertised, err := model.NewRequest(model.Request{Tools: inv.Tools}) // non-empty unique names and object-shaped schemas are exactly what the landed request constructor enforces; its owned deep copy is the immutable advertised list for every later request.
	if err != nil {
		return TerminalResult{}, fmt.Errorf("%w: %v", newBoundaryViolation("agent", "advertised tools are invalid"), err)
	}

	var lastSettled *model.Output // the most recent authoritative model settlement; only a completed one may be carried by a later interruption.
	effects := 0                  // logical model effects already run, counted against the fixed cap.

	for {
		if ctx.Err() != nil { // cancellation dominates every other loop-top outcome, the cap included.
			return retainedInterruption(lastSettled, nil)
		}
		if effects == maxModelEffects { // a settlement would otherwise continue: fail before a 26th request is ever constructed.
			return finishTerminal(TerminalResult{Status: TerminalFailure, LastOutput: lastSettled, Detail: detailCap})
		}

		msgs, err := inv.Context(ctx)
		if err != nil {
			if isCancellation(err) && ctx.Err() != nil { // a cancellation report maps to interruption only under a genuinely done run context.
				return retainedInterruption(lastSettled, nil)
			}
			return TerminalResult{}, err // any other non-nil source error is infrastructure and wins over concurrent cancellation: returned with no terminal result.
		}
		req, err := model.NewRequest(model.Request{Messages: msgs, Tools: advertised.Tools}) // role-specific message invariants re-validate at this trust boundary; the request owns deep copies while the advertised list stays the entry-owned immutable one.
		if err != nil {
			return TerminalResult{}, fmt.Errorf("%w: %v", newBoundaryViolation("agent", "context snapshot is invalid"), err)
		}

		if ctx.Err() != nil {
			return retainedInterruption(lastSettled, nil)
		}
		cb := &callbackProtocol{ctx: ctx, expected: inv.ExpectedModel} // one observation scope per logical model effect; its callback closes over the run context so no caller can substitute another cancellation channel.
		set, err := inv.ModelEffect(ctx, req, cb.assemble)
		effects++
		if err != nil {
			if isCancellation(err) { // an active model effect settles cancellation through an interruption disposition, never through a Go error.
				return TerminalResult{}, newBoundaryViolation("model", "model effect reported cancellation as an error instead of an interruption settlement")
			}
			return TerminalResult{}, err // a non-nil caller error is never reclassified by concurrent cancellation.
		}
		if err := cb.settlementFailure(set.Output); err != nil { // the callback discipline behind the settlement gates it before anything else: an ignored protocol or invalid-call failure cannot settle.
			return TerminalResult{}, err
		}
		if err := validateSettlement(set, inv.ExpectedModel); err != nil {
			return TerminalResult{}, err
		}

		switch set.Disposition {
		case DispoFailure:
			return finishTerminal(TerminalResult{Status: TerminalFailure, LastOutput: set.Output, Detail: set.Detail})
		case DispoInterruption: // the matching terminal preserves the settlement's own detail; a retained completed response has established its completion, so all of its — so far undispatched — calls come back unstarted in message order.
			var unstarted []model.ToolCall
			if set.Output != nil && set.Output.Status == model.OutputCompleted {
				unstarted = set.Output.Message.ToolCalls
			}
			return interruptionTerminal(set.Detail, set.Output, unstarted)
		case DispoContinue:
			lastSettled = set.Output // an errored output can never ride an interruption terminal, so a later checkpoint simply carries nothing.
			continue
		}

		lastSettled = set.Output
		calls := set.Output.Message.ToolCalls // validation guarantees a ready disposition carries a completed output, and completed guarantees its message.
		if len(calls) == 0 {
			if ctx.Err() != nil { // the established completion is not discarded: the interruption carries exactly the output a success would have.
				return interruptionTerminal(detailInterrupted, set.Output, nil)
			}
			return finishTerminal(TerminalResult{Status: TerminalSuccess, LastOutput: set.Output})
		}

		for i, call := range calls { // sequential dispatch in assembled order.
			if ctx.Err() != nil { // no later effect starts: this call and everything after it remain unstarted.
				return interruptionTerminal(detailInterrupted, set.Output, calls[i:])
			}
			res, err := inv.ToolEffect(ctx, call)
			if err != nil {
				if isCancellation(err) { // an active tool effect settles cancellation through an interrupted result, never through a Go error.
					return TerminalResult{}, newBoundaryViolation("tool", "tool effect reported cancellation as an error instead of an interrupted result")
				}
				return TerminalResult{}, err
			}
			if err := validateToolResult(res, call.ID); err != nil {
				return TerminalResult{}, err
			}
			if res.Status == model.ResultInterrupted { // stops the batch immediately; only calls later than this settled one are unstarted.
				return interruptionTerminal(detailInterrupted, set.Output, calls[i+1:])
			}
			// success, error and denied settle only their own call and never stop the batch.
		}
	}
}

// isCancellation reports whether err is a context-cancellation failure, including wrapped values.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// callbackProtocol observes one logical model effect's use of the assembly callback it was handed: the expected identity is enforced before anything assembles, a second attempt never enters another assembly path, and any protocol or invalid-call failure is remembered so the effect cannot ignore it and settle anyway.
type callbackProtocol struct {
	ctx      context.Context
	expected model.ModelRef
	calls    int
	failure  error // sticky first violation or assembler invalid-call error.
}

// assemble is the AssemblyCallback handed to one model effect; stream ownership transfers into the assembler exactly like the callback contract says, but only for a correctly identified first attempt.
func (p *callbackProtocol) assemble(source model.ModelRef, stream model.Stream) (model.Output, error) {
	p.calls++
	var fail error
	switch {
	case p.calls > 1: // exactly one callback invocation accepts model work for one logical effect.
		fail = newBoundaryViolation("model", "assembly callback invoked more than once for one logical model effect")
	case source != p.expected: // the callback carries no authority to substitute another model identity.
		fail = newBoundaryViolation("model", fmt.Sprintf("assembly callback source %q does not equal invocation expected model identity %q", source.String(), p.expected.String()))
	}
	if fail != nil {
		p.remember(fail)
		return model.Output{}, fail
	}
	out, err := Assemble(p.ctx, source, stream)
	if err != nil {
		p.remember(err) // an invalid-call failure is the effect's own protocol breach to answer for, not a discarded attempt.
	}
	return out, err
}

func (p *callbackProtocol) remember(err error) {
	if p.failure == nil {
		p.failure = err
	}
}

// settlementFailure gates an authoritative settlement on the callback discipline behind it: any remembered failure ends the run, and the settlement's output presence must match the callback count exactly — every settled output came from one accepted callback, and no output means nothing was accepted. Callback and settlement outputs are deliberately not compared for content: substituting different otherwise-valid content is a broken trusted caller, not another protocol path.
func (p *callbackProtocol) settlementFailure(out *model.Output) error {
	if p.failure != nil {
		return p.failure
	}
	if out != nil && p.calls != 1 {
		return newBoundaryViolation("model", fmt.Sprintf("settlement carries an output from %d assembly callback invocations, exactly one is required", p.calls))
	}
	if out == nil && p.calls != 0 {
		return newBoundaryViolation("model", fmt.Sprintf("settlement carries no output but the assembly callback ran %d times, zero are required", p.calls))
	}
	return nil
}

// retainedInterruption ends the run under Agent-observed cancellation at a checkpoint: a completed last settlement survives as-is together with the given undispatched calls, while an errored one cannot appear on an interruption terminal, so nothing is carried.
func retainedInterruption(out *model.Output, unstarted []model.ToolCall) (TerminalResult, error) {
	if out != nil && out.Status != model.OutputCompleted {
		out, unstarted = nil, nil
	}
	return interruptionTerminal(detailInterrupted, out, unstarted)
}

// interruptionTerminal builds one interruption result; unstarted calls are an owned copy of the retained output's own ordered suffix, so the returned terminal never aliases LastOutput's message through its call slice.
func interruptionTerminal(detail string, out *model.Output, unstarted []model.ToolCall) (TerminalResult, error) {
	res := TerminalResult{Status: TerminalInterruption, LastOutput: out, Detail: detail}
	if len(unstarted) > 0 {
		res.UnstartedCalls = append(make([]model.ToolCall, 0, len(unstarted)), unstarted...)
	}
	return finishTerminal(res)
}

// finishTerminal is the single producer of every terminal Run returns: each constructed result passes the shared terminal validator first. Every branch above is total on the validated settlements it consumes, so a rejection here is an internal inconsistency, reported the same way the assembler reports its own invariant violations.
func finishTerminal(res TerminalResult) (TerminalResult, error) {
	if err := validateTerminalResult(res); err != nil {
		panic(fmt.Sprintf("internal agent invariant violated: constructed terminal result failed validation: %v", err))
	}
	return res, nil
}
