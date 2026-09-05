package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// Dependencies are the construction inputs of one Harness: the durable
// Storage and the single preparation callback shared by every admission
// producer.
type Dependencies struct {
	Storage Storage
	Prepare func(context.Context, PreparationRequest) (PreparedExecution, error)
}

// PreparationSession is the revision-free owned Session view one preparation
// receives.
type PreparationSession struct {
	Identity  SessionIdentity
	AgentType string
}

// PreparationRequest is the input of one preparation call.
type PreparationRequest struct {
	Session     PreparationSession
	RequestKind RequestKind
}

// PreparedExecution is the outcome of one preparation: the durable capture
// plus the two process-local effect functions the Harness consumes. No
// returned function is durable.
type PreparedExecution struct {
	Capture ExecutionCapture
	Model   agent.ModelEffect
	Tool    func(context.Context, model.ToolCall) PreparedTool
}

// PreparedTool is one tool plan: exactly one immediate result or executor,
// with normalized arguments that are nil or one valid JSON value.
type PreparedTool struct {
	NormalizedArguments json.RawMessage
	Immediate           *model.ToolResult
	Execute             func(context.Context) model.ToolResult
}

// CreateSessionRequest is the input of one root Session creation.
type CreateSessionRequest struct {
	Workspace string
	AgentType string
}

// SubmitRequest is the input of one public Session message submission.
type SubmitRequest struct {
	SessionID   string
	OperationID string
	Origin      InputOrigin
	Content     []model.ContentPart
	Mode        MessageMode
}

// SubmitResult is the outcome of one public Submit. Operation is present for
// admitted/existing only; a buffered item is not a durable Operation yet.
type SubmitResult struct {
	Disposition SubmitDisposition
	Operation   *OperationRecord
}

// SweepPolicy is the explicit-time lifecycle thresholds of one Sweep call: an
// open Session is archived when now-last_activity exceeds ArchiveAfter, and an
// archived Session is deleted when now-archived_at exceeds DeleteAfterArchive.
// A nonpositive threshold disables only its corresponding transition.
type SweepPolicy struct {
	ArchiveAfter       time.Duration
	DeleteAfterArchive time.Duration
}

// Harness is the public durable state machine over one Storage. It owns
// Session materialization, reads, root creation, Agent-type change, the shared
// admission producer, public Submit with its two process-local buffers, the
// Session lifecycle (reopen, archive, delete, and explicit-time sweep), and
// execution; it owns neither storage close nor Runtime shutdown policy.
type Harness struct {
	ctx  context.Context
	deps Dependencies

	mu       sync.Mutex
	sessions map[string]*coordinator

	// generation counts post-commit coordinator removals: a materialization
	// that spans one retries against the current registry instead of
	// installing a stale coordinator for an absent Session.
	generation int

	// storageFailure retains the first storage-class failure that stopped
	// admitted work; Wait returns it after all process-local work has ended.
	storageFailure error
}

// pendingMessage is one buffered Submit: a process-local FIFO item that is
// neither a durable Operation nor a Session entry while it waits, carries no
// idempotency, and is discarded on Harness-context loss.
type pendingMessage struct {
	operationID string
	origin      InputOrigin
	content     []model.ContentPart
}

// activeExecution is the one in-flight execution of a coordinator: installed
// after the admission commit and released only after the terminal settlement
// and the post-terminal buffer drain have converged.
type activeExecution struct {
	done chan struct{} // closed when the execution goroutine finishes
}

// coordinator is the one per-Session authority: the validated Session view,
// a sticky corruption marker, the single admission/fork reservation, the two
// process-local message FIFOs, and the optional active execution.
type coordinator struct {
	mu    sync.Mutex
	graph *sessionGraph
	corru error

	reserved chan struct{} // non-nil while one reservation is held; closed on release

	steering []*pendingMessage // regular input waiting for the next model boundary
	queued   []*pendingMessage // queued input waiting for the next Agent turn end
	run      *activeExecution  // non-nil while one execution is in flight

	gone bool // set by the post-commit deletion invalidation: every holder of the coordinator gets ErrNotFound from then on
}

// New constructs one Harness over the given dependencies. It requires a live
// Harness context and non-nil dependencies, performs no I/O, and starts no
// goroutine.
func New(ctx context.Context, deps Dependencies) (*Harness, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, invalidInput("harness construction requires a live context")
	}
	if deps.Storage == nil || deps.Prepare == nil {
		return nil, invalidInput("harness construction requires non-nil storage and preparation dependencies")
	}
	return &Harness{ctx: ctx, deps: deps, sessions: map[string]*coordinator{}}, nil
}

// CreateSession creates one Workspace-bound root Session with a random
// identity and one sampled creation time for created_at and last_activity.
func (h *Harness) CreateSession(ctx context.Context, req CreateSessionRequest) (SessionRecord, error) {
	if err := validateWorkspace(req.Workspace); err != nil {
		return SessionRecord{}, invalidInput("workspace: %v", err)
	}
	if req.AgentType == "" {
		return SessionRecord{}, invalidInput("agent type must be non-empty")
	}
	sessionID, err := newHexID()
	if err != nil {
		return SessionRecord{}, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	now := time.Now().UTC()
	record := SessionRecord{
		Identity: SessionIdentity{SessionID: sessionID, Workspace: req.Workspace, CreatedAt: now},
		State: SessionState{
			Lifecycle:        LifecycleOpen,
			CurrentAgentType: req.AgentType,
			Usage:            UsageTotals{},
			LastActivity:     now,
		},
	}
	payload, err := encodeSessionRegister(record)
	if err != nil {
		return SessionRecord{}, err
	}
	if err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		registered, err := tx.InsertRegister(RegisterDraft{Key: RegisterKey{SessionID: sessionID, Kind: RegisterSession}, Payload: payload})
		if err != nil {
			return err
		}
		record.Revision = registered.Revision
		return nil
	}); err != nil {
		return SessionRecord{}, err
	}
	h.mu.Lock()
	h.sessions[sessionID] = &coordinator{graph: &sessionGraph{Session: record}}
	h.mu.Unlock()
	return ownSessionRecord(record), nil
}

// ReadSession returns the materialized Session record of one Session. A
// deletion that committed after materialization resolves to ErrNotFound
// rather than a stale cached read.
func (h *Harness) ReadSession(ctx context.Context, sessionID string) (SessionRecord, error) {
	c, err := h.coordinatorFor(ctx, sessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return SessionRecord{}, notFoundSession(sessionID)
	}
	return ownSessionRecord(c.graph.Session), nil
}

// ReadOperation returns the materialized Operation record of one Operation of
// one Session. A deletion that committed after materialization resolves to
// ErrNotFound rather than a stale cached read.
func (h *Harness) ReadOperation(ctx context.Context, sessionID, operationID string) (OperationRecord, error) {
	if err := validateOperationIdentity(operationID, "operation id"); err != nil {
		return OperationRecord{}, invalidInput("operation id: %v", err)
	}
	c, err := h.coordinatorFor(ctx, sessionID)
	if err != nil {
		return OperationRecord{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return OperationRecord{}, notFoundSession(sessionID)
	}
	rec, ok := c.graph.Operation(operationID)
	if !ok {
		return OperationRecord{}, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	return ownOperationRecord(rec), nil
}

// ChangeAgentType changes the current Agent type of an open Session. It
// preserves identity, usage, history, and last activity, and never alters an
// active Operation's admission capture.
func (h *Harness) ChangeAgentType(ctx context.Context, sessionID, agentType string) (SessionRecord, error) {
	if agentType == "" {
		return SessionRecord{}, invalidInput("agent type must be non-empty")
	}
	c, err := h.coordinatorFor(ctx, sessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	for {
		if err := c.waitIdle(ctx); err != nil {
			return SessionRecord{}, err
		}
		c.mu.Lock()
		if c.reserved != nil { // a reservation was taken between the wait and the lock
			c.mu.Unlock()
			continue
		}
		if c.gone { // a deletion committed while this call waited or relocked
			c.mu.Unlock()
			return SessionRecord{}, notFoundSession(sessionID)
		}
		if c.graph.Session.State.Lifecycle != LifecycleOpen {
			c.mu.Unlock()
			return SessionRecord{}, invalidInput("session %q is archived; the Agent type requires an open Session", sessionID)
		}
		expected := c.graph.Session.Revision
		var updated SessionRecord
		err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
			key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
			reg, err := tx.ReadRegister(key)
			if err != nil {
				return err
			}
			current, err := decodeSessionRegister(reg)
			if err != nil {
				return corruptSession(sessionID, "session register: %v", err)
			}
			if reg.Revision != expected {
				return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, expected, reg.Revision)
			}
			state := current.State
			state.CurrentAgentType = agentType
			changed := SessionRecord{Identity: current.Identity, State: state}
			payload, err := encodeSessionRegister(changed)
			if err != nil {
				return err
			}
			replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
			if err != nil {
				return err
			}
			changed.Revision = replaced.Revision
			updated = changed
			return nil
		})
		if err != nil {
			h.markCorrupt(sessionID, err)
			c.mu.Unlock()
			if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
				if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
					return SessionRecord{}, rerr
				}
			}
			return SessionRecord{}, err
		}
		c.graph.Session = updated
		result := ownSessionRecord(updated)
		c.mu.Unlock()
		return result, nil
	}
}

// Submit submits one Session message. While the Session is idle, regular and
// queued input use normal admission; while an Operation is active, regular
// input enters the steering FIFO and queued input enters the queued FIFO. The
// buffers are process-local: a buffered item is not a durable Operation or
// Session entry, has no idempotency, and is discarded on Harness-context
// loss. An existing same-Session/message Operation resolves before routing.
func (h *Harness) Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error) {
	if err := h.ctx.Err(); err != nil { // cancellation has closed admission
		return SubmitResult{}, err
	}
	switch req.Mode {
	case MessageModeRegular, MessageModeQueued:
	default:
		return SubmitResult{}, invalidInput("message mode %q is not one of regular or queued", req.Mode)
	}
	content, err := validateSubmitInput(req.OperationID, req.Origin, req.Content)
	if err != nil {
		return SubmitResult{}, err
	}
	c, err := h.coordinatorFor(ctx, req.SessionID)
	if err != nil {
		return SubmitResult{}, err
	}
	c.mu.Lock()
	if c.gone { // a deletion committed after materialization: no buffer or admission on the absent Session
		c.mu.Unlock()
		return SubmitResult{}, notFoundSession(req.SessionID)
	}
	if rec, ok := c.graph.Operation(req.OperationID); ok { // existing resolves before routing
		first := ownOperationRecord(rec)
		c.mu.Unlock()
		return SubmitResult{Disposition: DispositionExisting, Operation: &first}, nil
	}
	if c.graph.Session.State.Lifecycle != LifecycleOpen {
		c.mu.Unlock()
		return SubmitResult{}, invalidInput("session %q is archived; admission requires an open Session", req.SessionID)
	}
	if err := ctx.Err(); err != nil { // the routing gate: cancellation observed here publishes nothing
		c.mu.Unlock()
		return SubmitResult{}, err
	}
	if err := h.ctx.Err(); err != nil {
		c.mu.Unlock()
		return SubmitResult{}, err
	}
	if c.graph.Session.State.CurrentOperationID != "" || c.run != nil { // active: buffer by mode; the enqueue is the publication gate
		item := &pendingMessage{operationID: req.OperationID, origin: req.Origin, content: content}
		disposition := DispositionSteering
		if req.Mode == MessageModeQueued {
			c.queued = append(c.queued, item)
			disposition = DispositionQueued
		} else {
			c.steering = append(c.steering, item)
		}
		c.mu.Unlock()
		return SubmitResult{Disposition: disposition}, nil
	}
	c.mu.Unlock()

	rec, disposition, err := h.admit(ctx, admissionRequest{
		SessionID:   req.SessionID,
		OperationID: req.OperationID,
		Origin:      req.Origin,
		Content:     content,
	})
	if err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{Disposition: disposition, Operation: &rec}, nil
}

// ReopenSession reopens one archived Session: it changes the lifecycle to
// open, clears the archived time, and advances last activity to the reopen
// time, preserving history, usage, context, lineage, and Agent type.
// Reopening an already open Session is a no-write success.
func (h *Harness) ReopenSession(ctx context.Context, sessionID string) (SessionRecord, error) {
	c, err := h.coordinatorFor(ctx, sessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	for {
		if err := c.waitIdle(ctx); err != nil {
			return SessionRecord{}, err
		}
		c.mu.Lock()
		if c.reserved != nil { // a reservation was taken between the wait and the lock
			c.mu.Unlock()
			continue
		}
		if c.gone { // a deletion committed while this call waited or relocked
			c.mu.Unlock()
			return SessionRecord{}, notFoundSession(sessionID)
		}
		if c.graph.Session.State.Lifecycle != LifecycleArchived { // reopening open is a no-write success
			result := ownSessionRecord(c.graph.Session)
			c.mu.Unlock()
			return result, nil
		}
		expected := c.graph.Session.Revision
		now := time.Now().UTC() // the reopen time advances last activity
		var updated SessionRecord
		err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
			key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
			reg, err := tx.ReadRegister(key)
			if err != nil {
				return err
			}
			current, err := decodeSessionRegister(reg)
			if err != nil {
				return corruptSession(sessionID, "session register: %v", err)
			}
			if reg.Revision != expected {
				return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, expected, reg.Revision)
			}
			state := current.State
			state.Lifecycle = LifecycleOpen
			state.ArchivedAt = nil
			state.LastActivity = now
			changed := SessionRecord{Identity: current.Identity, State: state}
			payload, err := encodeSessionRegister(changed)
			if err != nil {
				return err
			}
			replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
			if err != nil {
				return err
			}
			changed.Revision = replaced.Revision
			updated = changed
			return nil
		})
		if err != nil {
			h.markCorrupt(sessionID, err)
			c.mu.Unlock()
			if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
				if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
					return SessionRecord{}, rerr
				}
			}
			return SessionRecord{}, err
		}
		c.graph.Session = updated
		result := ownSessionRecord(updated)
		c.mu.Unlock()
		return result, nil
	}
}

// ArchiveSession archives one open idle Session: it changes the lifecycle to
// archived with one sampled UTC time, leaves last activity unchanged, and
// clears the process-local buffers only after the commit. Archiving a running
// Session is rejected; archiving an already archived Session is a no-write
// success.
func (h *Harness) ArchiveSession(ctx context.Context, sessionID string) (SessionRecord, error) {
	c, err := h.coordinatorFor(ctx, sessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	for {
		if err := c.waitIdle(ctx); err != nil {
			return SessionRecord{}, err
		}
		c.mu.Lock()
		if c.reserved != nil { // a reservation was taken between the wait and the lock
			c.mu.Unlock()
			continue
		}
		if c.gone { // a deletion committed while this call waited or relocked
			c.mu.Unlock()
			return SessionRecord{}, notFoundSession(sessionID)
		}
		if c.graph.Session.State.CurrentOperationID != "" {
			c.mu.Unlock()
			return SessionRecord{}, invalidInput("session %q already runs operation %q; archiving requires an idle Session", sessionID, c.graph.Session.State.CurrentOperationID)
		}
		if c.graph.Session.State.Lifecycle == LifecycleArchived { // archiving archived is a no-write success
			result := ownSessionRecord(c.graph.Session)
			c.mu.Unlock()
			return result, nil
		}
		expected := c.graph.Session.Revision
		now := time.Now().UTC() // the one sampled archive time
		var updated SessionRecord
		err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
			key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
			reg, err := tx.ReadRegister(key)
			if err != nil {
				return err
			}
			current, err := decodeSessionRegister(reg)
			if err != nil {
				return corruptSession(sessionID, "session register: %v", err)
			}
			if reg.Revision != expected {
				return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, expected, reg.Revision)
			}
			state := current.State
			stamped := now
			state.Lifecycle = LifecycleArchived
			state.ArchivedAt = &stamped
			changed := SessionRecord{Identity: current.Identity, State: state}
			payload, err := encodeSessionRegister(changed)
			if err != nil {
				return err
			}
			replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
			if err != nil {
				return err
			}
			changed.Revision = replaced.Revision
			updated = changed
			return nil
		})
		if err != nil {
			h.markCorrupt(sessionID, err)
			c.mu.Unlock()
			if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
				if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
					return SessionRecord{}, rerr
				}
			}
			return SessionRecord{}, err
		}
		c.graph.Session = updated
		result := ownSessionRecord(updated)
		c.steering, c.queued = nil, nil // the buffers clear only after the commit
		c.mu.Unlock()
		return result, nil
	}
}

// DeleteSession deletes one archived Session: the same transaction that
// validates its state performs the storage deletion. After the commit it uses
// the post-commit coordinator invalidation: callers already holding that
// coordinator and later materialization both return ErrNotFound. Deleting an
// open or absent Session is rejected without a write.
func (h *Harness) DeleteSession(ctx context.Context, sessionID string) error {
	c, err := h.coordinatorFor(ctx, sessionID)
	if err != nil {
		return err
	}
	for {
		if err := c.waitIdle(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		if c.reserved != nil { // a reservation was taken between the wait and the lock
			c.mu.Unlock()
			continue
		}
		if c.gone { // a deletion committed while this call waited or relocked
			c.mu.Unlock()
			return notFoundSession(sessionID)
		}
		if c.graph.Session.State.Lifecycle != LifecycleArchived {
			c.mu.Unlock()
			return invalidInput("session %q is open; deletion requires an archived Session", sessionID)
		}
		expected := c.graph.Session.Revision
		err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
			key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
			reg, err := tx.ReadRegister(key)
			if err != nil {
				return err
			}
			if reg.Revision != expected {
				return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, expected, reg.Revision)
			}
			return tx.DeleteSession(sessionID)
		})
		if err != nil {
			h.markCorrupt(sessionID, err)
			c.mu.Unlock()
			if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
				if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
					return rerr
				}
			}
			return err
		}
		c.invalidate() // post-commit, under the same coordinator ownership: buffers cleared and absence marked before registry removal
		c.mu.Unlock()
		h.removeCoordinator(sessionID)
		return nil
	}
}

// Sweep runs one explicit-time lifecycle pass: it rejects a zero time before
// any storage read, enumerates the sorted Session IDs, and handles each in its
// own transaction — an open idle Session is archived when now-last_activity
// exceeds ArchiveAfter, and an archived Session is deleted when
// now-archived_at exceeds DeleteAfterArchive. A nonpositive threshold disables
// only its corresponding transition. Corrupt rows (including listed identities
// that violate the durable shape), running, or process-locally buffered
// Sessions are left unchanged; any other non-corruption error stops and
// returns from the call. Sweep deletion uses the same post-commit coordinator
// invalidation as DeleteSession. It publishes no per-Session status, starts no
// ticker, reads no configuration, and emits no event.
func (h *Harness) Sweep(ctx context.Context, policy SweepPolicy, now time.Time) error {
	if now.IsZero() {
		return invalidInput("sweep time must not be zero")
	}
	ids, err := h.deps.Storage.ListSessionIDs(ctx)
	if err != nil {
		return err
	}
	for _, sessionID := range ids {
		if err := validateHexID(sessionID, "session id"); err != nil { // a listed identity violating the durable shape is a corrupt row: left unchanged, the pass continues
			continue
		}
		c, err := h.coordinatorFor(ctx, sessionID)
		if err != nil {
			if !isCorruption(err) { // a corrupt Session is left unchanged; every other error stops the sweep
				return err
			}
			continue
		}
		if err := h.sweepOne(ctx, c, sessionID, policy, now); err != nil {
			if !isCorruption(err) { // corruption discovered under the cached view leaves the Session unchanged
				return err
			}
			continue
		}
	}
	return nil
}

// sweepOne handles one Sweep Session through the coordinator: it waits for an
// idle reservation like every other transition, then — under the coordinator
// mutex held across its own transaction and post-commit adoption — re-reads
// the current Session register and applies at most one transition against the
// explicit sweep time: archive for an open idle Session past its threshold,
// deletion for an archived Session past its threshold. A running or
// process-locally buffered Session is left unchanged. Deletion uses the same
// post-commit coordinator invalidation as DeleteSession; archiving adopts the
// committed record and clears the buffers under the same hold.
func (h *Harness) sweepOne(ctx context.Context, c *coordinator, sessionID string, policy SweepPolicy, now time.Time) error {
	for {
		if err := c.waitIdle(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		if c.reserved != nil { // a reservation was taken between the wait and the lock
			c.mu.Unlock()
			continue
		}
		if c.gone { // a deletion committed while this call waited or relocked
			c.mu.Unlock()
			return notFoundSession(sessionID)
		}
		if c.run != nil || len(c.steering) > 0 || len(c.queued) > 0 { // running or process-locally buffered: left unchanged
			c.mu.Unlock()
			return nil
		}
		var (
			archived   SessionRecord
			didArchive bool
			deleted    bool
		)
		err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
			key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
			reg, err := tx.ReadRegister(key)
			if err != nil {
				return err
			}
			current, err := decodeSessionRegister(reg)
			if err != nil {
				return corruptSession(sessionID, "session register: %v", err)
			}
			switch current.State.Lifecycle {
			case LifecycleOpen:
				if policy.ArchiveAfter <= 0 || current.State.CurrentOperationID != "" {
					return nil // a disabled threshold or a running Session is left unchanged
				}
				if now.Sub(current.State.LastActivity) <= policy.ArchiveAfter {
					return nil
				}
				state := current.State
				stamped := now // the sweep time stamps the archive
				state.Lifecycle = LifecycleArchived
				state.ArchivedAt = &stamped
				changed := SessionRecord{Identity: current.Identity, State: state}
				payload, err := encodeSessionRegister(changed)
				if err != nil {
					return err
				}
				replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
				if err != nil {
					return err
				}
				changed.Revision = replaced.Revision
				archived, didArchive = changed, true
			case LifecycleArchived:
				if policy.DeleteAfterArchive <= 0 {
					return nil
				}
				if now.Sub(*current.State.ArchivedAt) <= policy.DeleteAfterArchive {
					return nil
				}
				if err := tx.DeleteSession(sessionID); err != nil {
					return err
				}
				deleted = true
			}
			return nil
		})
		if err != nil {
			h.markCorrupt(sessionID, err)
			c.mu.Unlock()
			return err
		}
		if deleted { // the same post-commit coordinator invalidation as DeleteSession, under this hold
			c.invalidate()
			c.mu.Unlock()
			h.removeCoordinator(sessionID)
			return nil
		}
		if didArchive {
			c.graph.Session = archived
			c.steering, c.queued = nil, nil // like ArchiveSession: the buffers clear only after the commit
		}
		c.mu.Unlock()
		return nil
	}
}

// invalidate marks one coordinator absent under the caller-held coordinator
// mutex: it clears the process-local buffers and sets the gone flag in the
// same critical section as the deletion commit's adoption, so no holder can
// observe a committed deletion without the absence. The caller removes the
// registry entry through removeCoordinator after releasing the mutex. From
// then on, callers already holding the coordinator and later materialization
// both return ErrNotFound.
func (c *coordinator) invalidate() {
	c.steering, c.queued = nil, nil
	c.gone = true
}

// removeCoordinator drops one invalidated coordinator from the registry and
// bumps the registry generation, so a materialization that started before the
// removal retries against the post-deletion state instead of installing a
// stale coordinator for an absent Session.
func (h *Harness) removeCoordinator(sessionID string) {
	h.mu.Lock()
	delete(h.sessions, sessionID)
	h.generation++
	h.mu.Unlock()
}

// isCorruption reports whether one error is a persisted semantic violation of
// a Session, which lifecycle transitions leave unchanged rather than acting on.
func isCorruption(err error) bool {
	var corrupt *CorruptionError
	return errors.As(err, &corrupt)
}

// notFoundSession is the absent-Session error: the landed not-found class for
// one deleted or unknown Session identity.
func notFoundSession(sessionID string) error {
	return fmt.Errorf("%w: session %q not found", ErrNotFound, sessionID)
}

// coordinatorFor returns the materialized coordinator of one Session,
// validating its graph on first materialization. A persisted semantic
// violation caches a sticky corruption marker that makes the Session
// unavailable in this Harness instance; other Sessions remain usable. A
// deleted coordinator stays absent to every holder: it resolves to the
// not-found class instead of a stale cached read.
func (h *Harness) coordinatorFor(ctx context.Context, sessionID string) (*coordinator, error) {
	for {
		h.mu.Lock()
		c := h.sessions[sessionID]
		if c != nil {
			corru := c.corru
			h.mu.Unlock()
			if err := c.goneErr(sessionID); err != nil {
				return nil, err
			}
			if corru != nil {
				return nil, corru
			}
			return c, nil
		}
		generation := h.generation // captured on the cache miss: a removal during validation must force a retry
		h.mu.Unlock()
		graph, err := validateSessionGraph(ctx, h.deps.Storage, sessionID)
		if err != nil {
			h.markCorrupt(sessionID, err)
			return nil, err
		}
		h.mu.Lock()
		if h.generation != generation { // a post-commit coordinator removal landed during validation: retry against the current registry
			h.mu.Unlock()
			continue
		}
		c = h.sessions[sessionID]
		if c == nil {
			c = &coordinator{graph: graph}
			h.sessions[sessionID] = c
		}
		corru := c.corru
		h.mu.Unlock()
		if err := c.goneErr(sessionID); err != nil {
			return nil, err
		}
		if corru != nil {
			return nil, corru
		}
		return c, nil
	}
}

// goneErr reports the coordinator's post-deletion absence. The flag is set
// under the coordinator mutex by the deletion invalidation and read here with
// the registry lookup released, so no lock is ever held across the other.
func (c *coordinator) goneErr(sessionID string) error {
	c.mu.Lock()
	gone := c.gone
	c.mu.Unlock()
	if gone {
		return notFoundSession(sessionID)
	}
	return nil
}

// markCorrupt makes one Session unavailable in this Harness instance when a
// persisted semantic violation is detected outside first materialization.
func (h *Harness) markCorrupt(sessionID string, err error) {
	var corrupt *CorruptionError
	if !errors.As(err, &corrupt) {
		return
	}
	h.mu.Lock()
	if c := h.sessions[sessionID]; c != nil && c.corru == nil {
		c.corru = err
	}
	h.mu.Unlock()
}

// reserve takes the Session's single admission reservation, waiting while
// another reservation is held, subject to the caller's context. The returned
// release resolves it exactly once.
func (c *coordinator) reserve(ctx context.Context) (func(), error) {
	for {
		c.mu.Lock()
		if c.reserved == nil {
			c.reserved = make(chan struct{})
			c.mu.Unlock()
			return c.releaseReservation, nil
		}
		held := c.reserved
		c.mu.Unlock()
		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *coordinator) releaseReservation() {
	c.mu.Lock()
	if c.reserved != nil {
		close(c.reserved)
		c.reserved = nil
	}
	c.mu.Unlock()
}

// waitIdle waits until no admission reservation is held. A caller that
// transitions Session state must re-check idleness after acquiring the
// coordinator mutex, which blocks new reservations.
func (c *coordinator) waitIdle(ctx context.Context) error {
	for {
		c.mu.Lock()
		if c.reserved == nil {
			c.mu.Unlock()
			return nil
		}
		held := c.reserved
		c.mu.Unlock()
		select {
		case <-held:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// admissionRequest is the input of the private normal admission producer.
type admissionRequest struct {
	SessionID   string
	OperationID string
	Origin      InputOrigin
	Content     []model.ContentPart
}

// errAdmissionExisting aborts the admission transaction once the conflicting
// same-Session Operation has been read; it never escapes admit.
var errAdmissionExisting = errors.New("harness: operation already admitted in this session")

// errRevisionRace marks the revision-guard conflict of an admission,
// agent-type, or effect transaction: durable state changed under the
// coordinator's validated view. It keeps the landed conflict class reachable.
var errRevisionRace = fmt.Errorf("harness: session changed concurrently: %w", ErrConflict)

// validateSubmitInput validates one submitted message's identity, origin, and
// content, returning the owned content copy every downstream use consumes.
func validateSubmitInput(operationID string, origin InputOrigin, content []model.ContentPart) ([]model.ContentPart, error) {
	if err := validateOperationIdentity(operationID, "operation id"); err != nil {
		return nil, invalidInput("operation id: %v", err)
	}
	switch origin {
	case InputOriginUser, InputOriginRuntime, InputOriginPlugin:
	default:
		return nil, invalidInput("input origin %q is not one of user, runtime or plugin", origin)
	}
	owned := make([]model.ContentPart, 0, len(content))
	for i, part := range content {
		validated, err := model.NewContentPart(part) // the validated owned copy feeds every downstream use
		if err != nil {
			return nil, invalidInput("content[%d]: %v", i, err)
		}
		owned = append(owned, validated)
	}
	return owned, nil
}

// admit is the one normal admission path: validate the input, materialize the
// Session, return an existing same-Session Operation without preparing again,
// reserve admission, then run the reserved admission body. The reservation is
// the configuration-capture linearization point; preparation failure
// publishes nothing.
func (h *Harness) admit(ctx context.Context, req admissionRequest) (OperationRecord, SubmitDisposition, error) {
	content, err := validateSubmitInput(req.OperationID, req.Origin, req.Content)
	if err != nil {
		return OperationRecord{}, "", err
	}
	req.Content = content
	c, err := h.coordinatorFor(ctx, req.SessionID)
	if err != nil {
		return OperationRecord{}, "", err
	}
	c.mu.Lock()
	if c.gone { // a deletion committed while this call waited or relocked
		c.mu.Unlock()
		return OperationRecord{}, "", notFoundSession(req.SessionID)
	}
	if rec, ok := c.graph.Operation(req.OperationID); ok {
		c.mu.Unlock()
		return ownOperationRecord(rec), DispositionExisting, nil
	}
	if c.graph.Session.State.Lifecycle != LifecycleOpen {
		c.mu.Unlock()
		return OperationRecord{}, "", invalidInput("session %q is archived; admission requires an open Session", req.SessionID)
	}
	if c.graph.Session.State.CurrentOperationID != "" {
		running := c.graph.Session.State.CurrentOperationID
		c.mu.Unlock()
		return OperationRecord{}, "", invalidInput("session %q already runs operation %q; admission requires an idle Session", req.SessionID, running)
	}
	c.mu.Unlock()

	release, err := c.reserve(ctx)
	if err != nil {
		return OperationRecord{}, "", err
	}
	defer release()
	rec, prepared, disposition, err := h.admitReserved(ctx, c, req)
	if err != nil {
		return OperationRecord{}, "", err
	}
	if prepared != nil { // install process-local execution and start Agent only after commit
		h.startExecution(c, rec.Admission.OperationID, *prepared)
	}
	return rec, disposition, nil
}

// admitReserved is the reserved admission body: the re-checked existing,
// lifecycle, and idle guards, the outside-lock preparation, and the
// transactional publication. The caller holds the Session's admission
// reservation and releases it only after the returned execution, when present,
// is installed. A prepared execution returns only for a newly admitted
// Operation; an existing resolution carries none.
func (h *Harness) admitReserved(ctx context.Context, c *coordinator, req admissionRequest) (OperationRecord, *PreparedExecution, SubmitDisposition, error) {
	c.mu.Lock()
	if c.gone { // a deletion committed while the reservation holder was outside the lock: never prepare for an absent Session
		c.mu.Unlock()
		return OperationRecord{}, nil, "", notFoundSession(req.SessionID)
	}
	if rec, ok := c.graph.Operation(req.OperationID); ok { // the prior reservation holder admitted it
		c.mu.Unlock()
		return ownOperationRecord(rec), nil, DispositionExisting, nil
	}
	if c.graph.Session.State.Lifecycle != LifecycleOpen {
		c.mu.Unlock()
		return OperationRecord{}, nil, "", invalidInput("session %q is archived; admission requires an open Session", req.SessionID)
	}
	if c.graph.Session.State.CurrentOperationID != "" {
		running := c.graph.Session.State.CurrentOperationID
		c.mu.Unlock()
		return OperationRecord{}, nil, "", invalidInput("session %q already runs operation %q; admission requires an idle Session", req.SessionID, running)
	}
	view := c.graph.Session // the state the reservation resolved on
	c.mu.Unlock()

	prepared, capture, prepCtx, cleanup, err := h.prepareExecution(ctx, PreparationSession{
		Identity:  view.Identity,
		AgentType: view.State.CurrentAgentType,
	})
	if err != nil {
		return OperationRecord{}, nil, "", err
	}
	defer cleanup() // the combined context stays live until publication returns

	// the transaction runs on the combined preparation context: either
	// cancellation before commit aborts it, and a committed publication wins
	// later cancellation
	record, existing, err := h.publishAdmission(prepCtx, c, view, capture, req)
	if err != nil {
		return OperationRecord{}, nil, "", publicationError(ctx, err)
	}
	if existing {
		return ownOperationRecord(record), nil, DispositionExisting, nil
	}
	return ownOperationRecord(record), &prepared, DispositionAdmitted, nil
}

// prepareExecution is the shared outside-lock preparation and validation of
// one admission producer, used by normal admission and Fork alike: it merges
// the caller context with the Harness context, invokes the single preparation
// callback, requires non-nil model and tool functions and a valid capture,
// and returns the prepared execution with its owned durable capture plus the
// combined context and its cleanup. A preparation failure returns with the
// combined context already cleaned up; on success the caller keeps it live
// through publication and runs the cleanup when publication returns. Caller
// or Harness cancellation before or after preparation publishes nothing.
func (h *Harness) prepareExecution(ctx context.Context, session PreparationSession) (PreparedExecution, ExecutionCapture, context.Context, context.CancelFunc, error) {
	prepCtx, cancel := context.WithCancel(h.ctx)
	stop := context.AfterFunc(ctx, cancel)
	cleanup := func() {
		stop()
		cancel()
	}
	if err := ctx.Err(); err != nil { // the combined preparation context is already done: never invoke Prepare
		cleanup()
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, err
	}
	if err := h.ctx.Err(); err != nil {
		cleanup()
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, h.ctx.Err()
	}
	prepared, prepErr := h.deps.Prepare(prepCtx, PreparationRequest{
		Session:     session,
		RequestKind: RequestKindMessage,
	})
	if prepErr != nil {
		cleanup()
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, prepErr
	}
	if prepared.Model == nil || prepared.Tool == nil {
		cleanup()
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, invalidInput("prepared execution requires non-nil model and tool functions")
	}
	if err := validateExecutionCapture(prepared.Capture); err != nil {
		cleanup()
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, invalidInput("prepared execution capture: %v", err)
	}
	capture := ownCapture(prepared.Capture)

	if err := ctx.Err(); err != nil { // caller cancellation wins when both contexts are done
		cleanup()
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, err
	}
	if err := h.ctx.Err(); err != nil {
		cleanup()
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, h.ctx.Err()
	}
	return prepared, capture, prepCtx, cleanup, nil
}

// publicationError keeps the caller-facing cancellation class at the
// publication boundary: the combined preparation context is canceled through
// its AfterFunc hop, so a store abort that observed a caller deadline
// expiring during publication surfaces as context.Canceled even though the
// caller's own Err is context.DeadlineExceeded. The adapter restores that
// deadline class; nil, success, and every other error pass through
// unchanged.
func publicationError(caller context.Context, err error) error {
	if err != nil && errors.Is(err, context.Canceled) && caller.Err() == context.DeadlineExceeded {
		return caller.Err()
	}
	return err
}

// publishAdmission runs the in-transaction admission producer: re-read the
// exact Session revision, insert the input entry and the running Operation,
// and set the Session current Operation. A concurrent winner of the same
// identity resolves to the first Operation; a foreign revision change
// resolves to the existing Operation, a violated admission precondition, or
// a conflict; an insertion conflict without a same-Session owner is
// cross-Session reuse.
func (h *Harness) publishAdmission(ctx context.Context, c *coordinator, view SessionRecord, capture ExecutionCapture, req admissionRequest) (OperationRecord, bool, error) {
	var (
		published  OperationRecord
		newSession SessionRecord
		newEntry   graphEntry
		existing   bool
		foreign    bool
	)
	err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		sessionKey := RegisterKey{SessionID: req.SessionID, Kind: RegisterSession}
		reg, err := tx.ReadRegister(sessionKey)
		if err != nil {
			return err
		}
		current, err := decodeSessionRegister(reg)
		if err != nil {
			return corruptSession(req.SessionID, "session register: %v", err)
		}
		if reg.Revision != view.Revision {
			foreign = true
			rec, found, err := readExistingOperation(tx, req.SessionID, req.OperationID)
			if err != nil {
				return err
			}
			if found {
				published, existing = rec, true
				return errAdmissionExisting
			}
			// a violated semantic precondition outranks the conflict class
			if current.State.Lifecycle != LifecycleOpen {
				return invalidInput("session %q is archived; admission requires an open Session", req.SessionID)
			}
			if current.State.CurrentOperationID != "" {
				return invalidInput("session %q already runs operation %q; admission requires an idle Session", req.SessionID, current.State.CurrentOperationID)
			}
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, req.SessionID, view.Revision, reg.Revision)
		}
		record, committed, entry, perr := produceAdmission(tx, current, capture, req)
		if perr != nil {
			if errors.Is(perr, ErrConflict) { // first-writer resolution: the same-Session Operation read decides
				rec, found, rerr := readExistingOperation(tx, req.SessionID, req.OperationID)
				if rerr != nil {
					return rerr
				}
				if found {
					foreign = true
					published, existing = rec, true
					return errAdmissionExisting
				}
				return invalidInput("operation %q already exists in another session", req.OperationID)
			}
			return perr
		}
		published, newSession, newEntry = record, committed, entry
		return nil
	})
	if existing {
		if foreign { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(ctx, c, req.SessionID); rerr != nil {
				return OperationRecord{}, false, rerr
			}
		}
		return published, true, nil
	}
	if err != nil {
		h.markCorrupt(req.SessionID, err)
		if foreign { // the transaction proved the cached view stale; refresh it before the error returns
			if rerr := h.rematerialize(ctx, c, req.SessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return OperationRecord{}, false, rerr
			}
		}
		return OperationRecord{}, false, err
	}
	c.mu.Lock()
	c.graph.Entries = append(c.graph.Entries, newEntry)
	c.graph.Operations = append(c.graph.Operations, published)
	c.graph.Session = newSession
	c.mu.Unlock()
	return published, false, nil
}

// produceAdmission is the shared in-transaction destination admission
// producer, used by normal admission and Fork alike: it inserts the input
// entry and the running Operation register for one destination Session
// record held at its current revision, and sets the Session current
// Operation. First-writer resolution stays with the caller: an insertion
// conflict returns the raw storage conflict for the caller to resolve —
// the same-Session Operation read for normal admission, the exact
// idempotency lookup for Fork.
func produceAdmission(tx Transaction, session SessionRecord, capture ExecutionCapture, req admissionRequest) (OperationRecord, SessionRecord, graphEntry, error) {
	entryID, err := newHexID()
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	input := inputEntry{
		SessionID:   req.SessionID,
		EntryID:     entryID,
		OperationID: req.OperationID,
		Origin:      req.Origin,
		Content:     req.Content,
	}
	payload, err := encodeInputEntry(input)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	inserted, err := tx.InsertEntry(EntryDraft{
		SessionID:   req.SessionID,
		ID:          entryID,
		OperationID: req.OperationID,
		Kind:        EntryInput,
		Payload:     payload,
	})
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	record := OperationRecord{
		Admission: OperationAdmission{
			SessionID:     req.SessionID,
			OperationID:   req.OperationID,
			RequestKind:   RequestKindMessage,
			AdmittedEntry: EntryRef{SessionID: req.SessionID, EntryID: entryID},
			AgentType:     session.State.CurrentAgentType,
			Execution:     capture,
			AdmittedAt:    inserted.CommittedAt,
		},
		State: OperationCurrentState{
			Status:           OperationRunning,
			StartedAt:        inserted.CommittedAt,
			PendingToolCalls: []PendingToolCall{},
			Usage:            UsageTotals{},
		},
	}
	opPayload, err := encodeOperationRegister(record)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	registered, err := tx.InsertRegister(RegisterDraft{
		Key:     RegisterKey{SessionID: req.SessionID, Kind: RegisterOperation, OperationID: req.OperationID},
		Payload: opPayload,
	})
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	record.Revision = registered.Revision
	state := session.State
	state.CurrentOperationID = req.OperationID
	state.LastActivity = inserted.CommittedAt
	committed := SessionRecord{Identity: session.Identity, State: state}
	sessionPayload, err := encodeSessionRegister(committed)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	replaced, err := tx.ReplaceRegister(RegisterKey{SessionID: req.SessionID, Kind: RegisterSession}, session.Revision, sessionPayload)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	committed.Revision = replaced.Revision
	return record, committed, graphEntry{Envelope: inserted, Input: &input}, nil
}

// rematerialize replaces the coordinator's validated view after a foreign
// writer changed the durable state under it.
func (h *Harness) rematerialize(ctx context.Context, c *coordinator, sessionID string) error {
	graph, err := validateSessionGraph(ctx, h.deps.Storage, sessionID)
	if err != nil {
		h.markCorrupt(sessionID, err)
		return err
	}
	c.mu.Lock()
	c.graph = graph
	c.mu.Unlock()
	return nil
}

// startExecution installs the coordinator's active execution and starts the
// Agent composition on the Harness context after the admission commit. The
// execution slot releases, and the post-terminal buffer drain completes,
// before the execution's done channel closes.
func (h *Harness) startExecution(c *coordinator, operationID string, prepared PreparedExecution) {
	run := &activeExecution{done: make(chan struct{})}
	c.mu.Lock()
	c.run = run
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			if c.run == run { // a buffered drain may have installed the next execution already
				c.run = nil
			}
			c.mu.Unlock()
			close(run.done)
		}()
		err := h.execute(c, operationID, prepared)
		h.recordStorageFailure(err)
		h.drainBuffers(c, run)
	}()
}

// drainBuffers runs the post-terminal buffer drain after the terminal commit:
// undelivered steering first, then queued input, each in FIFO order through
// ordinary admission. Every item receives one scheduled delivery attempt; a
// failed attempt is final for the item and the drain advances to the next
// buffered message. A successful admission starts the next execution, whose
// own terminal re-drains. Harness-context loss discards both buffers without
// delivery attempts. An empty FIFO scan retires this run's slot in the same
// critical section — never a replacement run's — so no window remains where
// the Session looks active without a pending drain.
func (h *Harness) drainBuffers(c *coordinator, run *activeExecution) {
	c.mu.Lock()
	sessionID := c.graph.Session.Identity.SessionID
	c.mu.Unlock()
	release, err := c.reserve(h.ctx)
	if err != nil {
		return
	}
	defer release()
	for {
		c.mu.Lock()
		if h.ctx.Err() != nil { // Harness loss discards both buffers
			c.steering, c.queued = nil, nil
			c.mu.Unlock()
			return
		}
		if c.graph.Session.State.CurrentOperationID != "" { // the drain publishes only after terminal commit
			c.mu.Unlock()
			return
		}
		var item *pendingMessage
		switch {
		case len(c.steering) > 0:
			item, c.steering = c.steering[0], c.steering[1:]
		case len(c.queued) > 0:
			item, c.queued = c.queued[0], c.queued[1:]
		default: // the FIFO scan is empty: retire this run's slot in the same critical section
			if c.run == run { // never clear a replacement run
				c.run = nil
			}
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		rec, prepared, disposition, err := h.admitReserved(h.ctx, c, admissionRequest{
			SessionID:   sessionID,
			OperationID: item.operationID,
			Origin:      item.origin,
			Content:     item.content,
		})
		if err != nil || disposition != DispositionAdmitted || prepared == nil {
			continue // one failed delivery attempt is final for the item; the next proceeds
		}
		h.startExecution(c, rec.Admission.OperationID, *prepared)
		return
	}
}

// recordStorageFailure retains the first storage-class failure that stopped
// admitted work; later failures never replace it.
func (h *Harness) recordStorageFailure(err error) {
	if err == nil || !errors.Is(err, ErrStorage) {
		return
	}
	h.mu.Lock()
	if h.storageFailure == nil {
		h.storageFailure = err
	}
	h.mu.Unlock()
}

// snapshotCoordinators copies the registry's coordinator pointers under h.mu
// and returns them with h.mu released: the caller inspects each pointer under
// its own c.mu, so no lock is ever held across the other. A coordinator
// removed from the registry after the copy is still safe to inspect: its
// reservation and run slots only ever hold toward convergence.
func (h *Harness) snapshotCoordinators() []*coordinator {
	h.mu.Lock()
	cs := make([]*coordinator, 0, len(h.sessions))
	for _, c := range h.sessions {
		cs = append(cs, c)
	}
	h.mu.Unlock()
	return cs
}

// Wait joins the Harness after its context is canceled: it returns after
// cancellation has closed admission and every in-flight preparation,
// execution, and required settlement has converged. Its own context cancels
// only the wait. If admitted work stopped because required storage
// publication failed, it returns the first such recorded storage error after
// all process-local work has ended. Both scans copy the coordinator pointers
// under h.mu, release it, and inspect each under its own c.mu — no h.mu ->
// c.mu nesting, because a transition may hold c.mu while taking h.mu
// (markCorrupt) concurrently.
func (h *Harness) Wait(ctx context.Context) error {
	select {
	case <-h.ctx.Done():
	case <-ctx.Done():
		return ctx.Err()
	}
	for {
		var waits []chan struct{}
		for _, c := range h.snapshotCoordinators() {
			c.mu.Lock()
			if c.reserved != nil {
				waits = append(waits, c.reserved)
			}
			if c.run != nil {
				waits = append(waits, c.run.done)
			}
			c.mu.Unlock()
		}
		for _, done := range waits {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		busy := false // a waited reservation may have published a new execution: re-scan before converging
		for _, c := range h.snapshotCoordinators() {
			c.mu.Lock()
			if c.reserved != nil || c.run != nil {
				busy = true
			}
			c.steering, c.queued = nil, nil // Harness loss discards both buffers on every coordinator
			c.mu.Unlock()
		}
		if !busy {
			h.mu.Lock()
			failure := h.storageFailure
			h.mu.Unlock()
			return failure
		}
	}
}

// readExistingOperation reads one same-Session Operation register once inside
// a transaction, reporting whether it exists.
func readExistingOperation(tx Transaction, sessionID, operationID string) (OperationRecord, bool, error) {
	reg, err := tx.ReadRegister(RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return OperationRecord{}, false, nil
		}
		return OperationRecord{}, false, err
	}
	rec, err := decodeOperationRegister(reg)
	if err != nil {
		return OperationRecord{}, false, corruptSession(sessionID, "operation register %q: %v", operationID, err)
	}
	return rec, true, nil
}

// newHexID returns one durable identifier: 32 lowercase hexadecimal
// characters from 16 cryptographically random bytes.
func newHexID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// ownCapture returns an independent owned copy of one execution capture. A
// zero-length tool list normalizes to nil so no caller backing is retained,
// even with spare capacity.
func ownCapture(c ExecutionCapture) ExecutionCapture {
	out := c
	if len(c.Tools) == 0 {
		out.Tools = nil
		return out
	}
	out.Tools = make([]model.ToolDefinition, len(c.Tools))
	for i, tool := range c.Tools {
		out.Tools[i] = tool
		out.Tools[i].Parameters = model.CloneRaw(tool.Parameters)
	}
	return out
}

// ownUsageTotals returns an independent owned copy of one totals value.
func ownUsageTotals(t UsageTotals) UsageTotals {
	if len(t.ByModel) == 0 {
		return UsageTotals{}
	}
	out := UsageTotals{ByModel: make([]ModelUsage, len(t.ByModel))}
	copy(out.ByModel, t.ByModel)
	return out
}

// ownSessionRecord returns an independent owned copy of one Session record.
func ownSessionRecord(rec SessionRecord) SessionRecord {
	out := rec
	if rec.State.ArchivedAt != nil {
		stamped := *rec.State.ArchivedAt
		out.State.ArchivedAt = &stamped
	}
	out.State.Usage = ownUsageTotals(rec.State.Usage)
	return out
}

// ownOperationRecord returns an independent owned copy of one Operation record.
func ownOperationRecord(rec OperationRecord) OperationRecord {
	out := rec
	out.Admission.Execution = ownCapture(rec.Admission.Execution)
	out.State.Usage = ownUsageTotals(rec.State.Usage)
	out.State.PendingToolCalls = make([]PendingToolCall, len(rec.State.PendingToolCalls))
	copy(out.State.PendingToolCalls, rec.State.PendingToolCalls)
	if rec.State.ActiveEffect != nil {
		effect := *rec.State.ActiveEffect
		out.State.ActiveEffect = &effect
	}
	if rec.State.SettledAt != nil {
		stamped := *rec.State.SettledAt
		out.State.SettledAt = &stamped
	}
	if rec.State.Terminal != nil {
		terminal := *rec.State.Terminal
		out.State.Terminal = &terminal
	}
	return out
}
