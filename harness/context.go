package harness

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// contextSource returns the Agent context boundary of one Operation: the
// first call projects the newly admitted input only, and every later call
// first attempts the steering handoff — the one per-boundary decision point
// that quietly settles the predecessor through ordinary admission once
// eligible steering waits after a complete tool batch — then projects the
// fresh model.Message history. Physical retries and compact-piece context
// callbacks never reach this boundary.
func (h *Harness) contextSource(c *coordinator, run *activeExecution, operationID string) agent.ContextSource {
	firstRequest := true
	return func(ctx context.Context) ([]model.Message, error) {
		if !firstRequest { // first-request protection: the admitted input owns the first receiving request
			handed, err := h.attemptSteeringHandoff(c, run, operationID)
			if err != nil {
				return nil, err
			}
			if handed { // the Agent exits through its existing cancellation contract
				if cerr := run.execCtx.Err(); cerr != nil {
					return nil, cerr
				}
				return nil, context.Canceled
			}
		}
		firstRequest = false
		return h.projectContext(c, operationID)
	}
}

// projectContext is the pure projection body: it snapshots the cached graph
// under the coordinator lock, releases it, and builds the messages without
// touching steering or queued buffers. When the Session register's
// CompactionEntryID is set, the projection is the system message, one
// assistant summary message built from the named compaction entry's payload,
// and the entries after the sequence of the boundary entry that the named
// entry's payload records — the named entry and every entry at or before that
// boundary never project as messages. With the field empty the full history
// projects.
func (h *Harness) projectContext(c *coordinator, operationID string) ([]model.Message, error) {
	c.mu.Lock()
	sessionID := c.graph.Session.Identity.SessionID
	op, ok := c.graph.Operation(operationID)
	if !ok {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	entries := c.graph.Entries
	systemPrompt := op.Admission.Execution.SystemPrompt
	compactionID := c.graph.Session.State.CompactionEntryID
	c.mu.Unlock()

	messages := make([]model.Message, 0, len(entries)+1)
	if systemPrompt != "" {
		msg, err := model.NewMessage(model.Message{
			Role:    model.RoleSystem,
			Content: []model.ContentPart{{Kind: model.PartText, Text: systemPrompt}},
		})
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	// The named compaction's payload supplies the one summary message and
	// its boundary target's sequence is boundarySequence, at or before which
	// no entry projects; -1 means no compaction boundary and the full
	// history projects.
	compaction, boundarySequence := findCompactionCutoff(entries, compactionID)
	if compaction != nil {
		msg, err := model.NewMessage(model.Message{
			Role:    model.RoleAssistant,
			Source:  compaction.Model,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\n" + compaction.Summary + "\n\n[End of summary. Continue from here.]"}},
		})
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	for _, entry := range entries {
		if entry.Envelope.Sequence <= boundarySequence {
			continue
		}
		msg, ok, err := projectEntry(entry)
		if err != nil {
			return nil, err
		}
		if ok {
			messages = append(messages, msg)
		}
	}
	return messages, nil
}

// projectEntry maps one committed entry to its one model message under the
// kind-to-message mapping. An operation settlement and a hook result produce
// no model message — hook results are execution evidence, never conversation
// messages — and a compaction entry never materializes either: the register's
// CompactionEntryID carries its summary into the projection instead.
func projectEntry(entry graphEntry) (model.Message, bool, error) {
	switch {
	case entry.Input != nil:
		msg, err := model.NewMessage(model.Message{
			Role:    model.RoleUser,
			Content: entry.Input.Content,
		})
		return msg, true, err
	case entry.Assistant != nil:
		calls := make([]model.ToolCall, 0, len(entry.Assistant.ToolCalls))
		for _, call := range entry.Assistant.ToolCalls {
			raw, err := base64.StdEncoding.DecodeString(call.ArgumentsBase64)
			if err != nil {
				return model.Message{}, false, fmt.Errorf("entry %s: tool call %q arguments: %w", entry.Envelope.ID, call.ID, err)
			}
			calls = append(calls, model.ToolCall{ID: call.ID, Name: call.Name, Arguments: raw, Extra: call.Extra})
		}
		msg, err := model.NewMessage(model.Message{
			Role:      model.RoleAssistant,
			Source:    entry.Assistant.Source,
			Content:   entry.Assistant.Content,
			Refusal:   entry.Assistant.Refusal,
			Extra:     entry.Assistant.Extra,
			ToolCalls: calls,
		})
		return msg, true, err
	case entry.ToolResult != nil:
		msg, err := model.NewMessage(model.Message{
			Role:       model.RoleTool,
			ToolCallID: entry.ToolResult.ToolCallID,
			Content:    []model.ContentPart{{Kind: model.PartText, Text: entry.ToolResult.Content}},
		})
		return msg, true, err
	case entry.Signal != nil:
		msg, err := model.NewMessage(model.Message{
			Role:    model.RoleUser,
			Content: []model.ContentPart{{Kind: model.PartText, Text: signalProjectedText(entry.Signal.Content)}},
		})
		return msg, true, err
	default:
		return model.Message{}, false, nil
	}
}

// signalProjectedText renders one signal's fixed content inside the retained
// system-signal text wrapper; the typed signal payload stays the canonical
// origin and subtype authority.
func signalProjectedText(content string) string {
	return "<system-signal>" + strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(content) + "</system-signal>"
}

// attemptSteeringHandoff is the one private handoff action at a conversation
// boundary: it checks for eligible steering without taking a reservation, and
// when steering waits acquires the Session's admission reservation on the
// predecessor execution context, rechecks eligibility under the coordinator
// hold, and publishes the predecessor's quiet success through the shared
// result-transaction body under that same hold. The publication settles
// OperationSuccess with empty detail and no assistant, signal or usage; its
// release closure is carried on the execution until the retiring run's
// cleanup and the shared post-terminal drain have installed a successor, so
// no second reservation is ever acquired and no other admission can overtake
// the selected handoff. A decline or a reserve wait aborted by the
// predecessor's cancellation writes nothing and releases immediately; a
// publication failure releases the reservation and propagates its original
// error, never retried and never reclassified.
func (h *Harness) attemptSteeringHandoff(c *coordinator, run *activeExecution, operationID string) (bool, error) {
	c.mu.Lock()
	pending := len(c.steering) > 0
	c.mu.Unlock()
	if !pending { // no reservation on unrelated boundaries
		return false, nil
	}
	release, err := h.reserve(run.execCtx, c)
	if err != nil { // the predecessor died while waiting: the ordinary cancellation path owns the run
		return false, nil
	}
	c.mu.Lock()
	if !steeringHandoffEligible(h, c, run, operationID) {
		c.mu.Unlock()
		release() // a declined handoff writes nothing and releases immediately
		return false, nil
	}
	sessionID, _, entries, err := h.commitEffectResultLocked(context.WithoutCancel(h.ctx), c, operationID, nil, modelResult{terminal: OperationSuccess})
	if err != nil {
		c.mu.Unlock()
		release()                            // a failed publication releases the handoff reservation
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(context.WithoutCancel(h.ctx), c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return false, rerr
			}
		}
		return false, err
	}
	run.handoffRelease = release // retained until the execution's cleanup and the shared drain finish
	c.mu.Unlock()
	h.observeInvalidation(c) // the durable quiet-success settlement
	h.emitToolResultFacts(entries, sessionID, operationID)
	run.cancel() // only this predecessor, after the commit
	return true, nil
}

// steeringHandoffEligible is the handoff's recheck under the coordinator hold
// with the reservation already acquired: the execution is still installed,
// its message Operation is the current running quiet Operation with no
// pending calls, steering remains pending, the lifecycle is open, the group
// is not permanently closed — a root stop's stopping state never blocks the
// handoff — both contexts are live, and no interrupt marker names this
// Operation.
func steeringHandoffEligible(h *Harness, c *coordinator, run *activeExecution, operationID string) bool {
	if c.run != run { // that execution is no longer installed
		return false
	}
	if h.ctx.Err() != nil || run.execCtx.Err() != nil { // both contexts must be live
		return false
	}
	if c.interruptOp == operationID { // a matching marker hands the run to the ordinary interruption path
		return false
	}
	if c.graph.Session.State.Lifecycle != LifecycleOpen || c.bgState == bgClosed {
		return false
	}
	if c.graph.Session.State.CurrentOperationID != operationID || len(c.steering) == 0 {
		return false
	}
	op, ok := c.graph.Operation(operationID)
	if !ok || op.Admission.RequestKind != RequestKindMessage {
		return false
	}
	return op.State.Status == OperationRunning && op.State.ActiveEffect == nil && len(op.State.PendingToolCalls) == 0
}

// Interrupt interrupts one Session's execution, keyed on the durable state:
// an idle Session — no current Operation — returns nil. Otherwise both
// interrupt vehicles fire: the installed execution's context is canceled, and
// the not-yet-started executions' marker is set to the durable current
// Operation. The marker is consumed at execute's entry when it matches that
// operation — a marker for a running operation is inert — and a handoff
// declines under a matching marker so the ordinary interruption path owns the
// run. Buffers are never discarded by an interrupt: unselected items drain
// after the interrupted Operation's terminal.
func (h *Harness) Interrupt(ctx context.Context, sessionID string) error {
	c, err := h.coordinatorFor(ctx, sessionID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	operationID := c.graph.Session.State.CurrentOperationID
	if operationID == "" {
		c.mu.Unlock()
		return nil
	}
	c.interruptOp = operationID
	run := c.run
	c.mu.Unlock()
	if run != nil {
		run.cancel()
	}
	return nil
}
