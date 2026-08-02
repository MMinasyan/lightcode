package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/prompt"
)

func feedTranscriptEvents(tr *transcript, events []Event) {
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
	feedTranscriptEvents(tr, events)

	got := transcriptTailRows(tr)
	want := projectEvents(events)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("coordinator tail projection != event fold\n got: %#v\nwant: %#v", got, want)
	}
}

// TestTranscriptCoordinatorPerDeltaSequence verifies each streamed text delta
// advances the sequence and returns it, while the coalesced tail row carries its
// latest delta's sequence — so a delta delivered after a capture gates as new even
// though the tail still holds one coalesced row.
func TestTranscriptCoordinatorPerDeltaSequence(t *testing.T) {
	tr := newTranscript()
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()

	tr.appendEventLocked(Event{Kind: EventTurnStart, Turn: 1})
	s1 := tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "a"})
	s2 := tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "b"})
	s3 := tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "c"})

	if s1 == 0 || s2 != s1+1 || s3 != s2+1 {
		t.Fatalf("delta sequences = %d,%d,%d, want three consecutive nonzero", s1, s2, s3)
	}
	if len(tr.tail) != 1 {
		t.Fatalf("tail rows = %d, want 1 coalesced row", len(tr.tail))
	}
	if tr.tail[0].msg.Content != "abc" {
		t.Fatalf("coalesced content = %q, want %q", tr.tail[0].msg.Content, "abc")
	}
	if tr.tail[0].seq != s3 {
		t.Fatalf("coalesced row seq = %d, want last delta seq %d", tr.tail[0].seq, s3)
	}
}

// TestTranscriptCoordinatorToolEndKeepsRowSequence verifies a tool end updates its
// row in place and returns that row's original sequence, so an id-keyed result
// update is never re-sequenced and tail sequences stay monotonic even when a tool
// completes after a later row was appended.
func TestTranscriptCoordinatorToolEndKeepsRowSequence(t *testing.T) {
	tr := newTranscript()
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()

	startSeq := tr.appendEventLocked(Event{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "read_file"})
	userSeq := tr.appendEventLocked(Event{Kind: EventUserMessageDisplay, Result: "q", Turn: 1})
	endSeq := tr.appendEventLocked(Event{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "ok"})

	if endSeq != startSeq {
		t.Fatalf("tool end seq = %d, want the tool row's start seq %d", endSeq, startSeq)
	}
	if userSeq <= startSeq {
		t.Fatalf("later row seq %d must exceed the tool row seq %d", userSeq, startSeq)
	}
	if len(tr.tail) != 2 || tr.tail[0].seq != startSeq || !tr.tail[0].msg.Done {
		t.Fatalf("tool row = %#v, want seq %d done in place", tr.tail[0], startSeq)
	}
}

// TestTranscriptCoordinatorStagedToolLastEndWins verifies a second tool end for the
// same call overwrites the first in place, without adding a row or a sequence.
func TestTranscriptCoordinatorStagedToolLastEndWins(t *testing.T) {
	tr := newTranscript()
	feedTranscriptEvents(tr, []Event{
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
	feedTranscriptEvents(tr, []Event{
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
	feedTranscriptEvents(tr2, []Event{
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

// TestCaptureStateForSelectionRevalidation verifies the live-selection capture
// revalidates the coordinator revision across a fixed three attempts: a stable
// revision succeeds on the first read, a commit or rewrite during the read is
// retried once and then succeeds, three consecutive changes exhaust the attempts
// with a retryable error, and a durable read error returns immediately with no
// further attempt.
func TestCaptureStateForSelectionRevalidation(t *testing.T) {
	bumpCommit := func(tr *transcript) {
		tr.seqMu.Lock()
		tr.commitLocked(tr.committedTurn + 1)
		tr.seqMu.Unlock()
	}

	t.Run("stable_first", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		attempts := 0
		a.captureProbe = func(int) error { attempts++; return nil }
		if _, err := a.captureStateForSelection(a.session, nil); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (no retry)", attempts)
		}
	})

	t.Run("normal_commit_then_success", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		tr := a.session.transcript
		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			if attempt == 1 {
				bumpCommit(tr)
			}
			return nil
		}
		if _, err := a.captureStateForSelection(a.session, nil); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2 (one retry then success)", attempts)
		}
	})

	t.Run("rewrite_then_success", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		unit := a.session
		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			if attempt == 1 {
				// Drive the production compaction rewrite path: it advances the
				// epoch, so the racing capture must revalidate and retry rather
				// than publish the pre-compaction prefix.
				a.publishCompactionRewrite(unit, sessionIDOf(unit), unit.projectID, SessionSummary{}, nil)
			}
			return nil
		}
		if _, err := a.captureStateForSelection(a.session, nil); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2 (production compaction bumped the epoch once)", attempts)
		}
	})

	t.Run("three_mismatches", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		tr := a.session.transcript
		attempts := 0
		a.captureProbe = func(int) error {
			attempts++
			bumpCommit(tr)
			return nil
		}
		_, err := a.captureStateForSelection(a.session, nil)
		if !errors.Is(err, errCaptureRevisionChanged) {
			t.Fatalf("err = %v, want errCaptureRevisionChanged", err)
		}
		if attempts != 3 {
			t.Fatalf("attempts = %d, want 3", attempts)
		}
	})

	t.Run("read_error_attempt_1", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		sentinel := errors.New("read boom")
		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			if attempt == 1 {
				return sentinel
			}
			return nil
		}
		_, err := a.captureStateForSelection(a.session, nil)
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want sentinel", err)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (no later attempt)", attempts)
		}
	})

	t.Run("read_error_attempt_2_after_mismatch", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		tr := a.session.transcript
		sentinel := errors.New("read boom")
		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			switch attempt {
			case 1:
				bumpCommit(tr)
			case 2:
				return sentinel
			}
			return nil
		}
		_, err := a.captureStateForSelection(a.session, nil)
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want sentinel", err)
		}
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2", attempts)
		}
	})
}

// TestCaptureStateInvokesBoundaryWithBuiltState verifies the capture invokes the
// boundary callback with the immutable state it built, so an adapter can append
// its boundary while the capture still holds the locks.
func TestCaptureStateInvokesBoundaryWithBuiltState(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	unit := a.session

	tr := unit.transcript
	tr.seqMu.Lock()
	tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "hi"})
	tr.seqMu.Unlock()

	invoked := 0
	var seen completeState
	returned, err := a.captureState(unit, func(st completeState) {
		invoked++
		seen = st
	})
	if err != nil {
		t.Fatalf("captureState: %v", err)
	}
	if invoked != 1 {
		t.Fatalf("boundary invoked %d times, want 1", invoked)
	}
	if len(seen.transcript.tail) != 1 || seen.transcript.tail[0].msg.Content != "hi" {
		t.Fatalf("boundary state tail = %#v", seen.transcript.tail)
	}
	if len(returned.transcript.tail) != len(seen.transcript.tail) {
		t.Fatal("boundary state differs from the returned state")
	}
}

// TestCaptureStateCapturesPendingPermissions verifies the capture reads the
// session's pending permission requests under the gate lock.
func TestCaptureStateCapturesPendingPermissions(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	unit := a.session
	sessionID := sessionIDOf(unit)

	permCtx, permCancel := context.WithCancel(context.Background())
	defer permCancel()
	go a.gate.AskRequest(permCtx, permission.Request{SessionID: sessionID, ToolName: "write_file", Arg: "x"})

	deadline := time.Now().Add(2 * time.Second)
	for len(a.gate.PendingForSession(sessionID)) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	st, err := a.captureState(unit, nil)
	if err != nil {
		t.Fatalf("captureState: %v", err)
	}
	a.gate.CancelAll()

	if len(st.permissions) != 1 || st.permissions[0].ToolName != "write_file" || st.permissions[0].SessionID != sessionID {
		t.Fatalf("captured permissions = %#v, want one write_file for this session", st.permissions)
	}
}

// TestCaptureStateReadsAllLiveClasses verifies the full capture reads every live
// class — transcript, activity, model, tokens, queue, warnings — as one set, by
// seeding a distinctive value in each under its lock and asserting all of them.
func TestCaptureStateReadsAllLiveClasses(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	unit := a.session
	rt := a.ensureRuntime()

	rt.mu.Lock()
	unit.busy = true
	unit.compacting = true
	unit.currentRef = coremodel.ModelRef{Provider: "p", Model: "m"}
	unit.queue = []QueuedItem{{ID: "q1", Content: "queued"}}
	unit.queueVersion = 7
	rt.mu.Unlock()

	unit.tokensMu.Lock()
	unit.tokens = map[string]*TokenEntry{"p/m": {Provider: "p", Model: "m", Input: 11, Known: true}}
	unit.tokensMu.Unlock()

	tr := unit.transcript
	tr.seqMu.Lock()
	tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "hi"})
	tr.seqMu.Unlock()

	a.setWarningGroup("protocol", []prompt.Warning{{Kind: "k", Message: "m"}})

	st, err := a.captureState(unit, nil)
	if err != nil {
		t.Fatalf("captureState: %v", err)
	}
	if len(st.transcript.tail) != 1 || st.transcript.tail[0].msg.Content != "hi" {
		t.Fatalf("captured transcript tail = %#v", st.transcript.tail)
	}
	if !st.busy {
		t.Fatal("busy not captured")
	}
	if !st.compacting {
		t.Fatal("compacting not captured")
	}
	if st.model.Provider != "p" || st.model.Model != "m" {
		t.Fatalf("model = %+v, want p/m", st.model)
	}
	if st.tokens.Total.Input != 11 {
		t.Fatalf("tokens total input = %d, want 11", st.tokens.Total.Input)
	}
	if len(st.queue.Items) != 1 || st.queue.Items[0].ID != "q1" || st.queue.Version != 7 {
		t.Fatalf("queue = %+v, want one item q1 version 7", st.queue)
	}
	found := false
	for _, w := range st.warnings {
		if w.Kind == "k" && w.Message == "m" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warning not captured: %#v", st.warnings)
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
// monotonic.
func TestTranscriptCoordinatorCommit(t *testing.T) {
	tr := newTranscript()

	// Turn 1 streams two rows, then commits.
	feedTranscriptEvents(tr, []Event{
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
	feedTranscriptEvents(tr, []Event{
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
}
