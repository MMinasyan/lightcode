package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

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
// intent and reserved result identity, invokes the prepared function outside
// locks and storage transactions behind the privately tracked assembly
// callback, validates the settlement once, and commits the result and complete
// next Operation state before returning the committed settlement.
func (h *Harness) modelEffect(c *coordinator, operationID string, prepared agent.ModelEffect) agent.ModelEffect {
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
		calls := 0
		var callbackErr error
		tracked := func(source model.ModelRef, stream model.Stream) (model.Output, error) {
			calls++
			out, err := assemble(source, stream)
			if err != nil && callbackErr == nil {
				callbackErr = err // any callback error stays fatal even when the prepared function ignores it
			}
			return out, err
		}
		set, effectErr := prepared(ctx, req, tracked)
		if effectErr != nil {
			return settle(effectErr)
		}
		if callbackErr != nil {
			return settle(callbackErr)
		}
		if set.Output != nil && calls != 1 {
			return settle(&agent.ProtocolError{Boundary: "model", Detail: fmt.Sprintf("settlement carries an output from %d assembly callback invocations, exactly one successful call is required", calls)})
		}
		if set.Output == nil && calls != 0 {
			return settle(&agent.ProtocolError{Boundary: "model", Detail: fmt.Sprintf("settlement carries no output but the assembly callback ran %d times, zero are required", calls)})
		}
		owned, err := agent.ValidateModelSettlement(intent.expected, set)
		if err != nil {
			return settle(err)
		}
		// Only the wrapper's own steering decision may produce a
		// completed-output continue, and only for waiting steering; a prepared
		// callback returning that combination is invalid.
		if owned.Disposition == agent.DispoContinue && owned.Output != nil && owned.Output.Status == model.OutputCompleted {
			return settle(&agent.ProtocolError{Boundary: "model", Detail: "prepared model callback returned a completed-output continue; only the Harness-owned steering decision may produce it"})
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
		assistant, reported, err := newAssistantEntry(intent.sessionID, operationID, intent.resultID, owned.Output)
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
	if ptr, ok := c.graph.operationIndex()[operationID]; ok { // the index's pointers alias the view's slice
		*ptr = updated
	}
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
	if ptr, ok := c.graph.operationIndex()[operationID]; ok { // the index's pointers alias the view's slice
		*ptr = committedOp
	}
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
		RelatedOperation: operationRef{SessionID: sessionID, OperationID: operationID},
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

// normalizedArguments reports one call's raw argument payload as its durable
// normalized form when it is exactly one valid non-null JSON value — the rule
// the codecs enforce on stored normalized arguments.
func normalizedArguments(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || !json.Valid(trimmed) || bytes.Equal(trimmed, []byte("null")) {
		return nil, false
	}
	return trimmed, true
}

// newAssistantEntry builds one assistant entry from one validated model
// output under the given entry identity, reserving one result identity per
// completed call. An output without an eligible model-visible payload writes
// no assistant entry and returns its reported usage for the consuming
// settlement entry instead.
func newAssistantEntry(sessionID, operationID, entryID string, out *model.Output) (*assistantEntry, *UsageCount, error) {
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
			if normalized, ok := normalizedArguments(call.Arguments); ok {
				record.NormalizedArguments = normalized
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
// exactly one immediate result or executor, and normalized arguments that are
// nil or one valid JSON value.
func validToolPlan(p PreparedTool) bool {
	if (p.Immediate == nil) == (p.Execute == nil) { // both or neither
		return false
	}
	if len(p.NormalizedArguments) > 0 && !json.Valid(p.NormalizedArguments) {
		return false
	}
	return true
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

// pendingToolCall resolves one published call's pending reservation from the
// coordinator's validated view.
func pendingToolCall(c *coordinator, operationID, callID string) (PendingToolCall, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		return PendingToolCall{}, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, c.graph.Session.Identity.SessionID)
	}
	for _, pending := range op.State.PendingToolCalls {
		if pending.CallID == callID {
			return pending, nil
		}
	}
	return PendingToolCall{}, invalidInput("operation %q has no pending call %q; every call settles exactly once", operationID, callID)
}

// toolEffect encloses one concrete tool execution in the Harness effect
// boundary: it resolves the call's reserved result identity, prepares the plan
// outside locks, and commits exactly one terminal result through the ordinary
// tool-result transition. An immediate plan commits without an effect intent;
// an executor-backed plan commits intent, executes once behind the
// cancellation-before-start gate, and commits one validated outcome — a
// returned real outcome wins a cancellation race.
func (h *Harness) toolEffect(c *coordinator, operationID string, prepared func(context.Context, model.ToolCall) PreparedTool) agent.ToolEffect {
	return func(ctx context.Context, call model.ToolCall) (model.ToolResult, error) {
		pending, err := pendingToolCall(c, operationID, call.ID)
		if err != nil {
			return model.ToolResult{}, err
		}
		settleCtx := context.WithoutCancel(h.ctx)
		if ctx.Err() != nil { // execution cancellation prevents later preparation: interrupted-before-execution
			return h.commitToolResult(settleCtx, c, operationID, pending, interruptedToolResult(call.ID), false)
		}
		plan := prepared(ctx, call)
		if !validToolPlan(plan) {
			return h.commitToolResult(settleCtx, c, operationID, pending, invalidToolResult(call.ID), false)
		}
		if plan.Immediate != nil {
			outcome := *plan.Immediate
			if _, err := model.NewToolResult(outcome); err != nil || outcome.CallID != call.ID {
				outcome = invalidToolResult(call.ID)
			}
			return h.commitToolResult(settleCtx, c, operationID, pending, outcome, false)
		}
		if _, err := h.beginToolEffect(ctx, c, operationID, call.ID); err != nil {
			// An intent transaction aborted by cancellation settles the
			// cancellation outcome: interrupted-before-execution, never a
			// cancellation-shaped error out of an active effect.
			if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() != nil {
				return h.commitToolResult(settleCtx, c, operationID, pending, interruptedToolResult(call.ID), false)
			}
			return model.ToolResult{}, err
		}
		if ctx.Err() != nil { // cancellation before start commits interrupted-before-execution
			return h.commitToolResult(settleCtx, c, operationID, pending, interruptedToolResult(call.ID), true)
		}
		outcome := plan.Execute(ctx)
		if _, err := model.NewToolResult(outcome); err != nil || outcome.CallID != call.ID {
			outcome = invalidToolResult(call.ID)
		}
		// the real outcome wins a cancellation race: the settlement transaction
		// runs without cancellation and publishes what the execution produced
		return h.commitToolResult(settleCtx, c, operationID, pending, outcome, true)
	}
}

// beginToolEffect commits the one active-effect intent of an executor-backed
// tool effect: the intent addresses the first pending call and reserves its
// result identity.
func (h *Harness) beginToolEffect(ctx context.Context, c *coordinator, operationID, callID string) (string, error) {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return "", fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	if op.State.Status != OperationRunning {
		c.mu.Unlock()
		return "", invalidInput("operation %q is %s; a tool effect requires a running Operation", operationID, op.State.Status)
	}
	if op.State.ActiveEffect != nil {
		c.mu.Unlock()
		return "", invalidInput("operation %q already carries an active effect; effects never nest", operationID)
	}
	if len(op.State.PendingToolCalls) == 0 || op.State.PendingToolCalls[0].CallID != callID {
		c.mu.Unlock()
		return "", invalidInput("operation %q does not pending-start with call %q; a tool effect addresses the first pending call", operationID, callID)
	}
	resultID := op.State.PendingToolCalls[0].ResultEntryID
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
			return invalidInput("operation %q is %s; a tool effect requires a running Operation", operationID, current.State.Status)
		}
		if current.State.ActiveEffect != nil {
			return invalidInput("operation %q already carries an active effect; effects never nest", operationID)
		}
		if len(current.State.PendingToolCalls) == 0 || current.State.PendingToolCalls[0].CallID != callID {
			return invalidInput("operation %q does not pending-start with call %q; a tool effect addresses the first pending call", operationID, callID)
		}
		if reg.Revision != op.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, op.Revision, reg.Revision)
		}
		next := current
		next.State.ActiveEffect = &ActiveEffect{Kind: EffectTool, ResultEntryID: resultID, ToolCallID: callID}
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
	if ptr, ok := c.graph.operationIndex()[operationID]; ok { // the index's pointers alias the view's slice
		*ptr = updated
	}
	c.mu.Unlock()
	return resultID, nil
}

// commitToolResult commits the one terminal result of a settled call through
// the ordinary tool-result transition: the result entry under the call's
// reserved identity, the cleared active effect, and the complete next
// Operation state in one transaction. Immediate plans commit without an
// effect intent; executor-backed plans commit behind theirs.
func (h *Harness) commitToolResult(ctx context.Context, c *coordinator, operationID string, pending PendingToolCall, result model.ToolResult, fromIntent bool) (model.ToolResult, error) {
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
			Status:         result.Status,
			Content:        result.Content,
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
	if ptr, ok := c.graph.operationIndex()[operationID]; ok { // the index's pointers alias the view's slice
		*ptr = updated
	}
	c.graph.Session = committedSess
	c.mu.Unlock()
	return result, nil
}

// execute is the private agent.Run composition of one admitted execution: the
// three Agent boundaries run over the coordinator's validated state on the
// Harness context, then the outer terminal settlement converges the durable
// state with the run's outcome.
func (h *Harness) execute(c *coordinator, operationID string, prepared PreparedExecution) error {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	capture := op.Admission.Execution
	c.mu.Unlock()
	res, err := agent.Run(h.ctx, agent.Invocation{
		ExpectedModel: capture.Model,
		Tools:         capture.Tools,
		Context:       h.contextSource(c, operationID),
		ModelEffect:   h.modelEffect(c, operationID, prepared.Model),
		ToolEffect:    h.toolEffect(c, operationID, prepared.Tool),
	})
	return h.settleAgentTerminal(c, operationID, res, err)
}

// settleAgentTerminal is the outer terminal settlement: terminal settlement
// after agent.Run reuses the common terminal helper only when the Operation is
// still running, because model-effect-originated terminals already settled
// durably inside their own result transactions. A non-storage callback or
// Agent protocol error settles failure with the error's own text; an Agent
// terminal result preserves its non-empty detail. A committed running/intent
// state left by a publication failure stays for recovery.
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
