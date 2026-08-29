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
// appendEventLocked returns the sequence of the row this event created or
// extended, or zero for an event that produces no row. A row-inserting event
// gets a fresh sequence; a coalesced text delta advances the sequence and carries
// it on the open row (last delta wins) so a delta streamed after a capture gates
// as new; a tool end updates its row in place and keeps that row's sequence.
func (t *transcript) appendEventLocked(ev Event) int {
	switch ev.Kind {
	case EventTurnStart:
		t.textOpen = false
		t.curTurn = ev.Turn
	case EventTurnEnd:
		t.textOpen = false
	case EventTextDelta:
		if t.textOpen && len(t.tail) > 0 {
			last := &t.tail[len(t.tail)-1]
			last.msg.Content += ev.Result
			last.seq = t.nextSeq
			seq := t.nextSeq
			t.nextSeq++
			return seq
		}
		seq := t.appendRowLocked(DisplayMessage{Type: "assistant", Content: ev.Result, Turn: t.curTurn})
		t.textOpen = true
		return seq
	case EventToolCallStart:
		t.textOpen = false
		return t.appendRowLocked(DisplayMessage{Type: "tool", ID: ev.ToolCallID, Name: ev.ToolName, Args: ev.Args, Turn: t.curTurn})
	case EventToolCallEnd:
		// Last end wins: a later ToolCallEnd for the same id overwrites the
		// row, and display metadata and subagent-session links attach on
		// success and clear on failure, so the live tail carries the same edit
		// previews and child links the durable projection derives from a
		// successful tool result.
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
				return r.seq
			}
		}
	case EventUserMessageDisplay:
		t.textOpen = false
		return t.appendRowLocked(DisplayMessage{Type: "user", Content: ev.Result, Turn: ev.Turn})
	case EventGenericSystemSignal:
		t.textOpen = false
		return t.appendRowLocked(DisplayMessage{Type: "system", Content: "System: " + ev.Result, Turn: ev.Turn})
	case EventBackgroundProcessComplete:
		t.textOpen = false
		msg := DisplayMessage{Type: "background_process", Done: true, Success: !ev.IsError, Result: ev.Result, Turn: ev.Turn}
		if ev.BackgroundProcess != nil {
			msg.BackgroundProcess = ev.BackgroundProcess
			msg.ID = ev.BackgroundProcess.ID
		}
		return t.appendRowLocked(msg)
	}
	return 0
}

func (t *transcript) appendRowLocked(msg DisplayMessage) int {
	seq := t.nextSeq
	t.tail = append(t.tail, tailRow{seq: seq, msg: msg})
	t.nextSeq++
	return seq
}

// foldSubagentAssociationLocked attaches a new child session's start to the
// matching tool row idempotently, without changing the row's sequence, and
// reports whether the row exists. It scans from the tail end, matching the
// last-end-wins rule of EventToolCallEnd. The caller holds seqMu.
func (t *transcript) foldSubagentAssociationLocked(toolCallID string, link SubagentSessionLink) bool {
	for i := len(t.tail) - 1; i >= 0; i-- {
		r := &t.tail[i]
		if r.msg.Type != "tool" || r.msg.ID != toolCallID {
			continue
		}
		for _, existing := range r.msg.SubagentSessionIDs {
			if existing.Index == link.Index && existing.SessionID == link.SessionID {
				return true
			}
		}
		r.msg.SubagentSessionIDs = append(r.msg.SubagentSessionIDs, link)
		return true
	}
	return false
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

// compactionRewriteLocked publishes the rewrite boundary for a compaction. Unlike
// a revert or fork, a compaction completes no new turn and leaves the active
// turn's uncommitted tail live, so it advances only the rewrite epoch: the
// committed markers and the still-live tail are unchanged. Advancing the epoch is
// what makes a live-selection capture that raced the compaction revalidate and
// re-read the rewritten durable prefix.
func (t *transcript) compactionRewriteLocked() {
	t.rewriteEpoch++
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

// feedLocked routes one delivered event into the coordinator: a session-tagged
// error is retained, a turn end closes the open span and commits the turn, and
// every other event folds into the tail. The caller holds seqMu.
func (t *transcript) feedLocked(ev Event) int {
	switch ev.Kind {
	case EventError:
		if ev.SessionID != "" {
			return t.appendErrorLocked(ev)
		}
		return 0
	case EventTurnEnd:
		seq := t.appendEventLocked(ev)
		t.commitLocked(ev.Turn)
		return seq
	default:
		return t.appendEventLocked(ev)
	}
}

// appendErrorLocked retains a session-tagged error as a display row. It draws
// from the same sequence as transcript rows so it merges into display order, and
// it closes any open text span. Retained errors are not durable and ordinary
// commits never prune them.
func (t *transcript) appendErrorLocked(ev Event) int {
	t.textOpen = false
	seq := t.nextSeq
	t.retainedErrors = append(t.retainedErrors, errorRow{
		seq:  seq,
		turn: ev.Turn,
		msg:  DisplayMessage{Type: "error", Content: ev.Error, Turn: ev.Turn},
	})
	t.nextSeq++
	return seq
}

// dropErrorsThroughTurnLocked removes retained errors for turns at or below
// through. It is the compaction disposition: errors in the transcript range the
// compacted record replaces are removed. An error carrying no turn attribution
// (turn == 0) is kept — it has no merge key, so it belongs to no compacted range
// and keeps its position after all committed rows.
func (t *transcript) dropErrorsThroughTurnLocked(through int) {
	t.retainedErrors = filterErrors(t.retainedErrors, func(e errorRow) bool { return e.turn == 0 || e.turn > through })
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

// completeTranscript is the transcript portion of a session's complete live
// state: durable committed rows read from history, the retained uncommitted
// tail, retained session errors, and the capture revision. Tail and error rows
// keep their sequence so a consumer can interleave them into display order.
type completeTranscript struct {
	committed []DisplayMessage
	tail      []tailRow
	errors    []errorRow
	revision  captureRevision
	// assistantOpen reports whether the last row the payload carries is an
	// open assistant span, computed under seqMu from the rows the capture
	// actually carries. It is published on the hydration state and the resync
	// payload so a consumer can continue a turn that was streaming when the
	// boundary was captured.
	assistantOpen bool
}

// tailSnapshotLocked returns a copy of the retained tail preserving sequence.
func (t *transcript) tailSnapshotLocked() []tailRow {
	if len(t.tail) == 0 {
		return nil
	}
	out := make([]tailRow, len(t.tail))
	copy(out, t.tail)
	return out
}

// errorSnapshotLocked returns a copy of the retained errors preserving sequence.
func (t *transcript) errorSnapshotLocked() []errorRow {
	if len(t.retainedErrors) == 0 {
		return nil
	}
	out := make([]errorRow, len(t.retainedErrors))
	copy(out, t.retainedErrors)
	return out
}

// mergeLiveRowsLocked merges retained tail rows and retained error rows into
// one display list ordered by their shared sequence, the ordering the desktop
// snapshot applies to the same two classes. The inputs are the snapshot
// accessors' copies: each is already sorted by sequence, and both draw from one
// monotonic counter, so a two-way merge reproduces display order without a
// sort. The caller holds seqMu.
func mergeLiveRowsLocked(tail []tailRow, errors []errorRow) []DisplayMessage {
	live := make([]DisplayMessage, 0, len(tail)+len(errors))
	i, j := 0, 0
	for i < len(tail) || j < len(errors) {
		if j >= len(errors) || (i < len(tail) && tail[i].seq <= errors[j].seq) {
			live = append(live, tail[i].msg)
			i++
		} else {
			live = append(live, errors[j].msg)
			j++
		}
	}
	return live
}

// projectHydrationMessages builds the producer-ordered display projection for a
// complete hydration. Retained errors belonging to a committed turn are placed
// after that turn's committed rows, while the remaining tail and errors keep
// their shared live sequence. Turn-zero errors have no committed merge key and
// therefore remain in the live suffix.
func projectHydrationMessages(committed []DisplayMessage, tail []tailRow, errors []errorRow) []DisplayMessage {
	if len(committed) == 0 {
		return mergeLiveRowsLocked(tail, errors)
	}

	lastCommittedIndex := make(map[int]int)
	for i, msg := range committed {
		lastCommittedIndex[msg.Turn] = i
	}
	consumed := make([]bool, len(errors))
	out := make([]DisplayMessage, 0, len(committed)+len(tail)+len(errors))
	for i, msg := range committed {
		out = append(out, msg)
		if msg.Turn == 0 || lastCommittedIndex[msg.Turn] != i {
			continue
		}
		for j, err := range errors {
			if !consumed[j] && err.turn == msg.Turn {
				out = append(out, err.msg)
				consumed[j] = true
			}
		}
	}

	remainingErrors := make([]errorRow, 0, len(errors))
	for i, err := range errors {
		if !consumed[i] {
			remainingErrors = append(remainingErrors, err)
		}
	}
	out = append(out, mergeLiveRowsLocked(tail, remainingErrors)...)
	return out
}

// assistantSpanOpenLocked reports whether the last row of live is an open
// assistant span: the coordinator's text span is open and the span's row is
// still among the retained rows the payload carries. live is the capture's
// merged live rows — the retained tail after the duplicate-tail disposition
// plus the retained errors ordered by sequence — so a capture that dropped a
// duplicate tail carries no open row and reads false, and a span closed by any
// boundary reads false. The caller holds seqMu.
func (t *transcript) assistantSpanOpenLocked(live []DisplayMessage) bool {
	if !t.textOpen || len(live) == 0 {
		return false
	}
	return live[len(live)-1].Type == "assistant"
}
