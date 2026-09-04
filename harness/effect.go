package harness

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// interruptedToolResultContent is the contract-fixed model-visible content of
// every interrupted-before-execution tool result.
const interruptedToolResultContent = "Tool call interrupted."

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
			return agent.ModelSettlement{}, err
		}
		// Result and terminal transactions run without cancellation so an
		// already-produced result or required terminal settlement publishes.
		settleCtx := context.WithoutCancel(h.ctx)
		settle := func(cause error) (agent.ModelSettlement, error) {
			if _, err := h.commitModelResult(settleCtx, c, operationID, intent, modelResult{
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
		if _, err := h.commitModelResult(settleCtx, c, operationID, intent, res); err != nil {
			return agent.ModelSettlement{}, err
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

// commitModelResult commits the one result transaction of a settled logical
// model effect: the semantic result entries, usage, the complete next
// Operation state, and the required Session state. A running result clears the
// active effect and records the completed calls' reservations; a terminal
// result runs the common terminal helper — preserving any produced assistant
// payload, interrupting every call that will not execute, appending exactly one
// interruption signal, appending the settlement entry, writing the terminal
// Operation, and clearing the Session current Operation — all atomically. A
// publication failure leaves the committed running/intent state for recovery.
func (h *Harness) commitModelResult(ctx context.Context, c *coordinator, operationID string, intent modelEffectIntent, res modelResult) (OperationRecord, error) {
	c.mu.Lock()
	viewOp, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := intent.sessionID
		c.mu.Unlock()
		return OperationRecord{}, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	viewSession := c.graph.Session

	var (
		committedOp   OperationRecord
		committedSess SessionRecord
		newEntries    []graphEntry
	)
	err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		sessionKey := RegisterKey{SessionID: intent.sessionID, Kind: RegisterSession}
		sreg, err := tx.ReadRegister(sessionKey)
		if err != nil {
			return err
		}
		currentSession, err := decodeSessionRegister(sreg)
		if err != nil {
			return corruptSession(intent.sessionID, "session register: %v", err)
		}
		if sreg.Revision != viewSession.Revision {
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, intent.sessionID, viewSession.Revision, sreg.Revision)
		}
		opKey := RegisterKey{SessionID: intent.sessionID, Kind: RegisterOperation, OperationID: operationID}
		oreg, err := tx.ReadRegister(opKey)
		if err != nil {
			return err
		}
		currentOp, err := decodeOperationRegister(oreg)
		if err != nil {
			return corruptSession(intent.sessionID, "operation register %q: %v", operationID, err)
		}
		// violated semantic preconditions outrank the conflict class
		if currentOp.State.Status != OperationRunning {
			return invalidInput("operation %q is %s; its model effect requires a running Operation", operationID, currentOp.State.Status)
		}
		if currentOp.State.ActiveEffect == nil || currentOp.State.ActiveEffect.Kind != EffectModel ||
			currentOp.State.ActiveEffect.ResultEntryID != intent.resultID {
			return invalidInput("operation %q carries no committed model effect intent for result identity %q", operationID, intent.resultID)
		}
		if sreg.Revision != viewSession.Revision {
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, intent.sessionID, viewSession.Revision, sreg.Revision)
		}
		if oreg.Revision != viewOp.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, viewOp.Revision, oreg.Revision)
		}

		// The transaction's usage contribution rides the produced assistant
		// entry, or the settlement entry when the terminal output reported
		// usage without an eligible payload; the same totals land on the
		// producer entry, the Operation, and the Session.
		var contribution UsageTotals
		switch {
		case res.assistant != nil && res.assistant.Usage != nil:
			contribution = UsageTotals{ByModel: []ModelUsage{{Model: res.assistant.Source, Usage: *res.assistant.Usage}}}
		case res.assistant == nil && res.usage != nil:
			contribution = UsageTotals{ByModel: []ModelUsage{{Model: intent.expected, Usage: *res.usage}}}
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
			adopted, err := insertAndAdopt(tx, intent.sessionID, EntryDraft{
				SessionID:   intent.sessionID,
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
					AssistantEntry: EntryRef{SessionID: intent.sessionID, EntryID: res.assistant.EntryID},
					CallID:         call.ID,
					ResultEntryID:  call.ResultEntryID,
				})
			}
		}

		// Terminal entries: one interrupted result under every remaining
		// committed tool-result identity whose call will not execute — a
		// terminal Operation leaves no unresolved call. Only the interruption
		// terminal adds its signal.
		if res.terminal != "" {
			remaining := append(append([]PendingToolCall{}, currentOp.State.PendingToolCalls...), pending...)
			for _, call := range remaining {
				result := toolResultEntry{
					SessionID:      intent.sessionID,
					EntryID:        call.ResultEntryID,
					OperationID:    operationID,
					AssistantEntry: call.AssistantEntry,
					ToolCallID:     call.CallID,
					Status:         model.ResultInterrupted,
					Content:        interruptedToolResultContent,
				}
				payload, err := encodeToolResultEntry(result)
				if err != nil {
					return err
				}
				adopted, err := insertAndAdopt(tx, intent.sessionID, EntryDraft{
					SessionID:   intent.sessionID,
					ID:          result.EntryID,
					OperationID: operationID,
					Kind:        EntryToolResult,
					Payload:     payload,
				})
				if err != nil {
					return err
				}
				newEntries = append(newEntries, adopted)
			}
		}

		// The interruption terminal appends exactly one interruption signal.
		signalKind := res.signal
		if res.terminal == OperationInterruption {
			signalKind = SignalInterruption
		}
		if signalKind != "" {
			entry, err := newSignalEntry(tx, intent.sessionID, operationID, signalKind)
			if err != nil {
				return err
			}
			newEntries = append(newEntries, *entry)
		}

		next := currentOp
		next.State.ActiveEffect = nil
		next.State.Usage = opUsage
		if res.terminal == "" {
			next.State.Status = OperationRunning
			next.State.PendingToolCalls = append(next.State.PendingToolCalls, pending...)
		} else {
			settlementID := intent.resultID
			if res.assistant != nil {
				if settlementID, err = newHexID(); err != nil {
					return fmt.Errorf("%w: %v", ErrStorage, err)
				}
			}
			settlement := operationSettlementEntry{
				SessionID:   intent.sessionID,
				EntryID:     settlementID,
				OperationID: operationID,
				Status:      res.terminal,
				Detail:      res.detail,
			}
			if res.assistant == nil && res.usage != nil { // terminal no-output model usage rides the settlement entry
				modelRef := intent.expected
				settlement.Model = &modelRef
				settlement.Usage = res.usage
			}
			payload, err := encodeOperationSettlementEntry(settlement)
			if err != nil {
				return err
			}
			adopted, err := insertAndAdopt(tx, intent.sessionID, EntryDraft{
				SessionID:   intent.sessionID,
				ID:          settlement.EntryID,
				OperationID: operationID,
				Kind:        EntryOperationSettlement,
				Payload:     payload,
			})
			if err != nil {
				return err
			}
			newEntries = append(newEntries, adopted)
			settled := adopted.Envelope.CommittedAt
			next.State.Status = res.terminal
			next.State.PendingToolCalls = []PendingToolCall{}
			next.State.SettledAt = &settled
			next.State.Terminal = &OperationTerminal{
				SettlementEntry: EntryRef{SessionID: intent.sessionID, EntryID: settlementID},
				Detail:          res.detail,
			}
		}
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
		if res.terminal != "" {
			sessionState.CurrentOperationID = ""
		}
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
		h.markCorrupt(intent.sessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(ctx, c, intent.sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
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
			entry.ToolCalls = append(entry.ToolCalls, toolCallRecord{
				ID:              call.ID,
				Ordinal:         int64(i),
				Name:            call.Name,
				ArgumentsBase64: base64.StdEncoding.EncodeToString(call.Arguments),
				Extra:           call.Extra,
			})
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
