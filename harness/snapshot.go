package harness

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/MMinasyan/lightcode/model"
)

// PendingInput is one buffered process-local Submit item: neither a durable
// Operation nor a Session entry while it waits, with no idempotency. The
// content is an owned deep copy.
type PendingInput struct {
	OperationID string
	Origin      InputOrigin
	Content     []model.ContentPart
}

// BackgroundMemberView is one process-local member of a Session's background
// ownership group: the same Kind/ID domain a background_completion signal
// records as its related member. The list disappears on Runtime loss; it is
// informational history, never resolved across Sessions.
type BackgroundMemberView = RelatedMember

// HistoryFact is one validated committed history entry of a Session: the
// immutable envelope identity plus exactly one typed payload member selected
// by Kind. Every payload is the owned validated value decoded from the
// Session's graph; a hook_result entry is execution evidence and never a
// client fact.
type HistoryFact struct {
	EntryID     string
	OperationID string
	Sequence    int64
	CommittedAt time.Time
	Kind        EntryKind
	Input       *InputEntry
	Assistant   *AssistantEntry
	ToolResult  *ToolResultEntry
	Signal      *SignalEntry
	Compaction  *CompactionEntry
	Settlement  *OperationSettlementEntry
}

// SessionHeader is one owned metadata read of one Session: the identity and
// lifecycle state that list rows, command results, and navigation consume,
// plus the revision pair. It copies no usage totals, context state,
// Operations, history facts, or pending FIFO members; the archived-time
// pointee is the only referenced value and is copied.
type SessionHeader struct {
	Identity           SessionIdentity
	Lifecycle          SessionLifecycle
	CurrentAgentType   string
	CurrentOperationID string
	LastActivity       time.Time
	ArchivedAt         *time.Time
	Revision           SessionRevision
}

// OperationSummary is one value-only Operation register summary: the
// canonical admission identity, kind, Agent type, model capture, admission
// time, the current status and settlement time, the terminal detail, and the
// register usage totals. It copies no execution schema, capability list,
// system prompt, or effect machinery — full execution captures stay live
// execution state read through ReadOperation.
type OperationSummary struct {
	OperationID string
	RequestKind RequestKind
	AgentType   string
	Model       model.ModelRef
	AdmittedAt  time.Time
	Status      OperationState
	SettledAt   *time.Time
	Detail      string
	Usage       UsageTotals
}

// HistoryEntryHeader is one committed history entry's value metadata: the
// immutable envelope identity and kind, plus the typed origin of an input
// entry. It carries no decoded payload: callers select by entry identity and
// request exactly the facts they consume.
type HistoryEntryHeader struct {
	EntryID     string
	OperationID string
	Kind        EntryKind
	InputOrigin InputOrigin // meaningful only for an input
}

// HistoryReadView is the narrow history capture of one Session: the revision
// pair, the three scalar availability facts, the immutable entry prefix, and
// the current compaction boundary, all captured under one coordinator-mutex
// hold. It walks and copies no Operations, pending content, background rows,
// or usage totals. The unexported prefix is an appended-only-backed slice
// captured at its current length: later append and graph replacement cannot
// rewrite it, and the caller's reference keeps it alive.
type HistoryReadView struct {
	Revision      SessionRevision
	ExecutionBusy bool
	HasPending    bool
	HasBackground bool

	prefix            []graphEntry
	compactionEntryID string
}

// SessionReadView is the hydration capture of one Session: the shared history
// component plus the groups hydration consumes — the owned metadata header,
// identity-sorted Operation summaries, both pending FIFO contents, background
// membership rows, the register usage totals, and the durably current
// Operation's captured context window — all from one coordinator-mutex hold.
// Every exposed mutable slice and pointee is an owned copy; execution schemas,
// capabilities, and pending/active-effect internals are omitted, and full
// execution state stays behind ReadOperation.
type SessionReadView struct {
	Header              SessionHeader
	History             HistoryReadView
	Operations          []OperationSummary
	Steering            []PendingInput
	Queued              []PendingInput
	Background          []BackgroundMemberView
	TotalUsage          UsageTotals
	ActiveContextWindow int
}

// unavailableLocked reports one coordinator's availability as a typed error:
// nil means available, a gone coordinator maps to the not-found class, and the
// sticky corruption marker maps to its typed corruption error. The caller
// holds c.mu; the marker is read through the registry mutex taken inside that
// hold (the permitted c.mu→h.mu order). Every session read and the observation
// read consume the same check.
func (h *Harness) unavailableLocked(c *coordinator, sessionID string) error {
	if c.gone {
		return notFoundSession(sessionID)
	}
	h.mu.Lock()
	corrupt := c.corru
	h.mu.Unlock()
	return corrupt
}

// withSessionRead runs one private capture callback against one Session: the
// shared acquisition, then the shared captureAvailable admission. The callback
// performs no I/O or plugin work and is not an extension point.
func (h *Harness) withSessionRead(ctx context.Context, sessionID string, capture func(*coordinator) error) error {
	c, release, err := h.acquireCoordinator(ctx, sessionID)
	if err != nil {
		return err
	}
	defer release()
	return h.captureAvailable(c, sessionID, capture)
}

// captureAvailable runs one pure capture under one coordinator-mutex hold. The
// shared unavailable check runs inside that hold, so a deletion or a
// corruption marker that committed after the lookup refuses the capture
// rather than returning state of a Session already decided unavailable.
func (h *Harness) captureAvailable(c *coordinator, sessionID string, capture func(*coordinator) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := h.unavailableLocked(c, sessionID); err != nil {
		return err
	}
	return capture(c)
}

// captureHistoryLocked captures the shared narrow history component of one
// coordinator; the caller holds the coordinator mutex. It copies only scalar
// state and the immutable entry-prefix header, never a payload, Operation,
// FIFO member, membership row, or usage total.
func captureHistoryLocked(c *coordinator) HistoryReadView {
	return HistoryReadView{
		Revision:          sessionRevisionLocked(c),
		ExecutionBusy:     c.graph.Session.State.CurrentOperationID != "" || c.run != nil || c.reserved != nil,
		HasPending:        len(c.steering) > 0 || len(c.queued) > 0,
		HasBackground:     c.group != nil && len(c.group.members) > 0,
		prefix:            c.graph.Entries[:len(c.graph.Entries):len(c.graph.Entries)],
		compactionEntryID: c.graph.Session.State.CompactionEntryID,
	}
}

// captureSessionReadLocked captures the hydration component of one
// coordinator; the caller holds the coordinator mutex. It composes the shared
// history component and clones exactly the groups hydration consumes: every
// exposed mutable slice and pointee is owned.
func captureSessionReadLocked(c *coordinator) SessionReadView {
	view := SessionReadView{
		Header:     sessionHeaderLocked(c),
		History:    captureHistoryLocked(c),
		Steering:   make([]PendingInput, 0, len(c.steering)),
		Queued:     make([]PendingInput, 0, len(c.queued)),
		Background: make([]BackgroundMemberView, 0),
		TotalUsage: ownUsageTotals(c.graph.Session.State.Usage),
	}
	view.Operations = make([]OperationSummary, 0, len(c.graph.Operations))
	current := c.graph.Session.State.CurrentOperationID
	for _, op := range c.graph.Operations {
		if current != "" && op.Admission.OperationID == current {
			view.ActiveContextWindow = op.Admission.Execution.ContextWindow
		}
		view.Operations = append(view.Operations, ownOperationSummary(op))
	}
	// Warm coordinators append admissions, so the returned copy is sorted
	// here; the resident graph keeps its own read order.
	sort.Slice(view.Operations, func(i, j int) bool {
		return view.Operations[i].OperationID < view.Operations[j].OperationID
	})
	for _, item := range c.steering {
		view.Steering = append(view.Steering, PendingInput{
			OperationID: item.operationID,
			Origin:      item.origin,
			Content:     ownContentParts(item.content),
		})
	}
	for _, item := range c.queued {
		view.Queued = append(view.Queued, PendingInput{
			OperationID: item.operationID,
			Origin:      item.origin,
			Content:     ownContentParts(item.content),
		})
	}
	if c.group != nil {
		for _, m := range c.group.members {
			view.Background = append(view.Background, BackgroundMemberView{Kind: string(m.kind), ID: m.id})
		}
		sort.Slice(view.Background, func(i, j int) bool {
			if view.Background[i].Kind != view.Background[j].Kind {
				return view.Background[i].Kind < view.Background[j].Kind
			}
			return view.Background[i].ID < view.Background[j].ID
		})
	}
	return view
}

// ownOperationSummary returns one owned value-only summary of one Operation
// register.
func ownOperationSummary(rec OperationRecord) OperationSummary {
	summary := OperationSummary{
		OperationID: rec.Admission.OperationID,
		RequestKind: rec.Admission.RequestKind,
		AgentType:   rec.Admission.AgentType,
		Model:       rec.Admission.Execution.Model,
		AdmittedAt:  rec.Admission.AdmittedAt,
		Status:      rec.State.Status,
		Usage:       ownUsageTotals(rec.State.Usage),
	}
	if rec.State.SettledAt != nil {
		stamped := *rec.State.SettledAt
		summary.SettledAt = &stamped
	}
	if rec.State.Terminal != nil {
		summary.Detail = rec.State.Terminal.Detail
	}
	return summary
}

// ReadHistoryView materializes one Session and returns its narrow history
// view under one coordinator-mutex hold with the shared unavailable check. It
// copies no Operations, pending content, background rows, or usage totals. A
// deleted Session returns the not-found class, a corrupt Session its typed
// corruption error, and a storage or context failure passes through
// unmodified. The graph it loads is released when the call ends; the returned
// view retains its own entry prefix independently.
func (h *Harness) ReadHistoryView(ctx context.Context, sessionID string) (HistoryReadView, error) {
	var view HistoryReadView
	if err := h.withSessionRead(ctx, sessionID, func(c *coordinator) error {
		view = captureHistoryLocked(c)
		return nil
	}); err != nil {
		return HistoryReadView{}, err
	}
	return view, nil
}

// ReadSessionView materializes one Session and returns the composed
// hydration capture under one coordinator-mutex hold with the shared
// unavailable check. A deleted Session returns the not-found class, a corrupt
// Session its typed corruption error, and a storage or context failure passes
// through unmodified. The graph it loads is released when the call ends; the
// returned view is an owned copy.
func (h *Harness) ReadSessionView(ctx context.Context, sessionID string) (SessionReadView, error) {
	var view SessionReadView
	if err := h.withSessionRead(ctx, sessionID, func(c *coordinator) error {
		view = captureSessionReadLocked(c)
		return nil
	}); err != nil {
		return SessionReadView{}, err
	}
	return view, nil
}

// HistoryHeaders returns the sequence-ordered value metadata of every
// committed history entry that contributes a fact; hook evidence contributes
// none.
func (v HistoryReadView) HistoryHeaders() []HistoryEntryHeader {
	headers := make([]HistoryEntryHeader, 0, len(v.prefix))
	for _, entry := range v.prefix {
		kind, ok := entryPayloadKind(entry)
		if !ok {
			continue
		}
		header := HistoryEntryHeader{
			EntryID:     entry.Envelope.ID,
			OperationID: entry.Envelope.OperationID,
			Kind:        kind,
		}
		if entry.Input != nil {
			header.InputOrigin = entry.Input.Origin
		}
		headers = append(headers, header)
	}
	return headers
}

// HistoryFacts returns the owned facts of the requested entry identities in
// sequence order, deduplicating requests, ignoring identities that name no
// fact, and cloning only matched payloads. The empty result stays non-nil;
// an absent terminal result means a pending call, not an error.
func (v HistoryReadView) HistoryFacts(entryIDs []string) []HistoryFact {
	wanted := make(map[string]bool, len(entryIDs))
	for _, entryID := range entryIDs {
		wanted[entryID] = true
	}
	facts := make([]HistoryFact, 0, len(wanted))
	if len(wanted) == 0 {
		return facts
	}
	for _, entry := range v.prefix {
		if !wanted[entry.Envelope.ID] {
			continue
		}
		if fact, ok := ownHistoryFact(entry); ok {
			facts = append(facts, fact)
		}
	}
	return facts
}

// LatestContextUsage returns the newest owned usage measurement of the
// captured context: scanning backwards from the newest entry, the first
// usage-bearing assistant or settlement entry before the captured current
// compaction boundary. It returns nil when no measurement follows the
// boundary; compaction usage stays totals-only.
func (v HistoryReadView) LatestContextUsage() *UsageCount {
	for i := len(v.prefix) - 1; i >= 0; i-- {
		entry := v.prefix[i]
		if v.compactionEntryID != "" && entry.Envelope.ID == v.compactionEntryID {
			return nil
		}
		var usage *UsageCount
		switch {
		case entry.Assistant != nil:
			usage = entry.Assistant.Usage
		case entry.Settlement != nil:
			usage = entry.Settlement.Usage
		}
		if usage != nil {
			owned := *usage
			return &owned
		}
	}
	return nil
}

// ReadSessionHeader materializes one Session and returns its owned metadata
// header under one coordinator-mutex hold. The revision pair is computed
// directly inside that hold and the shared unavailable check runs there. A
// deleted Session returns the not-found class, a corrupt Session its typed
// corruption error, and a storage or context failure passes through
// unmodified. The graph it loads is released when the call ends.
func (h *Harness) ReadSessionHeader(ctx context.Context, sessionID string) (SessionHeader, error) {
	var header SessionHeader
	if err := h.withSessionRead(ctx, sessionID, func(c *coordinator) error {
		header = sessionHeaderLocked(c)
		return nil
	}); err != nil {
		return SessionHeader{}, err
	}
	return header, nil
}

// ListSessionCandidates returns the durable Session identities the metadata
// controls enumerate: storage's sorted IDs after the shared 32-hex shape
// filter, without loading any Session graph and without building any header.
// They are candidates, not validated headers: a listed identity may still be
// absent or corrupt when a caller validates it.
func (h *Harness) ListSessionCandidates(ctx context.Context) ([]string, error) {
	ids, err := h.deps.Storage.ListSessionIDs(ctx)
	if err != nil {
		return nil, err
	}
	candidates := make([]string, 0, len(ids))
	for _, sessionID := range ids {
		if err := validateHexID(sessionID, "session id"); err != nil {
			continue // a malformed stored identity is never a candidate
		}
		candidates = append(candidates, sessionID)
	}
	return candidates, nil
}

// ListSessions enumerates the durable Session identities through the shared
// candidate enumeration and returns each available Session's owned metadata
// header in the sorted listing order. Every candidate is validated through an
// ordinary use whose graph is released when that candidate's call ends, so
// the listing retains no corpus. A Session that fails validation is
// unavailable: absent from the returned list while valid siblings stay listed,
// with its direct reads returning the typed error. A malformed stored identity
// is never a candidate. Enumeration, storage, and context failures propagate:
// no misleading successful partial list is returned.
func (h *Harness) ListSessions(ctx context.Context) ([]SessionHeader, error) {
	ids, err := h.ListSessionCandidates(ctx)
	if err != nil {
		return nil, err
	}
	headers := make([]SessionHeader, 0, len(ids))
	for _, sessionID := range ids {
		c, release, err := h.acquireCoordinator(ctx, sessionID)
		if err != nil {
			if isCorruption(err) || errors.Is(err, ErrNotFound) { // an unavailable Session stays absent
				continue
			}
			return nil, err
		}
		c.mu.Lock()
		if h.unavailableLocked(c, sessionID) != nil { // deletion or corruption committed between the lookup and the read
			c.mu.Unlock()
			release()
			continue
		}
		headers = append(headers, sessionHeaderLocked(c))
		c.mu.Unlock()
		release()
	}
	return headers, nil
}

// sessionHeaderLocked copies one coordinator's metadata header; the caller
// holds the coordinator mutex.
func sessionHeaderLocked(c *coordinator) SessionHeader {
	record := c.graph.Session
	header := SessionHeader{
		Identity:           record.Identity,
		Lifecycle:          record.State.Lifecycle,
		CurrentAgentType:   record.State.CurrentAgentType,
		CurrentOperationID: record.State.CurrentOperationID,
		LastActivity:       record.State.LastActivity,
		Revision:           sessionRevisionLocked(c),
	}
	if record.State.ArchivedAt != nil {
		stamped := *record.State.ArchivedAt
		header.ArchivedAt = &stamped
	}
	return header
}

// sessionRevisionLocked computes one coordinator's revision pair; the caller
// holds the coordinator mutex.
func sessionRevisionLocked(c *coordinator) SessionRevision {
	return SessionRevision{DurableRevision: c.graph.Session.Revision, LocalRevision: c.localRev}
}

// entryPayloadKind resolves one validated graph entry's payload kind; hook
// evidence and a payload-less entry carry none.
func entryPayloadKind(entry graphEntry) (EntryKind, bool) {
	switch {
	case entry.Input != nil:
		return EntryInput, true
	case entry.Assistant != nil:
		return EntryAssistant, true
	case entry.ToolResult != nil:
		return EntryToolResult, true
	case entry.Signal != nil:
		return EntrySignal, true
	case entry.Compaction != nil:
		return EntryCompaction, true
	case entry.Settlement != nil:
		return EntryOperationSettlement, true
	default:
		return "", false
	}
}

// ownHistoryFact maps one validated graph entry to its owned public fact. A
// hook_result entry contributes no fact.
func ownHistoryFact(entry graphEntry) (HistoryFact, bool) {
	kind, ok := entryPayloadKind(entry)
	if !ok {
		return HistoryFact{}, false
	}
	fact := HistoryFact{
		EntryID:     entry.Envelope.ID,
		OperationID: entry.Envelope.OperationID,
		Sequence:    entry.Envelope.Sequence,
		CommittedAt: entry.Envelope.CommittedAt,
		Kind:        kind,
	}
	switch kind {
	case EntryInput:
		fact.Input = ownInputPayload(*entry.Input)
	case EntryAssistant:
		fact.Assistant = ownAssistantPayload(*entry.Assistant)
	case EntryToolResult:
		fact.ToolResult = ownToolResultPayload(*entry.ToolResult)
	case EntrySignal:
		fact.Signal = ownSignalPayload(*entry.Signal)
	case EntryCompaction:
		fact.Compaction = ownCompactionPayload(*entry.Compaction)
	case EntryOperationSettlement:
		fact.Settlement = ownSettlementPayload(*entry.Settlement)
	}
	return fact, true
}

// ownContentParts returns an independent owned copy of one content list: the
// parts' extras are cloned away from the coordinator's values.
func ownContentParts(parts []model.ContentPart) []model.ContentPart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]model.ContentPart, len(parts))
	for i, part := range parts {
		out[i] = part
		out[i].Extra = part.Extra.Clone()
	}
	return out
}

func ownInputPayload(v inputEntry) *inputEntry {
	out := v
	out.Content = ownContentParts(v.Content)
	return &out
}

func ownAssistantPayload(v assistantEntry) *assistantEntry {
	out := v
	out.Content = ownContentParts(v.Content)
	out.Extra = v.Extra.Clone()
	if len(v.ToolCalls) == 0 {
		out.ToolCalls = nil
	} else {
		out.ToolCalls = make([]toolCallRecord, len(v.ToolCalls))
		for i, call := range v.ToolCalls {
			out.ToolCalls[i] = call
			out.ToolCalls[i].Extra = call.Extra.Clone()
			out.ToolCalls[i].NormalizedArguments = model.CloneRaw(call.NormalizedArguments)
		}
	}
	if v.Usage != nil {
		usage := *v.Usage
		out.Usage = &usage
	}
	return &out
}

func ownToolResultPayload(v toolResultEntry) *toolResultEntry {
	out := v
	out.Metadata = model.CloneRaw(v.Metadata)
	return &out
}

func ownSignalPayload(v signalEntry) *signalEntry {
	out := v
	if v.RelatedOperation != nil {
		ref := *v.RelatedOperation
		out.RelatedOperation = &ref
	}
	if v.RelatedMember != nil {
		member := *v.RelatedMember
		out.RelatedMember = &member
	}
	return &out
}

func ownSettlementPayload(v operationSettlementEntry) *operationSettlementEntry {
	out := v
	if v.Model != nil {
		ref := *v.Model
		out.Model = &ref
	}
	if v.Usage != nil {
		usage := *v.Usage
		out.Usage = &usage
	}
	return &out
}

func ownCompactionPayload(v compactionEntry) *compactionEntry {
	out := v
	if v.Usage != nil {
		usage := *v.Usage
		out.Usage = &usage
	}
	return &out
}
