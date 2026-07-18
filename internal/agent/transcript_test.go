package agent

import (
	"reflect"
	"testing"
)

func feedTranscript(tr *transcript, events []Event) {
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()
	for _, ev := range events {
		tr.appendEventLocked(ev)
	}
}

func transcriptTailRows(tr *transcript) []transcriptRow {
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()
	return projectDisplay(tr.tailMessagesLocked())
}

// TestTranscriptCoordinatorProjectionMatchesEventFold verifies the coordinator's
// incremental retained tail is byte-identical to the batch event projection: text
// deltas coalesce per contiguous span, tool start/end collapse to one row, and
// user/system/background rows map one to one. projectEvents is the reference fold
// already asserted equal to the durable display projection elsewhere.
func TestTranscriptCoordinatorProjectionMatchesEventFold(t *testing.T) {
	events := []Event{
		{Kind: EventTurnStart, Turn: 1},
		{Kind: EventTextDelta, Result: "Hello "},
		{Kind: EventTextDelta, Result: "world"},
		{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "read_file", Args: `{"path":"a"}`},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "ok"},
		{Kind: EventTextDelta, Result: "after tool"},
		{Kind: EventGenericSystemSignal, Result: "a signal"},
		{Kind: EventBackgroundProcessComplete, Result: "bg done", BackgroundProcess: &BackgroundProcessDisplay{
			ID: "bg1", Command: "sleep 1", Reason: "completed", Output: "out", ExitCode: 0,
		}},
		{Kind: EventTurnEnd, Turn: 1},
		{Kind: EventUserMessageDisplay, Result: "next question", Turn: 2},
		{Kind: EventTurnStart, Turn: 2},
		{Kind: EventTextDelta, Result: "reply"},
		{Kind: EventTurnEnd, Turn: 2},
	}
	tr := newTranscript()
	feedTranscript(tr, events)

	got := transcriptTailRows(tr)
	want := projectEvents(events)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("coordinator tail projection != event fold\n got: %#v\nwant: %#v", got, want)
	}
}

// TestTranscriptCoordinatorStagedToolLastEndWins verifies a second tool end for the
// same call overwrites the first in place, without adding a row or a sequence.
func TestTranscriptCoordinatorStagedToolLastEndWins(t *testing.T) {
	tr := newTranscript()
	feedTranscript(tr, []Event{
		{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "apply_patch"},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "Staged."},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "Applied.", ToolName: "apply_patch"},
	})
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()
	if len(tr.tail) != 1 {
		t.Fatalf("tail rows = %d, want 1 (one tool row)", len(tr.tail))
	}
	if got := tr.tail[0].msg.Result; got != "Applied." {
		t.Fatalf("tool result = %q, want last end %q", got, "Applied.")
	}
	if tr.nextSeq != 2 {
		t.Fatalf("nextSeq = %d, want 2 (one sequence assigned)", tr.nextSeq)
	}
}

// TestTranscriptCoordinatorToolMetadataAndSubagentLinks verifies a successful tool
// end carries its display metadata and derived subagent-session links into the
// live tail (matching the durable projection), and a failing final end clears them.
func TestTranscriptCoordinatorToolMetadataAndSubagentLinks(t *testing.T) {
	meta := map[string]any{
		"edit_preview_files":   []any{"a.go"},
		"subagent_session_ids": []SubagentSessionLink{{Index: 0, SessionID: "child-1"}},
	}
	tr := newTranscript()
	feedTranscript(tr, []Event{
		{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "task"},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "done", Metadata: meta},
	})
	tr.seqMu.Lock()
	row := tr.tail[0].msg
	tr.seqMu.Unlock()
	if !reflect.DeepEqual(row.Metadata, meta) {
		t.Fatalf("tool metadata not preserved: %#v", row.Metadata)
	}
	if want := []SubagentSessionLink{{Index: 0, SessionID: "child-1"}}; !reflect.DeepEqual(row.SubagentSessionIDs, want) {
		t.Fatalf("subagent links = %#v, want %#v", row.SubagentSessionIDs, want)
	}

	// A failing final end drops metadata and links: the durable projection keeps
	// them only for a successful result, so the last-end-wins tail must too.
	tr2 := newTranscript()
	feedTranscript(tr2, []Event{
		{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "task"},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "staged", Metadata: meta},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "boom", IsError: true},
	})
	tr2.seqMu.Lock()
	row2 := tr2.tail[0].msg
	tr2.seqMu.Unlock()
	if row2.Metadata != nil || row2.SubagentSessionIDs != nil {
		t.Fatalf("failed end kept metadata/links: meta=%#v links=%#v", row2.Metadata, row2.SubagentSessionIDs)
	}
}

// TestTranscriptCoordinatorSessionErrorRetention verifies session-tagged errors are
// retained as sequenced display rows, kept across ordinary commits, and disposed
// per operation: history revert drops errors above its target turn, compaction
// drops errors through its replaced range, and rebase/removal clears them.
func TestTranscriptCoordinatorSessionErrorRetention(t *testing.T) {
	tr := newTranscript()

	tr.seqMu.Lock()
	tr.appendEventLocked(Event{Kind: EventTurnStart, Turn: 1})
	tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "hello"})
	tr.appendErrorLocked(Event{Kind: EventError, Error: "boom", Turn: 1})
	tr.appendEventLocked(Event{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "x"})

	// Rows and the error share one strictly increasing sequence.
	rowSeq1 := tr.tail[0].seq
	errSeq := tr.retainedErrors[0].seq
	rowSeq2 := tr.tail[1].seq
	if !(rowSeq1 < errSeq && errSeq < rowSeq2) {
		t.Fatalf("error not sequenced between rows: row=%d err=%d row=%d", rowSeq1, errSeq, rowSeq2)
	}
	e := tr.retainedErrors[0]
	if e.msg.Type != "error" || e.msg.Content != "boom" || e.turn != 1 {
		t.Fatalf("retained error = %+v", e)
	}

	// Ordinary commit preserves the error: completed messages do not represent it.
	tr.commitLocked(1)
	if len(tr.retainedErrors) != 1 {
		t.Fatalf("commit pruned retained error: %#v", tr.retainedErrors)
	}
	tr.seqMu.Unlock()

	// History revert to turn 1 drops errors above turn 1, keeps turn 1.
	tr.seqMu.Lock()
	tr.appendErrorLocked(Event{Kind: EventError, Error: "later", Turn: 2})
	tr.dropErrorsAboveTurnLocked(1)
	if len(tr.retainedErrors) != 1 || tr.retainedErrors[0].turn != 1 {
		t.Fatalf("history-revert disposition wrong: %#v", tr.retainedErrors)
	}
	tr.seqMu.Unlock()

	// Compaction through turn 1 removes the turn-1 error.
	tr.seqMu.Lock()
	tr.dropErrorsThroughTurnLocked(1)
	if len(tr.retainedErrors) != 0 {
		t.Fatalf("compaction disposition kept turn-1 error: %#v", tr.retainedErrors)
	}
	tr.seqMu.Unlock()

	// Rebase/removal clears everything.
	tr.seqMu.Lock()
	tr.appendErrorLocked(Event{Kind: EventError, Error: "x", Turn: 3})
	tr.clearErrorsLocked()
	empty := len(tr.retainedErrors) == 0 && tr.retainedErrorMessagesLocked() == nil
	tr.seqMu.Unlock()
	if !empty {
		t.Fatal("rebase/removal did not clear retained errors")
	}
}

// TestTranscriptCoordinatorCommit verifies the commit cursor partitions
// state exactly: a committed turn clears the retained tail and advances the
// committed markers to the sequence high-water, later preseeds keep sequence
// monotonic, and a rewrite advances the epoch and rebases the committed turn while
// preserving monotonic sequence.
func TestTranscriptCoordinatorCommit(t *testing.T) {
	tr := newTranscript()

	// Turn 1 streams two rows, then commits.
	feedTranscript(tr, []Event{
		{Kind: EventTurnStart, Turn: 1},
		{Kind: EventTextDelta, Result: "a"},
		{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "x"},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "ok"},
		{Kind: EventTurnEnd, Turn: 1},
	})
	tr.seqMu.Lock()
	high1 := tr.nextSeq - 1
	tr.commitLocked(1)
	rev1 := tr.revisionLocked()
	tailAfter1 := tr.tailMessagesLocked()
	tr.seqMu.Unlock()

	if high1 != 2 {
		t.Fatalf("turn 1 high-water seq = %d, want 2", high1)
	}
	if len(tailAfter1) != 0 {
		t.Fatalf("tail after commit = %d rows, want 0", len(tailAfter1))
	}
	if rev1 != (captureRevision{committedTurn: 1, committedSeq: 2, rewriteEpoch: 0}) {
		t.Fatalf("revision after commit 1 = %+v", rev1)
	}

	// Preseed of turn 2: its rows must take sequences strictly above committedSeq.
	feedTranscript(tr, []Event{
		{Kind: EventUserMessageDisplay, Result: "q2", Turn: 2},
		{Kind: EventTurnStart, Turn: 2},
		{Kind: EventTextDelta, Result: "b"},
		{Kind: EventTurnEnd, Turn: 2},
	})
	tr.seqMu.Lock()
	firstSeq2 := tr.tail[0].seq
	high2 := tr.nextSeq - 1
	tr.commitLocked(2)
	rev2 := tr.revisionLocked()
	tr.seqMu.Unlock()

	if firstSeq2 <= rev1.committedSeq {
		t.Fatalf("turn 2 first row seq = %d, must exceed committedSeq %d", firstSeq2, rev1.committedSeq)
	}
	if rev2 != (captureRevision{committedTurn: 2, committedSeq: high2, rewriteEpoch: 0}) {
		t.Fatalf("revision after commit 2 = %+v (high2=%d)", rev2, high2)
	}

	// A rewrite (revert/compaction/fork) bumps the epoch, rebases the committed
	// turn, keeps sequence monotonic, and clears the tail.
	feedTranscript(tr, []Event{
		{Kind: EventTurnStart, Turn: 3},
		{Kind: EventTextDelta, Result: "c"},
	})
	tr.seqMu.Lock()
	seqBeforeRewrite := tr.nextSeq
	tr.rewriteLocked(2)
	rev3 := tr.revisionLocked()
	tailAfter3 := tr.tailMessagesLocked()
	seqAfterRewrite := tr.nextSeq
	tr.rewriteLocked(2)
	rev4 := tr.revisionLocked()
	tr.seqMu.Unlock()

	if rev3.rewriteEpoch != 1 {
		t.Fatalf("rewrite epoch = %d, want 1", rev3.rewriteEpoch)
	}
	if rev3.committedTurn != 2 {
		t.Fatalf("rewrite committedTurn = %d, want rebased to 2", rev3.committedTurn)
	}
	if len(tailAfter3) != 0 {
		t.Fatalf("tail after rewrite = %d rows, want 0", len(tailAfter3))
	}
	if seqAfterRewrite != seqBeforeRewrite {
		t.Fatalf("rewrite reset sequence: before=%d after=%d", seqBeforeRewrite, seqAfterRewrite)
	}
	if rev4.rewriteEpoch != 2 {
		t.Fatalf("second rewrite epoch = %d, want 2 (each rewrite is one boundary)", rev4.rewriteEpoch)
	}
}
