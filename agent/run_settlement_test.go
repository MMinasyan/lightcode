package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// TestRunForgedModelSettlementsRejected pins every malformed settlement shape ending the run with one typed boundary-protocol error naming the "model" boundary and a zero terminal, through the shared validator — no effect after it runs.
func TestRunForgedModelSettlementsRejected(t *testing.T) {
	errored := mkOutput(model.OutputErrored, "boom", nil)
	completed := mkOutput(model.OutputCompleted, "", nil)
	foreign := mkForeignSettlementOutput(model.OutputCompleted, "", model.ModelRef{Provider: "acme", Model: "other"})

	rows := []struct {
		name string
		set  ModelSettlement
	}{
		{"ready-nil-output", ModelSettlement{Disposition: DispoReady}},
		{"ready-errored-output", ModelSettlement{Disposition: DispoReady, Output: errored}},
		{"continue-completed-output", ModelSettlement{Disposition: DispoContinue, Output: completed}},
		{"continue-nil-output", ModelSettlement{Disposition: DispoContinue}},
		{"failure-completed-output", ModelSettlement{Disposition: DispoFailure, Output: completed, Detail: "d"}},
		{"failure-empty-detail", ModelSettlement{Disposition: DispoFailure}},
		{"interruption-errored-output", ModelSettlement{Disposition: DispoInterruption, Output: errored, Detail: "d"}},
		{"interruption-empty-detail", ModelSettlement{Disposition: DispoInterruption}},
		{"unknown-disposition", ModelSettlement{Disposition: "bogus"}},
		{"foreign-source", ModelSettlement{Disposition: DispoReady, Output: foreign}},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := &runFakes{}
			f.modelSet = setScript(row.set)

			res, err := Run(context.Background(), f.invocation())
			requireBoundaryViolation(t, err, "model")
			requireZeroTerminal(t, res)
			if f.modelCalls != 1 || f.toolCalls != 0 {
				t.Fatalf("model=%d tool=%d, want the run to stop at the forged settlement", f.modelCalls, f.toolCalls)
			}
		})
	}
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
			if f.toolCalls != 1 {
				t.Fatalf("tool calls = %d, want the batch to stop at the forged settlement", f.toolCalls)
			}
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
	if f.modelCalls != 0 || f.toolCalls != 0 {
		t.Fatalf("model=%d tool=%d, want the request rejection to precede any effect", f.modelCalls, f.toolCalls)
	}
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
			if f.toolCalls != 0 {
				t.Fatalf("tool calls = %d, want no duplicate dispatch", f.toolCalls)
			}
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
		if f.modelCalls != 1 {
			t.Fatalf("row %d model calls = %d, want 1", i, f.modelCalls)
		}
	}
}

// TestRunInfraErrorAfterPriorContinuationVerbatim pins that a model-effect infrastructure error in a later iteration still returns verbatim with a zero terminal after an earlier authoritative settlement: prior state grants it no reclassification.
func TestRunInfraErrorAfterPriorContinuationVerbatim(t *testing.T) {
	f := &runFakes{}
	f.modelSet = func(call int, req model.Request, cb AssemblyCallback) (ModelSettlement, error) {
		if call == 1 {
			return erroredContinue(1), nil
		}
		return ModelSettlement{}, errInfra
	}

	res, err := Run(context.Background(), f.invocation())
	requireZeroTerminal(t, res)
	if !errors.Is(err, errInfra) {
		t.Fatalf("error = %v, want the infrastructure error verbatim", err)
	}
	if f.modelCalls != 2 || f.sourceCalls != 2 || f.toolCalls != 0 {
		t.Fatalf("model=%d source=%d tool=%d, want 2/2/0", f.modelCalls, f.sourceCalls, f.toolCalls)
	}
}

// TestDuplicateCallIDSharedValidator pins the one identity rule under both shared validators: a completed output whose tool calls repeat an ID is rejected naming the "model" boundary from the settlement validator and the "agent" boundary from the terminal validator, while distinct IDs pass both — the nearest allowed sibling.
func TestDuplicateCallIDSharedValidator(t *testing.T) {
	dup := mkOutput(model.OutputCompleted, "", []model.ToolCall{{ID: "a", Name: "fnA"}, {ID: "a", Name: "fnB"}})
	distinct := mkCallsTerminalOutput()

	if err := validateSettlement(ModelSettlement{Disposition: DispoReady, Output: dup}, testRef); !isBoundaryViolation(err, "model") {
		t.Fatalf("settlement validator on duplicate IDs = %v, want a model-boundary violation", err)
	}
	if err := validateSettlement(ModelSettlement{Disposition: DispoReady, Output: distinct}, testRef); err != nil {
		t.Fatalf("settlement validator on distinct IDs = %v, want acceptance", err)
	}

	if err := validateTerminalResult(TerminalResult{Status: TerminalFailure, LastOutput: dup, Detail: "d"}); !isBoundaryViolation(err, "agent") {
		t.Fatalf("terminal validator on duplicate IDs = %v, want an agent-boundary violation", err)
	}
	if err := validateTerminalResult(TerminalResult{Status: TerminalInterruption, LastOutput: distinct, UnstartedCalls: distinct.Message.ToolCalls[1:], Detail: "d"}); err != nil {
		t.Fatalf("terminal validator on distinct IDs = %v, want acceptance", err)
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

// TestRunFailureDispositionPassthrough pins the failure row of step 6: the matching terminal keeps the settlement's own detail and its optional errored (or absent) output, and nothing continues.
func TestRunFailureDispositionPassthrough(t *testing.T) {
	t.Run("with-errored-output", func(t *testing.T) {
		f := &runFakes{}
		errored := mkOutput(model.OutputErrored, "provider gave up", nil)
		f.modelSet = setScript(ModelSettlement{Disposition: DispoFailure, Output: errored, Detail: "gave up"})

		res, err := Run(context.Background(), f.invocation())
		if err != nil || res.Status != TerminalFailure || res.Detail != "gave up" {
			t.Fatalf("terminal = %#v, %v; want failure carrying the settlement detail", res, err)
		}
		if res.LastOutput != errored {
			t.Fatalf("failure dropped the settlement output: %p vs %p", res.LastOutput, errored)
		}
		if f.modelCalls != 1 || f.toolCalls != 0 {
			t.Fatalf("model=%d tool=%d, want 1/0", f.modelCalls, f.toolCalls)
		}
	})

	t.Run("without-output", func(t *testing.T) {
		f := &runFakes{}
		f.modelSet = setScript(ModelSettlement{Disposition: DispoFailure, Detail: "no accepted stream"})

		res, err := Run(context.Background(), f.invocation())
		if err != nil || res.Status != TerminalFailure || res.LastOutput != nil || res.Detail != "no accepted stream" {
			t.Fatalf("terminal = %#v, %v; want outputless failure", res, err)
		}
	})
}

// TestRunInterruptionDispositionPassthrough pins the interruption row of step 6: the settlement keeps its own detail (never re-labelled), and a retained completed response is never discarded — its not-yet-dispatched calls, all of them here, return in message order.
func TestRunInterruptionDispositionPassthrough(t *testing.T) {
	t.Run("in-flight-interrupted-output", func(t *testing.T) {
		f := &runFakes{}
		partial := mkOutput(model.OutputInterrupted, "cancelled mid-generation", nil)
		f.modelSet = setScript(ModelSettlement{Disposition: DispoInterruption, Output: partial, Detail: "cancelled mid-generation"})

		res, err := Run(context.Background(), f.invocation())
		requireRunInterrupted(t, res, err, "cancelled mid-generation")
		if res.LastOutput != partial || res.UnstartedCalls != nil {
			t.Fatalf("terminal = %#v, want the settlement output carried verbatim", res)
		}
	})

	t.Run("retained-completed-output", func(t *testing.T) {
		f := &runFakes{}
		callsOut := mkCallsTerminalOutput()
		f.modelSet = setScript(ModelSettlement{Disposition: DispoInterruption, Output: callsOut, Detail: "cancelled after completion"})

		res, err := Run(context.Background(), f.invocation())
		requireRunInterrupted(t, res, err, "cancelled after completion")
		if res.LastOutput != callsOut {
			t.Fatalf("established completion was discarded: %p vs %p", res.LastOutput, callsOut)
		}
		if got := joinedIDs(res.UnstartedCalls); got != "[a b c]" {
			t.Fatalf("unstarted = %s, want every undispatched call of the retained output", got)
		}
		if f.toolCalls != 0 {
			t.Fatalf("tool calls = %d, want no dispatch after an interrupted settlement", f.toolCalls)
		}
	})

	t.Run("nothing-accepted", func(t *testing.T) {
		f := &runFakes{}
		f.modelSet = setScript(ModelSettlement{Disposition: DispoInterruption, Detail: "cancelled pre-acceptance"})

		res, err := Run(context.Background(), f.invocation())
		requireRunInterrupted(t, res, err, "cancelled pre-acceptance")
		if res.LastOutput != nil || res.UnstartedCalls != nil {
			t.Fatalf("terminal = %#v, want a bare interruption", res)
		}
	})
}
