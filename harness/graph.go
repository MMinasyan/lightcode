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
	Settlement *operationSettlementEntry
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

// decodeGraphEntry decodes one entry envelope according to its kind. The
// landed hook_result and compaction kinds have no valid Phase 3 payload, so a
// stored record of either kind is corruption; unknown kinds are too.
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
	case EntryOperationSettlement:
		var v operationSettlementEntry
		if v, err = decodeOperationSettlementEntry(env); err == nil {
			entry.Settlement = &v
		}
	case EntryHookResult, EntryCompaction:
		return graphEntry{}, corruptSession(sessionID, "entry %s: kind %q has no durable payload in this phase", env.ID, env.Kind)
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
	reservations map[string]publishedReservation
}

func (g *sessionGraph) operationIndex() map[string]*OperationRecord {
	index := make(map[string]*OperationRecord, len(g.Operations))
	for i := range g.Operations {
		index[g.Operations[i].Admission.OperationID] = &g.Operations[i]
	}
	return index
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
	for _, entry := range v.graph.Entries {
		if _, dup := v.entryByID[entry.Envelope.ID]; dup {
			return v.corrupt("duplicate entry id %q", entry.Envelope.ID)
		}
		v.entryByID[entry.Envelope.ID] = entry
	}
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
// identity, and no running Operation in an archived Session.
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
	return nil
}

// validateEntryOwnership verifies every owned entry names an existing
// Operation register of this Session.
func (v *graphValidation) validateEntryOwnership() error {
	for _, entry := range v.graph.Entries {
		if entry.Envelope.OperationID == "" {
			continue
		}
		if _, ok := v.opsByID[entry.Envelope.OperationID]; !ok {
			return v.corrupt("entry %s is owned by unknown Operation %q", entry.Envelope.ID, entry.Envelope.OperationID)
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

// validateForkPrefixOwnership verifies the copied-prefix shape: a root
// Session carries no operationless entries, and in a fork every operationless
// entry precedes every operation-owned entry, so the copied prefix stays a
// strict sequence prefix.
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
// the copied prefix, and an owned signal's related Operation resolves inside
// the owning Session while a copied signal's informational history may not.
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
		}
	}
	return nil
}

// validateOperation verifies one Operation's admission reference, tool-call
// exactly-once accounting, terminal/settlement agreement, and usage totals.
func (v *graphValidation) validateOperation(op *OperationRecord) error {
	opID := op.Admission.OperationID

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

	published, err := v.collectPublishedCalls(opID)
	if err != nil {
		return err
	}
	resultCount, err := v.collectToolResults(opID, published)
	if err != nil {
		return err
	}

	if op.State.Status == OperationRunning {
		if err := v.validatePendingCalls(opID, published, resultCount, op.State.PendingToolCalls); err != nil {
			return err
		}
		for _, entry := range v.graph.Entries {
			if entry.Envelope.OperationID == opID && entry.Settlement != nil {
				return v.corrupt("running operation %q carries settlement entry %s", opID, entry.Envelope.ID)
			}
		}
		// A model active effect reserves its result entry identity: it must
		// stay free in the session-wide reservation index (a model effect
		// reserving a tool call's reserved result ID is corruption) and name
		// no committed entry, because the model result — or the consuming
		// settlement — commits under it later. A tool effect needs no check
		// here: the codec forces its reservation to equal the first pending
		// call's reservation, which the assistant record already indexed.
		if op.State.ActiveEffect != nil && op.State.ActiveEffect.Kind == EffectModel {
			reserved := op.State.ActiveEffect.ResultEntryID
			if prior, clash := v.reservations[reserved]; clash {
				return v.corrupt("model active effect of operation %q reserves result identity %q already reserved by call %q of assistant entry %s", opID, reserved, prior.callID, prior.assistantEntryID)
			}
			if committed, clash := v.entryByID[reserved]; clash {
				return v.corrupt("model active effect of operation %q reserves committed entry %s of kind %s", opID, committed.Envelope.ID, committed.Envelope.Kind)
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
		for _, entry := range v.graph.Entries {
			if entry.Envelope.OperationID == opID && entry.Settlement != nil {
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
	return nil
}

// collectPublishedCalls gathers the tool calls published by one operation's
// assistant entries in publication order, enforcing per-operation call
// identity uniqueness. Reservation uniqueness is owned by the session-wide
// index built in run().
func (v *graphValidation) collectPublishedCalls(opID string) ([]publishedCall, error) {
	var published []publishedCall
	seenCalls := make(map[string]bool)
	for _, entry := range v.graph.Entries {
		if entry.Envelope.OperationID != opID || entry.Assistant == nil {
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
	for _, entry := range v.graph.Entries {
		if entry.Envelope.OperationID != opID || entry.ToolResult == nil {
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
// usage-bearing producer entries — assistant usages plus terminal
// no-output-settlement usage pairs — and requires exact agreement.
func (v *graphValidation) validateUsage() error {
	sessionUsage := UsageTotals{}
	perOperation := make(map[string]UsageTotals, len(v.graph.Operations))
	for _, entry := range v.graph.Entries {
		var ref model.ModelRef
		var counts UsageCount
		bears := false
		switch {
		case entry.Assistant != nil && entry.Assistant.Usage != nil:
			ref, counts, bears = entry.Assistant.Source, *entry.Assistant.Usage, true
		case entry.Settlement != nil && entry.Settlement.Usage != nil:
			ref, counts, bears = *entry.Settlement.Model, *entry.Settlement.Usage, true
		}
		if !bears {
			continue
		}
		contribution := UsageTotals{ByModel: []ModelUsage{{Model: ref, Usage: counts}}}
		if entry.Envelope.OperationID != "" {
			updated, err := addUsageTotals(perOperation[entry.Envelope.OperationID], contribution)
			if err != nil {
				return v.corrupt("usage of operation %q overflows: %v", entry.Envelope.OperationID, err)
			}
			perOperation[entry.Envelope.OperationID] = updated
		}
		updated, err := addUsageTotals(sessionUsage, contribution)
		if err != nil {
			return v.corrupt("session usage overflows: %v", err)
		}
		sessionUsage = updated
	}
	for i := range v.graph.Operations {
		op := &v.graph.Operations[i]
		if !usageTotalsEqual(perOperation[op.Admission.OperationID], op.State.Usage) {
			return v.corrupt("usage totals of operation %q disagree with its usage-bearing entries", op.Admission.OperationID)
		}
	}
	if !usageTotalsEqual(sessionUsage, v.graph.Session.State.Usage) {
		return v.corrupt("usage totals of the Session disagree with its usage-bearing entries")
	}
	return nil
}
