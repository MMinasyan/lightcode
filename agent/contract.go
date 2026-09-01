// Package agent defines the caller-driven model/tool effect contracts of the
// fixed Lightcode Agent, its one stream assembler, and its execution loop. A
// later Harness layer supplies plain functions for context, model effects, and
// tool effects; this package owns turning an accepted model.Stream into exactly
// one finalized model.Output, sequencing those caller boundaries under one
// cancellation invariant and a fixed model-effect cap, and validating what those
// caller boundaries return. It imports only the standard library and
// lightcode/model; transport, retry policy, persistence, events, and adaptation
// all belong to layers above these contracts.
package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/MMinasyan/lightcode/model"
)

type (
	ContextSource func(context.Context) ([]model.Message, error)

	AssemblyCallback func(model.ModelRef, model.Stream) (model.Output, error)

	ModelEffect func(context.Context, model.Request, AssemblyCallback) (ModelSettlement, error)

	ToolEffect func(context.Context, model.ToolCall) (model.ToolResult, error)
)

type ModelDisposition string

// The closed disposition values of every settlement returned by a model effect; any other value is invalid wherever a settlement is validated.
const (
	DispoReady        ModelDisposition = "ready"
	DispoContinue     ModelDisposition = "continue"
	DispoFailure      ModelDisposition = "failure"
	DispoInterruption ModelDisposition = "interruption"
)

type TerminalStatus string

// The closed terminal statuses returned at invocation end; a value outside this set is invalid wherever a terminal result is validated.
const (
	TerminalSuccess      TerminalStatus = "success"
	TerminalFailure      TerminalStatus = "failure"
	TerminalInterruption TerminalStatus = "interruption"
)

type ProtocolError struct {
	Boundary string // closed set: "agent", "model", or "tool".
	Detail   string // non-empty; only presence and typed classification are guaranteed.
}

// ErrBoundaryProtocol is the sentinel every ProtocolError classifies under through errors.Is, so callers can branch by category without parsing rendered text.
var ErrBoundaryProtocol = errors.New("caller boundary protocol violation")

func (e *ProtocolError) Error() string { return fmt.Sprintf("%s: %s", e.Boundary, e.Detail) }

// Is reports whether target is the package's single boundary-protocol sentinel, keeping every ProtocolError reachable through both errors.Is and errors.As at any wrap depth.
func (e *ProtocolError) Is(target error) bool { return target == ErrBoundaryProtocol }

func newBoundaryViolation(boundary, detail string) *ProtocolError {
	return &ProtocolError{Boundary: boundary, Detail: detail}
}

type Invocation struct {
	ExpectedModel model.ModelRef         // complete identity every callback-produced source must equal for the whole run.
	Tools         []model.ToolDefinition // owned immutable advertised tools; names unique, parameters object-shaped as validated at their own accepting boundary.
	Context       ContextSource          // nil is a protocol violation at invocation entry — never normalized to any other context.
	ModelEffect   ModelEffect            // exactly one per invocation; its settlements are validated against the closed disposition table before anything proceeds from them.
	ToolEffect    ToolEffect             // invoked once per started call in assembled order; nil is likewise rejected at entry.
}

type ModelSettlement struct {
	Disposition ModelDisposition // closed enum; any other value fails settlement validation outright.
	Output      *model.Output
	Detail      string // empty for ready/continue, non-empty for failure/interruption — mismatch in either direction is a protocol violation.
}

type TerminalResult struct {
	Status         TerminalStatus   // closed enum: success / failure / interruption.
	LastOutput     *model.Output    // optional last settled output; nil allowed on every status except success, where it is mandatory and completed.
	UnstartedCalls []model.ToolCall // ordered calls never dispatched to the tool boundary; only populated by an interruption carrying a completed last output, always in that message's order.
	Detail         string           // empty for success, non-empty otherwise — its wording beyond presence is diagnostic text, not public contract prose.
}
