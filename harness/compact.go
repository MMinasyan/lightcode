package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// serializeCompactionMessages renders the frozen projected conversation as
// the compaction input: one inert JSON line per message, in order. Each
// message is copied before any field is rewritten — a shallow struct copy
// shares the ToolCalls backing array, so the slice is copied too. The source
// identity is cleared on the copy only: it identifies the model for provider
// replay and is not part of the provider-visible message. Each copied tool
// call's raw argument bytes are replaced with the JSON encoding of their
// quoted byte representation, the one treatment that keeps arguments whose
// bytes are not valid JSON marshalable while retaining them verbatim; every
// other extra value is validated JSON at its accepting boundary. The graph
// and the request's owned messages are never mutated.
func serializeCompactionMessages(messages []model.Message) ([]string, error) {
	lines := make([]string, 0, len(messages))
	for i := range messages {
		copied := messages[i]
		copied.Source = model.ModelRef{}
		if len(copied.ToolCalls) > 0 {
			copied.ToolCalls = append(make([]model.ToolCall, 0, len(copied.ToolCalls)), copied.ToolCalls...)
			for j := range copied.ToolCalls {
				quoted, err := json.Marshal(fmt.Sprintf("%q", []byte(copied.ToolCalls[j].Arguments)))
				if err != nil {
					return nil, err
				}
				copied.ToolCalls[j].Arguments = quoted
			}
		}
		line, err := json.Marshal(copied)
		if err != nil {
			return nil, err
		}
		lines = append(lines, string(line))
	}
	return lines, nil
}

// maintenanceFacts is the process-local carrier of one maintenance
// procedure's logical facts until their existing checkpoint or terminal
// publication: the actual tool-free logical Request messages the effect
// received (retained the moment the effect gets its request, never invented
// before it), and the effective contribution evidence the procedure consumed
// at its frozen canonical boundary. Reported usage rides the existing
// settlement or checkpoint usage members; this carrier is neither a separate
// model-result entry nor a crash-survival guarantee before that commit.
type maintenanceFacts struct {
	input           []model.Message
	consumed        RequestEvidence
	sourceRequestID string // the conversation request whose frozen view the maintenance procedure consumed; empty only for manual compaction
}

func (f *maintenanceFacts) hasInput() bool { return f != nil && len(f.input) > 0 }

func (f *maintenanceFacts) hasConsumed() bool { return f != nil && f.consumed.BoundaryEntryID != "" }

// compactModelEffect encloses the compact summary request in one logical
// model-effect intent/result pair — a sibling of modelEffect closing over the
// compact transport and the orchestration's reported-usage slot. The shared
// active-effect intent transaction commits the same durable intent carrying
// the maintenance producer's own request identity, the captured compact
// model, and the maintenance purpose — never the suspended conversation's
// identity — and publishes the supplied suspended conversation request state
// inside that existing transition when none is published yet. The in-memory
// intent's expected identity is the compact model, the assembly source the
// agent's callback validates and the terminal usage model, because the summary
// runs on the compact transport while the durable active-effect state is
// model-agnostic, and the intent is marked as compaction so its terminal usage
// carries summary provenance. The standard attempt-retry loop runs through the
// compact transport, and the assembled output's reported usage — nil-
// preserving, a reported zero stays known zero — lands on the slot before that
// output settles. A completed output settles by the empty running result — no
// entry, the active effect cleared, both registers replaced with no usage, the
// reserved identity unused — and returns the ready settlement whose output
// carries the summary; the orchestration validates the summary text after the
// settlement. A summary failure and an interruption settle through the effect's
// own terminal settlement with the slot's current usage keyed by the compact
// model and the retained maintenance input, returning the assembled output
// carried on the failure and interruption rows — the conversation effect's own
// shape, and the run's callback gate requires it whenever the assembly
// callback ran — while pre-assembly failures and cancellations return no
// output. The invocation's callbacks come from the opened maintenance
// invocation; its retry classifier is the enclosing Operation's.
func (h *Harness) compactModelEffect(c *coordinator, operationID string, inv InvocationExecution, retry RetryPolicy, capture ExecutionCapture, maintenanceID string, facts *maintenanceFacts, suspended *RunningRequest, reported **UsageCount) agent.ModelEffect {
	attempt := inv.Model
	return func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		// The actual tool-free logical maintenance request is retained on the
		// carrier the moment the effect receives it, deep-owned.
		facts.input = cloneRequestMessages(req.Messages)
		intent, err := h.beginModelEffect(ctx, c, operationID, maintenanceID, InvocationPurposeMaintenance, capture.Compact.Model, nil, suspended)
		if err != nil {
			// An intent transaction aborted by cancellation settles the
			// cancellation outcome: the run never sees a cancellation-shaped
			// error out of an active effect.
			if isExecutionCancellation(err) && ctx.Err() != nil {
				if _, cerr := h.commitEffectResult(context.WithoutCancel(h.ctx), c, operationID, nil, modelResult{
					terminal:      OperationInterruption,
					detail:        executionInterruptedDetail,
					usage:         *reported,
					usageModel:    capture.Compact.Model,
					compaction:    true,
					facts:         facts,
					maintenanceID: maintenanceID,
				}); cerr != nil {
					return agent.ModelSettlement{}, cerr
				}
				return agent.ModelSettlement{Disposition: agent.DispoInterruption, Detail: executionInterruptedDetail}, nil
			}
			return agent.ModelSettlement{}, err
		}
		// The durable intent transaction is the conversation model effect's
		// own; only the in-memory intent's expected identity is the compact
		// model's, and its provenance is compaction.
		// Execution cancellation stops new callbacks: a context that died
		// between the committed intent and the callback settles the Operation
		// as terminal interruption without ever starting the callback.
		if ctx.Err() != nil {
			return h.interruptModelEffect(c, operationID, intent, *reported, facts)
		}
		// Result and terminal transactions run without cancellation so an
		// already-produced result or required terminal settlement publishes.
		settleCtx := context.WithoutCancel(h.ctx)
		settle := func(cause error) (agent.ModelSettlement, error) {
			if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
				terminal:      OperationFailure,
				detail:        failureDetail(cause),
				usage:         *reported,
				facts:         facts,
				maintenanceID: maintenanceID,
			}); err != nil {
				return agent.ModelSettlement{}, err
			}
			return agent.ModelSettlement{}, cause
		}
		// The one shared attempt loop lives inside the committed intent,
		// identical to the conversation model effect's but running through the
		// compact transport. The summary consumes the maintenance request
		// exactly as the maintenance procedure planned it.
		var output model.Output
		switch res := runModelAttempts(ctx, req, attempt, retry); res.kind {
		case attemptAccepted:
			out, attemptErr := assemble(intent.expected, h.observeStream(intent.sessionID, operationID, res.stream)) // exactly one assembly after acceptance; the enclosing Operation owns the deltas
			if attemptErr != nil {
				return settle(attemptErr)
			}
			output = out
		case attemptInterrupted:
			return h.interruptModelEffect(c, operationID, intent, *reported, facts)
		case attemptInvalid:
			return settle(res.err)
		case attemptFailed:
			committed := agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: failureDetail(res.err)}
			if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
				terminal:      OperationFailure,
				detail:        failureDetail(res.err),
				usage:         *reported,
				facts:         facts,
				maintenanceID: maintenanceID,
			}); err != nil {
				return agent.ModelSettlement{}, err
			}
			return committed, nil
		}
		// The assembled output's reported usage lands on the orchestration's
		// slot before that output settles; an output reporting no usage leaves
		// the slot untouched.
		if output.Usage != nil {
			usage := UsageCount{
				InputTokens:       int64(output.Usage.InputTokens),
				CachedInputTokens: int64(output.Usage.CachedInputTokens),
				OutputTokens:      int64(output.Usage.OutputTokens),
			}
			*reported = &usage
		}
		if output.Status == model.OutputCompleted {
			// The empty running result inserts no entry, clears the active
			// effect, and replaces both registers with no usage; the reserved
			// result identity stays unused. The summary text is the
			// orchestration's validation, made after this ready settlement.
			if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{}); err != nil {
				return agent.ModelSettlement{}, err
			}
			return agent.ModelSettlement{Disposition: agent.DispoReady, Output: &output}, nil
		}
		// A summary failure and an interruption settle through the effect's
		// own terminal settlement, consuming the reserved identity. The
		// assembled output rides the returned settlement — an errored output
		// on the failure row, an interrupted one on the interruption row —
		// while a pre-assembly failure or cancellation returns none.
		terminal := OperationFailure
		disposition := agent.DispoFailure
		if output.Status == model.OutputInterrupted {
			terminal = OperationInterruption
			disposition = agent.DispoInterruption
		}
		if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
			terminal:      terminal,
			detail:        output.Detail,
			usage:         *reported,
			facts:         facts,
			maintenanceID: maintenanceID,
		}); err != nil {
			return agent.ModelSettlement{}, err
		}
		return agent.ModelSettlement{Disposition: disposition, Detail: output.Detail, Output: &output}, nil
	}
}

// runCompaction summarizes the frozen projected conversation through the
// no-Session compact agent. The snapshot — the complete canonical projected
// conversation minus the leading system message, supplied by the caller — is
// serialized into inert transcript lines and joined whole into one user
// message: the complete input is sent as one logical full-input request,
// never split, and request contributions are never appended to it. The
// orchestration opens one maintenance invocation for the model request it
// sends, with a fresh per-call producer identity, the maintenance purpose,
// and the captured compact model; an invocation the opener rejects cleans its
// own provisional resources. The always-returned non-nil closer disposes the
// opened maintenance invocation exactly once on the retained cleanup path: for
// a final maintenance stage the caller runs it only after the
// checkpoint/terminal transaction has committed — for a failure whose terminal
// already settled inside the orchestration the caller runs it immediately. The
// fixed no-Session Agent runs once: the captured compact prompt as the system
// message, the serialized conversation as the user message, the compact model
// effect over the opened invocation's model callback, and a tool boundary that
// rejects any hallucinated dispatch. The orchestration's reported-usage slot
// is set by the effect from the assembled output before that output settles
// and read after the run returns: a nil slot means the summary reported no
// usage, and a reported zero stays known zero. The summary candidate, that
// usage, the maintenance producer identity, and the facts carrier are returned
// to the caller — the commit is the caller's. A textless completed summary
// fails the orchestration after its ready settlement. A summary failure or
// interruption settles durably inside its own effect; a run-level terminal or
// any other failure — a pre-run cancellation, a cancellation observed after
// the settled summary result, an empty snapshot, an empty summary, an error
// after the settled result — settles directly on the quiet running Operation
// with the slot's usage keyed by the compact model. No compaction entry is
// written on any failure path, and a failure before the actual logical
// request reaches the effect boundary invents no input fact.
func (h *Harness) runCompaction(execCtx context.Context, c *coordinator, operationID string, exec Execution, capture ExecutionCapture, snapshot []model.Message, suspended *RunningRequest, facts *maintenanceFacts) (string, *UsageCount, string, *maintenanceFacts, func() error, error) {
	closeInvocation := func() error { return nil } // a stage that opened no invocation disposes nothing
	if facts == nil {
		facts = &maintenanceFacts{}
	}
	if suspended != nil {
		// Automatic maintenance records the conversation request its frozen
		// input view belongs to before the maintenance opener runs, so its
		// facts stay associated with that request even when the producer
		// identity is minted later or never minted at all.
		facts.sourceRequestID = suspended.RequestID
	}
	if len(snapshot) == 0 {
		return "", nil, "", facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, errors.New("nothing to compact"), capture, nil, "", facts)
	}
	lines, err := serializeCompactionMessages(snapshot)
	if err != nil {
		return "", nil, "", facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, err, capture, nil, "", facts)
	}
	maintenanceID, err := newHexID() // the maintenance Engine call's fresh producer identity
	if err != nil {
		return "", nil, "", facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, fmt.Errorf("%w: %v", ErrStorage, err), capture, nil, "", facts)
	}
	inv, err := exec.OpenInvocation(execCtx, InvocationRequest{
		ID:      maintenanceID,
		Purpose: InvocationPurposeMaintenance,
		Model:   capture.Compact.Model,
	})
	if err != nil {
		// An opener failure observed under a genuinely canceled execution is
		// deliberate control cancellation and settles interruption with the
		// fixed detail; a local opener timeout under a live execution context
		// stays a failure carrying its own diagnostic.
		terminal := OperationFailure
		if isExecutionCancellation(err) && execCtx.Err() != nil {
			terminal = OperationInterruption
			err = errors.New(executionInterruptedDetail)
		}
		return "", nil, maintenanceID, facts, closeInvocation, h.settleCompactionFailure(c, operationID, terminal, err, capture, nil, maintenanceID, facts)
	}
	if err := validateOpenedInvocation(inv); err != nil {
		if inv.Close != nil {
			h.recordCleanupFailure(inv.Close()) // an invalid invocation cleans its own opened resources
		}
		return "", nil, maintenanceID, facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, err, capture, nil, maintenanceID, facts)
	}
	closeInvocation = func() error { // the opened maintenance invocation disposes on the retained cleanup path
		if inv.Close != nil {
			h.recordCleanupFailure(inv.Close())
		}
		return nil
	}
	var reported *UsageCount
	effect := h.compactModelEffect(c, operationID, inv, exec.Retry, capture, maintenanceID, facts, suspended, &reported)
	res, runErr := agent.Run(execCtx, agent.Invocation{
		ExpectedModel: capture.Compact.Model,
		Request: func(context.Context) (model.Request, error) {
			return model.Request{Messages: []model.Message{
				{Role: model.RoleSystem, Content: []model.ContentPart{{Kind: model.PartText, Text: capture.Compact.SystemPrompt}}},
				{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: strings.Join(lines, "\n")}}},
			}}, nil
		},
		ModelEffect: effect,
		ToolEffect: func(context.Context, model.ToolCall, []string) (model.ToolResult, error) {
			return model.ToolResult{}, &agent.ProtocolError{Boundary: "tool", Detail: "compact agent has no tools"}
		},
	})
	if runErr != nil {
		return "", reported, maintenanceID, facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, runErr, capture, reported, maintenanceID, facts)
	}
	switch res.Status {
	case agent.TerminalSuccess:
		summary := res.LastOutput.Message.TextContent()
		if summary == "" {
			// A textless completed summary — including refusal-only —
			// fails the orchestration after its ready settlement: no
			// retry, the direct settlement carrying the retained detail
			// and the reported usage.
			return "", reported, maintenanceID, facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, errors.New("compaction summary is empty"), capture, reported, maintenanceID, facts)
		}
		return summary, reported, maintenanceID, facts, closeInvocation, nil
	default:
		// A run-level terminal — a pre-run cancellation, or a cancellation
		// observed after the settled summary result before the run's
		// success return — leaves the Operation running and quiet with no
		// settlement; the direct settlement covers it, and takes no
		// compensating write when the effect already settled the same
		// terminal.
		terminal := OperationFailure
		if res.Status == agent.TerminalInterruption {
			terminal = OperationInterruption
		}
		return "", reported, maintenanceID, facts, closeInvocation, h.settleCompactionFailure(c, operationID, terminal, errors.New(res.Detail), capture, reported, maintenanceID, facts)
	}
}

// settleCompactionFailure is the direct terminal settlement of one
// orchestration failure or run-level terminal on the quiet running Operation:
// the given terminal state and non-empty detail commit with the reported
// summary usage keyed by the compact model and marked as compaction usage,
// and the retained maintenance facts — the actual input once the effect
// received its request and the consumed contribution evidence — settle on
// this same transaction exactly once. An Operation already settled by the
// summary effect, or one whose committed intent state a publication failure
// left for recovery, takes no compensating write, and a failed settlement
// adds no guessed follow-up write. This is the one owner of the settlement
// attempt for these failures; callers never retry it.
func (h *Harness) settleCompactionFailure(c *coordinator, operationID string, terminal OperationState, cause error, capture ExecutionCapture, usage *UsageCount, maintenanceID string, facts *maintenanceFacts) error {
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
	if settled || !quiet {
		return cause
	}
	if _, err := h.commitEffectResult(context.WithoutCancel(h.ctx), c, operationID, nil, modelResult{
		terminal:      terminal,
		detail:        failureDetail(cause),
		usage:         usage,
		usageModel:    capture.Compact.Model,
		compaction:    true,
		facts:         facts,
		maintenanceID: maintenanceID,
	}); err != nil {
		return err
	}
	return cause
}

// commitCompaction commits the one atomic compaction transaction: the
// immutable compaction entry with its maintenance producer identity, actual
// tool-free input, and consumed contribution evidence, the Session register's
// projection field, and the usage totals on the Operation and Session
// registers — plus, on the manual shape, the dedicated Operation's success
// settlement — all inside one transaction on the cancellation-free Harness
// context. A published suspended conversation request state is advanced in
// this same transaction to the new checkpoint entry and its rebuilt
// projection tail, never replaced by the maintenance producer's identity. The
// entry's boundary names the last entry the frozen snapshot covered — the
// snapshotLen-th projectable entry after the prior compaction's own boundary
// target, derived under the coordinator lock the commit holds across the
// transaction from the cached graph and the pre-commit register — so an entry
// appended after the freeze, whose sequence is strictly higher than every
// freeze-time entry, can never be named as covered. The usage contribution
// lands keyed by the compact model on the entry and both register totals in
// the same transaction. A failed transaction leaves every previous value
// durable and settles the quiet running Operation exactly once through the
// direct terminal settlement carrying the commit error, the reported usage
// keyed by the compact model, and the retained maintenance facts; the caller
// performs no second settlement attempt.
func (h *Harness) commitCompaction(c *coordinator, operationID string, capture ExecutionCapture, maintenanceID string, facts *maintenanceFacts, summary string, snapshotLen int, usage *UsageCount, manual bool) error {
	c.mu.Lock()
	viewOp, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	sessionID := viewOp.Admission.SessionID
	viewSession := c.graph.Session
	namedCompactionID := viewSession.State.CompactionEntryID

	var (
		committedOp   OperationRecord
		committedSess SessionRecord
		newEntries    []graphEntry
	)
	err := h.deps.Storage.Transact(context.WithoutCancel(h.ctx), func(tx Transaction) error {
		// The boundary: the last entry the frozen snapshot covered.
		boundary := compactionBoundary(c.graph.Entries, namedCompactionID, snapshotLen)
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
		// a violated semantic precondition outranks the conflict class
		if currentOp.State.Status != OperationRunning {
			return invalidInput("operation %q is %s; compaction commits to a running Operation", operationID, currentOp.State.Status)
		}
		if sreg.Revision != viewSession.Revision {
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, viewSession.Revision, sreg.Revision)
		}
		if oreg.Revision != viewOp.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, viewOp.Revision, oreg.Revision)
		}

		entryID, err := newHexID()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrStorage, err)
		}
		consumed := RequestEvidence{CheckpointEntryID: namedCompactionID, BoundaryEntryID: boundary}
		consumedSource := ""
		if facts != nil {
			consumedSource = facts.sourceRequestID
		}
		if facts != nil && facts.hasConsumed() {
			consumed = facts.consumed // the effective contribution evidence the procedure consumed
		}
		var input []model.Message
		if facts.hasInput() {
			input = facts.input // the actual tool-free logical Request the effect consumed
		}
		payload, err := encodeCompactionEntry(compactionEntry{
			SessionID:             sessionID,
			EntryID:               entryID,
			OperationID:           operationID,
			Summary:               summary,
			BoundaryEntryID:       boundary,
			Model:                 capture.Compact.Model,
			ConfigurationRevision: capture.ConfigurationRevision,
			Usage:                 usage,
			RequestID:             maintenanceID,
			ConsumedRequestID:     consumedSource,
			Input:                 input,
			Evidence:              &consumed,
		})
		if err != nil {
			return err
		}
		adopted, err := insertAndAdopt(tx, sessionID, EntryDraft{
			SessionID:   sessionID,
			ID:          entryID,
			OperationID: operationID,
			Kind:        EntryCompaction,
			Payload:     payload,
		})
		if err != nil {
			return err
		}
		newEntries = append(newEntries, adopted)

		// The orchestration's reported total lands keyed by the compact
		// model on the entry and both register totals.
		var contribution UsageTotals
		if usage != nil {
			contribution = UsageTotals{ByModel: []ModelUsage{{Model: capture.Compact.Model, Usage: *usage}}}
		}
		opUsage, err := addUsageTotals(currentOp.State.Usage, contribution)
		if err != nil {
			return err
		}
		sessionUsage, err := addUsageTotals(currentSession.State.Usage, contribution)
		if err != nil {
			return err
		}

		nextOp := currentOp
		nextOp.State.Usage = opUsage
		if nextOp.State.Request != nil {
			// The checkpoint transition advances the suspended conversation
			// request state to the complete rebuilt projection pair — the
			// new checkpoint AND the rebuilt view's canonical tail boundary
			// — in this same transaction, so the state names exactly the
			// consumed projection the request source rebuilds after it.
			advanced := *nextOp.State.Request
			advanced.CheckpointEntryID = entryID
			advanced.BoundaryEntryID = entryID // summary-only rebuilt projection names the checkpoint as its tail
			nextOp.State.Request = &advanced
		}
		opPayload, err := encodeOperationRegister(nextOp)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(opKey, oreg.Revision, opPayload)
		if err != nil {
			return err
		}
		nextOp.Revision = replaced.Revision

		nextSess := currentSession
		nextSess.State.CompactionEntryID = entryID
		nextSess.State.Usage = sessionUsage
		committedSess = SessionRecord{Identity: currentSession.Identity, State: nextSess.State}
		sessionPayload, err := encodeSessionRegister(committedSess)
		if err != nil {
			return err
		}
		replacedSession, err := tx.ReplaceRegister(sessionKey, sreg.Revision, sessionPayload)
		if err != nil {
			return err
		}
		committedSess.Revision = replacedSession.Revision

		// The manual shape settles the dedicated Operation as success inside
		// the same transaction; the settlement itself carries no usage
		// because the compaction part already added the totals, and the
		// checkpoint owns the maintenance facts, so the settlement carries
		// none.
		if manual {
			settledOp, settledSess, settledEntries, err := commitTerminalSettlement(tx, sessionID, operationID, committedSess, nextOp, replacedSession.Revision, replaced.Revision, OperationSuccess, "", nil, model.ModelRef{}, nil, false, nil, "")
			if err != nil {
				return err
			}
			nextOp = settledOp
			committedSess = settledSess
			newEntries = append(newEntries, settledEntries...)
		}
		committedOp = nextOp
		return nil
	})
	if err != nil {
		h.markCorrupt(sessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(context.WithoutCancel(h.ctx), c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return rerr
			}
		}
		return h.settleCompactionFailure(c, operationID, OperationFailure, err, capture, usage, maintenanceID, facts)
	}
	c.graph.Entries = append(c.graph.Entries, newEntries...)
	c.graph.replaceOperation(operationID, committedOp)
	c.graph.Session = committedSess
	c.mu.Unlock()
	h.observeInvalidation(c) // every compaction commit replaces the Session register
	return nil
}

// findCompactionCutoff resolves the named compaction's payload and cutoff
// over one validated Session graph — the single lookup behind both the
// context projection and the compaction boundary. An empty named ID is the
// uncompacted history: no payload and no cutoff. A nonempty named ID's
// references are guaranteed by graph validation — the register names an
// in-session compaction entry and that entry's boundary names an in-session
// entry strictly before it — so the lookup returns that payload and the
// boundary target's sequence; a missing persisted reference is corruption
// rejected before this lookup runs.
func findCompactionCutoff(entries []graphEntry, namedID string) (*compactionEntry, int64) {
	if namedID == "" {
		return nil, -1
	}
	for i := range entries {
		if entries[i].Envelope.ID != namedID || entries[i].Compaction == nil {
			continue
		}
		for j := range entries {
			if entries[j].Envelope.ID == entries[i].Compaction.BoundaryEntryID {
				return entries[i].Compaction, entries[j].Envelope.Sequence
			}
		}
	}
	return nil, -1
}

// compactionBoundary names the last entry one frozen compaction snapshot
// covered: the need-th projectable entry strictly after the prior
// compaction's own boundary target, where need is the snapshot length minus
// the prior summary message the snapshot carries as its first covered
// message. Without a named prior compaction the count runs from the graph's
// start; a summary-only re-compaction (the snapshot is the prior summary
// alone) names the prior compaction entry itself, a valid compaction-kind
// boundary. The named cutoff comes from findCompactionCutoff over the same
// validated graph. A count shortfall returns the empty boundary: unreachable
// when the snapshot derives from the same graph's projectable entries, and
// the payload's hex rule rejects it inside the commit transaction as the
// ordinary failure.
func compactionBoundary(entries []graphEntry, namedCompactionID string, snapshotLen int) string {
	payload, cutoff := findCompactionCutoff(entries, namedCompactionID)
	need := snapshotLen
	if payload != nil {
		need-- // the prior summary the snapshot carries as its first covered message
	}
	if need == 0 {
		return namedCompactionID
	}
	count := 0
	for i := range entries {
		e := entries[i]
		if e.Input == nil && e.Assistant == nil && e.ToolResult == nil && e.Signal == nil {
			continue
		}
		if e.Envelope.Sequence <= cutoff {
			continue
		}
		count++
		if count == need {
			return e.Envelope.ID
		}
	}
	return ""
}
