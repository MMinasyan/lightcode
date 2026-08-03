package agent

import (
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// HydrationRow is one sequenced live row — a retained tail row or a retained
// session error — that a consumer merges into the durable message stream by
// sequence.
type HydrationRow struct {
	Seq     int            `json:"seq"`
	Message DisplayMessage `json:"message"`
}

// HydrationCursor is the transcript capture revision: the durable committed
// markers plus the rewrite epoch.
type HydrationCursor struct {
	CommittedTurn int `json:"committedTurn"`
	CommittedSeq  int `json:"committedSeq"`
	RewriteEpoch  int `json:"rewriteEpoch"`
}

// HydrationState is a session's complete live state for one adapter hydration:
// the durable committed messages plus the retained tail, retained errors, and
// every live class, captured as one consistent set. It is intentionally not part
// of the shared AdapterService — the owning adapter holds the concrete agent.
type HydrationState struct {
	Session     SessionSummary       `json:"session"`
	Messages    []DisplayMessage     `json:"messages"`
	Tail        []HydrationRow       `json:"tail"`
	Errors      []HydrationRow       `json:"errors"`
	Cursor      HydrationCursor      `json:"cursor"`
	Tokens      TokenReport          `json:"tokens"`
	Model       ModelInfo            `json:"model"`
	Busy        bool                 `json:"busy"`
	Compacting  bool                 `json:"compacting"`
	Queue       QueueState           `json:"queue"`
	Warnings    []PromptWarning      `json:"warnings"`
	Permissions []permission.Request `json:"permissions"`
}

// HydrateSession captures a session's complete live state for an adapter to apply
// as one snapshot before replaying subsequent live events. The capture is the
// revalidating live-selection shape: a compaction or commit landing between the
// durable read and the locked read forces a retry, and exhausting the three
// attempts surfaces an error rather than an empty session. A session id that is
// not a live root resolves as a child: a live child from its registry entry, a
// completed child from its durable store.
func (a *Agent) HydrateSession(sessionID string) (HydrationState, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return HydrationState{}, fmt.Errorf("session id is required")
	}
	// Hold the lifecycle lock across the preparation reads so a concurrent
	// archive/delete cannot detach the session between resolving it, reading its
	// summary, and capturing its state, which would yield a hybrid snapshot.
	defer a.lockLifecycle()()
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit, err := a.liveSessionLocked(sessionID)
	rt.mu.Unlock()
	if err != nil {
		return a.hydrateChildSession(sessionID)
	}
	summary, err := a.SessionSummaryForSession(sessionID)
	if err != nil {
		return HydrationState{}, err
	}
	cs, err := a.captureStateForSelection(unit, nil)
	if err != nil {
		return HydrationState{}, err
	}
	return hydrationStateFrom(summary, cs), nil
}

// hydrateChildSession resolves a child session's complete transcript. A live
// child resolves from its registry entry — the durable prefix read through the
// entry's store plus the retained tail, errors, and cursor — while a completed
// child resolves from its durable store exactly as SessionMessagesFor's
// non-live branch does, with an empty tail and errors so the frontend's gate is
// a no-op. The registry entry's presence is the liveness signal: a lookup miss
// is the common case (the viewer opens a completed subtask's transcript far
// more often than a live one) and is not an error.
func (a *Agent) hydrateChildSession(sessionID string) (HydrationState, error) {
	rt := a.ensureRuntime()
	rt.transcriptMu.Lock()
	e := rt.transcriptState[sessionID]
	rt.transcriptMu.Unlock()
	if e != nil {
		return a.captureLiveChildSession(sessionID, e)
	}
	root, err := a.sessionsRootForSession(sessionID)
	if err != nil {
		return HydrationState{}, err
	}
	store, err := snapshot.NewForSessionsRoot(root, "", "")
	if err != nil {
		return HydrationState{}, err
	}
	msgs, _, err := a.messagesForFrontendForStoreAndMaxTurn(store, sessionID)
	if err != nil {
		return HydrationState{}, err
	}
	return HydrationState{Messages: msgs}, nil
}

// captureLiveChildSession captures a live child's complete transcript with the
// same revalidating shape a root hydration uses: the durable prefix is read
// outside the coordinator lock, and a commit landing between the read and the
// locked revalidation forces a retry rather than a torn snapshot. Only the
// transcript portion is read — a child has no live unit classes — and it comes
// from the shared locked capture, so the disjoint-halves guard applies to a
// child exactly as it does to a root turn.
func (a *Agent) captureLiveChildSession(sessionID string, e *transcriptCursor) (HydrationState, error) {
	tr := e.coord
	for attempt := 1; attempt <= 3; attempt++ {
		tr.seqMu.Lock()
		rev0 := tr.revisionLocked()
		tr.seqMu.Unlock()

		committed, maxDurableTurn, err := a.messagesForFrontendForStoreAndMaxTurn(e.store, sessionID)
		if err != nil {
			return HydrationState{}, err
		}

		tr.seqMu.Lock()
		ct, ok := captureTranscriptLocked(tr, committed, maxDurableTurn, &rev0)
		tr.seqMu.Unlock()
		if !ok {
			continue
		}
		return hydrationStateFrom(SessionSummary{ID: sessionID}, completeState{transcript: ct}), nil
	}
	return HydrationState{}, errCaptureRevisionChanged
}

func hydrationStateFrom(summary SessionSummary, cs completeState) HydrationState {
	hs := HydrationState{
		Session:  summary,
		Messages: cs.transcript.committed,
		Cursor: HydrationCursor{
			CommittedTurn: cs.transcript.revision.committedTurn,
			CommittedSeq:  cs.transcript.revision.committedSeq,
			RewriteEpoch:  cs.transcript.revision.rewriteEpoch,
		},
		Tokens:      cs.tokens,
		Model:       cs.model,
		Busy:        cs.busy,
		Compacting:  cs.compacting,
		Queue:       cs.queue,
		Warnings:    cs.warnings,
		Permissions: cs.permissions,
	}
	for _, r := range cs.transcript.tail {
		hs.Tail = append(hs.Tail, HydrationRow{Seq: r.seq, Message: r.msg})
	}
	for _, e := range cs.transcript.errors {
		hs.Errors = append(hs.Errors, HydrationRow{Seq: e.seq, Message: e.msg})
	}
	return hs
}
