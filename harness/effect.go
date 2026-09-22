package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// interruptedToolResultContent is the contract-fixed model-visible content of
// every interrupted-before-execution tool result.
const interruptedToolResultContent = "Tool call interrupted."

// invalidToolResultContent is the contract-fixed model-visible content of the
// validation-error result an invalid prepared plan or invalid returned outcome
// maps onto.
const invalidToolResultContent = "Tool call failed internal validation."

// permissionDeniedToolResultContent is the contract-fixed model-visible
// content of every Harness policy denial.
const permissionDeniedToolResultContent = "Permission denied."

// maxToolDiagnosticBytes bounds one Harness-produced validation diagnostic
// carried in a tool-result content.
const maxToolDiagnosticBytes = 512

// executionInterruptedDetail is the diagnostic detail of the terminal
// interruption the Harness settles when execution cancellation stops a
// callback, mirroring the Agent's own cancellation checkpoints.
const executionInterruptedDetail = "agent interrupted"

// modelEffectIntent is the committed intent of one logical model effect: the
// facts the result transaction needs after the callback returns.
type modelEffectIntent struct {
	sessionID string
	expected  model.ModelRef
	resultID  string
}

// modelResult is the settlement content of one logical model effect: the
// produced assistant entry when the output carried an eligible payload, the
// fixed continuation signal when the disposition continues, the terminal
// classification when the effect settles the Operation, and the terminal
// no-output model usage when a reported usage has no assistant entry to ride.
type modelResult struct {
	assistant *assistantEntry // nil when no eligible assistant payload exists
	signal    SignalKind      // empty unless the disposition continues
	terminal  OperationState  // empty for ready/continue; failure or interruption otherwise
	detail    string          // terminal detail; empty for ready/continue
	usage     *UsageCount     // terminal no-output usage; nil when the assistant entry carries it or none was reported
}

// modelEffect encloses one complete invocation of the prepared model function
// in one logical model-effect intent/result pair: it commits the active-effect
// intent, makes the physical attempts with their retry classification and
// backoff, invokes the assembly callback exactly once after a stream is
// accepted, derives the settlement from the assembled output, validates it,
// and commits the result and complete next Operation state before returning
// the committed settlement. Before the assistant entry commits, every
// advertised completed call is normalized through the execution's pure
// callback outside storage transactions and owning locks.
func (h *Harness) modelEffect(c *coordinator, operationID string, exec Execution, capture ExecutionCapture) agent.ModelEffect {
	attempt := exec.Model
	normalize := exec.NormalizeTool
	advertised := advertisedToolNames(capture)
	retry := exec.Retry
	if retry == nil {
		retry = standardRetryPolicy
	}
	return func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		intent, err := h.beginModelEffect(ctx, c, operationID)
		if err != nil {
			// An intent transaction aborted by cancellation settles the
			// cancellation outcome: the run never sees a cancellation-shaped
			// error out of an active effect.
			if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() != nil {
				if _, cerr := h.commitEffectResult(context.WithoutCancel(h.ctx), c, operationID, nil, modelResult{
					terminal: OperationInterruption,
					detail:   executionInterruptedDetail,
				}); cerr != nil {
					return agent.ModelSettlement{}, cerr
				}
				return agent.ModelSettlement{Disposition: agent.DispoInterruption, Detail: executionInterruptedDetail}, nil
			}
			return agent.ModelSettlement{}, err
		}
		// Execution cancellation stops new callbacks: a context that died
		// between the committed intent and the callback settles the Operation
		// as terminal interruption without ever starting the callback.
		if ctx.Err() != nil {
			return h.interruptModelEffect(c, operationID, intent)
		}
		// Result and terminal transactions run without cancellation so an
		// already-produced result or required terminal settlement publishes.
		settleCtx := context.WithoutCancel(h.ctx)
		settle := func(cause error) (agent.ModelSettlement, error) {
			if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
				terminal: OperationFailure,
				detail:   cause.Error(),
			}); err != nil {
				return agent.ModelSettlement{}, err
			}
			return agent.ModelSettlement{}, cause
		}
		// The one attempt loop lives inside the committed intent: every
		// failed attempt is classified by the retry policy, waits run behind
		// the cancellation checks, and an accepted stream never re-enters
		// retry — assembly owns it, including closure.
		var output model.Output
		for failed := 1; ; failed++ {
			if err := ctx.Err(); err != nil { // observed cancellation before an attempt interrupts; no output and no assembly call
				return h.interruptModelEffect(c, operationID, intent)
			}
			stream, attemptErr := attempt(ctx, req)
			if attemptErr == nil && stream != nil {
				output, attemptErr = assemble(intent.expected, stream) // exactly one assembly after acceptance
				if attemptErr != nil {
					return settle(attemptErr)
				}
				break
			}
			if attemptErr == nil || stream != nil { // exactly one stream or one error must be returned; a supplied stream closes before the boundary failure
				if stream != nil {
					_ = stream.Close()
				}
				return settle(&agent.ProtocolError{Boundary: "model", Detail: "physical model request returned neither exactly one stream nor one error"})
			}
			// An attempt failure observed under a done execution context
			// settles the interruption outcome before any classification:
			// regardless of the attempt error's shape or the retry policy's
			// answer, pre-acceptance cancellation is an interruption with no
			// output and no assembly call.
			if ctx.Err() != nil {
				return h.interruptModelEffect(c, operationID, intent)
			}
			delay, again := retry(attemptErr, failed)
			if !again || delay < 0 {
				committed := agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: attemptErr.Error()}
				if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
					terminal: OperationFailure,
					detail:   attemptErr.Error(),
				}); err != nil {
					return agent.ModelSettlement{}, err
				}
				return committed, nil
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done(): // cancellation during backoff interrupts; no attempt starts
				return h.interruptModelEffect(c, operationID, intent)
			}
		}
		set := derivedSettlement(output)
		owned, err := agent.ValidateModelSettlement(intent.expected, set)
		if err != nil {
			return settle(err)
		}
		// A completed call whose identity the Operation already published can
		// never commit: one published call is one settlement unit, and a
		// second assistant entry repeating the identity is durable corruption.
		if owned.Output != nil && owned.Output.Message != nil && len(owned.Output.Message.ToolCalls) > 0 {
			published := publishedCallIDs(c, operationID)
			for _, call := range owned.Output.Message.ToolCalls {
				if published[call.ID] {
					return settle(&agent.ProtocolError{Boundary: "model", Detail: fmt.Sprintf("completed output repeats tool call id %q already published by operation %q", call.ID, operationID)})
				}
			}
		}
		assistant, reported, err := newAssistantEntry(intent.sessionID, operationID, intent.resultID, owned.Output, normalize, advertised)
		if err != nil {
			return agent.ModelSettlement{}, err
		}
		var res modelResult
		switch owned.Disposition {
		case agent.DispoReady:
			res = modelResult{assistant: assistant}
		case agent.DispoContinue:
			res = modelResult{assistant: assistant, signal: SignalModelFailureContinuation}
		case agent.DispoFailure:
			res = modelResult{assistant: assistant, terminal: OperationFailure, detail: owned.Detail, usage: reported}
		case agent.DispoInterruption:
			res = modelResult{assistant: assistant, terminal: OperationInterruption, detail: owned.Detail, usage: reported}
		}
		if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, res); err != nil {
			return agent.ModelSettlement{}, err
		}
		// Coordinator decision at the model-result boundary, linearized
		// against submissions: waiting steering keeps the Operation running
		// independent of completed/errored status, text, or tool calls. The
		// only shape where Agent would otherwise return is a ready settlement
		// with no tool calls, so the wrapper bridges it with the continue
		// disposition — the only source of a completed-output continue.
		c.mu.Lock()
		waiting := len(c.steering) > 0
		c.mu.Unlock()
		if waiting && owned.Disposition == agent.DispoReady &&
			(owned.Output == nil || owned.Output.Message == nil || len(owned.Output.Message.ToolCalls) == 0) {
			owned.Disposition = agent.DispoContinue
		}
		return owned, nil
	}
}

// interruptModelEffect settles one committed model effect intent as the fixed
// terminal interruption after observed execution cancellation before stream
// acceptance: no output, no assembly call, and the committed settlement
// returned with a nil error.
func (h *Harness) interruptModelEffect(c *coordinator, operationID string, intent modelEffectIntent) (agent.ModelSettlement, error) {
	settleCtx := context.WithoutCancel(h.ctx)
	committed := agent.ModelSettlement{Disposition: agent.DispoInterruption, Detail: executionInterruptedDetail}
	if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
		terminal: OperationInterruption,
		detail:   executionInterruptedDetail,
	}); err != nil {
		return agent.ModelSettlement{}, err
	}
	return committed, nil
}

// derivedSettlement maps one assembled output onto the closed disposition
// table: completed is ready, interrupted interrupts with its own detail, an
// errored output retaining a partial payload continues, and a payload-less
// errored output fails with its detail.
func derivedSettlement(out model.Output) agent.ModelSettlement {
	switch out.Status {
	case model.OutputCompleted:
		return agent.ModelSettlement{Disposition: agent.DispoReady, Output: &out}
	case model.OutputInterrupted:
		return agent.ModelSettlement{Disposition: agent.DispoInterruption, Output: &out, Detail: out.Detail}
	default:
		if outputCarriesPayload(out) {
			return agent.ModelSettlement{Disposition: agent.DispoContinue, Output: &out}
		}
		return agent.ModelSettlement{Disposition: agent.DispoFailure, Output: &out, Detail: out.Detail}
	}
}

// outputCarriesPayload reports whether one finalized output retains a
// model-visible payload, mirroring the model package's finalized payload
// predicate: a non-empty refusal, tool calls, one non-empty finalized content
// part, or one finalized non-null message extra.
func outputCarriesPayload(out model.Output) bool {
	if out.Message == nil {
		return false
	}
	if out.Message.Refusal != "" || len(out.Message.ToolCalls) > 0 {
		return true
	}
	for _, part := range out.Message.Content {
		if part.Text != "" || part.URL != "" || part.OpaqueWireType != "" || len(part.Extra.Finalize()) > 0 {
			return true
		}
	}
	return len(out.Message.Extra.Finalize()) > 0
}

// standardRetryPolicy is the nil-Retry classifier: HTTP 429 and 5xx failures,
// errors wrapping net.OpError, and pre-response io.EOF/io.ErrUnexpectedEOF
// permit retries 1..3 at 2s/4s/8s; every other failure stops.
func standardRetryPolicy(cause error, failed int) (time.Duration, bool) {
	if failed > 3 || !retryableTransportFailure(cause) {
		return 0, false
	}
	return 2 * time.Duration(1<<(failed-1)) * time.Second, true
}

// retryableTransportFailure classifies one failed physical attempt under the
// standard policy's fixed transport-failure classes.
func retryableTransportFailure(cause error) bool {
	var status *model.HTTPStatusError
	if errors.As(cause, &status) {
		return status.StatusCode == 429 || (status.StatusCode >= 500 && status.StatusCode <= 599)
	}
	var op *net.OpError
	if errors.As(cause, &op) {
		return true
	}
	return errors.Is(cause, io.EOF) || errors.Is(cause, io.ErrUnexpectedEOF)
}

// publishedCallIDs returns the tool call IDs the Operation's committed
// assistant entries already publish, read from the coordinator's validated
// view.
func publishedCallIDs(c *coordinator, operationID string) map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := map[string]bool{}
	for _, entry := range c.graph.Entries {
		if entry.Envelope.OperationID != operationID || entry.Assistant == nil {
			continue
		}
		for _, call := range entry.Assistant.ToolCalls {
			ids[call.ID] = true
		}
	}
	return ids
}

// beginModelEffect commits the one active-effect intent of a logical model
// effect: it reserves the effect's result entry identity and records it in the
// Operation register. The reserved identity stays free until the model result
// or the consuming settlement commits under it.
func (h *Harness) beginModelEffect(ctx context.Context, c *coordinator, operationID string) (modelEffectIntent, error) {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return modelEffectIntent{}, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	if op.State.Status != OperationRunning {
		c.mu.Unlock()
		return modelEffectIntent{}, invalidInput("operation %q is %s; a model effect requires a running Operation", operationID, op.State.Status)
	}
	if op.State.ActiveEffect != nil {
		c.mu.Unlock()
		return modelEffectIntent{}, invalidInput("operation %q already carries an active effect; effects never nest", operationID)
	}
	resultID, err := newHexID()
	if err != nil {
		c.mu.Unlock()
		return modelEffectIntent{}, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	var updated OperationRecord
	err = h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		key := RegisterKey{SessionID: op.Admission.SessionID, Kind: RegisterOperation, OperationID: operationID}
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		current, err := decodeOperationRegister(reg)
		if err != nil {
			return corruptSession(op.Admission.SessionID, "operation register %q: %v", operationID, err)
		}
		// a violated semantic precondition outranks the conflict class
		if current.State.Status != OperationRunning {
			return invalidInput("operation %q is %s; a model effect requires a running Operation", operationID, current.State.Status)
		}
		if reg.Revision != op.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, op.Revision, reg.Revision)
		}
		next := current
		next.State.ActiveEffect = &ActiveEffect{Kind: EffectModel, ResultEntryID: resultID}
		payload, err := encodeOperationRegister(next)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
		if err != nil {
			return err
		}
		updated = next
		updated.Revision = replaced.Revision
		return nil
	})
	if err != nil {
		h.markCorrupt(op.Admission.SessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(ctx, c, op.Admission.SessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return modelEffectIntent{}, rerr
			}
		}
		return modelEffectIntent{}, err
	}
	c.graph.replaceOperation(operationID, updated)
	c.mu.Unlock()
	return modelEffectIntent{
		sessionID: op.Admission.SessionID,
		expected:  op.Admission.Execution.Model,
		resultID:  resultID,
	}, nil
}

// commitEffectResult commits the one result transaction of a settled effect:
// the semantic result entries, usage, the complete next Operation state, and
// the required Session state. A running result clears the active effect and
// records the completed calls' reservations; a terminal result runs the common
// terminal helper — preserving any produced assistant payload, interrupting
// every call that will not execute, appending exactly one interruption signal,
// appending the settlement entry, writing the terminal Operation, and clearing
// the Session current Operation — all atomically. A non-nil intent must find
// its committed model effect intent, whose reserved identity the settlement
// consumes when no assistant payload exists; a nil intent (the outer terminal
// settlement after agent.Run) requires a quiet Operation and always assigns a
// fresh settlement identity. A publication failure leaves the committed
// running/intent state for recovery.
func (h *Harness) commitEffectResult(ctx context.Context, c *coordinator, operationID string, intent *modelEffectIntent, res modelResult) (OperationRecord, error) {
	c.mu.Lock()
	viewOp, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return OperationRecord{}, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	sessionID := viewOp.Admission.SessionID
	viewSession := c.graph.Session

	var (
		committedOp   OperationRecord
		committedSess SessionRecord
		newEntries    []graphEntry
	)
	err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		sessionKey := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
		sreg, err := tx.ReadRegister(sessionKey)
		if err != nil {
			return err
		}
		currentSession, err := decodeSessionRegister(sreg)
		if err != nil {
			return corruptSession(sessionID, "session register: %v", err)
		}
		opKey := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID}
		oreg, err := tx.ReadRegister(opKey)
		if err != nil {
			return err
		}
		currentOp, err := decodeOperationRegister(oreg)
		if err != nil {
			return corruptSession(sessionID, "operation register %q: %v", operationID, err)
		}
		// violated semantic preconditions outrank the conflict class
		if currentOp.State.Status != OperationRunning {
			return invalidInput("operation %q is %s; its model effect requires a running Operation", operationID, currentOp.State.Status)
		}
		if intent != nil {
			if currentOp.State.ActiveEffect == nil || currentOp.State.ActiveEffect.Kind != EffectModel ||
				currentOp.State.ActiveEffect.ResultEntryID != intent.resultID {
				return invalidInput("operation %q carries no committed model effect intent for result identity %q", operationID, intent.resultID)
			}
		} else if currentOp.State.ActiveEffect != nil {
			return invalidInput("operation %q carries an active effect; the settlement requires a quiet Operation", operationID)
		}
		if sreg.Revision != viewSession.Revision {
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, viewSession.Revision, sreg.Revision)
		}
		if oreg.Revision != viewOp.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, viewOp.Revision, oreg.Revision)
		}

		// A terminal result runs the common terminal helper on this
		// transaction's own decoded records.
		if res.terminal != "" {
			var usageModel model.ModelRef
			if intent != nil {
				usageModel = intent.expected
			}
			var err error
			committedOp, committedSess, newEntries, err = commitTerminalSettlement(tx, sessionID, operationID, currentSession, currentOp, sreg.Revision, oreg.Revision, res.terminal, res.detail, res.assistant, usageModel, res.usage)
			if err != nil {
				return err
			}
			return nil
		}

		// The transaction's usage contribution rides the produced assistant
		// entry; the same totals land on the producer entry, the Operation,
		// and the Session.
		var contribution UsageTotals
		if res.assistant != nil && res.assistant.Usage != nil {
			contribution = UsageTotals{ByModel: []ModelUsage{{Model: res.assistant.Source, Usage: *res.assistant.Usage}}}
		}
		opUsage, err := addUsageTotals(currentOp.State.Usage, contribution)
		if err != nil {
			return err
		}
		sessionUsage, err := addUsageTotals(currentSession.State.Usage, contribution)
		if err != nil {
			return err
		}

		// The assistant entry consumes the effect's reserved identity; its
		// completed calls become the durable tool-call intent.
		var pending []PendingToolCall
		if res.assistant != nil {
			payload, err := encodeAssistantEntry(*res.assistant)
			if err != nil {
				return err
			}
			adopted, err := insertAndAdopt(tx, sessionID, EntryDraft{
				SessionID:   sessionID,
				ID:          res.assistant.EntryID,
				OperationID: operationID,
				Kind:        EntryAssistant,
				Payload:     payload,
			})
			if err != nil {
				return err
			}
			newEntries = append(newEntries, adopted)
			for _, call := range res.assistant.ToolCalls {
				pending = append(pending, PendingToolCall{
					AssistantEntry: EntryRef{SessionID: sessionID, EntryID: res.assistant.EntryID},
					CallID:         call.ID,
					ResultEntryID:  call.ResultEntryID,
				})
			}
		}

		// A running continue result appends its fixed continuation signal.
		if res.signal != "" {
			entry, err := newSignalEntry(tx, sessionID, operationID, res.signal)
			if err != nil {
				return err
			}
			newEntries = append(newEntries, *entry)
		}

		next := currentOp
		next.State.ActiveEffect = nil
		next.State.Usage = opUsage
		next.State.Status = OperationRunning
		next.State.PendingToolCalls = append(next.State.PendingToolCalls, pending...)
		opPayload, err := encodeOperationRegister(next)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(opKey, oreg.Revision, opPayload)
		if err != nil {
			return err
		}
		next.Revision = replaced.Revision

		sessionState := currentSession.State
		sessionState.Usage = sessionUsage
		committedSess = SessionRecord{Identity: currentSession.Identity, State: sessionState}
		sessionPayload, err := encodeSessionRegister(committedSess)
		if err != nil {
			return err
		}
		replacedSession, err := tx.ReplaceRegister(sessionKey, sreg.Revision, sessionPayload)
		if err != nil {
			return err
		}
		committedSess.Revision = replacedSession.Revision
		committedOp = next
		return nil
	})
	if err != nil {
		h.markCorrupt(sessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return OperationRecord{}, rerr
			}
		}
		return OperationRecord{}, err
	}
	c.graph.Entries = append(c.graph.Entries, newEntries...)
	c.graph.replaceOperation(operationID, committedOp)
	c.graph.Session = committedSess
	c.mu.Unlock()
	return committedOp, nil
}

// commitTerminalSettlement performs the complete terminal transition in one
// transaction against freshly decoded register records: it preserves any
// produced assistant payload with its completed calls' reservations, writes
// model.ResultInterrupted under every remaining committed tool-result identity
// whose call will not execute — a terminal Operation leaves no unresolved
// call — appends exactly one interruption signal for an interruption
// terminal, appends the Operation settlement entry, writes the terminal
// Operation with its usage totals, and clears the Session current Operation,
// all atomically. The settlement entry consumes a committed model effect's
// reserved identity when no assistant payload exists; every other settlement
// takes a fresh identity. Live settlement calls it from the model-result
// transaction after its own preconditions; recovery calls it from its own
// re-reading transaction. It performs no precondition check of its own: the
// caller passes only records whose state it may settle.
func commitTerminalSettlement(tx Transaction, sessionID, operationID string, currentSession SessionRecord, currentOp OperationRecord, sregRevision, oregRevision int64, terminal OperationState, detail string, assistant *assistantEntry, usageModel model.ModelRef, usage *UsageCount) (OperationRecord, SessionRecord, []graphEntry, error) {
	var newEntries []graphEntry

	// The transaction's usage contribution rides the produced assistant
	// entry, or the settlement entry when the terminal output reported
	// usage without an eligible payload; the same totals land on the
	// producer entry, the Operation, and the Session.
	var contribution UsageTotals
	switch {
	case assistant != nil && assistant.Usage != nil:
		contribution = UsageTotals{ByModel: []ModelUsage{{Model: assistant.Source, Usage: *assistant.Usage}}}
	case usage != nil:
		contribution = UsageTotals{ByModel: []ModelUsage{{Model: usageModel, Usage: *usage}}}
	}
	opUsage, err := addUsageTotals(currentOp.State.Usage, contribution)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, nil, err
	}
	sessionUsage, err := addUsageTotals(currentSession.State.Usage, contribution)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, nil, err
	}

	// The assistant entry consumes the effect's reserved identity; its
	// completed calls become the durable tool-call intent.
	var pending []PendingToolCall
	if assistant != nil {
		payload, err := encodeAssistantEntry(*assistant)
		if err != nil {
			return OperationRecord{}, SessionRecord{}, nil, err
		}
		adopted, err := insertAndAdopt(tx, sessionID, EntryDraft{
			SessionID:   sessionID,
			ID:          assistant.EntryID,
			OperationID: operationID,
			Kind:        EntryAssistant,
			Payload:     payload,
		})
		if err != nil {
			return OperationRecord{}, SessionRecord{}, nil, err
		}
		newEntries = append(newEntries, adopted)
		for _, call := range assistant.ToolCalls {
			pending = append(pending, PendingToolCall{
				AssistantEntry: EntryRef{SessionID: sessionID, EntryID: assistant.EntryID},
				CallID:         call.ID,
				ResultEntryID:  call.ResultEntryID,
			})
		}
	}

	// One interrupted hook_result under an active hook effect's reserved
	// identity: recovery settles an unfinished hook without replaying or
	// resuming it, before the interrupted tool results commit.
	if currentOp.State.ActiveEffect != nil && currentOp.State.ActiveEffect.Kind == EffectHook {
		hook := hookResultEntry{
			SessionID:   sessionID,
			EntryID:     currentOp.State.ActiveEffect.ResultEntryID,
			OperationID: operationID,
			HookID:      currentOp.State.ActiveEffect.HookID,
			ToolCallID:  currentOp.State.ActiveEffect.ToolCallID,
			Status:      hookInterrupted,
			Error:       recoveryInterruptedDetail,
		}
		payload, err := encodeHookResultEntry(hook)
		if err != nil {
			return OperationRecord{}, SessionRecord{}, nil, err
		}
		adopted, err := insertAndAdopt(tx, sessionID, EntryDraft{
			SessionID:   sessionID,
			ID:          hook.EntryID,
			OperationID: operationID,
			Kind:        EntryHookResult,
			Payload:     payload,
		})
		if err != nil {
			return OperationRecord{}, SessionRecord{}, nil, err
		}
		newEntries = append(newEntries, adopted)
	}

	// One interrupted result under every remaining committed tool-result
	// identity whose call will not execute.
	remaining := append(append([]PendingToolCall{}, currentOp.State.PendingToolCalls...), pending...)
	for _, call := range remaining {
		result := toolResultEntry{
			SessionID:      sessionID,
			EntryID:        call.ResultEntryID,
			OperationID:    operationID,
			AssistantEntry: call.AssistantEntry,
			ToolCallID:     call.CallID,
			Status:         model.ResultInterrupted,
			Content:        interruptedToolResultContent,
		}
		payload, err := encodeToolResultEntry(result)
		if err != nil {
			return OperationRecord{}, SessionRecord{}, nil, err
		}
		adopted, err := insertAndAdopt(tx, sessionID, EntryDraft{
			SessionID:   sessionID,
			ID:          result.EntryID,
			OperationID: operationID,
			Kind:        EntryToolResult,
			Payload:     payload,
		})
		if err != nil {
			return OperationRecord{}, SessionRecord{}, nil, err
		}
		newEntries = append(newEntries, adopted)
	}

	// The interruption terminal appends exactly one interruption signal.
	if terminal == OperationInterruption {
		entry, err := newSignalEntry(tx, sessionID, operationID, SignalInterruption)
		if err != nil {
			return OperationRecord{}, SessionRecord{}, nil, err
		}
		newEntries = append(newEntries, *entry)
	}

	// The settlement entry consumes the model effect's reserved identity
	// when no assistant payload exists; every other settlement takes a fresh
	// identity.
	settlementID := ""
	if assistant == nil && currentOp.State.ActiveEffect != nil && currentOp.State.ActiveEffect.Kind == EffectModel {
		settlementID = currentOp.State.ActiveEffect.ResultEntryID
	}
	if settlementID == "" {
		if settlementID, err = newHexID(); err != nil {
			return OperationRecord{}, SessionRecord{}, nil, fmt.Errorf("%w: %v", ErrStorage, err)
		}
	}
	settlement := operationSettlementEntry{
		SessionID:   sessionID,
		EntryID:     settlementID,
		OperationID: operationID,
		Status:      terminal,
		Detail:      detail,
	}
	if assistant == nil && usage != nil { // terminal no-output model usage rides the settlement entry
		modelRef := usageModel
		settlement.Model = &modelRef
		settlement.Usage = usage
	}
	payload, err := encodeOperationSettlementEntry(settlement)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, nil, err
	}
	adopted, err := insertAndAdopt(tx, sessionID, EntryDraft{
		SessionID:   sessionID,
		ID:          settlement.EntryID,
		OperationID: operationID,
		Kind:        EntryOperationSettlement,
		Payload:     payload,
	})
	if err != nil {
		return OperationRecord{}, SessionRecord{}, nil, err
	}
	newEntries = append(newEntries, adopted)
	settled := adopted.Envelope.CommittedAt

	next := currentOp
	next.State.ActiveEffect = nil
	next.State.Usage = opUsage
	next.State.Status = terminal
	next.State.PendingToolCalls = []PendingToolCall{}
	next.State.SettledAt = &settled
	next.State.Terminal = &OperationTerminal{
		SettlementEntry: EntryRef{SessionID: sessionID, EntryID: settlementID},
		Detail:          detail,
	}
	opPayload, err := encodeOperationRegister(next)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, nil, err
	}
	replaced, err := tx.ReplaceRegister(RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID}, oregRevision, opPayload)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, nil, err
	}
	next.Revision = replaced.Revision

	sessionState := currentSession.State
	sessionState.Usage = sessionUsage
	sessionState.CurrentOperationID = ""
	committedSess := SessionRecord{Identity: currentSession.Identity, State: sessionState}
	sessionPayload, err := encodeSessionRegister(committedSess)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, nil, err
	}
	replacedSession, err := tx.ReplaceRegister(RegisterKey{SessionID: sessionID, Kind: RegisterSession}, sregRevision, sessionPayload)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, nil, err
	}
	committedSess.Revision = replacedSession.Revision
	return next, committedSess, newEntries, nil
}

// insertAndAdopt inserts one entry draft and returns its codec-cloned view
// entry, so no committed view value aliases the settlement handed upward.
func insertAndAdopt(tx Transaction, sessionID string, draft EntryDraft) (graphEntry, error) {
	inserted, err := tx.InsertEntry(draft)
	if err != nil {
		return graphEntry{}, err
	}
	adopted, err := decodeGraphEntry(sessionID, inserted)
	if err != nil {
		return graphEntry{}, corruptSession(sessionID, "committed entry %s: %v", inserted.ID, err)
	}
	return adopted, nil
}

// newSignalEntry builds and commits one signal entry of the given kind through
// the transaction, returning its codec-cloned view entry.
func newSignalEntry(tx Transaction, sessionID, operationID string, kind SignalKind) (*graphEntry, error) {
	entryID, err := newHexID()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	entry := signalEntry{
		SessionID:        sessionID,
		EntryID:          entryID,
		OperationID:      operationID,
		Signal:           kind,
		RelatedOperation: &operationRef{SessionID: sessionID, OperationID: operationID},
		Content:          signalContent(kind),
	}
	payload, err := encodeSignalEntry(entry)
	if err != nil {
		return nil, err
	}
	adopted, err := insertAndAdopt(tx, sessionID, EntryDraft{
		SessionID:   sessionID,
		ID:          entryID,
		OperationID: operationID,
		Kind:        EntrySignal,
		Payload:     payload,
	})
	if err != nil {
		return nil, err
	}
	return &adopted, nil
}

// advertisedToolNames returns the committed advertised tool-name set of one
// capture: the single advertisement authority of the model and tool
// boundaries.
func advertisedToolNames(capture ExecutionCapture) map[string]bool {
	names := make(map[string]bool, len(capture.Tools))
	for _, tool := range capture.Tools {
		names[tool.Name] = true
	}
	return names
}

// normalizeCallArguments runs one NormalizeTool invocation at the Harness
// boundary: the callback receives an owned copy of the completed call and
// must return exactly one complete JSON object, whose bytes the Harness
// returns as a fresh owned copy. A callback error or a malformed result is a
// per-call validation failure.
func normalizeCallArguments(normalize func(model.ToolCall) (json.RawMessage, error), call model.ToolCall) (json.RawMessage, error) {
	owned, err := model.NewToolCall(call)
	if err != nil {
		return nil, err
	}
	raw, err := normalize(owned)
	if err != nil {
		return nil, err
	}
	if _, err := decodePayloadObject(raw); err != nil {
		return nil, fmt.Errorf("%w: normalization result is not one complete JSON object", errMalformedNormalization)
	}
	return model.CloneRaw(raw), nil
}

// errMalformedNormalization marks a NormalizeTool result whose shape is not
// one complete JSON object: the existing internal-validation tool error
// class, not a plugin-defined status or a cancellation instruction.
var errMalformedNormalization = errors.New("malformed normalization")

// newAssistantEntry builds one assistant entry from one validated model
// output under the given entry identity, reserving one result identity per
// completed call. An output without an eligible model-visible payload writes
// no assistant entry and returns its reported usage for the consuming
// settlement entry instead. Before the entry commits, every advertised
// completed call is normalized through the execution's pure callback: its
// successful owned object populates the nested call's normalized_arguments
// member; a failed or malformed normalization, and every unknown or
// unadvertised name, leave the member absent while the original raw argument
// bytes are always retained.
func newAssistantEntry(sessionID, operationID, entryID string, out *model.Output, normalize func(model.ToolCall) (json.RawMessage, error), advertised map[string]bool) (*assistantEntry, *UsageCount, error) {
	if out == nil {
		return nil, nil, nil
	}
	entry := &assistantEntry{
		SessionID:   sessionID,
		EntryID:     entryID,
		OperationID: operationID,
		Status:      out.Status,
		Source:      out.Source,
	}
	var usage *UsageCount
	if out.Usage != nil {
		usage = &UsageCount{
			InputTokens:       int64(out.Usage.InputTokens),
			CachedInputTokens: int64(out.Usage.CachedInputTokens),
			OutputTokens:      int64(out.Usage.OutputTokens),
		}
	}
	if out.Message != nil {
		entry.Content = out.Message.Content
		entry.Refusal = out.Message.Refusal
		entry.Extra = out.Message.Extra
		for i, call := range out.Message.ToolCalls {
			record := toolCallRecord{
				ID:              call.ID,
				Ordinal:         int64(i),
				Name:            call.Name,
				ArgumentsBase64: base64.StdEncoding.EncodeToString(call.Arguments),
				Extra:           call.Extra,
			}
			if advertised[call.Name] {
				if normalized, err := normalizeCallArguments(normalize, call); err == nil {
					record.NormalizedArguments = normalized
				}
			}
			entry.ToolCalls = append(entry.ToolCalls, record)
		}
	}
	for i := range entry.ToolCalls {
		id, err := newHexID()
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrStorage, err)
		}
		entry.ToolCalls[i].ResultEntryID = id
	}
	if !assistantPayloadEligible(*entry) {
		return nil, usage, nil
	}
	entry.Usage = usage
	return entry, nil, nil
}

// validToolPlan reports whether one prepared plan has the contract shape:
// exactly one immediate result or executor.
func validToolPlan(p PreparedTool) bool {
	return (p.Immediate == nil) != (p.Execute == nil)
}

// authorizationRequired reports whether one shape-valid plan may produce an
// effect and therefore requires successfully evaluated declarations: an
// executor-backed plan or an immediate success. Immediate error, denied and
// interrupted outcomes need no target declaration.
func authorizationRequired(p PreparedTool) bool {
	if p.Execute != nil {
		return true
	}
	return p.Immediate.Result.Status == model.ResultSuccess
}

// toolCallAllowed decides one prepared plan at the Harness permission
// boundary: every declared pair must be a nonempty permission/target pair;
// file.read/file.write targets must be canonical and their prepared canonical
// Workspace root must be a required canonical that is nonempty, absolute and
// lexically clean; a readonly Agent with no configured write_dir denies every
// file.write pair; any configured write_dir confines every file.write pair to
// the prepared canonical write directory through Rel containment
// independently of permission allow, requiring its canonical root as a second
// required canonical; and only a plan whose every declared pair allows under
// the fixed evaluator against the prepared canonical Workspace root is
// allowed.
func toolCallAllowed(policy PermissionPolicy, capture ExecutionCapture, plan PreparedTool) bool {
	if len(plan.Permissions) == 0 {
		return false
	}
	var hasFile, hasWrite bool
	for _, req := range plan.Permissions {
		if req.Permission == "" || req.Target == "" {
			return false
		}
		switch req.Permission {
		case permissionFileRead:
			hasFile = true
		case permissionFileWrite:
			hasFile, hasWrite = true, true
		}
		if (req.Permission == permissionFileRead || req.Permission == permissionFileWrite) && !canonicalPathValue(req.Target) {
			return false
		}
	}
	if hasFile && !canonicalPathValue(plan.CanonicalWorkspace) {
		return false
	}
	if hasWrite {
		if capture.Readonly && capture.WriteDir == "" {
			return false
		}
		if capture.WriteDir != "" {
			if !canonicalPathValue(plan.CanonicalWriteDir) {
				return false
			}
			for _, req := range plan.Permissions {
				if req.Permission == permissionFileWrite && !containsPath(plan.CanonicalWriteDir, req.Target) {
					return false
				}
			}
		}
	}
	return policy.callAllowed(plan.CanonicalWorkspace, plan.Permissions)
}

// canonicalPathValue reports whether one prepared canonical target or root is
// a nonempty, absolute, lexically clean path.
func canonicalPathValue(p string) bool {
	return p != "" && filepath.IsAbs(p) && filepath.Clean(p) == p
}

// invalidToolResult is the ordinary validation-error result an invalid plan or
// invalid returned outcome maps onto for the original call.
func invalidToolResult(callID string) model.ToolResult {
	return model.ToolResult{CallID: callID, Status: model.ResultError, Content: invalidToolResultContent}
}

// interruptedToolResult is the ordinary interrupted-before-execution result.
func interruptedToolResult(callID string) model.ToolResult {
	return model.ToolResult{CallID: callID, Status: model.ResultInterrupted, Content: interruptedToolResultContent}
}

// pendingToolCallRecord resolves one published call's pending reservation and
// its committed assistant tool-call record from the coordinator's validated
// view: the boundary consumes the durable record, never call data from model
// JSON.
func pendingToolCallRecord(c *coordinator, operationID, callID string) (PendingToolCall, toolCallRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		return PendingToolCall{}, toolCallRecord{}, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, c.graph.Session.Identity.SessionID)
	}
	var pending PendingToolCall
	found := false
	for _, candidate := range op.State.PendingToolCalls {
		if candidate.CallID == callID {
			pending, found = candidate, true
			break
		}
	}
	if !found {
		return PendingToolCall{}, toolCallRecord{}, invalidInput("operation %q has no pending call %q; every call settles exactly once", operationID, callID)
	}
	for _, entry := range c.graph.Entries {
		if entry.Envelope.ID != pending.AssistantEntry.EntryID || entry.Assistant == nil {
			continue
		}
		for _, record := range entry.Assistant.ToolCalls {
			if record.ID == callID {
				return pending, record, nil
			}
		}
	}
	return PendingToolCall{}, toolCallRecord{}, invalidInput("operation %q pending call %q has no committed assistant record", operationID, callID)
}

// unavailableToolResult is the ordinary unavailable-tool error result the
// advertisement gate settles for a name outside the committed advertised set.
func unavailableToolResult(callID, name string) model.ToolResult {
	return model.ToolResult{CallID: callID, Status: model.ResultError, Content: fmt.Sprintf("Tool %q is not available.", name)}
}

// permissionDeniedToolResult is the ordinary policy-denial result settled for
// the original call under its reserved identity: no effect began and no tool
// active intent was written.
func permissionDeniedToolResult(callID string) model.ToolResult {
	return model.ToolResult{CallID: callID, Status: model.ResultDenied, Content: permissionDeniedToolResultContent}
}

// validationToolResult is the immediate validation-error result for an
// invalid model argument: status error carrying the callback's own bounded
// useful diagnostic.
func validationToolResult(callID string, cause error) model.ToolResult {
	content := boundedToolDiagnostic(cause)
	if content == "" {
		content = invalidToolResultContent
	}
	return model.ToolResult{CallID: callID, Status: model.ResultError, Content: content}
}

// boundedToolDiagnostic renders one Harness-produced validation diagnostic
// within the durable diagnostic bound.
func boundedToolDiagnostic(cause error) string {
	msg := cause.Error()
	if len(msg) > maxToolDiagnosticBytes {
		msg = strings.ToValidUTF8(msg[:maxToolDiagnosticBytes], "")
	}
	return msg
}

// toolEffect encloses one concrete tool execution in the shared Harness tool
// boundary. In order: it resolves the call's pending reservation and committed
// assistant record, and pre-start cancellation wins before anything else; the
// advertisement gate rejects names outside the committed advertised tool set
// with the ordinary unavailable-tool error before any hook or preparer runs;
// the selected argument hooks then run in configured order, each as its own
// settled effect, and a settled hook chain hands the final committed value on;
// without hooks (or for an unhooked call) the original assistant's committed
// normalized arguments are the selected input, and only an invalid unhooked
// original is normalized again to obtain its useful validation diagnostic. It
// then prepares the plan outside locks, rejects invalid plan shapes with the
// existing internal-validation error, and evaluates every declaration through
// the fixed boundary — including immediate-success plans — before
// beginPendingCallEffect. A denial settles only that original call with status denied
// and no metadata, no concrete effect and no tool active intent; later calls
// still run. An allowed executor commits intent, executes once behind the
// cancellation-before-start gate, and commits one validated outcome — a
// returned real outcome wins a cancellation race.
func (h *Harness) toolEffect(c *coordinator, operationID string, exec Execution, capture ExecutionCapture) agent.ToolEffect {
	prepared := exec.Tool
	advertised := advertisedToolNames(capture)
	return func(ctx context.Context, call model.ToolCall) (model.ToolResult, error) {
		pending, record, err := pendingToolCallRecord(c, operationID, call.ID)
		if err != nil {
			return model.ToolResult{}, err
		}
		settleCtx := context.WithoutCancel(h.ctx)
		if ctx.Err() != nil { // execution cancellation prevents later preparation: interrupted-before-execution
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: interruptedToolResult(call.ID)}, false)
		}
		if !advertised[record.Name] { // the one advertisement gate, ahead of every hook and preparer
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: unavailableToolResult(call.ID, record.Name)}, false)
		}
		// The selected normalized value feeding preparation: the final
		// committed hook replacement when hooks ran, otherwise the original
		// assistant's committed successful result. Both were already validated
		// and owned; neither is ever re-normalized. Only an absent (invalid)
		// unhooked original is normalized again, solely to obtain its useful
		// validation diagnostic.
		selected := record.NormalizedArguments
		if len(exec.ToolHooks) > 0 {
			final, settled, herr := h.runArgumentHooks(ctx, settleCtx, c, operationID, pending, record, exec)
			if herr != nil {
				return model.ToolResult{}, herr
			}
			if settled != nil { // the hook chain settled the call itself; remaining hooks are skipped and later calls continue
				return *settled, nil
			}
			selected = final
		} else if len(selected) == 0 {
			raw, derr := base64.StdEncoding.DecodeString(record.ArgumentsBase64)
			if derr != nil { // a non-canonical durable record is corruption, not empty arguments
				return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: invalidToolResult(call.ID)}, false)
			}
			normalized, nerr := normalizeCallArguments(exec.NormalizeTool, model.ToolCall{ID: record.ID, Name: record.Name, Arguments: raw, Extra: record.Extra})
			if nerr != nil {
				if errors.Is(nerr, errMalformedNormalization) { // a malformed callback outcome stays the internal-validation error
					return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: invalidToolResult(call.ID)}, false)
				}
				return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: validationToolResult(call.ID, nerr)}, false)
			}
			selected = normalized
		}
		// The preparer receives an owned copy of the selected normalized call
		// and nothing else.
		normalizedCall, err := model.NewToolCall(model.ToolCall{ID: record.ID, Name: record.Name, Arguments: selected, Extra: record.Extra})
		if err != nil {
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: invalidToolResult(call.ID)}, false)
		}
		plan := prepared(ctx, normalizedCall)
		if !validToolPlan(plan) {
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: invalidToolResult(call.ID)}, false)
		}
		if authorizationRequired(plan) && !toolCallAllowed(exec.Permissions, capture, plan) {
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: permissionDeniedToolResult(call.ID)}, false)
		}
		if plan.Immediate != nil {
			outcome := *plan.Immediate
			if _, err := model.NewToolResult(outcome.Result); err != nil || outcome.Result.CallID != call.ID {
				outcome = ToolOutcome{Result: invalidToolResult(call.ID)}
			}
			return h.commitToolResult(settleCtx, c, operationID, pending, outcome, false)
		}
		if _, err := h.beginPendingCallEffect(ctx, c, operationID, call.ID, EffectTool, ""); err != nil {
			// An intent transaction aborted by cancellation settles the
			// cancellation outcome: interrupted-before-execution, never a
			// cancellation-shaped error out of an active effect.
			if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() != nil {
				return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: interruptedToolResult(call.ID)}, false)
			}
			return model.ToolResult{}, err
		}
		if ctx.Err() != nil { // cancellation before start commits interrupted-before-execution
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: interruptedToolResult(call.ID)}, true)
		}
		outcome := plan.Execute(ctx)
		if _, err := model.NewToolResult(outcome.Result); err != nil || outcome.Result.CallID != call.ID {
			outcome = ToolOutcome{Result: invalidToolResult(call.ID)}
		}
		// the real outcome wins a cancellation race: the settlement transaction
		// runs without cancellation and publishes what the execution produced
		return h.commitToolResult(settleCtx, c, operationID, pending, outcome, true)
	}
}

// beginPendingCallEffect commits the one active-effect intent of a
// pending-call effect: the intent addresses the first pending call and
// reserves its result identity — the call's reserved identity for a tool
// effect, a fresh allocation for a hook — recording the active effect with
// the effect kind and, for hooks, the hook identity. Effects never nest, so
// any committed intent of another effect rejects.
func (h *Harness) beginPendingCallEffect(ctx context.Context, c *coordinator, operationID, callID string, kind EffectKind, hookID string) (string, error) {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return "", fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	if op.State.Status != OperationRunning {
		c.mu.Unlock()
		return "", invalidInput("operation %q is %s; a %s effect requires a running Operation", operationID, op.State.Status, kind)
	}
	if op.State.ActiveEffect != nil {
		c.mu.Unlock()
		return "", invalidInput("operation %q already carries an active effect; effects never nest", operationID)
	}
	if len(op.State.PendingToolCalls) == 0 || op.State.PendingToolCalls[0].CallID != callID {
		c.mu.Unlock()
		return "", invalidInput("operation %q does not pending-start with call %q; a %s effect addresses the first pending call", operationID, callID, kind)
	}
	resultID := op.State.PendingToolCalls[0].ResultEntryID
	if kind == EffectHook {
		fresh, err := newHexID()
		if err != nil {
			c.mu.Unlock()
			return "", fmt.Errorf("%w: %v", ErrStorage, err)
		}
		resultID = fresh
	}
	sessionID := op.Admission.SessionID
	var updated OperationRecord
	err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		key := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID}
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		current, err := decodeOperationRegister(reg)
		if err != nil {
			return corruptSession(sessionID, "operation register %q: %v", operationID, err)
		}
		// a violated semantic precondition outranks the conflict class
		if current.State.Status != OperationRunning {
			return invalidInput("operation %q is %s; a %s effect requires a running Operation", operationID, current.State.Status, kind)
		}
		if current.State.ActiveEffect != nil {
			return invalidInput("operation %q already carries an active effect; effects never nest", operationID)
		}
		if len(current.State.PendingToolCalls) == 0 || current.State.PendingToolCalls[0].CallID != callID {
			return invalidInput("operation %q does not pending-start with call %q; a %s effect addresses the first pending call", operationID, callID, kind)
		}
		if reg.Revision != op.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, op.Revision, reg.Revision)
		}
		next := current
		next.State.ActiveEffect = &ActiveEffect{Kind: kind, ResultEntryID: resultID, ToolCallID: callID, HookID: hookID}
		payload, err := encodeOperationRegister(next)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
		if err != nil {
			return err
		}
		updated = next
		updated.Revision = replaced.Revision
		return nil
	})
	if err != nil {
		h.markCorrupt(sessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return "", rerr
			}
		}
		return "", err
	}
	c.graph.replaceOperation(operationID, updated)
	c.mu.Unlock()
	return resultID, nil
}

// commitToolResult commits the one terminal result of a settled call through
// the ordinary tool-result transition: the result entry under the call's
// reserved identity, the cleared active effect, and the complete next
// Operation state in one transaction. The outcome's metadata is committed
// only when it is one well-formed JSON value within the durable bound;
// invalid or oversized metadata is dropped while the result still commits.
// Immediate plans commit without an effect intent; executor-backed plans
// commit behind theirs.
func (h *Harness) commitToolResult(ctx context.Context, c *coordinator, operationID string, pending PendingToolCall, outcome ToolOutcome, fromIntent bool) (model.ToolResult, error) {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return model.ToolResult{}, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	sessionID := op.Admission.SessionID
	viewSession := c.graph.Session
	var (
		updated       OperationRecord
		committedSess SessionRecord
		newEntries    []graphEntry
	)
	err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		sessionKey := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
		sreg, err := tx.ReadRegister(sessionKey)
		if err != nil {
			return err
		}
		currentSession, err := decodeSessionRegister(sreg)
		if err != nil {
			return corruptSession(sessionID, "session register: %v", err)
		}
		opKey := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID}
		oreg, err := tx.ReadRegister(opKey)
		if err != nil {
			return err
		}
		currentOp, err := decodeOperationRegister(oreg)
		if err != nil {
			return corruptSession(sessionID, "operation register %q: %v", operationID, err)
		}
		// violated semantic preconditions outrank the conflict class
		if currentOp.State.Status != OperationRunning {
			return invalidInput("operation %q is %s; a tool result requires a running Operation", operationID, currentOp.State.Status)
		}
		if fromIntent {
			if currentOp.State.ActiveEffect == nil || currentOp.State.ActiveEffect.Kind != EffectTool ||
				currentOp.State.ActiveEffect.ToolCallID != pending.CallID ||
				currentOp.State.ActiveEffect.ResultEntryID != pending.ResultEntryID {
				return invalidInput("operation %q carries no committed tool effect intent for call %q", operationID, pending.CallID)
			}
		} else if currentOp.State.ActiveEffect != nil {
			return invalidInput("operation %q carries an active effect; an immediate result requires a quiet Operation", operationID)
		}
		index := -1
		for i := range currentOp.State.PendingToolCalls {
			if currentOp.State.PendingToolCalls[i].CallID == pending.CallID {
				index = i
				break
			}
		}
		if index < 0 {
			return invalidInput("operation %q has no pending call %q; every call settles exactly once", operationID, pending.CallID)
		}
		reservation := currentOp.State.PendingToolCalls[index]
		if sreg.Revision != viewSession.Revision {
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, viewSession.Revision, sreg.Revision)
		}
		if oreg.Revision != op.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, op.Revision, oreg.Revision)
		}
		entry := toolResultEntry{
			SessionID:      sessionID,
			EntryID:        reservation.ResultEntryID,
			OperationID:    operationID,
			AssistantEntry: reservation.AssistantEntry,
			ToolCallID:     pending.CallID,
			Status:         outcome.Result.Status,
			Content:        outcome.Result.Content,
			Metadata:       ownedToolMetadata(outcome.Metadata),
		}
		payload, err := encodeToolResultEntry(entry)
		if err != nil {
			return err
		}
		adopted, err := insertAndAdopt(tx, sessionID, EntryDraft{
			SessionID:   sessionID,
			ID:          entry.EntryID,
			OperationID: operationID,
			Kind:        EntryToolResult,
			Payload:     payload,
		})
		if err != nil {
			return err
		}
		newEntries = append(newEntries, adopted)
		next := currentOp
		next.State.ActiveEffect = nil
		next.State.PendingToolCalls = append(next.State.PendingToolCalls[:index], next.State.PendingToolCalls[index+1:]...)
		opPayload, err := encodeOperationRegister(next)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(opKey, oreg.Revision, opPayload)
		if err != nil {
			return err
		}
		next.Revision = replaced.Revision
		committedSess = SessionRecord{Identity: currentSession.Identity, State: currentSession.State}
		sessionPayload, err := encodeSessionRegister(committedSess)
		if err != nil {
			return err
		}
		replacedSession, err := tx.ReplaceRegister(sessionKey, sreg.Revision, sessionPayload)
		if err != nil {
			return err
		}
		committedSess.Revision = replacedSession.Revision
		updated = next
		return nil
	})
	if err != nil {
		h.markCorrupt(sessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return model.ToolResult{}, rerr
			}
		}
		return model.ToolResult{}, err
	}
	c.graph.Entries = append(c.graph.Entries, newEntries...)
	c.graph.replaceOperation(operationID, updated)
	c.graph.Session = committedSess
	c.mu.Unlock()
	return outcome.Result, nil
}

// runArgumentHooks settles one call's selected argument-hook chain before
// concrete preparation: each hook runs as its own effect in configured order,
// the first receiving the original raw call arguments and every later one the
// preceding committed replacement. Each successful replacement is validated
// and normalized exactly once and its committed consequence returned to the
// synchronous continuation; a failed or interrupted hook settles the tool
// call itself (remaining hooks are skipped, later calls continue). It returns
// the final committed value, or the committed tool result when the chain
// settled the call, or the error a failed intent/result transaction leaves
// for normal recovery.
func (h *Harness) runArgumentHooks(ctx, settleCtx context.Context, c *coordinator, operationID string, pending PendingToolCall, record toolCallRecord, exec Execution) (json.RawMessage, *model.ToolResult, error) {
	raw, err := base64.StdEncoding.DecodeString(record.ArgumentsBase64)
	if err != nil { // a non-canonical durable record is corruption, not empty arguments
		settled, serr := h.settleHooklessInvalid(settleCtx, c, operationID, pending)
		return nil, settled, serr
	}
	current := raw
	for _, hook := range exec.ToolHooks {
		committed, settled, hookErr := h.runOneHook(ctx, settleCtx, c, operationID, pending, record, hook, exec.NormalizeTool, current)
		if hookErr != nil || settled != nil {
			return nil, settled, hookErr
		}
		current = committed
	}
	return current, nil, nil
}

// runOneHook settles one argument hook as its own Operation effect: it
// commits the hook active-effect intent with a fresh reserved result
// identity, runs the hook outside locks and transactions, and commits the
// validated consequence — a successful normalized replacement (a real result
// even when cancellation arrives before its commit), a hook error, or an
// interruption under the classification fixed for every hook.
func (h *Harness) runOneHook(ctx, settleCtx context.Context, c *coordinator, operationID string, pending PendingToolCall, record toolCallRecord, hook ToolArgumentsHook, normalize func(model.ToolCall) (json.RawMessage, error), current json.RawMessage) (json.RawMessage, *model.ToolResult, error) {
	resultID, err := h.beginPendingCallEffect(ctx, c, operationID, pending.CallID, EffectHook, hook.ID)
	if err != nil {
		// An intent transaction aborted by cancellation committed no
		// reservation, so no hook result exists: the call settles the existing
		// interrupted-before-execution result through the ordinary no-intent
		// transition, preserving earlier real hook results and preventing
		// later starts.
		if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() != nil {
			outcome := ToolOutcome{Result: interruptedToolResult(pending.CallID)}
			if _, terr := h.commitToolResult(settleCtx, c, operationID, pending, outcome, false); terr != nil {
				return nil, nil, terr
			}
			return nil, &outcome.Result, nil
		}
		return nil, nil, err
	}
	if ctx.Err() != nil { // cancellation between the committed intent and Run: interrupted hook, no replay
		settled, serr := h.settleHookInterruption(settleCtx, c, operationID, pending, hook.ID, resultID)
		return nil, settled, serr
	}
	owned, err := model.NewToolCall(model.ToolCall{ID: record.ID, Name: record.Name, Arguments: current, Extra: record.Extra})
	if err != nil { // the committed record's identity fields are validated; never reachable for a valid graph
		settled, serr := h.settleHookError(settleCtx, c, operationID, pending, hook.ID, resultID, invalidToolResult(record.ID))
		return nil, settled, serr
	}
	replacement, runErr := hook.Run(ctx, owned)
	if runErr != nil {
		// For Run's error return, a cancellation-shaped error with a done
		// execution context is interruption; every other returned error is a
		// hook error — no per-hook error policies.
		if (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded)) && ctx.Err() != nil {
			settled, serr := h.settleHookInterruption(settleCtx, c, operationID, pending, hook.ID, resultID)
			return nil, settled, serr
		}
		settled, serr := h.settleHookError(settleCtx, c, operationID, pending, hook.ID, resultID, validationToolResult(record.ID, runErr))
		return nil, settled, serr
	}
	if _, serr := decodePayloadObject(replacement); serr != nil { // an invalid returned value is a hook error of the malformed class
		settled, derr := h.settleHookError(settleCtx, c, operationID, pending, hook.ID, resultID, invalidToolResult(record.ID))
		return nil, settled, derr
	}
	// Normalize/default the replacement exactly once before committing the
	// successful hook consequence.
	normalized, nerr := normalizeCallArguments(normalize, model.ToolCall{ID: record.ID, Name: record.Name, Arguments: replacement, Extra: record.Extra})
	if nerr != nil {
		if errors.Is(nerr, errMalformedNormalization) {
			settled, serr := h.settleHookError(settleCtx, c, operationID, pending, hook.ID, resultID, invalidToolResult(record.ID))
			return nil, settled, serr
		}
		settled, serr := h.settleHookError(settleCtx, c, operationID, pending, hook.ID, resultID, validationToolResult(record.ID, nerr))
		return nil, settled, serr
	}
	// The successful normalized output is a real result even if cancellation
	// arrives before its commit: the settlement transaction runs without
	// cancellation.
	committed, cerr := h.commitHookResult(settleCtx, c, operationID, pending, hook.ID, resultID, hookResultEntry{Status: hookSucceeded, Arguments: normalized})
	if cerr != nil {
		return nil, nil, cerr
	}
	return committed, nil, nil
}

// settleHooklessInvalid settles the call's fixed internal-validation result
// through the ordinary no-intent transition, for shapes that never ran a hook.
func (h *Harness) settleHooklessInvalid(ctx context.Context, c *coordinator, operationID string, pending PendingToolCall) (*model.ToolResult, error) {
	result := invalidToolResult(pending.CallID)
	if _, err := h.commitToolResult(ctx, c, operationID, pending, ToolOutcome{Result: result}, false); err != nil {
		return nil, err
	}
	return &result, nil
}

// settleHookInterruption commits the interrupted hook_result under the
// effect's reserved identity and settles the pending call interrupted through
// the existing interruption helpers; the Operation's terminal interruption
// settles through the ordinary run settlement. No hook or tool replays.
func (h *Harness) settleHookInterruption(ctx context.Context, c *coordinator, operationID string, pending PendingToolCall, hookID, resultID string) (*model.ToolResult, error) {
	if _, err := h.commitHookResult(ctx, c, operationID, pending, hookID, resultID, hookResultEntry{Status: hookInterrupted, Error: executionInterruptedDetail}); err != nil {
		return nil, err
	}
	result := interruptedToolResult(pending.CallID)
	if _, err := h.commitToolResult(ctx, c, operationID, pending, ToolOutcome{Result: result}, false); err != nil {
		return nil, err
	}
	return &result, nil
}

// settleHookError commits the error hook_result under the effect's reserved
// identity and then the ordinary error result for the tool call. Both carry
// the result's already-bounded, nonempty diagnostic.
func (h *Harness) settleHookError(ctx context.Context, c *coordinator, operationID string, pending PendingToolCall, hookID, resultID string, outcome model.ToolResult) (*model.ToolResult, error) {
	if _, err := h.commitHookResult(ctx, c, operationID, pending, hookID, resultID, hookResultEntry{Status: hookFailed, Error: outcome.Content}); err != nil {
		return nil, err
	}
	if _, err := h.commitToolResult(ctx, c, operationID, pending, ToolOutcome{Result: outcome}, false); err != nil {
		return nil, err
	}
	return &outcome, nil
}

// commitHookResult commits one settled hook execution's result transaction:
// the hook_result entry under the effect's reserved identity plus the cleared
// active effect with the first call still pending, atomically. On success it
// returns the committed arguments as the codec-owned copy of the exact stored
// bytes — the one consequence the synchronous continuation consumes; every
// failure returns no consequence and leaves the committed running intent for
// normal recovery.
func (h *Harness) commitHookResult(ctx context.Context, c *coordinator, operationID string, pending PendingToolCall, hookID, resultID string, res hookResultEntry) (json.RawMessage, error) {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	sessionID := op.Admission.SessionID
	viewSession := c.graph.Session
	var (
		updated       OperationRecord
		committedSess SessionRecord
		newEntries    []graphEntry
		committed     json.RawMessage
	)
	err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		sessionKey := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
		sreg, err := tx.ReadRegister(sessionKey)
		if err != nil {
			return err
		}
		currentSession, err := decodeSessionRegister(sreg)
		if err != nil {
			return corruptSession(sessionID, "session register: %v", err)
		}
		opKey := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID}
		oreg, err := tx.ReadRegister(opKey)
		if err != nil {
			return err
		}
		currentOp, err := decodeOperationRegister(oreg)
		if err != nil {
			return corruptSession(sessionID, "operation register %q: %v", operationID, err)
		}
		// violated semantic preconditions outrank the conflict class
		if currentOp.State.Status != OperationRunning {
			return invalidInput("operation %q is %s; a hook result requires a running Operation", operationID, currentOp.State.Status)
		}
		if currentOp.State.ActiveEffect == nil || currentOp.State.ActiveEffect.Kind != EffectHook ||
			currentOp.State.ActiveEffect.HookID != hookID ||
			currentOp.State.ActiveEffect.ToolCallID != pending.CallID ||
			currentOp.State.ActiveEffect.ResultEntryID != resultID {
			return invalidInput("operation %q carries no committed hook effect intent for hook %q and call %q", operationID, hookID, pending.CallID)
		}
		if sreg.Revision != viewSession.Revision {
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, viewSession.Revision, sreg.Revision)
		}
		if oreg.Revision != op.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, op.Revision, oreg.Revision)
		}
		res.SessionID = sessionID
		res.EntryID = resultID
		res.OperationID = operationID
		res.HookID = hookID
		res.ToolCallID = pending.CallID
		payload, err := encodeHookResultEntry(res)
		if err != nil {
			return err
		}
		adopted, err := insertAndAdopt(tx, sessionID, EntryDraft{
			SessionID:   sessionID,
			ID:          res.EntryID,
			OperationID: operationID,
			Kind:        EntryHookResult,
			Payload:     payload,
		})
		if err != nil {
			return err
		}
		newEntries = append(newEntries, adopted)
		committed = model.CloneRaw(adopted.HookResult.Arguments) // the codec-owned stored bytes, cloned once for the continuation
		next := currentOp
		next.State.ActiveEffect = nil
		opPayload, err := encodeOperationRegister(next)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(opKey, oreg.Revision, opPayload)
		if err != nil {
			return err
		}
		next.Revision = replaced.Revision
		committedSess = SessionRecord{Identity: currentSession.Identity, State: currentSession.State}
		sessionPayload, err := encodeSessionRegister(committedSess)
		if err != nil {
			return err
		}
		replacedSession, err := tx.ReplaceRegister(sessionKey, sreg.Revision, sessionPayload)
		if err != nil {
			return err
		}
		committedSess.Revision = replacedSession.Revision
		updated = next
		return nil
	})
	if err != nil {
		h.markCorrupt(sessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return nil, rerr
			}
		}
		return nil, err
	}
	c.graph.Entries = append(c.graph.Entries, newEntries...)
	c.graph.replaceOperation(operationID, updated)
	c.graph.Session = committedSess
	c.mu.Unlock()
	return committed, nil
}

// execute is the private agent.Run composition of one admitted execution:
// after the commit it invokes the preparation's opener exactly once with the
// execution context and the owned committed admission, runs the Agent over
// the opened effects, then the outer terminal settlement converges the
// durable state with the run's outcome, and a non-nil resource cleanup runs
// once after that settlement attempt — before the slot releases or the next
// buffered delivery starts. The execution context is the installed run's
// execCtx: Interrupt cancels it, and closure or a consumed interrupt marker
// settles the durable Operation as interruption at entry. The Agent's
// expected model and advertised tools
// come from an independent capture retained before the opener runs, so an
// opener mutating its admission input locally never changes what is
// advertised after admission. An opener error resolves through the ordinary
// terminal settlement: cancellation interrupts, a storage failure retains the
// running state for recovery, and any other error fails. A canceled
// execution skips an unstarted opener. A successful invalid Execution has
// its non-nil Close invoked before rejection, and no Agent runs with invalid
// effects. A cleanup failure never rewrites the terminal Operation; it is
// retained for Wait alongside the first storage failure.
func (h *Harness) execute(c *coordinator, operationID string, prepared PreparedExecution, execCtx context.Context) error {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	admission := ownOperationRecord(op).Admission
	agentCapture := ownCapture(admission.Execution)
	guard := c.bgState == bgClosed || c.interruptOp == operationID
	if c.interruptOp == operationID {
		c.interruptOp = "" // consumed: the marker reaches exactly the execution it names
	}
	c.mu.Unlock()
	if guard || h.ctx.Err() != nil { // the entry guard joins the harness-loss early return: the opener never starts
		err := h.ctx.Err()
		if err == nil {
			err = context.Canceled
		}
		return h.settleAgentTerminal(c, operationID, agent.TerminalResult{}, err)
	}
	exec, err := prepared.Open(execCtx, admission)
	if err != nil {
		return h.settleAgentTerminal(c, operationID, agent.TerminalResult{}, err)
	}
	if exec.Model == nil || exec.Tool == nil || exec.NormalizeTool == nil {
		if exec.Close != nil {
			h.recordCleanupFailure(exec.Close())
		}
		return h.settleAgentTerminal(c, operationID, agent.TerminalResult{},
			invalidInput("opened execution requires non-nil model, tool and normalization functions"))
	}
	seenHooks := make(map[string]bool, len(exec.ToolHooks))
	for _, hook := range exec.ToolHooks {
		if hook.ID == "" || hook.Run == nil || seenHooks[hook.ID] {
			if exec.Close != nil {
				h.recordCleanupFailure(exec.Close())
			}
			return h.settleAgentTerminal(c, operationID, agent.TerminalResult{},
				invalidInput("opened execution requires a non-empty unique ID and a non-nil run function for every argument hook"))
		}
		seenHooks[hook.ID] = true
	}
	if exec.Close != nil {
		defer func() { h.recordCleanupFailure(exec.Close()) }()
	}
	res, err := agent.Run(execCtx, agent.Invocation{
		ExpectedModel: agentCapture.Model,
		Tools:         agentCapture.Tools,
		Context:       h.contextSource(c, operationID),
		ModelEffect:   h.modelEffect(c, operationID, exec, agentCapture),
		ToolEffect:    h.toolEffect(c, operationID, exec, agentCapture),
	})
	return h.settleAgentTerminal(c, operationID, res, err)
}

// settleAgentTerminal is the outer terminal settlement: terminal settlement
// after opening or running the Agent reuses the common terminal helper only
// when the Operation is still running, because model-effect-originated
// terminals already settled durably inside their own result transactions. A
// non-storage callback or Agent protocol error settles failure with the
// error's own text; an Agent terminal result preserves its non-empty detail.
// A committed running/intent state left by a publication failure stays for
// recovery.
func (h *Harness) settleAgentTerminal(c *coordinator, operationID string, res agent.TerminalResult, runErr error) error {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	settled := op.State.Status != OperationRunning
	quiet := op.State.ActiveEffect == nil
	c.mu.Unlock()
	if settled || !quiet { // already settled by a model effect, or a publication failure left the intent state for recovery
		return runErr
	}
	settleCtx := context.WithoutCancel(h.ctx)
	if runErr != nil {
		if errors.Is(runErr, ErrStorage) { // a storage-class error performs no compensating write: the committed running state stays for recovery
			return runErr
		}
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) { // between-effect cancellation settles the fixed interruption detail
			if _, err := h.commitEffectResult(settleCtx, c, operationID, nil, modelResult{
				terminal: OperationInterruption,
				detail:   executionInterruptedDetail,
			}); err != nil {
				return err
			}
			return runErr
		}
		if _, err := h.commitEffectResult(settleCtx, c, operationID, nil, modelResult{
			terminal: OperationFailure,
			detail:   runErr.Error(),
		}); err != nil {
			return err
		}
		return runErr
	}
	switch res.Status {
	case agent.TerminalSuccess:
		if _, err := h.commitEffectResult(settleCtx, c, operationID, nil, modelResult{terminal: OperationSuccess}); err != nil {
			return err
		}
	case agent.TerminalFailure, agent.TerminalInterruption:
		term := OperationFailure
		if res.Status == agent.TerminalInterruption {
			term = OperationInterruption
		}
		if _, err := h.commitEffectResult(settleCtx, c, operationID, nil, modelResult{terminal: term, detail: res.Detail}); err != nil {
			return err
		}
	}
	return nil
}
