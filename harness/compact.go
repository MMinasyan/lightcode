package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
// publication: the ordered per-request facts — the actual tool-free logical
// Request messages each model plan's effect consumed (retained the moment the
// effect gets its request, never invented before it), its producing model,
// and its optional reported usage — plus the effective contribution evidence
// the procedure consumed at its frozen canonical boundary and the producer
// identity of the current maintenance attempt. Reported usage rides the
// ordered facts on the existing settlement or checkpoint; this carrier is
// neither a separate model-result entry nor a crash-survival guarantee before
// that commit.
type maintenanceFacts struct {
	facts           []maintenanceFact
	consumed        RequestEvidence
	sourceRequestID string // the conversation request whose frozen view the maintenance procedure consumed; empty only for manual maintenance
	maintenanceID   string // the producer identity of the current maintenance attempt
}

func (f *maintenanceFacts) hasInput() bool { return f != nil && len(f.facts) > 0 }

func (f *maintenanceFacts) hasConsumed() bool { return f != nil && f.consumed.BoundaryEntryID != "" }

// witnessesRequest reports whether one retained fact already witnesses the
// named producing request, so its live producer is never repeated elsewhere.
func (f *maintenanceFacts) witnessesRequest(requestID string) bool {
	if f == nil || requestID == "" {
		return false
	}
	for _, fact := range f.facts {
		if fact.RequestID == requestID {
			return true
		}
	}
	return false
}

// factsList returns the carrier's ordered facts; a nil carrier holds none.
func factsList(f *maintenanceFacts) []maintenanceFact {
	if f == nil {
		return nil
	}
	return f.facts
}

// cloneMaintenanceFacts returns an independent owned copy of one ordered
// fact list: the input messages are deep-copied and the optional usage is
// copied value-wise.
func cloneMaintenanceFacts(facts []maintenanceFact) []maintenanceFact {
	if len(facts) == 0 {
		return nil
	}
	out := make([]maintenanceFact, len(facts))
	for i, fact := range facts {
		out[i] = fact
		out[i].Input = cloneRequestMessages(fact.Input)
		if fact.Usage != nil {
			usage := *fact.Usage
			out[i].Usage = &usage
		}
	}
	return out
}

// producer returns the procedure's own producer identity — the settlement
// and checkpoint witness — for a possibly-nil carrier.
func (f *maintenanceFacts) producer() string {
	if f == nil {
		return ""
	}
	return f.maintenanceID
}

// transferPending moves the carrier's uncommitted ordered facts to the
// active-execution owner — the coordinator's process-local pending list —
// before the completed invocation disposes. The facts stay uncommitted there
// until the next successful checkpoint or terminal publication consumes them;
// they are neither a separate result entry nor a crash-survival guarantee.
func (h *Harness) transferPending(c *coordinator, facts *maintenanceFacts) {
	if facts == nil || len(facts.facts) == 0 {
		return
	}
	c.mu.Lock()
	c.pendingFacts = append(c.pendingFacts, cloneMaintenanceFacts(facts.facts)...)
	c.mu.Unlock()
	facts.facts = nil
}

// consumePendingFacts merges the active-execution owner's accumulated
// uncommitted facts in front of the given carrier's own facts; the caller
// publishes that merged list exactly once and completes the consumption with
// completePending after the successful transaction, under the same
// coordinator hold the transaction runs on. A nil carrier still carries the
// accumulated facts alone.
func consumePendingFacts(c *coordinator, facts *maintenanceFacts) *maintenanceFacts {
	pending := c.pendingFacts
	if len(pending) == 0 {
		return facts
	}
	merged := &maintenanceFacts{}
	if facts != nil {
		merged = &maintenanceFacts{
			consumed:        facts.consumed,
			sourceRequestID: facts.sourceRequestID,
			maintenanceID:   facts.maintenanceID,
		}
	}
	merged.facts = append(append([]maintenanceFact{}, pending...), factsList(facts)...)
	return merged
}

// completePending clears the consumed accumulated facts after their
// successful publication, under the coordinator hold the transaction ran on.
func completePending(c *coordinator) {
	c.pendingFacts = nil
}

// clonePlanInput returns an independent deep copy of one frozen plan input:
// the strategy callback receives owned messages, tools, contributions,
// measurement and settings and can never mutate the Harness's frozen source
// copy used to resolve Retain, persisted facts, admission capture or a later
// callback.
func clonePlanInput(in PlanInput) PlanInput {
	out := in
	out.Messages = cloneRequestMessages(in.Messages)
	out.Tools = cloneToolDefinitions(in.Tools)
	if in.Contributions != nil {
		contributions := copyRequestEvidence(*in.Contributions)
		out.Contributions = &contributions
	}
	if in.Measurement != nil {
		measurement := *in.Measurement
		out.Measurement = &measurement
	}
	out.Settings = model.CloneRaw(in.Settings)
	return out
}

// ownModelPlan takes owned copies of one returned model plan's request and
// raw State before the value is retained or executed.
func ownModelPlan(plan *ModelPlan) *ModelPlan {
	if plan == nil {
		return nil
	}
	out := &ModelPlan{State: model.CloneRaw(plan.State)}
	out.Request.Messages = cloneRequestMessages(plan.Request.Messages)
	out.Request.Tools = cloneToolDefinitions(plan.Request.Tools)
	return out
}

// ownCheckpointPlan takes owned copies of one returned checkpoint plan's
// items before the value is retained or resolved.
func ownCheckpointPlan(plan *CheckpointPlan) *CheckpointPlan {
	if plan == nil {
		return nil
	}
	out := &CheckpointPlan{BoundaryEntryID: plan.BoundaryEntryID}
	out.Items = make([]CheckpointItem, len(plan.Items))
	for i, item := range plan.Items {
		if item.Retain != nil {
			position := *item.Retain
			out.Items[i].Retain = &position
		}
		if item.Context != nil {
			owned := cloneMessageValue(*item.Context)
			out.Items[i].Context = &owned
		}
	}
	return out
}

// resolveCheckpointItems materializes one proposed checkpoint's ordered items
// against the Harness's frozen covered view. A Retain item resolves its
// zero-based source position from the frozen canonical PlanInput.Messages —
// the Harness's own copy, never a callback-modified value — where positions
// address ordinary history or individual messages of the prior checkpoint and
// must be strictly increasing and inside the covered view. A Context item is
// explicitly strategy-authored and passes the shared message validation with
// no tool calls or tool-result call id. Before source positions are discarded,
// every retained tool exchange keeps its own OCCURRENCE pairing: outstanding
// call IDs open and retire in source order over the complete frozen canonical
// vector (completed IDs may be reused), so each assistant position is
// associated with its exact matching result positions. A retained assistant
// must carry all its own results and a retained result must carry its own
// assistant source; a retained call paired with another occurrence's identical
// result is rejected. The materialized self-contained messages are returned
// owned; proposal positions are never durable lineage.
func resolveCheckpointItems(frozen []model.Message, items []CheckpointItem, covered int) ([]model.Message, error) {
	if len(items) == 0 {
		return nil, errors.New("checkpoint messages must not be empty")
	}
	messages := make([]model.Message, 0, len(items))
	retained := make(map[int]bool)
	previous := -1
	for i, item := range items {
		switch {
		case (item.Retain == nil) == (item.Context == nil):
			return nil, fmt.Errorf("checkpoint items[%d] must carry exactly one of retain or context", i)
		case item.Retain != nil:
			position := *item.Retain
			if position <= previous {
				return nil, fmt.Errorf("checkpoint items[%d] retain position %d is not strictly increasing", i, position)
			}
			// A retained source must sit in the frozen canonical view and
			// at or before the proposal's covered boundary: only the covered
			// source prefix is retainable.
			if position < 0 || position >= covered {
				return nil, fmt.Errorf("checkpoint items[%d] retain position %d is outside the covered source of %d messages", i, position, covered)
			}
			previous = position
			retained[position] = true
			messages = append(messages, cloneMessageValue(frozen[position]))
		default:
			owned, err := validateContributionMessage(*item.Context)
			if err != nil {
				return nil, fmt.Errorf("checkpoint items[%d]: %w", i, err)
			}
			messages = append(messages, owned)
		}
	}
	// Occurrence pairing over the complete frozen canonical vector: each open
	// call ID retires its oldest open assistant occurrence in source order, so
	// one assistant position associates with its exact matching result
	// positions even when identical opaque call IDs recur across exchanges.
	resultsOf := make(map[int][]int)
	ownerOf := make(map[int]int)
	open := make(map[string][]int)
	for i := range frozen {
		for _, call := range frozen[i].ToolCalls {
			open[call.ID] = append(open[call.ID], i)
		}
		if frozen[i].ToolCallID == "" {
			continue
		}
		queue := open[frozen[i].ToolCallID]
		if len(queue) == 0 {
			continue // an unopened result is caught by the materialized pairing guard
		}
		resultsOf[queue[0]] = append(resultsOf[queue[0]], i)
		ownerOf[i] = queue[0]
		open[frozen[i].ToolCallID] = queue[1:]
	}
	for position := range retained {
		if len(frozen[position].ToolCalls) > 0 {
			for _, result := range resultsOf[position] {
				if !retained[result] {
					return nil, fmt.Errorf("checkpoint retain position %d omits its tool call's result at source position %d", position, result)
				}
			}
		}
		if frozen[position].ToolCallID != "" {
			owner, opened := ownerOf[position]
			if !opened {
				return nil, fmt.Errorf("checkpoint retain position %d is a tool result no call opens in the frozen source", position)
			}
			if !retained[owner] {
				return nil, fmt.Errorf("checkpoint retain position %d omits its tool result's call at source position %d", position, owner)
			}
		}
	}
	if err := validateToolPairing(messages); err != nil {
		return nil, err
	}
	return messages, nil
}

// validateToolPairing enforces complete tool call/result pairing over one
// materialized message view by tracking outstanding calls: each assistant
// call opens its opaque ID and each matching tool result retires it, so two
// authentic completed exchanges may carry the same opaque call ID. A
// duplicate still-outstanding ID, an orphan or repeated result, and a call
// whose result never arrives cannot commit or hide inside a checkpoint.
func validateToolPairing(messages []model.Message) error {
	outstanding := make(map[string]bool)
	for i, m := range messages {
		for _, call := range m.ToolCalls {
			if outstanding[call.ID] {
				return fmt.Errorf("checkpoint messages[%d] repeats still-outstanding tool call %q", i, call.ID)
			}
			outstanding[call.ID] = true
		}
		if m.ToolCallID == "" {
			continue
		}
		if !outstanding[m.ToolCallID] {
			return fmt.Errorf("checkpoint messages[%d] carries tool result %q with no outstanding call in the view", i, m.ToolCallID)
		}
		delete(outstanding, m.ToolCallID)
	}
	for id := range outstanding {
		return fmt.Errorf("checkpoint carries tool call %q with no result in the view", id)
	}
	return nil
}

// retainInput appends the actual logical maintenance request the effect
// received to the ordered facts the moment it receives it; the per-plan
// identity stays the fact's own producer, never the procedure's witness.
func (f *maintenanceFacts) retainInput(maintenanceID string, produced model.ModelRef, request model.Request) {
	f.facts = append(f.facts, maintenanceFact{
		RequestID: maintenanceID,
		Model:     produced,
		Input:     cloneRequestMessages(request.Messages),
	})
}

// retainUsage lands one assembled output's reported usage on the newest
// fact before that output settles; an output reporting no usage leaves the
// fact's usage absent, and a reported zero stays known zero.
func (f *maintenanceFacts) retainUsage(usage *model.Usage) {
	if usage == nil || len(f.facts) == 0 {
		return
	}
	counts := UsageCount{
		InputTokens:       int64(usage.InputTokens),
		CachedInputTokens: int64(usage.CachedInputTokens),
		OutputTokens:      int64(usage.OutputTokens),
	}
	f.facts[len(f.facts)-1].Usage = &counts
}

// factsUsageContribution renders the ordered facts' reported usage as the
// one totals contribution: per fact, absent usage contributes nothing and a
// reported zero stays known zero.
func factsUsageContribution(facts []maintenanceFact) (UsageTotals, error) {
	var contribution UsageTotals
	for _, fact := range facts {
		if fact.Usage == nil {
			continue
		}
		var err error
		contribution, err = addUsageTotals(contribution, UsageTotals{ByModel: []ModelUsage{{Model: fact.Model, Usage: *fact.Usage}}})
		if err != nil {
			return UsageTotals{}, err
		}
	}
	return contribution, nil
}

// compactModelEffect encloses one maintenance strategy model request in one
// logical model-effect intent/result pair — a sibling of modelEffect closing
// over the compact transport and the orchestration's maintenance facts. The
// shared active-effect intent transaction commits the same durable intent
// carrying the maintenance producer's own request identity, the captured
// compact model, and the maintenance purpose — never the suspended
// conversation's identity — and publishes the supplied suspended conversation
// request state inside that existing transition when none is published yet.
// The in-memory intent's expected identity is the compact model, the assembly
// source the agent's callback validates and the terminal usage model, because
// the maintenance request runs on the compact transport while the durable
// active-effect state is model-agnostic, and the intent is marked as
// compaction so its terminal usage carries maintenance provenance. The
// standard attempt-retry loop runs through the compact transport. The actual
// tool-free logical maintenance request is retained on the carrier's ordered
// facts the moment the effect receives it, and the assembled output's
// reported usage — nil-preserving, a reported zero stays known zero — lands on
// that fact before that output settles. A completed output settles by the
// empty running result — no entry, the active effect cleared, both registers
// replaced with no usage, the reserved identity unused — and returns the
// ready settlement whose output carries the completed result; the
// orchestration folds it through the strategy's Consume after the settlement.
// A maintenance failure and an interruption settle through the effect's own
// terminal settlement with the retained maintenance facts — the conversation
// effect's own shape, and the run's callback gate requires it whenever the
// assembly callback ran — while pre-assembly failures and cancellations
// return no output. The invocation's callbacks come from the opened
// maintenance invocation; its retry classifier is the enclosing Operation's.
func (h *Harness) compactModelEffect(c *coordinator, operationID string, inv InvocationExecution, retry RetryPolicy, capture ExecutionCapture, maintenanceID string, facts *maintenanceFacts, suspended *RunningRequest) agent.ModelEffect {
	attempt := inv.Model
	return func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		// The actual tool-free logical maintenance request is retained on the
		// carrier's ordered facts the moment the effect receives it,
		// deep-owned.
		facts.retainInput(maintenanceID, capture.Compact.Model, req)
		intent, err := h.beginModelEffect(ctx, c, operationID, maintenanceID, InvocationPurposeMaintenance, capture.Compact.Model, nil, suspended)
		if err != nil {
			// A marked publication failure propagates before any cancellation
			// rule, regardless of a concurrent cancellation. Only an unmarked
			// cancellation observed before the intent transaction was attempted
			// settles the cancellation outcome: the run never sees a
			// cancellation-shaped error out of an active effect.
			if !isPublicationFailure(err) && isExecutionCancellation(err) && ctx.Err() != nil {
				if _, cerr := h.commitEffectResult(context.WithoutCancel(h.ctx), c, operationID, nil, modelResult{
					terminal:      OperationInterruption,
					detail:        executionInterruptedDetail,
					compaction:    true,
					facts:         facts,
					maintenanceID: facts.producer(),
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
			return h.interruptModelEffect(c, operationID, intent, nil, facts)
		}
		// Result and terminal transactions run without cancellation so an
		// already-produced result or required terminal settlement publishes.
		settleCtx := context.WithoutCancel(h.ctx)
		settle := func(cause error) (agent.ModelSettlement, error) {
			if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
				terminal:      OperationFailure,
				detail:        failureDetail(cause),
				compaction:    true,
				facts:         facts,
				maintenanceID: facts.producer(),
			}); err != nil {
				return agent.ModelSettlement{}, err
			}
			return agent.ModelSettlement{}, cause
		}
		// The one shared attempt loop lives inside the committed intent,
		// identical to the conversation model effect's but running through the
		// compact transport. The maintenance request is consumed exactly as
		// the maintenance procedure planned it.
		var output model.Output
		switch res := runModelAttempts(ctx, req, attempt, retry); res.kind {
		case attemptAccepted:
			out, attemptErr := assemble(intent.expected, h.observeStream(intent.sessionID, operationID, res.stream)) // exactly one assembly after acceptance; the enclosing Operation owns the deltas
			if attemptErr != nil {
				return settle(attemptErr)
			}
			output = out
		case attemptInterrupted:
			return h.interruptModelEffect(c, operationID, intent, nil, facts)
		case attemptInvalid:
			return settle(res.err)
		case attemptFailed:
			committed := agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: failureDetail(res.err)}
			if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
				terminal:      OperationFailure,
				detail:        failureDetail(res.err),
				compaction:    true,
				facts:         facts,
				maintenanceID: facts.producer(),
			}); err != nil {
				return agent.ModelSettlement{}, err
			}
			return committed, nil
		}
		// The assembled output's reported usage lands on the carrier's
		// newest fact before that output settles; an output reporting no
		// usage leaves the fact's usage absent.
		facts.retainUsage(output.Usage)
		if output.Status == model.OutputCompleted {
			// The empty running result inserts no entry, clears the active
			// effect, and replaces both registers with no usage; the reserved
			// result identity stays unused. The strategy folds the completed
			// output through its Consume after this ready settlement.
			if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{}); err != nil {
				return agent.ModelSettlement{}, err
			}
			return agent.ModelSettlement{Disposition: agent.DispoReady, Output: &output}, nil
		}
		// A maintenance failure and an interruption settle through the
		// effect's own terminal settlement, consuming the reserved identity.
		// The assembled output rides the returned settlement — an errored
		// output on the failure row, an interrupted one on the interruption
		// row — while a pre-assembly failure or cancellation returns none.
		terminal := OperationFailure
		disposition := agent.DispoFailure
		if output.Status == model.OutputInterrupted {
			terminal = OperationInterruption
			disposition = agent.DispoInterruption
		}
		if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
			terminal:      terminal,
			detail:        output.Detail,
			compaction:    true,
			facts:         facts,
			maintenanceID: facts.producer(),
		}); err != nil {
			return agent.ModelSettlement{}, err
		}
		return agent.ModelSettlement{Disposition: disposition, Detail: output.Detail, Output: &output}, nil
	}
}

// runContextMaintenance runs the selected context strategy's maintenance
// procedure over the frozen input: Plan decides the next step, every model
// plan executes through its own maintenance invocation and the compact
// model-effect boundary with a fresh per-plan producer identity, and each
// completed output folds back through Consume with the plan's raw local
// State until the procedure ends. The strategy State is process-local only
// and is never persisted. Every callback receives an owned deep copy of the
// frozen input and every returned plan value is owned before it is retained,
// so a mutating strategy can never rewrite the Harness's frozen source copy.
// The selected binding is accepted and frozen once here — before any Plan
// callback, for model-free and model-producing procedures alike — requiring
// a nonnil implementation and the captured identity and requirement; a nil
// binding still selects the included strategy. A returned plan is validated
// against the closed union before anything runs; a malformed union, a
// strategy error, or a model failure settles the quiet running Operation
// through the direct terminal settlement carrying the retained maintenance
// facts, while an actual publication failure propagates with the running
// state retained and no guessed follow-up write. A procedure that ends at
// none after model work transfers its uncommitted ordered facts to the
// active-execution owner before the completed invocation disposes; they stay
// process-local until the next successful checkpoint or terminal publication
// accounts them once. The produced checkpoint — or nil when the strategy
// plans none — and the accumulated facts return to the caller, which
// validates and commits the checkpoint. Only the current owned invocation
// closer is retained: a completed invocation becomes intermediate only when
// its successor Model plan passes validation, and it then disposes before
// that successor opens; a plan that fails validation or ends the procedure
// retains the current closer through the settlement or consumption, and the
// final one disposes only after the checkpoint/terminal consumption.
func (h *Harness) runContextMaintenance(execCtx context.Context, c *coordinator, operationID string, exec Execution, capture ExecutionCapture, input PlanInput, suspended *RunningRequest) (*CheckpointPlan, *maintenanceFacts, func() error, error) {
	closeInvocation := func() error { return nil }
	// The procedure's producer identity is minted before the first plan, so
	// every early failure still carries a valid settlement witness.
	maintenanceID, err := newHexID()
	facts := &maintenanceFacts{maintenanceID: maintenanceID}
	if err != nil {
		return nil, facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, fmt.Errorf("%w: %v", ErrStorage, err), capture, facts)
	}
	// The consumed evidence names the frozen input references — never the
	// proposed cut — and the frozen contribution view replaces it with the
	// same references plus their effective deltas.
	facts.consumed = RequestEvidence{CheckpointEntryID: input.CheckpointEntryID, BoundaryEntryID: input.BoundaryEntryID}
	if input.Contributions != nil {
		facts.consumed = *input.Contributions
	}
	if suspended != nil {
		// Automatic maintenance records the conversation request its frozen
		// input view belongs to before the maintenance opener runs, so its
		// facts stay associated with that request even when the producer
		// identity is minted later or never minted at all.
		facts.sourceRequestID = suspended.RequestID
	}
	// The StrategyBinding is accepted and frozen once, before any Plan
	// callback, as a local value copy: nil still selects the included
	// strategy, the accepted binding must provide its implementation and match
	// the captured strategy inputs exactly, and a caller that mutates its own
	// supplied binding afterwards can never re-point this procedure's Plan or
	// Consume.
	binding := *resolveStrategyBinding(exec.Strategy)
	if binding.Strategy == nil {
		return nil, facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, invalidInput("opened strategy binding %q carries no implementation", binding.ID), capture, facts)
	}
	if binding.ID != capture.Strategy.ID || binding.RequiresContextWindow != capture.Strategy.RequiresContextWindow {
		return nil, facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, invalidInput("opened strategy binding %q does not match the captured strategy %q", binding.ID, capture.Strategy.ID), capture, facts)
	}
	if observeMaintenanceInterrupt(execCtx, c, operationID) {
		// Before entering the unstarted pure planning stage the execution
		// cancellation or the matching interruption marker settles the
		// ordinary interruption with the incurred facts; the stage never
		// starts and the waiting conversation never dispatches.
		return nil, facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationInterruption, errors.New(executionInterruptedDetail), capture, facts)
	}
	plan, err := binding.Strategy.Plan(clonePlanInput(input))
	if err != nil {
		return nil, facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, err, capture, facts)
	}
	plan.Model, plan.Checkpoint = ownModelPlan(plan.Model), ownCheckpointPlan(plan.Checkpoint)
	for {
		if err := validatePlanOutput(plan); err != nil {
			return nil, facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, err, capture, facts)
		}
		switch plan.Kind {
		case PlanNone:
			if facts.hasInput() {
				// Model work was incurred and the procedure settled at none:
				// the uncommitted ordered facts transfer to the
				// active-execution owner before the completed invocation
				// disposes, to settle once at the next successful checkpoint
				// or terminal publication.
				h.transferPending(c, facts)
			}
			return nil, facts, closeInvocation, nil
		case PlanCheckpoint:
			return plan.Checkpoint, facts, closeInvocation, nil
		}
		// The successor Model plan is validated: the completed
		// invocation's effects settled and Consume finished, so it is
		// intermediate and disposes before its successor opens, keeping
		// only the current owned closer. A plan that fails validation or
		// ends at none/checkpoint retains the current closer through the
		// settlement or consumption for the caller to dispose.
		closeInvocation()
		closeInvocation = func() error { return nil }
		output, closer, err := h.runMaintenanceModel(execCtx, c, operationID, exec, capture, plan.Model, suspended, facts)
		if err != nil {
			return nil, facts, closer, err
		}
		closeInvocation = closer
		if observeMaintenanceInterrupt(execCtx, c, operationID) {
			// The unstarted pure folding stage observes the same
			// cancellation and marker before it begins: the accepted
			// model usage and facts settle on the ordinary interruption.
			return nil, facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationInterruption, errors.New(executionInterruptedDetail), capture, facts)
		}
		next, err := binding.Strategy.Consume(ConsumeInput{Input: clonePlanInput(input), State: plan.Model.State, Output: output})
		if err != nil {
			return nil, facts, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, err, capture, facts)
		}
		next.Model, next.Checkpoint = ownModelPlan(next.Model), ownCheckpointPlan(next.Checkpoint)
		plan = next
	}
}

// runMaintenanceModel executes one tool-free maintenance model plan through
// its own opened maintenance invocation and the compact model effect: a fresh
// per-plan producer identity, the maintenance purpose, and the captured
// compact model. An invocation the opener rejects cleans its own provisional
// resources. The completed output returns to the procedure; every failure and
// run-level terminal settles durably here — inside the effect or through the
// direct terminal settlement — and returns the original cause.
func (h *Harness) runMaintenanceModel(execCtx context.Context, c *coordinator, operationID string, exec Execution, capture ExecutionCapture, plan *ModelPlan, suspended *RunningRequest, facts *maintenanceFacts) (model.Output, func() error, error) {
	closeInvocation := func() error { return nil }
	maintenanceID, err := newHexID() // the maintenance Engine call's fresh producer identity
	if err != nil {
		return model.Output{}, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, fmt.Errorf("%w: %v", ErrStorage, err), capture, facts)
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
		return model.Output{}, closeInvocation, h.settleCompactionFailure(c, operationID, terminal, err, capture, facts)
	}
	if err := validateOpenedInvocation(inv); err != nil {
		if inv.Close != nil {
			h.recordCleanupFailure(inv.Close()) // an invalid invocation cleans its own opened resources
		}
		return model.Output{}, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, err, capture, facts)
	}
	closeInvocation = func() error { // the opened maintenance invocation disposes on the retained cleanup path
		if inv.Close != nil {
			h.recordCleanupFailure(inv.Close())
		}
		return nil
	}
	effect := h.compactModelEffect(c, operationID, inv, exec.Retry, capture, maintenanceID, facts, suspended)
	request := plan.Request
	res, runErr := runEngineCall(execCtx, agent.Invocation{
		ExpectedModel: capture.Compact.Model,
		Request: func(context.Context) (model.Request, error) {
			return request, nil
		},
		ModelEffect: effect,
		ToolEffect: func(context.Context, model.ToolCall, []string) (model.ToolResult, error) {
			return model.ToolResult{}, &agent.ProtocolError{Boundary: "tool", Detail: "compact agent has no tools"}
		},
	})
	if runErr != nil {
		return model.Output{}, closeInvocation, h.settleCompactionFailure(c, operationID, OperationFailure, runErr, capture, facts)
	}
	switch res.Status {
	case agent.TerminalSuccess:
		return *res.LastOutput, closeInvocation, nil
	default:
		// A run-level terminal — a pre-run cancellation, or a cancellation
		// observed after the settled result before the run's success return —
		// leaves the Operation running and quiet with no settlement; the
		// direct settlement covers it, and takes no compensating write when
		// the effect already settled the same terminal.
		terminal := OperationFailure
		if res.Status == agent.TerminalInterruption {
			terminal = OperationInterruption
		}
		return model.Output{}, closeInvocation, h.settleCompactionFailure(c, operationID, terminal, errors.New(res.Detail), capture, facts)
	}
}

// observeMaintenanceInterrupt reports whether an unstarted pure planning
// stage must not begin: the execution cancellation or the matching
// interruption marker already names this Operation. A foreign marker never
// matches here.
func observeMaintenanceInterrupt(execCtx context.Context, c *coordinator, operationID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return execCtx.Err() != nil || c.interruptOp == operationID
}

// settleCompactionFailure is the direct terminal settlement of one
// orchestration failure or run-level terminal on the quiet running Operation:
// the given terminal state and non-empty detail commit with the retained
// maintenance facts — the ordered actual inputs with their producing models
// and reported usage, and the consumed contribution evidence — settling on
// this same transaction exactly once, together with the accumulated
// uncommitted facts of the active execution. An Operation already settled by
// the maintenance effect takes no compensating write, and an actual
// publication failure — classified by the boundary where it occurred, not by
// its error identity — propagates with the durable running state retained for
// recovery and no guessed follow-up write. A failed settlement adds no
// guessed follow-up write either. This is the one owner of the settlement
// attempt for these failures; callers never retry it.
func (h *Harness) settleCompactionFailure(c *coordinator, operationID string, terminal OperationState, cause error, capture ExecutionCapture, facts *maintenanceFacts) error {
	if isPublicationFailure(cause) {
		return cause
	}
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
		compaction:    true,
		facts:         facts,
		maintenanceID: facts.producer(),
	}); err != nil {
		return err
	}
	return cause
}

// validateCheckpointPlan resolves and validates one proposed checkpoint
// before any commit: the ordered Retain/Context items materialize against the
// Harness's frozen covered view — retained source positions unique, strictly
// increasing and inside that view, authored context tool-free — and the
// covered boundary names an in-snapshot canonical boundary inside the frozen
// snapshot's covered range, resolved from the supplied frozen boundary
// reference: the named prior checkpoint entry or a projectable entry strictly
// after the prior checkpoint's own cut and at or before that frozen boundary.
// The materialized checkpoint and the rebuilt kept tail are validated
// together: complete tool call/result pairing across the combined view, and
// no tool-call/result pair straddling the cut and no unfinished call hidden
// at it — a proposed boundary between a call and its result is invalid even
// if the materialized checkpoint messages happen to pair internally. The
// resolved self-contained messages return to the caller for the commit.
// Every violation is a strategy failure settled before the checkpoint
// commit; the previous checkpoint stays current.
func (h *Harness) validateCheckpointPlan(c *coordinator, input PlanInput, checkpoint *CheckpointPlan) ([]model.Message, error) {
	if checkpoint == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := c.graph.Entries
	prior, cutoff := findCompactionCutoff(entries, input.CheckpointEntryID)
	// The covered range ends at the supplied frozen boundary entry — the
	// actual frozen view's tail — never at a reconstructed endpoint.
	coveredEndSequence := int64(-1)
	if input.BoundaryEntryID != "" {
		if boundary, ok := entryByID(entries, input.BoundaryEntryID); ok {
			coveredEndSequence = boundary.Envelope.Sequence
		}
	}
	proposed := checkpoint.BoundaryEntryID
	boundary, found := entryByID(entries, proposed)
	covered := proposed == input.CheckpointEntryID && input.CheckpointEntryID != ""
	if !covered {
		if !found {
			return nil, fmt.Errorf("checkpoint boundary %q names no in-session entry", proposed)
		}
		projectable := boundary.Input != nil || boundary.Assistant != nil || boundary.ToolResult != nil || boundary.Signal != nil
		covered = projectable && boundary.Envelope.Sequence > cutoff && boundary.Envelope.Sequence <= coveredEndSequence
	}
	if !covered {
		return nil, fmt.Errorf("checkpoint boundary %q is outside the frozen snapshot's covered range", proposed)
	}
	if !found {
		return nil, fmt.Errorf("checkpoint boundary %q names no in-session entry", proposed)
	}
	// The covered source prefix of the frozen view: the prior checkpoint's
	// own messages in front of the messages of every entry the proposed cut
	// covers — resolved from the canonical refs and entry projection, never
	// from a message count.
	coveredSource := 0
	if prior != nil {
		coveredSource = len(prior.Messages)
	}
	cutSequence := boundary.Envelope.Sequence
	for i := range entries {
		entry := entries[i]
		if entry.Envelope.Sequence > cutoff && entry.Envelope.Sequence <= cutSequence && messageBearingEntryKind(entry) {
			coveredSource++
		}
	}
	materialized, err := resolveCheckpointItems(input.Messages, checkpoint.Items, coveredSource)
	if err != nil {
		return nil, err
	}
	// No tool-call/result pair straddles the cut and no unfinished call is
	// hidden: the same precomputed cut-safety rule the cold graph validation
	// applies rejects any boundary whose published calls leave a missing
	// result or a reserved-result sequence beyond it.
	if offender, unsafe := buildCutSafety(entries)[proposed]; unsafe {
		return nil, fmt.Errorf("checkpoint boundary %q cuts between tool call %q and its result", proposed, offender)
	}
	// The materialized checkpoint and the rebuilt kept tail validate
	// together: every carried call and result pairs completely across the
	// rebuilt view, so no unfinished call can hide behind the cut.
	tail := make([]model.Message, 0, len(entries))
	for i := range entries {
		if found && entries[i].Envelope.Sequence <= boundary.Envelope.Sequence {
			continue
		}
		msg, ok, err := projectEntry(entries[i])
		if err != nil {
			return nil, err
		}
		if ok {
			tail = append(tail, msg)
		}
	}
	if err := validateToolPairing(append(append([]model.Message{}, materialized...), tail...)); err != nil {
		return nil, err
	}
	return materialized, nil
}

// entryByID resolves one committed entry by identity.
func entryByID(entries []graphEntry, id string) (graphEntry, bool) {
	for i := range entries {
		if entries[i].Envelope.ID == id {
			return entries[i], true
		}
	}
	return graphEntry{}, false
}

// commitMaintenance validates and commits one maintenance procedure's
// proposed checkpoint. A proposed checkpoint must pass the resolution,
// coverage and pairing validation before the commit transaction; a violation
// is a strategy failure settled through the direct terminal settlement with
// the retained facts, leaving the previous checkpoint current. So is an
// automatic proposal whose published running request names a different
// conversation request: the published request is never replaced. A manual
// procedure whose strategy planned none still settles its dedicated
// Operation, carrying its actual ordered facts when model work was incurred.
// An automatic procedure that planned none proceeds with no write at all.
// The execution context crosses this completion boundary so the checkpoint
// and manual-success publication can select interruption versus prospective
// success under its own publication hold.
func (h *Harness) commitMaintenance(execCtx context.Context, c *coordinator, operationID string, capture ExecutionCapture, input PlanInput, facts *maintenanceFacts, checkpoint *CheckpointPlan, manual bool) error {
	materialized, err := h.validateCheckpointPlan(c, input, checkpoint)
	if err != nil {
		return h.settleCompactionFailure(c, operationID, OperationFailure, err, capture, facts)
	}
	// The automatic procedure's authoritative request association: a published
	// running request naming a conversation request other than the one the
	// procedure consumed is a mismatched association — the proposal is rejected
	// as a strategy failure before the commit, never a replacement of the
	// published request. Manual maintenance names no conversation request.
	if !manual && checkpoint != nil && facts != nil && facts.sourceRequestID != "" {
		c.mu.Lock()
		published := ""
		if op, ok := c.graph.Operation(operationID); ok && op.State.Request != nil {
			published = op.State.Request.RequestID
		}
		c.mu.Unlock()
		if published != "" && published != facts.sourceRequestID {
			return h.settleCompactionFailure(c, operationID, OperationFailure, invalidInput("operation %q publishes running request %q; the maintenance procedure consumed request %q", operationID, published, facts.sourceRequestID), capture, facts)
		}
	}
	if checkpoint == nil && !manual {
		return nil
	}
	return h.commitCheckpoint(execCtx, c, operationID, capture, input, facts, checkpoint, materialized, manual)
}

// commitCheckpoint commits the one atomic context checkpoint transaction: the
// immutable checkpoint entry with its strategy provenance, owned messages,
// ordered maintenance facts, and consumed contribution evidence, the Session
// register's projection field, and the usage totals on the Operation and
// Session registers — plus, on the manual shape, the dedicated Operation's
// success settlement — all inside one transaction on the cancellation-free
// Harness context. A published suspended conversation request state is
// advanced in this same transaction to the new checkpoint entry and its
// rebuilt projection tail, never replaced by the maintenance producer's
// identity; when none is published yet the automatic procedure's checkpoint
// transition publishes the conversation request it consumed with the rebuilt
// references, while manual maintenance creates no conversation
// RunningRequest. The entry's boundary names the covered boundary the
// procedure proposed and the caller validated. The ordered facts' reported usage lands
// keyed by each producing model on the entry and both register totals in the
// same transaction. A failed transaction leaves every previous value durable
// and the running state to recovery: it settles nothing and the caller
// returns the error, so the waiting conversation request is never sent
// uncompacted and no guessed follow-up terminal write is ever fabricated. A
// manual procedure whose strategy planned none commits no checkpoint entry:
// only the dedicated Operation's success settlement lands. The
// checkpoint/manual-success publication selects interruption versus
// prospective success under this same hold with the shared success
// predicate: an interruption winning it publishes the ordinary interruption
// settlement with all incurred facts, preserves the previous checkpoint, and
// never dispatches the waiting conversation; once success has committed, a
// later interruption never rewrites it.
func (h *Harness) commitCheckpoint(execCtx context.Context, c *coordinator, operationID string, capture ExecutionCapture, input PlanInput, facts *maintenanceFacts, checkpoint *CheckpointPlan, materialized []model.Message, manual bool) error {
	c.mu.Lock()
	viewOp, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return wrapPublication(fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID))
	}
	sessionID := viewOp.Admission.SessionID
	viewSession := c.graph.Session
	// The maintenance completion guard: one hold with the publication, before
	// the transaction, under the shared success predicate. A foreign marker
	// and a root stop's stopping state never veto the prospective success.
	interrupted := execCtx.Err() != nil || c.interruptOp == operationID || c.bgState == bgClosed
	// The active execution's accumulated uncommitted facts — incurred by
	// earlier procedures that settled at none — merge once, in incurred order
	// in front of this procedure's own facts, into the carrier this checkpoint
	// or manual terminal publishes, and are consumed only after the
	// transaction commits.
	mergedFacts := consumePendingFacts(c, facts)

	// The checkpoint transaction's authoritative conversation request
	// association, resolved once from the procedure's facts.
	consumedSource := ""
	if facts != nil {
		consumedSource = facts.sourceRequestID
	}

	var (
		committedOp   OperationRecord
		committedSess SessionRecord
		newEntries    []graphEntry
	)
	err := h.deps.Storage.Transact(context.WithoutCancel(h.ctx), func(tx Transaction) error {
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
			return invalidInput("operation %q is %s; a checkpoint commits to a running Operation", operationID, currentOp.State.Status)
		}
		if sreg.Revision != viewSession.Revision {
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, viewSession.Revision, sreg.Revision)
		}
		if oreg.Revision != viewOp.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, viewOp.Revision, oreg.Revision)
		}

		nextOp := currentOp
		nextSess := currentSession
		committedSess = SessionRecord{Identity: currentSession.Identity, State: nextSess.State}
		var replaced, replacedSession Register
		if interrupted {
			// The interruption wins the publication hold: the existing
			// ordinary interruption settlement publishes with all incurred
			// facts and the previous checkpoint stays current.
			settledOp, settledSess, settledEntries, err := commitTerminalSettlement(tx, sessionID, operationID, committedSess, nextOp, sreg.Revision, oreg.Revision, OperationInterruption, executionInterruptedDetail, nil, model.ModelRef{}, nil, true, mergedFacts, mergedFacts.producer())
			if err != nil {
				return err
			}
			nextOp = settledOp
			committedSess = settledSess
			newEntries = append(newEntries, settledEntries...)
			committedOp = nextOp
			return nil
		}
		if checkpoint != nil {
			entryID, err := newHexID()
			if err != nil {
				return fmt.Errorf("%w: %v", ErrStorage, err)
			}
			// The consumed evidence names the frozen input references —
			// never the proposed cut.
			consumed := RequestEvidence{CheckpointEntryID: input.CheckpointEntryID, BoundaryEntryID: input.BoundaryEntryID}
			// The merged carrier holds this checkpoint's facts exactly once,
			// the accumulated uncommitted facts already in incurred order
			// before the committing procedure's own.
			factList := factsList(mergedFacts)
			if mergedFacts.hasConsumed() {
				consumed = mergedFacts.consumed // the effective contribution evidence the procedure consumed
			}
			payload, err := encodeContextCheckpointEntry(contextCheckpointEntry{
				SessionID:             sessionID,
				EntryID:               entryID,
				OperationID:           operationID,
				StrategyID:            capture.Strategy.ID,
				Messages:              materialized,
				BoundaryEntryID:       checkpoint.BoundaryEntryID,
				ConfigurationRevision: capture.ConfigurationRevision,
				Facts:                 factList,
				ConsumedRequestID:     consumedSource,
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

			// The ordered facts' reported usage lands keyed by each producing
			// model on the entry and both register totals.
			contribution, err := factsUsageContribution(factList)
			if err != nil {
				return err
			}
			opUsage, err := addUsageTotals(currentOp.State.Usage, contribution)
			if err != nil {
				return err
			}
			sessionUsage, err := addUsageTotals(currentSession.State.Usage, contribution)
			if err != nil {
				return err
			}

			nextOp.State.Usage = opUsage
			// The checkpoint transition names the complete rebuilt projection
			// pair — the new checkpoint AND the rebuilt view's canonical tail
			// boundary — in this same transaction, so the state names exactly
			// the consumed projection the request source rebuilds after it. An
			// automatic procedure publishes the authoritative conversation
			// request when none is published yet; manual maintenance never
			// creates the conversation RunningRequest. An already-published
			// request keeps its identity and recorded hook results while only
			// its references advance.
			rebuilt := rebuiltBoundary(c.graph.Entries, checkpoint.BoundaryEntryID, entryID)
			if nextOp.State.Request == nil {
				if !manual && consumedSource != "" {
					nextOp.State.Request = &RunningRequest{RequestID: consumedSource, CheckpointEntryID: entryID, BoundaryEntryID: rebuilt}
				}
			} else {
				advanced := *nextOp.State.Request
				advanced.CheckpointEntryID = entryID
				advanced.BoundaryEntryID = rebuilt
				nextOp.State.Request = &advanced
			}
			opPayload, err := encodeOperationRegister(nextOp)
			if err != nil {
				return err
			}
			replaced, err = tx.ReplaceRegister(opKey, oreg.Revision, opPayload)
			if err != nil {
				return err
			}
			nextOp.Revision = replaced.Revision

			nextSess.State.CheckpointEntryID = entryID
			nextSess.State.Usage = sessionUsage
			committedSess = SessionRecord{Identity: currentSession.Identity, State: nextSess.State}
			sessionPayload, err := encodeSessionRegister(committedSess)
			if err != nil {
				return err
			}
			replacedSession, err = tx.ReplaceRegister(sessionKey, sreg.Revision, sessionPayload)
			if err != nil {
				return err
			}
			committedSess.Revision = replacedSession.Revision
		} else {
			replaced = Register{Revision: oreg.Revision}
			replacedSession = Register{Revision: sreg.Revision}
		}

		// The manual shape settles the dedicated Operation as success inside
		// the same transaction; the settlement itself carries no usage
		// because the checkpoint part already added the totals, and the
		// checkpoint owns the maintenance facts, so the settlement carries
		// none. A manual procedure that planned none settles its actual
		// ordered facts — the accumulated uncommitted facts in front of the
		// procedure's own — on this same success terminal, exactly once.
		if manual {
			var settlementFacts *maintenanceFacts
			if checkpoint == nil && mergedFacts.hasInput() {
				settlementFacts = mergedFacts
			}
			settledOp, settledSess, settledEntries, err := commitTerminalSettlement(tx, sessionID, operationID, committedSess, nextOp, replacedSession.Revision, replaced.Revision, OperationSuccess, "", nil, model.ModelRef{}, nil, settlementFacts != nil, settlementFacts, mergedFacts.producer())
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
				return wrapPublication(rerr)
			}
		}
		// A publication failure leaves the actual running state to recovery:
		// no settlement attempt runs here and the caller performs none.
		return wrapPublication(err)
	}
	c.pendingFacts = nil // the accumulated facts are consumed only after their successful publication
	c.graph.Entries = append(c.graph.Entries, newEntries...)
	c.graph.replaceOperation(operationID, committedOp)
	c.graph.Session = committedSess
	c.mu.Unlock()
	h.observeInvalidation(c) // every checkpoint commit replaces the Session register
	if interrupted {
		// The interruption settlement already published: the waiting
		// conversation never dispatches and no later interruption rewrites
		// the committed terminal.
		return errors.New(executionInterruptedDetail)
	}
	return nil
}

// findCompactionCutoff resolves the named checkpoint's payload and cutoff
// over one validated Session graph — the single lookup behind both the
// context projection and the compaction boundary. An empty named ID is the
// uncompacted history: no payload and no cutoff. A nonempty named ID's
// references are guaranteed by graph validation — the register names an
// in-session checkpoint entry and that entry's boundary names an in-session
// entry strictly before it — so the lookup returns that payload and the
// boundary target's sequence; a missing persisted reference is corruption
// rejected before this lookup runs.
func findCompactionCutoff(entries []graphEntry, namedID string) (*contextCheckpointEntry, int64) {
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

// rebuiltBoundary names the canonical tail boundary of the rebuilt
// projection one committed checkpoint produces: the newest
// conversation-bearing entry the kept tail carries strictly after the covered
// cut, and the new checkpoint entry itself when the entry tail carries none —
// a rebuilt projection whose tail is the checkpoint names the checkpoint
// entry as its canonical boundary.
func rebuiltBoundary(entries []graphEntry, cutEntryID, checkpointEntryID string) string {
	cut, ok := entryByID(entries, cutEntryID)
	if !ok {
		return checkpointEntryID
	}
	boundary := ""
	for i := range entries {
		if entries[i].Envelope.Sequence <= cut.Envelope.Sequence {
			continue
		}
		if messageBearingEntryKind(entries[i]) {
			boundary = entries[i].Envelope.ID
		}
	}
	if boundary == "" {
		return checkpointEntryID
	}
	return boundary
}
