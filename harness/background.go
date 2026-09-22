package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MMinasyan/lightcode/model"
)

// memberKind classifies one background ownership-group member.
type memberKind string // "child", "job"

const (
	memberChild memberKind = "child"
	memberJob   memberKind = "job"
)

// bgState is the architecture's one group lifecycle.
type bgState string // "open", "stopping", "closed"

const (
	bgOpen     bgState = "open"
	bgStopping bgState = "stopping"
	bgClosed   bgState = "closed"
)

// backgroundMember is one registered member of a Session's background
// ownership group.
type backgroundMember struct {
	kind         memberKind
	id           string        // child session ID or job ID
	completionID string        // reserved, session-unique, first-writer-wins
	claimed      bool          // set by the first-winner claim; a second claim is a no-op
	done         chan struct{} // closed when the member finishes
}

// backgroundGroup is one Session's set of live background members.
type backgroundGroup struct {
	members map[string]*backgroundMember // keyed by completionID; claimed members remain until finished
}

// launchInfo is one child Session's reserved completion identity.
type launchInfo struct {
	completionID string // the child member's reserved completion identity
	outputLimit  int
}

// stopInterval is one stop's join handle: err is written before done closes
// (the workspaceAttempt pattern, runtime/composition.go:673-677).
type stopInterval struct {
	done chan struct{}
	err  error
}

// admitBackgroundMember registers one background member on the Session's
// group. It runs under the caller-held c.mu and rejects a non-open Session
// lifecycle, harness cancellation (the raw error, Submit's entry-gate shape),
// and a group that is stopping or closed; with a positive cap the kind's live
// count must stay below it. The returned member is the caller's handle —
// member.completionID is the completion identity every later step consumes.
func (h *Harness) admitBackgroundMember(c *coordinator, kind memberKind, id string, cap int) (*backgroundMember, error) {
	if c.graph.Session.State.Lifecycle != LifecycleOpen {
		return nil, invalidInput("session %q is archived; admission requires an open Session", c.graph.Session.Identity.SessionID)
	}
	if err := h.ctx.Err(); err != nil {
		return nil, err
	}
	if c.bgState != bgOpen {
		return nil, invalidInput("background group is closed or stopping")
	}
	if c.group == nil {
		c.group = &backgroundGroup{members: map[string]*backgroundMember{}}
	}
	if cap > 0 {
		count := 0
		for _, m := range c.group.members {
			if m.kind == kind {
				count++
			}
		}
		if count >= cap {
			return nil, invalidInput("background group is full (%d/%d). Wait for a background member to complete, then retry", count, cap)
		}
	}
	completionID, err := newHexID()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	m := &backgroundMember{kind: kind, id: id, completionID: completionID, done: make(chan struct{})}
	c.group.members[completionID] = m
	return m, nil
}

// claimBackgroundMember marks one member claimed and returns it; the member
// stays in the map — visible to Stop's snapshot and the archive gate while
// its settlement is in flight. A missing or already-claimed ID returns
// (nil, false). It runs under the caller-held c.mu.
func (h *Harness) claimBackgroundMember(c *coordinator, completionID string) (*backgroundMember, bool) {
	if c.group == nil {
		return nil, false
	}
	m := c.group.members[completionID]
	if m == nil || m.claimed {
		return nil, false
	}
	m.claimed = true
	return m, true
}

// finishBackgroundMember performs the terminal transition: it removes the
// member if still present and closes done. An already-removed member is a
// no-op. Every member-ending path runs claim-then-finish. It takes c.mu
// itself, then — after removal and outside c.mu — runs the recursive
// completion check: a finishing descendant may unblock this session's own
// pending completion.
func (h *Harness) finishBackgroundMember(c *coordinator, m *backgroundMember) {
	c.mu.Lock()
	if c.group != nil {
		if _, ok := c.group.members[m.completionID]; ok {
			delete(c.group.members, m.completionID)
			close(m.done)
		}
	}
	c.mu.Unlock()
	h.childCompletionSettled(c, "")
}

// bgDelivery is the resolved delivery mode of one background completion.
type bgDelivery int

const (
	bgDeliverClosed   bgDelivery = iota // the durable signal entry
	bgDeliverSteering                   // the parent's active Operation consumes it
	bgDeliverIdle                       // normal admission under the completion identity
)

// backgroundDeliveryPriority resolves one completion's delivery mode under
// the caller-held coordinator mutex: a not-receivable parent (harness loss, a
// non-open lifecycle, or a closed group) closes regardless of activity; an
// active Operation steers — the waiting steering item is enqueued here, under
// the same hold; an idle Session admits. Receivability outranks activity.
func backgroundDeliveryPriority(h *Harness, c *coordinator, item *pendingMessage) bgDelivery {
	if h.ctx.Err() != nil || c.graph.Session.State.Lifecycle != LifecycleOpen || c.bgState == bgClosed {
		return bgDeliverClosed
	}
	if c.graph.Session.State.CurrentOperationID != "" || c.run != nil {
		c.steering = append(c.steering, item)
		return bgDeliverSteering
	}
	return bgDeliverIdle
}

// DeliverBackgroundCompletion delivers one background member's completion to
// its owning Session. It resolves the process-local completion obligation
// first — an unknown completion is not new work and never materializes a
// Session — then claims the member first-writer and delivers by the resolved
// mode: a not-receivable parent commits the durable background_completion
// signal entry; a receivable active parent steers the completion into the
// running Operation; a receivable idle parent admits it as an Operation under
// the completion identity. The claimed member finishes only after the
// delivery attempt, so a successful closed record commits before the member
// is marked terminal; a second delivery is a no-op.
func (h *Harness) DeliverBackgroundCompletion(ctx context.Context, sessionID, completionID, content string) error {
	if content == "" {
		return invalidInput("background completion content must be non-empty")
	}
	h.mu.Lock()
	cached := h.sessions[sessionID]
	h.mu.Unlock()
	if cached == nil { // registration necessarily cached the member's owning coordinator
		return nil
	}
	cached.mu.Lock()
	member, ok := h.claimBackgroundMember(cached, completionID)
	cached.mu.Unlock()
	if !ok {
		return nil
	}
	err := h.deliverBackgroundCompletion(ctx, cached, member, content)
	h.finishBackgroundMember(cached, member)
	if err != nil {
		h.recordStorageFailure(err)
	}
	return err
}

// deliverBackgroundCompletion runs one claimed completion's validated
// delivery attempt. Normal coordinator validation precedes any graph
// interpretation: an unavailable parent returns its ordinary error with no
// durable work, and the claimed member still finishes. The claimed member
// remains in the group through the whole attempt, so recursive readiness and
// Stop cannot treat it as finished before delivery has resolved.
func (h *Harness) deliverBackgroundCompletion(ctx context.Context, c *coordinator, member *backgroundMember, content string) error {
	c.mu.Lock()
	sessionID := c.graph.Session.Identity.SessionID
	c.mu.Unlock()
	validated, err := h.coordinatorFor(ctx, sessionID)
	if err != nil {
		return err
	}
	part, err := model.NewContentPart(model.ContentPart{Kind: model.PartText, Text: content})
	if err != nil {
		return invalidInput("background completion content: %v", err)
	}
	parts := []model.ContentPart{part}
	item := &pendingMessage{operationID: member.completionID, origin: InputOriginRuntime, content: parts}

	validated.mu.Lock()
	mode := backgroundDeliveryPriority(h, validated, item)
	validated.mu.Unlock()

	switch mode {
	case bgDeliverSteering:
		return nil // enqueued at evaluation; the active Operation's boundary or drain delivers it
	case bgDeliverIdle:
		return h.deliverIdleCompletion(validated, member, parts, content)
	default:
		return h.deliverClosedCompletion(validated, member, content)
	}
}

// deliverIdleCompletion admits one completion through the idle path: it takes
// the Session's admission reservation, re-evaluates the same priority under
// it — a run installed while the reservation waited re-routes to steering,
// lost receivability to the closed path — and admits through the held
// reservation, installing the execution before releasing it. admit is not
// used because it would reserve recursively. A reserve failure resolves to
// the closed path; an admission failure is final: no retry and no fallback
// signal write.
func (h *Harness) deliverIdleCompletion(c *coordinator, member *backgroundMember, parts []model.ContentPart, content string) error {
	release, err := c.reserve(h.ctx)
	if err != nil { // the reservation failed under harness loss: re-evaluated receivability is the closed path
		return h.deliverClosedCompletion(c, member, content)
	}
	defer release()
	c.mu.Lock()
	sessionID := c.graph.Session.Identity.SessionID
	mode := backgroundDeliveryPriority(h, c, &pendingMessage{operationID: member.completionID, origin: InputOriginRuntime, content: parts})
	c.mu.Unlock()
	switch mode {
	case bgDeliverClosed:
		return h.deliverClosedCompletion(c, member, content)
	case bgDeliverSteering:
		return nil // enqueued at the re-evaluation
	}
	rec, prepared, _, err := h.admitReserved(h.ctx, c, admissionRequest{
		SessionID:   sessionID,
		OperationID: member.completionID,
		Origin:      InputOriginRuntime,
		Content:     parts,
	})
	if err != nil {
		return err
	}
	if prepared != nil { // install the execution before the deferred release runs
		h.startExecution(c, rec.Admission.OperationID, *prepared)
	}
	return nil
}

// deliverClosedCompletion commits one operationless background_completion
// signal entry through the compare/commit/adopt/rematerialize pattern, with
// the coordinator mutex held across the whole transaction: it re-reads and
// decodes the parent register, compares its revision with the cached view —
// decode failure is corruption and drift the revision-race conflict — inserts
// the entry with related_member{kind, id} and the producer-bounded content,
// and replaces the Session register with its unchanged payload and advanced
// revision. On transaction error the filtered corruption marker runs; on
// revision drift the view rematerializes once on the settlement context,
// whose failure replaces the original error. Delivery is never retried.
func (h *Harness) deliverClosedCompletion(c *coordinator, member *backgroundMember, content string) error {
	c.mu.Lock()
	sessionID := c.graph.Session.Identity.SessionID
	view := c.graph.Session
	entryID, err := newHexID()
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrStorage, err)
	}
	entry := signalEntry{
		SessionID:     sessionID,
		EntryID:       entryID,
		Signal:        SignalBackgroundCompletion,
		RelatedMember: &relatedMember{Kind: string(member.kind), ID: member.id},
		Content:       content,
	}
	payload, err := encodeSignalEntry(entry)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	settleCtx := context.WithoutCancel(h.ctx)
	var (
		committedSession SessionRecord
		adopted          graphEntry
	)
	err = h.deps.Storage.Transact(settleCtx, func(tx Transaction) error {
		key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		current, err := decodeSessionRegister(reg)
		if err != nil {
			return corruptSession(sessionID, "session register: %v", err)
		}
		if reg.Revision != view.Revision {
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, view.Revision, reg.Revision)
		}
		adopted, err = insertAndAdopt(tx, sessionID, EntryDraft{
			SessionID: sessionID,
			ID:        entryID,
			Kind:      EntrySignal,
			Payload:   payload,
		})
		if err != nil {
			return err
		}
		committedSession = SessionRecord{Identity: current.Identity, State: current.State}
		sessionPayload, err := encodeSessionRegister(committedSession)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(key, reg.Revision, sessionPayload)
		if err != nil {
			return err
		}
		committedSession.Revision = replaced.Revision
		return nil
	})
	if err != nil {
		h.markCorrupt(sessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(settleCtx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return rerr
			}
		}
		return err
	}
	c.graph.Entries = append(c.graph.Entries, adopted)
	c.graph.Session = committedSession
	c.mu.Unlock()
	return nil
}

// childCompletionSettled is the one settlement check behind both readiness
// triggers — a member finish and an execution's deferred retirement. It
// returns immediately when the child carries no pending completion; otherwise
// it validates the child's coordinator, re-checks the pending obligation
// under the lock (another trigger may have taken it), and either abandons the
// obligation — a validation failure, or a non-empty settled operation
// identity still current because that execution's terminal commit failed —
// or defers while the child is not terminal, retired, and quiet: a non-empty
// current Operation, an installed run, or a non-empty group. When settled, it
// takes the pending info and renders the parent-facing completion text; every
// parent-side call happens after releasing the child lock.
func (h *Harness) childCompletionSettled(c *coordinator, settledOperationID string) {
	c.mu.Lock()
	if c.pendingCompletion == nil {
		c.mu.Unlock()
		return
	}
	childID := c.graph.Session.Identity.SessionID
	parentID := c.graph.Session.Identity.ParentSessionID
	c.mu.Unlock()

	if _, err := h.coordinatorFor(h.ctx, childID); err != nil {
		// validation failure takes the pending obligation for abandonment
		c.mu.Lock()
		info := c.pendingCompletion
		c.pendingCompletion = nil
		c.mu.Unlock()
		if info != nil {
			h.abandonChildCompletion(parentID, info.completionID)
		}
		return
	}

	c.mu.Lock()
	info := c.pendingCompletion
	if info == nil { // another trigger took the obligation
		c.mu.Unlock()
		return
	}
	if settledOperationID != "" && settledOperationID == c.graph.Session.State.CurrentOperationID {
		// that execution's terminal commit failed: the durable Operation stays
		// running for recovery and the completion obligation is abandoned
		c.pendingCompletion = nil
		c.mu.Unlock()
		h.abandonChildCompletion(parentID, info.completionID)
		return
	}
	if c.graph.Session.State.CurrentOperationID != "" || c.run != nil || (c.group != nil && len(c.group.members) > 0) {
		c.mu.Unlock() // not yet terminal, retired and quiet: the obligation waits
		return
	}
	c.pendingCompletion = nil
	content := childCompletionContent(c.graph, info.outputLimit)
	c.mu.Unlock()
	h.DeliverBackgroundCompletion(h.ctx, parentID, info.completionID, content)
}

// abandonChildCompletion releases one pending completion's process-local
// ownership without delivery: it looks up the parent's cached registry
// member and, on a resolved coordinator, claim-finishes the completion —
// no corruption validation is bypassed and no compensating write is made;
// durable running child Operations remain for recovery.
func (h *Harness) abandonChildCompletion(parentID, completionID string) {
	h.mu.Lock()
	parent := h.sessions[parentID]
	h.mu.Unlock()
	if parent == nil {
		return
	}
	h.claimFinishBackgroundMember(parent, completionID)
}

// childCompletionContent renders one settled child's parent-facing completion
// text from its latest committed Operation settlement by entry sequence: the
// settlement entry's status and detail — matching that Operation's terminal
// register — and, on success, the last assistant entry belonging to that
// Operation with its text parts joined in stored order. Earlier assistant
// turns, tool results, and previous Operations never contribute. Success with
// no assistant text renders the stated empty-success fallback; interruption
// and failure render their fixed texts, failure with its detail. The rendered
// text is capped at the completion's output limit in UTF-8 bytes. A terminal,
// retired and quiet child with a pending completion always owns a settlement
// entry — a pending completion implies a launched child, an empty current
// Operation leaves every Operation terminal, and each terminal Operation owns
// exactly one settlement — so an absent settlement is unreachable for a
// validated graph and renders as empty content, which the delivery's
// non-empty-content guard rejects uniformly.
func childCompletionContent(g *sessionGraph, outputLimit int) string {
	var settlement *operationSettlementEntry
	for i := range g.Entries {
		if g.Entries[i].Settlement != nil {
			settlement = g.Entries[i].Settlement
		}
	}
	if settlement == nil {
		return "" // unreachable for a validated graph
	}
	var text string
	switch settlement.Status {
	case OperationSuccess:
		var assistant *assistantEntry
		for i := range g.Entries {
			e := &g.Entries[i]
			if e.Assistant != nil && e.Envelope.OperationID == settlement.OperationID {
				assistant = e.Assistant
			}
		}
		if assistant != nil {
			for _, part := range assistant.Content {
				text += part.Text
			}
		}
		if text == "" {
			text = "Task completed."
		}
	case OperationInterruption:
		text = "Task interrupted."
	default:
		text = "Task failed: " + settlement.Detail
	}
	return truncateCompletionText(text, outputLimit)
}

// truncateCompletionText caps one rendered completion at the output limit in
// UTF-8 bytes, retaining the longest complete-character prefix that fits the
// trailing marker.
func truncateCompletionText(text string, limit int) string {
	const marker = "\n[truncated]"
	if limit <= 0 || len(text) <= limit {
		return text
	}
	max := limit - len(marker)
	if max < 0 {
		max = 0
	}
	for max > 0 && text[max]&0xC0 == 0x80 { // back off to a complete-character boundary
		max--
	}
	return text[:max] + marker
}

// backgroundHandoffFailure re-checks one starter's handoff set under the
// coordinator mutex: the exact member must remain present, the group must be
// open, and the Harness context alive. A nil return admits the start; every
// state outside the valid set fails uniformly — the raw Harness error when
// the Harness is closed, the closed-or-stopping rejection otherwise. No new
// sentinel or model-visible error class exists for a lost handoff.
func (h *Harness) backgroundHandoffFailure(c *coordinator, member *backgroundMember) error {
	c.mu.Lock()
	present := c.group != nil && c.group.members[member.completionID] == member
	open := c.bgState == bgOpen
	c.mu.Unlock()
	if present && open && h.ctx.Err() == nil {
		return nil
	}
	if err := h.ctx.Err(); err != nil {
		return err
	}
	return invalidInput("background group is closed or stopping")
}

// claimFinishBackgroundMember releases one member without delivery on a
// resolved coordinator: the first-writer claim runs under the coordinator
// mutex and the finish follows only when the claim succeeded — a second
// ending is a no-op.
func (h *Harness) claimFinishBackgroundMember(c *coordinator, completionID string) {
	c.mu.Lock()
	m, ok := h.claimBackgroundMember(c, completionID)
	c.mu.Unlock()
	if ok {
		h.finishBackgroundMember(c, m)
	}
}

// StartJob starts one background job on one Session's ownership group: it
// validates the job identity, the required job-stop dependency, and the spawn
// callback before resolution, admits a job member (the Jobs capability owns
// the jobs limit, so no group cap applies), performs the common handoff, and
// hands the reserved completion identity to the spawn outside locks. The
// spawn owns the concrete start: an error means it did not transfer a live
// completion obligation — the member claim-finishes and the error returns
// unchanged; success means it arranged terminal delivery through
// DeliverBackgroundCompletion using the supplied identity.
func (h *Harness) StartJob(ctx context.Context, sessionID, jobID string, spawn func(ctx context.Context, completionID string) error) error {
	if err := validateJobID(jobID, "job id"); err != nil {
		return invalidInput("job id: %v", err)
	}
	if h.deps.Jobs == nil {
		return invalidInput("no job stopper configured")
	}
	if spawn == nil {
		return invalidInput("job start requires a non-nil spawn callback")
	}
	c, err := h.coordinatorFor(ctx, sessionID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	member, err := h.admitBackgroundMember(c, memberJob, jobID, 0)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if err := h.backgroundHandoffFailure(c, member); err != nil {
		h.claimFinishBackgroundMember(c, member.completionID)
		return err
	}
	if err := spawn(ctx, member.completionID); err != nil {
		h.claimFinishBackgroundMember(c, member.completionID)
		return err
	}
	return nil
}

// LaunchChildRequest is the input of one child launch: the open root parent
// Session, the requested Agent type and message, the caller-generated
// globally unique Operation identity, the parent's child concurrency cap, and
// the completion output limit in UTF-8 bytes.
type LaunchChildRequest struct {
	ParentSessionID string
	AgentType       string
	Content         []model.ContentPart
	OperationID     string // caller-generated, globally unique
	MaxConcurrent   int    // 1..20
	OutputLimit     int    // 1024..1048576 UTF-8 bytes
}

// LaunchChildResult is the outcome of one child launch: the durable child
// Session identity whose execution has started under the reserved completion.
type LaunchChildResult struct {
	ChildSessionID string
}

// LaunchChildSession launches one child Session as an ordinary Session
// through the shared admission path: it validates the request, refuses child
// parents, prepares the child through the one preparation callback with the
// complete child identity, admits the child member under the requested cap,
// publishes child and admission in one transaction against the re-read
// parent register, performs the common handoff, materializes the child,
// installs the pending completion under the reserved identity, and starts the
// child execution. A rejected handoff or failed start settles its member — a
// committed child first attempts its durable Operation's terminal
// interruption, a materialization or settlement failure leaves the durable
// running Operation for recovery — and the launch never retries nor publishes
// a partial child.
func (h *Harness) LaunchChildSession(ctx context.Context, req LaunchChildRequest) (LaunchChildResult, error) {
	if req.MaxConcurrent < 1 || req.MaxConcurrent > 20 {
		return LaunchChildResult{}, invalidInput("max concurrent %d is outside 1..20", req.MaxConcurrent)
	}
	if req.OutputLimit < 1024 || req.OutputLimit > 1048576 {
		return LaunchChildResult{}, invalidInput("output limit %d is outside 1024..1048576 UTF-8 bytes", req.OutputLimit)
	}
	if err := validateHexID(req.ParentSessionID, "parent session id"); err != nil {
		return LaunchChildResult{}, invalidInput("parent session id: %v", err)
	}
	content, err := validateSubmitInput(req.OperationID, InputOriginPlugin, req.Content)
	if err != nil {
		return LaunchChildResult{}, err
	}
	c, err := h.coordinatorFor(ctx, req.ParentSessionID)
	if err != nil {
		return LaunchChildResult{}, err
	}
	c.mu.Lock()
	if c.graph.Session.Identity.ParentSessionID != "" { // the retained recursion rule: no depth counters
		c.mu.Unlock()
		return LaunchChildResult{}, invalidInput("child sessions cannot launch child sessions")
	}
	view := c.graph.Session // the state the launch transaction re-reads
	c.mu.Unlock()

	childID, err := newHexID()
	if err != nil {
		return LaunchChildResult{}, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	now := time.Now().UTC() // one sampled creation time for created_at and last_activity
	childIdentity := SessionIdentity{
		SessionID:       childID,
		Workspace:       view.Identity.Workspace,
		CreatedAt:       now,
		ParentSessionID: req.ParentSessionID,
	}
	prepared, capture, prepCtx, cleanup, err := h.prepareExecution(ctx, PreparationSession{
		Identity:  childIdentity,
		AgentType: req.AgentType,
	})
	if err != nil {
		return LaunchChildResult{}, err
	}
	defer cleanup() // the combined context stays live until publication returns

	c.mu.Lock()
	member, err := h.admitBackgroundMember(c, memberChild, childID, req.MaxConcurrent)
	c.mu.Unlock()
	if err != nil {
		return LaunchChildResult{}, err
	}

	child := SessionRecord{
		Identity: childIdentity,
		State: SessionState{
			Lifecycle:        LifecycleOpen,
			CurrentAgentType: req.AgentType,
			Usage:            UsageTotals{},
			LastActivity:     now,
		},
	}
	err = h.deps.Storage.Transact(prepCtx, func(tx Transaction) error {
		return h.launchTransaction(tx, view, req, child, capture, content)
	})
	if err != nil {
		h.markCorrupt(req.ParentSessionID, err)
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(prepCtx, c, req.ParentSessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				h.claimFinishBackgroundMember(c, member.completionID)
				return LaunchChildResult{}, rerr
			}
		}
		h.claimFinishBackgroundMember(c, member.completionID)
		return LaunchChildResult{}, publicationError(ctx, err)
	}

	if err := h.backgroundHandoffFailure(c, member); err != nil {
		return LaunchChildResult{}, h.settleRejectedChildLaunch(ctx, c, member, childID, req.OperationID, err)
	}
	childCoordinator, err := h.coordinatorFor(ctx, childID)
	if err != nil {
		// the durable running Operation stays for recovery; no delivery
		h.recordStorageFailure(err)
		h.claimFinishBackgroundMember(c, member.completionID)
		return LaunchChildResult{}, err
	}
	childCoordinator.mu.Lock()
	childCoordinator.pendingCompletion = &launchInfo{completionID: member.completionID, outputLimit: req.OutputLimit}
	childCoordinator.mu.Unlock()
	h.startExecution(childCoordinator, req.OperationID, prepared)
	return LaunchChildResult{ChildSessionID: childID}, nil
}

// launchTransaction is the launch's one transaction: it re-reads and decodes
// the parent register and rejects revision and lifecycle drift, inserts the
// prepared child Session record — its identity already carries the parent
// lineage and both fork-lineage fields empty — and runs the shared
// in-transaction admission producer with the plugin origin, which sets the
// child's current Operation and admission-commit last activity in the same
// transaction. The committed child state is not carried out: the post-commit
// materialization reads the durable truth through the graph validator.
func (h *Harness) launchTransaction(tx Transaction, view SessionRecord, req LaunchChildRequest, child SessionRecord, capture ExecutionCapture, content []model.ContentPart) error {
	key := RegisterKey{SessionID: req.ParentSessionID, Kind: RegisterSession}
	reg, err := tx.ReadRegister(key)
	if err != nil {
		return err
	}
	current, err := decodeSessionRegister(reg)
	if err != nil {
		return corruptSession(req.ParentSessionID, "session register: %v", err)
	}
	if reg.Revision != view.Revision {
		return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, req.ParentSessionID, view.Revision, reg.Revision)
	}
	if current.State.Lifecycle != LifecycleOpen {
		return invalidInput("session %q is archived; admission requires an open Session", req.ParentSessionID)
	}
	childPayload, err := encodeSessionRegister(child)
	if err != nil {
		return err
	}
	childReg, err := tx.InsertRegister(RegisterDraft{Key: RegisterKey{SessionID: child.Identity.SessionID, Kind: RegisterSession}, Payload: childPayload})
	if err != nil {
		return err
	}
	child.Revision = childReg.Revision // the producer commits against the inserted revision
	_, _, _, perr := produceAdmission(tx, child, capture, admissionRequest{
		SessionID:   child.Identity.SessionID,
		OperationID: req.OperationID,
		Origin:      InputOriginPlugin,
		Content:     content,
	})
	if perr != nil {
		return perr
	}
	return nil
}

// settleRejectedChildLaunch resolves one committed child whose handoff was
// rejected: it first attempts the child Operation's terminal interruption
// through its coordinator — a materialization or settlement failure leaves
// the durable running Operation for recovery, latches the storage class
// through recordStorageFailure, and claim-finishes the parent member without
// delivery — and otherwise returns the handoff failure after the
// interruption and the finish.
func (h *Harness) settleRejectedChildLaunch(ctx context.Context, c *coordinator, member *backgroundMember, childID, operationID string, handoffErr error) error {
	child, err := h.coordinatorFor(ctx, childID)
	if err == nil {
		settleCtx := context.WithoutCancel(h.ctx)
		err = recoverRunningOperation(settleCtx, h.deps.Storage, childID, operationID)
		if err == nil { // the cached child view predates the settlement commit
			err = h.rematerialize(settleCtx, child, childID)
		}
	}
	if err != nil {
		h.recordStorageFailure(err)
		h.claimFinishBackgroundMember(c, member.completionID)
		return err
	}
	h.claimFinishBackgroundMember(c, member.completionID)
	return handoffErr
}
