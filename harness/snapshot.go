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

// SessionSnapshot is one coherent owned read of one Session's complete
// validated state: the durable Session record, every Operation record sorted
// lexicographically by Operation identity, the committed history facts in
// ascending sequence, both pending FIFOs in FIFO order, the process-local
// background membership sorted by kind then identity, the read-only busy
// fact, and the coordinator-local revision. Every slice, content value, and
// JSON value is an owned deep copy: caller mutation cannot reach the
// coordinator or another returned snapshot. All fields come from one
// coordinator publication.
type SessionSnapshot struct {
	Session       SessionRecord
	Operations    []OperationRecord
	Facts         []HistoryFact
	Steering      []PendingInput
	Queued        []PendingInput
	Background    []BackgroundMemberView
	ExecutionBusy bool
	LocalRevision uint64
}

// SnapshotSession materializes one Session's validated graph and returns its
// complete owned snapshot under one coordinator-mutex hold. A cold Session
// validates once and installs its coordinator; a warm valid Session reads its
// cached graph with no history re-read. A deleted Session returns the
// not-found class, a corrupt Session its typed corruption error, and a
// storage or context failure passes through unmodified; no partial snapshot
// is ever returned.
func (h *Harness) SnapshotSession(ctx context.Context, sessionID string) (SessionSnapshot, error) {
	c, err := h.coordinatorFor(ctx, sessionID)
	if err != nil {
		return SessionSnapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return SessionSnapshot{}, notFoundSession(sessionID)
	}
	snap := SessionSnapshot{
		Session:       ownSessionRecord(c.graph.Session),
		Operations:    make([]OperationRecord, 0, len(c.graph.Operations)),
		Facts:         make([]HistoryFact, 0, len(c.graph.Entries)),
		Steering:      make([]PendingInput, 0, len(c.steering)),
		Queued:        make([]PendingInput, 0, len(c.queued)),
		Background:    make([]BackgroundMemberView, 0),
		ExecutionBusy: c.graph.Session.State.CurrentOperationID != "" || c.run != nil || c.reserved != nil,
		LocalRevision: c.localRev,
	}
	for _, op := range c.graph.Operations {
		snap.Operations = append(snap.Operations, ownOperationRecord(op))
	}
	// Warm coordinators append admissions, so the returned copy is sorted
	// here; the cached graph keeps its own read order.
	sort.Slice(snap.Operations, func(i, j int) bool {
		return snap.Operations[i].Admission.OperationID < snap.Operations[j].Admission.OperationID
	})
	for _, entry := range c.graph.Entries {
		if fact, ok := ownHistoryFact(entry); ok {
			snap.Facts = append(snap.Facts, fact)
		}
	}
	for _, item := range c.steering {
		snap.Steering = append(snap.Steering, PendingInput{
			OperationID: item.operationID,
			Origin:      item.origin,
			Content:     ownContentParts(item.content),
		})
	}
	for _, item := range c.queued {
		snap.Queued = append(snap.Queued, PendingInput{
			OperationID: item.operationID,
			Origin:      item.origin,
			Content:     ownContentParts(item.content),
		})
	}
	if c.group != nil {
		for _, m := range c.group.members {
			snap.Background = append(snap.Background, BackgroundMemberView{Kind: string(m.kind), ID: m.id})
		}
		sort.Slice(snap.Background, func(i, j int) bool {
			if snap.Background[i].Kind != snap.Background[j].Kind {
				return snap.Background[i].Kind < snap.Background[j].Kind
			}
			return snap.Background[i].ID < snap.Background[j].ID
		})
	}
	return snap, nil
}

// ListSessions enumerates the durable Session identities through storage and
// returns each available Session's owned materialized record in the sorted
// listing order. A Session that fails validation is unavailable: absent from
// the returned list while valid siblings stay listed, with its direct reads
// returning the typed error. A malformed stored identity is omitted, like the
// sweep and recovery passes. Enumeration, storage, and context failures
// propagate: no misleading successful partial list is returned.
func (h *Harness) ListSessions(ctx context.Context) ([]SessionRecord, error) {
	ids, err := h.deps.Storage.ListSessionIDs(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]SessionRecord, 0, len(ids))
	for _, sessionID := range ids {
		if err := validateHexID(sessionID, "session id"); err != nil {
			continue
		}
		c, err := h.coordinatorFor(ctx, sessionID)
		if err != nil {
			if isCorruption(err) || errors.Is(err, ErrNotFound) { // an unavailable Session stays absent
				continue
			}
			return nil, err
		}
		c.mu.Lock()
		if c.gone { // a deletion committed between the lookup and the read
			c.mu.Unlock()
			continue
		}
		records = append(records, ownSessionRecord(c.graph.Session))
		c.mu.Unlock()
	}
	return records, nil
}

// ownHistoryFact maps one validated graph entry to its owned public fact. A
// hook_result entry contributes no fact.
func ownHistoryFact(entry graphEntry) (HistoryFact, bool) {
	fact := HistoryFact{
		EntryID:     entry.Envelope.ID,
		OperationID: entry.Envelope.OperationID,
		Sequence:    entry.Envelope.Sequence,
		CommittedAt: entry.Envelope.CommittedAt,
	}
	switch {
	case entry.Input != nil:
		fact.Kind = EntryInput
		fact.Input = ownInputPayload(*entry.Input)
	case entry.Assistant != nil:
		fact.Kind = EntryAssistant
		fact.Assistant = ownAssistantPayload(*entry.Assistant)
	case entry.ToolResult != nil:
		fact.Kind = EntryToolResult
		fact.ToolResult = ownToolResultPayload(*entry.ToolResult)
	case entry.Signal != nil:
		fact.Kind = EntrySignal
		fact.Signal = ownSignalPayload(*entry.Signal)
	case entry.Compaction != nil:
		fact.Kind = EntryCompaction
		fact.Compaction = ownCompactionPayload(*entry.Compaction)
	case entry.Settlement != nil:
		fact.Kind = EntryOperationSettlement
		fact.Settlement = ownSettlementPayload(*entry.Settlement)
	default:
		return HistoryFact{}, false
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
