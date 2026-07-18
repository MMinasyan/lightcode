package agent

import (
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/permission"
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
	Model       coremodel.ModelRef   `json:"model"`
	Busy        bool                 `json:"busy"`
	Compacting  bool                 `json:"compacting"`
	Queue       QueueState           `json:"queue"`
	Warnings    []PromptWarning      `json:"warnings"`
	Permissions []permission.Request `json:"permissions"`
}

// HydrateSession captures a session's complete live state for an adapter to apply
// as one snapshot before replaying subsequent live events.
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
		return HydrationState{}, err
	}
	summary, err := a.SessionSummaryForSession(sessionID)
	if err != nil {
		return HydrationState{}, err
	}
	cs, err := a.captureState(unit, nil)
	if err != nil {
		return HydrationState{}, err
	}
	return hydrationStateFrom(summary, cs), nil
}

// HydrateSessionWithBoundary captures a session's complete state and calls emit
// with it while the capture locks are still held, so a boundary the adapter appends
// from emit is atomic with the captured state: no event delivered after the capture
// can be enqueued before the boundary. emit is called exactly once — with the
// captured state, or with the zero state when the session is empty/unresolvable or
// the durable read fails (a detach).
func (a *Agent) HydrateSessionWithBoundary(sessionID string, emit func(HydrationState)) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		emit(HydrationState{})
		return
	}
	defer a.lockLifecycle()()
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit, err := a.liveSessionLocked(sessionID)
	rt.mu.Unlock()
	if err != nil {
		emit(HydrationState{})
		return
	}
	summary, err := a.SessionSummaryForSession(sessionID)
	if err != nil {
		emit(HydrationState{})
		return
	}
	emitted := false
	if _, err := a.captureState(unit, func(cs completeState) {
		emitted = true
		emit(hydrationStateFrom(summary, cs))
	}); err != nil && !emitted {
		emit(HydrationState{})
	}
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
