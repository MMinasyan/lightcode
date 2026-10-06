package harness

import "github.com/MMinasyan/lightcode/model"

// HarnessFactKind is the closed kind of one passive HarnessFact. It is exactly
// the set of SSE-originating progress and invalidation facts; no other kind is
// added by a plugin, an adapter, or a caller.
type HarnessFactKind string

const (
	// FactInvalidation reports that one Session's durable register revision or
	// its coordinator-local revision advanced. It carries the Session identity
	// only; a job member admission or finish additionally names the Job. The
	// passive publisher samples the current pair at publication time.
	FactInvalidation HarnessFactKind = "invalidation"
	// FactTextDelta reports one nonempty transient text fragment of an accepted
	// model stream. It carries no authority: the committed assistant entry is
	// the authoritative value.
	FactTextDelta HarnessFactKind = "text_delta"
	// FactRefusalDelta reports one nonempty transient refusal fragment of an
	// accepted model stream. It carries no position — refusal fragments
	// concatenate in arrival order — and no authority: the committed
	// assistant entry's refusal is the authoritative value.
	FactRefusalDelta HarnessFactKind = "refusal_delta"
	// FactToolStarted reports one dispatched tool call. It is emitted once at
	// the live dispatcher entry, before any branch-specific validation, hook,
	// permission decision, or execution.
	FactToolStarted HarnessFactKind = "tool_started"
	// FactToolFinished reports one committed tool result, including the
	// synthetic interrupted result a terminal settlement writes for a call
	// that was never dispatched.
	FactToolFinished HarnessFactKind = "tool_finished"
)

// SessionRevision is the value identity of one Session's revision pair: the
// durable Session register revision and the coordinator-local publication
// counter. It carries no instance identity and no codec; the wire adds the
// owning Runtime instance separately.
type SessionRevision struct {
	DurableRevision int64
	LocalRevision   uint64
}

// HarnessFact is one passive observation of the Harness: a closed union whose
// members are exactly the fields the fact's Kind requires. An invalidation
// carries SessionID and the optional JobID of a job member admission or
// finish; a text delta carries SessionID, OperationID, Position, and the
// nonempty Content; a refusal delta carries SessionID, OperationID, and the
// nonempty Content and no Position; a tool start carries SessionID,
// OperationID, CallID, Ordinal, and Name; a tool finish carries SessionID,
// OperationID, CallID, and the closed Status. Every field a kind does not
// require stays zero. A fact grants no authority: it can never alter an
// admission, an effect, a settlement, or a lifecycle transition. A fact
// carries no revision pair: no producer sample can become notification
// authority; passive publishers read the current pair at publication time.
type HarnessFact struct {
	Kind        HarnessFactKind
	SessionID   string
	OperationID string
	JobID       string
	Position    int
	Content     string
	CallID      string
	Ordinal     int64
	Name        string
	Status      model.ToolResultStatus
}

// emitFact delivers one passive fact to the optional observer. A nil observer
// is a no-op, delivery is synchronous, and a panicking observer is contained:
// passive observation never changes settlement and is never retried.
func (h *Harness) emitFact(fact HarnessFact) {
	if h.deps.Observe == nil {
		return
	}
	defer func() { _ = recover() }() // the callback alone is contained; no logging, retry, or error return
	h.deps.Observe(fact)
}

// readObservation samples one coordinator's current identity and revision
// pair under the coordinator mutex, using the shared unavailable check (gone
// and the sticky corruption marker, the latter read through the registry
// mutex taken inside that hold — the permitted c.mu→h.mu order). A deleted or
// corrupt coordinator reports false. No snapshot or history copy is built to
// read the pair; the identity is copied by value from the cached graph.
func (h *Harness) readObservation(c *coordinator) (SessionIdentity, SessionRevision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h.unavailableLocked(c, c.graph.Session.Identity.SessionID) != nil {
		return SessionIdentity{}, SessionRevision{}, false
	}
	return c.graph.Session.Identity,
		SessionRevision{DurableRevision: c.graph.Session.Revision, LocalRevision: c.localRev},
		true
}

// currentSubject samples one coordinator's stored identity after a producer
// released its state lock, delegating to the shared cached availability
// check. A deleted or corrupt coordinator suppresses a delayed stale hint.
// No snapshot, history, or revision copy is built.
func (h *Harness) currentSubject(c *coordinator) (string, bool) {
	identity, _, ok := h.readObservation(c)
	if !ok {
		return "", false
	}
	return identity.SessionID, true
}

// ReadObservation reads one Session's current identity and revision pair
// from the cached coordinator only: the registry is consulted under the
// registry mutex and released, and the sample then follows the same
// coordinator validity rule as the passive facts. A cache miss, a deleted, or
// a corrupt Session reports false; no materialization, storage read, or
// registry growth happens. It is the passive publishers' cached identity
// read, never an authoritative snapshot.
func (h *Harness) ReadObservation(sessionID string) (SessionIdentity, SessionRevision, bool) {
	h.mu.Lock()
	c := h.sessions[sessionID]
	h.mu.Unlock()
	if c == nil {
		return SessionIdentity{}, SessionRevision{}, false
	}
	return h.readObservation(c)
}

// observeInvalidation emits one Session-scoped invalidation. It is the one
// emission every durable Session advance and every coordinator-local
// publication calls after releasing its lock and transaction.
func (h *Harness) observeInvalidation(c *coordinator) {
	if h.deps.Observe == nil {
		return
	}
	sessionID, ok := h.currentSubject(c)
	if !ok {
		return
	}
	h.emitFact(HarnessFact{Kind: FactInvalidation, SessionID: sessionID})
}

// observeMemberInvalidation emits one Job-scoped invalidation for a job
// member's admission or finish: the owning Session identity and the Job
// identity. Child members and group transitions use observeInvalidation and
// never name a Job.
func (h *Harness) observeMemberInvalidation(c *coordinator, jobID string) {
	if h.deps.Observe == nil {
		return
	}
	sessionID, ok := h.currentSubject(c)
	if !ok {
		return
	}
	h.emitFact(HarnessFact{Kind: FactInvalidation, SessionID: sessionID, JobID: jobID})
}

// emitToolResultFacts delivers one finished fact per committed tool-result
// entry, derived from the adopted entries rather than a started registry: a
// live dispatched result and a terminal settlement's synthetic unstarted
// result both finish through this one path.
func (h *Harness) emitToolResultFacts(entries []graphEntry, sessionID, operationID string) {
	for _, entry := range entries {
		if entry.ToolResult == nil {
			continue
		}
		h.emitFact(HarnessFact{
			Kind:        FactToolFinished,
			SessionID:   sessionID,
			OperationID: operationID,
			CallID:      entry.ToolResult.ToolCallID,
			Status:      entry.ToolResult.Status,
		})
	}
}

// observeStream wraps one accepted model stream for passive progress
// observation; without an observer the stream passes through untouched. The
// wrapper is the single accepted→assemble handoff for both the conversation
// and the compact model transport.
func (h *Harness) observeStream(sessionID, operationID string, stream model.Stream) model.Stream {
	if h.deps.Observe == nil {
		return stream
	}
	return observedStream{inner: stream, emit: func(kind HarnessFactKind, position int, content string) {
		h.emitFact(HarnessFact{
			Kind:        kind,
			SessionID:   sessionID,
			OperationID: operationID,
			Position:    position,
			Content:     content,
		})
	}}
}

// observedStream is the one accepted-stream wrapper: Recv delegates unchanged,
// Close delegates exactly once, and only a successfully parsed choice-bearing
// delta contributes one fact per nonempty fragment. An empty, non-text, or
// invalid delta emits nothing, and a delta returned together with an error
// (EOF included) emits nothing; the original delta and error are always
// returned exactly as received — the validation gate reads the same contract
// the constructor enforces without building an owned copy. A delta's
// positioned content-text fragments are emitted before its refusal fragment,
// and refusal emits no position. No reasoning, tool, or finish fragment is
// interpreted, and no second assembler or accumulator exists.
type observedStream struct {
	inner model.Stream
	emit  func(kind HarnessFactKind, position int, content string)
}

func (s observedStream) Recv() (model.StreamDelta, error) {
	delta, err := s.inner.Recv()
	if err != nil || !delta.HasChoice {
		return delta, err
	}
	if verr := model.ValidateStreamDelta(delta); verr != nil { // validation gate only; the original delta is returned unchanged
		return delta, err
	}
	for _, fragment := range delta.ContentFragments {
		if fragment.Kind == model.PartText && fragment.Text != "" {
			s.emit(FactTextDelta, fragment.Position, fragment.Text)
		}
	}
	if delta.RefusalFragment != "" {
		s.emit(FactRefusalDelta, 0, delta.RefusalFragment)
	}
	return delta, err
}

func (s observedStream) Close() error { return s.inner.Close() }
