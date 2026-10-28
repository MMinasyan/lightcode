package harness

import (
	"context"
	"fmt"

	"github.com/MMinasyan/lightcode/model"
)

// graphEntry is one committed entry of a validated Session graph: the storage
// envelope plus exactly one typed decoded payload, selected by the envelope
// kind.
type graphEntry struct {
	Envelope   Entry
	Input      *inputEntry
	Assistant  *assistantEntry
	ToolResult *toolResultEntry
	Signal     *signalEntry
	HookResult *hookResultEntry
	Settlement *operationSettlementEntry
	Compaction *contextCheckpointEntry
}

// sessionGraph is the owned decoded view of one Session: the Session record,
// every Operation record in storage read order, and every entry in committed
// order. It is produced only by the graph validator and carries no
// coordinator behavior.
type sessionGraph struct {
	Session    SessionRecord
	Operations []OperationRecord
	Entries    []graphEntry
}

// Operation returns the operation record addressed by identity.
func (g *sessionGraph) Operation(operationID string) (OperationRecord, bool) {
	for _, op := range g.Operations {
		if op.Admission.OperationID == operationID {
			return op, true
		}
	}
	return OperationRecord{}, false
}

// corruptSession reports one persisted semantic violation owned by one
// Session: the Session becomes unavailable while valid siblings stay usable.
func corruptSession(sessionID, format string, args ...any) error {
	return &CorruptionError{SessionID: sessionID, Detail: fmt.Sprintf(format, args...)}
}

// validateSessionGraph reads one Session exactly once through the Storage
// public reads only (the Session register first, then all Operation registers
// in landed order via one ReadRegisters call, then every entry), decodes every
// payload, verifies the closed invariant set, and returns the owned decoded
// view. Persisted semantic violations surface as CorruptionError for the
// owning Session; storage-service failures pass through unmodified and are
// never reclassified, and an addressed-but-absent record passes the landed
// not-found class through. The validator performs no mutation.
func validateSessionGraph(ctx context.Context, store Storage, sessionID string) (*sessionGraph, error) {
	if err := validateHexID(sessionID, "session id"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	registers, err := store.ReadRegisters(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	entries, err := store.ReadEntries(ctx, sessionID, 0)
	if err != nil {
		return nil, err
	}

	var session *SessionRecord
	operations := make([]OperationRecord, 0, len(registers))
	for _, reg := range registers {
		if reg.Key.SessionID != sessionID {
			return nil, corruptSession(sessionID, "register key addresses session %q", reg.Key.SessionID)
		}
		switch reg.Key.Kind {
		case RegisterSession:
			if session != nil {
				return nil, corruptSession(sessionID, "duplicate Session register")
			}
			rec, err := decodeSessionRegister(reg)
			if err != nil {
				return nil, corruptSession(sessionID, "session register: %v", err)
			}
			session = &rec
		case RegisterOperation:
			rec, err := decodeOperationRegister(reg)
			if err != nil {
				return nil, corruptSession(sessionID, "operation register %q: %v", reg.Key.OperationID, err)
			}
			operations = append(operations, rec)
		default:
			return nil, corruptSession(sessionID, "unknown register kind %q", reg.Key.Kind)
		}
	}

	graph := &sessionGraph{Operations: operations}
	for _, env := range entries {
		if env.SessionID != sessionID {
			return nil, corruptSession(sessionID, "entry envelope addresses session %q", env.SessionID)
		}
		entry, err := decodeGraphEntry(sessionID, env)
		if err != nil {
			return nil, err
		}
		graph.Entries = append(graph.Entries, entry)
	}

	if session == nil {
		if len(operations) > 0 || len(graph.Entries) > 0 {
			return nil, corruptSession(sessionID, "entries or Operation registers exist without a Session register")
		}
		return nil, fmt.Errorf("%w: session %q not found", ErrNotFound, sessionID)
	}
	graph.Session = *session

	if err := validateGraphInvariants(sessionID, graph); err != nil {
		return nil, err
	}
	return graph, nil
}

// decodeGraphEntry decodes one entry envelope according to its kind. Unknown
// kinds are corruption.
func decodeGraphEntry(sessionID string, env Entry) (graphEntry, error) {
	entry := graphEntry{Envelope: env}
	var err error
	switch env.Kind {
	case EntryInput:
		var v inputEntry
		if v, err = decodeInputEntry(env); err == nil {
			entry.Input = &v
		}
	case EntryAssistant:
		var v assistantEntry
		if v, err = decodeAssistantEntry(env); err == nil {
			entry.Assistant = &v
		}
	case EntryToolResult:
		var v toolResultEntry
		if v, err = decodeToolResultEntry(env); err == nil {
			entry.ToolResult = &v
		}
	case EntrySignal:
		var v signalEntry
		if v, err = decodeSignalEntry(env); err == nil {
			entry.Signal = &v
		}
	case EntryHookResult:
		var v hookResultEntry
		if v, err = decodeHookResultEntry(env); err == nil {
			entry.HookResult = &v
		}
	case EntryOperationSettlement:
		var v operationSettlementEntry
		if v, err = decodeOperationSettlementEntry(env); err == nil {
			entry.Settlement = &v
		}
	case EntryCompaction:
		var v contextCheckpointEntry
		if v, err = decodeContextCheckpointEntry(env); err == nil {
			entry.Compaction = &v
		}
	default:
		return graphEntry{}, corruptSession(sessionID, "entry %s: unknown kind %q", env.ID, env.Kind)
	}
	if err != nil {
		return graphEntry{}, corruptSession(sessionID, "entry %s: %v", env.ID, err)
	}
	return entry, nil
}

// publishedCall is one tool call published by an assistant entry, with the
// publication facts the exactly-once accounting needs.
type publishedCall struct {
	callID           string
	assistantEntryID string
	reservedResultID string
}

// validateGraphInvariants verifies the closed invariant set over the decoded
// graph. Every violation is corruption of the owning Session.
func validateGraphInvariants(sessionID string, graph *sessionGraph) error {
	state := &graphValidation{sessionID: sessionID, graph: graph}
	return state.run()
}

type graphValidation struct {
	sessionID    string
	graph        *sessionGraph
	opsByID      map[string]*OperationRecord
	entryByID    map[string]graphEntry
	entriesByOp  map[string][]int
	reservations map[string]publishedReservation
	cutSafety    map[string]string
}

// buildCutSafety computes one cut-safety verdict per canonical boundary of
// the indexed immutable entry sequence in a single cumulative
// maximum-result-sequence pass over the carried calls: a boundary is unsafe
// exactly when a tool call published at or before it has a missing result or
// a reserved-result sequence beyond it, and the table retains one offending
// call ID for the existing diagnostics. A missing result keeps every later
// cut unsafe. The table is a per-pass computation over the graph's entries:
// no persistent index and no storage surface.
func buildCutSafety(entries []graphEntry) map[string]string {
	byID := make(map[string]graphEntry, len(entries))
	for i := range entries {
		byID[entries[i].Envelope.ID] = entries[i]
	}
	unsafe := make(map[string]string)
	maxResult := int64(-1)
	maxCall := ""
	missingCall := ""
	for i := range entries {
		entry := entries[i]
		if entry.Assistant != nil {
			for _, call := range entry.Assistant.ToolCalls {
				result, ok := byID[call.ResultEntryID]
				if !ok {
					if missingCall == "" {
						missingCall = call.ID
					}
					continue
				}
				if result.Envelope.Sequence > maxResult {
					maxResult = result.Envelope.Sequence
					maxCall = call.ID
				}
			}
		}
		switch {
		case missingCall != "":
			unsafe[entry.Envelope.ID] = missingCall
		case maxResult > entry.Envelope.Sequence:
			unsafe[entry.Envelope.ID] = maxCall
		}
	}
	return unsafe
}

func (g *sessionGraph) operationIndex() map[string]*OperationRecord {
	index := make(map[string]*OperationRecord, len(g.Operations))
	for i := range g.Operations {
		index[g.Operations[i].Admission.OperationID] = &g.Operations[i]
	}
	return index
}

// replaceOperation updates the cached view record of exactly the addressed
// Operation in place and reports whether the view carried it: one committed
// state adoption replaces one slice record without allocating the full
// validation index, which remains the validator's duplicate/reference map.
func (g *sessionGraph) replaceOperation(operationID string, rec OperationRecord) bool {
	for i := range g.Operations {
		if g.Operations[i].Admission.OperationID == operationID {
			g.Operations[i] = rec
			return true
		}
	}
	return false
}

func (v *graphValidation) corrupt(format string, args ...any) error {
	return corruptSession(v.sessionID, format, args...)
}

func (v *graphValidation) run() error {
	v.opsByID = v.graph.operationIndex()
	if len(v.opsByID) != len(v.graph.Operations) {
		return v.corrupt("duplicate Operation identity among Operation registers")
	}
	v.entryByID = make(map[string]graphEntry, len(v.graph.Entries))
	// entriesByOp groups committed entry indexes by owning Operation ID,
	// preserving sequence order, so per-Operation scans traverse only their own
	// operation's entries instead of rescanning the whole history. It is a
	// transient index into graph.Entries: indexes only, no payload copies, and
	// it is discarded with the validation pass that built it. Operationless
	// entries remain available to the Session-wide checks on graph.Entries.
	v.entriesByOp = make(map[string][]int, len(v.graph.Operations))
	for i, entry := range v.graph.Entries {
		if _, dup := v.entryByID[entry.Envelope.ID]; dup {
			return v.corrupt("duplicate entry id %q", entry.Envelope.ID)
		}
		v.entryByID[entry.Envelope.ID] = entry
		if opID := entry.Envelope.OperationID; opID != "" {
			v.entriesByOp[opID] = append(v.entriesByOp[opID], i)
		}
	}
	// One cut-safety table for every checkpoint cut this validation checks:
	// built once over the indexed immutable entry sequence, never once per
	// checkpoint, and discarded with the validation pass that built it.
	v.cutSafety = buildCutSafety(v.graph.Entries)
	if err := v.validateSessionState(); err != nil {
		return err
	}
	if err := v.validateEntryOwnership(); err != nil {
		return err
	}
	if err := v.collectReservations(); err != nil {
		return err
	}
	if err := v.validateReservations(); err != nil {
		return err
	}
	if err := v.validateForkPrefixOwnership(); err != nil {
		return err
	}
	if err := v.validateEntryReferences(); err != nil {
		return err
	}
	for i := range v.graph.Operations {
		if err := v.validateOperation(&v.graph.Operations[i]); err != nil {
			return err
		}
	}
	return v.validateUsage()
}

// validateSessionState verifies the current-Operation agreement rules: at
// most one running Operation, exact agreement with the Session's current
// identity, no running Operation in an archived Session, and a projection
// reference naming an in-session compaction entry when present.
func (v *graphValidation) validateSessionState() error {
	var running []string
	for id, op := range v.opsByID {
		if op.State.Status == OperationRunning {
			running = append(running, id)
		}
	}
	session := &v.graph.Session
	if len(running) > 1 {
		return v.corrupt("%d running Operations, at most one is durable", len(running))
	}
	if session.State.CurrentOperationID != "" {
		op, ok := v.opsByID[session.State.CurrentOperationID]
		if !ok {
			return v.corrupt("current Operation %q has no Operation register", session.State.CurrentOperationID)
		}
		if op.State.Status != OperationRunning {
			return v.corrupt("current Operation %q is %s, not running", session.State.CurrentOperationID, op.State.Status)
		}
	} else if len(running) == 1 {
		return v.corrupt("running Operation %q is not named as the Session current Operation", running[0])
	}
	if session.State.Lifecycle == LifecycleArchived && len(running) > 0 {
		return v.corrupt("running Operation %q in an archived Session", running[0])
	}
	if session.State.CheckpointEntryID != "" {
		compaction, ok := v.entryByID[session.State.CheckpointEntryID]
		if !ok || compaction.Compaction == nil {
			return v.corrupt("checkpoint_entry_id %q names no in-session context checkpoint entry", session.State.CheckpointEntryID)
		}
	}
	return nil
}

// validateEntryOwnership verifies every owned entry names an existing
// Operation register of this Session, and the one durable input rule: an
// Operation-owned input is exactly its owning message Operation's admitted
// entry. A compact Operation owns no input and an extra input under a message
// Operation is corruption; independently copied inputs omit Operation
// ownership and stay valid.
func (v *graphValidation) validateEntryOwnership() error {
	for _, entry := range v.graph.Entries {
		if entry.Envelope.OperationID == "" {
			continue
		}
		op, ok := v.opsByID[entry.Envelope.OperationID]
		if !ok {
			return v.corrupt("entry %s is owned by unknown Operation %q", entry.Envelope.ID, entry.Envelope.OperationID)
		}
		if entry.Input == nil {
			continue
		}
		// the one valid set: an owned input is exactly its message
		// Operation's admitted entry; everything outside it is one ordinary
		// typed corruption
		if op.Admission.RequestKind != RequestKindMessage || op.Admission.AdmittedEntry.EntryID != entry.Envelope.ID {
			return v.corrupt("input entry %s is not its owning operation's admitted input", entry.Envelope.ID)
		}
	}
	return nil
}

// publishedReservation is one tool-call result reservation published by any
// committed assistant entry of the Session, operation-owned or copied.
type publishedReservation struct {
	callID           string
	assistantEntryID string
	operationID      string
}

// hookExecution is one comparable hook-execution identity: the extension and
// hook IDs plus exactly one binding — the tool call it executed for, or the
// request it executed for. The two binding families stay disjoint even for
// equal opaque strings and for a capability used at both boundaries.
type hookExecution struct {
	extensionID string
	hookID      string
	callID      string
	requestID   string
}

// collectReservations builds the one session-wide reservation index from
// every committed assistant entry, owned or copied: two calls reserving one
// result identity anywhere in the Session is corruption. Per-operation
// accounting and the copied-reference checks consult this shared producer.
func (v *graphValidation) collectReservations() error {
	v.reservations = make(map[string]publishedReservation, len(v.graph.Entries))
	for _, entry := range v.graph.Entries {
		if entry.Assistant == nil {
			continue
		}
		for _, call := range entry.Assistant.ToolCalls {
			if prior, dup := v.reservations[call.ResultEntryID]; dup {
				return v.corrupt("result entry %q is reserved by calls %q and %q", call.ResultEntryID, prior.callID, call.ID)
			}
			v.reservations[call.ResultEntryID] = publishedReservation{
				callID:           call.ID,
				assistantEntryID: entry.Envelope.ID,
				operationID:      entry.Envelope.OperationID,
			}
		}
	}
	return nil
}

// validateReservations requires every reservation naming a committed entry to
// name exactly that call's terminal tool result: a collision with a
// differently-owned result or with an input, assistant, signal, or settlement
// entry is corruption. Operation-owned reservations without a committed
// result are the valid pending and unresolved shapes; a copied prefix call's
// reservation is a rewritten reference that must resolve inside the copied
// prefix, so it may never stay uncommitted.
func (v *graphValidation) validateReservations() error {
	for resultID, res := range v.reservations {
		committed, ok := v.entryByID[resultID]
		if !ok {
			if res.operationID == "" {
				return v.corrupt("copied call %q of assistant entry %s reserves result identity %q with no copied result entry", res.callID, res.assistantEntryID, resultID)
			}
			continue
		}
		if committed.Envelope.Kind != EntryToolResult || committed.ToolResult == nil ||
			committed.Envelope.OperationID != res.operationID ||
			committed.ToolResult.ToolCallID != res.callID ||
			committed.ToolResult.AssistantEntry.EntryID != res.assistantEntryID {
			return v.corrupt("result identity %q reserved by call %q of assistant entry %s names %s entry %s, not that call's terminal tool result",
				resultID, res.callID, res.assistantEntryID, committed.Envelope.Kind, committed.Envelope.ID)
		}
	}
	return nil
}

// validateForkPrefixOwnership verifies the copied-prefix shape: a non-fork
// Session carries no operationless entries except background_completion
// signals (child-lineage completion history, allowed on every Session and
// exempt from prefix ordering), and in a fork every other operationless entry
// precedes every operation-owned entry, so the copied prefix stays a strict
// sequence prefix.
func (v *graphValidation) validateForkPrefixOwnership() error {
	fork := v.graph.Session.Identity.SourceSessionID != ""
	var maxCopiedSeq, minOwnedSeq int64
	var maxCopiedID, minOwnedID string
	for _, entry := range v.graph.Entries {
		if entry.Envelope.OperationID != "" {
			if minOwnedID == "" || entry.Envelope.Sequence < minOwnedSeq {
				minOwnedSeq, minOwnedID = entry.Envelope.Sequence, entry.Envelope.ID
			}
			continue
		}
		if entry.Signal != nil && entry.Signal.Signal == SignalBackgroundCompletion {
			continue
		}
		if !fork {
			return v.corrupt("root Session carries independently copied entry %s", entry.Envelope.ID)
		}
		if maxCopiedID == "" || entry.Envelope.Sequence > maxCopiedSeq {
			maxCopiedSeq, maxCopiedID = entry.Envelope.Sequence, entry.Envelope.ID
		}
	}
	if fork && maxCopiedID != "" && minOwnedID != "" && maxCopiedSeq >= minOwnedSeq {
		return v.corrupt("copied entry %s does not precede every operation-owned entry (first owned %s at sequence %d)", maxCopiedID, minOwnedID, minOwnedSeq)
	}
	return nil
}

// validateEntryReferences verifies the reference rules that fall outside one
// Operation's accounting: a copied fork-prefix tool result resolves inside
// the copied prefix, an owned signal's related Operation resolves inside the
// owning Session while a copied signal's informational history may not, and
// a compaction entry's boundary names an in-session entry strictly before it.
func (v *graphValidation) validateEntryReferences() error {
	for _, entry := range v.graph.Entries {
		switch {
		case entry.Envelope.OperationID == "" && entry.ToolResult != nil:
			ref := entry.ToolResult.AssistantEntry
			published, ok := v.entryByID[ref.EntryID]
			if ref.SessionID != v.sessionID || !ok {
				return v.corrupt("copied tool result %s references assistant entry %q outside the copied prefix", entry.Envelope.ID, ref.EntryID)
			}
			if published.Envelope.Kind != EntryAssistant || published.Envelope.OperationID != "" {
				return v.corrupt("copied tool result %s references entry %q that is not a copied assistant entry", entry.Envelope.ID, ref.EntryID)
			}
			var call *toolCallRecord
			for i := range published.Assistant.ToolCalls {
				if published.Assistant.ToolCalls[i].ID == entry.ToolResult.ToolCallID {
					call = &published.Assistant.ToolCalls[i]
					break
				}
			}
			if call == nil {
				return v.corrupt("copied tool result %s answers call %q that the referenced copied assistant entry never published", entry.Envelope.ID, entry.ToolResult.ToolCallID)
			}
			if entry.Envelope.ID != call.ResultEntryID {
				return v.corrupt("copied tool result %s does not equal its reservation %q", entry.Envelope.ID, call.ResultEntryID)
			}
		case entry.Envelope.OperationID != "" && entry.Signal != nil:
			related := entry.Signal.RelatedOperation
			if related.SessionID != v.sessionID {
				return v.corrupt("signal entry %s references its related operation outside the owning Session", entry.Envelope.ID)
			}
			if _, ok := v.opsByID[related.OperationID]; !ok {
				return v.corrupt("signal entry %s references missing related operation %q", entry.Envelope.ID, related.OperationID)
			}
		case entry.Compaction != nil:
			boundary, ok := v.entryByID[entry.Compaction.BoundaryEntryID]
			if !ok {
				return v.corrupt("compaction entry %s names boundary entry %q outside the Session", entry.Envelope.ID, entry.Compaction.BoundaryEntryID)
			}
			if boundary.Envelope.Sequence >= entry.Envelope.Sequence {
				return v.corrupt("compaction entry %s names boundary entry %s at or after its own sequence", entry.Envelope.ID, boundary.Envelope.ID)
			}
			// No tool-call/result pair straddles the covered cut and no
			// unfinished call is hidden at it: the shared cut-safety rule
			// rejects any boundary whose published calls leave a missing
			// result or a reserved-result sequence beyond it.
			if offender, unsafe := v.cutSafety[entry.Compaction.BoundaryEntryID]; unsafe {
				return v.corrupt("compaction entry %s covers a cut between tool call %q and its result", entry.Envelope.ID, offender)
			}
			if entry.Envelope.OperationID == "" {
				// A copied prefix entry's evidence references stay local: the
				// fork remapped them, and a source-only reference never
				// resolves here.
				if err := v.validateEvidenceRefs("copied compaction entry "+entry.Envelope.ID+" evidence", entry.Compaction.Evidence, entry); err != nil {
					return err
				}
			}
		case entry.Envelope.OperationID == "" && entry.Assistant != nil:
			if err := v.validateEvidenceRefs("copied assistant entry "+entry.Envelope.ID+" evidence", entry.Assistant.Evidence, entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateOperation verifies one Operation's admission reference, tool-call
// exactly-once accounting, terminal/settlement agreement, and usage totals.
// The request kind selects the admitted-input rule: a message Operation
// requires its self-owned input entry, a compact Operation requires none.
func (v *graphValidation) validateOperation(op *OperationRecord) error {
	opID := op.Admission.OperationID

	if op.Admission.RequestKind == RequestKindCompact {
		if op.Admission.AdmittedEntry != (EntryRef{}) {
			return v.corrupt("compact operation %q carries an admitted input entry %q", opID, op.Admission.AdmittedEntry.EntryID)
		}
	} else {
		admitted, ok := v.entryByID[op.Admission.AdmittedEntry.EntryID]
		if op.Admission.AdmittedEntry.SessionID != v.sessionID || !ok {
			return v.corrupt("operation %q admits missing input entry %q", opID, op.Admission.AdmittedEntry.EntryID)
		}
		if admitted.Envelope.Kind != EntryInput {
			return v.corrupt("operation %q admits entry %q of kind %s, not input", opID, admitted.Envelope.ID, admitted.Envelope.Kind)
		}
		if admitted.Envelope.OperationID != opID {
			return v.corrupt("operation %q admits entry %s owned by operation %q, not itself", opID, admitted.Envelope.ID, admitted.Envelope.OperationID)
		}
	}

	published, err := v.collectPublishedCalls(opID)
	if err != nil {
		return err
	}
	resultCount, err := v.collectToolResults(opID, published)
	if err != nil {
		return err
	}
	settledHooks, hookStatus, successfulByCall, err := v.collectHookResults(opID, published)
	if err != nil {
		return err
	}

	if op.State.Status == OperationRunning {
		if err := v.validatePendingCalls(opID, published, resultCount, op.State.PendingToolCalls); err != nil {
			return err
		}
		for _, i := range v.entriesByOp[opID] {
			if entry := v.graph.Entries[i]; entry.Settlement != nil {
				return v.corrupt("running operation %q carries settlement entry %s", opID, entry.Envelope.ID)
			}
		}
		// A model or hook active effect reserves a fresh result entry
		// identity: it must stay free in the session-wide reservation index
		// (an effect reserving a tool call's reserved result ID is
		// corruption) and name no committed entry, because the effect's
		// result — or the consuming settlement — commits under it later. A
		// tool effect needs no check here: the codec forces its reservation
		// to equal the first pending call's reservation, which the assistant
		// record already indexed. A hook active effect additionally must not
		// repeat an already-settled hook execution of the same call: the
		// fresh reservation does not make a second execution valid. And no
		// active hook or tool effect may address a call whose hook chain
		// already failed: a failed or interrupted hook settles that call, so
		// the concrete effect never starts.
		if op.State.ActiveEffect != nil {
			if op.State.ActiveEffect.Kind == EffectModel || op.State.ActiveEffect.Kind == EffectHook {
				reserved := op.State.ActiveEffect.ResultEntryID
				if prior, clash := v.reservations[reserved]; clash {
					return v.corrupt("active effect of operation %q reserves result identity %q already reserved by call %q of assistant entry %s", opID, reserved, prior.callID, prior.assistantEntryID)
				}
				if committed, clash := v.entryByID[reserved]; clash {
					return v.corrupt("active effect of operation %q reserves committed entry %s of kind %s", opID, committed.Envelope.ID, committed.Envelope.Kind)
				}
				if op.State.ActiveEffect.Kind == EffectHook {
					execution := hookExecution{extensionID: op.State.ActiveEffect.ExtensionID, hookID: op.State.ActiveEffect.HookID, callID: op.State.ActiveEffect.ToolCallID, requestID: op.State.ActiveEffect.RequestID}
					if _, repeat := settledHooks[execution]; repeat {
						return v.corrupt("active hook effect of operation %q repeats hook %q's settled execution of call %q", opID, execution.hookID, execution.callID)
					}
					if execution.callID != "" && execution.requestID != "" {
						return v.corrupt("active hook effect of operation %q binds both a request and a tool call", opID)
					}
				}
			}
			if op.State.ActiveEffect.Kind == EffectHook || op.State.ActiveEffect.Kind == EffectTool {
				if op.State.ActiveEffect.ToolCallID != "" {
					if status, ok := hookStatus[op.State.ActiveEffect.ToolCallID]; ok && status != hookSucceeded {
						return v.corrupt("active %s effect of operation %q addresses call %q after its failed hook", op.State.ActiveEffect.Kind, opID, op.State.ActiveEffect.ToolCallID)
					}
				}
			}
			if op.State.ActiveEffect.Kind == EffectTool {
				// A committed concrete tool intent names the accepted selection it
				// will settle under: its contribution identities must reconcile
				// completely and in entry order with the call's successful hook
				// effects, entry-less pure identities informational.
				if err := v.validateEvidenceHookOrder(op.State.ActiveEffect.ContributionIDs, successfulByCall[op.State.ActiveEffect.ToolCallID], graphEntry{}); err != nil {
					return v.corrupt("active tool effect provenance of operation %q: %v", opID, err)
				}
			}
			if op.State.ActiveEffect.Kind == EffectHook && op.State.ActiveEffect.RequestID == "" && op.State.ActiveEffect.ToolCallID == "" {
				return v.corrupt("active hook effect of operation %q carries no binding", opID)
			}
		}
	} else {
		for _, call := range published {
			switch resultCount[call.callID] {
			case 0:
				return v.corrupt("terminal operation %q leaves published call %q unresolved", opID, call.callID)
			case 1:
			default:
				return v.corrupt("published call %q of operation %q has %d results", call.callID, opID, resultCount[call.callID])
			}
		}
		settlementCount := 0
		for _, i := range v.entriesByOp[opID] {
			if v.graph.Entries[i].Settlement != nil {
				settlementCount++
			}
		}
		if settlementCount != 1 {
			return v.corrupt("terminal operation %q owns %d settlement entries, exactly one is durable", opID, settlementCount)
		}
		settlement, ok := v.entryByID[op.State.Terminal.SettlementEntry.EntryID]
		if op.State.Terminal.SettlementEntry.SessionID != v.sessionID || !ok {
			return v.corrupt("terminal operation %q names missing settlement entry %q", opID, op.State.Terminal.SettlementEntry.EntryID)
		}
		if settlement.Envelope.Kind != EntryOperationSettlement {
			return v.corrupt("terminal operation %q names entry %q of kind %s, not operation_settlement", opID, settlement.Envelope.ID, settlement.Envelope.Kind)
		}
		if settlement.Envelope.OperationID != opID {
			return v.corrupt("terminal operation %q names settlement entry %s owned by operation %q", opID, settlement.Envelope.ID, settlement.Envelope.OperationID)
		}
		if settlement.Settlement.Status != op.State.Status {
			return v.corrupt("terminal operation %q is %s while its settlement reports %s", opID, op.State.Status, settlement.Settlement.Status)
		}
		if settlement.Settlement.Detail != op.State.Terminal.Detail {
			return v.corrupt("terminal operation %q terminal detail disagrees with its settlement entry", opID)
		}
	}
	return v.collectRequestDomain(opID)
}

// collectPublishedCalls gathers the tool calls published by one operation's
// assistant entries in publication order, enforcing per-operation call
// identity uniqueness. Reservation uniqueness is owned by the session-wide
// index built in run().
func (v *graphValidation) collectPublishedCalls(opID string) ([]publishedCall, error) {
	var published []publishedCall
	seenCalls := make(map[string]bool)
	for _, i := range v.entriesByOp[opID] {
		entry := v.graph.Entries[i]
		if entry.Assistant == nil {
			continue
		}
		for _, call := range entry.Assistant.ToolCalls {
			if seenCalls[call.ID] {
				return nil, v.corrupt("operation %q publishes duplicate tool call id %q", opID, call.ID)
			}
			seenCalls[call.ID] = true
			published = append(published, publishedCall{
				callID:           call.ID,
				assistantEntryID: entry.Envelope.ID,
				reservedResultID: call.ResultEntryID,
			})
		}
	}
	return published, nil
}

// collectToolResults counts the terminal results answering each published
// call of one operation, enforcing that every result answers a published call
// from its publishing assistant entry under its reserved identity.
func (v *graphValidation) collectToolResults(opID string, published []publishedCall) (map[string]int, error) {
	byCall := make(map[string]*publishedCall, len(published))
	for i := range published {
		byCall[published[i].callID] = &published[i]
	}
	counts := make(map[string]int, len(published))
	for _, i := range v.entriesByOp[opID] {
		entry := v.graph.Entries[i]
		if entry.ToolResult == nil {
			continue
		}
		call, ok := byCall[entry.ToolResult.ToolCallID]
		if !ok {
			return nil, v.corrupt("tool result %s answers unpublished call %q", entry.Envelope.ID, entry.ToolResult.ToolCallID)
		}
		if entry.ToolResult.AssistantEntry.SessionID != v.sessionID ||
			entry.ToolResult.AssistantEntry.EntryID != call.assistantEntryID {
			return nil, v.corrupt("tool result %s answers call %q from assistant entry %q, not its publisher %q",
				entry.Envelope.ID, entry.ToolResult.ToolCallID, entry.ToolResult.AssistantEntry.EntryID, call.assistantEntryID)
		}
		if entry.Envelope.ID != call.reservedResultID {
			return nil, v.corrupt("tool result %s does not equal its reservation %q", entry.Envelope.ID, call.reservedResultID)
		}
		counts[entry.ToolResult.ToolCallID]++
	}
	return counts, nil
}

// collectHookResults verifies every committed hook result of one operation:
// it must execute a call published by that operation's assistant entries,
// follow its publishing assistant entry, precede the call's terminal tool
// result when one is present, and not repeat a settled hook identity for the
// same call. Once a hook fails or is interrupted, no later hook result and no
// active hook or tool effect may address that call, and the call's terminal
// tool result — when one is present — must match the settled hook consequence:
// an interrupted hook permits only the interrupted result, while a failed hook
// permits the error or interrupted result (cancellation or recovery settles
// the interrupted one); a running quiet gap before tool settlement stays valid
// and recoverable. The accepted tool selection — a settled tool result's
// normalized arguments with its provenance, or a concrete tool effect's
// committed contribution identities — reconciles with the call's committed
// successful tool-bound hook results in entry order through the same
// shared order rule as request evidence; a settlement without an accepted
// selection (a quiet-gap recovery) carries nothing to reconcile and stays
// valid. It returns the settled hook-execution set, each executed call's last
// hook status, and each call's successful hook results for the active-effect
// checks.
func (v *graphValidation) collectHookResults(opID string, published []publishedCall) (map[hookExecution]bool, map[string]hookResultStatus, map[string][]graphEntry, error) {
	byCall := make(map[string]*publishedCall, len(published))
	for i := range published {
		byCall[published[i].callID] = &published[i]
	}
	settled := make(map[hookExecution]bool, len(published))
	lastStatus := make(map[string]hookResultStatus, len(published))
	successfulByCall := make(map[string][]graphEntry, len(published))
	for _, i := range v.entriesByOp[opID] {
		entry := v.graph.Entries[i]
		if entry.HookResult == nil {
			continue
		}
		if entry.HookResult.RequestID != "" { // the request-bound family binds a request, never a tool call, and is checked in the request-domain pass
			if _, repeat := settled[hookExecution{extensionID: entry.HookResult.ExtensionID, hookID: entry.HookResult.HookID, requestID: entry.HookResult.RequestID}]; repeat {
				return nil, nil, nil, v.corrupt("request hook %q of extension %q executed request %q of operation %q twice", entry.HookResult.HookID, entry.HookResult.ExtensionID, entry.HookResult.RequestID, opID)
			}
			settled[hookExecution{extensionID: entry.HookResult.ExtensionID, hookID: entry.HookResult.HookID, requestID: entry.HookResult.RequestID}] = true
			continue
		}
		call, ok := byCall[entry.HookResult.ToolCallID]
		if !ok {
			return nil, nil, nil, v.corrupt("hook result %s executes unpublished call %q", entry.Envelope.ID, entry.HookResult.ToolCallID)
		}
		publisher, ok := v.entryByID[call.assistantEntryID]
		if !ok || entry.Envelope.Sequence <= publisher.Envelope.Sequence {
			return nil, nil, nil, v.corrupt("hook result %s for call %q does not follow its publishing assistant entry %s", entry.Envelope.ID, call.callID, call.assistantEntryID)
		}
		if committed, ok := v.entryByID[call.reservedResultID]; ok && entry.Envelope.Sequence >= committed.Envelope.Sequence {
			return nil, nil, nil, v.corrupt("hook result %s for call %q follows that call's terminal tool result %s", entry.Envelope.ID, call.callID, committed.Envelope.ID)
		}
		// Historical first-pending ordering: a hook runs only while its call
		// is the first pending call, so every earlier published call's
		// terminal result must already be committed before this hook's
		// sequence.
		for _, earlier := range published {
			if earlier.callID == call.callID {
				break
			}
			committed, ok := v.entryByID[earlier.reservedResultID]
			if !ok || committed.Envelope.Sequence >= entry.Envelope.Sequence {
				return nil, nil, nil, v.corrupt("hook result %s for call %q runs before the terminal result of earlier call %q", entry.Envelope.ID, call.callID, earlier.callID)
			}
		}
		if failed := lastStatus[call.callID]; failed != "" && failed != hookSucceeded {
			return nil, nil, nil, v.corrupt("hook result %s executes call %q after that call's %s hook result", entry.Envelope.ID, call.callID, failed)
		}
		execution := hookExecution{extensionID: entry.HookResult.ExtensionID, hookID: entry.HookResult.HookID, callID: call.callID}
		if settled[execution] {
			return nil, nil, nil, v.corrupt("hook %q executed call %q of operation %q twice", execution.hookID, execution.callID, opID)
		}
		settled[execution] = true
		lastStatus[call.callID] = entry.HookResult.Status
		if entry.HookResult.Status == hookSucceeded {
			successfulByCall[call.callID] = append(successfulByCall[call.callID], entry)
		}
	}
	// The accepted tool selection reconciles with the call's successful hook
	// effects through the shared complete-order rule: a settled tool result
	// carrying accepted normalized arguments names them completely and in
	// entry order, entry-less pure identities stay informational, and the
	// failed or interrupted hook's own identity is not an accepted
	// contribution. A settlement without an accepted selection invents none.
	for _, i := range v.entriesByOp[opID] {
		entry := v.graph.Entries[i]
		if entry.ToolResult == nil || len(entry.ToolResult.NormalizedArguments) == 0 {
			continue
		}
		if err := v.validateEvidenceHookOrder(entry.ToolResult.HookIDs, successfulByCall[entry.ToolResult.ToolCallID], entry); err != nil {
			return nil, nil, nil, v.corrupt("tool result %s for call %q: %v", entry.Envelope.ID, entry.ToolResult.ToolCallID, err)
		}
	}
	// The tool result of a call whose hook chain failed must match the settled
	// hook consequence; its absence is the valid running quiet gap recovery
	// interrupts.
	for callID, status := range lastStatus {
		if status == hookSucceeded {
			continue
		}
		committed, ok := v.entryByID[byCall[callID].reservedResultID]
		if !ok || committed.ToolResult == nil {
			continue
		}
		switch status {
		case hookInterrupted:
			if committed.ToolResult.Status != model.ResultInterrupted {
				return nil, nil, nil, v.corrupt("call %q of operation %q settled %s after its interrupted hook result", callID, opID, committed.ToolResult.Status)
			}
		case hookFailed:
			if committed.ToolResult.Status != model.ResultError && committed.ToolResult.Status != model.ResultInterrupted {
				return nil, nil, nil, v.corrupt("call %q of operation %q settled %s after its failed hook result", callID, opID, committed.ToolResult.Status)
			}
		}
	}
	return settled, lastStatus, successfulByCall, nil
}

// validatePendingCalls verifies that a running operation's pending list is
// exactly its unresolved published calls in publication order with matching
// reservations: every published call is represented exactly once as
// unresolved or by one terminal result.
func (v *graphValidation) validatePendingCalls(opID string, published []publishedCall, resultCount map[string]int, pending []PendingToolCall) error {
	var unresolved []publishedCall
	for _, call := range published {
		if resultCount[call.callID] == 0 {
			unresolved = append(unresolved, call)
		}
	}
	if len(unresolved) != len(pending) {
		return v.corrupt("operation %q has %d pending calls for %d unresolved published calls", opID, len(pending), len(unresolved))
	}
	for i, call := range unresolved {
		got := pending[i]
		if got.CallID != call.callID ||
			got.AssistantEntry.EntryID != call.assistantEntryID ||
			got.ResultEntryID != call.reservedResultID ||
			got.AssistantEntry.SessionID != v.sessionID {
			return v.corrupt("pending_tool_calls[%d] of operation %q does not match unresolved call %q in publication order", i, opID, call.callID)
		}
	}
	return nil
}

// validateUsage recomputes Operation and Session totals from the
// usage-bearing producer entries — assistant usages, terminal
// no-output-settlement usage pairs, and committed maintenance fact usages —
// and
// requires exact agreement. Counts accumulate per model identity into maps
// and each total materializes exactly once: accumulation is linear in entries,
// followed by sorting each total's distinct model identities.
func (v *graphValidation) validateUsage() error {
	sessionCounts := make(map[model.ModelRef]UsageCount)
	perOperation := make(map[string]map[model.ModelRef]UsageCount, len(v.graph.Operations))
	for _, entry := range v.graph.Entries {
		if entry.Assistant != nil && entry.Assistant.Usage != nil {
			if err := v.recordUsage(sessionCounts, perOperation, entry.Envelope.OperationID, entry.Assistant.Source, *entry.Assistant.Usage); err != nil {
				return err
			}
		}
		// The ordinary no-output settlement usage and the committed
		// maintenance fact usages are additive when both occur on one
		// settlement.
		if entry.Settlement != nil && entry.Settlement.Usage != nil {
			if err := v.recordUsage(sessionCounts, perOperation, entry.Envelope.OperationID, *entry.Settlement.Model, *entry.Settlement.Usage); err != nil {
				return err
			}
		}
		// Committed maintenance facts carry their optional per-producer usage.
		var facts []maintenanceFact
		switch {
		case entry.Compaction != nil:
			facts = entry.Compaction.Facts
		case entry.Settlement != nil:
			facts = entry.Settlement.Facts
		}
		for _, fact := range facts {
			if fact.Usage == nil {
				continue
			}
			if err := v.recordUsage(sessionCounts, perOperation, entry.Envelope.OperationID, fact.Model, *fact.Usage); err != nil {
				return err
			}
		}
	}
	for i := range v.graph.Operations {
		op := &v.graph.Operations[i]
		counts := perOperation[op.Admission.OperationID]
		materialized := UsageTotals{ByModel: make([]ModelUsage, 0, len(counts))}
		for ref, usage := range counts {
			materialized.ByModel = append(materialized.ByModel, ModelUsage{Model: ref, Usage: usage})
		}
		sortUsageTotals(materialized.ByModel)
		if !usageTotalsEqual(materialized, op.State.Usage) {
			return v.corrupt("usage totals of operation %q disagree with its usage-bearing entries", op.Admission.OperationID)
		}
	}
	sessionUsage := UsageTotals{ByModel: make([]ModelUsage, 0, len(sessionCounts))}
	for ref, usage := range sessionCounts {
		sessionUsage.ByModel = append(sessionUsage.ByModel, ModelUsage{Model: ref, Usage: usage})
	}
	sortUsageTotals(sessionUsage.ByModel)
	if !usageTotalsEqual(sessionUsage, v.graph.Session.State.Usage) {
		return v.corrupt("usage totals of the Session disagree with its usage-bearing entries")
	}
	return nil
}

// recordUsage accumulates one usage-bearing producer's counts into the
// session and owning-operation totals.
func (v *graphValidation) recordUsage(sessionCounts map[model.ModelRef]UsageCount, perOperation map[string]map[model.ModelRef]UsageCount, operationID string, ref model.ModelRef, counts UsageCount) error {
	if operationID != "" {
		opCounts := perOperation[operationID]
		if opCounts == nil {
			opCounts = make(map[model.ModelRef]UsageCount)
			perOperation[operationID] = opCounts
		}
		sum, err := addUsageCount(opCounts[ref], counts)
		if err != nil {
			return v.corrupt("usage of operation %q overflows: %v", operationID, err)
		}
		opCounts[ref] = sum
	}
	sum, err := addUsageCount(sessionCounts[ref], counts)
	if err != nil {
		return v.corrupt("session usage overflows: %v", err)
	}
	sessionCounts[ref] = sum
	return nil
}

// requestAccounting is one operation's committed request domain: per request
// identity, the ordered committed request-hook results and their statuses,
// and the assistant that answered the request.
type requestAccounting struct {
	hookResults []graphEntry
	hookFailed  bool
	assistant   *graphEntry
}

func (r *requestAccounting) orderedResults() []graphEntry {
	if r == nil {
		return nil
	}
	return r.hookResults
}

// collectRequestDomain verifies the closed request-evidence rules of one
// operation: every conversation assistant carries its producing request
// identity and final effective request evidence with a request identity
// unique per Operation; every evidence and consumed-input value references
// the canonical projection actually consumed at its boundary — the checkpoint
// names an in-session compaction entry and the boundary names a projectable
// conversation entry (or that compaction entry for a summary-only
// projection), each strictly before the record that carries it; committed
// effectful hook identities occur completely and in committed sequence order
// at each consumed and final boundary while entry-less pure identities stay
// informational; a request-bound hook result precedes the assistant answering
// the same request and stops that request's dispatch once it fails or is
// interrupted; an active request-bound effect may not address an answered or
// hook-failed request; the maintenance facts settle once on the checkpoint or
// the settlement, never both, and every automatic maintenance record's
// consumed evidence reconciles with the committed hook executions of the
// consumed conversation request rather than the maintenance producer; and
// the running request state names committed
// result references of its own request in exact entry order. Recovery
// validates this same domain before any recovery write copies evidence.
func (v *graphValidation) collectRequestDomain(opID string) error {
	requests := make(map[string]*requestAccounting)
	automatic := false
	if op, ok := v.opsByID[opID]; ok {
		automatic = op.Admission.RequestKind == RequestKindMessage
	}
	for _, i := range v.entriesByOp[opID] {
		entry := v.graph.Entries[i]
		if entry.HookResult == nil || entry.HookResult.RequestID == "" {
			continue
		}
		requestID := entry.HookResult.RequestID
		if requests[requestID] == nil {
			requests[requestID] = &requestAccounting{}
		}
		requests[requestID].hookResults = append(requests[requestID].hookResults, entry)
	}
	for _, i := range v.entriesByOp[opID] {
		entry := v.graph.Entries[i]
		switch {
		case entry.HookResult != nil && entry.HookResult.RequestID != "":
			result := entry.HookResult
			req := requests[result.RequestID]
			if req == nil {
				req = &requestAccounting{}
				requests[result.RequestID] = req
			}
			if req.assistant != nil {
				return v.corrupt("hook result %s for request %q follows the assistant entry %s answering that request", entry.Envelope.ID, result.RequestID, req.assistant.Envelope.ID)
			}
			if req.hookFailed {
				return v.corrupt("hook result %s executes request %q after its failed request hook result", entry.Envelope.ID, result.RequestID)
			}
			if result.Status != hookSucceeded {
				req.hookFailed = true
			}
			if err := v.validateEvidenceRefs("hook result "+entry.Envelope.ID+" consumed view", result.InputEvidence, entry); err != nil {
				return err
			}
			if err := v.validateEvidenceHookOrder(result.InputEvidence.HookIDs, req.hookResults, entry); err != nil {
				return v.corrupt("hook result %s for request %q: %v", entry.Envelope.ID, result.RequestID, err)
			}
		case entry.Assistant != nil:
			assistant := entry.Assistant
			if assistant.RequestID == "" {
				return v.corrupt("assistant entry %s carries no producing request identity", entry.Envelope.ID)
			}
			req := requests[assistant.RequestID]
			if req == nil {
				req = &requestAccounting{}
				requests[assistant.RequestID] = req
			}
			if req.hookFailed {
				return v.corrupt("assistant entry %s answers request %q after its failed request hook result", entry.Envelope.ID, assistant.RequestID)
			}
			if req.assistant != nil {
				return v.corrupt("operation %q answers request %q with a second assistant entry %s", opID, assistant.RequestID, entry.Envelope.ID)
			}
			req.assistant = &entry
			if err := v.validateEvidenceRefs("assistant entry "+entry.Envelope.ID+" evidence", assistant.Evidence, entry); err != nil {
				return err
			}
			if err := v.validateEvidenceHookOrder(assistant.Evidence.HookIDs, req.hookResults, entry); err != nil {
				return v.corrupt("assistant entry %s: %v", entry.Envelope.ID, err)
			}
		case entry.Settlement != nil:
			settlement := entry.Settlement
			if settlement.Evidence != nil {
				if err := v.validateEvidenceRefs("settlement entry "+entry.Envelope.ID+" evidence", settlement.Evidence, entry); err != nil {
					return err
				}
				orderKey := settlement.RequestID
				if settlement.CompactionUsage {
					// Any settlement of the maintenance procedure's own
					// evidence — including an early opener failure that
					// retained the consumed contribution evidence without
					// any actual input — is reconciled with the committed
					// hook executions of the conversation request whose
					// view was consumed, never with the producer identity.
					if automatic && settlement.ConsumedRequestID == "" {
						return v.corrupt("maintenance settlement %s of operation %q names no consumed conversation request", entry.Envelope.ID, opID)
					}
					if settlement.ConsumedRequestID != "" {
						orderKey = settlement.ConsumedRequestID
					}
				}
				if err := v.validateEvidenceHookOrder(settlement.Evidence.HookIDs, requests[orderKey].orderedResults(), entry); err != nil {
					return v.corrupt("settlement entry %s: %v", entry.Envelope.ID, err)
				}
			}
		case entry.Compaction != nil:
			if err := v.validateEvidenceRefs("compaction entry "+entry.Envelope.ID+" evidence", entry.Compaction.Evidence, entry); err != nil {
				return err
			}
			orderKey := ""
			if len(entry.Compaction.Facts) > 0 {
				orderKey = entry.Compaction.Facts[0].RequestID
			}
			if entry.Compaction.ConsumedRequestID != "" {
				orderKey = entry.Compaction.ConsumedRequestID
			} else if automatic {
				return v.corrupt("automatic maintenance record %s of operation %q names no consumed conversation request", entry.Envelope.ID, opID)
			}
			if err := v.validateEvidenceHookOrder(entry.Compaction.Evidence.HookIDs, requests[orderKey].orderedResults(), entry); err != nil {
				return v.corrupt("compaction entry %s: %v", entry.Envelope.ID, err)
			}
		}
	}
	maintenanceProducers := make(map[string]string)
	for _, i := range v.entriesByOp[opID] {
		entry := v.graph.Entries[i]
		var facts []maintenanceFact
		switch {
		case entry.Compaction != nil:
			facts = entry.Compaction.Facts
		case entry.Settlement != nil:
			facts = entry.Settlement.Facts
		}
		for _, fact := range facts {
			producerID := fact.RequestID
			if producerID == "" {
				continue
			}
			if previous, exists := maintenanceProducers[producerID]; exists {
				return v.corrupt("operation %q settles maintenance producer %q on both entries %s and %s", opID, producerID, previous, entry.Envelope.ID)
			}
			maintenanceProducers[producerID] = entry.Envelope.ID
		}
	}
	op, ok := v.opsByID[opID]
	if !ok {
		return nil
	}
	// Request-bound active effects stay inside the same domain: a live request
	// identity may not already be answered by a committed assistant of this
	// Operation, and no model or request-hook effect may dispatch for a
	// request whose hook chain already failed. An undispatched interrupted
	// request needs no assistant response: its settlement and retained hook
	// results witness it.
	if effect := op.State.ActiveEffect; effect != nil && effect.RequestID != "" {
		req := requests[effect.RequestID]
		if req != nil && req.assistant != nil {
			return v.corrupt("active %s effect of operation %q answers request %q already answered by assistant entry %s", effect.Kind, opID, effect.RequestID, req.assistant.Envelope.ID)
		}
		if req != nil && req.hookFailed {
			return v.corrupt("active %s effect of operation %q dispatches request %q after its failed request hook result", effect.Kind, opID, effect.RequestID)
		}
		if effect.Evidence != nil {
			if effect.Kind == EffectHook {
				for _, identity := range effect.Evidence.HookIDs {
					if identity.ExtensionID == effect.ExtensionID && identity.HookID == effect.HookID {
						return v.corrupt("active request hook %q names itself in its consumed provenance", effect.HookID)
					}
				}
			}
			if err := v.validateActiveEvidenceRefs(effect.Evidence); err != nil {
				return err
			}
			if err := v.validateEvidenceHookOrder(effect.Evidence.HookIDs, req.orderedResults(), graphEntry{}); err != nil {
				return v.corrupt("active effect evidence of operation %q: %v", opID, err)
			}
		}
	}
	return v.validateRunningRequestState(op, requests)
}

// validateActiveEvidenceRefs resolves an active effect's evidence against the
// committed graph before any recovery copies it: the same reference rules as
// committed consumers, with the active record's own sequence unknown, so
// references must resolve to committed entries only.
func (v *graphValidation) validateActiveEvidenceRefs(evidence *RequestEvidence) error {
	return v.validateEvidenceRefs("active effect evidence", evidence, graphEntry{})
}

// validateEvidenceRefs enforces one evidence value's canonical references
// against the graph: the checkpoint names an in-session compaction entry and
// the boundary names a projectable conversation entry — never a hook result
// or settlement — each strictly before the carrying record. The boundary
// reference is required on every recorded evidence value. A zero-sequence
// owner (an in-flight active effect) is checked against committed entries
// only.
func (v *graphValidation) validateEvidenceRefs(what string, evidence *RequestEvidence, owner graphEntry) error {
	if evidence == nil {
		return v.corrupt("%s carries no evidence", what)
	}
	if evidence.BoundaryEntryID == "" {
		return v.corrupt("%s carries no canonical boundary reference", what)
	}
	boundary, ok := v.entryByID[evidence.BoundaryEntryID]
	if !ok {
		return v.corrupt("%s references boundary entry %q outside the Session", what, evidence.BoundaryEntryID)
	}
	if !conversationBearingEntryKind(boundary) {
		return v.corrupt("%s references boundary entry %s of non-canonical kind %s", what, boundary.Envelope.ID, boundary.Envelope.Kind)
	}
	if owner.Envelope.Sequence != 0 && boundary.Envelope.Sequence >= owner.Envelope.Sequence {
		return v.corrupt("%s references boundary entry %s at or after its own sequence", what, boundary.Envelope.ID)
	}
	if evidence.CheckpointEntryID != "" {
		checkpoint, ok := v.entryByID[evidence.CheckpointEntryID]
		if !ok || checkpoint.Compaction == nil {
			return v.corrupt("%s checkpoint reference %q names no in-session compaction entry", what, evidence.CheckpointEntryID)
		}
		// A consumed pair names the projection actually frozen for it: the
		// boundary is the checkpoint itself (an empty kept tail) or a
		// canonical tail entry it did not cover, never an entry the
		// checkpoint already covered.
		if boundary.Envelope.ID == checkpoint.Envelope.ID {
			if owner.Envelope.Sequence != 0 && checkpoint.Envelope.Sequence >= owner.Envelope.Sequence {
				return v.corrupt("%s references checkpoint entry %s at or after its own sequence", what, checkpoint.Envelope.ID)
			}
			return nil
		}
		target, ok := v.entryByID[checkpoint.Compaction.BoundaryEntryID]
		if !ok {
			return v.corrupt("%s names checkpoint entry %s with boundary entry %q outside the Session", what, checkpoint.Envelope.ID, checkpoint.Compaction.BoundaryEntryID)
		}
		if boundary.Envelope.Sequence <= target.Envelope.Sequence {
			return v.corrupt("%s references boundary entry %s already covered by checkpoint entry %s", what, boundary.Envelope.ID, checkpoint.Envelope.ID)
		}
		if owner.Envelope.Sequence != 0 && checkpoint.Envelope.Sequence >= owner.Envelope.Sequence {
			return v.corrupt("%s references checkpoint entry %s at or after its own sequence", what, checkpoint.Envelope.ID)
		}
	}
	return nil
}

// conversationBearingEntryKind reports whether one committed entry projects
// into the canonical conversation view a request consumes.
func conversationBearingEntryKind(entry graphEntry) bool {
	return entry.Input != nil || entry.Assistant != nil || entry.ToolResult != nil || entry.Signal != nil || entry.Compaction != nil
}

// messageBearingEntryKind reports whether one committed entry contributes an
// ordinary message to the projection; a checkpoint contributes its owned
// messages through the named register entry instead.
func messageBearingEntryKind(entry graphEntry) bool {
	return entry.Input != nil || entry.Assistant != nil || entry.ToolResult != nil || entry.Signal != nil
}

// validateEvidenceHookOrder reconciles one evidence value's recorded hook
// identities against the committed request-hook results in entry order: every
// result-backed identity must occur completely and in committed order, and no
// identity may name a result at or after the consuming record. Entry-less
// pure identities stay informational: they may interleave or net-disappear.
func (v *graphValidation) validateEvidenceHookOrder(identities []HookIdentity, committedResults []graphEntry, owner graphEntry) error {
	limit := len(committedResults)
	if owner.Envelope.Sequence != 0 {
		for i, entry := range committedResults {
			if entry.Envelope.Sequence >= owner.Envelope.Sequence {
				limit = i
				break
			}
		}
	}
	next := 0
	for _, identity := range identities {
		for i := range committedResults {
			result := committedResults[i].HookResult
			if result.ExtensionID == identity.ExtensionID && result.HookID == identity.HookID {
				if i >= limit {
					return fmt.Errorf("names committed hook result %s at or after its own sequence", committedResults[i].Envelope.ID)
				}
				if i != next {
					return fmt.Errorf("names committed hook result %s out of order", committedResults[i].Envelope.ID)
				}
				next++
				break
			}
		}
	}
	if next != limit {
		return fmt.Errorf("omits committed hook result %s from consumed provenance", committedResults[next].Envelope.ID)
	}
	return nil
}

// validateRunningRequestState enforces the one suspended conversation request
// state: its checkpoint reference coheres with the Session's current
// compaction entry, its boundary reference resolves to a projectable
// conversation entry of this Session, and its hook-result references equal
// the request's committed request-hook result entries in exact commit order.
func (v *graphValidation) validateRunningRequestState(op *OperationRecord, requests map[string]*requestAccounting) error {
	state := op.State.Request
	if state == nil {
		return nil
	}
	opID := op.Admission.OperationID
	if v.graph.Session.State.CheckpointEntryID != state.CheckpointEntryID {
		return v.corrupt("running request state of operation %q carries a checkpoint reference foreign to the Session's current compaction entry", opID)
	}
	if err := v.validateEvidenceRefs(fmt.Sprintf("running request state of operation %q", opID), &RequestEvidence{
		CheckpointEntryID: state.CheckpointEntryID,
		BoundaryEntryID:   state.BoundaryEntryID,
	}, graphEntry{}); err != nil {
		return err
	}
	req := requests[state.RequestID]
	committed := req.orderedResults()
	if len(committed) != len(state.HookResults) {
		return v.corrupt("running request state of operation %q records %d hook results for %d committed", opID, len(state.HookResults), len(committed))
	}
	for i, ref := range state.HookResults {
		if ref.SessionID != v.sessionID {
			return v.corrupt("running request state of operation %q references hook result %s through a foreign session id", opID, ref.EntryID)
		}
		if committed[i].Envelope.ID != ref.EntryID {
			return v.corrupt("running request state of operation %q references hook results out of committed entry order", opID)
		}
	}
	return nil
}
