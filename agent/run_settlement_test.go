package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// TestRunForgedModelSettlementsRejected pins every shared malformed settlement
// shape ending the run with its typed boundary-protocol error and a zero
// terminal, through the shared validator — no effect after it runs.
func TestRunForgedModelSettlementsRejected(t *testing.T) {
	for _, row := range forgedModelSettlements() {
		t.Run(row.name, func(t *testing.T) {
			f := &runFakes{}
			f.modelSet = setScript(row.set)

			res, err := Run(context.Background(), f.invocation())
			requireBoundaryViolation(t, err, "model")
			requireZeroTerminal(t, res)
			testassert.Require(t, f.modelCalls == 1 && f.toolCalls == 0, "model=%d tool=%d, want the run to stop at the forged settlement", f.modelCalls, f.toolCalls)
		})
	}
}

// TestRunContinueCompletedOutputContinues pins the newly permitted continuation row: a completed output carrying no tool calls settles as continue and the loop proceeds to the next model effect from a fresh snapshot.
func TestRunContinueCompletedOutputContinues(t *testing.T) {
	f := &runFakes{}
	f.modelSet = setScript(
		ModelSettlement{Disposition: DispoContinue, Output: mkOutput(model.OutputCompleted, "", nil)},
		ModelSettlement{Disposition: DispoReady, Output: mkOutput(model.OutputCompleted, "", nil)},
	)

	res, err := Run(context.Background(), f.invocation())
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success after completed continuation", res, err)
	testassert.Require(t, f.modelCalls == 2 && f.sourceCalls == 2 && f.toolCalls == 0, "model=%d source=%d tool=%d, want the continuation to fetch a fresh snapshot", f.modelCalls, f.sourceCalls, f.toolCalls)
}

// TestRunForgedToolResultsRejected pins the shared tool-result validator inside the batch: a settlement answering another call, carrying an unclosed status, or omitting mandated content ends the run with one typed "tool" violation, and no later call in the batch starts.
func TestRunForgedToolResultsRejected(t *testing.T) {
	rows := []struct {
		name string
		res  model.ToolResult
	}{
		{"wrong-call-id", model.ToolResult{CallID: "other", Status: model.ResultSuccess}},
		{"empty-call-id", model.ToolResult{Status: model.ResultSuccess}},
		{"unknown-status", model.ToolResult{CallID: "a", Status: "bogus", Content: "x"}},
		{"error-without-content", model.ToolResult{CallID: "a", Status: model.ResultError}},
		{"interrupted-without-content", model.ToolResult{CallID: "a", Status: model.ResultInterrupted}},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := &runFakes{}
			f.modelSet = readyOutputs(mkCallsTerminalOutput())
			f.toolRes = resScript(row.res)

			res, err := Run(context.Background(), f.invocation())
			requireBoundaryViolation(t, err, "tool")
			requireZeroTerminal(t, res)
			testassert.Equal(t, f.toolCalls, 1)
		})
	}
}

// TestRunInvalidSnapshotMessagesRejected pins that a context snapshot whose messages break role-specific invariants ends the run with an "agent" boundary-protocol error before any model effect starts.
func TestRunInvalidSnapshotMessagesRejected(t *testing.T) {
	f := &runFakes{}
	f.snapshot = func(int) ([]model.Message, error) {
		return []model.Message{{Role: model.RoleUser, Source: testRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}}}, nil // a user message may not carry a source identity.
	}

	res, err := Run(context.Background(), f.invocation())
	requireBoundaryViolation(t, err, "agent")
	requireZeroTerminal(t, res)
	testassert.Require(t, f.modelCalls == 0 && f.toolCalls == 0, "model=%d tool=%d, want the request rejection to precede any effect", f.modelCalls, f.toolCalls)
}

// TestRunDuplicateCallIDSettlementsRejected pins the shared completed-call identity rule inside the loop: duplicate IDs on a retained completed output are one typed model-boundary violation on both dispositions that may carry it, the run stops with a zero terminal, no call dispatches, and the interruption path cannot panic its way into a terminal.
func TestRunDuplicateCallIDSettlementsRejected(t *testing.T) {
	dup := func() *model.Output {
		return mkOutput(model.OutputCompleted, "", []model.ToolCall{{ID: "a", Name: "fnA"}, {ID: "a", Name: "fnB"}})
	}

	rows := []struct {
		name string
		set  func(out *model.Output) ModelSettlement
	}{
		{"ready", func(out *model.Output) ModelSettlement { return ModelSettlement{Disposition: DispoReady, Output: out} }},
		{"interruption", func(out *model.Output) ModelSettlement {
			return ModelSettlement{Disposition: DispoInterruption, Output: out, Detail: "cancelled after completion"}
		}},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := &runFakes{}
			f.modelSet = setScript(row.set(dup()))

			res, err := Run(context.Background(), f.invocation())
			requireBoundaryViolation(t, err, "model")
			requireZeroTerminal(t, res)
			testassert.Equal(t, f.toolCalls, 0)
		})
	}
}

// TestRunDetailMismatchDispositionsRejectedThroughRun pins the empty-detail rows of the closed disposition table at loop level: ready and continue settlements that smuggle in diagnostic detail never proceed.
func TestRunDetailMismatchDispositionsRejectedThroughRun(t *testing.T) {
	rows := []ModelSettlement{
		{Disposition: DispoReady, Output: mkOutput(model.OutputCompleted, "", nil), Detail: "extra"},
		{Disposition: DispoContinue, Output: mkOutput(model.OutputErrored, "boom", nil), Detail: "extra"},
	}

	for i, set := range rows {
		f := &runFakes{}
		f.modelSet = setScript(set)

		res, err := Run(context.Background(), f.invocation())
		requireBoundaryViolation(t, err, "model")
		requireZeroTerminal(t, res)
		testassert.Require(t, f.modelCalls == 1, "row %d model calls = %d, want 1", i, f.modelCalls)
	}
}

// TestRunInfraErrorAfterPriorContinuationVerbatim pins that a model-effect infrastructure error in a later iteration still returns verbatim with a zero terminal after an earlier authoritative settlement: prior state grants it no reclassification.
func TestRunInfraErrorAfterPriorContinuationVerbatim(t *testing.T) {
	f := &runFakes{}
	f.modelSet = func(call int, req model.Request, cb AssemblyCallback) (ModelSettlement, error) {
		if call <= 30 {
			return erroredContinue(call), nil
		}
		return ModelSettlement{}, errInfra
	}

	res, err := Run(context.Background(), f.invocation())
	requireZeroTerminal(t, res)
	testassert.Require(t, errors.Is(err, errInfra), "error = %v, want the infrastructure error verbatim", err)
	testassert.Require(t, f.modelCalls == 31 && f.sourceCalls == 31 && f.toolCalls == 0, "model=%d source=%d tool=%d, want 31/31/0", f.modelCalls, f.sourceCalls, f.toolCalls)
}

// TestDuplicateCallIDSharedValidator pins the one identity rule under both shared validators: a completed output whose tool calls repeat an ID is rejected naming the "model" boundary from the settlement validator and the "agent" boundary from the terminal validator, while distinct IDs pass both — the nearest allowed sibling.
func TestDuplicateCallIDSharedValidator(t *testing.T) {
	dup := mkOutput(model.OutputCompleted, "", []model.ToolCall{{ID: "a", Name: "fnA"}, {ID: "a", Name: "fnB"}})
	distinct := mkCallsTerminalOutput()

	{
		_, err := validateSettlement(ModelSettlement{Disposition: DispoReady, Output: dup}, testRef, true)
		testassert.Require(t, isBoundaryViolation(err, "model"), "settlement validator on duplicate IDs = %v, want a model-boundary violation", err)
	}
	{
		_, err := validateSettlement(ModelSettlement{Disposition: DispoReady, Output: distinct}, testRef, true)
		testassert.NoError(t, err)
	}

	{
		err := validateTerminalResult(TerminalResult{Status: TerminalFailure, LastOutput: dup, Detail: "d"})
		testassert.Require(t, isBoundaryViolation(err, "agent"), "terminal validator on duplicate IDs = %v, want an agent-boundary violation", err)
	}
	{
		err := validateTerminalResult(TerminalResult{Status: TerminalInterruption, LastOutput: distinct, UnstartedCalls: distinct.Message.ToolCalls[1:], Detail: "d"})
		testassert.NoError(t, err)
	}
}

// isBoundaryViolation reports whether err is a typed boundary-protocol error naming the given boundary with a present detail.
func isBoundaryViolation(err error, boundary string) bool {
	if !errors.Is(err, ErrBoundaryProtocol) {
		return false
	}
	var pe *ProtocolError
	return errors.As(err, &pe) && pe.Boundary == boundary && pe.Detail != ""
}

// TestRunFailureDispositionPassthrough pins the failure disposition: the matching terminal keeps the settlement's own detail and its optional errored (or absent) output, and nothing continues.
func TestRunFailureDispositionPassthrough(t *testing.T) {
	t.Run("with-errored-output", func(t *testing.T) {
		f := &runFakes{}
		errored := mkOutput(model.OutputErrored, "provider gave up", nil)
		f.modelSet = setScript(ModelSettlement{Disposition: DispoFailure, Output: errored, Detail: "gave up"})

		res, err := Run(context.Background(), f.invocation())
		testassert.Require(t, err == nil && res.Status == TerminalFailure && res.Detail == "gave up", "terminal = %#v, %v; want failure carrying the settlement detail", res, err)
		testassert.Equal(t, res.LastOutput, errored)
		testassert.Require(t, f.modelCalls == 1 && f.toolCalls == 0, "model=%d tool=%d, want 1/0", f.modelCalls, f.toolCalls)
	})

	t.Run("without-output", func(t *testing.T) {
		f := &runFakes{}
		f.modelSet = setScript(ModelSettlement{Disposition: DispoFailure, Detail: "no accepted stream"})

		res, err := Run(context.Background(), f.invocation())
		testassert.Require(t, err == nil && res.Status == TerminalFailure && res.LastOutput == nil && res.Detail == "no accepted stream", "terminal = %#v, %v; want outputless failure", res, err)
	})
}

// TestRunInterruptionDispositionPassthrough pins the interruption disposition: the settlement keeps its own detail (never re-labelled), and a retained completed response is never discarded — its not-yet-dispatched calls, all of them here, return in message order.
func TestRunInterruptionDispositionPassthrough(t *testing.T) {
	t.Run("in-flight-interrupted-output", func(t *testing.T) {
		f := &runFakes{}
		partial := mkOutput(model.OutputInterrupted, "cancelled mid-generation", nil)
		f.modelSet = setScript(ModelSettlement{Disposition: DispoInterruption, Output: partial, Detail: "cancelled mid-generation"})

		res, err := Run(context.Background(), f.invocation())
		requireRunInterrupted(t, res, err, "cancelled mid-generation")
		testassert.Require(t, res.LastOutput == partial && res.UnstartedCalls == nil, "terminal = %#v, want the settlement output carried verbatim", res)
	})

	t.Run("retained-completed-output", func(t *testing.T) {
		f := &runFakes{}
		callsOut := mkCallsTerminalOutput()
		f.modelSet = setScript(ModelSettlement{Disposition: DispoInterruption, Output: callsOut, Detail: "cancelled after completion"})

		res, err := Run(context.Background(), f.invocation())
		requireRunInterrupted(t, res, err, "cancelled after completion")
		testassert.Equal(t, res.LastOutput, callsOut)
		{
			got := joinedIDs(res.UnstartedCalls)
			testassert.Equal(t, got, "[a b c]")
		}
		testassert.Equal(t, f.toolCalls, 0)
	})

	t.Run("nothing-accepted", func(t *testing.T) {
		f := &runFakes{}
		f.modelSet = setScript(ModelSettlement{Disposition: DispoInterruption, Detail: "cancelled pre-acceptance"})

		res, err := Run(context.Background(), f.invocation())
		requireRunInterrupted(t, res, err, "cancelled pre-acceptance")
		testassert.Require(t, res.LastOutput == nil && res.UnstartedCalls == nil, "terminal = %#v, want a bare interruption", res)
	})
}
