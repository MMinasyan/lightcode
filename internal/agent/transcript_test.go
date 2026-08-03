package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/message"
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

// newLiveCatalogBackedTestAgent returns an agent whose current session is a
// registered live session, so coordinator lookups resolve through the runtime's
// transcriptState registry. Coordinator tests that operate on the current
// session use this rather than reaching a coordinator field on the unit.
func newLiveCatalogBackedTestAgent(t *testing.T) *Agent {
	t.Helper()
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return a
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

// TestTranscriptRegistryCursorsAdvanceIndependently verifies the step-12
// registry property: each live session's coordinator lives in its own registry
// entry with its own seqMu, so feeding or committing one session never moves
// another's {nextSeq, committedSeq, committedTurn}, and a feed to one session
// never blocks on another session's coordinator lock. Mutation check: key the
// registry so both sessions share one entry; this test must fail.
func TestTranscriptRegistryCursorsAdvanceIndependently(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}

	trA := a.transcriptForSessionID(firstID)
	trB := a.transcriptForSessionID(secondID)
	if trA == nil || trB == nil {
		t.Fatal("live sessions have no registry entries")
	}
	if trA == trB {
		t.Fatal("live sessions share one coordinator")
	}

	nextSeq := func(tr *transcript) int {
		tr.seqMu.Lock()
		defer tr.seqMu.Unlock()
		return tr.nextSeq
	}

	// Interleaved feed: A's rows advance A only; B's cursor stays put. Turn
	// start consumes no sequence; each text delta consumes one.
	seqA0, seqB0 := nextSeq(trA), nextSeq(trB)
	feedTranscript(trA, Event{Kind: EventTurnStart, Turn: 1})
	feedTranscript(trA, Event{Kind: EventTextDelta, Result: "a1"})
	feedTranscript(trA, Event{Kind: EventTextDelta, Result: "a2"})
	if got := nextSeq(trA); got != seqA0+2 {
		t.Fatalf("session A nextSeq = %d, want %d", got, seqA0+2)
	}
	if got := nextSeq(trB); got != seqB0 {
		t.Fatalf("feeding session A advanced session B's cursor: %d -> %d", seqB0, got)
	}

	feedTranscript(trB, Event{Kind: EventTurnStart, Turn: 1})
	feedTranscript(trB, Event{Kind: EventTextDelta, Result: "b1"})
	if got := nextSeq(trB); got != seqB0+1 {
		t.Fatalf("session B nextSeq = %d, want %d", got, seqB0+1)
	}
	if got := nextSeq(trA); got != seqA0+2 {
		t.Fatalf("feeding session B advanced session A's cursor: %d -> %d", seqA0+2, got)
	}

	// Commits are per-session too: committing A's turn moves only A's markers.
	trA.seqMu.Lock()
	trA.commitLocked(1)
	revA := trA.revisionLocked()
	trA.seqMu.Unlock()
	trB.seqMu.Lock()
	revB := trB.revisionLocked()
	trB.seqMu.Unlock()
	if revA.committedTurn != 1 || revA.committedSeq == 0 {
		t.Fatalf("session A revision after commit = %+v, want committedTurn 1 with a nonzero committedSeq", revA)
	}
	if revB != (captureRevision{}) {
		t.Fatalf("committing session A advanced session B's committed markers: %+v", revB)
	}

	// A feed to B must not need A's lock: holding A's coordinator lock does not
	// stall B's feed, which is exactly what a registry-wide hold would do.
	trA.seqMu.Lock()
	fedB := make(chan int, 1)
	go func() {
		fedB <- feedTranscript(trB, Event{Kind: EventTextDelta, Result: "b-concurrent"})
	}()
	select {
	case <-fedB:
	case <-time.After(2 * time.Second):
		t.Fatal("feed to session B blocked on session A's coordinator lock: one lock guards both sessions' feeds")
	}
	trA.seqMu.Unlock()
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
		a := newLiveCatalogBackedTestAgent(t)
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
		a := newLiveCatalogBackedTestAgent(t)
		tr := a.transcriptForSessionID(sessionIDOf(a.session))
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
		a := newLiveCatalogBackedTestAgent(t)
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
		a := newLiveCatalogBackedTestAgent(t)
		tr := a.transcriptForSessionID(sessionIDOf(a.session))
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
		a := newLiveCatalogBackedTestAgent(t)
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
		a := newLiveCatalogBackedTestAgent(t)
		tr := a.transcriptForSessionID(sessionIDOf(a.session))
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
	a := newLiveCatalogBackedTestAgent(t)
	unit := a.session

	tr := a.transcriptForSessionID(sessionIDOf(unit))
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
	a := newLiveCatalogBackedTestAgent(t)
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
	a := newLiveCatalogBackedTestAgent(t)
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

	tr := a.transcriptForSessionID(sessionIDOf(unit))
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

// commitState is the coordinator snapshot a turn-end event handler records at
// the moment EventTurnEnd is delivered.
type commitState struct {
	seen          bool
	turn          int
	committedTurn int
	committedSeq  int
	nextSeq       int
	tailLen       int
}

// TestTranscriptCommitContractMatrix verifies the turn-end commit ordering
// contract: the drainer flush and the coordinator commit run after the turn's
// loop returns and strictly before EventTurnEnd is emitted. With the commit
// inside the runtime.mu section that clears busy, a submit that observes busy
// cleared and claims the unit cannot feed the next turn's rows into a tail the
// previous turn's commit then wipes.
func TestTranscriptCommitContractMatrix(t *testing.T) {
	// The event handler observes the coordinator at the instant turn_end is
	// delivered. The commit must already have run: committedTurn must equal
	// the delivered turn, the retained tail must be empty, and the committed
	// sequence must be the high-water. With the commit after the emit (the
	// pre-fix order) the handler sees the turn's rows still uncommitted.
	t.Run("order=commit_before_end_emit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTextResponse(w, "hello back")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		ctx := startEventOrderAgent(t, a, &eventCapture{})
		// Swap in the observing handler after Init: startEventOrderAgent
		// installs the capture handler, and this one records the coordinator
		// state at every turn_end delivery. The handler must take seqMu only
		// for turn_end — row events are emitted from feedAndEmit, which
		// already holds seqMu.
		var stateMu sync.Mutex
		var atEnd commitState
		a.SetEventHandler(func(ev Event) {
			if ev.Kind != EventTurnEnd {
				return
			}
			tr := a.transcriptForSessionID(ev.SessionID)
			if tr == nil {
				return
			}
			tr.seqMu.Lock()
			st := commitState{
				seen:          true,
				turn:          ev.Turn,
				committedTurn: tr.committedTurn,
				committedSeq:  tr.committedSeq,
				nextSeq:       tr.nextSeq,
				tailLen:       len(tr.tail),
			}
			tr.seqMu.Unlock()
			stateMu.Lock()
			atEnd = st
			stateMu.Unlock()
		})
		if _, err := a.Submit(ctx, "hello"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitUntilEventOrderAgentIdle(t, a)

		stateMu.Lock()
		st := atEnd
		stateMu.Unlock()
		if !st.seen {
			t.Fatal("no turn_end delivered")
		}
		if st.committedTurn != st.turn {
			t.Fatalf("at turn_end delivery committedTurn = %d, want the delivered turn %d (the commit ran after the emit)", st.committedTurn, st.turn)
		}
		if st.tailLen != 0 {
			t.Fatalf("at turn_end delivery retained tail = %d rows, want 0 (the commit ran after the emit)", st.tailLen)
		}
		if st.committedSeq != st.nextSeq-1 {
			t.Fatalf("at turn_end delivery committedSeq = %d, want high-water %d (the commit ran after the emit)", st.committedSeq, st.nextSeq-1)
		}
	})

	// A submit racing a turn end must not lose the next turn's rows. The
	// pre-fix window: the turn-end path cleared busy and released runtime.mu
	// before feeding the end event, so a concurrent submit could observe busy
	// false, claim, and launch turn N+1 whose rows enter the tail before turn
	// N's late commit set t.tail = nil, erasing them.
	//
	// The subtest parks turn N's commit feed by holding seqMu (the only
	// parkable point on that path), releases turn N's model gate, and submits
	// turn N+1 while the commit is still pending:
	//
	//   - Pre-fix the feed is post-unlock, so the submit claims, launches, and
	//     its turn_start is delivered (and turn N's turn_end was already
	//     delivered before the feed) while the commit is still pending — the
	//     exact window the defect is built on. The assertion below fails.
	//   - With the commit inside the busy-clear section the parked feed holds
	//     runtime.mu, so the submit cannot claim until the commit has run: no
	//     turn_start(N+1) is delivered before the release, and the next turn's
	//     rows then feed after the commit and survive.
	t.Run("race=submit_vs_turn_end", func(t *testing.T) {
		releaseRun := make(chan struct{})
		releaseN1 := make(chan struct{})
		reqNSeen := make(chan struct{}, 1)
		reqN1Seen := make(chan struct{}, 1)
		var runOnce, n1Once sync.Once
		closeRun := func() { runOnce.Do(func() { close(releaseRun) }) }
		closeN1 := func() { n1Once.Do(func() { close(releaseN1) }) }
		t.Cleanup(closeRun)
		t.Cleanup(closeN1)

		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Payload-only response: no TextDelta reaches the loop event queue
			// while seqMu is held, so the drainer stays idle and the flush ack
			// completes instead of deadlocking on the parked commit feed.
			gate := releaseN1
			seen := reqN1Seen
			if reqs.Add(1) == 1 {
				gate = releaseRun
				seen = reqNSeen
			}
			select {
			case seen <- struct{}{}:
			default:
			}
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","reasoning":"stall"},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		t.Cleanup(server.Close)

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		// Turn N: submit and park its Run in the model request.
		resN, err := a.SubmitToSession(ctx, id, "turn N")
		if err != nil {
			t.Fatalf("SubmitToSession N: %v", err)
		}
		if !resN.Started {
			t.Fatalf("turn N enqueued instead of starting")
		}
		waitForSignal(t, reqNSeen, "turn N model request")

		// Park turn N's commit feed: hold seqMu, then release Run(N), so the
		// turn-end path runs through the busy clear and parks at the commit
		// feed with the deferred cleanup pending.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		seqMuHeld := true
		defer func() {
			if seqMuHeld {
				tr.seqMu.Unlock()
			}
		}()

		closeRun()
		// Wait out the completion and the park: with the commit feed inside
		// the busy-clear section the parked feed holds runtime.mu, so busy is
		// not observable until the commit completes.
		time.Sleep(200 * time.Millisecond)

		// Submit turn N+1 racing the turn end while the commit is pending.
		var resN1 SubmitResult
		var errN1 error
		var subWg sync.WaitGroup
		subWg.Add(1)
		go func() {
			defer subWg.Done()
			resN1, errN1 = a.SubmitToSession(ctx, id, "turn N+1")
		}()
		time.Sleep(100 * time.Millisecond)

		// While turn N's commit is still pending (seqMu is still held here),
		// the racing submit must not have claimed: no turn_start(N+1) may be
		// delivered, and turn N's turn_end may not precede the commit.
		startedNext, endedN := false, false
		for _, ev := range cap.snapshot() {
			switch {
			case ev.Kind == EventTurnStart && ev.Turn == resN.Turn+1:
				startedNext = true
			case ev.Kind == EventTurnEnd && ev.Turn == resN.Turn:
				endedN = true
			}
		}
		if startedNext {
			t.Fatalf("the racing submit claimed and launched turn %d while turn %d's commit was still pending — the window that wipes the next turn's rows", resN.Turn+1, resN.Turn)
		}
		if endedN {
			t.Fatalf("turn %d's turn_end was delivered before its commit ran", resN.Turn)
		}

		// Release the stall: turn N's commit completes, then the submit
		// claims and turn N+1 runs to the second gate.
		tr.seqMu.Unlock()
		seqMuHeld = false
		waitForSignal(t, reqN1Seen, "turn N+1 model request")

		// The next turn's rows must be intact: its user row is in the retained
		// tail while the turn is live (the loop emits the user display before
		// the model request).
		userRow := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && !userRow {
			tr.seqMu.Lock()
			for _, row := range tr.tail {
				if row.msg.Type == "user" && row.msg.Content == "turn N+1" {
					userRow = true
					break
				}
			}
			tr.seqMu.Unlock()
			if !userRow {
				time.Sleep(2 * time.Millisecond)
			}
		}
		if !userRow {
			t.Fatal("the next turn's user row is missing from the retained tail: turn N's commit wiped it")
		}

		closeN1()
		subWg.Wait()
		if errN1 != nil {
			t.Fatalf("SubmitToSession N+1: %v", errN1)
		}
		if !resN1.Started {
			t.Fatalf("turn N+1 enqueued instead of claiming the unit")
		}
		waitSessionDrained(t, a, id)

		tr.seqMu.Lock()
		committedTurn := tr.committedTurn
		tr.seqMu.Unlock()
		if committedTurn != resN1.Turn {
			t.Fatalf("committedTurn = %d, want turn N+1's turn %d", committedTurn, resN1.Turn)
		}
	})

	// A queued preseed completes its turn durably and commits it before the
	// final queued item launches. While the launched turn is still live (its
	// model request parked), the coordinator must already have committed the
	// preseed's turn, and a hydration taken then must return the preseed
	// exactly once — from the durable half alone, with the retained tail no
	// longer carrying it. The pre-fix drain preseeded under rt.mu with no feed
	// and no commit, so the preseed's row stayed in the retained tail while its
	// turn was already durable: a mid-drain hydration returned it from both
	// halves and rendered it twice.
	t.Run("preseed=commits_before_launch", func(t *testing.T) {
		release := make(chan struct{})
		reqSeen := make(chan struct{}, 1)
		var reqSeenOnce, runOnce sync.Once
		closeReqSeen := func() { reqSeenOnce.Do(func() { close(reqSeen) }) }
		closeRun := func() { runOnce.Do(func() { close(release) }) }
		// Register the server close before closeRun: cleanup runs LIFO, so a
		// parked handler is released before the server is allowed to close.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			closeReqSeen()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			writeTextResponse(w, "ok")
		}))
		t.Cleanup(func() { server.Close() })
		t.Cleanup(closeRun)

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		rt.sessionLocked().queue = []QueuedItem{
			{ID: "q-1", Content: "preseed me"},
			{ID: "q-2", Content: "launch me"},
		}
		rt.sessionLocked().queueSeq = 2
		rt.sessionLocked().queueVersion = 1
		rt.mu.Unlock()

		go rt.tryDrainQueue(ctx)
		waitForSignal(t, reqSeen, "launched turn model request")

		// The preseed's turn must already be committed while the launched turn
		// is still live: the pre-fix drain fed nothing, so committedTurn is 0
		// here.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		committedTurn := tr.committedTurn
		tr.seqMu.Unlock()
		if committedTurn != 1 {
			t.Fatalf("committedTurn = %d while the launched turn is live, want 1 (the preseed commits before the launch)", committedTurn)
		}

		// A hydration taken mid-drain returns the preseed exactly once. The
		// pre-fix drain left the preseed's row in the retained tail while its
		// turn was already durable, so the capture returned it twice.
		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession mid-drain: %v", err)
		}
		if got := countHydrationContent(hs, "preseed me"); got != 1 {
			t.Fatalf("mid-drain hydration returns the preseed %d times, want exactly once (durable + tail)", got)
		}

		closeRun()
		waitSessionDrained(t, a, id)

		hs, err = a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession after drain: %v", err)
		}
		for _, content := range []string{"preseed me", "launch me"} {
			if got := countHydrationContent(hs, content); got != 1 {
				t.Fatalf("post-drain hydration returns %q %d times, want exactly once", content, got)
			}
		}
		var livePreseed, liveLaunch int
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventUserMessageDisplay {
				switch ev.Result {
				case "preseed me":
					livePreseed++
				case "launch me":
					liveLaunch++
				}
			}
		}
		if livePreseed != 1 || liveLaunch != 1 {
			t.Fatalf("live user displays: preseed=%d launch=%d, want 1 each", livePreseed, liveLaunch)
		}
	})

	// A preseed whose durable marker write fails must surface the failure
	// through the transcript (sequenced, so a hydration or reconnect sees it),
	// launch no further turn, requeue the untouched remainder — and only that,
	// with its original ids — ahead of anything submitted meanwhile, and leave
	// the unit drainable. Then, with the fault cleared, draining again must
	// render the failed message exactly once in the live stream and exactly
	// once in loop history: not once per attempt.
	//
	// The marker fault is armed with the drain parked at the preseed's turn
	// start feed: the drain has already called BeginTurn (so the turn dir and
	// Store.CurrentTurn() are known) but not yet MarkTurnComplete, and the
	// parked feed holds seqMu so the test controls the release. A directory
	// named complete at that turn's marker path makes the marker write fail
	// with EISDIR on any filesystem, regardless of root. Arming before the
	// drain cannot target a preseed: creating turns/<n> feeds the store's
	// nextTurnLocked scan, so every preseed takes max+1 and skips the armed
	// dir.
	t.Run("preseed=marker_failure_aborts_and_requeues", func(t *testing.T) {
		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			writeTextResponse(w, "ok")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[id]
		rt.mu.Unlock()

		rt.mu.Lock()
		unit.queue = []QueuedItem{
			{ID: "q-1", Content: "fail me"},
			{ID: "q-2", Content: "survivor"},
		}
		unit.queueSeq = 2
		unit.queueVersion = 1
		rt.mu.Unlock()

		// Park the drain at the first preseed's turn start feed: the preseed
		// loop has called BeginTurn (turns/<next> exists) and is waiting on the
		// coordinator lock the test holds. The park is what makes both the
		// arming and the mid-drain append deterministic.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		seqMuHeld := true
		defer func() {
			if seqMuHeld {
				tr.seqMu.Unlock()
			}
		}()

		drained := make(chan struct{})
		go func() {
			defer close(drained)
			rt.tryDrainQueue(ctx)
		}()
		time.Sleep(100 * time.Millisecond)

		// Arm the marker fault at the turn the parked drain just began. Then
		// drive the concurrent message through the real submit path: the drain
		// holds the claim (busy), so the submit enqueues, and the requeue must
		// prepend the remainder ahead of it. The submit nudges the drainer;
		// the settle sleep lets it consume the wake while the drain is still
		// parked and busy, so it cannot re-drain after the abort.
		failTurn := unit.store.CurrentTurn()
		if failTurn == 0 {
			t.Fatalf("drain did not begin a preseed turn before parking")
		}
		markerPath := filepath.Join(unit.store.Dir(), "turns", strconv.Itoa(failTurn), "complete")
		if err := os.MkdirAll(markerPath, 0o700); err != nil {
			t.Logf("armed marker fault failed (pre-fix drain does not park at the feed): %v", err)
		}
		clearFault := func() {
			if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
				t.Fatalf("clear marker fault: %v", err)
			}
		}
		t.Cleanup(clearFault)

		res, err := a.SubmitToSession(ctx, id, "meanwhile")
		if err != nil {
			t.Fatalf("SubmitToSession meanwhile: %v", err)
		}
		if res.Started {
			t.Fatal("submit during the drain claimed the unit")
		}
		if len(res.Queue) != 1 || res.Queue[0].Content != "meanwhile" {
			t.Fatalf("submit snapshot = %#v, want one enqueued meanwhile item", res.Queue)
		}
		time.Sleep(100 * time.Millisecond)

		tr.seqMu.Unlock()
		seqMuHeld = false
		select {
		case <-drained:
		case <-time.After(15 * time.Second):
			t.Fatal("drain did not return after the marker failure")
		}

		// The failure is sequenced into the coordinator — not just delivered
		// live — so a hydration or reconnect sees it.
		tr.seqMu.Lock()
		errRows := append([]errorRow(nil), tr.retainedErrors...)
		tr.seqMu.Unlock()
		if len(errRows) != 1 {
			t.Fatalf("retained errors = %d, want exactly 1 sequenced failure", len(errRows))
		}
		var liveErr bool
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventError {
				liveErr = true
			}
		}
		if !liveErr {
			t.Fatal("marker failure was not delivered live")
		}

		// The drain aborted: no further turn launched.
		if got := reqs.Load(); got != 0 {
			t.Fatalf("a model turn launched after the marker failure (%d requests)", got)
		}
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart {
				t.Fatalf("a turn_start was delivered after the marker failure: %#v", ev)
			}
		}

		// The untouched remainder is back on the queue — the failed item is
		// not — prepended ahead of the item appended meanwhile, with the
		// original ids preserved.
		q, err := a.QueueSnapshotForSession(id)
		if err != nil {
			t.Fatalf("QueueSnapshotForSession: %v", err)
		}
		if len(q.Items) != 2 ||
			q.Items[0].Content != "survivor" || q.Items[0].ID != "q-2" ||
			q.Items[1].Content != "meanwhile" {
			t.Fatalf("requeued queue = %#v, want [survivor(q-2), meanwhile]", q.Items)
		}
		// The requeue event must carry the real queue, not an empty snapshot:
		// a bare emptyQueue() here would tell every host the queue is empty
		// while items sit in it.
		assertQueueChangedPayloadForVersion(t, cap, q.Version, q.Items)

		// The unit is drainable: the abort cleared the claimed-but-not-launched
		// busy marker instead of wedging the session.
		if busy, err := a.BusyForSession(id); err != nil || busy {
			t.Fatalf("unit busy after failed drain: busy=%v err=%v", busy, err)
		}
		// The abort leaves the same residue as the other abort paths: the
		// per-turn context is cleared, not left installed-cancelled.
		rt.mu.Lock()
		turnCtxSet := unit.turnCtx != nil
		turnCancelSet := unit.turnCancel != nil
		rt.mu.Unlock()
		if turnCtxSet || turnCancelSet {
			t.Fatal("failed drain left the per-turn context installed")
		}

		// Clear the fault and drain again: the failed message renders exactly
		// once in the live stream and once in loop history, not once per
		// attempt (a requeue of the failed item would re-append it).
		clearFault()
		rt.nudgeQueueDrainer()
		waitSessionDrained(t, a, id)

		var liveFail, liveSurvivor, liveMeanwhile int
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventUserMessageDisplay {
				switch ev.Result {
				case "fail me":
					liveFail++
				case "survivor":
					liveSurvivor++
				case "meanwhile":
					liveMeanwhile++
				}
			}
		}
		if liveFail != 1 || liveSurvivor != 1 || liveMeanwhile != 1 {
			t.Fatalf("live user displays: fail=%d survivor=%d meanwhile=%d, want 1 each", liveFail, liveSurvivor, liveMeanwhile)
		}
		var histFail, histSurvivor int
		for _, m := range unit.lp.Messages() {
			if m.Role != message.RoleUser {
				continue
			}
			switch m.TextContent() {
			case "fail me":
				histFail++
			case "survivor":
				histSurvivor++
			}
		}
		if histFail != 1 {
			t.Fatalf("loop history has %q %d times, want exactly once", "fail me", histFail)
		}
		if histSurvivor != 1 {
			t.Fatalf("loop history has %q %d times, want exactly once", "survivor", histSurvivor)
		}
	})

	// Admission can close while the preseed runs. The section that reacquires
	// the runtime mutex before the final launch must re-check it: a shutdown
	// arriving during the preseed aborts the drain the way the marker-failure
	// path aborts — the untouched remainder goes back on the queue, no turn is
	// launched, and the per-iteration cleanup clears busy and releases the
	// count. The pre-fix drain installed nothing to re-check, so it launched
	// the final item after shutdown and emitted its turn_start.
	t.Run("preseed=shutdown_aborts_before_launch", func(t *testing.T) {
		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			writeTextResponse(w, "ok")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[id]
		rt.mu.Unlock()
		rt.mu.Lock()
		unit.queue = []QueuedItem{
			{ID: "q-1", Content: "preseed"},
			{ID: "q-2", Content: "launch"},
		}
		unit.queueSeq = 2
		unit.queueVersion = 1
		rt.mu.Unlock()

		// Park the drain at the first preseed's turn start feed, then start
		// the shutdown. ShutdownOwner sets admission closed, cancels the
		// claim-time turn context, and joins turnWG — which the parked drain
		// holds until it aborts.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		seqMuHeld := true
		defer func() {
			if seqMuHeld {
				tr.seqMu.Unlock()
			}
		}()

		drained := make(chan struct{})
		go func() {
			defer close(drained)
			rt.tryDrainQueue(ctx)
		}()
		time.Sleep(100 * time.Millisecond)

		shutdownDone := make(chan struct{})
		go func() {
			defer close(shutdownDone)
			a.ShutdownOwner()
		}()

		// Wait until the shutdown has cancelled the claim-time turn context —
		// only possible because the context is installed at claim. Releasing
		// before that would let the drain finish its preseed unhindered.
		cancelled := false
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			rt.mu.Lock()
			tc := unit.turnCtx
			rt.mu.Unlock()
			if tc != nil && tc.Err() != nil {
				cancelled = true
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if !cancelled {
			t.Fatal("shutdown did not cancel the claimed-but-not-launched turn context")
		}

		tr.seqMu.Unlock()
		seqMuHeld = false
		select {
		case <-drained:
		case <-time.After(15 * time.Second):
			t.Fatal("drain did not return after shutdown")
		}
		select {
		case <-shutdownDone:
		case <-time.After(15 * time.Second):
			t.Fatal("ShutdownOwner did not complete")
		}

		// No turn launched after shutdown.
		if got := reqs.Load(); got != 0 {
			t.Fatalf("a model turn launched after shutdown (%d requests)", got)
		}
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart {
				t.Fatalf("a turn_start was delivered after shutdown: %#v", ev)
			}
		}

		// The untouched remainder (the final item) is back on the queue with
		// its original id, and the requeue event carries it.
		q, err := a.QueueSnapshotForSession(id)
		if err != nil {
			t.Fatalf("QueueSnapshotForSession: %v", err)
		}
		if len(q.Items) != 1 || q.Items[0].Content != "launch" || q.Items[0].ID != "q-2" {
			t.Fatalf("queue after shutdown abort = %#v, want [launch(q-2)]", q.Items)
		}
		assertQueueChangedPayloadForVersion(t, cap, q.Version, q.Items)

		// The abort leaves the same residue as the other abort paths: the
		// per-turn context is cleared, not left installed-cancelled.
		rt.mu.Lock()
		turnCtxSet := unit.turnCtx != nil
		turnCancelSet := unit.turnCancel != nil
		rt.mu.Unlock()
		if turnCtxSet || turnCancelSet {
			t.Fatal("shutdown abort left the per-turn context installed")
		}
	})

	// The unit is busy from the moment it is claimed, so a cancel arriving
	// during the preseed must find the turn's cancel already installed and
	// must be honoured: no turn is launched afterwards, and the untouched
	// remainder stays on the queue. The pre-fix drain installed the cancel
	// only immediately before launchTurn, so the preseed window was not
	// cancellable and the launch went ahead.
	t.Run("preseed=cancel_aborts_before_launch", func(t *testing.T) {
		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			writeTextResponse(w, "ok")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[id]
		rt.mu.Unlock()
		rt.mu.Lock()
		unit.queue = []QueuedItem{
			{ID: "q-1", Content: "cancel me"},
			{ID: "q-2", Content: "launch"},
		}
		unit.queueSeq = 2
		unit.queueVersion = 1
		rt.mu.Unlock()

		// Park the drain at the first preseed's turn start feed, then cancel
		// the session. The cancel must succeed and actually cancel the
		// claim-time context, so the drain aborts instead of launching.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		seqMuHeld := true
		defer func() {
			if seqMuHeld {
				tr.seqMu.Unlock()
			}
		}()

		drained := make(chan struct{})
		go func() {
			defer close(drained)
			rt.tryDrainQueue(ctx)
		}()
		time.Sleep(100 * time.Millisecond)

		if err := a.CancelSession(id); err != nil {
			t.Fatalf("CancelSession: %v", err)
		}
		tr.seqMu.Unlock()
		seqMuHeld = false
		select {
		case <-drained:
		case <-time.After(15 * time.Second):
			t.Fatal("drain did not return after the cancel")
		}

		// No turn launched after the cancel.
		if got := reqs.Load(); got != 0 {
			t.Fatalf("a model turn launched after the cancel (%d requests)", got)
		}
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart {
				t.Fatalf("a turn_start was delivered after the cancel: %#v", ev)
			}
		}

		// The untouched remainder is back on the queue with its original id.
		q, err := a.QueueSnapshotForSession(id)
		if err != nil {
			t.Fatalf("QueueSnapshotForSession: %v", err)
		}
		if len(q.Items) != 1 || q.Items[0].Content != "launch" || q.Items[0].ID != "q-2" {
			t.Fatalf("queue after cancel abort = %#v, want [launch(q-2)]", q.Items)
		}
		assertQueueChangedPayloadForVersion(t, cap, q.Version, q.Items)

		// The abort leaves the same residue as the other abort paths: the
		// per-turn context is cleared, not left installed-cancelled.
		rt.mu.Lock()
		turnCtxSet := unit.turnCtx != nil
		turnCancelSet := unit.turnCancel != nil
		rt.mu.Unlock()
		if turnCtxSet || turnCancelSet {
			t.Fatal("cancel abort left the per-turn context installed")
		}

		// The unit is not wedged: the next drain launches the remainder
		// normally.
		rt.nudgeQueueDrainer()
		waitSessionDrained(t, a, id)
		if got := reqs.Load(); got != 1 {
			t.Fatalf("model requests after re-drain = %d, want exactly 1 (the remainder launched once)", got)
		}
	})

	// The drain's re-validation is not atomic with the launch: a cancel can
	// land after the revalidation passes and the runtime mutex is released,
	// but before launchTurn begins. launchTurn must reject such a handoff at
	// its receiving end, before it creates or emits anything, and unwind the
	// claim — the wait-group count and the busy flag — so the session is not
	// wedged. The handoff is driven directly, the same way
	// TestDeferredCleanupGuardContract drives launchTurn: the unit is
	// arranged as a claimed-but-not-yet-launched turn whose revalidation
	// already passed, the context is cancelled, and the call must be refused.
	// Without the rejection launchTurn accepts it: it creates a turn, emits
	// its turn_start, and returns a nonzero turn.
	//
	// The rejection's second half is the caller's: the final item was already
	// removed from the queue when the handoff is refused, so the drain must
	// put it back — the same requeue the other abort paths perform — and a
	// later drain must launch it exactly once. That part is driven through
	// the real drain with a deterministically refused launch
	// (unit.lp = nil makes launchTurn return 0 before creating anything), so
	// the closure's requeue-on-refusal is what is exercised.
	t.Run("preseed=launch_rejects_cancelled_handoff", func(t *testing.T) {
		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			writeTextResponse(w, "ok")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[id]
		rt.mu.Unlock()
		before := unit.store.CurrentTurn()

		// Arrange the claimed-but-not-yet-launched claim the drain leaves
		// behind when it hands off: busy set, per-turn context installed, the
		// preseed-phase wait-group count held.
		turnCtx, cancel := context.WithCancel(ctx)
		rt.mu.Lock()
		unit.busy = true
		unit.turnCtx = turnCtx
		unit.turnCancel = cancel
		rt.mu.Unlock()
		rt.turnWG.Add(1)

		// The cancel lands after the revalidation passed and before the
		// launch begins.
		cancel()
		if launched := rt.launchTurn(ctx, unit, turnCtx, cancel, []string{"late cancel"}); launched != 0 {
			t.Fatalf("launchTurn accepted a cancelled handoff, returned turn %d", launched)
		}

		// Nothing was created or emitted.
		if got := unit.store.CurrentTurn(); got != before {
			t.Fatalf("CurrentTurn = %d after the rejected handoff, want unchanged %d", got, before)
		}
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart {
				t.Fatalf("a turn_start was delivered for a cancelled handoff: %#v", ev)
			}
		}
		if got := reqs.Load(); got != 0 {
			t.Fatalf("model request after rejected handoff (%d)", got)
		}

		// The claim was unwound: busy and the per-turn context are cleared,
		// and the wait-group count is released.
		rt.mu.Lock()
		busy := unit.busy
		turnCtxSet := unit.turnCtx != nil
		turnCancelSet := unit.turnCancel != nil
		rt.mu.Unlock()
		if busy {
			t.Fatal("unit still busy after the rejected handoff")
		}
		if turnCtxSet || turnCancelSet {
			t.Fatal("unit still holds the per-turn context after the rejected handoff")
		}
		wgDone := make(chan struct{})
		go func() {
			rt.turnWG.Wait()
			close(wgDone)
		}()
		select {
		case <-wgDone:
		case <-time.After(2 * time.Second):
			t.Fatal("rejected handoff did not release the wait-group count")
		}

		// The session stays usable: a fresh submit claims, launches, and
		// drains.
		if _, err := a.Submit(ctx, "after cancel"); err != nil {
			t.Fatalf("Submit after rejected handoff: %v", err)
		}
		waitSessionDrained(t, a, id)
		if got := reqs.Load(); got != 1 {
			t.Fatalf("model requests = %d, want exactly 1 (the follow-up turn)", got)
		}

		// Second half: a launch refused through the real drain must not drop
		// the final item. Seed the queue, make launchTurn refuse the handoff
		// deterministically (lp nil returns 0 before anything is created),
		// and drain.
		before2 := unit.store.CurrentTurn()
		turnStartsBefore := 0
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart {
				turnStartsBefore++
			}
		}
		savedLP := unit.lp
		rt.mu.Lock()
		unit.lp = nil
		unit.queue = []QueuedItem{{ID: "q-1", Content: "late cancel"}}
		unit.queueSeq = 1
		unit.queueVersion = 0
		rt.mu.Unlock()
		rt.tryDrainQueue(ctx)

		// No turn was created or emitted by the refused launch.
		if got := unit.store.CurrentTurn(); got != before2 {
			t.Fatalf("CurrentTurn = %d after the refused launch, want unchanged %d", got, before2)
		}
		turnStartsAfter := 0
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart {
				turnStartsAfter++
			}
		}
		if turnStartsAfter != turnStartsBefore {
			t.Fatalf("turn_start count changed across the refused launch: %d -> %d", turnStartsBefore, turnStartsAfter)
		}
		if got := reqs.Load(); got != 1 {
			t.Fatalf("model requests after the refused launch = %d, want still 1", got)
		}

		// The final item is back on the queue with its original id, and the
		// requeue event carries it.
		q, err := a.QueueSnapshotForSession(id)
		if err != nil {
			t.Fatalf("QueueSnapshotForSession: %v", err)
		}
		if len(q.Items) != 1 || q.Items[0].Content != "late cancel" || q.Items[0].ID != "q-1" {
			t.Fatalf("queue after the refused launch = %#v, want [late cancel(q-1)]", q.Items)
		}
		assertQueueChangedPayloadForVersion(t, cap, q.Version, q.Items)

		// The claim was unwound: busy, the per-turn context, and the count.
		rt.mu.Lock()
		busy = unit.busy
		turnCtxSet = unit.turnCtx != nil
		turnCancelSet = unit.turnCancel != nil
		rt.mu.Unlock()
		if busy {
			t.Fatal("unit still busy after the refused launch")
		}
		if turnCtxSet || turnCancelSet {
			t.Fatal("unit still holds the per-turn context after the refused launch")
		}
		wgDone = make(chan struct{})
		go func() {
			rt.turnWG.Wait()
			close(wgDone)
		}()
		select {
		case <-wgDone:
		case <-time.After(2 * time.Second):
			t.Fatal("refused launch did not release the wait-group count")
		}

		// A later drain launches the requeued item exactly once.
		rt.mu.Lock()
		unit.lp = savedLP
		rt.mu.Unlock()
		rt.nudgeQueueDrainer()
		waitSessionDrained(t, a, id)
		if got := reqs.Load(); got != 2 {
			t.Fatalf("model requests after re-drain = %d, want exactly 2 (the requeued item launched once)", got)
		}
		var liveLate int
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventUserMessageDisplay && ev.Result == "late cancel" {
				liveLate++
			}
		}
		if liveLate != 1 {
			t.Fatalf("live user displays of %q = %d, want exactly once", "late cancel", liveLate)
		}
	})
}

// countHydrationContent counts how many times content appears across a
// hydration's durable messages, retained tail rows, and retained error rows —
// the three halves the frontend concatenates. A row present in both the
// durable half and the tail counts twice.
func countHydrationContent(hs HydrationState, content string) int {
	n := 0
	for _, m := range hs.Messages {
		if m.Content == content {
			n++
		}
	}
	for _, r := range hs.Tail {
		if r.Message.Content == content {
			n++
		}
	}
	for _, e := range hs.Errors {
		if e.Message.Content == content {
			n++
		}
	}
	return n
}

// assertQueueChangedPayloadForVersion asserts the EventQueueChanged carrying
// version carries exactly items: a requeue must publish the real queue, not an
// empty snapshot, or every host would be told the queue is empty while items
// sit in it.
func assertQueueChangedPayloadForVersion(t *testing.T, cap *eventCapture, version int, items []QueuedItem) {
	t.Helper()
	for _, ev := range cap.snapshot() {
		if ev.Kind == EventQueueChanged && ev.QueueVersion == version {
			if !reflect.DeepEqual(ev.Queue, items) {
				t.Fatalf("queue_changed v%d payload = %#v, want %#v", version, ev.Queue, items)
			}
			return
		}
	}
	t.Fatalf("no queue_changed event for version %d in %#v", version, cap.snapshot())
}

// TestDeferredCleanupGuardContract pins launchTurn's deferred cleanup guard
// directly, with no interleaving to force: the deferred cleanup must clear the
// unit's per-turn state only when the unit still holds that turn's context,
// and must leave a later claim's state untouched. The goroutine is driven to
// its early-return path (the owner context is already canceled, so Run never
// starts) and the deferred cleanup runs against the unit in the exact state
// the test arranged; completion is observed through the deferred cleanup's
// own cancel() of the turn context, which runs after the guard.
func TestDeferredCleanupGuardContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "unreachable")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	startEventOrderAgent(t, a, &eventCapture{})
	id, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	unit := a.sessions[id]
	rt := a.ensureRuntime()

	// An already-canceled owner context makes launchTurn's goroutine take its
	// early-return path, so the deferred cleanup runs without any turn work.
	canceled, cancelOwner := context.WithCancel(context.Background())
	cancelOwner()

	// runDeferred arranges the unit's per-turn state, launches a turn whose
	// deferred cleanup will run immediately, and waits for the deferred
	// cleanup to complete (it cancels thisCtx after the guard).
	runDeferred := func(unitBusy bool, unitCtx context.Context, unitCancel context.CancelFunc, thisCtx context.Context, thisCancel context.CancelFunc) {
		t.Helper()
		rt.mu.Lock()
		unit.busy = unitBusy
		unit.turnCtx = unitCtx
		unit.turnCancel = unitCancel
		rt.mu.Unlock()
		rt.turnWG.Add(1)
		rt.launchTurn(canceled, unit, thisCtx, thisCancel, []string{"x"})
		select {
		case <-thisCtx.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("deferred cleanup did not run")
		}
	}

	// A later claim replaced the unit's per-turn context (and busy flag)
	// before this turn's deferred cleanup ran: the guard must leave that
	// state untouched.
	t.Run("state=later_claim_untouched", func(t *testing.T) {
		thisCtx, thisCancel := context.WithCancel(context.Background())
		defer thisCancel()
		laterCtx, laterCancel := context.WithCancel(context.Background())
		defer laterCancel()
		runDeferred(true, laterCtx, laterCancel, thisCtx, thisCancel)

		rt.mu.Lock()
		busy := unit.busy
		turnCtx := unit.turnCtx
		turnCancel := unit.turnCancel
		rt.mu.Unlock()
		if !busy {
			t.Fatal("deferred cleanup cleared the later turn's busy flag")
		}
		if turnCtx != laterCtx {
			t.Fatal("deferred cleanup replaced the later turn's context")
		}
		if turnCancel == nil || reflect.ValueOf(turnCancel).Pointer() != reflect.ValueOf(laterCancel).Pointer() {
			t.Fatal("deferred cleanup replaced the later turn's cancel")
		}
	})

	// The unit still holds this turn's context: the deferred cleanup must
	// clear the claim so the unit is not stuck busy after an owner-cancelled
	// early return.
	t.Run("state=own_context_cleared", func(t *testing.T) {
		thisCtx, thisCancel := context.WithCancel(context.Background())
		defer thisCancel()
		runDeferred(true, thisCtx, thisCancel, thisCtx, thisCancel)

		rt.mu.Lock()
		busy := unit.busy
		turnCtx := unit.turnCtx
		turnCancel := unit.turnCancel
		rt.mu.Unlock()
		if busy {
			t.Fatal("deferred cleanup left the unit busy while it still held the turn's context")
		}
		if turnCtx != nil {
			t.Fatal("deferred cleanup left the unit's turn context set")
		}
		if turnCancel != nil {
			t.Fatal("deferred cleanup left the unit's turn cancel set")
		}
	})
}
