package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MMinasyan/lightcode/model"
)

// ForkRequest is the input of one transactional fork: the idle open or
// archived source Session, the user-origin input entry whose conversation
// prefix the destination inherits, the caller Operation identity of the
// fresh destination admission, and its content.
type ForkRequest struct {
	SourceSessionID string
	BoundaryEntryID string
	OperationID     string
	Content         []model.ContentPart
}

// ForkResult is the outcome of one Fork: the committed destination Session
// and its admitted running Operation.
type ForkResult struct {
	Session   SessionRecord
	Operation OperationRecord
}

// Fork forks one idle open or archived source Session at the selected
// user-origin input boundary: it first resolves the caller Operation ID's
// idempotency lookup, then reserves the source coordinator, prepares the
// destination view outside locks and storage, and performs one store-wide
// transaction that revalidates the source, creates the destination Session
// with source/boundary lineage, copies only the strict-before-boundary
// eligible entries under new identities, and runs the shared in-transaction
// destination admission producer with a fixed user origin. Destination
// execution starts only after the commit. Source records and the source's
// process-local buffers are never changed, and lineage is informational
// only: source and fork archive or delete independently.
func (h *Harness) Fork(ctx context.Context, req ForkRequest) (ForkResult, error) {
	if err := h.ctx.Err(); err != nil { // cancellation has closed admission
		return ForkResult{}, err
	}
	content, err := validateSubmitInput(req.OperationID, InputOriginUser, req.Content)
	if err != nil {
		return ForkResult{}, err
	}
	if err := validateHexID(req.SourceSessionID, "source session id"); err != nil {
		return ForkResult{}, invalidInput("source session id: %v", err)
	}
	if err := validateHexID(req.BoundaryEntryID, "boundary entry id"); err != nil {
		return ForkResult{}, invalidInput("boundary entry id: %v", err)
	}

	// the caller Operation ID's idempotency lookup precedes source
	// validation and preparation
	res, found, err := h.forkExisting(ctx, req)
	if err != nil || found {
		return res, err
	}

	c, err := h.coordinatorFor(ctx, req.SourceSessionID)
	if err != nil {
		return ForkResult{}, err
	}
	release, err := c.reserve(ctx)
	if err != nil {
		return ForkResult{}, err
	}
	defer release()
	c.mu.Lock()
	if c.gone { // a deletion committed while this call waited or relocked
		c.mu.Unlock()
		return ForkResult{}, notFoundSession(req.SourceSessionID)
	}
	if c.graph.Session.State.CurrentOperationID != "" { // forking requires an idle source
		running := c.graph.Session.State.CurrentOperationID
		c.mu.Unlock()
		return ForkResult{}, invalidInput("session %q already runs operation %q; forking requires an idle source Session", req.SourceSessionID, running)
	}
	view := c.graph.Session // the state the reservation resolved on
	c.mu.Unlock()

	destID, err := newHexID()
	if err != nil {
		return ForkResult{}, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	prepared, capture, prepCtx, cleanup, err := h.prepareExecution(ctx, PreparationSession{
		Identity:  SessionIdentity{SessionID: destID, Workspace: view.Identity.Workspace},
		AgentType: view.State.CurrentAgentType,
	})
	if err != nil {
		return ForkResult{}, err
	}
	defer cleanup() // the combined context stays live until publication returns

	var commit forkCommit
	txErr := h.deps.Storage.Transact(prepCtx, func(tx Transaction) error {
		return h.forkTransaction(tx, destID, view, req, content, capture, &commit)
	})
	if txErr != nil {
		h.markCorrupt(req.SourceSessionID, txErr)
		if errors.Is(txErr, ErrConflict) {
			// first-writer resolution: the same exact lookup runs once; the
			// preparation and the transaction never rerun
			return h.resolveForkConflict(ctx, req, txErr)
		}
		return ForkResult{}, publicationError(ctx, txErr)
	}

	dest := &coordinator{graph: &sessionGraph{Session: commit.session, Operations: []OperationRecord{commit.operation}, Entries: commit.entries}}
	h.mu.Lock()
	h.sessions[destID] = dest
	h.mu.Unlock()
	h.startExecution(dest, req.OperationID, prepared) // destination execution starts after commit
	return ForkResult{Session: ownSessionRecord(commit.session), Operation: ownOperationRecord(commit.operation)}, nil
}

// forkCommit carries the committed destination state of one fork transaction
// out to the post-commit coordinator installation.
type forkCommit struct {
	session   SessionRecord
	operation OperationRecord
	entries   []graphEntry
}

// resolveForkConflict resolves a lost concurrent same-ID fork race with the
// same exact lookup run once: a readable owner whose lineage and request
// kind match this fork is returned as the matching first destination, a
// different source/boundary/request use of that ID is invalid, and an
// unreadable or absent owner preserves the original storage conflict.
func (h *Harness) resolveForkConflict(ctx context.Context, req ForkRequest, conflict error) (ForkResult, error) {
	res, found, lerr := h.forkExisting(ctx, req)
	if lerr != nil {
		if isCorruption(lerr) { // no readable owner of the conflicting ID: the original conflict is preserved
			return ForkResult{}, conflict
		}
		return ForkResult{}, lerr
	}
	if !found {
		return ForkResult{}, conflict
	}
	return res, nil
}

// forkExisting is Fork's idempotency lookup: it scans the sorted Session IDs
// and locates the caller Operation ID through exact Operation-register reads
// only, skipping not-found and Session-scoped corruption before the ID is
// located. A located envelope's Session is materialized through the same
// graph validator as every other Harness read before its Operation is
// returned — corruption after location is returned rather than decoded
// through a second semantic path. The existing Operation is returned only
// when that validated destination Session's source/boundary lineage and
// message request kind match this fork; other reuse is invalid. A
// storage-service failure aborts.
func (h *Harness) forkExisting(ctx context.Context, req ForkRequest) (ForkResult, bool, error) {
	ids, err := h.deps.Storage.ListSessionIDs(ctx)
	if err != nil {
		return ForkResult{}, false, err
	}
	for _, sessionID := range ids {
		if _, err := h.deps.Storage.ReadRegister(ctx, RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: req.OperationID}); err != nil {
			h.markCorrupt(sessionID, err)                         // a Session-scoped read failure sticks to the materialized coordinator; other classes are a no-op
			if errors.Is(err, ErrNotFound) || isCorruption(err) { // not-found and Session-scoped corruption before the ID is located are skipped
				continue
			}
			return ForkResult{}, false, err
		}
		graph, err := validateSessionGraph(ctx, h.deps.Storage, sessionID)
		if err != nil { // corruption after location is returned
			h.markCorrupt(sessionID, err)
			return ForkResult{}, false, err
		}
		rec, ok := graph.Operation(req.OperationID)
		if !ok {
			return ForkResult{}, false, corruptSession(sessionID, "located operation register %q is absent from the validated graph", req.OperationID)
		}
		sess := graph.Session
		if sess.Identity.SourceSessionID != req.SourceSessionID || sess.Identity.SourceBoundaryEntryID != req.BoundaryEntryID ||
			rec.Admission.RequestKind != RequestKindMessage {
			return ForkResult{}, false, invalidInput("operation %q already exists in session %q with different fork lineage or request kind", req.OperationID, sessionID)
		}
		return ForkResult{Session: ownSessionRecord(sess), Operation: ownOperationRecord(rec)}, true, nil
	}
	return ForkResult{}, false, nil
}

// forkTransaction is Fork's one store-wide transaction: it revalidates the
// source identity and revision, requires the selected boundary to be a
// user-origin input entry, creates the destination Session with source
// Workspace/current Agent type and source/boundary lineage, copies only the
// strict-before-boundary input, assistant, tool_result, and signal entries
// under new identities — clearing source Operation ownership and usage and
// rewriting copied references, with copied signal source Operations
// informational only — excludes Operation settlements and every source
// Operation register, and runs the shared in-transaction destination
// admission producer with a fixed user origin and the freshly prepared
// capture. First-writer resolution stays with the Fork caller.
func (h *Harness) forkTransaction(tx Transaction, destID string, view SessionRecord, req ForkRequest, content []model.ContentPart, capture ExecutionCapture, out *forkCommit) error {
	sreg, err := tx.ReadRegister(RegisterKey{SessionID: req.SourceSessionID, Kind: RegisterSession})
	if err != nil {
		return err
	}
	source, err := decodeSessionRegister(sreg)
	if err != nil {
		return corruptSession(req.SourceSessionID, "session register: %v", err)
	}
	if sreg.Revision != view.Revision || source.Identity != view.Identity {
		return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, req.SourceSessionID, view.Revision, sreg.Revision)
	}
	entries, err := tx.ReadEntries(req.SourceSessionID, 0)
	if err != nil {
		return err
	}
	boundarySeq, boundaryFound := int64(0), false
	for _, env := range entries {
		if env.ID != req.BoundaryEntryID {
			continue
		}
		decoded, err := decodeGraphEntry(req.SourceSessionID, env)
		if err != nil {
			return err
		}
		if decoded.Input == nil {
			return invalidInput("boundary entry %q is a %s entry; forking requires an input boundary", req.BoundaryEntryID, env.Kind)
		}
		if decoded.Input.Origin != InputOriginUser {
			return invalidInput("boundary entry %q has origin %q; forking requires a user-origin boundary", req.BoundaryEntryID, decoded.Input.Origin)
		}
		boundarySeq, boundaryFound = env.Sequence, true
		break
	}
	if !boundaryFound {
		return invalidInput("boundary entry %q not found in source session %q", req.BoundaryEntryID, req.SourceSessionID)
	}

	now := time.Now().UTC() // one sampled creation time for created_at and last_activity
	dest := SessionRecord{
		Identity: SessionIdentity{
			SessionID:             destID,
			Workspace:             source.Identity.Workspace,
			CreatedAt:             now,
			SourceSessionID:       req.SourceSessionID,
			SourceBoundaryEntryID: req.BoundaryEntryID,
		},
		State: SessionState{
			Lifecycle:        LifecycleOpen,
			CurrentAgentType: source.State.CurrentAgentType,
			Usage:            UsageTotals{},
			LastActivity:     now,
		},
	}
	destPayload, err := encodeSessionRegister(dest)
	if err != nil {
		return err
	}
	destReg, err := tx.InsertRegister(RegisterDraft{Key: RegisterKey{SessionID: destID, Kind: RegisterSession}, Payload: destPayload})
	if err != nil {
		return err
	}
	dest.Revision = destReg.Revision

	// copy only the strict-before-boundary eligible entries; settlements are
	// excluded and every copy is independently owned by the destination
	entryIDs := make(map[string]string)  // source entry id -> destination entry id
	resultIDs := make(map[string]string) // reserved source result id -> destination result id
	var copied []graphEntry
	for _, env := range entries {
		if env.Sequence >= boundarySeq {
			break // entries are ascending: nothing from the boundary on is copied
		}
		decoded, err := decodeGraphEntry(req.SourceSessionID, env)
		if err != nil {
			return err
		}
		adopted, err := copyForkEntry(tx, destID, req.SourceSessionID, decoded, entryIDs, resultIDs)
		if err != nil {
			return err
		}
		if adopted != nil {
			copied = append(copied, *adopted)
		}
	}

	record, committed, entry, perr := produceAdmission(tx, dest, capture, admissionRequest{
		SessionID:   destID,
		OperationID: req.OperationID,
		Origin:      InputOriginUser,
		Content:     content,
	})
	if perr != nil {
		return perr // first-writer resolution belongs to the Fork caller
	}
	out.session, out.operation = committed, record
	out.entries = append(copied, entry)
	return nil
}

// copyForkEntry copies one eligible source entry into the destination under
// a new identity: source Operation ownership and usage are cleared, copied
// entry references are rewritten to the copied identities, and a copied
// signal's source Operation stays informational history. Operation
// settlements are excluded. A nil return with no error marks an excluded
// entry.
func copyForkEntry(tx Transaction, destID, sourceID string, decoded graphEntry, entryIDs, resultIDs map[string]string) (*graphEntry, error) {
	var draft EntryDraft
	switch {
	case decoded.Input != nil:
		newID, err := newHexID()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrStorage, err)
		}
		copied := *decoded.Input
		copied.SessionID, copied.EntryID, copied.OperationID = destID, newID, ""
		payload, err := encodeInputEntry(copied)
		if err != nil {
			return nil, err
		}
		draft = EntryDraft{SessionID: destID, ID: newID, Kind: EntryInput, Payload: payload}
	case decoded.Assistant != nil:
		newID, err := newHexID()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrStorage, err)
		}
		copied := *decoded.Assistant
		copied.SessionID, copied.EntryID, copied.OperationID = destID, newID, ""
		copied.Usage = nil // copied prefix entries carry no source usage
		for i := range copied.ToolCalls {
			fresh, err := newHexID()
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrStorage, err)
			}
			resultIDs[copied.ToolCalls[i].ResultEntryID] = fresh
			copied.ToolCalls[i].ResultEntryID = fresh
		}
		payload, err := encodeAssistantEntry(copied)
		if err != nil {
			return nil, err
		}
		draft = EntryDraft{SessionID: destID, ID: newID, Kind: EntryAssistant, Payload: payload}
	case decoded.ToolResult != nil:
		copied := *decoded.ToolResult
		published, ok := entryIDs[copied.AssistantEntry.EntryID]
		if !ok { // unreachable for a validated source: the reference lies outside the copied prefix
			return nil, corruptSession(sourceID, "copied tool result %s references assistant entry %q outside the copied prefix", decoded.Envelope.ID, copied.AssistantEntry.EntryID)
		}
		// the copied result commits exactly under its call's rewritten reservation
		newID, ok := resultIDs[decoded.Envelope.ID]
		if !ok {
			return nil, corruptSession(sourceID, "copied tool result %s has no rewritten reservation", decoded.Envelope.ID)
		}
		copied.SessionID, copied.EntryID, copied.OperationID = destID, newID, ""
		copied.AssistantEntry = EntryRef{SessionID: destID, EntryID: published}
		payload, err := encodeToolResultEntry(copied)
		if err != nil {
			return nil, err
		}
		draft = EntryDraft{SessionID: destID, ID: newID, Kind: EntryToolResult, Payload: payload}
	case decoded.Signal != nil:
		newID, err := newHexID()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrStorage, err)
		}
		copied := *decoded.Signal
		copied.SessionID, copied.EntryID, copied.OperationID = destID, newID, ""
		// the related source Operation remains informational history
		payload, err := encodeSignalEntry(copied)
		if err != nil {
			return nil, err
		}
		draft = EntryDraft{SessionID: destID, ID: newID, Kind: EntrySignal, Payload: payload}
	default:
		return nil, nil // Operation settlements and every other kind are excluded
	}
	inserted, err := tx.InsertEntry(draft)
	if err != nil {
		return nil, err
	}
	adopted, err := decodeGraphEntry(destID, inserted)
	if err != nil {
		return nil, corruptSession(destID, "copied entry %s: %v", inserted.ID, err)
	}
	entryIDs[decoded.Envelope.ID] = adopted.Envelope.ID
	return &adopted, nil
}
