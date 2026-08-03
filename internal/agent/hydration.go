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
// as one snapshot before replaying subsequent live events. The capture is the
// revalidating live-selection shape: a compaction or commit landing between the
// durable read and the locked read forces a retry, and exhausting the three
// attempts surfaces an error rather than an empty session.
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
	cs, err := a.captureStateForSelection(unit, nil)
	if err != nil {
		return HydrationState{}, err
	}
	return hydrationStateFrom(summary, cs), nil
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
