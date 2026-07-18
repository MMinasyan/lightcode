package agent

import "sync"

// transcript is the sole per-session coordinator for a session's live transcript
// state. It assigns one monotonic sequence to every display row, retains the
// uncommitted tail (with consecutive assistant text coalesced into one row), and
// tracks the committed markers plus a rewrite epoch. Those three values form the
// capture revision that a live-selection capture revalidates.
//
// The coordinator owns no adapter delivery and performs no I/O. Producers fold
// rows in through appendEventLocked while holding seqMu (the same section in
// which they enqueue the row for delivery); a capture reads the tail and revision
// under seqMu held in the total lock order. All state-mutating and reading methods
// therefore assume the caller already holds seqMu.
type transcript struct {
	seqMu sync.Mutex

	nextSeq       int
	committedSeq  int
	committedTurn int
	rewriteEpoch  int

	// curTurn is the turn assigned to coalesced assistant text; it advances on
	// turn start. textOpen is true while the last tail row is an assistant text
	// row still open for coalescing of consecutive deltas.
	curTurn  int
	textOpen bool

	tail []tailRow

	// retainedErrors holds session-tagged errors kept independently of ordinary
	// tail pruning. They are process-local (not durable) and draw from the same
	// sequence as rows so they merge into display order.
	retainedErrors []errorRow
}

// tailRow is one sequenced, uncommitted display row.
type tailRow struct {
	seq int
	msg DisplayMessage
}

// errorRow is one retained session-tagged error as a display row, tagged with
// the turn it belongs to for the revert and compaction dispositions.
type errorRow struct {
	seq  int
	turn int
	msg  DisplayMessage
}

// captureRevision identifies a coordinator state: the committed markers plus the
// rewrite epoch. A live-selection capture combines a durable-history prefix with
// the locked live tail only when this revision is unchanged across the read.
type captureRevision struct {
	committedTurn int
	committedSeq  int
	rewriteEpoch  int
}

func newTranscript() *transcript {
	return &transcript{nextSeq: 1}
}

// appendEventLocked folds one loop event into the retained tail, assigning a
// sequence to each new row. It mirrors the display projection exactly: contiguous
// assistant text deltas coalesce into one row; a tool start opens one row that its
// matching end completes in place (last end wins); user, system, and background
// rows map one to one; lifecycle and metadata events produce no row.
func (t *transcript) appendEventLocked(ev Event) {
	switch ev.Kind {
	case EventTurnStart:
		t.textOpen = false
		t.curTurn = ev.Turn
	case EventTurnEnd:
		t.textOpen = false
	case EventTextDelta:
		if t.textOpen && len(t.tail) > 0 {
			t.tail[len(t.tail)-1].msg.Content += ev.Result
			return
		}
		t.appendRowLocked(DisplayMessage{Type: "assistant", Content: ev.Result, Turn: t.curTurn})
		t.textOpen = true
	case EventToolCallStart:
		t.textOpen = false
		t.appendRowLocked(DisplayMessage{Type: "tool", ID: ev.ToolCallID, Name: ev.ToolName, Args: ev.Args})
	case EventToolCallEnd:
		// Last end wins: a staged edit emits a second end (the real result) after
		// its stage-time end, and the later end overwrites the row. Display metadata
		// and subagent-session links attach on success and clear on failure, so the
		// live tail carries the same edit previews and child links the durable
		// projection derives from a successful tool result.
		for i := len(t.tail) - 1; i >= 0; i-- {
			r := &t.tail[i]
			if r.msg.Type == "tool" && r.msg.ID == ev.ToolCallID {
				r.msg.Done = true
				r.msg.Success = !ev.IsError
				r.msg.Result = ev.Result
				if ev.ToolName != "" {
					r.msg.Name = ev.ToolName
				}
				if ev.Args != "" {
					r.msg.Args = ev.Args
				}
				if r.msg.Success {
					r.msg.Metadata = ev.Metadata
					r.msg.SubagentSessionIDs = subagentSessionLinksFromMetadata(ev.Metadata)
				} else {
					r.msg.Metadata = nil
					r.msg.SubagentSessionIDs = nil
				}
				break
			}
		}
	case EventUserMessageDisplay:
		t.textOpen = false
		t.appendRowLocked(DisplayMessage{Type: "user", Content: ev.Result, Turn: ev.Turn})
	case EventGenericSystemSignal:
		t.textOpen = false
		t.appendRowLocked(DisplayMessage{Type: "system", Content: "System: " + ev.Result})
	case EventBackgroundProcessComplete:
		t.textOpen = false
		msg := DisplayMessage{Type: "background_process", Done: true, Success: !ev.IsError, Result: ev.Result}
		if ev.BackgroundProcess != nil {
			msg.BackgroundProcess = ev.BackgroundProcess
			msg.ID = ev.BackgroundProcess.ID
		}
		t.appendRowLocked(msg)
	}
}

func (t *transcript) appendRowLocked(msg DisplayMessage) {
	t.tail = append(t.tail, tailRow{seq: t.nextSeq, msg: msg})
	t.nextSeq++
}

// commitLocked records that turn is durably persisted. The just-completed turn's
// rows become readable from durable history, so the tail is cleared and the
// committed markers advance to the current high-water. Sequence stays monotonic.
// The caller has already persisted the turn and flushed the event drainer.
func (t *transcript) commitLocked(turn int) {
	t.committedTurn = turn
	t.committedSeq = t.nextSeq - 1
	t.tail = nil
	t.textOpen = false
}

// rewriteLocked publishes one rewrite boundary for a revert, compaction, or fork:
// it advances the rewrite epoch, rebases the committed turn to the new durable
// turn, clears obsolete tail state, and keeps sequence monotonic.
func (t *transcript) rewriteLocked(turn int) {
	t.rewriteEpoch++
	t.committedTurn = turn
	t.committedSeq = t.nextSeq - 1
	t.tail = nil
	t.textOpen = false
}

func (t *transcript) revisionLocked() captureRevision {
	return captureRevision{
		committedTurn: t.committedTurn,
		committedSeq:  t.committedSeq,
		rewriteEpoch:  t.rewriteEpoch,
	}
}

// tailMessagesLocked returns a copy of the retained tail's display rows in
// sequence order, for a capture to merge with durable history.
func (t *transcript) tailMessagesLocked() []DisplayMessage {
	if len(t.tail) == 0 {
		return nil
	}
	out := make([]DisplayMessage, len(t.tail))
	for i, r := range t.tail {
		out[i] = r.msg
	}
	return out
}

// appendErrorLocked retains a session-tagged error as a display row. It draws
// from the same sequence as transcript rows so it merges into display order, and
// it closes any open text span. Retained errors are not durable and ordinary
// commits never prune them.
func (t *transcript) appendErrorLocked(ev Event) {
	t.textOpen = false
	t.retainedErrors = append(t.retainedErrors, errorRow{
		seq:  t.nextSeq,
		turn: ev.Turn,
		msg:  DisplayMessage{Type: "error", Content: ev.Error, Turn: ev.Turn},
	})
	t.nextSeq++
}

// dropErrorsAboveTurnLocked removes retained errors for turns above target. It is
// the history-revert disposition: errors at or below the revert target survive.
func (t *transcript) dropErrorsAboveTurnLocked(target int) {
	t.retainedErrors = filterErrors(t.retainedErrors, func(e errorRow) bool { return e.turn <= target })
}

// dropErrorsThroughTurnLocked removes retained errors for turns at or below
// through. It is the compaction disposition: errors in the transcript range the
// compacted record replaces are removed.
func (t *transcript) dropErrorsThroughTurnLocked(through int) {
	t.retainedErrors = filterErrors(t.retainedErrors, func(e errorRow) bool { return e.turn > through })
}

// clearErrorsLocked removes all retained errors. It is the external-rebase and
// lifecycle-removal disposition.
func (t *transcript) clearErrorsLocked() {
	t.retainedErrors = nil
}

// retainedErrorMessagesLocked returns a copy of the retained error rows in
// sequence order, for a capture to merge with tail rows and durable history.
func (t *transcript) retainedErrorMessagesLocked() []DisplayMessage {
	if len(t.retainedErrors) == 0 {
		return nil
	}
	out := make([]DisplayMessage, len(t.retainedErrors))
	for i, e := range t.retainedErrors {
		out[i] = e.msg
	}
	return out
}

func filterErrors(errs []errorRow, keep func(errorRow) bool) []errorRow {
	var out []errorRow
	for _, e := range errs {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out
}
