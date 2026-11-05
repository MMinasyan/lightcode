package harness

import (
	"context"
	"fmt"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// executionDriver is the one owner of one admitted run: the immutable capture,
// the opened Execution, the conversation invocation with its request frame,
// the execution's uncommitted maintenance facts, and the complete
// completion/retirement of the run. It supplies the bound callbacks to the
// unchanged Engine; no caller receives a result/closer tuple or retains a
// post-call tail.
type executionDriver struct {
	harness      *Harness
	coordinator  *coordinator
	control      *activeExecution
	operationID  string
	prepared     PreparedExecution
	capture      ExecutionCapture
	execution    Execution
	conversation *openedInvocation
	pendingFacts []maintenanceFact
}

// openedInvocation is one opened Engine invocation: the actual issued
// request identity, the callback contracts bound to it, the conversation
// request-source control, and the current accepted conversation frame.
// Maintenance does not overwrite the conversation invocation's frame.
type openedInvocation struct {
	request      InvocationRequest
	callbacks    InvocationExecution
	firstRequest bool
	round        conversationRound
}

// runExecution drives one admitted run's complete lifecycle: the bound
// execution, the one quiet completion owner with its single lexical
// reservation acquisition, and the complete retirement. It retires only its
// matching run slot — a drain may already have retired it or installed a
// successor, and neither that run nor its context is cleared — then cancels
// its own context, settles the child completion for this operation, releases
// its graph use and closes its own done.
func (h *Harness) runExecution(c *coordinator, control *activeExecution, operationID string, prepared PreparedExecution, releaseGraph func()) error {
	driver := &executionDriver{
		harness:     h,
		coordinator: c,
		control:     control,
		operationID: operationID,
		prepared:    prepared,
	}
	res, runErr := driver.execute(control.execCtx)
	err := driver.complete(res, runErr)
	retired := false
	c.mu.Lock()
	if c.run == control { // a buffered drain may have installed the next execution already
		c.run = nil
		c.bumpLocalRevision()
		retired = true
	}
	c.mu.Unlock()
	if retired {
		h.observeInvalidation(c)
	}
	control.cancel() // the context dies only after the drain: a drain-installed successor owns a distinct context
	h.childCompletionSettled(c, operationID)
	if releaseGraph != nil {
		releaseGraph()
	}
	close(control.done)
	return err
}

// complete is the one completion owner of a finished execution: it acquires
// the Session's admission reservation once, maps the established outcome
// through the one quiet gate, retains the actual publication/storage cause,
// then disposes the owned conversation invocation, the opened Execution and
// the preparation reservation, and drains the post-terminal buffers while the
// reservation is held. A canceled contended acquisition skips only the drain.
func (d *executionDriver) complete(res agent.TerminalResult, runErr error) error {
	h := d.harness
	c := d.coordinator
	release, reserveErr := h.reserve(h.ctx, c)
	if reserveErr == nil {
		defer release()
	}
	err := d.settleTerminal(d.control.execCtx, res, runErr)
	h.recordStorageFailure(err) // the established outcome's storage-class cause; maintenance latches its own before disposing
	if d.conversation != nil {
		h.closeOpenedInvocation(d.conversation)
		d.conversation = nil
	}
	if d.execution.Close != nil {
		h.recordCleanupFailure(d.execution.Close())
	}
	h.releasePrepared(&d.prepared) // the adopted reservation releases after the execution's Close, on every completion path
	if reserveErr == nil {
		h.drainBuffers(c, d.control)
	}
	return err
}

// closeOpenedInvocation disposes one opened invocation's resources exactly
// once and retains its cleanup failure through the existing Wait owner.
func (h *Harness) closeOpenedInvocation(inv *openedInvocation) {
	if inv == nil || inv.callbacks.Close == nil {
		return
	}
	h.recordCleanupFailure(inv.callbacks.Close())
}

// execute is the private Agent composition of one admitted execution: it
// invokes the preparation's opener exactly once with the execution context and
// the owned committed admission, opens the conversation invocation, and runs
// the unchanged Engine over the bound callbacks. A compact Operation instead
// runs the complete manual maintenance procedure. It returns the Engine's
// already-established terminal and error verbatim; every disposal belongs to
// complete. The execution context is the control's: Interrupt cancels it, and
// closure or a consumed interrupt marker settles the durable Operation as
// interruption at entry.
func (d *executionDriver) execute(execCtx context.Context) (agent.TerminalResult, error) {
	h := d.harness
	c := d.coordinator
	c.mu.Lock()
	op, ok := c.graph.Operation(d.operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return agent.TerminalResult{}, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, d.operationID, sessionID)
	}
	admission := ownOperationRecord(op).Admission
	// The owner adopts the committed admission's capture once, under this
	// coordinator read and before the opener: a preparation producer that
	// mutates its retained containers after publication can never change what
	// this execution dispatches.
	d.capture = ownCapture(admission.Execution)
	guard := c.bgState == bgClosed || c.interruptOp == d.operationID
	if c.interruptOp == d.operationID {
		c.interruptOp = "" // consumed: the marker reaches exactly the execution it names
	}
	c.mu.Unlock()
	if guard || h.ctx.Err() != nil { // the entry guard joins the harness-loss early return: the opener never starts; genuine control cancellation is the explicit interruption
		return agent.TerminalResult{Status: agent.TerminalInterruption, Detail: executionInterruptedDetail}, nil
	}
	exec, err := d.prepared.Open(execCtx, admission)
	if err != nil {
		if res := executionCancellationResult(execCtx, err); res != nil {
			return *res, nil
		}
		return agent.TerminalResult{}, err
	}
	// The opened Execution owns its selected strategy from here: the resolved
	// StrategyBinding value is copied once, before any invocation or strategy
	// callback, so every maintenance procedure of this Execution selects the
	// same accepted binding and a caller mutating its own supplied binding
	// afterwards can never change the selection. A nil selection resolves to
	// a copy of the included binding.
	acceptedBinding := *resolveStrategyBinding(exec.Strategy)
	exec.Strategy = &acceptedBinding
	if exec.OpenInvocation == nil {
		if exec.Close != nil {
			h.recordCleanupFailure(exec.Close())
		}
		return agent.TerminalResult{}, invalidInput("opened execution requires a non-nil invocation opener")
	}
	d.execution = exec
	// A compact Operation wires no conversation Agent invocation: it projects
	// the pure canonical conversation snapshot, runs the selected strategy's
	// complete manual maintenance procedure over it, and commits the manual
	// checkpoint whose one transaction also settles the Operation's success.
	if admission.RequestKind == RequestKindCompact {
		messages, checkpointID, boundaryID, injected, err := h.projectContextRefs(c, d.operationID)
		if err != nil {
			return agent.TerminalResult{}, err
		}
		snapshot := messages
		if injected {
			snapshot = snapshot[1:]
		}
		planInput := PlanInput{
			Trigger:           triggerManual,
			Messages:          cloneRequestMessages(snapshot),
			CheckpointEntryID: checkpointID,
			BoundaryEntryID:   boundaryID,
			Instructions:      d.capture.Compact.SystemPrompt,
			Tools:             cloneToolDefinitions(d.capture.Tools),
			Measurement:       contextMeasurement(c),
			ContextWindow:     d.capture.ContextWindow,
			OutputReserve:     d.capture.OutputReserve,
			Settings:          model.CloneRaw(d.capture.Strategy.Settings),
		}
		_, err = d.completeMaintenance(execCtx, planInput, nil, true)
		return agent.TerminalResult{}, err
	}
	conversation, err := d.openInvocation(execCtx, InvocationPurposeConversation, d.capture.Model)
	if err != nil {
		if res := executionCancellationResult(execCtx, err); res != nil {
			return *res, nil
		}
		return agent.TerminalResult{}, err
	}
	conversation.firstRequest = true
	d.conversation = conversation
	res, runErr := agent.Run(execCtx, agent.Invocation{
		ExpectedModel: d.capture.Model,
		Request:       d.requestSource(),
		ModelEffect:   d.modelEffect(),
		ToolEffect:    d.toolEffect(),
	})
	// The Engine's already-established result or error is returned verbatim:
	// no context sampled after the Engine may relabel it.
	return res, runErr
}

// openInvocation opens one Engine invocation with a fresh per-call identity
// over the given purpose and model. The caller owns the returned invocation
// through its consumption; an invocation the opener rejects cleans its own
// provisional resources.
func (d *executionDriver) openInvocation(ctx context.Context, purpose InvocationPurpose, modelRef model.ModelRef) (*openedInvocation, error) {
	invocationID, err := newHexID() // the Engine call's fresh per-invocation identity
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	request := InvocationRequest{
		ID:      invocationID,
		Purpose: purpose,
		Model:   modelRef,
	}
	inv, err := d.execution.OpenInvocation(ctx, request)
	if err != nil {
		return nil, err
	}
	ownOpenedInvocationDeclarations(&inv) // the accepted invocation owns its declarations before anything else consumes them
	if err := validateOpenedInvocation(inv); err != nil {
		if inv.Close != nil {
			d.harness.recordCleanupFailure(inv.Close()) // an invalid invocation cleans its own opened resources
		}
		return nil, err
	}
	return &openedInvocation{request: request, callbacks: inv}, nil
}

// settleTerminal maps one established conversation or manual outcome onto the
// shared quiet gate. A zero terminal with no cause proposes no outcome through
// the same closed switch; the private steering-boundary token maps to the
// settled quiet success; a returned error maps to the ordinary quiet failure.
func (d *executionDriver) settleTerminal(execCtx context.Context, res agent.TerminalResult, runErr error) error {
	prospectiveSuccess := runErr == errSteeringBoundary // exact identity only; a projection/model/tool error is never this token
	if prospectiveSuccess {                             // consumed before the gate: the private run error clears to its settled value
		runErr = nil
	}
	terminal := res
	var proposed modelResult
	switch {
	case runErr != nil:
		proposed = modelResult{terminal: OperationFailure, detail: failureDetail(runErr)}
	case prospectiveSuccess, terminal.Status == agent.TerminalSuccess:
		proposed = modelResult{terminal: OperationSuccess, successContext: execCtx}
	case terminal.Status == agent.TerminalFailure, terminal.Status == agent.TerminalInterruption:
		term := OperationFailure
		if terminal.Status == agent.TerminalInterruption {
			term = OperationInterruption
		}
		proposed = modelResult{terminal: term, detail: terminal.Detail}
	default:
		return nil // a zero terminal without an error carries nothing to publish
	}
	if gateErr := d.publishQuiet(runErr, proposed); gateErr != nil {
		return gateErr
	}
	return runErr
}

// publishQuiet is the one quiet publication gate shared by established
// conversation and maintenance outcomes: an authoritative terminal or active
// effect and every marked publication failure are preserved, otherwise the
// proposed modelResult publishes at most once. It returns only its own
// inability to preserve or publish that outcome — nil when it published or the
// state needs no publication. The original callback cause stays with the
// caller, which returns it itself.
func (d *executionDriver) publishQuiet(runErr error, proposed modelResult) error {
	c := d.coordinator
	c.mu.Lock()
	op, ok := c.graph.Operation(d.operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return fmt.Errorf("%w: operation %q in session %q", ErrNotFound, d.operationID, sessionID)
	}
	settled := op.State.Status != OperationRunning
	quiet := op.State.ActiveEffect == nil
	c.mu.Unlock()
	if isPublicationFailure(runErr) { // classified by the publication boundary it occurred at: the durable running state stays for recovery
		return runErr
	}
	if settled || !quiet { // an authoritative terminal or a committed intent state left for recovery
		return nil
	}
	if _, err := d.commitEffectResult(context.WithoutCancel(d.harness.ctx), nil, proposed); err != nil {
		return err
	}
	return nil
}
