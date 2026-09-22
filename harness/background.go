package harness

import (
	"context"
	"errors"
	"fmt"

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
// member, claims it under the parent's coordinator mutex, and finishes it
// only if the claim succeeded. No corruption validation is bypassed and no
// compensating write is made; durable running child Operations remain for
// recovery.
func (h *Harness) abandonChildCompletion(parentID, completionID string) {
	h.mu.Lock()
	parent := h.sessions[parentID]
	h.mu.Unlock()
	if parent == nil {
		return
	}
	parent.mu.Lock()
	m, ok := h.claimBackgroundMember(parent, completionID)
	parent.mu.Unlock()
	if !ok {
		return
	}
	h.finishBackgroundMember(parent, m)
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
