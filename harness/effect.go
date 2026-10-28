package harness

import (
	"bytes"
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

// PermissionDeniedContent is the contract-fixed model-visible content of
// every Harness policy denial; plugins that settle their own denial results
// carry the same published string.
const PermissionDeniedContent = "Permission denied."

// unknownToolResultPrefix is the fixed prefix the shared result producer
// bakes into the model-facing content of a call whose concrete execution is
// possibly-executed-but-unverified (unknown evidence).
const unknownToolResultPrefix = "Execution outcome unknown; the action may have occurred."

// unknownHookToolResultPrefix is the fixed prefix the shared result producer
// bakes into the model-facing content of a proven-unstarted call whose
// pre-tool hook outcome is unknown.
const unknownHookToolResultPrefix = "Pre-tool hook outcome unknown; the hook may have taken effect. The tool was not executed."

// buildToolResultContent renders one settled call's final model-facing content
// exactly once from its producer-assigned execution knowledge and base text:
// an unknown concrete execution and an unknown-outcome pre-tool hook each
// carry their fixed uncertainty prefix joined to the unchanged base by a
// blank line, and every other result stores its base verbatim. It never
// alters status, detail, usage or evidence, and because the built string is
// what the producer stores, no later settlement or recovery can remove a
// prefix.
func buildToolResultContent(evidence ExecutionEvidence, hookUnknown bool, base string) string {
	switch {
	case evidence == ExecutionUnknown:
		return unknownToolResultPrefix + "\n\n" + base
	case hookUnknown:
		return unknownHookToolResultPrefix + "\n\n" + base
	default:
		return base
	}
}

// normalizationFailureContent renders the bounded stored diagnostic of one
// advertised call's failed original normalization: the fixed internal-
// validation content for a malformed callback outcome or an empty
// diagnostic, otherwise the bounded useful diagnostic. The stored value is
// exactly the content later dispatch settles with, without another
// NormalizeTool invocation.
func normalizationFailureContent(cause error) string {
	if errors.Is(cause, errMalformedNormalization) {
		return invalidToolResultContent
	}
	if content := boundedToolDiagnostic(cause); content != "" {
		return content
	}
	return invalidToolResultContent
}

// shapingFailureDiagnostic renders one aborted shaping chain's bounded
// processing failure: the bounded cause diagnostic, falling back to the fixed
// internal-validation content when the cause carries no usable text, so a
// failed shaper always publishes its processing failure.
func shapingFailureDiagnostic(cause error) string {
	if content := boundedToolDiagnostic(cause); content != "" {
		return content
	}
	return invalidToolResultContent
}

// maxToolDiagnosticBytes bounds one Harness-produced validation diagnostic
// carried in a tool-result content.
const maxToolDiagnosticBytes = 512

// executionInterruptedDetail is the diagnostic detail of the terminal
// interruption the Harness settles when execution cancellation stops a
// callback, mirroring the Agent's own cancellation checkpoints.
const executionInterruptedDetail = "agent interrupted"

// genericExecutionFailureDetail is the non-empty standard terminal detail one
// execution error whose own text is empty still settles with; an actual
// nonempty diagnostic is always carried verbatim.
const genericExecutionFailureDetail = "execution failure"

// failureDetail renders one Go failure cause as the non-empty terminal detail
// the closed settlement validator requires.
func failureDetail(cause error) string {
	if cause == nil {
		return genericExecutionFailureDetail
	}
	if text := cause.Error(); text != "" {
		return text
	}
	return genericExecutionFailureDetail
}

// publicationFailure is the one private publication-origin wrapper: it marks an
// error raised while publishing durable intent, result, checkpoint or
// terminal state, preserving the original cause through errors.Is and
// errors.As. A publication failure is distinguished by the boundary where it
// occurred — never by its error identity — and retains the actual durable
// running state for recovery, with no guessed follow-up terminal write.
type publicationFailure struct{ cause error }

func (e *publicationFailure) Error() string { return e.cause.Error() }
func (e *publicationFailure) Unwrap() error { return e.cause }

// wrapPublication wraps one publication-boundary cause.
func wrapPublication(cause error) error {
	if cause == nil {
		return nil
	}
	return &publicationFailure{cause: cause}
}

// isPublicationFailure reports whether one error originated at a durable
// publication boundary.
func isPublicationFailure(err error) bool {
	var failure *publicationFailure
	return errors.As(err, &failure)
}

// runEngineCall runs one Engine invocation at the one shared Harness
// composition boundary: each supplied callback's publication-origin error is
// retained before the Engine handles it, and after the run the retained
// original propagates to the owning Harness orchestration or settler in place
// of the Engine's substituted error or terminal. Every other outcome —
// including a genuine pre-intent cancellation interruption and a
// semantic or provider error of a similar sentinel shape — returns verbatim.
// The Engine invokes the wrapped callbacks synchronously and sequentially, so
// one captured variable holds the first retained error.
func runEngineCall(execCtx context.Context, invocation agent.Invocation) (agent.TerminalResult, error) {
	var retained error
	keep := func(err error) error {
		if err != nil && retained == nil && isPublicationFailure(err) {
			retained = err
		}
		return err
	}
	request := invocation.Request
	invocation.Request = func(ctx context.Context) (model.Request, error) {
		req, err := request(ctx)
		return req, keep(err)
	}
	modelEffect := invocation.ModelEffect
	invocation.ModelEffect = func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		settle, err := modelEffect(ctx, req, assemble)
		return settle, keep(err)
	}
	res, err := agent.Run(execCtx, invocation)
	if retained != nil {
		return agent.TerminalResult{}, retained
	}
	return res, err
}

// modelEffectIntent is the committed intent of one logical model effect: the
// facts the result transaction needs after the callback returns, including
// the codec-adopted effective input evidence the intent published.
type modelEffectIntent struct {
	sessionID  string
	expected   model.ModelRef
	resultID   string
	compaction bool // true for the compact effect's intent: its usage is summary usage
	requestID  string
	evidence   *RequestEvidence // the adopted evidence the committed intent carries (conversation effects)
}

// hookEffectBinding is one settled hook execution's binding: its extension
// and hook identities and exactly one of the tool call or request it executed
// for. The two binding families stay disjoint even for equal opaque strings.
type hookEffectBinding struct {
	hookID    string
	callID    string
	requestID string
}

// pendingCallIntent carries one pending-call effect's intent input: a hook
// effect its extension identity and consumed input bytes, a tool effect its
// accepted normalized selection and contributing hook provenance.
type pendingCallIntent struct {
	hookID               string
	extensionID          string
	inputArgumentsBase64 string
	finalArguments       json.RawMessage
	contributionIDs      []HookIdentity
}

// toolSelection is the final effective argument value a settled tool call
// consumed and the ordered hook provenance that produced it. Results that
// never accepted a value carry neither.
type toolSelection struct {
	arguments json.RawMessage
	hooks     []HookIdentity
}

// modelResult is the settlement content of one logical model effect: the
// produced assistant entry when the output carried an eligible payload, the
// fixed continuation signal when the disposition continues, the terminal
// classification when the effect settles the Operation, the terminal
// no-output model usage when a reported usage has no assistant entry to ride,
// and the maintenance facts a maintenance terminal retains. The usage model
// is the intent's expected identity while an intent is live; without one the
// result declares its own usage model. The compaction flag carries that
// usage's provenance: true marks summary usage produced by the compaction
// summarizer, and conversation producers leave it false.
// successContext is the execution context of an outer prospective success
// only; the publisher's shared guard consults it before the transaction.
// Effect-origin terminals, compaction commits and recovery leave it nil.
type modelResult struct {
	assistant      *assistantEntry   // nil when no eligible assistant payload exists
	signal         SignalKind        // empty unless the disposition continues
	terminal       OperationState    // empty for ready/continue; failure or interruption otherwise
	detail         string            // terminal detail; empty for ready/continue
	usage          *UsageCount       // terminal no-output usage; nil when the assistant entry carries it or none was reported
	usageModel     model.ModelRef    // the terminal no-output usage model when no intent is live; zero otherwise
	compaction     bool              // true when the carried terminal usage is summary usage
	facts          *maintenanceFacts // retained maintenance input/consumed evidence a maintenance terminal settles
	maintenanceID  string            // the maintenance producer's own request identity for settlement facts
	successContext context.Context
}

// modelEffect encloses one complete invocation of the opened conversation
// invocation's model function in one logical model-effect intent/result pair:
// it commits the active-effect intent carrying the round's request identity,
// purpose, model, and effective input evidence, dispatches the physical
// attempts against the request materialized from the committed intent's
// adopted evidence over the round's frozen canonical view (never a reread
// history or replayed hooks), invokes the assembly callback exactly once
// after a stream is accepted, derives the settlement from the assembled
// output, validates it, and commits the result and complete next Operation
// state before returning the committed settlement. The automatic maintenance
// orchestration and the request contributions belong to the request source;
// this effect consumes the round they built. Before the assistant entry
// commits, every advertised completed call is normalized through the
// invocation's pure callback outside storage transactions and owning locks.
// The retry classifier belongs to the enclosing Execution.
func (h *Harness) modelEffect(c *coordinator, operationID string, exec Execution, inv InvocationExecution, capture ExecutionCapture, round *conversationRound) agent.ModelEffect {
	attempt := inv.Model
	normalize := inv.NormalizeTool
	return func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		// The advertisement of this logical effect is the effective tool view
		// of its frozen round: the Engine derived it from the validated
		// request before the effect runs and hands the dispatch gate a
		// per-call copy.
		intent, err := h.beginModelEffect(ctx, c, operationID, round.requestID, InvocationPurposeConversation, capture.Model, &round.net, nil)
		if err != nil {
			// A marked publication failure propagates before any cancellation
			// rule, regardless of a concurrent cancellation. Only an unmarked
			// cancellation observed before the intent transaction was ever
			// attempted settles the cancellation outcome: the run never sees a
			// cancellation-shaped error out of an active effect.
			if !isPublicationFailure(err) && isExecutionCancellation(err) && ctx.Err() != nil {
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
			return h.interruptModelEffect(c, operationID, intent, nil, nil)
		}
		// Result and terminal transactions run without cancellation so an
		// already-produced result or required terminal settlement publishes.
		settleCtx := context.WithoutCancel(h.ctx)
		settle := func(cause error) (agent.ModelSettlement, error) {
			if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
				terminal: OperationFailure,
				detail:   failureDetail(cause),
			}); err != nil {
				return agent.ModelSettlement{}, err
			}
			return agent.ModelSettlement{}, cause
		}
		// The dispatch view is materialized from the committed intent's
		// adopted evidence over the round's frozen canonical messages, so
		// actual I/O consumes exactly the values the durable intent owns.
		dispatchReq, dispatchNames := buildDispatchRequest(capture, round.canonical, *intent.evidence)
		// The one shared attempt loop lives inside the committed intent: every
		// failed attempt is classified by the retry policy, waits run behind
		// the cancellation checks, and an accepted stream never re-enters
		// retry — assembly owns it, including closure.
		var output model.Output
		switch res := runModelAttempts(ctx, dispatchReq, attempt, exec.Retry); res.kind {
		case attemptAccepted:
			out, attemptErr := assemble(intent.expected, h.observeStream(intent.sessionID, operationID, res.stream)) // exactly one assembly after acceptance
			if attemptErr != nil {
				return settle(attemptErr)
			}
			output = out
		case attemptInterrupted:
			return h.interruptModelEffect(c, operationID, intent, nil, nil)
		case attemptInvalid:
			return settle(res.err)
		case attemptFailed:
			committed := agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: failureDetail(res.err)}
			if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
				terminal: OperationFailure,
				detail:   failureDetail(res.err),
			}); err != nil {
				return agent.ModelSettlement{}, err
			}
			return committed, nil
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
		assistant, reported, err := newAssistantEntry(intent.sessionID, operationID, intent.resultID, owned.Output, normalize, dispatchNames, intent.requestID, intent.evidence)
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
		return owned, nil
	}
}

// contextMeasurement samples the newest reported conversation usage after
// the current checkpoint boundary under a brief coordinator hold — the
// optional conversation measurement the selected strategy plans over. A
// missing report stays missing.
func contextMeasurement(c *coordinator) *UsageCount {
	c.mu.Lock()
	usage := latestContextUsage(c.graph.Entries, c.graph.Session.State.CheckpointEntryID)
	c.mu.Unlock()
	return usage
}

// interruptModelEffect settles one committed model effect intent as the fixed
// terminal interruption after observed execution cancellation before stream
// acceptance: no output, no assembly call, and the committed settlement
// returned with a nil error. The given usage and maintenance facts ride the
// settlement entry.
func (h *Harness) interruptModelEffect(c *coordinator, operationID string, intent modelEffectIntent, usage *UsageCount, facts *maintenanceFacts) (agent.ModelSettlement, error) {
	settleCtx := context.WithoutCancel(h.ctx)
	committed := agent.ModelSettlement{Disposition: agent.DispoInterruption, Detail: executionInterruptedDetail}
	if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
		terminal:      OperationInterruption,
		detail:        executionInterruptedDetail,
		usage:         usage,
		facts:         facts,
		maintenanceID: facts.producer(),
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
		if model.HasAssistantContent(out.Message) { // the content-only query: an errored output carries no tool calls, so content is its whole retained-payload question.
			return agent.ModelSettlement{Disposition: agent.DispoContinue, Output: &out}
		}
		return agent.ModelSettlement{Disposition: agent.DispoFailure, Output: &out, Detail: out.Detail}
	}
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
// effect: it reserves the effect's result entry identity, records the request
// identity, actual model, purpose, and (for a conversation effect) the
// effective input evidence it consumes, and publishes or preserves the
// suspended conversation request state in the same transaction — a
// conversation effect publishes its round's state when none is published, and
// a maintenance effect publishes the supplied suspended conversation state
// under the conversation identity, never replacing it with its own producer
// identity. The reserved identity stays free until the model result or the
// consuming settlement commits under it. The caller continues on the
// codec-adopted record the transaction wrote.
// beginModelEffect commits the one active-effect intent of a model effect.
// A cancellation observed before the intent transaction is invoked returns
// unwrapped and may use the caller's ordinary interruption settlement. Once
// the transaction is invoked it runs on a cancellation-free copy of the
// execution context — values only, like the result and terminal publications
// — so every error from it is a real store failure and a publication-origin
// failure at the intent boundary: it propagates with the durable state
// retained for recovery and is never followed by a guessed compensating
// terminal write.
func (h *Harness) beginModelEffect(ctx context.Context, c *coordinator, operationID string, requestID string, purpose InvocationPurpose, mdl model.ModelRef, evidence *RequestEvidence, suspended *RunningRequest) (modelEffectIntent, error) {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return modelEffectIntent{}, wrapPublication(fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID))
	}
	if op.State.Status != OperationRunning {
		c.mu.Unlock()
		return modelEffectIntent{}, wrapPublication(invalidInput("operation %q is %s; a model effect requires a running Operation", operationID, op.State.Status))
	}
	if op.State.ActiveEffect != nil {
		c.mu.Unlock()
		return modelEffectIntent{}, wrapPublication(invalidInput("operation %q already carries an active effect; effects never nest", operationID))
	}
	if ctx.Err() != nil { // cancellation observed before the Transact is ever invoked is not a
		// publication failure: it returns unwrapped and the caller's ordinary
		// interruption settlement applies. An invoked Transact can no longer observe this
		// context, so every error from it is a real store failure marked publication-origin.
		c.mu.Unlock()
		return modelEffectIntent{}, ctx.Err()
	}
	resultID, err := newHexID()
	if err != nil {
		c.mu.Unlock()
		return modelEffectIntent{}, wrapPublication(fmt.Errorf("%w: %v", ErrStorage, err))
	}
	effectModel := mdl
	var updated OperationRecord
	// Once the pre-Transact check admits the transition, the durable intent
	// transaction runs without execution cancellation or deadline — values
	// only, exactly like the result and terminal publications — so a shutdown
	// that lands mid-transaction never aborts the admitted intent. The
	// caller's execution-context gate settles the interruption afterwards.
	err = h.deps.Storage.Transact(context.WithoutCancel(ctx), func(tx Transaction) error {
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
		if current.State.ActiveEffect != nil {
			return invalidInput("operation %q already carries an active effect; effects never nest", operationID)
		}
		if reg.Revision != op.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, op.Revision, reg.Revision)
		}
		next := current
		next.State.ActiveEffect = &ActiveEffect{
			Kind:          EffectModel,
			ResultEntryID: resultID,
			RequestID:     requestID,
			Model:         &effectModel,
			Purpose:       purpose,
			Evidence:      evidence,
		}
		switch purpose {
		case InvocationPurposeConversation:
			if evidence != nil {
				if next.State.Request == nil {
					next.State.Request = &RunningRequest{
						RequestID:         requestID,
						CheckpointEntryID: evidence.CheckpointEntryID,
						BoundaryEntryID:   evidence.BoundaryEntryID,
					}
				} else {
					// Refresh the already-published request's canonical references to
					// the rebuilt projection pair — the same checkpoint and tail the
					// incoming evidence names — so the running state always describes
					// the projection actually consumed. Hook-result references stay.
					advanced := *next.State.Request
					advanced.CheckpointEntryID = evidence.CheckpointEntryID
					advanced.BoundaryEntryID = evidence.BoundaryEntryID
					next.State.Request = &advanced
				}
			}
		case InvocationPurposeMaintenance:
			if next.State.Request == nil && suspended != nil {
				published := *suspended
				next.State.Request = &published
			}
		}
		payload, err := encodeOperationRegister(next)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
		if err != nil {
			return err
		}
		// The transition adopts the codec-owned record it wrote: the caller's
		// cached state, continuation, and dispatch evidence all come from the
		// committed value.
		adopted, err := decodeOperationRegister(replaced)
		if err != nil {
			return corruptSession(op.Admission.SessionID, "committed operation register %q: %v", operationID, err)
		}
		updated = adopted
		return nil
	})
	if err != nil {
		h.markCorrupt(op.Admission.SessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(ctx, c, op.Admission.SessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return modelEffectIntent{}, wrapPublication(rerr)
			}
		}
		return modelEffectIntent{}, wrapPublication(err)
	}
	c.graph.replaceOperation(operationID, updated)
	c.mu.Unlock()
	committed := updated.State.ActiveEffect
	intent := modelEffectIntent{
		sessionID: op.Admission.SessionID,
		expected:  op.Admission.Execution.Model,
		resultID:  resultID,
		requestID: requestID,
	}
	if purpose == InvocationPurposeMaintenance {
		intent.compaction = true
		intent.expected = mdl
	}
	if committed != nil && committed.Evidence != nil {
		intent.evidence = committed.Evidence
	}
	return intent, nil
}

// commitEffectResult commits the one result transaction of a settled effect:
// the semantic result entries, usage, the complete next Operation state, and
// the required Session state. A running result clears the active effect and
// records the completed calls' reservations; the suspended running request
// state survives a result that produced a pending batch and a maintenance
// result unchanged, and clears at a conversation result with no batch. A
// terminal result runs the common terminal helper — preserving any produced
// assistant payload, interrupting every call that will not execute, appending
// exactly one interruption signal, appending the settlement entry, writing the
// terminal Operation, and clearing the Session current Operation — all
// atomically. A non-nil intent must find its committed model effect intent,
// whose reserved identity the settlement consumes when no assistant payload
// exists; a nil intent (the outer terminal settlement after agent.Run)
// requires a quiet Operation and always assigns a fresh settlement identity. A
// publication failure leaves the committed running/intent state for recovery.
func (h *Harness) commitEffectResult(ctx context.Context, c *coordinator, operationID string, intent *modelEffectIntent, res modelResult) (OperationRecord, error) {
	c.mu.Lock()
	if res.successContext != nil { // the shared success guard: one hold with the publication, before the transaction
		if res.successContext.Err() != nil || c.interruptOp == operationID || c.bgState == bgClosed {
			// Interrupt winning this hold cannot be missed; a foreign
			// marker or a root stop's stopping state never converts the
			// success, and an Interrupt after the committed success cannot
			// rewrite it. The conversion keeps the result's own facts,
			// evidence and usage: only the prospective success becomes the
			// ordinary interruption.
			res.terminal, res.detail = OperationInterruption, executionInterruptedDetail
		}
	}
	viewOp, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return OperationRecord{}, wrapPublication(fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID))
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
			// The usage model and the usage provenance come from the live
			// intent; a quiet direct settlement declares its own.
			usageModel := res.usageModel
			compactionUsage := res.compaction
			if intent != nil {
				usageModel = intent.expected
				compactionUsage = intent.compaction
			}
			// Every terminal kind accounts the active execution's
			// accumulated uncommitted maintenance facts exactly once,
			// together with the settled effect's own retained facts.
			facts := consumePendingFacts(c, res.facts)
			var err error
			committedOp, committedSess, newEntries, err = commitTerminalSettlement(tx, sessionID, operationID, currentSession, currentOp, sreg.Revision, oreg.Revision, res.terminal, res.detail, res.assistant, usageModel, res.usage, compactionUsage, facts, res.maintenanceID)
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
		if (intent == nil || !intent.compaction) && len(next.State.PendingToolCalls) == 0 {
			next.State.Request = nil // the conversation result closed its request with no batch to suspend
		}
		opPayload, err := encodeOperationRegister(next)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(opKey, oreg.Revision, opPayload)
		if err != nil {
			return err
		}
		adoptedOp, err := decodeOperationRegister(replaced)
		if err != nil {
			return corruptSession(sessionID, "committed operation register %q: %v", operationID, err)
		}
		committedOp = adoptedOp

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
		return nil
	})
	if err != nil {
		h.markCorrupt(sessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return OperationRecord{}, wrapPublication(rerr)
			}
		}
		return OperationRecord{}, wrapPublication(err)
	}
	if res.terminal != "" {
		completePending(c) // the accumulated facts are consumed only after their successful publication
	}
	c.graph.Entries = append(c.graph.Entries, newEntries...)
	c.graph.replaceOperation(operationID, committedOp)
	c.graph.Session = committedSess
	c.mu.Unlock()
	h.observeInvalidation(c) // every result/terminal Session register replacement advances
	h.emitToolResultFacts(newEntries, sessionID, operationID)
	return committedOp, nil
}

// commitTerminalSettlement performs the complete terminal transition in one
// transaction against freshly decoded register records: it preserves any
// produced assistant payload with its completed calls' reservations, writes
// model.ResultInterrupted under every remaining committed tool-result identity
// whose call will not execute — a terminal Operation leaves no unresolved
// call — appends exactly one interruption signal for an interruption terminal,
// appends the Operation settlement entry, writes the terminal Operation with
// its usage totals, and clears the Session current Operation and the running
// request state, all atomically. The settlement entry consumes a committed
// model effect's reserved identity when no assistant payload exists; every
// other settlement takes a fresh identity. A no-output settlement that closed
// a request owns that request's evidence exactly once — the committed model
// intent's effective evidence when one is live, otherwise the retained
// maintenance carry — and the settlement never repeats evidence an assistant
// entry already owns. A maintenance settlement's facts (its producer identity,
// actual input, and consumed evidence) settle on the successful checkpoint or
// here, never both. Recovery calls it from its own re-reading transaction
// with no process-local facts. It performs no precondition check of its own:
// the caller passes only records whose state it may settle.
func commitTerminalSettlement(tx Transaction, sessionID, operationID string, currentSession SessionRecord, currentOp OperationRecord, sregRevision, oregRevision int64, terminal OperationState, detail string, assistant *assistantEntry, usageModel model.ModelRef, usage *UsageCount, compactionUsage bool, facts *maintenanceFacts, maintenanceID string) (OperationRecord, SessionRecord, []graphEntry, error) {
	var newEntries []graphEntry

	// The transaction's usage contribution is additive: the ordinary
	// assistant-carried or terminal no-output usage and the retained
	// maintenance facts' own per-producer usage both land when both occur,
	// and the same totals land on the producer entry, the Operation, and the
	// Session. A reported zero stays known zero and an absent report
	// contributes nothing.
	var contribution UsageTotals
	switch {
	case assistant != nil && assistant.Usage != nil:
		contribution = UsageTotals{ByModel: []ModelUsage{{Model: assistant.Source, Usage: *assistant.Usage}}}
	case usage != nil:
		contribution = UsageTotals{ByModel: []ModelUsage{{Model: usageModel, Usage: *usage}}}
	}
	factUsage, err := factsUsageContribution(factsList(facts))
	if err != nil {
		return OperationRecord{}, SessionRecord{}, nil, err
	}
	contribution, err = addUsageTotals(contribution, factUsage)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, nil, err
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
	// resuming it, with conservative unknown knowledge, the intent's
	// extension identity and binding, and the consumed input the intent
	// committed — before the interrupted tool results commit. Request-bound
	// hooks carry no fabricated tool call.
	if currentOp.State.ActiveEffect != nil && currentOp.State.ActiveEffect.Kind == EffectHook {
		intent := currentOp.State.ActiveEffect
		hook := hookResultEntry{
			SessionID:            sessionID,
			EntryID:              intent.ResultEntryID,
			OperationID:          operationID,
			HookID:               intent.HookID,
			ExtensionID:          intent.ExtensionID,
			ToolCallID:           intent.ToolCallID,
			RequestID:            intent.RequestID,
			Status:               hookInterrupted,
			Evidence:             ExecutionUnknown,
			Error:                recoveryInterruptedDetail,
			InputArgumentsBase64: intent.InputArgumentsBase64,
		}
		if intent.Evidence != nil {
			consumed := *intent.Evidence
			hook.InputEvidence = &consumed
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
	// identity whose call will not execute. The active tool effect's
	// interrupted result copies the committed intent's accepted arguments and
	// provenance and carries conservative unknown evidence: the executor's
	// external outcome is possibly-executed-but-unverified. A call carries
	// the hook-uncertainty prefix — with proven-unstarted concrete evidence —
	// when an active hook effect addresses it, or when an earlier committed
	// tool-bound hook result already holds unknown evidence (the quiet gap a
	// crash between the hook result's and the tool result's transactions
	// leaves): an unknown pre-tool hook's notice is never dropped. Unstarted
	// pending siblings carry plain evidence neither. The committed hook
	// result scan serves synthetic settlement only: it runs just for the
	// calls actually settled here, and an ordinary completion with no
	// unresolved call never loads the history.
	remaining := append(append([]PendingToolCall{}, currentOp.State.PendingToolCalls...), pending...)
	unknownHookCalls := map[string]bool{}
	if len(remaining) > 0 {
		committedEntries, err := tx.ReadEntries(sessionID, 0)
		if err != nil {
			return OperationRecord{}, SessionRecord{}, nil, err
		}
		for _, env := range committedEntries {
			if env.Kind != EntryHookResult || env.OperationID != operationID {
				continue
			}
			hook, err := decodeHookResultEntry(env)
			if err != nil {
				return OperationRecord{}, SessionRecord{}, nil, corruptSession(sessionID, "committed hook result %s: %v", env.ID, err)
			}
			if hook.ToolCallID != "" && hook.Evidence == ExecutionUnknown {
				unknownHookCalls[hook.ToolCallID] = true
			}
		}
	}
	for _, call := range remaining {
		evidence := ExecutionNotStarted
		hookUnknown := unknownHookCalls[call.CallID]
		if currentOp.State.ActiveEffect != nil && currentOp.State.ActiveEffect.ToolCallID == call.CallID {
			switch currentOp.State.ActiveEffect.Kind {
			case EffectTool:
				evidence = ExecutionUnknown
			case EffectHook:
				hookUnknown = true
			}
		}
		result := toolResultEntry{
			SessionID:       sessionID,
			EntryID:         call.ResultEntryID,
			OperationID:     operationID,
			AssistantEntry:  call.AssistantEntry,
			ToolCallID:      call.CallID,
			Status:          model.ResultInterrupted,
			Content:         buildToolResultContent(evidence, hookUnknown, interruptedToolResultContent),
			OriginalContent: interruptedToolResultContent, // synthetic settlements are never shaped
			Evidence:        evidence,
		}
		if currentOp.State.ActiveEffect != nil && currentOp.State.ActiveEffect.Kind == EffectTool &&
			currentOp.State.ActiveEffect.ToolCallID == call.CallID {
			if len(currentOp.State.ActiveEffect.FinalArguments) > 0 {
				result.NormalizedArguments = model.CloneRaw(currentOp.State.ActiveEffect.FinalArguments)
			}
			if len(currentOp.State.ActiveEffect.ContributionIDs) > 0 {
				result.HookIDs = append([]HookIdentity(nil), currentOp.State.ActiveEffect.ContributionIDs...)
			}
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
	// identity. It witnesses the closed request: the published running
	// request when one exists, otherwise a live model intent's or the
	// maintenance producer's identity. A no-output settlement owns the final
	// request evidence exactly once, copied from the committed model intent
	// when one is live or from the retained maintenance carry otherwise.
	settlementID := ""
	activeEffect := currentOp.State.ActiveEffect
	if assistant == nil && activeEffect != nil && activeEffect.Kind == EffectModel {
		settlementID = activeEffect.ResultEntryID
	}
	if settlementID == "" {
		if settlementID, err = newHexID(); err != nil {
			return OperationRecord{}, SessionRecord{}, nil, fmt.Errorf("%w: %v", ErrStorage, err)
		}
	}
	settlement := operationSettlementEntry{
		SessionID:       sessionID,
		EntryID:         settlementID,
		OperationID:     operationID,
		Status:          terminal,
		Detail:          detail,
		CompactionUsage: compactionUsage,
	}
	// The settlement's request witness: a maintenance-family settlement
	// names its own producer identity (never a replacement of the suspended
	// conversation state, which lives on the register state until this
	// terminal clears it); otherwise the published running request;
	// otherwise a live model intent's identity. Ordinary accumulated
	// maintenance facts can ride any terminal kind without owning the
	// settlement's own witness.
	switch {
	case compactionUsage && facts != nil && (facts.hasInput() || facts.hasConsumed()):
		settlement.RequestID = maintenanceID
		settlement.ConsumedRequestID = facts.sourceRequestID
	case currentOp.State.Request != nil:
		settlement.RequestID = currentOp.State.Request.RequestID
	case assistant == nil && activeEffect != nil && activeEffect.Kind == EffectModel && activeEffect.RequestID != "":
		settlement.RequestID = activeEffect.RequestID
	}
	if assistant == nil {
		if activeEffect != nil && activeEffect.Kind == EffectModel && activeEffect.Evidence != nil {
			evidence := *activeEffect.Evidence
			settlement.Evidence = &evidence
		} else if compactionUsage && facts != nil && facts.hasConsumed() {
			consumed := facts.consumed
			settlement.Evidence = &consumed
		}
	}
	if facts.hasInput() {
		// The actual ordered maintenance facts settle on this terminal
		// exactly once beside any ordinary usage; their own reported usage
		// never becomes the settlement's carried conversation usage.
		settlement.Facts = cloneMaintenanceFacts(facts.facts)
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
	next.State.Request = nil
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
// completed call and recording the producing request identity, the
// effective advertised names, and the request evidence the model effect
// consumed. An output without an eligible model-visible payload writes no
// assistant entry and returns its reported usage for the consuming settlement
// entry instead. Before the entry commits, every completed call whose name is
// in the producing request's ordered advertised names is normalized through
// the execution's pure callback exactly once: its successful owned object
// populates the nested call's normalized_arguments member, and a failed or
// malformed normalization stores its bounded failure content in the nested
// call's normalization_failure member instead. Every unknown or unadvertised
// name stores neither and runs no callback; the original raw argument bytes
// are always retained.
func newAssistantEntry(sessionID, operationID, entryID string, out *model.Output, normalize func(model.ToolCall) (json.RawMessage, error), advertised []string, requestID string, evidence *RequestEvidence) (*assistantEntry, *UsageCount, error) {
	if out == nil {
		return nil, nil, nil
	}
	entry := &assistantEntry{
		SessionID:       sessionID,
		EntryID:         entryID,
		OperationID:     operationID,
		Status:          out.Status,
		Source:          out.Source,
		RequestID:       requestID,
		AdvertisedNames: append([]string(nil), advertised...),
	}
	if entry.AdvertisedNames == nil {
		entry.AdvertisedNames = []string{}
	}
	if evidence != nil {
		owned := copyRequestEvidence(*evidence)
		entry.Evidence = &owned
	}
	advertisedNames := make(map[string]bool, len(advertised))
	for _, name := range advertised {
		advertisedNames[name] = true
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
			if advertisedNames[call.Name] {
				if normalized, err := normalizeCallArguments(normalize, call); err == nil {
					record.NormalizedArguments = normalized
				} else {
					record.NormalizationFailure = normalizationFailureContent(err)
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

// copyRequestEvidence returns an independent deep-owned copy of one evidence
// value: canonical references, the prompt pointee, every recorded message's
// nested parts and extras, and the name and provenance lists.
func copyRequestEvidence(v RequestEvidence) RequestEvidence {
	out := v
	if v.Prompt != nil {
		prompt := *v.Prompt
		out.Prompt = &prompt
	}
	out.Messages = cloneRequestMessages(v.Messages)
	if v.ToolNames != nil {
		names := make([]string, len(v.ToolNames))
		copy(names, v.ToolNames)
		out.ToolNames = names
	}
	if v.HookIDs != nil {
		out.HookIDs = append([]HookIdentity(nil), v.HookIDs...)
	}
	return out
}

// copyRequestEvidencePointer deep-copies an evidence value behind its pointer.
func copyRequestEvidencePointer(evidence *RequestEvidence) *RequestEvidence {
	if evidence == nil {
		return nil
	}
	owned := copyRequestEvidence(*evidence)
	return &owned
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
// required canonical. Constraints and target preparation are never
// delegated. Only then does the evaluator decide the complete prepared
// action, and the plan is allowed exactly when it returns allow without
// error: a deny, a decision outside the closed set, or an error denies the
// call alike. A nil evaluator selects the included built-in policy.
func toolCallAllowed(ctx context.Context, evaluator PermissionEvaluator, capture ExecutionCapture, plan PreparedTool) bool {
	if len(plan.Permissions) == 0 {
		return false
	}
	var hasFile, hasWrite bool
	for _, req := range plan.Permissions {
		if req.Permission == "" || req.Target == "" {
			return false
		}
		switch req.Permission {
		case PermissionFileRead:
			hasFile = true
		case PermissionFileWrite:
			hasFile, hasWrite = true, true
		}
		if (req.Permission == PermissionFileRead || req.Permission == PermissionFileWrite) && !canonicalPathValue(req.Target) {
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
				if req.Permission == PermissionFileWrite && !containsPath(plan.CanonicalWriteDir, req.Target) {
					return false
				}
			}
		}
	}
	if evaluator == nil {
		evaluator = PermissionPolicy{}
	}
	decision, err := evaluator.Evaluate(ctx, PreparedAction{
		CanonicalWorkspace: plan.CanonicalWorkspace,
		CanonicalWriteDir:  plan.CanonicalWriteDir,
		Permissions:        append([]PermissionRequest(nil), plan.Permissions...),
	})
	return err == nil && decision == PermissionAllow
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
// advertisement gate settles for a name outside the producing request's
// advertised set.
func unavailableToolResult(callID, name string) model.ToolResult {
	return model.ToolResult{CallID: callID, Status: model.ResultError, Content: fmt.Sprintf("Tool %q is not available.", name)}
}

// permissionDeniedToolResult is the ordinary policy-denial result settled for
// the original call under its reserved identity: no effect began and no tool
// active intent was written.
func permissionDeniedToolResult(callID string) model.ToolResult {
	return model.ToolResult{CallID: callID, Status: model.ResultDenied, Content: PermissionDeniedContent}
}

// validationToolResult is the immediate validation-error result for an invalid
// model argument: status error carrying the callback's own bounded useful
// diagnostic. An empty diagnostic still carries the internal-validation text.
func validationToolResult(callID string, cause error) model.ToolResult {
	content := boundedToolDiagnostic(cause)
	if content == "" {
		content = invalidToolResultContent
	}
	return model.ToolResult{CallID: callID, Status: model.ResultError, Content: content}
}

// boundedToolDiagnostic renders one Harness-produced validation diagnostic as
// valid UTF-8 within the durable diagnostic bound: invalid bytes are removed
// at any length, and over-long text truncates within the same sanitizing
// pass, so no diagnostic can make its owning entry unencodable.
func boundedToolDiagnostic(cause error) string {
	msg := cause.Error()
	if len(msg) > maxToolDiagnosticBytes {
		msg = msg[:maxToolDiagnosticBytes]
	}
	return strings.ToValidUTF8(msg, "")
}

// toolEffect encloses one concrete tool execution in the shared Harness tool
// boundary. In order: it resolves the call's pending reservation and committed
// assistant record, and pre-start cancellation wins before anything else; the
// advertisement gate rejects names outside the engine-supplied advertisement of
// the producing request — the per-call owned copy the Engine derived from the
// validated request before its model effect — with the ordinary
// unavailable-tool error before any hook or preparer runs, while the canonical
// pending-call record stays the authority for the call's own data; the
// selected argument hooks then run in configured order — a pure hook validates
// and normalizes its changed bytes once in memory with no effect transaction,
// an effectful hook runs as its own settled effect retaining the input it
// actually consumed — and a settled hook chain hands the final committed value
// on. A pure-only chain can repair an invalid original without inventing a
// durable hook effect, and later dispatch never normalizes the repaired value
// again. Without hooks (or for an unhooked call) the original assistant's
// committed normalized arguments are the selected input, and an invalid
// unhooked original settles with the failure diagnostic its one producer
// normalization stored, without another NormalizeTool invocation. It then
// prepares the plan outside locks, rejects invalid plan
// shapes with the existing internal-validation error, and evaluates every
// declaration through the fixed boundary — including immediate-success plans —
// before beginPendingCallEffect. A denial settles only that original call with
// status denied and no metadata, no concrete effect and no tool active intent;
// later calls still run. An allowed executor commits intent carrying the
// accepted selection and provenance, executes once behind the
// cancellation-before-start gate, and commits one validated outcome — a
// returned real outcome wins a cancellation race. Every settled call's result
// carries the final effective arguments and contributing hook provenance
// actually consumed. A tool-produced outcome — an immediate or executor
// result, its invalid replacement included — passes the invocation's ordered
// pure output-shaping chain before its final commit; the harness-synthetic
// refusals settle unshaped. The preparation and normalization callbacks come
// from the opened conversation invocation; the permission policy from the
// enclosing Execution.
func (h *Harness) toolEffect(c *coordinator, operationID string, exec Execution, inv InvocationExecution, capture ExecutionCapture) agent.ToolEffect {
	prepared := inv.Tool
	return func(ctx context.Context, call model.ToolCall, names []string) (model.ToolResult, error) {
		pending, record, err := pendingToolCallRecord(c, operationID, call.ID)
		if err != nil {
			return model.ToolResult{}, err
		}
		// The live dispatcher entry: one started fact after the durable pending
		// call resolved, before any validation, hook, permission or execution.
		// A dispatched denial, error, immediate or interrupted call shares it.
		h.emitFact(HarnessFact{
			Kind:        FactToolStarted,
			SessionID:   pending.AssistantEntry.SessionID,
			OperationID: operationID,
			CallID:      record.ID,
			Ordinal:     record.Ordinal,
			Name:        record.Name,
		})
		settleCtx := context.WithoutCancel(h.ctx)
		if ctx.Err() != nil { // execution cancellation prevents later preparation: interrupted-before-execution
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: interruptedToolResult(call.ID)}, nil, false, toolSelection{}, ExecutionNotStarted, false)
		}
		advertised := make(map[string]bool, len(names))
		for _, name := range names {
			advertised[name] = true
		}
		if !advertised[record.Name] { // the one advertisement gate, ahead of every hook and preparer: the engine-supplied per-call advertisement of the producing request is the authority
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: unavailableToolResult(call.ID, record.Name)}, nil, false, toolSelection{}, ExecutionNotStarted, false)
		}
		// The selected normalized value feeding preparation: the final
		// committed hook chain value when hooks ran, otherwise the original
		// assistant's committed successful result. Both were already validated
		// and owned; neither is ever re-normalized. An unhooked invalid
		// original consumes the diagnostic its one producer normalization
		// stored and settles without another NormalizeTool invocation or
		// preparation.
		selection := toolSelection{arguments: record.NormalizedArguments}
		if len(inv.ToolHooks) > 0 {
			chain, settled, herr := h.runArgumentHooks(ctx, settleCtx, c, operationID, pending, record, inv)
			if herr != nil {
				return model.ToolResult{}, herr
			}
			if settled != nil { // the hook chain settled the call itself; remaining hooks are skipped and later calls continue
				return *settled, nil
			}
			selection = chain
		} else if len(selection.arguments) == 0 {
			result := invalidToolResult(call.ID) // neither member present: only reachable for gated or unadvertised names, which settle earlier
			if record.NormalizationFailure != "" {
				result = model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: record.NormalizationFailure}
			}
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: result}, nil, false, toolSelection{}, ExecutionNotStarted, false)
		}
		if ctx.Err() != nil { // finish the accepted chain, but start no subsequent preparation
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: interruptedToolResult(call.ID)}, nil, false, selection, ExecutionNotStarted, false)
		}
		// The preparer receives an owned copy of the selected normalized call
		// and nothing else.
		normalizedCall, err := model.NewToolCall(model.ToolCall{ID: record.ID, Name: record.Name, Arguments: selection.arguments, Extra: record.Extra})
		if err != nil {
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: invalidToolResult(call.ID)}, nil, false, selection, ExecutionNotStarted, false)
		}
		plan := prepared(ctx, normalizedCall)
		if !validToolPlan(plan) {
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: invalidToolResult(call.ID)}, nil, false, selection, ExecutionNotStarted, false)
		}
		if authorizationRequired(plan) && !toolCallAllowed(ctx, exec.Permissions, capture, plan) {
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: permissionDeniedToolResult(call.ID)}, nil, false, selection, ExecutionNotStarted, false)
		}
		if plan.Immediate != nil {
			outcome := *plan.Immediate
			if _, err := model.NewToolResult(outcome.Result); err != nil || outcome.Result.CallID != call.ID {
				outcome = ToolOutcome{Result: invalidToolResult(call.ID)}
			}
			// An immediate outcome is observed only when it reports success:
			// an immediate error, denied or interrupted result proves the
			// action never executed.
			evidence := ExecutionNotStarted
			if outcome.Result.Status == model.ResultSuccess {
				evidence = ExecutionObserved
			}
			// Pure output shaping over the tool-produced outcome (the
			// invalid-replacement content included), before the final commit.
			shaping := shapeToolOutput(ctx, inv.ToolOutputHooks, outcome, call.ID)
			return h.commitToolResult(settleCtx, c, operationID, pending, outcome, shaping, false, selection, evidence, false)
		}
		if _, err := h.beginPendingCallEffect(ctx, c, operationID, call.ID, EffectTool, pendingCallIntent{
			finalArguments:  selection.arguments,
			contributionIDs: selection.hooks,
		}); err != nil {
			// An intent transaction aborted by cancellation settles the
			// cancellation outcome: interrupted-before-execution, never a
			// cancellation-shaped error out of an active effect.
			if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() != nil {
				return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: interruptedToolResult(call.ID)}, nil, false, selection, ExecutionNotStarted, false)
			}
			return model.ToolResult{}, err
		}
		if ctx.Err() != nil { // cancellation before start commits interrupted-before-execution carrying the intent's accepted selection
			return h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: interruptedToolResult(call.ID)}, nil, true, toolSelection{}, ExecutionNotStarted, false)
		}
		outcome := plan.Execute(ctx)
		evidence := ExecutionObserved // every valid executor outcome is observed execution, whatever its status
		if _, err := model.NewToolResult(outcome.Result); err != nil || outcome.Result.CallID != call.ID {
			outcome = ToolOutcome{Result: invalidToolResult(call.ID)}
			// The executor ran — the action may have occurred — but produced
			// no valid terminal response: conservative unknown.
			evidence = ExecutionUnknown
		}
		// Pure output shaping over the tool-produced outcome (the
		// invalid-replacement content included), before the final commit.
		shaping := shapeToolOutput(ctx, inv.ToolOutputHooks, outcome, call.ID)
		// the real outcome wins a cancellation race: the settlement transaction
		// runs without cancellation and publishes what the execution produced.
		return h.commitToolResult(settleCtx, c, operationID, pending, outcome, shaping, true, toolSelection{}, evidence, false)
	}
}

// beginPendingCallEffect commits the one active-effect intent of a
// pending-call effect: the intent addresses the first pending call and
// reserves its result identity — the call's reserved identity for a tool
// effect, a fresh allocation for a hook — recording the effect kind, the tool
// selection and provenance for a tool effect, or the hook identity, extension
// identity, and consumed input bytes before I/O for a hook effect. Effects
// never nest, so any committed intent of another effect rejects.
func (h *Harness) beginPendingCallEffect(ctx context.Context, c *coordinator, operationID, callID string, kind EffectKind, input pendingCallIntent) (string, error) {
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
		next.State.ActiveEffect = &ActiveEffect{
			Kind:                 kind,
			ResultEntryID:        resultID,
			ToolCallID:           callID,
			HookID:               input.hookID,
			ExtensionID:          input.extensionID,
			InputArgumentsBase64: input.inputArgumentsBase64,
			FinalArguments:       input.finalArguments,
			ContributionIDs:      input.contributionIDs,
		}
		payload, err := encodeOperationRegister(next)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
		if err != nil {
			return err
		}
		adopted, err := decodeOperationRegister(replaced)
		if err != nil {
			return corruptSession(sessionID, "committed operation register %q: %v", operationID, err)
		}
		updated = adopted
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

// beginRequestHookEffect commits the one active-effect intent of a
// request-bound hook effect: it reserves a fresh result identity, records the
// hook identity, extension identity, and the consumed effective-view evidence
// before any callback runs, and publishes the suspended conversation request
// state inside this same existing effect transition when none is published
// yet — the request state is never written by a pure-only transaction.
func (h *Harness) beginRequestHookEffect(ctx context.Context, c *coordinator, operationID, requestID, hookID, extensionID string, consumed RequestEvidence) (string, error) {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return "", fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	if op.State.Status != OperationRunning {
		c.mu.Unlock()
		return "", invalidInput("operation %q is %s; a request hook effect requires a running Operation", operationID, op.State.Status)
	}
	if op.State.ActiveEffect != nil {
		c.mu.Unlock()
		return "", invalidInput("operation %q already carries an active effect; effects never nest", operationID)
	}
	resultID, err := newHexID()
	if err != nil {
		c.mu.Unlock()
		return "", fmt.Errorf("%w: %v", ErrStorage, err)
	}
	sessionID := op.Admission.SessionID
	consumedCopy := consumed
	var updated OperationRecord
	err = h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		key := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID}
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		current, err := decodeOperationRegister(reg)
		if err != nil {
			return corruptSession(sessionID, "operation register %q: %v", operationID, err)
		}
		if current.State.Status != OperationRunning {
			return invalidInput("operation %q is %s; a request hook effect requires a running Operation", operationID, current.State.Status)
		}
		if current.State.ActiveEffect != nil {
			return invalidInput("operation %q already carries an active effect; effects never nest", operationID)
		}
		if reg.Revision != op.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, op.Revision, reg.Revision)
		}
		next := current
		next.State.ActiveEffect = &ActiveEffect{
			Kind:          EffectHook,
			ResultEntryID: resultID,
			RequestID:     requestID,
			HookID:        hookID,
			ExtensionID:   extensionID,
			Evidence:      &consumedCopy,
		}
		if next.State.Request == nil {
			next.State.Request = &RunningRequest{
				RequestID:         requestID,
				CheckpointEntryID: consumed.CheckpointEntryID,
				BoundaryEntryID:   consumed.BoundaryEntryID,
			}
		}
		payload, err := encodeOperationRegister(next)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
		if err != nil {
			return err
		}
		adopted, err := decodeOperationRegister(replaced)
		if err != nil {
			return corruptSession(sessionID, "committed operation register %q: %v", operationID, err)
		}
		updated = adopted
		return nil
	})
	if err != nil {
		h.markCorrupt(sessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) {
			if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil {
				return "", rerr
			}
		}
		return "", err
	}
	c.graph.replaceOperation(operationID, updated)
	c.mu.Unlock()
	return resultID, nil
}

// toolShaping is one settled call's pure output-shaping product: the shaped
// base text the shared result producer renders, and the bounded processing
// failure of an aborted chain. A nil value means no shaping ran over the
// outcome and the producer's original content is the base.
type toolShaping struct {
	base    string
	failure string
}

// shapeToolOutput runs one settled call's ordered pure output-shaping chain
// over the producer's original content: every hook receives its own owned
// outcome copy and the current content text — the original content for the
// first hook, each preceding hook's return after it. The first hook error or
// return failing the unchanged result-shape rule aborts the whole chain: the
// base becomes the original content, the real outcome preserved with partial
// shaping abandoned, and one bounded processing failure names the cause.
// Shaping is text only and never changes status, evidence, usage or metadata.
func shapeToolOutput(ctx context.Context, hooks []ToolOutputHook, outcome ToolOutcome, callID string) *toolShaping {
	if len(hooks) == 0 {
		return nil
	}
	base := outcome.Result.Content
	for _, hook := range hooks {
		shaped, err := hook.Run(ctx, ToolOutputInput{
			Outcome: ToolOutcome{Result: outcome.Result, Metadata: model.CloneRaw(outcome.Metadata)},
			Content: base,
		})
		if err != nil {
			return &toolShaping{base: outcome.Result.Content, failure: shapingFailureDiagnostic(err)}
		}
		if _, verr := model.NewToolResult(model.ToolResult{CallID: callID, Status: outcome.Result.Status, Content: shaped}); verr != nil {
			return &toolShaping{base: outcome.Result.Content, failure: shapingFailureDiagnostic(fmt.Errorf("shaped output violates the unchanged status content rule: %w", verr))}
		}
		base = shaped
	}
	return &toolShaping{base: base}
}

// commitToolResult commits the one terminal result of a settled call through
// the ordinary tool-result transition: the result entry under the call's
// reserved identity, the cleared active effect, and the complete next
// Operation state in one transaction. The producer-assigned execution
// knowledge and hook-uncertainty flag decide the final model-facing content
// once through the shared result producer — over the shaped base when a pure
// output-shaping chain ran, the outcome's original content otherwise — and
// are stored alongside that original content and an aborted chain's bounded
// processing failure; the settled result returned to the caller carries that
// same final content and unchanged status. The outcome's metadata is
// committed only when it is one well-formed JSON value within the durable
// bound; invalid or oversized metadata is dropped while the result still
// commits. Immediate plans commit without an effect intent; executor-backed
// plans commit behind theirs and copy the intent's committed final arguments
// and hook provenance. The suspended running request state clears when this
// was the last pending call's result.
func (h *Harness) commitToolResult(ctx context.Context, c *coordinator, operationID string, pending PendingToolCall, outcome ToolOutcome, shaping *toolShaping, fromIntent bool, selection toolSelection, evidence ExecutionEvidence, hookUnknown bool) (model.ToolResult, error) {
	base := outcome.Result.Content
	var failure string
	if shaping != nil {
		base, failure = shaping.base, shaping.failure
	}
	content := buildToolResultContent(evidence, hookUnknown, base)
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
			selection = toolSelection{
				arguments: currentOp.State.ActiveEffect.FinalArguments,
				hooks:     currentOp.State.ActiveEffect.ContributionIDs,
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
			SessionID:           sessionID,
			EntryID:             reservation.ResultEntryID,
			OperationID:         operationID,
			AssistantEntry:      reservation.AssistantEntry,
			ToolCallID:          pending.CallID,
			Status:              outcome.Result.Status,
			Content:             content,
			OriginalContent:     outcome.Result.Content,
			ShapingFailure:      failure,
			Evidence:            evidence,
			Metadata:            ownedToolMetadata(outcome.Metadata),
			NormalizedArguments: model.CloneRaw(selection.arguments),
			HookIDs:             append([]HookIdentity(nil), selection.hooks...),
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
		if len(next.State.PendingToolCalls) == 0 {
			next.State.Request = nil // the request's final tool settlement closed its batch
		}
		opPayload, err := encodeOperationRegister(next)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(opKey, oreg.Revision, opPayload)
		if err != nil {
			return err
		}
		adoptedOp, err := decodeOperationRegister(replaced)
		if err != nil {
			return corruptSession(sessionID, "committed operation register %q: %v", operationID, err)
		}
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
		updated = adoptedOp
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
	h.observeInvalidation(c) // every tool result replaces the Session register
	h.emitToolResultFacts(newEntries, sessionID, operationID)
	settled := outcome.Result
	settled.Content = content
	return settled, nil
}

// runArgumentHooks settles one call's selected argument-hook chain before
// concrete preparation, applying the same pure-versus-effectful rule as the
// request boundary. Each hook runs in configured order: the first receives
// the original raw call arguments and every later one the preceding committed
// value. A pure hook validates and normalizes its changed bytes exactly once
// in memory with no intent or result transaction; an effectful hook runs as
// its own settled effect whose intent retains the input it actually consumed.
// Execution cancellation gates each next hook, pure or effectful: the call
// then settles interrupted carrying the prior accepted normalized value and
// contributing provenance. A failed or interrupted effectful hook settles the
// tool call itself (remaining hooks are skipped, later calls continue). It
// returns the final accepted selection, or the committed tool result when the
// chain settled the call, or the error a failed intent or result transaction
// leaves for normal recovery.
func (h *Harness) runArgumentHooks(ctx, settleCtx context.Context, c *coordinator, operationID string, pending PendingToolCall, record toolCallRecord, inv InvocationExecution) (toolSelection, *model.ToolResult, error) {
	raw, err := base64.StdEncoding.DecodeString(record.ArgumentsBase64)
	if err != nil { // a non-canonical durable record is corruption, not empty arguments
		settled, serr := h.settleHooklessInvalid(settleCtx, c, operationID, pending)
		return toolSelection{}, settled, serr
	}
	current := raw
	accepted := model.CloneRaw(record.NormalizedArguments)
	type contribution struct {
		identity HookIdentity
		pure     bool
	}
	var contributions []contribution
	accept := func(value json.RawMessage, hook ToolArgumentsHook) {
		if hook.Mode == HookModeEffectful || !bytes.Equal(value, accepted) {
			contributions = append(contributions, contribution{
				identity: HookIdentity{ExtensionID: hook.ExtensionID, HookID: hook.ID},
				pure:     hook.Mode == HookModePure,
			})
		}
		current, accepted = value, value
		if bytes.Equal(accepted, record.NormalizedArguments) {
			kept := contributions[:0]
			for _, c := range contributions {
				if !c.pure {
					kept = append(kept, c)
				}
			}
			contributions = kept
		}
	}
	// chainSelection renders what the chain has accepted so far: a hook
	// boundary settles carrying the last accepted value and provenance. Pure
	// provenance survives only while the chain net-changed the argument view
	// from the original bytes; effectful provenance always rides its
	// boundaries, and a consumed pure intermediate stays retained in the
	// effectful hook's own consumed input.
	chainSelection := func() toolSelection {
		selection := toolSelection{arguments: model.CloneRaw(accepted)}
		netChanged := !bytes.Equal(accepted, record.NormalizedArguments)
		for _, contribution := range contributions {
			if !contribution.pure || netChanged {
				selection.hooks = append(selection.hooks, contribution.identity)
			}
		}
		return selection
	}
	for _, hook := range inv.ToolHooks {
		if ctx.Err() != nil { // cancellation stops the next hook: the call settles interrupted carrying what the chain already accepted
			result := interruptedToolResult(pending.CallID)
			settled, terr := h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: result}, nil, false, chainSelection(), ExecutionNotStarted, false)
			if terr != nil {
				return toolSelection{}, nil, terr
			}
			return toolSelection{}, &settled, nil
		}
		if hook.Mode == HookModePure {
			changed, failure, knowledge := evaluateArgumentHook(ctx, record, hook, inv.NormalizeTool, current)
			if failure != nil { // the refusal settles the call through the ordinary no-intent transition carrying the chain's accepted selection; a declared unknown hook outcome leaves the concrete call unstarted but uncertain
				settled, cerr := h.commitToolResult(settleCtx, c, operationID, pending, ToolOutcome{Result: *failure}, nil, false, chainSelection(), ExecutionNotStarted, knowledge == ExecutionUnknown)
				if cerr != nil {
					return toolSelection{}, nil, cerr
				}
				return toolSelection{}, &settled, nil
			}
			accept(changed, hook)
			continue
		}
		committed, settled, hookErr := h.runOneHook(ctx, settleCtx, c, operationID, pending, record, hook, inv.NormalizeTool, current, chainSelection())
		if hookErr != nil || settled != nil {
			return toolSelection{}, settled, hookErr
		}
		accept(committed.Arguments, hook)
	}
	return chainSelection(), nil, nil
}

// evaluateArgumentHook owns and evaluates one callback's input and replacement.
// Its caller retains the pure or effectful settlement boundary; normalization
// failures are observed because the callback already produced its result.
func evaluateArgumentHook(ctx context.Context, record toolCallRecord, hook ToolArgumentsHook, normalize func(model.ToolCall) (json.RawMessage, error), current json.RawMessage) (json.RawMessage, *model.ToolResult, ExecutionEvidence) {
	owned, err := model.NewToolCall(model.ToolCall{ID: record.ID, Name: record.Name, Arguments: current, Extra: record.Extra})
	if err != nil { // the committed record's identity fields are validated; never reachable for a valid graph
		failure := invalidToolResult(record.ID)
		return nil, &failure, ExecutionNotStarted
	}
	if ctx.Err() != nil {
		failure := interruptedToolResult(record.ID)
		return nil, &failure, ExecutionNotStarted
	}
	replacement, runErr := hook.Run(ctx, owned)
	if runErr != nil {
		failure := validationToolResult(record.ID, runErr)
		if isExecutionCancellation(runErr) && ctx.Err() != nil {
			failure = interruptedToolResult(record.ID)
		}
		return nil, &failure, declaredExecutionKnowledge(runErr)
	}
	if _, err := decodePayloadObject(replacement); err != nil { // an invalid returned value is the malformed-class validation failure
		failure := invalidToolResult(record.ID)
		return nil, &failure, ExecutionObserved
	}
	normalized, nerr := normalizeCallArguments(normalize, model.ToolCall{ID: record.ID, Name: record.Name, Arguments: replacement, Extra: record.Extra})
	if nerr != nil {
		if errors.Is(nerr, errMalformedNormalization) {
			failure := invalidToolResult(record.ID)
			return nil, &failure, ExecutionObserved
		}
		failure := validationToolResult(record.ID, nerr)
		return nil, &failure, ExecutionObserved
	}
	return normalized, nil, ExecutionObserved
}

// runOneHook settles one effectful argument hook as its own Operation effect:
// it commits the hook active-effect intent carrying the extension identity and
// the consumed input bytes before I/O with a fresh reserved result identity,
// runs the hook outside locks and transactions, and commits the validated
// consequence — a successful normalized replacement (a real result even when
// cancellation arrives before its commit), a hook error, or an interruption
// under the classification fixed for every hook. The continuation consumes
// the codec-adopted committed consequence.
func (h *Harness) runOneHook(ctx, settleCtx context.Context, c *coordinator, operationID string, pending PendingToolCall, record toolCallRecord, hook ToolArgumentsHook, normalize func(model.ToolCall) (json.RawMessage, error), current json.RawMessage, chain toolSelection) (hookResultEntry, *model.ToolResult, error) {
	resultID, err := h.beginPendingCallEffect(ctx, c, operationID, pending.CallID, EffectHook, pendingCallIntent{
		hookID:               hook.ID,
		extensionID:          hook.ExtensionID,
		inputArgumentsBase64: base64.StdEncoding.EncodeToString(current),
	})
	if err != nil {
		// An intent transaction aborted by cancellation committed no
		// reservation, so no hook result exists: the call settles the existing
		// interrupted-before-execution result through the ordinary no-intent
		// transition, preserving the chain's accepted selection and preventing
		// later starts.
		if isExecutionCancellation(err) && ctx.Err() != nil {
			outcome := ToolOutcome{Result: interruptedToolResult(pending.CallID)}
			settled, terr := h.commitToolResult(settleCtx, c, operationID, pending, outcome, nil, false, chain, ExecutionNotStarted, false)
			if terr != nil {
				return hookResultEntry{}, nil, terr
			}
			return hookResultEntry{}, &settled, nil
		}
		return hookResultEntry{}, nil, err
	}
	if ctx.Err() != nil { // cancellation between the committed intent and Run: the hook provably never ran
		settled, serr := h.settleHookInterruption(settleCtx, c, operationID, pending, hook, resultID, ExecutionNotStarted, chain)
		return hookResultEntry{}, settled, serr
	}
	normalized, failure, knowledge := evaluateArgumentHook(ctx, record, hook, normalize, current)
	if failure != nil {
		if failure.Status == model.ResultInterrupted {
			settled, serr := h.settleHookInterruption(settleCtx, c, operationID, pending, hook, resultID, knowledge, chain)
			return hookResultEntry{}, settled, serr
		}
		settled, serr := h.settleHookError(settleCtx, c, operationID, pending, hook, resultID, *failure, knowledge, chain)
		return hookResultEntry{}, settled, serr
	}
	// The successful normalized output is a real result even if cancellation
	// arrives before its commit: the settlement transaction runs without
	// cancellation.
	committed, cerr := h.commitHookResult(settleCtx, c, operationID, hookEffectBinding{hookID: hook.ID, callID: pending.CallID}, resultID, hookResultEntry{Status: hookSucceeded, Evidence: ExecutionObserved, Arguments: normalized})
	if cerr != nil {
		return hookResultEntry{}, nil, cerr
	}
	return committed, nil, nil
}

// declaredExecutionKnowledge resolves one hook callback failure's execution
// knowledge: a typed EffectError carries its declared knowledge through a
// binding-infrastructure failure unchanged (an invalid declaration reaches the
// shared accepting validator and rejects, never a guessed value), and an
// ordinary native terminal callback failure is an observed execution.
func declaredExecutionKnowledge(runErr error) ExecutionEvidence {
	var typed *EffectError
	if errors.As(runErr, &typed) {
		return typed.Evidence
	}
	return ExecutionObserved
}

// isExecutionCancellation reports the cancellation-shaped classification gate
// shared by every hook boundary: only a cooperative terminal interruption
// under a genuinely canceled execution context converts a returned error into
// an interruption.
func isExecutionCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// settleHooklessInvalid settles the call's fixed internal-validation result
// through the ordinary no-intent transition, for shapes that never ran a hook.
func (h *Harness) settleHooklessInvalid(ctx context.Context, c *coordinator, operationID string, pending PendingToolCall) (*model.ToolResult, error) {
	result := invalidToolResult(pending.CallID)
	settled, err := h.commitToolResult(ctx, c, operationID, pending, ToolOutcome{Result: result}, nil, false, toolSelection{}, ExecutionNotStarted, false)
	if err != nil {
		return nil, err
	}
	return &settled, nil
}

// settleHookInterruption commits the interrupted hook_result under the
// effect's reserved identity with the intent-retained consumed input and the
// declared knowledge (not_started for a proven pre-Run interruption, observed
// for a cooperative terminal interruption) and settles the pending call
// interrupted through the existing interruption helpers; the Operation's
// terminal interruption settles through the ordinary run settlement. A hook
// that declared unknown knowledge leaves the concrete call's proven-unstarted
// result uncertain. No hook or tool replays.
func (h *Harness) settleHookInterruption(ctx context.Context, c *coordinator, operationID string, pending PendingToolCall, hook ToolArgumentsHook, resultID string, knowledge ExecutionEvidence, chain toolSelection) (*model.ToolResult, error) {
	if _, err := h.commitHookResult(ctx, c, operationID, hookEffectBinding{hookID: hook.ID, callID: pending.CallID}, resultID, hookResultEntry{
		Status: hookInterrupted, Evidence: knowledge, Error: executionInterruptedDetail,
	}); err != nil {
		return nil, err
	}
	result := interruptedToolResult(pending.CallID)
	settled, err := h.commitToolResult(ctx, c, operationID, pending, ToolOutcome{Result: result}, nil, false, chain, ExecutionNotStarted, knowledge == ExecutionUnknown)
	if err != nil {
		return nil, err
	}
	return &settled, nil
}

// settleHookError commits the error hook_result under the effect's reserved
// identity with the declared execution knowledge and the intent-retained
// consumed input, and then the ordinary error result for the tool call. Both
// carry the result's already-bounded, nonempty diagnostic; a hook that
// declared unknown knowledge leaves the concrete call's proven-unstarted
// result uncertain.
func (h *Harness) settleHookError(ctx context.Context, c *coordinator, operationID string, pending PendingToolCall, hook ToolArgumentsHook, resultID string, outcome model.ToolResult, knowledge ExecutionEvidence, chain toolSelection) (*model.ToolResult, error) {
	if _, err := h.commitHookResult(ctx, c, operationID, hookEffectBinding{hookID: hook.ID, callID: pending.CallID}, resultID, hookResultEntry{
		Status: hookFailed, Evidence: knowledge, Error: outcome.Content,
	}); err != nil {
		return nil, err
	}
	settled, err := h.commitToolResult(ctx, c, operationID, pending, ToolOutcome{Result: outcome}, nil, false, chain, ExecutionNotStarted, knowledge == ExecutionUnknown)
	if err != nil {
		return nil, err
	}
	return &settled, nil
}

// commitHookResult commits one settled effectful hook execution's result
// transaction: the hook_result entry under the effect's reserved identity,
// with the committed intent's extension identity and binding-appropriate
// consumed input stamped in unconditionally, plus the cleared active effect
// atomically — for a request-bound hook, the committed result reference
// extends the published running request state in the same transaction. On
// success it returns the codec-adopted committed entry, whose values the
// synchronous continuation consumes; every failure returns no entry and
// leaves the committed running intent for normal recovery.
func (h *Harness) commitHookResult(ctx context.Context, c *coordinator, operationID string, binding hookEffectBinding, resultID string, res hookResultEntry) (hookResultEntry, error) {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return hookResultEntry{}, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	sessionID := op.Admission.SessionID
	viewSession := c.graph.Session
	var (
		updated       OperationRecord
		committedSess SessionRecord
		newEntries    []graphEntry
		committed     hookResultEntry
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
			currentOp.State.ActiveEffect.HookID != binding.hookID ||
			currentOp.State.ActiveEffect.ResultEntryID != resultID ||
			(binding.callID != "" && currentOp.State.ActiveEffect.ToolCallID != binding.callID) ||
			(binding.requestID != "" && currentOp.State.ActiveEffect.RequestID != binding.requestID) {
			return invalidInput("operation %q carries no committed hook effect intent for hook %q", operationID, binding.hookID)
		}
		if sreg.Revision != viewSession.Revision {
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, viewSession.Revision, sreg.Revision)
		}
		if oreg.Revision != op.Revision {
			return fmt.Errorf("%w: operation %q revision %d changed concurrently to %d", errRevisionRace, operationID, op.Revision, oreg.Revision)
		}
		intent := currentOp.State.ActiveEffect
		res.SessionID = sessionID
		res.EntryID = resultID
		res.OperationID = operationID
		res.HookID = binding.hookID
		res.ExtensionID = intent.ExtensionID // the committed intent owns the extension identity through the result
		if binding.callID != "" {
			res.ToolCallID = binding.callID
			res.InputArgumentsBase64 = intent.InputArgumentsBase64
		} else {
			res.RequestID = binding.requestID
			if intent.Evidence != nil {
				consumed := *intent.Evidence
				res.InputEvidence = &consumed
			}
		}
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
		committed = *adopted.HookResult
		next := currentOp
		next.State.ActiveEffect = nil
		if binding.requestID != "" && next.State.Request != nil {
			requestState := *next.State.Request
			requestState.HookResults = append(requestState.HookResults, EntryRef{SessionID: sessionID, EntryID: resultID})
			next.State.Request = &requestState
		}
		opPayload, err := encodeOperationRegister(next)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(opKey, oreg.Revision, opPayload)
		if err != nil {
			return err
		}
		adoptedOp, err := decodeOperationRegister(replaced)
		if err != nil {
			return corruptSession(sessionID, "committed operation register %q: %v", operationID, err)
		}
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
		updated = adoptedOp
		return nil
	})
	if err != nil {
		h.markCorrupt(sessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return hookResultEntry{}, rerr
			}
		}
		return hookResultEntry{}, err
	}
	c.graph.Entries = append(c.graph.Entries, newEntries...)
	c.graph.replaceOperation(operationID, updated)
	c.graph.Session = committedSess
	c.mu.Unlock()
	h.observeInvalidation(c) // every hook result replaces the Session register; hooks have no progress kind
	return committed, nil
}

// runRequestHooks runs the opened invocation's selected request hooks in
// configured order over the live effective view: every accepted consequence
// is validated before another hook consumes it. A pure consequence validates
// and coalesces in memory with no effect transaction. An effectful hook runs
// as its own settled effect: its intent commits the consumed view before I/O,
// the callback receives the deep-owned input materialized from the committed
// intent's adopted evidence over the round's frozen canonical messages, and
// the continuation applies the codec-adopted committed consequence. A failed
// hook stops this boundary before its next external effect: the request is
// never dispatched, the failure returns for the ordinary terminal settlement,
// and an effectful failure durably settles its error hook_result.
func (h *Harness) runRequestHooks(ctx context.Context, c *coordinator, operationID string, inv InvocationExecution, requestID string, view *requestView) error {
	for _, hook := range inv.RequestHooks {
		if ctx.Err() != nil { // cancellation stops the next hook before anything commits
			return ctx.Err()
		}
		if hook.Mode == HookModePure {
			consequence, err := h.runOneRequestHookPure(ctx, requestID, view, hook)
			if err != nil {
				return err
			}
			view.recordPure(HookIdentity{ExtensionID: hook.ExtensionID, HookID: hook.ID}, consequence)
			continue
		}
		if err := h.runRequestHookEffect(ctx, c, operationID, hook, requestID, view); err != nil {
			return err
		}
	}
	return nil
}

// runOneRequestHookPure validates one pure consequence against the captured
// catalog and shared message rule and returns it owned; nothing durable is
// written for a pure change. The hook input carries the bound request
// identity, the frozen canonical references, and deep-owned views.
func (h *Harness) runOneRequestHookPure(ctx context.Context, requestID string, view *requestView, hook RequestHook) (RequestConsequence, error) {
	input := RequestHookInput{
		RequestID:         requestID,
		CanonicalMessages: cloneRequestMessages(view.canonical),
		Messages:          cloneRequestMessages(view.currentCanonical()),
		CheckpointEntryID: view.checkpoint,
		BoundaryEntryID:   view.boundary,
		CapturedPrompt:    view.capture.SystemPrompt,
		Prompt:            view.prompt,
		Catalog:           cloneToolDefinitions(view.capture.Tools),
		Tools:             view.effectiveTools(),
	}
	consequence, err := hook.Run(ctx, input)
	if err != nil {
		return RequestConsequence{}, err
	}
	return validateRequestConsequence(view.capture, consequence)
}

// runRequestHookEffect settles one effectful request hook as its own
// Operation effect.
func (h *Harness) runRequestHookEffect(ctx context.Context, c *coordinator, operationID string, hook RequestHook, requestID string, view *requestView) error {
	// The consumed evidence the hook will actually receive is rendered from
	// the live view before anything durable commits.
	consumed := view.evidenceFor()
	resultID, err := h.beginRequestHookEffect(ctx, c, operationID, requestID, hook.ID, hook.ExtensionID, consumed)
	if err != nil {
		return err
	}
	if ctx.Err() != nil { // cancellation between the committed intent and Run: the hook provably never ran
		if _, cerr := h.commitHookResult(context.WithoutCancel(h.ctx), c, operationID,
			hookEffectBinding{hookID: hook.ID, requestID: requestID}, resultID, hookResultEntry{
				Status: hookInterrupted, Evidence: ExecutionNotStarted, Error: executionInterruptedDetail,
			}); cerr != nil {
			return cerr
		}
		return ctx.Err() // the Engine's own cancellation checkpoints convert this into the typed interruption
	}
	// The callback consumes the adopted evidence the committed intent owns,
	// materialized over the round's frozen canonical context — never a reread
	// projection.
	committedIntent, err := h.readRequestHookIntent(c, operationID, resultID)
	if err != nil {
		return err
	}
	materialized := materializeRequestView(view.capture, view.canonical, *committedIntent.Evidence)
	view.prompt, view.additions, view.names = materialized.prompt, materialized.additions, materialized.names
	input := RequestHookInput{
		RequestID:         requestID,
		CanonicalMessages: cloneRequestMessages(view.canonical),
		Messages:          cloneRequestMessages(materialized.currentCanonical()),
		CheckpointEntryID: committedIntent.Evidence.CheckpointEntryID,
		BoundaryEntryID:   committedIntent.Evidence.BoundaryEntryID,
		CapturedPrompt:    view.capture.SystemPrompt,
		Prompt:            materialized.prompt,
		Catalog:           cloneToolDefinitions(view.capture.Tools),
		Tools:             materialized.effectiveTools(),
	}
	consequence, runErr := hook.Run(ctx, input)
	settleCtx := context.WithoutCancel(h.ctx)
	if runErr != nil {
		knowledge := declaredExecutionKnowledge(runErr)
		status := hookFailed
		if isExecutionCancellation(runErr) && ctx.Err() != nil { // a cooperative terminal interruption
			status = hookInterrupted
		}
		if _, cerr := h.commitHookResult(settleCtx, c, operationID,
			hookEffectBinding{hookID: hook.ID, requestID: requestID}, resultID, hookResultEntry{
				Status: status, Evidence: knowledge, Error: failureDetail(runErr),
			}); cerr != nil {
			return cerr
		}
		return runErr
	}
	accepted, verr := validateRequestConsequence(view.capture, consequence)
	if verr != nil { // an invalid consequence settles the hook effect as its error before any view advances
		if _, cerr := h.commitHookResult(settleCtx, c, operationID,
			hookEffectBinding{hookID: hook.ID, requestID: requestID}, resultID, hookResultEntry{
				Status: hookFailed, Evidence: ExecutionObserved, Error: boundedToolDiagnostic(verr),
			}); cerr != nil {
			return cerr
		}
		return verr
	}
	// The durable commit publishes first; the continuation advances the view
	// from the codec-adopted committed consequence, never the raw callback
	// return.
	entry, cerr := h.commitHookResult(settleCtx, c, operationID,
		hookEffectBinding{hookID: hook.ID, requestID: requestID}, resultID, hookResultEntry{
			Status: hookSucceeded, Evidence: ExecutionObserved, Consequence: &accepted,
		})
	if cerr != nil {
		return cerr
	}
	applied := *entry.Consequence
	view.recordEffectful(HookIdentity{ExtensionID: hook.ExtensionID, HookID: hook.ID}, applied)
	return nil
}

// readRequestHookIntent returns the committed request-hook effect intent of
// the given reserved identity from the coordinator's validated view.
func (h *Harness) readRequestHookIntent(c *coordinator, operationID, resultID string) (*ActiveEffect, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		return nil, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, c.graph.Session.Identity.SessionID)
	}
	if op.State.ActiveEffect == nil || op.State.ActiveEffect.Kind != EffectHook ||
		op.State.ActiveEffect.ResultEntryID != resultID || op.State.ActiveEffect.Evidence == nil {
		return nil, invalidInput("operation %q carries no committed request hook effect intent for result identity %q", operationID, resultID)
	}
	return ownActiveEffect(op.State.ActiveEffect), nil
}

// execute is the private agent.Run composition of one admitted execution:
// after the admission it invokes the preparation's opener exactly once with
// the execution context and the owned committed admission, and runs the same
// Agent/compaction work over the opened effects. It returns a pointer to the
// actual Agent terminal — including its zero value on an Agent error — and
// the valid execution's Close; the conversation outer settlement belongs to
// finishExecution and never happens here. Entry/Open/invalid-Execution
// rejection returns a non-nil zero terminal for ordinary outer error
// classification; an invalid execution still closes immediately and returns
// no cleanup function. The valid compact branch returns a nil terminal, its
// Close and its original error: its compaction path already owns every
// settlement attempt, so completion must not feed that publication error
// into another terminal attempt. A missing Operation returns nil terminal, no
// Close, and the original lookup error. No public Agent result is forged.
// The execution context is execCtx: Interrupt cancels it, and closure or a
// consumed interrupt marker settles the durable Operation as interruption at
// entry — genuinely observed control cancellation is represented there as an
// explicit interruption result, while a cancellation-shaped opener error under
// a live execution context stays the original infrastructure error for
// failure classification. The Agent's expected model and advertised tools
// come from an independent capture retained before the opener runs, so an
// opener mutating its admission input locally never changes what is
// advertised after admission. An opener error resolves through the ordinary
// outer terminal settlement: an explicit boundary interruption interrupts, a
// storage failure retains the running state for recovery, and any other error
// fails. A canceled execution skips an unstarted opener. The conversation
// Engine call opens exactly one invocation immediately before its run, and
// every opened invocation is validated as its own contract. A cleanup failure
// never rewrites the terminal Operation; it is retained for Wait alongside
// the first storage failure. The returned closers run in order: the opened
// conversation invocation first, then the opened execution.
func (h *Harness) execute(c *coordinator, operationID string, prepared PreparedExecution, execCtx context.Context) (*agent.TerminalResult, func() error, func() error, error) {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	admission := ownOperationRecord(op).Admission
	agentCapture := ownCapture(admission.Execution)
	guard := c.bgState == bgClosed || c.interruptOp == operationID
	if c.interruptOp == operationID {
		c.interruptOp = "" // consumed: the marker reaches exactly the execution it names
	}
	c.mu.Unlock()
	if guard || h.ctx.Err() != nil { // the entry guard joins the harness-loss early return: the opener never starts; genuine control cancellation is the explicit interruption
		interrupted := agent.TerminalResult{Status: agent.TerminalInterruption, Detail: executionInterruptedDetail}
		return &interrupted, nil, nil, nil
	}
	exec, err := prepared.Open(execCtx, admission)
	if err != nil {
		if res := executionCancellationResult(execCtx, err); res != nil {
			return res, nil, nil, nil
		}
		return &agent.TerminalResult{}, nil, nil, err
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
		return &agent.TerminalResult{}, nil, nil,
			invalidInput("opened execution requires a non-nil invocation opener")
	}
	// A compact Operation wires no conversation Agent invocation: it projects
	// the pure canonical conversation snapshot — the projected messages minus
	// the leading system message, with no compact input to exclude — runs the
	// selected strategy's maintenance procedure over it, and commits the
	// manual checkpoint whose one transaction also settles the Operation's
	// success. The orchestration
	// opens its own maintenance invocations; every orchestration failure or
	// interruption has already settled the terminal durably with the retained
	// facts (a model failure inside its own effect, any other failure
	// through the direct terminal settlement), and only the checkpoint
	// publication failure returns its error with the running state retained.
	if admission.RequestKind == RequestKindCompact {
		messages, checkpointID, boundaryID, injected, err := h.projectContextRefs(c, operationID)
		if err != nil {
			return nil, nil, exec.Close, err
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
			Instructions:      agentCapture.Compact.SystemPrompt,
			Tools:             cloneToolDefinitions(agentCapture.Tools),
			Measurement:       contextMeasurement(c),
			ContextWindow:     agentCapture.ContextWindow,
			OutputReserve:     agentCapture.OutputReserve,
			Settings:          model.CloneRaw(agentCapture.Strategy.Settings),
		}
		checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(execCtx, c, operationID, exec, agentCapture, planInput, nil)
		if err != nil {
			return nil, closeMaintenance, exec.Close, err
		}
		// The completion owner closes the maintenance invocation only after
		// this returned transaction — the manual compaction's commit is also
		// its terminal settlement — so the checkpoint precedes the disposal.
		return nil, closeMaintenance, exec.Close, h.commitMaintenance(execCtx, c, operationID, agentCapture, planInput, facts, checkpoint, true)
	}
	invocationID, err := newHexID() // the Engine call's fresh per-invocation identity
	if err != nil {
		return &agent.TerminalResult{}, nil, exec.Close, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	inv, err := exec.OpenInvocation(execCtx, InvocationRequest{
		ID:      invocationID,
		Purpose: InvocationPurposeConversation,
		Model:   agentCapture.Model,
	})
	if err != nil {
		if res := executionCancellationResult(execCtx, err); res != nil {
			return res, exec.Close, nil, nil
		}
		return &agent.TerminalResult{}, nil, exec.Close, err
	}
	if err := validateOpenedInvocation(inv); err != nil {
		if inv.Close != nil {
			h.recordCleanupFailure(inv.Close())
		}
		return &agent.TerminalResult{}, nil, exec.Close, err
	}
	round := &conversationRound{}
	res, runErr := runEngineCall(execCtx, agent.Invocation{
		ExpectedModel: agentCapture.Model,
		Request:       h.requestSource(c, operationID, agentCapture, exec, inv, round),
		ModelEffect:   h.modelEffect(c, operationID, exec, inv, agentCapture, round),
		ToolEffect:    h.toolEffect(c, operationID, exec, inv, agentCapture),
	})
	// The Engine's already-established result or error is returned verbatim:
	// no context sampled after the Engine may relabel it.
	return &res, inv.Close, exec.Close, runErr
}

// executionCancellationResult classifies one pre-Engine execution boundary: a
// cancellation-shaped error under a genuinely done execution context is
// deliberate control cancellation and returns the explicit interruption
// terminal result; under a live context the same error shape stays the
// original infrastructure error and this returns nil. It is applied only at
// execute's entry guard, the preparation opener, and the invocation openers —
// never after the Engine has produced its own authoritative outcome.
func executionCancellationResult(execCtx context.Context, err error) *agent.TerminalResult {
	if err != nil && isExecutionCancellation(err) && execCtx.Err() != nil {
		return &agent.TerminalResult{Status: agent.TerminalInterruption, Detail: executionInterruptedDetail}
	}
	return nil
}

// validateOpenedInvocation is the opened invocation's contract: a successful
// Model, Tool and NormalizeTool must be non-nil, every argument and
// request hook requires a non-empty unique ID, a non-empty extension
// identity, a valid mode, and a non-nil Run, while every output hook — pure
// by contract — requires the same identities and Run without a mode. The
// selected strategy binding is accepted once at the maintenance entry, never
// re-gated per invocation. An invalid invocation closes its own opened
// resources before the rejection returns.
func validateOpenedInvocation(inv InvocationExecution) error {
	if inv.Model == nil || inv.Tool == nil || inv.NormalizeTool == nil {
		return invalidInput("opened invocation requires non-nil model, tool and normalization functions")
	}
	seenHooks := make(map[string]bool, len(inv.ToolHooks))
	for _, hook := range inv.ToolHooks {
		if hook.ID == "" || hook.Run == nil || hook.ExtensionID == "" || !validHookMode(hook.Mode) || seenHooks[hook.ID] {
			return invalidInput("opened invocation requires a non-empty unique ID, a non-empty extension identity, a valid mode, and a non-nil run function for every argument hook")
		}
		seenHooks[hook.ID] = true
	}
	seenOutputHooks := make(map[string]bool, len(inv.ToolOutputHooks))
	for _, hook := range inv.ToolOutputHooks {
		if hook.ID == "" || hook.Run == nil || hook.ExtensionID == "" || seenOutputHooks[hook.ID] {
			return invalidInput("opened invocation requires a non-empty unique ID, a non-empty extension identity, and a non-nil run function for every output hook")
		}
		seenOutputHooks[hook.ID] = true
	}
	seenRequestHooks := make(map[string]bool, len(inv.RequestHooks))
	for _, hook := range inv.RequestHooks {
		if hook.ID == "" || hook.Run == nil || hook.ExtensionID == "" || !validHookMode(hook.Mode) || seenRequestHooks[hook.ID] {
			return invalidInput("opened invocation requires a non-empty unique ID, a non-empty extension identity, a valid mode, and a non-nil run function for every request hook")
		}
		seenRequestHooks[hook.ID] = true
	}
	return nil
}

// finishExecution is the one completion owner of a finished execution: it
// acquires the Session's admission reservation once before the outer terminal
// settlement and releases it as a local deferred call, settles a non-nil
// terminal exactly once through the shared publisher (a nil terminal
// preserves the already-owned compact/lookup outcome without another
// attempt), records the resulting storage-class error once, closes a non-nil
// opened invocation once, then a non-nil execution Close once, then releases
// the adopted preparation reservation once — every retained cleanup error
// keeps its existing Wait owner — and invokes the ordinary drain while the
// reservation is held so a successor installs before the release. A canceled
// contended acquisition skips only the drain.
func (h *Harness) finishExecution(c *coordinator, run *activeExecution, operationID string, prepared PreparedExecution, res *agent.TerminalResult, runErr error, closeInvocation, closeExecution func() error) error {
	release, reserveErr := h.reserve(h.ctx, c)
	if reserveErr == nil {
		defer release()
	}
	err := runErr
	if res != nil {
		err = h.settleAgentTerminal(c, operationID, *res, runErr, run.execCtx)
	}
	h.recordStorageFailure(err) // the sole storage-class recorder, including nil-terminal outcomes
	if closeInvocation != nil {
		h.recordCleanupFailure(closeInvocation())
	}
	if closeExecution != nil {
		h.recordCleanupFailure(closeExecution())
	}
	h.releasePrepared(&prepared) // the adopted reservation releases after the execution's Close, on every completion path
	if reserveErr == nil {
		h.drainBuffers(c, run)
	}
	return err
}

// settleAgentTerminal is the outer terminal settlement: terminal settlement
// after opening or running the Agent reuses the common terminal helper only
// when the Operation is still running, because model-effect-originated
// terminals already settled durably inside their own result transactions. The
// private boundary token is consumed here by exact identity before the
// settled/active-effect guard: it selects the prospective quiet success and
// clears the private run error, which the guard then returns; no invalid
// Agent TerminalSuccess is manufactured. A non-storage callback or Agent
// protocol error — including a local hook or opener failure with a
// cancellation-shaped error under a live execution context — settles failure
// with the error's own text: no error shape and no later context sample
// relabels an Engine result as interruption. Interruption terminals come only
// from the Agent's typed terminal result or from an execute boundary's
// explicit interruption result. The existing settled/active-effect/ErrStorage
// discriminator constructs one modelResult and calls the shared publisher
// once. A committed running/intent state left by a publication failure stays
// for recovery.
func (h *Harness) settleAgentTerminal(c *coordinator, operationID string, res agent.TerminalResult, runErr error, execCtx context.Context) error {
	prospectiveSuccess := runErr == errSteeringBoundary // exact identity only; a projection/model/tool error is never this token
	if prospectiveSuccess {                             // consumed before the guards: the private run error clears to its settled value
		runErr = nil
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
	if settled || !quiet { // already settled by a model effect, or a committed intent state left for recovery
		return runErr
	}
	if isPublicationFailure(runErr) { // classified by the publication boundary it occurred at: the durable running state stays for recovery
		return runErr
	}
	if runErr != nil && errors.Is(runErr, ErrStorage) { // a storage-class error performs no compensating write: the committed running state stays for recovery
		return runErr
	}
	settleCtx := context.WithoutCancel(h.ctx)
	var result modelResult
	switch {
	case runErr != nil:
		result = modelResult{terminal: OperationFailure, detail: failureDetail(runErr)}
	case prospectiveSuccess, res.Status == agent.TerminalSuccess:
		result = modelResult{terminal: OperationSuccess, successContext: execCtx}
	case res.Status == agent.TerminalFailure, res.Status == agent.TerminalInterruption:
		term := OperationFailure
		if res.Status == agent.TerminalInterruption {
			term = OperationInterruption
		}
		result = modelResult{terminal: term, detail: res.Detail}
	default:
		return nil // a zero terminal without an error carries nothing to publish
	}
	if _, err := h.commitEffectResult(settleCtx, c, operationID, nil, result); err != nil {
		return err
	}
	return runErr
}

// materializeRequestView rebuilds one effective view from the frozen capture
// and canonical messages plus one committed evidence value: the adopted
// representation the durable boundary owns, never a fresh-history reread.
func materializeRequestView(capture ExecutionCapture, canonical []model.Message, evidence RequestEvidence) *requestView {
	view := newRequestView(capture, evidence.CheckpointEntryID, evidence.BoundaryEntryID, canonical)
	if evidence.Prompt != nil {
		view.prompt = *evidence.Prompt
	}
	view.additions = cloneRequestMessages(evidence.Messages)
	if evidence.ToolNames != nil && !coversCaptured(evidence.ToolNames, capture.Tools) {
		view.names = append([]string{}, evidence.ToolNames...)
	}
	return view
}
